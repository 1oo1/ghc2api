# 会话亲和

代码：`internal/copilot/affinity.go`（绑定表）、`affinity_key.go`（`sessionKey`、会话信号来源）、`affinity_store.go`（持久化）、`affinity_gate.go`（会话门 `keyGate`）、`pool.go`（`forward`、`pickSession`、`Pool.bind`）。

目的：同一客户端会话固定使用同一账号，提高上游提示词缓存命中率。配置见 `config.md`（`session_affinity`、`session_affinity_ttl`，默认开启、2h，每次使用续期；`session_affinity_max_per_account`，见下文「绑定上限」）。

## 适用范围

只对 POST 生成请求（chat completions、responses、`/v1/messages`）生效；模型列表不使用。

## 会话键

`sessionKey(r, body, upstreamPath) string`：`hex(sha256(signal))`，直接对会话信号取哈希，不含客户端密钥，不保留正文。不同客户端密钥携带同一会话信号时得到同一键。

- `signal` 的来源与优先级参照 CLIProxyAPI（cpa）的 `ExtractSessionInfo`，同步方式与差异见 `magpie-copilot-sync.md`「同步 CLIProxyAPI 会话亲和键」。显式会话 ID 与上游路径无关，形如 `前缀:ID`，按以下顺序取第一个有效值：
  1. 请求头 `X-Claude-Code-Session-Id` → `claude:`；
  2. 请求体 `metadata.user_id`（字符串，裁剪空白）中的 Claude Code 会话：以 `{` 开头时取 JSON 的 `session_id`，否则匹配旧格式 `_session_([a-f0-9-]+)$` → `claude:`。请求头与 metadata 中的同一会话得到同一键；
  3. Codex → `codex:`：会话 ID（请求头 `Session-Id`、`Session_id`、`X-Codex-Turn-Metadata` JSON 的 `session_id`）优先，没有时取线程 ID（`Thread-Id`、`Thread_id`、turn metadata 的 `thread_id`）。子代理与主会话共用会话 ID，因此落到同一账号；
  4. 通用请求头：`X-Http-Session-Id` → `agy:`，`X-Session-ID` → `header:`，`X-Session-Affinity`（OpenCode）→ `affinity:`，`X-Slot-Session-Id`（pi）→ `slot:`，`X-Task-ID`/`X-Task_ID` → `task:`，`X-Conversation-Id` → `conv:`，`X-Thread-Id` → `thread:`，`X-Client-Request-Id` → `clientreq:`。`X-Agent-Task-Id` 与 `X-Request-Id` 不作为会话信号（VS Code Copilot（BYOK）把二者设为每轮用户消息都不同的 ID，会拆散亲和），请求回落到请求体来源或内容兜底；
  5. 请求体字段（`a.b` 为 a 对象下的 b）：`thread_id`、`threadId`、`metadata.thread_id` → `thread:`；`session_id`、`sessionId`、`sessionID`、`child_session_id`、`childSessionId` 及其在 `metadata.`、`extra_body.` 下的部分别名 → `session:`；`task_id`、`action_id` 等任务字段（同样含部分 `metadata.`、`extra_body.` 别名）→ `task:`；`prompt_cache_key`、`promptCacheKey` → `pck:`；`conversation.id` 或字符串 `conversation` → `conv:`；未识别出 Claude 会话的 `metadata.user_id` 整串 → `user:`；`conversation_id`、`chat_id` 等 → `conv:`。完整字段表见 `affinity_key.go` 的 `bodySessionSources`；
  6. 兜底：第一条 user 消息文本（content 为字符串或按序拼接的 `type=text` 片段；Responses 的 `input` 为字符串或 `input[]` 第一条 user 项）。无 user 消息则无信号。兜底为 `content\x00` + `hex(sha256(文本))`，不含 `model`，与显式 ID 互不混淆。
