package copilot

// 本文件覆盖 geminiToolShapes（见 geminishapes.go），以及它与 geminiToolTypes 共同遵守的约定（tools 之外的字节不动、无改动返回原切片、
// 非法输入不 panic 等，geminitypes_test.go 里的用例不再重复这一类）。
//
// 背景：Copilot 的 Gemini 对六种形状的工具 schema 返回 400 "invalid request body"（MCP 服务常写出这些形状）：
// 没有 properties 的对象、分支同类型的 anyOf、有分支无法判断类型的 anyOf、数组上的 enum、根上的联合、元组数组（prefixItems）。
// geminiToolShapes 把每种形状改写成 Gemini 接受的最接近的 schema；另外，Copilot 的 Gemini 对 oneOf 返回 200 却丢掉其分支
// （模型于是发出字符串），所以可证明不相交的对象联合（如 Codex 的 create_thread 的 target）也折叠成同一个对象，而不是改名为 oneOf。
//
// 期望行为（对应 magpie internal/gateway/copilot_gemini_shapes.go 的 copilotGeminiToolShapes，但不检查模型/协议）：
//   - func geminiToolShapes(body []byte) []byte；
//   - 没有可改内容（tools 不是数组、请求体不是 JSON 对象、非法输入等）时返回原切片；有改动时只替换 tools 数组的原文，
//     请求体其余字节不变；数字原文保留（1.0、1e3 不变形）、不转义 HTML（<>&）；
//   - 只处理 tools[*].function.parameters、tools[*].parameters、tools[*].input_schema；default、examples、enum 等数据值里的对象不当作 schema；
//   - 不 panic。
//
// 与上游测试的差异（本项目没有翻译层，也不改写 JSON 的键序）：
//   - 上游用例经 gateway 翻译后用子串比较（键按字母序）；这里解析成 JSON 后按语义比较，不依赖键序；
//   - 上游用例跑整条管线并用 fake 上游按规则拒绝；这里直接断言管线的输出 schema 的结构，并把 fake 的拒绝规则改写成结构检查；
//   - 管线是 geminiToolTypes(geminiToolShapes(geminiToolsFit(body)))，见 geminiPipeline。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// geminiPipeline 是 Service 对 Gemini /chat/completions 请求体做的完整适配（顺序见 service.go）。
func geminiPipeline(body []byte) []byte {
	return geminiToolTypes(geminiToolShapes(geminiToolsFit(body)))
}

// geminiTransforms 是遵守同一组「共同约定」的函数：两个单步函数与整条管线。
var geminiTransforms = []struct {
	name string
	fn   func([]byte) []byte
}{
	{"geminiToolShapes", geminiToolShapes},
	{"geminiToolTypes", geminiToolTypes},
	{"pipeline", geminiPipeline},
}

// geminiOneToolBody 构造只有一个工具（function.parameters 为 schema）的请求体，其余字段与 geminiChatBody 相同（含 temperature 的 1.0）。
func geminiOneToolBody(schema string) string {
	return geminiChatBody("gemini-3.8-flash", `[`+geminiFn("t", schema)+`]`)
}

// decodeJSONValue 以 UseNumber 解析任意 JSON 值（数字保持原文）。
func decodeJSONValue(t *testing.T, data []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("解析 JSON 失败: %v; data=%s", err, data)
	}
	return v
}

