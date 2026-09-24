# livemonitor

用 Go 重写的直播容器监控与媒体转码服务。原 Python 脚本（`livemonitor.py`）依赖
`schedule`、`mutagen`、`shutil` 等第三方库；Go 版本**零第三方依赖**，标准库即可编译出单文件二进制。

功能与原脚本一致：

1. **定时启动容器** — 每个容器可配置多个每日启动时刻，到点自动 `docker start`。
2. **日志关键词监控** — `docker logs -f` 实时跟踪，命中关键词（默认 `等待直播`）立即 `docker stop`。
3. **最大运行时长保护** — 到达设定时长后自动停止容器。
4. **媒体转码** — 监控目录中的视频/音频文件用 ffmpeg 转为 MP3（单声道 / 22050Hz）。
5. **过期文件归档** — 早于阈值的 MP3 自动移入 `历史` 子目录。

## 与原脚本的对应关系

| Python 实现 | Go 实现 | 说明 |
| --- | --- | --- |
| `schedule.every().day.at()` | `internal/scheduler` | 按秒轮询，按「日期 + 时刻」去重，避免同一时刻重复触发 |
| `mutagen.mp3.MP3().info.bitrate` | `internal/media/mp3.go` | 自行解析 MPEG 帧头，跳过 ID3v2 标签，含二次帧校验防误判 |
| `subprocess.run/Popen` | `internal/dockerctl`、`internal/media` | 全部改为 `context` 驱动，支持超时与优雅取消 |
| `threading.Thread` + `Event` | goroutine + `context.Context` | 用 context 取消替代 Event 信号，避免竞态 |
| `re.compile(r'[\x00-\x1F\x7F]')` | `monitor.CleanLogLine` | 逐 rune 扫描，保留制表符 |
| `shutil.move` | `media.moveFile` | 先 `rename`，跨分区时回退为复制 + 删除 |
| 硬编码 `CONTAINERS_CONFIG` | `config.json` | 配置外置，支持校验、容器级关键词覆盖 |

## 快速开始

```bash
# 1. 生成配置
go run ./cmd/livemonitor init -config config.json

# 2. 校验配置（会打印解析结果与生效参数）
go run ./cmd/livemonitor check -config config.json

# 3. 启动服务
go run ./cmd/livemonitor run -config config.json -log-level info
```

编译单文件二进制：

```bash
make build            # 产出 bin/livemonitor
```

## 命令行

```
livemonitor run     启动监控服务
livemonitor init    生成默认配置文件
livemonitor check   校验配置并打印摘要
livemonitor probe   查看 MP3 文件码率等参数
livemonitor version 查看版本
```

通用选项：`-config <路径>`、`-log-level debug|info|warn|error`。

## 配置说明

见 [`config.example.json`](config.example.json)。关键字段：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `watch_dir` | `/audio` | 媒体监控目录（容器内路径，建议卷挂载） |
| `history_dir` | `<watch_dir>/历史` | 归档目录，留空则按默认推导 |
| `check_interval` | `30` | 目录扫描间隔（秒） |
| `stable_delay` | `60` | 文件最后修改后需静置的秒数，防止处理仍在写入的文件 |
| `archive_after_hours` | `45` | 归档阈值（小时） |
| `mp3_bitrate` | `32k` | 可选 `16k/24k/32k/48k/64k/96k/128k` |
| `monitor_keywords` | `["等待直播"]` | 全局日志监控关键词 |
| `containers[].start_times` | — | `"20:01"` 或 `["17:03","17:13"]` 两种写法均可 |
| `containers[].max_run_duration` | `3600` | 最大运行秒数 |
| `containers[].keywords` | 继承全局 | 容器级关键词覆盖 |

### 关于时间与时区

原脚本用 `datetime.utcnow()` 做年份守卫，并对齐的是**容器本地时间**。Go 版本直接使用
`time.Local`（容器内由 `TZ` 环境变量决定，Dockerfile 默认 `Asia/Shanghai`），
并在启动日志中打印当前时区与下一个计划启动时刻，避免时区错位。

## Docker 部署

```bash
docker build -t livemonitor:1.0.0 .

docker run -d --name livemonitor \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /你的媒体目录:/audio \
  -v /你的配置目录:/config \
  -e TZ=Asia/Shanghai \
  -e PUID=1000 -e PGID=1000 \
  --restart unless-stopped \
  livemonitor:1.0.0
```

或使用 docker compose：

```bash
docker compose up -d
```

**必须挂载 `/var/run/docker.sock`**，否则无法控制宿主机容器（媒体转码仍可工作）。

## 测试

```bash
make test              # 全部单元测试
make test-race         # 竞态检测
```

测试不依赖网络与真实 docker：`internal/monitor` 使用假的 Runner 与可控日志流驱动关键词匹配；
`internal/media` 在 ffmpeg 可用时生成真实音频文件验证码率解析与转码产物。

## 项目结构

```
cmd/livemonitor/          程序入口
internal/cli/             子命令与参数解析
internal/config/          配置定义、加载、校验
internal/logging/         带作用域前缀的并发安全日志
internal/dockerctl/       docker CLI 封装（同步 + 流式）
internal/scheduler/       每日定点调度
internal/monitor/         容器生命周期监控
internal/media/           MP3 解析、ffmpeg 转码、目录扫描与归档
internal/manager/         编排各组件与信号处理
```
