package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sgw/jailtime/internal/config"
	"github.com/sgw/jailtime/internal/watch"
)

// TestHitTrackerNoTrigger verifies that hits below threshold do not trigger.
func TestHitTrackerNoTrigger(t *testing.T) {
	ht := NewHitTracker()
	threshold := 3
	findTime := time.Minute
	now := time.Now()

	for i := 1; i < threshold; i++ {
		count, triggered := ht.Record("192.168.1.1", now.Add(time.Duration(i)*time.Second), findTime, threshold)
		if triggered {
			t.Fatalf("hit %d: expected triggered=false, got true (count=%d)", i, count)
		}
	}
}

// TestHitTrackerTrigger verifies that reaching threshold triggers and resets count.
func TestHitTrackerTrigger(t *testing.T) {
	ht := NewHitTracker()
	threshold := 3
	findTime := time.Minute
	now := time.Now()

	var lastCount int
	var triggered bool
	for i := 1; i <= threshold; i++ {
		lastCount, triggered = ht.Record("10.0.0.1", now.Add(time.Duration(i)*time.Second), findTime, threshold)
	}
	if !triggered {
		t.Fatalf("expected triggered=true at threshold %d, count=%d", threshold, lastCount)
	}
	if lastCount != threshold {
		t.Fatalf("expected count=%d at trigger, got %d", threshold, lastCount)
	}

	// Next hit after reset should not re-trigger with threshold=3.
	count, triggered := ht.Record("10.0.0.1", now.Add(time.Duration(threshold+1)*time.Second), findTime, threshold)
	if triggered {
		t.Fatalf("expected no retrigger after reset, got triggered=true count=%d", count)
	}
	if count != 1 {
		t.Fatalf("expected count=1 after reset, got %d", count)
	}
}

// TestHitTrackerWindowExpiry verifies that the count resets after the window expires.
func TestHitTrackerWindowExpiry(t *testing.T) {
	ht := NewHitTracker()
	threshold := 5
	findTime := 100 * time.Millisecond
	now := time.Now()

	// Record threshold-1 hits inside the window.
	for i := 0; i < threshold-1; i++ {
		_, triggered := ht.Record("172.16.0.1", now, findTime, threshold)
		if triggered {
			t.Fatal("unexpected trigger during fill phase")
		}
	}

	// Advance time past window expiry.
	expired := now.Add(findTime + 10*time.Millisecond)

	// First hit after expiry should reset to count=1.
	count, triggered := ht.Record("172.16.0.1", expired, findTime, threshold)
	if triggered {
		t.Fatalf("expected no trigger after expiry, got triggered=true count=%d", count)
	}
	if count != 1 {
		t.Fatalf("expected count=1 after expiry reset, got %d", count)
	}
}

