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
2. **设置**（设置 → ChatGPT）：
   - **图片会话模型**：驱动生图的 ChatGPT 模型，必须出现在账号的模型目录里。留空时图片路由不可用。
   - **输出上限**：默认 16,384。
   - **单图 Token**：默认 256。
   - **默认图片模型**：Responses 图片工具没写模型时使用。
   - **Cloudflare Clearance**：默认关闭。
3. **模型**：账号模型同步后，聊天模型自动出现为路由；`gpt-image-2` 的生图和编辑路由在启动时注册。
4. **价格**（模型页 → 设置价格）：ChatGPT 没有官方价。未设置价格时按 0 计费，模型页会标"未定价"。
5. **密钥**：范围为"全部"的密钥自动包含 ChatGPT 和以后新增的 Provider。ChatGPT 不受密钥的档位限制。

新账号每个默认并发为 1。额度按 `reason`、`image_gen`、`file_upload` 三个窗口显示，每 30 分钟自动同步一次，用完请求后也会刷新。

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

### 聊天

- 不支持的参数会被忽略。
- `max_tokens` / `max_completion_tokens` / `max_output_tokens` 按"输出上限"截断；达到上限时 Responses 状态为 `incomplete`。
- `reasoning.effort` / `reasoning_effort` 映射到上游的推理强度。
- 声明 function 工具时，模型会被提示直接回答，不会返回 `tool_calls`。
- Responses 流式按官方事件序列输出，带 `sequence_number`；失败时发送 `response.failed`。
- 用量为本地 `o200k_base` 估算，包含推理 token 和图片输入的固定 token，审计里标记为估算。

### 图片

- **编辑输入**：`image`、`images`、`image_url`、`mask` 接受以下形式：
  - data URI、https URL、裸 base64；
  - `{"image_url": …}`、`{"url": …}`、`{"b64_json": …}` 及其数组；
  - multipart 文件。

  `file_id` 会被明确拒绝。
- **返回格式**：默认返回 `b64_json`；需要链接时传 `response_format: "url"`。
- **参数**：支持 `background`（含 `transparent`）和 `output_format`。透明背景不能输出 jpeg。
- **参数校验**：按 Provider 进行；被拒绝的路由会换同名的其他 Provider 路由。

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
