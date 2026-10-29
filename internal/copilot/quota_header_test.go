package copilot

// 本文件覆盖响应头额度快照的合并（VS Code 同款语义）与 Additional Budget 分母的留存：
//   - 头名：免费用户（缓存额度的 Subscription.Plan == "Copilot Free"）只认 x-quota-snapshot-chat；
//     付费或计划未知的账号优先 x-quota-snapshot-premium_models，缺失时回退 x-quota-snapshot-premium_interactions。
//   - 头值是 URL 编码键值对：rem（剩余百分比）、ov（overage_count）、rst（RFC3339 重置时间）等。
//   - 只合并进已有的缓存额度：缓存为空时 no-op；不改变 Stale 与 checkedAt，只刷新 ObservedAt。
//   - Additional Budget 的分母（overage_entitlement）由 parseQuota 记在 additional_budget 桶的导出字段 Entitlement 上
//     （json:"entitlement,omitempty"），供合并时重算 RemainingFraction；它随 quota.json 落盘，重启后 ov 头仍能更新 Additional Budget。
//     其他桶（主额度）不带该键。
//
// 本文件引用的下列标识符由实现方提供（如改名需同步修改测试）：
//   - func (a *poolAccount) applyQuotaHeaders(header http.Header)
//   - func parseQuotaHeader(value string) quotaHeaderSnapshot（忽略 url.ParseQuery 的错误，使用其返回的部分结果），其中
//     quotaHeaderSnapshot{remaining *float64（rem 百分比原值）, overage *float64（ov）, reset time.Time（rst，零值表示缺失或非法）,
//     unlimited bool（ent 解析为整数 -1 时为 true；缺失、非法或其他值为 false）}
//   - unlimited 为 true（组织共享额度的不限量快照）时，applyQuotaHeaders 不合并 rem（保留 parseQuota 造的 0% 合成主窗口），rst、ov 照常合并。
//   - QuotaBucket.Entitlement（float64，json:"entitlement,omitempty"）；不再有未导出的 entitlement / used 字段

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"
)

const (
	headerChat          = "x-quota-snapshot-chat"
	headerPremiumModels = "x-quota-snapshot-premium_models"
	headerPremiumInter  = "x-quota-snapshot-premium_interactions"

	// headerOldReset 是缓存额度中窗口的初始重置时间。
	headerOldReset = "2026-10-20T00:00:00Z"
)

// headerTestQuota 构造指定计划的缓存额度：主额度窗口剩余 0.8，Additional Budget 窗口剩余 0.75（分母 100，已用 25）。
// plan 为空表示没有订阅信息；主额度窗口名按计划取 chat（Copilot Free）或 premium_requests。
func headerTestQuota(plan string) *Quota {
	mainWindow := "premium_requests"
	if plan == "Copilot Free" {
		mainWindow = "chat"
	}
	q := &Quota{
		Groups: []QuotaGroup{{DisplayName: "GitHub Copilot", Buckets: []QuotaBucket{
			{Window: mainWindow, Description: "main", RemainingFraction: 0.8, ResetTime: headerOldReset},
			{Window: "additional_budget", Description: "Additional Budget", RemainingFraction: 0.75, ResetTime: headerOldReset,
				Entitlement: 100},
		}}},
		ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	if plan != "" {
		q.Subscription = &QuotaSubscription{Plan: plan, TierName: plan}
	}
	return q
}

// headerTestAccount 创建只含缓存额度的账号（不访问上游）；quota 为 nil 表示缓存没有数据。
func headerTestAccount(t *testing.T, quota *Quota) *poolAccount {
	t.Helper()
	service := testService(t, Config{APIKey: testAPIKey, Token: "oauth-alice", User: "alice"})
	account := newPoolAccount(service, "alice")
	account.quota.mu.Lock()
	account.quota.data = quota
	account.quota.mu.Unlock()
	return account
}

// headerTestHeader 用 name:value 成对参数构造响应头。
func headerTestHeader(pairs ...string) http.Header {
	header := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		header.Set(pairs[i], pairs[i+1])
	}
	return header
}

// headerTestSameInstant 报告两个重置时间文本是否都是 RFC3339 且表示同一时刻。
func headerTestSameInstant(t *testing.T, got, want string) bool {
	t.Helper()
	g, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Errorf("重置时间 %q 不是 RFC3339: %v", got, err)
		return false
	}
	w, err := time.Parse(time.RFC3339, want)
	if err != nil {
		t.Fatalf("测试数据 %q 不是 RFC3339: %v", want, err)
	}
	return g.Equal(w)
}

