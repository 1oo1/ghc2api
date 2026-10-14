# ghc2api

从 magpie 的 GitHub Copilot provider 提取的独立 Go HTTP 服务，配置解析使用 `yaml.v3`。
当前实现以 magpie 上游提交 `250bdd5c` 为对照基线；源码定位与后续同步流程见
[`docs/magpie-copilot-sync.md`](docs/magpie-copilot-sync.md)。

## 配置与启动

需要 Go 1.23 或更新版本，以及有效的 GitHub Copilot OAuth 订阅。复制示例配置后，
把每个账号的 OAuth 凭证 JSON 放到 `<data_path>/credentials/` 目录下，文件名为 `<GitHub 用户名>.json`：

```sh
cp config.example.yaml config.yaml
mkdir -p data/credentials
# 将凭证 JSON 写入 data/credentials/<GitHub 用户名>.json，并执行 chmod 600。
chmod 600 config.yaml
# 编辑 config.yaml；真实凭证不要提交到版本库。
make build

# 默认读取当前目录的 config.yaml。
./bin/ghc2api

# 启动参数只支持 -c 指定配置文件。
./bin/ghc2api -c /path/to/config.yaml
```

`config.yaml` 是唯一运行配置来源，不再通过命令行 flag 或环境变量覆盖服务设置。
`data_path` 和 `logs_path` 的相对路径以配置文件所在目录为基准；默认分别为 `./data` 和 `./logs`。
缓存写入 `<data_path>/cache`，并按账号隔离。各字段的用途、默认值和安全要求见
[`config.example.yaml`](config.example.yaml) 中的中文说明。
服务只接受 GitHub OAuth token，并通过 OAuth 接口换取 Copilot session；不支持 Copilot CLI token。
凭证 JSON 只允许 `oauth_token`、`host` 和 `user` 字段；每个凭证文件都应执行
`chmod 600` 限制文件权限，启动时对权限过宽的文件逐个告警。

### 多账号与凭证目录

每个凭证文件对应一个 GitHub Copilot 账号。服务启动时扫描 `<data_path>/credentials/` 目录，规则如下：

- 只扫描这一层，不递归进入子目录。
- 文件名为 `<GitHub 用户名>.json`。去掉 `.json` 后必须是合法的 GitHub 用户名：只能包含字母、数字和单个连字符，不能以连字符开头或结尾，不能含连续连字符，长度 1 到 39。不合法时启动失败，错误信息包含完整文件名。
- 用户名不区分大小写，因此 `Alice.json` 与 `alice.json` 视为重复账号，启动失败（即使两者的 token 不同）。
- 忽略的条目：以 `.` 开头的隐藏文件和临时文件、扩展名不是 `.json` 的文件、子目录（包括子目录中的 `.json` 文件）、指向目录的符号链接。指向普通文件的符号链接会被读取；悬空的符号链接会导致启动失败。
- 凭证 JSON 中的 `user` 字段可以省略；若非空，必须与文件名中的用户名相同（忽略大小写），否则启动失败。
- 账号按文件名的字节序排列，轮询顺序即此顺序。日志中的 `account` 为该下标，`user` 为文件名中的用户名。
- 目录不存在或其中没有任何凭证文件时启动失败。
- 同一账号（`host` 与 `oauth_token` 都相同）不能出现在两个文件中，否则启动失败。顶层 `host` 设置后会覆盖所有账号的 `host`。
- `api_key` 对所有账号生效（它是全局配置，不分账号）。
- 配置中已移除的 `credentials` 字段会被拒绝，启动时报"不是有效 YAML"。

默认只监听 `127.0.0.1:8080`。监听非本机地址时，必须在配置中设置 `api_key`；
公网部署还需要反向代理和 TLS。`api_key` 是本服务的客户端访问密钥，不是 GitHub token。

日志以 JSON 格式写入 `<logs_path>/ghc2api.log`，并同步输出到标准错误，支持
`debug`、`info`、`warn`、`error` 四级。默认单文件上限为 10 MiB，最多保留 5 个滚动备份；
可通过 `log_max_size_mb` 和 `log_max_backups` 调整（备份数设为 `0` 时不保留旧文件）。
同一个 `logs_path` 只应由一个进程使用，避免多进程同时滚动日志。
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

