package copilot

// 本文件覆盖 geminiToolTypes（见 geminitypes.go）：给工具 schema 里缺 type 的节点补上 type（Vertex 把函数声明的 parameters 当作
// 自己的 Schema 对象，type 必填，缺一处就整个请求 400），并先去掉 Schema 容纳不了的布尔 schema 与 x- 关键字。
// 与 geminiToolShapes 共同遵守的约定（无改动返回原切片、只替换 tools、非法输入不 panic 等）见 geminishapes_test.go 的共同约定测试。
//
// 期望行为（对应 magpie internal/gateway/gemini_schema.go 的 geminiTypes(body, true)，但不检查模型）：
//   - func geminiToolTypes(body []byte) []byte；
//   - 缺 type 的 schema 按其取值（enum、const、default）、关键字（properties 为 object，items 为 array，pattern 为 string 等）
//     或分支（anyOf、oneOf、allOf、本地 $ref）说出的那个类型补上，null 不算；工具的根是 object；什么都没说的（{} 或只有描述）是 string；
//     分支类型不止一种、或类型说不清的 $ref/联合保持原样；not、if 之下不补；
//   - type 就地写在 schema 的左花括号之后，其余字节不动（缓存前缀不变）；没有可补的内容时返回原切片；
//   - 布尔 schema（true 是 {}，false 是「没有值」：作为属性、分支、关键字都去掉）与 x- 关键字先去掉（geminiSchemaShapes）；
//     名字以 x- 开头的属性是名字不是关键字，保留；additionalProperties 的 false 保留；default、examples、enum、const 是数据，不处理；
//   - 偏离上游的两处（上游实机记录 Copilot 对根 {"type":"object"} 无 properties 返回 400；required 里留着已去掉的属性是同类问题）：
//     被补成 "type":"object"、没有 properties、且没有 $ref、allOf、anyOf、oneOf 的节点（裸 object）一并补 "properties":{}
//     （$ref 与联合旁边不偏离上游；已有 type:"object" 的节点由 Shapes 处理，Types 不管）；
//     去掉值为 false 的属性时，同级 required 里该名字也去掉（删空则去掉 required 键）。
//
// 与上游测试的差异（上游 geminiTypes 有 keepUnions 开关，本项目固定为 true）：
//   - keepUnions=true 时，非根的 anyOf/oneOf 节点本身不补 type，其分支照常补（Copilot 的 Gemini 对联合旁写的 type 返回 400，#4374）；
//     根节点即使是联合也补 type（根必须是 object）。上游用例里依赖 keepUnions=false 的期望已按此调整：
//     TestGeminiToolTypes 里 u、num 两个联合节点不再补 type；cua-driver 的 drag 里 target（anyOf）与其第一个分支（oneOf）不再补 object；
//   - 上游的「另一个模型的请求体原样返回」「经 Anthropic/Responses 翻译后的路由」「Antigravity 的 plainSchema」「Kimi」都不适用：
//     本项目的函数不检查模型，三种工具形状（function.parameters、parameters、input_schema）直接在 tools 里测。

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// geminiTypesBody 构造只有 tools 的请求体（精确比较字节用）。
func geminiTypesBody(model, tools string) string {
	return fmt.Sprintf(`{"model":%q,"tools":%s}`, model, tools)
}

// geminiTypesTool 构造 Chat 风格的单工具数组，function.parameters 为 schema。
func geminiTypesTool(schema string) string {
	return `[{"type":"function","function":{"name":"t","parameters":` + schema + `}}]`
}

// cua-driver 的 drag 工具，其 MCP 服务在 cua 的 #4888 之前列出的 schema（kthink 的 dsh 发出的那个）：target 是 anyOf，
// 第一个分支是没有 type 的 oneOf、第二个是 null；scope 只有 const 没有 type。Vertex 对它返回
// 400 "`mcp__cua-driver__drag` functionDeclaration `parameters.target` schema didn't specify the schema type field."
// 描述文字已缩短，结构不变。
const geminiCuaDragSchema = `{"additionalProperties":false,"properties":{"button":{"enum":["left","right","middle"],"type":"string"},` +
	`"duration_ms":{"maximum":10000,"minimum":0,"type":"integer"},"from_x":{"type":"number"},"from_y":{"type":"number"},` +
	`"modifier":{"items":{"type":"string"},"type":"array"},` +
	`"scope":{"const":"desktop","description":"Deprecated flat desktop target retained for wire compatibility."},` +
	`"session":{"description":"A short public session label.","type":"string"},"steps":{"maximum":200,"minimum":1,"type":"integer"},` +
	`"target":{"anyOf":[{"description":"Exact capture/input target selected independently for each action.","oneOf":[` +
	`{"additionalProperties":true,"properties":{"kind":{"const":"window","type":"string"},"pid":{"minimum":0,"type":"integer"},"window_id":{"minimum":0,"type":"integer"}},"required":["kind","pid","window_id"],"type":"object"},` +
	`{"additionalProperties":true,"properties":{"display_id":{"type":"string"},"kind":{"const":"desktop","type":"string"}},"required":["kind","display_id"],"type":"object"}]},` +
	`{"type":"null"}],"description":"Preferred per-call target: an exact window or the primary desktop."},` +
	`"to_x":{"type":"number"},"to_y":{"type":"number"}},"required":["from_x","from_y","to_x","to_y"],"type":"object"}`

