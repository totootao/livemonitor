package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// TestFiresAtScheduledTime 验证任务会在到点后触发且当天只触发一次。
func TestFiresAtScheduledTime(t *testing.T) {
	s := New(logging.New("test"))

	var mu sync.Mutex
	var runs []time.Time

	// 计划时刻设为当前时间的 1 秒前，构造"已到点"的初始状态。
	past := time.Now().Add(-time.Second)
	s.Add(Job{
		Name: "job-a",
		At:   past,
		Fn: func() {
			mu.Lock()
			runs = append(runs, time.Now())
			mu.Unlock()
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go s.Run(ctx)

	time.Sleep(1500 * time.Millisecond)

	mu.Lock()
	n := len(runs)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("任务触发次数 = %d, 期望 1（同一天不应重复触发）", n)
	}
}

// TestDoesNotFireBeforeTime 验证未到点的任务不会提前触发。
func TestDoesNotFireBeforeTime(t *testing.T) {
	s := New(logging.New("test"))
	fired := make(chan struct{}, 1)
	s.Add(Job{
		Name: "future",
		At:   time.Now().Add(2 * time.Hour),
		Fn:   func() { fired <- struct{}{} },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	go s.Run(ctx)
	<-ctx.Done()

	select {
	case <-fired:
		t.Error("未到点的任务被提前触发")
	default:
	}
}

// TestMultipleTimesSameContainer 验证同一容器的多个时间点都会注册。
func TestMultipleTimesSameContainer(t *testing.T) {
	s := New(logging.New("test"))
	var count int
	var mu sync.Mutex
	fn := func() {
		mu.Lock()
		count++
		mu.Unlock()
	}
	for _, ts := range []string{"17:03", "17:13", "17:33"} {
		at, err := time.ParseInLocation("15:04", ts, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		s.Add(Job{Name: "c", At: at, Fn: fn})
	}
	if s.Jobs() != 3 {
		t.Errorf("任务数 = %d, 期望 3", s.Jobs())
	}
}

// TestNextRunReturnsEarliest 验证下一次执行时刻计算正确。
// 注意：23:30 晚于 13:00，属于"当天稍后"，因此应优先于次日的 06:15。
func TestNextRunReturnsEarliest(t *testing.T) {
	s := New(logging.New("test"))
	for _, ts := range []string{"23:30", "06:15", "12:00"} {
		at, _ := time.ParseInLocation("15:04", ts, time.Local)
		s.Add(Job{Name: "c", At: at, Fn: func() {}})
	}

	// 基准 05:00：06:15 尚未到达，应为当天 06:15。
	base := time.Date(2026, 3, 1, 5, 0, 0, 0, time.Local)
	next, ok := s.NextRun(base)
	if !ok {
		t.Fatal("应有下一次执行时刻")
	}
	if next.Hour() != 6 || next.Minute() != 15 || next.Day() != 1 {
		t.Errorf("下一执行时刻 = %s, 期望当天 06:15", next.Format("2006-01-02 15:04"))
	}

	// 基准 13:00：06:15 与 12:00 已过，但 23:30 仍在当天，应为当天 23:30。
	base = time.Date(2026, 3, 1, 13, 0, 0, 0, time.Local)
	next, _ = s.NextRun(base)
	if next.Hour() != 23 || next.Minute() != 30 || next.Day() != 1 {
		t.Errorf("下一执行时刻 = %s, 期望当天 23:30", next.Format("2006-01-02 15:04"))
	}

	// 基准 23:45：当天所有时刻均已过，应跨到次日 06:15。
	base = time.Date(2026, 3, 1, 23, 45, 0, 0, time.Local)
	next, _ = s.NextRun(base)
	if next.Hour() != 6 || next.Minute() != 15 || next.Day() != 2 {
		t.Errorf("下一执行时刻 = %s, 期望次日 06:15", next.Format("2006-01-02 15:04"))
	}
}

// TestPanicInJobDoesNotCrashScheduler 任务 panic 不应导致调度器退出。
func TestPanicInJobDoesNotCrashScheduler(t *testing.T) {
	s := New(logging.New("test"))
	done := make(chan struct{})
	s.Add(Job{Name: "panicky", At: time.Now().Add(-time.Second), Fn: func() {
		defer close(done)
		panic("boom")
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() { s.Run(ctx); close(runDone) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("任务未执行")
	}

	// 调度器应仍在运行。
	select {
	case <-runDone:
		t.Error("单个任务 panic 不应终止调度器")
	case <-time.After(300 * time.Millisecond):
	}
}

// ---------- 触发记录持久化 ----------

// TestStatePersistsAcrossRestart 是这次线上故障的核心回归用例。
//
// 现象：同一个容器在几分钟内被反复启动，日志里出现多条
// "容器已在运行，本次计划跳过"。根因是"今天是否已执行"只存在内存里，
// 进程重启（或任何配置热更新）都会把它清零，于是当天所有已过点的任务被重放。
//
// 这里模拟：第一个调度器触发过任务 → 落盘 → 第二个调度器（等价于重启后的进程）
// 加载同一份状态，即使任务时刻已过也**不能**再触发一次。
func TestStatePersistsAcrossRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "scheduler-state.json")

	// ---- 第一个进程 ----
	s1 := New(logging.New("test"))
	s1.SetStatePath(statePath)
	s1.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})

	if !s1.RunOnce() {
		t.Fatal("首个进程应触发已到点的任务")
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("触发后应写出状态文件: %v", err)
	}

	// ---- 第二个进程（重启） ----
	s2 := New(logging.New("test"))
	s2.SetStatePath(statePath)

	var mu sync.Mutex
	fired := 0
	s2.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {
		mu.Lock()
		fired++
		mu.Unlock()
	}})

	if s2.RunOnce() {
		mu.Lock()
		n := fired
		mu.Unlock()
		t.Fatalf("重启后不应重放当天已执行的任务，实际触发 %d 次", n)
	}

	mu.Lock()
	n := fired
	mu.Unlock()
	if n != 0 {
		t.Errorf("重启后任务触发次数 = %d, 期望 0", n)
	}
}

// TestStateSurvivesJobRebuild 覆盖配置热更新的场景。
//
// Reload 会 RemoveByPrefix + Add 重建所有 Job 对象。如果 Add 不继承已有的
// 触发记录，重建后的任务会因为 lastRunDate 为空而再次触发——
// 这正是日志里同一个容器名出现两次的原因。
func TestStateSurvivesJobRebuild(t *testing.T) {
	s := New(logging.New("test"))
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})
	if !s.RunOnce() {
		t.Fatal("首次应触发")
	}

	// 模拟 installContainer：先移除，再以同样的 ID 重新添加。
	s.RemoveByPrefix("c1")
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})

	if s.RunOnce() {
		t.Error("任务重建后不应重放当天已执行的任务")
	}
}

