# Provider 架构与 ChatGPT 接入

网关里的每个上游（Grok Build、Grok Web、Grok Console、ChatGPT）都是一个 Provider。它们共用同一套账号管理、客户端密钥、模型路由与价格、请求审计、媒体任务和图库。新增 Provider 时不需要另建一套。

> **状态**：ChatGPT 的协议代码只用模拟上游测过。OAuth、聊天、生图、蒙版、透明背景都要先用真实账号逐项验收，再对外开放。

## 架构

```
infra/provider.Registry
  ├─ Definition（能力、额度方式、凭据方式、展示名、出口作用域、默认并发、额度模式、是否走 xAI 官方价）
  └─ 适配器：cli(grok_build) web(grok_web) console(grok_console) chatgpt
        │
        ├─ GET /api/admin/v1/providers ── 前端 Provider 目录（账号页、密钥、模型、审计、仪表盘、出口设置）
        ├─ 网关：选路由 → 密钥范围 → 选账号 → 预留费用 → 转发 → 分类 → 结算 → 审计
        ├─ 价格簿：自定义价 → xAI 官方价（仅声明 OfficialPricing 的 Provider）→ 未定价
        └─ 媒体任务：图片 / 视频共用持久化任务（checkpoint、领取防重、幂等、结果不确定状态）
```

Provider 的能力由 `Definition` 声明，再由可选接口实现：对话、模型目录、凭据导入、续期、交互式登录、身份、额度、图片、异步图片、视频、语音。注册表启动时会交叉校验声明和实现是否一致。

### 新增一个 Provider

1. 在 `backend/internal/infra/provider/<name>` 实现 `Definition()` 和所声明能力对应的接口。
2. 在 `backend/internal/app/application.go` 里把适配器放进 `provider.NewRegistry(...)`。如果有运行时配置，在设置回调里调用它的 `UpdateConfig`。
3. 静态模型目录在启动时用 `UpsertRoutes` / `ReplaceProviderRoutes` 注册；远端目录会随账号模型同步自动出现。

账号页、密钥范围、出口作用域、价格、审计、仪表盘都从目录读取，不需要改动。

## ChatGPT 接入步骤

1. **添加账号**（账号页 → ChatGPT 标签）：
   - **浏览器 OAuth**：打开授权链接并登录，把浏览器地址栏里的完整回调 URL（`https://platform.openai.com/auth/callback?code=…&state=…`）粘贴回来。会话 10 分钟内有效。
   - **导入**：JSON、JSONL，或每行一个 access token。只有 refresh token 的记录会在导入时换取 token。导入时会查询身份和套餐（plan）。
   - **sub2api / CPA**：直接上传 sub2api 导出的账号文件（只取其中的 OpenAI OAuth 账号，其他平台跳过），或 CLIProxyAPI 的 `codex-*.json` 凭据文件（可多选）。这些 token 由 Codex CLI 的 OAuth 客户端签发，账号同样走 ChatGPT 网页接口。refresh token 只交回签发它的客户端续期：客户端从 access token 的 `client_id` 或 ID token 的 `aud` 读出，读不到时按这两个工具的默认值视为 Codex 客户端。
2. **设置**（设置 → ChatGPT）：
   - **图片会话模型**：留空即可。留空时按账号自动选择 ChatGPT 会话模型：`gpt-image-2` 用 `gpt-5-3`；`gpt-image-2.5` 系列优先用 `gpt-6`，不可用时用 `auto`。只有在 `/backend-api/models` 里开启了生图工具（`image_gen_tool_enabled`）的模型才会被选用。填写后所有图片模型都改用该模型。
   - **输出上限**：默认 16,384。
   - **单图 Token**：默认 256。
   - **默认图片模型**：Responses 图片工具没写模型时使用，留空为 `gpt-image-2.5`。
   - **Cloudflare Clearance**：默认关闭。
