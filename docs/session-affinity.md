# 会话亲和

代码：`internal/copilot/affinity.go`（绑定表、`sessionKey`）、`affinity_store.go`（持久化）、`pool.go`（`forward`、`Pool.bind`）。

目的：同一客户端会话固定使用同一账号，提高上游提示词缓存命中率。配置见 `config.md`（`session_affinity`、`session_affinity_ttl`，默认开启、2h，每次使用续期）。

## 适用范围

只对 POST 生成请求（chat completions、responses、`/v1/messages`）生效；模型列表与用量不使用。未配置 `api_key` 时同样生效。

## 会话键

`sessionKey(r, body, upstreamPath) string`：`hex(sha256(json.Marshal([2]string{clientKey, signal})))`，不保留原始密钥与正文。

- `clientKey`：`Authorization: Bearer` 优先，否则 `X-Api-Key`，可为空。
- `signal` 按优先级：
  1. `/responses` 的 `prompt_cache_key`（非空字符串）；
  2. `/v1/messages` 的 `metadata.user_id`（字符串，≤4096 字节）中的 Claude Code 会话 UUID：JSON `{"session_id":"<uuid>"}` 或旧格式 `user_<x>_account_<y>_session_<uuid>`；UUID 转小写，两种格式得到同一键；非法 UUID 视为无信号；
  3. 兜底：`model` + 第一条 user 消息文本（content 为字符串或按序拼接的 `type=text` 片段；Responses 的 `input` 为字符串或 `input[]` 第一条 user 项）。无 user 消息则无信号。兜底与 Claude 会话信号的编码互不混淆。
- 无信号或非法 JSON 返回 `""`：不使用亲和、不绑定。
- 内容兜底只看第一条 user 消息，开头相同的不同会话会落到同一账号，只影响缓存命中，不影响正确性。
- Pool 选账号前读取请求体（≤16 MiB）并原样还给下游；超限时不计算键，下游仍返回原有 413。

## 选择规则

- 键已绑定且账号存在、未禁用、未冷却、本请求未尝试过 → 直接使用，不参与 `pickLocked`；否则按 `pool.md` 的规则选择。
- 成功取得 session 后，把键绑定（或续期）到最终使用的账号；绑定账号 session 失败则按规则冷却并切换，新账号成为新绑定。

## 绑定表

- `const DefaultSessionAffinityTTL = 2 * time.Hour`，`const maxSessionBindings = 500`。
- `newAffinityTable(ttl)`；`bind(key, accountID, now)` 新建/续期（截止 `now+ttl`）；`lookup(key, now)` 截止时刻及之后视为不存在；`count()` 含未清理的过期项；并发安全。
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
