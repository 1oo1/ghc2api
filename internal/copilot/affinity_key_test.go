package copilot

// 本文件覆盖会话键中显式会话 ID 的提取优先级（与 CLIProxyAPI 同步）：各请求头与请求体来源、
// 同前缀跨来源同键、不同前缀互异、相邻优先级、ID 规范化、Codex 的 sid/tid 与 Claude 的多种表示，
// 以及显式信号与路径无关、非法 JSON 时请求头信号仍有效、内容兜底与显式信号互不混淆。
// 全部通过 sessionKey 黑盒验证，不依赖实现内部的函数名。

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// affinityKeyReq 描述一个用于计算会话键的请求：请求头与 body 顶层字段。
type affinityKeyReq struct {
	headers map[string]string
	fields  map[string]any
}

// affinityKeyOf 计算指定请求头与 body 字段下的会话键。fields 与固定的 model、首条 user 消息合并，
// 使无显式信号时的内容兜底保持稳定；同名顶层字段以 fields 为准。
func affinityKeyOf(t *testing.T, upstreamPath string, headers map[string]string, fields map[string]any) string {
	t.Helper()
	merged := map[string]any{
		"model":    "gpt-4.1",
		"messages": []any{map[string]string{"role": "user", "content": "first question"}},
	}
	for name, value := range fields {
		merged[name] = value
	}
	r := affinityRequest(upstreamPath, "client-a", "")
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	return sessionKey(r, affinityBody(t, merged), upstreamPath)
}

// affinityKeyRaw 用原始 body 文本计算会话键，供非法 JSON 用例使用。
func affinityKeyRaw(t *testing.T, upstreamPath string, headers map[string]string, body string) string {
	t.Helper()
	r := affinityRequest(upstreamPath, "client-a", "")
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	return sessionKey(r, []byte(body), upstreamPath)
}

// affinityFallbackKey 返回无任何显式信号时的内容兜底键，供断言「信号视为不存在」时对比。
func affinityFallbackKey(t *testing.T, upstreamPath string) string {
	t.Helper()
	return affinityKeyOf(t, upstreamPath, nil, nil)
}

// affinityMergeReq 合并多个请求描述：同名请求头或顶层字段后者覆盖前者（用例中高低优先级的字段不重名）。
func affinityMergeReq(reqs ...affinityKeyReq) affinityKeyReq {
	out := affinityKeyReq{headers: map[string]string{}, fields: map[string]any{}}
	for _, q := range reqs {
		for name, value := range q.headers {
			out.headers[name] = value
		}
		for name, value := range q.fields {
			out.fields[name] = value
		}
	}
	return out
}

// affinityNested 构造单个嵌套字段的顶层 map，如 path 为 ["metadata", "thread_id"] 时返回 {"metadata":{"thread_id":value}}。
func affinityNested(value string, path ...string) map[string]any {
	leaf := map[string]any{path[len(path)-1]: value}
	for i := len(path) - 2; i >= 0; i-- {
		leaf = map[string]any{path[i]: leaf}
	}
	return leaf
}

// affinityFieldSource 是一个请求体字段来源：name 为字段路径，build 给定 ID 生成顶层字段。
type affinityFieldSource struct {
	name  string
	build func(id string) map[string]any
}

// affinityFields 返回按嵌套路径构造字段的 builder，如 ["metadata","thread_id"]。
func affinityFields(path ...string) func(string) map[string]any {
	return func(id string) map[string]any { return affinityNested(id, path...) }
}

// TestSessionKeyHeaderSources 各请求头来源都能产生非空且稳定的会话键，且都不同于内容兜底（证明来源确实被读取）。
func TestSessionKeyHeaderSources(t *testing.T) {
	fallback := affinityFallbackKey(t, "/chat/completions")
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"claude-code", map[string]string{"X-Claude-Code-Session-Id": "id-1"}},
		{"codex-session-id", map[string]string{"Session-Id": "id-1"}},
		{"codex-session-id-underscore", map[string]string{"Session_id": "id-1"}},
		{"codex-turn-metadata", map[string]string{"X-Codex-Turn-Metadata": `{"session_id":"id-1"}`}},
		{"codex-thread-id", map[string]string{"Thread-Id": "id-1"}},
		{"codex-thread-id-underscore", map[string]string{"Thread_id": "id-1"}},
		{"agy", map[string]string{"X-Http-Session-Id": "id-1"}},
		{"session-id", map[string]string{"X-Session-ID": "id-1"}},
		{"opencode", map[string]string{"X-Session-Affinity": "id-1"}},
		{"pi-slot", map[string]string{"X-Slot-Session-Id": "id-1"}},
		{"task", map[string]string{"X-Task-ID": "id-1"}},
		{"task-underscore", map[string]string{"X-Task_ID": "id-1"}},
		{"conversation", map[string]string{"X-Conversation-Id": "id-1"}},
		{"thread", map[string]string{"X-Thread-Id": "id-1"}},
		{"client-request", map[string]string{"X-Client-Request-Id": "id-1"}},
		{"agent-task", map[string]string{"X-Agent-Task-Id": "id-1"}},
		{"request-id", map[string]string{"X-Request-Id": "id-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := affinityKeyOf(t, "/chat/completions", tc.headers, nil)
			affinityAssertKeyShape(t, key)
			if key == fallback {
				t.Fatal("请求头来源没有产生显式信号（与内容兜底同键）")
			}
			if got := affinityKeyOf(t, "/chat/completions", tc.headers, nil); got != key {
				t.Fatalf("同一请求头的会话键不稳定: %s != %s", got, key)
			}
		})
	}
}

