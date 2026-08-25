// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// ContainerStates returns the current state and health of every container on
// the host, including stopped ones — the payload of the event stream's opening
// snapshot.
//
// One ContainerList call, never a per-container inspect. container.Summary
// carries Health directly, and N+1 inspects taken at N different instants
// would produce a "snapshot" that was never simultaneously true.
//
// Health arrived on the list response in Docker API v1.52 (Engine 29). On an
// older Engine Summary.Health is nil for every container and they all report
// "none"; the events themselves are unaffected.
func (c *Client) ContainerStates(ctx context.Context) ([]ContainerStateSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, classify("list container states", err)
	}

	items := make([]ContainerStateSummary, 0, len(res.Items))
	for _, s := range res.Items {
		items = append(items, toContainerStateSummary(s))
	}

	items, truncated := truncate(items)
	if truncated {
		c.log.Warn("container state snapshot truncated", slog.Int("count", len(res.Items)))
	}

	return items, nil
}

// toContainerStateSummary projects a container.Summary onto the snapshot DTO.
// s.Health is a pointer and is nil for a container with no healthcheck and on
// every Engine older than API v1.52 — both mean "none".
func toContainerStateSummary(s container.Summary) ContainerStateSummary {
	var name string
	if len(s.Names) > 0 {
		name = trimContainerName(s.Names[0])
	}

	return ContainerStateSummary{
		ID:     s.ID,
		Name:   name,
		State:  string(s.State),
		Health: healthOrNone(s.Health),
	}
}

// healthOrNone maps a *container.HealthSummary onto the four-value contract
// vocabulary. A nil summary, an empty status, or a status the Engine invents
// in a future release all collapse to "none": the field is an enum on the
// wire, and forwarding an unrecognised value would break that promise.
func healthOrNone(h *container.HealthSummary) string {
	if h == nil {
		return string(container.NoHealthcheck)
	}

	switch h.Status {
	case container.NoHealthcheck, container.Starting, container.Healthy, container.Unhealthy:
		return string(h.Status)
	default:
		return string(container.NoHealthcheck)
	}
}

// trimContainerName strips the single leading "/" the Engine puts on container
// names in list responses. The events API's own "name" attribute has no slash,
// and the snapshot and the events it is reconciled against must agree.
func trimContainerName(name string) string {
	return strings.TrimPrefix(name, "/")
}

// eventHealthStatus is the normalised literal both health actions forward as.
const eventHealthStatus = "health_status"

// eventActions is the closed allowlist of Docker container actions this agent
// forwards, mapped onto the normalised literal that reaches the wire.
//
// Map-driven so an action outside it can never fall through. The health entries
// match the two EXACT SDK constants and nothing else: events.Action for a health
// event is a compound string, and the SDK's own doc comment records that a
// health_status action can be free-form, "followed by the output of the
// health-check output". Matching a "health_status" PREFIX would therefore
// publish healthcheck output to a client and, worse, into the agent's log.
var eventActions = map[events.Action]string{
	events.ActionHealthStatusHealthy:   eventHealthStatus,
	events.ActionHealthStatusUnhealthy: eventHealthStatus,
	events.ActionDie:                   "die",
	events.ActionStart:                 "start",
	events.ActionStop:                  "stop",
	events.ActionOOM:                   "oom",
}

// eventHealthByAction gives the resulting health value for the two health
// actions. Absent for every lifecycle action, which is why ContainerEvent.Health
// is omitempty.
var eventHealthByAction = map[events.Action]string{
	events.ActionHealthStatusHealthy:   string(container.Healthy),
	events.ActionHealthStatusUnhealthy: string(container.Unhealthy),
}

// toContainerEvent projects an Engine message onto the allowlisted DTO,
// reporting false for anything outside eventActions.
//
// msg.Actor.Attributes is the container's LABEL SET (see events.Actor's own doc
// comment). It is never forwarded and never logged; exactly one key, "name", is
// read out of it.
func toContainerEvent(msg events.Message) (ContainerEvent, bool) {
	if msg.Type != events.ContainerEventType {
		return ContainerEvent{}, false
	}

	action, ok := eventActions[msg.Action]
	if !ok {
		return ContainerEvent{}, false
	}

	return ContainerEvent{
		ID:     msg.Actor.ID,
		Name:   trimContainerName(msg.Actor.Attributes["name"]),
		Event:  action,
		Health: eventHealthByAction[msg.Action],
		Time:   eventTime(msg),
	}, true
}

// eventTime converts an Engine message's timestamp to RFC3339 UTC, preferring
// the nanosecond field over the second one when both are set.
func eventTime(msg events.Message) string {
	switch {
	case msg.TimeNano != 0:
		return time.Unix(0, msg.TimeNano).UTC().Format(time.RFC3339)
	case msg.Time != 0:
		return time.Unix(msg.Time, 0).UTC().Format(time.RFC3339)
	default:
		return ""
	}
}

// StreamContainerEvents follows the Engine's container event feed, calling emit
// once per allowlisted event until ctx is cancelled, the feed ends, or emit
// returns an error.
//
// onReady is invoked exactly once, after the Engine subscription is established
// and before the first event can be delivered. Callers depend on this: the event
// stream's opening snapshot must be taken AFTER the subscription exists, or an
// event firing in between is in neither the snapshot nor the stream, and this
// feature has no replay to recover it. client.Client.Events blocks internally
// until GET /events has been issued and its response received, so the moment it
// returns is exactly that point.
//
// emit MUST NOT BLOCK. The SDK's message channel is unbuffered, so a slow emit
// stalls the Engine decoder goroutine and backs the feed up for every consumer.
//
// No callTimeout here, deliberately, for the reason StreamContainerLogs gives:
// the subscription's lifetime is ctx and nothing else.
func (c *Client) StreamContainerEvents(ctx context.Context, onReady func(), emit func(ContainerEvent) error) error {
	filters := make(client.Filters).Add("type", string(events.ContainerEventType))
	res := c.api.Events(ctx, client.EventsListOptions{Filters: filters})

	if onReady != nil {
		onReady()
	}

	for {
		select {
		case <-ctx.Done():
			return nil // a cancelled subscription is a clean stop
		case msg, ok := <-res.Messages:
			if !ok {
				return ErrEventFeedClosed
			}
			ev, forward := toContainerEvent(msg)
			if !forward {
				continue
			}
			if err := emit(ev); err != nil {
				return err
			}
		case err, ok := <-res.Err:
			if !ok {
				return ErrEventFeedClosed
			}
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return ErrEventFeedClosed
			}
			return classify("stream container events", err)
		}
	}
}
