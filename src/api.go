package main

// HTTP API + SSE。前端每 500ms 收一帧 /api/stream。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type App struct {
	mu sync.Mutex
	// startMu 只保护「一个任务从判空到置位」这一段。
	// 原先的判空在 a.mu 里、置位在另一个临界区，中间隔着 buildProfile，
	// 两个设备同时点「开始」时都能通过判空，于是同时跑起两个引擎：
	// 共享同一份 stats（字节数翻倍、平均值虚高），而 a.engine 只记得后一个，
	// 前一个再也停不掉。
	startMu    sync.Mutex
	engine     *Engine
	stats      *Stats
	cfg        *Config
	store      *Store
	dataDir    string
	settings   *SettingsStore
	note       string
	lastReason string
	lastURLs   []URLOut
	lastPing   PingSnapshot
	lastRes    ResStat
}

func NewApp(dataDir string, store *Store, settings *SettingsStore) *App {
	return &App{stats: NewStats(), dataDir: dataDir, store: store, settings: settings}
}

// handleSettings 读写全局测速参数（并发/时长/流量上限）。
// 与配置集无关：换源不影响这两个值。
func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"settings": a.settings.Get()})
	case http.MethodPost:
		var g GlobalSettings
		if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
			return
		}
		saved, err := a.settings.Set(g)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"settings": saved})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET/POST")
	}
}

// handleRegions 返回可选的测速位置（省 + 运营商），供设置区下拉使用。
// 全量一次吐出，前端自己分组 —— 数据量只有几十条，没必要分页。
func (a *App) handleRegions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"current": a.settings.Get().Region,
		"groups":  RegionOptions(),
		"total":   len(regionNodes),
	})
}

func (a *App) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/api/stream", a.handleStream)
	mux.HandleFunc("/api/start", a.handleStart)
	mux.HandleFunc("/api/stop", a.handleStop)
	mux.HandleFunc("/api/probe", a.handleProbe)
	mux.HandleFunc("/api/profiles", a.handleProfiles)
	mux.HandleFunc("/api/settings", a.handleSettings)
	mux.HandleFunc("/api/regions", a.handleRegions)
	mux.HandleFunc("/api/config", a.handleProfiles) // 兼容旧路径
	mux.HandleFunc("/api/config/export", a.handleExport)
	mux.HandleFunc("/api/presets/restore", a.handleRestorePresets)
	mux.HandleFunc("/api/selftest", a.handleSelfTest)
	mux.HandleFunc("/api/ipinfo", a.handleIPInfo)
	mux.HandleFunc("/api/hits", a.handleHits)
	mux.HandleFunc("/api/history", a.handleHistory)
}

// handleRestorePresets 把内置测速源合并回配置（用户自建配置保留）。
func (a *App) handleRestorePresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	_, err := a.store.RestorePresets()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	saved, note := a.store.Load()
	a.mu.Lock()
	a.cfg, a.note = saved, note
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"config": saved, "note": note, "path": a.store.Path(), "count": len(saved.Profiles),
	})
}

// mergeConfigs 按 id 合并：incoming 覆盖同 id 的旧项，其余旧项保留。
func mergeConfigs(base, incoming *Config) *Config {
	if base == nil || len(base.Profiles) == 0 {
		return incoming
	}
	out := &Config{
		Format:            ConfigFormat,
		Version:           ConfigVersion,
		SelectedProfileID: base.SelectedProfileID,
		Profiles:          append([]Profile(nil), base.Profiles...),
	}
	for _, p := range incoming.Profiles {
		replaced := false
		for i := range out.Profiles {
			if out.Profiles[i].ID == p.ID {
				out.Profiles[i] = p
				replaced = true
				break
			}
		}
		if !replaced {
			out.Profiles = append(out.Profiles, p)
		}
	}
	if incoming.SelectedProfileID != "" {
		out.SelectedProfileID = incoming.SelectedProfileID
	}
	return out
}

