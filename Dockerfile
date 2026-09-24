# syntax=docker/dockerfile:1

# ---------- 构建阶段 ----------
FROM golang:1.21-alpine AS builder

WORKDIR /src

# 先复制依赖描述文件以充分利用层缓存（本项目零第三方依赖）。
COPY go.mod ./
RUN go mod download

COPY . .

# 静态编译，关闭 CGO，便于在精简运行时镜像中运行。
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w" \
        -o /out/livemonitor ./cmd/livemonitor

# ---------- 运行阶段 ----------
# 需要 ffmpeg 与 docker CLI，因此基于 alpine 而非 scratch。
FROM alpine:3.20

RUN apk add --no-cache \
        ffmpeg \
        docker-cli \
        tzdata \
        ca-certificates \
        su-exec \
        bash \
    && rm -rf /var/cache/apk/*

# 使用国内时区（可通过 TZ 环境变量覆盖）。
ENV TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder /out/livemonitor /usr/local/bin/livemonitor
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY config.example.json /app/config.example.json

RUN chmod +x /usr/local/bin/entrypoint.sh

# 媒体目录与归档目录，建议通过卷挂载宿主机路径。
VOLUME ["/audio"]
# 配置文件目录。
VOLUME ["/config"]

ENV LIVEMONITOR_CONFIG=/config/config.json
ENV LIVEMONITOR_LOG_LEVEL=info

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["run"]
