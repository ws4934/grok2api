package inference

import (
	"encoding/json"
	"math"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
)

func normalizeMetadataUsage(metadata responseMetadata, protocol streamProtocol) responseMetadata {
	if protocol != streamProtocolAnthropic {
		return metadata
	}
	inputTokens := saturatingUsageSum(metadata.Usage.InputTokens, metadata.Usage.CachedInputTokens, metadata.cacheCreationInputTokens)
	metadata.Usage.InputTokens = inputTokens
	metadata.Usage.TotalTokens = saturatingUsageSum(inputTokens, metadata.Usage.OutputTokens)
	return metadata
}

func saturatingUsageSum(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if value > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += value
	}
	return total
}

func extractMetadata(data []byte) responseMetadata {
	var root responsePayloadDTO
	if json.Unmarshal(data, &root) != nil {
		return responseMetadata{}
	}
	metadata := responseMetadata{ResponseID: root.ID, Model: root.Model, SequenceNumber: root.SequenceNumber}
	usage := root.Usage
	if root.Response != nil {
		if metadata.ResponseID == "" {
			metadata.ResponseID = root.Response.ID
		}
		if metadata.Model == "" {
			metadata.Model = root.Response.Model
		}
		if metadata.SequenceNumber == 0 {
			metadata.SequenceNumber = root.Response.SequenceNumber
		}
		if usage == nil {
			usage = root.Response.Usage
		}
	}
	if usage == nil {
		return metadata
	}
	metadata.Usage = usage.toGatewayUsage(metadata.Model)
	metadata.cacheCreationInputTokens = usage.CacheCreationInputTokens
	return metadata
}

type responsePayloadDTO struct {
	ID             string              `json:"id"`
	Model          string              `json:"model"`
	SequenceNumber int64               `json:"sequence_number"`
	Usage          *responseUsageDTO   `json:"usage"`
	Response       *responsePayloadDTO `json:"response"`
}

type responseUsageDTO struct {
	InputTokens            int64 `json:"input_tokens"`
	InputTokensCamel       int64 `json:"inputTokens"`
	OutputTokens           int64 `json:"output_tokens"`
	OutputTokensCamel      int64 `json:"outputTokens"`
	TotalTokens            int64 `json:"total_tokens"`
	TotalTokensCamel       int64 `json:"totalTokens"`
	CostInUSDTicks         int64 `json:"cost_in_usd_ticks"`
	NumSourcesUsed         int64 `json:"num_sources_used"`
	NumServerSideToolsUsed int64 `json:"num_server_side_tools_used"`
	// Responses protocol: input_tokens_details.cached_tokens
	InputTokensDetails responseInputDetailsDTO `json:"input_tokens_details"`
	// OpenAI Chat Completions protocol: prompt_tokens_details.cached_tokens
	PromptTokensDetails responseInputDetailsDTO `json:"prompt_tokens_details"`
	// Anthropic Messages protocol: top-level cache_read_input_tokens
	CacheReadInputTokens     int64                    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64                    `json:"cache_creation_input_tokens"`
	OutputTokensDetails      responseOutputDetailsDTO `json:"output_tokens_details"`
	// OpenAI Chat Completions protocol: completion_tokens_details.reasoning_tokens
	CompletionTokensDetails responseOutputDetailsDTO  `json:"completion_tokens_details"`
	ContextDetails          responseContextDetailsDTO `json:"context_details"`
	PromptTokens            int64                     `json:"prompt_tokens"`
	CompletionTokens        int64                     `json:"completion_tokens"`
}

type responseInputDetailsDTO struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type responseOutputDetailsDTO struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
	ThinkingTokens  int64 `json:"thinking_tokens"`
}

type responseContextDetailsDTO struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (value responseUsageDTO) toGatewayUsage(responseModel string) gateway.Usage {
	input := value.InputTokens
	if input == 0 {
		input = value.InputTokensCamel
	}
	if input == 0 {
		input = value.PromptTokens
	}
	output := value.OutputTokens
	if output == 0 {
		output = value.OutputTokensCamel
	}
	if output == 0 {
		output = value.CompletionTokens
	}
	total := value.TotalTokens
	if total == 0 {
		total = value.TotalTokensCamel
	}
	if total == 0 {
		total = input + output
	}
	// Unified cache hits: Responses / Chat Completions / Anthropic Messages
	cached := value.InputTokensDetails.CachedTokens
	if cached == 0 {
		cached = value.PromptTokensDetails.CachedTokens
	}
	if cached == 0 {
		cached = value.CacheReadInputTokens
	}
	reasoning := value.OutputTokensDetails.ReasoningTokens
	if reasoning == 0 {
		reasoning = value.CompletionTokensDetails.ReasoningTokens
	}
	if reasoning == 0 {
		reasoning = value.OutputTokensDetails.ThinkingTokens
	}
	return gateway.Usage{
		Reported:    true,
		InputTokens: input, CachedInputTokens: cached,
		OutputTokens: output, ReasoningTokens: reasoning,
		TotalTokens: total, CostInUSDTicks: value.CostInUSDTicks,
		NumSourcesUsed: value.NumSourcesUsed, NumServerSideToolsUsed: value.NumServerSideToolsUsed,
		ContextInputTokens: value.ContextDetails.InputTokens, ContextOutputTokens: value.ContextDetails.OutputTokens,
		ResponseModel: responseModel,
	}
}

func hasUsageMetadata(usage gateway.Usage) bool {
	return usage.Reported || usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 ||
		usage.CachedInputTokens > 0 || usage.ReasoningTokens > 0 || usage.CostInUSDTicks > 0 ||
		usage.NumSourcesUsed > 0 || usage.NumServerSideToolsUsed > 0 ||
		usage.ContextInputTokens > 0 || usage.ContextOutputTokens > 0
}

// mergeGatewayUsage merges usage from multiple streaming frames; non-zero fields overwrite,
// preventing a later partial frame from erasing an already parsed cache hit.
func mergeGatewayUsage(base, next gateway.Usage) gateway.Usage {
	base.Reported = base.Reported || next.Reported
	if next.InputTokens > 0 {
		base.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		base.OutputTokens = next.OutputTokens
	}
	if next.TotalTokens > 0 {
		base.TotalTokens = next.TotalTokens
	}
	if next.CachedInputTokens > 0 {
		base.CachedInputTokens = next.CachedInputTokens
	}
	if next.ReasoningTokens > 0 {
		base.ReasoningTokens = next.ReasoningTokens
	}
	if next.CostInUSDTicks > 0 {
		base.CostInUSDTicks = next.CostInUSDTicks
	}
	if next.NumSourcesUsed > 0 {
		base.NumSourcesUsed = next.NumSourcesUsed
	}
	if next.NumServerSideToolsUsed > 0 {
		base.NumServerSideToolsUsed = next.NumServerSideToolsUsed
	}
	if next.ContextInputTokens > 0 {
		base.ContextInputTokens = next.ContextInputTokens
	}
	if next.ContextOutputTokens > 0 {
		base.ContextOutputTokens = next.ContextOutputTokens
	}
	if next.ResponseModel != "" {
		base.ResponseModel = next.ResponseModel
	}
	if base.TotalTokens == 0 && (base.InputTokens > 0 || base.OutputTokens > 0) {
		base.TotalTokens = base.InputTokens + base.OutputTokens
	}
	return base
}
