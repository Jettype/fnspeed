package main

// Store 双文件模型的回归测试。
// 这几个用例锁的是 1.5.1 的核心承诺：
// 「默认区每次安装整体覆盖，用户区永不触碰，删掉的默认分组不会被塞回来」。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	return NewStore(dir), dir
}

func readUserDoc(t *testing.T, dir string) *userDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, userFileName))
	if err != nil {
		t.Fatalf("读 user.json 失败: %v", err)
	}
	var d userDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("解析 user.json 失败: %v", err)
	}
	return &d
}

func findProfile(c *Config, id string) *Profile {
	for i := range c.Profiles {
		if c.Profiles[i].ID == id {
			return &c.Profiles[i]
		}
	}
	return nil
}

// 首次安装：两个文件都要生成，配置集数量等于出厂预设。
func TestFirstInstall(t *testing.T) {
	s, dir := newTestStore(t)
	cfg, wrote, _, err := s.EnsureSeeded()
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("首次安装应该写盘")
	}
	if len(cfg.Profiles) != len(DefaultPresets()) {
		t.Fatalf("配置集数量 %d，期望 %d", len(cfg.Profiles), len(DefaultPresets()))
	}
	for _, f := range []string{builtinFileName, userFileName} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s 未生成: %v", f, err)
		}
	}
	if cfg.SelectedProfileID != DefaultPresets()[0].ID {
		t.Fatalf("选中项 %q，期望第一条", cfg.SelectedProfileID)
	}
}

// 迁移：旧的 profiles.json 整体丢弃并改名备份，垃圾分组一条都不能留下。
func TestMigrateLegacyDropsEverything(t *testing.T) {
	s, dir := newTestStore(t)

	legacy := &Config{
		Format: ConfigFormat, Version: ConfigVersion, SelectedProfileID: "old-1",
		Profiles: []Profile{
			{ID: "old-1", Name: "小文件", DownloadURLs: []string{"https://example.com/a"}},
			{ID: "old-2", Name: "手机厂", DownloadURLs: []string{"https://example.com/b"}},
			{ID: "old-3", Name: "电商广告", DownloadURLs: []string{"https://example.com/c"}},
		},
	}
	b, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, legacyFileName), b, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, _, note, err := s.EnsureSeeded()
	if err != nil {
		t.Fatal(err)
	}
	if findProfile(cfg, "old-1") != nil {
		t.Fatal("旧配置集 old-1 不应出现在新视图里")
	}
	if len(cfg.Profiles) != len(DefaultPresets()) {
		t.Fatalf("迁移后配置集 %d 个，期望只有出厂预设 %d 个", len(cfg.Profiles), len(DefaultPresets()))
	}
	if note == "" {
		t.Fatal("迁移应给出提示")
	}
	if _, err := os.Stat(filepath.Join(dir, legacyFileName+".legacy.bak")); err != nil {
		t.Fatal("旧文件应改名为 .legacy.bak 留档")
	}
	if _, err := os.Stat(filepath.Join(dir, legacyFileName)); !os.IsNotExist(err) {
		t.Fatal("旧 profiles.json 应已被改名")
	}
}

// 改默认分组 → 自动 fork 进用户区，重新加载后改动仍在。
func TestEditBuiltinForksToUser(t *testing.T) {
	s, dir := newTestStore(t)
	cfg, _, _, err := s.EnsureSeeded()
	if err != nil {
		t.Fatal(err)
	}
	target := DefaultPresets()[0].ID
	p := findProfile(cfg, target)
	if p == nil {
		t.Fatalf("找不到 %s", target)
	}
	p.Name = "我改过的源"
	p.DownloadURLs = []string{"https://mirror.example.com/1gb.bin"}

	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}

	u := readUserDoc(t, dir)
	var forked *Profile
	for i := range u.Profiles {
		if u.Profiles[i].ID == target {
			forked = &u.Profiles[i]
		}
	}
	if forked == nil {
		t.Fatal("改过的默认分组应写入 user.json")
	}
	if forked.ForkedFrom != target {
		t.Fatalf("forkedFrom = %q，期望 %s", forked.ForkedFrom, target)
	}
	// 未改动的分组不应被复制进用户区
	if len(u.Profiles) != 1 {
		t.Fatalf("user.json 里应有 1 条增量，实际 %d 条", len(u.Profiles))
	}

	reloaded, _ := s.Load()
	if got := findProfile(reloaded, target); got == nil || got.Name != "我改过的源" {
		t.Fatal("重新加载后改动丢失")
	}
}

