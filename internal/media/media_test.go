package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// ---- 测试素材生成 ----

// requireFFmpeg 在环境缺 ffmpeg（或 ffmpeg 不带所需输入格式）时跳过测试。
//
// 转码相关用例真的会去调 ffmpeg，缺了它只能跳过而不是失败——
// 否则在没有 ffmpeg 的开发机（如只跑码率解析的单测）上会得到假阳性。
// 用 t.Skip 而非 t.Fatal，并在提示里说明原因，便于区分"环境不全"与"真的挂了"。
//
// 这里额外探测 lavfi 是否可用。原因：本项目镜像里带的是**为 MP3 转码裁剪过**的
// ffmpeg（docker/ffmpeg），它只有 mp3 demuxer，既没有 lavfi 也没有 s16le，
// 因而无法凭空"造"出音频，只能 MP3→MP3 重编码。若有人在容器里跑这些测试，
// 用 lavfi 造素材的那几条会以一个看不懂的 "Unknown input format: 'lavfi'"
// 失败。提前探测并给出明确提示，比让人对着那句报错猜要好。
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(FFmpegBinary); err != nil {
		t.Skipf("跳过：环境没有 %s，无法验证转码（%v）", FFmpegBinary, err)
	}
	if !supportsLavfi() {
		t.Skipf("跳过：当前 %s 不带 lavfi（多半是镜像里那个裁剪版），"+
			"无法合成测试素材；请用带 lavfi 的完整版 ffmpeg 跑测试", FFmpegBinary)
	}
}

// requireAnyFFmpeg 只要求环境里有 ffmpeg，不要求它带 lavfi。
//
// 适用于不需要合成素材的用例——它们只调 ffmpeg 做 MP3→MP3 重编码，
// 镜像里那个裁剪版也满足。与 requireFFmpeg 区分开，
// 是为了让这类用例在裁剪版上也能真跑，而不是被一并跳过。
func requireAnyFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(FFmpegBinary); err != nil {
		t.Skipf("跳过：环境没有 %s（%v）", FFmpegBinary, err)
	}
}

// supportsLavfi 探测 ffmpeg 是否支持 lavfi 虚拟输入设备。
func supportsLavfi() bool {
	out, err := exec.Command(FFmpegBinary, "-hide_banner", "-demuxers").Output()
	if err != nil {
		// 拿不到列表就假定支持——这条探测只是为了让跳过信息更友好，
		// 不该因为它自己失败而误跳过真正该跑的测试。
		return true
	}
	return strings.Contains(string(out), "lavfi")
}

