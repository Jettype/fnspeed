# fnspeed · 飞牛 fnOS 带宽压测应用

双向并发带宽压测。Go 单二进制（CGO 关闭、静态链接），前端单页内联，运行时零依赖。

装到飞牛 fnOS 上，用可动态增减的 HTTP 并发流把上行 / 下行链路压满，实时画出速率曲线，
并按「本机 / 链路 / 目标端」三层给出瓶颈结论。现成的在线测速只能打单一测速点、且被运营商
优化过，测不出跨地区跨运营商的真实表现——这是它存在的理由。

> **免责声明｜仅限自有或已获授权的链路**
>
> 本项目用于**自有或已获明确授权**的链路测速。向未授权的站点、机房或第三方服务施压，
> 可能违反当地法律与目标站点的使用条款，并产生真实流量费用。使用者承担全部责任，
> 作者不提供任何担保。

## 快速开始

**装到 NAS（推荐）**

1. 到 [Releases](../../releases) 下载对应架构的 `.fpk`：x86（N100 / J4125 / 多数成品 NAS）或 arm64
2. 飞牛应用中心 → 手动安装 → 选包。升级会保留 `user.json`（自建源、隐藏列表）
3. 装完图标不刷新时执行 `sudo systemctl restart trim_app_center.service`
4. 浏览器打开 `http://<NAS-IP>:8976`

**从源码构建**

```bash
./build.sh
# 产出 dist/fnspeed_x86_<VER>.fpk 与 dist/fnspeed_arm_<VER>.fpk
```

依赖 Go ≥ 1.21、python3 + Pillow（仅用于生成图标）。无第三方 Go 依赖，不需要 `go mod download`。

**上行压测需要接收端**：公网没有可直接写的端点，需自建 `sink/`（独立模块，含 `-rate`
限速模拟）。默认预设里只有 `127.0.0.1:8977` 占位。

## 文档

| 文件 | 内容 |
|---|---|
| 本文件 | 功能说明、配置格式、接口、构建与部署 |
| [CHANGELOG.md](CHANGELOG.md) | 各版本改了什么、为什么改（含已缺失的 1.5.2） |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 硬约束、新增测速源门槛、提交前必跑 |
| `PLAN-1.5.1.md` | 1.5.1 开工清单，历史文档，留作决策记录 |
| `sink/` | 接收端源码；部署步骤（systemd + nginx 反代示例）见本文件「部署接收端」一节 |

## 许可

[MIT](LICENSE)。测速源预设指向的第三方 CDN 与安装包归各厂商所有，本项目只引用其公开下载地址。

## 1.5.4 变更（2026-11-03）

### 定向测速：打指定省份、指定运营商的 CDN 节点

新增「测速位置」。选一个「省 + 运营商」（共 **88 个位置**），本机就不再从就近节点取数据，
而是让 CDN 把该省的节点交出来，再直连过去。用来验证跨地区、跨运营商的真实速度。

原理两步，都已实测跑通：

1. **EDNS Client Subnet 解析** —— 向 `223.5.5.5` 发带 `edns_client_subnet=<该省运营商地址>`
   的 DoH 查询，CDN 按这个「门牌号」返回当地节点。同一个域名实测差异很直观：

   | ECS 位置 | 返回节点 |
   |---|---|
   | 默认（本机出口） | 183.3.204.44–51、14.29.50.63–69 |
   | 北京联通 | 101.73.101.60–67、218.12.78.73 |
   | 上海移动 | 39.185.246.20、39.185.246.61–67 |
   | 四川电信 | 182.140.142.54–61 |

2. **强制拨号** —— 丢掉 DNS 结果，直接连目标 IP，TLS 的 SNI 与 Host 仍用原域名。
   实测四地节点全部 200，CDN 不校验 IP 与域名的归属关系。

几个实现细节：

- 解析结果缓存 10 分钟；`auto`（默认）等于不改任何东西，行为和以前完全一致
- 302 跨域名时（游戏厂的 latest 入口是两跳三个域名）会对新域名按同一位置重新解析，
  不会拿旧 IP 硬连新域名
- 端点上同时显示「首跳解析地址 → 实际下载节点」，例如
  `◎ 定向 北京 · 联通 · 43.145.18.240 → 36.110.220.110`
- 省份地址表用公网 IP 归属库逐条核对过，**只保留省对且运营商对的条目**；
  山西电信、吉林电信、贵州联通、江苏移动、宁夏移动这 5 个位置核不准，宁缺勿错直接不收

> 局限说清楚：ECS 只是「模拟位置」，不等于真机在该省的结果（CDN 还看 Local DNS 出口、
> 节点负载）。部分 CDN 不响应 ECS，这时解析结果和就近一样，从端点行的 IP 能看出来。

### 新增源：游戏厂 CDN

`游戏 CDN · 鹰角`：明日方舟安卓包 **1.8GB**，实际落在字节跳动 CDN（`server: Byte-nginx`）。
入口用的是官方 `latest` 别名（`ak.hypergryph.com/downloads/android_lastest`），
302 两跳指向当前最新包 —— 所以它**不会随版本失效**，这是它能进预设的关键。

米哈游（原神/星铁/绝区零）和库洛（鸣潮）这次没能收：它们的下载地址把版本号写进路径，
启动器接口 `hyp-api` 对 launcher_id 1–240 全部返回 `launcher not found`，官网又是纯 SPA
拿不到直链。硬塞一条几天后必然 404 的地址，比没有更糟。留到「动态源」那版再说。

### 多源不再「只有前几条发力」

旧逻辑用「当前流数 % URL 条数」分配，流的重建顺序固定，前几条源会一直轮到自己。
现在改成两条：

