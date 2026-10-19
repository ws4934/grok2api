package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func (a *Adapter) uploadFileV2Direct(ctx context.Context, cfg Config, lease *egress.Lease, token string, file provider.ImageInput, referer, fileSource, stage string) (uploadedFile, error) {
	body, contentType, err := buildDirectFileUploadBody(file, fileSource)
	if err != nil {
		return uploadedFile{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, cfg.BaseURL+"/http/upload-file-v2/direct", bytes.NewReader(body))
	if err != nil {
		return uploadedFile{}, err
	}
	request.Header = buildHeaders(token, lease, contentType)
	request.Header.Del("x-xai-request-id")
	applyAppHeaders(request.Header, cfg.BaseURL, referer)
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		return uploadedFile{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
		if readErr != nil {
			return uploadedFile{}, fmt.Errorf("读取 V2 上传文件错误响应: %w", readErr)
		}
		truncated := len(responseBody) > webMediaDiagnosticBodyLimit
		if truncated {
			responseBody = responseBody[:webMediaDiagnosticBodyLimit]
		}
		upstreamErr := newWebMediaUpstreamError(response.StatusCode, responseBody, truncated)
		if isClearanceRefreshableMediaError(upstreamErr) {
			lease.InvalidateClearance()
		}
		a.logWebMediaUpstreamRejection(stage, response, upstreamErr)
		return uploadedFile{}, upstreamErr
	}
	uploaded, err := decodeDirectFileUploadResponse(io.LimitReader(response.Body, directFileUploadResponseLimit))
	if err != nil {
		return uploadedFile{}, err
	}
	return uploaded, nil
}

func buildDirectFileUploadBody(file provider.ImageInput, fileSource string) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, browserMultipartFilename(file.Filename)))
	header.Set("Content-Type", file.MIMEType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(file.Data); err != nil {
		return nil, "", err
	}
	if fileSource != "" {
		if err := writer.WriteField("file_source", fileSource); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func browserMultipartFilename(value string) string {
	value = strings.Map(func(character rune) rune {
		switch {
		case character == '\r' || character == '\n':
			return -1
		case character < 0x20 || character == 0x7f:
			return '_'
		default:
			return character
		}
	}, value)
	if strings.TrimSpace(value) == "" {
		value = "upload.bin"
	}
	return strings.NewReplacer("\\", "\\\\", `"`, `\"`).Replace(value)
}

func decodeDirectFileUploadResponse(source io.Reader) (uploadedFile, error) {
	var value struct {
		UploadID      string          `json:"uploadId"`
		TerminalError json.RawMessage `json:"terminalError"`
		FileMetadata  struct {
			ID      string `json:"fileMetadataId"`
			FileID  string `json:"fileId"`
			FileURI string `json:"fileUri"`
		} `json:"fileMetadata"`
	}
	if err := json.NewDecoder(source).Decode(&value); err != nil {
		return uploadedFile{}, fmt.Errorf("V2 上传文件响应无效: %w", err)
	}
	if directFileUploadTerminalError(value.TerminalError) {
		return uploadedFile{}, errors.New("V2 上传文件被上游拒绝")
	}
	metadataID := strings.TrimSpace(value.FileMetadata.ID)
	fileID := metadataID
	if fileID == "" {
		fileID = strings.TrimSpace(value.FileMetadata.FileID)
	}
	if fileID == "" {
		// Some successful uploads complete asynchronously and only expose the
		// upload task ID. Gateway accepts it as the file reference; prefer the
		// browser's fileMetadataId whenever it is already available.
		fileID = strings.TrimSpace(value.UploadID)
	}
	fileURI := ""
	if value.FileMetadata.FileURI != "" {
		fileURI = absoluteAssetURL(value.FileMetadata.FileURI)
	}
	if fileID == "" && fileURI == "" {
		return uploadedFile{}, fmt.Errorf("V2 上传文件成功但上游未返回完整文件标识")
	}
	return uploadedFile{ID: fileID, MetadataID: metadataID, URI: fileURI}, nil
}

func directFileUploadTerminalError(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("false")) || bytes.Equal(trimmed, []byte("0")) {
		return false
	}
	var value any
	if json.Unmarshal(trimmed, &value) != nil {
		return true
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) != 0
	case []any:
		return len(typed) != 0
	case bool:
		return typed
	case float64:
		return typed != 0
	default:
		return value != nil
	}
}

func (a *Adapter) postJSON(ctx context.Context, cfg Config, lease *egress.Lease, token, endpoint string, payload any, timeout time.Duration) (*http.Response, error) {
	return a.postJSONWithReferer(ctx, cfg, lease, token, endpoint, payload, timeout, cfg.BaseURL+"/imagine")
}

func (a *Adapter) postJSONWithReferer(ctx context.Context, cfg Config, lease *egress.Lease, token, endpoint string, payload any, timeout time.Duration, referer string) (*http.Response, error) {
	data, _ := json.Marshal(payload)
	for attempt := 0; attempt < 2; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			cancel()
			return nil, err
		}
		request.Header = buildHeaders(token, lease, "application/json")
		applyAppHeaders(request.Header, cfg.BaseURL, referer)
		a.applySignedStatsig(requestCtx, request, token, lease)
		response, err := lease.DoDeferredForbidden(request)
		if err != nil {
			cancel()
			return nil, err
		}
		if response.StatusCode == http.StatusForbidden {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
			_ = response.Body.Close()
			cancel()
			if readErr != nil {
				return nil, fmt.Errorf("读取 Grok Web 403 响应: %w", readErr)
			}
			truncated := len(body) > webMediaDiagnosticBodyLimit
			if truncated {
				body = body[:webMediaDiagnosticBodyLimit]
			}
			upstreamErr := newWebMediaUpstreamError(response.StatusCode, body, truncated)
			response.Body = io.NopCloser(bytes.NewReader(body))
			response.ContentLength = int64(len(body))
			if isClearanceRefreshableMediaError(upstreamErr) {
				lease.InvalidateClearance()
				_ = a.invalidateSignedStatsig(http.MethodPost, endpoint)
				return response, nil
			}
			// Code 7 is the application-layer equivalent of reloading the Grok
			// page: refresh only the path-bound Statsig signature and replay the
			// explicitly rejected POST once. It is not a Cloudflare challenge, so
			// the current Clearance lease remains valid.
			if isStatsigRefreshableMediaError(upstreamErr, body) {
				if attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, endpoint) {
					continue
				}
				return response, nil
			}
			// Remaining structured JSON responses are application policy decisions.
			// They must not invalidate Clearance, affect egress health, or be replayed.
			if upstreamErr.bodyKind == "json" || attempt > 0 || !a.invalidateSignedStatsig(http.MethodPost, endpoint) {
				return response, nil
			}
			continue
		}
		response.Body = &cancelBody{ReadCloser: response.Body, cancel: cancel}
		return response, nil
	}
	return nil, fmt.Errorf("Grok Web Statsig 刷新失败")
}
