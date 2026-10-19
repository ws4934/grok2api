package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// imageKeepAlive spaces the SSE comments sent while an image is drawn.
var imageKeepAlive = 15 * time.Second

// imageJob is one Images API request.
type imageJob struct {
	model          string
	edit           bool
	prompt         string
	count          int
	size           string
	quality        string
	responseFormat string
	outputFormat   string
	background     string
	inputs         []string
	mask           string
	streaming      bool
	partialImages  int
	aspectRatio    string
	resolution     string
}

func (j imageJob) options() provider.ImageRequestOptions {
	return provider.ImageRequestOptions{
		Edit: j.edit, Count: j.count, InputImages: len(j.inputs), HasMask: j.mask != "", Size: j.size, AspectRatio: j.aspectRatio,
		Resolution: j.resolution, Quality: j.quality, ResponseFormat: j.responseFormat, OutputFormat: j.outputFormat,
		Background: j.background, Streaming: j.streaming, PartialImages: j.partialImages,
	}
}

// imageOutput is one archived image.
type imageOutput struct {
	data          []byte
	asset         media.Asset
	width, height int
}

func (a *Adapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	return a.images(ctx, request.Credential, imageJob{
		model: request.Model, prompt: request.Prompt, count: request.Count, size: request.Size, quality: request.Quality, responseFormat: request.ResponseFormat,
		outputFormat: request.OutputFormat, background: request.Background, streaming: request.Streaming, partialImages: request.PartialImages,
		aspectRatio: request.AspectRatio, resolution: request.Resolution,
	})
}

func (a *Adapter) EditImage(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	return a.images(ctx, request.Credential, imageJob{
		model: request.Model, edit: true, prompt: request.Prompt, count: request.Count, size: request.Size, quality: request.Quality, responseFormat: request.ResponseFormat,
		outputFormat: request.OutputFormat, background: request.Background, inputs: request.ImageURLs, mask: request.MaskURL,
		streaming: request.Streaming, partialImages: request.PartialImages, aspectRatio: request.AspectRatio, resolution: request.Resolution,
	})
}

// ValidateImageRequest applies the ChatGPT Web image limits. partial_images
// is accepted and ignored: the Web tool sends no intermediate frames.
func (a *Adapter) ValidateImageRequest(o provider.ImageRequestOptions) error {
	switch {
	case o.Count < 0 || o.Count > 10:
		return invalidRequest("n must be between 1 and 10")
	case o.AspectRatio != "" || o.Resolution != "":
		return invalidRequest("aspect_ratio and resolution are not supported; use size")
	case !validImageSize(o.Size):
		return invalidRequest("size must be auto or WIDTHxHEIGHT between 256 and 4096")
	case !slices.Contains([]string{"", "auto", "low", "medium", "high"}, o.Quality):
		return invalidRequest("quality must be auto, low, medium or high")
	case o.ResponseFormat != "" && o.ResponseFormat != "url" && o.ResponseFormat != "b64_json":
		return invalidRequest("response_format must be url or b64_json")
	case o.PartialImages < 0 || o.PartialImages > 3:
		return invalidRequest("partial_images must be between 0 and 3")
	case o.Edit && o.InputImages == 0:
		return invalidRequest("image editing requires an input image")
	case o.InputImages > 8:
		return invalidRequest("at most 8 input images are supported")
	case o.HasMask && (!o.Edit || o.InputImages != 1):
		return invalidRequest("a mask requires exactly one input image")
	}
	if err := (ImageOutputOptions{OutputFormat: o.OutputFormat, Background: o.Background}).Validate(); err != nil {
		return invalidRequest(err.Error())
	}
	return nil
}

func validImageSize(size string) bool {
	if size == "" || size == "auto" {
		return true
	}
	var w, h int
	_, err := fmt.Sscanf(size, "%dx%d", &w, &h)
	return err == nil && fmt.Sprintf("%dx%d", w, h) == size && w >= 256 && h >= 256 && w <= 4096 && h <= 4096
}

