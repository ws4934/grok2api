package media

import "context"

// Origin records who produced a media asset. The gateway attaches it to the
// request context; asset storage reads it when saving generated media.
type Origin struct {
	Provider      string
	Model         string
	UpstreamModel string
	ClientKeyID   uint64
	// ClientKeyName and AccountName are snapshots that survive key/account deletion.
	ClientKeyName string
	AccountID     uint64
	AccountName   string
	// Operation is the audit operation, e.g. image, image_edit, video.
	Operation string
	JobID     string
	RequestID string
	// Prompt is stored truncated; it is display metadata only.
	Prompt string
}

type originKey struct{}

// WithOrigin returns a context whose saved assets are attributed to origin.
func WithOrigin(ctx context.Context, origin Origin) context.Context {
	return context.WithValue(ctx, originKey{}, origin)
}

// OriginFrom returns the origin attached by WithOrigin.
func OriginFrom(ctx context.Context) (Origin, bool) {
	origin, ok := ctx.Value(originKey{}).(Origin)
	return origin, ok
}

// JobOrigin attributes assets produced by a media job to that job.
func JobOrigin(job Job, operation string) Origin {
	return Origin{
		Provider: job.Provider, Model: job.Model, UpstreamModel: job.UpstreamModel,
		ClientKeyID: job.ClientKeyID, ClientKeyName: job.ClientKeyName, AccountID: job.AccountID, AccountName: job.AccountName,
		Operation: operation, JobID: job.ID, RequestID: job.RequestID, Prompt: job.Prompt,
	}
}
