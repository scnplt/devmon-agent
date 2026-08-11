// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/scnplt/devmon-agent/internal/config"
	"github.com/scnplt/devmon-agent/internal/dockerx"
	"github.com/scnplt/devmon-agent/internal/policy"
	"github.com/scnplt/devmon-agent/internal/state"
)

// fakeDocker implements DockerReader with a per-method function field, so
// each test injects only the behaviour it needs. A nil function field
// answers with a zero value and no error rather than panicking, mirroring
// what a handler sees from a Docker reader that has nothing to say yet.
type fakeDocker struct {
	listContainersFn   func(ctx context.Context, all bool) (dockerx.ListResult[dockerx.ContainerSummary], error)
	inspectContainerFn func(ctx context.Context, ref string) (dockerx.ContainerDetail, error)
	listImagesFn       func(ctx context.Context) (dockerx.ListResult[dockerx.ImageSummary], error)
	inspectImageFn     func(ctx context.Context, ref string) (dockerx.ImageDetail, error)
	listNetworksFn     func(ctx context.Context) (dockerx.ListResult[dockerx.NetworkSummary], error)
	inspectNetworkFn   func(ctx context.Context, ref string) (dockerx.NetworkDetail, error)
	listVolumesFn      func(ctx context.Context) (dockerx.ListResult[dockerx.VolumeSummary], error)
	inspectVolumeFn    func(ctx context.Context, ref string) (dockerx.VolumeSummary, error)

	// containerLogsFn and streamContainerLogsFn back LogReader (D14). Nil-safe
	// by default like every field above, so every existing test that leaves
	// them unset keeps working unmodified.
	containerLogsFn       func(ctx context.Context, ref string, opts dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error)
	streamContainerLogsFn func(ctx context.Context, ref string, opts dockerx.LogOptions, emit func(dockerx.LogLine) error) error

	// The five lifecycle fields back ContainerController (D7). Nil-safe by
	// default like every field above: a nil function answers with no error,
	// mirroring an Engine call that succeeded.
	startContainerFn   func(ctx context.Context, ref string) error
	restartContainerFn func(ctx context.Context, ref string) error
	stopContainerFn    func(ctx context.Context, ref string) error
	killContainerFn    func(ctx context.Context, ref string) error
	removeContainerFn  func(ctx context.Context, ref string) error
}

var _ DockerReader = (*fakeDocker)(nil)

func (fd *fakeDocker) ListContainers(ctx context.Context, all bool) (dockerx.ListResult[dockerx.ContainerSummary], error) {
	if fd.listContainersFn == nil {
		return dockerx.ListResult[dockerx.ContainerSummary]{}, nil
	}
	return fd.listContainersFn(ctx, all)
}

func (fd *fakeDocker) InspectContainer(ctx context.Context, ref string) (dockerx.ContainerDetail, error) {
	if fd.inspectContainerFn == nil {
		return dockerx.ContainerDetail{}, nil
	}
	return fd.inspectContainerFn(ctx, ref)
}

func (fd *fakeDocker) ListImages(ctx context.Context) (dockerx.ListResult[dockerx.ImageSummary], error) {
	if fd.listImagesFn == nil {
		return dockerx.ListResult[dockerx.ImageSummary]{}, nil
	}
	return fd.listImagesFn(ctx)
}

func (fd *fakeDocker) InspectImage(ctx context.Context, ref string) (dockerx.ImageDetail, error) {
	if fd.inspectImageFn == nil {
		return dockerx.ImageDetail{}, nil
	}
	return fd.inspectImageFn(ctx, ref)
}

func (fd *fakeDocker) ListNetworks(ctx context.Context) (dockerx.ListResult[dockerx.NetworkSummary], error) {
	if fd.listNetworksFn == nil {
		return dockerx.ListResult[dockerx.NetworkSummary]{}, nil
	}
	return fd.listNetworksFn(ctx)
}

func (fd *fakeDocker) InspectNetwork(ctx context.Context, ref string) (dockerx.NetworkDetail, error) {
	if fd.inspectNetworkFn == nil {
		return dockerx.NetworkDetail{}, nil
	}
	return fd.inspectNetworkFn(ctx, ref)
}

func (fd *fakeDocker) ListVolumes(ctx context.Context) (dockerx.ListResult[dockerx.VolumeSummary], error) {
	if fd.listVolumesFn == nil {
		return dockerx.ListResult[dockerx.VolumeSummary]{}, nil
	}
	return fd.listVolumesFn(ctx)
}

