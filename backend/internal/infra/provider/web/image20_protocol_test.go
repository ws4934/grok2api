package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestImage20EditUploadsFiveReferencesThroughTheNativeFlow(t *testing.T) {
	var uploads atomic.Int32
	var generated atomic.Int32
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
		switch r.URL.Path {
		case "/http/upload-file-v2/direct":
			id := uploads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"fileMetadata":{"fileMetadataId":"asset-%d","fileUri":"users/test/ref/%d/content"}}`, id, id)
		case "/rest/app-chat/conversations/new":
			generated.Add(1)
			var got map[string]any
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			want := buildImage20EditPayload("combine", []string{"asset-1", "asset-2", "asset-3", "asset-4", "asset-5"}, "21:9")
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("native request = %s, want %s", gotJSON, wantJSON)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {}\n\n")
		default:
			fhttp.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, credential := testMediaAdapter(t, server.URL)
	credential.WebTier = account.WebTierBasic
	inputs := make([]string, 5)
	for i := range inputs {
		inputs[i] = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	}
	response, err := adapter.EditImage(context.Background(), provider.ImageEditRequest{Credential: credential, Model: "grok-imagine-image-2.0", Prompt: "combine", ImageURLs: inputs, Count: 1, AspectRatio: "21:9", Resolution: "2k", Quality: "high", Streaming: true})
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	_ = response.Body.Close()
	if uploads.Load() != 5 || generated.Load() != 1 {
		t.Fatalf("uploads=%d generations=%d", uploads.Load(), generated.Load())
	}
}

func TestImage20EditBuilderMatchesCurrentOfficialSource(t *testing.T) {
	// Source-derived contract (2026-10-28), not a live generation response:
	// cdn.grok.com/_next/static/chunks/01ovajz11efdy.js, eS and er.k.
	assets := []string{"asset-1", "asset-2", "asset-3", "asset-4", "asset-5"}
	payload := buildImage20EditPayload("组合这五张图", assets, "21:9")
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"enableSideBySide":false,"mediaGenInput":{"imageToImage":{"aspectRatio":"21:9","inputAssets":["asset-1","asset-2","asset-3","asset-4","asset-5"],"numOfImages":1,"prompt":"组合这五张图"}},"message":"组合这五张图","modelName":"imagine-image-edit","responseMetadata":{"modelConfigOverride":{"modelMap":{"imageEditModel":"imagine"}}},"sendFinalMetadata":true}`
	if string(encoded) != want {
		t.Fatalf("request = %s", encoded)
	}
	withoutRatio := buildImage20EditPayload("edit", assets[:1], "")
	if _, exists := withoutRatio["mediaGenInput"].(map[string]any)["imageToImage"].(map[string]any)["aspectRatio"]; exists {
		t.Fatal("omitted ratio must preserve the source shape")
	}
}

func TestImage20UsesCurrentQualityRatiosWithoutChangingOtherWebModels(t *testing.T) {
	for _, ratio := range webImage20Ratios {
		if got := resolveImage20AspectRatio(ratio, ""); got != ratio {
			t.Fatalf("ratio %s mapped to %s", ratio, got)
		}
	}
	if got := resolveImage20AspectRatio("3:4", ""); got != "2:3" {
		t.Fatalf("unsupported 2.0 ratio = %s", got)
	}
	if got := resolveImageAspectRatio("3:4", ""); got != "3:4" {
		t.Fatalf("existing Web contract changed to %s", got)
	}
	message := imagineRequestMessage("test", "prompt", "auto", false, true, 1)
	properties := message["item"].(map[string]any)["content"].([]any)[0].(map[string]any)["properties"].(map[string]any)
	if _, exists := properties["aspect_ratio"]; exists {
		t.Fatal("Quality auto aspect ratio must use the upstream default")
	}
}

func TestImage20SixReferencesAreRejectedBeforeUploading(t *testing.T) {
	response, err := (&Adapter{}).EditImage(context.Background(), provider.ImageEditRequest{Model: "grok-imagine-image-2.0", ImageURLs: make([]string, 6), Count: 1})
	if err != nil || response == nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if len(body) == 0 {
		t.Fatal("missing Chinese protocol error")
	}
}

func TestImage20QuotaModesKeepFreeAndPaidEditWindowsSeparate(t *testing.T) {
	adapter := &Adapter{}
	registry := provider.NewRegistry(adapter)
	if got := registry.ImageQuotaMode(account.ProviderWeb, "grok-imagine-image-2.0", false); got != account.QuotaModeWebImagePro {
		t.Fatalf("generation quota = %s", got)
	}
	mode := registry.ImageQuotaMode(account.ProviderWeb, "grok-imagine-image-2.0", true)
	if mode != account.QuotaModeWebImageEdit {
		t.Fatalf("edit quota = %s", mode)
	}
	for _, tier := range []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy} {
		want := account.QuotaModeWebImageEdit
		if tier == account.WebTierBasic {
			want = account.QuotaModeWebImagePro
		}
		if got := adapter.CandidateQuotaMode(account.RoutingCandidate{Credential: account.Credential{WebTier: tier}}, mode); got != want {
			t.Fatalf("tier=%s quota=%s", tier, got)
		}
	}
	models, err := adapter.ListModels(context.Background(), account.Credential{WebTier: account.WebTierBasic})
	count := 0
	for _, model := range models {
		if model == "grok-imagine-image-2.0" {
			count++
		}
	}
	if err != nil || count != 1 {
		t.Fatalf("duplicate account models: %v error=%v", models, err)
	}
}
