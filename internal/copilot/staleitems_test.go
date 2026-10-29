package copilot

// 本文件覆盖“上游 /responses 因输出项绑定在原连接上而拒绝回放”的处理（见 staleitems.go）。
//
// Copilot /responses 返回的输出项 id（以及 reasoning 项的 encrypted_content）绑定在产生它的上游连接上；
// 客户端下一轮回放这些项时若落到另一个账号或连接已变，上游会拒绝：
//   - 401，正文含 "does not belong to this connection"；
//   - 400，正文同时含 "encrypted content" 与 "could not be verified"。
//
// 期望行为：
//   - staleItemError(status, body) 识别上述两种响应（不区分大小写），其余（普通 401/400 等）不是。
//   - withoutConnectionItems(body) 丢弃 reasoning 与 item_reference 项、删去其余对象项的顶层 id，
//     没有可改内容或请求体不合法时返回原 body 与 false。
//   - Service 在同一账号、同一 session、同一模型上用清理后的请求体最多重发一次，仅限上游 /responses。
//   - 无法重试或重试后仍失败时，最终响应标记 failureIgnored：Pool 不计账号错误、不冷却、不写风控记录。
//   - 最终响应是失效项 401 时不能把 401 返回客户端（入口按 401 频率封禁 IP）：状态改为 500，正文仍为上游正文；
//     是否 stream 不影响该转换。失效项 400 的最终响应仍按 400 原样返回；重试后是其他错误也原样返回。
//
// 实现者必须提供的内部名字（测试按此书写）：
//   - func staleItemError(status int, body []byte) bool
//   - func withoutConnectionItems(body []byte) ([]byte, bool)

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// 上游拒绝回放的两种典型正文。
	stale401Body  = `{"error":{"message":"input item does not belong to this connection","type":"invalid_request_error"}}`
	stale401BodyB = `{"error":{"message":"input item ID does not belong to this connection","type":"invalid_request_error"}}`
	stale400Body  = `{"error":{"message":"The encrypted content for item rs_byok_mvku8ffo_d5 could not be verified. Reason: Encrypted content item_id did not match the target item id.","type":"invalid_request_error"}}`

	// staleRequestBody 含需要清理的各类项：带 id 的 message、reasoning、function_call，不带 id 的 function_call_output，以及 item_reference。
	staleRequestBody = `{"model":"gpt-4.1","input":[` +
		`{"type":"message","id":"msg_1","role":"user","content":"hi <b> & c"},` +
		`{"type":"reasoning","id":"rs_1","encrypted_content":"abc","summary":[]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"ok"},` +
		`{"type":"item_reference","id":"msg_0"}` +
		`],"temperature":1.0,"seed":12345678901234567890}`

	// staleCleanedBody 是 staleRequestBody 清理后的语义：reasoning 与 item_reference 被丢弃，其余项没有顶层 id。
	staleCleanedBody = `{"model":"gpt-4.1","input":[` +
		`{"type":"message","role":"user","content":"hi <b> & c"},` +
		`{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"ok"}` +
		`],"temperature":1.0,"seed":12345678901234567890}`

	// staleUncleanableBody 没有任何可清理的内容：无 id、无 reasoning、无 item_reference。
	staleUncleanableBody = `{"model":"gpt-4.1","input":[{"role":"user","content":"hi"}]}`
)

