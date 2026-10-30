# 上游 Copilot 交互

代码：`internal/copilot/` 的 `session.go`、`config.go`、`models.go`、`request.go`、`login.go`、`cache.go`、`username.go`。与 magpie 的对照与同步见 `magpie-copilot-sync.md`。

- **Session**：OAuth token 换取短期 Copilot session，在内存和磁盘复用，按 `expires_at` 判断有效期（剩余不足 2 分钟提前刷新）。
- **请求身份**：沿用 magpie 的 VS Code Copilot Chat 身份头与 API 版本；按最后一条输入区分用户与工具轮次，并识别图片请求。
- **模型列表**：缓存在内存和磁盘，默认 2 小时（`models_cache_ttl`）；过滤内部和不可用模型，首次使用有条款的模型时尝试接受。另记录 `/models` 中每个 chat 模型（含未展示的隐藏模型）的 `supported_endpoints`（空视为仅 `/chat/completions`），供生成前的路径校验使用（见 `http-api.md`）。
- **缓存**：写入 `<data_path>/cache`，按账号哈希分子目录，权限 `0700`/`0600`。
- **出站客户端**：每个账号一个 HTTP 客户端（`Service.httpClient()`，可原子替换），配置了 SOCKS5 代理时该账号的所有出站请求（session、模型列表、生成、额度）只经代理，不回退直连，见 `proxy.md`。
- **安全边界**：不转发客户端认证信息、Cookie、逐跳头；拒绝重定向与非 GitHub/Copilot 白名单的 session API 地址。
- **范围**：不含 magpie 的桌面 UI、WSL 和钥匙串凭证发现；只支持 OAuth token 流程。magpie 的 Auto 模型功能已有意删除，`model` 必须是具体模型。
