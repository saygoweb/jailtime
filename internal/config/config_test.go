package config

import (
	"log/slog"
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

func TestLoadQueryBeforeMatchDefault(t *testing.T) {
	path := writeTemp(t, minimalValidYAML)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if c.Jails[0].QueryBeforeMatch {
		t.Error("QueryBeforeMatch should default to false when not set")
	}
}

func TestLoadQueryBeforeMatchTrue(t *testing.T) {
	y := minimalValidYAML + "    query: /usr/local/bin/ipset-test-cidr.sh mySet\n    query_before_match: true\n"
	path := writeTemp(t, y)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !c.Jails[0].QueryBeforeMatch {
		t.Error("QueryBeforeMatch should be true when set to true in YAML")
	}
}

func TestLoadQueryBeforeMatchFalseExplicit(t *testing.T) {
	y := minimalValidYAML + "    query: /usr/local/bin/ipset-test-cidr.sh mySet\n    query_before_match: false\n"
	path := writeTemp(t, y)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if c.Jails[0].QueryBeforeMatch {
		t.Error("QueryBeforeMatch should be false when explicitly set to false in YAML")
	}
}

const jailFragmentYAML = `
jails:
  - name: nginx
    files:
      - /var/log/nginx/access.log
    filters:
      - 'invalid user .* from (?P<ip>[0-9\.]+)'
    actions:
      on_match:
        - 'iptables -I INPUT -s {{ .IP }} -j DROP'
    hit_count: 3
    find_time: 5m
    jail_time: 30m
`

func TestLoadIncludeGlob(t *testing.T) {
	dir := t.TempDir()

	// Write a fragment file into a jails.d subdirectory.
	jailsDir := dir + "/jails.d"
	if err := os.Mkdir(jailsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fragPath := jailsDir + "/nginx.yaml"
	if err := os.WriteFile(fragPath, []byte(jailFragmentYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write main config that includes the fragment via glob.
	mainYAML := minimalValidYAML + "include:\n  - jails.d/*.yaml\n"
	mainPath := dir + "/jail.yaml"
	if err := os.WriteFile(mainPath, []byte(mainYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(mainPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(c.Jails) != 2 {
		t.Fatalf("len(Jails) = %d, want 2", len(c.Jails))
	}
	names := map[string]bool{c.Jails[0].Name: true, c.Jails[1].Name: true}
	if !names["sshd"] || !names["nginx"] {
		t.Errorf("unexpected jail names: %v", names)
	}
}

func TestLoadIncludeAbsoluteGlob(t *testing.T) {
	dir := t.TempDir()

	fragPath := dir + "/extra.yaml"
	if err := os.WriteFile(fragPath, []byte(jailFragmentYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	mainYAML := minimalValidYAML + "include:\n  - " + dir + "/*.yaml\n"
	mainPath := dir + "/jail.yaml"
	if err := os.WriteFile(mainPath, []byte(mainYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(mainPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(c.Jails) != 2 {
		t.Fatalf("len(Jails) = %d, want 2 (sshd + nginx)", len(c.Jails))
	}
}

func TestLoadIncludeNoMatch(t *testing.T) {
	// A glob that matches nothing should be silently skipped.
	mainYAML := minimalValidYAML + "include:\n  - jails.d/*.yaml\n"
	path := writeTemp(t, mainYAML)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(c.Jails) != 1 {
		t.Errorf("len(Jails) = %d, want 1 (no extra files matched)", len(c.Jails))
	}
}

func TestLoadIncludeDuplicateName(t *testing.T) {
	dir := t.TempDir()

	// Fragment defines a jail named "sshd" — same as the main config.
	dupYAML := strings.Replace(jailFragmentYAML, "name: nginx", "name: sshd", 1)
	fragPath := dir + "/dup.yaml"
	if err := os.WriteFile(fragPath, []byte(dupYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	mainYAML := minimalValidYAML + "include:\n  - " + dir + "/*.yaml\n"
	mainPath := dir + "/jail.yaml"
	if err := os.WriteFile(mainPath, []byte(mainYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(mainPath)
	if err == nil {
		t.Fatal("expected error for duplicate jail name, got nil")
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

func TestEngineConfigLogValue(t *testing.T) {
cfg := EngineConfig{
WatcherMode:  "fsnotify",
PollInterval: Duration{Duration: 5 * time.Second},
ReadFromEnd:  true,
}

val := cfg.LogValue()
if val.Kind() != slog.KindGroup {
t.Fatalf("expected KindGroup, got %v", val.Kind())
}

attrs := val.Group()
m := make(map[string]slog.Value, len(attrs))
for _, a := range attrs {
m[a.Key] = a.Value
}

if got := m["watcher_mode"].String(); got != "fsnotify" {
t.Errorf("watcher_mode = %q, want %q", got, "fsnotify")
}
if got := m["poll_interval"].Duration(); got != 5*time.Second {
t.Errorf("poll_interval = %v, want 5s", got)
}
if got := m["read_from_end"].Bool(); !got {
t.Error("read_from_end should be true")
}
}
