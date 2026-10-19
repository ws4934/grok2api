package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Voice models: the Web composer's dictation and the read aloud of answers.
const (
	TTSModel = "chatgpt-tts"
	STTModel = "chatgpt-transcribe"

	maxSpeechInput     = 4096 // characters, as on OpenAI's speech endpoint
	maxSpeechAudio     = 32 << 20
	maxTranscribeAudio = 25 << 20
)

// speechModel answers the closing turn of a speech conversation. On free
// accounts it does not count against the reason quota (checked live), unlike
// auto, which is used when the account lacks it.
const speechModel = "gpt-5-3-mini"

// speechFormats are the read aloud formats; OpenAI's wav, flac and pcm have no equivalent.
var speechFormats = map[string]bool{"mp3": true, "aac": true, "opus": true}

// audioExtensions name an upload after its declared or sniffed type: dictation
// answers HTTP 500 for a file name without an audio extension (live: "blob").
var audioExtensions = map[string]string{
	"audio/webm": ".webm", "audio/ogg": ".ogg", "audio/mpeg": ".mp3", "audio/mp3": ".mp3", "audio/mp4": ".m4a",
	"audio/x-m4a": ".m4a", "audio/wav": ".wav", "audio/x-wav": ".wav", "audio/flac": ".flac", "audio/aac": ".aac",
	// http.DetectContentType results
	"video/webm": ".webm", "application/ogg": ".ogg", "video/mp4": ".mp4", "audio/wave": ".wav",
}

// voiceListTTL bounds how long an account's voice list is reused.
const voiceListTTL = 10 * time.Minute

type voiceList struct {
	voices    []provider.VoiceInfo
	selected  string
	fetchedAt time.Time
}

// voiceFailure is a dictation or read aloud backend error. Those services
// are shared by every account, so another account would fail the same way.
type voiceFailure struct{ upstream *upstreamError }

func (e *voiceFailure) Error() string              { return e.upstream.Error() }
func (e *voiceFailure) Unwrap() error              { return e.upstream }
func (e *voiceFailure) HTTPStatusCode() int        { return http.StatusBadGateway }
func (e *voiceFailure) RequestScopedFailure() bool { return true }

// PublicErrorMessage quotes at most 300 characters of the upstream body; an
// HTML error page stays in the diagnostics.
func (e *voiceFailure) PublicErrorMessage() string {
	message := fmt.Sprintf("ChatGPT %s failed: HTTP %d", e.upstream.Endpoint, e.upstream.Status)
	detail := []rune(strings.Join(strings.Fields(e.upstream.Message), " "))
	if len(detail) > 300 {
		detail = append(detail[:299], '…')
	}
	if len(detail) > 0 && detail[0] != '<' {
		message += " " + string(detail)
	}
	return message
}

// voiceError keeps a backend (5xx) failure of a voice endpoint on the
// request; a Cloudflare challenge stays an egress problem.
func voiceError(err error) error {
	var upstream *upstreamError
	if errors.As(err, &upstream) && upstream.Status >= http.StatusInternalServerError && !upstream.Challenge {
		return &voiceFailure{upstream: upstream}
	}
	return err
}

var conversationIDPattern = regexp.MustCompile(`"conversation_id":\s*"([0-9a-f-]{36})"`)

// SynthesizeSpeech reads text aloud. Read aloud only plays messages of a kept
// conversation, so the text becomes an assistant message of a new
// conversation, is synthesized by its ID, and the conversation is then hidden
// from the account's history.
func (a *Adapter) SynthesizeSpeech(ctx context.Context, request provider.TTSRequest) (provider.TTSResult, error) {
	text := strings.TrimSpace(request.Text)
	if text == "" {
		return provider.TTSResult{}, invalidRequest("input is required")
	}
	if utf8.RuneCountInString(text) > maxSpeechInput {
		return provider.TTSResult{}, invalidRequest(fmt.Sprintf("input is limited to %d characters", maxSpeechInput))
	}
	format := strings.ToLower(strings.TrimSpace(request.OutputFormat.Codec))
	if format == "" {
		format = "mp3"
	}
	if !speechFormats[format] {
		return provider.TTSResult{}, invalidRequest("ChatGPT speech supports the mp3, aac and opus formats")
	}
	c, err := a.decrypt(request.Credential)
	if err != nil {
		return provider.TTSResult{}, err
	}
	catalog, err := a.accountCatalog(ctx, request.Credential)
	if err != nil {
		return provider.TTSResult{}, err
	}
	model := "auto"
	if _, ok := catalog[speechModel]; ok {
		model = speechModel
	}
	var result provider.TTSResult
	err = a.call(ctx, request.Credential, c, func(ctx context.Context, w wireAccount) error {
		voice, err := a.speechVoice(ctx, w, request.Credential.ID, request.VoiceID)
		if err != nil {
			return err
		}
		conversation, message, err := a.speechConversation(ctx, w, model, text)
		if conversation != "" {
			defer a.hideConversation(ctx, w, conversation)
		}
		if err != nil {
			return err
		}
		query := url.Values{"message_id": {message}, "conversation_id": {conversation}, "format": {format}}
		if voice != "" {
			query.Set("voice", voice)
		}
		response, err := a.image.do(ctx, http.MethodGet, "/backend-api/synthesize?"+query.Encode(), w, nil, map[string]string{"Accept": "*/*"}, false)
		if err != nil {
			return voiceError(err)
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return voiceError(readOpenAIHTTPError(response, "/backend-api/synthesize"))
		}
		audio, err := io.ReadAll(io.LimitReader(response.Body, maxSpeechAudio+1))
		if err != nil {
			return err
		}
		if len(audio) > maxSpeechAudio {
			return errors.New("ChatGPT speech audio exceeds the size limit")
		}
		result = provider.TTSResult{Audio: audio, ContentType: response.Header.Get("Content-Type")}
		return nil
	})
	return result, err
}

