# ghc2api

从 magpie 的 GitHub Copilot provider 提取的独立 Go HTTP 服务：把一个或多个 GitHub Copilot 账号暴露为
OpenAI Chat Completions / Responses 与 Anthropic Messages 兼容的接口，支持多账号选择、会话亲和、额度统计和 Web 控制台。
当前实现以 magpie 上游提交 `a1f7b8c1` 为对照基线，同步流程见 [`docs/magpie-copilot-sync.md`](docs/magpie-copilot-sync.md)。

## 快速开始

需要 Go 1.23+ 与有效的 GitHub Copilot OAuth 订阅。

```sh
cp config.example.yaml config.yaml && chmod 600 config.yaml
# 编辑 config.yaml：api_key 必填，去首尾空白后至少 32 字节，例如 openssl rand -hex 32 的输出
mkdir -p data/credentials   # 凭证 JSON 放在 data/credentials/<GitHub 用户名>.json，chmod 600
make build
./bin/ghc2api               # 默认读取当前目录 config.yaml；-c 指定其他路径
```

默认监听 `127.0.0.1:8080`；设置 `management_key`（同样至少 32 字节）后可访问 `http://127.0.0.1:8080/management`。
`api_key` 无论监听地址都必须配置，不存在匿名模式。
真实凭证不要提交到版本库。

## 文档

| 文档 | 内容 |
| --- | --- |
| [`docs/config.md`](docs/config.md) | 配置、启动、监听与日志 |
| [`docs/credentials.md`](docs/credentials.md) | 凭证文件格式、目录扫描、用户名规则 |
| [`docs/http-api.md`](docs/http-api.md) | 对外 HTTP 接口、鉴权、协议转发 |
| [`docs/pool.md`](docs/pool.md) | 多账号选择、故障切换、限流冷却 |
| [`docs/session-affinity.md`](docs/session-affinity.md) | 会话亲和 |
| [`docs/quota-stats.md`](docs/quota-stats.md) | 额度解析与刷新、请求统计 |
| [`docs/management.md`](docs/management.md) | 管理接口、导入与设备登录 |
| [`docs/web-console.md`](docs/web-console.md) | Web 控制台与前端构建 |
| [`docs/upstream.md`](docs/upstream.md) | 上游 session、模型、Auto 行为 |
| [`docs/development.md`](docs/development.md) | Makefile、测试、来源与服务条款 |
| [`docs/magpie-copilot-sync.md`](docs/magpie-copilot-sync.md) | 与 magpie 上游同步 |
