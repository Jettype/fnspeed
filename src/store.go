package main

// 配置持久化：默认配置与自建配置物理分离。
//
//	builtin.json —— 程序托管。每次启动比对 hash，不同就整体覆盖。
//	user.json    —— 用户托管。安装、升级、恢复默认都不会碰它。
//
// 「默认分组删不掉」在这个结构下不可能发生：默认区的清理靠整体替换文件，
// 不靠逐条比对 ID。用户不想看某个默认分组，就把它的 ID 记进 user.json 的
// hidden —— 既不污染默认区，也不会被下次升级塞回来。
//
// user.json 只存「增量」：自建的、改过的、隐藏的。原样未动的默认分组不写入，
// 否则第一次保存就会把默认区整份复制过来，builtin 的覆盖就失效了。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	builtinFileName = "builtin.json"
	userFileName    = "user.json"
	legacyFileName  = "profiles.json" // 1.5.0 及以前的合并文件
)

// builtinDoc 是默认配置快照。Hash 用来判断要不要重写：
// 比版本号可靠 —— 改了源却忘了加版本号也不会漏更新。
type builtinDoc struct {
	Format   string    `json:"format"`
	Version  int       `json:"version"`
	Hash     string    `json:"hash"`
	Profiles []Profile `json:"profiles"`
}

// userDoc 是用户区。Hidden 记录被用户主动隐藏的默认分组 ID。
type userDoc struct {
	Format            string    `json:"format"`
	Version           int       `json:"version"`
	SelectedProfileID string    `json:"selectedProfileId"`
	Hidden            []string  `json:"hidden,omitempty"`
	Profiles          []Profile `json:"profiles"`
}

type Store struct {
	mu          sync.Mutex
	dir         string
	builtinPath string
	userPath    string
	legacyPath  string
}

func NewStore(dir string) *Store {
	return &Store{
		dir:         dir,
		builtinPath: filepath.Join(dir, builtinFileName),
		userPath:    filepath.Join(dir, userFileName),
		legacyPath:  filepath.Join(dir, legacyFileName),
	}
}

// Path 返回用户可编辑的那份文件路径，用于界面展示。
func (s *Store) Path() string { return s.userPath }

// ---------------------------------------------------------------------------
// 读写
// ---------------------------------------------------------------------------

func (s *Store) readBuiltin() (*builtinDoc, error) {
	b, err := os.ReadFile(s.builtinPath)
	if err != nil {
		return nil, err
	}
	var d builtinDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) readUser() (*userDoc, error) {
	b, err := os.ReadFile(s.userPath)
	if err != nil {
		return nil, err
	}
	var d userDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) writeBuiltin(d *builtinDoc) error {
	d.Format, d.Version = ConfigFormat, ConfigVersion
	return atomicWriteJSON(s.dir, s.builtinPath, d)
}

func (s *Store) writeUser(d *userDoc) error {
	d.Format, d.Version = ConfigFormat, ConfigVersion
	if d.Profiles == nil {
		d.Profiles = []Profile{} // 序列化成 []，别让前端拿到 null
	}
	return atomicWriteJSON(s.dir, s.userPath, d)
}

// atomicWriteJSON 临时文件 + fsync + rename，替换前把旧文件另存 .bak。
func atomicWriteJSON(dir, path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cfg-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && len(old) > 0 {
		_ = os.WriteFile(path+".bak", old, 0644)
	}
	return os.Rename(tmpName, path)
}

// ---------------------------------------------------------------------------
// 视图合并
// ---------------------------------------------------------------------------

