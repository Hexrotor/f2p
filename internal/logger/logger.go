package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

var logger *slog.Logger

type CustomHandler struct {
	level slog.Level
	w     io.Writer
	loc   *time.Location
}

func NewCustomHandler(w io.Writer, level slog.Level) *CustomHandler {
	// Determine timezone location: prefer LOG_TZ, then TZ, fallback to local
	loc := time.Local
	if tz := os.Getenv("LOG_TZ"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	} else if tz := os.Getenv("TZ"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return &CustomHandler{
		level: level,
		w:     w,
		loc:   loc,
	}
}

func (h *CustomHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *CustomHandler) Handle(_ context.Context, r slog.Record) error {
	// Use configured timezone and show only numeric offset (e.g., +08:00)
	timeStr := r.Time.In(h.loc).Format("2006-01-02 15:04:05 Z07:00")

	var levelStr string
	var colorCode string
	switch r.Level {
	case slog.LevelDebug:
		levelStr = "DEBUG"
		colorCode = "\033[36m" // 青色 (Cyan)
	case slog.LevelInfo:
		levelStr = "INFO"
		colorCode = "\033[34m" // 偏黑的蓝色 (Blue)
	case slog.LevelWarn:
		levelStr = "WARN"
		colorCode = "\033[33m" // 黄色 (Yellow)
	case slog.LevelError:
		levelStr = "ERROR"
		colorCode = "\033[31m" // 红色 (Red)
	default:
		levelStr = r.Level.String()
		colorCode = "\033[37m" // 白色 (White)
	}

	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("%s[%s] [%s] %s\033[0m", colorCode, timeStr, levelStr, r.Message))

	r.Attrs(func(a slog.Attr) bool {
		if a.Key != "" {
			msg.WriteString(fmt.Sprintf(" \033[90m(%s: %v)\033[0m", a.Key, a.Value)) // 深灰色属性
		}
		return true
	})

	msg.WriteString("\n")

	_, err := h.w.Write([]byte(msg.String()))
	return err
}

func (h *CustomHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *CustomHandler) WithGroup(name string) slog.Handler {
	return h
}

func InitLogger(level string) {
	var logLevel slog.Level

	switch strings.ToLower(level) {
	case "debug":
		logLevel = slog.LevelDebug
	case "info":
		logLevel = slog.LevelInfo
	case "warn", "warning":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	handler := NewCustomHandler(os.Stdout, logLevel)
	logger = slog.New(handler)
	slog.SetDefault(logger)
}