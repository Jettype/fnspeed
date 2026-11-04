package main

// 出口 IP / 命中 IP 探测。
//
// 两个概念要分开：
//   出口 IP —— NAS 出网时对端看到的「你」，用来判断走没走旁路由、哪家运营商出口
//   命中 IP —— 测速目标实际连到了哪台机器，用来判断 CDN 调度到哪个节点
//
// 硬性约定（用户红线）：这里拿到的任何 IP 只在内存里短暂存在，
// 不写日志、不写 history.jsonl、不进 user.json、不进 crash.log。
// 因此本文件里不允许出现任何把 IP 输出到日志的代码。

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const ipProbeTimeout = 5 * time.Second

// ExitIP 是一次出口 IP 探测的结果。
type ExitIP struct {
	Source  string  `json:"source"`
	IP      string  `json:"ip,omitempty"`
	Country string  `json:"country,omitempty"`
	Region  string  `json:"region,omitempty"`
	City    string  `json:"city,omitempty"`
	ISP     string  `json:"isp,omitempty"`
	ASN     string  `json:"asn,omitempty"`
	Ms      float64 `json:"ms"`
	Err     string  `json:"err,omitempty"`
}

// HitInfo 描述「这次实际连到了哪台机器」。
type HitInfo struct {
	URL      string   `json:"url"`
	Host     string   `json:"host"`
	DNSIPs   []string `json:"dnsIps,omitempty"`
	RemoteIP string   `json:"remoteIp,omitempty"`
	Port     int      `json:"port,omitempty"`
	CDN      string   `json:"cdn,omitempty"`
	Colo     string   `json:"colo,omitempty"`
	Geo      string   `json:"geo,omitempty"`
	ASN      string   `json:"asn,omitempty"`
	Status   int      `json:"status,omitempty"`
	Ms       float64  `json:"ms,omitempty"`
	Err      string   `json:"err,omitempty"`
}

// IPReport 是 /api/ipinfo 的响应体。
type IPReport struct {
	Exit      []ExitIP `json:"exit"`
	Multi     bool     `json:"multi"` // 多个源返回了不同 IP，说明存在多出口/分流
	Hit       *HitInfo `json:"hit,omitempty"`
	ElapsedMs float64  `json:"elapsedMs"`
	Hidden    bool     `json:"hidden,omitempty"` // 前端关了「显示 IP」，服务端直接不给数据
}

// ---------------------------------------------------------------------------
// 出口 IP：多源并行，谁先回先显示，其余陆续补齐
// ---------------------------------------------------------------------------

type ipParser func([]byte) (ExitIP, error)

type ipSource struct {
	name  string
	url   string
	parse ipParser
}

type ipAPIResp struct {
	Status     string `json:"status"`
	Message    string `json:"message"`
	Country    string `json:"country"`
	RegionName string `json:"regionName"`
	City       string `json:"city"`
	ISP        string `json:"isp"`
	AS         string `json:"as"`
	Query      string `json:"query"`
}

type ipSBResp struct {
	IP           string `json:"ip"`
	Country      string `json:"country"`
	Region       string `json:"region"`
	City         string `json:"city"`
	ISP          string `json:"isp"`
	Organization string `json:"organization"`
	ASN          int    `json:"asn"`
}

type ipinfoResp struct {
	IP      string `json:"ip"`
	City    string `json:"city"`
	Region  string `json:"region"`
	Country string `json:"country"`
	Org     string `json:"org"`
}

func parseIPAPI(b []byte) (ExitIP, error) {
	var r ipAPIResp
	if err := json.Unmarshal(b, &r); err != nil {
		return ExitIP{}, err
	}
	if r.Status != "success" || r.Query == "" {
		msg := r.Message
		if msg == "" {
			msg = "无结果"
		}
		return ExitIP{}, fmt.Errorf("%s", msg)
	}
	asn := r.AS
	if i := strings.IndexByte(asn, ' '); i > 0 {
		asn = asn[:i]
	}
	return ExitIP{
		IP: r.Query, Country: zhCountry(r.Country), Region: r.RegionName,
		City: r.City, ISP: r.ISP, ASN: asn,
	}, nil
}

