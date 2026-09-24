// Package media 的转码器：封装 ffmpeg 调用。
package media

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// Transcoder 调用 ffmpeg 把音视频转成 MP3。
type Transcoder struct {
	ffmpeg  string
	bitrate string
	log     *logging.Logger
	timeout time.Duration
}

// NewTranscoder 创建转码器。ffmpeg 为空时使用 PATH 中的 ffmpeg。
func NewTranscoder(ffmpeg, bitrate string, log *logging.Logger) *Transcoder {
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	return &Transcoder{
		ffmpeg:  ffmpeg,
		bitrate: bitrate,
		log:     log,
		// 单个文件转码上限，避免流式文件异常导致永久占用。
		timeout: time.Hour,
	}
}

// Available 检查 ffmpeg 是否可用。
func (t *Transcoder) Available() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := exec.LookPath(t.ffmpeg); err != nil {
		return fmt.Errorf("未找到 ffmpeg 可执行文件 %q: %w", t.ffmpeg, err)
	}
	out, err := exec.CommandContext(ctx, t.ffmpeg, "-hide_banner", "-version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg 执行失败: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// transcodeResult 描述一次转码的产物。
type transcodeResult struct {
	Output string
	Err    error
}

// TranscodeFile 把 src 转为 MP3，成功后按原脚本语义删除源文件并重命名产物。
func (t *Transcoder) TranscodeFile(ctx context.Context, src string) error {
	base := strings.TrimSuffix(src, filepath.Ext(src))
	outputTmp := base + "-i.mp3"
	outputFinal := base + ".mp3"

	// 已存在中间产物：直接清理源文件并改名为最终名（对齐原脚本分支）。
	if _, err := os.Stat(outputTmp); err == nil {
		t.log.Info("MP3 中间产物已存在，跳过转换: %s", filepath.Base(outputTmp))
		return t.finalize(src, outputTmp, outputFinal)
	}

	args := []string{
		"-y",
		"-i", src,
		"-c:a", "libmp3lame",
		"-b:a", t.bitrate,
		"-ac", "1",
		"-ar", "22050",
		"-hide_banner", "-loglevel", "error",
		outputTmp,
	}
	t.log.Info("开始转换: %s -> %s", filepath.Base(src), filepath.Base(outputTmp))
	t.log.Debug("执行命令: %s %s", t.ffmpeg, strings.Join(args, " "))

	runCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, t.ffmpeg, args...)
	stderr, err := cmd.CombinedOutput()
	if err != nil {
		// 转码失败时清理可能产生的残缺文件，避免下次误判为已完成。
		if _, statErr := os.Stat(outputTmp); statErr == nil {
			_ = os.Remove(outputTmp)
		}
		return fmt.Errorf("ffmpeg 转码失败: %w (%s)", err, strings.TrimSpace(string(stderr)))
	}

	info, statErr := os.Stat(outputTmp)
	if statErr != nil || info.Size() == 0 {
		_ = os.Remove(outputTmp)
		return fmt.Errorf("转换后输出文件缺失或为空，ffmpeg 输出: %s", strings.TrimSpace(string(stderr)))
	}

	t.log.Info("转换成功: %s (%s)", filepath.Base(outputTmp), humanSize(info.Size()))
	return t.finalize(src, outputTmp, outputFinal)
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
