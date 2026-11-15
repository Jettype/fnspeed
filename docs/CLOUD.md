# 云配置（远端测速源）

1.6.0 起，界面里「国内 / 国外 / 上传」那些测速源不再由二进制里的
`presets.go` 决定，而是从一台你自己的 HTTPS 服务器拉下来。改源只要改那份 JSON，
不用重新打包、不用重启进程。

> **默认清单地址**指向本仓库 `main` 分支上的 `cloud/sources.json`：
> `https://raw.githubusercontent.com/Jettype/fnspeed/main/cloud/sources.json`。
> `raw.githubusercontent.com` 在部分网络环境不可达，此时会自动退到本地缓存与出厂清单，
> 压测不受影响；也可以在界面「云端源」里换成自建地址。

- [1. 一次请求长什么样](#1-一次请求长什么样)
- [2. 数据格式](#2-数据格式)
- [3. 签名（可选）](#3-签名可选)
- [4. 降级链](#4-降级链)
- [5. 动态更新](#5-动态更新)
- [6. 与用户配置的关系](#6-与用户配置的关系)
- [7. 部署一份清单](#7-部署一份清单)
- [8. 接口参考](#8-接口参考)
- [9. 排障](#9-排障)

## 1. 一次请求长什么样

```
GET https://<你的域名>/v1/sources.json HTTP/1.1
Accept: application/json
User-Agent: fnspeed/1.6.0
If-None-Match: "<上次的 ETag>"
```

| 项 | 约定 |
|---|---|
| 方法 | 只有 GET，不发任何写请求 |
| 协议 | **必须 https**。`http://` 默认拒绝，只有显式加 `-cloud-allow-http` 才放行（内网调试用） |
| 超时 | 单次总时限 12 秒（DNS 5s / TLS 5s / 响应头 8s） |
| 体积 | 响应体上限 1 MiB，超了整份丢弃 |
| 重定向 | 最多 3 跳，且每一跳都必须是 https |
| 条件请求 | 第二次起带 `If-None-Match`，服务端回 `304 Not Modified` 时只续期不重读 |
| 代理 | 遵循 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` |

成功返回 `200` + JSON。`304` 表示「你手上那份还是最新的」，客户端只把下次刷新
时间往后推，不改任何内容。

地址从哪来（优先级从高到低）：

1. 界面侧栏「云端源」输入框（保存进 `etc/settings.json` 的 `cloudEndpoint`）
2. `POST /api/settings` 的 `cloudEndpoint` 字段
3. 编译期常量 `defaultCloudEndpoint`（`src/cloud.go`）

`cloudDisabled: true` 会彻底关掉远端拉取，只用本地快照与出厂清单 —— 排查
「云端下发了一份坏数据」时用得上。

## 2. 数据格式

```json
{
  "format": "FNS_CLOUD_SOURCES",
  "version": 1,
  "rev": "2026-11-09.1",
  "ttlSec": 21600,
  "issuedAt": 1762660800,
  "expiresAt": 1765267200,
  "minAppVersion": "1.6.0-alpha",
  "mode": "replace",
  "notice": "公共测速源，2026-11 实测",
  "profiles": [
    {
      "id": "cn-tencent",
      "group": "国内",
      "name": "腾讯 CDN · 3源",
      "desc": "王者荣耀安装包 + 微信 PC/x86，腾讯自建分发",
      "mode": "down",
      "downloadUrls": [
        "https://dlied4.myapp.com/myapp/.../sgame.apk",
        "https://dldir1.qq.com/weixin/Windows/WeChatSetup.exe"
      ],
      "uploadUrls": [],
      "concurrency": 16,
      "durationSec": 60,
      "urlRotate": "sticky",
      "switchPolicy": "failover"
    },
    {
      "id": "up-cf",
      "group": "上传",
      "name": "Cloudflare 上传",
      "mode": "up",
      "uploadUrls": ["https://speed.cloudflare.com/__up"]
    }
  ],
  "signature": "9f2c…"
}
```

### 顶层字段

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `format` | string | 是 | 固定 `FNS_CLOUD_SOURCES`，写错整份拒绝 |
| `version` | int | 是 | 清单格式版本，当前只认 `1` |
| `rev` | string | 是 | 修订号，任意字符串（建议日期或递增序号）。**变了才提示「已更新」**，参与签名 |
| `ttlSec` | int | 否 | 缓存有效期，默认 21600（6h），限制在 300–86400 |
| `expiresAt` | int | 否 | Unix 秒。早于本地按 TTL 推算的过期时间时以它为准 —— 云端可主动作废一份清单 |
| `issuedAt` | int | 否 | Unix 秒，仅展示 |
| `minAppVersion` | string | 否 | 客户端版本低于它就拒绝采用（如 `1.6.0`），用于「新清单依赖新功能」的场景 |

> 线上清单当前写的是 `1.6.0-alpha`，这是刻意保留的：它正是第一版支持云配置的客户端，
> 而 semver 下 `1.6.0-alpha < 1.6.0`，正式版能过这道门槛，预发布版也不会被踢下线。
| `mode` | string | 否 | `replace`（默认）或 `append`，见下 |
| `notice` | string | 否 | 一句话备注，显示在状态行 |
| `profiles` | array | 是 | 1–64 个配置集，空数组算非法 |
| `signature` | string | 否 | HMAC-SHA256 十六进制，可带 `hmac-sha256=` 前缀 |

### profiles 里每个配置集

沿用的是「跑满了吗 v2」的配置结构（本实现的超集），所以一份云端清单可以直接
粘贴到 `user.json` 或手机上用。

| 字段 | 必填 | 说明 |
|---|---|---|
| `id` | 是 | 唯一，≤64 字符。**它就是分组的身份**，改名可以，改 ID 等于换了个源 |
| `name` | 否 | 界面显示名，>120 字截断 |
| `desc` | 否 | 说明，>300 字截断 |
| `group` | 否 | 下拉分组（`国内` / `国外` / `上传` / 自建，可自定义） |
| `mode` | 否 | `down` / `up` / `both`，缺省 `both` |
| `downloadUrls` | 否 | 下载地址数组 |
| `uploadUrls` | 否 | 上传地址数组 |
| `downloadEndpoints` | 否 | 带 method / headers / pinRegion 的进阶写法 |
| `uploadEndpoints` | 否 | 同上 |
| `concurrency` / `durationSec` | 否 | 会被全局设置覆盖，填了也不生效，留着是为了格式兼容 |
| `urlRotate` | 否 | `sticky`（默认）/ `round` |
| `switchPolicy` | 否 | `failover`（默认） |

校验规则（任一条不满足**整份拒绝**，不会「丢掉坏的留下好的」）：

- `profiles` 数量 1–64，`id` 非空、唯一、≤64 字符
- 每个配置集至少有 1 个端点，且端点总数 ≤64
- 每条地址必须过 `checkEndpointURL`：`http` / `https`、有主机名、≤4096 字符
- 清单整体 ≤1 MiB

### mode 的两种语义

| 值 | 结果 |
|---|---|
| `replace`（默认） | 云端清单**替代**出厂清单。云端下架一个源，它就真的消失 |
| `append` | 云端清单**追加**在出厂清单之后，同 ID 以云端为准 |

默认 `replace`：如果 `append`，云端删掉的分组会被出厂清单「接住」复活，
「下架」就成了摆设。只有当云端只想补几条额外源、其余仍想用出厂房那份时才用 `append`。

## 3. 签名（可选）

云端清单决定本机接下来向哪些地址发起大流量请求 —— 明文通道上一个中间人就能把
源换成自己的服务器。签名是防这个的。

密钥来源优先级：

1. 启动参数 `-cloud-secret <密钥>`
2. 环境变量 `FNSPEED_CLOUD_SECRET`
3. 文件 `etc/cloud-secret`（推荐，`chmod 600`）

三处都没有 = **不校验**，状态里 `signed: false`。功能照常，只是没有任何完整性保证。

### 签名原文

纯文本行协议，任何语言十行以内就能拼出来（刻意不用 JSON 序列化：键序、空白、
HTML 转义、`omitempty` 各语言不一致，服务端几乎不可能拼出逐字节相同的串）。

```
FNS_CLOUD_SOURCES
1
<rev>
PROFILE<TAB><id>
DOWN<TAB><url>
DOWN<TAB><url>
UP<TAB><url>
PROFILE<TAB><id>
...
```

规则：

- 前 3 行是 `format`、`version`、`rev`
- 每个配置集一行 `PROFILE\t<id>`，随后是它的全部端点，每条一行，`DOWN\t` 或 `UP\t`
- 端点顺序 = `downloadUrls` 在前、`downloadEndpoints` 在后（上传同理），按清单原顺序，不排序
- 行尾统一 `\n`，最后一行也有
- 只签这些字段。`notice`、`ttlSec`、`expiresAt` 不参与 —— 改它们不改变压测打到谁

签名值 = `hex(HMAC-SHA256(secret, 原文))`，填进 `signature` 字段。

### 生成清单的参考实现

```python
import hashlib, hmac, json

SECRET = b"your-shared-secret"

def canonical(doc):
    out = [doc["format"], str(doc["version"]), doc["rev"]]
    for p in doc["profiles"]:
        out.append(f'PROFILE\t{p["id"]}')
        for u in p.get("downloadUrls") or []:
            if u.strip(): out.append(f'DOWN\t{u.strip()}')
        for e in p.get("downloadEndpoints") or []:
            if e["url"].strip(): out.append(f'DOWN\t{e["url"].strip()}')
        for u in p.get("uploadUrls") or []:
            if u.strip(): out.append(f'UP\t{u.strip()}')
        for e in p.get("uploadEndpoints") or []:
            if e["url"].strip(): out.append(f'UP\t{e["url"].strip()}')
    return ("\n".join(out) + "\n").encode()

doc = json.load(open("sources.raw.json", encoding="utf-8"))
doc["signature"] = hmac.new(SECRET, canonical(doc), hashlib.sha256).hexdigest()
json.dump(doc, open("sources.json", "w", encoding="utf-8"),
          ensure_ascii=False, indent=2)
```

Go 侧的实现就是 `CloudSign()`（`src/cloud.go`），两侧对同一份内容必须算出同一个值。

## 4. 降级链

远端不可用**不是错误状态**。压测工具在断网的内网里也得能开工，所以每一级都可用，
只是界面要如实标出现在用的是哪一级。

| 级别 | 状态值 | 触发条件 | 界面显示 | 颜色 |
|---|---|---|---|---|
| 1 | `live` | 本次进程内拉取成功（含 304） | `云端 · rev · 3 分钟前 · 34 个配置集` | 正常 |
| 2 | `cache` | 远端失败或还没拉过，`etc/cloud.json` 可用 | `缓存快照 · rev · 2 小时前 · 34 个配置集` | 黄 |
| 3 | `builtin` | 缓存也没有或不合法 | `出厂兜底 · 15 个配置集（尚未取到云端清单）` | 黄 |
| 4 | `off` | 设置里关掉了远端拉取 | `远端拉取已关闭，使用本地清单` | 正常 |

几条重要的行为：

- **拉到坏数据不会覆盖好缓存**。校验失败只是「这次更新失败」，`lastError` 里记下
  原因，正在用的清单原样保留。宁可旧，不可错。
- **服务器 500 / 超时 / DNS 失败同理**：状态行显示「上次刷新失败：HTTP 500」，
  数据仍来自上一份成功拉取的清单。
- **`etc/cloud.json` 是唯一云端快照**。它损坏就退到出厂清单（`builtin.json`），
  不会出现「半份云端 + 半份出厂」的混合态。
- **`builtin.json` 在云端接管期间不会被改写**。它必须一直是出厂清单，这样云端
  永久不可用时用户点「恢复默认」还能退回去。

## 5. 动态更新

```
启动 ──→ 读 etc/cloud.json（同步，立刻有源可用）
          └─→ 后台立即拉一次
                 ├─ 200  → 校验 → 落盘 → 热切换默认区 → 按 ttlSec 排下一次
                 ├─ 304  → 只续期
                 └─ 失败 → 指数退避 1m → 2m → 4m …（封顶 30m）
```

- 成功后按清单的 `ttlSec` 排下次（默认 6 小时，限制 5 分钟–24 小时）
- 连续失败时退避翻倍，封顶 30 分钟；任何一次成功都会把退避重置
- 换地址会清掉 ETag 并立刻重拉
- 界面上的「立即刷新」= `POST /api/cloud/refresh`，同步等结果（最长约 17 秒）

改一份源要多久生效：**最迟一个 TTL**。想立刻看到就把 `rev` 改掉再点「立即刷新」。
整个过程不动二进制、不重启进程 —— 这就是「不重新打包也能更新」的实现方式。

## 6. 与用户配置的关系

配置分三层，优先级从高到低：

```
user.json   ← 用户自建 / 改过 / 隐藏的
云端清单     ← 远端下发（live / cache 都算）
出厂清单     ← presets.go 编译进二进制，只在降级时出现
```

合并规则和 1.5.x 完全一致，只是「默认区」的来历换了：

| 动作 | 结果 |
|---|---|
| 用户在界面上改某个云端分组 | `user.json` 复制一份并打 `forkedFrom`，之后云端再改也盖不动它 |
| 用户没动过的分组 | 不写进 `user.json`，云端更新时跟着变 |
| 用户删掉某个云端分组 | ID 记进 `user.json` 的 `hidden`，重启不复活 |
| 云端下架一个分组 | 从视图消失（`replace` 模式下不会被出厂清单接住） |
| 点「恢复默认」 | 重置成**当前生效的那一层**（云端清单），清空 hidden 与用户副本 |

## 7. 部署一份清单

清单是纯静态文件，能让 HTTPS 吐出来就行。

**GitHub raw（最省事，当前用的就是这个）**：把 `sources.json` 放进仓库，
地址就是 `https://raw.githubusercontent.com/<owner>/<repo>/<branch>/<path>`。
支持 `ETag` / `304`，`Cache-Control: max-age=300` 正好是「改完 5 分钟内生效」。
限制：仓库必须公开（私有库的 raw 要带 token，客户端不带认证头）；国内直连时通时不通，
NAS 上先 `curl` 验一条。

**对象存储**：把 `sources.json` 传到 COS / OSS / S3 的公有读桶，
拿到 `https://bucket.cos.ap-guangzhou.myqcloud.com/sources.json` 填进界面即可。
注意别用带时效的签名 URL —— 那个地址本身会过期。

**nginx**：

```nginx
server {
  listen 443 ssl;
  server_name cfg.example.com;
  # 客户端会带 If-None-Match，nginx 默认就回 304，不用额外配置
  location /v1/sources.json {
    root /srv/fnspeed-cloud;
    add_header Cache-Control "public, max-age=300";
    default_type application/json;
  }
}
```

**内网自签**：自签证书在客户端会因证书校验失败而拒绝（这是对的）。要么用受信任的
证书，要么临时加 `-cloud-allow-http` 走明文 —— 后者只适合在隔离网段里验证功能。

## 8. 接口参考

### `GET /api/cloud`

```json
{
  "cloud": {
    "state": "live",
    "source": "cloud",
    "endpoint": "https://cfg.example.com/v1/sources.json",
    "rev": "2026-11-09.1",
    "mode": "replace",
    "notice": "公共测速源",
    "fetchedAt": 1762660800,
    "expiresAt": 1762682400,
    "ageSec": 183,
    "ttlSec": 21600,
    "nextFetchAt": 1762682400,
    "profileCount": 15,
    "lastError": "",
    "signed": true,
    "refreshing": false,
    "version": "1.6.0"
  },
  "cachePath": "/var/apps/fnspeed/var/etc/cloud.json"
}
```

### `POST /api/cloud/refresh`

立即拉一次，返回最新的状态与重建后的配置：

```json
{
  "cloud": { "state": "live", "rev": "2026-11-09.2", "…": "…" },
  "config": { "profiles": [] },
  "changed": true,
  "path": "/var/apps/fnspeed/var/etc/user.json",
  "cachePath": "/var/apps/fnspeed/var/etc/cloud.json",
  "error": "可选：本次刷新失败的原因"
}
```

失败也返回 `200` —— 界面要拿状态把「失败原因 + 仍在用哪一级」显示出来。

### `POST /api/settings`

新增两个字段，其余字段语义不变：

| 字段 | 类型 | 说明 |
|---|---|---|
| `cloudEndpoint` | string | 清单地址，留空用编译期默认值 |
| `cloudDisabled` | bool | `true` 彻底关闭远端拉取 |

前端保存设置时会原样回传这两个字段，不会被其它设置项的改动冲掉。

## 9. 排障

**先看界面侧栏那条状态行**，它把「哪一级 + 上次错误」都写在上面了。

命令行（在 NAS 上，服务只监听回环）：

```bash
curl -s http://127.0.0.1:8976/api/cloud | python3 -m json.tool
curl -s -X POST http://127.0.0.1:8976/api/cloud/refresh | python3 -m json.tool
tail -n 50 /var/apps/fnspeed/var/app.log     # 找「云配置生效」
```

| 现象 | 原因 |
|---|---|
| `云端地址必须是 https` | 用了 `http://`。要么上证书，要么加 `-cloud-allow-http` |
| `HTTP 404` | 路径不对，或者对象存储的桶不是公有读 |
| `HTTP 403` | 桶策略 / Referer 防盗链 / WAF 拦了非浏览器 UA |
| `x509: certificate signed by unknown authority` | 自签或根证书缺失 |
| `清单不是合法 JSON` | 大概率是被网关的登录页 / 错误页劫持了，看 `Content-Type` |
| `签名校验失败` | 两侧拼的原文不一致，或者密钥不同。把清单喂给 `docs/CLOUD.md#3` 的 Python 片段复算一次对比 |
| `清单要求最低版本 x` | 云端把 `minAppVersion` 写高了 |
| `配置集 xxx 的端点 yyy 不合法` | 那条 URL 不是 http/https、缺主机名，或超过 4096 字符 |
| 状态一直是「出厂兜底」 | 缓存文件不存在且远端从未成功过，按上面的 `curl` 看具体错误 |