func (a *App) snapshot(limit int) Snapshot {
	a.mu.Lock()
	eng := a.engine
	lastReason := a.lastReason
	lastURLs := a.lastURLs
	lastPing := a.lastPing
	lastRes := a.lastRes
	a.mu.Unlock()

	var snap Snapshot
	if eng != nil {
		snap = a.stats.Snapshot(limit, eng.StopReason(), eng.URLStates())
		snap.Ping = eng.PingSnapshot()
		snap.Res = eng.ResSnapshot()
		if n := eng.StreamCount(); n > 0 {
			snap.Concurrency = n
		}
	} else {
		snap = a.stats.Snapshot(limit, lastReason, lastURLs)
		snap.Ping = lastPing
		snap.Res = lastRes
	}
	snap.Verdict = Judge(snap)
	return snap
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.snapshot(600))
}

// handleStream 是 SSE 端点，每 500ms 推一个快照。
// 必须带 X-Accel-Buffering: no，否则 fnOS 的 Nginx 会缓冲整段响应，曲线变成一次性刷出。
func (a *App) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "不支持流式响应")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	defer guard("sse")
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			b, err := json.Marshal(a.snapshot(300))
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

type startReq struct {
	ProfileID    string   `json:"profileId"`
	Authorized   bool     `json:"authorized"`
	Concurrency  int      `json:"concurrency"`
	DurationSec  int      `json:"durationSec"`
	BytesLimit   int64    `json:"bytesLimit"`
	Mode         string   `json:"mode"`
	DownloadURLs []string `json:"downloadUrls"`
	UploadURLs   []string `json:"uploadUrls"`
}

func (a *App) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	// 串行化「判空 → 建引擎 → 置位」，避免两个并发请求各自起一个引擎。
	a.startMu.Lock()
	defer a.startMu.Unlock()

	var req startReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if !req.Authorized {
		writeErr(w, http.StatusForbidden, "需要先勾选「目标为自有或已授权资源」")
		return
	}

	a.mu.Lock()
	if a.engine != nil {
		a.mu.Unlock()
		writeErr(w, http.StatusConflict, "已有任务在运行，请先停止")
		return
	}
	a.mu.Unlock()

	p := a.buildProfile(req)
	if len(p.DownloadEndpoints) == 0 && len(p.UploadEndpoints) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可用的端点，请先填入目标 URL")
		return
	}

	a.stats.Reset()
	// 预检要花几秒，先把状态置成运行中，UI 立刻进入爬坡态，不让用户盯空白
	a.stats.Running.Store(true)
	a.stats.SetPhase("ramp")
	a.stats.StartAt = time.Now()
	eng := NewEngine(p, a.stats, a.settings.Get().Region)

	a.mu.Lock()
	a.engine = eng
	a.lastReason = ""
	a.lastURLs = nil
	a.mu.Unlock()

	go func() {
		defer guard("engine")

		eng.Prepare()
		n := eng.Start()
		if n == 0 {
			eng.Stop("没有可运行的流")
		}
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		go func() {
			for range tick.C {
				if !a.stats.Running.Load() {
					return
				}
				a.stats.Tick()
			}
		}()
		eng.Wait(a.dataDir)
		tick.Stop()

		a.mu.Lock()
		if a.engine == eng {
			a.engine = nil
			a.lastReason = eng.StopReason()
			a.lastURLs = eng.URLStates()
			a.lastPing = eng.PingSnapshot()
			a.lastRes = eng.ResSnapshot()
		}
		a.mu.Unlock()
	}()

	writeJSON(w, http.StatusOK, map[string]any{"started": true, "streams": initialStreams, "maxStreams": p.Concurrency})
}

