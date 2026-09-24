package monitor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/dockerctl"
)

// 启动回溯检查的专项用例。
//
// 背景：watchLogs 挂上日志流之后发生的输出由实时监控负责，
// 但如果 LogsFollow 建流失败，实时监控就成了摆设——而容器内应用
// 往往一启动就打印"等待直播"（实测约在启动后 5 秒）。
// 启动回溯检查是兜底：启动后 30 秒内回查最近一分钟的日志，命中即停。

// TestStartupSweepStopsContainer 实时监控不可用时，回查必须兜住：
// 日志里已有"等待直播"的容器最迟也要在启动后 30 秒内被停掉。
func TestStartupSweepStopsContainer(t *testing.T) {
	fn := newFakeRunner()
	// 关键前置：日志流建立失败，实时监控不存在，回查是唯一的网。
	fn.followErr = errors.New("simulated stream failure")
	fn.logsRangeFn = func(since time.Time) ([]string, error) {
		return []string{
			"[INFO] 服务启动完成",
			"[INFO] \x07主播正在准备，等待直播中\x1b[0m",
		}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	stoppedCh := make(chan string, 1)
	m.SetHooks(nil, func(reason string) { stoppedCh <- reason })

	m.Start()
	m.waitRunning(t)

	select {
	case reason := <-stoppedCh:
		if !strings.Contains(reason, "启动回溯检查") {
			t.Errorf("停止原因应标明来自启动回溯检查，实际: %s", reason)
		}
		if !strings.Contains(reason, "等待直播") {
			t.Errorf("停止原因应包含关键词，实际: %s", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("实时监控不可用时，启动回溯检查应停止容器，但超时未停")
	}

	_, stopped, _, _ := fn.counts()
	if stopped != 1 {
		t.Errorf("stop 调用次数 = %d, 期望 1", stopped)
	}
}

// TestStartupSweepOnAdoptedContainer 接管的容器同样要回查。
// 且窗口起点必须是"最近一分钟"，不能回溯到接管时刻之前——
// 否则会读到上一轮运行留下的关键词，把刚接管的容器误杀。
func TestStartupSweepOnAdoptedContainer(t *testing.T) {
	fn := newFakeRunner()
	startedAt := time.Now().Add(-10 * time.Minute)
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		return dockerctl.ContainerState{Running: true, StartedAt: startedAt}, nil
	}

	fn.logsRangeFn = func(since time.Time) ([]string, error) {
		return []string{"[INFO] 等待直播"}, nil
	}

	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)
	stoppedCh := make(chan string, 1)
	m.SetHooks(nil, func(reason string) { stoppedCh <- reason })

	m.Start()
	select {
	case <-stoppedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("接管容器的启动回溯检查应命中关键词并停止，但超时未停")
	}

	// 容器 10 分钟前启动，回查窗口 1 分钟：起点应是约 1 分钟前，
	// 而不是 10 分钟前（后者会读到上一轮残留日志）。
	gotSince := fn.lastLogsRangeSince()
	if gotSince.IsZero() {
		t.Fatal("未观察到回查调用")
	}
	age := time.Since(gotSince)
	if age < 50*time.Second || age > 70*time.Second {
		t.Errorf("回查窗口起点应为约 1 分钟前，实际距今 %s", age)
	}
}

// TestStartupSweepWindowCoversFreshStart 刚启动的容器，窗口起点就是启动时刻：
// 回查从启动到现在的全部日志，一分钟上限此时不起约束作用。
func TestStartupSweepWindowCoversFreshStart(t *testing.T) {
	fn := newFakeRunner()
	fn.logsRangeFn = func(time.Time) ([]string, error) {
		return nil, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	started := make(chan time.Time, 1)
	m.SetHooks(func(at time.Time) { started <- at }, nil)

	m.Start()
	m.waitRunning(t)
	time.Sleep(200 * time.Millisecond)

	var at time.Time
	select {
	case at = <-started:
	default:
		t.Fatal("应触发 onStarted")
	}
	gotSince := fn.lastLogsRangeSince()
	if gotSince.IsZero() {
		t.Fatal("未观察到回查调用")
	}
	if gotSince.Before(at.Add(-2*time.Second)) || gotSince.After(at.Add(2*time.Second)) {
		t.Errorf("刚启动的容器回查起点应约等于启动时刻 %s，实际 %s", at, gotSince)
	}
}

// TestStartupSweepNoMatchKeepsRunning 日志干净时容器应继续运行。
func TestStartupSweepNoMatchKeepsRunning(t *testing.T) {
	fn := newFakeRunner()
	fn.logsRangeFn = func(time.Time) ([]string, error) {
		return []string{"[INFO] 正在推流", "[INFO] 观众 +1"}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	m.waitRunning(t)
	time.Sleep(300 * time.Millisecond)

	if !m.IsRunning() {
		t.Error("回查未命中时容器不应停止")
	}
	_, stopped, _, _ := fn.counts()
	if stopped != 0 {
		t.Errorf("stop 调用次数 = %d, 期望 0", stopped)
	}
	m.Stop(ReasonShutdown)
}

// TestStartupSweepFailureIsTolerated 回查失败不应影响容器运行与实时监控。
func TestStartupSweepFailureIsTolerated(t *testing.T) {
	fn := newFakeRunner()
	fn.logsRangeFn = func(time.Time) ([]string, error) {
		return nil, errors.New("simulated docker error")
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)

	m.Start()
	m.waitRunning(t)
	time.Sleep(300 * time.Millisecond)

	if !m.IsRunning() {
		t.Error("回查失败时容器不应停止")
	}
	_, stopped, _, _ := fn.counts()
	if stopped != 0 {
		t.Errorf("stop 调用次数 = %d, 期望 0", stopped)
	}
	m.Stop(ReasonShutdown)
}

// TestStartupSweepSkippedAfterStop 容器在回查执行前就被停掉时，
// 挂起的检查应随上下文取消作废，而不是对着一个已停止的容器白查一遍。
func TestStartupSweepSkippedAfterStop(t *testing.T) {
	fn := newFakeRunner()
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)
	m.sweepDelay = 200 * time.Millisecond

	m.Start()
	m.waitRunning(t)
	m.Stop(ReasonShutdown)
	time.Sleep(400 * time.Millisecond)

	fn.mu.Lock()
	calls := fn.logsRangeCalls
	fn.mu.Unlock()
	if calls != 0 {
		t.Errorf("容器已停止后不应再执行回查，实际调用了 %d 次", calls)
	}
}

// TestStartupSweepIgnoresStaleGeneration 回查执行期间容器被重新拉起时，
// 旧一代的检查必须作废——否则它会用上一轮的关键词命中去停新一轮的容器。
func TestStartupSweepIgnoresStaleGeneration(t *testing.T) {
	fn := newFakeRunner()
	fn.inspectStateFn = func() (dockerctl.ContainerState, error) {
		// 必须返回"未运行"，Start 才会走真正的启动分支。
		return dockerctl.ContainerState{}, nil
	}
	fn.logsRangeFn = func(time.Time) ([]string, error) {
		return []string{"[INFO] 等待直播"}, nil
	}
	m := newTestMonitor(t, fn, []string{"等待直播"}, time.Hour)
	m.sweepDelay = 100 * time.Millisecond

	stoppedCh := make(chan string, 8)
	m.SetHooks(nil, func(reason string) { stoppedCh <- reason })

	m.Start()
	m.waitRunning(t)
	// 在第一轮回查触发前（100ms 内）完成停止 + 重启，制造代次切换。
	// 注意：这次手动停止也会触发 onStopped（"程序退出"），
	// 断言时要把测试自己造成的停止与回查造成的停止区分开。
	m.Stop(ReasonShutdown)
	m.Start()
	m.waitRunning(t)

	// 等待第一刀出现。
	sweepStops := 0
	deadline := time.After(3 * time.Second)
collect:
	for {
		select {
		case reason := <-stoppedCh:
			if strings.Contains(reason, "启动回溯检查") {
				sweepStops++
				break collect
			}
			// 其它原因（如测试自身的"程序退出"）不算。
		case <-deadline:
			t.Fatal("新一代容器的回查应命中关键词并停止，但超时未停")
		}
	}
	// 再等一小段，确认没有第二刀（旧一代抢停）。
	time.Sleep(300 * time.Millisecond)
	for {
		select {
		case reason := <-stoppedCh:
			if strings.Contains(reason, "启动回溯检查") {
				sweepStops++
				continue
			}
			continue
		default:
		}
		break
	}
	if sweepStops != 1 {
		t.Errorf("应恰好由回查停止一次（旧代次的回查必须作废），实际 %d 次", sweepStops)
	}
	// 共 2 次 docker stop：测试重启流程中的手动停止 + 回查触发的停止。
	_, stopped, _, _ := fn.counts()
	if stopped != 2 {
		t.Errorf("stop 调用次数 = %d, 期望 2（手动 1 次 + 回查 1 次）", stopped)
	}
}

// TestStartupSweepOnlyMatchesConfiguredKeyword 回查与实时监控使用同一份关键词：
// 配置的关键词是"开播"时，不含该词的日志不应触发停止。
// （注意文案里不能出现"开播"二字——否则就成了误报夹具。）
func TestStartupSweepOnlyMatchesConfiguredKeyword(t *testing.T) {
	fn := newFakeRunner()
	fn.followErr = errors.New("simulated stream failure")
	fn.logsRangeFn = func(time.Time) ([]string, error) {
		return []string{"[INFO] 观众 +1，推流正常"}, nil
	}
	m := newTestMonitor(t, fn, []string{"开播"}, time.Hour)

	m.Start()
	m.waitRunning(t)
	time.Sleep(300 * time.Millisecond)

	if !m.IsRunning() {
		t.Error("关键词不匹配时回查不应停止容器")
	}
	_, stopped, _, _ := fn.counts()
	if stopped != 0 {
		t.Errorf("stop 调用次数 = %d, 期望 0", stopped)
	}
	m.Stop(ReasonShutdown)
}
