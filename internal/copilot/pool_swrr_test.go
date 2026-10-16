package copilot

// 本文件覆盖同一档内的平滑加权轮询（SWRR，算法同 nginx）。“同档”指未冷却、优先级相同、且同为有 Credits 或同为无 Credits 的账号；
// 档位之间是严格的先后关系，档内按权重（见 quotaWeight.swrrWeight）轮流被选中。
// 期望序列按手算的 SWRR 给出：每次选择时，组内每个账号的 current 加上自己的权重，选 current 最大者（并列按文件名升序），
// 再把选中者的 current 减去组内总权重。测试只通过 choose、pickLocked 与 HTTP 入口观察结果，不读取内部状态。

import (
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

// swrrEntry 是一种选择入口。choose 是生产路径（无会话键）；pickLocked 是选择核心，以当前时刻直接调用。
type swrrEntry struct {
	name string
	pick func(p *Pool) *poolAccount
}

// swrrEntries 返回两种入口，同一组期望需要在两者上都成立。
func swrrEntries() []swrrEntry {
	return []swrrEntry{
		{name: "choose", pick: func(p *Pool) *poolAccount { return p.choose("", nil) }},
		{name: "pickLocked", pick: func(p *Pool) *poolAccount { return pickLockedAt(p, time.Now(), nil) }},
	}
}

// TestSwrrSequenceFollowsWeights 权重 5:1:1（a 的 Credits 为 0.05，b、c 为 0.01）时，连续 7 次选择的序列为 a a b a c a a。
// 一个周期结束后状态回到起点，因此第二个周期重复同样的序列。choose 与 pickLocked 的结果一致。
func TestSwrrSequenceFollowsWeights(t *testing.T) {
	cycle := []string{"a", "a", "b", "a", "c", "a", "a"}
	want := append(slices.Clone(cycle), cycle...)
	for _, entry := range swrrEntries() {
		t.Run(entry.name, func(t *testing.T) {
			p := newPriorityPool(t, nil, priorityAccount("a", 0), priorityAccount("b", 0), priorityAccount("c", 0))
			setQuota(t, p, "a", quotaOf(creditsBucket(0.05)))
			setQuota(t, p, "b", quotaOf(creditsBucket(0.01)))
			setQuota(t, p, "c", quotaOf(creditsBucket(0.01)))
			got := make([]string, 0, len(want))
			for range want {
				got = append(got, userOf(entry.pick(p)))
			}
			if !slices.Equal(got, want) {
				t.Fatalf("选择序列 = %v, want %v", got, want)
			}
		})
	}
}

// TestSwrrCreditsTierBeatsBudgetWeight 严格分档：有 Credits 的账号（权重 1）排在只剩 Additional Budget 的账号（权重 100）之前。
// 档位先于权重比较，因此 x 每次都被选中，y 没有机会。
func TestSwrrCreditsTierBeatsBudgetWeight(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("x", 0), priorityAccount("y", 0))
	setQuota(t, p, "x", quotaOf(creditsBucket(0.01)))
	setQuota(t, p, "y", quotaOf(creditsBucket(0), budgetBucket(1)))
	assertChooseSequence(t, p, "x", "x", "x", "x", "x")
}

// TestSwrrHigherPriorityBeatsHigherWeight 严格分档：优先级高的账号（权重 1）排在优先级低的账号（权重 100）之前，每次都由它被选中。
func TestSwrrHigherPriorityBeatsHigherWeight(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("alice", 5), priorityAccount("bob", 0))
	setQuota(t, p, "alice", quotaOf(creditsBucket(0.01)))
	setQuota(t, p, "bob", quotaOf(creditsBucket(1)))
	assertChooseSequence(t, p, "alice", "alice", "alice")
}

