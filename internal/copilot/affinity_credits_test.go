package copilot

// 本文件覆盖会话亲和命中绑定时的 Credits 检查（Pool.choose）：绑定账号可用（未禁用、未冷却、未耗尽、本请求未尝试）时，
//   - 绑定账号有 Credits（主额度剩余 > 0）：继续使用，不推进 SWRR；
//   - 绑定账号无 Credits，且最优档（topGroupLocked，即排除不可用账号后按 priority、Credits 分档的最优档）也无 Credits：继续使用，不推进 SWRR；
//   - 绑定账号无 Credits，而最优档有 Credits：放弃绑定，按无绑定路径重新选择，之后由 bind 把会话键改绑到新账号。
//
// 无额度数据的账号视为无 Credits；“无 Credits”在这里指主额度剩余 ≤ 0 但 Additional Budget 仍有剩余（因而未耗尽）。
// 账号权重在本文件中都相同，因此档内轮询顺序为文件名顺序的循环。

import (
	"net/http"
	"testing"
	"time"
)

// noCreditsQuota 返回主额度已用完但 Additional Budget 仍有剩余的额度：无 Credits，但未耗尽，仍可被选中。
func noCreditsQuota() *Quota { return quotaOf(creditsBucket(0), budgetBucket(0.9)) }

// swrrCurrentOf 读取账号的 SWRR 当前值，用于断言 choose 是否推进了轮询。
func swrrCurrentOf(p *Pool, user string) int {
	account := p.findAccount(user)
	p.mu.Lock()
	defer p.mu.Unlock()
	return account.swrrCurrent
}

// TestAffinityKeepsBindingWithCredits 绑定账号有 Credits：继续使用，即使其他账号也有 Credits；不推进 SWRR。
func TestAffinityKeepsBindingWithCredits(t *testing.T) {
	p := newCapPool(t, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
	setQuota(t, p, "alice", quotaOf(creditsBucket(0.5)))
	setQuota(t, p, "bob", quotaOf(creditsBucket(0.5)))
	p.affinity.bind("s1", "bob", time.Now())
	for i := range 3 {
		if got := userOf(p.choose("s1", nil)); got != "bob" {
			t.Fatalf("请求 %d choose = %s, want bob（绑定账号有 Credits）", i+1, got)
		}
	}
	if a, b := swrrCurrentOf(p, "alice"), swrrCurrentOf(p, "bob"); a != 0 || b != 0 {
		t.Fatalf("swrrCurrent alice=%d bob=%d, want 0 0（命中绑定不推进轮询）", a, b)
	}
}

// TestAffinityKeepsNoCreditsBindingWhenTopTierHasNoCredits 绑定账号无 Credits，最优档也无 Credits：继续使用，不推进 SWRR。
// 覆盖两种“无 Credits”：只剩 Additional Budget，以及没有额度数据。
func TestAffinityKeepsNoCreditsBindingWhenTopTierHasNoCredits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alice *Quota // 绑定账号 alice 的额度
		bob   *Quota // 另一个账号 bob 的额度
	}{
		{name: "都只剩 Additional Budget", alice: noCreditsQuota(), bob: noCreditsQuota()},
		{name: "绑定账号无 Credits，另一个账号无额度数据", alice: noCreditsQuota(), bob: nil},
		{name: "两者都没有额度数据", alice: nil, bob: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newCapPool(t, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
			setQuota(t, p, "alice", tc.alice)
			setQuota(t, p, "bob", tc.bob)
			// 绑定 bob（轮询中排在 alice 之后），避免与“从零开始轮询选 alice”混淆。
			p.affinity.bind("s1", "bob", time.Now())
			for i := range 3 {
				if got := userOf(p.choose("s1", nil)); got != "bob" {
					t.Fatalf("请求 %d choose = %s, want bob（最优档无 Credits，保留绑定）", i+1, got)
				}
			}
			if a, b := swrrCurrentOf(p, "alice"), swrrCurrentOf(p, "bob"); a != 0 || b != 0 {
				t.Fatalf("swrrCurrent alice=%d bob=%d, want 0 0（命中绑定不推进轮询）", a, b)
			}
		})
	}
}

