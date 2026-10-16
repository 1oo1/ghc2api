package copilot

// 本文件覆盖额度耗尽时的跳过规则：主额度（Credits）与 Additional Budget 都耗尽的账号不参与选择，
// 直到主额度的重置时间之后才恢复。判断只读取账号缓存的额度，不访问上游。
// 时间都通过 now 参数传入，不依赖真实时间的 sleep；需要“已过去”的重置时间时直接使用过去的时刻。

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// withReset 返回把 ResetTime 设为 reset 的额度窗口副本。
func withReset(bucket QuotaBucket, reset string) QuotaBucket {
	bucket.ResetTime = reset
	return bucket
}

// exhaustedQuota 构造 Credits 与 Additional Budget 都已耗尽的额度，主额度的重置时间为 reset。
func exhaustedQuota(reset string) *Quota {
	return quotaOf(withReset(creditsBucket(0), reset), budgetBucket(0))
}

// newAffinityPriorityPool 与 newPriorityPool 相同，但开启会话亲和（默认 TTL），用于测试 choose 的亲和分支。
func newAffinityPriorityPool(t *testing.T, accounts ...PoolAccount) *Pool {
	t.Helper()
	p, err := NewManagedPool(PoolConfig{APIKey: testAPIKey, CacheDir: t.TempDir(), SessionAffinityTTL: DefaultSessionAffinityTTL}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)
	return p
}

// pickLockedAt 在 now 时调用 pickLocked，与 choose 一样持有 p.mu。
func pickLockedAt(p *Pool, now time.Time, tried map[*poolAccount]bool) *poolAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pickLocked(now, tried)
}

// TestExhaustedUntilResolvesResetTime exhaustedUntil 只在主额度与 Additional Budget 都耗尽、
// 且主额度的重置时间可解析时返回重置时刻；其余情况都不跳过，返回零值。
func TestExhaustedUntilResolvesResetTime(t *testing.T) {
	const rfc = "2026-10-20T08:30:00Z"
	reset := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		quota *Quota
		want  time.Time // 仅 ok 为 true 时有效
		ok    bool
	}{
		{name: "rfc3339-utc", quota: quotaOf(withReset(creditsBucket(0), rfc)), want: reset, ok: true},
		{name: "rfc3339-with-offset", quota: quotaOf(withReset(creditsBucket(0), "2026-10-20T16:30:00+08:00")), want: reset, ok: true},
		{name: "date-only-is-utc-midnight", quota: quotaOf(withReset(creditsBucket(0), "2026-10-20")),
			want: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), ok: true},
		{name: "budget-zero-counts-as-exhausted", quota: quotaOf(withReset(creditsBucket(0), rfc), budgetBucket(0)), want: reset, ok: true},
		{name: "budget-before-main-bucket", quota: quotaOf(budgetBucket(0), withReset(creditsBucket(0), rfc)), want: reset, ok: true},
		{name: "premium-requests-is-main-bucket",
			quota: quotaOf(withReset(QuotaBucket{Window: "premium_requests", Description: "Premium requests"}, rfc)), want: reset, ok: true},
		{name: "budget-reset-time-is-ignored",
			quota: quotaOf(withReset(creditsBucket(0), rfc), withReset(budgetBucket(0), "2026-11-01T00:00:00Z")), want: reset, ok: true},
		{name: "budget-positive-not-exhausted", quota: quotaOf(withReset(creditsBucket(0), rfc), budgetBucket(0.5))},
		{name: "main-has-credits", quota: quotaOf(withReset(creditsBucket(0.2), rfc), budgetBucket(0))},
		{name: "nil-quota", quota: nil},
		{name: "no-main-bucket", quota: quotaOf(withReset(budgetBucket(0), rfc))},
		{name: "empty-reset-time", quota: quotaOf(creditsBucket(0), budgetBucket(0))},
		{name: "reset-only-on-budget", quota: quotaOf(creditsBucket(0), withReset(budgetBucket(0), rfc))},
		{name: "invalid-reset-text", quota: quotaOf(withReset(creditsBucket(0), "tomorrow"))},
		{name: "datetime-without-T", quota: quotaOf(withReset(creditsBucket(0), "2026-10-20 08:30:00"))},
		{name: "invalid-date", quota: quotaOf(withReset(creditsBucket(0), "2026-13-01"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := exhaustedUntil(tc.quota)
			if ok != tc.ok {
				t.Fatalf("exhaustedUntil ok = %v, want %v", ok, tc.ok)
			}
			if tc.ok && !got.Equal(tc.want) {
				t.Fatalf("重置时刻 = %s, want %s", got, tc.want)
			}
			if !tc.ok && !got.IsZero() {
				t.Fatalf("不跳过时返回的时刻 = %s, want 零值", got)
			}
		})
	}
}