- 显式 ID 的规范化（同 cpa `NormalizeExplicitID`）：含控制字符无效；裁剪空白后为空或超过 256 字节无效；不要求 UUID、不做大小写归一。无效等同于不存在，继续检查下一来源。请求头名大小写不敏感，同名多值取第一个有效值；请求体字段只接受 JSON 字符串。
- 前缀相同、ID 相同的不同来源得到同一键（如请求头 `X-Task-ID` 与请求体 `task_id`）；前缀不同则键不同。
- 请求体不是合法 JSON 对象时跳过请求体来源与兜底，请求头信号仍然有效；无任何信号时 `sessionKey` 返回 `""`；开启亲和时 Pool 直接返回 500（`无法计算模型缓存 key`），不选账号、不访问上游。请求体读取失败或超限时不计算键，也不返回 500，按空键放行。
- 不同步 cpa 的父子会话继承、fork 识别、LCP 前缀匹配与派生会话指纹；绑定表保持扁平。
- 会话键规则变化后，已持久化的旧绑定不再被命中，按 TTL 过期，属可接受的缓存语义。
- 内容兜底只看第一条 user 消息，开头相同的不同会话会落到同一账号，只影响缓存命中，不影响正确性。
- `messages`（或 `input[]`）中的非对象元素（数字等）被跳过，不影响第一条 user 消息的查找。
- 字段类型错误的消息或内容块整项按无效处理并跳过（已接受的语义变化）：`role` 不是字符串的消息不算 user 消息，`text` 或 `type` 类型错误的内容块不计入文本，其余项照常参与查找。与按通用 JSON 解析的旧实现相比，这会改变兜底信号的取值（见 `http-api.md`「上游请求头与消息解析」）。
- Pool 选账号前读取请求体（≤100 MiB）并原样还给下游；超限时不计算键，下游仍返回原有 413。

## 选择规则

- 键已绑定且账号存在、未禁用、未冷却、未耗尽、本请求未尝试过 → 直接使用，不参与 `pickLocked`，也不推进轮询，且不受绑定上限限制；否则为无可用绑定的会话选择账号：先取最优档，在档内排除已达上限的账号（见下文「绑定上限」），再按 `pool.md` 的规则选择。
- 成功取得 session 后，把键绑定（或续期）到最终使用的账号；绑定账号 session 失败则计入其错误（连续失败达到冷却条件时进入冷却）并切换，新账号成为新绑定。
- 绑定账号进入冷却时，冷却期间它不参与选择，因此绑定视为不可用；进入冷却后亲和表中指向该账号的全部绑定会被异步删除（见「冷却与绑定」）。取得 session 之后若账号已进入冷却，则不绑定会话键，本次请求仍由该账号处理。
- 同一会话键的选择、取 session、绑定串行执行：`Pool.pickSession` 在开启亲和且键非空时持有该键的会话门（`keyGate`，`affinity_gate.go`）。后到的同键请求排队，先到者完成绑定后，后到者命中其绑定，因此同键请求使用同一账号。门在绑定之后、响应之前释放，不持有到流式响应结束；亲和关闭或键为空时不加门，不同键互不阻塞。
- 等待门的上界：门的持有时间受请求 ctx 约束（客户端断开即取消），服务端没有单独的总超时。持门期间的主要耗时是取 session：有缓存 session 时很快；需要向 GitHub 换取时，单次 `getSession` 的 OAuth 请求超时为 30 秒（`session.go`）。选中的账号 session 失败后会换用下一个账号（按状态码规则计入，401/403 一次即冷却，其余计入的错误连续第 3 次冷却），因此最坏情况约为「按账号依次故障切换的超时之和」，即约 账号数 × 30 秒；此外 `getSession` 还在账号级 `sessionMu` 上排队，同账号其他会话键的取 session 也会占用它，这部分同样受 ctx 约束、会额外累加。
- 代价与取舍：同键后到请求在门外排队，会承受先到者取 session 的耗时，包括先到者的失败超时。若先到者全部失败，它不会绑定，后到者随后自行重新选择并取 session（冷却中的账号不参与）。若没有门，后到请求在先到者绑定前会直接经 SWRR 选出其他账号，同一会话的请求可能分散到不同账号。这里选择以等待换取同键同账号，是有意的取舍。
- 等待门期间客户端取消的请求：不进入选择，不计入任何账号的请求数，也不推进 SWRR 轮询，不冷却、不写响应。日志层仍会记录「客户端请求处理完成」，其 `status` 为未写入响应时的默认值 200。

## 绑定上限

- 配置 `session_affinity_max_per_account`，对应 `PoolConfig.SessionAffinityMaxPerAccount`。缺省为 `DefaultSessionAffinityMaxPerAccount = 5`，`0` 表示不限制，负数与非整数启动失败。
- 账号的有效绑定数由 `(*affinityTable).activeCounts(now) map[string]int` 统计（键为账号 ID）。只计截止时刻之后的绑定，与 `lookup` 的规则一致：过期项不占名额，恰好到截止时刻的绑定也不计数。
- 键非空、开启亲和且没有可用绑定（新会话，或绑定账号不可用后的故障切换）时，`Pool.choose` 先取最优档（优先级、有无 Credits，见 `pool.md`），只在该档内排除有效绑定数达到上限的账号，再对剩余账号 SWRR；上限不会把会话分流到低档。
- 回退：最优档内账号全满时忽略上限，对整个最优档轮询，绝不降到低优先级或无 Credits 档。冷却中的账号本来就不是候选，因此上限不会把会话推向冷却中的账号；所有账号都已满时也照常分配，不会因上限返回 503。
- 选中账号后照常绑定，即使回退使该账号超过上限。
- 已有的绑定不受影响：超限账号的既有绑定仍可续期；降低上限只影响之后的新会话。键为空（没有会话信号）或关闭亲和时上限无效。
- 并发：同一会话键的请求不再竞争，它们在会话门内串行（见「选择规则」）。但不同键的新会话仍可能同时读到相同的账号计数，实际绑定数可能略超上限。上限用于分散负载，不是硬性配额，这一点可以接受。

