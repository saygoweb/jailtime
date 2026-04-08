package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/sgw/jailtime/internal/control"
	"github.com/sgw/jailtime/pkg/version"
	"github.com/spf13/cobra"
)

const defaultSocket = "/run/jailtime/jailtimed.sock"

func main() {
	var socketPath string

	root := &cobra.Command{
		Use:           "jailtime",
		Short:         "Control the jailtimed daemon",
		Long:          "jailtime is the command-line client for the jailtimed intrusion-prevention daemon.\nIt communicates with jailtimed over a Unix domain socket.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&socketPath, "socket", defaultSocket, "path to the jailtimed control socket")

	client := func() *control.Client {
		return control.NewClient(socketPath)
	}

	// ── status ───────────────────────────────────────────────────────────────
	statusCmd := &cobra.Command{
		Use:   "status [jail]",
		Short: "Show status of all jails, or a specific jail",
		Long:  "Show the running status of all jails managed by jailtimed.\nPass a jail name to query a single jail.",
		Example: `  jailtime status
  jailtime status sshd`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client()
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			if len(args) == 1 {
				resp, err := c.JailStatus(args[0])
				if err != nil {
					return err
				}
				fmt.Fprintf(tw, "NAME\tSTATUS\n%s\t%s\n", resp.Name, resp.Status)
				return tw.Flush()
			}
			resp, err := c.ListJails()
			if err != nil {
				return err
			}
			fmt.Fprintln(tw, "NAME\tSTATUS")
			for _, j := range resp.Jails {
				fmt.Fprintf(tw, "%s\t%s\n", j.Name, j.Status)
			}
			return tw.Flush()
		},
	}

	// ── start ────────────────────────────────────────────────────────────────
	startCmd := &cobra.Command{
		Use:     "start <jail>",
		Short:   "Start a jail",
		Example: "  jailtime start sshd",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().StartJail(args[0])
		},
	}

	// ── stop ─────────────────────────────────────────────────────────────────
	stopCmd := &cobra.Command{
		Use:     "stop <jail>",
		Short:   "Stop a jail",
		Example: "  jailtime stop sshd",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().StopJail(args[0])
		},
	}

	// ── restart ──────────────────────────────────────────────────────────────
	restartCmd := &cobra.Command{
		Use:   "restart <jail>",
		Short: "Restart a jail (reloads config from disk)",
		Long: `Restart a jail. jailtimed reloads its configuration from disk before
restarting the named jail, picking up any changes to the config file or
any included fragment files under jails.d/.`,
		Example: "  jailtime restart sshd",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().RestartJail(args[0])
		},
	}

	// ── version ──────────────────────────────────────────────────────────────
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("%s %s\n", version.AppName, version.Version)
		},
	}

	// ── config ───────────────────────────────────────────────────────────────
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Helpers for inspecting and testing jail configuration",
		Long:  "Helpers that query a running jailtimed daemon to inspect or test jail configuration without affecting running state.",
	}

	// config files <jail>
	var filesLimit int
	var filesLog bool
	filesCmd := &cobra.Command{
		Use:   "files <jail>",
		Short: "List files currently matched by a jail's glob patterns",
		Long: `Expand the configured file globs for a jail and list every matching path.
Globs are re-evaluated at query time, so files in newly-created subdirectories
will appear even if they did not exist when jailtimed was started.`,
		Example: `  jailtime config files sshd
  jailtime config files apache2 --limit=0
  jailtime config files nginx --log`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := client().ConfigFiles(args[0], filesLimit, filesLog)
			if err != nil {
				return err
			}
			for _, f := range resp.Files {
				fmt.Println(f)
			}
			fmt.Printf("(%d file(s) matched)\n", resp.Count)
			return nil
		},
	}
	filesCmd.Flags().IntVar(&filesLimit, "limit", 10, "maximum number of files to return (0 = no limit)")
	filesCmd.Flags().BoolVar(&filesLog, "log", false, "also log matched file paths via the daemon's logger")

	// config test <jail> <file>
	var testLimit int
	var testMatching bool
	testCmd := &cobra.Command{
		Use:   "test <jail> <file>",
		Short: "Test a jail's filters against a log file (no actions triggered)",
		Long: `Read every line of the given log file and run it through the jail's include
and exclude filters. Reports the total lines processed and the number that
matched. No hit counts are modified and no actions are executed.`,
		Example: `  jailtime config test sshd /var/log/auth.log
  jailtime config test nginx /var/log/nginx/access.log --matching
  jailtime config test apache2 /var/log/apache2/access.log --matching --limit=20`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := client().ConfigTest(args[0], args[1], testLimit, testMatching)
			if err != nil {
				return err
			}
			fmt.Printf("Total lines:    %d\n", resp.TotalLines)
			fmt.Printf("Matching lines: %d\n", resp.MatchingLines)
			if testMatching && len(resp.Matches) > 0 {
				fmt.Println()
				for _, line := range resp.Matches {
					fmt.Println(line)
				}
			}
			return nil
		},
	}
	testCmd.Flags().IntVar(&testLimit, "limit", 10, "maximum matching lines to return with --matching (0 = no limit)")
	testCmd.Flags().BoolVar(&testMatching, "matching", false, "print the lines that matched")

	// ── perf ─────────────────────────────────────────────────────────────────
	perfCmd := &cobra.Command{
		Use:   "perf",
		Short: "Show daemon performance metrics",
		Long:  "Display current latency, execution delay, average execution time, and CPU usage.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client()
			resp, err := c.Perf()
			if err != nil {
				return err
			}
			fmt.Printf("Performance Metrics (window=%d):\n", resp.WindowSize)
			fmt.Printf("  Current latency:     %.0fms\n", resp.CurrentLatencyMs)
			fmt.Printf("  Current interval:    %.0fms\n", resp.CurrentIntervalMs)
			fmt.Printf("  Avg execution time:  %.1fms\n", resp.AvgExecTimeMs)
			fmt.Printf("  Avg CPU usage:       %.1f%%\n", resp.AvgCPUPercent)
			return nil
		},
	}

	configCmd.AddCommand(filesCmd, testCmd)

	// ── whitelist ─────────────────────────────────────────────────────────────
	whitelistCmd := &cobra.Command{
		Use:   "whitelist",
		Short: "Manage whitelists",
		Long:  "Commands for listing, starting, stopping and restarting whitelists in a running jailtimed daemon.",
	}

	whitelistStatusCmd := &cobra.Command{
		Use:   "status [whitelist]",
		Short: "Show status of all whitelists, or a specific whitelist",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client()
			if len(args) == 1 {
				resp, err := c.WhitelistStatus(args[0])
				if err != nil {
					return err
				}
				fmt.Printf("%-30s %s\n", resp.Name, resp.Status)
				return nil
			}
			resp, err := c.ListWhitelists()
			if err != nil {
				return err
			}
			for _, wl := range resp.Whitelists {
				fmt.Printf("%-30s %s\n", wl.Name, wl.Status)
			}
			return nil
		},
	}

	whitelistStartCmd := &cobra.Command{
		Use:   "start <whitelist>",
		Short: "Start a whitelist",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().StartWhitelist(args[0])
		},
	}

	whitelistStopCmd := &cobra.Command{
		Use:   "stop <whitelist>",
		Short: "Stop a whitelist",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().StopWhitelist(args[0])
		},
	}

	whitelistRestartCmd := &cobra.Command{
		Use:   "restart <whitelist>",
		Short: "Restart a whitelist (reloads config from disk)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().RestartWhitelist(args[0])
		},
	}

	whitelistCmd.AddCommand(whitelistStatusCmd, whitelistStartCmd, whitelistStopCmd, whitelistRestartCmd)
	root.AddCommand(statusCmd, startCmd, stopCmd, restartCmd, versionCmd, configCmd, perfCmd, whitelistCmd)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
