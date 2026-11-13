package copilot

// 本文件是 F4（账号池按模型选号）与 R3（模型列表逐个尝试）测试的公共部分，以及 Pool 层的行为测试。
// 测试通过 HTTP 入口与假上游观察行为，只依赖 (*Service).modelAvailability 这一个内部名字（见 model_availability_test.go）。
//
// 期望行为（见 docs/pool.md）：
//   - 生成请求在选号前取请求体的 model；账号的模型列表已加载（known）且不含该模型（!listed）时，本次选择中视为排除：
//     亲和绑定的账号被排除时不使用该绑定并改绑；分档、SWRR 只在未被排除的账号中计算；
//     排除后选不出账号时去掉排除再选一次（旧行为，交给上游返回 400）；列表未知的账号不被排除；未开启亲和时同样过滤；
//   - GET /models、/v1/models 按生成请求同样的规则（无会话键、无模型过滤：分档 + SWRR，排除禁用、冷却、耗尽与本请求已试过的账号）
//     选一个账号，由它答复：2xx 时把它的响应原样写给客户端并结束，其他账号不被访问；否则换下一个账号，
//     全部失败时原样写出最后一次尝试的失败响应；一个账号都选不出时 503；客户端取消时停止尝试且不写响应。
//     每次尝试计该账号一次请求，统计、连续错误与冷却规则同其他请求；没有会话键，不产生亲和绑定。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// modelsUpstream 在 poolUpstream 之上让每个账号的 /models 各不相同：按 OAuth token 返回各自登记的列表，
// 并记录每个账号的 /models 请求次数；session 交换与生成请求仍由 pool 处理（记录生成请求所属账号）。
type modelsUpstream struct {
	t    *testing.T
	pool *poolUpstream

	mu       sync.Mutex
	lists    map[string]string      // OAuth token -> /models 正文；未登记时返回空列表
	failures map[string]poolFailure // OAuth token -> /models 返回的失败
	calls    map[string]int         // OAuth token -> /models 请求次数
	hooks    map[string]func()      // OAuth token -> /models 请求到达（已计数）时调用，可阻塞
}

func newModelsUpstream(t *testing.T) *modelsUpstream {
	t.Helper()
	return &modelsUpstream{
		t: t, pool: newPoolUpstream(t),
		lists: map[string]string{}, failures: map[string]poolFailure{}, calls: map[string]int{}, hooks: map[string]func(){},
	}
}

func (u *modelsUpstream) client() *http.Client { return testClient(u.t, u.serve) }

func (u *modelsUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/models" {
		u.pool.serve(w, r)
		return
	}
	account := strings.TrimPrefix(r.Header.Get("Authorization"), poolBearerPrefix)
	u.mu.Lock()
	u.calls[account]++
	body, listed := u.lists[account]
	failure, failing := u.failures[account]
	hook := u.hooks[account]
	u.mu.Unlock()
	if hook != nil {
		hook()
	}
	if failing {
		w.WriteHeader(failure.status)
		_, _ = io.WriteString(w, failure.body)
		return
	}
	if !listed {
		body = `{"data":[]}`
	}
	_, _ = io.WriteString(w, body)
}

// setModels 登记用户 user 的账号（OAuth token 为 "oauth-" + user）的 /models 正文，entries 是各模型的 JSON。
func (u *modelsUpstream) setModels(user string, entries ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lists["oauth-"+user] = `{"data":[` + strings.Join(entries, ",") + `]}`
}

func (u *modelsUpstream) failModels(user string, failure poolFailure) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failures["oauth-"+user] = failure
}

func (u *modelsUpstream) hookModels(user string, hook func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.hooks["oauth-"+user] = hook
}

func (u *modelsUpstream) modelsCalls(user string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls["oauth-"+user]
}

// modelEntry 返回一个可见的 chat 模型（支持三种生成路径）；name 用来区分不同账号里同 id 的条目。
func modelEntry(id, name string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"model_picker_enabled":true,"capabilities":{"type":"chat"},`+
		`"supported_endpoints":["/chat/completions","/responses","/v1/messages"],"policy":{"state":"enabled"}}`, id, name)
}

// modelEntries 为每个 id 返回 name 等于 id 的模型条目。
func modelEntries(ids ...string) []string {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, modelEntry(id, id))
	}
	return entries
}

// newModelsPool 用 NewManagedPool 创建使用 up 的池，账号按传入顺序加入（即 Pool.accounts 的顺序）；
// affinityTTL 为 0 时不开启会话亲和。
func newModelsPool(t *testing.T, up *modelsUpstream, affinityTTL time.Duration, accounts ...PoolAccount) *Pool {
	t.Helper()
	p, err := NewManagedPool(PoolConfig{
		APIKey: testAPIKey, CacheDir: t.TempDir(), Client: up.client(), SessionAffinityTTL: affinityTTL,
	}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)
	t.Cleanup(p.WaitCooldownEffects) // 清理先于 TempDir 的删除完成
	return p
}

// warmModels 让池内每个账号各自加载一次模型列表。它直接请求各账号的 Service，不经过 Pool：
// 不依赖并集功能，也不计入账号的请求统计。
func warmModels(t *testing.T, p *Pool) {
	t.Helper()
	for _, account := range p.accountsSnapshot() {
		warmAccount(t, account)
	}
}

func warmAccount(t *testing.T, account *poolAccount) {
	t.Helper()
	if w := testRequest(account.service, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("预热 %s 的模型列表 status=%d body=%s", account.id, w.Code, w.Body.String())
	}
}

// modelsChat 经 Pool 发送一次 Chat Completions 请求；session 非空时带显式会话 ID（开启亲和的池需要会话键）。
func modelsChat(p *Pool, model, session string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]`, model)
	if session != "" {
		body += fmt.Sprintf(`,"session_id":%q`, session)
	}
	return poolRequest(p, http.MethodPost, "/v1/chat/completions", body+"}")
}

// modelsChatBounded 与 modelsChat 相同，但请求超过 timeout 仍未返回时取消请求并终止测试：
// 用于可能因反复重选同一个失败账号而挂起的场景（取消使循环退出，不会留下失控的协程）。
func modelsChatBounded(t *testing.T, p *Pool, model string, timeout time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		withTestAPIKey(p.Handler()).ServeHTTP(w, r)
	}()
	select {
	case <-done:
		return w
	case <-time.After(timeout):
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
		t.Fatalf("请求 %s 内没有返回（可能反复重选了同一个 session 失败的账号）", timeout)
		return nil
	}
}

// modelsServed 断言生成请求按顺序由这些用户名的账号处理。
// 不传用户名表示没有任何生成请求到达上游。
func modelsServed(t *testing.T, up *modelsUpstream, users ...string) {
	t.Helper()
	if len(users) == 0 {
		if got := up.pool.generationOrder(); len(got) != 0 {
			t.Fatalf("不应有生成请求到达上游，实际由 %v 处理", got)
		}
		return
	}
	assertGenerationOrder(t, up.pool, mgmtOrder(users...)...)
}

// modelsRequests 返回账号经 Pool 处理的请求数与错误数。
func modelsRequests(t *testing.T, p *Pool, id string) (requests, errs uint64) {
	t.Helper()
	view := cooldownView(t, p, id)
	return view.Stats.Requests, view.Stats.Errors
}

