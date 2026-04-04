package logging

import (
	"fmt"
	"log/slog"
	"os"
)

const (
	TargetJournal = "journal"
	TargetFile    = "file"
)

// Config holds logging configuration.
type Config struct {
	Target string
	File   string
	Level  string
}

// Setup initializes the global slog logger based on cfg.
// For "journal" target: writes to os.Stdout with a text handler.
// For "file" target: opens the file in append mode and writes JSON.
// Returns a cleanup func (closes file if opened) and any error.
func Setup(cfg Config) (func(), error) {
	level := parseLevel(cfg.Level)
	opts := &slog.HandlerOptions{Level: level}

	switch cfg.Target {
	case TargetFile:
		f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return func() {}, fmt.Errorf("logging: open file %q: %w", cfg.File, err)
		}
		slog.SetDefault(slog.New(slog.NewJSONHandler(f, opts)))
		return func() { f.Close() }, nil

	default: // TargetJournal and anything else
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, opts)))
		return func() {}, nil
	}
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
