package copilot

// 本文件覆盖进入冷却的异步副作用与并发：
//   - 进入冷却的副作用不阻塞请求路径：触发冷却的错误响应不等待会话锁或模型锁（含 pickSession 持有会话门的情形）；
//   - 解除冷却与进入冷却的副作用并发时，冷却不得复活（cooldown.json 不因迟到的写入而重新出现）；
//   - 已在冷却中的在途请求不得把 session.json、models.json 写回磁盘。
// 阻塞通过 mgmtUpstream 的 hold 与 cooldownWrap 实现，使阻塞期间的时序确定；异步副作用的完成用 cooldownEventually / cooldownSettle 等待。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// cooldownBlockedSession 让测试在两个请求之间建立确定的时序：
// 先让 holdAt 指向的生成请求阻塞在上游（此时它已经越过会话获取，不再持有会话锁）；
// 再让另一个请求的 session 交换阻塞在上游并持有会话锁；随后释放生成请求，使它的 401 在会话锁被持有时到达。
// 每次取 session 都重新交换（TTL 1 分钟），因此第二个请求一定会发起交换。返回的清理函数会释放全部阻塞。
func cooldownSessionLockedDuring401(t *testing.T, f *cooldownFixture, p *Pool) (first chan *httptest.ResponseRecorder, release func()) {
	t.Helper()
	// 预热模型列表缓存：holdGenerations 匹配所有带 session 的上游请求，若生成请求还要读取模型列表，hold 会先命中 /models。
	if w := poolRequest(p, "GET", "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("预热模型列表 status=%d body=%s", w.Code, w.Body.String())
	}
	gen := f.up.holdGenerations("oauth-a")
	first = make(chan *httptest.ResponseRecorder, 1)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		first <- cooldownChat(p)
	}()
	reviewAwait(t, gen.started, "生成请求未到达上游")

	sess := f.up.holdSessions("oauth-a")
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		_ = cooldownChat(p)
	}()
	reviewAwait(t, sess.started, "会话交换未到达上游")

	gen.free()
	return first, func() {
		sess.free()
		cooldownFinished(t, firstDone, "触发冷却的请求")
		cooldownFinished(t, secondDone, "在途请求")
		cooldownSettle(p)
	}
}

// TestCooldownUnauthorizedResponseDoesNotWaitForSessionLock 触发冷却的 401 响应不等待会话锁：
// 另一个请求正在阻塞的会话交换中持有会话锁，进入冷却的副作用（清空 session）必须异步执行，401 应快速返回。
func TestCooldownUnauthorizedResponseDoesNotWaitForSessionLock(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.setSessionTTL(time.Minute)
	f.up.pool.failGeneration("oauth-a", poolFailure{401, cooldownBody(401)})
	p := f.start()

	first, release := cooldownSessionLockedDuring401(t, f, p)
	t.Cleanup(release)
	var w *httptest.ResponseRecorder
	select {
	case w = <-first:
	case <-time.After(time.Second):
		t.Fatal("触发冷却的 401 响应等待了会话锁：进入冷却的副作用应异步执行，响应不应被阻塞")
	}
	if w.Code != http.StatusUnauthorized || w.Body.String() != cooldownBody(401) {
		t.Fatalf("触发冷却的响应 status=%d body=%s, want 原样返回 401", w.Code, w.Body.String())
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "401" {
		t.Fatalf("冷却状态 until=%v reason=%q, want 立即冷却且原因 401（同步部分必须立即生效）", v.CooldownUntil, v.CooldownReason)
	}
}

// TestCooldownPickSessionFailureDoesNotWaitForModelsLock pickSession 中因会话获取失败（401）进入冷却时，
// 即使会话门已被持有、且模型列表锁被另一个请求阻塞，401 响应仍须快速返回；清空模型列表的副作用异步完成。
func TestCooldownPickSessionFailureDoesNotWaitForModelsLock(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL // 开启亲和：带会话键的请求在 pickSession 中持有会话门
	f.up.pool.setSessionTTL(time.Minute)

	var mu sync.Mutex
	blocking := false // 模型列表请求已阻塞之后，会话交换返回 401；此前的交换（/v1/models 自身取得 session）照常成功
	modelsStarted := make(chan struct{})
	modelsRelease := make(chan struct{})
	releaseModels := sync.OnceFunc(func() { close(modelsRelease) })
	var modelsOnce sync.Once
	f.config.Client = cooldownWrap(f.up, func(r *http.Request) (*http.Response, bool, error) {
		switch {
		case r.URL.Path == "/copilot_internal/v2/token" && r.Header.Get("Authorization") == "token oauth-a":
			mu.Lock()
			failing := blocking
			mu.Unlock()
			if failing {
				return cooldownHTTPResponse(r, http.StatusUnauthorized, "application/json", io.NopCloser(strings.NewReader(cooldownBody(401)))), true, nil
			}
		case r.URL.Path == "/models":
			first := false
			modelsOnce.Do(func() {
				first = true
				mu.Lock()
				blocking = true
				mu.Unlock()
			})
			if first {
				close(modelsStarted)
				<-modelsRelease
			}
		}
		return nil, false, nil
	})
	p := f.start()
	listedDone := make(chan struct{})
	boundDone := make(chan struct{})
	t.Cleanup(func() {
		releaseModels()
		cooldownFinished(t, listedDone, "/v1/models 请求")
		cooldownFinished(t, boundDone, "带会话键的请求")
		cooldownSettle(p)
	})

	// 先让 /v1/models 阻塞在模型列表的上游请求中（持有模型锁），它已经取得了 session。
	listed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		defer close(listedDone)
		listed <- poolRequest(p, "GET", "/v1/models", "")
	}()
	reviewAwait(t, modelsStarted, "/v1/models 未到达模型列表上游")

	// 再发送带会话键的生成请求：session 交换返回 401，pickSession 据此进入冷却。
	body := affinityChatBody(t, "pickSession failure")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		defer close(boundDone)
		done <- poolRequest(p, "POST", "/v1/chat/completions", body)
	}()
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(time.Second):
		t.Fatal("pickSession 中进入冷却的 401 响应等待了模型锁：进入冷却的副作用应异步执行")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("会话获取失败的响应 status=%d body=%s, want 401", w.Code, w.Body.String())
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "401" {
		t.Fatalf("冷却状态 until=%v reason=%q, want 冷却且原因 401", v.CooldownUntil, v.CooldownReason)
	}

	releaseModels()
	select {
	case <-listed:
	case <-time.After(5 * time.Second):
		t.Fatal("释放模型列表阻塞后 /v1/models 未结束")
	}
}

