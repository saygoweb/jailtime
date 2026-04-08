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
		"system boot",                            // no IP, no match
		"Connection from 9.9.9.9 whitelist",      // excluded
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
			OnAdd: []string{"echo {{ .IP }} > " + outFile},
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
	jr.WaitForInflight()

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
			OnAdd: []string{"echo {{ .IP }} > " + outFile},
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
			OnAdd: []string{"echo {{ .IP }} > " + outFile},
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

// TestHandleEventQuerySuppresses verifies that when query_before_match is true
// and the query pre-check returns exit 0 (IP already blocked), on_match is NOT executed.
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
		Query:            "true",
		QueryBeforeMatch: true,
		Actions: config.JailActions{
			OnAdd: []string{"echo {{ .IP }} > " + outFile},
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
	jr.WaitForInflight() // ensure goroutine ran and query suppressed on_match

	if _, err := os.Stat(outFile); err == nil {
		t.Fatal("on_match should have been suppressed by query exit 0, but output file exists")
	}
}

// TestHandleEventQueryPermits verifies that when query_before_match is true
// and the query pre-check returns non-zero (IP not yet blocked), on_match IS executed.
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
		Query:            "false",
		QueryBeforeMatch: true,
		Actions: config.JailActions{
			OnAdd: []string{"echo {{ .IP }} > " + outFile},
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
	jr.WaitForInflight()

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("on_match should have fired (query exited 1) but output file was not created: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "2.3.4.5" {
		t.Fatalf("expected output %q, got %q", "2.3.4.5", got)
	}
}

// TestHandleEventQueryNotRunWhenDisabled verifies that when query_before_match is
// false (the default), the query is never run — even if a query command is set —
// and on_match fires unconditionally on a threshold hit.
func TestHandleEventQueryNotRunWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")

	cfg := &config.JailConfig{
		Name:     "query-disabled-jail",
		Enabled:  true,
		Filters:  []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
		HitCount: 1,
		FindTime: config.Duration{Duration: time.Minute},
		// "true" exits 0, which would suppress on_match — but QueryBeforeMatch is
		// false (default) so the query must not be run at all.
		Query:            "true",
		QueryBeforeMatch: false,
		Actions: config.JailActions{
			OnAdd: []string{"echo {{ .IP }} > " + outFile},
		},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	evt := watch.Event{
		JailName: "query-disabled-jail",
		FilePath: "/var/log/auth.log",
		Line:     "Failed password from 3.4.5.6",
		Time:     time.Now(),
	}

	ctx := context.Background()
	if err := jr.HandleEvent(ctx, evt); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	jr.WaitForInflight()

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("on_match should have fired (query_before_match=false) but output file was not created: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "3.4.5.6" {
		t.Fatalf("expected output %q, got %q", "3.4.5.6", got)
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
			OnAdd: []string{"echo hit > " + outFile},
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

// TestHandleEventInflightSkip verifies that a concurrent second threshold trigger
// for the same IP is skipped while the first on_match action is still running.
func TestHandleEventInflightSkip(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count.txt")

	// Action sleeps briefly to simulate a slow command (e.g. WHOIS lookup),
	// then appends a line to countFile so we can count executions.
	cfg := &config.JailConfig{
		Name:     "inflight-jail",
		Enabled:  true,
		Filters:  []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
		HitCount: 1,
		FindTime: config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnAdd: []string{"sleep 0.2 && echo hit >> " + countFile},
		},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	evt := watch.Event{
		JailName: "inflight-jail",
		FilePath: "/var/log/auth.log",
		Line:     "Failed password from 7.7.7.7",
		Time:     time.Now(),
	}

	ctx := context.Background()

	// Fire three concurrent HandleEvent calls for the same IP.  Each one
	// re-records the hit (HitCount=1 threshold, so each triggers on_match),
	// but only the first should actually run the action.
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			errs <- jr.HandleEvent(ctx, evt)
		}()
	}
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			t.Errorf("HandleEvent[%d] unexpected error: %v", i, err)
		}
	}

	// Wait for the in-flight action to finish.
	jr.WaitForInflight()

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("countFile not created — no on_match ran: %v", err)
	}
	lines := strings.Count(strings.TrimSpace(string(data)), "hit")
	if lines != 1 {
		t.Fatalf("expected exactly 1 on_match execution, got %d (countFile: %q)", lines, string(data))
	}
}