// images runs each image synchronously: prepare, submit, poll, fetch, archive.
// A failure before submission is a Response so the gateway may fail over;
// after submission it is an error so the gateway never resubmits.
func (a *Adapter) images(ctx context.Context, credential account.Credential, job imageJob) (*provider.Response, error) {
	if err := a.ValidateImageRequest(job.options()); err != nil {
		return errorResponse("", http.StatusBadRequest, "invalid_request", err.Error()), nil
	}
	model, err := a.resolveImageModel(ctx, credential, job.model)
	if err != nil {
		return failureResponse("", err)
	}
	if a.assets == nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, errors.New("image storage is not configured"))
	}
	if job.count == 0 {
		job.count = 1
	}
	if job.responseFormat == "" {
		job.responseFormat = "b64_json"
	}
	c, err := a.decrypt(credential)
	if err != nil {
		return nil, err
	}
	ctx = a.WithImageJobScope(ctx)
	if job.streaming {
		return a.streamImages(ctx, credential, c, job, model)
	}
	var outputs []imageOutput
	for len(outputs) < job.count {
		output, submitted, err := a.generateImage(ctx, credential, c, job, model, nil)
		if err != nil {
			if len(outputs) > 0 {
				break // ponytail: deliver the images already drawn instead of failing them all
			}
			return imageFailure(err, submitted)
		}
		outputs = append(outputs, output)
	}
	data, err := json.Marshal(a.imageBody(job, outputs))
	if err != nil {
		return nil, err
	}
	response := jsonResponse(http.StatusOK, data)
	response.QuotaUnits = len(outputs)
	return response, nil
}

func imageFailure(err error, submitted bool) (*provider.Response, error) {
	var upstream *upstreamError
	if submitted || (errors.As(err, &upstream) && upstream.Status >= 500) {
		return nil, err
	}
	return failureResponse("", err)
}

func (a *Adapter) streamImages(ctx context.Context, credential account.Credential, c credentials, job imageJob, model string) (*provider.Response, error) {
	reader, writer := io.Pipe()
	out := &sseWriter{w: writer}
	runCtx, cancel := context.WithCancel(ctx)
	started := make(chan error, 1)
	event := "image_generation.completed"
	if job.edit {
		event = "image_edit.completed"
	}
	go func() {
		defer cancel()
		defer writer.Close()
		var stopKeepAlive func()
		var err error
		for delivered := 0; delivered < job.count; delivered++ {
			var output imageOutput
			output, _, err = a.generateImage(runCtx, credential, c, job, model, func() {
				if stopKeepAlive == nil {
					started <- nil
					stopKeepAlive = out.keepAlive(imageKeepAlive)
				}
			})
			if err == nil {
				err = out.send(event, a.imageEvent(job, event, output))
			}
			if err != nil {
				break
			}
		}
		if stopKeepAlive == nil {
			started <- err
			return
		}
		stopKeepAlive()
		if err != nil && runCtx.Err() == nil {
			code, message := publicFailure(err)
			_ = out.send("error", map[string]any{"type": "error", "error": map[string]any{"type": "server_error", "code": code, "message": message, "param": nil}})
		}
	}()
	if err := <-started; err != nil {
		return imageFailure(err, false)
	}
	header := http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: &streamBody{PipeReader: reader, cancel: cancel}, QuotaUnits: job.count}, nil
}

