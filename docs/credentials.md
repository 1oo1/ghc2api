# 账号与凭证文件

代码：`cmd/ghc2api/credentials.go`（启动扫描）、`internal/copilot/credentials.go`（读写校验）。

## 凭证 JSON

只允许以下字段，每个文件应 `chmod 600`（权限过宽启动时逐个告警）：

| 字段 | 说明 |
| --- | --- |
| `oauth_token` | GitHub OAuth token |
| `host` | 可选，GHE 主机；顶层 `host` 设置后覆盖所有账号 |
| `user` | 可省略；非空时须与文件名用户名一致（忽略大小写） |
| `disabled` | `true` 表示启动后不参与选择，仍在管理接口中可见 |
| `priority` | -1 到 100 的整数，省略为 0，越大越优先；超范围报 `priority 必须在 -1 到 100 之间`，非整数解码时即拒绝 |

`disabled` 与 `priority` 通常由管理接口写入，`omitempty`（false/0 时省略）。`MinPriority`/`MaxPriority` 定义于 `credentials.go`；`LoadCredential`、`SaveCredential` 都校验范围。

## 目录扫描规则

启动时扫描 `<data_path>/credentials/`，每个文件对应一个账号：

- 只扫一层，不递归；按文件名字节序加载。
- 文件名 `<GitHub 用户名>.json`，须通过 `ValidGitHubUserName`（见下）；不合法则启动失败，错误含完整文件名。
- 用户名不区分大小写：`Alice.json` 与 `alice.json` 视为重复，启动失败。
- 忽略：`.` 开头的隐藏/临时文件、非 `.json` 文件、子目录（含其中的 json）、指向目录的符号链接。指向普通文件的符号链接会被读取；悬空链接导致启动失败。
- 同一账号（`host` 与 `oauth_token` 都相同）不能出现在两个文件中。
- 目录不存在或无凭证文件时启动失败；设置 `management_key` 时例外（自动创建目录，允许 0 账号）。
- 直接修改目录中的文件后需重启；通过管理接口增删改无需重启（见 `management.md`）。

## 用户名与账号 ID

- 账号 ID = 用户名小写形式，池内唯一。
- `ValidGitHubUserName(name) bool`（导出，cmd 与 internal 共用）：长度 1–100；由字母、数字片段以**单个** `-` 或 `_` 分隔，不以分隔符开头/结尾，无连续分隔符。`_` 用于 GHE.com 企业托管用户（如 `mona-cat_octo`）。
- `Config.User` 为空时（旧测试用 `NewPool` 建池），ID 退化为账号在初始列表中的下标字符串。

## 写入与删除（管理接口使用）

- 保存路径 `<CredentialsDir>/<GitHub 登录名>.json`（保留 GitHub 返回的大小写，`0600`，复用 `SaveCredential` 原子写入）。
- 写入前扫描目录，忽略大小写确认无同名文件（含运行期间放入、尚未加载的）；有则 409，原文件不变。
- PATCH：一次重写文件的 `disabled` 与 `priority`（未提供的取当前值），再更新内存；写入失败则内存不变、返回 500。
- 删除：先删凭证文件，再从池中移除（文件不存在也视为成功），并清理该账号缓存目录。进行中的请求不得重建已删除账号的缓存目录（`writeCache` 的已删除标记保证）。
