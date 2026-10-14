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

// minimalConfig 是只改变凭证目录扫描所需内容的最小配置（YAML 不能为空文档）。
const minimalConfig = "log_level: info\n"

// accountJSON 返回只包含 oauth_token 的凭证文件内容。
func accountJSON(token string) string {
	return fmt.Sprintf(`{"oauth_token":%q}`, token)
}

// writeConfigWithAccount 在新的临时目录中写入配置文件，并在默认 data_path（配置目录下的 data）
// 的 credentials 目录中放入 <user>.json，返回配置文件路径。
func writeConfigWithAccount(t *testing.T, user, raw, contents string) string {
	t.Helper()
	dir := t.TempDir()
	writeCredentialFile(t, filepath.Join(dir, "data", "credentials", user+".json"), raw)
	return writeConfigInDir(t, dir, contents)
}

// isolatedConfigDir 返回路径中不含测试名的临时目录，避免路径片段意外满足错误信息断言。
func isolatedConfigDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ghc2api-config-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// accountByUser 按用户名查找账号，不依赖账号顺序。
func accountByUser(t *testing.T, config appConfig, user string) accountConfig {
	t.Helper()
	for _, account := range config.Accounts {
		if account.User == user {
			return account
		}
	}
	t.Fatalf("缺少用户 %q 的账号，账号数量 = %d", user, len(config.Accounts))
	return accountConfig{}
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

func appConfigYAML(listen, host, apiKey, logLevel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "listen: %s\n", strconv.Quote(listen))
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
	writeCredentialFile(t, filepath.Join(dir, "data", "credentials", "alice.json"),
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
	// 只写入必需的最小配置，其余设置都省略并使用默认值。
	configPath := writeConfigWithAccount(t, "default-values", accountJSON("default-values-token"), minimalConfig)
	err := runWithCancelledContext([]string{"-c", configPath})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("省略可选项时默认配置无效: %v", err)
	}
}

func TestLoadConfigUsesDefaultDataAndCredentialsPaths(t *testing.T) {
	configDir := t.TempDir()
	configPath := writeConfigInDir(t, configDir, "listen: 127.0.0.1:0\n")
	dataPath := filepath.Join(configDir, "data")
	writeCredentialFile(t, filepath.Join(dataPath, "credentials", "alice.json"),
		`{"oauth_token":"default-data-path-token"}`)

	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig() with default data_path credentials: %v", err)
	}
	if token := singleAccount(t, config).Token; token != "default-data-path-token" {
		t.Fatalf("token loaded from default credentials = %q", token)
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
	writeCredentialFile(t, filepath.Join(dataPath, "credentials", "alice.json"),
		`{"oauth_token":"relative-data-path-token"}`)

	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig() with relative data_path: %v", err)
	}
	if token := singleAccount(t, config).Token; token != "relative-data-path-token" {
		t.Fatalf("token loaded from relative data_path = %q", token)
	}
	assertResolvedDataAndLogPaths(t, config, dataPath, filepath.Join(configDir, "logs"))
}

// singleAccount 返回唯一的账号配置；数量不符时立即失败，避免断言时越界。
func singleAccount(t *testing.T, config appConfig) accountConfig {
	t.Helper()
	if len(config.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(config.Accounts))
	}
	return config.Accounts[0]
}