// speechConversation opens a kept conversation holding text as an assistant
// message and returns its ID and the message ID. The stream is dropped once
// ChatGPT echoes the message (or ends): the closing reply is not needed. The
// conversation ID comes back with an error too, so the caller can hide it.
func (a *Adapter) speechConversation(ctx context.Context, w wireAccount, model, text string) (string, string, error) {
	payload := openAIChatPayload(wireChatRequest{Model: model, Messages: []wireMessage{
		{Role: "user", Content: "Read the next message aloud."},
		{Role: "assistant", Content: text},
		{Role: "user", Content: "Reply with OK."},
	}})
	payload["history_and_training_disabled"] = false
	message := stringValue(payload["messages"].([]any)[1].(map[string]any)["id"])
	response, err := a.image.postConversation(ctx, w, payload)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	conversation := ""
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if match := conversationIDPattern.FindStringSubmatch(line); match != nil && conversation == "" {
			conversation = match[1]
		}
		if conversation != "" && strings.Contains(line, message) {
			return conversation, message, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return conversation, "", err
	}
	if conversation != "" {
		return conversation, message, nil
	}
	return "", "", errors.New("ChatGPT did not open a speech conversation")
}

// hideConversation removes a speech conversation from the history; a failure
// only leaves it visible there.
func (a *Adapter) hideConversation(ctx context.Context, w wireAccount, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	response, err := a.image.do(ctx, http.MethodPatch, "/backend-api/conversation/"+url.PathEscape(id), w, map[string]any{"is_visible": false}, nil, false)
	if err == nil {
		_ = response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			err = fmt.Errorf("status %d", response.StatusCode)
		}
	}
	if err != nil {
		a.log().Warn("chatgpt_speech_conversation_hide_failed", "conversation_id", id, "error", err)
	}
}

// speechVoice resolves a voice by ID or display name from the account's
// cached list. Other names, such as OpenAI's alloy, use the selected voice.
func (a *Adapter) speechVoice(ctx context.Context, w wireAccount, accountID uint64, requested string) (string, error) {
	cached, ok := a.cachedVoices(accountID)
	if !ok {
		var err error
		if cached, err = a.voices(ctx, w, accountID); err != nil {
			return "", err
		}
	}
	if voice, ok := findVoice(cached.voices, requested); ok {
		return voice.VoiceID, nil
	}
	return cached.selected, nil
}

func findVoice(voices []provider.VoiceInfo, requested string) (provider.VoiceInfo, bool) {
	requested = strings.TrimSpace(requested)
	for _, voice := range voices {
		if requested != "" && (strings.EqualFold(voice.VoiceID, requested) || strings.EqualFold(voice.Name, requested)) {
			return voice, true
		}
	}
	return provider.VoiceInfo{}, false
}

// cachedVoices returns the account's voice list while it is fresh.
func (a *Adapter) cachedVoices(accountID uint64) (voiceList, bool) {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	cached, ok := a.voiceLists[accountID]
	return cached, ok && time.Since(cached.fetchedAt) <= voiceListTTL
}

