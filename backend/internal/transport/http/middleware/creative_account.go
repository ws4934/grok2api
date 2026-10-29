package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/gin-gonic/gin"
)

// CreativeAccount runs after AdminAuth and before ClientAuth. The admin JWT
// authenticates this route; the independent client key keeps normal API limits.
func CreativeAccount() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, authenticated := c.Get(AdminKey); !authenticated {
			WriteAPIError(c, http.StatusUnauthorized, "adminUnauthorized", "管理员登录已失效")
			return
		}
		id, err := strconv.ParseUint(strings.TrimSpace(c.GetHeader("X-Creative-Account-ID")), 10, 64)
		if err != nil || id == 0 {
			WriteAPIError(c, http.StatusBadRequest, "invalid_account_id", "请选择有效的指定账号")
			return
		}
		c.Request = c.Request.WithContext(gateway.WithCreativeAccount(c.Request.Context(), id))
		c.Request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.GetHeader("X-Client-Key")))
		c.Request.Header.Del("X-Client-Key")
		c.Next()
	}
}