// TestExhaustedAccountTimeBoundary 账号在重置时刻之前视为耗尽；等于或晚于重置时刻即恢复。
// YYYY-MM-DD 按 UTC 0 点计算，与 RFC3339 写出同一时刻时边界一致。
func TestExhaustedAccountTimeBoundary(t *testing.T) {
	reset := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, text string }{
		{name: "rfc3339", text: "2026-10-20T00:00:00Z"},
		{name: "date-only", text: "2026-10-20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPriorityPool(t, nil, priorityAccount("alice", 0))
			setQuota(t, p, "alice", exhaustedQuota(tc.text))
			account := p.findAccount("alice")
			for _, c := range []struct {
				name string
				now  time.Time
				want bool
			}{
				{name: "一纳秒前仍耗尽", now: reset.Add(-time.Nanosecond), want: true},
				{name: "重置时刻恢复", now: reset, want: false},
				{name: "重置之后恢复", now: reset.Add(time.Second), want: false},
			} {
				if got := account.exhausted(c.now); got != c.want {
					t.Errorf("%s: exhausted = %v, want %v", c.name, got, c.want)
				}
			}
		})
	}
}

// TestExhaustedAccountWithoutExhaustionIsNotSkipped 没有额度数据、主额度仍有剩余、
// 或只有 Additional Budget 仍有剩余时，账号都不视为耗尽。
func TestExhaustedAccountWithoutExhaustionIsNotSkipped(t *testing.T) {
	const reset = "2026-10-20T00:00:00Z"
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	p := newPriorityPool(t, nil, priorityAccount("nodata", 0), priorityAccount("credits", 0), priorityAccount("budget", 0))
	setQuota(t, p, "credits", quotaOf(withReset(creditsBucket(0.5), reset), budgetBucket(0)))
	setQuota(t, p, "budget", quotaOf(withReset(creditsBucket(0), reset), budgetBucket(0.5)))
	for _, user := range []string{"nodata", "credits", "budget"} {
		if p.findAccount(user).exhausted(now) {
			t.Errorf("账号 %s 不应视为耗尽", user)
		}
	}
}

// TestExhaustPickLockedSkipsHigherPriorityExhaustedAccount 耗尽的账号即使优先级更高也不会被选中，与禁用账号一样直接跳过。
// 先把同一账号设为未耗尽作为对照，确认 alice 原本确实会被选中。
func TestExhaustPickLockedSkipsHigherPriorityExhaustedAccount(t *testing.T) {
	const reset = "2026-10-20T00:00:00Z"
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	p := newPriorityPool(t, nil, priorityAccount("alice", 10), priorityAccount("bob", 0))
	setQuota(t, p, "alice", quotaOf(withReset(creditsBucket(0.5), reset)))
	if got := userOf(pickLockedAt(p, now, nil)); got != "alice" {
		t.Fatalf("对照：alice 未耗尽时选中 %s, want alice", got)
	}
	setQuota(t, p, "alice", exhaustedQuota(reset))
	if got := userOf(pickLockedAt(p, now, nil)); got != "bob" {
		t.Fatalf("alice 耗尽时选中 %s, want bob", got)
	}
}

