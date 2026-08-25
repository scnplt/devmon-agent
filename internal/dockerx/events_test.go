// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestHealthOrNone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		h    *container.HealthSummary
		want string
	}{
		{name: "nil summary", h: nil, want: "none"},
		{name: "no healthcheck", h: &container.HealthSummary{Status: container.NoHealthcheck}, want: "none"},
		{name: "starting", h: &container.HealthSummary{Status: container.Starting}, want: "starting"},
		{name: "healthy", h: &container.HealthSummary{Status: container.Healthy}, want: "healthy"},
		{name: "unhealthy", h: &container.HealthSummary{Status: container.Unhealthy}, want: "unhealthy"},
		{name: "unknown status", h: &container.HealthSummary{Status: "bogus"}, want: "none"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := healthOrNone(tt.h); got != tt.want {
				t.Errorf("healthOrNone(%+v) = %q, want %q", tt.h, got, tt.want)
			}
		})
	}
}

func TestToContainerStateSummary(t *testing.T) {
	t.Parallel()

	s := container.Summary{
		ID:    "abc123",
		Names: []string{"/api"},
		State: container.ContainerState("running"),
	}

	got := toContainerStateSummary(s)

	want := ContainerStateSummary{ID: "abc123", Name: "api", State: "running", Health: "none"}
	if got != want {
		t.Errorf("toContainerStateSummary() = %+v, want %+v", got, want)
	}
}

func TestTrimContainerName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "leading slash stripped", input: "/api", want: "api"},
		{name: "no leading slash", input: "api", want: "api"},
		{name: "empty string", input: "", want: ""},
		{name: "only one slash stripped", input: "//weird", want: "/weird"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := trimContainerName(tt.input); got != tt.want {
				t.Errorf("trimContainerName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestContainerStateSummaryNamesEmpty(t *testing.T) {
	t.Parallel()

	s := container.Summary{ID: "abc123", Names: nil, State: container.ContainerState("running")}

	got := toContainerStateSummary(s)

	if got.Name != "" {
		t.Errorf("toContainerStateSummary() Name = %q, want empty", got.Name)
	}
}
