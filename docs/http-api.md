# HTTP 接口

代码：`internal/copilot/service.go`（路由与转发）、`pool.go`（`Pool.Handler()` 承载业务与管理路由）。

## 鉴权

- `api_key` 必填（见 `config.md`），与监听地址无关；不存在匿名模式。除健康检查和管理接口外的请求需 `Authorization: Bearer <api_key>`，也支持 Anthropic 客户端的 `x-api-key`。库层同样不接受空密钥：构造函数拒绝空 `APIKey`，鉴权中间件收到空密钥时拒绝所有需鉴权的请求（失败即拒绝），见 `pool.md`。
- `/v0/management/*` 改用控制台令牌（管理密钥只能用于换取令牌），`api_key` 不能访问；详见 `management.md`。
- 只有鉴权失败（业务 `api_key`，或管理接口的控制台令牌/session 接口的 `management_key`，返回 401）的请求才不输出"收到客户端请求"与"客户端请求处理完成"，只输出一条 WARN 告警，字段为 `method`、`path`（各自最多 256 字节，按 UTF-8 字符边界截断）与 `remote_addr`（客户端地址）。其他请求都输出这两条日志：鉴权通过的请求、`/healthz`、`/favicon.ico`、控制台页面 `/management`，以及 ServeMux 自身产生的响应（如 `POST /management` 的 405；携带控制台令牌的 `/v0/management//accounts` 的路径清理重定向）。鉴权位于请求日志之外（见 `logging.go` 的 `withRequestID`、`withRequestLog` 与 `pool.go`、`management.go` 中的鉴权中间件）。
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
| `GET /v1/models`、`/models` | `/models` | 模型列表，保留端点等元数据 |
| `POST /v1/chat/completions`、`/chat/completions` | `/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses`、`/responses` | `/responses` | OpenAI Responses |
| `POST /v1/messages` | `/v1/messages` | Anthropic Messages |

Web 控制台与管理接口见 `management.md`。

```sh
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <api_key>' -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4.1","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

OpenAI 客户端 base URL：`http://127.0.0.1:8080/v1`；Anthropic 客户端 base URL：`http://127.0.0.1:8080`。

## 协议与转发

