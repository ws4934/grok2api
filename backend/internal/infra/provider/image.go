package provider

import (
	"context"
	"image"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

type ImageGenerationRequest struct {
	Credential     account.Credential
	Model          string
	Prompt         string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	Quality        string
	ResponseFormat string
	Streaming      bool
	PartialImages  int
	// OutputFormat and Background follow the OpenAI Images API; empty means provider default.
	OutputFormat string
	Background   string
}

type ImageInput struct {
	Filename string
	MIMEType string
	Data     []byte
}

type ImageEditRequest struct {
	Credential     account.Credential
	Model          string
	Prompt         string
	ImageURLs      []string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	Quality        string
	ResponseFormat string
	Streaming      bool
	PartialImages  int
	OutputFormat   string
	Background     string
	// MaskURL is a data URI or https URL of the edit mask; empty means no mask.
	MaskURL string
}

// ImageGenerationAdapter defines an optional Provider image-generation capability.
type ImageGenerationAdapter interface {
	Adapter
	GenerateImage(ctx context.Context, request ImageGenerationRequest) (*Response, error)
}

// ImageEditAdapter defines an optional Provider image-editing capability.
type ImageEditAdapter interface {
	Adapter
	EditImage(ctx context.Context, request ImageEditRequest) (*Response, error)
}

// ImageJobRequest is one image of a durable image job. A request with
// ImageURLs is an edit; images and the mask are data URIs or https URLs.
type ImageJobRequest struct {
	Credential   account.Credential
	Model        string
	Prompt       string
	ImageURLs    []string
	MaskURL      string
	Size         string
	Quality      string
	OutputFormat string
	Background   string
}

// ImageCheckpoint is the persisted progress of one image. Submitted is saved
// before the irreversible request is sent and UpstreamID once the upstream
// accepted it; Submitted without UpstreamID means the outcome is unknown.
type ImageCheckpoint struct {
	Submitted  bool     `json:"submitted,omitempty"`
	UpstreamID string   `json:"upstream_id,omitempty"`
	Outputs    []string `json:"outputs,omitempty"`
}

// AsyncImageAdapter draws one image in resumable steps for durable image
// jobs. SubmitImage calls save right before the irreversible send and once
// the upstream ID is known; a failure while Submitted is unset never reached
// the upstream, so another account may retry it.
type AsyncImageAdapter interface {
	Adapter
	SubmitImage(ctx context.Context, request ImageJobRequest, save func(ImageCheckpoint) error) (ImageCheckpoint, error)
	PollImage(ctx context.Context, request ImageJobRequest, checkpoint ImageCheckpoint) (ImageCheckpoint, error)
	// FetchImage returns the finished image with the output options applied.
	FetchImage(ctx context.Context, request ImageJobRequest, checkpoint ImageCheckpoint) ([]byte, error)
	// ImageUsage estimates the official usage object of delivered images with the given pixel sizes.
	ImageUsage(prompt string, inputImages int, quality string, outputs []image.Point) map[string]any
}

// ImageRequestOptions are the client image parameters checked against a
// provider before routing; empty strings mean "not specified".
type ImageRequestOptions struct {
	Edit           bool
	Count          int
	InputImages    int
	HasMask        bool
	Size           string
	AspectRatio    string
	Resolution     string
	Quality        string
	ResponseFormat string
	OutputFormat   string
	Background     string
	Streaming      bool
	PartialImages  int
}

// ImageRequestValidator is implemented by image providers that refuse some
// requests (counts, inputs, masks); shape and quality values are adapted, not
// refused. The gateway drops routes whose provider rejects the request and
// returns the first rejection (a RequestScopedError-style 400) when none
// remain; providers without it accept every request.
type ImageRequestValidator interface {
	Adapter
	ValidateImageRequest(options ImageRequestOptions) error
}

// ImageAssetStore archives generated images as local resources that the backend can read reliably.
type ImageAssetStore interface {
	SaveImage(ctx context.Context, data []byte) (media.Asset, error)
	PublicImageURL(id string) string
}

func (r *Registry) ImageRequestValidator(value account.Provider) (ImageRequestValidator, bool) {
	return adapterAs[ImageRequestValidator](r, value)
}

// ImageGeneration returns the image-generation capability registered by the Provider.
func (r *Registry) ImageGeneration(value account.Provider) (ImageGenerationAdapter, bool) {
	return adapterAs[ImageGenerationAdapter](r, value)
}

// ImageEdit returns the image-editing capability registered by the Provider.
func (r *Registry) ImageEdit(value account.Provider) (ImageEditAdapter, bool) {
	return adapterAs[ImageEditAdapter](r, value)
}

// AsyncImages returns the resumable image capability used by image jobs.
// AsyncImageModel reports whether the provider draws upstreamModel as a
// resumable image job; providers may exclude models answered synchronously.
func (r *Registry) AsyncImageModel(value account.Provider, upstreamModel string) bool {
	adapter, ok := r.AsyncImages(value)
	if !ok {
		return false
	}
	if filter, ok := adapter.(interface{ AsyncImageModel(string) bool }); ok {
		return filter.AsyncImageModel(upstreamModel)
	}
	return true
}

func (r *Registry) AsyncImages(value account.Provider) (AsyncImageAdapter, bool) {
	return adapterAs[AsyncImageAdapter](r, value)
}