// TestSwrrOnlyTopGroupAdvances 只有最优组内成员的轮询状态会变化。alice 优先级最高，先被选中三次（每次都单独成组）；
// 之后排除 alice，bob 与 carol 构成最优组，它们的轮询从各自的状态开始，不受 alice 那三次选择的影响。
// 若 alice 的选择误推进了 bob、carol 的状态，权重不等时序列会偏离下面的期望。
func TestSwrrOnlyTopGroupAdvances(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bob, carol float64 // 两者的 Credits 剩余比例，决定权重
		want       []string
	}{
		{name: "equal-weights", bob: 0.01, carol: 0.01, want: []string{"bob", "carol", "bob", "carol"}},
		{name: "unequal-weights-2-to-1", bob: 0.02, carol: 0.01, want: []string{"bob", "carol", "bob", "bob", "carol", "bob"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPriorityPool(t, nil, priorityAccount("alice", 5), priorityAccount("bob", 0), priorityAccount("carol", 0))
			setQuota(t, p, "alice", quotaOf(creditsBucket(1)))
			setQuota(t, p, "bob", quotaOf(creditsBucket(tc.bob)))
			setQuota(t, p, "carol", quotaOf(creditsBucket(tc.carol)))
			for range 3 {
				if got := userOf(p.choose("", nil)); got != "alice" {
					t.Fatalf("alice 优先级最高，choose = %s, want alice", got)
				}
			}
			tried := map[*poolAccount]bool{p.findAccount("alice"): true}
			got := make([]string, 0, len(tc.want))
			for range tc.want {
				got = append(got, userOf(p.choose("", tried)))
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("排除 alice 后的序列 = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSwrrNoCreditsTierUsesBudgetWeight 同档账号都没有 Credits 时，档内按 Additional Budget 的剩余比例轮询。
// budget-weights：d 剩 0.8、e 剩 0.2，权重 4:1，一个周期为 d d e d d，两个周期序列相同。
// no-quota-data：没有额度数据的账号同属无 Credits 档、权重 1，与只剩 0.01 Budget（权重 1）的账号平分。
func TestSwrrNoCreditsTierUsesBudgetWeight(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quota map[string]*Quota // nil 表示不设置额度数据
		want  []string
	}{
		{
			name: "budget-weights",
			quota: map[string]*Quota{
				"d": quotaOf(creditsBucket(0), budgetBucket(0.8)),
				"e": quotaOf(creditsBucket(0), budgetBucket(0.2)),
			},
			want: []string{"d", "d", "e", "d", "d", "d", "d", "e", "d", "d"},
		},
		{
			name: "no-quota-data",
			quota: map[string]*Quota{
				"d": quotaOf(creditsBucket(0), budgetBucket(0.01)),
				"e": nil,
			},
			want: []string{"d", "e", "d", "e"},
		},
	} {
		for _, entry := range swrrEntries() {
			t.Run(tc.name+"/"+entry.name, func(t *testing.T) {
				p := newPriorityPool(t, nil, priorityAccount("d", 0), priorityAccount("e", 0))
				for user, quota := range tc.quota {
					if quota != nil {
						setQuota(t, p, user, quota)
					}
				}
				got := make([]string, 0, len(tc.want))
				for range tc.want {
					got = append(got, userOf(entry.pick(p)))
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("选择序列 = %v, want %v", got, tc.want)
				}
			})
		}
	}
}

// TestSwrrTiesBrokenByFileNameWhenCurrentEqual 同档内 current 相同时按凭证文件名升序，与加入顺序无关。
// carol、bob 的权重都为 2，首次并列时选 bob；alice 权重为 1，要等权重累积后才轮到，之后继续按轮询。
// 加入顺序是 carol、bob、alice，与文件名顺序相反。
func TestSwrrTiesBrokenByFileNameWhenCurrentEqual(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("carol", 0), priorityAccount("bob", 0), priorityAccount("alice", 0))
	setQuota(t, p, "carol", quotaOf(creditsBucket(0.02)))
	setQuota(t, p, "bob", quotaOf(creditsBucket(0.02)))
	setQuota(t, p, "alice", quotaOf(creditsBucket(0.01)))
	assertChooseSequence(t, p, "bob", "carol", "alice", "bob", "carol", "bob")
}

// TestSwrrConcurrentPicksSplitByWeight 并发选择（配合 -race）：权重 5:1:1 下，420 次选择恰好是 7 次一个周期的 60 倍，
// 因此 a 300 次、b 60 次、c 60 次。选择在 p.mu 下串行推进状态，与并发的先后无关。
func TestSwrrConcurrentPicksSplitByWeight(t *testing.T) {
	p := newPriorityPool(t, nil, priorityAccount("a", 0), priorityAccount("b", 0), priorityAccount("c", 0))
	setQuota(t, p, "a", quotaOf(creditsBucket(0.05)))
	setQuota(t, p, "b", quotaOf(creditsBucket(0.01)))
	setQuota(t, p, "c", quotaOf(creditsBucket(0.01)))
	const workers, perWorker = 30, 14
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
	counts := map[string]int{}
	for user := range results {
		counts[user]++
	}
	if want := map[string]int{"a": 300, "b": 60, "c": 60}; !maps.Equal(counts, want) {
		t.Fatalf("并发选择次数 = %v, want %v", counts, want)
	}
}

// TestSwrrHTTPRoundRobinByWeight 通过 HTTP 入口：a 的权重为 2（Credits 0.02），b、c 为 1（Credits 0.01），三者同档。
// 四次请求依次由 a、b、c、a 处理，之后周期重复。
func TestSwrrHTTPRoundRobinByWeight(t *testing.T) {
	upstream := newPoolUpstream(t)
	p := testPool(t, poolUserConfigs(t, upstream.client(), "a", "b", "c"))
	setQuota(t, p, "a", quotaOf(creditsBucket(0.02)))
	setQuota(t, p, "b", quotaOf(creditsBucket(0.01)))
	setQuota(t, p, "c", quotaOf(creditsBucket(0.01)))
	for i := range 4 {
		if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
			t.Fatalf("请求 %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	assertGenerationOrder(t, upstream, "oauth-a", "oauth-b", "oauth-c", "oauth-a")
}

// TestSwrrBindingHitDoesNotAdvance 绑定命中时直接使用绑定账号，不推进轮询。a 权重 5、b 权重 1，会话 s 绑定到 b：
// 连续 5 次带键选择都返回 b，之后 12 次无键选择的结果应与同配置新池的前 12 次完全相同。
// 手算的无键序列为 a a a b a a 循环。若命中分支额外调用了 pickLocked，每次命中都会推进状态，后续序列会偏离（第 4 次变为 a）。
func TestSwrrBindingHitDoesNotAdvance(t *testing.T) {
	const picks = 12
	newWeightedPool := func(t *testing.T) *Pool {
		t.Helper()
		p := newAffinityPriorityPool(t, priorityAccount("a", 0), priorityAccount("b", 0))
		setQuota(t, p, "a", quotaOf(creditsBucket(0.05)))
		setQuota(t, p, "b", quotaOf(creditsBucket(0.01)))
		return p
	}

	fresh := newWeightedPool(t)
	want := make([]string, 0, picks)
	for range picks {
		want = append(want, userOf(fresh.choose("", nil)))
	}

	p := newWeightedPool(t)
	p.affinity.bind("s", "b", time.Now())
	for i := range 5 {
		if got := userOf(p.choose("s", nil)); got != "b" {
			t.Fatalf("绑定命中第 %d 次 choose = %s, want b", i+1, got)
		}
	}
	got := make([]string, 0, picks)
	for range picks {
		got = append(got, userOf(p.choose("", nil)))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("命中之后的无键序列 = %v, want %v（与同配置新池一致）", got, want)
	}
}