// TestHandleEventInflightPreventsBatchRetrigger verifies that sequential
// HandleEvent calls — as occur in timer-based processBatch — cannot re-trigger
// on_match for the same IP while the first action goroutine is still running.
//
// This is the production failure mode: processBatch processes events serially;
// with a synchronous action the old code cleared inflight before the next event
// was reached, allowing immediate re-triggers on transient failures.
func TestHandleEventInflightPreventsBatchRetrigger(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count.txt")

	// Slow action (200ms) so the goroutine is definitely still in flight when
	// the second sequential HandleEvent call is made.
	cfg := &config.JailConfig{
		Name:     "batch-retrigger-jail",
		Enabled:  true,
		Filters:  []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
		HitCount: 1,
		FindTime: config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnAdd: []string{"sleep 0.2 && echo hit >> " + countFile},
		},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	ctx := context.Background()
	now := time.Now()

	// Event 1: triggers threshold (count 0→1, threshold=1), action starts.
	evt1 := watch.Event{
		JailName: cfg.Name,
		FilePath: "/var/log/auth.log",
		Line:     "Failed password from 7.7.7.7",
		Time:     now,
	}
	if err := jr.HandleEvent(ctx, evt1); err != nil {
		t.Fatalf("HandleEvent 1: %v", err)
	}

	// Event 2: same IP, called immediately after (simulating the next item in
	// the same processBatch loop). With async actions, the goroutine from
	// event 1 is still running; inflight must block this trigger.
	evt2 := watch.Event{
		JailName: cfg.Name,
		FilePath: "/var/log/auth.log",
		Line:     "Failed password from 7.7.7.7",
		Time:     now.Add(time.Millisecond),
	}
	if err := jr.HandleEvent(ctx, evt2); err != nil {
		t.Fatalf("HandleEvent 2: %v", err)
	}

	jr.WaitForInflight()

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("countFile not created — no on_match ran: %v", err)
	}
	hits := strings.Count(strings.TrimSpace(string(data)), "hit")
	if hits != 1 {
		t.Fatalf("expected exactly 1 on_match execution, got %d (countFile: %q)", hits, string(data))
	}
}

// TestHandleEventInflightDifferentIPs verifies that concurrent on_match actions
// for different IPs are NOT blocked by each other.
func TestHandleEventInflightDifferentIPs(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.JailConfig{
		Name:     "inflight-multi-jail",
		Enabled:  true,
		Filters:  []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
		HitCount: 1,
		FindTime: config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnAdd: []string{"sleep 0.1 && echo {{ .IP }} >> " + filepath.Join(dir, "out.txt")},
		},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	ctx := context.Background()
	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	errs := make(chan error, len(ips))
	for _, ip := range ips {
		ip := ip
		go func() {
			errs <- jr.HandleEvent(ctx, watch.Event{
				JailName: "inflight-multi-jail",
				FilePath: "/var/log/auth.log",
				Line:     "Failed password from " + ip,
				Time:     time.Now(),
			})
		}()
	}
	for i := 0; i < len(ips); i++ {
		if err := <-errs; err != nil {
			t.Errorf("HandleEvent error: %v", err)
		}
	}

	time.Sleep(300 * time.Millisecond)
	jr.WaitForInflight()

	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	got := strings.TrimSpace(string(data))
	for _, ip := range ips {
		if !strings.Contains(got, ip) {
			t.Errorf("expected IP %s in output, but it was missing:\n%s", ip, got)
		}
	}
}

// TestActionRunnerDrop verifies that a second Submit for the same IP while the
// first is still in flight returns false (duplicate dropped) and the fn runs
// exactly once.
func TestActionRunnerDrop(t *testing.T) {
	var r ActionRunner

	ran := make(chan struct{}, 2)
	done := make(chan struct{})

	// First submit: slow fn that blocks until we close done.
	if !r.Submit("1.2.3.4", func() {
		ran <- struct{}{}
		<-done
	}) {
		t.Fatal("first Submit should return true")
	}

	// Wait until fn has started (to ensure inflight entry is set).
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("first fn did not start within 1s")
	}

	// Second submit while first is still in flight — must be dropped.
	if r.Submit("1.2.3.4", func() {
		ran <- struct{}{}
	}) {
		t.Fatal("second Submit should return false (duplicate in flight)")
	}

	close(done) // unblock first fn
	r.Wait()

	// Only one execution should have happened (ran has one buffered item already read).
	select {
	case <-ran:
		t.Fatal("expected no more items in ran channel — fn ran more than once")
	default:
	}
}

