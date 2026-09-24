// Package manager 编排容器监控、定时调度、媒体处理与 Web 管理界面。
package manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/dockerctl"
	"github.com/totootao/livemonitor/internal/logging"
	"github.com/totootao/livemonitor/internal/media"
	"github.com/totootao/livemonitor/internal/monitor"
	"github.com/totootao/livemonitor/internal/runtime"
	"github.com/totootao/livemonitor/internal/scheduler"
	"github.com/totootao/livemonitor/internal/web"
)

// Manager 是综合管理器。
type Manager struct {
	log    *logging.Logger
	docker *dockerctl.Client
	sched  *scheduler.Scheduler
	video  *media.Processor
	store  *runtime.Store

	mu       sync.RWMutex
	monitors map[string]*monitor.ContainerMonitor
	order    []string // 容器名的稳定展示顺序

	startedAt time.Time
	webSrv    *web.Server
	srv       *http.Server
}

// New 依据配置构建管理器。
func New(cfg *config.Config, cfgPath string, log *logging.Logger) (*Manager, error) {
	dockerClient := dockerctl.New(log)
	store := runtime.NewStore(cfg, cfgPath, log)

	video, err := media.NewProcessor(media.Options{
		WatchDir:      cfg.WatchDir,
		HistoryDir:    cfg.HistoryDir,
		CheckInterval: time.Duration(cfg.CheckInterval) * time.Second,
		StableDelay:   time.Duration(cfg.StableDelay) * time.Second,
		ArchiveAfter:  time.Duration(cfg.ArchiveAfterHours) * time.Hour,
		Transcoder:    media.NewTranscoder("", cfg.MP3Bitrate, log),
		Log:           log,
	})
	if err != nil {
		return nil, err
	}

	m := &Manager{
		log:      log,
		docker:   dockerClient,
		sched:    scheduler.New(log),
		video:    video,
		store:    store,
		monitors: make(map[string]*monitor.ContainerMonitor),
	}

	// 把"今天已触发过哪些任务"落盘到配置目录旁。
	//
	// 不持久化的话，进程重启或任何配置热更新都会让当天已过点的任务重放一遍，
	// 表现为同一批容器在几分钟内被反复启动。
	// 路径跟着配置文件走，不额外引入新的挂载点。
	m.sched.SetStatePath(schedulerStatePath(cfgPath))

	// 按配置初始化全部容器与定时任务。
	for _, cc := range cfg.Containers {
		m.installContainer(cc, cfg.MonitorKeywords)
	}

	return m, nil
}

// schedulerStatePath 由配置文件路径推导调度状态文件的位置。
// 配置文件形如 /config/config.json，状态文件落在 /config/scheduler-state.json。
func schedulerStatePath(cfgPath string) string {
	dir := filepath.Dir(cfgPath)
	if dir == "" || dir == "." {
		return "scheduler-state.json"
	}
	return filepath.Join(dir, "scheduler-state.json")
}

// installContainer 创建（或复用）容器监控器并注册其定时任务。调用方不应持有 m.mu。
func (m *Manager) installContainer(cc config.ContainerConfig, globalKeywords []string) {
	m.mu.Lock()
	mon, exists := m.monitors[cc.Name]
	if !exists {
		mon = monitor.New(cc, globalKeywords, m.docker, logging.New(cc.Name))
		m.monitors[cc.Name] = mon
		m.order = append(m.order, cc.Name)
		sort.Strings(m.order)
	} else {
		mon.Update(cc, globalKeywords)
	}
	m.mu.Unlock()

	// 先清掉该容器的旧任务，再按新时刻重建，避免热更新后残留旧时间点。
	m.sched.RemoveByPrefix(cc.Name + "@")
	for _, ts := range cc.StartTimes {
		at, err := config.ParseClock(ts)
		if err != nil {
			m.log.Error("容器 %s 的时间 %q 非法，已跳过: %v", cc.Name, ts, err)
			continue
		}
		m.sched.Add(scheduler.Job{
			ID:   cc.Name + "@" + at.Format("15:04"),
			Name: cc.Name,
			At:   at,
			Fn:   func() { mon.Start() },
		})
		m.log.Info("已为容器 %s 配置定时启动: %s（每天）", cc.Name, at.Format("15:04"))
	}
}

