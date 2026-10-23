package copilot

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// consoleTokenSig 按规格计算控制台令牌签名：HMAC-SHA256（密钥为 key，消息为 "ghc2api-console-token:v1.<exp>"）
// 的 base64.RawURLEncoding。测试用它核对 issueConsoleToken 的输出格式，也用于构造边界用例的令牌。
func consoleTokenSig(key string, exp int64) string {
	message := "ghc2api-console-token:v1." + strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// forgeConsoleToken 用给定密钥与过期时间拼出一枚控制台令牌，用于构造防御性边界用例。
func forgeConsoleToken(key string, exp int64) string {
	return "v1." + strconv.FormatInt(exp, 10) + "." + consoleTokenSig(key, exp)
}

// consoleTokenFor 用给定管理密钥签发一枚当前有效的控制台令牌，供直接构造请求的测试放入 Authorization 头。
// 除 POST /v0/management/session 外，管理接口只接受控制台令牌，不接受管理密钥。
func consoleTokenFor(key string) string {
	token, _ := issueConsoleToken(key, time.Now())
	return token
}

// mgmtConsoleSession 是 POST /v0/management/session 的响应：控制台令牌与过期时间。
type mgmtConsoleSession struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// mgmtCreateSession 调用 POST /v0/management/session 并解析响应；调用方自行断言状态码。
// key 原样发送（管理密钥即登录换令牌，有效令牌即续期），因此用 mgmtCallRaw，不经过 mgmtCall 的密钥替换。
func mgmtCreateSession(t *testing.T, p *Pool, key string) (int, mgmtConsoleSession) {
	t.Helper()
	w := mgmtCallRaw(p, "POST", "/v0/management/session", "", key)
	var res mgmtConsoleSession
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("session 响应不是合法 JSON: %v; body=%s", err, w.Body.String())
		}
	}
	return w.Code, res
}

// mgmtSessionToken 用 key（管理密钥或有效令牌）换取控制台令牌；非 200 时终止测试。
func mgmtSessionToken(t *testing.T, p *Pool, key string) string {
	t.Helper()
	code, res := mgmtCreateSession(t, p, key)
	if code != http.StatusOK {
		t.Fatalf("POST session status = %d, want 200", code)
	}
	return res.Token
}

// mgmtConsoleExpiresAt 解析响应中的 expires_at（RFC3339）。
func mgmtConsoleExpiresAt(t *testing.T, res mgmtConsoleSession) time.Time {
	t.Helper()
	expiresAt, err := time.Parse(time.RFC3339, res.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at 不是合法的 RFC3339 时间: %q, %v", res.ExpiresAt, err)
	}
	return expiresAt
}

// mgmtAssertFreshExpiry 断言过期时间约为请求时刻 + 14 天（允许分钟级误差，覆盖请求耗时）。
func mgmtAssertFreshExpiry(t *testing.T, expiresAt, before, after time.Time) {
	t.Helper()
	if min := before.Add(consoleTokenTTL - time.Minute); expiresAt.Before(min) {
		t.Fatalf("expires_at = %s, 不应早于 %s", expiresAt, min)
	}
	if max := after.Add(consoleTokenTTL + time.Minute); expiresAt.After(max) {
		t.Fatalf("expires_at = %s, 不应晚于 %s", expiresAt, max)
	}
}

// TestConsoleTokenTTLIsFourteenDays 控制台令牌有效期为 14 天。
func TestConsoleTokenTTLIsFourteenDays(t *testing.T) {
	if consoleTokenTTL != 14*24*time.Hour {
		t.Fatalf("consoleTokenTTL = %v, want 14 天", consoleTokenTTL)
	}
}

