package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestQuotaRefreshIntervalConfig quota_refresh_interval 默认 1 小时；合法的 Go duration（不小于 1 分钟）原样生效，
// 并出现在传给账号池的 PoolConfig 中；无法解析、非正或小于 1 分钟的值被拒绝，错误信息包含字段名。
func TestQuotaRefreshIntervalConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string // 空表示配置中省略该字段
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: time.Hour},
		{name: "custom", value: "30m", want: 30 * time.Minute},
		{name: "minimum-boundary", value: "1m", want: time.Minute},
		{name: "invalid", value: "abc", wantErr: true},
		{name: "zero", value: "0s", wantErr: true},
		{name: "negative", value: "-1h", wantErr: true},
		{name: "below-minimum", value: "30s", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contents := minimalConfig
			if tc.value != "" {
				contents += fmt.Sprintf("quota_refresh_interval: %s\n", strconv.Quote(tc.value))
			}
			configPath := writeConfigWithAccount(t, "quota-refresh", accountJSON("quota-refresh-config-token"), contents)
			config, err := loadConfig(configPath)
			if tc.wantErr {
				if err == nil {
					t.Fatal("非法的 quota_refresh_interval 未被拒绝")
				}
				if !strings.Contains(err.Error(), "quota_refresh_interval") {
					t.Fatalf("错误信息 = %q, want 包含 quota_refresh_interval", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() error = %v", err)
			}
			if config.QuotaRefreshInterval != tc.want {
				t.Fatalf("quota_refresh_interval = %s, want %s", config.QuotaRefreshInterval, tc.want)
			}
			if got := config.poolConfig(nil).QuotaRefreshInterval; got != tc.want {
				t.Fatalf("PoolConfig.QuotaRefreshInterval = %s, want %s", got, tc.want)
			}
		})
	}
}
