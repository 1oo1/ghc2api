package copilot

// 本文件覆盖额度刷新的三类入口，断言的是可观察的行为而非内部实现：
//   - 刷新间隔：Config / PoolConfig 的 QuotaRefreshInterval 决定非强制刷新的节流窗口；
//   - 后台定时刷新 RunQuotaRefresh：启动即刷新一次，之后每隔 quotaRefreshTick 刷新一轮，直到 ctx 取消；
//   - 请求失败时的异步刷新：非 429 的失败（生成请求或会话获取）会异步强制刷新该账号额度，
//     受 quotaErrorRefreshInterval 与“正在刷新”标志限制。
// “应当发生”的事件通过轮询上游请求次数或账号 checkedAt 等待，超时即终止测试，不用固定时长的 sleep。
// “不应发生”的请求只能在 quotaSettleWindow 内观察，因为实现没有可观察的钩子。
// 注意：刷新在写入额度缓存时持有 quotaCache.mu，因此读到新的 checkedAt 时 quota.json 已经写完。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// quotaAsyncTimeout 是等待异步刷新完成的上限。
const quotaAsyncTimeout = 5 * time.Second

// quotaSettleWindow 是断言“不应发生的请求”时的观察窗口。
const quotaSettleWindow = 200 * time.Millisecond

// quotaExhaustedBody 是个人版账号的耗尽额度：Premium 剩余 0%，重置日期为 2099 年，使账号在测试期间一直保持耗尽。
const quotaExhaustedBody = `{"copilot_plan":"individual","quota_reset_date":"2099-01-01","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":0}}}`

// setQuotaRefreshTick 临时修改后台刷新的轮询间隔，测试结束时恢复。
func setQuotaRefreshTick(t *testing.T, d time.Duration) {
	t.Helper()
	previous := quotaRefreshTick
	quotaRefreshTick = d
	t.Cleanup(func() { quotaRefreshTick = previous })
}

// setQuotaErrorRefreshInterval 临时修改请求失败后异步刷新的节流间隔，测试结束时恢复。
func setQuotaErrorRefreshInterval(t *testing.T, d time.Duration) {
	t.Helper()
	previous := quotaErrorRefreshInterval
	quotaErrorRefreshInterval = d
	t.Cleanup(func() { quotaErrorRefreshInterval = previous })
}

// setQuotaRefreshTimeout 临时修改额度刷新的总超时，测试结束时恢复。
func setQuotaRefreshTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := quotaRefreshTimeout
	quotaRefreshTimeout = d
	t.Cleanup(func() { quotaRefreshTimeout = previous })
}