// modelsList 是 /v1/models 响应中本测试关心的字段。
type modelsList struct {
	Object string `json:"object"`
	Data   []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"data"`
}

func (l modelsList) ids() []string {
	ids := make([]string, 0, len(l.Data))
	for _, m := range l.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

func (l modelsList) nameOf(id string) string {
	for _, m := range l.Data {
		if m.ID == id {
			return m.Name
		}
	}
	return ""
}

// modelsGet 经 Pool 请求模型列表并解析；状态不是 200 时终止测试。
func modelsGet(t *testing.T, p *Pool, path string) modelsList {
	t.Helper()
	w := modelsGetRaw(t, p, path)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s, want 200", path, w.Code, w.Body.String())
	}
	var list modelsList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("模型列表不是合法 JSON: %v; body=%s", err, w.Body.String())
	}
	return list
}

func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("模型 id = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("模型 id = %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 生成请求选号
// ---------------------------------------------------------------------------

// TestPoolGenerationGoesOnlyToAccountsListingTheModel 未开启亲和时同样按模型过滤：列表已知的账号里，
// 只有含该模型的账号处理请求；两个账号都含该模型时仍按 SWRR 轮流。
func TestPoolGenerationGoesOnlyToAccountsListingTheModel(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("alice", modelEntries("m-all", "m-alice")...)
	up.setModels("bob", modelEntries("m-all", "m-bob")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
	warmModels(t, p)

	models := []string{"m-bob", "m-bob", "m-bob", "m-alice", "m-alice", "m-alice", "m-all", "m-all", "m-all", "m-all"}
	for i, model := range models {
		if w := modelsChat(p, model, ""); w.Code != http.StatusOK {
			t.Fatalf("请求 %d（%s）status=%d body=%s, want 200", i, model, w.Code, w.Body.String())
		}
	}
	// 只含 m-bob / m-alice 的账号各自独占；单成员的分组不改变 SWRR 状态，m-all 从 alice 开始轮流。
	modelsServed(t, up, "bob", "bob", "bob", "alice", "alice", "alice", "alice", "bob", "alice", "bob")
}

// TestPoolGenerationFilterAppliesBeforePriorityTier 分档只在未被排除的账号中计算：优先级最高的账号没有该模型时，
// 请求落到次一档含该模型的账号，而不是因最优档被排空而回退到旧行为。
func TestPoolGenerationFilterAppliesBeforePriorityTier(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("alice", modelEntries("m-all", "m-alice")...)
	up.setModels("bob", modelEntries("m-all", "m-bob")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 10), priorityAccount("bob", 0))
	warmModels(t, p)

	for i, model := range []string{"m-bob", "m-all", "m-alice"} {
		if w := modelsChat(p, model, ""); w.Code != http.StatusOK {
			t.Fatalf("请求 %d（%s）status=%d body=%s, want 200", i, model, w.Code, w.Body.String())
		}
	}
	modelsServed(t, up, "bob", "alice", "alice")
}

// TestPoolGenerationSkipsAccountThatRefusedTheModel 账号当前拒绝该模型（isRefused）时不算列出该模型，请求只落到另一个账号。
func TestPoolGenerationSkipsAccountThatRefusedTheModel(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("alice", modelEntries("m-all")...)
	up.setModels("bob", modelEntries("m-all")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
	warmModels(t, p)
	p.findAccount("alice").service.refuse("m-all")

	for i := range 3 {
		if w := modelsChat(p, "m-all", ""); w.Code != http.StatusOK {
			t.Fatalf("请求 %d status=%d body=%s, want 200", i, w.Code, w.Body.String())
		}
	}
	modelsServed(t, up, "bob", "bob", "bob")
}

// TestPoolGenerationFallsBackWhenNoAccountListsTheModel 所有可选账号的列表都已知且都没有该模型时去掉排除再选一次：
// 即旧行为，请求交给账号处理并得到 400（不是 503），账号仍按常规 SWRR 轮流，上游生成接口不被调用。
func TestPoolGenerationFallsBackWhenNoAccountListsTheModel(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("alice", modelEntries("m-alice")...)
	up.setModels("bob", modelEntries("m-bob")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
	warmModels(t, p)

	for i := range 2 {
		w := modelsChat(p, "m-none", "")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不存在或当前账号不可用") {
			t.Fatalf("请求 %d status=%d body=%s, want 400 模型不存在或当前账号不可用", i, w.Code, w.Body.String())
		}
	}
	modelsServed(t, up) // 没有生成请求到达上游
	for _, id := range []string{"alice", "bob"} {
		if requests, _ := modelsRequests(t, p, id); requests != 1 {
			t.Errorf("账号 %s 处理了 %d 个请求, want 1（回退后按 SWRR 轮流）", id, requests)
		}
	}
}

// TestPoolGenerationUnknownListsAreNotExcluded 列表未知（从未加载）的账号不被排除：alice 的列表已知且没有该模型，被排除；
// bob、carol 未加载，仍参与选择（先 bob 后 carol）。路由不为选号额外访问上游：每个账号的列表在它自己处理第一个请求时才加载。
func TestPoolGenerationUnknownListsAreNotExcluded(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("alice", modelEntries("m-alice")...)
	up.setModels("bob", modelEntries("m-bob")...)
	up.setModels("carol", modelEntries("m-bob")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0), priorityAccount("carol", 0))
	warmAccount(t, p.findAccount("alice"))

	if w := modelsChat(p, "m-bob", ""); w.Code != http.StatusOK {
		t.Fatalf("请求 0 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if got := up.modelsCalls("carol"); got != 0 {
		t.Fatalf("carol 没有被选中，其模型列表不应被访问，实际 %d 次", got)
	}
	if w := modelsChat(p, "m-bob", ""); w.Code != http.StatusOK {
		t.Fatalf("请求 1 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	modelsServed(t, up, "bob", "carol")
	for user, want := range map[string]int{"alice": 1, "bob": 1, "carol": 1} {
		if got := up.modelsCalls(user); got != want {
			t.Errorf("%s 的 /models 上游请求 %d 次, want %d", user, got, want)
		}
	}
}

// TestPoolGenerationLoadsListOnlyOnChosenAccount 全新的池收到第一个生成请求时，只有被选中的账号加载模型列表。
func TestPoolGenerationLoadsListOnlyOnChosenAccount(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("alice", modelEntries("m-all")...)
	up.setModels("bob", modelEntries("m-all")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
	if w := modelsChat(p, "m-all", ""); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	modelsServed(t, up, "alice")
	if a, b := up.modelsCalls("alice"), up.modelsCalls("bob"); a != 1 || b != 0 {
		t.Fatalf("/models 上游请求 alice=%d bob=%d, want 1 与 0", a, b)
	}
}

// TestPoolGenerationReadsModelFromBodyLeniently 选号读取的 model 与 Service 一致：键名大小写不敏感；
// 请求体非法、model 缺失或不是非空字符串时视为无模型、不过滤（按常规 SWRR 交给第一个账号 alice，由它返回 400）。
func TestPoolGenerationReadsModelFromBodyLeniently(t *testing.T) {
	newPool := func(t *testing.T) (*Pool, *modelsUpstream) {
		up := newModelsUpstream(t)
		up.setModels("alice", modelEntries("m-alice")...)
		up.setModels("bob", modelEntries("m-bob")...)
		p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
		warmModels(t, p)
		return p, up
	}

	t.Run("key-is-case-insensitive", func(t *testing.T) {
		p, up := newPool(t)
		w := poolRequest(p, http.MethodPost, "/v1/chat/completions", `{"MODEL":"m-bob","messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
		}
		modelsServed(t, up, "bob")
	})

	for _, tc := range []struct{ name, body string }{
		{"trailing-garbage", `{"model":"m-bob","messages":[]} x`},
		{"top-level-array", `[{"model":"m-bob"}]`},
		{"not-json", `model m-bob`},
		{"model-is-an-array", `{"model":["m-bob"],"messages":[]}`},
		{"model-is-a-number", `{"model":5,"messages":[]}`},
		{"model-is-null", `{"model":null,"messages":[]}`},
		{"model-is-empty", `{"model":"","messages":[]}`},
		{"model-missing", `{"messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, up := newPool(t)
			w := poolRequest(p, http.MethodPost, "/v1/chat/completions", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", w.Code, w.Body.String())
			}
			if requests, _ := modelsRequests(t, p, "alice"); requests != 1 {
				t.Errorf("alice 处理了 %d 个请求, want 1（无模型时不过滤，SWRR 先选 alice）", requests)
			}
			if requests, _ := modelsRequests(t, p, "bob"); requests != 0 {
				t.Errorf("bob 处理了 %d 个请求, want 0", requests)
			}
			modelsServed(t, up)
		})
	}
}

// TestPoolGenerationSkipsAccountWhereModelIsDisabledByPolicy 账号对某模型的策略是禁用（无条款）时，该模型只在其 Endpoints 里留有路径记录，
// 不算该账号有这个模型：请求落到另一个可用的账号，而不是落到这个账号得到上游的 400。
func TestPoolGenerationSkipsAccountWhereModelIsDisabledByPolicy(t *testing.T) {
	const disabled = `{"id":"m-x","model_picker_enabled":false,"capabilities":{"type":"chat"},` +
		`"supported_endpoints":["/chat/completions"],"policy":{"state":"disabled"}}`

	t.Run("another-account-has-it", func(t *testing.T) {
		up := newModelsUpstream(t)
		up.setModels("alice", disabled, modelEntry("m-all", "m-all"))
		up.setModels("bob", modelEntry("m-x", "m-x"), modelEntry("m-all", "m-all"))
		// 没有该过滤时请求会落到 alice，上游拒绝该模型。
		up.pool.failGeneration("oauth-alice", poolFailure{http.StatusBadRequest, `{"error":{"code":"model_not_supported","message":"The requested model is not supported."}}`})
		p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
		warmModels(t, p)

		for i := range 3 {
			if w := modelsChat(p, "m-x", ""); w.Code != http.StatusOK {
				t.Fatalf("请求 %d status=%d body=%s, want 200", i, w.Code, w.Body.String())
			}
		}
		modelsServed(t, up, "bob", "bob", "bob")
		if requests, _ := modelsRequests(t, p, "alice"); requests != 0 {
			t.Errorf("alice 对 m-x 策略禁用，不应处理请求，实际 %d 个", requests)
		}
	})

	// 所有账号都被策略禁用时回退到旧行为：请求交给账号，手动指定隐藏模型的路径校验不变，仍转发给上游。
	t.Run("no-account-has-it-falls-back", func(t *testing.T) {
		up := newModelsUpstream(t)
		up.setModels("alice", disabled)
		up.setModels("bob", disabled)
		p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
		warmModels(t, p)

		if w := modelsChat(p, "m-x", ""); w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200（回退后由账号转发给上游）", w.Code, w.Body.String())
		}
		modelsServed(t, up, "alice")
	})
}

// TestPoolSessionFailureSwitchHonorsModelFilter session 失败后在同一请求内换号时，每次选择都按模型过滤：
// alice（最优档，含该模型）的 session 失败后，改选 carol 而不是同档但没有该模型的 bob。
func TestPoolSessionFailureSwitchHonorsModelFilter(t *testing.T) {
	up := newModelsUpstream(t)
	up.pool.setSessionTTL(time.Minute) // 会话很快过期，使 session 失败在下一次请求时生效
	up.setModels("alice", modelEntries("m-x")...)
	up.setModels("bob", modelEntries("m-other")...)
	up.setModels("carol", modelEntries("m-x")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 10), priorityAccount("bob", 0), priorityAccount("carol", 0))
	warmModels(t, p)
	up.pool.failSession("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"alice down"}`})
	aliceSessions, bobSessions := up.pool.sessionCount("oauth-alice"), up.pool.sessionCount("oauth-bob")

	if w := modelsChatBounded(t, p, "m-x", 10*time.Second); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200（alice 失败后改由 carol 处理）", w.Code, w.Body.String())
	}
	modelsServed(t, up, "carol")
	if got := up.pool.sessionCount("oauth-alice"); got != aliceSessions+1 {
		t.Errorf("session 失败的 alice 同一请求内只应被尝试一次: session 交换 %d -> %d", aliceSessions, got)
	}
	if got := up.pool.sessionCount("oauth-bob"); got != bobSessions {
		t.Errorf("没有该模型的 bob 不应被尝试: session 交换 %d -> %d", bobSessions, got)
	}
}

