# 配置、启动与日志

代码：`cmd/ghc2api/config.go`、`main.go`、`logfile.go`；字段说明见根目录 `config.example.yaml`。

## 启动

需要 Go 1.26.10+ 与有效的 GitHub Copilot OAuth 订阅。

```sh
cp config.example.yaml config.yaml && chmod 600 config.yaml
mkdir -p data/credentials   # 凭证放这里，见 credentials.md
make build
./bin/ghc2api               # 默认读取当前目录 config.yaml
./bin/ghc2api -c /path/to/config.yaml   # 启动服务时唯一的命令行参数
./bin/ghc2api healthcheck [-c /path/to/config.yaml]   # 健康检查，见下
```

- `healthcheck` 子命令（用于容器 `HEALTHCHECK`，见 `development.md`）：用与启动相同的方式加载配置与凭证（`-c` 规则同启动参数），请求其中 `listen` 的本机 `GET /healthz`（未指定的主机换成 `127.0.0.1`；不使用代理、不跟随重定向），请求超时 3 秒；200 退出码为 0，其他情况退出码为 1 并在 stderr 输出错误，不初始化日志文件。因此它判定的是"磁盘上的配置与凭证能被启动流程加载，且该地址上的服务健康"：运行中改坏配置或凭证目录、改了 `listen` 但未重启，都会报不健康。

- `config.yaml` 是唯一配置来源，不支持 flag 或环境变量覆盖。
- `data_path`（默认 `./data`）、`logs_path`（默认 `./logs`）的相对路径以配置文件所在目录为基准。
- 配置中已移除的 `credentials` 字段会被拒绝（报"不是有效 YAML"）。
- 只接受 GitHub OAuth token，不支持 Copilot CLI token。

## 监听与鉴权

- 默认只监听 `127.0.0.1:8080`；公网部署还需反向代理与 TLS。
- `api_key` 是本服务的客户端访问密钥（全局，不分账号），不是 GitHub token，**必填**：去除首尾空白后为空时启动失败，与监听地址无关（本机与非本机地址都一样）。错误信息包含 `api_key`。
- 不存在匿名模式。配置文件中的 `allow_anonymous` 键已被移除，出现即因未知字段而启动失败（报"不是有效 YAML"，错误信息包含 `allow_anonymous`）。
- `api_key` 与 `management_key` 去除首尾空白后非空时，字节长度必须至少 32（按字节而非字符计算），否则启动失败，错误信息包含字段名。
- `listen` 只校验格式与端口（`host:port`，端口 0 到 65535；省略或留空时为 `127.0.0.1:8080`）。`127.0.0.1`、`::1`、`localhost` 等本机地址没有特殊待遇，鉴权规则与其他地址相同。
- `http.Server` 的请求头上限为 64 KiB（`MaxHeaderBytes`，Go 实际另有约 4 KiB 余量），超出时由 Go 的 HTTP 服务直接拒绝。
- 生成接口向客户端写响应时，每次写入（含刷新）前续期 60 秒的写超时（`generationWriteTimeout`，见 `http-api.md`）；服务器没有全局 `WriteTimeout`，因此不会截断长时间的生成。

## 管理与会话亲和相关配置

```yaml
api_key: ""                   # 必填，去首尾空白后至少 32 字节；不接受 allow_anonymous 键
management_key: ""            # 空则禁用控制台与管理接口；必须与 api_key 不同，非空时至少 32 字节
session_affinity: true        # 默认 true
session_affinity_ttl: "2h"    # Go duration，必须 > 0，默认 2h
session_affinity_max_per_account: 5   # 整数，>= 0，默认 5；0 表示不限制
account_cooldown: "2h"        # Go duration，必须 > 0，默认 2h：账号进入冷却后不参与选择的时长
max_concurrent_requests: 32   # 整数，>= 0，默认 32；0 表示不限制
quota_refresh_interval: "1h"  # Go duration，必须 >= 1m，默认 1h
```

