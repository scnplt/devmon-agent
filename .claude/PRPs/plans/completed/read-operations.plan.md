# Plan: Read Operations (DevMon Agent Phase 3)

## Summary

Give a paired device full read visibility into the host's Docker objects: list and inspect for
containers, images, networks, and volumes. Eight new mTLS-guarded `GET` routes, backed by a new
read-only surface on `internal/dockerx`, projecting Docker Engine responses through **explicit
allowlisted DTOs** rather than forwarding raw Engine JSON.

This is the first phase where host data leaves the agent. The entire security weight of the phase
sits in one decision: what a response is *allowed* to contain. Forwarding `container.InspectResponse`
verbatim would put `Config.Env` — every database password, API key, and token the operator baked
into a container — onto a phone over a channel the PRD never budgeted for carrying secrets.

## User Story

As an operator diagnosing a problem from my phone,
I want to see every container, image, network, and volume on the host and drill into any one of them,
so that I can identify what is broken before I act on it.

## Problem → Solution

A paired device today can do exactly three things: read `/v1/status`, renew its own certificate, and
unpair itself. It cannot see a single container. → A paired device can enumerate and inspect all four
Docker object types over the authenticated channel, receiving a stable, versioned, secret-free
projection of the Engine's data.

## Metadata

- **Complexity**: Medium–Large (18 files, 10 tasks; ~900 lines of production Go plus tests)
- **Source PRD**: `.claude/PRPs/prds/devmon-agent.prd.md`
- **PRD Phase**: 3 — Read operations
- **Depends on**: Phase 2 (complete)
- **Parallel with**: Phase 4 (Logs & live streaming) — they share no code beyond `dockerx.Client`
- **Estimated Files**: 18 changed (12 created, 6 updated)

---

## Decisions Settled Before Planning

Each has a plausible-looking wrong answer. Settled here so implementation never re-litigates them.

| # | Decision | Choice | Why not the alternative |
|---|---|---|---|
| D1 | Response shape | **Explicit DTOs in `internal/dockerx`, field-by-field allowlist**, snake_case JSON | Forwarding `container.InspectResponse` (or its `Raw json.RawMessage`) leaks `Config.Env` — every secret the operator passed to a container — to the phone, and couples the versioned API contract to whatever the Engine changes next release. The PRD's whole premise is "a deliberate subset of Docker's surface"; the response body is part of that surface. |
| D2 | Environment variables | **Never returned, at any level.** `Config.Env` has no DTO field | Redacting values (`FOO=***`) still discloses which secrets exist and their names. Omission is the only version of this that cannot leak. An operator who needs env vars has host access. |
| D3 | Command and entrypoint | **Returned.** `command`, `entrypoint`, `args` are in the inspect DTO | Unlike `Env` these are what identifies a misconfigured container, and they are already visible in `docker ps` output the operator sees on the host. Documented in the threat model as a deliberate, bounded disclosure — a credential passed on a command line is already visible in the host's process table. |
| D4 | Where the DTOs live | **`internal/dockerx`** returns DTOs; `internal/httpapi` serialises them unchanged | Putting the mapping in `httpapi` would force `httpapi` to import `moby/api/types/...`, and Phase 5's audit code and Phase 4's log code would each need their own copy. `dockerx` owning the projection is what keeps "the agent never proxies arbitrary Docker" a structural property rather than a convention. |
| D5 | Testability of the Docker surface | **Four small interfaces** (`ContainerReader`, `ImageReader`, `NetworkReader`, `VolumeReader`) declared in `httpapi`, composed into `DockerReader`; `*dockerx.Client` satisfies it | Handlers must be testable without a live daemon (see `dockerx/client_test.go`: "a live daemon is deliberately not required"). Declaring the interfaces in the consumer, not the producer, is the repo's stated Go rule. Four 2-method interfaces rather than one 8-method one keeps each within the ≤3-method guideline. |
| D6 | Object reference validation | **Validated against `^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$` before it reaches the SDK**; anything else is 400 | The moby client interpolates the ID directly into the request URL path. `net/http.ServeMux` already refuses a `/` inside a `{id}` wildcard and cleans `..` segments, but relying on two layers of framework behaviour to keep `../../info` out of an Engine URL is exactly the kind of assumption that breaks on a dependency bump. Validate at the boundary, as the coding rules require. |
| D7 | Not-found mapping | `cerrdefs.IsNotFound(err)` → **404 with a terse fixed body**; every other Engine failure → **502** | The Engine is an upstream dependency of this agent, so its failures are gateway failures, not agent bugs — 502 says that accurately and keeps 500 meaning "the agent broke". A 404 is safe to distinguish here because the caller is already an authenticated device; the anti-enumeration reasoning that makes `/v1/pair` uniform does not apply behind `requireDevice`. |
| D8 | List bounds | **Hard server-side cap of 500 items**, plus a `truncated` boolean in the envelope | A host with thousands of images would otherwise produce a multi-megabyte body inside the server's 30s `WriteTimeout`, on a phone's mobile connection. A cap the client cannot raise is consistent with "startup configuration is the security boundary". `truncated` exists so the app can say "showing the first 500" rather than silently lying. |
| D9 | Response envelope | Lists return `{"items": [...], "truncated": bool}`; inspects return the object directly | A bare top-level JSON array cannot gain a field later without breaking every client. `truncated` needs somewhere to live from day one. |
| D10 | Policy gating | A `requireOp(policy.OpRead)` middleware wraps all eight routes now, even though `OpRead` is permitted by **every** mode | Zero behavioural change today, and it means Phase 5 adds tiers to existing plumbing rather than retrofitting a gate onto eight already-shipped routes. `policy.Allows` already fails closed on unregistered operations — this is what connects that guarantee to an actual route. |
| D11 | Per-call Engine timeout | **15s**, applied inside `dockerx` on every call | The server's `WriteTimeout` is 30s. A wedged Engine must surface as a 502 the agent controls, not as a connection the client sees die mid-body. 15s leaves headroom for the response write. |
| D12 | `all` parameter | `GET /v1/containers?all=true` only; images/networks/volumes take no parameters | Stopped containers are the ones an operator is looking for during an incident, so it must be reachable. `ImageListOptions.All` returns intermediate layers — noise on a phone, and not something the PRD asks for. Unknown query parameters are ignored, not rejected. |
| D13 | Agent self-marking (`protected: true` in listings) | **Not in this phase** | The PRD assigns self-exclusion to Phase 5. Marking requires the agent to resolve its own container ID (`/proc/self/cgroup` parsing, or `HOSTNAME` matching, neither reliable alone), which is a design problem that belongs with the enforcement that consumes it. |

---

## UX Design

### Before

```
┌────────────────────────────────────────────────────────────┐
│  Phone (paired, holding a valid client certificate)         │
│    GET  /v1/status          ──►  200 {version, policy, …}   │
│    POST /v1/device/renew    ──►  200 {cert, not_after}      │
│    DEL  /v1/device/self     ──►  204                        │
│    GET  /v1/containers      ──►  404   (no such route)      │
│                                                             │
│  The device is authenticated and can see nothing.           │
└────────────────────────────────────────────────────────────┘
```

### After

