// Package dockerctl 通过 Docker Engine API 控制宿主机上的容器。
//
// 实现方式：直接对 /var/run/docker.sock 发 HTTP 请求，不依赖 docker CLI。
// 好处是容器镜像里不必再装 docker-cli（约 31MB），且行为更可控——
// 不必解析命令行输出，也不受 CLI 版本差异影响。
//
// 仅依赖标准库。涉及的 Engine API 端点：
//
//	GET  /_ping                                  探活
//	GET  /containers/{id}/json                   查询状态
//	POST /containers/{id}/start                  启动
//	POST /containers/{id}/stop                   停止
//	POST /containers/{id}/exec                   创建 exec 实例
//	POST /exec/{id}/start                        运行 exec 实例
//	GET  /containers/{id}/logs?follow=1&tail=0   流式日志
package dockerctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// ErrNotRunning 表示容器当前不处于运行状态。
var ErrNotRunning = errors.New("容器未在运行")

// 默认 Docker socket 路径。可通过 DOCKER_HOST 覆盖（仅支持 unix:// 形式）。
const defaultSocket = "/var/run/docker.sock"

// StreamHandle 表示一个正在运行的日志流。
type StreamHandle interface {
	// Lines 返回日志行通道，流结束或上下文取消后通道关闭。
	Lines() <-chan string
	// Wait 等待流结束并返回其错误。
	Wait() error
	// Kill 强制中断流。
	Kill() error
}

// Client 是面向 Docker Engine API 的客户端。
type Client struct {
	http   *http.Client
	log    *logging.Logger
	socket string
}

// New 创建客户端，使用默认 Docker socket。
func New(log *logging.Logger) *Client {
	return NewWithSocket(resolveSocket(), log)
}

// NewWithSocket 用指定 socket 路径创建客户端，主要用于测试。
func NewWithSocket(socket string, log *logging.Logger) *Client {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		// 容器操作可能持续较久（stop 默认等 10s 优雅退出），不设全局超时，
		// 由调用方通过 context 控制。
		DisableCompression: true,
	}
	return &Client{
		http:   &http.Client{Transport: transport},
		log:    log,
		socket: socket,
	}
}

// resolveSocket 解析 DOCKER_HOST 环境变量，得到要连接的 unix socket 路径。
// 非 unix:// 形式（如 tcp://）不支持，退回默认路径。
func resolveSocket() string {
	host := strings.TrimSpace(envOr("DOCKER_HOST", ""))
	if host == "" {
		return defaultSocket
	}
	const prefix = "unix://"
	if strings.HasPrefix(host, prefix) {
		p := strings.TrimPrefix(host, prefix)
		if p != "" {
			return p
		}
	}
	return defaultSocket
}