// removeContainer 摘除容器监控器与其全部定时任务。
func (m *Manager) removeContainer(name string) {
	m.mu.Lock()
	mon, ok := m.monitors[name]
	if ok {
		delete(m.monitors, name)
		for i, n := range m.order {
			if n == name {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
	}
	m.mu.Unlock()

	if !ok {
		return
	}
	// 先停掉正在运行的容器，避免留下无人接管的运行实例。
	if mon.IsRunning() {
		m.log.Info("容器 %s 被移除，正在停止其运行实例", name)
		mon.Stop(monitor.ReasonShutdown)
	}
	m.sched.RemoveByPrefix(name + "@")
}

// Reload 用存储中的最新配置重建全部容器与定时任务。
// 用于「全局关键词」等影响所有容器的设置变更。
func (m *Manager) Reload() {
	snap := m.store.Snapshot()
	desired := make(map[string]config.ContainerConfig, len(snap.Containers))
	for _, cc := range snap.Containers {
		desired[cc.Name] = cc
	}

	// 摘除已在配置中删除的容器。
	m.mu.RLock()
	var stale []string
	for name := range m.monitors {
		if _, ok := desired[name]; !ok {
			stale = append(stale, name)
		}
	}
	m.mu.RUnlock()
	for _, name := range stale {
		m.removeContainer(name)
		m.log.Info("容器 %s 已从配置中移除，监控已停止", name)
	}

	// 安装/更新剩余容器。
	for _, cc := range snap.Containers {
		m.installContainer(cc, snap.MonitorKeywords)
	}
}

// Run 启动全部服务并阻塞，直到收到退出信号或 ctx 取消。
// 返回值为进程退出码。
func (m *Manager) Run(ctx context.Context, webAddr string) int {
	m.startedAt = time.Now()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	snap := m.store.Snapshot()
	m.log.Info("综合管理器已启动，共管理 %d 个容器，%d 个定时任务",
		len(m.monitors), m.sched.Jobs())
	m.log.Info("当前时区: %s，本地时间: %s", time.Local.String(), time.Now().Format("2006-01-02 15:04:05"))
	if next, ok := m.sched.NextRun(time.Now()); ok {
		m.log.Info("下一个计划启动时刻: %s（还有 %s）",
			next.Format("2006-01-02 15:04:05"), next.Sub(time.Now()).Round(time.Second))
	}

	m.video.Start(ctx)

	// 启动前探测 Docker Engine 是否可达，不可达只告警不阻塞
	//（原脚本同样允许 docker 缺失，此时媒体转码仍可工作）。
	if err := m.docker.Available(ctx); err != nil {
		m.log.Warn("Docker Engine 不可达: %v，容器控制将不可用", err)
	}

	// 启动 Web 管理界面。
	// 注意：Web 地址是用户在命令行/环境变量里显式给出的，说明管理界面是预期功能。
	// 此时监听失败（例如端口被占用）不能只记一条日志就继续跑——进程看起来"正常运行"，
	// 实际管理界面完全不可用，用户无从察觉。所以这里直接报错并返回非零退出码。
	if webAddr != "" && webAddr != "off" {
		if err := m.startWeb(webAddr); err != nil {
			m.log.Error("%v", err)
			cancel()
			// shutdown 内部已包含视频处理器与容器的停止逻辑。
			m.shutdown()
			return 1
		}
	} else {
		m.log.Info("Web 管理界面已禁用")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		m.sched.Run(ctx)
	}()

	_ = snap
	select {
	case sig := <-sigCh:
		m.log.Info("收到信号 %s，正在停止所有服务...", sig)
	case <-ctx.Done():
		m.log.Info("上下文已取消，正在停止所有服务...")
	}

	cancel()
	m.shutdown()
	<-schedDone
	return 0
}

// startWeb 启动 Web 管理服务。
func (m *Manager) startWeb(addr string) error {
	srv := web.New(web.Options{
		Addr:    addr,
		Store:   m.store,
		Monitor: m, // Manager 实现 web.Controller
		Log:     m.log,
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", addr, err)
	}
	m.webSrv = srv
	m.srv = &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := m.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.log.Error("Web 服务异常退出: %v", err)
		}
	}()
	m.log.Info("Web 管理界面已启动: http://%s", ln.Addr().String())
	return nil
}

// shutdown 停止 Web、容器与媒体处理。
func (m *Manager) shutdown() {
	if m.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := m.srv.Shutdown(ctx); err != nil {
			m.log.Warn("Web 服务关闭超时: %v", err)
		}
		cancel()
	}

	m.mu.RLock()
	monitors := make([]*monitor.ContainerMonitor, 0, len(m.monitors))
	for _, mon := range m.monitors {
		monitors = append(monitors, mon)
	}
	m.mu.RUnlock()

	running := 0
	for _, mon := range monitors {
		if mon.IsRunning() {
			running++
		}
	}
	m.log.Info("正在停止 %d 个运行中的容器...", running)

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, mon := range monitors {
			mon := mon
			wg.Add(1)
			go func() {
				defer wg.Done()
				mon.Stop(monitor.ReasonShutdown)
			}()
		}
		wg.Wait()
	}()

	select {
	case <-done:
		m.log.Info("所有容器已处理完毕")
	case <-time.After(3 * time.Minute):
		m.log.Warn("停止容器超时，继续退出")
	}

	m.video.Stop()
	m.log.Info("已运行 %s，程序退出", time.Since(m.startedAt).Round(time.Second))
}

