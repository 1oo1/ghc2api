package copilot

// 本文件给 Copilot 的 Gemini 模型的工具 schema 里缺 type 的节点补上 type，移植自 magpie 的 internal/gateway/gemini_schema.go
// （geminiTypes，keepUnions 固定为 true，去掉模型检查）与 kimi_schema.go 里的类型推断辅助函数（schemaTypes、keywordType、valuesType）。
//
// Vertex 把函数声明的 parameters 当作自己的 Schema 对象，其中 type 必填，缺一处就对整个请求返回 400（即使这个工具从不被调用）：
// "`mcp__cua-driver__drag` functionDeclaration `parameters.target` schema didn't specify the schema type field."。
// Copilot 的 Gemini 背后是 Vertex，缺 type 的节点原样保留同样被拒。
//
// 每个缺 type 的 schema 补上它的取值（enum、const、default）、关键字（properties 为 object，items 为 array，pattern 为 string 等；
// 对象与数组的关键字先于取值）或分支（anyOf、oneOf、allOf、本地 $ref）说出的那个类型，null 不算：Vertex 一个节点只有一个类型，
// 可空另算。分支类型不止一种、或类型说不清的 $ref 与联合保持原样：没有哪个类型是对的。什么都没说的（{} 或只有 description）
// 是 string（能装任何值的类型），工具的根是 object。not、if 之下不补：那里补 type 会把规则反过来。
// 非根的联合节点（anyOf 或 oneOf）本身不补 type：Copilot 的 Gemini 对可空联合旁边写的 type 返回 400（magpie #4374），
// 无论分支写了什么，联合都保持客户端写的样子，其分支照常补。根必须是 object，所以根即使是联合也补。
//
// type 就地写在 schema 的左花括号之后，请求体其余字节不动（缓存前缀不变）。
// 先去掉 Schema 容纳不了的布尔 schema 与 x- 关键字（geminiSchemaShapes 对应的 shapeToolSchemas）。
//
// 相对上游的两处偏离：
//   - 补成 object 的裸节点（自己没有 properties，旁边也没有 $ref、allOf、anyOf、oneOf）一并补 properties:{}：上游实机记录 Copilot 对
//     根 {"type":"object"} 没有 properties 返回 400，Shapes 在本步之前运行，处理不到这里新补的 object（已经写了 type:"object" 的节点仍归 Shapes）；
//   - 去掉值为 false 的属性时，同级 required 里该名字也去掉，删空则去掉 required 键：Fit 在本步之前运行，否则会留下它要避免的 400 条件。
//
// 工具里的三处 schema（function.parameters、parameters、input_schema）各自处理，只有对象才处理。
// 没有可改的内容、tools 不是数组、请求体不是合法的 JSON 对象时返回原切片；有改动时只替换 tools 数组的原文，数字原文保留。

import (
	"bytes"
	"net/url"
	"slices"
	"strings"
)

// geminiToolTypes 先去掉工具 schema 中的布尔 schema 与 x- 关键字，再给缺 type 的节点补 type，见本文件头注释。
func geminiToolTypes(body []byte) []byte {
	at, tools := geminiToolsArray(body)
	if tools == nil {
		return body
	}
	shaped, shapedChanged := shapeToolSchemas(tools)
	typed, typedChanged := typeToolSchemas(shaped)
	if !shapedChanged && !typedChanged {
		return body
	}
	return replaceTools(body, at, len(tools), typed)
}

// textEdit 把原文的 [from, to) 换成 text；from == to 是插入。
type textEdit struct {
	from, to int
	text     string
}

// applyEdits 返回 src 应用 edits 之后的结果，并报告是否有改动；没有编辑时返回 src 本身。
// 编辑按位置排序，与前一个重叠的跳过。
func applyEdits(src []byte, edits []textEdit) ([]byte, bool) {
	if len(edits) == 0 {
		return src, false
	}
	slices.SortStableFunc(edits, func(a, b textEdit) int { return a.from - b.from })
	out := make([]byte, 0, len(src)+len(edits)*24)
	last := 0
	for _, e := range edits {
		if e.from < last {
			continue
		}
		out = append(out, src[last:e.from]...)
		out = append(out, e.text...)
		last = e.to
	}
	return append(out, src[last:]...), true
}

