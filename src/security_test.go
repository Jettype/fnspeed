package main

// 输入校验 / 边界条件 / 资源回收的回归用例。
// 这些函数不起网络、不碰平台 syscall，改动它们时先看这里是不是会红。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckEndpointURL(t *testing.T) {
	good := []string{
		"http://127.0.0.1:8080/dl",
		"https://speed.cloudflare.com/__down?bytes=1000",
		"https://dlied1.qq.com/a.apk?sig=abc",
		"HTTPS://Example.COM/x",
	}
	for _, u := range good {
		if err := checkEndpointURL(u); err != nil {
			t.Fatalf("%q 应通过，实际被拒: %v", u, err)
		}
	}

	bad := []struct{ url, want string }{
		{"", "为空"},
		{"file:///etc/passwd", "只支持 http/https"},
		{"ftp://host/path", "只支持 http/https"},
		{"gopher://host/", "只支持 http/https"},
		{"//example.com/x", "缺少 http://"},
		{"example.com/x", "缺少 http://"},
		{"http://", "缺少主机名"},
		{"https://" + strings.Repeat("a", maxEndpointURLLen), "超过"},
	}
	for _, c := range bad {
		err := checkEndpointURL(c.url)
		if err == nil {
			t.Fatalf("%q 应被拒绝，实际通过", c.url)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%q 的错误信息应含 %q，实际 %q", c.url, c.want, err.Error())
		}
	}
}

func TestFirstBadEndpoint(t *testing.T) {
	if bad, _ := firstBadEndpoint(nil); bad != "" {
		t.Fatalf("nil profile 应返回空，实际 %q", bad)
	}
	p := &Profile{
		DownloadEndpoints: []Endpoint{{URL: "https://ok.example/a"}},
		UploadEndpoints:   []Endpoint{{URL: "ftp://bad.example/up"}},
	}
	bad, why := firstBadEndpoint(p)
	if bad != "ftp://bad.example/up" {
		t.Fatalf("应指出上传端点，实际 %q (%s)", bad, why)
	}
}

func TestSafeHeader(t *testing.T) {
	// 值里的 CRLF 必须被压平，否则能在请求里插一条新的
	name, val, ok := safeHeader("X-Test", "a\r\nX-Injected: 1")
	if !ok {
		t.Fatal("合法头名不该被丢弃")
	}
	if name != "X-Test" {
		t.Fatalf("头名被改动: %q", name)
	}
	if strings.ContainsAny(val, "\r\n") {
		t.Fatalf("值里仍有换行: %q", val)
	}

	// 头名带冒号能把整行改写成任意头部
	for _, k := range []string{"X-A: 1", "X-B\r\n", "", "   ", "X-C\n"} {
		if _, _, ok := safeHeader(k, "v"); ok {
			t.Fatalf("头名 %q 应被丢弃", k)
		}
	}
}

func TestTickNeverReportsNegativeRate(t *testing.T) {
	s := NewStats()
	s.SetStartAt(time.Now())
	s.Running.Store(true)
	// 先攒一轮正数，再模拟「换局清零」：计数归零、上一个 Tick 的 lastRec 还在
	s.RecvBytes.Store(8 << 20)
	s.SentBytes.Store(4 << 20)
	s.Tick()
	s.RecvBytes.Store(0)
	s.SentBytes.Store(0)
	s.Tick()

	if rr := s.lastRR.Load(); rr < 0 {
		t.Fatalf("下行瞬时速率不应为负，实际 %d", rr)
	}
	if rs := s.lastRS.Load(); rs < 0 {
		t.Fatalf("上行瞬时速率不应为负，实际 %d", rs)
	}
}

func TestTrimHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString(`{"at":`)
		b.WriteString(itoa(i))
		b.WriteString("}\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if err := trimHistory(path, 10); err != nil {
		t.Fatalf("trimHistory 失败: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 10 {
		t.Fatalf("应保留 10 行，实际 %d", len(lines))
	}
	if lines[0] != `{"at":40}` {
		t.Fatalf("应保留最后 10 行，首行是 %q", lines[0])
	}
}
