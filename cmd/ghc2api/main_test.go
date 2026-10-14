package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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
	writeCredentialFile(t, path, raw)
	return path
}

func writeCredentialFile(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	return writeConfigInDir(t, t.TempDir(), contents)
}

func writeConfigInDir(t *testing.T, dir, contents string) string {
	t.Helper()
	return writeConfigAt(t, dir, "config.yaml", contents)
}

func writeConfigAt(t *testing.T, dir, name, contents string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appConfigYAML(listen, credentials, host, apiKey, logLevel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "listen: %s\n", strconv.Quote(listen))
	if credentials != "" {
		fmt.Fprintf(&b, "credentials: %s\n", strconv.Quote(credentials))
	}
	fmt.Fprintf(&b, "host: %s\n", strconv.Quote(host))
	fmt.Fprintf(&b, "api_key: %s\n", strconv.Quote(apiKey))
	if logLevel != "" {
		fmt.Fprintf(&b, "log_level: %s\n", strconv.Quote(logLevel))
	}
	return b.String()
}

func configStringField(t *testing.T, config appConfig, names ...string) string {
	t.Helper()
	value := reflect.ValueOf(config)
	for _, name := range names {
		field := value.FieldByName(name)
		if field.IsValid() && field.Kind() == reflect.String {
			return field.String()
		}
	}
	t.Fatalf("appConfig 缺少字符串字段之一: %s", strings.Join(names, ", "))
	return ""
}

func configIntField(t *testing.T, config appConfig, names ...string) int64 {
	t.Helper()
	value := reflect.ValueOf(config)
	for _, name := range names {
		field := value.FieldByName(name)
		if !field.IsValid() {
			continue
		}
		switch field.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return field.Int()
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return int64(field.Uint())
		}
	}
	t.Fatalf("appConfig 缺少整数值字段之一: %s", strings.Join(names, ", "))
	return 0
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
	writeCredentialFile(t, filepath.Join(dir, "data", "credentials", "credentials.json"),
		`{"oauth_token":"default-config-token"}`)
	configPath := writeConfigInDir(t, dir, "host: evil.example\n")
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
	if !strings.Contains(err.Error(), "host") && !strings.Contains(err.Error(), "域名") {
		t.Fatalf("默认凭证路径未生效，错误不是非法 host: %v (config=%s)", err, configPath)
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

func TestLoadConfigUsesDefaultDataAndCredentialsPaths(t *testing.T) {
	configDir := t.TempDir()
	configPath := writeConfigInDir(t, configDir, "listen: 127.0.0.1:0\n")
	dataPath := filepath.Join(configDir, "data")
	writeCredentialFile(t, filepath.Join(dataPath, "credentials", "credentials.json"),
		`{"oauth_token":"default-data-path-token"}`)

	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig() with default data_path credentials: %v", err)
	}
	if config.Token != "default-data-path-token" {
		t.Fatalf("token loaded from default credentials = %q", config.Token)
	}
	assertResolvedDataAndLogPaths(t, config, dataPath, filepath.Join(configDir, "logs"))
	if got := configIntField(t, config, "LogMaxSizeMB", "logMaxSizeMB"); got <= 0 {
		t.Fatalf("default log_max_size_mb = %d, want a positive value", got)
	}
	if got := configIntField(t, config, "LogMaxBackups", "logMaxBackups"); got <= 0 {
		t.Fatalf("default log_max_backups = %d, want a positive value", got)
	}
}

func TestLoadConfigResolvesRelativeDataAndCredentialsPaths(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataPath := filepath.Join(root, "state")
	configPath := writeConfigInDir(t, configDir, "data_path: ../state\n")
	writeCredentialFile(t, filepath.Join(dataPath, "credentials", "credentials.json"),
		`{"oauth_token":"relative-data-path-token"}`)

	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig() with relative data_path: %v", err)
	}
	if config.Token != "relative-data-path-token" {
		t.Fatalf("token loaded from relative data_path = %q", config.Token)
	}
	assertResolvedDataAndLogPaths(t, config, dataPath, filepath.Join(configDir, "logs"))
}

func TestLoadConfigResolvesRelativeCredentialsUnderDataPath(t *testing.T) {
	configDir := t.TempDir()
	configPath := writeConfigInDir(t, configDir, "data_path: state\ncredentials: work.json\n")
	writeCredentialFile(t, filepath.Join(configDir, "state", "credentials", "work.json"),
		`{"oauth_token":"relative-credentials-token"}`)

	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig() with relative credentials path: %v", err)
	}
	if config.Token != "relative-credentials-token" {
		t.Fatalf("token loaded from relative credentials path = %q", config.Token)
	}
	assertResolvedDataAndLogPaths(t, config, filepath.Join(configDir, "state"), filepath.Join(configDir, "logs"))
}

