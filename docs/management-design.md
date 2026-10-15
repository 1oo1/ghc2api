# 会话亲和、额度统计与 Web 管理界面设计

本文是 TODO.md 中三项功能的接口约定，测试与实现都以此为准。保持简单，不做本文之外的扩展。

## 1. 账号标识与 Pool 结构

- 账号 ID 为 GitHub 用户名的小写形式（凭证文件名去掉 `.json` 后转小写），在池内唯一。
- 用户名规则由 `ValidGitHubUserName(name string) bool`（导出，internal/copilot）统一定义，启动加载（cmd/ghc2api）、
  管理导入/设备登录、`NewManagedPool` 共用：长度 1 到 100（企业托管用户 EMU 的登录名更长，需要容纳短码后缀）；
  由字母、数字组成的片段以**单个** `-` 或 `_` 分隔，不以分隔符开头或结尾。`_` 用于支持 GHE.com 的企业托管用户，
  如 `mona-cat_octo`。`NewManagedPool` 中用户名不合规时返回错误。
  `Config.User` 为空时（旧测试直接用 `NewPool` 创建的池），ID 退化为账号在初始列表中的下标字符串（`"0"`、`"1"`…）。
- Pool 内部用 `accounts []*poolAccount` 保存账号，列表保持插入顺序：`NewPool`/`NewManagedPool` 按传入顺序（cmd 层已按凭证文件名排序），
  管理接口新增的账号追加到末尾。轮询顺序与 `GET accounts` 的顺序都是此列表顺序；ID 只用于查找与唯一性（不排序，原因：
  `Config.User` 为空时 ID 为下标，按字节序会改变既有账号的下标）。运行时可增删、启用/禁用。
- 测试通过 `p.accounts[i].service` 访问第 i 个账号的 `*Service`（取代旧的 `p.services`）；`next()` 与
  `pick(tried []bool) int` 的名字与语义保持不变。
  `poolAccount` 至少包含：`id`、`service *Service`、`routes serviceRoutes`、`disabled bool`、请求统计、额度缓存。
- 被选中的账号指针在请求期间独立使用；删除账号不影响正在进行中的请求。
- 禁用的账号完全不参与选择（也不作为“冷却账号最后候选”）。没有任何可选账号时，需要账号的请求返回
  `503`，错误格式同 `writeError`，消息为 `没有可用的 GitHub Copilot 账号`。
- 不访问上游的请求（`/healthz`、404、405、重定向）不依赖任何账号，池中 0 个账号时也能正常处理。
- 日志中的 `account` 字段仍为账号在当前列表中的下标，`user` 为用户名。

## 2. 构造函数

```go
// 现有签名保持不变：会话亲和与管理接口均关闭，至少需要一个账号。
func NewPool(configs []Config) (*Pool, error)

// PoolConfig 是账号池的服务级配置，账号无关的设置都放在这里。
type PoolConfig struct {
    APIKey             string        // 客户端访问密钥，空表示不鉴权
    ManagementKey      string        // 管理密钥；空表示禁用管理接口和控制台（对应路径按普通 404 处理）
    CredentialsDir     string        // 管理接口新增/删除/禁用账号时读写凭证文件的目录
    Host               string        // 顶层 host 覆盖；非空时新增账号与设备登录都使用它
    CacheDir           string        // 缓存根目录；各账号 Service 在其下按账号哈希分子目录（测试必须指向 t.TempDir()）
    ModelsCacheTTL     time.Duration
    SessionAffinityTTL time.Duration // <=0 表示关闭会话亲和（不创建绑定表）
    Client             *http.Client  // 访问 GitHub/Copilot 的客户端（测试注入），新增账号与设备登录也使用它
    Logger             *slog.Logger
}

// PoolAccount 是启动时载入的一个账号。
type PoolAccount struct {
    Token    string
    Host     string
    User     string // GitHub 用户名，必填（为空返回错误）
    Disabled bool
}

// NewManagedPool 允许 0 个账号（仅当 ManagementKey 非空时；否则返回错误“至少需要一个 GitHub Copilot 账号”）。
// 账号重复（Host+Token 相同）或用户名忽略大小写重复时返回错误。
// ManagementKey 与 APIKey 去除首尾空白后相同时返回错误（管理密钥不得与服务访问密钥相同）。
func NewManagedPool(config PoolConfig, accounts []PoolAccount) (*Pool, error)
```

