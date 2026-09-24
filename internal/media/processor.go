// Package media 的视频目录处理器：扫描、转码、归档。
package media

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// VideoExtensions 是被识别为媒体的扩展名集合。
//
// 只保留 MP3：转码改为纯 Go 实现后，解码端只支持 MP3，
// 其他格式没有可用的纯 Go 解码器（见 transcoder.go 的说明）。
// 扫描到曾经的视频/音频扩展名时会给出告警日志，提示需要先转成 MP3。
var VideoExtensions = map[string]bool{
	".mp3": true,
}

// unsupportedMediaExtensions 是"看起来像媒体但我们处理不了"的扩展名。
// 命中时打一条告警，避免用户把 .ts 丢进来后困惑于"为什么一直没动静"。
var unsupportedMediaExtensions = map[string]bool{
	".mp4": true, ".avi": true, ".mov": true, ".flv": true, ".mkv": true,
	".wmv": true, ".mpeg": true, ".mpg": true, ".ts": true, ".m4a": true,
	".aac": true, ".opus": true, ".wav": true, ".flac": true, ".ogg": true,
}

// HistoryDirName 是归档子目录名。
const HistoryDirName = "历史"

// Queue 是一个去重的待转码任务队列。
type Queue struct {
	mu    sync.Mutex
	items []string
	seen  map[string]bool
}

// NewQueue 创建任务队列。
func NewQueue() *Queue { return &Queue{seen: make(map[string]bool)} }

// Push 入队，重复路径会被忽略。
func (q *Queue) Push(path string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.seen[path] {
		return
	}
	q.seen[path] = true
	q.items = append(q.items, path)
}

// Len 返回待处理任务数。
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Pop 取出一个任务。
func (q *Queue) Pop() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return "", false
	}
	p := q.items[0]
	q.items = q.items[1:]
	delete(q.seen, p)
	return p, true
}

// Processor 监控媒体目录，转码新文件并归档旧 MP3。
type Processor struct {
	watchDir      string
	historyDir    string
	checkInterval time.Duration
	stableDelay   time.Duration
	archiveAfter  time.Duration
	// outputBitrateKbps 是转码输出码率（kbps）。码率不高于它的 MP3 无需重转。
	outputBitrateKbps int

	transcoder *Transcoder
	log        *logging.Logger

	queue *Queue

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	// cancelScan 只取消扫描循环，保留队列用于退出时排空。
	cancelScan context.CancelFunc
	done       chan struct{}
	wg         sync.WaitGroup

	// 转码失败重试控制：路径 -> 上次失败时间。
	failMu    sync.Mutex
	failTimes map[string]time.Time

	// 不受支持格式的告警去重：路径 -> 已告警。
	warnMu            sync.Mutex
	warnedUnsupported map[string]bool

	// 已处理记录：路径 -> 处理完成时该文件的 (大小, 修改时间)。
	// 这是防止"转码产物被反复重新编码"的关键：
	// 产物与源文件同名，仅凭扩展名/码率无法区分"新录制文件"与"自己的输出"，
	// 因此记录处理结果，只有文件在此之后确实发生变化时才再次处理。
	doneMu  sync.Mutex
	doneSet map[string]fileStamp
}

// fileStamp 记录文件的处理状态，用于避免重复转码。
type fileStamp struct {
	// claimed 表示该文件已被认领（正在处理或已处理完成）。
	claimed bool
	// finished 表示处理已完成；为 false 时说明仍在处理中。
	finished bool
	// size/modTime 是处理完成时的文件状态，用于识别后续改动。
	size    int64
	modTime time.Time
}

// Options 是 Processor 的构造参数。
type Options struct {
	WatchDir      string
	HistoryDir    string
	CheckInterval time.Duration
	StableDelay   time.Duration
	ArchiveAfter  time.Duration
	// OutputBitrate 是转码输出码率（如 "32k"），用于判定已有 MP3 是否需要重转。
	// 留空时取 DefaultOutputBitrate。
	OutputBitrate string
	Transcoder    *Transcoder
	Log           *logging.Logger
}

// DefaultOutputBitrate 是默认转码输出码率。
const DefaultOutputBitrate = "32k"

// retryCooldown 是同一文件两次转码尝试之间的最小间隔。
const retryCooldown = 10 * time.Minute

