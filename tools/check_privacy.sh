#!/usr/bin/env bash
# 发布前隐私自检：扫描将要提交/公开的内容里是否混入个人信息。
#
# 用法：
#   ./tools/check_privacy.sh          # 扫当前暂存区（commit 前）
#   ./tools/check_privacy.sh --tree   # 扫工作树全部已跟踪文件
#
# 退出码：0 干净（可能有提醒项）；1 命中阻断项。
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

if [ "${1:-}" = "--tree" ]; then
  mapfile -t FILES < <(git ls-files)
else
  mapfile -t FILES < <(git diff --cached --name-only --diff-filter=ACM)
fi

if [ "${#FILES[@]}" -eq 0 ]; then
  echo "没有待检查的文件。"
  exit 0
fi

# 本脚本自身的检测规则里必然含有关键字，跳过它自己
_keep=()
for _f in "${FILES[@]}"; do
  [ "$_f" = "tools/check_privacy.sh" ] || _keep+=("$_f")
done
FILES=("${_keep[@]}")

block=0
warn=0

# hits <正则> <白名单正则> —— 用 grep -H 一次扫全部文件
hits() {
  grep -nIEH -- "$1" "${FILES[@]}" 2>/dev/null | grep -vE -- "${2:-^$}"
}

block_on() {
  local out
  out=$(hits "$1" "${3:-^$}")
  if [ -n "$out" ]; then
    echo "✗ $2"
    printf '%s\n' "$out" | head -20
    echo
    block=1
  fi
}

warn_on() {
  local out
  out=$(hits "$1")
  if [ -n "$out" ]; then
    echo "⚠ $2（确认是通用示例而非本机地址）"
    printf '%s\n' "$out" | head -10
    echo
    warn=1
  fi
}

echo "==> 阻断项"

# 邮箱：排除 HTML 里的 @media / @font-face 等 CSS at-rule
block_on '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' \
  "出现邮箱地址" \
  '@(media|font|keyframes|supports|import)|users\.noreply\.github\.com'

# 本机绝对路径：Windows 用户目录、Linux 家目录、常见挂载点
block_on '([A-Za-z]:\\?[Uu]sers|/workspace/|/home/[a-z_][a-z0-9_-]*/|/Users/[a-z]|AppData|/mnt/[a-z]|/vol[0-9])' \
  "出现本机绝对路径"

# 硬编码凭据与 token
block_on '(api[_-]?key|secret|passwd|password)["]?\s*[:=]\s*["][^"]{8,}' "疑似硬编码凭据"
block_on 'sk-[A-Za-z0-9]{20,}|ghp_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}' "疑似 API token"

# 个人账号名（go.mod 的模块路径是必需的，放行）
block_on 'jettype|sakuta|坂田' "出现个人账号名" \
  'go\.mod:[0-9]+:module github\.com/jettype/fnspeed'

echo "==> 提醒项"

warn_on '\b(192\.168\.[0-9]{1,3}\.[0-9]{1,3}|10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3})\b' \
  "出现私网地址"

echo "==> 结果"
if [ "$block" -eq 1 ]; then
  echo "存在阻断项，先处理再提交。"
  exit 1
fi
if [ "$warn" -eq 1 ]; then
  echo "无阻断项；提醒项已列出，逐条确认后即可提交。"
else
  echo "干净。"
fi
