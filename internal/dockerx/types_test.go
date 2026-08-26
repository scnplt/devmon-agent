// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

// keysOf returns the sorted-by-insertion (map order, unspecified) keys of a
// decoded JSON object, for readable failure messages in the *FieldCount
// tests below.
func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// marshalToMap marshals v and decodes the result into a map[string]any, so a
// *FieldCount test can assert on the JSON shape rather than the Go struct.
func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%#v) error = %v", v, err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", raw, err)
	}
	return body
}

// The *FieldCount tests below are the D1 allowlist guard: every DTO is
// marshalled fully populated (so omitempty cannot hide a field from the
// count) and the resulting key count is asserted exactly, not just the
// presence of the expected keys. A later phase that widens a payload must
// touch this test deliberately (FIELD_ALLOWLIST_GUARD, mirroring
// TestStatusFieldCount in internal/httpapi/status_test.go).

func TestContainerSummaryFieldCount(t *testing.T) {
	t.Parallel()

	// Arrange
	cs := ContainerSummary{
		ID:        "abc123",
		Names:     []string{"/api"},
		Image:     "myapp:1.4",
		ImageID:   "sha256:xyz",
		Command:   "/app/server",
		CreatedAt: "2023-11-14T22:13:20Z",
		State:     "running",
		Status:    "Up 3 minutes",
		Health:    "healthy",
		Labels:    map[string]string{"app": "myapp"},
		Ports:     []Port{{PrivatePort: 8080, Protocol: "tcp"}},
		Protected: true,
	}

	// Act
	body := marshalToMap(t, cs)

	// Assert
	if len(body) != 12 {
		t.Fatalf("ContainerSummary payload has %d keys (%v), want exactly 12", len(body), keysOf(body))
	}
}

func TestContainerDetailFieldCount(t *testing.T) {
	t.Parallel()

	// Arrange
	cd := ContainerDetail{
		ID:            "abc123",
		Name:          "/api",
		Image:         "myapp:1.4",
		ImageID:       "sha256:abcdef",
		CreatedAt:     "2023-11-14T22:13:20Z",
		State:         "running",
		Running:       true,
		Paused:        false,
		Restarting:    false,
		ExitCode:      0,
		StartedAt:     "2023-11-14T22:13:21Z",
		FinishedAt:    "2023-11-14T22:14:21Z",
		Health:        "healthy",
		RestartCount:  1,
		RestartPolicy: "always",
		Platform:      "linux",
		Labels:        map[string]string{"app": "myapp"},
		Command:       "/app/server",
		Args:          []string{"--port", "8080"},
		Entrypoint:    []string{"/entrypoint.sh"},
		WorkingDir:    "/app",
		User:          "nobody",
		Mounts:        []Mount{{Type: "volume", Source: "/data", Destination: "/data", ReadWrite: true}},
		Networks:      []EndpointSummary{{NetworkName: "bridge", NetworkID: "net1"}},
		Ports:         []Port{{PrivatePort: 8080, Protocol: "tcp"}},
		Protected:     true,
	}

	// Act
	body := marshalToMap(t, cd)

	// Assert
	if len(body) != 26 {
		t.Fatalf("ContainerDetail payload has %d keys (%v), want exactly 26", len(body), keysOf(body))
	}
}

// TestContainerStateSummaryFieldCount is the D1 allowlist guard for the event
// stream's snapshot entry: Health has no omitempty, so this must be exactly 4
// keys regardless of population.
func TestContainerStateSummaryFieldCount(t *testing.T) {
	t.Parallel()

	// Arrange
	css := ContainerStateSummary{
		ID:     "abc123",
		Name:   "api",
		State:  "running",
		Health: "healthy",
	}

	// Act
	body := marshalToMap(t, css)

	// Assert
	if len(body) != 4 {
		t.Fatalf("ContainerStateSummary payload has %d keys (%v), want exactly 4", len(body), keysOf(body))
	}
}

