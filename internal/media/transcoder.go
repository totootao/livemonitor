// Package media 的转码器：调用外部 ffmpeg 把 MP3 重新编码为更低码率。
//
// 为什么用 ffmpeg 而不是纯 Go：
// 早期版本用 go-mp3 解码 + shine-mp3 编码自己实现，好处是零外部依赖，
// 但实测下来有两个硬伤——
//
//	速度  同一文件 0.38s vs 0.14s，LAME 的 C 实现快一倍多
//	音质  原实现固定降采样到 22050Hz 单声道，而同为 32k 码率下，
//	      44.1kHz 立体声的体积几乎相同（117.6 vs 117.4 KB），
//	      也就是说那点"省下来"根本不存在，纯粹是白丢音质
//
// 代价是镜像里要带一个可执行文件。这里用的是为 MP3 转码专门裁剪的
// 1.4MB 静态二进制（见 docker/README-ffmpeg.md），而不是完整版 ffmpeg——
// 后者在 Alpine 上要拖进约 130MB 的共享库，几乎全是本项目用不到的视频组件。
//
// 输出参数取 ffmpeg 默认的 44.1kHz 立体声，刻意不传 -ac/-ar：
// 32k 码率下体积由时长决定而非采样率，降采样换不来空间，只会损失音质。
package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// FFmpegBinary 是转码所依赖的可执行文件名。
//
// 通过 PATH 查找而非写死绝对路径：镜像里放在 /usr/local/bin，
// 但开发机上通常用的是系统安装的 ffmpeg，靠 PATH 两边都能工作。
const FFmpegBinary = "ffmpeg"

// ffmpegInvalidData 是 ffmpeg 在"输入无法识别"时使用的退出码。
//
// 这个码有实际意义：它把"用户丢进来一个 AAC/FLAC/损坏文件"与
// "ffmpeg 自己出问题"区分开，前者只需提示用户转格式，
// 后者才值得告警。这个值来自 ffmpeg 源码中的 AVERROR_INVALIDDATA
// 取模 256 后的结果（-1094995529 & 0xFF）。
const ffmpegInvalidData = 183

// Transcoder 调用外部 ffmpeg 把 MP3 重新编码为更低码率。
//
// 零值不可用，需经 NewTranscoder 构造。
type Transcoder struct {
	// bin 是 ffmpeg 可执行文件的路径，由 Available 解析后缓存在此。
	// 留空表示尚未解析，届时走 PATH 查找。
	bin     string
	bitrate string
	log     *logging.Logger
	timeout time.Duration
}

// NewTranscoder 创建转码器。bitrate 形如 "32k"，为空时取 DefaultOutputBitrate。
//
// 第一个参数保留自更早的版本（那时用于指定 ffmpeg 路径），现已忽略——
// 路径统一由 Available 从 PATH 解析。签名不改成 (string) 是为了不动调用方。
func NewTranscoder(_ string, bitrate string, log *logging.Logger) *Transcoder {
	if bitrate == "" {
		bitrate = DefaultOutputBitrate
	}
	return &Transcoder{
		bitrate: bitrate,
		log:     log,
		// 单个文件处理上限，避免异常文件永久占用工作协程。
		timeout: time.Hour,
	}
}

// Available 检查转码所需的依赖是否就绪。
//
// 与纯 Go 版本不同，这里是真的在做外部依赖自检：ffmpeg 不存在时必须
// 在启动阶段就报出来，而不是等第一次转码才失败——后者会让用户先看到
// 一堆"处理文件失败"，却不容易联想到是镜像/环境缺了可执行文件。
func (t *Transcoder) Available() error {
	path, err := exec.LookPath(FFmpegBinary)
	if err != nil {
		return fmt.Errorf("未找到可执行文件 %q，转码功能不可用: %w", FFmpegBinary, err)
	}
	t.bin = path
	return nil
}

// Bitrate 返回输出码率字符串。
func (t *Transcoder) Bitrate() string { return t.bitrate }

// TranscodeFile 把 src 转为低码率 MP3，成功后按原脚本语义删除源文件并重命名产物。
//
// 之所以叫"转码"而非"转换"：源与产物都是 MP3，这里做的是解码再以目标码率重编码。
func (t *Transcoder) TranscodeFile(ctx context.Context, src string) error {
	base := strings.TrimSuffix(src, filepath.Ext(src))
	outputTmp := base + "-i.mp3"
	outputFinal := base + ".mp3"

	// 已存在中间产物：直接清理源文件并改名为最终名（对齐原脚本分支）。
	if _, err := os.Stat(outputTmp); err == nil {
		t.log.Info("MP3 中间产物已存在，跳过转换: %s", filepath.Base(outputTmp))
		return t.finalize(src, outputTmp, outputFinal)
	}

	t.log.Info("开始转换: %s -> %s（目标 %s / 44100Hz / 立体声）",
		filepath.Base(src), filepath.Base(outputTmp), t.bitrate)

	runCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	size, err := t.encodeFile(runCtx, src, outputTmp)
	if err != nil {
		// 失败时清理可能产生的残缺文件，避免下次误判为已完成。
		//
		// 这一步不能省：ffmpeg 在部分错误下会留下一个已经建好、
		// 但内容不完整的输出文件。若不删，下一轮扫描会因为它存在而走
		// "中间产物已存在"分支，直接把它当成成功结果改名为最终文件。
		if _, statErr := os.Stat(outputTmp); statErr == nil {
			_ = os.Remove(outputTmp)
		}
		return err
	}

	t.log.Info("转换成功: %s (%s)", filepath.Base(outputTmp), humanSize(size))
	return t.finalize(src, outputTmp, outputFinal)
}

