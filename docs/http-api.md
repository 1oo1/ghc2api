# HTTP 接口

代码：`internal/copilot/service.go`（路由与转发）、`pool.go`（`Pool.Handler()` 承载业务与管理路由）、`pool_models.go`（Pool 的模型列表）、`inputitemids.go`（`/responses` 输入项 id 规整）、`geminitools.go`（Gemini 模型的工具 schema 适配）、`groktools.go`/`grokschema.go`/`grokreply.go`（Grok 模型 `/responses` 的工具适配：请求改写与回复还原）。

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
| `GET /v1/models`、`/models` | `/models` | 模型列表，保留端点等元数据；账号池下由逐个尝试的首个成功账号答复（见下） |
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

- 模型列表取首个成功账号：前提是各账号的模型相同，因此不做并集；各账号模型不同时，只返回首个成功账号的列表。账号池下 `GET /v1/models`、`/models` 按生成请求同样的规则逐个选账号（分档加 SWRR，排除禁用、冷却、耗尽与已试过的账号，不按模型过滤、无会话键）尝试，首个 2xx 的响应（状态码、头、正文）原样返回，不再访问其他账号；失败的账号换下一个，全部失败时原样返回最后一个失败账号的响应；没有可选账号时 503；某次尝试失败后发现客户端已取消则停止尝试、不写响应。生成请求只会发往列表里有该模型的账号（列表未知的账号不受限；都没有时交给账号返回 400）。细则见 `pool.md` 的「模型列表」与「按模型过滤」。单账号 `Service`（不经 Pool）只返回该账号自己的列表。
- 三种生成协议不自动互转，按模型的 `supported_endpoints` 选接口。模型列表可用时，生成前会校验模型与上游路径，不匹配返回 400：模型不在列表中（含非 chat 模型、`auto` 等）提示 `模型 "x" 不存在或当前账号不可用`，模型存在但不支持该路径提示 `模型 "x" 不支持 API 路径 /xxx`，均不转发生成请求（获取 session 和模型列表仍可能访问上游）；旧模型无该字段按 Chat Completions 处理。模型路径依次取：`/v1/models` 展示的模型的 `supported_endpoints`；`/models` 中未展示的 chat 模型（可手动指定）取其 `supported_endpoints`；其余（未知模型）视为不存在；列表不可用时模型交上游判断。
- 被拒模型隐藏：生成请求遇到上游 400 且正文是 JSON、其 `error.code` 为 `model_not_supported` 或 `error.message` 含 `requested model is not supported`（不区分大小写；`code`、`message` 不是字符串时忽略该字段）时，该模型会从 `/v1/models` 中隐藏 6 小时（仅保存在内存，重启后清空），400 原样返回客户端；其他位置的同样文本或非 JSON 正文不算。拒绝后发起的请求返回 2xx，且在途期间没有新的拒绝时，立即恢复显示；拒绝前发起的旧请求不会撤销新的拒绝。代码见 `service.go`、`models.go` 的 `refuse`/`unrefuse`/`isRefused`。
- 上游的 HTTP 状态、生成响应和 SSE 原样转发（例外见下条）；请求取消传递给上游；生成请求体最大 100 MiB。
- Responses 流的输出项 id 统一：Copilot 的 `/responses` SSE 在同一输出项的每个事件里都给出新的 id（`output_item.added`、各 delta 的 `item_id`、`output_item.done` 的 `item.id` 及 `response.completed` 的 `output[i].id` 互不相同），Codex 按 added 的 id 开始、按 done 的 id 结束一项，会把同一条消息显示两次。因此仅对上游路径 `/responses` 且 `Content-Type` 为 `text/event-stream` 的响应，按 `output_index` 把后续事件及结束事件（`response.completed`/`incomplete`/`failed`）中的 id 改回该项 `output_item.added` 时的 id（项类型不一致时不改）。需改写的事件会重新编码 JSON（键顺序可能变化，数字与 `<`、`>`、`&` 保持原文）；id 已一致、非 JSON 数据（如 `[DONE]`）和其他事件逐字节透传。只输出完整事件，未完整的尾部在上游结束时原样写出。代码见 `itemids.go`、`request.go` 的 `streamResponse`。
- 无工具时去掉 `tool_choice`：Copilot 的 `/responses` 对设置了 `tool_choice` 却没有声明工具的请求直接返回 400（如 Codex 压缩上下文的请求会去掉 `tools` 但保留 `tool_choice`）。因此转发到上游 `/responses` 的请求，顶层 `tools` 缺失、为 `null` 或空数组时去掉顶层 `tool_choice`；改写时重新编码 JSON（键顺序可能变化，数字与 `<`、`>`、`&` 保持原文）。`tools` 为非空数组或其他类型、请求体不是合法 JSON 对象时原样转发；Chat Completions 与 `/v1/messages` 不做此处理。代码见 `request.go` 的 `withoutLoneToolChoice`。
- 去掉 output token replay 的 `encrypted_content`：Codex 在 provider 名为 `OpenAI` 时开启 output token replay，请求顶层 `include` 会带 `output.encrypted_content`，`input` 中 message、function_call 等项会带 `encrypted_content`，Copilot 的 `/responses` 对此返回 400（`Invalid value: 'output.encrypted_content'`）；Codex 对非 OpenAI provider 本就会去掉这两样（只保留 reasoning 与 compaction 类项的 `encrypted_content`）。因此转发到上游 `/responses` 的请求：顶层 `include` 为字符串数组时，删除其中所有与 `output.encrypted_content` 精确相等的值（其余值保序，删空则去掉 `include` 键）；顶层 `input` 为数组时，删除对象项的顶层 `encrypted_content`，但 `type` 为 `reasoning`、`compaction`、`compaction_summary`、`context_compaction` 的项保留（缺 `type` 或 `type` 不是字符串的项照常删除）。改写时只重编码改动过的 `input` 项与顶层对象（键顺序可能变化，数字与 `<`、`>`、`&` 保持原文，未改动的项保留原文）。`include` 不是字符串数组时不改 `include`，`input` 不是数组时不改 `input`；两处都无改动或请求体不是合法 JSON 对象时整体原样转发；Chat Completions 与 `/v1/messages` 不做此处理。代码见 `outputreplay.go`。
- 规整输入项 id：Copilot 给输出项发的 id 可长达 450 字符（Base64，含 `+`、`/`，没有类型前缀），Codex 下一轮原样回传时，Copilot 会以空消息的 400 `{"error":{"message":"","code":"invalid_request_body"}}` 或 `string too long ... maximum length 64` 拒绝整个请求；别家厂商的 id 也可能含冒号等字符。因此转发到上游 `/responses` 的请求（在去掉 output replay 内容之后），只看顶层 `input` 数组中 `id` 为非空字符串的对象项：`id` 不超过 64 字节且只含 `[A-Za-z0-9_-]` 的不动；`type` 为 `reasoning` 且 `id` 含其他字符的整项删除（别家的 reasoning，其 `encrypted_content` 本服务也读不了）；否则 `id` 超过 64 字节时，`type` 缺失或属于 message、function_call、function_call_output、custom_tool_call(_output)、tool_search_call/output、computer_call_output、shell_call(_output)、apply_patch_call(_output)、mcp_approval_response、additional_tools、compaction 之类可省略 id 的删去 `id`，其余类型改为「类型前缀 + sha256 十六进制」截断到 64 字节（前缀：reasoning `rs_`、web_search_call `ws_`、file_search_call `fs_`、computer_call `cu_`、image_generation_call `ig_`、code_interpreter_call `ci_`、mcp_call `mcp_`、mcp_list_tools `mcpl_`、mcp_approval_request `mcpr_`；未知类型沿用 `id` 自带的前缀——第一个 `_` 在 1 到 4 字节内时——否则无前缀），同一 id 每次得到同一结果、不同 id 不会撞到同一结果；其余（不超过 64 字节但含其他字符）把每个其他字符换成 `_`。`call_id` 一律不动（调用与其输出靠它配对）。改写时只重编码改动过的 `input` 项与顶层对象（键顺序可能变化，数字与 `<`、`>`、`&` 保持原文，未改动的项保留原文）；无改动、请求体不是合法 JSON 对象或 `input` 不是数组时原样转发；Chat Completions 与 `/v1/messages` 不做此处理。代码见 `inputitemids.go`。
- Gemini 模型的工具 schema 适配：Copilot 的 Gemini 对某些工具 schema 整个请求返回 400 `invalid request body`。因此仅对 `/chat/completions` 且模型是 Gemini（`model` 取最后一个 `/` 之后的部分，转小写后以 `gemini` 开头）的请求，转发前先后做两件事：① 顶层 `tools` 为数组时，删去工具 schema 中 `required` 列出了其同级 `properties` 里没有的字符串条目（任意深度，删空则去掉 `required` 键，非字符串条目保留），同名工具（`function.name` 非空）只保留第一个，只替换 `tools` 数组原文（数字与 `<`、`>`、`&` 保持原文，请求体其余字节不变）；② 工具 schema（`function.parameters`、`parameters`、`input_schema`）中，分支可证明互不相交的对象分支 `anyOf` 原地改名为 `oneOf`（等长替换，其余字节包括排版都不动）。互不相交只认两种证明：一个分支要求的字段在另一个分支也必填且字符串取值（`enum`、`const`）不相交，或另一个分支以 `additionalProperties:false` 禁止该字段（且没有 `patternProperties`）；所有两两分支都要被证明，证明不了的（含可空联合如 string 或 null）保持 `anyOf`，同一 schema 里已有 `oneOf` 时也不改。没有可改的内容时逐字节原样转发；`/responses`、`/v1/messages` 与其他模型不做此处理。代码见 `geminitools.go`。
- Grok 模型的工具适配：Copilot 前面的 Grok（如 grok-4.7，走 `/responses`）对自定义工具（`type: custom`，如 Codex 的 apply_patch、code mode 的 exec）、namespace 工具（如 `mcp__codex_app`）和参数根节点是 `anyOf`/`oneOf`/`allOf` 的 function 只回裸的 400/422（`422 Unprocessable Entity`），Codex 每个带工具的轮次都失败。因此仅对 `/responses` 且模型是 Grok（`model` 取最后一个 `/` 之后的部分，转小写后以 `grok` 开头；同一 `/responses` 上的 GPT 不处理）的请求，转发前改写，做法同 magpie：
  - 顶层 `tools`：`custom` 当 `function` 发（`description` 为原描述加固定说明，`format` 为 grammar 时再附语法定义，`parameters` 是只有字符串 `input` 的 object）；`namespace` 在原位置换成其内部的 `function` 与 `custom`（其他类型丢弃），名字摊平为 `ns__name`（namespace 为空或 `functions` 时保持原名；超过 64 字节时取前 55 字节加 `_` 与 sha256 前 4 字节的十六进制）；`function` 的 `parameters` 根节点是联合时合并为 object（`allOf` 全部合并，`anyOf`/`oneOf` 的对象分支合并 properties，所有分支都要求的字段才必填，嵌套联合与局部 `$ref` 先折叠；联合分支的 `$ref` 若已在当前展开栈上（循环引用，如 `$defs.a` 的分支指回 `a`）不再展开，按不可解析的 `$ref` 丢弃该分支；另外每个工具的 `parameters` 做一次 `objectRoot`，最多展开 256 次（`objectRootRefBudget`）联合分支的 `$ref`，用尽后再遇到的 `$ref` 分支同样按不可解析处理，避免无环但扇出很大的 `$defs` 链指数展开。这两处都是相对 magpie 的加固，正常 schema 只展开个位数的 `$ref`，结果与 magpie 一致）；其他类型（web_search 等）不动。
  - `tool_choice`：`custom` 或 `function` 对象改为 `function`，有 `namespace` 时名字摊平并删去 `namespace`；`allowed_tools` 对其 `tools` 每项同样处理；字符串形式不动。
  - 顶层 `input`：`custom_tool_call` 变 `function_call`（`arguments` 为 `{"input": 原 input}`，删去 `input`、`namespace`、`id`），`custom_tool_call_output` 变 `function_call_output`（删去 `id`），带 `namespace` 的 `function_call` 名字摊平并删去 `namespace`。
  - 改写时只重编码改动过的项与顶层对象（键顺序可能变化，数字与 `<`、`>`、`&` 保持原文，未改动的项保留原文）；没有可改的内容、请求体不是合法 JSON 对象时原样转发。改写后（namespace 摊平后 `tools` 可能为空，如里面只有 function 与 custom 之外的工具）再按上面「无工具时去掉 `tool_choice`」的规则清理一次，Copilot 对有 `tool_choice` 却没有 `tools` 的请求回 400。失效输出项清理重试的请求体基于改写后的请求体。
  - 回复还原：请求里有改名或自定义调用时（含历史里的调用，即使本次没有再声明对应工具），2xx 回复里模型对它们的调用还原成客户端声明时的形式，错误状态的回复与没有这类调用的请求的回复逐字节透传。自定义调用变 `custom_tool_call`（`name` 为原名，`arguments` 换成 `input`，`id` 的 `fc_` 换成 `ctc_`，有 namespace 时带上）；SSE 里参数增量 `response.function_call_arguments.delta` 逐段解码为 `response.custom_tool_call_input.delta`（解码为空的整条丢弃），`...arguments.done` 变 `response.custom_tool_call_input.done`；namespace 里的 `function` 还原名字、加 `namespace` 与空的 `encrypted_function_args`（Codex 把缺这个列表的 namespace 调用当作已加密）。流式事件按 `output_index` 关联到 `output_item.added` 中被识别的调用，`response.*` 事件的 `response.output[]` 与非流式 JSON 回复（顶层 `object` 为 `response`）的 `output[]` 同样还原，JSON 回复改写后更新 `Content-Length`。非流式 JSON 回复只对不超过 16 MiB（包级变量 `grokJSONLimit`）的回复还原；超过时不还原，已读部分与剩余部分原样透传，状态码、响应头与 `Content-Length` 都不变。还原后的 JSON 回复分块写出（每次 `Write` 32 KiB），使生成响应的写超时逐块续期，慢客户端读大回复不会被一次整体写超时。这一步先于输出项 id 统一，且只输出完整事件、未完整的尾部在上游结束时原样写出。调用表只属于当次请求，不跨请求共享。
  - 不同步：tool search（`tool_search_call`）与 `additional_tools` 中的工具、magpie「自定义工具被拒后再学习」的机制（本项目只对 Copilot 的 Grok 固定改写）。
  代码见 `groktools.go`（请求改写）、`grokschema.go`（`objectRoot`）、`grokreply.go`（回复还原，`customInput`、`customStream`）。
