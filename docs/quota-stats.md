# 额度与请求统计

代码：`internal/copilot/quota.go`（解析、获取、缓存、持久化）、`quota_header.go`（响应头额度快照合并）、`weight.go`（权重与耗尽判断）、`pool.go`（统计）。

## 额度数据结构与解析

```go
type Quota struct { Subscription *QuotaSubscription; Groups []QuotaGroup; Summary []QuotaMetric; Stale bool; ObservedAt time.Time }
type QuotaGroup struct { DisplayName string; Buckets []QuotaBucket }
type QuotaBucket struct { Window, Description string; RemainingFraction float64; ResetTime string; Entitlement float64 /* 仅 additional_budget 桶 */ }
type QuotaMetric struct { Key, Label string; Value float64; Format string }
parseQuota(raw []byte) (Quota, error)
```

JSON 名为小驼峰（如 `remainingFraction`、`observedAt`），完整定义见 `quota.go`。解析规则：

- 计划名由 `access_type_sku`/`copilot_plan` 映射。`token_based_billing=true` 时主额度为 `credits`/`Credits`，否则 `premium_requests`/`Premium requests`；Copilot Free 用 `chat` 快照（`Chat messages`，`token_based_billing=true` 时仍为 `credits`），无该快照时由 `monthly_quotas.chat` 与 `limited_user_quotas.chat` 计算。
- `unlimited=true` 不产生 bucket（例外见下条）；`credits_used`（`has_quota` 缺失或为 true）输出到 Summary（`Key=credits_used`、`Label=Credits Used`、`Format=number`）。
- Premium 快照取 `quota_snapshots.premium_models`，缺失（或不是对象）时整体回退 `premium_interactions`；回退是对象级的，`premium_models` 存在但字段缺失时不借用 `premium_interactions` 的字段。下文的 Premium 主额度、组织共享额度与 Additional Budget 都使用选中的快照。
- 组织共享 Premium 额度用完：非 Copilot Free 账号的 Premium 快照为 `unlimited=true` 且 `has_quota` 明确为 `false` 时，视为组织共享额度已用完（VS Code 会暂停该席位，见 magpie #1063），输出剩余比例为 0 的主额度 bucket（窗口与名称同上，重置时间按下述规则），因此账号进入"无 Credits"档，重置时间可解析时按耗尽跳过（见 `pool.md`）。例外：该快照 `overage_permitted=true` 且 `copilot_plan` 不是 `business`/`enterprise`（不区分大小写）时仍不产生 bucket。
- Additional Budget：Premium 快照的 `overage_count`、`overage_entitlement` 均为合法非负数且 entitlement>0 时输出 `additional_budget` 桶，`RemainingFraction=max(0, 1-count/entitlement)`，并把分母留存在桶的 `Entitlement` 字段（JSON 键 `entitlement`，`omitempty`，只出现在 `additional_budget` 桶；随 `quota.json` 落盘，重启后响应头合并仍能重算）；非法或 entitlement 为 0 时不输出，不影响主额度。
- 重置时间：`quota_reset_at`（Unix 秒）优先，其次 `quota_reset_date_utc`/`quota_reset_date`/`limited_user_reset_date`（RFC3339 转 UTC，或保留 `YYYY-MM-DD`；无法解析的跳过）。
- 字段逐个容错；顶层不是 JSON 对象（含 `null`）返回错误。

## 响应头额度快照合并

生成请求的上游响应携带额度快照头，`poolAccount.applyQuotaHeaders(header)`（`quota_header.go`）把它合并进缓存额度，使额度在两次轮询之间也能更新（语义参照 VS Code）。接线：`Service.onResponseHeaders` 钩子由 `newPoolAccount` 装配为 `applyQuotaHeaders`，`handleGeneration` 拿到上游响应后（400 分支与流式透传之前，一次调用覆盖两个出口）调用。所有状态码的生成响应（含 4xx/5xx，它们走 `streamResponse` 原样透传而非 `handleError`）都会合并额度头，与 VS Code 失败路径也解析的行为一致；`handleError` 只处理网络错误与 session 交换失败，没有生成响应头可解析（`retryStaleItems` 中被丢弃的第一次响应不合并，无影响）。

