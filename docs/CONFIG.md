# 配置与数据

文件从哪来、写到哪里、格式长什么样。改配置前先看这一篇。

- [1. 文件树](#1-文件树)
- [2. 配置持久化](#2-配置持久化)
- [3. 全局测速参数](#3-全局测速参数)
- [4. 内置测速源](#4-内置测速源)
- [5. 增删配置集](#5-增删配置集)
- [6. 配置格式](#6-配置格式)

## 1. 文件树

```
fnspeed/
├── manifest                    # appname/version/service_port/checksum
├── app.tgz                     # 由 build.sh 生成，解压后落位 target/fnspeed
├── build.sh                    # 双架构编译 + 打包（注入 checksum）
├── fnspeed.sc                  # 端口声明（仅 8976，port_forward=no：走网关不开端口）
├── ICON.PNG / ICON_256.PNG     # 64x64 / 256x256
├── cmd/                        # 生命周期脚本（飞牛规范要求 main）
│   ├── common                  # 公用变量与启停函数
│   ├── main                    # start / stop / status
│   ├── install_init  install_callback
│   ├── upgrade_init  upgrade_callback
│   ├── uninstall_init  uninstall_callback
│   └── config_init   config_callback
├── config/
│   ├── privilege               # run-as: package
│   └── resource                # port-config.protocol-file -> fnspeed.sc
├── ui/
│   ├── config                  # 桌面入口（iframe + gatewaySocket 统一网关）
│   └── images/icon_{64,256}.png
├── src/
│   ├── go.mod
│   ├── main.go                 # 入口、TCP+Unix Socket 双监听、前缀剥离
│   ├── engine.go               # 可增删的流池（动态并发）
│   ├── scheduler.go            # 爬坡 + 掉速补偿
│   ├── probe.go                # 端点预检与传输模式判定
│   ├── ping.go                 # 目标/网关双探针
│   ├── resource.go             # CPU/内存/goroutine 采样
│   ├── analysis.go             # 结论判定（本机/链路/目标端）
│   ├── stats.go                # 原子计数 + 环形缓冲 + 稳定值
│   ├── config.go               # v2 配置解析/导出
│   ├── presets.go              # 出厂测速源（1.6.0 起只在降级时生效）
│   ├── cloud.go                # 云配置：远端拉取 / 校验 / 降级 / 快照
│   ├── selftest.go             # 分层网络自检（DNS→TCP→TLS→HTTP）
│   ├── store.go                # etc/ 下配置原子写 + .bak
│   ├── api.go                  # HTTP API + SSE
│   ├── tune.go                 # 内核参数调优
│   └── web/index.html          # 前端（编译期 embed）
├── sink/                       # 配套接收端（含 -rate 限速，可模拟受限链路）
├── cloud/                      # 云端清单 sources.json + 维护说明
├── tools/
│   ├── make_icons.py           # 构建期生成图标
│   ├── sign_cloud.py           # 给云端清单算 HMAC 签名
│   └── check_privacy.sh        # 发布前隐私自检（CI 也跑这一条）
├── docs/history/               # 已完工的历史计划
└── screenshots/                # README 用的界面截图
```

## 2. 配置持久化

| 项 | 规则 |
|---|---|
| 唯一写入位置 | `etc/`（`$TRIM_PKGETC`）与 `var/`；**绝不写 `target/`** |
| 默认区（出厂） | `etc/builtin.json`：程序所有，每次启动按内容 hash 比对，不同就**整体覆盖** |
| 默认区（云端） | `etc/cloud.json`：远端清单快照，带 ETag 与拉取时间。云端可用时由它接管默认区，详见 [CLOUD.md](CLOUD.md) |
| 用户区 | `etc/user.json`：**永不触碰**，只存增量（自建的、改过的、隐藏的） |
| 原子写 | 临时文件 → `fsync` → `chmod` → `rename` |
| 备份 | 每次替换前把旧文件写成 `.bak` |
| 损坏恢复 | 启动时主文件解析失败 → 回退 `.bak`，界面顶部提示来源 |
| 前端 | **不用 localStorage**（App 内 webview 会清）；保存一律 `POST /api/profiles` 落盘后**回读**，不做本地乐观更新 |

云端接管期间 `builtin.json` 不会被改写 —— 它必须一直是出厂清单，否则云端永久不可用时
用户点「恢复默认」也退不回去。

拆成多份文件的原因：旧版把所有配置集塞在一个 `profiles.json` 里，升级时只能「按 ID 补齐」，
从不删除 —— 删掉的内置分组会在恢复默认后原样复活。现在默认区靠整体替换文件清理，
用户区只存增量，结构上不可能残留垃圾。

- 用户改了某个默认分组 → 自动在 `user.json` 复制一份并打 `forkedFrom` 标记，默认区那份照常被覆盖
- 用户删了某个默认分组 → ID 记进 `user.json` 的 `hidden`，重启不会复活
- `presetRevision` 常数已废弃，判断更新靠内容 hash

**首次升级会迁移**：旧的 `profiles.json` 整体丢弃，改名 `profiles.json.legacy.bak` 留档。
想捡回旧配置，把文件改回原名即可。（迁移按「整体丢弃」处理，分不清旧文件里哪些是用户自建 ——
这是刻意的取舍，换取结构上不可能残留垃圾。）

保存接口返回的是磁盘上的真实内容：

```json
{"config": {...}, "note": "", "path": "/var/apps/fnspeed/etc/user.json", "savedAt": 1793586619}
```

卸载时保留 `var/` 与 `shares/`，`etc/` 随应用走。

## 3. 全局测速参数

**并发、时长、流量上限是全局设置**，只有一份，存在 `etc/settings.json`，与配置集无关：

- 换源、导入配置、恢复预设都**不会**改变这三个值
- 配置集里自带的 `concurrency` / `durationSec` 字段在 1.4.0 起被忽略（兼容旧文件不报错）
- **并发是上限，不是起步值**：调度器只在这个数以内爬坡，填 4 就最多 4 条。
  填 0 或不填等于不限制，回落到内置上限 64
- 默认值：并发 **64**（= 不限制）/ 时长 **60s** / 不限流量；改动即落盘（防抖 0.7s），重启保留
- 同一份文件里还有两个「打哪儿」的开关，容易被忽略：
  - `region`：定向测速位置（`gd-ct` 或海外城市 `intl-jp-tyo`），空串 = 就近
  - `prefer`：线路偏好，`""` 就近 / `"overseas"` 海外优先（改用海外 DoH 解析，见 DESIGN §2）。
    指定了 `region` 时 `prefer` 不生效
- 接口：`GET/POST /api/settings`；`GET /api/profiles` 响应也附带当前值

## 4. 内置测速源

预设写进 `etc/builtin.json`，每次启动按内容 hash 比对，不同就整体覆盖；
用户改过的分组自动落到 `etc/user.json`，升级不会覆盖，删掉的也不会被塞回来。

| 分组 | ID | 源 | 说明 |
|---|---|---|---|
| 国内 | `cn-tencent` | 腾讯 CDN 3 源 | 王者安装包 1.9GB + 微信 PC/x86，腾讯自建分发，不拦不限 |
| 国内 | `cn-aliyun` | 阿里 CDN · npmmirror 4 源 | node/electron 大包 81–111MB，阿里云 CDN，国内海外都稳 |
| 国内 | `cn-xunlei` | 迅雷 CDN 2 源 | 172MB + 13MB，自建分发节点 |
| 国内 | `cn-apple` | 苹果 CDN | 2.9GB iOS 固件，境内速度最好的一条 |
| 国内 | `cn-game` | 游戏 CDN · 鹰角 | 1.8GB 明日方舟安卓包，字节 CDN，latest 别名不随版本失效 |
| 国内 | **`cn-multi`** | 上面 11 个源聚合 | ⭐ 任意一个挂了自动换源，最容易跑满 |
| 国外 | `intl-cf` | Cloudflare 100MB 循环 | 全球 Anycast，最稳的跨境基准 |
| 国外 | `intl-ovh` | OVH 法国 100MB | 测欧洲回程 |
| 国外 | `intl-vultr-jp` | Vultr 东京 100MB | 出海最近的一跳 |
| 国外 | `intl-vultr-sg` | Vultr 新加坡 100MB | 东南亚出口 |
| 国外 | `intl-vultr-us` | Vultr 新泽西 100MB | 美东回程 |
| 国外 | `intl-vultr-de` | Vultr 法兰克福 100MB | 欧洲回程 |
| 国外 | `intl-fastly` | Fastly · kernel.org 133MB | Fastly 自有骨干，单线程实测 6.8MB/s |
| 国外 | `intl-cloudfront` | AWS CloudFront · docker 62MB | CloudFront 边缘，实测 5.8MB/s |
| 国外 | `intl-google` | Google CDN · golang 66MB | `dl.google.com`，实测 17MB/s |
| 国外 | `intl-hetzner` | Hetzner 新加坡/美东 各 1GB | 1GB 单文件，重连频率低，实测 3.7/4.2MB/s |
| 国外 | **`intl-multi`** | 八家厂商各一条 | ⭐ CF + OVH + Vultr×2 + Fastly + CloudFront + Google + Hetzner |
| 上传 | `up-cf` | `speed.cloudflare.com/__up` | 公共可写端点，无需自建接收端 |
| 上传 | `up-sink` | `127.0.0.1:8977/up` | 改成 NAS 局域网 IP，或用云端 sink |

> 1.6.1 之前「海外六源聚合」里 5 条是 Vultr、另有 17 条 DataPacket 镜像 —— 同一家抖动
> 整组跟着抖，才是「海外源不稳定」的主因。现在按厂商拆分，一家掉线不牵连其余。
> 另外 `speedtest.*.linode.com`、`*.digitalocean.com` 这类经典测速域名在国内多数解析不出来，
> 不要凭印象往里加。

> 1.5.2 起国内源只留「软件厂商自有 CDN 上的大安装包」。高校与云厂商镜像站对本应用
> 几乎全线不可用：华为云 418 风控、清华/中科大/北外/上交 403、南大重定向死循环、
> 网易 503、阿里 302 到不存在的路径。那些站为浏览器下载设计，WAF 把并发长连接当攻击；
> 厂商分发安装包的 CDN 本来就是给百万用户同时拉几 GB 用的。

选源思路与 [lolicp 流量消耗器](https://lolicp.com/api/wasted_traffic_plus/) 一致：**挑大厂 CDN
上的单文件大对象**，而不是自建测速脚本。区别是那边用几百 KB 的图片/APK 刷流量（打定向流量包），
这边只保留 GB 级 ISO / 100MB 测速文件，才能真正把带宽压满。

**一个源天生跑不满千兆**：镜像站按单连接限速，所以靠多流叠加。能爬到多少由「并发上限」兜住。
跑前先点「预检端点」：`HTTP 418` = 目标端风控拦了你的 IP（换源），`HTTP 429` = 限流（等一会），
`超时` = 线路不通。

**运营商自有测速点**（电信 / 联通 / 移动）基本全部下线，本项目**没有**这三类分组。
线路覆盖由华为云 / 阿里云 / 腾讯云 / 教育网镜像承担，均是多线 BGP。

## 5. 增删配置集

- **新建 / 改**：下拉选一个、改 URL 列表、点「存为配置集」（按 ID 合并，不会冲掉别的）
- **删除**：下拉选中 → 点「删除当前」→ 确认。只删这一条，落盘后回读
- **一键回来**：点「恢复内置预设」，按 ID 覆盖已有预设、自建的不动

接口：`DELETE /api/profiles?id=<id>`（可重复传 `id` 批量删），返回 `{"removed":N,"config":{...}}`。

## 6. 配置格式

与「跑满了吗」v2 互通，本实现是其超集。

```json
{
  "format": "PAOMAN_SPEED_TEST_CONFIG",
  "version": 2,
  "selectedProfileId": "p1",
  "profiles": [{
    "id": "p1",
    "name": "示例",
    "downloadUrls": ["https://example.com/big.bin"],
    "uploadUrls": ["https://example.com/up"],
    "downloadEndpoints": [{"url": "...", "method": "GET", "headers": {}}],
    "uploadEndpoints": [],
    "concurrency": 8, "durationSec": 300, "bytesLimit": 0,
    "mode": "both", "urlRotate": "sticky", "switchPolicy": "failover"
  }]
}
```

导入规则：

- `downloadUrls` 与 `downloadEndpoints` 重复时，以 endpoints 为准
- 上传 URL 指向站点首页（路径空或 `/`）→ 标记「非直传端点」，不废整条配置
- 带 `sign` / `t` / `timestamp` / `expires` 参数的 → 标记「时效链接」，403 时只降级该条
- 导出两档：标准 v2（剥掉扩展字段，可导回手机 App）/ 含扩展字段
