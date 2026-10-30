package copilot

// 本文件实现账号的错误计数与长冷却：哪些错误计入、何时进入冷却、进入与解除冷却的副作用，以及 cooldown.json 的持久化。
// 冷却期间账号完全不参与选择（见 Pool.topGroupLocked）。计数点：poolAccount.record（按写给客户端的状态码，或标记的网络错误）
// 与 Pool.pickSession（session 获取失败）。
//
// 时序：进入冷却时，截止时间的设置与计数清零同步完成，因此触发冷却的响应立即返回，账号立即被排除；
// 清空缓存、删除会话绑定与写入 cooldown.json 是异步的（settleCooldown），不阻塞请求路径。

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// DefaultAccountCooldown 是账号冷却的缺省时长（PoolConfig.AccountCooldown 未设置或非正时使用）。
const DefaultAccountCooldown = 2 * time.Hour

// cooldownStreakLimit 是触发冷却所需的连续计入错误次数（429、5xx、网络错误与 session 获取失败）。
const cooldownStreakLimit = 3

// cooldownReasonNetwork 是无状态码的错误（网络错误等）记录的冷却原因。
const cooldownReasonNetwork = "network"

// cooldownFileName 是冷却记录在账号缓存目录下的文件名（即 cacheFile("cooldown") 指向的 cooldown.json）。
const cooldownFileName = "cooldown"

// cooldownRecord 是 cooldown.json 的内容：冷却截止时间与触发冷却的原因。
type cooldownRecord struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

// failureRule 描述一次错误的处理方式。
type failureRule int

const (
	ruleIgnore     failureRule = iota // 其他 4xx（400、404、413、422 等）：既不计数也不清零
	ruleImmediate                     // 401、403：一次即冷却
	ruleCounted                       // 429、5xx、网络错误与 session 获取失败：连续 cooldownStreakLimit 次后冷却
	ruleUntilReset                    // 402（额外预算用尽）：一次即冷却，截止到主额度重置时刻（见 untilResetDeadline）
)

// ruleOf 按状态码决定错误的处理方式，只对状态码 >= 400 调用（2xx 清零与 3xx 忽略由调用方处理）。
func ruleOf(status int) failureRule {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ruleImmediate
	case status == http.StatusPaymentRequired:
		return ruleUntilReset
	case status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
		return ruleCounted
	default:
		return ruleIgnore
	}
}

// cooldownTracker 保存账号的连续错误计数与冷却原因，由 poolAccount 持有。
type cooldownTracker struct {
	// mu 保护 streak 与 reason，只做内存操作，不做 IO；锁序：fileMu 先于 mu。
	mu     sync.Mutex
	streak int    // 连续计入的错误次数，触发冷却或成功后清零
	reason string // 最近一次触发冷却的原因（状态码字符串或 "network"），冷却中有效
	// fileMu 串行化 cooldown.json 的写入与删除，并保证异步写入前冷却仍有效（见 writeRecord、liftCooldown）。
	fileMu sync.Mutex
}

// countFailure 计入一次错误。rule 为 ruleIgnore 时不做任何事；reason 为触发冷却时记录的原因。
// 账号已在冷却中时不计数、不延长冷却、不重复触发（冷却截止时间与原因保持不变）。
// 触发时同步设置截止时间并清零计数，之后的副作用见 enterCooldown。返回本次是否使账号进入冷却（风控记录据此判断，见 Pool.recordRisk）。
func (a *poolAccount) countFailure(ctx context.Context, rule failureRule, reason string) bool {
	if rule == ruleIgnore {
		return false
	}
	now := time.Now()
	until := now.Add(a.cooldown)
	if rule == ruleUntilReset {
		until = a.untilResetDeadline(now) // 在 errs.mu 之外读取额度缓存，避免嵌套加锁
	}
	a.errs.mu.Lock()
	if !a.service.available(now) {
		a.errs.mu.Unlock()
		return false
	}
	if rule == ruleCounted {
		a.errs.streak++
		if a.errs.streak < cooldownStreakLimit {
			a.errs.mu.Unlock()
			return false
		}
	}
	a.errs.streak = 0
	a.errs.reason = reason
	a.service.setCooldown(until)
	a.errs.mu.Unlock()
	a.enterCooldown(ctx, until, reason)
	return true
}

// untilResetDeadline 返回 402 冷却的截止时间：缓存额度中主额度的重置时刻；
// 没有额度数据、没有主额度、重置时间不可解析或不晚于 now 时退回 now + a.cooldown。
// 额外预算用尽不会自行恢复，冷却到重置时刻可避免反复试探；付款后可手动解除冷却（clear_cooldown）。
func (a *poolAccount) untilResetDeadline(now time.Time) time.Time {
	quota, _ := a.quota.snapshot()
	if main, _ := quotaBuckets(quota); main != nil {
		if reset, ok := parseResetTime(main.ResetTime); ok && reset.After(now) {
			return reset
		}
	}
	return now.Add(a.cooldown)
}

