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
	"text/template"
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

// ActionRunner manages non-blocking, deduplicated execution of on_match actions.
// At most one action per IP is in flight at any time; duplicate submits are dropped.
type ActionRunner struct {
	inflight sync.Map
	wg       sync.WaitGroup
}

// Submit runs fn in a goroutine for the given ip. Returns false if an action
// for this ip is already in flight (duplicate dropped).
func (r *ActionRunner) Submit(ip string, fn func()) bool {
	if _, exists := r.inflight.LoadOrStore(ip, struct{}{}); exists {
		return false
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.inflight.Delete(ip)
		fn()
	}()
	return true
}

// Wait blocks until all in-flight actions complete.
func (r *ActionRunner) Wait() {
	r.wg.Wait()
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
	runner   ActionRunner
	// Pre-compiled action templates (populated by compileTemplates)
	onAddTmpls    []*template.Template
	onRemoveTmpls []*template.Template
	queryTmpl     *template.Template // nil if no query configured
	// Whitelist membership state (only populated for watch_mode: static)
	memberIPs   map[string]struct{}
	memberCIDRs []*net.IPNet
	// ignoreSetsChecker is injected by Manager after all runtimes are created.
	// When non-nil, a match on the checker suppresses on_add actions.
	ignoreSetsChecker func(ip string) bool
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
	jr := &JailRuntime{
		cfg:         cfg,
		includes:    includes,
		excludes:    excludes,
		hits:        NewHitTracker(),
		status:      StatusStopped,
		debugLog:    newDebugRateLimiter(2),
		memberIPs:   make(map[string]struct{}),
		memberCIDRs: nil,
	}
	if err := jr.compileTemplates(cfg); err != nil {
		return nil, err
	}
	return jr, nil
}