// builtinHash 对预设序列化后取 sha256。DefaultPresets 每次返回一致结果，
// 所以这个值稳定；内置清单一旦改动，hash 必变，触发整体覆盖。
func builtinHash(ps []Profile) string {
	b, err := json.Marshal(ps)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// mergeView 把默认区与用户区拼成对外视图：
// 默认在前，用户同 ID 覆盖，用户独有的追加，hidden 里的剔除。
func mergeView(b *builtinDoc, u *userDoc) *Config {
	out := &Config{Format: ConfigFormat, Version: ConfigVersion}
	hidden := make(map[string]bool, len(u.Hidden))
	for _, id := range u.Hidden {
		hidden[id] = true
	}
	index := make(map[string]int, len(b.Profiles)+len(u.Profiles))
	for _, p := range b.Profiles {
		if hidden[p.ID] {
			continue
		}
		index[p.ID] = len(out.Profiles)
		out.Profiles = append(out.Profiles, p)
	}
	for _, p := range u.Profiles {
		if hidden[p.ID] {
			continue
		}
		if i, ok := index[p.ID]; ok {
			out.Profiles[i] = p // 用户改过的覆盖默认
			continue
		}
		index[p.ID] = len(out.Profiles)
		out.Profiles = append(out.Profiles, p)
	}
	out.SelectedProfileID = u.SelectedProfileID
	if _, ok := index[out.SelectedProfileID]; !ok {
		if len(out.Profiles) > 0 {
			out.SelectedProfileID = out.Profiles[0].ID
		} else {
			out.SelectedProfileID = ""
		}
	}
	return out
}

// profileSig 判断某个分组是否被用户改过。
// 全局参数（并发/时长/上限）不属于分组身份，参与比较会让所有分组都被判成"改过"。
func profileSig(p Profile) string {
	q := p
	q.Concurrency, q.DurationSec, q.BytesLimit = 0, 0, 0
	q.ForkedFrom = ""
	b, _ := json.Marshal(q)
	return string(b)
}

// extractUserProfiles 从合并视图里挑出「属于用户区」的部分。
// 与默认区完全一致的分组不写入，否则第一次保存就把默认区整份复制过来。
func extractUserProfiles(view []Profile, b *builtinDoc) []Profile {
	builtinSig := make(map[string]string, len(b.Profiles))
	for _, p := range b.Profiles {
		builtinSig[p.ID] = profileSig(p)
	}
	out := make([]Profile, 0, len(view))
	for _, p := range view {
		sig, inBuiltin := builtinSig[p.ID]
		if inBuiltin && sig == profileSig(p) {
			continue // 原样未动，交给默认区
		}
		if inBuiltin && p.ForkedFrom == "" {
			p.ForkedFrom = p.ID // 改过的默认分组自动标记为副本
		}
		out = append(out, p)
	}
	return out
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// Load 返回合并视图。任何一个文件损坏都不会让界面空掉。
func (s *Store) Load() (*Config, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (*Config, string) {
	b, errB := s.readBuiltin()
	if errB != nil {
		ps := DefaultPresets()
		b = &builtinDoc{Profiles: ps, Hash: builtinHash(ps)}
	}
	u, errU := s.readUser()
	note := ""
	if errU != nil && !os.IsNotExist(errU) {
		u = &userDoc{}
		note = "user.json 已损坏，本次以默认配置启动（原文件未动）"
	}
	if u == nil {
		u = &userDoc{}
	}
	c := mergeView(b, u)
	if len(c.Profiles) == 0 && note == "" {
		note = "配置为空，可点「恢复默认」重建"
	}
	return c, note
}

// EnsureSeeded 是启动入口：
//  1. user.json 不存在且存在旧 profiles.json → 一次性迁移
//  2. user.json 不存在 → 首次安装
//  3. 其余情况 → 只同步默认区（hash 不同就整体覆盖）
func (s *Store) EnsureSeeded() (*Config, bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	note := ""
	wrote := false

	if _, err := os.Stat(s.userPath); os.IsNotExist(err) {
		legacy := 0
		if _, err := os.Stat(s.legacyPath); err == nil {
			// 迁移：旧文件里分不清哪些是用户自建，按约定整体丢弃。
			// 不删原文件，改名留档，用户随时能捡回来。
			legacy = countProfiles(s.legacyPath)
			bak := s.legacyPath + ".legacy.bak"
			_ = os.Remove(bak)
			if err := os.Rename(s.legacyPath, bak); err != nil {
				return nil, false, "", fmt.Errorf("旧配置改名失败: %w", err)
			}
		}
		ps := DefaultPresets()
		if err := s.writeBuiltin(&builtinDoc{Hash: builtinHash(ps), Profiles: ps}); err != nil {
			return nil, false, "", err
		}
		sel := ""
		if len(ps) > 0 {
			sel = ps[0].ID
		}
		if err := s.writeUser(&userDoc{SelectedProfileID: sel}); err != nil {
			return nil, false, "", err
		}
		wrote = true
		if legacy > 0 {
			note = fmt.Sprintf("已升级配置结构：清理 %d 个旧配置集，原文件备份为 %s.legacy.bak", legacy, legacyFileName)
		}
		c, n := s.loadLocked()
		if note == "" {
			note = n
		}
		return c, wrote, note, nil
	}

	changed, err := s.syncBuiltinLocked()
	if err != nil {
		return nil, false, "", err
	}
	wrote = changed
	if changed {
		note = "默认配置已更新到当前版本，你的自建配置未受影响"
	}
	c, n := s.loadLocked()
	if note == "" {
		note = n
	}
	return c, wrote, note, nil
}

func countProfiles(path string) int {
	c, err := loadConfigFile(path)
	if err != nil {
		return 0
	}
	return len(c.Profiles)
}

// syncBuiltinLocked 比对 hash，不同就整体覆盖默认区。返回是否写入。
func (s *Store) syncBuiltinLocked() (bool, error) {
	ps := DefaultPresets()
	want := builtinHash(ps)
	cur, err := s.readBuiltin()
	if err == nil && cur.Hash == want {
		return false, nil
	}
	if err := s.writeBuiltin(&builtinDoc{Hash: want, Profiles: ps}); err != nil {
		return false, err
	}
	return true, nil
}

// RestorePresets 恢复默认：强制重写默认区、清空隐藏列表，
// 并丢掉用户对默认分组的改动（否则「恢复默认」名不副实）。
// 纯自建的分组原样保留 —— 这是「恢复默认」不是「清空重来」。
func (s *Store) RestorePresets() (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ps := DefaultPresets()
	if err := s.writeBuiltin(&builtinDoc{Hash: builtinHash(ps), Profiles: ps}); err != nil {
		return nil, err
	}
	u, err := s.readUser()
	if err != nil {
		u = &userDoc{}
	}
	u.Hidden = nil
	builtinIDs := make(map[string]bool, len(ps))
	for _, p := range ps {
		builtinIDs[p.ID] = true
	}
	kept := make([]Profile, 0, len(u.Profiles))
	for _, p := range u.Profiles {
		if builtinIDs[p.ID] {
			continue
		}
		kept = append(kept, p)
	}
	u.Profiles = kept
	if err := s.writeUser(u); err != nil {
		return nil, err
	}
	c, _ := s.loadLocked()
	return c, nil
}

// Save 全量保存用户区。传入的是合并视图，内部只挑出属于用户的部分落盘。
func (s *Store) Save(c *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(c)
}

func (s *Store) saveLocked(c *Config) error {
	b, err := s.readBuiltin()
	if err != nil {
		b = &builtinDoc{Profiles: DefaultPresets()}
	}
	u, err := s.readUser()
	if err != nil {
		u = &userDoc{}
	}
	u.Profiles = extractUserProfiles(c.Profiles, b)
	u.SelectedProfileID = c.SelectedProfileID
	// 用户重新加回来的分组从隐藏列表里移除
	if len(u.Hidden) > 0 {
		alive := make(map[string]bool, len(c.Profiles))
		for _, p := range c.Profiles {
			alive[p.ID] = true
		}
		kept := u.Hidden[:0]
		for _, id := range u.Hidden {
			if !alive[id] {
				kept = append(kept, id)
			}
		}
		u.Hidden = kept
	}
	return s.writeUser(u)
}

// Delete 删除配置集。默认区的分组删掉即隐藏（否则下次启动又冒出来），
// 自建的直接从用户区移除。两种都是「一次删除、彻底消失」。
func (s *Store) Delete(ids ...string) (*Config, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			drop[id] = true
		}
	}
	if len(drop) == 0 {
		c, _ := s.loadLocked()
		return c, 0, nil
	}

	b, err := s.readBuiltin()
	if err != nil {
		b = &builtinDoc{Profiles: DefaultPresets()}
	}
	u, err := s.readUser()
	if err != nil {
		u = &userDoc{}
	}

	gone := make(map[string]bool, len(drop))
	kept := make([]Profile, 0, len(u.Profiles))
	keptIDs := make(map[string]bool, len(u.Profiles))
	for _, p := range u.Profiles {
		if drop[p.ID] {
			gone[p.ID] = true
			continue
		}
		kept = append(kept, p)
		keptIDs[p.ID] = true
	}
	u.Profiles = kept

	// 默认区里存在、用户区里没有对应副本 → 记进 hidden
	known := make(map[string]bool, len(u.Hidden))
	for _, id := range u.Hidden {
		known[id] = true
	}
	for _, p := range b.Profiles {
		if drop[p.ID] && !keptIDs[p.ID] {
			gone[p.ID] = true
			if !known[p.ID] {
				u.Hidden = append(u.Hidden, p.ID)
				known[p.ID] = true
			}
		}
	}

	if drop[u.SelectedProfileID] {
		u.SelectedProfileID = ""
	}
	if err := s.writeUser(u); err != nil {
		return nil, 0, err
	}
	c, _ := s.loadLocked()
	if c.SelectedProfileID == "" && len(c.Profiles) > 0 {
		c.SelectedProfileID = c.Profiles[0].ID
	}
	return c, len(gone), nil
}

// ---------------------------------------------------------------------------
// 兼容旧调用
// ---------------------------------------------------------------------------

func loadConfigFile(p string) (*Config, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("空文件")
	}
	return ParseConfig(b)
}

func emptyConfig() *Config {
	return &Config{Format: ConfigFormat, Version: ConfigVersion, Profiles: []Profile{}}
}