## 3. 会话亲和（internal/copilot/affinity.go）

- 默认 TTL：`DefaultSessionAffinityTTL = 2 * time.Hour`；绑定表上限 `maxSessionBindings = 4096` 条，满时先删除过期项，
  仍满则删除最早过期的一条（并列按键字典序），续期已有键不触发淘汰。
- 绑定表（测试依赖的内部接口）：
  - `newAffinityTable(ttl time.Duration) *affinityTable`；`SessionAffinityTTL<=0` 时 Pool 不创建绑定表。
  - `(t *affinityTable) bind(key, accountID string, now time.Time)`：新绑定或续期，同时更新账号；有效期截止于 `now+ttl`。
  - `(t *affinityTable) lookup(key string, now time.Time) (accountID string, ok bool)`：截止时刻及之后视为不存在。
  - `(t *affinityTable) count() int`：当前条数（含尚未清理的过期项；满表插入后的条数即清理与淘汰的结果）。
  - 所有方法并发安全。
- 只对 POST 生成请求（chat completions、responses、`/v1/messages`）生效；模型列表、用量不使用亲和。
- Pool 在选账号前读取请求体（最多 16 MiB），计算会话键后把请求体原样还给下游 handler；超过上限时不计算会话键，
  并保证下游仍能读到完整请求体从而返回原有的 413。
- 会话键 `sessionKey(r *http.Request, body []byte, upstreamPath string) string`：
  1. 客户端密钥：`Authorization: Bearer <k>` 优先，否则 `X-Api-Key`；可以为空（未配置 api_key 的本机部署）。
  2. 会话信号，按优先级：
     - `upstreamPath == "/responses"` 且 `prompt_cache_key` 为非空字符串；非字符串或空字符串视为无信号；
     - `upstreamPath == "/v1/messages"` 且 `metadata.user_id` 为字符串（≤4096 字节），其中包含 Claude Code 会话 UUID：
       可以是 JSON 字符串 `{"session_id":"<uuid>"}`，或旧格式 `user_<x>_account_<y>_session_<uuid>`；
       UUID 统一转小写，不是合法 UUID 时忽略（该字段视为无信号）；
       同一 UUID 的 JSON 格式与旧格式必须得到相同的会话键，因此哈希输入中 Claude 会话信号为小写 UUID 本身；
     - 兜底：第一条 user 消息的文本（`messages[]` 中 role 为 `user` 的第一条，content 为字符串或 `type=text`
       片段按顺序拼接、忽略非文本片段；Responses 的 `input` 为字符串，或 `input[]` 中第一条 role 为 `user` 的项）。
       前面加上 `model` 值以区分模型。没有 user 消息时无信号（仅有 model 不构成信号）。
       兜底信号与 Claude 会话信号的哈希编码必须互不混淆（具体格式实现自定）。
     - 都没有时返回 `""`（不使用亲和）。非法 JSON 也返回 `""`。
  3. 返回 `hex(sha256(json.Marshal([2]string{clientKey, signal})))`，不保留原始密钥或正文。
- 选择规则（`forward` 中）：会话键已绑定、账号仍存在、未禁用、未冷却、本请求未尝试过时优先使用该账号，
  且不推进轮询游标；否则按原有轮询规则选择。成功取得 session 后把会话键绑定（或续期）到最终使用的账号。
- 绑定的账号 session 获取失败时，照原规则冷却并切换账号，切换后的账号成为新的绑定。
- 会话键为 `""` 时不使用亲和、也不绑定。

## 4. 额度解析（internal/copilot/quota.go）

从 `/Users/zc/Downloads/cpa-github-copilot/internal/copilot/quota.go` 的 `parseQuota` 移植，去掉 Admission 字段。

