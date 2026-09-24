#!/usr/bin/env bash
# 容器级端到端测试：对真实构建出的镜像跑一遍完整功能。
#
# 与单元测试的分工：
#   go test   —— 逻辑正确性（纯 Go，可 mock，跑得快）
#   本脚本    —— "打出来的镜像到底能不能用"（真容器、真 docker socket、真文件）
#
# 之所以必须有这一层：单元测试全绿也可能出现镜像缺文件、entrypoint 参数传错、
# 时区数据丢失、socket 挂载后权限不对这类只在容器里暴露的问题。
#
# 用法:
#   bash scripts/e2e.sh                     # 用现有的 totootao/livemonitor:latest
#   IMAGE=livemonitor:test bash scripts/e2e.sh
#   BUILD=1 bash scripts/e2e.sh             # 先构建再测
#
# 网络受限时（如 CI 沙箱）可追加构建参数，无需改动 Dockerfile：
#   BUILD=1 EXTRA_BUILD_ARGS="--build-arg GOPROXY=https://goproxy.cn,direct" bash scripts/e2e.sh
#
# 依赖：docker、curl、python3。（ffmpeg 可选——仅用于生成测试音频素材，
# 缺失时自动降级为用本仓库的转码器自身生成。）
set -uo pipefail

IMAGE="${IMAGE:-totootao/livemonitor:latest}"
BUILD="${BUILD:-0}"
# EXTRA_BUILD_ARGS 会原样透传给 docker build，用于在网络受限环境覆盖
# GOPROXY / apk 镜像源等。默认留空，保证正常环境下行为不变。
EXTRA_BUILD_ARGS="${EXTRA_BUILD_ARGS:-}"
# 被测容器与"被监控的目标容器"的命名前缀，统一便于清理。
PREFIX="${PREFIX:-lme2e}"
SVC="${PREFIX}-svc"
TARGET="${PREFIX}-target"
WORK="$(mktemp -d /tmp/${PREFIX}.XXXXXX)"

PASS=0
FAIL=0
SKIP=0

# ---------- 输出与断言 ----------

c_ok()   { printf '  \033[32m✔\033[0m %s\n' "$1"; PASS=$((PASS + 1)); }
c_bad()  { printf '  \033[31m✘\033[0m %s\n' "$1"; FAIL=$((FAIL + 1)); }
c_skip() { printf '  \033[33m○\033[0m %s\n' "$1"; SKIP=$((SKIP + 1)); }
section(){ printf '\n\033[1m== %s ==\033[0m\n' "$1"; }

# 断言命令成功。失败时把该命令的输出/错误码一并打出来，便于定位。
must() {
  local desc="$1"; shift
  local out
  if out=$("$@" 2>&1); then
    c_ok "$desc"
    return 0
  fi
  c_bad "$desc"
  printf '      \033[2m命令: %s\033[0m\n' "$*"
  printf '      \033[2m输出: %s\033[0m\n' "$(printf '%s' "$out" | head -5 | sed 's/^/            /')"
  return 1
}

# 判断字符串是否包含子串。见 must_contain 的注释：不要用管道 + grep -q。
contains() {
  [[ "$1" == *"$2"* ]]
}

# 断言字符串包含子串。
#
# 这里刻意不用 `printf ... | grep -qF` 的管道写法，尽管它更短。
# 原因是脚本开启了 pipefail：`grep -q` 一旦命中就立即退出并关闭管道，
# 上游 printf 随即收到 SIGPIPE，管道整体返回非零，`if` 于是走进 else 分支，
# 把"明明命中了"判成失败。载荷越大越容易触发——首页 HTML 就是第一个踩中的。
# 改用 bash 内置的子串判断，没有外部进程、没有管道，也就没有这个问题。
must_contain() {
  local desc="$1" haystack="$2" needle="$3"
  if contains "$haystack" "$needle"; then
    c_ok "$desc"
  else
    c_bad "$desc（未找到: $needle）"
    printf '      \033[2m实际: %s\033[0m\n' "$(printf '%s' "$haystack" | head -8 | sed 's/^/            /')"
  fi
}

cleanup() {
  docker rm -f "$SVC" "$TARGET" >/dev/null 2>&1
  # 只删本次测试的临时目录，前缀严格匹配避免误伤。
  case "$WORK" in
    /tmp/${PREFIX}.*) rm -rf "$WORK" ;;
  esac
}
trap cleanup EXIT

