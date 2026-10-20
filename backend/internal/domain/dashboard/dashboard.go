package dashboard

// Resources 表示 Dashboard 所需的资源总量与可用量。
type Resources struct {
	ActiveAccounts     int64
	TotalAccounts      int64
	AccountsByProvider map[string]int64
	EnabledModels      int64
	TotalModels        int64
	ActiveClientKeys   int64
	TotalClientKeys    int64
}

// Usage 表示指定时间窗口内的请求聚合。
type Usage struct {
	Requests           int64
	SuccessfulRequests int64
	FailedRequests     int64
	InputTokens        int64
	CachedInputTokens  int64
	OutputTokens       int64
	ReasoningTokens    int64
	Tokens             int64
	BilledCostUSDTicks int64
	FirstTokenSamples  int64
	FirstTokenTotalMS  int64
	ThroughputSamples  int64
	ThroughputTokens   int64
	GenerationTotalMS  int64
}

// Bucket 表示一个固定时间桶内的请求和 token 数量。
type Bucket struct {
	Index              int
	Requests           int64
	InputTokens        int64
	CachedInputTokens  int64
	OutputTokens       int64
	ReasoningTokens    int64
	Tokens             int64
	BilledCostUSDTicks int64
}

// ModelUsage 表示指定时间范围内按公开模型聚合的调用量。
type ModelUsage struct {
	Model              string
	Requests           int64
	InputTokens        int64
	CachedInputTokens  int64
	OutputTokens       int64
	ReasoningTokens    int64
	Tokens             int64
	BilledCostUSDTicks int64
}

// ActivityBucket 表示活动热力图中的单日请求量。
type ActivityBucket struct {
	Index    int
	Requests int64
}

// ProviderUsage 表示指定时间范围内单个上游渠道的调用量。
type ProviderUsage struct {
	Provider           string
	Requests           int64
	SuccessfulRequests int64
	Tokens             int64
}

// MediaQuotaSource names the quota windows of one provider that make up one
// media quota kind (image, video or credits).
type MediaQuotaSource struct {
	Provider string
	Kind     string
	Modes    []string
}

// MediaQuota sums one provider's media quota kind over the enabled, logged-in
// accounts that have it. A window past its reset time counts as full;
// Total is 0 when the upstream reports only what is left (ChatGPT).
type MediaQuota struct {
	Provider  string
	Kind      string
	Remaining int64
	Total     int64
	Accounts  int64
}

// Aggregate 表示持久化层返回的 Dashboard 聚合快照。
type Aggregate struct {
	Resources       Resources
	Usage           Usage
	Buckets         []Bucket
	ActivityBuckets []ActivityBucket
	TopModels       []ModelUsage
	Providers       []ProviderUsage
	MediaQuota      []MediaQuota
}
