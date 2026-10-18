package channel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/google/uuid"
)

type MediaStore interface {
	SaveImage(context.Context, []byte) (media.Asset, error)
	SaveInputImage(context.Context, []byte) (media.Asset, error)
	OpenInputAsset(context.Context, string) (media.Asset, io.ReadCloser, error)
	OpenImage(context.Context, string) (media.Asset, io.ReadCloser, error)
	PublicImageURL(string) string
}

// ResponseIDPrefix makes every image job addressable as an OpenAI Responses object.
const ResponseIDPrefix = "resp_"

type ImageExecution struct {
	jobs  domain.JobRepository
	media MediaStore
}

func (s *Service) SetImages(jobs domain.JobRepository, media MediaStore) {
	s.images = &ImageExecution{jobs: jobs, media: media}
}
func (s *Service) SaveImageInput(ctx context.Context, b domain.Blob) (domain.ImageInput, error) {
	if s.images == nil {
		return domain.ImageInput{}, domain.ErrUnsupported
	}
	a, e := s.images.media.SaveInputImage(ctx, b.Data)
	return domain.ImageInput{AssetID: a.ID, Name: b.Name, MIME: a.MIMEType}, e
}
func (s *Service) SubmitImages(ctx context.Context, key clientkey.Key, route model.Route, r domain.ImageRequest, idempotency string) (domain.ImageJob, error) {
	var zero domain.ImageJob
	if s.images == nil || s.execution == nil {
		return zero, domain.ErrUnsupported
	}
	cfg, e := s.authorize(ctx, key, route)
	if e != nil {
		return zero, e
	}
	if r.N == 0 {
		r.N = 1
	}
	if r.N < 1 || r.N > 10 || strings.TrimSpace(r.Prompt) == "" || len(r.Prompt) > 100000 || len(r.Inputs) > 8 || len(idempotency) > 160 {
		return zero, errors.New("invalid image request")
	}
	if r.Operation != "image" && r.Operation != "image_edit" {
		return zero, errors.New("invalid image operation")
	}
	if r.Operation == "image_edit" && len(r.Inputs) == 0 {
		return zero, errors.New("image editing requires a source image")
	}
	if r.Mask != nil && len(r.Inputs) != 1 {
		return zero, errors.New("mask requires one input")
	}
	if r.Background == "transparent" && (r.OutputFormat == "jpeg" || r.OutputFormat == "jpg") {
		return zero, errors.New("JPEG cannot carry transparency")
	}
	r.Model = route.UpstreamModel
	r.ConversationModel = cfg.ImageConversationModel
	if r.ConversationModel == "" {
		return zero, errors.New("image conversation model is not configured")
	}
	if r.OutputFormat == "" {
		r.OutputFormat = "png"
	}
	if r.Background == "" {
		r.Background = "auto"
	}
	if r.Quality == "" {
		r.Quality = "auto"
	}
	if r.Size == "" {
		r.Size = "auto"
	}
	price, e := s.resolvePrice(ctx, route.Provider, route.UpstreamModel, r.Operation, r.Quality, r.Size)
	if e != nil {
		return zero, e
	}
	m, _ := s.module(route.Provider)
	adapter, ok := m.(domain.ImageWorkflowAdapter)
	if !ok {
		return zero, domain.ErrUnsupported
	}
	// Validate masks, formats and all protected inputs before acceptance and billing.
	load := func(input domain.ImageInput) (domain.Blob, error) { return s.loadImageInput(ctx, input) }
	if _, e := adapter.PrepareImage(ctx, domain.Credentials{}, r, load); e != nil {
		return zero, e
	}
	canonical := r
	canonical.ConversationModel = "" // A server configuration change is not a different client request.
	canonical.Inputs = append([]domain.ImageInput(nil), r.Inputs...)
	for i, input := range r.Inputs {
		b, e := load(input)
		if e != nil {
			return zero, e
		}
		sum := sha256.Sum256(b.Data)
		canonical.Inputs[i].AssetID = hex.EncodeToString(sum[:])
		canonical.Inputs[i].Name = ""
	}
	if r.Mask != nil {
		b, e := load(*r.Mask)
		if e != nil {
			return zero, e
		}
		sum := sha256.Sum256(b.Data)
		mask := *r.Mask
		mask.AssetID = hex.EncodeToString(sum[:])
		mask.Name = ""
		canonical.Mask = &mask
	}
	b, e := json.Marshal(struct {
		Provider    account.Provider
		PublicModel string
		Request     domain.ImageRequest
	}{route.Provider, route.PublicID, canonical})
	if e != nil {
		return zero, e
	}
	sum := sha256.Sum256(b)
	job := domain.ImageJob{ID: ResponseIDPrefix + strings.ReplaceAll(uuid.NewString(), "-", ""), ClientKeyID: key.ID, Provider: route.Provider, ModelRouteID: route.ID, BoundAccountIDs: append([]uint64(nil), route.BoundAccountIDs...), Request: r, Price: price, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(24 * time.Hour), IdempotencyKey: idempotency, PayloadHash: hex.EncodeToString(sum[:])}
	if idempotency != "" {
		if prior, ok, e := s.images.jobs.FindImageJob(ctx, key.ID, idempotency); e != nil {
			return zero, e
		} else if ok {
			if prior.PayloadHash != job.PayloadHash {
				return zero, domain.ErrConflict
			}
			return prior, nil
		}
	}
	cleanup := []string{}
	committed := false
	defer func() {
		if !committed {
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			for _, event := range cleanup {
				_ = s.execution.billing.CancelBilling(c, event)
			}
		}
	}()
	for i := 0; i < r.N; i++ {
		item := domain.ImageItem{ID: uuid.NewString(), JobID: job.ID, Position: i}
		_, e := s.execution.billing.ReserveBilling(ctx, key, item.ID, price.ImageTicks, 25*time.Hour)
		if e != nil {
			return zero, e
		}
		cleanup = append(cleanup, item.ID)
		job.Items = append(job.Items, item)
	}
	accepted, created, e := s.images.jobs.CreateImageJob(ctx, job)
	if e != nil {
		return zero, e
	}
	committed = created
	return accepted, nil
}
func (s *Service) loadImageInput(ctx context.Context, input domain.ImageInput) (domain.Blob, error) {
	a, r, e := s.images.media.OpenInputAsset(ctx, input.AssetID)
	if e != nil {
		return domain.Blob{}, e
	}
	defer r.Close()
	b, e := io.ReadAll(io.LimitReader(r, 20<<20+1))
	if len(b) > 20<<20 {
		return domain.Blob{}, errors.New("input exceeds 20 MiB")
	}
	return domain.Blob{Name: input.Name, MIME: a.MIMEType, Data: b}, e
}
func (s *Service) GetImageJob(ctx context.Context, key uint64, id string) (domain.ImageJob, error) {
	if key == 0 || s.images == nil {
		return domain.ImageJob{}, domain.ErrNotFound
	}
	return s.images.jobs.GetImageJob(ctx, id, key)
}
func (s *Service) ImageJobs(ctx context.Context, key uint64, p account.Provider, offset, limit int) ([]domain.ImageJob, int64, error) {
	if s.images == nil {
		return nil, 0, domain.ErrUnsupported
	}
	return s.images.jobs.ListImageJobs(ctx, key, p, offset, limit)
}
func (s *Service) CancelImageJob(ctx context.Context, key uint64, id string) error {
	if key == 0 || s.images == nil {
		return domain.ErrNotFound
	}
	job, e := s.images.jobs.GetImageJob(ctx, id, key)
	if e != nil {
		return e
	}
	if e = s.images.jobs.CancelImageJob(ctx, id, key); e != nil {
		return e
	}
	for _, item := range job.Items {
		_ = s.execution.billing.CancelBilling(ctx, item.ID)
		_ = s.images.jobs.FinishChannelQuota(ctx, item.ID, false)
	}
	return nil
}

