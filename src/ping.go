package main

// 并行双探针：目标 host（TCP 握手 RTT）+ 默认网关（DNS/HTTP/HTTPS 握手 RTT），1s 一次。
// 应用以 package 用户运行，拿不到 CAP_NET_RAW，无法用 ICMP 原始套接字；
// TCP/UDP 握手 RTT 与 ICMP RTT 的差异在 1ms 量级，足以判断链路抖动与丢包。

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type PingResult struct {
	Host   string  `json:"host"`
	Method string  `json:"method"` // 实际生效的探测方式，如 udp/53、tcp/80
	AvgRTT float64 `json:"avgRtt"` // ms
	Jitter float64 `json:"jitter"` // ms，相邻样本差的绝对值均值
	Loss   float64 `json:"loss"`   // 0-1
	Sent   int     `json:"sent"`
}

type PingSnapshot struct {
	Target  PingResult `json:"target"`
	Gateway PingResult `json:"gateway"`
}

type pingSeries struct {
	mu   sync.Mutex
	host string
	rtts []float64 // 0 表示丢包
	cap  int
}

func newPingSeries(host string, cap int) *pingSeries {
	return &pingSeries{host: host, cap: cap}
}

// probeStatus 区分「主动拒绝」和「压测自己把探针挤到超时」。
// 满速下载时 TCP 握手经常 >1s，那不是丢包，不能让判定背锅。
type probeStatus int

const (
	probeHit probeStatus = iota
	probeWait
	probeRejected
)

func (p *pingSeries) add(ms float64) {
	p.mu.Lock()
	p.rtts = append(p.rtts, ms)
	if len(p.rtts) > p.cap {
		p.rtts = p.rtts[len(p.rtts)-p.cap:]
	}
	p.mu.Unlock()
}

func (p *pingSeries) snapshot() PingResult {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := PingResult{Host: p.host, Sent: len(p.rtts)}
	if len(p.rtts) == 0 {
		return out
	}
	var sum, ok int
	var loss int
	var jsum float64
	var jn int
	var prev float64
	for _, v := range p.rtts {
		if v < 0 {
			continue // -1 = 探针自身超时，不计入丢包分母
		}
		if v == 0 {
			loss++
			continue
		}
		sum += int(v * 1000)
		ok++
		if prev > 0 {
			d := v - prev
			if d < 0 {
				d = -d
			}
			jsum += d
			jn++
		}
		prev = v
	}
	valid := len(p.rtts)
	for _, v := range p.rtts {
		if v < 0 {
			valid--
		}
	}
	if valid > 0 {
		out.Loss = float64(loss) / float64(valid)
	}
	if ok > 0 {
		out.AvgRTT = float64(sum) / 1000 / float64(ok)
	}
	if jn > 0 {
		out.Jitter = jsum / float64(jn)
	}
	return out
}

type Prober struct {
	target  *pingSeries
	gateway *pingSeries
	gwAddr  string
	tcpAddr string

	// 网关探测方式：不同路由器开放的服务不一样，按顺序探测并记住可用的那个。
	gwMode   atomic.Int32
	gwMethod atomic.Value // string
	tcpMode  atomic.Value // string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// 网关探测候选（下标即 gwMode）
// 0: UDP/53 最小 DNS 查询   1: TCP/53   2: TCP/80   3: TCP/443   4: TCP/22
var gwCandidates = []string{"udp/53", "tcp/53", "tcp/80", "tcp/443", "tcp/22"}
var gwPorts = []string{"53", "53", "80", "443", "22"}

// NewProber 从压测目标里取一个主机，网关从 /proc/net/route 读。
func NewProber(targetURL string) *Prober {
	p := &Prober{}
	if u, err := url.Parse(targetURL); err == nil && u.Hostname() != "" {
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		p.tcpAddr = net.JoinHostPort(u.Hostname(), port)
		p.target = newPingSeries(u.Hostname(), 30)
	}
	p.gwAddr = defaultGateway()
	if p.gwAddr != "" {
		p.gateway = newPingSeries(p.gwAddr, 30)
	}
	p.gwMethod.Store("")
	p.tcpMode.Store("")
	return p
}

func (p *Prober) Start(parent context.Context) {
	if p.target == nil && p.gateway == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel

	if p.target != nil {
		p.wg.Add(1)
		go p.loop(ctx, p.target, p.probeTCP)
	}
	if p.gateway != nil {
		p.wg.Add(1)
		go p.loop(ctx, p.gateway, p.probeGateway)
	}
}

func (p *Prober) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

func (p *Prober) loop(ctx context.Context, s *pingSeries, fn func(context.Context) (float64, probeStatus)) {
	defer p.wg.Done()
	defer guard("prober")
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ms, st := fn(ctx)
			switch st {
			case probeHit:
				s.add(ms)
			case probeRejected:
				s.add(0) // 只有明确被拒绝/不可达才算丢包
			default:
				s.add(-1) // 超时不参与丢包统计
			}
		}
	}
}

