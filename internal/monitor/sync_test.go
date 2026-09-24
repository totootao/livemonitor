package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/dockerctl"
)

// TestSyncStateCorrectsStaleRunning 是本轮的核心回归用例。
//
// 早先的现象：容器实际上已经停止，程序却一直认为它在运行——
// 界面显示"运行中"，手动启动被以"已在运行中"拒绝，手动停止被以"未在运行"拒绝。
// 根因是 m.running 只在 Start/Stop 被调用时更新，容器被外部停掉后无人通知。
func TestSyncStateCorrectsStaleRunning(t *testing.T) {
	fn := newFakeRunner()
	// Docker 报告：容器没在运行。
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{Running: false}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	// 程序先进入运行态（等价于"之前启动过，之后容器被外部停掉了"）。
	m.enterRunning(time.Now().Add(-10 * time.Minute))
	if !m.IsRunning() {
		t.Fatal("前置条件：内部应认为在运行")
	}

	stopped := make(chan string, 1)
	m.SetHooks(nil, func(reason string) { stopped <- reason })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if m.SyncState(ctx) {
		t.Error("SyncState 应报告容器未在运行")
	}
	if m.IsRunning() {
		t.Error("SyncState 后内部记账必须被纠正为未运行")
	}

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Error("纠正状态时应触发 onStopped 回调")
	}
}

// TestSyncStateKeepsRunningWhenActuallyRunning 容器确实在跑时不应误判为停止。
func TestSyncStateKeepsRunningWhenActuallyRunning(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{Running: true, StartedAt: time.Now().Add(-time.Minute)}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)
	m.enterRunning(time.Now().Add(-time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if !m.SyncState(ctx) {
		t.Error("容器在运行时 SyncState 应返回 true")
	}
	if !m.IsRunning() {
		t.Error("容器在运行时不应被纠正为未运行")
	}
}

// TestSyncStateQueryFailureKeepsCurrentState 查询失败时保守沿用旧值。
//
// 宁可短暂显示旧的"运行中"，也不要因为一次网络抖动就把运行中的容器
// 标成已停止——那会导致界面误导，甚至让后续判断把正在跑的容器又启动一次。
func TestSyncStateQueryFailureKeepsCurrentState(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{}, errors.New("connection refused")
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)
	m.enterRunning(time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if !m.SyncState(ctx) {
		t.Error("查询失败时应沿用现有的运行状态")
	}
	if !m.IsRunning() {
		t.Error("查询失败不应改动内部记账")
	}
}

// TestSyncStateContainerDeleted 容器被删除时也要纠正为未运行。
func TestSyncStateContainerDeleted(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{}, errors.New("容器 ghost 不存在")
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)
	m.enterRunning(time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if m.SyncState(ctx) {
		t.Error("容器已被删除，应报告未在运行")
	}
	if m.IsRunning() {
		t.Error("容器被删除后内部记账必须被纠正")
	}
}

// TestSyncStateNotRunningStaysNotRunning 本来就没在跑时不应产生多余动作。
func TestSyncStateNotRunningStaysNotRunning(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{Running: false}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	called := false
	m.SetHooks(nil, func(string) { called = true })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if m.SyncState(ctx) {
		t.Error("未运行时应返回 false")
	}
	if called {
		t.Error("本来就没在运行时不应触发 onStopped")
	}
}

// TestSyncStateRestartingCountsAsRunning 重启中的容器不能被当成已停止，
// 否则会被误下发一次 start。
func TestSyncStateRestartingCountsAsRunning(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{Running: false, Restarting: true}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)
	m.enterRunning(time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if !m.SyncState(ctx) {
		t.Error("重启中的容器应视为在运行")
	}
	if !m.IsRunning() {
		t.Error("重启中不应被纠正为未运行")
	}
}

// TestSyncStateStopsLogWatcher 纠正状态时应一并停掉日志跟踪，
// 否则它会挂在一个已经消失的容器上白耗资源。
func TestSyncStateStopsLogWatcher(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{Running: false}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.enterRunning(time.Now())
	stream := fn.lastStream(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.SyncState(ctx)

	select {
	case <-stream.done:
	case <-time.After(time.Second):
		t.Error("纠正状态后日志流应被关闭")
	}
}
