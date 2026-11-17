package main

// 随附云端清单的格式门禁。
//
// cloud/sources.json 是客户端真正会拉取的那份清单的源文件，改坏一个逗号、
// 漏一条 URL、把 id 写重，客户端都会「整份拒收」并回落到 builtin.json ——
// 表现只是「源变旧了」，很难从界面上看出来。这个用例把校验前移到 CI。
//
// 与其他用例不同，它读磁盘（仓库内的 cloud/sources.json），不联网。

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestShippedManifestValid(t *testing.T) {
	raw, err := os.ReadFile("../cloud/sources.json")
	if err != nil {
		t.Fatalf("读取 cloud/sources.json 失败: %v", err)
	}
	if len(raw) > cloudMaxBody {
		t.Fatalf("清单 %d 字节，超过 %d 上限", len(raw), cloudMaxBody)
	}

	var doc cloudDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("清单不是合法 JSON: %v", err)
	}
	if doc.Format != cloudFormat {
		t.Fatalf("format = %q，期望 %q", doc.Format, cloudFormat)
	}
	if doc.Version <= 0 || doc.Version > cloudVersion {
		t.Fatalf("version = %d，本程序最高 %d", doc.Version, cloudVersion)
	}
	if strings.TrimSpace(doc.Rev) == "" {
		t.Fatal("rev 为空：客户端靠它判断要不要换缓存")
	}

	ps, err := sanitizeCloudProfiles(doc.Profiles)
	if err != nil {
		t.Fatalf("清单未通过 sanitize: %v", err)
	}
	if len(ps) != len(doc.Profiles) {
		t.Fatalf("校验后 %d 条，原 %d 条", len(ps), len(doc.Profiles))
	}

	// 分组语义：上传组必须有上传端点，国内/国外组必须有下载端点。
	// 写错分组不会报错，只会让界面上的选项点下去什么都不跑。
	for _, p := range ps {
		switch p.Group {
		case "上传":
			if len(p.UploadEndpoints) == 0 {
				t.Errorf("%s 归在「上传」组但没有上传端点", p.ID)
			}
		case "国内", "国外":
			if len(p.DownloadEndpoints) == 0 {
				t.Errorf("%s 归在「%s」组但没有下载端点", p.ID, p.Group)
			}
		}
	}

	// 端点总数是 UI 上「N 条」的来源，也是并发上限的比较基准；
	// 0 条说明清单被清空了，单条超过上限则会在启动时被拒。
	total := 0
	for _, p := range ps {
		n := len(p.DownloadEndpoints) + len(p.UploadEndpoints)
		if n > maxEndpoints {
			t.Errorf("%s 有 %d 个端点，超过 %d", p.ID, n, maxEndpoints)
		}
		total += n
	}
	if total == 0 {
		t.Fatal("清单里没有任何端点")
	}
	t.Logf("清单 %s：%d 个配置集，%d 个端点", doc.Rev, len(ps), total)
}