// forEachToolSchema 对 tools 数组 list 中每个工具的三处 schema（function.parameters、parameters、input_schema）里是对象的那些调用 fn。
func forEachToolSchema(list *jsonNode, fn func(root *jsonNode)) {
	if !list.isArray() {
		return
	}
	for _, tool := range list.kids {
		for _, schema := range []*jsonNode{tool.get("function").get("parameters"), tool.get("parameters"), tool.get("input_schema")} {
			if schema.isObject() {
				fn(schema)
			}
		}
	}
}

// ---- 去掉布尔 schema 与 x- 关键字 ----

// schemaMaps 是值为「名字到 schema 的映射」的关键字，schemaOne 的值是一个 schema，schemaList 的值是 schema 的列表。
// additionalProperties、unevaluatedProperties、unevaluatedItems 有意不在其中：它们的 false 是常见的「不许再有别的」，
// 各家转换器都认得，不能去掉。
var (
	schemaMaps = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true}
	schemaOne  = map[string]bool{"items": true, "contains": true, "propertyNames": true, "not": true, "if": true, "then": true, "else": true, "contentSchema": true}
	schemaList = map[string]bool{"anyOf": true, "oneOf": true, "allOf": true, "prefixItems": true}
)

// shapeToolSchemas 把工具 schema 裁成 Schema 对象容纳得了的样子，返回新的 tools 数组原文及是否有改动（对应上游 geminiSchemaShapes）。
// 有效 JSON Schema 里有两样东西 Schema 对象放不下，Google 对其中任何一样都会拒绝整个请求：
//   - 布尔 schema（items: true、属性值为 true 或 false）："Invalid value at '…items.items' (…aiplatform.master.Schema), true"。
//     true 是 {}（任何值），之后同其他 {} 一样补 type；false（没有值）去掉：作为属性、分支或关键字的值都去掉；
//   - x- 关键字（厂商自己的注解，没有模型会读）：MCP 的 x-mcp-header（2026-07-28 规范，SEP-2243）写在属性上，
//     转发到 Vertex 得到 'Unknown name "x-mcp-header" ...: Cannot find field'。只有 schema 自己的键才是关键字：名为 x-… 的属性是名字，保留。
//
// 没有这两样的 schema 逐字节原样保留；有的整个 schema 按各成员原有的顺序重写（其中没有这两样的子 schema 仍保留原文）。
func shapeToolSchemas(tools []byte) ([]byte, bool) {
	root := parseJSONTree(tools)
	var edits []textEdit
	forEachToolSchema(root, func(schema *jsonNode) {
		if raw, changed, keep := shapeSchema(schema); changed && keep {
			edits = append(edits, textEdit{schema.at, schema.at + len(schema.raw), string(raw)})
		}
	})
	return applyEdits(tools, edits)
}

