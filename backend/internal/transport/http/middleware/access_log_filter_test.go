package middleware

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAccessLogOmitsRoutineSelfTrafficButKeepsActionsAndFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		method, path    string
		status          int
		matched, logged bool
	}{
		{"GET", "/api/admin/v1/system/logs", 200, true, false}, {"GET", "/api/admin/v1/system/logs", 500, true, false},
		{"GET", "/api/admin/v1/accounts", 200, true, false}, {"GET", "/api/admin/v1/media/jobs", 200, true, false},
		{"POST", "/api/admin/v1/auth/refresh", 200, true, false}, {"GET", "/healthz", 200, true, false},
		{"GET", "/readyz", 503, true, false},
		{"GET", "/assets/index.js", 200, false, false}, {"GET", "/system-logs", 200, false, false},
		{"GET", "/assets/missing.js", 404, false, true}, {"GET", "/api/admin/v1/accounts", 503, true, true},
		{"POST", "/api/admin/v1/accounts/42/refresh-quota", 503, true, true}, {"POST", "/api/admin/v1/accounts/42/refresh-quota", 200, true, true},
		{"GET", "/api/admin/v1/accounts/export", 200, true, true}, {"GET", "/v1/models", 200, true, true},
		{"GET", "/api/admin/v1/request-audits", 401, true, false}, {"GET", "/api/admin/v1/models", 401, true, false},
		{"GET", "/api/admin/v1/providers", 401, true, false}, {"HEAD", "/api/admin/v1/accounts", 401, true, false},
		{"POST", "/api/admin/v1/auth/refresh", 401, true, false}, {"POST", "/api/admin/v1/auth/refresh", 503, true, true},
		{"POST", "/api/admin/v1/auth/login", 401, true, true}, {"POST", "/api/admin/v1/accounts/42/refresh-quota", 401, true, true},
		{"GET", "/api/admin/v1/accounts/export", 401, true, true}, {"GET", "/v1/models", 401, true, true},
		{"GET", "/api/admin/v1/accounts", 400, true, true}, {"GET", "/api/admin/v1/accounts", 403, true, true},
		{"GET", "/api/admin/v1/unknown", 401, false, true},
	} {
		t.Run(fmt.Sprintf("%s %s %d", test.method, test.path, test.status), func(t *testing.T) {
			var logs bytes.Buffer
			router := gin.New()
			router.Use(AccessLog(slog.New(slog.NewJSONHandler(&logs, nil))))
			handler := func(c *gin.Context) { c.Status(test.status) }
			if test.matched {
				router.Handle(test.method, test.path, handler)
			} else {
				router.NoRoute(handler)
			}
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(test.method, test.path, nil))
			if (logs.Len() > 0) != test.logged {
				t.Fatalf("logged=%t wanted=%t: %s", logs.Len() > 0, test.logged, logs.String())
			}
		})
	}
}

func TestAccessLogOmitsRoutineReadsAbortedByAdminAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	router := gin.New()
	router.Use(AccessLog(slog.New(slog.NewJSONHandler(&logs, nil))))
	admin := router.Group("/api/admin/v1", AdminAuth(nil))
	admin.GET("/accounts/:id", func(*gin.Context) { t.Fatal("unauthenticated handler ran") })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/admin/v1/accounts/42", nil))
	if recorder.Code != 401 || logs.Len() != 0 {
		t.Fatalf("status=%d logs=%s", recorder.Code, logs.String())
	}
}
