package copilot

// 本文件覆盖“去掉 Codex output token replay 带来的 encrypted_content”：Codex 在 provider 名为 OpenAI 时，
// 会在 Responses 请求的顶层 include 加入 "output.encrypted_content"，并让 input 中 message、function_call 等项带上 encrypted_content，
// Copilot 上游 /responses 对此返回 400。因此转发到上游 /responses 前：
//   - 删除顶层 include 中的 "output.encrypted_content"（删空则删去 include 键），其余值与顺序保留；
//   - 删除 input 中非 reasoning/compaction 类对象项的顶层 encrypted_content（缺 type 的项也删除）；
//   - 无需改动、请求体非 JSON 对象时返回与输入完全相同的字节，且不修改输入切片；
//   - 有改动时重编码，不转义 <>&，数字原文保留，键顺序可能变化（按语义比较）。
//
// /chat/completions 与 /v1/messages 的请求体逐字节透传。

import (
	"bytes"
	"reflect"
	"testing"
)

// assertSameJSON 以 UseNumber 解析后按语义比较两份 JSON 对象。
func assertSameJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	g, w := decodeNumberMap(t, got), decodeNumberMap(t, []byte(want))
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("语义不一致:\n got=%s\nwant=%s", got, want)
	}
}

// runOutputReplay 调用 withoutOutputReplay，并断言输入切片未被修改。
func runOutputReplay(t *testing.T, body string) []byte {
	t.Helper()
	in := []byte(body)
	original := append([]byte(nil), in...)
	out := withoutOutputReplay(in)
	if !bytes.Equal(in, original) {
		t.Fatalf("输入切片被修改:\n got=%s\nwant=%s", in, original)
	}
	return out
}

// TestWithoutOutputReplayInclude 只有 include 时：删除 output.encrypted_content 的所有出现，其余值与顺序保留；删空则删去 include 键。
func TestWithoutOutputReplayInclude(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			"keep-others-in-order",
			`{"model":"gpt-5","input":"hi","include":["reasoning.encrypted_content","output.encrypted_content","message.output_text.logprobs"]}`,
			`{"model":"gpt-5","input":"hi","include":["reasoning.encrypted_content","message.output_text.logprobs"]}`,
		},
		{
			"first-position",
			`{"model":"gpt-5","include":["output.encrypted_content","reasoning.encrypted_content"]}`,
			`{"model":"gpt-5","include":["reasoning.encrypted_content"]}`,
		},
		{
			"duplicates-removed-all",
			`{"model":"gpt-5","include":["output.encrypted_content","a","output.encrypted_content","b","output.encrypted_content"]}`,
			`{"model":"gpt-5","include":["a","b"]}`,
		},
		{
			"only-value-drops-key",
			`{"model":"gpt-5","input":"hi","include":["output.encrypted_content"]}`,
			`{"model":"gpt-5","input":"hi"}`,
		},
		{
			"only-duplicates-drops-key",
			`{"model":"gpt-5","include":["output.encrypted_content","output.encrypted_content"]}`,
			`{"model":"gpt-5"}`,
		},
		{
			"spaced-json",
			"{ \"include\" : [ \"output.encrypted_content\" , \"x\" ] ,\n \"model\" : \"gpt-5\" }",
			`{"include":["x"],"model":"gpt-5"}`,
		},
		{
			"nested-include-untouched",
			`{"model":"gpt-5","include":["output.encrypted_content"],"metadata":{"include":["output.encrypted_content"]}}`,
			`{"model":"gpt-5","metadata":{"include":["output.encrypted_content"]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runOutputReplay(t, tc.body)
			assertSameJSON(t, out, tc.want)
		})
	}
}

// TestWithoutOutputReplayInputStrips 只有 input 时：非 reasoning/compaction 类对象项（含缺 type、type 非字符串）删去顶层 encrypted_content，其余字段不变。
func TestWithoutOutputReplayInputStrips(t *testing.T) {
	const body = `{"model":"gpt-5","input":[` +
		`{"type":"message","role":"assistant","encrypted_content":"AAA","content":[{"type":"output_text","text":"hi"}]},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}","encrypted_content":"BBB"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok","encrypted_content":"CCC"},` +
		`{"type":"custom_tool_call","call_id":"c2","encrypted_content":"DDD"},` +
		`{"role":"assistant","content":"no type","encrypted_content":"EEE"},` +
		`{"type":null,"encrypted_content":"FFF","k":1},` +
		`{"type":5,"encrypted_content":"GGG"},` +
		`{"type":"message","encrypted_content":null},` +
		`{"type":"message","role":"user","content":"plain"}` +
		`]}`
	const want = `{"model":"gpt-5","input":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok"},` +
		`{"type":"custom_tool_call","call_id":"c2"},` +
		`{"role":"assistant","content":"no type"},` +
		`{"type":null,"k":1},` +
		`{"type":5},` +
		`{"type":"message"},` +
		`{"type":"message","role":"user","content":"plain"}` +
		`]}`
	out := runOutputReplay(t, body)
	assertSameJSON(t, out, want)
	if bytes.Contains(out, []byte("encrypted_content")) {
		t.Fatalf("输出仍含 encrypted_content: %s", out)
	}
}