// BitrateToKbps 把 "32k" 这类码率字符串转换为 kbps 数值。
func BitrateToKbps(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	s = strings.TrimSuffix(s, "k")
	s = strings.TrimSuffix(s, "bps")
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// NewProcessor 创建目录处理器，并确保所需目录存在。
func NewProcessor(opt Options) (*Processor, error) {
	if opt.HistoryDir == "" {
		opt.HistoryDir = filepath.Join(opt.WatchDir, HistoryDirName)
	}
	if opt.CheckInterval <= 0 {
		opt.CheckInterval = 30 * time.Second
	}
	if opt.StableDelay < 0 {
		opt.StableDelay = 0
	}
	if opt.OutputBitrate == "" {
		opt.OutputBitrate = DefaultOutputBitrate
	}
	if opt.Transcoder != nil && opt.Transcoder.bitrate != "" {
		opt.OutputBitrate = opt.Transcoder.bitrate
	}
	outputBitrateKbps := BitrateToKbps(opt.OutputBitrate)
	if outputBitrateKbps <= 0 {
		outputBitrateKbps = BitrateToKbps(DefaultOutputBitrate)
	}
	if err := os.MkdirAll(opt.WatchDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建监控目录 %s 失败: %w", opt.WatchDir, err)
	}
	if err := os.MkdirAll(opt.HistoryDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建归档目录 %s 失败: %w", opt.HistoryDir, err)
	}
	return &Processor{
		watchDir:          opt.WatchDir,
		historyDir:        opt.HistoryDir,
		checkInterval:     opt.CheckInterval,
		stableDelay:       opt.StableDelay,
		archiveAfter:      opt.ArchiveAfter,
		outputBitrateKbps: outputBitrateKbps,
		transcoder:        opt.Transcoder,
		log:               opt.Log,
		queue:             NewQueue(),
		done:              make(chan struct{}),
		failTimes:         make(map[string]time.Time),
		doneSet:           make(map[string]fileStamp),
		warnedUnsupported: make(map[string]bool),
	}, nil
}

// WatchDir 返回被监控目录。
func (p *Processor) WatchDir() string { return p.watchDir }

// HistoryDir 返回归档目录。
func (p *Processor) HistoryDir() string { return p.historyDir }

// Pending 返回待转码队列长度，供 Web 界面展示。
func (p *Processor) Pending() int { return p.queue.Len() }

// Start 启动扫描与转码工作协程。
func (p *Processor) Start(parent context.Context) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	scanCtx, cancelScan := context.WithCancel(parent)
	p.cancel = cancel
	p.cancelScan = cancelScan
	p.started = true
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()

	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		p.scanLoop(scanCtx)
	}()
	go func() {
		defer p.wg.Done()
		p.workerLoop(ctx)
	}()
	go func() {
		p.wg.Wait()
		close(done)
	}()

	p.log.Info("开始监控媒体目录，扫描间隔 %s", p.checkInterval)
}

// Stop 停止处理器并等待协程退出。
// 退出前会把队列中已发现的文件处理完，避免扫描到的任务被静默丢弃。
func (p *Processor) Stop() {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	cancel := p.cancel
	done := p.done
	p.started = false
	p.mu.Unlock()

	// 先停掉扫描协程，再排空队列，最后取消上下文。
	p.scanCancel()
	if n := p.queue.Len(); n > 0 {
		p.log.Info("正在处理队列中剩余的 %d 个文件...", n)
	}
	p.drainQueue()

	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		p.log.Warn("媒体处理器停止超时，可能存在仍在进行的转码任务")
	}
	p.log.Info("媒体处理已停止")
}

// drainQueue 串行处理队列中剩余的任务，使用独立的超时上下文。
func (p *Processor) drainQueue() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for {
		path, ok := p.queue.Pop()
		if !ok {
			return
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		p.claim(path)
		if err := p.transcoder.TranscodeFile(ctx, path); err != nil {
			p.log.Error("处理文件 %s 失败: %v", filepath.Base(path), err)
			p.releaseClaim(path)
			p.markFailed(path)
			continue
		}
		p.markProcessed(path)
	}
}

// scanCancel 取消扫描循环（若存在）。
func (p *Processor) scanCancel() {
	p.mu.Lock()
	c := p.cancelScan
	p.mu.Unlock()
	if c != nil {
		c()
	}
}

