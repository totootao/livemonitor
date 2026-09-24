// Package scheduler 提供每日定点任务调度，语义对齐 Python 的 schedule.every().day.at()。
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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

	// statePath 是触发记录的落盘路径，为空表示不持久化。
	statePath string
	// fired 记录每个任务最近一次触发的日期（任务 ID -> YYYY-MM-DD）。
	//
	// 必须持久化的原因：这个字段是"今天是否已执行"的唯一依据。放在内存里的话，
	// 进程重启或任何配置热更新（Reload 会重建 Job 对象）都会把它清零，
	// 导致当天所有已过点的任务被重放一遍——表现为同一容器在几分钟内被反复启动。
	fired map[string]string
}

type scheduledJob struct {
	Job
	// lastRunDate 是当天是否已触发的运行时快照，真值以 Scheduler.fired 为准。
	lastRunDate string
}

// New 创建调度器。
func New(log *logging.Logger) *Scheduler {
	return &Scheduler{
		log:   log,
		tick:  time.Second,
		loc:   time.Local,
		nowFn: time.Now,
		fired: make(map[string]string),
	}
}

// SetStatePath 设置触发记录的持久化路径，并立即载入已有记录。
//
// 载入失败不算致命：最坏情况是当天任务被重放一次，比让整个服务起不来要好。
func (s *Scheduler) SetStatePath(path string) {
	s.mu.Lock()
	s.statePath = path
	s.mu.Unlock()
	s.loadState()
}

// loadState 从磁盘读回触发记录，只保留当天的条目。
func (s *Scheduler) loadState() {
	s.mu.Lock()
	path := s.statePath
	s.mu.Unlock()
	if path == "" {
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("读取调度状态失败（将按未执行处理）: %v", err)
		}
		return
	}
	var saved map[string]string
	if err := json.Unmarshal(data, &saved); err != nil {
		s.log.Warn("解析调度状态失败（将按未执行处理）: %v", err)
		return
	}

	// 只接受今天的记录。昨天的记录没有意义，留着反而会干扰日期比较。
	today := s.nowFn().Format("2006-01-02")
	s.mu.Lock()
	kept := 0
	for id, date := range saved {
		if date == today {
			s.fired[id] = date
			kept++
		}
	}
	s.mu.Unlock()

	if kept > 0 {
		s.log.Info("已恢复当天的调度记录：%d 个任务今天已执行", kept)
	}
}

// saveState 把触发记录写回磁盘（原子替换，避免写一半时进程退出导致文件损坏）。
func (s *Scheduler) saveState() {
	s.mu.Lock()
	path := s.statePath
	if path == "" {
		s.mu.Unlock()
		return
	}
	snapshot := make(map[string]string, len(s.fired))
	for id, date := range s.fired {
		snapshot[id] = date
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		s.log.Warn("序列化调度状态失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.log.Warn("创建调度状态目录失败: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		s.log.Warn("写入调度状态失败: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		s.log.Warn("替换调度状态文件失败: %v", err)
		_ = os.Remove(tmp)
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
// 若已存在相同 ID 的任务，则原地替换，同时**保留其触发记录**——
// 热更新配置时不应该让今天已经执行过的任务重新获得触发机会。
func (s *Scheduler) Add(job Job) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var lastRun string
	if job.ID != "" {
		lastRun = s.fired[job.ID]
	}
	entry := &scheduledJob{Job: job, lastRunDate: lastRun}

	for i, existing := range s.jobs {
		if job.ID != "" && existing.ID == job.ID {
			s.jobs[i] = entry
			s.sortLocked()
			return
		}
	}
	s.jobs = append(s.jobs, entry)
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

// ClearRunRecord 清除某个任务今天的触发记录，让它当天可以再次触发。
//
// 用途：Web 界面的"立即执行"是人的显式意图，不该被"今天已经跑过"挡住。
// 调用后任务在下一次轮询时若已过点便立即触发。
func (s *Scheduler) ClearRunRecord(id string) {
	s.mu.Lock()
	delete(s.fired, id)
	for _, j := range s.jobs {
		if j.ID == id {
			j.lastRunDate = ""
		}
	}
	s.mu.Unlock()
	s.saveState()
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
		lastRun := j.lastRunDate
		if v, ok := s.fired[j.ID]; ok {
			lastRun = v
		}
		out = append(out, JobInfo{
			ID:          j.ID,
			Name:        j.Name,
			Time:        j.At.Format("15:04"),
			LastRunDate: lastRun,
			NextRun:     target,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

// RunOnce 同步执行一轮到点检查，返回是否触发了任务。
//
// 生产代码走 Run 的定时轮询；这个入口供测试使用，让"是否重放"这类断言
// 不必依赖 sleep 去等一个轮询周期。
//
// 实现上刻意只做两件事：给任务函数套一层"完成即上报"的包装，
// 然后原样调用 runPending。判定逻辑一律复用生产路径——
// 如果这里另写一份"是否到点"的判断（比如再查一次 s.fired），
// 测出来的就是测试自己的逻辑而不是线上那套，会出现"测试全绿但 bug 仍在"。
// 这个坑踩过一次：早期版本自带 isDueLocked，回退生产代码后测试依然通过。
func (s *Scheduler) RunOnce() bool {
	done := make(chan struct{}, 64)

	s.mu.Lock()
	for _, j := range s.jobs {
		inner := j.Fn
		name := j.Name
		j.Fn = func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("定时任务 %s 发生 panic: %v", name, r)
				}
				done <- struct{}{}
			}()
			inner()
		}
	}
	s.mu.Unlock()

	s.runPending()

	// runPending 对每个到点任务起一个 goroutine，这里用"静默窗口"收敛：
	// 等到至少一次上报后，再静默一小会儿就认为本轮没有更多任务了。
	select {
	case <-done:
		for {
			select {
			case <-done:
			case <-time.After(20 * time.Millisecond):
				return true
			}
		}
	case <-time.After(50 * time.Millisecond):
		return false
	}
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
		// 真值以 fired 为准：它可能来自上一次进程运行或上一次配置热更新。
		if last, ok := s.fired[j.ID]; ok && last == dateKey {
			j.lastRunDate = last
			continue
		}
		if j.lastRunDate == dateKey {
			continue
		}
		// 当天应执行的时刻。
		target := time.Date(now.Year(), now.Month(), now.Day(),
			j.At.Hour(), j.At.Minute(), j.At.Second(), 0, s.loc)
		if !now.Before(target) {
			// 已过点（含轮询导致的轻微延迟），当天只触发一次。
			j.lastRunDate = dateKey
			if j.ID != "" {
				s.fired[j.ID] = dateKey
			}
			due = append(due, j)
		}
	}
	s.mu.Unlock()

	if len(due) == 0 {
		return
	}
	// 先落盘再执行：万一下游 panic 或进程被杀，也已经记下"今天跑过了"，
	// 不至于重启后又来一遍。
	s.saveState()

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