```go
type Quota struct {
    Subscription *QuotaSubscription `json:"subscription,omitempty"`
    Groups       []QuotaGroup       `json:"groups,omitempty"`
    Summary      []QuotaMetric      `json:"summary,omitempty"`
    Stale        bool               `json:"stale,omitempty"`      // 最近一次刷新被限流，数据为旧值
    ObservedAt   time.Time          `json:"observedAt,omitempty"` // 数据获取时间（parseQuota 不设置）
}
type QuotaSubscription struct { Plan string `json:"plan"`; TierName string `json:"tierName"` }
type QuotaGroup struct { DisplayName string `json:"displayName"`; Buckets []QuotaBucket `json:"buckets"` }
type QuotaBucket struct {
    Window            string  `json:"window"`      // credits | premium_requests | chat | additional_budget
    Description       string  `json:"description"` // Credits | Premium requests | Chat messages | Additional Budget
    RemainingFraction float64 `json:"remainingFraction"`
    ResetTime         string  `json:"resetTime,omitempty"`
}
type QuotaMetric struct { Key, Label string; Value float64; Format string } // json: key,label,value,format

func parseQuota(raw []byte) (Quota, error)
```

解析规则与参考实现一致：

- 计划名由 `access_type_sku`/`copilot_plan` 映射；`token_based_billing=true` 时主额度为 `credits`/`Credits`，
  否则为 `premium_requests`/`Premium requests`；Copilot Free 使用 `chat` 快照（`Chat messages`，`token_based_billing=true`
  时仍为 `credits`）。Copilot Free 没有 `chat` 快照时，用 `monthly_quotas.chat` 与 `limited_user_quotas.chat` 计算 `chat` 桶。
- `unlimited=true` 时不产生 bucket；`credits_used` 在 `has_quota` 缺失或为 `true` 时输出到 Summary（`has_quota=false` 不输出），
  `Key="credits_used"`、`Label="Credits Used"`、`Format="number"`。
- Additional Budget：`premium_interactions.overage_count` 与 `overage_entitlement` 都是合法非负数且
  entitlement > 0 时，输出 `additional_budget` bucket（`Description="Additional Budget"`），
  `RemainingFraction = max(0, 1 - count/entitlement)`；字段非法或 entitlement 为 0 时不输出该桶，但不影响主额度。
- 重置时间：快照 `quota_reset_at`（Unix 秒）优先，否则依次为 `quota_reset_date_utc`/`quota_reset_date`/
  `limited_user_reset_date`（RFC3339 转 UTC，或保留 `YYYY-MM-DD`；无法解析的跳过）。
- 字段逐个容错解析：某个可选字段格式错误不影响其他字段。顶层不是 JSON 对象（含 `null`）时返回错误。

额度获取：`(*Service).fetchQuota(ctx) (Quota, error)` 请求 GitHub `/copilot_internal/user`（与 `/usage` 相同），
2xx 时解析并设置 `ObservedAt`；429 返回 `errQuotaRateLimited`（`var errQuotaRateLimited = errors.New(...)`，调用方用 `errors.Is` 判断）；
403 且带 `Retry-After` 头同样视为限流（返回 `errQuotaRateLimited`），没有该头的 403 仍是普通错误；
限流时调用方保留旧数据并置 `Stale=true`；其他非 2xx 返回 `*upstreamError`；2xx 但正文非法时返回解析错误。
- 节流：额度按账号的 `checkedAt`（最近一次**尝试**的时间）节流，距今不足 2 小时时不重复请求上游（强制刷新除外）。
  失败与超时同样计为一次尝试，2 小时内不会因为列表反复刷新而再次请求。**调用方主动取消**（`context.Canceled`，例如客户端断开）不是上游状态，
  不算一次尝试：不更新 `checkedAt`，也不写入 `quota_error`，下一次列表请求会重新请求上游。
- `GET accounts`：对从未获取或距上次尝试超过 2 小时的账号并发刷新（总超时 10 秒）。刷新失败时：
  429 保留旧数据并置 `Stale=true`；其他错误有旧数据则保留并设置 `quota_error`，没有旧数据则只设置 `quota_error`。失败不影响 200。
- `POST quota/refresh`（强制刷新，忽略节流）：
  - 429 返回 200、保留旧数据并 `Stale=true`；
  - 其他错误有旧数据时返回 200、保留旧数据并带非空 `quota_error`，没有旧数据时返回 502；
  - 超时（`context.DeadlineExceeded`）按其他错误处理：计为一次尝试（更新 `checkedAt`），有旧数据则返回 200、保留旧数据并带
    `quota_error = "额度查询超时"`（固定中文文本，不含 URL 等上游细节），没有旧数据时返回 502。超时总时长为包级变量
    `quotaRefreshTimeout = 10 * time.Second`（测试中可临时改小，实现不得改为常量）。
  - 调用方取消（`context.Canceled`）：不更新 `checkedAt`、不写入 `quota_error`；响应由实现自行决定（客户端已断开），之后的列表请求会重新请求上游。

