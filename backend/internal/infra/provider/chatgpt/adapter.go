package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
)

const ID account.Provider = "chatgpt"

type Adapter struct {
	image    *OpenAIImage
	oauth    *http.Client
	tokenURL string
	authURL  string
}

func NewAdapter() *Adapter {
	return newAdapter("https://chatgpt.com", "https://auth.openai.com/api/accounts/authorize", "https://auth.openai.com/oauth/token", &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
}
func newAdapter(base, auth, token string, client *http.Client) *Adapter {
	return &Adapter{image: &OpenAIImage{BaseURL: strings.TrimRight(base, "/"), Client: client, RequestTimeout: 10 * time.Minute, browsers: map[string]*browserHTTP{}, browserMu: sync.Mutex{}, pow: make(chan struct{}, 2)}, oauth: client, authURL: auth, tokenURL: token}
}
func (a *Adapter) Descriptor() channel.ChannelDescriptor {
	return channel.ChannelDescriptor{ID: ID, Name: "ChatGPT", Version: 1, AuthMethods: []string{"access_token", "refresh_token", "oauth_pkce"}, Capabilities: channel.Capabilities{Chat: true, Responses: true, ImageGeneration: true, ImageEdit: true, Mask: true, Transparent: true, OAuth: true, Refresh: true}}
}
func (a *Adapter) Close() error {
	a.image.browserMu.Lock()
	defer a.image.browserMu.Unlock()
	for _, b := range a.image.browsers {
		b.CloseIdleConnections()
	}
	return nil
}
func wire(c channel.Credentials) (wireAccount, error) {
	p := browserprofile.Default()
	if c.BrowserProfile != "" {
		found := false
		for _, candidate := range browserprofile.Candidates() {
			if candidate.Name == c.BrowserProfile {
				p = candidate
				found = true
				break
			}
		}
		if !found {
			if json.Unmarshal([]byte(c.BrowserProfile), &p) != nil || !p.Valid() {
				return wireAccount{}, errors.New("unsupported browser profile")
			}
		}
	}
	return wireAccount{Token: c.AccessToken, Fields: map[string]any{"access_token": c.AccessToken, "browser_profile": p, "oai_device_id": c.DeviceID, "oai_session_id": c.SessionID}}, nil
}
func (a *Adapter) Chat(ctx context.Context, c channel.Credentials, r channel.ChatRequest, emit func(channel.ChatEvent) error) error {
	w, e := wire(c)
	if e != nil {
		return e
	}
	request := wireChatRequest{Model: r.Model}
	var images []OpenAIChatImage
	for mi, m := range r.Messages {
		parts := []any{}
		for pi, p := range m.Content {
			switch p.Type {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
			case "image_url":
				b, err := a.loadInput(ctx, p.ImageURL)
				if err != nil {
					return err
				}
				images = append(images, OpenAIChatImage{MessageIndex: mi, PartIndex: pi, Input: OpenAIImageInput{Name: b.Name, MIME: b.MIME, Data: b.Data}})
				parts = append(parts, map[string]any{"type": "image_url"})
			default:
				return channel.ErrUnsupported
			}
		}
		request.Messages = append(request.Messages, wireMessage{Role: m.Role, Content: parts})
	}
	return NewOpenAIChat(a.image).Stream(ctx, w, request, func(e OpenAIChatEvent) error {
		return emit(channel.ChatEvent{Text: e.Text, Reasoning: e.Thinking, Done: e.Done})
	}, images...)
}
func (a *Adapter) loadInput(ctx context.Context, s string) (channel.Blob, error) {
	var raw []byte
	if strings.HasPrefix(s, "data:") {
		head, data, ok := strings.Cut(s, ",")
		if !ok || !strings.HasSuffix(head, ";base64") {
			return channel.Blob{}, errors.New("invalid input image")
		}
		if len(data) > 28<<20 {
			return channel.Blob{}, errors.New("input image too large")
		}
		b, e := base64.StdEncoding.DecodeString(data)
		if e != nil {
			return channel.Blob{}, errors.New("invalid image encoding")
		}
		raw = b
	} else {
		u, e := url.Parse(s)
		if e != nil || u.Scheme != "https" || u.User != nil {
			return channel.Blob{}, errors.New("invalid input image URL")
		}
		r, e := a.image.doAbsolute(ctx, "GET", s, wireAccount{}, nil, nil, false)
		if e != nil {
			return channel.Blob{}, e
		}
		defer r.Body.Close()
		raw, e = readBounded(r.Body, 20<<20)
		if e != nil {
			return channel.Blob{}, e
		}
	}
	mime, e := validatedImageMIME(raw)
	return channel.Blob{Name: "image", MIME: mime, Data: raw}, e
}
func (a *Adapter) PrepareImage(ctx context.Context, c channel.Credentials, r channel.ImageRequest, load func(channel.ImageInput) (channel.Blob, error)) (channel.PreparedImage, error) {
	p := channel.PreparedImage{Request: r}
	if r.ConversationModel == "" || r.ConversationModel == "gpt-image-2" {
		return p, errors.New("a validated image conversation model is required")
	}
	o := ImageOutputOptions{OutputFormat: r.OutputFormat, Background: r.Background}
	if e := o.Validate(); e != nil {
		return p, e
	}
	var inputs []OpenAIImageInput
	for _, v := range r.Inputs {
		b, e := load(v)
		if e != nil {
			return p, e
		}
		if len(b.Data) > 20<<20 {
			return p, errors.New("input image too large")
		}
		if _, e := validatedImageMIME(b.Data); e != nil {
			return p, e
		}
		p.Inputs = append(p.Inputs, b)
		inputs = append(inputs, OpenAIImageInput{Name: b.Name, MIME: b.MIME, Data: b.Data})
	}
	if r.Mask != nil {
		b, e := load(*r.Mask)
		if e != nil {
			return p, e
		}
		mask, e := ParseImageMask(b.Data, inputs)
		if e != nil {
			return p, e
		}
		p.Mask = &channel.Blob{Name: "mask.png", MIME: "image/png", Data: mask.png}
	}
	return p, nil
}

type checkpointKey struct{}

func (a *Adapter) SubmitImage(ctx context.Context, c channel.Credentials, p channel.PreparedImage, save func(channel.Checkpoint) error) (channel.Checkpoint, error) {
	w, e := wire(c)
	if e != nil {
		return channel.Checkpoint{}, e
	}
	ctx = a.image.withTimezone(ctx, w)
	cp := p.Checkpoint
	cp.Version = 1
	persist := func(next channel.Checkpoint) error {
		next.Uploads = cp.Uploads
		if err := save(next); err != nil {
			return err
		}
		cp = next
		return nil
	}
	upload := func(blob channel.Blob, index int, mask bool) (openAIImageReference, error) {
		for _, saved := range cp.Uploads {
			if saved.Index == index && saved.Mask == mask {
				return openAIImageReference{FileID: saved.FileID, FileName: saved.Name, MIME: saved.MIME, FileSize: saved.Size, Width: saved.Width, Height: saved.Height}, nil
			}
		}
		ref, err := a.image.uploadInputWithPurpose(ctx, w, OpenAIImageInput{Name: blob.Name, MIME: blob.MIME, Data: blob.Data}, index+1, mask)
		if err != nil {
			return ref, err
		}
		cp.Uploads = append(cp.Uploads, channel.UploadedImage{Index: index, Mask: mask, FileID: ref.FileID, Name: ref.FileName, MIME: ref.MIME, Size: ref.FileSize, Width: ref.Width, Height: ref.Height})
		return ref, persist(cp)
	}
	if e := channel.ObserveStage(ctx, "uploading"); e != nil {
		return cp, e
	}
	var refs, masks []openAIImageReference
	for index, blob := range p.Inputs {
		ref, err := upload(blob, index, false)
		if err != nil {
			return cp, err
		}
		refs = append(refs, ref)
	}
	if p.Mask != nil {
		ref, err := upload(*p.Mask, len(p.Inputs), true)
		if err != nil {
			return cp, err
		}
		masks = append(masks, ref)
	}
	if e := channel.ObserveStage(ctx, "sentinel"); e != nil {
		return cp, e
	}
	scripts, build, e := a.image.bootstrap(ctx, w)
	if e != nil {
		return cp, e
	}
	requirements, e := a.image.chatRequirements(ctx, w, scripts, build)
	if e != nil {
		return cp, e
	}
	r := p.Request
	prompt := r.Prompt + (ImageOutputOptions{OutputFormat: r.OutputFormat, Background: r.Background}).promptSuffix()
	conduit, e := a.image.prepare(ctx, w, requirements, prompt, r.ConversationModel)
	if e != nil {
		return cp, e
	}
	ctx = context.WithValue(ctx, checkpointKey{}, persist)
	id, files, e := a.image.start(ctx, w, requirements, conduit, prompt, r.ConversationModel, r.Size, r.Quality, refs, masks...)
	if e != nil {
		return cp, e
	}
	if id == "" {
		return cp, &channel.Failure{Kind: channel.FailureUncertain, Code: "missing_conversation", Message: "upstream submission has no recoverable conversation ID"}
	}
	cp.Submitted, cp.ConversationID, cp.FileIDs = true, id, files
	return cp, persist(cp)
}

func (a *Adapter) PollImage(ctx context.Context, c channel.Credentials, cp channel.Checkpoint) (channel.Checkpoint, error) {
	w, e := wire(c)
	if e != nil {
		return cp, e
	}
	if cp.ConversationID == "" {
		return cp, errors.New("missing conversation checkpoint")
	}
	if len(cp.FileIDs) > 0 {
		return cp, nil
	}
	cp.FileIDs, e = a.image.pollConversation(ctx, w, cp.ConversationID)
	return cp, e
}
func (a *Adapter) FetchImage(ctx context.Context, c channel.Credentials, cp channel.Checkpoint) ([]channel.Blob, error) {
	w, e := wire(c)
	if e != nil {
		return nil, e
	}
	out := []channel.Blob{}
	for _, id := range cp.FileIDs {
		raw, mime, e := a.image.downloadImageRef(ctx, w, cp.ConversationID, id)
		if e != nil {
			return nil, e
		}
		out = append(out, channel.Blob{Name: "image", MIME: mime, Data: raw})
	}
	return out, nil
}
func (a *Adapter) Classify(err error) channel.Failure {
	var f *channel.Failure
	if errors.As(err, &f) {
		return *f
	}
	var u *upstreamError
	if errors.As(err, &u) {
		kind := channel.FailureNetwork
		if u.Status == 401 {
			kind = channel.FailureAuth
		} else if u.Status == 429 {
			kind = channel.FailureQuota
		} else if IsModelRejected(err) {
			kind = channel.FailureModel
		}
		return channel.Failure{Kind: kind, Code: fmt.Sprintf("upstream_%d", u.Status), Message: u.Error(), Unsent: u.Status >= 400 && u.Status < 500}
	}
	return channel.Failure{Kind: channel.FailureNetwork, Code: "protocol_error", Message: "ChatGPT protocol request failed"}
}
func (a *Adapter) ChatQuota(r channel.ChatRequest) []channel.QuotaNeed {
	n := int64(0)
	for _, m := range r.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				n++
			}
		}
	}
	v := []channel.QuotaNeed{{Feature: "reason", Amount: 1}}
	if n > 0 {
		v = append(v, channel.QuotaNeed{Feature: "file_upload", Amount: n})
	}
	return v
}
func (a *Adapter) ImageQuota(r channel.ImageRequest) []channel.QuotaNeed {
	n := len(r.Inputs)
	if r.Mask != nil {
		n++
	}
	return []channel.QuotaNeed{{Feature: "reason", Amount: 1}, {Feature: "image_gen", Amount: 1}, {Feature: "file_upload", Amount: int64(n)}}
}
func ConvertImage(b channel.Blob, format, background string) (channel.Blob, error) {
	raw, mime, e := (ImageOutputOptions{OutputFormat: format, Background: background}).apply(b.Data, b.MIME)
	return channel.Blob{Name: b.Name, MIME: mime, Data: raw}, e
}

func (a *Adapter) ValidateImageOutput(b channel.Blob, r channel.ImageRequest) (channel.Blob, error) {
	return ConvertImage(b, r.OutputFormat, r.Background)
}