// lookupJSON 按点号分隔的路径取值：对象按键，数组按下标（如 tools.0.function.parameters.properties.x）。键里不含点号。
func lookupJSON(v any, path string) (any, bool) {
	for _, seg := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			next, ok := x[seg]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// assertJSONAt 断言 data（JSON）在 path 处的值与 want（JSON 文本）语义一致（数字按原文比较）。
func assertJSONAt(t *testing.T, data []byte, path, want string) {
	t.Helper()
	got, ok := lookupJSON(decodeJSONValue(t, data), path)
	if !ok {
		t.Fatalf("路径 %s 不存在:\n%s", path, data)
	}
	if w := decodeJSONValue(t, []byte(want)); !reflect.DeepEqual(got, w) {
		gotJSON, _ := json.Marshal(got)
		t.Fatalf("%s 的值不符:\n got=%s\nwant=%s", path, gotJSON, want)
	}
}

// assertNoJSONAt 断言 data（JSON）在 path 处没有值。
func assertNoJSONAt(t *testing.T, data []byte, path string) {
	t.Helper()
	if got, ok := lookupJSON(decodeJSONValue(t, data), path); ok {
		gotJSON, _ := json.Marshal(got)
		t.Fatalf("路径 %s 不应存在，得到 %s", path, gotJSON)
	}
}

// assertToolSchema 断言 body 的 tools[0].function.parameters 与 want（JSON 文本）语义一致。
func assertToolSchema(t *testing.T, body []byte, want string) {
	t.Helper()
	assertJSONAt(t, body, "tools.0.function.parameters", want)
}

// assertAround 断言 out 以 prefix 开头、以 suffix 结尾（逐字节），且中间是 tools 数组（合法 JSON）：tools 之外的字节没有被改动。
func assertAround(t *testing.T, out []byte, prefix, suffix string) {
	t.Helper()
	if len(out) < len(prefix)+len(suffix) || !bytes.HasPrefix(out, []byte(prefix)) || !bytes.HasSuffix(out, []byte(suffix)) {
		t.Fatalf("tools 之外的字节被改动:\n got=%q\nwant 前缀=%q 后缀=%q", out, prefix, suffix)
	}
	mid := out[len(prefix) : len(out)-len(suffix)]
	if len(mid) == 0 || mid[0] != '[' || !json.Valid(mid) {
		t.Fatalf("前缀与后缀之间应是 tools 数组: %q", mid)
	}
}

// assertChatToolsOnlyChanged 断言 out 与 in（geminiChatBody 构造，tools 是最后一个键）只有 tools 数组的内容不同。
func assertChatToolsOnlyChanged(t *testing.T, in, out []byte) {
	t.Helper()
	const key = `"tools":`
	at := bytes.Index(in, []byte(key))
	if at < 0 {
		t.Fatalf("输入里没有 tools: %s", in)
	}
	assertAround(t, out, string(in[:at+len(key)]), "}")
}

// assertUnchangedSlice 调用 fn 并断言输入未被修改、返回的是传入的原切片。
func assertUnchangedSlice(t *testing.T, fn func([]byte) []byte, body string) {
	t.Helper()
	in := []byte(body)
	original := append([]byte(nil), in...)
	out := fn(in)
	if !bytes.Equal(in, original) {
		t.Fatalf("输入切片被修改:\n got=%s\nwant=%s", in, original)
	}
	assertSameSlice(t, out, in)
}

// geminiBadShape 说出 Copilot 的 Gemini 会返回 400 的 schema 形状（上游 badGeminiShape），能接受的 schema 返回空串。
func geminiBadShape(node any, root bool) string {
	switch v := node.(type) {
	case []any:
		for _, e := range v {
			if s := geminiBadShape(e, false); s != "" {
				return s
			}
		}
	case map[string]any:
		if _, ok := v["prefixItems"]; ok {
			return "prefixItems"
		}
		if _, ok := v["enum"]; ok && v["type"] == "array" {
			return "array enum"
		}
		if root && (v["oneOf"] != nil || v["anyOf"] != nil) {
			return "root union"
		}
		if v["type"] == "object" && v["properties"] == nil {
			return "object without properties"
		}
		if alts, ok := v["anyOf"].([]any); ok {
			types := map[string]bool{}
			nonNull := 0
			for _, a := range alts {
				m, _ := a.(map[string]any)
				typ, _ := m["type"].(string)
				if typ == "" {
					return "untyped branch"
				}
				if typ != "null" {
					types[typ] = true
					nonNull++
				}
			}
			if len(types) == 1 && nonNull > 1 {
				return "same type"
			}
		}
		for _, e := range v {
			if s := geminiBadShape(e, false); s != "" {
				return s
			}
		}
	}
	return ""
}

// geminiReporterRefuses 复现上游 fake 对报告者用例的拒绝规则（#4374）：anyOf/oneOf 节点旁写着 type，或数组没有 items。
func geminiReporterRefuses(node any) bool {
	switch v := node.(type) {
	case map[string]any:
		_, anyOf := v["anyOf"]
		_, oneOf := v["oneOf"]
		if _, hasType := v["type"]; (anyOf || oneOf) && hasType {
			return true
		}
		if v["type"] == "array" && v["items"] == nil {
			return true
		}
		for _, c := range v {
			if geminiReporterRefuses(c) {
				return true
			}
		}
	case []any:
		for _, c := range v {
			if geminiReporterRefuses(c) {
				return true
			}
		}
	}
	return false
}

// geminiShapeCases 是上游 TestCopilotGeminiTakesToolShapes 的 8 行，外加一行「items 写成列表」。
// shaped 是只经 geminiToolShapes 的 schema，final 是经完整管线的 schema（二者只在「分支无法判断类型」一行不同：
// 该节点的联合被丢掉后，由 geminiToolTypes 按描述补成 string）。
var geminiShapeCases = []struct{ name, schema, shaped, final string }{
	{
		"object without properties",
		`{"type":"object","properties":{"opts":{"type":"object","description":"o"}}}`,
		`{"type":"object","properties":{"opts":{"type":"object","description":"o","properties":{}}}}`, "",
	},
	{
		"object at the root without properties",
		`{"type":"object"}`,
		`{"type":"object","properties":{}}`, "",
	},
	{
		"anyOf of one type beside its type",
		`{"type":"object","properties":{"v":{"description":"d","type":"string","anyOf":[{"type":"string"},{"type":"string"}]}}}`,
		`{"type":"object","properties":{"v":{"description":"d","type":"string"}}}`, "",
	},
	{
		"anyOf of one type",
		`{"type":"object","properties":{"v":{"anyOf":[{"type":"string","enum":["a"]},{"type":"string","enum":["b"]}]}}}`,
		`{"type":"object","properties":{"v":{"type":"string","enum":["a","b"]}}}`, "",
	},
	{
		"anyOf with untyped branches",
		`{"type":"object","properties":{"v":{"description":"d","anyOf":[{"description":"x"},{"type":"string"}]}}}`,
		`{"type":"object","properties":{"v":{"description":"d"}}}`,
		`{"type":"object","properties":{"v":{"description":"d","type":"string"}}}`,
	},
	{
		"enum on an array",
		`{"type":"object","properties":{"v":{"type":"array","enum":["a","b"]}}}`,
		`{"type":"object","properties":{"v":{"type":"array","items":{"type":"string","enum":["a","b"]}}}}`, "",
	},
	{
		"oneOf at the root",
		`{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"number"}},"required":["b"]}]}`,
		`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}}}`, "",
	},
	{
		"prefixItems",
		`{"type":"object","properties":{"p":{"type":"array","prefixItems":[{"type":"number"},{"type":"number"}]}}}`,
		`{"type":"object","properties":{"p":{"type":"array","items":{"type":"number"}}}}`, "",
	},
	{
		// 上游 shapeForGemini 把 items 写成列表也当元组：各位置不同的 schema 合成 anyOf（联合节点本身不补 type）。
		"items as a list",
		`{"type":"object","properties":{"p":{"type":"array","items":[{"type":"string"},{"type":"number"}]}}}`,
		`{"type":"object","properties":{"p":{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"number"}]}}}}`, "",
	},
}

// TestGeminiToolShapesTakesToolShapes 每种形状改写成 Gemini 接受的最接近的 schema（上游 TestCopilotGeminiTakesToolShapes）：
// 单独的 geminiToolShapes 给出 shaped，完整管线给出 final，且 final 里不再有 Copilot 会拒绝的形状（上游 fake 的拒绝规则）。
// tools 之外的字节不变。
func TestGeminiToolShapesTakesToolShapes(t *testing.T) {
	for _, tc := range geminiShapeCases {
		t.Run(tc.name, func(t *testing.T) {
			final := tc.final
			if final == "" {
				final = tc.shaped
			}
			body := geminiOneToolBody(tc.schema)

			out := runGeminiTransform(t, geminiToolShapes, body)
			assertToolSchema(t, out, tc.shaped)
			assertChatToolsOnlyChanged(t, []byte(body), out)

			piped := runGeminiTransform(t, geminiPipeline, body)
			assertToolSchema(t, piped, final)
			assertChatToolsOnlyChanged(t, []byte(body), piped)
			params, _ := lookupJSON(decodeJSONValue(t, piped), "tools.0.function.parameters")
			if bad := geminiBadShape(params, true); bad != "" {
				t.Errorf("管线的输出仍有 Copilot 会拒绝的形状 %q: %s", bad, piped)
			}
		})
	}
}

// TestGeminiToolShapesLeavesFitSchemas 没有上述六种形状的 schema 原样通过：返回原切片（上游 TestCopilotGeminiToolShapesLeavesFitSchemas）。
// 可空联合（anyOf 加 null）、非根的 oneOf、带 items 的数组都是 Copilot 接受的。上游的 Antigravity 用例与本项目无关，不移植。
func TestGeminiToolShapesLeavesFitSchemas(t *testing.T) {
	body := geminiOneToolBody(`{"type":"object","properties":{"n":{"anyOf":[{"type":"string"},{"type":"null"}]},` +
		`"u":{"oneOf":[{"type":"string"},{"type":"number"}]},"a":{"type":"array","items":{"type":"string"}}}}`)
	assertUnchangedSlice(t, geminiToolShapes, body)
	assertUnchangedSlice(t, geminiPipeline, body)
}

// geminiCodexCreateThread 是 Codex 的 create_thread 工具的 schema：target 是互不相交的对象联合
// （各分支要求的 type 取值不相交，#3931 的比较证明过的写法）。
const geminiCodexCreateThread = `{"type":"object","properties":{"target":{"description":"Where the thread lives.","anyOf":[` +
	`{"type":"object","properties":{"type":{"type":"string","enum":["project"]},"project_id":{"type":"string"}},"required":["type","project_id"],"additionalProperties":false},` +
	`{"type":"object","properties":{"type":{"type":"string","enum":["projectless"]}},"required":["type"],"additionalProperties":false}]}},` +
	`"required":["target"],"additionalProperties":false}`

// TestGeminiToolShapesFoldsCodexUnions Copilot 的 Gemini 对 anyOf 的对象分支返回 400（#4374），对 oneOf 返回 200 却丢掉分支，
// 模型于是发出字符串。互不相交的分支折叠成它们描述的那一个对象：同名属性 type 的 enum 取并集，只保留各分支共有的 required，
// 其余字段取第一个分支的；不再有 anyOf 或 oneOf（上游 TestCopilotGeminiFoldsCodexUnions，那里经 Responses 翻译，这里直接是 chat 工具）。
func TestGeminiToolShapesFoldsCodexUnions(t *testing.T) {
	const want = `{"type":"object","properties":{"target":{"description":"Where the thread lives.","type":"object",` +
		`"properties":{"type":{"type":"string","enum":["project","projectless"]},"project_id":{"type":"string"}},` +
		`"required":["type"],"additionalProperties":false}},"required":["target"],"additionalProperties":false}`
	body := geminiOneToolBody(geminiCodexCreateThread)
	for _, tc := range []struct {
		name string
		fn   func([]byte) []byte
	}{{"geminiToolShapes", geminiToolShapes}, {"pipeline", geminiPipeline}} {
		t.Run(tc.name, func(t *testing.T) {
			out := runGeminiTransform(t, tc.fn, body)
			assertToolSchema(t, out, want)
			if bytes.Contains(out, []byte(`"anyOf"`)) || bytes.Contains(out, []byte(`"oneOf"`)) {
				t.Errorf("联合应被折叠，不应留下 anyOf 或 oneOf: %s", out)
			}
		})
	}
}

// TestGeminiToolShapesReporterSchemas 报告者（DaVinci42，#4374，v0.1.3925）的四个 schema，原样（上游 TestCopilotGeminiReporterSchemas）。
// 断言完整管线输出的 schema；not 是输出文本里不应出现的片段。
func TestGeminiToolShapesReporterSchemas(t *testing.T) {
	cases := []struct{ name, schema, want, not string }{
		{
			"disjoint object anyOf",
			`{"type":"object","properties":{"target":{"anyOf":[
              {"type":"object","properties":{"type":{"type":"string","enum":["project"]},"projectId":{"type":"string"}},"required":["type","projectId"],"additionalProperties":false},
              {"type":"object","properties":{"type":{"type":"string","enum":["none"]}},"required":["type"],"additionalProperties":false}]}},"required":["target"]}`,
			`{"type":"object","properties":{"target":{"type":"object","properties":{"projectId":{"type":"string"},"type":{"type":"string","enum":["project","none"]}},` +
				`"required":["type"],"additionalProperties":false}},"required":["target"]}`,
			`Of"`,
		},
		{
			// $ref 分支按 $defs 解析后再判断，不会因为「看不出类型」被丢成无类型的 string。
			"$ref object anyOf",
			`{"type":"object","$defs":{"ById":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]},"ByPath":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
              "properties":{"target":{"anyOf":[{"$ref":"#/$defs/ById"},{"$ref":"#/$defs/ByPath"}]}},"required":["target"]}`,
			`{"type":"object","$defs":{"ById":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]},"ByPath":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},` +
				`"properties":{"target":{"type":"object","properties":{"id":{"type":"string"},"path":{"type":"string"}}}},"required":["target"]}`,
			`"target":{"type":"string"`,
		},
		{
			// 写在可空单分支联合旁边的 type 会让 Copilot 返回 400，去掉；联合本身保留。
			"type beside a nullable anyOf",
			`{"type":"object","properties":{"x":{"type":"string","anyOf":[{"type":"string"},{"type":"null"}]}}}`,
			`{"type":"object","properties":{"x":{"anyOf":[{"type":"string"},{"type":"null"}]}}}`,
			`"x":{"type":"string"`,
		},
		{
			"bare array",
			`{"type":"object","properties":{"items":{"type":"array"}}}`,
			`{"type":"object","properties":{"items":{"type":"array","items":{"type":"string"}}}}`,
			``,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiOneToolBody(tc.schema)
			out := runGeminiTransform(t, geminiPipeline, body)
			assertToolSchema(t, out, tc.want)
			assertChatToolsOnlyChanged(t, []byte(body), out)
			if tc.not != "" && bytes.Contains(out, []byte(tc.not)) {
				t.Errorf("输出里不应有 %s: %s", tc.not, out)
			}
			params, _ := lookupJSON(decodeJSONValue(t, out), "tools.0.function.parameters")
			if geminiReporterRefuses(params) {
				t.Errorf("输出仍有 Copilot 会拒绝的形状: %s", out)
			}
		})
	}
}

