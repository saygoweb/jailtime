package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sgw/jailtime/internal/config"
	"github.com/sgw/jailtime/internal/watch"
)

// apacheWPLines are real Apache access-log lines from a WordPress wp-login.php
// brute-force scenario.  51.103.25.59 appears twice; all others appear once.
var apacheWPLines = []string{
	`31.24.155.180 - - [04/Apr/2026:00:08:21 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "https://rexshort.co.nz/wp-login.php" "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_5) AppleWebKit/600.8.9 (KHTML, like Gecko) Version/10.1.2 Safari/603.3.8"`,
	`50.6.229.148 - - [04/Apr/2026:00:19:22 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "https://rexshort.co.nz/wp-login.php" "Mozilla/5.0 (Windows NT 6.1; WOW64; Trident/7.0; NISSC; rv:11.0) like Gecko"`,
	`94.46.170.34 - - [04/Apr/2026:00:30:52 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "https://rexshort.co.nz/wp-login.php" "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_13_2) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/65.0.3325.181 Safari/537.36"`,
	`69.167.171.193 - - [04/Apr/2026:00:41:50 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "https://rexshort.co.nz/wp-login.php" "Mozilla/5.0 (Windows NT 6.1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/67.0.3396.87 Safari/537.36"`,
	`162.215.219.73 - - [04/Apr/2026:00:53:10 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "https://rexshort.co.nz/wp-login.php" "Mozilla/5.0 (Windows NT 6.1; WOW64; Trident/7.0; MDDCJS; rv:11.0) like Gecko"`,
	`51.103.25.59 - - [04/Apr/2026:01:04:24 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "https://rexshort.co.nz/wp-login.php" "Mozilla/5.0 (Windows NT 6.3; Win64; x64; Trident/7.0; MDDCJS; rv:11.0) like Gecko"`,
	`51.103.25.59 - - [04/Apr/2026:01:26:51 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "https://rexshort.co.nz/wp-login.php" "Mozilla/5.0 (Windows NT 5.1; WOW64; Trident/7.0; rv:11.0) like Gecko"`,
	`15.161.25.29 - - [04/Apr/2026:01:38:06 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "https://rexshort.co.nz/wp-login.php" "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_11_6) AppleWebKit/604.1.38 (KHTML, like Gecko) Version/11.0 Safari/604.1.38"`,
	`41.76.215.184 - - [04/Apr/2026:02:23:31 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "https://rexshort.co.nz/wp-login.php" "Mozilla/5.0 (Windows NT 10.0; WOW64; Trident/7.0; MAGWJS; rv:11.0) like Gecko"`,
	`64.91.240.67 - - [04/Apr/2026:02:34:48 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "https://rexshort.co.nz/wp-login.php" "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12) AppleWebKit/604.4.7 (KHTML, like Gecko) Version/10.0 Safari/602.1.31"`,
}

// apacheWPNonMatchingLines are lines from the same vhost that should NOT match
// the wp-login filter (GET requests, non-PHP paths, static assets).
var apacheWPNonMatchingLines = []string{
	`51.103.25.59 - - [04/Apr/2026:01:00:00 +0000] "GET / HTTP/1.1" 200 4096 "-" "Mozilla/5.0"`,
	`51.103.25.59 - - [04/Apr/2026:01:01:00 +0000] "GET /wp-content/themes/style.css HTTP/1.1" 200 1234 "-" "Mozilla/5.0"`,
	`10.0.0.1 - - [04/Apr/2026:01:02:00 +0000] "POST /xmlrpc.php HTTP/1.1" 403 512 "-" "curl/7.68"`,
}

// apacheWPFilter matches client IPs from Apache combined-log lines where the
// request is POST /wp-login.php.
const apacheWPFilter = `^(?P<ip>\S+)\s+\S+\s+\S+\s+\[.*\]\s+"POST /wp-login\.php HTTP`

// newApacheWPJailConfig returns a JailConfig that mirrors a real
// apache-wordpress jail.  outFile receives one "echo <IP>" per trigger.
func newApacheWPJailConfig(logGlob, outFile string, hitCount int) *config.JailConfig {
	return &config.JailConfig{
		Name:     "apache-wordpress",
		Enabled:  true,
		Files:    []string{logGlob},
		Filters:  []string{apacheWPFilter},
		HitCount: hitCount,
		FindTime: config.Duration{Duration: time.Minute},
		Actions: config.JailActions{
			OnMatch: []string{"echo {{ .IP }} >> " + outFile},
		},
	}
}

