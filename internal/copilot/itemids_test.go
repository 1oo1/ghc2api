package copilot

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// itemIDEvent 是测试流中的一个 SSE 事件：event 行的类型与 data 行的原始 JSON。
type itemIDEvent struct {
	typ  string
	data string
}

// rotatingAddedIDs 是轮换流中每个 output_index 由 output_item.added 给出的 id，即期望客户端看到的唯一 id。
var rotatingAddedIDs = map[int]string{0: "rs_A0", 1: "ws_A1", 2: "msg_A2", 3: "fc_A3"}

// rotatingReplacements 把轮换流中后续事件使用的新 id 映射回 added 的 id。
var rotatingReplacements = map[string]string{
	"rs_D0": "rs_A0", "rs_X0": "rs_A0",
	"ws_B1": "ws_A1", "ws_C1": "ws_A1", "ws_E1": "ws_A1", "ws_D1": "ws_A1", "ws_X1": "ws_A1",
	"msg_B2": "msg_A2", "msg_C2": "msg_A2", "msg_E2": "msg_A2", "msg_F2": "msg_A2", "msg_G2": "msg_A2", "msg_D2": "msg_A2", "msg_X2": "msg_A2",
	"fc_B3": "fc_A3", "fc_C3": "fc_A3", "fc_D3": "fc_A3", "fc_X3": "fc_A3",
}

// rotatingEvents 模拟 Copilot 上游的 /responses 流：同一输出项在每个事件里都换一个新 id。
func rotatingEvents() []itemIDEvent {
	return []itemIDEvent{
		{"response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","output":[]}}`},
		{"response.in_progress", `{"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_1","object":"response","status":"in_progress","output":[]}}`},
		// output_index 0：reasoning
		{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_A0","type":"reasoning","summary":[]}}`},
		{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"rs_D0","type":"reasoning","summary":[],"encrypted_content":"enc=="}}`},
		// output_index 1：web_search_call
		{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":4,"output_index":1,"item":{"id":"ws_A1","type":"web_search_call","status":"in_progress"}}`},
		{"response.web_search_call.in_progress", `{"type":"response.web_search_call.in_progress","sequence_number":5,"output_index":1,"item_id":"ws_B1"}`},
		{"response.web_search_call.searching", `{"type":"response.web_search_call.searching","sequence_number":6,"output_index":1,"item_id":"ws_C1"}`},
		{"response.web_search_call.completed", `{"type":"response.web_search_call.completed","sequence_number":7,"output_index":1,"item_id":"ws_E1"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":8,"output_index":1,"item":{"id":"ws_D1","type":"web_search_call","status":"completed","action":{"type":"search","query":"weather"}}}`},
		// output_index 2：message
		{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":9,"output_index":2,"item":{"id":"msg_A2","type":"message","status":"in_progress","role":"assistant","content":[]}}`},
		{"response.content_part.added", `{"type":"response.content_part.added","sequence_number":10,"output_index":2,"content_index":0,"item_id":"msg_B2","part":{"type":"output_text","text":"","annotations":[]}}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","sequence_number":11,"output_index":2,"content_index":0,"item_id":"msg_C2","delta":"Hello <b> & "}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","sequence_number":12,"output_index":2,"content_index":0,"item_id":"msg_E2","delta":"\"q\" world"}`},
		{"response.output_text.done", `{"type":"response.output_text.done","sequence_number":13,"output_index":2,"content_index":0,"item_id":"msg_F2","text":"Hello <b> & \"q\" world"}`},
		{"response.content_part.done", `{"type":"response.content_part.done","sequence_number":14,"output_index":2,"content_index":0,"item_id":"msg_G2","part":{"type":"output_text","text":"Hello <b> & \"q\" world","annotations":[]}}`},
		{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":15,"output_index":2,"item":{"id":"msg_D2","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello <b> & \"q\" world","annotations":[]}]}}`},
		// output_index 3：function_call
		{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":16,"output_index":3,"item":{"id":"fc_A3","type":"function_call","status":"in_progress","call_id":"call_1","name":"get_weather","arguments":""}}`},
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","sequence_number":17,"output_index":3,"item_id":"fc_B3","delta":"{\"city\":\"Paris\"}"}`},
		{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":18,"output_index":3,"item_id":"fc_C3","arguments":"{\"city\":\"Paris\"}"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":19,"output_index":3,"item":{"id":"fc_D3","type":"function_call","status":"completed","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}`},
		// 汇总：output 中每项的 id 再次不同；usage 中的数字写法必须原样保留。
		{"response.completed", `{"type":"response.completed","sequence_number":20,"response":{"id":"resp_1","object":"response","status":"completed","output":[` +
			`{"id":"rs_X0","type":"reasoning","summary":[]},` +
			`{"id":"ws_X1","type":"web_search_call","status":"completed","action":{"type":"search","query":"weather"}},` +
			`{"id":"msg_X2","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello <b> & \"q\" world","annotations":[]}]},` +
			`{"id":"fc_X3","type":"function_call","status":"completed","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}` +
			`],"usage":{"input_tokens":1.0,"output_tokens":12345678901234567890,"total_tokens":2.50e1}}}`},
	}
}

