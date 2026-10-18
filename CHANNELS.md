# 扩展渠道与 ChatGPT 接入

本分支在原有 Grok 网关之外增加了“扩展渠道”框架，首个实现是 ChatGPT Web 渠道。原有 Grok 的账号、模型、权限和计费逻辑保持不变。

> **状态：实验性。** ChatGPT 的协议实现移植自 [gptGrok2api@1577309](https://github.com/SkyKoo/gptGrok2api/tree/15773099641575776025e21d1290b5209b058c5a)（MIT，见 `backend/internal/infra/provider/chatgpt/LICENSE.reference`），本地测试只用模拟上游。OAuth、聊天、生图、蒙版、透明背景都需要用真实账号逐项验收后再对外开放。参考项目曾记录过真实上游返回 `404 Model not found` 的问题。

## 启用步骤

渠道默认关闭。在管理后台的“渠道中心”按顺序操作：

1. **添加账号**：导入 AT/RT，或使用浏览器 OAuth（打开授权链接，登录后把浏览器地址栏里的完整回调 URL 粘贴回来，必须带 `state`）。
2. **检测账号**：新账号处于“待检测”，检测通过（`verified`）后才参与调度。
3. **设置**：打开“启用渠道”，填写“图片 Web 会话模型”。该模型必须出现在已验证账号的模型目录里，不会自动替换成某个写死的模型。
4. **定价**：为每个上游模型和操作保存价格。显式零价有效，没有价格的模型不能开放。
5. **建路由**：创建对外模型名到上游模型的路由。
6. **授权密钥**：给客户端密钥单独授权。原有密钥的 `all` 只覆盖 Grok，**不会自动获得 ChatGPT 权限**。

## 出口代理与 FlareSolverr

**出口代理**复用原项目的出口管理：

- 在“设置 → 出口节点”新建节点或订阅源时，作用域选择渠道（如 ChatGPT）。支持 HTTP(S)、SOCKS4/5 和 Trojan/VLESS/SS/VMess 隧道，代理可以部署在内网或本机（例如 sidecar）。
- 同一账号按亲和性固定使用同一节点（`{account}` 粘性代理模板的账号标识为 `chatgpt_<账号ID>`）。聊天、上传下载和 OAuth 续期都走该节点。也可以在渠道中心把账号手动绑定到指定节点。
- **故障处理**：连接失败的节点进入指数退避冷却并触发探测；被 Cloudflare 拦截的节点降低健康度，账号随之迁移。请求在到达上游之前失败时，立即换一个出口重试一次。已经发出的请求不会重放，避免重复生成和重复扣额度。
- **备用出口**：在“设置 → 出口运维”为渠道配置备用出口（不回退 / 直连 / 指定节点）。主节点全部不可用时启用；指定节点在主节点健康时不承担流量。
- 没有该渠道的节点且未配置备用出口时直连，不会借用 Grok 节点。审计记录出口节点 ID 和名称。

**Cloudflare 与 FlareSolverr**：

- 网关会识别 Cloudflare 挑战（403 加 `cf-mitigated: challenge` 响应头或挑战页面），与账号自身的 403 分开处理。
- 渠道设置中的 **Cloudflare clearance** 开关默认关闭。开启后，某个出口遇到挑战时，下一次请求会通过系统设置里配置的 FlareSolverr（与 Grok Web 共用地址、超时和刷新间隔）在该出口上求解 `https://chatgpt.com`，然后带着求解得到的 cookie 重试。clearance 按节点或粘性代理账号缓存，与 Grok 互不覆盖。
- `cf_clearance` 只对求解浏览器的 UA 和出口 IP 有效。所以带 clearance 的请求会改用求解浏览器的身份：UA、client hints、TLS 指纹和 Sentinel 环境，一般是 Linux Chrome。没有挑战时继续使用账号自己的浏览器档案。
- 每次轮换 IP 的代理池无法复用 clearance。FlareSolverr 不支持隧道协议代理。

## 账号导入格式

支持单个 JSON 对象、JSON 数组、`{"accounts": [...]}`、JSONL，以及每行一个 AT 的纯文本。每条记录单独处理，结果逐条返回（导入、重复、失败及原因）。

```json
{"name": "a", "access_token": "…", "refresh_token": "…", "client_profile": "platform_pkce_v1",
 "oai_device_id": "…", "oai_session_id": "…", "browser_profile": "chrome_131"}
```

- 兼容 `accessToken`、`token`、`refreshToken` 等字段名；`browser_profile` 可以是内置名称或参考项目的对象格式。
- 只有 RT 时必须写在 `refresh_token` 字段里，且需要 `client_profile`，否则不会自动续期。
- 默认跳过重复账号；勾选“替换重复凭据”时只替换凭据，保留名称、启用状态和并发设置。
- 导出为带版本号的规范格式，包含明文凭据，响应禁止缓存。

## 对外接口

| 接口 | 说明 |
|---|---|
| `GET /v1/models` | 返回已授权、已启用、已定价的扩展模型 |
| `POST /v1/chat/completions` | 文本、多轮历史、图片输入，流式和非流式 |
| `POST /v1/responses` | 基础会话（不带工具）；带 `image_generation` 工具时生成图片，见下文 |
| `POST /v1/images/generations` | 同步生图，`n` 为 1–10，`response_format` 支持 `url`、`b64_json` |
| `POST /v1/images/edits` | 同步编辑，JSON 或 multipart，支持多参考图和 PNG 蒙版 |
| `GET /v1/responses/{id}` | 查询图片响应 |
| `POST /v1/responses/{id}/cancel` | 取消尚未提交到上游的图片响应 |

### 异步生图（OpenAI Responses 官方格式）

```bash
curl https://<host>/v1/responses -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" -d '{
  "model": "gpt-image-2",
  "background": true,
  "input": "a red fox in the snow",
  "tools": [{"type": "image_generation", "size": "1024x1024", "quality": "high",
             "background": "transparent", "output_format": "png"}]
}'
# => {"id": "resp_…", "object": "response", "status": "queued", ...}

curl https://<host>/v1/responses/resp_… -H "Authorization: Bearer $KEY"
# status: queued → in_progress → completed | failed | cancelled
# output[0]: {"type": "image_generation_call", "status": "completed", "result": "<base64>", "size": "1024x1024"}
```

- 不带 `background` 时请求会等待生成完成后返回同样的响应对象。
- 编辑：在 `input` 的消息里加入 `{"type": "input_image", "image_url": "<data URI 或 https URL>"}`；蒙版写在工具的 `input_image_mask.image_url`，只能配合一张原图。
- 路由优先用工具里的 `model`，没有时用顶层 `model`。
- 每个响应生成一张图，与官方工具一致。需要多张时并发提交多个请求，或使用同步 Images API 的 `n`。
- 支持 `Idempotency-Key` 请求头：同一密钥、同一请求内容返回原响应，内容不同返回 `409`。
- 取消只对尚未提交到上游的任务生效；已提交的生成无法撤回，返回 `409 response_not_cancellable`。对已结束的响应调用取消会直接返回最终对象。
- 状态为 `failed` 且 `error.code` 为 `result_uncertain` 表示上游可能已经出图但无法确认，系统不会自动重试，管理员可在“图片任务”中手动重新执行（可能再次消耗上游额度）。

### 不支持的参数（明确返回 400，不会静默忽略）

- 聊天：工具调用、`previous_response_id`、`store: true`、采样参数、上游输出长度参数。
- 图片响应：`stream: true`、除 `image_generation` 外的工具、`output_compression`、`input_fidelity`、`moderation: low`、`file_id` 引用、助手消息作为输入。`partial_images` 可以传 0–3，但不会产生中间帧（上游不提供）。
- `DELETE /v1/responses/{id}` 不处理图片响应，记录按保留策略清理。

## 计费与估算

| 项目 | 规则 |
|---|---|
| 聊天 | 本地 `o200k_base` 分词估算输入、输出 Token，按每百万 Token 价格计费。这只是估算口径，不等于 ChatGPT Web 的实际用量 |
| 图片输入 | 每张按固定 Token 估算，默认 256 |
| 输出上限 | 默认 16,384 估算 Token；达到上限时网关截断，`finish_reason` 为 `length` |
| 图片 | 只对成功归档、可交付的图片按张计费；失败和结果不确定的子任务不收费 |
| 价格版本 | 请求受理时固定价格版本，之后改价不影响已受理的请求；审计记录价格版本、估算规则和截断标记 |

## 默认值

| 项目 | 默认 |
|---|---|
| 渠道 | 关闭 |
| OAuth 会话 | 10 分钟 |
| 每账号并发 | 1（导入时取渠道设置） |
| 图片 worker | 2 |
| 单张图片期限 / 任务期限 / 排队期限 | 10 分钟 / 60 分钟 / 24 小时 |
| 任务租约 / 心跳 | 90 秒 / 30 秒 |
| 聊天期限 | 跟随 `server.requestTimeout`（默认 2 小时），账号并发槽每 30 秒续租 |
| PoW / Sentinel VM | 最多 50 万次尝试 / 约 2 万步 |
| 图片任务记录 | 30 天后清理，图片资产按媒体保留策略回收 |

## 上游兼容记录

| 项目 | 内容 |
|---|---|
| 上游 | `https://github.com/chenyme/grok2api` |
| 基线 | `e03cacb9`（2026-10-18，`docs: update README.md`） |
| Go 模块名 | 保持 `github.com/chenyme/grok2api/backend`，不做全仓 import 改名 |
| 渠道契约版本 | `channel.ContractVersion = 1` |
| 新增依赖 | `github.com/tiktoken-go/tokenizer v0.8.0`、`github.com/HugoSmits86/nativewebp v1.3.0`、`golang.org/x/image v0.43.0` |

新代码集中在新目录，不与上游重叠：`backend/internal/{domain,application}/channel`、`backend/internal/infra/provider/chatgpt`、`backend/internal/transport/http/channel`、`backend/internal/infra/persistence/relational/channel_*.go`、`frontend/src/features/channels`。

修改的上游接入点：

| 文件 | 改动 |
|---|---|
| `backend/internal/app/application.go` | 构造渠道注册表、服务、ChatGPT 适配器，启动图片 worker 与维护任务 |
| `backend/internal/transport/http/server.go` | 注册渠道管理接口、公开接口和 `Dispatch` 中间件 |
| `backend/internal/transport/http/inference/handler.go` | `/v1/models` 追加扩展渠道模型 |
| `backend/internal/transport/http/media/ingest.go` | 导出 `FetchRemoteImage`，复用远程图片的安全检查 |
| `backend/internal/domain/account/account.go` | Provider ID 只校验格式，新增 `IsLegacy()` |
| `backend/internal/domain/model/model.go` | 按实际存在的 Provider 解析模型名 |
| `backend/internal/infra/provider/provider.go` | 注册表按实际适配器枚举 |
| `backend/internal/infra/persistence/relational/models.go`、`schema.go`、`schema_channels.go` | 去掉 Provider/出口作用域/认证类型的枚举 `CHECK`，并阻止旧迁移重建 |
| `backend/internal/infra/persistence/relational/{audit_repository,mapping,model_repository,media_repository,egress_repository,account_links}.go` | 审计估算字段、动态 Provider 路由、渠道资产保护、保留出口作用域 `all` |
| `backend/internal/domain/audit/audit.go`、`transport/http/audit/handler.go` | 审计记录估算规则 |
| `backend/internal/application/egress/{service,operations}.go`、`backend/internal/transport/http/egress/handler.go` | 出口节点、订阅源、账号绑定和备用出口接受已注册的扩展作用域（`backend/internal/domain/egress/extension.go`） |
| `backend/internal/infra/egress/manager.go` | 扩展作用域的 clearance 配置与缓存键（`extension_clearance.go`），扩展租约不再构造 Grok 浏览器客户端 |
| `backend/internal/infra/persistence/relational/{egress_repository,schema}.go` | 扩展作用域的备用出口存放在新表 `egress_extension_fallbacks`（`egress_extension_fallbacks.go`），不改原有列 |
| 前端 `app-shell`、`router`、`deferred-pages`、`i18n`（1 个导航词条）、模型/审计页 Provider 类型、创作控制台 | 渠道中心入口，Provider 类型放宽为字符串 |

### 合并演练

上游当前没有比基线更新的提交，因此用历史回放演练：把上游最近一段时间的改动与本分支做三方合并（`git merge-tree --merge-base=e03cacb9 <过去的上游提交> HEAD`），统计冲突文件。

| 回放窗口 | 上游提交 | 上游改动文件 | 冲突文件 |
|---|---|---|---|
| 1 个月（2026-09-16 起） | 36 | 60 | 0 |
| 2 个月（2026-08-17 起） | 254 | 218 | 8，全部是上表的接入点 |

按月同步时预计没有或只有少量冲突，冲突会集中在上表列出的文件。

### 同步上游后的检查

```bash
git remote add upstream https://github.com/chenyme/grok2api.git   # 首次
git fetch upstream && git merge upstream/main
cd backend && go build ./... && go vet ./...
TEST_POSTGRES_ADMIN_DSN='postgres://…' go test ./...               # 含 SQLite/PostgreSQL 迁移
cd ../frontend && pnpm build && pnpm lint && pnpm test
grep -rn "check:chk_.*provider IN\|check:chk_.*scope IN" ../backend/internal/infra/persistence/relational/models.go   # 应无输出
```

重点确认：上游是否新增了写死 Provider 枚举的 `CHECK` 或 `switch`；`Dispatch` 拦截的路径（`/v1/chat/completions`、`/v1/responses`、`/v1/responses/{id}`、`/v1/images/*`）的上游处理器是否改变了请求体读取方式。

回滚优先在渠道中心关闭渠道。原版程序能否直接运行在已写入扩展数据的数据库上未经验证，不要假定可以直接降级。
