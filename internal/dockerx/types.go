package dockerx

// Response DTOs for the read operations (Phase 3).
//
// D1: this file is the complete, explicit allowlist of what a paired device is
// permitted to learn about a Docker object. Nothing in internal/dockerx forwards
// an Engine struct verbatim — every field below was added on purpose, and adding
// a new one is a deliberate act that must also update the corresponding
// *FieldCount test.
//
// D2: environment variables are never returned, at any level. container.Config.Env,
// the image config's baked-in Env, and volume.Volume.Options (which routinely
// carries tmpfs/CIFS/NFS credentials) have no field here and must never gain one.
// Redacting a value still discloses that a secret exists and its name; omission is
// the only version of this that cannot leak.

// ListResult is the envelope every list route returns. A bare top-level JSON
// array cannot gain a field later without breaking every client, and Truncated
// needs somewhere to live from day one (D9).
type ListResult[T any] struct {
	Items     []T  `json:"items"`
	Truncated bool `json:"truncated"`
}

// Port is a single published or exposed container port.
type Port struct {
	IP          string `json:"ip,omitempty"` // from netip.Addr; "" when !IsValid()
	PrivatePort uint16 `json:"private_port"`
	PublicPort  uint16 `json:"public_port,omitempty"`
	Protocol    string `json:"protocol"` // from PortSummary.Type
}

// Mount is a single filesystem mount attached to a container.
type Mount struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadWrite   bool   `json:"read_write"`
}

// EndpointSummary is a container's attachment to a single network.
type EndpointSummary struct {
	NetworkName string   `json:"network_name"`
	NetworkID   string   `json:"network_id"`
	IPAddress   string   `json:"ip_address,omitempty"`
	Gateway     string   `json:"gateway,omitempty"`
	MACAddress  string   `json:"mac_address,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

// ContainerSummary is one entry of a container list. 11 fields.
type ContainerSummary struct {
	ID        string            `json:"id"`
	Names     []string          `json:"names"`
	Image     string            `json:"image"`
	ImageID   string            `json:"image_id"`
	Command   string            `json:"command"`
	CreatedAt string            `json:"created_at"` // RFC3339 UTC
	State     string            `json:"state"`
	Status    string            `json:"status"`
	Health    string            `json:"health,omitempty"` // "" when Health is nil
	Labels    map[string]string `json:"labels"`
	Ports     []Port            `json:"ports"`
}

// ContainerDetail is the full projection of a single container. 24 fields.
// NO env. NO raw Engine payload.
type ContainerDetail struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	CreatedAt     string            `json:"created_at"` // RFC3339 UTC
	State         string            `json:"state"`
	Running       bool              `json:"running"`
	Paused        bool              `json:"paused"`
	Restarting    bool              `json:"restarting"`
	ExitCode      int               `json:"exit_code"`
	StartedAt     string            `json:"started_at,omitempty"`
	FinishedAt    string            `json:"finished_at,omitempty"`
	Health        string            `json:"health,omitempty"`
	RestartCount  int               `json:"restart_count"`
	RestartPolicy string            `json:"restart_policy,omitempty"`
	Platform      string            `json:"platform"`
	Labels        map[string]string `json:"labels"`
	Command       string            `json:"command"` // InspectResponse.Path
	Args          []string          `json:"args"`
	Entrypoint    []string          `json:"entrypoint,omitempty"`
	WorkingDir    string            `json:"working_dir,omitempty"`
	User          string            `json:"user,omitempty"`
	Mounts        []Mount           `json:"mounts"`
	Networks      []EndpointSummary `json:"networks"`
	Ports         []Port            `json:"ports"`
}

// ImageSummary is one entry of an image list. 8 fields.
type ImageSummary struct {
	ID          string            `json:"id"`
	ParentID    string            `json:"parent_id,omitempty"`
	RepoTags    []string          `json:"repo_tags"`
	RepoDigests []string          `json:"repo_digests"`
	CreatedAt   string            `json:"created_at"` // RFC3339 UTC from int64
	Size        int64             `json:"size"`
	Containers  int64             `json:"containers"` // -1 means "not calculated"
	Labels      map[string]string `json:"labels"`
}

// ImageDetail is the full projection of a single image. 9 fields.
//
// image.InspectResponse.Config carries the image's baked-in Env. It is NOT
// mapped. D2 applies to images too.
type ImageDetail struct {
	ID           string   `json:"id"`
	RepoTags     []string `json:"repo_tags"`
	RepoDigests  []string `json:"repo_digests"`
	CreatedAt    string   `json:"created_at,omitempty"`
	Size         int64    `json:"size"`
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	Author       string   `json:"author,omitempty"`
	Comment      string   `json:"comment,omitempty"`
}

// NetworkSummary is one entry of a network list. 8 fields.
type NetworkSummary struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Scope      string            `json:"scope"`
	CreatedAt  string            `json:"created_at"`
	Internal   bool              `json:"internal"`
	EnableIPv6 bool              `json:"enable_ipv6"`
	Labels     map[string]string `json:"labels"`
}

// NetworkDetail is a NetworkSummary plus the containers attached to it.
type NetworkDetail struct {
	NetworkSummary
	Containers []NetworkEndpoint `json:"containers"`
}

// NetworkEndpoint is a single container's attachment to an inspected network.
type NetworkEndpoint struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	IPv4Address string `json:"ipv4_address,omitempty"`
	IPv6Address string `json:"ipv6_address,omitempty"`
}

// LogLine is one demultiplexed line of container output. Unlike every other DTO
// in this package, its payload is not a projection of Engine metadata — Line is
// whatever the container printed, which is precisely what the operator asked
// for. That makes it the one field in the codebase that may legitimately carry
// a secret, and the reason D16 forbids writing it to the agent's own log.
type LogLine struct {
	Timestamp string `json:"ts"`                  // RFC3339Nano; "" if the Engine emitted no parsable prefix
	Stream    string `json:"stream"`              // "stdout" or "stderr"
	Line      string `json:"line"`
	Truncated bool   `json:"truncated,omitempty"` // set when the line exceeded maxLogLineBytes
}

// VolumeSummary is both a volume list entry and a volume inspect response
// (VolumeDetail is VolumeSummary; there is no separate detail shape). 7 fields.
//
// volume.Volume.Options is NOT mapped: for tmpfs and CIFS/NFS volumes it
// routinely contains credentials (o=username=...,password=...). D2's reasoning
// applies verbatim.
type VolumeSummary struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Mountpoint string            `json:"mountpoint"`
	CreatedAt  string            `json:"created_at,omitempty"`
	Scope      string            `json:"scope"`
	Labels     map[string]string `json:"labels"`
	SizeBytes  *int64            `json:"size_bytes,omitempty"` // from UsageData.Size when non-nil
}