// TestSessionKeyBodySources 各请求体字段来源都能产生非空且稳定的会话键；
// 同前缀的字段（含嵌套在 metadata / extra_body 下的别名）得到相同的键，改变 ID 得到不同的键。
func TestSessionKeyBodySources(t *testing.T) {
	const path = "/chat/completions"
	groups := []struct {
		prefix  string // 信号前缀；同组内所有来源的键必须相同
		sources []affinityFieldSource
	}{
		{"thread", []affinityFieldSource{
			{"thread_id", affinityFields("thread_id")},
			{"threadId", affinityFields("threadId")},
			{"metadata.thread_id", affinityFields("metadata", "thread_id")},
		}},
		{"session", []affinityFieldSource{
			{"session_id", affinityFields("session_id")},
			{"sessionId", affinityFields("sessionId")},
			{"sessionID", affinityFields("sessionID")},
			{"child_session_id", affinityFields("child_session_id")},
			{"childSessionId", affinityFields("childSessionId")},
			{"metadata.session_id", affinityFields("metadata", "session_id")},
			{"metadata.sessionId", affinityFields("metadata", "sessionId")},
			{"metadata.sessionID", affinityFields("metadata", "sessionID")},
			{"metadata.child_session_id", affinityFields("metadata", "child_session_id")},
			{"extra_body.session_id", affinityFields("extra_body", "session_id")},
			{"extra_body.sessionId", affinityFields("extra_body", "sessionId")},
			{"extra_body.sessionID", affinityFields("extra_body", "sessionID")},
		}},
		{"task", []affinityFieldSource{
			{"task_id", affinityFields("task_id")},
			{"taskId", affinityFields("taskId")},
			{"taskID", affinityFields("taskID")},
			{"action_id", affinityFields("action_id")},
			{"actionId", affinityFields("actionId")},
			{"actionID", affinityFields("actionID")},
			{"metadata.task_id", affinityFields("metadata", "task_id")},
			{"metadata.taskId", affinityFields("metadata", "taskId")},
			{"metadata.taskID", affinityFields("metadata", "taskID")},
			{"metadata.action_id", affinityFields("metadata", "action_id")},
			{"metadata.actionId", affinityFields("metadata", "actionId")},
			{"metadata.actionID", affinityFields("metadata", "actionID")},
			{"extra_body.task_id", affinityFields("extra_body", "task_id")},
			{"extra_body.taskId", affinityFields("extra_body", "taskId")},
			{"extra_body.taskID", affinityFields("extra_body", "taskID")},
		}},
		{"pck", []affinityFieldSource{
			{"prompt_cache_key", affinityFields("prompt_cache_key")},
			{"promptCacheKey", affinityFields("promptCacheKey")},
		}},
		{"conv", []affinityFieldSource{
			{"conversation.id", affinityFields("conversation", "id")},
			{"conversation", func(id string) map[string]any { return map[string]any{"conversation": id} }},
			{"conversation_id", affinityFields("conversation_id")},
			{"conversationId", affinityFields("conversationId")},
			{"chat_id", affinityFields("chat_id")},
			{"chatId", affinityFields("chatId")},
			{"metadata.conversation_id", affinityFields("metadata", "conversation_id")},
			{"extra_body.conversation_id", affinityFields("extra_body", "conversation_id")},
		}},
		{"user", []affinityFieldSource{
			{"metadata.user_id", affinityFields("metadata", "user_id")},
		}},
	}
	for _, g := range groups {
		t.Run(g.prefix, func(t *testing.T) {
			fallback := affinityFallbackKey(t, path)
			want := ""
			for _, s := range g.sources {
				key := affinityKeyOf(t, path, nil, s.build("id-1"))
				affinityAssertKeyShape(t, key)
				if key == fallback {
					t.Fatalf("%s 没有产生显式信号（与内容兜底同键）", s.name)
				}
				if want == "" {
					want = key
				} else if key != want {
					t.Fatalf("%s 与同前缀来源的键不同: %s != %s", s.name, key, want)
				}
				if got := affinityKeyOf(t, path, nil, s.build("id-2")); got == key {
					t.Fatalf("%s 改变 ID 后会话键未变化", s.name)
				}
			}
		})
	}
}

