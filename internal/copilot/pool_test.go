package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	poolChatBody       = `{"model":"gpt-4.1","messages":[]}`
	poolTokenPrefix    = "token "
	poolBearerPrefix   = "Bearer session-"
	poolLocalAPIKey    = "pool-local-key"
	poolFailingAccount = 0
	poolRequestsTotal  = 4
)

// poolFailure 是测试上游对某个账号返回的固定失败响应。
type poolFailure struct {
	status int
	body   string
}

// poolUpstream 模拟多个 GitHub 账号共用的上游：按 OAuth token 颁发不同的 session，
// 并记录每个生成请求到达的账号，用于断言账号选择、session 复用和冷却行为。
type poolUpstream struct {
	t *testing.T

	mu                sync.Mutex
	total             int                    // 所有上游请求数，用于断言健康检查和鉴权失败不访问上游
	sessionCalls      map[string]int         // 每个账号的 session 交换次数
	generations       []string               // 按到达顺序记录生成请求所属账号的 OAuth token
	sessionFailures   map[string]poolFailure // 指定账号的 session 交换直接返回该失败
	generationResult  map[string]poolFailure // 指定账号的生成请求返回该状态与正文
	sessionTTL        time.Duration          // 签发的 session 有效期；短于 2 分钟时客户端每次都会重新交换
	autoCalls         map[string]int         // 每个账号访问 Auto 接口（/auto、/models/session）的次数
	autoResult        map[string]poolFailure // 指定账号的 Auto 接口返回该失败
	generationHeaders map[string]http.Header // 指定账号生成请求失败时额外返回的响应头
}

func newPoolUpstream(t *testing.T) *poolUpstream {
	return &poolUpstream{
		t:                 t,
		sessionCalls:      map[string]int{},
		sessionFailures:   map[string]poolFailure{},
		generationResult:  map[string]poolFailure{},
		sessionTTL:        time.Hour,
		autoCalls:         map[string]int{},
		autoResult:        map[string]poolFailure{},
		generationHeaders: map[string]http.Header{},
	}
}

// client 返回共享给所有账号的测试客户端；账号身份通过 Authorization 头区分。
func (u *poolUpstream) client() *http.Client {
	return testClient(u.t, u.serve)
}

func (u *poolUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.total++
	u.mu.Unlock()
	switch r.URL.Path {
	case "/copilot_internal/v2/token":
		account := strings.TrimPrefix(r.Header.Get("Authorization"), poolTokenPrefix)
		u.mu.Lock()
		u.sessionCalls[account]++
		failure, failing := u.sessionFailures[account]
		ttl := u.sessionTTL
		u.mu.Unlock()
		if failing {
			w.WriteHeader(failure.status)
			_, _ = io.WriteString(w, failure.body)
			return
		}
		testJSON(w, map[string]any{
			"token": "session-" + account, "expires_at": time.Now().Add(ttl).Unix(),
			"endpoints": map[string]string{"api": ""},
		})
	case "/copilot_internal/user":
		testJSON(w, map[string]string{"plan": "pool-test"})
	case "/models":
		_, _ = io.WriteString(w, testModels)
	case "/chat/completions", "/responses", "/v1/messages":
		account, ok := strings.CutPrefix(r.Header.Get("Authorization"), poolBearerPrefix)
		if !ok {
			u.t.Errorf("生成请求未使用账号 session 鉴权: %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		u.mu.Lock()
		u.generations = append(u.generations, account)
		failure, failing := u.generationResult[account]
		header := u.generationHeaders[account]
		u.mu.Unlock()
		if failing {
			for key, values := range header {
				w.Header()[key] = values
			}
			w.WriteHeader(failure.status)
			_, _ = io.WriteString(w, failure.body)
			return
		}
		testJSON(w, map[string]string{"served_by": account})
	case "/auto", "/models/session":
		account, ok := strings.CutPrefix(r.Header.Get("Authorization"), poolBearerPrefix)
		if !ok {
			u.t.Errorf("Auto 请求未使用账号 session 鉴权: %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		u.mu.Lock()
		u.autoCalls[account]++
		failure, failing := u.autoResult[account]
		u.mu.Unlock()
		if failing {
			w.WriteHeader(failure.status)
			_, _ = io.WriteString(w, failure.body)
			return
		}
		testJSON(w, map[string]any{
			"session_token": "auto-" + account, "selected_model": "gpt-4.1",
			"expires_at": time.Now().Add(24 * time.Hour).Unix(),
		})
	default:
		u.t.Errorf("意外的上游路径 %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (u *poolUpstream) failSession(account string, failure poolFailure) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.sessionFailures[account] = failure
}

func (u *poolUpstream) failGeneration(account string, failure poolFailure) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.generationResult[account] = failure
}

func (u *poolUpstream) setSessionTTL(ttl time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.sessionTTL = ttl
}

// failGenerationWithHeader 与 failGeneration 相同，但失败响应还会带上 header 中的响应头。
func (u *poolUpstream) failGenerationWithHeader(account string, failure poolFailure, header http.Header) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.generationResult[account] = failure
	u.generationHeaders[account] = header
}

func (u *poolUpstream) failAuto(account string, failure poolFailure) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.autoResult[account] = failure
}

func (u *poolUpstream) autoCount(account string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.autoCalls[account]
}

func (u *poolUpstream) sessionCount(account string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.sessionCalls[account]
}

func (u *poolUpstream) requestCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.total
}

func (u *poolUpstream) generationOrder() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.generations...)
}

