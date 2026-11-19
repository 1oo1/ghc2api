# 与 magpie 上游同步 GitHub Copilot provider

本项目是从 magpie 拆分的独立服务，不是 magpie 的子模块或自动镜像。同步时以
[magpie GitHub 仓库](https://github.com/yetone/magpie) 中实际跟踪的源码、测试和提交历史为准。

每次同步 magpie 时，还要按下文「同步 CLIProxyAPI 会话亲和键」核对
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（下称 cpa）的会话 ID 提取写法，
并按「同步 VS Code 的 Credits 与 Additional Budget」核对 [VS Code](https://github.com/microsoft/vscode)
的额度解析与展示，三者的基线分别记录。

当前实现对照的上游基线：`3869aee3b0e68cbda626853c99565e4e33f962dc`（2026-11-19）。
完成后续同步并通过验证时，应将本文和 README 中记录的基线更新为实际核对的上游提交。

## 已删除的 Auto

本项目已有意删除 magpie 的 Auto 模型功能（本项目不使用）：`model` 必须是具体模型，不再有 `auto`
条目、`/auto` 与 `/models/session` 请求、Auto 选择以及拒绝后的重选。同步时跳过上游的
`copilot_auto.go`，以及其他文件中的 Auto 相关改动（选择、拒绝后重选、`/auto`、`/models/session`），不要移植回来。

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
BASE=3869aee3b0e68cbda626853c99565e4e33f962dc
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
- `internal/provider/copilot_refused.go`、`internal/provider/copilot_lapsed.go`：拒绝后的处理和失效状态。`copilot_auto.go` 是 Auto 功能，本项目已有意删除，同步时跳过。
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
| `internal/copilot/models.go` | `internal/provider/account.go` 的 `copilotModels` 及相关模型数据和测试（不含 Auto 相关部分）。展示规则同步 #4343（picker 启用的 `Experimental` 模型可见）；同步 767029501 的 `GET /models` 使用 API 版本 `2026-08-01`（上游 `copilotModelsVersion`，本项目 `modelsAPIVersion`，见 `upstream.md`「请求身份」）；不同步图片模型（`Draws`，本项目不提供 `/images`）、把 `max_prompt_tokens` 当窗口与按 `billing.token_prices` 计算窗口/价格（`copilotTokenPrices.apply`，模型元数据原样透传，由客户端自行解读） |
| `internal/copilot/service.go`、`request.go`、`itemids.go`、`outputreplay.go` | provider 请求签名与 `internal/gateway/` 的转发、协议和流式行为（含 `itemids.go` 的 Responses 项 id 统一、`outputreplay.go` 对应 `codex_backend.go` 的 `forVendor` 中去掉 output token replay 的部分） |
| `internal/copilot/inputitemids.go` | `internal/gateway/codex_backend.go` 的 `plainItemIDs`（含 `itemIDCap`、`boundItemID`、`idOptional`、`itemKindPrefix`、`openaiItemPrefix`），在 `service.go` 的 `handleGeneration` 中于 `withoutOutputReplay` 之后调用。不同步 `boundCallIDs`（`call_id` 超长，仅 ChatGPT 后端）与 `callItemIDs`（`fc_` 改 `tsc_`/`ctc_`） |
| `internal/copilot/geminitools.go`、`geminishapes.go`、`geminitypes.go`、`jsontree.go` | `internal/gateway/copilot_gemini_required.go` 的 `copilotGeminiToolsFit`、`copilot_gemini_shapes.go` 的 `copilotGeminiToolShapes`、`gemini_schema.go` 的 `geminiTypes`（`keepUnions` 固定为 true，即非根联合节点不补 `type`）与 `geminiSchemaShapes`、`geminiNamed`，以及 `kimi_schema.go` 的类型推断辅助函数（`schemaTypes`、`keywordType`、`valuesType` 等），调用顺序同 `gateway.go`（先 ToolsFit、再 ToolShapes、最后 geminiTypes）。差异与已知缺口见下文「Gemini 工具 schema」；Grok 部分见下一行 |
| `internal/copilot/groktools.go`、`grokschema.go`、`grokreply.go` | `internal/gateway/copilot_gemini_schema.go` 的 `copilotGrokFlat`、`grok_integral.go` 的 `grokModel`、`gateway.go` 中 Copilot Grok 有自定义工具时翻译的分支；`internal/provider/grok.go` 的 `GrokFlatBody`、`grokFlat`、`flatCall`、`FlatName`、`liteNamespace`、`ObjectRoot`（含 `objectRootIn`、`nestedUnion`、`appendDistinct`、`grokRef`）；`internal/gateway/responses.go` 的 `customSchema`、`customDescription`、`customInput`、`callTo`、`itemPrefix`，工具提供、历史项转换与流式事件发出；`custom_tool.go` 的 `customStream`；`grok_namespace.go` 的 `namespacedIn`、`nsTidy`。magpie 有翻译层，自定义工具当 function、namespace 摊平、根联合合并后，模型的调用在翻译层还原；本项目没有翻译层，改为请求改写（`grokRequest`）加回复还原（`grokRestorer`，SSE 与 JSON，先于 `itemids.go` 的 id 统一）。不同步 tool search（`tool_search_call`）与 `additional_tools` 中的工具、「自定义工具被拒后再学习」的机制（本项目对 Copilot 的 Grok 固定改写） |
| `internal/copilot/imagecap.go` | `internal/gateway/image_cap.go`（afd2df09b）：Copilot Gemini 图片超限时从明确的 400 学习账号、模型上限，替换最早的历史图片后重试；只处理本项目的 `/chat/completions`，不引入其他供应商或用户自定义上限。 |
| `internal/copilot/pool.go`（`peekGeneration`、`chooseFor`）、`pool_models.go`、`models.go` 的 `modelAvailability` | `internal/provider/copilot_pool.go`（b9a1b4aa1）：只同步生成请求按模型过滤账号（只发往有该模型的账号）；模型列表不取并集（`/models` 逐个尝试、首个成功即返回，见 `pool.md`「模型列表」）。只借用语义，实现按本项目结构；不同步 24h 节流与桌面账号管理 |

只同步本服务已经支持的功能。magpie 的多账号管理、桌面 UI、agent 集成等不属于本项目的
provider 代理范围；不要整文件复制，也不要为追求代码结构一致而引入这些依赖。

### 已核对但不同步的上游提交

- 62ced98cc（`gateway/gemini_level_body.go` 的 `geminiLevelBody`）：在非 Gemini 官方的 Chat/Anthropic 上游上，把 Gemini 3+ 的 thinking budget 改写为 thinking level。动机是自建中转把 budget 原样转给 Gemini 后被拒，没有 Copilot 拒绝 budget 的证据；本项目也不做协议翻译。出现 Copilot 拒绝的实例后再评估。
- 1e2287758（Copilot Responses WebSocket 增量续传）：上游在模型声明 `ws:/responses` 时复用同一 WebSocket，仅发送上一响应后的增量。本项目仍使用 HTTP/SSE；新增传输需要单独处理按账号、会话和出站代理隔离连接、取消及回退，不作为本次兼容性同步移植。
- Responses 未知历史项、无 ID 的 reasoning 降级、Messages 工具配对与 GPT-6.2 指令重排：上游新增覆盖主要针对其他提供商或协议翻译；没有新的 Copilot 直接转发失败证据，不扩大现有请求改写范围。

### Gemini 工具 schema

算法见 `http-api.md` 的「Gemini 模型的工具 schema 适配」，这里只记与上游的差异，下次同步时逐条核对。

不同步：
- `copilotGeminiToolUnions`：上游已删除（把 `anyOf` 改名 `oneOf` 会被 Copilot 的 Gemini 丢掉分支）。
- 模型、协议与 provider 检查：由 `service.go` 按 `/chat/completions` 与 `geminiModel` 决定。

相对上游的差异：
- 用标准库代替 gjson：`jsontree.go` 的带位置节点树，保持「就地在左花括号后插入 `type`」。
- 先校验请求体是合法 JSON，非法时返回原切片。
- Shapes 重编码 `tools` 数组时不转义 HTML（上游的 `json.Marshal` 会转义）。
- `$defs` 等映射按键排序处理：上游按 Go map 随机序，联合分支的 `$ref` 可能读到已被改写的 `$defs` 项，结果不确定。
- 联合分支比较 `type` 用 `reflect.DeepEqual`：上游 `!=` 在两个 `type` 都是数组时 panic。
- 类型推断展开 `$ref` 最多 256 次（`refExpansionBudget`）：上游对 `allOf` 互相引用的环会指数展开。
- Shapes 每处 schema 最多写入 256 份 `$ref` 内联拷贝（`derefBudget`，只按写进输出的拷贝数扣，用尽后按解析不了的 `$ref` 处理）：上游无上限，大 `$defs` 被很多联合引用时输出线性放大。目标的序列化按 `$ref` 缓存。
- 三处 schema 位置统一为 `function.parameters`、`parameters`、`input_schema`：上游 Shapes 在有 `function` 时只看其下的 `parameters`/`input_schema`，没有时才看工具自身的。
- Types 把节点补成 `object` 且它是裸 object（无 `properties`，也无 `$ref`/`allOf`/`anyOf`/`oneOf`）时一并补 `properties:{}`：Copilot 对无 `properties` 的 object 返回 400，而 Shapes 先于 Types 运行。
- Types 去掉值为 `false` 的属性时同步去掉同级 `required` 里的该名字，删空则去掉键：否则留下 Fit 要避免的 400 条件。

已知缺口（与上游一致，未修）：
- 可空的 `$ref` 联合在目标里含嵌套 `$ref` 时（`derefLocal` 不展开）被降为 `string`。
- `derefLocal` 不处理 `~0`/`~1`/`%20` 转义。
- 没有 `type` 的 `items` 加 `enum` 节点在管线里成为带 `enum` 的数组：Shapes 先于 Types，`enum` 没被挪到 `items`。
- 联合分支的 `type` 写成数组时：分支带有可推断类型的关键字（`properties`、`items`、全字符串 `enum`、字符串 `const`）则 `guessType` 推出的类型覆盖数组 `type`，可空性丢失；没有这些关键字则整个联合被去掉。
- Shapes 不遍历 `if`/`then`/`else`、`dependentSchemas`、`unevaluated*`、`propertyNames`、`contentSchema`，其中已写 `type:"object"` 却没有 `properties` 的节点不会补 `properties`（Types 不管已有 `type` 的节点）。

## 同步 CLIProxyAPI 会话亲和键

本项目会话键（`sessionKey`，见 `session-affinity.md`）的会话 ID 来源与优先级参照 cpa。cpa 的
基线：`db7f177d6b0e51eb237daed718de4d25e7a893f5`（2026-11-19）。本次核对未发现会话键来源、顺序或规范化的变更；Codex 指令缓存的分键不是账号亲和键。

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
CPA_BASE=db7f177d6b0e51eb237daed718de4d25e7a893f5
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
| （cpa 无此来源） | 不使用 `X-Agent-Task-Id`、`X-Request-Id` 作为会话来源：VS Code Copilot（BYOK）把二者设为每轮用户消息都不同的 ID，会使每轮会话键都不同、亲和无法命中；同步 cpa 时不要加回 |
| `NormalizeExplicitID`：拒绝控制字符，裁剪空白，限 256 字节 | 同步 |
| 父会话、子代理、fork 识别与绑定继承 | 不同步。本项目绑定表是扁平的；Codex 取 `Session-Id` 优先于 `Thread-Id`，Claude Code 子代理与主会话共用会话 ID，因此子代理自然落到主会话账号 |
| Gemini `cachedContent`、请求体外层 `request` 包装 | 不同步，本项目不接收 Gemini 协议 |
| `execution_session_id`、LCP 前缀匹配、`derived` 会话指纹 | 不同步，保留本项目的「首条 user 消息」兜底 |
| 绑定键含 provider 与 model | 不同步，键只由会话信号决定，不含客户端密钥与 model |
| gjson 宽松解析（cpa 于 6e6afab9 改用 API 兼容的 MoeForge `ojson`）：数字等非字符串字段转成字符串；未闭合或带尾随内容的 `metadata.user_id` JSON 仍可取出 `session_id`；重复键取第一个 | `encoding/json` 严格解析：只接受 JSON 字符串；畸形 `user_id` JSON 不识别为 Claude 会话（落到 `user:` 或兜底）；重复键取最后一个 |

cpa 新增或调整会话来源（请求头、请求体字段、优先级、规范化）时，先在 `affinity_key_test.go`
补测试，再修改来源表，并同步 `session-affinity.md`「会话键」。会话键变化会使已持久化的绑定失效，
属可接受的缓存语义（旧绑定按 TTL 过期）。

## 同步 VS Code 的 Credits 与 Additional Budget

本项目额度（`quota-stats.md`）的主额度窗口（Credits / Premium requests / Chat messages）、
Additional Budget 窗口，以及生成响应头的额度快照合并，语义参照 VS Code（Copilot 扩展已并入
`microsoft/vscode` 仓库的 `extensions/copilot/`）。对应的初次同步是本项目提交 `571ca90`。VS Code 的
基线：`e82e774f0735d47ee7021d34a6055234229cc9b3`（2026-11-19）。本次核对未发现额度字段或计算变更；新增的 Credits 遥测不改变额度解析和展示。

VS Code 仓库很大，用浅克隆放到 `.local/vscode-src`：

```sh
git clone --depth=1 https://github.com/microsoft/vscode.git .local/vscode-src
git -C .local/vscode-src status --short --branch
git -C .local/vscode-src remote get-url origin
git -C .local/vscode-src fetch --depth=1 origin main &&
git -C .local/vscode-src switch --detach FETCH_HEAD &&
git -C .local/vscode-src log -1 --format='%H %cs %s'
```

浅克隆没有提交历史，`log` 不能列出区间内的提交；把基线也按提交号浅取下来，再比较两棵树：

```sh
VSCODE_BASE=e82e774f0735d47ee7021d34a6055234229cc9b3
git -C .local/vscode-src fetch --depth=1 origin "${VSCODE_BASE}"
git -C .local/vscode-src diff "${VSCODE_BASE}" HEAD -- \
  extensions/copilot/src/platform/chat/common/chatQuotaServiceImpl.ts \
  extensions/copilot/src/platform/chat/common/chatQuotaService.ts \
  extensions/copilot/src/extension/prompt/node/chatMLFetcher.ts \
  src/vs/workbench/services/chat/common/chatEntitlementService.ts \
  src/vs/workbench/contrib/chat/browser/chatStatus/chatStatusDashboard.ts
git -C .local/vscode-src grep -n -E \
  'x-quota-snapshot|premium_models|premium_interactions|overage_entitlement|overage_count|token_based_billing|credits_used' \
  HEAD -- '*.ts'
```

重点位置（文件名以检索结果为准）：

- `src/vs/workbench/services/chat/common/chatEntitlementService.ts`：解析 `/copilot_internal/user`
  （`quota_snapshots` 的 `chat`、`completions`、`premium_interactions`，以及
  `overage_count`/`overage_entitlement`/`overage_permitted`、`credits_used`、`token_based_billing`），
  未分配额度（非 unlimited 且 `entitlement` 为 0）的快照跳过；`getQuotaUsage` 区分按比例与只有
  `creditsUsed` 的情形，并识别组织共享额度用完。
- `src/vs/workbench/contrib/chat/browser/chatStatus/chatStatusDashboard.ts`：状态面板。
  `token_based_billing` 时主额度标为「Credits」，否则为「Premium requests」；`overage_entitlement > 0`
  时显示「Additional Budget」条，剩余比例为 `(entitlement - count) / entitlement`（夹在 0–100），
  重置时间取主额度的；unlimited 但有 `credits_used` 时显示「Credits Used」。
- `extensions/copilot/src/platform/chat/common/chatQuotaServiceImpl.ts`：`processQuotaHeaders`
  （响应头选择：Free 用 `x-quota-snapshot-chat`，其余用 `x-quota-snapshot-premium_models`，再回退
  `x-quota-snapshot-premium_interactions`）、`_processHeaderValue`（`ent`/`ov`/`ovPerm`/`rem`/`rst`，
  `ent === -1` 为 unlimited）、`processQuotaSnapshots`（优先 `premium_models`）、
  `_processUserInfoQuotaSnapshot`（`/copilot_internal/user`，只用 `premium_interactions`）。
- `extensions/copilot/src/extension/prompt/node/chatMLFetcher.ts`：哪些生成响应会调用
  `processQuotaHeaders`（含失败路径）。

本项目对应实现：`internal/copilot/quota.go`（`parseQuota`、`QuotaBucket.Entitlement`）、
`quota_header.go`（`parseQuotaHeader`、`quotaHeaderSnapshot`、`applyQuotaHeaders`）、`service.go`
（`onResponseHeaders` 钩子）、`weight.go`（权重与耗尽判断）、`web/src/quota.ts`（控制台展示）；测试在
`quota*_test.go`（响应头合并见 `quota_header_test.go`、`quota_header_hook_test.go`，`premium_models` 回退见
`quota_alias_test.go`）。同步内容与刻意不同步的部分：

| VS Code 行为 | 本项目 |
| --- | --- |
| `token_based_billing` 时主额度为 Credits，否则为 Premium requests；Free 用 `chat` 快照 | 同步：主额度窗口 `credits`/`Credits` 或 `premium_requests`/`Premium requests`，Free 为 `Chat messages`（`token_based_billing` 时仍为 `credits`） |
| `overage_entitlement > 0` 时显示 Additional Budget，剩余 `(entitlement - count) / entitlement`，重置时间同主额度 | 同步：`additional_budget` 窗口，分母存入 `QuotaBucket.Entitlement` 并落盘，供响应头 `ov` 重算 |
| unlimited 但 `has_quota=false` 视为组织共享额度用完 | 同步：输出剩余 0 的主额度窗口（例外规则见 `quota-stats.md`） |
| `credits_used` 显示为 Credits Used | 同步：输出到 Summary 的 `credits_used` |
| 响应头：Free 只认 `chat`，其余 `premium_models` 再回退 `premium_interactions`；`rem`/`ov`/`rst`/`ent` | 同步，差异：按头是否存在（`header.Values`）回退，空值不再回退（VS Code 用 `||`，空串会回退） |
| 响应头覆盖整个额度信息（缺字段按默认值，`rst` 缺失时为一个月后） | 不同步：只合并合法字段到已有窗口，不新建窗口、不用默认值；`unlimited` 头不合并 `rem`，以免把组织额度用完的 0 改回有额度 |
| `/copilot_internal/user` 的 Premium 快照只取 `premium_interactions`（工作台与扩展都是），`premium_models` 只出现在响应头和 agent 会话快照中 | 差异：`parseQuota` 优先 `premium_models`，缺失时整体回退 `premium_interactions`（`571ca90`）。两者同时存在且内容不同时，结果与 VS Code 不同 |
| `x-usage-ratelimit-session`/`weekly` 响应头、每轮 `copilot_usage` credits、`completions` 快照 | 不同步，本项目不展示这些信息 |

VS Code 调整额度来源、字段、Credits/Additional Budget 的计算或展示时，先在对应测试中补用例，再改
`quota.go`/`quota_header.go`，并同步 `quota-stats.md`；影响权重或耗尽判断时同步 `pool.md`。

## 修改与验证

先把上游差异归纳为具体行为及对应测试，再决定是否影响本项目。按本项目的开发约束先补充或
调整测试，再实现必要的改动；优先使用本地模拟服务验证，不使用真实账号/token 做自动化测试。
至少核对：

- OAuth token 换取 session、有效期与刷新。
- session 返回的 API 主机、企业域名校验和重定向/请求头安全边界。
- 模型列表元数据、协议端点、模型条款和账号权限差异。
- 模型被上游拒绝（400 `model_not_supported`）后的处理；Auto 相关改动不同步（见上文「已删除的 Auto」）。
- 请求身份头、用户/工具轮次、图片请求，以及 SSE 转发和请求取消。

同步完成后运行：

```sh
go test -race ./...
go vet ./...
go build -o .local/ghc2api-verify ./cmd/ghc2api
git check-ignore .local/magpie .local/cpa .local/vscode-src
```

检查 diff，确认未带入无关 provider 行为或凭证数据。所有验证通过后，将本文开头的基线、
上方核对命令中的 `BASE` 和 README 中的短提交号更新为本次实际核对的上游提交；cpa 同理更新
「同步 CLIProxyAPI 会话亲和键」中的基线、`CPA_BASE` 和 README 中的短提交号；VS Code 同理更新
「同步 VS Code 的 Credits 与 Additional Budget」中的基线、`VSCODE_BASE` 和 README 中的短提交号；保留本项目
独立实现，不在 `.local/` 中直接修改或提交上游代码。