- **按负载派人**：新流给「身上流最少且不是小文件」的源，先把所有源点亮，再回头加码
- **覆盖优先**：并发被带宽拐点卡住时，只要还有源一条流都没分到，就继续补流，
  哪怕这一批没有吞吐增益 —— 总速度不会因此变高，但用户要看到每条源的真实状态

小于 32MB 的源（js、图片、13MB 的安装器）标记为「短文件」，爬坡时不参与分配，
避免它们读完就 EOF、反复重连制造噪声。

实测 11 源 / 16 并发：改造前末尾 3 条源全程 0 字节，改造后 45 秒内全部点亮。

### 跑完之后大号读数定格为本次平均

以前一停就归零显示 `0 bps`，而下面「平均」格写着 177.4M，两个数打架。现在结束后大号
定格成本次平均，并在旁边标一个「本次平均」小标签，说明这不是实时值。

## 1.5.3 变更（2026-11-03）

### 新增浅色主题

顶栏右上角新增主题按钮，三态循环：**跟随系统 → 深色 → 浅色**，选择存 localStorage。
默认 `auto`，系统切到浅色时自动跟随；首帧前由一段内联脚本预先设置 `data-theme`，
不会出现深色闪一下再变白。

配色不是简单反色，而是两套独立令牌：浅色下琥珀 `#A35F00`、青 `#0E6F7C`，
均为在白底上实测对比度 ≥ 5:1 的深色化版本；`<meta name="theme-color">` 同步跟随，
手机状态栏颜色不会和页面打架。canvas 曲线拿不到 CSS 变量，改为切换主题时读一次
computed style 缓存进 `TH`，图表网格、坐标轴、填充渐变色一并跟着变。

### 可读性：字号整体上调，对比度按 AA 校准

| 元素 | 旧 | 新 |
|---|---|---|
| 主速率 | 48 / 58px | **56 / 66px** |
| 指标格数字 | 14 / 15px | **18 / 19px** |
| 指标格标签 | 11px | **12.5 / 13px** |
| 卡片标题 | 11px | **12.5px** |
| 链路四格数值 | 15px | **18px** |
| 结论文字 | 12px | **13.5px** |
| 端点 URL | 11px | **12px** |
| 表单控件 | 12.5px | **14px** |
| 按钮（主操作） | 13px | **15px** |
| 提示 / hint | 9.5px | **11px** |

最小字号从 8px 提到 10–11px，正文基准 14→15px。深色下 `--dim2` 由 `#767D89` 提亮到
`#8A929E`（对比度 5.87:1），浅色下所有正文级颜色均 ≥ 4.5:1。

另外几处顺手的修正：

- 图表右轴刻度原来会被画布边缘裁掉最后一位（字号变大后更明显），`padOf()` 边距加密
- 全局参数三格补了固定标签「并发流 / 时长·秒 / 上限·GB」，不再依赖 placeholder
- 运行中「开始」退成中性描边（原来是块半透明的脏琥珀），「停止」转为琥珀高亮
- 端点开关、IP 卡片、自检层次字号同步放大，标签列宽从 30px 调到 38px

## 1.5.2 变更（2026-11-03）

### 大号速率一直显示 0 bps，累计流量却在涨

根因不在前端，在服务端算速率的方式。

`Snapshot()` 原来自己拿 `lastRec` 现场算差值：`(rec - lastRec) / dt`。但 `lastRec` 是由
`Tick()`（500ms 一次）更新的，而 SSE 推送几乎总是**紧跟在 Tick 之后**触发 —— 此时 `rec`
和 `lastRec` 相等，`dt` 又接近 0，商出来恒等于 0。

改成 `Tick` 里算好瞬时速率存进原子变量、`Snapshot` 直接读，两边共用同一个时间基准。
现在速率和累计流量同一份数据源，不会再出现「下面动上面不动」。

### 曲线锯齿：所有流同时到达文件末尾

同一 URL 被多条流压时，旧实现是每条流都从头读到尾。文件读完的时刻几乎一致 →
一起断开、一起重连、速率集体归零，画出来就是 0 到峰值之间来回跳。

两处改动：

- **随机起点 + 读到尾**：每条流首次请求带 `Range: bytes=<随机>-`，各流从不同位置起读，
  结束时刻自然错开。顺带把重连频率从「每 32MB 一次」降到「每个文件一次」。
  服务器不认 Range 时返回 200 全量，行为退化成从头读，不影响正确性。
- **重连抖动**：读完重连时加 10–80ms 随机延迟，避免多条流在同一毫秒撞在一起。

实测同一场景（千兆家宽）：修改前曲线在 0 与峰值之间剧烈摆动，谷底直接归零，稳定值
只有峰值的 36%；修改后不再归零，稳定值提升到峰值的约 79%。

### 国内源全部换掉

上一版的十个国内源在普通国内家宽环境下**只有腾讯和苹果能用**，其余全部被拦：

| 源 | 实际响应 |
|---|---|
| 华为云镜像 | HTTP 418，风控拦截 |
| 阿里云镜像 | 连接被拒绝 |
| 网易镜像 | HTTP 503 |
| 南大镜像 | 重定向死循环 |
| 清华 / 中科大 / 北外 / 上交 | HTTP 403 |

这些站点是为「浏览器点一下下载」设计的，WAF 会把并发长连接当成攻击。换成**软件厂商的
分发 CDN**——它们本来就是给几百万人同时拉几 GB 安装包用的，不拦、不限速、支持 Range：

