package main

// 传输引擎：下载走 HTTP/1.1，上传走裸 TCP + 固定 Content-Length。
// 每条流一个 goroutine、一条独立连接，共享原子计数，全程无全局锁。
//
// 与上一版的区别：流是「可增删的」——调度器会根据吞吐爬坡与掉速情况
// 动态开关流，因此每条流持有自己的 context，停止由 ctx 驱动而非全局标志。

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxConcurrency   = 64
	initialStreams   = 4  // 爬坡起点
	rampStep         = 2  // 每轮新增
	rampWindow       = 10 // 秒，超过则锁定并发
	maxDurationSec   = 24 * 3600
	chunkSize        = 32 << 20 // Range 分片 32MB
	shortFileBytes   = 32 << 20 // 小于这个大小算小文件，爬坡时不给它加流
	readBufSize      = 256 << 10
	maxFailBeforeRot = 3
	uploadBodyLen    = int64(8) << 30
	browserUA        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// sockBufSize 是手动指定的 socket 收发缓冲（字节）。0 = 不设，交给内核自适应。
//
// 早先固定写死 8MB，副作用比收益大：一旦显式设置 SO_RCVBUF / SO_SNDBUF，
// 内核的自适应调优（tcp_moderate_rcvbuf）就关掉了，窗口从此钉死在申请值上；
// 而申请值本身还受 net.core.rmem_max 约束，没跑过 tune.go 的机器会被静默压回 208KB
// —— 也就是说这个「加大缓冲」在非 root 环境下从来没生效过，只是白关了自适应。
//
// 千兆内网的 BDP 只有几十 KB，固定 8MB 的收益是让队列更深；
// 代价是 64 条连接饱和时内核缓冲上限能到 1GB，换来更大的拷贝工作集和内存回收。
// tune.go 已经把 rmem_max/wmem_max 抬到 16MB，自适应调优会按实际 BDP 自己长，
// 长肥管道（跨境高 RTT）不会因此被压。
//
// 想复核固定值是否更划算：FNSPEED_SOCKBUF=8388608 启动做 A/B。
var sockBufSize = func() int {
	v := strings.TrimSpace(os.Getenv("FNSPEED_SOCKBUF"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}()

// tlsSessionCache 让重连走会话复用，而不是每次完整握手。
// 完整握手要验整条证书链，一次几百微秒到几毫秒；随机起点 + 读到文件尾的模式下，
// 每条流每隔几秒就要换一次连接，64 条流叠起来在弱核上很显眼。
var tlsSessionCache = tls.NewLRUClientSessionCache(128)

type URLState struct {
	EP     Endpoint
	Kind   string // download | upload
	Mode   TransferMode
	Status URLStatus
	Size   int64

	Recv       atomic.Int64
	Sent       atomic.Int64
	Reconnects atomic.Int64
	Failures   atomic.Int64

	errMu   sync.Mutex
	lastErr string

	// remoteIP 是这条源实际连上的对端地址。运行中顺手记下，
	// 界面要显示「命中 IP」时不用再发请求 —— 连接本来就已经建立。
	remoteMu sync.Mutex
	remoteIP string

	// ---- 定向测速 ----
	// 预检是并发跑的、还可能被 2.5s 超时甩在后面，所以这组字段用
	// atomic.Pointer 整体替换，读写都不抢锁。
	pin atomic.Pointer[pinInfo]

	// settled 保证「预检结果」与「乐观放行」只有一方能写 Mode/Status。
	// 谁 CAS 成功谁写，另一方放弃，避免数据竞争。
	settled atomic.Bool

	// Short 标记「小文件」：几十 KB 的 js、几 MB 的图片读完就 EOF，
	// 给它们加流只会制造重连噪声，爬坡时应当先跳过。
	Short atomic.Bool
	// active 是当前挂在这条源上的流数，分流时用来做负载均衡。
	active atomic.Int32
}

func (s *URLState) setErr(msg string) {
	s.errMu.Lock()
	s.lastErr = msg
	s.errMu.Unlock()
}

func (s *URLState) getErr() string {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastErr
}

func (s *URLState) setRemote(addr string) {
	if addr == "" {
		return
	}
	s.remoteMu.Lock()
	s.remoteIP = addr
	s.remoteMu.Unlock()
}

func (s *URLState) getRemote() string {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	return s.remoteIP
}

func (s *URLState) out() URLOut {
	o := URLOut{
		URL: s.EP.URL, Kind: s.Kind, Mode: string(s.Mode), Status: string(s.Status),
		Recv: s.Recv.Load(), Sent: s.Sent.Load(),
		Reconnects: s.Reconnects.Load(), Failures: s.Failures.Load(),
		RemoteIP:  s.getRemote(),
		LastError: s.getErr(),
	}
	if p := s.pin.Load(); p != nil {
		o.PinIP, o.PinWarn, o.PinRegion = p.ip, p.warn, p.region
	}
	return o
}

// pinInfo 是定向解析的结果，整体替换避免半写。
type pinInfo struct {
	ip     string
	warn   string
	region string
}

func (s *URLState) pinIP() string {
	if p := s.pin.Load(); p != nil {
		return p.ip
	}
	return ""
}

// stream 是一条活动连接。rate 由调度器每 500ms 回填，用于识别慢流。
type stream struct {
	id     int
	kind   string
	st     *URLState
	ctx    context.Context
	cancel context.CancelFunc
	bytes  atomic.Int64
	rate   atomic.Int64
	born   time.Time
}

type Engine struct {
	profile *Profile
	stats   *Stats

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	streams map[int]*stream
	nextID  int
	dl      []*URLState
	ul      []*URLState

	prober *Prober
	res    *ResSampler
	sched  *scheduler

	// 定向测速：region 为空表示就近（跟随本机 DNS 调度）。
	region   string
	resolver *pinResolver

	stopOnce sync.Once
	stopMu   sync.Mutex
	reason   string

	phase       atomic.Value // string: ramp | steady
	conc        atomic.Int32 // 当前流数
	peakStreams atomic.Int32 // 本次运行达到过的最大流数
}

func NewEngine(p *Profile, st *Stats, region string) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		profile: p, stats: st, ctx: ctx, cancel: cancel,
		streams: make(map[int]*stream),
		res:     NewResSampler(),
		region:  region,
	}
	if region != "" {
		e.resolver = NewPinResolver()
	}
	e.phase.Store("ramp")
	return e
}

