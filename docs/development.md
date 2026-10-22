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

## 版本与发布

- 版本号：`internal/copilot/version.go` 的 `Version`（当前 `v0.0.1`），启动日志与 `GET /v0/management/version` 输出它。发布新版本时先更新该默认值，再打同名 git 标签（测试只校验格式 `vX.Y.Z`）。
- 发布：推送 `v<数字>...` 标签或在 GitHub Actions 手动运行 `docker` 工作流（`.github/workflows/docker.yml`），会先运行 `go vet` 与 `go test`，再构建 `linux/amd64`、`linux/arm64` 镜像并推送到 `ghcr.io/<owner>/<repo>`（全小写）。
  - 标签构建：`Version` 由 `-ldflags -X` 覆盖为标签名；镜像标签为 `1.2.3`、`1.2`、`1` 与 `latest`；`0.x` 版本不打主版本标签 `0`；预发布标签（如 `v1.2.3-rc.1`）只打完整版本号，不打 `latest`。另有 `sha-<短 commit>`。
  - 手动在分支上运行：`Version` 为 `dev-<短 commit>`，镜像标签为分支名与 `sha-<短 commit>`；在标签上运行同标签构建。
  - 首次推送后，到仓库的 Packages 设置中将镜像设为 public（或授权拉取者）。
- 镜像（`Dockerfile`）：多阶段构建，运行于 distroless（非 root，uid 65532），并带有 `/licenses`；启动命令 `ghc2api -c /data/config.yaml`，`/data` 为数据卷。
  - 在 `/data` 放置 `config.yaml`（参考 `config.example.yaml`）：`listen` 写 `0.0.0.0:8080`（默认 `127.0.0.1` 在容器外不可达），`api_key` 必填，需要控制台时设置 `management_key`。
  - 默认 `data_path: ./data` 以配置文件目录为基准，即容器内 `/data/data`（宿主机上为 `<挂载目录>/data/credentials`）；也可设为 `.`。
  - bind mount 的宿主机目录需对 uid 65532 可读写（`chown -R 65532 <目录>`），配置与凭证文件权限建议 0600；命名卷无需处理。
  - 运行：`docker run -d -p 8080:8080 -v $PWD/ghc2api:/data ghcr.io/<owner>/<repo>:latest`
  - 健康检查：镜像内置 `HEALTHCHECK`，调用 `ghc2api healthcheck -c /data/config.yaml`（语义见 `config.md`；每 30 秒一次，连续 3 次失败标记为 unhealthy）。用 `docker ps` 或 `docker inspect --format '{{.State.Health.Status}}' <容器>` 查看，compose 自动继承。每次探测会在服务日志里留下两条 `/healthz` 请求日志。
  - Compose：复制 `docker-compose.example.yml` 为 `docker-compose.yml`，步骤见文件内注释；配置文件只读挂载，数据放在命名卷。
  - 本地验证构建：`docker buildx build --platform linux/arm64 --build-arg VERSION=v0.0.1 --load -t ghc2api:dev .`（多平台构建需要 `docker buildx create --use` 的 docker-container driver，且结果需 `--push` 推送）。

## 来源与服务条款

- 原项目 magpie 使用 MIT 许可，版权及许可文本保留在 `LICENSE`。
- `web/` 改编自 cpa-github-copilot 的 Web 界面，额度相关代码改编自 CPA Management Center（MIT，Router-For.ME 版权，见 `LICENSES/Management-Center.txt`）。
- 会话亲和键的会话 ID 提取优先级参照 CLIProxyAPI（MIT，Luis Pater 与 Router-For.ME 版权，见 `LICENSES/CLIProxyAPI.txt`），同步流程见 `magpie-copilot-sync.md`。
- 本服务使用原 provider 的客户端身份头，不是 GitHub 官方服务或受支持的公共 API。使用前请确认 GitHub Copilot 的服务条款、账号权限和额度政策；上游接口或写死的客户端版本变化时需调整实现。
