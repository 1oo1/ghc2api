package copilot

// 本文件覆盖上游 402（Copilot 额外预算用尽）的冷却规则：
//   - ruleOf(402) == ruleUntilReset：一次即冷却，原因为 "402"，不写风控记录（风控只针对 401/403）；
//   - 截止时间取缓存额度中主额度（第一个非 additional_budget 窗口）的重置时间（parseResetTime），
//     没有额度、没有主额度、重置时间为空或不可解析、或不晚于当前时间时，退回 now + account_cooldown；
//   - 生成响应与 session 交换两条路径都适用；其余冷却副作用与既有规则一致。
//
// 实现必须提供的名字：规则常量 ruleUntilReset（类型 failureRule）。

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// cooldown402Body 是上游 402 的错误正文。
const cooldown402Body = `{"message":"You've reached your additional usage limit for your plan"}`

// cooldown402Failure 是测试上游返回的 402 失败。
var cooldown402Failure = poolFailure{http.StatusPaymentRequired, cooldown402Body}

// cooldown402Account 创建单账号池，冷却时长固定为 30 分钟（与重置时间区分开），并设置缓存额度。
func cooldown402Account(t *testing.T, quota *Quota) *poolAccount {
	t.Helper()
	p := newPriorityPool(t, nil, priorityAccount("alice", 0))
	setQuota(t, p, "alice", quota)
	a := p.findAccount("alice")
	a.cooldown = 30 * time.Minute
	return a
}

// cooldown402Until 读取账号当前的冷却截止时间（未冷却时为 Unix 零点）。
func cooldown402Until(a *poolAccount) time.Time {
	return time.Unix(0, a.service.cooldownUntil.Load())
}

// cooldown402Reset 返回距今 36 小时之后的重置时刻（取整到秒，UTC）。
func cooldown402Reset() time.Time {
	return time.Now().Add(36 * time.Hour).UTC().Truncate(time.Second)
}

// TestRuleOf402 402 为 ruleUntilReset；401/403 仍为 ruleImmediate，429/5xx 仍为 ruleCounted，其他 4xx 仍忽略。
func TestRuleOf402(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   failureRule
	}{
		{http.StatusPaymentRequired, ruleUntilReset},
		{http.StatusUnauthorized, ruleImmediate},
		{http.StatusForbidden, ruleImmediate},
		{http.StatusTooManyRequests, ruleCounted},
		{http.StatusInternalServerError, ruleCounted},
		{http.StatusServiceUnavailable, ruleCounted},
		{http.StatusBadRequest, ruleIgnore},
		{http.StatusNotFound, ruleIgnore},
		{http.StatusRequestEntityTooLarge, ruleIgnore},
		{http.StatusUnprocessableEntity, ruleIgnore},
	} {
		if got := ruleOf(tc.status); got != tc.want {
			t.Errorf("ruleOf(%d) = %d, want %d", tc.status, got, tc.want)
		}
	}
}

// TestCooldown402UntilUsesMainBucketReset 截止时间取主额度的重置时间：RFC3339 按其时刻，YYYY-MM-DD 取该日 UTC 0 点；
// 主额度不必已耗尽，Additional Budget 窗口（即使排在前面、带重置时间）不参与。
func TestCooldown402UntilUsesMainBucketReset(t *testing.T) {
	reset := cooldown402Reset()
	day := time.Now().UTC().AddDate(0, 0, 3).Truncate(24 * time.Hour)
	budgetReset := withReset(budgetBucket(0), time.Now().Add(72*time.Hour).UTC().Format(time.RFC3339))
	for _, tc := range []struct {
		name  string
		quota *Quota
		want  time.Time
	}{
		{"rfc3339", quotaOf(withReset(creditsBucket(0), reset.Format(time.RFC3339))), reset},
		{"rfc3339-with-offset", quotaOf(withReset(creditsBucket(0), reset.In(time.FixedZone("", 8*3600)).Format(time.RFC3339))), reset},
		{"date-only-is-utc-midnight", quotaOf(withReset(creditsBucket(0), day.Format(time.DateOnly))), day},
		{"main-not-exhausted", quotaOf(withReset(creditsBucket(0.5), reset.Format(time.RFC3339))), reset},
		{"budget-first-is-skipped", quotaOf(budgetReset, withReset(creditsBucket(0), reset.Format(time.RFC3339))), reset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := cooldown402Account(t, tc.quota)
			if !a.countFailure(context.Background(), ruleOf(402), "402") {
				t.Fatal("402 应一次即使账号进入冷却")
			}
			if got := cooldown402Until(a); !got.Equal(tc.want) {
				t.Fatalf("冷却截止时间 = %s, want %s", got, tc.want)
			}
			if got := a.cooldownReason(); got != "402" {
				t.Fatalf("冷却原因 = %q, want 402", got)
			}
		})
	}
}

