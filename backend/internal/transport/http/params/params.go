// Package params parses the IDs and paging of admin API requests.
package params

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

// ID parses a database ID: positive and within the signed 64-bit columns.
func ID(value string) (uint64, error) {
	id, err := strconv.ParseUint(value, 10, 63)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("无效 ID: %s", value)
	}
	return id, nil
}

// IDs parses a list of database IDs.
func IDs(values []string) ([]uint64, error) {
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		id, err := ID(value)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// PathID reads the :id route parameter; an invalid ID is answered with 400.
func PathID(c *gin.Context) (uint64, bool) {
	id, err := ID(c.Param("id"))
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return 0, false
	}
	return id, true
}

// Page reads the normalized page and pageSize query parameters.
func Page(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	return repository.NormalizePage(page, size, repository.DefaultPageSize)
}
