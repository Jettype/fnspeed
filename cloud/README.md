# 云端清单

程序从远端拉这份 JSON，用它取代编译进二进制的出厂清单。

**当前托管**：主仓库的 `cloud/sources.json`，线上地址

```
https://raw.githubusercontent.com/Jettype/fnspeed/main/cloud/sources.json
```

填进侧栏「云端源」即可（命令行是 `POST /api/settings` 的 `cloudEndpoint`）。
现在 34 个配置集 / 140 条端点。**标了速度的都是用 fnspeed 自己真跑出来的**（并发 2–8、8–10 秒，
本机出口上限约 400Mbps），不是探活就算过。

| 分组 | 内容 |
|---|---|
| 国内 | 腾讯 3 / 阿里 4 / 迅雷 2 / 苹果 1 / 鹰角 1 / 十一源聚合 11 / 厂商大文件 9 / 微软 2 · B站 5 · 字节 1 / **米哈游 6 · 库洛 5 · 完美 1 · 网易 1** / 大文件补充 4 |
| 国外 | Cloudflare / OVH / Vultr 单点 4 / 海外六源聚合 7 / Vultr 亚太 5·欧洲 7·美洲 9·中东非 2 / **DataPacket 亚太 3·欧洲 9·北美 7 / 10GB 档 5 / LibreSpeed 11** |
| 上传 | **LibreSpeed 11** / Cloudflare `__up` / 本机 sink |

### 实测速度（并发 2，单源）

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
| WPS 296MB | 119 Mbps | 金山 KS3 |
| LibreSpeed `garbage.php` | 119 Mbps | 国际源里最快，且能指定大小 |
| Apple IPSW 2975MB | 54 Mbps | http 明文 |
| DataPacket 法兰克福 / 香港 | 36 / 26 Mbps | 19 节点全通但普遍慢 |
| Hivelocity 10GiB | 32 Mbps | |
| truefilesize 1GB | 7 Mbps | 但 CC0 许可最干净 |

2026-11 第三轮：按游戏大厂补的四组（实测速度都很好）

- **库洛 · 鸣潮**（5 条）：PC 客户端 pak 分卷，AWS / 腾讯云 / 火山引擎三条 CDN 挂同一份文件。
  最大的是 18.5GB 的 HD 包。**同一文件三个 CDN 差距很大**（AWS 356、腾讯云 75、火山 51 Mbps），
  所以三条都留。URL 带 `3.7.1` 版本目录和随机哈希，游戏更新即失效，重取方法写在 desc 里：
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
  注意原神 `.001` 会偶发 404（CDN 节点缓存不一致，重试或换卷即可），所以用的是 `.002/.003`
- **DataPacket/CDN77**：官方测速文件，`{三字码}.download.datapacket.com/10000mb.bin`，
  单文件 9.5GB，19 个节点全覆盖。**全是 http 明文**，且实测只有 26–36Mbps，靠节点叠加才有意义
- **LibreSpeed 公共节点**：`garbage.php?ckSize=512` 实时生成 512MB 不可压缩随机流，
  `no-store` 不缓存——不会出现「同一个文件被 CDN 优化过所以测不准」。不支持 Range，
  引擎从头读、读完重连，正好是要的持续流。上传侧同一节点换 `empty.php`，
  PUT/POST 都吃（实测各 200），是除自建 sink 外唯一成规模的上传目标
- **国内大文件补充**：王者荣耀 1.9GB、学习强国 266MB、WPS 296MB、Apple IPSW 3GB。
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
- **带版本号的地址会随厂商发版失效**（百度 7.30.5.2、爱奇艺 10.9.1、.NET SDK 9.0.100）。
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

## 用自己的 123 云盘文件做源

比用腾讯/阿里的公开安装包更站得住脚 —— 那是你自己的文件，符合「自有或已获授权」。

前提：账号开通「开发者权益包」，并在开放平台创建应用拿到 clientID / clientSecret。

```bash
# 凭据放 etc/123pan.json（.gitignore 已覆盖），或走环境变量 PAN123_CLIENT_ID / _SECRET
./tools/123pan.py token --client-id ... --client-secret ...

./tools/123pan.py link 10861131                    # 只看直链
./tools/123pan.py profile 10861131 --id pan123-big # 只看会生成什么条目
./tools/123pan.py add 10861131 --id pan123-big --name "123云盘 · 自有大文件"
```

`add` 会自动取直链、写进 `profiles`、把 `rev` 推到下一个序号。清单原来有签名的话
它会提醒你重跑 `./tools/sign_cloud.py` —— 内容变了旧签名必然失效，不重签客户端会整份拒绝。

没有权益包也能做：在网页版上传文件，用会员的「直链提取」拿到地址，手工粘进 `downloadUrls` 即可。

**把清单本身托管在 123 云盘**也走同一条路（上传 `sources.json` → 取直链 → 填到侧栏「云端源」），
但每次改都要重新上传，fileID 变了直链就跟着变，你还得改一次 endpoint。
适合一次性分发，不适合长期托管。

**压测注意**：直链走 123 自己的 CDN，几十条并发长连接对一个网盘账号来说很像异常流量。
先用 4–8 并发试，盯一下有没有 403 / 429，别一上来就 64。
