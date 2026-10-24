package settings

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type runtimeSettingsRepositoryStub struct {
	value     settingsdomain.Config
	updatedAt time.Time
	revision  uint64
	found     bool
	getCount  int
}

func (r *runtimeSettingsRepositoryStub) Get(context.Context) (settingsdomain.Config, time.Time, uint64, bool, error) {
	r.getCount++
	return r.value, r.updatedAt, r.revision, r.found, nil
}

func (r *runtimeSettingsRepositoryStub) Save(_ context.Context, value settingsdomain.Config, expectedRevision uint64) (time.Time, uint64, error) {
	if expectedRevision != r.revision {
		return time.Time{}, 0, repository.ErrConflict
	}
	r.value = value
	r.updatedAt = time.Now().UTC()
	r.revision++
	r.found = true
	return r.updatedAt, r.revision, nil
}

func TestUpdatePersistsAppliesAndReportsRestart(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Server.MaxConcurrentRequests = 2048
	input.ProviderBuild.ResponseHeaderTimeout = "7m"
	input.Routing.MaxAttempts = 5
	input.Routing.PreferFreeBuild = true
	input.Routing.SegmentedSelector = SegmentedSelectorConfig{Enabled: true, MinCandidates: 5000, WindowSize: 96}
	input.Audit.BufferSize = cfg.Audit.BufferSize + 1
	input.Media.MaxTotalBytes = 2 << 30
	input.Media.CleanupThresholdPercent = 75
	input.Media.CleanupInterval = "5m"
	input.Frontend.PublicAPIBaseURL = "https://public.example.com"
	input.ProviderConsole.BaseURL = "https://console.example.com"
	input.ProviderConsole.ChatTimeout = "6m"
	input.ProviderWeb.ClearanceMode = config.ClearanceModeOnDemand
	input.ProviderWeb.FlareSolverrURL = "http://flaresolverr:8191"
	input.Batch = BatchConfig{ImportConcurrency: 26, ConversionConcurrency: 27, SyncConcurrency: 28, RefreshConcurrency: 29, RandomDelay: "750ms"}

	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Routing.MaxAttempts != 5 || !applied.Routing.PreferFreeBuild || !applied.Routing.SegmentedSelectorEnabled || applied.Routing.SegmentedMinCandidates != 5000 || applied.Routing.SegmentedWindowSize != 96 {
		t.Fatalf("runtime configuration was not applied: %#v", applied.Routing)
	}
	if applied.Server.MaxConcurrentRequests != 2048 {
		t.Fatalf("server configuration was not applied: %#v", applied.Server)
	}
	if applied.Provider.Build.ResponseHeaderTimeout.Value() != 7*time.Minute {
		t.Fatalf("Build response header timeout was not applied: %s", applied.Provider.Build.ResponseHeaderTimeout.Value())
	}
	if applied.Media.MaxTotalBytes != 2<<30 || applied.Media.CleanupThresholdPercent != 75 || applied.Media.CleanupInterval.Value() != 5*time.Minute {
		t.Fatalf("media configuration was not applied: %#v", applied.Media)
	}
	if applied.Frontend.PublicAPIBaseURLOverride != "https://public.example.com" || applied.Frontend.EffectivePublicAPIBaseURL() != "https://public.example.com" {
		t.Fatalf("frontend configuration was not applied: %#v", applied.Frontend)
	}
	if applied.Batch.ImportConcurrency != 26 || applied.Batch.ConversionConcurrency != 27 || applied.Batch.SyncConcurrency != 28 || applied.Batch.RefreshConcurrency != 29 || applied.Batch.RandomDelay.Value() != 750*time.Millisecond {
		t.Fatalf("batch configuration was not applied: %#v", applied.Batch)
	}
	if applied.Provider.Console.BaseURL != "https://console.example.com" || applied.Provider.Console.ChatTimeout.Value() != 6*time.Minute {
		t.Fatalf("console configuration was not applied: %#v", applied.Provider.Console)
	}
	if applied.Provider.Web.ClearanceMode != config.ClearanceModeOnDemand || applied.Provider.Web.FlareSolverrURL != "http://flaresolverr:8191" {
		t.Fatalf("on-demand Clearance configuration was not applied: %#v", applied.Provider.Web)
	}
	if len(snapshot.RestartRequired) != 1 || snapshot.RestartRequired[0] != "audit.bufferSize" {
		t.Fatalf("restartRequired = %#v", snapshot.RestartRequired)
	}
	reloaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Server.MaxConcurrentRequests != 2048 || reloaded.Provider.Build.ResponseHeaderTimeout.Value() != 7*time.Minute || reloaded.Routing.MaxAttempts != 5 || !reloaded.Routing.PreferFreeBuild || !reloaded.Routing.SegmentedSelectorEnabled || reloaded.Routing.SegmentedMinCandidates != 5000 || reloaded.Routing.SegmentedWindowSize != 96 || reloaded.Audit.BufferSize != input.Audit.BufferSize || reloaded.Media.MaxTotalBytes != 2<<30 || reloaded.Media.CleanupThresholdPercent != 75 || reloaded.Batch.SyncConcurrency != 28 || reloaded.Batch.RandomDelay.Value() != 750*time.Millisecond || reloaded.Provider.Console.BaseURL != "https://console.example.com" || reloaded.Provider.Web.ClearanceMode != config.ClearanceModeOnDemand {
		t.Fatalf("configuration was not persisted")
	}
}