// TestParseQuotaHeaderFields 纯解析函数：字段级非法值只丢弃该字段；ent 仅在解析为整数 -1 时标记 unlimited。
func TestParseQuotaHeaderFields(t *testing.T) {
	wantReset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ptr := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name      string
		value     string
		remaining *float64
		overage   *float64
		reset     time.Time // 零值表示缺失或非法
		unlimited bool
	}{
		{name: "full", value: "ent=300&rem=42.5&ov=3&ovPerm=true&rst=2026-11-01T00%3A00%3A00Z",
			remaining: ptr(42.5), overage: ptr(3), reset: wantReset},
		{name: "extra-keys-ignored", value: "foo=bar&rem=10", remaining: ptr(10)},
		{name: "rem-zero-is-valid", value: "rem=0", remaining: ptr(0)},
		{name: "rem-not-number", value: "rem=abc&ov=2&rst=2026-11-01T00%3A00%3A00Z", overage: ptr(2), reset: wantReset},
		{name: "rem-negative", value: "rem=-5&ov=2", overage: ptr(2)},
		{name: "rem-nan", value: "rem=NaN&ov=2", overage: ptr(2)},
		{name: "rem-inf", value: "rem=Inf&ov=2", overage: ptr(2)},
		{name: "rem-empty", value: "rem=&ov=2", overage: ptr(2)},
		{name: "ov-negative", value: "rem=10&ov=-1", remaining: ptr(10)},
		{name: "ov-not-number", value: "rem=10&ov=abc", remaining: ptr(10)},
		{name: "rst-invalid", value: "rem=10&rst=tomorrow", remaining: ptr(10)},
		{name: "rst-date-only-is-not-rfc3339", value: "rem=10&rst=2026-11-01", remaining: ptr(10)},
		{name: "rst-with-offset", value: "rst=2026-11-01T08%3A00%3A00%2B08%3A00", reset: wantReset},
		{name: "no-known-fields", value: "ent=100&ovPerm=false"},
		// ent=-1 表示不限量（VS Code：entitlement === -1）；其他值、缺失、非法都不是。
		{name: "ent-unlimited", value: "ent=-1&rem=100", remaining: ptr(100), unlimited: true},
		{name: "ent-unlimited-only", value: "ent=-1", unlimited: true},
		{name: "ent-300-not-unlimited", value: "ent=300&rem=100", remaining: ptr(100)},
		{name: "ent-zero-not-unlimited", value: "ent=0&rem=100", remaining: ptr(100)},
		{name: "ent-other-negative-not-unlimited", value: "ent=-2&rem=100", remaining: ptr(100)},
		{name: "ent-not-number-not-unlimited", value: "ent=abc&rem=100", remaining: ptr(100)},
		{name: "ent-float-not-unlimited", value: "ent=-1.0&rem=100", remaining: ptr(100)},
		{name: "ent-empty-not-unlimited", value: "ent=&rem=100", remaining: ptr(100)},
		{name: "ent-missing-not-unlimited", value: "rem=100", remaining: ptr(100)},
		// 坏键值对不影响其余合法键值对（url.ParseQuery 的错误被忽略）。
		{name: "bad-pair-others-kept", value: "rem=10&ov=%g1", remaining: ptr(10)},
		{name: "bad-pair-first-others-kept", value: "ov=%g1&rem=10&ent=-1&rst=2026-11-01T00%3A00%3A00Z",
			remaining: ptr(10), reset: wantReset, unlimited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseQuotaHeader(tc.value)
			if got.unlimited != tc.unlimited {
				t.Errorf("unlimited = %v, want %v", got.unlimited, tc.unlimited)
			}
			if !reflect.DeepEqual(got.remaining, tc.remaining) {
				t.Errorf("remaining = %v, want %v", derefOrNil(got.remaining), derefOrNil(tc.remaining))
			}
			if !reflect.DeepEqual(got.overage, tc.overage) {
				t.Errorf("overage = %v, want %v", derefOrNil(got.overage), derefOrNil(tc.overage))
			}
			if !got.reset.Equal(tc.reset) {
				t.Errorf("reset = %s, want %s", got.reset, tc.reset)
			}
		})
	}
}

// derefOrNil 供失败信息打印：指针为 nil 时返回 nil，否则返回其指向的值。
func derefOrNil(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestParseQuotaHeaderMalformedPartial 头值含非法键值对时不 panic：非法的键值对被丢弃，其余合法字段保留；
// 没有任何合法字段时所有字段为空（零值）。
func TestParseQuotaHeaderMalformedPartial(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	wantReset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		value     string
		remaining *float64
		overage   *float64
		reset     time.Time
		unlimited bool
	}{
		{name: "all-bad-1", value: "%zz"},
		{name: "all-bad-2", value: "rem=%"},
		{name: "all-bad-3", value: "a=%1"},
		{name: "empty", value: ""},
		{name: "bad-ov-keeps-rem", value: "rem=10&ov=%g1", remaining: ptr(10)},
		{name: "bad-rem-keeps-ov", value: "rem=%&ov=1", overage: ptr(1)},
		{name: "bad-key-keeps-rest", value: "%zz=1&rem=20&ov=3&ent=-1", remaining: ptr(20), overage: ptr(3), unlimited: true},
		{name: "bad-last-keeps-rest", value: "rem=20&rst=2026-11-01T00%3A00%3A00Z&x=%", remaining: ptr(20), reset: wantReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseQuotaHeader(tc.value)
			if !reflect.DeepEqual(got.remaining, tc.remaining) {
				t.Errorf("remaining = %v, want %v", derefOrNil(got.remaining), derefOrNil(tc.remaining))
			}
			if !reflect.DeepEqual(got.overage, tc.overage) {
				t.Errorf("overage = %v, want %v", derefOrNil(got.overage), derefOrNil(tc.overage))
			}
			if !got.reset.Equal(tc.reset) {
				t.Errorf("reset = %s, want %s", got.reset, tc.reset)
			}
			if got.unlimited != tc.unlimited {
				t.Errorf("unlimited = %v, want %v", got.unlimited, tc.unlimited)
			}
		})
	}
}

