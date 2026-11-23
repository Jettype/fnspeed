package main

// 内置测速源。选源原则：
//
//	国内 —— 软件厂商自有 CDN 上的大安装包，单文件从百 MB 到数 GB，支持 Range、
//	        不掐长连接。为什么不选镜像站，见下方 1.5.2 的说明；
//	海外 —— Cloudflare / OVH / Vultr 的官方测速文件，跨境链路可复现；
//	上传 —— 只有 Cloudflare __up 与自建 sink 可用，公共站点基本不接受任意 body。
//
// 单源跑不满千兆是常态：CDN 会按单连接限速，必须靠多流并发叠加，
// 所以预设默认并发 16 并交给调度器继续爬坡。
//
// 另有两组「聚合」预设：把多个不同 CDN 的 URL 塞进同一个配置集，
// sticky 绑定 + failover 会在某个站挂掉时自动换源 —— 最不容易「连不上」的玩法。
//
// 这份清单是「出厂默认」，也是三级降级链里最低的一层：1.6.0 起默认区优先用
// 云端清单（见 cloud.go），拉不到才回落到这里。它同时写进 etc/builtin.json，
// 每次启动按内容 hash 比对，不同就整体覆盖 —— 不存在「删了又被塞回来」的问题。
// 用户改过的分组自动落到 etc/user.json，升级不会覆盖。

const (
	presetConcurrency = 16
	presetDurationSec = 60
)

