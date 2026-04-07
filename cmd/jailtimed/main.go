package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/sgw/jailtime/internal/config"
	"github.com/sgw/jailtime/internal/control"
	"github.com/sgw/jailtime/internal/engine"
	"github.com/sgw/jailtime/internal/logging"
	"github.com/sgw/jailtime/pkg/version"
	"github.com/spf13/cobra"
)

const defaultConfigPath = "/etc/jailtime/jail.yaml"

func main() {
	var configPath string

	root := &cobra.Command{
		Use:          "jailtimed",
		Short:        "jailtimed — jailtime daemon",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(configPath)
		},
	}

	root.Flags().StringVar(&configPath, "config", defaultConfigPath, "path to config file")
	root.Flags().Bool("version", false, "print version and exit")
	root.PreRunE = func(cmd *cobra.Command, args []string) error {
		if v, _ := cmd.Flags().GetBool("version"); v {
			fmt.Printf("%s %s\n", version.AppName, version.Version)
			os.Exit(0)
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cleanup, err := logging.Setup(logging.Config{
		Target: cfg.Logging.Target,
		File:   cfg.Logging.File,
		Level:  cfg.Logging.Level,
	})
	if err != nil {
		return fmt.Errorf("setting up logging: %w", err)
	}
	defer cleanup()

	slog.Info("starting jailtimed",
		"version", version.Version,
		"config", configPath,
	)
	slog.Info("engine config", "engine", cfg.Engine)

	// Check config file permissions.
	if info, err := os.Stat(configPath); err == nil {
		if info.Mode()&0o022 != 0 {
			slog.Warn("config file is group-writable or world-writable; consider tightening permissions",
				"path", configPath,
				"mode", fmt.Sprintf("%04o", info.Mode().Perm()),
			)
		}
	}

	mgr, err := engine.NewManager(cfg, configPath)
	if err != nil {
		return fmt.Errorf("creating engine manager: %w", err)
	}

	adapter := &JailControllerAdapter{m: mgr}
	srv := control.NewServer(cfg.Control.Socket, adapter)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			slog.Info("received signal, shutting down", "signal", sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := mgr.Run(ctx); err != nil && err != context.Canceled {
			errCh <- fmt.Errorf("engine: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(ctx); err != nil && err != context.Canceled {
			errCh <- fmt.Errorf("control server: %w", err)
		}
	}()

	wg.Wait()
	close(errCh)

	slog.Info("shutting down")

	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// JailControllerAdapter wraps *engine.Manager and implements control.JailController.
type JailControllerAdapter struct {
	m *engine.Manager
}

func (a *JailControllerAdapter) StartJail(ctx context.Context, name string) error {
	return a.m.StartJail(ctx, name)
}

func (a *JailControllerAdapter) StopJail(ctx context.Context, name string) error {
	return a.m.StopJail(ctx, name)
}

func (a *JailControllerAdapter) RestartJail(ctx context.Context, name string) error {
	return a.m.RestartJail(ctx, name)
}

func (a *JailControllerAdapter) JailStatus(name string) (string, error) {
	status, err := a.m.JailStatus(name)
	return string(status), err
}

func (a *JailControllerAdapter) AllJailStatuses() map[string]string {
	raw := a.m.AllJailStatuses()
	out := make(map[string]string, len(raw))
	for name, status := range raw {
		out[name] = string(status)
	}
	return out
}

func (a *JailControllerAdapter) ConfigFiles(name string, limit int, logFiles bool) ([]string, error) {
	return a.m.ConfigFiles(name, limit, logFiles)
}

func (a *JailControllerAdapter) ConfigTest(name, filePath string, limit int, returnMatching bool) (int, int, []string, error) {
	return a.m.ConfigTest(name, filePath, limit, returnMatching)
}

func (a *JailControllerAdapter) PerfStats() control.PerfResponse {
	snap := a.m.PerfStats()
	return control.PerfResponse{
		CurrentLatencyMs:  snap.CurrentLatencyMs,
		CurrentIntervalMs: snap.CurrentIntervalMs,
		AvgExecTimeMs:     snap.AvgExecTimeMs,
		AvgCPUPercent:     snap.AvgCPUPercent,
		WindowSize:        snap.WindowSize,
	}
}

func (a *JailControllerAdapter) StartWhitelist(ctx context.Context, name string) error {
	return a.m.StartWhitelist(ctx, name)
}

func (a *JailControllerAdapter) StopWhitelist(ctx context.Context, name string) error {
	return a.m.StopWhitelist(ctx, name)
}

func (a *JailControllerAdapter) RestartWhitelist(ctx context.Context, name string) error {
	return a.m.RestartWhitelist(ctx, name)
}

func (a *JailControllerAdapter) WhitelistStatus(name string) (string, error) {
	status, err := a.m.WhitelistStatus(name)
	return string(status), err
}

func (a *JailControllerAdapter) AllWhitelistStatuses() map[string]string {
	raw := a.m.AllWhitelistStatuses()
	out := make(map[string]string, len(raw))
	for name, status := range raw {
		out[name] = string(status)
	}
	return out
}
