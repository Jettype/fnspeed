# 提交飞牛应用中心

从「改完代码」到「审核通过」的操作清单。每次发版照做，字段口径不再逐次重新确认。

官方只接受表单提交，没有开发者后台，也没有公开 API。

## 1. 表单与渠道

| 项 | 值 |
|---|---|
| 提交表单 | <https://trim-nas.feishu.cn/share/base/form/shrcn9ewk59TfoFWVAPMPy6aWSc> |
| 官方论坛开发者专区 | <https://club.fnnas.com> → 「应用中心 & Docker」 |
| 商务合作邮箱 | jubao@infinities.com.cn |

表单是飞书的 `contenteditable` 富文本，不是标准 `<input>`，脚本填写要走 `document.execCommand('insertText')`。

## 2. 七条硬规则与 fnspeed 的落点

审核按这七条走。逐条自查，任何一条不过就不提交。

| # | 官方规则 | fnspeed 落点 | 核对方式 |
|---|---|---|---|
| 1 | root 权限应用无法上架 | `config/privilege` 用 `"run-as": "package"` | `cat config/privilege` |
| 2 | 鉴权优先接入统一网关、不监听公网端口 | `main.go` 绑 `127.0.0.1:8976`；`ui/config` 用 `gatewaySocket`；`checkport = false`；`fnspeed.sc` 的 `port_forward="no"` | `grep Addr src/main.go`、`grep checkport manifest` |
| 3 | platform 取值与提交前自测 | 双包：`platform = x86` / `arm` | 两个包分别 `tar xzOf ... manifest` |
| 4 | 文件读写接口需校验路径安全 | 只写自己的 `etc/` 与 `var/`，不接受用户传入路径，不读写 NAS 用户目录 | `grep -n 'filepath.Join\|WriteFile' src/store.go` |
| 5 | 付费内容需在介绍首行声明 | 无付费，不适用 | — |
| 6 | 版本号固定三段式 | 三处同步：`build.sh` 的 `VER`、`manifest` 的 `version`、本文件第 3 节 | `grep '^VER' build.sh; grep '^version' manifest` |
| 7 | 应用名称不得以「飞牛」为前缀 | `display_name = 带宽压测` | `grep display_name manifest` |

**规则 2 是 1.5.5 的主要改动点**，也是最容易踩的：只要进程绑到 `0.0.0.0`，或者 `checkport = true`、
`port_forward = "yes"` 同时开着，都等于对外宣告「我开了公网端口」。三项必须同时是关的。

**规则 6 是第二容易踩的**：应用中心按版本号判断是否需要安装，版本与已装箱相同会被当成同版本，
走不到完整安装链路。每次提交前必须升版本。

## 3. 提交前要做的事

版本号三处同步（漏一处就会带错版本出包）：

```bash
grep '^VER=' build.sh                    # 1
grep '^version ' manifest                # 2
head -3 CHANGELOG.md                     # 3
```

构建与校验：

```bash
bash build.sh
bash tools/fpk_verify.sh                 # 不存在就按下面手动核
```

手动校验要点，两个包都要过：

```bash
tar xzOf dist/fnspeed_x86_1.5.5.fpk manifest | grep -E '^(version|platform|checkport|checksum)'
tar xzOf dist/fnspeed_x86_1.5.5.fpk app.tgz | tar tzf - | grep -E 'ui/config|^\S*fnspeed$'
md5sum dist/*.fpk      # 与 manifest 的 checksum 逐位对比
```

