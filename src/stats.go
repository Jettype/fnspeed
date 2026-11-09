package main

// 统计：原子计数 + 环形缓冲采样。
// 热路径只做 atomic.Add，不碰锁；前端 500ms 拉一次快照。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const slowStartSec = 3.0 // 慢启动隔离窗口：前 3 秒不计入平均与稳定值

// Sample 是一次采样点。Recv/Sent 是累计字节数，速率由相邻两点差分得出。
type Sample struct {
	T    int64 `json:"t"`
	Recv int64 `json:"r"`
	Sent int64 `json:"s"`
}

// Ring 是固定容量的环形缓冲，满了覆盖最旧的点。
type Ring struct {
	mu   sync.RWMutex
	buf  []Sample
	head int
	size int
}

func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 2048
	}
	return &Ring{buf: make([]Sample, capacity)}
}

func (r *Ring) Push(s Sample) {
	r.mu.Lock()
	r.buf[r.head] = s
	r.head++
	if r.head >= len(r.buf) {
		r.head = 0
	}
	if r.size < len(r.buf) {
		r.size++
	}
	r.mu.Unlock()
}

func (r *Ring) Snapshot(limit int) []Sample {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := r.size
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Sample, 0, n)
	start := r.head - n
	for i := 0; i < n; i++ {
		idx := (start + i) % len(r.buf)
		if idx < 0 {
			idx += len(r.buf)
		}
		out = append(out, r.buf[idx])
	}
	return out
}

type Stats struct {
	// startAtMs 是本次运行起点（Unix 毫秒），0 表示还没开始。
	// 用原子量而不是 time.Time：开始一局时 Reset 会写它，而上一个引擎的
	// scheduler / watchLimits 可能还没退干净，两边同时读写 time.Time
	// （三个机器字）会读出半写值，算出来的 elapsed 直接是天文数字。
	startAtMs  atomic.Int64
	RecvBytes  atomic.Int64
	SentBytes  atomic.Int64
	Failures   atomic.Int64
	Reconnects atomic.Int64
	Running    atomic.Bool

	Compensations atomic.Int64
	SlowDropped   atomic.Int64

	peakRecv atomic.Int64
	peakSent atomic.Int64

	steadyRecv   atomic.Int64
	steadySent   atomic.Int64
	finalized    atomic.Bool
	streamsFinal atomic.Int32

	// 结束时的平均速率，在 Finalize 里定格。
	// 不能在 Snapshot 里现算：分母（耗时）停表后不动，分子只要再涨一点，
	// 平均值就会无上限地往上爬，最后出现「平均 3.10G 而峰值只有 1.01G」这种
	// 物理上不可能的数。
	avgRecvFinal atomic.Int64
	avgSentFinal atomic.Int64
	avgFinal     atomic.Bool
	// 结束时的累计流量。健康情况下与实时值相同；一旦结束后还有字节进来
	// （引擎没收干净），界面上的「累计流量」也不该跟着涨 —— 那些字节不属于本次运行。
	recvFinal atomic.Int64
	sentFinal atomic.Int64

	phase atomic.Value // string
	endAt atomic.Int64 // 停止时刻，毫秒；0 表示未结束

	ringMu sync.RWMutex
	ring   *Ring

	mu      sync.Mutex
	lastAt  time.Time
	lastRec int64
	lastSen int64

	// Tick 算出来的瞬时速率。Snapshot 直接读这两个值，
	// 不再自己拿 lastRec 重算 —— SSE 每 500ms 推一帧，几乎总是紧跟在
	// Tick 之后，此时自算的 dt≈0、差值也≈0，算出来恒为 0，
	// 表现就是「累计流量在涨、大号速率一直 0」。
	lastRR atomic.Int64
	lastRS atomic.Int64
}

func NewStats() *Stats {
	s := &Stats{ring: NewRing(6000)}
	s.phase.Store("idle")
	return s
}

func (s *Stats) SetPhase(p string) { s.phase.Store(p) }

// SetStartAt 记录本次运行的起点。
func (s *Stats) SetStartAt(t time.Time) { s.startAtMs.Store(t.UnixMilli()) }

