# fnspeed 1.5.1 开工清单

> 状态：**已完工 / 已验证**（2026-11-03）
> 基线：1.5.0（`manifest version` 模板 1.3.0，但 `build.sh` 用 sed 覆写，**不是 bug**）
> 范围：只做「默认配置/自建配置分离 + 预设修正 + UI 修复」，**不做云端配置**（留 1.6.0）

---

## 0. 决策记录（已拍板，执行时不再讨论）

| # | 问题 | 决定 |
|---|---|---|
| D1 | 老用户迁移 | **方案 A**：旧的 `profiles.json` 整体丢弃，改名备份后重建；不尝试识别"哪些是自建" |
| D2 | 用户改默认分组 | **自动 fork**：保存时自动在 user 区复制一份副本，builtin 不动，下次安装照常覆盖 |
| D3 | 选中项悬空 | **方案 A**：回落到列表第一个，并提示"原选中项已移除" |
| D4 | 命中 IP 展示 | **折中**：端点状态卡片加「命中」开关；开=每条源内联显示，关=整行隐藏 |
| D5 | 两个 IP 开关的关系 | 「显示 IP」是总闸（关掉服务端零外发）；「显示命中」是子开关，总闸关了它直接不渲染 |
| D6 | 累计流量 | 必须补回（1.5.0 丢失）；端点状态里的"速率"实际是累计量却标了 `/s`，属 bug |
| D7 | 可读性 | 次要字号 10–11px → 12–13px；灰字提亮；手机端隐藏刻度带 |

---

## 1. 目标产物

- `dist/fnspeed_x86_1.5.1.fpk`
- `dist/fnspeed_arm_1.5.1.fpk`
- `dist/*.md5`
- 三张截图：桌面 / 手机主界面 / 手机端点+命中

`build.sh` 第 9 行 `VER="1.5.0"` → `1.5.1`。

---

## 2. 数据模型变更

### 2.1 文件布局（`TRIM_PKGETC` 下）

| 文件 | 归属 | 安装/升级时 | 说明 |
|---|---|---|---|
| `builtin.json` | 程序 | **无条件整体覆盖** | 默认配置集，代码生成 |
| `user.json` | 用户 | **永不触碰** | 自建 + fork 副本 + 选中项 |
| `settings.json` | 用户 | 保留 | 并发/时长/上限/showIp/showHit（已解耦，不动） |
| `profiles.json` | — | 迁移后改名 | 1.5.0 及以前的合并文件，见 §2.4 |

### 2.2 文件结构

**builtin.json**（程序写，用户不可改）
```json
{
  "format": "PAOMAN_SPEED_TEST_CONFIG",
  "version": 2,
  "hash": "sha256:xxxx",            // DefaultPresets() 序列化后的 sha256，用于判断要不要重写
  "profiles": [ { ...Profile } ]
}
```

**user.json**（用户写）
```json
{
  "format": "PAOMAN_SPEED_TEST_CONFIG",
  "version": 2,
  "selectedProfileId": "cn-huawei",
  "profiles": [ { ...Profile, "forkedFrom": "cn-huawei" } ]
}
```

新增 Profile 扩展字段（`src/config.go:29` 结构体内追加）：
```go
ForkedFrom string `json:"forkedFrom,omitempty"` // 非空表示是默认分组的用户副本
```

### 2.3 合并规则

`Load()` 返回 `builtin.profiles + user.profiles`：

- builtin 在前，user 在后
- **同 ID 以 user 为准**（fork 副本覆盖默认）
- `selectedProfileId` 只在 user.json 里；若指向不存在或已删除的 ID → 回落到合并列表第一个，并在 note 里带提示（D3）

### 2.4 迁移（D1 / 方案 A）

`EnsureSeeded()` 新逻辑，按顺序判断：

```
1. user.json 存在          → 正常加载，结束
2. 旧 profiles.json 存在   → 迁移分支：
     a. os.Rename("profiles.json" → "profiles.json.legacy.bak")
     b. 若 .bak 已存在，先删旧的 .bak（只留一份）
     c. 统计旧条目数 N
     d. 写全新的 builtin.json + 空的 user.json（selected = 第一个预设）
     e. note = "已升级配置结构：清理 %d 个旧配置集（已备份为 profiles.json.legacy.bak）"
3. 都没有                   → 首次安装：写 builtin + 空 user
```