// Prepare 并发预检所有端点，按结果填入 Mode/Status。
func (e *Engine) Prepare() {
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for _, ep := range e.profile.DownloadEndpoints {
		ep := ep
		mu.Lock()
		st := &URLState{EP: ep, Kind: "download", Mode: ModeStream, Status: StatusUnknown}
		e.dl = append(e.dl, st)
		mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e.pinEndpoint(st)
			r := ProbeURL(e.ctx, ep)
			if r.Size > 0 && r.Size < shortFileBytes {
				st.Short.Store(true)
			}
			if st.settled.CompareAndSwap(false, true) {
				st.Mode, st.Status, st.Size = r.Mode, r.Status, r.Size
			}
			if r.Err != "" {
				st.setErr(r.Err)
			}
		}()
	}

	for _, ep := range e.profile.UploadEndpoints {
		ep := ep
		mu.Lock()
		st := &URLState{EP: ep, Kind: "upload", Mode: ModeRaw, Status: StatusUnknown}
		e.ul = append(e.ul, st)
		mu.Unlock()

		// 上传端点不做 GET 预检：多数接收端不响应 GET，探测只会得到 405 假阴性。
		if isHomepageURL(ep.URL) {
			st.Status = StatusInvalid
			st.setErr("非直传端点（指向站点首页）")
			continue
		}
		if _, err := url.Parse(ep.URL); err != nil {
			st.Status = StatusInvalid
			st.setErr("URL 无法解析")
			continue
		}
		st.Status = StatusOK
	}
	// 预检只是「探路」，不能让用户干等。国内源通常 1 秒内探完，
	// 探不完的多半是被墙或在限流 —— 与其让用户盯 15 秒空白，
	// 不如 2.5 秒就放行：没探完的按乐观值先跑，真连不上由流内 failover 纠正。
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2500 * time.Millisecond):
		mu.Lock()
		for _, st := range e.dl {
			if st.settled.CompareAndSwap(false, true) {
				st.Status = StatusOK
				st.Mode = ModeStream
				st.setErr("预检未在 2.5s 内完成，先按可用处理")
			}
		}
		mu.Unlock()
	}
}

