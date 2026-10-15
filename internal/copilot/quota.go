package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// quotaCacheTTL 是额度的刷新间隔：距上次尝试不足该时长时不重复请求上游（强制刷新除外）。
const quotaCacheTTL = time.Minute

// maxQuotaResetSeconds 是重置时间戳的上限（公元 10000 年），超过则视为无效。
const maxQuotaResetSeconds = 253402300800

// errQuotaRateLimited 表示 GitHub 对额度接口限流（HTTP 429）。调用方应保留旧数据并标记为 Stale。
var errQuotaRateLimited = errors.New("额度查询被 GitHub 限流")

// errQuotaTimeout 是额度刷新超时时写入 quota_error 的固定中文说明，不包含 URL 等上游细节。
var errQuotaTimeout = errors.New("额度查询超时")

// Quota 是账号的 Copilot 额度快照，JSON 结构供管理控制台展示。
type Quota struct {
	Subscription *QuotaSubscription `json:"subscription,omitempty"`
	Groups       []QuotaGroup       `json:"groups,omitempty"`
	Summary      []QuotaMetric      `json:"summary,omitempty"`
	Stale        bool               `json:"stale,omitempty"`      // 最近一次刷新被限流，数据为旧值
	ObservedAt   time.Time          `json:"observedAt,omitempty"` // 数据获取时间（parseQuota 不设置）
}

// QuotaSubscription 是订阅计划信息。
type QuotaSubscription struct {
	Plan     string `json:"plan"`
	TierName string `json:"tierName"`
}

// QuotaGroup 是一组额度窗口。
type QuotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []QuotaBucket `json:"buckets"`
}

// QuotaBucket 是一个额度窗口。
type QuotaBucket struct {
	Window            string  `json:"window"`      // credits | premium_requests | chat | additional_budget
	Description       string  `json:"description"` // Credits | Premium requests | Chat messages | Additional Budget
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
}

// QuotaMetric 是不属于额度窗口的汇总指标。
type QuotaMetric struct {
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Value  float64 `json:"value"`
	Format string  `json:"format"`
}

// quotaFields 是额度 JSON 对象的原始字段。每个字段单独解码，某个字段格式错误不影响其他字段。
type quotaFields map[string]json.RawMessage

// quotaPlanNames 把 copilot_plan 映射为展示用的计划名。
var quotaPlanNames = map[string]string{
	"individual_edu": "Copilot Student", "individual": "Copilot Pro", "individual_pro": "Copilot Pro+",
	"individual_max": "Copilot Max", "business": "Copilot Business", "enterprise": "Copilot Enterprise",
}