func (a *App) buildProfile(req startReq) *Profile {
	a.mu.Lock()
	var base Profile
	if a.cfg != nil {
		for _, p := range a.cfg.Profiles {
			if p.ID == req.ProfileID || (req.ProfileID == "" && p.ID == a.cfg.SelectedProfileID) {
				base = p
				break
			}
		}
	}
	a.mu.Unlock()

	if base.ID == "" {
		base.ID = "adhoc"
		base.Name = "临时任务"
	}
	if len(req.DownloadURLs) > 0 {
		var eps []Endpoint
		for _, u := range req.DownloadURLs {
			if u = strings.TrimSpace(u); u != "" {
				eps = append(eps, Endpoint{URL: u, Method: "GET"})
			}
		}
		base.DownloadEndpoints = eps
		base.DownloadURLs = nil
	}
	if len(req.UploadURLs) > 0 {
		var eps []Endpoint
		for _, u := range req.UploadURLs {
			if u = strings.TrimSpace(u); u != "" {
				eps = append(eps, Endpoint{URL: u, Method: "PUT"})
			}
		}
		base.UploadEndpoints = eps
		base.UploadURLs = nil
	}
	// 并发/时长按全局设置来，配置集里带的值一律忽略。
	// req 里显式传了仍可覆盖（保留给脚本/调试用），前端不再传。
	gs := a.settings.Get()
	base.Concurrency = gs.Concurrency
	base.DurationSec = gs.DurationSec
	if gs.BytesLimit > 0 {
		base.BytesLimit = gs.BytesLimit
	}
	if req.Concurrency > 0 {
		base.Concurrency = req.Concurrency
	}
	if req.DurationSec > 0 {
		base.DurationSec = req.DurationSec
	}
	if req.BytesLimit > 0 {
		base.BytesLimit = req.BytesLimit
	}
	if req.Mode != "" {
		base.Mode = req.Mode
	} else if base.Mode == "" {
		base.Mode = "both"
	}
	normalizeProfile(&base)
	return &base
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	a.mu.Lock()
	eng := a.engine
	a.mu.Unlock()
	if eng == nil {
		writeJSON(w, http.StatusOK, map[string]any{"stopped": false, "reason": "没有运行中的任务"})
		return
	}
	eng.Stop("手动停止")
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}

type probeReq struct {
	URLs []string `json:"urls"`
	Kind string   `json:"kind"`
}