// 默认区覆盖不能碰用户 fork 的副本。
func TestBuiltinRefreshKeepsUserFork(t *testing.T) {
	s, _ := newTestStore(t)
	cfg, _, _, err := s.EnsureSeeded()
	if err != nil {
		t.Fatal(err)
	}
	p := findProfile(cfg, "cn-tencent")
	p.Name = "改过的腾讯源"
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	// 模拟升级：让 builtin 的 hash 失配
	reloaded, _, _, err := s.EnsureSeeded()
	if err != nil {
		t.Fatal(err)
	}
	if got := findProfile(reloaded, "cn-tencent"); got == nil || got.Name != "改过的腾讯源" {
		t.Fatal("默认区刷新后用户副本应仍然生效")
	}
}

// 删掉默认分组 → 记进 hidden，重启也不会回来。
func TestDeleteBuiltinStaysDeleted(t *testing.T) {
	s, dir := newTestStore(t)
	if _, _, _, err := s.EnsureSeeded(); err != nil {
		t.Fatal(err)
	}
	// 不写死 ID：预设清单会变，测试要跟着走
	ps := DefaultPresets()
	victim := ps[len(ps)-1].ID
	cfg, removed, err := s.Delete(victim)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d，期望 1", removed)
	}
	if findProfile(cfg, victim) != nil {
		t.Fatal("删除后不应再出现")
	}
	u := readUserDoc(t, dir)
	if len(u.Hidden) != 1 || u.Hidden[0] != victim {
		t.Fatalf("hidden = %v，期望 [%s]", u.Hidden, victim)
	}
	// 重启
	again, _, _, err := s.EnsureSeeded()
	if err != nil {
		t.Fatal(err)
	}
	if findProfile(again, victim) != nil {
		t.Fatal("重启后已删除的默认分组不应复活")
	}
}

// 恢复默认：hidden 清空，删掉的分组回来。
func TestRestoreBringsBackDeleted(t *testing.T) {
	s, _ := newTestStore(t)
	if _, _, _, err := s.EnsureSeeded(); err != nil {
		t.Fatal(err)
	}
	ps := DefaultPresets()
	victim := ps[len(ps)-1].ID
	if _, _, err := s.Delete(victim); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.RestorePresets()
	if err != nil {
		t.Fatal(err)
	}
	if findProfile(cfg, victim) == nil {
		t.Fatal("恢复默认后应重新出现")
	}
	if len(cfg.Profiles) != len(DefaultPresets()) {
		t.Fatalf("恢复后 %d 条，期望 %d 条", len(cfg.Profiles), len(DefaultPresets()))
	}
}

// 选中项被删后回落第一条。
func TestSelectedFallback(t *testing.T) {
	s, _ := newTestStore(t)
	cfg, _, _, err := s.EnsureSeeded()
	if err != nil {
		t.Fatal(err)
	}
	first := cfg.SelectedProfileID
	cfg.SelectedProfileID = "cn-apple"
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Delete("cn-apple"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Load()
	if after.SelectedProfileID == "" && len(after.Profiles) > 0 {
		t.Fatal("选中项被删后应回落到第一条，而不是空")
	}
	if after.SelectedProfileID == "cn-apple" {
		t.Fatal("选中项不应指向已删除的分组")
	}
	_ = first
}

// builtin hash 与实际内容不符时要触发整体覆盖。
func TestBuiltinHashMismatchRewrites(t *testing.T) {
	s, dir := newTestStore(t)
	if _, _, _, err := s.EnsureSeeded(); err != nil {
		t.Fatal(err)
	}
	// 把 builtin 改成只有一条，hash 留旧值 → 下次启动应重写
	bogus := &builtinDoc{
		Format: ConfigFormat, Version: ConfigVersion, Hash: "sha256:bogus",
		Profiles: []Profile{{ID: "only-one", Name: "残留"}},
	}
	b, _ := json.MarshalIndent(bogus, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, builtinFileName), b, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, _, _, err := s.EnsureSeeded()
	if err != nil {
		t.Fatal(err)
	}
	if findProfile(cfg, "only-one") != nil {
		t.Fatal("hash 不匹配时默认区应被整体覆盖")
	}
	if len(cfg.Profiles) != len(DefaultPresets()) {
		t.Fatalf("覆盖后 %d 条，期望 %d 条", len(cfg.Profiles), len(DefaultPresets()))
	}
}

// ---------------------------------------------------------------------------
// 云配置层接管默认区（1.6.0）
// ---------------------------------------------------------------------------

func mkCloudProfile(id, url string) Profile {
	p := Profile{ID: id, Name: id, Mode: "down", DownloadURLs: []string{url}}
	normalizeProfile(&p)
	return p
}

// 云端接管后：出厂分组退位，但 user.json 不因「保存一次」被灌满。
func TestCloudBaseDoesNotLeakIntoUser(t *testing.T) {
	s, dir := newTestStore(t)
	if _, _, _, err := s.EnsureSeeded(); err != nil {
		t.Fatal(err)
	}

	s.SetBase([]Profile{
		mkCloudProfile("cloud-a", "https://cdn.example.com/a.bin"),
		mkCloudProfile("cloud-b", "https://cdn.example.com/b.bin"),
	}, "r1")

	cfg, _ := s.Load()
	if len(cfg.Profiles) != 2 {
		t.Fatalf("默认区应为云端 2 条，实际 %d", len(cfg.Profiles))
	}
	if findProfile(cfg, "cn-tencent") != nil {
		t.Fatal("云端接管后出厂分组不该再出现")
	}

	// 只改其中一条，其余原样保存
	cfg.Profiles[0].DownloadEndpoints = []Endpoint{{URL: "https://cdn.example.com/mine.bin", Method: "GET"}}
	cfg.Profiles[0].DownloadURLs = nil
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}

	u := readUserDoc(t, dir)
	if len(u.Profiles) != 1 {
		t.Fatalf("只有被改过的那条该落进 user.json，实际 %d 条", len(u.Profiles))
	}
	if u.Profiles[0].ID != "cloud-a" || u.Profiles[0].ForkedFrom != "cloud-a" {
		t.Fatalf("用户副本标记不对: %+v", u.Profiles[0])
	}
	for _, p := range u.Profiles {
		if p.ID == "cloud-b" {
			t.Fatal("未改动的云端分组不该被写进 user.json —— 否则云端更新会被它挡住")
		}
	}
}

