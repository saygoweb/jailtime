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

	if c.Engine.TargetLatency.Duration <= 0 {
		return fmt.Errorf("engine: target_latency must be > 0")
	}
	if c.Engine.PerfWindow < 1 {
		return fmt.Errorf("engine: perf_window must be >= 1")
	}

	// Build a combined name set for collision detection between jails and whitelists.
	allNames := make(map[string]struct{}, len(c.Jails)+len(c.Whitelists))

	if err := validateJails(c.Jails, allNames, false); err != nil {
		return err
	}
	if err := validateJails(c.Whitelists, allNames, true); err != nil {
		return err
	}
	return nil
}

func validateJails(jails []JailConfig, names map[string]struct{}, isWhitelist bool) error {
	kind := "jail"
	if isWhitelist {
		kind = "whitelist"
	}
	for i, j := range jails {
		if j.Name == "" {
			return fmt.Errorf("%s[%d]: name must not be empty", kind, i)
		}
		if _, seen := names[j.Name]; seen {
			return fmt.Errorf("%s[%d]: duplicate name %q (names must be unique across jails and whitelists)", kind, i, j.Name)
		}
		names[j.Name] = struct{}{}

		if len(j.Files) == 0 {
			return fmt.Errorf("%s %q: must have at least one file glob", kind, j.Name)
		}
		if len(j.Filters) == 0 {
			return fmt.Errorf("%s %q: must have at least one filter regex", kind, j.Name)
		}

		if j.WatchMode != "tail" && j.WatchMode != "static" {
			return fmt.Errorf("%s %q: watch_mode must be \"tail\" or \"static\", got %q", kind, j.Name, j.WatchMode)
		}

		if j.WatchMode == "tail" {
			// Tail-mode jails require threshold-based fields.
			if len(j.Actions.OnAdd) == 0 {
				return fmt.Errorf("%s %q: actions.on_add (or deprecated on_match) cannot be empty", kind, j.Name)
			}
			if j.FindTime.Duration <= 0 {
				return fmt.Errorf("%s %q: find_time must be > 0", kind, j.Name)
			}
			if j.JailTime.Duration <= 0 {
				return fmt.Errorf("%s %q: jail_time must be > 0", kind, j.Name)
			}
			if j.HitCount < 1 {
				return fmt.Errorf("%s %q: hit_count must be >= 1", kind, j.Name)
			}
		} else {
			// Static-mode: on_add is optional (membership list only), but
			// find_time/jail_time/hit_count must not be set.
			if j.FindTime.Duration != 0 {
				return fmt.Errorf("%s %q: find_time must not be set for watch_mode: static", kind, j.Name)
			}
			if j.JailTime.Duration != 0 {
				return fmt.Errorf("%s %q: jail_time must not be set for watch_mode: static", kind, j.Name)
			}
			if j.HitCount != 0 {
				return fmt.Errorf("%s %q: hit_count must not be set for watch_mode: static", kind, j.Name)
			}
		}

		if j.NetType != "IP" && j.NetType != "CIDR" {
			return fmt.Errorf("%s %q: net_type must be \"IP\" or \"CIDR\", got %q", kind, j.Name, j.NetType)
		}

		if j.LabelFrom != "" && j.LabelFrom != "match" && j.LabelFrom != "parent_dir" {
			return fmt.Errorf("%s %q: label_from must be \"match\" or \"parent_dir\", got %q", kind, j.Name, j.LabelFrom)
		}

		for k, f := range j.Filters {
			if _, err := regexp.Compile(f); err != nil {
				return fmt.Errorf("%s %q: filter[%d] %q: invalid regex: %w", kind, j.Name, k, f, err)
			}
		}
		for k, f := range j.ExcludeFilters {
			if _, err := regexp.Compile(f); err != nil {
				return fmt.Errorf("%s %q: exclude_filter[%d] %q: invalid regex: %w", kind, j.Name, k, f, err)
			}
		}
	}
	return nil
}
