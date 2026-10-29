package copilot

import (
	"math"
	"time"
)

// 本文件基于账号缓存的额度，提供两个判断：额度权重（档位与剩余比例），用于同一档内的平滑加权轮询，SWRR 权重由 swrrWeight 换算（见 Pool.pickLocked）；
// 额度耗尽判断，耗尽的账号暂不参与选择，直到主额度的重置时刻（见 poolAccount.exhausted）。
// 两者都只读取缓存的额度，不访问上游；缓存额度在刷新或合并生成响应头额度快照（见 applyQuotaHeaders）后变化，耗尽判断还取决于当前时间。

// additionalBudgetWindow 是 Additional Budget 额度窗口的 window 字段值，它不属于主额度。
const additionalBudgetWindow = "additional_budget"

// quotaWeight 是账号的额度权重。hasCredits 决定档位，选择时先于其他字段比较；fraction 决定同档内 SWRR 的权重（见 swrrWeight）。
type quotaWeight struct {
	hasCredits bool    // 主额度仍有剩余，即“有 Credits”档
	fraction   float64 // 剩余比例（0 到 1）：有 Credits 档为主额度的比例，否则为 Additional Budget 的比例，都没有时为 0
}

// quotaBuckets 返回额度中的主额度与 Additional Budget 窗口，各取第一个，不存在时为 nil；q 为 nil 时两者都为 nil。
// 主额度是第一个不属于 Additional Budget 的额度窗口。
func quotaBuckets(q *Quota) (main, budget *QuotaBucket) {
	if q == nil {
		return nil, nil
	}
	for _, group := range q.Groups {
		for i := range group.Buckets {
			bucket := &group.Buckets[i]
			if bucket.Window == additionalBudgetWindow {
				if budget == nil {
					budget = bucket
				}
			} else if main == nil {
				main = bucket
			}
		}
	}
	return main, budget
}

// weigh 根据额度快照计算权重；q 为 nil 即没有额度数据，此时权重为“无 Credits”档、值 0。
// 主额度剩余比例大于 0 时为“有 Credits”档；否则取 Additional Budget 的剩余比例，档位为“无 Credits”。
func weigh(q *Quota) quotaWeight {
	main, budget := quotaBuckets(q)
	if main != nil && main.RemainingFraction > 0 {
		return quotaWeight{hasCredits: true, fraction: main.RemainingFraction}
	}
	if budget != nil {
		return quotaWeight{fraction: budget.RemainingFraction}
	}
	return quotaWeight{}
}

// swrrWeight 返回账号在同档平滑加权轮询中的权重：max(1, round(fraction*100))。
// 没有额度数据、剩余比例为 0 或取整后不足 1 时权重为 1，保证同档内每个账号都会轮到（见 swrrPickLocked）。
func (w quotaWeight) swrrWeight() int {
	return max(1, int(math.Round(w.fraction*100)))
}

// exhaustedUntil 判断额度是否耗尽：主额度与 Additional Budget 的剩余比例都不大于 0（没有 Additional Budget 窗口视为没有剩余），
// 且主额度的重置时间可解析。耗尽时返回主额度的重置时刻与 true；否则返回零值与 false，即不因耗尽而跳过账号。
func exhaustedUntil(q *Quota) (time.Time, bool) {
	main, budget := quotaBuckets(q)
	if main == nil || main.RemainingFraction > 0 || (budget != nil && budget.RemainingFraction > 0) {
		return time.Time{}, false
	}
	return parseResetTime(main.ResetTime)
}

// parseResetTime 解析额度的重置时间：RFC3339 按其时刻，YYYY-MM-DD 取该日期 UTC 0 点；其他格式（含空串）返回 false。
func parseResetTime(text string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, text); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// exhausted 报告账号在 now 时是否因额度耗尽而暂不参与选择：按缓存额度判断已耗尽，且 now 早于主额度的重置时刻。
// 没有额度数据或重置时间不可解析时不视为耗尽。
func (a *poolAccount) exhausted(now time.Time) bool {
	quota, _ := a.quota.snapshot()
	until, ok := exhaustedUntil(quota)
	return ok && now.Before(until)
}
