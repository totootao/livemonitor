// Package config 负责监控配置的定义、加载与校验。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/totootao/livemonitor/internal/logging"
)

// DefaultBitrates 是允许的 MP3 比特率集合。
var DefaultBitrates = []string{"16k", "24k", "32k", "48k", "64k", "96k", "128k"}

// ContainerConfig 描述单个被监控容器的启动计划。
type ContainerConfig struct {
	// Name 是 docker 容器名。
	Name string `json:"name"`
	// StartTimes 支持 "20:01" 单值或 ["17:03","17:13"] 数组两种写法。
	StartTimes StringList `json:"start_times"`
	// MaxRunDuration 是最大运行秒数，<=0 时取 DefaultMaxRunDuration。
	MaxRunDuration int `json:"max_run_duration"`
	// Keywords 覆盖全局监控关键词；为空时继承全局配置。
	Keywords StringList `json:"keywords"`
}

// Config 是整个监控程序的配置。
type Config struct {
	// WatchDir 是被监控的媒体目录（音频/视频文件落地目录）。
	WatchDir string `json:"watch_dir"`
	// HistoryDir 是归档目录，为空时取 WatchDir/历史。
	HistoryDir string `json:"history_dir"`
	// CheckInterval 是目录扫描间隔秒数。
	CheckInterval int `json:"check_interval"`
	// StableDelay 是文件最后修改后需静置的秒数，避免处理仍在写入的文件。
	StableDelay int `json:"stable_delay"`
	// ArchiveAfterHours 是归档阈值（小时），早于该时长的 MP3 会被移入归档目录。
	ArchiveAfterHours int `json:"archive_after_hours"`
	// MP3Bitrate 是压缩输出的目标比特率。
	MP3Bitrate string `json:"mp3_bitrate"`
	// MonitorKeywords 是触发停止容器的日志关键词。
	MonitorKeywords StringList `json:"monitor_keywords"`
	// Containers 是容器监控列表。
	Containers []ContainerConfig `json:"containers"`
}

// 默认值常量。
const (
	DefaultWatchDir          = "/audio"
	DefaultCheckInterval     = 30
	DefaultStableDelay       = 60
	DefaultArchiveAfterHours = 45
	DefaultMP3Bitrate        = "32k"
	DefaultMaxRunDuration    = 3600
)

// DefaultMonitorKeywords 是默认的日志监控关键词。
func DefaultMonitorKeywords() []string { return []string{"等待直播"} }

// StringList 允许 JSON 中同一个字段既写成字符串又写成字符串数组。
type StringList []string

// UnmarshalJSON 实现单值/数组兼容的解析。
func (s *StringList) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" || trimmed == "" {
		*s = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []string
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		*s = StringList(list)
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		// 兼容数字型的时间写法（例如 2001 之类），统一转成字符串再校验。
		var num json.Number
		if err2 := json.Unmarshal(data, &num); err2 == nil {
			*s = StringList{num.String()}
			return nil
		}
		return fmt.Errorf("start_times 需要是字符串或字符串数组: %w", err)
	}
	*s = StringList{single}
	return nil
}

// Load 从 JSON 文件读取配置并填充默认值、执行校验。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	cfg := &Config{}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.WatchDir == "" {
		c.WatchDir = DefaultWatchDir
	}
	if c.HistoryDir == "" {
		c.HistoryDir = filepath.Join(c.WatchDir, "历史")
	}
	if c.CheckInterval <= 0 {
		c.CheckInterval = DefaultCheckInterval
	}
	if c.StableDelay <= 0 {
		c.StableDelay = DefaultStableDelay
	}
	if c.ArchiveAfterHours <= 0 {
		c.ArchiveAfterHours = DefaultArchiveAfterHours
	}
	if c.MP3Bitrate == "" {
		c.MP3Bitrate = DefaultMP3Bitrate
	}
	if len(c.MonitorKeywords) == 0 {
		c.MonitorKeywords = StringList(DefaultMonitorKeywords())
	}
	for i := range c.Containers {
		if c.Containers[i].MaxRunDuration <= 0 {
			c.Containers[i].MaxRunDuration = DefaultMaxRunDuration
		}
	}
}