- 失效输出项清理重试：Copilot `/responses` 的输出项 id（及 reasoning 的 `encrypted_content`）绑定在产生它的上游连接上，回放时若请求落到另一个账号或连接已变，上游返回 401 `input item does not belong to this connection`，或 400 `encrypted content ... could not be verified`（不区分大小写）。因此对上游 `/responses` 的此类响应，丢弃 `input` 中 `type` 为 `reasoning`、`item_reference` 的对象项，并删除其余对象项的顶层 `id`，在同一账号、session 与模型上重发一次（重发的请求体基于已去掉孤立 `tool_choice`、output replay 内容、规整过输入项 id 并经 Grok 工具适配改写的请求体，重试成功的回复同样还原）。每个请求最多重试一次；无可清理内容或重试后仍失败时，返回上游响应：状态 401 改为 500（正文与头部不变，因服务前端对过多的 401 会封禁客户端 IP），400 保持不变；且不计账号错误、不冷却、不写风控记录（见 `pool.md`）。输出 Warn「上游拒绝了与连接绑定的输入项」，重试仍失败时再输出「清理输入项后上游仍拒绝」，均不含请求正文。Chat Completions 与 `/v1/messages` 不处理。代码见 `staleitems.go`。
- 不转发客户端认证信息、Cookie 和逐跳头；拒绝重定向及不在 GitHub/Copilot 白名单内的 session API 地址。
- 生成请求体必须是包含非空字符串 `model` 的 JSON 对象。直接使用单账号 `Service` 时先校验请求体，再获取 session；实际部署使用的 `Pool` 会先读取一次请求体取 `model` 与会话键（`model` 用于按模型过滤账号，无效时不过滤）、选账号并获取 session，再交给 `Service` 校验。因此无效请求仍可能访问上游，所有账号 session 获取失败时返回最后一次 session 错误，并按既有规则处理冷却和风控，而不是返回 400。
- `model` 键按 `encoding/json` 的规则不区分大小写匹配（如 `Model` 也被接受），请求体原样转发（`/responses` 的工具选择、output token replay 的 `encrypted_content`、输入项 id 和失效项清理，Gemini 模型 `/chat/completions` 的工具 schema，以及 Grok 模型 `/responses` 的工具可能另行改写请求体）。
- 开启会话亲和时，生成请求无法得到会话键（没有显式会话 ID，也没有 user 消息，含请求体不是合法 JSON 对象的情况）返回 500，响应体为 `{"error":{"message":"无法计算模型缓存 key","type":"copilot_proxy_error"}}`，不选账号、不访问上游。规则见 `session-affinity.md`。

