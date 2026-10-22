package copilot

// 本文件覆盖“组织共享（pooled）Premium 额度已用完”的解析：
// premium_interactions 快照 unlimited=true 且 has_quota 明确为 false 时，表示组织共享额度已用完，
// parseQuota 输出剩余 0 的主额度窗口（重置时间按常规规则）。
// 例外：该快照 overage_permitted=true 且 copilot_plan 不是 business/enterprise（不区分大小写）时，行为不变（不输出额度窗口）。
// has_quota 为 true 或缺失、以及 Copilot Free 的 chat 快照，行为都不变。

import (
	"testing"
	"time"
)

// pooledTestResetAt 是快照级 quota_reset_at（Unix 秒）的测试值。
const pooledTestResetAt = 1793491200

// TestParseQuotaPooledPremiumSpent 覆盖组织共享额度用完时主额度窗口的输出条件、窗口名与重置时间。
func TestParseQuotaPooledPremiumSpent(t *testing.T) {
	resetAt := time.Unix(pooledTestResetAt, 0).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name        string
		raw         string
		wantBucket  bool   // 是否输出主额度窗口
		window      string // 主额度窗口名（wantBucket 为 true 时检查）
		description string
		reset       string
		wantSummary bool // 是否输出 credits_used 汇总
	}{
		{
			name:       "business-reset-date",
			raw:        `{"copilot_plan":"business","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":true,"has_quota":false,"credits_used":12.5}}}`,
			wantBucket: true, window: "premium_requests", description: "Premium requests", reset: "2026-11-01",
		},
		{
			name:       "business-reset-at",
			raw:        `{"copilot_plan":"business","quota_reset_date":"2026-12-01","quota_snapshots":{"premium_interactions":{"unlimited":true,"has_quota":false,"quota_reset_at":1793491200}}}`,
			wantBucket: true, window: "premium_requests", description: "Premium requests", reset: resetAt,
		},
		{
			name:       "enterprise-overage-permitted",
			raw:        `{"copilot_plan":"enterprise","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":true,"has_quota":false,"overage_permitted":true}}}`,
			wantBucket: true, window: "premium_requests", description: "Premium requests", reset: "2026-11-01",
		},
		{
			name:       "business-overage-permitted-case-insensitive",
			raw:        `{"copilot_plan":"Business","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":true,"has_quota":false,"overage_permitted":true}}}`,
			wantBucket: true, window: "premium_requests", description: "Premium requests", reset: "2026-11-01",
		},
		{
			name:       "individual-overage-permitted-unchanged",
			raw:        `{"copilot_plan":"individual","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":true,"has_quota":false,"overage_permitted":true}}}`,
			wantBucket: false,
		},
		{
			name:       "individual-overage-not-permitted",
			raw:        `{"copilot_plan":"individual","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":true,"has_quota":false,"overage_permitted":false}}}`,
			wantBucket: true, window: "premium_requests", description: "Premium requests", reset: "2026-11-01",
		},
		{
			name:       "business-token-based-billing",
			raw:        `{"copilot_plan":"business","token_based_billing":true,"quota_reset_date_utc":"2026-11-01T00:00:00Z","quota_snapshots":{"premium_interactions":{"unlimited":true,"has_quota":false}}}`,
			wantBucket: true, window: "credits", description: "Credits", reset: "2026-11-01T00:00:00Z",
		},
		{
			name:        "business-has-quota-true-unchanged",
			raw:         `{"copilot_plan":"business","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":true,"has_quota":true,"credits_used":12.5}}}`,
			wantBucket:  false,
			wantSummary: true,
		},
		{
			name:        "business-has-quota-missing-unchanged",
			raw:         `{"copilot_plan":"business","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":true,"credits_used":12.5}}}`,
			wantBucket:  false,
			wantSummary: true,
		},
		{
			name:       "free-chat-snapshot-unchanged",
			raw:        `{"access_type_sku":"free_limited_copilot","copilot_plan":"business","quota_reset_date":"2026-11-01","quota_snapshots":{"chat":{"unlimited":true,"has_quota":false},"premium_interactions":{"unlimited":true,"has_quota":false}}}`,
			wantBucket: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := quotaTestParse(t, tc.raw)
			main, _ := quotaBuckets(&q)
			if !tc.wantBucket {
				if main != nil {
					t.Fatalf("不应输出主额度窗口，实际 %+v", *main)
				}
			} else {
				if main == nil {
					t.Fatalf("缺少主额度窗口: %+v", q.Groups)
				}
				want := QuotaBucket{Window: tc.window, Description: tc.description, RemainingFraction: 0, ResetTime: tc.reset}
				if *main != want {
					t.Fatalf("主额度窗口 = %+v, want %+v", *main, want)
				}
			}
			if tc.wantSummary {
				if len(q.Summary) != 1 || q.Summary[0].Key != "credits_used" || q.Summary[0].Value != 12.5 {
					t.Fatalf("Summary = %+v, want 一个 credits_used=12.5", q.Summary)
				}
			} else if len(q.Summary) != 0 {
				t.Fatalf("不应输出 credits_used 汇总: %+v", q.Summary)
			}
		})
	}
}

// TestParseQuotaPooledPremiumSpentKeepsAdditionalBudget 组织共享额度用完时，Additional Budget 仍照常输出，不影响主额度窗口。
func TestParseQuotaPooledPremiumSpentKeepsAdditionalBudget(t *testing.T) {
	q := quotaTestParse(t, `{"copilot_plan":"business","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":`+
		`{"unlimited":true,"has_quota":false,"overage_count":25,"overage_entitlement":100}}}`)
	if main := quotaTestBucket(t, q, "premium_requests"); main.RemainingFraction != 0 || main.ResetTime != "2026-11-01" {
		t.Fatalf("主额度窗口 = %+v, want 剩余 0、重置 2026-11-01", main)
	}
	if budget := quotaTestBucket(t, q, "additional_budget"); !quotaTestApprox(budget.RemainingFraction, 0.75) {
		t.Fatalf("additional_budget = %+v, want 0.75", budget)
	}
}

// TestExhaustedUntilPooledPremiumSpent 组织共享额度用完且没有 Additional Budget 时视为耗尽，直到重置时刻；
// Additional Budget 仍有剩余时不视为耗尽（沿用既有语义）。
func TestExhaustedUntilPooledPremiumSpent(t *testing.T) {
	t.Run("no-budget-exhausted", func(t *testing.T) {
		q := quotaTestParse(t, `{"copilot_plan":"business","quota_reset_date_utc":"2026-11-01T00:00:00Z","quota_snapshots":{"premium_interactions":{"unlimited":true,"has_quota":false}}}`)
		until, ok := exhaustedUntil(&q)
		want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		if !ok || !until.Equal(want) {
			t.Fatalf("exhaustedUntil = (%s, %v), want (%s, true)", until, ok, want)
		}
	})
	t.Run("budget-remaining-not-exhausted", func(t *testing.T) {
		q := quotaTestParse(t, `{"copilot_plan":"business","quota_reset_date_utc":"2026-11-01T00:00:00Z","quota_snapshots":{"premium_interactions":`+
			`{"unlimited":true,"has_quota":false,"overage_count":10,"overage_entitlement":100}}}`)
		if until, ok := exhaustedUntil(&q); ok {
			t.Fatalf("Additional Budget 仍有剩余时不应视为耗尽，实际 (%s, true)", until)
		}
	})
}
