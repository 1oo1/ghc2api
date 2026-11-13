package copilot

// 本文件覆盖 Copilot 的 Grok 模型在 /responses 上的工具适配的端到端行为（请求改写 groktools.go 与回复还原 grokreply.go），
// 全部经 Service 的 HTTP 入口，上游是假 Copilot，不碰真实账号与网络。
//
// 期望行为（对应上游 magpie 的 TestCopilotGrokTakesCustomToolsAsFunctions、TestCopilotGrokTakesNamespacesFlat 与 nsTidy 的回复还原）：
//   - 对 Grok 模型（取最后一个 "/" 之后小写以 grok 开头）的 /responses 请求，转发前改写：自定义工具当 function、namespace 摊平、
//     根联合合并为 object，历史里的调用同样转换。假 Copilot 对未改写的这类请求回裸的 422 或纯文本 422，改写后请求成功。
//   - 请求有 calls（有改名或自定义调用）且上游回复是 2xx 时，回复里模型对这些工具的调用还原成客户端的形式：
//     自定义调用还原为 custom_tool_call（id fc_→ctc_、input、参数增量改为 input 增量并逐段解码），namespace 调用还原名字与 namespace 并带 encrypted_function_args: []；
//     SSE 与 JSON 回复都改写，其余事件与错误回复逐字节透传；itemids 的 id 统一在其后，客户端看到一致的 id。
//   - GPT（同一 /responses）、Grok 的 /chat/completions 与 /v1/messages、calls 为空的 Grok 请求，请求与回复都逐字节不变。
//   - staleitems 的重试请求体基于改写后的请求体，重试成功的回复同样还原。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// grokTestModels 是假 Copilot 的 /models：Grok 与 GPT 都声明支持三种生成路径，这样同一个 Grok 模型也能验证 /chat/completions 与 /v1/messages 不被改写。
const grokTestModels = `{"data":[` +
	`{"id":"grok-4.7","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/responses","/chat/completions","/v1/messages"],"policy":{"state":"enabled"}},` +
	`{"id":"gpt-4.1","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions","/responses","/v1/messages"],"policy":{"state":"enabled"}}]}`

// grokReply 是假上游对一次生成请求的应答。SSE 的 chunks 逐块写出并 flush（模拟流式分块）；
// 非 SSE 应答带准确的 Content-Length，这样回复被改写后长度变化却没有更新头部的实现会被发现。
type grokReply struct {
	status      int // 0 表示 200
	contentType string
	headers     map[string]string // 其他应答头
	chunks      []string
}

// grokUpstream 是假 Copilot：记录收到的生成请求体，按路径、序号（从 1 开始）给出应答。
type grokUpstream struct {
	mu      sync.Mutex
	bodies  map[string][][]byte
	respond func(path string, n int, body []byte) grokReply
}

func (u *grokUpstream) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, grokTestModels)
		case "/responses", "/chat/completions", "/v1/messages":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("读取上游请求体失败: %v", err)
			}
			u.mu.Lock()
			u.bodies[r.URL.Path] = append(u.bodies[r.URL.Path], data)
			n := len(u.bodies[r.URL.Path])
			u.mu.Unlock()
			reply := u.respond(r.URL.Path, n, data)
			if reply.contentType != "" {
				w.Header().Set("Content-Type", reply.contentType)
			}
			for key, value := range reply.headers {
				w.Header().Set(key, value)
			}
			stream := strings.HasPrefix(reply.contentType, "text/event-stream")
			if !stream {
				total := 0
				for _, chunk := range reply.chunks {
					total += len(chunk)
				}
				w.Header().Set("Content-Length", strconv.Itoa(total))
			}
			if reply.status == 0 {
				reply.status = http.StatusOK
			}
			w.WriteHeader(reply.status)
			for _, chunk := range reply.chunks {
				_, _ = io.WriteString(w, chunk)
				if stream {
					w.(http.Flusher).Flush()
				}
			}
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func (u *grokUpstream) sent(path string) [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies[path]...)
}

// grokService 创建使用假 Copilot 的 Service。
func grokService(t *testing.T, respond func(path string, n int, body []byte) grokReply) (*Service, *grokUpstream) {
	t.Helper()
	u := &grokUpstream{bodies: map[string][][]byte{}, respond: respond}
	s := testService(t, Config{APIKey: testAPIKey, Token: "github-secret", Client: testClient(t, u.serve(t))})
	return s, u
}

// grokAlways 返回对每个请求都给同一应答的 respond。
func grokAlways(reply grokReply) func(string, int, []byte) grokReply {
	return func(string, int, []byte) grokReply { return reply }
}

// grokSplit 把 s 切成上游分块写出：size 为 0 不切，小于 0 按 SSE 事件切，大于 0 按字节数切。
func grokSplit(s string, size int) []string {
	switch {
	case size == 0:
		return []string{s}
	case size < 0:
		var chunks []string
		for {
			end := sseEventEnd(s)
			if end < 0 {
				if s != "" {
					chunks = append(chunks, s)
				}
				return chunks
			}
			chunks = append(chunks, s[:end])
			s = s[end:]
		}
	}
	var chunks []string
	for i := 0; i < len(s); i += size {
		chunks = append(chunks, s[i:min(i+size, len(s))])
	}
	return chunks
}

// grokSSEReply 是 200 的 SSE 应答。
func grokSSEReply(body string, size int) grokReply {
	return grokReply{contentType: "text/event-stream", chunks: grokSplit(body, size)}
}

// grokJSONReply 是 JSON 应答。
func grokJSONReply(status int, body string) grokReply {
	return grokReply{status: status, contentType: "application/json", chunks: []string{body}}
}

// grokBuildSSE 拼接 SSE 事件：eventLines 为 true 时带 event 行，eol 为行尾，事件以空行结束。
func grokBuildSSE(events []itemIDEvent, eol string, eventLines bool) string {
	var b strings.Builder
	for _, e := range events {
		if eventLines {
			b.WriteString("event: " + e.typ + eol)
		}
		b.WriteString("data: " + e.data + eol + eol)
	}
	return b.String()
}

// grokPlainStream 是不含任何工具调用的上游流，客户端应逐字节收到它。
func grokPlainStream() string {
	return grokBuildSSE([]itemIDEvent{
		{"response.created", `{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"DONE"}`},
		{"response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":1}}}`},
	}, "\n", true)
}

// grokPost 向 Service 发请求，要求状态码为 200，返回响应。
func grokPost(t *testing.T, s *Service, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := testRequest(s, http.MethodPost, path, body)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s: status=%d body=%s, want 200", path, w.Code, w.Body.String())
	}
	return w
}

// ---------------------------------------------------------------------------------------------------------------------
// 请求侧：假 Copilot 拒绝 Grok 不接受的内容

// grokRefuses 判断请求体是否含 Copilot 背后的 Grok 会拒绝的内容：custom 或 namespace 工具、参数根节点是联合的 function、
// type 为 custom 或带 namespace 的 tool_choice、历史里的 custom_tool_call 及其输出、带 namespace 的 function_call。
func grokRefuses(body []byte) bool {
	var request struct {
		Tools      []map[string]any `json:"tools"`
		ToolChoice map[string]any   `json:"tool_choice"`
		Input      []map[string]any `json:"input"`
	}
	_ = json.Unmarshal(body, &request) // input 可能是字符串，此时只看 tools
	for _, tool := range request.Tools {
		switch tool["type"] {
		case "custom", "namespace":
			return true
		case "function":
			if ps, ok := tool["parameters"].(map[string]any); ok {
				for _, key := range []string{"anyOf", "oneOf", "allOf"} {
					if _, has := ps[key]; has {
						return true
					}
				}
			}
		}
	}
	if request.ToolChoice != nil {
		if _, has := request.ToolChoice["namespace"]; has || request.ToolChoice["type"] == "custom" {
			return true
		}
	}
	for _, item := range request.Input {
		switch item["type"] {
		case "custom_tool_call", "custom_tool_call_output":
			return true
		case "function_call":
			if _, has := item["namespace"]; has {
				return true
			}
		}
	}
	return false
}

// grokRefusal 是 Copilot 前面的 Grok 给出的裸错误：JSON 的 422、纯文本的 422，或只有 "Bad Request" 的 400。
func grokRefusal(style int) grokReply {
	switch style % 3 {
	case 0:
		return grokJSONReply(http.StatusUnprocessableEntity, `{"error":{"message":"422 Unprocessable Entity: Unprocessable Entity","type":"invalid_request_error"}}`)
	case 1:
		return grokReply{status: http.StatusUnprocessableEntity, contentType: "text/plain", chunks: []string{"422 Unprocessable Entity"}}
	}
	return grokJSONReply(http.StatusBadRequest, `{"error":{"message":"Bad Request","type":"invalid_request_error"}}`)
}

// grokRefusingUpstream 返回对 Grok 拒绝的内容回裸错误、否则回无工具调用的流的 respond。
func grokRefusingUpstream(style int) func(string, int, []byte) grokReply {
	return func(_ string, _ int, body []byte) grokReply {
		if grokRefuses(body) {
			return grokRefusal(style)
		}
		return grokSSEReply(grokPlainStream(), 0)
	}
}

// grokAsGPT 把请求体里的 Grok 模型换成 GPT。
func grokAsGPT(body string) string {
	return strings.Replace(body, `"model":"grok-4.7"`, `"model":"gpt-4.1"`, 1)
}

// TestGrokRequestAcceptedAfterRewrite 对 Grok 的 /responses 请求含自定义工具、namespace 工具、根联合参数、tool_choice 指向自定义工具、
// 历史里的自定义调用或 namespace 调用时，假 Copilot 对未改写的请求回裸的 422/400；改写后请求成功，客户端收到上游的流。
// 同一路径上的 GPT 不改写：转发的请求体与客户端发送的逐字节一致。
func TestGrokRequestAcceptedAfterRewrite(t *testing.T) {
	const noop = `{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}}`
	for i, tc := range []struct{ name, body string }{
		{"custom-tool", `{"model":"grok-4.7","stream":true,"store":false,"reasoning":{"effort":"low"},"input":[{"role":"user","content":"hi check current time"}],"tools":[` +
			`{"type":"custom","name":"apply_patch","description":"Apply a patch.","format":{"type":"text"}},` +
			`{"type":"function","name":"shell","description":"Run a command.","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}]}`},
		{"namespace-tool", `{"model":"grok-4.7","stream":true,"store":false,"input":[{"role":"user","content":"hi"}],"tools":[` +
			`{"type":"namespace","name":"mcp__codex_app","description":"Codex app.","tools":[{"type":"function","name":"move_thread_to_project","description":"Move.","parameters":{"type":"object","properties":{"projectId":{"type":"string"}}}}]},` + noop + `]}`},
		{"root-union", `{"model":"grok-4.7","stream":true,"store":false,"input":[{"role":"user","content":"hi"}],"tools":[` +
			`{"type":"function","name":"automation_update","description":"Update.","parameters":{"oneOf":[` +
			`{"type":"object","properties":{"mode":{"type":"string","enum":["a"]}},"required":["mode"]},` +
			`{"type":"object","properties":{"mode":{"type":"string","enum":["b"]}},"required":["mode"]}]}}]}`},
		{"root-all-of", `{"model":"grok-4.7","stream":true,"store":false,"input":[{"role":"user","content":"hi"}],"tools":[` +
			`{"type":"function","name":"merge","description":"Merge.","parameters":{"allOf":[` +
			`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"integer"}}}]}}]}`},
		{"tool-choice-custom", `{"model":"grok-4.7","stream":true,"input":"hi","tools":[{"type":"custom","name":"apply_patch","description":"d"}],` +
			`"tool_choice":{"type":"custom","name":"apply_patch"}}`},
		{"history-custom-call", `{"model":"grok-4.7","stream":true,"input":[{"role":"user","content":"patch"},` +
			`{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch\n*** End Patch","status":"completed"},` +
			`{"type":"custom_tool_call_output","id":"cto_1","call_id":"call_1","output":"Done!"}]}`},
		{"history-namespaced-call", `{"model":"grok-4.7","stream":true,"input":[{"role":"user","content":"title"},` +
			`{"type":"function_call","call_id":"call_1","name":"set_thread_title","namespace":"codex_app","arguments":"{\"title\":\"a\"}"},` +
			`{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`},
		{"codex-turn", grokCodexRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/responses", "/v1/responses"} {
				s, up := grokService(t, grokRefusingUpstream(i))
				w := grokPost(t, s, path, tc.body)
				sent := up.sent("/responses")
				if len(sent) != 1 {
					t.Fatalf("%s: 上游 /responses 请求数 = %d, want 1（改写后不应再被拒绝）", path, len(sent))
				}
				if grokRefuses(sent[0]) || bytes.Equal(sent[0], []byte(tc.body)) {
					t.Fatalf("%s: 转发的请求体应已改写:\n%s", path, sent[0])
				}
				if w.Body.String() != grokPlainStream() {
					t.Errorf("%s: 客户端应逐字节收到上游的流:\n got=%q", path, w.Body.String())
				}
				if tc.name == "codex-turn" {
					assertSameJSON(t, sent[0], grokCodexRewritten)
				}
			}

			// 同一 /responses 上的 GPT 原样转发。
			gpt := grokAsGPT(tc.body)
			// GPT 不会被假 Copilot 拒绝：这里只关心转发的字节，所以上游对所有请求都成功。
			s, up := grokService(t, grokAlways(grokSSEReply(grokPlainStream(), 0)))
			w := grokPost(t, s, "/v1/responses", gpt)
			sent := up.sent("/responses")
			if len(sent) != 1 || !bytes.Equal(sent[0], []byte(gpt)) {
				t.Fatalf("GPT 的请求体应逐字节转发:\n got=%q\nwant=%s", sent, gpt)
			}
			if w.Body.String() != grokPlainStream() {
				t.Errorf("GPT 的回复应逐字节透传: %q", w.Body.String())
			}
		})
	}
}

