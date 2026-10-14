package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testCredentialFile(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testEditorConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	editorDir := filepath.Join(dir, "github-copilot")
	if err := os.MkdirAll(editorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(editorDir, "apps.json"), []byte(`{"github.com":{"oauth_token":"editor-oauth-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appConfigYAML(listen, credentials, editorConfig, host, apiKey, logLevel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "listen: %s\n", strconv.Quote(listen))
	if credentials != "" {
		fmt.Fprintf(&b, "credentials: %s\n", strconv.Quote(credentials))
	}
	if editorConfig != "" {
		fmt.Fprintf(&b, "editor_config: %s\n", strconv.Quote(editorConfig))
	}
	fmt.Fprintf(&b, "host: %s\n", strconv.Quote(host))
	fmt.Fprintf(&b, "api_key: %s\n", strconv.Quote(apiKey))
	if logLevel != "" {
		fmt.Fprintf(&b, "log_level: %s\n", strconv.Quote(logLevel))
	}
	return b.String()
}

func runWithCancelledContext(args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return run(ctx, args, &bytes.Buffer{}, &bytes.Buffer{})
}

func TestRunAcceptsOnlyConfigOption(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "startup-argument-probe-token")
	t.Setenv("GHC2API_API_KEY", "")
	t.Setenv("COPILOT_HOST", "")
	configPath := writeConfigFile(t, "listen: invalid\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, args := range [][]string{
		{"serve"},
		{"login"},
		{"help"},
		{"--help"},
		{"-h"},
		{"config.yaml"},
		{"--config", configPath},
		{"--listen", "127.0.0.1:8080"},
		{"--credentials", "credentials.json"},
		{"--token-mode", "cli"},
		{"-c"},
		{"-c", configPath, "extra"},
		{"-c", configPath, "--listen", "127.0.0.1:8080"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			err := run(ctx, args, &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil {
				t.Fatalf("unexpected startup arguments accepted: %q", args)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("legacy command reached startup instead of being rejected: %q", args)
			}
		})
	}
}

func TestRunUsesDefaultConfigYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"oauth_token":"default-config-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("credentials: credentials.json\nhost: evil.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWorkingDirectory); err != nil {
			t.Errorf("恢复工作目录失败: %v", err)
		}
	})
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GHC2API_API_KEY", "")
	t.Setenv("COPILOT_HOST", "")

	err = runWithCancelledContext(nil)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("默认 config.yaml 未读取其中的非法 host: %v", err)
	}
	if strings.Contains(err.Error(), "未知子命令") || strings.Contains(strings.ToLower(err.Error()), "unknown command") {
		t.Fatalf("默认配置路径未被解析: %v", err)
	}
}

func TestConfigYAMLUsesDefaultsForOmittedSettings(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GHC2API_API_KEY", "")
	t.Setenv("COPILOT_HOST", "")
	credentials := testCredentialFile(t, `{"oauth_token":"default-values-token"}`)
	configPath := writeConfigFile(t, fmt.Sprintf("credentials: %s\n", strconv.Quote(credentials)))
	err := runWithCancelledContext([]string{"-c", configPath})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("省略可选项时默认配置无效: %v", err)
	}
}

func TestModelsCacheTTLConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: 2 * time.Hour},
		{name: "custom", value: "1h30m", want: 90 * time.Minute},
		{name: "invalid", value: "not-a-duration", wantErr: true},
		{name: "zero", value: "0s", wantErr: true},
		{name: "negative", value: "-1s", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var contents strings.Builder
			contents.WriteString("github_token: models-cache-config-token\n")
			if tc.value != "" {
				fmt.Fprintf(&contents, "models_cache_ttl: %s\n", strconv.Quote(tc.value))
			}
			configPath := writeConfigFile(t, contents.String())
			config, err := loadConfig(configPath)
			if tc.wantErr {
				if err == nil {
					t.Fatal("无效的 models_cache_ttl 未被拒绝")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() error = %v", err)
			}
			if config.ModelsCacheTTL != tc.want {
				t.Fatalf("models_cache_ttl = %s, want %s", config.ModelsCacheTTL, tc.want)
			}
		})
	}
}

func TestRunWarnsWhenConfigPermissionsAreTooBroad(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持 Unix 配置文件权限检查")
	}

	configPath := writeConfigFile(t, "github_token: permission-warning-oauth-token\nlog_level: info\n")
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("config.yaml mode = %04o, want 0644", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{"-c", configPath}, &stdout, &stderr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run with cancelled context = %v, want context.Canceled", err)
	}

	logOutput := stderr.String()
	if !strings.Contains(logOutput, "WARN") {
		t.Fatalf("startup logs have no warning for a 0644 config.yaml: %s", logOutput)
	}
	if !strings.Contains(logOutput, configPath) {
		t.Fatalf("config permission warning does not identify config.yaml: %s", logOutput)
	}
	if !strings.Contains(logOutput, "权限") &&
		!strings.Contains(strings.ToLower(logOutput), "permission") {
		t.Fatalf("startup warning does not explain that config permissions are too broad: %s", logOutput)
	}
}

func TestRunWarnsWhenCredentialPermissionsAreTooBroad(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持 Unix 凭证文件权限检查")
	}

	credentialPath := testCredentialFile(t, `{"oauth_token":"credential-permission-warning-token"}`)
	if err := os.Chmod(credentialPath, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("credentials file mode = %04o, want 0644", got)
	}

	configPath := writeConfigFile(t, appConfigYAML("127.0.0.1:0", credentialPath, "", "", "", "info"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{"-c", configPath}, &stdout, &stderr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run with cancelled context = %v, want context.Canceled", err)
	}

	for _, line := range strings.Split(stderr.String(), "\n") {
		if !strings.Contains(line, "WARN") || !strings.Contains(line, credentialPath) {
			continue
		}
		if strings.Contains(line, "权限") || strings.Contains(strings.ToLower(line), "permission") {
			return
		}
	}
	t.Fatalf("startup logs have no permission warning for the 0644 credentials file: %s", stderr.String())
}

func TestRunLoadsYAMLServiceSettings(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GHC2API_API_KEY", "")
	t.Setenv("COPILOT_HOST", "")
	t.Setenv("COPILOT_TOKEN_MODE", "cli")
	credentials := testCredentialFile(t, `{"oauth_token":"yaml-oauth-token"}`)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	const apiKey = "yaml-api-key-secret"
	configPath := writeConfigFile(t, appConfigYAML(addr, credentials, "", "acme.ghe.com", apiKey, "info"))
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr synchronizedBuffer
	done := make(chan struct{})
	runResult := make(chan error, 1)
	go func() {
		runResult <- run(ctx, []string{"-c", configPath}, &stdout, &stderr)
		close(done)
	}()
	var stopOnce sync.Once
	stopped := false
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-done:
				stopped = true
			case <-time.After(15 * time.Second):
			}
		})
	}
	waitForStop := func() bool {
		stop()
		return stopped
	}
	t.Cleanup(func() {
		if !waitForStop() {
			t.Error("服务在取消上下文后未退出")
		}
	})

	baseURL := "http://" + addr
	client := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-done:
			runErr := <-runResult
			t.Fatalf("服务未能启动: %v; stderr=%s", runErr, stderr.String())
		default:
		}
		res, requestErr := client.Get(baseURL + "/healthz")
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("配置的监听地址未启动: %v; stdout=%s stderr=%s", requestErr, stdout.String(), stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	res, err := client.Get(baseURL + "/__config_auth_probe__")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("YAML api_key 未启用鉴权: status=%d", res.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, baseURL+"/__config_auth_probe__", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("YAML api_key 未接受正确密钥: status=%d", res.StatusCode)
	}

	if !waitForStop() {
		t.Fatal("服务在取消上下文后未退出")
	}
	runErr := <-runResult
	if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, http.ErrServerClosed) {
		t.Fatalf("服务关闭错误: %v", runErr)
	}
}

func TestRunLoadsEditorConfigFromYAML(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GHC2API_API_KEY", "")
	t.Setenv("COPILOT_HOST", "")
	editorConfig := testEditorConfig(t)
	configPath := writeConfigFile(t, appConfigYAML("127.0.0.1:0", "", editorConfig, "", "", "info"))
	err := runWithCancelledContext([]string{"-c", configPath})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("editor_config 未从 YAML 读取: %v", err)
	}
}

func TestRunAcceptsSupportedLogLevels(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GHC2API_API_KEY", "")
	t.Setenv("COPILOT_HOST", "")
	credentials := testCredentialFile(t, `{"oauth_token":"log-level-token"}`)
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			configPath := writeConfigFile(t, appConfigYAML("0.0.0.0:0", credentials, "", "", "test-api-key", level))
			err := runWithCancelledContext([]string{"-c", configPath})
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("日志级别 %q 未被接受: %v", level, err)
			}
		})
	}
}

func TestRunRejectsInvalidYAMLAndSettings(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GHC2API_API_KEY", "")
	t.Setenv("COPILOT_HOST", "")
	credentials := testCredentialFile(t, `{"oauth_token":"config-validation-token"}`)
	for _, tc := range []struct {
		name   string
		config string
	}{
		{"malformed-yaml", "listen: [\n"},
		{"invalid-log-level", appConfigYAML("127.0.0.1:0", credentials, "", "", "", "trace")},
		{"invalid-host", appConfigYAML("127.0.0.1:0", credentials, "", "evil.example", "", "info")},
		{"legacy-cli-token-mode", appConfigYAML("127.0.0.1:0", credentials, "", "", "", "info") + "token_mode: cli\n"},
		{"invalid-listen-address", appConfigYAML("not-a-listener", credentials, "", "", "", "info")},
		{"non-local-listen-requires-api-key", appConfigYAML("0.0.0.0:0", credentials, "", "", "", "info")},
		{"missing-credential-source", "listen: 127.0.0.1:0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := writeConfigFile(t, tc.config)
			err := runWithCancelledContext([]string{"-c", configPath})
			if err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("无效配置未被拒绝: %v", err)
			}
			message := strings.ToLower(err.Error())
			if strings.Contains(err.Error(), "未知子命令") || strings.Contains(message, "unknown command") ||
				strings.Contains(message, "unknown flag") || strings.Contains(message, "flag provided but not defined") {
				t.Fatalf("配置文件没有被解析，错误来自参数处理: %v", err)
			}
		})
	}
}