// TestCooldown402FallsBackToAccountCooldown 没有额度数据、没有主额度、重置时间为空或不可解析、
// 或重置时间不晚于当前时间时，截止时间退回 now + account_cooldown。
func TestCooldown402FallsBackToAccountCooldown(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC()
	for _, tc := range []struct {
		name  string
		quota *Quota
	}{
		{"no-quota", nil},
		{"no-main-bucket", quotaOf(withReset(budgetBucket(0), cooldown402Reset().Format(time.RFC3339)))},
		{"empty-reset", quotaOf(creditsBucket(0))},
		{"invalid-reset", quotaOf(withReset(creditsBucket(0), "tomorrow"))},
		{"reset-only-on-budget", quotaOf(creditsBucket(0), withReset(budgetBucket(0), cooldown402Reset().Format(time.RFC3339)))},
		{"past-rfc3339", quotaOf(withReset(creditsBucket(0), past.Format(time.RFC3339)))},
		{"past-date-only", quotaOf(withReset(creditsBucket(0), past.AddDate(0, 0, -1).Format(time.DateOnly)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := cooldown402Account(t, tc.quota)
			before := time.Now()
			if !a.countFailure(context.Background(), ruleOf(402), "402") {
				t.Fatal("402 应一次即使账号进入冷却")
			}
			cooldownAssertWindow(t, cooldown402Until(a), before, time.Now(), a.cooldown)
			if got := a.cooldownReason(); got != "402" {
				t.Fatalf("冷却原因 = %q, want 402", got)
			}
		})
	}
}

// TestCooldown402DoesNotChangeAuthRules 401/403 即使缓存额度带重置时间，仍按 account_cooldown 冷却。
func TestCooldown402DoesNotChangeAuthRules(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		a := cooldown402Account(t, quotaOf(withReset(creditsBucket(0), cooldown402Reset().Format(time.RFC3339))))
		before := time.Now()
		if !a.countFailure(context.Background(), ruleOf(status), "x") {
			t.Fatalf("%d 应一次即冷却", status)
		}
		cooldownAssertWindow(t, cooldown402Until(a), before, time.Now(), a.cooldown)
	}
}

// TestCooldown402AlreadyCoolingIsNotExtended 账号已在冷却中时再收到 402：不触发、不延长，截止时间与原因不变。
func TestCooldown402AlreadyCoolingIsNotExtended(t *testing.T) {
	reset := cooldown402Reset()
	a := cooldown402Account(t, quotaOf(withReset(creditsBucket(0), reset.Format(time.RFC3339))))
	if !a.countFailure(context.Background(), ruleUntilReset, "402") {
		t.Fatal("首次 402 应进入冷却")
	}
	a.quota.mu.Lock() // 换成更晚的重置时间，验证第二次 402 不会据此延长冷却
	a.quota.data = quotaOf(withReset(creditsBucket(0), reset.Add(24*time.Hour).Format(time.RFC3339)))
	a.quota.mu.Unlock()
	if a.countFailure(context.Background(), ruleUntilReset, "402") {
		t.Fatal("冷却中再次 402 不应重复触发冷却")
	}
	if got := cooldown402Until(a); !got.Equal(reset) {
		t.Fatalf("冷却被延长：截止时间 = %s, want %s", got, reset)
	}
}