// TestGrokRequestWithLeadingWhitespaceIsRewritten 以空白开头的请求体（JSON 允许）同样改写。
func TestGrokRequestWithLeadingWhitespaceIsRewritten(t *testing.T) {
	s, up := grokService(t, grokRefusingUpstream(0))
	grokPost(t, s, "/v1/responses", "\n  "+grokCodexRequest+"\n")
	sent := up.sent("/responses")
	if len(sent) != 1 {
		t.Fatalf("上游 /responses 请求数 = %d, want 1", len(sent))
	}
	assertSameJSON(t, sent[0], grokCodexRewritten)
}

// ---------------------------------------------------------------------------------------------------------------------
// 回复侧：SSE

// grokStep 是主流中的一个上游事件与客户端应看到的事件。
type grokStep struct {
	up     itemIDEvent
	want   itemIDEvent // 客户端应看到的事件（same 与 drop 时忽略）
	drop   bool        // 事件应整条丢弃（增量解码为空）
	same   bool        // 事件应逐字节不变
	ignore []string    // 比较时忽略事件顶层的这些键（规格没有规定它们是否出现）
}

const (
	// grokPatchArgs 是自定义调用 apply_patch 的完整 arguments：含转义的引号、反斜杠、\u 转义与代理对。
	grokPatchArgs = `{"input":"a\"b\\n \u00e9 \ud83d\ude00 end"}`
	// grokPatchInput 是上面 arguments 里 input 的解码值。
	grokPatchInput = "a\"b\\n é 😀 end"
)

// grokPatchPieces 是上游把 grokPatchArgs 切成的参数增量：第一段还没读到 input 字符串，其余各段在转义、\u 与代理对中间被切开。
var grokPatchPieces = []string{`{"input":`, `"a\`, `"b\\n \u00`, `e9 \ud83d`, `\ude00 end"}`}

// grokPatchDeltas 是各段增量应解码出的文本，空串表示该事件被丢弃；拼起来等于 grokPatchInput。
var grokPatchDeltas = []string{"", "a", "\"b\\n ", "é ", "😀 end"}

// grokTurn 是 Grok 一轮回复的上游事件序列（对应 grokCodexRequest）：一条消息、一个自定义调用 apply_patch、
// 一个 namespace 调用 codex_app__set_thread_title、一个普通调用 shell，最后 response.completed。
// vary 为 true 时模拟 Copilot 在同一输出项的每个事件里换一个新 id；
// 为 false 时 id 一致。客户端应看到的事件与 vary 无关：自定义调用统一为 ctc_a1，namespace 调用统一为 fc_b1。
func grokTurn(vary bool) []grokStep {
	id := func(base string, n int) string {
		if vary {
			return base + strconv.Itoa(n)
		}
		return base + "1"
	}
	q := itemIDQuote
	oddID := "fc_a1"
	if vary {
		oddID = "Zm9v+/Zm9v=="
	}
	const msgItem = `{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Working","annotations":[]}]}`
	const shellItem = `{"id":"fc_c1","type":"function_call","status":"completed","call_id":"call_s1","name":"shell","arguments":"{\"cmd\":\"ls\"}"}`
	const titleArgs = `"{\"title\":\"x\"}"`
	const usage = `{"input_tokens":12,"output_tokens":1.0e1,"total_tokens":12345678901234567890}`

	steps := []grokStep{
		{up: itemIDEvent{"response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","output":[]}}`}, same: true},
		// 下面几个事件故意用非紧凑写法与转义（空格、\/、\u0026）：被重新编码就会变样，应逐字节不变。
		{up: itemIDEvent{"response.output_item.added", `{"type": "response.output_item.added", "sequence_number": 1, "output_index": 0, "item": {"id": "msg_1", "type": "message", "status": "in_progress", "role": "assistant", "content": []}}`}, same: true},
		{up: itemIDEvent{"response.output_text.delta", `{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"Work\/ing <b> \u0026 more"}`}, same: true},
		{up: itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":` + msgItem + `}`}, same: true},

		// 输出项 1：自定义调用 apply_patch。
		{
			up:   itemIDEvent{"response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":4,"output_index":1,"item":{"id":"%s","type":"function_call","status":"in_progress","call_id":"call_p1","name":"apply_patch","arguments":""}}`, id("fc_a", 1))},
			want: itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":4,"output_index":1,"item":{"id":"ctc_a1","type":"custom_tool_call","status":"in_progress","call_id":"call_p1","name":"apply_patch","input":""}}`},
		},
	}
	for i, piece := range grokPatchPieces {
		itemID := id("fc_a", i+5)
		if i == 2 {
			itemID = oddID // 不带 fc_ 前缀的不透明 id：只能靠 itemids 统一
		}
		steps = append(steps, grokStep{
			up:   itemIDEvent{"response.function_call_arguments.delta", fmt.Sprintf(`{"type":"response.function_call_arguments.delta","sequence_number":%d,"item_id":%s,"output_index":1,"delta":%s}`, i+5, q(itemID), q(piece))},
			want: itemIDEvent{"response.custom_tool_call_input.delta", fmt.Sprintf(`{"type":"response.custom_tool_call_input.delta","sequence_number":%d,"item_id":"ctc_a1","output_index":1,"delta":%s}`, i+5, q(grokPatchDeltas[i]))},
			drop: grokPatchDeltas[i] == "",
		})
	}
	steps = append(steps,
		grokStep{
			up:   itemIDEvent{"response.function_call_arguments.done", fmt.Sprintf(`{"type":"response.function_call_arguments.done","sequence_number":10,"item_id":"%s","output_index":1,"name":"apply_patch","arguments":%s}`, id("fc_a", 10), q(grokPatchArgs))},
			want: itemIDEvent{"response.custom_tool_call_input.done", fmt.Sprintf(`{"type":"response.custom_tool_call_input.done","sequence_number":10,"item_id":"ctc_a1","output_index":1,"input":%s}`, q(grokPatchInput))},
		},
		grokStep{
			up:   itemIDEvent{"response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":11,"output_index":1,"item":{"id":"%s","type":"function_call","status":"completed","call_id":"call_p1","name":"apply_patch","arguments":%s}}`, id("fc_a", 11), q(grokPatchArgs))},
			want: itemIDEvent{"response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":11,"output_index":1,"item":{"id":"ctc_a1","type":"custom_tool_call","status":"completed","call_id":"call_p1","name":"apply_patch","input":%s}}`, q(grokPatchInput))},
		},

		// 输出项 2：namespace 调用 codex_app__set_thread_title。
		grokStep{
			up:   itemIDEvent{"response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":12,"output_index":2,"item":{"id":"%s","type":"function_call","status":"in_progress","call_id":"call_t1","name":"codex_app__set_thread_title","arguments":""}}`, id("fc_b", 1))},
			want: itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":12,"output_index":2,"item":{"id":"fc_b1","type":"function_call","status":"in_progress","call_id":"call_t1","name":"set_thread_title","namespace":"codex_app","encrypted_function_args":[],"arguments":""}}`},
		},
		grokStep{
			up:   itemIDEvent{"response.function_call_arguments.delta", fmt.Sprintf(`{"type":"response.function_call_arguments.delta","sequence_number":13,"item_id":"%s","output_index":2,"delta":%s}`, id("fc_b", 2), titleArgs)},
			want: itemIDEvent{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","sequence_number":13,"item_id":"fc_b1","output_index":2,"delta":` + titleArgs + `}`},
			same: !vary, // id 一致时 delta 不动
		},
		grokStep{
			up:     itemIDEvent{"response.function_call_arguments.done", fmt.Sprintf(`{"type":"response.function_call_arguments.done","sequence_number":14,"item_id":"%s","output_index":2,"name":"codex_app__set_thread_title","arguments":%s}`, id("fc_b", 3), titleArgs)},
			want:   itemIDEvent{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":14,"item_id":"fc_b1","output_index":2,"name":"set_thread_title","namespace":"codex_app","arguments":` + titleArgs + `}`},
			ignore: []string{"encrypted_function_args"},
		},
		grokStep{
			up:   itemIDEvent{"response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":15,"output_index":2,"item":{"id":"%s","type":"function_call","status":"completed","call_id":"call_t1","name":"codex_app__set_thread_title","arguments":%s}}`, id("fc_b", 4), titleArgs)},
			want: itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":15,"output_index":2,"item":{"id":"fc_b1","type":"function_call","status":"completed","call_id":"call_t1","name":"set_thread_title","namespace":"codex_app","encrypted_function_args":[],"arguments":` + titleArgs + `}}`},
		},

		// 输出项 3：普通调用 shell，不在 calls 中，四个事件逐字节不变。
		grokStep{up: itemIDEvent{"response.output_item.added", `{"type": "response.output_item.added", "sequence_number": 16, "output_index": 3, "item": {"id": "fc_c1", "type": "function_call", "status": "in_progress", "call_id": "call_s1", "name": "shell", "arguments": ""}}`}, same: true},
		grokStep{up: itemIDEvent{"response.function_call_arguments.delta", `{"type": "response.function_call_arguments.delta", "sequence_number": 17, "item_id": "fc_c1", "output_index": 3, "delta": "{\"cmd\":\"ls\"}"}`}, same: true},
		grokStep{up: itemIDEvent{"response.function_call_arguments.done", `{"type": "response.function_call_arguments.done", "sequence_number": 18, "item_id": "fc_c1", "output_index": 3, "name": "shell", "arguments": "{\"cmd\":\"ls\"}"}`}, same: true},
		grokStep{up: itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":19,"output_index":3,"item":` + shellItem + `}`}, same: true},

		grokStep{
			up: itemIDEvent{"response.completed", fmt.Sprintf(`{"type":"response.completed","sequence_number":20,"response":{"id":"resp_1","object":"response","status":"completed","output":[%s,`+
				`{"id":"%s","type":"function_call","status":"completed","call_id":"call_p1","name":"apply_patch","arguments":%s},`+
				`{"id":"%s","type":"function_call","status":"completed","call_id":"call_t1","name":"codex_app__set_thread_title","arguments":%s},%s],"usage":%s}}`,
				msgItem, id("fc_a", 20), q(grokPatchArgs), id("fc_b", 20), titleArgs, shellItem, usage)},
			want: itemIDEvent{"response.completed", fmt.Sprintf(`{"type":"response.completed","sequence_number":20,"response":{"id":"resp_1","object":"response","status":"completed","output":[%s,`+
				`{"id":"ctc_a1","type":"custom_tool_call","status":"completed","call_id":"call_p1","name":"apply_patch","input":%s},`+
				`{"id":"fc_b1","type":"function_call","status":"completed","call_id":"call_t1","name":"set_thread_title","namespace":"codex_app","encrypted_function_args":[],"arguments":%s},%s],"usage":%s}}`,
				msgItem, q(grokPatchInput), titleArgs, shellItem, usage)},
		},
	)
	return steps
}

