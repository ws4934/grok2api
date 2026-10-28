package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
)

// errImageAccountsBusy reports that an image of a fanned-out request found no
// free account; a later round draws it.
var errImageAccountsBusy = errors.New("no free account for an extra image")

// Match the public JSON transfer limit, including native multi-image batches.
const maxFannedImageBody = 128 << 20

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
	result    *Result
	body      []byte
	err       error
	submitted int
	outputs   int
}

// drawImageBatches preserves parallel single-image products and native batches.
// Each native draw fits its selected account's budget. Only unsubmitted work
// advances to another round; partial/uncertain batches stop without replay.
// Already generated results survive a later failure or exhausted account pool.
func (s *Service) drawImageBatches(ctx context.Context, count int, parallel bool, draw func(ctx context.Context, count int, extra bool) (*Result, error)) (*Result, error) {
	var delivered, failed []fannedImage
	submitted := 0
	stop := false
	for extra := parallel; submitted < count && len(failed) == 0 && !stop; {
		outcomes := make([]fannedImage, count-submitted)
		batchCount := count - submitted
		if parallel {
			batchCount = 1
		}
		if !parallel || !extra {
			outcomes = outcomes[:1]
		}
		var wait sync.WaitGroup
		for index := range outcomes {
			wait.Go(func() {
				outcome := &outcomes[index]
				outcome.result, outcome.err = draw(ctx, batchCount, extra)
				if outcome.err == nil && auditRequestSucceeded(outcome.result.StatusCode, "") {
					outcome.body, outcome.err = io.ReadAll(io.LimitReader(outcome.result.Body, maxFannedImageBody+1))
					if outcome.err == nil && len(outcome.body) > maxFannedImageBody {
						outcome.err = invalidImageResult{}
					}
					if outcome.err == nil {
						outcome.submitted = outcome.result.imageBatchSize
						outcome.outputs = outcome.result.imageOutputCount()
						if outcome.outputs == 0 {
							outcome.err = invalidImageResult{}
						}
					}
					if outcome.err == nil {
						// Free the account and commit upstream spend before the next
						// round, but settle client fees only after response delivery.
						if outcome.result.completeImage != nil {
							outcome.result.completeImage()
						}
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
				submitted += outcome.submitted
				stop = stop || outcome.outputs < outcome.submitted
				extra = parallel
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
	return &Result{
		StatusCode: results[0].StatusCode, Status: results[0].Status, Header: header,
		Body: &fannedBody{Reader: body, results: results},
		Finalize: func(usage Usage, responseID, errorCode string) {
			for _, result := range results {
				result.Finalize(usage, responseID, errorCode)
			}
		},
	}, nil
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
	io.Reader
	results []*Result
}

func (b *fannedBody) Close() error {
	for _, result := range b.results {
		_ = result.Body.Close()
	}
	return nil
}

// mergeImageBodies streams the original encoded image items rather than
// decoding Base64 strings and allocating a second full encoded response.
// Metadata retains exact JSON numbers; usage keeps the original input and
// adds the output tokens of every batch.
func mergeImageBodies(bodies [][]byte) (io.Reader, error) {
	if len(bodies) == 1 {
		return bytes.NewReader(bodies[0]), nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(bodies[0], &merged); err != nil {
		return nil, err
	}
	var usage map[string]json.RawMessage
	_ = json.Unmarshal(merged["usage"], &usage)
	data := []json.RawMessage{merged["data"]}
	delete(merged, "data")
	for _, body := range bodies[1:] {
		var next struct {
			Data  json.RawMessage            `json:"data"`
			Usage map[string]json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(body, &next); err != nil {
			return nil, err
		}
		data = append(data, next.Data)
		if usage != nil && next.Usage != nil {
			addImageOutputUsage(usage, next.Usage)
		}
	}
	if usage != nil {
		merged["usage"], _ = json.Marshal(usage)
	}
	metadata, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	readers := []io.Reader{bytes.NewReader(metadata[:len(metadata)-1])}
	if len(merged) > 0 {
		readers = append(readers, strings.NewReader(","))
	}
	readers = append(readers, strings.NewReader(`"data":[`))
	for index, items := range data {
		items = bytes.TrimSpace(items)
		if index > 0 {
			readers = append(readers, strings.NewReader(","))
		}
		readers = append(readers, bytes.NewReader(items[1:len(items)-1]))
	}
	readers = append(readers, strings.NewReader("]}"))
	return io.MultiReader(readers...), nil
}

func addImageOutputUsage(usage, next map[string]json.RawMessage) {
	number := func(values map[string]json.RawMessage, key string) int64 {
		var value int64
		_ = json.Unmarshal(values[key], &value)
		return value
	}
	outputs := number(usage, "output_tokens") + number(next, "output_tokens")
	usage["output_tokens"], _ = json.Marshal(outputs)
	usage["total_tokens"], _ = json.Marshal(number(usage, "input_tokens") + outputs)
	var details, nextDetails map[string]json.RawMessage
	_ = json.Unmarshal(usage["output_tokens_details"], &details)
	_ = json.Unmarshal(next["output_tokens_details"], &nextDetails)
	for key, value := range nextDetails {
		var amount int64
		if details != nil && json.Unmarshal(value, &amount) == nil {
			details[key], _ = json.Marshal(number(details, key) + amount)
		}
	}
	if details != nil {
		usage["output_tokens_details"], _ = json.Marshal(details)
	}
}
