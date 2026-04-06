package config

import (
	"fmt"
	"regexp"
)

// Validate checks the config for semantic errors.
func Validate(c *Config) error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d (must be 1)", c.Version)
	}

	if c.Engine.MinLatency.Duration <= 0 {
		return fmt.Errorf("engine: min_latency must be > 0")
	}
	if c.Engine.MaxLatency.Duration <= 0 {
		return fmt.Errorf("engine: max_latency must be > 0")
	}
	if c.Engine.MaxLatency.Duration < c.Engine.MinLatency.Duration {
		return fmt.Errorf("engine: max_latency must be >= min_latency")
	}
	if c.Engine.PerfWindow < 1 {
		return fmt.Errorf("engine: perf_window must be >= 1")
	}

	names := make(map[string]struct{}, len(c.Jails))
	for i, j := range c.Jails {
		if j.Name == "" {
			return fmt.Errorf("jail[%d]: name must not be empty", i)
		}
		if _, seen := names[j.Name]; seen {
			return fmt.Errorf("jail[%d]: duplicate jail name %q", i, j.Name)
		}
		names[j.Name] = struct{}{}

		if len(j.Files) == 0 {
			return fmt.Errorf("jail %q: must have at least one file glob", j.Name)
		}
		if len(j.Filters) == 0 {
			return fmt.Errorf("jail %q: must have at least one filter regex", j.Name)
		}
		if len(j.Actions.OnMatch) == 0 {
			return fmt.Errorf("jail %q: actions.on_match cannot be empty", j.Name)
		}
		if j.FindTime.Duration <= 0 {
			return fmt.Errorf("jail %q: find_time must be > 0", j.Name)
		}
		if j.JailTime.Duration <= 0 {
			return fmt.Errorf("jail %q: jail_time must be > 0", j.Name)
		}
		if j.HitCount < 1 {
			return fmt.Errorf("jail %q: hit_count must be >= 1", j.Name)
		}
		if j.NetType != "IP" && j.NetType != "CIDR" {
			return fmt.Errorf("jail %q: net_type must be \"IP\" or \"CIDR\", got %q", j.Name, j.NetType)
		}

		for k, f := range j.Filters {
			if _, err := regexp.Compile(f); err != nil {
				return fmt.Errorf("jail %q: filter[%d] %q: invalid regex: %w", j.Name, k, f, err)
			}
		}
		for k, f := range j.ExcludeFilters {
			if _, err := regexp.Compile(f); err != nil {
				return fmt.Errorf("jail %q: exclude_filter[%d] %q: invalid regex: %w", j.Name, k, f, err)
			}
		}
	}
	return nil
}
