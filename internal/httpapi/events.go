// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"

	"github.com/scnplt/devmon-agent/internal/dockerx"
)

// EventReader is the container event surface, named separately from the read
// interfaces because one of its two methods never returns until the caller
// stops it — the same reason LogReader is its own interface.
type EventReader interface {
	ContainerStates(ctx context.Context) ([]dockerx.ContainerStateSummary, error)
	StreamContainerEvents(ctx context.Context, onReady func(), emit func(dockerx.ContainerEvent) error) error
}
