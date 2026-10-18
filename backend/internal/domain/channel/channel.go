// Package channel defines versioned extension contracts without transport or storage dependencies.
package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

const ContractVersion = 1

var (
	ErrNotFound       = errors.New("channel resource not found")
	ErrConflict       = errors.New("channel resource changed or already exists")
	ErrDisabled       = errors.New("channel is disabled")
	ErrUnsupported    = errors.New("unsupported channel capability")
	ErrQuotaExhausted = errors.New("channel capability quota exhausted")
	ErrNoAccount      = errors.New("no available verified channel account")
)

type Capabilities struct {
	Chat            bool `json:"chat"`
	Responses       bool `json:"responses"`
	ImageGeneration bool `json:"image_generation"`
	ImageEdit       bool `json:"image_edit"`
	Mask            bool `json:"mask"`
	Transparent     bool `json:"transparent"`
	OAuth           bool `json:"oauth"`
	Refresh         bool `json:"refresh"`
}
type ChannelDescriptor struct {
	ID           account.Provider `json:"id"`
	Name         string           `json:"name"`
	Version      int              `json:"version"`
	Capabilities Capabilities     `json:"capabilities"`
	AuthMethods  []string         `json:"auth_methods"`
	// ClearanceURL is the site FlareSolverr solves when Cloudflare clearance is
	// enabled for the channel; empty means the channel cannot use clearance.
	ClearanceURL string `json:"clearance_url,omitempty"`
}

// Settings carries operational policy. Protocol secrets are never configuration fields.
type Settings struct {
	Version                int    `json:"version"`
	Enabled                bool   `json:"enabled"`
	ImageConversationModel string `json:"image_conversation_model"`
	OutputTokenLimit       int    `json:"output_token_limit"`
	VisionTokens           int    `json:"vision_tokens"`
	AccountConcurrency     int    `json:"account_concurrency"`
	ImageWorkers           int    `json:"image_workers"`
	// DefaultImageModel is the public image route used when a Responses
	// image_generation tool names no model and the top-level model is a chat model.
	DefaultImageModel string `json:"default_image_model"`
	// CloudflareClearance solves challenges on demand through the shared
	// FlareSolverr endpoint; requests then present the solver's browser.
	CloudflareClearance bool `json:"cloudflare_clearance"`
}

func DefaultSettings() Settings {
	return Settings{Version: 1, OutputTokenLimit: 16384, VisionTokens: 256, AccountConcurrency: 1, ImageWorkers: 2}
}
func (v Settings) Validate() error {
	if v.Version != 1 || v.OutputTokenLimit < 1 || v.OutputTokenLimit > 131072 || v.VisionTokens < 0 || v.VisionTokens > 100000 || v.AccountConcurrency < 1 || v.AccountConcurrency > 256 || v.ImageWorkers < 1 || v.ImageWorkers > 32 {
		return errors.New("invalid channel settings")
	}
	return nil
}

type Credentials struct {
	Version        int        `json:"version"`
	AccessToken    string     `json:"access_token,omitempty"`
	RefreshToken   string     `json:"refresh_token,omitempty"`
	IDToken        string     `json:"id_token,omitempty"`
	ClientProfile  string     `json:"client_profile,omitempty"`
	DeviceID       string     `json:"device_id,omitempty"`
	SessionID      string     `json:"session_id,omitempty"`
	BrowserProfile string     `json:"browser_profile,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
}
type ImportRecord struct {
	Error       string      `json:"-"`
	Name        string      `json:"name,omitempty"`
	Credentials Credentials `json:"credentials"`
}
type Identity struct {
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Email       string `json:"email,omitempty"`
	Plan        string `json:"plan,omitempty"`
}
type FeatureQuota struct {
	Feature    string     `json:"feature"`
	Remaining  *int64     `json:"remaining"`
	ResetAt    *time.Time `json:"reset_at,omitempty"`
	ObservedAt time.Time  `json:"observed_at"`
}
type Snapshot struct {
	Identity Identity
	Models   []string
	Quotas   []FeatureQuota
}
type AccountLifecycle interface {
	ParseImport([]byte) ([]ImportRecord, error)
	Prepare(context.Context, Credentials) (Credentials, error)
	Validate(context.Context, Credentials) (Snapshot, error)
	Refresh(context.Context, Credentials) (Credentials, error)
}
type OAuthStart struct {
	URL         string
	State       string
	Verifier    string
	RedirectURI string
	Profile     string
}
type OAuthAdapter interface {
	StartOAuth(context.Context) (OAuthStart, error)
	ExchangeOAuth(context.Context, OAuthStart, string) (Credentials, error)
}
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}
type Message struct {
	Role    string        `json:"role"`
	Content []ContentPart `json:"content"`
}
type ChatRequest struct {
	Model    string
	Messages []Message
	// ReasoningEffort is normalized by the transport: "", low, medium, high or extended.
	ReasoningEffort string
	// MaxOutputTokens lowers the channel output limit for one request (0 = channel limit).
	MaxOutputTokens int
}
type ChatEvent struct {
	Text      string
	Reasoning string
	Done      bool
	Truncated bool
}
type ChatAdapter interface {
	Chat(context.Context, Credentials, ChatRequest, func(ChatEvent) error) error
}
type ImageInput struct {
	AssetID string `json:"asset_id"`
	Name    string `json:"name"`
	MIME    string `json:"mime"`
}
type ImageRequest struct {
	Operation         string       `json:"operation"`
	Model             string       `json:"model"`
	ConversationModel string       `json:"conversation_model"`
	Prompt            string       `json:"prompt"`
	N                 int          `json:"n"`
	Size              string       `json:"size"`
	Quality           string       `json:"quality"`
	OutputFormat      string       `json:"output_format"`
	Background        string       `json:"background"`
	Inputs            []ImageInput `json:"inputs,omitempty"`
	Mask              *ImageInput  `json:"mask,omitempty"`
	// PublicModel and Async echo the client's Responses request when the job is
	// read back through GET /v1/responses/{id}.
	PublicModel string `json:"public_model,omitempty"`
	Async       bool   `json:"async,omitempty"`
}

// Checkpoint contains upstream references only; credentials stay in the account store.
type Checkpoint struct {
	Submitted      bool            `json:"submitted"`
	Version        int             `json:"version"`
	ConversationID string          `json:"conversation_id,omitempty"`
	FileIDs        []string        `json:"file_ids,omitempty"`
	Uploads        []UploadedImage `json:"uploads,omitempty"`
}
type UploadedImage struct {
	Index  int    `json:"index"`
	Mask   bool   `json:"mask"`
	FileID string `json:"file_id"`
	Name   string `json:"name"`
	MIME   string `json:"mime"`
	Size   int    `json:"size"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}