// TestPoolSessionFailureWithoutCooldownIsNotRetriedInSameRequest session 失败的状态不计入冷却（如 400）时，
// 失败的账号也必须计入本次请求的已尝试集合：同一请求内不得重选它（否则账号永不冷却，请求会一直重试而挂起）。
// 按模型过滤时排除集合是在已尝试集合之外另行构造的，这里 bob 被排除，正好经过那条路径。
func TestPoolSessionFailureWithoutCooldownIsNotRetriedInSameRequest(t *testing.T) {
	up := newModelsUpstream(t)
	up.pool.setSessionTTL(time.Minute) // 会话很快过期，使 session 失败在下一次请求时生效
	up.setModels("alice", modelEntries("m-x")...)
	up.setModels("bob", modelEntries("m-other")...)
	up.setModels("carol", modelEntries("m-x")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 10), priorityAccount("bob", 0), priorityAccount("carol", 0))
	warmModels(t, p)
	up.pool.failSession("oauth-alice", poolFailure{http.StatusBadRequest, `{"message":"alice rejected the exchange"}`})
	aliceSessions, bobSessions := up.pool.sessionCount("oauth-alice"), up.pool.sessionCount("oauth-bob")

	if w := modelsChatBounded(t, p, "m-x", 10*time.Second); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200（alice 失败后改由 carol 处理）", w.Code, w.Body.String())
	}
	modelsServed(t, up, "carol")
	if got := up.pool.sessionCount("oauth-alice"); got != aliceSessions+1 {
		t.Errorf("session 失败的 alice 同一请求内只应被尝试一次: session 交换 %d -> %d", aliceSessions, got)
	}
	if got := up.pool.sessionCount("oauth-bob"); got != bobSessions {
		t.Errorf("没有该模型的 bob 不应被尝试: session 交换 %d -> %d", bobSessions, got)
	}
	if view := cooldownView(t, p, "alice"); view.CooldownUntil != nil {
		t.Errorf("400 不计入冷却，alice 却冷却至 %s", view.CooldownUntil)
	}
}