// scanLoop 周期性扫描目录并归档旧文件。
func (p *Processor) scanLoop(ctx context.Context) {
	p.scanOnce()
	ticker := time.NewTicker(p.checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.scanOnce()
		}
	}
}

// scanOnce 执行一轮扫描 + 归档，供内部与测试调用。
func (p *Processor) scanOnce() {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("目录扫描 panic: %v", r)
		}
	}()
	p.enqueueCandidates()
	p.archiveOldMP3()
}

// enqueueCandidates 遍历目录，把符合条件的文件加入转码队列。
func (p *Processor) enqueueCandidates() {
	if _, err := os.Stat(p.watchDir); err != nil {
		return
	}
	now := time.Now()

	walkErr := filepath.WalkDir(p.watchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			p.log.Debug("遍历 %s 出错: %v", path, err)
			return nil
		}
		if d.IsDir() {
			// 跳过归档目录，避免把历史文件再转一遍。
			if path != p.watchDir && filepath.Base(path) == HistoryDirName {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		name := d.Name()
		lower := strings.ToLower(name)
		ext := filepath.Ext(lower)

		if !hasMediaExt(lower) {
			// 曾经支持、现在处理不了的格式：告警一次，免得用户以为程序卡住了。
			// 用 Debug 之外的级别是有意的——这是需要用户采取行动的情况。
			if unsupportedMediaExtensions[ext] {
				p.warnUnsupported(path, ext)
			}
			return nil
		}
		// 中间产物由转码流程内部处理，不重复入队。
		if strings.HasSuffix(lower, "-i.mp3") {
			return nil
		}
		// 已是 MP3：仅当码率高于输出码率时才值得重转。
		// 低码率文件重转只会更低或持平，无收益，直接跳过。
		if strings.HasSuffix(lower, ".mp3") {
			info, err := ProbeMP3(path)
			if err != nil {
				p.log.Debug("解析 MP3 失败 %s: %v", name, err)
				return nil
			}
			if info.BitrateKbps <= p.outputBitrateKbps {
				return nil
			}
		}

		st, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if now.Sub(st.ModTime()) < p.stableDelay {
			return nil
		}
		if p.inRetryCooldown(path) {
			return nil
		}
		// 已经处理过且文件未被改动：跳过。这是防止转码产物被反复重编码的关键。
		if p.alreadyProcessed(path, st) {
			return nil
		}
		p.log.Info("发现待转换文件: %s（最后修改于 %s）",
			path, st.ModTime().Format("2006-01-02 15:04:05"))
		p.queue.Push(path)
		return nil
	})
	if walkErr != nil {
		p.log.Warn("扫描目录失败: %v", walkErr)
	}
}

func hasMediaExt(lower string) bool {
	ext := strings.ToLower(filepath.Ext(lower))
	return VideoExtensions[ext]
}

// warnUnsupported 对不受支持的媒体格式打一条告警。
//
// 按文件路径去重：扫描是周期性的，不去重会把日志刷爆。
// 用路径而非扩展名做键，是为了让每个文件各自提醒一次。
func (p *Processor) warnUnsupported(path, ext string) {
	p.warnMu.Lock()
	if p.warnedUnsupported[path] {
		p.warnMu.Unlock()
		return
	}
	p.warnedUnsupported[path] = true
	p.warnMu.Unlock()

	p.log.Warn("跳过不支持的格式 %s（%s）：纯 Go 转码只处理 MP3，请先将其转为 MP3",
		path, ext)
}

// workerLoop 串行消费转码队列，避免多个文件同时编码争抢 CPU。
func (p *Processor) workerLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			path, ok := p.queue.Pop()
			if !ok {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				p.log.Debug("文件已不存在，跳过: %s", path)
				continue
			}
			p.claim(path)
			if err := p.transcoder.TranscodeFile(ctx, path); err != nil {
				p.log.Error("处理文件 %s 失败: %v", filepath.Base(path), err)
				p.releaseClaim(path)
				p.markFailed(path)
				continue
			}
			p.markProcessed(path)
		}
	}
}

// inRetryCooldown 报告文件是否处于失败重试冷却期内。
func (p *Processor) inRetryCooldown(path string) bool {
	p.failMu.Lock()
	defer p.failMu.Unlock()
	last, ok := p.failTimes[path]
	if !ok {
		return false
	}
	return time.Since(last) < retryCooldown
}

