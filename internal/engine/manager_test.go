package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sgw/jailtime/internal/config"
	"github.com/sgw/jailtime/internal/watch"
)

// TestSortedKeys verifies that sortedKeys orders by source filename first, then
// by jail name as a tiebreaker for jails in the same file.
func TestSortedKeys(t *testing.T) {
	// All keys with no source file — falls back to name order.
	m := map[string]*JailRuntime{
		"zebra":  {cfg: &config.JailConfig{Name: "zebra"}},
		"alpha":  {cfg: &config.JailConfig{Name: "alpha"}},
		"middle": {cfg: &config.JailConfig{Name: "middle"}},
	}
	got := sortedKeys(m)
	want := []string{"alpha", "middle", "zebra"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sortedKeys (no source file) = %v, want %v", got, want)
	}

	// Keys with distinct source files — ordered by filename, not by name.
	m2 := map[string]*JailRuntime{
		"stripe":      {cfg: &config.JailConfig{Name: "stripe", SourceFile: "/etc/jailtime/jails/010-stripe.yaml"}},
		"apache":      {cfg: &config.JailConfig{Name: "apache", SourceFile: "/etc/jailtime/jails/020-apache.yaml"}},
		"uptimerobot": {cfg: &config.JailConfig{Name: "uptimerobot", SourceFile: "/etc/jailtime/jails/005-uptimerobot.yaml"}},
	}
	got2 := sortedKeys(m2)
	want2 := []string{"uptimerobot", "stripe", "apache"}
	if fmt.Sprint(got2) != fmt.Sprint(want2) {
		t.Errorf("sortedKeys (by source file) = %v, want %v", got2, want2)
	}
}

// makeMinimalJailCfg creates a minimal enabled JailConfig that appends the jail
// name to outFile when started (on_start action).
func makeMinimalJailCfg(name, logFile, outFile string) *config.JailConfig {
	return &config.JailConfig{
		Name:    name,
		Enabled: true,
		Files:   []string{logFile},
		Filters: []string{`(?P<ip>[0-9.]+)`},
		Actions: config.JailActions{
			OnStart: []string{fmt.Sprintf("echo %s >> %s", name, outFile)},
			OnAdd:   []string{"echo {{ .IP }}"},
		},
		HitCount:      1,
		FindTime:      config.Duration{Duration: time.Minute},
		JailTime:      config.Duration{Duration: time.Hour},
		NetType:       "IP",
		WatchMode:     "tail",
		ActionTimeout: config.Duration{Duration: 5 * time.Second},
	}
}

// TestGlobalOnStartRunsBeforeJails verifies that global on_start actions execute
// before any jail on_start action.
func TestGlobalOnStartRunsBeforeJails(t *testing.T) {
	dir := t.TempDir()
	outFile := dir + "/order.txt"
	logFile := dir + "/app.log"
	// Create the log file so the jail glob matches.
	if err := os.WriteFile(logFile, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Actions: config.GlobalActions{
			OnStart: []string{fmt.Sprintf("echo global >> %s", outFile)},
		},
		Engine: config.EngineConfig{
			WatcherMode:   "poll",
			PollInterval:  config.Duration{Duration: 50 * time.Millisecond},
			ReadFromEnd:   true,
			TargetLatency: config.Duration{Duration: 50 * time.Millisecond},
			PerfWindow:    1,
		},
	}

	jailCfg := makeMinimalJailCfg("alpha", logFile, outFile)
	jr, err := NewJailRuntime(jailCfg)
	if err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		cfg:        cfg,
		jails:      map[string]*JailRuntime{"alpha": jr},
		whitelists: map[string]*JailRuntime{},
		backend:    watch.NewAuto("poll", 50*time.Millisecond),
		perf:       NewPerfMetrics(50*time.Millisecond, 1, ""),
	}
	m.currentInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- m.Run(ctx)
	}()
	// Allow enough time for on_start actions to fire.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines in output, got: %v", lines)
	}
	if lines[0] != "global" {
		t.Errorf("first line = %q, want \"global\" (global on_start must fire before jail on_start)", lines[0])
	}
	if lines[1] != "alpha" {
		t.Errorf("second line = %q, want \"alpha\"", lines[1])
	}
}