// TestLoadConfigScansCredentialsUnderCustomDataPath 覆盖相对与绝对 data_path 下的凭证目录扫描。
func TestLoadConfigScansCredentialsUnderCustomDataPath(t *testing.T) {
	root := t.TempDir()
	relativeData := filepath.Join(root, "config", "state")
	parentData := filepath.Join(root, "shared")
	absoluteData := filepath.Join(root, "absolute-state")
	for _, tc := range []struct{ name, dataPath, wantData, user, token string }{
		{"relative-under-config-dir", "state", relativeData, "rel-user", "rel-token"},
		{"relative-parent-directory", "../shared", parentData, "parent-user", "parent-token"},
		{"absolute", absoluteData, absoluteData, "abs-user", "abs-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(tc.wantData, "credentials", tc.user+".json")
			writeCredentialFile(t, file, accountJSON(tc.token))
			configPath := writeConfigInDir(t, filepath.Join(root, "config"),
				fmt.Sprintf("data_path: %s\n", strconv.Quote(tc.dataPath)))
			config, err := loadConfig(configPath)
			if err != nil {
				t.Fatalf("loadConfig() error = %v", err)
			}
			if got := configStringField(t, config, "DataPath", "dataPath"); got != tc.wantData {
				t.Errorf("data_path = %q, want %q", got, tc.wantData)
			}
			account := singleAccount(t, config)
			if account.User != tc.user || account.Token != tc.token || account.credentialFile != file {
				t.Errorf("账号 user=%q file=%q, want user=%q file=%q", account.User, account.credentialFile, tc.user, file)
			}
		})
	}
}

// TestLoadConfigScansAccountsSortedByFileName 账号顺序与文件名排序一致，User 保持文件名原有大小写。
func TestLoadConfigScansAccountsSortedByFileName(t *testing.T) {
	configDir := t.TempDir()
	dataPath := filepath.Join(configDir, "data")
	for name, token := range map[string]string{
		"work.json": "sorted-work-token", "alpha.json": "sorted-alpha-token", "Team-Bot.json": "sorted-team-token",
	} {
		writeCredentialFile(t, filepath.Join(dataPath, "credentials", name), accountJSON(token))
	}
	config, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	want := []struct{ user, token string }{
		{"Team-Bot", "sorted-team-token"},
		{"alpha", "sorted-alpha-token"},
		{"work", "sorted-work-token"},
	}
	if len(config.Accounts) != len(want) {
		t.Fatalf("accounts = %d, want %d", len(config.Accounts), len(want))
	}
	for i, w := range want {
		account := config.Accounts[i]
		file := filepath.Join(dataPath, "credentials", w.user+".json")
		if account.User != w.user || account.Token != w.token || account.credentialFile != file {
			t.Errorf("account %d = user %q file %q, want user %q file %q",
				i, account.User, account.credentialFile, w.user, file)
		}
	}
}