// TestWithoutOutputReplayKeepsReasoningAndCompaction reasoning/compaction 类项的 encrypted_content 原样保留，其余项照常删除。
func TestWithoutOutputReplayKeepsReasoningAndCompaction(t *testing.T) {
	const body = `{"model":"gpt-5","input":[` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"R1"},` +
		`{"type":"compaction","encrypted_content":"C1"},` +
		`{"type":"compaction_summary","encrypted_content":"C2"},` +
		`{"type":"context_compaction","encrypted_content":"C3"},` +
		`{"type":"reasoning","encrypted_content":null},` +
		`{"type":"message","role":"assistant","encrypted_content":"M1"}` +
		`]}`
	const want = `{"model":"gpt-5","input":[` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"R1"},` +
		`{"type":"compaction","encrypted_content":"C1"},` +
		`{"type":"compaction_summary","encrypted_content":"C2"},` +
		`{"type":"context_compaction","encrypted_content":"C3"},` +
		`{"type":"reasoning","encrypted_content":null},` +
		`{"type":"message","role":"assistant"}` +
		`]}`
	assertSameJSON(t, runOutputReplay(t, body), want)
}

// TestWithoutOutputReplayAllKeepTypesIsIdentity 只含 reasoning/compaction 类带密文的项且无 include 改动时，返回原字节。
func TestWithoutOutputReplayAllKeepTypesIsIdentity(t *testing.T) {
	const body = `{"model":"gpt-5","input":[{"type":"reasoning","encrypted_content":"R"},{"type":"compaction","encrypted_content":"C"},` +
		`{"type":"compaction_summary","encrypted_content":"S"},{"type":"context_compaction","encrypted_content":"X"}],` +
		`"include":["reasoning.encrypted_content"]}`
	if out := runOutputReplay(t, body); string(out) != body {
		t.Fatalf("应原样返回:\n got=%s\nwant=%s", out, body)
	}
}

// TestWithoutOutputReplayBoth include 与 input 同时存在时两处都处理。
func TestWithoutOutputReplayBoth(t *testing.T) {
	const body = `{"model":"gpt-5","include":["reasoning.encrypted_content","output.encrypted_content"],` +
		`"input":[{"type":"reasoning","encrypted_content":"R"},{"type":"message","role":"assistant","encrypted_content":"M","content":"x"}]}`
	const want = `{"model":"gpt-5","include":["reasoning.encrypted_content"],` +
		`"input":[{"type":"reasoning","encrypted_content":"R"},{"type":"message","role":"assistant","content":"x"}]}`
	assertSameJSON(t, runOutputReplay(t, body), want)

	const onlyKey = `{"include":["output.encrypted_content"],"input":[{"type":"message","encrypted_content":"M"}],"model":"gpt-5"}`
	assertSameJSON(t, runOutputReplay(t, onlyKey), `{"input":[{"type":"message"}],"model":"gpt-5"}`)
}

// TestWithoutOutputReplayNonObjectAndNested 非对象 input 项原样保留；只处理项的顶层 encrypted_content，更深层的同名键不动；input 之外的同名键不动。
func TestWithoutOutputReplayNonObjectAndNested(t *testing.T) {
	const body = `{"model":"gpt-5","encrypted_content":"top","metadata":{"encrypted_content":"meta","input":[{"type":"message","encrypted_content":"deep"}]},"input":[` +
		`"str",42,null,true,["x",{"type":"message","encrypted_content":"in-array"}],` +
		`{"type":"message","encrypted_content":"M","content":[{"type":"output_text","text":"t","encrypted_content":"nested"}],"extra":{"encrypted_content":"extra"}}` +
		`]}`
	const want = `{"model":"gpt-5","encrypted_content":"top","metadata":{"encrypted_content":"meta","input":[{"type":"message","encrypted_content":"deep"}]},"input":[` +
		`"str",42,null,true,["x",{"type":"message","encrypted_content":"in-array"}],` +
		`{"type":"message","content":[{"type":"output_text","text":"t","encrypted_content":"nested"}],"extra":{"encrypted_content":"extra"}}` +
		`]}`
	assertSameJSON(t, runOutputReplay(t, body), want)
}

// TestWithoutOutputReplayKeepsBytes 无需改动或无法处理时返回与输入完全相同的字节。
func TestWithoutOutputReplayKeepsBytes(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no-fields", `{"model":"gpt-5","input":"hi","temperature":1.0}`},
		{"include-without-target", `{"model":"gpt-5","include":["reasoning.encrypted_content"]}`},
		{"include-empty-array", `{"model":"gpt-5","include":[]}`},
		{"input-string", `{"model":"gpt-5","input":"encrypted_content output.encrypted_content"}`},
		{"input-items-without-encrypted", `{"model":"gpt-5","input":[{"type":"message","role":"user","content":"hi"}]}`},
		{"input-text-mentions-field", `{"model":"gpt-5","input":[{"type":"message","content":"\"encrypted_content\":\"x\" output.encrypted_content"}]}`},
		{"nested-only", `{"model":"gpt-5","input":[{"type":"message","content":[{"type":"output_text","encrypted_content":"n"}]}],"metadata":{"include":["output.encrypted_content"]}}`},
		{"spaced-no-change", "{ \"model\" : \"gpt-5\" ,\n \"input\" : [ ] }"},
		{"invalid-json", `{"model":"gpt-5","include":["output.encrypted_content"]`},
		{"invalid-json-trailing", `{"include":["output.encrypted_content"]} x`},
		{"json-array", `[{"include":["output.encrypted_content"]}]`},
		{"json-string", `"output.encrypted_content"`},
		{"json-null", `null`},
		{"empty", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out := runOutputReplay(t, tc.body); string(out) != tc.body {
				t.Fatalf("请求体应原样返回:\n got=%s\nwant=%s", out, tc.body)
			}
		})
	}

	t.Run("nil", func(t *testing.T) {
		if out := withoutOutputReplay(nil); len(out) != 0 {
			t.Fatalf("nil 输入应原样返回，得到 %q", out)
		}
	})
}