// grokUpstreamEvents 返回各步的上游事件。
func grokUpstreamEvents(steps []grokStep) []itemIDEvent {
	events := make([]itemIDEvent, len(steps))
	for i, step := range steps {
		events[i] = step.up
	}
	return events
}

// grokWithoutKeys 解码 data 并删去顶层的 keys。
func grokWithoutKeys(t *testing.T, data string, keys []string) map[string]any {
	t.Helper()
	m := decodeNumberJSON(t, data)
	for _, key := range keys {
		delete(m, key)
	}
	return m
}

// assertGrokStream 断言 got 恰好是 steps 期望客户端看到的流：
// 被丢弃的事件不出现；same 的事件逐字节等于上游事件；其余事件的 event 行（若有）与 type 一起改，data 与期望的 JSON 语义一致（数字原文保留）。
func assertGrokStream(t *testing.T, steps []grokStep, got, eol string, eventLines bool) {
	t.Helper()
	events, rest := splitSSE(got)
	if rest != "" {
		t.Fatalf("输出末尾有不完整的事件 %q", rest)
	}
	var wanted []grokStep
	for _, step := range steps {
		if !step.drop {
			wanted = append(wanted, step)
		}
	}
	if len(events) != len(wanted) {
		t.Fatalf("输出 %d 个事件, want %d:\n%s", len(events), len(wanted), got)
	}
	for i, step := range wanted {
		event := events[i]
		if step.same {
			if want := grokBuildSSE([]itemIDEvent{step.up}, eol, eventLines); event != want {
				t.Errorf("事件 %d 应逐字节不变:\n got=%q\nwant=%q", i, event, want)
			}
			continue
		}
		var wantLines []string
		if eventLines {
			wantLines = []string{"event: " + step.want.typ}
		}
		if lines := sseOtherLines(event); !reflect.DeepEqual(lines, wantLines) {
			t.Errorf("事件 %d 的 event 行 = %q, want %q", i, lines, wantLines)
		}
		data, ok := sseData(event)
		if !ok {
			t.Errorf("事件 %d 没有 data 行: %q", i, event)
			continue
		}
		if g, w := grokWithoutKeys(t, data, step.ignore), grokWithoutKeys(t, step.want.data, step.ignore); !reflect.DeepEqual(g, w) {
			t.Errorf("事件 %d 的 data 不一致:\n got=%s\nwant=%s", i, data, step.want.data)
		}
	}
}

// TestGrokReplyStreamRestoresCalls 对 Grok 的有 calls 的请求，2xx 的 SSE 回复里模型的调用还原成客户端的形式：
// 自定义调用变为 custom_tool_call（id ctc_、input），参数增量逐段解码为 custom_tool_call_input.delta（空的丢弃、sequence_number 保留），
// done 变为 custom_tool_call_input.done；namespace 调用还原名字与 namespace 并带 encrypted_function_args: []；response.completed 的 output 同样还原；
// 其余事件逐字节不变。上游在每个事件里换 id 时，客户端看到的 id 一致（Grok 回复改写先于 itemids 的统一）。
// 上游分块不影响结果；event 行（若有）随 type 一起改；CRLF 行尾同样处理。
func TestGrokReplyStreamRestoresCalls(t *testing.T) {
	for _, tc := range []struct {
		name       string
		vary       bool
		eol        string
		eventLines bool
		chunk      int
	}{
		{"event-lines", true, "\n", true, 0},
		{"event-lines-per-event-chunks", true, "\n", true, -1},
		{"event-lines-5-byte-chunks", true, "\n", true, 5},
		{"event-lines-1-byte-chunks", true, "\n", true, 1},
		{"data-only", true, "\n", false, 0},
		{"data-only-7-byte-chunks", true, "\n", false, 7},
		{"crlf", true, "\r\n", true, 7},
		{"consistent-ids", false, "\n", true, 0},
		{"consistent-ids-data-only", false, "\n", false, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := grokTurn(tc.vary)
			stream := grokBuildSSE(grokUpstreamEvents(steps), tc.eol, tc.eventLines)
			s, up := grokService(t, grokAlways(grokSSEReply(stream, tc.chunk)))
			w := grokPost(t, s, "/v1/responses", grokCodexRequest)
			if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
				t.Errorf("Content-Type = %q", got)
			}
			if sent := up.sent("/responses"); len(sent) != 1 {
				t.Fatalf("上游请求数 = %d, want 1", len(sent))
			}
			assertGrokStream(t, steps, w.Body.String(), tc.eol, tc.eventLines)
		})
	}
}

