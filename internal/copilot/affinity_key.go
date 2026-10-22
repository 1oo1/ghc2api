package copilot

// 本文件计算会话亲和键。显式会话 ID 的来源、优先级、前缀与规范化参照 CLIProxyAPI 的
// sdk/cliproxy/session/info.go（ExtractSessionInfo）与 identity.go（NormalizeExplicitID），
// 不同步其父子会话继承等行为，差异见 docs/magpie-copilot-sync.md「同步 CLIProxyAPI 会话亲和键」。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

// maxSessionIDBytes 是显式会话 ID 裁剪空白后的长度上限，超过则视为无效。
const maxSessionIDBytes = 256

// legacyClaudeUserID 匹配旧格式 user_<x>_account_<y>_session_<id> 的 user_id，并捕获会话 ID。
var legacyClaudeUserID = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// sessionSource 是一类显式会话 ID 来源：按 names 顺序取第一个有效值，信号为 prefix + ID。
// names 对请求头是头名，对请求体是字段路径（"a.b" 表示 a 对象下的 b 字段）。
type sessionSource struct {
	prefix string
	names  []string
}

// headerSessionSources 是 Claude Code 与 Codex 之后的通用请求头来源，按优先级排列。
var headerSessionSources = []sessionSource{
	{"agy:", []string{"X-Http-Session-Id"}},       // Antigravity CLI
	{"header:", []string{"X-Session-ID"}},         // 通用
	{"affinity:", []string{"X-Session-Affinity"}}, // OpenCode
	{"slot:", []string{"X-Slot-Session-Id"}},      // pi
	{"task:", []string{"X-Task-ID", "X-Task_ID"}},
	{"conv:", []string{"X-Conversation-Id"}},
	{"thread:", []string{"X-Thread-Id"}},
	{"clientreq:", []string{"X-Client-Request-Id"}},
}

// bodySessionSources 是请求体中的来源，排在所有请求头来源之后，按优先级排列。
var bodySessionSources = []sessionSource{
	{"thread:", []string{"thread_id", "threadId", "metadata.thread_id"}},
	{"session:", []string{
		"session_id", "sessionId", "sessionID", "child_session_id", "childSessionId",
		"metadata.session_id", "metadata.sessionId", "metadata.sessionID", "metadata.child_session_id",
		"extra_body.session_id", "extra_body.sessionId", "extra_body.sessionID",
	}},
	{"task:", []string{
		"task_id", "taskId", "taskID", "action_id", "actionId", "actionID",
		"metadata.task_id", "metadata.taskId", "metadata.taskID",
		"metadata.action_id", "metadata.actionId", "metadata.actionID",
		"extra_body.task_id", "extra_body.taskId", "extra_body.taskID",
	}},
	{"pck:", []string{"prompt_cache_key", "promptCacheKey"}},
	{"conv:", []string{"conversation.id", "conversation"}},
	// 未识别出 Claude Code 会话的 metadata.user_id 整串作为普通用户标识。
	{"user:", []string{"metadata.user_id"}},
	{"conv:", []string{
		"conversation_id", "conversationId", "chat_id", "chatId",
		"metadata.conversation_id", "extra_body.conversation_id",
	}},
}

// sessionKey 计算请求的会话键，用于把同一会话固定到同一账号。键由客户端密钥与会话信号的哈希组成，
// 不保留原始密钥或正文。没有任何会话信号时返回空字符串，表示不使用亲和。
func sessionKey(r *http.Request, body []byte, upstreamPath string) string {
	signal := sessionSignal(r.Header, body, upstreamPath)
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

// sessionSignal 按优先级返回会话信号，与上游路径无关的显式会话 ID 优先：
// Claude Code 请求头与 metadata.user_id、Codex 请求头、通用请求头、请求体字段，最后是第一条 user 消息的内容兜底。
// 显式信号形如 "前缀:ID"，ID 不含控制字符；兜底以 "content\x00" 开头，两者互不混淆。
// 请求体不是合法 JSON 对象时跳过请求体来源，请求头信号仍然有效。
func sessionSignal(header http.Header, body []byte, upstreamPath string) string {
	fields := parseRequestFields(body)
	if id := headerSessionID(header, "X-Claude-Code-Session-Id"); id != "" {
		return "claude:" + id
	}
	if id := claudeSessionID(fields.str("metadata.user_id")); id != "" {
		return "claude:" + id
	}
	if id := codexSessionID(header); id != "" {
		return "codex:" + id
	}
	for _, source := range headerSessionSources {
		if id := headerSessionID(header, source.names...); id != "" {
			return source.prefix + id
		}
	}
	for _, source := range bodySessionSources {
		for _, path := range source.names {
			if id := normalizeSessionID(fields.str(path)); id != "" {
				return source.prefix + id
			}
		}
	}
	if fields.top == nil {
		return ""
	}
	return contentSignal(fields.top, upstreamPath)
}

// normalizeSessionID 校验显式会话 ID：含控制字符、裁剪空白后为空或超过 maxSessionIDBytes 时返回空字符串，
// 否则返回裁剪后的值。不要求 UUID 格式，也不做大小写归一。
func normalizeSessionID(raw string) string {
	if strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return ""
	}
	raw = strings.TrimSpace(raw)
	if len(raw) > maxSessionIDBytes {
		return ""
	}
	return raw
}

