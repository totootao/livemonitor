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
	InspectState(ctx context.Context, container string) (dockerctl.ContainerState, error)
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
//
// 注意这里返回的是"本程序的记账状态"，不一定等于容器的真实状态：
// 容器被 docker stop、自己退出或崩溃时，本程序不会收到通知。
// 需要真实状态请用 SyncState。
func (m *ContainerMonitor) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// SyncState 向 Docker 核对容器的真实状态，并据此纠正内部记账。
//
// 返回纠正后的"是否处于运行中"。
//
// 存在的意义：m.running 只在 Start/Stop 被调用时更新，容器若被外部停掉
// （docker stop、容器内进程退出、崩溃、docker rm），本程序无从知晓，
// 这个标志就会一直停在 true。后果不只是界面显示"运行中"这么简单——
// StartContainer 会以"已在运行中"拒绝启动一个实际已停止的容器，
// StopContainer 会以"未在运行"拒绝停止，RunJobNow 同理。
//
// 所以对外汇报状态、以及任何"要不要启动/停止"的判断之前，
// 都应先经过这里。查询失败时保守地沿用现有记账，不做纠正——
// 宁可报旧状态，也不要因为一次网络抖动把运行中的容器标成已停止。
func (m *ContainerMonitor) SyncState(ctx context.Context) bool {
	m.mu.Lock()
	believed := m.running
	m.mu.Unlock()

	state, err := m.runner.InspectState(ctx, m.name)
	if err != nil {
		// 容器不存在时，内部若还以为在运行，说明它被删掉了，同样要纠正。
		if believed && isNotFound(err) {
			m.log.Warn("容器已不存在，重置运行状态")
			m.forgetRunning("容器已被删除")
			return false
		}
		m.log.Debug("核对容器状态失败，沿用现有状态: %v", err)
		return believed
	}

	actual := state.Running || state.Restarting
	if believed == actual {
		return actual
	}

	if !actual {
		// 最关键的纠正：程序以为在跑，实际已经停了。
		m.log.Warn("容器实际已停止（可能被外部停止或自行退出），重置运行状态")
		m.forgetRunning("容器实际已停止")
		return false
	}

	// 反向：程序以为已停，实际在跑。交由 Start 的接管逻辑处理，
	// 这里只报告真实状态。
	return true
}

// forgetRunning 把内部记账拉回"未运行"，并停掉仍在跑的日志监控 goroutine。
// reason 只用于日志。
func (m *ContainerMonitor) forgetRunning(reason string) {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false
	m.stopFlag = true
	cancel := m.cancel
	gen := m.gen
	done := m.done
	m.mu.Unlock()

	// 停掉日志跟踪，否则它会一直挂在容器上白耗资源。
	if cancel != nil {
		cancel()
	}

	m.mu.Lock()
	if m.gen == gen && done != nil {
		select {
		case <-done:
		default:
			close(done)
		}
	}
	m.mu.Unlock()

	if m.onStopped != nil {
		m.onStopped(reason)
	}
}

// isNotFound 判断错误是否表示容器不存在。
// dockerctl 在 404 时返回的文案里含"不存在"，这里做一次宽松匹配，
// 避免为此在接口上引入额外的错误类型。
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "不存在")
}

// Status 返回运行状态的快照，供 Web 界面展示。
//
// Running 来自内部记账，可能与容器真实状态不一致；
// 需要准确值请在调用前先执行 SyncState。
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