// TestApplyQuotaHeadersMergesFields 表驱动覆盖 rem、rst、ov 三类字段对主额度窗口与 Additional Budget 的合并规则（付费账号，premium_models 头）。
func TestApplyQuotaHeadersMergesFields(t *testing.T) {
	const newReset = "2026-11-01T00:00:00Z"
	for _, tc := range []struct {
		name         string
		value        string
		wantMain     float64
		wantReset    string // 主额度窗口的重置时间
		wantBudget   float64
		wantNoChange bool // 为 true 时整个额度（含 ObservedAt）应与初始一致
	}{
		{name: "rem-updates-fraction", value: "rem=42.5", wantMain: 0.425, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "rem-100", value: "rem=100", wantMain: 1, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "rem-zero", value: "rem=0", wantMain: 0, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "rem-above-100-clamped", value: "rem=150", wantMain: 1, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "rem-negative-skipped", value: "rem=-5", wantMain: 0.8, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "rem-nan-skipped", value: "rem=NaN", wantMain: 0.8, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "rem-inf-skipped", value: "rem=Inf", wantMain: 0.8, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "rem-missing-keeps-fraction", value: "rst=2026-11-01T00%3A00%3A00Z", wantMain: 0.8, wantReset: newReset, wantBudget: 0.75},
		{name: "rst-updates-reset", value: "rem=50&rst=2026-11-01T00%3A00%3A00Z", wantMain: 0.5, wantReset: newReset, wantBudget: 0.75},
		{name: "rst-with-offset-same-instant", value: "rst=2026-11-01T08%3A00%3A00%2B08%3A00&rem=50", wantMain: 0.5, wantReset: newReset, wantBudget: 0.75},
		{name: "rst-missing-keeps-reset", value: "rem=50", wantMain: 0.5, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "rst-invalid-keeps-reset", value: "rem=50&rst=tomorrow", wantMain: 0.5, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "ov-recomputes-budget", value: "ov=50", wantMain: 0.8, wantReset: headerOldReset, wantBudget: 0.5},
		{name: "ov-zero-full-budget", value: "ov=0", wantMain: 0.8, wantReset: headerOldReset, wantBudget: 1},
		{name: "ov-equals-entitlement", value: "ov=100", wantMain: 0.8, wantReset: headerOldReset, wantBudget: 0},
		{name: "ov-above-entitlement-floored-at-zero", value: "ov=250", wantMain: 0.8, wantReset: headerOldReset, wantBudget: 0},
		{name: "ov-negative-skipped", value: "rem=50&ov=-1", wantMain: 0.5, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "ov-not-number-skipped", value: "rem=50&ov=abc", wantMain: 0.5, wantReset: headerOldReset, wantBudget: 0.75},
		{name: "all-fields", value: "ent=300&rem=20&ov=25&ovPerm=true&rst=2026-11-01T00%3A00%3A00Z",
			wantMain: 0.2, wantReset: newReset, wantBudget: 0.75},
		// 字段级非法值只跳过该字段，其他合法字段照常合并。
		{name: "bad-rem-others-merge", value: "rem=abc&ov=50&rst=2026-11-01T00%3A00%3A00Z", wantMain: 0.8, wantReset: newReset, wantBudget: 0.5},
		{name: "bad-ov-others-merge", value: "rem=30&ov=abc&rst=2026-11-01T00%3A00%3A00Z", wantMain: 0.3, wantReset: newReset, wantBudget: 0.75},
		{name: "bad-rst-others-merge", value: "rem=30&ov=50&rst=nope", wantMain: 0.3, wantReset: headerOldReset, wantBudget: 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := headerTestAccount(t, headerTestQuota("Copilot Pro"))
			account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, tc.value))

			got, _ := account.quota.snapshot()
			main, budget := quotaBuckets(got)
			if main == nil || budget == nil {
				t.Fatalf("合并后缺少窗口: %+v", got)
			}
			if !quotaTestApprox(main.RemainingFraction, tc.wantMain) {
				t.Errorf("主额度比例 = %v, want %v", main.RemainingFraction, tc.wantMain)
			}
			if !headerTestSameInstant(t, main.ResetTime, tc.wantReset) {
				t.Errorf("主额度重置时间 = %q, want %q", main.ResetTime, tc.wantReset)
			}
			if !quotaTestApprox(budget.RemainingFraction, tc.wantBudget) {
				t.Errorf("Additional Budget 比例 = %v, want %v", budget.RemainingFraction, tc.wantBudget)
			}
			// 主额度的 Additional Budget 重置时间不受 rst 影响。
			if budget.ResetTime != headerOldReset {
				t.Errorf("Additional Budget 重置时间 = %q, want 保持 %q", budget.ResetTime, headerOldReset)
			}
		})
	}
}

// TestApplyQuotaHeadersRepeatedMergeKeepsDenominator 合并不丢失 Additional Budget 的分母：连续两次 ov 都按同一分母重算。
func TestApplyQuotaHeadersRepeatedMergeKeepsDenominator(t *testing.T) {
	account := headerTestAccount(t, headerTestQuota("Copilot Pro"))
	account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "rem=10&ov=50"))
	account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "ov=75"))
	got, _ := account.quota.snapshot()
	_, budget := quotaBuckets(got)
	if budget == nil || !quotaTestApprox(budget.RemainingFraction, 0.25) {
		t.Fatalf("两次合并后的 Additional Budget = %+v, want 比例 0.25", budget)
	}
}

