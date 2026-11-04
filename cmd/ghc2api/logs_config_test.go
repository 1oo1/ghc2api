package main

import (
	"path/filepath"
	"testing"
)

// TestPoolConfigLogFileJoinsLogsPath poolConfig 把当前日志文件的完整路径交给托管池：
// LogFile 等于 filepath.Join(LogsPath, "ghc2api.log")，使控制台读取的正是 newConfiguredLogger 写入的文件。
func TestPoolConfigLogFileJoinsLogsPath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
	}{
		{name: "default-logs-path", contents: "management_key: " + validManagementKey + "\n" + apiKeySetting},
		{name: "custom-logs-path", contents: "management_key: " + validManagementKey + "\nlogs_path: ./my-logs\n" + apiKeySetting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := isolatedConfigDir(t)
			config, err := loadConfig(writeConfigInDir(t, configDir, tc.contents))
			if err != nil {
				t.Fatalf("loadConfig() error = %v", err)
			}
			want := filepath.Join(config.LogsPath, "ghc2api.log")
			if got := config.poolConfig(nil).LogFile; got != want {
				t.Fatalf("poolConfig().LogFile = %q, want %q", got, want)
			}
		})
	}
}

// TestLogFileNameConstant 日志文件名由包内常量 logFileName 统一给出，写入与读取共用。
func TestLogFileNameConstant(t *testing.T) {
	if logFileName != "ghc2api.log" {
		t.Fatalf("logFileName = %q, want ghc2api.log", logFileName)
	}
}