// genMP3 合成一段指定码率、指定采样率的 MP3，用作测试素材。
//
// 用 ffmpeg 自身来造素材（而非另找一个编码器）：这样测试素材与生产转码
// 走同一条编码路径，产物的 Xing 头、帧长等特征与实际情形一致，
// 不会出现"测试素材太规整、掩盖了真实文件才会触发的解析问题"。
func genMP3(t *testing.T, path string, bitrateKbps, sampleRate, seconds int, toneHz float64) {
	t.Helper()
	requireFFmpeg(t)

	// 用 lavfi 的正弦波发生器直接产出目标码率的 MP3。
	// 声道数固定 2，与生产输出一致（本项目的转码产物是立体声）。
	cmd := exec.Command(FFmpegBinary,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi",
		"-i", fmt.Sprintf("sine=frequency=%g:duration=%d:sample_rate=%d", toneHz, seconds, sampleRate),
		"-b:a", fmt.Sprintf("%dk", bitrateKbps),
		"-ac", "2",
		path, "-y",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("生成测试 MP3 失败: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
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
	requireFFmpeg(t)
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
	// 44.1kHz / 立体声：与实际转码路径保持一致的期望值。
	// 参数选择的理由见 TestTranscodeOutputFormat 的注释。
	if info.SampleRate != 44100 {
		t.Errorf("采样率 = %d, 期望 44100", info.SampleRate)
	}
	if info.Channels != 2 {
		t.Errorf("声道数 = %d, 期望 2（立体声）", info.Channels)
	}
}

// TestTranscodePreservesDuration 验证转码后时长基本不变。
//
// 这是对"编码器帧参数未随码率重算导致时长严重偏短"这一坑的回归防护。
// 早期纯 Go 实现需要自己重算 Shine 的帧参数，否则写出的帧头与帧长不匹配；
// 现在交给 ffmpeg 了，但这条断言依然有价值——它守的是最终行为，
// 而不关心底层是哪个编码器。
func TestTranscodePreservesDuration(t *testing.T) {
	requireFFmpeg(t)
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
	requireFFmpeg(t)
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
	requireFFmpeg(t)
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

// TestAvailableDetectsFFmpeg 验证自检会真的去找 ffmpeg 可执行文件。
//
// 这是新旧实现最本质的区别：纯 Go 版没有外部依赖，Available() 只能查码率；
// 现在真的依赖一个外部二进制，缺失时必须在启动阶段就报错，
// 而不是等第一次转码才失败（那时用户看到的是一堆"处理文件失败"，很难联想到根因）。
func TestAvailableDetectsFFmpeg(t *testing.T) {
	log := logging.New("test")

	// 1) 正常环境：PATH 里有 ffmpeg 时应当通过。
	//
	// 这里刻意只用"存在性"守卫（requireAnyFFmpeg），不用 requireFFmpeg：
	// 本用例不合成素材，裁剪版的 ffmpeg 也完全能跑。
	// 而且它正是最该在裁剪版上验证的一条——镜像里放的就是那个二进制。
	requireAnyFFmpeg(t)
	tc := NewTranscoder("", "32k", log)
	if err := tc.Available(); err != nil {
		t.Fatalf("环境有 ffmpeg 但自检失败: %v", err)
	}
	if tc.bin == "" {
		t.Error("Available 成功后应缓存解析到的路径")
	}

	// 2) 把 PATH 清空，模拟镜像里漏掉了可执行文件。
	//    这是回归测试：确保缺依赖时真的会失败，而不是悄悄放过去。
	t.Setenv("PATH", t.TempDir())
	if err := NewTranscoder("", "32k", log).Available(); err == nil {
		t.Error("PATH 中没有 ffmpeg 时自检应当失败")
	} else if !contains(err.Error(), FFmpegBinary) {
		t.Errorf("错误信息应点名缺失的可执行文件，实际: %v", err)
	}
}

// TestTranscodeOutputFormat 验证转码产物的关键参数。
//
// 钉住两件事：
//  1. 输出是 44.1kHz —— 旧实现固定降采样到 22050Hz。实测同码率下体积几乎相同
//     （117.6 vs 117.4 KB），降采样换不来空间，只损失音质，因此改为保持 44.1kHz。
//  2. 输出是立体声 —— 同理。
//
// 这两项是刻意选的默认值，改动会直接影响所有既有用户的听感，值得用测试锁住。
func TestTranscodeOutputFormat(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp3")
	// 源用 44.1kHz 立体声，确保"保持原样"与"降采样"能被区分开。
	genMP3(t, src, 128, 44100, 2, 440)

	log := logging.New("test")
	if err := NewTranscoder("", "32k", log).TranscodeFile(context.Background(), src); err != nil {
		t.Fatalf("转码失败: %v", err)
	}

	probe, err := ProbeMP3(src)
	if err != nil {
		t.Fatalf("解析产物失败: %v", err)
	}
	if probe.SampleRate != 44100 {
		t.Errorf("输出采样率 = %d, 期望 44100（不应降采样）", probe.SampleRate)
	}
	if probe.Channels != 2 {
		t.Errorf("输出声道数 = %d, 期望 2（立体声）", probe.Channels)
	}
	if probe.BitrateKbps != 32 {
		t.Errorf("输出码率 = %d, 期望 32", probe.BitrateKbps)
	}
}

// ---- 目录处理 ----

// TestProcessorScanFilters 验证扫描过滤规则：静置时间、低码率 MP3、-i.mp3、归档目录。
func TestProcessorScanFilters(t *testing.T) {
	requireFFmpeg(t)
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
	requireFFmpeg(t)
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
	requireFFmpeg(t)
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
	requireFFmpeg(t)
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

// decodedSeconds 计算 MP3 的时长（秒）。
//
// 原先用 go-mp3 解码数采样数来算。删除纯 Go 转码器后没有解码器了，
// 改用 ffmpeg 读取容器时长——比逐帧解码更直接，且与生产转码走同一个工具。
func decodedSeconds(t *testing.T, path string) float64 {
	t.Helper()
	requireFFmpeg(t)

	// 只打印容器信息时 ffmpeg 会把数据输出到 stderr 并以非 0 退出，
	// 所以这里不能按"失败"处理，得解析 stderr 拿 Duration。
	cmd := exec.Command(FFmpegBinary, "-hide_banner", "-i", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()

	// 形如 "  Duration: 00:00:02.00, start: ..."
	re := regexp.MustCompile(`Duration:\s*(\d+):(\d+):(\d+\.?\d*)`)
	m := re.FindStringSubmatch(stderr.String())
	if m == nil {
		t.Fatalf("无法从 ffmpeg 输出中解析时长:\n%s", stderr.String())
	}
	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	sec, _ := strconv.ParseFloat(m[3], 64)
	return float64(h)*3600 + float64(min)*60 + sec
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