func (jr *JailRuntime) compileTemplates(cfg *config.JailConfig) error {
	onAddTmpls := make([]*template.Template, 0, len(cfg.Actions.OnAdd))
	for i, tmplStr := range cfg.Actions.OnAdd {
		t, err := action.CompileTemplate(fmt.Sprintf("on_add[%d]", i), tmplStr)
		if err != nil {
			return fmt.Errorf("compiling on_add[%d]: %w", i, err)
		}
		onAddTmpls = append(onAddTmpls, t)
	}
	jr.onAddTmpls = onAddTmpls

	onRemoveTmpls := make([]*template.Template, 0, len(cfg.Actions.OnRemove))
	for i, tmplStr := range cfg.Actions.OnRemove {
		t, err := action.CompileTemplate(fmt.Sprintf("on_remove[%d]", i), tmplStr)
		if err != nil {
			return fmt.Errorf("compiling on_remove[%d]: %w", i, err)
		}
		onRemoveTmpls = append(onRemoveTmpls, t)
	}
	jr.onRemoveTmpls = onRemoveTmpls

	if cfg.Query != "" {
		t, err := action.CompileTemplate("query", cfg.Query)
		if err != nil {
			return fmt.Errorf("compiling query: %w", err)
		}
		jr.queryTmpl = t
	} else {
		jr.queryTmpl = nil
	}
	return nil
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
	onAddTmpls := make([]*template.Template, 0, len(cfg.Actions.OnAdd))
	for i, tmplStr := range cfg.Actions.OnAdd {
		t, err := action.CompileTemplate(fmt.Sprintf("on_add[%d]", i), tmplStr)
		if err != nil {
			return fmt.Errorf("compiling on_add[%d]: %w", i, err)
		}
		onAddTmpls = append(onAddTmpls, t)
	}
	onRemoveTmpls := make([]*template.Template, 0, len(cfg.Actions.OnRemove))
	for i, tmplStr := range cfg.Actions.OnRemove {
		t, err := action.CompileTemplate(fmt.Sprintf("on_remove[%d]", i), tmplStr)
		if err != nil {
			return fmt.Errorf("compiling on_remove[%d]: %w", i, err)
		}
		onRemoveTmpls = append(onRemoveTmpls, t)
	}
	var queryTmpl *template.Template
	if cfg.Query != "" {
		queryTmpl, err = action.CompileTemplate("query", cfg.Query)
		if err != nil {
			return fmt.Errorf("compiling query: %w", err)
		}
	}
	jr.mu.Lock()
	jr.cfg = cfg
	jr.includes = includes
	jr.excludes = excludes
	jr.hits = NewHitTracker()
	jr.onAddTmpls = onAddTmpls
	jr.onRemoveTmpls = onRemoveTmpls
	jr.queryTmpl = queryTmpl
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

// WaitForInflight blocks until all in-flight on_add goroutines have
// completed.  Useful in tests and for graceful shutdown.
func (jr *JailRuntime) WaitForInflight() {
	jr.runner.Wait()
}

// IsMember reports whether the given IP string is a member of this runtime's
// static membership set. It is safe to call from multiple goroutines.
func (jr *JailRuntime) IsMember(ip string) bool {
	jr.mu.RLock()
	memberIPs := jr.memberIPs
	memberCIDRs := jr.memberCIDRs
	jr.mu.RUnlock()

	if _, ok := memberIPs[ip]; ok {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, cidr := range memberCIDRs {
		if cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

// addMember adds ip to the in-memory membership set. Must be called with mu held or from single goroutine.
func (jr *JailRuntime) addMember(ip string) {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	// Try parsing as CIDR first, then plain IP.
	if _, cidr, err := net.ParseCIDR(ip); err == nil {
		// Avoid duplicates.
		for _, existing := range jr.memberCIDRs {
			if existing.String() == cidr.String() {
				return
			}
		}
		jr.memberCIDRs = append(jr.memberCIDRs, cidr)
		return
	}
	jr.memberIPs[ip] = struct{}{}
}

// removeMember removes ip from the in-memory membership set.
func (jr *JailRuntime) removeMember(ip string) {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	if _, cidr, err := net.ParseCIDR(ip); err == nil {
		target := cidr.String()
		for i, existing := range jr.memberCIDRs {
			if existing.String() == target {
				jr.memberCIDRs = append(jr.memberCIDRs[:i], jr.memberCIDRs[i+1:]...)
				return
			}
		}
		return
	}
	delete(jr.memberIPs, ip)
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
	onAddTmpls := jr.onAddTmpls
	onRemoveTmpls := jr.onRemoveTmpls
	queryTmpl := jr.queryTmpl
	ignoreSetsChecker := jr.ignoreSetsChecker
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

	switch evt.Kind {
	case watch.EventAdded:
		// Static-mode: IP first appeared in file.
		jr.addMember(result.IP)
		if len(onAddTmpls) == 0 {
			return nil
		}
		actCtx := action.Context{
			IP:        result.IP,
			Jail:      cfg.Name,
			File:      evt.FilePath,
			Line:      evt.Line,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}
		submitted := jr.runner.Submit(result.IP, func() {
			if _, err := action.RunAllCompiled(ctx, onAddTmpls, actCtx, cfg.ActionTimeout.Duration); err != nil {
				slog.Warn("on_add action failed", "jail", cfg.Name, "ip", result.IP, "error", err)
			}
		})
		if !submitted {
			slog.Info("on_add already in flight for ip, duplicate dropped", "jail", cfg.Name, "ip", result.IP)
		}
		return nil

	case watch.EventRemoved:
		// Static-mode: IP disappeared from file.
		jr.removeMember(result.IP)
		if len(onRemoveTmpls) == 0 {
			return nil
		}
		actCtx := action.Context{
			IP:        result.IP,
			Jail:      cfg.Name,
			File:      evt.FilePath,
			Line:      evt.Line,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}
		if _, err := action.RunAllCompiled(ctx, onRemoveTmpls, actCtx, cfg.ActionTimeout.Duration); err != nil {
			slog.Warn("on_remove action failed", "jail", cfg.Name, "ip", result.IP, "error", err)
		}
		return nil

	default: // EventTail
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

		slog.Info("hit threshold reached, running on_add",
			"jail", cfg.Name,
			"ip", result.IP,
			"count", count,
			"threshold", threshold,
		)

		// Check ignore_sets before running on_add actions.
		if ignoreSetsChecker != nil && ignoreSetsChecker(result.IP) {
			slog.Info("ip suppressed by ignore_sets",
				"jail", cfg.Name,
				"ip", result.IP,
			)
			return nil
		}

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

		submitted := jr.runner.Submit(result.IP, func() {
			cfg := jr.cfg
			// Query pre-check: only run when query_before_match is true.
			// Exit 0 means the IP is already blocked — skip on_add.
			if cfg.QueryBeforeMatch && queryTmpl != nil {
				res, _ := action.RunCompiled(ctx, queryTmpl, actCtx, cfg.ActionTimeout.Duration)
				if res.ExitCode == 0 && res.Error == nil {
					slog.Info("query pre-check suppressed on_add",
						"jail", cfg.Name,
						"ip", result.IP,
					)
					return
				}
			}
			if _, err := action.RunAllCompiled(ctx, onAddTmpls, actCtx, cfg.ActionTimeout.Duration); err != nil {
				slog.Warn("on_add action failed", "jail", cfg.Name, "ip", result.IP, "error", err)
			}
		})
		if !submitted {
			slog.Info("on_add already in flight for ip, duplicate dropped",
				"jail", jr.cfg.Name,
				"ip", result.IP,
			)
		}
		return nil
	}
}
