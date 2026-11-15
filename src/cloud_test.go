package main

// 云配置的纯逻辑测试。不联网，不碰磁盘。
//
// 本机（Windows）跑不了这个包的单测：engine.go 的 syscall 签名在 Windows
// 与 Linux 不同，整个包编不过测试二进制。这些用例由 CI 的 ubuntu runner 执行，
// 本地最低验证是 GOOS=linux go vet ./... 加双架构 go build。

import (
	"strings"
	"testing"
	"time"
)

func mkProfile(id string, dl ...string) Profile {
	p := Profile{ID: id, Name: id, Mode: "down", DownloadURLs: dl}
	normalizeProfile(&p)
	return p
}

func TestComposeBaseReplace(t *testing.T) {
	cloud := []Profile{mkProfile("only-cloud", "https://cdn.example.com/a.bin")}
	out := composeBase("replace", cloud)
	if len(out) != 1 || out[0].ID != "only-cloud" {
		t.Fatalf("replace 模式应只保留云端清单，得到 %d 条", len(out))
	}
	// 出厂清单必须一条都不出现：云端下架一个源要立刻生效，
	// 不能被出厂清单「接住」复活。
	for _, p := range out {
		if p.ID == "cn-tencent" {
			t.Fatal("replace 模式下出厂分组不应出现")
		}
	}
}

func TestComposeBaseAppend(t *testing.T) {
	cloud := []Profile{
		mkProfile("cloud-new", "https://cdn.example.com/new.bin"),
		mkProfile("cn-tencent", "https://cdn.example.com/override.bin"),
	}
	out := composeBase("append", cloud)
	if len(out) != len(DefaultPresets())+1 {
		t.Fatalf("append 应等于出厂条数 + 1 条新增，实际 %d", len(out))
	}
	var tencent *Profile
	for i := range out {
		if out[i].ID == "cn-tencent" {
			tencent = &out[i]
		}
	}
	if tencent == nil {
		t.Fatal("append 模式应保留出厂分组")
	}
	if got := tencent.DownloadEndpoints[0].URL; got != "https://cdn.example.com/override.bin" {
		t.Fatalf("同 ID 应以云端为准，实际 %q", got)
	}
}

func TestComposeBaseEmpty(t *testing.T) {
	if out := composeBase("replace", nil); out != nil {
		t.Fatal("空云端清单应返回 nil，交回降级链")
	}
}

func TestSanitizeCloudProfiles(t *testing.T) {
	cases := []struct {
		name    string
		in      []Profile
		wantOK  bool
		wantSub string
	}{
		{
			name:   "正常",
			in:     []Profile{mkProfile("a", "https://cdn.example.com/a.bin")},
			wantOK: true,
		},
		{
			name:   "空清单",
			in:     nil,
			wantOK: false,
		},
		{
			name: "缺 id",
			in: []Profile{{
				Name: "x", DownloadEndpoints: []Endpoint{{URL: "https://cdn.example.com/a.bin"}},
			}},
			wantOK:  false,
			wantSub: "id",
		},
		{
			name: "id 重复",
			in: []Profile{
				mkProfile("dup", "https://cdn.example.com/a.bin"),
				mkProfile("dup", "https://cdn.example.com/b.bin"),
			},
			wantOK:  false,
			wantSub: "重复",
		},
		{
			name:    "没有端点",
			in:      []Profile{{ID: "empty", Name: "空"}},
			wantOK:  false,
			wantSub: "没有端点",
		},
		{
			name:    "file 协议",
			in:      []Profile{mkProfile("bad", "file:///etc/passwd")},
			wantOK:  false,
			wantSub: "不合法",
		},
		{
			name:    "缺主机名",
			in:      []Profile{mkProfile("bad", "https:///nohost")},
			wantOK:  false,
			wantSub: "不合法",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := sanitizeCloudProfiles(c.in)
			if c.wantOK {
				if err != nil {
					t.Fatalf("应通过，实际报错: %v", err)
				}
				if len(out) != len(c.in) {
					t.Fatalf("条数对不上: %d", len(out))
				}
				// normalizeProfile 必须被调用过：下载端点要补齐 method
				for _, p := range out {
					for _, ep := range p.DownloadEndpoints {
						if ep.Method == "" {
							t.Fatal("端点未规范化（method 为空）")
						}
					}
				}
				return
			}
			if err == nil {
				t.Fatal("应被拒绝，实际通过")
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("错误信息应包含 %q，实际 %q", c.wantSub, err.Error())
			}
		})
	}
}

func TestSanitizeTruncatesLongText(t *testing.T) {
	p := mkProfile("long", "https://cdn.example.com/a.bin")
	p.Name = strings.Repeat("名", 200)
	p.Desc = strings.Repeat("述", 400)
	out, err := sanitizeCloudProfiles([]Profile{p})
	if err != nil {
		t.Fatalf("应通过: %v", err)
	}
	if n := len([]rune(out[0].Name)); n > 120 {
		t.Fatalf("name 应截断到 120 字以内，实际 %d", n)
	}
	if n := len([]rune(out[0].Desc)); n > 300 {
		t.Fatalf("desc 应截断到 300 字以内，实际 %d", n)
	}
}