// 云端发新版：用户改过的保持用户的值，没改过的跟着云端走。
func TestCloudUpdateRespectsUserEdits(t *testing.T) {
	s, _ := newTestStore(t)
	if _, _, _, err := s.EnsureSeeded(); err != nil {
		t.Fatal(err)
	}
	s.SetBase([]Profile{
		mkCloudProfile("cloud-a", "https://cdn.example.com/a.bin"),
		mkCloudProfile("cloud-b", "https://cdn.example.com/b.bin"),
	}, "r1")

	cfg, _ := s.Load()
	cfg.Profiles[0].DownloadEndpoints = []Endpoint{{URL: "https://cdn.example.com/mine.bin", Method: "GET"}}
	cfg.Profiles[0].DownloadURLs = nil
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}

	s.SetBase([]Profile{
		mkCloudProfile("cloud-a", "https://cdn.example.com/a-v2.bin"),
		mkCloudProfile("cloud-b", "https://cdn.example.com/b-v2.bin"),
	}, "r2")
	cfg, _ = s.Load()

	a := findProfile(cfg, "cloud-a")
	if a == nil || a.DownloadEndpoints[0].URL != "https://cdn.example.com/mine.bin" {
		t.Fatalf("用户改过的 cloud-a 应保留自己的地址，实际 %+v", a)
	}
	b := findProfile(cfg, "cloud-b")
	if b == nil || b.DownloadEndpoints[0].URL != "https://cdn.example.com/b-v2.bin" {
		t.Fatalf("没改过的 cloud-b 应跟随云端新值，实际 %+v", b)
	}
}

// 云端下架一个分组：它必须真的消失，不能被出厂清单接住复活。
func TestCloudRemovalTakesEffect(t *testing.T) {
	s, _ := newTestStore(t)
	if _, _, _, err := s.EnsureSeeded(); err != nil {
		t.Fatal(err)
	}
	s.SetBase([]Profile{
		mkCloudProfile("cloud-a", "https://cdn.example.com/a.bin"),
		mkCloudProfile("cloud-b", "https://cdn.example.com/b.bin"),
	}, "r1")
	if _, _, err := s.Delete("cloud-b"); err != nil {
		t.Fatal(err)
	}

	cfg, _ := s.Load()
	if findProfile(cfg, "cloud-b") != nil {
		t.Fatal("删掉的分组不该复活")
	}
	if len(cfg.Profiles) != 1 {
		t.Fatalf("应剩 1 条，实际 %d", len(cfg.Profiles))
	}
}

// 云端接管期间不碰 builtin.json：那份文件必须一直是出厂清单，
// 否则云端永久不可用时用户退不回去。
func TestCloudBaseKeepsBuiltinFile(t *testing.T) {
	s, dir := newTestStore(t)
	if _, _, _, err := s.EnsureSeeded(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, builtinFileName))
	if err != nil {
		t.Fatal(err)
	}
	s.SetBase([]Profile{mkCloudProfile("cloud-a", "https://cdn.example.com/a.bin")}, "r1")
	if _, _, _, err := s.EnsureSeeded(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, builtinFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("云端接管期间 builtin.json 不该被改写")
	}
}
