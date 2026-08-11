// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"context"
	"log/slog"

	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// ListVolumes returns every volume on the host, capped at maxListItems.
func (c *Client) ListVolumes(ctx context.Context) (ListResult[VolumeSummary], error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return ListResult[VolumeSummary]{}, classify("list volumes", err)
	}

	// Warnings can name driver-internal host paths and must never reach the
	// response body; they are logged for the operator instead.
	if len(res.Warnings) > 0 {
		c.log.Warn("volume list warnings", slog.Any("warnings", res.Warnings))
	}

	items := make([]VolumeSummary, 0, len(res.Items))
	for _, v := range res.Items {
		items = append(items, toVolumeSummary(v))
	}

	items, truncated := truncate(items)

	return ListResult[VolumeSummary]{Items: items, Truncated: truncated}, nil
}

// InspectVolume returns the full projection of a single volume. ref is
// validated before any Engine call reaches out over the network.
func (c *Client) InspectVolume(ctx context.Context, ref string) (VolumeSummary, error) {
	if err := ValidateRef(ref); err != nil {
		return VolumeSummary{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.VolumeInspect(ctx, ref, client.VolumeInspectOptions{})
	if err != nil {
		return VolumeSummary{}, classify("inspect volume", err)
	}

	return toVolumeSummary(res.Volume), nil
}

// toVolumeSummary projects a volume.Volume onto the allowlisted DTO. It is
// used for both list entries and the inspect response (VolumeDetail is
// VolumeSummary). CreatedAt is already an RFC3339 string. UsageData can be
// nil, in which case SizeBytes stays nil. Options is never mapped: for tmpfs
// and CIFS/NFS volumes it routinely carries credentials (D2).
func toVolumeSummary(v volume.Volume) VolumeSummary {
	var sizeBytes *int64
	if v.UsageData != nil {
		size := v.UsageData.Size
		sizeBytes = &size
	}

	return VolumeSummary{
		Name:       v.Name,
		Driver:     v.Driver,
		Mountpoint: v.Mountpoint,
		CreatedAt:  v.CreatedAt,
		Scope:      v.Scope,
		Labels:     defaultLabels(v.Labels),
		SizeBytes:  sizeBytes,
	}
}
