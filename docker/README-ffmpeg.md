# docker/ffmpeg —— 定制精简版 FFmpeg

这个 1.4 MB 的静态二进制不是通用 FFmpeg 发行版，而是为**本项目的单一需求**专门裁剪的：
把 MP3 重新编码为更低码率（320k → 32k）。

来源：<https://github.com/totootao/ffmpeg-mp3-32k>
（FFmpeg 7.1.1 + LAME 3.100，musl 静态链接）

## 为什么不用系统 FFmpeg

完整版 ffmpeg 在 Alpine 上要拖进约 130 MB 的共享库，其中绝大部分是视频编解码器、
GPU 后端、各种容器格式与网络协议——而这个项目只用得到 MP3 那一条转码路径。

用 `--disable-everything` 逐个打开所需组件后，二进制降到 1.4 MB，
`ldd` 显示 `not a dynamic executable`，可以直接丢进 alpine 运行，无需 `apk add` 任何东西。

代价是**只能处理 MP3 输入**。AAC/FLAC/视频等会直接报
`Invalid data found when processing input`，扫描到时给出告警日志。

## 内置能力

| 组件 | 内容 |
|---|---|
| 解码器 | `mp3` / `mp3float` |
| 编码器 | `libmp3lame` |
| 容器 | mp3 demuxer + mp3 muxer（含 ID3/Xing 头） |
| 协议 | `file` |
| 过滤器 | `aresample`、`aformat`、`anull` 等转码管线必需项 |
| 不含 | 网络、视频、设备、`ffprobe`、`ffplay`、其余全部编解码器 |

## 校验

```sh
sha256sum docker/ffmpeg
# 9fee093ba8611deb6edb6af8b5424f9d55f4b90bda01b6d00f08e2a996b092d9

file docker/ffmpeg
# ELF 64-bit LSB executable, x86-64, statically linked, stripped
```

## 升级步骤

1. 在上游仓库重新构建（见其 `build.sh`）
2. 覆盖本目录的 `ffmpeg`
3. 更新上面那行 sha256
4. `bash scripts/e2e.sh` 确认全绿

## 许可

FFmpeg 核心与 LAME 均为 **LGPL**，二进制按 LGPL-2.1+ 分发。
完整许可文本见同目录的 `COPYING.LGPLv2.1`，该文件也会被打进镜像。
