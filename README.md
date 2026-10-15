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
凭证 JSON 只允许 `oauth_token`、`host`、`user` 和 `disabled` 字段（`disabled: true` 表示启动后不参与轮询，通常由管理接口写入）；每个凭证文件都应执行
`chmod 600` 限制文件权限，启动时对权限过宽的文件逐个告警。

### 多账号与凭证目录

每个凭证文件对应一个 GitHub Copilot 账号。服务启动时扫描 `<data_path>/credentials/` 目录，规则如下：

- 只扫描这一层，不递归进入子目录。
- 文件名为 `<GitHub 用户名>.json`。去掉 `.json` 后必须是合法的 GitHub 用户名：只能包含字母、数字，片段之间以单个连字符 `-` 或下划线 `_` 分隔（下划线用于 GHE.com 企业托管用户，如 `mona-cat_octo`），不能以分隔符开头或结尾，不能含连续分隔符，长度 1 到 100。不合法时启动失败，错误信息包含完整文件名。
- 用户名不区分大小写，因此 `Alice.json` 与 `alice.json` 视为重复账号，启动失败（即使两者的 token 不同）。
- 忽略的条目：以 `.` 开头的隐藏文件和临时文件、扩展名不是 `.json` 的文件、子目录（包括子目录中的 `.json` 文件）、指向目录的符号链接。指向普通文件的符号链接会被读取；悬空的符号链接会导致启动失败。
- 凭证 JSON 中的 `user` 字段可以省略；若非空，必须与文件名中的用户名相同（忽略大小写），否则启动失败。
- 账号按文件名的字节序排列，轮询顺序即此顺序。日志中的 `account` 为该下标，`user` 为文件名中的用户名。
- 目录不存在或其中没有任何凭证文件时启动失败；设置了 `management_key` 时例外：目录不存在会自动创建（权限 `0700`），允许没有任何账号，之后可通过控制台添加。
- `disabled` 为 `true` 的账号启动后不参与轮询，仍会出现在管理接口中，可通过控制台重新启用。
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

设置服务密钥后，除健康检查和管理接口外的请求均需要 `Authorization: Bearer <服务访问密钥>`，
也支持 Anthropic 客户端的 `x-api-key`。该密钥不是 GitHub token。

启用管理接口后，`/v0/management/*` 改用管理密钥（`Authorization: Bearer <management_key>`，见下文「Web 控制台与管理接口」），
服务访问密钥不能用于管理接口。控制台页面 `/management` 本身无需鉴权，页面中输入的管理密钥只随 API 请求发送。

| 本地接口 | 上游接口 | 说明 |
| --- | --- | --- |
| `GET /healthz` | 无 | 进程健康状态，不检查订阅 |
| `GET /v1/models`、`GET /models` | `/models` | 模型列表，保留端点等元数据，并补充 `auto` |
| `POST /v1/chat/completions`、`POST /chat/completions` | `/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses`、`POST /responses` | `/responses` | OpenAI Responses |
| `POST /v1/messages` | `/v1/messages` | Anthropic Messages |
| `GET /usage`、`GET /v1/usage` | GitHub `/copilot_internal/user` | 原始计划、额度和重置时间 |

Web 控制台与管理接口见下文「Web 控制台与管理接口」。

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

## 会话亲和

`session_affinity` 默认开启：同一客户端会话的生成请求（chat completions、responses、`/v1/messages`）固定使用同一账号，以提高上游提示词缓存的命中率。`session_affinity_ttl` 是绑定的有效期，默认 2 小时，每次使用都会续期。

- 会话信号按优先级取值：Responses 的 `prompt_cache_key`；`/v1/messages` 的 `metadata.user_id` 中的 Claude Code 会话 UUID（JSON 格式或旧格式均可）；两者都没有时，取第一条 user 消息的内容并结合 `model`。没有任何信号的请求不使用亲和。
- 内容兜底只看第一条 user 消息：开头完全相同的不同会话（例如都以同一句模板提问开始）会落到同一账号。这只影响上游缓存的命中率，不影响请求的正确性。
- 会话键由客户端密钥与信号的哈希组成，内存中不保存原始密钥或正文；未配置 `api_key` 时同样生效。
- 绑定的账号被禁用、删除、冷却或获取 session 失败时，请求改用其他账号，并把会话绑定到新账号。亲和命中时不推进轮询游标。
- 绑定表最多 4096 条，满时先清理过期项，仍满则淘汰最早过期的一条。绑定只保存在内存中，重启后清空。
- 模型列表与用量请求不使用会话亲和。

## 额度与请求统计