// StartAt 返回本次运行的起点，零值表示还没开始。
func (s *Stats) StartAt() time.Time {
	ms := s.startAtMs.Load()
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func (s *Stats) Phase() string {
	if v, ok := s.phase.Load().(string); ok {
		return v
	}
	return "idle"
}

func (s *Stats) Reset() {
	s.SetStartAt(time.Now())
	s.RecvBytes.Store(0)
	s.SentBytes.Store(0)
	s.Failures.Store(0)
	s.Reconnects.Store(0)
	s.Compensations.Store(0)
	s.SlowDropped.Store(0)
	s.peakRecv.Store(0)
	s.peakSent.Store(0)
	s.ringMu.Lock()
	s.ring = NewRing(6000)
	s.ringMu.Unlock()
	s.phase.Store("ramp")
	s.finalized.Store(false)
	s.endAt.Store(0)
	s.steadyRecv.Store(0)
	s.steadySent.Store(0)
	s.avgRecvFinal.Store(0)
	s.avgSentFinal.Store(0)
	s.avgFinal.Store(false)
	s.recvFinal.Store(0)
	s.sentFinal.Store(0)
	s.mu.Lock()
	s.lastAt = time.Time{}
	s.lastRec = 0
	s.lastSen = 0
	s.lastRR.Store(0)
	s.lastRS.Store(0)
	s.mu.Unlock()
}

func (s *Stats) Tick() {
	now := time.Now()
	rec := s.RecvBytes.Load()
	sen := s.SentBytes.Load()

	s.ringMu.RLock()
	rd := s.ring
	s.ringMu.RUnlock()
	rd.Push(Sample{T: now.UnixMilli(), Recv: rec, Sent: sen})

	s.mu.Lock()
	if !s.lastAt.IsZero() {
		dt := now.Sub(s.lastAt).Seconds()
		if dt > 0 {
			rr := int64(float64(rec-s.lastRec) / dt)
			rs := int64(float64(sen-s.lastSen) / dt)
			// 换局瞬间计数器被 Reset 归零，而上一局的 Tick 可能还在跑：
			// 差值会变成负数，直接显示就是「-1829000000 bps」。
			if rr < 0 {
				rr = 0
			}
			if rs < 0 {
				rs = 0
			}
			s.lastRR.Store(rr)
			s.lastRS.Store(rs)
			if rr > s.peakRecv.Load() {
				s.peakRecv.Store(rr)
			}
			if rs > s.peakSent.Load() {
				s.peakSent.Store(rs)
			}
		}
	}
	s.lastAt = now
	s.lastRec = rec
	s.lastSen = sen
	s.mu.Unlock()

	if s.Running.Load() && !s.finalized.Load() {
		s.refreshSteady()
	}
}

// refreshSteady 运行中实时刷新稳定值：丢弃前 3 秒与后 10%，取中位数。
// 每 500ms 一次、最多 600 个样本，开销可忽略。
func (s *Stats) refreshSteady() {
	s.ringMu.RLock()
	rd := s.ring
	s.ringMu.RUnlock()
	samples := rd.Snapshot(600)
	if len(samples) < 4 {
		return
	}
	base := s.StartAt().UnixMilli()
	var recv, sent []float64
	for i := 1; i < len(samples); i++ {
		dt := float64(samples[i].T-samples[i-1].T) / 1000
		if dt <= 0 {
			continue
		}
		if float64(samples[i].T-base)/1000 < slowStartSec {
			continue
		}
		recv = append(recv, float64(samples[i].Recv-samples[i-1].Recv)/dt)
		sent = append(sent, float64(samples[i].Sent-samples[i-1].Sent)/dt)
	}
	keep := int(float64(len(recv)) * 0.9)
	if keep < 1 {
		return
	}
	s.steadyRecv.Store(int64(median(recv[:keep])))
	s.steadySent.Store(int64(median(sent[:keep])))
}

// Finalize 计算稳定值：丢弃前 3 秒与后 10% 的样本，剩余取中位数。
func (s *Stats) Finalize(streams int) {
	if s.finalized.Swap(true) {
		return
	}
	s.streamsFinal.Store(int32(streams))

	// 先把总量与平均值定格：后面几个分支都可能提前 return，但这两项必须留下。
	s.recvFinal.Store(s.RecvBytes.Load())
	s.sentFinal.Store(s.SentBytes.Load())
	if v := s.endAt.Load(); v > 0 {
		if start := s.StartAt(); !start.IsZero() {
			if elapsed := time.UnixMilli(v).Sub(start).Seconds(); elapsed > slowStartSec {
				s.avgRecvFinal.Store(clampToPeak(int64(float64(s.RecvBytes.Load())/elapsed), s.peakRecv.Load()))
				s.avgSentFinal.Store(clampToPeak(int64(float64(s.SentBytes.Load())/elapsed), s.peakSent.Load()))
				s.avgFinal.Store(true)
			}
		}
	}

	s.ringMu.RLock()
	rd := s.ring
	s.ringMu.RUnlock()
	samples := rd.Snapshot(0)
	if len(samples) < 4 {
		return
	}
	base := s.StartAt().UnixMilli()

	var recv, sent []float64
	for i := 1; i < len(samples); i++ {
		dt := float64(samples[i].T-samples[i-1].T) / 1000
		if dt <= 0 {
			continue
		}
		rel := float64(samples[i].T-base) / 1000
		if rel < slowStartSec {
			continue
		}
		recv = append(recv, float64(samples[i].Recv-samples[i-1].Recv)/dt)
		sent = append(sent, float64(samples[i].Sent-samples[i-1].Sent)/dt)
	}
	keep := int(float64(len(recv)) * 0.9)
	if keep < 1 {
		return
	}
	s.steadyRecv.Store(int64(median(recv[:keep])))
	s.steadySent.Store(int64(median(sent[:keep])))
}

type Snapshot struct {
	Running       bool         `json:"running"`
	Phase         string       `json:"phase"` // ramp | steady | idle
	ElapsedSec    float64      `json:"elapsedSec"`
	Concurrency   int          `json:"concurrency"`
	RecvTotal     int64        `json:"recvTotal"`
	SentTotal     int64        `json:"sentTotal"`
	RecvRate      int64        `json:"recvRate"`
	SentRate      int64        `json:"sentRate"`
	PeakRecv      int64        `json:"peakRecv"`
	PeakSent      int64        `json:"peakSent"`
	SteadyRecv    int64        `json:"steadyRecv"`
	SteadySent    int64        `json:"steadySent"`
	AvgRecv       int64        `json:"avgRecv"`
	AvgSent       int64        `json:"avgSent"`
	Failures      int64        `json:"failures"`
	Reconnects    int64        `json:"reconnects"`
	Compensations int64        `json:"compensations"`
	SlowDropped   int64        `json:"slowDropped"`
	Series        []Sample     `json:"series"`
	StopReason    string       `json:"stopReason,omitempty"`
	URLStates     []URLOut     `json:"urlStates"`
	Ping          PingSnapshot `json:"ping"`
	Res           ResStat      `json:"res"`
	Verdict       Verdict      `json:"verdict"`
}

type URLOut struct {
	URL        string `json:"url"`
	Kind       string `json:"kind"`
	Mode       string `json:"mode"`
	Status     string `json:"status"`
	Recv       int64  `json:"recv"`
	Sent       int64  `json:"sent"`
	Reconnects int64  `json:"reconnects"`
	Failures   int64  `json:"failures"`
	// RemoteIP 是这条源实际连上的对端地址，取自连接建立那一刻，
	// 空闲重启后仍然有效，不需要额外探测。
	RemoteIP  string `json:"remoteIp,omitempty"`
	LastError string `json:"lastError,omitempty"`

	// 定向测速：PinIP 是 ECS 指到的节点，PinWarn 是「没按预期生效」的说明。
	PinIP     string `json:"pinIp,omitempty"`
	PinWarn   string `json:"pinWarn,omitempty"`
	PinRegion string `json:"pinRegion,omitempty"`
}

// clampToPeak 保证「平均不超过峰值」。平均是窗口均值，本来就不可能高于
// 采样到的最大值；一旦越界，说明统计窗口出了问题，此时宁可显示峰值，
// 也不要把一个物理上不可能的数字摆到用户面前。
func clampToPeak(avg, peak int64) int64 {
	if peak > 0 && avg > peak {
		return peak
	}
	return avg
}

func (s *Stats) series(limit int) []Sample {
	s.ringMu.RLock()
	rd := s.ring
	s.ringMu.RUnlock()
	if rd == nil {
		return nil
	}
	return rd.Snapshot(limit)
}

func (s *Stats) Snapshot(seriesLimit int, stopReason string, urls []URLOut) Snapshot {
	rec := s.RecvBytes.Load()
	sen := s.SentBytes.Load()

	// 直接取 Tick（500ms 一次）算好的瞬时速率。
	// 早先这里用 lastRec 现场重算，但 SSE 几乎总是紧跟着 Tick 触发，
	// dt 接近 0、差值也接近 0，商出来恒等于 0 —— 就是「累计流量在涨、
	// 大号速率一直显示 0 bps」的原因。
	rr := s.lastRR.Load()
	rs := s.lastRS.Load()
	if !s.Running.Load() {
		rr, rs = 0, 0
	}

	elapsed := 0.0
	if start := s.StartAt(); !start.IsZero() {
		end := time.Now()
		if !s.Running.Load() {
			if v := s.endAt.Load(); v > 0 {
				end = time.UnixMilli(v)
			}
		}
		elapsed = end.Sub(start).Seconds()
		if elapsed < 0 {
			elapsed = 0
		}
	}
	snap := Snapshot{
		Running:       s.Running.Load(),
		Phase:         s.Phase(),
		ElapsedSec:    elapsed,
		Concurrency:   int(s.streamsFinal.Load()),
		RecvTotal:     rec,
		SentTotal:     sen,
		RecvRate:      rr,
		SentRate:      rs,
		PeakRecv:      s.peakRecv.Load(),
		PeakSent:      s.peakSent.Load(),
		SteadyRecv:    s.steadyRecv.Load(),
		SteadySent:    s.steadySent.Load(),
		Failures:      s.Failures.Load(),
		Reconnects:    s.Reconnects.Load(),
		Compensations: s.Compensations.Load(),
		SlowDropped:   s.SlowDropped.Load(),
		Series:        s.series(seriesLimit),
		StopReason:    stopReason,
		URLStates:     urls,
	}
	if elapsed > slowStartSec {
		snap.AvgRecv = int64(float64(rec) / elapsed)
		snap.AvgSent = int64(float64(sen) / elapsed)
	}
	// 结束后一律用 Finalize 定格的数：分母（耗时）已停表，再拿活的总量去除，
	// 只要还有一丁点字节进来，平均值就会无上限往上爬。
	if !snap.Running && s.finalized.Load() {
		snap.RecvTotal = s.recvFinal.Load()
		snap.SentTotal = s.sentFinal.Load()
	}
	if !snap.Running && s.avgFinal.Load() {
		snap.AvgRecv = s.avgRecvFinal.Load()
		snap.AvgSent = s.avgSentFinal.Load()
	}
	snap.AvgRecv = clampToPeak(snap.AvgRecv, snap.PeakRecv)
	snap.AvgSent = clampToPeak(snap.AvgSent, snap.PeakSent)
	return snap
}

type HistoryRec struct {
	At          int64   `json:"at"`
	DurationSec int     `json:"durationSec"`
	Concurrency int     `json:"concurrency"`
	RecvTotal   int64   `json:"recvTotal"`
	SentTotal   int64   `json:"sentTotal"`
	PeakRecv    int64   `json:"peakRecv"`
	PeakSent    int64   `json:"peakSent"`
	SteadyRecv  int64   `json:"steadyRecv"`
	SteadySent  int64   `json:"steadySent"`
	AvgRecv     int64   `json:"avgRecv"`
	AvgSent     int64   `json:"avgSent"`
	Failures    int64   `json:"failures"`
	StopReason  string  `json:"stopReason"`
	Jitter      float64 `json:"jitter"`
	Loss        float64 `json:"loss"`
	Verdict     string  `json:"verdict"`
}

// 历史文件的两个上限：超过 historyMaxBytes 就只保留最后 historyKeep 行。
// 这是 NAS 上的长驻程序，一天跑十几轮、一年攒出几十 MB 也没人清理；
// /api/history 又是整文件读进来再截断的，必须有个天花板。
const (
	historyMaxBytes = 1 << 20
	historyKeep     = 200
)

func AppendHistory(dataDir string, rec HistoryRec) error {
	if dataDir == "" {
		return nil
	}
	path := filepath.Join(dataDir, "history.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		_ = f.Close()
		return err
	}
	b = append(b, '\n')
	_, err = f.Write(b)
	fi, statErr := f.Stat()
	_ = f.Close()
	if err != nil {
		return err
	}
	if statErr == nil && fi.Size() > historyMaxBytes {
		_ = trimHistory(path, historyKeep)
	}
	return nil
}

// trimHistory 只留最后 keep 行。失败不报错 —— 历史记录不是关键路径，
// 写不进去也不该影响本次测速的结果。
func trimHistory(path string, keep int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) <= keep {
		return nil
	}
	lines = lines[len(lines)-keep:]
	out := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(out), 0644)
}