func assertGenerationOrder(t *testing.T, u *poolUpstream, want ...string) {
	t.Helper()
	if got := u.generationOrder(); !reflect.DeepEqual(got, want) {
		t.Fatalf("生成请求的账号顺序 = %v, want %v", got, want)
	}
}

// poolConfigs 为每个 OAuth token 生成独立账号配置；每个账号使用独立临时缓存目录，避免磁盘 session 互相影响。
// 账号没有用户名，文件名由下标决定（0.json、1.json）；需要按用户名文件名选择时用 poolUserConfigs。
func poolConfigs(t *testing.T, client *http.Client, tokens ...string) []Config {
	t.Helper()
	configs := make([]Config, 0, len(tokens))
	for _, token := range tokens {
		configs = append(configs, Config{Token: token, CacheDir: t.TempDir(), Client: client})
	}
	return configs
}

// poolUserConfigs 按用户名生成账号配置：凭证文件名为 <user>.json，token 为 "oauth-" + user，
// 账号按传入顺序加入池（即 account 日志下标）。选择顺序由文件名决定，与传入顺序无关。
func poolUserConfigs(t *testing.T, client *http.Client, users ...string) []Config {
	t.Helper()
	configs := make([]Config, 0, len(users))
	for _, user := range users {
		configs = append(configs, Config{Token: "oauth-" + user, User: user, CacheDir: t.TempDir(), Client: client})
	}
	return configs
}

func testPool(t *testing.T, configs []Config) *Pool {
	t.Helper()
	pool, err := NewPool(configs)
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, pool)
	return pool
}

// poolRequest 通过 Pool 的 HTTP 入口发送请求，用法与 testRequest 对 Service 的一致。
func poolRequest(p *Pool, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, r)
	return w
}

// jsonLogEntries 解析捕获的 JSON 日志；非法行直接失败，保证断言的日志结构可信。
func jsonLogEntries(t *testing.T, output []byte) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("invalid JSON log entry %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestNewPoolRejectsInvalidAccounts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		configs []Config
	}{
		{name: "empty", configs: nil},
		{name: "empty-token", configs: []Config{{Token: "oauth-a"}, {Token: "  "}}},
		{name: "invalid-host", configs: []Config{{Token: "oauth-a"}, {Token: "oauth-b", Host: "evil.example"}}},
		{name: "duplicate-token-after-trim", configs: []Config{{Token: "same"}, {Token: " same "}}},
		{name: "duplicate-after-host-normalization", configs: []Config{
			{Token: "same", Host: "github.com"}, {Token: "same", Host: "GitHub.com"},
		}},
		{name: "api-key-mismatch", configs: []Config{{Token: "oauth-a", APIKey: "one"}, {Token: "oauth-b", APIKey: "two"}}},
		{name: "api-key-missing-on-one-account", configs: []Config{{Token: "oauth-a", APIKey: "one"}, {Token: "oauth-b"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPool(tc.configs); err == nil {
				t.Fatalf("NewPool(%d 个账号) 必须拒绝无效配置", len(tc.configs))
			}
		})
	}
}