// buildSSE 按真实 OpenAI SSE 的形状拼接事件：event 行在前、data 行在后，eol 为行尾，事件以空行结束。
func buildSSE(events []itemIDEvent, eol string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: " + e.typ + eol + "data: " + e.data + eol + eol)
	}
	return b.String()
}

// sseEventEnd 返回 s 中第一个事件分隔空行（"\n\n" 或 "\n\r\n"）结束处的下标；没有完整事件时返回 -1。
func sseEventEnd(s string) int {
	lf, crlf := strings.Index(s, "\n\n"), strings.Index(s, "\n\r\n")
	switch {
	case lf < 0 && crlf < 0:
		return -1
	case crlf < 0 || (lf >= 0 && lf < crlf):
		return lf + 2
	default:
		return crlf + 3
	}
}

// splitSSE 把文本切分为完整事件（各自带结束空行）与末尾不完整的剩余部分。
func splitSSE(s string) (events []string, rest string) {
	for {
		end := sseEventEnd(s)
		if end < 0 {
			return events, s
		}
		events = append(events, s[:end])
		s = s[end:]
	}
}

// sseLines 返回事件的各行（去掉行尾 "\r"，不含结束空行）。
func sseLines(event string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(event, "\r\n"), "\n") {
		lines = append(lines, strings.TrimSuffix(line, "\r"))
	}
	return lines
}

// sseData 按 SSE 规则把事件的 data 行用 "\n" 连接；没有 data 行时 ok 为 false。
func sseData(event string) (data string, ok bool) {
	var parts []string
	for _, line := range sseLines(event) {
		if line == "data" {
			parts = append(parts, "")
		} else if v, found := strings.CutPrefix(line, "data:"); found {
			parts = append(parts, strings.TrimPrefix(v, " "))
		}
	}
	return strings.Join(parts, "\n"), len(parts) > 0
}

// sseOtherLines 返回事件中除 data 行以外的行，用于确认 event 行、注释等被原样保留。
func sseOtherLines(event string) []string {
	var other []string
	for _, line := range sseLines(event) {
		if line != "data" && !strings.HasPrefix(line, "data:") {
			other = append(other, line)
		}
	}
	return other
}

// decodeNumberJSON 以 UseNumber 解码，保证比较时数字写法不丢失。
func decodeNumberJSON(t *testing.T, data string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("事件 data 不是合法 JSON: %v\n%s", err, data)
	}
	return v
}

