package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/gin-gonic/gin"
)

const RequestIDKey = "requestId"
const maxRequestIDLength = 64

// RequestID 为每个请求生成稳定关联 ID，并写入响应头。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if !validRequestID(requestID) {
			requestID, _ = security.NewOpaqueToken(12)
			if requestID == "" {
				requestID = "req-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			}
		}
		c.Set(RequestIDKey, requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}

// ClientIP captures Gin's trusted-proxy-aware caller address once at ingress.
// The server config controls which reverse proxies may supply forwarding headers.
func ClientIP() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(requestmeta.WithClientIP(c.Request.Context(), c.ClientIP()))
		c.Next()
	}
}

// validRequestID 只接受适合写入日志和审计索引的短 ASCII 标识。
func validRequestID(value string) bool {
	if value == "" || len(value) > maxRequestIDLength {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') {
			continue
		}
		switch character {
		case '-', '_', '.', ':':
		default:
			return false
		}
	}
	return true
}

// Timeout 为 HTTP 请求设置统一生命周期上限。
func Timeout(duration time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), duration)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// MaxBodyBytes 对所有请求体应用统一硬上限，避免管理端绑定无界读取。
// XAI 视频 PUT（/v1/media/uploads/）使用更高的视频安全上限，由票据再次限长。
func MaxBodyBytes(limit int64) gin.HandlerFunc {
	const mediaUploadPathPrefix = "/v1/media/uploads/"
	const mediaUploadMaxBytes = 256 << 20
	return func(c *gin.Context) {
		if c.Request.Body != nil && limit > 0 {
			effective := limit
			if strings.HasPrefix(c.Request.URL.Path, mediaUploadPathPrefix) && c.Request.Method == http.MethodPut {
				if mediaUploadMaxBytes > effective {
					effective = mediaUploadMaxBytes
				}
			}
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, effective)
		}
		c.Next()
	}
}

// SecurityHeaders 为 API 和媒体响应添加通用浏览器安全边界。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		c.Next()
	}
}

// errorNoteKey holds the error a handler answered the request with.
const errorNoteKey = "access_log_error"

type errorNote struct{ code, message string }

// NoteError records the code and message a request is answered with, so the
// access log shows why it failed.
func NoteError(c *gin.Context, code, message string) {
	c.Set(errorNoteKey, errorNote{code: code, message: message})
}

// AccessLog 记录路径、状态、耗时，以及处理器用 NoteError 记下的错误码和错误信息；
// 不读取请求或响应正文。
func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		startedAt := time.Now()
		c.Next()
		requestID, _ := c.Get(RequestIDKey)
		// Matched routes log their template so IDs and tokens stay out of the log;
		// unmatched requests (static files, SPA pages) log the bounded raw path.
		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
			if len(path) > 200 {
				path = path[:200]
			}
		}
		attrs := []any{"request_id", requestID, "method", c.Request.Method, "path", path, "status", c.Writer.Status(), "duration_ms", time.Since(startedAt).Milliseconds()}
		if value, ok := c.Get(errorNoteKey); ok {
			note, _ := value.(errorNote)
			message := []rune(note.message)
			if len(message) > 300 {
				message = append(message[:299], '…')
			}
			attrs = append(attrs, "error_code", note.code, "error", string(message))
		}
		logger.Info("http_request", attrs...)
	}
}
