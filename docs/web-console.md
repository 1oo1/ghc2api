# Web 控制台

代码：`web/`（React + vite + npm），`web/embed.go`（`package web`，`Index() ([]byte, error)`）。后端接口见 `management.md`。

- 访问 `http://127.0.0.1:8080/management`，输入管理密钥登录。
- 页面：
  - 「概览」：可用账号数（未禁用且未冷却）、累计请求、错误数、各账号请求统计表（状态列与账号卡片相同）。
  - 「账号与额度」：账号卡片显示状态、Credits/Premium/Additional Budget 进度条与重置时间、请求统计；可刷新额度、启用/禁用、改优先级（数字框+保存）、删除；工具栏可导入凭证 JSON、GitHub 设备登录。
- 账号状态徽标（`web/src/account.ts`）：「启用中」（绿色）、「禁用中」（灰色）、「冷却中」（黄色）。徽标优先级为禁用 > 冷却 > 启用。冷却中的徽标附带触发原因，如「冷却中 · 403」，网络错误显示为「冷却中 · 网络错误」，没有原因时只显示「冷却中」。
- 冷却与禁用相互独立：账号既禁用又冷却中时，徽标仍为「禁用中」，但卡片同样显示冷却原因（一行提示）与冷却截止时间，并显示「解除冷却」按钮。是否显示冷却信息与「解除冷却」按钮由 `isCooling(account, now)` 判断，即冷却截止时间晚于当前时间，与是否禁用无关。
- 冷却中（无论是否禁用）的账号卡片显示「解除冷却」按钮，调用 `PATCH /v0/management/accounts` 的 `clear_cooldown`（`ManagementClient.clearCooldown(id)`，`web/src/api.ts`），成功后提示并刷新列表。
- 版本：侧边栏品牌区显示服务端版本号（`GET /v0/management/version`）；加载失败时不显示。
- 登录态：管理密钥存于当前标签页 `sessionStorage`（`web/src/session.ts`，键 `ghc2api.managementKey`），刷新保持登录；关闭标签页、退出或密钥失效时清除；存储不可用时静默忽略，退化为只存 React state。
- 安全：同源脚本可读取该密钥，风险已接受；控制台不得引入第三方脚本。
- 不包含 API Keys、运行日志、设置、模型列表、刷新凭据等功能。

## 构建

- 产物 `web/dist/index.html` 提交到 git，经 `go:embed` 嵌入程序。
- 修改 `web/` 源码后执行 `make web`（`cd web && npm ci && npm run build`，需 Node.js 与 npm）；`make build` 不依赖 Node。
- 前端测试位于 `web/src/*.test.ts`（`npm test`，即 `vitest run`）；类型检查与构建为 `npm run build`（先 `tsc --noEmit` 再 `vite build`）。
