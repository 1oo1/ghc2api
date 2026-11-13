package copilot

// 本文件让 Copilot 的 Gemini 模型接受 /chat/completions 请求中的工具 schema。Copilot 的 Gemini 对下面两类工具
// 整个请求返回 400 "invalid request body"，请求体里的其他内容没有问题：
//   - 工具 schema 的 required 列出了其 properties 里没有的字段（MCP 服务的 schema 常见），或两个工具同名（geminiToolsFit）；
//   - 工具 schema 中由对象分支组成的 anyOf（改成 oneOf 就能通过，geminiToolUnions）。
// 这些函数本身不检查模型，由 handleGeneration 在 /chat/completions 且 geminiModel(model) 时调用。
// 请求体须已通过 json.Valid（handleGeneration 解析 model 时已校验）；对非法输入只保证不 panic 并返回原切片。

import (
	"bytes"
	"encoding/json"
	"strings"
)

// geminiModel 报告模型 id 是否是 Gemini 模型：取最后一个 "/" 之后的部分（兼容 google/gemini-… 这类带厂商前缀的 id），
// 转小写后以 gemini 开头。
func geminiModel(id string) bool {
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	return strings.HasPrefix(strings.ToLower(id), "gemini")
}

// geminiToolsFit 让请求体顶层 tools 的内容被 Copilot 的 Gemini 接受：
//   - 工具 schema 中 required 列出了其所在对象 properties 里没有的字符串条目时，删去这些条目（任意深度，删空则删去 required 键，
//     非字符串条目保留）：这样的条目没有可供模型使用的信息；
//   - 同名工具（function.name 非空）只保留第一个；没有名字的工具不参与去重。
//
// 没有可改的内容、tools 不是数组、请求体不是 JSON 对象时返回原切片。有改动时只替换 tools 数组的原文，
// 数字原文保留、不转义 HTML，请求体其余字节不变。
func geminiToolsFit(body []byte) []byte {
	toolsAt, tools := topLevelArray(body, "tools")
	if tools == nil {
		return body
	}
	var items []json.RawMessage
	if json.Unmarshal(tools, &items) != nil {
		return body
	}
	changed := false
	seen := map[string]bool{}
	kept := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		dec := json.NewDecoder(bytes.NewReader(item))
		dec.UseNumber()
		var tool any
		if dec.Decode(&tool) != nil {
			return body
		}
		if name := toolFunctionName(tool); name != "" {
			if seen[name] {
				changed = true
				continue
			}
			seen[name] = true
		}
		if pruneRequired(tool) {
			rewritten, ok := marshalLiteral(tool)
			if !ok {
				return body
			}
			item, changed = rewritten, true
		}
		kept = append(kept, item)
	}
	if !changed {
		return body
	}
	fitted, ok := marshalLiteral(kept)
	if !ok {
		return body
	}
	out := make([]byte, 0, len(body)-len(tools)+len(fitted))
	out = append(out, body[:toolsAt]...)
	out = append(out, fitted...)
	return append(out, body[toolsAt+len(tools):]...)
}

