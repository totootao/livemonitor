package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/dockerctl"
	"github.com/totootao/livemonitor/internal/logging"
	"github.com/totootao/livemonitor/internal/monitor"
	"github.com/totootao/livemonitor/internal/runtime"
	"github.com/totootao/livemonitor/internal/scheduler"
)

// fakeController 实现 Controller，用于在不启动真实 docker/ffmpeg 的情况下测试 Web 层。
type fakeController struct {
	store *runtime.Store
	sched *scheduler.Scheduler
	video VideoStatus

	mu        sync.Mutex
	monitors  map[string]*monitor.ContainerMonitor
	applyErr  error
	deleteErr error
	startErr  error
	stopErr   error
	runErr    error
	updateErr error
	reloadErr error
	reloaded  int
	startedAt time.Time

	lastApplied  config.ContainerConfig
	lastSettings *config.Config
}

func newFakeController(t *testing.T, containers ...config.ContainerConfig) *fakeController {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		WatchDir:          "/audio",
		HistoryDir:        "/audio/历史",
		CheckInterval:     30,
		StableDelay:       60,
		ArchiveAfterHours: 45,
		MP3Bitrate:        "32k",
		MonitorKeywords:   config.StringList{"等待直播"},
		Containers:        containers,
	}
	store := runtime.NewStore(cfg, dir+"/config.json", logging.New("test"))
	sched := scheduler.New(logging.New("test"))
	for _, cc := range containers {
		for _, ts := range cc.StartTimes {
			at, _ := config.ParseClock(ts)
			sched.Add(scheduler.Job{ID: cc.Name + "@" + ts, Name: cc.Name, At: at, Fn: func() {}})
		}
	}
	return &fakeController{
		store:     store,
		sched:     sched,
		video:     VideoStatus{WatchDir: "/audio", HistoryDir: "/audio/历史", Pending: 3},
		monitors:  make(map[string]*monitor.ContainerMonitor),
		startedAt: time.Now().Add(-2 * time.Minute),
	}
}

func (f *fakeController) Store() *runtime.Store           { return f.store }
func (f *fakeController) Scheduler() *scheduler.Scheduler { return f.sched }
func (f *fakeController) VideoStatus() VideoStatus        { return f.video }
func (f *fakeController) StartedAt() time.Time            { return f.startedAt }
func (f *fakeController) Monitors() []*monitor.ContainerMonitor {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*monitor.ContainerMonitor, 0, len(f.monitors))
	for _, m := range f.monitors {
		out = append(out, m)
	}
	return out
}
func (f *fakeController) MonitorByName(name string) (*monitor.ContainerMonitor, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.monitors[name]
	return m, ok
}
func (f *fakeController) ApplyContainer(cc config.ContainerConfig) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.mu.Lock()
	f.lastApplied = cc
	f.mu.Unlock()
	_, _, err := f.store.UpsertContainer(cc)
	return err
}
func (f *fakeController) DeleteContainer(name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.store.DeleteContainer(name)
}
func (f *fakeController) StartContainer(name string) error { return f.startErr }
func (f *fakeController) StopContainer(name string) error  { return f.stopErr }
func (f *fakeController) RunJobNow(name string) error      { return f.runErr }
func (f *fakeController) UpdateSettings(cfg *config.Config) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.mu.Lock()
	f.lastSettings = cfg
	f.mu.Unlock()
	return nil
}
func (f *fakeController) ReloadConfig() error {
	if f.reloadErr != nil {
		return f.reloadErr
	}
	f.mu.Lock()
	f.reloaded++
	f.mu.Unlock()
	return nil
}

func newTestServer(t *testing.T, ctrl *fakeController) *Server {
	t.Helper()
	return New(Options{
		Addr:    ":0",
		Store:   ctrl.store,
		Monitor: ctrl,
		Log:     logging.New("web"),
	})
}

