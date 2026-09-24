// Package scheduler 提供每日定点任务调度，语义对齐 Python 的 schedule.every().day.at()。
package scheduler

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// Job 是一个每日在固定时刻执行的调度项。
type Job struct {
	// ID 唯一标识一个调度项（容器名 + 时刻），用于精确删除与去重。
	ID string
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
// 若已存在相同 ID 的任务，则原地替换，避免热更新时产生重复触发。
func (s *Scheduler) Add(job Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.jobs {
		if job.ID != "" && existing.ID == job.ID {
			s.jobs[i] = &scheduledJob{Job: job}
			s.sortLocked()
			return
		}
	}
	s.jobs = append(s.jobs, &scheduledJob{Job: job})
	s.sortLocked()
}

func (s *Scheduler) sortLocked() {
	sort.SliceStable(s.jobs, func(i, j int) bool {
		return s.jobs[i].At.Format("15:04:05") < s.jobs[j].At.Format("15:04:05")
	})
}

// RemoveByPrefix 删除 ID 以 prefix 开头的所有任务，返回删除数量。
// 容器被删除时用它清理该容器的全部启动时刻。
func (s *Scheduler) RemoveByPrefix(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.jobs[:0]
	removed := 0
	for _, j := range s.jobs {
		if strings.HasPrefix(j.ID, prefix) {
			removed++
			continue
		}
		kept = append(kept, j)
	}
	s.jobs = kept
	return removed
}

// Reset 清空全部任务。
func (s *Scheduler) Reset() {
	s.mu.Lock()
	s.jobs = nil
	s.mu.Unlock()
}

// Jobs 返回已注册任务的数量。
func (s *Scheduler) Jobs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// JobInfo 是任务的可读快照，供 Web 界面展示。
type JobInfo struct {
	ID string `json:"id"`
	// Name 是归属容器的名称。
	Name string `json:"name"`
	// Time 是每日执行时刻，格式 HH:MM。
	Time string `json:"time"`
	// LastRunDate 是最近一次触发的日期（YYYY-MM-DD），空表示尚未触发。
	LastRunDate string `json:"lastRunDate"`
	// NextRun 是下一次计划触发时刻。
	NextRun time.Time `json:"nextRun"`
}

// Snapshot 返回所有任务的可读快照，按时刻排序。
func (s *Scheduler) Snapshot(now time.Time) []JobInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]JobInfo, 0, len(s.jobs))
	for _, j := range s.jobs {
		target := time.Date(now.Year(), now.Month(), now.Day(),
			j.At.Hour(), j.At.Minute(), j.At.Second(), 0, now.Location())
		if !target.After(now) {
			target = target.Add(24 * time.Hour)
		}
		out = append(out, JobInfo{
			ID:          j.ID,
			Name:        j.Name,
			Time:        j.At.Format("15:04"),
			LastRunDate: j.lastRunDate,
			NextRun:     target,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
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