// collectItemIDs 汇总输出中每个 output_index 出现过的全部 id：item_id、item.id 以及 response.output[i].id。
func collectItemIDs(t *testing.T, out string) map[int]map[string]bool {
	t.Helper()
	ids := map[int]map[string]bool{}
	add := func(index int, id any) {
		s, ok := id.(string)
		if !ok {
			return
		}
		if ids[index] == nil {
			ids[index] = map[string]bool{}
		}
		ids[index][s] = true
	}
	events, _ := splitSSE(out)
	for _, event := range events {
		data, ok := sseData(event)
		if !ok || !strings.HasPrefix(strings.TrimSpace(data), "{") {
			continue
		}
		v := decodeNumberJSON(t, data)
		if n, ok := v["output_index"].(json.Number); ok {
			index, err := n.Int64()
			if err != nil {
				t.Fatalf("output_index 不是整数: %v", n)
			}
			if id, exists := v["item_id"]; exists {
				add(int(index), id)
			}
			if item, ok := v["item"].(map[string]any); ok {
				add(int(index), item["id"])
			}
		}
		if response, ok := v["response"].(map[string]any); ok {
			output, _ := response["output"].([]any)
			for i, raw := range output {
				if item, ok := raw.(map[string]any); ok {
					add(i, item["id"])
				}
			}
		}
	}
	return ids
}

// runTidy 把输入按 chunk 字节一片写入 itemIDTidy（chunk<=0 表示一次写入），最后拼上 flush 的结果。
// 同时检查每次 write 只返回完整事件。
func runTidy(t *testing.T, in string, chunk int) (written, flushed string) {
	t.Helper()
	tidy := &itemIDTidy{}
	if chunk <= 0 {
		chunk = len(in)
	}
	var out strings.Builder
	for start := 0; start < len(in); start += chunk {
		end := min(start+chunk, len(in))
		got := string(tidy.write([]byte(in[start:end])))
		if got != "" {
			if _, rest := splitSSE(got); rest != "" {
				t.Fatalf("write 返回了不完整的事件: %q", got)
			}
		}
		out.WriteString(got)
	}
	return out.String(), string(tidy.flush())
}

