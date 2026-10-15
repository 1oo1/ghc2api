package copilot

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"testing"
	"time"
)

// quotaTestIndividualBody 是个人版账号的额度响应：Premium 剩余 80%，账号级重置日期为 2026-11-01。
const quotaTestIndividualBody = `{"copilot_plan":"individual","quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":80}}}`

// quotaTestParse 解析额度正文，失败即终止测试。
func quotaTestParse(t *testing.T, raw string) Quota {
	t.Helper()
	q, err := parseQuota([]byte(raw))
	if err != nil {
		t.Fatalf("parseQuota(%s) error = %v", raw, err)
	}
	return q
}

// quotaTestBucket 返回指定 window 的额度桶；不存在时终止测试。
func quotaTestBucket(t *testing.T, q Quota, window string) QuotaBucket {
	t.Helper()
	for _, group := range q.Groups {
		for _, bucket := range group.Buckets {
			if bucket.Window == window {
				return bucket
			}
		}
	}
	t.Fatalf("缺少 window=%q 的额度桶: %+v", window, q.Groups)
	return QuotaBucket{}
}

// quotaTestHasBucket 报告是否存在指定 window 的额度桶。
func quotaTestHasBucket(q Quota, window string) bool {
	for _, group := range q.Groups {
		for _, bucket := range group.Buckets {
			if bucket.Window == window {
				return true
			}
		}
	}
	return false
}

// quotaTestPlan 返回订阅计划名；没有订阅信息时返回空字符串。
func quotaTestPlan(q Quota) string {
	if q.Subscription == nil {
		return ""
	}
	return q.Subscription.Plan
}

