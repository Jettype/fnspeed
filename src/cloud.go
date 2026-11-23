package main

// 云配置：测速源清单从远端服务拉取，取代编译期硬编码。
//
// 1.6.0 起，默认区（界面里的「国内 / 国外 / 上传」三组预设）不再由
// presets.go 决定，而是由远端下发。presets.go 那份退化成「离线兜底」。
//
// 三级数据源，按优先级：
//
//	live    本次进程内从远端拉取成功
//	cache   etc/cloud.json，上次成功拉取的快照（带 ETag，可用条件请求续期）
//	builtin 编译期出厂清单 DefaultPresets()
//
// 启动时先同步读缓存（有就用，界面立刻有源），远端刷新在后台跑；
// 成功后热切换默认区，不重启、不重新打包。
//
// 远端不可用**不是错误状态**：压测工具在断网环境里也得能开工，
// 所以降级链每一级都可用，只是界面会明确标注当前用的是哪一级。
//
// 安全边界（云端清单是外部输入，等同于可被中间人篡改的配置）：
//   - 端点必须 https（内网调试可显式放行 http）
//   - 响应体上限 1 MiB、单次总时限 12s、重定向最多 3 跳
//   - 每条端点地址必须过 checkEndpointURL，否则整份清单拒绝采用
//   - 可选 HMAC-SHA256 签名：配了密钥就校验，不匹配整份拒绝
//   - 任何一项校验失败都只影响「这次更新」，绝不覆盖已有的好缓存

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// appVersion 由 build.sh 用 -ldflags "-X main.appVersion=$VER" 注入，
// 保证二进制自检的版本号与 manifest 永远一致。
var appVersion = "1.6.1"

const (
	// cloudFormat 是远端清单的 format 标识，写错一个字整份拒绝。
	cloudFormat  = "FNS_CLOUD_SOURCES"
	cloudVersion = 1

	cloudFileName = "cloud.json"
	// cloudSecretFileName 是可选的签名密钥文件（etc/cloud-secret）。
	// 放文件而不是命令行：flag 会出现在 ps 输出里，等同公开。
	cloudSecretFileName = "cloud-secret"

	// 响应体积上限：15 个分组的清单实测 6–8 KB，1 MiB 已经宽松到能放上千条。
	cloudMaxBody = 1 << 20
	// 分组数量上限，与单次任务的端点数上限对齐。
	cloudMaxProfiles = 64

	cloudDefaultTTL = 6 * time.Hour
	cloudMinTTL     = 5 * time.Minute
	cloudMaxTTL     = 24 * time.Hour

	// 拉取失败后的退避区间：1 分钟起翻倍，封顶 30 分钟。
	cloudBackoffMin = 1 * time.Minute
	cloudBackoffMax = 30 * time.Minute

	// cloudFetchTimeout 是单次拉取的总时限（含 DNS / TLS / 读完 body）。
	cloudFetchTimeout = 12 * time.Second

	// 数据源状态。
	cloudStateLive    = "live"
	cloudStateCache   = "cache"
	cloudStateBuiltin = "builtin"
	cloudStateOff     = "off"
)

// defaultCloudEndpoint 是编译期兜底的清单地址，指向本仓库 main 分支上的
// cloud/sources.json。改源只需更新那个文件，客户端下次刷新即生效，不必重新打包。
//
// 用 main 而不是钉死某个 commit：清单是「活配置」，跟随最新才是预期行为。
// raw.githubusercontent.com 在部分网络环境不可达，此时会依次退到 etc/cloud.json
// 快照与出厂清单，压测功能不受影响；也可以在界面「云端源」里换成自建地址。
const defaultCloudEndpoint = "https://raw.githubusercontent.com/Jettype/fnspeed/main/cloud/sources.json"

