package copilot

// 本文件覆盖鉴权与请求日志的关系：业务请求或管理接口鉴权失败（401）时只输出鉴权失败告警，
// 不输出"收到客户端请求"与"客户端请求处理完成"；其余请求（鉴权通过、免鉴权路径、ServeMux 直接产生的管理路径响应）
// 都输出完整的请求日志。

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	securityAPIKey        = "security-local-api-key"  // 服务访问密钥
	securityMgmtKey       = "security-management-key" // 管理密钥
	securityMaxFieldBytes = 256                       // 鉴权失败告警中 path、method 字段的最大字节数
	securityRemoteAddr    = "203.0.113.7:54321"       // 测试请求的 RemoteAddr，告警中的 remote_addr 必须等于它

	msgRequestReceived = "收到客户端请求"
	msgRequestDone     = "客户端请求处理完成"
	msgClientAuthFail  = "客户端鉴权失败"
	msgMgmtAuthFail    = "管理接口鉴权失败"
)

// securityLogCase 是一次请求及其期望的响应与日志行为。
type securityLogCase struct {
	name         string
	method, path string
	auth         string // Authorization 头原文，空表示不带该头
	wantStatus   int
	// wantAuthWarning 非空时必须恰好输出一条该消息的 WARN 告警，且 path、method 字段为请求值截断后的值，
	// remote_addr 为 securityRemoteAddr；为空时不应有任何鉴权告警。
	wantAuthWarning string
	// wantRequestLogs 为 true 时必须同时输出"收到客户端请求"与"客户端请求处理完成"，为 false 时两者都不能出现。
	wantRequestLogs bool
}

// securityJSONLogger 返回把 JSON 日志行写入 buf 的日志器，级别为 Info（包含 Warn）。
func securityJSONLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// newSecurityService 返回以 apiKey 构造的单账号 Service 的 HTTP 入口（不访问上游）。
func newSecurityService(apiKey string) func(*testing.T, *slog.Logger) http.Handler {
	return func(t *testing.T, logger *slog.Logger) http.Handler {
		t.Helper()
		return testService(t, Config{Token: "github-secret", APIKey: apiKey, Logger: logger}).Handler()
	}
}

// newSecurityPool 返回以 apiKey 与 securityMgmtKey 构造的托管池的 HTTP 入口（没有账号，不访问上游）。
func newSecurityPool(apiKey string) func(*testing.T, *slog.Logger) http.Handler {
	return func(t *testing.T, logger *slog.Logger) http.Handler {
		t.Helper()
		p, err := NewManagedPool(PoolConfig{
			APIKey: apiKey, ManagementKey: securityMgmtKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
			Logger: logger,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return p.Handler()
	}
}

// securityRequest 按用例构造请求，RemoteAddr 固定为 securityRemoteAddr。
func securityRequest(tc securityLogCase) *http.Request {
	r := httptest.NewRequest(tc.method, tc.path, nil)
	r.RemoteAddr = securityRemoteAddr
	if tc.auth != "" {
		r.Header.Set("Authorization", tc.auth)
	}
	return r
}

// runSecurityLogCases 对每个用例使用新构造的入口发送请求，检查状态码、响应正文与日志是否符合用例的期望。
func runSecurityLogCases(t *testing.T, newHandler func(*testing.T, *slog.Logger) http.Handler, cases []securityLogCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			handler := newHandler(t, securityJSONLogger(&logs))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, securityRequest(tc))
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			assertSecurityLogs(t, logs.Bytes(), tc)
		})
	}
}

// truncateSecurityField 返回告警字段的期望值：超过 securityMaxFieldBytes 字节时只保留前面的字节。
// 按字节截断，仅适用于 ASCII 输入；多字节路径见 TestUnauthorizedWarnKeepsUTF8PathPrefix。
func truncateSecurityField(value string) string {
	if len(value) > securityMaxFieldBytes {
		return value[:securityMaxFieldBytes]
	}
	return value
}

