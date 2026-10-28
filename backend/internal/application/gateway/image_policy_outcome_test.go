package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// WebSocket outcomes expose a safe message without inventing an upstream
// HTTP response or implementing UpstreamDiagnosticError.
type publicModerationError struct{}

func (publicModerationError) Error() string              { return "WebSocket moderated=true" }
func (publicModerationError) HTTPStatusCode() int        { return http.StatusBadRequest }
func (publicModerationError) RequestScopedFailure() bool { return true }
func (publicModerationError) PublicErrorMessage() string { return provider.ContentPolicyMessage }

const imagePolicyFailureEvent = `{"type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"内容未通过上游安全审核"}}`

type policyOutcomeImageAdapter struct {
	*asyncImageAdapter
	stream bool
	calls  int
}

func (a *policyOutcomeImageAdapter) GenerateImage(context.Context, provider.ImageGenerationRequest) (*provider.Response, error) {
	a.calls++
	if !a.stream {
		return nil, publicModerationError{}
	}
	return &provider.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: error\ndata: " + imagePolicyFailureEvent + "\n\n")), QuotaUnits: 1}, nil
}

func TestImagePolicyOutcomeKeepsDiagnosticsWithoutBillingOrAccountPenalty(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[stream], func(t *testing.T) {
			f := newImageJobFixture(t, 2)
			adapter := &policyOutcomeImageAdapter{asyncImageAdapter: f.adapter, stream: stream}
			f.service.providers = provider.NewRegistry(adapter)
			result, err := f.service.GenerateImage(context.Background(), ImageGenerationInput{RequestID: "image-policy-outcome", ClientKey: f.key, PublicModel: "gpt-image-2", Prompt: "image", Count: 1, Streaming: stream})
			wantStatus, wantCode := http.StatusBadRequest, provider.ContentPolicyViolation
			if !stream {
				var failure *UpstreamFailure
				if result != nil || !errors.As(err, &failure) || failure.HTTPStatus != wantStatus || failure.Code != wantCode || failure.PublicMessage != provider.ContentPolicyMessage || !failure.SafetyRejection {
					t.Fatalf("result=%+v failure=%+v err=%v", result, failure, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer result.Body.Close()
				if _, err := io.ReadAll(result.Body); err != nil {
					t.Fatal(err)
				}
				if result.RecordStreamFailure == nil {
					t.Fatal("image stream failure diagnostics are not connected")
				}
				result.RecordStreamFailure(StreamFailureDiagnostic{Body: []byte(imagePolicyFailureEvent)})
				result.Finalize(Usage{}, "", "upstream_stream_error")
				wantStatus, wantCode = http.StatusOK, "upstream_stream_error"
			}
			if adapter.calls != 1 {
				t.Fatalf("policy outcome regenerated %d times", adapter.calls)
			}
			if billed, reserved := f.billing(t); billed != 0 || reserved != 0 {
				t.Fatalf("billed=%d reserved=%d", billed, reserved)
			}
			for _, account := range f.accounts {
				view, err := f.service.accounts.Get(context.Background(), account.ID)
				if err != nil || view.Credential.FailureCount != 0 || view.Credential.AuthStatus != account.AuthStatus {
					t.Fatalf("account = %+v, %v", view, err)
				}
			}
			records, _, err := f.audits.List(context.Background(), 0, 10)
			if err != nil || len(records) != 1 {
				t.Fatalf("audit records = %+v, %v", records, err)
			}
			record, err := f.audits.Get(context.Background(), records[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if record.StatusCode != wantStatus || record.ErrorCode != wantCode || len(record.Attempts) != 1 {
				t.Fatalf("policy audit = %+v", record)
			}
			if stream && (record.Attempts[0].Stage != "response_stream" || !strings.Contains(string(record.Attempts[0].ResponseBody), provider.ContentPolicyViolation)) {
				t.Fatalf("image stream diagnostic lost: %+v", record.Attempts)
			}
		})
	}
}