func parseIPSB(b []byte) (ExitIP, error) {
	var r ipSBResp
	if err := json.Unmarshal(b, &r); err != nil {
		return ExitIP{}, err
	}
	if r.IP == "" {
		return ExitIP{}, fmt.Errorf("无结果")
	}
	asn := ""
	if r.ASN > 0 {
		asn = fmt.Sprintf("AS%d", r.ASN)
	}
	isp := r.ISP
	if isp == "" {
		isp = r.Organization
	}
	return ExitIP{
		IP: r.IP, Country: zhCountry(r.Country), Region: r.Region,
		City: r.City, ISP: isp, ASN: asn,
	}, nil
}

func parseIPInfo(b []byte) (ExitIP, error) {
	var r ipinfoResp
	if err := json.Unmarshal(b, &r); err != nil {
		return ExitIP{}, err
	}
	if r.IP == "" {
		return ExitIP{}, fmt.Errorf("无结果")
	}
	asn, isp := "", r.Org
	if strings.HasPrefix(r.Org, "AS") {
		if i := strings.IndexByte(r.Org, ' '); i > 0 {
			asn, isp = r.Org[:i], strings.TrimSpace(r.Org[i+1:])
		}
	}
	return ExitIP{
		IP: r.IP, Country: zhCountry(r.Country), Region: r.Region,
		City: r.City, ISP: isp, ASN: asn,
	}, nil
}

var ipipRe = regexp.MustCompile(`当前\s*IP[：:]\s*([0-9a-fA-F.:]+)[\s\S]*?来自于[：:]\s*([^\r\n]+)`)

func parseIPIP(b []byte) (ExitIP, error) {
	m := ipipRe.FindSubmatch(b)
	if len(m) < 3 {
		return ExitIP{}, fmt.Errorf("返回格式变了")
	}
	fields := strings.Fields(string(m[2]))
	out := ExitIP{IP: strings.TrimSpace(string(m[1]))}
	if len(fields) > 0 {
		out.Country = zhCountry(fields[0])
	}
	if len(fields) > 1 {
		out.Region = fields[1]
	}
	if len(fields) > 2 {
		out.City = fields[2]
	}
	if len(fields) > 3 {
		out.ISP = strings.Join(fields[3:], " ")
	}
	return out, nil
}

func zhCountry(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "china", "cn", "中国", "中华人民共和国", "中華人民共和國", "china mainland":
		return "中国"
	case "hong kong", "hk", "中国香港", "中國香港":
		return "中国香港"
	case "taiwan", "tw", "中国台湾", "中國台灣":
		return "中国台湾"
	case "macao", "macau", "mo", "中国澳门", "中國澳門":
		return "中国澳门"
	case "japan", "jp":
		return "日本"
	case "singapore", "sg":
		return "新加坡"
	case "united states", "us", "美国":
		return "美国"
	case "germany", "de":
		return "德国"
	case "france", "fr":
		return "法国"
	case "":
		return ""
	}
	return s
}

var exitSources = []ipSource{
	{
		name:  "ip-api",
		url:   "http://ip-api.com/json/?fields=status,message,country,regionName,city,isp,as,query&lang=zh-CN",
		parse: parseIPAPI,
	},
	{
		name:  "ip.sb",
		url:   "https://api.ip.sb/geoip",
		parse: parseIPSB,
	},
	{
		name:  "ipip.net",
		url:   "https://myip.ipip.net",
		parse: parseIPIP,
	},
	{
		name:  "ipinfo.io",
		url:   "https://ipinfo.io/json",
		parse: parseIPInfo,
	},
}

