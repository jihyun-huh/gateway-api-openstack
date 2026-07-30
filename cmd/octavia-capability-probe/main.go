// Licensed under the Apache License, Version 2.0.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jihyun-huh/gateway-api-openstack/internal/probe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		printUsage()
		return fmt.Errorf("a subcommand is required")
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	switch os.Args[1] {
	case "run":
		config, err := probe.ParseRunFlags(os.Args[2:], os.Getenv)
		if err != nil {
			return err
		}
		report, err := probe.Run(ctx, config)
		fmt.Printf("report: %s\n", config.ReportFile)
		if config.KeepResources && report.Resources.LoadBalancerID != "" {
			fmt.Printf(
				"resources retained; clean up with: %s cleanup --state-file %s\n",
				os.Args[0],
				config.StateFile,
			)
		}
		return err

	case "cleanup":
		config, err := probe.ParseCleanupFlags(os.Args[2:], os.Getenv)
		if err != nil {
			return err
		}
		if err := probe.Cleanup(ctx, config); err != nil {
			return err
		}
		fmt.Printf("cleanup completed: %s\n", config.StateFile)
		return nil

	case "help", "-h", "--help":
		printUsage()
		return nil

	default:
		printUsage()
		return fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Octavia capability probe

Usage:
  octavia-capability-probe run [flags]
  octavia-capability-probe cleanup [flags]

The run subcommand creates, updates, inspects, tests, and deletes one isolated
Octavia HTTP path. Use --keep-resources to inspect it manually before cleanup.

Authentication is read from standard OS_* environment variables. Run the
subcommand with -h to see all inputs.`)
}