- `management_key` 去除首尾空白；非空且与 `api_key` 相同、或长度不足 32 字节时启动失败。
- 设置了 `management_key` 时，凭证目录不存在会自动创建（0700），允许 0 个账号启动。
- `session_affinity: false` 时传给 Pool 的 `SessionAffinityTTL` 为 0；TTL 非法（含 `0s`、负值）启动失败。
- `session_affinity_max_per_account` 是每个账号的有效会话绑定数上限（见 `session-affinity.md`），缺省为 `5`，`0` 表示不限制。负数、非整数（如 `abc`、`1.5`）与显式空值（`""`）启动失败，错误信息包含字段名 `session_affinity_max_per_account`。关闭 `session_affinity` 时该项不生效，但仍会校验。传给 Pool 的 `PoolConfig.SessionAffinityMaxPerAccount` 与之相同。
- `account_cooldown` 是账号进入冷却后暂停参与选择的时长（规则见 `pool.md`「限流、错误计数与冷却」）。缺省为 `2h`；无法解析、非正（如 `0s`、`-1m`）或显式空值（`""`）启动失败，错误信息包含字段名 `account_cooldown`。写成 YAML null（`account_cooldown:` 或 `account_cooldown: ~`，即只写键名、不写值）视为未设置，取 `2h`，与其他可选键一致。`appConfig.AccountCooldown`（`yaml:"-"`）为解析后的值，`poolConfig` 把它填入 `PoolConfig.AccountCooldown`。
- `max_concurrent_requests` 是同时处理的生成请求数上限（见 `pool.md` 的「生成请求并发上限」与 `http-api.md`）。缺省为 `32`，显式 `0` 表示不限制；负数与非整数启动失败，错误信息包含字段名 `max_concurrent_requests`。写成空值（`max_concurrent_requests:` 即 YAML null）时按未设置处理，取 `32`。传给 Pool 的 `PoolConfig.MaxConcurrentRequests` 与之相同。
- `quota_refresh_interval` 是额度的节流窗口，也是后台定时刷新的判断依据（见 `quota-stats.md`）。无法解析、非正或小于 `1m` 的值启动失败，错误信息包含字段名 `quota_refresh_interval`；省略时为 `1h`。它与是否设置 `management_key` 无关。
- `appConfig.poolConfig(logger)` 生成 `copilot.PoolConfig`（`CredentialsDir=DataPath/credentials`、`CacheDir=DataPath/cache`、`QuotaRefreshInterval` 等），`poolAccounts()` 把凭证转换为 `[]copilot.PoolAccount`（保持文件名顺序）；`loadConfig` 与 `serve` 都经 `NewManagedPool` 建池。
- `serve` 在 HTTP 服务开始监听后启动 `pool.RunQuotaRefresh`，随退出信号（或 serve 返回）取消；它不参与 HTTP 关闭等待，也不影响会话亲和的保存顺序。
- 关闭时保存会话亲和表前，`saveSessionAffinity` 最多等待 5 秒进入冷却的异步副作用（`Pool.WaitCooldownEffectsTimeout`，清缓存、删绑定、写 `cooldown.json`）。超时记 Warn「会话亲和：等待冷却副作用超时，继续保存」并照常保存，因此关闭耗时至多增加 5 秒。

## 日志

- JSON 格式写入 `<logs_path>/ghc2api.log` 并默认同步输出到标准错误；`log_to_stderr: false` 时只写文件，终端不再打印（启动失败等致命错误仍输出到标准错误；容器中关闭后 `docker logs` 看不到服务日志，需从 `logs_path` 读取）；级别 `debug`/`info`/`warn`/`error`。
- `log_to_stderr: false` 时没有兜底：日志文件写入失败（如磁盘满），或单条记录超过 `log_max_size_mb`（`debug` 级别记录完整上游正文时可能发生）时，该记录会被丢弃，且不会输出到标准错误；默认的同时输出模式下标准错误仍有这些记录。
- 单文件上限默认 10 MiB，保留 5 个备份；用 `log_max_size_mb`、`log_max_backups` 调整（备份数 `0` 表示不保留）。
- 同一 `logs_path` 只应由一个进程使用。
- `debug` 记录脱敏后的完整上游请求/响应正文，可能含提示词等业务数据；排障后应恢复 `info`。认证头、凭证字段、已知 token 会脱敏。
- 只有鉴权失败（业务 `api_key`，或管理接口的控制台令牌/session 接口的 `management_key`，返回 401）的请求不输出"收到客户端请求"与"客户端请求处理完成"，只输出一条 WARN 告警，字段为 `method`、`path`（各自最多 256 字节，按 UTF-8 字符边界截断）与 `remote_addr`。其他请求（包括 `/healthz`、`/favicon.ico`、控制台页面，以及 ServeMux 自身产生的 405、404 与路径清理重定向）都输出这两条日志。
- 日志中 `account` 为账号在当前列表中的下标，`user` 为用户名。

## 文件权限

- 缓存目录 `0700`、缓存文件 `0600`；日志目录仅新建时 `0700`，日志文件 `0600`。
- Windows 使用目录继承的默认 ACL，程序不修改 DACL。