// TestWithoutOutputReplayIncludeNotStringArray include 不是字符串数组时不改 include（即使含目标值），input 规则仍适用；无 input 改动时返回原字节。
func TestWithoutOutputReplayIncludeNotStringArray(t *testing.T) {
	for _, tc := range []struct{ name, include string }{
		{"mixed-array", `["output.encrypted_content",1]`},
		{"array-with-null", `["output.encrypted_content",null]`},
		{"array-with-object", `["output.encrypted_content",{"a":"b"}]`},
		{"string", `"output.encrypted_content"`},
		{"object", `{"output.encrypted_content":true}`},
		{"number", `1`},
		{"null", `null`},
	} {
		t.Run(tc.name+"/identity", func(t *testing.T) {
			body := `{"model":"gpt-5","input":[{"type":"message","content":"hi"}],"include":` + tc.include + `}`
			if out := runOutputReplay(t, body); string(out) != body {
				t.Fatalf("应原样返回:\n got=%s\nwant=%s", out, body)
			}
		})
		t.Run(tc.name+"/input-still-applies", func(t *testing.T) {
			body := `{"model":"gpt-5","include":` + tc.include + `,"input":[{"type":"message","encrypted_content":"M"}]}`
			want := `{"model":"gpt-5","include":` + tc.include + `,"input":[{"type":"message"}]}`
			assertSameJSON(t, runOutputReplay(t, body), want)
		})
	}
}

// TestWithoutOutputReplayInputNotArray input 不是数组时不改 input（即使含 encrypted_content），include 规则仍适用。
func TestWithoutOutputReplayInputNotArray(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"string", `"hi"`},
		{"object", `{"type":"message","encrypted_content":"M"}`},
		{"null", `null`},
		{"number", `3`},
	} {
		t.Run(tc.name+"/identity", func(t *testing.T) {
			body := `{"model":"gpt-5","input":` + tc.input + `,"include":["reasoning.encrypted_content"]}`
			if out := runOutputReplay(t, body); string(out) != body {
				t.Fatalf("应原样返回:\n got=%s\nwant=%s", out, body)
			}
		})
		t.Run(tc.name+"/include-still-applies", func(t *testing.T) {
			body := `{"model":"gpt-5","input":` + tc.input + `,"include":["output.encrypted_content","x"]}`
			want := `{"model":"gpt-5","input":` + tc.input + `,"include":["x"]}`
			assertSameJSON(t, runOutputReplay(t, body), want)
		})
	}
}

