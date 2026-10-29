package copilot

// 本文件覆盖 premium 快照的别名：新后端把 quota_snapshots.premium_interactions 改发为 premium_models。
// 取值优先 premium_models，缺失（或不是对象）时回退 premium_interactions，两者都缺失为无 premium 快照；
// 主额度窗口、Additional Budget（overage_count/overage_entitlement）与组织共享额度用完的判断都使用选中的快照，其余解析规则不变。

import "testing"

// aliasTestWant 描述解析结果中期望的某个额度窗口；missing 为 true 表示该窗口不应存在。
type aliasTestWant struct {
	window   string
	fraction float64
	missing  bool
}

// TestParseQuotaPremiumModelsAlias 表驱动覆盖 premium_models 与 premium_interactions 的取值优先级及两者的回退关系。
func TestParseQuotaPremiumModelsAlias(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		wants []aliasTestWant
	}{
		{
			name: "only-premium-models",
			raw: `{"copilot_plan":"individual_pro","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_models":` +
				`{"unlimited":false,"percent_remaining":60,"overage_count":25,"overage_entitlement":100}}}`,
			wants: []aliasTestWant{{window: "premium_requests", fraction: 0.6}, {window: "additional_budget", fraction: 0.75}},
		},
		{
			// 两个快照都存在时 premium_models 优先，主额度与 Additional Budget 都不能混用 premium_interactions 的值。
			name: "premium-models-wins-over-interactions",
			raw: `{"copilot_plan":"individual_pro","quota_reset_date":"2026-11-01","quota_snapshots":{` +
				`"premium_models":{"unlimited":false,"percent_remaining":30,"overage_count":10,"overage_entitlement":100},` +
				`"premium_interactions":{"unlimited":false,"percent_remaining":80,"overage_count":50,"overage_entitlement":100}}}`,
			wants: []aliasTestWant{{window: "premium_requests", fraction: 0.3}, {window: "additional_budget", fraction: 0.9}},
		},
		{
			// premium_models 没有 overage 字段时，Additional Budget 不输出（不从 premium_interactions 借用）。
			name: "premium-models-wins-no-budget-borrowed",
			raw: `{"copilot_plan":"individual_pro","quota_snapshots":{` +
				`"premium_models":{"unlimited":false,"percent_remaining":30},` +
				`"premium_interactions":{"unlimited":false,"percent_remaining":80,"overage_count":50,"overage_entitlement":100}}}`,
			wants: []aliasTestWant{{window: "premium_requests", fraction: 0.3}, {window: "additional_budget", missing: true}},
		},
		{
			name: "only-premium-interactions-unchanged",
			raw: `{"copilot_plan":"individual_pro","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":` +
				`{"unlimited":false,"percent_remaining":40,"overage_count":25,"overage_entitlement":100}}}`,
			wants: []aliasTestWant{{window: "premium_requests", fraction: 0.4}, {window: "additional_budget", fraction: 0.75}},
		},
		{
			// premium_models 不是对象（null）视为缺失，回退到 premium_interactions。
			name: "null-premium-models-falls-back",
			raw: `{"copilot_plan":"individual_pro","quota_snapshots":{"premium_models":null,` +
				`"premium_interactions":{"unlimited":false,"percent_remaining":40,"overage_count":25,"overage_entitlement":100}}}`,
			wants: []aliasTestWant{{window: "premium_requests", fraction: 0.4}, {window: "additional_budget", fraction: 0.75}},
		},
		{
			name:  "neither-snapshot",
			raw:   `{"copilot_plan":"individual_pro","quota_snapshots":{}}`,
			wants: []aliasTestWant{{window: "premium_requests", missing: true}, {window: "additional_budget", missing: true}},
		},
		{
			// token_based_billing 时主额度窗口名仍为 credits，别名不影响窗口命名。
			name: "premium-models-token-based-billing",
			raw: `{"copilot_plan":"individual_max","token_based_billing":true,"quota_snapshots":{"premium_models":` +
				`{"unlimited":false,"percent_remaining":96}}}`,
			wants: []aliasTestWant{{window: "credits", fraction: 0.96}, {window: "premium_requests", missing: true}},
		},
		{
			// Free 使用 chat 快照作主额度；premium_models 上的 overage 字段照常产生 Additional Budget。
			name: "free-chat-main-with-premium-models-budget",
			raw: `{"access_type_sku":"free_limited_copilot","quota_snapshots":{"chat":{"unlimited":false,"percent_remaining":75},` +
				`"premium_models":{"overage_count":20,"overage_entitlement":80}}}`,
			wants: []aliasTestWant{{window: "chat", fraction: 0.75}, {window: "additional_budget", fraction: 0.75}},
		},
		{
			// Free 的 premium_models 快照 overage_entitlement 为 0 时不输出 Additional Budget。
			name: "free-premium-models-zero-entitlement",
			raw: `{"access_type_sku":"free_limited_copilot","quota_snapshots":{"chat":{"unlimited":false,"percent_remaining":75},` +
				`"premium_models":{"overage_count":0,"overage_entitlement":0}}}`,
			wants: []aliasTestWant{{window: "chat", fraction: 0.75}, {window: "additional_budget", missing: true}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := quotaTestParse(t, tc.raw)
			for _, want := range tc.wants {
				if want.missing {
					if quotaTestHasBucket(q, want.window) {
						t.Fatalf("不应输出 %s 窗口: %+v", want.window, q.Groups)
					}
					continue
				}
				bucket := quotaTestBucket(t, q, want.window)
				if !quotaTestApprox(bucket.RemainingFraction, want.fraction) {
					t.Fatalf("%s 的剩余比例 = %v, want %v", want.window, bucket.RemainingFraction, want.fraction)
				}
			}
		})
	}
}

// TestParseQuotaPremiumModelsResetAt premium_models 快照级的 quota_reset_at 同时决定主额度与 Additional Budget 的重置时间。
func TestParseQuotaPremiumModelsResetAt(t *testing.T) {
	q := quotaTestParse(t, `{"copilot_plan":"individual_pro","quota_reset_date":"2026-12-01","quota_snapshots":{"premium_models":`+
		`{"unlimited":false,"percent_remaining":60,"overage_count":25,"overage_entitlement":100,"quota_reset_at":1790812800}}}`)
	const want = "2026-10-01T00:00:00Z"
	for _, window := range []string{"premium_requests", "additional_budget"} {
		if got := quotaTestBucket(t, q, window).ResetTime; got != want {
			t.Fatalf("%s 的重置时间 = %q, want %q", window, got, want)
		}
	}
}

// TestParseQuotaPremiumModelsPooledSpent 组织共享额度用完（unlimited 且 has_quota=false）的判断同样适用于 premium_models 快照。
func TestParseQuotaPremiumModelsPooledSpent(t *testing.T) {
	q := quotaTestParse(t, `{"copilot_plan":"business","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_models":`+
		`{"unlimited":true,"has_quota":false,"credits_used":12.5}}}`)
	bucket := quotaTestBucket(t, q, "premium_requests")
	if bucket.RemainingFraction != 0 || bucket.ResetTime != "2026-11-01" {
		t.Fatalf("premium_requests = %+v, want 剩余 0、重置 2026-11-01", bucket)
	}
	if len(q.Summary) != 0 {
		t.Fatalf("额度用完时不应输出 credits_used: %+v", q.Summary)
	}
}