// shapeSchema 返回 schema s 去掉布尔 schema 与 x- 关键字之后的原文，changed 报告是否有去掉的；keep 为 false 表示 s 本身是 false，
// 即根本没有这个 schema。s 之下没有改动的子 schema 返回其原文。
// 值为 false 的属性去掉时，同级 required 里该名字也去掉，删空则去掉 required 键（偏离上游：否则 required 里留着一个已不存在的属性，
// 正是 geminiToolsFit 要避免的 400 条件，而它在本步之前已经运行过）。
func shapeSchema(s *jsonNode) (raw []byte, changed, keep bool) {
	switch s.kind {
	case 't':
		return []byte("{}"), true, true
	case 'f':
		return nil, true, false
	case '{':
	default:
		return s.raw, false, true
	}
	// 先逐个成员算出新原文，没有任何改动时直接返回 s 的原文
	type member struct {
		key, value []byte
		skip       bool      // required 删空后整个成员去掉
		required   *jsonNode // required 成员的原节点，等 properties 处理完再修剪
	}
	members := make([]member, 0, len(s.keys))
	gone := map[string]bool{} // properties 里值为 false 而被去掉的属性名
	for i, k := range s.keys {
		v := s.kids[i]
		switch {
		case strings.HasPrefix(k.name, "x-"):
			changed = true
		case schemaMaps[k.name] && v.isObject():
			value, ch, dropped := shapeSchemaList(v, true)
			changed = changed || ch
			if k.name == "properties" {
				for _, name := range dropped {
					gone[name] = true
				}
			}
			members = append(members, member{key: k.raw, value: value})
		case schemaOne[k.name]:
			value, ch, ok := shapeSchema(v)
			changed = changed || ch
			if ok {
				members = append(members, member{key: k.raw, value: value})
			}
		case schemaList[k.name] && v.isArray():
			value, ch, _ := shapeSchemaList(v, false)
			changed = changed || ch
			members = append(members, member{key: k.raw, value: value})
		case k.name == "required" && v.isArray():
			members = append(members, member{key: k.raw, value: v.raw, required: v})
		default:
			members = append(members, member{key: k.raw, value: v.raw})
		}
	}
	if len(gone) > 0 {
		for i, m := range members {
			if m.required == nil {
				continue
			}
			var kept [][]byte
			for _, entry := range m.required.kids {
				if entry.kind == '"' && gone[entry.text()] {
					continue
				}
				kept = append(kept, entry.raw)
			}
			if len(kept) == len(m.required.kids) {
				continue
			}
			members[i].skip = len(kept) == 0
			members[i].value = append(append([]byte{'['}, bytes.Join(kept, []byte{','})...), ']')
		}
	}
	if !changed {
		return s.raw, false, true
	}
	out := []byte{'{'}
	for _, m := range members {
		if m.skip {
			continue
		}
		if len(out) > 1 {
			out = append(out, ',')
		}
		out = append(out, m.key...)
		out = append(out, ':')
		out = append(out, m.value...)
	}
	return append(out, '}'), true, true
}

// shapeSchemaList 返回容器 v（名字到 schema 的映射，或 schema 的数组）去掉布尔 schema 与 x- 关键字之后的原文，
// 值为 false 的成员去掉（映射时返回被去掉的名字）；没有改动时是 v 的原文。
func shapeSchemaList(v *jsonNode, isMap bool) (raw []byte, changed bool, dropped []string) {
	type member struct{ key, value []byte }
	members := make([]member, 0, len(v.kids))
	for i, sub := range v.kids {
		value, ch, keep := shapeSchema(sub)
		changed = changed || ch
		if !keep {
			if isMap {
				dropped = append(dropped, v.keys[i].name)
			}
			continue
		}
		var key []byte
		if isMap {
			key = v.keys[i].raw
		}
		members = append(members, member{key, value})
	}
	if !changed {
		return v.raw, false, nil
	}
	open, closing := byte('['), byte(']')
	if isMap {
		open, closing = '{', '}'
	}
	out := []byte{open}
	for i, m := range members {
		if i > 0 {
			out = append(out, ',')
		}
		if isMap {
			out = append(out, m.key...)
			out = append(out, ':')
		}
		out = append(out, m.value...)
	}
	return append(out, closing), true, dropped
}

// ---- 补 type ----

// typeWalkOne 是补 type 时只含一个 schema 的关键字（值是 schema 或 schema 的列表）；not、if 不在其中，那里补 type 会把规则反过来。
// 名字到 schema 的映射用 schemaMaps。
var typeWalkOne = map[string]bool{
	"items": true, "additionalProperties": true, "contains": true, "propertyNames": true, "then": true, "else": true,
	"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true,
	"unevaluatedProperties": true, "unevaluatedItems": true, "contentSchema": true,
}

// typeToolSchemas 给 tools 数组里工具 schema 中缺 type 的节点补 type，返回新的 tools 数组原文及是否有改动。
func typeToolSchemas(tools []byte) ([]byte, bool) {
	root := parseJSONTree(tools)
	var edits []textEdit
	forEachToolSchema(root, func(schema *jsonNode) { collectUntyped(schema, schema, true, &edits) })
	return applyEdits(tools, edits)
}