// 单文件源（2026-11 实测，速率取沙箱出口，仅供参考）
//
// 1.5.2 起国内源全部换成「软件厂商自有 CDN 上的大安装包」。
// 原因：高校镜像站与云厂商镜像站对本应用几乎全线不可用 ——
// 华为云 418 风控、清华/中科大/北外/上交 403、南大重定向死循环、
// 网易 503、阿里 302 到不存在的路径。这些站点为「浏览器下载」设计，
// WAF 会把并发长连接当成攻击。而厂商分发安装包的 CDN 本来就是给
// 百万用户同时拉几 GB 文件用的，不拦、不限速、支持 Range。
const (
	// 腾讯 CDN：dlied4 走腾讯云 COS，dldir1 是腾讯自建分发
	uTencentGame = "https://dlied4.myapp.com/myapp/1104466820/cos.release-40109/10040714_com.tencent.tmgp.sgame_a2480356_8.2.1.9_F0BvnI.apk" // 1968MB · 15.8MB/s
	uWeChat      = "https://dldir1.qq.com/weixin/Windows/WeChatSetup.exe"                                                                    // 270MB · 3.1MB/s
	uWeChatX86   = "https://dldir1.qq.com/weixin/Windows/WeChatSetup_x86.exe"                                                                // 175MB · 3.1MB/s
	// 企业微信：523MB。路径带版本号，官方发新版后需换名（dldir1 同域，不另建 CDN）
	uWeCom = "https://dldir1.qq.com/wework/work_weixin/WeCom_4.1.36.6004.exe" // 523MB · 12.1MB/s

	// 阿里 CDN：npmmirror（淘宝 npm 镜像）走阿里云 CDN，是国内最好用的大文件海外/国内双通源
	uNpmNode18 = "https://registry.npmmirror.com/-/binary/node/v18.20.4/node-v18.20.4.tar.gz"                      // 81MB · 10.1MB/s
	uNpmNode22 = "https://registry.npmmirror.com/-/binary/node/v22.11.0/node-v22.11.0.tar.gz"                      // 92MB · 5.7MB/s
	uNpmElec22 = "https://registry.npmmirror.com/-/binary/electron/v22.0.0/electron-v22.0.0-linux-x64.zip"         // 89MB · 8.1MB/s
	uNpmElec28 = "https://registry.npmmirror.com/-/binary/electron/v28.0.0/electron-v28.0.0-win32-x64.zip"         // 111MB · 9.7MB/s
	uNpmElec33 = "https://registry.npmmirror.com/-/binary/electron/v33.0.0/electron-v33.0.0-win32-x64.zip"         // 108MB · 7.7MB/s
	uNpmChrome = "https://registry.npmmirror.com/-/binary/chrome-for-testing/131.0.6778.85/win64/chrome-win64.zip" // 157MB · 7.8MB/s

	// 迅雷 CDN
	uXunlei     = "https://down.sandai.net/thunder11/XunLeiSetup12.0.6.2440.exe" // 172MB · 14.1MB/s
	uXunleiMini = "https://down.sandai.net/thunder11/XunLeiWebSetup.exe"         // 13MB · 11.7MB/s

	// 苹果：域名是 updates. 不是 updates-http.，旧地址已失效
	uApple = "https://updates.cdn-apple.com/2019WinterFCS/fullrestores/041-39257/32129B6C-292C-11E9-9E72-4511412B0A59/iPhone_4.7_12.1.4_16D57_Restore.ipsw" // 2975MB · 17.9MB/s

	// 游戏厂 CDN：鹰角《明日方舟》安卓包，实际落在字节跳动 CDN（server: Byte-nginx）。
	// 入口是官方 latest 别名，302 两跳 → ak.hycdn.cn，永远指向最新包，
	// 不像米哈游/库洛那样把版本号写进路径 —— 这是它能做预设的关键。
	uArknights = "https://ak.hypergryph.com/downloads/android_lastest" // 1934MB · 302×2 · 支持 Range

	uCF      = "https://speed.cloudflare.com/__down?during=download&bytes=99999999" // 单请求上限 99,999,999 字节，≥1e8 直接 403
	uOVH     = "https://proof.ovh.net/files/100Mb.dat"
	uVultrJP = "https://hnd-jp-ping.vultr.com/vultr.com.100MB.bin"
	uVultrSG = "https://sgp-ping.vultr.com/vultr.com.100MB.bin"
	uVultrUS = "https://nj-us-ping.vultr.com/vultr.com.100MB.bin"
	uVultrDE = "https://fra-de-ping.vultr.com/vultr.com.100MB.bin"
	// 原 uCachefly（cachefly.cachefly.net/100mb.test）2026-11 复测拒连，已剔除。

	// ---- 2026-11-23 实测新增：都是「大厂自有 CDN + 国内可直连」----
	// 判据是单线程直连（不走代理）跑到 3 MB/s 以上，且必须支持 Range。
	// 这一批的意义在于换掉了「一家供应商多个镜像」的写法：
	// 之前海外组 34 条里有 23 条是 Vultr、17 条是 DataPacket，
	// 同一家抖一下整组跟着抖，看着就像「海外源全挂」。
	uKernelFastly = "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.6.tar.xz"            // 133MB · Fastly · 6.8MB/s
	uDockerCF     = "https://download.docker.com/linux/static/stable/x86_64/docker-24.0.7.tgz" // 62MB · AWS CloudFront · 5.8MB/s
	uGoGoogle     = "https://dl.google.com/go/go1.22.0.linux-amd64.tar.gz"                     // 66MB · Google · 17MB/s
	uHetznerSG    = "https://sin-speed.hetzner.com/1GB.bin"                                    // 1000MB · Hetzner 新加坡 · 3.7MB/s
	uHetznerASH   = "https://ash-speed.hetzner.com/1GB.bin"                                    // 1000MB · Hetzner 美东 · 4.2MB/s

	uCFUp   = "https://speed.cloudflare.com/__up"
	uSinkUp = "http://127.0.0.1:8977/up"
)

func presetProfile(id, group, name, desc, mode string, dl, ul []string) Profile {
	p := Profile{
		ID: id, Group: group, Name: name, Desc: desc, Mode: mode,
		DownloadURLs: dl, UploadURLs: ul,
		Concurrency: presetConcurrency, DurationSec: presetDurationSec,
		URLRotate: "sticky", SwitchPolicy: "failover",
	}
	normalizeProfile(&p)
	return p
}