// TestApplyQuotaHeadersBudgetWithoutDenominator Additional Budget 缺少分母（Entitlement 为 0，例如老版本 quota.json 没有该键）或根本没有该窗口时，ov 不起作用，也不会新建窗口。
func TestApplyQuotaHeadersBudgetWithoutDenominator(t *testing.T) {
	t.Run("denominator-zero", func(t *testing.T) {
		quota := headerTestQuota("Copilot Pro")
		quota.Groups[0].Buckets[1].Entitlement = 0
		account := headerTestAccount(t, quota)
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "rem=50&ov=50"))
		got, _ := account.quota.snapshot()
		main, budget := quotaBuckets(got)
		if !quotaTestApprox(main.RemainingFraction, 0.5) {
			t.Errorf("主额度比例 = %v, want 0.5（rem 仍应合并）", main.RemainingFraction)
		}
		if !quotaTestApprox(budget.RemainingFraction, 0.75) {
			t.Errorf("无分母时 Additional Budget 比例 = %v, want 保持 0.75", budget.RemainingFraction)
		}
	})
	t.Run("no-budget-bucket", func(t *testing.T) {
		quota := headerTestQuota("Copilot Pro")
		quota.Groups[0].Buckets = quota.Groups[0].Buckets[:1]
		account := headerTestAccount(t, quota)
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "rem=50&ov=50"))
		got, _ := account.quota.snapshot()
		if quotaTestHasBucket(*got, "additional_budget") {
			t.Fatalf("不应新建 Additional Budget 窗口: %+v", got.Groups)
		}
		if main, _ := quotaBuckets(got); !quotaTestApprox(main.RemainingFraction, 0.5) {
			t.Fatalf("主额度比例 = %v, want 0.5", main.RemainingFraction)
		}
	})
	t.Run("no-main-bucket", func(t *testing.T) {
		// 只有 Additional Budget 时，rem/rst 没有可合并的主额度窗口，不应 panic；ov 仍可重算 Additional Budget。
		quota := headerTestQuota("Copilot Pro")
		quota.Groups[0].Buckets = quota.Groups[0].Buckets[1:]
		account := headerTestAccount(t, quota)
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "rem=50&ov=50&rst=2026-11-01T00%3A00%3A00Z"))
		got, _ := account.quota.snapshot()
		main, budget := quotaBuckets(got)
		if main != nil {
			t.Fatalf("不应新建主额度窗口: %+v", *main)
		}
		if budget == nil || !quotaTestApprox(budget.RemainingFraction, 0.5) {
			t.Fatalf("Additional Budget = %+v, want 比例 0.5", budget)
		}
	})
}

// TestApplyQuotaHeadersEmptyCacheIsNoop 缓存没有额度数据时不创建新额度，也不改变 checkedAt。
func TestApplyQuotaHeadersEmptyCacheIsNoop(t *testing.T) {
	account := headerTestAccount(t, nil)
	checked := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	quotaSetCheckedAt(account, checked)
	account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "rem=50&ov=1&rst=2026-11-01T00%3A00%3A00Z"))
	if account.quota.hasData() {
		t.Fatal("缓存为空时合并不应创建额度")
	}
	if got := quotaCheckedAt(account); !got.Equal(checked) {
		t.Fatalf("checkedAt = %s, want 不变 %s", got, checked)
	}
}

// TestApplyQuotaHeadersSelectsHeaderByPlan 表驱动覆盖按计划选择头名：
// 免费用户只认 chat；付费与计划未知的账号优先 premium_models、回退 premium_interactions，且都忽略 chat 头。
func TestApplyQuotaHeadersSelectsHeaderByPlan(t *testing.T) {
	// 各头使用不同的 rem，以区分生效的是哪一个。
	for _, tc := range []struct {
		name   string
		plan   string
		header http.Header
		want   float64 // 主额度期望的比例；0.8 表示 no-op
	}{
		{name: "free-chat", plan: "Copilot Free", header: headerTestHeader(headerChat, "rem=10"), want: 0.1},
		{name: "free-ignores-premium-models", plan: "Copilot Free", header: headerTestHeader(headerPremiumModels, "rem=20"), want: 0.8},
		{name: "free-ignores-premium-interactions", plan: "Copilot Free", header: headerTestHeader(headerPremiumInter, "rem=30"), want: 0.8},
		{name: "free-chat-wins-over-premium", plan: "Copilot Free",
			header: headerTestHeader(headerChat, "rem=10", headerPremiumModels, "rem=20", headerPremiumInter, "rem=30"), want: 0.1},
		{name: "paid-premium-models", plan: "Copilot Pro", header: headerTestHeader(headerPremiumModels, "rem=20"), want: 0.2},
		{name: "paid-premium-interactions-fallback", plan: "Copilot Pro", header: headerTestHeader(headerPremiumInter, "rem=30"), want: 0.3},
		{name: "paid-premium-models-wins", plan: "Copilot Pro",
			header: headerTestHeader(headerPremiumModels, "rem=20", headerPremiumInter, "rem=30"), want: 0.2},
		{name: "paid-ignores-chat", plan: "Copilot Pro", header: headerTestHeader(headerChat, "rem=10"), want: 0.8},
		{name: "paid-chat-and-premium", plan: "Copilot Pro",
			header: headerTestHeader(headerChat, "rem=10", headerPremiumInter, "rem=30"), want: 0.3},
		{name: "unknown-plan-premium-models", plan: "", header: headerTestHeader(headerPremiumModels, "rem=20"), want: 0.2},
		{name: "unknown-plan-premium-interactions", plan: "", header: headerTestHeader(headerPremiumInter, "rem=30"), want: 0.3},
		{name: "unknown-plan-ignores-chat", plan: "", header: headerTestHeader(headerChat, "rem=10"), want: 0.8},
		{name: "no-quota-headers", plan: "Copilot Pro", header: headerTestHeader("x-other", "rem=10"), want: 0.8},
		{name: "nil-header", plan: "Copilot Pro", header: nil, want: 0.8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := headerTestAccount(t, headerTestQuota(tc.plan))
			account.applyQuotaHeaders(tc.header)
			got, _ := account.quota.snapshot()
			main, _ := quotaBuckets(got)
			if main == nil || !quotaTestApprox(main.RemainingFraction, tc.want) {
				t.Fatalf("主额度 = %+v, want 比例 %v", main, tc.want)
			}
		})
	}
}