// TestGeminiToolShapesKeepsUnprovenUnions 分支类型不同的联合（这里是对象与数组）无法折叠成一个 schema，保持原样：
// 单步与管线都返回原切片（上游 TestCopilotGeminiKeepsUnprovenUnions：那里的 fake 会拒绝并把 400 原样给客户端，
// 本项目没有 fake 上游，等价的断言是「请求体逐字节不变」）。分支同类型的对象联合见 TestGeminiToolShapesMergesSameTypeObjectUnions。
func TestGeminiToolShapesKeepsUnprovenUnions(t *testing.T) {
	body := geminiOneToolBody(`{"type":"object","properties":{"shape":{"description":"The shape.","anyOf":[` +
		`{"type":"object","properties":{"round":{"type":"boolean"}}},{"type":"array","items":{"type":"string"}}]}}}`)
	assertUnchangedSlice(t, geminiToolShapes, body)
	assertUnchangedSlice(t, geminiPipeline, body)
}

// TestGeminiToolShapesMergesSameTypeObjectUnions 分支同类型但不能证明不相交的对象联合也合并成一个对象，属性取并集，没有必填
// （上游提交 65cb76ca2：此前这样的联合不被改写而被 Copilot 拒绝）。
func TestGeminiToolShapesMergesSameTypeObjectUnions(t *testing.T) {
	body := geminiOneToolBody(`{"type":"object","properties":{"shape":{"description":"The shape.","anyOf":[` +
		`{"type":"object","properties":{"round":{"type":"boolean"}}},{"type":"object","properties":{"square":{"type":"boolean"}}}]}}}`)
	out := runGeminiTransform(t, geminiToolShapes, body)
	assertToolSchema(t, out, `{"type":"object","properties":{"shape":{"description":"The shape.","type":"object",`+
		`"properties":{"round":{"type":"boolean"},"square":{"type":"boolean"}}}}}`)
}

// TestGeminiToolShapesKeepsNullableUnionsUntyped Codex Desktop 的 move_thread_to_project（#4374）：可空联合（anyOf 或 oneOf，
// 在属性、数组 items、$defs 里）都按客户端写的样子保留，旁边不写 type：Copilot 的 Gemini 对联合旁的 type 返回 400
// （上游 TestCopilotGeminiKeepsNullableUnionsUntyped）。单步 geminiToolShapes 没有可改的内容，返回原切片；管线补 type 时也跳过联合节点。
func TestGeminiToolShapesKeepsNullableUnionsUntyped(t *testing.T) {
	body := geminiOneToolBody(`{"type":"object","properties":{` +
		`"projectId":{"anyOf":[{"type":"string"},{"type":"null"}],"description":"Destination, or null."},` +
		`"alt":{"oneOf":[{"type":"integer"},{"type":"null"}]},` +
		`"many":{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"null"}]}},` +
		`"ref":{"$ref":"#/$defs/maybe"}},` +
		`"$defs":{"maybe":{"anyOf":[{"type":"number"},{"type":"null"}]}},` +
		`"required":["projectId"],"additionalProperties":false}`)
	assertUnchangedSlice(t, geminiToolShapes, body)

	out := runGeminiTransform(t, geminiPipeline, body)
	const base = "tools.0.function.parameters."
	assertJSONAt(t, out, base+"properties.projectId", `{"anyOf":[{"type":"string"},{"type":"null"}],"description":"Destination, or null."}`)
	assertJSONAt(t, out, base+"properties.alt", `{"oneOf":[{"type":"integer"},{"type":"null"}]}`)
	assertJSONAt(t, out, base+"properties.many", `{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"null"}]}}`)
	assertJSONAt(t, out, base+"$defs.maybe", `{"anyOf":[{"type":"number"},{"type":"null"}]}`)
	params, _ := lookupJSON(decodeJSONValue(t, out), "tools.0.function.parameters")
	if geminiReporterRefuses(params) {
		t.Errorf("联合旁不应写 type: %s", out)
	}
}