// parseQuota 解析 /copilot_internal/user 的响应。顶层不是 JSON 对象（含 null）时返回错误；
// 单个字段缺失或格式错误只影响该字段对应的输出。返回值的 ObservedAt 由调用方设置。
// 解析规则移植自 cpa-github-copilot，去掉了其中的 admission 字段。
func parseQuota(raw []byte) (Quota, error) {
	var account quotaFields
	if err := json.Unmarshal(raw, &account); err != nil {
		return Quota{}, errors.New("Copilot 额度响应不是 JSON 对象")
	}
	if account == nil {
		return Quota{}, errors.New("Copilot 额度响应不是 JSON 对象")
	}
	var q Quota
	plan := quotaPlan(account)
	if plan != "" {
		q.Subscription = &QuotaSubscription{Plan: plan, TierName: plan}
	}

	snapshots := account.object("quota_snapshots")
	premium := snapshots.object("premium_interactions")
	tokenBilling := account.isTrue("token_based_billing")
	selected := premium
	window, label := "premium_requests", "Premium requests"
	if tokenBilling {
		window, label = "credits", "Credits"
	}
	if plan == "Copilot Free" {
		selected = snapshots.object("chat")
		if !tokenBilling {
			window, label = "chat", "Chat messages"
		}
	}

	reset := quotaReset(selected, account)
	var buckets []QuotaBucket
	unlimited, known := selected.boolean("unlimited")
	switch {
	case known && unlimited:
		hasQuota, hasKnown := selected.boolean("has_quota")
		if used, ok := selected.number("credits_used"); ok && (!hasKnown || hasQuota) {
			q.Summary = append(q.Summary, QuotaMetric{Key: "credits_used", Label: "Credits Used", Value: used, Format: "number"})
		}
	case known:
		if remaining, ok := selected.number("percent_remaining"); ok {
			entitlement, allocated := selected.number("entitlement")
			if !allocated || entitlement > 0 {
				buckets = append(buckets, QuotaBucket{Window: window, Description: label,
					RemainingFraction: min(100, remaining) / 100, ResetTime: reset})
			}
		}
	case plan == "Copilot Free" && selected == nil:
		// Copilot Free 没有 chat 快照时，按月度配额与剩余配额计算。
		total, ok := account.object("monthly_quotas").number("chat")
		remaining, valid := account.object("limited_user_quotas").number("chat")
		if ok && valid && total > 0 {
			buckets = append(buckets, QuotaBucket{Window: window, Description: label,
				RemainingFraction: min(1, remaining/total), ResetTime: reset})
		}
	}

	// Additional Budget 只要 overage_count 与 overage_entitlement 都合法且额度大于 0 即输出，不影响主额度。
	count, validCount := premium.number("overage_count")
	entitlement, validEntitlement := premium.number("overage_entitlement")
	if validCount && validEntitlement && entitlement > 0 {
		buckets = append(buckets, QuotaBucket{Window: "additional_budget", Description: "Additional Budget",
			RemainingFraction: max(0, 1-count/entitlement), ResetTime: quotaReset(premium, account)})
	}
	if len(buckets) > 0 {
		q.Groups = []QuotaGroup{{DisplayName: "GitHub Copilot", Buckets: buckets}}
	}
	return q, nil
}

// quotaPlan 由 access_type_sku 或 copilot_plan 得到计划名；无法识别时返回空字符串。
func quotaPlan(account quotaFields) string {
	switch account.text("access_type_sku") {
	case "free_limited_copilot":
		return "Copilot Free"
	case "free_educational_quota":
		return "Copilot Student"
	}
	// 可以申请限量版的账号，套餐信息不可靠，不展示计划名。
	if account.isTrue("can_signup_for_limited") {
		return ""
	}
	return quotaPlanNames[account.text("copilot_plan")]
}

// quotaReset 返回额度重置时间：快照中的 quota_reset_at（Unix 秒）优先，其次是账号级的日期字段。
// RFC3339 时间转为 UTC；纯日期（YYYY-MM-DD）保持原样；都无法解析时返回空字符串。
func quotaReset(snapshot, account quotaFields) string {
	if seconds, ok := snapshot.number("quota_reset_at"); ok && seconds > 0 && seconds < maxQuotaResetSeconds {
		return time.Unix(int64(seconds), 0).UTC().Format(time.RFC3339)
	}
	for _, key := range []string{"quota_reset_date_utc", "quota_reset_date", "limited_user_reset_date"} {
		value := account.text(key)
		if t, err := time.Parse(time.RFC3339, value); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
		if _, err := time.Parse(time.DateOnly, value); err == nil {
			return value
		}
	}
	return ""
}

// object 返回字段对应的 JSON 对象；字段缺失或不是对象时返回 nil。
func (f quotaFields) object(key string) quotaFields {
	var value quotaFields
	if json.Unmarshal(f[key], &value) != nil {
		return nil
	}
	return value
}

// text 返回字段的字符串值；缺失或类型不符时返回空字符串。
func (f quotaFields) text(key string) string {
	var value string
	_ = json.Unmarshal(f[key], &value)
	return value
}