- 头名：Copilot Free（缓存额度 `Subscription.Plan == "Copilot Free"`）只认 `x-quota-snapshot-chat`；其余账号（含计划未知）优先 `x-quota-snapshot-premium_models`，该头缺失时回退 `x-quota-snapshot-premium_interactions`（头存在但值为空串或非法时不再回退，按 `header.Values` 判断存在性）。
- 头值是 URL 编码键值对（`parseQuotaHeader` → `quotaHeaderSnapshot`）：`rem`（剩余百分比）、`ov`（overage_count）、`rst`（RFC3339 重置时间）、`ent`（`strconv.Atoi` 解析为 `-1` 时标记 `unlimited`，对应 VS Code 的 `unlimited: entitlement === -1`；缺失、非法或其他值为 false）；`ovPerm` 等其他键忽略。`url.ParseQuery` 的错误被忽略，坏键值对只丢弃自身，其余部分照常使用；`rem`/`ov` 非数字、NaN、Inf、负数或 `rst` 非 RFC3339 时只丢弃该字段。
- 合并规则：主额度窗口 `rem` 合法且头不是 `unlimited` 时 `RemainingFraction=min(100, rem)/100`，`rst` 合法时更新 `ResetTime`（UTC 的 RFC3339）；`unlimited` 头不合并 `rem`（`parseQuota` 对组织共享额度耗尽的 unlimited 快照造了 `RemainingFraction=0` 的主窗口，不限量头的 `rem` 会把它错误地改回有额度；代价是组织额度恢复后要等下一次轮询才解除耗尽），`rst`、`ov` 照常；`additional_budget` 窗口在 `ov` 合法且已记录分母（`entitlement>0`）时 `RemainingFraction=max(0, 1-ov/entitlement)`，其 `ResetTime` 不变。窗口不存在时不新建，对应字段不合并。
- no-op 条件：缓存没有额度数据（不会由头创建额度）、头缺失，或没有任何字段被合并（如头值无可用字段、窗口都不存在、旧版本写入的 `quota.json` 恢复后缺分母且只有 `ov`）。no-op 时 `ObservedAt` 也不变。
- 至少合并一个字段后 `ObservedAt=time.Now()`；**不改** `Stale` 与 `checkedAt`（不影响轮询节流）。头合并本身不触发写盘；之后任何一次刷新落盘（限流或错误路径的 `saveQuotaLocked`）会把合并结果（含分母 `Entitlement`）写入 `quota.json`。合并后的额度立即参与权重与耗尽判断（见 `pool.md`）。
- 已知边缘：Copilot Free 走 `x-quota-snapshot-chat` 头时，`ov` 会合并进来自 `premium_models` 快照的 `additional_budget` 桶（两者来源的快照不同；Free 账号极少有该桶，概率低，行为保持现状）。
- 写时复制：`quotaCache.snapshot()` 是浅拷贝，管理接口可能正在序列化共享的 `Groups`/`Buckets`，因此合并先复制 `Groups` 与 `Buckets` 再改，最后在 `quotaCache.mu` 内替换缓存指针，不原地修改。

## 获取与节流

`(*Service).fetchQuota(ctx) (Quota, error)` 请求 GitHub `/copilot_internal/user`：2xx 解析并设置 `ObservedAt`；429 或带 `Retry-After` 的 403 返回 `errQuotaRateLimited`（用 `errors.Is` 判断）；其他非 2xx 返回 `*upstreamError`；2xx 但正文非法返回解析错误。

- 按账号 `checkedAt`（最近一次**尝试**）节流：距上次尝试不足刷新间隔（`quota_refresh_interval`，默认 1 小时，见 `config.md`；账号的值为 `Service.quotaRefreshInterval`）不重复请求（强制刷新除外），失败和超时也算一次尝试。
- 调用方取消（`context.Canceled`）不算尝试：不更新 `checkedAt`、不写 `quota_error`，下次列表请求或后台轮询（最迟一分钟后）重新请求。
- 限流：保留旧数据并 `Stale=true`；其他失败保留旧数据并设 `quota_error`（无旧数据则只设 `quota_error`）。

## 刷新入口

- `GET /v0/management/accounts`：对从未获取或距上次尝试已超过刷新间隔的账号并发刷新，总超时 `quotaRefreshTimeout`（10 秒）；失败不影响 200。
- `POST /v0/management/quota/refresh`（强制刷新，忽略节流）：429 返回 200 + `Stale`；其他错误有旧数据返回 200 + 非空 `quota_error`，无旧数据返回 502；超时按其他错误处理，`quota_error="额度查询超时"`（固定中文，不含上游细节）。
- 新增账号（导入或设备登录，见 `management.md`）：入池后立即异步强制刷新一次（`poolAccount.refreshAsync(0)`，不节流，因此即使从缓存恢复了较新的 `checked_at` 也会刷新）；不阻塞导入或登录的成功响应，失败只记入 `quota_error`。
- 超时时长为包级变量 `var quotaRefreshTimeout = 10 * time.Second`（测试可临时改小，不得改为常量）。
- 后台定时刷新（`RunQuotaRefresh`）、请求失败后的异步刷新与新增账号后的异步刷新，都复用上述 `refresh` 与超时，见下节。

