package dockerctl

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// 真机集成测试：仅在显式设置 LIVEMONITOR_DOCKER_E2E=1 时运行。
// 需要宿主机有可用的 docker socket 与名为 livemonitor-selftest 的运行中容器。
func TestLiveEngineAPI(t *testing.T) {
	if os.Getenv("LIVEMONITOR_DOCKER_E2E") != "1" {
		t.Skip("未设置 LIVEMONITOR_DOCKER_E2E=1，跳过真机测试")
	}
	const name = "livemonitor-selftest"
	log := logging.New("debug")
	c := New(log)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.Available(ctx); err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	t.Log("探活成功")

	if _, err := c.InspectRunning(ctx, "definitely-not-exist-xyz"); err == nil {
		t.Error("不存在的容器应返回错误")
	} else {
		t.Logf("不存在容器返回错误（预期）: %v", err)
	}

	running, err := c.InspectRunning(ctx, name)
	if err != nil {
		t.Fatalf("查询 %s 失败: %v", name, err)
	}
	if !running {
		t.Fatalf("容器 %s 应处于运行中", name)
	}
	t.Log("状态查询正确: running = true")

	// 日志流：应能收到按帧解析后的内容。
	h, err := c.LogsFollow(ctx, name, nil)
	if err != nil {
		t.Fatalf("建立日志流失败: %v", err)
	}
	defer func() { _ = h.Kill() }()

	select {
	case line, ok := <-h.Lines():
		if !ok {
			t.Fatal("日志通道意外关闭")
		}
		t.Logf("收到日志行: %q", line)
	case <-time.After(8 * time.Second):
		t.Fatal("等待日志超时")
	}

	// 停止 + 启动，验证写操作。
	if err := c.Stop(ctx, name); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	running, err = c.InspectRunning(ctx, name)
	if err != nil {
		t.Fatalf("停止后查询失败: %v", err)
	}
	if running {
		t.Error("停止后应处于未运行状态")
	}
	t.Log("停止成功")

	if err := c.Start(ctx, name); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	running, err = c.InspectRunning(ctx, name)
	if err != nil {
		t.Fatalf("启动后查询失败: %v", err)
	}
	if !running {
		t.Error("启动后应处于运行状态")
	}
	t.Log("启动成功")

	// exec：清空容器内日志。
	if err := c.TruncateInternalLogs(ctx, name); err != nil {
		t.Errorf("容器内清空日志失败: %v", err)
	} else {
		t.Log("exec 成功")
	}

	// RotateLogs：读取尾部 0 行做探活。
	if err := c.RotateLogs(ctx, name); err != nil {
		t.Errorf("读取日志失败: %v", err)
	} else {
		t.Log("日志读取成功")
	}
}
