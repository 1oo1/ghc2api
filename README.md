# ghc2api

从 magpie 的 GitHub Copilot provider 提取的独立 Go HTTP 服务，配置解析使用 `yaml.v3`。
当前实现以 magpie 上游提交 `250bdd5c` 为对照基线；源码定位与后续同步流程见
[`docs/magpie-copilot-sync.md`](docs/magpie-copilot-sync.md)。

## 配置与启动

需要 Go 1.23 或更新版本，以及有效的 GitHub Copilot OAuth 订阅。复制示例配置后，
填写 `github_token`、`credentials` 或 `editor_config` 其中一种凭证来源：

```sh
cp config.example.yaml config.yaml
chmod 600 config.yaml
# 编辑 config.yaml；真实凭证不要提交到版本库。
make build

# 默认读取当前目录的 config.yaml。
./bin/ghc2api

# 启动参数只支持 -c 指定配置文件。
./bin/ghc2api -c /path/to/config.yaml
```

`config.yaml` 是唯一运行配置来源，不再通过命令行 flag 或环境变量覆盖服务设置。
各字段的用途、默认值和安全要求见 [`config.example.yaml`](config.example.yaml) 中的中文说明。
服务只接受 GitHub OAuth token，并通过 OAuth 接口换取 Copilot session；不支持 Copilot CLI token。
凭证 JSON 只允许 `oauth_token`、`host` 和 `user` 字段；若使用 `credentials` 文件，也应执行
`chmod 600` 限制文件权限。

默认只监听 `127.0.0.1:8080`。监听非本机地址时，必须在配置中设置 `api_key`；
公网部署还需要反向代理和 TLS。`api_key` 是本服务的客户端访问密钥，不是 GitHub token。

日志以 JSON 格式写入标准错误，支持 `debug`、`info`、`warn`、`error` 四级。
`debug` 会记录脱敏后的完整上游 API 请求和响应正文，适合排查问题，但会增加内存和日志占用；
正文可能包含提示词、生成内容等业务数据，应限制日志访问并在排障后恢复为 `info`。
认证头、凭证字段和已知 token 值会脱敏。

## HTTP 接口

除健康检查外，设置服务密钥后均需要 `Authorization: Bearer <服务访问密钥>`，
也支持 Anthropic 客户端的 `x-api-key`。该密钥不是 GitHub token。

| 本地接口 | 上游接口 | 说明 |
| --- | --- | --- |
| `GET /healthz` | 无 | 进程健康状态，不检查订阅 |
| `GET /v1/models`、`GET /models` | `/models` | 模型列表，保留端点等元数据，并补充 `auto` |
| `POST /v1/chat/completions`、`POST /chat/completions` | `/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses`、`POST /responses` | `/responses` | OpenAI Responses |
| `POST /v1/messages` | `/v1/messages` | Anthropic Messages |
| `GET /usage`、`GET /v1/usage` | GitHub `/copilot_internal/user` | 原始计划、额度和重置时间 |

```sh
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer 服务访问密钥'

curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer 服务访问密钥' \
  -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

客户端 base URL 可以配置为 `http://127.0.0.1:8080/v1`；
Anthropic 客户端 base URL 为 `http://127.0.0.1:8080`。

## 行为与边界

- OAuth token 换取短期 session，剩余有效期超过 2 分钟时复用。
- 使用 magpie 的 VS Code Copilot Chat 身份头和 API 版本，按最后一条输入区分用户与工具轮次，并识别图片请求。
- 模型列表缓存 5 分钟；过滤内部和不可使用的模型，首次使用有条款的模型时尝试接受。
- `auto` 优先请求 `/auto`，再尝试 `/models/session`；若均不可用则选择账号可手动使用的模型。Auto 缓存最多 1 小时，拒绝后最多进行 3 次生成尝试。
- 上游的 HTTP 状态、生成响应和 SSE 原样转发；请求取消会传递给上游。生成请求体最大 16 MiB。
- 三种生成协议不自动互转。请根据 `supported_endpoints` 选择接口；旧模型没有该字段时按 Chat Completions 处理。Auto 也可能选到不支持当前接口的模型，上游会直接返回相应错误。
- 用量接口返回原始 `quota_snapshots`，不把 Business/Enterprise 的共享额度解释成无限可用。
- 单进程服务一个账号，不包含 magpie 的多账号切换、桌面 UI、WSL 和钥匙串发现。凭证变更后需重启。
- 不自动尝试其他账号，也不在普通生成请求失败后重新计费重试；仅 Auto 的明确模型/session 拒绝会触发有限重选。
- 不转发客户端的认证信息、Cookie 和逐跳头；拒绝重定向以及不在 GitHub/Copilot 白名单内的 session API 地址。

## Makefile 与验证

```sh
make build
make test
make test-race
make vet
```

自动化测试使用本地模拟 GitHub/Copilot，不需要真实凭证。
真实 GitHub 登录、订阅和各模型可用性需要使用自己的账号验证。

## 来源与服务条款

原项目 magpie 使用 MIT 许可证，版权及许可文本保留在 `LICENSE`。
本服务使用原 provider 的客户端身份头；这不是 GitHub 官方服务或受支持的公共 API。
请在使用前确认 GitHub Copilot 的服务条款、账号权限和额度政策。
上游接口或写死的客户端版本发生变化时，需要调整实现。
