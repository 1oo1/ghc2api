package copilot

// 本文件计算账号的额度权重，用于在优先级相同的账号之间排序（见 Pool.pickLocked）。
// 权重只读取账号缓存的额度，不访问上游；额度刷新后才会变化。

// additionalBudgetWindow 是 Additional Budget 额度窗口的 window 字段值，它不属于主额度。
const additionalBudgetWindow = "additional_budget"

// quotaWeight 是账号的额度权重。比较时先看档位（有 Credits 高于无 Credits），同档内剩余比例高者优先。
type quotaWeight struct {
	hasCredits bool    // 主额度仍有剩余，即“有 Credits”档
	fraction   float64 // 剩余比例（0 到 1）：有 Credits 档为主额度的比例，否则为 Additional Budget 的比例，都没有时为 0
}

// weigh 根据额度快照计算权重；q 为 nil 即没有额度数据，此时权重为“无 Credits”档、值 0。
// 主额度是第一个不属于 Additional Budget 的额度窗口：剩余比例大于 0 时为“有 Credits”档；
// 否则取 Additional Budget 的剩余比例，档位为“无 Credits”。
func weigh(q *Quota) quotaWeight {
	var main, budget *QuotaBucket
	if q != nil {
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
	}
	if main != nil && main.RemainingFraction > 0 {
		return quotaWeight{hasCredits: true, fraction: main.RemainingFraction}
	}
	if budget != nil {
		return quotaWeight{fraction: budget.RemainingFraction}
	}
	return quotaWeight{}
}

// better 报告 w 是否严格优于 o：有 Credits 档优先，同档内剩余比例高者优先。两者相等时返回 false。
func (w quotaWeight) better(o quotaWeight) bool {
	if w.hasCredits != o.hasCredits {
		return w.hasCredits
	}
	return w.fraction > o.fraction
}
