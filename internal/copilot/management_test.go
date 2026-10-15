package copilot

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ghc2api/web"
)

const (
	mgmtKey    = "mgmt-test-key"     // 管理密钥
	mgmtAPIKey = "mgmt-test-api-key" // 服务访问密钥；管理接口不接受它
)

// mgmtQuotaBody 是默认的 /copilot_internal/user 正文：个人版（Copilot Pro），Premium 剩余 80%。
const mgmtQuotaBody = `{"copilot_plan":"individual","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":80}}}`

// mgmtHold 让上游在某个环节（生成、session 交换、额度查询）阻塞，直到 free 被调用或请求被取消，
// 用于构造“进行中的请求”与超时场景。
type mgmtHold struct {
	started chan struct{} // 请求到达阻塞点时发送一次
	release chan struct{} // 关闭后请求继续处理
	once    sync.Once
}

func (h *mgmtHold) free() {
	h.once.Do(func() { close(h.release) })
}

// mgmtUpstream 在 poolUpstream 之上模拟 GitHub 账号接口：/user、/copilot_internal/user 与设备登录；
// 其余路径（session、模型、生成、用量）交给 pool。
type mgmtUpstream struct {
	t      *testing.T
	pool   *poolUpstream
	client *http.Client

	mu           sync.Mutex
	logins       map[string]string      // OAuth token -> GitHub 登录名；未登记的 token 访问 /user 返回 401
	quotaCalls   map[string]int         // 每个 token 访问 /copilot_internal/user 的次数
	quotaFails   map[string]poolFailure // 指定 token 的额度请求返回该失败
	holds        map[string]*mgmtHold   // 指定 token 的生成请求在 hold 释放前阻塞
	sessionHolds map[string]*mgmtHold   // 指定 token 的 session 交换在 hold 释放前阻塞
	quotaHolds   map[string]*mgmtHold   // 指定 token 的额度查询在 hold 释放前阻塞
	devicePolls  int                    // 设备登录轮询次数
	deviceToken  string                 // 设备登录成功时返回的 OAuth token
	deviceError  string                 // 非空时设备登录轮询返回该 OAuth 错误
}

func newMgmtUpstream(t *testing.T) *mgmtUpstream {
	t.Helper()
	m := &mgmtUpstream{
		t: t, pool: newPoolUpstream(t),
		logins: map[string]string{}, quotaCalls: map[string]int{},
		quotaFails: map[string]poolFailure{}, holds: map[string]*mgmtHold{},
		sessionHolds: map[string]*mgmtHold{}, quotaHolds: map[string]*mgmtHold{},
	}
	m.client = testClient(t, m.serve)
	return m
}

func (m *mgmtUpstream) serve(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
	switch r.URL.Path {
	case "/user":
		m.mu.Lock()
		login, ok := m.logins[token]
		m.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		testJSON(w, map[string]string{"login": login})
	case "/copilot_internal/user":
		m.serveQuota(w, r, token)
	case "/login/device/code":
		testDevice(w, "")
	case "/login/oauth/access_token":
		m.serveDevicePoll(w)
	default:
		if r.URL.Path == "/copilot_internal/v2/token" {
			m.waitOn(m.sessionHolds, token, r)
		}
		if account, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer session-"); ok {
			m.waitOn(m.holds, account, r)
		}
		m.pool.serve(w, r)
	}
}

func (m *mgmtUpstream) serveQuota(w http.ResponseWriter, r *http.Request, token string) {
	m.mu.Lock()
	m.quotaCalls[token]++
	failure, failing := m.quotaFails[token]
	m.mu.Unlock()
	m.waitOn(m.quotaHolds, token, r)
	if failing {
		w.WriteHeader(failure.status)
		_, _ = io.WriteString(w, failure.body)
		return
	}
	_, _ = io.WriteString(w, mgmtQuotaBody)
}

// serveDevicePoll 第一次轮询返回 authorization_pending，之后返回 token；deviceError 非空时直接返回该错误。
func (m *mgmtUpstream) serveDevicePoll(w http.ResponseWriter) {
	m.mu.Lock()
	m.devicePolls++
	first := m.devicePolls == 1
	deviceError, token := m.deviceError, m.deviceToken
	m.mu.Unlock()
	switch {
	case deviceError != "":
		testJSON(w, map[string]string{"error": deviceError})
	case first:
		testJSON(w, map[string]string{"error": "authorization_pending"})
	default:
		testJSON(w, map[string]string{"access_token": token})
	}
}

