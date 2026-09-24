// Package dockerctl 封装对 docker CLI 的调用。
package dockerctl

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// ErrNotRunning 表示容器当前不处于运行状态。
var ErrNotRunning = errors.New("容器未在运行")

// Runner 抽象命令执行，便于测试时注入假实现。
type Runner interface {
	// Run 同步执行命令并返回标准输出 + 标准错误。
	Run(ctx context.Context, name string, args ...string) (stdout string, stderr string, err error)
	// Stream 流式执行命令，逐行回调输出（stdout 与 stderr 合并）。
	Stream(ctx context.Context, name string, args ...string) (StreamHandle, error)
}

// StreamHandle 表示一个正在运行的流式子进程。
type StreamHandle interface {
	// Lines 返回日志行通道，进程退出或上下文取消后通道关闭。
	Lines() <-chan string
	// Wait 等待进程退出并返回其错误（若被取消则为 context 错误或 nil）。
	Wait() error
	// Kill 强制结束子进程。
	Kill() error
}

// Client 是面向 docker 的客户端。
type Client struct {
	run    Runner
	binary string
	log    *logging.Logger
}

// New 创建 docker 客户端，使用系统 PATH 中的 docker 可执行文件。
func New(log *logging.Logger) *Client {
	return &Client{run: &execRunner{}, binary: "docker", log: log}
}

// NewWithRunner 用自定义 Runner 创建客户端，主要用于测试。
func NewWithRunner(r Runner, binary string, log *logging.Logger) *Client {
	return &Client{run: r, binary: binary, log: log}
}

// BinaryAvailable 检查 docker 命令是否可用。
func (c *Client) BinaryAvailable() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := c.run.Run(ctx, c.binary, "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker 不可用: %w", err)
	}
	return nil
}

// InspectRunning 判断容器是否处于 running 状态。
func (c *Client) InspectRunning(ctx context.Context, container string) (bool, error) {
	out, _, err := c.run.Run(ctx, c.binary, "inspect", "-f", "{{.State.Running}}", container)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

// Start 启动容器。
func (c *Client) Start(ctx context.Context, container string) error {
	if _, stderr, err := c.run.Run(ctx, c.binary, "start", container); err != nil {
		return fmt.Errorf("docker start %s 失败: %w (%s)", container, err, strings.TrimSpace(stderr))
	}
	return nil
}

// Stop 停止容器，带超时时间（秒）。
func (c *Client) Stop(ctx context.Context, container string) error {
	if _, stderr, err := c.run.Run(ctx, c.binary, "stop", container); err != nil {
		return fmt.Errorf("docker stop %s 失败: %w (%s)", container, err, strings.TrimSpace(stderr))
	}
	return nil
}

// TruncateInternalLogs 尝试清空容器内部 /var/log/*log。失败不算致命。
func (c *Client) TruncateInternalLogs(ctx context.Context, container string) error {
	_, stderr, err := c.run.Run(ctx, c.binary, "exec", container, "sh", "-c", "truncate -s 0 /var/log/*log 2>/dev/null || true")
	if err != nil {
		return fmt.Errorf("容器内清空日志失败: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return nil
}

// RotateLogs 借助 `docker logs --tail 0` 触发 json-file 日志驱动的轮转读取。
// 说明：外部进程无法直接清空 docker 的 json 日志文件，这里用读取尾部 0 行做无损探活，
// 真正的日志回收依赖 docker 的 log-opts(max-size/max-file) 配置。
func (c *Client) RotateLogs(ctx context.Context, container string) error {
	_, stderr, err := c.run.Run(ctx, c.binary, "logs", "--tail", "0", container)
	if err != nil {
		return fmt.Errorf("docker logs 清理失败: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return nil
}

// LogsFollow 以流式方式跟踪容器日志。
// since 为 nil 时使用 --tail=0 只跟踪新产生的日志。
func (c *Client) LogsFollow(ctx context.Context, container string, since *time.Time) (StreamHandle, error) {
	args := []string{"logs", "-f", "--tail=0"}
	if since != nil {
		args = append(args, "--since", since.Format(time.RFC3339))
	}
	args = append(args, container)
	c.log.Debug("执行: %s %s", c.binary, strings.Join(args, " "))
	return c.run.Stream(ctx, c.binary, args...)
}

// ---- 默认 Runner 实现 ----

type execRunner struct{}

func (e *execRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

type execStream struct {
	lines chan string
	cmd   *exec.Cmd
	pipe  io.ReadCloser
	once  sync.Once
}

func (e *execRunner) Stream(ctx context.Context, name string, args ...string) (StreamHandle, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout // 合并 stderr 到同一管道，与原脚本行为一致
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	st := &execStream{lines: make(chan string, 256), cmd: cmd, pipe: pipe}
	go st.pump()
	return st, nil
}

func (s *execStream) pump() {
	defer close(s.lines)
	scanner := bufio.NewScanner(s.pipe)
	// 直播弹幕日志单行可能很长，放宽缓冲区上限到 1MB。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		s.lines <- scanner.Text()
	}
}

// Lines 返回日志行通道。
func (s *execStream) Lines() <-chan string { return s.lines }

// Wait 等待子进程退出。
func (s *execStream) Wait() error {
	err := s.cmd.Wait()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// 被 SIGTERM/上下文取消导致的退出属预期行为。
			return nil
		}
	}
	return err
}

// Kill 终止子进程。
func (s *execStream) Kill() error {
	var err error
	s.once.Do(func() {
		if s.cmd.Process != nil {
			err = s.cmd.Process.Kill()
		}
	})
	return err
}
