package copilot

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// affinityBody 把字段编码为 JSON，避免手写转义出错。
func affinityBody(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// affinityChatBody 返回首条 user 消息为 text 的 chat 请求体。
func affinityChatBody(t *testing.T, text string) string {
	t.Helper()
	return string(affinityBody(t, map[string]any{
		"model":    "gpt-4.1",
		"messages": []any{map[string]string{"role": "user", "content": text}},
	}))
}

// affinityRequest 构造生成请求。bearer 非空时带 Authorization: Bearer，xAPIKey 非空时带 X-Api-Key。
func affinityRequest(path, bearer, xAPIKey string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if xAPIKey != "" {
		r.Header.Set("X-Api-Key", xAPIKey)
	}
	return r
}

// affinityAssertKeyShape 检查会话键是 64 位十六进制字符串（sha256），不包含任何原文。
func affinityAssertKeyShape(t *testing.T, key string) {
	t.Helper()
	if len(key) != 64 {
		t.Fatalf("会话键长度 = %d, want 64: %q", len(key), key)
	}
	if _, err := hex.DecodeString(key); err != nil {
		t.Fatalf("会话键不是十六进制: %q", key)
	}
}

// affinityClaudeBody 构造 /v1/messages 请求体；userID 为 nil 时不带 metadata。
func affinityClaudeBody(t *testing.T, userID any, text string) []byte {
	t.Helper()
	fields := map[string]any{
		"model":    "claude-sonnet",
		"messages": []any{map[string]string{"role": "user", "content": text}},
	}
	if userID != nil {
		fields["metadata"] = map[string]any{"user_id": userID}
	}
	return affinityBody(t, fields)
}

const affinitySession = "11111111-2222-4333-8444-555555555555"

// TestSessionKeyPromptCacheKey prompt_cache_key（及 promptCacheKey）在所有上游路径上作为会话信号，优先于内容兜底；
// 非字符串或空字符串视为没有该信号，回退到内容兜底。跨路径一致性见 TestSessionKeySignalsApplyToAllPaths。
func TestSessionKeyPromptCacheKey(t *testing.T) {
	key := func(fields map[string]any) string {
		return sessionKey(affinityRequest("/v1/responses", "client-a", ""), affinityBody(t, fields), "/responses")
	}
	cacheA := key(map[string]any{"model": "gpt-4.1", "prompt_cache_key": "cache-a", "input": "first question"})
	affinityAssertKeyShape(t, cacheA)
	if got := key(map[string]any{"model": "gpt-4.1", "prompt_cache_key": "cache-a", "input": "later question"}); got != cacheA {
		t.Fatal("同一 prompt_cache_key 应优先于输入内容，键必须相同")
	}
	if got := key(map[string]any{"model": "gpt-4.1", "prompt_cache_key": "cache-b", "input": "first question"}); got == cacheA {
		t.Fatal("不同 prompt_cache_key 得到了相同的会话键")
	}
	plain := key(map[string]any{"model": "gpt-4.1", "input": "first question"})
	for _, value := range []any{42, "", nil} {
		if got := key(map[string]any{"model": "gpt-4.1", "prompt_cache_key": value, "input": "first question"}); got != plain {
			t.Fatalf("prompt_cache_key=%v 应视为无信号，回退到内容兜底", value)
		}
	}
}

// TestSessionKeyClaudeSessionFromMetadata metadata.user_id 中的 Claude Code 会话 ID。
// JSON 与旧格式表示同一会话时键相同；session_id 不要求 UUID 格式，也不做大小写归一（大小写不同是不同会话）；
// 不再有 user_id 的 4096 字节上限；非字符串的 user_id 视为不存在，回退到内容兜底。
func TestSessionKeyClaudeSessionFromMetadata(t *testing.T) {
	claudeKey := func(userID any, text string) string {
		return sessionKey(affinityRequest("/v1/messages", "client-a", ""), affinityClaudeBody(t, userID, text), "/v1/messages")
	}
	jsonID := `{"device_id":"synthetic-device","session_id":"` + affinitySession + `"}`
	want := claudeKey(jsonID, "first")
	affinityAssertKeyShape(t, want)

	if got := claudeKey(jsonID, "second"); got != want {
		t.Fatal("同一会话的后续消息内容变化时键应保持不变")
	}
	if got := claudeKey("user_synthetic_account_acct_session_"+affinitySession, "third"); got != want {
		t.Fatal("旧格式 user_id 与 JSON 格式表示同一会话，键必须相同")
	}
	// 大小写不做归一：用含十六进制字母的 session_id 验证（affinitySession 全是数字，无法区分大小写）。
	const cased = "aabbccdd-1122-4333-8444-55555555aabb"
	if got, upper := claudeKey(`{"session_id":"`+cased+`"}`, "fourth"),
		claudeKey(`{"session_id":"`+strings.ToUpper(cased)+`"}`, "fourth"); got == upper {
		t.Fatal("session_id 大小写不同应得到不同的会话键")
	}
	if got := claudeKey(`{"session_id":"66666666-7777-4888-8999-000000000000"}`, "first"); got == want {
		t.Fatal("不同会话得到了相同的会话键")
	}
	// session_id 不要求 UUID 格式：任意非空 ID 都是有效会话。
	notUUID := claudeKey(`{"session_id":"not-a-uuid"}`, "first")
	affinityAssertKeyShape(t, notUUID)
	if notUUID == want {
		t.Fatal("不同 session_id 得到了相同的会话键")
	}

	fallback := claudeKey(nil, "first")
	if fallback == "" || fallback == want || fallback == notUUID {
		t.Fatalf("没有 metadata 时应回退到内容兜底（非空且与会话键不同），实际 = %q", fallback)
	}
	// 不再有 4096 字节上限：超长 user_id 中的 session_id 照常生效。
	if got := claudeKey(`{"pad":"`+strings.Repeat("x", 4096)+`","session_id":"`+affinitySession+`"}`, "fifth"); got != want {
		t.Fatal("超长 user_id 不再受限，同一 session_id 应得到相同的会话键")
	}
	// 非字符串的 user_id 视为不存在。
	if got := claudeKey(map[string]string{"session_id": affinitySession}, "first"); got != fallback {
		t.Fatal("非字符串的 user_id 应被忽略，回退到内容兜底")
	}
}

// TestSessionKeyFallbackUsesFirstUserMessageAndModel 没有会话 ID 时，兜底信号为 model 与首条 user 消息的文本。
func TestSessionKeyFallbackUsesFirstUserMessageAndModel(t *testing.T) {
	chatKey := func(model string, messages []any) string {
		body := affinityBody(t, map[string]any{"model": model, "messages": messages})
		return sessionKey(affinityRequest("/v1/chat/completions", "client-a", ""), body, "/chat/completions")
	}
	user := func(text string) map[string]any { return map[string]any{"role": "user", "content": text} }
	first := chatKey("gpt-4.1", []any{user("first question")})
	affinityAssertKeyShape(t, first)

	assistant := map[string]any{"role": "assistant", "content": "answer"}
	if got := chatKey("gpt-4.1", []any{user("first question"), assistant, user("follow up")}); got != first {
		t.Fatal("后续轮次追加消息不应改变会话键")
	}
	system := map[string]any{"role": "system", "content": "a system prompt"}
	if got := chatKey("gpt-4.1", []any{system, user("first question")}); got != first {
		t.Fatal("system 消息应被跳过，由首条 user 消息决定兜底信号")
	}
	parts := []any{
		map[string]any{"type": "image_url", "image_url": map[string]string{"url": "https://example.invalid/x.png"}},
		map[string]string{"type": "text", "text": "first question"},
	}
	if got := chatKey("gpt-4.1", []any{map[string]any{"role": "user", "content": parts}}); got != first {
		t.Fatal("content 数组应拼接文本片段，并忽略非文本片段")
	}
	if chatKey("gpt-4.1-mini", []any{user("first question")}) == first {
		t.Fatal("不同 model 得到了相同的兜底键")
	}
	if chatKey("gpt-4.1", []any{user("another question")}) == first {
		t.Fatal("不同首条 user 消息得到了相同的兜底键")
	}

	responsesKey := func(input any) string {
		body := affinityBody(t, map[string]any{"model": "gpt-4.1", "input": input})
		return sessionKey(affinityRequest("/v1/responses", "client-a", ""), body, "/responses")
	}
	inputText := responsesKey("first question")
	if inputText == "" {
		t.Fatal("Responses 字符串 input 应产生兜底键")
	}
	if got := responsesKey([]any{map[string]string{"role": "user", "content": "first question"}}); got != inputText {
		t.Fatal("Responses input 数组中的首条 user 项应与字符串 input 等价")
	}
}

// TestSessionKeyWithoutSignalIsEmpty 没有任何会话信号（仅 model、无 user 消息或非法 JSON）时返回空字符串，不使用亲和。
func TestSessionKeyWithoutSignalIsEmpty(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{name: "model-only", path: "/chat/completions", body: `{"model":"gpt-4.1"}`},
		{name: "no-user-message", path: "/chat/completions", body: `{"model":"gpt-4.1","messages":[{"role":"assistant","content":"hi"}]}`},
		{name: "empty-messages", path: "/chat/completions", body: `{"model":"gpt-4.1","messages":[]}`},
		{name: "responses-without-input", path: "/responses", body: `{"model":"gpt-4.1"}`},
		{name: "invalid-json", path: "/chat/completions", body: `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionKey(affinityRequest(tc.path, "client-a", ""), []byte(tc.body), tc.path); got != "" {
				t.Fatalf("无会话信号时键 = %q, want 空字符串", got)
			}
		})
	}
}

// TestSessionKeySignalsApplyToAllPaths 显式会话信号不再按路径限制：prompt_cache_key 与 metadata.user_id 的
// Claude 会话在 /chat/completions、/responses、/v1/messages 上都生效，且同一信号在各路径上得到相同的键
// （键只取决于客户端密钥与信号本身）。
func TestSessionKeySignalsApplyToAllPaths(t *testing.T) {
	paths := []string{"/chat/completions", "/responses", "/v1/messages"}
	user := []any{map[string]string{"role": "user", "content": "first question"}}

	cacheKey := func(path, cache string) string {
		body := affinityBody(t, map[string]any{"model": "gpt-4.1", "messages": user, "prompt_cache_key": cache})
		return sessionKey(affinityRequest(path, "client-a", ""), body, path)
	}
	a := cacheKey(paths[0], "cache-a")
	affinityAssertKeyShape(t, a)
	for _, path := range paths[1:] {
		if got := cacheKey(path, "cache-a"); got != a {
			t.Fatalf("%s 的 prompt_cache_key 信号与 %s 不同: %s != %s", path, paths[0], got, a)
		}
	}
	if got := cacheKey(paths[0], "cache-b"); got == a {
		t.Fatal("不同 prompt_cache_key 得到了相同的会话键")
	}

	claudeKey := func(path, session string) string {
		body := affinityBody(t, map[string]any{"model": "gpt-4.1", "messages": user,
			"metadata": map[string]any{"user_id": `{"session_id":"` + session + `"}`}})
		return sessionKey(affinityRequest(path, "client-a", ""), body, path)
	}
	c := claudeKey(paths[0], affinitySession)
	affinityAssertKeyShape(t, c)
	for _, path := range paths[1:] {
		if got := claudeKey(path, affinitySession); got != c {
			t.Fatalf("%s 的 Claude 会话信号与 %s 不同: %s != %s", path, paths[0], got, c)
		}
	}
	if c == a {
		t.Fatal("prompt_cache_key 与 Claude 会话是不同前缀的信号，不应同键")
	}
}

// TestSessionKeyClientKeyIsolation 会话键包含客户端密钥：Bearer 优先于 X-Api-Key，不同密钥互不共享，
// 未配置密钥时仍然稳定地产生键；键中不包含密钥或正文原文。
func TestSessionKeyClientKeyIsolation(t *testing.T) {
	body := []byte(affinityChatBody(t, "first question"))
	key := func(bearer, xAPIKey string) string {
		return sessionKey(affinityRequest("/v1/chat/completions", bearer, xAPIKey), body, "/chat/completions")
	}
	keyA := key("key-a", "")
	affinityAssertKeyShape(t, keyA)
	if key("key-b", "") == keyA {
		t.Fatal("不同客户端密钥共享了同一个会话键")
	}
	if key("key-a", "key-b") != keyA {
		t.Fatal("Authorization: Bearer 应优先于 X-Api-Key")
	}
	if key("", "key-a") != keyA {
		t.Fatal("只带 X-Api-Key 且密钥相同时应得到相同的会话键")
	}
	anonymous := key("", "")
	if anonymous == "" || anonymous != key("", "") || anonymous == keyA {
		t.Fatalf("未配置客户端密钥时应得到稳定且独立的会话键，实际 = %q", anonymous)
	}
	for _, secret := range []string{"key-a", "first question"} {
		if strings.Contains(keyA, secret) {
			t.Fatalf("会话键包含原文 %q", secret)
		}
	}
}

// TestAffinityTableExpiresBindingsAfterTTL 绑定在 TTL 内有效，过期后查询不到；重新绑定会续期并可以更换账号。
func TestAffinityTableExpiresBindingsAfterTTL(t *testing.T) {
	const ttl = time.Hour
	table := newAffinityTable(ttl)
	now := time.Now()
	table.bind("session", "alice", now)
	if id, ok := table.lookup("session", now.Add(ttl-time.Second)); !ok || id != "alice" {
		t.Fatalf("TTL 内 lookup = %q, %v; want alice, true", id, ok)
	}
	if _, ok := table.lookup("session", now.Add(ttl+time.Second)); ok {
		t.Fatal("过期的绑定仍然有效")
	}

	table.bind("session", "bob", now.Add(ttl))
	if id, ok := table.lookup("session", now.Add(ttl+time.Minute)); !ok || id != "bob" {
		t.Fatalf("续期后 lookup = %q, %v; want bob, true", id, ok)
	}
}

// TestAffinityTableCapacityEvictsEarliestExpiring 表满时新键淘汰最早过期的绑定；续期已有键不淘汰任何绑定。
func TestAffinityTableCapacityEvictsEarliestExpiring(t *testing.T) {
	table := newAffinityTable(2 * time.Hour)
	base := time.Now()
	// key-0000 绑定最早，因此最早过期。
	for i := range maxSessionBindings {
		table.bind(fmt.Sprintf("key-%04d", i), "alice", base.Add(time.Duration(i)*time.Second))
	}
	if got := table.count(); got != maxSessionBindings {
		t.Fatalf("count() = %d, want %d", got, maxSessionBindings)
	}
	now := base.Add(maxSessionBindings * time.Second)

	table.bind("key-0001", "bob", now)
	if got := table.count(); got != maxSessionBindings {
		t.Fatalf("续期后 count() = %d, want %d", got, maxSessionBindings)
	}
	if _, ok := table.lookup("key-0000", now); !ok {
		t.Fatal("续期已有键淘汰了无关绑定")
	}

	table.bind("new-session", "carol", now)
	if got := table.count(); got != maxSessionBindings {
		t.Fatalf("满表插入后 count() = %d, want %d", got, maxSessionBindings)
	}
	if _, ok := table.lookup("key-0000", now); ok {
		t.Fatal("最早过期的绑定没有被淘汰")
	}
	if id, ok := table.lookup("new-session", now); !ok || id != "carol" {
		t.Fatalf("新绑定 lookup = %q, %v; want carol, true", id, ok)
	}
	if id, ok := table.lookup("key-0001", now); !ok || id != "bob" {
		t.Fatalf("续期后的绑定 lookup = %q, %v; want bob, true", id, ok)
	}
	if _, ok := table.lookup("key-0002", now); !ok {
		t.Fatal("非最早过期的绑定不应被淘汰")
	}
}

// TestAffinityTableDropsExpiredBeforeLiveBindings 表满时先删除已过期的绑定，不得因此淘汰仍有效的绑定。
func TestAffinityTableDropsExpiredBeforeLiveBindings(t *testing.T) {
	table := newAffinityTable(time.Hour)
	now := time.Now()
	table.bind("expired", "alice", now.Add(-2*time.Hour))
	for i := range maxSessionBindings - 1 {
		table.bind(fmt.Sprintf("live-%04d", i), "bob", now)
	}
	if got := table.count(); got != maxSessionBindings {
		t.Fatalf("填满后 count() = %d, want %d", got, maxSessionBindings)
	}

	table.bind("new-session", "carol", now)
	if got := table.count(); got != maxSessionBindings {
		t.Fatalf("清理过期项后 count() = %d, want %d", got, maxSessionBindings)
	}
	if _, ok := table.lookup("live-0000", now); !ok {
		t.Fatal("过期项清理时误删了仍有效的绑定")
	}
	if id, ok := table.lookup("new-session", now); !ok || id != "carol" {
		t.Fatalf("新绑定 lookup = %q, %v; want carol, true", id, ok)
	}
}

// TestSessionAffinityPinsSessionOverPriority 同一会话固定使用首次绑定的账号，即使之后出现优先级更高的账号；
// 新会话则按当前优先级选择。
func TestSessionAffinityPinsSessionOverPriority(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, DefaultSessionAffinityTTL, "alice", "bob")
	x := affinityChatBody(t, "first question")
	y := affinityChatBody(t, "second question")
	// X 首次绑定到文件名最小的 alice；之后 bob 的优先级高于 alice。
	if res := poolRequest(p, "POST", "/v1/chat/completions", x); res.Code != http.StatusOK {
		t.Fatalf("X 首次请求 status=%d body=%s", res.Code, res.Body.String())
	}
	mgmtPatchPriority(t, p, "bob", 10)
	for i, body := range []string{y, x} {
		if res := poolRequest(p, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
			t.Fatalf("请求 %d status=%d body=%s", i, res.Code, res.Body.String())
		}
	}
	// Y 是新会话，按当前优先级由 bob 处理；X 命中亲和，仍由 alice 处理。
	assertGenerationOrder(t, up.pool, mgmtOrder("alice", "bob", "alice")...)
}

// TestSessionAffinityDisabledWithZeroTTLReselectsEachRequest SessionAffinityTTL<=0 时关闭亲和：
// 相同会话的请求每次都按当前优先级重新选择，不会固定到首次使用的账号。
func TestSessionAffinityDisabledWithZeroTTLReselectsEachRequest(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice", "bob")
	body := affinityChatBody(t, "pinned?")
	if res := poolRequest(p, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
		t.Fatalf("请求 0 status=%d body=%s", res.Code, res.Body.String())
	}
	mgmtPatchPriority(t, p, "bob", 10)
	for i := 1; i < 3; i++ {
		if res := poolRequest(p, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
			t.Fatalf("请求 %d status=%d body=%s", i, res.Code, res.Body.String())
		}
	}
	// 关闭亲和时没有绑定，bob 优先级更高，之后的请求都由 bob 处理。
	assertGenerationOrder(t, up.pool, mgmtOrder("alice", "bob", "bob")...)
}

// TestSessionAffinityRebindsWhenBoundAccountUnavailable 绑定账号被禁用、删除、冷却或 session 失败后，
// 会话改用其他账号，并重新绑定到新账号；原账号不再被使用。
func TestSessionAffinityRebindsWhenBoundAccountUnavailable(t *testing.T) {
	for _, reason := range []string{"disabled", "deleted", "cooldown", "session-failure"} {
		t.Run(reason, func(t *testing.T) {
			up := newMgmtUpstream(t)
			up.pool.setSessionTTL(time.Minute) // 会话很快过期，使 session 失败在下一次请求时生效。
			p, _ := mgmtNewPool(t, up, DefaultSessionAffinityTTL, "alice", "bob", "carol")
			body := affinityChatBody(t, "pinned session")
			if res := poolRequest(p, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
				t.Fatalf("准备请求 status=%d body=%s", res.Code, res.Body.String())
			}

			switch reason {
			case "disabled":
				if res := mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"alice","disabled":true}`, mgmtKey); res.Code != http.StatusOK {
					t.Fatalf("禁用 alice status=%d body=%s", res.Code, res.Body.String())
				}
			case "deleted":
				if res := mgmtCall(p, "DELETE", "/v0/management/accounts?id=alice", "", mgmtKey); res.Code != http.StatusOK {
					t.Fatalf("删除 alice status=%d body=%s", res.Code, res.Body.String())
				}
			case "cooldown":
				p.accounts[0].service.setCooldown(time.Now().Add(time.Hour))
			case "session-failure":
				up.pool.failSession("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"alice down"}`})
			}

			// 第一次改用其他账号，第二次必须继续使用同一个账号（已重新绑定），而不是重新按优先级选择。
			var served []string
			for i := range 2 {
				if res := poolRequest(p, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
					t.Fatalf("失效后请求 %d status=%d body=%s", i, res.Code, res.Body.String())
				}
				order := up.pool.generationOrder()
				served = append(served, order[len(order)-1])
			}
			if served[0] == "oauth-alice" || served[1] != served[0] {
				t.Fatalf("失效后的服务账号 = %v, want 非 alice 且两次相同", served)
			}
			aliceServed := 0
			for _, account := range up.pool.generationOrder() {
				if account == "oauth-alice" {
					aliceServed++
				}
			}
			if aliceServed != 1 {
				t.Fatalf("alice 被服务 %d 次, want 只有准备请求的 1 次", aliceServed)
			}
		})
	}
}

// TestSessionAffinityOversizedBodyStillReturns413 超过 16 MiB 的请求体不计算会话键，但下游仍须读到完整请求体并返回 413。
func TestSessionAffinityOversizedBodyStillReturns413(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, DefaultSessionAffinityTTL, "alice", "bob")
	body := `{"model":"gpt-4.1","messages":[{"role":"user","content":"` + strings.Repeat("a", 16<<20) + `"}]}`
	w := poolRequest(p, "POST", "/v1/chat/completions", body)
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "16 MiB") {
		t.Fatalf("超大请求体 status=%d body=%.200s, want 413 且提示 16 MiB", w.Code, w.Body.String())
	}
	if got := up.pool.generationOrder(); len(got) != 0 {
		t.Fatalf("超大请求体不应到达上游生成接口: %v", got)
	}
}