// TestStaleItemError 表驱动检查识别规则：401 看 "does not belong to this connection"，
// 400 要求同时含 "encrypted content" 与 "could not be verified"，均不区分大小写；其余一律为 false。
func TestStaleItemError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"401-item", 401, stale401Body, true},
		{"401-item-id", 401, stale401BodyB, true},
		{"401-uppercase", 401, `{"error":{"message":"INPUT ITEM DOES NOT BELONG TO THIS CONNECTION"}}`, true},
		{"401-mixed-case", 401, `Input Item Does Not Belong To This Connection`, true},
		{"401-plain-text", 401, `does not belong to this connection`, true},
		{"401-bad-credentials", 401, `{"error":{"message":"bad credentials"}}`, false},
		{"401-empty", 401, ``, false},
		{"401-partial-phrase", 401, `{"error":{"message":"item does not belong to this account"}}`, false},
		{"401-with-400-phrases", 401, stale400Body, false},

		{"400-encrypted", 400, stale400Body, true},
		{"400-uppercase", 400, `{"error":{"message":"THE ENCRYPTED CONTENT FOR ITEM rs_1 COULD NOT BE VERIFIED"}}`, true},
		{"400-mixed-case", 400, `Encrypted Content could Not Be Verified`, true},
		{"400-only-encrypted-content", 400, `{"error":{"message":"encrypted content is invalid"}}`, false},
		{"400-only-could-not-be-verified", 400, `{"error":{"message":"signature could not be verified"}}`, false},
		{"400-plain", 400, `{"error":{"message":"Invalid request"}}`, false},
		{"400-empty", 400, ``, false},
		{"400-with-401-phrase", 400, stale401Body, false},

		{"403-401-phrase", 403, stale401Body, false},
		{"403-400-phrases", 403, stale400Body, false},
		{"404", 404, stale401Body, false},
		{"429", 429, stale401Body, false},
		{"500", 500, stale400Body, false},
		{"200", 200, stale401Body, false},
		{"0", 0, stale401Body, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleItemError(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("staleItemError(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}

	t.Run("nil-body", func(t *testing.T) {
		if staleItemError(401, nil) || staleItemError(400, nil) {
			t.Fatal("nil 正文不应识别为失效项错误")
		}
	})
}

// TestWithoutConnectionItemsCleans 需要清理的请求体：输出与期望在语义上一致（数字原文保留），
// 并且输入切片未被修改、HTML 字符与数字原样保留。
func TestWithoutConnectionItemsCleans(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			"drops-reasoning-with-encrypted-content",
			`{"model":"m","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"abc","summary":[]},{"type":"message","role":"user","content":"hi"}]}`,
			`{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`,
		},
		{
			"drops-reasoning-without-encrypted-content",
			`{"model":"m","input":[{"type":"reasoning","summary":[]},{"type":"message","role":"user","content":"hi"}]}`,
			`{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`,
		},
		{
			"drops-reasoning-with-null-encrypted-content",
			`{"model":"m","input":[{"type":"reasoning","id":"rs_1","encrypted_content":null,"summary":[]},{"type":"message","role":"user","content":"hi"}]}`,
			`{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`,
		},
		{
			"drops-item-reference",
			`{"model":"m","input":[{"type":"item_reference","id":"msg_0"},{"type":"message","role":"user","content":"hi"}]}`,
			`{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`,
		},
		{
			"drops-all-items-leaves-empty-array",
			`{"model":"m","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"abc"},{"type":"item_reference","id":"msg_0"}]}`,
			`{"model":"m","input":[]}`,
		},
		{
			"removes-id-from-message-function-call-and-output",
			`{"model":"m","input":[` +
				`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"x"}]},` +
				`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"f","arguments":"{}"},` +
				`{"type":"function_call_output","id":"fco_1","call_id":"call_1","output":"ok"}]}`,
			`{"model":"m","input":[` +
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x"}]},` +
				`{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},` +
				`{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`,
		},
		{
			"removes-id-from-item-without-type",
			`{"model":"m","input":[{"id":"msg_1","role":"user","content":"hi"}]}`,
			`{"model":"m","input":[{"role":"user","content":"hi"}]}`,
		},
		{
			"only-top-level-id-is-removed",
			`{"model":"m","input":[{"type":"function_call_output","id":"fco_1","call_id":"call_1","output":{"id":"inner","items":[{"id":"deep"}]}}]}`,
			`{"model":"m","input":[{"type":"function_call_output","call_id":"call_1","output":{"id":"inner","items":[{"id":"deep"}]}}]}`,
		},
		{
			"keeps-non-object-elements",
			`{"model":"m","input":["hello",1,null,true,["reasoning"],{"type":"message","id":"msg_1","role":"user","content":"hi"}]}`,
			`{"model":"m","input":["hello",1,null,true,["reasoning"],{"type":"message","role":"user","content":"hi"}]}`,
		},
		{
			"keeps-other-top-level-fields",
			`{"model":"m","id":"top-level-id","store":false,"instructions":"be <nice> & kind","tools":[{"type":"function","name":"f"}],` +
				`"tool_choice":"auto","metadata":{"id":"m1"},"input":[{"type":"reasoning","id":"rs_1"},{"type":"message","id":"msg_1","role":"user","content":"hi"}]}`,
			`{"model":"m","id":"top-level-id","store":false,"instructions":"be <nice> & kind","tools":[{"type":"function","name":"f"}],` +
				`"tool_choice":"auto","metadata":{"id":"m1"},"input":[{"type":"message","role":"user","content":"hi"}]}`,
		},
		{
			"reasoning-type-only-matches-exact-value",
			`{"model":"m","input":[{"type":"reasoning_summary","id":"x","text":"t"},{"type":"message","id":"msg_1"}]}`,
			`{"model":"m","input":[{"type":"reasoning_summary","text":"t"},{"type":"message"}]}`,
		},
		{
			"spaced-json",
			`{ "model" : "m" , "input" : [ { "type" : "reasoning" , "id" : "rs_1" } , { "type" : "message" , "id" : "msg_1" } ] }`,
			`{"model":"m","input":[{"type":"message"}]}`,
		},
		{"full-request", staleRequestBody, staleCleanedBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			original := append([]byte(nil), in...)
			out, changed := withoutConnectionItems(in)
			if !changed {
				t.Fatalf("withoutConnectionItems 应返回 true; out=%s", out)
			}
			if !bytes.Equal(in, original) {
				t.Fatalf("输入切片被修改: %s", in)
			}
			got, want := decodeNumberMap(t, out), decodeNumberMap(t, []byte(tc.want))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("清理结果语义不符:\n got=%s\nwant=%s", out, tc.want)
			}
		})
	}
}