// waitOn 若 token 在 holds 中登记了阻塞点，则通知测试并等待释放；请求被取消时立即返回。
func (m *mgmtUpstream) waitOn(holds map[string]*mgmtHold, token string, r *http.Request) {
	m.mu.Lock()
	hold := holds[token]
	m.mu.Unlock()
	if hold == nil {
		return
	}
	select {
	case hold.started <- struct{}{}:
	default:
	}
	select {
	case <-hold.release:
	case <-r.Context().Done():
	}
}

// newHold 为 token 登记阻塞点；测试结束时自动释放，避免测试失败时 httptest 服务无法关闭。
func (m *mgmtUpstream) newHold(holds map[string]*mgmtHold, token string) *mgmtHold {
	hold := &mgmtHold{started: make(chan struct{}, 1), release: make(chan struct{})}
	m.mu.Lock()
	holds[token] = hold
	m.mu.Unlock()
	m.t.Cleanup(hold.free)
	return hold
}

// holdGenerations 让 token 对应账号的生成请求在上游阻塞，直到返回的 hold 被释放。
func (m *mgmtUpstream) holdGenerations(token string) *mgmtHold {
	return m.newHold(m.holds, token)
}

// holdSessions 让 token 的 session 交换在上游阻塞，直到返回的 hold 被释放。
func (m *mgmtUpstream) holdSessions(token string) *mgmtHold {
	return m.newHold(m.sessionHolds, token)
}

// holdQuota 让 token 的额度查询在上游阻塞，直到返回的 hold 被释放或请求被取消。
func (m *mgmtUpstream) holdQuota(token string) *mgmtHold {
	return m.newHold(m.quotaHolds, token)
}

func (m *mgmtUpstream) setLogin(token, login string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logins[token] = login
}

// setDeviceLogin 让设备登录成功后返回 token，并使该 token 的 /user 返回 login。
func (m *mgmtUpstream) setDeviceLogin(token, login string) {
	m.setLogin(token, login)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deviceToken = token
}

func (m *mgmtUpstream) denyDeviceLogin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deviceError = "access_denied"
}

func (m *mgmtUpstream) failQuota(token string, status int, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotaFails[token] = poolFailure{status, body}
}

func (m *mgmtUpstream) quotaCount(token string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quotaCalls[token]
}

// mgmtOrder 把账号用户名映射为 OAuth token，用于与 generationOrder 比较。
func mgmtOrder(users ...string) []string {
	tokens := make([]string, 0, len(users))
	for _, user := range users {
		tokens = append(tokens, "oauth-"+user)
	}
	return tokens
}

// mgmtPoolConfig 返回托管池配置：凭证目录为 dir，缓存目录为临时目录，上游使用 up。
func mgmtPoolConfig(t *testing.T, up *mgmtUpstream, dir string, ttl time.Duration) PoolConfig {
	t.Helper()
	return PoolConfig{
		ManagementKey: mgmtKey, CredentialsDir: dir, CacheDir: t.TempDir(),
		Client: up.client, SessionAffinityTTL: ttl,
	}
}

// mgmtNewPool 为每个用户写入 <dir>/<user>.json 凭证（token 为 "oauth-<user>"），并按传入顺序创建托管池。
// 返回的 dir 即凭证目录，供测试检查文件变化。
func mgmtNewPool(t *testing.T, up *mgmtUpstream, ttl time.Duration, users ...string) (*Pool, string) {
	t.Helper()
	dir := t.TempDir()
	accounts := make([]PoolAccount, 0, len(users))
	for _, user := range users {
		token := "oauth-" + user
		if err := SaveCredential(filepath.Join(dir, user+".json"), Credential{Token: token, User: user}); err != nil {
			t.Fatal(err)
		}
		up.setLogin(token, user)
		accounts = append(accounts, PoolAccount{Token: token, User: user})
	}
	p, err := NewManagedPool(mgmtPoolConfig(t, up, dir, ttl), accounts)
	if err != nil {
		t.Fatal(err)
	}
	return p, dir
}

// mgmtCall 通过 Pool 的 HTTP 入口调用接口；key 为空时不带 Authorization 头。
func mgmtCall(p *Pool, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, r)
	return w
}

// mgmtAccounts 调用 GET /v0/management/accounts 并解析结果。
func mgmtAccounts(t *testing.T, p *Pool) []AccountView {
	t.Helper()
	w := mgmtCall(p, "GET", "/v0/management/accounts", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("GET accounts status=%d body=%s", w.Code, w.Body.String())
	}
	var views []AccountView
	if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil {
		t.Fatalf("账号列表不是合法 JSON: %v; body=%s", err, w.Body.String())
	}
	return views
}

// mgmtAccountByID 返回指定 ID 的账号视图；不存在时终止测试。
func mgmtAccountByID(t *testing.T, views []AccountView, id string) AccountView {
	t.Helper()
	for _, view := range views {
		if view.ID == id {
			return view
		}
	}
	t.Fatalf("账号列表中缺少 %q: %+v", id, views)
	return AccountView{}
}

