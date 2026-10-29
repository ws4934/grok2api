package middleware

import (
	"bytes"
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
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
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
