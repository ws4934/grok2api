# Provider 架构与 ChatGPT 接入

网关里的每个上游（Grok Build、Grok Web、Grok Console、ChatGPT、Adobe、Muse）都是一个 Provider。它们共用同一套账号管理、客户端密钥、模型路由与价格、请求审计、媒体任务和图库。新增 Provider 时不需要另建一套。

> **状态**：ChatGPT 的协议代码只用模拟上游测过。OAuth、聊天、生图、蒙版、透明背景都要先用真实账号逐项验收，再对外开放。

## 架构

```
infra/provider.Registry
  ├─ Definition（能力、额度方式、凭据方式、展示名、出口作用域、默认并发、额度模式、是否走 xAI 官方价）
  └─ 适配器：cli(grok_build) web(grok_web) console(grok_console) chatgpt adobe muse
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

## 在请求详情中核对参数

管理端「请求记录 → 请求诊断详情 → 请求信息」提供三类参数，可直接复制：

- **客户端参数**：协议转换前的请求参数，包括提示词、模型、数量和尺寸等。
- **网关生效参数**：网关归一化后交给适配器的参数。跨账号分批时，客户端仍显示原始 `n`，每条批次记录显示自己的实际 `n`；这不表示所有原生引擎都支持同名参数。
- **上游请求摘要**：目前记录 Imagine WebSocket 和 Console 图片接口构造的原生请求。是否提交成功、上游如何响应，结合「上游诊断」判断。

凭据字段及 URL 的账号、查询参数和片段会脱敏，Base64 和媒体内容不保存；multipart 文件只记文件名、类型和大小。每份快照最多 16 KiB，单个字符串还有限长，截断时会明确标记。快照只在详情读取，随审计记录清理；异步图片和视频任务会持久化创建时的参数，恢复执行或结算后仍可查询。启用前的旧记录没有参数可回填。

## 客户端错误协议

- 客户端错误的 `message` 使用中文；HTTP 状态、`type`、`code`、`param` 和 SSE 事件名按对应 API 协议返回，不把机器字段翻译成中文。已符合协议的上游错误对象保留其机器字段。
- 同步接口中的内容审核拒绝保留实际的 400／403／422 状态，不改成 502；网关生成的 OpenAI 风格错误使用 `content_policy_violation`。审核拒绝不自动重新生成、不切换账号，也不计入账号故障。
- Grok Web Imagine 全批结果被标记为 `moderated: true` 时，同步 Images 请求映射为 400 `content_policy_violation`；流式保留已发送的 200，通过 `error` 事件报告审核拒绝，不空断流。诊断保留 WebSocket 地址及拦截数量，不保存被拦截的图片内容；没有已交付图片时不扣客户端费用。
- Responses 图片任务通过 `status: "failed"` 和 `error` 返回失败；审核拒绝使用 `image_content_policy_violation`，无效输入使用 `invalid_prompt`，其他生成故障使用 `server_error`。流式请求通过 `response.failed` 或对应协议的 `error` 事件报告，开始输出后不另写 HTTP 错误正文。
- xAI 视频任务轮询仍返回 `status: "failed"` 和 `error: {code, message}`，审核拒绝使用 `invalid_argument`，不将内部错误码直接放入该字段。
- 管理端上游诊断展示中文错误说明，并保留脱敏后的原始响应、错误详情和错误链，便于排查。
- 客户端 `error.message` 只写原因和必要的处理方法，不附带 Grok Web、Grok Console 等渠道标签；状态码、协议错误类型、机器码和参数名不变，渠道与账号信息留在管理端诊断。

协议依据：[OpenAI Response error](https://developers.openai.com/api/reference/resources/responses/methods/cancel)、[xAI Video Error Handling](https://docs.x.ai/developers/model-capabilities/video/generation#error-handling)。

## Grok 图片：同一入口，按实际路由提供参数

客户端继续使用 `/v1/images/generations` 和 `/v1/images/edits`。创作台按实际 Provider 和上游模型决定可选参数，不依赖管理员可修改的公开模型名。同名模型跨多个路由时，只展示这些路由都支持的 Grok 参数；发送请求和新历史记录使用同一组有效值。

- **Console 生图**：可选 `1k/2k` 和 9 种宽高比；只有 `grok-imagine-image-2.0` 展示 `auto/low/medium` 质量选项。2.0 的 `auto` / 省略质量在生图时使用 `low`、编辑时使用 `medium`；`high` 对齐 `medium`，超出支持档位的分辨率就近适配。归一化在账号选择及费用预占前完成，转发、审计与结算用同一组有效参数。编辑保留前 3 张输入图，输入图费用也按这 3 张计算。
- **Web 2.0**：`grok-imagine-image-2.0` 同时注册生图和编辑能力，不带输入图调用 `/v1/images/generations`，带图调用 `/v1/images/edits`；编辑合计最多 5 张输入图，每个上游批次出 1 张，客户端可用 `n` 请求多张，由网关分批。编辑沿用官网的 `modelName=imagine-image-edit`，通过 `responseMetadata.modelConfigOverride.modelMap.imageEditModel=imagine` 选择当前引擎，而不是把 API 模型名硬塞给私有接口。宽高比支持 `auto`、1:1、16:9、9:16、4:3、3:2、2:3、21:9、5:2，其他值就近适配。
- **免费号路径**：当前 Web 接入按 1K 和模型自身质量运行；官网的 2K 字段 `resolution_name/resolutionName` 受账号功能开关控制，本次不发送它，也不将 API 的 `low/medium/auto` 当作 Web 质量档。创作台不展示无效选项，Web 生图和编辑计费也不再因被忽略的 `resolution/quality` 加价。Basic 编辑使用 `image_pro` 额度；付费号仍使用 `image_edit`，与同名文生图的额度分开。
- **其他 Web 入口**：`grok-imagine-image` 保留原有 13 种比例，Lite 仅展示数量；已有 `grok-imagine-image-edit` 编辑入口继续可用，保留原有最多 8 张输入图的调用契约。
- **创作台参考图**：在同一图片面板粘贴 HTTPS 图片地址，每行一张，自动切换生成/编辑接口；参考图和新历史记录一起保存，重新生成仍是编辑。切换模型不清空输入，若新模型不支持或超出数量则提示移除，不静默丢图。切换模型保留各控件选择，但请求与新记录只包含当前有效参数。API 仍支持原有 JSON、Base64 和 multipart 文件输入。
- **其他 Provider**：既有 OpenAI 风格的 `size/quality` 控件和调用方式不变。

### NSFW 开关的生效范围

设置中的「Imagine 文生图 NSFW 偏好」（`cfg.Provider.Web.AllowNSFW`）控制 Imagine WebSocket 请求里的 `enable_nsfw`，更新设置后热生效。它仅用于 Imagine 文生图，不用于 Lite、图片编辑、视频或 Console。账号页的「启用 NSFW」是另一项操作，修改账号的 `always_show_nsfw_content` 偏好。

已保存的运行设置优先于 YAML；只改配置文件时，要同时核对管理端当前设置。用新请求详情中的「上游请求摘要」查看实际构造的 `enable_nsfw`。即使为 `true`，上游仍执行审核，返回 `moderated: true` 的图片仍按审核拒绝处理。

### 多张生成保留成功结果，按有效额度分批

- 同步 Images 接口的 `n` 支持 1–10。请求 N 张只成功 K 张（K>0）时，仍返回标准 `200` / `data`，其中只含成功的 K 张，客户端费用按 K 张结算；一张都没有时才返回协议错误。`url` 和 `b64_json` 规则相同，创作台显示实际数量 K / N。
- 原生支持批量的模型，按账号已知余量提交可负担的最大批次，再把尚未提交的数量交给后续账号。Web / ChatGPT 次数型窗口按张数；Console 的共享窗口单位是美分，按实际模型、质量、分辨率和每批参考图成本计算，不能把 8 美分理解为 8 张。可计量窗口尚未同步时先逐张提交；无法换算成张数的周用量窗口仍遵守原有选择规则。
- 单张模型保留空闲账号间的并发分轮；编辑原生只出一张时也能聚合为客户端请求的 N 张。共享预算的在途占用不随路由缓存刷新而丢失，上游完成后先记实际额度消耗、释放账号，客户端费用等交付结果后结算。
- 某个已提交批次少出图、审核拒绝、断流或结果不确定时，不自动补发缺失结果；之前成功的图片保留。上游已生成却下载失败的图片可能仍消耗账号额度，但不计入客户端的成功图片费用。流式仍固定 `n=1`。

依据：[Imagine Image 2.0](https://x.ai/news/grok-imagine-image-2) 和 2026-10-28 登录后的官网前端 [请求构造及功能开关](https://cdn.grok.com/_next/static/chunks/01ovajz11efdy.js)。本次免费账号页面未开放 2K；尚未用付费账号验证其分辨率能力。

## Console 视频优先分配合适的周预算

大额视频优先使用本周尚未消费的账号。例如，一个预估 $3.75 的视频遇到 $1 周预算时，优先选剩余 $1 的账号，而不是先尝试已用 $0.06、剩余 $0.94 的账号。

- 选择依据是有效视频参数对应的原生预估美分，与客户端自定义售价分开。能覆盖消耗的账号优先；消耗超过整个周预算时，完整预算账号优先。没有完整预算或尚未同步额度的账号仍可作为候选，不把价格估算当作上游准入规则。
- 未提交任务在执行前重新选账号，避免入队后余额已被其他请求消费；实际提交前原子预留本地共享预算，图片和视频不能重复占用它。普通与大池分段选号使用同一偏好，密钥范围、模型绑定和账号状态仍然生效。
- 生成成功后按原生预估消耗暂扣 Console 的共享窗口，并排队获取官方额度覆盖本地估算，不再把一条视频只当作 1 美分。上游已生成但本地保存失败时仍记账号消耗，客户端不收取未交付的视频费用。
- 创建阶段明确返回 402 时仍可换账号；已经提交的任务始终在原账号继续轮询，不因预算偏好重新生成。已接受或提交结果不确定的请求暂记原生预算占用，直到官方额度同步校正。

## ChatGPT 接入步骤

1. **添加账号**（账号页 → ChatGPT 标签）：
   - **浏览器 OAuth**：打开授权链接并登录，把浏览器地址栏里的完整回调 URL（`https://platform.openai.com/auth/callback?code=…&state=…`）粘贴回来。会话 10 分钟内有效。
   - **导入**：JSON、JSONL，或每行一个 access token。只有 refresh token 的记录会在导入时换取 token。导入时会查询身份和套餐（plan）。
   - **sub2api / CPA**：直接上传 sub2api 导出的账号文件（只取其中的 OpenAI OAuth 账号，其他平台跳过），或 CLIProxyAPI 的 `codex-*.json` 凭据文件（可多选）。这些 token 由 Codex CLI 的 OAuth 客户端签发，账号同样走 ChatGPT 网页接口。refresh token 只交回签发它的客户端续期：客户端以 access token 的 `client_id`（或 ID token 的 `aud`）为准，文件里写的标记与之不符时以 token 为准；读不到时按这两个工具的默认值视为 Codex 客户端。CPA 文件夹里其他服务（Claude、Gemini、Kimi 等）的文件和没有 token 的记录会被跳过。
   - **同一份凭据不要两处同时使用**：refresh token 每次续期都会换新、旧的立即失效。在本服务续期后，留在 sub2api 或 CPA 里的那份就不能再续期了，反之亦然。
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

