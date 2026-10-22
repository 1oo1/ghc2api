# 多架构镜像（linux/amd64、linux/arm64）。
# 构建阶段固定在构建机原生架构上，通过 GOOS/GOARCH 交叉编译，无需 QEMU 模拟；
# 管理控制台已内嵌于 web/dist/index.html，构建不依赖 Node。
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine AS build
ARG TARGETOS
ARG TARGETARCH
# 版本号；未指定时使用源码中的默认值。
ARG VERSION=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
    -ldflags "-s -w ${VERSION:+-X ghc2api/internal/copilot.Version=${VERSION}}" \
    -o /out/ghc2api ./cmd/ghc2api
# distroless 没有 shell，数据目录需在此创建并在最终阶段指定属主，命名卷才能被非 root 用户写入。
RUN mkdir /out/data

# distroless 自带 CA 证书（访问 GitHub 需要），默认以非 root（uid 65532）运行。
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ghc2api /usr/local/bin/ghc2api
COPY LICENSE /licenses/LICENSE
COPY LICENSES/ /licenses/
COPY --from=build --chown=65532:65532 /out/data /data
# 配置文件放在 /data/config.yaml；其中的相对路径以配置文件所在目录为基准（默认 data_path 即 /data/data）。
WORKDIR /data
VOLUME /data
EXPOSE 8080
# distroless 没有 shell 与 curl，由程序自带的 healthcheck 子命令请求配置中监听地址的 /healthz。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/ghc2api", "healthcheck", "-c", "/data/config.yaml"]
ENTRYPOINT ["/usr/local/bin/ghc2api", "-c", "/data/config.yaml"]