// TestPoolExcludedAccountsAreStillUsableAfterFilteredOnesFail 被排除不等于已尝试：唯一含该模型的账号 session 失败后，
// 同一请求内去掉排除再选，改用被排除但未尝试的账号，由它返回 400；而不是直接返回 alice 的 session 错误。
func TestPoolExcludedAccountsAreStillUsableAfterFilteredOnesFail(t *testing.T) {
	up := newModelsUpstream(t)
	up.pool.setSessionTTL(time.Minute)
	up.setModels("alice", modelEntries("m-x")...)
	up.setModels("bob", modelEntries("m-other")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
	warmModels(t, p)
	up.pool.failSession("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"alice down"}`})
	bobSessions := up.pool.sessionCount("oauth-bob")

	w := modelsChat(p, "m-x", "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不存在或当前账号不可用") {
		t.Fatalf("status=%d body=%s, want 400 模型不存在或当前账号不可用（由回退选中的 bob 返回）", w.Code, w.Body.String())
	}
	if got := up.pool.sessionCount("oauth-bob"); got <= bobSessions {
		t.Errorf("bob 应在回退时被尝试: session 交换 %d -> %d", bobSessions, got)
	}
}

// TestPoolAffinityRebindsWhenBoundAccountLacksTheModel 开启亲和：会话绑定的账号没有该模型时，不使用该绑定，改选含该模型的账号并改绑；
// 之后同一会话继续命中新绑定（即使绑定账号并非 SWRR 首选）。
func TestPoolAffinityRebindsWhenBoundAccountLacksTheModel(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("alice", modelEntries("m-all", "m-alice")...)
	up.setModels("bob", modelEntries("m-all", "m-bob")...)
	p := newModelsPool(t, up, DefaultSessionAffinityTTL, priorityAccount("alice", 0), priorityAccount("bob", 0))
	warmModels(t, p)

	for i, step := range []struct{ model, session string }{
		{"m-alice", "s1"}, // 绑定到 alice
		{"m-bob", "s1"},   // 绑定的 alice 没有 m-bob：改选并改绑到 bob
		{"m-all", "s1"},   // 两个账号都有 m-all：命中新绑定 bob，而不是 SWRR 首选 alice
	} {
		if w := modelsChat(p, step.model, step.session); w.Code != http.StatusOK {
			t.Fatalf("步骤 %d（%s）status=%d body=%s, want 200", i, step.model, w.Code, w.Body.String())
		}
	}
	modelsServed(t, up, "alice", "bob", "bob")
}

// TestPoolAffinityFallbackKeepsTheBinding 开启亲和：所有账号都没有该模型而回退到旧行为时，旧行为包括会话绑定：
// 请求仍由绑定的账号 bob 处理（不是 SWRR 首选 alice），并返回 400。
func TestPoolAffinityFallbackKeepsTheBinding(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("alice", modelEntries("m-alice")...)
	up.setModels("bob", modelEntries("m-bob")...)
	p := newModelsPool(t, up, DefaultSessionAffinityTTL, priorityAccount("alice", 0), priorityAccount("bob", 0))
	warmModels(t, p)

	if w := modelsChat(p, "m-bob", "s1"); w.Code != http.StatusOK {
		t.Fatalf("绑定请求 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	w := modelsChat(p, "m-none", "s1")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不存在或当前账号不可用") {
		t.Fatalf("回退请求 status=%d body=%s, want 400 模型不存在或当前账号不可用", w.Code, w.Body.String())
	}
	if requests, _ := modelsRequests(t, p, "bob"); requests != 2 {
		t.Errorf("bob 处理了 %d 个请求, want 2（回退时仍用绑定）", requests)
	}
	if requests, _ := modelsRequests(t, p, "alice"); requests != 0 {
		t.Errorf("alice 处理了 %d 个请求, want 0", requests)
	}
}

// ---------------------------------------------------------------------------
// 模型列表逐个尝试（R3）：按生成请求同样的规则选一个账号，首个成功即返回
// ---------------------------------------------------------------------------

// newFailoverPool 返回 alice（优先级 10）、bob（5）、carol（0）三个账号的池，开启会话亲和。
// 优先级固定了尝试顺序 alice、bob、carol，不依赖 SWRR 的轮询状态。各账号登记各不相同的列表（共有的 shared 与自己独有的条目），
// 便于从响应判断是哪个账号答复的。
func newFailoverPool(t *testing.T) (*modelsUpstream, *Pool) {
	t.Helper()
	up := newModelsUpstream(t)
	for _, user := range []string{"alice", "bob", "carol"} {
		up.setModels(user, modelEntry("shared", "from-"+user), modelEntry(user[:1]+"-only", user[:1]+"-only"))
	}
	p := newModelsPool(t, up, DefaultSessionAffinityTTL,
		priorityAccount("alice", 10), priorityAccount("bob", 5), priorityAccount("carol", 0))
	return up, p
}

// modelsStreak 返回账号当前的连续错误计数。
func modelsStreak(p *Pool, id string) int {
	account := p.findAccount(id)
	account.errs.mu.Lock()
	defer account.errs.mu.Unlock()
	return account.errs.streak
}

// assertUntouched 断言这些用户的账号完全没有被访问：上游没有收到其 /models 与 session 请求，Pool 没有为其计请求或错误。
func assertUntouched(t *testing.T, p *Pool, up *modelsUpstream, users ...string) {
	t.Helper()
	for _, user := range users {
		if got := up.modelsCalls(user); got != 0 {
			t.Errorf("账号 %s 不应被访问，/models 上游请求 %d 次", user, got)
		}
		if got := up.pool.sessionCount("oauth-" + user); got != 0 {
			t.Errorf("账号 %s 不应被访问，session 交换 %d 次", user, got)
		}
		if requests, errs := modelsRequests(t, p, user); requests != 0 || errs != 0 {
			t.Errorf("账号 %s 不应被计数: requests=%d errors=%d", user, requests, errs)
		}
	}
}

// assertAttempts 断言账号的 /models 上游请求数 calls、Pool 统计的请求数 requests 与错误数 errs。
func assertAttempts(t *testing.T, p *Pool, up *modelsUpstream, user string, calls int, requests, errs uint64) {
	t.Helper()
	if got := up.modelsCalls(user); got != calls {
		t.Errorf("账号 %s 的 /models 上游请求 %d 次, want %d", user, got, calls)
	}
	if gotRequests, gotErrs := modelsRequests(t, p, user); gotRequests != requests || gotErrs != errs {
		t.Errorf("账号 %s requests=%d errors=%d, want %d 与 %d", user, gotRequests, gotErrs, requests, errs)
	}
}

// assertCooled 断言账号是否处于冷却，冷却时还核对原因。
func assertCooled(t *testing.T, p *Pool, user string, cooled bool, reason string) {
	t.Helper()
	view := cooldownView(t, p, user)
	if (view.CooldownUntil != nil) != cooled {
		t.Fatalf("账号 %s 冷却 = %v, want %v", user, view.CooldownUntil != nil, cooled)
	}
	if cooled && view.CooldownReason != reason {
		t.Errorf("账号 %s 冷却原因 = %q, want %q", user, view.CooldownReason, reason)
	}
}

// assertAffinityEmpty 断言没有任何亲和绑定。
func assertAffinityEmpty(t *testing.T, p *Pool) {
	t.Helper()
	if counts := p.affinity.activeCounts(time.Now()); len(counts) != 0 {
		t.Errorf("模型列表请求不应产生亲和绑定: %v", counts)
	}
}

// responseSpy 记录处理器是否向客户端写过响应（状态码或正文）。
type responseSpy struct {
	*httptest.ResponseRecorder
	wrote bool
}

func (s *responseSpy) WriteHeader(code int) {
	s.wrote = true
	s.ResponseRecorder.WriteHeader(code)
}

func (s *responseSpy) Write(data []byte) (int, error) {
	s.wrote = true
	return s.ResponseRecorder.Write(data)
}

// modelsServe 经 Pool 发送一次 GET path 请求，响应写入 w；parent 是客户端的上下文（取消它即模拟客户端断开）。
// 请求超过 5 秒仍未返回时取消请求并终止测试：逐个尝试的循环没有正确排除已试账号时会反复重选同一个失败账号而挂起，
// 取消使循环退出，不会留下失控的协程。
func modelsServe(t *testing.T, p *Pool, w http.ResponseWriter, path string, parent context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		withTestAPIKey(p.Handler()).ServeHTTP(w, r)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
		t.Fatalf("GET %s 5 秒内没有返回（可能反复重选了同一个失败账号）", path)
	}
}

// modelsGetRaw 经 Pool 请求模型列表，返回任意状态的响应。
func modelsGetRaw(t *testing.T, p *Pool, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	modelsServe(t, p, w, path, context.Background())
	return w
}

// TestPoolModelsReturnsFirstSuccessWithoutTouchingOthers 模型列表只由选中的一个账号答复：返回它的列表（不是并集），
// 状态码、响应头与正文与该账号自己处理的结果完全一致；其余账号不被访问，上游收不到它们的 /models 与 session 请求，也不计请求数。
// 池内顺序为 carol、bob、alice，选中 alice 是因为同权重时按凭证文件名升序，而不是账号顺序。
func TestPoolModelsReturnsFirstSuccessWithoutTouchingOthers(t *testing.T) {
	for name, path := range map[string]string{"v1-models": "/v1/models", "models": "/models"} {
		t.Run(name, func(t *testing.T) {
			up := newModelsUpstream(t)
			up.setModels("alice", modelEntry("shared", "from-alice"), modelEntry("a-only", "a-only"))
			up.setModels("bob", modelEntry("shared", "from-bob"), modelEntry("b-only", "b-only"))
			up.setModels("carol", modelEntry("c-only", "c-only"))
			p := newModelsPool(t, up, DefaultSessionAffinityTTL,
				priorityAccount("carol", 0), priorityAccount("bob", 0), priorityAccount("alice", 0))

			w := modelsGetRaw(t, p, path)
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d body=%s, want 200", path, w.Code, w.Body.String())
			}
			var list modelsList
			if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
				t.Fatalf("模型列表不是合法 JSON: %v; body=%s", err, w.Body.String())
			}
			if list.Object != "list" {
				t.Errorf("object = %q, want list", list.Object)
			}
			assertIDs(t, list.ids(), []string{"shared", "a-only"})
			if got := list.nameOf("shared"); got != "from-alice" {
				t.Errorf("shared 的 name = %q, want from-alice", got)
			}

			// 与 alice 自己处理同一请求的结果逐项一致（此时列表已缓存，不再访问上游）。
			direct := testRequest(p.findAccount("alice").service, http.MethodGet, path, "")
			if direct.Code != w.Code || direct.Body.String() != w.Body.String() {
				t.Errorf("响应与账号自己的结果不一致: status %d/%d body %q/%q", w.Code, direct.Code, w.Body.String(), direct.Body.String())
			}
			for key, want := range direct.Header() {
				if got := w.Header().Values(key); strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("响应头 %s = %v, want %v（与账号自己的结果一致）", key, got, want)
				}
			}

			assertAttempts(t, p, up, "alice", 1, 1, 0)
			assertUntouched(t, p, up, "bob", "carol")
			assertAffinityEmpty(t, p)
		})
	}
}

