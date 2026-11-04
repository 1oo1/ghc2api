package copilot

// 本文件覆盖 Pool.ApplySettings 的语义：服务密钥与管理密钥切换、生成请求并发上限的调低/调高/取消、
// 冷却时长、每账号绑定上限、模型缓存有效期与额度刷新间隔的热更新，以及新增账号继承当前值、并发调用无数据竞争。
// 断言尽量通过可观察行为（HTTP 状态、冷却截止时间、上游请求次数、选择结果）完成；
// 只有“脱敏列表”与“新账号的密钥”直接读取 Service 的已有字段。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	settingsNewAPIKey  = "settings-new-api-key"
	settingsNewMgmtKey = "settings-new-management-key"
)

// settingsCall 用预先构造的 handler 发送请求（模拟已在运行的服务），key 作为 Bearer 原样发送，返回状态码与响应体。
func settingsCall(h http.Handler, method, path, body, key string) (int, []byte) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

// settingsAuthorized 报告 key 是否通过业务接口鉴权（/healthz 免鉴权，故用 /v1/models：失败为 401，通过后不会是 401）。
func settingsAuthorized(h http.Handler, key string) bool {
	code, _ := settingsCall(h, http.MethodGet, "/v1/models", "", key)
	return code != http.StatusUnauthorized
}

// settingsHasSecret 报告 Service 的脱敏列表是否包含 value。
func settingsHasSecret(s *Service, value string) bool {
	s.secretsMu.RLock()
	defer s.secretsMu.RUnlock()
	_, ok := s.secrets[value]
	return ok
}

// TestApplySettingsAPIKey 切换 api_key：已构造的 handler 立即只接受新密钥（首尾空白被去除），
// 新密钥进入所有账号的脱敏列表；空值被忽略，保持当前值。
func TestApplySettingsAPIKey(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice", "bob")
	h := p.Handler()

	if !settingsAuthorized(h, testAPIKey) {
		t.Fatal("切换前原密钥应通过鉴权")
	}
	p.ApplySettings(RuntimeSettings{APIKey: "  " + settingsNewAPIKey + "\n"})
	if settingsAuthorized(h, testAPIKey) {
		t.Error("切换后旧密钥应返回 401")
	}
	if !settingsAuthorized(h, settingsNewAPIKey) {
		t.Error("切换后新密钥应通过鉴权")
	}
	for _, a := range p.accountsSnapshot() {
		if !settingsHasSecret(a.service, settingsNewAPIKey) {
			t.Errorf("账号 %s 的脱敏列表缺少新 api_key", a.id)
		}
	}

	// 空值（含纯空白）被忽略。
	for _, empty := range []string{"", "   "} {
		p.ApplySettings(RuntimeSettings{APIKey: empty})
		if !settingsAuthorized(h, settingsNewAPIKey) || settingsAuthorized(h, testAPIKey) {
			t.Errorf("APIKey=%q 应被忽略：新密钥应继续有效、旧密钥应继续无效", empty)
		}
	}
}

// TestApplySettingsManagementKey 切换 management_key：旧密钥不能再换令牌，旧密钥签发的令牌立即失效，
// 新密钥可以换令牌且令牌用新密钥签名；空值被忽略。
func TestApplySettingsManagementKey(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice")
	h := p.Handler()
	oldToken := consoleTokenFor(mgmtKey)
	if code, _ := settingsCall(h, http.MethodGet, "/v0/management/accounts", "", oldToken); code != http.StatusOK {
		t.Fatalf("切换前旧令牌状态 = %d, want 200", code)
	}

	p.ApplySettings(RuntimeSettings{ManagementKey: " " + settingsNewMgmtKey + " "})
	if code, _ := settingsCall(h, http.MethodPost, managementSessionPath, "", mgmtKey); code != http.StatusUnauthorized {
		t.Errorf("旧管理密钥换令牌状态 = %d, want 401", code)
	}
	if code, _ := settingsCall(h, http.MethodGet, "/v0/management/accounts", "", oldToken); code != http.StatusUnauthorized {
		t.Errorf("旧令牌状态 = %d, want 401", code)
	}
	code, body := settingsCall(h, http.MethodPost, managementSessionPath, "", settingsNewMgmtKey)
	if code != http.StatusOK {
		t.Fatalf("新管理密钥换令牌状态 = %d, want 200; body=%s", code, body)
	}
	var session mgmtConsoleSession
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatal(err)
	}
	if !verifyConsoleToken(settingsNewMgmtKey, session.Token, time.Now()) {
		t.Error("新令牌应由新管理密钥签名")
	}
	if code, _ := settingsCall(h, http.MethodGet, "/v0/management/accounts", "", session.Token); code != http.StatusOK {
		t.Errorf("新令牌访问管理接口状态 = %d, want 200", code)
	}

	// 空值被忽略：新令牌继续有效，旧密钥继续无效。
	p.ApplySettings(RuntimeSettings{ManagementKey: ""})
	if code, _ := settingsCall(h, http.MethodGet, "/v0/management/accounts", "", session.Token); code != http.StatusOK {
		t.Errorf("空 ManagementKey 后新令牌状态 = %d, want 200", code)
	}
	if code, _ := settingsCall(h, http.MethodPost, managementSessionPath, "", mgmtKey); code != http.StatusUnauthorized {
		t.Errorf("空 ManagementKey 后旧密钥状态 = %d, want 401", code)
	}
}