## 上游请求头与消息解析

消息解析只在请求体上做字节级扫描，不把整个请求体解析成通用对象；`X-Initiator`、`Copilot-Vision-Request`、与会话亲和键共用这套规则（代码见 `request.go`、`jsonscan.go`）。

- `X-Initiator`：取 `messages` 数组的最后一个对象项；`messages` 没有任何元素时，取 Responses 的 `input` 数组（即使为空）。该项的 `role` 为 `user`，且 `content` 不是"只含 `tool_result` 的内容块数组"时为 `user`，否则为 `agent`。两者都不是数组时，`input` 为字符串得 `user`，其余得 `agent`。
- `Copilot-Vision-Request: true`：请求体是合法 JSON，且任意深度的对象中键 `type` 的字符串取值为 `image`、`image_url` 或 `input_image` 时发送。键与取值按解码后的内容比较（`\u` 转义同样生效），取值大小写敏感；非法 JSON 不发送。
- 已接受的语义变化（与按通用 JSON 解析的旧实现不同）：
  1. 数组中的非对象元素（数字、字符串、`null` 等）被跳过，例如 `messages:[{"role":"user"},1]` 的 `X-Initiator` 为 `user`；
  2. 字段类型错误的消息或内容块整项按无效处理并跳过，例如 `"role": true` 的消息、`"text": 5` 的内容块，不影响同一数组中的其他项；这会影响 `X-Initiator` 与会话亲和键。例如 `messages:[{"role":"user","content":"a"},{"role":true}]` 的 `X-Initiator` 为 `user`；
  3. 重复的 `type` 键只要任一次出现的取值为图片类型即判为图片。通用解析只保留最后一个值，这里从严判断，避免用重复键绕过图片检测。