// DefaultPresets 返回出厂默认配置集。
// 内容一旦改动，builtinHash 就会变，用户升级后自动拿到新版；
// 用户自己改过的分组在 user.json 里有副本，不受影响。
func DefaultPresets() []Profile {
	return []Profile{
		// ---------------- 国内 · 厂商自有 CDN（全部实测可用） ----------------
		presetProfile("cn-tencent", "国内", "腾讯 CDN · 4源", "王者荣耀 1968MB + 微信 PC/x86 + 企业微信 523MB，腾讯自建分发，不拦不限", "down",
			[]string{uTencentGame, uWeChat, uWeChatX86, uWeCom}, nil),
		presetProfile("cn-aliyun", "国内", "阿里 CDN · npmmirror 6源", "node 18/22、electron 22/28/33、Chrome for Testing 131，83–157MB，走阿里云 CDN", "down",
			[]string{uNpmNode18, uNpmNode22, uNpmElec22, uNpmElec28, uNpmElec33, uNpmChrome}, nil),
		presetProfile("cn-xunlei", "国内", "迅雷 CDN · 2源", "迅雷安装包 172MB + 13MB，自建分发节点", "down",
			[]string{uXunlei, uXunleiMini}, nil),
		presetProfile("cn-apple", "国内", "苹果 CDN", "2.9GB iOS 固件，苹果自有 CDN，境内速度最好的一条", "down",
			[]string{uApple}, nil),
		presetProfile("cn-game", "国内", "游戏 CDN · 鹰角", "1.8GB 明日方舟安卓包，走字节跳动 CDN，官方 latest 别名不会随版本失效", "down",
			[]string{uArknights}, nil),
		presetProfile("cn-multi", "国内", "国内十四源聚合 ⭐", "腾讯 4 + 阿里 6 + 迅雷 2 + 苹果 1 + 鹰角 1，任意一个挂了自动换源，最容易跑满", "down",
			[]string{uTencentGame, uWeChat, uWeChatX86, uWeCom, uNpmNode18, uNpmNode22, uNpmElec22, uNpmElec28, uNpmElec33, uNpmChrome, uXunlei, uXunleiMini, uApple, uArknights}, nil),

		// ---------------- 国外 ----------------
		presetProfile("intl-cf", "国外", "Cloudflare", "100MB 循环，全球 Anycast，最稳的跨境基准", "down",
			[]string{uCF}, nil),
		presetProfile("intl-ovh", "国外", "OVH 欧洲", "100MB 单文件，法国节点，测欧洲回程", "down",
			[]string{uOVH}, nil),
		presetProfile("intl-vultr-jp", "国外", "Vultr 东京", "100MB，日本节点，国内出海最近的一跳", "down",
			[]string{uVultrJP}, nil),
		presetProfile("intl-vultr-sg", "国外", "Vultr 新加坡", "100MB，新加坡节点，东南亚出口", "down",
			[]string{uVultrSG}, nil),
		presetProfile("intl-vultr-us", "国外", "Vultr 美东", "100MB，新泽西节点，测美东回程", "down",
			[]string{uVultrUS}, nil),
		presetProfile("intl-vultr-de", "国外", "Vultr 法兰克福", "100MB，德国节点，测欧洲回程", "down",
			[]string{uVultrDE}, nil),
		presetProfile("intl-fastly", "国外", "Fastly · kernel.org", "133MB Linux 内核源码，走 Fastly 自有骨干，实测 6.8MB/s", "down",
			[]string{uKernelFastly}, nil),
		presetProfile("intl-cloudfront", "国外", "AWS CloudFront · docker", "62MB docker 静态包，CloudFront 边缘，实测 5.8MB/s", "down",
			[]string{uDockerCF}, nil),
		presetProfile("intl-google", "国外", "Google CDN · golang", "66MB Go 发行包，Google 自有 CDN，实测 17MB/s，是这批里最快的", "down",
			[]string{uGoGoogle}, nil),
		presetProfile("intl-hetzner", "国外", "Hetzner 新加坡/美东 · 2源", "各 1GB，新加坡与阿什本节点，实测 3.7/4.2MB/s", "down",
			[]string{uHetznerSG, uHetznerASH}, nil),
		presetProfile("intl-multi", "国外", "海外八源聚合 ⭐", "Cloudflare+OVH+Vultr 日/新+Fastly+CloudFront+Google+Hetzner，八个不同厂商，单家抖动不拖垮全局", "down",
			[]string{uCF, uOVH, uVultrJP, uVultrSG, uKernelFastly, uDockerCF, uGoGoogle, uHetznerSG}, nil),

		// ---------------- 上传 ----------------
		presetProfile("up-cf", "上传", "Cloudflare 上传", "公共可写端点，无需自建接收端，直接测上行", "up",
			nil, []string{uCFUp}),
		presetProfile("up-sink", "上传", "本机 sink（需改 IP）", "把 127.0.0.1 改成 NAS 局域网 IP，或用自建云端 sink", "up",
			nil, []string{uSinkUp}),
	}
}

// presetConfig 构造一份带预设的完整配置。
func presetConfig() *Config {
	ps := DefaultPresets()
	return &Config{
		Format:            ConfigFormat,
		Version:           ConfigVersion,
		SelectedProfileID: ps[0].ID,
		Profiles:          ps,
	}
}