func TestNewPoolAcceptsDistinctAccountsInConfigOrder(t *testing.T) {
	// 同一 token 在不同 host 上是不同账号，允许共存；APIKey 去除首尾空白后一致即可。
	pool, err := NewPool([]Config{
		{Token: "oauth-a", APIKey: " pool-key ", CacheDir: t.TempDir()},
		{Token: "oauth-b", Host: "acme.ghe.com", APIKey: "pool-key", CacheDir: t.TempDir()},
		{Token: "oauth-a", Host: "acme.ghe.com", APIKey: "pool-key", CacheDir: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("NewPool() error = %v", err)
	}
	if len(pool.accounts) != 3 {
		t.Fatalf("accounts = %d, want 3", len(pool.accounts))
	}
	for i, want := range []string{"oauth-a", "oauth-b", "oauth-a"} {
		if got := pool.accounts[i].service.config.Token; got != want {
			t.Errorf("accounts[%d] token = %q, want %q", i, got, want)
		}
	}
	if got := pool.accounts[1].service.config.Host; got != "acme.ghe.com" {
		t.Errorf("accounts[1] host = %q, want acme.ghe.com", got)
	}
}

func TestServiceCooldownFollowsDeadline(t *testing.T) {
	if AccountCooldown != time.Minute {
		t.Fatalf("AccountCooldown = %s, want 1m", AccountCooldown)
	}
	s := testService(t, Config{Token: "oauth-a"})
	now := time.Now()
	if !s.available(now) {
		t.Fatal("从未冷却的账号必须可用")
	}
	until := now.Add(AccountCooldown)
	s.setCooldown(until)
	if s.available(until.Add(-time.Nanosecond)) {
		t.Fatal("冷却截止时间之前账号不可用")
	}
	if !s.available(until) {
		t.Fatal("冷却截止时刻账号应重新可用")
	}
	if !s.available(until.Add(time.Second)) {
		t.Fatal("冷却截止时间之后账号应可用")
	}
}

// TestPoolHandlerRoundRobinsAccountsByFileName 账号按 b、a、c 加入，但同档内的 SWRR 按凭证文件名轮流：
// 三者权重都为 1，因此请求依次由 a、b、c 处理并循环。每个账号的 session 只交换一次（之后复用）。
func TestPoolHandlerRoundRobinsAccountsByFileName(t *testing.T) {
	upstream := newPoolUpstream(t)
	p := testPool(t, poolUserConfigs(t, upstream.client(), "b", "a", "c"))
	for i := range 6 {
		if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
			t.Fatalf("请求 %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	assertGenerationOrder(t, upstream, "oauth-a", "oauth-b", "oauth-c", "oauth-a", "oauth-b", "oauth-c")
	for _, account := range []string{"oauth-a", "oauth-b", "oauth-c"} {
		if got := upstream.sessionCount(account); got != 1 {
			t.Errorf("%s session 交换 %d 次, want 1（session 应按账号复用）", account, got)
		}
	}
}

func TestPoolRoutesEveryPublicEndpoint(t *testing.T) {
	upstream := newPoolUpstream(t)
	p := testPool(t, poolConfigs(t, upstream.client(), "oauth-a", "oauth-b"))
	for _, path := range []string{"/models", "/v1/models"} {
		if w := poolRequest(p, "GET", path, ""); w.Code != http.StatusOK {
			t.Errorf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/chat/completions", "/v1/chat/completions", "/responses", "/v1/responses", "/v1/messages"} {
		if w := poolRequest(p, "POST", path, poolChatBody); w.Code != http.StatusOK {
			t.Errorf("POST %s status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
}

func TestPoolWithSingleAccountMatchesService(t *testing.T) {
	// 静默 Service 日志；本测试只比较 HTTP 结果，不断言日志内容。
	captureSlog(t, slog.LevelError)
	upstream := newPoolUpstream(t)
	client := upstream.client()
	service := testService(t, Config{Token: "oauth-solo", Client: client})
	pool := testPool(t, poolConfigs(t, client, "oauth-solo"))
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/models", ""},
		{"POST", "/v1/chat/completions", poolChatBody},
	} {
		want := testRequest(service, tc.method, tc.path, tc.body)
		got := poolRequest(pool, tc.method, tc.path, tc.body)
		if got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("%s %s: pool = %d %s, service = %d %s", tc.method, tc.path,
				got.Code, got.Body.String(), want.Code, want.Body.String())
		}
	}
}

func TestPoolSessionFailureSwitchesAccountInSameRequestAndCoolsDown(t *testing.T) {
	// 必须在 NewPool 之前捕获日志，账号 Service 在创建时读取默认 logger。
	logs := captureJSONSlog(t, slog.LevelWarn)
	upstream := newPoolUpstream(t)
	upstream.failSession("oauth-a", poolFailure{http.StatusInternalServerError, `{"message":"a down"}`})
	p := testPool(t, poolUserConfigs(t, upstream.client(), "a", "b", "c"))

	// 第一次请求先由 A 尝试（同档权重相同，按文件名），A 的 session 失败后同一请求改用其他账号，三次请求都必须成功。
	// A 冷却后不再被尝试；B、C 之间的轮询顺序由 SWRR 决定，这里只断言 A 不再出现，不固定 B、C 的先后。
	for i := range 3 {
		w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
		if w.Code != http.StatusOK {
			t.Fatalf("请求 %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	got := upstream.generationOrder()
	if len(got) != 3 {
		t.Fatalf("三次请求的生成账号顺序 = %v, want 3 次", got)
	}
	for _, account := range got {
		if account == "oauth-a" {
			t.Errorf("A 冷却后仍被用于生成请求: %v", got)
		}
	}
	if sessions := upstream.sessionCount("oauth-a"); sessions != 1 {
		t.Errorf("冷却期内 A 的 session 交换 %d 次, want 1", sessions)
	}
	warned := false
	for _, entry := range jsonLogEntries(t, logs.Bytes()) {
		if entry["level"] == "WARN" && entry["account"] == float64(poolFailingAccount) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("session 失败未记录带 account=%d 的 WARN 日志: %s", poolFailingAccount, logs.String())
	}
}

func TestPoolNetworkFailureOnOneAccountFallsBackToNextAccount(t *testing.T) {
	upstream := newPoolUpstream(t)
	healthy := upstream.client()
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == poolTokenPrefix+"oauth-a" {
			return nil, errors.New("test network failure")
		}
		return healthy.Transport.RoundTrip(r)
	})}
	p := testPool(t, poolUserConfigs(t, client, "a", "b"))
	for i := range 2 {
		if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
			t.Fatalf("请求 %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	assertGenerationOrder(t, upstream, "oauth-b", "oauth-b")
}

func TestPoolReturnsLastAccountErrorWhenAllSessionsFail(t *testing.T) {
	upstream := newPoolUpstream(t)
	upstream.failSession("oauth-a", poolFailure{http.StatusUnauthorized, "a rejected"})
	upstream.failSession("oauth-b", poolFailure{http.StatusInternalServerError, "b failed"})
	upstream.failSession("oauth-c", poolFailure{http.StatusForbidden, "c forbidden"})
	p := testPool(t, poolUserConfigs(t, upstream.client(), "a", "b", "c"))

	w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
	// 上游错误状态和正文原样透传，且最后一个尝试的账号是 C。
	if w.Code != http.StatusForbidden || w.Body.String() != "c forbidden" {
		t.Fatalf("全部账号失败时 status=%d body=%q, want 403 %q", w.Code, w.Body.String(), "c forbidden")
	}
	// 本测试只发出这一个请求，因此 session 交换总数即本请求内每个账号的尝试次数，应恰好为 1。
	for _, account := range []string{"oauth-a", "oauth-b", "oauth-c"} {
		if got := upstream.sessionCount(account); got != 1 {
			t.Errorf("%s 在同一请求内 session 交换 %d 次, want 1", account, got)
		}
	}
}

func TestPoolReturnsBadGatewayWhenEveryAccountNetworkFails(t *testing.T) {
	client := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("test network failure")
	})}
	p := testPool(t, poolConfigs(t, client, "oauth-a", "oauth-b"))
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusBadGateway {
		t.Fatalf("全部账号网络失败 status=%d body=%s, want 502", w.Code, w.Body.String())
	}
}

func TestPoolGeneration429PassesThroughAndCoolsAccount(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			upstream := newPoolUpstream(t)
			upstream.failGeneration("oauth-a", poolFailure{http.StatusTooManyRequests, `{"error":"quota exhausted"}`})
			p := testPool(t, poolUserConfigs(t, upstream.client(), "a", "b"))

			w := poolRequest(p, "POST", path, poolChatBody)
			if w.Code != http.StatusTooManyRequests || w.Body.String() != `{"error":"quota exhausted"}` {
				t.Fatalf("429 未原样透传: status=%d body=%s", w.Code, w.Body.String())
			}
			// 生成接口的 429 不在同一请求内换账号重试。
			assertGenerationOrder(t, upstream, "oauth-a")

			for i := range 3 {
				if w := poolRequest(p, "POST", path, poolChatBody); w.Code != http.StatusOK {
					t.Fatalf("冷却后第 %d 个请求 status=%d body=%s", i+1, w.Code, w.Body.String())
				}
			}
			// 第一次请求后 A 进入冷却，之后的请求全部落在 B。
			assertGenerationOrder(t, upstream, "oauth-a", "oauth-b", "oauth-b", "oauth-b")
		})
	}
}

func TestPoolCancelledRequestDoesNotCoolAccounts(t *testing.T) {
	upstream := newPoolUpstream(t)
	p := testPool(t, poolConfigs(t, upstream.client(), "oauth-a", "oauth-b"))
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(poolChatBody))
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, r.WithContext(ctx))
	// 已取消的请求由 handleError 按网络错误处理，返回 502（正文为 context canceled），且不访问上游。
	if w.Code != http.StatusBadGateway {
		t.Fatalf("已取消请求 status=%d body=%s, want 502", w.Code, w.Body.String())
	}
	if got := upstream.requestCount(); got != 0 {
		t.Fatalf("已取消请求访问了上游 %d 次", got)
	}
	now := time.Now()
	for i, a := range p.accounts {
		if !a.service.available(now) {
			t.Errorf("客户端取消的请求不应冷却账号 %d", i)
		}
	}
}

