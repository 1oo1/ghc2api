# 账号池：选择、故障切换与冷却

代码：`internal/copilot/pool.go`、`weight.go`、`service.go`（`setCooldown`/`available`）。

## 结构与构造

- `Pool.accounts []*poolAccount` 保持插入顺序：启动时为凭证文件名顺序，管理接口新增的追加到末尾。ID 只用于查找与唯一性。
- `poolAccount`：`id`、`service *Service`、`routes`、`disabled`、`priority`、请求统计、额度缓存。各账号的 session 与缓存相互独立。
- 被选中的账号指针在请求期间独立使用，删除账号不影响进行中的请求。

```go
NewPool(configs []Config) (*Pool, error)   // 亲和与管理关闭，至少 1 个账号（主要给旧测试）
NewManagedPool(PoolConfig, []PoolAccount) (*Pool, error)
```

- `PoolConfig`：`APIKey`、`ManagementKey`、`CredentialsDir`、`Host`（顶层覆盖）、`CacheDir`（测试须指向 `t.TempDir()`）、`ModelsCacheTTL`、`SessionAffinityTTL`（≤0 关闭亲和）、`Client`（测试注入）、`Logger`。
- `PoolAccount`：`Token`、`Host`、`User`（必填）、`Disabled`、`Priority`。
- `NewManagedPool`：仅 `ManagementKey` 非空时允许 0 账号；账号重复（Host+Token 相同）或用户名忽略大小写重复、`ManagementKey` 与 `APIKey`（去首尾空白）相同、用户名不合规，均返回错误。

## 账号选择

`pickLocked(now, tried)`（调用方持有 `Pool.mu`）。候选为未禁用且本请求未尝试过的账号，按以下顺序取最优：

1. 未冷却优先；只有没有未冷却候选时才选冷却中的账号。禁用的账号从不参与。
2. `priority` 高者在前。
3. 额度权重高者在前（`weigh`，`weight.go`）：主额度是第一个非 `additional_budget` 的额度窗口；剩余比例 > 0 为"有 Credits"档，值为该比例；否则取 `additional_budget` 比例，档位"无 Credits"；无额度数据为（无 Credits，0）。先比档位再比值。
4. 凭证文件名（`User+".json"`）字节序升序，无用户名用 ID。

- 权重只读账号的额度缓存，不访问上游，只在额度刷新后变化（见 `quota-stats.md`）。无 `management_key` 时没有控制台，不主动刷新，权重保持启动时从缓存恢复的值。
- 没有轮询游标：相同状态下连续请求选中同一账号。
- 无可选账号时，需要账号的请求返回 503（`没有可用的 GitHub Copilot 账号`）。
- `/healthz`、404、405、重定向不访问上游，由池直接处理，0 账号也能工作。
- 模型列表是各账号独立获取的；订阅不同时某模型可能只在部分账号可用，被分到不支持的账号会返回 400。

## 故障切换

- 会话已绑定账号时优先使用（见 `session-affinity.md`）。
- 仅模型列表与生成请求（chat completions、responses、`/v1/messages`）在 session 获取失败（网络错误、上游错误响应、无效 session）时切换：该账号冷却 1 分钟，同请求改用未尝试过的账号（已冷却的排最后），每个账号每请求最多尝试一次，全部失败返回最后一个账号的错误。
- 普通生成请求失败后不会在其他账号重发；仅 Auto 的明确模型/session 拒绝触发有限重选。

## 限流与冷却

- 以下情况账号冷却 1 分钟，且本请求不换号：生成接口直接返回的 429（原样流式透传）；经错误处理路径透传的 429（如 `/v1/models`、Auto 的 `/models/session`）。
- Auto 的 `/auto` 返回 429 时兜底请求 `/models/session`：成功则正常响应且不冷却；兜底也 429 才透传并冷却。
- 客户端取消不冷却也不计错误。冷却状态仅在内存，重启清空。

## 测试依赖的内部名字

`Pool.accounts`、`pickLocked(now, tried map[*poolAccount]bool)`、`MinPriority`/`MaxPriority`、`quotaWeight`/`weigh(*Quota)`、`(*Service).setCooldown(time.Time)`/`available(time.Time) bool`、`(*Pool).findAccount(id)`（忽略大小写）、`(*Service).cacheDir`、`(*Pool).Handler()`；测试辅助 `testClient`、`testJSON`、`testService`、`testDevice`（login_test.go）、`poolUpstream`（pool_test.go）。其余名字见 `session-affinity.md`、`quota-stats.md`、`management.md`。
