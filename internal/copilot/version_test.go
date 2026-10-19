package copilot

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// versionPool 创建启用管理接口的托管池（无账号）：管理密钥为 mgmtKey，服务密钥为 mgmtAPIKey。
func versionPool(t *testing.T) *Pool {
	t.Helper()
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestVersionDefaultFormat 源码默认版本形如 vX.Y.Z（只校验格式，发版更新默认值时无需改测试）；发布构建时由 -ldflags -X 覆盖。
func TestVersionDefaultFormat(t *testing.T) {
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("Version = %q, want vX.Y.Z", Version)
	}
}

// TestManagementVersionReturnsVersionVariable 接口返回的是 Version 变量的当前值，而不是写死的常量：
// 测试中临时修改 Version 后请求接口，并在测试结束时还原。
func TestManagementVersionReturnsVersionVariable(t *testing.T) {
	p := versionPool(t)
	original := Version
	t.Cleanup(func() { Version = original })

	const modified = "v9.9.9-test"
	Version = modified
	w := mgmtCall(p, "GET", "/v0/management/version", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v0/management/version status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("版本接口正文不是合法 JSON: %v; body=%s", err, w.Body.String())
	}
	if len(body) != 1 || body["version"] != modified {
		t.Fatalf("正文 = %v, want 仅含 {\"version\": %q}", body, modified)
	}
}

// TestManagementVersionRequiresManagementKey 版本接口与其他管理接口一样需要管理密钥：
// 无密钥、错误密钥或服务 api_key 均返回 401。
func TestManagementVersionRequiresManagementKey(t *testing.T) {
	p := versionPool(t)
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "missing-key", key: ""},
		{name: "wrong-key", key: "wrong"},
		{name: "api-key-is-not-management-key", key: mgmtAPIKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := mgmtCall(p, "GET", "/v0/management/version", "", tc.key); w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestManagementVersionAbsentWithoutManagementKey 未配置管理密钥时版本路由不存在，沿用其他管理接口的行为：
// 携带管理密钥得到 401，携带有效服务密钥只能得到普通 404。
func TestManagementVersionAbsentWithoutManagementKey(t *testing.T) {
	p := testPool(t, poolConfigs(t, nil, "oauth-a"))
	if w := mgmtCall(p, "GET", "/v0/management/version", "", mgmtKey); w.Code != http.StatusUnauthorized {
		t.Errorf("携带管理密钥 status = %d, want 401", w.Code)
	}
	if w := mgmtCall(p, "GET", "/v0/management/version", "", testAPIKey); w.Code != http.StatusNotFound {
		t.Errorf("携带服务密钥 status = %d, want 404", w.Code)
	}
}
