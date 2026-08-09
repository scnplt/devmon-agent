package dockerx

import (
	"context"
	"net/http"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/scnplt/devmon-agent/internal/selfid"
)

// containerInspectFixture builds the minimal Engine inspect response a
// confirmSelf test needs: an ID and nothing else.
func containerInspectFixture(id string) container.InspectResponse {
	return container.InspectResponse{ID: id}
}

// TestResolveSelfFirstConfirmed proves confirmSelf stops at the first
// candidate the Engine confirms and reports its ID, even when later
// candidates in the chain would also resolve.
func TestResolveSelfFirstConfirmed(t *testing.T) {
	t.Parallel()

	// Arrange
	const wantID = "b2222222222222222222222222222222222222222222222222222222222222"
	var candidate3Reached bool
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		"GET /containers/candidate-1/json": errorHandler(http.StatusNotFound),
		"GET /containers/candidate-2/json": jsonHandler(http.StatusOK, containerInspectFixture(wantID)),
		"GET /containers/candidate-3/json": func(w http.ResponseWriter, r *http.Request) {
			candidate3Reached = true
			jsonHandler(http.StatusOK, containerInspectFixture("c333"))(w, r)
		},
	})
	detected := selfid.Result{
		Containerized: true,
		Candidates:    []string{"candidate-1", "candidate-2", "candidate-3"},
	}

	// Act
	got := c.confirmSelf(context.Background(), detected)

	// Assert
	if got.id != wantID {
		t.Errorf("id = %q, want %q", got.id, wantID)
	}
	// The third candidate must never be reached once the second confirms.
	if candidate3Reached {
		t.Error("candidate-3 was inspected, want the search to stop at candidate-2")
	}
}

// TestResolveSelfStoresFullID proves confirmSelf stores the full 64-char ID
// the Engine returns, never the short candidate string that confirmed it. A
// 12-hex HOSTNAME candidate must never leak into self.id, or self-exclusion
// would never compare equal against a resolved target's full ID.
func TestResolveSelfStoresFullID(t *testing.T) {
	t.Parallel()

	// Arrange
	const shortCandidate = "abc123abc123"
	const fullID = "abc123abc123abc123abc123abc123abc123abc123abc123abc123abc123ab"
	c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
		"GET /containers/" + shortCandidate + "/json": jsonHandler(http.StatusOK, containerInspectFixture(fullID)),
	})
	detected := selfid.Result{
		Containerized: true,
		Candidates:    []string{shortCandidate},
	}

	// Act
	got := c.confirmSelf(context.Background(), detected)

	// Assert
	if got.id != fullID {
		t.Fatalf("id = %q, want the full ID %q", got.id, fullID)
	}
	if got.id == shortCandidate {
		t.Error("id equals the short candidate string, want the Engine's full ID")
	}
}

// TestResolveSelfNoneConfirmed proves that when every candidate is unknown
// to the Engine, the agent ends up with no self ID rather than an error —
// resolveSelf/confirmSelf never return an error, so New always succeeds
// regardless of the outcome (D3).
func TestResolveSelfNoneConfirmed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		containerized bool
	}{
		{name: "containerized but unresolved", containerized: true},
		{name: "not containerized", containerized: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			var candidate1Reached, candidate2Reached bool
			c, _ := newFakeEngine(t, map[string]http.HandlerFunc{
				"GET /containers/candidate-1/json": func(w http.ResponseWriter, r *http.Request) {
					candidate1Reached = true
					errorHandler(http.StatusNotFound)(w, r)
				},
				"GET /containers/candidate-2/json": func(w http.ResponseWriter, r *http.Request) {
					candidate2Reached = true
					errorHandler(http.StatusNotFound)(w, r)
				},
			})
			detected := selfid.Result{
				Containerized: tt.containerized,
				Candidates:    []string{"candidate-1", "candidate-2"},
			}

			// Act
			got := c.confirmSelf(context.Background(), detected)

			// Assert
			if got.id != "" {
				t.Errorf("id = %q, want empty", got.id)
			}
			if got.containerized != tt.containerized {
				t.Errorf("containerized = %v, want %v", got.containerized, tt.containerized)
			}
			if !candidate1Reached || !candidate2Reached {
				t.Errorf("candidate1Reached = %v, candidate2Reached = %v, want both true (every candidate tried)",
					candidate1Reached, candidate2Reached)
			}
		})
	}
}

// TestClientSelfAccessors proves SelfID, SelfKnown, and Containerized read
// exactly the fields resolveSelf/confirmSelf populate.
func TestClientSelfAccessors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		self          selfInfo
		wantID        string
		wantKnown     bool
		wantContained bool
	}{
		{
			name:          "resolved and containerized",
			self:          selfInfo{containerized: true, id: "full-id"},
			wantID:        "full-id",
			wantKnown:     true,
			wantContained: true,
		},
		{
			name:          "unresolved but containerized",
			self:          selfInfo{containerized: true, id: ""},
			wantID:        "",
			wantKnown:     false,
			wantContained: true,
		},
		{
			name:          "not containerized",
			self:          selfInfo{containerized: false, id: ""},
			wantID:        "",
			wantKnown:     false,
			wantContained: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			c := &Client{self: tt.self}

			// Act & Assert
			if got := c.SelfID(); got != tt.wantID {
				t.Errorf("SelfID() = %q, want %q", got, tt.wantID)
			}
			if got := c.SelfKnown(); got != tt.wantKnown {
				t.Errorf("SelfKnown() = %v, want %v", got, tt.wantKnown)
			}
			if got := c.Containerized(); got != tt.wantContained {
				t.Errorf("Containerized() = %v, want %v", got, tt.wantContained)
			}
		})
	}
}