// TestCooldownLiftWhileEffectsPendingLeavesNoRecord 进入冷却后立即解除，且进入冷却的副作用仍在等待会话锁时：
// 解除之后迟到的副作用不得把 cooldown.json 写回磁盘（冷却不复活）；之后重启也不冷却。
func TestCooldownLiftWhileEffectsPendingLeavesNoRecord(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.setSessionTTL(time.Minute)
	f.up.pool.failGeneration("oauth-a", poolFailure{401, cooldownBody(401)})
	p := f.start()

	first, release := cooldownSessionLockedDuring401(t, f, p)
	t.Cleanup(release)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("触发冷却的 401 响应未返回（进入冷却的副作用应异步执行）")
	}

	// 副作用仍被阻塞时立即解除冷却。
	cooldownDecodeView(t, cooldownPatch(p, `{"id":"a","clear_cooldown":true}`))
	// 在途请求释放后会拿到会话并继续处理：上游改为成功，使它不再触发新的冷却，本测试只验证解除与迟到副作用的并发。
	cooldownSetGeneration(f.up, "oauth-a", http.StatusOK)
	release()

	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
		t.Fatalf("解除后仍冷却至 %s", v.CooldownUntil)
	}
	if !cooldownRecordGone(t, cooldownFile(t, p, "a", "cooldown")) {
		t.Fatal("解除后迟到的副作用又写回了 cooldown.json（冷却复活）")
	}
	restarted := f.start()
	if v := cooldownView(t, restarted, "a"); v.CooldownUntil != nil {
		t.Fatalf("解除并等待副作用结束后，重启仍冷却至 %s", v.CooldownUntil)
	}
}

