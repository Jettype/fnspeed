package main

// 全局测速参数：并发 / 时长 / 流量上限。
//
// 设计原则：这些参数不跟随单个配置集 —— 换源只换 URL，
// 并发和时长全局只有一份，改一次所有源都生效，重启后保留。
// 落盘位置 etc/settings.json，与 builtin.json / user.json 分开，
// 导入配置文件不会覆盖这两个值。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	// 并发是上限而不是起步值：调度器只在这个范围内爬坡，填 4 就最多 4 条。
	// 默认给到编译期上限，等价于「不主动限制」，与 1.5.6 之前的行为一致。
	defaultConcurrency = maxConcurrency
	defaultDurationSec = 60
	defaultBytesLimit  = 0 // 0 = 不限量
)

type GlobalSettings struct {
	Concurrency int   `json:"concurrency"`
	DurationSec int   `json:"durationSec"`
	BytesLimit  int64 `json:"bytesLimit,omitempty"`
	// ShowIP 控制界面是否渲染出口/命中 IP。关掉后只显示「已隐藏」，
	// 服务端也不再去请求 echo 服务 —— 截图分享时不留痕。
	ShowIP bool `json:"showIp"`
	// ShowHit 控制端点状态里是否逐条展开「命中 IP · CDN」。
	// 它是 ShowIP 的子开关：总闸关掉时这一行根本不渲染，服务端也不探测。
	ShowHit bool `json:"showHit"`
	// Region 是定向测速位置，空串或 auto 表示就近（跟随本机 DNS 调度）。
	// 非空时用 ECS 让 CDN 返回该省该运营商的节点，再强制拨号过去。
	Region string `json:"region,omitempty"`
}

type SettingsStore struct {
	mu   sync.Mutex
	dir  string
	path string
	val  GlobalSettings
}

func NewSettingsStore(dir string) *SettingsStore {
	s := &SettingsStore{dir: dir, path: filepath.Join(dir, "settings.json")}
	s.val = s.load()
	return s
}

func defaults() GlobalSettings {
	// ShowHit 默认关：默认不采集任何对端地址，想看得自己打开。
	return GlobalSettings{Concurrency: defaultConcurrency, DurationSec: defaultDurationSec, ShowIP: true}
}

func (s *SettingsStore) load() GlobalSettings {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return defaults()
	}
	var g GlobalSettings
	if err := json.Unmarshal(b, &g); err != nil {
		return defaults()
	}
	// 老版本 settings.json 里没有 showIp 这个键：默认开着。
	// 不能靠零值判断，否则升级后开关会莫名其妙变成关。
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) == nil {
		if _, ok := raw["showIp"]; !ok {
			g.ShowIP = true
		}
	} else {
		g.ShowIP = true
	}
	normalizeSettings(&g)
	return g
}

func (s *SettingsStore) Get() GlobalSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.val
}

// Set 归一化后原子写盘。返回落盘后的值供前端回读。
func (s *SettingsStore) Set(g GlobalSettings) (GlobalSettings, error) {
	normalizeSettings(&g)
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return s.val, err
	}
	tmp, err := os.CreateTemp(s.dir, ".settings-*.tmp")
	if err != nil {
		return s.val, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return s.val, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return s.val, err
	}
	tmp.Close()
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return s.val, fmt.Errorf("落盘失败: %w", err)
	}
	s.val = g
	return g, nil
}

func normalizeSettings(g *GlobalSettings) {
	if g.Concurrency <= 0 {
		g.Concurrency = defaultConcurrency
	}
	if g.Concurrency > maxConcurrency {
		g.Concurrency = maxConcurrency
	}
	if g.DurationSec <= 0 {
		g.DurationSec = defaultDurationSec
	}
	if g.DurationSec > maxDurationSec {
		g.DurationSec = maxDurationSec
	}
	if g.BytesLimit < 0 {
		g.BytesLimit = 0
	}
	// auto 与空串等价；认不出的 id 一律回落就近，避免存了个不存在的省份
	// 之后整个定向功能静默失效。
	if g.Region == "auto" {
		g.Region = ""
	}
	if g.Region != "" {
		if _, ok := regionIndex[g.Region]; !ok {
			g.Region = ""
		}
	}
}
