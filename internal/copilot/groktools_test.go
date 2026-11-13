package copilot

// 本文件覆盖 Copilot 的 Grok 模型在 /responses 上的工具适配的请求侧与共用部分（groktools.go）。
//
// 背景：Copilot 前面的 Grok（如 grok-4.7，走 /responses）对自定义工具（type custom，如 apply_patch）、namespace 工具、
// 参数根节点为 anyOf/oneOf/allOf 的 function 只回裸的 400/422，Codex 每个带工具的轮次都失败。
// 对 Grok 的做法（与 magpie 一致）：自定义工具当 function 发，namespace 摊平，根联合合并为 object；
// 历史里的 custom_tool_call、custom_tool_call_output 与带 namespace 的 function_call 同样转换；模型的调用在回复里还原（见 grokreply_test.go）。
//
// 期望行为：
//   - grokModel(id)：取最后一个 "/" 之后，小写后以 "grok" 开头。
//   - flatName(ns, name)：ns__name；超过 64 字节时取前 55 字节 + "_" + sha256(ns+"\x00"+name) 前 4 字节的十六进制。
//   - objectRoot(ps)：把 function 的 parameters 就地变成根为 object 的 schema，返回是否改动；与上游 provider.ObjectRoot 一致。
//   - customInput(args)：{"input":字符串} 的 input；否则 arguments 本身若是 JSON 字符串则取其值；否则原文。
//   - customStream.feed：逐段读出 arguments 中 input 字符串的已解码文本，读不全的转义与字符留给下一段。
//   - grokRequest(body)：改写顶层 tools、tool_choice 与 input，返回新请求体和改名/自定义调用表；无改动返回原切片，不修改输入切片。
//
// 实现者必须提供的内部名字（测试按此书写）：
//   - func grokModel(id string) bool
//   - type grokCall struct { Namespace, Name string; Custom bool }；type grokCalls map[string]grokCall
//   - func grokRequest(body []byte) ([]byte, grokCalls)
//   - func objectRoot(ps map[string]any) bool
//   - func flatName(namespace, name string) string
//   - func customInput(args string) string
//   - type customStream；func (c *customStream) feed(s string) string
//
// 期望值参照 magpie 的 internal/provider/grok_test.go（ObjectRoot、FlatName、GrokBody 相关）与
// internal/gateway/call_args_stream_test.go（TestCustomInputStreams）。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	// grokCustomSchema 是自定义工具改成 function 后的 parameters，文本与上游 customSchema 完全一致。
	grokCustomSchema = `{"type":"object","properties":{"input":{"type":"string","description":"The tool's free-form input, the whole of it, exactly as it is to be run."}},"required":["input"],"additionalProperties":false}`
	// grokCustomNote 是 customDescription 固定追加的句子，文本与上游完全一致。
	grokCustomNote = "Put the tool's whole free-form input, as it is to be run, in the `input` string; it is passed on as written, so it is not itself JSON."
)

// grokCustomFn 返回自定义工具改成 function 之后的 JSON。
func grokCustomFn(name, description string) string {
	return `{"type":"function","name":` + itemIDQuote(name) + `,"description":` + itemIDQuote(description) + `,"parameters":` + grokCustomSchema + `}`
}

// runGrokRequest 调用 grokRequest，并断言输入切片未被修改。
func runGrokRequest(t *testing.T, body string) ([]byte, grokCalls) {
	t.Helper()
	in := []byte(body)
	original := append([]byte(nil), in...)
	out, calls := grokRequest(in)
	if !bytes.Equal(in, original) {
		t.Fatalf("输入切片被修改:\n got=%s\nwant=%s", in, original)
	}
	return out, calls
}

// assertGrokCalls 断言调用表一致；nil 与空表等价。
func assertGrokCalls(t *testing.T, got, want grokCalls) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls 不一致:\n got=%+v\nwant=%+v", got, want)
	}
}

// grokToolList 返回输出请求体的 tools 数组（数字保持原文）。
func grokToolList(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	raw, ok := decodeNumberMap(t, body)["tools"].([]any)
	if !ok {
		t.Fatalf("输出没有 tools 数组: %s", body)
	}
	tools := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		tool, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("tools 项不是对象: %v", v)
		}
		tools = append(tools, tool)
	}
	return tools
}

// grokRawTools 返回输出请求体 tools 数组各元素的原始文本。
func grokRawTools(t *testing.T, body []byte) []string {
	t.Helper()
	var request struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("解析输出失败: %v; body=%s", err, body)
	}
	raw := make([]string, len(request.Tools))
	for i, tool := range request.Tools {
		raw[i] = string(tool)
	}
	return raw
}

// grokToolNames 返回 tools 各项的 name（没有 name 的为空串）。
func grokToolNames(tools []map[string]any) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i], _ = tool["name"].(string)
	}
	return names
}

// grokPopInput 取出转换后 function_call 项的 arguments 并从项中删去：arguments 必须是只含 input（字符串）的 JSON 对象，返回该 input。
func grokPopInput(t *testing.T, item map[string]any) string {
	t.Helper()
	args, ok := item["arguments"].(string)
	if !ok {
		t.Fatalf("arguments 不是字符串: %v", item)
	}
	delete(item, "arguments")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments 不是 JSON 对象: %v; %q", err, args)
	}
	input, ok := parsed["input"].(string)
	if !ok || len(parsed) != 1 {
		t.Fatalf("arguments 应恰为 {\"input\": 字符串}: %q", args)
	}
	return input
}

