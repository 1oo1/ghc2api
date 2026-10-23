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
- 登录态：登录时用管理密钥调用 `POST /v0/management/session` 换取控制台令牌（见 `management.md`「控制台令牌」），之后所有请求只用令牌，管理密钥不保存。令牌与过期时间以 `{"token","expiresAt"}` 存于 `localStorage`（`web/src/session.ts`，键 `ghc2api.consoleSession`），跨标签页、重启浏览器后仍保持登录。
  - 打开页面时若本地令牌未过期，立即用它调用 `POST session` 续期（有效期重新计为 14 天）并替换保存的令牌；续期返回 401 则回到登录页，网络错误则继续用现有令牌。
  - 本地记录已过期、内容损坏时直接删除并显示登录页；退出登录或任一请求返回 401 时清除。
  - 读取时顺带删除旧版存于 `sessionStorage` 的明文密钥（键 `ghc2api.managementKey`）。
  - 存储不可用时静默忽略，退化为只存 React state。
- 安全：同源脚本可读取该令牌，风险已接受；控制台不得引入第三方脚本，也不应与其他应用共用同一源（协议+域名+端口）。退出登录只清除本地记录，不能吊销已泄露的令牌；需立即吊销时更换 `management_key`。
- 不包含 API Keys、运行日志、设置、模型列表、刷新凭据等功能。

## 构建

- 产物 `web/dist/index.html` 提交到 git，经 `go:embed` 嵌入程序。
- 修改 `web/` 源码后执行 `make web`（`cd web && npm ci && npm run build`，需 Node.js 与 npm）；`make build` 不依赖 Node。
- 前端测试位于 `web/src/*.test.ts`（`npm test`，即 `vitest run`）；类型检查与构建为 `npm run build`（先 `tsc --noEmit` 再 `vite build`）。
