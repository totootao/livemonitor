// Package monitor 实现容器运行时监控：定时启动、日志关键词触发停止、超时停止。
package monitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/dockerctl"
	"github.com/totootao/livemonitor/internal/logging"
)

// Runner 抽象 docker 操作，便于测试替换。
type Runner interface {
	InspectRunning(ctx context.Context, container string) (bool, error)
	Start(ctx context.Context, container string) error
	Stop(ctx context.Context, container string) error
	TruncateInternalLogs(ctx context.Context, container string) error
	RotateLogs(ctx context.Context, container string) error
	LogsFollow(ctx context.Context, container string, since *time.Time) (dockerctl.StreamHandle, error)
}

// StopReason 描述容器停止的原因。
type StopReason string

// 停止原因枚举。
const (
	ReasonKeyword   StopReason = "命中日志关键词"
	ReasonTimeout   StopReason = "超过最大运行时长"
	ReasonShutdown  StopReason = "程序退出"
	ReasonKickstart StopReason = "被新的启动计划重新拉起"
)

// ContainerMonitor 管理单个容器的生命周期。
type ContainerMonitor struct {
	name        string
	keywords    []string
	maxDuration time.Duration

	runner Runner
	log    *logging.Logger

	mu       sync.Mutex
	running  bool
	started  time.Time
	stopFlag bool
	// cancel 用于终止该容器的日志监控 goroutine。
	cancel context.CancelFunc
	// done 在完全停止后关闭。
	done chan struct{}
	// gen 是启动代次，避免旧的 goroutine 干扰新一轮启动。
	gen uint64

	// hooks 便于测试观察状态变化。
	onStarted func(startedAt time.Time)
	onStopped func(reason string)
}

// New 创建单容器监控器。
func New(cc config.ContainerConfig, globalKeywords []string, c Runner, log *logging.Logger) *ContainerMonitor {
	kw := []string(cc.Keywords)
	if len(kw) == 0 {
		kw = append(kw, globalKeywords...)
	}
	return &ContainerMonitor{
		name:        cc.Name,
		keywords:    kw,
		maxDuration: time.Duration(cc.MaxRunDuration) * time.Second,
		runner:      c,
		log:         log,
		done:        make(chan struct{}),
	}
}

// Name 返回容器名。
func (m *ContainerMonitor) Name() string { return m.name }

// IsRunning 返回当前是否处于已启动状态。
func (m *ContainerMonitor) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Status 返回运行状态的快照，供 Web 界面展示。
func (m *ContainerMonitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		Name:        m.name,
		Running:     m.running,
		Keywords:    append([]string(nil), m.keywords...),
		MaxDuration: int(m.maxDuration.Seconds()),
	}
	if m.running && !m.started.IsZero() {
		st.StartedAt = m.started
		st.ElapsedSeconds = int(time.Since(m.started).Seconds())
		st.RemainingSeconds = int(m.maxDuration.Seconds()) - st.ElapsedSeconds
		if st.RemainingSeconds < 0 {
			st.RemainingSeconds = 0
		}
	}
	return st
}

// Status 是容器状态的只读快照。
type Status struct {
	Name             string    `json:"name"`
	Running          bool      `json:"running"`
	StartedAt        time.Time `json:"startedAt"`
	ElapsedSeconds   int       `json:"elapsedSeconds"`
	RemainingSeconds int       `json:"remainingSeconds"`
	MaxDuration      int       `json:"maxDuration"`
	Keywords         []string  `json:"keywords"`
}

// Update 热更新该容器的运行参数。已启动的容器仅在下次启动时生效于时长计时。
func (m *ContainerMonitor) Update(cc config.ContainerConfig, globalKeywords []string) {
	kw := []string(cc.Keywords)
	if len(kw) == 0 {
		kw = append(kw, globalKeywords...)
	}
	m.mu.Lock()
	m.keywords = kw
	m.maxDuration = time.Duration(cc.MaxRunDuration) * time.Second
	m.mu.Unlock()
	m.log.Info("已更新容器参数：关键词 %s，最长运行 %s",
		strings.Join(kw, ", "), config.FormatDuration(cc.MaxRunDuration))
}

// Start 启动容器；若已在运行则忽略（保留原脚本的幂等语义）。
func (m *ContainerMonitor) Start() {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		m.log.Info("容器已在运行中，忽略启动命令")
		return
	}
	m.mu.Unlock()

	// 启动前先看容器真实状态，避免对已运行容器重复 start 造成计时错乱。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	running, err := m.runner.InspectRunning(ctx, m.name)
	cancel()
	if err == nil && running {
		m.log.Warn("容器已在运行，本次计划跳过（不影响已有监控）")
		return
	}
	if err != nil {
		m.log.Debug("查询容器状态失败，按未运行处理: %v", err)
	}

	startedAt := time.Now()
	m.log.Info("启动容器...")
	m.log.Info("启动时间: %s", startedAt.Format("2006-01-02 15:04:05"))

	startCtx, startCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer startCancel()
	if err := m.runner.Start(startCtx, m.name); err != nil {
		m.log.Error("启动失败: %v", err)
		return
	}
	m.log.Info("启动成功")

	runCtx, runCancel := context.WithCancel(context.Background())

	m.mu.Lock()
	m.running = true
	m.started = startedAt
	m.stopFlag = false
	m.cancel = runCancel
	m.gen++
	gen := m.gen
	m.done = make(chan struct{})
	done := m.done
	m.mu.Unlock()

	if m.onStarted != nil {
		m.onStarted(startedAt)
	}

	go m.watchLogs(runCtx, gen, startedAt)
	go m.enforceMaxDuration(runCtx, gen, done)
}

