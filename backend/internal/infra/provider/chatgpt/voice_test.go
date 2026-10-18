package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const voicesBody = `{"selected":"breeze","voices":[{"voice":"breeze","name":"Breeze","accent":"American English"},{"voice":"glimmer","name":"Sol","accent":"American English"}]}`

// voiceUpstream serves the voice endpoints and records the speech calls.
type voiceUpstream struct {
	mu sync.Mutex
	// models lists the account's conversation models.
	models                     []string
	payload                    map[string]any
	synthesize, hidden, upload string
	calls, voiceLists          int
	// stream shapes the conversation reply: "" echoes the speech message,
	// "no-echo" ends without it, "cut" drops the connection after the ID.
	stream string
	// fail makes synthesize and transcribe answer like a failing ASR/TTS backend.
	fail bool
}

func (v *voiceUpstream) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveWebBasics(w, r) {
			return
		}
		v.mu.Lock()
		defer v.mu.Unlock()
		v.calls++
		switch {
		case r.URL.Path == "/backend-api/models":
			var models []map[string]any
			for _, slug := range v.models {
				models = append(models, map[string]any{"slug": slug})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
		case r.URL.Path == "/backend-api/settings/voices":
			v.voiceLists++
			_, _ = w.Write([]byte(voicesBody))
		case r.URL.Path == "/backend-api/conversation":
			_ = json.NewDecoder(r.Body).Decode(&v.payload)
			id := v.payload["messages"].([]any)[1].(map[string]any)["id"].(string)
			w.Header().Set("Content-Type", "text/event-stream")
			head := `data: {"type":"resume_conversation_token","conversation_id":"11111111-2222-3333-4444-555555555555"}` + "\n\n"
			switch v.stream {
			case "cut":
				w.Header().Set("Content-Length", "100000")
				_, _ = w.Write([]byte(head))
			case "no-echo":
				_, _ = w.Write([]byte(head + "data: [DONE]\n\n"))
			default:
				_, _ = w.Write([]byte(head + `data: {"type":"input_message","input_message":{"id":"` + id + `","author":{"role":"assistant"}}}` + "\n\n"))
			}
		case r.URL.Path == "/backend-api/synthesize":
			v.synthesize = r.URL.RawQuery
			if v.fail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"detail":"Error in TTS API"}`))
				return
			}
			w.Header().Set("Content-Type", "audio/aac")
			_, _ = w.Write([]byte("AAC"))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/backend-api/conversation/"):
			body, _ := io.ReadAll(r.Body)
			v.hidden = strings.TrimPrefix(r.URL.Path, "/backend-api/conversation/") + " " + string(body)
			_, _ = w.Write([]byte(`{"success":true}`))
		case r.URL.Path == "/backend-api/transcribe":
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Errorf("transcribe form: %v", err)
				return
			}
			data, _ := io.ReadAll(file)
			v.upload = header.Filename + ":" + string(data) + ":" + r.FormValue("language")
			if v.fail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"detail":"Error in ASR API"}`))
				return
			}
			_, _ = w.Write([]byte(`{"text":"hello there","asset_format":"webm"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSpeechReadsTextThroughAHiddenConversation(t *testing.T) {
	upstream := &voiceUpstream{models: []string{"auto", "gpt-5-3-mini"}}
	a, _ := newTestAdapter(t, upstream.serve(t))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	result, err := a.SynthesizeSpeech(context.Background(), provider.TTSRequest{Credential: credential, Model: TTSModel, Text: " 你好 ", VoiceID: "Sol", OutputFormat: provider.TTSOutputFormat{Codec: "aac"}})
	if err != nil || string(result.Audio) != "AAC" || result.ContentType != "audio/aac" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	messages := upstream.payload["messages"].([]any)
	spoken := messages[1].(map[string]any)
	if upstream.payload["model"] != speechModel || upstream.payload["history_and_training_disabled"] != false || spoken["author"].(map[string]any)["role"] != "assistant" || spoken["content"].(map[string]any)["parts"].([]any)[0] != "你好" {
		t.Fatalf("payload = %v", upstream.payload)
	}
	want := "conversation_id=11111111-2222-3333-4444-555555555555&format=aac&message_id=" + spoken["id"].(string) + "&voice=glimmer"
	if upstream.synthesize != want {
		t.Fatalf("synthesize = %s", upstream.synthesize)
	}
	if !strings.HasPrefix(upstream.hidden, "11111111-2222-3333-4444-555555555555 ") || !strings.Contains(upstream.hidden, `"is_visible":false`) {
		t.Fatalf("hidden = %q", upstream.hidden)
	}

	// OpenAI voice names fall back to the account's voice.
	if _, err := a.SynthesizeSpeech(context.Background(), provider.TTSRequest{Credential: credential, Text: "hi", VoiceID: "ara"}); err != nil || !strings.Contains(upstream.synthesize, "format=mp3") || !strings.HasSuffix(upstream.synthesize, "voice=breeze") {
		t.Fatalf("fallback synthesize = %s, %v", upstream.synthesize, err)
	}

	// Without the speech model the conversation falls back to auto.
	other := &voiceUpstream{models: []string{"auto", "gpt-6"}}
	b, _ := newTestAdapter(t, other.serve(t))
	if _, err := b.SynthesizeSpeech(context.Background(), provider.TTSRequest{Credential: testCredential(t, b, credentials{AccessToken: "AT"}), Text: "hi"}); err != nil || other.payload["model"] != "auto" {
		t.Fatalf("model = %v, %v", other.payload["model"], err)
	}
}

func TestSpeechRejectsWhatReadAloudCannotDo(t *testing.T) {
	upstream := &voiceUpstream{}
	a, _ := newTestAdapter(t, upstream.serve(t))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	for _, request := range []provider.TTSRequest{
		{Credential: credential, Text: "hi", OutputFormat: provider.TTSOutputFormat{Codec: "wav"}},
		{Credential: credential, Text: " "},
		{Credential: credential, Text: strings.Repeat("字", maxSpeechInput+1)},
	} {
		var scoped *requestError
		if _, err := a.SynthesizeSpeech(context.Background(), request); !errors.As(err, &scoped) {
			t.Fatalf("%q accepted: %v", request.OutputFormat.Codec, err)
		}
	}
	if upstream.calls != 0 {
		t.Fatalf("invalid requests reached the upstream %d times", upstream.calls)
	}
}

func TestTranscriptionUsesDictation(t *testing.T) {
	upstream := &voiceUpstream{}
	a, _ := newTestAdapter(t, upstream.serve(t))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	result, err := a.TranscribeSpeech(context.Background(), provider.STTRequest{Credential: credential, Model: STTModel, FileMIME: "audio/webm", FileData: []byte("OPUS")})
	if err != nil || result.Text != "hello there" || upstream.upload != "audio.webm:OPUS:" {
		t.Fatalf("result = %+v, upload = %q, %v", result, upstream.upload, err)
	}
	// The language hint goes to dictation as its primary subtag.
	for _, language := range []string{" zh-CN ", "zh_CN", "ZH"} {
		if _, err := a.TranscribeSpeech(context.Background(), provider.STTRequest{Credential: credential, FileName: "a.mp3", FileData: []byte("MP3"), Language: language}); err != nil || upstream.upload != "a.mp3:MP3:zh" {
			t.Fatalf("%q: upload = %q, %v", language, upstream.upload, err)
		}
	}
	// Dictation fails on names without an audio extension (live: "blob" → HTTP 500).
	for _, upload := range []struct{ name, mime, data, want string }{
		{"blob", "audio/webm; codecs=opus", "OPUS", "blob.webm:OPUS:"},
		{"blob", "application/octet-stream", "ID3\x03rest", "blob.mp3:ID3\x03rest:"},
		{"voice.bin", "audio/mp4", "M4A", "voice.bin.m4a:M4A:"},
		{"take.M4A", "", "M4A", "take.M4A:M4A:"},
	} {
		if _, err := a.TranscribeSpeech(context.Background(), provider.STTRequest{Credential: credential, FileName: upload.name, FileMIME: upload.mime, FileData: []byte(upload.data)}); err != nil || upstream.upload != upload.want {
			t.Fatalf("%s %s: upload = %q, %v", upload.name, upload.mime, upstream.upload, err)
		}
	}
	var scoped *requestError
	if _, err := a.TranscribeSpeech(context.Background(), provider.STTRequest{Credential: credential, URL: "https://example.com/a.mp3"}); !errors.As(err, &scoped) {
		t.Fatalf("URL input accepted: %v", err)
	}

	voices, err := a.ListTTSVoices(context.Background(), credential)
	if err != nil || len(voices) != 2 || voices[1] != (provider.VoiceInfo{VoiceID: "glimmer", Name: "Sol", Language: "American English"}) {
		t.Fatalf("voices = %+v, %v", voices, err)
	}
	if _, err := a.GetTTSVoice(context.Background(), credential, "alloy"); provider.ErrorHTTPStatusOrZero(err) != http.StatusNotFound || !provider.IsRequestScopedError(err) {
		t.Fatalf("unknown voice: %v", err)
	} else if message, ok := provider.ErrorPublicMessage(err); !ok || !strings.Contains(message, "alloy") {
		t.Fatalf("unknown voice message = %q", message)
	}
}

func TestVoiceRoutesAndQuota(t *testing.T) {
	a := NewAdapter(Config{}, nil, nil, nil)
	if a.QuotaMode(STTModel) != "" || a.QuotaMode(TTSModel) != "" {
		t.Fatal("voice quota modes")
	}
	capabilities := map[string]modeldomain.Capability{}
	for _, route := range Routes() {
		capabilities[route.UpstreamModel] = route.Capability
	}
	if capabilities[TTSModel] != modeldomain.CapabilityTTS || capabilities[STTModel] != modeldomain.CapabilitySTT {
		t.Fatalf("voice routes = %v", capabilities)
	}
	if _, ok := any(a).(provider.TTSAdapter); !ok {
		t.Fatal("not a TTS adapter")
	}
	if _, ok := any(a).(provider.STTAdapter); !ok {
		t.Fatal("not an STT adapter")
	}
}

func TestSpeechConversationIsHiddenWhateverHappens(t *testing.T) {
	for _, stream := range []string{"no-echo", "cut"} {
		upstream := &voiceUpstream{models: []string{"gpt-5-3-mini"}, stream: stream}
		a, _ := newTestAdapter(t, upstream.serve(t))
		_, err := a.SynthesizeSpeech(context.Background(), provider.TTSRequest{Credential: testCredential(t, a, credentials{AccessToken: "AT"}), Text: "hi"})
		// A finished stream stored the message; a cut one may not have.
		if (stream == "no-echo") != (err == nil) || !strings.HasPrefix(upstream.hidden, "11111111-2222-3333-4444-555555555555 ") {
			t.Fatalf("%s: err = %v, hidden = %q", stream, err, upstream.hidden)
		}
	}
}

func TestVoiceBackendFailuresStayOnTheRequest(t *testing.T) {
	upstream := &voiceUpstream{models: []string{"gpt-5-3-mini"}, fail: true}
	a, _ := newTestAdapter(t, upstream.serve(t))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	_, ttsErr := a.SynthesizeSpeech(context.Background(), provider.TTSRequest{Credential: credential, Text: "hi"})
	_, sttErr := a.TranscribeSpeech(context.Background(), provider.STTRequest{Credential: credential, FileName: "a.mp3", FileData: []byte("MP3")})
	for name, err := range map[string]error{"tts": ttsErr, "stt": sttErr} {
		message, _ := provider.ErrorPublicMessage(err)
		if provider.ErrorHTTPStatusOrZero(err) != http.StatusBadGateway || !provider.IsRequestScopedError(err) || !strings.Contains(message, "API") {
			t.Fatalf("%s: err = %v, message = %q", name, err, message)
		}
		var diagnostic provider.UpstreamDiagnosticError
		if !errors.As(err, &diagnostic) {
			t.Fatalf("%s lost the upstream diagnostic: %v", name, err)
		}
	}
	if !strings.HasPrefix(upstream.hidden, "11111111-2222-3333-4444-555555555555 ") {
		t.Fatalf("failed speech conversation left visible: %q", upstream.hidden)
	}
}

func TestVoiceListIsCachedPerAccount(t *testing.T) {
	upstream := &voiceUpstream{models: []string{"gpt-5-3-mini"}}
	a, _ := newTestAdapter(t, upstream.serve(t))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	for range 2 {
		if _, err := a.SynthesizeSpeech(context.Background(), provider.TTSRequest{Credential: credential, Text: "hi", VoiceID: "Sol"}); err != nil {
			t.Fatal(err)
		}
	}
	if upstream.voiceLists != 1 || !strings.HasSuffix(upstream.synthesize, "voice=glimmer") {
		t.Fatalf("voice lists = %d, synthesize = %s", upstream.voiceLists, upstream.synthesize)
	}
}