// mgmtErrorMessage 解析 writeError 格式的错误消息。
func mgmtErrorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是 JSON: %v; body=%s", err, w.Body.String())
	}
	return body.Error.Message
}

// mgmtLoginStart 是 POST /v0/management/login 的响应。
type mgmtLoginStart struct {
	State     string `json:"state"`
	URL       string `json:"url"`
	UserCode  string `json:"user_code"`
	Interval  int    `json:"interval"`
	ExpiresIn int    `json:"expires_in"`
}

// mgmtLoginResult 是 GET /v0/management/login/status 的响应。
type mgmtLoginResult struct {
	Status string `json:"status"`
	ID     string `json:"id"`
	Error  string `json:"error"`
}

// mgmtPollLogin 轮询登录状态直到不再是 pending，最多等待 15 秒。
func mgmtPollLogin(t *testing.T, p *Pool, state string) mgmtLoginResult {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		w := mgmtCall(p, "GET", "/v0/management/login/status?state="+state, "", mgmtKey)
		if w.Code != http.StatusOK {
			t.Fatalf("login/status status=%d body=%s", w.Code, w.Body.String())
		}
		var result mgmtLoginResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != "pending" {
			return result
		}
		if time.Now().After(deadline) {
			t.Fatal("设备登录超过 15 秒仍为 pending")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// mgmtIsHex 报告 s 是否为 n 位十六进制字符串。
func mgmtIsHex(s string, n int) bool {
	_, err := hex.DecodeString(s)
	return err == nil && len(s) == n
}

func TestNewManagedPoolAcceptsZeroAccountsOnlyWithManagementKey(t *testing.T) {
	if _, err := NewManagedPool(PoolConfig{ManagementKey: mgmtKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir()}, nil); err != nil {
		t.Fatalf("有管理密钥时 0 个账号应允许启动: %v", err)
	}
	if _, err := NewManagedPool(PoolConfig{CredentialsDir: t.TempDir(), CacheDir: t.TempDir()}, nil); err == nil {
		t.Fatal("没有管理密钥且 0 个账号时必须拒绝")
	}
}

func TestNewManagedPoolRejectsInvalidAccounts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accounts []PoolAccount
	}{
		{name: "missing-user", accounts: []PoolAccount{{Token: "oauth-a"}}},
		{name: "duplicate-user-ignoring-case", accounts: []PoolAccount{
			{Token: "oauth-a", User: "alice"}, {Token: "oauth-b", User: "Alice"},
		}},
		{name: "duplicate-host-and-token", accounts: []PoolAccount{
			{Token: "same", User: "alice"}, {Token: " same ", User: "bob"},
		}},
		{name: "empty-token", accounts: []PoolAccount{{Token: "  ", User: "alice"}}},
		{name: "invalid-host", accounts: []PoolAccount{{Token: "oauth-a", Host: "evil.example", User: "alice"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := PoolConfig{ManagementKey: mgmtKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir()}
			if _, err := NewManagedPool(config, tc.accounts); err == nil {
				t.Fatalf("NewManagedPool(%+v) 必须拒绝", tc.accounts)
			}
		})
	}
}

func TestManagementRoutesRequireManagementKey(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		key  string
		want int
	}{
		{name: "management-key", key: mgmtKey, want: http.StatusOK},
		{name: "wrong-key", key: "wrong", want: http.StatusUnauthorized},
		{name: "missing-key", key: "", want: http.StatusUnauthorized},
		{name: "api-key-is-not-management-key", key: mgmtAPIKey, want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := mgmtCall(p, "GET", "/v0/management/accounts", "", tc.key); w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	// 管理密钥只接受 Authorization: Bearer，不能通过 X-Api-Key 传递。
	r := httptest.NewRequest("GET", "/v0/management/accounts", nil)
	r.Header.Set("X-Api-Key", mgmtKey)
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("X-Api-Key 携带管理密钥 status = %d, want 401", w.Code)
	}
}

func TestManagementRoutesAbsentWithoutManagementKey(t *testing.T) {
	p := testPool(t, poolConfigs(t, nil, "oauth-a"))
	for _, tc := range []struct{ method, path string }{
		{"GET", "/management"},
		{"GET", "/v0/management/accounts"},
		{"POST", "/v0/management/accounts"},
		{"POST", "/v0/management/login"},
		{"GET", "/v0/management/login/status?state=x"},
		{"POST", "/v0/management/quota/refresh"},
	} {
		// 即使携带任意 Authorization，未启用管理接口时也只能得到普通 404。
		if w := mgmtCall(p, tc.method, tc.path, "", mgmtKey); w.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", tc.method, tc.path, w.Code)
		}
	}
}

