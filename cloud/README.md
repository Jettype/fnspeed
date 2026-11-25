# 云端清单

程序从远端拉这份 JSON，用它取代编译进二进制的出厂清单。

> 本文只讲**怎么维护这份清单**：托管在哪、实测数据、改完怎么发。
> 接口形状、数据格式、签名原文、降级链、排障一律见 [docs/CLOUD.md](../docs/CLOUD.md)。

**当前托管**：主仓库的 `cloud/sources.json`，线上地址

```
https://raw.githubusercontent.com/Jettype/fnspeed/main/cloud/sources.json
```

填进侧栏「云端源」即可（命令行是 `POST /api/settings` 的 `cloudEndpoint`）。
现在 43 个配置集 / 163 条端点（`rev` = 2026-11-25.3）。**标了速度的都是用 fnspeed 自己真跑出来的**
（并发 2–8、8–10 秒，测试环境出口带宽明显高于表里所有峰值），不是探活就算过。

| 分组 | 内容 |
|---|---|
| 国内 | 腾讯 4 / 阿里 6 / 迅雷 2 / 苹果 1 / 鹰角 1 / 十四源聚合 14 / **百度 3** / 厂商大文件 5 / **办公影音 7 / 开源镜像 6 / 游戏大分卷 3** / 微软 2 · B站 5 · 字节 1 / **米哈游 6 · 库洛 2 · 完美 1 · 网易 1** / 大文件补充 3 |
| 国外 | Cloudflare / OVH / Vultr 单点 4 / Fastly · CloudFront · Google · Hetzner 5 / **海外八源聚合 8** / Vultr 亚太 5·欧洲 7·美洲 9·中东非 2 / **DataPacket 东京 1·欧洲 9·北美 7 / 10GB 档 5 / LibreSpeed 11 / 海外镜像 2** |
| 上传 | **LibreSpeed 11** / Cloudflare `__up` / 本机 sink |

### 2026-11-25 二轮扩充：四路并行检索

按「国内厂商 / 海外测速与云存储 / 国内镜像与开源分发 / 游戏 CDN」四路并行检索，
汇总 48 条候选（已剔除与现有 92 个域名重复的），先做 HEAD + 1KB Range 双检，
再用本项目口径**单线程 6 秒**复测，对入选项做**第二轮**同口径复测。

| 新增组 | 条数 | 内容 | 两轮实测 |
|---|---|---|---|
| `cn-app` 国内办公影音 | 7 | 钉钉 438MB、飞书 349MB、搜狗输入法 206MB、搜狐影音 142MB、咪咕 108MB、360 浏览器 100MB、115 浏览器 57MB | 3.0–38MB/s |
| `cn-mirror` 国内开源镜像 | 6 | 华为云 spark 382MB / netbeans 486MB / eclipse 331MB / torch 850MB，腾讯云 hadoop 929MB / gradle 130MB | 7–41MB/s |
| `cn-game-big` 游戏大分卷 | 3 | 永劫无间补丁 3.3GB / 2.0GB（网易 `d90.gdl.netease.com`）、雷霆手游整包 1.9GB | 18–45MB/s |
| `intl-mirror` 海外镜像 | 2 | Arch 官方镜像（Rackspace）1.25GB、维基英文 dump 285MB | 3.7–7.5MB/s |

**筛选规则**：两轮都要 ≥3MB/s；同一文件的多镜像只留最快的（6 条 Ubuntu 24.04 镜像跨境
全部低于 2.2MB/s，一条未收）；带过期签名的（JetBrains `Expires=`/`Signature=`）、需 Cookie 的
（Epic API 的 `__cf_bm`）、需 Referer 的一律不收。

**这次的坑**：华为云 ISO 路径会「HEAD 200 + `Accept-Ranges: bytes` + 正确 Content-Length，
但真实 GET 一律 418」——WAF 伪装，只看 HEAD 会把它当好源收进来。所以探测必须用
Range GET 而不是 HEAD。同类排除：清华 TUNA / 北外 BFSU 403、中科大 USTC 返回 JS 挑战页、
上交 Cerberus 挑战、南大 302 死循环、阿里云镜像到家宽仅 0.24–0.29MB/s、暴雪 `level3`
握手 EOF、Epic Akamai 节点超时。