// Validate 校验配置的合法性，返回聚合后的错误。
func (c *Config) Validate() error {
	var problems []string

	if _, err := time.LoadLocation("Local"); err != nil {
		problems = append(problems, "无法加载本地时区: "+err.Error())
	}

	if !isValidBitrate(c.MP3Bitrate) {
		problems = append(problems, fmt.Sprintf("mp3_bitrate %q 无效，可选值: %s",
			c.MP3Bitrate, strings.Join(DefaultBitrates, ", ")))
	}
	if c.CheckInterval < 1 {
		problems = append(problems, "check_interval 必须 >= 1")
	}
	if c.StableDelay < 0 {
		problems = append(problems, "stable_delay 不能为负数")
	}

	if len(c.Containers) == 0 {
		problems = append(problems, "containers 不能为空")
	}

	seen := make(map[string]bool)
	for i, cc := range c.Containers {
		where := fmt.Sprintf("containers[%d]", i)
		if strings.TrimSpace(cc.Name) == "" {
			problems = append(problems, where+".name 不能为空")
		} else {
			if seen[cc.Name] {
				problems = append(problems, fmt.Sprintf("容器名 %q 重复", cc.Name))
			}
			seen[cc.Name] = true
		}
		if len(cc.StartTimes) == 0 {
			problems = append(problems, where+".start_times 不能为空")
		}
		for _, ts := range cc.StartTimes {
			if _, err := ParseClock(ts); err != nil {
				problems = append(problems, fmt.Sprintf("%s.start_times 中的 %q 无效: %v", where, ts, err))
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("配置校验失败:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func isValidBitrate(b string) bool {
	for _, v := range DefaultBitrates {
		if v == b {
			return true
		}
	}
	return false
}

// ParseClock 解析 "HH:MM" 或 "HH:MM:SS" 形式的每日时间点。
func ParseClock(s string) (time.Time, error) {
	formats := []string{"15:04", "15:04:05"}
	for _, f := range formats {
		if t, err := time.ParseInLocation(f, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("时间格式应为 HH:MM 或 HH:MM:SS")
}

// LogSummary 打印配置摘要，便于启动时确认生效参数。
func (c *Config) LogSummary(log *logging.Logger) {
	log.Info("监控目录: %s", c.WatchDir)
	log.Info("归档目录: %s", c.HistoryDir)
	log.Info("扫描间隔: %ds  静置阈值: %ds  归档阈值: %dh", c.CheckInterval, c.StableDelay, c.ArchiveAfterHours)
	log.Info("MP3 比特率: %s  全局监控关键词: %s", c.MP3Bitrate, strings.Join(c.MonitorKeywords, ", "))
	log.Info("共管理 %d 个容器:", len(c.Containers))

	names := make([]string, 0, len(c.Containers))
	for _, cc := range c.Containers {
		names = append(names, cc.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, cc := range c.Containers {
			if cc.Name != name {
				continue
			}
			times := make([]string, 0, len(cc.StartTimes))
			for _, ts := range cc.StartTimes {
				if t, err := ParseClock(ts); err == nil {
					times = append(times, t.Format("15:04"))
				} else {
					times = append(times, ts)
				}
			}
			log.Info("  - %-22s 启动时间: %-45s 最长运行: %s",
				cc.Name, strings.Join(times, ","), FormatDuration(cc.MaxRunDuration))
		}
	}
}

// FormatDuration 把秒数格式化为易读的中文时长。
func FormatDuration(sec int) string {
	if sec < 60 {
		return fmt.Sprintf("%d秒", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%d分%d秒", sec/60, sec%60)
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	if m == 0 {
		return fmt.Sprintf("%d小时", h)
	}
	return fmt.Sprintf("%d小时%d分", h, m)
}