// pinEndpoint 给一条端点解析「目标节点」。
// 失败不影响压测：回落就近，并在端点上留一句人话说明原因。
func (e *Engine) pinEndpoint(st *URLState) {
	region := st.EP.PinRegion
	if region == "" {
		region = e.region
	}
	if region == "" || e.resolver == nil {
		return
	}
	u, err := url.Parse(st.EP.URL)
	if err != nil || u.Hostname() == "" {
		return
	}
	label := RegionLabel(region)
	if net.ParseIP(u.Hostname()) != nil {
		st.pin.Store(&pinInfo{region: label, warn: "该源直接写死 IP，没有可调度的节点"})
		return
	}
	host := u.Hostname()
	ip, err := e.resolver.Resolve(e.ctx, host, region)
	if err != nil {
		st.pin.Store(&pinInfo{region: label, warn: "定向解析失败，已回落就近节点"})
		return
	}
	// 只记「要打哪」，不猜「有没有打准」。
	// 早期版本拿「ECS 结果是否与本机解析重合」来判断 CDN 认不认 ECS，
	// 在游戏厂这类多跳源上全是误报（入口域名本来就不分省，
	// 真正分省的是 302 之后的下载域名），黄字一多就成了狼来了。
	// 有没有打准，看端点行里的实际拨号 IP 就行。
	st.pin.Store(&pinInfo{ip: ip, region: label})
}

// Start 起初始流（4 条），并启动调度器、探针与资源采样。返回初始流数。
func (e *Engine) Start() int {
	e.stats.Running.Store(true)
	e.stats.SetPhase("ramp")

	// 预检把端点全判死时不要直接收工。预检只是一次带 Range 的 GET，
	// 418/429/403 往往是按 UA 或频率的风控，真实长连接未必一样；
	// 反过来若真连不上，流内 failover + stall 监控也会自己收尾。
	e.forceRunIfBlocked()

	kind := e.pickKind()
	if kind == "" {
		return 0
	}
	for i := 0; i < initialStreams; i++ {
		if e.AddStream(kind) == nil {
			break
		}
	}

	n := e.StreamCount()
	if n == 0 {
		return 0
	}

	if len(e.dl) > 0 {
		e.prober = NewProber(e.dl[0].EP.URL)
		e.prober.Start(e.ctx)
	}
	e.res.Start()

	e.sched = newScheduler(e)
	go e.sched.run()
	go e.watchLimits()
	go e.watchStall()

	return n
}

// forceRunIfBlocked：所有端点都不可用时，把它们降级成「乐观试跑」。
// 只在完全没有可用端点时触发，不会掩盖部分失效的情况。
func (e *Engine) forceRunIfBlocked() {
	e.mu.Lock()
	defer e.mu.Unlock()

	usable := func(list []*URLState) bool {
		for _, st := range list {
			if st.Status == StatusOK {
				return true
			}
		}
		return false
	}
	if usable(e.dl) || usable(e.ul) {
		return
	}
	for _, st := range e.dl {
		st.Status = StatusOK
		if st.Mode == "" {
			st.Mode = ModeStream
		}
		st.setErr("预检判定不可用，已降级为试跑")
	}
	for _, st := range e.ul {
		st.Status = StatusOK
		if st.Mode == "" {
			st.Mode = ModeRaw
		}
	}
}

// pickKind 决定这一轮加的是下载流还是上传流。
func (e *Engine) pickKind() string {
	hasDL, hasUL := len(e.dl) > 0, len(e.ul) > 0
	switch e.profile.Mode {
	case "down":
		if hasDL {
			return "download"
		}
	case "up":
		if hasUL {
			return "upload"
		}
	default:
		if hasDL && hasUL {
			if e.countKind("download") <= e.countKind("upload") {
				return "download"
			}
			return "upload"
		}
	}
	if hasDL {
		return "download"
	}
	if hasUL {
		return "upload"
	}
	return ""
}

func (e *Engine) countKind(kind string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, s := range e.streams {
		if s.kind == kind {
			n++
		}
	}
	return n
}

func (e *Engine) StreamCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.streams)
}

// AddStream 新增一条流。达到上限或没有可用端点时返回 nil。
func (e *Engine) AddStream(kind string) *stream {
	if e.ctx.Err() != nil {
		return nil
	}
	if e.StreamCount() >= maxConcurrency {
		return nil
	}

	var st *URLState
	switch kind {
	case "download":
		if len(e.dl) == 0 {
			return nil
		}
		st = e.pickDL()
		if st == nil {
			return nil
		}
	default:
		if len(e.ul) == 0 {
			return nil
		}
		idx := e.countKind("upload") % len(e.ul)
		st = e.ul[idx]
	}

	ctx, cancel := context.WithCancel(e.ctx)
	e.mu.Lock()
	e.nextID++
	s := &stream{id: e.nextID, kind: kind, st: st, ctx: ctx, cancel: cancel, born: time.Now()}
	e.streams[s.id] = s
	e.mu.Unlock()
	if st != nil {
		st.active.Add(1)
	}

	e.conc.Store(int32(e.StreamCount()))
	if n := int32(e.StreamCount()); n > e.peakStreams.Load() {
		e.peakStreams.Store(n)
	}
	e.wg.Add(1)
	go e.runStream(s)
	return s
}

