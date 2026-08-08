package dockerx

import (
	"context"
	"log/slog"
	"os"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/scnplt/devmon-agent/internal/selfid"
)

// selfVerifyTimeout bounds each candidate's Engine inspect call during self
// resolution. Independent of callTimeout: this runs once at startup, before
// any HTTP handler exists, and a slow candidate must not stall New forever.
const selfVerifyTimeout = 5 * time.Second

// selfInfo is the agent's own container identity, resolved once at startup
// and never written again — Client must stay immutable for its whole life
// (D5).
type selfInfo struct {
	containerized bool
	id            string // full 64-char ID; empty when unresolved
}

// SelfID returns the agent's own resolved container ID, or "" if unresolved.
func (c *Client) SelfID() string { return c.self.id }

// SelfKnown reports whether the agent successfully identified its own
// container.
func (c *Client) SelfKnown() bool { return c.self.id != "" }

// Containerized reports whether the agent is running inside a Docker
// container at all (D4). A non-containerized agent has no container to
// protect, so self-exclusion is inapplicable rather than failing closed.
func (c *Client) Containerized() bool { return c.self.containerized }

// resolveSelf discovers the agent's own container identity. It gathers
// unverified candidates from selfid.Detect and hands them to confirmSelf,
// which is the piece that talks to the Engine (D2, D5, D6).
func (c *Client) resolveSelf(ctx context.Context, override string) selfInfo {
	detected := selfid.Detect("/", override, os.Getenv)
	return c.confirmSelf(ctx, detected)
}

// confirmSelf inspects each candidate against the Engine in order and keeps
// the full ID of the first one the Engine confirms. Split out from
// resolveSelf so it can be exercised directly against a fake Engine, without
// depending on selfid.Detect's real filesystem reads (D6).
func (c *Client) confirmSelf(ctx context.Context, detected selfid.Result) selfInfo {
	for _, candidate := range detected.Candidates {
		cctx, cancel := context.WithTimeout(ctx, selfVerifyTimeout)
		res, err := c.api.ContainerInspect(cctx, candidate, client.ContainerInspectOptions{})
		cancel()
		if err != nil {
			// A candidate the Engine does not recognise is normal, not an
			// error: mountinfo and cgroup can surface stale or unrelated
			// IDs. Only a non-not-found failure is worth a log line, and
			// even that must not stop the search or fail New.
			if !cerrdefs.IsNotFound(err) {
				c.log.Warn("verify self-id candidate",
					slog.String("candidate", candidate),
					slog.Any("err", err),
				)
			}
			continue
		}

		// The full ID, never the candidate string: a 12-hex HOSTNAME
		// candidate would otherwise never compare equal to a resolved
		// target's full ID, and self-exclusion would silently never fire.
		id := res.Container.ID
		c.log.Info("agent self-identified", slog.String("container_id", id))
		return selfInfo{containerized: detected.Containerized, id: id}
	}

	if detected.Containerized {
		c.log.Error("agent could not identify its own container; " +
			"set DEVMON_SELF_CONTAINER_ID to the agent's container ID and restart")
	} else {
		c.log.Info("agent is not running in a container; self-exclusion is inapplicable")
	}

	return selfInfo{containerized: detected.Containerized}
}