func TestUpdateRejectsBuildResponseHeaderTimeoutOutsideSafeRange(t *testing.T) {
	for _, value := range []string{"29s", "31m"} {
		t.Run(value, func(t *testing.T) {
			cfg := testConfig(t)
			repository := &runtimeSettingsRepositoryStub{}
			service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
			input := service.Get().Config
			input.ProviderBuild.ResponseHeaderTimeout = value
			if _, err := service.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v", err)
			}
			if repository.found {
				t.Fatal("invalid Build response header timeout was persisted")
			}
		})
	}
}

func TestUpdateRejectsStreamIdleTimeoutBeyondChatTimeout(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*EditableConfig)
	}{
		{
			name: "web",
			mutate: func(input *EditableConfig) {
				input.ProviderWeb.ChatTimeout = "1m"
				input.ProviderWeb.StreamIdleTimeout = "2m"
			},
		},
		{
			name: "console",
			mutate: func(input *EditableConfig) {
				input.ProviderConsole.ChatTimeout = "1m"
				input.ProviderConsole.StreamIdleTimeout = "2m"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig(t)
			repository := &runtimeSettingsRepositoryStub{}
			service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
			input := service.Get().Config
			test.mutate(&input)
			if _, err := service.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
			if repository.found {
				t.Fatal("shadowed stream idle timeout was persisted")
			}
		})
	}
}

func TestUpdateValidatesMaxAttemptsRange(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })

	input := service.Get().Config
	input.Routing.MaxAttempts = 65535
	snapshot, err := service.Update(context.Background(), 0, input)
	if err != nil {
		t.Fatalf("maximum maxAttempts was rejected: %v", err)
	}
	if applied.Routing.MaxAttempts != 65535 || snapshot.Config.Routing.MaxAttempts != 65535 {
		t.Fatalf("maximum maxAttempts was not applied: applied=%d snapshot=%d", applied.Routing.MaxAttempts, snapshot.Config.Routing.MaxAttempts)
	}

	input = snapshot.Config
	input.Routing.MaxAttempts = -1
	snapshot, err = service.Update(context.Background(), snapshot.Revision, input)
	if err != nil {
		t.Fatalf("unlimited maxAttempts was rejected: %v", err)
	}
	if applied.Routing.MaxAttempts != -1 || snapshot.Config.Routing.MaxAttempts != -1 {
		t.Fatalf("unlimited maxAttempts was not applied: applied=%d snapshot=%d", applied.Routing.MaxAttempts, snapshot.Config.Routing.MaxAttempts)
	}

	input = snapshot.Config
	input.Routing.MaxAttempts = 65536
	if _, err := service.Update(context.Background(), snapshot.Revision, input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("maxAttempts above maximum error = %v", err)
	}
	if repository.value.Routing.MaxAttempts != -1 {
		t.Fatalf("invalid maxAttempts was persisted: %d", repository.value.Routing.MaxAttempts)
	}
}

func TestLoadPersistedPreservesExplicitlyDisabledAccountIsolation(t *testing.T) {
	cfg := testConfig(t)
	cfg.Routing.AccountIsolatedConnections = true
	value := toDomainConfig(cfg)
	value.Routing.AccountIsolatedConnections = false
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}

	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.AccountIsolatedConnections {
		t.Fatal("explicitly disabled persisted account isolation was ignored")
	}
}