// Stop 停止容器。reason 用于日志与控制台展示。
func (m *ContainerMonitor) Stop(reason StopReason) {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false
	m.stopFlag = true
	cancel := m.cancel
	gen := m.gen
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	m.log.Info("因 %s 停止容器...", reason)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 120*time.Second)
	err := m.runner.Stop(ctx, m.name)
	cancelCtx()
	if err != nil {
		m.log.Error("停止失败: %v", err)
		// 回滚状态，让超时线程可以重试。
		m.mu.Lock()
		if m.gen == gen {
			m.running = true
			m.stopFlag = false
		}
		m.mu.Unlock()
		return
	}

	m.log.Info("已停止")
	m.cleanupLogs()

	m.mu.Lock()
	if m.gen == gen {
		select {
		case <-m.done:
		default:
			close(m.done)
		}
	}
	m.mu.Unlock()

	if m.onStopped != nil {
		m.onStopped(string(reason))
	}
}

// cleanupLogs 停止后尝试清理容器日志。任何一种方式成功即可。
func (m *ContainerMonitor) cleanupLogs() {
	m.log.Info("开始清理日志...")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := m.runner.TruncateInternalLogs(ctx, m.name); err == nil {
		m.log.Info("已通过容器内 truncate 清空日志")
		return
	} else {
		m.log.Debug("容器内 truncate 未生效: %v", err)
	}

	if err := m.runner.RotateLogs(ctx, m.name); err == nil {
		m.log.Info("已通过 docker logs 回收日志句柄")
		return
	}
	m.log.Warn("所有日志清理方式均未完全生效，可配置 docker log-opts 限制日志体积")
}

// watchLogs 跟踪容器日志，发现关键词即停止容器。
func (m *ContainerMonitor) watchLogs(ctx context.Context, gen uint64, since time.Time) {
	m.log.Info("开始监控新日志，监控关键词: %s", strings.Join(m.keywords, ", "))

	handle, err := m.runner.LogsFollow(ctx, m.name, &since)
	if err != nil {
		m.log.Error("日志监控启动失败: %v", err)
		return
	}
	defer func() {
		if kerr := handle.Kill(); kerr != nil {
			m.log.Debug("结束日志跟踪进程: %v", kerr)
		}
		_ = handle.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-handle.Lines():
			if !ok {
				m.log.Debug("日志流已结束")
				return
			}
			if m.matchAndStop(line, gen) {
				return
			}
		}
	}
}

// matchAndStop 检查单行日志是否命中关键词，命中则停止容器并返回 true。
func (m *ContainerMonitor) matchAndStop(line string, gen uint64) bool {
	cleaned := CleanLogLine(line)

	m.mu.Lock()
	// 若期间已被重新拉起（代次变化）或已停止，则忽略。
	if m.gen != gen || !m.running {
		m.mu.Unlock()
		return true
	}
	m.mu.Unlock()
	// 在同一把锁内取出关键词快照，避免与 Update 并发读写。
	keywords := append([]string(nil), m.keywords...)

	for _, kw := range keywords {
		if kw != "" && strings.Contains(cleaned, kw) {
			m.log.Info("检测到关键词: %s", kw)
			m.Stop(StopReason("命中日志关键词 '" + kw + "'"))
			return true
		}
	}
	return false
}

// enforceMaxDuration 到达最大运行时长后停止容器。
func (m *ContainerMonitor) enforceMaxDuration(ctx context.Context, gen uint64, done chan struct{}) {
	m.mu.Lock()
	maxDuration := m.maxDuration
	m.mu.Unlock()

	m.log.Info("将在 %s 后自动停止", config.FormatDuration(int(maxDuration.Seconds())))
	timer := time.NewTimer(maxDuration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-done:
		return
	case <-timer.C:
		m.mu.Lock()
		stale := m.gen != gen || !m.running
		m.mu.Unlock()
		if stale {
			return
		}
		m.Stop(StopReason("达到最大运行时长 " + config.FormatDuration(int(maxDuration.Seconds()))))
	}
}

// SetHooks 注册状态回调，仅供测试使用。
func (m *ContainerMonitor) SetHooks(onStarted func(time.Time), onStopped func(string)) {
	m.onStarted = onStarted
	m.onStopped = onStopped
}

// CleanLogLine 移除日志行中的控制字符，与原脚本的正则 [\x00-\x1F\x7F] 等价，
// 但保留制表符以便阅读，其余不可打印字符一律剔除。
func CleanLogLine(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	for _, r := range line {
		switch {
		case r == '\t':
			b.WriteRune(r)
		case r < 0x20, r == 0x7F:
			// 丢弃控制字符
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ErrAlreadyRunning 在需要严格启动语义时返回。
var ErrAlreadyRunning = errors.New("容器已在运行")
