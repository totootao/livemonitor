// Package scheduler 提供每日定点任务调度，语义对齐 Python 的 schedule.every().day.at()。
package scheduler

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// Job 是一个每日在固定时刻执行的调度项。
type Job struct {
	// Name 用于日志展示。
	Name string
	// At 是每日执行时刻（仅取时钟部分，日期无关）。
	At time.Time
	// Fn 是任务函数，同步执行；长耗时逻辑应自行起 goroutine。
	Fn func()
}

// Scheduler 按秒轮询到点的任务。
type Scheduler struct {
	mu    sync.Mutex
	jobs  []*scheduledJob
	log   *logging.Logger
	tick  time.Duration
	loc   *time.Location
	nowFn func() time.Time
}

type scheduledJob struct {
	Job
	lastRunDate string // YYYY-MM-DD，避免同一分钟重复触发
}

// New 创建调度器。
func New(log *logging.Logger) *Scheduler {
	return &Scheduler{
		log:   log,
		tick:  time.Second,
		loc:   time.Local,
		nowFn: time.Now,
	}
}

// SetNowFunc 注入时间源，便于单元测试。
func (s *Scheduler) SetNowFunc(fn func() time.Time) {
	s.mu.Lock()
	s.nowFn = fn
	s.mu.Unlock()
}

// SetTick 调整轮询间隔，默认 1 秒。
func (s *Scheduler) SetTick(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.tick = d
	s.mu.Unlock()
}

// Add 注册一个每日任务。
func (s *Scheduler) Add(job Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, &scheduledJob{Job: job})
	sort.SliceStable(s.jobs, func(i, j int) bool {
		return s.jobs[i].At.Format("15:04:05") < s.jobs[j].At.Format("15:04:05")
	})
}

// Jobs 返回已注册任务的数量。
func (s *Scheduler) Jobs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// Run 阻塞运行调度循环，直到 ctx 被取消。
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("调度器已停止")
			return
		case <-ticker.C:
			s.runPending()
		}
	}
}

// runPending 执行所有已到点的任务。
func (s *Scheduler) runPending() {
	now := s.nowFn()
	dateKey := now.Format("2006-01-02")

	s.mu.Lock()
	var due []*scheduledJob
	for _, j := range s.jobs {
		if j.lastRunDate == dateKey {
			continue
		}
		// 当天应执行的时刻。
		target := time.Date(now.Year(), now.Month(), now.Day(),
			j.At.Hour(), j.At.Minute(), j.At.Second(), 0, s.loc)
		if !now.Before(target) {
			// 已过点（含轮询导致的轻微延迟），当天只触发一次。
			j.lastRunDate = dateKey
			due = append(due, j)
		}
	}
	s.mu.Unlock()

	for _, j := range due {
		s.log.Info("触发定时任务: %s (计划时刻 %s)", j.Name, j.At.Format("15:04:05"))
		go func(job *scheduledJob) {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("定时任务 %s 发生 panic: %v", job.Name, r)
				}
			}()
			job.Fn()
		}(j)
	}
}

// NextRun 计算下一个计划执行时刻，用于启动提示。
func (s *Scheduler) NextRun(now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) == 0 {
		return time.Time{}, false
	}
	var next time.Time
	for _, j := range s.jobs {
		target := time.Date(now.Year(), now.Month(), now.Day(),
			j.At.Hour(), j.At.Minute(), j.At.Second(), 0, now.Location())
		if !target.After(now) {
			target = target.Add(24 * time.Hour)
		}
		if next.IsZero() || target.Before(next) {
			next = target
		}
	}
	return next, true
}