**跨境仍然不能按绝对速度判断**：15 条海外候选里 13 条低于 1MB/s；Firefox（Fastly）与
Epic 首轮 15.3 / 17.3MB/s，第二轮掉到 0.02 / 0.16MB/s —— 单次采样会误判，所以入选项一律
跑两轮。这也是「海外八源聚合」只选八家不同厂商的原因。

### 2026-11-25 百度源更新

百度那两个文件本来就没坏，单线程 43.5 / 34.3MB/s，问题在**版本落后**：清单里还是 7.30.5.2（307MB），
官网已到 8.10.5.101（504MB，42.9MB/s）。百度网盘下载页是 JS 渲染、HTML 里没有直链，要从接口取：

```bash
curl 'https://pan.baidu.com/disk/cmsdata?clienttype=0&web=1&channel=chunlei' \
  -H 'Referer: https://pan.baidu.com/download'
# list[].url 即当前各客户端的 CDN 直链（百度网盘 PC/Mac、库库 AI 等），list[].size 是体积
```

改动：PC 端换 8.10.5.101；补 `pkg-ant.baidu.com` 的库库 AI 客户端 172MB（42.5MB/s）。
百度现在占 3 条，分别落在 **issuepcdn / issuecdn / pkg-ant 三个不同域名**上，
单节点抖动不会让整组归零。配置集 7 → 8 源，端点 144 → 145。

同日晚些时候把这三条**从 `cn-vendor-mix` 里拆出来单列**成 `cn-baidu`「百度 · 3源」：
混在「国内厂商大文件」里时，百度是被当成填充项、而不是被当作品牌源来选；
独立成组后可以直接比较「百度这条链路今天跑多少」，也不会因为删掉和彩云或爱奇艺而误伤百度。
`cn-vendor-mix` 相应收窄为 5 源（和彩云 / 支付宝 / 天翼云 ×2 / 爱奇艺），端点总数不变（163）。

### 2026-11-17 全量复测与清理

140 条端点逐条测（先 8 路并发筛可用性，可疑项再单线程 6 秒复测），端点 140 → 137。

**删除（失效）**

- 网易新闻 APK `static.ws.126.net/.../newsreader_netease_gw.apk` — 403
- 原神 `.003` 分卷 — 404（`.001` 同样 404，CDN 缓存不一致；换 `.004`）
- Cachefly `100mb.test` — 拒连
- DataPacket 香港 — 8.6s 无响应；新加坡 — 0.01MB/s
- WPS 安装包 — 0.10MB/s（金山 KS3 对本机出口限速）

**删除（国内低速，单线程 < 2MB/s）**

- 库洛腾讯云 0.11MB/s、火山引擎 0.22MB/s、Akamai 0.76MB/s（只留 AWS 10.1MB/s）
- 网易视频 MP4 1.28MB/s

**新增（单线程实测）**

- 企业微信 `WeCom_4.1.36.6004.exe` 523MB — 12.1MB/s（腾讯 dldir1）
- npmmirror electron 33 108MB — 7.7MB/s
- Chrome for Testing 131 157MB — 7.8MB/s

**复测要点**

- 单线程下多数"慢"源其实不慢：npmmirror 5.7–10.1、天翼云 5.1/5.2、B站 6.3–12.6、
  微软 6.9/9.4MB/s。首轮 8 路并发会把单条压到 0.1MB/s，**不能用并发数据判源**。
- 镜像站依旧全线不可用：清华/中科大 403、阿里 302 后拒连、华为/腾讯云镜像 404。
- 跨境单条 0.06–15MB/s 属出口瓶颈，不按绝对速度删；OVH 法国 15.2MB/s 是本次最快的一条。

### 实测速度（2026-11-15，并发 2，单源；下表为当时记录，失效项已于 11-17 清理）