// assertGrokItem 断言 item（已去掉 arguments）与期望的 JSON 语义一致。
func assertGrokItem(t *testing.T, item map[string]any, want string) {
	t.Helper()
	if w := decodeNumberJSON(t, want); !reflect.DeepEqual(item, w) {
		t.Fatalf("input 项不一致:\n got=%v\nwant=%v", item, w)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// grokModel

// TestGrokModel 取最后一个 "/" 之后的部分，小写后以 grok 开头才是 Grok 模型。
func TestGrokModel(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"grok-4.7", true},
		{"Grok-4.7", true},
		{"GROK-CODE-FAST-1", true},
		{"grok", true},
		{"grokker", true},
		{"xai/grok-4.7", true},
		{"x-ai/Grok-4", true},
		{"copilot/grok-4.7", true},
		{"a/b/grok-4.7", true},
		{"/grok-4.7", true},
		{"gpt-4.1", false},
		{"gemini-3.8-flash", false},
		{"", false},
		{"xgrok-1", false},
		{"my-grok", false},
		{"grok-4.7/gpt-4.1", false},
		{"grok/", false},
		{"grok/gpt-4.1", false},
	} {
		if got := grokModel(tc.id); got != tc.want {
			t.Errorf("grokModel(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// flatName

// TestFlatName 名字不超过 64 字节时是 ns__name。
func TestFlatName(t *testing.T) {
	for _, tc := range []struct{ ns, name, want string }{
		{"collaboration", "spawn_agent", "collaboration__spawn_agent"},
		{"codex_app", "set_thread_title", "codex_app__set_thread_title"},
		{"mcp__codex_app", "automation_update", "mcp__codex_app__automation_update"},
		{"a", "b", "a__b"},
		// 恰好 64 字节：不截断。
		{strings.Repeat("a", 30), strings.Repeat("b", 32), strings.Repeat("a", 30) + "__" + strings.Repeat("b", 32)},
	} {
		if got := flatName(tc.ns, tc.name); got != tc.want {
			t.Errorf("flatName(%q, %q) = %q, want %q", tc.ns, tc.name, got, tc.want)
		}
	}
}

// TestFlatNameLong 超过 64 字节时取前 55 字节 + "_" + sha256(ns+"\x00"+name) 前 4 字节的十六进制，总长恰为 64；
// 同一输入结果不变，前 55 字节相同的不同名字不会撞到同一个结果。
func TestFlatNameLong(t *testing.T) {
	long := func(ns, name string) string {
		flat := ns + "__" + name
		sum := sha256.Sum256([]byte(ns + "\x00" + name))
		return flat[:55] + "_" + hex.EncodeToString(sum[:4])
	}
	for _, tc := range []struct{ ns, name string }{
		{strings.Repeat("a", 30), strings.Repeat("b", 33)}, // 65 字节
		{"mcp__codex_app", "a_very_long_tool_name_that_goes_on_and_on_and_on_and_on_and_on_forever"},
		{strings.Repeat("n", 70), "x"},
	} {
		got := flatName(tc.ns, tc.name)
		if want := long(tc.ns, tc.name); got != want {
			t.Errorf("flatName(%q, %q) = %q, want %q", tc.ns, tc.name, got, want)
		}
		if len(got) != 64 {
			t.Errorf("flatName(%q, %q) 长度 = %d, want 64", tc.ns, tc.name, len(got))
		}
		if again := flatName(tc.ns, tc.name); again != got {
			t.Errorf("同一输入的结果不稳定: %q vs %q", again, got)
		}
	}
	// 前 55 字节相同、之后不同的两个名字。
	prefix := strings.Repeat("p", 50)
	first, second := flatName("ns", prefix+"_one_with_a_tail"), flatName("ns", prefix+"_two_with_a_tail")
	if first == second {
		t.Errorf("不同的名字得到了同一个 flat 名: %q", first)
	}
	// 与上游 provider.FlatName 逐字符一致的一组固定值（由上游实现算出）。
	if got, want := flatName("mcp__codex_app", "a_very_long_tool_name_that_goes_on_and_on_and_on_and_on_and_on_forever"), grokUpstreamLongFlat; got != want {
		t.Errorf("与上游 FlatName 不一致: got %q, want %q", got, want)
	}
}

// grokUpstreamLongFlat 是上游 provider.FlatName("mcp__codex_app", "a_very_long_tool_name_…_forever") 的返回值。
const grokUpstreamLongFlat = "mcp__codex_app__a_very_long_tool_name_that_goes_on_and__e155b64e"

// ---------------------------------------------------------------------------------------------------------------------
// objectRoot

// parseSchema 解码 JSON schema（与上游测试一样用标准解码，数字为 float64）。
func parseSchema(t *testing.T, s string) map[string]any {
	t.Helper()
	var ps map[string]any
	if err := json.Unmarshal([]byte(s), &ps); err != nil {
		t.Fatalf("解析 schema 失败: %v\n%s", err, s)
	}
	return ps
}

// TestObjectRootKeepsOwnRequired 根节点与 allOf 自己的 required 在 anyOf/oneOf 分支旁仍然必填，分支只增加它们都要求的字段。
func TestObjectRootKeepsOwnRequired(t *testing.T) {
	ps := parseSchema(t, `{
		"type": "object",
		"properties": {"id": {"type": "string"}},
		"required": ["id"],
		"allOf": [{"properties": {"kind": {"type": "string"}}, "required": ["kind"]}],
		"anyOf": [
			{"type": "object", "properties": {"a": {"type": "string"}, "n": {"type": "integer"}}, "required": ["a", "n"]},
			{"type": "object", "properties": {"b": {"type": "string"}}, "required": ["b", "n", "id"]}
		]
	}`)
	if !objectRoot(ps) {
		t.Fatal("应报告有改动")
	}
	if got := fmt.Sprint(ps["required"]); got != "[id kind n]" {
		t.Fatalf("required = %s, want [id kind n]", got)
	}
	if props := ps["properties"].(map[string]any); len(props) != 5 {
		t.Fatalf("properties = %v, want 5 个字段", props)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if _, has := ps[key]; has {
			t.Errorf("根节点仍有 %s: %v", key, ps)
		}
	}
	if ps["type"] != "object" {
		t.Errorf("type = %v, want object", ps["type"])
	}
}

// TestObjectRootMergesBranches anyOf 的对象分支合并 properties，$ref 读 $defs，null 等非对象分支丢弃，所有分支都要求的字段才必填。
func TestObjectRootMergesBranches(t *testing.T) {
	ps := parseSchema(t, `{"anyOf":[
		{"type":"object","properties":{"id":{"type":"string"},"prompt":{"type":"string"}},"required":["id","prompt"]},
		{"$ref":"#/$defs/pause"},
		{"type":"null"}],
		"$defs":{"pause":{"type":"object","properties":{"id":{"type":"string"},"paused":{"type":"boolean"}},"required":["id"]}}}`)
	if !objectRoot(ps) {
		t.Fatal("应报告有改动")
	}
	props, _ := ps["properties"].(map[string]any)
	if ps["type"] != "object" || ps["anyOf"] != nil || ps["oneOf"] != nil || len(props) != 3 ||
		props["id"] == nil || props["prompt"] == nil || props["paused"] == nil || fmt.Sprint(ps["required"]) != "[id]" {
		t.Fatalf("合并结果不对: %v", ps)
	}
	if _, has := ps["$defs"]; !has {
		t.Errorf("$defs 应保留: %v", ps)
	}

	// 只有一个分支的 oneOf。
	one := parseSchema(t, `{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`)
	if !objectRoot(one) {
		t.Fatal("应报告有改动")
	}
	if one["type"] != "object" || one["oneOf"] != nil || one["properties"].(map[string]any)["a"] == nil {
		t.Fatalf("oneOf 单分支合并结果不对: %v", one)
	}
	if _, has := one["required"]; has {
		t.Errorf("没有任何必填字段时不应有 required: %v", one)
	}
}

// TestObjectRootMergesAllOf allOf 的分支全部合并（properties 与 required），根节点已有的字段不被覆盖，required 去重。
func TestObjectRootMergesAllOf(t *testing.T) {
	ps := parseSchema(t, `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],
		"allOf":[{"properties":{"b":{"type":"integer"},"a":{"type":"number"}},"required":["b"]},{"$ref":"#/$defs/c"}],
		"$defs":{"c":{"properties":{"c":{"type":"boolean"}},"required":["c","a"]}}}`)
	if !objectRoot(ps) {
		t.Fatal("应报告有改动")
	}
	props, _ := ps["properties"].(map[string]any)
	if _, has := ps["allOf"]; has || len(props) != 3 || !reflect.DeepEqual(props["a"], map[string]any{"type": "string"}) {
		t.Fatalf("allOf 合并结果不对: %v", ps)
	}
	if got := fmt.Sprint(ps["required"]); got != "[a b c]" {
		t.Fatalf("required = %s, want [a b c]", got)
	}
}

// TestObjectRootConflictingFields 分支给同一字段不同的 schema 时取 anyOf 并列；相同的合为一个；根节点已有的字段不被分支覆盖。
func TestObjectRootConflictingFields(t *testing.T) {
	ps := parseSchema(t, `{"anyOf":[
		{"type":"object","properties":{"k":{"type":"string","const":"a"},"x":{"type":"integer"}},"required":["k"]},
		{"type":"object","properties":{"k":{"type":"string","const":"b"},"x":{"type":"integer"}},"required":["k"]}]}`)
	if !objectRoot(ps) {
		t.Fatal("应报告有改动")
	}
	props, _ := ps["properties"].(map[string]any)
	wantK := map[string]any{"anyOf": []any{
		map[string]any{"type": "string", "const": "a"},
		map[string]any{"type": "string", "const": "b"},
	}}
	if !reflect.DeepEqual(props["k"], wantK) || !reflect.DeepEqual(props["x"], map[string]any{"type": "integer"}) || fmt.Sprint(ps["required"]) != "[k]" {
		t.Fatalf("冲突字段合并结果不对: %v", ps)
	}

	own := parseSchema(t, `{"type":"object","properties":{"k":{"type":"string"}},
		"oneOf":[{"type":"object","properties":{"k":{"type":"integer"},"y":{"type":"string"}}}]}`)
	if !objectRoot(own) {
		t.Fatal("应报告有改动")
	}
	ownProps, _ := own["properties"].(map[string]any)
	if !reflect.DeepEqual(ownProps["k"], map[string]any{"type": "string"}) || ownProps["y"] == nil || own["oneOf"] != nil {
		t.Fatalf("根节点自己的字段应保留: %v", own)
	}
}

// TestObjectRootRequiredNeedsEveryBranch 只有所有对象分支都要求的字段才必填；没有任何要求的分支会让 required 变空并删去该键，空的 required 同样被删去。
func TestObjectRootRequiredNeedsEveryBranch(t *testing.T) {
	ps := parseSchema(t, `{"anyOf":[
		{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},
		{"type":"object"}]}`)
	if !objectRoot(ps) {
		t.Fatal("应报告有改动")
	}
	if _, has := ps["required"]; has || ps["properties"].(map[string]any)["a"] == nil {
		t.Fatalf("结果不对: %v", ps)
	}

	empty := parseSchema(t, `{"required":[],"anyOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`)
	if !objectRoot(empty) {
		t.Fatal("应报告有改动")
	}
	if _, has := empty["required"]; has {
		t.Fatalf("空的 required 应删去: %v", empty)
	}
}

// TestObjectRootRefsAndBranchKinds $ref 可指向 $defs 或 definitions；解析不了的 $ref（外部、不存在）与非对象分支丢弃；
// 没有 type 但有 properties 的分支算对象分支。
func TestObjectRootRefsAndBranchKinds(t *testing.T) {
	ps := parseSchema(t, `{"anyOf":[
		{"$ref":"#/definitions/viaDefinitions"},
		{"$ref":"#/$defs/missing"},
		{"$ref":"https://example.com/schema.json"},
		{"type":"string"},
		{"type":"null"},
		{"properties":{"untyped":{"type":"string"}}}],
		"definitions":{"viaDefinitions":{"type":"object","properties":{"d":{"type":"string"}}}}}`)
	if !objectRoot(ps) {
		t.Fatal("应报告有改动")
	}
	props, _ := ps["properties"].(map[string]any)
	if len(props) != 2 || props["d"] == nil || props["untyped"] == nil || ps["type"] != "object" || ps["anyOf"] != nil {
		t.Fatalf("结果不对: %v", ps)
	}
}

// TestObjectRootLeavesObjectRoots 已是对象根节点且没有联合时不改动，返回 false；字段碰巧叫 anyOf/oneOf/allOf 也不是联合。
func TestObjectRootLeavesObjectRoots(t *testing.T) {
	for _, src := range []string{
		`{"type":"object"}`,
		`{"type":"object","properties":{}}`,
		`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`,
		`{"type":"object","properties":{"anyOf":{"type":"string"},"oneOf":{"type":"string"},"allOf":{"type":"string"}}}`,
		`{"type":"object","properties":{"p":{"anyOf":[{"type":"string"},{"type":"null"}]}},"$defs":{"a":{"oneOf":[{"type":"string"}]}}}`,
	} {
		ps := parseSchema(t, src)
		if objectRoot(ps) {
			t.Errorf("objectRoot(%s) 不应报告改动", src)
		}
		if !reflect.DeepEqual(ps, parseSchema(t, src)) {
			t.Errorf("objectRoot(%s) 不应修改 schema: %v", src, ps)
		}
	}
}

// TestObjectRootMissingType 根节点没有 type 时补上 type object（properties 保留）并报告改动。
func TestObjectRootMissingType(t *testing.T) {
	ps := parseSchema(t, `{"properties":{"a":{"type":"string"}},"required":["a"]}`)
	if !objectRoot(ps) {
		t.Fatal("应报告有改动")
	}
	if ps["type"] != "object" || ps["properties"].(map[string]any)["a"] == nil || fmt.Sprint(ps["required"]) != "[a]" {
		t.Fatalf("结果不对: %v", ps)
	}
}

// TestObjectRootCyclicRef $ref 循环引用的联合不会无限展开：深度有界，仍保留其中可读的字段。
func TestObjectRootCyclicRef(t *testing.T) {
	ps := parseSchema(t, `{"anyOf":[{"$ref":"#/$defs/a"}],
		"$defs":{"a":{"anyOf":[{"$ref":"#/$defs/a"},{"type":"object","properties":{"x":{"type":"string"}}}]}}}`)
	if !objectRoot(ps) {
		t.Fatal("应报告有改动")
	}
	props, _ := ps["properties"].(map[string]any)
	if ps["type"] != "object" || ps["anyOf"] != nil || props["x"] == nil {
		t.Fatalf("结果不对: %v", ps)
	}
}

// grokAutomationSchema 是 Codex desktop 的 automation_update 的参数：zod 的 discriminated union（mode），
// 其 create 与 update 分支各自又是 kind 上的联合（以 reused: "ref" 生成，各分支用 $defs 引用）。
// 内容与 magpie 的 internal/provider/testdata/codex_automation_update_schema.json 一致。
const grokAutomationSchema = `{
 "$schema": "https://json-schema.org/draft/2020-12/schema",
 "anyOf": [
  {"type": "object", "properties": {"mode": {"type": "string", "const": "view"}, "id": {"$ref": "#/$defs/__schema0"}}, "required": ["mode", "id"], "additionalProperties": false},
  {"anyOf": [
    {"type": "object", "properties": {
      "name": {"$ref": "#/$defs/__schema2"}, "prompt": {"$ref": "#/$defs/__schema3"}, "rrule": {"$ref": "#/$defs/__schema4"},
      "status": {"$ref": "#/$defs/__schema5"}, "notificationPolicy": {"$ref": "#/$defs/__schema6"}, "kind": {"$ref": "#/$defs/__schema7"},
      "projectId": {"$ref": "#/$defs/__schema8"}, "model": {"$ref": "#/$defs/__schema9"}, "reasoningEffort": {"$ref": "#/$defs/__schema10"},
      "mode": {"$ref": "#/$defs/__schema11"}, "destination": {"type": "string", "const": "local"}, "executionEnvironment": {"type": "string", "const": "local"}},
     "required": ["name", "prompt", "rrule", "status", "kind", "projectId", "model", "reasoningEffort", "mode", "executionEnvironment"], "additionalProperties": false},
    {"type": "object", "properties": {
      "name": {"$ref": "#/$defs/__schema2"}, "prompt": {"$ref": "#/$defs/__schema3"}, "rrule": {"$ref": "#/$defs/__schema4"},
      "status": {"$ref": "#/$defs/__schema5"}, "notificationPolicy": {"$ref": "#/$defs/__schema6"}, "kind": {"$ref": "#/$defs/__schema12"},
      "destination": {"$ref": "#/$defs/__schema13"}, "targetThreadId": {"$ref": "#/$defs/__schema14"}, "mode": {"$ref": "#/$defs/__schema11"}},
     "required": ["name", "prompt", "rrule", "status", "kind", "mode"], "additionalProperties": false}]},
  {"anyOf": [
    {"type": "object", "properties": {
      "name": {"$ref": "#/$defs/__schema2"}, "prompt": {"$ref": "#/$defs/__schema3"}, "rrule": {"$ref": "#/$defs/__schema15"},
      "status": {"$ref": "#/$defs/__schema5"}, "notificationPolicy": {"$ref": "#/$defs/__schema6"}, "kind": {"$ref": "#/$defs/__schema7"},
      "projectId": {"$ref": "#/$defs/__schema8"}, "model": {"$ref": "#/$defs/__schema9"}, "reasoningEffort": {"$ref": "#/$defs/__schema10"},
      "mode": {"$ref": "#/$defs/__schema16"}, "id": {"$ref": "#/$defs/__schema0"},
      "destination": {"type": "string", "enum": ["local", "worktree"]},
      "executionEnvironment": {"description": "env", "type": "string", "enum": ["worktree", "local"]},
      "localEnvironmentConfigPath": {"anyOf": [{"type": "string", "minLength": 1}, {"type": "null"}]}},
     "required": ["name", "prompt", "rrule", "status", "kind", "projectId", "model", "reasoningEffort", "mode", "id", "executionEnvironment"], "additionalProperties": false},
    {"type": "object", "properties": {
      "name": {"$ref": "#/$defs/__schema2"}, "prompt": {"$ref": "#/$defs/__schema3"}, "rrule": {"$ref": "#/$defs/__schema15"},
      "status": {"$ref": "#/$defs/__schema5"}, "notificationPolicy": {"$ref": "#/$defs/__schema6"}, "kind": {"$ref": "#/$defs/__schema12"},
      "destination": {"$ref": "#/$defs/__schema13"}, "targetThreadId": {"$ref": "#/$defs/__schema14"}, "mode": {"$ref": "#/$defs/__schema16"}, "id": {"$ref": "#/$defs/__schema0"}},
     "required": ["name", "prompt", "rrule", "status", "kind", "mode", "id"], "additionalProperties": false}]},
  {"type": "object", "properties": {"mode": {"type": "string", "const": "delete"}, "id": {"$ref": "#/$defs/__schema0"}}, "required": ["mode", "id"], "additionalProperties": false}
 ],
 "$defs": {
  "__schema0": {"description": "Automation id.", "$ref": "#/$defs/__schema1"},
  "__schema1": {"type": "string", "minLength": 1},
  "__schema2": {"description": "name", "$ref": "#/$defs/__schema1"},
  "__schema3": {"description": "prompt", "$ref": "#/$defs/__schema1"},
  "__schema4": {"description": "rrule create", "$ref": "#/$defs/__schema1"},
  "__schema5": {"description": "status", "type": "string", "enum": ["ACTIVE", "PAUSED"]},
  "__schema6": {"description": "np", "anyOf": [{"type": "string", "enum": ["all", "failed_runs_only"]}, {"type": "null"}]},
  "__schema7": {"description": "cron", "type": "string", "const": "cron"},
  "__schema8": {"description": "project", "anyOf": [{"type": "string", "minLength": 1}, {"type": "null"}]},
  "__schema9": {"description": "model", "$ref": "#/$defs/__schema1"},
  "__schema10": {"description": "effort", "type": "string", "enum": ["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"]},
  "__schema11": {"type": "string", "enum": ["create", "suggested_create"]},
  "__schema12": {"description": "hb", "type": "string", "const": "heartbeat"},
  "__schema13": {"description": "dest", "type": "string", "enum": ["local", "thread"]},
  "__schema14": {"format": "uuid", "description": "tt", "$ref": "#/$defs/__schema1"},
  "__schema15": {"description": "rrule", "$ref": "#/$defs/__schema1"},
  "__schema16": {"type": "string", "enum": ["update", "suggested_update"]}
 }
}`

// TestObjectRootNestedUnion Codex desktop 的 automation_update：分支本身又是联合时先折叠，字段保留，
// 各分支类型不同的字段（mode、kind）取其中任一种，只有所有分支都要求的 mode 仍必填。
func TestObjectRootNestedUnion(t *testing.T) {
	ps := parseSchema(t, grokAutomationSchema)
	if !objectRoot(ps) {
		t.Fatal("应报告有改动")
	}
	if ps["type"] != "object" || ps["anyOf"] != nil || ps["oneOf"] != nil {
		t.Fatalf("根节点不是普通 object: %v", ps)
	}
	props, _ := ps["properties"].(map[string]any)
	for _, key := range []string{"mode", "id", "name", "prompt", "rrule", "status", "kind", "projectId", "model", "targetThreadId", "executionEnvironment"} {
		if props[key] == nil {
			t.Errorf("properties 缺少 %s: %v", key, props)
		}
	}
	if got := fmt.Sprint(ps["required"]); got != "[mode]" {
		t.Errorf("required = %s, want [mode]", got)
	}
	// 每个分支能取的 mode 都还能发。
	mode, _ := json.Marshal(props["mode"])
	for _, want := range []string{`"view"`, `"delete"`, `__schema11`} {
		if !strings.Contains(string(mode), want) {
			t.Errorf("mode %s 缺少 %s", mode, want)
		}
	}
	kind, _ := json.Marshal(props["kind"])
	if !strings.Contains(string(kind), "__schema7") || !strings.Contains(string(kind), "__schema12") {
		t.Errorf("kind %s 应含两种取值", kind)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// customInput 与 customStream

// TestCustomInput 取 {"input":字符串} 的 input；否则 arguments 本身若是 JSON 字符串则取其值；否则原文。
func TestCustomInput(t *testing.T) {
	patch := "*** Begin Patch\n+\"q\" \\ é 😀 <a>&\t\n*** End Patch\n"
	marshalled, _ := json.Marshal(map[string]string{"input": patch})
	for _, tc := range []struct{ name, args, want string }{
		{"whole", string(marshalled), patch},
		{"escapes", `{"input":"😀 éA\/\n"}`, "😀 éA/\n"},
		{"other-keys", `{"path":"a.go","input":"x\"y"}`, `x"y`},
		{"spaced", `{ "input" : "spaced" }`, "spaced"},
		{"empty-input", `{"input":""}`, ""},
		{"lone-string", `"a lone string"`, "a lone string"},
		{"lone-string-escapes", `"a\nb"`, "a\nb"},
		{"input-not-string", `{"input":5}`, `{"input":5}`},
		{"input-null", `{"input":null}`, `{"input":null}`},
		{"no-input", `{"other":"x"}`, `{"other":"x"}`},
		{"not-json", `not json`, `not json`},
		{"truncated", `{"input":"abc`, `{"input":"abc`},
		{"empty", ``, ``},
		{"number", `42`, `42`},
		{"array", `["a"]`, `["a"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := customInput(tc.args); got != tc.want {
				t.Errorf("customInput(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// feedPieces 把 args 按 size 字节一段喂给新的 customStream，返回各段读出的文本。
func feedPieces(args string, size int) []string {
	var s customStream
	var out []string
	for i := 0; i < len(args); i += size {
		out = append(out, s.feed(args[i:min(i+size, len(args))]))
	}
	return out
}

// TestCustomInputStreams 逐段读出的 input 与整个 arguments 里的 input 一致，无论在哪里切段：转义中间、代理对两半之间、字符中间。
// （对应上游 TestCustomInputStreams）
func TestCustomInputStreams(t *testing.T) {
	patch := "*** Begin Patch\n+\"q\" \\ é 😀 <a>&\t\n*** End Patch\n"
	marshalled, _ := json.Marshal(map[string]string{"input": patch})
	for _, tc := range []struct{ args, want string }{
		{string(marshalled), patch},
		{`{"input":"😀 éA\/\n"}`, "😀 éA/\n"},
		{`{"path":"a.go","input":"x\"y"}`, `x"y`},
		{`{"meta":{"input":"no","list":["input"]},"input":"yes"}`, "yes"},
		{`{ "input" : "spaced" }`, "spaced"},
		{`"a lone string"`, "a lone string"},
		{`{"input":"lone \ud83d half"}`, "lone \ufffd half"},
		{`{"input":5}`, ""},
		{`{"input":null}`, ""},
		{`{"input":["a"]}`, ""},
		{`{"input":{"input":"nested"}}`, ""},
		{`{"other":"x"}`, ""},
		{`not json`, ""},
		{`{"in\u0070ut":"escaped key"}`, "escaped key"},
	} {
		for _, size := range []int{1, 2, 3, 5, 7, len(tc.args)} {
			if got := strings.Join(feedPieces(tc.args, size), ""); got != tc.want {
				t.Errorf("%s 按 %d 字节一段: %q, want %q", tc.args, size, got, tc.want)
			}
		}
		// 在任意一处切成两段，结果也一样。
		for cut := 0; cut <= len(tc.args); cut++ {
			var s customStream
			got := s.feed(tc.args[:cut]) + s.feed(tc.args[cut:])
			if got != tc.want {
				t.Errorf("%s 在 %d 处切成两段: %q, want %q", tc.args, cut, got, tc.want)
			}
		}
	}
}

// TestCustomStreamWaitsForWhatCannotBeReadYet 读不全的转义、代理对的一半、被切开的字符留给下一段；整段读完之后不再输出。
func TestCustomStreamWaitsForWhatCannotBeReadYet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pieces []string
		want   []string
	}{
		{"before-the-input", []string{`{"inp`, `ut":`, `"ab`, `c"}`}, []string{"", "", "ab", "c"}},
		{"escape-cut", []string{`{"input":"ab\`, `ncd"}`}, []string{"ab", "\ncd"}},
		{"unicode-escape-cut", []string{`{"input":"x\u00`, `e9y"}`}, []string{"x", "éy"}},
		{"surrogate-pair-cut-between-halves", []string{`{"input":"x\ud83d`, `\ude00y"}`}, []string{"x", "😀y"}},
		{"surrogate-low-half-cut", []string{`{"input":"\ud83d\ud`, `e00"}`}, []string{"", "😀"}},
		{"multibyte-cut", []string{"{\"input\":\"caf\xc3", "\xa9\"}"}, []string{"caf", "é"}},
		{"emoji-cut-in-three", []string{"{\"input\":\"a\xf0", "\x9f\x98", "\x80b\"}"}, []string{"a", "", "😀b"}},
		{"after-the-end", []string{`{"input":"ab"}`, `more`, `"x"`}, []string{"ab", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s customStream
			var got []string
			for _, piece := range tc.pieces {
				got = append(got, s.feed(piece))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("各段读出 %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCustomStreamZeroValueAndReuse customStream 的零值可直接使用，每个调用各用一个新的。
func TestCustomStreamZeroValueAndReuse(t *testing.T) {
	var first, second customStream
	if got := first.feed(`{"input":"a`); got != "a" {
		t.Fatalf("first = %q", got)
	}
	if got := second.feed(`{"input":"b`); got != "b" {
		t.Fatalf("second = %q（不应受 first 影响）", got)
	}
	if got := first.feed(`1"}`); got != "1" {
		t.Fatalf("first 续读 = %q", got)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// grokRequest：自定义工具

// TestGrokRequestCustomToolBecomesFunction 顶层 custom 工具改成 function：description 是 customDescription，parameters 是 customSchema，
// 原有的 format 等其他字段不再保留；记入 calls（Custom）。description 与 schema 的文本与上游完全一致，含 grammar 段落。
func TestGrokRequestCustomToolBecomesFunction(t *testing.T) {
	for _, tc := range []struct {
		name, tool, wantDescription string
	}{
		{"text-format", `{"type":"custom","name":"apply_patch","description":"Apply a patch.","format":{"type":"text"},"x":1}`,
			"Apply a patch.\n\n" + grokCustomNote},
		{"no-format", `{"type":"custom","name":"apply_patch","description":"Apply a patch."}`,
			"Apply a patch.\n\n" + grokCustomNote},
		{"no-description", `{"type":"custom","name":"apply_patch","format":{"type":"text"}}`,
			grokCustomNote},
		{"blank-description", `{"type":"custom","name":"apply_patch","description":"  \n\t "}`,
			grokCustomNote},
		{"description-trimmed", `{"type":"custom","name":"apply_patch","description":"\n  Apply a patch.  \n"}`,
			"Apply a patch.\n\n" + grokCustomNote},
		{"lark-grammar", `{"type":"custom","name":"apply_patch","description":"Apply a patch.","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/\n"}}`,
			"Apply a patch.\n\n" + grokCustomNote + "\n\nThe input follows this lark grammar:\nstart: /.+/"},
		{"regex-grammar", `{"type":"custom","name":"apply_patch","description":"Apply a patch.","format":{"type":"grammar","syntax":"regex","definition":"^a+$"}}`,
			"Apply a patch.\n\n" + grokCustomNote + "\n\nThe input follows this regex grammar:\n^a+$"},
		{"grammar-without-syntax", `{"type":"custom","name":"apply_patch","description":"Apply a patch.","format":{"type":"grammar","definition":"  abc  "}}`,
			"Apply a patch.\n\n" + grokCustomNote + "\n\nThe input follows this the grammar:\nabc"},
		{"grammar-without-description", `{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"x"}}`,
			grokCustomNote + "\n\nThe input follows this lark grammar:\nx"},
		{"grammar-keeps-inner-lines", `{"type":"custom","name":"apply_patch","description":"d","format":{"type":"grammar","syntax":"lark","definition":"\n a\n  b\n"}}`,
			"d\n\n" + grokCustomNote + "\n\nThe input follows this lark grammar:\na\n  b"},
		{"grammar-blank-definition", `{"type":"custom","name":"apply_patch","description":"d","format":{"type":"grammar","syntax":"lark","definition":"  \n"}}`,
			"d\n\n" + grokCustomNote},
		{"grammar-no-definition", `{"type":"custom","name":"apply_patch","description":"d","format":{"type":"grammar","syntax":"lark"}}`,
			"d\n\n" + grokCustomNote},
		{"text-format-ignores-definition", `{"type":"custom","name":"apply_patch","description":"d","format":{"type":"text","syntax":"lark","definition":"x"}}`,
			"d\n\n" + grokCustomNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, calls := runGrokRequest(t, `{"model":"grok-4.7","input":"hi","tools":[`+tc.tool+`]}`)
			assertSameJSON(t, out, `{"model":"grok-4.7","input":"hi","tools":[`+grokCustomFn("apply_patch", tc.wantDescription)+`]}`)
			assertGrokCalls(t, calls, grokCalls{"apply_patch": {Name: "apply_patch", Custom: true}})
		})
	}
}

// TestGrokRequestCustomToolKeepsPosition 自定义工具在 tools 中的位置不变，前后的工具保留。
func TestGrokRequestCustomToolKeepsPosition(t *testing.T) {
	out, calls := runGrokRequest(t, `{"model":"grok-4.7","tools":[`+
		`{"type":"function","name":"first","parameters":{"type":"object","properties":{}}},`+
		`{"type":"custom","name":"apply_patch","description":"d","format":{"type":"text"}},`+
		`{"type":"function","name":"last","parameters":{"type":"object","properties":{}}},`+
		`{"type":"custom","name":"exec","description":"e"}]}`)
	tools := grokToolList(t, out)
	if got := grokToolNames(tools); !reflect.DeepEqual(got, []string{"first", "apply_patch", "last", "exec"}) {
		t.Fatalf("工具顺序 = %v", got)
	}
	for _, i := range []int{1, 3} {
		if tools[i]["type"] != "function" {
			t.Errorf("tools[%d] 应是 function: %v", i, tools[i])
		}
	}
	assertGrokCalls(t, calls, grokCalls{
		"apply_patch": {Name: "apply_patch", Custom: true},
		"exec":        {Name: "exec", Custom: true},
	})
}

// ---------------------------------------------------------------------------------------------------------------------
// grokRequest：namespace

// TestGrokRequestFlattensNamespaces namespace 在原位置换成其内部的 function 与 custom（其他类型丢弃），内部工具名摊平为 ns__name，
// 并记入 calls；namespace 自己的 description 不再保留。
func TestGrokRequestFlattensNamespaces(t *testing.T) {
	out, calls := runGrokRequest(t, `{"model":"grok-4.7","tools":[`+
		`{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}},`+
		`{"type":"namespace","name":"codex_app","description":"Codex app.","tools":[`+
		`{"type":"function","name":"set_thread_title","description":"Set title.","strict":false,"parameters":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}},`+
		`{"type":"web_search"},`+
		`{"type":"function","name":"archive","description":"Archive.","parameters":{"type":"object","properties":{}}}]},`+
		`{"type":"web_search","external_web_access":false}]}`)
	assertSameJSON(t, out, `{"model":"grok-4.7","tools":[`+
		`{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}},`+
		`{"type":"function","name":"codex_app__set_thread_title","description":"Set title.","strict":false,"parameters":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}},`+
		`{"type":"function","name":"codex_app__archive","description":"Archive.","parameters":{"type":"object","properties":{}}},`+
		`{"type":"web_search","external_web_access":false}]}`)
	assertGrokCalls(t, calls, grokCalls{
		"codex_app__set_thread_title": {Namespace: "codex_app", Name: "set_thread_title"},
		"codex_app__archive":          {Namespace: "codex_app", Name: "archive"},
	})
	if bytes.Contains(out, []byte(`"namespace"`)) {
		t.Errorf("输出不应再有 namespace: %s", out)
	}
}

// TestGrokRequestCustomInNamespace namespace 里的 custom 同样转成 function（flat 名），记 Custom 与其 namespace、原名。
func TestGrokRequestCustomInNamespace(t *testing.T) {
	out, calls := runGrokRequest(t, `{"model":"grok-4.7","tools":[{"type":"namespace","name":"mcp__x","tools":[`+
		`{"type":"custom","name":"run","description":"Run.","format":{"type":"text"}},`+
		`{"type":"function","name":"f","parameters":{"type":"object"}}]}]}`)
	assertSameJSON(t, out, `{"model":"grok-4.7","tools":[`+
		grokCustomFn("mcp__x__run", "Run.\n\n"+grokCustomNote)+`,`+
		`{"type":"function","name":"mcp__x__f","parameters":{"type":"object"}}]}`)
	assertGrokCalls(t, calls, grokCalls{
		"mcp__x__run": {Namespace: "mcp__x", Name: "run", Custom: true},
		"mcp__x__f":   {Namespace: "mcp__x", Name: "f"},
	})
}

// TestGrokRequestFunctionsAndEmptyNamespace namespace 的名字为空或为 "functions"（Responses Lite 自己的 functions，Codex 视为没有 namespace）时，
// 内部工具保持原名；空的 namespace 整个消失。内部的 custom 仍转成 function 并记为 Custom；改名之外的 function 不记 calls 的 flat 名。
func TestGrokRequestFunctionsAndEmptyNamespace(t *testing.T) {
	out, calls := runGrokRequest(t, `{"model":"grok-4.7","tools":[`+
		`{"type":"namespace","name":"functions","tools":[`+
		`{"type":"function","name":"view_image","parameters":{"type":"object","properties":{}}},`+
		`{"type":"custom","name":"exec","description":"Run.","format":{"type":"text"}}]},`+
		`{"type":"namespace","name":"","tools":[{"type":"function","name":"bare","parameters":{"type":"object"}}]},`+
		`{"type":"namespace","name":"empty","tools":[]},`+
		`{"type":"function","name":"after","parameters":{"type":"object"}}]}`)
	assertSameJSON(t, out, `{"model":"grok-4.7","tools":[`+
		`{"type":"function","name":"view_image","parameters":{"type":"object","properties":{}}},`+
		grokCustomFn("exec", "Run.\n\n"+grokCustomNote)+`,`+
		`{"type":"function","name":"bare","parameters":{"type":"object"}},`+
		`{"type":"function","name":"after","parameters":{"type":"object"}}]}`)
	if call, ok := calls["exec"]; !ok || !call.Custom || call.Name != "exec" {
		t.Errorf("functions 里的 custom 应记为 Custom，calls = %+v", calls)
	}
	for key := range calls {
		if strings.Contains(key, "__") {
			t.Errorf("functions 与空 namespace 的工具不应以 flat 名记入 calls: %q", key)
		}
	}
}

// TestGrokRequestLongNamespacedNames 超长的 ns__name 用 flatName 的截断加哈希形式，calls 的键与 tools 里的名字一致。
func TestGrokRequestLongNamespacedNames(t *testing.T) {
	ns, name := "mcp__codex_app", "a_very_long_tool_name_that_goes_on_and_on_and_on_and_on_and_on_forever"
	flat := flatName(ns, name)
	if len(flat) != 64 {
		t.Fatalf("测试前提：flatName 长度 = %d", len(flat))
	}
	out, calls := runGrokRequest(t, `{"model":"grok-4.7","tools":[{"type":"namespace","name":"`+ns+`","tools":[`+
		`{"type":"function","name":"`+name+`","parameters":{"type":"object"}}]}],`+
		`"tool_choice":{"type":"function","name":"`+name+`","namespace":"`+ns+`"},`+
		`"input":[{"type":"function_call","call_id":"c1","name":"`+name+`","namespace":"`+ns+`","arguments":"{}"}]}`)
	if names := grokToolNames(grokToolList(t, out)); !reflect.DeepEqual(names, []string{flat}) {
		t.Fatalf("工具名 = %v, want [%s]", names, flat)
	}
	body := decodeNumberMap(t, out)
	if choice, _ := body["tool_choice"].(map[string]any); choice["name"] != flat || choice["namespace"] != nil {
		t.Errorf("tool_choice = %v", choice)
	}
	if items := itemIDInput(t, out); items[0]["name"] != flat || items[0]["namespace"] != nil {
		t.Errorf("历史 function_call = %v", items[0])
	}
	assertGrokCalls(t, calls, grokCalls{flat: {Namespace: ns, Name: name}})
}

// ---------------------------------------------------------------------------------------------------------------------
// grokRequest：其他工具类型与根联合

// TestGrokRequestLeavesOtherToolTypes web_search、tool_search、mcp 等类型保持原文（含键顺序、数字写法），不记入 calls。
func TestGrokRequestLeavesOtherToolTypes(t *testing.T) {
	others := []string{
		`{"external_web_access":false,"type":"web_search"}`,
		`{"type":"tool_search","execution":"client","description":"search","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}`,
		`{"type":"image_generation","output_format":"png","n":1.0}`,
		`{"type":"mcp","server_label":"x","server_url":"https://e.example/mcp?a=1&b=<2>"}`,
		`{"type":"local_shell"}`,
	}
	body := `{"model":"grok-4.7","tools":[` + others[0] + `,{"type":"custom","name":"c","description":"d"},` +
		strings.Join(others[1:], ",") + `]}`
	out, calls := runGrokRequest(t, body)
	raw := grokRawTools(t, out)
	if len(raw) != 6 {
		t.Fatalf("tools 数 = %d, want 6: %s", len(raw), out)
	}
	if raw[0] != others[0] || !reflect.DeepEqual(raw[2:], others[1:]) {
		t.Fatalf("其他类型的工具应保持原文:\n got=%q\nwant=%q", raw, others)
	}
	assertGrokCalls(t, calls, grokCalls{"c": {Name: "c", Custom: true}})
}

// TestGrokRequestObjectRoot 参数根节点为联合的 function（顶层的与从 namespace 摊平出来的）合并为 object；已是 object 的保持原文。
// （对应上游 TestGrokBodyObjectRoot）
func TestGrokRequestObjectRoot(t *testing.T) {
	const plain = `{"type":"function","name":"plain","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}`
	out, calls := runGrokRequest(t, `{"tools":[
		{"type":"function","name":"mcp__codex_app__automation_update","parameters":{"anyOf":[
			{"type":"object","properties":{"id":{"type":"string"},"prompt":{"type":"string"}},"required":["id","prompt"]},
			{"$ref":"#/$defs/pause"},
			{"type":"null"}],"$defs":{"pause":{"type":"object","properties":{"id":{"type":"string"},"paused":{"type":"boolean"}},"required":["id"]}}}},
		{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"pick","parameters":{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}}]},
		`+plain+`]}`)
	tools := grokToolList(t, out)
	if len(tools) != 3 {
		t.Fatalf("tools = %v", tools)
	}
	for _, tool := range tools {
		ps, _ := tool["parameters"].(map[string]any)
		if ps["type"] != "object" || ps["anyOf"] != nil || ps["oneOf"] != nil || ps["allOf"] != nil {
			t.Fatalf("%v: parameters 根节点不是普通 object: %v", tool["name"], ps)
		}
	}
	first, _ := tools[0]["parameters"].(map[string]any)
	props, _ := first["properties"].(map[string]any)
	if len(props) != 3 || props["paused"] == nil || props["prompt"] == nil || fmt.Sprint(first["required"]) != "[id]" {
		t.Fatalf("合并结果不对: %v", first)
	}
	if tools[1]["name"] != "codex_app__pick" || tools[1]["parameters"].(map[string]any)["properties"].(map[string]any)["a"] == nil {
		t.Fatalf("namespace 里的联合没有合并: %v", tools[1])
	}
	if raw := grokRawTools(t, out); raw[2] != plain {
		t.Fatalf("已是 object 的工具应保持原文: %s", raw[2])
	}
	assertGrokCalls(t, calls, grokCalls{"codex_app__pick": {Namespace: "codex_app", Name: "pick"}})
}

// TestGrokRequestObjectRootMatchesObjectRoot grokRequest 对 parameters 的合并结果与对同一 schema 调用 objectRoot 的结果一致。
func TestGrokRequestObjectRootMatchesObjectRoot(t *testing.T) {
	out, calls := runGrokRequest(t, `{"model":"grok-4.7","tools":[{"type":"function","name":"automation_update","parameters":`+grokAutomationSchema+`}]}`)
	dec := json.NewDecoder(strings.NewReader(grokAutomationSchema))
	dec.UseNumber()
	var want map[string]any
	if err := dec.Decode(&want); err != nil {
		t.Fatal(err)
	}
	if !objectRoot(want) {
		t.Fatal("objectRoot 应报告有改动")
	}
	got, _ := grokToolList(t, out)[0]["parameters"].(map[string]any)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parameters 与 objectRoot 的结果不一致:\n got=%v\nwant=%v", got, want)
	}
	// 只合并了参数，名字没变，不记 calls。
	assertGrokCalls(t, calls, nil)
}

// TestGrokRequestLeavesNonObjectParameters parameters 不是对象（缺失、null、字符串）的 function 不动。
func TestGrokRequestLeavesNonObjectParameters(t *testing.T) {
	tools := []string{
		`{"type":"function","name":"a"}`,
		`{"type":"function","name":"b","parameters":null}`,
		`{"type":"function","name":"c","parameters":"{\"anyOf\":[]}"}`,
	}
	body := `{"model":"grok-4.7","tools":[` + strings.Join(tools, ",") + `,{"type":"custom","name":"x"}]}`
	out, _ := runGrokRequest(t, body)
	if raw := grokRawTools(t, out); !reflect.DeepEqual(raw[:3], tools) {
		t.Fatalf("parameters 不是对象的 function 应保持原文:\n got=%q\nwant=%q", raw[:3], tools)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// grokRequest：tool_choice

// TestGrokRequestToolChoice tool_choice 是 custom 或 function 对象时改为 function，有 namespace 时名字摊平并删去 namespace；
// allowed_tools 对其 tools 的每一项做同样处理；字符串形式与其他类型不动。
func TestGrokRequestToolChoice(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"custom", `{"type":"custom","name":"apply_patch"}`, `{"type":"function","name":"apply_patch"}`},
		{"namespaced-function", `{"type":"function","name":"set_thread_title","namespace":"codex_app"}`, `{"type":"function","name":"codex_app__set_thread_title"}`},
		{"namespaced-custom", `{"type":"custom","name":"x","namespace":"ns"}`, `{"type":"function","name":"ns__x"}`},
		{"functions-namespace", `{"type":"function","name":"view_image","namespace":"functions"}`, `{"type":"function","name":"view_image"}`},
		{"allowed-tools",
			`{"type":"allowed_tools","mode":"auto","tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"f","namespace":"ns"},{"type":"function","name":"g"},{"type":"web_search"}]}`,
			`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"apply_patch"},{"type":"function","name":"ns__f"},{"type":"function","name":"g"},{"type":"web_search"}]}`},
		{"allowed-tools-required",
			`{"type":"allowed_tools","mode":"required","tools":[{"type":"custom","name":"x","namespace":"functions"}]}`,
			`{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"x"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := runGrokRequest(t, `{"model":"grok-4.7","tools":[{"type":"function","name":"g"}],"tool_choice":`+tc.in+`}`)
			assertSameJSON(t, out, `{"model":"grok-4.7","tools":[{"type":"function","name":"g"}],"tool_choice":`+tc.want+`}`)
		})
	}

	// 字符串形式与不相关的对象保持原样，整个请求体原样返回。
	for _, choice := range []string{`"auto"`, `"required"`, `"none"`, `{"type":"function","name":"g"}`, `{"type":"web_search"}`,
		`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"g"},{"type":"web_search"}]}`} {
		body := []byte(`{"model":"grok-4.7","tools":[{"type":"function","name":"g"}],"tool_choice":` + choice + `}`)
		out, calls := grokRequest(body)
		assertSameSlice(t, out, body)
		assertGrokCalls(t, calls, nil)
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// grokRequest：历史 input 项

// grokHistoryPatch 含 HTML 敏感字符、引号、反斜杠、多字节字符与换行的补丁文本。
const grokHistoryPatch = "*** Begin Patch\n<a> & \"b\" \\ é 😀\n*** End Patch"

// TestGrokRequestHistory 历史里的 custom_tool_call 变成 function_call（arguments 是只含 input 字符串的 JSON，删去 input、namespace、id），
// custom_tool_call_output 变成 function_call_output（删去 id），带 namespace 的 function_call 名字摊平并删去 namespace；其余字段保留。
// 这些调用都记入 calls，即使本次请求没有声明对应的工具。
func TestGrokRequestHistory(t *testing.T) {
	out, calls := runGrokRequest(t, `{"model":"grok-4.7","input":[`+
		`{"role":"user","content":"hi"},`+
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":`+itemIDQuote(grokHistoryPatch)+`,"status":"completed"},`+
		`{"type":"custom_tool_call_output","id":"cto_1","call_id":"call_1","output":"Done!"},`+
		`{"type":"custom_tool_call","call_id":"call_2","name":"run","namespace":"mcp__x","input":"ls"},`+
		`{"type":"custom_tool_call_output","call_id":"call_2","output":[{"type":"input_text","text":"a.go"}]},`+
		`{"type":"function_call","id":"fc_1","call_id":"call_3","name":"set_thread_title","namespace":"codex_app","arguments":"{\"title\":\"t\"}","status":"completed"},`+
		`{"type":"function_call_output","call_id":"call_3","output":"ok"},`+
		`{"type":"function_call","call_id":"call_5","name":"shell","arguments":"{}"}]}`)
	items := itemIDInput(t, out)
	if len(items) != 8 {
		t.Fatalf("input 项数 = %d, want 8: %s", len(items), out)
	}
	assertGrokItem(t, items[0], `{"role":"user","content":"hi"}`)
	if input := grokPopInput(t, items[1]); input != grokHistoryPatch {
		t.Errorf("custom_tool_call 的 input = %q, want %q", input, grokHistoryPatch)
	}
	assertGrokItem(t, items[1], `{"type":"function_call","call_id":"call_1","name":"apply_patch","status":"completed"}`)
	assertGrokItem(t, items[2], `{"type":"function_call_output","call_id":"call_1","output":"Done!"}`)
	if input := grokPopInput(t, items[3]); input != "ls" {
		t.Errorf("custom_tool_call 的 input = %q, want ls", input)
	}
	assertGrokItem(t, items[3], `{"type":"function_call","call_id":"call_2","name":"mcp__x__run"}`)
	assertGrokItem(t, items[4], `{"type":"function_call_output","call_id":"call_2","output":[{"type":"input_text","text":"a.go"}]}`)
	assertGrokItem(t, items[5], `{"type":"function_call","id":"fc_1","call_id":"call_3","name":"codex_app__set_thread_title","arguments":"{\"title\":\"t\"}","status":"completed"}`)
	assertGrokItem(t, items[6], `{"type":"function_call_output","call_id":"call_3","output":"ok"}`)
	assertGrokItem(t, items[7], `{"type":"function_call","call_id":"call_5","name":"shell","arguments":"{}"}`)
	assertGrokCalls(t, calls, grokCalls{
		"apply_patch":                 {Name: "apply_patch", Custom: true},
		"mcp__x__run":                 {Namespace: "mcp__x", Name: "run", Custom: true},
		"codex_app__set_thread_title": {Namespace: "codex_app", Name: "set_thread_title"},
	})
	// 转回的 arguments 与 customInput 互为逆运算。
	var raw struct {
		Input []struct {
			Arguments string `json:"arguments"`
			Name      string `json:"name"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if got := customInput(raw.Input[1].Arguments); got != grokHistoryPatch {
		t.Errorf("customInput(arguments) = %q, want %q", got, grokHistoryPatch)
	}
}

// TestGrokRequestHistoryFunctionsNamespace 历史里 namespace 为 "functions" 的 function_call 删去 namespace、名字不变。
func TestGrokRequestHistoryFunctionsNamespace(t *testing.T) {
	out, _ := runGrokRequest(t, `{"model":"grok-4.7","input":[`+
		`{"type":"function_call","call_id":"c1","name":"view_image","namespace":"functions","arguments":"{}"},`+
		`{"type":"custom_tool_call","call_id":"c2","name":"exec","namespace":"functions","input":"ls"}]}`)
	items := itemIDInput(t, out)
	assertGrokItem(t, items[0], `{"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"}`)
	if input := grokPopInput(t, items[1]); input != "ls" {
		t.Errorf("input = %q", input)
	}
	assertGrokItem(t, items[1], `{"type":"function_call","call_id":"c2","name":"exec"}`)
}

// TestGrokRequestHistoryWithoutDeclaredTools 请求里没有 tools 时，历史里的调用同样转换并记入 calls，回复才能还原。
func TestGrokRequestHistoryWithoutDeclaredTools(t *testing.T) {
	out, calls := runGrokRequest(t, `{"model":"grok-4.7","input":[`+
		`{"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"x"},`+
		`{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}]}`)
	items := itemIDInput(t, out)
	if items[0]["type"] != "function_call" || items[1]["type"] != "function_call_output" {
		t.Fatalf("历史项没有转换: %s", out)
	}
	assertGrokCalls(t, calls, grokCalls{"apply_patch": {Name: "apply_patch", Custom: true}})
}

// TestGrokRequestOnlyConvertsCallItems 只有 custom_tool_call、custom_tool_call_output 与 function_call 三类项被改写：
// 其他类型的项即使带 namespace 或 input 字段也保持原文；input 为字符串时不动。
func TestGrokRequestOnlyConvertsCallItems(t *testing.T) {
	items := []string{
		`{"type":"message","role":"user","namespace":"x","content":"hi"}`,
		`{"type":"reasoning","id":"rs_1","namespace":"x","summary":[]}`,
		`{"type":"tool_search_call","call_id":"c","namespace":"x","arguments":{}}`,
		`"plain string item"`,
		`{"type":"function_call_output","id":"fco_1","call_id":"c","output":"ok"}`,
	}
	body := []byte(`{"model":"grok-4.7","input":[` + strings.Join(items, ",") + `]}`)
	out, calls := grokRequest(body)
	assertSameSlice(t, out, body)
	assertGrokCalls(t, calls, nil)

	text := []byte(`{"model":"grok-4.7","input":"custom_tool_call namespace"}`)
	out, calls = grokRequest(text)
	assertSameSlice(t, out, text)
	assertGrokCalls(t, calls, nil)
}

// ---------------------------------------------------------------------------------------------------------------------
// grokRequest：无改动、非 JSON 与重编码

// TestGrokRequestUnchanged 没有需要改写的内容时返回传入的原切片（逐字节一致），calls 为空，不修改输入。
func TestGrokRequestUnchanged(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no-tools", `{"model":"grok-4.7","input":"hi"}`},
		{"empty-tools", `{"model":"grok-4.7","input":[],"tools":[]}`},
		{"plain-tools", `{"model":"grok-4.7","input":[{"role":"user","content":"hi"},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}],` +
			`"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"p":{"anyOf":[{"type":"string"},{"type":"null"}]}}}},{"type":"web_search","external_web_access":false}],"tool_choice":"auto"}`},
		{"fields-named-like-unions", `{"model":"grok-4.7","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"anyOf":{"type":"string"},"oneOf":{"type":"string"},"allOf":{"type":"string"}}}}]}`},
		{"tools-nested-elsewhere", `{"model":"grok-4.7","metadata":{"tools":[{"type":"custom","name":"x"},{"type":"namespace","name":"n","tools":[]}]},"tools":[]}`},
		{"mentions-in-text", `{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"{\"type\":\"custom\"} namespace custom_tool_call oneOf"}]}`},
		{"tools-not-array", `{"model":"grok-4.7","tools":null,"input":"hi"}`},
		{"tools-is-object", `{"model":"grok-4.7","tools":{"type":"custom","name":"x"},"input":"hi"}`},
		{"no-model", `{"input":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			out, calls := grokRequest(body)
			assertSameSlice(t, out, body)
			assertGrokCalls(t, calls, nil)
		})
	}
}

// TestGrokRequestNotJSON 请求体不是 JSON 对象时原样返回（原切片），calls 为空。
func TestGrokRequestNotJSON(t *testing.T) {
	for _, body := range []string{``, `not json`, `{"tools":[{"type":"custom","name":"x"}]`, `[]`, `"x"`, `null`, `42`, `[{"tools":[{"type":"custom","name":"x"}]}]`} {
		in := []byte(body)
		out, calls := grokRequest(in)
		if !bytes.Equal(out, in) {
			t.Errorf("grokRequest(%q) = %q, 应原样返回", body, out)
		}
		if len(in) > 0 && len(out) > 0 && &out[0] != &in[0] {
			t.Errorf("grokRequest(%q) 应返回传入的原切片", body)
		}
		assertGrokCalls(t, calls, nil)
	}
}

// TestGrokRequestKeepsLiterals 有改动时：未改动的 tools 元素与 input 项保留原文（键顺序、数字写法），
// 改动过的部分与顶层其他字段数字原文保留、不转义 < > &。
func TestGrokRequestKeepsLiterals(t *testing.T) {
	const untouchedTool = `{"z":1,"type":"function","name":"keep","description":"<b> & c","parameters":{"type":"object","properties":{"n":{"type":"number","default":1.0}}}}`
	const untouchedItem = `{"z":1,"type":"function_call_output","call_id":"c1","output":"2.50 <b> & c","n":1e3}`
	out, _ := runGrokRequest(t, `{"model":"grok-4.7","temperature":1.0,"seed":12345678901234567890,"instructions":"be <nice> & kind","input":[`+
		untouchedItem+`,`+
		`{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"a<b>&c","n":3.10,"big":98765432109876543210}],"tools":[`+
		untouchedTool+`,`+
		`{"type":"custom","name":"apply_patch","description":"Patch <files> & more.","format":{"type":"text"}},`+
		`{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"n":{"type":"number","default":2.50,"maximum":12345678901234567890}}}}]}]}`)
	// 前提：确实发生了改写。
	if names := grokToolNames(grokToolList(t, out)); !reflect.DeepEqual(names, []string{"keep", "apply_patch", "ns__f"}) {
		t.Fatalf("工具名 = %v, want [keep apply_patch ns__f]", names)
	}
	if items := itemIDInput(t, out); items[1]["type"] != "function_call" || items[1]["name"] != "apply_patch" {
		t.Fatalf("历史里的自定义调用没有转换: %v", items[1])
	}
	if raw := grokRawTools(t, out); raw[0] != untouchedTool {
		t.Fatalf("未改动的工具应保留原文:\n got=%s\nwant=%s", raw[0], untouchedTool)
	}
	for _, literal := range []string{untouchedItem, `"temperature":1.0`, `12345678901234567890`, `be <nice> & kind`, `"default":2.50`, `Patch <files> & more.`, `3.10`, `98765432109876543210`} {
		if !bytes.Contains(out, []byte(literal)) {
			t.Fatalf("输出缺少原样片段 %q（数字原文保留，不转义 HTML）:\n%s", literal, out)
		}
	}
	for _, escaped := range []string{"\\u00" + "3c", "\\u00" + "3e", "\\u00" + "26"} {
		if bytes.Contains(out, []byte(escaped)) {
			t.Fatalf("输出含 HTML 转义 %q: %s", escaped, out)
		}
	}
}

// TestGrokRequestOddShapes tools 中的非对象元素原样保留；顶层其他字段语义不变。
func TestGrokRequestOddShapes(t *testing.T) {
	out, calls := runGrokRequest(t, `{"model":"grok-4.7","store":false,"metadata":{"tools":[{"type":"custom","name":"nested"}]},"tools":["x",null,1,{"type":"custom","name":"c"}]}`)
	assertSameJSON(t, out, `{"model":"grok-4.7","store":false,"metadata":{"tools":[{"type":"custom","name":"nested"}]},"tools":["x",null,1,`+
		grokCustomFn("c", grokCustomNote)+`]}`)
	assertGrokCalls(t, calls, grokCalls{"c": {Name: "c", Custom: true}})
}

// TestGrokRequestLeadingWhitespace 请求体以空白开头、结尾带换行（JSON 允许）时同样改写。
func TestGrokRequestLeadingWhitespace(t *testing.T) {
	out, calls := runGrokRequest(t, "\n  "+`{"model":"grok-4.7","tools":[{"type":"custom","name":"c","description":"d"}]}`+"\n")
	assertSameJSON(t, out, `{"model":"grok-4.7","tools":[`+grokCustomFn("c", "d\n\n"+grokCustomNote)+`]}`)
	assertGrokCalls(t, calls, grokCalls{"c": {Name: "c", Custom: true}})
}

// grokCodexRequest 是 Codex 在 Grok 上的一个完整轮次：自定义工具 apply_patch（带 grammar）、普通 function、namespace、
// 参数根节点是联合的 function、web_search，历史里有 apply_patch 的调用与输出、namespace 里 function 的调用与输出。
const grokCodexRequest = `{"model":"grok-4.7","stream":true,"store":false,"temperature":1.0,"tool_choice":"auto","parallel_tool_calls":true,"input":[` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"patch <a.go> & test"}]},` +
	`{"type":"custom_tool_call","id":"ctc_old","call_id":"call_p1","name":"apply_patch","input":"*** Begin Patch\n*** End Patch\n","status":"completed"},` +
	`{"type":"custom_tool_call_output","call_id":"call_p1","output":"Done!"},` +
	`{"type":"function_call","id":"fc_old","call_id":"call_t1","name":"set_thread_title","namespace":"codex_app","arguments":"{\"title\":\"a\"}","status":"completed"},` +
	`{"type":"function_call_output","call_id":"call_t1","output":"ok"},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}],"tools":[` +
	`{"type":"custom","name":"apply_patch","description":"Apply a patch.","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},` +
	`{"type":"function","name":"shell","description":"Run.","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},` +
	`{"type":"namespace","name":"codex_app","description":"Codex app.","tools":[{"type":"function","name":"set_thread_title","description":"Title.","parameters":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}]},` +
	`{"type":"function","name":"automation_update","description":"Update.","parameters":{"oneOf":[` +
	`{"type":"object","properties":{"mode":{"type":"string","enum":["a"]},"id":{"type":"string"}},"required":["mode","id"]},` +
	`{"type":"object","properties":{"mode":{"type":"string","enum":["b"]}},"required":["mode"]}]}},` +
	`{"type":"web_search","external_web_access":false}]}`

// grokCodexRewritten 是 grokCodexRequest 经 grokRequest 改写后的样子。
const grokCodexRewritten = `{"model":"grok-4.7","stream":true,"store":false,"temperature":1.0,"tool_choice":"auto","parallel_tool_calls":true,"input":[` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"patch <a.go> & test"}]},` +
	`{"type":"function_call","call_id":"call_p1","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\\n\"}","status":"completed"},` +
	`{"type":"function_call_output","call_id":"call_p1","output":"Done!"},` +
	`{"type":"function_call","id":"fc_old","call_id":"call_t1","name":"codex_app__set_thread_title","arguments":"{\"title\":\"a\"}","status":"completed"},` +
	`{"type":"function_call_output","call_id":"call_t1","output":"ok"},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}],"tools":[` +
	`{"type":"function","name":"apply_patch","description":"Apply a patch.\n\n` + `Put the tool's whole free-form input, as it is to be run, in the ` + "`input`" + ` string; it is passed on as written, so it is not itself JSON.\n\nThe input follows this lark grammar:\nstart: /.+/","parameters":` + grokCustomSchema + `},` +
	`{"type":"function","name":"shell","description":"Run.","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},` +
	`{"type":"function","name":"codex_app__set_thread_title","description":"Title.","parameters":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}},` +
	`{"type":"function","name":"automation_update","description":"Update.","parameters":{"type":"object","properties":{` +
	`"mode":{"anyOf":[{"type":"string","enum":["a"]},{"type":"string","enum":["b"]}]},"id":{"type":"string"}},"required":["mode"]}},` +
	`{"type":"web_search","external_web_access":false}]}`

// TestGrokRequestCodexTurn 一个完整的 Codex 轮次：所有规则一起生效，其余字段与未涉及的工具、项保持不变。
func TestGrokRequestCodexTurn(t *testing.T) {
	out, calls := runGrokRequest(t, grokCodexRequest)
	assertSameJSON(t, out, grokCodexRewritten)
	assertGrokCalls(t, calls, grokCalls{
		"apply_patch":                 {Name: "apply_patch", Custom: true},
		"codex_app__set_thread_title": {Namespace: "codex_app", Name: "set_thread_title"},
	})
	for _, banned := range []string{`"type":"custom"`, `"type":"namespace"`, `"oneOf"`, `custom_tool_call`, `"namespace"`} {
		if bytes.Contains(out, []byte(banned)) {
			t.Errorf("输出不应再含 %s: %s", banned, out)
		}
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// R2：只含 allOf 根的参数、循环 $ref 的联合不指数展开

// TestGrokRequestMergesAllOfRoot 只含 allOf 根的 function 参数也被合并为 object：请求体里没有 anyOf、oneOf、custom、namespace 字样，
// 只有 "allOf" 能触发改写（预筛选漏掉 allOf 就会原样转发，Grok 后端因根节点不是 object 回 422）。
func TestGrokRequestMergesAllOfRoot(t *testing.T) {
	const branches = `[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"integer"}}}]`
	for _, tc := range []struct {
		name, parameters string
		wantProps        []string
	}{
		{"no-type", `{"allOf":` + branches + `}`, []string{"a", "b"}},
		{"type-object", `{"type":"object","properties":{"c":{"type":"boolean"}},"allOf":` + branches + `}`, []string{"a", "b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"grok-4.7","tools":[{"type":"function","name":"merge","description":"Merge.","parameters":` + tc.parameters + `}]}`
			for _, word := range []string{`"anyOf"`, `"oneOf"`, `"custom"`, `"namespace"`, `custom_tool_call`} {
				if strings.Contains(body, word) {
					t.Fatalf("测试前提：请求体不应含 %s", word)
				}
			}
			out, calls := runGrokRequest(t, body)
			if bytes.Equal(out, []byte(body)) {
				t.Fatalf("只含 allOf 根的参数应被合并: %s", out)
			}
			ps, _ := grokToolList(t, out)[0]["parameters"].(map[string]any)
			props, _ := ps["properties"].(map[string]any)
			if ps["type"] != "object" || ps["allOf"] != nil || len(props) != len(tc.wantProps) || fmt.Sprint(ps["required"]) != "[a]" {
				t.Fatalf("合并结果不对: %v", ps)
			}
			for _, key := range tc.wantProps {
				if props[key] == nil {
					t.Errorf("properties 缺少 %s: %v", key, props)
				}
			}
			assertGrokCalls(t, calls, nil)
		})
	}
}

// grokCycleRefs 返回 n 个指向 $defs 里 name 的 $ref 分支，逗号分隔。
func grokCycleRefs(name string, n int) string {
	return strings.TrimSuffix(strings.Repeat(`{"$ref":"#/$defs/`+name+`"},`, n), ",")
}

// withinTime 在 limit 内执行 fn；超时则让测试失败，不等 fn（病态 schema 若被指数展开，fn 要算上几个小时）。
func withinTime(t *testing.T, limit time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s 在 %v 内没有完成（循环 $ref 的联合被指数展开）", what, limit)
	}
}

// grokCyclicSchemas 是病态的循环 $ref schema：$defs 里的联合有 16 个分支指回自己（或互相指），
// 逐层展开 16^8 次，上游的深度上限挡不住。已在展开栈上的引用不再展开，因此结果里只留下可读的对象分支。
func grokCyclicSchemas() []struct {
	name, schema string
	wantProps    []string
} {
	object := func(field string) string {
		return `{"type":"object","properties":{"` + field + `":{"type":"string"}},"required":["` + field + `"]}`
	}
	return []struct {
		name, schema string
		wantProps    []string
	}{
		{"self-any-of", `{"anyOf":[{"$ref":"#/$defs/a"}],"$defs":{"a":{"anyOf":[` + grokCycleRefs("a", 16) + `,` + object("x") + `]}}}`, []string{"x"}},
		{"self-one-of", `{"oneOf":[{"$ref":"#/$defs/a"}],"$defs":{"a":{"oneOf":[` + grokCycleRefs("a", 16) + `,` + object("x") + `]}}}`, []string{"x"}},
		{"self-definitions", `{"anyOf":[{"$ref":"#/definitions/a"}],"definitions":{"a":{"anyOf":[` +
			strings.ReplaceAll(grokCycleRefs("a", 16), "#/$defs/", "#/definitions/") + `,` + object("x") + `]}}}`, []string{"x"}},
		{"mutual", `{"anyOf":[{"$ref":"#/$defs/a"}],"$defs":{` +
			`"a":{"anyOf":[` + grokCycleRefs("b", 16) + `,` + object("x") + `]},` +
			`"b":{"anyOf":[` + grokCycleRefs("a", 16) + `,` + object("y") + `]}}}`, []string{"x", "y"}},
		{"root-is-the-cycle", `{"anyOf":[` + grokCycleRefs("a", 16) + `,` + object("z") + `],"$defs":{"a":{"anyOf":[` + grokCycleRefs("a", 16) + `,` + object("x") + `]}}}`, []string{"x", "z"}},
	}
}

// TestObjectRootCyclicRefsDoNotExplode 病态的循环 $ref schema 下 objectRoot 在 1 秒内完成（应是毫秒量级），并保留可读的字段。
// 无循环引用的 schema 结果与上游一致，由上面的 NestedUnion、MergesBranches 等用例覆盖。
func TestObjectRootCyclicRefsDoNotExplode(t *testing.T) {
	for _, tc := range grokCyclicSchemas() {
		t.Run(tc.name, func(t *testing.T) {
			ps := parseSchema(t, tc.schema)
			var changed bool
			withinTime(t, time.Second, "objectRoot", func() { changed = objectRoot(ps) })
			if !changed {
				t.Fatal("应报告有改动")
			}
			props, _ := ps["properties"].(map[string]any)
			if ps["type"] != "object" || ps["anyOf"] != nil || ps["oneOf"] != nil {
				t.Fatalf("根节点不是普通 object: %v", ps)
			}
			for _, key := range tc.wantProps {
				if props[key] == nil {
					t.Errorf("properties 缺少 %s: %v", key, props)
				}
			}
		})
	}
}

// TestGrokRequestCyclicRefsFinishQuickly 带病态循环 $ref 参数的 function 经 grokRequest 同样很快改写完（请求处理不能被一个恶意 schema 拖住）。
func TestGrokRequestCyclicRefsFinishQuickly(t *testing.T) {
	for _, tc := range grokCyclicSchemas() {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"grok-4.7","tools":[{"type":"function","name":"f","parameters":` + tc.schema + `}]}`
			var out []byte
			withinTime(t, time.Second, "grokRequest", func() { out, _ = grokRequest([]byte(body)) })
			ps, _ := grokToolList(t, out)[0]["parameters"].(map[string]any)
			props, _ := ps["properties"].(map[string]any)
			if ps["type"] != "object" || ps["anyOf"] != nil || ps["oneOf"] != nil {
				t.Fatalf("参数根节点不是普通 object: %v", ps)
			}
			for _, key := range tc.wantProps {
				if props[key] == nil {
					t.Errorf("properties 缺少 %s: %v", key, props)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// R2-e：无环但大扇出的 $ref 链，展开有预算

// boundedRun 在 limit 内执行 fn，并把 fn 里的 panic 报告为测试失败；超时则失败，不等 fn（无预算时 fn 要算上很久）。
func boundedRun(t *testing.T, limit time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		fn()
	}()
	select {
	case p := <-done:
		if p != nil {
			t.Fatalf("%s panic: %v", what, p)
		}
	case <-time.After(limit):
		t.Fatalf("%s 在 %v 内没有完成（无环的 $ref 链按扇出指数展开，没有展开预算）", what, limit)
	}
}

// grokRefChain 返回 8 层 $defs 链的 schema：根与 a1…a7 各是联合，有 fanout 个指向下一层的 $ref 分支，a8 是带自己属性（leaf、n）的 object。
// inline 为 true 时 a1…a7 还各有一个内联的 object 分支，属性 pK 只属于第 K 层。
// 无环，没有展开预算时要展开 fanout^7 次（fanout 为 8 时请求体约 1.5 KB，上游实测超过 10 秒）。
func grokRefChain(fanout int, inline bool) string {
	var defs []string
	for k := 1; k <= 7; k++ {
		branches := grokCycleRefs(fmt.Sprintf("a%d", k+1), fanout)
		if inline {
			branches += fmt.Sprintf(`,{"type":"object","properties":{"p%d":{"type":"string"}}}`, k)
		}
		defs = append(defs, fmt.Sprintf(`"a%d":{"anyOf":[%s]}`, k, branches))
	}
	defs = append(defs, `"a8":{"type":"object","properties":{"leaf":{"type":"string"},"n":{"type":"integer"}},"required":["leaf"]}`)
	return `{"anyOf":[` + grokCycleRefs("a1", fanout) + `],"$defs":{` + strings.Join(defs, ",") + `}}`
}

// assertRefChainResult 断言 ps 是合并后的 object 根：没有联合，第一条路径能走到的叶子属性 leaf 在；inline 时各层自己的 pK 都在
// （内联分支不是 $ref，不受预算影响）。
func assertRefChainResult(t *testing.T, ps map[string]any, inline bool) {
	t.Helper()
	if ps["type"] != "object" || ps["anyOf"] != nil || ps["oneOf"] != nil || ps["allOf"] != nil {
		t.Fatalf("根节点不是普通 object: %v", ps)
	}
	props, _ := ps["properties"].(map[string]any)
	if props["leaf"] == nil {
		t.Errorf("properties 缺少叶子的 leaf（第一条路径上的 $ref 在预算内，应该展开到）: %v", props)
	}
	if inline {
		for k := 1; k <= 7; k++ {
			if key := fmt.Sprintf("p%d", k); props[key] == nil {
				t.Errorf("properties 缺少 %s: %v", key, props)
			}
		}
	}
}

// TestObjectRootRefFanOutIsBounded 8 层 $defs 链、每层 8 或 16 个指向下一层的 $ref 分支：objectRoot 在 1 秒内完成（展开 $ref 有预算，
// 预算用尽后的 $ref 按不可解析处理），结果仍是 object 根，不 panic。
func TestObjectRootRefFanOutIsBounded(t *testing.T) {
	for _, fanout := range []int{8, 16} {
		for _, inline := range []bool{false, true} {
			t.Run(fmt.Sprintf("fanout=%d/inline=%v", fanout, inline), func(t *testing.T) {
				ps := parseSchema(t, grokRefChain(fanout, inline))
				var changed bool
				boundedRun(t, time.Second, "objectRoot", func() { changed = objectRoot(ps) })
				if !changed {
					t.Fatal("应报告有改动")
				}
				assertRefChainResult(t, ps, inline)
			})
		}
	}
}

// TestGrokRequestRefFanOutIsBounded 同样的链经 grokRequest：一个请求里三个 function 都带这样的参数（预算按每次 objectRoot 计，
// 每个工具各算一次，所以三个都能展开到叶子，不是前一个用完了后面的就没有预算），整个请求在 1 秒内改写完。
func TestGrokRequestRefFanOutIsBounded(t *testing.T) {
	for _, fanout := range []int{8, 16} {
		for _, inline := range []bool{false, true} {
			t.Run(fmt.Sprintf("fanout=%d/inline=%v", fanout, inline), func(t *testing.T) {
				schema := grokRefChain(fanout, inline)
				body := `{"model":"grok-4.7","tools":[` +
					`{"type":"function","name":"f1","parameters":` + schema + `},` +
					`{"type":"function","name":"f2","parameters":` + schema + `},` +
					`{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f3","parameters":` + schema + `}]}]}`
				var out []byte
				boundedRun(t, time.Second, "grokRequest", func() { out, _ = grokRequest([]byte(body)) })
				tools := grokToolList(t, out)
				if names := grokToolNames(tools); !reflect.DeepEqual(names, []string{"f1", "f2", "ns__f3"}) {
					t.Fatalf("工具名 = %v", names)
				}
				for _, tool := range tools {
					ps, _ := tool["parameters"].(map[string]any)
					assertRefChainResult(t, ps, inline)
				}
			})
		}
	}
}

// grokFlatRefs 返回根是 anyOf、有 n 个指向各自 $defs 的 $ref 分支的 schema，第 i 个定义是只有属性 pI 的 object。
func grokFlatRefs(n int) string {
	var refs, defs []string
	for i := 0; i < n; i++ {
		refs = append(refs, fmt.Sprintf(`{"$ref":"#/$defs/d%d"}`, i))
		defs = append(defs, fmt.Sprintf(`"d%d":{"type":"object","properties":{"p%d":{"type":"string"}}}`, i, i))
	}
	return `{"anyOf":[` + strings.Join(refs, ",") + `],"$defs":{` + strings.Join(defs, ",") + `}}`
}

// TestObjectRootRefBudgetCoversNormalSchemas 预算够用于正常 schema：一个联合有 200 个各指向不同定义的 $ref 分支时全部展开（预算 256）；
// 多到 2000 个时预算用尽，展开的不超过 256 个，其余按不可解析处理，仍在 1 秒内得到 object 根。
// 上游测试里的 Codex automation_update（TestObjectRootNestedUnion）等已有用例只展开个位数的 $ref，远低于预算，结果与上游一致，由它们覆盖。
func TestObjectRootRefBudgetCoversNormalSchemas(t *testing.T) {
	for _, tc := range []struct {
		name           string
		refs, min, max int
	}{
		{"within-budget", 200, 200, 200},
		{"over-budget", 2000, 200, 256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := parseSchema(t, grokFlatRefs(tc.refs))
			boundedRun(t, time.Second, "objectRoot", func() { objectRoot(ps) })
			props, _ := ps["properties"].(map[string]any)
			if ps["type"] != "object" || ps["anyOf"] != nil || len(props) < tc.min || len(props) > tc.max {
				t.Fatalf("展开了 %d 个 $ref 的属性, want %d 到 %d 个; root = %v", len(props), tc.min, tc.max, ps["type"])
			}
		})
	}
}
