package dockerctl

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// fakeEngine 在临时 unix socket 上模拟 Docker Engine API。
type fakeEngine struct {
	t       *testing.T
	srv     *http.Server
	socket  string
	mu      sync.Mutex
	paths   []string
	methods []string
	// 按路径返回的响应；键为 method + " " + path。
	handlers map[string]handlerFunc
}

type handlerFunc func(w http.ResponseWriter, r *http.Request)

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	// socket 路径长度在 Linux 上有限制（108 字节），用短的临时目录。
	dir := t.TempDir()
	socket := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("监听 unix socket 失败: %v", err)
	}
	f := &fakeEngine{
		t:        t,
		socket:   socket,
		handlers: map[string]handlerFunc{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.methods = append(f.methods, r.Method)
		h := f.handlers[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if h == nil {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"message":"no handler for %s %s"}`, r.Method, r.URL.Path)
			return
		}
		h(w, r)
	})
	f.srv = &http.Server{Handler: mux}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() { _ = f.srv.Close() })
	return f
}

func (f *fakeEngine) on(method, path string, h handlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method+" "+path] = h
}

func (f *fakeEngine) json(method, path string, status int, v any) {
	f.on(method, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	})
}

func (f *fakeEngine) client() *Client {
	return NewWithSocket(f.socket, logging.New("error"))
}

// requests 返回记录到的请求路径。
func (f *fakeEngine) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// ---- 探活 ----

func TestAvailableOK(t *testing.T) {
	f := newFakeEngine(t)
	f.on(http.MethodGet, "/_ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	if err := f.client().Available(context.Background()); err != nil {
		t.Fatalf("Available 应成功，实际: %v", err)
	}
}

func TestAvailableSocketMissing(t *testing.T) {
	c := NewWithSocket(filepath.Join(t.TempDir(), "missing.sock"), logging.New("error"))
	err := c.Available(context.Background())
	if err == nil {
		t.Fatal("socket 不存在时应报错")
	}
	if !strings.Contains(err.Error(), "docker 不可达") {
		t.Errorf("错误信息应包含「docker 不可达」，实际: %v", err)
	}
}

func TestAvailableNonOKStatus(t *testing.T) {
	f := newFakeEngine(t)
	f.on(http.MethodGet, "/_ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := f.client().Available(context.Background())
	if err == nil {
		t.Fatal("非 200 时应报错")
	}
	if !strings.Contains(err.Error(), "探活失败") {
		t.Errorf("错误信息应包含「探活失败」，实际: %v", err)
	}
}

// ---- 状态查询 ----

func TestInspectRunningTrue(t *testing.T) {
	f := newFakeEngine(t)
	f.json(http.MethodGet, "/containers/zhangsan/json", http.StatusOK,
		map[string]any{"State": map[string]any{"Running": true}})
	running, err := f.client().InspectRunning(context.Background(), "zhangsan")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !running {
		t.Error("应返回运行中")
	}
}

func TestInspectRunningFalse(t *testing.T) {
	f := newFakeEngine(t)
	f.json(http.MethodGet, "/containers/zhangsan/json", http.StatusOK,
		map[string]any{"State": map[string]any{"Running": false}})
	running, err := f.client().InspectRunning(context.Background(), "zhangsan")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if running {
		t.Error("应返回未运行")
	}
}

func TestInspectRunningNotFound(t *testing.T) {
	f := newFakeEngine(t)
	f.json(http.MethodGet, "/containers/ghost/json", http.StatusNotFound,
		map[string]any{"message": "No such container: ghost"})
	_, err := f.client().InspectRunning(context.Background(), "ghost")
	if err == nil {
		t.Fatal("容器不存在时应报错")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误信息应包含「不存在」，实际: %v", err)
	}
}

func TestInspectRunningInvalidJSON(t *testing.T) {
	f := newFakeEngine(t)
	f.on(http.MethodGet, "/containers/bad/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	})
	_, err := f.client().InspectRunning(context.Background(), "bad")
	if err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if !strings.Contains(err.Error(), "解析") {
		t.Errorf("错误信息应包含「解析」，实际: %v", err)
	}
}

// ---- 启动 / 停止 ----

func TestStartSuccess(t *testing.T) {
	f := newFakeEngine(t)
	f.on(http.MethodPost, "/containers/zhangsan/start", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := f.client().Start(context.Background(), "zhangsan"); err != nil {
		t.Fatalf("启动应成功: %v", err)
	}
}

// 304 表示容器已在运行，属幂等成功。
func TestStartAlreadyRunningIsSuccess(t *testing.T) {
	f := newFakeEngine(t)
	f.on(http.MethodPost, "/containers/zhangsan/start", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	if err := f.client().Start(context.Background(), "zhangsan"); err != nil {
		t.Fatalf("已在运行时启动应视为成功: %v", err)
	}
}

func TestStartFailureIncludesMessage(t *testing.T) {
	f := newFakeEngine(t)
	f.json(http.MethodPost, "/containers/zhangsan/start", http.StatusInternalServerError,
		map[string]any{"message": "driver failed"})
	err := f.client().Start(context.Background(), "zhangsan")
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "driver failed") {
		t.Errorf("应带上 Engine 返回的消息，实际: %v", err)
	}
}

func TestStopSuccess(t *testing.T) {
	f := newFakeEngine(t)
	f.on(http.MethodPost, "/containers/zhangsan/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := f.client().Stop(context.Background(), "zhangsan"); err != nil {
		t.Fatalf("停止应成功: %v", err)
	}
}

// 304 表示容器已停止，属幂等成功。
func TestStopAlreadyStoppedIsSuccess(t *testing.T) {
	f := newFakeEngine(t)
	f.on(http.MethodPost, "/containers/zhangsan/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	if err := f.client().Stop(context.Background(), "zhangsan"); err != nil {
		t.Fatalf("已停止时停止应视为成功: %v", err)
	}
}

func TestStopFailure(t *testing.T) {
	f := newFakeEngine(t)
	f.json(http.MethodPost, "/containers/zhangsan/stop", http.StatusInternalServerError,
		map[string]any{"message": "boom"})
	if err := f.client().Stop(context.Background(), "zhangsan"); err == nil {
		t.Fatal("应报错")
	}
}

// 容器名含特殊字符时必须被正确转义。
func TestContainerNameIsEscaped(t *testing.T) {
	f := newFakeEngine(t)
	// 空格在路径中会被转义为 %20，服务端解码后路径仍是原样。
	f.json(http.MethodGet, "/containers/a b/json", http.StatusOK,
		map[string]any{"State": map[string]any{"Running": true}})
	if _, err := f.client().InspectRunning(context.Background(), "a b"); err != nil {
		t.Fatalf("带空格的名字应可用: %v", err)
	}
}

// ---- exec ----

func TestTruncateInternalLogs(t *testing.T) {
	f := newFakeEngine(t)
	var gotCmd []string
	f.on(http.MethodPost, "/containers/zhangsan/exec", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Cmd []string `json:"Cmd"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotCmd = payload.Cmd
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"exec123"}`))
	})
	f.on(http.MethodPost, "/exec/exec123/start", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	if err := f.client().TruncateInternalLogs(context.Background(), "zhangsan"); err != nil {
		t.Fatalf("清空日志应成功: %v", err)
	}
	if len(gotCmd) != 3 || gotCmd[0] != "sh" || gotCmd[1] != "-c" {
		t.Errorf("exec 命令形状不对: %v", gotCmd)
	}
	if !strings.Contains(gotCmd[2], "truncate -s 0") {
		t.Errorf("exec 命令内容不对: %v", gotCmd)
	}
}

func TestTruncateInternalLogsExecCreateFails(t *testing.T) {
	f := newFakeEngine(t)
	f.json(http.MethodPost, "/containers/zhangsan/exec", http.StatusInternalServerError,
		map[string]any{"message": "exec disabled"})
	err := f.client().TruncateInternalLogs(context.Background(), "zhangsan")
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "exec disabled") {
		t.Errorf("应带上 Engine 消息，实际: %v", err)
	}
}

func TestTruncateInternalLogsMissingExecID(t *testing.T) {
	f := newFakeEngine(t)
	f.json(http.MethodPost, "/containers/zhangsan/exec", http.StatusCreated, map[string]any{})
	err := f.client().TruncateInternalLogs(context.Background(), "zhangsan")
	if err == nil {
		t.Fatal("缺少 Id 时应报错")
	}
	if !strings.Contains(err.Error(), "Id") {
		t.Errorf("错误信息应提到 Id，实际: %v", err)
	}
}

// ---- 日志 ----

func TestRotateLogs(t *testing.T) {
	f := newFakeEngine(t)
	f.on(http.MethodGet, "/containers/zhangsan/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if err := f.client().RotateLogs(context.Background(), "zhangsan"); err != nil {
		t.Fatalf("应成功: %v", err)
	}
	found := false
	for _, p := range f.requested() {
		if p == "/containers/zhangsan/logs" {
			found = true
		}
	}
	if !found {
		t.Error("应请求 /containers/zhangsan/logs")
	}
}

// 日志流必须按 8 字节帧头切分，否则帧头会混进内容。
func TestLogsFollowParsesFrames(t *testing.T) {
	f := newFakeEngine(t)
	f.on(http.MethodGet, "/containers/zhangsan/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		writeFrame(w, "等待直播\n")
		writeFrame(w, "第二条\n第三条\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	h, err := f.client().LogsFollow(context.Background(), "zhangsan", nil)
	if err != nil {
		t.Fatalf("跟踪日志应成功: %v", err)
	}
	var got []string
	timeout := time.After(5 * time.Second)
	for len(got) < 3 {
		select {
		case line, ok := <-h.Lines():
			if !ok {
				t.Fatalf("通道提前关闭，已收到: %v", got)
			}
			got = append(got, line)
		case <-timeout:
			t.Fatalf("等待日志超时，已收到: %v", got)
		}
	}
	want := []string{"等待直播", "第二条", "第三条"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 行应为 %q，实际 %q", i, want[i], got[i])
		}
	}
	// 帧头不应出现在内容里。
	for _, l := range got {
		if strings.ContainsAny(l, "\x01\x02") {
			t.Errorf("日志内容混入了帧头字节: %q", l)
		}
	}
}

func TestLogsFollowNotFound(t *testing.T) {
	f := newFakeEngine(t)
	f.json(http.MethodGet, "/containers/ghost/logs", http.StatusNotFound,
		map[string]any{"message": "No such container: ghost"})
	_, err := f.client().LogsFollow(context.Background(), "ghost", nil)
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "No such container") {
		t.Errorf("应带上 Engine 消息，实际: %v", err)
	}
}

// since 参数应作为 Unix 秒传给 Engine。
func TestLogsFollowPassesSince(t *testing.T) {
	f := newFakeEngine(t)
	var gotQuery string
	f.on(http.MethodGet, "/containers/zhangsan/logs", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	})
	since := time.Unix(1700000000, 0)
	_, err := f.client().LogsFollow(context.Background(), "zhangsan", &since)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !strings.Contains(gotQuery, "since=1700000000") {
		t.Errorf("应带上 since，实际 query: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "follow=1") {
		t.Errorf("应带上 follow=1，实际 query: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "tail=0") {
		t.Errorf("应带上 tail=0，实际 query: %s", gotQuery)
	}
}

// 上下文取消后，日志通道应关闭。
func TestLogsFollowClosesOnCancel(t *testing.T) {
	f := newFakeEngine(t)
	f.on(http.MethodGet, "/containers/zhangsan/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// 保持连接，等待客户端取消。
		<-time.After(3 * time.Second)
	})

	ctx, cancel := context.WithCancel(context.Background())
	h, err := f.client().LogsFollow(ctx, "zhangsan", nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	cancel()
	// Kill 关闭响应体，pump 应随之退出并关闭通道。
	_ = h.Kill()

	select {
	case _, ok := <-h.Lines():
		if ok {
			// 可能读到空行，继续等关闭。
			for range h.Lines() {
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后日志通道应关闭")
	}
}

// ---- 辅助 ----

// writeFrame 按 Docker 日志流格式写入一帧。
func writeFrame(w io.Writer, payload string) {
	var header [8]byte
	header[0] = 1 // stdout
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	_, _ = w.Write(header[:])
	_, _ = w.Write([]byte(payload))
}

// ---- socket 解析 ----

func TestResolveSocketDefault(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	if got := resolveSocket(); got != defaultSocket {
		t.Errorf("应为默认路径，实际: %s", got)
	}
}

func TestResolveSocketUnix(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///run/docker.sock")
	if got := resolveSocket(); got != "/run/docker.sock" {
		t.Errorf("应解析出 /run/docker.sock，实际: %s", got)
	}
}

// 非 unix 形式不支持，退回默认路径而不是构造出非法地址。
func TestResolveSocketNonUnix(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	if got := resolveSocket(); got != defaultSocket {
		t.Errorf("非 unix 形式应退回默认路径，实际: %s", got)
	}
}

func TestResolveSocketBareUnixPrefix(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix://")
	if got := resolveSocket(); got != defaultSocket {
		t.Errorf("空路径应退回默认，实际: %s", got)
	}
}

// 编译期断言：Client 必须满足 monitor.Runner 所需的全部方法签名。
var _ interface {
	InspectRunning(context.Context, string) (bool, error)
	Start(context.Context, string) error
	Stop(context.Context, string) error
	TruncateInternalLogs(context.Context, string) error
	RotateLogs(context.Context, string) error
	LogsFollow(context.Context, string, *time.Time) (StreamHandle, error)
} = (*Client)(nil)

// 编译期断言：httpStream 实现 StreamHandle。
var _ StreamHandle = (*httpStream)(nil)

// 确保 httptest 被引用（保留给将来可能的 HTTP 层测试）。
var _ = httptest.NewRequest