// quotaWaitFor 轮询直到 cond 返回 true；超过 quotaAsyncTimeout 仍不满足则终止测试。
func quotaWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(quotaAsyncTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// quotaCheckedAt 读取账号最近一次尝试的时间（加锁）。
func quotaCheckedAt(a *poolAccount) time.Time {
	a.quota.mu.Lock()
	defer a.quota.mu.Unlock()
	return a.quota.checkedAt
}

// quotaSetCheckedAt 把账号最近一次尝试的时间设为 at（加锁）。设为过去的时刻即可让下一次非强制刷新不再节流。
// 调用前须确保账号没有进行中的刷新，否则刷新结束时会覆盖这里设置的值，见 quotaWaitAttempt。
func quotaSetCheckedAt(a *poolAccount, at time.Time) {
	a.quota.mu.Lock()
	defer a.quota.mu.Unlock()
	a.quota.checkedAt = at
}

// quotaWaitAttempt 等待账号的一次额度刷新完成：checked_at 晚于 since。since 为零值时等待首次尝试。
func quotaWaitAttempt(t *testing.T, a *poolAccount, since time.Time) {
	t.Helper()
	quotaWaitFor(t, "额度刷新完成", func() bool { return quotaCheckedAt(a).After(since) })
}

// quotaStaysAt 在 quotaSettleWindow 内反复确认 token 的额度请求次数保持为 want，用于断言“不应发生”的刷新。
func quotaStaysAt(t *testing.T, up *mgmtUpstream, token string, want int) {
	t.Helper()
	deadline := time.Now().Add(quotaSettleWindow)
	for {
		if got := up.quotaCount(token); got != want {
			t.Fatalf("%s 的额度请求次数 = %d, want %d（不应触发刷新）", token, got, want)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// quotaRefreshing 报告账号是否有异步刷新（见 refreshAsync）正在进行，加锁读取。
func quotaRefreshing(a *poolAccount) bool {
	a.quota.mu.Lock()
	defer a.quota.mu.Unlock()
	return a.quota.refreshing
}

// quotaAwaitIdle 注册清理函数，测试结束前等待池内所有账号的异步额度刷新结束。
// 异步刷新由请求失败（非 429）或新增账号触发，结束前会写入账号缓存目录；测试先结束时，t.TempDir 的清理会与写入竞争
// （RemoveAll: directory not empty）。建池之后立即调用：t.Cleanup 后进先出，因此它先于 t.TempDir 与测试服务器的清理执行。
// 异步刷新的上游请求受 quotaRefreshTimeout 约束，因此等待上限为 quotaRefreshTimeout 加 quotaAsyncTimeout。
func quotaAwaitIdle(t *testing.T, p *Pool) {
	t.Helper()
	t.Cleanup(func() {
		deadline := time.Now().Add(quotaRefreshTimeout + quotaAsyncTimeout)
		for _, a := range p.accountsSnapshot() {
			for quotaRefreshing(a) {
				if time.Now().After(deadline) {
					t.Errorf("测试结束时账号 %s 的异步额度刷新仍未完成", a.id)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	})
}

// quotaGenerateAsync 在另一个协程发送生成请求，响应通过返回的 channel 交付。ctx 作为请求上下文，可用于模拟客户端断开。
func quotaGenerateAsync(p *Pool, ctx context.Context) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(poolChatBody)).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, r)
		done <- w
	}()
	return done
}

// quotaAwaitResponse 等待请求返回并交出响应；超时即终止测试，用于断言异步刷新不阻塞响应。
func quotaAwaitResponse(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(quotaAsyncTimeout):
		t.Fatal("生成请求没有返回：额度刷新不应阻塞响应")
		return nil
	}
}

// quotaRefreshRun 是后台运行 RunQuotaRefresh 的句柄。
type quotaRefreshRun struct {
	cancel context.CancelFunc
	done   chan struct{} // RunQuotaRefresh 返回时关闭
}

// startQuotaRefresh 在后台运行 p.RunQuotaRefresh。测试结束时自动取消并等待其返回，
// 保证后台协程不会在临时目录被删除后仍在写入缓存。
func startQuotaRefresh(t *testing.T, p *Pool) *quotaRefreshRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &quotaRefreshRun{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(run.done)
		p.RunQuotaRefresh(ctx)
	}()
	t.Cleanup(func() {
		run.cancel()
		run.waitReturned(t)
	})
	return run
}

// waitReturned 等待 RunQuotaRefresh 返回（调用前须已取消 ctx）；超时即终止测试。
func (r *quotaRefreshRun) waitReturned(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(quotaAsyncTimeout):
		t.Fatal("RunQuotaRefresh 取消后没有在超时内返回")
	}
}

// quotaIntervalCases 是 Config 与 PoolConfig 共用的间隔用例：正值原样生效，非正值使用 DefaultQuotaRefreshInterval。
var quotaIntervalCases = []struct {
	name     string
	interval time.Duration // 配置的 QuotaRefreshInterval
	want     time.Duration // 生效的节流窗口
}{
	{name: "configured", interval: 10 * time.Minute, want: 10 * time.Minute},
	{name: "zero-uses-default", interval: 0, want: DefaultQuotaRefreshInterval},
	{name: "negative-uses-default", interval: -time.Minute, want: DefaultQuotaRefreshInterval},
}

