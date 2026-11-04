package copilot

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RuntimeSettings 是可在运行中修改的服务级配置。
type RuntimeSettings struct {
	APIKey                       string        // 去除首尾空白；为空时忽略，保持当前值
	ManagementKey                string        // 去除首尾空白；为空时忽略，保持当前值
	ModelsCacheTTL               time.Duration // <=0 使用 DefaultModelsCacheTTL
	QuotaRefreshInterval         time.Duration // <=0 使用 DefaultQuotaRefreshInterval
	AccountCooldown              time.Duration // <=0 使用 DefaultAccountCooldown
	SessionAffinityMaxPerAccount int           // <=0 不限制
	MaxConcurrentRequests        int           // <=0 不限制
}

// ApplySettings 立即应用运行时设置；对进行中的请求无影响。PoolConfig 中的对应字段只是初始值。
//   - 密钥：之后的请求只接受新密钥；新 api_key 加入所有账号的脱敏列表，之后新增的账号也使用它；
//     新 management_key 使旧密钥签发的控制台令牌立即失效。
//   - 模型缓存有效期、额度刷新间隔：作用于所有现有账号与之后新增的账号。
//   - 冷却时长：之后进入的冷却使用新时长，已在冷却中的截止时间不变。
//   - 绑定上限：之后的选择立即使用新上限。
//   - 并发上限：调低后进行中的请求不中断，在途数不低于新上限时新的生成请求返回 503。
func (p *Pool) ApplySettings(s RuntimeSettings) {
	p.settingsMu.Lock()
	defer p.settingsMu.Unlock()
	if key := strings.TrimSpace(s.APIKey); key != "" {
		p.apiKey.Store(key)
	}
	if key := strings.TrimSpace(s.ManagementKey); key != "" {
		p.managementKey.Store(key)
	}
	p.modelsCacheTTL.Store(positiveOr(s.ModelsCacheTTL, DefaultModelsCacheTTL))
	p.quotaRefreshInterval.Store(positiveOr(s.QuotaRefreshInterval, DefaultQuotaRefreshInterval))
	p.accountCooldown.Store(positiveOr(s.AccountCooldown, DefaultAccountCooldown))
	p.affinityMaxPerAcct.Store(int64(s.SessionAffinityMaxPerAccount))
	p.generations.setLimit(s.MaxConcurrentRequests)

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, account := range p.accounts {
		p.syncSettingsLocked(account.service)
	}
}

// syncSettingsLocked 把池当前的 api_key（脱敏）、模型缓存有效期与额度刷新间隔同步到账号的 Service。
// 调用方须持有 p.mu：新账号在入池的同一临界区内同步，因此不会漏掉创建期间发生的 ApplySettings。
func (p *Pool) syncSettingsLocked(service *Service) {
	service.addSecret(p.apiKey.Load())
	service.modelsCacheTTL.Store(p.modelsCacheTTL.Load())
	service.quotaRefreshInterval.Store(p.quotaRefreshInterval.Load())
}

// positiveOr 在 d 非正时返回 def。
func positiveOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// liveString 是可并发读写的字符串，零值为空串。
type liveString struct{ v atomic.Pointer[string] }

func (s *liveString) Load() string {
	if v := s.v.Load(); v != nil {
		return *v
	}
	return ""
}

func (s *liveString) Store(v string) { s.v.Store(&v) }

// liveDuration 是可并发读写的时长，零值为 0。
type liveDuration struct{ ns atomic.Int64 }

func (d *liveDuration) Load() time.Duration   { return time.Duration(d.ns.Load()) }
func (d *liveDuration) Store(v time.Duration) { d.ns.Store(int64(v)) }

// generationLimiter 限制同时处理的生成请求数，上限可在运行中调整；零值不限制。
// 占用名额不等待，已满时立即失败；调低上限不影响在途请求，它们结束后才释放名额。
type generationLimiter struct {
	mu       sync.Mutex
	inflight int
	limit    int // <=0 表示不限制
}

// setLimit 设置上限，<=0 表示不限制。
func (l *generationLimiter) setLimit(limit int) {
	l.mu.Lock()
	l.limit = limit
	l.mu.Unlock()
}

// limitNow 返回当前上限，用于日志。
func (l *generationLimiter) limitNow() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// acquire 尝试占用一个名额，不等待；未限制时总是成功。
func (l *generationLimiter) acquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit > 0 && l.inflight >= l.limit {
		return false
	}
	l.inflight++
	return true
}

// release 释放 acquire 占用的名额；只能在成功占用后调用。
func (l *generationLimiter) release() {
	l.mu.Lock()
	l.inflight--
	l.mu.Unlock()
}