| 分组 | 内容 | 实测 |
|---|---|---|
| 腾讯 CDN · 3源 | 王者荣耀 1968MB + 微信 270MB + 微信 x86 175MB | 3.1–9.7 MB/s |
| 阿里 CDN · 4源 | npmmirror 上的 node/electron 包 81–111MB | 9.0–12.6 MB/s |
| 迅雷 CDN · 2源 | 安装包 172MB + 13MB | 11.7–14.1 MB/s |
| 苹果 CDN | iOS 固件 2975MB | 17.9 MB/s |
| 国内十源聚合 ⭐ | 以上 10 条全部 | — |

`registry.npmmirror.com` 是淘宝的 npm 镜像，走阿里云 CDN，是少数国内直连通畅的大文件源。

### 底栏与配置区不再重复

手机端布局是「主视图在上、配置区在下」，同一屏里曾出现两个「开始」和两个配置集下拉。
现在用 `IntersectionObserver` 监听配置区那排按钮：它离开视口时底栏才淡入，
回到视口就淡出。任何时刻屏幕上只有一个「开始」。

## 1.5.1 变更（2026-11-03）

### 配置拆成两个文件，默认区每次安装整体覆盖

| 文件 | 归属 | 安装/升级时 |
|---|---|---|
| `etc/builtin.json` | 程序 | **按内容 hash 比对，不同就整体覆盖** |
| `etc/user.json` | 用户 | **永不触碰** |

旧版把所有配置集塞在一个 `profiles.json` 里，升级时只能「按 ID 补齐」，从不删除 ——
删掉的内置分组（小文件 / 手机厂 / 电商广告）会在恢复默认后原样复活。

新版不存在这个问题：默认区靠**整体替换文件**清理，用户区只存增量（自建的、改过的、隐藏的）。

- 用户改了某个默认分组 → 自动在 `user.json` 复制一份并打 `forkedFrom` 标记，默认区那份照常被覆盖
- 用户删了某个默认分组 → ID 记进 `user.json` 的 `hidden`，重启不会复活
- `presetRevision` 常数已废弃，判断更新靠内容 hash

**首次升级会迁移**：旧的 `profiles.json` 整体丢弃，改名 `profiles.json.legacy.bak` 留档。
想捡回旧配置，把文件改回原名即可。

### 新增源

| 分组 | 文件 | 实测 |
|---|---|---|
| 腾讯云 CDN | 游戏包 1.9GB | 19.4 MB/s |
| 苹果 CDN | iOS 固件 2.9GB | 11.8 MB/s |
| 腾讯 · 微信安装包 | 270MB | 2.8 MB/s |

苹果源域名是 `updates.cdn-apple.com`，旧版的 `updates-http.` 前缀已失效。

### UI

- **补回累计流量**：累计 / 已下载 / 已上传 三格，实时上行速率挪到顶部眉标
- **修掉假速率**：端点状态右侧原来显示 `68.8 GB/s`，实际是累计字节被当成了速率（`fmt.rate(recv)`），现在直接显示容量
- **修掉假绿点**：有 `lastError` 却标健康态，现在是红点
- **可读性**：次要字号 10–11px → 12–13px，灰字提亮到 AA 对比度，手机隐藏刻度带
- **命中 IP 内联**：端点状态卡片加「命中」开关，打开后每条源下面显示 `IP · CDN`

### 命中 IP 的采集方式

- **运行中**：连接建立时 `httptrace.GotConn` 顺手记下对端地址，**不产生任何额外请求**
- **空闲时**：打开开关才调 `POST /api/hits` 批量探测，4 并发 / 单条 5s / 结果缓存 10 分钟
- **总闸**：「显示 IP」关闭时服务端一个外部请求都不发，端点里的 IP 文本立即清空（不是隐藏）

### 已知限制

- 运营商自有测速点（电信 / 联通 / 移动）基本全部下线，本版**没有**这三类分组。
  线路覆盖由华为云 / 阿里云 / 腾讯云 / 教育网镜像承担，均是多线 BGP。
- `builtin.json` 内容和 `DefaultPresets()` 绑定，改源需要重新打包。1.6.0 计划做云端配置（远端 JSON + 快照兜底）。
- 迁移按「整体丢弃」处理，分不清旧文件里哪些是用户自建 —— 这是刻意的取舍，换取结构上不可能残留垃圾。

## 1. 文件树

```
fnspeed/
├── manifest                    # appname/version/service_port/checksum
├── app.tgz                     # 由 build.sh 生成，解压后落位 target/fnspeed
├── build.sh                    # 双架构编译 + 打包（注入 checksum）
├── fnspeed.sc                  # 防火墙端口规则（8976/8977）
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
│   ├── presets.go              # 内置国内/国外/上传测速源
│   ├── selftest.go             # 分层网络自检（DNS→TCP→TLS→HTTP）
│   ├── store.go                # etc/profiles.json 原子写 + .bak
│   ├── api.go                  # HTTP API + SSE
│   ├── tune.go                 # 内核参数调优
│   └── web/index.html          # 前端（编译期 embed）
├── sink/                       # 配套接收端（含 -rate 限速，可模拟受限链路）
├── tools/make_icons.py
└── screenshots/
```

## 2. 应用入口与反代（打不开就是这一节）

飞牛有两套入口机制，**只有第二套能在手机 App 和 FN Connect 远程打开**：

| 模式 | 配置 | 结果 |
|---|---|---|
| 端口直连 | `protocol: http` + `port: "8976"` + `url: "/"` | 桌面点开 = 浏览器跳 `http://NAS:8976`。手机 App 内不内嵌；FN Connect 的 HTTPS 页面里嵌 HTTP 会被浏览器按 mixed-content 拦掉 |
| **统一网关**（本项目） | `protocol: ""`、`port: ""`、`url: "/app/fnspeed"`、`gatewayPrefix`、`gatewaySocket` | 网关把 `/app/fnspeed/*` 从 Unix Socket 转发给应用，复用飞牛域名与登录态，手机 App 内直接内嵌 |