3. **模型**：账号模型同步后，聊天模型自动出现为路由（来自 ChatGPT 的 `/backend-api/models`）。图片路由在启动时注册：`gpt-image-2`、`gpt-image-2.5`、`gpt-image-2.5-flare`、`gpt-image-2.5-sunburst`，各有生图和编辑两条。ChatGPT 网页端没有图片模型列表，这几个名字是兼容官方 API 的别名。账号里有可用的会话模型时，对应的图片模型才可用：`gpt-image-2` 需要 `gpt-5-3`，2.5 系列需要 `gpt-6` 或 `auto`。Free 账号通常没有 `gpt-5-3`，所以只能用 2.5 系列。

   ChatGPT 网页端自己决定用哪个图片引擎（目前为 Images 2.5），请求里无法指定 Flare 或 Sunburst。所以 `-flare`、`-sunburst` 只是兼容官方 API 的名称，出图效果与 `gpt-image-2.5` 相同；`quality` 只支持 `auto/low/medium/high`，不支持官方 API 的 `xhigh`、`max`。
4. **价格**（模型页 → 设置价格）：ChatGPT 没有官方价。未设置价格时按 0 计费，模型页会标"未定价"。
5. **密钥**：范围为"全部"的密钥自动包含 ChatGPT 和以后新增的 Provider。ChatGPT 不受密钥的档位限制。

新账号每个默认并发为 1。额度按 `reason`、`image_gen`、`file_upload` 三个窗口显示，带上游返回的重置时间（`reset_after`）：每次请求后刷新对应窗口，用完的窗口到重置时间自动恢复，空闲账号每 6 小时同步一次（其他渠道仍为 30 分钟）。

访问令牌有效期约 7 天，网关在过期前 1 天续期（剩余有效期不足 2 天时取剩余时间的一半），续期失败还有一天可以重试；其他渠道仍在过期前几分钟续期。

## 出口代理与 FlareSolverr

- **出口节点**：ChatGPT 使用作用域 `chatgpt` 的出口节点。支持 HTTP(S)、SOCKS4/5 和 Trojan/VLESS/SS/VMess 隧道，节点可以在内网或本机。账号可以手动绑定节点，也可以自动分配。聊天、上传下载和 OAuth 续期都走同一个节点。
- **故障处理**：连接失败的节点进入冷却。请求在到达上游之前失败时，会立即换一个出口重试一次；已经发出的请求不会重放。
- **备用出口**：在"设置 → 出口运维"里为 `chatgpt` 配置（不回退 / 直连 / 指定节点）。
- **Cloudflare 挑战**：识别为 403（`cf-mitigated: challenge` 或挑战页面），按出口问题处理，换出口重试。
- **FlareSolverr**：打开 ChatGPT 设置里的 Cloudflare Clearance 后，遇到挑战会通过已配置的 FlareSolverr 按需求解，之后请求使用求解器给出的浏览器 UA、TLS 指纹和 cookie。

## 对外接口（ChatGPT 路由）

| 接口 | 说明 |
|---|---|
| `POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/responses` | 由网关统一转换，ChatGPT 侧以 Responses 为主协议 |
| `POST /v1/images/generations`、`POST /v1/images/edits` | 同步出图；编辑支持 JSON 或 multipart、多参考图、PNG 蒙版 |
| `POST /v1/responses`（带 `image_generation` 工具） | 同步、`background: true` 异步或 `stream: true` 流式 |
| `GET /v1/responses/{id}`、`POST /v1/responses/{id}/cancel` | 查询或取消异步图片响应（id 以 `resp_img_` 开头） |
| `POST /v1/audio/speech`、`POST /v1/tts`、`GET /v1/tts/voices` | 语音合成（`chatgpt-tts`）和账号音色列表 |
| `POST /v1/audio/transcriptions`、`POST /v1/stt` | 语音识别（`chatgpt-transcribe`） |

### 聊天

- 不支持的参数会被忽略。
- `max_tokens` / `max_completion_tokens` / `max_output_tokens` 按"输出上限"截断；达到上限时 Responses 状态为 `incomplete`。
- `reasoning.effort` / `reasoning_effort` 映射到上游的推理强度。
- Responses 流式按官方事件序列输出，带 `sequence_number`；失败时发送 `response.failed`。
- 用量为本地 `o200k_base` 估算，包含推理 token、工具定义、模型写出的调用标记和图片输入的固定 token，审计里标记为估算。
- 每个请求最前面加一条系统指令，要求 ChatGPT 只输出纯 Markdown、不写网页端的 UI 组件（`<box>`、`<row>`、天气卡片等），`<Cite/>` 引用除外；客户端的 `instructions` / 系统消息排在它后面。仍漏出的 UI 标记由网关过滤。

