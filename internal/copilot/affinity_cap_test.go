package copilot

// 本文件覆盖每账号会话绑定上限（PoolConfig.SessionAffinityMaxPerAccount）：affinityTable.activeCounts 统计仍有效的绑定数，
// Pool.choose 在新会话或故障切换时排除已满的账号。规则：命中可用绑定时不受上限限制；上限不会把会话推向冷却账号；
// 所有账号都已满时忽略上限照常选择并绑定；键为空或关闭亲和时上限无效；上限 0 表示不限制。
// 期望序列按 pool_swrr_test.go 中的 SWRR 手算：本文件的账号权重都为 1，因此轮询顺序即文件名顺序的循环。
// 测试只通过 choose、activeCounts、HTTP 入口与 affinity.lookup 观察结果。账号都没有额度数据（权重均为 1），
// HTTP 测试只发送成功的请求，因此不会触发异步额度刷新。

import (
	"net/http"
	"testing"
	"time"
)

// TestDefaultSessionAffinityMaxPerAccountIsFive 缺省的每账号绑定上限为 5（cmd 层以此为 session_affinity_max_per_account 的缺省值）。
func TestDefaultSessionAffinityMaxPerAccountIsFive(t *testing.T) {
	if DefaultSessionAffinityMaxPerAccount != 5 {
		t.Fatalf("DefaultSessionAffinityMaxPerAccount = %d, want 5", DefaultSessionAffinityMaxPerAccount)
	}
}

// assertActiveCounts 比较各账号的有效绑定数。未出现在 want 中的账号应为 0，实现可以省略零值条目。
func assertActiveCounts(t *testing.T, got, want map[string]int) {
	t.Helper()
	for id, n := range want {
		if got[id] != n {
			t.Fatalf("账号 %s 的有效绑定数 = %d, want %d（完整结果 %v）", id, got[id], n, got)
		}
	}
	for id, n := range got {
		if _, ok := want[id]; !ok && n != 0 {
			t.Fatalf("账号 %s 的有效绑定数 = %d, want 0（完整结果 %v）", id, n, got)
		}
	}
}

// TestAffinityActiveCountsByAccount activeCounts 按账号 ID 统计仍有效的绑定数。
func TestAffinityActiveCountsByAccount(t *testing.T) {
	table := newAffinityTable(time.Hour)
	now := time.Now()
	table.bind("k1", "alice", now)
	table.bind("k2", "alice", now)
	table.bind("k3", "bob", now)
	assertActiveCounts(t, table.activeCounts(now), map[string]int{"alice": 2, "bob": 1})
}

// TestAffinityActiveCountsSkipsExpiredBindings 截止时刻及之后的绑定不计数，与 lookup 的规则一致。
// k1 在 base+1h 恰好到期，k2 在 base+1h30m 到期。
func TestAffinityActiveCountsSkipsExpiredBindings(t *testing.T) {
	const ttl = time.Hour
	table := newAffinityTable(ttl)
	base := time.Now()
	table.bind("k1", "alice", base)
	table.bind("k2", "bob", base.Add(30*time.Minute))
	for _, tc := range []struct {
		name string
		now  time.Time
		want map[string]int
	}{
		{name: "截止前一纳秒两者都计数", now: base.Add(ttl - time.Nanosecond), want: map[string]int{"alice": 1, "bob": 1}},
		{name: "恰好到截止时刻 alice 不计数", now: base.Add(ttl), want: map[string]int{"bob": 1}},
		{name: "全部过期后为空", now: base.Add(2 * time.Hour), want: map[string]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertActiveCounts(t, table.activeCounts(tc.now), tc.want)
		})
	}
}

