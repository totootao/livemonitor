// Package logging 提供带前缀和级别、线程安全的日志输出。
package logging

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Level 日志级别。
type Level int

// 日志级别定义，数值越大越严重。
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String 返回级别的可读名称。
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// ParseLevel 解析级别名称，无法识别时返回 LevelInfo 和 false。
func ParseLevel(s string) (Level, bool) {
	switch s {
	case "debug", "DEBUG":
		return LevelDebug, true
	case "info", "INFO":
		return LevelInfo, true
	case "warn", "warning", "WARN", "WARNING":
		return LevelWarn, true
	case "error", "ERROR":
		return LevelError, true
	default:
		return LevelInfo, false
	}
}

// Logger 是一个带组件前缀的并发安全日志器。
type Logger struct {
	mu    sync.Mutex
	scope string
	out   io.Writer
	level Level
}

var (
	globalMu    sync.RWMutex
	globalLevel = LevelInfo
)

// SetGlobalLevel 设置全局最低输出级别。
func SetGlobalLevel(l Level) {
	globalMu.Lock()
	globalLevel = l
	globalMu.Unlock()
}

func currentGlobalLevel() Level {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalLevel
}

// New 创建一个带 scope 前缀的日志器，scope 通常为容器名或 "video" 等组件名。
func New(scope string) *Logger {
	return &Logger{scope: scope, out: os.Stdout, level: LevelDebug}
}

// SetOutput 设置输出目标，便于测试。
func (l *Logger) SetOutput(w io.Writer) {
	l.mu.Lock()
	l.out = w
	l.mu.Unlock()
}

func (l *Logger) log(lv Level, format string, args ...any) {
	if lv < currentGlobalLevel() {
		return
	}
	msg := fmt.Sprintf(format, args...)
	ts := time.Now().Format("2006-01-02 15:04:05")
	if l.scope == "" {
		msg = fmt.Sprintf("%s [%s] %s\n", ts, lv, msg)
	} else {
		msg = fmt.Sprintf("%s [%s] [%s] %s\n", ts, lv, l.scope, msg)
	}
	l.mu.Lock()
	_, _ = io.WriteString(l.out, msg)
	l.mu.Unlock()
}

// Debug 输出调试日志。
func (l *Logger) Debug(format string, args ...any) { l.log(LevelDebug, format, args...) }

// Info 输出信息日志。
func (l *Logger) Info(format string, args ...any) { l.log(LevelInfo, format, args...) }

// Warn 输出警告日志。
func (l *Logger) Warn(format string, args ...any) { l.log(LevelWarn, format, args...) }

// Error 输出错误日志。
func (l *Logger) Error(format string, args ...any) { l.log(LevelError, format, args...) }