// settingsLimit 只修改并发上限（其余字段取忽略/默认值）。
func settingsLimit(p *Pool, n int) { p.ApplySettings(RuntimeSettings{MaxConcurrentRequests: n}) }

// settingsStartHeld 在后台发起一个生成请求，等它到达上游（占用一个名额）后返回结果通道。
func settingsStartHeld(t *testing.T, p *Pool, up *heldUpstream) chan *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- poolRequest(p, http.MethodPost, "/v1/chat/completions", concurrencyChatBody) }()
	up.awaitArrival(t)
	return done
}

// settingsAssertRejected 断言新的生成请求因并发名额已满得到 503。
func settingsAssertRejected(t *testing.T, p *Pool, when string) {
	t.Helper()
	// 若限制未生效，请求会被上游阻塞：放到协程里并设超时，避免测试挂死（清理函数会释放上游）。
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- poolRequest(p, http.MethodPost, "/v1/chat/completions", concurrencyChatBody) }()
	select {
	case w := <-done:
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "并发") {
			t.Fatalf("%s：status = %d, want 503（并发）; body=%s", when, w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s：请求没有被拒绝（被放行并阻塞在上游），期望立即 503", when)
	}
}

// settingsAwaitOK 等待后台请求结束并断言 200。
func settingsAwaitOK(t *testing.T, done chan *httptest.ResponseRecorder, what string) {
	t.Helper()
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Errorf("%s：status = %d, want 200; body=%s", what, w.Code, w.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s：请求未结束", what)
	}
}

// TestApplySettingsRaisesConcurrencyLimit 上限 1 且名额被占满时：调高到 2 立即放行一个，再满后 503；改为 0（不限制）后继续放行。
func TestApplySettingsRaisesConcurrencyLimit(t *testing.T) {
	up := newHeldUpstream(t)
	p := newLimitedPool(t, up.client, 1)

	r1 := settingsStartHeld(t, p, up)
	settingsAssertRejected(t, p, "上限 1、在途 1")
	settingsLimit(p, 2)
	r2 := settingsStartHeld(t, p, up)
	settingsAssertRejected(t, p, "上限 2、在途 2")
	settingsLimit(p, 0)
	r3 := settingsStartHeld(t, p, up)
	r4 := settingsStartHeld(t, p, up)

	up.releaseAll()
	for i, r := range []chan *httptest.ResponseRecorder{r1, r2, r3, r4} {
		settingsAwaitOK(t, r, "请求 "+string(rune('1'+i)))
	}
}