// TestAffinityActiveCountsFollowsRebinding 改绑把计数从原账号转到新账号；续期同一个键不会重复计数。
func TestAffinityActiveCountsFollowsRebinding(t *testing.T) {
	table := newAffinityTable(time.Hour)
	now := time.Now()
	table.bind("k1", "alice", now)
	table.bind("k2", "alice", now)
	table.bind("k1", "bob", now)
	assertActiveCounts(t, table.activeCounts(now), map[string]int{"alice": 1, "bob": 1})

	table.bind("k2", "bob", now)
	assertActiveCounts(t, table.activeCounts(now), map[string]int{"bob": 2})

	table.bind("k2", "bob", now.Add(time.Minute))
	assertActiveCounts(t, table.activeCounts(now.Add(time.Minute)), map[string]int{"bob": 2})
}

// newCapPool 创建开启会话亲和（默认 TTL）并设置每账号绑定上限的池；账号按传入顺序加入，不启用管理接口。
func newCapPool(t *testing.T, limit int, accounts ...PoolAccount) *Pool {
	t.Helper()
	p, err := NewManagedPool(PoolConfig{
		CacheDir: t.TempDir(), SessionAffinityTTL: DefaultSessionAffinityTTL, SessionAffinityMaxPerAccount: limit,
	}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)
	return p
}

// TestCapExcludesFullAccountForNewSession 新会话（键没有绑定）不选已满的账号。上限 1 时 alice 已有一条绑定；
// 无上限时 alice 是轮询的第一位，因此选中 bob 即说明上限生效。
func TestCapExcludesFullAccountForNewSession(t *testing.T) {
	p := newCapPool(t, 1, priorityAccount("alice", 0), priorityAccount("bob", 0))
	p.affinity.bind("old", "alice", time.Now())
	if got := userOf(p.choose("new", nil)); got != "bob" {
		t.Fatalf("新会话 choose = %s, want bob（alice 已满）", got)
	}
}

// TestCapIgnoresExpiredBindings 过期的绑定不占名额：alice 唯一的绑定已过期，新会话仍可选中 alice（轮询的第一位）。
func TestCapIgnoresExpiredBindings(t *testing.T) {
	p := newCapPool(t, 1, priorityAccount("alice", 0), priorityAccount("bob", 0))
	p.affinity.bind("old", "alice", time.Now().Add(-2*DefaultSessionAffinityTTL))
	if got := userOf(p.choose("new", nil)); got != "alice" {
		t.Fatalf("新会话 choose = %s, want alice（过期绑定不计数）", got)
	}
}

// TestCapDivertsNewSessionToLowerPriority 高优先级账号已满时，新会话分流到低优先级账号，而不是回退到已满的高优先级账号。
func TestCapDivertsNewSessionToLowerPriority(t *testing.T) {
	p := newCapPool(t, 1, priorityAccount("alice", 10), priorityAccount("bob", 0))
	p.affinity.bind("old", "alice", time.Now())
	if got := userOf(p.choose("new", nil)); got != "bob" {
		t.Fatalf("新会话 choose = %s, want bob（高优先级的 alice 已满）", got)
	}
}

// TestCapFallsBackWhenEveryAccountIsFull 所有账号都已满时忽略上限，按原最优档轮询选择；此时为 alice，轮询的第一位。
func TestCapFallsBackWhenEveryAccountIsFull(t *testing.T) {
	p := newCapPool(t, 1, priorityAccount("alice", 0), priorityAccount("bob", 0))
	p.affinity.bind("s1", "alice", time.Now())
	p.affinity.bind("s2", "bob", time.Now())
	if got := userOf(p.choose("s3", nil)); got != "alice" {
		t.Fatalf("全部已满时 choose = %s, want alice（忽略上限，按轮询选择）", got)
	}
}

// TestCapDoesNotLimitUsableBinding 已有可用绑定的会话继续由原账号处理，即使该账号已达上限，空闲的 bob 也不改选。
func TestCapDoesNotLimitUsableBinding(t *testing.T) {
	p := newCapPool(t, 1, priorityAccount("alice", 0), priorityAccount("bob", 0))
	p.affinity.bind("s1", "alice", time.Now())
	for i := range 3 {
		if got := userOf(p.choose("s1", nil)); got != "alice" {
			t.Fatalf("请求 %d choose = %s, want alice（绑定可用，不受上限限制）", i+1, got)
		}
	}
}

