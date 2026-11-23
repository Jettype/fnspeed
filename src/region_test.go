package main

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestRegionNodesIntegrity(t *testing.T) {
	if len(regionNodes) < 60 {
		t.Fatalf("可选位置太少：%d 条，至少应有 60", len(regionNodes))
	}
	seen := map[string]bool{}
	for _, n := range regionNodes {
		if seen[n.ID] {
			t.Errorf("ID 重复：%s", n.ID)
		}
		seen[n.ID] = true
		if n.Prov == "" || n.ISP == "" {
			t.Errorf("%s 缺省份或运营商", n.ID)
		}
		ip := net.ParseIP(n.ECS)
		if ip == nil || ip.To4() == nil {
			t.Errorf("%s 的 ECS 不是合法 IPv4：%q", n.ID, n.ECS)
		}
		if ip.IsLoopback() || ip.IsPrivate() {
			t.Errorf("%s 的 ECS 是内网地址，CDN 认不出来：%s", n.ID, n.ECS)
		}
	}
	if regionIndex["gd-ct"].Prov != "广东" {
		t.Errorf("索引没建对：%+v", regionIndex["gd-ct"])
	}
}

func TestRegionLabel(t *testing.T) {
	cases := map[string]string{
		"":      "",
		"auto":  "",
		"gd-ct": "广东 · 电信",
		"zz-xx": "",
	}
	for in, want := range cases {
		if got := RegionLabel(in); got != want {
			t.Errorf("RegionLabel(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func TestRegionOptionsGrouped(t *testing.T) {
	opts := RegionOptions()
	if len(opts) < 20 {
		t.Fatalf("省份分组太少：%d", len(opts))
	}
	total := 0
	for _, o := range opts {
		items, _ := o["items"].([]map[string]string)
		if len(items) == 0 {
			t.Errorf("省份 %v 没有条目", o["prov"])
		}
		total += len(items)
	}
	if total != len(regionNodes) {
		t.Errorf("分组总数 %d 与表长度 %d 不一致", total, len(regionNodes))
	}
}

func TestNormalizeSettingsRegion(t *testing.T) {
	g := GlobalSettings{Concurrency: 32, DurationSec: 60, Region: "auto"}
	normalizeSettings(&g)
	if g.Region != "" {
		t.Errorf("auto 应归一化成空串，实际 %q", g.Region)
	}

	g = GlobalSettings{Region: "gd-ct"}
	normalizeSettings(&g)
	if g.Region != "gd-ct" {
		t.Errorf("合法位置被抹掉了：%q", g.Region)
	}

	g = GlobalSettings{Region: "mars-cm"}
	normalizeSettings(&g)
	if g.Region != "" {
		t.Errorf("非法位置应回落就地，实际 %q", g.Region)
	}
}

// TestOverseasRegions 锁住「海外节点」这一层：
// 少一条只是选项少了，但 id 写错或 Group 漏填会让 IsOverseasRegion 判错，
// 进而让「海外优先」的起始并发与提示都不生效 —— 这种错静默且难查。
func TestOverseasRegions(t *testing.T) {
	n := 0
	provinces := map[string]bool{}
	for _, r := range regionNodes {
		if r.Group == "overseas" {
			n++
			provinces[r.Prov] = true
			if net.ParseIP(r.ECS) == nil {
				t.Errorf("%s 的 ECS 不合法", r.ID)
			}
		}
	}
	if n < 10 {
		t.Fatalf("海外节点只有 %d 条，至少应有 10", n)
	}
	if len(provinces) < 8 {
		t.Fatalf("海外只覆盖 %d 个地区，至少要 8 个", len(provinces))
	}
	if !IsOverseasRegion("intl-jp-tyo") {
		t.Error("东京节点应被判定为海外")
	}
	if IsOverseasRegion("gd-ct") {
		t.Error("广东电信是境内节点")
	}
	if IsOverseasRegion("") || IsOverseasRegion("nope") {
		t.Error("空值与未知 id 都应为 false")
	}
}

// 前端靠 overseas 标记给分组加「海外 ·」前缀，漏了就只能靠省份名猜。
func TestRegionOptionsMarksOverseas(t *testing.T) {
	found := false
	for _, o := range RegionOptions() {
		if o["prov"] == "日本" {
			found = true
			if v, _ := o["overseas"].(bool); !v {
				t.Error("日本分组应标记为海外")
			}
		}
	}
	if !found {
		t.Fatal("没有日本分组")
	}
}

func TestNormalizeSettingsPrefer(t *testing.T) {
	g := GlobalSettings{Prefer: "overseas"}
	normalizeSettings(&g)
	if g.Prefer != "overseas" {
		t.Errorf("合法偏好被抹掉了：%q", g.Prefer)
	}
	for _, bad := range []string{"auto", "intl", "海外", "OVERSEAS"} {
		g = GlobalSettings{Prefer: bad}
		normalizeSettings(&g)
		if g.Prefer != "" {
			t.Errorf("非法偏好 %q 应回落就近，实际 %q", bad, g.Prefer)
		}
	}
}

// TestPinResolveLive 会真的发 DoH 查询，默认跳过（CI 无外网时不该红）。
// 手动跑：go test -run TestPinResolveLive -v
func TestPinResolveLive(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过联网测试")
	}
	r := NewPinResolver()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	host := "sf3-cn.feishucdn.com"
	gd, err := r.Resolve(ctx, host, "gd-ct")
	if err != nil {
		t.Fatalf("广东电信解析失败：%v", err)
	}
	bj, err := r.Resolve(ctx, host, "bj-cu")
	if err != nil {
		t.Fatalf("北京联通解析失败：%v", err)
	}
	t.Logf("%s → 广东电信 %s / 北京联通 %s", host, gd, bj)
	if gd == bj {
		t.Logf("警告：两地解析结果相同，该源可能不支持 ECS（这不一定是 bug）")
	}
	if net.ParseIP(gd) == nil || net.ParseIP(bj) == nil {
		t.Fatalf("解析结果不是 IP：%q %q", gd, bj)
	}

	// 第二次应命中缓存（不再发请求），只验证不报错。
	if again, err := r.Resolve(ctx, host, "gd-ct"); err != nil || again != gd {
		t.Fatalf("缓存复用异常：%q %v", again, err)
	}
}