// TestCooldown402SessionFailureUsesSameRule countSessionFailure 把上游 402 按同一规则处理：一次即冷却，返回状态码 402。
func TestCooldown402SessionFailureUsesSameRule(t *testing.T) {
	reset := cooldown402Reset()
	a := cooldown402Account(t, quotaOf(withReset(creditsBucket(0), reset.Format(time.RFC3339))))
	status, cooled := a.countSessionFailure(context.Background(), &upstreamError{status: http.StatusPaymentRequired})
	if status != http.StatusPaymentRequired || !cooled {
		t.Fatalf("countSessionFailure = (%d, %v), want (402, true)", status, cooled)
	}
	if got := cooldown402Until(a); !got.Equal(reset) {
		t.Fatalf("冷却截止时间 = %s, want %s", got, reset)
	}
	if got := a.cooldownReason(); got != "402" {
		t.Fatalf("冷却原因 = %q, want 402", got)
	}
}

// TestCooldown402GenerationHTTP 生成请求返回 402：响应原样透传，账号冷却至主额度重置时间、原因为 402，
// cooldown.json 持久化同样的截止时间与原因，会话绑定被移走，后续同会话请求改由其他账号处理，且不写风控记录。
func TestCooldown402GenerationHTTP(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	f.accounts[0].Priority = 100 // 首次请求选中 a
	f.up.pool.failGeneration("oauth-a", cooldown402Failure)
	p := f.start()
	reset := cooldown402Reset()
	// Additional Budget 仍有剩余，账号不会因额度耗尽被跳过；上游却返回 402，这正是真实场景。
	setQuota(t, p, "a", quotaOf(withReset(creditsBucket(0), reset.Format(time.RFC3339)), budgetBucket(0.5)))

	key := riskSessionKey(t, "s402")
	w := riskChat(p, "s402")
	if w.Code != http.StatusPaymentRequired || w.Body.String() != cooldown402Body {
		t.Fatalf("402 请求 status=%d body=%s, want 原样透传 402", w.Code, w.Body.String())
	}
	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil || !v.CooldownUntil.Equal(reset) || v.CooldownReason != "402" {
		t.Fatalf("账号视图 until=%v reason=%q, want %s 与 402", v.CooldownUntil, v.CooldownReason, reset)
	}

	// 异步副作用：持久化记录与会话绑定移除。
	cooldownAwaitRecord(t, p, "a")
	fields := cooldownFields(t, cooldownFile(t, p, "a", "cooldown"))
	if got := persistTimeField(t, fields, "until"); !got.Equal(reset) {
		t.Fatalf("cooldown.json 的 until = %s, want %s", got, reset)
	}
	if string(fields["reason"]) != `"402"` {
		t.Fatalf("cooldown.json 的 reason = %s, want \"402\"", fields["reason"])
	}
	cooldownEventually(t, "指向 a 的会话绑定删除", func() bool {
		_, ok := p.affinity.lookup(key, time.Now())
		return !ok
	})

	// 后续同会话请求避开 a，由 b 处理并重新绑定。
	if w := riskChat(p, "s402"); w.Code != http.StatusOK {
		t.Fatalf("后续请求 status=%d body=%s, want 由 b 处理得到 200", w.Code, w.Body.String())
	}
	if got := countAccount(f.up.pool.generationOrder(), "oauth-a"); got != 1 {
		t.Fatalf("冷却后 a 又收到了生成请求，共 %d 次", got)
	}
	if id, ok := p.affinity.lookup(key, time.Now()); !ok || id != "b" {
		t.Fatalf("会话绑定 = (%q, %v), want 重新绑定到 b", id, ok)
	}

	// 402 不写风控记录。
	riskMustMiss(t, p.risk, key, time.Now())
	if _, err := os.Stat(riskFilePath(f)); !os.IsNotExist(err) {
		t.Fatalf("402 不应写风控记录文件，Stat err = %v", err)
	}
}

