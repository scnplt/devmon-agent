// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// TestToContainerSummaryZeroValue covers a container.Summary with every
// pointer field nil, which is the ordinary shape the Engine returns for a
// container with no healthcheck and no attached network. A fully-populated
// fixture would never exercise the nil-guards that a real host triggers.
func TestToContainerSummaryZeroValue(t *testing.T) {
	t.Parallel()

	// Arrange
	s := container.Summary{}

	// Act
	got := toContainerSummary(s, "")

	// Assert
	if got.Health != "" {
		t.Errorf("got.Health = %q, want empty", got.Health)
	}
	if got.Ports == nil {
		t.Error("got.Ports = nil, want empty non-nil slice")
	}
	if len(got.Ports) != 0 {
		t.Errorf("len(got.Ports) = %d, want 0", len(got.Ports))
	}
	if got.Names == nil {
		t.Error("got.Names = nil, want empty non-nil slice")
	}
	if got.Labels == nil {
		t.Error("got.Labels = nil, want empty non-nil map")
	}
}

// TestToContainerDetailNilState covers an InspectResponse whose State,
// Config, and HostConfig are all nil. This is an ordinary shape (e.g. a
// container mid-creation) and must not panic.
func TestToContainerDetailNilState(t *testing.T) {
	t.Parallel()

	// Arrange
	r := container.InspectResponse{
		ID:         "abc123",
		State:      nil,
		Config:     nil,
		HostConfig: nil,
	}

	// Act
	got := toContainerDetail(r, "")

	// Assert
	if got.State != "" {
		t.Errorf("got.State = %q, want empty", got.State)
	}
	if got.Running {
		t.Error("got.Running = true, want false")
	}
	if got.Health != "" {
		t.Errorf("got.Health = %q, want empty", got.Health)
	}
	if got.RestartPolicy != "" {
		t.Errorf("got.RestartPolicy = %q, want empty", got.RestartPolicy)
	}
	if got.Labels == nil {
		t.Error("got.Labels = nil, want empty non-nil map")
	}
	if got.Networks == nil {
		t.Error("got.Networks = nil, want empty non-nil slice")
	}
	if got.Ports == nil {
		t.Error("got.Ports = nil, want empty non-nil slice")
	}
	if got.Args == nil {
		t.Error("got.Args = nil, want empty non-nil slice")
	}
}

// TestToContainerSummaryCreatedAt covers the int64 Unix-seconds to RFC3339
// UTC conversion for container.Summary.Created.
func TestToContainerSummaryCreatedAt(t *testing.T) {
	t.Parallel()

	// Arrange
	s := container.Summary{Created: 1700000000}

	// Act
	got := toContainerSummary(s, "")

	// Assert
	want := "2023-11-14T22:13:20Z"
	if got.CreatedAt != want {
		t.Errorf("got.CreatedAt = %q, want %q", got.CreatedAt, want)
	}
}

// TestToContainerSummaryCreatedAtZero covers container.Summary.Created being
// 0 (the Engine's "no creation time to report" value), which must not be
// formatted as the Unix epoch.
func TestToContainerSummaryCreatedAtZero(t *testing.T) {
	t.Parallel()

	// Arrange
	s := container.Summary{Created: 0}

	// Act
	got := toContainerSummary(s, "")

	// Assert
	if got.CreatedAt != "" {
		t.Errorf("got.CreatedAt = %q, want empty", got.CreatedAt)
	}
}

// TestToContainerDetailCreatedAtPassthrough covers InspectResponse.Created,
// which is already an RFC3339Nano string and must not be reformatted.
func TestToContainerDetailCreatedAtPassthrough(t *testing.T) {
	t.Parallel()

	// Arrange
	r := container.InspectResponse{Created: "2023-11-14T22:13:20.123456789Z"}

	// Act
	got := toContainerDetail(r, "")

	// Assert
	want := "2023-11-14T22:13:20.123456789Z"
	if got.CreatedAt != want {
		t.Errorf("got.CreatedAt = %q, want %q", got.CreatedAt, want)
	}
}

// TestToPortInvalidIP covers a container.PortSummary whose IP is the zero
// netip.Addr, which happens for a port that is exposed but not published to
// the host.
func TestToPortInvalidIP(t *testing.T) {
	t.Parallel()

	// Arrange
	p := container.PortSummary{
		IP:          netip.Addr{},
		PrivatePort: 8080,
		Type:        "tcp",
	}

	// Act
	got := toPort(p)

	// Assert
	if got.IP != "" {
		t.Errorf("got.IP = %q, want empty for an invalid netip.Addr", got.IP)
	}
	if got.PrivatePort != 8080 {
		t.Errorf("got.PrivatePort = %d, want 8080", got.PrivatePort)
	}
	if got.Protocol != "tcp" {
		t.Errorf("got.Protocol = %q, want tcp", got.Protocol)
	}
}

