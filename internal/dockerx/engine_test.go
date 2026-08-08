package dockerx

// This file exercises the eight Engine-calling wrapper methods (List/Inspect
// for containers, images, networks, and volumes) against a fake Docker
// Engine, without a live daemon.
//
// Rather than an httptest.Server (a real listening socket), the fake Engine
// is an in-process http.RoundTripper backed by an http.ServeMux: requests
// never leave the process, there is no port to bind, and version-prefixed
// paths ("/v1.55/containers/json") are normalized before routing so each
// route table only needs to name the unprefixed Engine path. client.New is
// still exercised exactly as production code calls it (client.New(opts...)),
// only the transport is swapped via the SDK's own exported client.WithHTTPClient
// option; dockerx.New's Ping/negotiation path is already covered by
// client_test.go and is not re-tested here.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// versionPrefix matches the negotiated API version segment ("/v1.55/...")
// the moby client prepends to every request path.
var versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)

func stripVersion(path string) string {
	return versionPrefix.ReplaceAllString(path, "")
}

// callRecorder counts Engine requests so a test can prove the Engine was, or
// was not, contacted.
type callRecorder struct {
	mu    sync.Mutex
	count int
}

func (r *callRecorder) record() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
}

func (r *callRecorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// newFakeEngine builds a *Client whose Engine calls are served in-process by
// routes, keyed by method-prefixed pattern ("GET /containers/json") exactly
// like internal/httpapi's own route registration. It returns a recorder of
// every request the Engine transport received.
func newFakeEngine(t *testing.T, routes map[string]http.HandlerFunc) (*Client, *callRecorder) {
	t.Helper()

	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}

	rec := &callRecorder{}
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rec.record()

		r2 := req.Clone(req.Context())
		r2.URL.Path = stripVersion(req.URL.Path)
		r2.URL.RawPath = ""

		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r2)
		res := w.Result()
		res.Request = req
		return res, nil
	})

	api, err := client.New(client.WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatalf("client.New() error = %v, want nil", err)
	}

	return &Client{api: api, log: testLogger()}, rec
}

// jsonHandler answers every request with status and body JSON-encoded.
func jsonHandler(status int, body any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// errorHandler answers every request with a bare status code, the shape the
// Engine uses for failures. errhttp.ToNative classifies the sentinel purely
// from the status code, so no Engine-style JSON error body is required.
func errorHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "simulated engine failure", status)
	}
}

// opSpec names one of the eight Engine-calling wrapper methods so the
// classification, generic-failure, and empty-list tests can iterate all
// eight without duplicating the route wiring per method.
type opSpec struct {
	name   string
	method string

	// pathFor returns the version-stripped Engine path the method requests.
	// List operations ignore ref.
	pathFor func(ref string) string

	// callList runs a list operation, reporting the item count, whether the
	// Items slice is non-nil, and Truncated. nil for inspect operations.
	callList func(ctx context.Context, c *Client) (itemsLen int, itemsNonNil, truncated bool, err error)

	// callInspect runs an inspect operation against ref, discarding the
	// mapped result. nil for list operations.
	callInspect func(ctx context.Context, c *Client, ref string) error
}

// route returns the method-prefixed ServeMux pattern for ref, used to wire a
// single-endpoint fake Engine for this spec.
func (s opSpec) route(ref string) string {
	return s.method + " " + s.pathFor(ref)
}

// run exercises the spec end-to-end (list or inspect, whichever this spec
// is) against ref, and returns only the resulting error. Used by the tests
// that don't care about the mapped payload, only the error path.
func (s opSpec) run(ctx context.Context, c *Client, ref string) error {
	if s.callList != nil {
		_, _, _, err := s.callList(ctx, c)
		return err
	}
	return s.callInspect(ctx, c, ref)
}