// TestActionRunnerSequential verifies that after the first Submit completes,
// the same IP can be submitted again and the fn runs a second time.
func TestActionRunnerSequential(t *testing.T) {
	var r ActionRunner

	count := 0
	fn := func() { count++ }

	if !r.Submit("1.2.3.4", fn) {
		t.Fatal("first Submit should return true")
	}
	r.Wait()

	if !r.Submit("1.2.3.4", fn) {
		t.Fatal("second Submit (after first completed) should return true")
	}
	r.Wait()

	if count != 2 {
		t.Fatalf("expected fn to run 2 times, got %d", count)
	}
}

// ---- Tests for static watch_mode (EventAdded / EventRemoved) ----

func TestHandleEventStaticAdded(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "added.txt")

	cfg := &config.JailConfig{
		Name:      "wl",
		Enabled:   true,
		WatchMode: "static",
		Files:     []string{"/tmp/wl.txt"},
		Filters:   []string{`(?P<ip>[0-9.]+)`},
		NetType:   "IP",
		Actions: config.JailActions{
			OnAdd: []string{"echo add-{{ .IP }} > " + outFile},
		},
		ActionTimeout: config.Duration{Duration: 5 * time.Second},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	ctx := context.Background()
	if err := jr.HandleEvent(ctx, watch.Event{
		JailName: "wl",
		FilePath: "/tmp/wl.txt",
		Line:     "1.2.3.4",
		Kind:     watch.EventAdded,
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	jr.WaitForInflight()

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("on_add output not created: %v", err)
	}
	if !strings.Contains(string(data), "add-1.2.3.4") {
		t.Errorf("output = %q, want to contain \"add-1.2.3.4\"", string(data))
	}

	// Verify IsMember works.
	if !jr.IsMember("1.2.3.4") {
		t.Error("IsMember(1.2.3.4) should be true after EventAdded")
	}
}

func TestHandleEventStaticRemoved(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "removed.txt")

	cfg := &config.JailConfig{
		Name:      "wl",
		Enabled:   true,
		WatchMode: "static",
		Files:     []string{"/tmp/wl.txt"},
		Filters:   []string{`(?P<ip>[0-9.]+)`},
		NetType:   "IP",
		Actions: config.JailActions{
			OnRemove: []string{"echo remove-{{ .IP }} > " + outFile},
		},
		ActionTimeout: config.Duration{Duration: 5 * time.Second},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}
	ctx := context.Background()

	// First add the IP so it can be removed.
	if err := jr.HandleEvent(ctx, watch.Event{
		Line: "1.2.3.4",
		Kind: watch.EventAdded,
	}); err != nil {
		t.Fatalf("HandleEvent Added: %v", err)
	}
	jr.WaitForInflight()

	if !jr.IsMember("1.2.3.4") {
		t.Fatal("IsMember should be true after EventAdded")
	}

	// Remove it.
	if err := jr.HandleEvent(ctx, watch.Event{
		JailName: "wl",
		FilePath: "/tmp/wl.txt",
		Line:     "1.2.3.4",
		Kind:     watch.EventRemoved,
	}); err != nil {
		t.Fatalf("HandleEvent Removed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("on_remove output not created: %v", err)
	}
	if !strings.Contains(string(data), "remove-1.2.3.4") {
		t.Errorf("output = %q, want to contain \"remove-1.2.3.4\"", string(data))
	}

	if jr.IsMember("1.2.3.4") {
		t.Error("IsMember(1.2.3.4) should be false after EventRemoved")
	}
}

func TestIsMemberCIDR(t *testing.T) {
	cfg := &config.JailConfig{
		Name:          "wl",
		Enabled:       true,
		WatchMode:     "static",
		Files:         []string{"/tmp/wl.txt"},
		Filters:       []string{`(?P<ip>[0-9./]+)`},
		NetType:       "CIDR",
		ActionTimeout: config.Duration{Duration: 5 * time.Second},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}
	ctx := context.Background()

	// Add a CIDR.
	if err := jr.HandleEvent(ctx, watch.Event{
		Line: "192.168.1.0/24",
		Kind: watch.EventAdded,
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	jr.WaitForInflight()

	if !jr.IsMember("192.168.1.100") {
		t.Error("192.168.1.100 should match 192.168.1.0/24")
	}
	if jr.IsMember("10.0.0.1") {
		t.Error("10.0.0.1 should NOT match 192.168.1.0/24")
	}
}

// ---- Tests for ignore_sets (Phase 4) ----

func TestIgnoreSetsSuppress(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")

	cfg := &config.JailConfig{
		Name:      "test-jail",
		Enabled:   true,
		WatchMode: "tail",
		Files:     []string{"/var/log/auth.log"},
		Filters:   []string{`Failed password from (?P<ip>[0-9.]+)`},
		NetType:   "IP",
		HitCount:  1,
		FindTime:  config.Duration{Duration: time.Minute},
		JailTime:  config.Duration{Duration: time.Hour},
		Actions: config.JailActions{
			OnAdd: []string{"echo {{ .IP }} >> " + outFile},
		},
		ActionTimeout: config.Duration{Duration: 5 * time.Second},
		IgnoreSets:    []string{"whitelist"},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	// Inject an ignore-set checker that blocks 1.2.3.4.
	jr.mu.Lock()
	jr.ignoreSetsChecker = func(ip string) bool {
		return ip == "1.2.3.4"
	}
	jr.mu.Unlock()

	ctx := context.Background()

	// Blocked IP: should be suppressed.
	if err := jr.HandleEvent(ctx, watch.Event{
		JailName: "test-jail",
		FilePath: "/var/log/auth.log",
		Line:     "Failed password from 1.2.3.4",
		Time:     time.Now(),
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	jr.WaitForInflight()

	if _, err := os.Stat(outFile); err == nil {
		data, _ := os.ReadFile(outFile)
		if strings.Contains(string(data), "1.2.3.4") {
			t.Error("on_add should have been suppressed by ignore_sets for 1.2.3.4")
		}
	}

	// Non-whitelisted IP: should fire.
	if err := jr.HandleEvent(ctx, watch.Event{
		JailName: "test-jail",
		FilePath: "/var/log/auth.log",
		Line:     "Failed password from 5.6.7.8",
		Time:     time.Now(),
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	jr.WaitForInflight()

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("on_add should have fired for non-whitelisted IP: %v", err)
	}
	if !strings.Contains(string(data), "5.6.7.8") {
		t.Errorf("expected 5.6.7.8 in output, got %q", string(data))
	}
}

// TestHandleEventCIDRPlainIPNormalized verifies that a plain IP extracted when
// net_type is CIDR gets normalised to IP/32 and the on_match action still fires.
func TestHandleEventCIDRPlainIPNormalized(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")

	cfg := &config.JailConfig{
		Name:    "cidr-jail",
		Enabled: true,
		// Pattern matches both CIDR (e.g. 1.2.3.4/24) and plain IPs (e.g. 1.2.3.4).
		Filters:  []string{`(?P<ip>[0-9]{1,3}(?:\.[0-9]{1,3}){3}(?:/\d{1,2})?)$`},
		NetType:  "CIDR",
		HitCount: 1,
		FindTime: config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnAdd: []string{"echo {{ .IP }} >> " + outFile},
		},
		ActionTimeout: config.Duration{Duration: 5 * time.Second},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	ctx := context.Background()

	// Send a plain IP (no /nn); should be normalised to /32 and trigger.
	if err := jr.HandleEvent(ctx, watch.Event{
		JailName: cfg.Name,
		FilePath: "/var/log/test.log",
		Line:     "blocked 10.0.0.5",
		Time:     time.Now(),
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	jr.WaitForInflight()

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("output file not created — on_match did not fire: %v", err)
	}
	got := strings.TrimSpace(string(data))
	if got != "10.0.0.5/32" {
		t.Errorf("expected IP in output to be 10.0.0.5/32, got %q", got)
	}
}

// TestHandleEventCIDRWithSlashNormalized verifies that a CIDR like 192.168.1.0/24
// extracted when net_type is CIDR still works unchanged.
func TestHandleEventCIDRWithSlashNormalized(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")

	cfg := &config.JailConfig{
		Name:     "cidr-jail2",
		Enabled:  true,
		Filters:  []string{`(?P<ip>[0-9]{1,3}(?:\.[0-9]{1,3}){3}(?:/\d{1,2})?)$`},
		NetType:  "CIDR",
		HitCount: 1,
		FindTime: config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnAdd: []string{"echo {{ .IP }} >> " + outFile},
		},
		ActionTimeout: config.Duration{Duration: 5 * time.Second},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	ctx := context.Background()

	if err := jr.HandleEvent(ctx, watch.Event{
		JailName: cfg.Name,
		FilePath: "/var/log/test.log",
		Line:     "blocked 192.168.1.0/24",
		Time:     time.Now(),
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	jr.WaitForInflight()

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("output file not created — on_match did not fire: %v", err)
	}
	got := strings.TrimSpace(string(data))
	if got != "192.168.1.0/24" {
		t.Errorf("expected IP in output to be 192.168.1.0/24, got %q", got)
	}
}

// TestResolveLabelMatch verifies that resolveLabel returns the filter-captured
// label when label_from is "match" or empty.
func TestResolveLabelMatch(t *testing.T) {
	for _, labelFrom := range []string{"", "match"} {
		got := resolveLabel(labelFrom, "captured", "/var/log/svc/access.log")
		if got != "captured" {
			t.Errorf("resolveLabel(%q) = %q, want %q", labelFrom, got, "captured")
		}
	}
}

// TestResolveLabelParentDir verifies that resolveLabel returns the parent
// directory name of the file when label_from is "parent_dir".
func TestResolveLabelParentDir(t *testing.T) {
	got := resolveLabel("parent_dir", "captured", "/var/log/apache2/some-domain.com/access.log")
	if got != "some-domain.com" {
		t.Errorf("resolveLabel(parent_dir) = %q, want %q", got, "some-domain.com")
	}
}

// TestHandleEventLabelFromMatch verifies that with label_from unset the Label
// template variable is populated from the (?P<label>...) capture group.
func TestHandleEventLabelFromMatch(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")

	cfg := &config.JailConfig{
		Name:     "label-match-jail",
		Enabled:  true,
		Filters:  []string{`(?P<ip>\d+\.\d+\.\d+\.\d+) (?P<label>\S+)`},
		HitCount: 1,
		FindTime: config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnAdd: []string{"echo {{ .Label }} > " + outFile},
		},
		ActionTimeout: config.Duration{Duration: 5 * time.Second},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	if err := jr.HandleEvent(context.Background(), watch.Event{
		JailName: cfg.Name,
		FilePath: "/var/log/app.log",
		Line:     "Failed from 1.2.3.4 webapp",
		Time:     time.Now(),
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	jr.WaitForInflight()

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "webapp" {
		t.Errorf("Label = %q, want %q", got, "webapp")
	}
}

// TestHandleEventLabelFromParentDir verifies that with label_from: parent_dir
// the Label template variable is populated with the parent directory name.
func TestHandleEventLabelFromParentDir(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")

	cfg := &config.JailConfig{
		Name:      "label-parentdir-jail",
		Enabled:   true,
		LabelFrom: "parent_dir",
		Filters:   []string{`(?P<ip>\d+\.\d+\.\d+\.\d+)`},
		HitCount:  1,
		FindTime:  config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnAdd: []string{"echo {{ .Label }} > " + outFile},
		},
		ActionTimeout: config.Duration{Duration: 5 * time.Second},
	}

	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	if err := jr.HandleEvent(context.Background(), watch.Event{
		JailName: cfg.Name,
		FilePath: "/var/log/apache2/some-domain.com/access.log",
		Line:     "Failed from 1.2.3.4",
		Time:     time.Now(),
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	jr.WaitForInflight()

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "some-domain.com" {
		t.Errorf("Label = %q, want %q", got, "some-domain.com")
	}
}