// TestWithoutOutputReplayPreservesLiterals 有改动重编码时不转义 <>&，数字原文（含大整数、尾零）与未改动项保持不变。
func TestWithoutOutputReplayPreservesLiterals(t *testing.T) {
	const body = `{"model":"gpt-5","temperature":1.0,"seed":12345678901234567890,"include":["output.encrypted_content"],` +
		`"input":[{"type":"message","role":"user","content":"<b> & c","n":2.50,"encrypted_content":"M"},` +
		`{"type":"reasoning","encrypted_content":"R<>&","big":1e3}],"metadata":{"n":0.10,"s":"<i>&"}}`
	out := runOutputReplay(t, body)
	if bytes.Contains(out, []byte("output.encrypted_content")) || bytes.Contains(out, []byte(`"encrypted_content":"M"`)) {
		t.Fatalf("应删去的内容仍在: %s", out)
	}
	for _, literal := range []string{`<b> & c`, `R<>&`, `<i>&`, `1.0`, `12345678901234567890`, `2.50`, `1e3`, `0.10`, `"encrypted_content":"R<>&"`} {
		if !bytes.Contains(out, []byte(literal)) {
			t.Fatalf("输出缺少原样片段 %q（不得 HTML 转义或改写数字）: %s", literal, out)
		}
	}
	want := `{"model":"gpt-5","temperature":1.0,"seed":12345678901234567890,` +
		`"input":[{"type":"message","role":"user","content":"<b> & c","n":2.50},` +
		`{"type":"reasoning","encrypted_content":"R<>&","big":1e3}],"metadata":{"n":0.10,"s":"<i>&"}}`
	assertSameJSON(t, out, want)
}

// TestWithoutOutputReplayDoesNotMutateInput 即使有改动，输入切片（含其底层数组）也不得被修改，输出不与输入共享内存。
func TestWithoutOutputReplayDoesNotMutateInput(t *testing.T) {
	in := []byte(`{"model":"gpt-5","include":["output.encrypted_content","x"],"input":[{"type":"message","encrypted_content":"M"}]}`)
	original := append([]byte(nil), in...)
	out := withoutOutputReplay(in)
	if bytes.Equal(out, original) {
		t.Fatalf("本例应有改动: %s", out)
	}
	for i := range out {
		out[i] = 'X'
	}
	if !bytes.Equal(in, original) {
		t.Fatalf("输入切片被修改或与输出共享内存:\n got=%s\nwant=%s", in, original)
	}
}

// outputReplayBody 是 Codex 开启 output token replay 时的典型 Responses 请求体（含 HTML 敏感字符与需保留的数字写法）。
const outputReplayBody = `{"model":"gpt-4.1","temperature":1.0,"include":["reasoning.encrypted_content","output.encrypted_content"],` +
	`"input":[{"type":"message","role":"user","content":"hi <b> & c"},` +
	`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"yo"}],"encrypted_content":"SEALED"},` +
	`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}","encrypted_content":"SEALED2"},` +
	`{"type":"reasoning","summary":[],"encrypted_content":"KEEP"}]}`