// TestGeminiToolTypesCuaDriverDrag 只有 scope（只有 const 的节点）被补上 type: string，其余字节不动（上游 TestGeminiToolTypesCuaDriverDrag）。
// 上游（keepUnions=false）还会给 target 与它的第一个分支补 "type":"object"；本项目固定 keepUnions=true，联合节点保持没有 type，
// 它们的分支（已有 type 的对象）不变。三种工具形状（Chat 的 function.parameters、Anthropic 的 input_schema、Responses 的 parameters）都一样。
func TestGeminiToolTypesCuaDriverDrag(t *testing.T) {
	want := strings.Replace(geminiCuaDragSchema, `"scope":{"const"`, `"scope":{"type":"string","const"`, 1)
	if want == geminiCuaDragSchema {
		t.Fatal("固定样例已不是用例要测的形状")
	}
	for _, tc := range []struct{ name, tools string }{
		{"chat-function-parameters", `[{"type":"function","function":{"name":"mcp__cua-driver__drag","parameters":%s}}]`},
		{"messages-input-schema", `[{"name":"mcp__cua-driver__drag","input_schema":%s}]`},
		{"responses-parameters", `[{"type":"function","name":"mcp__cua-driver__drag","parameters":%s}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiTypesBody("google/gemini-3.8-flash", fmt.Sprintf(tc.tools, geminiCuaDragSchema))
			out := runGeminiTransform(t, geminiToolTypes, body)
			if want := geminiTypesBody("google/gemini-3.8-flash", fmt.Sprintf(tc.tools, want)); string(out) != want {
				t.Fatalf("补 type 的结果不符:\n got=%s\nwant=%s", out, want)
			}
		})
	}
}

// geminiTypesParams 是 TestGeminiToolTypes 的输入：每个属性是一种需要（或不需要）补 type 的写法。
const geminiTypesParams = `{"properties":{` +
	`"any":{},"spaced":{ },"said":{"description":"x"},` +
	`"n":{"enum":[1,2]},"f":{"enum":[1,2.5]},"b":{"const":true},"maybe":{"enum":["a",null]},"mixed":{"enum":["a",1]},` +
	`"list":{"items":{}},"obj":{"properties":{"k":{"type":"string"}}},"pat":{"pattern":"^a"},` +
	`"u":{"anyOf":[{"type":"string"},{"type":"null"}]},"two":{"anyOf":[{"type":"string"},{"type":"integer"}]},` +
	`"num":{"oneOf":[{"type":"integer"},{"type":"number"}]},"r":{"$ref":"#/$defs/D"},"dflt":{"default":3},` +
	`"neg":{"type":"string","not":{"enum":["x"]}},"typed":{"type":"array","items":{"type":"integer"}},` +
	`"un":{"anyOf":[{"enum":["a"]},{"type":"null"}]},"uo":{"oneOf":[{"properties":{"k":{}}},{"type":"null"}]},` +
	`"ua":{"allOf":[{"properties":{"k":{"enum":[1]}}}]}},` +
	`"$defs":{"D":{"properties":{}}}}`

// geminiTypesWant 是 geminiTypesParams 补 type 之后的样子（就地插入：type 写在左花括号之后，{ } 里写成 {"type":"string" }）。
const geminiTypesWant = `{"type":"object","properties":{` +
	`"any":{"type":"string"},"spaced":{"type":"string" },"said":{"type":"string","description":"x"},` +
	`"n":{"type":"integer","enum":[1,2]},"f":{"type":"number","enum":[1,2.5]},"b":{"type":"boolean","const":true},"maybe":{"type":"string","enum":["a",null]},"mixed":{"enum":["a",1]},` +
	`"list":{"type":"array","items":{"type":"string"}},"obj":{"type":"object","properties":{"k":{"type":"string"}}},"pat":{"type":"string","pattern":"^a"},` +
	`"u":{"anyOf":[{"type":"string"},{"type":"null"}]},"two":{"anyOf":[{"type":"string"},{"type":"integer"}]},` +
	`"num":{"oneOf":[{"type":"integer"},{"type":"number"}]},"r":{"type":"object","$ref":"#/$defs/D"},"dflt":{"type":"integer","default":3},` +
	`"neg":{"type":"string","not":{"enum":["x"]}},"typed":{"type":"array","items":{"type":"integer"}},` +
	`"un":{"anyOf":[{"type":"string","enum":["a"]},{"type":"null"}]},"uo":{"oneOf":[{"type":"object","properties":{"k":{"type":"string"}}},{"type":"null"}]},` +
	`"ua":{"type":"object","allOf":[{"type":"object","properties":{"k":{"type":"integer","enum":[1]}}}]}},` +
	`"$defs":{"D":{"type":"object","properties":{}}}}`

// TestGeminiToolTypes 每个缺 type 的 schema 得到其取值、关键字或分支说出的那一个类型（上游 TestGeminiToolTypes）：
//   - 取值：enum/const 的取值类型（整数与小数混在一起是 number，null 不算，字符串与整数混在一起说不清所以不补）；default 也算；
//   - 关键字：properties 为 object，items 为 array，pattern 为 string；
//   - 分支：$ref 取它指向的 schema 的类型；allOf 取各分支的类型；
//   - 什么都没说的（{}、只有描述）是 string，根是 object；已有 type 的、not 之下的不动。
//
// keepUnions=true 的调整：u、two、num（可空或多类型的 anyOf/oneOf 节点）本身不补 type（上游 keepUnions=false 时 u 补 string、num 补 number）；
// un、uo 是新增的行，证明联合节点没有 type 的同时，它们的分支照常补 type。
// r（$ref 指向对象）与 ua（allOf 的分支是对象）被补成 object，但旁边有 $ref 或 allOf，不补 properties:{}（与上游一致，见文件头）。
// 函数本身不检查模型：对非 Gemini 模型 id、没有 model 的请求体同样补 type。
func TestGeminiToolTypes(t *testing.T) {
	for _, model := range []string{"gemini-3.8-flash", "google/gemini-3.8-flash", "vertex_ai/Gemini-2.5-pro", "gpt-5.5", ""} {
		t.Run("model="+model, func(t *testing.T) {
			body := geminiTypesBody(model, geminiTypesTool(geminiTypesParams))
			out := runGeminiTransform(t, geminiToolTypes, body)
			if want := geminiTypesBody(model, geminiTypesTool(geminiTypesWant)); string(out) != want {
				t.Fatalf("补 type 的结果不符:\n got=%s\nwant=%s", out, want)
			}
		})
	}
	t.Run("no-model-key", func(t *testing.T) {
		out := runGeminiTransform(t, geminiToolTypes, `{"tools":`+geminiTypesTool(geminiTypesParams)+`}`)
		if want := `{"tools":` + geminiTypesTool(geminiTypesWant) + `}`; string(out) != want {
			t.Fatalf("补 type 的结果不符:\n got=%s\nwant=%s", out, want)
		}
	})
}

// TestGeminiToolTypesRootUnion 根节点即使是联合也补 type（工具的根必须是 object），只有 keepUnions 跳过的是非根的联合节点；
// 分支类型不止一种、说不清根是什么时保持原样。
func TestGeminiToolTypesRootUnion(t *testing.T) {
	t.Run("branches-of-one-type", func(t *testing.T) {
		out := runGeminiTransform(t, geminiToolTypes, geminiTypesBody("gemini-3.8-flash",
			geminiTypesTool(`{"anyOf":[{"type":"object","properties":{}},{"type":"object","properties":{"a":{}}}]}`)))
		want := geminiTypesBody("gemini-3.8-flash",
			geminiTypesTool(`{"type":"object","anyOf":[{"type":"object","properties":{}},{"type":"object","properties":{"a":{"type":"string"}}}]}`))
		if string(out) != want {
			t.Fatalf("根联合应补 type（不补 properties，见 TestGeminiToolTypesObjectWithRefOrUnionKeepsNoProperties），分支照常补:\n got=%s\nwant=%s", out, want)
		}
	})
	t.Run("branches-of-two-types", func(t *testing.T) {
		assertUnchangedSlice(t, geminiToolTypes, geminiTypesBody("gemini-3.8-flash", geminiTypesTool(`{"anyOf":[{"type":"string"},{"type":"number"}]}`)))
	})
}

// TestGeminiToolTypesLeavesUnknownTypes 类型说不清的节点保持原样：指向不存在或非本地目标的 $ref、字符串与整数混写的 enum、not 之下的节点。
func TestGeminiToolTypesLeavesUnknownTypes(t *testing.T) {
	assertUnchangedSlice(t, geminiToolTypes, geminiTypesBody("gemini-3.8-flash", geminiTypesTool(
		`{"type":"object","properties":{"m":{"$ref":"#/$defs/Missing"},"w":{"$ref":"https://example.com/s.json"},`+
			`"mixed":{"enum":["a",1]},"neg":{"type":"string","not":{"enum":["x"]}}}}`)))
}

// TestGeminiToolTypesKeywordNamesAreNames 属性名恰好叫 default、examples、enum、const、properties、items 时它们是名字，其值是 schema，照常补 type；
// 根上真正的 default、examples 是数据，其中的对象不补 type。
func TestGeminiToolTypesKeywordNamesAreNames(t *testing.T) {
	const in = `{"type":"object","properties":{"default":{},"examples":{},"enum":{"enum":["a"]},"const":{},"properties":{"type":"string"},` +
		`"items":{"description":"d"}},"default":{"k":{}},"examples":[{"k":{}}]}`
	const want = `{"type":"object","properties":{"default":{"type":"string"},"examples":{"type":"string"},"enum":{"type":"string","enum":["a"]},` +
		`"const":{"type":"string"},"properties":{"type":"string"},"items":{"type":"string","description":"d"}},"default":{"k":{}},"examples":[{"k":{}}]}`
	out := runGeminiTransform(t, geminiToolTypes, geminiTypesBody("gemini-3.8-flash", geminiTypesTool(in)))
	if got := geminiTypesBody("gemini-3.8-flash", geminiTypesTool(want)); string(out) != got {
		t.Fatalf("结果不符:\n got=%s\nwant=%s", out, got)
	}
}

// TestGeminiToolTypesLeavesDataValuesAlone default、examples、enum、const 是数据：其中形如 schema 的对象（缺 type 的节点）不补 type，
// 其中的 x- 键与布尔值不去掉；同一 schema 里真正的 schema 照常处理（x- 关键字去掉、缺 type 的补上）。
func TestGeminiToolTypesLeavesDataValuesAlone(t *testing.T) {
	body := geminiOneToolBody(`{"type":"object","properties":{` +
		`"d":{"type":"object","default":{"properties":{"a":{}}},"examples":[{"k":{}},{"x-keep":true,"items":true}]},` +
		`"e":{"type":"string","enum":[{"x-e":true}],"const":{"x-c":{}}},` +
		`"v":{"type":"string","x-drop":1},"u":{}}}`)
	out := runGeminiTransform(t, geminiToolTypes, body)
	assertToolSchema(t, out, `{"type":"object","properties":{`+
		`"d":{"type":"object","default":{"properties":{"a":{}}},"examples":[{"k":{}},{"x-keep":true,"items":true}]},`+
		`"e":{"type":"string","enum":[{"x-e":true}],"const":{"x-c":{}}},`+
		`"v":{"type":"string"},"u":{"type":"string"}}}`)
	assertChatToolsOnlyChanged(t, []byte(body), out)
}

// TestGeminiToolTypesDropsBooleansAndXKeys 工具 schema 里的布尔 schema 与 x- 关键字去掉（上游 TestGeminiToolTypesDropsBooleansAndXKeys）：
// items:true 是 {} 再补 type，值为 false 的属性与分支去掉，名为 x-… 的属性与 additionalProperties 保留；没有这两样的工具原样不动（连多余的空白）。
func TestGeminiToolTypesDropsBooleansAndXKeys(t *testing.T) {
	const plain = `{"type":"function","function":{"name":"plain","parameters":{"type":"object",  "properties":{"a":{"type":"string"}}}}}`
	const head = `{"model":"google/gemini-3.8-flash","tools":[`
	body := head + `{"type":"function","function":{"name":"w","parameters":{"type":"object",
	"properties":{"x-region":{"type":"string","x-mcp-header":"Region"},"rows":{"type":"array","items":{"type":"array","items":true}},
	"gone":false,"u":{"anyOf":[false,{"type":"string"}]}},"additionalProperties":false}}},
	` + plain + `]}`
	out := runGeminiTransform(t, geminiToolTypes, body)
	const base = "tools.0.function.parameters."
	assertJSONAt(t, out, base+"properties.x-region", `{"type":"string"}`)
	assertJSONAt(t, out, base+"additionalProperties", `false`)
	assertJSONAt(t, out, base+"properties.rows", `{"type":"array","items":{"type":"array","items":{"type":"string"}}}`)
	assertJSONAt(t, out, base+"properties.u", `{"anyOf":[{"type":"string"}]}`)
	assertNoJSONAt(t, out, base+"properties.gone")
	raw := string(out)
	for _, left := range []string{"x-mcp-header", "true", `"gone"`} {
		if strings.Contains(raw, left) {
			t.Errorf("不应留下 %s: %s", left, out)
		}
	}
	if !strings.Contains(raw, plain) {
		t.Errorf("没有布尔 schema 与 x- 关键字的工具应原样保留: %s", out)
	}
	if !strings.HasPrefix(raw, head) {
		t.Errorf("tools 之前的字节不应改动: %s", out)
	}
}

// TestGeminiToolTypesBooleansAndXKeysEverywhere JSON Schema 里每个放 schema 的位置都可以写布尔，每一层都可以带 x- 关键字，
// 都不会发给 Vertex；名为 x-… 的属性是名字，保留（改编自上游 TestPlainSchemaBooleansAndXKeysEverywhere，那个测的是 Antigravity 的
// plainSchema，这里测 geminiToolTypes 的同一类处理；plainSchema 还会给 items:false 补 maxItems，本项目没有；
// 它把 false 属性从 required 里去掉的做法，geminiToolTypes 也有，见 TestGeminiToolTypesDropsFalsePropertiesFromRequired）。
func TestGeminiToolTypesBooleansAndXKeysEverywhere(t *testing.T) {
	const in = `{"type":"object","x-root":1,"$defs":{"Any":true,"None":false,"Cell":{"type":"array","items":true,"x-def":"d"}},
	"properties":{
		"any":true,
		"gone":false,
		"x-region":{"type":"string","x-mcp-header":"Region"},
		"grid":{"type":"array","items":{"type":"array","items":{"type":"array","items":true}}},
		"empty":{"type":"array","items":false},
		"ref":{"$ref":"#/$defs/Any"},
		"never":{"$ref":"#/$defs/None"},
		"either":{"anyOf":[true,{"type":"null"}]},
		"first":{"anyOf":[false,{"type":"integer","x-k":1}]},
		"merged":{"allOf":[true,{"type":"object","properties":{"a":true,"b":{"type":"string","x-b":2}}}]},
		"obj":{"type":"object","properties":{"deep":{"type":"array","items":{"type":"object","properties":{"v":true},"x-n":{"a":1}}}}}
	},"required":["any","gone","never","x-region"]}`
	out := runGeminiTransform(t, geminiToolTypes, geminiOneToolBody(in))
	const base = "tools.0.function.parameters."
	for _, keyword := range []string{`"x-root"`, `"x-def"`, `"x-mcp-header"`, `"x-k"`, `"x-b"`, `"x-n"`} {
		if strings.Contains(string(out), keyword) {
			t.Errorf("x- 关键字 %s 应去掉: %s", keyword, out)
		}
	}
	// 值为 false 的属性与 $defs 项去掉，true 是 {}（再补 type）。
	assertNoJSONAt(t, out, base+"properties.gone")
	assertNoJSONAt(t, out, base+"$defs.None")
	assertJSONAt(t, out, base+"$defs.Any", `{"type":"string"}`)
	assertJSONAt(t, out, base+"properties.any", `{"type":"string"}`)
	assertJSONAt(t, out, base+"$defs.Cell", `{"type":"array","items":{"type":"string"}}`)
	// 被去掉的属性（gone）同时从 required 里去掉；其余条目原样保留，包括指向被去掉的 $defs 项的 never（它是 $ref，属性还在）。
	assertJSONAt(t, out, base+"required", `["any","never","x-region"]`)
	// 名为 x-region 的属性是名字，保留，只去掉它里面的 x-mcp-header。
	assertJSONAt(t, out, base+"properties.x-region", `{"type":"string"}`)
	// 嵌套的 items:true 层层补上 type；items:false 只是去掉 items。
	assertJSONAt(t, out, base+"properties.grid", `{"type":"array","items":{"type":"array","items":{"type":"array","items":{"type":"string"}}}}`)
	assertJSONAt(t, out, base+"properties.empty", `{"type":"array"}`)
	// 联合的分支：true 是 {}，false 的分支去掉；联合节点本身没有 type。
	assertJSONAt(t, out, base+"properties.either", `{"anyOf":[{"type":"string"},{"type":"null"}]}`)
	assertJSONAt(t, out, base+"properties.first", `{"anyOf":[{"type":"integer"}]}`)
	// 指向被去掉的 $defs 项的 $ref 原样留着（这里不去重写引用）。
	assertJSONAt(t, out, base+"properties.never", `{"$ref":"#/$defs/None"}`)
	// allOf 的分支里：true 是 {}，属性里的 true 同样；x- 关键字去掉。
	assertJSONAt(t, out, base+"properties.merged.allOf.1.properties.a", `{"type":"string"}`)
	assertJSONAt(t, out, base+"properties.merged.allOf.1.properties.b", `{"type":"string"}`)
	assertJSONAt(t, out, base+"properties.obj", `{"type":"object","properties":{"deep":{"type":"array","items":{"type":"object","properties":{"v":{"type":"string"}}}}}}`)
}

// ---- 第二轮：独立 review 后补的用例 ----

// TestGeminiToolTypesObjectGetsProperties 被 geminiToolTypes 补成 "type":"object"、自身没有 properties、旁边也没有 $ref/allOf/anyOf/oneOf 的节点
// （裸 object，根或任意节点），一并补 "properties":{}（偏离上游：上游实机记录 Copilot 对根 {"type":"object"} 无 properties 返回 400，Shapes 会补，但 Types 补出的 object 之后没人补）。
// 单步 Types 的字节级期望：在左花括号后插入 "type":"object","properties":{}，后面有成员时跟一个逗号，原本是空对象 {} 时没有逗号；
// 管线里的期望按语义比较，并且输出里不再有 Copilot 会拒绝的形状（没有 properties 的对象）。
func TestGeminiToolTypesObjectGetsProperties(t *testing.T) {
	for _, tc := range []struct{ name, schema, typesOnly, piped string }{
		{"root-empty-object", `{}`,
			`{"type":"object","properties":{}}`,
			`{"type":"object","properties":{}}`},
		{"root-with-description", `{"description":"x"}`,
			`{"type":"object","properties":{},"description":"x"}`,
			`{"type":"object","properties":{},"description":"x"}`},
		// required 说出了 object：单步 Types 保留 required；管线里 Fit 先把 properties 里没有的 a 从 required 删掉（删空去掉键），
		// 根成了 {}，再补 type 与 properties。
		{"root-with-required", `{"required":["a"]}`,
			`{"type":"object","properties":{},"required":["a"]}`,
			`{"type":"object","properties":{}}`},
		{"non-root-additionalProperties", `{"type":"object","properties":{"m":{"additionalProperties":{"type":"string"}}}}`,
			`{"type":"object","properties":{"m":{"type":"object","properties":{},"additionalProperties":{"type":"string"}}}}`,
			`{"type":"object","properties":{"m":{"type":"object","properties":{},"additionalProperties":{"type":"string"}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := geminiTypesBody("gemini-3.8-flash", geminiTypesTool(tc.schema))
			out := runGeminiTransform(t, geminiToolTypes, body)
			if want := geminiTypesBody("gemini-3.8-flash", geminiTypesTool(tc.typesOnly)); string(out) != want {
				t.Fatalf("单步 Types 的结果不符:\n got=%s\nwant=%s", out, want)
			}

			piped := runGeminiTransform(t, geminiPipeline, geminiOneToolBody(tc.schema))
			assertToolSchema(t, piped, tc.piped)
			params, _ := lookupJSON(decodeJSONValue(t, piped), "tools.0.function.parameters")
			if bad := geminiBadShape(params, true); bad != "" {
				t.Errorf("管线的输出仍有 Copilot 会拒绝的形状 %q: %s", bad, piped)
			}
		})
	}
}

// TestGeminiToolTypesLeavesTypedObjectsToShapes 已经写了 "type":"object" 却没有 properties 的节点由 Shapes 处理，Types 不管：
// 单步 Types 返回原切片（本用例在补 properties 的实现下也应是绿的，防止 Types 越界）。
func TestGeminiToolTypesLeavesTypedObjectsToShapes(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object"}`,
		`{"type":"object","properties":{"k":{"type":"object"},"m":{"type":"object","additionalProperties":{"type":"string"}}}}`,
	} {
		assertUnchangedSlice(t, geminiToolTypes, geminiTypesBody("gemini-3.8-flash", geminiTypesTool(schema)))
	}
}

// TestGeminiToolTypesObjectWithRefOrUnionKeepsNoProperties 被补成 object 的节点旁边有 $ref、allOf、anyOf 或 oneOf 时不补 properties:{}
// （补 properties 的依据只覆盖裸 object；这些节点的结构由它们的引用与分支决定，不偏离上游）：
// $ref 指向对象的节点、allOf 的分支是对象的节点，以及根上的 anyOf、oneOf（非根的联合节点本身不补 type，见 keepUnions）。
// 这些节点只补 type；分支、被指向的定义照常处理。单步 Types 的字节级期望。
func TestGeminiToolTypesObjectWithRefOrUnionKeepsNoProperties(t *testing.T) {
	for _, tc := range []struct{ name, schema, want string }{
		{"ref",
			`{"type":"object","$defs":{"D":{"properties":{"k":{"type":"string"}}}},"properties":{"r":{"$ref":"#/$defs/D"}}}`,
			`{"type":"object","$defs":{"D":{"type":"object","properties":{"k":{"type":"string"}}}},"properties":{"r":{"type":"object","$ref":"#/$defs/D"}}}`},
		{"allOf",
			`{"type":"object","properties":{"a":{"allOf":[{"properties":{"k":{"enum":[1]}}}]}}}`,
			`{"type":"object","properties":{"a":{"type":"object","allOf":[{"type":"object","properties":{"k":{"type":"integer","enum":[1]}}}]}}}`},
		{"root-anyOf",
			`{"anyOf":[{"type":"object","properties":{}},{"type":"object","properties":{"a":{}}}]}`,
			`{"type":"object","anyOf":[{"type":"object","properties":{}},{"type":"object","properties":{"a":{"type":"string"}}}]}`},
		{"root-oneOf",
			`{"oneOf":[{"type":"object","properties":{"a":{}}},{"type":"object","properties":{}}]}`,
			`{"type":"object","oneOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runGeminiTransform(t, geminiToolTypes, geminiTypesBody("gemini-3.8-flash", geminiTypesTool(tc.schema)))
			if want := geminiTypesBody("gemini-3.8-flash", geminiTypesTool(tc.want)); string(out) != want {
				t.Fatalf("结果不符:\n got=%s\nwant=%s", out, want)
			}
		})
	}
}

// TestGeminiToolTypesDropsFalsePropertiesFromRequired 去掉值为 false（没有值）的属性时，同级 required 里该名字也去掉，删空则去掉 required 键
// （偏离上游：否则 required 里留着一个已不存在的属性，Copilot 的 Gemini 对这样的 schema 返回 400，而管线里 Fit 在 Types 之前已经跑过）。
// 单步 Types 与管线的结果相同；没有被去掉的属性在 required 里时 required 不变。
func TestGeminiToolTypesDropsFalsePropertiesFromRequired(t *testing.T) {
	for _, tc := range []struct{ name, schema, want string }{
		{"one-of-two",
			`{"type":"object","properties":{"a":{"type":"string"},"gone":false},"required":["a","gone"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`},
		{"only-entry-so-the-key-goes",
			`{"type":"object","properties":{"a":{"type":"string"},"gone":false},"required":["gone"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}}}`},
		{"two-false-properties",
			`{"type":"object","properties":{"a":{"type":"string"},"g1":false,"g2":false},"required":["g1","a","g2"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`},
		{"nested-object",
			`{"type":"object","properties":{"o":{"type":"object","properties":{"x":{"type":"string"},"y":false},"required":["x","y"]}}}`,
			`{"type":"object","properties":{"o":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}}}`},
		// 被去掉的属性不在 required 里：required 原样保留（这一行在没有 required 处理的实现下也是绿的）。
		{"not-required",
			`{"type":"object","properties":{"a":{"type":"string"},"gone":false},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typesOnly := runGeminiTransform(t, geminiToolTypes, geminiOneToolBody(tc.schema))
			assertToolSchema(t, typesOnly, tc.want)
			piped := runGeminiTransform(t, geminiPipeline, geminiOneToolBody(tc.schema))
			assertToolSchema(t, piped, tc.want)
		})
	}
}

// TestGeminiToolTypesRefExpansionBudgetBoundsWork $defs 里 allOf 的分支互相引用（环）时，推断类型会按分支数的深度次方展开 $ref；
// refExpansionBudget 限制每个节点展开的 $ref 次数，用尽后该节点保持原样（与上游 gjson 版本相比是新增的保护）。
// 12 个定义两两互相引用：单步 Types 与管线都应在合理时间内（< 1s）完成，结果是确定的：说不出类型的 $ref 与 allOf 节点保持原样，
// 一个指向 {"type":"integer"} 的 $ref 照常补 type。
func TestGeminiToolTypesRefExpansionBudgetBoundsWork(t *testing.T) {
	const n = 12
	var defs strings.Builder
	for i := 0; i < n; i++ {
		var refs []string
		for j := 0; j < n; j++ {
			if j != i {
				refs = append(refs, fmt.Sprintf(`{"$ref":"#/$defs/D%d"}`, j))
			}
		}
		fmt.Fprintf(&defs, `"D%d":{"allOf":[%s]},`, i, strings.Join(refs, ","))
	}
	schema := `{"type":"object","$defs":{` + defs.String() + `"Leaf":{"type":"integer"}},` +
		`"properties":{"loop":{"$ref":"#/$defs/D0"},"ok":{"$ref":"#/$defs/Leaf"}}}`
	want := strings.Replace(schema, `"ok":{"$ref"`, `"ok":{"type":"integer","$ref"`, 1)
	body := geminiOneToolBody(schema)
	wantBody := geminiOneToolBody(want)

	for _, tr := range []struct {
		name string
		fn   func([]byte) []byte
	}{{"geminiToolTypes", geminiToolTypes}, {"pipeline", geminiPipeline}} {
		t.Run(tr.name, func(t *testing.T) {
			done := make(chan [2][]byte, 1)
			start := time.Now()
			go func() { done <- [2][]byte{tr.fn([]byte(body)), tr.fn([]byte(body))} }()
			select {
			case outs := <-done:
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Errorf("耗时 %v，应远小于 1s：$ref 的展开没有预算", elapsed)
				}
				for _, out := range outs {
					if string(out) != wantBody {
						t.Errorf("结果不符（应只给 ok 补 type）:\n got=%.400s\nwant=%.400s", out, wantBody)
					}
				}
			case <-time.After(10 * time.Second):
				t.Fatal("10s 内没有完成：$ref 环按分支数的深度次方展开，没有预算")
			}
		})
	}
}

// TestGeminiToolTypesKeywordsBeforeValues 对象与数组的关键字先于取值说出类型：没有 type 的 {"items":…,"enum":["a"]} 补成 array
// （不是 string：带 items 的 schema 配一个字符串 enum 被补成 string，发给 Google 就成了字符串上有 items，整个轮次被拒），
// properties 同理补成 object。单步 Types 的行为；管线里 Shapes 先于 Types，不会处理这种没有 type 的 schema，
// 所以补成 array 之后数组上的 enum 仍在（与上游的管线顺序一致）。
func TestGeminiToolTypesKeywordsBeforeValues(t *testing.T) {
	const in = `{"type":"object","properties":{"v":{"items":{"type":"string"},"enum":["a"]},"w":{"properties":{"k":{"type":"string"}},"const":"x"}}}`
	const want = `{"type":"object","properties":{"v":{"type":"array","items":{"type":"string"},"enum":["a"]},` +
		`"w":{"type":"object","properties":{"k":{"type":"string"}},"const":"x"}}}`
	out := runGeminiTransform(t, geminiToolTypes, geminiTypesBody("gemini-3.8-flash", geminiTypesTool(in)))
	if got := geminiTypesBody("gemini-3.8-flash", geminiTypesTool(want)); string(out) != got {
		t.Fatalf("结果不符:\n got=%s\nwant=%s", out, got)
	}
}

// TestGeminiToolTypesResolvesEscapedRefs $ref 里的 JSON 指针转义被正确解析：~1 是 /，~0 是 ~，还有百分号编码（%20 是空格）；
// 指向的 schema 的类型补给引用它的节点。
func TestGeminiToolTypesResolvesEscapedRefs(t *testing.T) {
	body := geminiOneToolBody(`{"type":"object","$defs":{"a/b":{"type":"integer"},"a~b":{"type":"boolean"},"a b":{"type":"number"}},"properties":{` +
		`"s1":{"$ref":"#/$defs/a~1b"},"s2":{"$ref":"#/$defs/a~0b"},"s3":{"$ref":"#/$defs/a%20b"}}}`)
	out := runGeminiTransform(t, geminiToolTypes, body)
	const base = "tools.0.function.parameters.properties."
	assertJSONAt(t, out, base+"s1", `{"type":"integer","$ref":"#/$defs/a~1b"}`)
	assertJSONAt(t, out, base+"s2", `{"type":"boolean","$ref":"#/$defs/a~0b"}`)
	assertJSONAt(t, out, base+"s3", `{"type":"number","$ref":"#/$defs/a%20b"}`)
}

// TestGeminiToolPipelineBooleanBranch 联合里有布尔分支时，管线与单步 Types 的结果不同（与上游一致，不是缺陷）：
//   - 管线里 Shapes 先于 Types，此时 false 还没被去掉，anyOf 里的 false 不是对象，被当作「无法判断类型的分支」，整个联合被丢掉，
//     节点只剩 description，再由 Types 补成 string；
//   - 单步 Types 先去掉 false 分支，联合只剩 integer 一个分支，联合节点本身不补 type。
func TestGeminiToolPipelineBooleanBranch(t *testing.T) {
	schema := `{"type":"object","properties":{"p":{"description":"d","anyOf":[false,{"type":"integer"}]}}}`
	assertToolSchema(t, runGeminiTransform(t, geminiPipeline, geminiOneToolBody(schema)),
		`{"type":"object","properties":{"p":{"description":"d","type":"string"}}}`)
	assertToolSchema(t, runGeminiTransform(t, geminiToolTypes, geminiOneToolBody(schema)),
		`{"type":"object","properties":{"p":{"description":"d","anyOf":[{"type":"integer"}]}}}`)
}

// TestGeminiToolTypesRequiredPruningEdgeCases 去掉 false 属性时修剪 required 的边角：
//   - 名字按解码后的值比较：键或 required 条目里的转义写法（a\"b、é）与字面写法（a"b、é）是同一个名字；
//   - required 里的非字符串条目保留（它们不是名字）；
//   - required 写在 properties 之前也照样修剪；
//   - required 删空时整个成员去掉，它在首位、中间或末尾都不留多余的逗号，输出是合法 JSON；只删掉一部分时位置不变。
//
// 单步 Types 与管线（Fit 在前）的结果相同。
func TestGeminiToolTypesRequiredPruningEdgeCases(t *testing.T) {
	const a = `"a":{"type":"string"}`
	for _, tc := range []struct{ name, schema, want string }{
		{"escaped-quote-in-both",
			`{"type":"object","properties":{"a\"b":false,` + a + `},"required":["a\"b","a"]}`,
			`{"type":"object","properties":{` + a + `},"required":["a"]}`},
		{"unicode-escaped-quote-key-and-backslash-escaped-entry",
			`{"type":"object","properties":{"a\u0022b":false,` + a + `},"required":["a\"b","a"]}`,
			`{"type":"object","properties":{` + a + `},"required":["a"]}`},
		{"literal-accent-key-escaped-entry",
			`{"type":"object","properties":{"é":false,` + a + `},"required":["\u00e9","a"]}`,
			`{"type":"object","properties":{` + a + `},"required":["a"]}`},
		{"escaped-accent-key-literal-entry",
			`{"type":"object","properties":{"\u00e9":false,` + a + `},"required":["é","a"]}`,
			`{"type":"object","properties":{` + a + `},"required":["a"]}`},
		{"non-string-entries-are-kept",
			`{"type":"object","properties":{"gone":false,` + a + `},"required":["a",7,"gone",null,true,{"k":1},["x"]]}`,
			`{"type":"object","properties":{` + a + `},"required":["a",7,null,true,{"k":1},["x"]]}`},
		{"only-non-string-entries-are-left",
			`{"type":"object","properties":{"gone":false,` + a + `},"required":["gone",7]}`,
			`{"type":"object","properties":{` + a + `},"required":[7]}`},
		{"required-before-properties",
			`{"type":"object","required":["a","gone"],"properties":{` + a + `,"gone":false}}`,
			`{"type":"object","required":["a"],"properties":{` + a + `}}`},
		{"emptied-required-first",
			`{"required":["gone"],"type":"object","properties":{` + a + `,"gone":false}}`,
			`{"type":"object","properties":{` + a + `}}`},
		{"emptied-required-in-the-middle",
			`{"type":"object","required":["gone"],"properties":{` + a + `,"gone":false},"additionalProperties":false}`,
			`{"type":"object","properties":{` + a + `},"additionalProperties":false}`},
		{"emptied-required-last",
			`{"type":"object","properties":{` + a + `,"gone":false},"required":["gone"]}`,
			`{"type":"object","properties":{` + a + `}}`},
		{"emptied-required-is-the-only-member-left",
			`{"required":["gone"],"properties":{"gone":false}}`,
			`{"type":"object","properties":{}}`},
		{"partly-emptied-first",
			`{"required":["gone","a"],"type":"object","properties":{` + a + `,"gone":false}}`,
			`{"required":["a"],"type":"object","properties":{` + a + `}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, fn := range []struct {
				name string
				run  func([]byte) []byte
			}{{"geminiToolTypes", geminiToolTypes}, {"pipeline", geminiPipeline}} {
				out := runGeminiTransform(t, fn.run, geminiOneToolBody(tc.schema))
				if !json.Valid(out) {
					t.Fatalf("%s：输出不是合法 JSON: %s", fn.name, out)
				}
				for _, stray := range []string{",,", "{,", "[,", ",}", ",]"} {
					if strings.Contains(string(out), stray) {
						t.Errorf("%s：输出里有多余的逗号 %q: %s", fn.name, stray, out)
					}
				}
				assertToolSchema(t, out, tc.want)
			}
		})
	}
}