// cloudDoc 是远端清单的完整结构，字段与 docs/CLOUD.md 一一对应。
type cloudDoc struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	Rev       string    `json:"rev"`
	TTLSec    int       `json:"ttlSec,omitempty"`
	IssuedAt  int64     `json:"issuedAt,omitempty"`
	ExpiresAt int64     `json:"expiresAt,omitempty"`
	MinApp    string    `json:"minAppVersion,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Notice    string    `json:"notice,omitempty"`
	Profiles  []Profile `json:"profiles"`
	Signature string    `json:"signature,omitempty"`
}

// cloudCache 是落盘的快照。与 cloudDoc 的差别：
// 多存 ETag 与拉取时间，少存签名（校验在采用前已完成）。
type cloudCache struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	Endpoint  string    `json:"endpoint,omitempty"`
	Rev       string    `json:"rev"`
	Mode      string    `json:"mode,omitempty"`
	Notice    string    `json:"notice,omitempty"`
	ETag      string    `json:"etag,omitempty"`
	FetchedAt int64     `json:"fetchedAt"`
	ExpiresAt int64     `json:"expiresAt,omitempty"`
	Profiles  []Profile `json:"profiles"`
}

// CloudStatus 是 /api/cloud 的输出，也是界面上那条状态行的唯一数据源。
type CloudStatus struct {
	State        string `json:"state"`  // live / cache / builtin / off
	Source       string `json:"source"` // 当前默认区实际来自：cloud / builtin
	Endpoint     string `json:"endpoint"`
	Rev          string `json:"rev,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Notice       string `json:"notice,omitempty"`
	FetchedAt    int64  `json:"fetchedAt,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
	AgeSec       int64  `json:"ageSec"`
	TTLSec       int64  `json:"ttlSec"`
	NextFetchAt  int64  `json:"nextFetchAt,omitempty"`
	ProfileCount int    `json:"profileCount"`
	LastError    string `json:"lastError,omitempty"`
	Signed       bool   `json:"signed"`
	Refreshing   bool   `json:"refreshing"`
	Version      string `json:"version"`
}

// CloudManager 负责拉取、校验、缓存与状态维护。
// 一进程一个，由 main 创建后长期持有。
type CloudManager struct {
	dir       string
	path      string
	secret    string
	allowHTTP bool

	// mu 保护下面所有状态字段；网络请求一律在锁外进行。
	mu         sync.Mutex
	endpoint   string
	state      string
	profiles   []Profile // 云端原始清单（未与出厂清单合并）
	mode       string
	rev        string
	notice     string
	etag       string
	fetchedAt  time.Time
	expiresAt  time.Time
	lastErr    string
	ttl        time.Duration
	nextAt     time.Time
	tries      int
	refreshing bool

	// fetchMu 串行化所有拉取：后台定时与手动刷新不会同时打远端。
	fetchMu sync.Mutex

	client   *http.Client
	wake     chan struct{}
	onChange func(ps []Profile, source, rev string)
}

func NewCloudManager(dir string) *CloudManager {
	m := &CloudManager{
		dir:      dir,
		path:     filepath.Join(dir, cloudFileName),
		endpoint: defaultCloudEndpoint,
		state:    cloudStateBuiltin,
		ttl:      cloudDefaultTTL,
		wake:     make(chan struct{}, 1),
	}
	// etc/cloud-secret 存在即启用签名校验。读不到就是「没配密钥」，
	// 不是错误 —— 未配密钥时允许先跑起来再补密钥。
	if b, err := os.ReadFile(filepath.Join(dir, cloudSecretFileName)); err == nil {
		m.secret = strings.TrimSpace(string(b))
	}
	m.client = &http.Client{
		Timeout: cloudFetchTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("重定向超过 3 次")
			}
			if strings.ToLower(req.URL.Scheme) != "https" && !m.allowHTTP {
				return errors.New("重定向到非 https 地址，已拒绝")
			}
			return nil
		},
	}
	return m
}

// SetSecret 设置 HMAC 共享密钥。空串表示关闭签名校验。
func (m *CloudManager) SetSecret(s string) {
	m.mu.Lock()
	m.secret = strings.TrimSpace(s)
	m.mu.Unlock()
}

// SetAllowHTTP 允许明文 http 端点，仅供内网调试。
func (m *CloudManager) SetAllowHTTP(ok bool) {
	m.mu.Lock()
	m.allowHTTP = ok
	m.mu.Unlock()
}

// SetEndpoint 更新清单地址并唤醒后台循环。返回是否发生了变化。
// 空串表示关闭远端拉取（state 置 off，只用缓存与出厂清单）。
func (m *CloudManager) SetEndpoint(ep string) bool {
	ep = strings.TrimSpace(ep)
	m.mu.Lock()
	changed := ep != m.endpoint
	m.endpoint = ep
	if changed {
		// 换地址后旧的 ETag 不再适用，下次必须全量拉
		m.etag = ""
		m.tries = 0
		m.nextAt = time.Now()
	}
	m.mu.Unlock()
	if changed {
		m.poke()
	}
	return changed
}

// cloudEndpointOf 把设置里的两项收敛成一个地址：
// 关掉了就是空串（停用），没填就是编译期默认值。
func cloudEndpointOf(g GlobalSettings) string {
	if g.CloudDisabled {
		return ""
	}
	if ep := strings.TrimSpace(g.CloudEndpoint); ep != "" {
		return ep
	}
	return defaultCloudEndpoint
}

// SetOnChange 注册「默认区变了」的回调。回调在后台 goroutine 里执行，
// 不要在里面做耗时操作。
func (m *CloudManager) SetOnChange(fn func(ps []Profile, source, rev string)) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

func (m *CloudManager) Endpoint() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.endpoint
}

// Path 返回快照文件路径，仅用于界面展示与排障。
func (m *CloudManager) Path() string { return m.path }

func (m *CloudManager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// 降级链
// ---------------------------------------------------------------------------

// Base 返回当前生效的默认区与来源标识。
// 云端可用（live / cache）就用云端，否则退回编译期出厂清单。
func (m *CloudManager) Base() ([]Profile, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if (m.state == cloudStateLive || m.state == cloudStateCache) && len(m.profiles) > 0 {
		return composeBase(m.mode, m.profiles), "cloud"
	}
	return DefaultPresets(), "builtin"
}

// composeBase 按 mode 把云端清单合进出厂清单。
//
//	replace（默认）—— 云端说了算，出厂清单不出现
//	append        —— 云端增量挂在出厂清单之后，同 ID 覆盖
//
// replace 是默认值：云端下架一个源要立刻生效，而不是被出厂清单「接住」复活。
func composeBase(mode string, cloud []Profile) []Profile {
	if len(cloud) == 0 {
		return nil
	}
	out := make([]Profile, 0, len(cloud))
	if mode != "append" {
		return append(out, cloud...)
	}
	out = append(out, DefaultPresets()...)
	index := make(map[string]int, len(out))
	for i, p := range out {
		index[p.ID] = i
	}
	for _, p := range cloud {
		if i, ok := index[p.ID]; ok {
			out[i] = p
			continue
		}
		index[p.ID] = len(out)
		out = append(out, p)
	}
	return out
}

// LoadCache 同步读上次的快照，让界面在联网前就有源可用。
// 返回是否成功加载到可用的云端清单。
func (m *CloudManager) LoadCache() bool {
	b, err := os.ReadFile(m.path)
	if err != nil {
		return false
	}
	var c cloudCache
	if err := json.Unmarshal(b, &c); err != nil || c.Format != cloudFormat {
		return false
	}
	ps, err := sanitizeCloudProfiles(c.Profiles)
	if err != nil {
		return false
	}
	now := time.Now()
	m.mu.Lock()
	m.state = cloudStateCache
	m.profiles = ps
	m.mode = c.Mode
	m.rev = c.Rev
	m.notice = c.Notice
	m.etag = c.ETag
	if c.FetchedAt > 0 {
		m.fetchedAt = time.Unix(c.FetchedAt, 0)
	}
	if c.ExpiresAt > 0 {
		m.expiresAt = time.Unix(c.ExpiresAt, 0)
	}
	m.ttl = m.ttlFrom(c.ExpiresAt, c.FetchedAt, now)
	m.nextAt = now
	m.mu.Unlock()
	return true
}

// ttlFrom 由清单自带的 expiresAt 反推缓存时长，缺省回落到默认值。
func (m *CloudManager) ttlFrom(expiresAt, fetchedAt int64, now time.Time) time.Duration {
	if expiresAt > 0 && fetchedAt > 0 && expiresAt > fetchedAt {
		return clampTTL(time.Duration(expiresAt-fetchedAt) * time.Second)
	}
	return clampTTL(m.ttl)
}

func clampTTL(d time.Duration) time.Duration {
	if d < cloudMinTTL {
		return cloudMinTTL
	}
	if d > cloudMaxTTL {
		return cloudMaxTTL
	}
	return d
}

// ---------------------------------------------------------------------------
// 调度
// ---------------------------------------------------------------------------

// Start 启动后台循环。首次拉取立即进行，之后的间隔由清单的 ttlSec 决定，
// 失败按指数退避重试。
func (m *CloudManager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.nextAt.IsZero() {
		m.nextAt = time.Now()
	}
	m.mu.Unlock()
	safeGo("cloudLoop", func() { m.loop(ctx) })
}

func (m *CloudManager) loop(ctx context.Context) {
	// 清掉 Start 之前可能积压的唤醒信号，免得第一轮拉完立刻又拉一次。
	select {
	case <-m.wake:
	default:
	}
	for {
		_, _ = m.refreshOnce(ctx)

		m.mu.Lock()
		d := time.Until(m.nextAt)
		m.mu.Unlock()
		if d < 5*time.Second {
			d = 5 * time.Second
		}
		if d > cloudMaxTTL {
			d = cloudMaxTTL
		}

		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-m.wake:
			t.Stop()
		}
	}
}

// RefreshNow 立即拉一次，供界面上的「刷新」按钮使用。
func (m *CloudManager) RefreshNow(ctx context.Context) (bool, error) {
	changed, err := m.refreshOnce(ctx)
	m.poke()
	return changed, err
}

// ---------------------------------------------------------------------------
// 拉取与校验
// ---------------------------------------------------------------------------

func (m *CloudManager) refreshOnce(ctx context.Context) (bool, error) {
	m.fetchMu.Lock()
	defer m.fetchMu.Unlock()

	m.mu.Lock()
	ep := m.endpoint
	etag := m.etag
	prevRev := m.rev
	hasList := len(m.profiles) > 0
	m.refreshing = true
	m.mu.Unlock()
	if !hasList {
		// 手上没有清单却带 If-None-Match，可能换回一个 304 —— 那就什么也没拿到
		etag = ""
	}

	defer func() {
		m.mu.Lock()
		m.refreshing = false
		m.mu.Unlock()
	}()

	if strings.TrimSpace(ep) == "" {
		m.mu.Lock()
		m.state = cloudStateOff
		m.lastErr = ""
		m.mu.Unlock()
		return false, nil
	}
	if err := m.checkCloudEndpoint(ep); err != nil {
		m.noteFailure(err, time.Now())
		return false, err
	}

	doc, notModified, newETag, err := m.fetch(ctx, ep, etag)
	now := time.Now()
	if err != nil {
		m.noteFailure(err, now)
		return false, err
	}

	if notModified {
		// 304：清单没变，只把有效期往后推，不重写清单。
		// 冷启动读到的是缓存快照，此时 304 等于「服务端确认你手上就是最新」，
		// 状态要从 cache 提成 live —— 否则界面会永远显示「缓存快照」。
		m.mu.Lock()
		if m.state == cloudStateCache {
			m.state = cloudStateLive
		}
		m.fetchedAt = now
		m.expiresAt = now.Add(m.ttl)
		m.nextAt = now.Add(m.ttl)
		m.tries = 0
		m.lastErr = ""
		if newETag != "" {
			m.etag = newETag
		}
		m.mu.Unlock()
		m.saveCache()
		return false, nil
	}

	ps, err := sanitizeCloudProfiles(doc.Profiles)
	if err != nil {
		m.noteFailure(err, now)
		return false, err
	}
	if doc.MinApp != "" && !versionAtLeast(appVersion, doc.MinApp) {
		err = fmt.Errorf("清单要求最低版本 %s，当前 %s，请先升级", doc.MinApp, appVersion)
		m.noteFailure(err, now)
		return false, err
	}

	rev := strings.TrimSpace(doc.Rev)
	if rev == "" {
		rev = now.UTC().Format("2006-01-02T15:04:05Z")
	}
	mode := strings.TrimSpace(doc.Mode)
	if mode != "append" {
		mode = "replace"
	}
	ttl := clampTTL(time.Duration(doc.TTLSec) * time.Second)
	if doc.TTLSec <= 0 {
		ttl = cloudDefaultTTL
	}

	// 过期时间取「清单自带」与「本地 TTL 推算」中较早的那个：
	// 云端可以主动把这份清单作废，而不必等本地 TTL 到点。
	exp := now.Add(ttl)
	if doc.ExpiresAt > 0 {
		if e := time.Unix(doc.ExpiresAt, 0); e.Before(exp) {
			exp = e
		}
	}

	changed := prevRev != rev

	m.mu.Lock()
	m.state = cloudStateLive
	m.profiles = ps
	m.mode = mode
	m.rev = rev
	m.notice = strings.TrimSpace(doc.Notice)
	m.fetchedAt = now
	m.expiresAt = exp
	m.ttl = ttl
	m.nextAt = now.Add(ttl)
	m.tries = 0
	m.lastErr = ""
	if newETag != "" {
		m.etag = newETag
	}
	fn := m.onChange
	m.mu.Unlock()

	m.saveCache()
	if fn != nil {
		ps, src := m.Base()
		fn(ps, src, rev)
	}
	return changed, nil
}

// fetch 发一次条件 GET。返回 (doc, notModified, etag, err)。
func (m *CloudManager) fetch(ctx context.Context, ep, etag string) (*cloudDoc, bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, cloudFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
	if err != nil {
		return nil, false, "", fmt.Errorf("清单地址无法解析: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "fnspeed/"+appVersion)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, false, "", fmt.Errorf("请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		return nil, true, etag, nil
	}
	if resp.StatusCode != http.StatusOK {
		// 把 body 读掉一点再关，连接才能复用；错误信息里不带 body（可能是登录页）
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, false, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, cloudMaxBody+1))
	if err != nil {
		return nil, false, "", fmt.Errorf("读取响应失败: %w", err)
	}
	if int64(len(body)) > cloudMaxBody {
		return nil, false, "", fmt.Errorf("响应超过 %d 字节，已丢弃", cloudMaxBody)
	}

	var doc cloudDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false, "", fmt.Errorf("清单不是合法 JSON: %w", err)
	}
	if doc.Format != cloudFormat {
		return nil, false, "", fmt.Errorf("format 应为 %q，实际 %q", cloudFormat, doc.Format)
	}
	if doc.Version <= 0 || doc.Version > cloudVersion {
		return nil, false, "", fmt.Errorf("不支持的清单版本 %d（本程序最高 %d）", doc.Version, cloudVersion)
	}
	if err := m.verifySignature(&doc); err != nil {
		return nil, false, "", err
	}
	return &doc, false, strings.TrimSpace(resp.Header.Get("ETag")), nil
}

// checkCloudEndpoint 是云端地址专用校验：比端点校验更严，默认要求 https。
//
// 理由：这份清单决定本机接下来向哪些地址发起大流量请求。明文通道意味着
// 任何一个中间人都能把源换成自己的服务器 —— 对压测工具来说这就是武器化。
func (m *CloudManager) checkCloudEndpoint(ep string) error {
	u, err := url.Parse(ep)
	if err != nil || u.Hostname() == "" {
		return errors.New("云端地址不合法（需要完整 URL）")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		m.mu.Lock()
		allow := m.allowHTTP
		m.mu.Unlock()
		if allow {
			return nil
		}
		return errors.New("云端地址必须是 https（内网调试可用 -cloud-allow-http 显式放行）")
	default:
		return fmt.Errorf("云端地址只支持 http/https，实际是 %s://", u.Scheme)
	}
}

// sanitizeCloudProfiles 校验并规范化云端清单。
//
// 任何一条不合法就整份拒绝，不做「丢掉坏的留下好的」：一份清单里混进
// 非法地址通常意味着签名/传输环节出了问题，残缺采用比拒绝更危险。
func sanitizeCloudProfiles(in []Profile) ([]Profile, error) {
	if len(in) == 0 {
		return nil, errors.New("清单里没有任何配置集")
	}
	if len(in) > cloudMaxProfiles {
		return nil, fmt.Errorf("配置集数量 %d 超过上限 %d", len(in), cloudMaxProfiles)
	}
	out := make([]Profile, 0, len(in))
	seen := make(map[string]bool, len(in))
	for i := range in {
		p := in[i]
		p.ID = strings.TrimSpace(p.ID)
		if p.ID == "" || len(p.ID) > 64 {
			return nil, fmt.Errorf("第 %d 个配置集的 id 为空或过长", i+1)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("配置集 id 重复: %s", p.ID)
		}
		seen[p.ID] = true
		if len(p.Name) > 120 {
			p.Name = truncRunes(p.Name, 120)
		}
		if len(p.Desc) > 300 {
			p.Desc = truncRunes(p.Desc, 300)
		}
		normalizeProfile(&p)
		n := len(p.DownloadEndpoints) + len(p.UploadEndpoints)
		if n == 0 {
			return nil, fmt.Errorf("配置集 %s 没有端点", p.ID)
		}
		if n > maxEndpoints {
			return nil, fmt.Errorf("配置集 %s 有 %d 个端点，超过单次任务上限 %d", p.ID, n, maxEndpoints)
		}
		if bad, why := firstBadEndpoint(&p); bad != "" {
			return nil, fmt.Errorf("配置集 %s 的端点 %s 不合法（%s）", p.ID, bad, why)
		}
		out = append(out, p)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 签名
// ---------------------------------------------------------------------------

// cloudCanonical 是签名原文，格式写死在 docs/CLOUD.md。
//
// 刻意不用「profiles 的 JSON 序列化」：Go、Python、Node 对 JSON 的键顺序、
// 空白、HTML 转义、omitempty 处理各不相同，服务端几乎不可能拼出逐字节相同
// 的串。改成纯文本行协议，任何语言十行以内就能复现：
//
//	FNS_CLOUD_SOURCES
//	1
//	<rev>
//	PROFILE<TAB><id>
//	DOWN<TAB><url>
//	UP<TAB><url>
//	...
//
// 只签「决定源在哪」的字段：格式、版本、rev、每个配置集的 id 与全部端点地址。
// notice、ttlSec 这类展示性字段不签 —— 改它们不改变压测会打到谁。
func cloudCanonical(doc *cloudDoc) []byte {
	var b strings.Builder
	b.WriteString(doc.Format)
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(doc.Version))
	b.WriteByte('\n')
	b.WriteString(doc.Rev)
	b.WriteByte('\n')
	for i := range doc.Profiles {
		p := &doc.Profiles[i]
		b.WriteString("PROFILE\t")
		b.WriteString(strings.TrimSpace(p.ID))
		b.WriteByte('\n')
		for _, u := range cloudURLs(p, false) {
			b.WriteString("DOWN\t")
			b.WriteString(u)
			b.WriteByte('\n')
		}
		for _, u := range cloudURLs(p, true) {
			b.WriteString("UP\t")
			b.WriteString(u)
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

// cloudURLs 按「纯 URL 列表在前、endpoints 在后」合并，顺序即签名顺序。
func cloudURLs(p *Profile, upload bool) []string {
	var raw []string
	var eps []Endpoint
	if upload {
		raw, eps = p.UploadURLs, p.UploadEndpoints
	} else {
		raw, eps = p.DownloadURLs, p.DownloadEndpoints
	}
	out := make([]string, 0, len(raw)+len(eps))
	for _, u := range raw {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	for _, e := range eps {
		if u := strings.TrimSpace(e.URL); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// CloudSign 计算清单签名，供服务端生成与本地校验共用。
func CloudSign(secret string, doc *cloudDoc) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(cloudCanonical(doc))
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *CloudManager) verifySignature(doc *cloudDoc) error {
	m.mu.Lock()
	secret := m.secret
	m.mu.Unlock()
	if secret == "" {
		// 没配密钥就不校验，但要让人能从状态里看出「这份清单没验过」。
		return nil
	}
	got := strings.ToLower(strings.TrimSpace(doc.Signature))
	got = strings.TrimPrefix(got, "hmac-sha256=")
	want := CloudSign(secret, doc)
	if got == "" || !hmac.Equal([]byte(want), []byte(got)) {
		return errors.New("签名校验失败：清单可能被篡改，已拒绝采用")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 状态与落盘
// ---------------------------------------------------------------------------

// Snapshot 汇总一份给界面看的现状。
func (m *CloudManager) Snapshot() CloudStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := CloudStatus{
		State:        m.state,
		Endpoint:     m.endpoint,
		Rev:          m.rev,
		Mode:         m.mode,
		Notice:       m.notice,
		LastError:    m.lastErr,
		Signed:       m.secret != "",
		Refreshing:   m.refreshing,
		Version:      appVersion,
		ProfileCount: len(composeBase(m.mode, m.profiles)),
		TTLSec:       int64(m.ttl / time.Second),
	}
	if (m.state == cloudStateLive || m.state == cloudStateCache) && len(m.profiles) > 0 {
		st.Source = "cloud"
	} else {
		st.Source = "builtin"
		st.ProfileCount = len(DefaultPresets())
	}
	if !m.fetchedAt.IsZero() {
		st.FetchedAt = m.fetchedAt.Unix()
		st.AgeSec = int64(time.Since(m.fetchedAt) / time.Second)
		if st.AgeSec < 0 {
			st.AgeSec = 0
		}
	}
	if !m.expiresAt.IsZero() {
		st.ExpiresAt = m.expiresAt.Unix()
	}
	if !m.nextAt.IsZero() {
		st.NextFetchAt = m.nextAt.Unix()
	}
	return st
}

func (m *CloudManager) noteFailure(err error, now time.Time) {
	back := cloudBackoffMin
	m.mu.Lock()
	m.tries++
	for i := 0; i < m.tries && back < cloudBackoffMax; i++ {
		back *= 2
	}
	if back > cloudBackoffMax {
		back = cloudBackoffMax
	}
	m.nextAt = now.Add(back)
	m.lastErr = err.Error()
	// 已到手的清单照常生效：拉不到新的不代表旧的就该作废。
	if m.state != cloudStateLive && m.state != cloudStateCache {
		m.state = cloudStateBuiltin
	}
	m.mu.Unlock()
}

// saveCache 把当前清单落盘。写失败不影响运行，只是下次冷启动少一份快照。
func (m *CloudManager) saveCache() {
	m.mu.Lock()
	if len(m.profiles) == 0 {
		m.mu.Unlock()
		return
	}
	c := cloudCache{
		Format:   cloudFormat,
		Version:  cloudVersion,
		Endpoint: m.endpoint,
		Rev:      m.rev,
		Mode:     m.mode,
		Notice:   m.notice,
		ETag:     m.etag,
		Profiles: m.profiles,
		ExpiresAt: func() int64 {
			if m.expiresAt.IsZero() {
				return 0
			}
			return m.expiresAt.Unix()
		}(),
		FetchedAt: func() int64 {
			if m.fetchedAt.IsZero() {
				return 0
			}
			return m.fetchedAt.Unix()
		}(),
	}
	m.mu.Unlock()

	_ = atomicWriteJSON(m.dir, m.path, c)
}

// ---------------------------------------------------------------------------
// 版本比较
// ---------------------------------------------------------------------------

// versionAtLeast 判断 cur >= min。只理解 "x.y.z" 与 "x.y.z-pre" 两种形态，
// 够用于清单里的 minAppVersion 门槛。
//
// 带后缀的版本小于同号不带后缀的版本：1.6.0-alpha < 1.6.0 —— 这是 semver 的
// 语义，也是「alpha 版不该被当成正式版」的正确表达。
func versionAtLeast(cur, min string) bool {
	c := parseVersion(cur)
	mn := parseVersion(min)
	for i := 0; i < 3; i++ {
		if c.num[i] != mn.num[i] {
			return c.num[i] > mn.num[i]
		}
	}
	if c.pre == mn.pre {
		return true
	}
	if c.pre == "" {
		return true
	}
	if mn.pre == "" {
		return false
	}
	return c.pre >= mn.pre
}

type semver struct {
	num [3]int
	pre string
}

// truncRunes 按字符截断。云端清单里全是中文字段，按字节切会把最后一个字切成
// 半个 UTF-8 序列，最终 JSON 里出现 U+FFFD。
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func parseVersion(s string) semver {
	var out semver
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		out.pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		n, _ := strconv.Atoi(strings.TrimSpace(parts[i]))
		out.num[i] = n
	}
	return out
}
