package provider

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

type TTSOutputFormat struct {
	Codec      string
	SampleRate int
	BitRate    int
}

type TTSRequest struct {
	Credential               account.Credential
	Model                    string
	Text                     string
	VoiceID                  string
	Language                 string
	OutputFormat             TTSOutputFormat
	Speed                    float64
	OptimizeStreamingLatency int
	TextNormalization        bool
	WithTimestamps           bool
}

type TTSTimestampSpan struct {
	Start float64
	End   float64
}

type TTSTimestamps struct {
	GraphChars []string
	GraphTimes []TTSTimestampSpan
}

type TTSResult struct {
	Audio        []byte
	ContentType  string
	Duration     float64
	Base64Audio  string
	Timestamps   *TTSTimestamps
	JSONEnvelope bool
}

type STTRequest struct {
	Credential   account.Credential
	Model        string
	FileName     string
	FileMIME     string
	FileData     []byte
	URL          string
	AudioFormat  string
	SampleRate   string
	Language     string
	Format       bool
	Multichannel bool
	Channels     int
	Diarize      bool
	KeyTerms     []string
	FillerWords  bool
	VADThreshold *float64
}

type STTWord struct {
	Text    string
	Start   float64
	End     float64
	Speaker *int
}

type STTChannel struct {
	Index int
	Text  string
	Words []STTWord
}

type STTResult struct {
	Text     string
	Language string
	Duration float64
	Words    []STTWord
	Channels []STTChannel
	RawJSON  []byte
}

type VoiceInfo struct {
	VoiceID  string
	Name     string
	Language string
}

// TTSAdapter synthesizes speech audio for text prompts.
type TTSAdapter interface {
	Adapter
	SynthesizeSpeech(ctx context.Context, request TTSRequest) (TTSResult, error)
	ListTTSVoices(ctx context.Context, credential account.Credential) ([]VoiceInfo, error)
	GetTTSVoice(ctx context.Context, credential account.Credential, voiceID string) (VoiceInfo, error)
}

// STTAdapter transcribes audio into text.
type STTAdapter interface {
	Adapter
	TranscribeSpeech(ctx context.Context, request STTRequest) (STTResult, error)
}

// VoiceWebSocketConn is a minimal duplex websocket used by voice streaming proxies.
type VoiceWebSocketConn interface {
	ReadMessage() (messageType int, data []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetReadLimit(limit int64)
	Close() error
}

// VoiceWebSocketRequest dials an upstream voice websocket with provider auth.
type VoiceWebSocketRequest struct {
	Credential account.Credential
	// Path is a v1-relative path such as /realtime or /stt.
	Path  string
	Model string
}

// VoiceWebSocketAdapter dials official voice websocket endpoints with account auth.
type VoiceWebSocketAdapter interface {
	Adapter
	DialVoiceWebSocket(ctx context.Context, request VoiceWebSocketRequest) (VoiceWebSocketConn, func(), error)
}

func (r *Registry) TTS(value account.Provider) (TTSAdapter, bool) {
	return adapterAs[TTSAdapter](r, value)
}

func (r *Registry) STT(value account.Provider) (STTAdapter, bool) {
	return adapterAs[STTAdapter](r, value)
}

func (r *Registry) VoiceWebSocket(value account.Provider) (VoiceWebSocketAdapter, bool) {
	return adapterAs[VoiceWebSocketAdapter](r, value)
}