// TestApplyQuotaHeadersMalformedValueIsNoop 头值没有任何可合并字段（全部非法或为空）时 no-op，不 panic；额度（含 ObservedAt）保持原样。
func TestApplyQuotaHeadersMalformedValueIsNoop(t *testing.T) {
	for _, value := range []string{"%zz", "rem=%", "rem=%&ent=-1", ""} {
		t.Run(value, func(t *testing.T) {
			initial := headerTestQuota("Copilot Pro")
			account := headerTestAccount(t, headerTestQuota("Copilot Pro"))
			account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, value))
			got, _ := account.quota.snapshot()
			if !reflect.DeepEqual(got, initial) {
				t.Fatalf("无可合并字段的头值应为 no-op:\n got  %+v\n want %+v", got, initial)
			}
		})
	}
}

// TestApplyQuotaHeadersMalformedPairKeepsValidFields 头值中的坏键值对只被丢弃，其余合法字段照常合并。
func TestApplyQuotaHeadersMalformedPairKeepsValidFields(t *testing.T) {
	t.Run("bad-ov-rem-merges", func(t *testing.T) {
		account := headerTestAccount(t, headerTestQuota("Copilot Pro"))
		before := time.Now()
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "rem=10&ov=%g1"))
		got, _ := account.quota.snapshot()
		main, budget := quotaBuckets(got)
		if !quotaTestApprox(main.RemainingFraction, 0.1) {
			t.Errorf("主额度比例 = %v, want 0.1", main.RemainingFraction)
		}
		if !quotaTestApprox(budget.RemainingFraction, 0.75) {
			t.Errorf("Additional Budget 比例 = %v, want 保持 0.75（ov 非法）", budget.RemainingFraction)
		}
		if got.ObservedAt.Before(before) {
			t.Errorf("ObservedAt = %s, want 已更新", got.ObservedAt)
		}
	})
	t.Run("bad-rem-ov-merges", func(t *testing.T) {
		account := headerTestAccount(t, headerTestQuota("Copilot Pro"))
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "rem=%&ov=50"))
		got, _ := account.quota.snapshot()
		main, budget := quotaBuckets(got)
		if !quotaTestApprox(main.RemainingFraction, 0.8) {
			t.Errorf("主额度比例 = %v, want 保持 0.8（rem 非法）", main.RemainingFraction)
		}
		if !quotaTestApprox(budget.RemainingFraction, 0.5) {
			t.Errorf("Additional Budget 比例 = %v, want 0.5", budget.RemainingFraction)
		}
	})
}

// TestApplyQuotaHeadersMalformedPremiumModelsDoesNotFallback premium_models 头存在但值无可用字段时，
// 仍选定该头，不回退 premium_interactions（头选择只看 header.Values 判断的存在性，与解析结果无关）。
func TestApplyQuotaHeadersMalformedPremiumModelsDoesNotFallback(t *testing.T) {
	initial := headerTestQuota("Copilot Pro")
	account := headerTestAccount(t, headerTestQuota("Copilot Pro"))
	account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "%zz", headerPremiumInter, "rem=30"))
	got, _ := account.quota.snapshot()
	if !reflect.DeepEqual(got, initial) {
		t.Fatalf("premium_models 非法时不应回退 premium_interactions:\n got  %+v\n want %+v", got, initial)
	}
}

// TestApplyQuotaHeadersEmptyPremiumModelsDoesNotFallback premium_models 头存在但值为空串时仍选定该头、不回退 premium_interactions：
// 头选择按“是否存在”而不是“值是否非空”；空值没有可合并字段，整个额度（含 ObservedAt）保持不变。
// 头真正缺失时仍回退 premium_interactions（见 TestApplyQuotaHeadersSelectsHeaderByPlan）。
func TestApplyQuotaHeadersEmptyPremiumModelsDoesNotFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		plan   string
		header http.Header
	}{
		{name: "paid-empty-models-with-interactions", plan: "Copilot Pro",
			header: headerTestHeader(headerPremiumModels, "", headerPremiumInter, "rem=30&ov=50")},
		{name: "unknown-plan-empty-models-with-interactions", plan: "",
			header: headerTestHeader(headerPremiumModels, "", headerPremiumInter, "rem=30&ov=50")},
		{name: "paid-empty-models-only", plan: "Copilot Pro", header: headerTestHeader(headerPremiumModels, "")},
		// 不经 Set 直接以规范化键写入，确认空串值的头确实“存在”（Values 长度为 1）。
		{name: "canonical-key-empty-value", plan: "Copilot Pro",
			header: http.Header{http.CanonicalHeaderKey(headerPremiumModels): {""}, http.CanonicalHeaderKey(headerPremiumInter): {"rem=30"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.header.Values(headerPremiumModels); len(got) != 1 || got[0] != "" {
				t.Fatalf("前提不成立：premium_models 头应存在且为空串，Values = %q", got)
			}
			initial := headerTestQuota(tc.plan)
			account := headerTestAccount(t, headerTestQuota(tc.plan))
			account.applyQuotaHeaders(tc.header)
			got, _ := account.quota.snapshot()
			if !reflect.DeepEqual(got, initial) {
				t.Fatalf("premium_models 为空串时应 no-op，不回退 premium_interactions:\n got  %+v\n want %+v", got, initial)
			}
		})
	}
}