// KillStream 中断一条流。worker 退出时自行从 map 摘除。
func (e *Engine) KillStream(s *stream) {
	if s == nil {
		return
	}
	s.cancel()
}

// RestartStream 杀掉并重建同类型的流（用于剔除慢流并重置拥塞窗口）。
func (e *Engine) RestartStream(s *stream) {
	if s == nil {
		return
	}
	kind := s.kind
	e.KillStream(s)
	// 异步重建：worker 退出后才会从 map 摘除，稍等可避免瞬时流数超过上限
	safeGo("restartStream", func() {
		time.Sleep(80 * time.Millisecond)
		e.AddStream(kind)
	})
}

func (e *Engine) removeStream(id int) {
	e.mu.Lock()
	s := e.streams[id]
	delete(e.streams, id)
	n := len(e.streams)
	e.mu.Unlock()
	if s != nil && s.st != nil {
		s.st.active.Add(-1)
	}
	e.conc.Store(int32(n))
}

// coverGap 报告是否还有「可用、但不是小文件、却一条流都没分到」的下载源。
// 调度器据此判断要不要做覆盖补漏。
func (e *Engine) coverGap() bool {
	for _, st := range e.dl {
		if st.Status == StatusOK && !st.Short.Load() && st.active.Load() == 0 {
			return true
		}
	}
	return false
}

// slowestDL 返回当前速率最低的下载流，用于把流挪给没覆盖到的源。
func (e *Engine) slowestDL() *stream {
	e.mu.Lock()
	defer e.mu.Unlock()
	var best *stream
	br := int64(1 << 62)
	for _, s := range e.streams {
		if s.kind != "download" {
			continue
		}
		if r := s.rate.Load(); r < br {
			br, best = r, s
		}
	}
	return best
}

// pickDL 选一条最该拿新流的下载源。
//
// 旧实现是「当前下载流数 % URL 条数」：流的重建顺序固定，前几条源就会
// 一直轮到自己，后面的源永远分不到流 —— 屏幕上表现为「只有前五个在发力」。
// 改成按负载挑：谁身上流最少给谁，先把所有源都点亮，再回头加码。
// 小文件（读完即 EOF）不参与爬坡，除非整批源都是小文件。
func (e *Engine) pickDL() *URLState {
	var best, fallback *URLState
	bestN, fbN := int32(1<<30), int32(1<<30)
	for _, st := range e.dl {
		if st.Status != StatusOK {
			continue
		}
		n := st.active.Load()
		if st.Short.Load() {
			if n < fbN {
				fbN, fallback = n, st
			}
			continue
		}
		if n < bestN {
			bestN, best = n, st
		}
	}
	if best != nil {
		return best
	}
	return fallback
}

// StreamRates 返回每流瞬时速率（B/s），调度器用。
func (e *Engine) StreamRates() []float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]float64, 0, len(e.streams))
	for _, s := range e.streams {
		out = append(out, float64(s.rate.Load()))
	}
	return out
}

func (e *Engine) Streams() []*stream {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*stream, 0, len(e.streams))
	for _, s := range e.streams {
		out = append(out, s)
	}
	return out
}

func (e *Engine) SetPhase(p string) {
	e.phase.Store(p)
	e.stats.SetPhase(p)
}

func (e *Engine) Phase() string {
	if v, ok := e.phase.Load().(string); ok {
		return v
	}
	return "ramp"
}

// Stop 触发停止。reason 会出现在状态里。
func (e *Engine) Stop(reason string) {
	e.stopOnce.Do(func() {
		e.stopMu.Lock()
		e.reason = reason
		e.stopMu.Unlock()
		e.cancel()
	})
}

func (e *Engine) StopReason() string {
	e.stopMu.Lock()
	defer e.stopMu.Unlock()
	return e.reason
}