### 额度耗尽时自动换号

- 已知当前额度窗口为 0 的账号在选路时跳过；上游首次报告耗尽、且尚未接收生成任务时，网关在同一次客户端请求内换到可用账号，遵守密钥的账号范围和最大尝试次数。
- 聊天和图片 SSE 中的明确额度错误按 429 处理，不当成普通 400／422／502。流式生图确认上游接收后才开始下发响应，避免先返回 200、再把原本可换号解决的限额错误发给客户端。
- 已无可用账号或达到配置的尝试上限后，才返回限流错误：同步请求保留 429；Responses 图片任务使用 `error.code = "rate_limit_exceeded"`。`Retry-After` 和 SSE 中的等待时长用于冷却与额度恢复。
- 临时限流只做冷却，不增加账号故障次数；已接受、已开始输出或结果不确定的生成不自动重新提交，以免重复消耗额度。

### 聊天

- 不支持的参数会被忽略。
- `max_tokens` / `max_completion_tokens` / `max_output_tokens` 按"输出上限"截断；达到上限时 Responses 状态为 `incomplete`。
- `reasoning.effort` / `reasoning_effort` 换成网页端的思考档位：`minimal` / `low` → `min`，`medium` → `standard`，`high` / `xhigh` → `extended`；也可以直接传 `min`、`standard`、`extended`、`max`。网页端不认 `low` / `medium` / `high`，原样发送会被拒绝（422）。
- Responses 流式按官方事件序列输出，带 `sequence_number`；失败时发送 `response.failed`。
- 用量为本地 `o200k_base` 估算，包含推理 token、工具定义、模型写出的调用标记和图片输入的固定 token，审计里标记为估算；续接会话时输入仍按完整历史计算。
- 每个请求最前面加一条系统指令，要求 ChatGPT 只输出纯 Markdown、不写网页端的 UI 组件（`<box>`、`<row>`、天气卡片等），`<Cite/>` 引用除外；客户端的 `instructions` / 系统消息排在它后面。仍漏出的 UI 标记由网关过滤。

