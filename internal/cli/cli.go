// Package cli 实现命令行入口与子命令。
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/totootao/livemonitor/internal/config"
	"github.com/totootao/livemonitor/internal/logging"
	"github.com/totootao/livemonitor/internal/manager"
	"github.com/totootao/livemonitor/internal/media"
)

// Version 是程序版本号。
const Version = "1.0.0"

const defaultConfigPath = "config.json"

// Run 解析参数并执行对应子命令。
func Run(ctx context.Context, args ...string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "run":
		return runCmd(ctx, args[1:])
	case "init":
		return initCmd(args[1:])
	case "check":
		return checkCmd(args[1:])
	case "probe":
		return probeCmd(args[1:])
	case "version", "-v", "--version":
		fmt.Printf("livemonitor %s\n", Version)
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("未知命令: %s", args[0])
	}
}

func printUsage() {
	fmt.Print(`livemonitor - 直播容器监控与媒体转码服务 (Go 实现)

用法:
  livemonitor <命令> [选项]

命令:
  run       启动监控服务（定时启动容器、日志监控、媒体转码与归档）
  init      生成默认配置文件
  check     校验配置文件并打印解析结果
  probe     查看 MP3 文件的码率等音频参数
  version   打印版本号
  help      显示本帮助

通用选项:
  -config <路径>   配置文件路径，默认 config.json

示例:
  livemonitor init -config config.json
  livemonitor check -config config.json
  livemonitor run   -config config.json -log-level info
  livemonitor probe -file /audio/example.mp3
`)
}

// commonFlags 是各子命令共用的选项。
type commonFlags struct {
	config   string
	logLevel string
}

func parseCommon(fs *flag.FlagSet) *commonFlags {
	cf := &commonFlags{}
	fs.StringVar(&cf.config, "config", defaultConfigPath, "配置文件路径")
	fs.StringVar(&cf.logLevel, "log-level", "info", "日志级别: debug|info|warn|error")
	return cf
}

func (cf *commonFlags) applyLogLevel() error {
	lv, ok := logging.ParseLevel(cf.logLevel)
	if !ok {
		return fmt.Errorf("无效的日志级别: %s", cf.logLevel)
	}
	logging.SetGlobalLevel(lv)
	return nil
}

func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	cf := parseCommon(fs)
	force := fs.Bool("force", false, "覆盖已存在的配置文件")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if _, err := os.Stat(cf.config); err == nil {
		if !*force {
			return fmt.Errorf("配置文件 %s 已存在，如需覆盖请加 -force", cf.config)
		}
		if err := os.Remove(cf.config); err != nil {
			return err
		}
	}
	if err := manager.WriteDefaultConfig(cf.config); err != nil {
		return err
	}
	fmt.Printf("已生成默认配置: %s\n", cf.config)

	// 顺带校验一次，确保生成的配置本身合法。
	if _, err := config.Load(cf.config); err != nil {
		return err
	}
	fmt.Println("配置校验通过")
	return nil
}

func checkCmd(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	cf := parseCommon(fs)
	asJSON := fs.Bool("json", false, "以 JSON 形式输出解析后的配置")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := cf.applyLogLevel(); err != nil {
		return err
	}

	cfg, err := config.Load(cf.config)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg)
	}
	log := logging.New("config")
	fmt.Printf("配置文件 %s 校验通过\n", cf.config)
	cfg.LogSummary(log)
	return nil
}

func probeCmd(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	file := fs.String("file", "", "MP3 文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("必须通过 -file 指定 MP3 文件")
	}
	info, err := media.ProbeMP3(*file)
	if err != nil {
		return err
	}
	fmt.Printf("文件: %s\n%s\n", *file, info.Describe())
	return nil
}

func runCmd(parent context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cf := parseCommon(fs)
	force := fs.Bool("force", false, "跳过对配置的严格校验（仅告警）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := cf.applyLogLevel(); err != nil {
		return err
	}

	log := logging.New("main")
	log.Info("livemonitor %s 启动中...", Version)

	cfg, err := config.Load(cf.config)
	if err != nil {
		if !*force {
			return err
		}
		log.Warn("配置校验失败，仍尝试启动: %v", err)
	}
	if cfg == nil {
		return err
	}
	cfg.LogSummary(log)

	mgr, err := manager.New(cfg, log)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
	}()

	code := mgr.Run(ctx)
	if code != 0 {
		os.Exit(code)
	}
	_ = time.Now
	return nil
}