func (a *App) handleProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	var req probeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	type item struct {
		URL    string `json:"url"`
		Status string `json:"status"`
		Mode   string `json:"mode"`
		Size   int64  `json:"size"`
		Err    string `json:"error,omitempty"`
	}
	out := make([]item, 0, len(req.URLs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 6)
	for _, u := range req.URLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if req.Kind == "upload" {
			it := item{URL: u, Status: string(StatusOK), Mode: string(ModeRaw)}
			if isHomepageURL(u) {
				it.Status, it.Err = string(StatusInvalid), "非直传端点（指向站点首页）"
			} else if _, err := url.Parse(u); err != nil {
				it.Status, it.Err = string(StatusInvalid), "URL 无法解析"
			}
			out = append(out, it)
			continue
		}
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := ProbeURL(r.Context(), Endpoint{URL: u, Method: "GET"})
			mu.Lock()
			out = append(out, item{URL: u, Status: string(res.Status), Mode: string(res.Mode), Size: res.Size, Err: res.Err})
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// handleProfiles 是配置集唯一的读写入口。
// POST 一律落盘后回读，返回磁盘上的真实内容，前端据此重绘（不做本地乐观更新）。
func (a *App) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.mu.Lock()
		cfg, note := a.cfg, a.note
		a.mu.Unlock()
		if cfg == nil {
			cfg = emptyConfig()
		}
		writeJSON(w, http.StatusOK, map[string]any{"config": cfg, "note": note, "path": a.store.Path(), "settings": a.settings.Get()})

	case http.MethodPost:
		raw, err := readAllLimited(r, 4<<20)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		cfg, err := ParseConfig(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// 默认「按 id 合并」：前端只提交当前这一条，不能把其余配置集冲掉。
		// 只有在导入备份时才显式传 replace=true 做全量替换。
		var opts struct {
			Replace bool `json:"replace"`
		}
		_ = json.Unmarshal(raw, &opts)

		if !opts.Replace {
			a.mu.Lock()
			cur := a.cfg
			a.mu.Unlock()
			cfg = mergeConfigs(cur, cfg)
		}

		if err := a.store.Save(cfg); err != nil {
			writeErr(w, http.StatusInternalServerError, "写入失败: "+err.Error())
			return
		}
		saved, note := a.store.Load()
		a.mu.Lock()
		a.cfg, a.note = saved, note
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"config": saved, "note": note, "path": a.store.Path(),
			"savedAt": time.Now().Unix(),
		})

	case http.MethodDelete:
		// 删除配置集：DELETE /api/profiles?id=xxx[&id=yyy]
		// 也支持 DELETE /api/profiles/xxx
		ids := r.URL.Query()["id"]
		if p := strings.TrimPrefix(r.URL.Path, "/api/profiles"); p != "" && p != "/" {
			ids = append(ids, strings.Trim(p, "/"))
		}
		// 再兜一层：body 里传 {"ids":[...]} 也能删
		if len(ids) == 0 {
			if raw, err := readAllLimited(r, 1<<20); err == nil && len(raw) > 0 {
				var body struct {
					IDs []string `json:"ids"`
					ID  string   `json:"id"`
				}
				if json.Unmarshal(raw, &body) == nil {
					ids = append(ids, body.IDs...)
					if body.ID != "" {
						ids = append(ids, body.ID)
					}
				}
			}
		}
		if len(ids) == 0 {
			writeErr(w, http.StatusBadRequest, "缺少 id")
			return
		}
		saved, removed, err := a.store.Delete(ids...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error())
			return
		}
		a.mu.Lock()
		a.cfg, a.note = saved, ""
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"config": saved, "removed": removed, "path": a.store.Path(),
			"note": fmt.Sprintf("已删除 %d 个配置集", removed),
		})

	default:
		writeErr(w, http.StatusMethodNotAllowed, "只接受 GET/POST/DELETE")
	}
}

func (a *App) handleExport(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	if cfg == nil {
		writeErr(w, http.StatusNotFound, "还没有导入配置")
		return
	}
	mode := r.URL.Query().Get("mode")
	var (
		b   []byte
		err error
	)
	if mode == "full" {
		b, err = cfg.ExportFull()
	} else {
		b, err = cfg.ExportStandard()
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="fnspeed-config-`+mode+`.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(a.dataDir, "history.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	items := make([]json.RawMessage, 0, len(lines))
	for _, ln := range lines {
		if ln = strings.TrimSpace(ln); ln == "" {
			continue
		}
		items = append(items, json.RawMessage(ln))
	}
	if len(items) > 50 {
		items = items[len(items)-50:]
	}
	sort.SliceStable(items, func(i, j int) bool { return i > j })
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func readAllLimited(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 32<<10)
	var total int64
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			total += int64(n)
			if total > max {
				return nil, fmt.Errorf("请求体超过 %d 字节", max)
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// ---------------- 网络自检 ----------------

func (a *App) handleSelfTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	var req struct {
		URLs []string `json:"urls"`
	}
	if raw, err := readAllLimited(r, 1<<20); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}

	urls := cleanURLList(req.URLs)
	if len(urls) == 0 {
		// 不传就测当前配置里的全部端点
		a.mu.Lock()
		cfg := a.cfg
		a.mu.Unlock()
		for _, p := range cfg.Profiles {
			urls = append(urls, p.DownloadURLs...)
			urls = append(urls, p.UploadURLs...)
		}
	}
	if len(urls) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可检测的 URL")
		return
	}

	env := EnvProbe()
	results := SelfTestMany(r.Context(), urls)
	okCount := 0
	for _, x := range results {
		if x.OK {
			okCount++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"env":     env,
		"results": results,
		"total":   len(results),
		"ok":      okCount,
	})
}