// startIntegrationPipeline wires a PollBackend to a JailRuntime and returns a
// cancel func that shuts the pipeline down.
func startIntegrationPipeline(t *testing.T, jr *JailRuntime, logGlob string) context.CancelFunc {
	t.Helper()

	b := watch.NewPollBackend(50 * time.Millisecond)
	specs := []watch.WatchSpec{{
		JailName:    jr.cfg.Name,
		Globs:       []string{logGlob},
		ReadFromEnd: false,
	}}

	events := make(chan watch.RawLine, 64)
	ctx, cancel := context.WithCancel(context.Background())

	go func() { _ = b.Start(ctx, specs, events) }()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case rawLine, ok := <-events:
				if !ok {
					return
				}
				for _, jailName := range rawLine.Jails {
					evt := watch.Event{
						JailName: jailName,
						FilePath: rawLine.FilePath,
						Line:     rawLine.Line,
						Time:     rawLine.EnqueueAt,
					}
					_ = jr.HandleEvent(ctx, evt)
				}
			}
		}
	}()

	// Allow the backend to reach its first poll cycle before the caller writes.
	time.Sleep(150 * time.Millisecond)

	return cancel
}

// appendLines appends lines to path, one per line.
func appendLines(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open log for append: %v", err)
	}
	defer f.Close()
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
}

// waitForContent polls outFile until it contains want, or times out.
func waitForContent(t *testing.T, outFile, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(outFile)
		if err == nil && strings.Contains(string(data), want) {
			return string(data)
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, _ := os.ReadFile(outFile)
	t.Fatalf("timed out waiting for %q in output; got: %q", want, string(data))
	return ""
}

// TestApacheWordpressIntegration_FullPipeline exercises the complete path:
//
//	PollBackend file-tail  →  watch.Event  →  JailRuntime.HandleEvent
//	→  filter match  →  HitTracker threshold  →  on_match action
//
// It uses real Apache combined-log lines from a WordPress brute-force attack.
// With HitCount=2, only 51.103.25.59 (which appears twice) should trigger.
func TestApacheWordpressIntegration_FullPipeline(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "access.log")
	outFile := filepath.Join(dir, "blocked.txt")

	// Pre-create an empty log file so the backend can open a tailer on it.
	if err := os.WriteFile(logFile, nil, 0644); err != nil {
		t.Fatal(err)
	}

	jr, err := NewJailRuntime(newApacheWPJailConfig(logFile, outFile, 2))
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	cancel := startIntegrationPipeline(t, jr, logFile)
	defer cancel()

	appendLines(t, logFile, apacheWPLines)

	// 51.103.25.59 hit the threshold (2 posts) — it must appear in the output.
	content := waitForContent(t, outFile, "51.103.25.59", 3*time.Second)

	// IPs that appeared only once must NOT have triggered.
	for _, single := range []string{
		"31.24.155.180", "50.6.229.148", "94.46.170.34",
		"69.167.171.193", "162.215.219.73", "15.161.25.29",
		"41.76.215.184", "64.91.240.67",
	} {
		if strings.Contains(content, single) {
			t.Errorf("IP %s triggered on_match but should not have (only 1 hit < threshold 2)", single)
		}
	}
}

