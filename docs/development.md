# 开发与验证

```sh
make build        # 构建 bin/ghc2api，不依赖 Node
make test
make test-race
make vet
make web          # 仅修改 web/ 后需要，需 Node.js 与 npm
```

- 自动化测试使用本地模拟 GitHub/Copilot，无需真实凭证；真实登录、订阅和模型可用性需用自己的账号验证。
- 测试辅助：`testClient`、`testJSON`、`testService`、`testDevice`（`login_test.go`）、`poolUpstream`（`pool_test.go`）；涉及缓存的测试必须把 `CacheDir` 指向 `t.TempDir()`。
- 代码布局：`cmd/ghc2api/`（配置、凭证扫描、日志、入口）、`internal/copilot/`（账号池、上游服务、管理接口）、`web/`（控制台）。各模块文档见 `AGENTS.md` 索引。

## 来源与服务条款

- 原项目 magpie 使用 MIT 许可，版权及许可文本保留在 `LICENSE`。
- `web/` 改编自 cpa-github-copilot 的 Web 界面，额度相关代码改编自 CPA Management Center（MIT，Router-For.ME 版权，见 `LICENSES/Management-Center.txt`）。
- 本服务使用原 provider 的客户端身份头，不是 GitHub 官方服务或受支持的公共 API。使用前请确认 GitHub Copilot 的服务条款、账号权限和额度政策；上游接口或写死的客户端版本变化时需调整实现。
