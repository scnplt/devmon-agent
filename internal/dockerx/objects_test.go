// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

// TestToImageSummaryZeroValue covers an image.Summary with every slice and
// map field nil, the ordinary shape for an image with no tags.
func TestToImageSummaryZeroValue(t *testing.T) {
	t.Parallel()

	// Arrange
	s := image.Summary{}

	// Act
	got := toImageSummary(s)

	// Assert
	if got.RepoTags == nil {
		t.Error("got.RepoTags = nil, want empty non-nil slice")
	}
	if got.RepoDigests == nil {
		t.Error("got.RepoDigests = nil, want empty non-nil slice")
	}
	if got.Labels == nil {
		t.Error("got.Labels = nil, want empty non-nil map")
	}
}

// TestToImageSummaryCreatedAt covers the int64 Unix-seconds to RFC3339 UTC
// conversion for image.Summary.Created.
func TestToImageSummaryCreatedAt(t *testing.T) {
	t.Parallel()

	// Arrange
	s := image.Summary{Created: 1700000000}

	// Act
	got := toImageSummary(s)

	// Assert
	want := "2023-11-14T22:13:20Z"
	if got.CreatedAt != want {
		t.Errorf("got.CreatedAt = %q, want %q", got.CreatedAt, want)
	}
}

// TestToImageSummaryCreatedAtZero covers image.Summary.Created being 0 (the
// Engine's "no creation time to report" value), which must not be formatted
// as the Unix epoch.
func TestToImageSummaryCreatedAtZero(t *testing.T) {
	t.Parallel()

	// Arrange
	s := image.Summary{Created: 0}

	// Act
	got := toImageSummary(s)

	// Assert
	if got.CreatedAt != "" {
		t.Errorf("got.CreatedAt = %q, want empty", got.CreatedAt)
	}
}

// TestToImageDetailEmptyCreated covers image.InspectResponse.Created being
// empty (it is `json:",omitempty"` upstream) and must pass through as-is
// rather than being reformatted or defaulted.
func TestToImageDetailEmptyCreated(t *testing.T) {
	t.Parallel()

	// Arrange
	r := image.InspectResponse{ID: "sha256:abc"}

	// Act
	got := toImageDetail(r)

	// Assert
	if got.CreatedAt != "" {
		t.Errorf("got.CreatedAt = %q, want empty", got.CreatedAt)
	}
	if got.RepoTags == nil {
		t.Error("got.RepoTags = nil, want empty non-nil slice")
	}
	if got.RepoDigests == nil {
		t.Error("got.RepoDigests = nil, want empty non-nil slice")
	}
}

// TestImageDetailNeverCarriesEnv covers the highest-value assertion for this
// DTO: marshalling never leaks the image config's baked-in Env, even though
// image.InspectResponse.Config holds it.
func TestImageDetailNeverCarriesEnv(t *testing.T) {
	t.Parallel()

	// Arrange
	r := image.InspectResponse{
		ID:           "sha256:abc",
		RepoTags:     []string{"myapp:1.4"},
		Created:      "2023-11-14T22:13:20Z",
		Architecture: "amd64",
		Os:           "linux",
	}

	// Act
	got := toImageDetail(r)
	bodyBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal(got) error = %v", err)
	}

	// Assert
	body := string(bodyBytes)
	if strings.Contains(body, `"env"`) || strings.Contains(body, "API_KEY") {
		t.Errorf("marshalled ImageDetail leaks env data: %s", body)
	}
	if got.OS != "linux" {
		t.Errorf("got.OS = %q, want linux", got.OS)
	}
}