// TestPoolModelsFollowsTiersAndRotation 选号规则与生成请求相同（无会话键、无模型过滤）：高优先级档先于低档；
// 同档内按 SWRR 轮流，连续请求落到不同账号；并与生成请求共用轮询状态。
func TestPoolModelsFollowsTiersAndRotation(t *testing.T) {
	t.Run("rotates-within-the-same-tier", func(t *testing.T) {
		up := newModelsUpstream(t)
		for _, user := range []string{"alice", "bob", "carol"} {
			up.setModels(user, modelEntries(user[:1]+"-only")...)
		}
		p := newModelsPool(t, up, DefaultSessionAffinityTTL,
			priorityAccount("carol", 0), priorityAccount("bob", 0), priorityAccount("alice", 0))

		want := []string{"a-only", "b-only", "c-only", "a-only", "b-only", "c-only"}
		for i, id := range want {
			got := modelsGet(t, p, "/v1/models").ids()
			if len(got) != 1 || got[0] != id {
				t.Fatalf("第 %d 次请求的答复来自 %v, want [%s]（同档账号按 SWRR 轮流）", i+1, got, id)
			}
		}
		for _, user := range []string{"alice", "bob", "carol"} {
			assertAttempts(t, p, up, user, 1, 2, 0) // 每个账号答复 2 次，列表只在第一次访问上游
		}
		assertAffinityEmpty(t, p)
	})

	t.Run("higher-priority-tier-first", func(t *testing.T) {
		up := newModelsUpstream(t)
		for _, user := range []string{"alice", "bob", "carol"} {
			up.setModels(user, modelEntries(user[:1]+"-only")...)
		}
		p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 5), priorityAccount("carol", 0))

		for i := range 3 {
			if got := modelsGet(t, p, "/v1/models").ids(); len(got) != 1 || got[0] != "b-only" {
				t.Fatalf("第 %d 次请求的答复来自 %v, want [b-only]（最高优先级档只有 bob）", i+1, got)
			}
		}
		assertAttempts(t, p, up, "bob", 1, 3, 0)
		assertUntouched(t, p, up, "alice", "carol")
	})

	t.Run("failure-falls-to-the-same-tier-before-a-lower-tier", func(t *testing.T) {
		up := newModelsUpstream(t)
		for _, user := range []string{"alice", "bob", "carol"} {
			up.setModels(user, modelEntries(user[:1]+"-only")...)
		}
		up.failModels("alice", poolFailure{http.StatusInternalServerError, `{"error":"alice down"}`})
		p := newModelsPool(t, up, 0, priorityAccount("alice", 5), priorityAccount("bob", 5), priorityAccount("carol", 0))

		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"b-only"})
		assertAttempts(t, p, up, "alice", 1, 1, 1)
		assertAttempts(t, p, up, "bob", 1, 1, 0)
		assertUntouched(t, p, up, "carol")
	})

	t.Run("shares-rotation-state-with-generation-requests", func(t *testing.T) {
		up := newModelsUpstream(t)
		up.setModels("alice", modelEntry("shared", "shared"), modelEntry("a-only", "a-only"))
		up.setModels("bob", modelEntry("shared", "shared"), modelEntry("b-only", "b-only"))
		p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))

		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "a-only"})
		if w := modelsChat(p, "shared", ""); w.Code != http.StatusOK {
			t.Fatalf("生成请求 status=%d body=%s", w.Code, w.Body.String())
		}
		modelsServed(t, up, "bob") // 模型列表请求让 alice 的轮询值下降，下一次选号（生成请求）轮到 bob
		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "a-only"})
	})
}