// assertTidiedRotatingStream 校验整理后的轮换流：每个输出项只剩 added 的 id，其余内容与写法不变。
func assertTidiedRotatingStream(t *testing.T, in, out, eol string) {
	t.Helper()
	inEvents, inRest := splitSSE(in)
	outEvents, outRest := splitSSE(out)
	if inRest != "" || outRest != "" {
		t.Fatalf("存在未完整的事件: in=%q out=%q", inRest, outRest)
	}
	if len(outEvents) != len(inEvents) {
		t.Fatalf("事件数量 = %d, want %d\n%s", len(outEvents), len(inEvents), out)
	}

	ids := collectItemIDs(t, out)
	for index, want := range rotatingAddedIDs {
		if got := ids[index]; len(got) != 1 || !got[want] {
			t.Errorf("output_index %d 的 id 集合 = %v, want 仅 %q", index, got, want)
		}
	}
	if len(ids) != len(rotatingAddedIDs) {
		t.Errorf("出现了意外的 output_index: %v", ids)
	}
	for rotated := range rotatingReplacements {
		if strings.Contains(out, rotated) {
			t.Errorf("输出仍包含轮换后的 id %q", rotated)
		}
	}

	for i := range inEvents {
		inEvent, outEvent := inEvents[i], outEvents[i]
		if !strings.HasSuffix(outEvent, eol+eol) {
			t.Errorf("事件 %d 的结束符被改变: %q", i, outEvent)
		}
		if !reflect.DeepEqual(sseOtherLines(outEvent), sseOtherLines(inEvent)) {
			t.Errorf("事件 %d 的非 data 行被改变: got %q want %q", i, sseOtherLines(outEvent), sseOtherLines(inEvent))
		}
		needsChange := false
		for rotated := range rotatingReplacements {
			if strings.Contains(inEvent, rotated) {
				needsChange = true
			}
		}
		if !needsChange {
			if outEvent != inEvent {
				t.Errorf("无需改写的事件 %d 应逐字节保持不变:\ngot  %q\nwant %q", i, outEvent, inEvent)
			}
			continue
		}
		// 改写后的事件：把输入中的轮换 id 换成 added id 后，语义（含数字写法）必须完全一致。
		inData, _ := sseData(inEvent)
		for rotated, added := range rotatingReplacements {
			inData = strings.ReplaceAll(inData, `"`+rotated+`"`, `"`+added+`"`)
		}
		outData, ok := sseData(outEvent)
		if !ok {
			t.Fatalf("事件 %d 丢失了 data 行: %q", i, outEvent)
		}
		if got, want := decodeNumberJSON(t, outData), decodeNumberJSON(t, inData); !reflect.DeepEqual(got, want) {
			t.Errorf("事件 %d 除 id 外的内容被改变:\ngot  %v\nwant %v", i, got, want)
		}
		if strings.Contains(outData, `\u003c`) || strings.Contains(outData, `\u003e`) || strings.Contains(outData, `\u0026`) {
			t.Errorf("事件 %d 的 <、>、& 被 HTML 转义: %s", i, outData)
		}
	}

	// 文本增量原样保留，且 <、& 未被转义。
	if !strings.Contains(out, `"delta":"Hello <b> & "`) && !strings.Contains(out, `"Hello <b> & "`) {
		t.Errorf("输出中找不到未转义的文本增量 `Hello <b> & `:\n%s", out)
	}
	var deltas []string
	for _, event := range outEvents {
		data, _ := sseData(event)
		v := decodeNumberJSON(t, data)
		if v["type"] == "response.output_text.delta" {
			deltas = append(deltas, v["delta"].(string))
		}
	}
	if want := []string{"Hello <b> & ", `"q" world`}; !reflect.DeepEqual(deltas, want) {
		t.Errorf("文本增量 = %q, want %q", deltas, want)
	}

	// 汇总事件中的数字写法原样保留。
	last, _ := sseData(outEvents[len(outEvents)-1])
	usage := decodeNumberJSON(t, last)["response"].(map[string]any)["usage"].(map[string]any)
	for key, want := range map[string]string{"input_tokens": "1.0", "output_tokens": "12345678901234567890", "total_tokens": "2.50e1"} {
		if got, _ := usage[key].(json.Number); got.String() != want {
			t.Errorf("usage.%s = %v, want 原样 %s", key, usage[key], want)
		}
	}
	for _, raw := range []string{"1.0", "12345678901234567890", "2.50e1", `"resp_1"`, `"call_1"`} {
		if !strings.Contains(out, raw) {
			t.Errorf("输出中缺少原样内容 %s", raw)
		}
	}
}

// TestItemIDTidyRotatingStream 验证 Copilot 轮换 id 的流在不同分片方式下都被统一为 added 的 id。
func TestItemIDTidyRotatingStream(t *testing.T) {
	in := buildSSE(rotatingEvents(), "\n")
	for _, tc := range []struct {
		name  string
		chunk int
	}{{"all-at-once", 0}, {"7-byte", 7}, {"1-byte", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			written, flushed := runTidy(t, in, tc.chunk)
			if flushed != "" {
				t.Errorf("完整的流不应剩余字节, flush = %q", flushed)
			}
			assertTidiedRotatingStream(t, in, written, "\n")
		})
	}
}

// TestItemIDTidyCRLFStream 验证使用 "\r\n" 行尾的流同样被整理，且每个事件的 "\r\n\r\n" 结束符保留。
func TestItemIDTidyCRLFStream(t *testing.T) {
	in := buildSSE(rotatingEvents(), "\r\n")
	for _, chunk := range []int{0, 7} {
		written, flushed := runTidy(t, in, chunk)
		if flushed != "" {
			t.Errorf("chunk=%d 完整的流不应剩余字节, flush = %q", chunk, flushed)
		}
		assertTidiedRotatingStream(t, in, written, "\r\n")
		if strings.Count(written, "\n") != strings.Count(written, "\r\n") {
			t.Errorf("chunk=%d 输出出现了裸 \\n 行尾:\n%q", chunk, written)
		}
	}
}