// assertQuotaIntervalGovernsRefresh 断言账号按 want 节流：距上次尝试 want-1 分钟时非强制刷新不请求上游，
// 距上次尝试 want+1 分钟时请求上游。首次刷新没有尝试记录，必定请求。
func assertQuotaIntervalGovernsRefresh(t *testing.T, a *poolAccount, up *mgmtUpstream, token string, want time.Duration) {
	t.Helper()
	if got := a.service.quotaRefreshInterval; got != want {
		t.Fatalf("Service.quotaRefreshInterval = %s, want %s", got, want)
	}
	ctx := context.Background()
	if err := a.refresh(ctx, false); err != nil {
		t.Fatalf("首次刷新 error = %v", err)
	}
	if got := up.quotaCount(token); got != 1 {
		t.Fatalf("首次刷新后额度请求次数 = %d, want 1", got)
	}
	quotaSetCheckedAt(a, time.Now().Add(-(want - time.Minute)))
	if err := a.refresh(ctx, false); err != nil {
		t.Fatalf("距上次尝试 %s 时刷新 error = %v", want-time.Minute, err)
	}
	if got := up.quotaCount(token); got != 1 {
		t.Fatalf("距上次尝试 %s 时不应请求上游，额度请求次数 = %d, want 1", want-time.Minute, got)
	}
	quotaSetCheckedAt(a, time.Now().Add(-(want + time.Minute)))
	if err := a.refresh(ctx, false); err != nil {
		t.Fatalf("距上次尝试 %s 时刷新 error = %v", want+time.Minute, err)
	}
	if got := up.quotaCount(token); got != 2 {
		t.Fatalf("距上次尝试 %s 时应请求上游，额度请求次数 = %d, want 2", want+time.Minute, got)
	}
}

// TestQuotaRefreshIntervalFromConfig Service 由 Config.QuotaRefreshInterval 决定节流窗口，非正值使用默认值。
func TestQuotaRefreshIntervalFromConfig(t *testing.T) {
	for _, tc := range quotaIntervalCases {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			service := testService(t, Config{
				Token: "oauth-alice", User: "alice", Client: up.client, QuotaRefreshInterval: tc.interval,
			})
			assertQuotaIntervalGovernsRefresh(t, newPoolAccount(service, "alice"), up, "oauth-alice", tc.want)
		})
	}
}

// TestQuotaRefreshIntervalFromPoolConfig 托管池的 PoolConfig.QuotaRefreshInterval 传递给池内账号，非正值使用默认值。
func TestQuotaRefreshIntervalFromPoolConfig(t *testing.T) {
	for _, tc := range quotaIntervalCases {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			p, err := NewManagedPool(PoolConfig{
				CacheDir: t.TempDir(), Client: up.client, QuotaRefreshInterval: tc.interval,
			}, []PoolAccount{priorityAccount("alice", 0)})
			if err != nil {
				t.Fatal(err)
			}
			assertQuotaIntervalGovernsRefresh(t, p.findAccount("alice"), up, "oauth-alice", tc.want)
		})
	}
}

// TestQuotaRefreshIntervalAppliesToManagementImport 管理接口新增的账号继承池的 QuotaRefreshInterval。
func TestQuotaRefreshIntervalAppliesToManagementImport(t *testing.T) {
	up := newMgmtUpstream(t)
	up.setLogin("oauth-carol", "Carol")
	config := mgmtPoolConfig(t, up, t.TempDir(), 0)
	config.QuotaRefreshInterval = 10 * time.Minute
	p, err := NewManagedPool(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)
	if w := mgmtCall(p, "POST", "/v0/management/accounts", `{"credential":{"oauth_token":"oauth-carol"}}`, mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("导入 status=%d body=%s", w.Code, w.Body.String())
	}
	carol := p.findAccount("carol")
	if carol == nil {
		t.Fatal("导入后账号列表缺少 carol")
	}
	if got := carol.service.quotaRefreshInterval; got != 10*time.Minute {
		t.Fatalf("导入账号的 quotaRefreshInterval = %s, want 10m", got)
	}
	// 导入校验请求 1 次（入池之前），入池后的立即刷新 1 次；等它落地后再取基准次数，否则计数会多 1。
	quotaWaitAttempt(t, carol, time.Time{})
	before := up.quotaCount("oauth-carol")
	quotaSetCheckedAt(carol, time.Now().Add(-9*time.Minute))
	mgmtAccounts(t, p)
	if got := up.quotaCount("oauth-carol"); got != before {
		t.Fatalf("距上次尝试 9 分钟时列表请求了上游: %d -> %d, want 不变", before, got)
	}
	quotaSetCheckedAt(carol, time.Now().Add(-11*time.Minute))
	mgmtAccounts(t, p)
	if got := up.quotaCount("oauth-carol"); got != before+1 {
		t.Fatalf("距上次尝试 11 分钟时列表额度请求次数 = %d, want %d", got, before+1)
	}
}