func TestPoolAuthenticationAndHealth(t *testing.T) {
	upstream := newPoolUpstream(t)
	configs := poolConfigs(t, upstream.client(), "oauth-a", "oauth-b")
	for i := range configs {
		configs[i].APIKey = poolLocalAPIKey
	}
	p := testPool(t, configs)

	w := poolRequest(p, "GET", "/healthz", "")
	var health map[string]string
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &health) != nil || health["status"] != "ok" {
		t.Fatalf("healthz 无需鉴权时 status=%d body=%s", w.Code, w.Body.String())
	}
	for _, tc := range []struct{ method, path, auth string }{
		{"GET", "/v1/models", ""},
		{"POST", "/v1/chat/completions", ""},
		{"GET", "/v1/models", "Bearer wrong-key"},
		{"GET", "/v1/models", "Basic " + poolLocalAPIKey},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(poolChatBody))
		if tc.auth != "" {
			r.Header.Set("Authorization", tc.auth)
		}
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s auth=%q status=%d, want 401", tc.method, tc.path, tc.auth, w.Code)
		}
	}
	if got := upstream.requestCount(); got != 0 {
		t.Fatalf("健康检查或未授权请求访问了上游 %d 次", got)
	}
	for _, header := range []string{"Authorization", "X-Api-Key"} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		value := poolLocalAPIKey
		if header == "Authorization" {
			value = "Bearer " + value
		}
		r.Header.Set(header, value)
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("%s 携带正确密钥 status=%d body=%s", header, w.Code, w.Body.String())
		}
	}
}