`app.tgz` 里必须能看到 `./ui/config`。看不到就是「应用中心能装、图标点不开」，见
[docs/INSTALL.md](INSTALL.md#4-入口文件到底放在哪最容易踩的坑)。

## 4. 一次性材料（必须真机准备）

| 材料 | 说明 |
|---|---|
| **应用中心详情页截图** | 在真实 fnOS 设备的应用中心打开「带宽压测」详情页截图。仅用于审核，不对外展示，但必须能看清**应用名称与版本号** |
| **微信昵称 / 联系方式** | 表单必填，审核进度通过它沟通。沿用同一账号，方便官方对照 |
| **发布者名称** | 可接收审核反馈的群名或联系人，与上一项可以相同 |

截图不要提交进仓库。装包前先确认 `trim_app_center.service` 已重启，否则详情页可能还是旧版本号。

## 5. 表单字段口径

带 `*` 为必填。文本可直接复制。

| 字段 | 填写内容 |
|---|---|
| 应用名称* | `带宽压测`（与 `manifest` 的 `display_name` 逐字符一致，不带「飞牛」前缀） |
| 系统* | `fnOS` |
| 版本号* | 当前 `manifest` 的 `version`，三段式。**更新上架必须先升版本** |
| 上架类型* | 首次「新应用上架」；已上架后「应用更新」；只换包不改版本选「应用包替换」 |
| 开发者* | `jettype` |
| 发布者* | 见第 4 节 |
| 微信昵称/联系方式* | 见第 4 节 |
| 应用详情页截图* | 见第 4 节 |
| 平台* | `x86和arm`（两个包同时上传） |
| fpk 文件* | `fnspeed_x86_<VER>.fpk` + `fnspeed_arm_<VER>.fpk` |
| 备注 | 见下 |

### 备注文案（首次上架）

```text
本应用是带宽压测工具，用于测出 NAS 所在宽带的真实可用上下行速率，并分段定位瓶颈在
本机、链路还是目标端。目标用户是装了千兆宽带但不知道实际带宽的家用 NAS 用户。

关于合规边界，需要向审核说明三点：

1. 用途限定：仅用于用户自有或已获明确授权的链路。应用内开始压测前必须勾选授权确认，
   未勾选不能启动。
2. 测速源全部是公开软件分发地址：腾讯、阿里、网易、苹果等厂商用于分发安装包的 CDN，
   只做 GET 读取，不做任何写入，也不对接入的任何业务系统发请求。不包含 Ookla 等商业
   测速服务的私有协议。
3. 网络行为：服务只监听 127.0.0.1，通过 fnOS 统一网关提供界面，不监听公网端口，不做
   端口转发；运行身份为 run-as: package，不使用 root。

功能上，除常规单点多线程测速外，还支持把并发流拆到多个源上同时跑，以及按「省 × 运营商」
定向到指定地区的 CDN 节点，用于判断跨省跨网的真实表现。界面为单页内联，不加载任何外部
CDN 资源，NAS 离线环境下也能正常打开。

源码以 MIT 许可开源：https://github.com/Jettype/fnspeed
```

### 备注文案（更新上架）

```text
本次更新内容见 manifest 的 changelog 字段，简述为：<一句话概括本次用户可感知的变化>。

兼容性：升级保留用户自建的测速配置与隐藏列表（user.json 不被触碰）；配置文件格式
从 1.5.1 起为双文件结构，旧版 profiles.json 会自动改名备份为 profiles.json.legacy.bak。
```

## 6. 审核可能的追问与应答

| 追问 | 应答 |
|---|---|
| 「这个能打别人网站，算不算攻击工具？」 | 默认源是厂商公开的软件分发包，单源并发受调度器限制，且启动前有授权确认。用户要测第三方站点必须自己填 URL，属于使用者责任，README 与关于页都有免责声明。 |
| 「为什么还需要联网？」 | 压测的性质决定必须联网取数据，否则测不出带宽。DNS 与 HTTP 全部走系统默认网络栈，没有自建代理或隧道。 |
| 「为什么不走网关，要开端口？」 | 已改。1.5.5 起只监听 127.0.0.1，对外只有网关一条路。 |
| 「跑起来占用多少资源？」 | 空闲时进程常驻约 15 MB 内存、CPU 近 0；满速压测时单核 5–15%，内存随并发线性增长（32 并发约 40 MB），不写磁盘。 |

## 7. 审核通过之后

- 应用先推给内测组，满一周后开放公测。
- 之后每次更新走「应用更新」，版本号必须递增；`manifest` 的 `changelog` 写清本次变更。
- 需要下架或改包，通过表单里的联系方式直接跟官方沟通，没有自助下架入口。

## 变更记录

| 日期 | 变更 |
|---|---|
| 2026-11-04 | 首次编写。为提交应用中心，服务改为只绑回环、关闭 checkport 与端口转发，补齐 manifest 元数据。 |
