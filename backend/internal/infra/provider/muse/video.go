package muse

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// videoCheckpoint records the side chat of a sent video prompt, so a restart
// waits in that thread instead of spending usage on a second video.
type videoCheckpoint struct {
	Thread     string `json:"thread,omitempty"`
	Submitting bool   `json:"submitting,omitempty"`
}

// museVideoRatios are the shapes the agent renders.
var museVideoRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4"}

// NormalizeVideoRequest: the agent renders 720p and picks the final length
// itself; the requested duration and shape go into the prompt. Other values
// map to the closest it takes: a ratio it lacks, a duration outside 1–15 s,
// and a first frame plus references beyond 4 images keep the first ones;
// reference audios are dropped.
func (a *Adapter) NormalizeVideoRequest(options provider.VideoRequestOptions) (provider.VideoRequestOptions, error) {
	if options.Operation != provider.VideoOperationGenerate {
		return options, nil
	}
	options.AspectRatio = cmp.Or(provider.ClosestImageRatio(options.AspectRatio, museVideoRatios), "16:9")
	options.Resolution = "720p"
	options.Duration = min(max(cmp.Or(options.Duration, 5), 1), 15)
	limit := maxInputImages
	if options.HasImage {
		limit--
	}
	options.ReferenceImages, options.ReferenceAudios = min(options.ReferenceImages, limit), 0
	return options, nil
}

func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	if options.Operation != provider.VideoOperationGenerate {
		return provider.ErrVideoOperationUnsupported
	}
	return nil
}

func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	var checkpoint videoCheckpoint
	if strings.TrimSpace(request.Checkpoint) != "" {
		if err := json.Unmarshal([]byte(request.Checkpoint), &checkpoint); err != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
		}
	}
	if checkpoint.Submitting && checkpoint.Thread == "" {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageSubmitted, 0, fmt.Errorf("Muse 视频提示已发送但未记录会话，无法安全恢复"))
	}
	job := mediaJob{Kind: mediaVideo, Prompt: videoPrompt(request), Thread: checkpoint.Thread}
	sent := false
	if job.Thread == "" {
		// The first frame goes first: the prompt names it as the first image.
		refs := request.ReferenceURLs
		if request.ImageURL != "" {
			refs = append([]string{request.ImageURL}, refs...)
		}
		for _, ref := range refs {
			input, err := a.readInput(ctx, ref)
			if err != nil {
				return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
			}
			job.Inputs = append(job.Inputs, input)
		}
		if err := provider.SaveVideoCheckpoint(request, videoCheckpoint{Submitting: true}); err != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
		}
		job.OnSent = func() { sent = true }
		job.OnThread = func(path string) error {
			job.Thread = path
			return provider.SaveVideoCheckpoint(request, videoCheckpoint{Thread: path})
		}
	}
	var file mediaFile
	err := a.withSession(ctx, request.Credential, func(ctx context.Context, s session) error {
		var err error
		file, err = a.engine.Generate(ctx, s, job)
		return err
	})
	if err != nil {
		err = classify(err)
		switch {
		case job.Thread == "" && !sent:
			// The prompt never left the composer, so nothing was spent: the
			// job can start over, on another account if the gateway picks one.
			if request.SaveCheckpoint != nil {
				_ = request.SaveCheckpoint("")
			}
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageCreate, 0, err)
		case job.Thread == "":
			// Sent but the thread is unknown: it cannot be resumed and must
			// not be sent again. The checkpoint stays "submitting".
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageSubmitted, 0, err)
		}
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePoll, 0, err)
	}
	asset, err := a.assets.SaveVideo(ctx, request.JobID, cmp.Or(file.MIME, "video/mp4"), bytes.NewReader(file.Data))
	if err != nil {
		return provider.VideoResult{}, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	return provider.VideoResult{AssetID: asset.ID, ContentType: asset.MIMEType}, nil
}

// videoPrompt asks for a new video, one that starts from the attached frame,
// or one guided by reference images.
func videoPrompt(request provider.VideoRequest) string {
	shape := map[string]string{"16:9": "16:9 landscape", "9:16": "9:16 portrait", "1:1": "1:1 square", "4:3": "4:3", "3:4": "3:4 portrait"}[request.AspectRatio]
	specs := " Aspect ratio: " + cmp.Or(shape, "16:9 landscape") + "."
	if request.Duration > 0 {
		specs += " Duration: " + strconv.Itoa(request.Duration) + " seconds."
	}
	lead := "Create a brand-new video from scratch based only on the description below (text to video; do not use earlier images)."
	switch {
	case request.ImageURL != "" && len(request.ReferenceURLs) > 0:
		lead = "Create a video that starts from the first image attached to this message as its first frame, uses the other attached images as references for the subjects, scene and style, and follows the description below. Use only the attached images."
	case request.ImageURL != "":
		lead = "Create a video that starts from the image attached to this message as its first frame and follows the description below. Use only the attached image."
	case len(request.ReferenceURLs) > 0:
		lead = "Create a video that uses the images attached to this message as references for the subjects, scene and style and follows the description below. Use only the attached images."
	}
	return lead + specs + " Reply with the video.\n\n" + request.Prompt
}
