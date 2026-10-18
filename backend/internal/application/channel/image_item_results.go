package channel

import (
	"context"
	"errors"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
)

// ImageItemResults returns one succeeded item's images while the rest of its
// job may still run, so streams can deliver each image as soon as it is stored.
func (s *Service) ImageItemResults(ctx context.Context, key uint64, jobID, itemID, format string) ([]ImageResult, error) {
	if format != "url" && format != "b64_json" {
		return nil, errors.New("invalid response_format")
	}
	job, e := s.GetImageJob(ctx, key, jobID)
	if e != nil {
		return nil, e
	}
	for _, item := range job.Items {
		if item.ID != itemID {
			continue
		}
		if item.Status != "succeeded" {
			return nil, domain.ErrConflict
		}
		return s.assetResults(ctx, item.AssetIDs, format)
	}
	return nil, domain.ErrNotFound
}
