package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
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

func dataURI(raw []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

// loadAllocated returns what loadInput allocated, in bytes.
func loadAllocated(t *testing.T, uri string) (OpenAIImageInput, uint64, error) {
	t.Helper()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	input, err := (&Adapter{}).loadInput(context.Background(), uri)
	runtime.ReadMemStats(&after)
	return input, after.TotalAlloc - before.TotalAlloc, err
}

func TestLoadInputChecksDimensionsWithoutDecodingPixels(t *testing.T) {
	// 25 MP of 8-bit gray: decoding it would allocate 25 MB.
	var encoded bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&encoded, image.NewGray(image.Rect(0, 0, 5000, 5000))); err != nil {
		t.Fatal(err)
	}
	raw := encoded.Bytes()
	input, allocated, err := loadAllocated(t, dataURI(raw))
	if err != nil || input.MIME != "image/png" || allocated > 8<<20 {
		t.Fatalf("at the cap: mime=%q err=%v allocated=%d MiB", input.MIME, err, allocated>>20)
	}
	// The same file declaring one more column is above the cap.
	larger := bytes.Clone(raw)
	binary.BigEndian.PutUint32(larger[16:20], 5001)
	binary.BigEndian.PutUint32(larger[29:33], crc32.ChecksumIEEE(larger[12:29]))
	if _, allocated, err = loadAllocated(t, dataURI(larger)); err == nil || !strings.Contains(err.Error(), "25 megapixels") || allocated > 8<<20 {
		t.Fatalf("above the cap: err=%v allocated=%d MiB", err, allocated>>20)
	}
}

func TestImageJobScopePreparesInputsOnce(t *testing.T) {
	a := &Adapter{}
	job := imageJob{inputs: []string{dataURI(maskFixture(t, 2, 2, 255))}, mask: dataURI(maskFixture(t, 2, 2, 0))}
	ctx := a.WithImageJobScope(context.Background())
	inputs, mask, err := a.prepareImage(ctx, job)
	if err != nil || len(inputs) != 1 || mask == nil {
		t.Fatalf("prepare = %v %v %v", inputs, mask, err)
	}
	again, maskAgain, err := a.prepareImage(ctx, job)
	if err != nil || &again[0] != &inputs[0] || maskAgain != mask {
		t.Fatal("the second image of the job loaded its inputs again")
	}
	if fresh, _, _ := a.prepareImage(context.Background(), job); &fresh[0] == &inputs[0] {
		t.Fatal("inputs leaked out of their job scope")
	}
}

// A staged job input loads like a data URI, and a job whose upstream accepted
// it forgets its cached inputs.
func TestStagedInputLoadsAndAcceptedJobForgetsIt(t *testing.T) {
	a := &Adapter{}
	raw := maskFixture(t, 2, 2, 255)
	reference := media.InputReference("input_" + strings.Repeat("A", 32))
	ctx := provider.WithInputOpener(a.WithImageJobScope(context.Background()), func(context.Context, string) (provider.InputFile, error) {
		return provider.InputFile{Body: io.NopCloser(bytes.NewReader(raw)), MIMEType: "image/png", Size: int64(len(raw))}, nil
	})
	job := imageJob{inputs: []string{reference}}
	inputs, _, err := a.prepareImage(ctx, job)
	if err != nil || len(inputs) != 1 || inputs[0].MIME != "image/png" || !bytes.Equal(inputs[0].Data, raw) {
		t.Fatalf("prepare = %v %v", inputs, err)
	}
	forgetPreparedImage(ctx)
	if again, _, err := a.prepareImage(ctx, job); err != nil || &again[0] == &inputs[0] {
		t.Fatalf("the accepted job kept its inputs: err = %v", err)
	}
}

func TestImageOutputErrorTellsCallersNotToFetchAgain(t *testing.T) {
	var opaque bytes.Buffer
	if err := png.Encode(&opaque, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	_, _, err := (ImageOutputOptions{Background: "transparent"}).applyDecoded(downloadedImage{raw: opaque.Bytes(), mime: "image/png"})
	var marked interface{ ImageOutputFailure() bool }
	if !errors.As(err, &marked) || !marked.ImageOutputFailure() {
		t.Fatalf("err = %v", err)
	}
}

// The reason a generated image fails its output options reaches clients, in
// JSON and in a stream's error event.
func TestImageOutputReasonIsPublic(t *testing.T) {
	for _, test := range []struct {
		name  string
		alpha uint8
		want  string
	}{
		{"opaque", 255, "no transparent pixels"},
		{"empty", 0, "fully transparent"},
	} {
		canvas := image.NewNRGBA(image.Rect(0, 0, 2, 2))
		for i := range canvas.Pix {
			if i%4 == 3 {
				canvas.Pix[i] = test.alpha
			}
		}
		_, _, err := (ImageOutputOptions{Background: "transparent"}).applyDecoded(downloadedImage{decoded: canvas, mime: "image/png"})
		err = provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err)
		message, ok := provider.ErrorPublicMessage(err)
		if !ok || !strings.Contains(message, test.want) || strings.Contains(message, "HTTP 502") {
			t.Fatalf("%s: public message = %q, %v", test.name, message, ok)
		}
		if code, streamed := publicFailure(err); code != "media_postprocessing_failed" || streamed != message {
			t.Fatalf("%s: stream error = %s %q", test.name, code, streamed)
		}
	}
}

func TestApplyUsesTheDownloadDecodedPixels(t *testing.T) {
	// raw is not decodable: the conversion must use the pixels decoded on download.
	download := downloadedImage{raw: []byte("already decoded"), mime: "image/png", decoded: image.NewNRGBA(image.Rect(0, 0, 2, 2))}
	raw, mime, err := (ImageOutputOptions{OutputFormat: "jpeg"}).applyDecoded(download)
	if err != nil || mime != "image/jpeg" {
		t.Fatalf("apply = %q %v", mime, err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
}