```
┌──────────────────────────────────────────────────────────────────────┐
│  Phone (paired)                                                       │
│    GET /v1/containers?all=true                                        │
│      ◄── 200 {"items":[{"id":"a1b2…","names":["/api"],                │
│                         "image":"myapp:1.4","state":"exited",         │
│                         "status":"Exited (1) 3 minutes ago",          │
│                         "health":"unhealthy","ports":[…]}],           │
│                "truncated":false}                                     │
│                                                                       │
│    GET /v1/containers/a1b2c3                                          │
│      ◄── 200 {"id":"a1b2c3…","name":"/api","state":"exited",          │
│               "command":"/app/server","args":["--port","8080"],       │
│               "mounts":[…],"networks":[…],"restart_count":5}          │
│               ── no "env" field exists, at any depth ──               │
│                                                                       │
│    GET /v1/images   /v1/images/{id}                                   │
│    GET /v1/networks /v1/networks/{id}                                 │
│    GET /v1/volumes  /v1/volumes/{name}                                │
│                                                                       │
│    GET /v1/containers/does-not-exist  ◄── 404 {"error":"not found"}   │
│    GET /v1/containers/%2e%2e         ◄── 400 {"error":"invalid …"}    │
│    (no client certificate)            ◄── 401 {"error":"client …"}    │
└──────────────────────────────────────────────────────────────────────┘
```

### Interaction Changes

| Touchpoint | Before | After | Notes |
|---|---|---|---|
| App home screen | Empty — nothing to show | Container list with state and health | The entry point for every diagnostic path |
| Container detail | Does not exist | Inspect projection: state, mounts, networks, ports, restart count | No env vars, by design (D2) |
| Images / networks / volumes tabs | Do not exist | List + detail | Explicit PRD "Must" requirements |
| Unknown object | n/a | 404 with a fixed terse body | Distinguishable from 401 and 502 |
| Engine down | n/a | 502 with a fixed terse body | The app can say "the host's Docker is not responding" rather than "the agent is broken" |
| Host with 900 images | n/a | First 500 items, `truncated:true` | App shows a "list truncated" affordance |

---

## Mandatory Reading

| Priority | File | Lines | Why |
|---|---|---|---|
| P0 | `internal/httpapi/server.go` | 37–91 | `Server` struct, `NewServer` signature, `routes()` — all three change |
| P0 | `internal/httpapi/respond.go` | 1–47 | `writeJSON` / `writeError` are the only response exits; every new handler uses them |
| P0 | `internal/httpapi/middleware.go` | 29–85 | `requireDevice` and `DeviceFrom` — the guard every new route sits behind |
| P0 | `internal/dockerx/client.go` | all (72) | The package doc explicitly reserves reads for this phase; `Client.api` is the handle to extend |
| P0 | `internal/policy/mode.go` | 28–65 | `Operation`, `OpRead`, `Mode.Allows` — the gate `requireOp` calls |
| P1 | `internal/httpapi/device.go` | 43–95 | The canonical guarded-handler shape: resolve device, do work, map error, write |
| P1 | `internal/httpapi/pair.go` | 16–43, 61–85 | Constant-block conventions for limits/messages; error → status mapping |
| P1 | `internal/httpapi/status_test.go` | 25–72 | `testLogger`, `testServer`, `testServerWithCA`, `testServerWithStore`, `peerCertWithSerial` — the helpers that need the new `NewServer` argument |
| P1 | `cmd/devmon-agent/main.go` | 113–174 | `serve` construction order; `dc` exists at line 155 and must reach `NewServer` at line 164 |
| P2 | `internal/dockerx/client_test.go` | all (61) | Test conventions for this package: `t.Parallel`, AAA comments, no live daemon |
| P2 | `internal/httpapi/status_test.go` | 74–106 | `TestStatusFieldCount` — the "field allowlist enforced by exact count" pattern this phase reuses for DTOs |
| P2 | `README.md` | 192–220 | The API table this phase extends |
| P2 | `Makefile` | 28–58 | The exact gate commands (`test-race`, `cover`, `lint`, `sec`) |

## External Documentation

| Topic | Source | Key Takeaway |
|---|---|---|
| moby client v29 read methods | `go doc github.com/moby/moby/client` (module `github.com/moby/moby/client v0.5.1`) | Every method takes an options struct and returns a `*Result` struct, not a bare slice. Verified signatures are transcribed below — do not write these from memory. |
| Engine API version | `client.MaxAPIVersion = "1.55"`, `MinAPIVersion = "1.40"` | Negotiated already at `dockerx.New` via `PingOptions{NegotiateAPIVersion: true}`. Nothing further needed. |
| Error classification | `github.com/containerd/errdefs` | `errdefs.IsNotFound(err)` is the supported way to detect a missing object. Currently an **indirect** dependency in `go.mod` — this phase promotes it to direct. |
| `net/http.ServeMux` wildcards (Go 1.22+) | stdlib | `mux.HandleFunc("GET /v1/containers/{id}", …)` + `r.PathValue("id")`. A `{id}` segment never matches a `/`. Method-prefixed patterns are already the repo convention (`server.go:79`). |

### Verified SDK signatures — transcribe exactly

```go
// github.com/moby/moby/client (v29 API)
func (cli *Client) ContainerList(ctx context.Context, options ContainerListOptions) (ContainerListResult, error)
func (cli *Client) ContainerInspect(ctx context.Context, containerID string, options ContainerInspectOptions) (ContainerInspectResult, error)
func (cli *Client) ImageList(ctx context.Context, options ImageListOptions) (ImageListResult, error)
func (cli *Client) ImageInspect(ctx context.Context, imageID string, inspectOpts ...ImageInspectOption) (ImageInspectResult, error)  // variadic, NOT an options struct
func (cli *Client) NetworkList(ctx context.Context, options NetworkListOptions) (NetworkListResult, error)
func (cli *Client) NetworkInspect(ctx context.Context, networkID string, options NetworkInspectOptions) (NetworkInspectResult, error)
func (cli *Client) VolumeList(ctx context.Context, options VolumeListOptions) (VolumeListResult, error)
func (cli *Client) VolumeInspect(ctx context.Context, volumeID string, options VolumeInspectOptions) (VolumeInspectResult, error)

type ContainerListResult    struct { Items []container.Summary }
type ContainerInspectResult struct { Container container.InspectResponse; Raw json.RawMessage }
type ImageListResult        struct { Items []image.Summary }
type ImageInspectResult     struct { image.InspectResponse }              // EMBEDDED, no named field
type NetworkListResult      struct { Items []network.Summary }            // network.Summary embeds network.Network
type NetworkInspectResult   struct { Network network.Inspect; Raw json.RawMessage }
type VolumeListResult       struct { Items []volume.Volume; Warnings []string }
type VolumeInspectResult    struct { Volume volume.Volume; Raw json.RawMessage }

type ContainerListOptions    struct { Size bool; All bool; Limit int; Filters Filters; /* + deprecated fields */ }
type ContainerInspectOptions struct { Size bool }
type ImageListOptions        struct { All bool; Filters Filters; SharedSize bool; Manifests bool; Identity bool }
type NetworkListOptions      struct { Filters Filters }
type NetworkInspectOptions   struct { Scope string; Verbose bool }
type VolumeListOptions       struct { Filters Filters }
type VolumeInspectOptions    struct { }
```

### Verified field types that will not compile if guessed

