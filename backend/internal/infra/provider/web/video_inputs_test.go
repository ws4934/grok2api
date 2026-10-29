package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestWebVideoImagePayloadsMatchCapturedBrowserRequests(t *testing.T) {
	// Browser capture on a logged-in free account, 2026-10-29. All generation
	// POSTs were intercepted before sending; this is protocol, not output QA.
	for _, tc := range []struct {
		name, first, kind string
		refs              []string
		want              string
	}{
		{"first frame", "first", "imageToVideo", nil, `{"aspectRatio":"16:9","duration":6,"inputAssets":["first"],"mode":"custom","prompt":"test","resolutionName":"480p"}`},
		{"single reference", "", "imageToVideo", []string{"ref"}, `{"aspectRatio":"16:9","duration":6,"inputAssets":["ref"],"mode":"custom","prompt":"test","resolutionName":"480p","useFirstFrame":false}`},
		{"multiple references", "", "referenceToVideo", []string{"ref-1", "ref-2"}, `{"aspectRatio":"16:9","duration":6,"inputAssets":["ref-1","ref-2"],"prompt":"test","resolutionName":"480p"}`},
		{"first frame plus reference", "first", "referenceToVideo", []string{"ref"}, `{"aspectRatio":"16:9","duration":6,"firstFrameAsset":"first","inputAssets":["ref"],"prompt":"test","resolutionName":"480p"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := videoCreatePayload("test", "16:9", "480p", 6, tc.first, tc.refs)
			input := payload["mediaGenInput"].(map[string]any)
			got, _ := json.Marshal(input[tc.kind])
			if len(input) != 1 || string(got) != tc.want {
				t.Fatalf("input=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestWebVideoUploadsStagedFirstFrameAndReferencesOnSubmittingAccount(t *testing.T) {
	const encodedPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	png, _ := base64.StdEncoding.DecodeString(encodedPNG)
	var uploads, creates atomic.Int32
	payloads := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/http/upload-file-v2/direct":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer r.MultipartForm.RemoveAll()
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			data, _ := io.ReadAll(file)
			_ = file.Close()
			if !bytes.Equal(data, png) || header.Header.Get("Content-Type") != "image/png" || r.FormValue("file_source") != imagineSelfUploadSource || !strings.HasSuffix(r.Referer(), "/imagine") {
				t.Errorf("invalid upload: type=%s source=%s referer=%s", header.Header.Get("Content-Type"), r.FormValue("file_source"), r.Referer())
			}
			id := uploads.Add(1)
			_, _ = fmt.Fprintf(w, `{"uploadId":"not-an-asset","fileMetadata":{"fileMetadataId":"asset-%d","fileUri":"users/test/asset-%d/content"}}`, id, id)
		case "/rest/app-chat/conversations/new":
			creates.Add(1)
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			payloads <- payload
			_, _ = io.WriteString(w, `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"users/test/generated/new/generated_video.mp4"}}}}`)
		default:
			t.Errorf("unexpected retired or guessed endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	a, credential := testMediaAdapter(t, server.URL)
	a.videoNow = sequentialVideoTestClock()
	credential.WebTier = account.WebTierBasic
	first := media.InputReference("input_" + strings.Repeat("A", 32))
	var opened atomic.Int32
	ctx := provider.WithInputOpener(context.Background(), func(_ context.Context, ref string) (provider.InputFile, error) {
		if ref != first {
			return provider.InputFile{}, provider.ErrInputUnavailable
		}
		opened.Add(1)
		return provider.InputFile{Body: io.NopCloser(bytes.NewReader(png)), MIMEType: "image/png", Size: int64(len(png))}, nil
	})
	refs := make([]string, maxWebVideoReferences)
	for i := range refs {
		refs[i] = "data:image/png;base64," + encodedPNG
	}
	var saved []string
	result, err := a.GenerateVideo(ctx, provider.VideoRequest{
		Credential: credential, Prompt: "test", Duration: 15, Resolution: "720p", ImageURL: first, ReferenceURLs: refs,
		SaveCheckpoint: func(value string) error { saved = append(saved, value); return nil },
	})
	if err != nil || result.URL == "" || result.Seconds != 6 || uploads.Load() != 8 || creates.Load() != 1 || opened.Load() != 1 || len(saved) != 2 {
		t.Fatalf("result=%+v err=%v uploads=%d creates=%d opened=%d saved=%v", result, err, uploads.Load(), creates.Load(), opened.Load(), saved)
	}
	payload := <-payloads
	input := payload["mediaGenInput"].(map[string]any)["referenceToVideo"].(map[string]any)
	want := []any{"asset-2", "asset-3", "asset-4", "asset-5", "asset-6", "asset-7", "asset-8"}
	if input["firstFrameAsset"] != "asset-1" || !reflect.DeepEqual(input["inputAssets"], want) || input["duration"] != float64(6) {
		t.Fatalf("input=%+v", input)
	}
}

func TestWebVideoMissingInputNeverSubmits(t *testing.T) {
	a := &Adapter{}
	ref := media.InputReference("input_" + strings.Repeat("B", 32))
	_, _, err := a.prepareVideoImages(context.Background(), normalizedConfig(Config{}), nil, "", ref, nil)
	if !errors.Is(err, provider.ErrInputUnavailable) {
		t.Fatalf("missing staged input=%v", err)
	}
}

func TestWebVideoNormalizesReferencesWithoutDroppingFirstFrame(t *testing.T) {
	a := &Adapter{}
	options, err := a.NormalizeVideoRequest(provider.VideoRequestOptions{
		Operation: provider.VideoOperationGenerate, HasImage: true, ReferenceImages: 14, Duration: 15, Resolution: "1080p",
	})
	if err != nil || !options.HasImage || options.ReferenceImages != 7 || options.Duration != 10 || options.Resolution != "720p" {
		t.Fatalf("normalized=%+v err=%v", options, err)
	}
	if err := a.ValidateVideoRequest(options); err != nil {
		t.Fatal(err)
	}
	options.ReferenceImages = 8
	if err := a.ValidateVideoRequest(options); !errors.Is(err, provider.ErrVideoParameterInvalid) {
		t.Fatalf("oversized references accepted: %v", err)
	}
}

func TestWebVideoUploadWithoutAssetMetadataNeverCreates(t *testing.T) {
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/http/upload-file-v2/direct" {
			_, _ = io.WriteString(w, `{"uploadId":"upload-is-not-an-asset"}`)
			return
		}
		creates.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	a, credential := testMediaAdapter(t, server.URL)
	var saved []string
	_, err := a.GenerateVideo(context.Background(), provider.VideoRequest{Credential: credential, Duration: 6, ImageURL: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=", SaveCheckpoint: func(value string) error { saved = append(saved, value); return nil }})
	if stage, _ := provider.VideoErrorStage(err); err == nil || stage != provider.VideoStagePrepare || len(saved) != 0 || creates.Load() != 0 {
		t.Fatalf("upload task id used as asset: err=%v saved=%v creates=%d", err, saved, creates.Load())
	}
}
