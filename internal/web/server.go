// Package web 提供定时任务与容器监控的 Web 管理界面。
//
// 设计取舍：
//   - 不引入任何第三方 Web 框架，仅用标准库 net/http；
//   - 页面通过 fetch 调用 JSON API 局部刷新，避免整页重载；
//   - 静态资源用 go:embed 内嵌，保证单文件二进制可直接运行。
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/logging"
	"github.com/totootao/livemonitor/internal/monitor"
	"github.com/totootao/livemonitor/internal/runtime"
	"github.com/totootao/livemonitor/internal/scheduler"
)

//go:embed assets/index.html
var assetsFS embed.FS

// indexHTML 是内嵌的管理页面。
var indexHTML = mustReadAsset("assets/index.html")

func mustReadAsset(name string) []byte {
	data, err := fs.ReadFile(assetsFS, name)
	if err != nil {
		// 内嵌资源缺失属于构建期错误，直接 panic 比运行时静默 404 更容易定位。
		panic(fmt.Sprintf("内嵌资源 %s 读取失败: %v", name, err))
	}
	return data
}

// Controller 是 Web 层需要的管理器能力。由 manager.Manager 实现。
// 定义成接口便于在测试中注入假实现。
type Controller interface {
	Store() *runtime.Store
	Scheduler() *scheduler.Scheduler
	// VideoStatus 返回媒体处理器状态快照。
	VideoStatus() VideoStatus
	StartedAt() time.Time

	Monitors() []*monitor.ContainerMonitor
	MonitorByName(name string) (*monitor.ContainerMonitor, bool)

	ApplyContainer(cc config.ContainerConfig) error
	DeleteContainer(name string) error
	StartContainer(name string) error
	StopContainer(name string) error
	RunJobNow(name string) error
	UpdateSettings(cfg *config.Config) error
	ReloadConfig() error
}

// VideoStatus 是媒体处理器的状态快照。
type VideoStatus struct {
	WatchDir   string `json:"watchDir"`
	HistoryDir string `json:"historyDir"`
	// Pending 是待转码队列长度。
	Pending int `json:"pending"`
}

// Options 是 Server 的构造参数。
type Options struct {
	Addr    string
	Store   *runtime.Store
	Monitor Controller
	Log     *logging.Logger
}

// Server 是 Web 管理服务。
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// New 创建 Web 服务。
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler 返回可直接交给 http.Server 的处理器。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/containers", s.handleContainers)
	s.mux.HandleFunc("/api/containers/", s.handleContainerByName)
	s.mux.HandleFunc("/api/settings", s.handleSettings)
	s.mux.HandleFunc("/api/reload", s.handleReload)
}

// ---- 页面 ----

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(indexHTML); err != nil {
		s.opts.Log.Debug("写出首页失败: %v", err)
	}
}

// ---- JSON API ----

// writeJSON 输出 JSON 响应。
func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.opts.Log.Debug("写出 JSON 响应失败: %v", err)
	}
}

