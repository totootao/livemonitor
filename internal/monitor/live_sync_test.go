package monitor

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/dockerctl"
	"github.com/totootao/livemonitor/internal/logging"
)

// TestLiveSyncDetectsExternalStop 用真实 Docker 验证一个此前会误报的场景：
// 程序接管了容器、认为它在运行，随后容器被外部停掉，
// SyncState 必须能把内部记账纠正过来。
//
// 容器的新建与删除借 docker CLI 完成（程序自身只负责控制已存在的容器，
// dockerctl 刻意不提供 create/remove）；状态查询与停止走程序自己的实现。
// Docker 不可用时自动跳过，因此不会影响普通单元测试。
func TestLiveSyncDetectsExternalStop(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("未找到 docker 命令")
	}

	c := dockerctl.New(logging.New("live"))
	name := "lmsynctest"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := c.Available(ctx); err != nil {
		t.Skip("Docker 不可用:", err)
	}

	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", name,
		"alpine:3.22", "sleep", "600").CombinedOutput(); err != nil {
		t.Skipf("无法创建测试容器: %v (%s)", err, out)
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()

	cc := config.ContainerConfig{
		Name:           name,
		MaxRunDuration: 3600,
		Keywords:       config.StringList{"NEVERMATCH"},
	}
	m := New(cc, nil, c, logging.New("live"))

	// Start 会看到容器在运行，从而接管它。
	m.Start()
	time.Sleep(700 * time.Millisecond)
	if !m.IsRunning() {
		t.Fatal("前置条件：应已接管该容器")
	}

	// 从外部把容器停掉——等价于用户执行 docker stop。
	if err := c.Stop(ctx, name); err != nil {
		t.Fatalf("外部停止失败: %v", err)
	}
	time.Sleep(700 * time.Millisecond)

	// 此时内部记账仍是"运行中"，这正是待修的偏差。
	if !m.IsRunning() {
		t.Fatal("前置条件：内部记账此刻应仍为运行中（尚未核对）")
	}

	if m.SyncState(ctx) {
		t.Error("容器已被外部停止，SyncState 应报告未在运行")
	}
	if m.IsRunning() {
		t.Error("SyncState 后内部记账必须被纠正为未运行")
	}
	t.Log("验证通过：外部停止被正确识别并纠正")
}
