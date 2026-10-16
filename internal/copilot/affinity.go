package copilot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
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

// maxClaudeUserIDBytes 是 metadata.user_id 参与解析的上限，超过则视为没有会话信号。
const maxClaudeUserIDBytes = 4096

var (
	// claudeSessionUUID 是 Claude Code 会话 ID 的标准 UUID 格式。
	claudeSessionUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// legacyClaudeUserID 匹配旧格式 user_<x>_account_<y>_session_<uuid> 的 user_id，并捕获其中的 UUID。
	legacyClaudeUserID = regexp.MustCompile(`^user_[A-Za-z0-9-]+_account_[A-Za-z0-9-]*_session_([0-9a-fA-F-]{36})$`)
)

// sessionKey 计算请求的会话键，用于把同一会话固定到同一账号。键由客户端密钥与会话信号的哈希组成，
// 不保留原始密钥或正文。没有任何会话信号（或请求体不是合法 JSON）时返回空字符串，表示不使用亲和。
func sessionKey(r *http.Request, body []byte, upstreamPath string) string {
	signal := sessionSignal(body, upstreamPath)
	if signal == "" {
		return ""
	}
	raw, _ := json.Marshal([2]string{clientKey(r), signal})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// clientKey 返回客户端密钥：Authorization: Bearer 优先，否则使用 X-Api-Key；两者都没有时为空字符串。
func clientKey(r *http.Request) string {
	if key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && key != "" {
		return key
	}
	return r.Header.Get("X-Api-Key")
}

// sessionSignal 按优先级返回会话信号：Responses 的 prompt_cache_key、Claude Code 会话 UUID，
// 最后是第一条 user 消息的内容。两种带前缀的信号以 \x00 与后续内容区分；Claude 会话 UUID 只含十六进制字符与连字符，
// 不含 \x00，因此三种编码互不相同，不会相互混淆。
func sessionSignal(body []byte, upstreamPath string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return ""
	}
	if upstreamPath == "/responses" {
		var cacheKey string
		if json.Unmarshal(fields["prompt_cache_key"], &cacheKey) == nil && cacheKey != "" {
			return "prompt_cache_key\x00" + cacheKey
		}
	}
	if upstreamPath == "/v1/messages" {
		if id := claudeSessionID(fields["metadata"]); id != "" {
			return id
		}
	}
	return contentSignal(fields, upstreamPath)
}

// claudeSessionID 从 metadata.user_id 中取出 Claude Code 会话 UUID（小写）；无法识别时返回空字符串。
// 支持 JSON 格式 {"session_id":"<uuid>"} 与旧格式 user_<x>_account_<y>_session_<uuid>。
func claudeSessionID(metadata json.RawMessage) string {
	var meta map[string]json.RawMessage
	if json.Unmarshal(metadata, &meta) != nil {
		return ""
	}
	var userID string
	if json.Unmarshal(meta["user_id"], &userID) != nil || len(userID) > maxClaudeUserIDBytes {
		return ""
	}
	var session struct {
		SessionID string `json:"session_id"`
	}
	id := ""
	if json.Unmarshal([]byte(userID), &session) == nil {
		id = session.SessionID
	} else if matches := legacyClaudeUserID.FindStringSubmatch(userID); len(matches) == 2 {
		id = matches[1]
	}
	if !claudeSessionUUID.MatchString(id) {
		return ""
	}
	return strings.ToLower(id)
}

// contentSignal 以第一条 user 消息的文本作为兜底信号，并带上 model 以区分模型；没有 user 消息时返回空字符串。
// 文本先取摘要再参与编码，避免把整段正文保留在内存中。
func contentSignal(fields map[string]json.RawMessage, upstreamPath string) string {
	text, ok := firstUserText(fields, upstreamPath)
	if !ok {
		return ""
	}
	var model string
	_ = json.Unmarshal(fields["model"], &model)
	digest := sha256.Sum256([]byte(text))
	raw, _ := json.Marshal([2]string{model, hex.EncodeToString(digest[:])})
	return "content\x00" + string(raw)
}

// firstUserText 返回第一条 user 消息的文本。Responses 的 input 可以是字符串（即一条 user 消息），
// 也可以是消息数组；其他接口读取 messages 数组。
func firstUserText(fields map[string]json.RawMessage, upstreamPath string) (string, bool) {
	if upstreamPath == "/responses" {
		if raw := fields["input"]; len(raw) > 0 && raw[0] == '"' {
			var text string
			if json.Unmarshal(raw, &text) != nil {
				return "", false
			}
			return text, true
		}
		return firstUserItemText(fields["input"])
	}
	return firstUserItemText(fields["messages"])
}

// firstUserItemText 返回数组中第一条 role 为 user 的对象项的文本（见 contentText）。非对象元素与字段类型错误的消息
// 整项跳过（见 eachMessage），数组无效时返回 false。
func firstUserItemText(raw json.RawMessage) (string, bool) {
	var text string
	found := false
	eachMessage(raw, func(m message) bool {
		if m.Role != "user" {
			return true
		}
		text, found = contentText(m.Content, ""), true
		return false
	})
	return text, found
}

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
