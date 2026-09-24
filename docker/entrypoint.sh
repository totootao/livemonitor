#!/bin/sh
# entrypoint.sh - 容器入口脚本
# 职责：
#   1. 配置缺失时自动生成默认配置；
#   2. 若挂载了 docker socket 且以 root 运行，直接启动；
#   3. 通过 su-exec 支持以指定 UID/GID 运行，便于处理挂载目录的属主问题。

set -eu

CONFIG_PATH="${LIVEMONITOR_CONFIG:-/config/config.json}"
LOG_LEVEL="${LIVEMONITOR_LOG_LEVEL:-info}"
WEB_ADDR="${LIVEMONITOR_WEB_ADDR:-:8080}"

mkdir -p "$(dirname "$CONFIG_PATH")"

if [ ! -f "$CONFIG_PATH" ]; then
    echo "[entrypoint] 未找到配置 $CONFIG_PATH，正在生成默认配置..."
    livemonitor init -config "$CONFIG_PATH"
fi

echo "[entrypoint] 使用配置: $CONFIG_PATH"
echo "[entrypoint] 日志级别: $LOG_LEVEL"
echo "[entrypoint] Web 管理界面: ${WEB_ADDR:-已禁用}"
echo "[entrypoint] 本地时间: $(date '+%Y-%m-%d %H:%M:%S %Z')"

# 检查 docker socket 是否可用，缺失时仅告警（媒体转码仍可工作）。
if [ ! -S /var/run/docker.sock ]; then
    echo "[entrypoint] 警告: 未检测到 /var/run/docker.sock，容器控制功能将不可用"
fi

# 检查转码依赖的 ffmpeg 是否存在。
# 只告警不退出：没有它程序仍能跑（容器定时启停、日志监控、归档都不受影响），
# 只是 MP3 压缩这一步会失败。直接退出反而会把"少了个二进制"升级成"服务起不来"。
if ! command -v ffmpeg >/dev/null 2>&1; then
    echo "[entrypoint] 警告: 未找到 ffmpeg，MP3 压缩功能将不可用（容器与归档功能不受影响）"
fi

# 仅在 run 子命令上追加 -web，避免 init/check 等命令因未知 flag 报错。
set -- "$@"
case "${1:-run}" in
    run) set -- "$@" -web "$WEB_ADDR" ;;
esac

if [ -n "${PUID:-}" ] && [ -n "${PGID:-}" ]; then
    echo "[entrypoint] 以 UID=${PUID} GID=${PGID} 运行"
    exec su-exec "${PUID}:${PGID}" livemonitor "$@" -config "$CONFIG_PATH" -log-level "$LOG_LEVEL"
fi

exec livemonitor "$@" -config "$CONFIG_PATH" -log-level "$LOG_LEVEL"
