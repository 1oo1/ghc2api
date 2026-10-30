package copilot

// 代理 URL 校验、凭证中的 proxy 字段，以及构造账号时对 proxy 的校验（见 docs/proxy.md「凭证字段」）。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeProxyAcceptsValidAddresses 合法的代理地址原样通过（只去除首尾空白，scheme 保持原样），空串表示无代理。
func TestNormalizeProxyAcceptsValidAddresses(t *testing.T) {
	user255 := strings.Repeat("u", 255)
	for _, tc := range []struct{ name, raw, want string }{
		{"空串表示无代理", "", ""},
		{"纯空白表示无代理", "  \t ", ""},
		{"socks5", "socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"socks5h 与域名", "socks5h://proxy.example.com:1080", "socks5h://proxy.example.com:1080"},
		{"scheme 大小写不敏感且保持原样", "SOCKS5://proxy.example.com:1080", "SOCKS5://proxy.example.com:1080"},
		{"只有用户名", "socks5://user@127.0.0.1:1080", "socks5://user@127.0.0.1:1080"},
		{"用户名与密码", "socks5://user:pass@127.0.0.1:1080", "socks5://user:pass@127.0.0.1:1080"},
		{"百分号编码的用户名与密码", "socks5://us%40er:p%3Ass%2F@127.0.0.1:1080", "socks5://us%40er:p%3Ass%2F@127.0.0.1:1080"},
		{"IPv6 主机", "socks5://[::1]:1080", "socks5://[::1]:1080"},
		{"IPv6 主机带认证与最大端口", "socks5://user:pass@[2001:db8::1]:65535", "socks5://user:pass@[2001:db8::1]:65535"},
		{"允许尾随斜杠", "socks5://127.0.0.1:1080/", "socks5://127.0.0.1:1080/"},
		{"去除首尾空白", " \tsocks5://127.0.0.1:1080\n", "socks5://127.0.0.1:1080"},
		{"最小端口", "socks5://127.0.0.1:1", "socks5://127.0.0.1:1"},
		{"用户名与密码各恰为 255 字节", "socks5://" + user255 + ":" + user255 + "@h:1080", "socks5://" + user255 + ":" + user255 + "@h:1080"},
		{"长度按解码后的字节计算（85 个汉字共 255 字节）", "socks5://" + strings.Repeat("%E4%B8%AD", 85) + "@h:1080", "socks5://" + strings.Repeat("%E4%B8%AD", 85) + "@h:1080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeProxy(tc.raw)
			if err != nil || got != tc.want {
				t.Fatalf("normalizeProxy(%q) = %q, %v; want %q, nil", tc.raw, got, err, tc.want)
			}
		})
	}
}

// TestNormalizeProxyRejectsInvalidAddresses 非法地址返回以“代理地址无效”开头的错误，且错误信息不含密码。
func TestNormalizeProxyRejectsInvalidAddresses(t *testing.T) {
	secret := proxyTestSecret
	long := strings.Repeat("x", 256)
	for _, tc := range []struct{ name, raw string }{
		{"scheme 不是 socks5", "http://u:" + secret + "@127.0.0.1:1080"},
		{"https scheme", "https://127.0.0.1:1080"},
		{"没有 scheme", "127.0.0.1:1080"},
		{"缺少端口", "socks5://u:" + secret + "@127.0.0.1"},
		{"缺少主机", "socks5://u:" + secret + "@:1080"},
		{"端口为 0", "socks5://u:" + secret + "@127.0.0.1:0"},
		{"端口超过 65535", "socks5://u:" + secret + "@127.0.0.1:65536"},
		{"端口不是数字", "socks5://u:" + secret + "@127.0.0.1:abc"},
		{"带路径", "socks5://u:" + secret + "@127.0.0.1:1080/path"},
		{"带 query", "socks5://u:" + secret + "@127.0.0.1:1080?x=1"},
		{"带 fragment", "socks5://u:" + secret + "@127.0.0.1:1080#frag"},
		{"有密码无用户名", "socks5://:" + secret + "@127.0.0.1:1080"},
		{"用户名超过 255 字节", "socks5://" + long + ":" + secret + "@127.0.0.1:1080"},
		{"密码超过 255 字节", "socks5://u:" + long + secret + "@127.0.0.1:1080"},
		{"长度按解码后的字节计算（86 个汉字共 258 字节）", "socks5://" + strings.Repeat("%E4%B8%AD", 86) + ":" + secret + "@h:1080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeProxy(tc.raw)
			if err == nil {
				t.Fatalf("normalizeProxy(%q) = %q, want 错误", tc.raw, got)
			}
			if !strings.HasPrefix(err.Error(), "代理地址无效") {
				t.Fatalf("错误 %q 应以“代理地址无效”开头", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("错误信息泄露了密码: %v", err)
			}
		})
	}
}

