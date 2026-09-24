// Command livemonitor 是直播容器监控与媒体转码服务的 Go 实现。
//
// 用法:
//
//	livemonitor run     -config config.json     启动监控服务
//	livemonitor init    -config config.json     生成默认配置
//	livemonitor check   -config config.json     校验配置
//	livemonitor probe   -file xxx.mp3           查看 MP3 信息
//	livemonitor version                         查看版本
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/totootao/livemonitor/internal/cli"
)

func main() {
	if err := cli.Run(context.Background(), os.Args[1:]...); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}
