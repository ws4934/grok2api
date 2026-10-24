package muse

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
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

// NormalizeVideoRequest: the agent renders 720p and picks the final length
// itself; the requested duration and shape go into the prompt.
func (a *Adapter) NormalizeVideoRequest(options provider.VideoRequestOptions) (provider.VideoRequestOptions, error) {
	if options.Operation != provider.VideoOperationGenerate {
		return options, nil
	}
	options.AspectRatio = cmp.Or(options.AspectRatio, "16:9")
	options.Resolution = "720p"
	options.Duration = cmp.Or(options.Duration, 5)
	return options, nil
}

func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	switch {
	case options.Operation != provider.VideoOperationGenerate:
		return provider.ErrVideoOperationUnsupported
	case options.ReferenceImages > 0 || options.ReferenceAudios > 0:
		return fmt.Errorf("%w: Muse 视频只支持首帧图片（image）", provider.ErrVideoParameterInvalid)
	case options.Duration < 1 || options.Duration > 15:
		return fmt.Errorf("%w: duration 必须在 1 到 15 秒之间", provider.ErrVideoParameterInvalid)
	case !slices.Contains([]string{"16:9", "9:16", "1:1", "4:3", "3:4"}, options.AspectRatio):
		return fmt.Errorf("%w: aspect_ratio 必须是 16:9、9:16、1:1、4:3 或 3:4", provider.ErrVideoParameterInvalid)
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
	if job.Thread == "" {
		if request.ImageURL != "" {
			input, err := a.readInput(ctx, request.ImageURL)
			if err != nil {
				return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
			}
			job.Inputs = []attachment{input}
		}
		if err := provider.SaveVideoCheckpoint(request, videoCheckpoint{Submitting: true}); err != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
		}
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
		if job.Thread == "" {
			// No side chat was opened, so nothing was spent: the job can
			// start over, on another account if the gateway picks one.
			if request.SaveCheckpoint != nil {
				_ = request.SaveCheckpoint("")
			}
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageCreate, 0, err)
		}
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePoll, 0, err)
	}
	asset, err := a.assets.SaveVideo(ctx, request.JobID, cmp.Or(file.MIME, "video/mp4"), bytes.NewReader(file.Data))
	if err != nil {
		return provider.VideoResult{}, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	return provider.VideoResult{AssetID: asset.ID, ContentType: asset.MIMEType}, nil
}

// videoPrompt asks for a new video, or one that starts from the attached frame.
func videoPrompt(request provider.VideoRequest) string {
	shape := map[string]string{"16:9": "16:9 landscape", "9:16": "9:16 portrait", "1:1": "1:1 square", "4:3": "4:3", "3:4": "3:4 portrait"}[request.AspectRatio]
	specs := " Aspect ratio: " + cmp.Or(shape, "16:9 landscape") + "."
	if request.Duration > 0 {
		specs += " Duration: " + strconv.Itoa(request.Duration) + " seconds."
	}
	lead := "Create a brand-new video from scratch based only on the description below (text to video; do not use earlier images)."
	if request.ImageURL != "" {
		lead = "Create a video that starts from the image attached to this message as its first frame and follows the description below. Use only the attached image."
	}
	return lead + specs + " Reply with the video.\n\n" + request.Prompt
}