// TestJailRuntimeConfigFiles verifies that ConfigFiles expands globs and
// returns matching file paths, respecting the limit.
func TestJailRuntimeConfigFiles(t *testing.T) {
	base := t.TempDir()

	// Create two subdirs with access.log, plus one that doesn't match.
	for _, sub := range []string{"site1", "site2"} {
		dir := filepath.Join(base, sub)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "access.log"), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	// A file that should NOT match the glob.
	if err := os.WriteFile(filepath.Join(base, "other.log"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	pattern := filepath.Join(base, "*", "access.log")
	cfg := &config.JailConfig{
		Name:    "apache2",
		Enabled: true,
		Files:   []string{pattern},
		Filters: []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
	}
	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	files := jr.ConfigFiles(0, false)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
	for _, f := range files {
		if filepath.Base(f) != "access.log" {
			t.Errorf("unexpected file %q", f)
		}
	}

	// Test limit.
	limited := jr.ConfigFiles(1, false)
	if len(limited) != 1 {
		t.Fatalf("expected 1 file with limit=1, got %d", len(limited))
	}
}

// TestJailRuntimeConfigFilesNewSubdir verifies that ConfigFiles picks up new
// subdirectories created after the JailRuntime was initialised (glob is
// re-expanded on each call).
func TestJailRuntimeConfigFilesNewSubdir(t *testing.T) {
	base := t.TempDir()
	pattern := filepath.Join(base, "*", "access.log")
	cfg := &config.JailConfig{
		Name:    "apache2",
		Enabled: true,
		Files:   []string{pattern},
		Filters: []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
	}
	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	// No subdirs yet.
	if got := jr.ConfigFiles(0, false); len(got) != 0 {
		t.Fatalf("expected 0 files initially, got %d", len(got))
	}

	// Add a new subdir with a log file.
	dir := filepath.Join(base, "vhost1")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "access.log"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	files := jr.ConfigFiles(0, false)
	if len(files) != 1 {
		t.Fatalf("expected 1 file after adding subdir, got %d: %v", len(files), files)
	}
}

// TestJailRuntimeConfigTest verifies filter testing against a log file.
func TestJailRuntimeConfigTest(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "auth.log")

	lines := []string{
		"Failed password from 1.2.3.4 port 22",   // matches
		"Accepted password from 5.6.7.8 port 22", // matches
		"system boot",                             // no IP, no match
		"Connection from 9.9.9.9 whitelist",       // excluded
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.JailConfig{
		Name:           "ssh",
		Enabled:        true,
		Filters:        []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
		ExcludeFilters: []string{`whitelist`},
	}
	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	total, matching, matches, err := jr.ConfigTest(logFile, 0, true)
	if err != nil {
		t.Fatalf("ConfigTest: %v", err)
	}
	if total != 4 {
		t.Errorf("expected total=4, got %d", total)
	}
	if matching != 2 {
		t.Errorf("expected matching=2, got %d", matching)
	}
	if len(matches) != 2 {
		t.Errorf("expected 2 returned matches, got %d: %v", len(matches), matches)
	}

	// Test limit on returned matches — stats are still full.
	_, _, limitedMatches, err := jr.ConfigTest(logFile, 1, true)
	if err != nil {
		t.Fatalf("ConfigTest (limited): %v", err)
	}
	if len(limitedMatches) != 1 {
		t.Errorf("expected 1 returned match with limit=1, got %d", len(limitedMatches))
	}

	// Without --matching flag, matches slice should be empty.
	_, _, noMatches, err := jr.ConfigTest(logFile, 0, false)
	if err != nil {
		t.Fatalf("ConfigTest (no matching): %v", err)
	}
	if len(noMatches) != 0 {
		t.Errorf("expected no returned matches when returnMatching=false, got %d", len(noMatches))
	}
}


func TestJailRuntimeHandleEvent(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")

	cfg := &config.JailConfig{
		Name:     "test-jail",
		Enabled:  true,
		Filters:  []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
		HitCount: 1,
		FindTime: config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnMatch: []string{"echo {{ .IP }} > " + outFile},
		},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	evt := watch.Event{
		JailName: "test-jail",
		FilePath: "/var/log/auth.log",
		Line:     "Failed password from 1.2.3.4",
		Time:     time.Now(),
	}

	ctx := context.Background()
	if err := jr.HandleEvent(ctx, evt); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	got := strings.TrimSpace(string(data))
	if got != "1.2.3.4" {
		t.Fatalf("expected output %q, got %q", "1.2.3.4", got)
	}
}

// TestJailRuntimeExcludeFilter verifies that exclude filters suppress on_match.
func TestJailRuntimeExcludeFilter(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")

	cfg := &config.JailConfig{
		Name:           "excl-jail",
		Enabled:        true,
		Filters:        []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
		ExcludeFilters: []string{`whitelist`},
		HitCount:       1,
		FindTime:       config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnMatch: []string{"echo {{ .IP }} > " + outFile},
		},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	evt := watch.Event{
		JailName: "excl-jail",
		FilePath: "/var/log/auth.log",
		Line:     "Failed password from 5.6.7.8 whitelist",
		Time:     time.Now(),
	}

	ctx := context.Background()
	if err := jr.HandleEvent(ctx, evt); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	if _, err := os.Stat(outFile); err == nil {
		t.Fatal("on_match should not have fired for excluded line, but output file exists")
	}
}

// TestDebugRateLimiter verifies the rate limiter allows at most maxPerSec
// entries per second and resets correctly after each window.
func TestDebugRateLimiter(t *testing.T) {
base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
mockNow := base

rl := newDebugRateLimiter(2)
rl.now = func() time.Time { return mockNow }

if !rl.Allow() {
t.Fatal("1st call in window should be allowed")
}
if !rl.Allow() {
t.Fatal("2nd call in window should be allowed")
}
if rl.Allow() {
t.Fatal("3rd call in window should be denied")
}

// Advance into a new window.
mockNow = base.Add(time.Second)
if !rl.Allow() {
t.Fatal("1st call after window reset should be allowed")
}
if !rl.Allow() {
t.Fatal("2nd call after window reset should be allowed")
}
if rl.Allow() {
t.Fatal("3rd call after window reset should be denied")
}
}

// TestDebugRateLimiterDisabledLogging verifies HandleEvent still processes
// events correctly when debug logging is disabled (the default).
func TestDebugRateLimiterDisabledLogging(t *testing.T) {
dir := t.TempDir()
outFile := filepath.Join(dir, "output.txt")

cfg := &config.JailConfig{
Name:     "rl-test-jail",
Enabled:  true,
Filters:  []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
HitCount: 1,
FindTime: config.Duration{Duration: time.Minute},
Actions: config.JailActions{
OnMatch: []string{"echo {{ .IP }} > " + outFile},
},
}

jr, err := NewJailRuntime(cfg)
if err != nil {
t.Fatalf("NewJailRuntime: %v", err)
}

// Send more events than the rate limit allows (3 > 2/s).
ctx := context.Background()
for i := 0; i < 3; i++ {
evt := watch.Event{
JailName: "rl-test-jail",
FilePath: "/var/log/auth.log",
Line:     "no-ip line",
Time:     time.Now(),
}
if err := jr.HandleEvent(ctx, evt); err != nil {
t.Fatalf("HandleEvent: %v", err)
}
}
}

// TestHandleEventQuerySuppresses verifies that when the query pre-check returns
// exit 0 (IP already blocked), on_match is NOT executed.
func TestHandleEventQuerySuppresses(t *testing.T) {
dir := t.TempDir()
outFile := filepath.Join(dir, "output.txt")

cfg := &config.JailConfig{
Name:     "query-jail",
Enabled:  true,
Filters:  []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
HitCount: 1,
FindTime: config.Duration{Duration: time.Minute},
// "true" always exits 0 → IP already blocked → skip on_match.
Query: "true",
Actions: config.JailActions{
OnMatch: []string{"echo {{ .IP }} > " + outFile},
},
}

jr, err := NewJailRuntime(cfg)
if err != nil {
t.Fatalf("NewJailRuntime: %v", err)
}

evt := watch.Event{
JailName: "query-jail",
FilePath: "/var/log/auth.log",
Line:     "Failed password from 1.2.3.4",
Time:     time.Now(),
}

ctx := context.Background()
if err := jr.HandleEvent(ctx, evt); err != nil {
t.Fatalf("HandleEvent: %v", err)
}

if _, err := os.Stat(outFile); err == nil {
t.Fatal("on_match should have been suppressed by query exit 0, but output file exists")
}
}

// TestHandleEventQueryPermits verifies that when the query pre-check returns
// non-zero (IP not yet blocked), on_match IS executed.
func TestHandleEventQueryPermits(t *testing.T) {
dir := t.TempDir()
outFile := filepath.Join(dir, "output.txt")

cfg := &config.JailConfig{
Name:     "query-permit-jail",
Enabled:  true,
Filters:  []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
HitCount: 1,
FindTime: config.Duration{Duration: time.Minute},
// "false" exits 1 → IP not yet blocked → proceed with on_match.
Query: "false",
Actions: config.JailActions{
OnMatch: []string{"echo {{ .IP }} > " + outFile},
},
}

jr, err := NewJailRuntime(cfg)
if err != nil {
t.Fatalf("NewJailRuntime: %v", err)
}

evt := watch.Event{
JailName: "query-permit-jail",
FilePath: "/var/log/auth.log",
Line:     "Failed password from 2.3.4.5",
Time:     time.Now(),
}

ctx := context.Background()
if err := jr.HandleEvent(ctx, evt); err != nil {
t.Fatalf("HandleEvent: %v", err)
}

data, err := os.ReadFile(outFile)
if err != nil {
t.Fatalf("on_match should have fired (query exited 1) but output file was not created: %v", err)
}
if got := strings.TrimSpace(string(data)); got != "2.3.4.5" {
t.Fatalf("expected output %q, got %q", "2.3.4.5", got)
}
}

// TestHandleEventIPValidationFails verifies that a filter match whose captured
// group is not a valid IP address is silently dropped (no on_match).
func TestHandleEventIPValidationFails(t *testing.T) {
dir := t.TempDir()
outFile := filepath.Join(dir, "output.txt")

// This filter matches any line but extracts a non-IP token as the "ip" group.
cfg := &config.JailConfig{
Name:     "bad-ip-jail",
Enabled:  true,
Filters:  []string{`word=(?P<ip>[a-z]+)`},
HitCount: 1,
FindTime: config.Duration{Duration: time.Minute},
Actions: config.JailActions{
OnMatch: []string{"echo hit > " + outFile},
},
}

jr, err := NewJailRuntime(cfg)
if err != nil {
t.Fatalf("NewJailRuntime: %v", err)
}

evt := watch.Event{
JailName: "bad-ip-jail",
FilePath: "/var/log/test.log",
Line:     "word=notanip extra stuff",
Time:     time.Now(),
}

ctx := context.Background()
if err := jr.HandleEvent(ctx, evt); err != nil {
t.Fatalf("HandleEvent: %v", err)
}

if _, err := os.Stat(outFile); err == nil {
t.Fatal("on_match should not have fired for invalid IP, but output file exists")
}
}