// TestGrokReplyStreamDeltasAddUpToTheInput 客户端收到的 custom_tool_call_input.delta 拼起来等于 done 里的 input，也等于完成项与 response.completed 里的 input。
func TestGrokReplyStreamDeltasAddUpToTheInput(t *testing.T) {
	steps := grokTurn(true)
	stream := grokBuildSSE(grokUpstreamEvents(steps), "\n", true)
	s, _ := grokService(t, grokAlways(grokSSEReply(stream, 3)))
	w := grokPost(t, s, "/v1/responses", grokCodexRequest)
	var deltas []string
	var inputs []string
	events, _ := splitSSE(w.Body.String())
	for _, event := range events {
		data, _ := sseData(event)
		var ev struct {
			Type     string                       `json:"type"`
			Delta    string                       `json:"delta"`
			Input    string                       `json:"input"`
			Item     struct{ Type, Input string } `json:"item"`
			Response struct {
				Output []struct{ Type, Input string } `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case "response.custom_tool_call_input.delta":
			deltas = append(deltas, ev.Delta)
		case "response.custom_tool_call_input.done":
			inputs = append(inputs, ev.Input)
		case "response.output_item.done":
			if ev.Item.Type == "custom_tool_call" {
				inputs = append(inputs, ev.Item.Input)
			}
		case "response.completed":
			for _, item := range ev.Response.Output {
				if item.Type == "custom_tool_call" {
					inputs = append(inputs, item.Input)
				}
			}
		}
	}
	if got := strings.Join(deltas, ""); got != grokPatchInput {
		t.Errorf("增量拼起来 = %q, want %q（各段 %q）", got, grokPatchInput, deltas)
	}
	if len(inputs) != 3 {
		t.Fatalf("done、完成项、completed 里的 input 共 %d 个, want 3: %q", len(inputs), inputs)
	}
	for _, input := range inputs {
		if input != grokPatchInput {
			t.Errorf("input = %q, want %q", input, grokPatchInput)
		}
	}
}

// TestGrokReplyStreamKeepsIDsConsistent 上游在每个事件里给同一输出项换一个新 id 时，客户端看到的每个输出项只有一个 id：
// 自定义调用 ctc_a1（added 里改写后的 id），namespace 调用 fc_b1。
func TestGrokReplyStreamKeepsIDsConsistent(t *testing.T) {
	steps := grokTurn(true)
	stream := grokBuildSSE(grokUpstreamEvents(steps), "\n", true)
	s, _ := grokService(t, grokAlways(grokSSEReply(stream, 0)))
	w := grokPost(t, s, "/v1/responses", grokCodexRequest)
	ids := collectItemIDs(t, w.Body.String())
	for index, want := range map[int]string{0: "msg_1", 1: "ctc_a1", 2: "fc_b1", 3: "fc_c1"} {
		if got := ids[index]; len(got) != 1 || !got[want] {
			t.Errorf("输出项 %d 的 id = %v, want 只有 %s", index, got, want)
		}
	}
}

// TestGrokReplyStreamByOutputIndex 流式事件按 output_index 关联到 added 中被识别的调用：
// 两个自定义调用的参数增量交错到达时各自解码；不在 calls 中的调用、从未 added 的 output_index 的事件不动；
// done 事件不带 name 时不改（也不加 name 与 namespace）；只有 done 没有 added 的自定义调用项仍按名字还原。
func TestGrokReplyStreamByOutputIndex(t *testing.T) {
	q := itemIDQuote
	added := func(seq, index int, id, name string) itemIDEvent {
		return itemIDEvent{"response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":%d,"output_index":%d,"item":{"id":%s,"type":"function_call","call_id":"call_%d","name":%s,"arguments":""}}`, seq, index, q(id), index, q(name))}
	}
	delta := func(seq, index int, id, text string) itemIDEvent {
		return itemIDEvent{"response.function_call_arguments.delta", fmt.Sprintf(`{"type":"response.function_call_arguments.delta","sequence_number":%d,"item_id":%s,"output_index":%d,"delta":%s}`, seq, q(id), index, q(text))}
	}
	custom := func(seq, index int, id, text string) itemIDEvent {
		return itemIDEvent{"response.custom_tool_call_input.delta", fmt.Sprintf(`{"type":"response.custom_tool_call_input.delta","sequence_number":%d,"item_id":%s,"output_index":%d,"delta":%s}`, seq, q(id), index, q(text))}
	}
	steps := []grokStep{
		{up: added(1, 0, "fc_x1", "apply_patch"), want: itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"ctc_x1","type":"custom_tool_call","call_id":"call_0","name":"apply_patch","input":""}}`}},
		{up: added(2, 1, "fc_y1", "exec"), want: itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":2,"output_index":1,"item":{"id":"ctc_y1","type":"custom_tool_call","call_id":"call_1","name":"exec","input":""}}`}},
		{up: added(3, 2, "fc_z1", "shell"), same: true},
		{up: delta(4, 0, "fc_x1", `{"input":"one `), want: custom(4, 0, "ctc_x1", "one ")},
		{up: delta(5, 1, "fc_y1", `{"input":"two `), want: custom(5, 1, "ctc_y1", "two ")},
		{up: delta(6, 2, "fc_z1", `{"input":"not custom `), same: true},
		{up: delta(7, 1, "fc_y1", `more"}`), want: custom(7, 1, "ctc_y1", "more")},
		{up: delta(8, 0, "fc_x1", `\nlines"}`), want: custom(8, 0, "ctc_x1", "\nlines")},
		// 从未 added 的 output_index：不动。
		{up: delta(9, 7, "fc_q1", `{"input":"stray"}`), same: true},
		// 不带 name 的 done：从未 added 的 output_index 不动。
		{up: itemIDEvent{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":10,"item_id":"fc_n1","output_index":5,"arguments":"{}"}`}, same: true},
		// namespace 调用：added 与完成项还原，不带 name 的 done 不动（也不加 name 与 namespace）。
		{
			up:   added(12, 3, "fc_n1", "codex_app__set_thread_title"),
			want: itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":12,"output_index":3,"item":{"id":"fc_n1","type":"function_call","call_id":"call_3","name":"set_thread_title","namespace":"codex_app","encrypted_function_args":[],"arguments":""}}`},
		},
		{up: itemIDEvent{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":13,"item_id":"fc_n1","output_index":3,"arguments":"{}"}`}, same: true},
		// 只有 done 没有 added 的自定义调用项：按名字还原。
		{
			up:   itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":11,"output_index":6,"item":{"id":"fc_w1","type":"function_call","status":"completed","call_id":"call_w","name":"apply_patch","arguments":"{\"input\":\"late\"}"}}`},
			want: itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":11,"output_index":6,"item":{"id":"ctc_w1","type":"custom_tool_call","status":"completed","call_id":"call_w","name":"apply_patch","input":"late"}}`},
		},
	}
	stream := grokBuildSSE(grokUpstreamEvents(steps), "\n", true)
	s, _ := grokService(t, grokAlways(grokSSEReply(stream, 11)))
	w := grokPost(t, s, "/v1/responses", `{"model":"grok-4.7","stream":true,"input":"hi","tools":[`+
		`{"type":"custom","name":"apply_patch","description":"a"},{"type":"custom","name":"exec","description":"e"},`+
		`{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}},`+
		`{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"set_thread_title","parameters":{"type":"object","properties":{}}}]}]}`)
	assertGrokStream(t, steps, w.Body.String(), "\n", true)
}

// TestGrokReplyStreamRestoresIncompleteResponse response.incomplete 事件的 response.output 里的调用同样还原。
func TestGrokReplyStreamRestoresIncompleteResponse(t *testing.T) {
	steps := []grokStep{
		{
			up: itemIDEvent{"response.incomplete", `{"type":"response.incomplete","sequence_number":3,"response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[` +
				`{"id":"fc_1","type":"function_call","status":"incomplete","call_id":"call_p1","name":"apply_patch","arguments":"{\"input\":\"half"},` +
				`{"id":"fc_2","type":"function_call","status":"completed","call_id":"call_t1","name":"codex_app__set_thread_title","arguments":"{}"}]}}`},
			want: itemIDEvent{"response.incomplete", `{"type":"response.incomplete","sequence_number":3,"response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[` +
				`{"id":"ctc_1","type":"custom_tool_call","status":"incomplete","call_id":"call_p1","name":"apply_patch","input":"{\"input\":\"half"},` +
				`{"id":"fc_2","type":"function_call","status":"completed","call_id":"call_t1","name":"set_thread_title","namespace":"codex_app","encrypted_function_args":[],"arguments":"{}"}]}}`},
		},
	}
	stream := grokBuildSSE(grokUpstreamEvents(steps), "\n", true)
	s, _ := grokService(t, grokAlways(grokSSEReply(stream, 0)))
	w := grokPost(t, s, "/v1/responses", grokCodexRequest)
	assertGrokStream(t, steps, w.Body.String(), "\n", true)
}

// TestGrokReplyStreamKeepsOtherEventsVerbatim 注释行、retry 字段、非 JSON 的 data（含 [DONE]）与其他事件在改写时逐字节透传，顺序不变。
func TestGrokReplyStreamKeepsOtherEventsVerbatim(t *testing.T) {
	stream := ": ping\n\n" +
		"retry: 3000\n\n" +
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"output\":[]}}\n\n" +
		"data: not json at all\n\n" +
		"event: response.output_item.added\ndata: {broken\n\n" +
		"event: response.in_progress\ndata: {\"type\": \"response.in_progress\", \"response\": {\"id\": \"r\", \"output\": []}}\n\n" +
		"data: [DONE]\n\n"
	for _, size := range []int{0, 1, 6} {
		s, _ := grokService(t, grokAlways(grokSSEReply(stream, size)))
		w := grokPost(t, s, "/v1/responses", grokCodexRequest)
		if w.Body.String() != stream {
			t.Errorf("分块 %d: 应逐字节透传:\n got=%q\nwant=%q", size, w.Body.String(), stream)
		}
	}
}

// TestGrokReplyStreamIncompleteTailIsVerbatim 只输出完整事件：上游结束时没有以空行结束的尾部原样写出（不改写），之前的完整事件照常改写。
func TestGrokReplyStreamIncompleteTailIsVerbatim(t *testing.T) {
	steps := grokTurn(false)
	complete := grokBuildSSE(grokUpstreamEvents(steps[4:5]), "\n", true)  // 自定义调用的 added
	tail := "event: " + steps[10].up.typ + "\ndata: " + steps[10].up.data // 自定义调用的 arguments.done，没有结束的空行
	s, _ := grokService(t, grokAlways(grokSSEReply(complete+tail, 9)))
	w := grokPost(t, s, "/v1/responses", grokCodexRequest)
	events, rest := splitSSE(w.Body.String())
	if len(events) != 1 || rest != tail {
		t.Fatalf("输出 %d 个完整事件, 尾部 %q; want 1 个与尾部 %q", len(events), rest, tail)
	}
	assertGrokStream(t, steps[4:5], events[0], "\n", true)
}