// quotaTestApprox 比较浮点比例，容忍极小的舍入误差。
func quotaTestApprox(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

// TestParseQuotaCreditsForTokenBasedBilling token_based_billing=true 时主额度为 credits，重置时间取快照所在的账号级字段。
func TestParseQuotaCreditsForTokenBasedBilling(t *testing.T) {
	q := quotaTestParse(t, `{"copilot_plan":"individual_max","token_based_billing":true,"quota_reset_date_utc":"2026-10-01T00:00:00Z","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":96}}}`)
	if got := quotaTestPlan(q); got != "Copilot Max" {
		t.Fatalf("计划名 = %q, want Copilot Max", got)
	}
	bucket := quotaTestBucket(t, q, "credits")
	if bucket.Description != "Credits" || !quotaTestApprox(bucket.RemainingFraction, 0.96) || bucket.ResetTime != "2026-10-01T00:00:00Z" {
		t.Fatalf("credits 额度桶 = %+v", bucket)
	}
	if quotaTestHasBucket(q, "premium_requests") {
		t.Fatal("token_based_billing=true 时不应输出 premium_requests")
	}
	// 解析函数不设置获取时间，该字段由 fetchQuota 填写。
	if !q.ObservedAt.IsZero() {
		t.Fatalf("parseQuota 不应设置 ObservedAt，实际 = %s", q.ObservedAt)
	}
}

// TestParseQuotaPremiumRequestsByDefault 未启用 token_based_billing 时主额度为 premium_requests。
func TestParseQuotaPremiumRequestsByDefault(t *testing.T) {
	q := quotaTestParse(t, `{"copilot_plan":"individual_pro","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":40}}}`)
	if got := quotaTestPlan(q); got != "Copilot Pro+" {
		t.Fatalf("计划名 = %q, want Copilot Pro+", got)
	}
	bucket := quotaTestBucket(t, q, "premium_requests")
	if bucket.Description != "Premium requests" || !quotaTestApprox(bucket.RemainingFraction, 0.4) {
		t.Fatalf("premium_requests 额度桶 = %+v", bucket)
	}
}

// TestParseQuotaCopilotFreeUsesChatSnapshot Copilot Free 使用 chat 快照；没有快照时回退到月度配额字段。
func TestParseQuotaCopilotFreeUsesChatSnapshot(t *testing.T) {
	q := quotaTestParse(t, `{"access_type_sku":"free_limited_copilot","quota_snapshots":{"chat":{"unlimited":false,"percent_remaining":75}}}`)
	if got := quotaTestPlan(q); got != "Copilot Free" {
		t.Fatalf("计划名 = %q, want Copilot Free", got)
	}
	bucket := quotaTestBucket(t, q, "chat")
	if bucket.Description != "Chat messages" || !quotaTestApprox(bucket.RemainingFraction, 0.75) {
		t.Fatalf("chat 额度桶 = %+v", bucket)
	}
	if quotaTestHasBucket(q, "premium_requests") {
		t.Fatal("Copilot Free 不应输出 premium_requests")
	}

	q = quotaTestParse(t, `{"access_type_sku":"free_limited_copilot","token_based_billing":true,"quota_snapshots":{"chat":{"unlimited":false,"percent_remaining":75}}}`)
	if bucket := quotaTestBucket(t, q, "credits"); !quotaTestApprox(bucket.RemainingFraction, 0.75) {
		t.Fatalf("Copilot Free + token_based_billing 的 credits 桶 = %+v", bucket)
	}

	q = quotaTestParse(t, `{"access_type_sku":"free_limited_copilot","monthly_quotas":{"chat":100},"limited_user_quotas":{"chat":40}}`)
	if bucket := quotaTestBucket(t, q, "chat"); !quotaTestApprox(bucket.RemainingFraction, 0.4) {
		t.Fatalf("无 chat 快照时应按月度配额计算: %+v", bucket)
	}
}

// TestParseQuotaUnlimitedSummarizesCreditsUsed unlimited 不产生额度桶；credits_used 按 has_quota 决定是否输出。
func TestParseQuotaUnlimitedSummarizesCreditsUsed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		premium string
		want    bool
	}{
		{name: "has-quota-true", premium: `{"unlimited":true,"credits_used":12.5,"has_quota":true}`, want: true},
		{name: "has-quota-absent", premium: `{"unlimited":true,"credits_used":12.5}`, want: true},
		{name: "has-quota-false", premium: `{"unlimited":true,"credits_used":12.5,"has_quota":false}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := quotaTestParse(t, `{"quota_snapshots":{"premium_interactions":`+tc.premium+`}}`)
			if len(q.Groups) != 0 {
				t.Fatalf("unlimited 不应产生额度桶: %+v", q.Groups)
			}
			if !tc.want {
				if len(q.Summary) != 0 {
					t.Fatalf("has_quota=false 时不应输出 credits_used: %+v", q.Summary)
				}
				return
			}
			if len(q.Summary) != 1 || q.Summary[0].Key != "credits_used" || q.Summary[0].Value != 12.5 {
				t.Fatalf("Summary = %+v, want 一个 credits_used=12.5", q.Summary)
			}
		})
	}
}

// TestParseQuotaAdditionalBudget 只有 overage_count 与 overage_entitlement 都合法且 entitlement > 0 时才输出 Additional Budget。
func TestParseQuotaAdditionalBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		count        string // 原始 JSON 值，用于构造 overage_count
		entitlement  string // 原始 JSON 值，用于构造 overage_entitlement
		wantBudget   bool
		wantFraction float64
	}{
		{name: "partially-used", count: "25", entitlement: "100", wantBudget: true, wantFraction: 0.75},
		{name: "overage-clamped-at-zero", count: "150", entitlement: "100", wantBudget: true, wantFraction: 0},
		{name: "zero-entitlement", count: "0", entitlement: "0", wantBudget: false},
		{name: "null-count", count: "null", entitlement: "100", wantBudget: false},
		{name: "negative-count", count: "-1", entitlement: "100", wantBudget: false},
		{name: "non-numeric-string-count", count: `"NaN"`, entitlement: "100", wantBudget: false},
		{name: "bool-count", count: "false", entitlement: "100", wantBudget: false},
		{name: "non-numeric-entitlement", count: "25", entitlement: `"abc"`, wantBudget: false},
		{name: "missing-entitlement", count: "25", entitlement: "null", wantBudget: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":50,` +
				`"overage_count":` + tc.count + `,"overage_entitlement":` + tc.entitlement + `}}}`
			q := quotaTestParse(t, raw)
			// 预算字段非法时只影响预算桶，主额度仍然输出。
			if primary := quotaTestBucket(t, q, "premium_requests"); !quotaTestApprox(primary.RemainingFraction, 0.5) {
				t.Fatalf("主额度 = %+v, want 0.5", primary)
			}
			if !tc.wantBudget {
				if quotaTestHasBucket(q, "additional_budget") {
					t.Fatalf("非法或 entitlement 为 0 时不应输出 additional_budget: %+v", q.Groups)
				}
				return
			}
			budget := quotaTestBucket(t, q, "additional_budget")
			if budget.Description != "Additional Budget" || !quotaTestApprox(budget.RemainingFraction, tc.wantFraction) {
				t.Fatalf("additional_budget = %+v, want fraction %v", budget, tc.wantFraction)
			}
		})
	}
}