// TestContainerEventFieldCount is the D1 allowlist guard for the event
// stream's per-transition frame: Health is omitempty, so a lifecycle event
// (no Health) marshals to 4 keys and a health event marshals to 5.
func TestContainerEventFieldCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   ContainerEvent
		want int
	}{
		{
			name: "lifecycle event without health has four keys",
			ev: ContainerEvent{
				ID:    "abc123",
				Name:  "api",
				Event: "die",
				Time:  "2023-11-14T22:13:20Z",
			},
			want: 4,
		},
		{
			name: "health event has five keys",
			ev: ContainerEvent{
				ID:     "abc123",
				Name:   "api",
				Event:  "health_status",
				Health: "healthy",
				Time:   "2023-11-14T22:13:20Z",
			},
			want: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			body := marshalToMap(t, tt.ev)

			// Assert
			if len(body) != tt.want {
				t.Fatalf("ContainerEvent payload has %d keys (%v), want exactly %d", len(body), keysOf(body), tt.want)
			}
		})
	}
}

func TestImageSummaryFieldCount(t *testing.T) {
	t.Parallel()

	// Arrange
	is := ImageSummary{
		ID:          "sha256:abc",
		ParentID:    "sha256:parent",
		RepoTags:    []string{"myapp:1.4"},
		RepoDigests: []string{"myapp@sha256:def"},
		CreatedAt:   "2023-11-14T22:13:20Z",
		Size:        1024,
		Containers:  2,
		Labels:      map[string]string{"maintainer": "ops"},
	}

	// Act
	body := marshalToMap(t, is)

	// Assert
	if len(body) != 8 {
		t.Fatalf("ImageSummary payload has %d keys (%v), want exactly 8", len(body), keysOf(body))
	}
}

func TestImageDetailFieldCount(t *testing.T) {
	t.Parallel()

	// Arrange
	id := ImageDetail{
		ID:           "sha256:abc",
		RepoTags:     []string{"myapp:1.4"},
		RepoDigests:  []string{"myapp@sha256:def"},
		CreatedAt:    "2023-11-14T22:13:20Z",
		Size:         1024,
		Architecture: "amd64",
		OS:           "linux",
		Author:       "ops team",
		Comment:      "release build",
	}

	// Act
	body := marshalToMap(t, id)

	// Assert
	if len(body) != 9 {
		t.Fatalf("ImageDetail payload has %d keys (%v), want exactly 9", len(body), keysOf(body))
	}
}

func TestNetworkSummaryFieldCount(t *testing.T) {
	t.Parallel()

	// Arrange
	ns := NetworkSummary{
		ID:         "net1",
		Name:       "bridge",
		Driver:     "bridge",
		Scope:      "local",
		CreatedAt:  "2023-11-14T22:13:20Z",
		Internal:   false,
		EnableIPv6: true,
		Labels:     map[string]string{"env": "prod"},
	}

	// Act
	body := marshalToMap(t, ns)

	// Assert
	if len(body) != 8 {
		t.Fatalf("NetworkSummary payload has %d keys (%v), want exactly 8", len(body), keysOf(body))
	}
}

func TestNetworkDetailFieldCount(t *testing.T) {
	t.Parallel()

	// Arrange
	nd := NetworkDetail{
		NetworkSummary: NetworkSummary{
			ID:         "net1",
			Name:       "bridge",
			Driver:     "bridge",
			Scope:      "local",
			CreatedAt:  "2023-11-14T22:13:20Z",
			Internal:   false,
			EnableIPv6: true,
			Labels:     map[string]string{"env": "prod"},
		},
		Containers: []NetworkEndpoint{{ContainerID: "c1", Name: "api", IPv4Address: "172.19.0.2"}},
	}

	// Act
	body := marshalToMap(t, nd)

	// Assert
	// NetworkSummary's 8 embedded fields, flattened, plus "containers".
	if len(body) != 9 {
		t.Fatalf("NetworkDetail payload has %d keys (%v), want exactly 9", len(body), keysOf(body))
	}
}

func TestVolumeSummaryFieldCount(t *testing.T) {
	t.Parallel()

	// Arrange
	size := int64(4096)
	vs := VolumeSummary{
		Name:       "tardis",
		Driver:     "local",
		Mountpoint: "/var/lib/docker/volumes/tardis/_data",
		CreatedAt:  "2023-11-14T22:13:20Z",
		Scope:      "local",
		Labels:     map[string]string{"app": "myapp"},
		SizeBytes:  &size,
	}

	// Act
	body := marshalToMap(t, vs)

	// Assert
	if len(body) != 7 {
		t.Fatalf("VolumeSummary payload has %d keys (%v), want exactly 7", len(body), keysOf(body))
	}
}

