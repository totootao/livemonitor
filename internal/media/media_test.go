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

// ---- Xing/Info 信息帧回归测试 ----
//
// 背景：LAME 在 CBR 文件开头写一个 Xing/Info "信息帧"。它不是音频数据，
// 且其帧头里的码率字段常与实际音频码率不一致——实测 `ffmpeg -b:a 32k`
// 产出的文件，信息帧写的是 56k，真正的音频帧才是 32k。
//
// 旧实现直接取首个合法帧头的码率，于是把 32k 文件判成 56k，超过输出阈值
// 32k，导致这个文件每轮扫描都被重新压一遍（音质白掉一次）。
//
// 下面这 858 字节是真实 LAME 输出的文件头（ID3v2 + 信息帧 + 6 个音频帧）。

// lame32kFixture 是一段真实 LAME 编码的 32k CBR 单声道文件的开头部分。
var lame32kFixture = []byte{
	0x49, 0x44, 0x33, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x23, 0x54, 0x53, 0x53, 0x45, 0x00, 0x00,
	0x00, 0x0F, 0x00, 0x00, 0x03, 0x4C, 0x61, 0x76, 0x66, 0x36, 0x32, 0x2E, 0x31, 0x33, 0x2E, 0x31,
	0x30, 0x32, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFB, 0x40,
	0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x49, 0x6E, 0x66, 0x6F, 0x00, 0x00, 0x00, 0x0F, 0x00, 0x00, 0x00, 0xE7, 0x00, 0x00,
	0x5E, 0xFF, 0x00, 0x05, 0x07, 0x09, 0x0C, 0x0F, 0x11, 0x14, 0x16, 0x19, 0x1C, 0x1E, 0x20, 0x24,
	0x26, 0x28, 0x2A, 0x2D, 0x30, 0x32, 0x35, 0x37, 0x3A, 0x3D, 0x3F, 0x41, 0x45, 0x47, 0x49, 0x4B,
	0x4E, 0x51, 0x53, 0x56, 0x58, 0x5B, 0x5E, 0x60, 0x62, 0x66, 0x68, 0x6A, 0x6D, 0x6F, 0x72, 0x74,
	0x77, 0x79, 0x7C, 0x7F, 0x81, 0x83, 0x87, 0x89, 0x8B, 0x8E, 0x90, 0x93, 0x95, 0x98, 0x9A, 0x9D,
	0xA0, 0xA2, 0xA4, 0xA8, 0xAA, 0xAC, 0xAF, 0xB1, 0xB4, 0xB7, 0xB9, 0xBB, 0xBE, 0xC1, 0xC3, 0xC5,
	0xC9, 0xCB, 0xCD, 0xD0, 0xD2, 0xD5, 0xD8, 0xDA, 0xDC, 0xDE, 0xE2, 0xE4, 0xE6, 0xE9, 0xEC, 0xEE,
	0xF1, 0xF3, 0xF6, 0xF9, 0xFB, 0xFD, 0x00, 0x00, 0x00, 0x00, 0x4C, 0x61, 0x76, 0x63, 0x36, 0x32,
	0x2E, 0x33, 0x30, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x24,
	0x03, 0xA8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x5E, 0xFF, 0xD9, 0xF6, 0x0E, 0xD2, 0x00, 0x00,
	0x00, 0x00, 0x00, 0xFF, 0xFB, 0x10, 0xC4, 0x00, 0x00, 0x04, 0x74, 0x13, 0x55, 0x54, 0x90, 0x80,
	0x30, 0xA6, 0x09, 0xAF, 0x37, 0x1A, 0x20, 0x02, 0x00, 0x01, 0xAD, 0x39, 0x40, 0x00, 0x01, 0x59,
	0x3A, 0x3D, 0x50, 0x50, 0x08, 0x06, 0x09, 0x01, 0xF0, 0x7C, 0x1F, 0x07, 0xCA, 0x02, 0x00, 0x80,
	0x61, 0x10, 0x7C, 0x1F, 0xD4, 0x08, 0x3B, 0x13, 0x87, 0xF8, 0x83, 0x70, 0x04, 0x93, 0xF6, 0xC0,
	0x60, 0x38, 0x1C, 0x0E, 0x00, 0x00, 0x00, 0x00, 0x00, 0x28, 0x89, 0x2A, 0x99, 0x14, 0x64, 0x08,
	0xE9, 0x02, 0x48, 0x16, 0xA3, 0xF7, 0x85, 0x01, 0xF0, 0x13, 0x1B, 0xF0, 0x22, 0x94, 0x2F, 0xA8,
	0x1A, 0x12, 0xFC, 0x24, 0x0D, 0x2A, 0x0A, 0x00, 0x18, 0x30, 0x00, 0xFF, 0xFB, 0x12, 0xC4, 0x02,
	0x83, 0xC5, 0x58, 0x1D, 0x20, 0x1D, 0xE0, 0x00, 0x28, 0x9B, 0x03, 0xE3, 0x41, 0xAF, 0x68, 0x4C,
	0xCC, 0x09, 0x00, 0xBC, 0x40, 0x04, 0x86, 0x00, 0xE0, 0x78, 0x67, 0xEE, 0xF6, 0xA6, 0x63, 0x03,
	0x96, 0x61, 0xC4, 0x11, 0x26, 0x0C, 0x00, 0x7E, 0x60, 0x42, 0x06, 0x06, 0x05, 0x20, 0x4C, 0x60,
	0x5E, 0x03, 0xC5, 0x9A, 0xB4, 0x95, 0x87, 0x98, 0x20, 0x86, 0x64, 0xF9, 0xC4, 0xB4, 0x61, 0x7E,
	0x28, 0xA6, 0xAB, 0xD4, 0xA2, 0x6A, 0x7E, 0x28, 0xA6, 0x18, 0x40, 0xCC, 0x73, 0xDE, 0x99, 0xF4,
	0xA6, 0x68, 0xF1, 0x9B, 0x8E, 0x62, 0x42, 0xB8, 0x94, 0xE0, 0x9B, 0xBE, 0x9A, 0x30, 0xD0, 0xE3,
	0x0E, 0x1D, 0x31, 0xD3, 0xFF, 0xFB, 0x10, 0xC4, 0x03, 0x83, 0xC5, 0x04, 0x1F, 0x18, 0x0D, 0xFB,
	0x22, 0x40, 0xAD, 0x84, 0x62, 0x81, 0xBF, 0x6C, 0x48, 0x83, 0x3E, 0x83, 0x30, 0xAF, 0x1B, 0xE3,
	0x50, 0xCE, 0x1C, 0x34, 0xEB, 0x1B, 0x43, 0x09, 0xE0, 0x6D, 0x35, 0xCC, 0x02, 0x06, 0x6E, 0xA8,
	0x75, 0x7E, 0x6B, 0x06, 0xC9, 0xA5, 0xA1, 0xCF, 0xD7, 0xF4, 0x24, 0x41, 0x89, 0x88, 0x99, 0xB1,
	0xC1, 0xBB, 0xC6, 0x98, 0x97, 0x0E, 0xD1, 0xC1, 0x5F, 0x5C, 0x1B, 0xFC, 0x0E, 0xE1, 0x89, 0xA8,
	0x4F, 0x9C, 0x23, 0x21, 0x9E, 0x23, 0x19, 0x99, 0xF9, 0x99, 0x3C, 0x18, 0xB0, 0xB3, 0x07, 0x8C,
	0x53, 0xE0, 0x1F, 0xFA, 0xBE, 0x8A, 0x30, 0xA1, 0x13, 0x0F, 0x1C, 0x31, 0xFF, 0xFB, 0x12, 0xC4,
	0x03, 0x03, 0xC5, 0x00, 0x1F, 0x18, 0x0D, 0xFB, 0x22, 0x40, 0xAB, 0x04, 0x22, 0x81, 0xBF, 0x6C,
	0x48, 0xA3, 0xB3, 0x3D, 0x86, 0x30, 0xA6, 0x1C, 0x93, 0x4D, 0xBE, 0x6B, 0x34, 0xB8, 0x1B, 0xF3,
	0x09, 0x70, 0x70, 0x34, 0x91, 0x01, 0x04, 0x6F, 0x26, 0x76, 0xF8, 0x6B, 0x84, 0xCD, 0x67, 0x83,
	0x7F, 0xAF, 0xE8, 0x47, 0xF3, 0x14, 0x0E, 0x33, 0x93, 0x53, 0x7A, 0x84, 0x31, 0x35, 0x1C, 0x73,
	0x85, 0x6E, 0x21, 0x38, 0x23, 0x1C, 0x93, 0x13, 0xA0, 0x9A, 0x38, 0x56, 0x53, 0x3B, 0x47, 0x33,
	0x03, 0xE3, 0x2F, 0x7D, 0x31, 0x51, 0x75, 0xD1, 0x2F, 0xA8, 0x0E, 0x7E, 0xCF, 0xBA, 0x30, 0xB1,
	0x03, 0x0C, 0x1F, 0x31, 0x93, 0xFF, 0xFB, 0x10, 0xC4, 0x03, 0x83, 0xC5, 0x00, 0x1F, 0x18, 0x0D,
	0xFB, 0x22, 0x40, 0xA7, 0x04, 0x22, 0x81, 0xAF, 0x6C, 0x48, 0xC3, 0x38, 0x89, 0x30, 0x9F, 0x1D,
	0x23, 0x4A, 0x4E, 0xB4, 0x34, 0x85, 0x1C, 0x83, 0x08, 0xC0, 0x75, 0x33, 0x54, 0x03, 0x0A, 0x70,
	0xA0, 0x77, 0x72, 0x6C, 0x02, 0xCD, 0xA7, 0x43, 0x9F, 0xA7, 0xE8, 0x49, 0x03, 0x2E, 0x34, 0xDD,
	0x30, 0x3F, 0xBF, 0xCC, 0x4E, 0x86, 0xC0, 0xE1, 0xFF, 0x60, 0x8E, 0x12, 0x86, 0xD0, 0xC4, 0xFC,
	0x25, 0x4E, 0x19, 0x88, 0xCE, 0x92, 0x0C, 0xBD, 0x04, 0xCB, 0x1F, 0x8C, 0x48, 0x61, 0x75, 0xCA,
	0x33, 0x04, 0xFF, 0x77, 0xAA, 0x30, 0xA1, 0x13, 0x0D, 0x1E, 0x31, 0x63, 0xD3, 0xFF, 0xFB, 0x12,
	0xC4, 0x04, 0x03, 0xC4, 0xFC, 0x1F, 0x18, 0x0D, 0xFB, 0x22, 0x40, 0x9C, 0x83, 0xE3, 0x01, 0xAF,
	0x68, 0x4C, 0x35, 0x8C, 0x30, 0x94, 0x1D, 0xD3, 0x46, 0xFE, 0xFB, 0x34, 0x4E, 0x1D, 0x33, 0x08,
	0x50, 0x78, 0x32, 0x19, 0x07, 0x14, 0x71, 0x1A, 0x78, 0xE8, 0x02, 0xC9, 0x7E, 0xCF, 0x06, 0xFF,
	0x4F, 0xD8, 0xAF, 0xCC, 0x50, 0x03, 0x4E, 0x74, 0xEE, 0x54, 0x30, 0xF1, 0x13, 0x73, 0x6A, 0x49,
	0xE5, 0x36, 0x7B, 0x13, 0xB3, 0x0F, 0x20, 0x66, 0x3C, 0x31, 0x4D, 0x2B, 0x13, 0x34, 0xB0, 0xCC,
	0xDD, 0x30, 0xE3, 0x5A, 0xE5, 0x38, 0x26, 0x73, 0xEB, 0x30, 0xA2, 0x8C, 0x31, 0x93, 0x1E, 0xD0,
	0xD3, 0x7B, 0x30, 0x79, 0x1A, 0x63, 0xFF, 0xFB, 0x10, 0xC4,
}

