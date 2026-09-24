package scheduler

import (
	"context"
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
