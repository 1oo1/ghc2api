# 账号代理（SOCKS5）

代码：`internal/copilot/proxy.go`（解析、校验、构造走代理的客户端），接入点见下文。依赖 `golang.org/x/net/proxy`。

每个账号可配置一个 SOCKS5 代理，默认为空（直连）。账号配置了代理时，该账号的**所有**出站请求都经由该代理，任何情况下都不回退直连。

## 凭证字段

凭证 JSON 新增可选字段 `proxy`（`omitempty`），值为代理 URL：

```json
{ "oauth_token": "gho_...", "proxy": "socks5://user:pass@127.0.0.1:1080" }
```

校验规则（`LoadCredential`、`SaveCredential`、管理接口共用，失败时错误信息不含密码）：

- 去除首尾空白；空字符串表示无代理。
- scheme 只能是 `socks5` 或 `socks5h`（二者行为相同，见下文 DNS），大小写不敏感，保存时保持原样。
- 必须有主机和端口，端口 1–65535；不能有路径（`/` 除外）、query、fragment。
- 用户名、密码可选；有密码时必须有用户名；用户名与密码各不超过 255 字节（RFC 1929）。用户名、密码中的特殊字符按 URL 百分号编码。
- 不合法时报错 `代理地址无效: <原因>`。校验函数为 `normalizeProxy(raw string) (string, error)`：返回去除首尾空白后的字符串（scheme 保持原样），空串返回空串。`Config.Proxy`、`PoolAccount.Proxy`、`Credential.Proxy` 都是该格式的字符串；`New`/`NewPool`/`NewManagedPool` 同样校验，非法时构造失败。

## 行为

- **范围**：session 交换、模型列表、生成请求（chat completions、responses、`/v1/messages`）、额度刷新，以及管理接口中该账号的导入校验、设备登录（申请 device code、轮询 token、查询 `/user`）、修改代理时的连通性验证，都走该账号的代理。
- **不绕过**：走代理的客户端忽略环境变量代理（`HTTP_PROXY` 等），所有 TCP 连接只通过 SOCKS5 dialer 建立；DNS 由代理端解析（客户端把域名原样发给代理），本地不解析目标域名。连接代理服务器本身使用独立的 `net.Dialer`（超时 10 秒）。
- **构造**：`proxyClient(base *http.Client, proxy string) (*http.Client, error)`。`proxy` 为空时等同 `safeClient(base)`；否则先经 `safeClient(base)`（`base` 为 nil 时得到带 `ResponseHeaderTimeout` 的默认 Transport 克隆），再克隆其 `*http.Transport`（`Transport` 为 nil 时用 `http.DefaultTransport` 的克隆）（保留 TLS 配置等），然后设置 `Proxy = nil`、`DialTLSContext = nil`、`DialContext` 为 SOCKS5 dialer；`base.Transport` 是其他类型时返回错误（无法保证不绕过代理）。返回的客户端同样经过 `safeClient` 处理（禁止重定向、无 Jar）。
- **错误**：经代理建立连接失败时，错误信息为 `代理 <host:port> 连接失败: <原因>`，不含用户名和密码，并随现有错误链原样返回给调用方（生成接口为 502，管理接口按各自规则）。
- **故障切换**：代理失败按网络错误处理（计数、冷却原因 `network`，规则同 `pool.md`），session 获取阶段失败照常切换到其他账号；其他账号使用各自的代理或直连，不算绕过。所有账号都失败时返回最后一个账号的错误，其中含代理失败原因。
- **脱敏**：代理密码加入账号的脱敏集合（与 token 相同），日志与返回给客户端的错误中不出现。
- **运行时修改**：`Service` 的 HTTP 客户端可原子替换。修改代理后，新发起的请求立即使用新客户端；进行中的请求继续在旧连接上完成；旧客户端的空闲连接被关闭。session 缓存不清除。

## 管理接口

`AccountView` 新增 `proxy`（无代理时省略）：

```json
"proxy": { "scheme": "socks5", "host": "127.0.0.1", "port": 1080, "username": "user", "has_password": true }
```

不返回密码；`username` 为空时省略；`has_password` 始终输出；`scheme` 保持保存时的写法（如 `socks5h`）；IPv6 的 `host` 不带方括号（如 `::1`）。