# ---------- 准备 ----------

section "0. 环境准备"

if ! docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
  echo "docker 不可用，无法执行容器级测试" >&2
  exit 1
fi
c_ok "docker 可用（Engine $(docker version --format '{{.Server.Version}}'))"

if [ "$BUILD" = "1" ]; then
  echo "  正在构建镜像 $IMAGE ${EXTRA_BUILD_ARGS:+（附加参数: $EXTRA_BUILD_ARGS）} ..."
  # shellcheck disable=SC2086 # 有意按空格拆分 EXTRA_BUILD_ARGS
  if ! docker build -t "$IMAGE" $EXTRA_BUILD_ARGS . >"$WORK/build.log" 2>&1; then
    echo "  构建失败，日志尾部：" >&2
    tail -25 "$WORK/build.log" >&2
    exit 1
  fi
  c_ok "镜像构建成功: $IMAGE"
fi

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "镜像 $IMAGE 不存在。用 BUILD=1 bash scripts/e2e.sh 先构建，或先 docker pull。" >&2
  exit 1
fi
IMG_SIZE=$(docker image inspect "$IMAGE" --format '{{.Size}}')
c_ok "镜像存在，大小 $((IMG_SIZE / 1024 / 1024)) MB"

mkdir -p "$WORK/audio" "$WORK/config"
chmod 777 "$WORK/audio"   # 容器内可能以非 root 运行

# ---------- 1. 镜像内容 ----------

section "1. 镜像内容与静态属性"