// TestCredentialProxyRoundTrip proxy 字段写入凭证文件后可原样读回，文件里的键名为 proxy。
func TestCredentialProxyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alice.json")
	proxy := "socks5://user:p%40ss@127.0.0.1:1080"
	if err := SaveCredential(path, Credential{Token: "oauth-a", User: "alice", Proxy: proxy}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCredential(path)
	if err != nil || got.Proxy != proxy {
		t.Fatalf("LoadCredential() = %+v, %v; want Proxy=%q", got, err, proxy)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil || fields["proxy"] != proxy {
		t.Fatalf("凭证文件应含 proxy 字段 %q: %s", proxy, data)
	}
}

// TestCredentialOmitsEmptyProxy 没有代理时不写出 proxy 字段，未写 proxy 的旧凭证照常读取。
func TestCredentialOmitsEmptyProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alice.json")
	if err := SaveCredential(path, Credential{Token: "oauth-a", User: "alice"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "proxy") {
		t.Fatalf("没有代理时不应写出 proxy: %s", data)
	}
	if got, err := LoadCredential(path); err != nil || got.Proxy != "" {
		t.Fatalf("LoadCredential() = %+v, %v; want 无代理", got, err)
	}
}

// TestCredentialTrimsProxy 保存与读取时去除 proxy 首尾空白；纯空白视为无代理，且不写出该字段。
func TestCredentialTrimsProxy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alice.json")
	if err := SaveCredential(path, Credential{Token: "oauth-a", User: "alice", Proxy: "  socks5://127.0.0.1:1080 "}); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadCredential(path); err != nil || got.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("LoadCredential() = %+v, %v; want 去除空白后的代理", got, err)
	}
	blank := filepath.Join(dir, "blank.json")
	if err := SaveCredential(blank, Credential{Token: "oauth-b", User: "blank", Proxy: "   "}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(blank); strings.Contains(string(data), "proxy") {
		t.Fatalf("纯空白代理不应写出: %s", data)
	}
}

// TestCredentialRejectsInvalidProxy SaveCredential 拒绝非法 proxy 且不创建文件；LoadCredential 拒绝含非法 proxy 的文件；
// 两者的错误信息都不含密码。
func TestCredentialRejectsInvalidProxy(t *testing.T) {
	dir := t.TempDir()
	bad := "http://u:" + proxyTestSecret + "@127.0.0.1:1080"

	savePath := filepath.Join(dir, "save.json")
	err := SaveCredential(savePath, Credential{Token: "oauth-a", User: "alice", Proxy: bad})
	if err == nil || !strings.Contains(err.Error(), "代理地址无效") || strings.Contains(err.Error(), proxyTestSecret) {
		t.Fatalf("SaveCredential 错误 = %v, want 代理地址无效且不含密码", err)
	}
	if _, statErr := os.Stat(savePath); !os.IsNotExist(statErr) {
		t.Fatalf("非法代理时不应创建凭证文件: %v", statErr)
	}

	loadPath := filepath.Join(dir, "load.json")
	raw, _ := json.Marshal(map[string]string{"oauth_token": "oauth-a", "proxy": bad})
	if err := os.WriteFile(loadPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadCredential(loadPath)
	if err == nil || !strings.Contains(err.Error(), "代理地址无效") || strings.Contains(err.Error(), proxyTestSecret) {
		t.Fatalf("LoadCredential 错误 = %v, want 代理地址无效且不含密码", err)
	}
}

// TestNewValidatesConfigProxy New 校验 Config.Proxy：非法地址报“代理地址无效”且不含密码；
// 配置了代理时注入的客户端必须是 nil 或 *http.Transport（testTransport 无法保证不绕过代理，应报错）；合法时成功。
func TestNewValidatesConfigProxy(t *testing.T) {
	base := Config{Token: "oauth-a", User: "alice", APIKey: testAPIKey, CacheDir: t.TempDir()}

	invalid := base
	invalid.Proxy = "http://u:" + proxyTestSecret + "@127.0.0.1:1080"
	if _, err := New(invalid); err == nil || !strings.Contains(err.Error(), "代理地址无效") || strings.Contains(err.Error(), proxyTestSecret) {
		t.Fatalf("New(非法代理) 错误 = %v, want 代理地址无效且不含密码", err)
	}

	custom := base
	custom.Proxy = "socks5://127.0.0.1:1080"
	custom.Client = testClient(t, func(http.ResponseWriter, *http.Request) {})
	if _, err := New(custom); err == nil {
		t.Fatal("配置了代理而 Client 的 Transport 不是 *http.Transport 时 New 必须报错")
	}

	valid := base
	valid.Proxy = "socks5://127.0.0.1:1080"
	if _, err := New(valid); err != nil {
		t.Fatalf("New(合法代理, 默认客户端) error = %v", err)
	}
}

// TestNewPoolRejectsInvalidAccountProxy 池的构造入口（NewPool 与 NewManagedPool）拒绝非法代理，错误信息不含密码。
func TestNewPoolRejectsInvalidAccountProxy(t *testing.T) {
	bad := "http://u:" + proxyTestSecret + "@127.0.0.1:1080"

	_, err := NewPool([]Config{{Token: "oauth-a", User: "alice", APIKey: testAPIKey, CacheDir: t.TempDir(), Proxy: bad}})
	if err == nil || !strings.Contains(err.Error(), "代理地址无效") || strings.Contains(err.Error(), proxyTestSecret) {
		t.Fatalf("NewPool 错误 = %v, want 代理地址无效且不含密码", err)
	}

	_, err = NewManagedPool(PoolConfig{APIKey: testAPIKey, ManagementKey: mgmtKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir()},
		[]PoolAccount{{Token: "oauth-a", User: "alice", Proxy: bad}})
	if err == nil || !strings.Contains(err.Error(), "代理地址无效") || strings.Contains(err.Error(), proxyTestSecret) {
		t.Fatalf("NewManagedPool 错误 = %v, want 代理地址无效且不含密码", err)
	}
}