### 后台定时刷新

- `(*Pool).RunQuotaRefresh(ctx)` 阻塞运行直到 `ctx` 取消，`serve` 在 HTTP 服务开始监听后以 `go` 启动。**无需打开控制台、与是否配置 `management_key` 无关**：服务运行期间额度就会按时刷新。
- 启动时立即对全部账号（含禁用账号）做一轮非强制刷新；之后每隔 `var quotaRefreshTick = time.Minute`（测试可临时缩短，不得改为常量）检查一次。
- 每次检查都重新取账号快照，因此运行期间通过管理接口新增的账号同样被刷新。
- 检查只按各账号的 `checkedAt` 与刷新间隔判断：未到期的账号不请求上游，启动时从 `quota.json` 恢复的 `checked_at` 同样生效。每轮总超时 `quotaRefreshTimeout`，超时的账号记录 `quota_error="额度查询超时"`，下一轮照常进行。
- `ctx` 取消后进行中的刷新随之中止，`RunQuotaRefresh` 返回，之后不再请求上游。

### 错误触发刷新

- 账号处理请求失败（响应状态 ≥400 且不是 429，或会话获取失败且不是上游 429）时，异步强制刷新该账号的额度（`poolAccount.refreshAfterError`，它以节流间隔调用 `refreshAsync`，`quota.go`）。触发点：`poolAccount.record`（经 `Pool.serve` 处理结束后，客户端未取消时）与 `Pool.pickSession` 中会话获取失败的分支。
- 客户端断开造成的失败（`context.Canceled`）不触发；429 是限流，不触发刷新（429 本身按连续错误计数，见 `pool.md`）。
- 节流：距上次尝试不足 `var quotaErrorRefreshInterval = time.Minute`（测试可临时修改，不得改为常量）不触发。同一账号已有异步刷新进行中（`quotaCache.refreshing`，错误触发与新增账号共用）时不再发起，因此单账号会话失败时两处触发只刷新一次。该标志不约束后台定时刷新与控制台列表刷新（非强制）：它们互相之间、以及与异步刷新之间没有在途保护，同一账号偶尔会并发请求一次上游（最长持续一次刷新的超时 10 秒，不会累积）。
- 429 不触发刷新：Copilot 若以 429 表示额度耗尽，该账号的 429 只计入连续错误（连续第 3 次才冷却，冷却时长为 `account_cooldown`，缺省 2 小时，见 `pool.md`），耗尽状态要等下一次后台刷新（最长一个刷新间隔）才能被识别。
- 异步执行，不阻塞响应；使用独立于请求的 context（超时 `quotaRefreshTimeout`），客户端断开不影响刷新。结果照常在 `quotaCache.mu` 内更新 `checkedAt` 与额度并写入 `quota.json`，因此刷新得到的耗尽额度立即影响后续选择（见 `pool.md`）。

## 持久化

- 每次有结果的刷新（成功、429、其他错误含超时）后，经 `writeCache("quota", ...)` 把 `{"quota","checked_at","error"}` 写入账号缓存目录 `<缓存根>/<账号哈希>/quota.json`（`0600`，在 `quotaCache.mu` 内写盘）；无数据/无错误时省略对应字段；调用方取消不写；写失败只记 Warn。
- `newPoolAccount` 时恢复额度、`checked_at`、`error`：文件不存在静默忽略；损坏或超过 8 MiB 记 Warn（不含 token）并按无缓存处理；`checked_at` 晚于当前时间视为无效（立即允许刷新），额度与 `error` 仍恢复。因此刷新间隔的节流跨重启生效。恢复的额度同时决定耗尽判断（见 `pool.md`），重启后耗尽的账号仍按缓存跳过。
- 删除账号后不重建缓存目录；无缓存目录时额度只在内存。
- 测试可读 `(*poolAccount).quota`（`quotaCache`）及其 `snapshot() (*Quota, string)`。

## 请求统计

每账号内存记录 `requests`（分发到该账号的请求数，含模型列表与生成）、`errors`（响应状态 ≥400）、`last_used_at`，重启清零。

- session 失败后切换账号，只计入最终处理的账号；所有账号都失败时计入最后尝试的账号（`requests`、`errors` 各 +1）。
- 客户端取消不计 `errors`（`requests` 可计）。
- `/responses` 的失效输出项错误（401/400，见 `http-api.md`）标记为忽略（写给客户端的 401 改为 500）：不计 `errors`，也不触发错误后的额度刷新（`requests` 照常计）。
