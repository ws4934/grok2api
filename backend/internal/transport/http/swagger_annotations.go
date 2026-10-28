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
// duration / aspect_ratio / resolution 不传时用模型默认值（Grok：8 秒、16:9、720p）。
// Grok 路由限 1–15 秒、7 种比例与 480p/720p/1080p，参考图模式最高 720p；Adobe 模型按
// 自身范围，不支持的值就近对齐，实际时长见结果的 video.duration。
// image 能否与 reference_images 同时使用由模型决定：Muse 与 Adobe kling-3.0-omni 可以，Grok 不行。
// last_frame（尾帧）必须同时提供 image，目前仅 Adobe 支持首尾帧的模型接受。
// reference_videos（参考视频，url 或 file_id，MP4/QuickTime，最多 10 个）需提供 prompt，
// 目前仅 Adobe 的 Seedance 接受（不能与首尾帧同用）。
// 编辑与延长（/v1/videos/edits、/v1/videos/extensions）需 prompt 与 video：Grok 走 Console
// 的 grok-imagine-video（编辑不收 duration、比例、分辨率；延长 2–10 秒，默认 6）；Adobe 的
// seedance-2.5 支持编辑与延长（延长 4–30 秒），kling-3.0-omni 支持编辑。
type SwaggerVideoGenerationRequest struct {
	Model       string                  `json:"model" example:"grok-imagine-video"`
	Prompt      string                  `json:"prompt" example:"A cinematic tracking shot in the rain"`
	Duration    int                     `json:"duration" example:"8"`
	AspectRatio string                  `json:"aspect_ratio,omitempty" example:"16:9"`
	Resolution  string                  `json:"resolution,omitempty" example:"720p"`
	Image       *SwaggerVideoMediaInput `json:"image,omitempty"`
	LastFrame   *SwaggerVideoMediaInput `json:"last_frame,omitempty"`
	// Reference images: Console 1.5 accepts 14 alongside image; classic Console accepts 7, Build 8, both exclusive with image. Extra inputs are trimmed.
	ReferenceImages []SwaggerVideoMediaInput `json:"reference_images,omitempty"`
	ReferenceAudios []SwaggerVideoAudioInput `json:"reference_audios,omitempty"`
	ReferenceVideos []SwaggerVideoMediaInput `json:"reference_videos,omitempty"`
	Video           *SwaggerVideoMediaInput  `json:"video,omitempty"`
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

// SwaggerTTSOutputFormat 表示 TTS 输出格式；设置 codec 时优先于 response_format。
type SwaggerTTSOutputFormat struct {
	Codec      string `json:"codec,omitempty" example:"mp3"`
	SampleRate int    `json:"sample_rate,omitempty" example:"24000"`
	BitRate    int    `json:"bit_rate,omitempty" example:"128000"`
}

// SwaggerSpeechRequest 表示 OpenAI 兼容语音合成请求；language、voice_id、output_format、optimize_streaming_latency、text_normalization、with_timestamps 为 Grok 扩展字段。
type SwaggerSpeechRequest struct {
	Model                    string                  `json:"model,omitempty" example:"grok-voice-latest"`
	Input                    string                  `json:"input" example:"Hello from the gateway"`
	Voice                    string                  `json:"voice,omitempty" example:"alloy"`
	ResponseFormat           string                  `json:"response_format,omitempty" example:"mp3"`
	Speed                    float64                 `json:"speed,omitempty" example:"1"`
	Language                 string                  `json:"language,omitempty" example:"en"`
	VoiceID                  string                  `json:"voice_id,omitempty" example:"eve"`
	OutputFormat             *SwaggerTTSOutputFormat `json:"output_format,omitempty"`
	OptimizeStreamingLatency int                     `json:"optimize_streaming_latency,omitempty" example:"0"`
	TextNormalization        bool                    `json:"text_normalization,omitempty" example:"false"`
	WithTimestamps           bool                    `json:"with_timestamps,omitempty" example:"false"`
}

// SwaggerTTSRequest 表示原生 TTS 请求；text 与 language 必填。
type SwaggerTTSRequest struct {
	Model                    string                  `json:"model,omitempty" example:"grok-voice-latest"`
	Text                     string                  `json:"text" example:"Hello from the gateway"`
	VoiceID                  string                  `json:"voice_id,omitempty" example:"eve"`
	Language                 string                  `json:"language" example:"en"`
	OutputFormat             *SwaggerTTSOutputFormat `json:"output_format,omitempty"`
	Speed                    float64                 `json:"speed,omitempty" example:"1"`
	OptimizeStreamingLatency int                     `json:"optimize_streaming_latency,omitempty" example:"0"`
	TextNormalization        bool                    `json:"text_normalization,omitempty" example:"false"`
	WithTimestamps           bool                    `json:"with_timestamps,omitempty" example:"false"`
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

// swaggerApplyUpdate godoc
// @Summary 在线更新到最新版本
// @Description 需要管理端登录会话。下载 GitHub Release 中当前平台的 grok2api_<tag>_<os>_<arch>.tar.gz，按 checksums.txt 校验 SHA-256 后替换可执行文件、前端静态目录与 VERSION，原文件保留为 <路径>.backup。
// @Description 仅在 GET /api/admin/v1/system/version 返回 canUpdate=true 时可用；同一时间只允许一个更新或回滚。返回 data.version 为新版本；data.restarting=true 表示受 Docker 或 systemd 托管、服务即将自动重启，否则需手动重启。
// @Tags System
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 409 {object} map[string]any "没有新版本、当前安装不支持在线更新或已有操作进行中"
// @Failure 500 {object} map[string]any
// @Router /api/admin/v1/system/update/apply [post]
func swaggerApplyUpdate() {}

// swaggerRollbackUpdate godoc
// @Summary 回滚上次在线更新
// @Description 需要管理端登录会话。用上次更新留下的 .backup 换回可执行文件、前端静态目录与 VERSION，响应与在线更新相同（data.version 为回滚后的版本）。
// @Tags System
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 409 {object} map[string]any "没有可回滚的备份或已有操作进行中"
// @Failure 500 {object} map[string]any
// @Router /api/admin/v1/system/update/rollback [post]
func swaggerRollbackUpdate() {}

// swaggerModels godoc
// @Summary 获取可用模型
// @Description ChatGPT 账号的模型：文本模型来自账号模型同步（如 auto、gpt-6、gpt-6-mini、gpt-5-3-mini）；图片模型 gpt-image-2、gpt-image-2.5、gpt-image-2.5-flare、gpt-image-2.5-sunburst；语音模型 chatgpt-tts、chatgpt-transcribe。
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
// @Description ChatGPT 文本模型（账号模型同步，如 auto、gpt-6、gpt-6-mini、gpt-5-3-mini）：usage 为本地 o200k_base 估算（上游不返回用量）；不支持的采样参数被忽略；reasoning.effort 映射为上游推理强度；回复为纯 Markdown（网关要求 ChatGPT 不输出网页 UI 组件）。
// @Description ChatGPT 函数工具：通过提示词与 XML 调用约定模拟，返回 function_call 项；只解析已声明的函数名；支持 tool_choice 的 none、required 与指定函数，parallel_tool_calls=false 时只保留第一个调用；最多 128 个工具，每个 description 不超过 16 KB；不模拟 custom 自由格式工具；能否产生调用取决于模型是否遵循约定格式。
// @Description ChatGPT 联网搜索：web_search / web_search_preview 工具由 ChatGPT 在服务端搜索，结果为 web_search_call 项加 url_citation 注释；支持用 tool_choice 强制搜索；实时类问题即使未声明工具也可能触发 ChatGPT 自动搜索。
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
// @Description ChatGPT 文本模型（账号模型同步，如 auto、gpt-6、gpt-6-mini、gpt-5-3-mini）：usage 为本地 o200k_base 估算（上游不返回用量）；不支持的采样参数被忽略；reasoning_effort 映射为上游推理强度；回复为纯 Markdown（网关要求 ChatGPT 不输出网页 UI 组件）。
// @Description ChatGPT 函数工具：通过提示词与 XML 调用约定模拟，返回 tool_calls 且 finish_reason 为 tool_calls；只解析已声明的函数名；支持 tool_choice 的 none、required 与指定函数，parallel_tool_calls=false 时只保留第一个调用；最多 128 个工具，每个 description 不超过 16 KB；不模拟 custom 自由格式工具；能否产生调用取决于模型是否遵循约定格式。
// @Description ChatGPT 联网搜索：web_search_options 由 ChatGPT 在服务端搜索，引用以 url_citation 写入 annotations；实时类问题即使未开启搜索也可能触发 ChatGPT 自动搜索。
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
// @Description ChatGPT 文本模型（账号模型同步，如 auto、gpt-6、gpt-6-mini、gpt-5-3-mini）：usage 为本地 o200k_base 估算（上游不返回用量）；不支持的采样参数被忽略；回复为纯 Markdown（网关要求 ChatGPT 不输出网页 UI 组件）。
// @Description ChatGPT 函数工具：通过提示词与 XML 调用约定模拟，返回 tool_use 块；只解析已声明的函数名；支持 tool_choice 的 none、required 与指定函数，parallel_tool_calls=false 时只保留第一个调用；最多 128 个工具，每个 description 不超过 16 KB；不模拟 custom 自由格式工具；能否产生调用取决于模型是否遵循约定格式。
// @Description ChatGPT 联网搜索：web_search_* 工具由 ChatGPT 在服务端搜索，结果为 server_tool_use 与 web_search_tool_result 块；支持用 tool_choice 强制搜索；实时类问题即使未声明工具也可能触发 ChatGPT 自动搜索。
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
// @Description ChatGPT 图片模型：gpt-image-2、gpt-image-2.5、gpt-image-2.5-flare、gpt-image-2.5-sunburst。
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
// @Description ChatGPT 图片模型：gpt-image-2、gpt-image-2.5、gpt-image-2.5-flare、gpt-image-2.5-sunburst。
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

// swaggerAudioSpeech godoc
// @Summary 合成语音（OpenAI 兼容）
// @Description 默认直接返回音频；with_timestamps=true 或上游返回 JSON 封装时返回 JSON：audio（Base64）、content_type、duration，可选 audio_timestamps（graph_chars、graph_times）。
// @Description model 缺省为 grok-voice-latest，language 缺省为 en；response_format 支持 mp3、opus（ogg）、aac、flac、wav、pcm，缺省为 mp3，output_format.codec 优先；speed 取值 0.25 到 4.0；optimize_streaming_latency 取值 0 到 4；voice_id 优先于 voice，OpenAI 音色名映射为 Grok 音色（alloy/verse→ara、echo/ballad→eve、fable/coral→sal、onyx/ash→rex、nova/sage→leo、shimmer/marin→sia），其余值原样透传。
// @Description ChatGPT 模型 chatgpt-tts：response_format 仅支持 mp3、aac、opus（wav、flac、pcm 返回 400）；input 不超过 4096 字符；忽略 speed 与 language；voice 按账号音色 ID 或显示名匹配（breeze、cove、glimmer（显示名 Sol）、orbit（显示名 Spruce）、maple、juniper、ember、vale、fathom（显示名 Arbor）），未知名称（如 OpenAI 的 alloy）使用账号当前选中的音色；单次合成约 9 秒。
// @Tags Audio
// @Security BearerAuth
// @Accept json
// @Produce audio/mpeg,json
// @Param request body SwaggerSpeechRequest true "请求"
// @Success 200 {file} binary
// @Failure 400 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Failure 415 {object} map[string]any
// @Router /v1/audio/speech [post]
func swaggerAudioSpeech() {}

// swaggerAudioTask godoc
// @Summary 合成语音（/v1/audio/speech 兼容别名）
// @Description 行为与参数同 POST /v1/audio/speech（含 ChatGPT chatgpt-tts 的限制）：默认直接返回音频，with_timestamps=true 时返回 JSON。
// @Tags Audio
// @Security BearerAuth
// @Accept json
// @Produce audio/mpeg,json
// @Param request body SwaggerSpeechRequest true "请求"
// @Success 200 {file} binary
// @Failure 400 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Failure 415 {object} map[string]any
// @Router /v1/audio/tasks [post]
func swaggerAudioTask() {}

// swaggerAudioTranscription godoc
// @Summary 转写音频（OpenAI 兼容）
// @Description 接受 multipart/form-data，或字段同名的 application/json（用 url 代替 file）；file 与 url 提供其一。model 缺省为 grok-stt。
// @Description response_format：json（缺省）返回 text；text 返回纯文本；verbose_json 返回 task、text、language、duration 与 words（word、start、end、可选 speaker）。传 prompt、非 0 的 temperature 或 timestamp_granularities 返回 400。其余 Console 扩展字段同 POST /v1/stt。
// @Description ChatGPT 模型 chatgpt-transcribe：只接受上传的 file（不超过 25 MB，不支持 url），只返回文本（无 words、language、duration）。
// @Tags Audio
// @Security BearerAuth
// @Accept mpfd,json
// @Produce json,plain
// @Param file formData file false "音频文件"
// @Param url formData string false "音频 URL"
// @Param model formData string false "模型" default(grok-stt)
// @Param language formData string false "语言"
// @Param response_format formData string false "响应格式" Enums(json,text,verbose_json) default(json)
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Failure 415 {object} map[string]any
// @Router /v1/audio/transcriptions [post]
func swaggerAudioTranscription() {}

// swaggerTTS godoc
// @Summary 合成语音
// @Description 原生 Console 兼容接口：text 与 language 必填，model 缺省为 grok-voice-latest；speed 取值 0.25 到 4.0；optimize_streaming_latency 取值 0 到 4。默认直接返回音频；with_timestamps=true 或上游返回 JSON 封装时返回 JSON：audio（Base64）、content_type、duration，可选 audio_timestamps。
// @Description ChatGPT 模型 chatgpt-tts：output_format.codec 缺省为 mp3，仅支持 mp3、aac、opus；text 不超过 4096 字符；language 仍需传但会被忽略，speed 也被忽略；voice_id 的匹配规则同 POST /v1/audio/speech。
// @Tags Audio
// @Security BearerAuth
// @Accept json
// @Produce audio/mpeg,json
// @Param request body SwaggerTTSRequest true "请求"
// @Success 200 {file} binary
// @Failure 400 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Failure 415 {object} map[string]any
// @Router /v1/tts [post]
func swaggerTTS() {}

// swaggerListTTSVoices godoc
// @Summary 列出 TTS 音色
// @Description 返回 voices 数组，每项含 voice_id、name、language（可能为 null）。model=chatgpt-tts 时列出 ChatGPT 账号的音色。
// @Tags Audio
// @Security BearerAuth
// @Produce json
// @Param model query string false "模型" default(grok-voice-latest)
// @Success 200 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Router /v1/tts/voices [get]
func swaggerListTTSVoices() {}

// swaggerGetTTSVoice godoc
// @Summary 查询 TTS 音色
// @Description 返回 voice_id、name、language（可能为 null）。
// @Tags Audio
// @Security BearerAuth
// @Produce json
// @Param voiceId path string true "Voice ID"
// @Param model query string false "模型" default(grok-voice-latest)
// @Success 200 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Failure 404 {object} map[string]any
// @Router /v1/tts/voices/{voiceId} [get]
func swaggerGetTTSVoice() {}

// swaggerSTT godoc
// @Summary 转写音频
// @Description 原生 Console 兼容接口：接受 multipart/form-data，或字段同名的 application/json（用 url 代替 file）；file 与 url 提供其一。model 缺省为 grok-stt。
// @Description 返回上游原始 JSON，或 text、language、duration 与可选 words（text、start、end、speaker）、channels（index、text、words）。
// @Description ChatGPT 模型 chatgpt-transcribe：只接受上传的 file（不超过 25 MB，不支持 url），只返回文本（无 words、language、duration）。
// @Tags Audio
// @Security BearerAuth
// @Accept mpfd,json
// @Produce json
// @Param file formData file false "音频文件"
// @Param url formData string false "音频 URL"
// @Param model formData string false "模型" default(grok-stt)
// @Param language formData string false "语言"
// @Param audio_format formData string false "音频格式"
// @Param sample_rate formData string false "采样率；也接受 sample_rate_hertz"
// @Param format formData boolean false "透传 Console STT 的 format"
// @Param multichannel formData boolean false "多声道"
// @Param channels formData integer false "声道数"
// @Param diarize formData boolean false "说话人分离"
// @Param filler_words formData boolean false "保留语气词"
// @Param keyterm formData []string false "关键词，可重复" collectionFormat(multi)
// @Param vad_threshold formData number false "VAD 阈值"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Failure 415 {object} map[string]any
// @Router /v1/stt [post]
func swaggerSTT() {}

// swaggerSTTWebSocket godoc
// @Summary 流式转写（WebSocket）
// @Description 需要 WebSocket Upgrade，普通请求返回 405；升级后在客户端与上游语音 WebSocket 之间双向转发消息。model 缺省为 grok-stt。
// @Tags Audio
// @Security BearerAuth
// @Param model query string false "模型" default(grok-stt)
// @Success 101 "切换到 WebSocket"
// @Failure 405 {object} map[string]any
// @Router /v1/stt [get]
func swaggerSTTWebSocket() {}

// swaggerRealtimeWebSocket godoc
// @Summary 实时语音（WebSocket）
// @Description 需要 WebSocket Upgrade，普通请求返回 400；升级后在客户端与上游语音 WebSocket 之间双向转发消息。model 缺省为 grok-voice-latest。
// @Tags Audio
// @Security BearerAuth
// @Param model query string false "模型" default(grok-voice-latest)
// @Success 101 "切换到 WebSocket"
// @Failure 400 {object} map[string]any
// @Router /v1/realtime [get]
func swaggerRealtimeWebSocket() {}