// TestWithoutConnectionItemsKeepsLiteralFormatting 数字原文保留、< > & 不转义（与 withoutLoneToolChoice 的重编码方式相同）。
func TestWithoutConnectionItemsKeepsLiteralFormatting(t *testing.T) {
	in := []byte(`{"model":"m","temperature":1.0,"seed":12345678901234567890,"top_p":2.50,"metadata":{"n":1e3},` +
		`"input":[{"type":"reasoning","id":"rs_1"},{"type":"message","id":"msg_1","role":"user","content":"<b> & c > d"},{"type":"x","n":3.10,"big":98765432109876543210}]}`)
	out, changed := withoutConnectionItems(in)
	if !changed {
		t.Fatalf("withoutConnectionItems 应返回 true; out=%s", out)
	}
	for _, literal := range []string{`<b> & c > d`, `1.0`, `12345678901234567890`, `2.50`, `1e3`, `3.10`, `98765432109876543210`} {
		if !bytes.Contains(out, []byte(literal)) {
			t.Fatalf("输出缺少原样片段 %q（不得 HTML 转义或改写数字）: %s", literal, out)
		}
	}
	// Go JSON 默认会把 < > & 转义为 6 个字符的序列（反斜杠 + u003c / u003e / u0026），这里显式拼接出来。
	for _, escaped := range []string{"\\u00" + "3c", "\\u00" + "3e", "\\u00" + "26"} {
		if bytes.Contains(out, []byte(escaped)) {
			t.Fatalf("输出含 HTML 转义 %q: %s", escaped, out)
		}
	}
}

// TestWithoutConnectionItemsKeepsBodyWhenNothingToChange 没有可改的内容、input 不是数组、请求体不是合法 JSON 对象时，
// 返回原 body（逐字节一致）与 false。
func TestWithoutConnectionItemsKeepsBodyWhenNothingToChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"input-empty-array", `{"model":"m","input":[]}`},
		{"input-items-without-id", `{"model":"m","input":[{"type":"message","role":"user","content":"hi"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`},
		{"input-only-non-objects", `{"model":"m","input":["a",1,null,["reasoning"]]}`},
		{"input-string", `{"model":"m","input":"hi","id":"x"}`},
		{"input-object", `{"model":"m","input":{"type":"reasoning","id":"rs_1"}}`},
		{"input-null", `{"model":"m","input":null}`},
		{"input-number", `{"model":"m","input":1}`},
		{"input-absent", `{"model":"m","previous_response_id":"resp_1"}`},
		{"nested-reasoning-not-in-input", `{"model":"m","metadata":{"input":[{"type":"reasoning","id":"rs_1"}]},"input":"hi"}`},
		{"top-level-id-only", `{"id":"abc","model":"m","input":[]}`},
		{"invalid-json", `{"model":"m","input":[{"type":"reasoning","id":"rs_1"}]`},
		{"invalid-json-trailing", `{"model":"m","input":[{"type":"reasoning","id":"rs_1"}]} x`},
		{"json-array", `[{"type":"reasoning","id":"rs_1"}]`},
		{"json-string", `"reasoning"`},
		{"json-null", `null`},
		{"empty", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.body)
			original := append([]byte(nil), in...)
			out, changed := withoutConnectionItems(in)
			if changed {
				t.Fatalf("withoutConnectionItems 应返回 false; out=%s", out)
			}
			if !bytes.Equal(out, original) {
				t.Fatalf("请求体应原样返回:\n got=%s\nwant=%s", out, original)
			}
		})
	}

	t.Run("nil", func(t *testing.T) {
		out, changed := withoutConnectionItems(nil)
		if changed || len(out) != 0 {
			t.Fatalf("nil 输入应返回 (空, false)，得到 (%q, %v)", out, changed)
		}
	})
}

// staleReply 是测试上游对某次生成请求的应答。
type staleReply struct {
	status      int
	body        string
	contentType string // 为空时不设置
}

// staleUpstream 记录上游收到的生成请求，并按序号返回应答：n 从 1 开始，按各上游路径分别计数。
type staleUpstream struct {
	mu         sync.Mutex
	bodies     map[string][][]byte      // 每个路径收到的请求体，按到达顺序
	headers    map[string][]http.Header // 每个路径收到的请求头，按到达顺序
	tokenCalls int                      // session 交换次数
	respond    func(path string, n int, body []byte) staleReply
}

// serve 是 testClient 使用的上游处理器。
func (u *staleUpstream) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			u.mu.Lock()
			u.tokenCalls++
			u.mu.Unlock()
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/responses", "/chat/completions", "/v1/messages":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("读取上游请求体失败: %v", err)
			}
			u.mu.Lock()
			u.bodies[r.URL.Path] = append(u.bodies[r.URL.Path], data)
			u.headers[r.URL.Path] = append(u.headers[r.URL.Path], r.Header.Clone())
			n := len(u.bodies[r.URL.Path])
			u.mu.Unlock()
			reply := u.respond(r.URL.Path, n, data)
			if reply.contentType != "" {
				w.Header().Set("Content-Type", reply.contentType)
			}
			w.WriteHeader(reply.status)
			_, _ = io.WriteString(w, reply.body)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}
}

// staleServiceWith 构造使用 staleUpstream 的 Service。
func staleServiceWith(t *testing.T, respond func(path string, n int, body []byte) staleReply) (*Service, *staleUpstream) {
	t.Helper()
	u := &staleUpstream{bodies: map[string][][]byte{}, headers: map[string][]http.Header{}, respond: respond}
	s := testService(t, Config{APIKey: testAPIKey, Token: "github-secret", Client: testClient(t, u.serve(t))})
	return s, u
}

