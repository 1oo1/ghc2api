# HTTP 接口

代码：`internal/copilot/service.go`（路由与转发）、`pool.go`（`Pool.Handler()` 承载业务与管理路由）。

## 鉴权

- 设置 `api_key` 后，除健康检查和管理接口外的请求需 `Authorization: Bearer <api_key>`，也支持 Anthropic 客户端的 `x-api-key`。
- `/v0/management/*` 改用管理密钥，`api_key` 不能访问；详见 `management.md`。

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
- 上游的 HTTP 状态、生成响应和 SSE 原样转发；请求取消传递给上游；生成请求体最大 16 MiB。
- 不转发客户端认证信息、Cookie 和逐跳头；拒绝重定向及不在 GitHub/Copilot 白名单内的 session API 地址。
- 生成请求体校验在 session 获取之后，故所有账号 session 都失败时，无效请求体收到 session 错误而非 400。