// RetryUncertainImages is an explicit administrator action. Only uncertain
// children are retried; each gets a new task, current price and reservation.
func (s *Service) RetryUncertainImages(ctx context.Context, p account.Provider, id, idempotency string) (domain.ImageJob, error) {
	if s.clientKeys == nil || idempotency == "" {
		return domain.ImageJob{}, domain.ErrUnsupported
	}
	job, e := s.images.jobs.GetImageJob(ctx, id, 0)
	if e != nil {
		return domain.ImageJob{}, e
	}
	if job.Provider != p || !terminalJob(job.Status) {
		return domain.ImageJob{}, domain.ErrConflict
	}
	count := 0
	for _, item := range job.Items {
		if item.Status == "uncertain" {
			count++
		}
	}
	if count == 0 {
		return domain.ImageJob{}, domain.ErrConflict
	}
	key, e := s.clientKeys.Get(ctx, job.ClientKeyID)
	if e != nil || !key.IsAvailable(time.Now()) {
		return domain.ImageJob{}, domain.ErrNotFound
	}
	route, e := s.models.Get(ctx, job.ModelRouteID)
	if e != nil {
		return domain.ImageJob{}, e
	}
	request := job.Request
	request.N = count
	return s.SubmitImages(ctx, key, route, request, idempotency)
}
func terminalJob(status string) bool { return status != "queued" && status != "running" }
func (s *Service) WaitImageJob(ctx context.Context, key uint64, id string) (domain.ImageJob, error) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		job, e := s.GetImageJob(ctx, key, id)
		if e != nil {
			return job, e
		}
		if terminalJob(job.Status) {
			return job, nil
		}
		select {
		case <-ctx.Done():
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = s.CancelImageJob(cleanup, key, id)
			return job, ctx.Err()
		case <-t.C:
		}
	}
}

