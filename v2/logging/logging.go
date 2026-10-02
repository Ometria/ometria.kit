// Package logging provides the standard Ometria *slog.Logger.
//
// Its JSON output has the same shape as zap's production config, which v1
// services used, so log queries and dashboards keep working when a service
// moves from zap to slog:
//
//	{"level":"info","ts":1790936009.34,"caller":"api/push.go:42","msg":"request","logger":"api","duration":0.0012}
package logging

import (
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// NameKey is the attribute Named sets, matching zap's logger name field.
const NameKey = "logger"

// New returns a logger that writes JSON lines to w for records at level or
// above. Pass a *slog.LevelVar as level to change it at runtime.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(NewHandler(w, level))
}

// NewHandler returns the slog.Handler behind New, for wrapping or composing
// with other handlers.
func NewHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		AddSource:   true,
		Level:       level,
		ReplaceAttr: replaceAttr,
	})
}

// Named returns a child logger tagged with name, like zap's Logger.Named.
// Unlike zap, names don't nest: call it once per logger, on the root logger.
func Named(l *slog.Logger, name string) *slog.Logger {
	return l.With(NameKey, name)
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindDuration {
		// zap encodes durations as float seconds; slog as int nanoseconds.
		return slog.Float64(a.Key, a.Value.Duration().Seconds())
	}
	if len(groups) > 0 {
		return a
	}

	switch a.Key {
	case slog.TimeKey:
		t := a.Value.Time()
		return slog.Float64("ts", float64(t.UnixNano())/float64(time.Second))
	case slog.LevelKey:
		return slog.String(slog.LevelKey, strings.ToLower(a.Value.String()))
	case slog.SourceKey:
		src, ok := a.Value.Any().(*slog.Source)
		if !ok || src.File == "" {
			return slog.Attr{}
		}
		short := filepath.Join(filepath.Base(filepath.Dir(src.File)), filepath.Base(src.File))
		return slog.String("caller", short+":"+strconv.Itoa(src.Line))
	}
	return a
}
