package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// quotaRefreshTick 是后台刷新的检查间隔（见 RunQuotaRefresh）：每次检查都按各账号的 checkedAt 与刷新间隔决定是否请求上游。
// 声明为变量以便测试临时缩短，实现中不得改为常量。
var quotaRefreshTick = time.Minute

// quotaErrorRefreshInterval 是请求失败后异步强制刷新额度的节流间隔：距上次尝试不足该时长时不再触发（见 refreshAfterError）。
// 声明为变量以便测试临时修改，实现中不得改为常量。
var quotaErrorRefreshInterval = time.Minute

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
	// Entitlement 是 Additional Budget 的分母 overage_entitlement，只在 additional_budget 窗口上有值（由 parseQuota 填写）。
	// 随 quota.json 落盘，重启后响应头合并 ov（已用量）时仍能重算 RemainingFraction（见 applyQuotaHeaders）；
	// 旧版本写入的 quota.json 没有该键（零值），此时响应头无法更新 Additional Budget，直到下一次刷新。
	Entitlement float64 `json:"entitlement,omitempty"`
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
	// 新后端把 premium_interactions 改发为 premium_models：优先取 premium_models，缺失（或不是对象）时整体回退到 premium_interactions。
	// 回退是对象级的：premium_models 存在但字段缺失时不会借用 premium_interactions 的字段。
	premium := snapshots.object("premium_models")
	if premium == nil {
		premium = snapshots.object("premium_interactions")
	}
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
		// 组织共享（pooled）Premium 额度用完时快照仍为 unlimited，但 has_quota=false，VS Code 会暂停该席位（magpie #1063）。
		// 非组织套餐允许超额计费时不算用完。
		copilotPlan := strings.ToLower(account.text("copilot_plan"))
		overage, _ := selected.boolean("overage_permitted")
		organization := copilotPlan == "business" || copilotPlan == "enterprise"
		if plan != "Copilot Free" && hasKnown && !hasQuota && (!overage || organization) {
			buckets = append(buckets, QuotaBucket{Window: window, Description: label, RemainingFraction: 0, ResetTime: reset})
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
			RemainingFraction: max(0, 1-count/entitlement), ResetTime: quotaReset(premium, account),
			Entitlement: entitlement})
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

// fetchQuota 请求 /copilot_internal/user 获取额度，成功时设置 ObservedAt。
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

// quotaCache 保存账号最近一次的额度结果，由 poolAccount 持有。每次刷新后写入账号缓存目录的 quota.json，
// 新建账号时从该文件恢复（见 restoreQuota），因此重启后仍遵守刷新间隔的节流。
type quotaCache struct {
	mu        sync.Mutex
	data      *Quota    // 最近一次成功获取的额度；限流时保留旧值并标记 Stale
	checkedAt time.Time // 最近一次尝试（成功或失败）的时间，用于刷新间隔内不重复请求
	errText   string    // 最近一次刷新失败的说明（不含 token）；成功后清空
	// retry 为 true 表示上一次刷新被调用方取消，它不算一次尝试，因此下一次列表刷新或后台轮询（最迟一分钟后）不受节流限制。
	retry bool
	// refreshing 为 true 表示异步刷新（见 refreshAsync）正在进行，同一账号同时只保留一个。
	// 它只约束异步刷新（请求失败与新增账号），不约束后台定时刷新与列表刷新。
	refreshing bool
}

