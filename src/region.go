package main

// 定向测速：让 CDN 把节点「交出来」。
//
// 原理：CDN 的调度看两样东西 —— 递归 DNS 的出口位置，以及 EDNS Client Subnet
// 里带的客户端网段。我们直接向公共 DNS 发带 ECS 的查询，把 ECS 写成目标省份
// 某运营商的地址，CDN 就会返回「那个省那个运营商」的边缘节点 IP。
// 拿到 IP 后强制拨号过去（TLS 的 SNI 仍是原域名），就实现了跨地区跨运营商压测。
//
// 只做定点点测：一次选一个位置，不做省 × 运营商的矩阵扫描。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RegionNode 是一个可选测速位置。
// ECS 只是「用来让 CDN 判断位置」的地址，本身不会被连接。
type RegionNode struct {
	ID   string `json:"id"`   // gd-ct
	Prov string `json:"prov"` // 广东
	ISP  string `json:"isp"`  // 电信
	ECS  string `json:"ecs"`  // 代表地址（该省该运营商的公网地址）
}

// regionNodes 用公网 IP 归属库逐条核对过省和运营商，只保留「省对 + 运营商对」
// 的条目；宁缺勿错 —— 错一条用户就会读到假数据。
//
// 缺项说明（都因为核不到准确地址，不硬凑）：
//
//	山西电信、吉林电信、贵州联通、江苏移动、宁夏移动。
//
// ECS 只是让 CDN 判断位置的「门牌号」，本身不会被连接，所以地址不必可达。
var regionNodes = []RegionNode{
	{ID: "bj-ct", Prov: "北京", ISP: "电信", ECS: "219.141.136.10"},
	{ID: "bj-cu", Prov: "北京", ISP: "联通", ECS: "202.106.0.20"},
	{ID: "bj-cm", Prov: "北京", ISP: "移动", ECS: "211.136.17.107"},
	{ID: "tj-ct", Prov: "天津", ISP: "电信", ECS: "219.150.32.132"},
	{ID: "tj-cu", Prov: "天津", ISP: "联通", ECS: "202.99.104.68"},
	{ID: "tj-cm", Prov: "天津", ISP: "移动", ECS: "211.137.160.5"},
	{ID: "he-ct", Prov: "河北", ISP: "电信", ECS: "222.222.222.222"},
	{ID: "he-cu", Prov: "河北", ISP: "联通", ECS: "202.99.160.68"},
	{ID: "he-cm", Prov: "河北", ISP: "移动", ECS: "211.143.60.1"},
	{ID: "sx-cu", Prov: "山西", ISP: "联通", ECS: "202.99.192.68"},
	{ID: "sx-cm", Prov: "山西", ISP: "移动", ECS: "211.142.53.100"},
	{ID: "nm-ct", Prov: "内蒙古", ISP: "电信", ECS: "219.148.162.31"},
	{ID: "nm-cu", Prov: "内蒙古", ISP: "联通", ECS: "202.99.224.68"},
	{ID: "nm-cm", Prov: "内蒙古", ISP: "移动", ECS: "211.138.91.1"},
	{ID: "ln-ct", Prov: "辽宁", ISP: "电信", ECS: "219.148.204.66"},
	{ID: "ln-cu", Prov: "辽宁", ISP: "联通", ECS: "202.96.64.68"},
	{ID: "ln-cm", Prov: "辽宁", ISP: "移动", ECS: "211.140.196.1"},
	{ID: "jl-cu", Prov: "吉林", ISP: "联通", ECS: "202.98.0.68"},
	{ID: "jl-cm", Prov: "吉林", ISP: "移动", ECS: "211.141.16.99"},
	{ID: "hl-ct", Prov: "黑龙江", ISP: "电信", ECS: "219.147.198.230"},
	{ID: "hl-cu", Prov: "黑龙江", ISP: "联通", ECS: "202.97.224.69"},
	{ID: "hl-cm", Prov: "黑龙江", ISP: "移动", ECS: "211.137.241.34"},
	{ID: "sh-ct", Prov: "上海", ISP: "电信", ECS: "202.96.209.133"},
	{ID: "sh-cu", Prov: "上海", ISP: "联通", ECS: "210.22.70.3"},
	{ID: "sh-cm", Prov: "上海", ISP: "移动", ECS: "211.136.112.200"},
	{ID: "js-ct", Prov: "江苏", ISP: "电信", ECS: "218.2.135.1"},
	{ID: "js-cu", Prov: "江苏", ISP: "联通", ECS: "221.6.4.66"},
	{ID: "zj-ct", Prov: "浙江", ISP: "电信", ECS: "61.153.177.196"},
	{ID: "zj-cu", Prov: "浙江", ISP: "联通", ECS: "221.12.1.227"},
	{ID: "zj-cm", Prov: "浙江", ISP: "移动", ECS: "211.140.13.188"},
	{ID: "ah-ct", Prov: "安徽", ISP: "电信", ECS: "218.22.21.2"},
	{ID: "ah-cu", Prov: "安徽", ISP: "联通", ECS: "218.104.78.2"},
	{ID: "ah-cm", Prov: "安徽", ISP: "移动", ECS: "211.138.180.2"},
	{ID: "fj-ct", Prov: "福建", ISP: "电信", ECS: "218.85.152.99"},
	{ID: "fj-cu", Prov: "福建", ISP: "联通", ECS: "218.104.128.106"},
	{ID: "fj-cm", Prov: "福建", ISP: "移动", ECS: "211.138.151.161"},
	{ID: "jx-ct", Prov: "江西", ISP: "电信", ECS: "202.101.224.69"},
	{ID: "jx-cu", Prov: "江西", ISP: "联通", ECS: "220.248.192.12"},
	{ID: "jx-cm", Prov: "江西", ISP: "移动", ECS: "211.141.90.68"},
	{ID: "sd-ct", Prov: "山东", ISP: "电信", ECS: "219.146.0.130"},
	{ID: "sd-cu", Prov: "山东", ISP: "联通", ECS: "202.102.128.68"},
	{ID: "sd-cm", Prov: "山东", ISP: "移动", ECS: "218.201.96.130"},
	{ID: "ha-ct", Prov: "河南", ISP: "电信", ECS: "222.85.85.85"},
	{ID: "ha-cu", Prov: "河南", ISP: "联通", ECS: "202.102.224.68"},
	{ID: "ha-cm", Prov: "河南", ISP: "移动", ECS: "211.142.102.5"},
	{ID: "hb-ct", Prov: "湖北", ISP: "电信", ECS: "202.103.24.68"},
	{ID: "hb-cu", Prov: "湖北", ISP: "联通", ECS: "218.104.111.114"},
	{ID: "hb-cm", Prov: "湖北", ISP: "移动", ECS: "211.137.58.20"},
	{ID: "hn-ct", Prov: "湖南", ISP: "电信", ECS: "222.246.129.80"},
	{ID: "hn-cu", Prov: "湖南", ISP: "联通", ECS: "58.20.127.170"},
	{ID: "hn-cm", Prov: "湖南", ISP: "移动", ECS: "211.142.210.98"},
	{ID: "gd-ct", Prov: "广东", ISP: "电信", ECS: "202.96.128.86"},
	{ID: "gd-cu", Prov: "广东", ISP: "联通", ECS: "210.21.196.6"},
	{ID: "gd-cm", Prov: "广东", ISP: "移动", ECS: "211.136.192.6"},
	{ID: "gx-ct", Prov: "广西", ISP: "电信", ECS: "202.103.224.68"},
	{ID: "gx-cu", Prov: "广西", ISP: "联通", ECS: "221.7.128.68"},
	{ID: "gx-cm", Prov: "广西", ISP: "移动", ECS: "211.138.245.180"},
	{ID: "hi-ct", Prov: "海南", ISP: "电信", ECS: "202.100.192.68"},
	{ID: "hi-cu", Prov: "海南", ISP: "联通", ECS: "221.11.132.2"},
	{ID: "hi-cm", Prov: "海南", ISP: "移动", ECS: "211.138.164.3"},
	{ID: "cq-ct", Prov: "重庆", ISP: "电信", ECS: "61.128.128.68"},
	{ID: "cq-cu", Prov: "重庆", ISP: "联通", ECS: "221.5.203.98"},
	{ID: "cq-cm", Prov: "重庆", ISP: "移动", ECS: "218.201.4.3"},
	{ID: "sc-ct", Prov: "四川", ISP: "电信", ECS: "61.139.2.69"},
	{ID: "sc-cu", Prov: "四川", ISP: "联通", ECS: "119.6.6.6"},
	{ID: "sc-cm", Prov: "四川", ISP: "移动", ECS: "211.137.96.205"},
	{ID: "gz-ct", Prov: "贵州", ISP: "电信", ECS: "202.98.192.67"},
	{ID: "gz-cm", Prov: "贵州", ISP: "移动", ECS: "211.139.5.29"},
	{ID: "yn-ct", Prov: "云南", ISP: "电信", ECS: "222.172.200.68"},
	{ID: "yn-cu", Prov: "云南", ISP: "联通", ECS: "221.3.131.11"},
	{ID: "yn-cm", Prov: "云南", ISP: "移动", ECS: "211.139.29.68"},
	{ID: "xz-ct", Prov: "西藏", ISP: "电信", ECS: "202.98.224.68"},
	{ID: "xz-cu", Prov: "西藏", ISP: "联通", ECS: "221.13.65.34"},
	{ID: "xz-cm", Prov: "西藏", ISP: "移动", ECS: "211.139.73.34"},
	{ID: "sn-ct", Prov: "陕西", ISP: "电信", ECS: "218.30.19.40"},
	{ID: "sn-cu", Prov: "陕西", ISP: "联通", ECS: "221.11.1.67"},
	{ID: "sn-cm", Prov: "陕西", ISP: "移动", ECS: "211.137.130.3"},
	{ID: "gs-ct", Prov: "甘肃", ISP: "电信", ECS: "202.100.64.68"},
	{ID: "gs-cu", Prov: "甘肃", ISP: "联通", ECS: "221.7.34.10"},
	{ID: "gs-cm", Prov: "甘肃", ISP: "移动", ECS: "218.203.160.194"},
	{ID: "qh-ct", Prov: "青海", ISP: "电信", ECS: "202.100.128.68"},
	{ID: "qh-cu", Prov: "青海", ISP: "联通", ECS: "221.207.58.58"},
	{ID: "qh-cm", Prov: "青海", ISP: "移动", ECS: "211.138.75.123"},
	{ID: "nx-ct", Prov: "宁夏", ISP: "电信", ECS: "222.75.152.129"},
	{ID: "nx-cu", Prov: "宁夏", ISP: "联通", ECS: "221.199.12.157"},
	{ID: "xj-ct", Prov: "新疆", ISP: "电信", ECS: "61.128.114.167"},
	{ID: "xj-cu", Prov: "新疆", ISP: "联通", ECS: "221.7.1.20"},
	{ID: "xj-cm", Prov: "新疆", ISP: "移动", ECS: "211.138.34.2"},
}

