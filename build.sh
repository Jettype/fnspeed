#!/usr/bin/env bash
# fnspeed 构建脚本：交叉编译双架构并打成 .fpk
# 依赖：go 1.21+、python3 + Pillow（仅用于生成图标）
# 包结构严格遵循 fnOS 规范：
#   manifest / app.tgz / cmd（含 main）/ config / ui（含 config 与 images）/ ICON*.PNG / *.sc
set -euo pipefail

APP="fnspeed"
VER="1.5.5"
ROOT="$(cd "$(dirname "$0")" && pwd)"
SRC="$ROOT/src"
SINK="$ROOT/sink"
BUILD="$ROOT/.build"
DIST="$ROOT/dist"

rm -rf "$BUILD"
mkdir -p "$BUILD" "$DIST"
# 只清旧的包，dist 里的配置清单/截图是交付物，留着
rm -f "$DIST"/fnspeed_*.fpk

echo "==> 生成图标"
if command -v python3 >/dev/null 2>&1; then
  python3 "$ROOT/tools/make_icons.py" || echo "    图标生成失败，沿用已有 ICON"
else
  echo "    未找到 python3，跳过"
fi

echo "==> 编译 sink（本机上传测试用）"
mkdir -p "$BUILD/sink"
for arch in amd64 arm64; do
  ( cd "$SINK" && GOOS=linux GOARCH=$arch CGO_ENABLED=0 \
      go build -trimpath -ldflags="-s -w" -o "$BUILD/sink/fnspeed-sink-$arch" . )
done
cp "$BUILD/sink/fnspeed-sink-amd64" "$ROOT/sink/fnspeed-sink" 2>/dev/null || echo "    (sink 正在运行，跳过覆盖)"

build_pkg() {
  local arch=$1 platform=$2
  local stage="$BUILD/$arch"
  mkdir -p "$stage/target"

  echo "==> 编译 $arch"
  ( cd "$SRC" && GOOS=linux GOARCH=$arch CGO_ENABLED=0 \
      go build -trimpath -ldflags="-s -w" -o "$stage/target/fnspeed" . )

  # ★ 关键：桌面入口必须落在 target/ui/ 下
  #   官方文档 app-entry 明确 desktop_uidir 是「相对于应用目录」的 UI 目录，
  #   fnpack 工程里就是 app/ui/ —— 打包后进 app.tgz，安装后即
  #   /var/apps/{appname}/target/ui/config。
  #   之前只在 fpk 顶层放 ui/，应用中心读不到 → 桌面永远没有入口。
  mkdir -p "$stage/target/ui/images"
  cp "$ROOT/ui/config"     "$stage/target/ui/config"
  cp "$ROOT/ICON.PNG"      "$stage/target/ui/images/icon_64.png"
  cp "$ROOT/ICON_256.PNG"  "$stage/target/ui/images/icon_256.png"
  # 兼容 {0} 占位符可能被替换成 "64"/"256" 之外的连字符写法
  cp "$ROOT/ICON.PNG"      "$stage/target/ui/images/icon-64.png"
  cp "$ROOT/ICON_256.PNG"  "$stage/target/ui/images/icon-256.png"

  # app.tgz 解压到 fnOS 的 target/ 目录，因此内部保持平铺
  echo "==> 打包 app.tgz ($arch)"
  ( cd "$stage/target" && tar czf "$BUILD/app-$arch.tgz" . )

  local pkg="$BUILD/pkg-$arch"
  mkdir -p "$pkg/ui/images" "$pkg/config" "$pkg/cmd"

  # 1. app.tgz
  cp "$BUILD/app-$arch.tgz" "$pkg/app.tgz"

  # 2. cmd：main / install_* / upgrade_* / uninstall_* / config_* + common
  cp "$ROOT"/cmd/* "$pkg/cmd/"
  chmod 755 "$pkg/cmd/"* 2>/dev/null || true

  # 3. config：privilege / resource（resource 里声明 protocol-file 才能放行端口）
  cp "$ROOT/config/privilege" "$pkg/config/privilege"
  cp "$ROOT/config/resource"  "$pkg/config/resource"

  # 4. ui：桌面入口定义 + 图标（两套命名都放，{0} 占位符替换成 64/256 后都能命中）
  cp "$ROOT/ui/config" "$pkg/ui/config"
  cp "$ROOT/ICON.PNG"     "$pkg/ui/images/icon-64.png"
  cp "$ROOT/ICON_256.PNG" "$pkg/ui/images/icon-256.png"
  cp "$ROOT/ICON.PNG"     "$pkg/ui/images/icon_64.png"
  cp "$ROOT/ICON_256.PNG" "$pkg/ui/images/icon_256.png"

  # 5. 防火墙端口规则
  cp "$ROOT/$APP.sc" "$pkg/$APP.sc"

  # 6. 根图标
  cp "$ROOT/ICON.PNG"     "$pkg/ICON.PNG"
  cp "$ROOT/ICON_256.PNG" "$pkg/ICON_256.PNG"

  # 7. manifest：改 platform，注入 app.tgz 的 md5
  local sum
  sum=$(md5sum "$pkg/app.tgz" | cut -d' ' -f1)
  sed -e "s/^platform .*/platform                = $platform/" \
      -e "s/^appname .*/appname                 = $APP/" \
      -e "s/^version .*/version                 = $VER/" \
      -e "s/^checksum .*/checksum                = $sum/" \
      "$ROOT/manifest" > "$pkg/manifest"

  local out="$DIST/${APP}_${platform}_${VER}.fpk"
  ( cd "$pkg" && tar czf "$out" \
      manifest app.tgz cmd config ui ICON.PNG ICON_256.PNG "$APP.sc" )
  echo "    产出 $out ($(du -h "$out" | cut -f1)) md5=$sum"
}

build_pkg amd64 x86
build_pkg arm64 arm

echo
echo "==> 完成"
ls -lh "$DIST"
echo
echo "安装：fnOS 应用中心 → 手动安装 → 选择对应架构的 .fpk"
echo "访问：桌面「带宽压测」图标（走 fnOS 统一网关，服务只监听 127.0.0.1）"
echo "自检：tar xzOf dist/*.fpk app.tgz | tar tzf - | grep ui/config   # 缺了图标点不开"
