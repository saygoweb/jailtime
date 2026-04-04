package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// rawEngineConfig is used during YAML parsing to detect unset booleans.
type rawEngineConfig struct {
	WatcherMode  string   `yaml:"watcher_mode"`
	PollInterval Duration `yaml:"poll_interval"`
	ReadFromEnd  *bool    `yaml:"read_from_end"`
}

// rawJailConfig mirrors JailConfig with pointer booleans to detect unset fields.
type rawJailConfig struct {
	Name           string      `yaml:"name"`
	Enabled        *bool       `yaml:"enabled"`
	Files          []string    `yaml:"files"`
	Filters        []string    `yaml:"filters"`
	ExcludeFilters []string    `yaml:"exclude_filters"`
	Actions        JailActions `yaml:"actions"`
	HitCount       int         `yaml:"hit_count"`
	FindTime       Duration    `yaml:"find_time"`
	JailTime       Duration    `yaml:"jail_time"`
	NetType        string      `yaml:"net_type"`
	Query          string      `yaml:"query"`
}

// rawConfig mirrors Config but uses raw sub-types to allow default detection.
type rawConfig struct {
	Version int              `yaml:"version"`
	Logging LoggingConfig    `yaml:"logging"`
	Control ControlConfig    `yaml:"control"`
	Engine  rawEngineConfig  `yaml:"engine"`
	Jails   []rawJailConfig  `yaml:"jails"`
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

	c := &Config{
		Version: raw.Version,
		Logging: raw.Logging,
		Control: raw.Control,
		Engine: EngineConfig{
			WatcherMode:  raw.Engine.WatcherMode,
			PollInterval: raw.Engine.PollInterval,
		},
	}

	for _, rj := range raw.Jails {
		jc := JailConfig{
			Name:           rj.Name,
			Files:          rj.Files,
			Filters:        rj.Filters,
			ExcludeFilters: rj.ExcludeFilters,
			Actions:        rj.Actions,
			HitCount:       rj.HitCount,
			FindTime:       rj.FindTime,
			JailTime:       rj.JailTime,
			NetType:        rj.NetType,
			Query:          rj.Query,
		}
		if rj.Enabled == nil {
			jc.Enabled = true
		} else {
			jc.Enabled = *rj.Enabled
		}
		c.Jails = append(c.Jails, jc)
	}

	applyDefaults(c, raw.Engine.ReadFromEnd)

	if err := Validate(c); err != nil {
		return nil, err
	}
	return c, nil
}

func applyDefaults(c *Config, readFromEnd *bool) {
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
	for i := range c.Jails {
		if c.Jails[i].NetType == "" {
			c.Jails[i].NetType = "IP"
		}
	}
}
