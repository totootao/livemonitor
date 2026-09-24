package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// genMP3 用 ffmpeg 生成指定码率的测试 MP3，跳过需要网络的场景。
func genMP3(t *testing.T, path, bitrate string, seconds int) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("缺少 ffmpeg，跳过")
	}
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i",
		"anullsrc=r=44100:cl=mono", "-t", itoa(seconds), "-b:a", bitrate, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成测试 MP3 失败: %v (%s)", err, out)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestProbeMP3Bitrate 验证码率解析与 mutagen 行为一致。
func TestProbeMP3Bitrate(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		bitrate string
		want    int
	}{
		{"32k", 32},
		{"64k", 64},
		{"128k", 128},
	}
	for _, tc := range cases {
		t.Run(tc.bitrate, func(t *testing.T) {
			p := filepath.Join(dir, "sample-"+tc.bitrate+".mp3")
			genMP3(t, p, tc.bitrate, 1)

			info, err := ProbeMP3(p)
			if err != nil {
				t.Fatalf("ProbeMP3 失败: %v", err)
			}
			if info.BitrateKbps != tc.want {
				t.Errorf("码率 = %d kbps, 期望 %d kbps", info.BitrateKbps, tc.want)
			}
			if info.SampleRate != 44100 {
				t.Errorf("采样率 = %d, 期望 44100", info.SampleRate)
			}
			if info.Channels != 1 {
				t.Errorf("声道 = %d, 期望 1", info.Channels)
			}
		})
	}
}

// TestProbeMP3LowQualityThreshold 验证低码率判定，等价于原脚本的 < 100 判断。
func TestProbeMP3LowQualityThreshold(t *testing.T) {
	dir := t.TempDir()

	low := filepath.Join(dir, "low.mp3")
	genMP3(t, low, "32k", 1)
	info, err := ProbeMP3(low)
	if err != nil {
		t.Fatalf("ProbeMP3 失败: %v", err)
	}
	if !info.IsLowQuality(100) {
		t.Errorf("32k 应被判为低码率")
	}

	high := filepath.Join(dir, "high.mp3")
	genMP3(t, high, "128k", 1)
	info, err = ProbeMP3(high)
	if err != nil {
		t.Fatalf("ProbeMP3 失败: %v", err)
	}
	if info.IsLowQuality(100) {
		t.Errorf("128k 不应被判为低码率")
	}
}

// TestProbeMP3WithID3v2Tag 验证带 ID3v2 标签的文件也能正确跳过标签。
func TestProbeMP3WithID3v2Tag(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("缺少 ffmpeg，跳过")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp3")
	genMP3(t, src, "64k", 1)

	tagged := filepath.Join(dir, "tagged.mp3")
	cmd := exec.Command("ffmpeg", "-y", "-i", src, "-metadata", "title=测试标题",
		"-metadata", "artist=livemonitor", "-codec", "copy", tagged)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("写入标签失败: %v (%s)", err, out)
	}

	info, err := ProbeMP3(tagged)
	if err != nil {
		t.Fatalf("带 ID3v2 标签的文件解析失败: %v", err)
	}
	if info.BitrateKbps != 64 {
		t.Errorf("码率 = %d kbps, 期望 64 kbps", info.BitrateKbps)
	}
}

// TestProbeMP3Invalid 验证非法文件返回错误而不是 panic。
func TestProbeMP3Invalid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "not_mp3.mp3")
	if err := os.WriteFile(p, []byte("this is definitely not an mp3 file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeMP3(p); err == nil {
		t.Error("对非 MP3 内容应返回错误")
	}
}

// TestProbeMP3Empty 验证空文件不会 panic。
func TestProbeMP3Empty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.mp3")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeMP3(p); err == nil {
		t.Error("空文件应返回错误")
	}
}

