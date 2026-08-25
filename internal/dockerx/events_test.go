// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
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

func TestToContainerEventAllowlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		action    events.Action
		wantEvent string
		wantOK    bool
	}{
		{name: "health healthy", action: events.ActionHealthStatusHealthy, wantEvent: "health_status", wantOK: true},
		{name: "health unhealthy", action: events.ActionHealthStatusUnhealthy, wantEvent: "health_status", wantOK: true},
		{name: "die", action: events.ActionDie, wantEvent: "die", wantOK: true},
		{name: "start", action: events.ActionStart, wantEvent: "start", wantOK: true},
		{name: "stop", action: events.ActionStop, wantEvent: "stop", wantOK: true},
		{name: "oom", action: events.ActionOOM, wantEvent: "oom", wantOK: true},
		{name: "create", action: events.Action("create"), wantOK: false},
		{name: "destroy", action: events.Action("destroy"), wantOK: false},
		{name: "rename", action: events.Action("rename"), wantOK: false},
		{name: "pause", action: events.Action("pause"), wantOK: false},
		{name: "restart", action: events.Action("restart"), wantOK: false},
		{name: "kill", action: events.Action("kill"), wantOK: false},
		{name: "exec_create carries a command line", action: events.Action("exec_create: /bin/sh -c 'cat /run/secrets/db'"), wantOK: false},
		{name: "health_status: running", action: events.ActionHealthStatusRunning, wantOK: false},
		{name: "free-form health output", action: events.Action("health_status: connection refused to 10.0.0.5:5432"), wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := events.Message{
				Type:   events.ContainerEventType,
				Action: tt.action,
				Actor:  events.Actor{ID: "abc123", Attributes: map[string]string{"name": "/api"}},
			}

			got, ok := toContainerEvent(msg)

			if ok != tt.wantOK {
				t.Fatalf("toContainerEvent() ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if got.Event != tt.wantEvent {
				t.Errorf("toContainerEvent() Event = %q, want %q", got.Event, tt.wantEvent)
			}
		})
	}
}

func TestToContainerEventHealthValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		action     events.Action
		wantHealth string
	}{
		{name: "healthy", action: events.ActionHealthStatusHealthy, wantHealth: "healthy"},
		{name: "unhealthy", action: events.ActionHealthStatusUnhealthy, wantHealth: "unhealthy"},
		{name: "die carries no health", action: events.ActionDie, wantHealth: ""},
		{name: "start carries no health", action: events.ActionStart, wantHealth: ""},
		{name: "stop carries no health", action: events.ActionStop, wantHealth: ""},
		{name: "oom carries no health", action: events.ActionOOM, wantHealth: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := events.Message{
				Type:   events.ContainerEventType,
				Action: tt.action,
				Actor:  events.Actor{ID: "abc123", Attributes: map[string]string{"name": "/api"}},
			}

			got, ok := toContainerEvent(msg)
			if !ok {
				t.Fatalf("toContainerEvent() ok = false, want true")
			}
			if got.Health != tt.wantHealth {
				t.Errorf("toContainerEvent() Health = %q, want %q", got.Health, tt.wantHealth)
			}
		})
	}
}

func TestToContainerEventNeverCarriesAttributes(t *testing.T) {
	t.Parallel()

	msg := events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionDie,
		Actor: events.Actor{
			ID:         "abc123",
			Attributes: map[string]string{"com.example.token": "hunter2", "name": "/api"},
		},
	}

	got, ok := toContainerEvent(msg)
	if !ok {
		t.Fatalf("toContainerEvent() ok = false, want true")
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	if strings.Contains(string(body), "hunter2") {
		t.Errorf("marshalled event %s contains the secret label value", body)
	}
	if strings.Contains(string(body), "com.example.token") {
		t.Errorf("marshalled event %s contains the label key", body)
	}
	if !strings.Contains(string(body), "api") {
		t.Errorf("marshalled event %s does not contain the container name", body)
	}
}

func TestToContainerEventTime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		time     int64
		timeNano int64
		want     string
	}{
		{
			name:     "TimeNano set",
			timeNano: time.Date(2026, 8, 25, 9, 14, 2, 0, time.UTC).UnixNano(),
			want:     "2026-08-25T09:14:02Z",
		},
		{
			name: "only Time set",
			time: time.Date(2026, 8, 25, 9, 31, 47, 0, time.UTC).Unix(),
			want: "2026-08-25T09:31:47Z",
		},
		{
			name: "both zero",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := events.Message{
				Type:     events.ContainerEventType,
				Action:   events.ActionDie,
				Actor:    events.Actor{ID: "abc123", Attributes: map[string]string{"name": "/api"}},
				Time:     tt.time,
				TimeNano: tt.timeNano,
			}

			got, ok := toContainerEvent(msg)
			if !ok {
				t.Fatalf("toContainerEvent() ok = false, want true")
			}
			if got.Time != tt.want {
				t.Errorf("toContainerEvent() Time = %q, want %q", got.Time, tt.want)
			}
		})
	}
}

func TestToContainerEventName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		attributes map[string]string
		want       string
	}{
		{name: "name present", attributes: map[string]string{"name": "api"}, want: "api"},
		{name: "name missing", attributes: map[string]string{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := events.Message{
				Type:   events.ContainerEventType,
				Action: events.ActionDie,
				Actor:  events.Actor{ID: "abc123", Attributes: tt.attributes},
			}

			got, ok := toContainerEvent(msg)
			if !ok {
				t.Fatalf("toContainerEvent() ok = false, want true")
			}
			if got.Name != tt.want {
				t.Errorf("toContainerEvent() Name = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestToContainerEventIgnoresNonContainerType(t *testing.T) {
	t.Parallel()

	msg := events.Message{
		Type:   events.ImageEventType,
		Action: events.ActionDie,
		Actor:  events.Actor{ID: "abc123", Attributes: map[string]string{"name": "/api"}},
	}

	_, ok := toContainerEvent(msg)
	if ok {
		t.Errorf("toContainerEvent() ok = true, want false for non-container type")
	}
}
