# 开发约束

- 不要过度设计，保持实现简洁且易于维护。
- 遵循测试优先原则，先编写测试再实现功能。编写测试和实现代码的不能是同一个 agent。
- 保持代码模块化，确保各部分职责单一且清晰。
- 保持良好的文档和注释，确保代码易于理解和维护。文档和注释语言使用中文。
- review 代码需要使用一个新的 agent，不能由实现功能的 agent 完成。
- 不要读取 .gitignore 列表中的文件或目录。

# 文档索引

按需阅读，不要一次性读取全部。修改某模块的行为时，同步更新对应文档。

| 文档 | 何时阅读 | 对应代码 |
| --- | --- | --- |
| `README.md` | 了解项目概况与快速开始 | — |
| `docs/config.md` | 修改配置项、启动流程、日志 | `cmd/ghc2api/config.go`、`main.go`、`logfile.go` |
| `docs/credentials.md` | 修改凭证格式、目录扫描、用户名校验、凭证读写 | `cmd/ghc2api/credentials.go`、`internal/copilot/credentials.go`、`username.go` |
| `docs/http-api.md` | 修改对外接口、鉴权、协议转发 | `internal/copilot/service.go`、`request.go`、`itemids.go`、`outputreplay.go` |
| `docs/pool.md` | 修改账号选择、故障切换、冷却、风控记录、Pool 构造 | `internal/copilot/pool.go`、`cooldown.go`、`weight.go`、`riskblock.go` |
| `docs/session-affinity.md` | 修改会话键、绑定表、持久化 | `internal/copilot/affinity*.go` |
| `docs/quota-stats.md` | 修改额度解析、刷新节流、持久化、请求统计 | `internal/copilot/quota.go` |
| `docs/management.md` | 修改管理接口、账号导入、设备登录 | `internal/copilot/management*.go`、`login.go` |
| `docs/web-console.md` | 修改控制台前端或其构建 | `web/` |
| `docs/upstream.md` | 修改 session 交换、模型列表、请求身份头 | `internal/copilot/session.go`、`models.go`、`cache.go` |
| `docs/development.md` | 运行测试、构建，查看来源与许可 | `Makefile` |
| `docs/magpie-copilot-sync.md` | 与 magpie 上游同步，同时核对 CLIProxyAPI 会话亲和键 | `internal/copilot/affinity_key.go`（cpa 部分） |
