package params

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// IDs above the signed 64-bit range would fail in the database with a 500.
func TestIDsRejectTopBitAndZero(t *testing.T) {
	const topBit = "9223372036854775808"
	for _, value := range []string{topBit, "0", "-1", "x"} {
		if _, err := IDs([]string{"1", value}); err == nil {
			t.Fatalf("IDs accepted %q", value)
		}
	}
	if ids, err := IDs([]string{"1", "9223372036854775807"}); err != nil || len(ids) != 2 {
		t.Fatalf("IDs = %v, %v", ids, err)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: topBit}}
	if _, ok := PathID(c); ok || recorder.Code != http.StatusBadRequest {
		t.Fatalf("PathID accepted a top-bit ID: status %d", recorder.Code)
	}
}