- `GET /v0/management/accounts` 为每个账号返回额度快照：计划名（如 Copilot Pro）、Premium requests 或 Credits、Chat messages（Copilot Free）、Additional Budget 的剩余比例与重置时间。
- 额度按账号的最近一次**尝试**节流：距上次尝试（无论成功、失败或超时）不足 2 小时时不会重复请求上游，因此失败的账号也不会被控制台的列表反复请求。列表接口只刷新超过 2 小时未尝试的账号，并发进行，总超时 10 秒。需要最新数据时，可在控制台点击账号的「刷新额度」强制刷新（忽略节流）。
- GitHub 对额度接口限流（429，或带 `Retry-After` 的 403）时保留旧数据并标记 `stale`；其他失败保留旧数据并返回 `quota_error`，超时显示“额度查询超时”。
- 调用方取消（如关闭页面）的额度查询不算一次尝试，下一次列表请求会重新查询。
- 请求统计按账号记录：分发到该账号的请求数（含用量与生成）、响应状态不低于 400 的错误数、最近使用时间。session 失败后切换账号时，只计入最终处理请求的账号；所有账号都失败时计入最后尝试的账号。客户端取消的请求不计入错误数。
- 额度与统计只保存在内存中，重启后清零。

## Web 控制台与管理接口

设置 `management_key`（且不同于 `api_key`）后启用控制台与管理接口。留空则两者都不存在：未设置 `api_key` 时，访问 `/management` 与 `/v0/management/*` 返回 404；
设置了 `api_key` 时，这些路径与其他未注册路径一样先经过服务访问密钥校验，未携带有效密钥返回 401，携带有效密钥则返回 404。

- 控制台：打开 `http://127.0.0.1:8080/management` 并输入管理密钥。密钥只保存在当前页面的内存中，刷新页面后需要重新输入。
- 「概览」显示可用账号数、累计请求、错误数和各账号的请求统计。
- 「账号与额度」显示每个账号的状态、额度进度与重置时间、请求统计；可刷新额度、启用或禁用、删除账号，也可以导入凭证 JSON 或通过 GitHub 设备登录新增账号。

管理接口位于 `/v0/management/`，鉴权方式为 `Authorization: Bearer <management_key>`。`api_key` 不能访问管理接口，`X-Api-Key` 也不被接受。

| 方法与路径 | 说明 |
| --- | --- |
| `GET /v0/management/accounts` | 账号列表（按轮询顺序），包含额度、请求统计与冷却时间，不含 OAuth token |
| `POST /v0/management/accounts` | 导入凭证，请求体 `{"credential":{"oauth_token":"..."}}`；校验后以 GitHub 登录名保存，并追加到列表末尾 |
| `PATCH /v0/management/accounts` | 启用或禁用账号，请求体 `{"id":"alice","disabled":true}` |
| `DELETE /v0/management/accounts?id=alice` | 删除账号及其凭证文件、本地缓存目录 |
| `POST /v0/management/quota/refresh` | 强制刷新指定账号的额度，请求体 `{"id":"alice"}` |
| `POST /v0/management/login` | 发起 GitHub 设备登录，返回 `state`、验证页地址与验证码 |
| `GET /v0/management/login/status?state=...` | 查询设备登录状态：`pending`、`success`（附带新账号 `id`）或 `error` |

- 账号 ID 为 GitHub 登录名的小写形式。导入或设备登录得到的凭证保存为 `<data_path>/credentials/<GitHub 登录名>.json`，权限 `0600`。登录名不合法时返回 400；凭证目录中已有同名文件（忽略大小写，即使是运行期间放入的）时返回 409（设备登录则状态为 `error`），原文件不被覆盖。
- 禁用状态写入凭证文件的 `disabled` 字段。写入失败时接口返回 500，内存中的状态保持不变。
- 新增、删除、禁用立即生效，无需重启。删除账号不影响正在处理的请求。
- 设备登录结束后，状态保留 10 分钟。
- 控制台页面带有 `Content-Security-Policy` 与 `X-Frame-Options: DENY`，禁止被其他站点嵌入。
- 管理密钥通过 HTTP 头发送。监听非本机地址时，应将服务放在启用 TLS 的反向代理之后。

控制台页面是构建产物 `web/dist/index.html`，经 `go:embed` 嵌入程序。修改 `web/` 源码后执行 `make web` 重新构建（需要 Node.js 与 npm）；`make build` 不依赖 Node，直接使用已提交的产物。

## 行为与边界