// Wait 等所有 worker 退出，然后写历史。
func (e *Engine) Wait(dataDir string) {
	e.wg.Wait()
	e.stats.Running.Store(false)
	e.SetPhase("done")
	e.stats.endAt.Store(time.Now().UnixMilli())
	e.stats.Tick()

	if e.sched != nil {
		e.sched.stop()
	}
	if e.prober != nil {
		e.prober.Stop()
	}
	e.res.Stop()

	e.stats.Finalize(int(e.peakStreams.Load()))
	snap := e.stats.Snapshot(0, e.StopReason(), e.urlOuts())
	if e.prober != nil {
		snap.Ping = e.prober.Snapshot()
	}
	snap.Res = e.res.Snapshot()
	snap.Concurrency = int(e.peakStreams.Load())
	snap.Verdict = Judge(snap)

	_ = AppendHistory(dataDir, HistoryRec{
		At: time.Now().Unix(), DurationSec: e.profile.DurationSec, Concurrency: int(e.peakStreams.Load()),
		RecvTotal: snap.RecvTotal, SentTotal: snap.SentTotal,
		PeakRecv: snap.PeakRecv, PeakSent: snap.PeakSent,
		SteadyRecv: snap.SteadyRecv, SteadySent: snap.SteadySent,
		AvgRecv: snap.AvgRecv, AvgSent: snap.AvgSent,
		Failures: snap.Failures, StopReason: snap.StopReason,
		Jitter: snap.Ping.Target.Jitter, Loss: snap.Ping.Target.Loss,
		Verdict: snap.Verdict.Text,
	})
}

func (e *Engine) URLStates() []URLOut { return e.urlOuts() }

func (e *Engine) urlOuts() []URLOut {
	out := make([]URLOut, 0, len(e.dl)+len(e.ul))
	for _, s := range e.dl {
		out = append(out, s.out())
	}
	for _, s := range e.ul {
		out = append(out, s.out())
	}
	return out
}

func (e *Engine) PingSnapshot() PingSnapshot {
	if e.prober == nil {
		return PingSnapshot{}
	}
	return e.prober.Snapshot()
}

func (e *Engine) ResSnapshot() ResStat {
	if e.res == nil {
		return ResStat{}
	}
	return e.res.Snapshot()
}

// watchStall 监控「全链路零吞吐」。
//
// 旧实现是「8 秒零字节就判死、20 秒不涨就收工」，在真实网络里太急：
// 目标端限流退避、DNS 抖动、运营商偶发丢包都会让所有流同时进入重试间隙，
// 几秒不涨很常见 —— 用户看到的就是「测速老是中断」。
//
// 现在的策略是先自救、再判死：
//
//	停 15s → 把所有流重开一遍（新连接往往能绕开半死的旧连接）
//	停 30s，或开局 15s 一个字节都没拿到 → 才收工给结论
func (e *Engine) watchStall() {
	defer guard("watchStall")
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()

	var last int64 = -1
	var stallSince time.Time
	gotData := false
	revived := 0
	const (
		reviveAfter = 15 * time.Second
		deadAfter   = 30 * time.Second
		deadNoData  = 15 * time.Second
	)
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-t.C:
			cur := e.stats.RecvBytes.Load() + e.stats.SentBytes.Load()
			if cur > 0 {
				gotData = true
			}
			if cur != last {
				last = cur
				stallSince = time.Time{}
				revived = 0
				continue
			}
			if stallSince.IsZero() {
				stallSince = now
				continue
			}
			idle := now.Sub(stallSince)

			// 自救：重开全部流，最多两次，别陷入无限重启
			if idle >= reviveAfter && revived < 2 {
				revived++
				for _, s := range e.Streams() {
					e.RestartStream(s)
				}
				continue
			}

			limit := deadAfter
			if !gotData {
				limit = deadNoData
			}
			if idle >= limit {
				if !gotData {
					e.Stop("15 秒一个字节都没拿到：目标源全都连不上。换源，或点「网络自检」看卡在哪一层")
				} else {
					e.Stop("目标端 30 秒无任何数据（已自动重连 2 次）")
				}
				return
			}
		}
	}
}

// watchLimits 每 500ms 检查一次时长和流量上限。
func (e *Engine) watchLimits() {
	defer guard("watchLimits")
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			if e.profile.DurationSec > 0 && time.Since(e.stats.StartAt).Seconds() >= float64(e.profile.DurationSec) {
				e.Stop("到达时长上限")
				return
			}
			if e.profile.BytesLimit > 0 {
				total := e.stats.RecvBytes.Load() + e.stats.SentBytes.Load()
				if total >= e.profile.BytesLimit {
					e.Stop("到达流量上限")
					return
				}
			}
		}
	}
}

func (e *Engine) runStream(s *stream) {
	defer e.wg.Done()
	defer guard("runStream")
	defer e.removeStream(s.id)
	if s.kind == "download" {
		e.downloadLoop(s)
	} else {
		e.uploadLoop(s)
	}
}

