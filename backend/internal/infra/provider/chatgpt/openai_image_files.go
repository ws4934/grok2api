// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	_ "golang.org/x/image/webp"
)

const openAIImageReferenceUploadConcurrency = 3

func (o *OpenAIImage) uploadInputs(ctx context.Context, account wireAccount, inputs []OpenAIImageInput) ([]openAIImageReference, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	references := make([]openAIImageReference, len(inputs))
	jobs := make(chan int, len(inputs))
	for index := range inputs {
		jobs <- index
	}
	close(jobs)

	workerCount := min(len(inputs), openAIImageReferenceUploadConcurrency)
	var workers sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	workers.Add(workerCount)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if uploadCtx.Err() != nil {
					return
				}
				ref, uploadErr := o.uploadInput(uploadCtx, account, inputs[index], index+1)
				if uploadErr != nil {
					errOnce.Do(func() {
						firstErr = uploadErr
						cancel()
					})
					return
				}
				references[index] = ref
			}
		}()
	}
	workers.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return references, nil
}

func (o *OpenAIImage) uploadInput(ctx context.Context, account wireAccount, input OpenAIImageInput, index int) (openAIImageReference, error) {
	return o.uploadInputWithPurpose(ctx, account, input, index, false)
}

func (o *OpenAIImage) uploadInputWithPurpose(ctx context.Context, account wireAccount, input OpenAIImageInput, index int, mask bool) (openAIImageReference, error) {
	if len(input.Data) == 0 {
		return openAIImageReference{}, fmt.Errorf("image input %d is empty", index)
	}
	mimeType := strings.TrimSpace(input.MIME)
	if mimeType == "" {
		mimeType = "image/png"
	}
	width, height := imageDimensions(input.Data)
	name := input.Name
	if name == "" {
		name = fmt.Sprintf("image_%d.png", index)
	}
	payload := map[string]any{
		"file_name": name, "file_size": len(input.Data), "use_case": "multimodal",
		"width": width, "height": height,
	}
	if mask {
		payload["use_case"] = "dalle_agent"
		payload["entry_surface"] = "image_edit_mask"
		payload["store_in_library"] = false
	}
	meta, err := o.prepareInputUpload(ctx, account, payload)
	if err != nil {
		return openAIImageReference{}, err
	}
	fileID := stringValue(meta["file_id"])
	uploadURL := stringValue(meta["upload_url"])
	if fileID == "" || uploadURL == "" {
		return openAIImageReference{}, fmt.Errorf("OpenAI image upload returned incomplete metadata")
	}
	err = o.uploadInputBlob(ctx, account, uploadURL, input.Data, map[string]string{
		"Content-Type": mimeType, "x-ms-blob-type": "BlockBlob", "x-ms-version": "2020-04-08",
		"Accept": "application/json, text/plain, */*",
	})
	if err != nil {
		return openAIImageReference{}, err
	}
	response, err := o.do(ctx, http.MethodPost, "/backend-api/files/"+url.PathEscape(fileID)+"/uploaded", account, map[string]any{}, map[string]string{"Content-Type": "application/json", "Accept": "application/json"}, false)
	if err != nil {
		return openAIImageReference{}, err
	}
	_ = response.Body.Close()
	return openAIImageReference{FileID: fileID, FileName: name, FileSize: len(input.Data), MIME: mimeType, Width: width, Height: height}, nil
}

// downloadedImage is a validated image download with its decoded pixels, so
// the output options do not decode it a second time.
type downloadedImage struct {
	raw     []byte
	mime    string
	decoded image.Image
}

func (o *OpenAIImage) downloadFile(ctx context.Context, account wireAccount, fileID string) (downloadedImage, error) {
	response, err := o.do(ctx, http.MethodGet, "/backend-api/files/"+url.PathEscape(fileID)+"/download", account, nil, nil, false)
	if err != nil {
		return downloadedImage{}, err
	}
	return o.readImageDownloadResponse(ctx, account, response)
}