// TestApplySettingsLowersConcurrencyLimit 调低上限不中断在途请求：上限 3、在途 2 时调为 1，新请求 503，
// 在途数仍 >= 上限（即使调为 2）时继续 503；在途请求全部正常结束后恢复放行。
func TestApplySettingsLowersConcurrencyLimit(t *testing.T) {
	up := newHeldUpstream(t)
	p := newLimitedPool(t, up.client, 3)

	r1 := settingsStartHeld(t, p, up)
	r2 := settingsStartHeld(t, p, up)
	settingsLimit(p, 1)
	settingsAssertRejected(t, p, "调低到 1、在途 2")
	settingsLimit(p, 2)
	settingsAssertRejected(t, p, "调到 2、在途 2")

	up.releaseAll()
	settingsAwaitOK(t, r1, "在途请求 1")
	settingsAwaitOK(t, r2, "在途请求 2")
	if w := poolRequest(p, http.MethodPost, "/v1/chat/completions", concurrencyChatBody); w.Code != http.StatusOK {
		t.Fatalf("在途请求结束后 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestApplySettingsAccountCooldown 新进入的冷却使用新时长，已在冷却中的账号截止时间不变。
func TestApplySettingsAccountCooldown(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.config.AccountCooldown = 30 * time.Minute
	f.accounts[0].Priority = 10 // a 总是先被选中
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()

	before := time.Now()
	cooldownChat(p)
	after := time.Now()
	a := cooldownView(t, p, "a")
	if a.CooldownUntil == nil {
		t.Fatal("前提不成立：401 后 a 应冷却")
	}
	cooldownAssertWindow(t, *a.CooldownUntil, before, after, 30*time.Minute)

	p.ApplySettings(RuntimeSettings{AccountCooldown: 5 * time.Minute})
	cooldownSetGeneration(f.up, "oauth-b", http.StatusUnauthorized)
	before = time.Now()
	cooldownChat(p) // a 冷却中，选中 b 并使其冷却
	after = time.Now()
	b := cooldownView(t, p, "b")
	if b.CooldownUntil == nil {
		t.Fatal("前提不成立：401 后 b 应冷却")
	}
	cooldownAssertWindow(t, *b.CooldownUntil, before, after, 5*time.Minute)
	if again := cooldownView(t, p, "a"); again.CooldownUntil == nil || !again.CooldownUntil.Equal(*a.CooldownUntil) {
		t.Errorf("已有冷却的截止时间应不变: %v -> %v", a.CooldownUntil, again.CooldownUntil)
	}
}

// TestApplySettingsSessionAffinityMaxPerAccount 绑定上限立即作用于之后的选择：alice 已有一条绑定，
// 上限 1 时新会话选 bob；调为 0（不限制）后新会话回到轮询第一位 alice；反向亦然。
func TestApplySettingsSessionAffinityMaxPerAccount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial int
		apply   int
		want    string
	}{
		{name: "调低为 1 后 alice 已满", initial: 0, apply: 1, want: "bob"},
		{name: "调为 0 后不再限制", initial: 1, apply: 0, want: "alice"},
		{name: "负值视为不限制", initial: 1, apply: -3, want: "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newCapPool(t, tc.initial, priorityAccount("alice", 0), priorityAccount("bob", 0))
			p.affinity.bind("old", "alice", time.Now())
			p.ApplySettings(RuntimeSettings{SessionAffinityMaxPerAccount: tc.apply})
			if got := userOf(p.choose("new", nil)); got != tc.want {
				t.Fatalf("choose = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestApplySettingsModelsCacheTTL 调小模型缓存有效期后，已缓存的模型列表过期并重新请求上游。
func TestApplySettingsModelsCacheTTL(t *testing.T) {
	up := newPoolUpstream(t)
	var modelCalls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			modelCalls.Add(1)
		}
		up.serve(w, r)
	})
	p, err := NewManagedPool(PoolConfig{
		APIKey: testAPIKey, CacheDir: t.TempDir(), Client: client, ModelsCacheTTL: time.Hour,
	}, []PoolAccount{priorityAccount("alice", 0)})
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)

	if w := poolRequest(p, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("首次 /v1/models status = %d; body=%s", w.Code, w.Body.String())
	}
	base := modelCalls.Load()
	if base == 0 {
		t.Fatal("首次请求应访问上游 /models")
	}
	poolRequest(p, http.MethodGet, "/v1/models", "")
	if got := modelCalls.Load(); got != base {
		t.Fatalf("缓存有效期 1h 内不应重复请求上游: %d -> %d", base, got)
	}

	p.ApplySettings(RuntimeSettings{ModelsCacheTTL: 20 * time.Millisecond})
	time.Sleep(80 * time.Millisecond)
	poolRequest(p, http.MethodGet, "/v1/models", "")
	quotaWaitFor(t, "调小有效期后重新请求 /models", func() bool { return modelCalls.Load() > base })
}

// TestApplySettingsQuotaRefreshInterval 额度刷新间隔立即作用于现有账号；非正值使用默认间隔。
func TestApplySettingsQuotaRefreshInterval(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply time.Duration
		want  time.Duration // 生效的节流窗口
	}{
		{name: "调大到 30m", apply: 30 * time.Minute, want: 30 * time.Minute},
		{name: "非正值使用默认值", apply: 0, want: DefaultQuotaRefreshInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			p, err := NewManagedPool(PoolConfig{
				APIKey: testAPIKey, CacheDir: t.TempDir(), Client: up.client, QuotaRefreshInterval: 10 * time.Minute,
			}, []PoolAccount{priorityAccount("alice", 0)})
			if err != nil {
				t.Fatal(err)
			}
			quotaAwaitIdle(t, p)
			p.ApplySettings(RuntimeSettings{QuotaRefreshInterval: tc.apply})

			a := p.findAccount("alice")
			ctx := context.Background()
			if err := a.refresh(ctx, false); err != nil {
				t.Fatal(err)
			}
			if got := up.quotaCount("oauth-alice"); got != 1 {
				t.Fatalf("首次刷新后额度请求次数 = %d, want 1", got)
			}
			quotaSetCheckedAt(a, time.Now().Add(-(tc.want - time.Minute)))
			_ = a.refresh(ctx, false)
			if got := up.quotaCount("oauth-alice"); got != 1 {
				t.Fatalf("距上次尝试 %s 时不应请求上游，次数 = %d, want 1", tc.want-time.Minute, got)
			}
			quotaSetCheckedAt(a, time.Now().Add(-(tc.want + time.Minute)))
			_ = a.refresh(ctx, false)
			if got := up.quotaCount("oauth-alice"); got != 2 {
				t.Fatalf("距上次尝试 %s 时应请求上游，次数 = %d, want 2", tc.want+time.Minute, got)
			}
		})
	}
}