// ---------------------------------------------------------------------------------------------------------------------
// 回复侧：JSON

// TestGrokReplyJSONRestoresCalls 非流式的 JSON 回复（顶层 object 为 response）的 output 里模型的调用同样还原：
// 自定义调用变为 custom_tool_call（缺失或空的 arguments 得到空 input，非 JSON 的 arguments 原文作为 input），
// namespace 调用还原名字与 namespace；不在 calls 中的调用与其他项不动；数字原文保留；Content-Length（若有）与改写后的正文一致。
func TestGrokReplyJSONRestoresCalls(t *testing.T) {
	const reply = `{"id":"resp_1","object":"response","status":"completed","output":[` +
		`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Working <b> & more","annotations":[]}]},` +
		`{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_p1","name":"apply_patch","arguments":"{\"input\":\"x\\ny\"}"},` +
		`{"id":"fc_2","type":"function_call","status":"completed","call_id":"call_p2","name":"apply_patch"},` +
		`{"id":"fc_3","type":"function_call","status":"completed","call_id":"call_p3","name":"apply_patch","arguments":"not json"},` +
		`{"id":"fc_4","type":"function_call","status":"completed","call_id":"call_t1","name":"codex_app__set_thread_title","arguments":"{\"title\":\"x\"}"},` +
		`{"id":"fc_5","type":"function_call","status":"completed","call_id":"call_s1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},` +
		`{"id":"fc_6","type":"function_call","status":"completed","call_id":"call_u1","name":"unknown_tool","arguments":"{}"}],` +
		`"usage":{"input_tokens":12,"output_tokens":1.0e1,"total_tokens":12345678901234567890}}`
	const want = `{"id":"resp_1","object":"response","status":"completed","output":[` +
		`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Working <b> & more","annotations":[]}]},` +
		`{"id":"ctc_1","type":"custom_tool_call","status":"completed","call_id":"call_p1","name":"apply_patch","input":"x\ny"},` +
		`{"id":"ctc_2","type":"custom_tool_call","status":"completed","call_id":"call_p2","name":"apply_patch","input":""},` +
		`{"id":"ctc_3","type":"custom_tool_call","status":"completed","call_id":"call_p3","name":"apply_patch","input":"not json"},` +
		`{"id":"fc_4","type":"function_call","status":"completed","call_id":"call_t1","name":"set_thread_title","namespace":"codex_app","encrypted_function_args":[],"arguments":"{\"title\":\"x\"}"},` +
		`{"id":"fc_5","type":"function_call","status":"completed","call_id":"call_s1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},` +
		`{"id":"fc_6","type":"function_call","status":"completed","call_id":"call_u1","name":"unknown_tool","arguments":"{}"}],` +
		`"usage":{"input_tokens":12,"output_tokens":1.0e1,"total_tokens":12345678901234567890}}`
	request := strings.Replace(grokCodexRequest, `"stream":true`, `"stream":false`, 1)
	for _, contentType := range []string{"application/json", "application/json; charset=utf-8"} {
		t.Run(contentType, func(t *testing.T) {
			s, _ := grokService(t, grokAlways(grokReply{contentType: contentType, chunks: []string{reply}}))
			w := grokPost(t, s, "/v1/responses", request)
			assertSameJSON(t, w.Body.Bytes(), want)
			for _, literal := range []string{`1.0e1`, `12345678901234567890`, `Working <b> & more`} {
				if !bytes.Contains(w.Body.Bytes(), []byte(literal)) {
					t.Errorf("回复缺少原样片段 %q: %s", literal, w.Body.String())
				}
			}
			if cl := w.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(w.Body.Len()) {
				t.Errorf("Content-Length = %s, 正文长度 = %d", cl, w.Body.Len())
			}
			if got := w.Header().Get("Content-Type"); got != contentType {
				t.Errorf("Content-Type = %q, want %q", got, contentType)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// 逐字节不变的情形

// grokChatBody 是 Chat Completions 形式的 Grok 请求：工具参数根节点是联合，若被改写就会变样。
const grokChatBody = `{"model":"grok-4.7","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[` +
	`{"type":"function","function":{"name":"automation_update","parameters":{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}}}]}`

// TestGrokOtherPathsAndModelsAreVerbatim GPT 走同一 /responses、Grok 走 /chat/completions 与 /v1/messages 时，请求体与回复都逐字节不变：
// 即使回复里有 apply_patch 的 function_call 与轮换的 id，也不改写（回复里的内容与请求无关，请求没有 calls）。
func TestGrokOtherPathsAndModelsAreVerbatim(t *testing.T) {
	stream := grokBuildSSE(grokUpstreamEvents(grokTurn(false)), "\n", true)
	for _, tc := range []struct{ name, local, upstream, body string }{
		{"gpt-responses", "/v1/responses", "/responses", grokAsGPT(grokCodexRequest)},
		{"gpt-responses-without-v1", "/responses", "/responses", grokAsGPT(grokCodexRequest)},
		{"grok-chat", "/v1/chat/completions", "/chat/completions", grokChatBody},
		{"grok-chat-without-v1", "/chat/completions", "/chat/completions", grokChatBody},
		{"grok-messages", "/v1/messages", "/v1/messages",
			`{"model":"grok-4.7","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"t","input_schema":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, up := grokService(t, grokAlways(grokSSEReply(stream, 0)))
			w := grokPost(t, s, tc.local, tc.body)
			sent := up.sent(tc.upstream)
			if len(sent) != 1 || !bytes.Equal(sent[0], []byte(tc.body)) {
				t.Fatalf("转发的请求体应与客户端发送的逐字节一致:\n got=%q\nwant=%s", sent, tc.body)
			}
			if w.Body.String() != stream {
				t.Errorf("回复应逐字节透传:\n got=%q\nwant=%q", w.Body.String(), stream)
			}
		})
	}
}

// grokUnionOnlyRequest 是只有根联合参数的 Grok 请求：请求体会被改写（合并为 object），但没有改名也没有自定义调用，calls 为空。
const grokUnionOnlyRequest = `{"model":"grok-4.7","stream":true,"input":"hi","tools":[` +
	`{"type":"function","name":"apply_patch","parameters":{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"string"}}}]}},` +
	`{"type":"function","name":"codex_app__set_thread_title","parameters":{"type":"object","properties":{}}}]}`

// TestGrokEmptyCallsRepliesAreVerbatim calls 为空的 Grok 请求（请求体被改写，但没有改名与自定义调用）的回复不经 Grok 改写：
// id 一致的流逐字节不变，id 轮换的流与 GPT 走同一路径得到的字节完全相同（只有 itemids 的统一），即使回复里有同名的调用也不还原。
func TestGrokEmptyCallsRepliesAreVerbatim(t *testing.T) {
	for _, vary := range []bool{false, true} {
		t.Run(fmt.Sprintf("vary=%v", vary), func(t *testing.T) {
			stream := grokBuildSSE(grokUpstreamEvents(grokTurn(vary)), "\n", true)
			grokSide, up := grokService(t, grokAlways(grokSSEReply(stream, 5)))
			w := grokPost(t, grokSide, "/v1/responses", grokUnionOnlyRequest)
			sent := up.sent("/responses")
			if len(sent) != 1 || bytes.Contains(sent[0], []byte(`"oneOf"`)) {
				t.Fatalf("请求体应已改写（根联合合并）: %q", sent)
			}
			gptSide, _ := grokService(t, grokAlways(grokSSEReply(stream, 5)))
			gpt := grokPost(t, gptSide, "/v1/responses", grokAsGPT(grokUnionOnlyRequest))
			if w.Body.String() != gpt.Body.String() {
				t.Errorf("calls 为空的 Grok 回复应与 GPT 的字节相同:\n grok=%q\n  gpt=%q", w.Body.String(), gpt.Body.String())
			}
			if !vary && w.Body.String() != stream {
				t.Errorf("id 一致的流应逐字节透传:\n got=%q\nwant=%q", w.Body.String(), stream)
			}
		})
	}

	// JSON 回复同样原样。
	const reply = `{"id":"resp_1","object":"response","output":[{"id":"fc_1","type":"function_call","name":"apply_patch","arguments":"{\"input\":\"x\"}"}]}`
	s, _ := grokService(t, grokAlways(grokJSONReply(200, reply)))
	if w := grokPost(t, s, "/v1/responses", strings.Replace(grokUnionOnlyRequest, `"stream":true`, `"stream":false`, 1)); w.Body.String() != reply {
		t.Errorf("JSON 回复应逐字节透传: %q", w.Body.String())
	}
}

// TestGrokErrorRepliesAreVerbatim 只改 2xx 的回复：错误状态的回复即使内容像可改写的响应或流，也原样返回（状态码与正文都不变）。
func TestGrokErrorRepliesAreVerbatim(t *testing.T) {
	stream := grokBuildSSE(grokUpstreamEvents(grokTurn(false)), "\n", true)
	const looksLikeResponse = `{"id":"resp_1","object":"response","output":[{"id":"fc_1","type":"function_call","name":"apply_patch","arguments":"{\"input\":\"x\"}"}]}`
	for _, tc := range []struct {
		name  string
		reply grokReply
	}{
		{"500-json", grokJSONReply(http.StatusInternalServerError, looksLikeResponse)},
		{"500-sse", grokReply{status: http.StatusInternalServerError, contentType: "text/event-stream", chunks: grokSplit(stream, 0)}},
		{"422-json", grokJSONReply(http.StatusUnprocessableEntity, looksLikeResponse)},
		{"422-text", grokReply{status: http.StatusUnprocessableEntity, contentType: "text/plain", chunks: []string{"422 Unprocessable Entity"}}},
		{"502-text", grokReply{status: http.StatusBadGateway, contentType: "text/plain", chunks: []string{looksLikeResponse}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := grokService(t, grokAlways(tc.reply))
			w := testRequest(s, http.MethodPost, "/v1/responses", grokCodexRequest)
			want := strings.Join(tc.reply.chunks, "")
			if w.Code != tc.reply.status || w.Body.String() != want {
				t.Fatalf("status=%d body=%q, want %d %q", w.Code, w.Body.String(), tc.reply.status, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// staleitems 的重试

// TestGrokRetryAfterStaleItemsUsesRewrittenBody 上游以 401/400 拒绝与连接绑定的输入项时，清理后重发一次：重发的请求体也是 Grok 改写后的
// （没有 custom/namespace/联合，历史调用已转换），并去掉了 reasoning 与各项的 id；重试成功的回复同样还原。
func TestGrokRetryAfterStaleItemsUsesRewrittenBody(t *testing.T) {
	const request = `{"model":"grok-4.7","stream":true,"store":false,"input":[` +
		`{"type":"message","id":"msg_9","role":"user","content":"patch it"},` +
		`{"type":"reasoning","id":"rs_1","encrypted_content":"abc","summary":[]},` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch\n*** End Patch","status":"completed"},` +
		`{"type":"custom_tool_call_output","id":"cto_1","call_id":"call_1","output":"ok"},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_2","name":"set_thread_title","namespace":"codex_app","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_2","output":"done"}],"tools":[` +
		`{"type":"custom","name":"apply_patch","description":"Apply a patch.","format":{"type":"text"}},` +
		`{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"set_thread_title","parameters":{"type":"object","properties":{}}}]},` +
		`{"type":"function","name":"automation_update","parameters":{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}}]}`
	const rewritten = `{"model":"grok-4.7","stream":true,"store":false,"input":[` +
		`{"type":"message","id":"msg_9","role":"user","content":"patch it"},` +
		`{"type":"reasoning","id":"rs_1","encrypted_content":"abc","summary":[]},` +
		`{"type":"function_call","call_id":"call_1","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}","status":"completed"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"ok"},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_2","name":"codex_app__set_thread_title","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_2","output":"done"}],"tools":[` +
		`{"type":"function","name":"apply_patch","description":"Apply a patch.\n\nPut the tool's whole free-form input, as it is to be run, in the ` + "`input`" + ` string; it is passed on as written, so it is not itself JSON.","parameters":` + grokCustomSchema + `},` +
		`{"type":"function","name":"codex_app__set_thread_title","parameters":{"type":"object","properties":{}}},` +
		`{"type":"function","name":"automation_update","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}]}`
	const cleaned = `{"model":"grok-4.7","stream":true,"store":false,"input":[` +
		`{"type":"message","role":"user","content":"patch it"},` +
		`{"type":"function_call","call_id":"call_1","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}","status":"completed"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"ok"},` +
		`{"type":"function_call","call_id":"call_2","name":"codex_app__set_thread_title","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_2","output":"done"}],"tools":[` +
		`{"type":"function","name":"apply_patch","description":"Apply a patch.\n\nPut the tool's whole free-form input, as it is to be run, in the ` + "`input`" + ` string; it is passed on as written, so it is not itself JSON.","parameters":` + grokCustomSchema + `},` +
		`{"type":"function","name":"codex_app__set_thread_title","parameters":{"type":"object","properties":{}}},` +
		`{"type":"function","name":"automation_update","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}]}`

	steps := grokTurn(true)
	stream := grokBuildSSE(grokUpstreamEvents(steps), "\n", true)
	for _, tc := range []struct {
		name  string
		stale grokReply
	}{
		{"401", grokJSONReply(http.StatusUnauthorized, stale401Body)},
		{"400", grokJSONReply(http.StatusBadRequest, stale400Body)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, up := grokService(t, func(_ string, n int, _ []byte) grokReply {
				if n == 1 {
					return tc.stale
				}
				return grokSSEReply(stream, 13)
			})
			w := grokPost(t, s, "/v1/responses", request)
			sent := up.sent("/responses")
			if len(sent) != 2 {
				t.Fatalf("上游 /responses 请求数 = %d, want 2", len(sent))
			}
			assertSameJSON(t, sent[0], rewritten)
			assertSameJSON(t, sent[1], cleaned)
			for i, body := range sent {
				for _, banned := range []string{`"type":"custom"`, `"type":"namespace"`, `"anyOf"`, `custom_tool_call`, `"namespace"`} {
					if bytes.Contains(body, []byte(banned)) {
						t.Errorf("第 %d 次请求体不应再含 %s: %s", i+1, banned, body)
					}
				}
			}
			assertGrokStream(t, steps, w.Body.String(), "\n", true)
		})
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// namespace 中的自定义工具、只由历史声明的调用与请求之间互不影响

// TestGrokReplyCustomToolInNamespace namespace 里的自定义工具：模型调用其 flat 名时，还原为 custom_tool_call，name 为原名并带 namespace，
// 参数增量同样解码；同一 namespace 里的普通 function 还原名字与 namespace。
func TestGrokReplyCustomToolInNamespace(t *testing.T) {
	const request = `{"model":"grok-4.7","stream":true,"input":"hi","tools":[{"type":"namespace","name":"mcp__x","tools":[` +
		`{"type":"custom","name":"run","description":"Run.","format":{"type":"text"}},` +
		`{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}]}]}`
	q := itemIDQuote
	args := `{"input":"ls -la \"x\""}`
	const input = `ls -la "x"`
	steps := []grokStep{
		{
			up:   itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_r1","type":"function_call","call_id":"call_r","name":"mcp__x__run","arguments":""}}`},
			want: itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"ctc_r1","type":"custom_tool_call","call_id":"call_r","name":"run","namespace":"mcp__x","input":""}}`},
		},
		{
			up:   itemIDEvent{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":"fc_r2","output_index":0,"delta":` + q(args) + `}`},
			want: itemIDEvent{"response.custom_tool_call_input.delta", `{"type":"response.custom_tool_call_input.delta","sequence_number":2,"item_id":"ctc_r1","output_index":0,"delta":` + q(input) + `}`},
		},
		{
			up:   itemIDEvent{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":3,"item_id":"fc_r3","output_index":0,"name":"mcp__x__run","arguments":` + q(args) + `}`},
			want: itemIDEvent{"response.custom_tool_call_input.done", `{"type":"response.custom_tool_call_input.done","sequence_number":3,"item_id":"ctc_r1","output_index":0,"input":` + q(input) + `}`},
		},
		{
			up:   itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"id":"fc_r4","type":"function_call","status":"completed","call_id":"call_r","name":"mcp__x__run","arguments":` + q(args) + `}}`},
			want: itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"id":"ctc_r1","type":"custom_tool_call","status":"completed","call_id":"call_r","name":"run","namespace":"mcp__x","input":` + q(input) + `}}`},
		},
		{
			up:   itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":5,"output_index":1,"item":{"id":"fc_f1","type":"function_call","status":"completed","call_id":"call_f","name":"mcp__x__f","arguments":"{}"}}`},
			want: itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":5,"output_index":1,"item":{"id":"fc_f1","type":"function_call","status":"completed","call_id":"call_f","name":"f","namespace":"mcp__x","encrypted_function_args":[],"arguments":"{}"}}`},
		},
	}
	stream := grokBuildSSE(grokUpstreamEvents(steps), "\n", true)
	s, _ := grokService(t, grokAlways(grokSSEReply(stream, 17)))
	w := grokPost(t, s, "/v1/responses", request)
	assertGrokStream(t, steps, w.Body.String(), "\n", true)
}

// TestGrokReplyRestoresCallsKnownOnlyFromHistory 历史里出现过的调用即使本次请求没有声明对应的工具（没有 tools），模型再次调用时也还原：
// 自定义调用（带 namespace）还原为 custom_tool_call，namespace 里的 function 还原名字与 namespace，其他调用不动。
func TestGrokReplyRestoresCallsKnownOnlyFromHistory(t *testing.T) {
	const request = `{"model":"grok-4.7","input":[` +
		`{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","namespace":"mcp__x","input":"a"},` +
		`{"type":"custom_tool_call_output","call_id":"call_1","output":"ok"},` +
		`{"type":"function_call","call_id":"call_2","name":"set_thread_title","namespace":"codex_app","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_2","output":"ok"}]}`
	const reply = `{"id":"resp_1","object":"response","status":"completed","output":[` +
		`{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_3","name":"mcp__x__apply_patch","arguments":"{\"input\":\"b\"}"},` +
		`{"id":"fc_2","type":"function_call","status":"completed","call_id":"call_4","name":"codex_app__set_thread_title","arguments":"{}"},` +
		`{"id":"fc_3","type":"function_call","status":"completed","call_id":"call_5","name":"mcp__x__other","arguments":"{}"}]}`
	const want = `{"id":"resp_1","object":"response","status":"completed","output":[` +
		`{"id":"ctc_1","type":"custom_tool_call","status":"completed","call_id":"call_3","name":"apply_patch","namespace":"mcp__x","input":"b"},` +
		`{"id":"fc_2","type":"function_call","status":"completed","call_id":"call_4","name":"set_thread_title","namespace":"codex_app","encrypted_function_args":[],"arguments":"{}"},` +
		`{"id":"fc_3","type":"function_call","status":"completed","call_id":"call_5","name":"mcp__x__other","arguments":"{}"}]}`
	s, up := grokService(t, grokAlways(grokJSONReply(200, reply)))
	w := grokPost(t, s, "/v1/responses", request)
	assertSameJSON(t, w.Body.Bytes(), want)
	if sent := up.sent("/responses"); len(sent) != 1 || bytes.Contains(sent[0], []byte("custom_tool_call")) {
		t.Fatalf("历史里的自定义调用应已转换后再转发: %q", sent)
	}
}

// TestGrokCallsDoNotLeakAcrossRequests 调用表只属于当次请求：同一个 Service 上先后（以及并发）的两个请求，
// 一个声明了 apply_patch 为自定义工具，一个没有，上游对两者都回同一个 apply_patch 的 function_call：前者被还原，后者原样。
func TestGrokCallsDoNotLeakAcrossRequests(t *testing.T) {
	const reply = `{"id":"resp_1","object":"response","status":"completed","output":[` +
		`{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"apply_patch","arguments":"{\"input\":\"x\"}"}]}`
	const restored = `{"id":"resp_1","object":"response","status":"completed","output":[` +
		`{"id":"ctc_1","type":"custom_tool_call","status":"completed","call_id":"call_1","name":"apply_patch","input":"x"}]}`
	const withCustom = `{"model":"grok-4.7","input":"hi","tools":[{"type":"custom","name":"apply_patch","description":"d"}]}`
	const withFunction = `{"model":"grok-4.7","input":"hi","tools":[{"type":"function","name":"apply_patch","parameters":{"type":"object","properties":{}}}]}`
	s, _ := grokService(t, grokAlways(grokJSONReply(200, reply)))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			custom := testRequest(s, http.MethodPost, "/v1/responses", withCustom)
			if custom.Code != http.StatusOK || !reflect.DeepEqual(decodeNumberJSON(t, custom.Body.String()), decodeNumberJSON(t, restored)) {
				t.Errorf("声明了自定义工具的请求应得到还原的回复: %d %s", custom.Code, custom.Body.String())
			}
			plain := testRequest(s, http.MethodPost, "/v1/responses", withFunction)
			if plain.Code != http.StatusOK || plain.Body.String() != reply {
				t.Errorf("没有自定义工具的请求应原样得到回复: %d %s", plain.Code, plain.Body.String())
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------------------------------------------------
// R2-a：namespace 摊平后 tools 为空，转发的请求不含 tool_choice

// grokStrictUpstream 是更像真实 Copilot 的假上游：对 Grok 不接受的内容回裸 422；请求没有 tools（缺失、null 或空数组）却带 tool_choice 时回 400
// （"A tool_choice was set on the request but no tools were specified"）；否则回无调用的流。
func grokStrictUpstream() func(string, int, []byte) grokReply {
	return func(_ string, _ int, body []byte) grokReply {
		if grokRefuses(body) {
			return grokRefusal(0)
		}
		var request struct {
			Tools      json.RawMessage `json:"tools"`
			ToolChoice json.RawMessage `json:"tool_choice"`
		}
		_ = json.Unmarshal(body, &request)
		var tools []json.RawMessage
		if len(request.ToolChoice) > 0 && (json.Unmarshal(request.Tools, &tools) != nil || len(tools) == 0) {
			return grokJSONReply(http.StatusBadRequest, `{"error":{"message":"A tool_choice was set on the request but no tools were specified","type":"invalid_request_error"}}`)
		}
		return grokSSEReply(grokPlainStream(), 0)
	}
}

// TestGrokRequestDropsToolChoiceWhenFlatteningEmptiesTools namespace 里只有 function 与 custom 之外的工具（或根本没有工具）时，
// 摊平后 tools 为空，tool_choice 随之去掉（同 withoutLoneToolChoice），否则 Copilot 对"有 tool_choice 却没有 tools"的请求回 400。
// tools 摊平后仍有工具时 tool_choice 保留（并按规则改写）；同一路径上的 GPT 原样转发。
func TestGrokRequestDropsToolChoiceWhenFlatteningEmptiesTools(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"only-web-search",
			`{"model":"grok-4.7","stream":true,"input":"hi","temperature":1.0,"tools":[{"type":"namespace","name":"ns","tools":[{"type":"web_search"}]}],"tool_choice":"auto"}`,
			`{"model":"grok-4.7","stream":true,"input":"hi","temperature":1.0,"tools":[]}`},
		{"empty-namespace",
			`{"model":"grok-4.7","stream":true,"input":"hi","tools":[{"type":"namespace","name":"ns","tools":[]}],"tool_choice":"required"}`,
			`{"model":"grok-4.7","stream":true,"input":"hi","tools":[]}`},
		{"several-namespaces-and-named-choice",
			`{"model":"grok-4.7","stream":true,"input":"hi","tools":[{"type":"namespace","name":"a","tools":[{"type":"tool_search"}]},{"type":"namespace","name":"b","tools":[]}],` +
				`"tool_choice":{"type":"function","name":"f","namespace":"a"}}`,
			`{"model":"grok-4.7","stream":true,"input":"hi","tools":[]}`},
		{"allowed-tools",
			`{"model":"grok-4.7","stream":true,"input":"hi","tools":[{"type":"namespace","name":"a","tools":[{"type":"web_search"}]}],` +
				`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"f","namespace":"a"}]}}`,
			`{"model":"grok-4.7","stream":true,"input":"hi","tools":[]}`},
		// 摊平后仍有工具：tool_choice 保留。
		{"keeps-it-when-tools-remain",
			`{"model":"grok-4.7","stream":true,"input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}],` +
				`"tool_choice":{"type":"function","name":"f","namespace":"ns"}}`,
			`{"model":"grok-4.7","stream":true,"input":"hi","tools":[{"type":"function","name":"ns__f","parameters":{"type":"object"}}],` +
				`"tool_choice":{"type":"function","name":"ns__f"}}`},
		{"keeps-it-when-a-custom-tool-remains",
			`{"model":"grok-4.7","stream":true,"input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"custom","name":"c","description":"d"}]}],"tool_choice":"auto"}`,
			`{"model":"grok-4.7","stream":true,"input":"hi","tools":[` + grokCustomFn("ns__c", "d\n\n"+grokCustomNote) + `],"tool_choice":"auto"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, up := grokService(t, grokStrictUpstream())
			w := grokPost(t, s, "/v1/responses", tc.body)
			sent := up.sent("/responses")
			if len(sent) != 1 {
				t.Fatalf("上游 /responses 请求数 = %d, want 1", len(sent))
			}
			assertSameJSON(t, sent[0], tc.want)
			if w.Body.String() != grokPlainStream() {
				t.Errorf("客户端应逐字节收到上游的流: %q", w.Body.String())
			}
		})
	}

	// GPT 不改写：tools 非空（namespace 原样），tool_choice 原样。
	body := `{"model":"gpt-4.1","stream":true,"input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"web_search"}]}],"tool_choice":"auto"}`
	s, up := grokService(t, grokAlways(grokSSEReply(grokPlainStream(), 0)))
	grokPost(t, s, "/v1/responses", body)
	if sent := up.sent("/responses"); len(sent) != 1 || !bytes.Equal(sent[0], []byte(body)) {
		t.Fatalf("GPT 的请求体应逐字节转发: %q", sent)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// R2-c：非流式 JSON 回复的还原上限与分块写出

// setGrokJSONLimit 在本测试内把 grokJSONLimit 设为 limit 字节（不关心它的整数类型）。
func setGrokJSONLimit(t *testing.T, limit int) {
	t.Helper()
	v := reflect.ValueOf(&grokJSONLimit).Elem()
	old := v.Int()
	v.SetInt(int64(limit))
	t.Cleanup(func() { v.SetInt(old) })
}

// grokSizedReply 是含一个自定义调用 apply_patch 和一段 text 的 JSON 回复（对应 grokCodexRequest 的 calls），长度随 text 而定。
func grokSizedReply(text string) string {
	return `{"id":"resp_1","object":"response","status":"completed","output":[` +
		`{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"apply_patch","arguments":"{\"input\":\"x\"}"},` +
		`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":` + itemIDQuote(text) + `,"annotations":[]}]}]}`
}

// grokSizedRestored 是 grokSizedReply 还原后的样子。
func grokSizedRestored(text string) string {
	return `{"id":"resp_1","object":"response","status":"completed","output":[` +
		`{"id":"ctc_1","type":"custom_tool_call","status":"completed","call_id":"call_1","name":"apply_patch","input":"x"},` +
		`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":` + itemIDQuote(text) + `,"annotations":[]}]}]}`
}

// grokCounterText 返回约 n 字节、各段互不相同的文本（段序错乱或丢失都能发现）。
func grokCounterText(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "%06d,", i)
	}
	return b.String()
}

// grokNonStreamRequest 是 grokCodexRequest 的非流式形式。
var grokNonStreamRequest = strings.Replace(grokCodexRequest, `"stream":true`, `"stream":false`, 1)

// TestGrokReplyJSONLimitBoundary 不超过 grokJSONLimit 的 JSON 回复照常还原，超过一个字节就原样透传。
func TestGrokReplyJSONLimitBoundary(t *testing.T) {
	text := strings.Repeat("a", 500)
	reply := grokSizedReply(text)
	length := len(reply)
	for _, tc := range []struct {
		name     string
		limit    int
		restored bool
	}{
		{"exactly-the-limit", length, true},
		{"one-under-the-limit", length + 1, true},
		{"one-over-the-limit", length - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setGrokJSONLimit(t, tc.limit)
			s, _ := grokService(t, grokAlways(grokJSONReply(200, reply)))
			w := grokPost(t, s, "/v1/responses", grokNonStreamRequest)
			if tc.restored {
				assertSameJSON(t, w.Body.Bytes(), grokSizedRestored(text))
				if cl := w.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(w.Body.Len()) {
					t.Errorf("Content-Length = %s, 正文长度 = %d", cl, w.Body.Len())
				}
				return
			}
			if w.Body.String() != reply {
				t.Errorf("超限的回复应原样透传:\n got=%q\nwant=%q", w.Body.String(), reply)
			}
			if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(length) {
				t.Errorf("Content-Length = %q, want %d（原样透传时头不变）", cl, length)
			}
		})
	}
}

// TestGrokReplyJSONOverLimitIsPassedThroughWhole 超过上限的大 JSON 回复原样透传且完整（已读部分与剩余部分都写出，顺序不错），
// 状态码与头不变：Content-Type、Content-Length、其他头，2xx 的非 200 状态也一样。
func TestGrokReplyJSONOverLimitIsPassedThroughWhole(t *testing.T) {
	setGrokJSONLimit(t, 1024)
	reply := grokSizedReply(grokCounterText(300 << 10))
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s, _ := grokService(t, grokAlways(grokReply{
				status: status, contentType: "application/json; charset=utf-8",
				headers: map[string]string{"X-Upstream-Marker": "kept"},
				chunks:  grokSplit(reply, 4096),
			}))
			w := testRequest(s, http.MethodPost, "/v1/responses", grokNonStreamRequest)
			if w.Code != status {
				t.Fatalf("status = %d, want %d", w.Code, status)
			}
			if w.Body.String() != reply {
				t.Fatalf("超限的回复应原样透传且完整: 收到 %d 字节, want %d 字节", w.Body.Len(), len(reply))
			}
			if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := w.Header().Get("X-Upstream-Marker"); got != "kept" {
				t.Errorf("X-Upstream-Marker = %q", got)
			}
			// 上游带了 Content-Length，原样透传时头不变。
			if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(len(reply)) {
				t.Errorf("Content-Length = %q, want %d", cl, len(reply))
			}
		})
	}
}