// countSessionFailure 计入一次 session 获取失败：上游错误按其状态码计入；其他错误（网络等）按网络错误计入。
// 返回上游状态码（非上游错误为 0）与本次是否使账号进入冷却。
func (a *poolAccount) countSessionFailure(ctx context.Context, err error) (int, bool) {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		return upstream.status, a.countFailure(ctx, ruleOf(upstream.status), strconv.Itoa(upstream.status))
	}
	return 0, a.countFailure(ctx, ruleCounted, cooldownReasonNetwork)
}

// resetStreak 在成功响应（状态 < 300）后清零连续错误计数。
func (a *poolAccount) resetStreak() {
	a.errs.mu.Lock()
	defer a.errs.mu.Unlock()
	a.errs.streak = 0
}

// cooldownReason 返回冷却原因，供账号视图使用；调用方须只在冷却中读取它。
func (a *poolAccount) cooldownReason() string {
	a.errs.mu.Lock()
	defer a.errs.mu.Unlock()
	return a.errs.reason
}

// enterCooldown 输出进入冷却的 Warn 日志（不含 token），并在后台执行副作用。
// 副作用独立于触发它的请求：请求的取消与结束都不影响它，因此使用 context.WithoutCancel 保留日志等上下文值。
// 调用方不得持有 a.errs.mu。
func (a *poolAccount) enterCooldown(ctx context.Context, until time.Time, reason string) {
	a.service.log(ctx, slog.LevelWarn, "账号进入冷却", "id", a.id, "reason", reason, "until", until.Format(time.RFC3339))
	a.effects.begin()
	go func() {
		defer a.effects.end()
		a.settleCooldown(context.WithoutCancel(ctx), until, reason)
	}()
}

// cooldownEffects 计数进行中的冷却异步副作用，供关闭时等待（见 Pool.WaitCooldownEffectsTimeout）。
// 它用互斥锁保护计数与空闲通道，因此登记（begin）与等待（idleChan）可以任意并发调用；零值可用。
// 之所以不用 sync.WaitGroup：WaitGroup 不允许在计数为零时与 Wait 并发调用 Add，而关闭与新的冷却可能同时发生。
type cooldownEffects struct {
	mu      sync.Mutex
	pending int
	idle    chan struct{} // 有等待者且仍有进行中的副作用时存在；计数降为零时关闭并置空
}

// begin 登记一个进行中的副作用。
func (e *cooldownEffects) begin() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending++
}

// end 注销一个已结束的副作用；计数降为零时唤醒所有等待者。
func (e *cooldownEffects) end() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending--
	if e.pending == 0 && e.idle != nil {
		close(e.idle)
		e.idle = nil
	}
}

// idleChan 返回一个通道：调用时进行中的副作用全部结束后关闭。调用时没有进行中的副作用则立即关闭。
func (e *cooldownEffects) idleChan() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending == 0 {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	if e.idle == nil {
		e.idle = make(chan struct{})
	}
	return e.idle
}

// WaitCooldownEffects 等待所有进入冷却的异步副作用（清空缓存、删除会话绑定、写入 cooldown.json）结束。
// 没有上界，测试与需要完整结果的场景使用；服务关闭时应使用 WaitCooldownEffectsTimeout。
func (p *Pool) WaitCooldownEffects() {
	<-p.effects.idleChan()
}

// WaitCooldownEffectsTimeout 最多等待 d 时长，等待进入冷却的异步副作用结束。全部结束返回 true；超时返回 false，
// 此时副作用仍在后台进行（服务关闭时不再等待，保存会话亲和表照常进行）。
func (p *Pool) WaitCooldownEffectsTimeout(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-p.effects.idleChan():
		return true
	case <-timer.C:
		return false
	}
}

// settleCooldown 执行进入冷却的异步副作用（由 enterCooldown 启动，并计入 poolAccount.effects），顺序固定：
// 先写入 cooldown.json（崩溃时冷却不会丢失），再清空 session 与模型列表（内存与磁盘），最后删除指向本账号的会话绑定。
// 破坏性步骤执行前（清缓存的步骤在取得缓存锁之后）都检查账号仍处于本次冷却（截止时间仍为 until）；已被解除则跳过（见 liftCooldown）。
func (a *poolAccount) settleCooldown(ctx context.Context, until time.Time, reason string) {
	a.writeRecord(ctx, until, reason)
	still := func() bool { return a.service.cooldownUntil.Load() == until.UnixNano() }
	if err := errors.Join(a.service.forgetSession(still), a.service.forgetModels(still)); err != nil {
		a.service.log(ctx, slog.LevelWarn, "清空冷却账号的缓存失败", "id", a.id, "error", a.service.redactString(err.Error()))
	}
	if a.affinity != nil && still() {
		a.affinity.dropAccount(a.id)
	}
}

