package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// speechFormats are the read aloud formats; OpenAI's wav, flac and pcm have no equivalent.
var speechFormats = map[string]bool{"mp3": true, "aac": true, "opus": true}

// audioExtensions name an unnamed upload after its type, so dictation can tell the format.
var audioExtensions = map[string]string{
	"audio/webm": ".webm", "audio/ogg": ".ogg", "audio/mpeg": ".mp3", "audio/mp3": ".mp3", "audio/mp4": ".m4a",
	"audio/x-m4a": ".m4a", "audio/wav": ".wav", "audio/x-wav": ".wav", "audio/flac": ".flac", "audio/aac": ".aac",
}

var conversationIDPattern = regexp.MustCompile(`"conversation_id":\s*"([0-9a-f-]{36})"`)

// SynthesizeSpeech reads text aloud. Read aloud only plays messages of a kept
// conversation, so the text becomes an assistant message of a new
// conversation (one request against the reason quota), is synthesized by its
// ID, and the conversation is then hidden from the account's history.
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
	var result provider.TTSResult
	err = a.call(ctx, request.Credential, c, func(ctx context.Context, w wireAccount) error {
		voice, err := a.speechVoice(ctx, w, request.VoiceID)
		if err != nil {
			return err
		}
		conversation, message, err := a.speechConversation(ctx, w, text)
		if err != nil {
			return err
		}
		defer a.hideConversation(ctx, w, conversation)
		query := url.Values{"message_id": {message}, "conversation_id": {conversation}, "format": {format}}
		if voice != "" {
			query.Set("voice", voice)
		}
		response, err := a.image.do(ctx, http.MethodGet, "/backend-api/synthesize?"+query.Encode(), w, nil, map[string]string{"Accept": "*/*"}, false)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return readOpenAIHTTPError(response, "/backend-api/synthesize")
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
// ChatGPT echoes the message: the closing reply is not needed.
func (a *Adapter) speechConversation(ctx context.Context, w wireAccount, text string) (string, string, error) {
	payload := openAIChatPayload(wireChatRequest{Model: "auto", Messages: []wireMessage{
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
		return "", "", err
	}
	return "", "", errors.New("ChatGPT did not store the speech message")
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
		slog.Warn("chatgpt_speech_conversation_hide_failed", "conversation_id", id, "error", err)
	}
}

// speechVoice resolves a voice by ID or display name. Other names, such as
// OpenAI's alloy, use the voice selected on the account.
func (a *Adapter) speechVoice(ctx context.Context, w wireAccount, requested string) (string, error) {
	voices, selected, err := a.voices(ctx, w)
	if err != nil {
		return "", err
	}
	if voice, ok := findVoice(voices, requested); ok {
		return voice.VoiceID, nil
	}
	return selected, nil
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

// voices lists the account's read aloud voices and the selected one.
func (a *Adapter) voices(ctx context.Context, w wireAccount) ([]provider.VoiceInfo, string, error) {
	body, err := a.getJSON(ctx, w, http.MethodGet, "/backend-api/settings/voices", nil)
	if err != nil {
		return nil, "", err
	}
	var voices []provider.VoiceInfo
	for _, raw := range toAnyList(body["voices"]) {
		item, _ := raw.(map[string]any)
		if id := stringValue(item["voice"]); id != "" {
			voices = append(voices, provider.VoiceInfo{VoiceID: id, Name: stringValue(item["name"]), Language: stringValue(item["accent"])})
		}
	}
	return voices, stringValue(body["selected"]), nil
}

func (a *Adapter) ListTTSVoices(ctx context.Context, credential account.Credential) ([]provider.VoiceInfo, error) {
	c, err := a.decrypt(credential)
	if err != nil {
		return nil, err
	}
	var voices []provider.VoiceInfo
	err = a.call(ctx, credential, c, func(ctx context.Context, w wireAccount) error {
		voices, _, err = a.voices(ctx, w)
		return err
	})
	return voices, err
}

func (a *Adapter) GetTTSVoice(ctx context.Context, credential account.Credential, voiceID string) (provider.VoiceInfo, error) {
	voices, err := a.ListTTSVoices(ctx, credential)
	if err != nil {
		return provider.VoiceInfo{}, err
	}
	if voice, ok := findVoice(voices, voiceID); ok {
		return voice, nil
	}
	return provider.VoiceInfo{}, &upstreamError{Status: http.StatusNotFound, Message: "Unknown ChatGPT voice " + voiceID, Body: "unknown voice"}
}

// TranscribeSpeech sends an uploaded audio file to the composer's dictation,
// which returns the text only and does not use the conversation quota.
func (a *Adapter) TranscribeSpeech(ctx context.Context, request provider.STTRequest) (provider.STTResult, error) {
	if len(request.FileData) == 0 {
		return provider.STTResult{}, invalidRequest("ChatGPT transcription needs an uploaded audio file")
	}
	if len(request.FileData) > maxTranscribeAudio {
		return provider.STTResult{}, invalidRequest("audio file is limited to 25 MB")
	}
	name := path.Base(strings.TrimSpace(request.FileName))
	if name == "." || name == "/" {
		mediaType, _, _ := mime.ParseMediaType(request.FileMIME)
		name = "audio" + audioExtensions[mediaType]
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", name)
	if err != nil {
		return provider.STTResult{}, err
	}
	_, _ = part.Write(request.FileData)
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
			return err
		}
		var out struct {
			Text string `json:"text"`
		}
		if err := decodeOpenAIJSON(response, &out, "/backend-api/transcribe"); err != nil {
			return err
		}
		result = provider.STTResult{Text: out.Text}
		return nil
	})
	return result, err
}
