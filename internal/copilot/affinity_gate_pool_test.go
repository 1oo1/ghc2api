package copilot

// 本文件覆盖会话亲和的门（Pool.gate，见 affinity_gate.go）在 Pool 层的并发行为：
// 同一会话键的请求在选账号前排队，先到的请求完成 bind 后门即释放（不持有到响应结束），后到的请求才轮到；
// 不同会话键、亲和关闭或键为空时不加门，请求互不阻塞；等待门时客户端取消不影响账号状态。
// 同步只用 channel 与 awaitGateWaiter 轮询门的内部状态（有超时保护）；不使用固定时长的 sleep。

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// gateOtherSession 是与 affinitySession 不同的 Claude Code 会话 UUID，用于构造不同会话键。
const gateOtherSession = "66666666-7777-4888-8999-000000000000"

// gateClaudeBody 构造 /v1/messages 请求体：metadata.user_id 为 Claude Code 的 JSON 格式会话 ID，模型为 gpt-4.1（与 pool_test 的生成请求一致）。
func gateClaudeBody(t *testing.T, session, text string) string {
	t.Helper()
	return string(affinityBody(t, map[string]any{
		"model":    "gpt-4.1",
		"metadata": map[string]any{"user_id": `{"session_id":"` + session + `"}`},
		"messages": []any{map[string]string{"role": "user", "content": text}},
	}))
}

// gateSessionKey 计算 gateClaudeBody 请求对应的完整会话键（客户端密钥与请求头同 gateSend）。
func gateSessionKey(t *testing.T, body string) string {
	t.Helper()
	key := sessionKey(affinityRequest("/v1/messages", affinityLogClientKey, ""), []byte(body), "/v1/messages")
	affinityAssertKeyShape(t, key)
	return key
}

// gateSend 在新 goroutine 中经 Pool 入口发送 /v1/messages 请求（Bearer 客户端密钥），响应通过返回的 channel 交付。
// 它不调用 t 的方法，可在任何 goroutine 中使用；ctx 为请求上下文，用于模拟客户端取消。
func gateSend(ctx context.Context, p *Pool, body string) <-chan *httptest.ResponseRecorder {
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+affinityLogClientKey)
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, r)
		ch <- w
	}()
	return ch
}

// gateWait 等待异步请求结束并返回响应；超时则终止测试。只能在测试 goroutine 中调用。
func gateWait(t *testing.T, ch <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-ch:
		return w
	case <-time.After(gateGuardTimeout):
		t.Fatal("并发请求长时间未结束")
		return nil
	}
}

// gateAwaitHeld 等待先到的请求在上游 session 交换处阻塞（hold 已开始）。
func gateAwaitHeld(t *testing.T, hold *mgmtHold) {
	t.Helper()
	select {
	case <-hold.started:
	case <-time.After(gateGuardTimeout):
		t.Fatal("先到的请求没有在 session 交换处阻塞")
	}
}

// awaitGateWaiter 等待至少一个请求在 key 的门上排队（Pool.gate 中该 key 的等待者数大于 0）。
// second 是该同键请求的响应 channel：若它在排队出现之前已有响应，说明该请求没有经过门（例如缺门时它直接选了其他账号），
// 立即失败，不必等到超时。len(second) 只读取缓冲计数、不消费响应，之后仍可由 gateWait 取得响应。
// 测试借此保证“同键请求已到达门口”之后才放行先到的请求，从而稳定复现竞争窗口，不依赖固定时长的 sleep。
func awaitGateWaiter(t *testing.T, p *Pool, key string, second <-chan *httptest.ResponseRecorder) {
	t.Helper()
	deadline := time.Now().Add(gateWaiterTimeout)
	for {
		p.gate.mu.Lock()
		e := p.gate.m[key]
		waiting := e != nil && e.waiters > 0
		p.gate.mu.Unlock()
		if waiting {
			return
		}
		if len(second) > 0 {
			t.Fatal("同键请求未排队即已完成（没有经过会话门）")
		}
		if time.Now().After(deadline) {
			t.Fatal("同会话键的请求没有在门上排队")
		}
		runtime.Gosched()
	}
}

// gateExpectServed 断言响应为 200，且由 want 账号（上游记录的 OAuth token，如 oauth-alice）处理。
func gateExpectServed(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v; body=%s", err, w.Body.String())
	}
	if got["served_by"] != want {
		t.Fatalf("served_by = %q, want %q", got["served_by"], want)
	}
}