// alreadyProcessed 报告该文件是否已被认领（正在处理中）或已处理完成且未被改动。
//
// 判定分两种情况：
//  1. 文件已被认领但尚未完成 —— 转码期间源文件会被删除，若此时重新扫描会再次入队，
//     导致同一文件被反复编码，因此进入转码前就先打上认领标记；
//  2. 文件已处理完成 —— 比对 (大小, 修改时间)，只有确实变化过的文件才重新处理。
func (p *Processor) alreadyProcessed(path string, st fs.FileInfo) bool {
	p.doneMu.Lock()
	defer p.doneMu.Unlock()
	rec, ok := p.doneSet[path]
	if !ok {
		return false
	}
	if rec.claimed && !rec.finished {
		// 正在处理中，避免重复入队。
		return true
	}
	return rec.size == st.Size() && rec.modTime.Equal(st.ModTime())
}

// claim 在开始转码前标记文件已被认领，防止处理期间被重复入队。
func (p *Processor) claim(path string) {
	p.doneMu.Lock()
	p.doneSet[path] = fileStamp{claimed: true}
	p.doneMu.Unlock()
}

// markProcessed 记录文件已成功处理，并清除其失败记录。
func (p *Processor) markProcessed(path string) {
	p.doneMu.Lock()
	if st, err := os.Stat(path); err == nil {
		p.doneSet[path] = fileStamp{
			claimed:  true,
			finished: true,
			size:     st.Size(),
			modTime:  st.ModTime(),
		}
	} else {
		delete(p.doneSet, path)
	}
	p.doneMu.Unlock()

	p.failMu.Lock()
	delete(p.failTimes, path)
	p.failMu.Unlock()
}

// releaseClaim 在转码失败时释放认领标记，交由失败冷却机制控制重试节奏。
func (p *Processor) releaseClaim(path string) {
	p.doneMu.Lock()
	if rec, ok := p.doneSet[path]; ok && rec.claimed && !rec.finished {
		delete(p.doneSet, path)
	}
	p.doneMu.Unlock()
}

// markFailed 记录转码失败时间，触发重试冷却。
func (p *Processor) markFailed(path string) {
	p.failMu.Lock()
	p.failTimes[path] = time.Now()
	p.failMu.Unlock()
}

// forgetProcessed 清理指定路径的处理记录（文件被移除时调用）。
func (p *Processor) forgetProcessed(path string) {
	p.doneMu.Lock()
	delete(p.doneSet, path)
	p.doneMu.Unlock()
}

// archiveOldMP3 把早于阈值时长的 MP3 移入归档目录。
func (p *Processor) archiveOldMP3() {
	if p.archiveAfter <= 0 {
		return
	}
	threshold := time.Now().Add(-p.archiveAfter)
	moved := 0

	walkErr := filepath.WalkDir(p.watchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != p.watchDir && filepath.Base(path) == HistoryDirName {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(strings.ToLower(d.Name()), ".mp3") {
			return nil
		}
		st, statErr := d.Info()
		if statErr != nil || st.ModTime().After(threshold) {
			return nil
		}
		dest := p.uniqueDest(filepath.Join(p.historyDir, d.Name()), st.ModTime())
		if mvErr := moveFile(path, dest); mvErr != nil {
			p.log.Warn("归档 %s 失败: %v", path, mvErr)
			return nil
		}
		p.log.Info("已归档过期文件: %s -> %s", path, filepath.Base(dest))
		moved++
		return nil
	})
	if walkErr != nil {
		p.log.Warn("归档扫描失败: %v", walkErr)
	}
	if moved > 0 {
		p.log.Info("本轮共归档 %d 个旧 MP3 文件", moved)
	}
}

// uniqueDest 在目标已存在时追加时间戳后缀。
func (p *Processor) uniqueDest(dest string, modTime time.Time) string {
	if _, err := os.Stat(dest); errors.Is(err, fs.ErrNotExist) {
		return dest
	}
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	return fmt.Sprintf("%s_%s%s", base, modTime.Format("20060102_150405"), ext)
}

// moveFile 跨分区移动文件：优先 rename，失败则复制后删除。
func moveFile(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		_ = os.Remove(dest)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dest)
		return err
	}
	return os.Remove(src)
}