// TestManagementConsolePageIsPublicAndMatchesEmbeddedHTML 控制台页面无需鉴权，且返回内嵌的 HTML；
// 管理接口不受 api_key 影响，携带管理密钥即可访问。
func TestManagementConsolePageIsPublicAndMatchesEmbeddedHTML(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := web.Index()
	if err != nil {
		t.Fatal(err)
	}
	w := mgmtCall(p, "GET", "/management", "", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("GET /management status=%d content-type=%q，正文与内嵌 HTML 不一致", w.Code, w.Header().Get("Content-Type"))
	}
	if w := mgmtCall(p, "GET", "/v0/management/accounts", "", mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("携带管理密钥访问管理接口 status = %d, want 200", w.Code)
	}
}

// TestManagementAccountsListShowsQuotaAndRequestStats 账号列表包含额度、请求数、错误数与冷却信息；
// 会话切换时只计入最终处理请求的账号；响应中不包含 OAuth token。
func TestManagementAccountsListShowsQuotaAndRequestStats(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice", "bob", "carol")
	up.pool.failGeneration("oauth-bob", poolFailure{http.StatusInternalServerError, `{"error":"bob down"}`})
	up.pool.failSession("oauth-carol", poolFailure{http.StatusInternalServerError, `{"message":"carol down"}`})

	// 用量查询由 alice 处理；生成请求轮到 bob（上游 500）；再下一次轮到 carol，其 session 失败后改由 alice 处理。
	if w := poolRequest(p, "GET", "/v1/usage", ""); w.Code != http.StatusOK {
		t.Fatalf("用量请求 status=%d body=%s", w.Code, w.Body.String())
	}
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusInternalServerError {
		t.Fatalf("bob 的生成请求 status=%d, want 500", w.Code)
	}
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
		t.Fatalf("carol 失败后的生成请求 status=%d body=%s", w.Code, w.Body.String())
	}

	w := mgmtCall(p, "GET", "/v0/management/accounts", "", mgmtKey)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("GET accounts status=%d content-type=%q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, token := range mgmtOrder("alice", "bob", "carol") {
		if strings.Contains(w.Body.String(), token) {
			t.Fatalf("账号列表包含 OAuth token %q", token)
		}
	}
	var views []AccountView
	if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil {
		t.Fatalf("账号列表不是合法 JSON: %v", err)
	}
	alice := mgmtAccountByID(t, views, "alice")
	if alice.Stats.Requests != 2 || alice.Stats.Errors != 0 || alice.Stats.LastUsedAt == nil {
		t.Fatalf("alice 统计 = %+v, want 2 次请求、0 次错误、有最近使用时间", alice.Stats)
	}
	bob := mgmtAccountByID(t, views, "bob")
	if bob.Stats.Requests != 1 || bob.Stats.Errors != 1 {
		t.Fatalf("bob 统计 = %+v, want 1 次请求、1 次错误", bob.Stats)
	}
	carol := mgmtAccountByID(t, views, "carol")
	if carol.Stats.Requests != 0 || carol.Stats.Errors != 0 {
		t.Fatalf("carol 统计 = %+v, session 失败的请求不应计入 carol", carol.Stats)
	}
	if carol.CooldownUntil == nil {
		t.Fatal("session 失败后 carol 应显示冷却截止时间")
	}
	for _, view := range []AccountView{alice, bob, carol} {
		if view.Quota == nil || view.Quota.Subscription == nil || view.Quota.Subscription.Plan != "Copilot Pro" {
			t.Fatalf("%s 额度 = %+v, want 计划 Copilot Pro", view.ID, view.Quota)
		}
	}
}

// TestManagementQuotaIsCachedForSixtySecondsUnlessRefreshed 额度在 60 秒内不重复请求上游，强制刷新除外。
func TestManagementQuotaIsCachedForSixtySecondsUnlessRefreshed(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice", "bob")
	mgmtAccounts(t, p) // 首次列表：两个账号都没有额度缓存，各拉取一次。
	mgmtAccounts(t, p) // 缓存仍然新鲜，不再请求上游。
	if alice, bob := up.quotaCount("oauth-alice"), up.quotaCount("oauth-bob"); alice != 1 || bob != 1 {
		t.Fatalf("两次列表后额度请求次数 alice=%d bob=%d, want 1 与 1", alice, bob)
	}
	if w := mgmtCall(p, "POST", "/v0/management/quota/refresh", `{"id":"alice"}`, mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("强制刷新 status=%d body=%s", w.Code, w.Body.String())
	}
	if alice, bob := up.quotaCount("oauth-alice"), up.quotaCount("oauth-bob"); alice != 2 || bob != 1 {
		t.Fatalf("强制刷新后额度请求次数 alice=%d bob=%d, want 2 与 1", alice, bob)
	}
}

