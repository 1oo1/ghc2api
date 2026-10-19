package main

// 本文件覆盖 healthcheck 子命令：读取配置中的 listen 地址，请求 GET /healthz，
// 仅当返回 200 时成功；不初始化日志文件，也不向 stdout 输出内容。

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// startHealthzServer 在 127.0.0.1 随机端口启动模拟 /healthz 的服务，所有请求都返回 status。
// 收到的请求以 "方法 路径" 形式写入返回的通道；写入为非阻塞，通道满时丢弃，避免阻塞服务处理。
func startHealthzServer(t *testing.T, status int) (string, <-chan string) {
	t.Helper()
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.Method + " " + r.URL.Path:
		default:
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server.Listener.Addr().String(), requests
}

// writeHealthcheckConfig 在新的临时目录中写入配置文件并返回其路径：listen 为 listen，
// data_path 指向该目录下的 data（其中放入一个合法凭证），api_key 合法；extra 追加在末尾，例如 logs_path。
func writeHealthcheckConfig(t *testing.T, listen, extra string) string {
	t.Helper()
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "data")
	writeCredentialFile(t, filepath.Join(dataPath, "credentials", "healthcheck.json"), accountJSON("healthcheck-token"))
	contents := "listen: " + strconv.Quote(listen) + "\n" +
		"data_path: " + strconv.Quote(dataPath) + "\n" +
		apiKeySetting + extra
	return writeConfigInDir(t, dir, contents)
}

// runHealthcheckForTest 以 healthcheck 子命令加上 args 调用 run，返回标准输出内容与错误；标准错误被丢弃。
func runHealthcheckForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), append([]string{"healthcheck"}, args...), &stdout, &stderr)
	return stdout.String(), err
}

// TestHealthcheckURLUsesLoopbackForUnspecifiedHost 未指定的监听主机（0.0.0.0、空主机、::）换成回环地址；
// 已指定的主机（包括 IPv6 回环与 localhost）原样保留。
func TestHealthcheckURLUsesLoopbackForUnspecifiedHost(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   string
	}{
		{listen: "0.0.0.0:8080", want: "http://127.0.0.1:8080/healthz"},
		{listen: ":8080", want: "http://127.0.0.1:8080/healthz"},
		{listen: "[::]:8080", want: "http://127.0.0.1:8080/healthz"},
		{listen: "127.0.0.1:9", want: "http://127.0.0.1:9/healthz"},
		{listen: "[::1]:9", want: "http://[::1]:9/healthz"},
		{listen: "localhost:9", want: "http://localhost:9/healthz"},
	} {
		t.Run(tc.listen, func(t *testing.T) {
			if got := healthcheckURL(tc.listen); got != tc.want {
				t.Fatalf("healthcheckURL(%q) = %q, want %q", tc.listen, got, tc.want)
			}
		})
	}
}

// TestHealthcheckSucceedsWhenHealthzReturns200 /healthz 返回 200 时 run 返回 nil，
// 请求必须是 GET /healthz，且 healthcheck 不向 stdout 输出内容。
func TestHealthcheckSucceedsWhenHealthzReturns200(t *testing.T) {
	addr, requests := startHealthzServer(t, http.StatusOK)
	configPath := writeHealthcheckConfig(t, addr, "")

	stdout, err := runHealthcheckForTest(t, "-c", configPath)
	if err != nil {
		t.Fatalf("/healthz 返回 200 时 healthcheck 失败: %v", err)
	}
	if stdout != "" {
		t.Fatalf("healthcheck 不应向 stdout 输出内容，实际为 %q", stdout)
	}
	select {
	case got := <-requests:
		if got != "GET /healthz" {
			t.Fatalf("服务收到的请求 = %q, want %q", got, "GET /healthz")
		}
	default:
		t.Fatal("服务未收到 healthcheck 请求")
	}
}

// TestHealthcheckFailsWhenHealthzReturns503 /healthz 返回 503（未就绪）时 healthcheck 必须返回错误。
func TestHealthcheckFailsWhenHealthzReturns503(t *testing.T) {
	addr, _ := startHealthzServer(t, http.StatusServiceUnavailable)
	configPath := writeHealthcheckConfig(t, addr, "")

	stdout, err := runHealthcheckForTest(t, "-c", configPath)
	if err == nil {
		t.Fatal("/healthz 返回 503 时 healthcheck 未报错")
	}
	if stdout != "" {
		t.Fatalf("healthcheck 不应向 stdout 输出内容，实际为 %q", stdout)
	}
}

