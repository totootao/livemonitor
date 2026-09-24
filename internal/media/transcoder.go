// Package media 的转码器：纯 Go 实现 MP3 解码 + 重采样 + 重编码。
//
// 为什么不用 ffmpeg：
// 本项目的音频需求只有两条——把文件统一成 MP3、把 MP3 压到更低码率。
// 为此在镜像里塞进整套 ffmpeg（约 130MB 共享库，绝大多数是视频编解码器
// 与 GPU 后端）并不划算。这里改用两个纯 Go 库自己完成：
//
//	解码  github.com/hajimehoshi/go-mp3   —— MPEG-1/2 Layer III 解码为 PCM
//	编码  github.com/braheezy/shine-mp3   —— Shine 编码器的 Go 移植（定点运算）
//
// 代价是不支持除 MP3 以外的输入格式（TS/AAC/FLAC 等需要各自的解码器），
// 音质也弱于 LAME。对"录完压低码率存档"这一用途可以接受。
package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"strings"
	"time"

	"github.com/braheezy/shine-mp3/pkg/mp3"
	gomp3 "github.com/hajimehoshi/go-mp3"

	"github.com/totootao/livemonitor/internal/logging"
)

// 输出参数：与原脚本一致——单声道、22050Hz。
// 这两个值是对"语音为主的直播录音"在体积与可懂度之间的折中。
const (
	outputSampleRate = 22050
	outputChannels   = 1
)

// Transcoder 用纯 Go 把 MP3 重新编码为更低码率的 MP3。
//
// 零值不可用，需经 NewTranscoder 构造。
type Transcoder struct {
	bitrate string
	log     *logging.Logger
	timeout time.Duration
}

// NewTranscoder 创建转码器。bitrate 形如 "32k"，为空时取 DefaultOutputBitrate。
//
// 参数名沿用历史上的 ffmpeg 路径参数以便平滑替换，现已忽略——保留是为了
// 不破坏调用方签名（manager 与测试都在用）。
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
// 纯 Go 实现没有外部可执行文件依赖，因此只在编译期确定的问题（不支持的
// 目标码率）才会失败。保留该方法是为了与原先的依赖自检流程对齐。
func (t *Transcoder) Available() error {
	if _, err := parseTarget(t.bitrate); err != nil {
		return fmt.Errorf("MP3 输出码率 %q 不受支持: %w", t.bitrate, err)
	}
	return nil
}

// Bitrate 返回输出码率字符串。
func (t *Transcoder) Bitrate() string { return t.bitrate }

// transcodeParams 是一次转码的全部输入参数。
type transcodeParams struct {
	bitrateKbps      int
	bitrateIndex     int
	sampleRate       int
	channels         int
	granulesPerFrame int
}

// parseTarget 把 "32k" 之类的码率字符串解析为编码参数。
func parseTarget(bitrate string) (transcodeParams, error) {
	kbps := BitrateToKbps(bitrate)
	if kbps <= 0 {
		return transcodeParams{}, fmt.Errorf("无法解析码率 %q", bitrate)
	}
	idx := bitrateIndex(kbps, outputSampleRate)
	if idx < 0 {
		return transcodeParams{}, fmt.Errorf("输出采样率 %d 下没有 %dk 这一档码率", outputSampleRate, kbps)
	}
	// 22050Hz 落在 MPEG-2，每帧 1 个 granule、576 个样本。
	return transcodeParams{
		bitrateKbps:      kbps,
		bitrateIndex:     idx,
		sampleRate:       outputSampleRate,
		channels:         outputChannels,
		granulesPerFrame: 1,
	}, nil
}

// mpeg1Bitrates / mpeg2Bitrates 是 Layer III 的码率表（索引 0 与 15 为保留值）。
// 与编码器内部使用的一致；帧头里写的是索引而非数值，所以必须自己反查。
var (
	mpeg1Bitrates = []int{-1, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, -1}
	mpeg2Bitrates = []int{-1, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1}
)