func TestLoadConfigKeepsAbsoluteCredentialsOutsideDataPath(t *testing.T) {
	credentialPath := testCredentialFile(t, `{"oauth_token":"absolute-credentials-token"}`)
	configPath := writeConfigFile(t, fmt.Sprintf("data_path: local-state\ncredentials: %s\n",
		strconv.Quote(credentialPath)))

	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig() with absolute credentials path: %v", err)
	}
	if config.Token != "absolute-credentials-token" {
		t.Fatalf("token loaded from absolute credentials path = %q", config.Token)
	}
}

func TestLogRotationConfigValuesAndValidation(t *testing.T) {
	credentials := testCredentialFile(t, `{"oauth_token":"log-rotation-config-token"}`)
	configPath := writeConfigFile(t, fmt.Sprintf(
		"credentials: %s\nlog_max_size_mb: 1\nlog_max_backups: 2\n",
		strconv.Quote(credentials)))
	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig() with log rotation settings: %v", err)
	}
	if got := configIntField(t, config, "LogMaxSizeMB", "logMaxSizeMB"); got != 1 {
		t.Fatalf("log_max_size_mb = %d, want 1", got)
	}
	if got := configIntField(t, config, "LogMaxBackups", "logMaxBackups"); got != 2 {
		t.Fatalf("log_max_backups = %d, want 2", got)
	}

	for _, tc := range []struct {
		name   string
		fields string
		field  string
	}{
		{name: "zero-size", fields: "log_max_size_mb: 0\n", field: "log_max_size_mb"},
		{name: "negative-size", fields: "log_max_size_mb: -1\n", field: "log_max_size_mb"},
		{name: "non-integer-size", fields: "log_max_size_mb: invalid\n", field: "log_max_size_mb"},
		{name: "negative-backups", fields: "log_max_backups: -1\n", field: "log_max_backups"},
		{name: "non-integer-backups", fields: "log_max_backups: invalid\n", field: "log_max_backups"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalidConfigPath := writeConfigFile(t, fmt.Sprintf("credentials: %s\n%s",
				strconv.Quote(credentials), tc.fields))
			_, err := loadConfig(invalidConfigPath)
			if err == nil {
				t.Fatalf("无效的 %s 未被拒绝", tc.field)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.field)) {
				t.Fatalf("错误未指出无效字段 %s: %v", tc.field, err)
			}
		})
	}
}

func assertResolvedDataAndLogPaths(t *testing.T, config appConfig, dataPath, logsPath string) {
	t.Helper()
	if got := configStringField(t, config, "DataPath", "dataPath"); got != dataPath {
		t.Errorf("data_path = %q, want %q", got, dataPath)
	}
	if got := configStringField(t, config, "CacheDir", "CachePath", "cacheDir", "cachePath"); got != filepath.Join(dataPath, "cache") {
		t.Errorf("cache root = %q, want %q", got, filepath.Join(dataPath, "cache"))
	}
	if got := configStringField(t, config, "LogsPath", "logsPath"); got != logsPath {
		t.Errorf("logs_path = %q, want %q", got, logsPath)
	}
}

