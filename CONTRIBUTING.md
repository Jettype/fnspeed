# 参与贡献

先读 [README.md](README.md)，再读 [CHANGELOG.md](CHANGELOG.md) 了解各版本为什么这么改。

## 硬约束（不接受例外）

1. **不引入第三方 Go 依赖**。`go.mod` 只有 `module` 和 `go` 两行，这是刻意的：
   目标机是 NAS，`CGO_ENABLED=0` 静态单二进制才能到处跑，加依赖会让构建链变脆。
2. **不引入前端 CDN**。前端全部内联在 `src/web/index.html`，由 `go:embed` 编进二进制。
   NAS 常年离线或走内网，任何外部资源失效都会让面板变白。
3. **不碰用户数据**。`user.json` 永不被程序改写，也不要把本地 `etc/` 下的真实配置提交上来。
4. **改前端必须同时检查深色与浅色两套令牌**。只改一套必然在另一套里丢对比度。

## 新增测速源的门槛

预设源写在 `src/presets.go`，新加一条必须全部满足：

- `Content-Length ≥ 100 MB`
- 支持 Range 请求
- URL 不带签名 / 时效参数
- 入口尽量用 `latest` 之类别名，不用带版本号的路径（含版本号的几天内必 404）
- 不要用镜像站（华为 / 阿里 / 网易 / 高校）——它们的 WAF 会把 32 并发长连接判成攻击，
  实测 418 / 403 / 503。用软件分发 CDN。

验证命令：

```bash
curl -sI -r 0-1023 <URL> | grep -Ei 'content-length|content-range|^HTTP'
```

## 提交前必跑

```bash
cd src && go vet ./... && go test -short ./...
```

`-short` 会跳过 `TestPinResolveLive` —— 它真发 DoH 请求，依赖阿里 DNS 与某个 CDN 域名存活，
网络一抖就让无关改动变红。要跑联网用例就不加 `-short`。

Windows 上这两条都跑不了：`syscall.SetsockoptInt` 的参数在 Linux 是 `int`、Windows 是
`syscall.Handle`，直接 `go vet ./...` 就报签名不匹配。这是平台差异，不是代码缺陷。
Windows 上只能做交叉编译检查：

```bash
GOOS=linux GOARCH=amd64 go vet ./...
GOOS=linux GOARCH=amd64 go test -c -o /tmp/fnspeed.test .   # 只能编出来，执行不了
```

真正跑测试交给 CI 的 ubuntu runner。

## 测试守着哪些不变量

改调度或统计逻辑时，测试红了先分清是代码错了、还是口径故意变了：

- **稳定值** = 丢掉前 3 秒（`slowStartSec`）与尾部 10% 之后的中位数。改这个等于改
  「稳定值」的定义，会让历史记录不再可比。
- **爬坡**：头 4 秒不做决策；加流后等满 4 秒才比较增益（新连接还在慢启动）；增益门槛 5%；
  连续两轮无增益才锁定拐点。
- **补偿**：巡航期跌破拐点 75% 才触发，4 秒内不重复；补偿后水位最多砸到 85%，
  否则下一轮立刻又触发，形成连环补偿。
- **瞬时速率**必须来自 `Tick` 算好的值，不能在快照里现场差分 —— SSE 紧跟在 `Tick`
  后面触发，现场差分会撞上 `dt≈0`，商恒等于 0，这就是 1.5.x 那个「累计流量在涨、
  大号速率一直 0」的根因。

**不要为了让测试通过就改断言**。那些阈值是拿真实家宽跑出来的，改测试等于骗自己。

另外，提交前跑一次隐私自检，确认没把本机路径、邮箱、私网地址或账号名带进来：

```bash
./tools/check_privacy.sh        # 扫暂存区
./tools/check_privacy.sh --tree # 扫全部已跟踪文件
```

在 Windows 上编辑过的脚本记得确认可执行位还在（`git ls-files -s build.sh` 应为 `100755`），
否则 Linux 上 `./build.sh` 会 Permission denied。

## 版本号要三处同步

`build.sh` 的 `VER` 常量、`manifest` 的 `version`、`README.md` 顶部章节。

`manifest` 里的 `version` 总是旧值也**不影响产物**——`build.sh` 会用 `sed` 覆盖它。
所以**必须走 `build.sh` 打包**，手工 `tar` 会带错版本号。

## 打包

```bash
./build.sh
# 产出 dist/fnspeed_x86_<VER>.fpk 与 dist/fnspeed_arm_<VER>.fpk
```

依赖 Go ≥ 1.21、python3 + Pillow（仅用于生成图标）。产出的 `.fpk` 不要提交进仓库，
挂到 Release 资产上。

注意 `build.sh` 有副作用：会重写 `ICON.PNG` / `ICON_256.PNG` / `ui/icon.png`，
并往 `sink/` 写一份二进制。提交前用 `git status` 确认没有把意外的图标改动带进来。

## 发版

打 tag 即发版，CI 自动打包并挂到 Release 上（`.github/workflows/release.yml`）：

```bash
# 1. 改 build.sh 的 VER、manifest 的 version、README 顶部章节
# 2. 提交后打 tag（必须带 v 前缀，且号码与 VER 完全一致）
git tag -a v1.5.5 -m "fnspeed 1.5.5"
git push origin main --tags
```

tag 与 `build.sh` 的 `VER` 不一致时流程会直接失败，不会产出名字和内容对不上的包。
workflow 还会独立复核：manifest 的 version/platform、manifest 声明的 checksum 是否
等于 `app.tgz` 实际 md5、包内必备条目是否齐全、内嵌程序是否为 64 位 ELF。

需要重发某个已有 tag 时，用 Actions 页面的 `workflow_dispatch` 手动指定 tag，
不要 `git tag -f` 后强推——那样会让别人的克隆里出现分裂历史。

## 免责

本工具用于**自有或已获明确授权**的链路测速。向未授权的站点施压可能违反当地法律
与目标站点的使用条款，责任由使用者承担。