// TestTranscodeAndCleanup 验证完整转码流程：产出 mp3、删除源文件、无 -i.mp3 残留。
func TestTranscodeAndCleanup(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("缺少 ffmpeg，跳过")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "video.mp4")
	// 用 lavfi 造一个带音轨的 mp4。
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo",
		"-t", "1", "-c:a", "aac", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成测试 mp4 失败: %v (%s)", err, out)
	}

	log := logging.New("test")
	tc := NewTranscoder("", "32k", log)
	if err := tc.TranscodeFile(context.Background(), src); err != nil {
		t.Fatalf("转码失败: %v", err)
	}

	final := filepath.Join(dir, "video.mp3")
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("期望产出 %s: %v", final, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("源文件应已被删除")
	}
	if _, err := os.Stat(filepath.Join(dir, "video-i.mp3")); !os.IsNotExist(err) {
		t.Errorf("中间产物 -i.mp3 不应残留")
	}

	info, err := ProbeMP3(final)
	if err != nil {
		t.Fatalf("产物解析失败: %v", err)
	}
	if info.BitrateKbps != 32 || info.SampleRate != 22050 || info.Channels != 1 {
		t.Errorf("产物参数不符: %s（期望 32k / 22050Hz / 单声道）", info.Describe())
	}
}

// TestProcessorScanFilters 验证扫描过滤规则：静置时间、低码率 MP3、-i.mp3、归档目录。
func TestProcessorScanFilters(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("缺少 ffmpeg，跳过")
	}
	dir := t.TempDir()
	histDir := filepath.Join(dir, "历史")
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. 正常的旧 mp4，应入队。
	ready := filepath.Join(dir, "ready.mp4")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono",
		"-t", "1", "-c:a", "aac", ready)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成 mp4 失败: %v (%s)", err, out)
	}
	old := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(ready, old, old)

	// 2. 刚写入的 mp4，应被静置时间过滤。
	fresh := filepath.Join(dir, "fresh.mp4")
	genMP3(t, fresh, "64k", 1)
	// genMP3 生成的是 mp3，这里改用 aac 更贴近场景；直接用 mp3 也可，扩展名在允许列表内。
	_ = os.Chtimes(fresh, time.Now(), time.Now())

	// 3. 低码率 mp3，应被过滤。
	low := filepath.Join(dir, "low.mp3")
	genMP3(t, low, "32k", 1)
	_ = os.Chtimes(low, old, old)

	// 4. 高码率 mp3（高于输出码率），应入队。
	high := filepath.Join(dir, "high.mp3")
	genMP3(t, high, "192k", 1)
	_ = os.Chtimes(high, old, old)

	// 5. 中间产物，应被过滤。
	mid := filepath.Join(dir, "mid-i.mp3")
	genMP3(t, mid, "128k", 1)
	_ = os.Chtimes(mid, old, old)

	// 6. 历史目录中的 mp3，应被过滤。
	inHist := filepath.Join(histDir, "archived.mp3")
	genMP3(t, inHist, "128k", 1)
	_ = os.Chtimes(inHist, old, old)

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

	if !got["ready.mp4"] {
		t.Error("ready.mp4 应入队")
	}
	if !got["high.mp3"] {
		t.Error("high.mp3 应入队")
	}
	for _, unwanted := range []string{"fresh.mp4", "low.mp3", "mid-i.mp3", "archived.mp3"} {
		if got[unwanted] {
			t.Errorf("%s 不应入队", unwanted)
		}
	}
}

// TestProcessedMP3NotRequeued 验证转码产物（32k）不会被反复重新入队。
// 这是对"输出码率低于跳过阈值导致无限重编码"这一回归的防护。
func TestProcessedMP3NotRequeued(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("缺少 ffmpeg，跳过")
	}
	dir := t.TempDir()

	// 造一个高码率源文件，转码后应得到 32k 产物。
	src := filepath.Join(dir, "live.mp3")
	genMP3(t, src, "192k", 1)
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
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("缺少 ffmpeg，跳过")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "live.mp3")
	genMP3(t, src, "192k", 1)
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

	// 确认产物为 32k，且未被反复重编码。
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
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("缺少 ffmpeg，跳过")
	}
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
	genMP3(t, oldFile, "32k", 1)
	oldTime := time.Now().Add(-50 * time.Hour)
	_ = os.Chtimes(oldFile, oldTime, oldTime)

	// 新文件不应被归档。
	newFile := filepath.Join(dir, "new.mp3")
	genMP3(t, newFile, "32k", 1)

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