var regionIndex = func() map[string]RegionNode {
	m := make(map[string]RegionNode, len(regionNodes))
	for _, n := range regionNodes {
		m[n.ID] = n
	}
	return m
}()

// RegionLabel 返回「广东 · 电信」，未知返回空串。
func RegionLabel(id string) string {
	if id == "" || id == "auto" {
		return ""
	}
	if n, ok := regionIndex[id]; ok {
		return n.Prov + " · " + n.ISP
	}
	return ""
}

// RegionOptions 给前端渲染下拉框：按省分组，省内固定 电信 / 联通 / 移动 顺序。
func RegionOptions() []map[string]any {
	byProv := map[string][]RegionNode{}
	order := []string{}
	for _, n := range regionNodes {
		if _, ok := byProv[n.Prov]; !ok {
			order = append(order, n.Prov)
		}
		byProv[n.Prov] = append(byProv[n.Prov], n)
	}
	out := make([]map[string]any, 0, len(order))
	for _, p := range order {
		items := make([]map[string]string, 0, len(byProv[p]))
		for _, n := range byProv[p] {
			items = append(items, map[string]string{"id": n.ID, "isp": n.ISP})
		}
		out = append(out, map[string]any{"prov": p, "items": items})
	}
	return out
}

// ---------------- 解析 ----------------

