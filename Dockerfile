# syntax=docker/dockerfile:1

# ---------- 构建阶段 ----------
FROM golang:1.23-alpine AS builder

WORKDIR /src

# GOPROXY 可在构建时覆盖：国内网络下 proxy.golang.org 常常不可达，
# 可传入 --build-arg GOPROXY=https://goproxy.cn,direct。
# 先声明再置空，避免默认值在 COPY 之前被缓存成旧层的环境变量。
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

# 本项目是**纯标准库**实现：go.mod 里一个 require 都没有，go.sum 因而不存在。
# 所以只 COPY go.mod。别再把 go.sum 加回来——文件不存在时 docker build
# 会在计算缓存键那一步直接报 "/go.sum": not found，而且错误信息里
# 看不出是缺文件，只会说 "failed to compute cache key"。
COPY go.mod ./

# 依赖分层缓存用的。没有依赖时 go mod download 是空操作，
# 保留它是为了将来真的加了依赖时这一层仍然成立。
RUN go mod download

# 复制源码。测试文件已由 .dockerignore 排除。
COPY . .

# 版本号由构建参数注入，供 `livemonitor version` 与镜像标签对齐。
# 注意：必须指向 cli.Version（string 变量），const 无法被 -X 覆盖。
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
        -trimpath \
        -ldflags "-s -w -X github.com/totootao/livemonitor/internal/cli.Version=${VERSION}" \
        -o /out/livemonitor ./cmd/livemonitor \
    && /out/livemonitor version

# ---------- 运行阶段 ----------
# 音频转码由 docker/ffmpeg 提供——一个为 MP3 转码专门裁剪的 1.4MB 静态二进制，
# 而不是完整版 ffmpeg。详见 docker/README-ffmpeg.md。
FROM alpine:3.22

ARG VERSION=dev

# 刻意只装最小依赖：
#   tzdata           定时任务依赖正确的时区数据
#   ca-certificates  HTTPS 证书（Docker Engine API 走 unix socket，但用户可能配置 TLS）
#   su-exec          以指定 UID/GID 运行，处理挂载目录属主
# 不装发行版的 ffmpeg：完整版在 Alpine 上要拖进约 130MB 共享库，
# 其中几乎全是本项目用不到的视频编解码器与 GPU 后端。
# 转码只需要 MP3 那一条路径，用裁剪版即可，见下面的 COPY。
RUN apk add --no-cache \
        tzdata \
        ca-certificates \
        su-exec \
    && rm -rf /var/cache/apk/*

# 使用国内时区（可通过 TZ 环境变量覆盖）。
ENV TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder /out/livemonitor /usr/local/bin/livemonitor
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY config.example.json /app/config.example.json

# 裁剪版 ffmpeg（静态链接，无动态库依赖）与它的 LGPL 许可文本。
# 放 /usr/local/bin 下，转码器通过 PATH 查找。
COPY docker/ffmpeg /usr/local/bin/ffmpeg
COPY docker/COPYING.LGPLv2.1 /usr/local/share/doc/ffmpeg/COPYING.LGPLv2.1

RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/ffmpeg

# 媒体目录与归档目录，建议通过卷挂载宿主机路径。
VOLUME ["/audio"]
# 配置文件目录。
VOLUME ["/config"]

ENV LIVEMONITOR_CONFIG=/config/config.json
ENV LIVEMONITOR_LOG_LEVEL=info
# Web 管理界面监听地址；设为 off 可禁用。
ENV LIVEMONITOR_WEB_ADDR=:8080
# Docker Engine API 的 socket 路径。程序用它控制宿主机容器，
# 需要在运行时把宿主机的 /var/run/docker.sock 挂载进来（见 docker-compose.yml）。
ENV DOCKER_HOST=unix:///var/run/docker.sock

# Web 管理界面端口。需在 docker run -p / compose ports 中映射到宿主机。
EXPOSE 8080

# OCI 标准标签，便于在 Docker Hub / 各类注册表中展示来源信息。
LABEL org.opencontainers.image.title="livemonitor" \
      org.opencontainers.image.description="直播容器监控与媒体压缩服务：定时启停 Docker 容器、日志关键词触发停止、纯 Go MP3 压缩与过期文件归档" \
      org.opencontainers.image.source="https://github.com/totootao/livemonitor" \
      org.opencontainers.image.url="https://github.com/totootao/livemonitor" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"

# 健康检查：配置可读且二进制可用即视为健康。
# 使用 `version` 子命令而非 `check`，避免因用户配置错误导致容器被反复重启。
HEALTHCHECK --interval=60s --timeout=10s --start-period=10s --retries=3 \
    CMD livemonitor version || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["run"]