// TestApacheWordpressIntegration_NonMatchingLinesIgnored verifies that GET
// requests and other non-wp-login lines do not count toward the hit threshold,
// even when they originate from an IP that later sends a matching POST.
func TestApacheWordpressIntegration_NonMatchingLinesIgnored(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "access.log")
	outFile := filepath.Join(dir, "blocked.txt")

	if err := os.WriteFile(logFile, nil, 0644); err != nil {
		t.Fatal(err)
	}

	// Threshold=2.  51.103.25.59 sends 3 non-matching GET requests first, then
	// only 1 matching POST — still below threshold, so no block.
	jr, err := NewJailRuntime(newApacheWPJailConfig(logFile, outFile, 2))
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	cancel := startIntegrationPipeline(t, jr, logFile)
	defer cancel()

	appendLines(t, logFile, apacheWPNonMatchingLines)

	// One matching POST from 51.103.25.59 — below threshold.
	appendLines(t, logFile, []string{
		`51.103.25.59 - - [04/Apr/2026:01:04:24 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "-" "-"`,
	})

	// Wait long enough for the pipeline to process, then assert nothing fired.
	time.Sleep(500 * time.Millisecond)

	if _, err := os.Stat(outFile); err == nil {
		data, _ := os.ReadFile(outFile)
		t.Fatalf("on_match should not have fired (only 1 matching hit < threshold 2); output: %q", string(data))
	}
}

// TestApacheWordpressIntegration_FilterMatchesAllSampleLines confirms that
// every sample line is matched by the filter and that the extracted IP is
// correct — without needing the watch pipeline.  This acts as a fast
// smoke-test for the regex itself.
func TestApacheWordpressIntegration_FilterMatchesAllSampleLines(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "access.log")
	outFile := filepath.Join(dir, "blocked.txt")

	cfg := newApacheWPJailConfig(logFile, outFile, 1)
	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	// Extract the expected IP (first field) for each sample line.
	wantIPs := make([]string, len(apacheWPLines))
	for i, line := range apacheWPLines {
		wantIPs[i] = strings.Fields(line)[0]
	}

	// Write all sample lines to a temp file and run ConfigTest.
	if err := os.WriteFile(logFile, []byte(strings.Join(apacheWPLines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	total, matching, matches, err := jr.ConfigTest(logFile, 0, true)
	if err != nil {
		t.Fatalf("ConfigTest: %v", err)
	}
	if total != len(apacheWPLines) {
		t.Errorf("total lines: want %d, got %d", len(apacheWPLines), total)
	}
	if matching != len(apacheWPLines) {
		t.Errorf("matching lines: want %d (all), got %d", len(apacheWPLines), matching)
	}

	// Verify each matched line yields the correct IP as its first field.
	for i, line := range matches {
		gotIP := strings.Fields(line)[0]
		if gotIP != wantIPs[i] {
			t.Errorf("line %d: expected IP %q, got %q", i, wantIPs[i], gotIP)
		}
	}
}

// TestManagerRunRoutesEvents verifies that Manager.Run correctly starts the
// watch backend in a goroutine and routes events to HandleEvent.  This is a
// regression test for the bug where m.backend.Start was called synchronously,
// causing the event-routing loop to never execute.
func TestManagerRunRoutesEvents(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "access.log")
	outFile := filepath.Join(dir, "blocked.txt")

	if err := os.WriteFile(logFile, nil, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Jails: []config.JailConfig{
			*newApacheWPJailConfig(logFile, outFile, 1),
		},
		Engine: config.EngineConfig{
			PollInterval: config.Duration{Duration: 50 * time.Millisecond},
			MinLatency:   config.Duration{Duration: 100 * time.Millisecond},
			MaxLatency:   config.Duration{Duration: 500 * time.Millisecond},
			PerfWindow:   3,
			ReadFromEnd:  false,
		},
	}

	mgr, err := NewManager(cfg, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = mgr.Run(ctx) }()

	// Allow the manager and backend to initialise.
	time.Sleep(200 * time.Millisecond)

	// Write one matching line — threshold=1 so a single hit must trigger.
	appendLines(t, logFile, []string{apacheWPLines[0]})

	waitForContent(t, outFile, "31.24.155.180", 3*time.Second)
}
// writeRestartTestCfg writes a minimal jail YAML config to cfgFile.
// The on_match action appends the literal jail_time seconds value to outFile
// so tests can verify which config was in effect when the action ran.
func writeRestartTestCfg(t *testing.T, cfgFile, logFile, outFile string, jailTimeSec int) {
	t.Helper()
	content := fmt.Sprintf(`version: 1
engine:
  poll_interval: 50ms
  read_from_end: false
jails:
  - name: test-restart
    enabled: true
    files:
      - %s
    filters:
      - '^(?P<ip>\S+) '
    hit_count: 1
    find_time: 1m
    jail_time: %ds
    actions:
      on_match:
        - 'echo %d >> %s'
`, logFile, jailTimeSec, jailTimeSec, outFile)
	if err := os.WriteFile(cfgFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestRestartJailReloadsConfig is a regression test for the bug where
// RestartJail did not apply updated config values to existing JailRuntimes.
// After a restart the new jail_time (and any other changed config) must be
// used by subsequent on_match actions.
func TestRestartJailReloadsConfig(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "access.log")
	outFile := filepath.Join(dir, "blocked.txt")
	cfgFile := filepath.Join(dir, "jail.yaml")

	if err := os.WriteFile(logFile, nil, 0644); err != nil {
		t.Fatal(err)
	}

	// Initial config: jail_time=3600s (1h).
	writeRestartTestCfg(t, cfgFile, logFile, outFile, 3600)

	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	mgr, err := NewManager(cfg, cfgFile)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx := context.Background()
	jr := mgr.jails["test-restart"]

	// First trigger (IP 1.2.3.4) — must write "3600" to outFile.
	if err := jr.HandleEvent(ctx, watch.Event{
		JailName: "test-restart",
		FilePath: logFile,
		Line:     "1.2.3.4 - first hit",
		Time:     time.Now(),
	}); err != nil {
		t.Fatalf("HandleEvent (before restart): %v", err)
	}
	jr.WaitForInflight()

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("outFile not created after first hit: %v", err)
	}
	if !strings.Contains(string(data), "3600") {
		t.Fatalf("expected '3600' in output before restart, got %q", string(data))
	}

	// Update config on disk: jail_time=14400s (4h).
	writeRestartTestCfg(t, cfgFile, logFile, outFile, 14400)

	if err := mgr.RestartJail(ctx, "test-restart"); err != nil {
		t.Fatalf("RestartJail: %v", err)
	}

	// Second trigger: use a different IP (2.3.4.5) so the HitTracker doesn't
	// suppress the second event.  on_match must now write "14400".
	if err := jr.HandleEvent(ctx, watch.Event{
		JailName: "test-restart",
		FilePath: logFile,
		Line:     "2.3.4.5 - second hit",
		Time:     time.Now(),
	}); err != nil {
		t.Fatalf("HandleEvent (after restart): %v", err)
	}
	jr.WaitForInflight()

	data, err = os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading outFile after restart: %v", err)
	}
	if !strings.Contains(string(data), "14400") {
		t.Fatalf("expected '14400' in output after restart (config reload), got %q", string(data))
	}
}

