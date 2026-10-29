package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const rejectedAPIKeyBody = `{"code":"invalid-argument","error":"Incorrect API key provided. You can obtain an API key from https://console.x.ai."}`

type rejectedAPIKeyError struct{}

func (rejectedAPIKeyError) Error() string              { return "test rejected account API key" }
func (rejectedAPIKeyError) HTTPStatusCode() int        { return 400 }
func (rejectedAPIKeyError) PublicErrorMessage() string { return provider.APIKeyRejectedMessage }
func (rejectedAPIKeyError) UpstreamDiagnostic() (string, provider.DiagnosticResponse) {
	return "", provider.DiagnosticResponse{StatusCode: 400, Body: []byte(rejectedAPIKeyBody)}
}

func keyTestResponse(status int, body string) *provider.Response {
	return &provider.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestAPIKeyFailureClassificationPreservesRawStatusAndValidSSO(t *testing.T) {
	f := ProviderUpstreamFailure(rejectedAPIKeyError{}, 42, "account")
	if f == nil || !f.APIKeyRejected || !f.AccountScoped || f.CredentialRejected || f.QuotaExhausted || f.HTTPStatus != 400 || f.PublicMessage != provider.APIKeyRejectedMessage {
		t.Fatalf("failure=%+v", f)
	}
	for _, body := range []string{`{"error":"duration is invalid"}`, `{"code":"invalid_api_key","error":"Generated video rejected by content moderation."}`} {
		f := newHTTPUpstreamFailure(400, []byte(body), 42, "account")
		if f.APIKeyRejected {
			t.Fatalf("request-scoped refusal was treated as an API key: %+v", f)
		}
	}
	// Inspection must not discard a large, unrelated parameter-error body.
	body := `{"error":"` + strings.Repeat("parameter detail ", 10000) + `"}`
	response := keyTestResponse(400, body)
	if apiKeyRejectedResponse(response, account.Credential{ID: 42}) != nil {
		t.Fatal("ordinary 400 rotated accounts")
	}
	raw, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(raw) != body {
		t.Fatalf("400 response changed: bytes=%d err=%v", len(raw), err)
	}
}

func TestVideoAPIKeyFailureRotatesOnlyBeforeSubmissionAndHonorsPinnedAccount(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		pinned, submitted, allBad bool
	}{
		{"automatic create", false, false, false},
		{"pinned create", true, false, false},
		{"already submitted poll", false, true, false},
		{"all accounts rejected", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, adapter := newNativeVideoFixture(t, 100, 100)
			first := f.ids[0]
			prioritizeNativeAccount(t, f, first)
			if tc.allBad {
				f.service.videoMaxAttempts.Store(3)
			}
			adapter.before = func(request provider.VideoRequest) error {
				if request.Credential.ID != first && !tc.allBad {
					return nil
				}
				if tc.submitted {
					if err := request.SaveCheckpoint(`{"request_id":"already-created"}`); err != nil {
						return err
					}
					return provider.WrapVideoStage(provider.VideoStagePoll, 0, rejectedAPIKeyError{})
				}
				if err := request.SaveCheckpoint(`{"submitting":true}`); err != nil {
					return err
				}
				return provider.VideoCreateFailed(request, rejectedAPIKeyError{})
			}
			ctx := context.Background()
			if tc.pinned {
				ctx = WithCreativeAccount(ctx, first)
			}
			job, err := f.service.CreateVideo(ctx, VideoInput{RequestID: "api-key-video", ClientKey: f.key, PublicModel: "Console/grok-imagine-video-1.5", Prompt: "a landscape", Duration: 6, Resolution: "720p"})
			if err != nil {
				t.Fatal(err)
			}
			f.service.processMediaJob(context.Background(), job.ID)
			job, err = f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := []uint64{first}
			if tc.allBad {
				wantCalls = append(wantCalls, f.ids[1])
			}
			if !tc.pinned && !tc.submitted && !tc.allBad {
				wantCalls = append(wantCalls, f.ids[1])
				if job.Status != media.StatusCompleted || job.AccountID != f.ids[1] {
					t.Fatalf("job=%+v", job)
				}
				f.checkBilling(t, 84)
			} else {
				if job.Status != media.StatusFailed || job.ErrorCode != provider.APIKeyRejectedCode || job.ErrorMessage != provider.APIKeyRejectedMessage {
					t.Fatalf("status=%s code=%s message=%s", job.Status, job.ErrorCode, job.ErrorMessage)
				}
				f.checkBilling(t, 0)
			}
			if !slices.Equal(adapter.calls, wantCalls) {
				t.Fatalf("calls=%v want=%v", adapter.calls, wantCalls)
			}
			credential, err := f.accounts.Get(context.Background(), first)
			if err != nil || credential.AuthStatus != account.AuthStatusActive || !credential.Enabled {
				t.Fatalf("valid SSO was retired: %+v err=%v", credential, err)
			}
			if credential.FailureCount != 1 || credential.CooldownUntil == nil || !credential.CooldownUntil.After(time.Now()) {
				t.Fatalf("invalid API key account was not cooled: failures=%d until=%v", credential.FailureCount, credential.CooldownUntil)
			}
			windows, err := f.accounts.GetQuotaWindows(context.Background(), []uint64{first})
			if err != nil {
				t.Fatal(err)
			}
			remaining := 100
			if tc.submitted {
				remaining -= 84 // Already accepted work retains the estimated native spend.
			}
			for _, window := range windows[first] {
				if window.Remaining != remaining {
					t.Fatalf("rejected creation consumed quota: %+v want=%d", window, remaining)
				}
			}
			records, _, err := f.audits.List(context.Background(), 0, 5)
			if err != nil || len(records) != 1 {
				t.Fatalf("records=%d err=%v", len(records), err)
			}
			record, err := f.audits.Get(context.Background(), records[0].ID)
			if err != nil || len(record.Attempts) == 0 {
				t.Fatalf("audit=%+v err=%v", record, err)
			}
			firstAttempt := record.Attempts[0]
			if firstAttempt.UpstreamStatusCode == nil || *firstAttempt.UpstreamStatusCode != 400 || !strings.Contains(string(firstAttempt.ResponseBody), "Incorrect API key provided") {
				t.Fatalf("raw key refusal lost from diagnostics: %+v", firstAttempt)
			}
		})
	}
}