// TestCooldownInFlightRequestDoesNotRewriteCacheWhileCooling 已在冷却中的在途请求（冷却前已选中）不得把
// session.json 与 models.json 写回磁盘；cooldown.json 保留。
func TestCooldownInFlightRequestDoesNotRewriteCacheWhileCooling(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.setSessionTTL(time.Minute)
	f.up.pool.failGeneration("oauth-a", poolFailure{401, cooldownBody(401)})
	p := f.start()

	// 在途请求：GET /v1/models 在会话交换中阻塞（持有会话锁），之后才完成会话获取与模型列表读取。
	gen := f.up.holdGenerations("oauth-a")
	first := make(chan *httptest.ResponseRecorder, 1)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		first <- cooldownChat(p)
	}()
	reviewAwait(t, gen.started, "生成请求未到达上游")
	sess := f.up.holdSessions("oauth-a")
	inFlight := make(chan *httptest.ResponseRecorder, 1)
	inFlightDone := make(chan struct{})
	go func() {
		defer close(inFlightDone)
		inFlight <- poolRequest(p, "GET", "/v1/models", "")
	}()
	t.Cleanup(func() {
		sess.free()
		gen.free()
		cooldownFinished(t, firstDone, "触发冷却的请求")
		cooldownFinished(t, inFlightDone, "在途请求")
		cooldownSettle(p)
	})
	reviewAwait(t, sess.started, "在途请求的会话交换未到达上游")

	gen.free()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("触发冷却的 401 响应未返回")
	}

	// cooldown.json 的写入排在清空 session 之后，而清空等待在途请求持有的会话锁：必须先释放在途请求，再等待写入。
	sess.free()
	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("在途请求未结束")
	}
	cooldownAwaitRecord(t, p, "a")
	cooldownSettle(p)

	for _, name := range []string{"session", "models"} {
		if _, err := os.Stat(cooldownFile(t, p, "a", name)); !os.IsNotExist(err) {
			t.Errorf("冷却中在途请求写回了 %s.json（err=%v），冷却期间不得写入该缓存", name, err)
		}
	}
	if _, err := os.Stat(cooldownFile(t, p, "a", "cooldown")); err != nil {
		t.Errorf("冷却期间 cooldown.json 应保留: %v", err)
	}
}

// TestCooldownRecordIsWrittenBeforeCachesAreCleared 进入冷却的副作用顺序：先写入 cooldown.json，再清空缓存。
// 清空 session 被会话锁阻塞时，cooldown.json 已经存在，而 session.json 仍在；释放后 session.json 最终被清空。
func TestCooldownRecordIsWrittenBeforeCachesAreCleared(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.setSessionTTL(time.Minute)
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()
	sessionPath := cooldownFile(t, p, "a", "session")

	first, release := cooldownSessionLockedDuring401(t, f, p)
	t.Cleanup(release)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("触发冷却的 401 响应未返回")
	}
	cooldownAwaitRecord(t, p, "a")
	if !cooldownSessionFileHasToken(sessionPath) {
		t.Fatal("cooldown.json 应先于缓存清理写入：清理被会话锁阻塞时 session.json 仍应存在")
	}

	release()
	cooldownEventually(t, "清理后 session.json 无令牌", func() bool {
		return !cooldownSessionFileHasToken(sessionPath)
	})
}

// TestCooldownLiftSkipsPendingDestructiveSteps 解除冷却时，仍待决的破坏性步骤（清空 session、删除会话绑定）被跳过：
// 解除后 session.json 保留（由解除后的正常请求写入），指向该账号的会话绑定也保留。
// 实现应在执行破坏性步骤前（取得缓存锁之后）检查账号仍处于本次冷却，已解除则跳过。
func TestCooldownLiftSkipsPendingDestructiveSteps(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	f.up.pool.setSessionTTL(time.Minute)
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()
	const key = "lift-skips-pending-steps"
	p.affinity.bind(key, "a", time.Now())
	sessionPath := cooldownFile(t, p, "a", "session")

	first, release := cooldownSessionLockedDuring401(t, f, p)
	t.Cleanup(release)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("触发冷却的 401 响应未返回")
	}
	// 清理被会话锁阻塞时解除冷却；在途请求释放后拿到会话并成功返回。
	cooldownDecodeView(t, cooldownPatch(p, `{"id":"a","clear_cooldown":true}`))
	cooldownSetGeneration(f.up, "oauth-a", http.StatusOK)
	release()

	if !cooldownSessionFileHasToken(sessionPath) {
		t.Fatal("解除冷却后，待决的副作用不应再清空 session.json")
	}
	if id, ok := p.affinity.lookup(key, time.Now()); !ok || id != "a" {
		t.Fatalf("解除冷却后，待决的副作用不应删除会话绑定，实际 id=%q ok=%v", id, ok)
	}
}

// cooldownGate 让测试上游在某一类请求上阻塞：arm 之后，第一次到达时关闭 started，之后所有到达都等待 releaseAll。
type cooldownGate struct {
	mu        sync.Mutex
	armed     bool
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	relOnce   sync.Once
}

func newCooldownGate() *cooldownGate {
	return &cooldownGate{started: make(chan struct{}), release: make(chan struct{})}
}

// arm 开启阻塞；未开启时 pass 立即返回。
func (g *cooldownGate) arm() {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
}

// pass 在门开启时阻塞，直到 releaseAll。
func (g *cooldownGate) pass() {
	g.mu.Lock()
	armed := g.armed
	g.mu.Unlock()
	if !armed {
		return
	}
	g.startOnce.Do(func() { close(g.started) })
	<-g.release
}