// Available 检查 Engine API 是否可达。
func (c *Client) Available(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, status, err := c.do(rctx, http.MethodGet, "/_ping", nil)
	if err != nil {
		return fmt.Errorf("docker 不可达（socket %s）: %w", c.socket, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("docker 探活失败: HTTP %d %s", status, strings.TrimSpace(string(body)))
	}
	return nil
}

// ContainerState 是容器运行时状态的快照。
type ContainerState struct {
	// Running 表示容器当前是否在运行。
	Running bool
	// StartedAt 是本次运行的启动时刻（容器未运行时为零值）。
	// 用于接管一个"已经在跑"的容器时推断它已运行了多久。
	StartedAt time.Time
	// Restarting 表示容器正处于重启过程中。
	Restarting bool
}

// InspectState 查询容器的完整运行时状态。
//
// 比 InspectRunning 多返回 StartedAt：接管一个本程序启动前就已在运行的容器时，
// 需要用它推算已运行时长，否则最长运行时长会从接管时刻重新起算，
// 让一个早就该停的容器继续跑下去。
func (c *Client) InspectState(ctx context.Context, container string) (ContainerState, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(container)+"/json", nil)
	if err != nil {
		return ContainerState{}, err
	}
	if status == http.StatusNotFound {
		return ContainerState{}, fmt.Errorf("容器 %s 不存在", container)
	}
	if status != http.StatusOK {
		return ContainerState{}, fmt.Errorf("查询容器 %s 状态失败: HTTP %d %s", container, status, truncate(body))
	}
	var info struct {
		State struct {
			Running    bool   `json:"Running"`
			Restarting bool   `json:"Restarting"`
			StartedAt  string `json:"StartedAt"`
		} `json:"State"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return ContainerState{}, fmt.Errorf("解析容器 %s 状态失败: %w", container, err)
	}

	st := ContainerState{
		Running:    info.State.Running,
		Restarting: info.State.Restarting,
	}
	// Docker 用 RFC3339Nano 输出，且从未启动过的容器会给零值时间
	// "0001-01-01T00:00:00Z"。解析失败不算致命，留零值即可。
	if info.State.StartedAt != "" {
		if t, perr := time.Parse(time.RFC3339Nano, info.State.StartedAt); perr == nil {
			st.StartedAt = t
		} else {
			c.log.Debug("解析容器 %s 的 StartedAt 失败: %v", container, perr)
		}
	}
	return st, nil
}

// InspectRunning 判断容器是否处于 running 状态。
func (c *Client) InspectRunning(ctx context.Context, container string) (bool, error) {
	st, err := c.InspectState(ctx, container)
	if err != nil {
		return false, err
	}
	return st.Running, nil
}

// Start 启动容器。
func (c *Client) Start(ctx context.Context, container string) error {
	body, status, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(container)+"/start", nil)
	if err != nil {
		return fmt.Errorf("启动容器 %s 失败: %w", container, err)
	}
	// 304 表示容器已在运行，按幂等语义视为成功。
	if status == http.StatusNoContent || status == http.StatusNotModified {
		return nil
	}
	return fmt.Errorf("启动容器 %s 失败: HTTP %d %s", container, status, apiMessage(body))
}

// Stop 停止容器。使用 Engine 默认的超时（10 秒优雅退出）。
func (c *Client) Stop(ctx context.Context, container string) error {
	body, status, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(container)+"/stop", nil)
	if err != nil {
		return fmt.Errorf("停止容器 %s 失败: %w", container, err)
	}
	// 304 表示容器已停止，视为成功。
	if status == http.StatusNoContent || status == http.StatusNotModified {
		return nil
	}
	return fmt.Errorf("停止容器 %s 失败: HTTP %d %s", container, status, apiMessage(body))
}

// TruncateInternalLogs 尝试清空容器内部 /var/log/*log。失败不算致命。
func (c *Client) TruncateInternalLogs(ctx context.Context, container string) error {
	execID, err := c.createExec(ctx, container, []string{
		"sh", "-c", "truncate -s 0 /var/log/*log 2>/dev/null || true",
	})
	if err != nil {
		return err
	}
	if err := c.startExec(ctx, execID); err != nil {
		return fmt.Errorf("容器内清空日志失败: %w", err)
	}
	return nil
}

// RotateLogs 借助读取日志尾部 0 行做无损探活。
// 说明：外部进程无法直接清空 docker 的 json 日志文件，这里只做一次读取确认容器仍在，
// 真正的日志回收依赖 docker 的 log-opts(max-size/max-file) 配置。
func (c *Client) RotateLogs(ctx context.Context, container string) error {
	path := "/containers/" + url.PathEscape(container) +
		"/logs?stdout=1&stderr=1&tail=0"
	body, status, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("读取容器 %s 日志失败: %w", container, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("读取容器 %s 日志失败: HTTP %d %s", container, status, truncate(body))
	}
	return nil
}

// LogsFollow 以流式方式跟踪容器日志。
// since 为 nil 时使用 tail=0 只跟踪新产生的日志。
func (c *Client) LogsFollow(ctx context.Context, container string, since *time.Time) (StreamHandle, error) {
	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("follow", "1")
	if since != nil {
		q.Set("since", strconv.FormatInt(since.Unix(), 10))
	}
	q.Set("tail", "0")
	path := "/containers/" + url.PathEscape(container) + "/logs?" + q.Encode()
	c.log.Debug("跟踪容器日志: %s", path)

	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	// 日志流是无限长度的，必须用流式响应，不能等 body 读完。
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("跟踪容器 %s 日志失败: %w", container, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("跟踪容器 %s 日志失败: HTTP %d %s",
			container, resp.StatusCode, apiMessage(body))
	}

	stream := &httpStream{
		lines:  make(chan string, 256),
		body:   resp.Body,
		cancel: ctx,
	}
	go stream.pump()
	return stream, nil
}

// LogsRange 一次性读取容器自 since 起至今的日志行，不跟随。
//
// 与 LogsFollow 的分工：follow 用于实时监控，本方法用于**回溯**——
// 典型场景是容器刚拉起时，回查最近一段时间的日志里是否已经出现了
// 关键词（比如应用一启动就打印"等待直播"），有则立即停止容器。
//
// 返回的行已按帧解复用（stdout/stderr 合并、剥离 8 字节帧头），
// 空行被丢弃。为防意外的大日志拖垮内存，最多读取 8MB。
func (c *Client) LogsRange(ctx context.Context, container string, since time.Time) ([]string, error) {
	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("follow", "0")
	q.Set("since", strconv.FormatInt(since.Unix(), 10))
	q.Set("tail", "0")
	path := "/containers/" + url.PathEscape(container) + "/logs?" + q.Encode()

	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取容器 %s 日志失败: %w", container, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("读取容器 %s 日志失败: HTTP %d %s",
			container, resp.StatusCode, apiMessage(body))
	}

	// 8MB 上限：一分钟内正常应用的日志远小于此，超出说明应用在刷屏，
	// 此时截断是合理代价。bufio 包在 LimitReader 外面，保证不越过上限。
	reader := bufio.NewReaderSize(io.LimitReader(resp.Body, 8<<20), 64*1024)
	var lines []string
	for {
		frame, err := readLogFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return lines, fmt.Errorf("读取容器 %s 日志失败: %w", container, err)
		}
		for _, line := range strings.Split(strings.TrimRight(string(frame), "\n"), "\n") {
			if line != "" {
				lines = append(lines, line)
			}
		}
	}
	return lines, nil
}

// ---- 内部实现 ----

// do 发起一次请求并读完全部响应体。
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	// 走 unix socket 时 host 无意义，但必须是非空值。
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// createExec 创建 exec 实例并返回其 ID。
func (c *Client) createExec(ctx context.Context, container string, cmd []string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          cmd,
	})
	if err != nil {
		return "", err
	}
	body, status, err := c.do(ctx, http.MethodPost,
		"/containers/"+url.PathEscape(container)+"/exec", payload)
	if err != nil {
		return "", fmt.Errorf("在容器 %s 中创建 exec 失败: %w", container, err)
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("在容器 %s 中创建 exec 失败: HTTP %d %s",
			container, status, apiMessage(body))
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("解析 exec 响应失败: %w", err)
	}
	if out.ID == "" {
		return "", errors.New("exec 响应缺少 Id 字段")
	}
	return out.ID, nil
}

// startExec 运行 exec 实例并等待结束。
func (c *Client) startExec(ctx context.Context, execID string) error {
	payload, err := json.Marshal(map[string]any{"Detach": true, "Tty": false})
	if err != nil {
		return err
	}
	body, status, err := c.do(ctx, http.MethodPost,
		"/exec/"+url.PathEscape(execID)+"/start", payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return fmt.Errorf("HTTP %d %s", status, apiMessage(body))
	}
	return nil
}

// httpStream 把 Engine 日志流的响应体按行推送到通道。
type httpStream struct {
	lines  chan string
	body   io.ReadCloser
	cancel context.Context
	once   sync.Once
	err    error
}

func (s *httpStream) pump() {
	defer close(s.lines)
	defer s.body.Close()

	reader := bufio.NewReaderSize(s.body, 64*1024)
	for {
		select {
		case <-s.cancel.Done():
			return
		default:
		}

		// 日志流（非 TTY）采用 8 字节帧头：1 字节流类型 + 3 字节填充 + 4 字节大端长度。
		// 必须按帧读取，否则帧头字节会混进日志内容里变成乱码。
		frame, err := readLogFrame(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.once.Do(func() { s.err = err })
			}
			return
		}

		// 一帧可能包含多行，逐行投递。
		for _, line := range strings.Split(strings.TrimRight(string(frame), "\n"), "\n") {
			select {
			case s.lines <- line:
			case <-s.cancel.Done():
				return
			}
		}
	}
}

// readLogFrame 读取一个日志帧的内容。
func readLogFrame(r *bufio.Reader) ([]byte, error) {
	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
	if size <= 0 {
		return []byte{}, nil
	}
	// 单帧异常大时限制读取量，避免被恶意/损坏的流拖垮内存。
	const maxFrame = 4 << 20
	if size > maxFrame {
		size = maxFrame
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// Lines 返回日志行通道。
func (s *httpStream) Lines() <-chan string { return s.lines }

// Wait 等待流结束。流因上下文取消而结束属预期行为，返回 nil。
func (s *httpStream) Wait() error {
	if s.cancel.Err() != nil {
		return nil
	}
	return s.err
}

// Kill 强制中断流。
func (s *httpStream) Kill() error {
	return s.body.Close()
}

// apiMessage 从 Engine 的错误响应中提取可读消息。
func apiMessage(body []byte) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err == nil && out.Message != "" {
		return out.Message
	}
	return truncate(body)
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func envOr(key, fallback string) string {
	return lookupEnv(key, fallback)
}
