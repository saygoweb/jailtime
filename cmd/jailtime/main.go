package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/sgw/jailtime/internal/control"
	"github.com/sgw/jailtime/pkg/version"
	"github.com/spf13/cobra"
)

const (
	defaultSocket = "/run/jailtime/jailtimed.sock"
	defaultConfig = "/etc/jailtime/jail.yaml"
)

func main() {
	var socketPath string
	var configPath string

	root := &cobra.Command{
		Use:   "jailtime",
		Short: "jailtime CLI — control the jailtimed daemon",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&socketPath, "socket", defaultSocket, "path to control socket")
	root.PersistentFlags().StringVar(&configPath, "config", defaultConfig, "path to config file")
	_ = configPath // reserved for future use

	client := func() *control.Client {
		return control.NewClient(socketPath)
	}

	// status [jail]
	statusCmd := &cobra.Command{
		Use:   "status [jail]",
		Short: "Show status of all jails or a specific jail",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := client()
			if len(args) == 1 {
				resp, err := c.JailStatus(args[0])
				if err != nil {
					return err
				}
				fmt.Printf("%s\t%s\n", resp.Name, resp.Status)
				return nil
			}
			resp, err := c.ListJails()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATUS")
			for _, j := range resp.Jails {
				fmt.Fprintf(tw, "%s\t%s\n", j.Name, j.Status)
			}
			tw.Flush()
			return nil
		},
	}

	// start <jail>
	startCmd := &cobra.Command{
		Use:   "start <jail>",
		Short: "Start a jail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().StartJail(args[0])
		},
	}

	// stop <jail>
	stopCmd := &cobra.Command{
		Use:   "stop <jail>",
		Short: "Stop a jail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().StopJail(args[0])
		},
	}

	// restart <jail>
	restartCmd := &cobra.Command{
		Use:   "restart <jail>",
		Short: "Restart a jail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().RestartJail(args[0])
		},
	}

	// version
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("%s %s\n", version.AppName, version.Version)
		},
	}

	root.AddCommand(statusCmd, startCmd, stopCmd, restartCmd, versionCmd)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
