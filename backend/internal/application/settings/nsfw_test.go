package settings

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestNSFWPersistedSettingsOverrideFileAndHotReloadBothWays(t *testing.T) {
	cfg := testConfig(t)
	cfg.Provider.Web.AllowNSFW = true
	value := toDomainConfig(cfg)
	value.ProviderWeb.AllowNSFW = false
	repo := &runtimeSettingsRepositoryStub{value: value, found: true}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider.Web.AllowNSFW {
		t.Fatal("file unexpectedly overrides saved runtime value")
	}
	var applied config.Config
	service := NewService(loaded, time.Time{}, 0, repo, nil, func(next config.Config) { applied = next })
	for _, flag := range []bool{true, false} {
		current := service.Get()
		input := current.Config
		input.ProviderWeb.AllowNSFW = flag
		updated, err := service.Update(context.Background(), current.Revision, input)
		if err != nil || applied.Provider.Web.AllowNSFW != flag || updated.Config.ProviderWeb.AllowNSFW != flag || repo.value.ProviderWeb.AllowNSFW != flag {
			t.Fatalf("NSFW hot reload flag=%v: snapshot=%+v err=%v", flag, updated.Config.ProviderWeb, err)
		}
	}
}
