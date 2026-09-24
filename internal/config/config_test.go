package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadAcceptsStringAndArrayStartTimes 验证 start_times 单值/数组两种写法。
func TestLoadAcceptsStringAndArrayStartTimes(t *testing.T) {
	p := writeTemp(t, `{
	  "watch_dir": "/tmp/audio",
	  "containers": [
	    { "name": "single", "start_times": "20:01", "max_run_duration": 10800 },
	    { "name": "multi",  "start_times": ["17:03","17:13"], "max_run_duration": 10800 }
	  ]
	}`)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(cfg.Containers) != 2 {
		t.Fatalf("容器数 = %d, 期望 2", len(cfg.Containers))
	}
	if got := cfg.Containers[0].StartTimes; len(got) != 1 || got[0] != "20:01" {
		t.Errorf("单值写法解析结果 = %v", got)
	}
	if got := cfg.Containers[1].StartTimes; len(got) != 2 {
		t.Errorf("数组写法解析结果 = %v", got)
	}
}

// TestDefaultsApplied 验证缺省字段被补全。
func TestDefaultsApplied(t *testing.T) {
	p := writeTemp(t, `{
	  "containers": [ { "name": "c1", "start_times": "20:01" } ]
	}`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.WatchDir != DefaultWatchDir {
		t.Errorf("watch_dir 默认值 = %s, 期望 %s", cfg.WatchDir, DefaultWatchDir)
	}
	if cfg.CheckInterval != DefaultCheckInterval {
		t.Errorf("check_interval 默认值 = %d", cfg.CheckInterval)
	}
	if cfg.StableDelay != DefaultStableDelay {
		t.Errorf("stable_delay 默认值 = %d", cfg.StableDelay)
	}
	if cfg.ArchiveAfterHours != DefaultArchiveAfterHours {
		t.Errorf("archive_after_hours 默认值 = %d", cfg.ArchiveAfterHours)
	}
	if cfg.MP3Bitrate != DefaultMP3Bitrate {
		t.Errorf("mp3_bitrate 默认值 = %s", cfg.MP3Bitrate)
	}
	if len(cfg.MonitorKeywords) != 1 || cfg.MonitorKeywords[0] != "等待直播" {
		t.Errorf("monitor_keywords 默认值 = %v", cfg.MonitorKeywords)
	}
	if cfg.Containers[0].MaxRunDuration != DefaultMaxRunDuration {
		t.Errorf("max_run_duration 默认值 = %d", cfg.Containers[0].MaxRunDuration)
	}
}

// TestValidateRejectsBadInput 验证各类非法配置会被拒绝。
func TestValidateRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{
			name:    "空容器列表",
			content: `{ "containers": [] }`,
			wantSub: "containers 不能为空",
		},
		{
			name:    "缺失容器名",
			content: `{ "containers": [ { "start_times": "20:01" } ] }`,
			wantSub: "name 不能为空",
		},
		{
			name:    "重复容器名",
			content: `{ "containers": [ {"name":"a","start_times":"20:01"}, {"name":"a","start_times":"21:01"} ] }`,
			wantSub: "重复",
		},
		{
			name:    "非法时间",
			content: `{ "containers": [ { "name": "a", "start_times": "25:99" } ] }`,
			wantSub: "无效",
		},
		{
			name:    "非法比特率",
			content: `{ "mp3_bitrate": "33k", "containers": [ { "name": "a", "start_times": "20:01" } ] }`,
			wantSub: "mp3_bitrate",
		},
		{
			name:    "空启动时间",
			content: `{ "containers": [ { "name": "a", "start_times": [] } ] }`,
			wantSub: "start_times 不能为空",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.content))
			if err == nil {
				t.Fatal("期望报错，但校验通过了")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息 %q 应包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestValidateRejectsUnknownField 验证未知字段会被拒绝，避免拼写错误被静默忽略。
func TestValidateRejectsUnknownField(t *testing.T) {
	_, err := Load(writeTemp(t, `{
	  "watch_dir": "/tmp",
	  "watch_dirs": "/tmp",
	  "containers": [ { "name": "a", "start_times": "20:01" } ]
	}`))
	if err == nil {
		t.Fatal("未知字段应导致解析失败")
	}
}

// TestParseClock 验证时间解析。
func TestParseClock(t *testing.T) {
	for _, ts := range []string{"00:00", "07:12", "23:59", "20:01:30"} {
		if _, err := ParseClock(ts); err != nil {
			t.Errorf("ParseClock(%q) 失败: %v", ts, err)
		}
	}
	for _, ts := range []string{"", "24:00", "12:60", "abc", "12"} {
		if _, err := ParseClock(ts); err == nil {
			t.Errorf("ParseClock(%q) 应失败", ts)
		}
	}
}

// TestPerContainerKeywordsOverride 验证容器级关键词可覆盖全局设置。
func TestPerContainerKeywordsOverride(t *testing.T) {
	p := writeTemp(t, `{
	  "monitor_keywords": ["等待直播"],
	  "containers": [
	    { "name": "inherit", "start_times": "20:01" },
	    { "name": "custom", "start_times": "20:01", "keywords": ["已结束"] }
	  ]
	}`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Containers[0].Keywords) != 0 {
		t.Error("未设置 keywords 的容器应保持为空以便继承全局")
	}
	if len(cfg.Containers[1].Keywords) != 1 || cfg.Containers[1].Keywords[0] != "已结束" {
		t.Errorf("容器级关键词 = %v", cfg.Containers[1].Keywords)
	}
}

// TestStringListUnmarshalNumbers 验证数字型时间写法也能被容错解析。
func TestStringListUnmarshalNumbers(t *testing.T) {
	var sl StringList
	if err := json.Unmarshal([]byte(`2001`), &sl); err != nil {
		t.Fatalf("数字写法解析失败: %v", err)
	}
	if len(sl) != 1 || sl[0] != "2001" {
		t.Errorf("解析结果 = %v", sl)
	}
}

// TestFormatDuration 验证时长格式化。
func TestFormatDuration(t *testing.T) {
	cases := map[int]string{
		45:    "45秒",
		90:    "1分30秒",
		3600:  "1小时",
		10800: "3小时",
		7200:  "2小时",
		4000:  "1小时6分",
	}
	for sec, want := range cases {
		if got := FormatDuration(sec); got != want {
			t.Errorf("FormatDuration(%d) = %q, 期望 %q", sec, got, want)
		}
	}
}

// TestClockTimezone 确认解析使用本地时区。
func TestClockTimezone(t *testing.T) {
	at, err := ParseClock("20:01")
	if err != nil {
		t.Fatal(err)
	}
	if at.Location() != time.Local {
		t.Errorf("时区 = %v, 期望 Local", at.Location())
	}
	if at.Hour() != 20 || at.Minute() != 1 {
		t.Errorf("解析结果 = %s", at.Format("15:04"))
	}
}
