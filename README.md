# livemonitor

用 Go 重写的直播容器监控与媒体压缩服务。原 Python 脚本（`livemonitor.py`）依赖
`schedule`、`mutagen`、`shutil` 等第三方库，并调用外部 `ffmpeg` 与 `docker` 命令；
Go 版本把这些都换成了进程内的实现，运行镜像只需一个几 MB 的 alpine。

功能与原脚本一致：

1. **定时启动容器** — 每个容器可配置多个每日启动时刻，到点自动启动容器。
2. **日志关键词监控** — 实时跟踪容器日志，命中关键词（默认 `等待直播`）立即停止容器。
3. **最大运行时长保护** — 到达设定时长后自动停止容器。
4. **MP3 压缩** — 监控目录中的 MP3 自动压到目标码率（默认 32k / 单声道 / 22050Hz）。
5. **过期文件归档** — 早于阈值的 MP3 自动移入 `历史` 子目录。
6. **Web 管理界面** — 浏览器里增删改定时任务、查看运行状态、调整全局设置，改动即时生效并写回磁盘。

## 两个容易踩的语义

### 容器已在运行时：接管，而不是跳过

定时任务到点时，如果容器已经处于运行状态（用户手动起的、上次进程退出后残留的、
或 `restart: unless-stopped` 自动拉起的），程序**不会**跳过这次计划，而是**接管**它：
继续做日志关键词监控与超时保护，并按容器的**真实启动时间**继续计时。

如果按真实启动时间算已经超过 `max_run_duration`，接管后会立即停止它——
这个容器本来就该停了，不该因为被接管而重新获得一整轮运行时长。

> 早期版本在这里是"跳过本次计划"，那是个设计错误：跳过意味着既不做关键词监控、
> 也不做超时停止，容器会一直跑下去直到有人手动停它。
> "容器正在运行"和"今天的计划已经执行过"是两件事，不能混为一谈。

**代价**：如果你手动起了一个需要长期运行的容器，而它又在本程序的配置里，
那么到点后它会被接管并在超时后停掉。这类容器不应配在 `containers` 里。

### 当天已执行记录会落盘

"今天是否已经执行过"记录在 `scheduler-state.json`，位置与配置文件同级
（配置在 `/config/config.json`，状态就在 `/config/scheduler-state.json`）。
只保留当天的条目，昨天的记录在启动时会被丢弃。

> 这个记录**必须**持久化。放在内存里的话，进程重启或任何配置热更新
> （`Reload` 会重建全部 Job 对象）都会把它清零，导致当天所有已过点的任务
> 被重放一遍——现象是同一批容器在几分钟内被反复启动，日志里出现多条
> "容器已在运行"。

Web 界面的**立即执行**不受此记录限制，是人的显式意图。

### 状态以 Docker 为准，不信内存记账

容器被 `docker stop`、自己退出或崩溃时，本程序**收不到任何通知**。
因此内部维护的"是否在运行"会在这些情况下停留在旧值——界面一直显示"运行中"，
点启动被拒（"已在运行中"），点停止也被拒（"未在运行"）。

为此，所有对外汇报状态、以及任何"要不要启动/停止"的判断，都会先向 Docker
查一次真实状态（`ContainerMonitor.SyncState`），并据此纠正内部记账。
查询失败时保守沿用旧值：宁可短暂显示旧状态，也不要因为一次网络抖动
就把运行中的容器标成已停止。

代价是每次状态查询多一次 inspect 调用。容器在几十个的量级、状态接口只在
打开界面或轮询时触发，可以接受。

## 设计取舍

### 为什么不依赖外部程序

| 原先依赖 | 现在的做法 | 收益 |
| --- | --- | --- |
| `docker` CLI（约 31MB 静态二进制） | 标准库经 unix socket 直接调 Docker Engine API | 镜像不再需要 CLI |
| `ffmpeg`（约 130MB 共享库） | 纯 Go：`go-mp3` 解码 + `shine-mp3` 编码 | 镜像不再需要 ffmpeg |
| `mutagen`（Python 库） | `internal/media/mp3.go` 自行解析 MPEG 帧头 | 无第三方运行时依赖 |

### 代价

- **只支持 MP3 作为输入**。TS/AAC/FLAC 等格式没有可用的纯 Go 解码器，
  扫描到时会给出一条告警日志，提示先转成 MP3。需要直接从 TS 转码的场景
  不适合本程序。
- **音质弱于 LAME**。`shine-mp3` 是 Shine 编码器的移植版，官方定位就是
  "能听、文件更大、质量不如 LAME"。对"录制后压低码率存档"这一用途足够。
- 由此换来的是：镜像体积从 144MB 降到 **15.6MB**，且没有外部进程调用。

## 与原脚本的对应关系