// TestRunQuotaRefreshRefreshesEveryAccountOnStartup 启动时立即对所有账号做一次刷新。
func TestRunQuotaRefreshRefreshesEveryAccountOnStartup(t *testing.T) {
	up := newMgmtUpstream(t)
	p := newPriorityPool(t, up.client, priorityAccount("alice", 0), priorityAccount("bob", 0))
	startQuotaRefresh(t, p)
	quotaWaitFor(t, "启动时未刷新 alice", func() bool { return up.quotaCount("oauth-alice") == 1 })
	quotaWaitFor(t, "启动时未刷新 bob", func() bool { return up.quotaCount("oauth-bob") == 1 })
}

// TestRunQuotaRefreshStartupHonoursRestoredCheckedAt 启动时的刷新同样受节流约束：从缓存恢复的 checked_at 仍在间隔内则不请求上游，超过则请求。
func TestRunQuotaRefreshStartupHonoursRestoredCheckedAt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		age         time.Duration // checked_at 距启动的时长
		wantRequest bool          // 启动刷新是否请求上游
	}{
		{name: "within-interval", age: DefaultQuotaRefreshInterval - time.Minute},
		{name: "beyond-interval", age: DefaultQuotaRefreshInterval + time.Minute, wantRequest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			cacheRoot := t.TempDir()
			// 先按账号的缓存目录写入 quota.json，再创建池，使账号创建时从缓存恢复 checked_at。
			seed, err := New(Config{Token: "oauth-alice", CacheDir: cacheRoot})
			if err != nil {
				t.Fatal(err)
			}
			checkedAt := time.Now().Add(-tc.age)
			restored := Quota{
				Subscription: &QuotaSubscription{Plan: "Copilot Student", TierName: "Copilot Student"},
				ObservedAt:   checkedAt,
			}
			persistWriteQuota(t, seed.cacheDir, &restored, checkedAt, "")
			p, err := NewManagedPool(PoolConfig{CacheDir: cacheRoot, Client: up.client}, []PoolAccount{priorityAccount("alice", 0)})
			if err != nil {
				t.Fatal(err)
			}
			startQuotaRefresh(t, p)
			if !tc.wantRequest {
				quotaStaysAt(t, up, "oauth-alice", 0)
				return
			}
			quotaWaitFor(t, "checked_at 已过期，启动时应刷新", func() bool { return up.quotaCount("oauth-alice") == 1 })
		})
	}
}

// TestRunQuotaRefreshIncludesDisabledAccounts 禁用只影响账号选择，后台刷新仍然覆盖禁用的账号。
func TestRunQuotaRefreshIncludesDisabledAccounts(t *testing.T) {
	up := newMgmtUpstream(t)
	p := newPriorityPool(t, up.client, PoolAccount{Token: "oauth-carol", User: "carol", Disabled: true})
	startQuotaRefresh(t, p)
	carol := p.findAccount("carol")
	quotaWaitFor(t, "禁用账号 carol 未被刷新", func() bool {
		q, _ := carol.quota.snapshot()
		return q != nil
	})
	if got := up.quotaCount("oauth-carol"); got != 1 {
		t.Fatalf("禁用账号的额度请求次数 = %d, want 1", got)
	}
}

