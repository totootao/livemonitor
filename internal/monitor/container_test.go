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
	cleared   int
	// running 是未设置 inspectFn/inspectStateFn 时的默认容器状态。
	// 默认 false 表示容器不存在；之所以不选择"调 Start 就变 true"，
	// 是为了让 Start 内部的 InspectState 与随后的 Start 两个调用语义清晰分离。
	running   bool
	inspectFn func() (bool, error)
	// inspectStateFn 优先于 inspectFn，便于构造带启动时间的状态。
	inspectStateFn func() (dockerctl.ContainerState, error)
	streams        []*fakeStream
	startErr       error
	stopErr        error
	// clearLogsErr 让启动前的日志清理失败，验证不阻塞启动。
	clearLogsErr error
	// followErr 让 LogsFollow 直接失败，用于构造"实时监控不可用"的场景。
	followErr error
	// logsRangeFn 定制回查结果；nil 时返回空日志。
	// 参数 since 是监控器计算出的回查窗口起点，供断言窗口语义。
	logsRangeFn func(since time.Time) ([]string, error)

	logsRangeCalls  int
	logsRangeSinces []time.Time
	// ops 记录 docker 操作的发生顺序，供"清日志必须在 start 之前"这类时序断言。
	ops []string
}

func newFakeRunner() *fakeRunner { return &fakeRunner{} }

func (f *fakeRunner) InspectState(ctx context.Context, container string) (dockerctl.ContainerState, error) {
	if f.inspectStateFn != nil {
		return f.inspectStateFn()
	}
	if f.inspectFn != nil {
		running, err := f.inspectFn()
		return dockerctl.ContainerState{Running: running}, err
	}
	// 默认容器不存在（Running=false），等价于旧行为。
	return dockerctl.ContainerState{Running: f.running}, nil
}

func (f *fakeRunner) Start(ctx context.Context, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "start")
	if f.startErr != nil {
		return f.startErr
	}
	f.started++
	return nil
}

// ClearLogs 模拟启动前的日志清理，并记录调用以供时序断言。
func (f *fakeRunner) ClearLogs(ctx context.Context, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "clear")
	f.cleared++
	return f.clearLogsErr
}

// opOrder 返回记录到的操作顺序快照。
func (f *fakeRunner) opOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// clearCount 返回启动前清理被调用的次数。
func (f *fakeRunner) clearCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleared
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
	if f.followErr != nil {
		return nil, f.followErr
	}
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

func (f *fakeRunner) LogsRange(ctx context.Context, container string, since time.Time) ([]string, error) {
	f.mu.Lock()
	f.logsRangeCalls++
	f.logsRangeSinces = append(f.logsRangeSinces, since)
	fn := f.logsRangeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(since)
	}
	return nil, nil
}

// lastLogsRangeSince 返回最近一次回查的窗口起点。
// 回查跑在独立 goroutine 上，读取必须走 fake 的锁，
// 否则 -race 下测试自身与回查 goroutine 构成数据竞争。
func (f *fakeRunner) lastLogsRangeSince() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logsRangeSinces) == 0 {
		return time.Time{}
	}
	return f.logsRangeSinces[len(f.logsRangeSinces)-1]
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
	m := New(cc, nil, fn, logging.New("test"))
	// 把回溯检查的默认 15s 延迟压缩到毫秒级：
	// 一来用例不必真等 15 秒，二来避免每个用例都留下一个
	// 挂着 15s 定时器的 goroutine（测试进程退出前它们会集体醒来）。
	m.sweepDelay = 50 * time.Millisecond
	m.sweepTimeout = 500 * time.Millisecond
	return m
}

// TestStartClearsLogsBeforeStarting 启动前必须先清空历史日志，
// 且清空必须发生在 docker start **之前**——否则上一轮残留的关键词
// 可能混进本轮的回放/回查窗口。
func TestStartClearsLogsBeforeStarting(t *testing.T) {
	fn := newFakeRunner()
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	m.waitRunning(t)

	if got := fn.clearCount(); got != 1 {
		t.Errorf("启动前清理调用次数 = %d, 期望 1", got)
	}
	ops := fn.opOrder()
	if len(ops) != 2 || ops[0] != "clear" || ops[1] != "start" {
		t.Errorf("操作顺序应为 [clear start]，实际 %v", ops)
	}
	m.Stop(ReasonShutdown)
}

// TestAdoptDoesNotClearLogs 接管已在运行的容器时绝不能清日志——
// 那些日志正在被监控，清掉等于销毁正在分析的数据。
func TestAdoptDoesNotClearLogs(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{Running: true, StartedAt: time.Now().Add(-time.Minute)}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	m.waitRunning(t)

	if got := fn.clearCount(); got != 0 {
		t.Errorf("接管路径不应触发日志清理，实际调用 %d 次", got)
	}
	if _, stopped, _, _ := fn.counts(); stopped != 0 {
		t.Error("接管路径不应启动或停止容器")
	}
	m.Stop(ReasonShutdown)
}

