package audit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
)

func TestParameterDTOUsesTextWithoutRoundingLargeNumbers(t *testing.T) {
	_, parameters := requestparams.Start(context.Background(), []byte(`{"seed":999999999999999999,"n":5}`))
	parameters.SetEffective(map[string]any{"n": 2})
	dto := newRequestParametersResponse(parameters)
	if dto == nil || dto.Client == nil || !strings.Contains(dto.Client.JSON, `999999999999999999`) {
		t.Fatalf("seed rounded: %+v", dto)
	}
	encoded, err := json.Marshal(dto)
	if err != nil || !strings.Contains(string(encoded), `"json":"{\n`) {
		t.Fatalf("JSON was not encoded as diagnostic text: %s %v", encoded, err)
	}
}
