package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type moderationDiagnosticError struct {
	status  int
	message string
}

func (e moderationDiagnosticError) Error() string       { return e.message }
func (e moderationDiagnosticError) HTTPStatusCode() int { return e.status }
func (e moderationDiagnosticError) UpstreamDiagnostic() (string, provider.DiagnosticResponse) {
	return "https://example.test/generate", provider.DiagnosticResponse{StatusCode: e.status, Body: []byte(e.message)}
}

func TestMediaModerationUsesRealStatusAndSafeChineseMessage(t *testing.T) {
	for _, tc := range []moderationDiagnosticError{
		{400, "Generated video rejected by content moderation."},
		{422, "We’re so sorry, but the image we created may violate our content policies."},
	} {
		failure := ProviderUpstreamFailure(tc, 1, "test")
		if failure == nil || failure.HTTPStatus != tc.status || failure.Code != provider.ContentPolicyViolation || failure.PublicMessage != provider.ContentPolicyMessage || !failure.SafetyRejection || failure.AccountScoped {
			t.Fatalf("failure = %+v", failure)
		}
		if !errors.Is(failure, tc) {
			t.Fatal("diagnostic cause was lost")
		}
	}
	private := ProviderUpstreamFailure(moderationDiagnosticError{422, "服务器凭据 secret-token 不可用"}, 1, "test")
	if strings.Contains(private.PublicMessage, "secret-token") {
		t.Fatalf("diagnostic-only body exposed: %s", private.PublicMessage)
	}
}

type moderationSyncImageAdapter struct {
	*asyncImageAdapter
	calls int
}

func (a *moderationSyncImageAdapter) GenerateImage(context.Context, provider.ImageGenerationRequest) (*provider.Response, error) {
	a.calls++
	return nil, moderationDiagnosticError{422, "OpenAI image generation stopped by upstream: We’re so sorry, but the image we created may violate our content policies."}
}

func TestSyncImageModerationReturns422WithoutAccountPenaltyOrRegeneration(t *testing.T) {
	f := newImageJobFixture(t, 2)
	adapter := &moderationSyncImageAdapter{asyncImageAdapter: f.adapter}
	f.service.providers = provider.NewRegistry(adapter)
	_, err := f.service.GenerateImage(context.Background(), ImageGenerationInput{RequestID: "sync-image-moderation", ClientKey: f.key, PublicModel: "gpt-image-2", Prompt: "image", Count: 1})
	var failure *UpstreamFailure
	if !errors.As(err, &failure) || failure.HTTPStatus != 422 || failure.Code != provider.ContentPolicyViolation || adapter.calls != 1 {
		t.Fatalf("failure = %+v, calls = %d, err = %v", failure, adapter.calls, err)
	}
	view, err := f.service.accounts.Get(context.Background(), f.accounts[0].ID)
	if err != nil || view.Credential.FailureCount != 0 || view.Credential.AuthStatus != f.accounts[0].AuthStatus {
		t.Fatalf("account = %+v, %v", view, err)
	}
	audits, _, err := f.audits.List(context.Background(), 0, 10)
	if err != nil || len(audits) != 1 || audits[0].StatusCode != 422 || audits[0].ErrorCode != provider.ContentPolicyViolation {
		t.Fatalf("audits = %+v, %v", audits, err)
	}
}

func TestAsyncImageModerationIsFailedNotUncertainAndNotRegenerated(t *testing.T) {
	for _, submitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_submission", true: "after_submission"}[submitted], func(t *testing.T) {
			f := newImageJobFixture(t, 2)
			f.adapter.submit = func(_ provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
				cp := provider.ImageCheckpoint{}
				if submitted {
					cp = provider.ImageCheckpoint{Submitted: true, UpstreamID: "moderated-conversation"}
					if err := save(cp); err != nil {
						return cp, err
					}
				}
				return cp, moderationDiagnosticError{422, "the image we created may violate our content policies"}
			}
			job := f.create(t, ImageJobInput{})
			f.service.processMediaJob(context.Background(), job.ID)
			stored := f.job(t, job.ID)
			if stored.Status != media.StatusFailed || stored.ErrorCode != provider.ContentPolicyViolation || !strings.Contains(stored.ErrorMessage, "content policies") {
				t.Fatalf("job = %+v", stored)
			}
			if calls := f.adapter.Calls(); len(calls) != 1 {
				t.Fatalf("unexpected repeated generation: %v", calls)
			}
			if billed, reserved := f.billing(t); billed != 0 || reserved != 0 {
				t.Fatalf("billed=%d reserved=%d", billed, reserved)
			}
			view, err := f.service.accounts.Get(context.Background(), stored.AccountID)
			if err != nil || view.Credential.FailureCount != 0 {
				t.Fatalf("account = %+v, %v", view, err)
			}
		})
	}
}