// TestJailsStartInFilenameOrder verifies that jails without a source file fall
// back to name order, and that jails with distinct source files start in
// source-filename order regardless of jail name.
func TestJailsStartInFilenameOrder(t *testing.T) {
	dir := t.TempDir()
	outFile := dir + "/order.txt"
	logFile := dir + "/app.log"
	if err := os.WriteFile(logFile, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Engine: config.EngineConfig{
			WatcherMode:   "poll",
			PollInterval:  config.Duration{Duration: 50 * time.Millisecond},
			ReadFromEnd:   true,
			TargetLatency: config.Duration{Duration: 50 * time.Millisecond},
			PerfWindow:    1,
		},
	}

	// Jail names are reverse-alphabetical but source files are numbered so that
	// the expected startup order is stripe → apache → uptimerobot.
	type entry struct {
		name string
		file string
	}
	entries := []entry{
		{"uptimerobot", "/etc/jailtime/jails/030-uptimerobot.yaml"},
		{"stripe", "/etc/jailtime/jails/010-stripe.yaml"},
		{"apache", "/etc/jailtime/jails/020-apache.yaml"},
	}

	jails := map[string]*JailRuntime{}
	for _, e := range entries {
		jailCfg := makeMinimalJailCfg(e.name, logFile, outFile)
		jailCfg.SourceFile = e.file
		jr, err := NewJailRuntime(jailCfg)
		if err != nil {
			t.Fatal(err)
		}
		jails[e.name] = jr
	}

	m := &Manager{
		cfg:        cfg,
		jails:      jails,
		whitelists: map[string]*JailRuntime{},
		backend:    watch.NewAuto("poll", 50*time.Millisecond),
		perf:       NewPerfMetrics(50*time.Millisecond, 1, ""),
	}
	m.currentInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- m.Run(ctx)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"stripe", "apache", "uptimerobot"}
	if fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Errorf("jail startup order = %v, want %v", lines, want)
	}
}

// TestJailsStartInAlphabeticalOrder verifies that jails without a source file
// fall back to alphabetical name order.
func TestJailsStartInAlphabeticalOrder(t *testing.T) {
	dir := t.TempDir()
	outFile := dir + "/order.txt"
	logFile := dir + "/app.log"
	if err := os.WriteFile(logFile, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Engine: config.EngineConfig{
			WatcherMode:   "poll",
			PollInterval:  config.Duration{Duration: 50 * time.Millisecond},
			ReadFromEnd:   true,
			TargetLatency: config.Duration{Duration: 50 * time.Millisecond},
			PerfWindow:    1,
		},
	}

	jails := map[string]*JailRuntime{}
	for _, name := range []string{"zebra", "alpha", "middle"} {
		jailCfg := makeMinimalJailCfg(name, logFile, outFile)
		jr, err := NewJailRuntime(jailCfg)
		if err != nil {
			t.Fatal(err)
		}
		jails[name] = jr
	}

	m := &Manager{
		cfg:        cfg,
		jails:      jails,
		whitelists: map[string]*JailRuntime{},
		backend:    watch.NewAuto("poll", 50*time.Millisecond),
		perf:       NewPerfMetrics(50*time.Millisecond, 1, ""),
	}
	m.currentInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- m.Run(ctx)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"alpha", "middle", "zebra"}
	if fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Errorf("jail startup order = %v, want %v", lines, want)
	}
}

// as the elapsed time between successive drain calls.
func TestManagerCurrentInterval(t *testing.T) {
	m := &Manager{
		perf: NewPerfMetrics(3, 1, ""),
	}

	ctx := context.Background()
	first := time.Now()

	// First processDrain call: lastDrainAt is zero so currentInterval is not set.
	m.processDrain(ctx, []watch.RawLine{})
	if m.lastDrainAt.IsZero() {
		t.Fatal("lastDrainAt should be set after first processDrain")
	}

	// Simulate a gap of ~50ms.
	time.Sleep(50 * time.Millisecond)

	// Second processDrain call: currentInterval should reflect elapsed time.
	m.processDrain(ctx, []watch.RawLine{})
	elapsed := time.Since(first)

	if m.currentInterval <= 0 {
		t.Fatalf("currentInterval should be > 0 after second processDrain, got %v", m.currentInterval)
	}
	if m.currentInterval > elapsed+10*time.Millisecond {
		t.Errorf("currentInterval %v seems too large (elapsed %v)", m.currentInterval, elapsed)
	}
}