// writeErr 输出错误响应。
func (s *Server) writeErr(w http.ResponseWriter, code int, format string, args ...any) {
	s.writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// ContainerView 是容器的展示模型。
type ContainerView struct {
	Name string `json:"name"`
	// StartTimes 是该容器全部每日启动时刻（HH:MM）。
	StartTimes       []string  `json:"startTimes"`
	MaxRunDuration   int       `json:"maxRunDuration"`
	Keywords         []string  `json:"keywords"`
	Running          bool      `json:"running"`
	StartedAt        time.Time `json:"startedAt"`
	ElapsedSeconds   int       `json:"elapsedSeconds"`
	RemainingSeconds int       `json:"remainingSeconds"`
	// NextRun 是该容器最近一次计划启动时刻。
	NextRun time.Time `json:"nextRun"`
	// Jobs 是该容器对应的调度任务明细。
	Jobs []scheduler.JobInfo `json:"jobs"`
}

// StateView 是 /api/state 的响应。
type StateView struct {
	ServerTime    time.Time       `json:"serverTime"`
	Timezone      string          `json:"timezone"`
	StartedAt     time.Time       `json:"startedAt"`
	UptimeSeconds int             `json:"uptimeSeconds"`
	JobsTotal     int             `json:"jobsTotal"`
	Containers    []ContainerView `json:"containers"`
	Video         VideoStatus     `json:"video"`
	Settings      SettingsView    `json:"settings"`
}

// SettingsView 是全局设置的展示模型。
type SettingsView struct {
	WatchDir          string   `json:"watchDir"`
	HistoryDir        string   `json:"historyDir"`
	CheckInterval     int      `json:"checkInterval"`
	StableDelay       int      `json:"stableDelay"`
	ArchiveAfterHours int      `json:"archiveAfterHours"`
	MP3Bitrate        string   `json:"mp3Bitrate"`
	MonitorKeywords   []string `json:"monitorKeywords"`
	ConfigPath        string   `json:"configPath"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	s.writeJSON(w, http.StatusOK, s.buildState())
}

// buildState 汇总当前运行状态。
func (s *Server) buildState() StateView {
	now := time.Now()
	snap := s.opts.Store.Snapshot()

	// 按容器名归集调度任务，避免在循环里反复遍历全部任务。
	jobsByContainer := make(map[string][]scheduler.JobInfo)
	for _, j := range s.opts.Monitor.Scheduler().Snapshot(now) {
		jobsByContainer[j.Name] = append(jobsByContainer[j.Name], j)
	}

	views := make([]ContainerView, 0, len(snap.Containers))
	seen := make(map[string]bool, len(snap.Containers))
	for _, cc := range snap.Containers {
		seen[cc.Name] = true
		views = append(views, s.buildContainerView(cc, jobsByContainer[cc.Name]))
	}
	// 监控器存在但配置已被删除的情况也要展示，便于发现不一致。
	for _, mon := range s.opts.Monitor.Monitors() {
		if seen[mon.Name()] {
			continue
		}
		views = append(views, s.buildContainerView(config.ContainerConfig{Name: mon.Name()}, nil))
	}

	return StateView{
		ServerTime:    now,
		Timezone:      time.Local.String(),
		StartedAt:     s.opts.Monitor.StartedAt(),
		UptimeSeconds: int(now.Sub(s.opts.Monitor.StartedAt()).Seconds()),
		JobsTotal:     s.opts.Monitor.Scheduler().Jobs(),
		Containers:    views,
		Video:         s.opts.Monitor.VideoStatus(),
		Settings: SettingsView{
			WatchDir:          snap.WatchDir,
			HistoryDir:        snap.HistoryDir,
			CheckInterval:     snap.CheckInterval,
			StableDelay:       snap.StableDelay,
			ArchiveAfterHours: snap.ArchiveAfterHours,
			MP3Bitrate:        snap.MP3Bitrate,
			MonitorKeywords:   []string(snap.MonitorKeywords),
			ConfigPath:        s.opts.Store.Path(),
		},
	}
}

// buildContainerView 组装单个容器的展示数据。
func (s *Server) buildContainerView(cc config.ContainerConfig, jobs []scheduler.JobInfo) ContainerView {
	v := ContainerView{
		Name:           cc.Name,
		StartTimes:     []string(cc.StartTimes),
		MaxRunDuration: cc.MaxRunDuration,
		Keywords:       []string(cc.Keywords),
		Jobs:           jobs,
	}
	if v.StartTimes == nil {
		v.StartTimes = []string{}
	}
	if v.Jobs == nil {
		v.Jobs = []scheduler.JobInfo{}
	}

	// 最近一次计划启动时刻。
	for _, j := range jobs {
		if v.NextRun.IsZero() || j.NextRun.Before(v.NextRun) {
			v.NextRun = j.NextRun
		}
	}

	if mon, ok := s.opts.Monitor.MonitorByName(cc.Name); ok {
		st := mon.Status()
		v.Running = st.Running
		v.StartedAt = st.StartedAt
		v.ElapsedSeconds = st.ElapsedSeconds
		v.RemainingSeconds = st.RemainingSeconds
		if len(st.Keywords) > 0 {
			v.Keywords = st.Keywords
		}
		if st.MaxDuration > 0 {
			v.MaxRunDuration = st.MaxDuration
		}
	}
	return v
}

// containerPayload 是新增/修改容器的请求体。
type containerPayload struct {
	Name string `json:"name"`
	// StartTimes 支持字符串或字符串数组，与前端的多输入框提交形式对应。
	StartTimes config.StringList `json:"startTimes"`
	// MaxRunDuration 支持秒数或 "3h"/"90m" 写法。
	MaxRunDuration any               `json:"maxRunDuration"`
	Keywords       config.StringList `json:"keywords"`
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeJSON(w, http.StatusOK, s.buildState().Containers)
	case http.MethodPost:
		s.upsertContainer(w, r)
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET / POST")
	}
}

// handleContainerByName 处理 /api/containers/{name} 与 /api/containers/{name}/{action}。
func (s *Server) handleContainerByName(w http.ResponseWriter, r *http.Request) {
	// 用 EscapedPath 而非 URL.Path 来切分路径段。
	//
	// 原因：URL.Path 已被解码，容器名里的 "%2F"（编码斜杠）会变成真实的 "/"，
	// 于是 "a%2Fb" 会被误切成两段、当成"容器 a + 操作 b"。
	// EscapedPath 保留原始百分号编码，按 "/" 切分得到的才是真正的路径段。
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/api/containers/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		s.writeErr(w, http.StatusBadRequest, "缺少容器名")
		return
	}
	name, err := decodeName(parts[0])
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "容器名非法: %v", err)
		return
	}
	if len(parts) > 2 {
		s.writeErr(w, http.StatusBadRequest, "路径层级过多")
		return
	}

	// /api/containers/{name}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPut:
			s.upsertContainer(w, r, name)
		case http.MethodDelete:
			if err := s.opts.Monitor.DeleteContainer(name); err != nil {
				s.writeErr(w, http.StatusBadRequest, "%v", err)
				return
			}
			s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})
		default:
			s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 PUT / DELETE")
		}
		return
	}

	if r.Method != http.MethodPost {
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	switch parts[1] {
	case "start":
		if err := s.opts.Monitor.StartContainer(name); err != nil {
			s.writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "starting", "name": name})
	case "stop":
		if err := s.opts.Monitor.StopContainer(name); err != nil {
			s.writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "stopping", "name": name})
	case "run-now":
		if err := s.opts.Monitor.RunJobNow(name); err != nil {
			s.writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "triggered", "name": name})
	default:
		s.writeErr(w, http.StatusNotFound, "未知操作: %s", parts[1])
	}
}

// upsertContainer 解析请求体并写入配置。
func (s *Server) upsertContainer(w http.ResponseWriter, r *http.Request, nameOverride ...string) {
	var p containerPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		s.writeErr(w, http.StatusBadRequest, "请求体解析失败: %v", err)
		return
	}
	if len(nameOverride) > 0 && nameOverride[0] != "" {
		p.Name = nameOverride[0]
	}

	maxRun := config.DefaultMaxRunDuration
	if p.MaxRunDuration != nil {
		switch v := p.MaxRunDuration.(type) {
		case float64:
			maxRun = int(v)
		case string:
			sec, err := runtime.ParseDurationInput(v)
			if err != nil {
				s.writeErr(w, http.StatusBadRequest, "最大运行时长非法: %v", err)
				return
			}
			maxRun = sec
		default:
			s.writeErr(w, http.StatusBadRequest, "最大运行时长格式不支持")
			return
		}
	}
	if maxRun <= 0 {
		maxRun = config.DefaultMaxRunDuration
	}

	cc := config.ContainerConfig{
		Name:           p.Name,
		StartTimes:     p.StartTimes,
		MaxRunDuration: maxRun,
		Keywords:       p.Keywords,
	}
	// 新增容器时，若只给了名字，默认补一个每日时刻以免任务为空。
	if len(cc.StartTimes) == 0 {
		s.writeErr(w, http.StatusBadRequest, "至少需要一个启动时刻（HH:MM）")
		return
	}
	if err := s.opts.Monitor.ApplyContainer(cc); err != nil {
		s.writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "name": cc.Name})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeJSON(w, http.StatusOK, s.buildState().Settings)
	case http.MethodPut:
		var payload struct {
			WatchDir          *string           `json:"watchDir"`
			HistoryDir        *string           `json:"historyDir"`
			CheckInterval     *int              `json:"checkInterval"`
			StableDelay       *int              `json:"stableDelay"`
			ArchiveAfterHours *int              `json:"archiveAfterHours"`
			MP3Bitrate        *string           `json:"mp3Bitrate"`
			MonitorKeywords   config.StringList `json:"monitorKeywords"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil {
			s.writeErr(w, http.StatusBadRequest, "请求体解析失败: %v", err)
			return
		}

		cur := s.opts.Store.Snapshot()
		if payload.WatchDir != nil {
			cur.WatchDir = strings.TrimSpace(*payload.WatchDir)
		}
		if payload.HistoryDir != nil {
			cur.HistoryDir = strings.TrimSpace(*payload.HistoryDir)
		}
		if payload.CheckInterval != nil {
			cur.CheckInterval = *payload.CheckInterval
		}
		if payload.StableDelay != nil {
			cur.StableDelay = *payload.StableDelay
		}
		if payload.ArchiveAfterHours != nil {
			cur.ArchiveAfterHours = *payload.ArchiveAfterHours
		}
		if payload.MP3Bitrate != nil {
			cur.MP3Bitrate = strings.TrimSpace(*payload.MP3Bitrate)
		}
		if len(payload.MonitorKeywords) > 0 {
			cur.MonitorKeywords = payload.MonitorKeywords
		}

		if err := cur.Validate(); err != nil {
			s.writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		if err := s.opts.Monitor.UpdateSettings(cur); err != nil {
			s.writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET / PUT")
	}
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	if err := s.opts.Monitor.ReloadConfig(); err != nil {
		s.writeErr(w, http.StatusBadRequest, "重载失败: %v", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
}

// decodeName 解码 URL 路径中的容器名。
func decodeName(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("空名称")
	}
	// 容器名本身不含 % 与 /，这里只需处理前端 encodeURIComponent 的结果。
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", err
	}
	decoded = strings.TrimSpace(decoded)
	if decoded == "" {
		return "", fmt.Errorf("空名称")
	}
	if strings.ContainsAny(decoded, "/\\") {
		return "", fmt.Errorf("名称不能包含 / 或 \\")
	}
	// 拒绝 "." 与 ".." 这类路径片段：它们会让下层的路径拼接产生意外语义。
	if decoded == "." || decoded == ".." {
		return "", fmt.Errorf("名称不能是 %q", decoded)
	}
	return decoded, nil
}
