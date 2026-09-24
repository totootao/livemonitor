#!/usr/bin/env bash
# 校验二进制里确实内嵌了 Web 管理界面。
#
# 背景：go:embed 的路径写错时，只要目标文件恰好存在就仍能编译通过，
# 问题只在运行期暴露。这里直接起一次服务、拉一次首页，确认页面真的被打进二进制。
#
# 使用独立脚本而非内联 run 的原因：
#   - 后台进程的退出码容易污染步骤的最终状态，需要显式兜住；
#   - 退出时必须无条件清理进程，否则 runner 上会残留监听端口，
#     导致后续重跑（同一 runner 复用）时 bind 失败。
set -uo pipefail

BIN="${1:-bin/livemonitor}"
ADDR="127.0.0.1:18099"
TMPDIR_LOCAL="$(mktemp -d)"
CONFIG="$TMPDIR_LOCAL/config.json"
INDEX="$TMPDIR_LOCAL/index.html"
SERVER_LOG="$TMPDIR_LOCAL/server.log"
SRV_PID=""

cleanup() {
  if [ -n "$SRV_PID" ] && kill -0 "$SRV_PID" 2>/dev/null; then
    kill "$SRV_PID" 2>/dev/null || true
    # 给优雅关闭一点时间，超时则强杀。
    for _ in $(seq 1 20); do
      kill -0 "$SRV_PID" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$SRV_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

fail() {
  echo "::error::$1"
  echo "--- 服务日志 ---"
  cat "$SERVER_LOG" 2>/dev/null || echo "(无日志)"
  exit 1
}

if [ ! -x "$BIN" ]; then
  fail "未找到可执行文件: $BIN"
fi

"$BIN" init -config "$CONFIG" || fail "init 失败"

"$BIN" run -config "$CONFIG" -web "$ADDR" >"$SERVER_LOG" 2>&1 &
SRV_PID=$!

ok=""
for i in $(seq 1 40); do
  sleep 0.5
  # 进程若已退出，继续等待没有意义，直接报错并附上日志。
  if ! kill -0 "$SRV_PID" 2>/dev/null; then
    fail "服务在就绪前退出（第 ${i} 次探测）"
  fi
  if curl -sf -o "$INDEX" "http://$ADDR/"; then
    ok="yes"
    break
  fi
done

[ -n "$ok" ] || fail "无法访问 http://$ADDR/ 首页"

grep -q "<!DOCTYPE html>" "$INDEX" || fail "首页缺少 <!DOCTYPE html>"
grep -q "/api/state" "$INDEX" || fail "首页缺少 /api/state 接口引用"

SIZE=$(wc -c <"$INDEX")
if [ "$SIZE" -lt 4096 ]; then
  fail "首页体积异常偏小: ${SIZE} 字节"
fi

echo "内嵌页面校验通过，体积 ${SIZE} 字节"
