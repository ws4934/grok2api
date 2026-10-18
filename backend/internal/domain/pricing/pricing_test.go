package pricing

import (
	"strings"
	"testing"
)

func TestParseAndFormatUSDAreExact(t *testing.T) {
	cases := []struct {
		component Component
		value     string
		ticks     int64
		formatted string
	}{
		{InputToken, "2", 20_000, "2"},
		{InputToken, "0.075", 750, "0.075"},
		{InputToken, "1e-4", 1, "0.0001"},
		{InputToken, " 10000 ", 100_000_000, "10000"},
		{Character, "15", 150_000, "15"},
		{OutputImage, "0.04", 400_000_000, "0.04"},
		{OutputSecond, "0.0000000001", 1, "0.0000000001"},
		{OutputSecond, "1000", 10_000_000_000_000, "1000"},
		{InputImage, "0", 0, "0"},
	}
	for _, tc := range cases {
		ticks, err := ParseUSD(tc.component, tc.value)
		if err != nil || ticks != tc.ticks {
			t.Fatalf("ParseUSD(%s, %q) = %d, %v; want %d", tc.component, tc.value, ticks, err, tc.ticks)
		}
		if got := FormatUSD(tc.component, ticks); got != tc.formatted {
			t.Fatalf("FormatUSD(%s, %d) = %q, want %q", tc.component, ticks, got, tc.formatted)
		}
	}
	for _, tc := range []struct {
		component Component
		value     string
	}{
		{InputToken, "0.00001"},        // 0.1 tick per token
		{OutputImage, "1e-11"},         // 0.1 tick per image
		{InputToken, "-1"},             // negative
		{InputToken, "10000.0001"},     // above $10,000 per million
		{OutputSecond, "1000.0000001"}, // above $1,000 per second
		{OutputToken, ""},
		{OutputToken, "abc"},
		{OutputToken, "NaN"},
	} {
		if _, err := ParseUSD(tc.component, tc.value); err == nil {
			t.Fatalf("ParseUSD(%s, %q) accepted", tc.component, tc.value)
		}
	}
}

func TestNewPriceValidatesOperationComponents(t *testing.T) {
	text, err := NewPrice("chatgpt", "gpt-5", OperationText, map[Component]string{InputToken: "1.25", OutputToken: "10"})
	if err != nil {
		t.Fatal(err)
	}
	if text.InputToken != 12_500 || text.CachedInputToken != 12_500 || text.OutputToken != 100_000 {
		t.Fatalf("text rates = %#v, cached input must default to the input rate", text.Rates)
	}
	video, err := NewPrice("chatgpt", "sora", OperationVideo, map[Component]string{OutputSecond: "0.1"})
	if err != nil || video.OutputSecond != 1_000_000_000 || video.InputImage != 0 {
		t.Fatalf("video = %#v, %v", video.Rates, err)
	}
	for name, tc := range map[string]struct {
		operation Operation
		values    map[Component]string
		message   string
	}{
		"missing output":    {OperationText, map[Component]string{InputToken: "1"}, "缺少"},
		"irrelevant image":  {OperationText, map[Component]string{InputToken: "1", OutputToken: "1", OutputImage: "1"}, "不适用"},
		"edit needs input":  {OperationImageEdit, map[Component]string{OutputImage: "0.04"}, "缺少"},
		"unknown operation": {Operation("audio"), map[Component]string{Character: "1"}, "未知"},
		"imprecise rate":    {OperationTTS, map[Component]string{Character: "0.00001"}, "精度"},
		"stt uses seconds":  {OperationSTT, map[Component]string{Character: "1"}, "不适用"},
	} {
		if _, err := NewPrice("chatgpt", "m", tc.operation, tc.values); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.message)
		}
	}
}

func TestPriceBreakdownAndQuotes(t *testing.T) {
	text := Price{ID: 7, PricingModel: "gpt-5", Operation: OperationText, Rates: Rates{InputToken: 12_500, CachedInputToken: 1_250, OutputToken: 100_000}}
	breakdown, ok := text.Breakdown(1000, 200, 300, 4, 0, 0)
	want := int64(800*12_500 + 200*1_250 + 300*100_000)
	if !ok || breakdown.CostInUSDTicks != want || len(breakdown.Components) != 3 || breakdown.Model != "gpt-5" {
		t.Fatalf("text breakdown = %#v", breakdown)
	}
	quote, ok := text.Quote(breakdown, ok)
	if !ok || quote.CostInUSDTicks != want || quote.PriceID != 7 || quote.Model != "gpt-5" {
		t.Fatalf("text quote = %#v", quote)
	}

	edit := Price{ID: 8, PricingModel: "gpt-image-1", Operation: OperationImageEdit, Rates: Rates{InputImage: 100, OutputImage: 1_000}}
	if b, ok := edit.Breakdown(0, 0, 0, 2, 3, 0); !ok || b.CostInUSDTicks != 3_200 {
		t.Fatalf("edit breakdown = %#v", b)
	}
	if _, ok := edit.Breakdown(0, 0, 0, 2, 0, 0); ok {
		t.Fatal("edit without output images priced")
	}
	video := Price{Operation: OperationVideo, Rates: Rates{InputImage: 5, OutputSecond: 100}}
	if b, ok := video.Breakdown(0, 0, 0, 1, 0, 6); !ok || b.CostInUSDTicks != 605 {
		t.Fatalf("video breakdown = %#v", b)
	}
	image := Price{Operation: OperationImage, Rates: Rates{OutputImage: 9}}
	if b, ok := image.Breakdown(0, 0, 0, 0, 2, 0); !ok || b.CostInUSDTicks != 18 {
		t.Fatalf("image breakdown = %#v", b)
	}

	tts := Price{ID: 9, Operation: OperationTTS, Rates: Rates{Character: 150_000}}
	if q, ok := tts.TTSCost("Hello 世界"); !ok || q.CostInUSDTicks != 8*150_000 || q.PriceID != 9 {
		t.Fatalf("tts quote = %#v", q)
	}
	if _, ok := tts.Breakdown(0, 0, 0, 0, 0, 0); ok {
		t.Fatal("tts quantities are not stored on audits and must not reconstruct")
	}
	stt := Price{Operation: OperationSTT, Rates: Rates{OutputSecond: 3}}
	if q, ok := stt.STTCost(2.5); !ok || q.CostInUSDTicks != 8 {
		t.Fatalf("stt quote = %#v", q)
	}
	if q, ok := (Price{Operation: OperationSTT}).STTCost(2.5); !ok || q.CostInUSDTicks != 0 {
		t.Fatalf("free stt quote = %#v", q)
	}
}
