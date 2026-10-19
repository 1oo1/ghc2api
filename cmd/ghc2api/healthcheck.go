package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// healthcheckTimeout 是一次健康检查请求的超时。
const healthcheckTimeout = 3 * time.Second

// healthcheckClient 不使用代理环境变量，也不跟随重定向：只接受本机服务直接返回的 200。
var healthcheckClient = &http.Client{
	Transport:     &http.Transport{},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// isHealthcheck 报告启动参数是否为 healthcheck 子命令。
func isHealthcheck(args []string) bool {
	return len(args) > 0 && args[0] == "healthcheck"
}

// runHealthcheck 读取配置中的监听地址，请求本机 /healthz；返回 nil 表示服务健康。
// 用于容器的 HEALTHCHECK（distroless 镜像没有 shell 与 curl）。它不初始化日志文件，也不输出内容，
// 只通过退出码与 main 输出的错误说明结果。参数形式：healthcheck [-c <配置文件>]，与启动服务时一致。
// 因为复用 loadConfig，判定的是“磁盘上的配置与凭证可被启动流程加载，且 listen 上的 /healthz 返回 200”。
func runHealthcheck(ctx context.Context, args []string) error {
	path, err := configPath(args[1:])
	if err != nil {
		return err
	}
	config, err := loadConfig(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthcheckURL(config.Listen), nil)
	if err != nil {
		return err
	}
	response, err := healthcheckClient.Do(request)
	if err != nil {
		return fmt.Errorf("健康检查失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("健康检查失败: HTTP %d", response.StatusCode)
	}
	return nil
}

// healthcheckURL 由监听地址得到本机 /healthz 的地址：未指定的主机（空、0.0.0.0、::）换成回环地址。
func healthcheckURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen + "/healthz"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}