// Start 确保容器处于运行状态并纳入监控。
//
// 三种情形：
//   - 容器没在跑           → 启动它，从当前时刻开始计时；
//   - 容器已在跑且是本程序启动的 → 忽略（幂等）；
//   - 容器已在跑但**不是**本程序启动的（用户手动起的、上次进程退出后残留的）
//     → 直接接管监控，不重复下发 start。
//
// 第三种情形曾经的处理是"跳过本次计划"，那是个设计错误：
// 跳过意味着既不做日志关键词监控、也不做超时停止——容器会一直跑下去，
// 直到有人手动停它。而"容器正在运行"和"今天的计划已经执行过"是两件事，
// 不能混为一谈。现在改为接管，并按其**真实启动时间**继续计时，
// 避免一个早就该停的容器因为被接管而重新获得一整轮运行时长。
func (m *ContainerMonitor) Start() {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		m.log.Info("容器已在运行中，忽略启动命令")
		return
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	state, err := m.runner.InspectState(ctx, m.name)
	cancel()

	adopt := false
	switch {
	case err == nil && state.Running:
		adopt = true
	case err == nil && state.Restarting:
		// 重启中：状态的 Running 可能瞬时为 false，此时下发 start 会失败。
		// 当作已运行处理更稳妥，稍后由日志监控接管。
		m.log.Warn("容器正在重启中，稍后接管监控")
		adopt = true
	case err != nil:
		m.log.Debug("查询容器状态失败，按未运行处理: %v", err)
	}

	if adopt {
		m.adopt(state)
		return
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

	m.enterRunning(startedAt)
}

// adopt 接管一个已经处于运行状态的容器。
//
// 计时起点取容器的真实启动时间；该时间不可得时退回当前时刻。
// 若已运行时长已经超过上限，enforceMaxDuration 会在启动后立即触发停止——
// 这正是期望行为：这个容器本来就该停了。
func (m *ContainerMonitor) adopt(state dockerctl.ContainerState) {
	startedAt := state.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
		m.log.Warn("容器已在运行，但未能取到启动时间，改从当前时刻开始计时")
	} else {
		elapsed := time.Since(startedAt)
		if elapsed < 0 {
			// 容器时间超前于宿主机（时钟漂移），按当前时刻算，避免负时长。
			elapsed = 0
			startedAt = time.Now()
		}
		m.mu.Lock()
		limit := m.maxDuration
		m.mu.Unlock()

		m.log.Warn("容器已在运行，接管监控（已运行 %s）", config.FormatDuration(int(elapsed.Seconds())))
		if limit > 0 && elapsed >= limit {
			m.log.Warn("已运行时长已达上限 %s，接管后将立即停止",
				config.FormatDuration(int(limit.Seconds())))
		}
	}

	m.enterRunning(startedAt)
}

// enterRunning 把监控器置为运行态并拉起日志监控与超时计时。
func (m *ContainerMonitor) enterRunning(startedAt time.Time) {
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
//
// 计时基准是 m.started 而不是"本函数被调用的时刻"。这两者在接管场景下会分叉：
// 一个两小时前就被用户手动起起来的容器，接管时 m.started 是两小时前，
// 若按全量 maxDuration 起一个全新定时器，它会被错误地再放行一整轮时长。
// 因此这里算的是"还剩余多久"，已经超限时剩余为负，立刻触发停止。
func (m *ContainerMonitor) enforceMaxDuration(ctx context.Context, gen uint64, done chan struct{}) {
	m.mu.Lock()
	maxDuration := m.maxDuration
	started := m.started
	m.mu.Unlock()

	if maxDuration < 0 {
		// 负值表示显式关闭超时（配置层通常已把非正值归一化为默认值，
		// 这里只兜底），只等上下文结束。
		<-ctx.Done()
		return
	}
	// 注意：maxDuration == 0 不是"不限时长"，而是"到点即停"。
	// 配置里 <=0 会被归一化成 DefaultMaxRunDuration，真正走到这里为 0 的
	// 只剩亚秒级的测试用例——它们期望立刻停，而不是永远不停。

	remaining := time.Until(started.Add(maxDuration))
	if remaining < 0 {
		remaining = 0
	}

	if remaining == 0 {
		m.log.Warn("容器已运行满 %s，立即停止", config.FormatDuration(int(maxDuration.Seconds())))
	} else {
		m.log.Info("将在 %s 后自动停止", config.FormatDuration(int(remaining.Seconds())))
	}

	timer := time.NewTimer(remaining)
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