// collectUntyped 为 node 及其之下每个缺 type 的 schema 加一个编辑：在它的左花括号之后写入说得出的类型。root 是工具 schema 的根，
// isRoot 表示 node 就是根。
func collectUntyped(root, node *jsonNode, isRoot bool, edits *[]textEdit) {
	switch {
	case node.isArray():
		for _, v := range node.kids {
			collectUntyped(root, v, false, edits)
		}
	case node.isObject():
		union := !isRoot && (node.get("anyOf") != nil || node.get("oneOf") != nil)
		if !union && node.get("type") == nil {
			if t := geminiSchemaType(root, node, isRoot); t != "" {
				text := `"type":"` + t + `"`
				if t == "object" && bareObject(node) {
					// Copilot 对没有 properties 的 object 返回 400，而 Shapes 在本步之前运行，处理不到这里新补的 object
					text += `,"properties":{}`
				}
				if len(node.kids) > 0 {
					text += "," // 空对象 {} 里没有后继成员，不需要逗号
				}
				*edits = append(*edits, textEdit{node.at + 1, node.at + 1, text})
			}
		}
		for i, k := range node.keys {
			switch {
			case schemaMaps[k.name]:
				for _, s := range node.kids[i].kids {
					collectUntyped(root, s, false, edits)
				}
			case typeWalkOne[k.name]:
				collectUntyped(root, node.kids[i], false, edits)
			}
		}
	}
}

// bareObject 报告被补成 object 的节点 n 是否是裸 object：自己没有 properties，旁边也没有 $ref、allOf、anyOf、oneOf
// （这些节点的结构由引用与分支决定，不补 properties）。
func bareObject(n *jsonNode) bool {
	for _, k := range []string{"properties", "$ref", "allOf", "anyOf", "oneOf"} {
		if n.get(k) != nil {
			return false
		}
	}
	return true
}

// refExpansionBudget 是为一个节点推断类型时最多展开的 $ref 次数。正常 schema 只展开个位数；
// 没有预算时，$defs 里 allOf 的分支互相引用（环）会按分支数的深度次方展开（与 objectRootRefBudget 同理）。
// 用尽后再遇到的 $ref 按不可解析处理，该节点不补 type。
const refExpansionBudget = 256

// geminiSchemaType 返回缺 type 的 schema s 补的那一个类型，保持原样时返回空串。
func geminiSchemaType(root, s *jsonNode, isRoot bool) string {
	if t := keywordType(s); t == "object" || t == "array" {
		// properties、items 先于取值说话：带 items 的 schema 配一个字符串 enum 被补成 "string"，
		// 发给 Google 就成了字符串上有 items，Antigravity 因此拒绝了整个轮次
		return t
	}
	enum, constant := s.get("enum"), s.get("const")
	if enum.isArray() || constant != nil {
		vals := enum.elements()
		if constant != nil {
			vals = []*jsonNode{constant}
		}
		var rest []*jsonNode
		for _, v := range vals {
			if v.kind != 'n' {
				rest = append(rest, v)
			}
		}
		return valuesType(rest)
	}
	budget := refExpansionBudget
	ts := schemaTypes(root, s, 0, &budget)
	delete(ts, "null")
	if ts["integer"] && ts["number"] {
		delete(ts, "integer")
	}
	if len(ts) == 1 {
		for t := range ts {
			return t
		}
	}
	if len(ts) > 1 {
		return ""
	}
	if isRoot {
		return "object"
	}
	for _, k := range []string{"$ref", "anyOf", "oneOf", "allOf"} {
		if s.get(k) != nil {
			return ""
		}
	}
	if d := s.get("default"); d != nil && d.kind != 'n' {
		switch d.kind {
		case '{':
			return "object"
		case '[':
			return "array"
		}
		if t := valuesType([]*jsonNode{d}); t != "" {
			return t
		}
	}
	return "string"
}