// TestLoadConfigIgnoresHiddenNonJSONFilesAndSubdirectories 隐藏文件、非 .json 文件、子目录（含子目录中的 .json）都被忽略；
// 这些忽略项的内容都是无效凭证，若被读取则加载会失败。
func TestLoadConfigIgnoresHiddenNonJSONFilesAndSubdirectories(t *testing.T) {
	configDir := t.TempDir()
	credentials := filepath.Join(configDir, "data", "credentials")
	writeCredentialFile(t, filepath.Join(credentials, "work.json"), accountJSON("ignore-work-token"))
	for _, name := range []string{".hidden.json", ".ghc2api-credential-123", "notes.txt", "backup.json.bak", "README"} {
		writeCredentialFile(t, filepath.Join(credentials, name), "not json")
	}
	writeCredentialFile(t, filepath.Join(credentials, "nested", "inner.json"), "not json")
	if err := os.Mkdir(filepath.Join(credentials, "folder.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
	if err != nil {
		t.Fatalf("被忽略的条目不应导致加载失败: %v", err)
	}
	if account := singleAccount(t, config); account.User != "work" || account.Token != "ignore-work-token" {
		t.Fatalf("账号 user=%q, want 仅 work", account.User)
	}
}

// TestLoadConfigFollowsSymlinkToRegularFileOnly 指向普通文件的符号链接会被加载；指向目录的符号链接被忽略。
func TestLoadConfigFollowsSymlinkToRegularFileOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 下创建符号链接需要额外权限")
	}
	configDir := t.TempDir()
	credentials := filepath.Join(configDir, "data", "credentials")
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	writeCredentialFile(t, target, accountJSON("symlink-user-token"))
	if err := os.Symlink(target, filepath.Join(credentials, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(credentials, "dirlink.json")); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	account := singleAccount(t, config)
	if account.User != "linked" || account.Token != "symlink-user-token" {
		t.Fatalf("账号 user=%q, want linked", account.User)
	}
}

// TestLoadConfigRejectsInvalidGitHubUserNames 文件名去掉 .json 后必须是合法的 GitHub 用户名，错误信息包含文件名。
func TestLoadConfigRejectsInvalidGitHubUserNames(t *testing.T) {
	for _, name := range []string{
		"-bad.json", "bad-.json", "a--b.json", "bad_name.json", "bad name.json", strings.Repeat("a", 40) + ".json",
	} {
		t.Run(name, func(t *testing.T) {
			configDir := isolatedConfigDir(t)
			writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", name), accountJSON("invalid-name-token"))
			_, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
			if err == nil {
				t.Fatalf("非法用户名文件 %q 未被拒绝", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("错误信息应包含文件名 %q: %v", name, err)
			}
		})
	}
}

// TestLoadConfigAcceptsValidGitHubUserNameBoundaries 合法的边界用户名（1 个字符、39 个字符、单个连字符）可以加载。
func TestLoadConfigAcceptsValidGitHubUserNameBoundaries(t *testing.T) {
	names := []string{"a", "a-b", strings.Repeat("a", 39), "A1-b2-C3"}
	configDir := t.TempDir()
	for i, name := range names {
		writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", name+".json"),
			accountJSON(fmt.Sprintf("valid-name-token-%d", i)))
	}
	config, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
	if err != nil {
		t.Fatalf("合法用户名应被接受: %v", err)
	}
	if len(config.Accounts) != len(names) {
		t.Fatalf("accounts = %d, want %d", len(config.Accounts), len(names))
	}
	for _, name := range names {
		accountByUser(t, config, name)
	}
}

// TestLoadConfigChecksUserFieldAgainstFileName 凭证中的 user 为空或与文件名（忽略大小写）一致时通过，不一致时报错。
func TestLoadConfigChecksUserFieldAgainstFileName(t *testing.T) {
	t.Run("matches-ignoring-case", func(t *testing.T) {
		configDir := t.TempDir()
		writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "Alice.json"),
			`{"oauth_token":"user-match-token","user":"alice"}`)
		config, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
		if err != nil {
			t.Fatalf("user 与文件名仅大小写不同时应通过: %v", err)
		}
		if got := singleAccount(t, config).User; got != "Alice" {
			t.Fatalf("User = %q, want 保持文件名大小写 Alice", got)
		}
	})
	t.Run("empty-user-allowed", func(t *testing.T) {
		configDir := t.TempDir()
		writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "carol.json"),
			`{"oauth_token":"user-empty-token","user":""}`)
		if _, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig)); err != nil {
			t.Fatalf("空 user 字段应被接受: %v", err)
		}
	})
	t.Run("mismatch-rejected", func(t *testing.T) {
		configDir := t.TempDir()
		writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "bob.json"),
			`{"oauth_token":"user-mismatch-token","user":"alice"}`)
		_, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
		if err == nil {
			t.Fatal("user 与文件名不一致未被拒绝")
		}
		if !strings.Contains(err.Error(), "bob.json") {
			t.Fatalf("错误信息应包含文件名 bob.json: %v", err)
		}
	})
}

