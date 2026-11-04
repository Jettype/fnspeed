package main

// 分层网络自检：把「连不上源」拆成 DNS → TCP → TLS → HTTP 四层，
// 每层单独计时、单独报错。用户点一下就知道卡在哪一层，
// 不用再猜是 NAS 的 DNS、旁路由、目标端风控，还是压测程序自己的问题。

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Layer 是单层检测结果。
type Layer struct {
	Layer string  `json:"layer"` // dns / tcp / tls / http
	OK    bool    `json:"ok"`
	Ms    float64 `json:"ms"`
	Info  string  `json:"info,omitempty"`
	Err   string  `json:"err,omitempty"`
}

// SiteTest 是单个 URL 的完整检测链路。
type SiteTest struct {
	URL     string   `json:"url"`
	Host    string   `json:"host"`
	OK      bool     `json:"ok"`
	IPs     []string `json:"ips,omitempty"`
	Layers  []Layer  `json:"layers"`
	Verdict string   `json:"verdict"`
}

// EnvInfo 给前端展示本机网络环境，很多"连不上"其实是 DNS 配错。
type EnvInfo struct {
	ResolvConf string   `json:"resolvConf"` // 实际生效的 nameserver 列表
	DNSServers []string `json:"dnsServers"`
	Gateway    string   `json:"gateway"`
	HTTPProxy  string   `json:"httpProxy"`
	Interface  string   `json:"interface"`
}

// SelfTest 对一个 URL 做四层检测。
func SelfTest(ctx context.Context, raw string) SiteTest {
	st := SiteTest{URL: raw}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		st.Verdict = "URL 格式不对，检查是不是少了 https://"
		return st
	}
	host := u.Hostname()
	st.Host = host

	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	total := 6 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 && d < total {
			total = d
		}
	}

	// ---------- 第 1 层：DNS ----------
	dnsLayer := Layer{Layer: "dns"}
	t0 := time.Now()
	dctx, dcancel := context.WithTimeout(ctx, total)
	ips, derr := net.DefaultResolver.LookupHost(dctx, host)
	dcancel()
	dnsLayer.Ms = msSince(t0)
	if derr != nil {
		dnsLayer.Err = shortErr(derr)
		st.Layers = append(st.Layers, dnsLayer)
		st.Verdict = "域名解析失败 —— NAS 的 DNS 出了问题，不是测速源的问题"
		return st
	}
	dnsLayer.OK = true
	dnsLayer.Info = strings.Join(ips, ", ")
	st.IPs = ips
	st.Layers = append(st.Layers, dnsLayer)

	// ---------- 第 2 层：TCP ----------
	tcpLayer := Layer{Layer: "tcp"}
	target := net.JoinHostPort(host, port)
	d := net.Dialer{Timeout: total}
	t1 := time.Now()
	conn, cerr := d.DialContext(ctx, "tcp", target)
	tcpLayer.Ms = msSince(t1)
	if cerr != nil {
		tcpLayer.Err = shortErr(cerr)
		st.Layers = append(st.Layers, tcpLayer)
		st.Verdict = tcpVerdict(cerr, port)
		return st
	}
	tcpLayer.OK = true
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		tcpLayer.Info = fmt.Sprintf("%s:%d", addr.IP, addr.Port)
	}
	st.Layers = append(st.Layers, tcpLayer)

	// ---------- 第 3 层：TLS ----------
	if u.Scheme == "https" {
		tlsLayer := Layer{Layer: "tls"}
		t2 := time.Now()
		tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		_ = tc.SetDeadline(time.Now().Add(total))
		herr := tc.HandshakeContext(ctx)
		tlsLayer.Ms = msSince(t2)
		if herr != nil {
			tlsLayer.Err = shortErr(herr)
			st.Layers = append(st.Layers, tlsLayer)
			st.Verdict = "TLS 握手失败 —— 多半被中间设备（旁路由/代理）改写了连接，或系统时间不对"
			_ = tc.Close()
			return st
		}
		state := tc.ConnectionState()
		certs := state.PeerCertificates
		info := fmt.Sprintf("TLS %s", tlsName(state.Version))
		if len(certs) > 0 && len(certs[0].DNSNames) > 0 {
			info += " · " + certs[0].DNSNames[0]
		}
		if len(certs) > 0 {
			left := time.Until(certs[0].NotAfter).Hours() / 24
			if left < 30 {
				info += fmt.Sprintf(" · 证书 %d 天后到期", int(left))
			}
			if state.VerifiedChains == nil {
				info += " · 未校验"
			}
		}
		tlsLayer.OK = true
		tlsLayer.Info = info
		st.Layers = append(st.Layers, tlsLayer)
		_ = tc.Close()
	} else {
		_ = conn.Close()
	}

	// ---------- 第 4 层：HTTP ----------
	httpLayer := Layer{Layer: "http"}
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if rerr != nil {
		httpLayer.Err = "构造请求失败"
		st.Layers = append(st.Layers, httpLayer)
		st.Verdict = "URL 无法构造请求"
		return st
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", browserUA)
	t3 := time.Now()
	resp, herr := newTransferClient(nil, "").Do(req)
	httpLayer.Ms = msSince(t3)
	if herr != nil {
		httpLayer.Err = shortErr(herr)
		st.Layers = append(st.Layers, httpLayer)
		switch {
		case strings.Contains(httpLayer.Err, "重定向"):
			st.Verdict = "重定向死循环 —— URL 里的 current / latest 是软链接且镜像站跳回自己，把路径换成带具体版本号的"
		case strings.Contains(httpLayer.Err, "超时"):
			st.Verdict = "DNS/TCP/TLS 都通，但 HTTP 迟迟不响应 —— 目标端在限流或丢大包，换源"
		case strings.Contains(httpLayer.Err, "EOF"):
			st.Verdict = "对端主动断开 —— 多半是风控，换源"
		default:
			st.Verdict = "HTTP 层失败：" + httpLayer.Err
		}
		return st
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	httpLayer.OK = resp.StatusCode < 400
	httpLayer.Info = fmt.Sprintf("HTTP %d", resp.StatusCode)
	if cl := resp.Header.Get("Content-Range"); cl != "" {
		httpLayer.Info += " · " + cl
	} else if resp.ContentLength > 0 {
		httpLayer.Info += fmt.Sprintf(" · %.1f MB", float64(resp.ContentLength)/1e6)
	}
	st.Layers = append(st.Layers, httpLayer)

	st.OK = httpLayer.OK
	switch {
	case resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusOK:
		st.Verdict = "全链路正常"
	case resp.StatusCode == http.StatusForbidden:
		st.Verdict = "目标端拒绝访问（403）—— 风控拦了这个 IP，换源"
	case resp.StatusCode == 418:
		st.Verdict = "目标端风控拦截（418）—— 短时间内请求太多或特征像脚本，等一会或换源"
	case resp.StatusCode == http.StatusTooManyRequests:
		st.Verdict = "目标端限流（429）—— 等几分钟或换源"
	case resp.StatusCode == http.StatusNotFound:
		st.Verdict = "路径不存在（404）—— URL 写错了或者文件被删了"
	case resp.StatusCode >= 500:
		st.Verdict = "目标端服务异常（5xx）—— 不是你的问题，等一会或换源"
	default:
		st.Verdict = fmt.Sprintf("HTTP %d —— 目标端不接受这个请求", resp.StatusCode)
	}
	return st
}

// SelfTestMany 并发检测多个 URL（上限 8 个）。
func SelfTestMany(ctx context.Context, urls []string) []SiteTest {
	if len(urls) > 8 {
		urls = urls[:8]
	}
	out := make([]SiteTest, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out[i] = SelfTest(c, u)
		}(i, u)
	}
	wg.Wait()
	return out
}