## 5. 请求统计

每个账号在内存中记录：`requests`（分发到该账号的需要账号的请求数，含用量与生成）、
`errors`（其中响应状态 ≥400 的数量）、`last_used_at`。session 失败后切换账号时，只计入最终处理请求的账号。
所有账号的 session 都失败时，错误计入**最后尝试的那个账号**（`requests` 与 `errors` 各加 1），之前失败的账号不计数。
客户端取消（`context.Canceled`，包括在 session 获取期间断开）的请求**不计入 `errors`**，也不冷却账号；`requests` 可以计入（请求确实分发给了该账号），测试不强制要求。
重启清零。

## 6. 凭证持久化

- `Credential` 新增 `Disabled bool \`json:"disabled,omitempty"\``；`LoadCredential` 接受该字段。
- 管理接口写入 `<CredentialsDir>/<GitHub 登录名>.json`（登录名保留 GitHub 返回的大小写；权限 0600，复用 `SaveCredential` 原子写入）。
  写入前必须确认目录中没有同名文件（忽略大小写，并扫描目录而不是只检查路径）：即使该文件是运行期间放入、尚未加载的，
  导入与设备登录也返回 409（设备登录则结束于 `status=error`），原文件内容保持不变。
- 文件名与 ID 使用的登录名必须通过 `ValidGitHubUserName`（见第 1 节）；不合规的登录名（如含空格或 `.`）导入时返回 400，不写文件。
- 禁用/启用：重写对应凭证文件的 `disabled` 字段，然后更新内存状态；写入失败时内存状态不变并返回 500。
  启用时 `disabled` 字段按 `omitempty` 省略。
- 删除：先删除凭证文件，再从池中移除；文件不存在也视为成功。删除后清理该账号的缓存目录（session 与模型列表缓存）。
  进行中的请求即使之后才完成 session 获取或写入缓存，也不得重新创建该账号的缓存目录（实现须在写缓存前确认账号仍在池中，或使用账号级的已删除标记）。
- 启动加载（cmd/ghc2api）读取 `disabled`，传入 `PoolAccount.Disabled`。


## 7. 管理接口（internal/copilot/management.go）

仅当 `ManagementKey` 非空时注册。鉴权：`Authorization: Bearer <management_key>`（常量时间比较），
失败返回 401。管理路径不受 `api_key` 鉴权影响，`api_key` 也不能访问管理接口；管理密钥只接受 `Authorization: Bearer`，
不接受 `X-Api-Key`。响应均为 JSON；错误格式同 `writeError`。请求体上限 1 MiB。

| 方法与路径 | 请求 | 响应 |
| --- | --- | --- |
| `GET /management` | 无需鉴权 | 控制台 HTML（`web.Index()`），`Content-Type: text/html; charset=utf-8` |
| `GET /v0/management/accounts` | — | `[]AccountView`；顺带为额度缓存超过 2 小时的账号并发刷新额度（总超时 10 秒，失败不影响返回） |
| `POST /v0/management/accounts` | `{"credential":{"oauth_token":"...","host":"","user":""}}` | `200 AccountView`；见下 |
| `PATCH /v0/management/accounts` | `{"id":"alice","disabled":true}` | `200 AccountView` |
| `DELETE /v0/management/accounts?id=alice` | — | `200 {"ok":true}` |
| `POST /v0/management/quota/refresh` | `{"id":"alice"}` | `200 AccountView`（强制刷新额度；失败时 502，但若已有旧数据则返回 200 并带 `quota_error`） |
| `POST /v0/management/login` | — | `200 {"state","url","user_code","interval","expires_in"}` |
| `GET /v0/management/login/status?state=...` | — | `200 {"status":"pending"|"success"|"error","id"?:..., "error"?:...}`；未知 state 返回 404 |

控制台响应必须带安全头：`X-Frame-Options: DENY` 与 `Content-Security-Policy`，其中包含 `default-src 'self'` 与 `frame-ancestors 'none'`。
控制台是 vite 单文件构建，内联了 script 与 style，因此 CSP 的 script-src 与 style-src 需允许 `'unsafe-inline'`（测试只断言 `default-src 'self'` 与 `frame-ancestors 'none'`）。

