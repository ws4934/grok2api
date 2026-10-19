package egress

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

// A fully populated 2,000-node guard state is slightly larger than 1 MiB.
// Keep a bounded limit while leaving headroom for audit cursors and events.
const maxQualityGuardStateBytes = 8 << 20

type qualityGuardState struct {
	Version           int                              `json:"version"`
	StartedAt         float64                          `json:"started_at"`
	UpdatedAt         float64                          `json:"updated_at"`
	LastActiveCycleAt float64                          `json:"last_active_cycle_at"`
	LastPassivePollAt float64                          `json:"last_passive_poll_at"`
	Guard             qualityGuardConfig               `json:"guard"`
	ProtectedNodeIDs  []string                         `json:"protected_node_ids"`
	Nodes             map[string]qualityGuardNodeState `json:"nodes"`
	RecentEvents      []qualityGuardEvent              `json:"recent_events"`
	Statistics        qualityGuardStatistics           `json:"statistics"`
	NodeSummary       qualityGuardNodeSummary          `json:"-"`
}

type qualityGuardNodeSummary struct {
	Total             int
	Quarantined       int
	QuarantinedLeases int
}

type qualityGuardStateCache struct {
	path     string
	fileInfo os.FileInfo
	state    qualityGuardState
	valid    bool
}

type qualityGuardStatistics struct {
	StartedAt float64                    `json:"started_at"`
	Active    qualityGuardDetectionStats `json:"active"`
	Passive   qualityGuardDetectionStats `json:"passive"`
	Actions   qualityGuardActionStats    `json:"actions"`
}

type qualityGuardDetectionStats struct {
	Total        uint64 `json:"total"`
	Healthy      uint64 `json:"healthy"`
	Soft         uint64 `json:"soft"`
	Hard         uint64 `json:"hard"`
	Errors       uint64 `json:"errors"`
	OutputTokens uint64 `json:"output_tokens"`
}

type qualityGuardActionStats struct {
	Quarantined uint64 `json:"quarantined"`
	Restored    uint64 `json:"restored"`
	Suppressed  uint64 `json:"suppressed"`
}

type qualityGuardConfig struct {
	Mode                  string   `json:"mode"`
	Model                 string   `json:"model"`
	NodeIDs               []string `json:"node_ids"`
	ActiveIntervalSeconds int      `json:"active_interval_seconds"`
	PassivePollSeconds    int      `json:"passive_poll_seconds"`
	SoftTPS               float64  `json:"soft_tps"`
	HardTPS               float64  `json:"hard_tps"`
	ConsecutiveSoft       int      `json:"consecutive_soft"`
	ConsecutiveErrors     int      `json:"consecutive_errors"`
	QuarantineSeconds     int      `json:"quarantine_seconds"`
	MinHealthyNodes       int      `json:"min_healthy_nodes"`
	MaxOutputTokens       int      `json:"max_output_tokens"`
	FailClosed            bool     `json:"fail_closed"`
	MinGenerationMS       int      `json:"min_generation_ms"`
	Prompt                string   `json:"prompt"`
	Expected              string   `json:"expected"`
}

type qualityGuardNodeState struct {
	ObserveOnly        bool    `json:"observe_only"`
	ObserveOnlyReason  string  `json:"observe_only_reason"`
	QuarantinedLeases  int     `json:"quarantined_lease_count"`
	ActiveSoftStrikes  int     `json:"active_soft_strikes"`
	PassiveSoftStrikes int     `json:"passive_soft_strikes"`
	ErrorStrikes       int     `json:"error_strikes"`
	QuarantinedUntil   float64 `json:"quarantined_until"`
	DisabledByGuard    bool    `json:"disabled_by_guard"`
	LastReason         string  `json:"last_reason"`
	LastProbeAt        float64 `json:"last_probe_at"`
	LastObservedAt     float64 `json:"last_observed_at"`
	LastSource         string  `json:"last_source"`
	LastClassification string  `json:"last_classification"`
	LastOutputTPS      float64 `json:"last_output_tps"`
	LastOutputTokens   int     `json:"last_output_tokens"`
	LastFirstTokenMS   int     `json:"last_first_token_ms"`
	LastDurationMS     int     `json:"last_duration_ms"`
}