// TestIssueConsoleTokenFormatAndExpiry 签发的令牌为 v1.<过期 Unix 秒十进制>.<签名>；
// 过期时间为 now 截断到秒后加 consoleTokenTTL（以 Unix 秒表示），签名为 HMAC-SHA256 的 base64.RawURLEncoding，
// 消息为 "ghc2api-console-token:v1.<exp>"；令牌不含管理密钥明文。
func TestIssueConsoleTokenFormatAndExpiry(t *testing.T) {
	key := "mgmt-secret-key"
	now := time.Date(2026, 10, 23, 12, 34, 56, 789_000_000, time.UTC)
	token, expiresAt := issueConsoleToken(key, now)
	wantExp := now.Truncate(time.Second).Add(consoleTokenTTL)
	if !expiresAt.Equal(wantExp) {
		t.Fatalf("expiresAt = %v, want %v", expiresAt, wantExp)
	}
	if expiresAt.UnixNano()%int64(time.Second) != 0 {
		t.Fatalf("expiresAt 应以 Unix 秒表示、不含亚秒部分: %v", expiresAt)
	}
	expStr := strconv.FormatInt(wantExp.Unix(), 10)
	want := "v1." + expStr + "." + consoleTokenSig(key, wantExp.Unix())
	if token != want {
		t.Fatalf("token = %q, want %q", token, want)
	}
	if strings.Contains(token, key) {
		t.Fatalf("令牌包含管理密钥明文: %q", token)
	}
}

// TestVerifyConsoleTokenAcceptsUnexpiredToken 正常签发、未过期的令牌校验通过。
func TestVerifyConsoleTokenAcceptsUnexpiredToken(t *testing.T) {
	key := "mgmt-secret-key"
	now := time.Unix(1761210000, 0).UTC()
	token, _ := issueConsoleToken(key, now)
	if !verifyConsoleToken(key, token, now) {
		t.Fatal("签发后立即校验应通过")
	}
}

// TestVerifyConsoleTokenExpiryBoundary 恰好在过期时刻校验失败（now 不早于 exp 即过期），过期前 1 秒通过。
func TestVerifyConsoleTokenExpiryBoundary(t *testing.T) {
	key := "mgmt-secret-key"
	now := time.Unix(1761210000, 0).UTC()
	token, expiresAt := issueConsoleToken(key, now)
	if verifyConsoleToken(key, token, expiresAt) {
		t.Fatal("恰好在过期时刻应返回 false")
	}
	if !verifyConsoleToken(key, token, expiresAt.Add(-time.Second)) {
		t.Fatal("过期前 1 秒应返回 true")
	}
	if verifyConsoleToken(key, token, expiresAt.Add(time.Second)) {
		t.Fatal("过期后应返回 false")
	}
}

// TestVerifyConsoleTokenRejectsInvalidTokens 密钥为空、格式不符、签名不符、已过期的令牌都校验失败。
func TestVerifyConsoleTokenRejectsInvalidTokens(t *testing.T) {
	key := "mgmt-secret-key"
	now := time.Unix(1761210000, 0).UTC()
	valid, expiresAt := issueConsoleToken(key, now)
	parts := strings.Split(valid, ".")
	if len(parts) != 3 {
		t.Fatalf("签发的令牌段数 = %d, want 3: %q", len(parts), valid)
	}
	otherKeyToken, _ := issueConsoleToken("another-key", now)
	expired, _ := issueConsoleToken(key, now.Add(-consoleTokenTTL-time.Second))
	cases := []struct {
		name  string
		key   string
		token string
	}{
		{name: "empty-key", key: "", token: valid},
		{name: "empty-token", key: key, token: ""},
		{name: "too-few-segments", key: key, token: "v1." + parts[1]},
		{name: "too-many-segments", key: key, token: valid + ".extra"},
		{name: "empty-exp", key: key, token: "v1.." + parts[2]},
		{name: "non-numeric-exp", key: key, token: "v1.abc." + parts[2]},
		{name: "hex-exp", key: key, token: "v1.0x1f." + parts[2]},
		{name: "wrong-version", key: key, token: "v0." + parts[1] + "." + parts[2]},
		{name: "capital-version", key: key, token: "V1." + parts[1] + "." + parts[2]},
		{name: "tampered-exp", key: key, token: "v1." + strconv.FormatInt(expiresAt.Unix()+1, 10) + "." + parts[2]},
		{name: "tampered-signature", key: key, token: parts[0] + "." + parts[1] + ".AAAA"},
		{name: "signed-with-another-key", key: key, token: otherKeyToken},
		{name: "expired", key: key, token: expired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if verifyConsoleToken(tc.key, tc.token, now) {
				t.Fatalf("verifyConsoleToken(%q, %q, %v) = true, want false", tc.key, tc.token, now)
			}
		})
	}
}