// TestHealthcheckFailsWhenServiceIsNotListening 目标端口没有服务（服务已关闭或从未监听）时，连接失败必须返回错误。
func TestHealthcheckFailsWhenServiceIsNotListening(t *testing.T) {
	configPath := writeHealthcheckConfig(t, freeLoopbackAddressForTest(t), "")

	stdout, err := runHealthcheckForTest(t, "-c", configPath)
	if err == nil {
		t.Fatal("无服务监听的端口上 healthcheck 未报错")
	}
	if stdout != "" {
		t.Fatalf("healthcheck 不应向 stdout 输出内容，实际为 %q", stdout)
	}
}

// TestHealthcheckFailsOnInvalidConfig 配置无效（缺少 api_key）时 healthcheck 必须失败，错误信息指出 api_key。
// 地址上有健康的服务，因此失败只能来自配置校验。
func TestHealthcheckFailsOnInvalidConfig(t *testing.T) {
	addr, _ := startHealthzServer(t, http.StatusOK)
	dir := t.TempDir()
	configPath := writeConfigInDir(t, dir, "listen: "+strconv.Quote(addr)+"\n")

	_, err := runHealthcheckForTest(t, "-c", configPath)
	if err == nil {
		t.Fatal("缺少 api_key 的配置未被 healthcheck 拒绝")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("错误信息应指出 api_key: %v", err)
	}
}

// TestHealthcheckRejectsExtraArguments healthcheck 之后只允许可选的 -c <path>，其他参数（包括残缺的 -c）必须返回错误。
// 地址上有健康的服务且配置有效，因此错误只能来自参数校验。
func TestHealthcheckRejectsExtraArguments(t *testing.T) {
	addr, _ := startHealthzServer(t, http.StatusOK)
	configPath := writeHealthcheckConfig(t, addr, "")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "extra-after-config", args: []string{"-c", configPath, "extra"}},
		{name: "extra-before-config", args: []string{"extra", "-c", configPath}},
		{name: "config-without-path", args: []string{"-c"}},
		{name: "blank-config-path", args: []string{"-c", ""}},
		{name: "listen-flag", args: []string{"--listen", addr}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := runHealthcheckForTest(t, tc.args...); err == nil {
				t.Fatalf("healthcheck 接受了多余参数 %q", tc.args)
			}
		})
	}
}

// TestHealthcheckDoesNotInitializeLogFile healthcheck 不初始化日志：logs_path 指向的尚不存在的目录，
// 运行后仍然不存在，且不向 stdout 输出内容。服务健康，因此 run 必须成功。
func TestHealthcheckDoesNotInitializeLogFile(t *testing.T) {
	addr, _ := startHealthzServer(t, http.StatusOK)
	logsDir := filepath.Join(t.TempDir(), "not-yet-created", "logs")
	configPath := writeHealthcheckConfig(t, addr, "logs_path: "+strconv.Quote(logsDir)+"\n")

	stdout, err := runHealthcheckForTest(t, "-c", configPath)
	if err != nil {
		t.Fatalf("healthcheck 失败: %v", err)
	}
	if stdout != "" {
		t.Fatalf("healthcheck 不应向 stdout 输出内容，实际为 %q", stdout)
	}
	if _, err := os.Stat(logsDir); !os.IsNotExist(err) {
		t.Fatalf("healthcheck 不应创建日志目录 %q，stat 结果 err = %v", logsDir, err)
	}
}

// TestHealthcheckUsesConfigYAMLInWorkingDirectory 不带 -c 时读取当前目录下的 config.yaml。
func TestHealthcheckUsesConfigYAMLInWorkingDirectory(t *testing.T) {
	addr, _ := startHealthzServer(t, http.StatusOK)
	configPath := writeHealthcheckConfig(t, addr, "")
	oldWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Dir(configPath)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWorkingDirectory); err != nil {
			t.Errorf("恢复工作目录失败: %v", err)
		}
	})

	if _, err := runHealthcheckForTest(t); err != nil {
		t.Fatalf("不带 -c 时未读取当前目录的 config.yaml: %v", err)
	}
}