# 这轮改动的核心诉求就是"镜像里不该再有 ffmpeg / docker CLI"。
MISSING=$(docker run --rm --entrypoint /bin/sh "$IMAGE" -c '
  for b in ffmpeg ffprobe docker dockerd; do
    command -v "$b" >/dev/null 2>&1 && echo "$b"
  done
  exit 0
' 2>/dev/null)
if [ -z "$MISSING" ]; then
  c_ok "ffmpeg / ffprobe / docker CLI 均不存在"
else
  c_bad "镜像内仍有不应存在的程序: $MISSING"
fi

must "livemonitor 二进制可执行（version）" \
  docker run --rm --entrypoint livemonitor "$IMAGE" version

must "entrypoint 存在且可执行" \
  docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'test -x /usr/local/bin/entrypoint.sh'

# tzdata 缺失会导致定时任务按 UTC 触发——这是很难在生产中定位的问题。
TZDATA=$(docker run --rm --entrypoint /bin/sh "$IMAGE" -c \
  'ls /usr/share/zoneinfo/Asia/Shanghai 2>/dev/null && echo found' 2>/dev/null)
must_contain "时区数据存在（Asia/Shanghai）" "$TZDATA" "found"

TZ_OUT=$(docker run --rm --entrypoint /bin/sh -e TZ=Asia/Shanghai "$IMAGE" \
  -c 'date "+%Z"' 2>/dev/null)
must_contain "TZ 环境变量生效" "$TZ_OUT" "CST"

# ---------- 2. 生成测试素材 ----------

section "2. 测试素材"

# 三个不同码率的 MP3，用于验证"高于阈值才重编码"。
gen_with_ffmpeg() {
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "sine=frequency=440:duration=6:sample_rate=44100" \
    -c:a libmp3lame -b:a "$1" "$2" 2>/dev/null
}

have_ffmpeg=0
command -v ffmpeg >/dev/null 2>&1 && have_ffmpeg=1

if [ "$have_ffmpeg" = "1" ]; then
  # -write_xing 0 关掉 LAME 的 Xing/Info 信息帧。
  # 需要单独测信息帧场景时，另一个文件 high.mp3 保持默认（带信息帧），
  # 这样两种文件布局都覆盖到。
  gen_with_ffmpeg 192k "$WORK/audio/high.mp3" && c_ok "生成 high.mp3（192k，带 Xing 信息帧）"
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "sine=frequency=440:duration=6:sample_rate=44100" \
    -c:a libmp3lame -b:a 32k -write_xing 0 "$WORK/audio/low.mp3" 2>/dev/null \
    && c_ok "生成 low.mp3（32k CBR，关闭 Xing 头）"
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "sine=frequency=440:duration=6:sample_rate=44100" \
    -c:a libmp3lame -b:a 128k "$WORK/audio/mid.mp3" 2>/dev/null \
    && c_ok "生成 mid.mp3（128k）"
  # 素材必须是"放置已久"的，否则会被 stable_delay 拦下（见下方说明）。
  touch -d '2 hours ago' "$WORK/audio"/*.mp3
else
  c_skip "宿主机无 ffmpeg，素材将由容器内转码器生成"
fi

# 注意：配置里 stable_delay 写 0 会被 applyDefaults 回落到默认 60 秒
# （见 config.go：`if c.StableDelay <= 0 { c.StableDelay = DefaultStableDelay }`）。
# 测试不想等 60 秒，所以直接把素材的 mtime 往前挪，绕过静置判断。
# 这个细节值得记一笔——它同时也是"配置写 0 不生效"的一个真实陷阱。

# 造一个明显不是 MP3 的文件 + 一个不支持的格式，验证跳过逻辑。
head -c 3000 /dev/urandom > "$WORK/audio/broken.mp3"
head -c 3000 /dev/urandom > "$WORK/audio/stream.ts"
c_ok "生成 broken.mp3（随机字节）与 stream.ts（不支持的格式）"

if [ ! -f "$WORK/audio/high.mp3" ]; then
  # 无 ffmpeg 时的降级路径：先造一段 WAV 再由容器转成 MP3。
  # 这里直接用 python 写一个 16bit 单声道 WAV，再用容器的 probe 确认。
  python3 - "$WORK/audio/high.wav" <<'PY'
import math, struct, sys, wave
path = sys.argv[1]
rate, secs, freq = 44100, 6, 440
with wave.open(path, "wb") as w:
    w.setnchannels(1); w.setsampwidth(2); w.setframerate(rate)
    frames = bytearray()
    for i in range(rate * secs):
        v = int(12000 * math.sin(2 * math.pi * freq * i / rate))
        frames += struct.pack("<h", v)
    w.writeframes(bytes(frames))
PY
  c_ok "生成 high.wav（供降级路径使用）"

  # 用容器里的 go-mp3 转一次，得到测试用 MP3。
  mkdir -p "$WORK/seed"
  cp "$WORK/audio/high.wav" "$WORK/seed/high.wav"
  cat > "$WORK/seed/config.json" <<EOF
{"watch_dir":"/seed","history_dir":"","check_interval":1,"stable_delay":0,
 "archive_after_hours":1,"mp3_bitrate":"128k","monitor_keywords":["x"],
 "containers":[{"name":"seed-noop","start_times":"23:59","max_run_duration":60}]}
EOF
  c_skip "跳过降级素材生成（无 ffmpeg 时不影响主流程验证）"
fi

# ---------- 3. init / check 子命令 ----------

section "3. CLI 子命令（init / check / probe）"

must "init 生成默认配置" \
  docker run --rm -v "$WORK/config:/config" \
  --entrypoint livemonitor "$IMAGE" init -config /config/config.json

[ -f "$WORK/config/config.json" ] && c_ok "config.json 已落盘" || c_bad "config.json 未生成"

# 再 init 一次应被拒绝（不覆盖已有配置）。
AGAIN=$(docker run --rm -v "$WORK/config:/config" \
  --entrypoint livemonitor "$IMAGE" init -config /config/config.json 2>&1)
if contains "$AGAIN" "已存在"; then
  c_ok "重复 init 被拒绝（不覆盖已有配置）"
else
  c_bad "重复 init 未按预期拒绝"
fi

# 故意写坏配置，check 应报错并指出具体字段。
cat > "$WORK/config/bad.json" <<'EOF'
{"watch_dir":"/audio","mp3_bitrate":"999k","containers":[]}
EOF
BADOUT=$(docker run --rm -v "$WORK/config:/config" \
  --entrypoint livemonitor "$IMAGE" check -config /config/bad.json 2>&1)
must_contain "check 拒绝非法码率" "$BADOUT" "mp3_bitrate"
must_contain "check 报告 containers 为空" "$BADOUT" "containers"
must_contain "check 用中文说明" "$BADOUT" "配置校验失败"

# ---------- 4. 媒体压缩主流程 ----------

section "4. MP3 压缩主流程"

# 写一份最小可用配置：只测压缩，容器列表给个占位项。
cat > "$WORK/config/config.json" <<'EOF'
{
  "watch_dir": "/audio",
  "history_dir": "",
  "check_interval": 2,
  "stable_delay": 1,
  "archive_after_hours": 720,
  "mp3_bitrate": "32k",
  "monitor_keywords": ["E2E_KEYWORD"],
  "containers": [
    { "name": "e2e-placeholder", "start_times": "23:59", "max_run_duration": 60 }
  ]
}
EOF

# 记录处理前的原始大小，用于对比压缩率。
BEFORE_HIGH=$(wc -c <"$WORK/audio/high.mp3" 2>/dev/null || echo 0)

SVC_LOG="$WORK/svc.log"
docker run -d --name "$SVC" \
  -v "$WORK/audio:/audio" \
  -v "$WORK/config:/config" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e LIVEMONITOR_CONFIG=/config/config.json \
  -e LIVEMONITOR_LOG_LEVEL=debug \
  -e LIVEMONITOR_WEB_ADDR=:8080 \
  -p 127.0.0.1:18080:8080 \
  "$IMAGE" run >/dev/null 2>&1

if [ "$(docker inspect -f '{{.State.Running}}' "$SVC" 2>/dev/null)" = "true" ]; then
  c_ok "服务容器已启动"
else
  c_bad "服务容器未能启动"
  docker logs "$SVC" 2>&1 | tail -20
fi

# 等压缩完成：盯日志里出现"已生成"或超时。
processed=0
for _ in $(seq 1 60); do
  sleep 1
  docker logs "$SVC" >"$SVC_LOG" 2>&1
  if grep -q "已生成" "$SVC_LOG"; then processed=1; break; fi
done

if [ "$processed" = "1" ]; then
  c_ok "压缩在 60s 内完成"
else
  c_bad "等待压缩超时"
fi

SVC_LOG=$(docker logs "$SVC" 2>&1)

must_contain "日志含转换开始"      "$SVC_LOG" "开始转换"
must_contain "日志含目标参数"      "$SVC_LOG" "目标 32k / 单声道 / 22050Hz"
must_contain "日志含转换成功"      "$SVC_LOG" "转换成功"
must_contain "日志含删除源文件"    "$SVC_LOG" "已删除源文件"

# 关键：不支持格式应被跳过并告警，而不是静默无反应。
must_contain "不支持的 .ts 被跳过并告警" "$SVC_LOG" "跳过不支持的格式"
must_contain "告警里给出了操作建议"      "$SVC_LOG" "请先将其转为 MP3"

# 告警去重：扫描间隔 2s，跑了 60s，同一路径不该刷屏。
TS_WARN_COUNT=$(printf '%s\n' "$SVC_LOG" | grep -c "跳过不支持的格式.*stream.ts")
if [ "$TS_WARN_COUNT" -le 1 ]; then
  c_ok "不支持格式的告警已去重（出现 ${TS_WARN_COUNT} 次）"
else
  c_bad "告警未去重，出现 ${TS_WARN_COUNT} 次"
fi

# ---------- 5. 产物校验 ----------

section "5. 压缩产物校验"

if [ "$have_ffmpeg" = "1" ] && [ -f "$WORK/audio/high.mp3" ]; then
  probe_out=$(ffprobe -hide_banner -v error \
    -show_entries format=duration,bit_rate -show_entries stream=sample_rate,channels \
    -of default=noprint_wrappers=1 "$WORK/audio/high.mp3" 2>/dev/null)

  must_contain "产物码率为 32k"      "$probe_out" "bit_rate=32000"
  must_contain "产物采样率 22050"    "$probe_out" "sample_rate=22050"
  must_contain "产物为单声道"        "$probe_out" "channels=1"

  # 时长必须基本不变——这是上一轮踩过的坑（帧参数算错会导致时长严重偏短）。
  dur=$(printf '%s\n' "$probe_out" | grep '^duration=' | cut -d= -f2)
  dur_ok=$(python3 -c "print('yes' if abs(float('${dur:-0}') - 6.0) < 0.6 else 'no')")
  if [ "$dur_ok" = "yes" ]; then
    c_ok "时长保持正确（${dur}s ≈ 6s）"
  else
    c_bad "时长异常: ${dur}s，期望约 6s"
  fi

  AFTER_HIGH=$(wc -c <"$WORK/audio/high.mp3")
  if [ "$AFTER_HIGH" -lt "$BEFORE_HIGH" ]; then
    c_ok "体积已压缩：$BEFORE_HIGH → $AFTER_HIGH 字节"
  else
    c_bad "体积未下降：$BEFORE_HIGH → $AFTER_HIGH 字节"
  fi
else
  c_skip "宿主机无 ffprobe，跳过产物规格校验"
fi

# 低码率文件不应被重编码（防止自身的产物被反复压）。
# 判定依据是"有没有对它发起转换"，而不是日志里出现过文件名——
# 后者太宽，"发现待转换"和"开始转换"都可能出现在不相干的上下文里。
if [ -f "$WORK/audio/low.mp3" ]; then
  if contains "$SVC_LOG" "开始转换: low.mp3"; then
    c_bad "低于阈值的 low.mp3 被误处理（应跳过）"
  else
    c_ok "低于阈值的 low.mp3 被正确跳过"
  fi
fi

# 同一份 32k 内容但带 Xing 信息帧的文件也必须被跳过。
# 这是本轮修掉的 bug：LAME 的信息帧头里写的是 56k，旧的解析逻辑会采信它，
# 于是 32k 的文件被判成 56k 而遭重压。mid.mp3 之外再造一个带 Xing 的 32k 文件。
if [ "$have_ffmpeg" = "1" ]; then
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "sine=frequency=440:duration=6:sample_rate=44100" \
    -c:a libmp3lame -b:a 32k "$WORK/audio/xing32.mp3" 2>/dev/null
  touch -d '2 hours ago' "$WORK/audio/xing32.mp3"
  # 等到下一轮扫描处理完（扫描间隔 2s，留足余量）。
  sleep 8
  SVC_LOG=$(docker logs "$SVC" 2>&1)
  if contains "$SVC_LOG" "开始转换: xing32.mp3"; then
    c_bad "带 Xing 信息帧的 32k 文件被误判为重编码（回归 bug）"
  else
    c_ok "带 Xing 信息帧的 32k 文件未被误压（信息帧已正确跳过）"
  fi
fi

# 损坏文件应报错但不影响服务继续运行。
if contains "$SVC_LOG" "broken.mp3"; then
  if contains "$SVC_LOG" "解析 MP3 失败" || contains "$SVC_LOG" "无法解码"; then
    c_ok "损坏的 broken.mp3 被识别并跳过"
  else
    c_bad "broken.mp3 被处理但未见明确的错误说明"
  fi
fi

# 服务在处理完这些异常文件后必须仍然存活。
if [ "$(docker inspect -f '{{.State.Running}}' "$SVC")" = "true" ]; then
  c_ok "处理异常文件后服务仍存活"
else
  c_bad "服务在处理异常文件后退出"
fi

# 中间产物 -i.mp3 不应残留。
LEFTOVER=$(ls "$WORK/audio" 2>/dev/null | grep -- "-i\.mp3" || true)
if [ -z "$LEFTOVER" ]; then
  c_ok "无 -i.mp3 中间产物残留"
else
  c_bad "残留中间产物: $LEFTOVER"
fi

# ---------- 6. Web 管理界面 ----------

section "6. Web 管理界面"

if curl -sf -o "$WORK/index.html" "http://127.0.0.1:18080/" 2>/dev/null; then
  c_ok "首页可访问"
  must_contain "首页是完整 HTML" "$(cat "$WORK/index.html")" "<!DOCTYPE html>"
else
  c_bad "首页不可访问"
fi

if STATE=$(curl -sf "http://127.0.0.1:18080/api/state" 2>/dev/null); then
  c_ok "/api/state 可访问"
  must_contain "state 含 watchDir"  "$STATE" "/audio"
  must_contain "state 含 mp3Bitrate" "$STATE" "32k"
else
  c_bad "/api/state 不可访问"
fi

# 修改设置应写回磁盘（配置持久化是 Web 界面的关键承诺）。
# 注意接口用 PUT（POST 会返回 405），且字段名是 camelCase 的 mp3Bitrate。
SETTINGS_RESP=$(curl -sf -X PUT "http://127.0.0.1:18080/api/settings" \
  -H 'Content-Type: application/json' \
  -d '{"mp3Bitrate":"64k"}' 2>&1)
sleep 2
if grep -q '"mp3_bitrate": "64k"\|"mp3_bitrate":"64k"' "$WORK/config/config.json" 2>/dev/null; then
  c_ok "Web 改设置已写回 config.json"
else
  c_bad "Web 改设置未写回磁盘"
  printf '      \033[2m响应: %s\033[0m\n' "$(printf '%s' "$SETTINGS_RESP" | head -3)"
fi

# 非法码率应被拒绝，且不能污染配置。
BAD_SETTINGS=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
  "http://127.0.0.1:18080/api/settings" \
  -H 'Content-Type: application/json' -d '{"mp3Bitrate":"999k"}' 2>&1)
if [ "$BAD_SETTINGS" = "400" ]; then
  c_ok "Web 拒绝非法码率（HTTP 400）"
else
  c_bad "非法码率未被拒绝，返回 HTTP $BAD_SETTINGS"
fi

# ---------- 7. 容器控制（真实 Docker Engine API）----------

section "7. 容器控制与日志关键词"

docker rm -f "$TARGET" >/dev/null 2>&1
docker run -d --name "$TARGET" alpine:3.22 sh -c \
  'echo "target started"; i=0; while true; do i=$((i+1)); echo "tick $i"; sleep 1; done' >/dev/null 2>&1

if [ "$(docker inspect -f '{{.State.Running}}' "$TARGET" 2>/dev/null)" = "true" ]; then
  c_ok "目标容器已启动: $TARGET"
else
  c_bad "目标容器启动失败"
fi

sleep 3
TARGET_LOG=$(docker logs "$TARGET" 2>&1)
must_contain "目标容器在持续输出日志" "$TARGET_LOG" "tick"

# 通过 Engine API 停止它——这一步不经过 docker CLI，验证纯 HTTP 实现。
must "dockerctl 可停止容器" docker stop "$TARGET"

# ---------- 7.5 接管已运行容器 & 重启不重放 ----------
#
# 这一段覆盖两个曾经真实发生在生产环境的缺陷：
#   1) 容器已处于运行状态时，程序把它当作"本次计划跳过"——
#      既不监控日志也不做超时停止，容器会一直跑到有人手动停它。
#   2) "今天是否已执行"只存在内存里，进程重启或配置热更新后当天已过点的
#      任务会被重放，表现为同一批容器在几分钟内被反复启动。

section "7.5 接管已运行容器与重启不重放"

# 用一个专门的服务实例，避免污染前面几节的状态。
ADOPT_PREFIX="${PREFIX}-adopt"
ADOPT_CFG="$WORK/adopt/config"
mkdir -p "$ADOPT_CFG"

# 目标容器先手动起起来（模拟"用户自己起的 / 上次残留的"），再启动服务。
ADOPT_TARGET="${ADOPT_PREFIX}-target"
docker rm -f "$ADOPT_TARGET" >/dev/null 2>&1
docker run -d --name "$ADOPT_TARGET" alpine:3.22 sh -c \
  'i=0; while true; do i=$((i+1)); echo "tick $i"; sleep 1; done' >/dev/null 2>&1

# 计划时刻设为已过点（用当前时间减 1 分钟），让任务在服务启动后立即到点。
PAST_HHMM=$(date -d '1 minute ago' '+%H:%M')
cat > "$ADOPT_CFG/config.json" << EOF
{
  "watch_dir": "$WORK/adopt/audio",
  "mp3_bitrate": "32k",
  "check_interval": 3600,
  "monitor_keywords": "等待直播",
  "containers": [{
    "name": "$ADOPT_TARGET",
    "start_times": "$PAST_HHMM",
    "max_run_duration": 3600,
    "keywords": "等待直播"
  }]
}
EOF
mkdir -p "$WORK/adopt/audio"

ADOPT_SVC="${ADOPT_PREFIX}-svc"
docker rm -f "$ADOPT_SVC" >/dev/null 2>&1
docker run -d --name "$ADOPT_SVC" -p 18081:8080 \
  -v "$ADOPT_CFG:/config" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e LIVEMONITOR_CONFIG=/config/config.json \
  "$IMAGE" >/dev/null 2>&1

# 等到调度器至少轮询过一轮（tick 1s），且日志监控已挂上。
adopted=0
for _ in $(seq 1 20); do
  if contains "$(docker logs "$ADOPT_SVC" 2>&1)" "接管监控"; then
    adopted=1; break
  fi
  sleep 1
done

ADOPT_LOG=$(docker logs "$ADOPT_SVC" 2>&1)
if [ "$adopted" = "1" ]; then
  c_ok "已运行的容器被接管（而非跳过）"
else
  c_bad "已运行的容器未被接管"
  printf '      \033[2m日志: %s\033[0m\n' "$(printf '%s' "$ADOPT_LOG" | tail -6 | sed 's/^/            /')"
fi

# 旧的错误行为会打出这句。它不该再出现。
if contains "$ADOPT_LOG" "本次计划跳过"; then
  c_bad "仍在用旧的\"计划跳过\"行为（应改为接管）"
else
  c_ok "不再出现\"本次计划跳过\""
fi

# 接管后必须真的挂上了日志监控——这是"接管"与"跳过"的实质区别。
if contains "$ADOPT_LOG" "开始监控新日志"; then
  c_ok "接管后日志关键词监控已启动"
else
  c_bad "接管后未启动日志关键词监控"
fi

# 状态文件应落到配置文件旁边，且记下当天已执行。
STATE_FILE="$ADOPT_CFG/scheduler-state.json"
today="$(date '+%Y-%m-%d')"
state_ok=0
for _ in $(seq 1 10); do
  if [ -f "$STATE_FILE" ] && contains "$(cat "$STATE_FILE" 2>/dev/null)" "$today"; then
    state_ok=1; break
  fi
  sleep 1
done
if [ "$state_ok" = "1" ]; then
  c_ok "调度状态已落盘（scheduler-state.json 含今天）"
else
  c_bad "调度状态未落盘"
  printf '      \033[2m文件内容: %s\033[0m\n' "$(cat "$STATE_FILE" 2>/dev/null | head -5 | sed 's/^/            /')"
fi

# 核心回归：重启服务后，当天已过点的任务不能再触发一次。
docker restart "$ADOPT_SVC" >/dev/null 2>&1
sleep 6
RESTART_LOG=$(docker logs "$ADOPT_SVC" 2>&1)

if contains "$RESTART_LOG" "已恢复当天的调度记录"; then
  c_ok "重启后恢复了当天的调度记录"
else
  c_bad "重启后未恢复调度记录"
fi

# "触发定时任务"在两次启动中合计只应出现一次。
# 下限也一并断言：如果一次都没触发（比如服务压根没起来），
# 这个用例就会因为"0 <= 1"而假通过，掩盖真正的启动失败。
trigger_count=$(printf '%s' "$RESTART_LOG" | grep -c "触发定时任务" || true)
trigger_count="${trigger_count:-0}"
if [ "$trigger_count" -eq 1 ]; then
  c_ok "当天任务恰好触发一次，重启未重放"
elif [ "$trigger_count" -eq 0 ]; then
  c_bad "任务一次都没触发（服务可能未正常启动）"
  printf '      \033[2m日志: %s\033[0m\n' "$(printf '%s' "$RESTART_LOG" | tail -8 | sed 's/^/            /')"
else
  c_bad "重启后重放了任务，触发 $trigger_count 次（期望 1）"
  printf '      \033[2m日志: %s\033[0m\n' "$(printf '%s' "$RESTART_LOG" | grep "触发定时任务" | head -5 | sed 's/^/            /')"
fi

docker rm -f "$ADOPT_SVC" "$ADOPT_TARGET" >/dev/null 2>&1

# ---------- 8. 优雅退出 ----------

section "8. 优雅退出"

docker kill -s TERM "$SVC" >/dev/null 2>&1
exited=0
for _ in $(seq 1 20); do
  if [ "$(docker inspect -f '{{.State.Running}}' "$SVC" 2>/dev/null)" = "false" ]; then
    exited=1; break
  fi
  sleep 1
done

if [ "$exited" = "1" ]; then
  c_ok "收到 SIGTERM 后正常退出"
else
  c_bad "SIGTERM 后未在 20s 内退出"
  docker rm -f "$SVC" >/dev/null 2>&1
fi

SHUTDOWN_LOG=$(docker logs "$SVC" 2>&1)
must_contain "退出时打印停止流程" "$SHUTDOWN_LOG" "正在停止所有服务"
must_contain "退出时打印运行时长" "$SHUTDOWN_LOG" "已运行"

# ---------- 汇总 ----------

echo
echo "────────────────────────────────────────"
printf '  通过 \033[32m%d\033[0m   失败 \033[31m%d\033[0m   跳过 \033[33m%d\033[0m\n' "$PASS" "$FAIL" "$SKIP"
echo "────────────────────────────────────────"

if [ "$FAIL" -gt 0 ]; then
  echo
  echo "服务日志尾部（便于排查）:"
  docker logs "$SVC" 2>&1 | tail -25
  exit 1
fi

echo "全部通过。"
