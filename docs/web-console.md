# Web 控制台

代码：`web/`（React + vite + npm），`web/embed.go`（`package web`，`Index() ([]byte, error)`）。后端接口见 `management.md`。

- 访问 `http://127.0.0.1:8080/management`，输入管理密钥登录。
- 布局：顶栏（品牌、版本号、页面切换「账号」「日志」、「↻ 刷新」「退出登录」）+ 当前页面，无侧边栏导航。当前页的切换按钮带 `aria-current="page"`。「↻ 刷新」作用于当前页面：账号页刷新账号列表，日志页重新加载日志。账号列表的 15 秒自动刷新照常进行（账号页切走后仍保持挂载，设备登录的轮询不受影响）；日志页不自动刷新。
- 账号页（`web/src/pages/AccountsPage.tsx`）自上而下：
  - 指标：可用账号数（未禁用且未冷却）、累计请求、错误数。
  - 工具栏：说明文字，「导入凭证 JSON」「GitHub 登录」。
  - 账号列表（`web/src/components/AccountTable.tsx`），只做展示，列为：账号（头像、用户名、订阅计划 `planLabel`）、状态（徽标；冷却中时下方附「至 截止时间」，禁用且冷却中时附冷却原因）、额度（每个额度窗口一行：名称、迷你进度条、已用百分比、重置时间；已用 Credits `creditsUsed`；额度提示 `quotaWarnings`，即查询失败与数据过期，每条一行，过长省略、悬停看全文）、请求、错误、最近使用、优先级、代理。
  - 排序（`web/src/sort.ts`）：额度、请求、错误、最近使用、优先级可点击表头排序，依次为降序 → 升序 → 默认顺序（服务端返回的顺序），表头箭头与 `aria-sort` 标示当前方向；额度按已用比例最高的窗口排序。没有额度窗口（只有 Credits 用量也算）、从未使用的账号无论升降序都排在最后，值相同时保持原顺序。排序状态不持久化，刷新页面或账号清空后恢复默认顺序。按请求、最近使用等会变化的列排序时，15 秒自动刷新可能改变行的顺序。
  - 点击行（键盘用户聚焦账号名按钮后按回车/空格）弹出该账号的卡片（`web/src/components/AccountDialog.tsx`，模态框，内容即 `AccountCard`）；Esc、点击遮罩或「×」关闭，关闭后焦点回到账号名按钮（账号已删除时除外）。弹窗按账号 id 从最新列表取数据，自动刷新后内容随之更新，账号被删除后自动关闭。卡片内嵌的确认、代理弹窗的 Esc 只关闭自身，不会连带关闭账号弹窗。
- 账号卡片：显示状态、订阅计划、Credits/Premium/Additional Budget 进度条与重置时间、请求统计；可刷新额度、启用/禁用、改优先级（数字框+保存）、删除；禁用与删除会先弹出确认对话框（`web/src/components/ConfirmDialog.tsx`，「取消」/「确认」，默认聚焦「取消」，Esc 取消），确认后才执行，启用无需确认。
  - 账号卡片显示「代理：无代理」或「代理：host:port」（IPv6 加方括号），点击打开代理弹窗（`web/src/components/ProxyDialog.tsx`）：预填主机、端口、用户名，密码框留空（有密码时占位「留空保持原密码」，主机、端口、用户名任一修改后改为「地址或用户名已修改，需重新填写密码」，见 `passwordPlaceholder`；`proxy_keep_password` 只在这三项都未改变时提交，服务端同样只在此时沿用密码）；当前有代理时主机不能清空（校验失败，提示使用「删除」按钮）；「保存」在表单无变化（`proxyFormChanged`）或校验失败时置灰，保存中显示「处理中…」，失败时错误显示在弹窗内且弹窗保持打开（服务端会先通过新代理验证，最长约 30 秒）；有代理时提供「清除密码」勾选与「删除」（经 `ConfirmDialog` 二次确认后提交 `proxy: ""`）。
  - 工具栏「导入凭证 JSON」「GitHub 登录」点击后先弹出同一个代理弹窗（可留空跳过；未填主机却填了用户名或密码时校验失败）；导入时代理写入 `credential.proxy`（覆盖 JSON 中的 `proxy`），留空则使用凭证 JSON 中的 proxy（若有），否则直连，登录时作为 `POST login` 的 `proxy`（留空则直连），弹窗提示文案按此区分。
  - 代理 URL 由前端拼装，表单逻辑为纯函数（`web/src/proxy.ts`：`buildProxyUrl`、`validateProxyForm`、`proxyFormChanged`、`proxyLabel`、`buildProxyPatch`），规则见 `proxy.md`「控制台」。弹窗内的请求经 `Actions.submit` 执行：失败返回错误文案（不进全局错误条），登录失效仍回到登录页。