// TestItemIDTidyPassesConsistentStreamVerbatim 验证 id 已一致的 OpenAI 流（含注释、[DONE]、非紧凑 JSON）逐字节不变。
func TestItemIDTidyPassesConsistentStreamVerbatim(t *testing.T) {
	in := ": keep-alive\n\n" +
		"event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\": \"response.output_item.added\", \"output_index\": 0, \"item\": {\"id\": \"msg_1\", \"type\": \"message\", \"content\": []}}\n\n" +
		"event: response.output_text.delta\n: inline comment\nid: 7\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"item_id\":\"msg_1\",\"delta\":\"a<b>&c \\u00e9\",\"logprob\":1.50}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"a<b>&c \\u00e9\"}]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\"}],\"usage\":{\"input_tokens\":1.0}}}\n\n" +
		"data: [DONE]\n\n"
	for _, chunk := range []int{0, 7} {
		written, flushed := runTidy(t, in, chunk)
		if got := written + flushed; got != in {
			t.Errorf("chunk=%d 输出应与输入逐字节相同:\ngot  %q\nwant %q", chunk, got, in)
		}
	}
}

// TestItemIDTidyFlushReturnsIncompleteTail 验证末尾没有空行的事件不由 write 返回，而由 flush 原样返回且不改写。
func TestItemIDTidyFlushReturnsIncompleteTail(t *testing.T) {
	added := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_A\",\"type\":\"message\"}}\n\n"
	tail := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"item_id\":\"msg_B\",\"delta\":\"x\"}\n"
	tidy := &itemIDTidy{}
	if got := string(tidy.write([]byte(added + tail))); got != added {
		t.Fatalf("write = %q, want 仅完整的 added 事件 %q", got, added)
	}
	if got := string(tidy.flush()); got != tail {
		t.Fatalf("flush = %q, want 原样剩余 %q", got, tail)
	}
}

// TestItemIDTidyKeepsIDWhenItemTypeDiffers 验证 item.type 与 added 不同时保留其自身 id；item_id 仍按索引统一。
func TestItemIDTidyKeepsIDWhenItemTypeDiffers(t *testing.T) {
	added := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_A\",\"type\":\"message\"}}\n\n"
	delta := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"item_id\":\"msg_B\",\"delta\":\"x\"}\n\n"
	done := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_Z\",\"type\":\"reasoning\"}}\n\n"
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"id\":\"rs_Y\",\"type\":\"reasoning\"}]}}\n\n"
	written, flushed := runTidy(t, added+delta+done+completed, 0)
	if flushed != "" {
		t.Fatalf("flush = %q, want 空", flushed)
	}
	events, _ := splitSSE(written)
	if len(events) != 4 {
		t.Fatalf("事件数量 = %d, want 4\n%s", len(events), written)
	}
	if events[0] != added || events[2] != done || events[3] != completed {
		t.Errorf("类型不匹配的事件应逐字节保持不变:\n%s", written)
	}
	data, _ := sseData(events[1])
	if got := decodeNumberJSON(t, data)["item_id"]; got != "msg_A" {
		t.Errorf("delta 的 item_id = %v, want msg_A", got)
	}
}