// staleSequence 返回按 /responses 调用序号给出应答的 respond：replies[n-1]，超出后重复最后一个。
func staleSequence(replies ...staleReply) func(string, int, []byte) staleReply {
	return func(_ string, n int, _ []byte) staleReply {
		if n > len(replies) {
			n = len(replies)
		}
		return replies[n-1]
	}
}

func (u *staleUpstream) sent(path string) [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies[path]...)
}

func (u *staleUpstream) sentHeaders(path string) []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]http.Header(nil), u.headers[path]...)
}

func (u *staleUpstream) sessionExchanges() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.tokenCalls
}

// staleOK 是成功的 JSON 应答。
var staleOK = staleReply{status: 200, body: `{"id":"resp_new","status":"completed"}`, contentType: "application/json"}

// assertStaleBodies 断言上游 /responses 恰好收到 2 次请求：第一次逐字节等于原始请求体，第二次语义等于 wantRetry。
func assertStaleBodies(t *testing.T, u *staleUpstream, original, wantRetry string) {
	t.Helper()
	got := u.sent("/responses")
	if len(got) != 2 {
		t.Fatalf("上游 /responses 收到 %d 次请求, want 2", len(got))
	}
	if string(got[0]) != original {
		t.Fatalf("第一次请求体应是原始请求体:\n got=%s\nwant=%s", got[0], original)
	}
	if want := decodeNumberMap(t, []byte(wantRetry)); !reflect.DeepEqual(decodeNumberMap(t, got[1]), want) {
		t.Fatalf("第二次请求体应已清理:\n got=%s\nwant=%s", got[1], wantRetry)
	}
	for _, literal := range []string{`hi <b> & c`, `12345678901234567890`, `1.0`} {
		if strings.Contains(original, literal) && !bytes.Contains(got[1], []byte(literal)) {
			t.Fatalf("重试请求体缺少原样片段 %q: %s", literal, got[1])
		}
	}
}

// TestStaleItemsRetriesAfter401 上游首次返回 401（两种措辞）、第二次 200：客户端得到 200，
// 上游 /responses 恰好 2 次，首次为原始请求体、第二次已清理；两次使用同一 session。
func TestStaleItemsRetriesAfter401(t *testing.T) {
	for _, tc := range []struct{ name, body string }{{"item", stale401Body}, {"item-id", stale401BodyB}} {
		t.Run(tc.name, func(t *testing.T) {
			s, u := staleServiceWith(t, staleSequence(staleReply{status: 401, body: tc.body, contentType: "application/json"}, staleOK))
			w := testRequest(s, "POST", "/v1/responses", staleRequestBody)
			if w.Code != 200 || w.Body.String() != staleOK.body {
				t.Fatalf("status=%d body=%s, want 200 与第二次应答", w.Code, w.Body.String())
			}
			assertStaleBodies(t, u, staleRequestBody, staleCleanedBody)
			headers := u.sentHeaders("/responses")
			if headers[0].Get("Authorization") != headers[1].Get("Authorization") {
				t.Fatalf("重试应使用同一 session: %q vs %q", headers[0].Get("Authorization"), headers[1].Get("Authorization"))
			}
			if n := u.sessionExchanges(); n != 1 {
				t.Fatalf("session 交换 %d 次, want 1（重试不得重新交换 session）", n)
			}
		})
	}
}

// TestStaleItemsRetriesAfter400EncryptedContent 400 "encrypted content could not be verified" 同样清理后重试一次。
func TestStaleItemsRetriesAfter400EncryptedContent(t *testing.T) {
	s, u := staleServiceWith(t, staleSequence(staleReply{status: 400, body: stale400Body, contentType: "application/json"}, staleOK))
	w := testRequest(s, "POST", "/v1/responses", staleRequestBody)
	if w.Code != 200 || w.Body.String() != staleOK.body {
		t.Fatalf("status=%d body=%s, want 200 与第二次应答", w.Code, w.Body.String())
	}
	assertStaleBodies(t, u, staleRequestBody, staleCleanedBody)
}

// TestStaleItemsRetryViaBothResponsesRoutes /responses 与 /v1/responses 两个入口都会重试。
func TestStaleItemsRetryViaBothResponsesRoutes(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/responses"} {
		t.Run(path, func(t *testing.T) {
			s, u := staleServiceWith(t, staleSequence(staleReply{status: 401, body: stale401Body}, staleOK))
			if w := testRequest(s, "POST", path, staleRequestBody); w.Code != 200 {
				t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
			}
			assertStaleBodies(t, u, staleRequestBody, staleCleanedBody)
		})
	}
}

// TestStaleItemsRetryStreamsSSE 重试得到的 SSE 响应照常透传给客户端：状态、Content-Type 与事件内容。
func TestStaleItemsRetryStreamsSSE(t *testing.T) {
	const events = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	s, u := staleServiceWith(t, staleSequence(
		staleReply{status: 401, body: stale401Body, contentType: "application/json"},
		staleReply{status: 200, body: events, contentType: "text/event-stream"},
	))
	w := testRequest(s, "POST", "/v1/responses", staleRequestBody)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type=%q, want text/event-stream", got)
	}
	if w.Body.String() != events {
		t.Fatalf("SSE 内容应原样透传:\n got=%q\nwant=%q", w.Body.String(), events)
	}
	assertStaleBodies(t, u, staleRequestBody, staleCleanedBody)
}