func opSpecs() []opSpec {
	return []opSpec{
		{
			name:    "ListContainers",
			method:  http.MethodGet,
			pathFor: func(string) string { return "/containers/json" },
			callList: func(ctx context.Context, c *Client) (int, bool, bool, error) {
				res, err := c.ListContainers(ctx, true)
				return len(res.Items), res.Items != nil, res.Truncated, err
			},
		},
		{
			name:    "InspectContainer",
			method:  http.MethodGet,
			pathFor: func(ref string) string { return "/containers/" + ref + "/json" },
			callInspect: func(ctx context.Context, c *Client, ref string) error {
				_, err := c.InspectContainer(ctx, ref)
				return err
			},
		},
		{
			name:    "ListImages",
			method:  http.MethodGet,
			pathFor: func(string) string { return "/images/json" },
			callList: func(ctx context.Context, c *Client) (int, bool, bool, error) {
				res, err := c.ListImages(ctx)
				return len(res.Items), res.Items != nil, res.Truncated, err
			},
		},
		{
			name:    "InspectImage",
			method:  http.MethodGet,
			pathFor: func(ref string) string { return "/images/" + ref + "/json" },
			callInspect: func(ctx context.Context, c *Client, ref string) error {
				_, err := c.InspectImage(ctx, ref)
				return err
			},
		},
		{
			name:    "ListNetworks",
			method:  http.MethodGet,
			pathFor: func(string) string { return "/networks" },
			callList: func(ctx context.Context, c *Client) (int, bool, bool, error) {
				res, err := c.ListNetworks(ctx)
				return len(res.Items), res.Items != nil, res.Truncated, err
			},
		},
		{
			name:    "InspectNetwork",
			method:  http.MethodGet,
			pathFor: func(ref string) string { return "/networks/" + ref },
			callInspect: func(ctx context.Context, c *Client, ref string) error {
				_, err := c.InspectNetwork(ctx, ref)
				return err
			},
		},
		{
			name:    "ListVolumes",
			method:  http.MethodGet,
			pathFor: func(string) string { return "/volumes" },
			callList: func(ctx context.Context, c *Client) (int, bool, bool, error) {
				res, err := c.ListVolumes(ctx)
				return len(res.Items), res.Items != nil, res.Truncated, err
			},
		},
		{
			name:    "InspectVolume",
			method:  http.MethodGet,
			pathFor: func(ref string) string { return "/volumes/" + ref },
			callInspect: func(ctx context.Context, c *Client, ref string) error {
				_, err := c.InspectVolume(ctx, ref)
				return err
			},
		},
	}
}