// releaseAll 释放所有阻塞的请求；可重复调用。
func (g *cooldownGate) releaseAll() {
	g.relOnce.Do(func() { close(g.release) })
}

// TestCooldownPickSessionDoesNotBindCooledAccount pickSession 选出账号并取得 session 之后、绑定之前账号已进入冷却：
// 会话键不得绑定到冷却账号。时序由钩子门确定：生成请求阻塞，之后以 401 冷却账号 a；会话交换阻塞在 pickSession 中，
// 释放后 pickSession 取得 session 时 a 已冷却。模型列表的读取也被阻塞（持有模型锁），使清理卡住，不会删除之后才产生的绑定，因此绑定可被直接观察。
func TestCooldownPickSessionDoesNotBindCooledAccount(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.accounts[0].Priority = 100 // a 优先被选中
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	f.config.ModelsCacheTTL = time.Nanosecond // 每次都重新读取模型列表，使模型锁的阻塞可控
	f.up.pool.setSessionTTL(time.Minute)

	gen := newCooldownGate()
	models := newCooldownGate()
	sess := newCooldownGate()
	f.config.Client = cooldownWrap(f.up, func(r *http.Request) (*http.Response, bool, error) {
		auth := r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/chat/completions" && auth == "Bearer session-oauth-a":
			gen.pass()
			return cooldownHTTPResponse(r, http.StatusUnauthorized, "application/json", io.NopCloser(strings.NewReader(cooldownBody(401)))), true, nil
		case r.URL.Path == "/models" && auth == "Bearer session-oauth-a":
			models.pass()
		case r.URL.Path == "/copilot_internal/v2/token" && auth == "token oauth-a":
			sess.pass()
		}
		return nil, false, nil
	})
	p := f.start()
	firstDone := make(chan struct{})
	modelsDone := make(chan struct{})
	boundDone := make(chan struct{})
	t.Cleanup(func() {
		sess.releaseAll()
		models.releaseAll()
		gen.releaseAll()
		cooldownFinished(t, firstDone, "触发冷却的请求")
		cooldownFinished(t, modelsDone, "/v1/models 请求")
		cooldownFinished(t, boundDone, "带会话键的请求")
		cooldownSettle(p)
	})
	body := affinityChatBody(t, "pickSession binds nothing to a cooled account")
	key := affinityPoolKey(t, body)

	// 1. 不带会话键的生成请求阻塞在上游；释放后以 401 冷却账号 a。
	gen.arm()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		defer close(firstDone)
		first <- cooldownChat(p)
	}()
	reviewAwait(t, gen.started, "生成请求未到达上游")

	// 2. /v1/models 阻塞在模型列表的上游请求中，持有 a 的模型锁，使冷却的清理卡在模型锁上。
	models.arm()
	go func() {
		defer close(modelsDone)
		_ = poolRequest(p, "GET", "/v1/models", "")
	}()
	reviewAwait(t, models.started, "/v1/models 未到达模型列表上游")

	// 3. 带会话键的请求：session 交换阻塞，pickSession 在绑定之前停住。
	sess.arm()
	go func() {
		defer close(boundDone)
		_ = poolRequest(p, "POST", "/v1/chat/completions", body)
	}()
	reviewAwait(t, sess.started, "会话交换未到达上游")

	// 4. 释放生成请求：它以 401 冷却账号 a（同步部分立即生效；清理被模型锁阻塞）。
	gen.releaseAll()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("触发冷却的 401 响应未返回")
	}

	// 5. 释放会话交换：pickSession 取得 session 时账号 a 已冷却，不得绑定。
	//    没有可观察的信号表示 pickSession 已完成绑定，只能留出时间。
	sess.releaseAll()
	time.Sleep(500 * time.Millisecond)
	if id, ok := p.affinity.lookup(key, time.Now()); ok && id == "a" {
		t.Fatal("冷却后 pickSession 仍把会话键绑定到了冷却账号 a")
	}
}

// TestCooldownDeletedAccountDoesNotRecreateCacheDir 进入冷却的副作用尚未完成时删除账号：之后的异步副作用不得重建账号的缓存目录。
func TestCooldownDeletedAccountDoesNotRecreateCacheDir(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.setSessionTTL(time.Minute)
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()
	cacheDir := filepath.Dir(cooldownFile(t, p, "a", "session"))

	first, release := cooldownSessionLockedDuring401(t, f, p)
	t.Cleanup(release)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("触发冷却的 401 响应未返回")
	}
	if w := mgmtCall(p, "DELETE", "/v0/management/accounts?id=a", "", mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("删除账号 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	release()
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("删除账号后异步副作用重建了缓存目录 %s（err=%v）", cacheDir, err)
	}
}
