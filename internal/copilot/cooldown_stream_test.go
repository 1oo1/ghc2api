package copilot

// 本文件覆盖流式响应中途的错误计数：
//   - 上游已返回 200 后读取上游流失败（非客户端取消）计为网络错误（原因 "network"），且不清零连续错误计数；
//   - 正常结束的 200 流仍视为成功并清零；
//   - 客户端断开（取消或写入失败）造成的结束不计为错误。
// 测试上游按脚本依次返回生成请求的响应（见 cooldownPlanned），之后的请求交给测试上游。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// cooldownStepKind 是测试上游对一次生成请求的脚本化响应。
type cooldownStepKind int

const (
	stepStatus       cooldownStepKind = iota // 返回 status 与 cooldownBody，非流式
	stepStream                               // 200 的 SSE，正常结束
	stepBroken                               // 200 的 SSE：写出一段后读取出错（上游中途断开，非客户端取消）
	stepHeld                                 // 200 的 SSE：写出一段后阻塞，直到请求上下文被取消（客户端取消）
	stepBrokenStatus                         // 返回 status，正文写出一段后读取出错（上游错误响应的正文中断）；stream 为 true 时是 SSE
)

// cooldownStep 是脚本中的一步。delivered 只用于 stepHeld：写出第一段数据后关闭，测试据此取消请求。
// stream 只用于 stepBrokenStatus：为 true 时响应的 Content-Type 是 text/event-stream，否则是 application/json。
type cooldownStep struct {
	kind      cooldownStepKind
	status    int
	stream    bool
	delivered chan struct{}
}

// cooldownPlanned 返回的客户端让账号 account 的生成请求依次得到 steps 中的响应；之后的生成请求交给测试上游。
func cooldownPlanned(up *mgmtUpstream, account string, steps []cooldownStep) *http.Client {
	var mu sync.Mutex
	next := 0
	auth := "Bearer session-" + account
	return cooldownWrap(up, func(r *http.Request) (*http.Response, bool, error) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != auth {
			return nil, false, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if next >= len(steps) {
			return nil, false, nil
		}
		step := steps[next]
		next++
		return cooldownStepResponse(r, step), true, nil
	})
}

// cooldownStepResponse 按脚本步骤构造上游响应。
func cooldownStepResponse(r *http.Request, step cooldownStep) *http.Response {
	switch step.kind {
	case stepStatus:
		return cooldownHTTPResponse(r, step.status, "application/json", io.NopCloser(strings.NewReader(cooldownBody(step.status))))
	case stepStream:
		return cooldownHTTPResponse(r, http.StatusOK, "text/event-stream", io.NopCloser(strings.NewReader("data: {\"ok\":true}\n\n")))
	case stepBroken:
		return cooldownHTTPResponse(r, http.StatusOK, "text/event-stream", &cooldownBrokenBody{})
	case stepBrokenStatus:
		contentType := "application/json"
		if step.stream {
			contentType = "text/event-stream"
		}
		return cooldownHTTPResponse(r, step.status, contentType, &cooldownBrokenBody{})
	default: // stepHeld
		return cooldownHTTPResponse(r, http.StatusOK, "text/event-stream", &cooldownHeldBody{ctx: r.Context(), delivered: step.delivered})
	}
}

// cooldownBrokenBody 第一次读取返回一段数据，之后返回连接中断错误（不是 context 错误，即非客户端取消）。
type cooldownBrokenBody struct {
	sent bool
}

func (b *cooldownBrokenBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "data: {\"partial\":true}\n\n"), nil
	}
	return 0, errors.New("connection reset by peer")
}

func (b *cooldownBrokenBody) Close() error { return nil }

// cooldownHeldBody 第一次读取返回一段数据并关闭 delivered；之后阻塞，直到请求上下文结束，返回上下文错误。
type cooldownHeldBody struct {
	ctx       context.Context
	delivered chan struct{}
	sent      bool
	once      sync.Once
}