// TestEngineHappyPath drives each of the eight wrapper methods through a
// fake Engine returning one populated object, asserting the wrapper maps the
// Engine's response onto the expected DTO.
func TestEngineHappyPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		routes map[string]http.HandlerFunc
		call   func(t *testing.T, c *Client)
	}{
		{
			name: "ListContainers",
			routes: map[string]http.HandlerFunc{
				"GET /containers/json": jsonHandler(http.StatusOK, []container.Summary{
					{ID: "c1", Names: []string{"/api"}, Image: "myapp:1.4", State: "running", Created: 1700000000},
				}),
			},
			call: func(t *testing.T, c *Client) {
				// Act
				got, err := c.ListContainers(context.Background(), true)
				// Assert
				if err != nil {
					t.Fatalf("ListContainers() error = %v, want nil", err)
				}
				if len(got.Items) != 1 {
					t.Fatalf("len(Items) = %d, want 1", len(got.Items))
				}
				if got.Items[0].ID != "c1" {
					t.Errorf("Items[0].ID = %q, want %q", got.Items[0].ID, "c1")
				}
				if got.Items[0].State != "running" {
					t.Errorf("Items[0].State = %q, want %q", got.Items[0].State, "running")
				}
			},
		},
		{
			name: "InspectContainer",
			routes: map[string]http.HandlerFunc{
				"GET /containers/ref1/json": jsonHandler(http.StatusOK, container.InspectResponse{
					ID:    "ref1",
					Name:  "/api",
					Image: "myapp:1.4",
				}),
			},
			call: func(t *testing.T, c *Client) {
				// Act
				got, err := c.InspectContainer(context.Background(), "ref1")
				// Assert
				if err != nil {
					t.Fatalf("InspectContainer() error = %v, want nil", err)
				}
				if got.ID != "ref1" {
					t.Errorf("ID = %q, want %q", got.ID, "ref1")
				}
				if got.Name != "/api" {
					t.Errorf("Name = %q, want %q", got.Name, "/api")
				}
			},
		},
		{
			name: "ListImages",
			routes: map[string]http.HandlerFunc{
				"GET /images/json": jsonHandler(http.StatusOK, []image.Summary{
					{ID: "img1", RepoTags: []string{"myapp:1.4"}, Created: 1700000000},
				}),
			},
			call: func(t *testing.T, c *Client) {
				// Act
				got, err := c.ListImages(context.Background())
				// Assert
				if err != nil {
					t.Fatalf("ListImages() error = %v, want nil", err)
				}
				if len(got.Items) != 1 {
					t.Fatalf("len(Items) = %d, want 1", len(got.Items))
				}
				if got.Items[0].ID != "img1" {
					t.Errorf("Items[0].ID = %q, want %q", got.Items[0].ID, "img1")
				}
			},
		},
		{
			name: "InspectImage",
			routes: map[string]http.HandlerFunc{
				"GET /images/ref1/json": jsonHandler(http.StatusOK, image.InspectResponse{
					ID:           "ref1",
					Architecture: "amd64",
				}),
			},
			call: func(t *testing.T, c *Client) {
				// Act
				got, err := c.InspectImage(context.Background(), "ref1")
				// Assert
				if err != nil {
					t.Fatalf("InspectImage() error = %v, want nil", err)
				}
				if got.ID != "ref1" {
					t.Errorf("ID = %q, want %q", got.ID, "ref1")
				}
				if got.Architecture != "amd64" {
					t.Errorf("Architecture = %q, want %q", got.Architecture, "amd64")
				}
			},
		},
		{
			name: "ListNetworks",
			routes: map[string]http.HandlerFunc{
				"GET /networks": jsonHandler(http.StatusOK, []network.Summary{
					{Network: network.Network{ID: "net1", Name: "bridge", Driver: "bridge"}},
				}),
			},
			call: func(t *testing.T, c *Client) {
				// Act
				got, err := c.ListNetworks(context.Background())
				// Assert
				if err != nil {
					t.Fatalf("ListNetworks() error = %v, want nil", err)
				}
				if len(got.Items) != 1 {
					t.Fatalf("len(Items) = %d, want 1", len(got.Items))
				}
				if got.Items[0].Name != "bridge" {
					t.Errorf("Items[0].Name = %q, want %q", got.Items[0].Name, "bridge")
				}
			},
		},
		{
			name: "InspectNetwork",
			routes: map[string]http.HandlerFunc{
				"GET /networks/ref1": jsonHandler(http.StatusOK, network.Inspect{
					Network: network.Network{ID: "ref1", Name: "bridge"},
				}),
			},
			call: func(t *testing.T, c *Client) {
				// Act
				got, err := c.InspectNetwork(context.Background(), "ref1")
				// Assert
				if err != nil {
					t.Fatalf("InspectNetwork() error = %v, want nil", err)
				}
				if got.ID != "ref1" {
					t.Errorf("ID = %q, want %q", got.ID, "ref1")
				}
				if got.Name != "bridge" {
					t.Errorf("Name = %q, want %q", got.Name, "bridge")
				}
			},
		},
		{
			name: "ListVolumes",
			routes: map[string]http.HandlerFunc{
				"GET /volumes": jsonHandler(http.StatusOK, volume.ListResponse{
					Volumes: []volume.Volume{{Name: "vol1", Driver: "local"}},
				}),
			},
			call: func(t *testing.T, c *Client) {
				// Act
				got, err := c.ListVolumes(context.Background())
				// Assert
				if err != nil {
					t.Fatalf("ListVolumes() error = %v, want nil", err)
				}
				if len(got.Items) != 1 {
					t.Fatalf("len(Items) = %d, want 1", len(got.Items))
				}
				if got.Items[0].Name != "vol1" {
					t.Errorf("Items[0].Name = %q, want %q", got.Items[0].Name, "vol1")
				}
			},
		},
		{
			name: "InspectVolume",
			routes: map[string]http.HandlerFunc{
				"GET /volumes/ref1": jsonHandler(http.StatusOK, volume.Volume{
					Name:   "ref1",
					Driver: "local",
				}),
			},
			call: func(t *testing.T, c *Client) {
				// Act
				got, err := c.InspectVolume(context.Background(), "ref1")
				// Assert
				if err != nil {
					t.Fatalf("InspectVolume() error = %v, want nil", err)
				}
				if got.Name != "ref1" {
					t.Errorf("Name = %q, want %q", got.Name, "ref1")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, _ := newFakeEngine(t, tt.routes)
			tt.call(t, c)
		})
	}
}

// TestEngineListTruncation drives ListContainers through the real call path
// with 501 and 500 fixture summaries, proving the truncation boundary is
// applied to what the Engine actually returned, not a synthetic slice.
func TestEngineListTruncation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		fixtureCount  int
		wantLen       int
		wantTruncated bool
	}{
		{name: "501 summaries truncates to 500", fixtureCount: 501, wantLen: 500, wantTruncated: true},
		{name: "500 summaries is the untruncated boundary", fixtureCount: 500, wantLen: 500, wantTruncated: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			summaries := make([]container.Summary, tt.fixtureCount)
			for i := range summaries {
				summaries[i] = container.Summary{ID: "c" + string(rune('0'+i%10))}
			}
			c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
				"GET /containers/json": jsonHandler(http.StatusOK, summaries),
			})

			// Act
			got, err := c.ListContainers(context.Background(), true)

			// Assert
			if err != nil {
				t.Fatalf("ListContainers() error = %v, want nil", err)
			}
			if len(got.Items) != tt.wantLen {
				t.Errorf("len(Items) = %d, want %d", len(got.Items), tt.wantLen)
			}
			if got.Truncated != tt.wantTruncated {
				t.Errorf("Truncated = %v, want %v", got.Truncated, tt.wantTruncated)
			}
		})
	}
}