**注意**：迁移只跑一次（因为跑完 user.json 就有了）。`.legacy.bak` 不自动删，留给用户手动处理。

---

## 3. 任务清单

### T1 · Store 双文件拆分

**文件**：`src/store.go`（现 222 行，几乎重写）、`src/presets.go`、`src/api.go`

**现状问题**
- `Store` 只有单一 `path`（`store.go:15-22`）
- `EnsureSeeded`（`:43`）用 `PresetRev` 比较，只在版本号变大时"补"预设，从不删 → 旧残留永久留存
- `RestorePresets`（`:81`）只按 ID 覆盖 + 追加，同样不删

**改法**

```go
type Store struct {
    mu          sync.Mutex
    dir         string
    builtinPath string   // dir/builtin.json
    userPath    string   // dir/user.json
    legacyPath  string   // dir/profiles.json
}
```

函数 | 行为
---|---
`Load()` | 读 builtin + user，合并返回 `*Config`；任一文件损坏 → 用另一份 + note 提示
`EnsureSeeded()` | 按 §2.4 的三分支；builtin 用 `hash` 比对，不同则整体重写
`RestorePresets()` | **只重写 builtin.json**，完全不碰 user.json
`UpsertProfile(p)` | 写 user.json；若 `p.ID` 在 builtin 中存在且内容有差异 → 自动打 `ForkedFrom = p.ID`（D2）
`Delete(ids...)` | 只从 user.json 删；若删的是 fork 副本，builtin 那条会重新出现（这是正确语义：恢复默认）
`SetSelected(id)` | 写 user.json 的 `selectedProfileId`

**废弃**：`presetRevision` 常量、`Config.PresetRev` 字段的**读写逻辑**（字段保留在结构体里做兼容，读出后忽略，不再写）。

**验收**
- 装 1.5.1 → 改一个默认分组 → 再覆盖安装 → 改动还在，且 user.json 里出现 `forkedFrom`
- 删一个 fork 副本 → 该分组回到默认内容
- 手动删除 builtin.json → 重启后自动重建

---

### T2 · 预设重写

**文件**：`src/presets.go`（现 18 条）

**保留不动**：华为 / 阿里 / 网易 / 南大 / TUNA / 中科大 / 北外 / 上交 / 国内八源聚合 / CF / OVH / Vultr×4 / 海外六源 / CF 上传 / sink

**必须删**：小文件、手机厂、电商广告（1.5.0 已无，但要确认旧残留随迁移一起消失）

**待实测后补齐**（旧 URL 已随 1.3.0 源码丢失）：

| 分组 | 目标 | 备注 |
|---|---|---|
| 电信系 | 3–4 条 | 优先找电信各省 IDC 测速文件；找不到就用托管在电信机房的 CDN 大文件 |
| 联通系 | 3–4 条 | 同上 |
| 移动系 | 3–4 条 | 同上 |
| 苹果 CDN | 1–2 条 | 旧值：`updates-http.cdn-apple.com/2019WinterFCS/fullrestores/041-39257/32129B6C-.../iPhone_4.7_12.1.4_16D57_Restore.ipsw`，先测活 |
| 腾讯系 | 2–4 条 | 游戏 APK / 微信安装包直链 |
| 百度系 | 2–3 条 | 网盘静态大文件 |
| 字节系 | 2–3 条 | 抖音/西瓜 CDN 大文件 |
| B站/斗鱼 | 2–3 条 | 视频 CDN 分片 |

**每条必须**：`curl -I` 200 且 `content-length ≥ 50MB`，记录实测速率写进 README。

---

### T3 · 累计流量（D6）

**文件**：`src/web/index.html`

后端**已经齐全**，不用改：`stats.go:249-257` 有 `recvTotal` / `sentTotal` / `avgRecv` / `avgSent`。

**改法**

1. `grid5`（`index.html:294-300`）扩成 `grid6`，加一格：
```html
<div><b class="num" id="gTotal">—</b><span>累计流量</span></div>
```
2. `render()`（`:601`）里新增：
```js
const total = (d.recvTotal||0) + (d.sentTotal||0);
$("gTotal").textContent = total ? fmt.bytes(total) : "—";
```
3. hero 下面加一行细字（可选，桌面显示、手机隐藏）：
`↓ 12.4 GB · ↑ 0.8 GB · 均速 87.2 Mbps`

