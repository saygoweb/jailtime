package engine

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sgw/jailtime/internal/action"
	"github.com/sgw/jailtime/internal/config"
	"github.com/sgw/jailtime/internal/filter"
	"github.com/sgw/jailtime/internal/watch"
)

// JailStatus is the running state of a jail.
type JailStatus string

const (
	StatusStarted JailStatus = "started"
	StatusStopped JailStatus = "stopped"
)

// debugRateLimiter allows at most maxPerSec log entries per second.
type debugRateLimiter struct {
	mu          sync.Mutex
	windowStart time.Time
	count       int
	maxPerSec   int
	now         func() time.Time
}

func newDebugRateLimiter(maxPerSec int) *debugRateLimiter {
	return &debugRateLimiter{
		maxPerSec: maxPerSec,
		now:       time.Now,
	}
}

func (r *debugRateLimiter) Allow() bool {
	t := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.windowStart.IsZero() || t.Sub(r.windowStart) >= time.Second {
		r.windowStart = t
		r.count = 0
	}
	if r.count < r.maxPerSec {
		r.count++
		return true
	}
	return false
}

// JailRuntime manages the lifecycle of a single jail.
type JailRuntime struct {
	cfg      *config.JailConfig
	includes []*filter.CompiledFilter
	excludes []*filter.CompiledFilter
	hits     *HitTracker
	mu       sync.RWMutex
	status   JailStatus
	debugLog *debugRateLimiter
	// inflight tracks IPs that currently have an on_match action running.
	// Only one on_match execution per IP is allowed at a time; concurrent
	// threshold triggers for an in-flight IP are silently skipped.
	inflight sync.Map
}

func NewJailRuntime(cfg *config.JailConfig) (*JailRuntime, error) {
	includes, err := filter.CompileAll(cfg.Filters)
	if err != nil {
		return nil, fmt.Errorf("compiling include filters for jail %q: %w", cfg.Name, err)
	}
	excludes, err := filter.CompileAll(cfg.ExcludeFilters)
	if err != nil {
		return nil, fmt.Errorf("compiling exclude filters for jail %q: %w", cfg.Name, err)
	}
	return &JailRuntime{
		cfg:      cfg,
		includes: includes,
		excludes: excludes,
		hits:     NewHitTracker(),
		status:   StatusStopped,
		debugLog: newDebugRateLimiter(2),
	}, nil
}