// TestStateSurvivesReloadWithoutRemove 覆盖只 Add 不 Remove 的幂等路径。
func TestStateSurvivesReloadWithoutRemove(t *testing.T) {
	s := New(logging.New("test"))
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})
	if !s.RunOnce() {
		t.Fatal("首次应触发")
	}
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})
	if s.RunOnce() {
		t.Error("重复 Add 同一任务不应导致重放")
	}
}

// TestLoadStateDropsYesterdayEntries 昨天的记录没有意义，必须丢弃。
func TestLoadStateDropsYesterdayEntries(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "scheduler-state.json")

	// 直接写一份"昨天触发过"的状态文件。
	stale := `{"c1":"2020-01-01"}`
	if err := os.WriteFile(statePath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(logging.New("test"))
	s.SetStatePath(statePath)

	var fired int
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() { fired++ }})
	if !s.RunOnce() {
		t.Error("昨天的触发记录不应阻止今天执行")
	}

	time.Sleep(100 * time.Millisecond)
	if fired != 1 {
		t.Errorf("任务触发次数 = %d, 期望 1", fired)
	}
}

// TestLoadStateHandlesCorruptFile 状态文件损坏时应降级为"未执行"，而不是崩溃。
func TestLoadStateHandlesCorruptFile(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "scheduler-state.json")
	if err := os.WriteFile(statePath, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(logging.New("test"))
	s.SetStatePath(statePath) // 不应 panic

	if !s.RunOnce() {
		t.Log("损坏状态下按未执行处理")
	}
}