func ipClient() *http.Client {
	return &http.Client{
		Timeout: ipProbeTimeout,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			ForceAttemptHTTP2:   true,
			DisableCompression:  false,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// ProbeExitIP 并行问多个 echo 源，返回全部结果（含失败的，前端可按需展示）。
func ProbeExitIP(ctx context.Context) ([]ExitIP, bool) {
	c := ipClient()
	out := make([]ExitIP, len(exitSources))
	var wg sync.WaitGroup
	for i, src := range exitSources {
		wg.Add(1)
		go func(i int, src ipSource) {
			defer guard("ipExit")
			defer wg.Done()
			out[i] = probeOneExit(ctx, c, src)
		}(i, src)
	}
	wg.Wait()

	// 判断是否多出口：把成功返回的 IP 去重计数
	seen := map[string]bool{}
	for _, e := range out {
		if e.IP != "" {
			seen[e.IP] = true
		}
	}
	return out, len(seen) > 1
}

func probeOneExit(ctx context.Context, c *http.Client, src ipSource) ExitIP {
	r := ExitIP{Source: src.name}
	t0 := time.Now()
	cctx, cancel := context.WithTimeout(ctx, ipProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, src.url, nil)
	if err != nil {
		r.Err = "请求构造失败"
		return r
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := c.Do(req)
	r.Ms = float64(time.Since(t0).Microseconds()) / 1000
	if err != nil {
		r.Err = shortErr(err)
		return r
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		r.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return r
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		r.Err = "读取响应失败"
		return r
	}
	got, err := src.parse(body)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	got.Source = src.name
	got.Ms = r.Ms
	return got
}

// ---------------------------------------------------------------------------
// 命中 IP：DNS 解析 + 实际连接地址 + 响应头里的 CDN 线索
// ---------------------------------------------------------------------------

// ProbeHit 对单个 URL 做一次轻量请求，采回实际命中的 IP 与 CDN 信息。
// 用 Range 0-0 只要 1 字节，够拿响应头又不产生流量。
func ProbeHit(ctx context.Context, raw string) HitInfo {
	h := HitInfo{URL: raw}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		h.Err = "URL 格式不对"
		return h
	}
	h.Host = u.Hostname()

	var remote string
	var dnsOnce sync.Once
	t0 := time.Now()

	trace := &httptrace.ClientTrace{
		DNSDone: func(info httptrace.DNSDoneInfo) {
			dnsOnce.Do(func() {
				for _, a := range info.Addrs {
					ip := a.IP.String()
					if !containsStr(h.DNSIPs, ip) {
						h.DNSIPs = append(h.DNSIPs, ip)
					}
				}
			})
		},
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn == nil {
				return
			}
			if ta, ok := info.Conn.RemoteAddr().(*net.TCPAddr); ok {
				remote = ta.IP.String()
				h.Port = ta.Port
			} else {
				remote = info.Conn.RemoteAddr().String()
			}
		},
	}

	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(cctx, trace), http.MethodGet, raw, nil)
	if err != nil {
		h.Err = "请求构造失败"
		return h
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")

	client := newTransferClient(nil, "")
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	h.Ms = float64(time.Since(t0).Microseconds()) / 1000
	h.RemoteIP = remote
	if err != nil {
		h.Err = shortErr(err)
		if remote != "" {
			// 连上了但 HTTP 层失败，CDN 信息拿不到，IP 还是有效的
			h.Err += "（已建连，但没拿到响应头）"
		}
		return h
	}
	defer func() {
		_, _ = io.CopyN(io.Discard, resp.Body, 1024)
		_ = resp.Body.Close()
	}()
	h.Status = resp.StatusCode
	h.CDN, h.Colo = cdnOf(h.Host, resp.Header)
	return h
}

// cdnOf 从响应头和域名后缀猜 CDN 厂商，猜不出就留空（不硬编一个假的）。
func cdnOf(host string, hdr http.Header) (cdn, colo string) {
	if ray := hdr.Get("Cf-Ray"); ray != "" {
		if i := strings.LastIndexByte(ray, '-'); i > 0 && i < len(ray)-1 {
			colo = strings.ToUpper(ray[i+1:])
		}
		return "Cloudflare", colo
	}
	if hdr.Get("X-Amz-Cf-Id") != "" {
		return "AWS CloudFront", ""
	}

	server := strings.ToLower(hdr.Get("Server"))
	via := strings.ToLower(hdr.Get("Via"))
	xcache := strings.ToLower(hdr.Get("X-Cache"))

	switch {
	case strings.Contains(server, "cloudflare"):
		return "Cloudflare", ""
	case strings.Contains(server, "awselb"), strings.Contains(server, "amazons3"):
		return "AWS", ""
	case strings.Contains(server, "cloudfront"), strings.Contains(xcache, "cloudfront"):
		return "AWS CloudFront", ""
	case strings.Contains(server, "tengine"), strings.Contains(via, "tengine"):
		return "阿里云 CDN", ""
	case hdr.Get("X-Obs-Request-Id") != "":
		return "华为云 OBS", ""
	case strings.Contains(server, "elb"):
		return "华为云 ELB", ""
	case strings.Contains(server, "gws"), strings.Contains(server, "gfe"):
		return "Google", ""
	}

	lhost := strings.ToLower(host)
	hostRules := []struct{ suffix, name string }{
		{"vultr.com", "Vultr"},
		{"linode.com", "Linode"},
		{"hetzner.com", "Hetzner"},
		{".ovh.net", "OVH"},
		{".ovh.us", "OVH"},
		{"datapacket.com", "CDN77"},
		{"cloudfront.net", "AWS CloudFront"},
		{"myapp.com", "腾讯云 CDN"},
		{"alicdn.com", "阿里云 CDN"},
		{"aliyun.com", "阿里云"},
		{"xuexi.cn", "阿里云 CDN"},
		{"huaweicloud.com", "华为云"},
		{"cloudflare.com", "Cloudflare"},
		{"cloudflare.net", "Cloudflare"},
		{"tuna.tsinghua.edu.cn", "清华 TUNA"},
		{"ustc.edu.cn", "中科大镜像"},
		{"cachefly.net", "Cachefly"},
	}
	for _, r := range hostRules {
		if strings.Contains(lhost, r.suffix) {
			return r.name, ""
		}
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// 归属地：一次查询，失败不影响主流程
// ---------------------------------------------------------------------------

func LookupGeo(ctx context.Context, ip string) (geo, asn string) {
	if net.ParseIP(ip) == nil {
		return "", ""
	}
	c := ipClient()
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	u := "http://ip-api.com/json/" + url.PathEscape(ip) +
		"?fields=status,country,regionName,city,isp,as&lang=zh-CN"
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("User-Agent", browserUA)

	resp, err := c.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", ""
	}
	var r ipAPIResp
	if json.Unmarshal(body, &r) != nil || r.Status != "success" {
		return "", ""
	}
	parts := make([]string, 0, 3)
	if c := zhCountry(r.Country); c != "" {
		parts = append(parts, c)
	}
	if r.RegionName != "" {
		parts = append(parts, r.RegionName)
	}
	if r.City != "" && r.City != r.RegionName {
		parts = append(parts, r.City)
	}
	if r.ISP != "" {
		parts = append(parts, r.ISP)
	}
	if r.AS != "" {
		if i := strings.IndexByte(r.AS, ' '); i > 0 {
			asn = r.AS[:i]
		} else {
			asn = r.AS
		}
	}
	return strings.Join(parts, " · "), asn
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// HTTP 接口
// ---------------------------------------------------------------------------

// handleIPInfo 返回出口 IP + 当前选中源的命中 IP。
// 关掉「显示 IP」开关时，服务端一个外部请求都不发，直接回 hidden ——
// 这样连出口商都看不到探测流量，截图分享也干净。
func (a *App) handleIPInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET/POST")
		return
	}

	var req struct {
		URL  string `json:"url"`
		Show *bool  `json:"show"`
	}
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		_ = json.Unmarshal(body, &req)
	} else {
		req.URL = r.URL.Query().Get("url")
	}

	show := a.settings.Get().ShowIP
	if req.Show != nil {
		show = *req.Show
	}
	if !show {
		writeJSON(w, http.StatusOK, IPReport{Hidden: true})
		return
	}

	t0 := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	var (
		exit  []ExitIP
		multi bool
		hit   HitInfo
		wg    sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer guard("ipExit")
		defer wg.Done()
		exit, multi = ProbeExitIP(ctx)
	}()
	go func() {
		defer guard("ipHit")
		defer wg.Done()
		if u := strings.TrimSpace(req.URL); u != "" {
			hit = ProbeHit(ctx, u)
		}
	}()
	wg.Wait()

	rep := IPReport{Exit: exit, Multi: multi}
	if hit.Host != "" {
		if hit.RemoteIP != "" {
			geo, asn := geoFromExit(exit, hit.RemoteIP)
			if geo == "" {
				// 命中 IP 与出口不同：单独查一次归属地，失败就留空
				geo, asn = LookupGeo(ctx, hit.RemoteIP)
			}
			hit.Geo, hit.ASN = geo, asn
		}
		rep.Hit = &hit
	}
	rep.ElapsedMs = float64(time.Since(t0).Microseconds()) / 1000
	writeJSON(w, http.StatusOK, rep)
}

