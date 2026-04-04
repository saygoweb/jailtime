package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestDurationParsing(t *testing.T) {
	valid := []struct {
		s    string
		want time.Duration
	}{
		{"300ms", 300 * time.Millisecond},
		{"10s", 10 * time.Second},
		{"5m", 5 * time.Minute},
		{"2h", 2 * time.Hour},
		{"1d", 24 * time.Hour},
		{"1w", 168 * time.Hour},
	}
	for _, tc := range valid {
		got, err := parseDuration(tc.s)
		if err != nil {
			t.Errorf("parseDuration(%q) unexpected error: %v", tc.s, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}

	invalid := []string{"", "abc", "1x"}
	for _, s := range invalid {
		if _, err := parseDuration(s); err == nil {
			t.Errorf("parseDuration(%q) expected error, got nil", s)
		}
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

const minimalValidYAML = `
version: 1
jails:
  - name: sshd
    files:
      - /var/log/auth.log
    filters:
      - 'Failed password for .* from (?P<ip>[0-9a-fA-F:\.]+)'
    actions:
      on_match:
        - 'nft add element inet filter blacklist { {{ .IP }} }'
    hit_count: 5
    find_time: 10m
    jail_time: 1h
`

func TestLoadValid(t *testing.T) {
	path := writeTemp(t, minimalValidYAML)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if c.Version != 1 {
		t.Errorf("Version = %d, want 1", c.Version)
	}
	if len(c.Jails) != 1 {
		t.Fatalf("len(Jails) = %d, want 1", len(c.Jails))
	}
	if c.Jails[0].Name != "sshd" {
		t.Errorf("Jails[0].Name = %q, want \"sshd\"", c.Jails[0].Name)
	}
	if c.Jails[0].FindTime.Duration != 10*time.Minute {
		t.Errorf("FindTime = %v, want 10m", c.Jails[0].FindTime.Duration)
	}
	if c.Jails[0].JailTime.Duration != time.Hour {
		t.Errorf("JailTime = %v, want 1h", c.Jails[0].JailTime.Duration)
	}
}

func TestLoadInvalidMissingOnMatch(t *testing.T) {
	yaml := strings.Replace(minimalValidYAML, "on_match:\n        - 'nft add element inet filter blacklist { {{ .IP }} }'", "on_match: []", 1)
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty on_match, got nil")
	}
}

func TestLoadInvalidBadRegex(t *testing.T) {
	yaml := strings.Replace(minimalValidYAML, "'Failed password for .* from (?P<ip>[0-9a-fA-F:\\.]+)'", "'[invalid'", 1)
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
}

func TestLoadInvalidNetType(t *testing.T) {
	yaml := minimalValidYAML + "    net_type: INVALID\n"
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid net_type, got nil")
	}
}

func TestLoadDefaults(t *testing.T) {
	path := writeTemp(t, minimalValidYAML)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !c.Engine.ReadFromEnd {
		t.Error("ReadFromEnd default should be true")
	}
	if !c.Jails[0].Enabled {
		t.Error("Enabled default should be true")
	}
	if c.Control.Socket != defaultSocketPath {
		t.Errorf("Socket = %q, want %q", c.Control.Socket, defaultSocketPath)
	}
}
