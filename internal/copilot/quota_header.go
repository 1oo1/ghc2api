package copilot

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// 本文件把生成请求的上游响应头中的额度快照合并进账号缓存的额度（语义参照 VS Code），使额度在两次轮询之间也能及时更新。
// 头名：免费用户（缓存额度的计划为 Copilot Free）取 x-quota-snapshot-chat；其余账号优先 x-quota-snapshot-premium_models，
// 缺失时回退 x-quota-snapshot-premium_interactions。头值是 URL 编码键值对，认 rem（剩余百分比）、ov（overage_count）、
// rst（RFC3339 重置时间）、ent（仅 -1 表示不限量），其他键（如 ovPerm）忽略。

const (
	quotaHeaderChat               = "x-quota-snapshot-chat"
	quotaHeaderPremiumModels      = "x-quota-snapshot-premium_models"
	quotaHeaderPremiumInteraction = "x-quota-snapshot-premium_interactions"
)

// quotaHeaderSnapshot 是响应头中解析出的额度快照；各字段缺失或非法时为零值（指针为 nil）。
type quotaHeaderSnapshot struct {
	remaining *float64  // rem 原值（百分比，未限制上限）
	overage   *float64  // ov：Additional Budget 已用量
	reset     time.Time // rst；零值表示缺失或非法
	unlimited bool      // ent 解析为整数 -1（对应 VS Code 的 unlimited: entitlement === -1）
}

// parseQuotaHeader 解析额度头的值。url.ParseQuery 遇到坏键值对会跳过并继续解析其余部分，
// 这里忽略它的错误、直接使用返回的部分结果；字段级的非法值（非数字、NaN、Inf、负数、非 RFC3339）只丢弃该字段。
// ent 仅在 strconv.Atoi 解析为 -1 时置 unlimited，缺失、非法或其他值均为 false。
func parseQuotaHeader(value string) quotaHeaderSnapshot {
	values, _ := url.ParseQuery(value)
	var snap quotaHeaderSnapshot
	snap.remaining = headerNumber(values.Get("rem"))
	snap.overage = headerNumber(values.Get("ov"))
	if t, err := time.Parse(time.RFC3339, values.Get("rst")); err == nil {
		snap.reset = t
	}
	if ent, err := strconv.Atoi(values.Get("ent")); err == nil && ent == -1 {
		snap.unlimited = true
	}
	return snap
}

// headerNumber 解析非负有限数值；其他情况返回 nil。
func headerNumber(text string) *float64 {
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return nil
	}
	return &v
}

// applyQuotaHeaders 把响应头中的额度快照合并进缓存额度。规则：
//   - 缓存没有额度数据或头缺失时完全 no-op（ObservedAt 也不动）；缓存不会被头创建。
//   - 主额度窗口：rem 合法且头不是不限量（unlimited）时 RemainingFraction = min(100, rem)/100；rst 合法时更新 ResetTime（UTC 的 RFC3339）。
//     unlimited 时不合并 rem：parseQuota 对组织共享额度耗尽的 unlimited 快照造了 RemainingFraction=0 的主窗口，
//     不限量头的 rem 会把它错误地改回有额度。
//   - Additional Budget 窗口：ov 合法且已记录分母（Entitlement > 0）时重算 RemainingFraction；其 ResetTime 不变。
//   - 窗口不存在时对应字段不合并，也不新建窗口。头值合法但没有任何字段被合并时同样 no-op（不更新 ObservedAt）。
//   - 至少合并了一个字段后才更新 ObservedAt；不改 Stale 与 checkedAt（不影响轮询节流）。
//     头合并本身不触发写盘；之后任何一次刷新落盘会带上合并结果。
//
// premium_models 头存在（值为空串或非法也算）时不回退读 premium_interactions（与 parseQuota 的对象级回退一致：存在即选定）；
// header.Get 无法区分空值头与缺失，因此用 header.Values 判断存在性。
// 写时复制：snapshot() 是浅拷贝，管理接口可能正在序列化共享的 Groups/Buckets，因此先复制再改，最后替换缓存指针。
func (a *poolAccount) applyQuotaHeaders(header http.Header) {
	a.quota.mu.Lock()
	defer a.quota.mu.Unlock()
	data := a.quota.data
	if data == nil {
		return
	}
	free := data.Subscription != nil && data.Subscription.Plan == "Copilot Free"
	value := header.Get(quotaHeaderPremiumModels)
	switch {
	case free:
		value = header.Get(quotaHeaderChat)
	case len(header.Values(quotaHeaderPremiumModels)) == 0:
		value = header.Get(quotaHeaderPremiumInteraction)
	}
	if value == "" {
		return
	}
	snap := parseQuotaHeader(value)

	updated := *data
	updated.Groups = make([]QuotaGroup, len(data.Groups))
	for i, group := range data.Groups {
		updated.Groups[i] = QuotaGroup{DisplayName: group.DisplayName, Buckets: append([]QuotaBucket(nil), group.Buckets...)}
	}
	main, budget := quotaBuckets(&updated) // 指向已复制的窗口
	merged := false
	if main != nil {
		if snap.remaining != nil && !snap.unlimited {
			main.RemainingFraction = min(100, *snap.remaining) / 100
			merged = true
		}
		if !snap.reset.IsZero() {
			main.ResetTime = snap.reset.UTC().Format(time.RFC3339)
			merged = true
		}
	}
	if budget != nil && snap.overage != nil && budget.Entitlement > 0 {
		budget.RemainingFraction = max(0, 1-*snap.overage/budget.Entitlement)
		merged = true
	}
	if !merged {
		return
	}
	updated.ObservedAt = time.Now()
	a.quota.data = &updated
}
