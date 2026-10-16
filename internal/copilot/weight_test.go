package copilot

// 本文件覆盖额度权重到 SWRR 权重的换算（见 quotaWeight.swrrWeight）：权重为 max(1, round(剩余比例 * 100))。
// 没有额度数据、剩余比例不足 0.5%、或剩余为 0 时权重都为 1，保证每个账号在轮询中至少占有一份。
// 剩余比例先经 weigh 取档位与比例，因此 Additional Budget 只在没有主额度剩余时才作为比例来源。

import "testing"

// TestSwrrWeightFromQuota 权重取值表：覆盖无额度数据、主额度的取整边界、上限 100，以及只剩 Additional Budget 的情况。
func TestSwrrWeightFromQuota(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quota *Quota
		want  int
	}{
		{name: "no-quota-data", quota: nil, want: 1},
		{name: "credits-0.8", quota: quotaOf(creditsBucket(0.8)), want: 80},
		{name: "credits-0.004-rounds-to-0-but-at-least-1", quota: quotaOf(creditsBucket(0.004)), want: 1},
		{name: "credits-0.015-rounds-half-away-from-zero", quota: quotaOf(creditsBucket(0.015)), want: 2},
		{name: "credits-1.0", quota: quotaOf(creditsBucket(1)), want: 100},
		{name: "credits-0-budget-0.5-uses-budget", quota: quotaOf(creditsBucket(0), budgetBucket(0.5)), want: 50},
		{name: "credits-0-budget-0-at-least-1", quota: quotaOf(creditsBucket(0), budgetBucket(0)), want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := weigh(tc.quota).swrrWeight(); got != tc.want {
				t.Fatalf("swrrWeight() = %d, want %d", got, tc.want)
			}
		})
	}
}