// bitrateIndex 按采样率推断 MPEG 版本，反查码率在表中的索引。
func bitrateIndex(kbps, sampleRate int) int {
	table := mpeg2Bitrates
	if sampleRate >= 32000 {
		table = mpeg1Bitrates
	}
	for i, v := range table {
		if v == kbps {
			return i
		}
	}
	return -1
}

// transcodeResult 描述一次转码的产物。
type transcodeResult struct {
	Output string
	Err    error
}

// TranscodeFile 把 src 转为低码率 MP3，成功后按原脚本语义删除源文件并重命名产物。
//
// 之所以叫"转码"而非"转换"：源与产物都是 MP3，这里做的是解码到 PCM、
// 重采样到单声道 22050Hz、再以目标码率重新编码。
func (t *Transcoder) TranscodeFile(ctx context.Context, src string) error {
	base := strings.TrimSuffix(src, filepath.Ext(src))
	outputTmp := base + "-i.mp3"
	outputFinal := base + ".mp3"

	// 已存在中间产物：直接清理源文件并改名为最终名（对齐原脚本分支）。
	if _, err := os.Stat(outputTmp); err == nil {
		t.log.Info("MP3 中间产物已存在，跳过转换: %s", filepath.Base(outputTmp))
		return t.finalize(src, outputTmp, outputFinal)
	}

	params, err := parseTarget(t.bitrate)
	if err != nil {
		return err
	}

	t.log.Info("开始转换: %s -> %s（目标 %dk / 单声道 / %dHz）",
		filepath.Base(src), filepath.Base(outputTmp), params.bitrateKbps, params.sampleRate)

	runCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	size, err := t.encodeFile(runCtx, src, outputTmp, params)
	if err != nil {
		// 失败时清理可能产生的残缺文件，避免下次误判为已完成。
		if _, statErr := os.Stat(outputTmp); statErr == nil {
			_ = os.Remove(outputTmp)
		}
		return err
	}

	t.log.Info("转换成功: %s (%s)", filepath.Base(outputTmp), humanSize(size))
	return t.finalize(src, outputTmp, outputFinal)
}

// encodeFile 解码 src 并编码到 dst，返回产物字节数。
func (t *Transcoder) encodeFile(ctx context.Context, src, dst string, p transcodeParams) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	dec, err := gomp3.NewDecoder(in)
	if err != nil {
		// 走到这里基本可以确定不是 MP3（或已损坏）。给出可操作的提示：
		// 纯 Go 方案不支持非 MP3 输入，用户需要先自行转成 MP3。
		return 0, fmt.Errorf("无法解码 %s（纯 Go 方案仅支持 MP3 输入）: %w", filepath.Base(src), err)
	}

	mono, srcRate, err := t.decodeToMono(ctx, dec)
	if err != nil {
		return 0, err
	}
	if len(mono) == 0 {
		return 0, errors.New("源文件没有可用的音频采样")
	}

	pcm := resampleLinear(mono, srcRate, p.sampleRate)
	if len(pcm) == 0 {
		return 0, errors.New("重采样后没有采样数据")
	}

	return t.writeEncoded(dst, pcm, p)
}

// decodeToMono 把解码器输出读成单声道 int16 采样序列，同时返回源采样率。
//
// 注意：go-mp3 的 Decoder 输出**恒为 16 位小端双声道**（即使源是单声道 MP3
// 也会复制成两份），因此这里固定按 LRLR 处理，无需判断声道数。
func (t *Transcoder) decodeToMono(ctx context.Context, dec *gomp3.Decoder) ([]int16, int, error) {
	srcRate := dec.SampleRate()

	// 先按最大可能长度预留，直播录音动辄数小时，逐次 append 触发的大量
	// 扩容拷贝会明显拖慢速度。
	est := int(srcRate) * 60
	if est < 1<<16 {
		est = 1 << 16
	}
	out := make([]int16, 0, est)

	buf := make([]byte, 64*1024)
	// 跨 Read 调用保留半个立体声帧（2 字节），否则缓冲区边界处会丢样本。
	var carry [2]byte
	hasCarry := false

	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		n, readErr := dec.Read(buf)
		if n > 0 {
			var chunk []byte
			if hasCarry {
				chunk = make([]byte, 0, n+2)
				chunk = append(chunk, carry[:]...)
				chunk = append(chunk, buf[:n]...)
			} else {
				chunk = buf[:n]
			}
			out = append(out, pcmToMono(chunk)...)

			// 记录落单的字节，留给下一轮拼接。
			if rem := len(chunk) % 4; rem != 0 {
				copy(carry[:], chunk[len(chunk)-rem:])
				hasCarry = true
			} else {
				hasCarry = false
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, 0, fmt.Errorf("解码失败: %w", readErr)
		}
	}
	return out, srcRate, nil
}