| 源 | 速度 | 备注 |
|---|---|---|
| 绝区零分卷 7.4GB | **402 Mbps** | 字节 CDN，最快的一条 |
| 王者荣耀 1968MB | 380 Mbps | 只有「自适应」线路可用 |
| 网易逆水寒 204MB | 384 Mbps | `nsh.gdl.netease.com`，openresty |
| 完美世界 9.0GB | 360 Mbps | `world2dl.wmupd.com` |
| 库洛鸣潮 pak 6.6GB | 356 Mbps | AWS 节点；同文件腾讯云 75、火山 51 |
| 微软 .NET SDK 8.0 | 351 Mbps | azureedge |
| 米哈游原神分卷 10GB | 327 Mbps | 腾讯 COS |
| 星铁分卷 7.0GB | 269 Mbps | |
| 学习强国 266MB | 142 Mbps | `latest/` 路径，不随版本变 |
| WPS 296MB | 119 Mbps | 金山 KS3（11-17 复测跌到 0.10MB/s，已删） |
| LibreSpeed `garbage.php` | 119 Mbps | 国际源里最快，且能指定大小 |
| Apple IPSW 2975MB | 54 Mbps | http 明文 |
| DataPacket 法兰克福 / 香港 | 36 / 26 Mbps | 19 节点全通但普遍慢（香港 11-17 已删） |
| Hivelocity 10GiB | 32 Mbps | |
| truefilesize 1GB | 7 Mbps | 但 CC0 许可最干净 |

2026-11 第三轮：按游戏大厂补的四组（实测速度都很好）

- **库洛 · 鸣潮**（11-17 复测后只留 2 条）：PC 客户端 pak 分卷，AWS / 腾讯云 / 火山引擎三条 CDN
  挂同一份文件。**同一文件三个 CDN 差距很大**（11-15：AWS 356、腾讯云 75、火山 51 Mbps；
  11-17 单线程：AWS 10.1、腾讯云 0.11、火山 0.22MB/s），后两条低于阈值已删，只留 AWS。
  URL 带 `3.7.1` 版本目录和随机哈希，游戏更新即失效，重取方法写在 desc 里：
  先取 `launcher/launcher/50004_…/G153/index.json` 拿 `resourcesBasePath`，
  再取 `…/resource/50004/<version>/indexFile.json` 拿 `dest`，拼 `{cdn}/{basePath}/{dest}`
- **完美世界**（1 条）：`world2dl.wmupd.com` 完整客户端 9.0GB，360Mbps
- **网易**（1 条）：`nsh.gdl.netease.com` 逆水寒 NGP 204MB，384Mbps。
  同域的《永劫无间》完整包有 83GB，但**带防盗链，直连 403**，没收录
- **鹰角**（原有）：`ak.hypergryph.com/downloads/android_lastest` 也是 365Mbps，属于 `latest` 语义

**微软那组做了减法**：VS Code 两条实测只有 22–23Mbps（302 到 Azure Front Door，
国内绕得厉害），已删除，只留 .NET SDK 8.0（351Mbps）和 9.0（102Mbps）。

2026-11 第二轮扩充的四个组：

- **米哈游**：原神/星铁/绝区零的 PC 完整包分卷，单卷 7–10GB，够千兆跑满 60 秒不重连。
  URL 带版本时间戳目录，**游戏更新就失效** —— 用官方 API 拿最新：
  `hyp-api.mihoyo.com/hyp/hyp-connect/api/getGamePackages?launcher_id=jGHBHlcOq1&game_ids[]=<id>`
  （原神 `1Z8W5NHUQb`、星铁 `64kMb5iAWu`、绝区零 `x6znKlJ0xK`）。
  注意原神 `.001` 会偶发 404（CDN 节点缓存不一致）；11-17 复测 `.003` 也 404，现用 `.002/.004`
- **DataPacket/CDN77**：官方测速文件，`{三字码}.download.datapacket.com/10000mb.bin`，
  单文件 9.5GB，19 个节点全覆盖。**全是 http 明文**，且实测只有 26–36Mbps，靠节点叠加才有意义
- **LibreSpeed 公共节点**：`garbage.php?ckSize=512` 实时生成 512MB 不可压缩随机流，
  `no-store` 不缓存——不会出现「同一个文件被 CDN 优化过所以测不准」。不支持 Range，
  引擎从头读、读完重连，正好是要的持续流。上传侧同一节点换 `empty.php`，
  PUT/POST 都吃（实测各 200），是除自建 sink 外唯一成规模的上传目标
- **国内大文件补充**：王者荣耀 1.9GB、学习强国 266MB、Apple IPSW 3GB（WPS 296MB 因 11-17
  复测只有 0.10MB/s 已删）。
  王者荣耀另三条三网定向域名实测不可用：联通 `ml-dlied4` 解析失败、移动 `cdntips` 哈希子域失败、
  电信 `csy.tcdnos` 回 **418 风控**（引擎已把 418 识别为「目标端风控拦截」）


