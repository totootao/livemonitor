// Package manager 编排容器监控、定时调度与媒体处理。
package manager

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/dockerctl"
	"github.com/totootao/livemonitor/internal/logging"
	"github.com/totootao/livemonitor/internal/media"
	"github.com/totootao/livemonitor/internal/monitor"
	"github.com/totootao/livemonitor/internal/scheduler"
)

// Manager 是综合管理器。
type Manager struct {
	cfg      *config.Config
	log      *logging.Logger
	docker   *dockerctl.Client
	sched    *scheduler.Scheduler
	video    *media.Processor
	monitors []*monitor.ContainerMonitor

	startedAt time.Time
}

// New 依据配置构建管理器。
func New(cfg *config.Config, log *logging.Logger) (*Manager, error) {
	dockerClient := dockerctl.New(log)

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
		cfg:    cfg,
		log:    log,
		docker: dockerClient,
		sched:  scheduler.New(log),
		video:  video,
	}

	for _, cc := range cfg.Containers {
		mon := monitor.New(cc, cfg.MonitorKeywords, dockerClient, logging.New(cc.Name))
		m.monitors = append(m.monitors, mon)

		for _, ts := range cc.StartTimes {
			at, err := config.ParseClock(ts)
			if err != nil {
				// 已在配置校验阶段拦截，这里只是兜底。
				log.Error("容器 %s 的时间 %q 非法，已跳过: %v", cc.Name, ts, err)
				continue
			}
			name := cc.Name
			monRef := mon
			m.sched.Add(scheduler.Job{
				Name: cc.Name,
				At:   at,
				Fn:   func() { monRef.Start() },
			})
			log.Info("已为容器 %s 配置定时启动: %s（每天）", name, at.Format("15:04"))
		}
	}

	return m, nil
}

// Run 启动全部服务并阻塞，直到收到退出信号或 ctx 取消。
// 返回值为进程退出码。
func (m *Manager) Run(ctx context.Context) int {
	m.startedAt = time.Now()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	m.log.Info("综合管理器已启动，共管理 %d 个容器，%d 个定时任务",
		len(m.monitors), m.sched.Jobs())
	m.log.Info("当前时区: %s，本地时间: %s", time.Local.String(), time.Now().Format("2006-01-02 15:04:05"))
	if next, ok := m.sched.NextRun(time.Now()); ok {
		m.log.Info("下一个计划启动时刻: %s（还有 %s）",
			next.Format("2006-01-02 15:04:05"), next.Sub(time.Now()).Round(time.Second))
	}

	m.video.Start(ctx)

	// 启动前检查依赖，缺失只告警不阻塞（原脚本同样允许 ffmpeg/docker 缺失）。
	if err := m.docker.BinaryAvailable(); err != nil {
		m.log.Warn("docker 依赖检查未通过: %v，容器控制将不可用", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		m.sched.Run(ctx)
	}()

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

// shutdown 停止所有容器与媒体处理。
func (m *Manager) shutdown() {
	var running int
	for _, mon := range m.monitors {
		if mon.IsRunning() {
			running++
		}
	}
	m.log.Info("正在停止 %d 个运行中的容器...", running)

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg = make(chan struct{}, len(m.monitors))
		for _, mon := range m.monitors {
			mon := mon
			go func() {
				defer func() { wg <- struct{}{} }()
				mon.Stop(monitor.ReasonShutdown)
			}()
		}
		for range m.monitors {
			<-wg
		}
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

// Monitors 返回容器监控器列表（供测试与状态查询）。
func (m *Manager) Monitors() []*monitor.ContainerMonitor { return m.monitors }

// Scheduler 返回调度器（供测试）。
func (m *Manager) Scheduler() *scheduler.Scheduler { return m.sched }

// Video 返回媒体处理器（供测试）。
func (m *Manager) Video() *media.Processor { return m.video }