// TestExhaustPickLockedReturnsNilWhenAllExhausted 所有账号都耗尽时没有候选，pickLocked 返回 nil，上层据此返回 503。
func TestExhaustPickLockedReturnsNilWhenAllExhausted(t *testing.T) {
	const reset = "2026-10-20T00:00:00Z"
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	p := newPriorityPool(t, nil, priorityAccount("alice", 10), priorityAccount("bob", 0))
	setQuota(t, p, "alice", exhaustedQuota(reset))
	setQuota(t, p, "bob", exhaustedQuota(reset))
	if account := pickLockedAt(p, now, nil); account != nil {
		t.Fatalf("全部耗尽时 pickLocked = %s, want nil", userOf(account))
	}
}

// TestExhaustPickLockedRestoresAccountAfterReset 重置时刻之后账号恢复参与选择，无需刷新额度：
// 额度数据保持不变，变化的只有 now。
func TestExhaustPickLockedRestoresAccountAfterReset(t *testing.T) {
	reset := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	p := newPriorityPool(t, nil, priorityAccount("alice", 10), priorityAccount("bob", 0))
	setQuota(t, p, "alice", exhaustedQuota(reset.Format(time.RFC3339)))
	for _, tc := range []struct {
		name string
		now  time.Time
		want string
	}{
		{name: "重置前一纳秒仍跳过", now: reset.Add(-time.Nanosecond), want: "bob"},
		{name: "重置时刻恢复", now: reset, want: "alice"},
		{name: "重置之后保持恢复", now: reset.Add(72 * time.Hour), want: "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := userOf(pickLockedAt(p, tc.now, nil)); got != tc.want {
				t.Fatalf("pickLocked = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestExhaustChooseAffinityRejectsExhaustedAccount 会话亲和绑定到耗尽的账号时，choose 不使用该绑定，改由 pickLocked 重选；
// 账号恢复后绑定重新生效。alice 的优先级低于 bob，因此只有亲和生效时才会选中 alice。
// 子测试共享同一个池，按顺序执行：先未耗尽，再耗尽，最后重置时刻已过。
func TestExhaustChooseAffinityRejectsExhaustedAccount(t *testing.T) {
	const key = "exhaust-affinity-session"
	p := newAffinityPriorityPool(t, priorityAccount("alice", 0), priorityAccount("bob", 10))
	p.affinity.bind(key, "alice", time.Now())
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)

	t.Run("未耗尽时使用绑定", func(t *testing.T) {
		setQuota(t, p, "alice", quotaOf(creditsBucket(0.5)))
		if got := userOf(p.choose(key, nil)); got != "alice" {
			t.Fatalf("choose = %s, want alice（绑定应生效）", got)
		}
	})
	t.Run("耗尽时改选其他账号", func(t *testing.T) {
		setQuota(t, p, "alice", exhaustedQuota(future))
		if got := userOf(p.choose(key, nil)); got != "bob" {
			t.Fatalf("choose = %s, want bob", got)
		}
	})
	t.Run("重置时刻已过后绑定重新生效", func(t *testing.T) {
		setQuota(t, p, "alice", exhaustedQuota(past))
		if got := userOf(p.choose(key, nil)); got != "alice" {
			t.Fatalf("choose = %s, want alice", got)
		}
	})
}

// TestExhaustPoolReturns503WhenEveryAccountExhausted 所有账号都耗尽时，请求返回 503，且不访问上游。
func TestExhaustPoolReturns503WhenEveryAccountExhausted(t *testing.T) {
	upstream := newPoolUpstream(t)
	p := newPriorityPool(t, upstream.client(), priorityAccount("alice", 0), priorityAccount("bob", 0))
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	setQuota(t, p, "alice", exhaustedQuota(future))
	setQuota(t, p, "bob", exhaustedQuota(future))
	w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "没有可用的 GitHub Copilot 账号") {
		t.Fatalf("全部耗尽时 status=%d body=%s, want 503 与“没有可用的 GitHub Copilot 账号”", w.Code, w.Body.String())
	}
	if got := upstream.requestCount(); got != 0 {
		t.Fatalf("全部耗尽时访问了上游 %d 次, want 0", got)
	}
}