// do 发起一次请求并返回响应。
func do(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func cc(name string, times ...string) config.ContainerConfig {
	return config.ContainerConfig{
		Name:           name,
		StartTimes:     config.StringList(times),
		MaxRunDuration: 7200,
	}
}

// TestIndexServesEmbeddedHTML 验证首页返回内嵌页面且禁用缓存。
func TestIndexServesEmbeddedHTML(t *testing.T) {
	s := newTestServer(t, newFakeController(t))
	rec := do(t, s, http.MethodGet, "/", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("首页应禁用缓存")
	}
	body := rec.Body.String()
	for _, want := range []string{"<!DOCTYPE html>", "livemonitor", "/api/state"} {
		if !strings.Contains(body, want) {
			t.Errorf("页面缺少 %q", want)
		}
	}
}

// TestUnknownPathReturns404 未知路径不应被首页通配吃掉。
func TestUnknownPathReturns404(t *testing.T) {
	s := newTestServer(t, newFakeController(t))
	rec := do(t, s, http.MethodGet, "/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d, 期望 404", rec.Code)
	}
}

// TestStateAggregatesContainersAndJobs 验证状态接口聚合配置、任务与设置。
func TestStateAggregatesContainersAndJobs(t *testing.T) {
	ctrl := newFakeController(t, cc("a", "10:00", "22:00"), cc("b", "11:00"))
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodGet, "/api/state", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	st := decodeJSON[StateView](t, rec)

	if len(st.Containers) != 2 {
		t.Fatalf("容器数 = %d, 期望 2", len(st.Containers))
	}
	if st.JobsTotal != 3 {
		t.Errorf("任务数 = %d, 期望 3", st.JobsTotal)
	}
	if st.Video.Pending != 3 {
		t.Errorf("待转码 %d, 期望 3", st.Video.Pending)
	}
	if st.Settings.MP3Bitrate != "32k" || st.Settings.WatchDir != "/audio" {
		t.Errorf("设置不符: %+v", st.Settings)
	}
	if st.Timezone == "" {
		t.Error("时区不应为空")
	}
	if st.UptimeSeconds < 60 {
		t.Errorf("运行时长 = %d, 应至少 60 秒", st.UptimeSeconds)
	}

	// 容器 a 有两个时刻，应聚合成 2 个任务，并算出最近的下次启动。
	var a *ContainerView
	for i := range st.Containers {
		if st.Containers[i].Name == "a" {
			a = &st.Containers[i]
		}
	}
	if a == nil {
		t.Fatal("未找到容器 a")
	}
	if len(a.StartTimes) != 2 {
		t.Errorf("a 的启动时刻 = %v, 期望 2 个", a.StartTimes)
	}
	if len(a.Jobs) != 2 {
		t.Errorf("a 的任务明细 = %d, 期望 2", len(a.Jobs))
	}
	if a.NextRun.IsZero() {
		t.Error("a 应有下次启动时刻")
	}
	if a.NextRun.Before(time.Now()) {
		t.Errorf("下次启动时刻 %s 不应早于当前时间", a.NextRun)
	}
}

// TestStateShowsMonitorWithoutConfig 配置被删但监控仍在时应暴露出来，便于发现不一致。
func TestStateShowsMonitorWithoutConfig(t *testing.T) {
	ctrl := newFakeController(t) // 无任何配置
	mon := monitor.New(cc("ghost", "10:00"), nil, noopRunner{}, logging.New("test"))
	ctrl.mu.Lock()
	ctrl.monitors["ghost"] = mon
	ctrl.mu.Unlock()

	s := newTestServer(t, ctrl)
	st := decodeJSON[StateView](t, do(t, s, http.MethodGet, "/api/state", nil))

	found := false
	for _, c := range st.Containers {
		if c.Name == "ghost" {
			found = true
		}
	}
	if !found {
		t.Error("无配置但存在的监控器应出现在状态里")
	}
}

// noopRunner 是满足 monitor.Runner 的空实现，仅用于构造监控器以便测试状态聚合。
type noopRunner struct{}

func (noopRunner) InspectState(ctx context.Context, container string) (dockerctl.ContainerState, error) {
	return dockerctl.ContainerState{}, nil
}
func (noopRunner) Start(ctx context.Context, container string) error                { return nil }
func (noopRunner) Stop(ctx context.Context, container string) error                 { return nil }
func (noopRunner) TruncateInternalLogs(ctx context.Context, container string) error { return nil }
func (noopRunner) RotateLogs(ctx context.Context, container string) error           { return nil }
func (noopRunner) LogsFollow(ctx context.Context, container string, since *time.Time) (dockerctl.StreamHandle, error) {
	return nil, fmt.Errorf("未实现")
}

