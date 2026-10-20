package egress

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type operationsConfigRequest struct {
	ProbeProvider             string                               `json:"probeProvider"`
	ProbeIntervalSeconds      int                                  `json:"probeIntervalSeconds"`
	AutoAssignEnabled         bool                                 `json:"autoAssignEnabled"`
	AutoBalanceEnabled        bool                                 `json:"autoBalanceEnabled"`
	AssignmentIntervalSeconds int                                  `json:"assignmentIntervalSeconds"`
	Fallbacks                 map[string]operationsFallbackRequest `json:"fallbacks"`
}

type operationsFallbackRequest struct {
	Mode   string `json:"mode"`
	NodeID string `json:"nodeId"`
}

type operationsConfigResponse struct {
	ProbeProvider             string                                `json:"probeProvider"`
	ProbeIntervalSeconds      int                                   `json:"probeIntervalSeconds"`
	AutoAssignEnabled         bool                                  `json:"autoAssignEnabled"`
	AutoBalanceEnabled        bool                                  `json:"autoBalanceEnabled"`
	AssignmentIntervalSeconds int                                   `json:"assignmentIntervalSeconds"`
	Fallbacks                 map[string]operationsFallbackResponse `json:"fallbacks"`
	UpdatedAt                 time.Time                             `json:"updatedAt"`
}

type operationsFallbackResponse struct {
	Mode   string `json:"mode"`
	NodeID string `json:"nodeId,omitempty"`
}

func (value operationsConfigRequest) input() (egressapp.OperationsConfigInput, error) {
	result := egressapp.OperationsConfigInput{
		ProbeProvider: egressdomain.ProbeProvider(strings.TrimSpace(value.ProbeProvider)), ProbeIntervalSeconds: value.ProbeIntervalSeconds, AutoAssignEnabled: value.AutoAssignEnabled,
		AutoBalanceEnabled: value.AutoBalanceEnabled, AssignmentIntervalSeconds: value.AssignmentIntervalSeconds,
	}
	if value.Fallbacks == nil {
		return result, nil
	}
	result.Fallbacks = make(map[egressdomain.Scope]egressapp.FallbackConfigInput, len(value.Fallbacks))
	for rawScope, fallback := range value.Fallbacks {
		nodeID := uint64(0)
		if strings.TrimSpace(fallback.NodeID) != "" {
			parsed, err := strconv.ParseUint(fallback.NodeID, 10, 64)
			if err != nil || parsed == 0 {
				return egressapp.OperationsConfigInput{}, fmt.Errorf("%w: 固定回退节点 ID 无效", egressapp.ErrInvalidInput)
			}
			nodeID = parsed
		}
		result.Fallbacks[egressdomain.Scope(rawScope)] = egressapp.FallbackConfigInput{
			Mode: egressdomain.FallbackMode(strings.TrimSpace(fallback.Mode)), NodeID: nodeID,
		}
	}
	return result, nil
}

func newOperationsConfigResponse(value egressdomain.OperationsConfig) operationsConfigResponse {
	fallbacks := make(map[string]operationsFallbackResponse, 5)
	for _, scope := range egressdomain.AllScopes() {
		fallback := value.FallbackFor(scope)
		item := operationsFallbackResponse{Mode: string(fallback.Mode)}
		if fallback.NodeID != 0 {
			item.NodeID = strconv.FormatUint(fallback.NodeID, 10)
		}
		fallbacks[string(scope)] = item
	}
	return operationsConfigResponse{
		ProbeProvider: string(value.ProbeProvider.Normalized()), ProbeIntervalSeconds: value.ProbeIntervalSeconds, AutoAssignEnabled: value.AutoAssignEnabled,
		AutoBalanceEnabled: value.AutoBalanceEnabled, AssignmentIntervalSeconds: value.AssignmentIntervalSeconds,
		Fallbacks: fallbacks, UpdatedAt: value.UpdatedAt,
	}
}

func (h *Handler) operationsConfig(c *gin.Context) {
	value, err := h.service.OperationsConfig(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newOperationsConfigResponse(value))
}

func (h *Handler) updateOperationsConfig(c *gin.Context) {
	var request operationsConfigRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	input, err := request.input()
	if err != nil {
		h.writeError(c, err)
		return
	}
	value, err := h.service.UpdateOperationsConfig(c.Request.Context(), input)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newOperationsConfigResponse(value))
}

func (h *Handler) rebalance(c *gin.Context) {
	config, err := h.service.OperationsConfig(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	value, err := h.service.RebalanceAccounts(c.Request.Context(), true, true, time.Duration(config.ProbeIntervalSeconds)*time.Second)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"assigned": value.Assigned, "rebalanced": value.Rebalanced, "unplaced": value.Unplaced})
}