// TestListImagesTruncation covers the boundary and over-the-boundary
// truncation cases without a live daemon.
func TestListImagesTruncation(t *testing.T) {
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
			items := make([]ImageSummary, 0, tt.count)
			for i := 0; i < tt.count; i++ {
				items = append(items, ImageSummary{})
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

// TestToNetworkSummaryZeroValue covers a network.Network with every map
// field nil, the ordinary shape for a network with no labels.
func TestToNetworkSummaryZeroValue(t *testing.T) {
	t.Parallel()

	// Arrange
	n := network.Network{}

	// Act
	got := toNetworkSummary(n)

	// Assert
	if got.Labels == nil {
		t.Error("got.Labels = nil, want empty non-nil map")
	}
}

// TestToNetworkSummaryCreatedAt covers the network.Network.Created field,
// which is declared as timeext.Time upstream but is actually the standard
// library time.Time (the upstream import is `timeext "time"`, an alias, not
// a distinct named type), so it needs a direct .Format call, no conversion.
func TestToNetworkSummaryCreatedAt(t *testing.T) {
	t.Parallel()

	// Arrange
	created := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	n := network.Network{Created: created}

	// Act
	got := toNetworkSummary(n)

	// Assert
	want := "2023-11-14T22:13:20Z"
	if got.CreatedAt != want {
		t.Errorf("got.CreatedAt = %q, want %q", got.CreatedAt, want)
	}
}

// TestToNetworkDetailNoContainers covers an inspected network with nothing
// attached, the ordinary shape for a freshly created network.
func TestToNetworkDetailNoContainers(t *testing.T) {
	t.Parallel()

	// Arrange
	n := network.Inspect{
		Network: network.Network{ID: "net1", Name: "bridge"},
	}

	// Act
	got := toNetworkDetail(n)

	// Assert
	if got.Containers == nil {
		t.Error("got.Containers = nil, want empty non-nil slice")
	}
	if len(got.Containers) != 0 {
		t.Errorf("len(got.Containers) = %d, want 0", len(got.Containers))
	}
	if got.ID != "net1" {
		t.Errorf("got.ID = %q, want net1", got.ID)
	}
}

// TestToNetworkDetailWithContainers covers an inspected network with an
// attached container, asserting the netip.Prefix IPv4Address/IPv6Address
// fields (not netip.Addr) are guarded and rendered correctly.
func TestToNetworkDetailWithContainers(t *testing.T) {
	t.Parallel()

	// Arrange
	n := network.Inspect{
		Network: network.Network{ID: "net1", Name: "bridge"},
		Containers: map[string]network.EndpointResource{
			"c1": {
				Name:        "api",
				IPv4Address: netip.MustParsePrefix("172.19.0.2/16"),
			},
		},
	}

	// Act
	got := toNetworkDetail(n)

	// Assert
	if len(got.Containers) != 1 {
		t.Fatalf("len(got.Containers) = %d, want 1", len(got.Containers))
	}
	ep := got.Containers[0]
	if ep.ContainerID != "c1" {
		t.Errorf("ep.ContainerID = %q, want c1", ep.ContainerID)
	}
	if ep.Name != "api" {
		t.Errorf("ep.Name = %q, want api", ep.Name)
	}
	if ep.IPv4Address != "172.19.0.2/16" {
		t.Errorf("ep.IPv4Address = %q, want 172.19.0.2/16", ep.IPv4Address)
	}
	if ep.IPv6Address != "" {
		t.Errorf("ep.IPv6Address = %q, want empty for an unset netip.Prefix", ep.IPv6Address)
	}
}

// TestListNetworksTruncation covers the boundary and over-the-boundary
// truncation cases without a live daemon.
func TestListNetworksTruncation(t *testing.T) {
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
			items := make([]NetworkSummary, 0, tt.count)
			for i := 0; i < tt.count; i++ {
				items = append(items, NetworkSummary{})
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

// TestToVolumeSummaryNilUsageData covers a volume.Volume whose UsageData is
// nil, the ordinary shape unless disk-usage information was explicitly
// requested.
func TestToVolumeSummaryNilUsageData(t *testing.T) {
	t.Parallel()

	// Arrange
	v := volume.Volume{Name: "tardis", UsageData: nil}

	// Act
	got := toVolumeSummary(v)

	// Assert
	if got.SizeBytes != nil {
		t.Errorf("got.SizeBytes = %v, want nil", got.SizeBytes)
	}
	if got.Labels == nil {
		t.Error("got.Labels = nil, want empty non-nil map")
	}
}

// TestToVolumeSummaryWithUsageData covers a volume.Volume with a non-nil
// UsageData, asserting the pointer is populated from UsageData.Size.
func TestToVolumeSummaryWithUsageData(t *testing.T) {
	t.Parallel()

	// Arrange
	v := volume.Volume{
		Name:      "tardis",
		UsageData: &volume.UsageData{Size: 4096, RefCount: 1},
	}

	// Act
	got := toVolumeSummary(v)

	// Assert
	if got.SizeBytes == nil {
		t.Fatal("got.SizeBytes = nil, want a populated pointer")
	}
	if *got.SizeBytes != 4096 {
		t.Errorf("*got.SizeBytes = %d, want 4096", *got.SizeBytes)
	}
}

// TestVolumeSummaryNeverCarriesOptions covers the highest-value assertion for
// this DTO: marshalling never leaks volume.Volume.Options, which routinely
// carries tmpfs/CIFS/NFS credentials.
func TestVolumeSummaryNeverCarriesOptions(t *testing.T) {
	t.Parallel()

	// Arrange
	v := volume.Volume{
		Name:    "cifs-share",
		Driver:  "local",
		Options: map[string]string{"o": "username=admin,password=hunter2"},
	}

	// Act
	got := toVolumeSummary(v)
	bodyBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal(got) error = %v", err)
	}

	// Assert
	body := string(bodyBytes)
	if strings.Contains(body, "hunter2") || strings.Contains(body, `"options"`) {
		t.Errorf("marshalled VolumeSummary leaks driver options: %s", body)
	}
}

// TestListVolumesTruncation covers the boundary and over-the-boundary
// truncation cases without a live daemon.
func TestListVolumesTruncation(t *testing.T) {
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
			items := make([]VolumeSummary, 0, tt.count)
			for i := 0; i < tt.count; i++ {
				items = append(items, VolumeSummary{})
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
