package copilot

// 本文件覆盖冷却期间的选择规则与冷却时长：冷却账号完全不参与选择（不再兜底），
// 全部冷却时 503，session 失败仍在同一请求内换号但不再短冷却，冷却时长取自配置并在到期后恢复。

import (
	"net/http"
	"testing"
	"time"
)

// TestCooldownAccountIsNeverSelected 冷却中的账号即使优先级最高也不参与选择，之后的请求全部由未冷却账号处理。
func TestCooldownAccountIsNeverSelected(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.accounts[0].Priority = 100
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()

	if w := cooldownChat(p); w.Code != http.StatusUnauthorized {
		t.Fatalf("首个请求由优先级最高的 a 处理并返回 401，实际 status=%d", w.Code)
	}
	for i := range 3 {
		if w := cooldownChat(p); w.Code != http.StatusOK {
			t.Fatalf("冷却后第 %d 个请求 status=%d body=%s, want 200 由 b 处理", i+1, w.Code, w.Body.String())
		}
	}
	assertGenerationOrder(t, f.up.pool, "oauth-a", "oauth-b", "oauth-b", "oauth-b")
}

// TestCooldownAllAccountsCoolingReturns503WithoutFallback 全部账号冷却时直接返回 503，不再从冷却账号兜底，也不访问上游。
func TestCooldownAllAccountsCoolingReturns503WithoutFallback(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	f.up.pool.failGeneration("oauth-b", poolFailure{http.StatusForbidden, cooldownBody(403)})
	p := f.start()

	// 两个请求依次让两个账号冷却（顺序由 SWRR 决定，这里只断言两者都已冷却）。
	for range 2 {
		if w := cooldownChat(p); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Fatalf("冷却前的请求 status=%d body=%s, want 上游的 401 或 403", w.Code, w.Body.String())
		}
	}
	for _, id := range []string{"a", "b"} {
		if cooldownView(t, p, id).CooldownUntil == nil {
			t.Fatalf("账号 %s 应已冷却", id)
		}
	}
	before := f.up.pool.requestCount()
	assertNoAccount(t, cooldownChat(p))
	if got := f.up.pool.requestCount(); got != before {
		t.Fatalf("全部冷却时访问了上游 %d 次, want 0", got-before)
	}
}

// TestCooldownSessionFailureSwitchesAccountWithoutShortCooldown session 失败仍在同一请求内换号（请求成功），
// 但不再 1 分钟短冷却：第 1 次失败后账号仍可用；连续第 3 次失败后冷却 2 小时（缺省值），冷却后不再尝试 session。
func TestCooldownSessionFailureSwitchesAccountWithoutShortCooldown(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.up.pool.failSession("oauth-a", poolFailure{http.StatusInternalServerError, cooldownBody(500)})
	p := f.start()

	before := time.Now()
	if w := cooldownChat(p); w.Code != http.StatusOK {
		t.Fatalf("session 失败后应换到 b 并成功，实际 status=%d body=%s", w.Code, w.Body.String())
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
		t.Fatalf("session 失败一次后账号不应冷却（连续 3 次才冷却），实际冷却至 %s", v.CooldownUntil)
	}
	for range 10 {
		if cooldownView(t, p, "a").CooldownUntil != nil {
			break
		}
		if w := cooldownChat(p); w.Code != http.StatusOK {
			t.Fatalf("session 失败时请求应换号成功，实际 status=%d body=%s", w.Code, w.Body.String())
		}
	}
	after := time.Now()

	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil || v.CooldownReason != "500" {
		t.Fatalf("冷却状态 until=%v reason=%q, want 冷却且原因 500", v.CooldownUntil, v.CooldownReason)
	}
	cooldownAssertWindow(t, *v.CooldownUntil, before, after, DefaultAccountCooldown)

	attempts := f.up.pool.sessionCount("oauth-a")
	if attempts != 3 {
		t.Fatalf("a 的 session 获取尝试 %d 次, want 3（第 3 次失败才冷却）", attempts)
	}
	for range 3 {
		if w := cooldownChat(p); w.Code != http.StatusOK {
			t.Fatalf("冷却后请求 status=%d body=%s, want 200", w.Code, w.Body.String())
		}
	}
	if got := f.up.pool.sessionCount("oauth-a"); got != attempts {
		t.Fatalf("冷却后仍尝试 a 的 session：%d -> %d", attempts, got)
	}
	for _, account := range f.up.pool.generationOrder() {
		if account == "oauth-a" {
			t.Fatalf("a 的 session 从未成功，不应有生成请求由 a 处理: %v", f.up.pool.generationOrder())
		}
	}
}