// ---------------- 下载 ----------------

func (e *Engine) downloadLoop(s *stream) {
	// 定向模式下把解析器交给 client：302 换域名时要能按同一个位置重新解析。
	region := s.st.EP.PinRegion
	if region == "" {
		region = e.region
	}
	var resolve *pinResolver
	if e.resolver != nil && region != "" {
		resolve = e.resolver
	}
	client := newTransferClient(resolve, region)
	buf := make([]byte, readBufSize)
	backoff := 200 * time.Millisecond
	rnd := rand.New(rand.NewSource(time.Now().UnixNano() + int64(s.id)))
	cur := s.st

	for s.ctx.Err() == nil {
		before := s.bytes.Load()
		err := e.downloadOnce(s, cur, client, buf, rnd)
		got := s.bytes.Load() - before

		if err == nil || errors.Is(err, context.Canceled) {
			// 服务端秒断（拿到的字节极少）时不能无间隔重连，否则空转烧 CPU
			// 却一根毛的吞吐都没有，表现就是「跑一会就歇菜」。
			// 正常读完（EOF）也要抖开一点：多条流几乎同时到尾部时，
			// 一起重连会把速率砸出一个深坑，看着就是曲线在跳。
			wait := 10*time.Millisecond + time.Duration(rnd.Int63n(int64(70*time.Millisecond)))
			if got < 64<<10 && err == nil {
				wait = 120 * time.Millisecond
			}
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(wait):
			}
			backoff = 200 * time.Millisecond
			continue
		}
		cur.Failures.Add(1)
		cur.setErr(err.Error())
		e.stats.Failures.Add(1)

		if cur.Failures.Load() >= maxFailBeforeRot {
			if next := e.nextDL(cur); next != nil {
				next.Failures.Store(0)
				cur = next
			}
			cur.Failures.Store(0)
			backoff = 500 * time.Millisecond
		}

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (e *Engine) nextDL(cur *URLState) *URLState {
	idx := -1
	for i, s := range e.dl {
		if s == cur {
			idx = i
			break
		}
	}

	// 第一轮只挑预检通过的端点。
	for i := 1; i <= len(e.dl); i++ {
		cand := e.dl[(idx+i)%len(e.dl)]
		if cand != cur && cand.Status == StatusOK {
			return cand
		}
	}
	// 全都被标成不可用时，退而轮换到任意另一个端点：
	// 预检结论可能已经过期（限流解除、签名刷新），总比死磕同一个 418 强。
	for i := 1; i <= len(e.dl); i++ {
		cand := e.dl[(idx+i)%len(e.dl)]
		if cand != cur {
			return cand
		}
	}
	return nil
}

func (e *Engine) downloadOnce(s *stream, st *URLState, client *http.Client, buf []byte, rnd *rand.Rand) error {
	// pinIP 非空时，请求上下文里带着目标节点，Transport 拨号会直接用它。
	reqCtx := s.ctx
	if ip := st.pinIP(); ip != "" {
		if u, err := url.Parse(st.EP.URL); err == nil {
			reqCtx = withPinIP(reqCtx, u.Hostname(), ip)
		}
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, st.EP.URL, nil)
	if err != nil {
		return err
	}
	for k, v := range st.EP.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", browserUA)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")

	// 随机起点 + 读到文件尾。两条收益：
	//   1) 同一 URL 被多条流同时压时，各流从不同位置起读，读完的时刻自然错开，
	//      不会出现「所有流同时到达 EOF、同时重连、速率集体归零」的锯齿。
	//   2) 一条请求能连续读很久，重连频率大幅下降。旧实现固定 32MB 分片，
	//      在千兆链路上 0.2 秒就下一块，等于每秒握手好几次。
	// 服务器不认 Range 时返回 200 全量，行为退化成从头读，不影响正确性。
	switch st.Mode {
	case ModeChunkLoop, ModeFileLoop:
		if st.Size > 0 {
			span := int64(256 << 20)
			if st.Size <= span {
				span = st.Size / 2
			}
			var start int64
			if span > 0 {
				start = rnd.Int63n(span)
			}
			if start > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
			}
		}
	}

	// 记下实际连上的对端地址：界面上的「命中 IP」由此而来，不额外发请求。
	var remote string
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn == nil {
				return
			}
			if host, _, err := net.SplitHostPort(info.Conn.RemoteAddr().String()); err == nil {
				remote = host
			} else {
				remote = info.Conn.RemoteAddr().String()
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	resp, err := client.Do(req)
	if remote != "" {
		st.setRemote(remote)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden && isTimeLimitedURL(st.EP.URL) {
		st.Status = StatusExpired
		return fmt.Errorf("HTTP 403，签名失效")
	}
	if resp.StatusCode == 418 {
		st.Status = StatusInvalid
		st.setErr("HTTP 418，目标端风控拦截")
		return fmt.Errorf("HTTP 418，目标端风控拦截")
	}
	if resp.StatusCode == 429 {
		return fmt.Errorf("HTTP 429，目标端限流")
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		st.Status = StatusInvalid
		return fmt.Errorf("HTTP %d，端点不可用", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 循环读进复用 buffer，热路径零分配。
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			s.bytes.Add(int64(n))
			st.Recv.Add(int64(n))
			e.stats.RecvBytes.Add(int64(n))
		}
		if rerr != nil {
			if rerr == io.EOF {
				st.Reconnects.Add(1)
				e.stats.Reconnects.Add(1)
				return nil
			}
			return rerr
		}
		if s.ctx.Err() != nil {
			return context.Canceled
		}
	}
}

// ---------------- 上传 ----------------

// errPeerReset 表示「连接建立好了，但对方不接受这个 body」——
// 413 / 405 / 鉴权拒绝 / Connection: close 后不等 body 就 FIN，全是这个样子。
//
// 它必须和普通错误分开：普通错误要按 200ms→5s 指数退避，而这一种在
// 上传里太常见（server 收够就走），只有「秒断」才需要放慢，正常传完换连不该等。
var errPeerReset = errors.New("对端关闭了连接")

// uploadLoop 的重连节奏与 downloadLoop 同一套逻辑：
// 允许快速换连，但对「一次只活了几毫秒」的尝试必须退避。
//
// 早先这里在成功路径上直接 continue，一次等待都没有。而 uploadOnce 把
// EPIPE / ECONNRESET 都当成「正常结束」返回 nil，两者一相接就成了纯握手热循环：
// 建 TCP → 发头 → 被 FIN → 立刻重来，一个核的 100% 全花在握手上，上行速率是 0。
//
// 秒断只记在「这条端点」的失败数上，不记全局 Failures —— 那个计数器喂的是
// 调度器的失败风暴保护（一轮 ≥ 6 次就暂停加流），而它是全局、不分上下行的。
// 双向模式下上传端点是坏的、下载源是好的时，把它算进全局会让下载侧的爬坡
// 被永久按在起始并发上，测出来的下载速度严重偏低且不报错 —— 比原来的 bug 更隐蔽。
func (e *Engine) uploadLoop(s *stream) {
	backoff := 200 * time.Millisecond
	rnd := rand.New(rand.NewSource(time.Now().UnixNano() + int64(s.id)))
	cur := s.st

	// 一次尝试存活不到这个时长，就按「秒断」处理。
	// 用时长而不是字节数判断：对端在 socket 缓冲区还没撑满时就 RST，
	// 客户端可能已经塞进去几百 KB，字节数区分不出「秒断」和「传了一会」。
	const quickReset = 250 * time.Millisecond

	for s.ctx.Err() == nil {
		start := time.Now()
		err := e.uploadOnce(s, cur)
		elapsed := time.Since(start)

		if s.ctx.Err() != nil { // 用户点了停止，不记账也不重连
			return
		}

		if errors.Is(err, errPeerReset) {
			if elapsed < quickReset {
				cur.Failures.Add(1)
				cur.setErr("连接建立后未传输数据即被断开，已放慢重连")
				select {
				case <-s.ctx.Done():
					return
				case <-time.After(150*time.Millisecond + time.Duration(rnd.Int63n(int64(100*time.Millisecond)))):
				}
				continue
			}
			// 传了一会才断：正常换连，只抖开一点，避免多条流同时重连砸出深坑
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(10*time.Millisecond + time.Duration(rnd.Int63n(int64(70*time.Millisecond)))):
			}
			backoff = 200 * time.Millisecond
			continue
		}

		if err == nil || errors.Is(err, context.Canceled) {
			backoff = 200 * time.Millisecond
			continue
		}
		cur.Failures.Add(1)
		cur.setErr(err.Error())
		e.stats.Failures.Add(1)

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (e *Engine) uploadOnce(s *stream, st *URLState) error {
	u, err := url.Parse(st.EP.URL)
	if err != nil {
		return err
	}
	port := u.Port()
	useTLS := u.Scheme == "https"
	if port == "" {
		if useTLS {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(u.Hostname(), port)

	conn, err := newDialer().DialContext(s.ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	if useTLS {
		tc := tls.Client(conn, &tls.Config{
			ServerName:         u.Hostname(),
			ClientSessionCache: tlsSessionCache, // 重连可复用会话，省掉整条证书链的校验
		})
		if err := tc.HandshakeContext(s.ctx); err != nil {
			return fmt.Errorf("TLS 握手失败（https 上传会先打满 CPU）: %w", err)
		}
		conn = tc
	}

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	hdr := strings.Builder{}
	hdr.WriteString("PUT " + path + " HTTP/1.1\r\n")
	hdr.WriteString("Host: " + u.Host + "\r\n")
	hdr.WriteString("User-Agent: " + browserUA + "\r\n")
	hdr.WriteString("Content-Type: application/octet-stream\r\n")
	hdr.WriteString(fmt.Sprintf("Content-Length: %d\r\n", uploadBodyLen))
	for k, v := range st.EP.Headers {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Host") {
			continue
		}
		hdr.WriteString(k + ": " + v + "\r\n")
	}
	hdr.WriteString("Connection: close\r\n\r\n")

	if _, err := io.WriteString(conn, hdr.String()); err != nil {
		return err
	}

	f, err := os.Open("/dev/zero")
	if err != nil {
		return fmt.Errorf("打开 /dev/zero 失败: %w", err)
	}
	defer f.Close()

	stop := make(chan struct{})
	safeGo("uploadDeadline", func() {
		select {
		case <-s.ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-stop:
		}
	})
	defer close(stop)

	_, err = io.Copy(&countingWriter{conn: conn, s: s, st: st, g: e.stats}, f)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrDeadlineExceeded) {
			st.Reconnects.Add(1)
			e.stats.Reconnects.Add(1)
			// 不返回 nil：nil 在循环里等于「传完了、可以马上换连」，
			// 而被对端关掉是另一件事，要不要退避由调用方按存活时长决定。
			return errPeerReset
		}
		return err
	}
	return nil
}

type countingWriter struct {
	conn net.Conn
	s    *stream
	st   *URLState
	g    *Stats
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.conn.Write(p)
	if n > 0 {
		w.s.bytes.Add(int64(n))
		w.st.Sent.Add(int64(n))
		w.g.SentBytes.Add(int64(n))
	}
	return n, err
}

// ---------------- 连接工厂 ----------------

func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
				if sockBufSize > 0 {
					_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, sockBufSize)
					_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, sockBufSize)
				}
			})
		},
	}
}