// assertSecurityLogs 检查请求日志的成对出现情况与鉴权告警的字段内容。
func assertSecurityLogs(t *testing.T, output []byte, tc securityLogCase) {
	t.Helper()
	received, done := false, false
	var authWarnings []map[string]any
	for _, entry := range jsonLogEntries(t, output) {
		switch entry["msg"] {
		case msgRequestReceived:
			received = true
		case msgRequestDone:
			done = true
		case msgClientAuthFail, msgMgmtAuthFail:
			authWarnings = append(authWarnings, entry)
		}
	}
	if received != tc.wantRequestLogs || done != tc.wantRequestLogs {
		t.Errorf("请求日志：收到=%v 完成=%v, want 均为 %v; logs=%s", received, done, tc.wantRequestLogs, output)
	}
	if tc.wantAuthWarning == "" {
		if len(authWarnings) != 0 {
			t.Errorf("不应输出鉴权告警: %v", authWarnings)
		}
		return
	}
	if len(authWarnings) != 1 || authWarnings[0]["msg"] != tc.wantAuthWarning || authWarnings[0]["level"] != "WARN" {
		t.Fatalf("鉴权告警 = %v, want 恰好一条 WARN %q", authWarnings, tc.wantAuthWarning)
	}
	fields := map[string]string{
		"path":        truncateSecurityField(tc.path),
		"method":      truncateSecurityField(tc.method),
		"remote_addr": securityRemoteAddr,
	}
	for key, want := range fields {
		if got := authWarnings[0][key]; got != want {
			t.Errorf("鉴权告警的 %s = %q, want %q", key, got, want)
		}
	}
}

// TestServiceRequestLogsFollowAuthorization 单账号 Service：api_key 鉴权失败时不输出请求日志，免鉴权路径与鉴权通过的请求保留请求日志。
func TestServiceRequestLogsFollowAuthorization(t *testing.T) {
	runSecurityLogCases(t, newSecurityService(securityAPIKey), []securityLogCase{
		{name: "missing-key-warns-without-request-logs", method: "GET", path: "/v1/models",
			wantStatus: http.StatusUnauthorized, wantAuthWarning: msgClientAuthFail},
		{name: "wrong-key-warns-without-request-logs", method: "POST", path: "/v1/chat/completions",
			auth: "Bearer wrong-key", wantStatus: http.StatusUnauthorized, wantAuthWarning: msgClientAuthFail},
		{name: "authorized-request-keeps-request-logs", method: "GET", path: "/no-such-route",
			auth: "Bearer " + securityAPIKey, wantStatus: http.StatusNotFound, wantRequestLogs: true},
		{name: "healthz-without-key-keeps-request-logs", method: "GET", path: "/healthz",
			wantStatus: http.StatusOK, wantRequestLogs: true},
		{name: "favicon-without-key-keeps-request-logs", method: "GET", path: "/favicon.ico",
			wantStatus: http.StatusNotFound, wantRequestLogs: true},
	})
}

// TestPoolRequestLogsFollowAuthorization 托管池：业务请求与管理接口鉴权失败时都不输出请求日志；
// 控制台页面是免鉴权路径，仍保留请求日志。管理接口只接受控制台令牌（POST /v0/management/session 除外），
// 携带管理密钥访问非 session 管理接口（含需路径清理的路径）属于鉴权失败。
func TestPoolRequestLogsFollowAuthorization(t *testing.T) {
	runSecurityLogCases(t, newSecurityPool(securityAPIKey), []securityLogCase{
		{name: "missing-key-warns-without-request-logs", method: "GET", path: "/v1/models",
			wantStatus: http.StatusUnauthorized, wantAuthWarning: msgClientAuthFail},
		{name: "wrong-key-warns-without-request-logs", method: "POST", path: "/v1/chat/completions",
			auth: "Bearer wrong-key", wantStatus: http.StatusUnauthorized, wantAuthWarning: msgClientAuthFail},
		{name: "authorized-request-keeps-request-logs", method: "GET", path: "/no-such-route",
			auth: "Bearer " + securityAPIKey, wantStatus: http.StatusNotFound, wantRequestLogs: true},
		{name: "healthz-without-key-keeps-request-logs", method: "GET", path: "/healthz",
			wantStatus: http.StatusOK, wantRequestLogs: true},
		{name: "favicon-without-key-keeps-request-logs", method: "GET", path: "/favicon.ico",
			wantStatus: http.StatusNotFound, wantRequestLogs: true},
		{name: "console-without-key-keeps-request-logs", method: "GET", path: "/management",
			wantStatus: http.StatusOK, wantRequestLogs: true},
		{name: "management-missing-key-warns-without-request-logs", method: "GET", path: "/v0/management/accounts",
			wantStatus: http.StatusUnauthorized, wantAuthWarning: msgMgmtAuthFail},
		{name: "management-with-api-key-warns-without-request-logs", method: "GET", path: "/v0/management/accounts",
			auth: "Bearer " + securityAPIKey, wantStatus: http.StatusUnauthorized, wantAuthWarning: msgMgmtAuthFail},
		{name: "management-with-mgmt-key-warns-without-request-logs", method: "GET", path: "/v0/management/accounts",
			auth: "Bearer " + securityMgmtKey, wantStatus: http.StatusUnauthorized, wantAuthWarning: msgMgmtAuthFail},
		{name: "management-mgmt-key-on-cleaned-path-warns-without-request-logs", method: "GET", path: "/v0/management//accounts",
			auth: "Bearer " + securityMgmtKey, wantStatus: http.StatusUnauthorized, wantAuthWarning: msgMgmtAuthFail},
		{name: "management-authorized-keeps-request-logs", method: "GET", path: "/v0/management/accounts",
			auth: "Bearer " + consoleTokenFor(securityMgmtKey), wantStatus: http.StatusOK, wantRequestLogs: true},
	})
}