// TestPoolLogsIncreasingRequestIDsAndAccountIndex 检查 request_id 在 Pool 内严格递增（因此也唯一），
// 且每个请求的日志带有一致的 account 下标。
func TestPoolLogsIncreasingRequestIDsAndAccountIndex(t *testing.T) {
	logs := captureJSONSlog(t, slog.LevelInfo)
	upstream := newPoolUpstream(t)
	p := testPool(t, poolUserConfigs(t, upstream.client(), "a", "b"))
	for i := range poolRequestsTotal {
		if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
			t.Fatalf("请求 %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}

	var received []float64 // 按到达顺序记录每个请求的 request_id
	accounts := map[float64]map[any]bool{}
	for _, entry := range jsonLogEntries(t, logs.Bytes()) {
		id, ok := entry["request_id"].(float64)
		if !ok {
			continue
		}
		if entry["msg"] == "收到客户端请求" {
			received = append(received, id)
		}
		if account, ok := entry["account"]; ok {
			if accounts[id] == nil {
				accounts[id] = map[any]bool{}
			}
			accounts[id][account] = true
		}
	}
	if len(received) != poolRequestsTotal {
		t.Fatalf("收到客户端请求的日志 %d 条, want %d", len(received), poolRequestsTotal)
	}
	for i := 1; i < len(received); i++ {
		if received[i] <= received[i-1] {
			t.Fatalf("request_id 未在 Pool 内严格递增: %v", received)
		}
	}
	var got []any
	for _, id := range received {
		seen := accounts[id]
		if len(seen) != 1 {
			t.Fatalf("request_id %v 的日志 account 字段 = %v, want 恰好一个账号下标", id, seen)
		}
		for account := range seen {
			got = append(got, account)
		}
	}
	// 两个账号按文件名轮流：请求依次由 a（账号下标 0）、b（账号下标 1）处理。
	want := []any{float64(0), float64(1), float64(0), float64(1)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("各请求日志中的 account = %v, want %v", got, want)
	}
}

func TestPoolSessionFreeRoutesIgnoreSessionFailures(t *testing.T) {
	upstream := newPoolUpstream(t)
	for _, account := range []string{"oauth-a", "oauth-b"} {
		upstream.failSession(account, poolFailure{http.StatusInternalServerError, `{"message":"session down"}`})
	}
	client := upstream.client()
	// 参照结果来自单账号 Service.Handler；这些路由不需要 session，两者结果必须一致。
	reference := testService(t, Config{Token: "oauth-a", Client: client})
	p := testPool(t, poolConfigs(t, client, "oauth-a", "oauth-b"))

	for _, tc := range []struct {
		name, method, path string
		wantCode           int
	}{
		{name: "double-slash-redirect", method: "GET", path: "//models", wantCode: http.StatusTemporaryRedirect},
		{name: "unknown-path", method: "GET", path: "/no-such-route", wantCode: http.StatusNotFound},
		{name: "wrong-method", method: "GET", path: "/v1/chat/completions", wantCode: http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := testRequest(reference, tc.method, tc.path, "")
			before := upstream.requestCount()
			got := poolRequest(p, tc.method, tc.path, "")
			if calls := upstream.requestCount() - before; calls != 0 {
				t.Errorf("Pool 处理 %s %s 访问上游 %d 次, want 0", tc.method, tc.path, calls)
			}
			if tc.wantCode != 0 && got.Code != tc.wantCode {
				t.Errorf("status = %d, want %d; body=%s", got.Code, tc.wantCode, got.Body.String())
			}
			// 重定向、404、405 需要与单账号 Service 的状态码和关键响应头完全一致。
			if got.Code != want.Code || got.Header().Get("Location") != want.Header().Get("Location") ||
				got.Header().Get("Allow") != want.Header().Get("Allow") {
				t.Errorf("%s %s: pool = %d Location=%q Allow=%q, service = %d Location=%q Allow=%q",
					tc.method, tc.path, got.Code, got.Header().Get("Location"), got.Header().Get("Allow"),
					want.Code, want.Header().Get("Location"), want.Header().Get("Allow"))
			}
			if got.Body.String() != want.Body.String() {
				t.Errorf("%s %s body = %q, service body = %q", tc.method, tc.path, got.Body.String(), want.Body.String())
			}
		})
	}
	for _, account := range []string{"oauth-a", "oauth-b"} {
		if got := upstream.sessionCount(account); got != 0 {
			t.Errorf("不需要 session 的请求触发了 %s 的 session 交换 %d 次", account, got)
		}
	}
	now := time.Now()
	for i, a := range p.accounts {
		if !a.service.available(now) {
			t.Errorf("不需要 session 的请求不应冷却账号 %d", i)
		}
	}
}

func TestPoolLogsRedactAccountTokensFromRequestPath(t *testing.T) {
	logs := captureJSONSlog(t, slog.LevelDebug)
	const tokenA, tokenB = "pool-path-secret-token-a", "pool-path-secret-token-b"
	upstream := newPoolUpstream(t)
	p := testPool(t, poolConfigs(t, upstream.client(), tokenA, tokenB))
	// 路径中同时带两个账号的 OAuth token；路由不存在，请求返回 404，不访问上游。
	if w := poolRequest(p, "GET", "/v1/models/"+tokenA+"/"+tokenB, ""); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	output := logs.String()
	if !strings.Contains(output, "收到客户端请求") {
		t.Fatalf("没有捕获到请求日志，无法验证脱敏: %s", output)
	}
	for _, token := range []string{tokenA, tokenB} {
		if strings.Contains(output, token) {
			t.Errorf("Pool 日志包含账号 OAuth token 原文 %q: %s", token, output)
		}
	}
}

// TestPoolLogsClientRequestHeadersWithSensitiveRedacted Pool 的"收到客户端请求"日志与单账号 Service 一致，
// 同样记录请求头并对 Authorization、X-Api-Key 脱敏。
func TestPoolLogsClientRequestHeadersWithSensitiveRedacted(t *testing.T) {
	logs := captureJSONSlog(t, slog.LevelInfo)
	configs := poolConfigs(t, newPoolUpstream(t).client(), "oauth-a", "oauth-b")
	for i := range configs {
		configs[i].APIKey = clientHeaderSecret
	}
	p := testPool(t, configs)
	if w := sendClientHeaderRequest(p.Handler()); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	assertClientRequestHeadersLogged(t, logs.Bytes())
}

// setupCooledAccountB 让账号 B 处于冷却中，A 与 C 未冷却。冷却直接设置而不经过 429 请求，
// 使测试只关注 session 失败与冷却档的选择顺序。session 有效期设为 1 分钟，使每个请求都会重新交换 session，
// 便于精确控制后续失败。
func setupCooledAccountB(t *testing.T) (*poolUpstream, *Pool) {
	t.Helper()
	upstream := newPoolUpstream(t)
	upstream.setSessionTTL(time.Minute)
	p := testPool(t, poolUserConfigs(t, upstream.client(), "a", "b", "c"))
	p.findAccount("b").service.setCooldown(time.Now().Add(time.Hour))
	return upstream, p
}

// sessionSnapshot 记录各账号当前的 session 交换次数，用于计算单个请求内的增量。
func sessionSnapshot(u *poolUpstream, accounts ...string) map[string]int {
	snapshot := make(map[string]int, len(accounts))
	for _, account := range accounts {
		snapshot[account] = u.sessionCount(account)
	}
	return snapshot
}

// assertSessionAttempts 检查单个请求内各账号 session 交换的增量与期望一致。
func assertSessionAttempts(t *testing.T, u *poolUpstream, before map[string]int, want map[string]int) {
	t.Helper()
	for account, delta := range want {
		if got := u.sessionCount(account) - before[account]; got != delta {
			t.Errorf("本请求内 %s 尝试 %d 次, want %d", account, got, delta)
		}
	}
}

func TestPoolRequestNeverRetriesAccountWithinOneRequest(t *testing.T) {
	accounts := []string{"oauth-a", "oauth-b", "oauth-c"}
	t.Run("cooled-account-is-last-candidate-and-serves", func(t *testing.T) {
		upstream, p := setupCooledAccountB(t)
		upstream.failSession("oauth-a", poolFailure{http.StatusInternalServerError, `{"message":"a down"}`})
		upstream.failSession("oauth-c", poolFailure{http.StatusInternalServerError, `{"message":"c down"}`})
		before := sessionSnapshot(upstream, "oauth-a", "oauth-c")
		if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200 由冷却中的 B 处理", w.Code, w.Body.String())
		}
		// A、C 各只在本请求内尝试一次（失败即冷却，之后不会再被选中）；
		// B 处于冷却档，只在 A、C 都被排除后才作为最后的候选被尝试，并成功处理。
		// 本请求只有 B 的生成请求，因此生成请求的账号顺序只含一次 oauth-b。
		assertSessionAttempts(t, upstream, before, map[string]int{"oauth-a": 1, "oauth-c": 1})
		assertGenerationOrder(t, upstream, "oauth-b")
	})
	t.Run("every-account-attempted-exactly-once", func(t *testing.T) {
		upstream, p := setupCooledAccountB(t)
		for _, account := range accounts {
			upstream.failSession(account, poolFailure{http.StatusInternalServerError, `{"message":"down"}`})
		}
		before := sessionSnapshot(upstream, accounts...)
		if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code == http.StatusOK {
			t.Fatalf("三个账号的 session 都失败时不应返回 200")
		}
		// 失败的 session 在预取阶段即返回、不进入请求处理函数，因此增量精确等于本请求内的尝试次数。
		assertSessionAttempts(t, upstream, before, map[string]int{"oauth-a": 1, "oauth-b": 1, "oauth-c": 1})
	})
}