```go
container.Summary.State                       // container.ContainerState (a string type) — needs string(...)
container.Summary.Created                     // int64 Unix SECONDS — needs time.Unix(n, 0).UTC()
container.Summary.Health                      // *container.HealthSummary — NIL on containers with no healthcheck
container.Summary.Health.Status               // container.HealthStatus (string type)
container.Summary.NetworkSettings             // *container.NetworkSettingsSummary — CAN BE NIL
container.Summary.NetworkSettings.Networks    // map[string]*network.EndpointSettings (values can be nil)
container.Summary.Ports[i].IP                 // netip.Addr, NOT string — use .IsValid() then .String()
container.Summary.Ports[i].Type               // "tcp"/"udp"/"sctp" — the field is Type; there is no Protocol field
container.InspectResponse.State               // *container.State — CAN BE NIL
container.InspectResponse.Config              // *container.Config — CAN BE NIL; holds Env (never map it)
container.InspectResponse.HostConfig          // *container.HostConfig — CAN BE NIL
container.InspectResponse.Created             // string (RFC3339Nano), unlike Summary.Created's int64
container.InspectResponse.SizeRw / SizeRootFs // *int64 — CAN BE NIL
network.Network.Created                       // timeext.Time, NOT time.Time
network.EndpointSettings.Gateway/IPAddress    // netip.Addr, NOT string
image.Summary.Created                         // int64 Unix seconds
image.InspectResponse.Created                 // string, and `json:",omitempty"` — CAN BE EMPTY
image.InspectResponse.Config                  // *dockerspec.DockerOCIImageConfig — holds the image's baked-in Env
volume.Volume.CreatedAt                       // string (RFC3339), already formatted
volume.Volume.UsageData                       // *volume.UsageData — CAN BE NIL
```

---

## Patterns to Mirror

### NAMING_CONVENTION
```go
// SOURCE: internal/httpapi/pair.go:16-43
// Grouped const block, doc comment on the block explaining WHY the values exist,
// then per-constant comments. Message constants are msg<Thing><Outcome>.
const (
	// maxPairBodyBytes bounds the pairing request body. This route has no
	// client certificate to gate it and there is no rate limiting until
	// Phase 6, so an unbounded JSON body is a trivial memory-exhaustion
	// vector.
	maxPairBodyBytes = 8 << 10

	// msgPairFailed is the terse rejection served for every reason a pairing
	// attempt fails: an unknown, expired, or already-used code, or a
	// malformed CSR. The client must never be able to tell which.
	msgPairFailed = "pairing failed"
)
```

### ERROR_HANDLING
```go
// SOURCE: internal/httpapi/pair.go:73-84
// Sentinel check first, then log-with-context + terse client body for everything else.
resp, err := s.pairDevice(r.Context(), req.PairingCode, csrDER)
if err != nil {
	if errors.Is(err, state.ErrPairingCodeInvalid) {
		s.writeError(w, http.StatusUnauthorized, msgPairFailed)
		return
	}
	s.log.Error("pair device", slog.Any("err", err))
	s.writeError(w, http.StatusInternalServerError, msgPairInternalError)
	return
}
s.writeJSON(w, http.StatusCreated, resp)
```

```go
// SOURCE: internal/dockerx/client.go:43-45
// Every error wrapped with %w and enough context to name the failing input.
if err != nil {
	return nil, fmt.Errorf("create docker client for %s: %w", host, err)
}
```

### LOGGING_PATTERN
```go
// SOURCE: internal/httpapi/device.go:68-71
// slog with typed attrs; slog.Any("err", err) for the error; never a formatted string.
s.log.Error("renew device certificate",
	slog.String("device_id", device.ID),
	slog.Any("err", err),
)
```

### SERVICE_PATTERN
```go
// SOURCE: internal/dockerx/client.go:24-28, 66-71
// Struct holds the SDK handle and a logger; methods are on *Client and wrap errors.
type Client struct {
	api *client.Client
	log *slog.Logger
}

func (c *Client) Close() error {
	if err := c.api.Close(); err != nil {
		return fmt.Errorf("close docker client: %w", err)
	}
	return nil
}
```

### HTTP_HANDLER_PATTERN
```go
// SOURCE: internal/httpapi/device.go:46-77
// Handler = resolve inputs, call a private method that returns (dto, error),
// map the error, write. No business logic inside the handler body.
func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	device, ok := DeviceFrom(r.Context())
	if !ok {
		s.writeError(w, http.StatusInternalServerError, msgDeviceInternalError)
		return
	}
	resp, err := s.renewDevice(r.Context(), device.ID, csrDER)
	if err != nil { /* … map … */ }
	s.writeJSON(w, http.StatusOK, resp)
}
```

### ROUTE_REGISTRATION
```go
// SOURCE: internal/httpapi/server.go:74-91
// Method-prefixed patterns (Go 1.22+); guarded routes wrapped in requireDevice;
// no catch-all "/" so unmatched paths get ServeMux's bare 404.
mux.HandleFunc("GET /v1/status", s.handleStatus)
mux.Handle("POST /v1/device/renew", s.requireDevice(http.HandlerFunc(s.handleRenew)))
return s.withRecovery(s.withRequestLog(mux))
```

### MIDDLEWARE_PATTERN
```go
// SOURCE: internal/httpapi/middleware.go:41-47
func (s *Server) requireDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			s.writeError(w, http.StatusUnauthorized, msgClientCertRequired)
			return
		}
		// …
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

### TEST_STRUCTURE
```go
// SOURCE: internal/dockerx/client_test.go:18-48
// t.Parallel at both levels, table of named cases, explicit // Arrange // Act // Assert,
// failure messages in the form: got = X, want Y.
func TestNewUnreachableEngine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
	}{
		{name: "missing unix socket", host: "unix:///nonexistent/docker.sock"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Act
			c, err := New(context.Background(), tt.host, testLogger())

			// Assert
			if err == nil {
				_ = c.Close()
				t.Fatalf("New(%q) error = nil, want an unreachable-engine failure", tt.host)
			}
		})
	}
}
```

### FIELD_ALLOWLIST_GUARD
```go
// SOURCE: internal/httpapi/status_test.go:74-105
// Asserting the EXACT key count, not just presence, so a later phase cannot
// widen a payload silently. This phase reuses the pattern for every read DTO.
if len(body) != 5 {
	t.Fatalf("status payload has %d keys (%v), want exactly 5", len(body), keysOf(body))
}
```

---

## API Contract (after this phase)

| Route | Auth | Policy op | Success | Body |
|---|---|---|---|---|
| `GET /v1/containers?all=<bool>` | client cert | `read` | 200 | `{"items":[ContainerSummary],"truncated":bool}` |
| `GET /v1/containers/{id}` | client cert | `read` | 200 | `ContainerDetail` |
| `GET /v1/images` | client cert | `read` | 200 | `{"items":[ImageSummary],"truncated":bool}` |
| `GET /v1/images/{id}` | client cert | `read` | 200 | `ImageDetail` |
| `GET /v1/networks` | client cert | `read` | 200 | `{"items":[NetworkSummary],"truncated":bool}` |
| `GET /v1/networks/{id}` | client cert | `read` | 200 | `NetworkDetail` |
| `GET /v1/volumes` | client cert | `read` | 200 | `{"items":[VolumeSummary],"truncated":bool}` |
| `GET /v1/volumes/{name}` | client cert | `read` | 200 | `VolumeDetail` |

Shared failure modes on all eight:

| Condition | Status | Body |
|---|---|---|
| No / unknown / revoked client certificate | 401 | `{"error":"client certificate required"}` |
| Policy mode forbids the operation | 403 | `{"error":"operation not permitted by host policy"}` |
| Reference fails D6 validation | 400 | `{"error":"invalid object reference"}` |
| Object does not exist | 404 | `{"error":"not found"}` |
| Engine unreachable, timed out, or any other Engine error | 502 | `{"error":"docker engine unavailable"}` |
| Wrong method on a known path | 405 | ServeMux default |

### DTO field allowlists

These structs are the complete, exhaustive set. Adding a field is a deliberate act that must also
update the corresponding `*FieldCount` test in Task 9.

```go
// internal/dockerx/types.go

