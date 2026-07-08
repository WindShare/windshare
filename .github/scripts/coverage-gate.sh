#!/usr/bin/env bash
# 覆盖率门禁(AGENTS.md 硬性要求:每个 Go 模块 ≥70%,按模块分别计,执行计划 §7)。
# 用法:coverage-gate.sh <coverprofile> <threshold>
#
# 尚无可覆盖语句时(M0 骨架仅 doc.go/空 main)按空集通过:门禁约束的是
# "已写语句的被测比例",不强迫为零语句包编造测试。
set -euo pipefail

profile="$1"
threshold="$2"

if [ ! -f "$profile" ]; then
  echo "coverage profile 不存在:${profile}" >&2
  exit 1
fi

statements=$(grep -c -v '^mode:' "$profile" || true)
if [ "$statements" -eq 0 ]; then
  echo "coverage: 模块暂无可覆盖语句,视为通过(门禁 ${threshold}%)"
  exit 0
fi

total=$(go tool cover -func="$profile" | awk '/^total:/ { sub(/%/, "", $NF); print $NF }')
echo "coverage total: ${total}% (门禁 ${threshold}%)"
awk -v t="$total" -v th="$threshold" 'BEGIN { exit (t + 0 >= th + 0) ? 0 : 1 }' || {
  echo "覆盖率 ${total}% 低于门禁 ${threshold}%" >&2
  exit 1
}
