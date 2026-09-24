package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braheezy/shine-mp3/pkg/mp3"
	gomp3 "github.com/hajimehoshi/go-mp3"

	"github.com/totootao/livemonitor/internal/logging"
)

// ---- 测试素材生成（纯 Go，不再依赖 ffmpeg） ----

// genMP3 用纯 Go 编码器合成一段指定码率、指定采样率的 MP3。
//
// 历史上这里调用 ffmpeg 造素材，去掉 ffmpeg 依赖后改为直接使用
// Shine 编码器，测试因此完全自持、可在任何环境跑。
func genMP3(t *testing.T, path string, bitrateKbps, sampleRate, seconds int, toneHz float64) {
	t.Helper()
	pcm := sinePCM(sampleRate, seconds, toneHz)
	if err := writeMP3File(path, pcm, bitrateKbps, sampleRate); err != nil {
		t.Fatalf("生成测试 MP3 失败: %v", err)
	}
}

// sinePCM 生成一段正弦波 PCM（单声道 int16）。
func sinePCM(sampleRate, seconds int, freq float64) []int16 {
	n := sampleRate * seconds
	out := make([]int16, n)
	// 幅度取满量程的一半，避免定点编码器在大信号上出现削波混叠。
	const amp = 12000
	for i := range out {
		phase := 2 * 3.14159265358979 * freq * float64(i) / float64(sampleRate)
		out[i] = int16(amp * sinApprox(phase))
	}
	return out
}

// sinApprox 是 sin 的简易实现，精度对测试足够（<1e-3）。
func sinApprox(x float64) float64 {
	const twoPi = 6.283185307179586
	const pi = 3.141592653589793
	for x > pi {
		x -= twoPi
	}
	for x < -pi {
		x += twoPi
	}
	x2 := x * x
	return x * (1 - x2/6*(1-x2/20*(1-x2/42)))
}