// TestPoolModelsSkipsFailingAccount 选中的账号失败（上游错误或 session 失败）时换下一个账号并返回它的响应；
// 失败账号按原规则计数：每次尝试计一次请求，失败计入错误数与连续错误，401 立即冷却，连续 3 次 500 冷却，冷却后不再被尝试。
// 第三个账号在第二个账号成功后始终不被访问。
func TestPoolModelsSkipsFailingAccount(t *testing.T) {
	t.Run("upstream-error-is-counted-and-next-account-answers-sequentially", func(t *testing.T) {
		up, p := newFailoverPool(t)
		up.failModels("alice", poolFailure{http.StatusInternalServerError, `{"error":"alice down"}`})
		var bobSeenWhileAliceInFlight atomic.Int32
		up.hookModels("alice", func() {
			time.Sleep(100 * time.Millisecond)
			bobSeenWhileAliceInFlight.Store(int32(up.modelsCalls("bob")))
		})

		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
		if got := bobSeenWhileAliceInFlight.Load(); got != 0 {
			t.Errorf("alice 的请求尚未返回时 bob 已被访问 %d 次：应在 alice 失败后才尝试下一个账号", got)
		}
		assertAttempts(t, p, up, "alice", 1, 1, 1)
		assertAttempts(t, p, up, "bob", 1, 1, 0)
		assertUntouched(t, p, up, "carol")
		assertCooled(t, p, "alice", false, "")
		if got := modelsStreak(p, "alice"); got != 1 {
			t.Errorf("alice 的连续错误计数 = %d, want 1", got)
		}
		assertAffinityEmpty(t, p)
	})

	t.Run("non-2xx-of-any-kind-moves-on", func(t *testing.T) {
		up, p := newFailoverPool(t)
		up.failModels("alice", poolFailure{http.StatusNotFound, `{"error":"alice has no models"}`})

		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
		assertAttempts(t, p, up, "alice", 1, 1, 1)
		assertAttempts(t, p, up, "bob", 1, 1, 0)
		assertUntouched(t, p, up, "carol")
		assertCooled(t, p, "alice", false, "")
	})

	t.Run("three-consecutive-500s-cool-the-account-and-it-stops-being-tried", func(t *testing.T) {
		up, p := newFailoverPool(t)
		up.failModels("alice", poolFailure{http.StatusInternalServerError, `{"error":"alice down"}`})

		for i := 1; i <= 3; i++ {
			assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
			assertCooled(t, p, "alice", i == 3, "500") // 每次失败只计一次，连续第 3 次才冷却
		}
		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
		assertAttempts(t, p, up, "alice", 3, 3, 3) // 冷却后第 4 次请求不再尝试 alice
		assertAttempts(t, p, up, "bob", 1, 4, 0)
		assertUntouched(t, p, up, "carol")
	})

	t.Run("401-cools-immediately", func(t *testing.T) {
		up, p := newFailoverPool(t)
		up.failModels("alice", poolFailure{http.StatusUnauthorized, `{"error":"alice rejected"}`})

		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
		assertCooled(t, p, "alice", true, "401")
		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
		assertAttempts(t, p, up, "alice", 1, 1, 1) // 冷却后不再尝试
		assertAttempts(t, p, up, "bob", 1, 2, 0)
		assertUntouched(t, p, up, "carol")
	})

	t.Run("session-failure-is-counted-once-per-attempt", func(t *testing.T) {
		up, p := newFailoverPool(t)
		up.pool.failSession("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"alice down"}`})

		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
		if got := up.modelsCalls("alice"); got != 0 {
			t.Errorf("alice 的 session 失败，不应再请求其 /models: 上游收到 %d 次", got)
		}
		if got := up.pool.sessionCount("oauth-alice"); got != 1 {
			t.Errorf("alice 的 session 交换 %d 次, want 1", got)
		}
		if requests, errs := modelsRequests(t, p, "alice"); requests != 1 || errs != 1 {
			t.Errorf("alice requests=%d errors=%d, want 1 与 1", requests, errs)
		}
		if got := modelsStreak(p, "alice"); got != 1 {
			t.Errorf("alice 的连续错误计数 = %d, want 1（session 失败只计一次）", got)
		}
		assertCooled(t, p, "alice", false, "")

		for i := 2; i <= 3; i++ {
			assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
			assertCooled(t, p, "alice", i == 3, "500")
		}
		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
		if got := up.pool.sessionCount("oauth-alice"); got != 3 {
			t.Errorf("冷却后 alice 不应再被尝试: session 交换 %d 次, want 3", got)
		}
		assertAttempts(t, p, up, "bob", 1, 4, 0)
		assertUntouched(t, p, up, "carol")
		assertAffinityEmpty(t, p)
	})

	t.Run("session-401-cools-immediately", func(t *testing.T) {
		up, p := newFailoverPool(t)
		up.pool.failSession("oauth-alice", poolFailure{http.StatusUnauthorized, `{"message":"alice rejected"}`})

		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
		assertCooled(t, p, "alice", true, "401")
		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"shared", "b-only"})
		if got := up.pool.sessionCount("oauth-alice"); got != 1 {
			t.Errorf("冷却后 alice 不应再被尝试: session 交换 %d 次, want 1", got)
		}
		assertUntouched(t, p, up, "carol")
	})
}

// TestPoolModelsAllFailWritesLastFailingResponse 全部账号都失败时原样写出最后一次尝试的失败响应（状态码与正文）：
// 以尝试顺序而不是账号顺序为准。每个账号只被尝试一次，各计一次请求与一次错误。
func TestPoolModelsAllFailWritesLastFailingResponse(t *testing.T) {
	t.Run("upstream-errors", func(t *testing.T) {
		up := newModelsUpstream(t)
		up.failModels("alice", poolFailure{http.StatusBadGateway, `{"error":"alice down"}`})
		up.failModels("bob", poolFailure{http.StatusServiceUnavailable, `{"error":"bob busy"}`})
		up.failModels("carol", poolFailure{http.StatusInternalServerError, `{"error":"carol down"}`})
		// 尝试顺序为 carol、bob、alice（优先级从高到低），最后一次失败来自 alice，而账号顺序里最后的是 carol。
		p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 5), priorityAccount("carol", 10))

		w := modelsGetRaw(t, p, "/v1/models")
		if w.Code != http.StatusBadGateway || w.Body.String() != `{"error":"alice down"}` {
			t.Fatalf("status=%d body=%q, want 502 %q（最后一次尝试 alice 的响应）", w.Code, w.Body.String(), `{"error":"alice down"}`)
		}
		for _, user := range []string{"alice", "bob", "carol"} {
			assertAttempts(t, p, up, user, 1, 1, 1)
		}
	})

	t.Run("last-attempt-is-a-session-failure", func(t *testing.T) {
		up := newModelsUpstream(t)
		up.failModels("alice", poolFailure{http.StatusInternalServerError, `{"error":"alice models down"}`})
		up.pool.failSession("oauth-bob", poolFailure{http.StatusServiceUnavailable, `{"message":"bob session down"}`})
		// 池内顺序为 bob、alice，与尝试顺序（alice、bob）相反。
		p := newModelsPool(t, up, 0, priorityAccount("bob", 0), priorityAccount("alice", 5))

		w := modelsGetRaw(t, p, "/v1/models")
		if w.Code != http.StatusServiceUnavailable || w.Body.String() != `{"message":"bob session down"}` {
			t.Fatalf("status=%d body=%q, want 503 %q（最后一次尝试 bob 的 session 失败）", w.Code, w.Body.String(), `{"message":"bob session down"}`)
		}
		assertAttempts(t, p, up, "alice", 1, 1, 1)
		assertAttempts(t, p, up, "bob", 0, 1, 1)
	})

	t.Run("last-attempt-is-a-models-failure-after-a-session-failure", func(t *testing.T) {
		up := newModelsUpstream(t)
		up.pool.failSession("oauth-alice", poolFailure{http.StatusServiceUnavailable, `{"message":"alice session down"}`})
		up.failModels("bob", poolFailure{http.StatusBadGateway, `{"error":"bob models down"}`})
		// 池内顺序为 bob、alice，与尝试顺序（alice、bob）相反。
		p := newModelsPool(t, up, 0, priorityAccount("bob", 0), priorityAccount("alice", 5))

		w := modelsGetRaw(t, p, "/v1/models")
		if w.Code != http.StatusBadGateway || w.Body.String() != `{"error":"bob models down"}` {
			t.Fatalf("status=%d body=%q, want 502 %q（最后一次尝试 bob 的 /models 失败）", w.Code, w.Body.String(), `{"error":"bob models down"}`)
		}
		assertAttempts(t, p, up, "alice", 0, 1, 1)
		assertAttempts(t, p, up, "bob", 1, 1, 1)
	})
}

