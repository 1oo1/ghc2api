package main

import (
	"path/filepath"
	"testing"
)

// TestAccountPriorityIsLoadedFromCredential 凭证文件中的 priority 被读取到账号配置，并带入账号池的输入；省略时为 0。
func TestAccountPriorityIsLoadedFromCredential(t *testing.T) {
	configDir := isolatedConfigDir(t)
	credentials := filepath.Join(configDir, "data", "credentials")
	writeCredentialFile(t, filepath.Join(credentials, "alice.json"), `{"oauth_token":"alice-token","priority":100}`)
	writeCredentialFile(t, filepath.Join(credentials, "bob.json"), `{"oauth_token":"bob-token","priority":-1}`)
	writeCredentialFile(t, filepath.Join(credentials, "carol.json"), accountJSON("carol-token"))
	config, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	want := map[string]int{"alice": 100, "bob": -1, "carol": 0}
	for user, priority := range want {
		if got := accountByUser(t, config, user).Priority; got != priority {
			t.Errorf("%s 的 Priority = %d, want %d", user, got, priority)
		}
	}
	// 账号池的输入与启动配置保持一致，优先级不能在转换时丢失。
	inputs := config.poolAccounts()
	if len(inputs) != len(want) {
		t.Fatalf("poolAccounts() 数量 = %d, want %d", len(inputs), len(want))
	}
	for _, input := range inputs {
		if input.Priority != want[input.User] {
			t.Errorf("poolAccounts() 中 %s 的 Priority = %d, want %d", input.User, input.Priority, want[input.User])
		}
	}
}

// TestAccountPriorityOutOfRangeFailsStartup 凭证中的优先级超出 [-1, 100] 或不是整数时，启动加载（loadConfig）失败。
func TestAccountPriorityOutOfRangeFailsStartup(t *testing.T) {
	for _, raw := range []string{
		`{"oauth_token":"t","priority":-2}`,
		`{"oauth_token":"t","priority":101}`,
		`{"oauth_token":"t","priority":1.5}`,
	} {
		t.Run(raw, func(t *testing.T) {
			configDir := isolatedConfigDir(t)
			writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "alice.json"), raw)
			if _, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig)); err == nil {
				t.Fatalf("loadConfig() 应拒绝凭证 %s", raw)
			}
		})
	}
}
