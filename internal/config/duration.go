package config

import (
	"fmt"
	"strings"
	"time"
)

// Duration wraps time.Duration to support custom YAML unmarshaling, including
// the non-standard suffixes "d" (day = 24h) and "w" (week = 168h).
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	parsed, err := parseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("duration string is empty")
	}

	// Handle "d" and "w" suffixes by converting to hours before parsing.
	if strings.HasSuffix(s, "w") {
		num := s[:len(s)-1]
		var weeks float64
		if _, err := fmt.Sscanf(num, "%f", &weeks); err != nil || num == "" {
			return 0, fmt.Errorf("invalid duration %q: cannot parse weeks", s)
		}
		return time.Duration(weeks * float64(168*time.Hour)), nil
	}
	if strings.HasSuffix(s, "d") {
		num := s[:len(s)-1]
		var days float64
		if _, err := fmt.Sscanf(num, "%f", &days); err != nil || num == "" {
			return 0, fmt.Errorf("invalid duration %q: cannot parse days", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}

	dur, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return dur, nil
}