// TestGateSameSessionWaitsForFirstBinding 同一会话的两个请求并发到达：先到的请求在 alice 的 session 交换处被卡住，
// 第二个请求在门上等待。放行后两个请求都由 alice 处理，只有一次“新建绑定”，没有“切换账号”，bob 不交换 session。
func TestGateSameSessionWaitsForFirstBinding(t *testing.T) {
	up := newMgmtUpstream(t)
	hold := up.holdSessions("oauth-alice")
	p, logs := affinityLogPool(t, up, slog.LevelDebug, DefaultSessionAffinityTTL)
	body := gateClaudeBody(t, affinitySession, "first question")
	key := gateSessionKey(t, body)

	first := gateSend(context.Background(), p, body)
	gateAwaitHeld(t, hold)
	second := gateSend(context.Background(), p, body)
	awaitGateWaiter(t, p, key, second)
	hold.free()

	gateExpectServed(t, gateWait(t, first), "oauth-alice")
	gateExpectServed(t, gateWait(t, second), "oauth-alice")
	assertGenerationOrder(t, up.pool, "oauth-alice", "oauth-alice")
	if n := up.pool.sessionCount("oauth-bob"); n != 0 {
		t.Fatalf("同会话的第二个请求交换了 bob 的 session %d 次, want 0", n)
	}
	prefix := key[:12]
	assertAffinityLogs(t, logs.Bytes(),
		affinityLogWant{msg: "会话亲和：新建绑定", level: "INFO", attrs: map[string]string{"session": prefix, "bound_account": "alice"}},
		affinityLogWant{msg: "会话亲和：命中", level: "DEBUG", attrs: map[string]string{"session": prefix, "bound_account": "alice"}},
	)
}

// TestGateReleasedBeforeResponseEnds 门只覆盖选账号与绑定，绑定之后即释放，不持有到响应结束：
// 先到的请求在生成阶段被上游阻塞时，同键的门已经空闲，其他调用者可以立即获取。
func TestGateReleasedBeforeResponseEnds(t *testing.T) {
	up := newMgmtUpstream(t)
	hold := up.holdGenerations("oauth-alice")
	p, _ := affinityLogPool(t, up, slog.LevelInfo, DefaultSessionAffinityTTL)
	body := gateClaudeBody(t, affinitySession, "first question")
	key := gateSessionKey(t, body)

	first := gateSend(context.Background(), p, body)
	gateAwaitHeld(t, hold) // 请求已完成选账号与绑定，并停在生成阶段

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release, err := p.gate.acquire(ctx, key)
	if err != nil {
		t.Fatalf("先到的请求仍在生成阶段时同键的门未释放: %v", err)
	}
	release()

	hold.free()
	gateExpectServed(t, gateWait(t, first), "oauth-alice")
}

// TestGateDifferentSessionsDoNotBlockEachOther 第一个请求卡在 alice 的 session 交换时，不同会话键的请求必须能完成。
// 账号按 SWRR 交替：第二个请求由 bob 处理（不会排在 alice 之后）。
func TestGateDifferentSessionsDoNotBlockEachOther(t *testing.T) {
	up := newMgmtUpstream(t)
	hold := up.holdSessions("oauth-alice")
	p, _ := affinityLogPool(t, up, slog.LevelInfo, DefaultSessionAffinityTTL)

	stuck := gateSend(context.Background(), p, gateClaudeBody(t, affinitySession, "first question"))
	gateAwaitHeld(t, hold)
	other := gateSend(context.Background(), p, gateClaudeBody(t, gateOtherSession, "second question"))
	gateExpectServed(t, gateWait(t, other), "oauth-bob")

	hold.free()
	gateExpectServed(t, gateWait(t, stuck), "oauth-alice")
}