func TestLoadPersistedPreservesExplicitlyDisabledSegmentedSelector(t *testing.T) {
	cfg := testConfig(t)
	cfg.Routing.SegmentedSelectorEnabled = true
	value := toDomainConfig(cfg)
	value.Routing.SegmentedSelector = settingsdomain.SegmentedSelectorConfig{
		ActiveEnabled: false, MinCandidates: 6000, WindowSize: 128,
	}
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.SegmentedSelectorEnabled || loaded.Routing.SegmentedMinCandidates != 6000 || loaded.Routing.SegmentedWindowSize != 128 {
		t.Fatalf("explicit segmented selector settings were not preserved: %#v", loaded.Routing)
	}
}

func TestLoadPersistedKeepsClearedFlareSolverrURL(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderWeb.FlareSolverrURL = ""
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, &runtimeSettingsRepositoryStub{value: value, found: true})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider.Web.FlareSolverrURL != "" {
		t.Fatalf("FlareSolverr URL = %q, want the persisted empty value", loaded.Provider.Web.FlareSolverrURL)
	}
}

func TestSnapshotIncludesRecommendedBuildBaseline(t *testing.T) {
	service := NewService(testConfig(t), time.Time{}, 0, &runtimeSettingsRepositoryStub{}, nil, nil)
	recommended := service.Get().RecommendedProviderBuild
	if recommended.ClientVersion != config.RecommendedBuildClientVersion || recommended.UserAgent != config.RecommendedBuildUserAgent {
		t.Fatalf("recommended build = %#v", recommended)
	}
}

func TestUpdateRejectsBatchConcurrencyOutsideSafeRange(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	input := service.Get().Config
	input.Batch.ConversionConcurrency = 51
	if _, err := service.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
	if repository.found {
		t.Fatal("invalid batch settings were persisted")
	}
}

func TestBatchRandomDelayCanBeDisabledAndPersisted(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	input := service.Get().Config
	input.Batch.RandomDelay = "0s"
	if _, err := service.Update(context.Background(), 0, input); err != nil {
		t.Fatal(err)
	}
	if repository.value.Batch.RandomDelay == nil || *repository.value.Batch.RandomDelay != 0 {
		t.Fatalf("persisted random delay = %#v", repository.value.Batch.RandomDelay)
	}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Batch.RandomDelay.Value() != 0 {
		t.Fatalf("loaded random delay = %s", loaded.Batch.RandomDelay.Value())
	}
}

func TestUpdateRejectsInvalidDurationWithoutChangingConfig(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	input := service.Get().Config
	input.Routing.StickyTTL = "tomorrow"
	if _, err := service.Update(context.Background(), service.Get().Revision, input); err == nil {
		t.Fatal("expected invalid duration error")
	}
	if service.Get().Config.Routing.StickyTTL != cfg.Routing.StickyTTL.String() || repository.found {
		t.Fatal("invalid update changed or persisted runtime configuration")
	}
}

func TestStatsigManualValueIsWriteOnlyAndClearedByURLMode(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	manual := base64.RawStdEncoding.EncodeToString(make([]byte, 70))
	input := service.Get().Config
	input.ProviderWeb.StatsigMode = config.StatsigModeManual
	input.ProviderWeb.StatsigManualValue = manual

	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Config.ProviderWeb.StatsigManualValue != "" || !snapshot.Config.ProviderWeb.StatsigManualConfigured {
		t.Fatalf("manual value leaked in snapshot: %#v", snapshot.Config.ProviderWeb)
	}
	if repository.value.ProviderWeb.StatsigManualValue != manual {
		t.Fatal("manual value was not persisted")
	}

	keep := service.Get().Config
	if _, err := service.Update(context.Background(), service.Get().Revision, keep); err != nil {
		t.Fatalf("blank write-only value did not preserve existing value: %v", err)
	}
	if repository.value.ProviderWeb.StatsigManualValue != manual {
		t.Fatal("blank write-only value cleared the existing manual value")
	}

	urlMode := service.Get().Config
	urlMode.ProviderWeb.StatsigMode = config.StatsigModeURL
	if _, err := service.Update(context.Background(), service.Get().Revision, urlMode); err != nil {
		t.Fatal(err)
	}
	if repository.value.ProviderWeb.StatsigManualValue != "" {
		t.Fatal("URL mode retained the manual x-statsig-id")
	}
}