type qualityGuardEvent struct {
	TS             float64 `json:"ts"`
	Event          string  `json:"event"`
	NodeID         string  `json:"node_id"`
	NodeName       string  `json:"node_name"`
	AccountID      string  `json:"account_id,omitempty"`
	RequestID      string  `json:"request_id,omitempty"`
	Reason         string  `json:"reason"`
	Classification string  `json:"classification"`
	OutputTPS      float64 `json:"output_tps"`
	CooldownUntil  float64 `json:"cooldown_until,omitempty"`
}

func (h *Handler) qualityGuardStatus(c *gin.Context) {
	state, available, err := h.readQualityGuardState()
	if !available {
		response.Success(c, http.StatusOK, gin.H{"available": false})
		return
	}
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "质量守护状态暂不可用")
		return
	}
	nodes := state.Nodes
	if nodeIDs, filtered := c.GetQueryArray("nodeId"); filtered {
		nodes, err = selectedQualityGuardNodes(state.Nodes, nodeIDs)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalidNodeIds", err.Error())
			return
		}
	}
	payload := gin.H{
		"available":         true,
		"editable":          h.guardConfigPath != "",
		"startedAt":         state.StartedAt,
		"updatedAt":         state.UpdatedAt,
		"lastActiveCycleAt": state.LastActiveCycleAt,
		"lastPassivePollAt": state.LastPassivePollAt,
		"config": gin.H{
			"mode": state.Guard.Mode, "model": state.Guard.Model,
			"node_ids": state.Guard.NodeIDs, "active_interval_seconds": state.Guard.ActiveIntervalSeconds,
			"passive_poll_seconds": state.Guard.PassivePollSeconds, "soft_tps": state.Guard.SoftTPS,
			"hard_tps": state.Guard.HardTPS, "consecutive_soft": state.Guard.ConsecutiveSoft,
			"consecutive_errors": state.Guard.ConsecutiveErrors, "quarantine_seconds": state.Guard.QuarantineSeconds,
			"min_healthy_nodes": state.Guard.MinHealthyNodes, "max_output_tokens": state.Guard.MaxOutputTokens,
			"fail_closed": state.Guard.FailClosed, "min_generation_ms": state.Guard.MinGenerationMS,
		},
		"nodes": nodes, "protectedNodeIds": state.ProtectedNodeIDs, "recentEvents": state.RecentEvents,
		"nodeSummary": gin.H{
			"total": state.NodeSummary.Total, "quarantined": state.NodeSummary.Quarantined, "quarantinedLeases": state.NodeSummary.QuarantinedLeases,
		},
	}
	if state.Statistics.StartedAt > 0 {
		payload["statistics"] = state.Statistics
	}
	if profiles, err := loadProbeProfileFile(h.profilesPath()); err == nil {
		payload["activeProfileId"] = profiles.ActiveProfileID
		payload["profiles"] = profiles.summaries()
	}
	response.Success(c, http.StatusOK, payload)
}

func selectedQualityGuardNodes(values map[string]qualityGuardNodeState, ids []string) (map[string]qualityGuardNodeState, error) {
	if len(ids) > 200 {
		return nil, errors.New("质量守护节点筛选不能超过 200 个")
	}
	result := make(map[string]qualityGuardNodeState, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, err := strconv.ParseUint(id, 10, 64); err != nil {
			return nil, errors.New("质量守护节点 ID 无效")
		}
		if value, ok := values[id]; ok {
			result[id] = value
		}
	}
	return result, nil
}

type qualityGuardConfigRequest struct {
	Mode                  string  `json:"mode"`
	ActiveIntervalSeconds int     `json:"activeIntervalSeconds"`
	PassivePollSeconds    int     `json:"passivePollSeconds"`
	SoftTPS               float64 `json:"softTPS"`
	HardTPS               float64 `json:"hardTPS"`
	ConsecutiveSoft       int     `json:"consecutiveSoft"`
	ConsecutiveErrors     int     `json:"consecutiveErrors"`
	QuarantineSeconds     int     `json:"quarantineSeconds"`
	MinHealthyNodes       int     `json:"minHealthyNodes"`
}