func (p *Prober) probeTCP(ctx context.Context) (float64, probeStatus) {
	// 3s：满速下载时握手排在本机网卡队列后面，1.2s 太容易误判
	d := net.Dialer{Timeout: 3 * time.Second}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", p.tcpAddr)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return 0, probeWait
		}
		return 0, probeRejected
	}
	_ = conn.Close()
	p.tcpMode.Store("tcp/" + portOf(p.tcpAddr))
	return float64(time.Since(start).Microseconds()) / 1000, probeHit
}

// probeGateway 依次尝试 DNS / HTTP / HTTPS / SSH，用第一个有响应的服务测 RTT。
// 上一次成功的方式优先复用，避免每秒都从头试一遍。
func (p *Prober) probeGateway(ctx context.Context) (float64, probeStatus) {
	startMode := int(p.gwMode.Load())
	n := len(gwCandidates)
	for i := 0; i < n; i++ {
		mode := (startMode + i) % n
		ms, ok := p.probeGatewayMode(ctx, mode)
		if !ok {
			continue
		}
		p.gwMode.Store(int32(mode))
		p.gwMethod.Store(gwCandidates[mode])
		return ms, probeHit
	}
	// 五种方式全无响应：多半是探针自己排不上队，按超时处理，
	// 真不可达会让 AvgRTT 一直是 0，UI 那边仍然显示「不可达」。
	return 0, probeWait
}

func (p *Prober) probeGatewayMode(ctx context.Context, mode int) (float64, bool) {
	addr := net.JoinHostPort(p.gwAddr, gwPorts[mode])
	if mode == 0 {
		return p.probeUDP53(ctx, addr)
	}
	d := net.Dialer{Timeout: 700 * time.Millisecond}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, false
	}
	_ = conn.Close()
	return float64(time.Since(start).Microseconds()) / 1000, true
}

// probeUDP53 向网关的 53 端口发一次最小 DNS 查询，用回包时间作为 RTT。
func (p *Prober) probeUDP53(ctx context.Context, addr string) (float64, bool) {
	d := net.Dialer{Timeout: 800 * time.Millisecond}
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(900 * time.Millisecond))

	// 随机子域避免命中本地缓存
	label := fmt.Sprintf("fnspeed%d", time.Now().UnixNano()%100000)
	start := time.Now()
	if _, err := conn.Write(buildDNSQuery(label)); err != nil {
		return 0, false
	}
	buf := make([]byte, 512)
	if _, err := conn.Read(buf); err != nil {
		return 0, false
	}
	return float64(time.Since(start).Microseconds()) / 1000, true
}

func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return ""
}

func buildDNSQuery(label string) []byte {
	pkt := []byte{0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	pkt = append(pkt, byte(len(label)))
	pkt = append(pkt, []byte(label)...)
	for _, part := range []string{"fnnas", "local"} {
		pkt = append(pkt, byte(len(part)))
		pkt = append(pkt, []byte(part)...)
	}
	pkt = append(pkt, 0x00)
	pkt = append(pkt, 0x00, 0x01, 0x00, 0x01)
	return pkt
}

// defaultGateway 从 /proc/net/route 解析默认网关（字段为小端十六进制）。
func defaultGateway() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	for _, ln := range lines[1:] {
		f := strings.Fields(ln)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v := f[2]
		if len(v) != 8 {
			continue
		}
		var oct [4]byte
		for i := 0; i < 4; i++ {
			var n uint32
			if _, err := fmt.Sscanf(v[i*2:i*2+2], "%02x", &n); err != nil {
				return ""
			}
			oct[i] = byte(n)
		}
		// 小端：文件里是 01 01 A8 C0 对应 192.168.1.1
		return net.IPv4(oct[3], oct[2], oct[1], oct[0]).String()
	}
	return ""
}

func (p *Prober) Snapshot() PingSnapshot {
	var s PingSnapshot
	if p.target != nil {
		s.Target = p.target.snapshot()
		if v, ok := p.tcpMode.Load().(string); ok {
			s.Target.Method = v
		}
	}
	if p.gateway != nil {
		s.Gateway = p.gateway.snapshot()
		if v, ok := p.gwMethod.Load().(string); ok {
			s.Gateway.Method = v
		}
	}
	return s
}