- OAuth token 换取短期 session，在内存和磁盘中复用，并按 session 的 `expires_at` 判断有效期（剩余不足 2 分钟时提前刷新）。
- 使用 magpie 的 VS Code Copilot Chat 身份头和 API 版本，按最后一条输入区分用户与工具轮次，并识别图片请求。
- 模型列表缓存在内存和磁盘中，默认有效期 2 小时，可通过 `models_cache_ttl` 调整；过滤内部和不可使用的模型，首次使用有条款的模型时尝试接受。
- `auto` 优先请求 `/auto`，再尝试 `/models/session`；若均不可用则选择账号可手动使用的模型。Auto 缓存最多 1 小时，拒绝后最多进行 3 次生成尝试。
- 上游的 HTTP 状态、生成响应和 SSE 原样转发；请求取消会传递给上游。生成请求体最大 16 MiB。
- 三种生成协议不自动互转。请根据 `supported_endpoints` 选择接口；模型列表可用时，服务会在生成前校验模型与上游路径，不匹配时返回 400。旧模型没有该字段时按 Chat Completions 处理；模型列表不可用时，手选模型仍交由上游判断，Auto 会重试读取列表，仍失败则返回错误。
- 用量接口返回原始 `quota_snapshots`，不把 Business/Enterprise 的共享额度解释成无限可用。
- 不包含 magpie 的桌面 UI、WSL 和钥匙串发现。凭证目录中的文件增删或内容变更后需重启。
- 多账号轮询：需要账号的请求按 round-robin 选择一个账号，顺序为凭证文件名的字节序；各账号的 session 与缓存相互独立。`/healthz`、未注册路由（404、405）和重定向不访问上游，由第一个账号的路由直接处理，也不推进轮询。
- 故障切换：只有模型列表（`GET /v1/models`、`GET /models`）和生成请求（`POST` 的 chat completions、responses、`/v1/messages`）会在账号 session 获取失败（网络错误、上游错误响应或无效 session）时切换账号：该账号冷却 1 分钟，同一请求改用其他未尝试过的账号，已冷却的账号排在最后作为候选；每个账号在同一请求内最多尝试一次，全部失败时返回最后一个账号的错误。
- `GET /usage` 不需要 session，按轮询落到某个账号；它的失败不会切换账号。
- 生成请求的请求体校验在 session 获取之后执行，因此所有账号的 session 都失败时，无效请求体会收到 session 错误而不是 400。
- 模型列表是各账号独立获取的，生成请求不会固定到返回模型列表的那个账号。如果各账号的订阅不同，某个模型可能只在部分账号上可用；请求被轮询到不支持该模型的账号时，服务会返回 400。
- 限流与冷却：以下情况都会使该账号冷却 1 分钟，且本请求不换号重试：
  - 生成接口直接返回的 429，原样流式透传给客户端；
  - 经错误处理路径透传的上游 429，例如 `/v1/models` 的模型列表请求，以及 Auto 的 `/models/session` 返回的 429。
- Auto 的 `/auto` 返回 429 时，服务会兜底改为请求 `/models/session`。兜底成功时客户端得到正常响应，账号不冷却；只有兜底也返回 429 时才透传并冷却。
- `GET /usage` 返回的 429 不会使账号冷却，原样返回给客户端。
- 客户端取消的请求不会使账号冷却。冷却状态只保存在内存中，重启后清空。
- 普通生成请求失败后不会在其他账号上重新发送；仅 Auto 的明确模型/session 拒绝会触发有限重选。
- 不转发客户端的认证信息、Cookie 和逐跳头；拒绝重定向以及不在 GitHub/Copilot 白名单内的 session API 地址。

## Makefile 与验证

```sh
make build
make test
make test-race
make vet
```

缓存文件保存在 `<data_path>/cache`，凭证文件默认保存在 `<data_path>/credentials`。
缓存目录和缓存文件权限分别设为 `0700`、`0600`；日志目录仅在新建时设为 `0700`，
日志文件设为 `0600`。Windows 使用目录继承的默认 ACL，程序不显式修改 DACL。

自动化测试使用本地模拟 GitHub/Copilot，不需要真实凭证。
真实 GitHub 登录、订阅和各模型可用性需要使用自己的账号验证。

## 来源与服务条款

原项目 magpie 使用 MIT 许可证，版权及许可文本保留在 `LICENSE`。
本服务使用原 provider 的客户端身份头；这不是 GitHub 官方服务或受支持的公共 API。
请在使用前确认 GitHub Copilot 的服务条款、账号权限和额度政策。
上游接口或写死的客户端版本发生变化时，需要调整实现。