// TestStartContinuesWhenClearFails 日志清不掉（未挂载宿主机日志目录等）
// 不能阻塞启动——监控语义仍以本轮启动时刻为界。
func TestStartContinuesWhenClearFails(t *testing.T) {
	fn := newFakeRunner()
	fn.clearLogsErr = errors.New("日志文件不可达")
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	m.waitRunning(t)

	started, _, _, _ := fn.counts()
	if started != 1 {
		t.Errorf("清理失败后仍应启动容器，start 次数 = %d", started)
	}
	if got := fn.clearCount(); got != 1 {
		t.Errorf("清理应被尝试调用 1 次，实际 %d", got)
	}
	m.Stop(ReasonShutdown)
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
	// 关键：让 InspectState 返回"没在运行"，Start 才会真的走启动分支。
	// 若返回 Running=true，Start 会走接管分支，把一个零值 StartedAt 当作
	// 刚刚启动，计时起点就变成了测试随机时间而非本次启动。
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{}, nil
	}
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

// TestRunningContainerAdopted 容器已在运行时应接管监控，而不是跳过。
//
// 历史行为是"跳过本次计划"，那是错的：跳过意味着既不做日志关键词监控、
// 也不做超时停止，容器会一直跑到有人手动停它。
// "容器正在运行"与"今天的计划已执行过"是两件事，不能混为一谈。
func TestRunningContainerAdopted(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{Running: true, StartedAt: time.Now().Add(-10 * time.Minute)}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	startedAt := make(chan time.Time, 1)
	m.SetHooks(func(at time.Time) { startedAt <- at }, nil)

	m.Start()
	time.Sleep(150 * time.Millisecond)

	// 不应重复下发 start。
	started, _, _, _ := fn.counts()
	if started != 0 {
		t.Errorf("接管已在运行的容器时不应再 start，实际 %d 次", started)
	}

	// 但必须进入运行态，从而挂上日志监控与超时计时。
	if !m.IsRunning() {
		t.Fatal("接管后应处于运行中状态（否则不会做关键词监控与超时停止）")
	}

	// 计时起点必须是容器的真实启动时间，而不是接管时刻。
	select {
	case at := <-startedAt:
		if time.Since(at) < 9*time.Minute {
			t.Errorf("计时起点应取容器真实启动时间（约 10 分钟前），实际距今仅 %s", time.Since(at))
		}
	default:
		t.Error("接管应触发 onStarted 回调")
	}
}

// TestAdoptContainerPastLimit 已运行超限的容器被接管后应立即停止。
func TestAdoptContainerPastLimit(t *testing.T) {
	fn := newFakeRunner()
	// 容器已经跑了 2 小时，而上限是 1 小时。
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{Running: true, StartedAt: time.Now().Add(-2 * time.Hour)}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	time.Sleep(250 * time.Millisecond)

	_, stopped, _, _ := fn.counts()
	if stopped == 0 {
		t.Error("已运行超过上限的容器被接管后应立即停止")
	}
	if m.IsRunning() {
		t.Error("停止后不应仍处于运行态")
	}
}

// TestAdoptWithoutStartedAt 取不到启动时间时退回当前时刻，不应崩溃。
func TestAdoptWithoutStartedAt(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		// StartedAt 为零值，模拟 Docker 未返回该字段。
		return dockerctl.ContainerState{Running: true}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	time.Sleep(150 * time.Millisecond)

	if !m.IsRunning() {
		t.Fatal("取不到启动时间时仍应接管并进入运行态")
	}
	_, stopped, _, _ := fn.counts()
	if stopped != 0 {
		t.Error("计时从当前时刻起算，不应立刻停止")
	}
}

// TestRunningContainerKeywordStillWorks 接管后的容器同样受关键词监控。
//
// 这是修复的核心价值：接管不只是"记一笔"，而是真正挂上日志监控。
func TestRunningContainerKeywordStillWorks(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{Running: true, StartedAt: time.Now()}, nil
	}

	stoppedCh := make(chan string, 1)
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)
	m.SetHooks(nil, func(reason string) { stoppedCh <- reason })

	m.Start()
	m.waitRunning(t)

	// 接管路径同样应建立日志流。
	stream := fn.lastStream(t)
	stream.Push("[INFO] 主播正在准备，等待直播中")

	select {
	case reason := <-stoppedCh:
		if !strings.Contains(reason, "等待直播") {
			t.Errorf("停止原因应包含关键词，实际: %s", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("接管后的容器应受关键词监控，但超时未停止")
	}

	_, stopped, _, _ := fn.counts()
	if stopped != 1 {
		t.Errorf("stop 调用次数 = %d, 期望 1", stopped)
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
