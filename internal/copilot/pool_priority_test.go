package copilot

// 本文件覆盖账号优先级对选择顺序的影响。Pool.choose 对候选账号排序后取第一个，排序键依次为：
// 冷却档（未冷却优先）、优先级（高者在前）、额度权重（有 Credits 高于无 Credits，同档内值高者在前）、
// 凭证文件名（用户名 + ".json"）的字节序升序。

import (
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

// priorityAccount 返回用户名为 user、优先级为 priority 的账号配置，token 为 "oauth-" + user。
func priorityAccount(user string, priority int) PoolAccount {
	return PoolAccount{Token: "oauth-" + user, User: user, Priority: priority}
}

// newPriorityPool 用 NewManagedPool 创建池（不启用亲和与管理接口），账号按传入顺序加入。
// client 为 nil 时池不会访问上游，只用于测试 choose；需要请求上游时传入测试客户端。
func newPriorityPool(t *testing.T, client *http.Client, accounts ...PoolAccount) *Pool {
	t.Helper()
	p, err := NewManagedPool(PoolConfig{CacheDir: t.TempDir(), Client: client}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)
	return p
}

// userOf 返回账号的用户名；没有候选（nil）时返回 "<nil>"，便于错误信息打印。
func userOf(account *poolAccount) string {
	if account == nil {
		return "<nil>"
	}
	return account.service.config.User
}

// rankedUsers 通过逐次排除已选中的账号，得到 choose 给出的完整候选顺序（不含禁用账号）。
// 冷却档只在未冷却账号耗尽后才会出现，因此冷却账号排在未冷却账号之后。
func rankedUsers(p *Pool) []string {
	tried := map[*poolAccount]bool{}
	var order []string
	for {
		account := p.choose("", tried)
		if account == nil {
			return order
		}
		order = append(order, userOf(account))
		tried[account] = true
	}
}

// assertRanking 断言 choose 的候选顺序与 want 完全一致。
func assertRanking(t *testing.T, p *Pool, want ...string) {
	t.Helper()
	if got := rankedUsers(p); !slices.Equal(got, want) {
		t.Fatalf("候选顺序 = %v, want %v", got, want)
	}
}

// creditsBucket 与 budgetBucket 构造主额度（Credits）与 Additional Budget 额度窗口。
func creditsBucket(fraction float64) QuotaBucket {
	return QuotaBucket{Window: "credits", Description: "Credits", RemainingFraction: fraction}
}

func budgetBucket(fraction float64) QuotaBucket {
	return QuotaBucket{Window: "additional_budget", Description: "Additional Budget", RemainingFraction: fraction}
}

// quotaOf 把额度窗口放入唯一的一个组中，结构与 parseQuota 的输出一致。
func quotaOf(buckets ...QuotaBucket) *Quota {
	return &Quota{Groups: []QuotaGroup{{DisplayName: "GitHub Copilot", Buckets: buckets}}}
}

// setQuota 直接设置账号缓存的额度，choose 只读取它来计算权重。
func setQuota(t *testing.T, p *Pool, user string, quota *Quota) {
	t.Helper()
	account := p.findAccount(user)
	if account == nil {
		t.Fatalf("缺少账号 %q", user)
	}
	account.quota.mu.Lock()
	account.quota.data = quota
	account.quota.mu.Unlock()
}

// coolDown 让账号在一小时内处于冷却中。
func coolDown(t *testing.T, p *Pool, user string) {
	t.Helper()
	account := p.findAccount(user)
	if account == nil {
		t.Fatalf("缺少账号 %q", user)
	}
	account.service.setCooldown(time.Now().Add(time.Hour))
}

// TestChooseWithoutPriorityOrQuotaPicksSmallestFileName 优先级与额度都相同（或没有额度数据）时，
// 选择凭证文件名字节序最小的账号；多次选择结果相同，不再轮流使用。
func TestChooseWithoutPriorityOrQuotaPicksSmallestFileName(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("carol", 0), priorityAccount("alice", 0), priorityAccount("bob", 0))
	for i := range 3 {
		if got := userOf(p.choose("", nil)); got != "alice" {
			t.Fatalf("第 %d 次 choose = %s, want alice", i, got)
		}
	}
	assertRanking(t, p, "alice", "bob", "carol")
}

// TestChoosePriorityOutranksQuotaWeight 优先级高的账号排在前面，即使另一个账号的额度权重更高。
func TestChoosePriorityOutranksQuotaWeight(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("alice", 0), priorityAccount("bob", 5), priorityAccount("carol", -1))
	setQuota(t, p, "alice", quotaOf(creditsBucket(1)))
	setQuota(t, p, "carol", quotaOf(creditsBucket(1)))
	assertRanking(t, p, "bob", "alice", "carol")
}

// TestChoosePriorityBoundaries 优先级的边界值：100 排在最前，-1 排在最后。
func TestChoosePriorityBoundaries(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("alice", 0), priorityAccount("bob", -1), priorityAccount("carol", 100))
	assertRanking(t, p, "carol", "alice", "bob")
}