const (
	dohTTL      = 10 * time.Minute
	dohTimeout  = 5 * time.Second
	dohEndpoint = "https://223.5.5.5/resolve" // 阿里公共 DNS 的 DoH JSON 接口
	dohBackup   = "https://223.6.6.6/resolve"
)

type dohAnswer struct {
	Status int `json:"Status"`
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"Answer"`
}

type pinEntry struct {
	ip string
	at time.Time
}

type pinResolver struct {
	mu    sync.Mutex
	cache map[string]pinEntry
	cli   *http.Client
}

func NewPinResolver() *pinResolver {
	return &pinResolver{
		cache: make(map[string]pinEntry),
		cli:   &http.Client{Timeout: dohTimeout},
	}
}

// Resolve 用带 ECS 的查询拿到 host 在目标位置的节点 IP。
// 结果缓存 10 分钟 —— CDN 调度不会秒级变化，重复解析纯属浪费。
func (r *pinResolver) Resolve(ctx context.Context, host, regionID string) (string, error) {
	n, ok := regionIndex[regionID]
	if !ok || n.ECS == "" {
		return "", fmt.Errorf("未知测速位置 %q", regionID)
	}
	key := host + "|" + regionID
	r.mu.Lock()
	if e, ok := r.cache[key]; ok && time.Since(e.at) < dohTTL {
		r.mu.Unlock()
		return e.ip, nil
	}
	r.mu.Unlock()

	ip, err := r.query(ctx, host, n.ECS)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.cache[key] = pinEntry{ip: ip, at: time.Now()}
	r.mu.Unlock()
	return ip, nil
}

func (r *pinResolver) query(ctx context.Context, host, ecs string) (string, error) {
	var lastErr error
	for _, base := range []string{dohEndpoint, dohBackup} {
		ip, err := r.queryOne(ctx, base, host, ecs)
		if err == nil {
			return ip, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("解析失败")
	}
	return "", lastErr
}

func (r *pinResolver) queryOne(ctx context.Context, base, host, ecs string) (string, error) {
	// 最多跟三跳 CNAME：有的 CDN 只返回 CNAME 不返回 A。
	name := host
	for hop := 0; hop < 3; hop++ {
		u := fmt.Sprintf("%s?name=%s&type=A&edns_client_subnet=%s", base, name, ecs)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "application/dns-json")
		resp, err := r.cli.Do(req)
		if err != nil {
			return "", err
		}
		var d dohAnswer
		dec := json.NewDecoder(resp.Body)
		decErr := dec.Decode(&d)
		resp.Body.Close()
		if decErr != nil {
			return "", decErr
		}
		if d.Status != 0 {
			return "", fmt.Errorf("DNS 返回状态 %d", d.Status)
		}
		var cname string
		for _, a := range d.Answer {
			if a.Type == 1 && net.ParseIP(a.Data) != nil {
				return a.Data, nil
			}
			if a.Type == 5 && cname == "" {
				cname = strings.TrimSuffix(a.Data, ".")
			}
		}
		if cname == "" || cname == name {
			return "", errors.New("没有 A 记录")
		}
		name = cname
	}
	return "", errors.New("CNAME 层数过多")
}

// ---------------- 拨号覆盖 ----------------

type pinCtxKey struct{}

// pinTarget 把「域名 → 目标 IP」绑在一起存。
// 只存 IP 会出事：CDN 302 到另一个域名时，拿旧 IP 去连新域名必然握手失败。
type pinTarget struct {
	host string
	ip   string
}

// withPinIP 把目标 IP 挂到请求上下文，Transport 的 DialContext 会读它。
// 这样不用为每条流单独造 Transport，连接池逻辑也不受影响。
func withPinIP(ctx context.Context, host, ip string) context.Context {
	if ip == "" || host == "" {
		return ctx
	}
	return context.WithValue(ctx, pinCtxKey{}, &pinTarget{host: host, ip: ip})
}

func pinFrom(ctx context.Context) *pinTarget {
	if p, ok := ctx.Value(pinCtxKey{}).(*pinTarget); ok {
		return p
	}
	return nil
}