type qualityGuardRuntimeConfigFile struct {
	Version  int                               `json:"version"`
	Settings qualityGuardRuntimeConfigSettings `json:"settings"`
}

type qualityGuardRuntimeConfigSettings struct {
	Mode                  string  `json:"mode"`
	ActiveIntervalSeconds int     `json:"active_interval_seconds"`
	PassivePollSeconds    int     `json:"passive_poll_seconds"`
	SoftTPS               float64 `json:"soft_tps"`
	HardTPS               float64 `json:"hard_tps"`
	ConsecutiveSoft       int     `json:"consecutive_soft"`
	ConsecutiveErrors     int     `json:"consecutive_errors"`
	QuarantineSeconds     int     `json:"quarantine_seconds"`
	MinHealthyNodes       int     `json:"min_healthy_nodes"`
}

func (h *Handler) updateQualityGuardConfig(c *gin.Context) {
	if h.guardConfigPath == "" {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardReadOnly", "质量守护策略当前只读")
		return
	}
	state, available, err := h.readQualityGuardState()
	if err != nil || !available {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "质量守护状态暂不可用")
		return
	}
	var request qualityGuardConfigRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	if err := request.validate(len(state.Guard.NodeIDs)); err != nil {
		response.Error(c, http.StatusBadRequest, "invalidQualityGuardConfig", err.Error())
		return
	}
	value := qualityGuardRuntimeConfigFile{Version: 1, Settings: qualityGuardRuntimeConfigSettings{
		Mode: request.Mode, ActiveIntervalSeconds: request.ActiveIntervalSeconds,
		PassivePollSeconds: request.PassivePollSeconds, SoftTPS: request.SoftTPS, HardTPS: request.HardTPS,
		ConsecutiveSoft: request.ConsecutiveSoft, ConsecutiveErrors: request.ConsecutiveErrors,
		QuarantineSeconds: request.QuarantineSeconds, MinHealthyNodes: request.MinHealthyNodes,
	}}
	if err := saveQualityGuardRuntimeConfig(h.guardConfigPath, value); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardConfigWriteFailed", "质量守护策略保存失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"saved": true})
}

func (r qualityGuardConfigRequest) validate(nodeCount int) error {
	if r.Mode != "active" && r.Mode != "passive" && r.Mode != "hybrid" {
		return errors.New("检测模式无效")
	}
	if r.ActiveIntervalSeconds < 60 || r.ActiveIntervalSeconds > 86400 {
		return errors.New("主动检测间隔必须在 60 到 86400 秒之间")
	}
	if r.PassivePollSeconds < 1 || r.PassivePollSeconds > 300 {
		return errors.New("被动审计间隔必须在 1 到 300 秒之间")
	}
	if math.IsNaN(r.SoftTPS) || math.IsInf(r.SoftTPS, 0) || math.IsNaN(r.HardTPS) || math.IsInf(r.HardTPS, 0) || r.SoftTPS < 1 || r.HardTPS > 10000 || r.SoftTPS >= r.HardTPS {
		return errors.New("Token/s 阈值无效，软阈值必须低于硬阈值")
	}
	if r.ConsecutiveSoft < 1 || r.ConsecutiveSoft > 20 || r.ConsecutiveErrors < 1 || r.ConsecutiveErrors > 20 {
		return errors.New("连续命中次数必须在 1 到 20 之间")
	}
	if r.QuarantineSeconds < 30 || r.QuarantineSeconds > 86400 {
		return errors.New("隔离时长必须在 30 到 86400 秒之间")
	}
	if r.MinHealthyNodes < 1 || r.MinHealthyNodes > nodeCount {
		return errors.New("最少保留节点必须在受管节点数量范围内")
	}
	return nil
}

