// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/scnplt/devmon-agent/internal/config"
)

// emptyEnv is a getenv that answers every lookup with "". Passing it into
// run() proves a help path never reaches config.Load — if it did, the
// missing required variables would surface as a *config.ValidationError
// instead of the expected nil.
func emptyEnv(string) string { return "" }

func TestRunPrintsRootUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		argv []string
	}{
		{name: "devmon with no args", argv: []string{"devmon"}},
		{name: "devmon.exe path with no args", argv: []string{`C:\x\devmon.exe`}},
		{name: "devmon-agent help", argv: []string{"devmon-agent", "help"}},
		{name: "devmon-agent -h", argv: []string{"devmon-agent", "-h"}},
		{name: "devmon-agent -help", argv: []string{"devmon-agent", "-help"}},
		{name: "devmon-agent --help", argv: []string{"devmon-agent", "--help"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			var stdout, stderr bytes.Buffer

			// Act
			err := run(tt.argv, emptyEnv, &stdout, &stderr)

			// Assert
			if err != nil {
				t.Fatalf("run() = %v, want nil", err)
			}
			if stdout.String() != rootUsage {
				t.Errorf("stdout = %q, want root usage", stdout.String())
			}
		})
	}
}

func TestRunPrintsPerCommandUsageWithoutLoadingConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "device --help", argv: []string{"devmon-agent", "device", "--help"}, want: deviceUsage},
		{name: "device pair-code --help", argv: []string{"devmon-agent", "device", "pair-code", "--help"}, want: deviceUsage},
		{name: "audit --help", argv: []string{"devmon-agent", "audit", "--help"}, want: auditUsage},
		{name: "health --help", argv: []string{"devmon-agent", "health", "--help"}, want: healthUsage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange — empty env: if run() reached config.Load, this would
			// fail validation and return a *config.ValidationError instead
			// of nil, so a nil result here is itself proof config was never
			// loaded.
			var stdout, stderr bytes.Buffer

			// Act
			err := run(tt.argv, emptyEnv, &stdout, &stderr)

			// Assert
			if err != nil {
				t.Fatalf("run() = %v, want nil", err)
			}
			if stdout.String() != tt.want {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.want)
			}
		})
	}
}

func TestRunVersionFlag(t *testing.T) {
	t.Parallel()

	// Arrange
	var stdout, stderr bytes.Buffer

	// Act
	err := run([]string{"devmon-agent", "--version"}, emptyEnv, &stdout, &stderr)

	// Assert
	if err != nil {
		t.Fatalf("run() = %v, want nil", err)
	}
	if !strings.HasPrefix(stdout.String(), "devmon-agent ") {
		t.Errorf("stdout = %q, want it to start with the version string", stdout.String())
	}
}

func TestRunUnknownCommandIsAUsageError(t *testing.T) {
	t.Parallel()

	// Arrange
	var stdout, stderr bytes.Buffer

	// Act
	err := run([]string{"devmon-agent", "bogus"}, emptyEnv, &stdout, &stderr)

	// Assert
	var uErr *usageError
	if !errors.As(err, &uErr) {
		t.Fatalf("run() error = %v (%T), want *usageError", err, err)
	}
	if !strings.Contains(uErr.Error(), `unknown command "bogus"`) {
		t.Errorf("usageError message = %q, want it to name the unknown command", uErr.Error())
	}
	if !strings.Contains(uErr.Error(), unknownCommandHint) {
		t.Errorf("usageError message = %q, want the %q hint", uErr.Error(), unknownCommandHint)
	}
}

func TestRunWithNoArgsUnderDevmonAgentNameReachesDaemonPath(t *testing.T) {
	t.Parallel()

	// Arrange — devmon-agent with no subcommand and no args is the daemon
	// path (container ENTRYPOINT). With an empty environment, config.Load
	// fails validation before anything is constructed, which is exactly the
	// proof this test wants: the daemon path was reached, not skipped as a
	// help screen would skip it, and nothing beyond config.Load ran.
	var stdout, stderr bytes.Buffer

	// Act
	err := run([]string{"devmon-agent"}, emptyEnv, &stdout, &stderr)

	// Assert
	var vErr *config.ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("run() error = %v (%T), want *config.ValidationError", err, err)
	}
}

func TestIsDevmonAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		argv0 string
		want  bool
	}{
		{name: "bare name", argv0: "devmon", want: true},
		{name: "unix path", argv0: "/usr/local/bin/devmon", want: true},
		{name: "windows path with .exe", argv0: `C:\x\devmon.exe`, want: true},
		{name: "mixed case", argv0: "DevMon", want: true},
		{name: "devmon-agent is not the alias", argv0: "devmon-agent", want: false},
		{name: "unrelated name", argv0: "bash", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isDevmonAlias(tt.argv0); got != tt.want {
				t.Errorf("isDevmonAlias(%q) = %v, want %v", tt.argv0, got, tt.want)
			}
		})
	}
}

func TestHelpRequested(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "empty", args: nil, want: false},
		{name: "no help flag", args: []string{"list"}, want: false},
		{name: "-h", args: []string{"-h"}, want: true},
		{name: "-help", args: []string{"-help"}, want: true},
		{name: "--help", args: []string{"--help"}, want: true},
		{name: "help flag not first", args: []string{"revoke", "some-id", "--help"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := helpRequested(tt.args); got != tt.want {
				t.Errorf("helpRequested(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
