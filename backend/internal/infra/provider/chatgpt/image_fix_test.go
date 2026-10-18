package chatgpt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUpstreamErrorTextStaysValidUTF8(t *testing.T) {
	// A Chinese refusal as ChatGPT returns it for a Chinese prompt: the 300-byte
	// excerpt cut lands inside a rune.
	reason := "抱歉，我无法生成这张图片，因为该请求可能违反了 OpenAI 的内容政策。" + strings.Repeat("请尝试修改描述后再试一次。", 6)
	value := map[string]any{"author": map[string]any{"role": "assistant"}, "metadata": map[string]any{"is_error": true}, "content": map[string]any{"content_type": "text", "parts": []any{reason}}}
	err := openAIImageTerminalError(value)
	if err == nil {
		t.Fatal("no terminal error")
	}
	if message := err.Error(); !utf8.ValidString(message) || !strings.HasSuffix(message, "…") {
		t.Fatalf("message = %q", message)
	}
	raw := &upstreamError{Status: 502, Message: "bad \xff\xfe body " + strings.Repeat("é", 200)}
	if message := raw.Error(); !utf8.ValidString(message) {
		t.Fatalf("invalid upstream bytes kept: %q", message)
	}
}