func newTransferClient(resolve *pinResolver, region string) *http.Client {
	d := newDialer()
	tr := &http.Transport{
		// 定向测速：上下文里带 pinIP 就把地址换成它，端口沿用原值。
		// TLS 的 SNI 和证书校验仍然按 URL 里的域名走，所以 CDN 不会察觉。
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if p := pinFrom(ctx); p != nil {
				if host, port, err := net.SplitHostPort(addr); err == nil && host == p.host {
					addr = net.JoinHostPort(p.ip, port)
				}
			}
			return d.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     false,
		DisableCompression:    true,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   0,
		IdleConnTimeout:       0,
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 20 * time.Second,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, ClientSessionCache: tlsSessionCache},
	}
	return &http.Client{
		Transport: tr,
		Timeout:   0,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("重定向超过 10 跳，疑似镜像站处理 Range 时死循环")
			}
			// 跳到已经访问过的地址 = 死循环，立刻掐断，别耗到超时。
			// 镜像站的 current / latest 软链接最容易出这种问题。
			for _, prev := range via {
				if prev.URL.String() == req.URL.String() {
					return fmt.Errorf("重定向死循环（%s 反复跳回同一地址）", req.URL.Host)
				}
			}
			// 定向模式下 302 换了域名：旧 IP 对新域名没有意义，得按同一个位置
			// 重新解析一次。游戏厂的 latest 入口就是两跳跨三个域名。
			if resolve != nil && region != "" {
				host := req.URL.Hostname()
				if p := pinFrom(req.Context()); p == nil || p.host != host {
					if ip, err := resolve.Resolve(req.Context(), host, region); err == nil {
						*req = *req.WithContext(withPinIP(req.Context(), host, ip))
					}
				}
			}
			return nil
		},
	}
}

func newProbeClient() *http.Client {
	c := newTransferClient(nil, "")
	c.Timeout = probeTimeout
	return c
}
