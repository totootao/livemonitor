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

# 打开命令追踪，CI 里失败时能从日志看清每一步到底做了什么。
if [ "${VERIFY_EMBED_TRACE:-0}" = "1" ]; then
  set -x
fi

BIN="${1:-bin/livemonitor}"
TMPDIR_LOCAL="$(mktemp -d)"
CONFIG="$TMPDIR_LOCAL/config.json"
INDEX="$TMPDIR_LOCAL/index.html"
SERVER_LOG="$TMPDIR_LOCAL/server.log"
SRV_PID=""
PORT=""
ADDR=""

# 挑一个当前空闲的端口。
#
# 不要用固定端口：runner 上端口占用情况不可控，一旦被占，服务会因监听失败而
# 退出，症状是"服务在就绪前退出"，和真正要校验的嵌入问题毫无关系，排查成本极高。
# 这里用 Python 让内核分配一个空闲端口后立刻释放，再交给被测进程使用。
pick_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

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
  echo "--- 服务日志 ($SERVER_LOG) ---"
  cat "$SERVER_LOG" 2>/dev/null || echo "(无日志)"
  echo "--- 配置内容 ---"
  cat "$CONFIG" 2>/dev/null || echo "(无配置)"
  echo "--- 进程状态 ---"
  echo "SRV_PID=$SRV_PID"
  # 失败时把日志逐行转成注解，保证在受限网络下也能通过 API 取到诊断信息。
  if [ -s "$SERVER_LOG" ]; then
    while IFS= read -r line; do
      echo "::error::log: $line"
    done <"$SERVER_LOG"
  fi
  exit 1
}

if [ ! -x "$BIN" ]; then
  fail "未找到可执行文件: $BIN"
fi

if [ -n "${VERIFY_EMBED_ADDR:-}" ]; then
  ADDR="$VERIFY_EMBED_ADDR"
else
  PORT="$(pick_port)"
  if [ -z "$PORT" ]; then
    fail "无法获取空闲端口（python3 不可用？）"
  fi
  ADDR="127.0.0.1:$PORT"
fi

echo "[verify-embed] 二进制: $BIN"
echo "[verify-embed] 监听地址: $ADDR"
echo "[verify-embed] 临时目录: $TMPDIR_LOCAL"

"$BIN" init -config "$CONFIG" || fail "init 失败"

# 默认配置把监控目录写成 /audio，那是容器内的挂载点。
# 在 CI runner 上这是根目录下的路径，普通用户无权创建，
# 会让 NewProcessor 因 MkdirAll 失败而直接退出（症状是服务"静默"消失，
# 且日志停在打印配置的中间）。这里改写到临时目录，聚焦校验内嵌资源本身。
if command -v python3 >/dev/null 2>&1; then
  python3 - "$CONFIG" "$TMPDIR_LOCAL" <<'PY'
import json, sys
cfg_path, tmp = sys.argv[1], sys.argv[2]
with open(cfg_path, encoding="utf-8") as f:
    cfg = json.load(f)
cfg["watch_dir"] = tmp + "/audio"
cfg["history_dir"] = tmp + "/audio/历史"
with open(cfg_path, "w", encoding="utf-8") as f:
    json.dump(cfg, f, ensure_ascii=False, indent=2)
PY
fi

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