**验收**：跑到 10GB 停下，数字停在 10 GB 附近，不随速率跳动。

---

### T4 · 可读性（D7）

**文件**：`src/web/index.html`（CSS 在 `:1-280`）

| 项 | 现状 | 改为 |
|---|---|---|
| 次要标签字号 | `10px` / `11px` | `12px` / `13px` |
| 灰字颜色 | 低对比 `--dim` | 提亮约 2 档（保证 AA 对比度 ≥ 4.5:1） |
| 刻度带 `.ticklbl`（`:291`） | 双端都显示 | `@media(max-width:640px){display:none}` |
| `.grid5` 的 `<span>` 标签 | 太小 | 字号 +1，字重 500 |
| 数字字体 | tabular-nums 保留 | 不动 |

**验收**：手机截图缩放 50% 仍能认出每个格子的含义。

---

### T5 · 命中 IP 内联显示（D4/D5）

#### 后端

**a) 运行中零成本采集**

`src/engine.go`：

1. `URLState`（`:42`）加字段：
```go
remoteMu sync.Mutex
remoteIP string
remoteCDN string
```
2. `URLState` 加 setter/getter（仿照 `setErr`/`getErr`）
3. `downloadOnce()`（`:699`）构造 request 时挂 trace：
```go
trace := &httptrace.ClientTrace{
    GotConn: func(info httptrace.GotConnInfo) {
        if info.Conn == nil { return }
        st.setRemote(info.Conn.RemoteAddr().String(), "")
    },
}
req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
```
4. `URLState.out()`（`:74`）返回 `RemoteIP`

**注意**：`httptrace` 只在**每次请求**创建一次 trace，不是每条字节，热路径开销可忽略；连接复用时 `Reused=true` 仍能拿到 `RemoteAddr`。

**b) URLOut 扩展**

`src/stats.go:271-281`：
```go
RemoteIP string `json:"remoteIp,omitempty"`
```

**c) 空闲期批量探测**

`src/ipinfo.go` 新增：
```go
func ProbeHits(ctx context.Context, urls []string, conc int) []HitInfo
```
- 并发度 4，单条超时 5s（`ProbeHit` 内部已有 8s，外层再收紧）
- 结果缓存 10 分钟（key = url），`sync.Map`
- 失败不重试
- 复用现有 `ProbeHit`（`:338`），它已经做了 `Range: bytes=0-0`，只花 1 字节

**d) 新接口**

`src/api.go:61` `Register()` 增加：
```go
mux.HandleFunc("/api/hits", guard("apiHits", a.handleHits))
```
`POST /api/hits`，body `{"urls":["..."]}`，返回 `{hits:[...]}`。
`ShowIP=false` 时直接返回 `{"hidden":true}`，**不发起任何外部请求**。

**e) settings**

`src/settings.go:24`：
```go
ShowHit bool `json:"showHit"`   // 默认 false
```
`defaults()`（`:46`）加 `ShowHit: false`；`load()` 的老文件兼容判断同理（无此键 → false）。

#### 前端

`index.html`：

1. 端点状态标题栏（`:350`）加开关：
```html
<div class="chd"><span>端点状态</span>
  <span class="r"><label class="sw" id="hitSw"><input type="checkbox" id="showHit"><i></i></label>
  <span id="urlTag">尚未运行</span></span></div>
```
2. `render()` 的端点渲染（`:671-681`）改为两行结构：
```js
const hitLine = (GS.showHit && u.remoteIp)
  ? `<div class="hit">${esc(u.remoteIp)}${u.cdn?" · "+esc(u.cdn):""}</div>` : "";
```
3. 开关变化时：开 → `POST /api/hits` 批量探测并回填；关 → **清空 DOM 里的 IP 文本**（不只是 display:none）
4. 总闸联动：`GS.showIp === false` 时，`#hitSw` 整体不渲染
5. 开关状态存 `settings.json`（走现有 `saveSettings/autosaveSettings`）

**验收**
- 运行中断网重连 → 仍显示命中 IP（因为来自 GotConn）
- 24 源全开命中 → 探测耗时 < 40s，界面有进度
- 关总闸 → Network 面板看不到任何 `/api/hits` 请求体带 IP，服务端日志无外发

---

### T6 · Bug 修复

