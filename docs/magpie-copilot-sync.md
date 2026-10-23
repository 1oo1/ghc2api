# 与 magpie 上游同步 GitHub Copilot provider

本项目是从 magpie 拆分的独立服务，不是 magpie 的子模块或自动镜像。同步时以
[magpie GitHub 仓库](https://github.com/yetone/magpie) 中实际跟踪的源码、测试和提交历史为准。

每次同步 magpie 时，还要按下文「同步 CLIProxyAPI 会话亲和键」核对
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（下称 cpa）的会话 ID 提取写法，
两者的基线分别记录。

当前实现对照的上游基线：`ea6be1592706ea4a0dd6cd526acec0b5d1fb967e`。
完成后续同步并通过验证时，应将本文和 README 中记录的基线更新为实际核对的上游提交。

## 获取或更新上游

首次获取时，在项目根目录执行：

```sh
mkdir -p .local
git clone https://github.com/yetone/magpie.git .local/magpie
```

`.local/` 已加入本项目的 `.gitignore`，其中的克隆、构建文件和临时检查不会进入本项目的
提交。

以后复用已有克隆时，先检查工作区和远端；确认工作区干净且远端是上述 GitHub 仓库后，再更新。
若工作区不干净，先自行检查，不要覆盖或清理：

```sh
git -C .local/magpie status --short --branch
git -C .local/magpie remote get-url origin
```

确认后同步到远端默认分支的最新提交：

```sh
git -C .local/magpie fetch --prune origin &&
git -C .local/magpie remote set-head origin --auto &&
git -C .local/magpie switch --detach origin/HEAD &&
git -C .local/magpie log -1 --format='%H %cs %s'
```

`git clone` 默认检出 GitHub 仓库的默认分支；更新时显式检出 `origin/HEAD`，避免当前分支
或其 upstream 指向旧分支。这里使用 detached HEAD 只读核对上游，不在克隆中开发。记录最后
一条命令输出的完整提交号，后续比较时使用它，不要只依赖“最新版”或分支名。

## 核对上游变化

从上次同步记录的提交开始查看相关差异。首次核对可用本文开头的基线：

```sh
BASE=ea6be1592706ea4a0dd6cd526acec0b5d1fb967e
git -C .local/magpie diff --name-status "${BASE}..HEAD"
git -C .local/magpie diff "${BASE}..HEAD" -- \
  internal/provider/account.go \
  internal/provider/account_test.go \
  'internal/provider/copilot_*.go' \
  internal/gateway \
  internal/catalog
```

差异路径只是入口，不是完整清单。上游可能把 Copilot 逻辑放在通用 provider 或 gateway 中，
也可能调整文件布局；先看完整文件变更清单，再跨目录检索当前源码和相关历史，沿调用关系
阅读改动及测试：

```sh
git -C .local/magpie grep -n -i -E \
  'copilot|copilot_internal|Copilot-Session-Token|supported_endpoints' \
  HEAD -- '*.go' '*.ts' '*.tsx' '*.js'
git -C .local/magpie log --oneline --all --regexp-ignore-case --grep='copilot'
```

重点关注以下上游代码和测试；文件名会随上游演进，以检索结果为准：

- `internal/provider/account.go`：Copilot provider 接入、session 交换、请求头、模型和条款处理。
- `internal/provider/copilot_signin.go`、`internal/provider/copilot_accounts.go`：OAuth 登录与 magpie 的 Copilot 账号管理。
- 编辑器凭证发现也需沿 `internal/provider/account.go` 查找；本项目仅同步 OAuth token 流程。
- `internal/provider/copilot_ghe.go`：GitHub Enterprise Cloud 主机行为。
- `internal/provider/copilot_auto.go`、`internal/provider/copilot_refused.go`、`internal/provider/copilot_lapsed.go`：Auto 选择、拒绝后的处理和失效状态。
- `internal/provider/copilot_*_test.go`、`internal/provider/account_test.go`、
  `internal/gateway/copilot_*_test.go`：预期行为、回归案例及 gateway 侧处理。
- `internal/gateway/`、`internal/catalog/`：通用请求转发、协议/模型元数据路径；不一定有
  Copilot 专属文件名。

## 映射到本项目

阅读当前实现和其测试后，再把上游行为映射到本项目的职责边界。下面是定位起点，不表示
两边代码一一对应：

| 本项目 | 重点核对的上游职责 |
| --- | --- |
| `internal/copilot/login.go`、`credentials.go` | `internal/provider/copilot_signin.go`、`copilot_accounts.go`、`account.go` 中的 OAuth 登录和凭证行为 |
| `internal/copilot/config.go`、`session.go` | `internal/provider/account.go`、`copilot_ghe.go` 中的主机、OAuth session 和身份头行为 |
| `internal/copilot/models.go`、`auto.go` | `internal/provider/account.go`、`copilot_auto.go` 及相关模型数据和测试 |
| `internal/copilot/service.go`、`request.go`、`itemids.go` | provider 请求签名与 `internal/gateway/` 的转发、协议和流式行为（含 `itemids.go` 的 Responses 项 id 统一） |

只同步本服务已经支持的功能。magpie 的多账号管理、桌面 UI、agent 集成等不属于本项目的
provider 代理范围；不要整文件复制，也不要为追求代码结构一致而引入这些依赖。

## 同步 CLIProxyAPI 会话亲和键

本项目会话键（`sessionKey`，见 `session-affinity.md`）的会话 ID 来源与优先级参照 cpa。cpa 的
基线：`0db19025571684b198a2ef508f99b41c717337e8`（2026-10-21）。

首次获取与更新方式同 magpie，克隆到 `.local/cpa`：

```sh
git clone https://github.com/router-for-me/CLIProxyAPI.git .local/cpa
git -C .local/cpa status --short --branch
git -C .local/cpa remote get-url origin
git -C .local/cpa fetch --prune origin &&
git -C .local/cpa remote set-head origin --auto &&
git -C .local/cpa switch --detach origin/HEAD &&
git -C .local/cpa log -1 --format='%H %cs %s'
```

核对差异：

```sh
CPA_BASE=0db19025571684b198a2ef508f99b41c717337e8
git -C .local/cpa diff "${CPA_BASE}..HEAD" -- \
  sdk/cliproxy/session/info.go \
  sdk/cliproxy/session/identity.go \
  sdk/cliproxy/session/info_test.go \
  sdk/cliproxy/auth/selector.go \
  internal/config/config_types.go \
  config.example.yaml
git -C .local/cpa grep -n -i -E \
  'session-affinity|ExtractSessionInfo|NormalizeExplicitID|X-Session-Affinity|X-Slot-Session-Id' \
  HEAD -- '*.go' '*.yaml'
```

重点位置（文件名以检索结果为准）：

- `sdk/cliproxy/session/info.go` 的 `ExtractSessionInfo`：会话 ID 的来源与优先级，函数注释列出了顺序。
  这是同步的主要对象。
- `sdk/cliproxy/session/identity.go`：`NormalizeExplicitID`（ID 规范化）、`ClaudeMetadataIdentities`
  （Claude Code `metadata.user_id` 解析）、`hasExplicitSession`（显式信号清单，可用来交叉核对）。
- `sdk/cliproxy/auth/selector.go` 的 `SessionAffinitySelector.Pick`、`extractSessionIDs`：绑定、兜底与
  父子会话继承。
- `internal/config/config_types.go`、`config.example.yaml` 中 `session-affinity` 的注释：cpa 对支持
  客户端的概述（目前为 Claude Code、Codex、OpenCode、pi）。
- `internal/runtime/executor/codex_executor_reasoning.go` 的 `codexReasoningReplaySessionKey*` 是
  Codex reasoning 回放缓存的键，不是账号亲和键；本项目没有 reasoning 回放，只在其新增客户端会话
  请求头时参考。

本项目的对应实现是 `internal/copilot/affinity_key.go`（`sessionSignal` 及其来源表），测试在
`affinity_key_test.go`。同步内容与刻意不同步的部分：

| cpa 行为 | 本项目 |
| --- | --- |
| 显式会话 ID 的来源、顺序与前缀（`claude:`、`codex:`、`header:`、`affinity:`、`slot:`、`pck:` 等） | 同步，前缀与 cpa 一致 |
| （cpa 无此来源） | 本项目扩展：通用请求头末尾追加 `X-Agent-Task-Id` → `agenttask:`、`X-Request-Id` → `req:`，同步 cpa 时保留 |
| `NormalizeExplicitID`：拒绝控制字符，裁剪空白，限 256 字节 | 同步 |
| 父会话、子代理、fork 识别与绑定继承 | 不同步。本项目绑定表是扁平的；Codex 取 `Session-Id` 优先于 `Thread-Id`，Claude Code 子代理与主会话共用会话 ID，因此子代理自然落到主会话账号 |
| Gemini `cachedContent`、请求体外层 `request` 包装 | 不同步，本项目不接收 Gemini 协议 |
| `execution_session_id`、LCP 前缀匹配、`derived` 会话指纹 | 不同步，保留本项目的「首条 user 消息」兜底 |
| 绑定键含 provider 与 model | 不同步，键只由会话信号决定，不含客户端密钥与 model |
| gjson 宽松解析：数字等非字符串字段转成字符串；未闭合或带尾随内容的 `metadata.user_id` JSON 仍可取出 `session_id`；重复键取第一个 | `encoding/json` 严格解析：只接受 JSON 字符串；畸形 `user_id` JSON 不识别为 Claude 会话（落到 `user:` 或兜底）；重复键取最后一个 |

cpa 新增或调整会话来源（请求头、请求体字段、优先级、规范化）时，先在 `affinity_key_test.go`
补测试，再修改来源表，并同步 `session-affinity.md`「会话键」。会话键变化会使已持久化的绑定失效，
属可接受的缓存语义（旧绑定按 TTL 过期）。

## 修改与验证

先把上游差异归纳为具体行为及对应测试，再决定是否影响本项目。按本项目的开发约束先补充或
调整测试，再实现必要的改动；优先使用本地模拟服务验证，不使用真实账号/token 做自动化测试。
至少核对：

- OAuth token 换取 session、有效期与刷新。
- session 返回的 API 主机、企业域名校验和重定向/请求头安全边界。
- 模型列表元数据、协议端点、模型条款和账号权限差异。
- Auto/session 的 API 版本、模型选择、缓存、拒绝后的有限重试与手动 fallback。
- 请求身份头、用户/工具轮次、图片请求，以及 SSE 转发和请求取消。

同步完成后运行：

```sh
go test -race ./...
go vet ./...
go build -o .local/ghc2api-verify ./cmd/ghc2api
git check-ignore .local/magpie .local/cpa
```

检查 diff，确认未带入无关 provider 行为或凭证数据。所有验证通过后，将本文开头的基线、
上方核对命令中的 `BASE` 和 README 中的短提交号更新为本次实际核对的上游提交；cpa 同理更新
「同步 CLIProxyAPI 会话亲和键」中的基线、`CPA_BASE` 和 README 中的短提交号；保留本项目
独立实现，不在 `.local/` 中直接修改或提交上游代码。