// writeMP3File 用 Shine 把单声道 PCM 写成指定码率的 MP3 文件。
//
// 编码器内部按默认 128k 算好帧参数，换码率必须自己重算——与
// transcoder.go 的 writeEncoded 保持同一套逻辑，否则帧头与帧长不匹配。
func writeMP3File(path string, pcm []int16, bitrateKbps, sampleRate int) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	const channels = 1
	enc := mp3.NewEncoder(sampleRate, channels)
	idx := bitrateIndex(bitrateKbps, sampleRate)
	if idx < 0 {
		return fmt.Errorf("采样率 %d 下不支持 %dk 码率", sampleRate, bitrateKbps)
	}
	granules := 1
	samplesPerFrame := 576
	if sampleRate >= 32000 {
		granules = 2
		samplesPerFrame = 1152
	}

	enc.Mpeg.Bitrate = int64(bitrateKbps)
	enc.Mpeg.BitrateIndex = int64(idx)
	enc.Mpeg.GranulesPerFrame = int64(granules)
	bitsPerFrame := float64(samplesPerFrame) / float64(sampleRate) * float64(bitrateKbps) * 1000
	enc.Mpeg.WholeSlotsPerFrame = int64(bitsPerFrame / 8)
	enc.Mpeg.FracSlotsPerFrame = bitsPerFrame/8 - float64(enc.Mpeg.WholeSlotsPerFrame)
	enc.Mpeg.SlotLag = -enc.Mpeg.FracSlotsPerFrame

	if rem := len(pcm) % samplesPerFrame; rem != 0 {
		pcm = append(pcm, make([]int16, samplesPerFrame-rem)...)
	}
	for i := 0; i < len(pcm); i += samplesPerFrame {
		end := i + samplesPerFrame
		if end > len(pcm) {
			end = len(pcm)
		}
		data, written := enc.EncodeBufferInterleaved(pcm[i:end])
		if written > 0 {
			if _, err := out.Write(data[:written]); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- 原有测试：码率解析 ----

// TestProbeMP3Bitrate 验证码率解析与 mutagen 行为一致。
func TestProbeMP3Bitrate(t *testing.T) {
	dir := t.TempDir()
	for _, want := range []int{32, 64, 128} {
		p := filepath.Join(dir, "a.mp3")
		genMP3(t, p, want, 44100, 1, 440)

		info, err := ProbeMP3(p)
		if err != nil {
			t.Fatalf("%dk 素材解析失败: %v", want, err)
		}
		if info.BitrateKbps != want {
			t.Errorf("码率 = %d, 期望 %d", info.BitrateKbps, want)
		}
		if info.SampleRate != 44100 {
			t.Errorf("采样率 = %d, 期望 44100", info.SampleRate)
		}
	}
}

// TestProbeMP3LowQualityThreshold 验证低码率判定，等价于原脚本的 < 100 判断。
func TestProbeMP3LowQualityThreshold(t *testing.T) {
	dir := t.TempDir()

	low := filepath.Join(dir, "low.mp3")
	genMP3(t, low, 32, 44100, 1, 440)
	info, err := ProbeMP3(low)
	if err != nil {
		t.Fatalf("ProbeMP3 失败: %v", err)
	}
	if !info.IsLowQuality(100) {
		t.Errorf("32k 应判为低质量，实际 %s", info.Describe())
	}

	high := filepath.Join(dir, "high.mp3")
	genMP3(t, high, 128, 44100, 1, 440)
	info, err = ProbeMP3(high)
	if err != nil {
		t.Fatalf("ProbeMP3 失败: %v", err)
	}
	if info.IsLowQuality(100) {
		t.Errorf("128k 不应判为低质量，实际 %s", info.Describe())
	}
}

// TestProbeMP3WithID3v2Tag 验证带 ID3v2 标签的文件也能正确跳过标签。
func TestProbeMP3WithID3v2Tag(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw.mp3")
	genMP3(t, raw, 128, 44100, 1, 440)

	body, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}

	// 手工拼一个最小 ID3v2.3 头：10 字节头声明正文 100 字节。
	header := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 100}
	tagged := filepath.Join(dir, "tagged.mp3")
	if err := os.WriteFile(tagged, append(header, append(make([]byte, 100), body...)...), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := ProbeMP3(tagged)
	if err != nil {
		t.Fatalf("带 ID3 标签的文件解析失败: %v", err)
	}
	if info.BitrateKbps != 128 {
		t.Errorf("码率 = %d, 期望 128", info.BitrateKbps)
	}
}

// TestProbeMP3Invalid 验证非法文件返回错误而不是 panic。
func TestProbeMP3Invalid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.mp3")
	if err := os.WriteFile(p, []byte("这不是 MP3 文件，只是一段中文文本"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeMP3(p); err == nil {
		t.Error("非法内容应返回错误")
	}
}

// TestProbeMP3Empty 验证空文件不会 panic。
func TestProbeMP3Empty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.mp3")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeMP3(p); err == nil {
		t.Error("空文件应返回错误")
	}
}

// ---- 纯 Go 转码 ----

// TestTranscodeAndCleanup 验证转码产物参数正确、源文件被清理、中间产物被改名。
func TestTranscodeAndCleanup(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.mp3")
	genMP3(t, src, 128, 44100, 2, 440)

	log := logging.New("test")
	tc := NewTranscoder("", "32k", log)
	if err := tc.TranscodeFile(context.Background(), src); err != nil {
		t.Fatalf("转码失败: %v", err)
	}

	// 产物应落在源文件名上（源已删除、中间产物改名过来）。
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("产物应存在于 %s: %v", src, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "live-i.mp3")); !errors.Is(err, os.ErrNotExist) {
		t.Error("中间产物应已被改名，不应残留")
	}

	info, err := ProbeMP3(src)
	if err != nil {
		t.Fatalf("产物无法解析: %v", err)
	}
	if info.BitrateKbps != 32 {
		t.Errorf("码率 = %d, 期望 32", info.BitrateKbps)
	}
	if info.SampleRate != 22050 {
		t.Errorf("采样率 = %d, 期望 22050", info.SampleRate)
	}
	if info.Channels != 1 {
		t.Errorf("声道数 = %d, 期望 1（应混为单声道）", info.Channels)
	}
}

