package config

import (
	"log/slog"
	"time"
)

// GlobalActions holds the shell commands run at daemon start and stop, before
// any jail or whitelist is started / after all have stopped.
type GlobalActions struct {
	OnStart []string `yaml:"on_start"`
	OnStop  []string `yaml:"on_stop"`
}

// Config is the top-level configuration structure.
type Config struct {
	Version    int           `yaml:"version"`
	Include    []string      `yaml:"include"`
	Logging    LoggingConfig `yaml:"logging"`
	Control    ControlConfig `yaml:"control"`
	Engine     EngineConfig  `yaml:"engine"`
	Actions    GlobalActions `yaml:"actions"`
	Jails      []JailConfig  `yaml:"jails"`
	Whitelists []JailConfig  `yaml:"whitelists"`
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
	// SourceFile is the path of the config file from which this jail was loaded.
	// It is set during loading and is not part of the YAML schema.
	SourceFile     string      `yaml:"-"`
	Name           string      `yaml:"name"`
	Enabled        bool        `yaml:"enabled"`
	Files          []string    `yaml:"files"`
	ExcludeFiles   []string    `yaml:"exclude_files"`
	Filters        []string    `yaml:"filters"`
	ExcludeFilters []string    `yaml:"exclude_filters"`
	Actions        JailActions `yaml:"actions"`
	HitCount       int         `yaml:"hit_count"`
	FindTime       Duration    `yaml:"find_time"`
	JailTime       Duration    `yaml:"jail_time"`
	// NetType is "IP" or "CIDR".
	NetType string `yaml:"net_type"`
	// WatchMode is "tail" (default) or "static".
	WatchMode string `yaml:"watch_mode"`
	Query     string `yaml:"query"`
	// QueryBeforeMatch controls whether the query pre-check is run before
	// on_add actions.  When false (the default), the query is never run and
	// on_add is always executed on a threshold hit.  When true, the query is
	// run first; an exit code of 0 suppresses on_add (IP already handled).
	QueryBeforeMatch bool `yaml:"query_before_match"`
	// TagsFrom is an ordered list of tag sources whose resolved values are
	// joined with "," and exposed as {{.Tags}} in action templates and as the
	// "tags" structured log field on every match.
	// Valid sources:
	//   "parent_dir"              – base name of the directory containing the file
	//   "match_tag1"…"match_tag9" – text from (?P<tag1>…)…(?P<tag9>…) in the filter
	// Defaults to empty (no tags).
	TagsFrom []string `yaml:"tags_from"`
	// ActionTimeout is the maximum time allowed for each individual action
	// command (on_add, query).  Defaults to defaultActionTimeout.
	ActionTimeout Duration `yaml:"action_timeout"`
	// IgnoreSets names loaded whitelists whose in-memory IP/CIDR membership
	// suppresses on_add for a matched IP.  Phase 4.
	IgnoreSets []string `yaml:"ignore_sets"`
}

// JailActions holds the shell command templates run at various lifecycle points.
type JailActions struct {
	// OnAdd fires when an IP is newly seen (threshold hit for tail, first
	// appearance in static file).  OnMatch is a deprecated alias for OnAdd.
	OnAdd     []string `yaml:"on_add"`
	OnMatch   []string `yaml:"on_match"`
	OnRemove  []string `yaml:"on_remove"`
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

// DefaultActionTimeout is the exported default for per-action timeouts.
const DefaultActionTimeout = defaultActionTimeout

const defaultTargetLatency = 2 * time.Second
const defaultPerfWindow = 3