// TestAffinitySwitchesNoCreditsBindingToCreditsAccount 绑定账号无 Credits、最优档有 Credits：放弃绑定，重新选择到最优档的账号。
// 绑定账号无 Credits 的两种形式：只剩 Additional Budget、没有额度数据。
func TestAffinitySwitchesNoCreditsBindingToCreditsAccount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alice *Quota
	}{
		{name: "绑定账号只剩 Additional Budget", alice: noCreditsQuota()},
		{name: "绑定账号没有额度数据", alice: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newCapPool(t, 0, priorityAccount("alice", 0), priorityAccount("bob", 0))
			setQuota(t, p, "alice", tc.alice)
			setQuota(t, p, "bob", quotaOf(creditsBucket(0.5)))
			p.affinity.bind("s1", "alice", time.Now())
			if got := userOf(p.choose("s1", nil)); got != "bob" {
				t.Fatalf("choose = %s, want bob（绑定账号无 Credits，最优档有 Credits，应切换）", got)
			}
		})
	}
}

// TestAffinitySwitchPicksWithinTopTierBySWRR 切换后在最优档内按 SWRR 选择，而不是固定选某一个：
// alice（绑定、无 Credits）；bob、carol 有 Credits。两个不同的会话依次切到 bob、carol。
func TestAffinitySwitchPicksWithinTopTierBySWRR(t *testing.T) {
	p := newCapPool(t, 0, priorityAccount("alice", 0), priorityAccount("bob", 0), priorityAccount("carol", 0))
	setQuota(t, p, "alice", noCreditsQuota())
	setQuota(t, p, "bob", quotaOf(creditsBucket(0.5)))
	setQuota(t, p, "carol", quotaOf(creditsBucket(0.5)))
	p.affinity.bind("s1", "alice", time.Now())
	p.affinity.bind("s2", "alice", time.Now())
	for i, tc := range []struct{ key, want string }{{"s1", "bob"}, {"s2", "carol"}} {
		if got := userOf(p.choose(tc.key, nil)); got != tc.want {
			t.Fatalf("第 %d 次 choose(%s) = %s, want %s（在有 Credits 的最优档内轮询）", i+1, tc.key, got, tc.want)
		}
	}
}

// TestAffinityPriorityDecidesTopTierCredits Credits 检查看的是最优档（priority 优先），而不是全体账号。
func TestAffinityPriorityDecidesTopTierCredits(t *testing.T) {
	t.Run("更高优先级账号无 Credits，只有低优先级账号有 Credits：保留绑定", func(t *testing.T) {
		// 最优档是 bob（priority 10，无 Credits），尽管 carol 有 Credits，但优先级更低，不在最优档。
		p := newCapPool(t, 0, priorityAccount("alice", 5), priorityAccount("bob", 10), priorityAccount("carol", 0))
		setQuota(t, p, "alice", noCreditsQuota())
		setQuota(t, p, "bob", noCreditsQuota())
		setQuota(t, p, "carol", quotaOf(creditsBucket(0.5)))
		p.affinity.bind("s1", "alice", time.Now())
		if got := userOf(p.choose("s1", nil)); got != "alice" {
			t.Fatalf("choose = %s, want alice（最优档 bob 无 Credits，保留绑定）", got)
		}
		if a, b, c := swrrCurrentOf(p, "alice"), swrrCurrentOf(p, "bob"), swrrCurrentOf(p, "carol"); a != 0 || b != 0 || c != 0 {
			t.Fatalf("swrrCurrent alice=%d bob=%d carol=%d, want 全 0（不推进轮询）", a, b, c)
		}
	})
	t.Run("绑定账号与有 Credits 的 carol 优先级都低于最优档：保留绑定", func(t *testing.T) {
		// 绑定账号 alice 与 carol 的优先级都低于最优档 bob；carol 有 Credits 但不在最优档。
		p := newCapPool(t, 0, priorityAccount("alice", 0), priorityAccount("bob", 10), priorityAccount("carol", 0))
		setQuota(t, p, "alice", noCreditsQuota())
		setQuota(t, p, "bob", noCreditsQuota())
		setQuota(t, p, "carol", quotaOf(creditsBucket(0.5)))
		p.affinity.bind("s1", "alice", time.Now())
		if got := userOf(p.choose("s1", nil)); got != "alice" {
			t.Fatalf("choose = %s, want alice（最优档 bob 无 Credits，保留绑定）", got)
		}
	})
	t.Run("更高优先级账号有 Credits：切换", func(t *testing.T) {
		p := newCapPool(t, 0, priorityAccount("alice", 0), priorityAccount("bob", 10), priorityAccount("carol", 0))
		setQuota(t, p, "alice", noCreditsQuota())
		setQuota(t, p, "bob", quotaOf(creditsBucket(0.5)))
		setQuota(t, p, "carol", quotaOf(creditsBucket(0.5)))
		p.affinity.bind("s1", "alice", time.Now())
		if got := userOf(p.choose("s1", nil)); got != "bob" {
			t.Fatalf("choose = %s, want bob（最优档 bob 有 Credits，应切换）", got)
		}
	})
	t.Run("绑定账号有 Credits、更高优先级账号也有 Credits：保留绑定", func(t *testing.T) {
		// 情形 1 优先：绑定账号自身有 Credits 时，不因更高优先级账号存在而切换。
		p := newCapPool(t, 0, priorityAccount("alice", 0), priorityAccount("bob", 10))
		setQuota(t, p, "alice", quotaOf(creditsBucket(0.5)))
		setQuota(t, p, "bob", quotaOf(creditsBucket(0.5)))
		p.affinity.bind("s1", "alice", time.Now())
		if got := userOf(p.choose("s1", nil)); got != "alice" {
			t.Fatalf("choose = %s, want alice（绑定账号有 Credits）", got)
		}
	})
}