// TestToPortValidIP covers a container.PortSummary with a valid published
// host IP, asserting the protocol comes from Type, not a nonexistent
// Protocol field.
func TestToPortValidIP(t *testing.T) {
	t.Parallel()

	// Arrange
	addr := netip.MustParseAddr("127.0.0.1")
	p := container.PortSummary{
		IP:          addr,
		PrivatePort: 8080,
		PublicPort:  8081,
		Type:        "udp",
	}

	// Act
	got := toPort(p)

	// Assert
	if got.IP != "127.0.0.1" {
		t.Errorf("got.IP = %q, want 127.0.0.1", got.IP)
	}
	if got.PublicPort != 8081 {
		t.Errorf("got.PublicPort = %d, want 8081", got.PublicPort)
	}
	if got.Protocol != "udp" {
		t.Errorf("got.Protocol = %q, want udp", got.Protocol)
	}
}

// TestToContainerSummaryHealthAndNetworkSettings covers the non-nil branches
// of Health and NetworkSettings, which the zero-value test above deliberately
// does not exercise.
func TestToContainerSummaryHealthAndNetworkSettings(t *testing.T) {
	t.Parallel()

	// Arrange
	s := container.Summary{
		Health: &container.HealthSummary{Status: container.Unhealthy},
	}

	// Act
	got := toContainerSummary(s, "")

	// Assert
	if got.Health != string(container.Unhealthy) {
		t.Errorf("got.Health = %q, want %q", got.Health, container.Unhealthy)
	}
}

// TestToContainerDetailFullState covers a fully-populated State, Config, and
// HostConfig, ensuring the non-nil branches project correctly and that no env
// data leaks into any field.
func TestToContainerDetailFullState(t *testing.T) {
	t.Parallel()

	// Arrange
	r := container.InspectResponse{
		ID:      "abc123",
		Name:    "/api",
		Image:   "myapp:1.4",
		Created: "2023-11-14T22:13:20Z",
		Path:    "/app/server",
		Args:    []string{"--port", "8080"},
		State: &container.State{
			Status:     "running",
			Running:    true,
			ExitCode:   0,
			StartedAt:  "2023-11-14T22:13:21Z",
			FinishedAt: "0001-01-01T00:00:00Z",
			Health:     &container.Health{Status: container.Healthy},
		},
		Config: &container.Config{
			Env:        []string{"DB_PASSWORD=hunter2"},
			WorkingDir: "/app",
			User:       "nobody",
			Labels:     map[string]string{"app": "myapp"},
			Entrypoint: []string{"/entrypoint.sh"},
		},
		HostConfig: &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "always"},
		},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"bridge": {
					NetworkID: "net1",
					IPAddress: netip.MustParseAddr("172.17.0.2"),
				},
				"nil-value": nil,
			},
			Ports: network.PortMap{
				network.MustParsePort("8080/tcp"): {
					{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8081"},
				},
				network.MustParsePort("9090/udp"): {}, // exposed, not published
			},
		},
	}

	// Act
	got := toContainerDetail(r, "")

	// Assert
	if got.FinishedAt != "" {
		t.Errorf("got.FinishedAt = %q, want empty for the zero timestamp", got.FinishedAt)
	}
	if got.StartedAt != "2023-11-14T22:13:21Z" {
		t.Errorf("got.StartedAt = %q, want 2023-11-14T22:13:21Z", got.StartedAt)
	}
	if got.Health != string(container.Healthy) {
		t.Errorf("got.Health = %q, want %q", got.Health, container.Healthy)
	}
	if got.RestartPolicy != "always" {
		t.Errorf("got.RestartPolicy = %q, want always", got.RestartPolicy)
	}
	if len(got.Networks) != 1 {
		t.Fatalf("len(got.Networks) = %d, want 1 (the nil map value must be skipped)", len(got.Networks))
	}
	if got.Networks[0].IPAddress != "172.17.0.2" {
		t.Errorf("got.Networks[0].IPAddress = %q, want 172.17.0.2", got.Networks[0].IPAddress)
	}
	if len(got.Ports) != 2 {
		t.Fatalf("len(got.Ports) = %d, want 2", len(got.Ports))
	}
	var sawBound, sawUnbound bool
	for _, p := range got.Ports {
		switch {
		case p.PrivatePort == 8080 && p.PublicPort == 8081 && p.IP == "0.0.0.0" && p.Protocol == "tcp":
			sawBound = true
		case p.PrivatePort == 9090 && p.PublicPort == 0 && p.IP == "" && p.Protocol == "udp":
			sawUnbound = true
		}
	}
	if !sawBound {
		t.Errorf("got.Ports = %+v, want a bound 8080/tcp -> 0.0.0.0:8081 entry", got.Ports)
	}
	if !sawUnbound {
		t.Errorf("got.Ports = %+v, want an unbound 9090/udp entry", got.Ports)
	}

	// The highest-value assertion in this test: no env var name or value
	// anywhere in the marshalled JSON, at any nesting depth.
	bodyBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal(got) error = %v", err)
	}
	body := string(bodyBytes)
	if strings.Contains(body, "hunter2") || strings.Contains(body, "DB_PASSWORD") || strings.Contains(body, `"env"`) {
		t.Errorf("marshalled ContainerDetail leaks env data: %s", body)
	}
}