// TestProbeMP3SkipsXingInfoFrame 验证解析码率时会跳过 Xing/Info 信息帧。
func TestProbeMP3SkipsXingInfoFrame(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lame32k.mp3")
	if err := os.WriteFile(p, lame32kFixture, 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := ProbeMP3(p)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	// 必须拿到音频帧的 32k，而不是信息帧的 56k。
	if info.BitrateKbps != 32 {
		t.Errorf("码率 = %d kbps, 期望 32（信息帧里写的是 56，不应采信）", info.BitrateKbps)
	}
	if info.SampleRate != 44100 {
		t.Errorf("采样率 = %d, 期望 44100", info.SampleRate)
	}
	if info.Channels != 1 {
		t.Errorf("声道数 = %d, 期望 1", info.Channels)
	}
	if info.Version != "MPEG-1" {
		t.Errorf("版本 = %q, 期望 MPEG-1", info.Version)
	}
}

// TestHasXingHeaderOffsets 验证 Xing/Info 标签偏移按版本与声道区分。
//
// 标签位置 = 4 字节帧头 + side info 长度，而 side info 长度取决于配置：
//
//	MPEG-1 立体声 32   MPEG-1 单声道 17
//	MPEG-2 立体声 17   MPEG-2 单声道 9
//
// 旧实现硬编码偏移 36（只对 MPEG-1 立体声成立），单声道文件永远检测不到。
func TestHasXingHeaderOffsets(t *testing.T) {
	mk := func(off int, tag string) []byte {
		frame := make([]byte, off+8)
		copy(frame[off:], tag)
		return frame
	}

	cases := []struct {
		name     string
		version  string
		channels int
		off      int
	}{
		{"MPEG-1 单声道", "MPEG-1", 1, 21},
		{"MPEG-1 立体声", "MPEG-1", 2, 36},
		{"MPEG-2 单声道", "MPEG-2", 1, 13},
		{"MPEG-2 立体声", "MPEG-2", 2, 21},
	}

	for _, c := range cases {
		for _, tag := range []string{"Xing", "Info"} {
			info := MP3Info{Version: c.version, Channels: c.channels}
			if !HasXingHeader(mk(c.off, tag), info) {
				t.Errorf("%s 的 %s 标签（偏移 %d）未被识别", c.name, tag, c.off)
			}
			// 偏移不对的位置不应误判。
			if HasXingHeader(mk(c.off+1, tag), info) {
				t.Errorf("%s 的 %s 在错误偏移处被误判为命中", c.name, tag)
			}
		}
	}

	// VBRI（Fraunhofer）固定偏移 32，与声道无关。
	if !HasXingHeader(mk(32, "VBRI"), MP3Info{Version: "MPEG-1", Channels: 1}) {
		t.Error("VBRI 标签未被识别")
	}
}
