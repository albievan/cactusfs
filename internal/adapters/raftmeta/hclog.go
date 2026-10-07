package raftmeta

import (
	"context"
	"io"
	"log"
	"log/slog"
	"strings"

	"github.com/hashicorp/go-hclog"
)

// slogLogger adapts a *slog.Logger to the hclog.Logger interface that
// hashicorp/raft logs through, so that Raft's messages come out in the same
// JSON stream as everything else.
type slogLogger struct {
	logger *slog.Logger
	name   string
	args   []any
}

var _ hclog.Logger = (*slogLogger)(nil)

func newHCLogger(logger *slog.Logger) hclog.Logger {
	return &slogLogger{logger: logger}
}

func slogLevel(level hclog.Level) slog.Level {
	switch level {
	case hclog.Trace:
		return slog.LevelDebug - 4
	case hclog.Debug:
		return slog.LevelDebug
	case hclog.Warn:
		return slog.LevelWarn
	case hclog.Error:
		return slog.LevelError
	case hclog.NoLevel, hclog.Info, hclog.Off:
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}

// Log implements hclog.Logger.
func (l *slogLogger) Log(level hclog.Level, msg string, args ...any) {
	if level == hclog.Off {
		return
	}
	l.logger.Log(context.Background(), slogLevel(level), msg, args...)
}

// Trace implements hclog.Logger.
func (l *slogLogger) Trace(msg string, args ...any) { l.Log(hclog.Trace, msg, args...) }

// Debug implements hclog.Logger.
func (l *slogLogger) Debug(msg string, args ...any) { l.Log(hclog.Debug, msg, args...) }

// Info implements hclog.Logger.
func (l *slogLogger) Info(msg string, args ...any) { l.Log(hclog.Info, msg, args...) }

// Warn implements hclog.Logger.
func (l *slogLogger) Warn(msg string, args ...any) { l.Log(hclog.Warn, msg, args...) }

// Error implements hclog.Logger.
func (l *slogLogger) Error(msg string, args ...any) { l.Log(hclog.Error, msg, args...) }

func (l *slogLogger) enabled(level hclog.Level) bool {
	return l.logger.Enabled(context.Background(), slogLevel(level))
}

// IsTrace implements hclog.Logger.
func (l *slogLogger) IsTrace() bool { return l.enabled(hclog.Trace) }

// IsDebug implements hclog.Logger.
func (l *slogLogger) IsDebug() bool { return l.enabled(hclog.Debug) }

// IsInfo implements hclog.Logger.
func (l *slogLogger) IsInfo() bool { return l.enabled(hclog.Info) }

// IsWarn implements hclog.Logger.
func (l *slogLogger) IsWarn() bool { return l.enabled(hclog.Warn) }

// IsError implements hclog.Logger.
func (l *slogLogger) IsError() bool { return l.enabled(hclog.Error) }

// ImpliedArgs implements hclog.Logger.
func (l *slogLogger) ImpliedArgs() []any { return l.args }

// With implements hclog.Logger.
func (l *slogLogger) With(args ...any) hclog.Logger {
	return &slogLogger{
		logger: l.logger.With(args...),
		name:   l.name,
		args:   append(append([]any(nil), l.args...), args...),
	}
}

// Name implements hclog.Logger.
func (l *slogLogger) Name() string { return l.name }

// Named implements hclog.Logger.
func (l *slogLogger) Named(name string) hclog.Logger {
	if l.name != "" {
		name = l.name + "." + name
	}
	return l.ResetNamed(name)
}

// ResetNamed implements hclog.Logger.
func (l *slogLogger) ResetNamed(name string) hclog.Logger {
	return &slogLogger{logger: l.logger.With(slog.String("component", name)), name: name, args: l.args}
}

// SetLevel implements hclog.Logger. The level is owned by the slog handler.
func (l *slogLogger) SetLevel(hclog.Level) {}

// GetLevel implements hclog.Logger.
func (l *slogLogger) GetLevel() hclog.Level {
	switch {
	case l.IsTrace():
		return hclog.Trace
	case l.IsDebug():
		return hclog.Debug
	case l.IsInfo():
		return hclog.Info
	case l.IsWarn():
		return hclog.Warn
	default:
		return hclog.Error
	}
}

// StandardLogger implements hclog.Logger.
func (l *slogLogger) StandardLogger(opts *hclog.StandardLoggerOptions) *log.Logger {
	return log.New(l.StandardWriter(opts), "", 0)
}

// StandardWriter implements hclog.Logger.
func (l *slogLogger) StandardWriter(*hclog.StandardLoggerOptions) io.Writer {
	return lineWriter{logger: l}
}

// lineWriter logs every write as one INFO message.
type lineWriter struct {
	logger *slogLogger
}

// Write implements io.Writer.
func (w lineWriter) Write(p []byte) (int, error) {
	w.logger.Info(strings.TrimRight(string(p), "\r\n"))
	return len(p), nil
}
