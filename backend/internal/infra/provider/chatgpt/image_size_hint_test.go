package chatgpt

import (
	"strings"
	"testing"
)

// ChatGPT Web follows the shape stated in the prompt, not the size field.
func TestImagePromptStatesTheRequestedShape(t *testing.T) {
	for size, want := range map[string]string{
		"1024x1536": "\nOutput requirement: 1024x1536 pixels (portrait, aspect ratio 2:3).",
		"1536x1024": "\nOutput requirement: 1536x1024 pixels (landscape, aspect ratio 3:2).",
		"1024x1024": "\nOutput requirement: 1024x1024 pixels (square, aspect ratio 1:1).",
		"auto":      "",
		"":          "",
	} {
		suffix := (ImageOutputOptions{Size: size}).promptSuffix()
		if want == "" && suffix != "" || want != "" && !strings.HasPrefix(suffix, want) {
			t.Fatalf("size %q: suffix = %q", size, suffix)
		}
	}
	if suffix := (ImageOutputOptions{Size: "1024x1024", Background: "transparent"}).promptSuffix(); !strings.Contains(suffix, "square") || !strings.Contains(suffix, "transparent background") {
		t.Fatalf("combined suffix = %q", suffix)
	}
}