type ListResult[T any] struct {          // Go generics; the module is on Go 1.26
	Items     []T  `json:"items"`
	Truncated bool `json:"truncated"`
}

type Port struct {
	IP          string `json:"ip,omitempty"`      // from netip.Addr; "" when !IsValid()
	PrivatePort uint16 `json:"private_port"`
	PublicPort  uint16 `json:"public_port,omitempty"`
	Protocol    string `json:"protocol"`          // from PortSummary.Type
}

type Mount struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadWrite   bool   `json:"read_write"`
}

type EndpointSummary struct {
	NetworkName string   `json:"network_name"`
	NetworkID   string   `json:"network_id"`
	IPAddress   string   `json:"ip_address,omitempty"`
	Gateway     string   `json:"gateway,omitempty"`
	MACAddress  string   `json:"mac_address,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

type ContainerSummary struct {            // 11 fields
	ID        string            `json:"id"`
	Names     []string          `json:"names"`
	Image     string            `json:"image"`
	ImageID   string            `json:"image_id"`
	Command   string            `json:"command"`
	CreatedAt string            `json:"created_at"`         // RFC3339 UTC
	State     string            `json:"state"`
	Status    string            `json:"status"`
	Health    string            `json:"health,omitempty"`   // "" when Health is nil
	Labels    map[string]string `json:"labels"`
	Ports     []Port            `json:"ports"`
}

type ContainerDetail struct {             // 24 fields. NO env. NO raw.
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	CreatedAt     string            `json:"created_at"`      // RFC3339 UTC
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
	Command       string            `json:"command"`         // InspectResponse.Path
	Args          []string          `json:"args"`
	Entrypoint    []string          `json:"entrypoint,omitempty"`
	WorkingDir    string            `json:"working_dir,omitempty"`
	User          string            `json:"user,omitempty"`
	Mounts        []Mount           `json:"mounts"`
	Networks      []EndpointSummary `json:"networks"`
	Ports         []Port            `json:"ports"`
}

type ImageSummary struct {                // 8 fields
	ID          string            `json:"id"`
	ParentID    string            `json:"parent_id,omitempty"`
	RepoTags    []string          `json:"repo_tags"`
	RepoDigests []string          `json:"repo_digests"`
	CreatedAt   string            `json:"created_at"`        // RFC3339 UTC from int64
	Size        int64             `json:"size"`
	Containers  int64             `json:"containers"`        // -1 means "not calculated"
	Labels      map[string]string `json:"labels"`
}

type ImageDetail struct {                 // 9 fields
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
// image.InspectResponse.Config carries the image's baked-in Env.
// It is NOT mapped. D2 applies to images too.

type NetworkSummary struct {              // 8 fields
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Scope      string            `json:"scope"`
	CreatedAt  string            `json:"created_at"`
	Internal   bool              `json:"internal"`
	EnableIPv6 bool              `json:"enable_ipv6"`
	Labels     map[string]string `json:"labels"`
}

type NetworkDetail struct {               // NetworkSummary + attached endpoints
	NetworkSummary
	Containers []NetworkEndpoint `json:"containers"`
}

type NetworkEndpoint struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	IPv4Address string `json:"ipv4_address,omitempty"`
	IPv6Address string `json:"ipv6_address,omitempty"`
}

type VolumeSummary struct {               // 7 fields; VolumeDetail = VolumeSummary
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Mountpoint string            `json:"mountpoint"`
	CreatedAt  string            `json:"created_at,omitempty"`
	Scope      string            `json:"scope"`
	Labels     map[string]string `json:"labels"`
	SizeBytes  *int64            `json:"size_bytes,omitempty"` // from UsageData.Size when non-nil
}
```

`volume.Volume.Options` is **not** mapped: for `tmpfs` and CIFS/NFS volumes it routinely contains
credentials (`o=username=…,password=…`). D2's reasoning applies verbatim.

---

## Files to Change

| File | Action | Justification |
|---|---|---|
| `internal/dockerx/types.go` | CREATE | The DTO allowlist (D1) plus the generic `ListResult` envelope |
| `internal/dockerx/types_test.go` | CREATE | JSON tag / field-count guards for every DTO |
| `internal/dockerx/errors.go` | CREATE | `ErrNotFound`, `ErrInvalidRef`, and `classify(err)` wrapping `cerrdefs.IsNotFound` |
| `internal/dockerx/errors_test.go` | CREATE | `classify` maps not-found, passes everything else through |
| `internal/dockerx/ref.go` | CREATE | `ValidateRef` (D6) — the boundary check |
| `internal/dockerx/ref_test.go` | CREATE | Traversal, empty, over-length, and valid-form cases |
| `internal/dockerx/containers.go` | CREATE | `ListContainers` / `InspectContainer` + mappers |
| `internal/dockerx/containers_test.go` | CREATE | Mapper tests over synthetic values, including all nil-pointer branches |
| `internal/dockerx/images.go` | CREATE | `ListImages` / `InspectImage` + mappers |
| `internal/dockerx/networks.go` | CREATE | `ListNetworks` / `InspectNetwork` + mappers |
| `internal/dockerx/volumes.go` | CREATE | `ListVolumes` / `InspectVolume` + mappers |
| `internal/dockerx/objects_test.go` | CREATE | Mapper tests for images, networks, volumes |
| `internal/httpapi/reads.go` | CREATE | The eight handlers plus `DockerReader` and its four sub-interfaces |
| `internal/httpapi/reads_test.go` | CREATE | Handler tests against a fake `DockerReader` |
| `internal/httpapi/policygate.go` | CREATE | `requireOp` middleware (D10) |
| `internal/httpapi/policygate_test.go` | CREATE | Allowed / denied per mode |
| `internal/httpapi/server.go` | UPDATE | `Server.dc` field, `NewServer` gains a `DockerReader`, eight routes registered |
| `internal/httpapi/server_test.go` | UPDATE | `runnableServer` passes the new argument |
| `internal/httpapi/status_test.go` | UPDATE | Three helpers pass the new argument; one probe path changes |
| `cmd/devmon-agent/main.go` | UPDATE | Pass `dc` into `NewServer` |
| `go.mod` / `go.sum` | UPDATE | Promote `github.com/containerd/errdefs` from indirect to direct |
| `README.md` | UPDATE | API table gains eight rows; a short section on why responses are projections |

## NOT Building

- **Container resource stats (CPU/memory)** — PRD "Could", not "Must". A separate Engine endpoint with a streaming shape of its own.
- **Container logs** — Phase 4, running in parallel with this one.
- **Any lifecycle operation** (start/restart/stop/kill/delete) — Phase 5.
- **Audit-logging read operations** — the PRD scopes the audit log to *mutating* calls. Reads are recorded by `withRequestLog` at Debug and nowhere else.
- **The agent's `protected: true` marker in listings** — D13; belongs with Phase 5's self-exclusion enforcement.
- **Server-side filtering or search** (`?name=`, `?label=`) — the client can filter 500 items locally. Passing filter strings through to `client.Filters` widens the enumerated surface with no PRD requirement behind it.
- **Pagination cursors** — D8's hard cap plus `truncated` is the MVP answer. Cursors are a contract commitment to revisit only if a real host exceeds the cap.
- **Prune, disk-usage, or system-info endpoints** — not in the enumerated operation set.
- **Rate limiting on the new routes** — Phase 6, and these are behind `requireDevice`.
- **Caching Engine responses** — an operator watching a container come back up needs the current state; a cache turns "restarted successfully" into a lie.

---

## Step-by-Step Tasks

### Task 1: DTO types and the list envelope
- **ACTION**: Create `internal/dockerx/types.go`.
- **IMPLEMENT**: Every struct from the "DTO field allowlists" section above, verbatim, with `ListResult[T any]`. Doc comment on the block stating D1 and D2: these types are an allowlist; `Config.Env` and `volume.Options` have no field here and must never gain one.
- **MIRROR**: NAMING_CONVENTION — the doc comment explains *why* the allowlist exists, not what the fields are.
- **IMPORTS**: none beyond stdlib (this file is pure type declarations).
- **GOTCHA**: JSON tags are snake_case, matching the existing `statusResponse` / `pairResponse` convention (`ca_fingerprint`, `certificate_pem`), **not** Docker's PascalCase.
- **VALIDATE**: `go build ./internal/dockerx/`; `gofmt -l internal/dockerx` prints nothing.

### Task 2: Error classification
- **ACTION**: Create `internal/dockerx/errors.go` and `errors_test.go`.
- **IMPLEMENT**:
  ```go
  // ErrNotFound is returned when the Engine reports no such object. It is the
  // only Engine failure the API distinguishes: the caller is an authenticated
  // device, so telling it "that container is gone" is information it is
  // entitled to, while every other Engine fault is an upstream problem the
  // client can only retry.
  var ErrNotFound = errors.New("docker object not found")

  // ErrInvalidRef is returned by ValidateRef. It never reaches the Engine.
  var ErrInvalidRef = errors.New("invalid docker object reference")

  func classify(op string, err error) error {
      if err == nil {
          return nil
      }
      if cerrdefs.IsNotFound(err) {
          return fmt.Errorf("%s: %w", op, ErrNotFound)
      }
      return fmt.Errorf("%s: %w", op, err)
  }
  ```
- **MIRROR**: ERROR_HANDLING (`fmt.Errorf` with `%w`); sentinel style from `state.ErrDeviceNotFound` / `state.ErrPairingCodeInvalid`.
- **IMPORTS**: `errors`, `fmt`, `cerrdefs "github.com/containerd/errdefs"`.
- **GOTCHA**: `errdefs` is currently an **indirect** requirement. Run `go get github.com/containerd/errdefs@v1.0.0`, then `go mod tidy`. Skipping this leaves a build that works locally and fails in a clean module cache.
- **VALIDATE**: `go test ./internal/dockerx/ -race -run TestClassify`; `go mod tidy` then confirm errdefs sits in the direct require block.

### Task 3: Object reference validation
- **ACTION**: Create `internal/dockerx/ref.go` and `ref_test.go`.
- **IMPLEMENT**: `ValidateRef(ref string) error` returning `ErrInvalidRef` unless `ref` matches `^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`. Compile the regexp once at package level with `regexp.MustCompile`.
- **MIRROR**: `config.isValidDNSName` (`internal/config/config.go:308-336`) — boundary validation as a small pure function whose doc comment names the attack it prevents.
- **IMPORTS**: `regexp`.
- **GOTCHA**: The pattern must reject a leading `-` and a leading `.`; both are excluded by the first character class. `..` therefore fails on its first character. Do **not** use `strings.Contains(ref, "..")` as the check — it misses `/` entirely and gives a false sense of coverage.
- **GOTCHA**: `sha256:abc…` must **fail**. Digest references are not accepted on these routes; the test asserts this explicitly so nobody "fixes" it later by widening the pattern to include `:`.
- **VALIDATE**: table test over `""`, `".."`, `"../../info"`, `"a/b"`, `"-abc"`, a 129-char string, `"sha256:ab"` (all → `ErrInvalidRef`) and `"a1b2c3d4e5f6"`, `"my_app-1.2"` (→ nil).

### Task 4: Container reads
- **ACTION**: Create `internal/dockerx/containers.go` and `containers_test.go`.
- **IMPLEMENT**:
  ```go
  // callTimeout bounds every Engine call. The HTTP server's WriteTimeout is 30s;
  // a wedged Engine must become a 502 the agent controls rather than a response
  // that dies mid-body on the client.
  const callTimeout = 15 * time.Second

  // maxListItems caps every list response. A host with thousands of images would
  // otherwise produce a body no phone should receive. The cap is server-side and
  // not client-adjustable, consistent with the agent's configuration boundary.
  const maxListItems = 500

  func (c *Client) ListContainers(ctx context.Context, all bool) (ListResult[ContainerSummary], error)
  func (c *Client) InspectContainer(ctx context.Context, ref string) (ContainerDetail, error)

  func toContainerSummary(s container.Summary) ContainerSummary
  func toContainerDetail(r container.InspectResponse) ContainerDetail
  ```
  `ListContainers` derives a timeout context, calls `ContainerList(ctx, client.ContainerListOptions{All: all})`, `classify`s the error, then maps and truncates. `InspectContainer` calls `ValidateRef` **first**, then `ContainerInspect(ctx, ref, client.ContainerInspectOptions{})` — `Size: false`, because size calculation walks the filesystem.
- **MIRROR**: SERVICE_PATTERN; ERROR_HANDLING.
- **IMPORTS**: `context`, `time`, `github.com/moby/moby/client`, `github.com/moby/moby/api/types/container`.
- **GOTCHA**: Every nil-pointer field listed in "Verified field types" must be guarded. `Health`, `NetworkSettings`, `State`, `Config`, and `HostConfig` are all nil in ordinary cases — a container with no healthcheck, a `--network=none` container, an image with no config. A nil deref here is a panic caught by `withRecovery` and served as 500: a bug that only appears on the operator's real host, never in a test written against a fully-populated fixture. **Write the fixtures with the pointers nil.**
- **GOTCHA**: `container.Summary.Created` is Unix **seconds** (`time.Unix(n, 0).UTC().Format(time.RFC3339)`), while `InspectResponse.Created` is already an RFC3339Nano **string**. Two different conversions in two adjacent mappers.
- **GOTCHA**: Initialise every slice with `make([]T, 0, n)`, never a nil slice — a nil slice marshals to `null`, and the app would have to handle both `null` and `[]`.
- **GOTCHA**: Truncate after mapping: `if len(items) > maxListItems { items, truncated = items[:maxListItems], true }`, so `Truncated` reflects the Engine's real count.
- **VALIDATE**: `go test ./internal/dockerx/ -race -run 'TestContainer'`; assert a zero-valued `container.Summary` maps without panicking and yields `health == ""`.

### Task 5: Image, network, and volume reads
- **ACTION**: Create `internal/dockerx/images.go`, `networks.go`, `volumes.go`, and `objects_test.go`.
- **IMPLEMENT**: `ListImages`/`InspectImage`, `ListNetworks`/`InspectNetwork`, `ListVolumes`/`InspectVolume`, each following Task 4's shape exactly (timeout, classify, validate-ref-first, map, truncate).
- **MIRROR**: Task 4's file, function for function.
- **IMPORTS**: add `github.com/moby/moby/api/types/image`, `.../network`, `.../volume`.
- **GOTCHA**: `ImageInspect` is **variadic** (`inspectOpts ...ImageInspectOption`), not an options struct — call it as `c.api.ImageInspect(ctx, ref)`. Writing `client.ImageInspectOptions{}` will not compile; that type does not exist.
- **GOTCHA**: `ImageInspectResult` **embeds** `image.InspectResponse`; there is no `.Image` field. Access as `res.ID`, `res.RepoTags`, and so on.
- **GOTCHA**: `network.Network.Created` is `timeext.Time`, not `time.Time`. Verify the conversion compiles before writing the mapper — try `time.Time(n.Created)`, and fall back to its `.String()` method if the direct conversion is rejected.
- **GOTCHA**: `network.Summary` embeds `network.Network`, so one mapper written against `network.Network` serves both list and inspect. `network.Inspect` embeds it too and adds `Containers map[string]EndpointResource`.
- **GOTCHA**: `VolumeListResult` carries `Warnings []string`. Log them at Warn; they do **not** go in the response — they can name driver-internal host paths.
- **VALIDATE**: `go test ./internal/dockerx/ -race`; package coverage ≥ 80%.

### Task 6: The `requireOp` policy gate
- **ACTION**: Create `internal/httpapi/policygate.go` and `policygate_test.go`.
- **IMPLEMENT**:
  ```go
  // msgPolicyForbidden is served when the host's startup policy does not permit
  // the operation. Unlike the terse authentication rejections, this one is
  // deliberately specific: the caller is an authenticated device, and the whole
  // point of advertising the policy mode is that a client can tell "the host
  // forbids this" apart from "the agent is broken".
  const msgPolicyForbidden = "operation not permitted by host policy"

  func (s *Server) requireOp(op policy.Operation, next http.Handler) http.Handler
  ```
  Reject with 403 when `!s.cfg.PolicyMode.Allows(op)`, logging the device ID (best-effort via `DeviceFrom`) and the operation at Warn.
- **MIRROR**: MIDDLEWARE_PATTERN.
- **IMPORTS**: `net/http`, `log/slog`, `github.com/scnplt/devmon-agent/internal/policy`.
- **GOTCHA**: Order matters — `requireDevice(s.requireOp(op, handler))`, so the policy check runs **after** authentication. Inverting it would let an unauthenticated scanner probe the host's policy tier by observing 403 vs 401.
- **GOTCHA**: `policy.OpRead` is permitted by all three modes, so this task changes no observable behaviour today. That is intended (D10). Do not "simplify" it away.
- **VALIDATE**: table test over `ModeReadOnly`/`ModeDefault`/`ModeFull` × `OpRead`/`OpDelete`, asserting 200 vs 403 and the exact `msgPolicyForbidden` body.

### Task 7: The `DockerReader` interfaces and the eight handlers
- **ACTION**: Create `internal/httpapi/reads.go`.
- **IMPLEMENT**:
  ```go
  // Four small interfaces, declared here rather than in dockerx, because the
  // consumer owns the contract (repo rule: accept interfaces, return structs).
  // Splitting by object type keeps each within the 1-3 method guideline and
  // lets Phase 4 and Phase 5 add their own without widening this one.
  type ContainerReader interface {
      ListContainers(ctx context.Context, all bool) (dockerx.ListResult[dockerx.ContainerSummary], error)
      InspectContainer(ctx context.Context, ref string) (dockerx.ContainerDetail, error)
  }
  type ImageReader interface   { /* ListImages, InspectImage */ }
  type NetworkReader interface { /* ListNetworks, InspectNetwork */ }
  type VolumeReader interface  { /* ListVolumes, InspectVolume */ }

  type DockerReader interface {
      ContainerReader
      ImageReader
      NetworkReader
      VolumeReader
  }

  // Compile-time proof the concrete client still satisfies the contract.
  var _ DockerReader = (*dockerx.Client)(nil)
  ```
  Then eight handlers, plus one shared error mapper so the mapping cannot drift:
  ```go
  // writeDockerError maps a dockerx failure onto a status code. ErrInvalidRef is
  // a client mistake (400), ErrNotFound is an answer (404), and everything else
  // is the Engine failing upstream of us (502) — never 500, which must keep
  // meaning "the agent itself broke".
  func (s *Server) writeDockerError(w http.ResponseWriter, op string, err error) {
      switch {
      case errors.Is(err, dockerx.ErrInvalidRef):
          s.writeError(w, http.StatusBadRequest, msgInvalidRef)
      case errors.Is(err, dockerx.ErrNotFound):
          s.writeError(w, http.StatusNotFound, msgNotFound)
      default:
          s.log.Error(op, slog.Any("err", err))
          s.writeError(w, http.StatusBadGateway, msgEngineUnavailable)
      }
  }
  ```
- **MIRROR**: HTTP_HANDLER_PATTERN; ERROR_HANDLING; NAMING_CONVENTION for the `msg*` constants.
- **IMPORTS**: `context`, `errors`, `log/slog`, `net/http`, `strconv`, `github.com/scnplt/devmon-agent/internal/dockerx`.
- **GOTCHA**: Parse `?all=` with `strconv.ParseBool`; on a parse error default to `false` rather than 400. `?all=` with no value and `?all=yes` are "the app sent something odd", not an attack, and failing the request would strand a client over a query-string typo.
- **GOTCHA**: The reference comes from `r.PathValue("id")` (`"name"` for volumes) — never from a query parameter or a body.
- **GOTCHA**: Every handler must tolerate `s.dc == nil`. The existing test helpers construct servers with nil dependencies deliberately, and the routes are registered unconditionally. Guard once in the handler prologue and answer 502; do not make `routes()` conditional.
- **VALIDATE**: `go build ./...`.

### Task 8: Wire the routes and the dependency
- **ACTION**: Update `internal/httpapi/server.go`, `cmd/devmon-agent/main.go`, `internal/httpapi/server_test.go`, `internal/httpapi/status_test.go`.
- **IMPLEMENT**:
  - `Server` gains `dc DockerReader`.
  - `NewServer(cfg config.Config, st *state.Store, ca *certs.CA, dc DockerReader, tlsCfg *tls.Config, log *slog.Logger)` — `dc` inserted after `ca`, before `tlsCfg`, so the dependency order still reads state → identity → docker → transport → logging. Extend the existing doc comment to say `dc` may be nil in tests, mirroring what it already says about `ca`.
  - In `routes()`, after the existing device routes:
    ```go
    // Read operations. Every one is guarded twice: requireDevice proves who is
    // calling, requireOp proves the host's startup policy permits it.
    read := func(pattern string, h http.HandlerFunc) {
        mux.Handle(pattern, s.requireDevice(s.requireOp(policy.OpRead, h)))
    }
    read("GET /v1/containers", s.handleListContainers)
    read("GET /v1/containers/{id}", s.handleInspectContainer)
    read("GET /v1/images", s.handleListImages)
    read("GET /v1/images/{id}", s.handleInspectImage)
    read("GET /v1/networks", s.handleListNetworks)
    read("GET /v1/networks/{id}", s.handleInspectNetwork)
    read("GET /v1/volumes", s.handleListVolumes)
    read("GET /v1/volumes/{name}", s.handleInspectVolume)
    ```
  - `main.go:164` becomes `httpapi.NewServer(cfg, st, ca, dc, tlsCfg, log)`. `dc` already exists at line 155.
  - Test helpers gain a `nil` in the new position: `runnableServer` (`server_test.go:31`), `testServer` (`status_test.go:32`), `testServerWithCA` (`status_test.go:46`), `testServerWithStore` (`status_test.go:62`).
- **MIRROR**: ROUTE_REGISTRATION.
- **GOTCHA**: `status_test.go:250` — `TestUnknownPathLeaksNothing` requests `GET /v1/containers` and asserts **404**. That route now exists. Change the probe path to something still unrouted (`/v1/system/info`) rather than deleting the test; it guards a real property.
- **GOTCHA**: The `var _ DockerReader = (*dockerx.Client)(nil)` assertion from Task 7 is what turns a signature mismatch into an error in the package that owns the contract instead of a confusing one at the `main.go` call site.
- **VALIDATE**: `go build ./... && go test ./internal/... -race` — all pre-existing tests still pass.

### Task 9: Handler and DTO tests
- **ACTION**: Create `internal/httpapi/reads_test.go` and `internal/dockerx/types_test.go`.
- **IMPLEMENT**:
  - A `fakeDocker` implementing `DockerReader`, with per-method function fields so each test injects its own behaviour (including returning `dockerx.ErrNotFound` and a generic error).
  - A `testServerWithDocker(t, mode, dc)` helper, **additive** alongside the three existing helpers — do not widen `testServer`, which several passing tests depend on.
  - Per-route tests: 200 with the expected body, 404 on `ErrNotFound`, 502 on a generic error, 400 on `ErrInvalidRef`, 401 with no client certificate, 405 on `POST`.
  - `Test<DTO>FieldCount` for each of the eight DTOs, in the exact shape of `TestStatusFieldCount`.
  - `TestContainerDetailNeverCarriesEnv`: map an `InspectResponse` whose `Config.Env` holds `"DB_PASSWORD=hunter2"`, marshal the DTO, and assert the JSON contains neither `"env"` nor `"hunter2"`. Same for `ImageDetail`, and for `volume.Volume.Options` → `VolumeSummary`.
- **MIRROR**: TEST_STRUCTURE; FIELD_ALLOWLIST_GUARD.
- **GOTCHA**: Requests to guarded routes need `req.TLS` set and a matching device in a real store — reuse `testServerWithStore` + `peerCertWithSerial` (`status_test.go:53-72`) rather than inventing a second mechanism.
- **GOTCHA**: The secret-leak tests are the highest-value tests in this phase. They must assert on the **marshalled JSON string**, not on struct fields — a future embedded struct or a removed `json:"-"` would slip past a field-level assertion.
- **VALIDATE**: `make cover` — `./internal/...` total ≥ 80%.

### Task 10: Docs and the manual sweep
- **ACTION**: Update `README.md`.
- **IMPLEMENT**: Eight rows in the API table (`README.md:194-199`). Below it, a short subsection — *Why responses are projections* — stating D1/D2/D3: responses are an explicit allowlist; environment variables and volume driver options are never returned at any level; command lines are returned deliberately and noted in the threat model; lists are capped at 500 with a `truncated` flag. Add the 400/404/502 rows to a failure-mode table.
- **MIRROR**: The existing README voice — each statement carries its reason, as in the `/v1/status` paragraph at `README.md:201-206`.
- **GOTCHA**: English only, per CLAUDE.md.
- **VALIDATE**: The full gate sweep plus the manual checklist below.

---

## Testing Strategy

### Unit Tests

| Test | Input | Expected Output | Edge Case? |
|---|---|---|---|
| `TestValidateRef` | `""`, `".."`, `"../../info"`, `"a/b"`, `"-x"`, 129 chars, `"sha256:ab"` | `ErrInvalidRef` for each | yes |
| `TestValidateRef` | `"a1b2c3"`, `"my_app-1.2"` | nil | no |
| `TestClassify` | an error satisfying `cerrdefs.IsNotFound` | `errors.Is(got, ErrNotFound)` true | no |
| `TestClassify` | a plain `errors.New` | not `ErrNotFound`; original wrapped | no |
| `TestToContainerSummaryZeroValue` | `container.Summary{}` (all pointers nil) | no panic; `health == ""`; `ports` empty | **yes** |
| `TestToContainerDetailNilState` | `InspectResponse{State: nil, Config: nil, HostConfig: nil}` | no panic; zero-valued fields | **yes** |
| `TestToContainerSummaryCreatedAt` | `Created: 1700000000` | `"2023-11-14T22:13:20Z"` | no |
| `TestPortIPInvalidAddr` | `PortSummary{IP: netip.Addr{}}` | `ip` omitted from JSON | yes |
| `TestListTruncation` | 501 summaries | 500 items, `truncated == true` | **yes** |
| `TestListTruncation` | 500 summaries | 500 items, `truncated == false` | boundary |
| `TestEmptyListMarshalsAsArray` | 0 summaries | `"items":[]`, never `"items":null` | yes |
| `TestVolumeWarningsNotInResponse` | `VolumeListResult{Warnings: ["/var/lib/…"]}` | response body contains no warning text | yes |
| `TestContainerDetailNeverCarriesEnv` | `Config.Env: ["DB_PASSWORD=hunter2"]` | marshalled JSON has no `env`, no `hunter2` | **yes** |
| `TestImageDetailNeverCarriesEnv` | image config with env | same | **yes** |
| `TestVolumeSummaryNeverCarriesOptions` | `Options: {"o":"password=x"}` | no `options`, no `password=x` | **yes** |
| `Test<DTO>FieldCount` × 8 | marshalled DTO | exact expected key count | guard |
| `TestReadRouteRequiresDevice` × 8 | no `req.TLS` | 401, `msgClientCertRequired` | yes |
| `TestReadRouteRejectsOtherMethods` | `POST /v1/containers` | 405 | yes |
| `TestInspectNotFound` | fake returns `ErrNotFound` | 404, `{"error":"not found"}` | yes |
| `TestInspectEngineFailure` | fake returns a generic error | 502, `msgEngineUnavailable`; error logged, not returned | yes |
| `TestInspectInvalidRef` | fake returns `ErrInvalidRef` | 400 | yes |
| `TestListAllParameter` | `?all=true`, `?all=false`, `?all=`, absent | fake sees `true,false,false,false` | yes |
| `TestNilDockerReader` | `s.dc == nil` | 502, no panic | **yes** |
| `TestRequireOp` | 3 modes × `OpRead`/`OpDelete` | 200 / 403 | no |
| `TestErrorBodiesLeakNothing` | every failure path | body contains no `StateDir`, `docker.sock`, `devmon.db` | **yes** |

### Edge Cases Checklist
- [ ] Empty host — no containers, images, networks, or volumes → `{"items":[],"truncated":false}`
- [ ] Container with no healthcheck (`Health` nil)
- [ ] Container on no network (`NetworkSettings` nil)
- [ ] Container never started (`State.StartedAt` zero-valued)
- [ ] Image with no tags (`RepoTags` nil → `[]`)
- [ ] Volume with no `UsageData` → `size_bytes` omitted
- [ ] 501 objects → truncation at exactly 500
- [ ] Reference containing a traversal attempt → 400 before any Engine call
- [ ] Engine socket removed while the agent runs → 502, agent stays up
- [ ] Engine hangs → `callTimeout` fires at 15s → 502, not a client-visible timeout
- [ ] Revoked device → 401 on every read route
- [ ] `read-only` policy mode → all eight routes still 200

---

## Validation Commands

### Static Analysis
```bash
gofmt -l .
go vet ./...
```
EXPECT: both print nothing.

### Lint
```bash
make lint
```
EXPECT: clean (or the "golangci-lint not installed" notice, with `gofmt`/`vet` clean).

### Security Scan
```bash
gosec ./...
```
EXPECT: no findings. Watch for G104 (unhandled errors) in the new mappers.

### Unit Tests
```bash
go test ./internal/dockerx/ ./internal/httpapi/ -race -v
```
EXPECT: all pass.

### Full Test Suite with Coverage
```bash
make cover
```
EXPECT: all packages pass; the total line is ≥ 80%.

### Build Verification
```bash
make build
CGO_ENABLED=0 go build ./...
make image
```
EXPECT: static binary; image builds.

### Module Hygiene
```bash
go mod tidy
git diff go.mod
```
EXPECT: `github.com/containerd/errdefs` appears in the direct require block and nowhere in the indirect block.

### End-to-End Validation (real host with real workloads)
```bash
# With a paired device's cert/key from the Phase 2 README flow:
curl --cacert ca.pem --cert device.pem --key device.key \
     "https://$HOST:8443/v1/containers?all=true" | jq '.items | length, .truncated'

curl --cacert ca.pem --cert device.pem --key device.key \
     "https://$HOST:8443/v1/containers/$(docker ps -q | head -1)" | jq 'keys'
# Assert: the key list contains no "env".

curl -o /dev/null -w '%{http_code}\n' --cacert ca.pem --cert device.pem --key device.key \
     "https://$HOST:8443/v1/containers/nonexistent"          # 404
curl -o /dev/null -w '%{http_code}\n' --cacert ca.pem --cert device.pem --key device.key \
     "https://$HOST:8443/v1/volumes/%2e%2e"                   # 400
curl -o /dev/null -w '%{http_code}\n' --cacert ca.pem \
     "https://$HOST:8443/v1/containers"                       # 401
```

### Manual Validation
- [ ] Start a container with `-e DB_PASSWORD=hunter2`; inspect it through the API and confirm the string `hunter2` appears nowhere in the response.
- [ ] Inspect an image built with `ENV API_KEY=…`; confirm the same.
- [ ] Create a `tmpfs` volume with driver options; confirm `options` is absent from the response.
- [ ] Stop a container; confirm it is absent from `/v1/containers` and present in `/v1/containers?all=true` with `state: "exited"`.
- [ ] Run a container with a failing healthcheck; confirm `health: "unhealthy"` in the list response.
- [ ] `docker network create` and `docker volume create`; confirm both appear immediately (no caching).
- [ ] Stop the Docker daemon; confirm every read route answers 502 and the agent container stays up.
- [ ] Restart the daemon; confirm reads recover without restarting the agent.
- [ ] Start the agent with `DEVMON_POLICY_MODE=read-only`; confirm all eight routes still answer 200.
- [ ] Revoke the device from the host; confirm the next read is 401 with no agent restart.
- [ ] Check `agent.log`: request lines carry method/path/status/duration only — no container names, no reference values.

---

## Acceptance Criteria
- [ ] All 10 tasks complete
- [ ] Eight routes registered, each behind `requireDevice` **and** `requireOp(policy.OpRead)`
- [ ] No response DTO carries environment variables or volume driver options, at any nesting depth, proven by tests asserting on marshalled JSON
- [ ] `ErrNotFound` → 404, `ErrInvalidRef` → 400, every other Engine failure → 502; 500 is never returned by a read route
- [ ] Lists are capped at 500 with an honest `truncated` flag, and an empty list marshals as `[]`
- [ ] `gofmt`, `go vet`, `golangci-lint`, `gosec` all clean
- [ ] `./internal/...` coverage ≥ 80%
- [ ] A paired client retrieves accurate data for all four object types on a host with real workloads (PRD Phase 3 success signal)

## Completion Checklist
- [ ] Code follows the patterns captured above
- [ ] Errors wrapped with `%w` and named context
- [ ] Logging uses `slog` typed attrs; failures logged with context, returned terse
- [ ] Tests are table-driven, `t.Parallel`, AAA-commented
- [ ] No hardcoded values — `callTimeout`,cle `maxListItems`, and every `msg*` are named constants
- [ ] README API table and projection rationale updated
- [ ] `go.mod` tidy, errdefs promoted to direct
- [ ] PRD Phase 3 row moved to `complete` with links to this plan and its report
- [ ] No scope beyond the eight read routes

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| A response leaks environment variables — DB passwords, API keys — to the phone | M | **CRITICAL** | D1/D2: explicit allowlist DTOs, no raw passthrough, no `Raw json.RawMessage`; dedicated marshalled-JSON tests; `*FieldCount` guards so a later phase cannot widen silently |
| Volume driver options leak NFS/CIFS credentials | M | HIGH | `Options` has no DTO field; explicit test |
| Nil-pointer panic on a real host that no fixture reproduced | **H** | HIGH | Every pointer field enumerated in this plan; fixtures written with nils; `withRecovery` is the backstop, not the plan |
| Object reference reaches the Engine URL unvalidated | L | HIGH | D6 boundary check before any SDK call, plus ServeMux's own segment rule; tests assert traversal forms are rejected |
| Code written from the pre-v29 Docker SDK does not compile | **H** | MEDIUM | Verified signatures transcribed above; `ImageInspect`'s variadic shape and the embedded `ImageInspectResult` are the two that differ most from memory |
| `errdefs` left indirect; build fails in a clean module cache | M | MEDIUM | Task 2 promotes it; `go mod tidy` in the gate |
| `NewServer` signature change breaks four existing test helpers | **H** | LOW | Enumerated by file and line in Task 8 |
| `TestUnknownPathLeaksNothing` starts failing because `/v1/containers` now exists | **H** | LOW | Task 8 calls it out with the replacement probe path |
| A large host produces an unusable response on a mobile connection | M | MEDIUM | D8's 500-item cap plus `truncated` |
| Read routes get audit entries and drown the audit log | L | MEDIUM | Explicitly out of scope; the PRD scopes audit to mutating calls |

## Notes

- **This phase is parallel with Phase 4.** Both extend `internal/dockerx` and `internal/httpapi`. If they are implemented concurrently, the collision points are `routes()`, the `NewServer` signature, and the `DockerReader` interface set. Phase 4 should compose its own `LogReader` interface rather than adding methods to `DockerReader`, and both should take the `NewServer` signature change from whichever lands first.
- **`ContainerDetail` is the one DTO worth arguing about.** It is larger than the others because it is the screen an operator actually stares at during an incident. Every field earns its place by answering "why is this container not working": state and exit code, restart count and policy, mounts, networks, ports, and the command it was told to run. Anything that does not answer that question was left out.
- **`ListResult[T]` uses generics.** The module is on Go 1.26 and no existing code uses type parameters — this is the first. The alternative, four near-identical envelope structs, is the repetition the DRY rule exists to prevent, and the type parameter costs nothing at runtime.
- **502, not 500, for Engine failures.** A small decision with a real operational payoff: once Phase 6's monitoring exists, "the agent is broken" and "the host's Docker is broken" are different pages, and the status code is the cheapest place to encode the difference.
- Deferred to Phase 5, recorded here so it is not lost: once the agent can identify its own container, the container list should mark it `protected: true`. That field will be an **addition** to `ContainerSummary`, so `TestContainerSummaryFieldCount` will need its count bumped deliberately — which is exactly the intended friction.