#### 函数工具（提示词模拟）

ChatGPT 网页端不能执行客户端函数，网关用和 Grok Web 相同的约定（`toolemu` 包）模拟：

- 声明 `function` 工具且 `tool_choice` 不是 `none` 时，工具定义和 XML 调用格式作为系统消息发给 ChatGPT；`required`、指定函数、`parallel_tool_calls: false` 会写进提示词，`parallel_tool_calls: false` 时只保留第一个调用。提示词还说明工具在客户端执行、调用块必须写在可见回复里（否则模型会把调用发到自己的 `functions.exec` / `container.exec` 通道而失败）；没有声明搜索时，要求不使用内置搜索、天气等工具。
- 回复里的调用块被解析为 Responses `function_call`（`status: completed`，唯一 `call_id`，参数为 JSON 字符串），排在文字之后；Chat 得到 `tool_calls` / `finish_reason: tool_calls`，Messages 得到 `tool_use` / `stop_reason: tool_use`。调用块前后的文字照常作为消息输出。
- 流式时调用块在完整解析前不会下发，客户端看不到 XML。调用块先于网页 UI 标记改写被取出，参数里的 `<title>` 等文本保持原样。
- 历史里的 `function_call` 按同一格式回放（同一轮的多个调用合并成一个块），`function_call_output` 作为工具结果消息发送，多轮 agent 循环可以继续。ChatGPT 会在流里回显请求消息，网关按请求消息 ID 跳过这些回显，历史调用不会被当成新回复。
- 只接受请求中声明过的函数名；未声明的名称或无法解析的块按普通文本返回。`tool_choice: none` 时不发约定，模型被提示直接回答。
- ChatGPT 无法执行的工具（`code_interpreter`、`file_search`、`computer_use_preview`、`custom`、`mcp` 等）不报错，模型被提示这些工具不可用。
- 限制：调用依赖模型遵守格式，可能直接回答或写出无效块；询问天气、新闻等实时信息时，ChatGPT 服务端的自动搜索（`server_ste_metadata.tool_name: SonicBrowserTool`）会在模型运行前触发，请求里没有关闭它的字段，模型可能直接用搜索结果回答而不调用函数；`custom` 自由格式工具（如 Codex 的 `apply_patch`）没有模拟；工具定义计入输入，单个 `description` 不超过 16 KB，最多 128 个工具。

#### 联网搜索

- 接受 Responses 的 `web_search` / `web_search_preview`、Chat 的 `web_search_options`、Messages 的 `web_search_*`，不再提示"工具不可用"。搜索由 ChatGPT 在服务端完成。
- `tool_choice` 强制搜索（`{"type":"web_search_preview"}`、只声明了搜索时的 `required`、Messages 指定 `web_search`）时，会话请求带 `system_hints: ["search"]`，与网页端搜索按钮、生图按钮的 `picture_v2` 同一机制。
- 回复用到搜索（`server_ste_metadata.search_tool_call_count`、消息元数据里的 `search_model_queries` / `search_queries` / `search_result_groups` / `content_references`）时，输出一个 `web_search_call`（`status: completed`，`action.query` 为第一条查询，`action.sources` 为结果链接），能在回答开始前确定时排在消息之前。
- 正文里的引用组件 `<Cite refs={["turn0search8"]}/>` 按搜索结果的 `ref_id`（`turn{turn_index}{ref_type}{ref_index}`）换成 `([站点](链接))`；旧格式引用标记（`U+E200 … U+E201`）换成 `content_references[].alt` 里 ChatGPT 自带的 Markdown 链接。两者都按替换后的文字生成 `url_citation`（字符偏移）；找不到对应结果的 `<Cite/>` 随网页 UI 标记删除，旧标记原样保留，不猜测位置。`url_moderation` 标为不安全的链接不会出现在来源和正文里。
- Chat 客户端在 `annotations` 里看到引用；Messages 客户端看到 `server_tool_use` / `web_search_tool_result`。
- 已用免费账号实测（`gpt-6-mini`）：查询词、来源、`<Cite/>` 引用和 `url_citation` 正确。限制：旧格式引用只在其 `content_references` 与标记同时到达时替换；`citations` / `safe_urls` 不参与映射。

### 图片