#### 函数工具（提示词模拟）

ChatGPT 网页端不能执行客户端函数，网关用和 Grok Web 相同的约定（`toolemu` 包）模拟：

- 声明 `function` 工具且 `tool_choice` 不是 `none` 时，工具定义和 XML 调用格式作为系统消息发给 ChatGPT；`required`、指定函数、`parallel_tool_calls: false` 会写进提示词，`parallel_tool_calls: false` 时只保留第一个调用。调用块写在回复末尾，前面可以有一两句说明，后面不能再有文字；回复里说要调用工具，就必须在同一条回复里写出调用块。提示词还说明工具在客户端执行、调用块必须写在可见回复里（否则模型会把调用发到自己的 `functions.exec` / `container.exec` 通道而失败）；没有声明搜索时，要求不使用内置搜索、天气等工具。
- 模型仍把调用发到 `functions.*` 通道、回复里却没有调用块时（这类调用的参数不会出现在流里），网关在同一会话里追加一条系统消息追问一次，只取这次回答里的调用，不输出它的文字。
- JSON 参数完整、但漏写 `</parameters>` 直接接 `</tool_call>` 的调用块也能解析。
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

#### 会话续接

agent 客户端每一步都带上完整历史。网关不再每次都把整份历史发到新会话：

- 每次回复后记下这段对话在 ChatGPT 里结束的位置（会话 ID 和最后一条消息），键为"本次历史 + 客户端将回传的这次回复"。
- 下一个请求在这段历史后面追加消息（工具结果、用户的新消息），并且落在同一个账号上时，只把新增的消息接到同一个会话里发送，之前的图片也不再重新上传。第一次请求、历史被改写或压缩、换了账号、记录过期时，照旧把整份历史发到新会话。
- 同一会话通过网关的会话身份（客户端密钥 + `prompt_cache_key` 或 Claude Code、Codex 的会话头，没有时用系统提示和第一条用户消息）尽量固定在同一个账号。记录保存在内存里，1 小时过期，重启后从整份历史开始。
- ChatGPT 不再接受续接（如临时会话已失效，返回 404）时自动改发整份历史。
- 限制：ChatGPT 网页端单次请求约 120 KB，超过时返回 400 `context_length_exceeded`；续接后每次只发新增消息，不再撞上这个上限。上下文长度仍由 ChatGPT 决定：免费账号的 `gpt-6` 实测超过约 2.5 万 token 后，会从最早的对话开始丢弃（系统消息保留），网关看不到也无法阻止，客户端应按这个窗口自行压缩历史。

### 图片

- **编辑输入**：`image`、`images`、`image_url`、`mask` 接受以下形式：
  - data URI、https URL、裸 base64；
  - `{"image_url": …}`、`{"url": …}`、`{"b64_json": …}` 及其数组；
  - multipart 文件。

  `file_id` 会被明确拒绝。
