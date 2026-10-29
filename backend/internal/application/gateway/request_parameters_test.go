package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
)

func TestImageParametersKeepClientValuesAndActualBatches(t *testing.T) {
	f := newNativeImageFixture(t, 8, 12)
	ctx := requestparams.WithClient(context.Background(), []byte(`{"model":"public-alias","prompt":"lighthouse","n":5,"resolution":"4k","quality":"standard","api_key":"do-not-save"}`))
	result, err := f.service.GenerateImage(ctx, ImageGenerationInput{RequestID: "batch-parameters", ClientKey: f.key, PublicModel: "Console/grok-imagine-image-2.0", Prompt: "lighthouse", Count: 5, Quality: "low", Resolution: "1k"})
	if err != nil {
		t.Fatal(err)
	}
	readFanned(t, result)
	rows, _, err := f.audits.List(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[int]bool{}
	for _, row := range rows {
		if row.RequestParameters != nil {
			t.Fatal("list loaded parameter payload")
		}
		row, err = f.audits.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.RequestParameters == nil {
			t.Fatal("detail lost parameters")
		}
		data := row.RequestParameters.Data()
		var client, effective map[string]any
		_ = json.Unmarshal(data.Client.Values, &client)
		_ = json.Unmarshal(data.Effective.Values, &effective)
		if client["n"] != float64(5) || client["quality"] != "standard" || effective["quality"] != "low" || effective["resolution"] != "1k" {
			t.Fatalf("parameters mismatch: client=%s effective=%s", data.Client.Values, data.Effective.Values)
		}
		if row.StatusCode == 200 {
			counts[int(effective["n"].(float64))] = true
		}
		if strings.Contains(string(data.Client.Values), "do-not-save") {
			t.Fatal("API key was persisted")
		}
	}
	if !counts[2] || !counts[3] {
		t.Fatalf("audited counts=%v", counts)
	}
}

func TestAsyncImageParametersSurviveWorkerAndSettlementRecovery(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := requestparams.WithClient(context.Background(), []byte(`{"model":"gpt-image-2","input":"lighthouse","seed":999999999999999999,"tools":[{"type":"image_generation"}]}`))
	job, err := f.service.CreateImageJob(ctx, ImageJobInput{RequestID: "async-parameters", ClientKey: f.key, PublicModel: "gpt-image-2", Prompt: "lighthouse", Size: "1536x1024", Quality: "high"})
	if err != nil {
		t.Fatal(err)
	}
	stored := f.job(t, job.ID)
	if stored.RequestParameters == nil || !strings.Contains(string(stored.RequestParameters.Data().Client.Values), `999999999999999999`) {
		t.Fatal("durable job lost original parameters")
	}
	f.service.processMediaJob(context.Background(), job.ID)
	row := f.auditsByEvent(t)[imageEventID(job)]
	row, err = f.audits.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.RequestParameters == nil || !strings.Contains(string(row.RequestParameters.Data().Effective.Values), `"quality":"high"`) {
		t.Fatalf("settlement lost creation parameters: %+v", row)
	}
}