func (jr *JailRuntime) lifecycleCtx() action.Context {
	return action.Context{
		Jail:      jr.cfg.Name,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

// Reconfigure updates the jail's config and recompiles its filters.
// It is safe to call concurrently with HandleEvent; the hit tracker is reset
// because find_time and hit_count may have changed.
func (jr *JailRuntime) Reconfigure(cfg *config.JailConfig) error {
	includes, err := filter.CompileAll(cfg.Filters)
	if err != nil {
		return fmt.Errorf("compiling include filters: %w", err)
	}
	excludes, err := filter.CompileAll(cfg.ExcludeFilters)
	if err != nil {
		return fmt.Errorf("compiling exclude filters: %w", err)
	}
	jr.mu.Lock()
	jr.cfg = cfg
	jr.includes = includes
	jr.excludes = excludes
	jr.hits = NewHitTracker()
	jr.mu.Unlock()
	return nil
}

// Start sets status to started and runs on_start actions.
func (jr *JailRuntime) Start(ctx context.Context) error {
	jr.mu.Lock()
	jr.status = StatusStarted
	cfg := jr.cfg
	jr.mu.Unlock()

	_, err := action.RunAll(ctx, cfg.Actions.OnStart, jr.lifecycleCtx(), 0)
	return err
}

// Stop sets status to stopped and runs on_stop actions.
func (jr *JailRuntime) Stop(ctx context.Context) error {
	jr.mu.Lock()
	jr.status = StatusStopped
	cfg := jr.cfg
	jr.mu.Unlock()

	_, err := action.RunAll(ctx, cfg.Actions.OnStop, jr.lifecycleCtx(), 0)
	return err
}

// Restart runs on_restart actions; status remains started.
func (jr *JailRuntime) Restart(ctx context.Context) error {
	jr.mu.RLock()
	cfg := jr.cfg
	jr.mu.RUnlock()
	_, err := action.RunAll(ctx, cfg.Actions.OnRestart, jr.lifecycleCtx(), 0)
	return err
}

// Status returns the current jail status.
func (jr *JailRuntime) Status() JailStatus {
	jr.mu.RLock()
	defer jr.mu.RUnlock()
	return jr.status
}

// ConfigFiles returns the file paths that currently match the jail's configured
// globs, deduplicated, capped at limit (0 = no limit). If logFiles is true
// each match is emitted via slog at Info level.
func (jr *JailRuntime) ConfigFiles(limit int, logFiles bool) []string {
	jr.mu.RLock()
	cfg := jr.cfg
	jr.mu.RUnlock()

	seen := make(map[string]bool)
	var files []string
	for _, pattern := range cfg.Files {
		paths, _ := filepath.Glob(pattern)
		for _, p := range paths {
			if seen[p] {
				continue
			}
			seen[p] = true
			if logFiles {
				slog.Info("config files match", "jail", cfg.Name, "file", p)
			}
			files = append(files, p)
			if limit > 0 && len(files) >= limit {
				return files
			}
		}
	}
	return files
}

// ConfigTest runs the jail's filters against every line in filePath without
// triggering any actions. It returns the total number of lines processed, the
// number that matched, and (when returnMatching is true) up to limit matching
// lines (0 = no limit).
func (jr *JailRuntime) ConfigTest(filePath string, limit int, returnMatching bool) (totalLines, matchingLines int, matches []string, err error) {
	jr.mu.RLock()
	includes := jr.includes
	excludes := jr.excludes
	jr.mu.RUnlock()

	f, err := os.Open(filePath)
	if err != nil {
		return 0, 0, nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		totalLines++
		result, matchErr := filter.Match(line, includes, excludes)
		if matchErr != nil {
			continue
		}
		if result != nil {
			matchingLines++
			if returnMatching && (limit <= 0 || len(matches) < limit) {
				matches = append(matches, line)
			}
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return totalLines, matchingLines, matches, scanErr
	}
	return totalLines, matchingLines, matches, nil
}

// HandleEvent processes a watch.Event through the filter/hit pipeline.
func (jr *JailRuntime) HandleEvent(ctx context.Context, evt watch.Event) error {
	// Snapshot mutable config state under the read lock so a concurrent
	// Reconfigure cannot race with this event's processing.
	jr.mu.RLock()
	cfg := jr.cfg
	includes := jr.includes
	excludes := jr.excludes
	hits := jr.hits
	jr.mu.RUnlock()

	result, err := filter.Match(evt.Line, includes, excludes)
	if err != nil {
		return fmt.Errorf("filter match: %w", err)
	}

	if result == nil {
		// Rate-limited debug log for non-matching lines.
		if slog.Default().Enabled(ctx, slog.LevelDebug) && jr.debugLog.Allow() {
			slog.DebugContext(ctx, "line considered",
				"jail", cfg.Name,
				"file", evt.FilePath,
				"line", evt.Line,
				"matched", false,
			)
		}
		return nil
	}

	// Filter matched — always log (matches are infrequent; no rate limit).
	slog.Debug("filter matched",
		"jail", cfg.Name,
		"file", evt.FilePath,
		"line", evt.Line,
		"ip", result.IP,
	)

	// Validate extracted address against configured net type.
	switch cfg.NetType {
	case "CIDR":
		if _, _, cidrErr := net.ParseCIDR(result.IP); cidrErr != nil {
			slog.Debug("ip validation failed",
				"jail", cfg.Name,
				"ip", result.IP,
				"net_type", "CIDR",
				"error", cidrErr,
			)
			return nil
		}
	default: // "IP" or unset
		if net.ParseIP(result.IP) == nil {
			slog.Debug("ip validation failed",
				"jail", cfg.Name,
				"ip", result.IP,
				"net_type", cfg.NetType,
			)
			return nil
		}
	}

	t := evt.Time
	if t.IsZero() {
		t = time.Now()
	}

	findTime := cfg.FindTime.Duration
	if findTime == 0 {
		findTime = time.Minute
	}
	threshold := cfg.HitCount
	if threshold == 0 {
		threshold = 1
	}

	count, triggered := hits.Record(result.IP, t, findTime, threshold)
	if !triggered {
		slog.Debug("hit count below threshold",
			"jail", cfg.Name,
			"ip", result.IP,
			"count", count,
			"threshold", threshold,
		)
		return nil
	}

	slog.Info("hit threshold reached, running on_match",
		"jail", cfg.Name,
		"ip", result.IP,
		"count", count,
		"threshold", threshold,
	)

	// Ensure at most one on_match action runs per IP at a time.
	// If a previous trigger for the same IP is still executing its actions,
	// skip this one — the IP will re-trigger once the in-flight action
	// completes and the hit window fills again.
	if _, alreadyInFlight := jr.inflight.LoadOrStore(result.IP, struct{}{}); alreadyInFlight {
		slog.Info("on_match already in flight for ip, skipping duplicate trigger",
			"jail", cfg.Name,
			"ip", result.IP,
		)
		return nil
	}
	defer jr.inflight.Delete(result.IP)

	actCtx := action.Context{
		IP:        result.IP,
		Jail:      cfg.Name,
		File:      evt.FilePath,
		Line:      evt.Line,
		JailTime:  int64(cfg.JailTime.Duration.Seconds()),
		FindTime:  int64(findTime.Seconds()),
		HitCount:  count,
		Timestamp: t.UTC().Format(time.RFC3339),
	}

	// Query pre-check: only run when query_before_match is true.
	// Exit 0 means the IP is already blocked — skip on_match.
	if cfg.QueryBeforeMatch && cfg.Query != "" {
		res, _ := action.Run(ctx, cfg.Query, actCtx, 10*time.Second)
		if res.ExitCode == 0 && res.Error == nil {
			slog.Info("query pre-check suppressed on_match",
				"jail", cfg.Name,
				"ip", result.IP,
				"query_exit_code", res.ExitCode,
			)
			return nil
		}
	}

	_, err = action.RunAll(ctx, cfg.Actions.OnMatch, actCtx, 0)
	return err
}