// TestManagementQuotaFailuresKeepOldData 额度刷新失败时：限流保留旧数据并标记 stale；
// 其他错误保留旧数据并带 quota_error；没有旧数据时强制刷新返回 502，账号列表仍然返回 200。
func TestManagementQuotaFailuresKeepOldData(t *testing.T) {
	refresh := func(t *testing.T, p *Pool) *httptest.ResponseRecorder {
		t.Helper()
		return mgmtCall(p, "POST", "/v0/management/quota/refresh", `{"id":"alice"}`, mgmtKey)
	}
	decodeView := func(t *testing.T, w *httptest.ResponseRecorder) AccountView {
		t.Helper()
		var view AccountView
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatalf("账号视图不是合法 JSON: %v; body=%s", err, w.Body.String())
		}
		return view
	}

	t.Run("rate-limited-keeps-old-data", func(t *testing.T) {
		up := newMgmtUpstream(t)
		p, _ := mgmtNewPool(t, up, 0, "alice")
		mgmtAccounts(t, p)
		up.failQuota("oauth-alice", http.StatusTooManyRequests, `{"error":"limited"}`)
		w := refresh(t, p)
		view := decodeView(t, w)
		if w.Code != http.StatusOK || view.Quota == nil || !view.Quota.Stale || quotaTestPlan(*view.Quota) != "Copilot Pro" {
			t.Fatalf("限流刷新 status=%d quota=%+v, want 200 并保留旧额度且 stale=true", w.Code, view.Quota)
		}
	})
	t.Run("error-keeps-old-data-with-quota-error", func(t *testing.T) {
		up := newMgmtUpstream(t)
		p, _ := mgmtNewPool(t, up, 0, "alice")
		mgmtAccounts(t, p)
		up.failQuota("oauth-alice", http.StatusInternalServerError, `{"error":"down"}`)
		w := refresh(t, p)
		view := decodeView(t, w)
		if w.Code != http.StatusOK || view.Quota == nil || view.QuotaError == "" {
			t.Fatalf("刷新失败但有旧数据 status=%d view=%+v, want 200、保留旧额度并带 quota_error", w.Code, view)
		}
	})
	t.Run("error-without-old-data-is-bad-gateway", func(t *testing.T) {
		up := newMgmtUpstream(t)
		p, _ := mgmtNewPool(t, up, 0, "alice")
		up.failQuota("oauth-alice", http.StatusInternalServerError, `{"error":"down"}`)
		if w := refresh(t, p); w.Code != http.StatusBadGateway {
			t.Fatalf("没有旧数据时刷新失败 status=%d, want 502", w.Code)
		}
	})
	t.Run("account-list-tolerates-quota-error", func(t *testing.T) {
		up := newMgmtUpstream(t)
		p, _ := mgmtNewPool(t, up, 0, "alice")
		up.failQuota("oauth-alice", http.StatusInternalServerError, `{"error":"down"}`)
		alice := mgmtAccountByID(t, mgmtAccounts(t, p), "alice")
		if alice.Quota != nil || alice.QuotaError == "" {
			t.Fatalf("额度拉取失败时列表视图 = %+v, want 无额度且带 quota_error", alice)
		}
	})
}

// TestManagementImportAccountAppendsToPool 导入凭证：登录名（保留大小写）决定文件名，ID 为小写，
// 文件权限为 0600，新账号追加到轮询末尾。
func TestManagementImportAccountAppendsToPool(t *testing.T) {
	up := newMgmtUpstream(t)
	up.setLogin("oauth-carol", "Carol")
	p, dir := mgmtNewPool(t, up, 0, "alice")
	w := mgmtCall(p, "POST", "/v0/management/accounts", `{"credential":{"oauth_token":"oauth-carol"}}`, mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("导入 status=%d body=%s", w.Code, w.Body.String())
	}
	var view AccountView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ID != "carol" || view.User != "Carol" || view.Host != "" || view.Disabled {
		t.Fatalf("导入结果 = %+v, want ID carol、User Carol、未禁用", view)
	}
	path := filepath.Join(dir, "Carol.json")
	c, err := LoadCredential(path)
	if err != nil || c.Token != "oauth-carol" || c.User != "Carol" {
		t.Fatalf("凭证文件 = %+v, %v", c, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("凭证文件权限 = %v, %v; want 0600", info, err)
	}
	for range 4 {
		if res := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); res.Code != http.StatusOK {
			t.Fatalf("生成请求 status=%d body=%s", res.Code, res.Body.String())
		}
	}
	assertGenerationOrder(t, up.pool, mgmtOrder("alice", "carol", "alice", "carol")...)
}

