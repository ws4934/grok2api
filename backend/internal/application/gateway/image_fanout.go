package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// errImageAccountsBusy reports that an image of a fanned-out request found no
// free account; a later round draws it.
var errImageAccountsBusy = errors.New("no free account for an extra image")

// maxFannedImageBody bounds one single-image response held in memory.
const maxFannedImageBody = 64 << 20

// fansOutImages reports whether a multi-image request is drawn as single
// images on separate accounts: it does not stream and every route of the
// model draws one image per upstream request.
func (s *Service) fansOutImages(ctx context.Context, publicModel string, count int, streaming bool) bool {
	if count < 2 || streaming {
		return false
	}
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil || len(routes) == 0 {
		return false
	}
	for _, route := range routes {
		if definition, _ := s.providers.Definition(route.Provider); !definition.Media.SingleImagePerRequest {
			return false
		}
	}
	return true
}

type fannedImage struct {
	result *Result
	body   []byte
	err    error
}

// fanOutImages draws count images as single-image requests, in rounds. A
// round starts one request per missing image at once; a request that finds
// no free account gives up quietly and is drawn in the next round, once this
// round's accounts are released, so a sibling holding the only free account
// never makes it fail. When a whole round finds no free account, the next
// round is one plain request: it fails as the request would without fan-out
// (same error and audit), or delivers an image and drawing goes on. Drawing
// stops at the first failure; the delivered images are returned as one
// response, each audited and billed on its own account when drawn.
func (s *Service) fanOutImages(ctx context.Context, count int, draw func(ctx context.Context, extra bool) (*Result, error)) (*Result, error) {
	var delivered, failed []fannedImage
	for extra := true; len(delivered) < count && len(failed) == 0; {
		outcomes := make([]fannedImage, count-len(delivered))
		if !extra {
			outcomes = outcomes[:1]
		}
		var wait sync.WaitGroup
		for index := range outcomes {
			wait.Go(func() {
				outcome := &outcomes[index]
				outcome.result, outcome.err = draw(ctx, extra)
				if outcome.err == nil && auditRequestSucceeded(outcome.result.StatusCode, "") {
					outcome.body, outcome.err = io.ReadAll(io.LimitReader(outcome.result.Body, maxFannedImageBody))
					if outcome.err == nil && !json.Valid(outcome.body) {
						outcome.err = errors.New("image response is not JSON")
					}
					if outcome.err == nil {
						// The image exists (also in the gallery): settle it now, so
						// the next round sees the account free and its quota spent.
						outcome.result.Finalize(Usage{}, "", "")
					}
				}
			})
		}
		wait.Wait()
		extra = false
		for _, outcome := range outcomes {
			switch {
			case errors.Is(outcome.err, errImageAccountsBusy):
			case outcome.err == nil && outcome.body != nil:
				delivered = append(delivered, outcome)
				extra = true
			default:
				failed = append(failed, outcome)
			}
		}
	}
	if len(delivered) == 0 {
		// Report the first failure as a plain request would.
		for _, other := range failed[1:] {
			settleFannedImage(other)
		}
		if first := failed[0]; first.err != nil && first.result != nil {
			settleFannedImage(first)
			return nil, first.err
		}
		return failed[0].result, failed[0].err
	}
	for _, other := range failed {
		settleFannedImage(other)
	}
	bodies := make([][]byte, len(delivered))
	results := make([]*Result, len(delivered))
	for index, image := range delivered {
		bodies[index], results[index] = image.body, image.result
	}
	body, err := mergeImageBodies(bodies)
	if err != nil {
		for _, result := range results {
			_ = result.Body.Close()
		}
		return nil, err
	}
	header := results[0].Header.Clone()
	header.Del("Content-Length")
	return &Result{StatusCode: results[0].StatusCode, Status: results[0].Status, Header: header, Body: &fannedBody{Reader: bytes.NewReader(body), results: results}, Finalize: func(Usage, string, string) {}}, nil
}

// settleFannedImage finalizes an image that is not returned to the client:
// a response with an error status records its own failure, an image the
// client never receives is recorded as interrupted and not billed.
func settleFannedImage(image fannedImage) {
	if image.result == nil {
		return
	}
	errorCode := "stream_interrupted"
	if !auditRequestSucceeded(image.result.StatusCode, "") {
		errorCode = ""
	}
	image.result.Finalize(Usage{}, "", errorCode)
	_ = image.result.Body.Close()
}

// fannedBody serves the merged response and closes every image's body.
type fannedBody struct {
	*bytes.Reader
	results []*Result
}

func (b *fannedBody) Close() error {
	for _, result := range b.results {
		_ = result.Body.Close()
	}
	return nil
}

// mergeImageBodies joins single-image responses: data in order, usage with
// the first request's input and every image's output.
func mergeImageBodies(bodies [][]byte) ([]byte, error) {
	var merged map[string]any
	if err := json.Unmarshal(bodies[0], &merged); err != nil {
		return nil, err
	}
	data, _ := merged["data"].([]any)
	usage, _ := merged["usage"].(map[string]any)
	for _, body := range bodies[1:] {
		var next struct {
			Data  []any          `json:"data"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(body, &next); err != nil {
			return nil, err
		}
		data = append(data, next.Data...)
		if usage != nil && next.Usage != nil {
			addImageOutputUsage(usage, next.Usage)
		}
	}
	merged["data"] = data
	return json.Marshal(merged)
}

func addImageOutputUsage(usage, next map[string]any) {
	number := func(values map[string]any, key string) float64 {
		value, _ := values[key].(float64)
		return value
	}
	usage["output_tokens"] = number(usage, "output_tokens") + number(next, "output_tokens")
	usage["total_tokens"] = number(usage, "input_tokens") + number(usage, "output_tokens")
	details, _ := usage["output_tokens_details"].(map[string]any)
	nextDetails, _ := next["output_tokens_details"].(map[string]any)
	for key, value := range nextDetails {
		if amount, ok := value.(float64); ok && details != nil {
			details[key] = number(details, key) + amount
		}
	}
}