// EnvProbe 读取本机的 DNS / 网关 / 代理配置。
func EnvProbe() EnvInfo {
	var e EnvInfo
	raw, _ := os.ReadFile("/etc/resolv.conf")
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "nameserver") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				e.DNSServers = append(e.DNSServers, f[1])
			}
		}
	}
	e.ResolvConf = strings.TrimSpace(string(raw))
	if len(e.ResolvConf) == 0 {
		e.ResolvConf = "(/etc/resolv.conf 读不到)"
	}
	e.Gateway = defaultGateway()
	e.HTTPProxy = strings.TrimSpace(os.Getenv("HTTPS_PROXY"))
	if e.HTTPProxy == "" {
		e.HTTPProxy = strings.TrimSpace(os.Getenv("HTTP_PROXY"))
	}
	if e.HTTPProxy == "" {
		e.HTTPProxy = strings.TrimSpace(os.Getenv("ALL_PROXY"))
	}
	return e
}

// ---------------- 小工具 ----------------

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}

func tlsName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS11:
		return "1.1"
	case tls.VersionTLS10:
		return "1.0"
	}
	return "?"
}

// shortErr 把 Go 的嵌套错误压成一句人话。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "超时"
	}
	switch {
	case strings.Contains(s, "no such host"):
		return "域名不存在或 DNS 无响应"
	case strings.Contains(s, "connection refused"):
		return "连接被拒绝（端口没开）"
	case strings.Contains(s, "network is unreachable"):
		return "网络不可达（本机没有到该地址的路由）"
	case strings.Contains(s, "no route to host"):
		return "无路由（检查网关/旁路由）"
	case strings.Contains(s, "i/o timeout"):
		return "超时（TCP 没有回包，常见于被墙或被防火墙丢包）"
	case strings.Contains(s, "certificate"):
		return "证书校验失败：" + s
	case strings.Contains(s, "EOF"):
		return "连接被对端断开（EOF）"
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func tcpVerdict(err error, port string) string {
	s := shortErr(err)
	switch {
	case strings.Contains(s, "超时"):
		return "TCP 连不上（超时）—— 目标端 443/80 被墙、被防火墙丢包，或旁路由没有把你放行"
	case strings.Contains(s, "拒绝"):
		return "端口拒绝连接 —— 目标端这个端口没开"
	case strings.Contains(s, "不可达"), strings.Contains(s, "无路由"):
		return "本机没有到该地址的路由 —— 检查 NAS 网关是否指向了旁路由"
	}
	return "TCP 握手失败 —— " + s
}

// cleanURLList 去空行、去注释、去重，缺 scheme 时补 https://
func cleanURLList(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if !strings.Contains(s, "://") {
			s = "https://" + s
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
