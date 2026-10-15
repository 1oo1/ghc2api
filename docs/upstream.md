# 上游 Copilot 交互

代码：`internal/copilot/` 的 `session.go`、`config.go`、`models.go`、`auto.go`、`request.go`、`login.go`、`cache.go`、`username.go`。与 magpie 的对照与同步见 `magpie-copilot-sync.md`。

- **Session**：OAuth token 换取短期 Copilot session，在内存和磁盘复用，按 `expires_at` 判断有效期（剩余不足 2 分钟提前刷新）。
- **请求身份**：沿用 magpie 的 VS Code Copilot Chat 身份头与 API 版本；按最后一条输入区分用户与工具轮次，并识别图片请求。
- **模型列表**：缓存在内存和磁盘，默认 2 小时（`models_cache_ttl`）；过滤内部和不可用模型，首次使用有条款的模型时尝试接受。
- **Auto**：`auto` 优先请求 `/auto`，再试 `/models/session`；均不可用则选账号可手动使用的模型。Auto 缓存最多 1 小时，拒绝后最多 3 次生成尝试。
- **缓存**：写入 `<data_path>/cache`，按账号哈希分子目录，权限 `0700`/`0600`。
- **安全边界**：不转发客户端认证信息、Cookie、逐跳头；拒绝重定向与非 GitHub/Copilot 白名单的 session API 地址。
- **范围**：不含 magpie 的桌面 UI、WSL 和钥匙串凭证发现；只支持 OAuth token 流程。