// TestTranscodePreservesDuration 验证重采样后时长基本不变。
//
// 这是对"编码器帧参数未随码率重算导致时长严重偏短"这一坑的回归防护：
// Shine 的 NewEncoder 内部按默认 128k 算好帧参数，换码率必须自己重算，
// 否则写出的帧头与帧长不匹配。
func TestTranscodePreservesDuration(t *testing.T) {
	dir := t.TempDir()
	const seconds = 3
	src := filepath.Join(dir, "live.mp3")
	genMP3(t, src, 128, 44100, seconds, 440)

	log := logging.New("test")
	if err := NewTranscoder("", "32k", log).TranscodeFile(context.Background(), src); err != nil {
		t.Fatalf("转码失败: %v", err)
	}

	got := decodedSeconds(t, src)
	// 允许 10% 误差（编码器逐帧处理，首尾会有半帧级的差异）。
	if got < float64(seconds)*0.9 || got > float64(seconds)*1.1 {
		t.Errorf("时长 = %.2fs, 期望约 %ds", got, seconds)
	}
}

// TestTranscodeInvalidInput 验证非 MP3 输入给出可操作的错误信息。
func TestTranscodeInvalidInput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bogus.mp3")
	if err := os.WriteFile(src, []byte("不是音频数据"), 0o644); err != nil {
		t.Fatal(err)
	}

	log := logging.New("test")
	err := NewTranscoder("", "32k", log).TranscodeFile(context.Background(), src)
	if err == nil {
		t.Fatal("非法输入应返回错误")
	}
	// 错误信息要指出"只支持 MP3"，用户才知道该怎么办。
	if !contains(err.Error(), "MP3") {
		t.Errorf("错误信息应提示仅支持 MP3，实际: %v", err)
	}
	// 失败后不应留下残缺的中间产物。
	if _, statErr := os.Stat(filepath.Join(dir, "bogus-i.mp3")); statErr == nil {
		t.Error("失败后不应残留中间产物")
	}
}

// TestTranscodeExistingIntermediate 验证中间产物已存在时直接改名，不重复编码。
func TestTranscodeExistingIntermediate(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.mp3")
	genMP3(t, src, 128, 44100, 1, 440)

	// 预置一个中间产物。
	tmp := filepath.Join(dir, "live-i.mp3")
	genMP3(t, tmp, 32, 22050, 1, 440)
	before, _ := os.ReadFile(tmp)

	log := logging.New("test")
	if err := NewTranscoder("", "32k", log).TranscodeFile(context.Background(), src); err != nil {
		t.Fatalf("转码失败: %v", err)
	}

	if _, err := os.Stat(src); err != nil {
		t.Fatal("中间产物应已改名为最终文件名")
	}
	after, _ := os.ReadFile(src)
	if string(before) != string(after) {
		t.Error("应直接复用已有中间产物，内容不应变化")
	}
}

// TestAvailableRejectsBadBitrate 验证不可用的码率会在自检阶段被发现。
func TestAvailableRejectsBadBitrate(t *testing.T) {
	log := logging.New("test")
	for _, bad := range []string{"999k", "abc", ""} {
		tc := &Transcoder{bitrate: bad, log: log}
		if bad == "" {
			// 空串在 NewTranscoder 里会被填默认值，这里直接构造才可能为空。
			if err := tc.Available(); err == nil {
				t.Errorf("空码率应被拒绝")
			}
			continue
		}
		if err := tc.Available(); err == nil {
			t.Errorf("码率 %q 应被拒绝", bad)
		}
	}
}

// TestResampleLinear 验证重采样长度与端点行为。
func TestResampleLinear(t *testing.T) {
	in := []int16{0, 10, 20, 30, 40, 50, 60, 70}
	out := resampleLinear(in, 44100, 22050)
	if len(out) != len(in)/2 {
		t.Errorf("降采样后长度 = %d, 期望 %d", len(out), len(in)/2)
	}

	same := resampleLinear(in, 44100, 44100)
	if len(same) != len(in) {
		t.Errorf("同采样率应原样返回，长度 = %d", len(same))
	}

	if got := resampleLinear(nil, 44100, 22050); got != nil {
		t.Error("空输入应返回 nil")
	}
}

