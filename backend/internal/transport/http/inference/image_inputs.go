package inference

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

var (
	errImageFileID    = errors.New("当前暂不支持 file_id 图片引用，请改用 image_url、url 或 Base64")
	errImageReference = errors.New("图片必须是 Base64 图片 data URI、https URL、Base64 字符串，或包含 image_url、url、b64_json 的对象")
	errImageFormValue = errors.New("n、partial_images 必须是整数，stream 必须是布尔值")
)

// readImageEditRequest decodes an edit request from JSON or multipart form data
// and normalizes every image and mask input to a data URI or https URL. It
// writes the client error itself and reports whether the request is usable.
func readImageEditRequest(c *gin.Context) (imageEditJSONRequest, []string, []string, bool) {
	var request imageEditJSONRequest
	var images, masks []string
	var err error
	switch {
	case isJSONRequest(c):
		body, readErr := io.ReadAll(c.Request.Body)
		if readErr != nil {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
			return request, nil, nil, false
		}
		if decodeSingleJSON(bytes.NewReader(body), &request, false) != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑 JSON 请求无效")
			return request, nil, nil, false
		}
		if images, err = imageInputURLs(request.Image, request.Images, request.ImageURL); err == nil {
			masks, err = imageInputURLs(request.Mask)
		}
	case isMultipartRequest(c):
		form, formErr := c.MultipartForm()
		if formErr != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(formErr, &tooLarge) {
				writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
			} else {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑 multipart 请求无效")
			}
			return request, nil, nil, false
		}
		defer form.RemoveAll()
		request, images, masks, err = readImageEditForm(form)
	default:
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "图片编辑仅支持 application/json 或 multipart/form-data")
		return request, nil, nil, false
	}
	if err != nil {
		code := "invalid_request"
		switch {
		case errors.Is(err, errImageFileID):
			code = "unsupported_parameter"
		case errors.Is(err, errImageFormValue):
			code = "invalid_parameter"
		}
		writeOpenAIError(c, http.StatusBadRequest, code, err.Error())
		return request, nil, nil, false
	}
	return request, images, masks, true
}

func isMultipartRequest(c *gin.Context) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "multipart/form-data")
}

// readImageEditForm maps the official multipart fields; image files and text
// references may use image, image[], images, images[], image_url or image_url[].
func readImageEditForm(form *multipart.Form) (imageEditJSONRequest, []string, []string, error) {
	value := func(name string) string {
		if values := form.Value[name]; len(values) > 0 {
			return values[0]
		}
		return ""
	}
	request := imageEditJSONRequest{
		Model: value("model"), Prompt: value("prompt"), Size: value("size"), AspectRatio: value("aspect_ratio"),
		Resolution: value("resolution"), Quality: value("quality"), ResponseFormat: value("response_format"),
		OutputFormat: value("output_format"), Background: value("background"),
	}
	var err error
	if request.Count, err = formInt(value("n")); err != nil {
		return request, nil, nil, errImageFormValue
	}
	if request.PartialImages, err = formInt(value("partial_images")); err != nil {
		return request, nil, nil, errImageFormValue
	}
	if raw := value("stream"); raw != "" {
		if request.Stream, err = strconv.ParseBool(raw); err != nil {
			return request, nil, nil, errImageFormValue
		}
	}
	images, err := formImageInputURLs(form, "image", "image[]", "images", "images[]", "image_url", "image_url[]")
	if err != nil {
		return request, nil, nil, err
	}
	masks, err := formImageInputURLs(form, "mask")
	return request, images, masks, err
}

func formInt(raw string) (*int, error) {
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.Atoi(raw)
	return &value, err
}

func formImageInputURLs(form *multipart.Form, names ...string) ([]string, error) {
	var result []string
	for _, name := range names {
		for _, file := range form.File[name] {
			value, err := formFileDataURI(file)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		for _, text := range form.Value[name] {
			// A text value is one reference, or a JSON array or object of them.
			raw, _ := json.Marshal(text)
			if trimmed := strings.TrimSpace(text); strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
				raw = []byte(trimmed)
			}
			values, err := imageInputURLs(raw)
			if err != nil {
				return nil, err
			}
			result = append(result, values...)
		}
	}
	return result, nil
}

func formFileDataURI(file *multipart.FileHeader) (string, error) {
	source, err := file.Open()
	if err != nil {
		return "", errImageReference
	}
	defer source.Close()
	raw, err := io.ReadAll(source)
	if err != nil {
		return "", errImageReference
	}
	return imageDataURI(raw)
}

func imageInputURLs(values ...json.RawMessage) ([]string, error) {
	var result []string
	for _, value := range values {
		references, err := imageReferences(value, false)
		if err != nil {
			return nil, err
		}
		for _, reference := range references {
			normalized, err := imageInputURL(reference)
			if err != nil {
				return nil, err
			}
			result = append(result, normalized)
		}
	}
	return result, nil
}

// imageReferences flattens the official and reference image forms into
// strings: a string, {"image_url": string | {"url"}}, {"url"},
// {"b64_json"|"base64"}, or a top-level array of those.
func imageReferences(raw json.RawMessage, nested bool) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return []string{value}, nil
	}
	var list []json.RawMessage
	if !nested && json.Unmarshal(raw, &list) == nil {
		var result []string
		for _, item := range list {
			values, err := imageReferences(item, true)
			if err != nil {
				return nil, err
			}
			result = append(result, values...)
		}
		return result, nil
	}
	var object struct {
		ImageURL json.RawMessage `json:"image_url"`
		URL      string          `json:"url"`
		B64JSON  string          `json:"b64_json"`
		Base64   string          `json:"base64"`
		FileID   string          `json:"file_id"`
	}
	if json.Unmarshal(raw, &object) != nil {
		return nil, errImageReference
	}
	switch {
	case object.FileID != "":
		return nil, errImageFileID
	case object.B64JSON != "":
		return []string{object.B64JSON}, nil
	case object.Base64 != "":
		return []string{object.Base64}, nil
	case object.URL != "":
		return []string{object.URL}, nil
	case len(object.ImageURL) > 0 && !bytes.Equal(object.ImageURL, []byte("null")):
		if json.Unmarshal(object.ImageURL, &value) != nil {
			var nestedURL struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(object.ImageURL, &nestedURL) != nil || nestedURL.URL == "" {
				return nil, errImageReference
			}
			value = nestedURL.URL
		}
		return []string{value}, nil
	}
	return nil, errImageReference
}

// imageInputURL keeps base64 image data URIs and https URLs as they are and
// turns bare base64 into a data URI; providers fetch and validate the content.
func imageInputURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return value, nil
	case strings.HasPrefix(lower, "data:"):
		header, _, ok := strings.Cut(lower, ",")
		if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
			return "", errImageReference
		}
		return value, nil
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", errImageReference
	}
	return imageDataURI(raw)
}

func imageDataURI(raw []byte) (string, error) {
	mimeType := http.DetectContentType(raw)
	if !strings.HasPrefix(mimeType, "image/") {
		return "", errImageReference
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}