// TestCreateContainer 验证新增容器接口。
func TestCreateContainer(t *testing.T) {
	ctrl := newFakeController(t)
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodPost, "/api/containers", map[string]any{
		"name":           "zhangsan",
		"startTimes":     []string{"20:01", "09:05"},
		"maxRunDuration": "3h",
		"keywords":       []string{"等待直播"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}

	ctrl.mu.Lock()
	applied := ctrl.lastApplied
	ctrl.mu.Unlock()
	if applied.Name != "zhangsan" {
		t.Errorf("容器名 = %s", applied.Name)
	}
	// "3h" 应被解析为 10800 秒。
	if applied.MaxRunDuration != 10800 {
		t.Errorf("最长运行 = %d, 期望 10800", applied.MaxRunDuration)
	}
	if len(applied.StartTimes) != 2 {
		t.Errorf("时刻 = %v, 期望 2 个", applied.StartTimes)
	}

	got, ok := ctrl.store.GetContainer("zhangsan")
	if !ok {
		t.Fatal("容器未写入存储")
	}
	if got.StartTimes[0] != "09:05" {
		t.Errorf("时刻应升序归一化，实际 %v", got.StartTimes)
	}
}

// TestCreateContainerNumericDuration 验证秒数形式的时长。
func TestCreateContainerNumericDuration(t *testing.T) {
	ctrl := newFakeController(t)
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodPost, "/api/containers", map[string]any{
		"name":           "a",
		"startTimes":     "10:00",
		"maxRunDuration": 5400,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.lastApplied.MaxRunDuration != 5400 {
		t.Errorf("最长运行 = %d, 期望 5400", ctrl.lastApplied.MaxRunDuration)
	}
}

// TestCreateContainerErrors 验证各类非法输入被拒绝且给出可读错误。
func TestCreateContainerErrors(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"缺少时刻", map[string]any{"name": "a"}, "至少需要"},
		{"非法时刻", map[string]any{"name": "a", "startTimes": []string{"25:00"}}, "非法"},
		{"空名称", map[string]any{"startTimes": []string{"10:00"}}, "容器名不能为空"},
		{"非法时长", map[string]any{"name": "a", "startTimes": []string{"10:00"}, "maxRunDuration": "3d"}, "非法"},
		{"时长类型错误", map[string]any{"name": "a", "startTimes": []string{"10:00"}, "maxRunDuration": true}, "格式不支持"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := newFakeController(t)
			s := newTestServer(t, ctrl)
			rec := do(t, s, http.MethodPost, "/api/containers", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400 (body=%s)", rec.Code, rec.Body.String())
			}
			resp := decodeJSON[map[string]string](t, rec)
			if !strings.Contains(resp["error"], tc.want) {
				t.Errorf("错误信息 = %q, 应包含 %q", resp["error"], tc.want)
			}
		})
	}
}

// TestUpdateContainerViaPut 验证按名称更新，且名称以路径为准。
func TestUpdateContainerViaPut(t *testing.T) {
	ctrl := newFakeController(t, cc("a", "10:00"))
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodPut, "/api/containers/a", map[string]any{
		"name":           "被忽略",
		"startTimes":     []string{"11:00", "12:00"},
		"maxRunDuration": "90m",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}

	got, _ := ctrl.store.GetContainer("a")
	if len(got.StartTimes) != 2 {
		t.Errorf("时刻 = %v, 期望 2 个", got.StartTimes)
	}
	if got.MaxRunDuration != 5400 {
		t.Errorf("最长运行 = %d, 期望 5400", got.MaxRunDuration)
	}
}

// TestDeleteContainer 验证删除接口。
func TestDeleteContainer(t *testing.T) {
	ctrl := newFakeController(t, cc("a", "10:00"))
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodDelete, "/api/containers/a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := ctrl.store.GetContainer("a"); ok {
		t.Error("容器应已删除")
	}

	// 再删一次应报错。
	rec = do(t, s, http.MethodDelete, "/api/containers/a", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("重复删除状态码 = %d, 期望 400", rec.Code)
	}
}