// TestCooldown402SessionExchangeHTTP session 交换返回 402：账号冷却至重置时间（原因 402），请求在同一次调用中切换到其他账号，
// 之后不再向 a 发起 session 交换，且不写风控记录。
func TestCooldown402SessionExchangeHTTP(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	f.accounts[0].Priority = 100
	f.up.pool.failSession("oauth-a", cooldown402Failure)
	p := f.start()
	reset := cooldown402Reset()
	// Additional Budget 仍有剩余，账号不会因额度耗尽被跳过；上游却返回 402，这正是真实场景。
	setQuota(t, p, "a", quotaOf(withReset(creditsBucket(0), reset.Format(time.RFC3339)), budgetBucket(0.5)))

	if w := riskChat(p, "s402-session"); w.Code != http.StatusOK {
		t.Fatalf("请求 status=%d body=%s, want 切换到 b 后得到 200", w.Code, w.Body.String())
	}
	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil || !v.CooldownUntil.Equal(reset) || v.CooldownReason != "402" {
		t.Fatalf("账号视图 until=%v reason=%q, want %s 与 402", v.CooldownUntil, v.CooldownReason, reset)
	}
	if got := countAccount(f.up.pool.generationOrder(), "oauth-b"); got != 1 {
		t.Fatalf("b 收到 %d 次生成请求, want 1", got)
	}

	sessions := f.up.pool.sessionCount("oauth-a")
	if w := riskChat(p, "s402-session"); w.Code != http.StatusOK {
		t.Fatalf("后续请求 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if got := f.up.pool.sessionCount("oauth-a"); got != sessions {
		t.Fatalf("冷却期间 a 仍发起了 session 交换: %d -> %d", sessions, got)
	}

	riskMustMiss(t, p.risk, riskSessionKey(t, "s402-session"), time.Now())
	if _, err := os.Stat(riskFilePath(f)); !os.IsNotExist(err) {
		t.Fatalf("402 不应写风控记录文件，Stat err = %v", err)
	}
}

// TestCooldown402ClearCooldown clear_cooldown 可解除 402 冷却：账号立即可选，持久化记录被删除。
func TestCooldown402ClearCooldown(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", cooldown402Failure)
	p := f.start()
	// Additional Budget 仍有剩余，账号不会因额度耗尽被跳过；上游却返回 402，这正是真实场景。
	setQuota(t, p, "a", quotaOf(withReset(creditsBucket(0), cooldown402Reset().Format(time.RFC3339)), budgetBucket(0.5)))

	if w := cooldownChat(p); w.Code != http.StatusPaymentRequired {
		t.Fatalf("402 请求 status=%d body=%s", w.Code, w.Body.String())
	}
	assertNoAccount(t, cooldownChat(p)) // 冷却期间不参与选择
	cooldownAwaitRecord(t, p, "a")

	view := cooldownDecodeView(t, cooldownPatch(p, `{"id":"a","clear_cooldown":true}`))
	if view.CooldownUntil != nil || view.CooldownReason != "" {
		t.Fatalf("解除后视图仍冷却: until=%v reason=%q", view.CooldownUntil, view.CooldownReason)
	}
	if !cooldownRecordGone(t, cooldownFile(t, p, "a", "cooldown")) {
		t.Fatal("解除冷却后 cooldown.json 应被删除")
	}
	cooldownSetGeneration(f.up, "oauth-a", http.StatusOK)
	if w := cooldownChat(p); w.Code != http.StatusOK {
		t.Fatalf("解除后请求 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
}