// TestApplySettingsAppliesToAccountsAddedLater 应用之后新增的账号使用新的 api_key（含脱敏）与新的额度刷新间隔。
func TestApplySettingsAppliesToAccountsAddedLater(t *testing.T) {
	up := newMgmtUpstream(t)
	up.setLogin("oauth-dave", "dave")
	config := mgmtPoolConfig(t, up, t.TempDir(), 0)
	config.QuotaRefreshInterval = 10 * time.Minute
	p, err := NewManagedPool(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)
	p.ApplySettings(RuntimeSettings{APIKey: settingsNewAPIKey, QuotaRefreshInterval: 30 * time.Minute})

	body := `{"credential":{"oauth_token":"oauth-` + "dave" + `"}}`
	if w := mgmtCall(p, http.MethodPost, "/v0/management/accounts", body, mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("导入 status=%d body=%s", w.Code, w.Body.String())
	}
	dave := p.findAccount("dave")
	if dave == nil {
		t.Fatal("导入后缺少 dave")
	}
	if got := dave.service.config.APIKey; got != settingsNewAPIKey {
		t.Errorf("新账号的 api_key = %q, want %q", got, settingsNewAPIKey)
	}
	if !settingsHasSecret(dave.service, settingsNewAPIKey) {
		t.Error("新账号的脱敏列表缺少新 api_key")
	}

	quotaWaitAttempt(t, dave, time.Time{})
	before := up.quotaCount("oauth-dave")
	quotaSetCheckedAt(dave, time.Now().Add(-20*time.Minute))
	mgmtAccounts(t, p)
	if got := up.quotaCount("oauth-dave"); got != before {
		t.Fatalf("距上次尝试 20 分钟、间隔 30m 时不应请求上游: %d -> %d", before, got)
	}
	quotaSetCheckedAt(dave, time.Now().Add(-31*time.Minute))
	mgmtAccounts(t, p)
	if got := up.quotaCount("oauth-dave"); got != before+1 {
		t.Fatalf("距上次尝试 31 分钟时额度请求次数 = %d, want %d", got, before+1)
	}
}

// TestApplySettingsConcurrentWithRequests 并发调用 ApplySettings 与业务请求、管理请求：在 -race 下无数据竞争，
// 且请求只会得到成功或并发名额已满的 503（密钥始终设为当前值，因此不应出现 401）。
func TestApplySettingsConcurrentWithRequests(t *testing.T) {
	up := newPoolUpstream(t)
	p := newLimitedPool(t, up.client(), 4)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			p.ApplySettings(RuntimeSettings{
				APIKey: testAPIKey, ManagementKey: securityMgmtKey,
				ModelsCacheTTL: time.Duration(i%3+1) * time.Hour, QuotaRefreshInterval: time.Duration(i%3+1) * time.Hour,
				AccountCooldown: time.Duration(i%3+1) * time.Hour, SessionAffinityMaxPerAccount: i % 4,
				MaxConcurrentRequests: i % 5,
			})
		}
	}()
	var requests sync.WaitGroup
	for range 4 {
		requests.Add(1)
		go func() {
			defer requests.Done()
			for range 25 {
				w := poolRequest(p, http.MethodPost, "/v1/chat/completions", concurrencyChatBody)
				if w.Code != http.StatusOK && w.Code != http.StatusServiceUnavailable {
					t.Errorf("生成请求 status = %d; body=%s", w.Code, w.Body.String())
				}
				if w := mgmtCall(p, http.MethodGet, "/v0/management/accounts", "", securityMgmtKey); w.Code != http.StatusOK {
					t.Errorf("管理请求 status = %d; body=%s", w.Code, w.Body.String())
				}
			}
		}()
	}
	requests.Wait()
	close(stop)
	wg.Wait()
}