// TestRunQuotaRefreshRefreshesStaleAccountEachTick 每一轮都会刷新已过期的账号，未过期的账号不再请求上游。
func TestRunQuotaRefreshRefreshesStaleAccountEachTick(t *testing.T) {
	setQuotaRefreshTick(t, 20*time.Millisecond)
	up := newMgmtUpstream(t)
	p := newPriorityPool(t, up.client, priorityAccount("alice", 0), priorityAccount("bob", 0))
	startQuotaRefresh(t, p)
	alice, bob := p.findAccount("alice"), p.findAccount("bob")
	quotaWaitFor(t, "启动时未刷新 alice", func() bool { return up.quotaCount("oauth-alice") == 1 })
	quotaWaitFor(t, "启动时未刷新 bob", func() bool { return up.quotaCount("oauth-bob") == 1 })
	quotaWaitAttempt(t, alice, time.Time{})
	quotaWaitAttempt(t, bob, time.Time{})

	// 只让 bob 的 checkedAt 过期，之后的两轮都应刷新 bob。每轮结束（checkedAt 更新）后才能再次设为过期。
	for want := 2; want <= 3; want++ {
		expired := time.Now().Add(-2 * time.Hour)
		quotaSetCheckedAt(bob, expired)
		quotaWaitFor(t, "已过期的 bob 未被后台刷新", func() bool { return up.quotaCount("oauth-bob") >= want })
		quotaWaitAttempt(t, bob, expired)
	}
	// 两轮之后 bob 的 checkedAt 都是新的；alice 始终未过期，请求次数应保持为 1。
	if got := up.quotaCount("oauth-alice"); got != 1 {
		t.Fatalf("未过期的 alice 被后台再次请求 %d 次, want 1", got)
	}
}

// TestRunQuotaRefreshReturnsAfterCancelWithoutFurtherRequests ctx 取消后 RunQuotaRefresh 尽快返回，之后不再请求上游。
func TestRunQuotaRefreshReturnsAfterCancelWithoutFurtherRequests(t *testing.T) {
	setQuotaRefreshTick(t, 20*time.Millisecond)
	up := newMgmtUpstream(t)
	p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
	run := startQuotaRefresh(t, p)
	alice := p.findAccount("alice")
	quotaWaitFor(t, "启动时未刷新 alice", func() bool { return up.quotaCount("oauth-alice") == 1 })
	quotaWaitAttempt(t, alice, time.Time{})

	run.cancel()
	run.waitReturned(t)
	// 返回之后即使账号已过期，也不应再有任何请求。
	quotaSetCheckedAt(alice, time.Now().Add(-2*time.Hour))
	quotaStaysAt(t, up, "oauth-alice", 1)
}

// TestRunQuotaRefreshCancelAbortsInFlightRefresh 上游阻塞时取消 ctx，进行中的刷新随之中止，RunQuotaRefresh 照常返回。
func TestRunQuotaRefreshCancelAbortsInFlightRefresh(t *testing.T) {
	setQuotaRefreshTick(t, 20*time.Millisecond)
	up := newMgmtUpstream(t)
	p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
	run := startQuotaRefresh(t, p)
	alice := p.findAccount("alice")
	quotaWaitFor(t, "启动时未刷新 alice", func() bool { return up.quotaCount("oauth-alice") == 1 })
	quotaWaitAttempt(t, alice, time.Time{})

	hold := up.holdQuota("oauth-alice")
	quotaSetCheckedAt(alice, time.Now().Add(-2*time.Hour))
	reviewAwait(t, hold.started, "后台刷新未到达上游")
	run.cancel()
	run.waitReturned(t)
}

// TestRunQuotaRefreshRoundIsBoundedByTimeout 上游一直不返回时，单轮在 quotaRefreshTimeout 后结束并记录超时错误，之后的轮次照常进行。
func TestRunQuotaRefreshRoundIsBoundedByTimeout(t *testing.T) {
	setQuotaRefreshTick(t, 20*time.Millisecond)
	setQuotaRefreshTimeout(t, 100*time.Millisecond)
	up := newMgmtUpstream(t)
	p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
	hold := up.holdQuota("oauth-alice")
	startQuotaRefresh(t, p)
	alice := p.findAccount("alice")
	reviewAwait(t, hold.started, "后台刷新未到达上游")
	quotaWaitFor(t, "超时后未记录 quota_error", func() bool {
		_, errText := alice.quota.snapshot()
		return errText == quotaTimeoutMessage
	})

	// 超时的一轮已结束（checkedAt 已更新）；把账号设为过期后，下一轮应再次请求上游。
	quotaSetCheckedAt(alice, time.Now().Add(-2*time.Hour))
	quotaWaitFor(t, "超时之后的轮次没有再次刷新", func() bool { return up.quotaCount("oauth-alice") >= 2 })
}