// TestSessionKeySamePrefixAcrossSources 前缀相同、ID 相同的不同来源（请求头与请求体、别名字段）得到相同的会话键。
// ID 取小写十六进制与连字符，使 Claude 旧格式正则也能识别。
func TestSessionKeySamePrefixAcrossSources(t *testing.T) {
	const path = "/chat/completions"
	const id = "aa11-bb22"
	for _, g := range []struct {
		name string
		reqs []affinityKeyReq
	}{
		{"claude", []affinityKeyReq{
			{headers: map[string]string{"X-Claude-Code-Session-Id": id}},
			{fields: map[string]any{"metadata": map[string]any{"user_id": `{"session_id":"` + id + `"}`}}},
			{fields: map[string]any{"metadata": map[string]any{"user_id": "user_synthetic_account_acct_session_" + id}}},
		}},
		{"codex", []affinityKeyReq{
			{headers: map[string]string{"Session-Id": id}},
			{headers: map[string]string{"Session_id": id}},
			{headers: map[string]string{"X-Codex-Turn-Metadata": `{"session_id":"` + id + `"}`}},
		}},
		{"task", []affinityKeyReq{
			{headers: map[string]string{"X-Task-ID": id}},
			{headers: map[string]string{"X-Task_ID": id}},
			{fields: map[string]any{"task_id": id}},
			{fields: map[string]any{"actionId": id}},
			{fields: map[string]any{"extra_body": map[string]any{"taskID": id}}},
		}},
		{"thread", []affinityKeyReq{
			{headers: map[string]string{"X-Thread-Id": id}},
			{fields: map[string]any{"thread_id": id}},
			{fields: map[string]any{"threadId": id}},
		}},
		{"conv", []affinityKeyReq{
			{headers: map[string]string{"X-Conversation-Id": id}},
			{fields: map[string]any{"conversation": id}},
			{fields: map[string]any{"conversation": map[string]any{"id": id}}},
			{fields: map[string]any{"conversationId": id}},
			{fields: map[string]any{"chat_id": id}},
		}},
	} {
		t.Run(g.name, func(t *testing.T) {
			want := affinityKeyOf(t, path, g.reqs[0].headers, g.reqs[0].fields)
			affinityAssertKeyShape(t, want)
			if want == affinityFallbackKey(t, path) {
				t.Fatal("同前缀来源没有产生显式信号（与内容兜底同键）")
			}
			for _, req := range g.reqs[1:] {
				if got := affinityKeyOf(t, path, req.headers, req.fields); got != want {
					t.Fatalf("同前缀同 ID 的来源键不同: %s != %s", got, want)
				}
			}
		})
	}
}

// TestSessionKeyDistinctPrefixes 前缀不同的信号即使 ID 相同，会话键也两两不同。
// 每个前缀只列一个来源；同前缀的跨来源一致性见 TestSessionKeySamePrefixAcrossSources。
func TestSessionKeyDistinctPrefixes(t *testing.T) {
	const path = "/chat/completions"
	const id = "same-id"
	sources := []struct {
		name string
		req  affinityKeyReq
	}{
		{"claude", affinityKeyReq{headers: map[string]string{"X-Claude-Code-Session-Id": id}}},
		{"codex", affinityKeyReq{headers: map[string]string{"Session-Id": id}}},
		{"agy", affinityKeyReq{headers: map[string]string{"X-Http-Session-Id": id}}},
		{"header", affinityKeyReq{headers: map[string]string{"X-Session-ID": id}}},
		{"affinity", affinityKeyReq{headers: map[string]string{"X-Session-Affinity": id}}},
		{"slot", affinityKeyReq{headers: map[string]string{"X-Slot-Session-Id": id}}},
		{"task", affinityKeyReq{headers: map[string]string{"X-Task-ID": id}}},
		{"conv", affinityKeyReq{headers: map[string]string{"X-Conversation-Id": id}}},
		{"thread", affinityKeyReq{headers: map[string]string{"X-Thread-Id": id}}},
		{"clientreq", affinityKeyReq{headers: map[string]string{"X-Client-Request-Id": id}}},
		{"agenttask", affinityKeyReq{headers: map[string]string{"X-Agent-Task-Id": id}}},
		{"req", affinityKeyReq{headers: map[string]string{"X-Request-Id": id}}},
		{"session", affinityKeyReq{fields: map[string]any{"session_id": id}}},
		{"pck", affinityKeyReq{fields: map[string]any{"prompt_cache_key": id}}},
		{"user", affinityKeyReq{fields: map[string]any{"metadata": map[string]any{"user_id": id}}}},
	}
	seen := map[string]string{}
	for _, s := range sources {
		key := affinityKeyOf(t, path, s.req.headers, s.req.fields)
		affinityAssertKeyShape(t, key)
		if prev, ok := seen[key]; ok {
			t.Fatalf("%s 与 %s 前缀不同却共享会话键 %s", s.name, prev, key)
		}
		seen[key] = s.name
	}
}