| # | 现象 | 根因 | 修法 |
|---|---|---|---|
| B1 | 端点状态显示 `68.8 GB/s` | `index.html:675` 用 `fmt.rate(u.recv)`，而 `u.recv` 是**累计字节**不是速率 | 改为 `fmt.bytes(u.recv)`，去掉 `/s`；真速率若需要，由后端 `URLOut` 补 `recvRate` 字段（用环形缓冲差分） |
| B2 | `unexpected EOF` 却显示绿点 | `:674` `const st=u.status\|\|"unknown"`，没把 `lastError` 计入 | `const st = u.lastError ? "fail" : (u.status\|\|"unknown")`；同步检查 `.dot.fail` 样式存在 |
| B3 | 上传方向速率取错字段 | `:675` `u.kind==="download"? u.recv : u.sent`，语义对但同样被 B1 影响 | 随 B1 一起改 |

**验收**：跑一次含 403 源的配置 → 红点 + 正确错误文案；累计量不出现 `/s`。

---

## 4. 逐文件改动索引

| 文件 | 改动 | 关联任务 |
|---|---|---|
| `src/store.go` | 重写为双文件 Store，删除 PresetRev 分支 | T1 |
| `src/presets.go` | 删 `presetRevision`，补 8 个分组 | T1/T2 |
| `src/config.go` | `Profile` 加 `ForkedFrom`；`ParseConfig` 忽略 `presetRev` | T1 |
| `src/settings.go` | 加 `ShowHit` + 老文件兼容 | T5 |
| `src/engine.go` | `URLState` 加 remoteIP + trace | T5 |
| `src/stats.go` | `URLOut` 加 `remoteIp` | T5 |
| `src/ipinfo.go` | 加 `ProbeHits` 批量 + 缓存 | T5 |
| `src/api.go` | 加 `/api/hits`；`handleProfiles` 改走 user 区；删 `PresetRev` 赋值 | T1/T5 |
| `src/web/index.html` | 累计流量、可读性、命中开关、单位修复 | T3/T4/T5/T6 |
| `build.sh` | `VER=1.5.1` | 全部 |
| `manifest` | 模板 version 同步 1.5.1（可读性，非必需） | 全部 |
| `README.md` | 补 1.5.1 变更说明 + 新源实测表 | 全部 |

---

## 5. 验收清单

**编译与崩溃**
- [ ] `go vet ./src` 无告警
- [ ] 双架构交叉编译通过
- [ ] 8 轮启停不 panic
- [ ] `var/crash.log` 为空

**配置分离**
- [ ] 全新安装 → builtin.json 生成、user.json 为空
- [ ] 覆盖安装 → builtin 被替换、user 保持
- [ ] 改默认分组 → 自动 fork，user.json 出现 `forkedFrom`
- [ ] 删 fork → 回到默认
- [ ] 旧 profiles.json → 改名 .legacy.bak，36 组垃圾全部消失
- [ ] 选中项被删 → 回落第一个 + 提示

**UI**
- [ ] 累计流量可见且准确
- [ ] 端点状态不再出现 `GB/s`
- [ ] 错误源显示红点
- [ ] 手机端刻度带隐藏，字号可读
- [ ] 命中开关：开→逐条显示，关→文本清空
- [ ] 总闸关闭 → 无任何外发请求

**隐私**
- [ ] IP 不写入 history / app.log / crash.log / builtin.json / user.json

---

## 6. 打包发布

```bash
cd /workspace/fnspeed && ./build.sh   # 产出 dist/*_1.5.1.fpk
md5sum dist/*.fpk > dist/fnspeed_1.5.1.md5
```

---

## 7. 风险与回滚

| 风险 | 概率 | 应对 |
|---|---|---|
| 迁移把用户自建源一起丢了 | 中 | 必须先确认 `profiles.json.legacy.bak` 命名不冲突；保留原文件不删 |
| 双文件并发写竞争 | 中 | 沿用现有 `s.mu` 单锁；builtin 只在启动时写一次 |
| 批量命中探测被目标站风控 | 中 | 缓存 10 分钟 + 4 并发 + 5s 超时；不做自动重试 |
| httptrace 影响吞吐 | 低 | 只在请求构造时挂一次，跑 1000Mbps 对比前后峰值 |
| 预设新源不稳定 | 高 | 全部实测后进包；README 标注实测日期 |

**回滚**：`.fpk` 覆盖安装 1.5.0 即可；`profiles.json.legacy.bak` 改回 `profiles.json` 能恢复旧配置。