// TestGateWaiterUsesFailoverAccountAfterFirstSessionFails 先到的请求的 alice session 失败，alice 被冷却，
// 请求在同一次处理中改用 bob；等待门的同键请求随后使用 bob，绑定一致：只有“新建绑定 bob”，没有“切换账号”。
// 本用例验证故障切换与门的协作：故障切换之后门仍会放行等待者（否则等待者超时失败）。
// 缺少门时同键请求不会排队，awaitGateWaiter 会失败，因此本用例也能发现门的缺失。
func TestGateWaiterUsesFailoverAccountAfterFirstSessionFails(t *testing.T) {
	up := newMgmtUpstream(t)
	hold := up.holdSessions("oauth-alice")
	p, logs := affinityLogPool(t, up, slog.LevelDebug, DefaultSessionAffinityTTL)
	body := gateClaudeBody(t, affinitySession, "first question")
	key := gateSessionKey(t, body)

	first := gateSend(context.Background(), p, body)
	gateAwaitHeld(t, hold)
	second := gateSend(context.Background(), p, body)
	awaitGateWaiter(t, p, key, second)

	up.pool.failSession("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"alice down"}`})
	hold.free()

	gateExpectServed(t, gateWait(t, first), "oauth-bob")
	gateExpectServed(t, gateWait(t, second), "oauth-bob")
	if p.findAccount("alice").service.available(time.Now()) {
		t.Fatal("session 失败的 alice 应处于冷却中")
	}
	assertGenerationOrder(t, up.pool, "oauth-bob", "oauth-bob")
	prefix := key[:12]
	assertAffinityLogs(t, logs.Bytes(),
		affinityLogWant{msg: "会话亲和：新建绑定", level: "INFO", attrs: map[string]string{"session": prefix, "bound_account": "bob"}},
		affinityLogWant{msg: "会话亲和：命中", level: "DEBUG", attrs: map[string]string{"session": prefix, "bound_account": "bob"}},
	)
}

// TestGateClientCancelWhileWaitingLeavesAccountsAlone 同键请求在门上等待时客户端取消：请求返回，
// 不选账号（没有请求计数增加、bob 不交换 session），没有账号被冷却；之后同键请求仍正常命中 alice。
func TestGateClientCancelWhileWaitingLeavesAccountsAlone(t *testing.T) {
	up := newMgmtUpstream(t)
	hold := up.holdSessions("oauth-alice")
	p, logs := affinityLogPool(t, up, slog.LevelDebug, DefaultSessionAffinityTTL)
	body := gateClaudeBody(t, affinitySession, "first question")
	key := gateSessionKey(t, body)

	first := gateSend(context.Background(), p, body)
	gateAwaitHeld(t, hold)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := gateSend(ctx, p, body)
	awaitGateWaiter(t, p, key, waiting)

	alice, bob := p.findAccount("alice"), p.findAccount("bob")
	requestsBefore := [2]uint64{alice.requests.Load(), bob.requests.Load()}
	cancel()
	gateWait(t, waiting) // 取消的请求必须返回；它不写入任何响应内容，因此只等待返回

	if got := [2]uint64{alice.requests.Load(), bob.requests.Load()}; got != requestsBefore {
		t.Fatalf("客户端取消后请求计数变化: alice/bob %v -> %v", requestsBefore, got)
	}
	if n := up.pool.sessionCount("oauth-bob"); n != 0 {
		t.Fatalf("取消的请求交换了 bob 的 session %d 次, want 0", n)
	}
	now := time.Now()
	if !alice.service.available(now) || !bob.service.available(now) {
		t.Fatal("客户端取消不应冷却任何账号")
	}
	if alice.failures.Load() != 0 || bob.failures.Load() != 0 {
		t.Fatal("客户端取消不应计入失败数")
	}

	hold.free()
	gateExpectServed(t, gateWait(t, first), "oauth-alice")
	gateExpectServed(t, gateWait(t, gateSend(context.Background(), p, body)), "oauth-alice")
	prefix := key[:12]
	assertAffinityLogs(t, logs.Bytes(),
		affinityLogWant{msg: "会话亲和：新建绑定", level: "INFO", attrs: map[string]string{"session": prefix, "bound_account": "alice"}},
		affinityLogWant{msg: "会话亲和：命中", level: "DEBUG", attrs: map[string]string{"session": prefix, "bound_account": "alice"}},
	)
}

// TestGateSkippedWhenAffinityDisabledOrKeyEmpty 亲和关闭或键为空时不加门：先到的请求卡住时，同一请求体的第二个请求
// 不等待，直接由其他账号（SWRR 轮到的 bob）完成。
func TestGateSkippedWhenAffinityDisabledOrKeyEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		body string
	}{
		{name: "affinity-disabled", ttl: 0, body: gateClaudeBody(t, affinitySession, "first question")},
		{name: "empty-key", ttl: DefaultSessionAffinityTTL, body: `{"model":"gpt-4.1","messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			hold := up.holdSessions("oauth-alice")
			p, _ := affinityLogPool(t, up, slog.LevelInfo, tc.ttl)

			stuck := gateSend(context.Background(), p, tc.body)
			gateAwaitHeld(t, hold)
			gateExpectServed(t, gateWait(t, gateSend(context.Background(), p, tc.body)), "oauth-bob")

			hold.free()
			gateExpectServed(t, gateWait(t, stuck), "oauth-alice")
		})
	}
}
