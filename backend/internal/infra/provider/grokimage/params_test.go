package grokimage

import "testing"

func TestParamsMapToWhatGrokRenders(t *testing.T) {
	for in, want := range map[string]string{"": "", "auto": "", "1k": "1k", "2K": "2k", "4k": "2k", "1024": "1k", "2048": "2k", "1080p": "2k", "720p": "1k", "big": ""} {
		if got := Resolution(in); got != want {
			t.Errorf("Resolution(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"": "", "auto": "", "low": "low", " Medium ": "medium", "high": "medium", "hd": "medium"} {
		if got := Quality(in); got != want {
			t.Errorf("Quality(%q) = %q, want %q", in, got, want)
		}
	}
}