type ImageResult struct {
	URL    string `json:"url,omitempty"`
	Base64 string `json:"b64_json,omitempty"`
}

func (s *Service) ImageResults(ctx context.Context, key uint64, id, format string) ([]ImageResult, error) {
	if format != "url" && format != "b64_json" {
		return nil, errors.New("invalid response_format")
	}
	job, e := s.GetImageJob(ctx, key, id)
	if e != nil {
		return nil, e
	}
	if !terminalJob(job.Status) {
		return nil, domain.ErrConflict
	}
	out := []ImageResult{}
	for _, item := range job.Items {
		results, e := s.assetResults(ctx, item.AssetIDs, format)
		if e != nil {
			return nil, e
		}
		out = append(out, results...)
	}
	return out, nil
}

// assetResults renders stored image assets as public URLs or base64 data.
func (s *Service) assetResults(ctx context.Context, assets []string, format string) ([]ImageResult, error) {
	out := []ImageResult{}
	for _, asset := range assets {
		if format == "url" {
			out = append(out, ImageResult{URL: s.images.media.PublicImageURL(asset)})
			continue
		}
		_, r, e := s.images.media.OpenImage(ctx, asset)
		if e != nil {
			return nil, e
		}
		raw, e := io.ReadAll(io.LimitReader(r, 64<<20+1))
		r.Close()
		if e != nil || len(raw) > 64<<20 {
			return nil, errors.New("result unavailable")
		}
		out = append(out, ImageResult{Base64: base64.StdEncoding.EncodeToString(raw)})
	}
	return out, nil
}
func (s *Service) RunImages(ctx context.Context) {
	if s.images == nil {
		return
	}
	var groups sync.WaitGroup
	for _, descriptor := range s.registry.Descriptors() {
		groups.Add(1)
		go func(provider account.Provider) { defer groups.Done(); s.runImageWorkers(ctx, provider) }(descriptor.ID)
	}
	groups.Wait()
}
func (s *Service) runImageWorkers(ctx context.Context, provider account.Provider) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	finished := make(chan struct{}, 32)
	active := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-finished:
			active--
		case <-ticker.C:
			cfg, e := s.repo.Settings(ctx, provider)
			if e != nil {
				continue
			}
			// Accepted jobs remain eligible after channel disablement. One coordinator
			// checks configuration; idle worker slots do not each query the database.
			for active < cfg.ImageWorkers && ctx.Err() == nil {
				job, item, e := s.images.jobs.ClaimImageItem(ctx, provider, time.Now().UTC(), time.Now().UTC().Add(90*time.Second), uuid.NewString())
				if e != nil {
					break
				}
				active++
				workers.Add(1)
				go func(job domain.ImageJob, item domain.ImageItem) {
					defer workers.Done()
					defer func() { finished <- struct{}{} }()
					s.runImageItem(ctx, job, item)
				}(job, item)
			}
		}
	}
}