// TestRunQuotaRefreshPicksUpAccountsAddedWhileRunning 运行期间通过管理接口新增的账号，之后的轮次同样会被刷新。
func TestRunQuotaRefreshPicksUpAccountsAddedWhileRunning(t *testing.T) {
	setQuotaRefreshTick(t, 20*time.Millisecond)
	up := newMgmtUpstream(t)
	up.setLogin("oauth-carol", "Carol")
	p, _ := mgmtNewPool(t, up, 0, "alice")
	startQuotaRefresh(t, p)
	quotaWaitFor(t, "启动时未刷新 alice", func() bool { return up.quotaCount("oauth-alice") == 1 })

	if w := mgmtCall(p, "POST", "/v0/management/accounts", `{"credential":{"oauth_token":"oauth-carol"}}`, mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("运行中导入 status=%d body=%s", w.Code, w.Body.String())
	}
	// 导入校验不会写入账号的额度，因此只有后台轮次刷新过 carol 之后才会出现额度快照。
	carol := p.findAccount("carol")
	quotaWaitFor(t, "运行中新增的 carol 未被后台刷新", func() bool {
		q, _ := carol.quota.snapshot()
		return q != nil
	})
}

// TestRunQuotaRefreshUpdatesExhaustionForSelection 后台刷新得到的耗尽额度立即影响选择：耗尽的高优先级账号被跳过。
func TestRunQuotaRefreshUpdatesExhaustionForSelection(t *testing.T) {
	up := newMgmtUpstream(t)
	up.setQuotaBody("oauth-alice", quotaExhaustedBody)
	p := newPriorityPool(t, up.client, priorityAccount("alice", 10), priorityAccount("bob", 0))
	startQuotaRefresh(t, p)
	alice := p.findAccount("alice")
	quotaWaitFor(t, "alice 的额度未被后台刷新", func() bool {
		q, _ := alice.quota.snapshot()
		return q != nil
	})
	if got := userOf(pickLockedAt(p, time.Now(), nil)); got != "bob" {
		t.Fatalf("alice 耗尽后选中 %s, want bob", got)
	}
}

// TestErrorRefreshFetchesQuotaAfterUpstreamFailure 生成请求返回 500、402 或 403（非 429）时，账号额度被异步刷新一次，
// 结果写入额度缓存与 quota.json；客户端收到的仍是上游的原始状态。
func TestErrorRefreshFetchesQuotaAfterUpstreamFailure(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusPaymentRequired, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			up := newMgmtUpstream(t)
			up.pool.failGeneration("oauth-alice", poolFailure{status, `{"error":"upstream"}`})
			p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
			if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != status {
				t.Fatalf("生成请求 status=%d, want 上游原始状态 %d; body=%s", w.Code, status, w.Body.String())
			}
			alice := p.findAccount("alice")
			quotaWaitAttempt(t, alice, time.Time{})
			if got := up.quotaCount("oauth-alice"); got != 1 {
				t.Fatalf("失败后额度请求次数 = %d, want 1", got)
			}
			if raw := persistReadFile(t, persistQuotaPath(t, p, "alice"))["quota"]; len(raw) == 0 || string(raw) == "null" {
				t.Fatalf("异步刷新后 quota.json 缺少额度: %s", raw)
			}
		})
	}
}

