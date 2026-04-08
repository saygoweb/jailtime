package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// rawEngineConfig is used during YAML parsing to detect unset booleans.
type rawEngineConfig struct {
	WatcherMode   string   `yaml:"watcher_mode"`
	PollInterval  Duration `yaml:"poll_interval"`
	ReadFromEnd   *bool    `yaml:"read_from_end"`
	TargetLatency Duration `yaml:"target_latency"`
	PerfWindow    *int     `yaml:"perf_window"`
}

// rawJailConfig mirrors JailConfig with pointer booleans to detect unset fields.
type rawJailConfig struct {
	Name             string      `yaml:"name"`
	Enabled          *bool       `yaml:"enabled"`
	Files            []string    `yaml:"files"`
	ExcludeFiles     []string    `yaml:"exclude_files"`
	Filters          []string    `yaml:"filters"`
	ExcludeFilters   []string    `yaml:"exclude_filters"`
	Actions          JailActions `yaml:"actions"`
	HitCount         int         `yaml:"hit_count"`
	FindTime         Duration    `yaml:"find_time"`
	JailTime         Duration    `yaml:"jail_time"`
	NetType          string      `yaml:"net_type"`
	WatchMode        string      `yaml:"watch_mode"`
	Query            string      `yaml:"query"`
	QueryBeforeMatch *bool       `yaml:"query_before_match"`
	ActionTimeout    Duration    `yaml:"action_timeout"`
	IgnoreSets       []string    `yaml:"ignore_sets"`
}

// rawConfig mirrors Config but uses raw sub-types to allow default detection.
type rawConfig struct {
	Version    int             `yaml:"version"`
	Include    []string        `yaml:"include"`
	Logging    LoggingConfig   `yaml:"logging"`
	Control    ControlConfig   `yaml:"control"`
	Engine     rawEngineConfig `yaml:"engine"`
	Actions    GlobalActions   `yaml:"actions"`
	Jails      []rawJailConfig `yaml:"jails"`
	Whitelists []rawJailConfig `yaml:"whitelists"`
}

// rawFragmentFile is the schema for included fragment files, which may define
// jails, whitelists, or both.
type rawFragmentFile struct {
	Jails      []rawJailConfig `yaml:"jails"`
	Whitelists []rawJailConfig `yaml:"whitelists"`
}

// Load reads the YAML config at path, applies defaults, validates, and returns
// the parsed *Config or an error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	// Expand include globs and merge jails/whitelists from fragment files.
	for _, pattern := range raw.Include {
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(filepath.Dir(path), pattern)
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("expanding include glob %q: %w", pattern, err)
		}
		for _, inc := range matches {
			// Never re-include the main config file itself.
			if inc == path {
				continue
			}
			extraJails, extraWhitelists, err := loadFragmentFile(inc)
			if err != nil {
				return nil, fmt.Errorf("include %q: %w", inc, err)
			}
			raw.Jails = append(raw.Jails, extraJails...)
			raw.Whitelists = append(raw.Whitelists, extraWhitelists...)
		}
	}

	c := &Config{
		Version: raw.Version,
		Include: raw.Include,
		Logging: raw.Logging,
		Control: raw.Control,
		Engine: EngineConfig{
			WatcherMode:   raw.Engine.WatcherMode,
			PollInterval:  raw.Engine.PollInterval,
			TargetLatency: raw.Engine.TargetLatency,
		},
		Actions: raw.Actions,
	}

	for _, rj := range raw.Jails {
		c.Jails = append(c.Jails, buildJailConfig(rj))
	}
	for _, rj := range raw.Whitelists {
		c.Whitelists = append(c.Whitelists, buildJailConfig(rj))
	}

	applyDefaults(c, raw.Engine.ReadFromEnd, raw.Engine.PerfWindow)

	if err := Validate(c); err != nil {
		return nil, err
	}
	return c, nil
}

// buildJailConfig converts a rawJailConfig to a JailConfig, applying
// OnMatch→OnAdd deprecation alias and pointer-bool defaults.
func buildJailConfig(rj rawJailConfig) JailConfig {
	actions := rj.Actions
	// OnMatch is a deprecated alias for OnAdd; merge at load time.
	if len(actions.OnAdd) == 0 && len(actions.OnMatch) > 0 {
		slog.Warn("on_match is deprecated; please rename to on_add", "jail", rj.Name)
		actions.OnAdd = actions.OnMatch
	}

	jc := JailConfig{
		Name:           rj.Name,
		Files:          rj.Files,
		ExcludeFiles:   rj.ExcludeFiles,
		Filters:        rj.Filters,
		ExcludeFilters: rj.ExcludeFilters,
		Actions:        actions,
		HitCount:       rj.HitCount,
		FindTime:       rj.FindTime,
		JailTime:       rj.JailTime,
		NetType:        rj.NetType,
		WatchMode:      rj.WatchMode,
		Query:          rj.Query,
		ActionTimeout:  rj.ActionTimeout,
		IgnoreSets:     rj.IgnoreSets,
	}
	if rj.Enabled == nil {
		jc.Enabled = true
	} else {
		jc.Enabled = *rj.Enabled
	}
	if rj.QueryBeforeMatch != nil {
		jc.QueryBeforeMatch = *rj.QueryBeforeMatch
	}
	return jc
}

// loadFragmentFile loads a fragment YAML file and returns its raw jail and
// whitelist configs.
func loadFragmentFile(path string) (jails []rawJailConfig, whitelists []rawJailConfig, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading file: %w", err)
	}
	var f rawFragmentFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, nil, fmt.Errorf("parsing file: %w", err)
	}
	return f.Jails, f.Whitelists, nil
}

func applyDefaults(c *Config, readFromEnd *bool, perfWindow *int) {
	if c.Control.Socket == "" {
		c.Control.Socket = defaultSocketPath
	}
	if c.Engine.WatcherMode == "" {
		c.Engine.WatcherMode = "auto"
	}
	if c.Engine.PollInterval.Duration == 0 {
		c.Engine.PollInterval.Duration = defaultPollInterval
	}
	if readFromEnd == nil {
		c.Engine.ReadFromEnd = true
	} else {
		c.Engine.ReadFromEnd = *readFromEnd
	}
	if c.Engine.TargetLatency.Duration == 0 {
		c.Engine.TargetLatency.Duration = defaultTargetLatency
	}
	if perfWindow == nil {
		c.Engine.PerfWindow = defaultPerfWindow
	} else {
		c.Engine.PerfWindow = *perfWindow
	}
	applyJailDefaults(c.Jails)
	applyJailDefaults(c.Whitelists)
}

func applyJailDefaults(jails []JailConfig) {
	for i := range jails {
		if jails[i].NetType == "" {
			jails[i].NetType = "IP"
		}
		if jails[i].WatchMode == "" {
			jails[i].WatchMode = "tail"
		}
		if jails[i].ActionTimeout.Duration == 0 {
			jails[i].ActionTimeout.Duration = defaultActionTimeout
		}
	}
}
