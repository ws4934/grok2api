package chatgpt

import (
	"context"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// imageModels lists the image routes and, in order of preference, the ChatGPT
// Web conversation models that may draw them. ChatGPT Web exposes no image
// engines: images come from the conversation's image tool, so the 2.5 names
// are compatibility aliases and cannot force Flare or Sunburst.
var imageModels = []struct {
	id           string
	conversation []string
}{
	{"gpt-image-2", []string{"gpt-5-3"}},
	{"gpt-image-2.5", []string{"gpt-6", "auto"}},
	{"gpt-image-2.5-flare", []string{"gpt-6", "auto"}},
	{"gpt-image-2.5-sunburst", []string{"gpt-6", "auto"}},
}

// imageCatalogTTL bounds how long an account's image-capable models are reused.
const imageCatalogTTL = 10 * time.Minute

type imageCatalog struct {
	models    map[string]bool
	fetchedAt time.Time
}

// imageCandidates returns the conversation models for an image route, the
// configured override alone when set, or nil for a non-image route.
func (a *Adapter) imageCandidates(upstreamModel string) []string {
	for _, item := range imageModels {
		if item.id == upstreamModel {
			if configured := a.config().ImageConversationModel; configured != "" {
				return []string{configured}
			}
			return item.conversation
		}
	}
	return nil
}

func pickImageModel(candidates []string, capable map[string]bool) string {
	for _, candidate := range candidates {
		if capable[candidate] {
			return candidate
		}
	}
	return ""
}

// rememberImageCatalog records which of an account's models have the image tool.
func (a *Adapter) rememberImageCatalog(accountID uint64, capable map[string]bool) {
	if accountID == 0 {
		return
	}
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	if a.catalogs == nil {
		a.catalogs = map[uint64]imageCatalog{}
	}
	a.catalogs[accountID] = imageCatalog{models: capable, fetchedAt: time.Now()}
}

// imageCapableFromCache returns which of models can draw images: the image
// tool flags of the last catalog fetch, or every listed model when unknown.
func (a *Adapter) imageCapableFromCache(accountID uint64, models []string) map[string]bool {
	a.catalogMu.Lock()
	cached, ok := a.catalogs[accountID]
	a.catalogMu.Unlock()
	capable := make(map[string]bool, len(models))
	for _, model := range models {
		capable[model] = !ok || cached.models[model]
	}
	return capable
}

// resolveImageModel picks the conversation model that draws upstreamModel on
// this account. An explicit override is used as configured; otherwise the
// first preferred model whose image tool is enabled wins (gpt-6, then auto).
func (a *Adapter) resolveImageModel(ctx context.Context, credential account.Credential, upstreamModel string) (string, error) {
	if engine, ok := codexImageEngine(upstreamModel); ok {
		return engine, nil
	}
	candidates := a.imageCandidates(upstreamModel)
	if len(candidates) == 0 {
		return "", &upstreamError{Status: http.StatusNotFound, Message: "Unknown ChatGPT image model " + upstreamModel, Body: "unknown image model"}
	}
	if a.config().ImageConversationModel != "" {
		return candidates[0], nil
	}
	catalog, err := a.accountCatalog(ctx, credential)
	if err != nil {
		return "", err
	}
	if model := pickImageModel(candidates, catalog); model != "" {
		return model, nil
	}
	return "", &upstreamError{Status: http.StatusNotFound, Message: "No conversation model of this account can draw " + upstreamModel, Body: "image model unavailable for account"}
}

// accountCatalog returns the account's conversation models, each mapped to
// whether its image tool is enabled, refreshing a stale cached catalog.
func (a *Adapter) accountCatalog(ctx context.Context, credential account.Credential) (map[string]bool, error) {
	a.catalogMu.Lock()
	cached, ok := a.catalogs[credential.ID]
	a.catalogMu.Unlock()
	if ok && time.Since(cached.fetchedAt) <= imageCatalogTTL {
		return cached.models, nil
	}
	if _, err := a.ListModels(ctx, credential); err != nil {
		return nil, err
	}
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	return a.catalogs[credential.ID].models, nil
}