func (b *cooldownHeldBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		n := copy(p, "data: {\"partial\":true}\n\n")
		b.once.Do(func() { close(b.delivered) })
		return n, nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cooldownHeldBody) Close() error { return nil }

// cooldownStatusStep 返回一个非流式的脚本步骤。
func cooldownStatusStep(status int) cooldownStep {
	return cooldownStep{kind: stepStatus, status: status}
}

// cooldownBrokenWriter 模拟客户端已断开：每次写入都失败。
type cooldownBrokenWriter struct {
	*httptest.ResponseRecorder
}

func (w cooldownBrokenWriter) Write([]byte) (int, error) {
	return 0, errors.New("broken pipe")
}

// cooldownChatWithWriter 用给定的 ResponseWriter 发送一次 chat 请求，返回时请求已结束。
func cooldownChatWithWriter(p *Pool, w http.ResponseWriter) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(poolChatBody))
	r.Header.Set("Content-Type", "application/json")
	withTestAPIKey(p.Handler()).ServeHTTP(w, r)
}

// cooldownChatCancelledMidStream 发送流式请求；等待第一段数据写出后取消请求，返回时请求已结束。
func cooldownChatCancelledMidStream(t *testing.T, p *Pool, delivered chan struct{}) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(poolChatBody)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		withTestAPIKey(p.Handler()).ServeHTTP(w, r)
	}()
	reviewAwait(t, delivered, "流式响应未写出第一段数据")
	cancel()
	reviewAwait(t, done, "客户端取消后请求未结束")
	return w
}

// TestCooldownBrokenStreamAfter200CountsAsNetworkError 上游已返回 200 后流中途断开（非客户端取消）：计为网络错误，
// 连续计数不清零。429、429、中途断开的流 → 第 3 次即冷却，原因为 "network"。
func TestCooldownBrokenStreamAfter200CountsAsNetworkError(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.Client = cooldownPlanned(f.up, "oauth-a", []cooldownStep{
		cooldownStatusStep(http.StatusTooManyRequests),
		cooldownStatusStep(http.StatusTooManyRequests),
		{kind: stepBroken},
	})
	p := f.start()

	for i := 1; i <= 2; i++ {
		if w := cooldownChat(p); w.Code != http.StatusTooManyRequests {
			t.Fatalf("第 %d 次请求 status=%d, want 429", i, w.Code)
		}
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
		t.Fatal("前提不成立：两次 429 后不应冷却")
	}
	if w := cooldownChat(p); w.Code != http.StatusOK {
		t.Fatalf("第 3 次请求的响应头应为 200（流已开始），status=%d", w.Code)
	}
	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil || v.CooldownReason != "network" {
		t.Fatalf("流中途断开后 until=%v reason=%q, want 冷却且原因 network（不清零计数）", v.CooldownUntil, v.CooldownReason)
	}
	assertNoAccount(t, cooldownChat(p))
}

// TestCooldownCompleteStreamResetsStreak 正常结束的 200 流仍视为成功并清零连续计数：
// 429、429、正常流、429、429 之后仍未冷却，第 3 次连续 429 才冷却，原因为 "429"。
func TestCooldownCompleteStreamResetsStreak(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.Client = cooldownPlanned(f.up, "oauth-a", []cooldownStep{
		cooldownStatusStep(http.StatusTooManyRequests),
		cooldownStatusStep(http.StatusTooManyRequests),
		{kind: stepStream},
		cooldownStatusStep(http.StatusTooManyRequests),
		cooldownStatusStep(http.StatusTooManyRequests),
		cooldownStatusStep(http.StatusTooManyRequests),
	})
	p := f.start()

	for i := 1; i <= 2; i++ {
		cooldownChat(p)
	}
	cooldownChat(p) // 正常结束的流：清零
	for i := 1; i <= 2; i++ {
		cooldownChat(p)
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
		t.Fatal("正常流之后的两次 429 不应冷却（计数已清零）")
	}
	cooldownChat(p)
	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil || v.CooldownReason != "429" {
		t.Fatalf("正常流之后第 3 次连续 429 后 until=%v reason=%q, want 冷却且原因 429", v.CooldownUntil, v.CooldownReason)
	}
}

