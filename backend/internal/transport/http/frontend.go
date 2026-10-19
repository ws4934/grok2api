package httpserver

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// registerFrontend 在构建产物存在时托管静态文件，并为前端路由提供 SPA 回退。
func registerFrontend(router *gin.Engine, staticPath string) {
	root, indexPath, ok := frontendRoot(staticPath)
	if !ok {
		return
	}
	files := http.FileServer(http.Dir(root))
	var gzipped sync.Map // relative path → gzip bytes
	router.NoRoute(func(c *gin.Context) {
		requestPath := c.Request.URL.Path
		if (c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) || isBackendPath(requestPath) {
			c.Status(http.StatusNotFound)
			return
		}
		if filePath, exists := frontendFile(root, requestPath); exists {
			if strings.HasPrefix(path.Clean(requestPath), "/assets/") {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
				if serveGzipAsset(c, &gzipped, root, filePath) {
					return
				}
			} else {
				c.Header("Cache-Control", "no-cache")
			}
			c.Request.URL.Path = "/" + filepath.ToSlash(filePath)
			files.ServeHTTP(c.Writer, c.Request)
			return
		}
		if path.Ext(path.Clean(requestPath)) != "" {
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "no-cache")
		http.ServeFile(c.Writer, c.Request, indexPath)
	})
}

// serveGzipAsset answers a gzip-capable client with a compressed script or
// stylesheet. Asset names carry a content hash, so the copy cached on first
// request never goes stale.
func serveGzipAsset(c *gin.Context, cache *sync.Map, root, relative string) bool {
	if ext := path.Ext(relative); ext != ".js" && ext != ".css" {
		return false
	}
	c.Header("Vary", "Accept-Encoding")
	if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
		return false
	}
	compressed, ok := cache.Load(relative)
	if !ok {
		raw, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			return false
		}
		var buffer bytes.Buffer
		writer, _ := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
		_, _ = writer.Write(raw)
		_ = writer.Close()
		compressed, _ = cache.LoadOrStore(relative, buffer.Bytes())
	}
	c.Header("Content-Encoding", "gzip")
	http.ServeContent(c.Writer, c.Request, relative, time.Time{}, bytes.NewReader(compressed.([]byte)))
	return true
}

func frontendRoot(staticPath string) (string, string, bool) {
	staticPath = strings.TrimSpace(staticPath)
	if staticPath == "" {
		return "", "", false
	}
	root, err := filepath.Abs(staticPath)
	if err != nil {
		return "", "", false
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", "", false
	}
	indexPath := filepath.Join(root, "index.html")
	indexInfo, err := os.Stat(indexPath)
	if err != nil || !indexInfo.Mode().IsRegular() {
		return "", "", false
	}
	return filepath.Clean(root), indexPath, true
}

func frontendFile(root, requestPath string) (string, bool) {
	cleanPath := strings.TrimPrefix(path.Clean("/"+requestPath), "/")
	if cleanPath == "" || cleanPath == "." {
		return "", false
	}
	fullPath := filepath.Join(root, filepath.FromSlash(cleanPath))
	relative, err := filepath.Rel(root, fullPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Stat(fullPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return relative, true
}

func isBackendPath(value string) bool {
	cleanPath := path.Clean("/" + value)
	for _, prefix := range []string{"/api", "/v1", "/swagger"} {
		if cleanPath == prefix || strings.HasPrefix(cleanPath, prefix+"/") {
			return true
		}
	}
	return cleanPath == "/healthz" || cleanPath == "/readyz"
}
