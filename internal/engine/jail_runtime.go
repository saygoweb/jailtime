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

type JailStatus string

const (
	StatusStarted JailStatus = "started"
	StatusStopped JailStatus = "stopped"
)

// JailRuntime manages the lifecycle of a single jail.
type JailRuntime struct {
	cfg      *config.JailConfig
	includes []*filter.CompiledFilter
	excludes []*filter.CompiledFilter
	hits     *HitTracker
	mu       sync.RWMutex
	status   JailStatus
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
	}, nil
}

func (jr *JailRuntime) lifecycleCtx() action.Context {
	return action.Context{
		Jail:      jr.cfg.Name,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

// Start sets status to started and runs on_start actions.
func (jr *JailRuntime) Start(ctx context.Context) error {
	jr.mu.Lock()
	jr.status = StatusStarted
	jr.mu.Unlock()

	_, err := action.RunAll(ctx, jr.cfg.Actions.OnStart, jr.lifecycleCtx(), 0)
	return err
}

// Stop sets status to stopped and runs on_stop actions.
func (jr *JailRuntime) Stop(ctx context.Context) error {
	jr.mu.Lock()
	jr.status = StatusStopped
	jr.mu.Unlock()

	_, err := action.RunAll(ctx, jr.cfg.Actions.OnStop, jr.lifecycleCtx(), 0)
	return err
}

// Restart runs on_restart actions; status remains started.
func (jr *JailRuntime) Restart(ctx context.Context) error {
	_, err := action.RunAll(ctx, jr.cfg.Actions.OnRestart, jr.lifecycleCtx(), 0)
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
	seen := make(map[string]bool)
	var files []string
	for _, pattern := range jr.cfg.Files {
		paths, _ := filepath.Glob(pattern)
		for _, p := range paths {
			if seen[p] {
				continue
			}
			seen[p] = true
			if logFiles {
				slog.Info("config files match", "jail", jr.cfg.Name, "file", p)
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
	f, err := os.Open(filePath)
	if err != nil {
		return 0, 0, nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		totalLines++
		result, matchErr := filter.Match(line, jr.includes, jr.excludes)
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
	result, err := filter.Match(evt.Line, jr.includes, jr.excludes)
	if err != nil {
		return fmt.Errorf("filter match: %w", err)
	}
	if result == nil {
		return nil
	}

	// Validate extracted address against configured net type.
	switch jr.cfg.NetType {
	case "CIDR":
		if _, _, err := net.ParseCIDR(result.IP); err != nil {
			return nil
		}
	default: // "IP" or unset
		if net.ParseIP(result.IP) == nil {
			return nil
		}
	}

	t := evt.Time
	if t.IsZero() {
		t = time.Now()
	}

	findTime := jr.cfg.FindTime.Duration
	if findTime == 0 {
		findTime = time.Minute
	}
	threshold := jr.cfg.HitCount
	if threshold == 0 {
		threshold = 1
	}

	count, triggered := jr.hits.Record(result.IP, t, findTime, threshold)
	if !triggered {
		return nil
	}

	actCtx := action.Context{
		IP:        result.IP,
		Jail:      jr.cfg.Name,
		File:      evt.FilePath,
		Line:      evt.Line,
		JailTime:  int64(jr.cfg.JailTime.Duration.Seconds()),
		FindTime:  int64(findTime.Seconds()),
		HitCount:  count,
		Timestamp: t.UTC().Format(time.RFC3339),
	}

	// Query pre-check: exit 0 means the IP is already blocked — skip on_match.
	if jr.cfg.Query != "" {
		res, _ := action.Run(ctx, jr.cfg.Query, actCtx, 10*time.Second)
		if res.ExitCode == 0 && res.Error == nil {
			return nil
		}
	}

	_, err = action.RunAll(ctx, jr.cfg.Actions.OnMatch, actCtx, 0)
	return err
}