// TestCooldownDurationFromPoolConfig 冷却时长取自 PoolConfig.AccountCooldown；未设置或非正时使用 DefaultAccountCooldown（2 小时）。
func TestCooldownDurationFromPoolConfig(t *testing.T) {
	if DefaultAccountCooldown != 2*time.Hour {
		t.Fatalf("DefaultAccountCooldown = %s, want 2h", DefaultAccountCooldown)
	}
	for _, tc := range []struct {
		name   string
		config time.Duration
		want   time.Duration
	}{
		{name: "unset", config: 0, want: 2 * time.Hour},
		{name: "negative", config: -time.Minute, want: 2 * time.Hour},
		{name: "custom", config: 90 * time.Minute, want: 90 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			f.config.AccountCooldown = tc.config
			f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
			p := f.start()

			before := time.Now()
			cooldownChat(p)
			after := time.Now()
			v := cooldownView(t, p, "a")
			if v.CooldownUntil == nil {
				t.Fatal("401 后账号应冷却")
			}
			cooldownAssertWindow(t, *v.CooldownUntil, before, after, tc.want)
		})
	}
}

// TestCooldownDurationWithNewPoolIsDefault NewPool（只接受账号配置，没有 PoolConfig）使用缺省冷却时长。
func TestCooldownDurationWithNewPoolIsDefault(t *testing.T) {
	up := newPoolUpstream(t)
	up.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := testPool(t, poolUserConfigs(t, up.client(), "a"))

	before := time.Now()
	cooldownChat(p)
	after := time.Now()
	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil {
		t.Fatal("401 后账号应冷却")
	}
	cooldownAssertWindow(t, *v.CooldownUntil, before, after, DefaultAccountCooldown)
}

// TestCooldownExpiresAndRestoresSelection 冷却截止时间到达后账号重新参与选择，视图中的冷却与原因一并消失。
func TestCooldownExpiresAndRestoresSelection(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.AccountCooldown = time.Second
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusForbidden, cooldownBody(403)})
	p := f.start()
	cooldownChat(p)

	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil {
		t.Fatal("403 后账号应冷却")
	}
	assertNoAccount(t, cooldownChat(p))

	cooldownSetGeneration(f.up, "oauth-a", http.StatusOK)
	time.Sleep(time.Until(*v.CooldownUntil) + 50*time.Millisecond)
	if w := cooldownChat(p); w.Code != http.StatusOK {
		t.Fatalf("冷却到期后账号应重新可用，实际 status=%d body=%s", w.Code, w.Body.String())
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil || v.CooldownReason != "" {
		t.Fatalf("冷却到期后视图仍显示冷却: until=%v reason=%q", v.CooldownUntil, v.CooldownReason)
	}
}

// TestCooldownCountRestartsAfterExpiry 触发冷却后计数清零：冷却到期后，连续错误重新从 0 计数，
// 到期后的 2 次 500 不冷却，第 3 次才冷却。
func TestCooldownCountRestartsAfterExpiry(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.AccountCooldown = time.Second
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusInternalServerError, cooldownBody(500)})
	p := f.start()
	for range 3 {
		cooldownChat(p)
	}
	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil {
		t.Fatal("前提不成立：3 次 500 后应冷却")
	}
	time.Sleep(time.Until(*v.CooldownUntil) + 50*time.Millisecond)

	cooldownChat(p)
	cooldownChat(p)
	if cooldownView(t, p, "a").CooldownUntil != nil {
		t.Fatal("冷却到期后的第 2 次 500 就冷却：触发冷却后计数未清零")
	}
	cooldownChat(p)
	if cooldownView(t, p, "a").CooldownUntil == nil {
		t.Fatal("冷却到期后的第 3 次 500 应再次冷却")
	}
}

// TestImportedAccountUsesPoolCooldownDuration 经管理接口导入的新账号使用池配置的冷却时长（30 分钟），而不是缺省值。
func TestImportedAccountUsesPoolCooldownDuration(t *testing.T) {
	f := newCooldownFixture(t)
	f.config.AccountCooldown = 30 * time.Minute
	f.up.setLogin("oauth-carol", "carol")
	f.up.pool.failGeneration("oauth-carol", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()

	if w := mgmtCall(p, "POST", "/v0/management/accounts", `{"credential":{"oauth_token":"oauth-carol"}}`, mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("导入 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	quotaAwaitIdle(t, p)

	before := time.Now()
	cooldownChat(p)
	after := time.Now()
	v := cooldownView(t, p, "carol")
	if v.CooldownUntil == nil {
		t.Fatal("导入的账号按 401 应冷却")
	}
	cooldownAssertWindow(t, *v.CooldownUntil, before, after, 30*time.Minute)
}