// TestLoadConfigRejectsMissingOrEmptyCredentialsDirectory 目录不存在、为空或只有被忽略的条目时报错，错误信息包含 credentials。
func TestLoadConfigRejectsMissingOrEmptyCredentialsDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, credentials string)
	}{
		{"missing-directory", func(t *testing.T, credentials string) {}},
		{"empty-directory", func(t *testing.T, credentials string) {
			if err := os.MkdirAll(credentials, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"only-ignored-entries", func(t *testing.T, credentials string) {
			writeCredentialFile(t, filepath.Join(credentials, ".hidden.json"), accountJSON("ignored-token"))
			writeCredentialFile(t, filepath.Join(credentials, "notes.txt"), accountJSON("ignored-token"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := isolatedConfigDir(t)
			tc.setup(t, filepath.Join(configDir, "data", "credentials"))
			_, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
			if err == nil {
				t.Fatal("没有凭证文件时未被拒绝")
			}
			// 错误必须指出 credentials；同时不能是旧的默认文件 credentials.json 缺失错误，否则说明仍在读取旧字段。
			if !strings.Contains(err.Error(), "credentials") || strings.Contains(err.Error(), "credentials.json") {
				t.Fatalf("错误信息应指出 credentials 目录而不是默认文件: %v", err)
			}
		})
	}
}

// TestLoadConfigRejectsDuplicateAccountAcrossFiles 两个文件规范化后 host+token 相同时报错。
func TestLoadConfigRejectsDuplicateAccountAcrossFiles(t *testing.T) {
	configDir := t.TempDir()
	credentials := filepath.Join(configDir, "data", "credentials")
	writeCredentialFile(t, filepath.Join(credentials, "a.json"), accountJSON("duplicate-token"))
	writeCredentialFile(t, filepath.Join(credentials, "b.json"), `{"oauth_token":"duplicate-token","host":"github.com"}`)
	_, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
	if err == nil {
		t.Fatal("重复账号未被拒绝")
	}
	if !strings.Contains(err.Error(), "账号与之前的账号重复") {
		t.Fatalf("错误信息应指出重复账号: %v", err)
	}
}

// TestLoadConfigAllowsSameTokenOnDifferentHosts 同一 token 在不同 host 上是不同账号，允许共存。
func TestLoadConfigAllowsSameTokenOnDifferentHosts(t *testing.T) {
	configDir := t.TempDir()
	credentials := filepath.Join(configDir, "data", "credentials")
	writeCredentialFile(t, filepath.Join(credentials, "github.json"), accountJSON("shared-token"))
	writeCredentialFile(t, filepath.Join(credentials, "acme.json"), `{"oauth_token":"shared-token","host":"acme.ghe.com"}`)
	config, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
	if err != nil {
		t.Fatalf("同一 token 在不同 host 上应被接受: %v", err)
	}
	if len(config.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(config.Accounts))
	}
	if h := accountByUser(t, config, "github").Host; h != "" {
		t.Fatalf("github host = %q, want 空", h)
	}
	if h := accountByUser(t, config, "acme").Host; h != "acme.ghe.com" {
		t.Fatalf("acme host = %q, want acme.ghe.com", h)
	}
}

// TestLoadConfigTopLevelHostOverridesEveryAccount 顶层 host 覆盖所有凭证文件中的 host，并保留去空白后的原值。
func TestLoadConfigTopLevelHostOverridesEveryAccount(t *testing.T) {
	configDir := t.TempDir()
	credentials := filepath.Join(configDir, "data", "credentials")
	writeCredentialFile(t, filepath.Join(credentials, "work.json"), accountJSON("host-work-token"))
	writeCredentialFile(t, filepath.Join(credentials, "team.json"), `{"oauth_token":"host-team-token","host":"github.com"}`)
	configPath := writeConfigInDir(t, configDir, "host: \"  acme.ghe.com  \"\n")
	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if config.Host != "acme.ghe.com" {
		t.Fatalf("top-level host = %q, want trimmed acme.ghe.com", config.Host)
	}
	if len(config.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(config.Accounts))
	}
	for i, account := range config.Accounts {
		if account.Host != "acme.ghe.com" {
			t.Errorf("account %d host = %q, want 顶层 host acme.ghe.com", i, account.Host)
		}
	}
}

// TestLoadConfigRejectsRemovedCredentialsField 配置中仍出现 credentials 字段时报错（不做兼容）。
func TestLoadConfigRejectsRemovedCredentialsField(t *testing.T) {
	for _, value := range []string{"work", "[work.json]", `""`} {
		t.Run(value, func(t *testing.T) {
			configPath := writeConfigWithAccount(t, "work", accountJSON("removed-field-token"),
				"credentials: "+value+"\n")
			_, err := loadConfig(configPath)
			if err == nil {
				t.Fatalf("已移除的 credentials 字段 %s 未被拒绝", value)
			}
			// 必须是 YAML 解析阶段（KnownFields）拒绝字段，而不是之后加载凭证失败时的 credentials 错误。
			if !strings.Contains(err.Error(), "不是有效 YAML") || !strings.Contains(err.Error(), "credentials") {
				t.Fatalf("错误信息应由 YAML 解析阶段指出 credentials 字段: %v", err)
			}
		})
	}
}

func TestLogRotationConfigValuesAndValidation(t *testing.T) {
	configPath := writeConfigWithAccount(t, "log-rotation", accountJSON("log-rotation-config-token"),
		"log_max_size_mb: 1\nlog_max_backups: 2\n")
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
			invalidConfigPath := writeConfigWithAccount(t, "log-rotation", accountJSON("log-rotation-config-token"), tc.fields)
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
			contents := minimalConfig
			if tc.value != "" {
				contents += fmt.Sprintf("models_cache_ttl: %s\n", strconv.Quote(tc.value))
			}
			configPath := writeConfigWithAccount(t, "models-cache", accountJSON("models-cache-config-token"), contents)
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

	configPath := writeConfigWithAccount(t, "config-permission", accountJSON("permission-warning-oauth-token"),
		"log_level: info\n")
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

	configDir := t.TempDir()
	credentialPath := filepath.Join(configDir, "data", "credentials", "permission-warning.json")
	writeCredentialFile(t, credentialPath, accountJSON("credential-permission-warning-token"))
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

	configPath := writeConfigInDir(t, configDir, appConfigYAML("127.0.0.1:0", "", "", "info"))
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

func TestRunWarnsForEachCredentialFileWithBroadPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持 Unix 凭证文件权限检查")
	}

	configDir := t.TempDir()
	dataPath := filepath.Join(configDir, "data")
	modes := map[string]os.FileMode{"strict.json": 0o600, "open.json": 0o644, "group.json": 0o640}
	for name, mode := range modes {
		path := filepath.Join(dataPath, "credentials", name)
		writeCredentialFile(t, path, accountJSON("permission-"+strings.TrimSuffix(name, ".json")))
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	configPath := writeConfigInDir(t, configDir, minimalConfig)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	err := run(ctx, []string{"-c", configPath}, &stdout, &stderr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run with cancelled context = %v, want context.Canceled", err)
	}

	warnings := map[string]int{}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSON log line %q: %v", line, err)
		}
		if entry["level"] != "WARN" || entry["msg"] != "凭证文件权限过宽，建议执行 chmod 600" {
			continue
		}
		file, _ := entry["credential_file"].(string)
		warnings[file]++
	}
	// 每个权限过宽的凭证文件各一条告警；0600 的文件不告警。
	for _, name := range []string{"open.json", "group.json"} {
		path := filepath.Join(dataPath, "credentials", name)
		if warnings[path] != 1 {
			t.Errorf("%s 权限过宽告警 %d 条, want 1; stderr=%s", name, warnings[path], stderr.String())
		}
	}
	if warnings[filepath.Join(dataPath, "credentials", "strict.json")] != 0 || len(warnings) != 2 {
		t.Errorf("告警文件集合 = %v, want 仅 open.json 与 group.json", warnings)
	}
}

