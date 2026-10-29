package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Media job inputs live on disk as staged input files. A queued or running
// job holds only their references, and an adapter reads a file while it sends
// it upstream (provider.OpenInput), so memory does not grow with the number
// of jobs in flight.

// inputStager stages the inline data URIs of a job being created and
// remembers what it saved, so a job that is not created releases its files.
type inputStager struct {
	service *Service
	staged  []string
}

// stage saves ref as a staged input file when it is a data URI and returns
// the reference that replaces it; URLs and references pass through.
func (st *inputStager) stage(ctx context.Context, ref, kind string) (string, error) {
	mimeType, data, isDataURI, err := decodeDataURI(ref)
	if err != nil || !isDataURI {
		return ref, err
	}
	if st.service.mediaAssets == nil {
		return "", fmt.Errorf("%w: 媒体存储未配置", ErrMediaStorageFull)
	}
	var asset media.Asset
	if kind == "video" {
		asset, err = st.service.mediaAssets.SaveInputVideo(ctx, mimeType, bytes.NewReader(data))
	} else {
		asset, err = st.service.mediaAssets.SaveInputImage(ctx, data)
	}
	switch {
	case errors.Is(err, mediaapp.ErrInvalidImage), errors.Is(err, mediaapp.ErrInvalidVideoUpload):
		return "", fmt.Errorf("%w: %v", ErrMediaInputInvalid, err)
	case errors.Is(err, mediaapp.ErrMediaCapacity):
		return "", fmt.Errorf("%w: %v", ErrMediaStorageFull, err)
	case err != nil:
		return "", err
	}
	reference := media.InputReference(asset.ID)
	st.staged = append(st.staged, reference)
	return reference, nil
}

// stageAll stages each of refs into a new slice.
func (st *inputStager) stageAll(ctx context.Context, refs []string, kind string) ([]string, error) {
	out := slices.Clone(refs)
	for index, ref := range out {
		next, err := st.stage(ctx, ref, kind)
		if err != nil {
			return nil, err
		}
		out[index] = next
	}
	return out, nil
}

func (st *inputStager) stageVideoInputs(ctx context.Context, in videoInputs) (videoInputs, error) {
	var err error
	if in.imageURL, err = st.stage(ctx, in.imageURL, "image"); err != nil {
		return in, err
	}
	if in.lastFrameURL, err = st.stage(ctx, in.lastFrameURL, "image"); err != nil {
		return in, err
	}
	in.keyframes = slices.Clone(in.keyframes)
	for i := range in.keyframes {
		if in.keyframes[i].ImageURL, err = st.stage(ctx, in.keyframes[i].ImageURL, "image"); err != nil {
			return in, err
		}
	}
	if in.referenceURLs, err = st.stageAll(ctx, in.referenceURLs, "image"); err != nil {
		return in, err
	}
	if in.referenceVideoURLs, err = st.stageAll(ctx, in.referenceVideoURLs, "video"); err != nil {
		return in, err
	}
	in.videoURL, err = st.stage(ctx, in.videoURL, "video")
	return in, err
}

// abort releases the files staged for a job that was not created.
func (st *inputStager) abort() {
	st.service.releaseInputs("", st.staged)
}

// decodeDataURI decodes a base64 data URI; isDataURI is false for any other value.
func decodeDataURI(value string) (mimeType string, data []byte, isDataURI bool, err error) {
	value = strings.TrimSpace(value)
	if len(value) < len("data:") || !strings.EqualFold(value[:len("data:")], "data:") {
		return "", nil, false, nil
	}
	head, payload, found := strings.Cut(value[len("data:"):], ",")
	head, isBase64 := strings.CutSuffix(strings.ToLower(head), ";base64")
	if !found || !isBase64 {
		return "", nil, true, fmt.Errorf("%w: data URI 必须使用 base64 编码", ErrMediaInputInvalid)
	}
	if data, err = base64.StdEncoding.DecodeString(payload); err != nil {
		return "", nil, true, fmt.Errorf("%w: data URI 不是有效的 base64", ErrMediaInputInvalid)
	}
	mimeType, _, _ = strings.Cut(head, ";")
	return strings.TrimSpace(mimeType), data, true, nil
}

// openStagedInput opens a staged job input for the adapters (provider.OpenInput).
func (s *Service) openStagedInput(ctx context.Context, reference string) (provider.InputFile, error) {
	fileID, ok := media.ParseInputReference(reference)
	if !ok || s.mediaAssets == nil {
		return provider.InputFile{}, provider.ErrInputUnavailable
	}
	asset, body, err := s.mediaAssets.OpenInputAsset(ctx, fileID)
	if err != nil {
		return provider.InputFile{}, fmt.Errorf("%w: %v", provider.ErrInputUnavailable, err)
	}
	return provider.InputFile{Body: body, MIMEType: asset.MIMEType, Size: asset.SizeBytes}, nil
}

// jobInputReferences lists everything a job's input_json names.
func jobInputReferences(job media.Job) []string {
	if job.Kind == media.KindImage {
		var inputs imageJobInputs
		_ = json.Unmarshal([]byte(job.InputJSON), &inputs)
		return append(slices.Clone(inputs.ImageURLs), inputs.MaskURL)
	}
	return decodeVideoInput(job.InputJSON)
}

// releaseJobInputs deletes the staged inputs of an ended job that no active job
// still names. Uncertain jobs keep theirs so an administrator can retry them.
func (s *Service) releaseJobInputs(job media.Job) {
	s.releaseInputs(job.ID, jobInputReferences(job))
}

func (s *Service) releaseInputs(jobID string, references []string) {
	if s.mediaAssets == nil || len(references) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.mediaAssets.ReleaseInputAssets(ctx, references); err != nil {
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("media_input_release_failed", "job_id", jobID, "error", err)
	}
}
