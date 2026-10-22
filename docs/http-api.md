# HTTP 接口

代码：`internal/copilot/service.go`（路由与转发）、`pool.go`（`Pool.Handler()` 承载业务与管理路由）。

## 鉴权

- `api_key` 必填（见 `config.md`），与监听地址无关；不存在匿名模式。除健康检查和管理接口外的请求需 `Authorization: Bearer <api_key>`，也支持 Anthropic 客户端的 `x-api-key`。库层同样不接受空密钥：构造函数拒绝空 `APIKey`，鉴权中间件收到空密钥时拒绝所有需鉴权的请求（失败即拒绝），见 `pool.md`。
- `/v0/management/*` 改用管理密钥，`api_key` 不能访问；详见 `management.md`。
- 只有鉴权失败（业务 `api_key` 或管理 `management_key`，返回 401）的请求才不输出"收到客户端请求"与"客户端请求处理完成"，只输出一条 WARN 告警，字段为 `method`、`path`（各自最多 256 字节，按 UTF-8 字符边界截断）与 `remote_addr`（客户端地址）。其他请求都输出这两条日志：鉴权通过的请求、`/healthz`、`/favicon.ico`、控制台页面 `/management`，以及 ServeMux 自身产生的响应（如 `POST /management` 的 405；携带管理密钥的 `/v0/management//accounts` 的路径清理重定向）。鉴权位于请求日志之外（见 `logging.go` 的 `withRequestID`、`withRequestLog` 与 `pool.go`、`management.go` 中的鉴权中间件）。
- 请求头上限 `MaxHeaderBytes` 为 64 KiB（Go 实际另有约 4 KiB 余量），超出时由 HTTP 服务拒绝。
- 生成响应的写超时：生成接口（流式与非流式）向客户端写响应时，每次写入（含刷新）前把写截止时间续期为现在起 60 秒（`generationWriteTimeout`，包级变量）。客户端长时间不读取、写入超时后请求结束，并释放并发名额，上游请求随之取消。请求结束时不清除截止时间：net/http（HTTP/1.1）在每个请求结束后会自行清除连接的写截止时间，因此 handler 返回后的最后一次 flush 仍受超时保护，之后复用该连接的请求不受影响。服务器没有全局 `WriteTimeout`，不会截断长时间的生成。

## 并发上限

- 生成接口（chat completions、responses、`/v1/messages`）同时处理的请求数上限由 `max_concurrent_requests` 控制，缺省 `32`，`0` 表示不限制。
- 名额已满时立即返回 503，响应体为 `{"error":{"message":"生成请求并发数已达上限，请稍后重试","type":"copilot_proxy_error"}}`，不读取请求体、不选账号、不访问上游。
- 名额在请求结束（含流式响应结束）后释放。模型列表、健康检查与管理接口不占用名额。
- 只在账号池（`Pool`）上生效；名额检查见 `pool.md`。

## 接口

| 本地接口 | 上游接口 | 说明 |
| --- | --- | --- |
| `GET /healthz` | 无 | 进程健康状态，不检查订阅 |
| `GET /v1/models`、`/models` | `/models` | 模型列表，保留端点等元数据，并补充 `auto` |
| `POST /v1/chat/completions`、`/chat/completions` | `/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses`、`/responses` | `/responses` | OpenAI Responses |
| `POST /v1/messages` | `/v1/messages` | Anthropic Messages |

Web 控制台与管理接口见 `management.md`。

```sh
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <api_key>' -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

OpenAI 客户端 base URL：`http://127.0.0.1:8080/v1`；Anthropic 客户端 base URL：`http://127.0.0.1:8080`。

## 协议与转发