// TestCapAppliesOnFailover 故障切换时上限同样生效。alice 绑定 s1、bob 绑定 s2，上限 1，两者都已满；carol 空闲。
// 会话 s1 的原账号 alice 在 tried 中或处于冷却时，bob 因已满必须排除，应选中 carol（无上限时轮询先选 bob）。
func TestCapAppliesOnFailover(t *testing.T) {
	accounts := []PoolAccount{priorityAccount("alice", 0), priorityAccount("bob", 0), priorityAccount("carol", 0)}
	setup := func(t *testing.T) *Pool {
		t.Helper()
		p := newCapPool(t, 1, accounts...)
		p.affinity.bind("s1", "alice", time.Now())
		p.affinity.bind("s2", "bob", time.Now())
		return p
	}

	t.Run("绑定账号在 tried 中", func(t *testing.T) {
		p := setup(t)
		tried := map[*poolAccount]bool{p.findAccount("alice"): true}
		if got := userOf(p.choose("s1", tried)); got != "carol" {
			t.Fatalf("故障切换 choose = %s, want carol（bob 已满）", got)
		}
	})
	t.Run("绑定账号处于冷却", func(t *testing.T) {
		p := setup(t)
		coolDown(t, p, "alice")
		if got := userOf(p.choose("s1", nil)); got != "carol" {
			t.Fatalf("绑定账号冷却后 choose = %s, want carol（bob 已满）", got)
		}
	})
}

// TestCapFallbackNeverReturnsTriedAccount 故障切换时已尝试但未满的账号不得被再次选中：alice 已尝试且无绑定，bob 已满，
// 排除后没有候选而回退，回退时仍须排除 alice，选未尝试的 bob。
func TestCapFallbackNeverReturnsTriedAccount(t *testing.T) {
	p := newCapPool(t, 1, priorityAccount("alice", 0), priorityAccount("bob", 0))
	p.affinity.bind("old", "bob", time.Now())
	tried := map[*poolAccount]bool{p.findAccount("alice"): true}
	if got := userOf(p.choose("new", tried)); got != "bob" {
		t.Fatalf("choose = %s, want bob（alice 已尝试过，不得再选）", got)
	}
}

// TestCapDoesNotPushSessionToCoolingAccount 上限不会把新会话推向冷却中的账号：alice 未冷却但已满，bob 冷却中且未满。
// 排除 alice 后只剩冷却的 bob，冷却档会从未冷却降为冷却，因此忽略上限，仍选 alice。
func TestCapDoesNotPushSessionToCoolingAccount(t *testing.T) {
	p := newCapPool(t, 1, priorityAccount("alice", 0), priorityAccount("bob", 0))
	p.affinity.bind("old", "alice", time.Now())
	coolDown(t, p, "bob")
	if got := userOf(p.choose("new", nil)); got != "alice" {
		t.Fatalf("新会话 choose = %s, want alice（不得改选冷却中的 bob）", got)
	}
}

// TestCapStillAppliesWhenEveryAccountIsCooling 全部冷却时上限照常生效：alice 冷却且已满，bob 冷却但未满，新会话选 bob。
// 排除 alice 后冷却档不变，因此上限有效；无上限时轮询会先选 alice。
func TestCapStillAppliesWhenEveryAccountIsCooling(t *testing.T) {
	p := newCapPool(t, 1, priorityAccount("alice", 0), priorityAccount("bob", 0))
	p.affinity.bind("old", "alice", time.Now())
	coolDown(t, p, "alice")
	coolDown(t, p, "bob")
	if got := userOf(p.choose("new", nil)); got != "bob" {
		t.Fatalf("全部冷却时 choose = %s, want bob（冷却档相同，上限生效）", got)
	}
}