// TestApplyQuotaHeadersUnlimitedKeepsSyntheticZero 不限量快照（ent=-1）不合并 rem：
// 组织共享额度耗尽时 parseQuota 造的 0% 合成主窗口不能被头里的 rem=100 覆盖；rst、ov 照常合并。
func TestApplyQuotaHeadersUnlimitedKeepsSyntheticZero(t *testing.T) {
	const newReset = "2026-11-01T00:00:00Z"
	// zeroMainQuota 模拟组织共享额度耗尽：主窗口 RemainingFraction = 0。
	zeroMainQuota := func() *Quota {
		quota := headerTestQuota("Copilot Pro")
		quota.Groups[0].Buckets[0].RemainingFraction = 0
		return quota
	}

	t.Run("rem-ignored-rst-merged", func(t *testing.T) {
		account := headerTestAccount(t, zeroMainQuota())
		before := time.Now()
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "ent=-1&rem=100&rst=2026-11-01T00%3A00%3A00Z"))
		got, _ := account.quota.snapshot()
		main, budget := quotaBuckets(got)
		if main.RemainingFraction != 0 {
			t.Errorf("主额度比例 = %v, want 保持 0（unlimited 不合并 rem）", main.RemainingFraction)
		}
		if !headerTestSameInstant(t, main.ResetTime, newReset) {
			t.Errorf("主额度重置时间 = %q, want %q", main.ResetTime, newReset)
		}
		if !quotaTestApprox(budget.RemainingFraction, 0.75) {
			t.Errorf("Additional Budget 比例 = %v, want 保持 0.75", budget.RemainingFraction)
		}
		if got.ObservedAt.Before(before) {
			t.Errorf("ObservedAt = %s, want 已更新", got.ObservedAt)
		}
	})
	t.Run("only-rem-is-noop", func(t *testing.T) {
		initial := zeroMainQuota()
		account := headerTestAccount(t, zeroMainQuota())
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "ent=-1&rem=100"))
		got, _ := account.quota.snapshot()
		if !reflect.DeepEqual(got, initial) {
			t.Fatalf("unlimited 且只有 rem 时应完全 no-op（含 ObservedAt）:\n got  %+v\n want %+v", got, initial)
		}
	})
	t.Run("ov-still-merged", func(t *testing.T) {
		account := headerTestAccount(t, zeroMainQuota())
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "ent=-1&rem=100&ov=50"))
		got, _ := account.quota.snapshot()
		main, budget := quotaBuckets(got)
		if main.RemainingFraction != 0 {
			t.Errorf("主额度比例 = %v, want 保持 0", main.RemainingFraction)
		}
		if !quotaTestApprox(budget.RemainingFraction, 0.5) {
			t.Errorf("Additional Budget 比例 = %v, want 0.5（ov 照常合并）", budget.RemainingFraction)
		}
	})
	t.Run("ent-300-merges-rem", func(t *testing.T) {
		account := headerTestAccount(t, zeroMainQuota())
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "ent=300&rem=100"))
		got, _ := account.quota.snapshot()
		main, _ := quotaBuckets(got)
		if !quotaTestApprox(main.RemainingFraction, 1) {
			t.Fatalf("主额度比例 = %v, want 1（非 unlimited 正常合并 rem）", main.RemainingFraction)
		}
	})
	t.Run("ent-invalid-merges-rem", func(t *testing.T) {
		account := headerTestAccount(t, zeroMainQuota())
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "ent=abc&rem=100"))
		got, _ := account.quota.snapshot()
		main, _ := quotaBuckets(got)
		if !quotaTestApprox(main.RemainingFraction, 1) {
			t.Fatalf("主额度比例 = %v, want 1（ent 非法不算 unlimited）", main.RemainingFraction)
		}
	})
	t.Run("stays-exhausted", func(t *testing.T) {
		// 集成 weight.go：unlimited 头不会让耗尽的账号复活（主窗口 0% 且 Additional Budget 为 0 时按耗尽跳过）。
		now := time.Now()
		reset := now.Add(time.Hour).UTC().Format(time.RFC3339)
		quota := zeroMainQuota()
		quota.Groups[0].Buckets[0].ResetTime = reset
		quota.Groups[0].Buckets[1].RemainingFraction = 0
		account := headerTestAccount(t, quota)
		if !account.exhausted(now) {
			t.Fatal("合并前账号应已耗尽")
		}
		account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "ent=-1&rem=100"))
		if !account.exhausted(now) {
			t.Fatal("unlimited 头带 rem=100 不应使耗尽的账号复活")
		}
	})
}

// TestApplyQuotaHeadersObservedAtButNotCheckedAtOrStale 合并成功后 ObservedAt 更新为当前时间；Stale 与 checkedAt 不变（不影响轮询节流）。
// 头缺失时 ObservedAt 也不变。
func TestApplyQuotaHeadersObservedAtButNotCheckedAtOrStale(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "stale-false"
		if stale {
			name = "stale-true"
		}
		t.Run(name, func(t *testing.T) {
			quota := headerTestQuota("Copilot Pro")
			quota.Stale = stale
			oldObserved := quota.ObservedAt
			account := headerTestAccount(t, quota)
			checked := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
			quotaSetCheckedAt(account, checked)

			// 头缺失：整个额度不变。
			account.applyQuotaHeaders(http.Header{})
			if got, _ := account.quota.snapshot(); !got.ObservedAt.Equal(oldObserved) {
				t.Fatalf("头缺失时 ObservedAt = %s, want 不变 %s", got.ObservedAt, oldObserved)
			}

			before := time.Now()
			account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "rem=42"))
			after := time.Now()

			got, _ := account.quota.snapshot()
			if got.ObservedAt.Before(before) || got.ObservedAt.After(after) {
				t.Fatalf("ObservedAt = %s, want 位于 [%s, %s]", got.ObservedAt, before, after)
			}
			if got.Stale != stale {
				t.Fatalf("Stale = %v, want 不变 %v", got.Stale, stale)
			}
			if gotChecked := quotaCheckedAt(account); !gotChecked.Equal(checked) {
				t.Fatalf("checkedAt = %s, want 不变 %s", gotChecked, checked)
			}
		})
	}
}