- 三种生成协议不自动互转，按模型的 `supported_endpoints` 选接口。模型列表可用时，生成前会校验模型与上游路径，不匹配返回 400；旧模型无该字段按 Chat Completions 处理；列表不可用时手选模型交上游判断，Auto 会重试读取列表，仍失败则返回错误。
- 上游的 HTTP 状态、生成响应和 SSE 原样转发（例外见下条）；请求取消传递给上游；生成请求体最大 16 MiB。
- Responses 流的输出项 id 统一：Copilot 的 `/responses` SSE 在同一输出项的每个事件里都给出新的 id（`output_item.added`、各 delta 的 `item_id`、`output_item.done` 的 `item.id` 及 `response.completed` 的 `output[i].id` 互不相同），Codex 按 added 的 id 开始、按 done 的 id 结束一项，会把同一条消息显示两次。因此仅对上游路径 `/responses` 且 `Content-Type` 为 `text/event-stream` 的响应，按 `output_index` 把后续事件及结束事件（`response.completed`/`incomplete`/`failed`）中的 id 改回该项 `output_item.added` 时的 id（项类型不一致时不改）。需改写的事件会重新编码 JSON（键顺序可能变化，数字与 `<`、`>`、`&` 保持原文）；id 已一致、非 JSON 数据（如 `[DONE]`）和其他事件逐字节透传。只输出完整事件，未完整的尾部在上游结束时原样写出。代码见 `itemids.go`、`request.go` 的 `streamResponse`。
- 无工具时去掉 `tool_choice`：Copilot 的 `/responses` 对设置了 `tool_choice` 却没有声明工具的请求直接返回 400（如 Codex 压缩上下文的请求会去掉 `tools` 但保留 `tool_choice`）。因此转发到上游 `/responses` 的请求（含 Auto），顶层 `tools` 缺失、为 `null` 或空数组时去掉顶层 `tool_choice`；改写时重新编码 JSON（键顺序可能变化，数字与 `<`、`>`、`&` 保持原文）。`tools` 为非空数组或其他类型、请求体不是合法 JSON 对象时原样转发；Chat Completions 与 `/v1/messages` 不做此处理。代码见 `request.go` 的 `withoutLoneToolChoice`。
- 不转发客户端认证信息、Cookie 和逐跳头；拒绝重定向及不在 GitHub/Copilot 白名单内的 session API 地址。
- 生成请求体校验在 session 获取之后，故所有账号 session 都失败时，无效请求体收到 session 错误而非 400。

## 上游请求头与消息解析

消息解析只在请求体上做字节级扫描，不把整个请求体解析成通用对象；`X-Initiator`、`Copilot-Vision-Request`、会话亲和键与 Auto 提示词共用这套规则（代码见 `request.go`、`jsonscan.go`）。

- `X-Initiator`：取 `messages` 数组的最后一个对象项；`messages` 没有任何元素时，取 Responses 的 `input` 数组（即使为空）。该项的 `role` 为 `user`，且 `content` 不是"只含 `tool_result` 的内容块数组"时为 `user`，否则为 `agent`。两者都不是数组时，`input` 为字符串得 `user`，其余得 `agent`。
- `Copilot-Vision-Request: true`：请求体是合法 JSON，且任意深度的对象中键 `type` 的字符串取值为 `image`、`image_url` 或 `input_image` 时发送。键与取值按解码后的内容比较（`\u` 转义同样生效），取值大小写敏感；非法 JSON 不发送。
- Auto 的提示文本（`promptFrom`）：`input` 为非空字符串时用它；否则取最后一条含非空文本的 `user` 消息的文本；都没有时为 `hi`。消息的筛选与内容块的文本规则与上面相同。
- 已接受的语义变化（与按通用 JSON 解析的旧实现不同）：
  1. 数组中的非对象元素（数字、字符串、`null` 等）被跳过，例如 `messages:[{"role":"user"},1]` 的 `X-Initiator` 为 `user`；
  2. 字段类型错误的消息或内容块整项按无效处理并跳过，例如 `"role": true` 的消息、`"text": 5` 的内容块，不影响同一数组中的其他项；这会影响 `X-Initiator`、会话亲和键与 Auto 提示词。例如 `messages:[{"role":"user","content":"a"},{"role":true}]` 的 `X-Initiator` 为 `user`；
  3. 重复的 `type` 键只要任一次出现的取值为图片类型即判为图片。通用解析只保留最后一个值，这里从严判断，避免用重复键绕过图片检测。
