package main

// 配置格式：与「跑满了吗」v2 互通，本实现取其超集。
// 导入时接受 JSON 的 \/ 转义（Go 的 encoding/json 原生支持）。
// 导出时按两档剥离：标准 v2 会丢掉所有扩展字段。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const (
	ConfigFormat  = "PAOMAN_SPEED_TEST_CONFIG"
	ConfigVersion = 2
)

// Endpoint 是单个可压测端点。
type Endpoint struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// PinRegion 是端点级定向覆盖（如 gd-ct）。留空则跟随全局设置。
	// 只对个别源需要特殊走位时才用得上，界面上不暴露。
	PinRegion string `json:"pinRegion,omitempty"`
}

// Profile 是一个配置集。
// 前 6 个字段属于标准 v2；后面的是本实现的扩展字段，缺省有默认值。
type Profile struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	DownloadURLs      []string   `json:"downloadUrls"`
	UploadURLs        []string   `json:"uploadUrls"`
	DownloadEndpoints []Endpoint `json:"downloadEndpoints"`
	UploadEndpoints   []Endpoint `json:"uploadEndpoints"`

	// ---- 扩展字段（标准 v2 不认，导出时剥离）----
	Group        string `json:"group,omitempty"` // 预设分组：国内/国外/上传
	Desc         string `json:"desc,omitempty"`  // 预设说明
	Concurrency  int    `json:"concurrency,omitempty"`
	DurationSec  int    `json:"durationSec,omitempty"`
	BytesLimit   int64  `json:"bytesLimit,omitempty"`
	Mode         string `json:"mode,omitempty"`
	URLRotate    string `json:"urlRotate,omitempty"`
	SwitchPolicy string `json:"switchPolicy,omitempty"`

	// ForkedFrom 非空表示这条是「默认分组的用户副本」：
	// 用户改了内置分组，系统自动在 user.json 复制一份，
	// 默认区那份照常被下次安装覆盖，互不干扰。
	ForkedFrom string `json:"forkedFrom,omitempty"`
}

// Config 是导入/导出的顶层结构。
type Config struct {
	Format            string    `json:"format"`
	Version           int       `json:"version"`
	SelectedProfileID string    `json:"selectedProfileId"`
	Profiles          []Profile `json:"profiles"`

	// PresetRev 是 1.5.0 及以前用来判断「预设要不要补」的版本号。
	// 1.5.1 起默认区改成按内容 hash 整体覆盖，这个字段不再读也不再写，
	// 仅保留在结构体里，避免导入老备份时报未知字段。
	PresetRev int `json:"presetRev,omitempty"`
}

// ParseConfig 解析并规范化配置。
// 重复项以 endpoints 为准；uploadUrls 指向站点首页时标记为非直传端点。
func ParseConfig(raw []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("配置不是合法 JSON: %w", err)
	}
	if c.Format != ConfigFormat {
		return nil, fmt.Errorf("format 字段应为 %q，实际 %q", ConfigFormat, c.Format)
	}
	if c.Version != ConfigVersion && c.Version != 0 {
		return nil, fmt.Errorf("只支持 version=2，实际 %d", c.Version)
	}
	// 允许 profiles 为空：首次安装初始化出的就是空配置集，
	// 导入合法性由调用方（api.importConfig）单独校验。
	for i := range c.Profiles {
		normalizeProfile(&c.Profiles[i])
	}
	if c.SelectedProfileID == "" && len(c.Profiles) > 0 {
		c.SelectedProfileID = c.Profiles[0].ID
	}
	return &c, nil
}

func normalizeProfile(p *Profile) {
	if p.ID == "" {
		p.ID = "profile-1"
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if p.Concurrency <= 0 {
		p.Concurrency = defaultConcurrency
	}
	if p.Concurrency > maxConcurrency {
		p.Concurrency = maxConcurrency
	}
	if p.DurationSec <= 0 {
		p.DurationSec = 300
	}
	if p.DurationSec > maxDurationSec {
		p.DurationSec = maxDurationSec
	}
	if p.Mode == "" {
		p.Mode = "both"
	}
	if p.URLRotate == "" {
		p.URLRotate = "sticky"
	}
	if p.SwitchPolicy == "" {
		p.SwitchPolicy = "failover"
	}
	p.DownloadEndpoints = mergeEndpoints(p.DownloadURLs, p.DownloadEndpoints)
	p.UploadEndpoints = mergeEndpoints(p.UploadURLs, p.UploadEndpoints)
}

// mergeEndpoints 把纯 URL 列表和 endpoints 列表合并。
// 规则：以 endpoints 为准，只把 URLs 里没出现过的补进来。
func mergeEndpoints(urls []string, eps []Endpoint) []Endpoint {
	seen := make(map[string]struct{}, len(eps))
	for _, e := range eps {
		seen[strings.TrimSpace(e.URL)] = struct{}{}
	}
	out := make([]Endpoint, 0, len(eps)+len(urls))
	out = append(out, eps...)
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, Endpoint{URL: u})
	}
	for i := range out {
		out[i].URL = strings.TrimSpace(out[i].URL)
		if out[i].Method == "" {
			out[i].Method = "GET"
		}
	}
	return out
}