| Python 实现 | Go 实现 | 说明 |
| --- | --- | --- |
| `schedule.every().day.at()` | `internal/scheduler` | 按秒轮询，按「日期 + 时刻」去重，避免同一时刻重复触发 |
| `mutagen.mp3.MP3().info.bitrate` | `internal/media/mp3.go` | 自行解析 MPEG 帧头，跳过 ID3v2 标签，含二次帧校验防误判 |
| `subprocess.run/Popen` | `internal/dockerctl`、`internal/media` | 全部改为 `context` 驱动，支持超时与优雅取消 |
| `subprocess.run(["docker", ...])` | `internal/dockerctl` + Docker Engine API | 直接用标准库经 unix socket 调 Engine API，镜像内不再需要 docker CLI |
| `subprocess.run(["ffmpeg", ...])` | `internal/media/transcoder.go` | 纯 Go 解码 + 重采样 + 重编码，镜像内不再需要 ffmpeg |
| `threading.Thread` + `Event` | goroutine + `context.Context` | 用 context 取消替代 Event 信号，避免竞态 |
| `re.compile(r'[\x00-\x1F\x7F]')` | `monitor.CleanLogLine` | 逐 rune 扫描，保留制表符 |
| `shutil.move` | `media.moveFile` | 先 `rename`，跨分区时回退为复制 + 删除 |
| 硬编码 `CONTAINERS_CONFIG` | `config.json` + Web 界面 | 配置外置，支持校验、容器级关键词覆盖、运行时热更新 |

> 这是项目里仅有的两个第三方依赖（都用于 MP3 编解码），其余全部为标准库。

## 快速开始

```bash
# 1. 生成配置
go run ./cmd/livemonitor init -config config.json

# 2. 校验配置（会打印解析结果与生效参数）
go run ./cmd/livemonitor check -config config.json

# 3. 启动服务（默认开启 Web 管理界面 :8080）
go run ./cmd/livemonitor run -config config.json -log-level info
```

编译单文件二进制：

```bash
make build            # 产出 bin/livemonitor
```

## 命令行

```
livemonitor run     启动监控服务（含 Web 管理界面）
livemonitor init    生成默认配置文件
livemonitor check   校验配置并打印摘要
livemonitor probe   查看 MP3 文件码率等参数
livemonitor version 查看版本
```

通用选项：`-config <路径>`、`-log-level debug|info|warn|error`。

`run` 专属选项：

| 选项 | 默认值 | 说明 |
| --- | --- | --- |
| `-web <地址>` | `:8080` | Web 管理界面监听地址，填 `off` 可禁用 |
| `-force` | `false` | 配置校验失败时仍尝试启动（仅告警） |

## Web 管理界面

启动后访问 `http://<主机>:8080` 即可管理定时任务，无需手工编辑 JSON。

![Web 管理界面](docs/web-ui.png)

能做什么：

| 区域 | 功能 |
| --- | --- |
| 运行概览 | 启动时间、已运行时长、媒体目录、待处理队列、服务器时间与配置文件路径 |
| 容器定时任务 | 每个容器的每日启动时刻、最长运行时长、运行状态与剩余时间、下次启动倒计时 |
| 容器操作 | **立即启动**（跳过计划手动跑一次）、**停止**、**编辑**、**删除** |
| 全局设置 | 监控目录、归档目录、扫描间隔、静置阈值、归档阈值、MP3 码率、全局关键词 |
| 重载配置 | 手工改完 `config.json` 后一键热应用，不必重启进程 |

设计要点：

- 所有写操作都**先落盘再热应用**。配置通过「写临时文件 + `rename`」原子替换，避免写一半损坏配置。
- 启动时刻在保存时自动**去空白、去重、升序**；容器名重复视为更新而非新增。
- 编辑某个容器的时刻后，该容器的旧调度任务会被精确摘除再重建，不会残留已删除的时间点。
- 页面每 5 秒自动刷新；配置为纯静态 HTML（`go:embed` 内嵌），单文件二进制即可运行。
- 路径参数按原始编码（`EscapedPath`）切分，容器名中的非法字符（`/`、`\`、`..`）会被拒绝。

> ⚠️ **该界面没有任何鉴权**，任何能访问该端口的人都可以启停你的容器、修改配置。
> Docker 部署时默认只绑定 `127.0.0.1`，需要远程访问请自行加反向代理与访问控制，
> 或把 `LIVEMONITOR_WEB_ADDR` 设为 `off` 关闭界面。

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
  -p 127.0.0.1:8080:8080 \
  -e TZ=Asia/Shanghai \
  -e PUID=1000 -e PGID=1000 \
  --restart unless-stopped \
  livemonitor:1.0.0
```

或使用 docker compose：

```bash
docker compose up -d
```

