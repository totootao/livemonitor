package dockerctl

import "os"

// lookupEnv 读取环境变量，为空时返回 fallback。
// 单独成函数便于测试时替换。
func lookupEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
