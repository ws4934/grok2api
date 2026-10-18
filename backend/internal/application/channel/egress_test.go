package channel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/google/uuid"
)

var errEgress = errors.New("proxy unreachable")

// egressModule fails the first calls with an egress error, optionally after
// marking quota as sent (meaning the request may have reached upstream).
type egressModule struct {
	*fixtureModule
	failures  int
	markFirst bool
	calls     int
}

func (m *egressModule) Classify(err error) domain.Failure {
	if errors.Is(err, errEgress) {
		return domain.Failure{Kind: domain.FailureNetwork, Code: domain.FailureCodeTransport}
	}
	return domain.Failure{Kind: domain.FailureNetwork, Code: "other"}
}
func (m *egressModule) fail(ctx context.Context, feature string) error {
	m.calls++
	if m.failures == 0 {
		return nil
	}
	m.failures--
	if m.markFirst {
		if e := domain.ObserveQuotaSend(ctx, feature); e != nil {
			return e
		}
	}
	return errEgress
}
func (m *egressModule) Chat(ctx context.Context, c domain.Credentials, r domain.ChatRequest, emit func(domain.ChatEvent) error) error {
	if e := m.fail(ctx, "reason"); e != nil {
		return e
	}
	return m.fixtureModule.Chat(ctx, c, r, emit)
}
func (m *egressModule) SubmitImage(ctx context.Context, c domain.Credentials, p domain.PreparedImage, save func(domain.Checkpoint) error) (domain.Checkpoint, error) {
	if e := m.fail(ctx, "image_gen"); e != nil {
		return domain.Checkpoint{}, e
	}
	return m.fixtureModule.SubmitImage(ctx, c, p, save)
}

type egressProbe struct{ acquired, released, failures, successes int }

func (p *egressProbe) install(s *Service) {
	s.SetEnvironment(func(ctx context.Context, _ account.Provider, _ uint64) (context.Context, func(), error) {
		p.acquired++
		env := domain.Environment{NodeID: uint64(p.acquired), Report: func(status int, err error) {
			if err != nil {
				p.failures++
			} else if status == 200 {
				p.successes++
			}
		}}
		return domain.WithEnvironment(ctx, env), func() { p.released++ }, nil
	})
}

func TestChatRetriesEgressFailureOnlyBeforeUpstream(t *testing.T) {
	for name, tc := range map[string]struct {
		markFirst     bool
		wantErr       bool
		wantAcquired  int
		wantFailures  int
		wantSuccesses int
	}{
		"nothing sent: retried on a new egress": {false, false, 2, 1, 1},
		"quota marked: not repeated":            {true, true, 1, 1, 0},
	} {
		t.Run(name, func(t *testing.T) {
			module := &egressModule{failures: 1, markFirst: tc.markFirst}
			f := newFixture(t, func(m *fixtureModule) domain.Module { module.fixtureModule = m; return module })
			ctx := context.Background()
			_ = f.s.SetGrant(ctx, "fixture", f.key.ID, true)
			probe := &egressProbe{}
			probe.install(f.s)
			request := domain.ChatRequest{Messages: []domain.Message{{Role: "user", Content: []domain.ContentPart{{Type: "text", Text: "hi"}}}}}
			_, err := f.s.Chat(ctx, f.key, f.route, request, false, func(domain.ChatEvent) error { return nil })
			if (err != nil) != tc.wantErr || probe.acquired != tc.wantAcquired || probe.released != probe.acquired || probe.failures != tc.wantFailures || probe.successes != tc.wantSuccesses {
				t.Fatalf("err=%v probe=%+v", err, probe)
			}
		})
	}
}

func TestImageRetriesEgressFailureBeforeSubmission(t *testing.T) {
	module := &egressModule{failures: 1}
	f := newFixture(t, func(m *fixtureModule) domain.Module { module.fixtureModule = m; return module })
	ctx := context.Background()
	_ = f.s.SetGrant(ctx, "fixture", f.key.ID, true)
	if _, e := f.s.SavePrice(ctx, "fixture", domain.Price{Model: "gpt-image-2", Operation: "image", ImageTicks: 25}); e != nil {
		t.Fatal(e)
	}
	route, e := f.s.CreateModel(ctx, "fixture", ModelInput{PublicID: "image", UpstreamModel: "gpt-image-2", Operation: "image", Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	probe := &egressProbe{}
	probe.install(f.s)
	job, e := f.s.SubmitImages(ctx, f.key, route, domain.ImageRequest{Operation: "image", Prompt: "test", N: 1}, "")
	if e != nil {
		t.Fatal(e)
	}
	_, item, e := f.repo.ClaimImageItem(ctx, "fixture", time.Now(), time.Now().Add(time.Minute), uuid.NewString())
	if e != nil {
		t.Fatal(e)
	}
	f.s.runImageItem(ctx, job, item)
	stored, e := f.repo.GetImageJob(ctx, job.ID, f.key.ID)
	if e != nil || stored.Status != "succeeded" || module.submissions != 1 {
		t.Fatalf("status=%s submissions=%d err=%v", stored.Status, module.submissions, e)
	}
	if probe.acquired != 2 || probe.released != 2 || probe.failures != 1 || probe.successes < 1 {
		t.Fatalf("egress probe %+v", probe)
	}
}