func TestLoadPersistedRejectsIncompleteStatsigPayload(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderWeb.StatsigMode = ""
	value.ProviderWeb.StatsigSignerURL = ""
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	if _, _, _, err := LoadPersisted(context.Background(), cfg, repository); err == nil {
		t.Fatal("incomplete Statsig settings were accepted")
	}
}

func TestLoadPersistedRejectsIncompleteBatchPayload(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.Batch = settingsdomain.BatchConfig{}
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	if _, _, _, err := LoadPersisted(context.Background(), cfg, repository); err == nil {
		t.Fatal("incomplete batch settings were accepted")
	}
}

func TestApplyDomainConfigPreservesExplicitCapacitySettings(t *testing.T) {
	base := testConfig(t)
	value := toDomainConfig(base)
	value.Server.MaxConcurrentRequests = 1024
	value.Routing.CapacityWait = 500 * time.Millisecond
	value.ProviderWeb.ChatTimeout = 2 * time.Minute
	value.ClientKeyDefaults.RPMLimit = 120
	value.ClientKeyDefaults.MaxConcurrent = 8

	applied := applyDomainConfig(base, value)
	if applied.Server.MaxConcurrentRequests != 1024 {
		t.Fatalf("maxConcurrentRequests = %d, want 1024", applied.Server.MaxConcurrentRequests)
	}
	if applied.Routing.CapacityWait.Value() != 500*time.Millisecond {
		t.Fatalf("capacityWait = %s, want 500ms", applied.Routing.CapacityWait.Value())
	}
	if applied.Provider.Web.ChatTimeout.Value() != 2*time.Minute {
		t.Fatalf("chatTimeout = %s, want 2m", applied.Provider.Web.ChatTimeout.Value())
	}
	if applied.ClientKeyDefaults.RPMLimit != 120 || applied.ClientKeyDefaults.MaxConcurrent != 8 {
		t.Fatalf("client key defaults = %+v, want 120/8", applied.ClientKeyDefaults)
	}
}

func TestLoadPersistedRejectsPartiallyInvalidConsoleSection(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderConsole.BaseURL = ""
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}

	if _, _, _, err := LoadPersisted(context.Background(), cfg, repository); err == nil {
		t.Fatal("partially invalid Console settings were accepted")
	}
}

func TestReloadPersistedAppliesOnlyNewerVersion(t *testing.T) {
	cfg := testConfig(t)
	updatedAt := time.Now().UTC()
	repository := &runtimeSettingsRepositoryStub{value: toDomainConfig(cfg), updatedAt: updatedAt, revision: 1, found: true}
	applyCount := 0
	service := NewService(cfg, updatedAt, 1, repository, nil, func(config.Config) { applyCount++ })

	if err := service.ReloadPersisted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applyCount != 0 {
		t.Fatalf("unchanged settings applied %d times", applyCount)
	}
	repository.value.Routing.MaxAttempts = cfg.Routing.MaxAttempts + 1
	repository.updatedAt = updatedAt.Add(time.Second)
	repository.revision = 2
	if err := service.ReloadPersisted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applyCount != 1 || service.Get().Config.Routing.MaxAttempts != cfg.Routing.MaxAttempts+1 {
		t.Fatalf("newer settings were not applied")
	}
}