type keyResponseAdapter struct {
	bad   uint64
	calls []uint64
	body  string
}

func (*keyResponseAdapter) Provider() account.Provider { return account.ProviderBuild }
func (*keyResponseAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderBuild)
}
func (a *keyResponseAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.calls = append(a.calls, request.Credential.ID)
	if request.Credential.ID == a.bad {
		return keyTestResponse(400, a.body), nil
	}
	return keyTestResponse(200, `{"id":"key-ok","object":"response","status":"completed","output":[]}`), nil
}

func TestConversationAPIKeyFailureDoesNotRotateOrdinary400OrPinnedRequests(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		pinned     bool
		wantCalls  int
	}{
		{"automatic key failure", rejectedAPIKeyBody, false, 2}, {"pinned key failure", rejectedAPIKeyBody, true, 1}, {"ordinary parameter error", `{"error":"duration is invalid"}`, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &keyResponseAdapter{body: tc.body}
			f := newResponseFixture(t, adapter, 2)
			adapter.bad = f.accounts[0].ID
			ctx := context.Background()
			if tc.pinned {
				ctx = WithCreativeAccount(ctx, adapter.bad)
			}
			result, err := f.service.CreateResponse(ctx, Input{RequestID: "key-chat", ClientKey: f.key, PublicModel: "grok-4.6", Body: []byte(`{"model":"grok-4.6","input":"test","store":false}`)})
			if tc.pinned {
				var failure *UpstreamFailure
				if !errors.As(err, &failure) || !failure.APIKeyRejected {
					t.Fatalf("err=%v", err)
				}
			} else {
				if err != nil || result == nil {
					t.Fatalf("result=%v err=%v", result, err)
				}
				_, _ = io.Copy(io.Discard, result.Body)
				result.Finalize(Usage{}, "", "")
				_ = result.Body.Close()
			}
			if len(adapter.calls) != tc.wantCalls {
				t.Fatalf("calls=%v", adapter.calls)
			}
		})
	}
}

type keyImageAdapter struct {
	*nativeConsoleImageStub
	bad      uint64
	allBad   bool
	accounts []uint64
	body     string
}

func (a *keyImageAdapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	a.accounts = append(a.accounts, request.Credential.ID)
	if a.allBad || request.Credential.ID == a.bad {
		return keyTestResponse(400, a.body), nil
	}
	return a.nativeConsoleImageStub.GenerateImage(ctx, request)
}