### 第一轮扩充的三个组（微软 / B站 / 字节）

- **微软**：.NET SDK 8.0 / 9.0 各 212MB，`dotnetcli.azureedge.net` 固定版本号，升级时要手动换。
  实测 351 / 102 Mbps。原先还有 VS Code 两条（`update.code.visualstudio.com/latest/...`，
  URL 永久有效、302 到当前版本），2026-11 复测**只有 22–23 Mbps** 已剔除 ——
  那个 302 的落点会漂到 Azure Front Door，落到金山云广州还是绕出国全看运气
- **B站**：`dl.hdslb.com/mobile/latest/...` 同样是「最新版」语义，安卓 64 位 231MB、
  通用 183MB、macOS 182MB、安卓 HD 122MB、必剪 42MB。注意 Windows 客户端
  `bili_win-install.exe` 返回 403，只有移动端和 mac 能直取
- **字节**：只探到抖音 PC 客户端 95MB。飞书 / 剪映 / 豆包的下载页是 JS 渲染，
  真实直链带签名参数、几分钟就失效，不适合放进清单

有两点值得记住：

- **Vultr 会按并发掐**。同一节点打到 8 条以上并发就回 503，串行访问立刻恢复。
  所以拆成 4 个地区组，让 failover 把压力摊开，别指望单节点跑满
- **带版本号的地址会随厂商发版失效**（百度 8.10.5.101、爱奇艺 10.9.1、.NET SDK 9.0.100）。
  这也是把源放云端的理由：换一条推上去就行，不用重新打包。
  用 `latest` 语义的（VS Code、B站）不受影响，只有固定版本号的要盯

## 改完怎么发

1. 改 `profiles`（加、删、换地址）
2. **改 `rev`** —— 不变的话客户端认为没有更新，不会提示也不会重绘
3. 提交推送：

   ```bash
   git add cloud/sources.json
   git commit -m "cloud: 换掉失效的 XX 源"
   git push
   ```

4. GitHub 那边的 CDN 有 5 分钟缓存，客户端最迟一个 `ttlSec`（默认 6 小时）后生效。
   想立刻看到就点侧栏「立即刷新」；缓存还没过的话会拿回旧内容，等几分钟再点

不需要重新打包，不需要重启进程。

## 为什么用 raw 而不是别的

- raw 支持 `ETag` 与 `304`：客户端手上那份没变就不重读，一次刷新只有一个空响应
- `Cache-Control: max-age=300` —— 5 分钟延迟，对配置文件正合适
- jsDelivr 国内确实更快，但 `max-age=604800`（**7 天**），改完要等一周才生效，不能放配置
- 私有仓库不行：raw 对私有库要带 token，而客户端不发送任何自定义认证头
- 国内直连 raw 是时通时不通的。NAS 上先验一条：

  ```bash
  curl -sS -o /dev/null -w "%{http_code} %{time_total}s\n" --max-time 20 \
    https://raw.githubusercontent.com/Jettype/fnspeed/main/cloud/sources.json
  ```

  200 就放心用；超时或 000 就得换 Gitee 或对象存储

## 要不要签名

服务端配了密钥就必须签，否则整份被拒；没配密钥就不校验。

```bash
./tools/sign_cloud.py cloud/sources.json           # 用 etc/cloud-secret 里的密钥，原地写回
./tools/sign_cloud.py cloud/sources.json --verify  # 只校验现有签名
```

签名原文是纯文本行协议，改算法要同时改 `tools/sign_cloud.py` 和 `src/cloud.go` 两处。

## 改之前先看一眼

- 字段说明与降级逻辑：[docs/CLOUD.md](../docs/CLOUD.md)
- 每条地址必须过 `checkEndpointURL`（http/https + 有主机名 + ≤4096 字符），
  任一条不合法**整份**被拒，不是丢掉那一条
- `up-sink` 那条是 `127.0.0.1:8977`，只在 NAS 本机能用，要测得改成局域网 IP
- 并发与时长不写在清单里：它们由界面上的「全局参数」控制，写在这里也不生效