- OAuth token 换取短期 session，在内存和磁盘中复用，并按 session 的 `expires_at` 判断有效期（剩余不足 2 分钟时提前刷新）。
- 使用 magpie 的 VS Code Copilot Chat 身份头和 API 版本，按最后一条输入区分用户与工具轮次，并识别图片请求。
- 模型列表缓存在内存和磁盘中，默认有效期 2 小时，可通过 `models_cache_ttl` 调整；过滤内部和不可使用的模型，首次使用有条款的模型时尝试接受。
- `auto` 优先请求 `/auto`，再尝试 `/models/session`；若均不可用则选择账号可手动使用的模型。Auto 缓存最多 1 小时，拒绝后最多进行 3 次生成尝试。
- 上游的 HTTP 状态、生成响应和 SSE 原样转发；请求取消会传递给上游。生成请求体最大 16 MiB。
- 三种生成协议不自动互转。请根据 `supported_endpoints` 选择接口；模型列表可用时，服务会在生成前校验模型与上游路径，不匹配时返回 400。旧模型没有该字段时按 Chat Completions 处理；模型列表不可用时，手选模型仍交由上游判断，Auto 会重试读取列表，仍失败则返回错误。
- 用量接口返回原始 `quota_snapshots`，不把 Business/Enterprise 的共享额度解释成无限可用。
- 不包含 magpie 的桌面 UI、WSL 和钥匙串发现。通过管理接口增删、禁用账号无需重启；直接修改凭证目录中的文件后需重启。
- 多账号轮询：需要账号的请求按 round-robin 选择一个账号，顺序为凭证文件名的字节序；各账号的 session 与缓存相互独立。禁用的账号不参与选择；没有可选账号时，需要账号的请求返回 503。`/healthz`、未注册路由（404、405）和重定向不访问上游，由账号池直接处理，不依赖任何账号，也不推进轮询。
- 故障切换：若会话已绑定到某账号（见「会话亲和」），优先使用该账号；只有模型列表（`GET /v1/models`、`GET /models`）和生成请求（`POST` 的 chat completions、responses、`/v1/messages`）会在账号 session 获取失败（网络错误、上游错误响应或无效 session）时切换账号：该账号冷却 1 分钟，同一请求改用其他未尝试过的账号，已冷却的账号排在最后作为候选；每个账号在同一请求内最多尝试一次，全部失败时返回最后一个账号的错误。
- `GET /usage` 不需要 session，按轮询落到某个账号；它的失败不会切换账号。
- 生成请求的请求体校验在 session 获取之后执行，因此所有账号的 session 都失败时，无效请求体会收到 session 错误而不是 400。
- 模型列表是各账号独立获取的，生成请求不会固定到返回模型列表的那个账号。如果各账号的订阅不同，某个模型可能只在部分账号上可用；请求被分配到不支持该模型的账号时，服务会返回 400。
- 限流与冷却：以下情况都会使该账号冷却 1 分钟，且本请求不换号重试：
  - 生成接口直接返回的 429，原样流式透传给客户端；
  - 经错误处理路径透传的上游 429，例如 `/v1/models` 的模型列表请求，以及 Auto 的 `/models/session` 返回的 429。
- Auto 的 `/auto` 返回 429 时，服务会兜底改为请求 `/models/session`。兜底成功时客户端得到正常响应，账号不冷却；只有兜底也返回 429 时才透传并冷却。
- `GET /usage` 返回的 429 不会使账号冷却，原样返回给客户端。
- 客户端取消的请求不会使账号冷却，也不计入账号的错误数。冷却状态只保存在内存中，重启后清空。
- 普通生成请求失败后不会在其他账号上重新发送；仅 Auto 的明确模型/session 拒绝会触发有限重选。
- 不转发客户端的认证信息、Cookie 和逐跳头；拒绝重定向以及不在 GitHub/Copilot 白名单内的 session API 地址。

## Makefile 与验证

```sh
make build
make test
make test-race
make vet
make web   # 仅在修改 web/ 源码后需要，需要 Node.js 与 npm
```

缓存文件保存在 `<data_path>/cache`，凭证文件默认保存在 `<data_path>/credentials`。
缓存目录和缓存文件权限分别设为 `0700`、`0600`；日志目录仅在新建时设为 `0700`，
日志文件设为 `0600`。Windows 使用目录继承的默认 ACL，程序不显式修改 DACL。

自动化测试使用本地模拟 GitHub/Copilot，不需要真实凭证。
真实 GitHub 登录、订阅和各模型可用性需要使用自己的账号验证。

## 来源与服务条款

原项目 magpie 使用 MIT 许可证，版权及许可文本保留在 `LICENSE`。
Web 控制台（`web/`）改编自 cpa-github-copilot 项目的 Web 界面，其中额度相关代码改编自 CPA Management Center
（MIT 许可，版权归 Router-For.ME 所有，许可文本见 `LICENSES/Management-Center.txt`）。
本服务使用原 provider 的客户端身份头；这不是 GitHub 官方服务或受支持的公共 API。
请在使用前确认 GitHub Copilot 的服务条款、账号权限和额度政策。
上游接口或写死的客户端版本发生变化时，需要调整实现。
