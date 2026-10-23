package copilot

import (
	"sync"
	"time"
)

// DefaultSessionAffinityTTL 是会话绑定的默认有效期。
const DefaultSessionAffinityTTL = 2 * time.Hour

// DefaultSessionAffinityMaxPerAccount 是每个账号的有效会话绑定数上限的缺省值（见 PoolConfig.SessionAffinityMaxPerAccount）。
const DefaultSessionAffinityMaxPerAccount = 5

// maxSessionBindings 限制绑定表的条数为 500（内存与绑定文件的规模上限，加载时也按此截断）；
// 表满时先清理过期项，仍满则淘汰最早过期的一条。
const maxSessionBindings = 500

// affinityTable 保存会话键到账号 ID 的绑定。每条绑定都有截止时间，条数有上限。所有方法并发安全。
type affinityTable struct {
	mu       sync.Mutex
	ttl      time.Duration
	bindings map[string]affinityBinding
}

// affinityBinding 是一条会话绑定：绑定的账号 ID 与截止时间（到达即失效）。
type affinityBinding struct {
	accountID string
	expires   time.Time
}

// newAffinityTable 创建绑定表，每条绑定的有效期为 ttl。
func newAffinityTable(ttl time.Duration) *affinityTable {
	return &affinityTable{ttl: ttl, bindings: make(map[string]affinityBinding)}
}

// bind 新建或续期会话绑定，并把账号更新为 accountID，有效期截止于 now+ttl。
// 续期已有键不会淘汰其他绑定；新键在表满时先清理过期项，仍满则淘汰最早过期的一条。
func (t *affinityTable) bind(key, accountID string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.bindings[key]; !exists && len(t.bindings) >= maxSessionBindings {
		t.evictLocked(now)
	}
	t.bindings[key] = affinityBinding{accountID: accountID, expires: now.Add(t.ttl)}
}

// evictLocked 在表满时腾出一个位置：先删除已过期的绑定；仍满则删除最早过期的一条（并列时按键字典序）。
// 调用方须持有 t.mu。
func (t *affinityTable) evictLocked(now time.Time) {
	for key, binding := range t.bindings {
		if !now.Before(binding.expires) {
			delete(t.bindings, key)
		}
	}
	if len(t.bindings) < maxSessionBindings {
		return
	}
	oldest := ""
	for key, binding := range t.bindings {
		if oldest == "" || binding.expires.Before(t.bindings[oldest].expires) ||
			(binding.expires.Equal(t.bindings[oldest].expires) && key < oldest) {
			oldest = key
		}
	}
	delete(t.bindings, oldest)
}

// lookup 返回 key 绑定的账号 ID；截止时刻及之后视为不存在。
func (t *affinityTable) lookup(key string, now time.Time) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	binding, ok := t.bindings[key]
	if !ok || !now.Before(binding.expires) {
		return "", false
	}
	return binding.accountID, true
}

// dropAccount 删除所有指向 accountID 的绑定（包括已过期的），用于账号进入冷却时：冷却期间这些绑定不可用，直接清除。
func (t *affinityTable) dropAccount(accountID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, binding := range t.bindings {
		if binding.accountID == accountID {
			delete(t.bindings, key)
		}
	}
}

// count 返回当前条数，包括尚未清理的过期项。
func (t *affinityTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.bindings)
}

// activeCounts 统计每个账号在 now 时仍有效的绑定数（键为账号 ID），有效规则与 lookup 相同：截止时刻及之后的绑定不计数。
// 没有有效绑定的账号不出现在结果中。结果是调用时刻的快照，供选择时判断每账号绑定数上限（见 Pool.choose）。
func (t *affinityTable) activeCounts(now time.Time) map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	counts := make(map[string]int)
	for _, binding := range t.bindings {
		if now.Before(binding.expires) {
			counts[binding.accountID]++
		}
	}
	return counts
}