func TestImageAPIKeyFailureRotatesAndKeepsTheFinalCause(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		pinned     bool
		allBad     bool
		wantCalls  int
	}{
		{"automatic", rejectedAPIKeyBody, false, false, 2},
		{"pinned", rejectedAPIKeyBody, true, false, 1},
		{"all accounts rejected", rejectedAPIKeyBody, false, true, 2},
		{"ordinary parameter error", `{"error":"duration is invalid"}`, false, false, 1},
		{"moderation", `{"error":"Generated video rejected by content moderation."}`, false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeImageFixture(t, 100, 100)
			prioritizeNativeAccount(t, f, f.ids[0])
			adapter := &keyImageAdapter{nativeConsoleImageStub: f.adapter, bad: f.ids[0], allBad: tc.allBad, body: tc.body}
			f.service.providers = provider.NewRegistry(adapter)
			if tc.allBad {
				f.service.maxAttempts.Store(3) // Exercise account exhaustion, not just the attempt cap.
			}
			ctx := context.Background()
			if tc.pinned {
				ctx = WithCreativeAccount(ctx, adapter.bad)
			}
			result, err := f.service.GenerateImage(ctx, ImageGenerationInput{RequestID: "key-image", ClientKey: f.key, PublicModel: "Console/grok-imagine-image-2.0", Prompt: "draw", Count: 1, Quality: "low", Resolution: "1k"})
			if tc.pinned || tc.allBad {
				var failure *UpstreamFailure
				if !errors.As(err, &failure) || !failure.APIKeyRejected || failure.PublicMessage != provider.APIKeyRejectedMessage {
					t.Fatalf("failure=%+v err=%v", failure, err)
				}
				f.checkBilling(t, 0)
			} else {
				if err != nil || result == nil {
					t.Fatalf("result=%v err=%v", result, err)
				}
				_, _ = io.Copy(io.Discard, result.Body)
				code := ""
				wantBilled := int64(4)
				if result.StatusCode != http.StatusOK {
					code, wantBilled = "upstream_error", 0
				}
				result.Finalize(Usage{}, "", code)
				_ = result.Body.Close()
				f.checkBilling(t, wantBilled)
			}
			if len(adapter.accounts) != tc.wantCalls {
				t.Fatalf("accounts=%v wantCalls=%d", adapter.accounts, tc.wantCalls)
			}
			credential, err := f.accounts.Get(context.Background(), adapter.bad)
			if err != nil || credential.AuthStatus != account.AuthStatusActive || !credential.Enabled {
				t.Fatalf("valid SSO was retired: %+v err=%v", credential, err)
			}
		})
	}
}

type keyVoiceAdapter struct {
	*nativeConsoleImageStub
	bad      uint64
	allBad   bool
	accounts []uint64
}

func (a *keyVoiceAdapter) SynthesizeSpeech(_ context.Context, request provider.TTSRequest) (provider.TTSResult, error) {
	a.accounts = append(a.accounts, request.Credential.ID)
	if a.allBad || request.Credential.ID == a.bad {
		return provider.TTSResult{}, rejectedAPIKeyError{}
	}
	return provider.TTSResult{Audio: []byte("test audio"), ContentType: "audio/mpeg"}, nil
}

func TestVoiceAPIKeyFailureSurvivesTypedErrorConversion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pinned    bool
		allBad    bool
		wantCalls int
	}{
		{"automatic", false, false, 2},
		{"pinned", true, false, 1},
		{"all accounts rejected", false, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeImageFixture(t, 100, 100)
			prioritizeNativeAccount(t, f, f.ids[0])
			adapter := &keyVoiceAdapter{nativeConsoleImageStub: f.adapter, bad: f.ids[0], allBad: tc.allBad}
			f.service.providers = provider.NewRegistry(adapter)
			if tc.allBad {
				f.service.maxAttempts.Store(3)
			}
			ctx := context.Background()
			if tc.pinned {
				ctx = WithCreativeAccount(ctx, adapter.bad)
			}
			result, err := f.service.SynthesizeSpeech(ctx, TTSInput{RequestID: "key-voice", ClientKey: f.key, PublicModel: "Console/grok-voice-latest", Text: "hello", VoiceID: "ara"})
			if tc.pinned || tc.allBad {
				var failure *UpstreamFailure
				if !errors.As(err, &failure) || !failure.APIKeyRejected || failure.PublicMessage != provider.APIKeyRejectedMessage {
					t.Fatalf("failure=%+v err=%v", failure, err)
				}
				f.checkBilling(t, 0)
			} else {
				if err != nil || result == nil || result.StatusCode != http.StatusOK {
					t.Fatalf("result=%v err=%v", result, err)
				}
				audio, readErr := io.ReadAll(result.Body)
				result.Finalize(Usage{}, "", "")
				_ = result.Body.Close()
				if readErr != nil || string(audio) != "test audio" {
					t.Fatalf("audio=%q err=%v", audio, readErr)
				}
			}
			if len(adapter.accounts) != tc.wantCalls {
				t.Fatalf("accounts=%v wantCalls=%d", adapter.accounts, tc.wantCalls)
			}
		})
	}
}
