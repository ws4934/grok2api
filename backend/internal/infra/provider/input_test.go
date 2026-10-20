package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func stagedInputs(files map[string]string) InputOpener {
	return func(_ context.Context, reference string) (InputFile, error) {
		content, ok := files[reference]
		if !ok {
			return InputFile{}, ErrInputUnavailable
		}
		return InputFile{Body: io.NopCloser(strings.NewReader(content)), MIMEType: "image/png", Size: int64(len(content))}, nil
	}
}

func TestMaterializeInputsReplacesStagedReferencesAndHoldsASlotUntilRelease(t *testing.T) {
	reference := media.InputReference("input_" + strings.Repeat("A", 32))
	ctx := WithInputOpener(context.Background(), stagedInputs(map[string]string{reference: "png"}))
	body := map[string]any{
		"prompt":           "a fox",
		"image":            map[string]any{"url": reference},
		"reference_images": []map[string]any{{"url": "https://example.com/a.png"}, {"url": reference}},
		"tags":             []any{reference, 3},
	}
	release, err := MaterializeInputs(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("png"))
	references := body["reference_images"].([]map[string]any)
	if body["image"].(map[string]any)["url"] != want || references[0]["url"] != "https://example.com/a.png" || references[1]["url"] != want || body["tags"].([]any)[0] != want || body["prompt"] != "a fox" {
		t.Fatalf("materialized body = %#v", body)
	}
	if len(inputSlots) != 1 {
		t.Fatalf("held slots = %d, want 1 for the whole body", len(inputSlots))
	}
	release()
	release()
	if len(inputSlots) != 0 {
		t.Fatalf("held slots after release = %d", len(inputSlots))
	}

	plain := map[string]any{"image": map[string]any{"url": "https://example.com/a.png"}}
	if release, err := MaterializeInputs(ctx, plain); err != nil || len(inputSlots) != 0 {
		t.Fatalf("body without staged inputs: err = %v, held slots = %d", err, len(inputSlots))
	} else {
		release()
	}

	missing := map[string]any{"image": map[string]any{"url": media.InputReference("input_" + strings.Repeat("B", 32))}}
	if _, err := MaterializeInputs(ctx, missing); !errors.Is(err, ErrInputUnavailable) || len(inputSlots) != 0 {
		t.Fatalf("missing input: err = %v, held slots = %d", err, len(inputSlots))
	}
}

func TestHoldInputsWaitsForAFreeSlot(t *testing.T) {
	var releases []func()
	for range cap(inputSlots) {
		release, err := HoldInputs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := HoldInputs(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("hold with every slot taken = %v, want context.Canceled", err)
	}
	for _, release := range releases {
		release()
	}
	if len(inputSlots) != 0 {
		t.Fatalf("held slots = %d", len(inputSlots))
	}
}

func TestReadInputRejectsMissingOpenerAndShortFiles(t *testing.T) {
	reference := media.InputReference("input_" + strings.Repeat("A", 32))
	if _, _, err := ReadInput(context.Background(), reference); !errors.Is(err, ErrInputUnavailable) {
		t.Fatalf("read without opener = %v", err)
	}
	short := WithInputOpener(context.Background(), func(context.Context, string) (InputFile, error) {
		return InputFile{Body: io.NopCloser(strings.NewReader("ab")), Size: 3}, nil
	})
	if _, _, err := ReadInput(short, reference); !errors.Is(err, ErrInputUnavailable) {
		t.Fatalf("short file = %v", err)
	}
}