func (fd *fakeDocker) InspectVolume(ctx context.Context, ref string) (dockerx.VolumeSummary, error) {
	if fd.inspectVolumeFn == nil {
		return dockerx.VolumeSummary{}, nil
	}
	return fd.inspectVolumeFn(ctx, ref)
}

func (fd *fakeDocker) ContainerLogs(ctx context.Context, ref string, opts dockerx.LogOptions) (dockerx.ListResult[dockerx.LogLine], error) {
	if fd.containerLogsFn == nil {
		return dockerx.ListResult[dockerx.LogLine]{}, nil
	}
	return fd.containerLogsFn(ctx, ref, opts)
}

func (fd *fakeDocker) StreamContainerLogs(ctx context.Context, ref string, opts dockerx.LogOptions, emit func(dockerx.LogLine) error) error {
	if fd.streamContainerLogsFn == nil {
		return nil
	}
	return fd.streamContainerLogsFn(ctx, ref, opts, emit)
}

func (fd *fakeDocker) StartContainer(ctx context.Context, ref string) error {
	if fd.startContainerFn == nil {
		return nil
	}
	return fd.startContainerFn(ctx, ref)
}

func (fd *fakeDocker) RestartContainer(ctx context.Context, ref string) error {
	if fd.restartContainerFn == nil {
		return nil
	}
	return fd.restartContainerFn(ctx, ref)
}

func (fd *fakeDocker) StopContainer(ctx context.Context, ref string) error {
	if fd.stopContainerFn == nil {
		return nil
	}
	return fd.stopContainerFn(ctx, ref)
}

func (fd *fakeDocker) KillContainer(ctx context.Context, ref string) error {
	if fd.killContainerFn == nil {
		return nil
	}
	return fd.killContainerFn(ctx, ref)
}

func (fd *fakeDocker) RemoveContainer(ctx context.Context, ref string) error {
	if fd.removeContainerFn == nil {
		return nil
	}
	return fd.removeContainerFn(ctx, ref)
}

// testServerWithDocker is a fifth Server helper, additive to testServer,
// testServerWithCA, testServerWithStore, and testServerForPairing: the read
// routes need both a real *state.Store, to drive requireDevice, and an
// injected DockerReader. testServer keeps constructing servers with a nil
// dc, which several passing tests rely on, so this helper stays separate
// rather than widening it.
func testServerWithDocker(t *testing.T, mode policy.Mode, dc DockerReader) (*Server, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{StateDir: dir, ListenAddr: ":8443", PolicyMode: mode}
	st, err := state.Open(context.Background(), filepath.Join(dir, "devmon.db"), testLogger())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewServer(cfg, st, nil, dc, nil, testLogger()), st
}

// testServerWithDockerAndLogger mirrors testServerWithDocker but takes an
// explicit logger, for the one test that needs to inspect what was logged.
func testServerWithDockerAndLogger(t *testing.T, mode policy.Mode, dc DockerReader, log *slog.Logger) (*Server, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{StateDir: dir, ListenAddr: ":8443", PolicyMode: mode}
	st, err := state.Open(context.Background(), filepath.Join(dir, "devmon.db"), log)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewServer(cfg, st, nil, dc, nil, log), st
}

// pairDeviceForRead creates and records an active device in st, returning
// the serial of the certificate that authenticates it, for driving
// requireDevice without a real TLS handshake — the same technique
// status_test.go's TestRequireDeviceResolvesRealDevice uses.
func pairDeviceForRead(t *testing.T, st *state.Store) *big.Int {
	t.Helper()
	ctx := context.Background()
	device, err := st.CreateDevice(ctx, "read test device")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	serial := big.NewInt(time.Now().UnixNano())
	now := time.Now()
	if err := st.RecordDeviceCert(ctx, device.ID, serial.Text(16), now, now.Add(90*24*time.Hour)); err != nil {
		t.Fatalf("RecordDeviceCert: %v", err)
	}
	return serial
}