func (o *OpenAIImage) downloadImageRef(ctx context.Context, account wireAccount, conversationID, imageRef string) (downloadedImage, error) {
	if strings.HasPrefix(imageRef, "sediment://") {
		attachmentID := strings.TrimPrefix(imageRef, "sediment://")
		if conversationID == "" || attachmentID == "" {
			return downloadedImage{}, fmt.Errorf("OpenAI sediment image reference is incomplete")
		}
		path := "/backend-api/conversation/" + url.PathEscape(conversationID) + "/attachment/" + url.PathEscape(attachmentID) + "/download"
		response, err := o.do(ctx, http.MethodGet, path, account, nil, map[string]string{
			"Accept":                "application/json, image/*, */*",
			"Referer":               o.BaseURL + "/c/" + conversationID,
			"X-OpenAI-Target-Route": "/backend-api/conversation/{conversation_id}/attachment/{attachment_id}/download",
		}, false)
		if err != nil {
			return downloadedImage{}, err
		}
		return o.readImageDownloadResponse(ctx, account, response)
	}
	return o.downloadFile(ctx, account, strings.TrimPrefix(imageRef, "file-service://"))
}

// fetchImageURL downloads a resolved image URL. Same-origin URLs such as
// chatgpt.com/backend-api/estuary/content need the account session (without
// it the upstream answers 403 "File stream access denied"); other hosts are
// signed CDN URLs fetched without credentials.
func (o *OpenAIImage) fetchImageURL(ctx context.Context, account wireAccount, rawURL string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	base, baseErr := url.Parse(o.BaseURL)
	if err == nil && baseErr == nil && u.User == nil && (u.Host == "" || strings.EqualFold(u.Host, base.Host)) {
		return o.doRequest(ctx, http.MethodGet, o.BaseURL+u.RequestURI(), u.Path, account, nil, map[string]string{"Accept": "image/*, */*"}, false, true)
	}
	return o.doAbsolute(ctx, http.MethodGet, rawURL, account, nil, nil, false)
}

func (o *OpenAIImage) readImageDownloadResponse(ctx context.Context, account wireAccount, response *http.Response) (downloadedImage, error) {
	var value map[string]any
	raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
	_ = response.Body.Close()
	if err != nil {
		return downloadedImage{}, err
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") || json.Valid(raw) {
		if json.Unmarshal(raw, &value) != nil {
			return downloadedImage{}, fmt.Errorf("decode OpenAI image download metadata")
		}
		downloadURL := firstStringValue(value, "download_url", "url")
		if downloadURL == "" {
			return downloadedImage{}, fmt.Errorf("OpenAI image download returned no URL")
		}
		response, err = o.fetchImageURL(ctx, account, downloadURL)
		if err != nil {
			return downloadedImage{}, err
		}
		raw, err = io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
		_ = response.Body.Close()
		if err != nil {
			return downloadedImage{}, err
		}
	}
	mimeType, decoded, err := decodeImageDownload(raw)
	if err != nil {
		return downloadedImage{}, err
	}
	return downloadedImage{raw: raw, mime: mimeType, decoded: decoded}, nil
}

// validatedImageMIME is decodeImageDownload without the pixels.
func validatedImageMIME(raw []byte) (string, error) {
	mimeType, _, err := decodeImageDownload(raw)
	return mimeType, err
}

// Headers and extensions are advisory. A 200 storage error document is not an
// image; decoding prevents XML/HTML, truncated data and spoofed MIME successes.
func decodeImageDownload(raw []byte) (string, image.Image, error) {
	invalid := func(message string) (string, image.Image, error) {
		return "", nil, &upstreamError{Status: http.StatusBadGateway, Message: message}
	}
	if len(raw) == 0 || len(raw) > 64<<20 {
		return invalid("OpenAI image download is empty or exceeds 64 MiB")
	}
	var storageError struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
	}
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf}))
	if len(trimmed) > 0 && trimmed[0] == '<' && len(trimmed) <= 64<<10 && xml.Unmarshal(trimmed, &storageError) == nil && storageError.XMLName.Local == "Error" {
		// Never copy arbitrary upstream messages or signed URLs into client errors.
		if storageError.Code == "ServerBusy" {
			return invalid("OpenAI image storage returned ServerBusy instead of image bytes")
		}
		return invalid("OpenAI image storage returned an error document instead of image bytes")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 || uint64(config.Width) > 64_000_000/uint64(config.Height) {
		return invalid("OpenAI image download is not a supported image or exceeds the pixel limit")
	}
	// DecodeConfig alone accepts files truncated after their dimensions header.
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return invalid("OpenAI image download contains corrupt or truncated image bytes")
	}
	switch format {
	case "png", "jpeg", "gif", "webp":
		return "image/" + format, decoded, nil
	default:
		return invalid("OpenAI image download has an unsupported image format")
	}
}

func imageDimensions(raw []byte) (int, int) {
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 1024, 1024
	}
	return config.Width, config.Height
}