`ui/config` 实际内容：

```json
{
    ".url": {
        "fnspeed.Application": {
            "title": "带宽压测",
            "icon": "images/icon_{0}.png",
            "type": "iframe",
            "protocol": "",
            "port": "",
            "url": "/app/fnspeed",
            "gatewayPrefix": "/app/fnspeed",
            "gatewaySocket": "fnspeed.sock",
            "allUsers": true
        }
    }
}
```

| 约束 | 取值 | 不遵守的后果 |
|---|---|---|
| `.url` key | 必须以 `appname` 为前缀，且与 `desktop_applaunchname` 逐字符相同 | 入口损坏，**构建不报错**，表现为点图标无反应 |
| `type` | `iframe` | `url` 类型在手机 App 内不内嵌 |
| `gatewaySocket` | `fnspeed.sock`，与 `cmd/common` 的 `$SOCK` 一致 | 网关不知道往哪转发 → 白屏 |
| `gatewayPrefix` | `/app/fnspeed` | 前缀对不上 → 404 |
| `icon` | `images/icon_{0}.png`（`{0}` 替换成 64/256） | 图标空白；`build.sh` 会同时产出 `icon_64.png` 与 `icon-64.png` 两套命名 |
| `control.accessPerm` | 只能写 `editable` / `readonly` / `hidden`，或不写 | 写了 `admin` 这类非法值，整条入口解析失败 → 桌面什么都没有 |
| **`ui/` 的位置** | **必须在 `app.tgz` 里**（安装后即 `/var/apps/fnspeed/target/ui/`） | 只放在 fpk 顶层 = 应用中心读不到 → **桌面永远不出图标**（1.2.0 之前就是这个 bug） |

### 入口文件到底放在哪（最容易踩的坑）

官方 `fnpack` 工程结构是：

```
myapp/
├── app/
│   └── ui/
│       ├── config          ← 入口定义
│       └── images/icon_{64,256}.png
├── manifest
└── config/
```

`app/` 整个目录会被打进 `app.tgz`，安装后落到 `/var/apps/fnspeed/target/`。
所以入口的真实路径是：

```
/var/apps/fnspeed/target/ui/config
/var/apps/fnspeed/target/ui/images/icon_64.png
/var/apps/fnspeed/target/ui/images/icon_256.png
```

**不是** fpk 包顶层的 `ui/config`。`build.sh` 现在两处都放（顶层那份纯属兼容，无副作用），
部署后可以用一条命令确认：

```bash
ls -l /var/apps/fnspeed/target/ui/config /var/apps/fnspeed/target/ui/images/
```

如果这条命令 404，桌面就不会有图标 —— 与包里的 `desktop_uidir=ui` 完全对应。

服务端配合做了四件事：

- **双通道监听**：TCP `:8976`（局域网直连调试）+ Unix Socket `$TARGET/fnspeed.sock`（网关，权限 0770）。
- **前缀剥离**：收到 `/app/fnspeed/api/status` 时剥掉前缀再路由；不带尾斜杠会 301 补斜杠，否则相对路径解析错位。
- **不加框限制**：不发送 `X-Frame-Options`、不设 CSP `frame-ancestors`。
- **前端零绝对路径**：所有请求都是 `api/...` 相对路径，挂哪个前缀都能命中。

`/api/stream` 固定返回：

```
Content-Type: text/event-stream
Cache-Control: no-cache
X-Accel-Buffering: no      ← 缺这行，Nginx 会缓冲整段，曲线变成一次性刷出
```

> 覆盖安装后桌面入口没刷新：`systemctl restart trim_app_center.service`（飞牛应用中心服务），再重进应用中心。

## 2.1 手机里显示发虚 / 元素忽大忽小

内嵌 WebView 和普通浏览器不是一回事，踩过三个坑，都已处理：

| 坑 | 现象 | 处理 |
|---|---|---|
| 飞牛 App 的 WebView 自行缩放 | 文字发虚，1 个 CSS 像素对不上整数个物理像素 | viewport 显式锁死 `maximum-scale=1, minimum-scale=1, user-scalable=no` |
| Android **Font Boosting** | 文字被系统额外放大，卡片比例全乱 | `html{-webkit-text-size-adjust:100%;text-size-adjust:100%}` |
| 字号用 `vw` / `clamp()` | 算出 `13.87px` 这种小数，高 DPI 屏渲染发虚 | 全部改成整数 px，桌面端用 `@media` 断点放大 |

另外曲线画布按 `devicePixelRatio` 缩放（上限 2），空态不再画假刻度。

## 3. 配置持久化

| 项 | 规则 |
|---|---|
| 唯一写入位置 | `etc/`（`$TRIM_PKGETC`）与 `var/`；**绝不写 `target/`** |
| 主文件 | `etc/profiles.json` |
| 原子写 | 临时文件 → `fsync` → `chmod` → `rename` |
| 备份 | 每次替换前把旧文件写成 `profiles.json.bak` |
| 损坏恢复 | 启动时主文件解析失败 → 回退 `.bak`，界面顶部提示来源 |
| 前端 | **不用 localStorage**（App 内 webview 会清）；保存一律 `POST /api/profiles` 落盘后**回读**，不做本地乐观更新 |

保存接口返回的是磁盘上的真实内容：

```json
{"config": {...}, "note": "", "path": "/var/apps/fnspeed/etc/profiles.json", "savedAt": 1793586619}
```

卸载时保留 `var/` 与 `shares/`，`etc/` 随应用走。

