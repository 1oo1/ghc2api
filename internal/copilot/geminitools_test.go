package copilot

// 本文件覆盖 Copilot 的 Gemini 模型在 /chat/completions 上的工具 schema 适配（见 geminitools.go）。
//
// 背景：Copilot 的 Gemini 对某些工具 schema 整个请求返回 400 "invalid request body"：
//   - 工具 schema 的 required 列出了其 properties 里没有的字段，或两个工具同名；
//   - 工具 schema 中由对象分支组成的 anyOf（改成 oneOf 就能通过）。
//
// 期望行为：
//   - geminiModel(id)：取最后一个 "/" 之后的部分，转小写后以 "gemini" 开头；
//   - geminiToolsFit(body)：顶层 tools 为数组时，删去 required 里 properties 没有的条目（任意深度，删空则删去 required 键，
//     非字符串条目保留），同名工具只保留第一个；都没有时返回原切片；
//   - geminiToolUnions(body)：工具 schema（function.parameters、parameters、input_schema）中，分支可证明互不相交的对象分支 anyOf
//     原地改名为 oneOf（等长替换，其余字节不动）；证明不了的（含可空联合）保持 anyOf；没有可改名的时返回原切片；
//   - 两个函数本身不检查模型；Service 只在 /chat/completions 且模型是 Gemini 时依次调用二者（先 geminiToolsFit，再 geminiToolUnions）。
//
// 实现者必须提供的内部名字（测试按此书写）：
//   - func geminiModel(id string) bool
//   - func geminiToolsFit(body []byte) []byte
//   - func geminiToolUnions(body []byte) []byte
//
// 期望值参照 magpie internal/gateway/copilot_gemini_required_test.go、copilot_gemini_grok_test.go 中 Gemini 的 oneOf / required / 重名工具用例
// （不含 Grok 与 "可空联合补 type" 的用例）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGeminiModel 模型 id 取最后一个 "/" 之后的部分，转小写后以 gemini 开头。
func TestGeminiModel(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"gemini-3.8-flash", true},
		{"Gemini-2.5-Pro", true},
		{"GEMINI-3", true},
		{"gemini", true},
		{"geminix", true}, // 只看前缀
		{"google/gemini-3.8-flash", true},
		{"vertex_ai/gemini-1.5", true},
		{"a/b/Gemini-3", true}, // 取最后一个 "/" 之后
		{"gpt-4.1", false},
		{"claude-sonnet-4", false},
		{"", false},
		{"my-gemini-1", false},    // 不是前缀
		{"gemini/gpt-4.1", false}, // 最后一段是 gpt-4.1
		{"gemini-3/", false},      // 最后一个 "/" 之后为空
		{"/gemini-3", true},       // 前导 "/" 之后是 gemini
		{" gemini-3", false},      // 不裁剪空白
		{"gem-ini-3", false},
	} {
		if got := geminiModel(tc.id); got != tc.want {
			t.Errorf("geminiModel(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// geminiChatBody 构造带 tools 的 Chat Completions 请求体；tools 是已编码的 JSON 数组。
func geminiChatBody(model, tools string) string {
	return fmt.Sprintf(`{"model":%q,"temperature":1.0,"messages":[{"role":"user","content":"hi"}],"tools":%s}`, model, tools)
}

// geminiFn 构造 Chat Completions 风格的函数工具。
func geminiFn(name, parameters string) string {
	return fmt.Sprintf(`{"type":"function","function":{"name":%q,"parameters":%s}}`, name, parameters)
}

// runGeminiTransform 调用 fn 并断言输入切片未被修改。
func runGeminiTransform(t *testing.T, fn func([]byte) []byte, body string) []byte {
	t.Helper()
	in := []byte(body)
	original := append([]byte(nil), in...)
	out := fn(in)
	if !bytes.Equal(in, original) {
		t.Fatalf("输入切片被修改:\n got=%s\nwant=%s", in, original)
	}
	return out
}

// TestGeminiToolsFitPrunesRequiredAndDuplicates required 里 properties 没有的条目被删去（任意深度，删空则删去 required 键，
// 非字符串条目保留），同名工具只保留第一个（其 schema 照常修剪），其余字段与工具顺序不变，数字原文保留。
func TestGeminiToolsFitPrunesRequiredAndDuplicates(t *testing.T) {
	const strProp = `{"type":"string"}`
	for _, tc := range []struct{ name, tools, want string }{
		{
			"ghost-next-to-real-entry",
			`[` + geminiFn("f", `{"type":"object","properties":{"a":`+strProp+`},"required":["ghost","a"]}`) + `]`,
			`[` + geminiFn("f", `{"type":"object","properties":{"a":`+strProp+`},"required":["a"]}`) + `]`,
		},
		{
			"every-entry-is-a-ghost-drops-the-key",
			`[` + geminiFn("f", `{"type":"object","properties":{"a":`+strProp+`},"required":["ghost","other"],"additionalProperties":false}`) + `]`,
			`[` + geminiFn("f", `{"type":"object","properties":{"a":`+strProp+`},"additionalProperties":false}`) + `]`,
		},
		{
			// magpie 的 nest 用例：内层 required 的 deep 不在内层 properties 中，外层的 o 在。
			"nested-object",
			`[` + geminiFn("nest", `{"type":"object","properties":{"o":{"type":"object","properties":{"x":`+strProp+`},"required":["deep"]}},"required":["o"]}`) + `]`,
			`[` + geminiFn("nest", `{"type":"object","properties":{"o":{"type":"object","properties":{"x":`+strProp+`}}},"required":["o"]}`) + `]`,
		},
		{
			"no-properties-at-all",
			`[` + geminiFn("f", `{"type":"object","required":["a"]}`) + `]`,
			`[` + geminiFn("f", `{"type":"object"}`) + `]`,
		},
		{
			"inside-array-items-and-union-branches-and-defs",
			`[` + geminiFn("f", `{"type":"object","properties":{`+
				`"list":{"type":"array","items":{"type":"object","properties":{"k":`+strProp+`},"required":["k","ghost"]}},`+
				`"u":{"anyOf":[{"type":"object","properties":{"p":`+strProp+`},"required":["q"]},{"type":"null"}]}},`+
				`"$defs":{"d":{"type":"object","properties":{"p":`+strProp+`},"required":["p","r"]}}}`) + `]`,
			`[` + geminiFn("f", `{"type":"object","properties":{`+
				`"list":{"type":"array","items":{"type":"object","properties":{"k":`+strProp+`},"required":["k"]}},`+
				`"u":{"anyOf":[{"type":"object","properties":{"p":`+strProp+`}},{"type":"null"}]}},`+
				`"$defs":{"d":{"type":"object","properties":{"p":`+strProp+`},"required":["p"]}}}`) + `]`,
		},
		{
			// 非字符串条目既不算缺字段，也不被删。
			"non-string-entries-are-kept",
			`[` + geminiFn("f", `{"type":"object","properties":{"a":`+strProp+`},"required":["ghost",7,"a",null]}`) + `]`,
			`[` + geminiFn("f", `{"type":"object","properties":{"a":`+strProp+`},"required":[7,"a",null]}`) + `]`,
		},
		{
			// magpie 的 dup 用例：第一个 dup 的 ghost 被删，第二个 dup 整个删去，其余工具不变。
			"duplicate-keeps-the-first",
			`[` + geminiFn("dup", `{"type":"object","properties":{"a":`+strProp+`},"required":["ghost","a"],"description":"first"}`) + `,` +
				geminiFn("dup", `{"type":"object","properties":{},"description":"second"}`) + `,` +
				geminiFn("other", `{"type":"object","properties":{}}`) + `]`,
			`[` + geminiFn("dup", `{"type":"object","properties":{"a":`+strProp+`},"required":["a"],"description":"first"}`) + `,` +
				geminiFn("other", `{"type":"object","properties":{}}`) + `]`,
		},
		{
			"duplicates-keep-order-of-first-occurrences",
			`[` + geminiFn("a", `{}`) + `,` + geminiFn("b", `{}`) + `,` + geminiFn("a", `{"x":1}`) + `,` + geminiFn("c", `{}`) + `,` + geminiFn("b", `{"y":2}`) + `]`,
			`[` + geminiFn("a", `{}`) + `,` + geminiFn("b", `{}`) + `,` + geminiFn("c", `{}`) + `]`,
		},
		{
			// 没有名字的工具（没有 function.name，如 web_search）不参与去重。
			"unnamed-tools-are-not-deduplicated",
			`[{"type":"web_search"},{"type":"web_search"},{"type":"function","function":{"parameters":{}}},{"type":"function","function":{"parameters":{}}}]`,
			`[{"type":"web_search"},{"type":"web_search"},{"type":"function","function":{"parameters":{}}},{"type":"function","function":{"parameters":{}}}]`,
		},
		{
			"number-literals-and-other-fields-are-kept",
			`[` + geminiFn("f", `{"type":"object","properties":{"n":{"type":"number","minimum":1.0,"maximum":12345678901234567890,"default":2.50,"description":"<b> & c"}},"required":["ghost"],"additionalProperties":false}`) + `]`,
			`[` + geminiFn("f", `{"type":"object","properties":{"n":{"type":"number","minimum":1.0,"maximum":12345678901234567890,"default":2.50,"description":"<b> & c"}},"additionalProperties":false}`) + `]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, model := range []string{"gemini-3.8-flash", "gpt-4.1"} { // 函数本身不检查模型
				out := runGeminiTransform(t, geminiToolsFit, geminiChatBody(model, tc.tools))
				assertSameJSON(t, out, geminiChatBody(model, tc.want))
			}
		})
	}
}

// TestGeminiToolsFitOnlyTouchesTools 只处理顶层 tools：其他位置（response_format 的 schema 等）里同样的 required 缺字段不动。
func TestGeminiToolsFitOnlyTouchesTools(t *testing.T) {
	const format = `"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object","properties":{"a":{"type":"string"}},"required":["ghost"]}}}`
	tool := func(required string) string {
		return `[` + geminiFn("f", `{"type":"object","properties":{"a":{"type":"string"}},"required":[`+required+`]}`) + `]`
	}
	body := `{"model":"gemini-3.8-flash",` + format + `,"tools":` + tool(`"ghost","a"`) + `}`
	want := `{"model":"gemini-3.8-flash",` + format + `,"tools":` + tool(`"a"`) + `}`
	assertSameJSON(t, runGeminiTransform(t, geminiToolsFit, body), want)
}

// TestGeminiToolsFitLeavesCleanBodies 没有可改的内容时返回传入的原切片（逐字节一致，不修改输入）：
// 没有 tools、tools 不是数组、工具干净、required 的形状不需要修剪、请求体不是 JSON 对象。
func TestGeminiToolsFitLeavesCleanBodies(t *testing.T) {
	const cleanTools = `[` +
		`{"type":"function","function":{"name":"a","parameters":{"type":"object","properties":{"x":{"type":"string"},"y":{"type":"number"}},"required":["x","y"]}}},` +
		`{"type":"function","function":{"name":"b","parameters":{"type":"object","properties":{}}}}]`
	for _, tc := range []struct{ name, body string }{
		{"no-tools-key", `{"model":"gemini-3.8-flash","messages":[]}`},
		{"tools-null", `{"model":"gemini-3.8-flash","tools":null}`},
		{"tools-object", `{"model":"gemini-3.8-flash","tools":{"required":["ghost"]}}`},
		{"tools-string", `{"model":"gemini-3.8-flash","tools":"required"}`},
		{"tools-empty-array", `{"model":"gemini-3.8-flash","tools":[]}`},
		{"clean-tools", geminiChatBody("gemini-3.8-flash", cleanTools)},
		{"clean-tools-spaced-json", "{ \"model\" : \"gemini-3.8-flash\" ,\n \"temperature\" : 1.0 ,\n \"tools\" : [ { \"type\" : \"function\" ,\n \"function\" : { \"name\" : \"a\" , \"parameters\" : { \"type\" : \"object\" , \"properties\" : { \"x\" : { \"type\" : \"string\" , \"description\" : \"<b> & c\" } } , \"required\" : [ \"x\" ] , \"n\" : 2.50 } } } ] }"},
		{"required-is-a-boolean", `{"model":"m","tools":[` + geminiFn("f", `{"type":"object","properties":{"a":{"type":"string","required":true}}}`) + `]}`},
		{"required-is-empty", `{"model":"m","tools":[` + geminiFn("f", `{"type":"object","properties":{},"required":[]}`) + `]}`},
		{"required-has-only-non-strings", `{"model":"m","tools":[` + geminiFn("f", `{"type":"object","properties":{},"required":[1,null,true]}`) + `]}`},
		{"property-named-required", `{"model":"m","tools":[` + geminiFn("f", `{"type":"object","properties":{"required":{"type":"boolean"}}}`) + `]}`},
		{"ghost-outside-tools-only", `{"model":"m","response_format":{"json_schema":{"schema":{"properties":{},"required":["ghost"]}}},"tools":` + cleanTools + `}`},
		{"json-array", `[` + geminiFn("f", `{"properties":{},"required":["ghost"]}`) + `]`},
		{"json-null", `null`},
		{"not-json", `not json "required"`},
		{"empty", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.body)
			original := append([]byte(nil), in...)
			out := geminiToolsFit(in)
			if !bytes.Equal(in, original) {
				t.Fatalf("输入切片被修改: %s", in)
			}
			assertSameSlice(t, out, in)
		})
	}
}

// geminiUnion 构造由 branches 组成的 anyOf。
func geminiUnion(branches ...string) string {
	return `{"anyOf":[` + strings.Join(branches, ",") + `]}`
}

// geminiParams 构造以 prop 为属性 p 的对象 schema。
func geminiParams(prop string) string {
	return `{"type":"object","properties":{"p":` + prop + `}}`
}

// renameAnyOf 返回 body 的副本，其中第 nths 个（从 1 起，按文本顺序）出现的 "anyOf" 被等长改写为 "oneOf"。
func renameAnyOf(t *testing.T, body string, nths ...int) string {
	t.Helper()
	var at []int
	for i := 0; ; {
		j := strings.Index(body[i:], `"anyOf"`)
		if j < 0 {
			break
		}
		at = append(at, i+j)
		i += j + len(`"anyOf"`)
	}
	out := []byte(body)
	for _, n := range nths {
		if n < 1 || n > len(at) {
			t.Fatalf("请求体中没有第 %d 个 \"anyOf\"（共 %d 个）: %s", n, len(at), body)
		}
		copy(out[at[n-1]:], `"oneOf"`)
	}
	return string(out)
}

const (
	// 两个分支都要求 kind，且 enum 取值不相交。
	geminiKindA = `{"type":"object","properties":{"kind":{"type":"string","enum":["a"]},"x":{"type":"string"}},"required":["kind"]}`
	geminiKindB = `{"type":"object","properties":{"kind":{"type":"string","enum":["b"]}},"required":["kind"]}`
	geminiKindC = `{"type":"object","properties":{"kind":{"type":"string","enum":["c"]}},"required":["kind"]}`
	// 一个分支要求 x，另一个分支不认识 x 且 additionalProperties 为 false。
	geminiNeedsX    = `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`
	geminiForbidsX  = `{"type":"object","properties":{"y":{"type":"string"}},"required":["y"],"additionalProperties":false}`
	geminiForbidsX2 = `{"type":"object","properties":{"y":{"type":"string"}},"additionalProperties":false}`
)

// TestGeminiToolUnionsRenamesProvenUnions 分支可证明互不相交的对象分支 anyOf 原地改名为 oneOf（等长替换，其余字节不动）：
// 要求的字段在两个分支都必填且字符串取值（enum、const）不相交，或一个分支要求的字段在另一个分支被
// additionalProperties:false（且没有 patternProperties）禁止；所有两两分支都要被证明。
func TestGeminiToolUnionsRenamesProvenUnions(t *testing.T) {
	for _, tc := range []struct{ name, union string }{
		{"enum-disjoint", geminiUnion(geminiKindA, geminiKindB)},
		{"const-disjoint", geminiUnion(
			`{"type":"object","properties":{"kind":{"type":"string","const":"a"}},"required":["kind"]}`,
			`{"type":"object","properties":{"kind":{"type":"string","const":"b"}},"required":["kind"]}`)},
		{"enum-against-const", geminiUnion(
			`{"type":"object","properties":{"kind":{"enum":["a","b"]}},"required":["kind"]}`,
			`{"type":"object","properties":{"kind":{"const":"c"}},"required":["kind"]}`)},
		{"multi-value-enums-disjoint", geminiUnion(
			`{"type":"object","properties":{"kind":{"enum":["a","b"]}},"required":["kind"]}`,
			`{"type":"object","properties":{"kind":{"enum":["c","d"]}},"required":["kind"]}`)},
		{"other-branch-forbids-the-field", geminiUnion(geminiNeedsX, geminiForbidsX)},
		{"other-branch-forbids-the-field-reverse-order", geminiUnion(geminiForbidsX, geminiNeedsX)},
		{"other-branch-forbids-the-field-without-required", geminiUnion(geminiNeedsX, geminiForbidsX2)},
		{"three-branches-pairwise-apart", geminiUnion(geminiKindA, geminiKindB, geminiKindC)},
		{"mixed-proofs-across-pairs", geminiUnion(geminiKindA, geminiKindB, geminiForbidsX)},
		{
			// magpie 的 copilotUnionsRequest：Codex Desktop 的 create_thread，type 的取值互斥，project_id 又被另一分支禁止。
			"codex-create-thread-target",
			geminiUnion(
				`{"type":"object","properties":{"type":{"type":"string","enum":["project"]},"project_id":{"type":"string"}},"required":["type","project_id"],"additionalProperties":false}`,
				`{"type":"object","properties":{"type":{"type":"string","enum":["projectless"]}},"required":["type"],"additionalProperties":false}`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", geminiParams(tc.union))+`]`)
			out := runGeminiTransform(t, geminiToolUnions, body)
			if want := renameAnyOf(t, body, 1); string(out) != want {
				t.Fatalf("应只把 anyOf 原地改名为 oneOf:\n got=%s\nwant=%s", out, want)
			}
		})
	}
}

// TestGeminiToolUnionsKeepsUnprovenUnions 不能证明不相交的联合保持 anyOf，请求体逐字节不变（返回原切片）：
// 可空联合、没有 required 的重叠分支、取值有交集、一侧非必填、没有 enum、$ref 分支、非对象分支、单分支、
// 数值 enum/const、patternProperties 可能放行字段，以及三个分支中有一对证明不了。
func TestGeminiToolUnionsKeepsUnprovenUnions(t *testing.T) {
	for _, tc := range []struct{ name, union string }{
		{"nullable-string", `{"anyOf":[{"type":"string"},{"type":"null"}],"description":"Project name"}`},
		{"nullable-pydantic-with-default", `{"anyOf":[{"type":"string"},{"type":"null"}],"default":null,"description":"Project name"}`},
		{"nullable-object", geminiUnion(geminiKindA, `{"type":"null"}`)},
		{"nullable-object-first", geminiUnion(`{"type":"null"}`, geminiKindA)},
		{
			// magpie 的 copilotOverlappingRequest：两个分支都没有 required，也不禁止任何字段。
			"overlapping-without-required",
			geminiUnion(
				`{"type":"object","properties":{"round":{"type":"boolean"}}}`,
				`{"type":"object","properties":{"square":{"type":"boolean"}}}`),
		},
		{"enums-share-a-value", geminiUnion(
			`{"type":"object","properties":{"kind":{"enum":["a","b"]}},"required":["kind"]}`,
			`{"type":"object","properties":{"kind":{"enum":["b","c"]}},"required":["kind"]}`)},
		{"enum-contains-the-const", geminiUnion(
			`{"type":"object","properties":{"kind":{"enum":["a","b"]}},"required":["kind"]}`,
			`{"type":"object","properties":{"kind":{"const":"b"}},"required":["kind"]}`)},
		{"field-optional-in-one-branch", geminiUnion(geminiKindA,
			`{"type":"object","properties":{"kind":{"type":"string","enum":["b"]}}}`)},
		{"no-enum-on-one-side", geminiUnion(geminiKindA,
			`{"type":"object","properties":{"kind":{"type":"string"}},"required":["kind"]}`)},
		{"other-branch-does-not-forbid-the-field", geminiUnion(geminiNeedsX,
			`{"type":"object","properties":{"y":{"type":"string"}},"required":["y"]}`)},
		{"additional-properties-true", geminiUnion(geminiNeedsX,
			`{"type":"object","properties":{"y":{"type":"string"}},"required":["y"],"additionalProperties":true}`)},
		{"additional-properties-is-a-schema", geminiUnion(geminiNeedsX,
			`{"type":"object","properties":{"y":{"type":"string"}},"required":["y"],"additionalProperties":{"type":"string"}}`)},
		{"pattern-properties-may-admit-the-field", geminiUnion(geminiNeedsX,
			`{"type":"object","properties":{"y":{"type":"string"}},"required":["y"],"additionalProperties":false,"patternProperties":{"^x":{"type":"string"}}}`)},
		{"ref-branch", geminiUnion(`{"$ref":"#/$defs/a"}`, geminiKindB)},
		{"branch-without-type", geminiUnion(
			`{"properties":{"kind":{"enum":["a"]}},"required":["kind"]}`, geminiKindB)},
		{"branch-of-other-type", geminiUnion(`{"type":"string"}`, geminiKindB)},
		{"branch-type-is-an-array", geminiUnion(
			`{"type":["object","null"],"properties":{"kind":{"enum":["a"]}},"required":["kind"]}`, geminiKindB)},
		{"single-branch", geminiUnion(geminiKindA)},
		{"empty-union", `{"anyOf":[]}`},
		{"union-is-not-an-array", `{"anyOf":{"type":"object"}}`},
		{"one-pair-unproven-among-three", geminiUnion(geminiKindA, geminiKindB, geminiKindB)},
		{"non-string-enum-values", geminiUnion(
			`{"type":"object","properties":{"kind":{"enum":[1,2]}},"required":["kind"]}`,
			`{"type":"object","properties":{"kind":{"enum":[3]}},"required":["kind"]}`)},
		{"numeric-const", geminiUnion(
			`{"type":"object","properties":{"kind":{"const":1}},"required":["kind"]}`,
			`{"type":"object","properties":{"kind":{"const":2}},"required":["kind"]}`)},
		{"enum-field-is-a-ref", geminiUnion(
			`{"type":"object","properties":{"kind":{"$ref":"#/$defs/k"}},"required":["kind"]}`, geminiKindB)},
		{"non-string-required-entries", geminiUnion(
			`{"type":"object","properties":{"kind":{"enum":["a"]}},"required":[1]}`, geminiKindB)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", geminiParams(tc.union))+`]`)
			in := []byte(body)
			original := append([]byte(nil), in...)
			out := geminiToolUnions(in)
			if !bytes.Equal(in, original) {
				t.Fatalf("输入切片被修改: %s", in)
			}
			assertSameSlice(t, out, in)
		})
	}
}

// TestGeminiToolUnionsFindsUnionsInToolSchemas 联合可以出现在工具 schema 的各个位置与三种工具形状里：
// 根、properties、嵌套、items、$defs、definitions、additionalProperties、allOf、oneOf 与 anyOf 分支里的属性、patternProperties、
// prefixItems、dependentSchemas；function.parameters（Chat）、parameters（Responses）、input_schema（Anthropic）。
func TestGeminiToolUnionsFindsUnionsInToolSchemas(t *testing.T) {
	u := geminiUnion(geminiKindA, geminiKindB)
	for _, tc := range []struct {
		name, params string
		nths         []int
	}{
		{"root", u, []int{1}},
		{"property", geminiParams(u), []int{1}},
		{"nested-property", geminiParams(geminiParams(u)), []int{1}},
		{"array-items", geminiParams(`{"type":"array","items":` + u + `}`), []int{1}},
		{"defs", `{"type":"object","properties":{"p":{"$ref":"#/$defs/u"}},"$defs":{"u":` + u + `}}`, []int{1}},
		{"definitions", `{"type":"object","properties":{"p":{"$ref":"#/definitions/u"}},"definitions":{"u":` + u + `}}`, []int{1}},
		{"additional-properties-schema", `{"type":"object","additionalProperties":` + u + `}`, []int{1}},
		{"all-of", `{"allOf":[` + geminiParams(u) + `]}`, []int{1}},
		{"one-of-branch-property", `{"oneOf":[` + geminiParams(u) + `,{"type":"string"}]}`, []int{1}},
		{"pattern-properties", `{"type":"object","patternProperties":{"^x":` + u + `}}`, []int{1}},
		{"prefix-items", `{"type":"array","prefixItems":[` + u + `]}`, []int{1}},
		{"dependent-schemas", `{"type":"object","dependentSchemas":{"a":` + geminiParams(u) + `}}`, []int{1}},
		{
			// 外层可空联合证明不了、保持 anyOf；它分支里的联合照常改名（第 2 个 anyOf）。
			"inside-an-unproven-union-branch",
			geminiUnion(geminiParams(u), `{"type":"null"}`), []int{2},
		},
		{
			// 外层联合可证明，分支里的联合也可证明：两个都改名。
			"inside-a-proven-union-branch",
			geminiUnion(
				`{"type":"object","properties":{"kind":{"enum":["a"]},"p":`+u+`},"required":["kind"]}`,
				geminiKindB),
			[]int{1, 2},
		},
		{
			// 同一个 schema 里一个可证明、一个是可空联合：只有可证明的改名，与出现的先后无关。
			"proven-and-nullable-side-by-side",
			`{"type":"object","properties":{"a":` + u + `,"b":{"anyOf":[{"type":"string"},{"type":"null"}]}}}`, []int{1},
		},
		{
			"nullable-and-proven-side-by-side",
			`{"type":"object","properties":{"a":{"anyOf":[{"type":"string"},{"type":"null"}]},"b":` + u + `}}`, []int{2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", tc.params)+`]`)
			out := runGeminiTransform(t, geminiToolUnions, body)
			if want := renameAnyOf(t, body, tc.nths...); string(out) != want {
				t.Fatalf("改名结果不符:\n got=%s\nwant=%s", out, want)
			}
		})
	}

	t.Run("three-tool-shapes", func(t *testing.T) {
		p := geminiParams(u)
		tools := `[{"type":"function","function":{"name":"f1","parameters":` + p + `}},` +
			`{"type":"function","name":"f2","parameters":` + p + `},` +
			`{"name":"f3","input_schema":` + p + `}]`
		body := geminiChatBody("gemini-3.8-flash", tools)
		out := runGeminiTransform(t, geminiToolUnions, body)
		if want := renameAnyOf(t, body, 1, 2, 3); string(out) != want {
			t.Fatalf("三种工具形状都应改名:\n got=%s\nwant=%s", out, want)
		}
	})
}

// TestGeminiToolUnionsOnlyTouchesToolSchemas 只改工具 schema 里的联合：请求体其他位置（response_format 的 schema、消息内容）、
// 工具中 function.parameters / parameters / input_schema 以外的字段、schema 里作为数据出现的值（default、examples）都不动。
func TestGeminiToolUnionsOnlyTouchesToolSchemas(t *testing.T) {
	u := geminiUnion(geminiKindA, geminiKindB)
	t.Run("outside-tools-and-other-tool-fields-are-untouched", func(t *testing.T) {
		body := `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"anyOf"}],` +
			`"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":` + geminiParams(u) + `}},` +
			`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}},"metadata":` + u + `},"extra":` + u + `}]}`
		in := []byte(body)
		assertSameSlice(t, geminiToolUnions(in), in)
	})
	t.Run("only-the-tool-union-is-renamed", func(t *testing.T) {
		body := `{"model":"gemini-3.8-flash","response_format":{"json_schema":{"schema":` + geminiParams(u) + `}},` +
			`"tools":[` + geminiFn("f", geminiParams(u)) + `]}`
		out := runGeminiTransform(t, geminiToolUnions, body)
		if want := renameAnyOf(t, body, 2); string(out) != want {
			t.Fatalf("只应改名工具里的联合（第 2 个 anyOf）:\n got=%s\nwant=%s", out, want)
		}
	})
	t.Run("data-values-inside-a-schema-are-untouched", func(t *testing.T) {
		params := `{"type":"object","properties":{"p":{"type":"object","default":` + u + `,"examples":[` + u + `]}}}`
		body := geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", params)+`]`)
		in := []byte(body)
		assertSameSlice(t, geminiToolUnions(in), in)
	})
}

// TestGeminiToolUnionsRenamesOnlyTheKey 只改联合的键：同一请求体里叫 anyOf 的属性名、取值为 "anyOf" 的字符串不受影响。
func TestGeminiToolUnionsRenamesOnlyTheKey(t *testing.T) {
	params := `{"type":"object","properties":{"anyOf":{"type":"string","enum":["anyOf"]},"t":` + geminiUnion(geminiKindA, geminiKindB) + `}}`
	body := geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", params)+`]`)
	out := runGeminiTransform(t, geminiToolUnions, body)
	// 文本顺序：属性名 "anyOf"（1）、enum 取值 "anyOf"（2）、联合的键（3）。
	if want := renameAnyOf(t, body, 3); string(out) != want {
		t.Fatalf("只应改名联合的键:\n got=%s\nwant=%s", out, want)
	}
}

// TestGeminiToolUnionsKeepsFormatting 改名是等长替换，空白与排版原样保留：缩进格式化的请求体、键与冒号之间有空白的请求体。
func TestGeminiToolUnionsKeepsFormatting(t *testing.T) {
	compact := geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", geminiParams(geminiUnion(geminiKindA, geminiKindB)))+`]`)
	var indented bytes.Buffer
	if err := json.Indent(&indented, []byte(compact), "", "  "); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, body string }{
		{"indented", indented.String()},
		{"space-around-colon", strings.ReplaceAll(compact, `"anyOf":`, "\"anyOf\" \n\t:  ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runGeminiTransform(t, geminiToolUnions, tc.body)
			if want := renameAnyOf(t, tc.body, 1); string(out) != want {
				t.Fatalf("格式应保持不变:\n got=%s\nwant=%s", out, want)
			}
		})
	}
}

// TestGeminiToolUnionsKeepsExistingOneOf 同一个 schema 里已经有 oneOf 时不改 anyOf。
func TestGeminiToolUnionsKeepsExistingOneOf(t *testing.T) {
	params := `{"type":"object","properties":{"p":{"anyOf":[` + geminiKindA + `,` + geminiKindB + `],"oneOf":[{"type":"string"}]}}}`
	body := []byte(geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", params)+`]`))
	assertSameSlice(t, geminiToolUnions(body), body)
}

// TestGeminiToolUnionsLeavesOtherBodiesAlone 没有 anyOf、没有 tools、请求体不是合法 JSON 对象时返回原切片；函数本身不检查模型。
func TestGeminiToolUnionsLeavesOtherBodiesAlone(t *testing.T) {
	u := geminiUnion(geminiKindA, geminiKindB)
	for _, tc := range []struct{ name, body string }{
		{"no-union-at-all", geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", `{"type":"object","properties":{}}`)+`]`)},
		{"no-tools", `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`},
		{"tools-null", `{"model":"gemini-3.8-flash","tools":null,"x":` + u + `}`},
		{"tools-empty", `{"model":"gemini-3.8-flash","tools":[],"x":` + u + `}`},
		{"json-array", `[` + geminiFn("f", geminiParams(u)) + `]`},
		{"not-json", `not json "anyOf"`},
		{"empty", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.body)
			original := append([]byte(nil), in...)
			out := geminiToolUnions(in)
			if !bytes.Equal(in, original) {
				t.Fatalf("输入切片被修改: %s", in)
			}
			assertSameSlice(t, out, in)
		})
	}
	t.Run("model-is-not-checked", func(t *testing.T) {
		body := geminiChatBody("gpt-4.1", `[`+geminiFn("f", geminiParams(u))+`]`)
		out := runGeminiTransform(t, geminiToolUnions, body)
		if want := renameAnyOf(t, body, 1); string(out) != want {
			t.Fatalf("函数本身不检查模型:\n got=%s\nwant=%s", out, want)
		}
	})
}

// TestGeminiToolUnionsEscapedKeys 键写成转义形式时按解码后的名字参与判断（与上游 gjson 的解码语义一致）：
// 转义写法的 oneOf、required、properties、type、enum、additionalProperties、patternProperties、$ref、$defs 与三种工具键都当作普通键；
// 但联合自己的键是转义写法时长度不同、不能等长替换，保持不变（输出仍是合法 JSON）。
func TestGeminiToolUnionsEscapedKeys(t *testing.T) {
	const (
		kindA = `{"type":"object","properties":{"kind":{"enum":["a"]}},"required":["kind"]}`
		kindB = `{"type":"object","properties":{"kind":{"enum":["b"]}},"required":["kind"]}`
	)
	u := geminiUnion(kindA, kindB)
	for _, tc := range []struct {
		name, params string
		nths         []int // 要改名的第几个（从 1 起）普通 "anyOf"；nil 表示请求体应逐字节不变
	}{
		{"escaped-required-key", `{"type":"object","properties":{"p":` + geminiUnion(
			`{"type":"object","properties":{"kind":{"enum":["a"]}},"\u0072equired":["kind"]}`,
			`{"type":"object","properties":{"kind":{"enum":["b"]}},"\u0072equired":["kind"]}`) + `}}`, []int{1}},
		{"escaped-required-entry", `{"type":"object","properties":{"p":` + geminiUnion(
			`{"type":"object","properties":{"kind":{"enum":["a"]}},"required":["\u006bind"]}`,
			`{"type":"object","properties":{"kind":{"enum":["b"]}},"required":["\u006bind"]}`) + `}}`, []int{1}},
		{"escaped-type-key", `{"type":"object","properties":{"p":` + geminiUnion(
			`{"\u0074ype":"object","properties":{"kind":{"enum":["a"]}},"required":["kind"]}`, kindB) + `}}`, []int{1}},
		{"escaped-properties-key-in-branches", `{"type":"object","properties":{"p":` + geminiUnion(
			`{"type":"object","\u0070roperties":{"kind":{"enum":["a"]}},"required":["kind"]}`, kindB) + `}}`, []int{1}},
		{"escaped-enum-key", `{"type":"object","properties":{"p":` + geminiUnion(
			`{"type":"object","properties":{"kind":{"\u0065num":["a"]}},"required":["kind"]}`, kindB) + `}}`, []int{1}},
		{"escaped-const-key", `{"type":"object","properties":{"p":` + geminiUnion(
			`{"type":"object","properties":{"kind":{"\u0063onst":"a"}},"required":["kind"]}`, kindB) + `}}`, []int{1}},
		{"escaped-enum-values-disjoint", `{"type":"object","properties":{"p":` + geminiUnion(
			`{"type":"object","properties":{"kind":{"enum":["\u0061"]}},"required":["kind"]}`, kindB) + `}}`, []int{1}},
		{"escaped-enum-values-shared", `{"type":"object","properties":{"p":` + geminiUnion(
			`{"type":"object","properties":{"kind":{"enum":["\u0061"]}},"required":["kind"]}`,
			`{"type":"object","properties":{"kind":{"enum":["a"]}},"required":["kind"]}`) + `}}`, nil},
		{"escaped-additionalProperties-key-forbids", `{"type":"object","properties":{"p":` + geminiUnion(
			geminiNeedsX,
			`{"type":"object","properties":{"y":{"type":"string"}},"required":["y"],"additional\u0050roperties":false}`) + `}}`, []int{1}},
		{"escaped-patternProperties-key-blocks-the-proof", `{"type":"object","properties":{"p":` + geminiUnion(
			geminiNeedsX,
			`{"type":"object","properties":{"y":{"type":"string"}},"required":["y"],"additionalProperties":false,"pattern\u0050roperties":{"^x":{"type":"string"}}}`) + `}}`, nil},
		{"escaped-ref-key-in-branch", `{"type":"object","properties":{"p":` + geminiUnion(`{"\u0024ref":"#/$defs/a"}`, kindB) + `}}`, nil},
		{"escaped-oneOf-key-beside-the-union", `{"type":"object","properties":{"p":{"\u006fneOf":[{"type":"string"}],"anyOf":[` + kindA + `,` + kindB + `]}}}`, nil},
		{"escaped-properties-key-holds-the-union", `{"type":"object","\u0070roperties":{"p":` + u + `}}`, []int{1}},
		{"escaped-defs-key-holds-the-union", `{"type":"object","\u0024defs":{"p":` + u + `}}`, []int{1}},
		{"escaped-items-key-holds-the-union", `{"type":"array","\u0069tems":` + u + `}`, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", tc.params)+`]`)
			out := runGeminiTransform(t, geminiToolUnions, body)
			want := body
			if tc.nths != nil {
				want = renameAnyOf(t, body, tc.nths...)
			}
			if string(out) != want {
				t.Fatalf("改名结果不符:\n got=%s\nwant=%s", out, want)
			}
			if !json.Valid(out) {
				t.Fatalf("输出不是合法 JSON: %s", out)
			}
		})
	}

	// 联合自己的键是转义写法："\u0061nyOf" 与 "anyOf" 长度不同，不能等长替换，保持不变；同一请求中另一工具的普通 anyOf 照常改名。
	t.Run("escaped-union-key-is-left-alone-and-plain-one-is-renamed", func(t *testing.T) {
		escaped := geminiFn("escaped", `{"type":"object","properties":{"p":{"\u0061nyOf":[`+geminiKindA+`,`+geminiKindB+`]}}}`)
		plain := geminiFn("plain", geminiParams(geminiUnion(geminiKindA, geminiKindB)))
		body := geminiChatBody("gemini-3.8-flash", `[`+escaped+`,`+plain+`]`)
		out := runGeminiTransform(t, geminiToolUnions, body)
		// 请求体里只有一个普通写法的 "anyOf"（第二个工具的）。
		if want := renameAnyOf(t, body, 1); string(out) != want {
			t.Fatalf("只应改名普通写法的联合:\n got=%s\nwant=%s", out, want)
		}
		if !json.Valid(out) {
			t.Fatalf("输出不是合法 JSON: %s", out)
		}
		if !bytes.Contains(out, []byte(`"\u0061nyOf"`)) {
			t.Fatalf("转义写法的键应原样保留: %s", out)
		}
	})

	t.Run("escaped-union-key-alone-leaves-the-body-untouched", func(t *testing.T) {
		body := []byte(geminiChatBody("gemini-3.8-flash", `[`+geminiFn("escaped", `{"type":"object","properties":{"p":{"\u0061nyOf":[`+geminiKindA+`,`+geminiKindB+`]}}}`)+`]`))
		assertSameSlice(t, geminiToolUnions(body), body)
	})

	// 工具自身的键也可以是转义写法（Chat 的 function.parameters、Responses 的 parameters、Anthropic 的 input_schema）。
	t.Run("escaped-tool-keys", func(t *testing.T) {
		p := geminiParams(u)
		tools := `[{"type":"function","\u0066unction":{"name":"f1","\u0070arameters":` + p + `}},` +
			`{"name":"f2","input\u005fschema":` + p + `},` +
			`{"type":"function","name":"f3","\u0070arameters":` + p + `}]`
		body := geminiChatBody("gemini-3.8-flash", tools)
		out := runGeminiTransform(t, geminiToolUnions, body)
		if want := renameAnyOf(t, body, 1, 2, 3); string(out) != want {
			t.Fatalf("改名结果不符:\n got=%s\nwant=%s", out, want)
		}
	})
}

// TestGeminiToolsFitEscapedKeys required、properties、function、name 写成转义形式时按解码后的名字参与判断：
// 缺字段的 required 条目照常删去，重名工具照常去重（转义写法的 name 值与普通写法相同也算重名）。
func TestGeminiToolsFitEscapedKeys(t *testing.T) {
	const strProp = `{"type":"string"}`
	body := geminiChatBody("gemini-3.8-flash", `[`+
		`{"type":"function","\u0066unction":{"name":"dup","parameters":{"type":"object","\u0070roperties":{"a":`+strProp+`},"\u0072equired":["\u0067host","a"]}}},`+
		`{"type":"function","function":{"name":"d\u0075p","parameters":{}}},`+
		geminiFn("other", `{"type":"object","properties":{}}`)+`]`)
	want := geminiChatBody("gemini-3.8-flash", `[`+
		geminiFn("dup", `{"type":"object","properties":{"a":`+strProp+`},"required":["a"]}`)+`,`+
		geminiFn("other", `{"type":"object","properties":{}}`)+`]`)
	assertSameJSON(t, runGeminiTransform(t, geminiToolsFit, body), want)
}

// geminiLeadingSpaces 是 JSON 允许出现在请求体前（后）的空白。
var geminiLeadingSpaces = []struct{ name, space string }{
	{"space", " "}, {"newline", "\n"}, {"tab-cr-lf", "\t\r\n  "},
}

// TestGeminiToolsFitLeadingWhitespace 请求体以空白开头（或结尾带换行）时照常适配：顶层 tools 照样被找到。
func TestGeminiToolsFitLeadingWhitespace(t *testing.T) {
	const tools = `[{"type":"function","function":{"name":"dup","parameters":{"type":"object","properties":{"a":{"type":"string"}},"required":["ghost","a"]}}},` +
		`{"type":"function","function":{"name":"dup","parameters":{}}}]`
	const fixed = `[{"type":"function","function":{"name":"dup","parameters":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}}]`
	for _, tc := range geminiLeadingSpaces {
		t.Run(tc.name, func(t *testing.T) {
			out := runGeminiTransform(t, geminiToolsFit, tc.space+geminiChatBody("gemini-3.8-flash", tools)+"\n")
			assertSameJSON(t, out, geminiChatBody("gemini-3.8-flash", fixed))
		})
		t.Run(tc.name+"-clean-body-is-returned-as-is", func(t *testing.T) {
			body := []byte(tc.space + geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", `{"type":"object","properties":{}}`)+`]`))
			assertSameSlice(t, geminiToolsFit(body), body)
		})
	}
}

// TestGeminiToolUnionsLeadingWhitespace 请求体以空白开头时照常改名，其余字节（含开头的空白）不动。
func TestGeminiToolUnionsLeadingWhitespace(t *testing.T) {
	compact := geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", geminiParams(geminiUnion(geminiKindA, geminiKindB)))+`]`)
	for _, tc := range geminiLeadingSpaces {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.space + compact + "\n"
			out := runGeminiTransform(t, geminiToolUnions, body)
			if want := renameAnyOf(t, body, 1); string(out) != want {
				t.Fatalf("改名结果不符:\n got=%s\nwant=%s", out, want)
			}
		})
		t.Run(tc.name+"-nothing-to-rename", func(t *testing.T) {
			body := []byte(tc.space + geminiChatBody("gemini-3.8-flash", `[`+geminiFn("f", geminiParams(`{"anyOf":[{"type":"string"},{"type":"null"}]}`))+`]`))
			assertSameSlice(t, geminiToolUnions(body), body)
		})
	}
}

// geminiTestModels 是端到端测试使用的 /models 响应：两个模型都声明支持三种生成路径，
// 这样同一个 Gemini 模型也能用来验证 /responses、/v1/messages 不被改写。
const geminiTestModels = `{"data":[` +
	`{"id":"gemini-3.8-flash","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions","/responses","/v1/messages"],"policy":{"state":"enabled"}},` +
	`{"id":"gpt-4.1","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions","/responses","/v1/messages"],"policy":{"state":"enabled"}}]}`

// geminiTestService 创建使用假上游的 Service，返回读取上游收到的生成请求体的函数。
func geminiTestService(t *testing.T) (*Service, func(path string) [][]byte) {
	t.Helper()
	var mu sync.Mutex
	bodies := map[string][][]byte{}
	s := testService(t, Config{APIKey: testAPIKey, Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, geminiTestModels)
		case "/chat/completions", "/responses", "/v1/messages":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("读取上游请求体失败: %v", err)
			}
			mu.Lock()
			bodies[r.URL.Path] = append(bodies[r.URL.Path], data)
			mu.Unlock()
			testJSON(w, map[string]string{"ok": "yes"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})})
	return s, func(path string) [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies[path]...)
	}
}

// geminiE2ETools 同时包含三个问题：重名工具（第一个还带缺字段的 required）、嵌套的缺字段 required、
// 以及 create_thread 的对象联合（它的 required 里还有一个缺字段的 ghost）。
const geminiE2ETools = `[` +
	`{"type":"function","function":{"name":"dup","description":"first","parameters":{"type":"object","properties":{"a":{"type":"string"}},"required":["ghost","a"]}}},` +
	`{"type":"function","function":{"name":"dup","description":"second","parameters":{"type":"object","properties":{}}}},` +
	`{"type":"function","function":{"name":"create_thread","parameters":{"type":"object","properties":{"target":{"anyOf":[` +
	`{"type":"object","properties":{"type":{"type":"string","enum":["project"]},"project_id":{"type":"string"}},"required":["type","project_id"],"additionalProperties":false},` +
	`{"type":"object","properties":{"type":{"type":"string","enum":["projectless"]}},"required":["type"],"additionalProperties":false}]}},` +
	`"required":["target","ghost"],"additionalProperties":false}}}]`

// geminiE2EFixedTools 是 geminiE2ETools 适配后的样子：重名工具只留第一个，缺字段的 required 条目删去，联合改为 oneOf。
const geminiE2EFixedTools = `[` +
	`{"type":"function","function":{"name":"dup","description":"first","parameters":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}},` +
	`{"type":"function","function":{"name":"create_thread","parameters":{"type":"object","properties":{"target":{"oneOf":[` +
	`{"type":"object","properties":{"type":{"type":"string","enum":["project"]},"project_id":{"type":"string"}},"required":["type","project_id"],"additionalProperties":false},` +
	`{"type":"object","properties":{"type":{"type":"string","enum":["projectless"]}},"required":["type"],"additionalProperties":false}]}},` +
	`"required":["target"],"additionalProperties":false}}}]`

// TestGeminiChatCompletionsToolsAreAdapted Gemini 模型走 /chat/completions 时，假上游收到的是适配后的请求体：
// 先修剪 required 与重名工具，再把可证明不相交的联合改成 oneOf；其余字段（含数字原文）不变。
func TestGeminiChatCompletionsToolsAreAdapted(t *testing.T) {
	for _, path := range []string{"/chat/completions", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			s, sent := geminiTestService(t)
			w := testRequest(s, http.MethodPost, path, geminiChatBody("gemini-3.8-flash", geminiE2ETools))
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
			}
			got := sent("/chat/completions")
			if len(got) != 1 {
				t.Fatalf("上游 /chat/completions 请求数 = %d, want 1", len(got))
			}
			assertSameJSON(t, got[0], geminiChatBody("gemini-3.8-flash", geminiE2EFixedTools))
			if bytes.Contains(got[0], []byte(`"anyOf"`)) || !bytes.Contains(got[0], []byte(`"oneOf"`)) {
				t.Fatalf("联合应改为 oneOf: %s", got[0])
			}
		})
	}
}

// TestGeminiToolsAreNotAdaptedElsewhere 非 Gemini 模型、Gemini 模型的 /responses 与 /v1/messages、以及没有可适配内容的 Gemini 请求，
// 转发给上游的请求体都与客户端发送的逐字节一致。
func TestGeminiToolsAreNotAdaptedElsewhere(t *testing.T) {
	responsesTools := `[{"type":"function","name":"dup","parameters":{"type":"object","properties":{"a":{"type":"string"}},"required":["ghost","a"]}},` +
		`{"type":"function","name":"dup","parameters":{"type":"object","properties":{}}},` +
		`{"type":"function","name":"t","parameters":{"type":"object","properties":{"p":` + geminiUnion(geminiKindA, geminiKindB) + `}}}]`
	messagesTools := `[{"name":"t","input_schema":{"type":"object","properties":{"p":` + geminiUnion(geminiKindA, geminiKindB) + `},"required":["ghost"]}}]`
	cleanTools := `[` + geminiFn("f", geminiParams(`{"anyOf":[{"type":"string"},{"type":"null"}],"description":"nullable"}`)) + `]`
	for _, tc := range []struct{ name, path, upstream, body string }{
		{"non-gemini-chat", "/v1/chat/completions", "/chat/completions", geminiChatBody("gpt-4.1", geminiE2ETools)},
		{"gemini-responses", "/v1/responses", "/responses",
			`{"model":"gemini-3.8-flash","input":"hi","tools":` + responsesTools + `}`},
		{"gemini-responses-without-v1", "/responses", "/responses",
			`{"model":"gemini-3.8-flash","input":"hi","tools":` + responsesTools + `}`},
		{"gemini-messages", "/v1/messages", "/v1/messages",
			`{"model":"gemini-3.8-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":` + messagesTools + `}`},
		{"gemini-chat-nothing-to-adapt", "/v1/chat/completions", "/chat/completions", geminiChatBody("gemini-3.8-flash", cleanTools)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sent := geminiTestService(t)
			w := testRequest(s, http.MethodPost, tc.path, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
			}
			got := sent(tc.upstream)
			if len(got) != 1 || !bytes.Equal(got[0], []byte(tc.body)) {
				t.Fatalf("转发的请求体应与客户端发送的逐字节一致:\n got=%q\nwant=%s", got, tc.body)
			}
		})
	}
}

// TestGeminiChatCompletionsToolsAreAdaptedWithLeadingWhitespace 请求体以空白开头（JSON 允许）的 Gemini /chat/completions 请求同样被适配。
func TestGeminiChatCompletionsToolsAreAdaptedWithLeadingWhitespace(t *testing.T) {
	s, sent := geminiTestService(t)
	w := testRequest(s, http.MethodPost, "/v1/chat/completions", "\n  "+geminiChatBody("gemini-3.8-flash", geminiE2ETools)+"\n")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	got := sent("/chat/completions")
	if len(got) != 1 {
		t.Fatalf("上游 /chat/completions 请求数 = %d, want 1", len(got))
	}
	assertSameJSON(t, got[0], geminiChatBody("gemini-3.8-flash", geminiE2EFixedTools))
	if bytes.Contains(got[0], []byte(`"anyOf"`)) {
		t.Fatalf("联合应改为 oneOf: %s", got[0])
	}
}
