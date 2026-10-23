package copilot

// 本文件覆盖错误计数规则：哪些响应计入、连续第几次触发冷却、成功与 4xx 对计数的影响、
// 客户端取消不计、同一请求只计一次、冷却中再出错不延长也不重复触发。
// 单账号池让每个请求都落在同一账号上，计数因此确定。

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// TestCooldownUnauthorizedOrForbiddenCoolsAtOnce 生成请求返回 401 或 403 时第一次就冷却，原因为该状态码；
// 触发冷却的那次响应原样返回，之后单账号池没有可用账号。
func TestCooldownUnauthorizedOrForbiddenCoolsAtOnce(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			f.up.pool.failGeneration("oauth-a", poolFailure{status, cooldownBody(status)})
			p := f.start()

			w := cooldownChat(p)
			if w.Code != status || w.Body.String() != cooldownBody(status) {
				t.Fatalf("触发冷却的请求 status=%d body=%s, want 原样返回 %d", w.Code, w.Body.String(), status)
			}
			v := cooldownView(t, p, "a")
			if v.CooldownUntil == nil || v.CooldownReason != strconv.Itoa(status) {
				t.Fatalf("冷却状态 until=%v reason=%q, want 冷却且原因 %d", v.CooldownUntil, v.CooldownReason, status)
			}
			assertNoAccount(t, cooldownChat(p))
		})
	}
}

// TestCooldownAuthFailureCoolsEvenAfterRetryableFailures 401/403 与之前的 429、5xx 计数无关，只要出现就立即冷却。
func TestCooldownAuthFailureCoolsEvenAfterRetryableFailures(t *testing.T) {
	f := newCooldownFixture(t, "a")
	p := f.start()
	cooldownSetGeneration(f.up, "oauth-a", http.StatusInternalServerError)
	if w := cooldownChat(p); w.Code != http.StatusInternalServerError {
		t.Fatalf("第 1 次 500 status=%d, want 500", w.Code)
	}
	cooldownSetGeneration(f.up, "oauth-a", http.StatusForbidden)
	if w := cooldownChat(p); w.Code != http.StatusForbidden {
		t.Fatalf("第 2 次 403 status=%d, want 403", w.Code)
	}
	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil || v.CooldownReason != "403" {
		t.Fatalf("第 2 次请求的 403 后 until=%v reason=%q, want 立即冷却且原因 403", v.CooldownUntil, v.CooldownReason)
	}
}

// TestCooldownRetryableFailuresCoolOnThirdConsecutive 429 与 5xx 连续第 3 次才冷却，之前的响应原样返回；
// 原因为第 3 次（触发那次）的状态码，与前两次是否相同无关。
func TestCooldownRetryableFailuresCoolOnThirdConsecutive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []int
		reason   string
	}{
		{name: "429s", statuses: []int{429, 429, 429}, reason: "429"},
		{name: "5xx-mixed", statuses: []int{500, 502, 503}, reason: "503"},
		{name: "reason-is-third", statuses: []int{502, 500, 500}, reason: "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			p := f.start()
			for i, status := range tc.statuses {
				cooldownSetGeneration(f.up, "oauth-a", status)
				w := cooldownChat(p)
				if w.Code != status || w.Body.String() != cooldownBody(status) {
					t.Fatalf("第 %d 次请求 status=%d body=%s, want 原样返回 %d", i+1, w.Code, w.Body.String(), status)
				}
				cooled := cooldownView(t, p, "a").CooldownUntil != nil
				if want := i == len(tc.statuses)-1; cooled != want {
					t.Fatalf("第 %d 次错误后冷却 = %v, want %v", i+1, cooled, want)
				}
			}
			if v := cooldownView(t, p, "a"); v.CooldownReason != tc.reason {
				t.Fatalf("冷却原因 = %q, want %q", v.CooldownReason, tc.reason)
			}
			assertNoAccount(t, cooldownChat(p))
		})
	}
}

