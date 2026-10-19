package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

func (a *Adapter) ListModels(ctx context.Context, credential account.Credential) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	// Always request the model catalog from the Build primary; do not preemptively switch to XAI because 1.5 or Super entitlement is absent.
	// NormalizeAccountModelCapabilities fills session-contract capabilities such
	// as Composer and paid video entitlement locally.
	models, status, err := a.listModelsAt(ctx, credential, accessToken, a.primaryBaseURL())
	if err != nil {
		return nil, err
	}
	if models != nil {
		return models, nil
	}
	return nil, fmt.Errorf("上游模型接口返回 %d", status)
}

// NormalizeAccountModelCapabilities normalizes capabilities that the OAuth
// session contract exposes independently of the account's sparse /models list.
// Composer is available to Build OAuth sessions independently of the sparse
// live catalog. Grok 4.6 sessions retain the still-supported Grok 4.5 route for
// backwards compatibility. Super always includes video 1.5; Free and Unknown
// remove video 1.5 exactly. BuildAPIFallback is ignored.
func (a *Adapter) NormalizeAccountModelCapabilities(models []string, billing *account.Billing, credential account.Credential) []string {
	super := account.IsBuildSuper(credential, billing)
	composer := credential.Provider == account.ProviderBuild && credential.AuthType == account.AuthTypeOAuth
	result := make([]string, 0, len(models)+2)
	seen := make(map[string]struct{}, len(models)+2)
	hasVideo15 := false
	hasGrok46 := false
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		if model == buildVideoModel {
			if !super {
				continue
			}
			hasVideo15 = true
		}
		if model == buildGrok46Model {
			hasGrok46 = true
		}
		seen[model] = struct{}{}
		result = append(result, model)
	}
	if credential.Provider == account.ProviderBuild && hasGrok46 {
		if _, exists := seen[buildGrok45Model]; !exists {
			seen[buildGrok45Model] = struct{}{}
			result = append(result, buildGrok45Model)
		}
	}
	if super && !hasVideo15 {
		result = append(result, buildVideoModel)
	}
	if composer {
		if _, exists := seen[modeldomain.GrokComposer25Fast]; !exists {
			result = append(result, modeldomain.GrokComposer25Fast)
		}
	}
	return result
}

type buildModelCatalogEntry struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	ModelID string `json:"modelId"`
	Hidden  bool   `json:"hidden"`
	Meta    struct {
		Model   string `json:"model"`
		ModelID string `json:"modelId"`
		Hidden  bool   `json:"hidden"`
	} `json:"_meta"`
	// Reasoning and budget metadata, mirroring the keys grok-build reads in
	// xai-grok-shell remote/client.rs parse_remote_model_value. Snake and camel
	// case spellings are both accepted there, so both are accepted here.
	ContextWindow            int64  `json:"context_window"`
	ContextWindowCamel       int64  `json:"contextWindow"`
	MaxCompletionTokens      int64  `json:"max_completion_tokens"`
	MaxCompletionTokensCamel int64  `json:"maxCompletionTokens"`
	ReasoningEffort          string `json:"reasoning_effort"`
	ReasoningEffortCamel     string `json:"reasoningEffort"`
	SupportsReasoningEffort  *bool  `json:"supports_reasoning_effort"`
	SupportsReasoningCamel   *bool  `json:"supportsReasoningEffort"`
	SupportsBackendSearch    bool   `json:"supports_backend_search"`
	// reasoning_efforts entries are either bare strings ("high") or objects
	// {value,id,label,description,default}; decode lazily per entry.
	ReasoningEfforts      []json.RawMessage `json:"reasoning_efforts"`
	ReasoningEffortsCamel []json.RawMessage `json:"reasoningEfforts"`
}

// modelIdentifier keeps the legacy top-level id authoritative and uses the
// additional official Grok Build shapes only as fallbacks. This makes catalog
// parsing additive: existing route IDs never change merely because model or
// modelId metadata appears alongside id.
func (e buildModelCatalogEntry) modelIdentifier() string {
	if e.Hidden || e.Meta.Hidden {
		return ""
	}
	return firstNonEmpty(e.ID, e.Model, e.ModelID, e.Meta.Model, e.Meta.ModelID)
}