// TestLoadStateMissingFileIsFine 状态文件不存在是首次启动的正常情形。
func TestLoadStateMissingFileIsFine(t *testing.T) {
	s := New(logging.New("test"))
	s.SetStatePath(filepath.Join(t.TempDir(), "does-not-exist.json"))
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})

	if !s.RunOnce() {
		t.Error("首次启动应正常触发任务")
	}
}

// TestSaveStateIsAtomic 落盘应经由临时文件 + rename，不留半截文件。
func TestSaveStateIsAtomic(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "scheduler-state.json")

	s := New(logging.New("test"))
	s.SetStatePath(statePath)
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})
	s.RunOnce()

	if _, err := os.Stat(statePath + ".tmp"); !os.IsNotExist(err) {
		t.Error("落盘后不应残留 .tmp 文件")
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	if !stringsContains(string(data), today) {
		t.Errorf("状态文件应包含今天的日期 %s，实际内容: %s", today, data)
	}
}

// TestSaveStateCreatesMissingDir 状态目录不存在时应自动创建。
func TestSaveStateCreatesMissingDir(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "nested", "deeper", "scheduler-state.json")

	s := New(logging.New("test"))
	s.SetStatePath(statePath)
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})
	s.RunOnce()

	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("应自动创建目录并写出状态文件: %v", err)
	}
}

// TestNoStatePathStillWorks 未配置落盘路径时，行为应与旧版一致（内存记录）。
func TestNoStatePathStillWorks(t *testing.T) {
	s := New(logging.New("test"))
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})
	if !s.RunOnce() {
		t.Error("未配置状态文件时仍应正常触发")
	}
	if s.RunOnce() {
		t.Error("同一天不应重复触发")
	}
}

// TestClearRunRecord 清除记录后当天可以再触发一次（供 Web "立即执行" 使用）。
func TestClearRunRecord(t *testing.T) {
	s := New(logging.New("test"))
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})
	if !s.RunOnce() {
		t.Fatal("首次应触发")
	}
	if s.RunOnce() {
		t.Fatal("同一天不应重复触发")
	}

	s.ClearRunRecord("c1")
	if !s.RunOnce() {
		t.Error("清除记录后应可再次触发")
	}
}

// TestClearRunRecordPersists 清除也应落盘，否则重启后记录会"复活"。
func TestClearRunRecordPersists(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "scheduler-state.json")

	s := New(logging.New("test"))
	s.SetStatePath(statePath)
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(-time.Second), Fn: func() {}})
	s.RunOnce()
	s.ClearRunRecord("c1")

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	if stringsContains(string(data), today) {
		t.Errorf("清除后状态文件不应再含今天的记录，实际: %s", data)
	}
}

// TestSnapshotReflectsRestoredState 重启后 Web 界面看到的"上次执行日期"应正确。
func TestSnapshotReflectsRestoredState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "scheduler-state.json")
	today := time.Now().Format("2006-01-02")

	if err := os.WriteFile(statePath, []byte(`{"c1":"`+today+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(logging.New("test"))
	s.SetStatePath(statePath)
	s.Add(Job{ID: "c1", Name: "c1", At: time.Now().Add(time.Hour), Fn: func() {}})

	for _, info := range s.Snapshot(time.Now()) {
		if info.ID == "c1" {
			if info.LastRunDate != today {
				t.Errorf("LastRunDate = %q, 期望 %q", info.LastRunDate, today)
			}
			return
		}
	}
	t.Error("未找到任务 c1 的快照")
}

func stringsContains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