// TestParseQuotaResetTimeSources 重置时间：快照的 quota_reset_at（Unix 秒）优先，其次是账号级的日期字段，
// RFC3339 转为 UTC，纯日期保持原样；无法解析的字段跳过。
func TestParseQuotaResetTimeSources(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "snapshot-unix-seconds-wins",
			raw:  `{"quota_reset_date":"2026-11-01","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":10,"quota_reset_at":1790812800}}}`,
			want: "2026-10-01T00:00:00Z",
		},
		{
			name: "rfc3339-converted-to-utc",
			raw:  `{"quota_reset_date_utc":"2026-10-01T08:00:00+08:00","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":10}}}`,
			want: "2026-10-01T00:00:00Z",
		},
		{
			name: "date-only-kept",
			raw:  `{"quota_reset_date":"2026-10-01","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":10}}}`,
			want: "2026-10-01",
		},
		{
			name: "invalid-utc-falls-through-to-limited-user-date",
			raw:  `{"quota_reset_date_utc":"soon","limited_user_reset_date":"2026-10-01","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":10}}}`,
			want: "2026-10-01",
		},
		{
			name: "no-valid-source",
			raw:  `{"quota_reset_date":"next month","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":10}}}`,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := quotaTestParse(t, tc.raw)
			if got := quotaTestBucket(t, q, "premium_requests").ResetTime; got != tc.want {
				t.Fatalf("ResetTime = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseQuotaPlanNames 计划名由 copilot_plan 或 access_type_sku 映射；未知值没有订阅信息。
func TestParseQuotaPlanNames(t *testing.T) {
	for raw, want := range map[string]string{
		`{"copilot_plan":"individual"}`:                "Copilot Pro",
		`{"copilot_plan":"individual_pro"}`:            "Copilot Pro+",
		`{"copilot_plan":"individual_max"}`:            "Copilot Max",
		`{"copilot_plan":"individual_edu"}`:            "Copilot Student",
		`{"copilot_plan":"business"}`:                  "Copilot Business",
		`{"copilot_plan":"enterprise"}`:                "Copilot Enterprise",
		`{"access_type_sku":"free_limited_copilot"}`:   "Copilot Free",
		`{"access_type_sku":"free_educational_quota"}`: "Copilot Student",
		`{"copilot_plan":"unknown-plan"}`:              "",
	} {
		if got := quotaTestPlan(quotaTestParse(t, raw)); got != want {
			t.Errorf("parseQuota(%s) 计划名 = %q, want %q", raw, got, want)
		}
	}
}

// TestParseQuotaRejectsNonObjectPayloads 顶层不是 JSON 对象时返回错误。
func TestParseQuotaRejectsNonObjectPayloads(t *testing.T) {
	for _, raw := range []string{"null", "[]", `"text"`, "12", "not json"} {
		if _, err := parseQuota([]byte(raw)); err == nil {
			t.Errorf("parseQuota(%q) 应返回错误", raw)
		}
	}
}

// TestParseQuotaFieldErrorsAreIsolated 单个字段格式错误只影响该字段，其他字段仍然解析。
func TestParseQuotaFieldErrorsAreIsolated(t *testing.T) {
	t.Run("invalid-percent-keeps-budget-and-plan", func(t *testing.T) {
		q := quotaTestParse(t, `{"copilot_plan":"individual","quota_reset_date":"2026-10-01","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":"bad","overage_count":25,"overage_entitlement":100}}}`)
		if got := quotaTestPlan(q); got != "Copilot Pro" {
			t.Fatalf("计划名 = %q, want Copilot Pro", got)
		}
		if quotaTestHasBucket(q, "premium_requests") {
			t.Fatal("percent_remaining 非法时不应输出主额度")
		}
		budget := quotaTestBucket(t, q, "additional_budget")
		if !quotaTestApprox(budget.RemainingFraction, 0.75) || budget.ResetTime != "2026-10-01" {
			t.Fatalf("additional_budget = %+v", budget)
		}
	})
	t.Run("invalid-billing-flag-means-premium-requests", func(t *testing.T) {
		q := quotaTestParse(t, `{"token_based_billing":"yes","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":40}}}`)
		if bucket := quotaTestBucket(t, q, "premium_requests"); !quotaTestApprox(bucket.RemainingFraction, 0.4) {
			t.Fatalf("非布尔的 token_based_billing 应按 false 处理: %+v", bucket)
		}
	})
	t.Run("invalid-plan-type-keeps-buckets", func(t *testing.T) {
		q := quotaTestParse(t, `{"copilot_plan":123,"quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":40}}}`)
		if q.Subscription != nil {
			t.Fatalf("非字符串的 copilot_plan 不应产生订阅: %+v", q.Subscription)
		}
		if bucket := quotaTestBucket(t, q, "premium_requests"); !quotaTestApprox(bucket.RemainingFraction, 0.4) {
			t.Fatalf("主额度 = %+v", bucket)
		}
	})
	t.Run("invalid-snapshot-reset-falls-back-to-account-date", func(t *testing.T) {
		q := quotaTestParse(t, `{"quota_reset_date":"2026-10-01","quota_snapshots":{"premium_interactions":{"unlimited":false,"percent_remaining":40,"quota_reset_at":"soon"}}}`)
		if got := quotaTestBucket(t, q, "premium_requests").ResetTime; got != "2026-10-01" {
			t.Fatalf("ResetTime = %q, want 2026-10-01", got)
		}
	})
}

// TestFetchQuotaReadsCopilotUserEndpoint fetchQuota 与 /usage 一样请求 /copilot_internal/user，并记录获取时间。
func TestFetchQuotaReadsCopilotUserEndpoint(t *testing.T) {
	type request struct{ path, authorization string }
	requests := make(chan request, 1)
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- request{r.URL.Path, r.Header.Get("Authorization")}
		_, _ = io.WriteString(w, quotaTestIndividualBody)
	})
	s := testService(t, Config{Token: "oauth-quota", Client: client})
	before := time.Now()
	q, err := s.fetchQuota(context.Background())
	if err != nil {
		t.Fatalf("fetchQuota() error = %v", err)
	}
	got := <-requests
	if got.path != "/copilot_internal/user" || got.authorization != "token oauth-quota" {
		t.Fatalf("上游请求 = %+v, want GET /copilot_internal/user 使用 token 鉴权", got)
	}
	if got := quotaTestPlan(q); got != "Copilot Pro" {
		t.Fatalf("计划名 = %q, want Copilot Pro", got)
	}
	if q.ObservedAt.Before(before) || q.ObservedAt.After(time.Now()) {
		t.Fatalf("ObservedAt = %s，应在本次获取期间", q.ObservedAt)
	}
}

// TestFetchQuotaRateLimitedReturnsSentinel 429 返回 errQuotaRateLimited，调用方据此保留旧数据并标记 Stale。
func TestFetchQuotaRateLimitedReturnsSentinel(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	s := testService(t, Config{Token: "oauth-quota", Client: client})
	q, err := s.fetchQuota(context.Background())
	if !errors.Is(err, errQuotaRateLimited) {
		t.Fatalf("fetchQuota() error = %v, want errQuotaRateLimited", err)
	}
	if q.Subscription != nil || len(q.Groups) != 0 {
		t.Fatalf("限流时不应返回数据: %+v", q)
	}
}

// TestFetchQuotaOtherFailuresAreUpstreamErrors 其他非 2xx 返回 upstreamError；2xx 但正文非法时返回错误。
func TestFetchQuotaOtherFailuresAreUpstreamErrors(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"message":"denied"}`)
			})
			s := testService(t, Config{Token: "oauth-quota", Client: client})
			_, err := s.fetchQuota(context.Background())
			var upstream *upstreamError
			if !errors.As(err, &upstream) || upstream.status != status {
				t.Fatalf("fetchQuota() error = %v, want upstreamError 状态 %d", err, status)
			}
			if errors.Is(err, errQuotaRateLimited) {
				t.Fatal("非 429 错误不应被当作限流")
			}
		})
	}

	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	})
	s := testService(t, Config{Token: "oauth-quota", Client: client})
	if _, err := s.fetchQuota(context.Background()); err == nil {
		t.Fatal("2xx 但正文不是合法额度时应返回错误")
	}
}

// TestFetchQuotaForbiddenWithRetryAfterIsRateLimited 403 且带 Retry-After 头视为限流（与 429 相同）；
// 没有该头的 403 仍是普通的上游错误。
func TestFetchQuotaForbiddenWithRetryAfterIsRateLimited(t *testing.T) {
	for _, tc := range []struct {
		name        string
		retryAfter  string
		wantLimited bool
	}{
		{name: "with-retry-after", retryAfter: "120", wantLimited: true},
		{name: "without-retry-after", wantLimited: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"forbidden"}`)
			})
			s := testService(t, Config{Token: "oauth-quota", Client: client})
			_, err := s.fetchQuota(context.Background())
			if tc.wantLimited {
				if !errors.Is(err, errQuotaRateLimited) {
					t.Fatalf("fetchQuota() error = %v, want errQuotaRateLimited", err)
				}
				return
			}
			var upstream *upstreamError
			if !errors.As(err, &upstream) || upstream.status != http.StatusForbidden {
				t.Fatalf("fetchQuota() error = %v, want *upstreamError 状态 403", err)
			}
			if errors.Is(err, errQuotaRateLimited) {
				t.Fatal("没有 Retry-After 的 403 不应被当作限流")
			}
		})
	}
}