## 绑定表

- `const DefaultSessionAffinityTTL = 2 * time.Hour`，`const maxSessionBindings = 500`。
- `newAffinityTable(ttl)`；`bind(key, accountID, now)` 新建/续期（截止 `now+ttl`）；`lookup(key, now)` 截止时刻及之后视为不存在；`count()` 含未清理的过期项；`activeCounts(now)` 按账号 ID 统计有效绑定数（见「绑定上限」）；并发安全。
- 满表插入：先清过期项，仍满则淘汰最早过期的一条（并列按键字典序）；续期已有键不触发淘汰。

## 持久化

- 文件 `<CacheDir>/session_affinity.json`，`{"version":1,"bindings":[{"key","account","expires"}]}`，权限 `0600`，经 `writeCacheFile` 原子写入。
- `save(path, now)` 只写仍有效的绑定。`load(path, now)`：文件不存在返回 nil；损坏返回错误且表不变；丢弃过期项与 key/账号 ID 为空的项；有效期取 `min(文件截止时刻, now+ttl)`；超 500 条保留截止最晚的（并列键字典序小者优先）。
- `(*Pool).SaveSessionAffinity() error`：未开启亲和或 `CacheDir` 为空时无操作。开启亲和且有 `CacheDir` 时池启动即加载，失败只输出含「会话亲和」的 Warn，以空表启动。
- cmd 层 `serve` 在 HTTP 服务 Shutdown 之后（defer）保存：先调用 `Pool.WaitCooldownEffectsTimeout(5 * time.Second)` 等待进入冷却的异步副作用结束（最多 5 秒，超时记 Warn「会话亲和：等待冷却副作用超时，继续保存」后照常保存），再保存；成功 Info「会话亲和：已保存」，失败 Warn，不改变 serve 返回值。进程被强杀会丢失最近绑定，属可接受的缓存语义。

## 风控记录

开启会话亲和时同时启用风控记录：401/403 使账号进入冷却时记录触发请求的会话键，之后同键请求直接返回 500（消息中附记录的原始状态码），有效期 2 天，持久化到 `<CacheDir>/risk_blocks.json`。规则见 `pool.md`「风控记录」。

## 日志

`Pool.bind` 仅在键非空且开启亲和时输出，属性 `session` 为键前 12 字符，不出现完整键、客户端密钥或正文：

- 无有效原绑定：Info `会话亲和：新建绑定`（`session`、`bound_account`）；
- 原绑定即本次账号：Debug `会话亲和：命中`；
- 原绑定是其他账号：Info `会话亲和：切换账号`（`session`、`from`、`to`）。

正常情况下，同一会话键的并发请求经会话门串行，后到者看到的是先到者完成后的绑定，因此不会出现「切换账号」；切换只发生在绑定账号不可用（禁用、冷却、耗尽或 session 失败）而故障切换到其他账号时。

## 冷却与绑定

- 账号进入冷却（见 `pool.md` 的「限流、错误计数与冷却」）后，`Pool` 在后台删除亲和表中指向该账号的全部绑定，包括已过期但尚未清理的项（`(*affinityTable).dropAccount(accountID)`）；指向其他账号的绑定保留。删除前检查账号仍处于本次冷却，已解除则不删除。
- 删除在后台异步进行，之后才保存亲和表：服务正常关闭时，`saveSessionAffinity` 先最多等待 5 秒这些异步副作用结束（`Pool.WaitCooldownEffectsTimeout`），再写入绑定表，因此正常关闭时保存的文件不含指向冷却账号的绑定。
- 进程崩溃（未经正常关闭）时，磁盘上的亲和表仍是上一次保存的内容，其中可能含指向冷却账号的旧绑定。这些旧绑定在启动时若账号仍处于冷却（来自 `cooldown.json`）则视为不可用，会被重新选择并覆盖；但若冷却已经结束，它们可能重新生效，直到 TTL 过期或被新的绑定覆盖。
- 冷却期间的请求不会因绑定而选中该账号。正常关闭后的绑定已被删除，冷却结束或手动解除后不会恢复；崩溃时如上所述，旧绑定可能在冷却结束后重新生效。