// TestManagementMuxResponsesKeepRequestLogs 管理路径上由 ServeMux 直接产生的响应不经过鉴权（没有进入管理密钥校验），
// 因此不是鉴权失败，必须输出请求日志，且不输出鉴权告警。覆盖方法不匹配（405，ServeMux 也可能给 404）与路径清理重定向。
// 路径清理重定向要求先通过鉴权，因此用控制台令牌（管理密钥在非 session 路径上是鉴权失败，见上一测试）。
func TestManagementMuxResponsesKeepRequestLogs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		method, path string
		auth         string
		// wantStatuses 是允许的状态码：405 与 404 均可接受；路径清理重定向的状态码随 Go 版本变化（301/307/308）。
		wantStatuses []int
	}{
		{name: "post-console-without-key", method: "POST", path: "/management",
			wantStatuses: []int{http.StatusMethodNotAllowed, http.StatusNotFound}},
		{name: "cleaned-path-redirect-with-console-token", method: "GET", path: "/v0/management//accounts",
			auth: "Bearer " + consoleTokenFor(securityMgmtKey), wantStatuses: []int{http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			handler := newSecurityPool(securityAPIKey)(t, securityJSONLogger(&logs))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, securityRequest(securityLogCase{method: tc.method, path: tc.path, auth: tc.auth}))
			if !slices.Contains(tc.wantStatuses, w.Code) {
				t.Fatalf("status = %d, want one of %v; body=%s", w.Code, tc.wantStatuses, w.Body.String())
			}
			assertSecurityLogs(t, logs.Bytes(), securityLogCase{method: tc.method, path: tc.path, wantRequestLogs: true})
		})
	}
}

// TestEmptyAPIKeyFailsClosed 库层对空服务访问密钥失败即拒绝：构造函数拒绝空（或纯空白）APIKey；
// 即使绕过构造函数，鉴权中间件收到空密钥时也拒绝所有需鉴权的请求，空的 Bearer 或 X-Api-Key 不会因哈希相同而被放行。
func TestEmptyAPIKeyFailsClosed(t *testing.T) {
	t.Run("constructors", func(t *testing.T) {
		for _, key := range []string{"", "  "} {
			if _, err := New(Config{Token: "oauth-a", APIKey: key, CacheDir: t.TempDir()}); err == nil {
				t.Errorf("New(APIKey=%q) 应失败", key)
			}
			if _, err := NewPool([]Config{{Token: "oauth-a", APIKey: key, CacheDir: t.TempDir()}}); err == nil {
				t.Errorf("NewPool(APIKey=%q) 应失败", key)
			}
			config := PoolConfig{APIKey: key, ManagementKey: "mgmt-key", CredentialsDir: t.TempDir(), CacheDir: t.TempDir()}
			if _, err := NewManagedPool(config, nil); err == nil {
				t.Errorf("NewManagedPool(APIKey=%q) 应失败", key)
			}
		}
	})
	noLog := func(context.Context, slog.Level, string, ...any) {}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	headers := []map[string]string{
		{},
		{"Authorization": "Bearer "},
		{"X-Api-Key": ""},
		{"Authorization": "Bearer ", "X-Api-Key": ""},
	}
	t.Run("requireAPIKey", func(t *testing.T) {
		handler := requireAPIKey("", noLog, ok)
		for _, h := range headers {
			r := httptest.NewRequest("GET", "/v1/models", nil)
			for name, value := range h {
				r.Header[name] = []string{value}
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("空密钥、请求头 %v: status = %d, want 401", h, w.Code)
			}
		}
		// 免鉴权路径不受影响。
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
		if w.Code != http.StatusOK {
			t.Errorf("/healthz status = %d, want 200", w.Code)
		}
	})
	t.Run("requireManagementKey", func(t *testing.T) {
		handler := requireManagementKey("", noLog, ok)
		for _, auth := range []string{"", "Bearer "} {
			r := httptest.NewRequest("GET", "/v0/management/accounts", nil)
			if auth != "" {
				r.Header.Set("Authorization", auth)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("空管理密钥、Authorization=%q: status = %d, want 401", auth, w.Code)
			}
		}
	})
}

