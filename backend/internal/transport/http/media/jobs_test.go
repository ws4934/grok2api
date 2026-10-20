package media

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

type jobControlStub struct {
	job     mediadomain.Job
	err     error
	id, key string
}

func (s *jobControlStub) CancelMediaJob(_ context.Context, id string) (mediadomain.Job, error) {
	s.id = id
	return s.job, s.err
}

func (s *jobControlStub) RetryMediaJob(_ context.Context, id, key string) (mediadomain.Job, error) {
	s.id, s.key = id, key
	return s.job, s.err
}

func TestAdminMediaJobsListStatsActionsAndDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-jobs-http.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountValue, _, err := relational.NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderChatGPT, AuthType: accountdomain.AuthTypeOAuth, Name: "jobs", SourceKey: "jobs",
		EncryptedAccessToken: "encrypted", AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := relational.NewClientKeyRepository(database).Create(ctx, clientkeydomain.Key{
		Name: "jobs", Prefix: "jobs", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs := relational.NewMediaJobRepository(database)
	now := time.Now().UTC()
	newJob := func(id string, kind mediadomain.Kind, provider string, status mediadomain.Status, created time.Time) mediadomain.Job {
		job := mediadomain.Job{
			ID: id, Kind: kind, RequestID: "request-" + id, ClientKeyID: key.ID, ClientKeyName: key.Name, AccountID: accountValue.ID,
			Provider: provider, Model: "model", ModelRouteID: 1, UpstreamModel: "upstream", Prompt: "prompt " + id,
			Status: status, InputJSON: "{}", CreatedAt: created, UpdatedAt: created,
		}
		if kind == mediadomain.KindVideo {
			job.Seconds, job.ResultAssetID = 6, "vid_jobs_result_0000001"
		}
		if err := jobs.CreateMediaJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		return job
	}
	newJob("video_jobs_done", mediadomain.KindVideo, string(accountdomain.ProviderConsole), mediadomain.StatusCompleted, now.Add(-3*time.Minute))
	uncertain := newJob("resp_img_jobs_uncertain", mediadomain.KindImage, string(accountdomain.ProviderChatGPT), mediadomain.StatusUncertain, now.Add(-2*time.Minute))
	queued := newJob("resp_img_jobs_queued", mediadomain.KindImage, string(accountdomain.ProviderChatGPT), mediadomain.StatusQueued, now.Add(-time.Minute))
	uncertain.ResultAssetID = "img_jobs_result_00000001"
	if err := jobs.UpdateMediaJob(ctx, uncertain); err != nil {
		t.Fatal(err)
	}

	control := &jobControlStub{}
	service := mediaapp.NewService(relational.NewMediaAssetRepository(database), jobs, nil, nil, mediaapp.Config{PublicBaseURL: "https://api.example"})
	router := gin.New()
	NewHandler(service).WithJobControl(control).RegisterAdmin(router.Group("/api/admin/v1"))
	call := func(method, path, body string, headers ...string) (int, map[string]any) {
		request := httptest.NewRequest(method, "/api/admin/v1"+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		for index := 0; index+1 < len(headers); index += 2 {
			request.Header.Set(headers[index], headers[index+1])
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		var envelope map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &envelope)
		return recorder.Code, envelope
	}
	items := func(envelope map[string]any) []map[string]any {
		data, _ := envelope["data"].(map[string]any)
		raw, _ := data["items"].([]any)
		values := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			values = append(values, item.(map[string]any))
		}
		return values
	}

	code, envelope := call("GET", "/media/jobs?kind=image&sortBy=createdAt&sortOrder=desc", "")
	images := items(envelope)
	if code != 200 || len(images) != 2 || images[0]["id"] != queued.ID || images[0]["cancellable"] != true || images[0]["retryable"] != false {
		t.Fatalf("image jobs: %d %v", code, envelope)
	}
	if images[1]["kind"] != "image" || images[1]["retryable"] != true ||
		images[1]["assetUrls"].([]any)[0] != "https://api.example/v1/media/images/img_jobs_result_00000001" {
		t.Fatalf("uncertain image job: %v", images[1])
	}
	code, envelope = call("GET", "/media/jobs?kind=video&provider=grok_console&status=completed", "")
	if videos := items(envelope); code != 200 || len(videos) != 1 || videos[0]["assetId"] != "vid_jobs_result_0000001" || videos[0]["cancellable"] != false {
		t.Fatalf("video jobs: %d %v", code, envelope)
	}
	for _, path := range []string{"/media/jobs?kind=audio", "/media/jobs?provider=Bad%20Provider", "/media/jobs?status=paused", "/media/jobs/stats?kind=audio"} {
		if code, envelope := call("GET", path, ""); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d %v", path, code, envelope)
		}
	}
	code, envelope = call("GET", "/media/jobs/stats?kind=image", "")
	if stats, _ := envelope["data"].(map[string]any); code != 200 || stats["totalJobs"] != float64(2) || stats["uncertain"] != float64(1) || stats["queued"] != float64(1) {
		t.Fatalf("image stats: %d %v", code, envelope)
	}

	control.err = gateway.ErrMediaJobNotCancellable
	if code, envelope := call("POST", "/media/jobs/"+queued.ID+"/cancel", ""); code != http.StatusConflict || control.id != queued.ID {
		t.Fatalf("cancel conflict: %d %v", code, envelope)
	}
	control.err, control.job = nil, queued
	control.job.Status = mediadomain.StatusCanceled
	if code, envelope := call("POST", "/media/jobs/"+queued.ID+"/cancel", ""); code != 200 || envelope["data"].(map[string]any)["status"] != "canceled" {
		t.Fatalf("cancel: %d %v", code, envelope)
	}
	control.job = mediadomain.Job{ID: "resp_img_jobs_retried", Kind: mediadomain.KindImage, Status: mediadomain.StatusQueued, CreatedAt: now}
	if code, envelope := call("POST", "/media/jobs/"+uncertain.ID+"/retry", "", "Idempotency-Key", "admin-1"); code != 200 || control.key != "admin-1" || envelope["data"].(map[string]any)["id"] != "resp_img_jobs_retried" || envelope["data"].(map[string]any)["resumed"] != false {
		t.Fatalf("retry: %d %v key=%q", code, envelope, control.key)
	}
	control.err = gateway.ErrMediaJobNotRetryable
	if code, _ := call("POST", "/media/jobs/"+queued.ID+"/retry", ""); code != http.StatusConflict {
		t.Fatalf("retry of a non-uncertain job = %d", code)
	}

	if code, envelope := call("DELETE", "/media/jobs", `{"ids":["`+queued.ID+`"]}`); code != http.StatusBadRequest {
		t.Fatalf("delete active job: %d %v", code, envelope)
	}
	if code, envelope := call("DELETE", "/media/jobs", `{"ids":["`+uncertain.ID+`"]}`); code != 200 || envelope["data"].(map[string]any)["deleted"] != float64(1) {
		t.Fatalf("delete uncertain job: %d %v", code, envelope)
	}

	// A failed image job whose checkpoint holds the upstream conversation is
	// retryable and resumes in place; one without it is not.
	newJob("resp_img_jobs_failed", mediadomain.KindImage, string(accountdomain.ProviderChatGPT), mediadomain.StatusFailed, now.Add(time.Minute))
	resumable := newJob("resp_img_jobs_resumable", mediadomain.KindImage, string(accountdomain.ProviderChatGPT), mediadomain.StatusFailed, now.Add(2*time.Minute))
	resumable.Submitted, resumable.Checkpoint = true, `{"submitted":true,"upstream_id":"conv","outputs":["file"]}`
	if err := jobs.UpdateMediaJob(ctx, resumable); err != nil {
		t.Fatal(err)
	}
	code, envelope = call("GET", "/media/jobs?kind=image&status=failed&sortBy=createdAt&sortOrder=desc", "")
	if failed := items(envelope); code != 200 || len(failed) != 2 || failed[0]["id"] != resumable.ID || failed[0]["retryable"] != true || failed[1]["retryable"] != false {
		t.Fatalf("failed image jobs: %d %v", code, envelope)
	}
	control.err, control.job = nil, resumable
	control.job.Status = mediadomain.StatusInProgress
	if code, envelope := call("POST", "/media/jobs/"+resumable.ID+"/retry", ""); code != 200 || envelope["data"].(map[string]any)["id"] != resumable.ID || envelope["data"].(map[string]any)["resumed"] != true {
		t.Fatalf("resume: %d %v", code, envelope)
	}
}