- 账号状态徽标（`web/src/account.ts`）：「启用中」（绿色）、「禁用中」（灰色）、「冷却中」（黄色）。徽标优先级为禁用 > 冷却 > 启用。冷却中的徽标附带触发原因，如「冷却中 · 403」，网络错误显示为「冷却中 · 网络错误」，没有原因时只显示「冷却中」。
- 冷却与禁用相互独立：账号既禁用又冷却中时，徽标仍为「禁用中」，但卡片同样显示冷却原因（一行提示）与冷却截止时间，并显示「解除冷却」按钮。是否显示冷却信息与「解除冷却」按钮由 `isCooling(account, now)` 判断，即冷却截止时间晚于当前时间，与是否禁用无关。
- 冷却中（无论是否禁用）的账号卡片显示「解除冷却」按钮，调用 `PATCH /v0/management/accounts` 的 `clear_cooldown`（`ManagementClient.clearCooldown(id)`，`web/src/api.ts`），成功后提示并刷新列表。
- 版本：顶栏品牌区显示服务端版本号（`GET /v0/management/version`）；加载失败时不显示。
- 登录态：登录时用管理密钥调用 `POST /v0/management/session` 换取控制台令牌（见 `management.md`「控制台令牌」），之后所有请求只用令牌，管理密钥不保存。令牌与过期时间以 `{"token","expiresAt"}` 存于 `localStorage`（`web/src/session.ts`，键 `ghc2api.consoleSession`），跨标签页、重启浏览器后仍保持登录。
  - 打开页面时若本地令牌未过期，立即用它调用 `POST session` 续期（有效期重新计为 14 天）并替换保存的令牌；续期返回 401 则回到登录页，网络错误则继续用现有令牌。
  - 本地记录已过期、内容损坏时直接删除并显示登录页；退出登录或任一请求返回 401 时清除。
  - 读取时顺带删除旧版存于 `sessionStorage` 的明文密钥（键 `ghc2api.managementKey`）。
  - 存储不可用时静默忽略，退化为只存 React state。
- 安全：同源脚本可读取该令牌，风险已接受；控制台不得引入第三方脚本，也不应与其他应用共用同一源（协议+域名+端口）。退出登录只清除本地记录，不能吊销已泄露的令牌；需立即吊销时更换 `management_key`。
- 不包含 API Keys、设置、模型列表、刷新凭据等功能。

## 日志页

代码：`web/src/pages/LogsPage.tsx`、`web/src/components/LogGroupList.tsx`、`CodeBlock.tsx`、`web/src/useLogs.ts`；纯函数在 `web/src/logs.ts`（解析、分组、筛选）与 `web/src/highlight.ts`（JSON 高亮）。接口见 `management.md`「日志读取」。

原则：服务端只返回原始 NDJSON 文本，解析、聚合、排序、正文块合并、JSON 格式化与高亮全部在浏览器中执行。