```go
type AccountView struct {
    ID            string     `json:"id"`
    User          string     `json:"user"`
    Host          string     `json:"host"`                     // 空表示 github.com
    Disabled      bool       `json:"disabled"`
    CooldownUntil *time.Time `json:"cooldown_until,omitempty"` // 仅在冷却中时输出
    Stats         struct {
        Requests   uint64     `json:"requests"`
        Errors     uint64     `json:"errors"`
        LastUsedAt *time.Time `json:"last_used_at,omitempty"`
    } `json:"stats"`
    Quota      *Quota `json:"quota,omitempty"`
    QuotaError string `json:"quota_error,omitempty"`
}
```

- 不存在的 `id` 返回 404。请求体不是合法 JSON 或缺少必填字段返回 400：PATCH 的 `id` 与 `disabled` 都必填
  （`disabled` 需用指针检测缺失）；DELETE 缺少 `id` 参数、quota/refresh 缺少 `id` 均返回 400。
- `GET accounts` 按账号列表顺序返回（见第 1 节）。
- 导入账号（POST accounts）：校验 token 与 host（顶层 Host 非空时覆盖），用该 token 请求 GitHub `/user` 得到登录名，
  再请求 `/copilot_internal/user` 确认订阅（复用 login.go 的 `finishLogin` 校验逻辑）。凭证中 `user` 非空且与登录名
  不一致（忽略大小写）时 400；登录名（忽略大小写）或 Host+Token 已存在时 409；凭证目录中已存在同名文件（忽略大小写，见第 6 节）时 409；
  登录名不符合 `ValidGitHubUserName` 时 400。成功后写入凭证文件并加入池（追加到列表末尾）。
  上游校验失败返回 400，消息不得包含 token。
- 设备登录：`POST login` 发起 GitHub Device Flow（使用顶层 Host），返回 state（16 字节随机数的十六进制，32 个字符）、验证页 URL、
  user code；后台 goroutine 按 login.go 现有轮询规则（slow_down、过期、连续 5 次失败）等待授权，成功后按导入账号同样的
  规则保存并加入池。`login/status` 成功时的 `id` 为新账号 ID（小写登录名）；授权失败时 `status=error` 并带 `error` 文本（不含 token）。
  验证码在用户授权前过期（`pollCtx` 超时而父上下文未取消）时，返回与 GitHub `expired_token` 相同的中文错误 `验证码已过期，请重新登录`，
  而不是 `context deadline exceeded`。CLI `Login` 与 Web 登录的 `login/status` 的 `error` 都必须是这一文本；父上下文取消（`context.Canceled`）仍返回上下文错误。
  状态保存在内存中，完成后保留 10 分钟。`login.go` 拆分出可复用的“发起”“轮询”“校验”函数，
  现有 `Login` 行为与测试保持不变。


## 8. 配置（cmd/ghc2api）

```yaml
# 管理密钥；为空时禁用 Web 控制台与管理接口。必须与 api_key 不同。
management_key: ""
# 会话亲和：同一客户端会话固定使用同一账号，提高上游缓存命中率。默认 true。
session_affinity: true
# 会话绑定有效期，Go duration，必须大于 0，默认 2h。
session_affinity_ttl: "2h"
```

- `management_key` 去除首尾空白；非空且与 `api_key` 相同时启动失败。
- 设置了 `management_key` 时，credentials 目录不存在会自动创建（0700），允许 0 个账号启动；
  未设置时保持原有“目录不存在或没有凭证文件则启动失败”的行为。
- `session_affinity: false` 时传给 Pool 的 `SessionAffinityTTL` 为 0。
- `session_affinity_ttl` 必须是大于 0 的 Go duration，非法值（含 `0s`、负值）启动失败。
- 内部字段（供测试与实现对齐）：`appConfig.ManagementKey string`、`appConfig.SessionAffinity bool`、
  `appConfig.SessionAffinityTTL time.Duration`（`loadConfig` 解析并校验）；`accountConfig.Disabled bool`（来自凭证文件）。