func TestUpdateRejectsStaleRevision(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	input := service.Get().Config
	input.Routing.MaxAttempts++
	if _, err := service.Update(context.Background(), 1, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`secrets:
  jwtSecret: "12345678901234567890123456789012"
  credentialEncryptionKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestLoadPersistedKeepsYAMLFrontendWhenUnset(t *testing.T) {
	cfg := testConfig(t)
	cfg.Frontend.PublicAPIBaseURL = "http://yaml.example.com"
	value := toDomainConfig(cfg)
	value.Frontend = settingsdomain.FrontendConfig{}
	repository := &runtimeSettingsRepositoryStub{
		value: value,
		found: true, revision: 1, updatedAt: time.Now().UTC(),
	}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Frontend.PublicAPIBaseURL != "http://yaml.example.com" || loaded.Frontend.PublicAPIBaseURLOverride != "" || loaded.Frontend.EffectivePublicAPIBaseURL() != "http://yaml.example.com" {
		t.Fatalf("frontend = %#v", loaded.Frontend)
	}
}

func TestUpdateEmptyFrontendOverrideFallsBackToYAML(t *testing.T) {
	cfg := testConfig(t)
	cfg.Frontend.PublicAPIBaseURL = "http://yaml.example.com"
	cfg.Frontend.PublicAPIBaseURLOverride = "http://runtime.example.com"
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Frontend.PublicAPIBaseURL = ""
	if _, err := service.Update(context.Background(), 0, input); err != nil {
		t.Fatal(err)
	}
	if applied.Frontend.PublicAPIBaseURLOverride != "" || applied.Frontend.EffectivePublicAPIBaseURL() != "http://yaml.example.com" {
		t.Fatalf("frontend fallback = %#v", applied.Frontend)
	}
	if repository.value.Frontend.PublicAPIBaseURL != "" {
		t.Fatalf("persisted override = %q", repository.value.Frontend.PublicAPIBaseURL)
	}
}

func TestUpdateAuditCommitDelayRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	repo := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Audit.CommitDelayMS = 12
	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Audit.CommitDelay.Value() != 12*time.Millisecond || snapshot.Config.Audit.CommitDelayMS != 12 {
		t.Fatalf("applied=%s snapshot=%d", applied.Audit.CommitDelay.Value(), snapshot.Config.Audit.CommitDelayMS)
	}
}

func TestUpdateAuditRetentionPreservesExplicitZero(t *testing.T) {
	cfg := testConfig(t)
	cfg.Audit.RetentionDays = 7
	repo := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Audit.RetentionDays = 0

	if _, err := service.Update(context.Background(), service.Get().Revision, input); err != nil {
		t.Fatal(err)
	}
	if applied.Audit.RetentionDays != 0 {
		t.Fatalf("applied audit policy = %#v", applied.Audit)
	}
	if repo.value.Audit.RetentionDays != 0 {
		t.Fatalf("persisted audit policy = %#v", repo.value.Audit)
	}
	reloaded, _, _, err := LoadPersisted(context.Background(), cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Audit.RetentionDays != 0 {
		t.Fatalf("reloaded audit policy = %#v", reloaded.Audit)
	}
}

func TestUpdateRejectsNegativeAuditCommitDelay(t *testing.T) {
	cfg := testConfig(t)
	service := NewService(cfg, time.Time{}, 0, &runtimeSettingsRepositoryStub{}, nil, nil)
	input := service.Get().Config
	input.Audit.CommitDelayMS = -1
	if _, err := service.Update(context.Background(), service.Get().Revision, input); err == nil {
		t.Fatal("negative audit commit delay was accepted")
	}
}

func TestUpdateAccountsAutoCleanRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	repo := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Accounts = AccountsConfig{
		MarkBuildForbiddenReauth:  true,
		BuildForbiddenReauthCodes: []string{" Permission-Denied ", "permission-denied", "team-access-denied"},
		AutoCleanReauthEnabled:    true, AutoCleanReauthInterval: "5m",
		AutoCleanReauthMinAge: "2h", AutoCleanIncludeDisabled: true,
	}
	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Accounts.MarkBuildForbiddenReauth || !applied.Accounts.AutoCleanReauthEnabled || !applied.Accounts.AutoCleanIncludeDisabled {
		t.Fatalf("applied accounts = %#v", applied.Accounts)
	}
	if len(applied.Accounts.BuildForbiddenReauthCodes) != 2 || applied.Accounts.BuildForbiddenReauthCodes[0] != "permission-denied" || applied.Accounts.BuildForbiddenReauthCodes[1] != "team-access-denied" {
		t.Fatalf("normalized Build forbidden codes = %#v", applied.Accounts.BuildForbiddenReauthCodes)
	}
	if applied.Accounts.AutoCleanReauthInterval.Value() != 5*time.Minute || applied.Accounts.AutoCleanReauthMinAge.Value() != 2*time.Hour {
		t.Fatalf("applied accounts durations = %#v", applied.Accounts)
	}
	if !snapshot.Config.Accounts.MarkBuildForbiddenReauth || !snapshot.Config.Accounts.AutoCleanReauthEnabled || snapshot.Config.Accounts.AutoCleanReauthInterval != "5m" {
		t.Fatalf("snapshot accounts = %#v", snapshot.Config.Accounts)
	}
}

func TestUpdateAutoDisableDegradedAccountsRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	cfg.QualityGuard.RequestRetry.AutoDisable = false
	repo := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.Accounts.AutoDisableDegradedAccounts = true

	snapshot, err := service.Update(context.Background(), 0, input)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.QualityGuard.RequestRetry.AutoDisable || !snapshot.Config.Accounts.AutoDisableDegradedAccounts {
		t.Fatalf("auto-disable switch was not applied: applied=%v snapshot=%#v", applied.QualityGuard.RequestRetry.AutoDisable, snapshot.Config.Accounts)
	}
	if !repo.value.Accounts.AutoDisableDegradedAccounts {
		t.Fatalf("auto-disable switch was not persisted: %#v", repo.value.Accounts)
	}
	reloaded, _, _, err := LoadPersisted(context.Background(), cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.QualityGuard.RequestRetry.AutoDisable {
		t.Fatal("persisted auto-disable switch was not restored")
	}
}

func TestUpdateRejectsInvalidBuildForbiddenCodes(t *testing.T) {
	for _, codes := range [][]string{{}, {"contains spaces"}, func() []string {
		values := make([]string, 33)
		for index := range values {
			values[index] = fmt.Sprintf("denial-%d", index)
		}
		return values
	}()} {
		cfg := testConfig(t)
		repository := &runtimeSettingsRepositoryStub{}
		service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
		input := service.Get().Config
		input.Accounts.BuildForbiddenReauthCodes = codes
		if _, err := service.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("codes %#v: error = %v", codes, err)
		}
		if repository.found {
			t.Fatalf("invalid codes were persisted: %#v", codes)
		}
	}
}

func TestProviderChatGPTSettingsRoundTripAndDefaults(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	if input.ProviderChatGPT != (ProviderChatGPTConfig{OutputTokenLimit: 16384, VisionTokens: 256}) {
		t.Fatalf("default providerChatGPT = %#v", input.ProviderChatGPT)
	}
	input.ProviderChatGPT = ProviderChatGPTConfig{
		ImageConversationModel: " gpt-5 ", OutputTokenLimit: 4096, VisionTokens: 0,
		DefaultImageModel: " gpt-image-2 ", CloudflareClearance: true,
	}
	want := config.ChatGPTProviderConfig{ImageConversationModel: "gpt-5", OutputTokenLimit: 4096, DefaultImageModel: "gpt-image-2", CloudflareClearance: true}
	snapshot, err := service.Update(context.Background(), 0, input)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Provider.ChatGPT != want || snapshot.Config.ProviderChatGPT != ProviderChatGPTConfig(want) {
		t.Fatalf("applied = %#v, snapshot = %#v", applied.Provider.ChatGPT, snapshot.Config.ProviderChatGPT)
	}
	reloaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil || reloaded.Provider.ChatGPT != want {
		t.Fatalf("reloaded = %#v, err = %v", reloaded.Provider.ChatGPT, err)
	}
}

func TestUpdateRejectsProviderChatGPTOutOfRange(t *testing.T) {
	for _, value := range []ProviderChatGPTConfig{
		{OutputTokenLimit: 0, VisionTokens: 256},
		{OutputTokenLimit: 131073, VisionTokens: 256},
		{OutputTokenLimit: 16384, VisionTokens: -1},
		{OutputTokenLimit: 16384, VisionTokens: 100001},
	} {
		repository := &runtimeSettingsRepositoryStub{}
		service := NewService(testConfig(t), time.Time{}, 0, repository, nil, nil)
		input := service.Get().Config
		input.ProviderChatGPT = value
		if _, err := service.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%#v: error = %v, want ErrInvalidInput", value, err)
		}
		if repository.found {
			t.Fatalf("%#v was persisted", value)
		}
	}
}

func TestProviderAdobeSettingsRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	if input.ProviderAdobe != (ProviderAdobeConfig{SherlockMintAPI: "http://mint:8777", Timeout: "1m"}) {
		t.Fatalf("default providerAdobe = %#v", input.ProviderAdobe)
	}
	input.ProviderAdobe = ProviderAdobeConfig{SherlockMintAPI: " http://127.0.0.1:9000 ", Timeout: "2m"}
	want := config.AdobeProviderConfig{SherlockMintAPI: "http://127.0.0.1:9000", Timeout: config.Duration(2 * time.Minute)}
	snapshot, err := service.Update(context.Background(), 0, input)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Provider.Adobe != want || snapshot.Config.ProviderAdobe != (ProviderAdobeConfig{SherlockMintAPI: "http://127.0.0.1:9000", Timeout: "2m"}) {
		t.Fatalf("applied = %#v, snapshot = %#v", applied.Provider.Adobe, snapshot.Config.ProviderAdobe)
	}
	reloaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil || reloaded.Provider.Adobe != want {
		t.Fatalf("reloaded = %#v, err = %v", reloaded.Provider.Adobe, err)
	}
	// An empty mint address disables Adobe submits and a zero timeout means 60s;
	// both must load instead of failing startup.
	repository.value.ProviderAdobe = settingsdomain.ProviderAdobeConfig{}
	if reloaded, _, _, err = LoadPersisted(context.Background(), cfg, repository); err != nil || reloaded.Provider.Adobe != (config.AdobeProviderConfig{}) {
		t.Fatalf("zero section: reloaded = %#v, err = %v", reloaded.Provider.Adobe, err)
	}
}

func TestUpdateRejectsInvalidProviderAdobe(t *testing.T) {
	for _, value := range []ProviderAdobeConfig{
		{SherlockMintAPI: "http://mint.example.com:8777", Timeout: "1m"},
		{SherlockMintAPI: "http://mint:8777/?token=x", Timeout: "1m"},
		{SherlockMintAPI: "http://mint:8777", Timeout: "4s"},
		{SherlockMintAPI: "http://mint:8777", Timeout: "31m"},
		{SherlockMintAPI: "http://mint:8777", Timeout: "-1m"},
		{SherlockMintAPI: "http://mint:8777", Timeout: ""},
	} {
		repository := &runtimeSettingsRepositoryStub{}
		service := NewService(testConfig(t), time.Time{}, 0, repository, nil, nil)
		input := service.Get().Config
		input.ProviderAdobe = value
		if _, err := service.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%#v: error = %v, want ErrInvalidInput", value, err)
		}
		if repository.found {
			t.Fatalf("%#v was persisted", value)
		}
	}
}

func TestAdobeMintTestValidatesTheAddressFirst(t *testing.T) {
	service := NewService(testConfig(t), time.Time{}, 0, &runtimeSettingsRepositoryStub{}, nil, nil)
	var tested []string
	service.SetAdobeMintTester(func(_ context.Context, base string) error {
		tested = append(tested, base)
		if base == "http://mint:9999" {
			return errors.New("connection refused")
		}
		return nil
	})
	for _, base := range []string{"", "  ", "http://mint.example.com:8777", "http://mint:8777/?a=1"} {
		if _, err := service.TestAdobeMint(context.Background(), base); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%q: error = %v, want ErrInvalidInput", base, err)
		}
	}
	if _, err := service.TestAdobeMint(context.Background(), " http://mint:8777 "); err != nil {
		t.Fatal(err)
	}
	if _, err := service.TestAdobeMint(context.Background(), "http://mint:9999"); err == nil || errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unreachable mint: error = %v", err)
	}
	if !reflect.DeepEqual(tested, []string{"http://mint:8777", "http://mint:9999"}) {
		t.Fatalf("tested = %v", tested)
	}
}

// Fields only the YAML file sets must survive applying persisted settings.
func TestApplyingSettingsKeepsFileOnlyFields(t *testing.T) {
	base := testConfig(t)
	base.Routing.AutoAssignMaxNodeShare = 0.4
	base.Routing.AutoAssignMaxMigrationShare = 0.2
	if got := applyDomainConfig(base, toDomainConfig(base)); !reflect.DeepEqual(got, base) {
		t.Fatalf("round trip changed the config:\n got %+v\nwant %+v", got, base)
	}
}

func TestUpdateAutoDeleteConsoleUnlinkedDefaultsOffAndRoundTrips(t *testing.T) {
	cfg := testConfig(t)
	repo := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repo, nil, func(next config.Config) { applied = next })
	if service.Get().Config.Accounts.AutoDeleteConsoleUnlinked {
		t.Fatal("auto-delete of unlinked Console accounts must default to off")
	}
	input := service.Get().Config
	input.Accounts.AutoDeleteConsoleUnlinked = true
	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Accounts.AutoDeleteConsoleUnlinked || !snapshot.Config.Accounts.AutoDeleteConsoleUnlinked {
		t.Fatalf("applied = %#v snapshot = %#v", applied.Accounts, snapshot.Config.Accounts)
	}
	input = snapshot.Config
	input.Accounts.AutoDeleteConsoleUnlinked = false
	snapshot, err = service.Update(context.Background(), snapshot.Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Accounts.AutoDeleteConsoleUnlinked || snapshot.Config.Accounts.AutoDeleteConsoleUnlinked {
		t.Fatal("the switch did not turn off")
	}
}