| 接口 | 变化 |
| --- | --- |
| `POST /v0/management/accounts` | `credential.proxy` 可选。非空时先校验格式（400），再**通过该代理**校验 token；保存的凭证文件包含 `proxy`，新账号使用该代理。 |
| `POST /v0/management/login` | 请求体可选：空体或 `{"proxy":"socks5://..."}`。非空代理先校验格式（400）；申请 device code、后台轮询、查询 `/user` 都走该代理；成功保存的凭证包含 `proxy`。 |
| `PATCH /v0/management/accounts` | 新增 `proxy`（字符串，未提供表示不变，`""` 表示删除代理）与 `proxy_keep_password`（布尔，默认 false）。 |

PATCH 的代理规则：

- `proxy_keep_password` 为 true 且新 URL 不含密码时，仅当新 URL 的主机、端口、用户名都与当前代理相同（主机与 scheme 忽略大小写）才沿用当前代理的密码（当前无密码则仍无密码）；三者任一不同则不沿用，按新 URL（无密码）处理，避免把旧密码发给另一台代理。新 URL 自带密码时以新 URL 为准。
- 设置或修改代理（非空且与当前值不同）时，先通过新代理用该账号 token 请求 GitHub `/user` 验证连通性，失败返回 502，错误信息为验证失败原因（含代理失败原因），凭证文件与内存都不变。返回的登录名与账号 ID 不一致（忽略大小写）时返回 400。
- 删除代理（`""`）不做验证。
- 新代理与当前值相同时视为未变化，不验证也不写文件。
- 与 `disabled`、`priority`、`clear_cooldown` 可组合提供：先验证代理，再一次重写凭证文件（写入失败返回 500，全部不变），然后同步内存（含替换 HTTP 客户端），最后处理 `clear_cooldown`。修改代理不自动解除冷却。

## 控制台

- 工具栏「导入凭证 JSON」与「GitHub 登录」点击后，先弹出代理弹窗（可跳过/留空）：主机、端口、用户名、密码四个输入框，scheme 固定 `socks5`。填写了主机时端口必填；未填主机却填了用户名或密码时校验失败（提示填写主机或清空用户名和密码）。确认后进入原有流程；导入时填写的代理写入 `credential.proxy`（覆盖粘贴 JSON 中的 `proxy`），留空则沿用 JSON 中的 `proxy`（若有），弹窗提示文案需如实说明（导入：「留空则使用凭证 JSON 中的 proxy（若有），否则直连」；登录：「留空则直连」）；登录时作为 `POST login` 的 `proxy`。
- 账号卡片显示「代理：无代理」或「代理：host:port」（IPv6 主机加方括号）。点击打开代理弹窗：
  - 预填 host、port、username；密码框留空，占位提示「留空保持原密码」（当前有密码且主机、端口、用户名未改变时；改变后提示「地址或用户名已修改，需重新填写密码」）。修改时以 `proxy_keep_password: true` 提交（用户填写了新密码则以新密码为准）。
  - 「保存」：表单与当前值相比没有变化时置灰禁用（密码框为空且未勾选清除视为未变化）；保存中显示进行状态，失败时在弹窗内显示错误，不关闭弹窗。
  - 当前有代理时，主机不能清空：主机为空时校验失败（提示删除请使用「删除」按钮），「保存」置灰，避免误操作绕过二次确认把账号改为直连。
  - 有代理时提供「清除密码」勾选，勾选后提交时不带 `proxy_keep_password`。
  - 「删除」：仅在当前有代理时显示，二次确认（`ConfirmDialog`）后提交 `proxy: ""`。
  - 无代理的账号打开弹窗时为空表单，只有「保存」。
- 代理 URL 由前端拼装：用户名、密码使用 `encodeURIComponent`；IPv6 主机加方括号。表单逻辑为纯函数（`web/src/proxy.ts`）：`buildProxyUrl`、`validateProxyForm`、`proxyFormChanged`、`proxyLabel`、`buildProxyPatch`；`proxy_keep_password` 仅在当前代理有密码、密码框为空、未勾选「清除密码」且主机、端口、用户名都未改变时提交；没有当前代理时，表单未填主机视为未变化。`ManagementClient` 对应 `setProxy(id, { proxy, proxy_keep_password? })`、`importAccount(credential, proxy?)`、`startLogin(proxy?)`。