// encodeFile 调用 ffmpeg 把 src 重新编码到 dst，返回产物字节数。
func (t *Transcoder) encodeFile(ctx context.Context, src, dst string) (int64, error) {
	bin := t.bin
	if bin == "" {
		// 调用方可能没先调 Available；这里补一次，避免直接 exec 空路径。
		resolved, err := exec.LookPath(FFmpegBinary)
		if err != nil {
			return 0, fmt.Errorf("未找到可执行文件 %q: %w", FFmpegBinary, err)
		}
		bin = resolved
		t.bin = resolved
	}

	// -y 覆盖输出；-loglevel error 只保留错误，正常转码不产生噪声。
	// 不传 -ac/-ar：保持 ffmpeg 默认的 44.1kHz 立体声，理由见包注释。
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-i", src,
		"-b:a", t.bitrate,
		dst,
		"-y",
	}
	cmd := exec.CommandContext(ctx, bin, args...)

	// 用独立的进程组并在超时时杀掉整组。
	//
	// CommandContext 默认只对直接子进程发 SIGKILL；若 ffmpeg 再派生
	// 子进程，那些孙子进程会变成孤儿继续占用 CPU 与文件句柄。
	// 本项目用的这个裁剪版 ffmpeg 不 fork，但设一下成本极低，
	// 属于"出问题时能兜住"的防御。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// 负号表示整个进程组。
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// stderr 必须单独收：ffmpeg 的错误信息只走 stderr，
	// 不捕获的话失败时只剩一句 "exit status 1"，无从定位。
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return 0, t.describeFailure(src, err, stderr.String())
	}

	info, err := os.Stat(dst)
	if err != nil {
		return 0, fmt.Errorf("转码后无法读取产物 %s: %w", filepath.Base(dst), err)
	}
	if info.Size() == 0 {
		return 0, errors.New("转码产物为空")
	}
	return info.Size(), nil
}

// describeFailure 把 ffmpeg 的退出状态与 stderr 整理成可操作的错误。
func (t *Transcoder) describeFailure(src string, runErr error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	// stderr 可能多行；只取前几行，避免把整个错误塞进日志。
	if lines := strings.Split(detail, "\n"); len(lines) > 3 {
		detail = strings.Join(lines[:3], "; ")
	}

	base := filepath.Base(src)

	// 上下文超时/取消要与 ffmpeg 自身的失败区分开：
	// 前者说明文件过大或卡住，后者说明文件本身有问题。
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		switch exitErr.ExitCode() {
		case ffmpegInvalidData:
			return fmt.Errorf(
				"无法解码 %s：不是 MP3，或文件已损坏（ffmpeg 仅支持 MP3 输入）%s",
				base, suffixDetail(detail))
		case -1:
			// 被信号杀死（多半是超时后我们发的 SIGKILL）。
			return fmt.Errorf("转码 %s 被中断（可能是耗时超过 %s）%s",
				base, formatDuration(int(t.timeout.Seconds())), suffixDetail(detail))
		default:
			return fmt.Errorf("转码 %s 失败（ffmpeg 退出码 %d）%s",
				base, exitErr.ExitCode(), suffixDetail(detail))
		}
	}
	return fmt.Errorf("执行 ffmpeg 失败: %w%s", runErr, suffixDetail(detail))
}

// formatDuration 把秒数格式化为人类可读的时长（如 "1小时"、"30秒"）。
func formatDuration(secs int) string {
	switch {
	case secs <= 0:
		return "0秒"
	case secs%3600 == 0:
		return fmt.Sprintf("%d小时", secs/3600)
	case secs%60 == 0:
		return fmt.Sprintf("%d分钟", secs/60)
	default:
		return fmt.Sprintf("%d秒", secs)
	}
}

// suffixDetail 把 stderr 摘要拼成错误后缀；无内容时返回空串。
func suffixDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// finalize 删除源文件并把 -i.mp3 中间产物改名为最终 .mp3。
func (t *Transcoder) finalize(src, tmp, final string) error {
	if strings.HasSuffix(strings.ToLower(src), "-i.mp3") {
		// 源文件本身已是中间产物，无需改名。
		return nil
	}
	if err := os.Remove(src); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.log.Warn("删除源文件失败: %v", err)
		return err
	}
	t.log.Info("已删除源文件: %s", filepath.Base(src))

	// 最终名已存在时先移除，避免 rename 失败。
	if _, err := os.Stat(final); err == nil {
		if rmErr := os.Remove(final); rmErr != nil {
			t.log.Warn("移除同名旧文件失败: %v", rmErr)
			return rmErr
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		t.log.Warn("重命名 %s -> %s 失败: %v", filepath.Base(tmp), filepath.Base(final), err)
		return err
	}
	t.log.Info("已生成: %s", filepath.Base(final))
	return nil
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