func geoFromExit(list []ExitIP, ip string) (string, string) {
	for _, e := range list {
		if e.IP != ip {
			continue
		}
		parts := make([]string, 0, 4)
		for _, s := range []string{e.Country, e.Region, e.City, e.ISP} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " · "), e.ASN
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// 批量命中探测：只在「显示命中」开关打开时调用
// ---------------------------------------------------------------------------

const (
	hitProbeConcurrency = 4                // 并发太高会被目标站风控
	hitProbeTimeout     = 5 * time.Second  // 单条上限，比 ProbeHit 内部的 8s 更紧
	hitCacheTTL         = 10 * time.Minute // 反复开关界面不重复外发
	hitCacheMax         = 256
)

type hitCacheEntry struct {
	info    HitInfo
	fetched time.Time
}

var (
	hitCacheMu sync.Mutex
	hitCache   = make(map[string]hitCacheEntry)
)

func hitCacheGet(url string) (HitInfo, bool) {
	hitCacheMu.Lock()
	defer hitCacheMu.Unlock()
	e, ok := hitCache[url]
	if !ok {
		return HitInfo{}, false
	}
	if time.Since(e.fetched) > hitCacheTTL {
		delete(hitCache, url)
		return HitInfo{}, false
	}
	return e.info, true
}

func hitCachePut(url string, info HitInfo) {
	hitCacheMu.Lock()
	defer hitCacheMu.Unlock()
	if len(hitCache) >= hitCacheMax {
		hitCache = make(map[string]hitCacheEntry, hitCacheMax)
	}
	hitCache[url] = hitCacheEntry{info: info, fetched: time.Now()}
}

// ProbeHits 对多条端点并发探测命中 IP，返回顺序与入参一致。
// 已在缓存里的直接复用，不发请求；缓存过期的重新探。
func ProbeHits(ctx context.Context, urls []string, conc int) []HitInfo {
	if conc <= 0 {
		conc = hitProbeConcurrency
	}
	// 去重但保留首次出现的顺序
	seen := make(map[string]bool, len(urls))
	uniq := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		uniq = append(uniq, u)
	}

	out := make([]HitInfo, len(uniq))
	todo := make([]int, 0, len(uniq))
	for i, u := range uniq {
		if info, ok := hitCacheGet(u); ok {
			out[i] = info
			continue
		}
		todo = append(todo, i)
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer guard("probeHits.worker")
			for i := range jobs {
				cctx, cancel := context.WithTimeout(ctx, hitProbeTimeout)
				info := ProbeHit(cctx, uniq[i])
				cancel()
				out[i] = info
				if info.Err == "" || info.RemoteIP != "" {
					hitCachePut(uniq[i], info)
				}
			}
		}()
	}
	for _, i := range todo {
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	return out
}

// handleHits 是 /api/hits：批量查条目命中 IP。
// 「显示 IP」总闸关掉时一个外部请求都不发。
func (a *App) handleHits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	if !a.settings.Get().ShowIP {
		writeJSON(w, http.StatusOK, map[string]any{"hidden": true})
		return
	}
	var req struct {
		URLs []string `json:"urls"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if len(req.URLs) == 0 {
		writeErr(w, http.StatusBadRequest, "缺少 urls")
		return
	}
	if len(req.URLs) > 64 {
		req.URLs = req.URLs[:64]
	}

	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	t0 := time.Now()

	hits := ProbeHits(ctx, req.URLs, hitProbeConcurrency)
	for i := range hits {
		if hits[i].RemoteIP == "" {
			continue
		}
		geo, asn := LookupGeo(ctx, hits[i].RemoteIP)
		hits[i].Geo, hits[i].ASN = geo, asn
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hits":      hits,
		"elapsedMs": float64(time.Since(t0).Microseconds()) / 1000,
	})
}