// voices fetches the account's read aloud voices and the selected one and caches them.
func (a *Adapter) voices(ctx context.Context, w wireAccount, accountID uint64) (voiceList, error) {
	body, err := a.getJSON(ctx, w, http.MethodGet, "/backend-api/settings/voices", nil)
	if err != nil {
		return voiceList{}, err
	}
	list := voiceList{selected: stringValue(body["selected"]), fetchedAt: time.Now()}
	for _, raw := range toAnyList(body["voices"]) {
		item, _ := raw.(map[string]any)
		if id := stringValue(item["voice"]); id != "" {
			list.voices = append(list.voices, provider.VoiceInfo{VoiceID: id, Name: stringValue(item["name"]), Language: stringValue(item["accent"])})
		}
	}
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	if a.voiceLists == nil {
		a.voiceLists = map[uint64]voiceList{}
	}
	a.voiceLists[accountID] = list
	return list, nil
}

func (a *Adapter) ListTTSVoices(ctx context.Context, credential account.Credential) ([]provider.VoiceInfo, error) {
	if cached, ok := a.cachedVoices(credential.ID); ok {
		return cached.voices, nil
	}
	c, err := a.decrypt(credential)
	if err != nil {
		return nil, err
	}
	var list voiceList
	err = a.call(ctx, credential, c, func(ctx context.Context, w wireAccount) error {
		list, err = a.voices(ctx, w, credential.ID)
		return err
	})
	return list.voices, err
}

func (a *Adapter) GetTTSVoice(ctx context.Context, credential account.Credential, voiceID string) (provider.VoiceInfo, error) {
	voices, err := a.ListTTSVoices(ctx, credential)
	if err != nil {
		return provider.VoiceInfo{}, err
	}
	if voice, ok := findVoice(voices, voiceID); ok {
		return voice, nil
	}
	return provider.VoiceInfo{}, &requestError{message: "unknown ChatGPT voice " + voiceID, status: http.StatusNotFound}
}

// TranscribeSpeech sends an uploaded audio file and the optional language
// hint to the composer's dictation, which returns the text only and does not
// use the conversation quota.
func (a *Adapter) TranscribeSpeech(ctx context.Context, request provider.STTRequest) (provider.STTResult, error) {
	if len(request.FileData) == 0 {
		return provider.STTResult{}, invalidRequest("ChatGPT transcription needs an uploaded audio file")
	}
	if len(request.FileData) > maxTranscribeAudio {
		return provider.STTResult{}, invalidRequest("audio file is limited to 25 MB")
	}
	name := path.Base(strings.TrimSpace(request.FileName))
	if name == "." || name == "/" {
		name = "audio"
	}
	if !knownAudioExtension(path.Ext(name)) {
		mediaType, _, _ := mime.ParseMediaType(request.FileMIME)
		if _, ok := audioExtensions[mediaType]; !ok {
			mediaType = sniffAudio(request.FileData)
		}
		name += audioExtensions[mediaType]
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", name)
	if err != nil {
		return provider.STTResult{}, err
	}
	_, _ = part.Write(request.FileData)
	// Dictation takes an ISO 639-1 hint, so "zh-CN" and "zh_CN" are sent as "zh".
	language, _, _ := strings.Cut(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(request.Language)), "_", "-"), "-")
	if language != "" {
		_ = form.WriteField("language", language)
	}
	if err := form.Close(); err != nil {
		return provider.STTResult{}, err
	}
	c, err := a.decrypt(request.Credential)
	if err != nil {
		return provider.STTResult{}, err
	}
	var result provider.STTResult
	err = a.call(ctx, request.Credential, c, func(ctx context.Context, w wireAccount) error {
		headers := map[string]string{"Content-Type": form.FormDataContentType(), "Accept": "application/json"}
		response, err := a.image.doRequest(ctx, http.MethodPost, a.image.BaseURL+"/backend-api/transcribe", "/backend-api/transcribe", w, bytes.NewReader(body.Bytes()), headers, false, true)
		if err != nil {
			return voiceError(err)
		}
		var out struct {
			Text string `json:"text"`
		}
		if err := decodeOpenAIJSON(response, &out, "/backend-api/transcribe"); err != nil {
			return voiceError(err)
		}
		result = provider.STTResult{Text: out.Text}
		return nil
	})
	return result, err
}

// sniffAudio names the audio type of data. http.DetectContentType knows MP3
// only behind an ID3 tag, so bare MPEG and ADTS frames and FLAC are checked first.
func sniffAudio(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("fLaC")):
		return "audio/flac"
	case len(data) > 1 && data[0] == 0xFF && data[1]&0xF6 == 0xF0:
		return "audio/aac"
	case len(data) > 1 && data[0] == 0xFF && data[1]&0xE0 == 0xE0:
		return "audio/mpeg"
	}
	mediaType, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	return mediaType
}

// knownAudioExtension reports a file extension dictation accepts.
func knownAudioExtension(extension string) bool {
	extension = strings.ToLower(extension)
	for _, known := range audioExtensions {
		if extension == known {
			return true
		}
	}
	return extension == ".mpeg" || extension == ".mpga" || extension == ".oga"
}
