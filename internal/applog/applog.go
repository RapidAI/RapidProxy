// Package applog 提供一个带环形缓冲与订阅能力的日志器。
//
// 日志同时写入内存（供界面展示）、文件（便于排查）与订阅者（实时推送到界面）。
package applog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger 是线程安全的日志器。
type Logger struct {
	mu       sync.Mutex
	lines    []string
	max      int
	nextID   int
	subs     map[int]func(string)
	file     *os.File
	filePath string
}

// New 创建日志器；dir 为空时不落盘。
func New(dir string, max int) *Logger {
	if max <= 0 {
		max = 500
	}
	l := &Logger{max: max, subs: map[int]func(string){}}
	if dir != "" {
		logDir := filepath.Join(dir, "logs")
		if err := os.MkdirAll(logDir, 0o755); err == nil {
			path := filepath.Join(logDir, "proxy.log")
			if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				l.file = f
				l.filePath = path
			}
		}
	}
	return l
}

// FilePath 返回日志文件路径。
func (l *Logger) FilePath() string { return l.filePath }

// Infof 记录一条普通日志。
func (l *Logger) Infof(format string, args ...any) { l.write("INFO", fmt.Sprintf(format, args...)) }

// Warnf 记录一条告警日志。
func (l *Logger) Warnf(format string, args ...any) { l.write("WARN", fmt.Sprintf(format, args...)) }

// Errorf 记录一条错误日志。
func (l *Logger) Errorf(format string, args ...any) { l.write("ERROR", fmt.Sprintf(format, args...)) }

func (l *Logger) write(level, msg string) {
	line := fmt.Sprintf("%s [%s] %s", time.Now().Format("2006-01-02 15:04:05"), level, msg)

	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > l.max {
		l.lines = l.lines[len(l.lines)-l.max:]
	}
	file := l.file
	subs := make([]func(string), 0, len(l.subs))
	for _, fn := range l.subs {
		subs = append(subs, fn)
	}
	l.mu.Unlock()

	if file != nil {
		_, _ = file.WriteString(line + "\n")
	}
	for _, fn := range subs {
		fn(line)
	}
}

// Subscribe 订阅新日志，返回取消订阅函数。
func (l *Logger) Subscribe(fn func(string)) func() {
	l.mu.Lock()
	id := l.nextID
	l.nextID++
	l.subs[id] = fn
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.subs, id)
		l.mu.Unlock()
	}
}

// Lines 返回最近的日志行。
func (l *Logger) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// Tail 返回最近 n 行日志。
func (l *Logger) Tail(n int) []string {
	lines := l.Lines()
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// Clear 清空内存中的日志（不影响日志文件）。
func (l *Logger) Clear() {
	l.mu.Lock()
	l.lines = nil
	l.mu.Unlock()
}

// Close 关闭日志文件。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
