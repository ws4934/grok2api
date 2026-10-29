package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/gin-gonic/gin"
)

func TestRejectedUpstreamAPIKeyIsNotAClientParameterOrClientKeyError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, anthropic := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		err := screenshotModerationError{http.StatusBadRequest, `{"code":"invalid-argument","error":"Incorrect API key provided. You can obtain an API key from https://console.x.ai."}`}
		if anthropic {
			writeGatewayAnthropicError(c, err)
		} else {
			writeGatewayError(c, err)
		}
		var envelope map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		problem := envelope["error"].(map[string]any)
		wantType := "server_error"
		if anthropic {
			wantType = "overloaded_error"
			if envelope["type"] != "error" {
				t.Fatal(envelope)
			}
		} else if value, exists := problem["param"]; !exists || value != nil {
			t.Fatal(problem)
		}
		if recorder.Code != http.StatusServiceUnavailable || problem["type"] != wantType || problem["code"] != provider.APIKeyRejectedCode || problem["message"] != provider.APIKeyRejectedMessage || strings.Contains(recorder.Body.String(), "console.x.ai") {
			t.Fatalf("anthropic=%v status=%d body=%s", anthropic, recorder.Code, recorder.Body.String())
		}
	}
}

func TestVideoAPIKeyFailureUsesOfficialVideoErrorCode(t *testing.T) {
	job := mediadomain.Job{Status: mediadomain.StatusFailed, ErrorCode: provider.APIKeyRejectedCode, ErrorMessage: provider.APIKeyRejectedMessage}
	response := videoGenerationResponse(job)
	problem := response["error"].(gin.H)
	if response["status"] != "failed" || problem["code"] != "service_unavailable" || problem["message"] != provider.APIKeyRejectedMessage {
		t.Fatal(response)
	}
}