// TestPoolGenerationLargeRateLimitBodyPassesThroughAndCoolsAccount 上游 429 的正文超过 4 MiB 时，
// 仍应原样透传正文和响应头（如 Retry-After），并让该账号进入冷却，而不是返回 502。
func TestPoolGenerationLargeRateLimitBodyPassesThroughAndCoolsAccount(t *testing.T) {
	body := strings.Repeat("q", 5<<20)
	upstream := newPoolUpstream(t)
	upstream.failGenerationWithHeader("oauth-a", poolFailure{http.StatusTooManyRequests, body},
		http.Header{"Retry-After": {"120"}})
	p := testPool(t, poolUserConfigs(t, upstream.client(), "a", "b"))

	w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("大正文 429 status=%d, want 429（正文 %d 字节）", w.Code, w.Body.Len())
	}
	if !bytes.Equal(w.Body.Bytes(), []byte(body)) {
		t.Fatalf("大正文 429 未逐字节透传: got %d 字节, want %d 字节", w.Body.Len(), len(body))
	}
	if got := w.Header().Get("Retry-After"); got != "120" {
		t.Fatalf("Retry-After = %q, want 120", got)
	}
	assertGenerationOrder(t, upstream, "oauth-a")
	for i := range 2 {
		if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
			t.Fatalf("冷却后第 %d 个请求 status=%d", i+1, w.Code)
		}
	}
	// 账号 A 已冷却，之后的请求全部由 B 处理。
	assertGenerationOrder(t, upstream, "oauth-a", "oauth-b", "oauth-b")
}