// TestStaleItemsRetryAlsoDropsLoneToolChoice 请求带孤立 tool_choice（无 tools）时，首次与重试的请求体都不含 tool_choice。
func TestStaleItemsRetryAlsoDropsLoneToolChoice(t *testing.T) {
	const body = `{"model":"gpt-4.1","tool_choice":"auto","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"abc"},{"type":"message","id":"msg_1","role":"user","content":"hi"}]}`
	s, u := staleServiceWith(t, staleSequence(staleReply{status: 401, body: stale401Body}, staleOK))
	if w := testRequest(s, "POST", "/v1/responses", body); w.Code != 200 {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	got := u.sent("/responses")
	if len(got) != 2 {
		t.Fatalf("上游 /responses 收到 %d 次请求, want 2", len(got))
	}
	for i, b := range got {
		if _, ok := decodeNumberMap(t, b)["tool_choice"]; ok {
			t.Fatalf("第 %d 次请求体仍含孤立的 tool_choice: %s", i+1, b)
		}
	}
	want := decodeNumberMap(t, []byte(`{"model":"gpt-4.1","input":[{"type":"message","role":"user","content":"hi"}]}`))
	if !reflect.DeepEqual(decodeNumberMap(t, got[1]), want) {
		t.Fatalf("重试请求体不符:\n got=%s", got[1])
	}
}

// TestStaleItemsRetryAlsoDropsOutputReplay 请求带 output token replay 内容时，首次与重试的请求体都已去掉
// include 中的 output.encrypted_content 与 message 项的 encrypted_content。
func TestStaleItemsRetryAlsoDropsOutputReplay(t *testing.T) {
	const body = `{"model":"gpt-4.1","include":["reasoning.encrypted_content","output.encrypted_content"],"input":[{"type":"reasoning","id":"rs_1","encrypted_content":"abc"},{"type":"message","id":"msg_1","role":"assistant","content":"hi","encrypted_content":"sealed"}]}`
	s, u := staleServiceWith(t, staleSequence(staleReply{status: 401, body: stale401Body}, staleOK))
	if w := testRequest(s, "POST", "/v1/responses", body); w.Code != 200 {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	got := u.sent("/responses")
	if len(got) != 2 {
		t.Fatalf("上游 /responses 收到 %d 次请求, want 2", len(got))
	}
	first := decodeNumberMap(t, []byte(`{"model":"gpt-4.1","include":["reasoning.encrypted_content"],"input":[{"type":"reasoning","id":"rs_1","encrypted_content":"abc"},{"type":"message","id":"msg_1","role":"assistant","content":"hi"}]}`))
	if !reflect.DeepEqual(decodeNumberMap(t, got[0]), first) {
		t.Fatalf("首次请求体不符:\n got=%s", got[0])
	}
	retry := decodeNumberMap(t, []byte(`{"model":"gpt-4.1","include":["reasoning.encrypted_content"],"input":[{"type":"message","role":"assistant","content":"hi"}]}`))
	if !reflect.DeepEqual(decodeNumberMap(t, got[1]), retry) {
		t.Fatalf("重试请求体不符:\n got=%s", got[1])
	}
}

// TestStaleItemsKeepsToolChoiceWhenToolsDeclared 声明了 tools 时 tool_choice 在重试中保留。
func TestStaleItemsKeepsToolChoiceWhenToolsDeclared(t *testing.T) {
	const body = `{"model":"gpt-4.1","tools":[{"type":"function","name":"f"}],"tool_choice":"auto","input":[{"type":"message","id":"msg_1","role":"user","content":"hi"}]}`
	s, u := staleServiceWith(t, staleSequence(staleReply{status: 401, body: stale401Body}, staleOK))
	if w := testRequest(s, "POST", "/v1/responses", body); w.Code != 200 {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	got := u.sent("/responses")
	if len(got) != 2 {
		t.Fatalf("上游 /responses 收到 %d 次请求, want 2", len(got))
	}
	want := decodeNumberMap(t, []byte(`{"model":"gpt-4.1","tools":[{"type":"function","name":"f"}],"tool_choice":"auto","input":[{"type":"message","role":"user","content":"hi"}]}`))
	if !reflect.DeepEqual(decodeNumberMap(t, got[1]), want) {
		t.Fatalf("重试请求体不符:\n got=%s", got[1])
	}
}

// TestStaleItemsNoRetryWhenNothingToClean 输入没有可清理内容：不重试（上游 1 次）。
// 最终响应是失效项 401 时客户端收到 500（正文不变，避免入口按 401 频率封禁）；失效项 400 仍原样 400。
func TestStaleItemsNoRetryWhenNothingToClean(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		wantStatus int // 客户端收到的状态码
		body       string
		input      string
	}{
		{"401-no-ids", 401, 500, stale401Body, staleUncleanableBody},
		{"401-string-input", 401, 500, stale401BodyB, `{"model":"gpt-4.1","input":"hi"}`},
		{"400-no-ids", 400, 400, stale400Body, staleUncleanableBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, u := staleServiceWith(t, staleSequence(staleReply{status: tc.status, body: tc.body, contentType: "application/json"}, staleOK))
			w := testRequest(s, "POST", "/v1/responses", tc.input)
			if w.Code != tc.wantStatus || w.Body.String() != tc.body {
				t.Fatalf("status=%d body=%s, want %d 与上游正文 %s", w.Code, w.Body.String(), tc.wantStatus, tc.body)
			}
			if got := u.sent("/responses"); len(got) != 1 {
				t.Fatalf("上游 /responses 收到 %d 次请求, want 1", len(got))
			}
		})
	}
}

// TestStaleItemsStreamFlagAlsoConverts401To500 请求带 "stream":true 时同样适用：重试后仍是失效项 401，
// 客户端收到 500 与第二次正文（stream 标志不影响 401→500 转换）。
func TestStaleItemsStreamFlagAlsoConverts401To500(t *testing.T) {
	body := strings.Replace(staleRequestBody, `"model":"gpt-4.1"`, `"model":"gpt-4.1","stream":true`, 1)
	s, u := staleServiceWith(t, staleSequence(
		staleReply{status: 401, body: stale401Body},
		staleReply{status: 401, body: stale401BodyB},
	))
	w := testRequest(s, "POST", "/v1/responses", body)
	if w.Code != 500 || w.Body.String() != stale401BodyB {
		t.Fatalf("status=%d body=%s, want 500 与第二次正文 %s", w.Code, w.Body.String(), stale401BodyB)
	}
	assertStaleBodies(t, u, body, strings.Replace(staleCleanedBody, `"model":"gpt-4.1"`, `"model":"gpt-4.1","stream":true`, 1))
}

// TestStaleItemsRetryFailureReturnsFinalError 重试后仍失败：上游收到 2 次，客户端收到第二次的正文，且不再重试。
// 第二次是失效项 401 时状态改为 500（正文不变）；失效项 400 仍原样 400。
func TestStaleItemsRetryFailureReturnsFinalError(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second staleReply
		wantStatus    int // 客户端收到的状态码
	}{
		{"401-then-401", staleReply{status: 401, body: stale401Body}, staleReply{status: 401, body: stale401BodyB}, 500},
		{"401-then-400", staleReply{status: 401, body: stale401Body}, staleReply{status: 400, body: stale400Body}, 400},
		{"400-then-401", staleReply{status: 400, body: stale400Body}, staleReply{status: 401, body: stale401Body}, 500},
		{"400-then-400", staleReply{status: 400, body: stale400Body}, staleReply{status: 400, body: stale400Body}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, u := staleServiceWith(t, staleSequence(tc.first, tc.second, staleOK))
			w := testRequest(s, "POST", "/v1/responses", staleRequestBody)
			if w.Code != tc.wantStatus || w.Body.String() != tc.second.body {
				t.Fatalf("status=%d body=%s, want %d 与第二次正文 %s", w.Code, w.Body.String(), tc.wantStatus, tc.second.body)
			}
			assertStaleBodies(t, u, staleRequestBody, staleCleanedBody)
		})
	}
}