func TestModelsCacheTTLConfig(t *testing.T) {
	credentials := testCredentialFile(t, `{"oauth_token":"models-cache-config-token"}`)
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
			fmt.Fprintf(&contents, "credentials: %s\n", strconv.Quote(credentials))
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

	credentials := testCredentialFile(t, `{"oauth_token":"permission-warning-oauth-token"}`)
	configPath := writeConfigFile(t, fmt.Sprintf("credentials: %s\nlog_level: info\n",
		strconv.Quote(credentials)))
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

	configPath := writeConfigFile(t, appConfigYAML("127.0.0.1:0", credentialPath, "", "", "info"))
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
	configPath := writeConfigFile(t, appConfigYAML(addr, credentials, "acme.ghe.com", apiKey, "info"))
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

func TestLoadConfigRejectsLegacyCredentialFields(t *testing.T) {
	credentials := testCredentialFile(t, `{"oauth_token":"legacy-field-probe-token"}`)
	for _, tc := range []struct {
		field string
		value string
	}{
		{field: "github_token", value: "legacy-inline-token"},
		{field: "editor_config", value: "legacy-editor-path"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			contents := fmt.Sprintf("credentials: %s\n%s: %s\n",
				strconv.Quote(credentials), tc.field, strconv.Quote(tc.value))
			configPath := writeConfigFile(t, contents)
			_, err := loadConfig(configPath)
			if err == nil {
				t.Fatalf("旧凭证字段 %q 未被 KnownFields 拒绝", tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("错误未指出不支持的字段 %q: %v", tc.field, err)
			}
		})
	}
}

func TestRunWritesLogToDefaultAndRelativeLogsPaths(t *testing.T) {
	for _, tc := range []struct {
		name     string
		config   string
		logsPath string
	}{
		{name: "default", logsPath: "logs"},
		{name: "relative", config: "logs_path: runtime-logs\n", logsPath: "runtime-logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "credentials.json"),
				`{"oauth_token":"log-path-token"}`)
			configPath := writeConfigInDir(t, configDir, tc.config+"listen: 127.0.0.1:0\n")

			err := runWithCancelledContext([]string{"-c", configPath})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("run with cancelled context = %v, want context.Canceled", err)
			}

			logsDir := filepath.Join(configDir, tc.logsPath)
			logPath := filepath.Join(logsDir, "ghc2api.log")
			dirInfo, err := os.Stat(logsDir)
			if err != nil {
				t.Fatalf("stat logs directory %q: %v", logsDir, err)
			}
			fileInfo, err := os.Stat(logPath)
			if err != nil {
				t.Fatalf("stat log file %q: %v", logPath, err)
			}
			if fileInfo.Size() == 0 {
				t.Fatalf("log file %q is empty", logPath)
			}
			if runtime.GOOS != "windows" {
				if got := dirInfo.Mode().Perm(); got != 0o700 {
					t.Errorf("logs directory permissions = %04o, want 0700", got)
				}
				if got := fileInfo.Mode().Perm(); got != 0o600 {
					t.Errorf("log file permissions = %04o, want 0600", got)
				}
			}
		})
	}
}

func TestRunRotatesLogsAndRetainsConfiguredBackups(t *testing.T) {
	configPath, configDir := writeLongConfigFile(t, "log_max_size_mb: 1\nlog_max_backups: 2\n")
	writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "credentials.json"),
		`{"oauth_token":"log-rotation-token"}`)

	args := []string{"-c", configPath}
	for i := 0; i < 4000; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := run(ctx, args, io.Discard, io.Discard)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run %d with cancelled context = %v, want context.Canceled", i, err)
		}
	}

	logsDir := filepath.Join(configDir, "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatalf("read logs directory %q: %v", logsDir, err)
	}
	var backups []os.DirEntry
	for _, entry := range entries {
		if entry.Name() != "ghc2api.log" && strings.HasPrefix(entry.Name(), "ghc2api.log") {
			backups = append(backups, entry)
		}
	}
	if len(backups) != 2 {
		t.Fatalf("retained log backups = %d, want 2; files=%v", len(backups), entries)
	}
	for _, backup := range backups {
		info, err := backup.Info()
		if err != nil {
			t.Fatalf("stat log backup %q: %v", backup.Name(), err)
		}
		if info.Size() == 0 {
			t.Errorf("log backup %q is empty", backup.Name())
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("log backup %q permissions = %04o, want 0600", backup.Name(), info.Mode().Perm())
		}
	}
	currentInfo, err := os.Stat(filepath.Join(logsDir, "ghc2api.log"))
	if err != nil {
		t.Fatalf("stat current log file: %v", err)
	}
	if currentInfo.Size() > 1<<20 {
		t.Errorf("current log file size = %d, want at most 1 MiB", currentInfo.Size())
	}
}