func TestCloudSign(t *testing.T) {
	doc := &cloudDoc{
		Format:   cloudFormat,
		Version:  cloudVersion,
		Rev:      "2026-11-09.1",
		Profiles: []Profile{mkProfile("a", "https://cdn.example.com/a.bin")},
	}
	sig := CloudSign("s3cret", doc)
	if sig == "" {
		t.Fatal("签名不应为空")
	}
	if sig != CloudSign("s3cret", doc) {
		t.Fatal("同一份内容两次签名必须一致")
	}
	if sig == CloudSign("other", doc) {
		t.Fatal("不同密钥不应产生相同签名")
	}

	m := &CloudManager{}
	m.SetSecret("s3cret")
	doc.Signature = sig
	if err := m.verifySignature(doc); err != nil {
		t.Fatalf("正确签名应通过: %v", err)
	}
	// 允许带算法前缀
	doc.Signature = "hmac-sha256=" + sig
	if err := m.verifySignature(doc); err != nil {
		t.Fatalf("带前缀的签名应通过: %v", err)
	}

	// 改掉源地址 → 签名失效
	doc.Signature = sig
	doc.Profiles[0].DownloadEndpoints[0].URL = "https://evil.example.com/x.bin"
	if err := m.verifySignature(doc); err == nil {
		t.Fatal("内容被篡改后必须拒绝")
	}

	// 改 rev 同样要失效：rev 参与签名
	doc2 := &cloudDoc{
		Format: cloudFormat, Version: cloudVersion, Rev: "2026-11-09.1",
		Profiles:  []Profile{mkProfile("a", "https://cdn.example.com/a.bin")},
		Signature: sig,
	}
	doc2.Rev = "2026-11-10.1"
	if err := m.verifySignature(doc2); err == nil {
		t.Fatal("rev 被改动后必须拒绝")
	}

	// 未配密钥 → 跳过校验，不报错
	m2 := &CloudManager{}
	if err := m2.verifySignature(doc); err != nil {
		t.Fatalf("未配密钥时应跳过: %v", err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		cur, min string
		want     bool
	}{
		{"1.6.0-alpha", "1.6.0-alpha", true},
		{"1.6.0-alpha", "1.5.0", true},
		{"1.6.0", "1.5.9", true},
		{"1.6.0", "1.6.0", true},
		{"1.6.0-alpha", "1.6.0", false}, // alpha 低于同号正式版
		{"1.5.9", "1.6.0", false},
		{"2.0.0", "1.99.99", true},
		{"1.6.0", "", true},
		{"", "1.0.0", false},
		{"1.6.1-beta", "1.6.1-alpha", true},
	}
	for _, c := range cases {
		if got := versionAtLeast(c.cur, c.min); got != c.want {
			t.Errorf("versionAtLeast(%q,%q)=%v，期望 %v", c.cur, c.min, got, c.want)
		}
	}
}

func TestClampTTL(t *testing.T) {
	if d := clampTTL(0); d != cloudMinTTL {
		t.Fatalf("0 应抬到下限，实际 %v", d)
	}
	if d := clampTTL(48 * time.Hour); d != cloudMaxTTL {
		t.Fatalf("过长应压到上限，实际 %v", d)
	}
	if d := clampTTL(time.Hour); d != time.Hour {
		t.Fatalf("区间内应原样返回，实际 %v", d)
	}
}

func TestCloudEndpointOf(t *testing.T) {
	if got := cloudEndpointOf(GlobalSettings{CloudDisabled: true}); got != "" {
		t.Fatalf("关闭时应返回空串，实际 %q", got)
	}
	if got := cloudEndpointOf(GlobalSettings{}); got != defaultCloudEndpoint {
		t.Fatalf("未配置应回落编译期默认值，实际 %q", got)
	}
	g := GlobalSettings{CloudEndpoint: "  https://cfg.example.com/s.json  "}
	if got := cloudEndpointOf(g); got != "https://cfg.example.com/s.json" {
		t.Fatalf("应去掉首尾空白，实际 %q", got)
	}
}

func TestCheckCloudEndpointScheme(t *testing.T) {
	m := &CloudManager{}
	if err := m.checkCloudEndpoint("http://cfg.example.com/s.json"); err == nil {
		t.Fatal("默认应拒绝明文 http")
	}
	m.SetAllowHTTP(true)
	if err := m.checkCloudEndpoint("http://cfg.example.com/s.json"); err != nil {
		t.Fatalf("显式放行后应通过: %v", err)
	}
	if err := m.checkCloudEndpoint("ftp://cfg.example.com/s.json"); err == nil {
		t.Fatal("只支持 http/https")
	}
	if err := m.checkCloudEndpoint("https:///nohost"); err == nil {
		t.Fatal("缺主机名应拒绝")
	}
	if err := m.checkCloudEndpoint("https://cfg.example.com/s.json"); err != nil {
		t.Fatalf("https 应通过: %v", err)
	}
}