// TestContainerActions 验证启动/停止/立即执行三个动作转发正确。
func TestContainerActions(t *testing.T) {
	for _, action := range []string{"start", "stop", "run-now"} {
		t.Run(action, func(t *testing.T) {
			ctrl := newFakeController(t, cc("a", "10:00"))
			s := newTestServer(t, ctrl)

			rec := do(t, s, http.MethodPost, "/api/containers/a/"+action, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
			}
			resp := decodeJSON[map[string]string](t, rec)
			if resp["name"] != "a" || resp["status"] == "" {
				t.Errorf("响应不符: %v", resp)
			}
		})
	}
}

// TestContainerActionErrors 验证动作失败时把原因透传给前端。
func TestContainerActionErrors(t *testing.T) {
	ctrl := newFakeController(t, cc("a", "10:00"))
	ctrl.startErr = fmt.Errorf("容器 %q 已在运行中", "a")
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodPost, "/api/containers/a/start", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", rec.Code)
	}
	resp := decodeJSON[map[string]string](t, rec)
	if !strings.Contains(resp["error"], "已在运行中") {
		t.Errorf("错误信息 = %q", resp["error"])
	}
}

// TestUnknownAction 未知动作返回 404。
func TestUnknownAction(t *testing.T) {
	ctrl := newFakeController(t, cc("a", "10:00"))
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodPost, "/api/containers/a/explode", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("状态码 = %d, 期望 404", rec.Code)
	}
}

// TestContainerNameURLEncoding 验证容器名会被 URL 解码。
func TestContainerNameURLEncoding(t *testing.T) {
	ctrl := newFakeController(t, cc("中文 容器", "10:00"))
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodDelete, "/api/containers/%E4%B8%AD%E6%96%87%20%E5%AE%B9%E5%99%A8", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := ctrl.store.GetContainer("中文 容器"); ok {
		t.Error("带中文与空格的容器名应已被解码并删除")
	}
}

// TestContainerNameRejectsUnsafeNames 容器名中的路径分隔符必须被拒绝。
//
// 注意两条路径的差异：
//   - %2F（编码的斜杠）会被 ServeMux 的路径规范化提前处理，
//     请求根本到不了我们的 handler，而是被 301 重定向到规整路径；
//   - %5C（反斜杠）与 %2e%2e（".." 片段）能到达 handler，
//     因此必须由 decodeName 亲自拦下，否则会拼出意外的 API 路径。
func TestContainerNameRejectsUnsafeNames(t *testing.T) {
	ctrl := newFakeController(t)
	s := newTestServer(t, ctrl)

	// 反斜杠：必须由 decodeName 拦截。
	rec := do(t, s, http.MethodDelete, "/api/containers/a%5Cb", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("含反斜杠的名称状态码 = %d, 期望 400 (body=%s)", rec.Code, rec.Body.String())
	}

	// ".." 片段：必须由 decodeName 拦截（安全归一化不会消除它）。
	rec = do(t, s, http.MethodDelete, "/api/containers/a%2e%2e%2Fb", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("含 .. 片段的名称状态码 = %d, 期望 400 (body=%s)", rec.Code, rec.Body.String())
	}

	// 编码斜杠：由 ServeMux 提前规范化，不会到达 handler，仅断言未被当作正常请求处理。
	rec = do(t, s, http.MethodDelete, "/api/containers/a%2F..%2Fb", nil)
	if rec.Code == http.StatusOK {
		t.Errorf("编码斜杠不应被当作合法请求处理（状态码 %d）", rec.Code)
	}
}

