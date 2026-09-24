package manager

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/dockerctl"
	"github.com/totootao/livemonitor/internal/logging"
	"github.com/totootao/livemonitor/internal/monitor"
)

// deadStream 是立即结束的空日志流：监控循环建立后无事可做即退出，
// 让接管路径的测试不需要维护一条活流。
type deadStream struct{ lines chan string }

func newDeadStream() *deadStream {
	ch := make(chan string)
	close(ch)
	return &deadStream{lines: ch}
}

func (d *deadStream) Lines() <-chan string { return d.lines }
func (d *deadStream) Wait() error          { return nil }
func (d *deadStream) Kill() error          { return nil }

// fakeRunner 实现 monitor.Runner，记录调用并模拟容器状态。
type fakeRunner struct {
	mu       sync.Mutex
	running  bool
	starts   int
	inspects int
}

func (f *fakeRunner) InspectState(ctx context.Context, container string) (dockerctl.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	return dockerctl.ContainerState{Running: f.running}, nil
}

func (f *fakeRunner) Start(ctx context.Context, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.running = true
	return nil
}

func (f *fakeRunner) Stop(ctx context.Context, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
	return nil
}

func (f *fakeRunner) TruncateInternalLogs(ctx context.Context, container string) error {
	return nil
}

func (f *fakeRunner) RotateLogs(ctx context.Context, container string) error { return nil }

func (f *fakeRunner) ClearLogs(ctx context.Context, container string) error { return nil }

func (f *fakeRunner) LogsFollow(ctx context.Context, container string, since *time.Time) (dockerctl.StreamHandle, error) {
	return newDeadStream(), nil
}

func (f *fakeRunner) LogsRange(ctx context.Context, container string, since time.Time) ([]string, error) {
	return nil, nil
}

func (f *fakeRunner) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func (f *fakeRunner) inspectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspects
}

// newTestManager 构建只含容器监控器的极简 Manager：
// adoptExternallyStarted 只依赖 monitors 与日志，其余字段保持零值即可。
// 返回与每个监控器配对的 fakeRunner，便于按用例设置容器状态。
func newTestManager(names []string) (*Manager, map[string]*fakeRunner) {
	m := &Manager{
		log:      logging.New("error"),
		monitors: make(map[string]*monitor.ContainerMonitor),
	}
	runners := make(map[string]*fakeRunner, len(names))
	for _, name := range names {
		r := &fakeRunner{}
		// MaxRunDuration 必须给足：0 在语义上是"到点即停"，
		// 会让接管后的超时保护立刻把容器停掉，断言永远等不到运行态。
		cc := config.ContainerConfig{Name: name, MaxRunDuration: 3600}
		m.monitors[name] = monitor.New(cc, nil, r, logging.New(name))
		runners[name] = r
	}
	return m, runners
}

// waitFor 轮询等待条件成立，超时返回 false。
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// 不在配置里的容器事件应被直接忽略，不产生任何接管动作。
func TestAdoptExternallyStartedUnknownContainer(t *testing.T) {
	m, _ := newTestManager([]string{"alpha"})
	// 无名可查，行为只有"静默返回"——能安全返回即通过。
	m.adoptExternallyStarted("ghost")
}

// 已被本程序跟踪的容器（大概率是自己的启动事件）应跳过，
// 且不能再向 Engine 发状态查询。
func TestAdoptExternallyStartedAlreadyTracked(t *testing.T) {
	m, runners := newTestManager([]string{"alpha"})
	r := runners["alpha"]
	r.running = true

	mon, _ := m.MonitorByName("alpha")
	mon.Start() // 容器已在跑 → 走接管分支，内部记账进入运行态
	if !mon.IsRunning() {
		t.Fatal("测试前置失败：容器应处于已跟踪状态")
	}
	mon.Stop(monitor.ReasonShutdown)

	// 让记账重新回到"运行中"再测（Stop 会把 fakeRunner 置为未运行并重置记账）。
	r.running = true
	mon.Start()
	base := r.inspectCount()

	m.adoptExternallyStarted("alpha")
	if n := r.inspectCount(); n != base {
		t.Errorf("已跟踪的容器不应再做状态查询：调用前 %d 次，调用后 %d 次", base, n)
	}
	mon.Stop(monitor.ReasonShutdown)
}

// 外部启动的配置容器：真实在跑 → 接管监控；接管走 adopt 分支，不下发 docker start。
func TestAdoptExternallyStartedRunningContainer(t *testing.T) {
	m, runners := newTestManager([]string{"alpha"})
	r := runners["alpha"]
	r.running = true // Docker 说容器在跑

	mon, _ := m.MonitorByName("alpha")
	m.adoptExternallyStarted("alpha")
	if !waitFor(2*time.Second, mon.IsRunning) {
		t.Fatal("外部启动的在跑容器应被接管监控")
	}
	if n := r.startCount(); n != 0 {
		t.Errorf("接管不应重新启动容器，实际下发 start %d 次", n)
	}
	mon.Stop(monitor.ReasonShutdown) // 清理日志监控 goroutine
}

// 事件迟到或容器瞬时退出：Engine 说没在跑 → 不接管。
func TestAdoptExternallyStartedButNotRunning(t *testing.T) {
	m, runners := newTestManager([]string{"alpha"})
	r := runners["alpha"]
	r.running = false // 事件说 start 了，但真实状态已经退出

	mon, _ := m.MonitorByName("alpha")
	m.adoptExternallyStarted("alpha")
	time.Sleep(100 * time.Millisecond)
	if mon.IsRunning() {
		t.Error("真实状态未运行的容器不应被接管")
	}
	if n := r.startCount(); n != 0 {
		t.Errorf("不应重新启动容器，实际下发 start %d 次", n)
	}
}

// 事件流端到端：watchContainerEvents 收到 start 事件后应接管配置容器，
// 忽略无关容器与无关动作；ctx 取消后循环退出。
func TestWatchContainerEventsAdoptsConfiguredContainer(t *testing.T) {
	const (
		known   = "alpha"
		unknown = "beta"
	)
	// 一条流里混入：配置容器的 start（应接管）、无关容器的 start（忽略）、
	// 配置容器的 die（忽略）。
	lines := []string{
		fmt.Sprintf(`{"Type":"container","Action":"start","Actor":{"Attributes":{"name":%q}}}`, known),
		fmt.Sprintf(`{"Type":"container","Action":"start","Actor":{"Attributes":{"name":%q}}}`, unknown),
		fmt.Sprintf(`{"Type":"container","Action":"die","Actor":{"Attributes":{"name":%q}}}`, known),
	}

	// 模拟 Docker Engine 的 /events 长连接。
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("监听 unix socket 失败: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	m, runners := newTestManager([]string{known})
	r := runners[known]
	r.running = true
	mon, _ := m.MonitorByName(known)
	m.docker = dockerctl.NewWithSocket(sock, logging.New("error"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.watchContainerEvents(ctx)
	}()

	if !waitFor(2*time.Second, mon.IsRunning) {
		t.Fatal("事件流中的配置容器应被接管监控")
	}
	mon.Stop(monitor.ReasonShutdown)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("ctx 取消后 watchContainerEvents 应退出")
	}
}
