package monitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/dockerctl"
	"github.com/totootao/livemonitor/internal/logging"
)

// fakeStream 是可控的日志流，用于驱动关键词匹配逻辑。
type fakeStream struct {
	lines chan string
	done  chan struct{}
	once  sync.Once
}

func newFakeStream() *fakeStream {
	return &fakeStream{lines: make(chan string, 64), done: make(chan struct{})}
}

func (f *fakeStream) Lines() <-chan string { return f.lines }
func (f *fakeStream) Push(s string)        { f.lines <- s }
func (f *fakeStream) Close()               { f.once.Do(func() { close(f.lines) }) }
func (f *fakeStream) Wait() error          { <-f.done; return nil }
func (f *fakeStream) Kill() error          { f.Close(); return nil }

// fakeRunner 记录 docker 调用，模拟容器状态。
type fakeRunner struct {
	mu        sync.Mutex
	started   int
	stopped   int
	truncated int
	rotated   int
	inspectFn func() (bool, error)
	streams   []*fakeStream
	startErr  error
	stopErr   error
}

func newFakeRunner() *fakeRunner { return &fakeRunner{} }

func (f *fakeRunner) InspectRunning(ctx context.Context, container string) (bool, error) {
	if f.inspectFn != nil {
		return f.inspectFn()
	}
	return false, nil
}

func (f *fakeRunner) Start(ctx context.Context, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started++
	return nil
}

func (f *fakeRunner) Stop(ctx context.Context, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped++
	return nil
}

func (f *fakeRunner) TruncateInternalLogs(ctx context.Context, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.truncated++
	return nil
}

func (f *fakeRunner) RotateLogs(ctx context.Context, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotated++
	return nil
}

func (f *fakeRunner) LogsFollow(ctx context.Context, container string, since *time.Time) (dockerctl.StreamHandle, error) {
	s := newFakeStream()
	f.mu.Lock()
	f.streams = append(f.streams, s)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		go func() { close(s.done) }()
		s.Close()
	}()
	return s, nil
}

func (f *fakeRunner) counts() (int, int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.stopped, f.truncated, f.rotated
}

func (f *fakeRunner) lastStream(t *testing.T) *fakeStream {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.streams)
		var s *fakeStream
		if n > 0 {
			s = f.streams[n-1]
		}
		f.mu.Unlock()
		if s != nil {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等不到日志流创建")
	return nil
}

func newTestMonitor(t *testing.T, fn *fakeRunner, keywords []string, maxDur time.Duration) *ContainerMonitor {
	t.Helper()
	hours := maxDur / time.Hour
	_ = hours
	cc := config.ContainerConfig{
		Name:           "testcontainer",
		StartTimes:     config.StringList{"23:59"},
		MaxRunDuration: int(maxDur.Seconds()),
		Keywords:       config.StringList(keywords),
	}
	return New(cc, nil, fn, logging.New("test"))
}

// TestStartIdempotent 重复 Start 只应实际启动一次。
func TestStartIdempotent(t *testing.T) {
	fn := newFakeRunner()
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	m.waitRunning(t)
	m.Start() // 应被忽略
	time.Sleep(50 * time.Millisecond)

	started, _, _, _ := fn.counts()
	if started != 1 {
		t.Errorf("start 调用次数 = %d, 期望 1", started)
	}
	if !m.IsRunning() {
		t.Error("容器应处于运行中")
	}
	m.Stop(ReasonShutdown)
}