// newCapturingLogger returns a logger whose output lands in the returned
// buffer, for the one test that must prove an Engine error's text reaches
// the log but not the response body.
func newCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// newCapturingLoggerAtLevel mirrors newCapturingLogger but with an explicit
// minimum level, for the tests in logs_test.go that must observe a DEBUG
// line — slog.NewTextHandler's default level is INFO, so the default helper
// above would silently drop it and the test would prove nothing.
func newCapturingLoggerAtLevel(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})), &buf
}

// dockerRouteCase describes one of the eight read routes: how to make its
// backing fakeDocker method fail, how to make it succeed, and how to verify
// a successful body.
type dockerRouteCase struct {
	name    string
	path    string
	setErr  func(fd *fakeDocker, err error)
	setOK   func(fd *fakeDocker)
	checkOK func(t *testing.T, body []byte)
}

func dockerRouteCases() []dockerRouteCase {
	return []dockerRouteCase{
		{
			name: "list containers",
			path: "/v1/containers",
			setErr: func(fd *fakeDocker, err error) {
				fd.listContainersFn = func(context.Context, bool) (dockerx.ListResult[dockerx.ContainerSummary], error) {
					return dockerx.ListResult[dockerx.ContainerSummary]{}, err
				}
			},
			setOK: func(fd *fakeDocker) {
				fd.listContainersFn = func(context.Context, bool) (dockerx.ListResult[dockerx.ContainerSummary], error) {
					return dockerx.ListResult[dockerx.ContainerSummary]{Items: []dockerx.ContainerSummary{{ID: "c1"}}}, nil
				}
			},
			checkOK: func(t *testing.T, body []byte) {
				var got dockerx.ListResult[dockerx.ContainerSummary]
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(got.Items) != 1 || got.Items[0].ID != "c1" {
					t.Errorf("items = %+v, want one item with id c1", got.Items)
				}
			},
		},
		{
			name: "inspect container",
			path: "/v1/containers/abc123",
			setErr: func(fd *fakeDocker, err error) {
				fd.inspectContainerFn = func(context.Context, string) (dockerx.ContainerDetail, error) {
					return dockerx.ContainerDetail{}, err
				}
			},
			setOK: func(fd *fakeDocker) {
				fd.inspectContainerFn = func(context.Context, string) (dockerx.ContainerDetail, error) {
					return dockerx.ContainerDetail{ID: "c1", Name: "/c1"}, nil
				}
			},
			checkOK: func(t *testing.T, body []byte) {
				var got dockerx.ContainerDetail
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.ID != "c1" {
					t.Errorf("id = %q, want c1", got.ID)
				}
			},
		},
		{
			name: "list images",
			path: "/v1/images",
			setErr: func(fd *fakeDocker, err error) {
				fd.listImagesFn = func(context.Context) (dockerx.ListResult[dockerx.ImageSummary], error) {
					return dockerx.ListResult[dockerx.ImageSummary]{}, err
				}
			},
			setOK: func(fd *fakeDocker) {
				fd.listImagesFn = func(context.Context) (dockerx.ListResult[dockerx.ImageSummary], error) {
					return dockerx.ListResult[dockerx.ImageSummary]{Items: []dockerx.ImageSummary{{ID: "i1"}}}, nil
				}
			},
			checkOK: func(t *testing.T, body []byte) {
				var got dockerx.ListResult[dockerx.ImageSummary]
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(got.Items) != 1 || got.Items[0].ID != "i1" {
					t.Errorf("items = %+v, want one item with id i1", got.Items)
				}
			},
		},
		{
			name: "inspect image",
			path: "/v1/images/abc123",
			setErr: func(fd *fakeDocker, err error) {
				fd.inspectImageFn = func(context.Context, string) (dockerx.ImageDetail, error) {
					return dockerx.ImageDetail{}, err
				}
			},
			setOK: func(fd *fakeDocker) {
				fd.inspectImageFn = func(context.Context, string) (dockerx.ImageDetail, error) {
					return dockerx.ImageDetail{ID: "i1"}, nil
				}
			},
			checkOK: func(t *testing.T, body []byte) {
				var got dockerx.ImageDetail
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.ID != "i1" {
					t.Errorf("id = %q, want i1", got.ID)
				}
			},
		},
		{
			name: "list networks",
			path: "/v1/networks",
			setErr: func(fd *fakeDocker, err error) {
				fd.listNetworksFn = func(context.Context) (dockerx.ListResult[dockerx.NetworkSummary], error) {
					return dockerx.ListResult[dockerx.NetworkSummary]{}, err
				}
			},
			setOK: func(fd *fakeDocker) {
				fd.listNetworksFn = func(context.Context) (dockerx.ListResult[dockerx.NetworkSummary], error) {
					return dockerx.ListResult[dockerx.NetworkSummary]{Items: []dockerx.NetworkSummary{{ID: "n1"}}}, nil
				}
			},
			checkOK: func(t *testing.T, body []byte) {
				var got dockerx.ListResult[dockerx.NetworkSummary]
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(got.Items) != 1 || got.Items[0].ID != "n1" {
					t.Errorf("items = %+v, want one item with id n1", got.Items)
				}
			},
		},
		{
			name: "inspect network",
			path: "/v1/networks/abc123",
			setErr: func(fd *fakeDocker, err error) {
				fd.inspectNetworkFn = func(context.Context, string) (dockerx.NetworkDetail, error) {
					return dockerx.NetworkDetail{}, err
				}
			},
			setOK: func(fd *fakeDocker) {
				fd.inspectNetworkFn = func(context.Context, string) (dockerx.NetworkDetail, error) {
					return dockerx.NetworkDetail{NetworkSummary: dockerx.NetworkSummary{ID: "n1"}}, nil
				}
			},
			checkOK: func(t *testing.T, body []byte) {
				var got dockerx.NetworkDetail
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.ID != "n1" {
					t.Errorf("id = %q, want n1", got.ID)
				}
			},
		},
		{
			name: "list volumes",
			path: "/v1/volumes",
			setErr: func(fd *fakeDocker, err error) {
				fd.listVolumesFn = func(context.Context) (dockerx.ListResult[dockerx.VolumeSummary], error) {
					return dockerx.ListResult[dockerx.VolumeSummary]{}, err
				}
			},
			setOK: func(fd *fakeDocker) {
				fd.listVolumesFn = func(context.Context) (dockerx.ListResult[dockerx.VolumeSummary], error) {
					return dockerx.ListResult[dockerx.VolumeSummary]{Items: []dockerx.VolumeSummary{{Name: "v1"}}}, nil
				}
			},
			checkOK: func(t *testing.T, body []byte) {
				var got dockerx.ListResult[dockerx.VolumeSummary]
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(got.Items) != 1 || got.Items[0].Name != "v1" {
					t.Errorf("items = %+v, want one item named v1", got.Items)
				}
			},
		},
		{
			name: "inspect volume",
			path: "/v1/volumes/myvol",
			setErr: func(fd *fakeDocker, err error) {
				fd.inspectVolumeFn = func(context.Context, string) (dockerx.VolumeSummary, error) {
					return dockerx.VolumeSummary{}, err
				}
			},
			setOK: func(fd *fakeDocker) {
				fd.inspectVolumeFn = func(context.Context, string) (dockerx.VolumeSummary, error) {
					return dockerx.VolumeSummary{Name: "v1"}, nil
				}
			},
			checkOK: func(t *testing.T, body []byte) {
				var got dockerx.VolumeSummary
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.Name != "v1" {
					t.Errorf("name = %q, want v1", got.Name)
				}
			},
		},
	}
}

