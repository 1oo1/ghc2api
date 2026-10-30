package main

// 凭证文件中的 proxy 字段在启动扫描时传入账号（见 docs/proxy.md「凭证字段」）。
// 约定的新字段：accountConfig.Proxy string（凭证文件 proxy 去除首尾空白后的值），并经 appConfig.poolAccounts 传给 copilot.PoolAccount.Proxy。

import (
	"fmt"
	"strings"
	"testing"
)

// TestLoadConfigPassesCredentialProxyToAccounts 带 proxy 的凭证加载后账号带有该代理，并传入账号池的输入；未写 proxy 的账号为空。
func TestLoadConfigPassesCredentialProxyToAccounts(t *testing.T) {
	configDir := isolatedConfigDir(t)
	credentials := configDir + "/data/credentials/"
	proxy := "socks5://proxy-user:proxy-pass@127.0.0.1:1080"
	writeCredentialFile(t, credentials+"alice.json", fmt.Sprintf(`{"oauth_token":"proxy-token-a","proxy":%q}`, proxy))
	writeCredentialFile(t, credentials+"bob.json", accountJSON("proxy-token-b"))

	config, err := loadConfig(writeConfigInDir(t, configDir, minimalConfig))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if got := accountByUser(t, config, "alice").Proxy; got != proxy {
		t.Errorf("alice 的 Proxy = %q, want %q", got, proxy)
	}
	if got := accountByUser(t, config, "bob").Proxy; got != "" {
		t.Errorf("bob 的 Proxy = %q, want 空", got)
	}
	for _, account := range config.poolAccounts() {
		want := ""
		if account.User == "alice" {
			want = proxy
		}
		if account.Proxy != want {
			t.Errorf("poolAccounts 中 %s 的 Proxy = %q, want %q", account.User, account.Proxy, want)
		}
	}
}

// TestLoadConfigRejectsInvalidCredentialProxy 凭证中的 proxy 非法时启动失败，错误指出文件名与“代理地址无效”，且不含密码。
func TestLoadConfigRejectsInvalidCredentialProxy(t *testing.T) {
	raw := `{"oauth_token":"proxy-token-a","proxy":"http://proxy-user:proxy-pass@127.0.0.1:1080"}`
	_, err := loadConfig(writeConfigWithAccount(t, "alice", raw, minimalConfig))
	if err == nil {
		t.Fatal("loadConfig() 应拒绝非法 proxy")
	}
	for _, want := range []string{"alice.json", "代理地址无效"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 %q 应包含 %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "proxy-pass") {
		t.Errorf("错误信息泄露了代理密码: %v", err)
	}
}