// TestCooldownSessionFailuresFollowStatusRules session 获取失败的计数与生成请求一致：
// 401/403 一次冷却；429 与 5xx 连续第 3 次冷却。冷却后不再尝试获取 session。
func TestCooldownSessionFailuresFollowStatusRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		count  int // 冷却所需的连续失败次数
	}{
		{name: "401-once", status: http.StatusUnauthorized, count: 1},
		{name: "403-once", status: http.StatusForbidden, count: 1},
		{name: "429-third", status: http.StatusTooManyRequests, count: 3},
		{name: "500-third", status: http.StatusInternalServerError, count: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			f.up.pool.failSession("oauth-a", poolFailure{tc.status, cooldownBody(tc.status)})
			p := f.start()
			for i := 1; i <= tc.count; i++ {
				w := cooldownChat(p)
				if w.Code != tc.status || w.Body.String() != cooldownBody(tc.status) {
					t.Fatalf("第 %d 次请求 status=%d body=%s, want 原样返回 %d", i, w.Code, w.Body.String(), tc.status)
				}
			}
			v := cooldownView(t, p, "a")
			if v.CooldownUntil == nil || v.CooldownReason != strconv.Itoa(tc.status) {
				t.Fatalf("冷却状态 until=%v reason=%q, want 冷却且原因 %d", v.CooldownUntil, v.CooldownReason, tc.status)
			}
			before := f.up.pool.sessionCount("oauth-a")
			assertNoAccount(t, cooldownChat(p))
			if got := f.up.pool.sessionCount("oauth-a"); got != before {
				t.Fatalf("冷却后仍尝试获取 session：%d -> %d", before, got)
			}
		})
	}
}

// TestCooldownSessionClientErrorsNeverCount session 获取返回 400、404 等 4xx 时不计数、不冷却，每次都原样返回。
func TestCooldownSessionClientErrorsNeverCount(t *testing.T) {
	for _, status := range []int{400, 404, 413, 422} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			f.up.pool.failSession("oauth-a", poolFailure{status, cooldownBody(status)})
			p := f.start()
			for i := range 5 {
				if w := cooldownChat(p); w.Code != status || w.Body.String() != cooldownBody(status) {
					t.Fatalf("第 %d 次请求 status=%d body=%s, want 原样返回 %d", i+1, w.Code, w.Body.String(), status)
				}
			}
			if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
				t.Fatalf("4xx 的 session 失败使账号冷却至 %s", v.CooldownUntil)
			}
		})
	}
}

// TestCooldownClientErrorsNeitherCountNorReset 生成请求的 400、404 等 4xx 既不计数也不清零：
// 序列 [500, 500, 4xx, 500] 中只有第 4 次（第 3 次计数）触发冷却。
func TestCooldownClientErrorsNeitherCountNorReset(t *testing.T) {
	for _, code := range []int{400, 404, 413, 422} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			p := f.start()
			sequence := []int{500, 500, code, 500}
			for i, status := range sequence {
				cooldownSetGeneration(f.up, "oauth-a", status)
				if w := cooldownChat(p); w.Code != status {
					t.Fatalf("第 %d 次请求 status=%d, want %d", i+1, w.Code, status)
				}
				cooled := cooldownView(t, p, "a").CooldownUntil != nil
				if want := i == len(sequence)-1; cooled != want {
					t.Fatalf("第 %d 次请求后冷却 = %v, want %v", i+1, cooled, want)
				}
			}
		})
	}
}

// TestCooldownSuccessResetsConsecutiveCount 成功（状态 < 400）清零连续计数：
// 序列 [500, 500, 200, 500, 500] 之后仍未冷却，之后的第 6 次 500 才冷却。
func TestCooldownSuccessResetsConsecutiveCount(t *testing.T) {
	f := newCooldownFixture(t, "a")
	p := f.start()
	sequence := []int{500, 500, 200, 500, 500, 500}
	for i, status := range sequence {
		cooldownSetGeneration(f.up, "oauth-a", status)
		if w := cooldownChat(p); w.Code != status {
			t.Fatalf("第 %d 次请求 status=%d, want %d", i+1, w.Code, status)
		}
		cooled := cooldownView(t, p, "a").CooldownUntil != nil
		if want := i == len(sequence)-1; cooled != want {
			t.Fatalf("第 %d 次请求后冷却 = %v, want %v", i+1, cooled, want)
		}
	}
}