- 三种生成协议不自动互转，按模型的 `supported_endpoints` 选接口。模型列表可用时，生成前会校验模型与上游路径，不匹配返回 400：模型不在列表中（含非 chat 模型、`auto` 等）提示 `模型 "x" 不存在或当前账号不可用`，模型存在但不支持该路径提示 `模型 "x" 不支持 API 路径 /xxx`，均不转发生成请求（获取 session 和模型列表仍可能访问上游）；旧模型无该字段按 Chat Completions 处理。模型路径依次取：`/v1/models` 展示的模型的 `supported_endpoints`；`/models` 中未展示的 chat 模型（可手动指定）取其 `supported_endpoints`；其余（未知模型）视为不存在；列表不可用时模型交上游判断。
- 被拒模型隐藏：生成请求遇到上游 400 且正文是 JSON、其 `error.code` 为 `model_not_supported` 或 `error.message` 含 `requested model is not supported`（不区分大小写；`code`、`message` 不是字符串时忽略该字段）时，该模型会从 `/v1/models` 中隐藏 6 小时（仅保存在内存，重启后清空），400 原样返回客户端；其他位置的同样文本或非 JSON 正文不算。拒绝后发起的请求返回 2xx，且在途期间没有新的拒绝时，立即恢复显示；拒绝前发起的旧请求不会撤销新的拒绝。代码见 `service.go`、`models.go` 的 `refuse`/`unrefuse`/`isRefused`。
- 上游的 HTTP 状态、生成响应和 SSE 原样转发（例外见下条）；请求取消传递给上游；生成请求体最大 100 MiB。
- Responses 流的输出项 id 统一：Copilot 的 `/responses` SSE 在同一输出项的每个事件里都给出新的 id（`output_item.added`、各 delta 的 `item_id`、`output_item.done` 的 `item.id` 及 `response.completed` 的 `output[i].id` 互不相同），Codex 按 added 的 id 开始、按 done 的 id 结束一项，会把同一条消息显示两次。因此仅对上游路径 `/responses` 且 `Content-Type` 为 `text/event-stream` 的响应，按 `output_index` 把后续事件及结束事件（`response.completed`/`incomplete`/`failed`）中的 id 改回该项 `output_item.added` 时的 id（项类型不一致时不改）。需改写的事件会重新编码 JSON（键顺序可能变化，数字与 `<`、`>`、`&` 保持原文）；id 已一致、非 JSON 数据（如 `[DONE]`）和其他事件逐字节透传。只输出完整事件，未完整的尾部在上游结束时原样写出。代码见 `itemids.go`、`request.go` 的 `streamResponse`。
- 无工具时去掉 `tool_choice`：Copilot 的 `/responses` 对设置了 `tool_choice` 却没有声明工具的请求直接返回 400（如 Codex 压缩上下文的请求会去掉 `tools` 但保留 `tool_choice`）。因此转发到上游 `/responses` 的请求，顶层 `tools` 缺失、为 `null` 或空数组时去掉顶层 `tool_choice`；改写时重新编码 JSON（键顺序可能变化，数字与 `<`、`>`、`&` 保持原文）。`tools` 为非空数组或其他类型、请求体不是合法 JSON 对象时原样转发；Chat Completions 与 `/v1/messages` 不做此处理。代码见 `request.go` 的 `withoutLoneToolChoice`。
- 失效输出项清理重试：Copilot `/responses` 的输出项 id（及 reasoning 的 `encrypted_content`）绑定在产生它的上游连接上，回放时若请求落到另一个账号或连接已变，上游返回 401 `input item does not belong to this connection`，或 400 `encrypted content ... could not be verified`（不区分大小写）。因此对上游 `/responses` 的此类响应，丢弃 `input` 中 `type` 为 `reasoning`、`item_reference` 的对象项，并删除其余对象项的顶层 `id`，在同一账号、session 与模型上重发一次（重发的请求体基于已去掉孤立 `tool_choice` 的请求体）。每个请求最多重试一次；无可清理内容或重试后仍失败时，返回上游响应：状态 401 改为 500（正文与头部不变，因服务前端对过多的 401 会封禁客户端 IP），400 保持不变；且不计账号错误、不冷却、不写风控记录（见 `pool.md`）。输出 Warn「上游拒绝了与连接绑定的输入项」，重试仍失败时再输出「清理输入项后上游仍拒绝」，均不含请求正文。Chat Completions 与 `/v1/messages` 不处理。代码见 `staleitems.go`。
- 不转发客户端认证信息、Cookie 和逐跳头；拒绝重定向及不在 GitHub/Copilot 白名单内的 session API 地址。
- 生成请求体必须是包含非空字符串 `model` 的 JSON 对象。直接使用单账号 `Service` 时先校验请求体，再获取 session；实际部署使用的 `Pool` 会先计算会话键、选账号并获取 session，再交给 `Service` 校验。因此无效请求仍可能访问上游，所有账号 session 获取失败时返回最后一次 session 错误，并按既有规则处理冷却和风控，而不是返回 400。
- `model` 键按 `encoding/json` 的规则不区分大小写匹配（如 `Model` 也被接受），请求体原样转发（`/responses` 的工具选择和失效项清理可能另行改写请求体）。
- 开启会话亲和时，生成请求无法得到会话键（没有显式会话 ID，也没有 user 消息，含请求体不是合法 JSON 对象的情况）返回 500，响应体为 `{"error":{"message":"无法计算模型缓存 key","type":"copilot_proxy_error"}}`，不选账号、不访问上游。规则见 `session-affinity.md`。

## 上游请求头与消息解析

消息解析只在请求体上做字节级扫描，不把整个请求体解析成通用对象；`X-Initiator`、`Copilot-Vision-Request`、与会话亲和键共用这套规则（代码见 `request.go`、`jsonscan.go`）。

- `X-Initiator`：取 `messages` 数组的最后一个对象项；`messages` 没有任何元素时，取 Responses 的 `input` 数组（即使为空）。该项的 `role` 为 `user`，且 `content` 不是"只含 `tool_result` 的内容块数组"时为 `user`，否则为 `agent`。两者都不是数组时，`input` 为字符串得 `user`，其余得 `agent`。
- `Copilot-Vision-Request: true`：请求体是合法 JSON，且任意深度的对象中键 `type` 的字符串取值为 `image`、`image_url` 或 `input_image` 时发送。键与取值按解码后的内容比较（`\u` 转义同样生效），取值大小写敏感；非法 JSON 不发送。
- 已接受的语义变化（与按通用 JSON 解析的旧实现不同）：
  1. 数组中的非对象元素（数字、字符串、`null` 等）被跳过，例如 `messages:[{"role":"user"},1]` 的 `X-Initiator` 为 `user`；
  2. 字段类型错误的消息或内容块整项按无效处理并跳过，例如 `"role": true` 的消息、`"text": 5` 的内容块，不影响同一数组中的其他项；这会影响 `X-Initiator` 与会话亲和键。例如 `messages:[{"role":"user","content":"a"},{"role":true}]` 的 `X-Initiator` 为 `user`；
  3. 重复的 `type` 键只要任一次出现的取值为图片类型即判为图片。通用解析只保留最后一个值，这里从严判断，避免用重复键绕过图片检测。