// TestUnauthorizedWarnTruncatesLongPath 鉴权失败告警中的 path 与 method 字段最多保留前 256 字节，超出部分被截断；
// 业务路径与管理路径都适用。
func TestUnauthorizedWarnTruncatesLongPath(t *testing.T) {
	longBusiness := "/v1/models/" + strings.Repeat("p", 1000)
	longManagement := "/v0/management/" + strings.Repeat("q", 1000)
	longMethod := strings.Repeat("M", 300)
	t.Run("service", func(t *testing.T) {
		runSecurityLogCases(t, newSecurityService(securityAPIKey), []securityLogCase{
			{name: "long-business-path", method: "GET", path: longBusiness,
				wantStatus: http.StatusUnauthorized, wantAuthWarning: msgClientAuthFail},
			{name: "long-method", method: longMethod, path: "/v1/models",
				wantStatus: http.StatusUnauthorized, wantAuthWarning: msgClientAuthFail},
		})
	})
	t.Run("pool", func(t *testing.T) {
		runSecurityLogCases(t, newSecurityPool(securityAPIKey), []securityLogCase{
			{name: "long-business-path", method: "GET", path: longBusiness,
				wantStatus: http.StatusUnauthorized, wantAuthWarning: msgClientAuthFail},
			{name: "long-management-path", method: "GET", path: longManagement,
				wantStatus: http.StatusUnauthorized, wantAuthWarning: msgMgmtAuthFail},
		})
	})
}

// TestUnauthorizedWarnKeepsUTF8PathPrefix 多字节路径超过 256 字节时，截断点回退到字符边界：path 字段不超过 256 字节、
// 是合法 UTF-8、是原路径的前缀，并且保留到距上限不足一个字符的位置（不会因为回退而丢掉过多内容）。
// 每个 é 占 2 字节，因此 256 字节处恰好落在字符中间，需要回退。
func TestUnauthorizedWarnKeepsUTF8PathPrefix(t *testing.T) {
	path := "/" + strings.Repeat("é", 200)
	for _, tc := range []struct {
		name       string
		newHandler func(*testing.T, *slog.Logger) http.Handler
	}{
		{name: "service", newHandler: newSecurityService(securityAPIKey)},
		{name: "pool", newHandler: newSecurityPool(securityAPIKey)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			handler := tc.newHandler(t, securityJSONLogger(&logs))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, securityRequest(securityLogCase{method: "GET", path: path}))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			var warnings []map[string]any
			for _, entry := range jsonLogEntries(t, logs.Bytes()) {
				if entry["msg"] == msgClientAuthFail || entry["msg"] == msgMgmtAuthFail {
					warnings = append(warnings, entry)
				}
			}
			if len(warnings) != 1 {
				t.Fatalf("鉴权告警 %d 条, want 1; logs=%s", len(warnings), logs.String())
			}
			got, _ := warnings[0]["path"].(string)
			if len(got) > securityMaxFieldBytes {
				t.Errorf("path 字段 %d 字节, want 不超过 %d 字节", len(got), securityMaxFieldBytes)
			}
			if !utf8.ValidString(got) {
				t.Errorf("path 字段不是合法 UTF-8: %q", got)
			}
			if !strings.HasPrefix(path, got) {
				t.Errorf("path 字段 %q 不是原路径的前缀", got)
			}
			if len(got) < securityMaxFieldBytes-utf8.UTFMax {
				t.Errorf("path 字段只剩 %d 字节，截断回退过度", len(got))
			}
		})
	}
}