- 状态（读取大小、已加载的日志、筛选条件，以及解析/分组/筛选的缓存结果，均用 `useMemo` 缓存）由控制台持有（`useLogs`），切换到账号页再回来不丢失。首次进入日志页时自动加载一次；之后只在点击「加载」或顶栏「↻ 刷新」时重新加载。加载失败显示错误，401 回到登录页（与其他请求一致）。
- 工具栏：读取大小（2 / 8（默认）/ 32 / 64 MiB，对应 `max_bytes`）、「加载」、筛选输入框（占位「按 request_id、路径或内容筛选」）。下方显示统计（组数、记录数、无效行数）；响应头 `X-Log-Truncated` 为 `true` 时提示「只显示最近 N MiB 的日志」。
- 解析（`parseLogText`）：按行解析，空白行跳过；不是 JSON 对象、`msg` 或 `time` 不是字符串、`time` 无法解析的行计入无效行。`request_id` 每次进程启动都从 1 重新计数，因此分组还需要区分是哪一次运行：每条日志带运行标识 `run_id`（见 `config.md`「日志」），分组键为 `run_id + request_id`（同一 `run_id` 下即使中间出现「配置加载完成」也属同一组）；旧日志没有 `run_id`，则以「配置加载完成」记录为界划分进程序号 `epoch`，分组键为 `epoch + request_id`（带与不带 `run_id` 的记录不会合并）；没有 `request_id` 的记录各自成组。
- 分组（`groupLogs`）：组按开始时间降序（最新在前），组内记录按 `(时间, 行号)` 升序。debug 级别下按 32 KiB 切块输出的上游正文块（`上游 API 请求正文块`、`上游 API 响应正文块`）按 `chunk_index` 合并为一项，同一 URL 重试时 `chunk_index` 重新从 1 开始则另起一项；块序号不是从 1 开始的连续整数（如日志被截断）时标注「不完整」。`上游 API 响应正文` 与 `上游 API 请求详情` 的正文同样以代码块显示。
- 筛选（`filterGroups`）：大小写不敏感，匹配 request_id（完全相等）、method、path、记录的 msg、正文，以及记录全部字段的 JSON 文本；输入使用延迟值，输入保持流畅。
- 组列表：默认折叠，最多先渲染 200 组，「显示更多」每次再加 200。组头显示开始时间（本地时间，含毫秒）、`#request_id`（无则「—」；有 `run_id` 时旁边以弱色小字显示其前 8 位，悬停显示完整 `run_id`）、method path、状态码徽标（2xx 绿、3xx 黄、4xx/5xx 红，无状态显示灰色「未完成」）、耗时、记录数、最高级别徽标（ERROR 红、WARN 黄、INFO 绿、DEBUG 灰）。展开后逐条显示时间、级别、msg 与摘要字段（method、url、path、status、duration_ms、account、model、error，存在时显示）；有正文时显示可折叠代码块（合并项标注「共 n 块」），每条还有「字段」折叠区显示完整 JSON。
- 代码块（`CodeBlock`）：展开时才格式化。整体是合法 JSON 则缩进 2 空格后高亮；含以 `data:` 开头的行则按 SSE 逐行处理（data 行的 JSON 重写为紧凑格式并高亮）；否则按纯文本。格式化没有大小上限；着色单独限制：格式化后的记号数超过 5 万（`MAX_HIGHLIGHT_TOKENS`）时仍显示格式化后的完整文本，但不着色并提示「内容较大，未着色」，避免生成过多节点（`formatBody(text, maxTokens)` 返回的 `highlighted` 为 `false`）。纯文本不着色。
- 限制：
  - 没有 `run_id` 的旧日志靠「配置加载完成」记录区分进程；`log_level` 为 `warn` 或 `error` 时该记录不写入，此时不同次启动的同一 `request_id` 可能被合并为一组。带 `run_id` 的日志不受影响。
  - 正文 JSON 经 `JSON.parse` 重新格式化后显示，超大整数的精度、重复键与键顺序可能与原文不同。
  - 每次重新加载日志后组列表重新挂载，展开状态不保留。
- 安全：日志含用户提示词等不可信内容，代码块的记号全部以 React 文本节点渲染（`<span className="tok-…">`），禁止使用 `dangerouslySetInnerHTML`。

## 构建

- 产物 `web/dist/index.html` 提交到 git，经 `go:embed` 嵌入程序。
- 修改 `web/` 源码后执行 `make web`（`cd web && npm ci && npm run build`，需 Node.js 与 npm）；`make build` 不依赖 Node。
- 前端测试位于 `web/src/*.test.ts`（`npm test`，即 `vitest run`）；类型检查与构建为 `npm run build`（先 `tsc --noEmit` 再 `vite build`）。