// TestReadRoutesSucceed asserts each of the eight routes answers 200 with
// the fake's data when a valid device drives the request.
func TestReadRoutesSucceed(t *testing.T) {
	t.Parallel()

	for _, tc := range dockerRouteCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			fd := &fakeDocker{}
			tc.setOK(fd)
			s, st := testServerWithDocker(t, policy.ModeDefault, fd)
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(http.MethodGet, tc.path, nil, serial)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			tc.checkOK(t, rec.Body.Bytes())
		})
	}
}

// TestReadRoutesNotFound asserts each route maps dockerx.ErrNotFound to a
// 404 with the exact fixed body.
func TestReadRoutesNotFound(t *testing.T) {
	t.Parallel()

	for _, tc := range dockerRouteCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			fd := &fakeDocker{}
			tc.setErr(fd, dockerx.ErrNotFound)
			s, st := testServerWithDocker(t, policy.ModeDefault, fd)
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(http.MethodGet, tc.path, nil, serial)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
			}
			want := `{"error":"not found"}` + "\n"
			if rec.Body.String() != want {
				t.Errorf("body = %q, want %q", rec.Body.String(), want)
			}
		})
	}
}

// TestReadRoutesInvalidRef asserts each route maps dockerx.ErrInvalidRef to
// a 400.
func TestReadRoutesInvalidRef(t *testing.T) {
	t.Parallel()

	for _, tc := range dockerRouteCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			fd := &fakeDocker{}
			tc.setErr(fd, dockerx.ErrInvalidRef)
			s, st := testServerWithDocker(t, policy.ModeDefault, fd)
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(http.MethodGet, tc.path, nil, serial)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
			}
			var body errorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != msgInvalidRef {
				t.Errorf("error = %q, want %q", body.Error, msgInvalidRef)
			}
		})
	}
}