// TestStaleItemsRetryReturnsOtherErrorsAsIs 重试得到的其他错误（如 500）按普通错误原样返回。
func TestStaleItemsRetryReturnsOtherErrorsAsIs(t *testing.T) {
	s, u := staleServiceWith(t, staleSequence(
		staleReply{status: 401, body: stale401Body},
		staleReply{status: 500, body: `{"error":{"message":"boom"}}`},
	))
	w := testRequest(s, "POST", "/v1/responses", staleRequestBody)
	if w.Code != 500 || w.Body.String() != `{"error":{"message":"boom"}}` {
		t.Fatalf("status=%d body=%s, want 500 与第二次正文", w.Code, w.Body.String())
	}
	assertStaleBodies(t, u, staleRequestBody, staleCleanedBody)
}

// TestStaleItemsOnlyRetriesResponsesPath /chat/completions 与 /v1/messages 即使返回同样正文也不重试，原样透传。
func TestStaleItemsOnlyRetriesResponsesPath(t *testing.T) {
	for _, tc := range []struct {
		name, path, upstream, body string
		status                     int
		reply                      string
	}{
		{"chat-401", "/v1/chat/completions", "/chat/completions", `{"model":"gpt-4.1","messages":[{"role":"user","id":"m1","content":"hi"}]}`, 401, stale401Body},
		{"chat-400", "/v1/chat/completions", "/chat/completions", `{"model":"gpt-4.1","messages":[{"role":"user","id":"m1","content":"hi"}]}`, 400, stale400Body},
		{"messages-401", "/v1/messages", "/v1/messages", `{"model":"gpt-4.1","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, 401, stale401Body},
		{"messages-400", "/v1/messages", "/v1/messages", `{"model":"gpt-4.1","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, 400, stale400Body},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, u := staleServiceWith(t, func(string, int, []byte) staleReply {
				return staleReply{status: tc.status, body: tc.reply, contentType: "application/json"}
			})
			w := testRequest(s, "POST", tc.path, tc.body)
			if w.Code != tc.status || w.Body.String() != tc.reply {
				t.Fatalf("status=%d body=%s, want 原样返回 %d %s", w.Code, w.Body.String(), tc.status, tc.reply)
			}
			if got := u.sent(tc.upstream); len(got) != 1 {
				t.Fatalf("上游 %s 收到 %d 次请求, want 1（非 /responses 不重试）", tc.upstream, len(got))
			}
		})
	}
}

