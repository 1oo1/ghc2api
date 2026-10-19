# 管理接口

代码：`internal/copilot/management.go`、`management_login.go`、`login.go`。前端见 `web-console.md`。

## 启用与鉴权

- 仅 `management_key` 非空（去除首尾空白后至少 32 字节，且不同于 `api_key`）时注册。留空则 `/management` 与 `/v0/management/*` 不存在：与其他未注册路径一样先校验服务密钥，无有效 `api_key` 返回 401，有则 404。
- 鉴权 `Authorization: Bearer <management_key>`（常量时间比较），失败 401 并输出一条鉴权失败告警（字段 `method`、`path` 各最多 256 字节，按 UTF-8 字符边界截断，以及 `remote_addr`），不输出请求日志。`api_key` 与 `X-Api-Key` 均不能访问管理接口；`/management` 页面本身无需鉴权。管理密钥校验只作用于 `/v0/management/` 前缀，且位于请求日志之外。路径清理重定向（如 `/v0/management//accounts`）同样发生在该前缀下，因此必须先通过管理密钥校验才会被重定向；`/management` 上由 ServeMux 直接产生的 405、404 不需要管理密钥，照常输出请求日志。
- 响应均为 JSON，错误格式同 `writeError`；请求体上限 1 MiB。
- 密钥通过 HTTP 头发送，监听非本机地址时应置于 TLS 反向代理之后。
- 控制台响应带 `X-Frame-Options: DENY` 与 `Content-Security-Policy`（含 `default-src 'self'`、`frame-ancestors 'none'`；单文件构建内联 script/style，故 script-src、style-src 允许 `'unsafe-inline'`）。

## 接口

| 方法与路径 | 请求 | 响应 |
| --- | --- | --- |
| `GET /management` | — | 控制台 HTML（`web.Index()`） |
| `GET /v0/management/version` | — | `{"version":"v0.0.1"}`，程序版本号（`copilot.Version`），控制台侧边栏显示 |
| `GET /v0/management/accounts` | — | `[]AccountView`，顺带刷新过期额度（见 `quota-stats.md`） |
| `POST /v0/management/accounts` | `{"credential":{"oauth_token":"...","host":"","user":""}}` | `200 AccountView` |
| `PATCH /v0/management/accounts` | `{"id":"alice","disabled":true,"priority":50}` | `200 AccountView` |
| `DELETE /v0/management/accounts?id=alice` | — | `200 {"ok":true}` |
| `POST /v0/management/quota/refresh` | `{"id":"alice"}` | `200 AccountView`（强制刷新） |
| `POST /v0/management/login` | — | `200 {"state","url","user_code","interval","expires_in"}` |
| `GET /v0/management/login/status?state=...` | — | `{"status":"pending"\|"success"\|"error","id"?,"error"?}`；未知 state 404 |

```go
type AccountView struct {
    ID, User, Host string        // Host 空表示 github.com
    Disabled       bool
    Priority       int           // -1..100，始终输出
    CooldownUntil  *time.Time    // 仅冷却中输出
    Stats          struct{ Requests, Errors uint64; LastUsedAt *time.Time }
    Quota          *Quota
    QuotaError     string
}
```

JSON 字段名为 `id`、`user`、`host`、`disabled`、`priority`、`cooldown_until`、`stats{requests,errors,last_used_at}`、`quota`、`quota_error`。不含 OAuth token。

## 行为约定

- 列表按账号池顺序（启动为凭证文件名顺序，新增追加末尾）。
- 新增、删除、禁用、改优先级立即生效，无需重启；删除不影响进行中的请求。
- 400：请求体非法 JSON；PATCH 缺 `id`、`disabled` 与 `priority` 都缺、`priority` 超出 [-1,100] 或非整数（均不写文件、不改状态）；DELETE/refresh 缺 `id`。404：`id` 不存在。写入失败 500。
- 凭证写入、同名冲突与登录名规则见 `credentials.md`。

## 导入账号

校验 token 与 host（顶层 Host 非空时覆盖），用 token 请求 GitHub `/user` 得登录名，再请求 `/copilot_internal/user` 确认订阅（复用 `finishLogin`）。

- 400：`user` 非空且与登录名不一致（忽略大小写）、登录名不合规、上游校验失败（消息不含 token）。
- 409：登录名或 Host+Token 已在池中，或凭证目录已有同名文件。
- 成功后写凭证文件、加入池末尾，优先级 0；随即在后台异步强制刷新一次该账号的额度（不节流，忽略从缓存恢复的 `checked_at`；不阻塞响应，失败只记入 `quota_error`，见 `quota-stats.md`）。因此响应里的账号视图可能尚无额度，之后的列表即可看到结果。

## 设备登录

`POST login` 发起 GitHub Device Flow（用顶层 Host），返回 32 字符十六进制 `state`、验证页 URL、`user_code`；后台 goroutine 按 `login.go` 的轮询规则（slow_down、过期、连续 5 次失败）等待授权，成功后按导入规则保存并加入池。

- 成功 `status=success` 带新账号 `id`；失败 `status=error` 带不含 token 的 `error`（同名冲突亦如此）。
- 入池后与导入相同，立即异步刷新一次额度；`status=success` 返回时该刷新可能尚未完成。
- 验证码在授权前过期（`pollCtx` 超时而父上下文未取消）时，错误为 `验证码已过期，请重新登录`（CLI `Login` 与 Web 一致）；父上下文取消仍返回上下文错误。
- 状态保存在内存，完成后保留 10 分钟。`login.go` 拆出可复用的发起/轮询/校验函数，CLI `Login` 行为不变。