- **返回格式**：默认返回 `b64_json`；需要链接时传 `response_format: "url"`。
- **参数**：支持 `background`（含 `transparent`）和 `output_format`（`jpg` 按 `jpeg`）。透明背景与 jpeg 同时给出时输出 jpeg、放弃透明。
- **透明背景**：网页端没有背景字段，网关在提示词里要求透明，出图后逐像素检查。图里没有透明像素（常见于满画面的场景）时返回 502 `media_postprocessing_failed`，消息写明原因；这张图已在 ChatGPT 生成，次数照样消耗。场景图请用 `background: "auto"`。
- **参数适配**：为其他接口写的参数不报错：`aspect_ratio` 配合 `resolution` 换成 `size`（如 16:9 + 2k → `2048x1152`，只给比例时按 1k），`size` 写成比例时按 `aspect_ratio` 处理，超出 256–4096 的尺寸按比例缩放到范围内；`quality` 的 `hd` / `standard` 对应 `high` / `medium`，`1k` / `2k` 等档位按分辨率处理；认不出的值忽略。`n` 超出 1–10 时取最近的值（流式固定为 1），`partial_images` 限在 0–3、只在流式时生效；编辑的输入图超过 8 张、蒙版超过 1 张时保留前面的；`storage_options` 等用不到的字段忽略。仍会拒绝的只有做不到的请求（没有输入图的编辑、Provider 不支持的蒙版或流式、`response_format` 写错），被拒绝的路由会换同名的其他 Provider 路由。
- **同名模型**：Adobe 也有 `gpt-image-2`、`gpt-image-2.5-flare`、`gpt-image-2.5-sunburst`；两边都有账号时同名请求可能落到任一方，用 `chatgpt/` 前缀可以固定走 ChatGPT。
- **尺寸**：网页端不理会请求里的尺寸字段。网关把尺寸和横竖比例写进提示词，所以画面形状会按请求生成。像素面积固定在约 157 万，例如请求 `1024x1024` 会返回约 `1254x1254`。网关不在本地缩放。
- **多张（`n` 为 2–10）**：网页端一次只画一张。非流式请求会拆成单张请求，同时分给空闲账号生成；没有空闲账号的图等前一轮结束后接着画。账号少时，总耗时不会比逐张生成更长。
  - 每张图单独记一条审计，在生成它的账号上计费，审计中的请求 ID 都相同。
  - 遇到第一次失败就停止，返回已生成的图片。
  - 流式只支持 `n=1`。

### Codex 生图

- **模型**：`codex-gpt-image-2`、`codex-gpt-image-2.5`、`codex-gpt-image-2.5-flare`、`codex-gpt-image-2.5-sunburst`，各有生图和编辑两条。请求走 Codex 后端的 Images API（`chatgpt.com/backend-api/codex/images/generations`、`/images/edits`），上游模型名是去掉 `codex-` 前缀后的名字，计费也按它。
- **账号**：只调度到 Plus、Pro、Team、Business、Enterprise、Edu 套餐的账号，Free 账号会被上游拒绝（403）。套餐优先取 access token 里的声明（续期后会更新），否则用导入时查到的套餐。凭据类型不限，浏览器 OAuth 登录的 Platform 账号也可以。
- **额度**：不占网页的"生图"次数，按 Codex 的 5 小时和每周用量计算（百分比）。账号页显示为一格"Codex"，悬停可看两个窗口；两者中剩余较少的那个决定账号能否接 Codex 生图请求，用满后等到重置。
  - Codex 用量读取失败（网络、5xx、429）时，本次额度同步整体失败、保留原有窗口，稍后重试；Codex 拒绝该账号（其他 4xx）或返回的套餐已不是付费版时，账号不再接 Codex 生图请求，直到下次同步。
- **失败处理**：上游 4xx 按未提交处理：401 先续期再重试，429 核对 Codex 额度后换账号，400 原样返回。已发出的生成请求在客户端断开后仍会完成并存入图库，单张最长等待 10 分钟。
- **参数**：`size`、`quality`、`background`、`output_format` 原样传给上游；编辑的输入图和遮罩以 data URL 发送，遮罩沿用 Images API 的透明区域约定。实测请求 `1024x1024` 时返回过 `1254x1254` 和 `1312x1199`，上游并不保证精确尺寸。
- **限制**：只支持同步的 Images API。用 Codex 模型创建异步生图任务会直接返回 400（`invalid_parameter`）。

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
- **参数**：顶层是图片模型或 `tool_choice` 强制图片工具时，Responses 的其他字段（`metadata`、`temperature` 等）和工具里用不到的选项（`output_compression`、`input_fidelity`、`moderation`、`partial_images`）忽略。聊天模型只是提供图片工具时，请求里有这些字段就仍走对话。
- **幂等**：支持 `Idempotency-Key` 请求头。同一密钥、同一内容返回同一任务；内容不同返回 409。
- **取消**：只对尚未提交到上游的任务有效，已提交的返回 `409 response_not_cancellable`。
- **结果不确定**：Responses 的 `error.code` 使用协议内的 `server_error`，中文消息说明结果尚未确认；管理端任务保留 `uncertain` 状态和内部码 `submission_uncertain`。系统不会自动重试；管理员可在“媒体任务”页手动重试，可能再次消耗上游额度。

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
  - 初始化仅更新已获授权的输入图数量约束：视频从旧上限 8 或 15 放宽到 20（首帧、尾帧、关键帧及参考图），图片仍最多 8 张。已有任务、图库和计费数据保留；当前约束不会在每次启动时重复重建。