// TestVerifyConsoleTokenRejectsFarFutureExpiry 防御性检查：签名正确但 exp 晚于 now + consoleTokenTTL 的令牌校验失败；
// exp 恰好等于 now + consoleTokenTTL 时仍合法（只拒绝“晚于”）。
func TestVerifyConsoleTokenRejectsFarFutureExpiry(t *testing.T) {
	key := "mgmt-secret-key"
	now := time.Unix(1761210000, 0).UTC()
	atLimit := forgeConsoleToken(key, now.Add(consoleTokenTTL).Unix())
	if !verifyConsoleToken(key, atLimit, now) {
		t.Fatal("exp = now + consoleTokenTTL 应通过")
	}
	beyond := forgeConsoleToken(key, now.Add(consoleTokenTTL).Unix()+1)
	if verifyConsoleToken(key, beyond, now) {
		t.Fatal("exp 晚于 now + consoleTokenTTL 应返回 false")
	}
	// 用未来时刻签发的令牌在“现在”校验：签名正确但 exp 过远，同样拒绝。
	future, _ := issueConsoleToken(key, now.Add(2*time.Second))
	if verifyConsoleToken(key, future, now) {
		t.Fatal("exp 过远的令牌应返回 false")
	}
}

// TestConsoleTokenSessionIssuesFreshToken 用管理密钥调用 POST /v0/management/session 返回 200 与
// {token, expires_at}；token 为有效控制台令牌且不含管理密钥明文，expires_at 约为当前时间 + 14 天（RFC3339）。
func TestConsoleTokenSessionIssuesFreshToken(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	code, res := mgmtCreateSession(t, p, mgmtKey)
	after := time.Now()
	if code != http.StatusOK {
		t.Fatalf("POST session status = %d, want 200", code)
	}
	if res.Token == "" {
		t.Fatal("响应缺少 token")
	}
	mgmtAssertFreshExpiry(t, mgmtConsoleExpiresAt(t, res), before, after)
	if !verifyConsoleToken(mgmtKey, res.Token, time.Now()) {
		t.Fatalf("签发的令牌校验失败: %q", res.Token)
	}
	if strings.Contains(res.Token, mgmtKey) {
		t.Fatalf("令牌包含管理密钥明文: %q", res.Token)
	}
}