### 3.1 全局测速参数（1.4.0 起）

**并发、时长、流量上限是全局设置**，只有一份，存在 `etc/settings.json`，与配置集无关：

- 换源、导入配置、恢复预设都**不会**改变这两个值
- 配置集里自带的 `concurrency` / `durationSec` 字段在 1.4.0 起被忽略（兼容旧文件不报错）
- 默认值：并发 **32** / 时长 **60s** / 不限流量；改动即落盘（防抖 0.7s），重启保留
- 接口：`GET/POST /api/settings`；`GET /api/profiles` 响应也附带当前值

## 4. 测速策略

调度器每 500ms 采样、每 2s 决策一次。

**爬坡（贪心加流，没有时间上限）**

| 条件 | 动作 |
|---|---|
| 起始 | 4 条流 |
| 加完一批后等 4s（新连接慢启动），吞吐比加流前涨 ≥5% | 继续加，步长 2→3→4（并发 8 / 24 分档） |
| 连续两轮涨幅 <5% | 判定拐点，锁定并发，进入巡航 |
| 达到 64 条 | 强制锁定 |

> 旧版「10 秒后无条件锁定」是错的：千兆链路上 10 秒只能加到 6 条流，永远跑不满。现在只有**真的加不动**才停。

**巡航与补偿**

| 现象 | 动作 |
|---|---|
| 吞吐跌破拐点的 75% | 剔除速率低于中位数 50% 的慢流并重连；全健康则 +1 流 |
| 吞吐创新高 | 抬高水位基准，后续按新峰值算跌幅 |
| 连续 20s 一个字节都没有 | 判定「目标端 20 秒无任何数据」并结束，不让你对着 0 干等 |

**流级自愈**

- 单流失败：指数退避重连（200ms → 5s，封顶）
- 连续 3 次失败：轮换到下一个 URL；所有 URL 都被判死时也会强行换，因为预检结论可能已过期
- 服务端秒断（一次拿到 <64KB）：加 120ms 间隔再连，避免空转烧 CPU 却零吞吐

## 4.0 连不上源？先点「网络自检」

「连不上源」是这类工具最常见的抱怨，但它是个笼统描述 —— 可能卡在 DNS、TCP、TLS、HTTP 任意一层，处理方式完全不同。所以界面上有个 **网络自检** 按钮，一次把每个 URL 的四层全测一遍：

| 层 | 测什么 | 断了说明 |
|---|---|---|
| **DNS** | 域名解析，记录耗时与解析到的 IP | NAS 的 `/etc/resolv.conf` 有问题，或旁路由不响应 |
| **TCP** | 到 `host:443` 的三次握手 | 出网被拦：被墙、防火墙丢包、旁路由没放行 |
| **TLS** | 握手 + 证书主体 + 到期天数 | 中间设备改写了连接，或系统时间不对 |
| **HTTP** | 带 `Range: bytes=0-0` 的 GET | 源本身的问题：风控 / 限流 / 路径错 / 重定向死循环 |

面板顶部还会显示本机实际生效的 **DNS 服务器、默认网关、环境代理** —— 很多"怪问题"到这里就明白了（比如 NAS 的 DNS 指着一个已经挂掉的容器）。

### 常见结论对照

| 症状 | 真实原因 | 怎么办 |
|---|---|---|
| `HTTP 403`，DNS/TCP/TLS 都绿 | 目标端风控拦了这个 IP（清华 debian-cd 必现） | 换源；别用 `debian-cd/current` |
| `HTTP 418` / `429` | 镜像站风控 / 限流 | 等几分钟，或换源 |
| TCP 超时，DNS 正常 | 出网被拦 | 检查旁路由 / 代理规则 |
| 重定向死循环 | `current` / `latest` 是软链接且镜像站跳回自己 | URL 里换成带具体版本号的路径 |
| DNS 失败 | NAS 解析不了 | 查 `resolv.conf`，或把 DNS 指回路由器 |

### 三个让「连不上」不至于变成「用不了」的兜底

1. **预检最多等 2.5 秒** —— 探不完的直接按可用放行，先跑起来，靠流内 failover 纠正。不再让用户盯 15 秒空白。
2. **全部端点被判死时降级试跑** —— 预检只是一次带 Range 的 GET，418/429/403 往往是按 UA 或频率的风控，真实长连接未必一样。所以即使全红也照样起流，而不是直接报「没有可运行的流」。
3. **8 秒零字节就收尾** —— 一个字节都没拿到时 8 秒足够下判断，提示里直接写「换源，或点网络自检」，而不是闷等 20 秒。

## 4.1 内置测速源（15 个配置集，分三组）

1.5.1 起预设写进 `etc/builtin.json`，每次启动按内容 hash 比对，不同就整体覆盖；
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
| 国外 | **`intl-multi`** | CF + OVH + Vultr×4 + Cachefly | ⭐ 跨境最容易跑出峰值 |
| 上传 | `up-cf` | `speed.cloudflare.com/__up` | 公共可写端点，无需自建接收端 |
| 上传 | `up-sink` | `127.0.0.1:8977/up` | 改成 NAS 局域网 IP，或用云端 sink |

> 1.5.2 起国内源只留「软件厂商自有 CDN 上的大安装包」。高校与云厂商镜像站对本应用
> 几乎全线不可用：华为云 418 风控、清华/中科大/北外/上交 403、南大重定向死循环、
> 网易 503、阿里 302 到不存在的路径。那些站为浏览器下载设计，WAF 把并发长连接当攻击；
> 厂商分发安装包的 CDN 本来就是给百万用户同时拉几 GB 用的。