// writeRecord 在 fileMu 内写入 cooldown.json，但只在账号的冷却截止时间仍等于 until 时写入：
// 已被解除或已被新的冷却取代时跳过，使迟到的副作用不会让已解除的冷却复活。
func (a *poolAccount) writeRecord(ctx context.Context, until time.Time, reason string) {
	a.errs.fileMu.Lock()
	defer a.errs.fileMu.Unlock()
	if a.service.cooldownUntil.Load() != until.UnixNano() {
		return
	}
	// 截止时间持久化为秒精度的 RFC3339（UTC）；内存中的截止时间保持原值。
	record := cooldownRecord{Until: until.UTC().Truncate(time.Second), Reason: reason}
	if err := a.service.writeCache(cooldownFileName, record); err != nil {
		a.service.log(ctx, slog.LevelWarn, "写入冷却记录失败", "id", a.id, "error", a.service.redactString(err.Error()))
	}
}

// restoreCooldown 从账号缓存目录的 cooldown.json 恢复仍在有效期内的冷却，只在账号创建时调用（尚未被并发访问）。
// 文件不存在或截止时间已过时忽略；内容损坏时记录 Warn 并删除该文件，之后启动不再重复告警。
func (a *poolAccount) restoreCooldown(now time.Time) {
	path := a.service.cacheFile(cooldownFileName)
	if path == "" {
		return
	}
	var record cooldownRecord
	if err := readCacheFile(path, &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		// 此处没有请求 ctx，使用 Background，日志落到 Service 默认 logger。
		a.service.log(context.Background(), slog.LevelWarn, "读取冷却记录失败，已删除", "id", a.id, "error", a.service.redactString(err.Error()))
		if removeErr := a.service.removeCacheFile(cooldownFileName); removeErr != nil {
			a.service.log(context.Background(), slog.LevelWarn, "删除损坏的冷却记录失败", "id", a.id, "error", a.service.redactString(removeErr.Error()))
		}
		return
	}
	if !record.Until.After(now) {
		return
	}
	a.errs.reason = record.Reason
	a.service.setCooldown(record.Until)
}

// liftCooldown 手动解除冷却：
//  1. 在 fileMu 内先删除 cooldown.json（仅当截止时间仍为读取时的值，即本次冷却仍有效时）；删除失败时返回错误，内存保持不变，账号仍冷却；
//  2. 删除成功后以 CompareAndSwap(读取时的截止时间, 0) 清除内存中的冷却，并清空原因与连续错误计数。
//     截止时间已变化（期间产生了新的冷却）时不清除新冷却：它的副作用会自行写入记录。
//
// 与异步副作用的并发：writeRecord 在 fileMu 内检查截止时间，因此解除之后迟到的写入会被跳过，不会复活记录。
func (a *poolAccount) liftCooldown() error {
	until := a.service.cooldownUntil.Load()
	a.errs.fileMu.Lock()
	defer a.errs.fileMu.Unlock()
	if a.service.cooldownUntil.Load() == until {
		if err := a.service.removeCacheFile(cooldownFileName); err != nil {
			return err
		}
	}
	a.errs.mu.Lock()
	defer a.errs.mu.Unlock()
	if a.service.cooldownUntil.CompareAndSwap(until, 0) {
		a.errs.streak = 0
		a.errs.reason = ""
	}
	return nil
}

// writeCacheUnlessCooling 写入 session.json 或 models.json；账号冷却中跳过，使冷却前已开始的请求不会把缓存回写到磁盘。
// 调用方须已持有对应的缓存锁（sessionMu 或 modelsMu），因此异步清空缓存会等待这次写入结束，之后磁盘上不会残留旧缓存。
func (s *Service) writeCacheUnlessCooling(name string, value any) error {
	if !s.available(time.Now()) {
		return nil
	}
	return s.writeCache(name, value)
}

// failureKind 是本次请求在响应写入器上标记的失败性质（见 markFailure 与 poolAccount.record）。
type failureKind int

const (
	failureNone    failureKind = iota
	failureNetwork             // 网络错误：写出的 502，或上游成功响应的正文中断；计为 network
	failureIgnored             // 上游的非错误状态（如 3xx）：既不计数也不清零
)

// markFailure 在响应写入器链上标记本次请求的失败性质（见 poolAccount.record）。
// Service 写出非上游错误或上游非错误状态时、以及上游成功响应的正文读取失败时调用；
// 沿 Unwrap 逐层查找统计用的 statusResponseWriter，找不到时不做任何事。
func markFailure(w http.ResponseWriter, kind failureKind) {
	for w != nil {
		if sw, ok := w.(*statusResponseWriter); ok {
			sw.failure = kind
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}

// setCooldown 设置账号的冷却截止时间，之前不参与选择。只设置截止时间：不记录原因、不清空缓存、不写盘（进入冷却的完整流程见 countFailure）。
func (s *Service) setCooldown(until time.Time) {
	s.cooldownUntil.Store(until.UnixNano())
}

// available 报告账号在 now 时是否可参与选择；冷却截止时刻本身即视为可用，从未冷却的账号始终可用。
func (s *Service) available(now time.Time) bool {
	until := s.cooldownUntil.Load()
	return until == 0 || now.UnixNano() >= until
}