// TestReadRoutesEngineFailure asserts a generic Engine error becomes a 502
// with the fixed msgEngineUnavailable body, and that the underlying error
// text is logged but never returned to the client.
func TestReadRoutesEngineFailure(t *testing.T) {
	t.Parallel()

	const wantLogged = "engine handle closed unexpectedly on host-internal-path"

	for _, tc := range dockerRouteCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			fd := &fakeDocker{}
			tc.setErr(fd, errors.New(wantLogged))
			log, buf := newCapturingLogger()
			s, st := testServerWithDockerAndLogger(t, policy.ModeDefault, fd, log)
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(http.MethodGet, tc.path, nil, serial)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
			}
			var body errorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != msgEngineUnavailable {
				t.Errorf("error = %q, want %q", body.Error, msgEngineUnavailable)
			}
			if bodyContains(rec.Body.String(), wantLogged) {
				t.Errorf("response body leaks the underlying error text: %s", rec.Body.String())
			}
			if !bodyContains(buf.String(), wantLogged) {
				t.Errorf("log does not contain the underlying error text; log: %s", buf.String())
			}
		})
	}
}

// TestReadRoutesRequireDevice asserts each route answers 401 with the
// standard terse body when no client certificate is presented.
func TestReadRoutesRequireDevice(t *testing.T) {
	t.Parallel()

	for _, tc := range dockerRouteCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s, _ := testServerWithDocker(t, policy.ModeDefault, &fakeDocker{})
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
			}
			var body errorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != msgClientCertRequired {
				t.Errorf("error = %q, want %q", body.Error, msgClientCertRequired)
			}
		})
	}
}

// TestReadRoutesRejectOtherMethods asserts POST to each known read path
// answers 405, mirroring TestStatusRejectsOtherMethods.
func TestReadRoutesRejectOtherMethods(t *testing.T) {
	t.Parallel()

	for _, tc := range dockerRouteCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s, _ := testServerWithDocker(t, policy.ModeDefault, &fakeDocker{})
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("POST %s = %d, want 405", tc.path, rec.Code)
			}
		})
	}
}

// TestReadRoutesAllowedUnderReadOnlyPolicy proves D10: OpRead is permitted
// by every policy mode, so ModeReadOnly must not change the observable
// behaviour of any of the eight routes.
func TestReadRoutesAllowedUnderReadOnlyPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range dockerRouteCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			fd := &fakeDocker{}
			tc.setOK(fd)
			s, st := testServerWithDocker(t, policy.ModeReadOnly, fd)
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(http.MethodGet, tc.path, nil, serial)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusOK {
				t.Errorf("status under read-only policy = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestListAllParameter confirms the ParseBool-defaults-to-false rule: a
// malformed or absent ?all= value is treated as false rather than 400.
func TestListAllParameter(t *testing.T) {
	t.Parallel()

	// Arrange
	var gotAll []bool
	fd := &fakeDocker{
		listContainersFn: func(_ context.Context, all bool) (dockerx.ListResult[dockerx.ContainerSummary], error) {
			gotAll = append(gotAll, all)
			return dockerx.ListResult[dockerx.ContainerSummary]{}, nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)

	queries := []string{"?all=true", "?all=false", "?all=", "?all=yes", ""}

	// Act
	for _, q := range queries {
		req := requestWithPeerSerial(http.MethodGet, "/v1/containers"+q, nil, serial)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("query %q: status = %d, want 200; body: %s", q, rec.Code, rec.Body.String())
		}
	}

	// Assert
	want := []bool{true, false, false, false, false}
	if len(gotAll) != len(want) {
		t.Fatalf("fake observed %d calls (%v), want %d", len(gotAll), gotAll, len(want))
	}
	for i := range want {
		if gotAll[i] != want[i] {
			t.Errorf("call %d (%q): all = %v, want %v", i, queries[i], gotAll[i], want[i])
		}
	}
}

// TestNilDockerReader asserts every read route answers 502 without panicking
// when the server has no Docker reader configured.
func TestNilDockerReader(t *testing.T) {
	t.Parallel()

	for _, tc := range dockerRouteCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			s, st := testServerWithDocker(t, policy.ModeDefault, nil)
			serial := pairDeviceForRead(t, st)
			req := requestWithPeerSerial(http.MethodGet, tc.path, nil, serial)
			rec := httptest.NewRecorder()

			// Act
			s.routes().ServeHTTP(rec, req)

			// Assert
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
			}
			var body errorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error != msgEngineUnavailable {
				t.Errorf("error = %q, want %q", body.Error, msgEngineUnavailable)
			}
		})
	}
}

