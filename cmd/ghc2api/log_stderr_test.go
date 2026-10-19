package main

// 本文件覆盖 log_to_stderr 配置项：省略或 true 时日志同时写入日志文件与标准错误（保持现有行为）；
// 显式 false 时日志只写入 <logs_path>/ghc2api.log，标准错误不输出任何日志。
// 配置值经 loadConfig 解析为 appConfig.LogFileOnly（零值 false 即同时写 stderr），再由 newConfiguredLogger 生效。
// 致命错误（run 返回的 error）不受此项影响，不在本文件覆盖范围内。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// logStderrProbe 是直接调用 logger 时写入的探针消息，用于区分文件与标准错误的输出。
const logStderrProbe = "log-to-stderr-probe"

// logStartupProbe 是启动时必定输出的“配置加载完成”日志（默认级别 info）。
const logStartupProbe = "配置加载完成"

// assertLogFileContains 断言日志文件存在且包含 probe。
func assertLogFileContains(t *testing.T, path, probe string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志文件 %q 失败: %v", path, err)
	}
	if !strings.Contains(string(data), probe) {
		t.Fatalf("日志文件 %q 缺少 %q: %s", path, probe, data)
	}
}

// assertStderrLogging 断言标准错误的输出：wantLogs 为 true 时必须包含 probe；为 false 时必须完全为空，即不含任何日志。
func assertStderrLogging(t *testing.T, stderr, probe string, wantLogs bool) {
	t.Helper()
	if !wantLogs {
		if stderr != "" {
			t.Fatalf("log_to_stderr: false 时标准错误应为空，实际输出: %s", stderr)
		}
		return
	}
	if !strings.Contains(stderr, probe) {
		t.Fatalf("标准错误缺少日志 %q: %s", probe, stderr)
	}
}

// TestLogToStderrConfigValues log_to_stderr 省略、为 null 或为 true 时 LogFileOnly 为 false（同时写 stderr）；
// 为 false 时 LogFileOnly 为 true（只写文件）；非布尔值（如 "abc"）启动失败。
func TestLogToStderrConfigValues(t *testing.T) {
	for _, tc := range []struct {
		name         string
		contents     string
		wantFileOnly bool
		wantErr      bool
	}{
		{name: "omitted", contents: ""},
		{name: "null", contents: "log_to_stderr: null\n"},
		{name: "true", contents: "log_to_stderr: true\n"},
		{name: "false", contents: "log_to_stderr: false\n", wantFileOnly: true},
		{name: "non-bool-fails", contents: "log_to_stderr: \"abc\"\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := loadSecurityConfig(t, apiKeySetting+tc.contents)
			if tc.wantErr {
				if err == nil {
					t.Fatal("非布尔值的 log_to_stderr 未被拒绝")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() error = %v", err)
			}
			if config.LogFileOnly != tc.wantFileOnly {
				t.Fatalf("LogFileOnly = %t, want %t", config.LogFileOnly, tc.wantFileOnly)
			}
		})
	}
}

// TestNewConfiguredLoggerStderrFollowsLogFileOnly 直接用 appConfig 调用 newConfiguredLogger：
// LogFileOnly 为 false 时日志同时出现在日志文件与标准错误；为 true 时只出现在日志文件，标准错误缓冲保持为空。
func TestNewConfiguredLoggerStderrFollowsLogFileOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fileOnly   bool
		wantStderr bool
	}{
		{name: "both-outputs", fileOnly: false, wantStderr: true},
		{name: "file-only", fileOnly: true, wantStderr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logsDir := t.TempDir()
			stderr := &bytes.Buffer{}
			logger, file, err := newConfiguredLogger(appConfig{
				LogsPath: logsDir, LogLevel: "info", LogMaxSizeMB: 1, LogMaxBackups: 1,
				LogFileOnly: tc.fileOnly,
			}, stderr)
			if err != nil {
				t.Fatalf("newConfiguredLogger() error = %v", err)
			}
			t.Cleanup(func() { _ = file.Close() })

			logger.Info(logStderrProbe)
			assertLogFileContains(t, filepath.Join(logsDir, "ghc2api.log"), logStderrProbe)
			assertStderrLogging(t, stderr.String(), logStderrProbe, tc.wantStderr)
		})
	}
}

// TestLogToStderrLoadConfigToConfiguredLogger 经过 loadConfig 再调用 newConfiguredLogger 的组合用例：
// 省略或 true 时日志同时出现在日志文件与标准错误；false 时只出现在日志文件，标准错误缓冲保持为空。
func TestLogToStderrLoadConfigToConfiguredLogger(t *testing.T) {
	for _, tc := range []struct {
		name       string
		contents   string
		wantStderr bool
	}{
		{name: "omitted", contents: "", wantStderr: true},
		{name: "true", contents: "log_to_stderr: true\n", wantStderr: true},
		{name: "false", contents: "log_to_stderr: false\n", wantStderr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := loadConfig(writeConfigWithAccount(t, "log-stderr", accountJSON("log-stderr-token"),
				minimalConfig+tc.contents))
			if err != nil {
				t.Fatalf("loadConfig() error = %v", err)
			}
			stderr := &bytes.Buffer{}
			logger, file, err := newConfiguredLogger(config, stderr)
			if err != nil {
				t.Fatalf("newConfiguredLogger() error = %v", err)
			}
			t.Cleanup(func() { _ = file.Close() })

			logger.Info(logStderrProbe)
			assertLogFileContains(t, filepath.Join(config.LogsPath, "ghc2api.log"), logStderrProbe)
			assertStderrLogging(t, stderr.String(), logStderrProbe, tc.wantStderr)
		})
	}
}

// TestRunLogToStderrControlsStderrOutput 端到端：经 run 启动服务，等待就绪后取消上下文并等待退出。
// 省略或 true 时标准错误包含“配置加载完成”；false 时标准错误不含任何日志，而日志文件仍包含该条日志。
func TestRunLogToStderrControlsStderrOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		contents   string
		wantStderr bool
	}{
		{name: "omitted", contents: "", wantStderr: true},
		{name: "true", contents: "log_to_stderr: true\n", wantStderr: true},
		{name: "false", contents: "log_to_stderr: false\n", wantStderr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := freeLoopbackAddressForTest(t)
			configDir := isolatedConfigDir(t)
			writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "alice.json"),
				accountJSON("log-stderr-token"))
			configPath := writeConfigInDir(t, configDir, appConfigYAML(addr, "", validAPIKey, "info")+tc.contents)

			stop, stderr := startServiceWithStop(t, configPath, addr)
			if err := stop(); err != nil {
				t.Fatalf("serve 返回错误: %v; stderr=%s", err, stderr.String())
			}

			assertLogFileContains(t, filepath.Join(configDir, "logs", "ghc2api.log"), logStartupProbe)
			assertStderrLogging(t, stderr.String(), logStartupProbe, tc.wantStderr)
		})
	}
}