// TestKeywordStopsContainer 命中关键词应停止容器并清理日志。
func TestKeywordStopsContainer(t *testing.T) {
	fn := newFakeRunner()
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	stoppedCh := make(chan string, 1)
	m.SetHooks(nil, func(reason string) { stoppedCh <- reason })

	m.Start()
	m.waitRunning(t)
	stream := fn.lastStream(t)

	// 构造带控制字符的日志行，验证清洗逻辑不影响匹配。
	stream.Push("[INFO] \x07主播正在准备，等待直播中\x1b[0m")

	select {
	case reason := <-stoppedCh:
		if !strings.Contains(reason, "等待直播") {
			t.Errorf("停止原因应包含关键词，实际: %s", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时未停止容器")
	}

	_, stopped, truncated, _ := fn.counts()
	if stopped != 1 {
		t.Errorf("stop 调用次数 = %d, 期望 1", stopped)
	}
	if truncated != 1 {
		t.Errorf("日志清理调用次数 = %d, 期望 1", truncated)
	}
	if m.IsRunning() {
		t.Error("容器应已停止")
	}
}

// TestNonMatchingKeywordKeepsRunning 未命中关键词时容器应继续运行。
func TestNonMatchingKeywordKeepsRunning(t *testing.T) {
	fn := newFakeRunner()
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	m.waitRunning(t)
	stream := fn.lastStream(t)
	stream.Push("正在推流，一切正常")
	stream.Push("[INFO] 观众 +1")

	time.Sleep(200 * time.Millisecond)
	if !m.IsRunning() {
		t.Error("未命中关键词时容器不应停止")
	}
	_, stopped, _, _ := fn.counts()
	if stopped != 0 {
		t.Errorf("stop 调用次数 = %d, 期望 0", stopped)
	}
	m.Stop(ReasonShutdown)
}

// TestMaxDurationStopsContainer 到达最大运行时长应停止容器。
func TestMaxDurationStopsContainer(t *testing.T) {
	fn := newFakeRunner()
	m := newTestMonitor(t, fn, []string{"等待直播"}, 150*time.Millisecond)

	stoppedCh := make(chan string, 1)
	m.SetHooks(nil, func(reason string) { stoppedCh <- reason })

	m.Start()
	m.waitRunning(t)

	select {
	case reason := <-stoppedCh:
		if !strings.Contains(reason, "最大运行时长") {
			t.Errorf("停止原因应为超时，实际: %s", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时未因最大运行时长停止")
	}
	_, stopped, _, _ := fn.counts()
	if stopped != 1 {
		t.Errorf("stop 调用次数 = %d, 期望 1", stopped)
	}
}

// TestStopNotRunningIsNoop 未运行时调用 Stop 不应触发 docker stop。
func TestStopNotRunningIsNoop(t *testing.T) {
	fn := newFakeRunner()
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Stop(ReasonShutdown)
	_, stopped, _, _ := fn.counts()
	if stopped != 0 {
		t.Errorf("未运行容器不应调用 docker stop，实际 %d 次", stopped)
	}
}

// TestStartFailureDoesNotMarkRunning docker start 失败时不应标记为运行中。
func TestStartFailureDoesNotMarkRunning(t *testing.T) {
	fn := newFakeRunner()
	fn.startErr = errors.New("simulated failure")
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	time.Sleep(100 * time.Millisecond)

	if m.IsRunning() {
		t.Error("启动失败时不应标记为运行中")
	}
}

// TestStopFailureRollsBack 停止失败时应回滚状态，允许超时线程重试。
func TestStopFailureRollsBack(t *testing.T) {
	fn := newFakeRunner()
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	m.waitRunning(t)
	fn.mu.Lock()
	fn.stopErr = errors.New("simulated stop failure")
	fn.mu.Unlock()

	m.Stop(ReasonShutdown)
	if !m.IsRunning() {
		t.Error("停止失败后状态应回滚为运行中")
	}

	fn.mu.Lock()
	fn.stopErr = nil
	fn.mu.Unlock()
	m.Stop(ReasonShutdown)
	if m.IsRunning() {
		t.Error("重试成功后容器应已停止")
	}
}

// TestInspectRunningSkipsStart 容器已在运行时应跳过本次启动计划。
func TestInspectRunningSkipsStart(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectFn = func() (bool, error) { return true, nil }
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	time.Sleep(100 * time.Millisecond)

	started, _, _, _ := fn.counts()
	if started != 0 {
		t.Errorf("容器已在运行时不应再次 start，实际 %d 次", started)
	}
	if m.IsRunning() {
		t.Error("跳过启动时不应标记为运行中")
	}
}

// TestCleanLogLine 验证控制字符清洗。
func TestCleanLogLine(t *testing.T) {
	in := "a\x00b\x07c\x1b[31md\x7fe"
	got := CleanLogLine(in)
	want := "abc[31mde"
	if got != want {
		t.Errorf("CleanLogLine = %q, 期望 %q", got, want)
	}
}

// waitRunning 等待监控器进入运行态。
func (m *ContainerMonitor) waitRunning(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.IsRunning() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("容器未进入运行态")
}
