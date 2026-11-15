#!/usr/bin/env bash
# 校验 src/tune.go 的 sysctlBody 与 cmd/install_callback 的 heredoc 逐字一致。
#
# 两处写的是同一个 /etc/sysctl.d/99-fnspeed.conf。ApplyKernelTuning 靠
# 「把文件读出来和 sysctlBody 比对」决定要不要写：首行注释差一个字，
# 判断就永远不成立，于是每次以 root 启动都白写一次盘 —— 而且没人看得出来。
#
# 用法：./tools/check_sysctl.sh
set -euo pipefail

cd "$(dirname "$0")/.."

die() { echo "sysctl 配置不一致：$*" >&2; exit 1; }

# tune.go：反引号常量，去掉首行的 "const sysctlBody = `" 与结尾的单独反引号行
go_body=$(sed -n '/^const sysctlBody = `/,/^`$/p' src/tune.go | sed '1s/^const sysctlBody = `//;$d')
[ -n "$go_body" ] || die "没能从 src/tune.go 里解析出 sysctlBody"

# install_callback：heredoc，去掉 "cat >... <<'SYSCTL'" 行与结尾的 SYSCTL 行
sh_body=$(sed -n "/cat >\/etc\/sysctl\.d\/99-fnspeed\.conf <</,/^SYSCTL\$/p" cmd/install_callback | sed '1d;$d')
[ -n "$sh_body" ] || die "没能从 cmd/install_callback 里解析出 heredoc 内容"

if [ "$go_body" != "$sh_body" ]; then
  echo "--- src/tune.go ---" >&2
  printf '%s\n' "$go_body" >&2
  echo "--- cmd/install_callback ---" >&2
  printf '%s\n' "$sh_body" >&2
  die "两处内容必须逐字相同（含注释与空行）"
fi

n=$(printf '%s' "$go_body" | wc -c)
echo "sysctl 两处内容一致，$n 字节"