// ---- 供 Web 层调用的控制器接口实现 ----

// Monitors 返回容器监控器列表。
func (m *Manager) Monitors() []*monitor.ContainerMonitor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*monitor.ContainerMonitor, 0, len(m.order))
	for _, name := range m.order {
		if mon, ok := m.monitors[name]; ok {
			out = append(out, mon)
		}
	}
	return out
}

// MonitorByName 按名称取监控器。
func (m *Manager) MonitorByName(name string) (*monitor.ContainerMonitor, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mon, ok := m.monitors[name]
	return mon, ok
}

// Scheduler 返回调度器。
func (m *Manager) Scheduler() *scheduler.Scheduler { return m.sched }

// Video 返回媒体处理器。
func (m *Manager) Video() *media.Processor { return m.video }

// VideoStatus 返回媒体处理器状态快照，供 Web 层展示。
func (m *Manager) VideoStatus() web.VideoStatus {
	return web.VideoStatus{
		WatchDir:   m.video.WatchDir(),
		HistoryDir: m.video.HistoryDir(),
		Pending:    m.video.Pending(),
	}
}

// Store 返回配置存储。
func (m *Manager) Store() *runtime.Store { return m.store }

// StartedAt 返回启动时间。
func (m *Manager) StartedAt() time.Time { return m.startedAt }

// ApplyContainer 应用单个容器的配置变更（新增或修改）。
func (m *Manager) ApplyContainer(cc config.ContainerConfig) error {
	if _, _, err := m.store.UpsertContainer(cc); err != nil {
		return err
	}
	m.installContainer(cc, m.store.Snapshot().MonitorKeywords)
	return nil
}

// DeleteContainer 删除容器。
func (m *Manager) DeleteContainer(name string) error {
	if err := m.store.DeleteContainer(name); err != nil {
		return err
	}
	m.removeContainer(name)
	return nil
}

// StartContainer 手动启动容器。
func (m *Manager) StartContainer(name string) error {
	mon, ok := m.MonitorByName(name)
	if !ok {
		return fmt.Errorf("容器 %q 不存在", name)
	}
	if mon.IsRunning() {
		return fmt.Errorf("容器 %q 已在运行中", name)
	}
	go mon.Start()
	return nil
}

// StopContainer 手动停止容器。
func (m *Manager) StopContainer(name string) error {
	mon, ok := m.MonitorByName(name)
	if !ok {
		return fmt.Errorf("容器 %q 不存在", name)
	}
	if !mon.IsRunning() {
		return fmt.Errorf("容器 %q 未在运行", name)
	}
	go mon.Stop(monitor.StopReason("手动停止"))
	return nil
}

// RunJobNow 立即触发某个定时时刻对应的任务（用于"立即执行一次"）。
func (m *Manager) RunJobNow(name string) error {
	mon, ok := m.MonitorByName(name)
	if !ok {
		return fmt.Errorf("容器 %q 不存在", name)
	}
	if mon.IsRunning() {
		return fmt.Errorf("容器 %q 已在运行中", name)
	}
	m.log.Info("手动触发容器启动: %s", name)
	go mon.Start()
	return nil
}

// UpdateSettings 更新全局设置；影响所有容器的项需要 Reload。
func (m *Manager) UpdateSettings(cfg *config.Config) error {
	if err := m.store.UpdateSettings(func(cur *config.Config) error {
		cur.WatchDir = cfg.WatchDir
		cur.HistoryDir = cfg.HistoryDir
		cur.CheckInterval = cfg.CheckInterval
		cur.StableDelay = cfg.StableDelay
		cur.ArchiveAfterHours = cfg.ArchiveAfterHours
		cur.MP3Bitrate = cfg.MP3Bitrate
		cur.MonitorKeywords = cfg.MonitorKeywords
		return nil
	}); err != nil {
		return err
	}
	m.Reload()
	return nil
}

// ReloadConfig 从磁盘重新加载配置并热应用。用于手工编辑文件后的重载。
func (m *Manager) ReloadConfig() error {
	cfg, err := config.Load(m.store.Path())
	if err != nil {
		return err
	}
	if err := m.store.UpdateSettings(func(cur *config.Config) error {
		*cur = *cfg
		return nil
	}); err != nil {
		return err
	}
	m.Reload()
	return nil
}