// TestChooseEqualPriorityBrokenByQuotaWeight 优先级相同时，额度权重高的账号在前；没有额度数据的账号权重为 0。
func TestChooseEqualPriorityBrokenByQuotaWeight(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("alice", 3), priorityAccount("bob", 3), priorityAccount("carol", 3))
	setQuota(t, p, "bob", quotaOf(creditsBucket(0.2)))
	// alice 的 Credits 为 0 且没有 budget，与没有额度数据的 carol 同为权重 0，按文件名排序。
	setQuota(t, p, "alice", quotaOf(creditsBucket(0)))
	assertRanking(t, p, "bob", "alice", "carol")
}

// TestChooseQuotaWeightSpecExample 规格中的例子：A（credits 0.1）> B（credits 0，budget 0.9）
// > C（credits 0，budget 0.5）> D（无额度数据）。账号名的字节序与权重顺序相反，排序必须以权重为先。
func TestChooseQuotaWeightSpecExample(t *testing.T) {
	p := newPriorityPool(t, nil,
		priorityAccount("alpha", 0),   // D：无额度数据
		priorityAccount("bravo", 0),   // C
		priorityAccount("charlie", 0), // B
		priorityAccount("delta", 0),   // A
	)
	setQuota(t, p, "delta", quotaOf(creditsBucket(0.1)))
	setQuota(t, p, "charlie", quotaOf(creditsBucket(0), budgetBucket(0.9)))
	setQuota(t, p, "bravo", quotaOf(creditsBucket(0), budgetBucket(0.5)))
	assertRanking(t, p, "delta", "charlie", "bravo", "alpha")
}

// TestChooseQuotaWeightRules 额度权重的档位与取值规则。三个账号优先级相同，alpha、bravo、charlie 的文件名字节序固定，
// 因此结果只取决于权重。
func TestChooseQuotaWeightRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		quotas map[string]*Quota // 未列出的账号没有额度数据
		want   []string
	}{
		{
			name:   "credits-compared-by-value",
			quotas: map[string]*Quota{"alpha": quotaOf(creditsBucket(0.2)), "bravo": quotaOf(creditsBucket(0.7))},
			want:   []string{"bravo", "alpha", "charlie"},
		},
		{
			name:   "any-credits-beats-budget",
			quotas: map[string]*Quota{"alpha": quotaOf(budgetBucket(1)), "bravo": quotaOf(creditsBucket(0.01))},
			want:   []string{"bravo", "alpha", "charlie"},
		},
		{
			name: "premium-requests-is-main-bucket",
			quotas: map[string]*Quota{
				"alpha": quotaOf(budgetBucket(0.9)),
				"bravo": quotaOf(QuotaBucket{Window: "premium_requests", Description: "Premium requests", RemainingFraction: 0.3}),
			},
			want: []string{"bravo", "alpha", "charlie"},
		},
		{
			name:   "main-bucket-found-after-budget",
			quotas: map[string]*Quota{"alpha": quotaOf(budgetBucket(0.2), creditsBucket(0.1)), "bravo": quotaOf(budgetBucket(1))},
			want:   []string{"alpha", "bravo", "charlie"},
		},
		{
			name:   "zero-credits-falls-back-to-budget",
			quotas: map[string]*Quota{"alpha": quotaOf(creditsBucket(0), budgetBucket(0.5)), "bravo": quotaOf(creditsBucket(0))},
			want:   []string{"alpha", "bravo", "charlie"},
		},
		{
			name:   "zero-credits-without-budget-ties-with-no-data",
			quotas: map[string]*Quota{"alpha": quotaOf(creditsBucket(0)), "charlie": quotaOf(budgetBucket(0))},
			want:   []string{"alpha", "bravo", "charlie"},
		},
		{
			name:   "budget-beats-no-quota",
			quotas: map[string]*Quota{"bravo": quotaOf(budgetBucket(0.1))},
			want:   []string{"bravo", "alpha", "charlie"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPriorityPool(t, nil, priorityAccount("alpha", 0), priorityAccount("bravo", 0), priorityAccount("charlie", 0))
			for user, quota := range tc.quotas {
				setQuota(t, p, user, quota)
			}
			assertRanking(t, p, tc.want...)
		})
	}
}

// TestChooseTiesBrokenByFileNameBytes 优先级与权重都相同时，按凭证文件名（用户名 + ".json"）的字节序升序排列：
// 连字符（0x2D）小于点（0x2E），大写字母小于小写字母，数字按字符比较（"10" 在 "9" 之前）。
// 账号按与期望相反的顺序加入，排除加入顺序的影响。
func TestChooseTiesBrokenByFileNameBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []string // 期望的候选顺序（用户名）
	}{
		{name: "hyphen-before-dot", want: []string{"a-b", "a"}},              // a-b.json < a.json
		{name: "dot-before-letter", want: []string{"a", "ab"}},               // a.json < ab.json
		{name: "uppercase-before-lowercase", want: []string{"Bob", "alice"}}, // Bob.json < alice.json
		{name: "digits-by-character", want: []string{"10", "9"}},             // 10.json < 9.json
		{name: "hyphen-before-underscore", want: []string{"a-b", "a_b"}},     // a-b.json < a_b.json
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := make([]PoolAccount, 0, len(tc.want))
			for i := len(tc.want) - 1; i >= 0; i-- {
				accounts = append(accounts, priorityAccount(tc.want[i], 0))
			}
			assertRanking(t, newPriorityPool(t, nil, accounts...), tc.want...)
		})
	}
}