// TestPoolModelsNoSelectableAccountReturns503 一个账号都选不出（全部禁用、全部冷却、全部额度耗尽，或池里没有账号）时返回 503，
// 不访问上游，不计请求。
func TestPoolModelsNoSelectableAccountReturns503(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, up *modelsUpstream) *Pool
	}{
		{"all-disabled", func(t *testing.T, up *modelsUpstream) *Pool {
			alice, bob := priorityAccount("alice", 0), priorityAccount("bob", 0)
			alice.Disabled, bob.Disabled = true, true
			return newModelsPool(t, up, 0, alice, bob)
		}},
		{"all-cooling", func(t *testing.T, up *modelsUpstream) *Pool {
			p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
			for _, id := range []string{"alice", "bob"} {
				p.findAccount(id).service.setCooldown(time.Now().Add(time.Hour))
			}
			return p
		}},
		{"all-exhausted", func(t *testing.T, up *modelsUpstream) *Pool {
			p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
			for _, id := range []string{"alice", "bob"} {
				setQuota(t, p, id, exhaustedQuota(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
			}
			return p
		}},
		{"no-accounts", func(t *testing.T, up *modelsUpstream) *Pool {
			p, err := NewManagedPool(PoolConfig{
				APIKey: testAPIKey, ManagementKey: mgmtKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(), Client: up.client(),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(p.WaitCooldownEffects)
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newModelsUpstream(t)
			up.setModels("alice", modelEntries("a-only")...)
			up.setModels("bob", modelEntries("b-only")...)
			p := tc.setup(t, up)
			for _, path := range []string{"/v1/models", "/models"} {
				assertNoAccount(t, modelsGetRaw(t, p, path))
			}
			if tc.name == "no-accounts" {
				return
			}
			assertUntouched(t, p, up, "alice", "bob")
		})
	}
}

// TestPoolModelsNeverTriesIneligibleAccounts 禁用、冷却、额度耗尽的账号不被尝试，即使它们的优先级更高、
// 即使唯一可选的账号失败也不会回退到它们。alice（10）禁用、carol（8）冷却、dave（6）额度耗尽，只有 bob（0）可选。
func TestPoolModelsNeverTriesIneligibleAccounts(t *testing.T) {
	newPool := func(t *testing.T) (*modelsUpstream, *Pool) {
		t.Helper()
		up := newModelsUpstream(t)
		for _, user := range []string{"alice", "bob", "carol", "dave"} {
			up.setModels(user, modelEntries(user[:1]+"-only")...)
		}
		alice := priorityAccount("alice", 10)
		alice.Disabled = true
		p := newModelsPool(t, up, 0, alice, priorityAccount("bob", 0), priorityAccount("carol", 8), priorityAccount("dave", 6))
		p.findAccount("carol").service.setCooldown(time.Now().Add(time.Hour))
		setQuota(t, p, "dave", exhaustedQuota(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
		return up, p
	}

	t.Run("eligible-account-answers", func(t *testing.T) {
		up, p := newPool(t)
		for i := range 3 {
			if got := modelsGet(t, p, "/v1/models").ids(); len(got) != 1 || got[0] != "b-only" {
				t.Fatalf("第 %d 次请求的答复来自 %v, want [b-only]", i+1, got)
			}
		}
		assertAttempts(t, p, up, "bob", 1, 3, 0)
		assertUntouched(t, p, up, "alice", "dave")
		if got := up.modelsCalls("carol"); got != 0 {
			t.Errorf("冷却中的 carol 不应被访问，/models 上游请求 %d 次", got)
		}
		if requests, _ := modelsRequests(t, p, "carol"); requests != 0 {
			t.Errorf("冷却中的 carol 请求数 = %d, want 0", requests)
		}
	})

	t.Run("eligible-account-fails", func(t *testing.T) {
		up, p := newPool(t)
		up.failModels("bob", poolFailure{http.StatusInternalServerError, `{"error":"bob down"}`})

		w := modelsGetRaw(t, p, "/v1/models")
		if w.Code != http.StatusInternalServerError || w.Body.String() != `{"error":"bob down"}` {
			t.Fatalf("status=%d body=%q, want 500 %q（bob 的失败响应）", w.Code, w.Body.String(), `{"error":"bob down"}`)
		}
		assertAttempts(t, p, up, "bob", 1, 1, 1)
		assertUntouched(t, p, up, "alice", "dave")
		if got := up.modelsCalls("carol"); got != 0 {
			t.Errorf("冷却中的 carol 不应被访问，/models 上游请求 %d 次", got)
		}
	})
}

// TestPoolModelsHidesModelsRefusedByAnsweringAccount 复用答复账号的 handleModels：该账号已拒绝的模型从返回的列表里隐藏，
// 且不会用其他账号的同名条目补上（其他账号不被访问）。
func TestPoolModelsHidesModelsRefusedByAnsweringAccount(t *testing.T) {
	t.Run("first-account", func(t *testing.T) {
		up := newModelsUpstream(t)
		up.setModels("alice", modelEntry("m-x", "from-alice"), modelEntry("a-only", "a-only"))
		up.setModels("bob", modelEntry("m-x", "from-bob"), modelEntry("b-only", "b-only"))
		p := newModelsPool(t, up, 0, priorityAccount("alice", 5), priorityAccount("bob", 0))
		p.findAccount("alice").service.refuse("m-x")

		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"a-only"})
		assertUntouched(t, p, up, "bob")
	})

	t.Run("account-after-a-failure", func(t *testing.T) {
		up := newModelsUpstream(t)
		up.setModels("bob", modelEntry("m-x", "from-bob"), modelEntry("b-only", "b-only"))
		up.setModels("carol", modelEntry("m-x", "from-carol"), modelEntry("c-only", "c-only"))
		up.failModels("alice", poolFailure{http.StatusInternalServerError, `{"error":"alice down"}`})
		p := newModelsPool(t, up, 0, priorityAccount("alice", 10), priorityAccount("bob", 5), priorityAccount("carol", 0))
		p.findAccount("bob").service.refuse("m-x")

		assertIDs(t, modelsGet(t, p, "/v1/models").ids(), []string{"b-only"})
		assertUntouched(t, p, up, "carol")
	})
}

// TestPoolModelsEmptySuccessfulListIsReturnedAsIs 2xx 就是成功：选中账号的列表为空时返回 200 与空数组（不是 null），
// 不再尝试其他账号。
func TestPoolModelsEmptySuccessfulListIsReturnedAsIs(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("bob", modelEntries("b-only")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 5), priorityAccount("bob", 0))

	w := modelsGetRaw(t, p, "/v1/models")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatalf("status=%d body=%s, want 200 且 data 为空数组", w.Code, w.Body.String())
	}
	assertAttempts(t, p, up, "alice", 1, 1, 0)
	assertUntouched(t, p, up, "bob")
}

// TestPoolModelsCreateNoAffinityBinding 模型列表请求没有会话键，成功、失败切换、全部失败都不产生亲和绑定；
// 作为对照，随后带会话 ID 的生成请求会产生绑定。
func TestPoolModelsCreateNoAffinityBinding(t *testing.T) {
	// 失败切换：alice 失败，bob 答复。
	up, p := newFailoverPool(t)
	up.failModels("alice", poolFailure{http.StatusInternalServerError, `{"error":"alice down"}`})
	modelsGet(t, p, "/v1/models")
	assertAffinityEmpty(t, p)

	// 全部失败。
	up, p = newFailoverPool(t)
	for _, user := range []string{"alice", "bob", "carol"} {
		up.failModels(user, poolFailure{http.StatusInternalServerError, `{"error":"` + user + ` down"}`})
	}
	if w := modelsGetRaw(t, p, "/models"); w.Code != http.StatusInternalServerError {
		t.Fatalf("全部失败 status=%d body=%s, want 500", w.Code, w.Body.String())
	}
	assertAffinityEmpty(t, p)

	if w := modelsChat(p, "shared", "s1"); w.Code != http.StatusOK {
		t.Fatalf("生成请求 status=%d body=%s", w.Code, w.Body.String())
	}
	if counts := p.affinity.activeCounts(time.Now()); len(counts) != 1 {
		t.Fatalf("带会话 ID 的生成请求应产生一个绑定: %v", counts)
	}
}

// cancelledModelsRequest 向 Pool 发送一次客户端已取消的模型列表请求，返回记录响应写入情况的 spy。
func cancelledModelsRequest(t *testing.T, p *Pool) *responseSpy {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	spy := &responseSpy{ResponseRecorder: httptest.NewRecorder()}
	modelsServe(t, p, spy, "/v1/models", ctx)
	return spy
}

// assertNoCancelFallout 断言取消没有使任何账号计错：错误数为 0、连续错误计数为 0、没有冷却。
func assertNoCancelFallout(t *testing.T, p *Pool, users ...string) {
	t.Helper()
	for _, user := range users {
		if got := modelsStreak(p, user); got != 0 {
			t.Errorf("账号 %s 的连续错误计数 = %d, want 0（取消不计错）", user, got)
		}
		if view := cooldownView(t, p, user); view.CooldownUntil != nil || view.Stats.Errors != 0 {
			t.Errorf("账号 %s 因取消冷却或计错: until=%v errors=%d", user, view.CooldownUntil, view.Stats.Errors)
		}
	}
}

// TestPoolModelsCancelledClientGetsNoResponse 客户端在请求开始前已取消：不写任何响应（状态码与正文都没有），
// 不再尝试其他账号（最多只有第一个被选中的账号计一次请求），取消不使账号冷却，也不计为账号的错误。
func TestPoolModelsCancelledClientGetsNoResponse(t *testing.T) {
	up, p := newFailoverPool(t)

	spy := cancelledModelsRequest(t, p)
	if spy.wrote || spy.Body.Len() != 0 {
		t.Fatalf("客户端已取消时不应写响应，实际 wrote=%v status=%d body=%s", spy.wrote, spy.Code, spy.Body.String())
	}
	var total uint64
	for _, user := range []string{"alice", "bob", "carol"} {
		requests, _ := modelsRequests(t, p, user)
		total += requests
	}
	if total > 1 {
		t.Errorf("取消后不应再尝试其他账号，共计 %d 次请求, want <= 1", total)
	}
	assertUntouched(t, p, up, "bob", "carol")
	assertNoCancelFallout(t, p, "alice", "bob", "carol")
}

// TestPoolModelsCancelDuringAttemptStopsTrying 客户端在第一个账号处理期间断开（该账号的请求随之失败）：
// 停止尝试，不写响应，不访问下一个账号；这次失败是取消导致的，不计入账号的错误与连续错误，账号也不冷却。
func TestPoolModelsCancelDuringAttemptStopsTrying(t *testing.T) {
	up, p := newFailoverPool(t)
	up.failModels("alice", poolFailure{http.StatusInternalServerError, `{"error":"alice down"}`})
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	up.hookModels("alice", cancel) // alice 的 /models 请求到达上游时客户端断开

	spy := &responseSpy{ResponseRecorder: httptest.NewRecorder()}
	modelsServe(t, p, spy, "/v1/models", parent)
	if spy.wrote || spy.Body.Len() != 0 {
		t.Fatalf("客户端已取消时不应写响应，实际 wrote=%v status=%d body=%s", spy.wrote, spy.Code, spy.Body.String())
	}
	if got := up.modelsCalls("alice"); got != 1 {
		t.Errorf("alice 的 /models 上游请求 %d 次, want 1", got)
	}
	assertUntouched(t, p, up, "bob", "carol")
	assertNoCancelFallout(t, p, "alice")
}

// TestPoolModelsCancelledRequestsDoNotCountTowardsCooldown 客户端取消不是账号的错误：取消导致的 session 获取失败不计入连续错误，
// 即使反复取消，账号也不冷却、连续错误计数不变，之后的正常请求照常由一个账号答复。
func TestPoolModelsCancelledRequestsDoNotCountTowardsCooldown(t *testing.T) {
	up := newModelsUpstream(t)
	up.setModels("alice", modelEntries("a-only")...)
	up.setModels("bob", modelEntries("b-only")...)
	p := newModelsPool(t, up, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))

	// 次数多于连续错误触发冷却的阈值（3 次），计入连续错误的话账号会因此冷却。
	for i := range 5 {
		if spy := cancelledModelsRequest(t, p); spy.wrote {
			t.Fatalf("取消的请求 %d 不应写响应，实际 status=%d body=%s", i, spy.Code, spy.Body.String())
		}
	}
	assertNoCancelFallout(t, p, "alice", "bob")
	got := modelsGet(t, p, "/v1/models").ids()
	if len(got) != 1 || (got[0] != "a-only" && got[0] != "b-only") {
		t.Fatalf("取消之后的正常请求应由一个账号答复，实际列表 %v", got)
	}
}