// toolFunctionName 返回 Chat Completions 风格工具的 function.name；没有名字时返回空串。
func toolFunctionName(tool any) string {
	m, _ := tool.(map[string]any)
	fn, _ := m["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return name
}

// pruneRequired 删去 node 之下每个对象中 required 里其 properties 没有的字符串条目，返回是否删过。
func pruneRequired(node any) bool {
	changed := false
	switch v := node.(type) {
	case []any:
		for _, e := range v {
			changed = pruneRequired(e) || changed
		}
	case map[string]any:
		if required, ok := v["required"].([]any); ok {
			properties, _ := v["properties"].(map[string]any)
			keep := make([]any, 0, len(required))
			for _, entry := range required {
				if name, ok := entry.(string); ok {
					if _, has := properties[name]; !has {
						continue
					}
				}
				keep = append(keep, entry)
			}
			if len(keep) != len(required) {
				changed = true
				if len(keep) == 0 {
					delete(v, "required")
				} else {
					v["required"] = keep
				}
			}
		}
		for _, e := range v {
			changed = pruneRequired(e) || changed
		}
	}
	return changed
}

// geminiToolUnions 把工具 schema（function.parameters、parameters、input_schema）中，分支可证明互不相交的对象分支 anyOf
// 原地改名为 oneOf：二者对选择调用什么的模型含义相同（至多一个分支匹配），而 Copilot 的 Gemini 拒绝前者。
// 等长替换，请求体其余字节（含排版）不动。
//
// 不相交的证明只有两种（成对的分支都要被证明，任一方向成立即可）：一个分支要求某字段，另一个分支把它声明为必填且
// 两者的字符串取值（enum、const）不相交；或另一个分支以 additionalProperties:false 禁止该字段（同时没有可能放行它的 patternProperties）。
// 证明不了的联合（包括可空联合，如 string 或 null）保持 anyOf；同一 schema 里已有 oneOf 时也不改。
// 没有可改名的联合、请求体不是 JSON 对象时返回原切片。
func geminiToolUnions(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"anyOf"`)) {
		return body
	}
	toolsAt, tools := topLevelArray(body, "tools")
	if tools == nil {
		return body
	}
	var at []int
	for _, tool := range jsonMembers(tools) {
		fields := jsonMembers(tool.value)
		at0 := toolsAt + tool.valueAt
		if fn, ok := findMember(fields, "function"); ok {
			if params, ok := findMember(jsonMembers(fn.value), "parameters"); ok {
				collectToolUnions(body, params.value, at0+fn.valueAt+params.valueAt, &at)
			}
		}
		for _, name := range []string{"parameters", "input_schema"} {
			if schema, ok := findMember(fields, name); ok {
				collectToolUnions(body, schema.value, at0+schema.valueAt, &at)
			}
		}
	}
	if len(at) == 0 {
		return body
	}
	out := bytes.Clone(body)
	for _, i := range at {
		copy(out[i:], `"oneOf"`)
	}
	return out
}

// anyOfKey 是联合键的字面量；改名为同长度的 oneOf，因此其余字节都不会移动。
const anyOfKey = `"anyOf"`

// collectToolUnions 在 body 中收集 schema 之下每个可证明不相交的 anyOf 的键的起始下标；schema 的原文从 body[at:] 开始。
// 只沿 JSON Schema 中放子 schema 的关键字向下找，default、examples 等作为数据出现的值不碰。
func collectToolUnions(body, schema []byte, at int, found *[]int) {
	switch firstByte(schema) {
	case '[':
		for _, m := range jsonMembers(schema) {
			collectToolUnions(body, m.value, at+m.valueAt, found)
		}
	case '{':
		members := jsonMembers(schema)
		if union, ok := findMember(members, "anyOf"); ok && !hasMember(members, "oneOf") && disjointObjectBranches(union.value) &&
			bytes.HasPrefix(body[at+union.keyAt:], []byte(anyOfKey)) { // 键写成转义形式时长度不同，不改
			*found = append(*found, at+union.keyAt)
		}
		for _, m := range members {
			switch m.name {
			case "properties", "$defs", "definitions", "dependentSchemas", "patternProperties":
				for _, sub := range jsonMembers(m.value) {
					collectToolUnions(body, sub.value, at+m.valueAt+sub.valueAt, found)
				}
			case "items", "additionalProperties", "contains", "propertyNames", "not", "if", "then", "else",
				"allOf", "anyOf", "oneOf", "prefixItems", "unevaluatedProperties", "unevaluatedItems", "contentSchema":
				collectToolUnions(body, m.value, at+m.valueAt, found)
			}
		}
	}
}

// disjointObjectBranches 报告 union（anyOf 的值）是否只由对象分支组成，且任意两个分支都被证明不相交。
func disjointObjectBranches(union []byte) bool {
	var branches []json.RawMessage
	if json.Unmarshal(union, &branches) != nil || len(branches) < 2 {
		return false
	}
	schemas := make([]map[string]json.RawMessage, len(branches))
	for i, branch := range branches {
		a := objectFields(branch)
		var typ string
		if _, hasRef := a["$ref"]; hasRef || !decodeJSONString(a["type"], &typ) || typ != "object" {
			return false
		}
		for _, b := range schemas[:i] {
			if !objectBranchesApart(a, b) && !objectBranchesApart(b, a) {
				return false
			}
		}
		schemas[i] = a
	}
	return true
}

// objectBranchesApart 报告 a 是否被证明与 b 不相交：a 要求的某个字段 b 永远不会取。
// 要么 b 以 additionalProperties:false 禁止该字段（且没有 patternProperties 放行它），
// 要么 b 也要求该字段，并且两者的字符串取值（enum、const）不相交；b 中非必填的字段可以缺省，等于 a 的所有取值。
func objectBranchesApart(a, b map[string]json.RawMessage) bool {
	aProps, bProps := objectFields(a["properties"]), objectFields(b["properties"])
	bRequired := map[string]bool{}
	for _, name := range requiredNames(b) {
		bRequired[name] = true
	}
	for _, field := range requiredNames(a) {
		bProp, there := bProps[field]
		if !there {
			if _, hasPattern := b["patternProperties"]; !hasPattern && string(bytes.TrimSpace(b["additionalProperties"])) == "false" {
				return true
			}
			continue
		}
		av, bv := schemaStringValues(aProps[field]), schemaStringValues(bProp)
		if !bRequired[field] || len(av) == 0 || len(bv) == 0 {
			continue
		}
		shared := false
		for v := range av {
			shared = shared || bv[v]
		}
		if !shared {
			return true
		}
	}
	return false
}

// requiredNames 返回 schema 的 required 中的字符串条目（保持顺序）；非字符串条目忽略。
func requiredNames(schema map[string]json.RawMessage) []string {
	var entries []json.RawMessage
	if json.Unmarshal(schema["required"], &entries) != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		var name string
		if decodeJSONString(entry, &name) {
			names = append(names, name)
		}
	}
	return names
}

// schemaStringValues 返回 schema 的字符串取值集合：const 为字符串时取它（此时忽略 enum），否则取 enum。
// 无法完整读取（含 $ref、enum 中有非字符串的值）或没有取值时返回空集合；调用方只看其长度，空表示证明不了。
func schemaStringValues(raw json.RawMessage) map[string]bool {
	schema := objectFields(raw)
	if _, hasRef := schema["$ref"]; hasRef {
		return nil
	}
	var constant string
	if decodeJSONString(schema["const"], &constant) {
		return map[string]bool{constant: true}
	}
	var enum []json.RawMessage
	if json.Unmarshal(schema["enum"], &enum) != nil {
		return nil
	}
	values := make(map[string]bool, len(enum))
	for _, entry := range enum {
		var value string
		if !decodeJSONString(entry, &value) {
			return nil
		}
		values[value] = true
	}
	return values
}

// objectFields 把 raw 解析为 JSON 对象的成员（成员的值保持原文）；raw 不是对象（含 null）时返回 nil，读取 nil map 总是安全的。
func objectFields(raw json.RawMessage) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

// decodeJSONString 把 JSON 字符串字面量 raw 解码到 s；raw 不是字符串（含 null）时返回 false。
func decodeJSONString(raw json.RawMessage, s *string) bool {
	return firstByte(raw) == '"' && json.Unmarshal(raw, s) == nil
}

// jsonMember 是 JSON 对象的一个成员（数组的一个元素，此时 name 为空、keyAt 为 -1）在其容器原文中的位置。
type jsonMember struct {
	name    string // 解码后的键
	keyAt   int    // 键的起始引号在容器原文中的下标
	value   []byte // 值的原文（容器的子切片，不含前导空白）
	valueAt int    // value 在容器原文中的下标
}

// jsonMembers 按顺序返回 JSON 对象（或数组）raw 的成员，位置相对 raw 的起点；raw 不是对象或数组时返回 nil。
// 重复的键都会返回。值按 jsonscan.go 的 valueEnd 定界，不解析容器内部；输入非法时在无法前进处停止。
func jsonMembers(raw []byte) []jsonMember {
	i := skipJSONSpace(raw, 0)
	if i >= len(raw) || (raw[i] != '{' && raw[i] != '[') {
		return nil
	}
	isObject := raw[i] == '{'
	i++
	var members []jsonMember
	for {
		i = skipJSONSpace(raw, i)
		if i >= len(raw) || raw[i] == '}' || raw[i] == ']' {
			return members
		}
		member := jsonMember{keyAt: -1}
		if isObject {
			if raw[i] != '"' {
				return members
			}
			end := jsonStringEnd(raw, i)
			member.keyAt, member.name = i, decodeKey(raw[i:end+1])
			i = skipJSONSpace(raw, end+1)
			if i >= len(raw) || raw[i] != ':' {
				return members
			}
			i = skipJSONSpace(raw, i+1)
		}
		end := valueEnd(raw, i)
		if end <= i {
			return members
		}
		member.value, member.valueAt = raw[i:end], i
		members = append(members, member)
		i = skipJSONSpace(raw, end)
		if i < len(raw) && raw[i] == ',' {
			i++
		}
	}
}

// decodeKey 解码键的字面量（含引号）；没有转义时不分配临时对象，解码失败返回空串。
func decodeKey(literal []byte) string {
	if len(literal) < 2 {
		return ""
	}
	if bytes.IndexByte(literal, '\\') < 0 {
		return string(literal[1 : len(literal)-1])
	}
	var key string
	_ = json.Unmarshal(literal, &key)
	return key
}

// findMember 返回第一个名为 name 的成员。
func findMember(members []jsonMember, name string) (jsonMember, bool) {
	for _, m := range members {
		if m.name == name {
			return m, true
		}
	}
	return jsonMember{}, false
}

func hasMember(members []jsonMember, name string) bool {
	_, ok := findMember(members, name)
	return ok
}

// topLevelArray 返回请求体顶层键 name 的数组值的原文及其在 body 中的下标；
// 请求体不是 JSON 对象、没有该键或其值不是数组时返回 nil。
func topLevelArray(body []byte, name string) (at int, value []byte) {
	if firstByte(body) != '{' || !bytes.Contains(body, []byte(`"`+name+`"`)) {
		return 0, nil
	}
	member, ok := findMember(jsonMembers(body), name)
	if !ok || firstByte(member.value) != '[' {
		return 0, nil
	}
	return member.valueAt, member.value
}
