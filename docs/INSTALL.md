# 安装与部署

装机、桌面入口、接收端与自检。跑不出速度、点不开图标，基本都在这一篇里。

- [1. 安装](#1-安装)
- [2. 端口](#2-端口)
- [3. 桌面入口：为什么用网关模式](#3-桌面入口为什么用网关模式)
- [4. 入口文件到底放在哪（最容易踩的坑）](#4-入口文件到底放在哪最容易踩的坑)
- [5. 应用中心没有图标](#5-应用中心没有图标)
- [6. 手机里显示发虚 / 元素忽大忽小](#6-手机里显示发虚--元素忽大忽小)
- [7. 部署接收端（上传测试必需）](#7-部署接收端上传测试必需)
- [8. 从源码构建](#8-从源码构建)
- [9. 验收](#9-验收)

## 1. 安装

1. fnOS 应用中心 → 手动安装 → 选择对应架构的 `.fpk`
2. 安装后从桌面「带宽压测」图标进入（走 fnOS 统一网关）
3. 首次打开是空列表（不内置任何压测目标）

> **不再提供 `http://<NAS-IP>:8976` 直连。** 1.5.5 起服务只绑 `127.0.0.1`，对外入口只有网关一条
> —— 应用中心要求「不监听公网端口」，绑 `0.0.0.0` 无法过审。要在别的机器上调试界面，用
> `http://<飞牛域名>/app/fnspeed`；要在本机调试，SSH 进去后 `curl http://127.0.0.1:8976/` 照常可用。

手动安装等价命令（调试用）：

```bash
mkdir -p /tmp/fpk && tar xzf fnspeed_x86_1.5.5.fpk -C /tmp/fpk
cat /tmp/fpk/manifest          # 核对 appname / platform / service_port
```

## 2. 端口

端口选 **8976**，刻意避开常见的 8876（qBittorrent WebUI 默认占用其同一网段端口）与 8080/8096 等媒体服务端口。

服务只绑 `127.0.0.1:8976`，因此 `manifest` 里 `checkport = false`、`fnspeed.sc` 里 `port_forward="no"`
—— 这两项都是给「需要从局域网直连端口」的应用准备的，本项目走网关，不需要，留在开启状态会被审核判为
「监听公网端口」。

要改端口需同步改三处：`manifest` 的 `service_port`、`fnspeed.sc` 的 `src/dst.ports`、`cmd/common` 的 `PORT`。
`ui/config` 的 `port` 保持空字符串（网关模式不读这个字段）。

## 3. 桌面入口：为什么用网关模式

飞牛有两套入口机制，**只有第二套能在手机 App 和 FN Connect 远程打开**：

| 模式 | 配置 | 结果 |
|---|---|---|
| 端口直连（**本项目未启用**） | `protocol: http` + `port: "8976"` + `url: "/"` | 桌面点开 = 浏览器跳 `http://NAS:8976`。手机 App 内不内嵌；FN Connect 的 HTTPS 页面里嵌 HTTP 会被浏览器按 mixed-content 拦掉。且服务已不监听非回环地址，这条路已走不通 |
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

服务端配合做了四件事：

- **双通道监听**：TCP `127.0.0.1:8976`（仅回环，供本机调试与 `cmd/common` 的健康探测）+ Unix Socket `$TARGET/fnspeed.sock`（网关，权限 0770）。
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

## 4. 入口文件到底放在哪（最容易踩的坑）

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

## 5. 应用中心没有图标

按这个顺序查：

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

## 6. 手机里显示发虚 / 元素忽大忽小

内嵌 WebView 和普通浏览器不是一回事，踩过三个坑，都已处理：

| 坑 | 现象 | 处理 |
|---|---|---|
| 飞牛 App 的 WebView 自行缩放 | 文字发虚，1 个 CSS 像素对不上整数个物理像素 | viewport 显式锁死 `maximum-scale=1, minimum-scale=1, user-scalable=no` |
| Android **Font Boosting** | 文字被系统额外放大，卡片比例全乱 | `html{-webkit-text-size-adjust:100%;text-size-adjust:100%}` |
| 字号用 `vw` / `clamp()` | 算出 `13.87px` 这种小数，高 DPI 屏渲染发虚 | 全部改成整数 px，桌面端用 `@media` 断点放大 |

另外曲线画布按 `devicePixelRatio` 缩放（上限 2），空态不再画假刻度。

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

## 8. 从源码构建

```bash
bash build.sh
```

产出 `dist/fnspeed_x86_<VER>.fpk` 与 `dist/fnspeed_arm_<VER>.fpk`。

依赖 Go ≥ 1.21、python3 + Pillow（仅用于生成图标）。无第三方 Go 依赖，不需要 `go mod download`。

脚本会依次：生成图标 → 交叉编译 sink → 分别编译 amd64/arm64 → 打 app.tgz → 改 manifest 的
`platform` 并注入 `checksum`（app.tgz 的 md5）→ 打 .fpk。

包结构按 fnOS 规范强制包含：`manifest`、`app.tgz`、`cmd/`（含 `main`）、`config/`（`privilege` +
`resource`，后者用 `port-config.protocol-file` 指向 `fnspeed.sc` 才会放行端口）、`ui/config`
桌面入口、`ui/images/{64,256}.png`、根图标、`fnspeed.sc`。缺任意一项，应用中心会报「应用数据包不完整」。

> `build.sh` 有副作用：会重写三个图标文件并往 `sink/` 写一份二进制。提交前用 `git status` 确认。

## 9. 验收

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