// TestEmptyListMarshalsAsArray asserts that an empty list from the fake
// serialises as "items":[], never "items":null.
func TestEmptyListMarshalsAsArray(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{
		listContainersFn: func(context.Context, bool) (dockerx.ListResult[dockerx.ContainerSummary], error) {
			return dockerx.ListResult[dockerx.ContainerSummary]{Items: []dockerx.ContainerSummary{}}, nil
		},
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)
	req := requestWithPeerSerial(http.MethodGet, "/v1/containers", nil, serial)
	rec := httptest.NewRecorder()

	// Act
	s.routes().ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !bodyContains(rec.Body.String(), `"items":[]`) {
		t.Errorf("body = %q, want it to contain %q", rec.Body.String(), `"items":[]`)
	}
	if bodyContains(rec.Body.String(), `"items":null`) {
		t.Errorf("body = %q, an empty list must never marshal as null", rec.Body.String())
	}
}

// TestErrorBodiesLeakNothing asserts that across every failure path, the
// response body contains none of the host's state directory path, the
// Docker socket, or the state database filename.
func TestErrorBodiesLeakNothing(t *testing.T) {
	t.Parallel()

	// Arrange
	fd := &fakeDocker{}
	for _, tc := range dockerRouteCases() {
		tc.setErr(fd, errors.New("dial unix "+`/var/run/docker.sock`+": connect: no such file or directory"))
	}
	s, st := testServerWithDocker(t, policy.ModeDefault, fd)
	serial := pairDeviceForRead(t, st)

	var bodies []string

	// Act — collect every failure body: 401 (no cert), 400/404/502 (fake
	// errors), and 405 (wrong method).
	for _, tc := range dockerRouteCases() {
		unauth := httptest.NewRequest(http.MethodGet, tc.path, nil)
		unauthRec := httptest.NewRecorder()
		s.routes().ServeHTTP(unauthRec, unauth)
		bodies = append(bodies, unauthRec.Body.String())

		wrongMethod := httptest.NewRequest(http.MethodPost, tc.path, nil)
		wrongMethodRec := httptest.NewRecorder()
		s.routes().ServeHTTP(wrongMethodRec, wrongMethod)
		bodies = append(bodies, wrongMethodRec.Body.String())

		engineFail := requestWithPeerSerial(http.MethodGet, tc.path, nil, serial)
		engineFailRec := httptest.NewRecorder()
		s.routes().ServeHTTP(engineFailRec, engineFail)
		bodies = append(bodies, engineFailRec.Body.String())

		tc.setErr(fd, dockerx.ErrNotFound)
		notFound := requestWithPeerSerial(http.MethodGet, tc.path, nil, serial)
		notFoundRec := httptest.NewRecorder()
		s.routes().ServeHTTP(notFoundRec, notFound)
		bodies = append(bodies, notFoundRec.Body.String())

		tc.setErr(fd, dockerx.ErrInvalidRef)
		invalidRef := requestWithPeerSerial(http.MethodGet, tc.path, nil, serial)
		invalidRefRec := httptest.NewRecorder()
		s.routes().ServeHTTP(invalidRefRec, invalidRef)
		bodies = append(bodies, invalidRefRec.Body.String())
	}

	// Assert
	for _, leak := range []string{s.cfg.StateDir, "docker.sock", "devmon.db"} {
		for _, body := range bodies {
			if leak != "" && bodyContains(body, leak) {
				t.Errorf("a failure body leaks %q: %s", leak, body)
			}
		}
	}
}