// TestLogLineFieldCount is the D9/D3 guard: Truncated is the only omitempty
// field, so an ordinary line marshals to exactly three keys and a truncated
// one to four.
func TestLogLineFieldCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line LogLine
		want int
	}{
		{
			name: "ordinary line has three keys",
			line: LogLine{Timestamp: "2023-11-14T22:13:20.5Z", Stream: "stdout", Line: "listening on :8080"},
			want: 3,
		},
		{
			name: "truncated line has four keys",
			line: LogLine{Timestamp: "2023-11-14T22:13:20.5Z", Stream: "stderr", Line: "panic: ...", Truncated: true},
			want: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			body := marshalToMap(t, tt.line)

			// Assert
			if len(body) != tt.want {
				t.Fatalf("LogLine payload has %d keys (%v), want exactly %d", len(body), keysOf(body), tt.want)
			}
		})
	}
}

func TestListResultFieldCount(t *testing.T) {
	t.Parallel()

	// Arrange
	lr := ListResult[ContainerSummary]{
		Items:     []ContainerSummary{{ID: "abc123"}},
		Truncated: true,
	}

	// Act
	body := marshalToMap(t, lr)

	// Assert
	if len(body) != 2 {
		t.Fatalf("ListResult payload has %d keys (%v), want exactly 2", len(body), keysOf(body))
	}
}

// TestContainerDetailNeverCarriesEnv is the D2 guard: it asserts on the
// marshalled JSON string rather than on struct fields, so a future embedded
// struct or a removed json:"-" cannot slip a leaked env var past this test.
func TestContainerDetailNeverCarriesEnv(t *testing.T) {
	t.Parallel()

	// Arrange
	r := container.InspectResponse{
		ID: "abc123",
		Config: &container.Config{
			Env: []string{"DB_PASSWORD=hunter2"},
		},
	}

	// Act
	got := toContainerDetail(r, "")
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal(got) error = %v", err)
	}

	// Assert
	body := string(raw)
	if strings.Contains(body, `"env"`) || strings.Contains(body, "hunter2") {
		t.Errorf("marshalled ContainerDetail leaks env data: %s", body)
	}
}

// TestImageDetailNeverCarriesEnv and TestVolumeSummaryNeverCarriesOptions are
// deliberately not duplicated here: internal/dockerx/objects_test.go already
// asserts both on marshalled JSON (Task 5), and re-adding them under
// different names would be redundant coverage of the same D2 guarantee
// rather than a new guard.

// TestContainerSummaryProtected is the D18 guard: protected has no
// omitempty, so a non-self container must still marshal "protected":false
// rather than omitting the field, and a container whose ID equals the self
// ID must marshal "protected":true.
func TestContainerSummaryProtected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cs   ContainerSummary
		want string
	}{
		{
			name: "non-self container marshals protected false",
			cs:   ContainerSummary{ID: "abc123", Protected: false},
			want: `"protected":false`,
		},
		{
			name: "self container marshals protected true",
			cs:   ContainerSummary{ID: "abc123", Protected: true},
			want: `"protected":true`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			raw, err := json.Marshal(tt.cs)
			if err != nil {
				t.Fatalf("json.Marshal(tt.cs) error = %v", err)
			}

			// Assert
			got := string(raw)
			if !strings.Contains(got, tt.want) {
				t.Errorf("marshalled ContainerSummary = %s, want it to contain %s", got, tt.want)
			}
		})
	}
}

// TestEmptyListMarshalsAsArray asserts a zero-item ListResult marshals its
// Items field as an empty array, never null, so a client never has to handle
// both shapes.
func TestEmptyListMarshalsAsArray(t *testing.T) {
	t.Parallel()

	// Arrange
	lr := ListResult[ContainerSummary]{Items: []ContainerSummary{}, Truncated: false}

	// Act
	raw, err := json.Marshal(lr)
	if err != nil {
		t.Fatalf("json.Marshal(lr) error = %v", err)
	}

	// Assert
	if !strings.Contains(string(raw), `"items":[]`) {
		t.Errorf("marshalled ListResult = %s, want it to contain \"items\":[]", raw)
	}
	if strings.Contains(string(raw), `"items":null`) {
		t.Errorf("marshalled ListResult = %s, items must never be null", raw)
	}
}