const outputReplayWant = `{"model":"gpt-4.1","temperature":1.0,"include":["reasoning.encrypted_content"],` +
	`"input":[{"type":"message","role":"user","content":"hi <b> & c"},` +
	`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"yo"}]},` +
	`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
	`{"type":"reasoning","summary":[],"encrypted_content":"KEEP"}]}`

// TestResponsesDropsOutputReplay /v1/responses 与 /responses 转发前去掉 include 中的 output.encrypted_content 与 input 项的 encrypted_content。
func TestResponsesDropsOutputReplay(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/responses"} {
		t.Run(path, func(t *testing.T) {
			s, upstreamBody := toolChoiceUpstream(t, "/responses")
			if w := testRequest(s, "POST", path, outputReplayBody); w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			out := upstreamBody()
			assertSameJSON(t, out, outputReplayWant)
			if bytes.Contains(out, []byte("output.encrypted_content")) || bytes.Contains(out, []byte("SEALED")) {
				t.Fatalf("上游请求体仍含应删除的内容: %s", out)
			}
			if !bytes.Contains(out, []byte(`hi <b> & c`)) || !bytes.Contains(out, []byte(`1.0`)) {
				t.Fatalf("上游请求体改写了 HTML 字符或数字: %s", out)
			}
		})
	}
}

// TestResponsesDropsOutputReplayIncludeKey include 只含 output.encrypted_content 时，上游请求体不再有 include 键。
func TestResponsesDropsOutputReplayIncludeKey(t *testing.T) {
	const body = `{"model":"gpt-4.1","input":"hi","include":["output.encrypted_content"]}`
	s, upstreamBody := toolChoiceUpstream(t, "/responses")
	if w := testRequest(s, "POST", "/v1/responses", body); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	assertSameJSON(t, upstreamBody(), `{"model":"gpt-4.1","input":"hi"}`)
}

// TestResponsesKeepsBodyWithoutOutputReplay 没有 output replay 内容的 /responses 请求体逐字节透传（reasoning 的 encrypted_content 与其他 include 值保留）。
func TestResponsesKeepsBodyWithoutOutputReplay(t *testing.T) {
	const body = `{"model":"gpt-4.1","include":["reasoning.encrypted_content"],"input":[{"type":"reasoning","summary":[],"encrypted_content":"KEEP"}],"temperature":1.0}`
	s, upstreamBody := toolChoiceUpstream(t, "/responses")
	if w := testRequest(s, "POST", "/v1/responses", body); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if out := upstreamBody(); string(out) != body {
		t.Fatalf("上游请求体应逐字节一致:\n got=%s\nwant=%s", out, body)
	}
}

// TestResponsesDropsOutputReplayAndLoneToolChoice 无 tools 却有 tool_choice 时，孤立 tool_choice 与 output replay 内容都被处理。
func TestResponsesDropsOutputReplayAndLoneToolChoice(t *testing.T) {
	const body = `{"model":"gpt-4.1","tool_choice":"auto","include":["output.encrypted_content"],` +
		`"input":[{"type":"message","role":"assistant","content":"x","encrypted_content":"SEALED"}]}`
	s, upstreamBody := toolChoiceUpstream(t, "/responses")
	if w := testRequest(s, "POST", "/v1/responses", body); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	out := upstreamBody()
	assertSameJSON(t, out, `{"model":"gpt-4.1","input":[{"type":"message","role":"assistant","content":"x"}]}`)
	if bytes.Contains(out, []byte("tool_choice")) || bytes.Contains(out, []byte("encrypted_content")) {
		t.Fatalf("上游请求体仍含应删除的内容: %s", out)
	}
}

// TestResponsesDropsOutputReplayKeepsToolChoiceWithTools 声明了工具时 tool_choice 保留，output replay 内容仍被删除。
func TestResponsesDropsOutputReplayKeepsToolChoiceWithTools(t *testing.T) {
	const body = `{"model":"gpt-4.1","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"tool_choice":"auto",` +
		`"include":["output.encrypted_content"],"input":[{"type":"function_call","call_id":"c","name":"f","encrypted_content":"SEALED"}]}`
	s, upstreamBody := toolChoiceUpstream(t, "/responses")
	if w := testRequest(s, "POST", "/v1/responses", body); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	assertSameJSON(t, upstreamBody(), `{"model":"gpt-4.1","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"tool_choice":"auto",`+
		`"input":[{"type":"function_call","call_id":"c","name":"f"}]}`)
}

// TestOtherProtocolsKeepOutputReplay /v1/chat/completions 与 /v1/messages 即使带同样字段也逐字节透传。
func TestOtherProtocolsKeepOutputReplay(t *testing.T) {
	for _, tc := range []struct{ path, upstream, body string }{
		{"/v1/chat/completions", "/chat/completions", `{"model":"gpt-4.1","messages":[{"role":"user","content":"hi","encrypted_content":"SEALED"}],` +
			`"input":[{"type":"message","encrypted_content":"SEALED"}],"include":["output.encrypted_content"]}`},
		{"/v1/messages", "/v1/messages", `{"model":"gpt-4.1","max_tokens":10,"messages":[{"role":"user","content":"hi","encrypted_content":"SEALED"}],` +
			`"input":[{"type":"message","encrypted_content":"SEALED"}],"include":["output.encrypted_content"]}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			s, upstreamBody := toolChoiceUpstream(t, tc.upstream)
			if w := testRequest(s, "POST", tc.path, tc.body); w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if out := upstreamBody(); string(out) != tc.body {
				t.Fatalf("上游请求体应逐字节一致:\n got=%s\nwant=%s", out, tc.body)
			}
		})
	}
}