// TestCooldownClientCancellationNeverCounts 客户端取消（context.Canceled）造成的失败不计数：
// 取消 5 次后再连续 2 次 500 仍未冷却，第 3 次 500 才冷却。
func TestCooldownClientCancellationNeverCounts(t *testing.T) {
	f := newCooldownFixture(t, "a")
	p := f.start()
	for range 5 {
		cooldownChatCancelled(p)
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
		t.Fatalf("客户端取消的请求使账号冷却至 %s", v.CooldownUntil)
	}
	cooldownSetGeneration(f.up, "oauth-a", http.StatusInternalServerError)
	for i := range 2 {
		cooldownChat(p)
		if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
			t.Fatalf("取消的请求也被计数：第 %d 次 500 后已冷却", i+1)
		}
	}
	cooldownChat(p)
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil {
		t.Fatal("第 3 次 500 后应冷却")
	}
}

// TestCooldownSessionFailureCountsOncePerRequest 单账号的 session 失败每个请求只计一次：pickSession 计数后，
// 同一请求经 handleError 写出错误时不得再计一次。若重复计数，第 2 次请求就会冷却。
func TestCooldownSessionFailureCountsOncePerRequest(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.failSession("oauth-a", poolFailure{http.StatusInternalServerError, cooldownBody(500)})
	p := f.start()
	for i := 1; i <= 2; i++ {
		if w := cooldownChat(p); w.Code != http.StatusInternalServerError {
			t.Fatalf("第 %d 次请求 status=%d, want 500", i, w.Code)
		}
		if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
			t.Fatalf("第 %d 次请求后已冷却：同一请求的错误被重复计数", i)
		}
	}
	w := cooldownChat(p)
	if w.Code != http.StatusInternalServerError || w.Body.String() != cooldownBody(500) {
		t.Fatalf("第 3 次请求 status=%d body=%s, want 原样返回 500", w.Code, w.Body.String())
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil {
		t.Fatal("第 3 次 session 失败后应冷却")
	}
	assertNoAccount(t, cooldownChat(p))
}

// TestCooldownGenerationNetworkErrorsCount 生成请求的网络错误计入：写给客户端的 502 计入错误，第 3 次冷却。
func TestCooldownGenerationNetworkErrorsCount(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.Client = cooldownFailingClient(f.up, "Bearer session-oauth-a")
	p := f.start()
	for i := 1; i <= 3; i++ {
		w := cooldownChat(p)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("第 %d 次网络失败 status=%d, want 502", i, w.Code)
		}
		cooled := cooldownView(t, p, "a").CooldownUntil != nil
		if want := i == 3; cooled != want {
			t.Fatalf("第 %d 次网络失败后冷却 = %v, want %v", i, cooled, want)
		}
	}
	// 冷却原因为 network，而不是代理写给客户端的 502。
	if v := cooldownView(t, p, "a"); v.CooldownReason != "network" {
		t.Fatalf("生成网络错误的冷却原因 = %q, want network", v.CooldownReason)
	}
	assertNoAccount(t, cooldownChat(p))
}