// grokWriteRecorder 记录每次 Write 的字节数。
type grokWriteRecorder struct {
	*httptest.ResponseRecorder
	writes []int
}

func (w *grokWriteRecorder) Write(p []byte) (int, error) {
	w.writes = append(w.writes, len(p))
	return w.ResponseRecorder.Write(p)
}

func (w *grokWriteRecorder) WriteString(s string) (int, error) {
	w.writes = append(w.writes, len(s))
	return w.ResponseRecorder.WriteString(s)
}

// TestGrokReplyJSONIsWrittenInChunks 还原后的大 JSON 回复分块写出：每次 Write 都不大，这样 generationWriter 的写截止时间能逐块续期，
// 慢客户端读一个大回复不会被一次整体写超时。
func TestGrokReplyJSONIsWrittenInChunks(t *testing.T) {
	text := grokCounterText(600 << 10)
	s, _ := grokService(t, grokAlways(grokJSONReply(200, grokSizedReply(text))))
	rec := &grokWriteRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(grokNonStreamRequest))
	r.Header.Set("Content-Type", "application/json")
	withTestAPIKey(s.Handler()).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	assertSameJSON(t, rec.Body.Bytes(), grokSizedRestored(text))
	if cl := rec.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %s, 正文长度 = %d", cl, rec.Body.Len())
	}
	most, total := 0, 0
	for _, n := range rec.writes {
		most, total = max(most, n), total+n
	}
	if total != rec.Body.Len() {
		t.Errorf("各次 Write 共 %d 字节, 正文 %d 字节", total, rec.Body.Len())
	}
	if len(rec.writes) < 5 || most > 128<<10 {
		t.Errorf("%d 字节的回复应分成多块写出: 共 %d 次 Write, 最大一次 %d 字节", rec.Body.Len(), len(rec.writes), most)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// R2-d：补测试缺口

// TestGrokReplyJSONOnlyRestoresResponses 只还原顶层 object 为 "response" 的 JSON 回复：没有 object、object 是别的值或 null 的回复，
// 即使 output 里有同名的 function_call 也原样返回（正文逐字节不变）。
func TestGrokReplyJSONOnlyRestoresResponses(t *testing.T) {
	const output = `"output":[{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"apply_patch","arguments":"{\"input\":\"x\"}"}]`
	for _, tc := range []struct {
		name, reply string
		restored    bool
	}{
		{"response", `{"id":"resp_1","object":"response",` + output + `}`, true},
		{"no-object", `{"id":"resp_1","status":"completed",` + output + `}`, false},
		{"object-list", `{"id":"resp_1","object":"list",` + output + `}`, false},
		{"object-chat-completion", `{"id":"resp_1","object":"chat.completion",` + output + `}`, false},
		{"object-null", `{"id":"resp_1","object":null,` + output + `}`, false},
		{"object-not-a-string", `{"id":"resp_1","object":["response"],` + output + `}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := grokService(t, grokAlways(grokJSONReply(200, tc.reply)))
			w := grokPost(t, s, "/v1/responses", grokNonStreamRequest)
			if tc.restored {
				if !bytes.Contains(w.Body.Bytes(), []byte(`"type":"custom_tool_call"`)) {
					t.Fatalf("object 为 response 的回复应还原: %s", w.Body.String())
				}
				return
			}
			if w.Body.String() != tc.reply {
				t.Errorf("不是 response 的 JSON 应原样返回:\n got=%q\nwant=%q", w.Body.String(), tc.reply)
			}
			if cl := w.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(tc.reply)) {
				t.Errorf("Content-Length = %s, want %d", cl, len(tc.reply))
			}
		})
	}
}

// TestGrokReplyKeepsUpstreamEncryptedArgs 上游已带 encrypted_function_args 的 namespace 调用保持它的值，不被覆盖成 []：
// JSON 回复的 output、SSE 的 added 与完成项、response.completed 的 output 与 function_call_arguments.done 事件都一样。
func TestGrokReplyKeepsUpstreamEncryptedArgs(t *testing.T) {
	const call = `{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_t1","name":"codex_app__set_thread_title","encrypted_function_args":["title"],"arguments":"{}"}`
	const restored = `{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_t1","name":"set_thread_title","namespace":"codex_app","encrypted_function_args":["title"],"arguments":"{}"}`

	t.Run("json", func(t *testing.T) {
		s, _ := grokService(t, grokAlways(grokJSONReply(200, `{"id":"resp_1","object":"response","output":[`+call+`]}`)))
		w := grokPost(t, s, "/v1/responses", grokNonStreamRequest)
		assertSameJSON(t, w.Body.Bytes(), `{"id":"resp_1","object":"response","output":[`+restored+`]}`)
	})

	t.Run("sse", func(t *testing.T) {
		const added = `{"id":"fc_1","type":"function_call","call_id":"call_t1","name":"codex_app__set_thread_title","encrypted_function_args":["title"],"arguments":""}`
		const addedWant = `{"id":"fc_1","type":"function_call","call_id":"call_t1","name":"set_thread_title","namespace":"codex_app","encrypted_function_args":["title"],"arguments":""}`
		steps := []grokStep{
			{up: itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":` + added + `}`},
				want: itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":` + addedWant + `}`}},
			{up: itemIDEvent{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":2,"item_id":"fc_1","output_index":0,"name":"codex_app__set_thread_title","encrypted_function_args":["title"],"arguments":"{}"}`},
				want: itemIDEvent{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":2,"item_id":"fc_1","output_index":0,"name":"set_thread_title","namespace":"codex_app","encrypted_function_args":["title"],"arguments":"{}"}`}},
			{up: itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":` + call + `}`},
				want: itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":` + restored + `}`}},
			{up: itemIDEvent{"response.completed", `{"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","output":[` + call + `]}}`},
				want: itemIDEvent{"response.completed", `{"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","output":[` + restored + `]}}`}},
		}
		stream := grokBuildSSE(grokUpstreamEvents(steps), "\n", true)
		s, _ := grokService(t, grokAlways(grokSSEReply(stream, 0)))
		w := grokPost(t, s, "/v1/responses", grokCodexRequest)
		assertGrokStream(t, steps, w.Body.String(), "\n", true)
	})
}

// TestGrokReplyStreamForgetsCallWhenOutputIndexIsReused 同一个 output_index 先登记了被识别的自定义调用，之后又出现未识别的 added（别的调用）时，
// 先前的登记被清除：后面这个 output_index 的参数增量、done 与完成项都不再按自定义调用改写，逐字节不变。
func TestGrokReplyStreamForgetsCallWhenOutputIndexIsReused(t *testing.T) {
	steps := []grokStep{
		{
			up:   itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc_x1","type":"function_call","call_id":"call_x","name":"apply_patch","arguments":""}}`},
			want: itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"ctc_x1","type":"custom_tool_call","call_id":"call_x","name":"apply_patch","input":""}}`},
		},
		{
			up:   itemIDEvent{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":"fc_x1","output_index":0,"delta":"{\"input\":\"a"}`},
			want: itemIDEvent{"response.custom_tool_call_input.delta", `{"type":"response.custom_tool_call_input.delta","sequence_number":2,"item_id":"ctc_x1","output_index":0,"delta":"a"}`},
		},
		// 同一 output_index 上换成未识别的调用。
		{up: itemIDEvent{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":3,"output_index":0,"item":{"id":"fc_y1","type":"function_call","call_id":"call_y","name":"shell","arguments":""}}`}, same: true},
		{up: itemIDEvent{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","sequence_number":4,"item_id":"fc_y1","output_index":0,"delta":"{\"input\":\"b\"}"}`}, same: true},
		{up: itemIDEvent{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":5,"item_id":"fc_y1","output_index":0,"name":"shell","arguments":"{\"input\":\"b\"}"}`}, same: true},
		{up: itemIDEvent{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":{"id":"fc_y1","type":"function_call","status":"completed","call_id":"call_y","name":"shell","arguments":"{\"input\":\"b\"}"}}`}, same: true},
	}
	stream := grokBuildSSE(grokUpstreamEvents(steps), "\n", true)
	s, _ := grokService(t, grokAlways(grokSSEReply(stream, 0)))
	w := grokPost(t, s, "/v1/responses", grokCodexRequest)
	assertGrokStream(t, steps, w.Body.String(), "\n", true)
}

// TestGrokReplyRestoresEverySuccessStatus 所有 2xx（200、201、202）的回复都还原，SSE 与 JSON 一样；状态码不变。非 2xx 不还原见 TestGrokErrorRepliesAreVerbatim。
func TestGrokReplyRestoresEverySuccessStatus(t *testing.T) {
	steps := grokTurn(true)
	stream := grokBuildSSE(grokUpstreamEvents(steps), "\n", true)
	text := "hello"
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted} {
		t.Run(strconv.Itoa(status)+"-sse", func(t *testing.T) {
			s, _ := grokService(t, grokAlways(grokReply{status: status, contentType: "text/event-stream", chunks: grokSplit(stream, 0)}))
			w := testRequest(s, http.MethodPost, "/v1/responses", grokCodexRequest)
			if w.Code != status {
				t.Fatalf("status = %d, want %d", w.Code, status)
			}
			assertGrokStream(t, steps, w.Body.String(), "\n", true)
		})
		t.Run(strconv.Itoa(status)+"-json", func(t *testing.T) {
			s, _ := grokService(t, grokAlways(grokReply{status: status, contentType: "application/json", chunks: []string{grokSizedReply(text)}}))
			w := testRequest(s, http.MethodPost, "/v1/responses", grokNonStreamRequest)
			if w.Code != status {
				t.Fatalf("status = %d, want %d", w.Code, status)
			}
			assertSameJSON(t, w.Body.Bytes(), grokSizedRestored(text))
		})
	}
}