- **崩溃恢复**：从 checkpoint 继续轮询，不会重复提交上游。Console 视频保存了上游 request id。
- **视频参数**：为其他视频接口写的参数不报错。
  - `seconds`、`size`（如 `1280x720` 换成 16:9 + 720p）、`quality`、`input_reference` 分别按 `duration`、`aspect_ratio`、`resolution`、`image` 处理；`image` 等素材也可以写成裸 URL 或 `{"image_url": …}`；`output`、`storage_options`、`seed` 等用不到的字段忽略；认不出的时长、比例、分辨率按没传处理，纯数字分辨率（`720`）按 `720p`。
  - 取值由路由的 Provider 对齐到最近的可用值。Grok（Build / Console / Web）：比例取 1:1、16:9、9:16、4:3、3:4、3:2、2:3 中最近的（21:9 → 16:9），时长 1–15 秒（默认 8），分辨率 480p / 720p。只有 Console 的 `grok-imagine-video-1.5` 在纯文本或仅首帧模式下有 1080p（2160p → 1080p）；带参考图、音色、尾帧或关键帧时最高 720p。编辑忽略这三项，延长的时长 2–10 秒（默认 6）。
  - 素材超出上限时保留前面的：参考图入口最多接收 14 张；Console 的 `grok-imagine-video-1.5` 支持 14 张参考图，并可叠加 1 张首帧、1 张尾帧、4 张关键帧（总计最多 20 张输入图）和最多 3 个参考音色，最长 15 秒、最高 720p。Console 旧版 `grok-imagine-video` 支持 7 张参考图、最长 10 秒，不支持参考音色（API 会忽略，创作台不允许选择），首帧与参考图互斥；Build 保持 8 张参考图和首帧互斥。互斥时保留首帧（也适用于 Veo、Seedance）。同名模型有多条路由时，优先选能收下全部素材的路由，例如 14 张参考图或首帧加参考图优先走 Console 1.5。
  - Console 1.5 的 `image` 固定开场，不会降为普通参考图；同时提供首帧时，提示词中的 `<IMAGE_0>` 指首帧，`reference_images` 从 `<IMAGE_1>` 开始编号；无首帧时从 `<IMAGE_0>` 开始。依据 [xAI Reference-to-Video](https://docs.x.ai/developers/model-capabilities/video/reference-to-video)。创作控制台按实际路由识别这些能力，支持批量上传、逐张移除和图片编号；切换到不兼容模型时提示移除冲突素材，不静默清空。
  - Console 1.5 的 `last_frame: {url 或 file_id}` 可单独使用；`keyframes` 最多 4 项，每项 `{image: {url 或 file_id}, timestamp_s: 秒}`，时间必须在 `(0, 实际 duration)` 内，且上游按 1/3 秒对齐后不能落在同一时间槽。有首帧、尾帧或关键帧时可不传 prompt。所有实际输入帧均计入输入图定价与原生额度成本，1080p 请求在这些参考模式下适配为 720p 后再计费。
  - Console 生成视频支持 `generate_audio: false` 静音、`true` 有声；省略时不注入参数，沿用上游有声默认。编辑、延长和未声明此能力的 Provider 会拒绝该字段，不静默忽略。创作台支持尾帧、四个带时间的关键帧及声音三态；历史可复用远程图片和声音选择，本地素材只保存名称，重载后明确要求重新上传，避免复用已释放的 file_id。
  - 仍会拒绝的是模型做不到的输入：Web 的图生视频、不支持的尾帧或参考视频、Provider 不支持的编辑或延长。
- **图片任务**：n 张图在同一账号上依次生成。租约 2 分钟、每 30 秒续租，任务期限 1 小时。每交付一张图，按下单时锁定的价格记一条审计。
- **图库来源**：每个资产记录生成它的 Provider、模型、密钥、账号、操作、任务和 prompt，管理端可以按这些条件筛选。
- **删除保护**：活动任务正在使用的资产不能删除，返回 409 `mediaAssetInUse`。
  - 终态任务的用量或费用尚未持久化时也不能删除，返回 409 `mediaJobUsagePending`，以保留补账依据。管理端显示“结算中”并继续刷新，结算完成后恢复删除；未提交就取消的视频无需结算。

## 计费

| 项目 | 规则 |
|---|---|
| 价格来源 | 管理员自定义价优先；xAI 官方价只用于 Grok Provider；都没有时按 0 计费并标记未定价 |
| 价格版本 | 自定义价只追加不覆盖，审计记录所用的价格行，明细按当时的版本重建 |
| ChatGPT 聊天 | 本地估算的输入、输出 token × 自定义的每百万 token 价格 |
| 图片 | 只对成功交付的图片计费；失败和结果不确定的不收费 |

## Adobe（Firefly）接入

Adobe 是**纯媒体 Provider**（图像生成/编辑 + 视频生成），上游是 `firefly-3p.ff.adobe.io` 的异步任务（submit → poll → 取结果 URL）。模型目录是一张手写表，请求体按官网前端的规则构建。图像走共享的 `AsyncImageAdapter`、视频走 `VideoAdapter`，持久任务、账号池、出口、定价、审计全部复用。

### 账号与凭据

- **导入**（账号页 → Adobe 标签）：登录 `https://firefly.adobe.com`，复制该站完整的浏览器 Cookie，每行一个粘贴；也支持 JSON/JSONL。导入时用 Cookie 换取 access token 验证，Cookie 存入 refresh token 字段。
- **续期**：access token（IMS JWT）约 1 小时过期，网关用 Cookie 调 `adobeid-na1.services.adobe.com/ims/check/v6/token` 自动续期；Cookie 失效（401/403）→ 账号转入需重新导入。没有注册新账号的能力——账号始终靠导入 Cookie。
- **浏览器身份**（与 ChatGPT 账号同理）：导入时给每个账号随机分一个 Chrome UA（146 / 144 × Windows / macOS，都是 tls-client 有对应指纹的版本），存进凭据状态。该账号的所有请求都用它：出口按这个 UA 选 TLS 指纹，`User-Agent` 与客户端提示也由它推出。重新导入沿用原身份；导出文件带 `user_agent` 字段，导回来不变。

### 全局 x-arp-session-id（mint sidecar）

- 提交 Adobe 任务需要一个全局共享、与账号无关的 `x-arp-session-id`，由 mint 容器（`docker/mint/`，`GET /mint`）提供。启动：

  ```bash
  docker compose --profile adobe up -d --build
  ```

- grok2api 默认指向 compose 的 `mint` 服务（`provider.adobe.sherlockMintAPI=http://mint:8777`），定期自动刷新并缓存；非 compose 部署需自行提供可达的 mint 地址。mint 地址和单次上游请求超时（`provider.adobe.timeout`，默认 60 秒）可在 **运行设置 → Adobe Firefly** 在线修改，立即生效；保存后以数据库中的值为准。公网 mint 地址只能用 HTTPS（443 端口）。地址框旁的「测试」按钮由网关向输入的地址实际请求一次 `/mint`（不必先保存），失败时显示 mint 返回的原因。
- mint 容器可选出站代理 `MINT_PROXY`：`http(s)://`（可带 `user:pass@`，由页面应答代理认证）或 `socks5://`（带认证时走容器内置中转）。
- 账号始终靠导入 Cookie，无自动注册。

### 模型（手写目录）

目录是 `backend/internal/infra/provider/adobe/models.go` 里的一张表，共 15 个模型。取值抄自官方 discovery schema，填法抄自官网前端（2026-10-19 核对）。

| 类型 | 对外名 | 上游（`modelId/版本`） |
|---|---|---|
| 视频 | `veo-3.1`、`veo-3.1-fast`、`veo-3.1-lite` | `veo/3.1-generate`、`veo/3.1-fast-generate`、`veo/3.1-lite-generate` |
| 视频 | `seedance-2.0`、`seedance-2.0-fast`、`seedance-2.5` | `seedance/seedance_2.0`、`seedance/seedance_2.0_fast`、`seedance/seedance_2.5` |
| 视频 | `kling-3.0`、`kling-3.0-omni`、`kling-video-2.5-turbo` | `kling/kling_v3`、`kling/kling_v3_omni`、`kling/kling_v2_5_turbo_pro_i2v` |
| 图像 | `nano-banana-pro`（Gemini 3）、`nano-banana-2`（Gemini 3.1）、`nano-banana-2.1` | `gemini-flash/nano-banana-2`、`gemini-flash/nano-banana-3`、`gemini-flash/nano-banana-2026-09` |
| 图像 | `gpt-image-2`、`gpt-image-2.5-flare`、`gpt-image-2.5-sunburst` | `gpt-image/2`、`gpt-image/gpt-image-2.5-flare`、`gpt-image/gpt-image-2.5-prism` |

- **路由**：启动时按表同步，表外模型的路由删除。视频模型有视频路由，图像模型有文生图与改图路由。上游模型（也是价格的键）是 `modelId/版本`。
- **请求体**：按表里的字段和取值填，再按官网前端的规则放置比例、分辨率与参考图用途（首帧 `frame`、参考图 `subject` / `style` / `general`、gpt-image 输入图 `source`、蒙版 `mask`）；表里没列的字段不发。
- **维护**：加模型、或 Adobe 改了取值时改 `models.go`。可选核对测试对照官网 discovery，列出下架、取值变化和同家族的新版本（默认跳过，需联网）：

  ```bash
  cd backend && ADOBE_DISCOVERY_CHECK=1 go test ./internal/infra/provider/adobe -run TestCatalogMatchesDiscovery
  ```

  核对官网后改表，并用线上条目更新 `testdata/discovery-reviewed.json`。
- **参数校验**：
  - 图像：模型不支持的 `aspect_ratio` 取最接近的比例，`resolution` 取最接近的档位，不支持透明背景时忽略 `background`；未给比例时按 `size` 取最接近的比例。模型不支持多张时 `n` 按 1；输入图超出上限（含蒙版计入的素材总数）时保留前面的；完全不收输入图或蒙版的模型返回 400，不支持 `stream` / `partial_images`。
  - 视频：首帧（`image`）、尾帧（`last_frame`，必须同时给 `image`）、参考图与参考视频（`reference_videos`）超出表里的数量或素材总数上限时保留前面的，模型不收的素材种类返回 400；`duration` / `aspect_ratio` / `resolution` 没传时用模型默认值（Kling 5 秒、Veo 8 秒、Seedance 4 秒；比例 16:9；分辨率通常 720p），传了但不支持的就近对齐（如 2K 取最高档 1080p）；入队前就选定实际值，任务记录、计费与结果里的 `video.duration` 都用它。参考视频只对 Seedance 2.0 / 2.0 Fast / 2.5 开放（MP4 或 QuickTime，上传到 `/v2/storage/video`，以 `source` 用途引用）。Seedance 与 Veo 3.1 的首尾帧不能与参考图、参考视频同时使用（与官网一致），同时给出时保留首尾帧；Seedance 2.5 按官网只开放 9 张参考图、3 个参考视频、合计 12 个。官网前端还要求 Seedance 参考视频 480–720p、总时长 2–15 秒；网关不预检，超限时以 Adobe 的返回为准。`reference_audios` 忽略。
- **视频编辑与延长**（`/v1/videos/edits`、`/v1/videos/extensions`，与官网一样按操作换上游版本，路由不变）：
  - `seedance-2.5`：编辑提交到 `seedance/seedance_2.5_editing`，延长提交到 `seedance/seedance_2.5_extension`（官网未开放延长，按 schema 接）。比例固定 auto，分辨率 480p / 720p（默认 720p）；延长时长 4–30 秒，默认 4 秒。编辑按官网默认不另生成音轨（`generateAudio: false`），延长与生成一样生成音轨。
  - `kling-3.0-omni`：编辑提交到 `kling/kling_v3_omni_v2v_edit`。可选 16:9 / 9:16 / 1:1 与 720p / 1080p（默认 16:9、720p；竖屏或方形源视频请传与之一致的 `aspect_ratio`），保留原音轨（`keepAudio`）。不支持延长。
  - 源视频上传到 `/v2/storage/video`，以 `source` 用途引用；`generationMetadata.submodule` 为 `ff-video-editor`。编辑的时长跟随源视频，传了 `duration` 也忽略。源视频与其他输入一样受网关单个文件 20 MiB 的上限（Adobe 本身收到 200 MiB）。官网前端对源视频的要求（Seedance 480–1080p、4–30 秒；Kling 720–2160p、3–10 秒）网关不预检，超限时以 Adobe 的返回为准。与 Grok 一样，编辑与延长不计费。
- 输入图支持 Base64 data URI 与 `https` URL（经带 SSRF 防护的下载器，不走账号出口），只接受 PNG/JPEG/WebP。
- Veo 3.1 带参考图时固定 16:9、8 秒，Veo 3.1 Fast 不接受参考图；`generateAudio` 按 schema 默认值（Kling 3.0 / Omni 关闭，其余开启）。图像 4K：Nano Banana Pro / 2 / 2.1 按 schema 的 4K 尺寸（如 16:9 为 5504×3072）；GPT Image 2 / 2.5 的 4K 取 schema 上限内的最大尺寸（边长 ≤3840、总像素 ≤8,294,400，如 16:9 为 3840×2160、1:1 为 2880×2880），官网前端只开放到 1K/2K；推导尺寸超限时按比例缩进上限。
- 定价：Adobe 无官方价，未设价按 0 计费、模型页标"未定价"；价格按上游模型（`modelId/版本`）存储。积分（`firefly.adobe.io/v1/credits/balance`）作为 `credits` 窗口，所有 Adobe 路由都消费它。

### 失败处理

- 提交前的失败（上传、mint 不可用、Adobe 明确 4xx 拒绝）不会生成任务：401 先续期重试，积分不足耗尽该账号 `credits` 窗口后换号，参数或内容被拒（400/422）直接返回原因、不换号。
- 提交后的失败（5xx、网络中断）结果未知，任务标记为不确定，绝不重提，避免重复扣积分。轮询容忍连续 5 次瞬时失败。
- mint 不可用时返回 503；`x-arp-session-id` 被 408 拒绝会强制重铸重试一次，仍被拒返回 408。两者都不换号、不扣账号健康度（问题在 mint 环境，不在账号）。

### 出口

使用作用域 `adobe` 的出口节点（HTTP/SOCKS/Trojan）。提交、上传下载、IMS 续期都走账号绑定的节点，连接用出口的浏览器 TLS 客户端（tls-client）；节点上配置的 UA 对 Adobe 不生效，用账号自己的浏览器身份。传输失败时按出口问题反馈健康度。

## Muse（muse.ai）接入

Muse 是 Meta 的 agent 应用（`https://muse.ai`）。它没有公开 API：网页与账号自己的云端 VM 之间走加密 WebSocket，所以网关不调接口，而是通过 Chrome DevTools 协议驱动一个独立的无头 Chrome，在网页里发送提示词、读取回复和生成的文件。

每个请求的流程：为账号新建一个独立的浏览器上下文（Cookie 互相隔离，代理取账号的出口）→ 写入账号 Cookie → 打开 `https://muse.ai/thread/new` → 发送提示词 → 等回复或文件 → 关闭上下文。

比 API 类 Provider 慢得多（实测）：页面加载 2–7 秒；对话首字 7–15 秒（agent 先思考），之后很快流完；生图约 30 秒；视频要几分钟。

### 部署与设置

1. **启动浏览器容器**（`chromedp/headless-shell`，固定版本）：

   ```bash
   docker compose --profile muse up -d
   ```

   grok2api 默认指向 compose 的 `muse-browser` 服务（`provider.muse.browserURL=http://muse-browser:9222`）。DevTools 端口没有认证，能完全控制浏览器和其中的账号 Cookie；compose 里没有发布这个端口，**不要把它暴露到宿主机或公网**。非 compose 部署需自行运行无头 Chrome，并让网关能访问它的 DevTools 端口。
2. **运行设置 → Muse**（立即生效，保存后以数据库中的值为准）：
   - **浏览器地址**：DevTools HTTP 端点，留空即用默认地址；公网地址只能用 HTTPS（443 端口）。地址框旁的「测试」由网关连接输入的地址（不必先保存）。
   - **并发页面数**：浏览器同时打开的页面上限，1–16，默认 2。每个请求占一个页面，超出的排队。
3. **导入账号**（账号页 → Muse 标签）：登录 `https://muse.ai`，用 Cookie-Editor（或 EditThisCookie）扩展把该站 Cookie 导出为 JSON，粘贴或上传，一个 JSON 数组为一个账号。也支持：
   - 本服务导出的 JSON / JSONL（`{"cookies":[…],"name":"…"}`）；
   - 每行一个 Cookie 字符串 `hatch_sess=…; hatch_vml=…; hatch_native_auth_device=…`，可加 `名称 | ` 前缀。

   必须包含 `hatch_sess`。导入时向 `https://muse.ai/api/session` 验证会话。

### 凭据与续期

- `hatch_sess` 有效期约 30 天，`hatch_vml` 约 48 小时。网关在 `hatch_vml` 过期前约 1 天用 `/api/session` 换新（走账号出口，不需要浏览器），并合并返回的 Cookie。
- **云端 VM 唤醒**：每个账号在 muse.ai 有一台云端 VM，闲置一段时间会挂起（`SUSPENDED`）。续期时如果发现 VM 不在运行，就顺带调用 `/api/hatch/vm/wake` 唤醒（实测几秒内恢复 `RUNNING`），失败只记日志、不影响续期。续期仍是每天一次；网页每次打开时也会自己唤醒一次。
- 会话被拒（401）→ 账号转入需重新导入。打开网页不会轮换登录 Cookie，浏览器里的操作不写回凭据。
- **浏览器身份**：每个账号固定一个桌面 Chrome 身份（Windows 10/11、macOS Intel/Apple 芯片、Linux 之一），导入时随机分配并存在凭据里，导出再导入、重新导入都保持不变。浏览器页面的 User-Agent、`navigator.platform` 和 Client Hints（`Sec-CH-UA*`、`navigator.userAgentData`）一起按这个身份改写；续期请求也带同一个 UA。Chrome 版本跟随实际运行的浏览器（续期请求按 compose 里固定的 155 版）。共用的仍是同一个 Chrome 进程，渲染与字体特征相同，区分账号主要靠给账号绑定不同出口。
- **时区与自动化标记**：每个账号按 ID 固定一个美国时区（纽约、芝加哥、丹佛、洛杉矶之一），网页里的 `Intl`/`Date` 按它显示。compose 用 `--disable-blink-features=AutomationControlled` 启动浏览器，网页读到的 `navigator.webdriver` 为 false；自行运行 Chrome 时请加上同一个参数。

### 额度

- 只有一个窗口 `weekly_usage`（每周用量），按百分比显示：总量 100，剩余 = 100 − 已用。数值读自网页设置里的用量面板（如 `2% used`），重置时间也取自面板（`Weekly limit resets on Oct 31`）。读取用量要打开一次网页。
- 所有 Muse 模型共用这一个限额，用满后账号等到重置时间再接请求。

### 模型

| 模型 | 类型 | 说明 |
|---|---|---|
| `muse-spark` | 对话 | 回复按网页渲染结果转回 Markdown（代码块、粗体、表格、列表） |
| `muse-image` | 图像生成 + 编辑 | 编辑时输入图作为附件上传到输入框；实测输出约 1600 像素宽的 WebP |
| `muse-video` | 视频 | 发送提示词后记下对话路径，重启后重新打开该对话等结果，不重复发送 |

定价：Muse 没有官方价，未设价按 0 计费、模型页标"未定价"。

### 限制

- **出口**：使用作用域 `muse` 的出口节点，节点的代理设为该账号浏览器上下文的代理。Chrome 不支持带用户名密码的代理，也不支持 Trojan/VLESS/SS/VMess 隧道（与 FlareSolverr 相同），这类节点会被拒绝并提示原因；只能用无认证的 HTTP(S) / SOCKS 代理或直连。
- **对话**：每个请求在网页里新开一个对话，历史消息由客户端每次带上；请求里的 `tools` 会被忽略（网页端的 agent 不会返回函数调用），历史里的工具调用和结果按文字转述。最后一条用户消息里的图片作为附件上传，更早的图片只标注为 `[image]`。
- **图片**：每次只出一张图，`n` 大于 1 时网关把请求分摊到多个账号。编辑的输入图超过 4 张时保留前 4 张。不支持蒙版、流式出图；比例取自 `aspect_ratio` 或 `size`，和 `resolution` 一起写进提示词，`size` 不是 宽x高 时忽略。agent 只回文字（多为拒绝）时返回 422，不扣账号健康度。
- **视频**：支持文生视频、首帧图片（`image`）和参考图（`reference_images`），两者合计最多 4 张，多出的保留前面的；参考音频忽略。比例取 16:9、9:16、1:1、4:3、3:4 中最近的，`duration` 对齐到 1–15 秒（默认 5），按 720p 计价。时长和比例写进提示词，最终由 agent 决定：实测要求 5 秒，得到 10 秒的 1280×720 H.264。
- **视频无法在浏览器里播放**：headless-shell 不带 H.264 解码器，网关拿不到视频宽高，但下载不受影响。