// TestPCMToMono 验证立体声交错的混音与残字节处理。
func TestPCMToMono(t *testing.T) {
	// 两组立体声帧：L=100,R=200 → 150；L=-100,R=-200 → -150
	b := []byte{
		100, 0, 200, 0,
		0x9C, 0xFF, 0x38, 0xFF, // -100, -200
	}
	out := pcmToMono(b)
	if len(out) != 2 {
		t.Fatalf("应产出 2 个样本，实际 %d", len(out))
	}
	if out[0] != 150 {
		t.Errorf("第 1 个样本 = %d, 期望 150", out[0])
	}
	if out[1] != -150 {
		t.Errorf("第 2 个样本 = %d, 期望 -150", out[1])
	}

	// 尾部不足 4 字节应被忽略，且不 panic。
	if got := pcmToMono([]byte{1, 0, 2, 0, 3}); len(got) != 1 {
		t.Errorf("残字节应被忽略，实际产出 %d 个样本", len(got))
	}
}

// ---- 目录处理 ----

// TestProcessorScanFilters 验证扫描过滤规则：静置时间、低码率 MP3、-i.mp3、归档目录。
func TestProcessorScanFilters(t *testing.T) {
	dir := t.TempDir()
	histDir := filepath.Join(dir, "历史")
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)

	// 1. 正常的旧高码率 mp3，应入队。
	ready := filepath.Join(dir, "ready.mp3")
	genMP3(t, ready, 128, 44100, 1, 440)
	_ = os.Chtimes(ready, old, old)

	// 2. 刚写入的 mp3，应被静置时间过滤。
	fresh := filepath.Join(dir, "fresh.mp3")
	genMP3(t, fresh, 128, 44100, 1, 440)
	_ = os.Chtimes(fresh, time.Now(), time.Now())

	// 3. 低码率 mp3（等于输出码率），应被过滤。
	low := filepath.Join(dir, "low.mp3")
	genMP3(t, low, 32, 44100, 1, 440)
	_ = os.Chtimes(low, old, old)

	// 4. 中间产物，应被过滤。
	mid := filepath.Join(dir, "mid-i.mp3")
	genMP3(t, mid, 128, 44100, 1, 440)
	_ = os.Chtimes(mid, old, old)

	// 5. 历史目录中的 mp3，应被过滤。
	inHist := filepath.Join(histDir, "archived.mp3")
	genMP3(t, inHist, 128, 44100, 1, 440)
	_ = os.Chtimes(inHist, old, old)

	// 6. 不支持格式，应被过滤（并触发一次告警）。
	ts := filepath.Join(dir, "stream.ts")
	if err := os.WriteFile(ts, []byte("fake ts"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(ts, old, old)

	log := logging.New("test")
	p, err := NewProcessor(Options{
		WatchDir:      dir,
		CheckInterval: time.Hour,
		StableDelay:   60 * time.Second,
		ArchiveAfter:  45 * time.Hour,
		Transcoder:    NewTranscoder("", "32k", log),
		Log:           log,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.scanOnce()

	got := map[string]bool{}
	for {
		item, ok := p.queue.Pop()
		if !ok {
			break
		}
		got[filepath.Base(item)] = true
	}

	if !got["ready.mp3"] {
		t.Error("ready.mp3 应入队")
	}
	for _, unwanted := range []string{"fresh.mp3", "low.mp3", "mid-i.mp3", "archived.mp3", "stream.ts"} {
		if got[unwanted] {
			t.Errorf("%s 不应入队", unwanted)
		}
	}

	// 不支持的格式要告警，且同一文件只告警一次。
	p.scanOnce()
	if !p.warnedUnsupported[ts] {
		t.Error("stream.ts 应被记录为已告警")
	}
}

// TestProcessedMP3NotRequeued 验证转码产物（32k）不会被反复重新入队。
// 这是对"输出码率低于跳过阈值导致无限重编码"这一回归的防护。
func TestProcessedMP3NotRequeued(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "live.mp3")
	genMP3(t, src, 128, 44100, 1, 440)
	old := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(src, old, old)

	log := logging.New("test")
	tc := NewTranscoder("", "32k", log)
	if err := tc.TranscodeFile(context.Background(), src); err != nil {
		t.Fatalf("转码失败: %v", err)
	}

	info, err := ProbeMP3(src)
	if err != nil {
		t.Fatal(err)
	}
	if info.BitrateKbps != 32 {
		t.Fatalf("产物码率 = %d, 期望 32", info.BitrateKbps)
	}

	p, err := NewProcessor(Options{
		WatchDir:      dir,
		CheckInterval: time.Hour,
		StableDelay:   time.Second,
		ArchiveAfter:  45 * time.Hour,
		Transcoder:    tc,
		Log:           log,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.scanOnce()

	if n := p.queue.Len(); n != 0 {
		t.Errorf("32k 转码产物不应再次入队，实际入队 %d 个", n)
	}
}

// TestNoRepeatTranscodeDuringProcessing 是防回归测试。
//
// 历史缺陷：转码过程中会先删除源文件、再把中间产物改名为源文件名。
// 若扫描恰好在"源文件已删除、改名尚未完成"的窗口内运行，
// 查不到文件导致防重判据失效，于是同一文件被无限次重新编码。
// 修复方式是在开始转码前先认领文件；本测试通过在同一路径反复扫描来锁定该行为。
func TestNoRepeatTranscodeDuringProcessing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.mp3")
	genMP3(t, src, 128, 44100, 1, 440)
	old := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(src, old, old)

	log := logging.New("test")
	p, err := NewProcessor(Options{
		WatchDir:      dir,
		CheckInterval: time.Hour,
		StableDelay:   time.Second,
		ArchiveAfter:  45 * time.Hour,
		Transcoder:    NewTranscoder("", "32k", log),
		Log:           log,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 第 1 轮：应完成一次转换。
	p.scanOnce()
	if n := p.queue.Len(); n != 1 {
		t.Fatalf("首轮应入队 1 个文件，实际 %d", n)
	}
	item, _ := p.queue.Pop()
	p.claim(item)
	if err := p.transcoder.TranscodeFile(context.Background(), item); err != nil {
		t.Fatalf("转码失败: %v", err)
	}
	p.markProcessed(item)

	// 后续多轮扫描：均不应再次入队。
	for round := 2; round <= 5; round++ {
		p.scanOnce()
		if n := p.queue.Len(); n != 0 {
			t.Fatalf("第 %d 轮扫描不应再入队，实际入队 %d 个（防重复转码失效）", round, n)
		}
	}

	info, err := ProbeMP3(src)
	if err != nil {
		t.Fatal(err)
	}
	if info.BitrateKbps != 32 {
		t.Errorf("最终码率 = %d, 期望 32", info.BitrateKbps)
	}
}

// TestArchiveOldMP3 验证过期 MP3 会被移入归档目录，且文件名冲突时自动加时间戳。
func TestArchiveOldMP3(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "子目录")
	histDir := filepath.Join(dir, "历史")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatal(err)
	}

	log := logging.New("test")
	p, err := NewProcessor(Options{
		WatchDir:      dir,
		HistoryDir:    histDir,
		CheckInterval: time.Hour,
		ArchiveAfter:  45 * time.Hour,
		Transcoder:    NewTranscoder("", "32k", log),
		Log:           log,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 旧文件（子目录中）应被归档。
	oldFile := filepath.Join(sub, "old.mp3")
	genMP3(t, oldFile, 32, 22050, 1, 440)
	oldTime := time.Now().Add(-50 * time.Hour)
	_ = os.Chtimes(oldFile, oldTime, oldTime)

	// 新文件不应被归档。
	newFile := filepath.Join(dir, "new.mp3")
	genMP3(t, newFile, 32, 22050, 1, 440)

	// 归档目录里预置同名文件，触发时间戳后缀分支。
	if err := os.WriteFile(filepath.Join(histDir, "old.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p.archiveOldMP3()

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("旧文件应已从原位置移走")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Error("新文件不应被移动")
	}

	entries, err := os.ReadDir(histDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// 期望：原 old.mp3 + 带时间戳的新归档文件 = 2 个。
	if len(names) != 2 {
		t.Errorf("归档目录应有 2 个文件（含时间戳重命名），实际 %v", names)
	}
}

// ---- 辅助 ----

// decodedSeconds 用 go-mp3 解码并计算时长。
func decodedSeconds(t *testing.T, path string) float64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dec, err := gomp3.NewDecoder(f)
	if err != nil {
		t.Fatalf("解码 %s 失败: %v", path, err)
	}
	rate := dec.SampleRate()

	buf := make([]byte, 32*1024)
	total := 0
	for {
		n, rerr := dec.Read(buf)
		total += n
		if rerr != nil {
			break
		}
	}
	// go-mp3 恒输出双声道 16 位，即每帧 4 字节。
	return float64(total/4) / float64(rate)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