func TestRunLoadsYAMLServiceSettings(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GHC2API_API_KEY", "")
	t.Setenv("COPILOT_HOST", "")
	t.Setenv("COPILOT_TOKEN_MODE", "cli")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	const apiKey = "yaml-api-key-secret"
	configPath := writeConfigWithAccount(t, "yaml-service", accountJSON("yaml-oauth-token"),
		appConfigYAML(addr, "acme.ghe.com", apiKey, "info"))
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
	for _, tc := range []struct {
		field string
		value string
	}{
		{field: "github_token", value: "legacy-inline-token"},
		{field: "editor_config", value: "legacy-editor-path"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			contents := fmt.Sprintf("%s: %s\n", tc.field, strconv.Quote(tc.value))
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
			writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "alice.json"),
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
	writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "alice.json"),
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
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			configPath := writeConfigWithAccount(t, "log-level", accountJSON("log-level-token"),
				appConfigYAML("0.0.0.0:0", "", "test-api-key", level))
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
	for _, tc := range []struct {
		name   string
		config string
	}{
		{"malformed-yaml", "listen: [\n"},
		{"invalid-log-level", appConfigYAML("127.0.0.1:0", "", "", "trace")},
		{"invalid-host", appConfigYAML("127.0.0.1:0", "evil.example", "", "info")},
		{"legacy-cli-token-mode", appConfigYAML("127.0.0.1:0", "", "", "info") + "token_mode: cli\n"},
		{"invalid-listen-address", appConfigYAML("not-a-listener", "", "", "info")},
		{"non-local-listen-requires-api-key", appConfigYAML("0.0.0.0:0", "", "", "info")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := writeConfigWithAccount(t, "validation", accountJSON("config-validation-token"), tc.config)
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

// TestLoadConfigIgnoresUppercaseJSONExtension 扩展名大小写不同（如 Alice.JSON）不是凭证文件，目录里只有它时报没有凭证文件。
func TestLoadConfigIgnoresUppercaseJSONExtension(t *testing.T) {
	configDir := isolatedConfigDir(t)
	writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "Alice.JSON"), accountJSON("uppercase-ext-token"))
	_, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
	if err == nil {
		t.Fatal("仅有 Alice.JSON 时未报错，扩展名应区分大小写")
	}
	if !strings.Contains(err.Error(), "没有凭证文件") {
		t.Fatalf("错误信息应指出没有凭证文件: %v", err)
	}
}

// TestLoadConfigRejectsUserNamesDifferingOnlyByCase 两个文件的用户名只有大小写不同（Alice.json 与 alice.json）时，
// 它们是同一个 GitHub 用户名，不能同时作为两个账号加载。
// 该场景需要大小写敏感的文件系统才能同时创建两个文件；macOS 默认文件系统不区分大小写，此时跳过。
func TestLoadConfigRejectsUserNamesDifferingOnlyByCase(t *testing.T) {
	probeDir := isolatedConfigDir(t)
	writeCredentialFile(t, filepath.Join(probeDir, "a.json"), accountJSON("case-probe-token"))
	if _, err := os.Stat(filepath.Join(probeDir, "A.json")); err == nil {
		t.Skip("文件系统不区分大小写（例如 macOS 默认），无法同时创建 Alice.json 与 alice.json")
	}
	configDir := isolatedConfigDir(t)
	credentials := filepath.Join(configDir, "data", "credentials")
	writeCredentialFile(t, filepath.Join(credentials, "Alice.json"), accountJSON("case-alice-token"))
	writeCredentialFile(t, filepath.Join(credentials, "alice.json"), accountJSON("case-alice-other-token"))
	_, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
	if err == nil {
		t.Fatal("仅大小写不同的重复用户名未被拒绝")
	}
	message := err.Error()
	if !strings.Contains(message, "重复") || (!strings.Contains(message, "Alice.json") && !strings.Contains(message, "alice.json")) {
		t.Fatalf("错误信息应指出重复并包含文件名: %v", err)
	}
}
