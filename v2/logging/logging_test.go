package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestNewMatchesZapProductionShape(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := Named(New(buf, slog.LevelInfo), "api")

	before := time.Now()
	logger.Warn("dispatch failed", "err", errors.New("boom"), "took", 1500*time.Millisecond, "account_id", 352)

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("decoding %q: %s", buf.String(), err)
	}

	want := map[string]any{
		"level":      "warn",
		"msg":        "dispatch failed",
		"logger":     "api",
		"err":        "boom",
		"took":       1.5,
		"account_id": float64(352),
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("expected %s=%v, got %v", k, v, line[k])
		}
	}

	ts, ok := line["ts"].(float64)
	if !ok || ts < float64(before.Unix()) || ts > float64(time.Now().Unix()+1) {
		t.Errorf("expected ts as epoch seconds, got %v", line["ts"])
	}
	if caller, _ := line["caller"].(string); !strings.HasPrefix(caller, "logging/logging_test.go:") {
		t.Errorf("expected short caller, got %v", line["caller"])
	}
	for _, slogKey := range []string{"time", "source"} {
		if _, ok := line[slogKey]; ok {
			t.Errorf("unexpected slog key %q in %v", slogKey, line)
		}
	}
}

func TestNewRespectsLevel(t *testing.T) {
	buf := &bytes.Buffer{}
	level := &slog.LevelVar{}
	level.Set(slog.LevelInfo)
	logger := New(buf, level)

	logger.Debug("hidden")
	level.Set(slog.LevelDebug)
	logger.Debug("shown")

	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, `"level":"debug"`) {
		t.Errorf("unexpected output %q", out)
	}
}

func TestDurationsInGroupsAreSeconds(t *testing.T) {
	buf := &bytes.Buffer{}
	New(buf, slog.LevelInfo).Info("x", slog.Group("kafka", slog.Duration("flush", 250*time.Millisecond)))

	if !strings.Contains(buf.String(), `"kafka":{"flush":0.25}`) {
		t.Errorf("expected grouped duration in seconds, got %q", buf.String())
	}
}