// TestSessionKeySignalPriority 相邻优先级：高优先级信号存在时，低优先级信号取值不同不影响会话键。
// 行按优先级从高到低排列，每行验证一对相邻层级。
func TestSessionKeySignalPriority(t *testing.T) {
	const path = "/chat/completions"
	for _, tc := range []struct {
		name     string
		high     affinityKeyReq
		low, alt affinityKeyReq // 同一低优先级来源的两个不同取值
	}{
		{"claude-header-优于-claude-metadata",
			affinityKeyReq{headers: map[string]string{"X-Claude-Code-Session-Id": "hi"}},
			affinityKeyReq{fields: map[string]any{"metadata": map[string]any{"user_id": `{"session_id":"lo-a"}`}}},
			affinityKeyReq{fields: map[string]any{"metadata": map[string]any{"user_id": `{"session_id":"lo-b"}`}}}},
		{"claude-metadata-优于-codex",
			affinityKeyReq{fields: map[string]any{"metadata": map[string]any{"user_id": `{"session_id":"hi"}`}}},
			affinityKeyReq{headers: map[string]string{"Session-Id": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"Session-Id": "lo-b"}}},
		{"codex-优于-agy",
			affinityKeyReq{headers: map[string]string{"Session-Id": "hi"}},
			affinityKeyReq{headers: map[string]string{"X-Http-Session-Id": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"X-Http-Session-Id": "lo-b"}}},
		{"agy-优于-session-id",
			affinityKeyReq{headers: map[string]string{"X-Http-Session-Id": "hi"}},
			affinityKeyReq{headers: map[string]string{"X-Session-ID": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"X-Session-ID": "lo-b"}}},
		{"session-id-优于-affinity",
			affinityKeyReq{headers: map[string]string{"X-Session-ID": "hi"}},
			affinityKeyReq{headers: map[string]string{"X-Session-Affinity": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"X-Session-Affinity": "lo-b"}}},
		{"affinity-优于-slot",
			affinityKeyReq{headers: map[string]string{"X-Session-Affinity": "hi"}},
			affinityKeyReq{headers: map[string]string{"X-Slot-Session-Id": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"X-Slot-Session-Id": "lo-b"}}},
		{"slot-优于-task-header",
			affinityKeyReq{headers: map[string]string{"X-Slot-Session-Id": "hi"}},
			affinityKeyReq{headers: map[string]string{"X-Task-ID": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"X-Task-ID": "lo-b"}}},
		{"task-header-优于-conv-header",
			affinityKeyReq{headers: map[string]string{"X-Task-ID": "hi"}},
			affinityKeyReq{headers: map[string]string{"X-Conversation-Id": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"X-Conversation-Id": "lo-b"}}},
		{"conv-header-优于-thread-header",
			affinityKeyReq{headers: map[string]string{"X-Conversation-Id": "hi"}},
			affinityKeyReq{headers: map[string]string{"X-Thread-Id": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"X-Thread-Id": "lo-b"}}},
		{"thread-header-优于-clientreq",
			affinityKeyReq{headers: map[string]string{"X-Thread-Id": "hi"}},
			affinityKeyReq{headers: map[string]string{"X-Client-Request-Id": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"X-Client-Request-Id": "lo-b"}}},
		{"clientreq-优于-agenttask",
			affinityKeyReq{headers: map[string]string{"X-Client-Request-Id": "hi"}},
			affinityKeyReq{headers: map[string]string{"X-Agent-Task-Id": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"X-Agent-Task-Id": "lo-b"}}},
		{"agenttask-优于-req",
			affinityKeyReq{headers: map[string]string{"X-Agent-Task-Id": "hi"}},
			affinityKeyReq{headers: map[string]string{"X-Request-Id": "lo-a"}},
			affinityKeyReq{headers: map[string]string{"X-Request-Id": "lo-b"}}},
		{"req-优于-body-thread",
			affinityKeyReq{headers: map[string]string{"X-Request-Id": "hi"}},
			affinityKeyReq{fields: map[string]any{"thread_id": "lo-a"}},
			affinityKeyReq{fields: map[string]any{"threadId": "lo-b"}}},
		{"body-thread-优于-body-session",
			affinityKeyReq{fields: map[string]any{"thread_id": "hi"}},
			affinityKeyReq{fields: map[string]any{"session_id": "lo-a"}},
			affinityKeyReq{fields: map[string]any{"child_session_id": "lo-b"}}},
		{"body-session-优于-body-task",
			affinityKeyReq{fields: map[string]any{"session_id": "hi"}},
			affinityKeyReq{fields: map[string]any{"task_id": "lo-a"}},
			affinityKeyReq{fields: map[string]any{"action_id": "lo-b"}}},
		{"body-task-优于-prompt-cache-key",
			affinityKeyReq{fields: map[string]any{"task_id": "hi"}},
			affinityKeyReq{fields: map[string]any{"prompt_cache_key": "lo-a"}},
			affinityKeyReq{fields: map[string]any{"promptCacheKey": "lo-b"}}},
		{"prompt-cache-key-优于-conversation",
			affinityKeyReq{fields: map[string]any{"prompt_cache_key": "hi"}},
			affinityKeyReq{fields: map[string]any{"conversation": "lo-a"}},
			affinityKeyReq{fields: map[string]any{"conversation": map[string]any{"id": "lo-b"}}}},
		{"conversation-优于-user-id",
			affinityKeyReq{fields: map[string]any{"conversation": "hi"}},
			affinityKeyReq{fields: map[string]any{"metadata": map[string]any{"user_id": "lo-a"}}},
			affinityKeyReq{fields: map[string]any{"metadata": map[string]any{"user_id": "lo-b"}}}},
		{"user-id-优于-conversation-id",
			affinityKeyReq{fields: map[string]any{"metadata": map[string]any{"user_id": "hi"}}},
			affinityKeyReq{fields: map[string]any{"conversation_id": "lo-a"}},
			affinityKeyReq{fields: map[string]any{"chat_id": "lo-b"}}},
		{"conversation-id-优于-内容兜底",
			affinityKeyReq{fields: map[string]any{"conversation_id": "hi"}},
			affinityKeyReq{fields: map[string]any{"messages": []any{map[string]string{"role": "user", "content": "lo-a"}}}},
			affinityKeyReq{fields: map[string]any{"messages": []any{map[string]string{"role": "user", "content": "lo-b"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := affinityKeyOf(t, path, tc.high.headers, tc.high.fields)
			affinityAssertKeyShape(t, want)
			for _, low := range []affinityKeyReq{tc.low, tc.alt} {
				merged := affinityMergeReq(tc.high, low)
				if got := affinityKeyOf(t, path, merged.headers, merged.fields); got != want {
					t.Fatalf("高优先级信号存在时低优先级取值影响了会话键: %s != %s", got, want)
				}
			}
		})
	}
}

// TestSessionKeyIDNormalization 显式会话 ID 的规范化：含控制字符或仅空白无效、256 字节有效而 257 字节无效、
// 前后空白裁剪后与无空白相同、不做大小写归一；请求体非字符串字段视为不存在。无效等同于不存在，回退到内容兜底。
func TestSessionKeyIDNormalization(t *testing.T) {
	const path = "/chat/completions"
	fallback := affinityFallbackKey(t, path)
	affinityAssertKeyShape(t, fallback)
	for _, tc := range []struct{ name, value string }{
		{"制表符", "a\tb"},
		{"换行", "a\nb"},
		{"NUL", "a\x00b"},
		{"其他控制字符", "a\x01b"},
		{"仅空白", "   "},
		{"空", ""},
		{"257字节", strings.Repeat("a", 257)},
	} {
		t.Run("header-"+tc.name, func(t *testing.T) {
			if got := affinityKeyOf(t, path, map[string]string{"X-Session-ID": tc.value}, nil); got != fallback {
				t.Fatalf("请求头 ID %q 无效时应视为不存在，实际 = %q", tc.value, got)
			}
		})
		t.Run("body-"+tc.name, func(t *testing.T) {
			if got := affinityKeyOf(t, path, nil, map[string]any{"session_id": tc.value}); got != fallback {
				t.Fatalf("body ID %q 无效时应视为不存在，实际 = %q", tc.value, got)
			}
		})
	}

	long := strings.Repeat("a", 256)
	key256 := affinityKeyOf(t, path, map[string]string{"X-Session-ID": long}, nil)
	affinityAssertKeyShape(t, key256)
	if key256 == fallback {
		t.Fatal("256 字节的 ID 应有效")
	}

	// 前后空白裁剪后与无空白相同（请求头与请求体分别对比自身来源）。
	if got, want := affinityKeyOf(t, path, map[string]string{"X-Session-ID": "  s1  "}, nil),
		affinityKeyOf(t, path, map[string]string{"X-Session-ID": "s1"}, nil); got != want {
		t.Fatalf("请求头 ID 裁剪空白后应与无空白相同: %s != %s", got, want)
	}
	if got, want := affinityKeyOf(t, path, nil, map[string]any{"session_id": "  s1  "}),
		affinityKeyOf(t, path, nil, map[string]any{"session_id": "s1"}); got != want {
		t.Fatalf("body ID 裁剪空白后应与无空白相同: %s != %s", got, want)
	}

	// 不做大小写归一：ABC 与 abc 是不同会话。
	if got, want := affinityKeyOf(t, path, map[string]string{"X-Session-ID": "ABC"}, nil),
		affinityKeyOf(t, path, map[string]string{"X-Session-ID": "abc"}, nil); got == want {
		t.Fatal("ID 大小写不同应得到不同的会话键")
	}

	// 请求体字段只接受 JSON 字符串，非字符串视为不存在。
	for _, value := range []any{42, 3.5, true, map[string]any{"id": "x"}, nil, []any{"x"}} {
		if got := affinityKeyOf(t, path, nil, map[string]any{"session_id": value}); got != fallback {
			t.Fatalf("session_id=%#v 应视为不存在，回退到内容兜底", value)
		}
	}
	if got := affinityKeyOf(t, path, nil, map[string]any{"metadata": map[string]any{"user_id": 42}}); got != fallback {
		t.Fatalf("非字符串 metadata.user_id 应视为不存在，实际 = %q", got)
	}
	if got := affinityKeyOf(t, path, nil, map[string]any{"conversation": map[string]any{"id": 42}}); got != fallback {
		t.Fatalf("conversation.id 非字符串应视为不存在，实际 = %q", got)
	}
}

// TestSessionKeyHeaderNameCaseInsensitive 请求头名字大小写不敏感（依赖 Go 的 Header 规范化）。
func TestSessionKeyHeaderNameCaseInsensitive(t *testing.T) {
	const path = "/chat/completions"
	for _, tc := range []struct {
		canonical string
		variants  []string
	}{
		{"X-Session-ID", []string{"x-session-id", "X-SESSION-ID", "X-Session-Id"}},
		{"X-Agent-Task-Id", []string{"x-agent-task-id", "X-AGENT-TASK-ID", "X-Agent-Task-ID"}},
		{"X-Request-Id", []string{"x-request-id", "X-REQUEST-ID", "X-Request-ID"}},
	} {
		t.Run(tc.canonical, func(t *testing.T) {
			want := affinityKeyOf(t, path, map[string]string{tc.canonical: "s1"}, nil)
			affinityAssertKeyShape(t, want)
			if want == affinityFallbackKey(t, path) {
				t.Fatal("请求头来源没有产生显式信号（与内容兜底同键）")
			}
			for _, name := range tc.variants {
				if got := affinityKeyOf(t, path, map[string]string{name: "s1"}, nil); got != want {
					t.Fatalf("请求头 %s 的会话键与 %s 不同: %s != %s", name, tc.canonical, got, want)
				}
			}
		})
	}
}

// TestSessionKeyInvalidHeaderValueFallsThroughToNextSource 请求头值无效（含控制字符或过长）时视为不存在，
// 落到优先级更低的下一来源：clientreq 无效落到 agenttask，agenttask 无效落到 req，req 无效落到请求体来源。
func TestSessionKeyInvalidHeaderValueFallsThroughToNextSource(t *testing.T) {
	const path = "/chat/completions"
	agenttask := affinityKeyOf(t, path, map[string]string{"X-Agent-Task-Id": "id-agent"}, nil)
	req := affinityKeyOf(t, path, map[string]string{"X-Request-Id": "id-req"}, nil)
	body := affinityKeyOf(t, path, nil, map[string]any{"session_id": "id-body"})
	for name, key := range map[string]string{"agenttask": agenttask, "req": req, "body": body} {
		affinityAssertKeyShape(t, key)
		if key == affinityFallbackKey(t, path) {
			t.Fatalf("%s 来源没有产生显式信号（与内容兜底同键）", name)
		}
	}
	for _, invalid := range []string{"bad\x01id", strings.Repeat("a", 257)} {
		if got := affinityKeyOf(t, path, map[string]string{
			"X-Client-Request-Id": invalid, "X-Agent-Task-Id": "id-agent",
		}, nil); got != agenttask {
			t.Fatalf("clientreq 值 %q 无效时应落到 agenttask: %s != %s", invalid, got, agenttask)
		}
		if got := affinityKeyOf(t, path, map[string]string{
			"X-Agent-Task-Id": invalid, "X-Request-Id": "id-req",
		}, nil); got != req {
			t.Fatalf("agenttask 值 %q 无效时应落到 req: %s != %s", invalid, got, req)
		}
		if got := affinityKeyOf(t, path, map[string]string{"X-Request-Id": invalid},
			map[string]any{"session_id": "id-body"}); got != body {
			t.Fatalf("req 值 %q 无效时应落到请求体 session_id: %s != %s", invalid, got, body)
		}
	}
}

// TestSessionKeyHeaderMultiValue 同名多值请求头取第一个有效值：跳过无效值，且不越过后值。
func TestSessionKeyHeaderMultiValue(t *testing.T) {
	const path = "/chat/completions"
	keyWith := func(values ...string) string {
		t.Helper()
		r := affinityRequest(path, "client-a", "")
		for _, value := range values {
			r.Header.Add("X-Session-ID", value)
		}
		body := affinityBody(t, map[string]any{
			"model":    "gpt-4.1",
			"messages": []any{map[string]string{"role": "user", "content": "first question"}},
		})
		return sessionKey(r, body, path)
	}
	first := keyWith("good-a", "good-b")
	affinityAssertKeyShape(t, first)
	if first == affinityFallbackKey(t, path) {
		t.Fatal("请求头来源没有产生显式信号（与内容兜底同键）")
	}
	if got := keyWith("good-a"); got != first {
		t.Fatalf("多值时应取第一个值: %s != %s", got, first)
	}
	// 第一个值无效时取下一个有效值。
	if got := keyWith("bad\x01", "good-b"); got != keyWith("good-b") {
		t.Fatal("第一个值无效时应取下一个有效值")
	}
	// 后值无效不影响第一个有效值。
	if got := keyWith("good-a", "bad\x01"); got != first {
		t.Fatal("后值无效不应影响第一个有效值")
	}
}

// TestSessionKeyCodexSignals Codex 信号：sid 依次取请求头 Session-Id、Session_id、X-Codex-Turn-Metadata 的 session_id；
// sid 缺失时 tid 依次取 Thread-Id、Thread_id、turn metadata 的 thread_id，取 sid（优先）或 tid。
// 同一 Session-Id 下 Thread-Id 不同（子代理）仍得到同一个键；X-Thread-Id 是 thread: 前缀，与 Codex 的 Thread-Id 不同。
func TestSessionKeyCodexSignals(t *testing.T) {
	const path = "/chat/completions"
	key := func(headers map[string]string) string {
		return affinityKeyOf(t, path, headers, nil)
	}
	want := key(map[string]string{"Session-Id": "s1"})
	affinityAssertKeyShape(t, want)
	if want == affinityFallbackKey(t, path) {
		t.Fatal("Codex sid 没有产生显式信号（与内容兜底同键）")
	}
	for _, headers := range []map[string]string{
		{"Session_id": "s1"},
		{"X-Codex-Turn-Metadata": `{"session_id":"s1"}`},
		{"X-Codex-Turn-Metadata": `{"session_id":"s1","thread_id":"t1"}`},
	} {
		if got := key(headers); got != want {
			t.Fatalf("同一 sid 的来源键不同: %v -> %s != %s", headers, got, want)
		}
	}
	// 前面的 sid 来源无效时落到下一个来源。
	if got := key(map[string]string{"Session-Id": "bad\x01", "Session_id": "s1"}); got != want {
		t.Fatal("Session-Id 无效时应落到 Session_id")
	}
	if got := key(map[string]string{"Session-Id": "bad\x01", "Session_id": "bad\x02", "X-Codex-Turn-Metadata": `{"session_id":"s1"}`}); got != want {
		t.Fatal("Session-Id 与 Session_id 无效时应落到 turn metadata 的 session_id")
	}

	// 无 sid 时 tid 作为会话信号，各来源同键。
	wantTid := key(map[string]string{"Thread-Id": "t1"})
	affinityAssertKeyShape(t, wantTid)
	if wantTid == want || wantTid == affinityFallbackKey(t, path) {
		t.Fatal("codex 的 tid 是独立于 sid 与内容兜底的显式信号")
	}
	for _, headers := range []map[string]string{
		{"Thread_id": "t1"},
		{"X-Codex-Turn-Metadata": `{"thread_id":"t1"}`},
	} {
		if got := key(headers); got != wantTid {
			t.Fatalf("同一 tid 的来源键不同: %v -> %s != %s", headers, got, wantTid)
		}
	}

	// sid 优先于 tid：同一 sid 下 tid 不同（子代理）仍同键。
	for _, tid := range []string{"t8", "t9"} {
		if got := key(map[string]string{"Session-Id": "s1", "Thread-Id": tid}); got != want {
			t.Fatalf("sid 存在时 tid=%s 影响了会话键", tid)
		}
	}
	// X-Thread-Id 与 Codex 的 Thread-Id 前缀不同，键不同。
	if got := key(map[string]string{"X-Thread-Id": "t1"}); got == wantTid {
		t.Fatal("X-Thread-Id 与 Codex Thread-Id 不应同键")
	}
}

// TestSessionKeyClaudeSources Claude Code 会话的多种表示：请求头与 metadata.user_id 同键、旧格式与 JSON 同键；
// session_id 不要求 UUID 格式也不做大小写归一；JSON user_id 无 session_id 时落到 user: 前缀（整串）。
func TestSessionKeyClaudeSources(t *testing.T) {
	const path = "/v1/messages"
	claude := func(userID string) string {
		return affinityKeyOf(t, path, nil, map[string]any{"metadata": map[string]any{"user_id": userID}})
	}
	want := claude(`{"session_id":"abc-1"}`)
	affinityAssertKeyShape(t, want)
	if want == affinityFallbackKey(t, path) {
		t.Fatal("metadata.user_id 中的 Claude 会话没有产生显式信号（与内容兜底同键）")
	}
	if got := affinityKeyOf(t, path, map[string]string{"X-Claude-Code-Session-Id": "abc-1"}, nil); got != want {
		t.Fatal("请求头与 metadata.user_id 中的同一会话应同键")
	}
	if got := claude("user_synthetic_account_acct_session_abc-1"); got != want {
		t.Fatal("旧格式与 JSON 格式表示同一会话，键必须相同")
	}
	// 非 UUID 的 session_id 也是有效会话；大小写不同是不同会话。
	other := claude(`{"session_id":"not-a-uuid"}`)
	affinityAssertKeyShape(t, other)
	if other == want {
		t.Fatal("不同 session_id 得到了相同的会话键")
	}
	if got := claude(`{"session_id":"ABC-1"}`); got == want {
		t.Fatal("session_id 大小写不同应得到不同的会话键")
	}

	// JSON user_id 无 session_id 时落到 user: 前缀：ID 为整串，裁剪空白后一致，且不同于内容兜底与 claude 会话。
	userKey := claude(`{"device_id":"d"}`)
	affinityAssertKeyShape(t, userKey)
	if userKey == want || userKey == other {
		t.Fatal("JSON user_id 无 session_id 时不应得到 claude 会话键")
	}
	if fallback := affinityFallbackKey(t, path); userKey == fallback {
		t.Fatal("JSON user_id 无 session_id 时是显式 user: 信号，不应等于内容兜底")
	}
	if got := claude("  {\"device_id\":\"d\"}  "); got != userKey {
		t.Fatal("user_id 整串应先 TrimSpace 再参与编码")
	}
	// 空或无效的 session_id 同样落到 user: 整串，是显式信号而不是内容兜底。
	// 第三个 user_id 用 JSON 转义 0x01，解析出的 session_id 含控制字符而无效。
	for _, userID := range []string{
		`{"session_id":""}`,
		`{"session_id":"   "}`,
		`{"session_id":"a` + `\u0001b"}`,
	} {
		if got := claude(userID); got == "" || got == affinityFallbackKey(t, path) {
			t.Fatalf("user_id=%s 应落到 user: 显式信号，实际 = %q", userID, got)
		}
	}
}

// TestSessionKeySignalIndependentOfPath 显式信号与上游路径无关：同一请求头或 body 信号在
// /chat/completions、/responses、/v1/messages 上得到相同的键。
func TestSessionKeySignalIndependentOfPath(t *testing.T) {
	paths := []string{"/chat/completions", "/responses", "/v1/messages"}
	for _, tc := range []struct {
		name string
		req  affinityKeyReq
	}{
		{"header", affinityKeyReq{headers: map[string]string{"X-Session-ID": "s1"}}},
		{"body-session", affinityKeyReq{fields: map[string]any{"session_id": "s1"}}},
		{"body-thread", affinityKeyReq{fields: map[string]any{"thread_id": "t1"}}},
		{"claude-metadata", affinityKeyReq{fields: map[string]any{"metadata": map[string]any{"user_id": `{"session_id":"s1"}`}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := affinityKeyOf(t, paths[0], tc.req.headers, tc.req.fields)
			affinityAssertKeyShape(t, want)
			for _, path := range paths[1:] {
				if got := affinityKeyOf(t, path, tc.req.headers, tc.req.fields); got != want {
					t.Fatalf("%s 的会话键与 %s 不同: %s != %s", path, paths[0], got, want)
				}
			}
		})
	}
}

// TestSessionKeyHeaderSignalWithInvalidJSON 请求体不是合法 JSON 对象时 body 来源全部跳过，但请求头信号仍然有效；
// 无任何信号时仍返回空字符串。
func TestSessionKeyHeaderSignalWithInvalidJSON(t *testing.T) {
	const path = "/chat/completions"
	headers := map[string]string{"X-Session-ID": "s1"}
	key := affinityKeyRaw(t, path, headers, "not json")
	affinityAssertKeyShape(t, key)
	// 会话键只取决于信号本身：非法 JSON 与合法 JSON 在同一请求头下同键。
	if got := affinityKeyOf(t, path, headers, nil); got != key {
		t.Fatalf("非法 JSON 不应影响请求头信号: %s != %s", got, key)
	}
	// 合法 JSON 但不是对象（数组、null）时 body 来源同样跳过。
	for _, body := range []string{`[1,2,3]`, `null`, `"text"`} {
		if got := affinityKeyRaw(t, path, headers, body); got != key {
			t.Fatalf("body=%s 时请求头信号应照常生效: %s != %s", body, got, key)
		}
	}
	// body 来源在非法 JSON 下不产生信号。
	if got := affinityKeyRaw(t, path, nil, `{"session_id":`); got != "" {
		t.Fatalf("非法 JSON 且无请求头时键 = %q, want 空字符串", got)
	}
	if got := affinityKeyRaw(t, path, nil, "not json"); got != "" {
		t.Fatalf("非法 JSON 且无请求头时键 = %q, want 空字符串", got)
	}
}

// TestSessionKeyContentFallbackDistinctFromExplicitID 内容兜底与显式会话 ID 的编码互不混淆：
// 加上显式信号后键改变；显式信号相同时内容不同不影响键。
func TestSessionKeyContentFallbackDistinctFromExplicitID(t *testing.T) {
	const path = "/chat/completions"
	fallback := affinityFallbackKey(t, path)
	affinityAssertKeyShape(t, fallback)
	withSession := affinityKeyOf(t, path, nil, map[string]any{"session_id": "s1"})
	if withSession == fallback {
		t.Fatal("显式 session_id 与内容兜底不应同键")
	}
	withHeader := affinityKeyOf(t, path, map[string]string{"X-Session-ID": "s1"}, nil)
	if withHeader == fallback || withHeader == withSession {
		t.Fatal("显式请求头信号应与内容兜底、body 信号互不相同")
	}
	changed := affinityKeyOf(t, path, nil, map[string]any{
		"session_id": "s1",
		"messages":   []any{map[string]string{"role": "user", "content": "another question"}},
	})
	if changed != withSession {
		t.Fatal("显式信号相同时内容不同不应改变会话键")
	}
}

// TestSessionKeyExactSignalStrings 白盒固定各来源的信号串编码（sessionSignal）：显式信号形如 前缀:ID，
// 每个来源取一个代表性取值断言精确输出；内容兜底以 "content\x00" 开头，与显式信号不会混淆。
func TestSessionKeyExactSignalStrings(t *testing.T) {
	const path = "/chat/completions"
	for _, tc := range []struct {
		name    string
		headers map[string]string
		fields  map[string]any
		want    string
	}{
		{"claude-header", map[string]string{"X-Claude-Code-Session-Id": "id-1"}, nil, "claude:id-1"},
		{"claude-metadata", nil, map[string]any{"metadata": map[string]any{"user_id": `{"session_id":"id-1"}`}}, "claude:id-1"},
		{"codex", map[string]string{"Session-Id": "id-1"}, nil, "codex:id-1"},
		{"agy", map[string]string{"X-Http-Session-Id": "id-1"}, nil, "agy:id-1"},
		{"header", map[string]string{"X-Session-ID": "id-1"}, nil, "header:id-1"},
		{"affinity", map[string]string{"X-Session-Affinity": "id-1"}, nil, "affinity:id-1"},
		{"slot", map[string]string{"X-Slot-Session-Id": "id-1"}, nil, "slot:id-1"},
		{"task-header", map[string]string{"X-Task-ID": "id-1"}, nil, "task:id-1"},
		{"conv-header", map[string]string{"X-Conversation-Id": "id-1"}, nil, "conv:id-1"},
		{"thread-header", map[string]string{"X-Thread-Id": "id-1"}, nil, "thread:id-1"},
		{"clientreq", map[string]string{"X-Client-Request-Id": "id-1"}, nil, "clientreq:id-1"},
		{"agenttask", map[string]string{"X-Agent-Task-Id": "id-1"}, nil, "agenttask:id-1"},
		{"req", map[string]string{"X-Request-Id": "id-1"}, nil, "req:id-1"},
		{"body-thread", nil, map[string]any{"thread_id": "id-1"}, "thread:id-1"},
		{"body-session", nil, map[string]any{"session_id": "id-1"}, "session:id-1"},
		{"body-task", nil, map[string]any{"task_id": "id-1"}, "task:id-1"},
		{"body-pck", nil, map[string]any{"prompt_cache_key": "id-1"}, "pck:id-1"},
		{"body-conversation", nil, map[string]any{"conversation": map[string]any{"id": "id-1"}}, "conv:id-1"},
		{"body-user-id", nil, map[string]any{"metadata": map[string]any{"user_id": "id-1"}}, "user:id-1"},
		{"body-conversation-id", nil, map[string]any{"conversation_id": "id-1"}, "conv:id-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := affinityRequest(path, "", "")
			for name, value := range tc.headers {
				r.Header.Set(name, value)
			}
			if got := sessionSignal(r.Header, affinityBody(t, tc.fields), path); got != tc.want {
				t.Fatalf("sessionSignal() = %q, want %q", got, tc.want)
			}
		})
	}

	// 内容兜底为 "content\x00" + hex(sha256(首条 user 文本))，不含 model，与任何 前缀:ID 的显式信号不会混淆。
	fallbackBody := affinityBody(t, map[string]any{
		"model":    "gpt-4.1",
		"messages": []any{map[string]string{"role": "user", "content": "first question"}},
	})
	fallback := sessionSignal(affinityRequest(path, "", "").Header, fallbackBody, path)
	digest := sha256.Sum256([]byte("first question"))
	wantFallback := "content\x00" + hex.EncodeToString(digest[:])
	if fallback != wantFallback {
		t.Fatalf("内容兜底信号 = %q, want %q", fallback, wantFallback)
	}

	// 会话键是信号串的 SHA-256 十六进制，不含客户端密钥。
	key := sessionKey(affinityRequest(path, "client-a", ""), fallbackBody, path)
	keyDigest := sha256.Sum256([]byte(wantFallback))
	if want := hex.EncodeToString(keyDigest[:]); key != want {
		t.Fatalf("sessionKey() = %q, want hex(sha256(signal)) = %q", key, want)
	}
}