// TestGeminiToolShapesLooksIntoSubschemas schema 里放子 schema 的各个关键字之下的形状都被改写
// （properties、items、additionalProperties、$defs、definitions、patternProperties、allOf、anyOf/oneOf 的分支、not、contains）。
// 每行把 inner（一个没有 properties 的对象）放在该关键字下，期望它补上 properties:{}。
func TestGeminiToolShapesLooksIntoSubschemas(t *testing.T) {
	const inner, fixed = `{"type":"object"}`, `{"type":"object","properties":{}}`
	for _, tc := range []struct{ name, tmpl string }{
		{"properties", `{"type":"object","properties":{"x":%s}}`},
		{"items", `{"type":"array","items":%s}`},
		{"additionalProperties", `{"type":"object","properties":{},"additionalProperties":%s}`},
		{"$defs", `{"type":"object","properties":{},"$defs":{"d":%s}}`},
		{"definitions", `{"type":"object","properties":{},"definitions":{"d":%s}}`},
		{"patternProperties", `{"type":"object","properties":{},"patternProperties":{"^a":%s}}`},
		{"allOf", `{"type":"object","properties":{},"allOf":[%s]}`},
		{"anyOf-branch", `{"type":"object","properties":{"u":{"anyOf":[%s,{"type":"string"}]}}}`},
		{"oneOf-branch", `{"type":"object","properties":{"u":{"oneOf":[%s,{"type":"string"}]}}}`},
		{"not", `{"type":"object","properties":{"u":{"not":%s}}}`},
		{"contains", `{"type":"array","items":{"type":"string"},"contains":%s}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiOneToolBody(strings.Replace(tc.tmpl, "%s", inner, 1))
			out := runGeminiTransform(t, geminiToolShapes, body)
			assertToolSchema(t, out, strings.Replace(tc.tmpl, "%s", fixed, 1))
		})
	}
}

// TestGeminiToolShapesLeavesDataValuesAlone default、examples、enum、const 是数据，其中形如 schema 的对象（缺 properties 的对象、
// 数组上的 enum）原样保留；同一 schema 里真正的 schema 照常改写。只有数据里有这些形状时返回原切片。
func TestGeminiToolShapesLeavesDataValuesAlone(t *testing.T) {
	const data = `"cfg":{"type":"object","properties":{"o":{"type":"string"}},"default":{"type":"object"},` +
		`"examples":[{"type":"array","enum":["a"]},{"type":"object"}]},` +
		`"e":{"type":"string","enum":[{"type":"object"}],"const":{"type":"array","enum":["a"]}}`
	only := geminiOneToolBody(`{"type":"object","properties":{` + data + `}}`)
	assertUnchangedSlice(t, geminiToolShapes, only)

	body := geminiOneToolBody(`{"type":"object","properties":{` + data + `,"n":{"type":"object"}}}`)
	out := runGeminiTransform(t, geminiToolShapes, body)
	assertToolSchema(t, out, `{"type":"object","properties":{`+data+`,"n":{"type":"object","properties":{}}}}`)
}

// geminiNoopBodies 是不需要改动的请求体：三个函数（geminiToolShapes、geminiToolTypes、管线）都应返回传入的原切片。
// 其中 schema 都会让某个函数「如果走错了位置」就被改写（缺 properties 的对象、缺 type 的节点），用来证明没有走错位置。
func geminiNoopBodies() []struct{ name, body string } {
	const fit = `{"type":"object","properties":{"n":{"anyOf":[{"type":"string"},{"type":"null"}]},` +
		`"u":{"oneOf":[{"type":"string"},{"type":"number"}]},"a":{"type":"array","items":{"type":"string"}}}}`
	// 数据值（default、examples、enum、const）里放着形如 schema 的对象，真正的 schema 都已完整。
	const dataOnly = `{"type":"object","properties":{"cfg":{"type":"object","properties":{"o":{"type":"string"}},"default":{"type":"object"},` +
		`"examples":[{"type":"array","enum":["a"]},{"properties":{"k":{}}}]},` +
		`"e":{"type":"string","enum":[{"type":"object"}],"const":{"type":"array","enum":["a"]}}}}`
	// 工具之外的位置放着同样需要改写的形状：response_format 的 schema、请求体顶层的 parameters/input_schema/function、
	// 嵌套在 metadata 里的 tools；顶层 tools 本身是完整的。
	const outside = `{"model":"gemini-3.8-flash","response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"properties":{"k":{"type":"object"}}}}},` +
		`"parameters":{"type":"object"},"input_schema":{"properties":{"k":{}}},"function":{"parameters":{"type":"object"}},` +
		`"metadata":{"tools":[{"parameters":{"type":"object"},"input_schema":{"properties":{"k":{}}}}]},` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}]}`
	return []struct{ name, body string }{
		{"no-tools-key", `{"model":"gemini-3.8-flash","messages":[]}`},
		{"tools-null", `{"model":"gemini-3.8-flash","tools":null}`},
		{"tools-object", `{"model":"gemini-3.8-flash","tools":{"parameters":{"type":"object"}}}`},
		{"tools-string", `{"model":"gemini-3.8-flash","tools":"parameters"}`},
		{"tools-empty-array", `{"model":"gemini-3.8-flash","tools":[]}`},
		{"tools-without-schemas", geminiChatBody("gemini-3.8-flash", `[{"type":"web_search"},{"type":"function","function":{"name":"f"}},{"name":"g"}]`)},
		{"tool-elements-are-not-objects", geminiChatBody("gemini-3.8-flash", `[null,5,"x",[],true]`)},
		{"schemas-are-not-objects", geminiChatBody("gemini-3.8-flash", `[{"type":"function","function":{"name":"f","parameters":"str"}},`+
			`{"name":"g","input_schema":null},{"type":"function","name":"h","parameters":[]},{"name":"i","input_schema":5}]`)},
		{"fit-schema", geminiOneToolBody(fit)},
		{"fit-schema-in-spaced-json", "{ \"model\" : \"gemini-3.8-flash\" ,\n \"temperature\" : 1.0 ,\n \"tools\" : [ { \"type\" : \"function\" ,\n \"function\" : " +
			"{ \"name\" : \"a\" , \"parameters\" : { \"type\" : \"object\" , \"properties\" : { \"x\" : { \"type\" : \"string\" , \"description\" : \"<b> & c\" } } , \"n\" : 2.50 } } } ] }"},
		{"data-values-are-not-schemas", geminiOneToolBody(dataOnly)},
		{"shapes-outside-top-level-tools", outside},
		{"leading-whitespace", "\n  " + geminiOneToolBody(fit) + "\n"},
	}
}

// TestGeminiToolTransformsReturnOriginalWhenNothingToChange 没有可改的内容时，三个函数都返回传入的原切片（逐字节一致、同一底层数组，
// 不修改输入）：没有 tools、tools 不是数组、工具没有 schema、schema 不是对象、schema 已是 Gemini 接受的、形状只出现在数据值或 tools 之外。
func TestGeminiToolTransformsReturnOriginalWhenNothingToChange(t *testing.T) {
	for _, tr := range geminiTransforms {
		for _, tc := range geminiNoopBodies() {
			t.Run(tr.name+"/"+tc.name, func(t *testing.T) {
				assertUnchangedSlice(t, tr.fn, tc.body)
			})
		}
	}
}

// TestGeminiToolTransformsLeaveInvalidJSON 请求体不是 JSON 对象、或 JSON 非法（截断、尾逗号、不带引号的键、单引号）时不 panic，返回原切片。
// 其中的 schema 都是「合法时必被改写」的（缺 properties 的对象、缺 type 的节点），所以返回原切片说明没有在非法输入上做改写。
func TestGeminiToolTransformsLeaveInvalidJSON(t *testing.T) {
	const tool = `{"type":"function","function":{"name":"t","parameters":{"type":"object","properties":{"k":{"type":"object"},"u":{}}}}}`
	for _, tc := range []struct{ name, body string }{
		{"empty", ``},
		{"not-json", `not json "tools"`},
		{"json-null", `null`},
		{"json-number", `123`},
		{"json-string", `"tools"`},
		{"json-array", `[` + tool + `]`},
		{"tools-array-truncated", `{"tools":[` + tool},
		{"body-truncated-inside-tools", `{"tools":[{"type":"function","function":{"parameters":{"type":"object","properties":{"k":{"type":"object"}`},
		{"trailing-comma-in-tools", `{"tools":[` + tool + `,]}`},
		{"trailing-comma-in-schema", `{"tools":[{"type":"function","function":{"parameters":{"type":"object","properties":{"u":{}},}}}]}`},
		{"unquoted-key", `{"tools":[{"type":"function","function":{parameters:{"type":"object","properties":{"u":{}}}}}]}`},
		{"single-quoted-key", `{'tools':[` + tool + `]}`},
	} {
		for _, tr := range geminiTransforms {
			t.Run(tr.name+"/"+tc.name, func(t *testing.T) {
				assertUnchangedSlice(t, tr.fn, tc.body)
			})
		}
	}
}

// TestGeminiToolTransformsOnlyReplaceTools 有改动时只替换 tools 数组的原文：tools 之前与之后的字节（含键与冒号间的空白、
// 数字原文 1.0、1e3、2.50、HTML 字符、结尾换行）逐字节不变；tools 里的数字原文保留，HTML 字符不转义（<>& 不变成 <）。
func TestGeminiToolTransformsOnlyReplaceTools(t *testing.T) {
	const (
		prefix = `{ "model" : "gemini-3.8-flash" , "temperature" : 1.0 , "note" : "<b>&</b>" , "response_format" : { "type" : "json_schema" } , "tools" : `
		suffix = ` , "top_p" : 1e3 , "metadata" : { "k" : 2.50 } }` + "\n"
	)
	const (
		// shapes 的 schema：o 是没有 properties 的对象，要补 properties:{}
		shapesSchema = `{"type":"object","properties":{"o":{"type":"object","minimum":1.0,"maximum":1e3,"default":2.50,"description":"<b> & c"},` +
			`"big":{"type":"number","maximum":12345678901234567890}}}`
		// types 的 schema：根、o、big 都没有 type，要补 type
		typesSchema = `{"properties":{"o":{"minimum":1.0,"maximum":1e3,"default":2.50,"description":"<b> & c"},"big":{"maximum":12345678901234567890}}}`
	)
	for _, tc := range []struct {
		name   string
		fn     func([]byte) []byte
		schema string
	}{
		{"geminiToolShapes", geminiToolShapes, shapesSchema},
		{"geminiToolTypes", geminiToolTypes, typesSchema},
		{"pipeline", geminiPipeline, shapesSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := `[ ` + geminiFn("t", tc.schema) + ` ]`
			in := prefix + tools + suffix
			out := runGeminiTransform(t, tc.fn, in)
			if string(out) == in {
				t.Fatal("应有改动")
			}
			assertAround(t, out, prefix, suffix)
			mid := string(out[len(prefix) : len(out)-len(suffix)])
			for _, want := range []string{`"minimum":1.0`, `"maximum":1e3`, `"default":2.50`, `"maximum":12345678901234567890`, `"description":"<b> & c"`} {
				if !strings.Contains(mid, want) {
					t.Errorf("tools 里应保留 %s: %s", want, mid)
				}
			}
			for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
				if strings.Contains(mid, escaped) {
					t.Errorf("不应转义 HTML 字符（出现 %s）: %s", escaped, mid)
				}
			}
		})
	}
}

// TestGeminiToolTransformsToolLocations 只处理 tools[*].function.parameters、tools[*].parameters、tools[*].input_schema 三处：
// 每处的 schema 被改写；工具里别处（extra、metadata.function.parameters 等）同样形状的对象原样保留。
func TestGeminiToolTransformsToolLocations(t *testing.T) {
	for _, tc := range []struct {
		name         string
		fn           func([]byte) []byte
		schema, want string
	}{
		{"geminiToolShapes", geminiToolShapes, `{"type":"object"}`, `{"type":"object","properties":{}}`},
		{"geminiToolTypes", geminiToolTypes, `{"properties":{"k":{}}}`, `{"type":"object","properties":{"k":{"type":"string"}}}`},
		{"pipeline", geminiPipeline, `{"type":"object"}`, `{"type":"object","properties":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.schema
			tools := `[` +
				`{"type":"function","function":{"name":"f1","parameters":` + s + `},"extra":{"parameters":` + s + `}},` +
				`{"name":"f2","input_schema":` + s + `,"extra":{"input_schema":` + s + `}},` +
				`{"type":"function","name":"f3","parameters":` + s + `,"metadata":{"function":{"parameters":` + s + `}}}]`
			body := geminiChatBody("gemini-3.8-flash", tools)
			out := runGeminiTransform(t, tc.fn, body)
			assertChatToolsOnlyChanged(t, []byte(body), out)
			assertJSONAt(t, out, "tools.0.function.parameters", tc.want)
			assertJSONAt(t, out, "tools.1.input_schema", tc.want)
			assertJSONAt(t, out, "tools.2.parameters", tc.want)
			assertJSONAt(t, out, "tools.0.extra.parameters", s)
			assertJSONAt(t, out, "tools.1.extra.input_schema", s)
			assertJSONAt(t, out, "tools.2.metadata.function.parameters", s)
		})
	}
}

// TestGeminiToolTransformsOddValuesDoNotPanic schema 里各关键字的值类型不对（properties 是数组、items 是数字、anyOf 是对象、
// enum 是字符串、$ref 是数字、引用成环等）、tools 里混着非对象时，三个函数都不 panic，输出仍是合法 JSON；schema 本身不是对象时返回原切片。
// 这些畸形输入的具体改写结果不做约定。
func TestGeminiToolTransformsOddValuesDoNotPanic(t *testing.T) {
	notObjects := []string{`"str"`, `[]`, `null`, `true`, `5`}
	odd := []string{
		`{"properties":[]}`, `{"properties":"x","required":["a"]}`, `{"type":"object","properties":null}`, `{"type":"object","properties":{"a":null}}`,
		`{"items":5}`, `{"items":[1,"a",null]}`, `{"type":"array","items":[]}`, `{"prefixItems":{}}`, `{"prefixItems":[1,null]}`,
		`{"anyOf":{}}`, `{"anyOf":[]}`, `{"anyOf":[1,null,"x",[]]}`, `{"oneOf":"x"}`, `{"oneOf":[]}`, `{"oneOf":[1]}`, `{"anyOf":[{"$ref":5}]}`,
		`{"type":"array","enum":"x"}`, `{"type":"array","enum":[1,2]}`, `{"type":"array","enum":[]}`, `{"type":["array","null"],"enum":["a"]}`, `{"type":null}`,
		`{"$ref":"#"}`, `{"properties":{"n":{"anyOf":[{"$ref":"#"},{"type":"null"}]}}}`,
		`{"$defs":{"a":{"anyOf":[{"$ref":"#/$defs/a"},{"type":"null"}]}},"properties":{"x":{"anyOf":[{"$ref":"#/$defs/a"},{"type":"null"}]}}}`,
		`{"required":null}`, `{"properties":{"a":true,"b":false},"items":true}`, `{"x-a":1,"properties":{"x-b":{"x-c":2}}}`,
	}
	for _, tr := range geminiTransforms {
		for _, schema := range notObjects {
			t.Run(tr.name+"/not-an-object/"+schema, func(t *testing.T) {
				assertUnchangedSlice(t, tr.fn, geminiOneToolBody(schema))
			})
		}
		for _, schema := range odd {
			t.Run(tr.name+"/"+schema, func(t *testing.T) {
				out := runGeminiTransform(t, tr.fn, geminiOneToolBody(schema))
				if !json.Valid(out) {
					t.Fatalf("输出不是合法 JSON: %s", out)
				}
			})
		}
		t.Run(tr.name+"/odd-tool-elements", func(t *testing.T) {
			tools := `[null,5,"x",[],{},{"function":null},{"function":"x"},{"function":[]},{"parameters":5},{"input_schema":[]},` +
				`{"function":{"parameters":{"type":"object"}}}]`
			out := runGeminiTransform(t, tr.fn, geminiChatBody("gemini-3.8-flash", tools))
			if !json.Valid(out) {
				t.Fatalf("输出不是合法 JSON: %s", out)
			}
		})
	}
}

// ---- 第二轮：独立 review 后补的用例 ----

// TestGeminiToolShapesDerefBudgetBoundsOutput 一个较大的 $defs.D 被很多个 anyOf:[{"$ref":D},{"$ref":D}] 引用时，每个联合解析时
// 都内联一份 D 的拷贝，输出随联合个数线性放大成「联合数 × D」；预算把总的内联次数限制在 derefBudget（偏离上游，上游没有预算）：
//   - 输出大小有界：小于 输入长度 + derefBudget × len(D) × 2；
//   - 预算内的内联照常生效：第一个联合（按键排序，处理顺序是确定的）被合并成 D 那样的对象；
//   - 预算用尽后的联合，$ref 分支按无法解析处理：整个联合被去掉，节点成为 {}（之后由 geminiToolTypes 补 type）。
//
// 这里每个联合要内联两次（两个分支），所以至多 derefBudget/2 个联合被合并。
func TestGeminiToolShapesDerefBudgetBoundsOutput(t *testing.T) {
	var d strings.Builder
	d.WriteString(`{"type":"object","properties":{`)
	for i := 0; i < 40; i++ {
		if i > 0 {
			d.WriteByte(',')
		}
		fmt.Fprintf(&d, `"p%02d":{"type":"string","description":"property %d of the shared definition D"}`, i, i)
	}
	d.WriteString(`}}`)
	def := d.String()

	const unions = 1200 // 不设预算时输出约 unions × len(D)，远超下面的上限
	var props strings.Builder
	for i := 0; i < unions; i++ {
		if i > 0 {
			props.WriteByte(',')
		}
		fmt.Fprintf(&props, `"u%04d":{"anyOf":[{"$ref":"#/$defs/D"},{"$ref":"#/$defs/D"}]}`, i)
	}
	body := geminiOneToolBody(`{"type":"object","$defs":{"D":` + def + `},"properties":{` + props.String() + `}}`)

	out := runGeminiTransform(t, geminiToolShapes, body)
	if limit := len(body) + derefBudget*len(def)*2; len(out) >= limit {
		t.Errorf("输出 %d 字节，应小于 %d（输入 %d 字节 + 预算 %d × len(D)=%d × 2）：内联没有预算，输出随联合个数放大",
			len(out), limit, len(body), derefBudget, len(def))
	}

	doc := decodeJSONValue(t, out)
	got, ok := lookupJSON(doc, "tools.0.function.parameters.properties")
	if !ok {
		t.Fatalf("输出没有 properties: %.200s", out)
	}
	props2, _ := got.(map[string]any)
	want := decodeJSONValue(t, []byte(def))
	merged, dropped := 0, 0
	for name, v := range props2 {
		switch m, _ := v.(map[string]any); {
		case reflect.DeepEqual(v, want):
			merged++
		case len(m) == 0:
			dropped++
		default:
			t.Errorf("%s 既不是合并后的 D 也不是被去掉联合的 {}: %.200v", name, v)
		}
	}
	if !reflect.DeepEqual(props2["u0000"], want) {
		t.Errorf("预算内的第一个联合应被合并成 D 那样的对象: %.300v", props2["u0000"])
	}
	if m, _ := props2["u1199"].(map[string]any); len(m) != 0 {
		t.Errorf("预算用尽之后的联合（$ref 无法解析）应被去掉、节点成为 {}: %.300v", props2["u1199"])
	}
	if merged < 1 || merged*2 > derefBudget || merged+dropped != unions {
		t.Errorf("被合并的联合应在 [1, %d] 个之间，其余被去掉：合并 %d，去掉 %d，共 %d", derefBudget/2, merged, dropped, unions)
	}
}

// TestGeminiToolShapesJoinEnumsWithArrayTypes 两个联合分支里同名属性的 type 都写成数组（["string","null"]）时不 panic
// （上游用 != 比较两个 any，同为切片会 panic）：type 相同则 enum 取并集，type 不同则取第一个分支的属性。
func TestGeminiToolShapesJoinEnumsWithArrayTypes(t *testing.T) {
	branch := func(typ, enum string) string {
		return `{"type":"object","properties":{"k":{"type":` + typ + `,"enum":[` + enum + `]}}}`
	}
	for _, tc := range []struct{ name, firstType, secondType, secondEnum, want string }{
		{"same-array-types", `["string","null"]`, `["string","null"]`, `"b"`, `{"type":["string","null"],"enum":["a","b"]}`},
		{"different-array-types", `["string","null"]`, `["integer","null"]`, `"b"`, `{"type":["string","null"],"enum":["a"]}`},
		{"array-and-string-types", `["string","null"]`, `"string"`, `"b"`, `{"type":["string","null"],"enum":["a"]}`},
		{"string-and-array-types", `"string"`, `["string","null"]`, `"b"`, `{"type":"string","enum":["a"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiOneToolBody(`{"type":"object","properties":{"u":{"anyOf":[` +
				branch(tc.firstType, `"a"`) + `,` + branch(tc.secondType, tc.secondEnum) + `]}}}`)
			for _, fn := range []func([]byte) []byte{geminiToolShapes, geminiPipeline} {
				out := runGeminiTransform(t, fn, body)
				assertToolSchema(t, out, `{"type":"object","properties":{"u":{"type":"object","properties":{"k":`+tc.want+`}}}}`)
			}
		})
	}
}

// TestGeminiToolShapesResolvesDefinitionsRefs "#/definitions/X" 的 $ref 分支同 "#/$defs/X" 一样先解析出它指向的 schema 再判断：
// 两个对象分支合并成一个对象，而不是因为「看不出类型」被丢掉。definitions 本身原样保留。
func TestGeminiToolShapesResolvesDefinitionsRefs(t *testing.T) {
	const defs = `{"ById":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]},` +
		`"ByPath":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`
	body := geminiOneToolBody(`{"type":"object","definitions":` + defs + `,` +
		`"properties":{"target":{"anyOf":[{"$ref":"#/definitions/ById"},{"$ref":"#/definitions/ByPath"}]}},"required":["target"]}`)
	for _, fn := range []func([]byte) []byte{geminiToolShapes, geminiPipeline} {
		out := runGeminiTransform(t, fn, body)
		assertToolSchema(t, out, `{"type":"object","definitions":`+defs+`,`+
			`"properties":{"target":{"type":"object","properties":{"id":{"type":"string"},"path":{"type":"string"}}}},"required":["target"]}`)
	}
}

// TestGeminiToolShapesRootUnionIsFlattened 根上没有 type 的联合（oneOf，或没写 type 的 anyOf）改成一个 object：
// properties 是各对象分支 properties 的并集（同名取第一个分支的），根自己的 properties 优先于分支的同名属性；
// 根自己的 required 与各分支的 required 都不保留（实际用的可能是另一个分支）；根上的其他字段（description）保留。
func TestGeminiToolShapesRootUnionIsFlattened(t *testing.T) {
	const branches = `[{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}},"required":["a"]},` +
		`{"type":"object","properties":{"a":{"type":"integer"},"c":{"type":"boolean"}},"required":["c"]}]`
	const want = `{"description":"root","type":"object","properties":{"a":{"type":"string"},"b":{"type":"boolean"},"c":{"type":"boolean"}}}`
	for _, key := range []string{"oneOf", "anyOf"} {
		t.Run(key, func(t *testing.T) {
			body := geminiOneToolBody(`{"description":"root","` + key + `":` + branches + `,"properties":{"b":{"type":"boolean"}},"required":["b"]}`)
			for _, fn := range []func([]byte) []byte{geminiToolShapes, geminiPipeline} {
				assertToolSchema(t, runGeminiTransform(t, fn, body), want)
			}
		})
	}
}

// TestGeminiToolShapesRootWithTypeMergesInsteadOfFlattening 根上写了 type 的 anyOf 走合并而不是折叠：只保留各分支共有的 required，
// 各分支一致的其他字段（additionalProperties）保留；折叠会把这些都丢掉。
func TestGeminiToolShapesRootWithTypeMergesInsteadOfFlattening(t *testing.T) {
	body := geminiOneToolBody(`{"type":"object","anyOf":[` +
		`{"type":"object","properties":{"id":{"type":"string"},"a":{"type":"string"}},"required":["id"],"additionalProperties":false},` +
		`{"type":"object","properties":{"id":{"type":"string"},"b":{"type":"string"}},"required":["id"],"additionalProperties":false}]}`)
	const want = `{"type":"object","properties":{"id":{"type":"string"},"a":{"type":"string"},"b":{"type":"string"}},"required":["id"],"additionalProperties":false}`
	for _, fn := range []func([]byte) []byte{geminiToolShapes, geminiPipeline} {
		assertToolSchema(t, runGeminiTransform(t, fn, body), want)
	}
}

// TestGeminiToolShapesMergeDropsEnumWhenABranchHasNone 合并同类型联合时，有分支没有 enum 就是接受任何值，合并结果去掉 enum：
// 同名属性一边有 enum 一边没有，以及分支本身是标量，顺序反过来结果相同。
func TestGeminiToolShapesMergeDropsEnumWhenABranchHasNone(t *testing.T) {
	const withEnum, without = `{"type":"string","enum":["a"]}`, `{"type":"string"}`
	object := func(k string) string { return `{"type":"object","properties":{"k":` + k + `}}` }
	for _, tc := range []struct{ name, first, second, want string }{
		{"property-enum-then-none", object(withEnum), object(without), `{"type":"object","properties":{"k":{"type":"string"}}}`},
		{"property-none-then-enum", object(without), object(withEnum), `{"type":"object","properties":{"k":{"type":"string"}}}`},
		{"scalar-enum-then-none", withEnum, without, `{"type":"string"}`},
		{"scalar-none-then-enum", without, withEnum, `{"type":"string"}`},
		{"scalar-both-enums-are-joined", withEnum, `{"type":"string","enum":["b"]}`, `{"type":"string","enum":["a","b"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiOneToolBody(`{"type":"object","properties":{"v":{"anyOf":[` + tc.first + `,` + tc.second + `]}}}`)
			out := runGeminiTransform(t, geminiToolShapes, body)
			assertToolSchema(t, out, `{"type":"object","properties":{"v":`+tc.want+`}}`)
		})
	}
}

// TestGeminiToolShapesDropsNonStringArrayEnum 数组上的 enum 只有全是字符串、且 items 能接住时才挪到 items；
// 其余情况（含非字符串的值、items 已有 enum、items 的类型不是 string）丢掉，不移到 items。
func TestGeminiToolShapesDropsNonStringArrayEnum(t *testing.T) {
	for _, tc := range []struct{ name, schema, want string }{
		{"numbers", `{"type":"array","enum":[1,2],"items":{"type":"integer"}}`, `{"type":"array","items":{"type":"integer"}}`},
		{"mixed", `{"type":"array","enum":["a",1],"items":{"type":"string"}}`, `{"type":"array","items":{"type":"string"}}`},
		{"numbers-and-no-items", `{"type":"array","enum":[1,2]}`, `{"type":"array","items":{}}`},
		{"strings-but-items-is-integer", `{"type":"array","enum":["a"],"items":{"type":"integer"}}`, `{"type":"array","items":{"type":"integer"}}`},
		{"items-has-its-own-enum", `{"type":"array","enum":["a"],"items":{"type":"string","enum":["b"]}}`, `{"type":"array","items":{"type":"string","enum":["b"]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiOneToolBody(`{"type":"object","properties":{"v":` + tc.schema + `}}`)
			out := runGeminiTransform(t, geminiToolShapes, body)
			assertToolSchema(t, out, `{"type":"object","properties":{"v":`+tc.want+`}}`)
		})
	}
}

// ---- 第四轮：derefBudget 的计数口径 ----
//
// 预算只在拷贝真正写进输出时扣减：即 mergeUnion 成功合并、联合被改写成合并结果时，每个写进结果的 $ref 拷贝扣 1。
// 只用来判断类型就丢掉的拷贝（可空的单分支联合 anyOf:[{$ref},{type:null}] 保持原样，类型不一致没有合并的联合保持原样）不扣。
// 否则这样的、本来不会放大输出的联合也会把预算耗光，第 257 个起其 $ref 无法解析，联合被无故删掉。

// geminiNumbered 返回 n 个属性的 JSON 成员文本，名字是 prefix 加三位序号（按键排序即按序号排序），值由 value(i) 生成。
func geminiNumbered(prefix string, n int, value func(i int) string) string {
	members := make([]string, n)
	for i := range members {
		members[i] = fmt.Sprintf(`"%s%03d":%s`, prefix, i, value(i))
	}
	return strings.Join(members, ",")
}

// geminiDef 构造共享定义 D：40 个字符串属性（extra 为真时再多一个 extra 属性，用来写出「合并了 D 的拷贝」的期望值）。
func geminiDef(extra bool) string {
	var d strings.Builder
	d.WriteString(`{"type":"object","properties":{`)
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&d, `"p%02d":{"type":"string","description":"property %d of the shared definition D"},`, i, i)
	}
	if extra {
		d.WriteString(`"extra":{"type":"string"},`)
	}
	return strings.TrimSuffix(d.String(), ",") + `}}`
}

// schemaProperties 返回 doc 中 path 处对象的成员（path 同 lookupJSON）。
func schemaProperties(t *testing.T, doc any, path string) map[string]any {
	t.Helper()
	v, ok := lookupJSON(doc, path)
	m, isMap := v.(map[string]any)
	if !ok || !isMap {
		t.Fatalf("路径 %s 不是对象", path)
	}
	return m
}

// assertKeptRefUnions 断言 props 里 prefix 加序号命名的 n 个属性都还是 anyOf:[分支0, 分支1]：分支的 ref 是 "" 表示 {"type":"null"}，
// 否则是 {"$ref":ref}（管线里补过 type 的 $ref 分支也算，只看 $ref 的值）。
func assertKeptRefUnions(t *testing.T, props map[string]any, prefix string, n int, ref0, ref1 string) {
	t.Helper()
	bad := 0
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s%03d", prefix, i)
		node, _ := props[name].(map[string]any)
		branches, _ := node["anyOf"].([]any)
		ok := len(branches) == 2
		for j, ref := range []string{ref0, ref1} {
			if !ok {
				break
			}
			b, _ := branches[j].(map[string]any)
			if ref == "" {
				ok = b["type"] == "null"
			} else {
				ok = b["$ref"] == ref
			}
		}
		if !ok {
			if bad++; bad <= 3 {
				t.Errorf("%s 应保持 anyOf:[$ref %q, %q]，实际是 %.200v", name, ref0, ref1, props[name])
			}
		}
	}
	if bad > 3 {
		t.Errorf("共 %d 个（共检查 %s000 起 %d 个）联合没有保持原样", bad, prefix, n)
	}
}

// TestGeminiToolShapesNullableRefUnionsAreNotBudgeted 同一个工具 schema 里有 300 个 anyOf:[{"$ref":S},{"type":"null"}]（S 是普通对象）：
// 可空的单分支联合与上游一样原样保留，$ref 拷贝只用来判断类型，没有写进输出，不扣预算；所以 300 个全部保持，
// 不能在第 257 个起因预算用尽被删。单步 geminiToolShapes 没有任何改动，返回原切片；管线只给 $ref 分支补 type，联合仍在。
func TestGeminiToolShapesNullableRefUnionsAreNotBudgeted(t *testing.T) {
	const unions = 300
	props := geminiNumbered("u", unions, func(int) string { return `{"anyOf":[{"$ref":"#/$defs/S"},{"type":"null"}]}` })
	body := geminiOneToolBody(`{"type":"object","$defs":{"S":{"type":"object","properties":{"k":{"type":"string"}}}},"properties":{` + props + `}}`)
	t.Run("geminiToolShapes", func(t *testing.T) {
		assertUnchangedSlice(t, geminiToolShapes, body)
	})
	t.Run("pipeline", func(t *testing.T) {
		out := runGeminiTransform(t, geminiPipeline, body)
		assertKeptRefUnions(t, schemaProperties(t, decodeJSONValue(t, out), "tools.0.function.parameters.properties"), "u", unions, "#/$defs/S", "")
	})
}

// TestGeminiToolShapesUnmergedRefUnionsAreNotBudgeted 先有 200 个类型不一致的 anyOf:[{"$ref":S},{"$ref":I}]（S 是对象，I 是 integer，
// 不合并、保持原样，两个拷贝都没写进输出），其后一个可空 $ref 联合也应保持：前面的联合没有耗预算。
func TestGeminiToolShapesUnmergedRefUnionsAreNotBudgeted(t *testing.T) {
	const mixed = 200
	props := geminiNumbered("m", mixed, func(int) string { return `{"anyOf":[{"$ref":"#/$defs/S"},{"$ref":"#/$defs/I"}]}` }) + "," +
		geminiNumbered("n", 1, func(int) string { return `{"anyOf":[{"$ref":"#/$defs/S"},{"type":"null"}]}` })
	body := geminiOneToolBody(`{"type":"object","$defs":{"S":{"type":"object","properties":{"k":{"type":"string"}}},"I":{"type":"integer"}},"properties":{` + props + `}}`)
	t.Run("geminiToolShapes", func(t *testing.T) {
		assertUnchangedSlice(t, geminiToolShapes, body)
	})
	t.Run("pipeline", func(t *testing.T) {
		out := runGeminiTransform(t, geminiPipeline, body)
		got := schemaProperties(t, decodeJSONValue(t, out), "tools.0.function.parameters.properties")
		assertKeptRefUnions(t, got, "m", mixed, "#/$defs/S", "#/$defs/I")
		assertKeptRefUnions(t, got, "n", 1, "#/$defs/S", "")
	})
}

// classifyUnions 把 props 里的属性分成：等于 want 的（合并了 $ref 拷贝）、空对象（联合因 $ref 无法解析被去掉）、其他（返回名字）。
func classifyUnions(props map[string]any, want any) (merged, dropped int, others []string) {
	for name, v := range props {
		switch m, _ := v.(map[string]any); {
		case reflect.DeepEqual(v, want):
			merged++
		case len(m) == 0:
			dropped++
		default:
			others = append(others, name)
		}
	}
	return merged, dropped, others
}

// TestGeminiToolShapesDerefBudgetIsPerSchema 预算按每处工具 schema 独立：两个工具（以及同一个工具的 function.parameters 与 input_schema）
// 各有 derefBudget/2 个可合并的 anyOf:[{"$ref":D},{"$ref":D}]（每个写进输出两个拷贝，正好用满各自的预算），全部合并，
// 后一个 schema 不因前一个用掉了预算而被去掉联合。
func TestGeminiToolShapesDerefBudgetIsPerSchema(t *testing.T) {
	const perSchema = derefBudget / 2
	def := geminiDef(false)
	schema := func() string {
		props := geminiNumbered("u", perSchema, func(int) string { return `{"anyOf":[{"$ref":"#/$defs/D"},{"$ref":"#/$defs/D"}]}` })
		return `{"type":"object","$defs":{"D":` + def + `},"properties":{` + props + `}}`
	}
	want := decodeJSONValue(t, []byte(def))
	for _, tc := range []struct {
		name  string
		tools string
		paths []string
	}{
		{"two-tools", `[` + geminiFn("t1", schema()) + `,` + geminiFn("t2", schema()) + `]`,
			[]string{"tools.0.function.parameters.properties", "tools.1.function.parameters.properties"}},
		{"two-slots-of-one-tool", `[{"type":"function","function":{"name":"t","parameters":` + schema() + `},"input_schema":` + schema() + `}]`,
			[]string{"tools.0.function.parameters.properties", "tools.0.input_schema.properties"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runGeminiTransform(t, geminiToolShapes, geminiChatBody("gemini-3.8-flash", tc.tools))
			doc := decodeJSONValue(t, out)
			for _, path := range tc.paths {
				merged, dropped, others := classifyUnions(schemaProperties(t, doc, path), want)
				if merged != perSchema || dropped != 0 || len(others) != 0 {
					t.Errorf("%s：应全部 %d 个联合都被合并，实际合并 %d、被去掉 %d、其他 %v", path, perSchema, merged, dropped, others)
				}
			}
		})
	}
}

// TestGeminiToolShapesDerefBudgetBoundary 预算的边界，单位是「写进输出的拷贝数」：
//   - 每个可合并的 anyOf:[{$ref:D},{$ref:D}] 写进两个拷贝，derefBudget/2 个正好用满，全部合并；再多一个，该联合没有预算可用，
//     $ref 按无法解析处理，联合被去掉，节点成为 {}；
//   - 每个 anyOf:[{$ref:D},{内联的对象}] 只写进一个拷贝，derefBudget 个正好用满；再多一个同样被去掉。
func TestGeminiToolShapesDerefBudgetBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		union  string // 一个联合
		extra  bool   // 合并结果带 extra 属性（内联分支带来的）
		copies int    // 每个联合写进输出的拷贝数
	}{
		{"two-copies-per-union", `{"anyOf":[{"$ref":"#/$defs/D"},{"$ref":"#/$defs/D"}]}`, false, 2},
		{"one-copy-per-union", `{"anyOf":[{"$ref":"#/$defs/D"},{"type":"object","properties":{"extra":{"type":"string"}}}]}`, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fit := derefBudget / tc.copies // 预算正好够的联合个数
			want := decodeJSONValue(t, []byte(geminiDef(tc.extra)))
			for _, n := range []int{fit, fit + 1} {
				props := geminiNumbered("u", n, func(int) string { return tc.union })
				body := geminiOneToolBody(`{"type":"object","$defs":{"D":` + geminiDef(false) + `},"properties":{` + props + `}}`)
				out := runGeminiTransform(t, geminiToolShapes, body)
				got := schemaProperties(t, decodeJSONValue(t, out), "tools.0.function.parameters.properties")
				merged, dropped, others := classifyUnions(got, want)
				wantDropped := n - fit
				if merged != fit || dropped != wantDropped || len(others) != 0 {
					t.Errorf("%d 个联合：应合并 %d、去掉 %d，实际合并 %d、去掉 %d、其他 %v", n, fit, wantDropped, merged, dropped, others)
				}
				if n > fit {
					// 被去掉的是排在最后的那个（按键排序处理，顺序是确定的）
					if last, _ := got[fmt.Sprintf("u%03d", n-1)].(map[string]any); len(last) != 0 {
						t.Errorf("最后一个联合应被去掉、节点成为 {}: %.200v", got[fmt.Sprintf("u%03d", n-1)])
					}
				}
			}
		})
	}
}

// ---- 第五轮：$ref 解析结果的缓存 ----
//
// shapeForGemini 按 $ref 缓存目标的序列化结果（避免对大 $defs 项重复序列化），$defs、definitions 的每一项处理完缓存作废（版本号加一），
// 因为目标可能已被改写。遍历顺序是：properties、patternProperties 先于 $defs、definitions，映射内按键排序。

// TestGeminiToolShapesRefCacheIsInvalidatedWhenDefsChange 缓存失效的回归：
//   - properties 阶段，联合先引用 #/$defs/A，此时 A 里还有 $ref 分支，解析不了（目标里有引用不展开），缓存下「解析不了」；
//   - 之后 $defs 阶段处理 A：它的 anyOf 合并成一个对象，不再含 $ref；
//   - 排在 A 之后的 $defs.B 里的联合引用 A，应能成功合并——结果与不缓存时一致；沿用 properties 阶段缓存的「解析不了」则 B 的联合会被误删。
//
// properties 里的那个联合当时确实解析不了，与上游一致被去掉（节点成为 {}）。三种写法：$defs、definitions，
// 以及 A 在 $defs、B 在 definitions（definitions 在 $defs 之后处理）。
func TestGeminiToolShapesRefCacheIsInvalidatedWhenDefsChange(t *testing.T) {
	const x = `{"type":"object","properties":{"k":{"type":"string"}}}`
	for _, tc := range []struct {
		name             string
		aKey, bKey, refA string // A（与 X）所在的映射、B 所在的映射、指向 A 的 $ref
	}{
		{"defs", "$defs", "$defs", "#/$defs/A"},
		{"definitions", "definitions", "definitions", "#/definitions/A"},
		{"defs-then-definitions", "$defs", "definitions", "#/$defs/A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A、X 在 aKey 映射里，B 在 bKey 映射里；A 与 X 的引用写同一个映射的路径
			refX := "#/" + tc.aKey + "/X"
			a := `{"anyOf":[{"$ref":"` + refX + `"},{"$ref":"` + refX + `"}]}`
			b := `{"anyOf":[{"$ref":"` + tc.refA + `"},{"$ref":"` + tc.refA + `"}]}`
			var defs string
			if tc.aKey == tc.bKey {
				defs = `"` + tc.aKey + `":{"A":` + a + `,"B":` + b + `,"X":` + x + `}`
			} else {
				defs = `"` + tc.aKey + `":{"A":` + a + `,"X":` + x + `},"` + tc.bKey + `":{"B":` + b + `}`
			}
			body := geminiOneToolBody(`{"type":"object","properties":{"p":{"anyOf":[{"$ref":"` + tc.refA + `"},{"type":"null"}]}},` + defs + `}`)
			out := runGeminiTransform(t, geminiToolShapes, body)

			var wantDefs string
			if tc.aKey == tc.bKey {
				wantDefs = `"` + tc.aKey + `":{"A":` + x + `,"B":` + x + `,"X":` + x + `}`
			} else {
				wantDefs = `"` + tc.aKey + `":{"A":` + x + `,"X":` + x + `},"` + tc.bKey + `":{"B":` + x + `}`
			}
			// p 当时解析不了 A（A 里还有 $ref），联合被去掉；B 在 A 合并之后引用它，被合并成 A 的样子
			assertToolSchema(t, out, `{"type":"object","properties":{"p":{}},`+wantDefs+`}`)
		})
	}
}

// TestGeminiToolShapesRefCopiesAreIndependent 同一个 schema 里多次引用同一个成功解析的目标，每次得到的拷贝互相独立：
// 各联合的合并结果只含自己的内容，不受其他联合的合并影响（属性并上各自的 extra，enum 各自并或去掉），
// 也不会写回 $defs 里的目标（目标没有 type：合并时给拷贝补的 type 不能漏到原目标上）。
// 目标在 properties 阶段被多次引用（缓存命中），$defs 阶段处理完目标后 $defs.Z 再引用（缓存已作废，重新解析）。
func TestGeminiToolShapesRefCopiesAreIndependent(t *testing.T) {
	const d = `{"properties":{"kind":{"type":"string","enum":["a"]},"name":{"type":"string"}}}`
	body := geminiOneToolBody(`{"type":"object","properties":{` +
		`"u1":{"anyOf":[{"$ref":"#/$defs/D"},{"type":"object","properties":{"kind":{"type":"string","enum":["b"]},"extra1":{"type":"string"}}}]},` +
		`"u2":{"anyOf":[{"$ref":"#/$defs/D"},{"type":"object","properties":{"kind":{"type":"string"},"extra2":{"type":"string"}}}]},` +
		`"u3":{"anyOf":[{"$ref":"#/$defs/D"},{"$ref":"#/$defs/D"}]},` +
		`"u4":{"anyOf":[{"$ref":"#/$defs/D"},{"type":"null"}]}},` +
		`"$defs":{"D":` + d + `,"Z":{"anyOf":[{"$ref":"#/$defs/D"},{"type":"object","properties":{"extra3":{"type":"string"}}}]}}}`)
	out := runGeminiTransform(t, geminiToolShapes, body)
	assertToolSchema(t, out, `{"type":"object","properties":{`+
		`"u1":{"type":"object","properties":{"kind":{"type":"string","enum":["a","b"]},"name":{"type":"string"},"extra1":{"type":"string"}}},`+
		`"u2":{"type":"object","properties":{"kind":{"type":"string"},"name":{"type":"string"},"extra2":{"type":"string"}}},`+
		`"u3":{"type":"object","properties":{"kind":{"type":"string","enum":["a"]},"name":{"type":"string"}}},`+
		`"u4":{"anyOf":[{"$ref":"#/$defs/D"},{"type":"null"}]}},`+
		`"$defs":{"D":`+d+`,"Z":{"type":"object","properties":{"kind":{"type":"string","enum":["a"]},"name":{"type":"string"},"extra3":{"type":"string"}}}}}`)
}
