package muse

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

// The muse.ai web agent picks its own model, so the catalog names the three
// things it can do. Every account serves all of them.
const (
	modelChat  = "muse-spark"
	modelImage = "muse-image"
	modelVideo = "muse-video"
)

// quotaModeWeekly is the weekly usage limit of the Settings → Usage panel,
// shared by chat, images and videos. It is not named "weekly": that mode has
// Grok Web special cases in the quota refresh queue.
const quotaModeWeekly = "weekly_usage"

// Routes is the static Muse model catalog synced at startup.
func Routes() []modeldomain.Route {
	route := func(id string, capability modeldomain.Capability) modeldomain.Route {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderMuse, id)
		return modeldomain.Route{PublicID: publicID, Provider: account.ProviderMuse, UpstreamModel: id, Capability: capability, Origin: modeldomain.OriginCatalog, Enabled: true}
	}
	return []modeldomain.Route{
		route(modelChat, modeldomain.CapabilityChat),
		route(modelImage, modeldomain.CapabilityImage),
		route(modelImage, modeldomain.CapabilityImageEdit),
		route(modelVideo, modeldomain.CapabilityVideo),
	}
}