// TestEngineNotFoundClassification drives every wrapper method through a
// fake Engine answering 404, proving classify's not-found mapping is
// reached from the real call path, not just exercised as a pure function.
func TestEngineNotFoundClassification(t *testing.T) {
	t.Parallel()

	for _, spec := range opSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			const ref = "ref1"
			c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
				spec.route(ref): errorHandler(http.StatusNotFound),
			})

			// Act
			err := spec.run(context.Background(), c, ref)

			// Assert
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want errors.Is(err, ErrNotFound)", err)
			}
		})
	}
}

// TestEngineGenericFailure drives every wrapper method through a fake Engine
// answering 500, proving a non-not-found Engine failure surfaces as an
// error that is NOT ErrNotFound.
func TestEngineGenericFailure(t *testing.T) {
	t.Parallel()

	for _, spec := range opSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			const ref = "ref1"
			c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
				spec.route(ref): errorHandler(http.StatusInternalServerError),
			})

			// Act
			err := spec.run(context.Background(), c, ref)

			// Assert
			if err == nil {
				t.Fatal("err = nil, want a generic engine failure")
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want NOT errors.Is(err, ErrNotFound)", err)
			}
		})
	}
}

// TestEngineValidateRefFirst drives every inspect method with an invalid
// reference, proving ValidateRef rejects it before any Engine request is
// made: the fake Engine records zero requests.
func TestEngineValidateRefFirst(t *testing.T) {
	t.Parallel()

	const badRef = "../../info"

	for _, spec := range opSpecs() {
		if spec.callInspect == nil {
			continue
		}

		t.Run(spec.name, func(t *testing.T) {
			t.Parallel()

			// Arrange: no routes registered — any request the Engine
			// receives is itself proof of failure via the recorder below.
			c, rec := newFakeEngine(t, map[string]http.HandlerFunc{})

			// Act
			err := spec.callInspect(context.Background(), c, badRef)

			// Assert
			if !errors.Is(err, ErrInvalidRef) {
				t.Fatalf("err = %v, want errors.Is(err, ErrInvalidRef)", err)
			}
			if got := rec.Count(); got != 0 {
				t.Errorf("engine request count = %d, want 0", got)
			}
		})
	}
}

// TestEngineEmptyList drives every list method through a fake Engine
// returning zero objects, proving the response marshals as a non-nil, empty
// Items slice with Truncated false, through the real call path.
func TestEngineEmptyList(t *testing.T) {
	t.Parallel()

	for _, spec := range opSpecs() {
		if spec.callList == nil {
			continue
		}

		t.Run(spec.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			var body any
			switch spec.name {
			case "ListVolumes":
				body = volume.ListResponse{Volumes: []volume.Volume{}}
			case "ListContainers":
				body = []container.Summary{}
			case "ListImages":
				body = []image.Summary{}
			case "ListNetworks":
				body = []network.Summary{}
			}
			c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
				spec.route(""): jsonHandler(http.StatusOK, body),
			})

			// Act
			itemsLen, itemsNonNil, truncated, err := spec.callList(context.Background(), c)

			// Assert
			if err != nil {
				t.Fatalf("%s() error = %v, want nil", spec.name, err)
			}
			if itemsLen != 0 {
				t.Errorf("len(Items) = %d, want 0", itemsLen)
			}
			if !itemsNonNil {
				t.Error("Items is nil, want a non-nil empty slice")
			}
			if truncated {
				t.Error("Truncated = true, want false")
			}
		})
	}
}

// TestListVolumesWarningsNotInResponse proves VolumeListResult.Warnings —
// which can name driver-internal host paths — never reaches the returned
// DTOs, only the log.
func TestListVolumesWarningsNotInResponse(t *testing.T) {
	t.Parallel()

	// Arrange
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		"GET /volumes": jsonHandler(http.StatusOK, volume.ListResponse{
			Volumes:  []volume.Volume{{Name: "vol1", Driver: "local"}},
			Warnings: []string{"/var/lib/docker/volumes/broken: unreadable"},
		}),
	})

	// Act
	got, err := c.ListVolumes(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("ListVolumes() error = %v, want nil", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(got.Items))
	}
	marshalled, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal ListVolumes result: %v", err)
	}
	if got, want := string(marshalled), "unreadable"; regexp.MustCompile(want).MatchString(got) {
		t.Errorf("response body %q contains warning text %q", got, want)
	}
}