func saveQualityGuardRuntimeConfig(path string, value qualityGuardRuntimeConfigFile) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".runtime-config-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func (h *Handler) readQualityGuardState() (qualityGuardState, bool, error) {
	if h.guardStatePath == "" {
		return qualityGuardState{}, false, nil
	}
	file, err := os.Open(h.guardStatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			h.guardStateMu.Lock()
			h.guardStateCache = qualityGuardStateCache{}
			h.guardStateMu.Unlock()
			return qualityGuardState{}, false, nil
		}
		return qualityGuardState{}, true, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return qualityGuardState{}, true, errors.New("质量守护状态不可读")
	}
	h.guardStateMu.Lock()
	defer h.guardStateMu.Unlock()
	if h.guardStateCache.valid && h.guardStateCache.path == h.guardStatePath && os.SameFile(h.guardStateCache.fileInfo, info) && h.guardStateCache.fileInfo.Size() == info.Size() && h.guardStateCache.fileInfo.ModTime().Equal(info.ModTime()) {
		return h.guardStateCache.state, true, nil
	}
	data, err := io.ReadAll(io.LimitReader(file, maxQualityGuardStateBytes+1))
	if err != nil || len(data) > maxQualityGuardStateBytes {
		return qualityGuardState{}, true, errors.New("质量守护状态不可读")
	}
	var state qualityGuardState
	if json.Unmarshal(data, &state) != nil || state.Version != 1 || state.Guard.Mode == "" || state.Nodes == nil {
		return qualityGuardState{}, true, errors.New("质量守护状态格式无效")
	}
	if state.RecentEvents == nil {
		state.RecentEvents = []qualityGuardEvent{}
	}
	if state.ProtectedNodeIDs == nil {
		state.ProtectedNodeIDs = []string{}
	}
	state.NodeSummary.Total = len(state.Nodes)
	for _, node := range state.Nodes {
		if node.DisabledByGuard {
			state.NodeSummary.Quarantined++
		}
		state.NodeSummary.QuarantinedLeases += node.QuarantinedLeases
	}
	h.guardStateCache = qualityGuardStateCache{
		path: h.guardStatePath, fileInfo: info, state: state, valid: true,
	}
	return state, true, nil
}

func (h *Handler) testQualityGuardNode(c *gin.Context) {
	nodeID, ok := pathID(c)
	if !ok {
		return
	}
	if h.guardProbe.ClientKeyID == 0 || strings.TrimSpace(h.guardProbe.Model) == "" {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "质量守护配置暂不可用")
		return
	}
	var request struct {
		ProfileID string `json:"profileId"`
		AccountID string `json:"accountId"`
	}
	_ = c.ShouldBindJSON(&request)
	input, err := h.resolveProbeInput(strings.TrimSpace(request.ProfileID))
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", err.Error())
		return
	}
	if strings.TrimSpace(input.Prompt) == "" {
		response.Error(c, http.StatusServiceUnavailable, "qualityGuardUnavailable", "质量守护配置暂不可用")
		return
	}
	if strings.TrimSpace(request.AccountID) != "" {
		accountID, parseErr := strconv.ParseUint(request.AccountID, 10, 64)
		if parseErr != nil || accountID == 0 {
			response.Error(c, http.StatusBadRequest, "invalidAccountId", "账号 ID 无效")
			return
		}
		input.AccountID = accountID
	}
	value, err := h.service.ProbeQuality(c.Request.Context(), nodeID, input)
	if err != nil {
		h.writeQualityProbeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"requestId": value.RequestID, "nodeId": strconv.FormatUint(value.NodeID, 10), "model": value.Model,
		"statusCode": value.StatusCode, "firstTokenMs": value.FirstTokenMS, "durationMs": value.DurationMS,
		"generationMs": value.GenerationMS, "chunkCount": value.ChunkCount,
		"outputTokens": value.OutputTokens, "reasoningTokens": value.ReasoningTokens,
		"visibleTokens": value.VisibleTokens, "visibleCharacters": value.VisibleCharacters,
		"outputTokensPerSecond":  value.OutputTokensPerSecond,
		"visibleTokensPerSecond": value.OutputTokensPerSecond, "expectedMatched": value.ExpectedMatched,
		"thinkingRequired": value.ThinkingRequired,
		"responseSha256":   value.ResponseSHA256,
	})
}