func TestPoolAutoRateLimitPassesThroughAndCoolsAccount(t *testing.T) {
	const autoBody = `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
	upstream := newPoolUpstream(t)
	upstream.failAuto("oauth-a", poolFailure{http.StatusTooManyRequests, `{"error":"auto limited"}`})
	p := testPool(t, poolUserConfigs(t, upstream.client(), "a", "b"))

	w := poolRequest(p, "POST", "/v1/chat/completions", autoBody)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("Auto 路径 429 未透传: status=%d body=%s", w.Code, w.Body.String())
	}
	autoAfterFirst := upstream.autoCount("oauth-a")
	for i := range 2 {
		if w := poolRequest(p, "POST", "/v1/chat/completions", autoBody); w.Code != http.StatusOK {
			t.Fatalf("冷却后请求 %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	// A 在 Auto 限流后进入冷却，之后的请求全部由 B 处理，且不再访问 A 的 Auto 接口。
	assertGenerationOrder(t, upstream, "oauth-b", "oauth-b")
	if got := upstream.autoCount("oauth-a"); got != autoAfterFirst {
		t.Errorf("冷却期内仍访问 A 的 Auto 接口: 次数 %d, want %d", got, autoAfterFirst)
	}
}

// TestPoolLogsUserAndAccountIndexPerAccount 账号设置了 User 时，账号级日志带 user 字段且与 User 一致，
// account 下标仍然正确；User 为空的账号，日志不带 user 字段。
func TestPoolLogsUserAndAccountIndexPerAccount(t *testing.T) {
	logs := captureJSONSlog(t, slog.LevelInfo)
	upstream := newPoolUpstream(t)
	configs := poolConfigs(t, upstream.client(), "oauth-a", "oauth-b")
	configs[0].User = "alice"
	p := testPool(t, configs)
	// 每次请求只留一个可用账号，使两个账号各处理一次，结果与两者的选择顺序无关。
	p.accounts[1].service.setCooldown(time.Now().Add(time.Hour))
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
		t.Fatalf("请求 0 status=%d body=%s", w.Code, w.Body.String())
	}
	p.accounts[0].service.setCooldown(time.Now().Add(time.Hour))
	p.accounts[1].service.setCooldown(time.Now().Add(-time.Second))
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
		t.Fatalf("请求 1 status=%d body=%s", w.Code, w.Body.String())
	}

	wantUser := map[float64]string{0: "alice", 1: ""} // 账号下标 -> 期望的 user 字段（空表示不应出现）
	seen := map[float64]bool{}
	for _, entry := range jsonLogEntries(t, logs.Bytes()) {
		account, ok := entry["account"].(float64)
		if !ok {
			continue
		}
		seen[account] = true
		want, known := wantUser[account]
		if !known {
			t.Errorf("日志的 account 下标 %v 超出账号范围", account)
			continue
		}
		user, hasUser := entry["user"]
		switch {
		case want != "" && (!hasUser || user != want):
			t.Errorf("账号 %v 的日志 user = %v (存在=%v), want %q", account, user, hasUser, want)
		case want == "" && hasUser:
			t.Errorf("未设置 User 的账号 %v 的日志不应带 user 字段，实际 = %v", account, user)
		}
	}
	for account := range wantUser {
		if !seen[account] {
			t.Errorf("账号 %v 没有带 account 字段的日志", account)
		}
	}
}
