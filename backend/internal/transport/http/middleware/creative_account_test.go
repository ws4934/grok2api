package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCreativeAccountRequiresAdministratorAndValidAccount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		admin  bool
		id     string
		status int
	}{
		{"no administrator", false, "42", 401},
		{"missing account", true, "", 400},
		{"zero account", true, "0", 400},
		{"negative account", true, "-1", 400},
		{"overflow", true, "18446744073709551616", 400},
		{"valid account", true, "42", 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			if tc.admin {
				router.Use(func(c *gin.Context) { c.Set(AdminKey, true); c.Next() })
			}
			router.Use(CreativeAccount())
			router.POST("/api/admin/v1/creative/responses", func(c *gin.Context) {
				if c.GetHeader("Authorization") != "Bearer client-test" || c.GetHeader("X-Client-Key") != "" {
					t.Fatal("administrator JWT or client key header leaked past the auth bridge")
				}
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/creative/responses", strings.NewReader(`{"model":"test"}`))
			req.Header.Set("Authorization", "Bearer admin-test")
			req.Header.Set("X-Client-Key", "client-test")
			req.Header.Set("X-Creative-Account-ID", tc.id)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
