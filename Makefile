BINARY     := livemonitor
BIN_DIR    := bin
CMD_PKG    := ./cmd/livemonitor
VERSION    ?= 1.0.0
IMAGE      ?= totootao/livemonitor
IMAGE_TAG  ?= $(VERSION)
PKG        := github.com/totootao/livemonitor/internal/cli
LDFLAGS    := -s -w -X $(PKG).Version=$(VERSION)

.PHONY: all build test test-race vet fmt lint clean run init check docker docker-run compose-up compose-down help
all: fmt vet test build

## build: 编译单文件二进制到 bin/
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD_PKG)
	@echo "已生成 $(BIN_DIR)/$(BINARY)"

## test: 运行全部单元测试
test:
	go test ./... -count=1

## test-race: 开启竞态检测运行测试
test-race:
	go test -race ./... -count=1

## vet: 静态检查
vet:
	go vet ./...

## fmt: 格式化代码
fmt:
	gofmt -w .
	@echo "格式化完成"

## lint: 检查格式（CI 用，不修改文件）
lint:
	@test -z "$$(gofmt -l .)" || { echo "以下文件未格式化:"; gofmt -l .; exit 1; }
	go vet ./...

## init: 生成默认配置
init:
	go run $(CMD_PKG) init -config config.json

## check: 校验配置
check:
	go run $(CMD_PKG) check -config config.json

## run: 本地运行服务
run:
	go run $(CMD_PKG) run -config config.json -log-level info

## docker: 构建镜像
docker:
	docker build -t $(IMAGE):$(IMAGE_TAG) .

## docker-run: 运行容器（需替换媒体与配置目录）
docker-run:
	docker run -d --name $(BINARY) \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-v /audio:/audio \
		-v $(PWD)/docker-config:/config \
		-e TZ=Asia/Shanghai \
		--restart unless-stopped \
		$(IMAGE):$(IMAGE_TAG)

## compose-up: 使用 docker compose 启动
compose-up:
	docker compose up -d --build

## compose-down: 停止 compose
compose-down:
	docker compose down

## clean: 清理构建产物
clean:
	rm -rf $(BIN_DIR)
	go clean -testcache

## help: 显示帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
