package copilot

// 本文件覆盖 Copilot 的 Gemini 模型在 /chat/completions 上的工具 schema 适配中的 geminiModel、geminiToolsFit，
// 以及适配管线在 Service 中的接线（端到端测试）。geminiToolShapes、geminiToolTypes 的单元测试见 geminishapes_test.go、geminitypes_test.go。
//
// 背景：Copilot 的 Gemini 对某些工具 schema 整个请求返回 400 "invalid request body"：
//   - 工具 schema 的 required 列出了其 properties 里没有的字段，或两个工具同名（geminiToolsFit）；
//   - 其他形状的 schema（无 properties 的对象、同类型的 anyOf、数组上的 enum、根上的联合、prefixItems 等，geminiToolShapes）；
//   - 缺 type 的 schema（Vertex 要求，geminiToolTypes）。
// 注意：Copilot 的 Gemini 对 oneOf 返回 200 却丢掉分支，模型会发出字符串，所以不再把 anyOf 改名为 oneOf（geminiToolUnions 已删除），
// 可证明不相交的对象联合由 geminiToolShapes 折叠成一个对象。
//
// 期望行为：
//   - geminiModel(id)：取最后一个 "/" 之后的部分，转小写后以 "gemini" 开头；
//   - geminiToolsFit(body)：顶层 tools 为数组时，删去 required 里 properties 没有的条目（任意深度，删空则删去 required 键，
//     非字符串条目保留），同名工具只保留第一个；都没有时返回原切片；
//   - 三个函数（geminiToolsFit、geminiToolShapes、geminiToolTypes）本身都不检查模型；Service 只在 /chat/completions 且模型是 Gemini 时
//     依次调用：body = geminiToolTypes(geminiToolShapes(geminiToolsFit(body)))。
//
// 期望值参照 magpie internal/gateway/copilot_gemini_required_test.go 中的 required / 重名工具用例；
// 联合、形状、补 type 的用例见另外两个测试文件。

import (
	"bytes"
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

// geminiKindA、geminiKindB 是两个对象分支（都要求 kind，enum 取值不相交），用来构造联合。
const (
	geminiKindA = `{"type":"object","properties":{"kind":{"type":"string","enum":["a"]},"x":{"type":"string"}},"required":["kind"]}`
	geminiKindB = `{"type":"object","properties":{"kind":{"type":"string","enum":["b"]}},"required":["kind"]}`
)

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

// geminiE2EFixedTools 是 geminiE2ETools 适配后的样子：重名工具只留第一个，缺字段的 required 条目删去，
// create_thread 的对象联合折叠成一个对象（同名属性 type 的 enum 并集，只保留各分支共有的 required，其余字段取第一个分支的），不再有 anyOf/oneOf。
const geminiE2EFixedTools = `[` +
	`{"type":"function","function":{"name":"dup","description":"first","parameters":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}},` +
	`{"type":"function","function":{"name":"create_thread","parameters":{"type":"object","properties":{"target":{"type":"object",` +
	`"properties":{"type":{"type":"string","enum":["project","projectless"]},"project_id":{"type":"string"}},"required":["type"],"additionalProperties":false}},` +
	`"required":["target"],"additionalProperties":false}}}]`

// geminiE2EStagesTools 让适配管线的三步各有一处可见的改动，且彼此顺序有影响：
//   - geminiToolsFit：根的 required 只有缺字段的 ghost，删去后连 required 键一起去掉；
//   - geminiToolShapes：v 的数组 enum 挪到 items；x 写在可空联合旁边的 type 去掉；arr 是没有 items 的数组，补 items:{}；
//   - geminiToolTypes：n 没有 type，按取值补 integer；补出来的 arr.items:{} 也要补 type（所以 Types 必须在 Shapes 之后）；
//     可空联合 x 本身不补 type（Copilot 的 Gemini 对联合旁的 type 返回 400）。
const geminiE2EStagesTools = `[` + `{"type":"function","function":{"name":"stages","parameters":{"type":"object","properties":{` +
	`"v":{"type":"array","enum":["a","b"]},"x":{"type":"string","anyOf":[{"type":"string"},{"type":"null"}]},"arr":{"type":"array"},"n":{"enum":[1,2]}},` +
	`"required":["ghost"]}}}]`

// geminiE2EStagesFixedTools 是 geminiE2EStagesTools 适配后的样子。
const geminiE2EStagesFixedTools = `[` + `{"type":"function","function":{"name":"stages","parameters":{"type":"object","properties":{` +
	`"v":{"type":"array","items":{"type":"string","enum":["a","b"]}},"x":{"anyOf":[{"type":"string"},{"type":"null"}]},` +
	`"arr":{"type":"array","items":{"type":"string"}},"n":{"type":"integer","enum":[1,2]}}}}}]`

// TestGeminiChatCompletionsToolsAreAdapted Gemini 模型走 /chat/completions 时，假上游收到的是适配后的请求体：
// 先修剪 required 与重名工具，再把可证明不相交的对象联合折叠成一个对象（不再改名为 oneOf：Copilot 的 Gemini 对 oneOf 返回 200 却丢掉分支）；
// 其余字段（含数字原文）不变。
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
			if bytes.Contains(got[0], []byte(`"anyOf"`)) || bytes.Contains(got[0], []byte(`"oneOf"`)) {
				t.Fatalf("联合应被折叠，不应留下 anyOf 或 oneOf: %s", got[0])
			}
		})
	}
}

// TestGeminiChatCompletionsToolsRunAllStagesInOrder 三步适配（Fit、Shapes、Types）在 Service 中按此顺序全部生效：
// 假上游收到的 tools 与逐步适配后的结果一致，请求体其余字段（含 temperature 的数字原文 1.0）不变。
func TestGeminiChatCompletionsToolsRunAllStagesInOrder(t *testing.T) {
	s, sent := geminiTestService(t)
	w := testRequest(s, http.MethodPost, "/v1/chat/completions", geminiChatBody("gemini-3.8-flash", geminiE2EStagesTools))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	got := sent("/chat/completions")
	if len(got) != 1 {
		t.Fatalf("上游 /chat/completions 请求数 = %d, want 1", len(got))
	}
	assertSameJSON(t, got[0], geminiChatBody("gemini-3.8-flash", geminiE2EStagesFixedTools))
	if !bytes.Contains(got[0], []byte(`"temperature":1.0`)) {
		t.Errorf("请求体其余字段应保持原样: %s", got[0])
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
	if bytes.Contains(got[0], []byte(`"anyOf"`)) || bytes.Contains(got[0], []byte(`"oneOf"`)) {
		t.Fatalf("联合应被折叠，不应留下 anyOf 或 oneOf: %s", got[0])
	}
}
