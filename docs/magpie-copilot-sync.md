# 与 magpie 上游同步 GitHub Copilot provider

本项目是从 magpie 拆分的独立服务，不是 magpie 的子模块或自动镜像。同步时以
[magpie GitHub 仓库](https://github.com/yetone/magpie) 中实际跟踪的源码、测试和提交历史为准。

当前实现对照的上游基线：`19e9fc47f0c9cc8e30edf9881968e8cff82f072f`。
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
BASE=19e9fc47f0c9cc8e30edf9881968e8cff82f072f
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
| `internal/copilot/service.go`、`request.go` | provider 请求签名与 `internal/gateway/` 的转发、协议和流式行为 |

只同步本服务已经支持的功能。magpie 的多账号管理、桌面 UI、agent 集成等不属于本项目的
provider 代理范围；不要整文件复制，也不要为追求代码结构一致而引入这些依赖。

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
git check-ignore .local/magpie
```

检查 diff，确认未带入无关 provider 行为或凭证数据。所有验证通过后，将本文开头的基线、
上方核对命令中的 `BASE` 和 README 中的短提交号更新为本次实际核对的上游提交；保留本项目
独立实现，不在 `.local/` 中直接修改或提交上游代码。