选源思路与 [lolicp 流量消耗器](https://lolicp.com/api/wasted_traffic_plus/) 一致：**挑大厂 CDN 上的单文件大对象**，而不是自建测速脚本。区别是那边用几百 KB 的图片/APK 刷流量（打定向流量包），这边只保留 GB 级 ISO / 100MB 测速文件，才能真正把带宽压满。

**一个源天生跑不满千兆**：镜像站按单连接限速，预设并发 16 只是调度器起点，它会自己往上爬。跑前先点「预检端点」：`HTTP 418` = 目标端风控拦了你的 IP（换源），`HTTP 429` = 限流（等一会），`超时` = 线路不通。

### 增删配置集

- **新建 / 改**：下拉选一个、改 URL 列表、点「存为配置集」（按 ID 合并，不会冲掉别的）
- **删除**：下拉选中 → 点「删除当前」→ 确认。只删这一条，落盘后回读
- **一键回来**：点「恢复内置预设」，按 ID 覆盖已有预设、自建的不动

接口：`DELETE /api/profiles?id=<id>`（可重复传 `id` 批量删），返回 `{"removed":N,"config":{...}}`。

用两段窗口均值而不是单点比值，因为 TCP 突发会让单点速率上下跳 20%。

**失败率保护**

| 条件 | 动作 |
|---|---|
| 一轮评估内新增失败 ≥ 6 次 | 本轮不加流（**只暂停，不锁定**） |

目标端开始成片拒绝时，加流只会把失败放大，吞吐反而更低。但这里刻意不做永久锁定：开局并发的 TLS 抖动也会攒出几次失败，一锁就再也爬不上去了。等失败平息，下一轮照常加流。

**掉速补偿**（锁定后）

3 秒滑动窗口内跌幅 > 20% 触发：

1. 先剔除速率低于中位数 50% 的慢流，断开重连（等价于重置拥塞窗口）
2. 所有流都健康仍然跌 → +1 条流，用新连接换新的初始窗口

**慢启动隔离与稳定值**

前 3 秒标记「爬坡中」，不计入平均值与稳定值。稳定值 = 丢弃前 3s 与后 10% 样本后取**中位数**（不是平均值，避免个别尖峰干扰）。运行中每 500ms 实时刷新，不必等结束。

**双探针**

| 探针 | 方法 | 说明 |
|---|---|---|
| 目标 host | TCP 握手 RTT | 应用以 package 运行，拿不到 CAP_NET_RAW，无法用 ICMP |
| 默认网关 | 依次试 `udp/53` → `tcp/53` → `tcp/80` → `tcp/443` → `tcp/22` | 网关从 `/proc/net/route` 解析；记住第一个有响应的方式，之后复用，避免每秒重试 |

1s 一次，输出均值 / 抖动（相邻差值绝对均值）/ 丢包率。小米、华硕等家用路由器普遍不对内网开放 DNS，所以网关探针必须带回退链，否则会一直显示「不可达」；当前生效的方式会在 UI 上标出来（如 `tcp/80`），方便判断是不是链路本身的问题。

**结论判定**顺序（从最能排除误判的开始）：

```
CPU ≥ 85% 或 内存 ≥ 92%   → 本机瓶颈
全部流失败且无吞吐         → 目标端
丢包 > 2% 或 抖动 > 30ms   → 链路
3 次以上掉速补偿           → 目标端限速
```

## 4.2 出口 IP / 命中 IP（1.5.0）

两件事常被混为一谈，这里分开看：

| | 是什么 | 用来判断 |
|---|---|---|
| **出口 IP** | NAS 出网时对端看到的「你」 | 走没走旁路由/代理、哪家运营商出口 |
| **命中 IP** | 测速目标实际连到了哪台机器 | CDN 就近调度到哪个节点、为什么快或慢 |

**出口 IP**：并行问 4 个 echo 源（ip-api / ip.sb / ipip.net / ipinfo.io），5 秒超时，任一先回先显示。多个源给出不同 IP 时判为**多出口**，逐条列出并标注 ASN —— 家宽单出口通常只有一条；走 OpenClash 时会全部变成节点 IP，一眼看出走没走代理。

**命中 IP**：对当前选中源的第一条 URL 发一次 `Range: bytes=0-0` 的请求，用 `httptrace.GotConn` 采实际连接地址，同时从响应头识别 CDN：

| 线索 | 判定 |
|---|---|
| `cf-ray` | Cloudflare（后缀是机房代码，如 `-LAX`）|
| `x-amz-cf-id` / `server: awselb` | AWS |
| `server: elb` | 华为云 ELB |
| `x-obs-request-id` | 华为云 OBS |
| `server: tengine` | 阿里云 CDN |
| 域名后缀 | vultr / linode / hetzner / ovh / datapacket(CDN77) / myapp(腾讯云) … |

**隐私（硬约束）**：IP **只在内存里存在** —— 不写 `history.jsonl`、不写 `app.log`、不进 `profiles.json`、不进 `crash.log`。`ipinfo.go` 里没有任何打印 IP 的代码。

卡片右上角有「显示 IP」开关，默认开。关掉后：

- 服务端**一个外部请求都不发**，直接返回 `{"hidden":true}`
- 界面只显示「已隐藏」，不渲染任何 IP 字符
- 状态存 `etc/settings.json` 的 `showIp`，重启保留

截图分享前先关掉它。

## 4.3 界面（1.5.0 重做）

工业仪表盘方向，不是换套模板：

- **读数窗**是记忆点：等宽大数字压着一排 tick 刻度带实时填充（当前速率 / 本次峰值）。琥珀 `#FFB224` 全站只给这里和「开始」按钮
- **四角定位标**：1px 直角线代替圆角描边，仪器感来自这里，不靠贴图
- **配色**：底 `#0A0B0D` / 面板 `#121417` / 发丝线 9% 白 / 正文 `#E8E6E1`
- **数字**：`ui-monospace` + `tabular-nums`，速率跳动时不抖位；SSE 帧间隔导致的瞬时归零用上一帧值兜 1.6 秒，避免数字闪 0
- **手机**：数据优先 —— 读数窗 → 五格 → 曲线 → 结论 → 链路 → IP → 端点，配置区在滚动区底部；底部固定条常驻「源 / 开始 / 停止」，不用滚到底也能开跑
- **桌面**：左栏 320px 常驻配置不滚动，右栏数据区独立滚动
- **字体不内嵌**：Chakra Petch 用 `Bahnschrift`（Windows）/ `DIN Alternate`（macOS）兜底，中文回落 PingFang / Noto。省 40–50KB 包体积，NAS 出不了墙也不会卡在等字体

## 5. 构建

```bash
bash build.sh
```

产出 `dist/fnspeed_x86_1.5.0.fpk` 与 `dist/fnspeed_arm_1.5.0.fpk`。
脚本会依次：生成图标 → 交叉编译 sink → 分别编译 amd64/arm64 → 打 app.tgz → 改 manifest 的 `platform` 并注入 `checksum`（app.tgz 的 md5）→ 打 .fpk。

包结构按 fnOS 规范强制包含：`manifest`、`app.tgz`、`cmd/`（含 `main`）、`config/`（`privilege` + `resource`，后者用 `port-config.protocol-file` 指向 `fnspeed.sc` 才会放行端口）、`ui/config` 桌面入口、`ui/images/{64,256}.png`、根图标、`fnspeed.sc`。缺任意一项，应用中心会报「应用数据包不完整」。

## 6. 安装

1. fnOS 应用中心 → 手动安装 → 选择对应架构的 `.fpk`
2. 安装后访问 `http://<NAS-IP>:8976`，或从桌面「带宽压测」图标进入
3. 首次打开是空列表（不内置任何压测目标）

端口选 **8976**，刻意避开常见的 8876（qBittorrent WebUI 默认占用其同一网段端口）与 8080/8096 等媒体服务端口。要改端口需同步改四处：`manifest` 的 `service_port`、`fnspeed.sc` 的 `src/dst.ports`、`ui/config` 的 `port`、`cmd/common` 的 `PORT`。

手动安装等价命令（调试用）：

```bash
mkdir -p /tmp/fpk && tar xzf fnspeed_x86_1.3.0.fpk -C /tmp/fpk
cat /tmp/fpk/manifest          # 核对 appname / platform / service_port
```

## 7. 部署接收端（上传测试必需）

上传测试需要一个接收数据的服务。同机部署：

```bash
sudo install -m755 fnspeed-sink /usr/local/bin/fnspeed-sink
sudo mkdir -p /etc/fnspeed
echo 'FNS_SPEED_TOKEN=换成一个随机串' | sudo tee /etc/fnspeed/sink.env
sudo install -m644 fnspeed-sink.service /etc/systemd/system/
sudo install -m644 99-fnspeed-sink.conf /etc/sysctl.d/
sudo sysctl --system
sudo systemctl daemon-reload && sudo systemctl enable --now fnspeed-sink
curl -s http://127.0.0.1:8977/health
```

然后在应用里填：

- 下载目标：`http://127.0.0.1:8977/dl?bytes=2147483648&token=<TOKEN>`
- 上传目标：`http://127.0.0.1:8977/up?token=<TOKEN>`

跨机测试同理，把 `127.0.0.1` 换成对端地址。

## 8. 验收

```bash
# 端口
ss -lntp | grep -E '8976|8977'

# UI
curl -s -o /dev/null -w '%{http_code} %{size_download}\n' http://127.0.0.1:8976/

# SSE 连通（应持续输出 data: 帧）
timeout 3 curl -sN http://127.0.0.1:8976/api/stream | head -4

# 预检三态
curl -s -X POST http://127.0.0.1:8976/api/probe -H 'Content-Type: application/json' \
  -d '{"urls":["http://127.0.0.1:8977/dl?bytes=104857600&token=T"],"kind":"download"}'
```

性能对照：

```bash
# 终端 A：看网卡真实吞吐
nload -u M  # 或 iftop
# 终端 B：应用内数字（浏览器顶部大号读数）
```

两边误差应 < 5%。超过说明统计窗口和采样节奏不一致，检查是否开启了压缩或代理。

本机回环实测（8 核容器，6 并发双向）：下行 8.0–10.1 GB/s，上行 5.4–7.6 GB/s，失败 0。

## 9. 配置格式

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

## 10. 传输模式判定

预检用 `Range: bytes=0-0` 探测，按响应特征选模式：

| 响应 | 模式 | 行为 |
|---|---|---|
| 206 + Content-Range | `chunk` | 每流请求 32MB 随机起点分片，到尾回绕 |
| 200 + Content-Length | `file` | 整文件循环，下完重连 |
| 200 + 无长度 | `stream` | 流式丢弃，EOF 重连 |
| 403 且带签名 | 降级该条 | 不废整条配置 |
| 404/405 | 剔除 | 端点不可用 |

轮换默认 `sticky`：每条流绑定一条 URL 直到失败，URL 数少于并发数时才复用。CDN 对单流独立限速，轮询会让流频繁换站，吞吐反而下降。

## 10.1 应用中心没有图标？按这个顺序查

| 步骤 | 命令 / 操作 | 判断 |
|---|---|---|
| ① 入口文件在不在 | `ls -l /var/apps/fnspeed/target/ui/config` | 不存在 = 包结构问题（1.2.0 已修，旧包必现） |
| ② 入口内容对不对 | `cat /var/apps/fnspeed/target/ui/config` | `.url` 首个 key 必须 = `fnspeed.Application` |
| ③ 图标在不在 | `ls -l /var/apps/fnspeed/target/ui/images/` | 需要 `icon_64.png` + `icon_256.png` |
| ④ 应用中心缓存 | `systemctl restart trim_app_center.service` | 覆盖安装后入口常不刷新，重启后重进应用中心 |
| ⑤ 服务状态 | `systemctl status trim_app_center --no-pager` | 报 `nil pointer` 说明入口 ID 匹配失败 |
| ⑥ Socket 是否就绪 | `ls -l /var/apps/fnspeed/target/fnspeed.sock` | 不存在 = 网关无法转发，页面白屏 |
| ⑦ 网关是否通 | `curl --unix-socket /var/apps/fnspeed/target/fnspeed.sock http://localhost/app/fnspeed/ -o /dev/null -w '%{http_code}'` | 应为 200 |

①②③ 属于包的问题，④⑤ 属于系统缓存，⑥⑦ 属于运行态。这七步能把「点不开」精确定位到某一层，不用猜。

## 10.2 稳定性：为什么以前会「跑到一半挂掉」「进来又要重选」

**1.3.1 修的三件事**

| 现象 | 原因 | 现在怎么处理 |
|---|---|---|
| 跑着跑着面板打不开，重进又是空的 | Go 里任意后台 goroutine panic 都会带走整个进程，本程序原先没有任何 `recover` | 每条流、调度器、探针、资源采样、SSE 全部套了 `guard`；出事只死那一条 goroutine，进程继续活着，堆栈写进 `var/crash.log` |
| 挂着之后只能手动点启动 | 没有任何自愈 | `cmd/common` 改为守护模式：进程异常退出（rc≠0）2 秒后自动拉起，连续 5 次失败才放弃；`stop` 时先立 `stop.flag`，不会被拉回来 |
| 每次打开都要重选源、重填并发和时长 | 只有点「保存配置」才落盘，切换下拉、改并发都不写 | 切换配置集、改并发/时长/流量、改 URL、切模式全部自动落盘（防抖 800ms–1.5s），开始测速前再存一次 |

**2. 测速中断改了什么**

旧的 `watchStall` 是「零字节 8 秒判死、20 秒不涨收工」。真实网络里目标端限流退避、DNS 抖动、同时重连，几秒不涨很常见，于是频繁误判。现在改成**先自救再判死**：

```
停 15 秒  → 所有流全部重开（新连接绕开半死的旧连接），最多两次
停 30 秒  → 才给结论：「目标端 30 秒无任何数据（已自动重连 2 次）」
开局一直零字节 → 15 秒后判定「源全都连不上」，并提示去点网络自检
```

**3. 自己怎么确认**

- `var/crash.log` 有内容 = 有过内部异常，里面带完整堆栈，直接发我
- `var/app.log` 里出现「进程退出 rc=…，2 秒后自动重启」= 自愈触发过
- 两个文件都在应用的 `var/` 目录下（飞牛：`/var/apps/fnspeed/var/`）

## 11. 已知限制

**零拷贝的实际边界**

- 下载走 `io.Copy(io.Discard, body)` 语义，但 Go 标准库的 splice 只在 `TCPConn → TCPConn` 时启用；dst 是 `io.Discard` 时退化为大 buffer read 循环。本实现用 256KB 复用 buffer，热路径零分配，实测吞吐不受影响，但**不是内核 splice**。
- 上传用 `/dev/zero` + `io.Copy(conn, f)`。`sendfile` 要求输入是普通文件，字符设备会返回 EINVAL，Go 自动回退到 read+write。功能正常，同样**不是 sendfile 零拷贝**。读 `/dev/zero` 由内核直接返回零页，开销可忽略。

**其他**

- 上传固定 `Content-Length: 8GB`，不支持 chunked。需要更长时长请改 `engine.go` 的 `uploadBodyLen`。
- HTTPS 上传会先打满 CPU（TLS 加密），吞吐上限明显低于 http。界面上这类端点会标注。
- `sysctl` 调优需要 root；非 root 时静默跳过，socket buffer 受系统默认 `rmem_max` 约束（通常 208KB），长肥管道下吞吐会受限。
- 性能目标「8 核跑满 10G」只在**本机回环**验证过（8–10 GB/s）。真实 10G 网卡受中断合并、NUMA、PCIe 带宽影响，未在物理 10G 环境验证。
- 前端在 485px 以上验证通过。桌面 Chrome 无头模式的窗口最小宽度就是 485px，390px 视口无法用该方式截图；CSS 内无固定最小宽度，理论上不会溢出。
- 内核调优（`net.core.rmem_max`、`tcp_congestion_control=bbr` 等）由 `install_callback` 在**以 root 执行安装脚本时**写入 `/etc/sysctl.d/99-fnspeed.conf`；应用本体以 `package` 用户运行，不依赖 root。若应用中心的安装脚本非 root 执行，该文件不会生成，需手工执行 README 第 3 节的 sysctl 命令。

## 12. 安全边界

- 启动前必须勾选「目标为自有或已授权资源」，后端同样校验，不只是前端
- 并发硬上限 64，时长硬上限 24h，在 `normalizeProfile` 里钳制，前端改不掉
- 内置 18 个公开测速源（镜像站 / 云厂商 / Vultr），全部是公开可访问的大文件，不涉及任何私有资源
- sink 的 `/dl` 校验 token，单请求上限默认 50GB，防止被当成公开流量源
- sink 未设 token 时启动会打印警告