// generateImage draws one image. Uploads made before a retried egress failure
// are reused; the generation request itself is sent at most once.
func (a *Adapter) generateImage(ctx context.Context, credential account.Credential, c credentials, job imageJob, model string, submitted func()) (imageOutput, bool, error) {
	if _, ok := codexImageEngine(job.model); ok {
		return a.generateCodexImage(ctx, credential, c, job, model, submitted)
	}
	var inputs []OpenAIImageInput
	var mask *OpenAIImageInput
	prepared := false
	uploads := map[int]openAIImageReference{}
	var download downloadedImage
	var onSubmit func() error
	if submitted != nil {
		onSubmit = func() error { submitted(); return nil }
	}
	sent, err := a.attempt(ctx, credential, onSubmit, func(ctx context.Context) error {
		var err error
		if !prepared {
			if inputs, mask, err = a.prepareImage(ctx, job); err != nil {
				return err
			}
			prepared = true
		}
		w, err := wire(ctx, c)
		if err != nil {
			return err
		}
		ctx = a.image.withTimezone(ctx, w)
		started := time.Now()
		id, files, err := a.submitDrawing(ctx, w, job, model, inputs, mask, uploads)
		a.logImageStage(ctx, credential, "submit", started, err, "conversation_id", id, "image_refs", len(files), "conversation_model", model)
		if err != nil {
			return err
		}
		// The generation is sent and costs quota: finish it even if the client
		// went away, so the image still lands in the gallery.
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.image.RequestTimeout)
		defer cancel()
		started = time.Now()
		download, err = a.finishDrawing(finishCtx, w, id, files)
		a.logImageStage(ctx, credential, "result", started, err, "conversation_id", id, "bytes", len(download.raw), "mime", download.mime)
		return err
	})
	if err != nil {
		return imageOutput{}, sent, err
	}
	output, err := a.archiveImage(ctx, job, download)
	return output, true, err
}

// archiveImage applies the requested output format and background, then
// stores the image.
func (a *Adapter) archiveImage(ctx context.Context, job imageJob, download downloadedImage) (imageOutput, error) {
	raw, _, err := (ImageOutputOptions{OutputFormat: job.outputFormat, Background: job.background}).applyDecoded(download)
	if err != nil {
		return imageOutput{}, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err)
	}
	asset, err := a.assets.SaveImage(context.WithoutCancel(ctx), raw)
	if err != nil {
		return imageOutput{}, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	output := imageOutput{data: raw, asset: asset}
	if config, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		output.width, output.height = config.Width, config.Height
	}
	return output, nil
}

type preparedImageKey struct{}

// preparedImage holds the inputs and mask loaded for the images of one request
// or job, so later images neither fetch nor decode them again.
type preparedImage struct {
	mu     sync.Mutex
	done   bool
	inputs []OpenAIImageInput
	mask   *OpenAIImageInput
}

// WithImageJobScope makes the images drawn under ctx (one job) load their
// inputs and mask once.
func (a *Adapter) WithImageJobScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, preparedImageKey{}, &preparedImage{})
}