// TestCooldownClientCancelledStreamIsNotCounted 客户端在流中途取消：不计为错误，连续 3 次取消后账号仍不冷却。
func TestCooldownClientCancelledStreamIsNotCounted(t *testing.T) {
	f := newCooldownFixture(t, "a")
	steps := make([]cooldownStep, 3)
	delivered := make([]chan struct{}, 3)
	for i := range steps {
		delivered[i] = make(chan struct{})
		steps[i] = cooldownStep{kind: stepHeld, delivered: delivered[i]}
	}
	f.config.Client = cooldownPlanned(f.up, "oauth-a", steps)
	p := f.start()

	for i := range steps {
		cooldownChatCancelledMidStream(t, p, delivered[i])
		if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
			t.Fatalf("第 %d 次客户端取消后账号冷却至 %s：取消不得计为错误", i+1, v.CooldownUntil)
		}
	}
}

// TestCooldownClientWriteFailureIsNotCounted 客户端写入失败（已断开）造成的流结束不计为错误：
// 连续 3 次写入失败后仍不冷却；之后 2 次 429 仍不冷却，第 3 次 429 才冷却。
func TestCooldownClientWriteFailureIsNotCounted(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.Client = cooldownPlanned(f.up, "oauth-a", []cooldownStep{
		{kind: stepStream}, {kind: stepStream}, {kind: stepStream},
		cooldownStatusStep(http.StatusTooManyRequests),
		cooldownStatusStep(http.StatusTooManyRequests),
		cooldownStatusStep(http.StatusTooManyRequests),
	})
	p := f.start()

	for i := 1; i <= 3; i++ {
		rec := httptest.NewRecorder()
		cooldownChatWithWriter(p, cooldownBrokenWriter{rec})
		if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
			t.Fatalf("第 %d 次客户端写入失败后账号冷却至 %s：写入失败不得计为错误", i, v.CooldownUntil)
		}
	}
	for i := 1; i <= 2; i++ {
		cooldownChat(p)
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
		t.Fatal("写入失败之后的两次 429 不应冷却")
	}
	cooldownChat(p)
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "429" {
		t.Fatalf("第 3 次 429 后 until=%v reason=%q, want 冷却且原因 429", v.CooldownUntil, v.CooldownReason)
	}
}

// TestCooldownClientWriteFailureResetsStreak 客户端写入失败（上游已返回 200，视为成功）清零连续计数：
// 429、429、写入失败的 200、429、429 之后仍未冷却，第 3 次连续 429 才冷却。
func TestCooldownClientWriteFailureResetsStreak(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.Client = cooldownPlanned(f.up, "oauth-a", []cooldownStep{
		cooldownStatusStep(http.StatusTooManyRequests),
		cooldownStatusStep(http.StatusTooManyRequests),
		{kind: stepStream},
		cooldownStatusStep(http.StatusTooManyRequests),
		cooldownStatusStep(http.StatusTooManyRequests),
		cooldownStatusStep(http.StatusTooManyRequests),
	})
	p := f.start()

	cooldownChat(p)
	cooldownChat(p)
	cooldownChatWithWriter(p, cooldownBrokenWriter{httptest.NewRecorder()}) // 上游 200 但客户端写入失败：视为成功，清零
	cooldownChat(p)
	cooldownChat(p)
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
		t.Fatalf("写入失败的 200 之后两次 429 不应冷却（计数已清零），实际冷却至 %s", v.CooldownUntil)
	}
	cooldownChat(p)
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "429" {
		t.Fatalf("第 3 次连续 429 后 until=%v reason=%q, want 冷却且原因 429", v.CooldownUntil, v.CooldownReason)
	}
}