// schemaTypes 返回 s 可能有的类型的集合：由它的 type、关键字、取值，或分支（含 $ref 指向的 schema）说出；说不出时返回 nil。
// budget 是还能展开的 $ref 次数，见 refExpansionBudget。
func schemaTypes(root, s *jsonNode, depth int, budget *int) map[string]bool {
	if depth > 16 || !s.isObject() {
		return nil
	}
	ts := map[string]bool{}
	if t := s.get("type"); t != nil {
		for _, v := range t.elements() {
			ts[v.text()] = true
		}
		return ts
	}
	if ref := s.get("$ref"); ref != nil && ref.kind == '"' {
		if *budget <= 0 {
			return nil
		}
		*budget--
		if t := refTarget(root, ref.text()); t != nil {
			return schemaTypes(root, t, depth+1, budget)
		}
		return nil
	}
	if enum, constant := s.get("enum"), s.get("const"); enum.isArray() || constant != nil {
		vals := enum.elements()
		if constant != nil {
			vals = []*jsonNode{constant}
		}
		for _, v := range vals {
			switch v.kind {
			case '"':
				ts["string"] = true
			case 't', 'f':
				ts["boolean"] = true
			case 'n':
				ts["null"] = true
			case '0':
				ts[numberType(v)] = true
			case '[':
				ts["array"] = true
			default:
				ts["object"] = true
			}
		}
		return ts
	}
	// 有说不出类型的分支就让 oneOf 与 anyOf 成为任何类型；allOf 只取说得出的分支
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		branches := s.get(k)
		if !branches.isArray() {
			continue
		}
		known := false
		for _, b := range branches.kids {
			bt := schemaTypes(root, b, depth+1, budget)
			if len(bt) == 0 && k != "allOf" {
				return nil
			}
			for t := range bt {
				ts[t] = true
				known = true
			}
		}
		if known {
			return ts
		}
	}
	t := keywordType(s)
	if t == "" {
		return nil
	}
	ts[t] = true
	return ts
}

// keywordType 返回 schema s 的关键字说出的类型（properties 为 object，items 为 array，pattern 为 string，minimum 为 number），
// 说不出时返回空串。对象与数组的关键字先看：它们带着自己的子 schema，校验器只在该类型下才读。
func keywordType(s *jsonNode) string {
	has := func(keys ...string) bool {
		for _, k := range keys {
			if s.get(k) != nil {
				return true
			}
		}
		return false
	}
	switch {
	case has("properties", "required", "additionalProperties", "patternProperties", "minProperties", "maxProperties", "propertyNames", "dependentRequired", "dependentSchemas", "unevaluatedProperties"):
		return "object"
	case has("items", "prefixItems", "minItems", "maxItems", "uniqueItems", "contains", "unevaluatedItems"):
		return "array"
	case has("minLength", "maxLength", "pattern", "format", "contentEncoding", "contentMediaType"):
		return "string"
	case has("minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"):
		return "number"
	}
	return ""
}

// numberType 返回数字节点的类型：原文含小数点或指数是 number，否则是 integer。
func numberType(v *jsonNode) string {
	if bytes.ContainsAny(v.raw, ".eE") {
		return "number"
	}
	return "integer"
}

// valuesType 返回 vals 全都有的 JSON Schema 类型；没有值或类型不一致时返回空串，整数与小数混在一起算 number。
func valuesType(vals []*jsonNode) string {
	t := ""
	for _, v := range vals {
		var vt string
		switch v.kind {
		case '"':
			vt = "string"
		case 't', 'f':
			vt = "boolean"
		case '0':
			vt = numberType(v)
		default:
			return ""
		}
		switch {
		case t == "" || t == vt:
			t = vt
		case t == "integer" && vt == "number" || t == "number" && vt == "integer":
			t = "number"
		default:
			return ""
		}
	}
	return t
}

// refTarget 返回本地 $ref（"#"、"#/$defs/x"）在 root 中指向的 schema；不是本地引用或指向的位置不存在时返回 nil。
func refTarget(root *jsonNode, ref string) *jsonNode {
	if ref != "#" && !strings.HasPrefix(ref, "#/") {
		return nil
	}
	tokens := strings.Split(ref, "/")[1:]
	for i, tok := range tokens {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		if u, err := url.PathUnescape(tok); err == nil {
			tok = u
		}
		tokens[i] = tok
	}
	return root.pointer(tokens)
}