// TestItemIDTidyMultiLineData 验证 data 分多行时按 SSE 规则连接后再识别并改写。
func TestItemIDTidyMultiLineData(t *testing.T) {
	added := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\n" +
		"data: \"output_index\":0,\"item\":{\"id\":\"msg_A\",\"type\":\"message\"}}\n\n"
	delta := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\n" +
		"data: \"output_index\":0,\"item_id\":\"msg_B\",\"delta\":\"x\"}\n\n"
	written, _ := runTidy(t, added+delta, 7)
	events, _ := splitSSE(written)
	if len(events) != 2 || events[0] != added {
		t.Fatalf("输出 = %q", written)
	}
	if got := sseOtherLines(events[1]); !reflect.DeepEqual(got, []string{"event: response.output_text.delta"}) {
		t.Errorf("非 data 行 = %q", got)
	}
	data, _ := sseData(events[1])
	v := decodeNumberJSON(t, data)
	if v["item_id"] != "msg_A" || v["delta"] != "x" {
		t.Errorf("改写后的 data = %v", v)
	}
}

// itemIDUpstream 构造一个假 Copilot 上游：generation 路径按 7 字节分片并逐片 flush 返回 body。
func itemIDUpstream(t *testing.T, path, contentType, body string) *Service {
	t.Helper()
	return testService(t, Config{APIKey: testAPIKey, Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case path:
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(http.StatusOK)
			for start := 0; start < len(body); start += 7 {
				_, _ = io.WriteString(w, body[start:min(start+7, len(body))])
				w.(http.Flusher).Flush()
			}
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})})
}

// TestResponsesStreamItemIDsAreTidied 验证经真实 handler 的 /v1/responses SSE 流被整理为一致的 id。
func TestResponsesStreamItemIDsAreTidied(t *testing.T) {
	in := buildSSE(rotatingEvents(), "\n")
	s := itemIDUpstream(t, "/responses", "text/event-stream", in)
	w := testRequest(s, "POST", "/v1/responses", `{"model":"gpt-4.1","input":"hello","stream":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	assertTidiedRotatingStream(t, in, w.Body.String(), "\n")
}

// TestOtherGenerationRepliesAreForwardedVerbatim 验证 chat completions、/v1/messages 的 SSE
// 以及 /responses 的非 SSE 回复都不经整理，逐字节透传。
func TestOtherGenerationRepliesAreForwardedVerbatim(t *testing.T) {
	stream := buildSSE(rotatingEvents(), "\n")
	for _, tc := range []struct {
		name, local, upstream, contentType, body string
	}{
		{"chat-completions-sse", "/v1/chat/completions", "/chat/completions", "text/event-stream",
			`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}],"stream":true}`},
		{"messages-sse", "/v1/messages", "/v1/messages", "text/event-stream",
			`{"model":"gpt-4.1","max_tokens":5,"messages":[{"role":"user","content":"hello"}],"stream":true}`},
		// 非 SSE 的 /responses 回复：内容故意带 SSE 分帧与轮换 id，若被错误整理即可察觉。
		{"responses-json", "/v1/responses", "/responses", "application/json",
			`{"model":"gpt-4.1","input":"hello"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := itemIDUpstream(t, tc.upstream, tc.contentType, stream)
			w := testRequest(s, "POST", tc.local, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			if got := w.Body.Bytes(); !bytes.Equal(got, []byte(stream)) {
				t.Errorf("回复应逐字节透传:\ngot  %q\nwant %q", got, stream)
			}
		})
	}
}

// TestResponsesJSONReplyWithRotatingIDsIsVerbatim 验证 /responses 的普通 JSON 回复（application/json）即使 id 各不相同也原样返回。
func TestResponsesJSONReplyWithRotatingIDsIsVerbatim(t *testing.T) {
	events := rotatingEvents()
	body := events[len(events)-1].data
	s := itemIDUpstream(t, "/responses", "application/json", body)
	w := testRequest(s, "POST", "/v1/responses", `{"model":"gpt-4.1","input":"hello"}`)
	if w.Code != http.StatusOK || w.Body.String() != body {
		t.Fatalf("status = %d, 回复应逐字节透传:\ngot  %q\nwant %q", w.Code, w.Body.String(), body)
	}
}