// headerSessionID 按 names 顺序返回第一个有效的请求头值（头名大小写不敏感，同名多值逐个检查）。
func headerSessionID(header http.Header, names ...string) string {
	for _, name := range names {
		for _, value := range header.Values(name) {
			if id := normalizeSessionID(value); id != "" {
				return id
			}
		}
	}
	return ""
}

// claudeSessionID 从 metadata.user_id 中取出 Claude Code 会话 ID；无法识别时返回空字符串。
// 支持 JSON 格式 {"session_id":"<id>"} 与旧格式 user_<x>_account_<y>_session_<id>。
func claudeSessionID(userID string) string {
	userID = strings.TrimSpace(userID)
	if strings.HasPrefix(userID, "{") {
		var meta struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(userID), &meta) != nil {
			return ""
		}
		return normalizeSessionID(meta.SessionID)
	}
	if matches := legacyClaudeUserID.FindStringSubmatch(userID); len(matches) == 2 {
		return normalizeSessionID(matches[1])
	}
	return ""
}

// codexSessionID 返回 Codex 会话 ID：会话 ID（Session-Id、Session_id、X-Codex-Turn-Metadata 的 session_id）优先，
// 没有时取线程 ID（Thread-Id、Thread_id、turn metadata 的 thread_id）。子代理与主会话共用会话 ID，因此落到同一账号。
func codexSessionID(header http.Header) string {
	var turn map[string]json.RawMessage
	_ = json.Unmarshal([]byte(strings.TrimSpace(header.Get("X-Codex-Turn-Metadata"))), &turn)
	turnID := func(name string) string {
		var id string
		_ = json.Unmarshal(turn[name], &id)
		return normalizeSessionID(id)
	}
	if id := headerSessionID(header, "Session-Id", "Session_id"); id != "" {
		return id
	}
	if id := turnID("session_id"); id != "" {
		return id
	}
	if id := headerSessionID(header, "Thread-Id", "Thread_id"); id != "" {
		return id
	}
	return turnID("thread_id")
}

// requestFields 是请求体顶层字段，按需解析并缓存嵌套对象，避免同一对象被重复解析。
// 请求体不是合法 JSON 对象时 top 为 nil，所有查询返回空字符串。
type requestFields struct {
	top    map[string]json.RawMessage
	nested map[string]map[string]json.RawMessage
}

// parseRequestFields 解析请求体顶层字段。
func parseRequestFields(body []byte) *requestFields {
	fields := &requestFields{nested: make(map[string]map[string]json.RawMessage)}
	if json.Unmarshal(body, &fields.top) != nil {
		fields.top = nil
	}
	return fields
}

// raw 返回 path（"a" 或 "a.b"）处的原始 JSON；不存在或上层不是对象时返回 nil。
func (f *requestFields) raw(path string) json.RawMessage {
	key, sub, nested := strings.Cut(path, ".")
	if !nested {
		return f.top[key]
	}
	object, ok := f.nested[key]
	if !ok {
		_ = json.Unmarshal(f.top[key], &object)
		f.nested[key] = object
	}
	return object[sub]
}

// str 返回 path 处的字符串值；字段不存在或不是 JSON 字符串时返回空字符串。
func (f *requestFields) str(path string) string {
	var value string
	if json.Unmarshal(f.raw(path), &value) != nil {
		return ""
	}
	return value
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
