# 会话亲和

代码：`internal/copilot/affinity.go`（绑定表、`sessionKey`）、`affinity_store.go`（持久化）、`affinity_gate.go`（会话门 `keyGate`）、`pool.go`（`forward`、`pickSession`、`Pool.bind`）。

目的：同一客户端会话固定使用同一账号，提高上游提示词缓存命中率。配置见 `config.md`（`session_affinity`、`session_affinity_ttl`，默认开启、2h，每次使用续期；`session_affinity_max_per_account`，见下文「绑定上限」）。

## 适用范围

只对 POST 生成请求（chat completions、responses、`/v1/messages`）生效；模型列表不使用。生成请求的 `api_key` 必填且已通过鉴权，会话键的 `clientKey` 取自请求头（`Authorization: Bearer` 优先，否则 `X-Api-Key`）；请求已通过鉴权，因此它总是非空。

## 会话键

`sessionKey(r, body, upstreamPath) string`：`hex(sha256(json.Marshal([2]string{clientKey, signal})))`，不保留原始密钥与正文。

- `clientKey`：`Authorization: Bearer` 优先，否则 `X-Api-Key`，可为空。
- `signal` 按优先级：
  1. `/responses` 的 `prompt_cache_key`（非空字符串）；
  2. `/v1/messages` 的 `metadata.user_id`（字符串，≤4096 字节）中的 Claude Code 会话 UUID：JSON `{"session_id":"<uuid>"}` 或旧格式 `user_<x>_account_<y>_session_<uuid>`；UUID 转小写，两种格式得到同一键；非法 UUID 视为无信号；
  3. 兜底：`model` + 第一条 user 消息文本（content 为字符串或按序拼接的 `type=text` 片段；Responses 的 `input` 为字符串或 `input[]` 第一条 user 项）。无 user 消息则无信号。兜底与 Claude 会话信号的编码互不混淆。
- 无信号或非法 JSON 返回 `""`：不使用亲和、不绑定。
- 内容兜底只看第一条 user 消息，开头相同的不同会话会落到同一账号，只影响缓存命中，不影响正确性。
- `messages`（或 `input[]`）中的非对象元素（数字等）被跳过，不影响第一条 user 消息的查找。
- 字段类型错误的消息或内容块整项按无效处理并跳过（已接受的语义变化）：`role` 不是字符串的消息不算 user 消息，`text` 或 `type` 类型错误的内容块不计入文本，其余项照常参与查找。与按通用 JSON 解析的旧实现相比，这会改变兜底信号的取值（见 `http-api.md`「上游请求头与消息解析」）。
- Pool 选账号前读取请求体（≤16 MiB）并原样还给下游；超限时不计算键，下游仍返回原有 413。

## 选择规则

- 键已绑定且账号存在、未禁用、未冷却、未耗尽、本请求未尝试过 → 直接使用，不参与 `pickLocked`，也不推进轮询，且不受绑定上限限制；否则为无可用绑定的会话选择账号：先排除已达上限的账号（见下文「绑定上限」），再按 `pool.md` 的规则选择。
- 成功取得 session 后，把键绑定（或续期）到最终使用的账号；绑定账号 session 失败则按规则冷却并切换，新账号成为新绑定。
- 同一会话键的选择、取 session、绑定串行执行：`Pool.pickSession` 在开启亲和且键非空时持有该键的会话门（`keyGate`，`affinity_gate.go`）。后到的同键请求排队，先到者完成绑定后，后到者命中其绑定，因此同键请求使用同一账号。门在绑定之后、响应之前释放，不持有到流式响应结束；亲和关闭或键为空时不加门，不同键互不阻塞。
- 等待门的上界：门的持有时间受请求 ctx 约束（客户端断开即取消），服务端没有单独的总超时。持门期间的主要耗时是取 session：有缓存 session 时很快；需要向 GitHub 换取时，单次 `getSession` 的 OAuth 请求超时为 30 秒（`session.go`）。选中的账号失败后会冷却并切换下一个，因此最坏情况约为「按账号依次故障切换的超时之和」，即约 账号数 × 30 秒；此外 `getSession` 还在账号级 `sessionMu` 上排队，同账号其他会话键的取 session 也会占用它，这部分同样受 ctx 约束、会额外累加。
- 代价与取舍：同键后到请求在门外排队，会承受先到者取 session 的耗时，包括先到者的失败超时。若先到者全部失败，它不会绑定，后到者随后自行重新选择并取 session（已冷却的账号排在后面）。若没有门，后到请求在先到者绑定前会直接经 SWRR 选出其他账号，同一会话的请求可能分散到不同账号。这里选择以等待换取同键同账号，是有意的取舍。
- 等待门期间客户端取消的请求：不进入选择，不计入任何账号的请求数，也不推进 SWRR 轮询，不冷却、不写响应。日志层仍会记录「客户端请求处理完成」，其 `status` 为未写入响应时的默认值 200。

## 绑定上限

- 配置 `session_affinity_max_per_account`，对应 `PoolConfig.SessionAffinityMaxPerAccount`。缺省为 `DefaultSessionAffinityMaxPerAccount = 5`，`0` 表示不限制，负数与非整数启动失败。
- 账号的有效绑定数由 `(*affinityTable).activeCounts(now) map[string]int` 统计（键为账号 ID）。只计截止时刻之后的绑定，与 `lookup` 的规则一致：过期项不占名额，恰好到截止时刻的绑定也不计数。
- 键非空、开启亲和且没有可用绑定（新会话，或绑定账号不可用后的故障切换）时，`Pool.choose` 排除有效绑定数达到上限的账号，再在最优档内轮询。
- 回退：排除后没有候选，或排除后最优档的冷却状态与原最优档不同（即从未冷却降为冷却中）时，忽略上限，按原最优档选择。这样上限不会把会话推向冷却中的账号；所有账号都已满时也照常分配，不会因上限返回 503。
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
- cmd 层 `serve` 在 HTTP 服务 Shutdown 之后（defer）保存：成功 Info「会话亲和：已保存」，失败 Warn，不改变 serve 返回值。进程被强杀会丢失最近绑定，属可接受的缓存语义。

## 日志

`Pool.bind` 仅在键非空且开启亲和时输出，属性 `session` 为键前 12 字符，不出现完整键、客户端密钥或正文：

- 无有效原绑定：Info `会话亲和：新建绑定`（`session`、`bound_account`）；
- 原绑定即本次账号：Debug `会话亲和：命中`；
- 原绑定是其他账号：Info `会话亲和：切换账号`（`session`、`from`、`to`）。

正常情况下，同一会话键的并发请求经会话门串行，后到者看到的是先到者完成后的绑定，因此不会出现「切换账号」；切换只发生在绑定账号不可用（禁用、冷却、耗尽或 session 失败）而故障切换到其他账号时。