type PreparedImage struct {
	Request    ImageRequest
	Checkpoint Checkpoint
	Inputs     []Blob
	Mask       *Blob
}
type Blob struct {
	Name string
	MIME string
	Data []byte
}
type ImageWorkflowAdapter interface {
	PrepareImage(context.Context, Credentials, ImageRequest, func(ImageInput) (Blob, error)) (PreparedImage, error)
	SubmitImage(context.Context, Credentials, PreparedImage, func(Checkpoint) error) (Checkpoint, error)
	PollImage(context.Context, Credentials, Checkpoint) (Checkpoint, error)
	FetchImage(context.Context, Credentials, Checkpoint) ([]Blob, error)
}
type QuotaNeed struct {
	Feature string
	Amount  int64
}
type QuotaPolicy interface {
	ChatQuota(ChatRequest) []QuotaNeed
	ImageQuota(ImageRequest) []QuotaNeed
}
type FailureKind string

const (
	FailureAuth      FailureKind = "authentication"
	FailureModel     FailureKind = "model"
	FailureQuota     FailureKind = "quota"
	FailureNetwork   FailureKind = "network"
	FailureRequest   FailureKind = "request"
	FailureUncertain FailureKind = "uncertain"
)

// Failure codes that describe the egress rather than the account or request.
// The gateway retries them on another egress before anything reached upstream.
const (
	FailureCodeTransport = "transport"
	FailureCodeChallenge = "cloudflare_challenge"
)

type Failure struct {
	Kind    FailureKind
	Code    string
	Message string
	Unsent  bool
}

func (e *Failure) Error() string { return e.Message }

type FailureClassifier interface{ Classify(error) Failure }
type Module interface{ Descriptor() ChannelDescriptor }
type Registry struct{ modules map[account.Provider]Module }

func NewRegistry(modules ...Module) (*Registry, error) {
	r := &Registry{modules: map[account.Provider]Module{}}
	for _, m := range modules {
		if m == nil {
			return nil, errors.New("nil channel module")
		}
		d := m.Descriptor()
		if !d.ID.IsValid() || d.ID.IsLegacy() || d.Name == "" || d.Version != ContractVersion {
			return nil, fmt.Errorf("invalid channel descriptor: %s", d.ID)
		}
		if _, ok := r.modules[d.ID]; ok {
			return nil, fmt.Errorf("duplicate channel: %s", d.ID)
		}
		if _, ok := m.(AccountLifecycle); !ok {
			return nil, errors.New("channel lacks account lifecycle")
		}
		if d.Capabilities.Chat || d.Capabilities.Responses {
			if _, ok := m.(ChatAdapter); !ok {
				return nil, errors.New("channel lacks chat adapter")
			}
		}
		if d.Capabilities.ImageGeneration || d.Capabilities.ImageEdit {
			if _, ok := m.(ImageWorkflowAdapter); !ok {
				return nil, errors.New("channel lacks image workflow")
			}
		}
		if d.Capabilities.OAuth {
			if _, ok := m.(OAuthAdapter); !ok {
				return nil, errors.New("channel lacks OAuth adapter")
			}
		}
		r.modules[d.ID] = m
	}
	return r, nil
}
func (r *Registry) Get(id account.Provider) (Module, bool) { m, ok := r.modules[id]; return m, ok }
func (r *Registry) Descriptors() []ChannelDescriptor {
	out := make([]ChannelDescriptor, 0, len(r.modules))
	for _, m := range r.modules {
		d := m.Descriptor()
		d.AuthMethods = append([]string(nil), d.AuthMethods...)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Close allows protocol clients to release idle transports during shutdown.
type Closer interface{ io.Closer }
