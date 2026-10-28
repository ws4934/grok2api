package media

type mediaAssetDTO struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	MimeType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
	// Origin fields are empty for uploads and assets saved before provenance.
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	UpstreamModel string `json:"upstreamModel"`
	ClientKeyID   uint64 `json:"clientKeyId"`
	ClientKeyName string `json:"clientKeyName"`
	AccountID     uint64 `json:"accountId"`
	AccountName   string `json:"accountName"`
	Operation     string `json:"operation"`
	JobID         string `json:"jobId"`
	RequestID     string `json:"requestId"`
	Prompt        string `json:"prompt"`
}

type imageStatsDTO struct {
	TotalImages int64 `json:"totalImages"`
	TotalBytes  int64 `json:"totalBytes"`
}

type mediaJobDTO struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Operation string `json:"operation"`
	Prompt    string `json:"prompt"`
	Status    string `json:"status"`
	Progress  int    `json:"progress"`
	Seconds   int    `json:"seconds"`
	Size      string `json:"size"`
	Quality   string `json:"quality"`
	// AssetID is the video result; AssetIDs/AssetURLs list every result.
	AssetID       string   `json:"assetId"`
	AssetIDs      []string `json:"assetIds"`
	AssetURLs     []string `json:"assetUrls"`
	AccountName   string   `json:"accountName"`
	ClientKeyName string   `json:"clientKeyName"`
	CreatedAt     string   `json:"createdAt"`
	CompletedAt   *string  `json:"completedAt"`
	ErrorCode     string   `json:"errorCode"`
	ErrorMessage  string   `json:"errorMessage"`
	Cancellable   bool     `json:"cancellable"`
	// Retryable: an uncertain job (retried as a new job) or a failed image job
	// whose drawn image can be fetched again (resumed in place).
	Retryable    bool `json:"retryable"`
	UsagePending bool `json:"usagePending"`
}

// retryJobDTO is the retried job; Resumed means it is the same job fetching
// its result again rather than a new job.
type retryJobDTO struct {
	mediaJobDTO
	Resumed bool `json:"resumed"`
}

type jobStatsDTO struct {
	TotalJobs  int64 `json:"totalJobs"`
	Queued     int64 `json:"queued"`
	InProgress int64 `json:"inProgress"`
	Completed  int64 `json:"completed"`
	Failed     int64 `json:"failed"`
	Canceled   int64 `json:"canceled"`
	Uncertain  int64 `json:"uncertain"`
}
