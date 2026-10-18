package httpserver

// SwaggerMessage 表示 Chat Completions 请求中的一条消息。
type SwaggerMessage struct {
	Role    string `json:"role" example:"user"`
	Content any    `json:"content"`
}

// SwaggerResponsesRequest 表示最小 Responses 请求。
type SwaggerResponsesRequest struct {
	Model              string `json:"model" example:"grok-chat-auto"`
	Input              any    `json:"input"`
	Stream             bool   `json:"stream" example:"false"`
	Store              bool   `json:"store" example:"false"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	PromptCacheKey     string `json:"prompt_cache_key,omitempty"`
	// Tools may carry one image_generation tool; Background then returns immediately.
	Tools      []any `json:"tools,omitempty"`
	Background bool  `json:"background,omitempty"`
}

// SwaggerChatRequest 表示最小 Chat Completions 请求。
type SwaggerChatRequest struct {
	Model    string           `json:"model" example:"grok-chat-fast"`
	Messages []SwaggerMessage `json:"messages"`
	Stream   bool             `json:"stream" example:"false"`
}

// SwaggerMessagesRequest 表示最小 Anthropic Messages 请求。
type SwaggerMessagesRequest struct {
	Model     string           `json:"model" example:"grok-chat-expert"`
	MaxTokens int              `json:"max_tokens" example:"1024"`
	Messages  []SwaggerMessage `json:"messages"`
	Stream    bool             `json:"stream" example:"false"`
}

// SwaggerImageGenerationRequest 表示图片生成请求；quality/size/output_format/background 的取值由目标 Provider 校验。
type SwaggerImageGenerationRequest struct {
	Model          string `json:"model" example:"grok-imagine-image-quality"`
	Prompt         string `json:"prompt" example:"A cinematic city at night"`
	N              int    `json:"n" example:"1"`
	Size           string `json:"size,omitempty" example:"1024x1024"`
	AspectRatio    string `json:"aspect_ratio,omitempty" example:"16:9"`
	Resolution     string `json:"resolution,omitempty" example:"2k"`
	Quality        string `json:"quality,omitempty" example:"medium"`
	ResponseFormat string `json:"response_format,omitempty" example:"url"`
	OutputFormat   string `json:"output_format,omitempty" example:"png"`
	Background     string `json:"background,omitempty" example:"auto"`
	Stream         bool   `json:"stream,omitempty" example:"false"`
	PartialImages  int    `json:"partial_images,omitempty" example:"0"`
}

// SwaggerImageReference 表示图片输入对象；也可直接传 Base64 图片 data URI、https URL 或 Base64 字符串，不支持 file_id。
type SwaggerImageReference struct {
	URL      string `json:"url,omitempty" example:"https://example.com/source.png"`
	ImageURL string `json:"image_url,omitempty"`
	B64JSON  string `json:"b64_json,omitempty"`
}

// SwaggerImageEditRequest 表示图片编辑请求；multipart 形式用 image/image[]/images[] 与 mask 文件字段，其余字段同名。
type SwaggerImageEditRequest struct {
	Model          string                  `json:"model" example:"grok-imagine-image-edit"`
	Prompt         string                  `json:"prompt" example:"Change the background to black"`
	Image          SwaggerImageReference   `json:"image"`
	Images         []SwaggerImageReference `json:"images,omitempty"`
	Mask           *SwaggerImageReference  `json:"mask,omitempty"`
	N              int                     `json:"n" example:"1"`
	Size           string                  `json:"size,omitempty" example:"1024x1024"`
	AspectRatio    string                  `json:"aspect_ratio,omitempty" example:"1:1"`
	Resolution     string                  `json:"resolution,omitempty" example:"1k"`
	Quality        string                  `json:"quality,omitempty" example:"medium"`
	ResponseFormat string                  `json:"response_format,omitempty" example:"url"`
	OutputFormat   string                  `json:"output_format,omitempty" example:"png"`
	Background     string                  `json:"background,omitempty" example:"auto"`
	Stream         bool                    `json:"stream,omitempty" example:"false"`
	PartialImages  int                     `json:"partial_images,omitempty" example:"0"`
}

// SwaggerVideoGenerationRequest 表示视频生成请求。
// image 与 reference_images/reference_audios 互斥；参考图模式 resolution 最高 720p。
type SwaggerVideoGenerationRequest struct {
	Model            string                    `json:"model" example:"grok-imagine-video"`
	Prompt           string                    `json:"prompt" example:"A cinematic tracking shot in the rain"`
	Duration         int                       `json:"duration" example:"8"`
	AspectRatio      string                    `json:"aspect_ratio,omitempty" example:"16:9"`
	Resolution       string                    `json:"resolution,omitempty" example:"720p"`
	Image            *SwaggerVideoMediaInput   `json:"image,omitempty"`
	ReferenceImages  []SwaggerVideoMediaInput  `json:"reference_images,omitempty"`
	ReferenceAudios  []SwaggerVideoAudioInput  `json:"reference_audios,omitempty"`
	Video            *SwaggerVideoMediaInput   `json:"video,omitempty"`
}

// SwaggerVideoMediaInput 表示视频相关的图片/视频输入。
type SwaggerVideoMediaInput struct {
	URL    string `json:"url,omitempty" example:"https://example.com/input.png"`
	FileID string `json:"file_id,omitempty" example:"file_123"`
}

// SwaggerVideoAudioInput 表示参考音频（内置 voice_id）。
type SwaggerVideoAudioInput struct {
	VoiceID string `json:"voice_id" example:"eve"`
}

// swaggerHealth godoc
// @Summary 存活检查
// @Tags System
// @Produce json
// @Success 200 {object} map[string]bool
// @Router /healthz [get]
func swaggerHealth() {}

// swaggerReady godoc
// @Summary 就绪检查
// @Tags System
// @Produce json
// @Success 200 {object} map[string]bool
// @Failure 503 {object} map[string]bool
// @Router /readyz [get]
func swaggerReady() {}

// swaggerModels godoc
// @Summary 获取可用模型
// @Tags Models
// @Security BearerAuth
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Router /v1/models [get]
func swaggerModels() {}

// swaggerResponses godoc
// @Summary 创建 Response
// @Description 支持 JSON 与 SSE；stream=true 时返回 text/event-stream。
// @Description 携带 image_generation 工具且图片路由支持可恢复图片任务（如 ChatGPT）时按图片任务执行：默认等待并返回含 image_generation_call 的 Response；background=true 立即返回 queued 对象，用 GET /v1/responses/{response_id} 查询、POST /v1/responses/{response_id}/cancel 取消。工具未指定 model 且顶层模型为该 Provider 的文本模型时使用运行时设置 providerChatGPT.defaultImageModel。
// @Tags Responses
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param Idempotency-Key header string false "图片任务幂等键：相同请求返回同一任务，请求体不同返回 409"
// @Param request body SwaggerResponsesRequest true "请求"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Router /v1/responses [post]
func swaggerResponses() {}

// swaggerCompactResponse godoc
// @Summary 压缩 Response 上下文
// @Tags Responses
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerResponsesRequest true "请求"
// @Success 200 {object} map[string]any
// @Router /v1/responses/compact [post]
func swaggerCompactResponse() {}

// swaggerGetResponse godoc
// @Summary 查询 Response
// @Description resp_img_ 前缀的 ID 为图片任务，返回其当前状态与已生成的图片。
// @Tags Responses
// @Security BearerAuth
// @Produce json
// @Param response_id path string true "Response ID"
// @Success 200 {object} map[string]any
// @Failure 404 {object} map[string]any
// @Router /v1/responses/{response_id} [get]
func swaggerGetResponse() {}

// swaggerDeleteResponse godoc
// @Summary 删除 Response
// @Tags Responses
// @Security BearerAuth
// @Produce json
// @Param response_id path string true "Response ID"
// @Success 200 {object} map[string]any
// @Failure 404 {object} map[string]any
// @Router /v1/responses/{response_id} [delete]
func swaggerDeleteResponse() {}

// swaggerCancelResponse godoc
// @Summary 取消 Response
// @Description 仅图片任务可取消，且其图片尚未提交上游；已结束的任务原样返回。
// @Tags Responses
// @Security BearerAuth
// @Produce json
// @Param response_id path string true "Response ID"
// @Success 200 {object} map[string]any
// @Failure 404 {object} map[string]any
// @Failure 409 {object} map[string]any
// @Router /v1/responses/{response_id}/cancel [post]
func swaggerCancelResponse() {}

// swaggerChat godoc
// @Summary 创建 Chat Completion
// @Description 支持 JSON 与 SSE、图片输入和函数工具。
// @Tags Chat
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerChatRequest true "请求"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/chat/completions [post]
func swaggerChat() {}

// swaggerMessages godoc
// @Summary 创建 Anthropic Message
// @Tags Messages
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param anthropic-version header string true "Anthropic API version" default(2023-06-01)
// @Param request body SwaggerMessagesRequest true "请求"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/messages [post]
func swaggerMessages() {}

// swaggerGenerateImage godoc
// @Summary 生成图片
// @Tags Images
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerImageGenerationRequest true "请求"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/images/generations [post]
func swaggerGenerateImage() {}

// swaggerEditImage godoc
// @Summary 编辑图片
// @Tags Images
// @Security BearerAuth
// @Accept json,mpfd
// @Produce json
// @Param request body SwaggerImageEditRequest true "请求"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/images/edits [post]
func swaggerEditImage() {}

// swaggerGetImage godoc
// @Summary 获取归档图片
// @Tags Images
// @Produce image/png
// @Param asset_id path string true "Asset ID"
// @Success 200 {file} binary
// @Failure 404
// @Router /v1/media/images/{asset_id} [get]
func swaggerGetImage() {}

// swaggerGenerateVideo godoc
// @Summary 创建异步视频任务
// @Tags Videos
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerVideoGenerationRequest true "请求"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]any
// @Router /v1/videos/generations [post]
func swaggerGenerateVideo() {}

// swaggerEditVideo godoc
// @Summary 创建异步视频编辑任务
// @Tags Videos
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerVideoGenerationRequest true "请求"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]any
// @Router /v1/videos/edits [post]
func swaggerEditVideo() {}

// swaggerExtendVideo godoc
// @Summary 创建异步视频延长任务
// @Tags Videos
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerVideoGenerationRequest true "请求"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]any
// @Router /v1/videos/extensions [post]
func swaggerExtendVideo() {}

// swaggerGetVideo godoc
// @Summary 查询异步视频任务
// @Tags Videos
// @Security BearerAuth
// @Produce json
// @Param request_id path string true "Request ID"
// @Success 200 {object} map[string]any
// @Failure 404 {object} map[string]any
// @Router /v1/videos/{request_id} [get]
func swaggerGetVideo() {}
