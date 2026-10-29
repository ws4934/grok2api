// Package accountnames resolves display-only account metadata for admin views.
package accountnames

import (
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
	"net/http"
)

func Load(c *gin.Context, reader repository.AccountNameRepository, ids []uint64) (map[uint64]string, bool) {
	if reader == nil {
		return nil, true
	}
	names, err := reader.DisplayNames(c.Request.Context(), ids)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "accountNamesFailed", "读取账号显示信息失败")
		return nil, false
	}
	return names, true
}
func Name(names map[uint64]string, id uint64, snapshot string) string {
	if name := names[id]; name != "" {
		return name
	}
	return snapshot
}
