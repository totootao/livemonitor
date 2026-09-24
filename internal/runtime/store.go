// Package runtime 提供可热更新的运行时配置存储。
//
// 与 config 包的区别：
//   - config 负责从磁盘加载、填充默认值、做静态校验；
//   - runtime.Store 负责在进程运行期间持有配置快照，支持并发读写、
//     原地修改（增删容器、改启动时刻）并回写磁盘。
package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/logging"
)

// Store 是并发安全的配置持有者。
type Store struct {
	mu   sync.RWMutex
	cfg  *config.Config
	path string
	log  *logging.Logger
}

// NewStore 创建配置存储。
func NewStore(cfg *config.Config, path string, log *logging.Logger) *Store {
	return &Store{cfg: cfg, path: path, log: log}
}

// Path 返回配置文件路径。
func (s *Store) Path() string { return s.path }

// Snapshot 返回配置的深拷贝，调用方可安全读写而不影响内部状态。
func (s *Store) Snapshot() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.cfg)
}

// Containers 返回容器配置列表的深拷贝，按名称排序。
func (s *Store) Containers() []config.ContainerConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]config.ContainerConfig, len(s.cfg.Containers))
	for i, c := range s.cfg.Containers {
		out[i] = cloneContainer(c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GetContainer 按名称查找容器配置。
func (s *Store) GetContainer(name string) (config.ContainerConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.cfg.Containers {
		if c.Name == name {
			return cloneContainer(c), true
		}
	}
	return config.ContainerConfig{}, false
}

// UpsertContainer 新增或更新容器配置，并回写磁盘。
// 返回更新后的配置与其是否为新增。
func (s *Store) UpsertContainer(cc config.ContainerConfig) (config.ContainerConfig, bool, error) {
	if err := validateContainer(cc); err != nil {
		return config.ContainerConfig{}, false, err
	}
	// 归一化启动时刻：统一格式与去重。
	cc.StartTimes = normalizeTimes(cc.StartTimes)
	cc.Name = strings.TrimSpace(cc.Name)

	s.mu.Lock()
	created := true
	for i := range s.cfg.Containers {
		if s.cfg.Containers[i].Name == cc.Name {
			s.cfg.Containers[i] = cc
			created = false
			break
		}
	}
	if created {
		s.cfg.Containers = append(s.cfg.Containers, cc)
	}
	sort.Slice(s.cfg.Containers, func(i, j int) bool {
		return s.cfg.Containers[i].Name < s.cfg.Containers[j].Name
	})
	s.mu.Unlock()

	if err := s.save(); err != nil {
		return cc, created, err
	}
	return cc, created, nil
}

// DeleteContainer 删除容器配置并回写磁盘。
func (s *Store) DeleteContainer(name string) error {
	s.mu.Lock()
	idx := -1
	for i := range s.cfg.Containers {
		if s.cfg.Containers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return fmt.Errorf("容器 %q 不存在", name)
	}
	s.cfg.Containers = append(s.cfg.Containers[:idx], s.cfg.Containers[idx+1:]...)
	s.mu.Unlock()

	return s.save()
}

// UpdateSettings 更新全局设置（媒体目录、比特率、关键词等）并回写磁盘。
func (s *Store) UpdateSettings(fn func(*config.Config) error) error {
	s.mu.Lock()
	if err := fn(s.cfg); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	return s.save()
}

// save 把当前配置原子写入磁盘：先写临时文件再 rename，避免写一半损坏配置。
func (s *Store) save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// 失败路径下清理临时文件；成功后它已被 rename 掉。
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时配置文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时配置文件失败: %w", err)
	}

	// 显式设定权限为 0644。
	//
	// os.CreateTemp 建的临时文件是 0600，而 os.Rename 会把这个权限原样带过去，
	// 于是每次保存都会把配置文件的权限收窄到"仅属主可读写"。
	// 容器默认以 root 运行、配置文件又是从宿主机挂载进去的，结果就是：
	// 用 Web 改一次设置之后，宿主机上的普通用户再也读不了自己的 config.json
	// （表现为 cat/ls 报 Permission denied，甚至编辑器打不开）。
	//
	// 配置文件里只是定时任务与目录路径，不含凭据，0644 是合适的。
	// 权限放宽失败不作为致命错误：文件已经写好，为此让保存整个失败不值得。
	if err := os.Chmod(tmpName, 0o644); err != nil {
		s.log.Warn("设置配置文件权限失败（不影响本次保存）: %v", err)
	}

	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("替换配置文件失败: %w", err)
	}
	s.log.Info("配置已保存到 %s", s.path)
	return nil
}

// validateContainer 校验单个容器配置。
func validateContainer(cc config.ContainerConfig) error {
	var problems []string
	if strings.TrimSpace(cc.Name) == "" {
		problems = append(problems, "容器名不能为空")
	}
	if len(cc.StartTimes) == 0 {
		problems = append(problems, "至少需要一个启动时刻")
	}
	for _, ts := range cc.StartTimes {
		if _, err := config.ParseClock(strings.TrimSpace(ts)); err != nil {
			problems = append(problems, fmt.Sprintf("启动时刻 %q 非法（应为 HH:MM）", ts))
		}
	}
	if cc.MaxRunDuration < 0 {
		problems = append(problems, "最大运行时长不能为负数")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "；"))
	}
	return nil
}

// normalizeTimes 去除空白、去重并排序启动时刻。
func normalizeTimes(times config.StringList) config.StringList {
	seen := make(map[string]bool)
	out := make(config.StringList, 0, len(times))
	for _, ts := range times {
		ts = strings.TrimSpace(ts)
		if ts == "" {
			continue
		}
		t, err := config.ParseClock(ts)
		if err != nil {
			continue
		}
		key := t.Format("15:04")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// cloneConfig 深拷贝配置。
func cloneConfig(c *config.Config) *config.Config {
	if c == nil {
		return nil
	}
	out := *c
	out.MonitorKeywords = append(config.StringList(nil), c.MonitorKeywords...)
	out.Containers = make([]config.ContainerConfig, len(c.Containers))
	for i, cc := range c.Containers {
		out.Containers[i] = cloneContainer(cc)
	}
	return &out
}

func cloneContainer(c config.ContainerConfig) config.ContainerConfig {
	out := c
	out.StartTimes = append(config.StringList(nil), c.StartTimes...)
	out.Keywords = append(config.StringList(nil), c.Keywords...)
	return out
}

// ParseDurationInput 解析前端提交的时长，支持纯秒数或 "3h"、"90m"、"5400" 等写法。
func ParseDurationInput(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("时长不能为空")
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("时长不能为负数")
		}
		return n, nil
	}
	unit := s[len(s)-1]
	value := s[:len(s)-1]
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("无法识别的时长: %s", s)
	}
	switch unit {
	case 's':
		return n, nil
	case 'm':
		return n * 60, nil
	case 'h':
		return n * 3600, nil
	default:
		return 0, fmt.Errorf("无法识别的时长单位: %s（支持 s/m/h 或纯秒数）", s)
	}
}
