/*
Copyright 2026 The gateway-api-openstack Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	os.Exit(runMain(os.Args[1:]))
}

func runMain(arguments []string) int {
	flags := flag.NewFlagSet("gateway-api-openstack-e2e-runner", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var configPath string
	var repositoryRoot string
	var auditBinary string
	flags.StringVar(&configPath, "config", "", "absolute path to the shared-project E2E config")
	flags.StringVar(&repositoryRoot, "repository-root", "", "repository root; defaults to the current repository")
	flags.StringVar(&auditBinary, "audit-binary", "", "ownership audit binary; defaults to bin/openstack-gateway-audit")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(os.Stderr, "error: positional arguments are not accepted")
		return 2
	}

	if repositoryRoot == "" {
		workingDirectory, err := os.Getwd()
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "error: could not determine the working directory")
			return 1
		}
		repositoryRoot, err = findRepositoryRoot(workingDirectory)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "error: run the E2E runner from this repository or set --repository-root")
			return 1
		}
	} else if !filepath.IsAbs(repositoryRoot) {
		_, _ = fmt.Fprintln(os.Stderr, "error: --repository-root must be absolute")
		return 2
	}
	repositoryRoot = filepath.Clean(repositoryRoot)

	file, err := loadFileConfig(configPath)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	config, err := resolveFileConfig(file, resolveOptions{repositoryRoot: repositoryRoot})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if auditBinary != "" {
		if !filepath.IsAbs(auditBinary) {
			_, _ = fmt.Fprintln(os.Stderr, "error: --audit-binary must be absolute")
			return 2
		}
		auditBinary = filepath.Clean(auditBinary)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, config, runOptions{
		repositoryRoot: repositoryRoot,
		auditBinary:    auditBinary,
		stdout:         os.Stdout,
		stderr:         os.Stderr,
	}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func findRepositoryRoot(start string) (string, error) {
	directory := filepath.Clean(start)
	for {
		if info, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil && info.Mode().IsRegular() {
			if _, err := os.Stat(filepath.Join(directory, "config", "manager", "deployment.yaml")); err == nil {
				return directory, nil
			}
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("repository root not found")
		}
		directory = parent
	}
}