// TestApacheWordpressIntegration_ThresholdResetAfterWindow verifies that hits
// outside the find_time window are discarded and do not contribute to the
// threshold, using HandleEvent directly with controlled timestamps.
func TestApacheWordpressIntegration_ThresholdResetAfterWindow(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "blocked.txt")
	logFile := filepath.Join(dir, "access.log")

	cfg := newApacheWPJailConfig(logFile, outFile, 2)
	jr, err := NewJailRuntime(cfg)
	if err != nil {
		t.Fatalf("NewJailRuntime: %v", err)
	}

	ctx := context.Background()
	now := time.Now()

	line := `51.103.25.59 - - [04/Apr/2026:01:04:24 +0000] "POST /wp-login.php HTTP/1.1" 200 1646 "-" "-"`

	// First hit — inside the window.
	if err := jr.HandleEvent(ctx, watch.Event{
		JailName: "apache-wordpress",
		FilePath: "/var/log/apache2/access.log",
		Line:     line,
		Time:     now,
	}); err != nil {
		t.Fatalf("HandleEvent (hit 1): %v", err)
	}

	// Second hit — well outside the find_time window (2 minutes later).
	if err := jr.HandleEvent(ctx, watch.Event{
		JailName: "apache-wordpress",
		FilePath: "/var/log/apache2/access.log",
		Line:     line,
		Time:     now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("HandleEvent (hit 2, expired): %v", err)
	}

	// The window expired between the two hits, so the counter reset to 1 on the
	// second hit — threshold of 2 was never reached.
	if _, err := os.Stat(outFile); err == nil {
		data, _ := os.ReadFile(outFile)
		t.Fatalf("on_match fired but should not have (window expired); output: %q", string(data))
	}
}