// maxEndpointURLLen 限制单条端点地址的长度。
// 正常 CDN 直链在 200–500 字符，签名串偶尔到 1500；4096 足够，同时挡住
// 「把整个文件当 URL 贴进来」这种输入。
const maxEndpointURLLen = 4096

// checkEndpointURL 校验一条端点地址是否可压测：必须 http/https 且带主机名。
//
// 卡在 scheme 上不是为了拦 SSRF（这是压测工具，用户本来就要指定目标），
// 而是为了让失败发生在请求之前：
//   - 下载侧 http.Client 遇到 file:// 直接抛 "unsupported protocol scheme"，
//     用户看到的是一句英文，还会被当成「源挂了」；
//   - 上传侧更糟，它绕开 http.Client 自己拼 socket，非 https 的 scheme 一律按
//     :80 明文连，ftp://host/path 会变成向 host:80 发 PUT，谁也不知道发生了什么。
func checkEndpointURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("URL 为空")
	}
	if len(raw) > maxEndpointURLLen {
		return fmt.Errorf("URL 超过 %d 字符", maxEndpointURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 无法解析")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "":
		return fmt.Errorf("缺少 http:// 或 https://")
	default:
		return fmt.Errorf("只支持 http/https，实际是 %s://", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("缺少主机名")
	}
	return nil
}

// firstBadEndpoint 返回第一处不合法的端点地址及其原因，全部合法时返回空串。
func firstBadEndpoint(p *Profile) (string, string) {
	if p == nil {
		return "", ""
	}
	scan := func(list []Endpoint) (string, string) {
		for _, ep := range list {
			if err := checkEndpointURL(ep.URL); err != nil {
				return ep.URL, err.Error()
			}
		}
		return "", ""
	}
	if bad, why := scan(p.DownloadEndpoints); bad != "" {
		return bad, why
	}
	return scan(p.UploadEndpoints)
}

// isHomepageURL 判断上传端点是不是站点首页（路径为空或 "/"）。
// 这种地址不可能接受 PUT/POST 数据，预检直接判失败。
func isHomepageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	p := u.EscapedPath()
	return p == "" || p == "/"
}

// signatureParams 是常见的时效签名参数名。
var signatureParams = []string{
	"sign", "signature", "sig", "t", "ts", "timestamp",
	"expires", "expire", "expiry", "e", "token", "auth_key", "x-signature",
}

// isTimeLimitedURL 判断 URL 是否带时效签名参数。
func isTimeLimitedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	q := u.Query()
	for _, k := range signatureParams {
		if q.Get(k) != "" {
			return true
		}
	}
	return false
}

// ExportStandard 导出纯净 v2：剥掉所有扩展字段。
func (c *Config) ExportStandard() ([]byte, error) {
	type stdProfile struct {
		ID                string     `json:"id"`
		Name              string     `json:"name"`
		DownloadURLs      []string   `json:"downloadUrls"`
		UploadURLs        []string   `json:"uploadUrls"`
		DownloadEndpoints []Endpoint `json:"downloadEndpoints"`
		UploadEndpoints   []Endpoint `json:"uploadEndpoints"`
	}
	type stdConfig struct {
		Format            string       `json:"format"`
		Version           int          `json:"version"`
		SelectedProfileID string       `json:"selectedProfileId"`
		Profiles          []stdProfile `json:"profiles"`
	}
	out := stdConfig{
		Format:            ConfigFormat,
		Version:           ConfigVersion,
		SelectedProfileID: c.SelectedProfileID,
	}
	for _, p := range c.Profiles {
		out.Profiles = append(out.Profiles, stdProfile{
			ID: p.ID, Name: p.Name,
			DownloadURLs: p.DownloadURLs, UploadURLs: p.UploadURLs,
			DownloadEndpoints: p.DownloadEndpoints, UploadEndpoints: p.UploadEndpoints,
		})
	}
	return encodeNoEscape(out)
}

// ExportFull 导出带扩展字段的完整配置。
func (c *Config) ExportFull() ([]byte, error) {
	return encodeNoEscape(c)
}

// encodeNoEscape 输出可读 JSON：不把 & < > 转成 \u0026 这类转义，
// 导出的配置要能直接贴回手机 App。
func encodeNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