// TestCapIgnoredForEmptyKey 键为空（没有会话信号）的请求不受上限限制：alice 已满，两次无键选择仍按轮询依次为 alice、bob。
func TestCapIgnoredForEmptyKey(t *testing.T) {
	p := newCapPool(t, 1, priorityAccount("alice", 0), priorityAccount("bob", 0))
	p.affinity.bind("old", "alice", time.Now())
	assertChooseSequence(t, p, "alice", "bob")
}

// TestCapZeroMeansUnlimited 上限为 0 表示不限制：alice 即使已有多条绑定，仍是优先级最高的账号，新会话照常选中它。
func TestCapZeroMeansUnlimited(t *testing.T) {
	p := newCapPool(t, 0, priorityAccount("alice", 10), priorityAccount("bob", 0))
	for _, key := range []string{"old1", "old2", "old3"} {
		p.affinity.bind(key, "alice", time.Now())
	}
	if got := userOf(p.choose("new", nil)); got != "alice" {
		t.Fatalf("上限 0 时 choose = %s, want alice（不限制）", got)
	}
}

// TestCapHasNoEffectWhenAffinityDisabled 关闭会话亲和（TTL 为 0，没有绑定表）时上限无效：带会话键的选择不会因上限跳过账号，
// 结果与轮询一致（alice、bob），也不会访问不存在的绑定表。
func TestCapHasNoEffectWhenAffinityDisabled(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{
		CacheDir: t.TempDir(), SessionAffinityTTL: 0, SessionAffinityMaxPerAccount: 1,
	}, []PoolAccount{priorityAccount("alice", 0), priorityAccount("bob", 0)})
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)
	for i, want := range []string{"alice", "bob"} {
		if got := userOf(p.choose("session-key", nil)); got != want {
			t.Fatalf("亲和关闭时第 %d 次 choose = %s, want %s", i+1, got, want)
		}
	}
}

// TestCapHTTPSpreadsNewSessionsAcrossAccounts 通过 HTTP 入口：上限 1，alice 与 bob 两个账号，三个不同的会话。
// 会话 1 绑定 alice；会话 2 因 alice 已满而绑定 bob；会话 3 两者都已满，回退到原最优档的轮询（此时为 bob），并照常绑定。
// 之后会话 1 仍由 alice 处理。
func TestCapHTTPSpreadsNewSessionsAcrossAccounts(t *testing.T) {
	up := newPoolUpstream(t)
	p, err := NewManagedPool(PoolConfig{
		CacheDir: t.TempDir(), Client: up.client(), SessionAffinityTTL: DefaultSessionAffinityTTL, SessionAffinityMaxPerAccount: 1,
	}, []PoolAccount{priorityAccount("alice", 0), priorityAccount("bob", 0)})
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)

	bodies := []string{affinityChatBody(t, "session one"), affinityChatBody(t, "session two"), affinityChatBody(t, "session three")}
	for i, body := range bodies {
		if res := poolRequest(p, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
			t.Fatalf("会话 %d status=%d body=%s", i+1, res.Code, res.Body.String())
		}
	}
	assertGenerationOrder(t, up, mgmtOrder("alice", "bob", "bob")...)

	wantBound := []string{"alice", "bob", "bob"}
	for i, body := range bodies {
		id, ok := p.affinity.lookup(affinityPoolKey(t, body), time.Now())
		if !ok || id != wantBound[i] {
			t.Fatalf("会话 %d 绑定 = %q, %v; want %s, true", i+1, id, ok, wantBound[i])
		}
	}

	if res := poolRequest(p, "POST", "/v1/chat/completions", bodies[0]); res.Code != http.StatusOK {
		t.Fatalf("会话 1 再次请求 status=%d body=%s", res.Code, res.Body.String())
	}
	assertGenerationOrder(t, up, mgmtOrder("alice", "bob", "bob", "alice")...)
}