// prepareImage loads the inputs and converts the mask before anything is sent.
func (a *Adapter) prepareImage(ctx context.Context, job imageJob) ([]OpenAIImageInput, *OpenAIImageInput, error) {
	scope, _ := ctx.Value(preparedImageKey{}).(*preparedImage)
	if scope == nil {
		scope = &preparedImage{}
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.done {
		return scope.inputs, scope.mask, nil
	}
	var inputs []OpenAIImageInput
	for _, value := range job.inputs {
		input, err := a.loadInput(ctx, value)
		if err != nil {
			return nil, nil, err
		}
		inputs = append(inputs, input)
	}
	var mask *OpenAIImageInput
	if job.mask != "" {
		b, err := a.loadInput(ctx, job.mask)
		if err != nil {
			return nil, nil, err
		}
		selection, err := ParseImageMask(b.Data, inputs)
		if err != nil {
			return nil, nil, invalidRequest(err.Error())
		}
		mask = &OpenAIImageInput{Name: "mask.png", MIME: "image/png", Data: selection.png}
	}
	scope.inputs, scope.mask, scope.done = inputs, mask, true
	return inputs, mask, nil
}

// submitDrawing uploads the inputs and submits the image conversation; it
// returns the conversation ID and any image references the stream carried.
func (a *Adapter) submitDrawing(ctx context.Context, w wireAccount, job imageJob, model string, inputs []OpenAIImageInput, mask *OpenAIImageInput, uploads map[int]openAIImageReference) (string, []string, error) {
	upload := func(key int, input OpenAIImageInput) (openAIImageReference, error) {
		if ref, ok := uploads[key]; ok {
			return ref, nil
		}
		index := key
		if key < 0 {
			index = len(inputs)
		}
		ref, err := a.image.uploadInputWithPurpose(ctx, w, input, index+1, key < 0)
		if err == nil {
			uploads[key] = ref
		}
		return ref, err
	}
	var refs, masks []openAIImageReference
	for index, input := range inputs {
		ref, err := upload(index, input)
		if err != nil {
			return "", nil, err
		}
		refs = append(refs, ref)
	}
	if mask != nil {
		ref, err := upload(-1, *mask)
		if err != nil {
			return "", nil, err
		}
		masks = append(masks, ref)
	}
	scripts, build, err := a.image.bootstrap(ctx, w)
	if err != nil {
		return "", nil, err
	}
	requirements, err := a.image.chatRequirements(ctx, w, scripts, build)
	if err != nil {
		return "", nil, err
	}
	prompt := job.prompt + (ImageOutputOptions{OutputFormat: job.outputFormat, Background: job.background, Size: job.size}).promptSuffix()
	conduit, err := a.image.prepare(ctx, w, requirements, prompt, model)
	if err != nil {
		return "", nil, err
	}
	quality := job.quality
	if quality == "" {
		quality = "auto"
	}
	id, files, err := a.image.start(ctx, w, requirements, conduit, prompt, model, job.size, quality, refs, masks...)
	if err != nil {
		return "", nil, err
	}
	if id == "" {
		return "", nil, errors.New("upstream submission has no recoverable conversation ID")
	}
	return id, files, nil
}

// finishDrawing waits for the generated image and downloads the first one.
func (a *Adapter) finishDrawing(ctx context.Context, w wireAccount, id string, files []string) (downloadedImage, error) {
	if len(files) == 0 {
		var err error
		if files, err = a.image.pollConversation(ctx, w, id); err != nil {
			return downloadedImage{}, err
		}
	}
	return a.image.downloadImageRef(ctx, w, id, files[0])
}

var _ provider.AsyncImageAdapter = (*Adapter)(nil)

func asyncImageJob(request provider.ImageJobRequest) imageJob {
	return imageJob{
		model: request.Model, edit: len(request.ImageURLs) > 0, prompt: request.Prompt, count: 1, size: request.Size, quality: request.Quality,
		outputFormat: request.OutputFormat, background: request.Background, inputs: request.ImageURLs, mask: request.MaskURL,
	}
}

// SubmitImage runs the sync path's steps up to the generation request for a
// durable image job. A request the upstream refused with a 4xx before it
// started generating is reported unsubmitted, so another account may retry.
func (a *Adapter) SubmitImage(ctx context.Context, request provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
	var cp provider.ImageCheckpoint
	if _, ok := codexImageEngine(request.Model); ok {
		// The Codex Images API answers with the image itself; there is no
		// upstream job to resume.
		return cp, invalidRequest("Codex image models do not support image jobs; use the Images API")
	}
	job := asyncImageJob(request)
	if err := a.ValidateImageRequest(job.options()); err != nil {
		return cp, err
	}
	model, err := a.resolveImageModel(ctx, request.Credential, job.model)
	if err != nil {
		return cp, err
	}
	c, err := a.decrypt(request.Credential)
	if err != nil {
		return cp, err
	}
	var inputs []OpenAIImageInput
	var mask *OpenAIImageInput
	prepared := false
	uploads := map[int]openAIImageReference{}
	_, err = a.attempt(ctx, request.Credential, func() error {
		next := provider.ImageCheckpoint{Submitted: true}
		if err := save(next); err != nil {
			return err
		}
		cp = next
		return nil
	}, func(ctx context.Context) error {
		var err error
		if !prepared {
			if inputs, mask, err = a.prepareImage(ctx, job); err != nil {
				return err
			}
			prepared = true
		}
		w, err := wire(ctx, c)
		if err != nil {
			return err
		}
		ctx = withConversationObserver(a.image.withTimezone(ctx, w), func(id string) error {
			cp.UpstreamID = id
			return save(cp)
		})
		id, files, err := a.submitDrawing(ctx, w, job, model, inputs, mask, uploads)
		if err == nil {
			cp.UpstreamID, cp.Outputs = id, files
		}
		return err
	})
	var upstream *upstreamError
	var terminal *imageTerminalError
	if err != nil && cp.UpstreamID == "" && errors.As(err, &upstream) && !errors.As(err, &terminal) && upstream.Status >= 400 && upstream.Status < 500 {
		cp.Submitted = false
	}
	return cp, err
}

// PollImage waits for the submitted conversation to produce its image.
func (a *Adapter) PollImage(ctx context.Context, request provider.ImageJobRequest, cp provider.ImageCheckpoint) (provider.ImageCheckpoint, error) {
	if len(cp.Outputs) > 0 {
		return cp, nil
	}
	if cp.UpstreamID == "" {
		return cp, errors.New("image checkpoint has no conversation ID")
	}
	c, err := a.decrypt(request.Credential)
	if err != nil {
		return cp, err
	}
	_, err = a.attempt(ctx, request.Credential, nil, func(ctx context.Context) error {
		w, err := wire(ctx, c)
		if err != nil {
			return err
		}
		cp.Outputs, err = a.image.pollConversation(ctx, w, cp.UpstreamID)
		return err
	})
	return cp, err
}

// FetchImage downloads the first generated image and applies the output options.
func (a *Adapter) FetchImage(ctx context.Context, request provider.ImageJobRequest, cp provider.ImageCheckpoint) ([]byte, error) {
	if len(cp.Outputs) == 0 {
		return nil, errors.New("image checkpoint has no output")
	}
	c, err := a.decrypt(request.Credential)
	if err != nil {
		return nil, err
	}
	var download downloadedImage
	_, err = a.attempt(ctx, request.Credential, nil, func(ctx context.Context) error {
		w, err := wire(ctx, c)
		if err != nil {
			return err
		}
		download, err = a.image.downloadImageRef(ctx, w, cp.UpstreamID, cp.Outputs[0])
		return err
	})
	if err != nil {
		return nil, err
	}
	raw, _, err := (ImageOutputOptions{OutputFormat: request.OutputFormat, Background: request.Background}).applyDecoded(download)
	return raw, err
}

// ImageUsage estimates official usage for images delivered by a job.
func (a *Adapter) ImageUsage(prompt string, inputImages int, quality string, outputs []image.Point) map[string]any {
	return a.usage(prompt, inputImages, "", quality, outputs)
}

func (a *Adapter) imageItem(job imageJob, output imageOutput) map[string]any {
	if job.responseFormat == "url" {
		return map[string]any{"url": a.assets.PublicImageURL(output.asset.ID)}
	}
	return map[string]any{"b64_json": base64.StdEncoding.EncodeToString(output.data)}
}

// imageBody renders the OpenAI Images response.
func (a *Adapter) imageBody(job imageJob, outputs []imageOutput) map[string]any {
	data := make([]any, 0, len(outputs))
	for _, output := range outputs {
		data = append(data, a.imageItem(job, output))
	}
	body := job.described(outputs[0])
	body["created"], body["data"], body["usage"] = time.Now().Unix(), data, a.imageUsage(job, outputs)
	return body
}

func (a *Adapter) imageEvent(job imageJob, event string, output imageOutput) map[string]any {
	body := job.described(output)
	for key, value := range a.imageItem(job, output) {
		body[key] = value
	}
	body["type"], body["created_at"], body["usage"] = event, time.Now().Unix(), a.imageUsage(job, []imageOutput{output})
	return body
}

// described reports the stored size and the effective output options.
func (j imageJob) described(output imageOutput) map[string]any {
	size, quality, format, background := j.size, j.quality, j.outputFormat, j.background
	if output.width > 0 && output.height > 0 {
		size = fmt.Sprintf("%dx%d", output.width, output.height)
	}
	if quality == "" {
		quality = "auto"
	}
	if format == "" {
		format = "png"
	}
	if background == "" {
		background = "auto"
	}
	return map[string]any{"size": size, "quality": quality, "output_format": format, "background": background}
}

// imageUsage estimates official usage, which the upstream does not report:
// the prompt plus a fixed estimate per input image, and the gpt-image patch
// formula on the stored output sizes.
func (a *Adapter) imageUsage(job imageJob, outputs []imageOutput) map[string]any {
	sizes := make([]image.Point, 0, len(outputs))
	for _, item := range outputs {
		sizes = append(sizes, image.Pt(item.width, item.height))
	}
	return a.usage(job.prompt, len(job.inputs), job.size, job.quality, sizes)
}

func (a *Adapter) usage(prompt string, inputImages int, size, quality string, outputs []image.Point) map[string]any {
	text, _ := countTokens(prompt)
	images := int64(inputImages) * int64(a.config().VisionTokens)
	var output int64
	for _, item := range outputs {
		output += outputImageTokens(item.X, item.Y, size, quality)
	}
	input := text + images
	return map[string]any{
		"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
		"input_tokens_details":  map[string]any{"text_tokens": text, "image_tokens": images, "cached_tokens": 0},
		"output_tokens_details": map[string]any{"text_tokens": 0, "image_tokens": output, "reasoning_tokens": 0},
	}
}

// outputImageTokens: ceil(w/32)*ceil(h/32) patches, times 17/64 for low,
// 65/16 for high and 33/32 otherwise; unknown sizes count as 1024x1024.
func outputImageTokens(w, h int, size, quality string) int64 {
	if w <= 0 || h <= 0 {
		w, h = 1024, 1024
		_, _ = fmt.Sscanf(size, "%dx%d", &w, &h)
	}
	p := int64(max(w, 0)+31) / 32 * (int64(max(h, 0)+31) / 32)
	switch quality {
	case "low":
		return (p*17 + 63) / 64
	case "high":
		return (p*65 + 15) / 16
	}
	return (p*33 + 31) / 32
}

// sseWriter serializes events and keep-alive comments onto one stream.
type sseWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *sseWriter) write(frame string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := io.WriteString(s.w, frame)
	return err
}

func (s *sseWriter) send(event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.write("event: " + event + "\ndata: " + string(data) + "\n\n")
}

// keepAlive writes SSE comments until stopped, so idle proxies keep the stream open.
func (s *sseWriter) keepAlive(every time.Duration) func() {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if s.write(": keep-alive\n\n") != nil {
					return
				}
			}
		}
	}()
	return func() { close(done); <-finished }
}

// logImageStage records one image step so failures show where they happened
// and whether the caller's context was already canceled.
func (a *Adapter) logImageStage(ctx context.Context, credential account.Credential, stage string, started time.Time, err error, attrs ...any) {
	attrs = append(attrs, "stage", stage, "account_id", credential.ID, "duration_ms", time.Since(started).Milliseconds())
	if err != nil {
		attrs = append(attrs, "error", err.Error(), "context_error", fmt.Sprint(ctx.Err()))
		a.log().Warn("chatgpt_image_stage_failed", attrs...)
		return
	}
	a.log().Info("chatgpt_image_stage", attrs...)
}