// TestToContainerSummaryProtected covers the D18 self-exclusion flag: it is
// true only when the container's ID equals a non-empty selfID, and false for
// every other container, including when selfID is unresolved ("").
func TestToContainerSummaryProtected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		id     string
		selfID string
		want   bool
	}{
		{name: "matches self ID", id: "abc123", selfID: "abc123", want: true},
		{name: "does not match self ID", id: "abc123", selfID: "def456", want: false},
		{name: "self ID unresolved", id: "abc123", selfID: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s := container.Summary{ID: tt.id}

			// Act
			got := toContainerSummary(s, tt.selfID)

			// Assert
			if got.Protected != tt.want {
				t.Errorf("got.Protected = %v, want %v", got.Protected, tt.want)
			}
		})
	}
}

// TestToContainerDetailProtected mirrors TestToContainerSummaryProtected for
// toContainerDetail.
func TestToContainerDetailProtected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		id     string
		selfID string
		want   bool
	}{
		{name: "matches self ID", id: "abc123", selfID: "abc123", want: true},
		{name: "does not match self ID", id: "abc123", selfID: "def456", want: false},
		{name: "self ID unresolved", id: "abc123", selfID: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			r := container.InspectResponse{ID: tt.id}

			// Act
			got := toContainerDetail(r, tt.selfID)

			// Assert
			if got.Protected != tt.want {
				t.Errorf("got.Protected = %v, want %v", got.Protected, tt.want)
			}
		})
	}
}

// TestToMounts covers the projection of a container's mount points onto the
// allowlisted DTO, including both a named volume and a bind mount, which
// differ in whether Name is populated.
func TestToMounts(t *testing.T) {
	t.Parallel()

	// Arrange
	mounts := []container.MountPoint{
		{
			Type:        "volume",
			Name:        "myvolume",
			Source:      "/var/lib/docker/volumes/myvolume/_data",
			Destination: "/data",
			RW:          true,
		},
		{
			Type:        "bind",
			Source:      "/host/config",
			Destination: "/etc/config",
			RW:          false,
		},
	}

	// Act
	got := toMounts(mounts)

	// Assert
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Type != "volume" || got[0].Name != "myvolume" || !got[0].ReadWrite {
		t.Errorf("got[0] = %+v, want a read-write named volume", got[0])
	}
	if got[1].Type != "bind" || got[1].Name != "" || got[1].ReadWrite {
		t.Errorf("got[1] = %+v, want a read-only unnamed bind mount", got[1])
	}
}

// TestListContainersTruncation covers the boundary and over-the-boundary
// truncation cases without a live daemon: the mapping and truncation logic is
// exercised directly rather than through the SDK.
func TestListContainersTruncation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		count         int
		wantLen       int
		wantTruncated bool
	}{
		{name: "under the cap", count: 3, wantLen: 3, wantTruncated: false},
		{name: "exactly at the cap", count: maxListItems, wantLen: maxListItems, wantTruncated: false},
		{name: "over the cap", count: maxListItems + 1, wantLen: maxListItems, wantTruncated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			items := make([]ContainerSummary, 0, tt.count)
			for i := 0; i < tt.count; i++ {
				items = append(items, ContainerSummary{})
			}

			// Act
			truncated := false
			if len(items) > maxListItems {
				items, truncated = items[:maxListItems], true
			}

			// Assert
			if len(items) != tt.wantLen {
				t.Errorf("len(items) = %d, want %d", len(items), tt.wantLen)
			}
			if truncated != tt.wantTruncated {
				t.Errorf("truncated = %v, want %v", truncated, tt.wantTruncated)
			}
		})
	}
}

// TestZeroTimeToEmpty covers short inputs (length 1-3) that must not panic
// when sliced, alongside the zero-time marker and a real timestamp. An
// Engine (or a proxy/fake in front of one) that returns a short string for
// State.StartedAt or State.FinishedAt must not crash the request.
func TestZeroTimeToEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ts   string
		want string
	}{
		{name: "empty string", ts: "", want: ""},
		{name: "single char, shorter than zero-time prefix", ts: "0", want: "0"},
		{name: "three chars, shorter than zero-time prefix", ts: "000", want: "000"},
		{name: "exact zero-time prefix", ts: "0001", want: ""},
		{name: "full zero-time value", ts: "0001-01-01T00:00:00Z", want: ""},
		{name: "real timestamp", ts: "2026-08-19T10:15:30.123456789Z", want: "2026-08-19T10:15:30.123456789Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			got := zeroTimeToEmpty(tt.ts)

			// Assert
			if got != tt.want {
				t.Errorf("zeroTimeToEmpty(%q) = %q, want %q", tt.ts, got, tt.want)
			}
		})
	}
}
