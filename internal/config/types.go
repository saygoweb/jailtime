package config

import (
	"log/slog"
	"time"
)

// Config is the top-level configuration structure.
type Config struct {
	Version int           `yaml:"version"`
	Include []string      `yaml:"include"`
	Logging LoggingConfig `yaml:"logging"`
	Control ControlConfig `yaml:"control"`
	Engine  EngineConfig  `yaml:"engine"`
	Jails   []JailConfig  `yaml:"jails"`
}

// LoggingConfig controls log output destination and verbosity.
type LoggingConfig struct {
	// Target is "journal" or "file".
	Target string `yaml:"target"`
	File   string `yaml:"file"`
	// Level is "debug", "info", "warn", or "error".
	Level string `yaml:"level"`
}

// ControlConfig configures the Unix-domain control socket.
type ControlConfig struct {
	Socket  string   `yaml:"socket"`
	Timeout Duration `yaml:"timeout"`
}

// EngineConfig controls file-watching behaviour.
type EngineConfig struct {
	// WatcherMode is "auto", "os", "fsnotify", "inotify", or "poll".
	// "inotify" and "os" are aliases for "fsnotify" (uses inotify on Linux).
	WatcherMode   string   `yaml:"watcher_mode"`
	PollInterval  Duration `yaml:"poll_interval"`
	ReadFromEnd   bool     `yaml:"read_from_end"`
	TargetLatency Duration `yaml:"target_latency"`
	PerfWindow    int      `yaml:"perf_window"`
}

// JailConfig defines a single jail rule.
type JailConfig struct {
	Name           string      `yaml:"name"`
	Enabled        bool        `yaml:"enabled"`
	Files          []string    `yaml:"files"`
	Filters        []string    `yaml:"filters"`
	ExcludeFilters []string    `yaml:"exclude_filters"`
	Actions        JailActions `yaml:"actions"`
	HitCount       int         `yaml:"hit_count"`
	FindTime       Duration    `yaml:"find_time"`
	JailTime       Duration    `yaml:"jail_time"`
	// NetType is "IP" or "CIDR".
	NetType string `yaml:"net_type"`
	Query   string `yaml:"query"`
	// QueryBeforeMatch controls whether the query pre-check is run before
	// on_match actions.  When false (the default), the query is never run and
	// on_match is always executed on a threshold hit.  When true, the query is
	// run first; an exit code of 0 suppresses on_match (IP already handled).
	QueryBeforeMatch bool `yaml:"query_before_match"`
	// ActionTimeout is the maximum time allowed for each individual action
	// command (on_match, query).  Defaults to defaultActionTimeout.
	ActionTimeout Duration `yaml:"action_timeout"`
}

// JailActions holds the shell command templates run at various lifecycle points.
type JailActions struct {
	OnMatch   []string `yaml:"on_match"`
	OnStart   []string `yaml:"on_start"`
	OnStop    []string `yaml:"on_stop"`
	OnRestart []string `yaml:"on_restart"`
}

// LogValue implements slog.LogValuer so EngineConfig fields are logged as a
// structured group.
func (e EngineConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("watcher_mode", e.WatcherMode),
		slog.Duration("poll_interval", e.PollInterval.Duration),
		slog.Bool("read_from_end", e.ReadFromEnd),
		slog.Duration("target_latency", e.TargetLatency.Duration),
		slog.Int("perf_window", e.PerfWindow),
	)
}

// defaultReadFromEnd is the sentinel value used to detect whether ReadFromEnd
// was explicitly set in YAML.  We use a pointer trick during loading instead.
const defaultSocketPath = "/run/jailtime/jailtimed.sock"

// defaultPollInterval is the default engine poll interval.
const defaultPollInterval = 2 * time.Second

// defaultActionTimeout is the per-command timeout for on_match and query actions.
const defaultActionTimeout = 30 * time.Second

const defaultTargetLatency = 2 * time.Second
const defaultPerfWindow = 3
