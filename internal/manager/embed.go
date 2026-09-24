// 把配置文件内嵌进二进制，配置缺失时自动释放一份默认配置。
package manager

import (
	_ "embed"
	"fmt"
	"os"
)

//go:embed default_config.json
var defaultConfigJSON []byte

// DefaultConfigJSON 返回内嵌的默认配置内容。
func DefaultConfigJSON() []byte {
	out := make([]byte, len(defaultConfigJSON))
	copy(out, defaultConfigJSON)
	return out
}

// WriteDefaultConfig 把默认配置写到指定路径。
func WriteDefaultConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("配置文件已存在: %s", path)
	}
	return os.WriteFile(path, defaultConfigJSON, 0o644)
}