**必须挂载 `/var/run/docker.sock`**，否则无法控制宿主机容器（MP3 压缩仍可工作）。
程序通过 Docker Engine API 直接与 socket 通信（HTTP over unix socket），**镜像内不安装
docker CLI**，因此体积比走命令行的方案小约 31MB。

> 挂载 socket 等同于把宿主机 Docker 的控制权交给容器，请勿把 Web 管理界面直接暴露到公网。
> 若确实需要更严格的隔离，可改用只读 socket 或 docker-socket-proxy 转发。

环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LIVEMONITOR_CONFIG` | `/config/config.json` | 配置文件路径 |
| `LIVEMONITOR_LOG_LEVEL` | `info` | 日志级别 |
| `LIVEMONITOR_WEB_ADDR` | `:8080` | Web 界面监听地址，设为 `off` 禁用 |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker Engine API 的 socket 路径（仅支持 unix://） |
| `TZ` | `Asia/Shanghai` | 时区（影响所有定时任务的判定） |
| `PUID` / `PGID` | 未设置 | 同时设置则以该 UID/GID 运行，便于处理挂载目录属主 |

## 测试

测试分两层：**单元测试**验证逻辑，**端到端测试**验证"打出来的镜像能不能用"。

### 单元测试

```bash
make test              # 全部单元测试
make test-race         # 竞态检测（CI 用这个）
```

不依赖网络，也不需要 ffmpeg——测试素材由 `go-mp3` / `shine-mp3` 现场合成：

- `internal/dockerctl` 用临时 unix socket 起一个假的 Engine API 服务，覆盖探活、启停、
  日志帧解析与请求路径转义；
- `internal/monitor` 使用假的客户端与可控日志流驱动关键词匹配；
- `internal/media` 覆盖帧头解析（含 Xing/Info 信息帧跳过）、解码、重采样、重编码与产物校验；
- `internal/runtime` 验证配置热更新的原子落盘与并发安全；
- `internal/web` 通过假控制器覆盖全部 HTTP 接口（含错误路径与路径穿越防护）。

### 端到端测试

```bash
make e2e               # 构建镜像后跑全流程（需要本机 docker）
IMAGE=tototao/livemonitor:latest bash scripts/e2e.sh   # 直接测已有镜像
```

49 项断言，覆盖 8 个方面：

| 分组 | 验证内容 |
| --- | --- |
| 镜像内容 | 确认 `ffmpeg` / `docker` CLI **确实不存在**，时区数据可用，entrypoint 可执行 |
| 素材准备 | 用 ffmpeg 造多种码率、带/不带 Xing 头的测试文件 |
| CLI 子命令 | `init` / `check` 的行为与错误提示（含重复 init 拒绝覆盖） |
| 压缩主流程 | 日志关键节点、不支持格式告警**且去重**（周期性扫描不应刷屏） |
| 产物校验 | 码率/采样率/声道/时长/压缩比，并验证低码率文件**被跳过** |
| Web 界面 | 首页、`/api/state`、改设置写回磁盘、非法输入返回 400 |
| 容器控制 | 真实 Engine API 启停容器 |
| 优雅退出 | SIGTERM 后正常退出并打印停止流程 |

想单独对真实 Docker 测 Engine API 客户端：

```bash
docker run -d --name livemonitor-selftest alpine:3.22 \
  sh -c 'while true; do echo "测试日志 $(date)"; sleep 2; done'
LIVEMONITOR_DOCKER_E2E=1 go test -run TestLive -v ./internal/dockerctl/
docker rm -f livemonitor-selftest
```

> 端到端测试不是摆设。它实际抓到过一个只有真实容器才暴露的 bug：
> LAME 写入的 Xing/Info 信息帧头里码率字段与实际音频不符（32k 的文件里写着 56k），
> 导致低码率文件每轮扫描都被重压一遍。纯逻辑测试造不出这种素材。

## 项目结构

```
cmd/livemonitor/          程序入口
internal/cli/             子命令与参数解析
internal/config/          配置定义、加载、校验
internal/logging/         带作用域前缀的并发安全日志
internal/dockerctl/       Docker Engine API 客户端（unix socket + HTTP，无 CLI 依赖）
internal/scheduler/       每日定点调度（支持按 ID 精确增删）
internal/monitor/         容器生命周期监控
internal/media/           MP3 帧头解析（跳过 Xing/Info 信息帧）、纯 Go 重编码、目录扫描与归档
internal/runtime/         可热更新的并发安全配置存储（原子落盘）
internal/web/             Web 管理界面与 JSON API（go:embed 内嵌页面）
internal/manager/         编排各组件、Web 服务与信号处理
scripts/verify-embed.sh   校验二进制内嵌了 Web 页面（CI 用）
scripts/e2e.sh            容器级端到端测试（49 项断言）
```
