# ghc2api

从 magpie 的 GitHub Copilot provider 提取的独立 Go HTTP 服务，仅依赖标准库。
当前实现以 magpie 上游提交 `250bdd5c` 为对照基线；源码定位与后续同步流程见
[`docs/magpie-copilot-sync.md`](docs/magpie-copilot-sync.md)。

## 启动

需要 Go 1.23 或更新版本，以及有效的 GitHub Copilot 订阅。

```sh
go build -o ghc2api ./cmd/ghc2api

# 使用 GitHub device flow 登录；凭证写入当前目录，权限为 0600。
./ghc2api login --credentials ./credentials.json

# 默认只监听 127.0.0.1:8080。
GHC2API_API_KEY='自行设置的服务访问密钥' ./ghc2api serve --credentials ./credentials.json
```

也可以直接配置 token，不进行 device flow：

```sh
GITHUB_TOKEN='GitHub OAuth token' GHC2API_API_KEY='服务访问密钥' ./ghc2api serve

# Copilot CLI 的 token 必须直接使用，不能交换 session。
GITHUB_TOKEN='Copilot CLI token' COPILOT_TOKEN_MODE=cli ./ghc2api serve

# 显式从编辑器的 apps.json / hosts.json 导入；不修改原文件。
./ghc2api serve --editor-config "$HOME/.config"
```

企业账号可增加 `--host acme.ghe.com`，或设置 `COPILOT_HOST`。
只有 `github.com` 和单层 `<name>.ghe.com` 域名可用于登录。
CLI token 的系统钥匙串读取不在本服务内，请显式提供 token。

配置优先级：显式环境 token、凭证文件、显式指定的编辑器配置目录。
不要把真实 token 写入命令行参数、提交到版本库或放入日志。
监听非本机地址时必须设置 `GHC2API_API_KEY`；公网部署还需要反向代理和 TLS。

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

- OAuth token 换取短期 session，剩余有效期超过 2 分钟时复用；CLI token 直接使用，端点信息缓存 30 分钟。
- 使用 magpie 的 VS Code/Copilot CLI 身份头和 API 版本，按最后一条输入区分用户与工具轮次，并识别图片请求。
- 模型列表缓存 5 分钟；过滤内部和不可使用的模型，首次使用有条款的模型时尝试接受。
- `auto` 优先请求 `/auto`，再尝试 `/models/session`；若均不可用则选择账号可手动使用的模型。Auto 缓存最多 1 小时，拒绝后最多进行 3 次生成尝试。
- 上游的 HTTP 状态、生成响应和 SSE 原样转发；请求取消会传递给上游。生成请求体最大 16 MiB。
- 三种生成协议不自动互转。请根据 `supported_endpoints` 选择接口；旧模型没有该字段时按 Chat Completions 处理。Auto 也可能选到不支持当前接口的模型，上游会直接返回相应错误。
- 用量接口返回原始 `quota_snapshots`，不把 Business/Enterprise 的共享额度解释成无限可用。
- 单进程服务一个账号，不包含 magpie 的多账号切换、桌面 UI、WSL 和钥匙串发现。凭证变更后需重启。
- 不自动尝试其他账号，也不在普通生成请求失败后重新计费重试；仅 Auto 的明确模型/session 拒绝会触发有限重选。
- 不转发客户端的认证信息、Cookie 和逐跳头；拒绝重定向以及不在 GitHub/Copilot 白名单内的 session API 地址。

## 验证

```sh
go test -race ./...
go vet ./...
go build ./cmd/ghc2api
```

自动化测试使用本地模拟 GitHub/Copilot，不需要真实凭证。
真实 GitHub 登录、订阅和各模型可用性需要使用自己的账号验证。

## 来源与服务条款

原项目 magpie 使用 MIT 许可证，版权及许可文本保留在 `LICENSE`。
本服务使用原 provider 的客户端身份头；这不是 GitHub 官方服务或受支持的公共 API。
请在使用前确认 GitHub Copilot 的服务条款、账号权限和额度政策。
上游接口或写死的客户端版本发生变化时，需要调整实现。
