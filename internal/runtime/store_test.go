package runtime

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/logging"
)

func newTestStore(t *testing.T, containers ...config.ContainerConfig) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
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
	return NewStore(cfg, path, logging.New("test")), path
}

func cc(name string, times ...string) config.ContainerConfig {
	return config.ContainerConfig{
		Name:           name,
		StartTimes:     config.StringList(times),
		MaxRunDuration: 3600,
	}
}

// TestSnapshotIsDeepCopy 验证 Snapshot 返回深拷贝，外部修改不影响内部状态。
func TestSnapshotIsDeepCopy(t *testing.T) {
	s, _ := newTestStore(t, cc("a", "10:00"))

	snap := s.Snapshot()
	snap.Containers[0].Name = "tampered"
	snap.Containers[0].StartTimes[0] = "23:59"
	snap.MonitorKeywords[0] = "tampered"

	again := s.Snapshot()
	if again.Containers[0].Name != "a" {
		t.Errorf("内部容器名被外部修改污染: %s", again.Containers[0].Name)
	}
	if again.Containers[0].StartTimes[0] != "10:00" {
		t.Errorf("内部启动时刻被外部修改污染: %v", again.Containers[0].StartTimes)
	}
	if again.MonitorKeywords[0] != "等待直播" {
		t.Errorf("内部关键词被外部修改污染: %v", again.MonitorKeywords)
	}
}

// TestUpsertPersistsAndNormalizes 验证新增容器会归一化时刻并落盘。
func TestUpsertPersistsAndNormalizes(t *testing.T) {
	s, path := newTestStore(t)

	// 时刻格式合法但写法杂乱：含空白、重复项，应被归一化为去空白 + 去重 + 升序。
	got, created, err := s.UpsertContainer(cc("zhangsan", "20:01", " 09:05 ", "20:01"))
	if err != nil {
		t.Fatalf("UpsertContainer 失败: %v", err)
	}
	if !created {
		t.Error("首次写入应为新增")
	}
	want := []string{"09:05", "20:01"}
	if len(got.StartTimes) != len(want) {
		t.Fatalf("归一化后时刻 = %v, 期望 %v", got.StartTimes, want)
	}
	for i := range want {
		if got.StartTimes[i] != want[i] {
			t.Errorf("时刻[%d] = %s, 期望 %s", i, got.StartTimes[i], want[i])
		}
	}

	// 磁盘上应已存在且可被 config.Load 重新读回。
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("落盘后的配置应可重新加载: %v", err)
	}
	if len(reloaded.Containers) != 1 || reloaded.Containers[0].Name != "zhangsan" {
		t.Errorf("磁盘配置不符: %+v", reloaded.Containers)
	}
}

// TestUpsertRejectsInvalid 验证非法输入被拒绝。
func TestUpsertRejectsInvalid(t *testing.T) {
	s, _ := newTestStore(t)

	cases := []struct {
		name string
		cc   config.ContainerConfig
	}{
		{"空名称", cc("", "10:00")},
		{"无时刻", cc("x")},
		{"非法时刻", cc("x", "25:99")},
		{"负时长", config.ContainerConfig{Name: "x", StartTimes: config.StringList{"10:00"}, MaxRunDuration: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.UpsertContainer(tc.cc); err == nil {
				t.Error("应返回错误")
			}
		})
	}
	if n := len(s.Containers()); n != 0 {
		t.Errorf("非法输入不应写入，当前容器数 = %d", n)
	}
}

// TestUpsertUpdatesExisting 验证同名容器为更新而非新增。
func TestUpsertUpdatesExisting(t *testing.T) {
	s, _ := newTestStore(t, cc("a", "10:00"))

	_, created, err := s.UpsertContainer(cc("a", "11:00", "12:00"))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("同名容器应为更新")
	}
	if n := len(s.Containers()); n != 1 {
		t.Fatalf("容器数 = %d, 期望 1", n)
	}
	got, _ := s.GetContainer("a")
	if len(got.StartTimes) != 2 {
		t.Errorf("更新后时刻 = %v, 期望 2 个", got.StartTimes)
	}
}

// TestDeleteContainer 验证删除后磁盘同步。
func TestDeleteContainer(t *testing.T) {
	s, path := newTestStore(t, cc("a", "10:00"), cc("b", "11:00"))

	if err := s.DeleteContainer("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetContainer("a"); ok {
		t.Error("a 应已删除")
	}
	if err := s.DeleteContainer("nope"); err == nil {
		t.Error("删除不存在的容器应报错")
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Containers) != 1 || reloaded.Containers[0].Name != "b" {
		t.Errorf("磁盘上应只剩 b, 实际 %+v", reloaded.Containers)
	}
}

// TestUpdateSettings 验证全局设置更新并落盘。
func TestUpdateSettings(t *testing.T) {
	s, path := newTestStore(t, cc("a", "10:00"))

	err := s.UpdateSettings(func(c *config.Config) error {
		c.MP3Bitrate = "64k"
		c.CheckInterval = 45
		c.MonitorKeywords = config.StringList{"已结束"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.MP3Bitrate != "64k" || reloaded.CheckInterval != 45 {
		t.Errorf("设置未正确落盘: %+v", reloaded)
	}
	if len(reloaded.MonitorKeywords) != 1 || reloaded.MonitorKeywords[0] != "已结束" {
		t.Errorf("关键词未正确落盘: %v", reloaded.MonitorKeywords)
	}
}

// TestConcurrentAccess 在竞态检测下验证并发读写安全。
func TestConcurrentAccess(t *testing.T) {
	s, _ := newTestStore(t, cc("a", "10:00"))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = s.Snapshot()
				_ = s.Containers()
			}
		}()
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				name := "c" + string(rune('0'+n)) + string(rune('0'+j%10))
				_, _, _ = s.UpsertContainer(cc(name, "10:00"))
				_ = s.DeleteContainer(name)
			}
		}(i)
	}
	wg.Wait()
}

// TestParseDurationInput 验证时长解析。
func TestParseDurationInput(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"3600", 3600, true},
		{"0", 0, true},
		{"90m", 5400, true},
		{"3h", 10800, true},
		{"45s", 45, true},
		{" 2H ", 7200, true},
		{"-1", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{"3d", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseDurationInput(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("期望成功，实际错误: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("期望失败，实际返回 %d", got)
			}
			if tc.ok && got != tc.want {
				t.Errorf("= %d, 期望 %d", got, tc.want)
			}
		})
	}
}

// TestSaveIsAtomic 验证保存通过临时文件 + rename，不产生半截文件。
func TestSaveIsAtomic(t *testing.T) {
	s, path := newTestStore(t)
	if _, _, err := s.UpsertContainer(cc("a", "10:00")); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("目录中残留临时文件: %s", e.Name())
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Error("配置文件应以换行结尾")
	}
}