// TestAffinityCreditsCheckIgnoresUnavailableTopTierAccounts 最优档按排除禁用/冷却/耗尽/已尝试之后计算：
// 有 Credits 的 bob 处于冷却或已尝试时，最优档是无 Credits 的 carol，因此保留绑定到 alice 的会话。
func TestAffinityCreditsCheckIgnoresUnavailableTopTierAccounts(t *testing.T) {
	setup := func(t *testing.T) *Pool {
		t.Helper()
		p := newCapPool(t, 0, priorityAccount("alice", 0), priorityAccount("bob", 0), priorityAccount("carol", 0))
		setQuota(t, p, "alice", noCreditsQuota())
		setQuota(t, p, "bob", quotaOf(creditsBucket(0.5)))
		setQuota(t, p, "carol", noCreditsQuota())
		p.affinity.bind("s1", "alice", time.Now())
		return p
	}
	t.Run("有 Credits 的账号冷却", func(t *testing.T) {
		p := setup(t)
		coolDown(t, p, "bob")
		if got := userOf(p.choose("s1", nil)); got != "alice" {
			t.Fatalf("choose = %s, want alice（bob 冷却，最优档无 Credits）", got)
		}
	})
	t.Run("有 Credits 的账号已尝试", func(t *testing.T) {
		p := setup(t)
		tried := map[*poolAccount]bool{p.findAccount("bob"): true}
		if got := userOf(p.choose("s1", tried)); got != "alice" {
			t.Fatalf("choose = %s, want alice（bob 已尝试，最优档无 Credits）", got)
		}
	})
}

// TestAffinityHTTPRebindsToCreditsAccount 通过 HTTP 入口：会话先绑定到 alice；alice 的 Credits 用完而 bob 有 Credits 后，
// 同一会话的下一个请求由 bob 处理，绑定表被改绑到 bob。此后 alice 即使重新有 Credits，会话仍留在 bob（bob 有 Credits，继续使用）。
func TestAffinityHTTPRebindsToCreditsAccount(t *testing.T) {
	up := newPoolUpstream(t)
	p, err := NewManagedPool(PoolConfig{
		APIKey:   testAPIKey,
		CacheDir: t.TempDir(), Client: up.client(), SessionAffinityTTL: DefaultSessionAffinityTTL,
	}, []PoolAccount{priorityAccount("alice", 0), priorityAccount("bob", 0)})
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)
	setQuota(t, p, "alice", quotaOf(creditsBucket(0.5)))
	setQuota(t, p, "bob", quotaOf(creditsBucket(0.5)))

	body := affinityChatBody(t, "credits session")
	key := affinityPoolKey(t, body)
	send := func(step string) {
		t.Helper()
		if res := poolRequest(p, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", step, res.Code, res.Body.String())
		}
	}
	assertBound := func(want string) {
		t.Helper()
		if id, ok := p.affinity.lookup(key, time.Now()); !ok || id != want {
			t.Fatalf("绑定 = %q, %v; want %s, true", id, ok, want)
		}
	}

	send("首次请求")
	assertGenerationOrder(t, up, mgmtOrder("alice")...)
	assertBound("alice")

	setQuota(t, p, "alice", noCreditsQuota())
	send("alice 无 Credits 后")
	assertGenerationOrder(t, up, mgmtOrder("alice", "bob")...)
	assertBound("bob")

	setQuota(t, p, "alice", quotaOf(creditsBucket(0.9)))
	send("alice 恢复 Credits 后")
	assertGenerationOrder(t, up, mgmtOrder("alice", "bob", "bob")...)
	assertBound("bob")
}
