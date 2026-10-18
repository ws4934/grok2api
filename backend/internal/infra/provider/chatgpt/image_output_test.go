// Tests adapted from SkyKoo/gptGrok2api 1577309; see LICENSE.reference.
package chatgpt

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func TestImageOutputOptionsValidateAndPrompt(t *testing.T) {
	for _, background := range []string{"", "auto", "transparent", "opaque"} {
		options := ImageOutputOptions{Background: background, OutputFormat: "png"}
		if err := options.Validate(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(options.promptSuffix()), "remove") {
			t.Fatal("output options became a removal operation")
		}
	}
	for _, options := range []ImageOutputOptions{{Background: "clear"}, {Background: "transparent", OutputFormat: "jpeg"}, {OutputFormat: "jpg"}, {OutputFormat: "gif"}} {
		var param *ImageOptionError
		if !errors.As(options.Validate(), &param) {
			t.Fatalf("accepted unsupported options: %+v", options)
		}
	}
	if (ImageOutputOptions{}).promptSuffix() != "" || (ImageOutputOptions{Background: "auto"}).promptSuffix() != "" {
		t.Fatal("default prompts changed")
	}
	if !strings.Contains((ImageOutputOptions{Background: "transparent"}).promptSuffix(), "alpha=0") {
		t.Fatal("missing real alpha requirement")
	}
}

func TestImageOutputChecksPixelsAndPreservesPNG(t *testing.T) {
	for _, tc := range []struct {
		name, background string
		alpha            []uint8
		wantError        bool
	}{
		{"transparent", "transparent", []uint8{0, 127, 255, 255}, false},
		{"painted-background", "transparent", []uint8{255, 255, 255, 255}, true},
		{"almost-opaque", "transparent", []uint8{254, 254, 254, 254}, true},
		{"empty", "transparent", []uint8{0, 0, 0, 0}, true},
		{"opaque", "opaque", []uint8{255, 255, 255, 255}, false},
		{"opaque-mismatch", "opaque", []uint8{0, 127, 255, 255}, true},
		{"auto-preserves-alpha", "auto", []uint8{0, 127, 255, 255}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canvas := image.NewNRGBA(image.Rect(0, 0, 2, 2))
			for i, alpha := range tc.alpha {
				canvas.SetNRGBA(i%2, i/2, color.NRGBA{R: 200, A: alpha})
			}
			var original bytes.Buffer
			if err := png.Encode(&original, canvas); err != nil {
				t.Fatal(err)
			}
			raw, mime, err := (ImageOutputOptions{Background: tc.background, OutputFormat: "png"}).apply(original.Bytes(), "image/png")
			if tc.wantError {
				if !IsImageOutputError(err) || raw != nil || openAIImageProxyFailure(err) {
					t.Fatalf("incorrect output failure: %v", err)
				}
				return
			}
			if err != nil || mime != "image/png" || !bytes.Equal(raw, original.Bytes()) {
				t.Fatalf("PNG bytes or alpha changed: %v", err)
			}
		})
	}
}

func TestImageOutputDefaultsToPNG(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			canvas.Set(x, y, color.RGBA{R: 200, A: 255})
		}
	}
	var source bytes.Buffer
	if err := jpeg.Encode(&source, canvas, nil); err != nil {
		t.Fatal(err)
	}
	_, mime, err := (ImageOutputOptions{}).apply(source.Bytes(), "image/jpeg")
	if err != nil || mime != "image/png" {
		t.Fatalf("default must encode PNG: %s %v", mime, err)
	}
	raw, mime, err := (ImageOutputOptions{OutputFormat: "png"}).apply(source.Bytes(), "image/jpeg")
	if err != nil || mime != "image/png" {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil || decoded.Bounds() != canvas.Bounds() {
		t.Fatalf("invalid PNG: %v", err)
	}
}

func TestImageOutputFormatRoundTrips(t *testing.T) {
	// Large uniform patches expose JPEG alpha-flattening errors without relying
	// on exact lossy RGB values at chroma-subsampled edges.
	source := image.NewNRGBA(image.Rect(0, 0, 48, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 48; x++ {
			alpha := uint8(255)
			if x < 16 {
				alpha = 0
			} else if x < 32 {
				alpha = 128
			}
			source.SetNRGBA(x, y, color.NRGBA{R: 255, A: alpha})
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, source); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"png", "jpeg", "webp"} {
		t.Run(format, func(t *testing.T) {
			background := "transparent"
			if format == "jpeg" {
				background = "auto"
			}
			options := ImageOutputOptions{Background: background, OutputFormat: format}
			if err := options.Validate(); err != nil {
				t.Fatal(err)
			}
			raw, mime, err := options.apply(input.Bytes(), "image/png")
			if err != nil || mime != "image/"+format {
				t.Fatalf("encode: %s %v", mime, err)
			}
			verifiedMIME, err := validatedImageMIME(raw)
			if err != nil || verifiedMIME != mime {
				t.Fatalf("MIME does not match actual bytes: %v", err)
			}
			decoded, actual, err := image.Decode(bytes.NewReader(raw))
			if err != nil || actual != format || decoded.Bounds() != source.Bounds() {
				t.Fatalf("decode: %s %v", actual, err)
			}
			for y := 0; y < 32; y++ {
				for x := 0; x < 48; x++ {
					want := color.NRGBAModel.Convert(source.At(x, y)).(color.NRGBA)
					got := color.NRGBAModel.Convert(decoded.At(x, y)).(color.NRGBA)
					if format == "jpeg" {
						if got.A != 255 {
							t.Fatal("JPEG retained alpha")
						}
					} else if got.A != want.A || (want.A > 0 && got != want) {
						t.Fatalf("lossless colors/alpha changed at %d,%d: %v != %v", x, y, got, want)
					}
				}
			}
			if format == "jpeg" {
				white := color.NRGBAModel.Convert(decoded.At(8, 16)).(color.NRGBA)
				edge := color.NRGBAModel.Convert(decoded.At(24, 16)).(color.NRGBA)
				if white.R < 250 || white.G < 250 || white.B < 250 {
					t.Fatalf("transparent area turned dark: %v", white)
				}
				if edge.R < 250 || edge.G < 122 || edge.G > 132 || edge.B < 122 || edge.B > 132 {
					t.Fatalf("incorrect alpha compositing: %v", edge)
				}
			}
		})
	}
}

func TestImageOutputWebPEdgeLimit(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 16385, 1))
	var input bytes.Buffer
	if err := png.Encode(&input, source); err != nil {
		t.Fatal(err)
	}
	_, _, err := (ImageOutputOptions{OutputFormat: "webp"}).apply(input.Bytes(), "image/png")
	if !IsImageOutputError(err) {
		t.Fatalf("WebP dimension overflow was not rejected: %v", err)
	}
}