// pcmToMono 把 16 位小端立体声交错 PCM（LRLR）混为单声道。
// 尾部不足 4 字节的部分会被忽略，调用方负责拼接残字节。
func pcmToMono(b []byte) []int16 {
	out := make([]int16, 0, len(b)/4)
	for i := 0; i+3 < len(b); i += 4 {
		l := int32(int16(uint16(b[i]) | uint16(b[i+1])<<8))
		r := int32(int16(uint16(b[i+2]) | uint16(b[i+3])<<8))
		out = append(out, int16((l+r)/2))
	}
	return out
}

// resampleLinear 用线性插值做重采样。
//
// 直播录音以人声为主，且输出只有 22050Hz，线性插值的混叠在听感上
// 可忽略；换成窗函数 sinc 插值会显著增加实现复杂度与运行时间。
func resampleLinear(in []int16, from, to int) []int16 {
	if from == to || len(in) == 0 {
		return in
	}
	n := int(int64(len(in)) * int64(to) / int64(from))
	if n <= 0 {
		return nil
	}
	out := make([]int16, n)
	ratio := float64(from) / float64(to)
	for i := range out {
		pos := float64(i) * ratio
		i0 := int(pos)
		if i0 >= len(in)-1 {
			out[i] = in[len(in)-1]
			continue
		}
		frac := pos - float64(i0)
		out[i] = int16(float64(in[i0])*(1-frac) + float64(in[i0+1])*frac)
	}
	return out
}

// writeEncoded 用 Shine 编码器把 PCM 写入 dst，返回字节数。
func (t *Transcoder) writeEncoded(dst string, pcm []int16, p transcodeParams) (int64, error) {
	samplesPerFrame := 576 * p.granulesPerFrame
	if r := len(pcm) % samplesPerFrame; r != 0 {
		// 补齐到整帧，避免尾帧样本不足导致编码器读到越界。
		pcm = append(pcm, make([]int16, samplesPerFrame-r)...)
	}

	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	enc := mp3.NewEncoder(p.sampleRate, p.channels)
	// NewEncoder 内部是按默认 128k 算好帧参数的，换码率必须自己重算，
	// 否则写出的帧头与帧长不匹配，解码器会算错帧边界、时长严重偏短。
	enc.Mpeg.Bitrate = int64(p.bitrateKbps)
	enc.Mpeg.BitrateIndex = int64(p.bitrateIndex)
	enc.Mpeg.GranulesPerFrame = int64(p.granulesPerFrame)
	bitsPerFrame := float64(samplesPerFrame) / float64(p.sampleRate) * float64(p.bitrateKbps) * 1000
	enc.Mpeg.WholeSlotsPerFrame = int64(bitsPerFrame / 8)
	enc.Mpeg.FracSlotsPerFrame = bitsPerFrame/8 - float64(enc.Mpeg.WholeSlotsPerFrame)
	enc.Mpeg.SlotLag = -enc.Mpeg.FracSlotsPerFrame

	// 编码器维护跨调用的输入游标，因此每次必须传入连续的整数帧切片。
	for i := 0; i < len(pcm); i += samplesPerFrame {
		end := i + samplesPerFrame
		if end > len(pcm) {
			end = len(pcm)
		}
		data, written := enc.EncodeBufferInterleaved(pcm[i:end])
		if written > 0 {
			if _, err := out.Write(data[:written]); err != nil {
				return 0, err
			}
		}
	}

	if err := out.Close(); err != nil {
		return 0, err
	}
	info, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	if info.Size() == 0 {
		return 0, errors.New("编码后输出为空")
	}
	return info.Size(), nil
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
