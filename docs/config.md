# 配置、启动与日志

代码：`cmd/ghc2api/config.go`、`main.go`、`logfile.go`；字段说明见根目录 `config.example.yaml`。

## 启动

需要 Go 1.23+ 与有效的 GitHub Copilot OAuth 订阅。

```sh
cp config.example.yaml config.yaml && chmod 600 config.yaml
mkdir -p data/credentials   # 凭证放这里，见 credentials.md
make build
./bin/ghc2api               # 默认读取当前目录 config.yaml
./bin/ghc2api -c /path/to/config.yaml   # 唯一的命令行参数
```

- `config.yaml` 是唯一配置来源，不支持 flag 或环境变量覆盖。
- `data_path`（默认 `./data`）、`logs_path`（默认 `./logs`）的相对路径以配置文件所在目录为基准。
- 配置中已移除的 `credentials` 字段会被拒绝（报"不是有效 YAML"）。
- 只接受 GitHub OAuth token，不支持 Copilot CLI token。

## 监听与鉴权

- 默认只监听 `127.0.0.1:8080`；监听非本机地址时必须设置 `api_key`，公网部署还需反向代理与 TLS。
- `api_key` 是本服务的客户端访问密钥（全局，不分账号），不是 GitHub token。

## 管理与会话亲和相关配置

```yaml
management_key: ""            # 空则禁用控制台与管理接口；必须与 api_key 不同
session_affinity: true        # 默认 true
session_affinity_ttl: "2h"    # Go duration，必须 > 0，默认 2h
session_affinity_max_per_account: 5   # 整数，>= 0，默认 5；0 表示不限制
quota_refresh_interval: "1h"  # Go duration，必须 >= 1m，默认 1h
```

- `management_key` 去除首尾空白；非空且与 `api_key` 相同时启动失败。
- 设置了 `management_key` 时，凭证目录不存在会自动创建（0700），允许 0 个账号启动。
- `session_affinity: false` 时传给 Pool 的 `SessionAffinityTTL` 为 0；TTL 非法（含 `0s`、负值）启动失败。
- `session_affinity_max_per_account` 是每个账号的有效会话绑定数上限（见 `session-affinity.md`），缺省为 `5`，`0` 表示不限制。负数、非整数（如 `abc`、`1.5`）与显式空值（`""`）启动失败，错误信息包含字段名 `session_affinity_max_per_account`。关闭 `session_affinity` 时该项不生效，但仍会校验。传给 Pool 的 `PoolConfig.SessionAffinityMaxPerAccount` 与之相同。
- `quota_refresh_interval` 是额度的节流窗口，也是后台定时刷新的判断依据（见 `quota-stats.md`）。无法解析、非正或小于 `1m` 的值启动失败，错误信息包含字段名 `quota_refresh_interval`；省略时为 `1h`。它与是否设置 `management_key` 无关。
- `appConfig.poolConfig(logger)` 生成 `copilot.PoolConfig`（`CredentialsDir=DataPath/credentials`、`CacheDir=DataPath/cache`、`QuotaRefreshInterval` 等），`poolAccounts()` 把凭证转换为 `[]copilot.PoolAccount`（保持文件名顺序）；`loadConfig` 与 `serve` 都经 `NewManagedPool` 建池。
- `serve` 在 HTTP 服务开始监听后启动 `pool.RunQuotaRefresh`，随退出信号（或 serve 返回）取消；它不参与 HTTP 关闭等待，也不影响会话亲和的保存顺序。

## 日志

- JSON 格式写入 `<logs_path>/ghc2api.log` 并同步输出到标准错误；级别 `debug`/`info`/`warn`/`error`。
- 单文件上限默认 10 MiB，保留 5 个备份；用 `log_max_size_mb`、`log_max_backups` 调整（备份数 `0` 表示不保留）。
- 同一 `logs_path` 只应由一个进程使用。
- `debug` 记录脱敏后的完整上游请求/响应正文，可能含提示词等业务数据；排障后应恢复 `info`。认证头、凭证字段、已知 token 会脱敏。
- 日志中 `account` 为账号在当前列表中的下标，`user` 为用户名。

## 文件权限

- 缓存目录 `0700`、缓存文件 `0600`；日志目录仅新建时 `0700`，日志文件 `0600`。
- Windows 使用目录继承的默认 ACL，程序不修改 DACL。
