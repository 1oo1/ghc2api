package copilot

// 本文件覆盖“去掉没有 tools 时的 tool_choice”：Copilot 上游 /responses 在请求带 tool_choice 却未声明任何工具时
// 直接返回 400（Codex 上下文压缩请求即如此），因此只对转发到上游 /responses 的请求删去这种孤立的 tool_choice；
// 其余协议路径的请求体保持逐字节透传。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

// decodeNumberMap 以 UseNumber 解析 JSON 对象，数字按原文保留，便于比较字段语义是否一致。
func decodeNumberMap(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("解析 JSON 对象失败: %v; body=%s", err, data)
	}
	if m == nil {
		t.Fatalf("期望 JSON 对象，得到 null; body=%s", data)
	}
	return m
}

// TestWithoutLoneToolChoiceStrips 顶层 tools 缺失、为 null 或空数组时删去顶层 tool_choice，其他字段语义不变。
func TestWithoutLoneToolChoiceStrips(t *testing.T) {
	// 公共字段：含 HTML 敏感字符、需原样保留的数字写法，以及嵌套的 tool_choice（不得被改动）。
	const common = `"model":"gpt-4.1","input":[{"role":"user","content":"<b> & c tool_choice"}],` +
		`"temperature":1.0,"seed":12345678901234567890,"metadata":{"tool_choice":"required","n":2.50}`
	for _, tc := range []struct {
		name string
		body string
	}{
		{"tools-absent-auto", `{` + common + `,"tool_choice":"auto"}`},
		{"tools-absent-none", `{"tool_choice":"none",` + common + `}`},
		{"tools-absent-required", `{` + common + `,"tool_choice":"required"}`},
		{"tools-absent-object", `{` + common + `,"tool_choice":{"type":"function","name":"f"}}`},
		{"tools-absent-null-choice", `{` + common + `,"tool_choice":null}`},
		{"tools-null-auto", `{` + common + `,"tools":null,"tool_choice":"auto"}`},
		{"tools-empty-auto", `{` + common + `,"tools":[],"tool_choice":"auto"}`},
		{"tools-empty-spaced-object-choice", `{ "tools" : [ ] , ` + common + ` , "tool_choice" : {"type":"auto"} }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.body)
			original := append([]byte(nil), in...)
			out := withoutLoneToolChoice(in)
			if !bytes.Equal(in, original) {
				t.Fatalf("输入切片被修改: %s", in)
			}
			got := decodeNumberMap(t, out)
			if _, ok := got["tool_choice"]; ok {
				t.Fatalf("顶层 tool_choice 未被删除: %s", out)
			}
			want := decodeNumberMap(t, in)
			delete(want, "tool_choice")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("其他字段语义变化:\n got=%v\nwant=%v\n out=%s", got, want, out)
			}
			for _, literal := range []string{`<b> & c`, `1.0`, `12345678901234567890`, `2.50`, `"tool_choice":"required"`} {
				if !bytes.Contains(out, []byte(literal)) {
					t.Fatalf("输出缺少原样片段 %q（不得 HTML 转义或改写数字）: %s", literal, out)
				}
			}
		})
	}
}

// TestWithoutLoneToolChoiceKeeps 不满足删除条件时原样返回同一份字节。
func TestWithoutLoneToolChoiceKeeps(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"tools-non-empty", `{"model":"gpt-4.1","input":"hi","tools":[{"type":"function","name":"f"}],"tool_choice":"auto"}`},
		{"tools-non-empty-required", `{"tool_choice":"required","tools":[{"type":"web_search"}],"input":"hi"}`},
		{"tools-object", `{"model":"gpt-4.1","tools":{},"tool_choice":"auto"}`},
		{"tools-string", `{"model":"gpt-4.1","tools":"x","tool_choice":"auto"}`},
		{"tools-number", `{"model":"gpt-4.1","tools":0,"tool_choice":"auto"}`},
		{"no-tool-choice", `{"model":"gpt-4.1","input":"hi","temperature":1.0}`},
		{"no-tool-choice-with-empty-tools", `{"model":"gpt-4.1","input":"hi","tools":[]}`},
		{"nested-in-input-text", `{"model":"gpt-4.1","input":[{"role":"user","content":"\"tool_choice\":\"auto\""}]}`},
		{"nested-in-metadata", `{"model":"gpt-4.1","input":"hi","metadata":{"tool_choice":"auto"}}`},
		{"nested-in-input-item", `{"model":"gpt-4.1","input":[{"type":"message","tool_choice":"auto"}]}`},
		{"invalid-json", `{"model":"gpt-4.1","tool_choice":"auto"`},
		{"invalid-json-trailing", `{"tool_choice":"auto"} x`},
		{"json-array", `[{"tool_choice":"auto"}]`},
		{"json-string", `"tool_choice"`},
		{"json-null", `null`},
		{"empty", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.body)
			original := append([]byte(nil), in...)
			out := withoutLoneToolChoice(in)
			if !bytes.Equal(out, original) {
				t.Fatalf("请求体应原样返回:\n got=%s\nwant=%s", out, original)
			}
		})
	}

	t.Run("nil", func(t *testing.T) {
		if out := withoutLoneToolChoice(nil); len(out) != 0 {
			t.Fatalf("nil 输入应原样返回，得到 %q", out)
		}
	})
}

// toolChoiceUpstream 构造一个服务，记录上游生成端点 upstream 收到的请求体。
func toolChoiceUpstream(t *testing.T, upstream string) (*Service, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var got []byte
	calls := 0
	s := testService(t, Config{APIKey: testAPIKey, Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case upstream:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("读取上游请求体失败: %v", err)
			}
			mu.Lock()
			got, calls = data, calls+1
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})})
	return s, func() []byte {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if calls != 1 {
			t.Fatalf("上游 %s 调用次数 = %d, want 1", upstream, calls)
		}
		return got
	}
}

// TestResponsesDropsLoneToolChoice /v1/responses 与 /responses 转发前删去孤立的 tool_choice，其余字段不变。
func TestResponsesDropsLoneToolChoice(t *testing.T) {
	const body = `{"model":"gpt-4.1","input":"hi <b> & c","temperature":1.0,"tool_choice":"auto"}`
	for _, path := range []string{"/v1/responses", "/responses"} {
		t.Run(path, func(t *testing.T) {
			s, upstreamBody := toolChoiceUpstream(t, "/responses")
			if w := testRequest(s, "POST", path, body); w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			out := upstreamBody()
			got := decodeNumberMap(t, out)
			if _, ok := got["tool_choice"]; ok {
				t.Fatalf("上游请求体仍含 tool_choice: %s", out)
			}
			want := decodeNumberMap(t, []byte(body))
			delete(want, "tool_choice")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("上游请求体字段变化:\n got=%v\nwant=%v", got, want)
			}
		})
	}
}

// TestResponsesKeepsToolChoiceWithTools 声明了工具时 tool_choice 保留，请求体逐字节透传。
func TestResponsesKeepsToolChoiceWithTools(t *testing.T) {
	const body = `{"model":"gpt-4.1","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"tool_choice":"auto"}`
	s, upstreamBody := toolChoiceUpstream(t, "/responses")
	if w := testRequest(s, "POST", "/v1/responses", body); w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if out := upstreamBody(); string(out) != body {
		t.Fatalf("上游请求体应逐字节一致:\n got=%s\nwant=%s", out, body)
	}
}

// TestOtherProtocolsKeepLoneToolChoice /v1/chat/completions 与 /v1/messages 即使带孤立 tool_choice 也逐字节透传。
func TestOtherProtocolsKeepLoneToolChoice(t *testing.T) {
	for _, tc := range []struct{ path, upstream, body string }{
		{"/v1/chat/completions", "/chat/completions", `{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto"}`},
		{"/v1/messages", "/v1/messages", `{"model":"gpt-4.1","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto"}}`},
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