// TestManagementImportRejectsInvalidCredentials 导入失败时返回对应状态码，不写入文件、不加入池，且响应不包含 token。
func TestManagementImportRejectsInvalidCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		setup  func(up *mgmtUpstream)
		want   int
		secret string // 不得出现在响应中的 OAuth token
	}{
		{
			name: "user-mismatch", body: `{"credential":{"oauth_token":"oauth-carol","user":"bob"}}`,
			setup: func(up *mgmtUpstream) { up.setLogin("oauth-carol", "carol") },
			want:  http.StatusBadRequest, secret: "oauth-carol",
		},
		{
			name: "duplicate-token", body: `{"credential":{"oauth_token":"oauth-alice"}}`,
			want: http.StatusConflict, secret: "oauth-alice",
		},
		{
			name: "duplicate-login-ignoring-case", body: `{"credential":{"oauth_token":"oauth-alias"}}`,
			setup: func(up *mgmtUpstream) { up.setLogin("oauth-alias", "ALICE") },
			want:  http.StatusConflict, secret: "oauth-alias",
		},
		{
			name: "subscription-check-failed", body: `{"credential":{"oauth_token":"oauth-dan"}}`,
			setup: func(up *mgmtUpstream) {
				up.setLogin("oauth-dan", "dan")
				up.failQuota("oauth-dan", http.StatusForbidden, `{"message":"no subscription"}`)
			},
			want: http.StatusBadRequest, secret: "oauth-dan",
		},
		{
			name: "token-rejected-by-github", body: `{"credential":{"oauth_token":"oauth-unknown"}}`,
			want: http.StatusBadRequest, secret: "oauth-unknown",
		},
		{
			name: "invalid-host", body: `{"credential":{"oauth_token":"oauth-carol","host":"evil.example"}}`,
			setup: func(up *mgmtUpstream) { up.setLogin("oauth-carol", "carol") },
			want:  http.StatusBadRequest, secret: "oauth-carol",
		},
		{name: "missing-token", body: `{"credential":{}}`, want: http.StatusBadRequest},
		{name: "malformed-json", body: `{"credential":`, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			if tc.setup != nil {
				tc.setup(up)
			}
			p, dir := mgmtNewPool(t, up, 0, "alice")
			w := mgmtCall(p, "POST", "/v0/management/accounts", tc.body, mgmtKey)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
			if tc.secret != "" && strings.Contains(w.Body.String(), tc.secret) {
				t.Fatalf("响应包含 OAuth token: %s", w.Body.String())
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("凭证目录条目 = %d, %v; want 只有 alice.json", len(entries), err)
			}
			if got := len(mgmtAccounts(t, p)); got != 1 {
				t.Fatalf("导入失败后账号数 = %d, want 1", got)
			}
		})
	}
}

// TestManagementDisableExcludesAccountAndPersistsFlag 禁用后账号不参与轮询，凭证文件的 disabled 被持久化；启用后恢复。
func TestManagementDisableExcludesAccountAndPersistsFlag(t *testing.T) {
	up := newMgmtUpstream(t)
	p, dir := mgmtNewPool(t, up, 0, "alice", "bob", "carol")
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
		t.Fatalf("请求 status=%d body=%s", w.Code, w.Body.String())
	}

	w := mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"bob","disabled":true}`, mgmtKey)
	if w.Code != http.StatusOK || !mgmtAccountByID(t, mgmtAccounts(t, p), "bob").Disabled {
		t.Fatalf("禁用 bob status=%d body=%s", w.Code, w.Body.String())
	}
	if c, err := LoadCredential(filepath.Join(dir, "bob.json")); err != nil || !c.Disabled {
		t.Fatalf("禁用后凭证文件 = %+v, %v; want Disabled=true", c, err)
	}
	for range 4 {
		poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
	}
	// 第一次请求后游标指向 bob，但 bob 已禁用，因此依次落在 carol 与 alice 之间。
	assertGenerationOrder(t, up.pool, mgmtOrder("alice", "carol", "alice", "carol", "alice")...)

	w = mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"bob","disabled":false}`, mgmtKey)
	if w.Code != http.StatusOK || mgmtAccountByID(t, mgmtAccounts(t, p), "bob").Disabled {
		t.Fatalf("启用 bob status=%d body=%s", w.Code, w.Body.String())
	}
	if c, err := LoadCredential(filepath.Join(dir, "bob.json")); err != nil || c.Disabled {
		t.Fatalf("启用后凭证文件 = %+v, %v; want Disabled=false", c, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "bob.json"))
	if err != nil || strings.Contains(string(raw), "disabled") {
		t.Fatalf("启用后凭证文件不应保留 disabled 字段: %s, %v", raw, err)
	}
	for range 3 {
		poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
	}
	assertGenerationOrder(t, up.pool, mgmtOrder("alice", "carol", "alice", "carol", "alice", "bob", "carol", "alice")...)
}