func (s *Service) runImageItem(parent context.Context, job domain.ImageJob, item domain.ImageItem) {
	deadline := job.ExpiresAt
	if job.StartedAt != nil && job.StartedAt.Before(job.ExpiresAt) {
		deadline = job.StartedAt.Add(time.Hour)
	}
	if item.StartedAt != nil && item.StartedAt.Add(10*time.Minute).Before(deadline) {
		deadline = item.StartedAt.Add(10 * time.Minute)
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	heartbeatDone := make(chan struct{})
	defer func() { cancel(); <-heartbeatDone }()
	go func(heartbeatCtx context.Context, claim domain.ImageItem) {
		defer close(heartbeatDone)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-t.C:
				if e := s.images.jobs.RenewImageItem(heartbeatCtx, claim, time.Now().UTC().Add(90*time.Second)); e != nil {
					cancel()
					return
				}
			}
		}
	}(ctx, item)
	quotaSent := item.Submitted
	complete := func(assets []string, status, message string, record *audit.Record) error {
		c, end := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer end()
		e := s.images.jobs.CompleteImageItem(c, item, assets, status, message, record)
		if e == nil {
			if status != "uncertain" {
				_ = s.images.jobs.FinishChannelQuota(c, item.ID, !quotaSent)
			}
			if record == nil {
				_ = s.execution.billing.CancelBilling(c, item.ID)
			} else {
				s.execution.billing.CompleteBilling(item.ID)
			}
		}
		return e
	}
	if item.Submitted && item.Checkpoint.ConversationID == "" {
		_ = complete(nil, "uncertain", "submission has no recoverable conversation ID", nil)
		return
	}
	if time.Now().After(deadline) {
		_ = complete(nil, "failed", "image execution deadline exceeded", nil)
		return
	}
	m, e := s.module(job.Provider)
	if e != nil {
		_ = complete(nil, "failed", "channel module is unavailable", nil)
		return
	}
	adapter, ok := m.(domain.ImageWorkflowAdapter)
	if !ok {
		_ = complete(nil, "failed", "channel image workflow unavailable", nil)
		return
	}
	if item.AccountID == 0 {
		quotaRejected := false
		for offset := 0; item.AccountID == 0; offset += 200 {
			rows, total, e := s.repo.ListAccounts(ctx, job.Provider, offset, 200)
			if e != nil {
				return
			}
			for _, a := range rows {
				if !a.Enabled || a.Status != "verified" || !slices.Contains(a.Models, job.Request.ConversationModel) || (len(job.BoundAccountIDs) > 0 && !slices.Contains(job.BoundAccountIDs, a.ID)) {
					continue
				}
				if e := s.images.jobs.AssignImageAccount(ctx, item, a.ID); e == nil {
					if policy, ok := m.(domain.QuotaPolicy); ok {
						if err := s.images.jobs.ReserveChannelQuota(ctx, a.ID, item.ID, policy.ImageQuota(job.Request)); err != nil {
							_ = s.images.jobs.ReleaseChannelSlot(ctx, item.ClaimToken)
							if errors.Is(err, domain.ErrQuotaExhausted) {
								quotaRejected = true
								continue
							}
							return
						}
					}
					item.AccountID = a.ID
					break
				}
			}
			if offset+len(rows) >= int(total) {
				break
			}
		}
		if item.AccountID == 0 {
			if quotaRejected {
				_ = complete(nil, "failed", "no account has the required capability quota", nil)
			}
			return
		}
	}
	if e := s.images.jobs.AssignImageAccount(ctx, item, item.AccountID); e != nil {
		return
	}
	secret, e := s.repo.GetSecret(ctx, job.Provider, item.AccountID)
	if e != nil {
		_ = complete(nil, "failed", "account unavailable", nil)
		return
	}
	credentials, e := s.decrypt(secret)
	if e != nil {
		_ = complete(nil, "failed", "account credential unavailable", nil)
		return
	}
	base := ctx
	releaseEnvironment := func() {}
	defer func() { releaseEnvironment() }()
	// Quota marks since the last egress switch. An upload is reusable only once
	// its file reference is in the checkpoint; any other mark means the request
	// may have reached upstream and must not be repeated.
	sent, uploadsMarked, uploadsKept := false, 0, len(item.Checkpoint.Uploads)
	enter := func() error {
		releaseEnvironment()
		envCtx, release, e := s.withEnvironment(base, job.Provider, item.AccountID)
		if e != nil {
			releaseEnvironment = func() {}
			return e
		}
		releaseEnvironment = release
		sent, uploadsMarked, uploadsKept = false, 0, len(item.Checkpoint.Uploads)
		ctx = domain.WithQuotaObserver(envCtx, func(feature string) error {
			if feature == "file_upload" {
				uploadsMarked++
			} else {
				sent = true
			}
			return s.images.jobs.MarkChannelQuotaSent(envCtx, item.ID, feature)
		})
		ctx = domain.WithStageObserver(ctx, func(stage string) error {
			return s.images.jobs.SaveImageCheckpoint(envCtx, item, item.Checkpoint, stage)
		})
		return nil
	}
	if enter() != nil {
		return
	}
	cp := item.Checkpoint
	// One immediate retry on another egress when the failure belongs to the egress
	// and generation was not submitted; stored uploads are reused, not resent.
	for attempt := 1; !item.Submitted; attempt++ {
		if policy, ok := m.(domain.QuotaPolicy); ok {
			if e := s.images.jobs.ReserveChannelQuota(ctx, item.AccountID, item.ID, policy.ImageQuota(job.Request)); e != nil {
				_ = complete(nil, "failed", "capability quota unavailable", nil)
				return
			}
		}
		prepared, e := adapter.PrepareImage(ctx, credentials, job.Request, func(input domain.ImageInput) (domain.Blob, error) { return s.loadImageInput(ctx, input) })
		if e != nil {
			_ = complete(nil, "failed", "image preparation failed", nil)
			return
		}
		prepared.Checkpoint = item.Checkpoint
		quotaSent = true
		cp, e = adapter.SubmitImage(ctx, credentials, prepared, func(checkpoint domain.Checkpoint) error {
			stage := "uploading"
			if checkpoint.Submitted {
				stage = "submitted"
			}
			if checkpoint.ConversationID != "" {
				stage = "polling"
			}
			if e := s.images.jobs.SaveImageCheckpoint(ctx, item, checkpoint, stage); e != nil {
				return e
			}
			item.Submitted = checkpoint.Submitted
			item.Checkpoint = checkpoint
			return nil
		})
		egressFailed := egressOutcome(ctx, m, e)
		reusable := !sent && uploadsMarked == len(item.Checkpoint.Uploads)-uploadsKept
		if e != nil && attempt == 1 && egressFailed && !item.Submitted && reusable && base.Err() == nil {
			if enter() != nil {
				return
			}
			continue
		}
		if e != nil {
			if item.Submitted && item.Checkpoint.ConversationID == "" {
				if classifier, ok := m.(domain.FailureClassifier); ok && classifier.Classify(e).Unsent {
					_ = complete(nil, "failed", "upstream explicitly rejected generation", nil)
					return
				}
				_ = complete(nil, "uncertain", "upstream acceptance is uncertain", nil)
			} else if !item.Submitted {
				if classifier, ok := m.(domain.FailureClassifier); ok && item.Generation < 3 && classifier.Classify(e).Kind == domain.FailureNetwork {
					return
				}
				_ = complete(nil, "failed", "upstream image preparation failed", nil)
			}
			return
		}
		break
	}
	cp, e = adapter.PollImage(ctx, credentials, cp)
	if e != nil {
		egressOutcome(ctx, m, e)
		return
	}
	if e = s.images.jobs.SaveImageCheckpoint(ctx, item, cp, "downloading"); e != nil {
		return
	}
	blobs, e := adapter.FetchImage(ctx, credentials, cp)
	egressOutcome(ctx, m, e)
	if e != nil {
		return
	}
	if len(blobs) == 0 {
		_ = complete(nil, "failed", "upstream returned no generated image", nil)
		return
	}
	blob := blobs[0]
	if validator, ok := m.(domain.ImageOutputValidator); ok {
		blob, e = validator.ValidateImageOutput(blob, job.Request)
		if e != nil {
			_ = complete(nil, "failed", "upstream image did not satisfy output requirements", nil)
			return
		}
	}
	asset, e := s.images.media.SaveImage(ctx, blob.Data)
	if e != nil {
		return
	}
	if config, _, err := image.DecodeConfig(bytes.NewReader(blob.Data)); err == nil {
		item.Width, item.Height = config.Width, config.Height
	}
	item.MIME = asset.MIMEType
	id := item.AccountID
	record := audit.Record{EventID: item.ID, RequestID: job.ID, ClientKeyID: job.ClientKeyID, ModelRouteID: job.ModelRouteID, ModelPublicID: job.Request.Model, ModelUpstreamModel: job.Request.Model, Provider: string(job.Provider), Operation: audit.Operation(job.Request.Operation), UsageSource: audit.UsageSourceEstimated, AccountID: &id, AccountName: secret.Name, StatusCode: 200, MediaInputImages: int64(len(job.Request.Inputs)), MediaOutputImages: 1, EstimatedCostInUSDTicks: job.Price.ImageTicks, PricingModel: job.Request.Model, PricingVersion: fmt.Sprintf("channel:%d", job.Price.Version), CreatedAt: time.Now().UTC()}
	env := domain.EnvironmentFrom(ctx)
	record.EgressScope = string(job.Provider)
	record.EgressMode = audit.EgressMode("direct")
	if env.ProxyURL != "" {
		record.EgressMode = audit.EgressMode("proxy")
	}
	if env.NodeID > 0 {
		record.EgressNodeID = &env.NodeID
		record.EgressNodeName = env.NodeName
	}
	record.Estimation = &audit.EstimationDetails{Version: 1, PriceVersion: job.Price.Version, ImageJobID: job.ID, Stage: "archived", Attempt: item.Generation}
	_ = complete([]string{asset.ID}, "succeeded", "", &record)
}