// TestApplyQuotaHeadersDoesNotMutateEarlierSnapshot 合并不得原地修改此前 snapshot() 返回的副本所共享的窗口
// （管理接口可能正在序列化它）：先取出的快照在合并后仍是旧值，新快照才是合并结果。
func TestApplyQuotaHeadersDoesNotMutateEarlierSnapshot(t *testing.T) {
	account := headerTestAccount(t, headerTestQuota("Copilot Pro"))
	earlier, _ := account.quota.snapshot()
	account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "rem=10&ov=100&rst=2026-11-01T00%3A00%3A00Z"))

	main, budget := quotaBuckets(earlier)
	if !quotaTestApprox(main.RemainingFraction, 0.8) || main.ResetTime != headerOldReset || !quotaTestApprox(budget.RemainingFraction, 0.75) {
		t.Fatalf("合并改动了先前取出的快照: main=%+v budget=%+v", *main, *budget)
	}
	now, _ := account.quota.snapshot()
	mainNow, budgetNow := quotaBuckets(now)
	if !quotaTestApprox(mainNow.RemainingFraction, 0.1) || !quotaTestApprox(budgetNow.RemainingFraction, 0) {
		t.Fatalf("合并后的快照 main=%+v budget=%+v", *mainNow, *budgetNow)
	}
}

// TestApplyQuotaHeadersMakesAccountExhausted 集成 weight.go：合并 rem=0 且 Additional Budget 重算为 0 后，账号判定为耗尽，直到主额度重置时刻；
// 只有主额度归零而 Additional Budget 仍有剩余时不算耗尽。
func TestApplyQuotaHeadersMakesAccountExhausted(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "main-and-budget-zero", value: "rem=0&ov=100", want: true},
		{name: "budget-overspent", value: "rem=0&ov=300", want: true},
		{name: "budget-remaining", value: "rem=0&ov=50", want: false},
		{name: "main-remaining", value: "rem=10&ov=100", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quota := headerTestQuota("Copilot Pro")
			quota.Groups[0].Buckets[0].ResetTime = reset
			account := headerTestAccount(t, quota)
			if account.exhausted(now) {
				t.Fatal("合并前账号不应耗尽")
			}
			account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, tc.value))
			if got := account.exhausted(now); got != tc.want {
				t.Fatalf("exhausted(now) = %v, want %v", got, tc.want)
			}
			if tc.want {
				if until, ok := exhaustedUntil(mustSnapshot(account)); !ok || until.Format(time.RFC3339) != reset {
					t.Fatalf("exhaustedUntil = %s, %v, want %s", until, ok, reset)
				}
				if account.exhausted(now.Add(2 * time.Hour)) {
					t.Fatal("重置时刻之后账号应恢复")
				}
			}
		})
	}
}

// mustSnapshot 返回账号缓存额度的副本（可能为 nil）。
func mustSnapshot(a *poolAccount) *Quota {
	q, _ := a.quota.snapshot()
	return q
}

// TestParseQuotaKeepsBudgetDenominator parseQuota 在 additional_budget 桶上记录 overage_entitlement（分母）
// （导出字段 Entitlement），RemainingFraction 的计算不变。
func TestParseQuotaKeepsBudgetDenominator(t *testing.T) {
	for _, tc := range []struct {
		name            string
		snapshotKey     string
		overage         string
		wantEntitlement float64
		wantFraction    float64
	}{
		{name: "premium-models", snapshotKey: "premium_models", overage: `"overage_count":25,"overage_entitlement":100`,
			wantEntitlement: 100, wantFraction: 0.75},
		{name: "premium-interactions", snapshotKey: "premium_interactions", overage: `"overage_count":12.5,"overage_entitlement":50`,
			wantEntitlement: 50, wantFraction: 0.75},
		{name: "numeric-strings", snapshotKey: "premium_interactions", overage: `"overage_count":"30","overage_entitlement":"120"`,
			wantEntitlement: 120, wantFraction: 0.75},
		{name: "overspent", snapshotKey: "premium_interactions", overage: `"overage_count":150,"overage_entitlement":100`,
			wantEntitlement: 100, wantFraction: 0},
		{name: "unused", snapshotKey: "premium_interactions", overage: `"overage_count":0,"overage_entitlement":100`,
			wantEntitlement: 100, wantFraction: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := quotaTestParse(t, `{"copilot_plan":"individual_pro","quota_snapshots":{"`+tc.snapshotKey+`":{"unlimited":false,"percent_remaining":60,`+tc.overage+`}}}`)
			budget := quotaTestBucket(t, q, "additional_budget")
			if budget.Entitlement != tc.wantEntitlement {
				t.Fatalf("Entitlement = %v, want %v", budget.Entitlement, tc.wantEntitlement)
			}
			if !quotaTestApprox(budget.RemainingFraction, tc.wantFraction) {
				t.Fatalf("RemainingFraction = %v, want %v", budget.RemainingFraction, tc.wantFraction)
			}
		})
	}
}

// TestParseQuotaBudgetDenominatorFeedsHeaderMerge 端到端：parseQuota 的输出直接作为缓存，响应头的 ov 能据此重算 Additional Budget。
func TestParseQuotaBudgetDenominatorFeedsHeaderMerge(t *testing.T) {
	q := quotaTestParse(t, `{"copilot_plan":"individual_pro","quota_snapshots":{"premium_models":`+
		`{"unlimited":false,"percent_remaining":60,"overage_count":25,"overage_entitlement":200}}}`)
	account := headerTestAccount(t, &q)
	account.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "rem=55&ov=100"))
	got, _ := account.quota.snapshot()
	main, budget := quotaBuckets(got)
	if !quotaTestApprox(main.RemainingFraction, 0.55) || !quotaTestApprox(budget.RemainingFraction, 0.5) {
		t.Fatalf("合并结果 main=%+v budget=%+v, want 0.55 / 0.5", *main, *budget)
	}
}