// ---- Pool 层：失效项错误不计账号错误、不冷却、不写风控记录 ----

// stalePoolUpstream 记录 Pool 测试中上游 /responses 收到的请求体，并按序号给出应答（序号全局递增，从 1 开始）。
type stalePoolUpstream struct {
	mu      sync.Mutex
	bodies  [][]byte
	respond func(n int, body []byte) staleReply
}

func (u *stalePoolUpstream) sent() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies...)
}

// stalePoolFixture 创建单账号 a 的托管池（开启会话亲和与风控表）：/responses 由 respond 应答，其余请求交给测试上游。
func stalePoolFixture(t *testing.T, respond func(n int, body []byte) staleReply) (*cooldownFixture, *Pool, *stalePoolUpstream) {
	t.Helper()
	f := newCooldownFixture(t, "a")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	u := &stalePoolUpstream{respond: respond}
	f.config.Client = cooldownWrap(f.up, func(r *http.Request) (*http.Response, bool, error) {
		if r.URL.Path != "/responses" {
			return nil, false, nil
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, true, err
		}
		u.mu.Lock()
		u.bodies = append(u.bodies, data)
		n := len(u.bodies)
		u.mu.Unlock()
		reply := u.respond(n, data)
		contentType := reply.contentType
		if contentType == "" {
			contentType = "application/json"
		}
		return cooldownHTTPResponse(r, reply.status, contentType, io.NopCloser(strings.NewReader(reply.body))), true, nil
	})
	return f, f.start(), u
}

// staleSend 发送带 X-Session-ID 会话信号的 /v1/responses 请求。
func staleSend(p *Pool, session, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Session-ID", session)
	w := httptest.NewRecorder()
	withTestAPIKey(p.Handler()).ServeHTTP(w, r)
	return w
}

// staleSessionKey 计算 staleSend 发送的请求对应的会话键。
func staleSessionKey(t *testing.T, session, body string) string {
	t.Helper()
	r := affinityRequest("/v1/responses", testAPIKey, "")
	r.Header.Set("X-Session-ID", session)
	key := sessionKey(r, []byte(body), "/responses")
	affinityAssertKeyShape(t, key)
	return key
}

// staleAssertNotPenalized 断言账号 a 未冷却、无错误计数、会话键 session 没有风控记录且风控文件未写入。
func staleAssertNotPenalized(t *testing.T, f *cooldownFixture, p *Pool, session, body string) {
	t.Helper()
	p.WaitCooldownEffects()
	view := cooldownView(t, p, "a")
	if view.CooldownUntil != nil {
		t.Fatalf("账号不应冷却, 冷却至 %s 原因 %q", view.CooldownUntil, view.CooldownReason)
	}
	if view.Stats.Errors != 0 {
		t.Fatalf("账号错误计数 = %d, want 0", view.Stats.Errors)
	}
	if status, ok := p.risk.lookup(staleSessionKey(t, session, body), time.Now()); ok {
		t.Fatalf("不应写入风控记录, 得到状态码 %d", status)
	}
	if _, err := os.Stat(riskFilePath(f)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("不应写入风控记录文件: stat error = %v", err)
	}
}

// TestStalePoolRetrySuccessDoesNotCoolOrRecord 失效项 401 经重试成功：客户端 200，账号不冷却、无错误计数、无风控记录，
// 之后同一会话与其他会话的请求仍可使用该账号。
func TestStalePoolRetrySuccessDoesNotCoolOrRecord(t *testing.T) {
	f, p, u := stalePoolFixture(t, func(n int, _ []byte) staleReply {
		if n == 1 {
			return staleReply{status: 401, body: stale401Body}
		}
		return staleOK
	})
	w := staleSend(p, "stale-retry", staleRequestBody)
	if w.Code != 200 || w.Body.String() != staleOK.body {
		t.Fatalf("status=%d body=%s, want 200 与第二次应答", w.Code, w.Body.String())
	}
	got := u.sent()
	if len(got) != 2 || string(got[0]) != staleRequestBody {
		t.Fatalf("上游收到 %d 次请求, want 2 且首次为原始请求体", len(got))
	}
	if !reflect.DeepEqual(decodeNumberMap(t, got[1]), decodeNumberMap(t, []byte(staleCleanedBody))) {
		t.Fatalf("重试请求体应已清理: %s", got[1])
	}
	staleAssertNotPenalized(t, f, p, "stale-retry", staleRequestBody)

	// 账号仍可用：同一会话与其他会话都能继续请求（此时上游恒返回 200）。
	for _, session := range []string{"stale-retry", "stale-other"} {
		if w := staleSend(p, session, staleUncleanableBody); w.Code != 200 {
			t.Fatalf("会话 %s 的后续请求 status=%d body=%s, want 200", session, w.Code, w.Body.String())
		}
	}
	staleAssertNotPenalized(t, f, p, "stale-retry", staleRequestBody)
}