- **编辑输入**：`image`、`images`、`image_url`、`mask` 接受以下形式：
  - data URI、https URL、裸 base64；
  - `{"image_url": …}`、`{"url": …}`、`{"b64_json": …}` 及其数组；
  - multipart 文件。

  `file_id` 会被明确拒绝。
- **返回格式**：默认返回 `b64_json`；需要链接时传 `response_format: "url"`。
- **参数**：支持 `background`（含 `transparent`）和 `output_format`。透明背景不能输出 jpeg。
- **参数校验**：按 Provider 进行；被拒绝的路由会换同名的其他 Provider 路由。
- **尺寸**：网页端不理会请求里的尺寸字段。网关把尺寸和横竖比例写进提示词，所以画面形状会按请求生成。像素面积固定在约 157 万，例如请求 `1024x1024` 会返回约 `1254x1254`。网关不在本地缩放。
- **多张（`n` 为 2–10）**：网页端一次只画一张。非流式请求会拆成单张请求，同时分给空闲账号生成；没有空闲账号的图等前一轮结束后接着画。账号少时，总耗时不会比逐张生成更长。
  - 每张图单独记一条审计，在生成它的账号上计费，审计中的请求 ID 都相同。
  - 遇到第一次失败就停止，返回已生成的图片。
  - 流式只支持 `n=1`。

### Codex 生图

- **模型**：`codex-gpt-image-2`、`codex-gpt-image-2.5`、`codex-gpt-image-2.5-flare`、`codex-gpt-image-2.5-sunburst`，各有生图和编辑两条。请求走 Codex 后端的 Images API（`chatgpt.com/backend-api/codex/images/generations`、`/images/edits`），上游模型名是去掉 `codex-` 前缀后的名字，计费也按它。
- **账号**：只调度到 Plus、Pro、Team、Business、Enterprise、Edu 套餐的账号，Free 账号会被上游拒绝（403）。凭据类型不限，浏览器 OAuth 登录的 Platform 账号也可以。
- **额度**：不占网页的"生图"次数，按 Codex 的 5 小时和每周用量计算（百分比）。账号页显示这两个窗口；两者中剩余较少的那个决定账号能否接 Codex 生图请求，用满后等到重置。
- **参数**：`size`、`quality`、`background`、`output_format` 原样传给上游；编辑的输入图和遮罩以 data URL 发送，遮罩沿用 Images API 的透明区域约定。实测请求 `1024x1024` 时返回过 `1254x1254` 和 `1312x1199`，上游并不保证精确尺寸。
- **限制**：只支持同步的 Images API，不支持异步生图任务。

### 异步生图（Responses 官方格式）

```bash
curl https://<host>/v1/responses -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" -d '{
  "model": "gpt-image-2",
  "background": true,
  "input": "a red fox in the snow",
  "tools": [{"type": "image_generation", "size": "1024x1024", "quality": "high",
             "background": "transparent", "output_format": "png"}]
}'
# => {"id": "resp_img_…", "object": "response", "status": "queued", …}

curl https://<host>/v1/responses/resp_img_… -H "Authorization: Bearer $KEY"
# status: queued → in_progress → completed | failed | cancelled
```

- **路由**：先用工具里的 `model`，没有时用顶层 `model`；顶层是 ChatGPT 聊天模型时，回退到"默认图片模型"。
- **编辑**：在 `input` 消息里放 `{"type": "input_image", "image_url": …}`。蒙版写在工具的 `input_image_mask.image_url`。
- **幂等**：支持 `Idempotency-Key` 请求头。同一密钥、同一内容返回同一任务；内容不同返回 409。
- **取消**：只对尚未提交到上游的任务有效，已提交的返回 `409 response_not_cancellable`。
- **结果不确定**：`error.code = result_uncertain` 表示上游可能已经出图但无法确认。系统不会自动重试；管理员可以在"媒体任务"页手动重试，可能再次消耗上游额度。

### 语音

两个模型由 ChatGPT 目录注册，模型同步后出现在所有 ChatGPT 账号上。