// TestManagementDisableWriteFailureKeepsState 凭证写入失败时返回 500，内存中的禁用状态保持不变。
func TestManagementDisableWriteFailureKeepsState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 可以写入只读目录，无法模拟写入失败")
	}
	up := newMgmtUpstream(t)
	p, dir := mgmtNewPool(t, up, 0, "alice", "bob")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if w := mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"bob","disabled":true}`, mgmtKey); w.Code != http.StatusInternalServerError {
		t.Fatalf("凭证写入失败时 status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if mgmtAccountByID(t, mgmtAccounts(t, p), "bob").Disabled {
		t.Fatal("写入失败后内存中的 bob 不应被禁用")
	}
	poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
	poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
	assertGenerationOrder(t, up.pool, mgmtOrder("alice", "bob")...)
}

// TestManagementDeleteRemovesCredentialAndAccount 删除账号会删除凭证文件，之后不再被选中；文件已不存在时仍然成功。
func TestManagementDeleteRemovesCredentialAndAccount(t *testing.T) {
	up := newMgmtUpstream(t)
	p, dir := mgmtNewPool(t, up, 0, "alice", "bob", "carol")
	w := mgmtCall(p, "DELETE", "/v0/management/accounts?id=bob", "", mgmtKey)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("删除 bob status=%d body=%s, want 200 {\"ok\":true}", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "bob.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("删除后凭证文件仍存在: %v", err)
	}
	for range 4 {
		poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
	}
	for _, token := range up.pool.generationOrder() {
		if token == "oauth-bob" {
			t.Fatal("已删除的账号仍被选中")
		}
	}

	if err := os.Remove(filepath.Join(dir, "carol.json")); err != nil {
		t.Fatal(err)
	}
	if w := mgmtCall(p, "DELETE", "/v0/management/accounts?id=carol", "", mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("凭证文件已不存在时删除 status=%d, want 200", w.Code)
	}
	views := mgmtAccounts(t, p)
	if len(views) != 1 || views[0].ID != "alice" {
		t.Fatalf("删除后账号列表 = %+v, want 只剩 alice", views)
	}
}

// TestManagementDeleteKeepsInFlightRequestAlive 删除正在处理请求的账号不影响该请求；删除之后不再选中它。
func TestManagementDeleteKeepsInFlightRequestAlive(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice", "bob")
	hold := up.holdGenerations("oauth-alice")

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- poolRequest(p, "POST", "/v1/chat/completions", poolChatBody) }()
	select {
	case <-hold.started:
	case <-time.After(10 * time.Second):
		t.Fatal("生成请求未到达 alice 的上游")
	}

	if w := mgmtCall(p, "DELETE", "/v0/management/accounts?id=alice", "", mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("删除进行中的账号 status=%d body=%s", w.Code, w.Body.String())
	}
	hold.free()
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("进行中的请求被删除影响: status=%d body=%s", w.Code, w.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("进行中的请求没有完成")
	}

	for range 4 {
		if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
			t.Fatalf("删除后的请求 status=%d body=%s", w.Code, w.Body.String())
		}
	}
	order := up.pool.generationOrder()
	for _, token := range order[1:] {
		if token == "oauth-alice" {
			t.Fatalf("删除后仍选中 alice: %v", order)
		}
	}
}

// TestManagementRejectsUnknownAccountAndBadRequests 未知 ID 返回 404；缺少必填字段返回 400，且不改变现有账号。
func TestManagementRejectsUnknownAccountAndBadRequests(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice")
	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{name: "patch-unknown-id", method: "PATCH", path: "/v0/management/accounts", body: `{"id":"nobody","disabled":true}`, want: http.StatusNotFound},
		{name: "delete-unknown-id", method: "DELETE", path: "/v0/management/accounts?id=nobody", want: http.StatusNotFound},
		{name: "refresh-unknown-id", method: "POST", path: "/v0/management/quota/refresh", body: `{"id":"nobody"}`, want: http.StatusNotFound},
		{name: "patch-missing-disabled", method: "PATCH", path: "/v0/management/accounts", body: `{"id":"alice"}`, want: http.StatusBadRequest},
		{name: "patch-missing-id", method: "PATCH", path: "/v0/management/accounts", body: `{"disabled":true}`, want: http.StatusBadRequest},
		{name: "delete-missing-id", method: "DELETE", path: "/v0/management/accounts", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := mgmtCall(p, tc.method, tc.path, tc.body, mgmtKey); w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	if got := mgmtAccounts(t, p); len(got) != 1 || got[0].Disabled {
		t.Fatalf("失败的请求改变了账号状态: %+v", got)
	}
}

// TestManagementWithoutUsableAccountsReturns503 没有任何可用账号（0 个或全部禁用）时，需要账号的请求返回 503；
// 健康检查不依赖账号，仍然返回 200 且不访问上游。
func TestManagementWithoutUsableAccountsReturns503(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accounts []PoolAccount
	}{
		{name: "zero-accounts", accounts: nil},
		{name: "all-disabled", accounts: []PoolAccount{
			{Token: "oauth-alice", User: "alice", Disabled: true},
			{Token: "oauth-bob", User: "bob", Disabled: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			p, err := NewManagedPool(mgmtPoolConfig(t, up, t.TempDir(), 0), tc.accounts)
			if err != nil {
				t.Fatal(err)
			}
			if w := poolRequest(p, "GET", "/healthz", ""); w.Code != http.StatusOK {
				t.Fatalf("healthz status = %d, want 200", w.Code)
			}
			for _, req := range []struct{ method, path, body string }{
				{"POST", "/v1/chat/completions", poolChatBody},
				{"GET", "/v1/usage", ""},
			} {
				w := poolRequest(p, req.method, req.path, req.body)
				if w.Code != http.StatusServiceUnavailable || mgmtErrorMessage(t, w) != "没有可用的 GitHub Copilot 账号" {
					t.Fatalf("%s %s status=%d body=%s, want 503 与统一提示", req.method, req.path, w.Code, w.Body.String())
				}
			}
			if got := up.pool.requestCount(); got != 0 {
				t.Fatalf("没有可用账号时访问了上游 %d 次", got)
			}
		})
	}
}

// TestManagementDeviceLoginAddsAccount 设备登录：先为 pending，之后成功并写入凭证文件、加入账号列表。
func TestManagementDeviceLoginAddsAccount(t *testing.T) {
	up := newMgmtUpstream(t)
	up.setDeviceLogin("oauth-dave", "dave")
	p, dir := mgmtNewPool(t, up, 0)

	w := mgmtCall(p, "POST", "/v0/management/login", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("发起登录 status=%d body=%s", w.Code, w.Body.String())
	}
	var start mgmtLoginStart
	if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if !mgmtIsHex(start.State, 32) || start.URL != "https://github.com/login/device" || start.UserCode != "ABCD-EFGH" ||
		start.Interval <= 0 || start.ExpiresIn <= 0 {
		t.Fatalf("登录参数 = %+v", start)
	}
	// 设备轮询需要等待 interval 之后才会完成，因此刚发起时应为 pending。
	if res := mgmtCall(p, "GET", "/v0/management/login/status?state="+start.State, "", mgmtKey); !strings.Contains(res.Body.String(), `"pending"`) {
		t.Fatalf("刚发起时状态 = %s, want pending", res.Body.String())
	}

	result := mgmtPollLogin(t, p, start.State)
	if result.Status != "success" || result.ID != "dave" {
		t.Fatalf("登录结果 = %+v, want success 且 id=dave", result)
	}
	if c, err := LoadCredential(filepath.Join(dir, "dave.json")); err != nil || c.Token != "oauth-dave" {
		t.Fatalf("登录凭证 = %+v, %v", c, err)
	}
	mgmtAccountByID(t, mgmtAccounts(t, p), "dave")
}

// TestManagementDeviceLoginFailureAndUnknownState 未知 state 返回 404；授权被拒绝时状态为 error，且不加入账号。
func TestManagementDeviceLoginFailureAndUnknownState(t *testing.T) {
	up := newMgmtUpstream(t)
	up.denyDeviceLogin()
	p, dir := mgmtNewPool(t, up, 0)

	if w := mgmtCall(p, "GET", "/v0/management/login/status?state=missing", "", mgmtKey); w.Code != http.StatusNotFound {
		t.Fatalf("未知 state status = %d, want 404", w.Code)
	}
	w := mgmtCall(p, "POST", "/v0/management/login", "", mgmtKey)
	var start mgmtLoginStart
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &start) != nil {
		t.Fatalf("发起登录 status=%d body=%s", w.Code, w.Body.String())
	}
	result := mgmtPollLogin(t, p, start.State)
	if result.Status != "error" || result.Error == "" {
		t.Fatalf("授权被拒绝时结果 = %+v, want status=error 且带错误信息", result)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("失败的登录写入了凭证目录: %d 个条目", len(entries))
	}
	if got := len(mgmtAccounts(t, p)); got != 0 {
		t.Fatalf("失败的登录加入了 %d 个账号", got)
	}
}