// TestChooseNeverPicksDisabledAccount 禁用账号即使优先级最高也不会被选中；全部禁用时没有候选。
func TestChooseNeverPicksDisabledAccount(t *testing.T) {
	p := newPriorityPool(t, nil,
		PoolAccount{Token: "oauth-alice", User: "alice", Priority: 100, Disabled: true},
		priorityAccount("bob", 0),
	)
	assertRanking(t, p, "bob")

	none := newPriorityPool(t, nil, PoolAccount{Token: "oauth-alice", User: "alice", Disabled: true})
	if account := none.choose("", nil); account != nil {
		t.Fatalf("全部禁用时 choose = %s, want nil", userOf(account))
	}
}

// TestChooseCoolingAccountsAreLastResort 冷却中的账号只在未冷却账号耗尽后才会被选中，即使它的优先级更高。
func TestChooseCoolingAccountsAreLastResort(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("alice", 100), priorityAccount("bob", 0), priorityAccount("carol", -1))
	coolDown(t, p, "alice")
	assertRanking(t, p, "bob", "carol", "alice")
}

// TestChooseAllCoolingUsesSameOrdering 全部冷却时进入冷却档，并按同样的优先级与文件名规则排序。
func TestChooseAllCoolingUsesSameOrdering(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("alice", 0), priorityAccount("bob", 5), priorityAccount("carol", 5))
	for _, user := range []string{"alice", "bob", "carol"} {
		coolDown(t, p, user)
	}
	assertRanking(t, p, "bob", "carol", "alice")
}

// TestChooseCooldownExpiryRestoresAccount 冷却截止时间过去后，账号重新参与选择，按优先级回到原来的位置。
func TestChooseCooldownExpiryRestoresAccount(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("alice", 100), priorityAccount("bob", 0))
	coolDown(t, p, "alice")
	assertRanking(t, p, "bob", "alice")
	// 不等待真实时间：把冷却截止时间设为已经过去。
	p.findAccount("alice").service.setCooldown(time.Now().Add(-time.Second))
	assertRanking(t, p, "alice", "bob")
}

// TestChooseSkipsTriedAccounts 本请求已尝试过的账号被排除，即使它的优先级最高。
func TestChooseSkipsTriedAccounts(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("alice", 100), priorityAccount("bob", 0), priorityAccount("carol", 0))
	tried := map[*poolAccount]bool{p.findAccount("alice"): true}
	if got := userOf(p.choose("", tried)); got != "bob" {
		t.Fatalf("alice 已尝试时 choose = %s, want bob", got)
	}
	tried[p.findAccount("bob")] = true
	if got := userOf(p.choose("", tried)); got != "carol" {
		t.Fatalf("alice、bob 已尝试时 choose = %s, want carol", got)
	}
	tried[p.findAccount("carol")] = true
	if account := p.choose("", tried); account != nil {
		t.Fatalf("所有账号都已尝试时 choose = %s, want nil", userOf(account))
	}
}

// TestChooseConcurrentCallsAgreeOnOneAccount 选择没有轮询游标，并发调用得到的结果一致。
func TestChooseConcurrentCallsAgreeOnOneAccount(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("carol", 0), priorityAccount("alice", 0), priorityAccount("bob", 0))
	const workers, perWorker = 30, 50
	results := make(chan string, workers*perWorker)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range perWorker {
				results <- userOf(p.choose("", nil))
			}
		}()
	}
	wait.Wait()
	close(results)
	for user := range results {
		if user != "alice" {
			t.Fatalf("并发 choose 返回 %s, want alice", user)
		}
	}
}

// TestPoolFailoverPicksNextBestAccountByPriority 最优账号的 session 失败后，同一请求改用下一个最优账号；
// 之后最优账号处于冷却中，后续请求仍由下一个最优账号处理，且不再尝试最优账号。
func TestPoolFailoverPicksNextBestAccountByPriority(t *testing.T) {
	upstream := newPoolUpstream(t)
	upstream.failSession("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"alice down"}`})
	p := newPriorityPool(t, upstream.client(), priorityAccount("alice", 10), priorityAccount("bob", 5), priorityAccount("carol", 0))
	for i := range 2 {
		if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
			t.Fatalf("请求 %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	assertGenerationOrder(t, upstream, "oauth-bob", "oauth-bob")
	if got := upstream.sessionCount("oauth-alice"); got != 1 {
		t.Errorf("alice 的 session 交换 %d 次, want 1（冷却后不再尝试）", got)
	}
	if got := upstream.sessionCount("oauth-carol"); got != 0 {
		t.Errorf("carol 的 session 交换 %d 次, want 0（bob 优先级更高，不应被尝试）", got)
	}
}
