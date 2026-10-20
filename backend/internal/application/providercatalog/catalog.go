// Package providercatalog publishes the registered providers to admin clients
// so every surface lists, labels and gates providers from one source.
package providercatalog

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type Conversation struct {
	Responses       bool `json:"responses"`
	ChatCompletions bool `json:"chatCompletions"`
	Messages        bool `json:"messages"`
}

type Media struct {
	ImageGeneration bool `json:"imageGeneration"`
	ImageEdit       bool `json:"imageEdit"`
	VideoGeneration bool `json:"videoGeneration"`
	TTS             bool `json:"tts"`
	STT             bool `json:"stt"`
	Realtime        bool `json:"realtime"`
	// AsyncImages providers run images as durable jobs (Responses image_generation with background).
	AsyncImages bool `json:"asyncImages"`
}

type Credential struct {
	AuthType      string `json:"authType"`
	Import        bool   `json:"import"`
	Refresh       bool   `json:"refresh"`
	DeviceOAuth   bool   `json:"deviceOAuth"`
	RedirectOAuth bool   `json:"redirectOAuth"`
}

type Entry struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	ShortName    string       `json:"shortName"`
	Order        int          `json:"order"`
	Capabilities []string     `json:"capabilities"`
	Conversation Conversation `json:"conversation"`
	Media        Media        `json:"media"`
	Quota        string       `json:"quota"`
	// QuotaModes lists the quota window modes shown for the provider's accounts; never null.
	QuotaModes []string `json:"quotaModes"`
	Usage      string   `json:"usage"`
	TierScoped bool     `json:"tierScoped"`
	// DefaultMaxConcurrent is the per-account concurrency given to new accounts.
	DefaultMaxConcurrent int        `json:"defaultMaxConcurrent"`
	EgressScopes         []string   `json:"egressScopes"`
	Credential           Credential `json:"credential"`
}

// Catalog lists registered providers in account.Providers() order.
type Catalog struct {
	registry *provider.Registry
}

func New(registry *provider.Registry) *Catalog {
	return &Catalog{registry: registry}
}

func (c *Catalog) Entries() []Entry {
	out := []Entry{}
	if c.registry != nil {
		for _, value := range c.registry.Providers() {
			if definition, ok := c.registry.Definition(value); ok {
				entry := entryFor(definition)
				_, entry.Media.AsyncImages = c.registry.AsyncImages(value)
				out = append(out, entry)
			}
		}
	}
	return out
}

func entryFor(d provider.Definition) Entry {
	entry := Entry{
		ID: string(d.Provider), Name: d.DisplayName(), ShortName: d.ShortName(), Order: d.Provider.Order(),
		Conversation: Conversation{Responses: d.Conversation.Responses, ChatCompletions: d.Conversation.ChatCompletions, Messages: d.Conversation.Messages},
		Media:        Media{ImageGeneration: d.Media.ImageGeneration, ImageEdit: d.Media.ImageEdit, VideoGeneration: d.Media.VideoGeneration, TTS: d.Media.TTS, STT: d.Media.STT, Realtime: d.Media.Realtime},
		Quota:        string(d.Quota), QuotaModes: append([]string{}, d.Profile.QuotaModes...), Usage: string(d.Inference.Usage), TierScoped: !d.Profile.TierExempt,
		DefaultMaxConcurrent: account.DefaultMaxConcurrent,
		Credential:           Credential{AuthType: string(d.Credential.AuthType), Import: d.Credential.Import, Refresh: d.Credential.Refresh, DeviceOAuth: d.Credential.DeviceOAuth, RedirectOAuth: d.Credential.RedirectOAuth},
	}
	if d.Profile.DefaultMaxConcurrent > 0 {
		entry.DefaultMaxConcurrent = d.Profile.DefaultMaxConcurrent
	}
	for _, capability := range d.ModelCapabilities {
		entry.Capabilities = append(entry.Capabilities, string(capability))
	}
	for _, scope := range d.EgressScopes() {
		entry.EgressScopes = append(entry.EgressScopes, string(scope))
	}
	return entry
}