// TestQuotaBucketJSONBudgetEntitlementKey 管理接口与 quota.json 的 JSON：主额度桶的键不变（description/remainingFraction/resetTime/window），
// additional_budget 桶多一个 entitlement 键（分母），且 JSON 往返后 Entitlement 保留；其余字段也不丢。
func TestQuotaBucketJSONBudgetEntitlementKey(t *testing.T) {
	q := quotaTestParse(t, `{"copilot_plan":"individual_pro","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_models":`+
		`{"unlimited":false,"percent_remaining":60,"overage_count":25,"overage_entitlement":100}}}`)
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Groups []struct {
			Buckets []map[string]any `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Groups) != 1 || len(decoded.Groups[0].Buckets) != 2 {
		t.Fatalf("JSON 结构不符: %s", data)
	}
	mainKeys := []string{"description", "remainingFraction", "resetTime", "window"}
	budgetKeys := []string{"description", "entitlement", "remainingFraction", "resetTime", "window"}
	for _, bucket := range decoded.Groups[0].Buckets {
		keys := make([]string, 0, len(bucket))
		for key := range bucket {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		want := mainKeys
		if bucket["window"] == "additional_budget" {
			want = budgetKeys
			if bucket["entitlement"] != float64(100) {
				t.Fatalf("additional_budget 的 entitlement = %v, want 100（完整输出 %s）", bucket["entitlement"], data)
			}
		}
		if !reflect.DeepEqual(keys, want) {
			t.Fatalf("窗口 %v 的 JSON 键 = %v, want %v（完整输出 %s）", bucket["window"], keys, want, data)
		}
	}
	// 往返（quota.json 恢复）后分母保留，其余字段不变。
	var restored Quota
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	budget := quotaTestBucket(t, restored, "additional_budget")
	if budget.Entitlement != 100 || !quotaTestApprox(budget.RemainingFraction, 0.75) || math.IsNaN(budget.RemainingFraction) {
		t.Fatalf("往返后的 Additional Budget = %+v, want Entitlement=100、比例 0.75", budget)
	}
	if !reflect.DeepEqual(restored.Groups, q.Groups) {
		t.Fatalf("往返后的窗口 = %+v, want %+v", restored.Groups, q.Groups)
	}
}

// TestApplyQuotaHeadersBudgetAfterRestartFromQuotaFile 端到端：额度经 saveQuotaLocked 写入缓存目录的 quota.json，
// 同一缓存目录新建账号（相当于重启）经 restoreQuota 恢复后，ov 头仍能据恢复的分母重算 additional_budget。
// 同时覆盖老版本 quota.json（additional_budget 桶没有 entitlement 键）恢复后 ov 不起作用。
func TestApplyQuotaHeadersBudgetAfterRestartFromQuotaFile(t *testing.T) {
	config := Config{APIKey: testAPIKey, Token: "oauth-alice", User: "alice", CacheDir: t.TempDir()}
	// restart 用同一 Config（同一缓存根目录与 Token，缓存目录相同）新建 Service 与账号，模拟进程重启。
	restart := func(t *testing.T) *poolAccount {
		t.Helper()
		return newPoolAccount(testService(t, config), "alice")
	}

	t.Run("entitlement-restored-and-ov-merged", func(t *testing.T) {
		q := quotaTestParse(t, `{"copilot_plan":"individual_pro","quota_snapshots":{"premium_models":`+
			`{"unlimited":false,"percent_remaining":60,"overage_count":25,"overage_entitlement":200}}}`)
		first := restart(t)
		first.quota.mu.Lock()
		first.quota.data = &q
		first.quota.checkedAt = time.Now()
		first.saveQuotaLocked(context.Background())
		first.quota.mu.Unlock()

		second := restart(t)
		if !second.quota.hasData() {
			t.Fatal("前提不成立：重启后应从 quota.json 恢复额度")
		}
		if budget := quotaTestBucket(t, mustQuotaValue(second), "additional_budget"); budget.Entitlement != 200 {
			t.Fatalf("恢复后的 Entitlement = %v, want 200", budget.Entitlement)
		}
		second.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "ov=100"))
		budget := quotaTestBucket(t, mustQuotaValue(second), "additional_budget")
		if !quotaTestApprox(budget.RemainingFraction, 0.5) {
			t.Fatalf("恢复后 ov=100 的 Additional Budget 比例 = %v, want 0.5（1 - 100/200）", budget.RemainingFraction)
		}
	})

	t.Run("old-file-without-entitlement-is-noop", func(t *testing.T) {
		config.CacheDir = t.TempDir() // 独立缓存目录，不受上一子测试写入的影响
		q := headerTestQuota("Copilot Pro")
		q.Groups[0].Buckets[1].Entitlement = 0 // 老版本 quota.json 的 additional_budget 桶没有 entitlement 键
		first := restart(t)
		first.quota.mu.Lock()
		first.quota.data = q
		first.quota.checkedAt = time.Now()
		first.saveQuotaLocked(context.Background())
		first.quota.mu.Unlock()

		second := restart(t)
		initial := mustQuotaValue(second)
		second.applyQuotaHeaders(headerTestHeader(headerPremiumModels, "ov=50"))
		if got := mustQuotaValue(second); !reflect.DeepEqual(got, initial) {
			t.Fatalf("缺分母的旧缓存只有 ov 时应 no-op:\n got  %+v\n want %+v", got, initial)
		}
	})
}

// mustQuotaValue 返回账号缓存额度的值拷贝；缓存没有额度时 panic（调用方须先确认有数据）。
func mustQuotaValue(a *poolAccount) Quota {
	return *mustSnapshot(a)
}
