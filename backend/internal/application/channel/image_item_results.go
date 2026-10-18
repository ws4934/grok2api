package channel

import (
	"context"
	"encoding/base64"
	"errors"
	"io"

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
		out := []ImageResult{}
		for _, asset := range item.AssetIDs {
			if format == "url" {
				out = append(out, ImageResult{URL: s.images.media.PublicImageURL(asset)})
				continue
			}
			_, r, e := s.images.media.OpenImage(ctx, asset)
			if e != nil {
				return nil, e
			}
			raw, e := io.ReadAll(io.LimitReader(r, 64<<20+1))
			r.Close()
			if e != nil || len(raw) > 64<<20 {
				return nil, errors.New("result unavailable")
			}
			out = append(out, ImageResult{Base64: base64.StdEncoding.EncodeToString(raw)})
		}
		return out, nil
	}
	return nil, domain.ErrNotFound
}
