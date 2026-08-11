// SPDX-License-Identifier: AGPL-3.0-only

package dockerx

import (
	"context"

	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
)

// ListImages returns every image on the host, capped at maxListItems.
func (c *Client) ListImages(ctx context.Context) (ListResult[ImageSummary], error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	// All stays false (the zero value): ImageListOptions.All returns
	// intermediate layers, which is noise on a phone and not part of D12.
	res, err := c.api.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return ListResult[ImageSummary]{}, classify("list images", err)
	}

	items := make([]ImageSummary, 0, len(res.Items))
	for _, s := range res.Items {
		items = append(items, toImageSummary(s))
	}

	items, truncated := truncate(items)

	return ListResult[ImageSummary]{Items: items, Truncated: truncated}, nil
}

// InspectImage returns the full projection of a single image. ref is
// validated before any Engine call reaches out over the network.
func (c *Client) InspectImage(ctx context.Context, ref string) (ImageDetail, error) {
	if err := ValidateRef(ref); err != nil {
		return ImageDetail{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	// ImageInspect is variadic in the v29 SDK, not an options struct.
	res, err := c.api.ImageInspect(ctx, ref)
	if err != nil {
		return ImageDetail{}, classify("inspect image", err)
	}

	return toImageDetail(res.InspectResponse), nil
}

// toImageSummary projects an image.Summary onto the allowlisted DTO. Created
// is Unix seconds and must be converted to RFC3339 UTC.
func toImageSummary(s image.Summary) ImageSummary {
	repoTags := make([]string, 0, len(s.RepoTags))
	repoTags = append(repoTags, s.RepoTags...)

	repoDigests := make([]string, 0, len(s.RepoDigests))
	repoDigests = append(repoDigests, s.RepoDigests...)

	return ImageSummary{
		ID:          s.ID,
		ParentID:    s.ParentID,
		RepoTags:    repoTags,
		RepoDigests: repoDigests,
		CreatedAt:   unixToRFC3339(s.Created),
		Size:        s.Size,
		Containers:  s.Containers,
		Labels:      defaultLabels(s.Labels),
	}
}

// toImageDetail projects an image.InspectResponse onto the allowlisted DTO.
// Created is already an RFC3339Nano string here, unlike Summary.Created's
// int64, and is `json:",omitempty"` upstream so it can be empty — passed
// through as-is, never reformatted. Config, which carries the image's baked-in
// Env, is never mapped (D2).
func toImageDetail(r image.InspectResponse) ImageDetail {
	repoTags := make([]string, 0, len(r.RepoTags))
	repoTags = append(repoTags, r.RepoTags...)

	repoDigests := make([]string, 0, len(r.RepoDigests))
	repoDigests = append(repoDigests, r.RepoDigests...)

	return ImageDetail{
		ID:           r.ID,
		RepoTags:     repoTags,
		RepoDigests:  repoDigests,
		CreatedAt:    r.Created,
		Size:         r.Size,
		Architecture: r.Architecture,
		OS:           r.Os,
		Author:       r.Author,
		Comment:      r.Comment,
	}
}