| 模型 | 上游 | 额度 | 说明 |
|---|---|---|---|
| `chatgpt-transcribe` | 输入框听写 `POST /backend-api/transcribe` | 不占对话额度 | 只返回文字，没有时间戳、逐词结果和语种；只接受上传文件（≤ 25 MB），不接受 URL；`language` 取主语言代码（`zh-CN`、`zh_CN` → `zh`）作为识别提示转给上游，上游只当软提示，无效代码不报错；文件名没有音频扩展名时（如浏览器录音的 `blob`）按声明类型或文件内容补上，否则上游返回 500 |
| `chatgpt-tts` | 朗读 `GET /backend-api/synthesize` | 不占 `reason`（账号没有 `gpt-5-3-mini` 时占 1 次） | 输出 `mp3` / `aac` / `opus`；`wav` / `flac` / `pcm` 返回 400；`speed`、`language` 被忽略；`input` ≤ 4096 字符 |

- **音色**：`voice` 按账号音色的 ID 或显示名匹配，如 `glimmer` 或 `Sol`；`/v1/tts/voices` 列出账号可用的音色。OpenAI 音色名（`alloy` 等）和未知名称使用账号当前选中的音色。
- **语言**：音色都能说多种语言，中文实测可用（转写回来与原文一致）。按 `spoken_language` 查询，中文、日文返回的仍是同样 9 个音色，没有中文专属音色；网页端在中文环境下默认用 `cove`。
- **合成流程**：网页端所有出声接口（`/synthesize`、`/conversation/{id}/audio_summary/synthesize`、`/generate_audio_paragen`）都绑定对话消息，没有独立的文本转语音接口；朗读只能播放已保存对话里的消息，临时对话会返回 `404 conversation_inaccessible`。网关新建一个保留历史的对话，把要朗读的文字作为一条 assistant 消息写进去，由 `gpt-5-3-mini` 回答收尾（实测不扣 `reason`，`auto` 会扣 1 次）；上游回显这条消息后断开流，按消息 ID 合成，再把对话从历史中隐藏（`is_visible: false`）。文字会以对话形式保存在上游账号里，隐藏失败时日志记录 `chatgpt_speech_conversation_hide_failed`。
- **失败处理**：听写和朗读服务所有账号共用，上游 5xx 直接返回 502 和上游原因，不换账号重试；遇到 Cloudflare 质询或限流仍会换出口或账号。其他语音请求换完所有账号仍失败时，返回最后一次上游错误，而不是"没有可用账号"。音色列表按账号缓存 10 分钟。
- **实测**（免费账号）：识别约 0.9 秒；合成约 9.5 秒，得到 6.3 秒的 mp3，再识别回来与原文一致。
- **实时语音**：网页端的 GPT-Live 走 WebRTC（向 `/realtime/vp` 提交 SDP），不在模型列表里，网关未接入。

## 媒体任务与图库

- **持久化**：图片和视频任务都写入 `media_jobs`。领取任务时会递增代次，旧 worker 无法写回结果。
- **崩溃恢复**：从 checkpoint 继续轮询，不会重复提交上游。Console 视频保存了上游 request id。
- **图片任务**：n 张图在同一账号上依次生成。租约 2 分钟、每 30 秒续租，任务期限 1 小时。每交付一张图，按下单时锁定的价格记一条审计。
- **图库来源**：每个资产记录生成它的 Provider、模型、密钥、账号、操作、任务和 prompt，管理端可以按这些条件筛选。
- **删除保护**：活动任务正在使用的资产不能删除，返回 409 `mediaAssetInUse`。

## 计费

| 项目 | 规则 |
|---|---|
| 价格来源 | 管理员自定义价优先；xAI 官方价只用于 Grok Provider；都没有时按 0 计费并标记未定价 |
| 价格版本 | 自定义价只追加不覆盖，审计记录所用的价格行，明细按当时的版本重建 |
| ChatGPT 聊天 | 本地估算的输入、输出 token × 自定义的每百万 token 价格 |
| 图片 | 只对成功交付的图片计费；失败和结果不确定的不收费 |

## 从扩展渠道版本升级

旧的"扩展渠道"子系统（渠道中心、`channel_*` 表、`/api/admin/v1/channels/*`）已删除。升级时会自动：

- 删除 `channel_*` 表。
- 删除旧渠道导入的 ChatGPT 账号（`auth_type = channel_v1`）。
- 删除旧渠道创建的路由。

ChatGPT 账号需要在账号页重新添加，价格需要在模型页重新设置。原有密钥范围为"全部"的，自动包含 ChatGPT。
