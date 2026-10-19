package egress

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type qualityLeaseRequest struct {
	AccountID         string `json:"accountId" binding:"required"`
	NodeID            string `json:"nodeId" binding:"required"`
	Reason            string `json:"reason"`
	Version           string `json:"version"`
	QuarantineSeconds int    `json:"quarantineSeconds"`
}

type qualityLeaseCursor struct {
	CooldownUntil int64  `json:"t"`
	AccountID     uint64 `json:"a"`
	NodeID        uint64 `json:"n"`
}

func qualityLeaseResponse(value accountdomain.EgressLeaseBlock) gin.H {
	return gin.H{
		"accountId": strconv.FormatUint(value.AccountID, 10), "nodeId": strconv.FormatUint(value.NodeID, 10),
		"reason": value.Reason, "version": value.Version, "cooldownUntil": float64(value.CooldownUntil.UnixMilli()) / 1000,
		"updatedAt": value.UpdatedAt.UTC(),
	}
}

func (h *Handler) listQualityGuardLeases(c *gin.Context) {
	limit, parseErr := strconv.Atoi(c.DefaultQuery("limit", "500"))
	if parseErr != nil || limit < 1 || limit > 1000 {
		response.Error(c, http.StatusBadRequest, "invalidPageSize", "分页大小无效")
		return
	}
	cursor, cursorErr := decodeQualityLeaseCursor(c.Query("cursor"))
	if cursorErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidCursor", "分页游标无效")
		return
	}
	values, err := h.service.ListQualityLeases(c.Request.Context(), limit+1, cursor)
	if err != nil {
		h.writeQualityLeaseError(c, err)
		return
	}
	hasMore := len(values) > limit
	if hasMore {
		values = values[:limit]
	}
	items := make([]gin.H, 0, len(values))
	for _, value := range values {
		items = append(items, qualityLeaseResponse(value))
	}
	nextCursor := ""
	if hasMore && len(values) > 0 {
		nextCursor = encodeQualityLeaseCursor(values[len(values)-1])
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "hasMore": hasMore, "nextCursor": nextCursor})
}

func decodeQualityLeaseCursor(raw string) (*accountdomain.EgressLeaseBlockCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 256 {
		return nil, errors.New("cursor too long")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var value qualityLeaseCursor
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || value.CooldownUntil <= 0 || value.AccountID == 0 || value.NodeID == 0 {
		return nil, errors.New("invalid cursor")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid cursor")
	}
	return &accountdomain.EgressLeaseBlockCursor{
		CooldownUntil: time.Unix(0, value.CooldownUntil).UTC(), AccountID: value.AccountID, NodeID: value.NodeID,
	}, nil
}

func encodeQualityLeaseCursor(value accountdomain.EgressLeaseBlock) string {
	payload, _ := json.Marshal(qualityLeaseCursor{
		CooldownUntil: value.CooldownUntil.UTC().UnixNano(), AccountID: value.AccountID, NodeID: value.NodeID,
	})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func (h *Handler) quarantineQualityGuardLease(c *gin.Context) {
	var request qualityLeaseRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	accountID, accountErr := strconv.ParseUint(request.AccountID, 10, 64)
	nodeID, nodeErr := strconv.ParseUint(request.NodeID, 10, 64)
	if accountErr != nil || nodeErr != nil || accountID == 0 || nodeID == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号或节点 ID 无效")
		return
	}
	value, err := h.service.QuarantineQualityLease(c.Request.Context(), egressapp.QualityLeaseInput{
		AccountID: accountID, NodeID: nodeID, Reason: request.Reason, QuarantineSeconds: request.QuarantineSeconds,
	})
	if err != nil {
		h.writeQualityLeaseError(c, err)
		return
	}
	response.Success(c, http.StatusOK, qualityLeaseResponse(value))
}

func (h *Handler) restoreQualityGuardLease(c *gin.Context) {
	var request qualityLeaseRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	accountID, accountErr := strconv.ParseUint(request.AccountID, 10, 64)
	nodeID, nodeErr := strconv.ParseUint(request.NodeID, 10, 64)
	if accountErr != nil || nodeErr != nil || accountID == 0 || nodeID == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号或节点 ID 无效")
		return
	}
	restored, err := h.service.RestoreQualityLease(c.Request.Context(), accountID, nodeID, request.Version)
	if err != nil {
		h.writeQualityLeaseError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"restored": restored})
}

func (h *Handler) writeQualityLeaseError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, egressapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, "invalidQualityLease", "租约隔离参数无效")
	case errors.Is(err, egressapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "egressNodeNotFound", "代理节点不存在")
	case errors.Is(err, egressapp.ErrQualityLeaseConflict):
		response.Error(c, http.StatusConflict, "qualityLeaseConflict", "租约绑定或隔离版本已变化")
	case errors.Is(err, egressapp.ErrQualityLeaseUnavailable):
		response.Error(c, http.StatusServiceUnavailable, "qualityLeaseUnavailable", "租约级质量隔离暂不可用")
	default:
		response.Error(c, http.StatusInternalServerError, "qualityLeaseOperationFailed", "租约级质量隔离操作失败")
	}
}