func TestNewRotatingLogWriterCleansBackupsExceedingRetention(t *testing.T) {
	for _, tc := range []struct {
		name        string
		maxBackups  int
		wantBackups int
	}{
		{name: "retain-one", maxBackups: 1, wantBackups: 1},
		{name: "retain-none", maxBackups: 0, wantBackups: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "ghc2api.log")
			for index := 1; index <= 3; index++ {
				backupPath := fmt.Sprintf("%s.%d", logPath, index)
				if err := os.WriteFile(backupPath, []byte(fmt.Sprintf("backup-%d", index)), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			writer, err := newRotatingLogWriter(logPath, 64, tc.maxBackups)
			if err != nil {
				t.Fatalf("newRotatingLogWriter() error = %v", err)
			}
			t.Cleanup(func() { _ = writer.Close() })

			for index := 1; index <= 3; index++ {
				backupPath := fmt.Sprintf("%s.%d", logPath, index)
				_, err := os.Stat(backupPath)
				if index <= tc.wantBackups {
					if err != nil {
						t.Errorf("retained backup %q is missing: %v", backupPath, err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("backup %q should be removed at initialization; stat error = %v", backupPath, err)
				}
			}
		})
	}
}

func TestNewRotatingLogWriterTruncatesIncompleteTrailingJSONL(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ghc2api.log")
	completeRecord := []byte("{\"level\":\"INFO\",\"msg\":\"complete\"}\n")
	truncatedRecord := []byte("{\"level\":\"INFO\",\"msg\":\"interrupted")
	initialContents := append(append([]byte(nil), completeRecord...), truncatedRecord...)
	if err := os.WriteFile(logPath, initialContents, 0o600); err != nil {
		t.Fatal(err)
	}

	writer, err := newRotatingLogWriter(logPath, 1<<20, 1)
	if err != nil {
		t.Fatalf("newRotatingLogWriter() error = %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read recovered log file: %v", err)
	}
	if !bytes.Equal(got, completeRecord) {
		t.Fatalf("log after recovery = %q, want complete prefix %q", got, completeRecord)
	}

	nextRecord := []byte("{\"level\":\"INFO\",\"msg\":\"after-restart\"}\n")
	if n, err := writer.Write(nextRecord); err != nil || n != len(nextRecord) {
		t.Fatalf("Write() = %d, %v; want %d bytes and no error", n, err, len(nextRecord))
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close rotating log writer: %v", err)
	}

	got, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file after append: %v", err)
	}
	want := append(append([]byte(nil), completeRecord...), nextRecord...)
	if !bytes.Equal(got, want) {
		t.Fatalf("log after append = %q, want %q", got, want)
	}
	for index, line := range bytes.Split(bytes.TrimSuffix(got, []byte{'\n'}), []byte{'\n'}) {
		if !json.Valid(line) {
			t.Errorf("JSONL record %d is invalid after recovery and append: %q", index, line)
		}
	}
}

func TestRotatingLogWriterKeepsOversizedSingleWriteWithinLimit(t *testing.T) {
	const maxSize = 8
	logPath := filepath.Join(t.TempDir(), "ghc2api.log")
	writer, err := newRotatingLogWriter(logPath, maxSize, 1)
	if err != nil {
		t.Fatalf("newRotatingLogWriter() error = %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	record := []byte("123456789")
	n, writeErr := writer.Write(record)
	if n < len(record) && writeErr == nil {
		t.Fatalf("Write() = %d, nil; partial writes must return an error", n)
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat active log file: %v", err)
	}
	if info.Size() > maxSize {
		t.Fatalf("active log size = %d, want at most %d", info.Size(), maxSize)
	}
}

func TestConfiguredLoggerWritesStderrWhenFileWriteFails(t *testing.T) {
	stderr := &bytes.Buffer{}
	logger, file, err := newConfiguredLogger(appConfig{
		LogsPath: t.TempDir(), LogLevel: "info", LogMaxSizeMB: 1, LogMaxBackups: 1,
	}, stderr)
	if err != nil {
		t.Fatalf("newConfiguredLogger() error = %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })

	// 关闭底层文件但保留 writer 状态，模拟日志文件在运行中写入失败。
	if err := file.file.Close(); err != nil {
		t.Fatalf("close underlying log file: %v", err)
	}
	logger.Info("stderr-fallback-probe")
	if !strings.Contains(stderr.String(), "stderr-fallback-probe") {
		t.Fatalf("stderr was not written after file failure: %s", stderr.String())
	}
}

func writeLongConfigFile(t *testing.T, contents string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	name := strings.Repeat("c", 160) + ".yaml"
	for len(filepath.Join(dir, name)) < 820 {
		next := filepath.Join(dir, strings.Repeat("d", 160))
		if len(filepath.Join(next, name)) > 940 {
			break
		}
		if err := os.Mkdir(next, 0o700); err != nil {
			t.Fatal(err)
		}
		dir = next
	}
	return writeConfigAt(t, dir, name, contents), dir
}

func TestRunAcceptsSupportedLogLevels(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GHC2API_API_KEY", "")
	t.Setenv("COPILOT_HOST", "")
	credentials := testCredentialFile(t, `{"oauth_token":"log-level-token"}`)
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			configPath := writeConfigFile(t, appConfigYAML("0.0.0.0:0", credentials, "", "test-api-key", level))
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
		{"invalid-log-level", appConfigYAML("127.0.0.1:0", credentials, "", "", "trace")},
		{"invalid-host", appConfigYAML("127.0.0.1:0", credentials, "evil.example", "", "info")},
		{"legacy-cli-token-mode", appConfigYAML("127.0.0.1:0", credentials, "", "", "info") + "token_mode: cli\n"},
		{"invalid-listen-address", appConfigYAML("not-a-listener", credentials, "", "", "info")},
		{"non-local-listen-requires-api-key", appConfigYAML("0.0.0.0:0", credentials, "", "", "info")},
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