// TestCooldownUpstream502KeepsStatusReason 上游真实返回的 502 仍以状态码 "502" 作为冷却原因（与网络错误区分）。
func TestCooldownUpstream502KeepsStatusReason(t *testing.T) {
	f := newCooldownFixture(t, "a")
	p := f.start()
	for i := 1; i <= 3; i++ {
		cooldownSetGeneration(f.up, "oauth-a", http.StatusBadGateway)
		if w := cooldownChat(p); w.Code != http.StatusBadGateway || w.Body.String() != cooldownBody(502) {
			t.Fatalf("第 %d 次请求 status=%d body=%s, want 原样返回上游的 502", i, w.Code, w.Body.String())
		}
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "502" {
		t.Fatalf("上游 502 的冷却原因 = %q (until=%v), want 502", v.CooldownReason, v.CooldownUntil)
	}
}

// TestCooldownSessionNetworkErrorsCoolWithReasonNetwork session 获取的网络错误按网络错误计入：第 3 次冷却，原因为 "network"。
func TestCooldownSessionNetworkErrorsCoolWithReasonNetwork(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.Client = cooldownFailingClient(f.up, "token oauth-a")
	p := f.start()
	for i := 1; i <= 3; i++ {
		if w := cooldownChat(p); w.Code != http.StatusBadGateway {
			t.Fatalf("第 %d 次网络失败 status=%d, want 502", i, w.Code)
		}
		cooled := cooldownView(t, p, "a").CooldownUntil != nil
		if want := i == 3; cooled != want {
			t.Fatalf("第 %d 次网络失败后冷却 = %v, want %v", i, cooled, want)
		}
	}
	if v := cooldownView(t, p, "a"); v.CooldownReason != "network" {
		t.Fatalf("冷却原因 = %q, want network", v.CooldownReason)
	}
	assertNoAccount(t, cooldownChat(p))
}

// TestCooldownFailuresWhileCoolingDoNotExtendOrRepeat 冷却中再出错（例如此前已开始的请求）不延长冷却、不改变原因，
// 也不重复触发：截止时间与原因保持不变，冷却日志只输出一次。
func TestCooldownFailuresWhileCoolingDoNotExtendOrRepeat(t *testing.T) {
	logs := captureJSONSlog(t, slog.LevelWarn)
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()
	cooldownChat(p) // 401 触发冷却
	first := cooldownView(t, p, "a")
	if first.CooldownUntil == nil {
		t.Fatal("前提不成立：401 后账号应冷却")
	}

	// 直接经 record 记录冷却中的错误，模拟在冷却开始前已经发出、此时才返回的请求。
	a := p.findAccount("a")
	for _, status := range []int{500, 500, 500, 429} {
		a.record(context.Background(), httptest.NewRecorder(), func(w http.ResponseWriter) { w.WriteHeader(status) }, true)
	}

	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil || !v.CooldownUntil.Equal(*first.CooldownUntil) {
		t.Fatalf("冷却中再出错后截止时间 = %v, want 保持 %v", v.CooldownUntil, first.CooldownUntil)
	}
	if v.CooldownReason != "401" {
		t.Fatalf("冷却中再出错后原因 = %q, want 保持 401", v.CooldownReason)
	}
	// 进入冷却的日志由异步副作用输出，等待其出现后再断言恰好一条。
	cooldownEventually(t, "进入冷却的日志", func() bool { return len(cooldownWarnings(t, logs.Bytes())) >= 1 })
	if n := len(cooldownWarnings(t, logs.Bytes())); n != 1 {
		t.Fatalf("进入冷却的日志 %d 条, want 恰好 1 条", n)
	}
}

// TestCooldownAutoFallbackSuccessIsNotAnError Auto 的 /auto 返回 429 但 /models/session 兜底成功时，请求最终为 200，
// 不计为错误，账号也不冷却（即使多次发生）。
func TestCooldownAutoFallbackSuccessIsNotAnError(t *testing.T) {
	const autoBody = `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
	upstream := newPoolUpstream(t)
	// 只让 A 的 /auto 返回 429；/models/session 仍由测试上游正常处理。
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auto" && r.Header.Get("Authorization") == "Bearer session-oauth-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		upstream.serve(w, r)
	})
	p := testPool(t, poolUserConfigs(t, client, "a", "b"))
	for i := range 6 {
		if w := poolRequest(p, "POST", "/v1/chat/completions", autoBody); w.Code != http.StatusOK {
			t.Fatalf("请求 %d status=%d body=%s, want 200（兜底成功）", i, w.Code, w.Body.String())
		}
	}
	if upstream.autoCount("oauth-a") == 0 {
		t.Fatal("前提不成立：A 应被 Auto 接口请求过")
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil || v.Stats.Errors != 0 {
		t.Fatalf("兜底成功的请求被计为错误: until=%v errors=%d", v.CooldownUntil, v.Stats.Errors)
	}
}
