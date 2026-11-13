# 上游 Copilot 交互

代码：`internal/copilot/` 的 `session.go`、`config.go`、`models.go`、`request.go`、`login.go`、`cache.go`、`username.go`。与 magpie 的对照与同步见 `magpie-copilot-sync.md`。

- **Session**：OAuth token 换取短期 Copilot session，在内存和磁盘复用，按 `expires_at` 判断有效期（剩余不足 2 分钟提前刷新）。
- **请求身份**：沿用 magpie 的 VS Code Copilot Chat 身份头与 API 版本；按最后一条输入区分用户与工具轮次，并识别图片请求。
- **模型列表**：缓存在内存和磁盘，默认 2 小时（`models_cache_ttl`）；只展示 chat 模型，并过滤内部和不可用模型：内部 id（`copilot-search`、`exec-agent`、`trajectory` 开头，或以 `-secondary`、`-tertiary`、`-4th`、`-free-auto` 结尾）一律不展示；`model_picker_enabled` 为 false 的模型在没有 `model_picker_category` 或 `vendor` 为 `Experimental` 时不展示（有 category 的非 Experimental 模型展示）；`vendor` 为 `Experimental` 且 picker 启用的模型与其他模型一样按策略规则展示——策略为 `enabled`、没有策略且 picker 启用的展示，策略未启用但带条款的展示并记入待接受（首次使用时尝试接受），其余跳过（对应 magpie #4343）。另记录 `Disabled`：策略明确不是 `enabled` 且没有条款的 chat 模型（不论内部 id、`Experimental`、picker 启用与否，它们不进可见列表），随模型缓存持久化（`models.json` 的 `disabled`，旧缓存没有该字段视为空）。账号每次加载或清空模型列表时同步更新一份 id 集合快照（可见模型，加上 `/models` 中未被 `Disabled` 标记的隐藏 chat 模型），`(*Service).modelAvailability` 据此回答账号是否有某模型而不阻塞，供账号池选号按模型过滤（见 `pool.md`）；生成前的路径校验不看 `Disabled`，仍可手动指定隐藏模型。另记录 `/models` 中每个 chat 模型（含未展示的隐藏模型）的 `supported_endpoints`（空视为仅 `/chat/completions`），供生成前的路径校验使用（见 `http-api.md`）。
- **缓存**：写入 `<data_path>/cache`，按账号哈希分子目录，权限 `0700`/`0600`。
- **出站客户端**：每个账号一个 HTTP 客户端（`Service.httpClient()`，可原子替换），配置了 SOCKS5 代理时该账号的所有出站请求（session、模型列表、生成、额度）只经代理，不回退直连，见 `proxy.md`。
- **安全边界**：不转发客户端认证信息、Cookie、逐跳头；拒绝重定向与非 GitHub/Copilot 白名单的 session API 地址。
- **范围**：不含 magpie 的桌面 UI、WSL 和钥匙串凭证发现；只支持 OAuth token 流程。magpie 的 Auto 模型功能已有意删除，`model` 必须是具体模型。