// quotaFile 是 quota.json 的内容，字段名固定：额度（无数据时省略）、最近一次尝试的时间与失败说明（无错误时省略）。
type quotaFile struct {
	Quota     *Quota    `json:"quota,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
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

// restoreQuota 从账号缓存目录的 quota.json 恢复额度与最近一次尝试的结果，只在账号创建时调用（尚未被并发访问）。
// 缓存目录为空或文件不存在时静默忽略；其他读取错误（损坏、超过大小限制、权限等）记录 Warn 后同样按没有缓存处理，不影响启动。
// checked_at 晚于当前时间视为无效，按从未检查处理（零值，立即允许刷新），但额度与 error 文本仍照常恢复。
func (a *poolAccount) restoreQuota() {
	path := a.service.cacheFile("quota")
	if path == "" {
		return
	}
	var file quotaFile
	if err := readCacheFile(path, &file); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// 此处没有请求 ctx，使用 Background，日志落到 Service 默认 logger。
			a.service.log(context.Background(), slog.LevelWarn, "读取额度缓存失败", "error", a.service.redactString(err.Error()))
		}
		return
	}
	a.quota.data = file.Quota
	a.quota.checkedAt = file.CheckedAt
	if a.quota.checkedAt.After(time.Now()) {
		a.quota.checkedAt = time.Time{}
	}
	a.quota.errText = file.Error
}

// saveQuotaLocked 把当前额度状态写入 quota.json。调用方须持有 a.quota.mu，使并发刷新时文件内容总是最新状态。
// 写入失败只记录告警，不影响刷新结果；账号已删除时 writeCache 直接跳过。
func (a *poolAccount) saveQuotaLocked(ctx context.Context) {
	file := quotaFile{Quota: a.quota.data, CheckedAt: a.quota.checkedAt, Error: a.quota.errText}
	if err := a.service.writeCache("quota", file); err != nil {
		a.service.log(ctx, slog.LevelWarn, "写入额度缓存失败", "error", a.service.redactString(err.Error()))
	}
}

// refresh 刷新账号额度。force 为 false 时，距上次尝试不足账号的刷新间隔（Service.quotaRefreshInterval）则直接返回。
// 限流（429）保留旧数据并标记 Stale，不视为错误；其他错误保留旧数据并返回错误，由调用方决定如何展示。
// 每次有结果的刷新（成功、限流或其他错误）后都写入 quota.json；调用方取消时不写入。
func (a *poolAccount) refresh(ctx context.Context, force bool) error {
	a.quota.mu.Lock()
	fresh := !force && !a.quota.retry && !a.quota.checkedAt.IsZero() &&
		time.Since(a.quota.checkedAt) < a.service.quotaRefreshInterval
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
	var result error
	switch {
	case err == nil:
		a.quota.data = &quota
		a.quota.errText = ""
	case errors.Is(err, errQuotaRateLimited):
		if a.quota.data != nil {
			stale := *a.quota.data
			stale.Stale = true
			a.quota.data = &stale
			a.quota.errText = ""
		} else {
			a.quota.errText = errQuotaRateLimited.Error()
		}
	default:
		a.quota.errText = a.quotaErrorText(err)
		result = err
	}
	a.saveQuotaLocked(ctx)
	return result
}

// quotaErrorText 返回额度刷新失败时展示给用户的文本（quota_error 与 502 响应共用）：
// 超时为固定的 errQuotaTimeout（不含 URL 等上游细节）；其他错误经脱敏后保留。
func (a *poolAccount) quotaErrorText(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return errQuotaTimeout.Error()
	}
	return a.service.redactString(err.Error())
}

// refreshAfterError 在账号请求失败（非 429）后异步强制刷新额度，节流规则见 refreshAsync。
func (a *poolAccount) refreshAfterError() {
	a.refreshAsync(quotaErrorRefreshInterval)
}

// refreshAsync 在后台强制刷新额度，不阻塞调用方，忽略 checked_at 的刷新间隔。
// 距上次尝试不足 throttle 时不启动（throttle 为 0 表示不节流）；同一账号已有异步刷新进行中（refreshing）时也不启动。
// 两项检查与设置 refreshing 在同一把锁内完成，因此并发调用至多启动一次刷新。
// 后台定时刷新与列表刷新不设置 refreshing，与它们并发时可能多请求一次上游。
// 刷新使用独立于调用方的 context（总超时 quotaRefreshTimeout），调用方结束或客户端断开不影响它；结果照常写入缓存与 quota.json。
func (a *poolAccount) refreshAsync(throttle time.Duration) {
	a.quota.mu.Lock()
	if a.quota.refreshing || (throttle > 0 && time.Since(a.quota.checkedAt) < throttle) {
		a.quota.mu.Unlock()
		return
	}
	a.quota.refreshing = true
	a.quota.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), quotaRefreshTimeout)
		defer cancel()
		// 错误已经写入 quota_error 并落盘，这里无需再处理返回值。
		_ = a.refresh(ctx, true)
		a.quota.mu.Lock()
		a.quota.refreshing = false
		a.quota.mu.Unlock()
	}()
}

// RunQuotaRefresh 在后台刷新全部账号（含禁用账号）的额度，阻塞直到 ctx 取消。
// 启动时立即刷新一轮，之后每隔 quotaRefreshTick 刷新一轮；每轮都重新取账号快照，因此运行期间新增的账号同样被刷新。
// 刷新是非强制的：距上次尝试不足刷新间隔的账号不请求上游，启动时从缓存恢复的 checked_at 同样生效。
func (p *Pool) RunQuotaRefresh(ctx context.Context) {
	ticker := time.NewTicker(quotaRefreshTick)
	defer ticker.Stop()
	for {
		roundCtx, cancel := context.WithTimeout(ctx, quotaRefreshTimeout)
		refreshQuotas(roundCtx, p.accountsSnapshot())
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