// TestSettingsGetAndUpdate 验证全局设置读写。
func TestSettingsGetAndUpdate(t *testing.T) {
	ctrl := newFakeController(t, cc("a", "10:00"))
	s := newTestServer(t, ctrl)

	st := decodeJSON[SettingsView](t, do(t, s, http.MethodGet, "/api/settings", nil))
	if st.WatchDir != "/audio" {
		t.Errorf("监控目录 = %s", st.WatchDir)
	}

	rec := do(t, s, http.MethodPut, "/api/settings", map[string]any{
		"watchDir":          "/media",
		"historyDir":        "/media/old",
		"checkInterval":     45,
		"stableDelay":       90,
		"archiveAfterHours": 24,
		"mp3Bitrate":        "64k",
		"monitorKeywords":   []string{"等待直播", "已结束"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}

	ctrl.mu.Lock()
	applied := ctrl.lastSettings
	ctrl.mu.Unlock()
	if applied == nil {
		t.Fatal("设置未下发到控制器")
	}
	if applied.WatchDir != "/media" || applied.CheckInterval != 45 || applied.MP3Bitrate != "64k" {
		t.Errorf("设置不符: %+v", applied)
	}
	if len(applied.MonitorKeywords) != 2 {
		t.Errorf("关键词 = %v", applied.MonitorKeywords)
	}
}

// TestSettingsPartialUpdate 验证只传部分字段时其余设置保持原值。
func TestSettingsPartialUpdate(t *testing.T) {
	ctrl := newFakeController(t, cc("a", "10:00"))
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodPut, "/api/settings", map[string]any{"mp3Bitrate": "96k"})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}

	ctrl.mu.Lock()
	applied := ctrl.lastSettings
	ctrl.mu.Unlock()
	if applied.MP3Bitrate != "96k" {
		t.Errorf("码率 = %s", applied.MP3Bitrate)
	}
	if applied.WatchDir != "/audio" || applied.CheckInterval != 30 {
		t.Errorf("未传的字段应保持原值: %+v", applied)
	}
}

// TestSettingsRejectsInvalid 非法设置应被拒绝。
func TestSettingsRejectsInvalid(t *testing.T) {
	ctrl := newFakeController(t, cc("a", "10:00"))
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodPut, "/api/settings", map[string]any{"mp3Bitrate": "999k"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400 (body=%s)", rec.Code, rec.Body.String())
	}
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.lastSettings != nil {
		t.Error("非法设置不应下发")
	}
}

// TestReloadConfig 验证重载接口。
func TestReloadConfig(t *testing.T) {
	ctrl := newFakeController(t)
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodPost, "/api/reload", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	ctrl.mu.Lock()
	n := ctrl.reloaded
	ctrl.mu.Unlock()
	if n != 1 {
		t.Errorf("重载次数 = %d, 期望 1", n)
	}
}

// TestMethodNotAllowed 验证各接口对不支持的方法返回 405。
func TestMethodNotAllowed(t *testing.T) {
	ctrl := newFakeController(t, cc("a", "10:00"))
	s := newTestServer(t, ctrl)

	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/state"},
		{http.MethodDelete, "/api/containers"},
		{http.MethodPatch, "/api/settings"},
		{http.MethodGet, "/api/reload"},
		{http.MethodGet, "/api/containers/a/start"},
		{http.MethodGet, "/api/containers/a"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(t, s, tc.method, tc.path, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("状态码 = %d, 期望 405", rec.Code)
			}
		})
	}
}

// TestListContainers 验证 GET /api/containers 返回数组。
func TestListContainers(t *testing.T) {
	ctrl := newFakeController(t, cc("a", "10:00"), cc("b", "11:00"))
	s := newTestServer(t, ctrl)

	rec := do(t, s, http.MethodGet, "/api/containers", nil)
	list := decodeJSON[[]ContainerView](t, rec)
	if len(list) != 2 {
		t.Errorf("容器数 = %d, 期望 2", len(list))
	}
}

// TestOversizedBodyRejected 超长请求体应被拒绝而不是耗尽内存。
func TestOversizedBodyRejected(t *testing.T) {
	ctrl := newFakeController(t)
	s := newTestServer(t, ctrl)

	huge := strings.Repeat("x", 2<<20)
	rec := do(t, s, http.MethodPost, "/api/containers", map[string]any{
		"name":       huge,
		"startTimes": []string{"10:00"},
	})
	if rec.Code == http.StatusOK {
		t.Error("超长请求体不应被接受")
	}
}