// boolean 只接受字面量 true 或 false，返回值及是否合法。
func (f quotaFields) boolean(key string) (bool, bool) {
	switch string(f[key]) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// isTrue 报告字段是否为字面量 true；缺失或非布尔值视为 false。
func (f quotaFields) isTrue(key string) bool {
	value, _ := f.boolean(key)
	return value
}

// number 返回非负有限数值。接受 JSON 数字或数字字符串，NaN、Inf、负数与其他格式都视为无效。
func (f quotaFields) number(key string) (float64, bool) {
	var number json.Number
	if json.Unmarshal(f[key], &number) != nil || number == "" {
		return 0, false
	}
	value, err := number.Float64()
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

// fetchQuota 请求 /copilot_internal/user 获取额度（与 /usage 使用同一接口），成功时设置 ObservedAt。
// 429 返回 errQuotaRateLimited；其他非 2xx 返回 *upstreamError；2xx 但正文非法时返回解析错误。
func (s *Service) fetchQuota(ctx context.Context) (Quota, error) {
	res, err := s.githubRequest(ctx, "/copilot_internal/user", true)
	if err != nil {
		return Quota{}, err
	}
	defer res.Body.Close()
	data, err := readLimited(res.Body, maxMetadataBody)
	if err != nil {
		return Quota{}, err
	}
	switch {
	case res.StatusCode == http.StatusTooManyRequests,
		res.StatusCode == http.StatusForbidden && res.Header.Get("Retry-After") != "":
		// GitHub 对额度接口的限流有时以带 Retry-After 的 403 返回。
		return Quota{}, errQuotaRateLimited
	case res.StatusCode/100 != 2:
		return Quota{}, &upstreamError{res.StatusCode, data, res.Header}
	}
	quota, err := parseQuota(data)
	if err != nil {
		return Quota{}, err
	}
	quota.ObservedAt = time.Now()
	return quota, nil
}

// quotaCache 保存账号最近一次的额度结果，由 poolAccount 持有。
type quotaCache struct {
	mu        sync.Mutex
	data      *Quota    // 最近一次成功获取的额度；限流时保留旧值并标记 Stale
	checkedAt time.Time // 最近一次尝试（成功或失败）的时间，用于 quotaCacheTTL 内不重复请求
	errText   string    // 最近一次刷新失败的说明（不含 token）；成功后清空
	// retry 为 true 表示上一次刷新被调用方取消，它不算一次尝试，因此下一次列表刷新不受节流限制。
	retry bool
}

// snapshot 返回额度的副本与最近一次失败说明，供账号视图使用。
func (c *quotaCache) snapshot() (*Quota, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var copied *Quota
	if c.data != nil {
		value := *c.data
		copied = &value
	}
	return copied, c.errText
}

// hasData 报告是否存在可用的旧数据。
func (c *quotaCache) hasData() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data != nil
}

// refresh 刷新账号额度。force 为 false 时，距上次尝试不足 quotaCacheTTL 则直接返回。
// 限流（429）保留旧数据并标记 Stale，不视为错误；其他错误保留旧数据并返回错误，由调用方决定如何展示。
func (a *poolAccount) refresh(ctx context.Context, force bool) error {
	a.quota.mu.Lock()
	fresh := !force && !a.quota.retry && !a.quota.checkedAt.IsZero() && time.Since(a.quota.checkedAt) < quotaCacheTTL
	a.quota.mu.Unlock()
	if fresh {
		return nil
	}
	quota, err := a.service.fetchQuota(ctx)
	// 调用方主动取消（例如客户端断开）不是上游状态：不更新 checkedAt，也不写入错误；
	// 只标记 retry，使下一次列表请求会重新请求上游。
	if errors.Is(err, context.Canceled) {
		a.quota.mu.Lock()
		a.quota.retry = true
		a.quota.mu.Unlock()
		return err
	}

	a.quota.mu.Lock()
	defer a.quota.mu.Unlock()
	a.quota.retry = false
	a.quota.checkedAt = time.Now()
	switch {
	case err == nil:
		a.quota.data = &quota
		a.quota.errText = ""
		return nil
	case errors.Is(err, errQuotaRateLimited):
		if a.quota.data != nil {
			stale := *a.quota.data
			stale.Stale = true
			a.quota.data = &stale
			a.quota.errText = ""
		} else {
			a.quota.errText = errQuotaRateLimited.Error()
		}
		return nil
	default:
		a.quota.errText = a.quotaErrorText(err)
		return err
	}
}

// quotaErrorText 返回额度刷新失败时展示给用户的文本（quota_error 与 502 响应共用）：
// 超时为固定的 errQuotaTimeout（不含 URL 等上游细节）；其他错误经脱敏后保留。
func (a *poolAccount) quotaErrorText(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return errQuotaTimeout.Error()
	}
	return a.service.redactString(err.Error())
}