// reasoningMenu decodes reasoning_efforts the way grok-build's
// parse_reasoning_effort_options does: each entry is a bare effort string or an
// object with a required `value`; unreadable entries are skipped instead of
// discarding the whole menu. It returns the ordered values and the entry
// flagged default (empty when none is flagged).
func (e buildModelCatalogEntry) reasoningMenu() (values []string, defaultValue string) {
	raw := e.ReasoningEfforts
	if len(raw) == 0 {
		raw = e.ReasoningEffortsCamel
	}
	for _, entry := range raw {
		var bare string
		if err := json.Unmarshal(entry, &bare); err == nil {
			if bare = strings.TrimSpace(bare); bare != "" {
				values = append(values, bare)
			}
			continue
		}
		var option struct {
			Value   string `json:"value"`
			Default bool   `json:"default"`
		}
		if err := json.Unmarshal(entry, &option); err != nil {
			continue
		}
		value := strings.TrimSpace(option.Value)
		if value == "" {
			continue
		}
		values = append(values, value)
		if option.Default && defaultValue == "" {
			defaultValue = value
		}
	}
	return values, defaultValue
}

// upstreamProfile projects the catalog entry onto the domain profile.
func (e buildModelCatalogEntry) upstreamProfile() modeldomain.UpstreamModelProfile {
	menu, menuDefault := e.reasoningMenu()
	supports := false
	switch {
	case e.SupportsReasoningEffort != nil:
		supports = *e.SupportsReasoningEffort
	case e.SupportsReasoningCamel != nil:
		supports = *e.SupportsReasoningCamel
	}
	contextWindow := e.ContextWindow
	if contextWindow <= 0 {
		contextWindow = e.ContextWindowCamel
	}
	maxCompletion := e.MaxCompletionTokens
	if maxCompletion <= 0 {
		maxCompletion = e.MaxCompletionTokensCamel
	}
	return modeldomain.UpstreamModelProfile{
		ReasoningEfforts:        menu,
		DefaultReasoningEffort:  firstNonEmpty(menuDefault, e.ReasoningEffort, e.ReasoningEffortCamel),
		SupportsReasoningEffort: supports,
		ContextWindow:           clampInt(contextWindow),
		MaxCompletionTokens:     clampInt(maxCompletion),
		SupportsBackendSearch:   e.SupportsBackendSearch,
	}
}

func clampInt(value int64) int {
	if value <= 0 {
		return 0
	}
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(value)
}

func (a *Adapter) listModelsAt(ctx context.Context, credential account.Credential, accessToken, base string) ([]string, int, error) {
	requestCtx := infraegress.WithCredential(ctx, credential)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, a.urlWithBase(base, "/models"), nil)
	if err != nil {
		return nil, 0, err
	}
	if err := a.applyHeaders(req, credential, accessToken, "", "", false); err != nil {
		return nil, 0, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, resp.StatusCode, err
	}
	models := make([]string, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, raw := range payload.Data {
		var item buildModelCatalogEntry
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		identifier := item.modelIdentifier()
		if identifier == "" {
			continue
		}
		if _, exists := seen[identifier]; exists {
			continue
		}
		seen[identifier] = struct{}{}
		models = append(models, identifier)
		// Publish the live reasoning menu / context budget so effort validation,
		// aliases, and catalog output follow the upstream catalog like grok-build does.
		modeldomain.RegisterUpstreamModelProfile(identifier, item.upstreamProfile())
	}
	a.recordModelsETag(credential.ID, resp.Header.Get("ETag"))
	return models, resp.StatusCode, nil
}

func (a *Adapter) recordModelsETag(accountID uint64, etag string) {
	etag = strings.TrimSpace(etag)
	if etag == "" {
		return
	}
	a.modelsMu.Lock()
	if a.modelsETags == nil {
		a.modelsETags = make(map[uint64]string)
	}
	a.modelsETags[accountID] = etag
	a.modelsMu.Unlock()
}

func (a *Adapter) modelCatalogChanged(accountID uint64, etag string) bool {
	etag = strings.TrimSpace(etag)
	if etag == "" {
		return false
	}
	a.modelsMu.Lock()
	defer a.modelsMu.Unlock()
	if a.modelsETags == nil {
		a.modelsETags = make(map[uint64]string)
	}
	current := a.modelsETags[accountID]
	if current == "" {
		// After a process restart there is no in-memory catalog baseline. Let the Gateway perform one account-level
		// /models sync; recordModelsETag establishes the baseline after success.
		return true
	}
	return current != etag
}