- `func (c appConfig) poolConfig(logger *slog.Logger) copilot.PoolConfig`：生成服务级配置，包含 `APIKey`、`ManagementKey`、
  `CredentialsDir`（`DataPath/credentials`）、`CacheDir`（`DataPath/cache`）、`ModelsCacheTTL`、`SessionAffinityTTL`
  （关闭时为 0）、`Host`（顶层 host）、`Logger`。替代原 `copilotConfigs`。
- `func (c appConfig) poolAccounts() []copilot.PoolAccount`：由 `Accounts` 转换而来，保持文件名顺序。
- `loadConfig` 与 `serve` 都通过 `copilot.NewManagedPool(c.poolConfig(...), c.poolAccounts())` 创建池。
- serve 使用 `NewManagedPool`。

## 9. Web 控制台（web/）

- 从 `/Users/zc/Downloads/cpa-github-copilot/web` 复制并裁剪；使用 npm（`package-lock.json`），构建产物
  `web/dist/index.html` 提交到 git，`web/embed.go`（`package web`，导入路径 `ghc2api/web`）提供 `func Index() ([]byte, error)`。
- 页面：「概览」（可用账号数、累计请求、错误数、各账号请求统计表）与「账号与额度」（账号卡片：状态、
  Credits/Premium/Additional Budget 进度条与重置时间、请求统计；操作：刷新额度、启用/禁用、删除；
  工具栏：导入凭证 JSON、GitHub 设备登录）。登录页输入管理密钥，密钥只保存在内存中。
- 不包含参考项目中的 API Keys、运行日志、设置、优先级、模型列表、刷新凭据等功能。
- Makefile 新增 `web` 目标：`cd web && npm ci && npm run build`；`build` 不依赖 Node。

## 10. 测试依赖的内部名字（实现须遵循）

以下名字由测试直接使用，实现必须采用相同的名字与签名：

- `Pool.accounts []*poolAccount`；`poolAccount` 字段：`id string`、`service *Service`、`disabled bool`。
- `(*Pool).next() int`、`(*Pool).pick(tried []bool) int`（已有，语义不变）。
- `(*Service).setCooldown(time.Time)`、`(*Service).available(time.Time) bool`（已有）。
- `sessionKey(r *http.Request, body []byte, upstreamPath string) string`。
- `const maxSessionBindings = 4096`；`newAffinityTable(ttl time.Duration) *affinityTable`；
  `bind` / `lookup` / `count` 见第 3 节。
- `const DefaultSessionAffinityTTL = 2 * time.Hour`。
- `(*Service).fetchQuota(ctx context.Context) (Quota, error)`；`var errQuotaRateLimited error`。
- `parseQuota(raw []byte) (Quota, error)`；`Quota`、`QuotaSubscription`、`QuotaGroup`、`QuotaBucket`、`QuotaMetric` 字段与 JSON 名见第 4 节。
- `Credential.Disabled bool`（json `disabled,omitempty`）。
- `NewManagedPool`、`PoolConfig`、`PoolAccount`、`AccountView`（字段见第 2、7 节）。
- `(*Pool).Handler() http.Handler`：同时承载业务路由与管理路由（管理路由仅在 `ManagementKey` 非空时存在）。
- `ValidGitHubUserName(name string) bool`：导出，用户名规则见第 1 节，cmd 层与 internal 共用。
- `var quotaRefreshTimeout time.Duration`（默认 `10 * time.Second`，测试中临时改小；见第 4 节）。
- `(*poolAccount).quota` 字段（类型 `quotaCache`）及其 `snapshot() (*Quota, string)` 方法：测试读取 `quota_error` 文本，检查调用方取消时不写入错误（见第 4 节）。
- `(*Pool).findAccount(id string) *poolAccount`（忽略大小写）；`(*Service).cacheDir string` 字段，测试经 `(*Pool).findAccount("alice").service.cacheDir` 读取（删除账号后缓存目录不存在，见第 6 节）。
- cmd：`appConfig.ManagementKey`、`appConfig.SessionAffinity`、`appConfig.SessionAffinityTTL`、`accountConfig.Disabled`、
  `(appConfig).poolConfig(*slog.Logger) copilot.PoolConfig`（见第 8 节）。
- 已有且测试复用的辅助：`testClient`、`testJSON`、`testService`、`testDevice`（login_test.go）、`poolUpstream`（pool_test.go）。