// TestConsoleTokenSessionRenewsSliding 用有效令牌调用 session 接口即为滑动续期：
// 返回新令牌与新的过期时间（约为当前 + 14 天），续期后的令牌仍可访问其他管理接口。
func TestConsoleTokenSessionRenewsSliding(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := mgmtSessionToken(t, p, mgmtKey)
	before := time.Now()
	code, second := mgmtCreateSession(t, p, first)
	after := time.Now()
	if code != http.StatusOK {
		t.Fatalf("用令牌续期 status = %d, want 200", code)
	}
	mgmtAssertFreshExpiry(t, mgmtConsoleExpiresAt(t, second), before, after)
	if !verifyConsoleToken(mgmtKey, second.Token, time.Now()) {
		t.Fatalf("续期后的令牌校验失败: %q", second.Token)
	}
	if w := mgmtCall(p, "GET", "/v0/management/accounts", "", second.Token); w.Code != http.StatusOK {
		t.Fatalf("用续期后的令牌访问管理接口 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestConsoleTokenSessionRejectsExpiredAndForgedTokens 过期、用其他密钥签发、篡改与伪造的令牌
// 调用 session 接口都返回 401；这些令牌访问其他管理接口同样返回 401。
func TestConsoleTokenSessionRejectsExpiredAndForgedTokens(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(mgmtSessionToken(t, p, mgmtKey), ".")
	expired, _ := issueConsoleToken(mgmtKey, time.Now().Add(-consoleTokenTTL-time.Hour))
	forged, _ := issueConsoleToken("another-key", time.Now())
	cases := []struct {
		name  string
		token string
	}{
		{name: "expired", token: expired},
		{name: "signed-with-another-key", token: forged},
		{name: "tampered-exp", token: "v1." + parts[1] + "0." + parts[2]},
		{name: "tampered-signature", token: parts[0] + "." + parts[1] + ".AAAA"},
		{name: "garbage", token: "not-a-token"},
	}
	for _, tc := range cases {
		t.Run("session-"+tc.name, func(t *testing.T) {
			if code, _ := mgmtCreateSession(t, p, tc.token); code != http.StatusUnauthorized {
				t.Fatalf("伪造令牌调用 session status = %d, want 401", code)
			}
		})
		t.Run("accounts-"+tc.name, func(t *testing.T) {
			if w := mgmtCall(p, "GET", "/v0/management/accounts", "", tc.token); w.Code != http.StatusUnauthorized {
				t.Fatalf("伪造令牌访问管理接口 status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestConsoleTokenSessionRequiresBearerAuth session 接口需要管理密钥或有效令牌：
// 错误密钥、缺失、服务访问密钥都返回 401；密钥与令牌都只接受 Authorization: Bearer，不接受 X-Api-Key。
func TestConsoleTokenSessionRequiresBearerAuth(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "wrong-key", key: "wrong"},
		{name: "missing-key", key: ""},
		{name: "api-key-is-not-management-key", key: mgmtAPIKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := mgmtCreateSession(t, p, tc.key); code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", code)
			}
		})
	}
	// X-Api-Key 携带管理密钥或有效令牌都不被接受。
	token := mgmtSessionToken(t, p, mgmtKey)
	for _, value := range []string{mgmtKey, token} {
		r := httptest.NewRequest("POST", "/v0/management/session", nil)
		r.Header.Set("X-Api-Key", value)
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("X-Api-Key 携带 %q status = %d, want 401", value, w.Code)
		}
	}
}

// TestConsoleTokenGrantsManagementButNotBusinessAPIs 控制台令牌可访问管理接口与续期，
// 但不能访问 api_key 保护的业务接口，也不能当作 X-Api-Key 使用。
func TestConsoleTokenGrantsManagementButNotBusinessAPIs(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := mgmtSessionToken(t, p, mgmtKey)

	// 管理接口：Bearer 令牌放行。
	for _, req := range []struct{ method, path, body string }{
		{"GET", "/v0/management/accounts", ""},
		{"GET", "/v0/management/version", ""},
		{"POST", "/v0/management/session", ""},
	} {
		if w := mgmtCall(p, req.method, req.path, req.body, token); w.Code != http.StatusOK {
			t.Fatalf("%s %s 用令牌 status = %d, want 200; body=%s", req.method, req.path, w.Code, w.Body.String())
		}
	}

	// 业务接口：令牌不是服务访问密钥。
	for _, req := range []struct{ method, path, body string }{
		{"GET", "/v1/models", ""},
		{"POST", "/v1/chat/completions", `{"model":"x","messages":[]}`},
	} {
		if w := mgmtCall(p, req.method, req.path, req.body, token); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 用令牌 status = %d, want 401; body=%s", req.method, req.path, w.Code, w.Body.String())
		}
	}

	// 令牌不能当作 X-Api-Key：业务与管理接口都拒绝。
	for _, path := range []string{"/v1/models", "/v0/management/accounts"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Api-Key", token)
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("X-Api-Key 携带令牌访问 %s status = %d, want 401", path, w.Code)
		}
	}
}

// TestManagementKeyOnlyExchangesSession 管理密钥只在 POST /v0/management/session 换取令牌时被接受；
// 其他管理接口（version、accounts、GET session、需路径清理的路径等）只接受有效控制台令牌，
// 携带管理密钥返回 401 且消息为“控制台令牌无效或缺失”。
func TestManagementKeyOnlyExchangesSession(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, method, path string }{
		{name: "version", method: "GET", path: "/v0/management/version"},
		{name: "accounts", method: "GET", path: "/v0/management/accounts"},
		{name: "import-account", method: "POST", path: "/v0/management/accounts"},
		{name: "get-session", method: "GET", path: "/v0/management/session"},
		{name: "cleaned-get-session", method: "GET", path: "/v0/management//session"},
		{name: "cleaned-post-session", method: "POST", path: "/v0/management//session"},
	} {
		t.Run("management-key-"+tc.name, func(t *testing.T) {
			w := mgmtCallRaw(p, tc.method, tc.path, "", mgmtKey)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			if msg := mgmtErrorMessage(t, w); msg != "控制台令牌无效或缺失" {
				t.Fatalf("error.message = %q, want %q", msg, "控制台令牌无效或缺失")
			}
		})
	}
	// 对照：管理密钥在 POST /v0/management/session 被接受（换取令牌的唯一入口）。
	if code, _ := mgmtCreateSession(t, p, mgmtKey); code != http.StatusOK {
		t.Fatalf("管理密钥 POST session status = %d, want 200", code)
	}
}

// TestManagementAuthMethodAndEscapedPathEdges 鉴权边缘情况：session 入口按“方法精确等于 POST 且路径解码后等于
// /v0/management/session”识别。小写方法 post 不构成 session 兑换，携带管理密钥返回 401；
// HEAD /v0/management/version 携带管理密钥返回 401，携带有效令牌通过鉴权（应答由路由决定，不得 401）；
// POST /v0/management/sess%69on（百分号编码，解码后等于 session 路径）按 session 入口处理，
// 携带管理密钥换取令牌返回 200（当前实现行为，予以固定）。
func TestManagementAuthMethodAndEscapedPathEdges(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 小写方法 post：方法比较区分大小写，不是 session 兑换入口，管理密钥被拒。
	if w := mgmtCallRaw(p, "post", "/v0/management/session", "", mgmtKey); w.Code != http.StatusUnauthorized {
		t.Fatalf("小写 post 携带管理密钥 status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	// HEAD version：非 session 路径，管理密钥被拒。
	if w := mgmtCallRaw(p, http.MethodHead, "/v0/management/version", "", mgmtKey); w.Code != http.StatusUnauthorized {
		t.Fatalf("HEAD version 携带管理密钥 status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	// HEAD version 用有效令牌通过鉴权，具体应答由路由决定（200 或 405/404 均可），但不得 401。
	token := mgmtSessionToken(t, p, mgmtKey)
	if w := mgmtCallRaw(p, http.MethodHead, "/v0/management/version", "", token); w.Code == http.StatusUnauthorized {
		t.Fatalf("HEAD version 携带令牌 status = 401, want 非 401; body=%s", w.Body.String())
	}
	// 百分号编码的 session 路径：按 session 入口处理，管理密钥换取令牌返回 200 并签发有效令牌。
	w := mgmtCallRaw(p, http.MethodPost, "/v0/management/sess%69on", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /v0/management/sess%%69on 携带管理密钥 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var res mgmtConsoleSession
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || !verifyConsoleToken(mgmtKey, res.Token, time.Now()) {
		t.Fatalf("百分号编码路径应路由到 session handler 并签发有效令牌: %s", w.Body.String())
	}
}

// TestConsoleTokenAccessesManagementRoutes 有效控制台令牌可访问各管理接口：
// version、accounts、POST session（续期）返回 200；GET session 与需路径清理的路径通过鉴权后
// 由 ServeMux 应答（方法未注册的 405/404 与路径清理重定向），都不得被鉴权拒绝为 401。
func TestConsoleTokenAccessesManagementRoutes(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := mgmtSessionToken(t, p, mgmtKey)
	for _, tc := range []struct{ method, path string }{
		{method: "GET", path: "/v0/management/version"},
		{method: "GET", path: "/v0/management/accounts"},
		{method: "POST", path: "/v0/management/session"},
	} {
		if w := mgmtCallRaw(p, tc.method, tc.path, "", token); w.Code != http.StatusOK {
			t.Fatalf("%s %s 用令牌 status = %d, want 200; body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	// GET session：路由只注册了 POST，令牌通过鉴权后由 ServeMux 应答（405，或 404）。
	if w := mgmtCallRaw(p, "GET", "/v0/management/session", "", token); !slices.Contains([]int{http.StatusMethodNotAllowed, http.StatusNotFound}, w.Code) {
		t.Fatalf("GET session 用令牌 status = %d, want 405 或 404（应通过鉴权）; body=%s", w.Code, w.Body.String())
	}
	// 需路径清理的路径：令牌通过鉴权后由 ServeMux 重定向到清理后的路径。
	redirects := []int{http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}
	if w := mgmtCallRaw(p, "GET", "/v0/management//session", "", token); !slices.Contains(redirects, w.Code) {
		t.Fatalf("GET /v0/management//session 用令牌 status = %d, want 301/307/308; body=%s", w.Code, w.Body.String())
	}
	if w := mgmtCallRaw(p, "POST", "/v0/management//session", "", token); !slices.Contains(redirects, w.Code) {
		t.Fatalf("POST /v0/management//session 用令牌 status = %d, want 301/307/308; body=%s", w.Code, w.Body.String())
	}
}

// TestManagementAuthFailureMessages 鉴权失败的 401 消息按请求区分：POST /v0/management/session
// （换取令牌的唯一入口）失败时为“管理密钥无效或缺失”；其他管理路径失败时为“控制台令牌无效或缺失”。
func TestManagementAuthFailureMessages(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	expired, _ := issueConsoleToken(mgmtKey, time.Now().Add(-consoleTokenTTL-time.Hour))
	const (
		msgSessionAuthFail = "管理密钥无效或缺失"
		msgTokenAuthFail   = "控制台令牌无效或缺失"
	)
	for _, tc := range []struct{ name, method, path, key, want string }{
		{name: "session-wrong-key", method: "POST", path: "/v0/management/session", key: "wrong", want: msgSessionAuthFail},
		{name: "session-missing-key", method: "POST", path: "/v0/management/session", key: "", want: msgSessionAuthFail},
		{name: "session-expired-token", method: "POST", path: "/v0/management/session", key: expired, want: msgSessionAuthFail},
		{name: "accounts-wrong-key", method: "GET", path: "/v0/management/accounts", key: "wrong", want: msgTokenAuthFail},
		{name: "accounts-management-key", method: "GET", path: "/v0/management/accounts", key: mgmtKey, want: msgTokenAuthFail},
		{name: "accounts-expired-token", method: "GET", path: "/v0/management/accounts", key: expired, want: msgTokenAuthFail},
		{name: "version-management-key", method: "GET", path: "/v0/management/version", key: mgmtKey, want: msgTokenAuthFail},
		{name: "version-api-key", method: "GET", path: "/v0/management/version", key: mgmtAPIKey, want: msgTokenAuthFail},
		{name: "get-session-wrong-key", method: "GET", path: "/v0/management/session", key: "wrong", want: msgTokenAuthFail},
		{name: "cleaned-path-management-key", method: "POST", path: "/v0/management//session", key: mgmtKey, want: msgTokenAuthFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := mgmtCallRaw(p, tc.method, tc.path, "", tc.key)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			if msg := mgmtErrorMessage(t, w); msg != tc.want {
				t.Fatalf("error.message = %q, want %q", msg, tc.want)
			}
		})
	}
}

// TestManagementKeyRejectionWarnsWithoutRequestLogs 携带管理密钥访问非 session 管理接口被拒时，
// 与其他鉴权失败行为一致：只输出一条“管理接口鉴权失败”告警，不输出请求日志。
func TestManagementKeyRejectionWarnsWithoutRequestLogs(t *testing.T) {
	var logs bytes.Buffer
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
		Logger: securityJSONLogger(&logs),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 用 securityRequest 构造请求：RemoteAddr 固定为 securityRemoteAddr，告警字段断言依赖它。
	tc := securityLogCase{
		method: "GET", path: "/v0/management/accounts", auth: "Bearer " + mgmtKey,
		wantStatus: http.StatusUnauthorized, wantAuthWarning: msgMgmtAuthFail,
	}
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, securityRequest(tc))
	if w.Code != tc.wantStatus {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	assertSecurityLogs(t, logs.Bytes(), tc)
}