// TestStalePoolRetry400DoesNotPenalize 400 encrypted content 变体经重试成功，同样不冷却、不记录。
func TestStalePoolRetry400DoesNotPenalize(t *testing.T) {
	f, p, u := stalePoolFixture(t, func(n int, _ []byte) staleReply {
		if n == 1 {
			return staleReply{status: 400, body: stale400Body}
		}
		return staleOK
	})
	if w := staleSend(p, "stale-400", staleRequestBody); w.Code != 200 {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if got := u.sent(); len(got) != 2 {
		t.Fatalf("上游收到 %d 次请求, want 2", len(got))
	}
	staleAssertNotPenalized(t, f, p, "stale-400", staleRequestBody)
}

// TestStalePoolUnretriable401Returns500WithoutCooldown 无法重试（输入没有可清理内容）时客户端收到 500 而非 401
// （入口按 401 频率封禁 IP），正文仍是上游正文；但不冷却账号、不写风控记录；
// 即使连续多次（超过连续错误阈值）账号仍可用，同一会话不会被风控拦截。
func TestStalePoolUnretriable401Returns500WithoutCooldown(t *testing.T) {
	failing := true
	var mu sync.Mutex
	f, p, u := stalePoolFixture(t, func(int, []byte) staleReply {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return staleReply{status: 401, body: stale401Body}
		}
		return staleOK
	})
	const rounds = cooldownStreakLimit + 1
	for i := 0; i < rounds; i++ {
		w := staleSend(p, "stale-pass", staleUncleanableBody)
		if w.Code != 500 || w.Body.String() != stale401Body {
			t.Fatalf("第 %d 次 status=%d body=%s, want 500 与上游正文 %s", i+1, w.Code, w.Body.String(), stale401Body)
		}
		staleAssertNotPenalized(t, f, p, "stale-pass", staleUncleanableBody)
	}
	if got := u.sent(); len(got) != rounds {
		t.Fatalf("上游收到 %d 次请求, want %d（无法清理时不重试）", len(got), rounds)
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	if w := staleSend(p, "stale-pass", staleUncleanableBody); w.Code != 200 {
		t.Fatalf("恢复后同一会话 status=%d body=%s, want 200（账号可用、会话未被风控）", w.Code, w.Body.String())
	}
}

// TestStalePoolRetryStillFailsDoesNotCoolOrRecord 重试后仍是失效项 401：客户端收到 500 与第二次正文
// （不把 401 返回给客户端，避免入口按 401 频率封禁），账号不冷却、不写风控记录，后续请求仍可用。
func TestStalePoolRetryStillFailsDoesNotCoolOrRecord(t *testing.T) {
	var mu sync.Mutex
	failing := true
	f, p, u := stalePoolFixture(t, func(n int, _ []byte) staleReply {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case !failing:
			return staleOK
		case n == 1:
			return staleReply{status: 401, body: stale401Body}
		default:
			return staleReply{status: 401, body: stale401BodyB}
		}
	})
	w := staleSend(p, "stale-fail", staleRequestBody)
	if w.Code != 500 || w.Body.String() != stale401BodyB {
		t.Fatalf("status=%d body=%s, want 500 与第二次正文 %s", w.Code, w.Body.String(), stale401BodyB)
	}
	if got := u.sent(); len(got) != 2 {
		t.Fatalf("上游收到 %d 次请求, want 2", len(got))
	}
	staleAssertNotPenalized(t, f, p, "stale-fail", staleRequestBody)

	mu.Lock()
	failing = false
	mu.Unlock()
	if w := staleSend(p, "stale-fail", staleRequestBody); w.Code != 200 {
		t.Fatalf("恢复后同一会话 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
}

// TestStalePoolPlainUnauthorizedStillCoolsAndRecords 对照：/responses 上不含失效项措辞的普通 401 仍立即冷却并写风控记录，且不重试。
func TestStalePoolPlainUnauthorizedStillCoolsAndRecords(t *testing.T) {
	const plain = `{"error":{"message":"bad credentials"}}`
	f, p, u := stalePoolFixture(t, func(int, []byte) staleReply {
		return staleReply{status: 401, body: plain}
	})
	w := staleSend(p, "plain-401", staleRequestBody)
	if w.Code != 401 || w.Body.String() != plain {
		t.Fatalf("status=%d body=%s, want 原样返回 401", w.Code, w.Body.String())
	}
	if got := u.sent(); len(got) != 1 {
		t.Fatalf("上游收到 %d 次请求, want 1（普通 401 不重试）", len(got))
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "401" {
		t.Fatalf("冷却状态 until=%v reason=%q, want 立即冷却且原因 401", v.CooldownUntil, v.CooldownReason)
	}
	riskMustHit(t, p.risk, staleSessionKey(t, "plain-401", staleRequestBody), time.Now(), http.StatusUnauthorized)
	if file := riskReadFile(t, riskFilePath(f)); len(file.Blocks) != 1 {
		t.Fatalf("风控记录文件条数 = %d, want 1", len(file.Blocks))
	}
}