// TestErrorRefreshIsBoundedByRefreshTimeout 异步刷新与列表刷新一样受 quotaRefreshTimeout 约束：上游一直不返回时，超时后记录 quota_error。
func TestErrorRefreshIsBoundedByRefreshTimeout(t *testing.T) {
	setQuotaRefreshTimeout(t, 100*time.Millisecond)
	up := newMgmtUpstream(t)
	up.pool.failGeneration("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"down"}`})
	hold := up.holdQuota("oauth-alice")
	p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
	alice := p.findAccount("alice")
	if w := quotaAwaitResponse(t, quotaGenerateAsync(p, context.Background())); w.Code != http.StatusInternalServerError {
		t.Fatalf("生成请求 status=%d, want 500", w.Code)
	}
	reviewAwait(t, hold.started, "错误后的额度刷新未到达上游")
	quotaWaitFor(t, "异步刷新超时后未记录 quota_error", func() bool {
		_, errText := alice.quota.snapshot()
		return errText == quotaTimeoutMessage
	})
}

// TestErrorRefreshSkipsRateLimitedGeneration 生成请求返回 429 时不刷新额度：限流由冷却处理，额度接口本身也可能限流。
func TestErrorRefreshSkipsRateLimitedGeneration(t *testing.T) {
	up := newMgmtUpstream(t)
	up.pool.failGeneration("oauth-alice", poolFailure{http.StatusTooManyRequests, `{"error":"limited"}`})
	p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusTooManyRequests {
		t.Fatalf("生成请求 status=%d, want 429 原样透传", w.Code)
	}
	quotaStaysAt(t, up, "oauth-alice", 0)
}

// TestErrorRefreshOnSessionFailure 会话获取失败（非 429）的账号会触发刷新，即使请求最终由其他账号处理；
// 只有一个账号时同一请求内的重复触发只刷新一次。
func TestErrorRefreshOnSessionFailure(t *testing.T) {
	t.Run("skipped-account-refreshed-while-other-serves", func(t *testing.T) {
		up := newMgmtUpstream(t)
		up.pool.failSession("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"down"}`})
		p := newPriorityPool(t, up.client, priorityAccount("alice", 10), priorityAccount("bob", 0))
		w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oauth-bob") {
			t.Fatalf("请求应由 bob 成功处理: status=%d body=%s", w.Code, w.Body.String())
		}
		quotaWaitAttempt(t, p.findAccount("alice"), time.Time{})
		if got := up.quotaCount("oauth-alice"); got != 1 {
			t.Fatalf("alice 的额度请求次数 = %d, want 1", got)
		}
		if got := up.quotaCount("oauth-bob"); got != 0 {
			t.Fatalf("成功处理请求的 bob 不应被刷新额度，实际请求 %d 次", got)
		}
	})
	t.Run("single-account-refreshed-once", func(t *testing.T) {
		up := newMgmtUpstream(t)
		up.pool.failSession("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"down"}`})
		p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
		if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusInternalServerError {
			t.Fatalf("全部 session 失败 status=%d, want 500", w.Code)
		}
		quotaWaitAttempt(t, p.findAccount("alice"), time.Time{})
		// 会话失败时 forward 与 track 都可能触发，但同一账号只应刷新一次。
		quotaStaysAt(t, up, "oauth-alice", 1)
	})
}

// TestErrorRefreshSkipsSessionRateLimit 会话获取返回 429 时不刷新额度，请求照常转给其他账号。
func TestErrorRefreshSkipsSessionRateLimit(t *testing.T) {
	up := newMgmtUpstream(t)
	up.pool.failSession("oauth-alice", poolFailure{http.StatusTooManyRequests, `{"message":"limited"}`})
	p := newPriorityPool(t, up.client, priorityAccount("alice", 10), priorityAccount("bob", 0))
	w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oauth-bob") {
		t.Fatalf("请求应由 bob 成功处理: status=%d body=%s", w.Code, w.Body.String())
	}
	quotaStaysAt(t, up, "oauth-alice", 0)
}

// TestErrorRefreshSkipsClientCancellation 客户端断开造成的失败不是账号的问题，不触发额度刷新。
func TestErrorRefreshSkipsClientCancellation(t *testing.T) {
	t.Run("cancelled-while-upstream-processes", func(t *testing.T) {
		up := newMgmtUpstream(t)
		hold := up.holdGenerations("oauth-alice")
		p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := quotaGenerateAsync(p, ctx)
		reviewAwait(t, hold.started, "生成请求未到达上游")
		cancel()
		quotaAwaitResponse(t, done)
		quotaStaysAt(t, up, "oauth-alice", 0)
	})
	t.Run("cancelled-before-session", func(t *testing.T) {
		up := newMgmtUpstream(t)
		p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		quotaAwaitResponse(t, quotaGenerateAsync(p, ctx))
		quotaStaysAt(t, up, "oauth-alice", 0)
	})
}

// TestErrorRefreshDoesNotBlockResponseOrDuplicateInFlight 刷新在上游阻塞时：请求照常返回，不等待刷新；
// 同一账号已有刷新进行中时，后续失败不再发起额度请求。
func TestErrorRefreshDoesNotBlockResponseOrDuplicateInFlight(t *testing.T) {
	up := newMgmtUpstream(t)
	up.pool.failGeneration("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"down"}`})
	hold := up.holdQuota("oauth-alice")
	p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
	alice := p.findAccount("alice")

	if w := quotaAwaitResponse(t, quotaGenerateAsync(p, context.Background())); w.Code != http.StatusInternalServerError {
		t.Fatalf("第一次生成请求 status=%d, want 500", w.Code)
	}
	reviewAwait(t, hold.started, "错误后的额度刷新未到达上游")
	if w := quotaAwaitResponse(t, quotaGenerateAsync(p, context.Background())); w.Code != http.StatusInternalServerError {
		t.Fatalf("刷新进行中的第二次生成请求 status=%d, want 500", w.Code)
	}
	quotaStaysAt(t, up, "oauth-alice", 1)

	hold.free()
	quotaWaitAttempt(t, alice, time.Time{})
}

// TestErrorRefreshThrottledByRefreshInterval 距上次尝试不足 quotaErrorRefreshInterval 时失败不触发刷新；超过后再次失败会触发。
func TestErrorRefreshThrottledByRefreshInterval(t *testing.T) {
	setQuotaErrorRefreshInterval(t, time.Hour)
	up := newMgmtUpstream(t)
	up.pool.failGeneration("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"down"}`})
	p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
	alice := p.findAccount("alice")
	fail := func(t *testing.T) {
		t.Helper()
		if w := quotaAwaitResponse(t, quotaGenerateAsync(p, context.Background())); w.Code != http.StatusInternalServerError {
			t.Fatalf("生成请求 status=%d, want 500", w.Code)
		}
	}

	fail(t)
	quotaWaitAttempt(t, alice, time.Time{})
	fail(t) // 距上次尝试不足 1 小时：不刷新。
	quotaStaysAt(t, up, "oauth-alice", 1)

	expired := time.Now().Add(-2 * time.Hour)
	quotaSetCheckedAt(alice, expired)
	fail(t) // 超过 1 小时：刷新。
	quotaWaitFor(t, "超过节流间隔后的失败未刷新额度", func() bool { return up.quotaCount("oauth-alice") == 2 })
	quotaWaitAttempt(t, alice, expired)
}

// TestErrorRefreshUsesContextIndependentOfRequest 刷新不随请求结束而取消：请求已返回且客户端 ctx 被取消后，
// 进行中的额度刷新仍能完成并写入结果。
func TestErrorRefreshUsesContextIndependentOfRequest(t *testing.T) {
	up := newMgmtUpstream(t)
	up.pool.failGeneration("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"down"}`})
	hold := up.holdQuota("oauth-alice")
	p := newPriorityPool(t, up.client, priorityAccount("alice", 0))
	alice := p.findAccount("alice")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if w := quotaAwaitResponse(t, quotaGenerateAsync(p, ctx)); w.Code != http.StatusInternalServerError {
		t.Fatalf("生成请求 status=%d, want 500", w.Code)
	}
	reviewAwait(t, hold.started, "错误后的额度刷新未到达上游")
	cancel() // 请求已经结束，客户端 ctx 随之取消；额度刷新不应受影响。
	hold.free()
	quotaWaitFor(t, "请求结束后额度刷新被取消，额度未写入", func() bool {
		q, _ := alice.quota.snapshot()
		return q != nil
	})
}

// TestErrorRefreshUpdatesQuotaUsedForSelection 异步刷新的结果立即影响选择：刷新得到耗尽额度后，该账号被跳过。
func TestErrorRefreshUpdatesQuotaUsedForSelection(t *testing.T) {
	up := newMgmtUpstream(t)
	up.pool.failGeneration("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"down"}`})
	up.setQuotaBody("oauth-alice", quotaExhaustedBody)
	p := newPriorityPool(t, up.client, priorityAccount("alice", 10), priorityAccount("bob", 0))
	alice := p.findAccount("alice")

	// 第一次请求由优先级更高的 alice 处理，失败后触发刷新。
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusInternalServerError {
		t.Fatalf("第一次生成请求 status=%d, want 500", w.Code)
	}
	quotaWaitAttempt(t, alice, time.Time{})

	// alice 仍然会失败，因此只有被跳过时请求才会成功，并由 bob 处理。
	w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oauth-bob") {
		t.Fatalf("alice 耗尽后请求应由 bob 处理: status=%d body=%s", w.Code, w.Body.String())
	}
}
