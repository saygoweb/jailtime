package config

import "time"

// Config is the top-level configuration structure.
type Config struct {
	Version int           `yaml:"version"`
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
	// WatcherMode is "auto", "os", "fsnotify", or "poll".
	WatcherMode  string   `yaml:"watcher_mode"`
	PollInterval Duration `yaml:"poll_interval"`
	ReadFromEnd  bool     `yaml:"read_from_end"`
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
}

// JailActions holds the shell command templates run at various lifecycle points.
type JailActions struct {
	OnMatch   []string `yaml:"on_match"`
	OnStart   []string `yaml:"on_start"`
	OnStop    []string `yaml:"on_stop"`
	OnRestart []string `yaml:"on_restart"`
}

// defaultReadFromEnd is the sentinel value used to detect whether ReadFromEnd
// was explicitly set in YAML.  We use a pointer trick during loading instead.
const defaultSocketPath = "/run/jailtime/jailtimed.sock"

// defaultPollInterval is the default engine poll interval.
const defaultPollInterval = 2 * time.Second
