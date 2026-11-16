package copilot

// 本文件让 Copilot 的 Gemini 模型接受 /chat/completions 请求中几种形状的工具 schema，移植自 magpie 的
// internal/gateway/copilot_gemini_shapes.go（copilotGeminiToolShapes，去掉 provider、协议与模型检查）。
//
// Copilot 的 Gemini 对六种形状的工具 schema 返回 400 "invalid request body"，别家厂商接受它们（MCP 服务常写出这些形状）：
// 没有 properties 的对象、分支同类型的 anyOf、有分支无法判断类型的 anyOf、数组上的 enum、根上的联合、元组数组（prefixItems）。
// 每种都改写成 Gemini 接受的最接近的 schema，同时保留模型能据以行动的信息：
//   - 没有 properties 的对象：补上 properties:{}；
//   - 分支同类型的 anyOf（null 除外）：合并成那一个 schema（对象的 properties 取并集，字符串 enum 取并集），可空的仍写成与 null 的 anyOf；
//     分支是本地 $ref 时先按 $defs/definitions 解析出（拷贝）它指向的 schema 再判断；
//   - 有分支无法判断类型的 anyOf：去掉联合，保留 description，该节点之后由 geminiToolTypes 按缺 type 的节点补 type；
//   - 可空的单分支联合旁边写着 type：去掉这个 type（Copilot 对联合旁的 type 返回 400）；
//   - 没有 items 的数组：补上 items:{}（之后由 geminiToolTypes 补 type）；
//   - 数组上的 enum：分支是字符串时挪到 items，否则丢掉；
//   - 根上的联合：改成一个 object，properties 是各对象分支 properties 的并集，不要求任何字段；
//   - prefixItems（或写成列表的 items）：改成 items：各位置共用的那一个 schema，或它们的 anyOf。
//
// 可证明不相交的对象联合（如 Codex 的 create_thread 的 target）同样这样折叠：Copilot 的 Gemini 对嵌套的 oneOf 返回 200
// 却丢掉分支，模型于是发出字符串，所以不能改名为 oneOf。客户端写的嵌套 oneOf 保持原样。
// 没有这些形状的 schema 逐字节原样通过。
//
// 工具里的三处 schema（function.parameters、parameters、input_schema）各自处理，只有对象才处理；
// default、examples、enum、const 是数据，其中形如 schema 的对象不当作 schema。
// 没有可改的内容、tools 不是数组、请求体不是合法的 JSON 对象时返回原切片；有改动时只替换 tools 数组的原文（键序按字母序，
// 数字原文保留，不转义 HTML），请求体其余字节不变。

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
)

// geminiToolShapes 把工具 schema 中 Copilot 的 Gemini 会返回 400 的形状改写成它接受的最接近的 schema，见本文件头注释。
func geminiToolShapes(body []byte) []byte {
	at, tools := geminiToolsArray(body)
	if tools == nil {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(tools))
	dec.UseNumber()
	var list []any
	if dec.Decode(&list) != nil {
		return body
	}
	changed := false
	for _, tool := range list {
		m, _ := tool.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		for _, schema := range []any{fn["parameters"], m["parameters"], m["input_schema"]} {
			if root, ok := schema.(map[string]any); ok {
				changed = shapeForGemini(root, true, &shapeScope{top: root, derefs: derefBudget, targets: map[string]derefTarget{}}) || changed
			}
		}
	}
	if !changed {
		return body
	}
	shaped, ok := marshalLiteral(list)
	if !ok {
		return body
	}
	return replaceTools(body, at, len(tools), shaped)
}

// derefBudget 是一个工具 schema 内最多写进输出的 $ref 内联拷贝数（derefLocal 返回、并被 mergeUnion 合并进结果的拷贝）。
// 每个联合的结果里，它的每个 $ref 分支都带着一份 $defs 里目标的拷贝，一个较大的 $defs.D 被很多个联合引用时，输出会随联合个数
// 线性放大成「联合数 × D」（148KB 的请求可放大到 49MB，分配约 2GB）；预算限制这个放大倍数。
// 只有真正写进输出的拷贝才扣预算：联合被去掉、分支类型不一致、可空的单分支联合这些丢弃拷贝的路径不扣（mergeUnion 在这些路径上
// 恢复预算）。正常 schema 只内联个位数份。用尽后再遇到的 $ref 分支按无法解析处理（与上游对解析不了的 $ref 相同，该联合被去掉）。
// 偏离上游（上游没有预算），与 objectRootRefBudget 同理。
const derefBudget = 256

// shapeScope 是改写一个工具 schema 时沿递归共享的状态。
type shapeScope struct {
	top     map[string]any         // 工具 schema 的根（本地 $ref 所指的 $defs 在其中）
	derefs  int                    // 还能内联的 $ref 拷贝数，见 derefBudget
	version int                    // $defs、definitions 的项被处理过的次数；缓存里版本不同的条目作废（目标可能已被改写）
	targets map[string]derefTarget // 按 $ref 缓存的解析结果，避免对大 $defs 项重复序列化
}

// derefTarget 是一个本地 $ref 的解析结果：raw 是目标的 JSON 原文，解析不了（目标不存在或其中还有 $ref）时为 nil。
type derefTarget struct {
	raw     []byte
	version int
}

// shapeForGemini 就地改写 schema s（及其之下的子 schema），返回是否有改动。root 表示 s 是工具 schema 的根，sc 是这个工具 schema
// 的共享状态。
func shapeForGemini(s map[string]any, root bool, sc *shapeScope) bool {
	changed := false
	// 先处理子 schema，使联合是按已适配的分支来判断的。映射内按键排序，使同一个请求体总是得到同一个结果
	// （联合分支的 $ref 会读到已被改写的 $defs 项，顺序不同结果会不同）。
	for _, k := range []string{"properties", "patternProperties", "$defs", "definitions"} {
		if m, ok := s[k].(map[string]any); ok {
			for _, name := range slices.Sorted(maps.Keys(m)) {
				if c, ok := m[name].(map[string]any); ok {
					changed = shapeForGemini(c, false, sc) || changed
					if k == "$defs" || k == "definitions" {
						sc.version++ // $ref 的目标可能被改写了，缓存的解析结果作废
					}
				}
			}
		}
	}
	for _, k := range []string{"items", "additionalProperties", "not", "contains"} {
		if c, ok := s[k].(map[string]any); ok {
			changed = shapeForGemini(c, false, sc) || changed
		}
	}
	for _, k := range []string{"anyOf", "oneOf", "allOf", "prefixItems", "items"} {
		if l, ok := s[k].([]any); ok {
			for _, v := range l {
				if c, ok := v.(map[string]any); ok {
					changed = shapeForGemini(c, false, sc) || changed
				}
			}
		}
	}

	// 元组：改成一个 items schema
	tuple, hasPrefix := s["prefixItems"].([]any)
	if l, ok := s["items"].([]any); ok {
		tuple, hasPrefix = l, true
	}
	if _, has := s["prefixItems"]; has || hasPrefix {
		delete(s, "prefixItems")
		if _, isSchema := s["items"].(map[string]any); !isSchema {
			s["items"] = tupleItems(tuple)
			if m, ok := s["items"].(map[string]any); ok {
				shapeForGemini(m, false, sc)
			}
		}
		if s["type"] == nil {
			s["type"] = "array"
		}
		changed = true
	}

	// 没有 items 的数组：Copilot 背后的 Vertex 拒绝它，补出的 items 之后由 geminiToolTypes 补 type
	if s["type"] == "array" && s["items"] == nil {
		s["items"] = map[string]any{}
		changed = true
	}

	// 数组上的 enum 属于它的 items
	if _, has := s["enum"]; has && s["type"] == "array" {
		if es, ok := s["enum"].([]any); ok && allStrings(es) {
			items, _ := s["items"].(map[string]any)
			if items == nil && s["items"] == nil {
				items = map[string]any{"type": "string"}
				s["items"] = items
			}
			if items != nil && items["enum"] == nil && (items["type"] == nil || items["type"] == "string") {
				items["enum"] = es
				if items["type"] == nil {
					items["type"] = "string"
				}
			}
		}
		delete(s, "enum")
		changed = true
	}

	// 联合
	if alts, ok := s["oneOf"].([]any); ok && root {
		flattenRootUnion(s, "oneOf", alts)
		changed = true
	} else if alts, ok := s["anyOf"].([]any); ok {
		if root && s["type"] == nil {
			flattenRootUnion(s, "anyOf", alts)
			changed = true
		} else {
			changed = mergeUnion(s, "anyOf", alts, sc) || changed
		}
	}

	if s["type"] == "object" {
		if _, has := s["properties"]; !has {
			s["properties"] = map[string]any{}
			changed = true
		}
	}
	return changed
}

func allStrings(es []any) bool {
	for _, e := range es {
		if _, ok := e.(string); !ok {
			return false
		}
	}
	return true
}

// tupleItems 返回元组各位置共用的 items schema：去重后只有一种就是它，有多种则是它们的 anyOf，没有则是 {}。
func tupleItems(prefix []any) any {
	var uniq []any
	for _, p := range prefix {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		dup := false
		for _, u := range uniq {
			dup = dup || reflect.DeepEqual(u, m)
		}
		if !dup {
			uniq = append(uniq, m)
		}
	}
	switch len(uniq) {
	case 0:
		return map[string]any{}
	case 1:
		return uniq[0]
	}
	return map[string]any{"anyOf": uniq}
}

// flattenRootUnion 把根上的联合改成一个 object，包含各对象分支的 properties（同名取第一个，根自己的优先）；
// 不保留任何分支的 required，因为实际用的可能是另一个分支。
func flattenRootUnion(s map[string]any, key string, alts []any) {
	props := map[string]any{}
	for _, a := range alts {
		m, _ := a.(map[string]any)
		if ps, ok := m["properties"].(map[string]any); ok {
			for n, v := range ps {
				if _, has := props[n]; !has {
					props[n] = v
				}
			}
		}
	}
	if own, ok := s["properties"].(map[string]any); ok {
		for n, v := range own {
			props[n] = v
		}
	}
	delete(s, key)
	delete(s, "required")
	s["type"] = "object"
	s["properties"] = props
}

// mergeUnion 把分支同类型（null 除外）的 anyOf 合并成一个 schema，去掉有分支没有类型的 anyOf，返回是否改动了 s。
func mergeUnion(s map[string]any, key string, alts []any, sc *shapeScope) bool {
	// 内联预算只按写进结果的拷贝扣：丢弃拷贝的返回路径用 discard 恢复入口时的预算
	before := sc.derefs
	discard := func(changed bool) bool {
		sc.derefs = before
		return changed
	}
	var typed []map[string]any
	nullable := false
	typ := ""
	for _, a := range alts {
		m, _ := a.(map[string]any)
		if m == nil {
			delete(s, key)
			return discard(true)
		}
		if ref, ok := m["$ref"].(string); ok {
			// 本地 $ref 的分支就是它指向的 schema，联合按模型实际要填的内容来判断
			if r := derefLocal(sc, ref); r != nil {
				shapeForGemini(r, false, sc)
				m = r
			}
		}
		t, _ := m["type"].(string)
		if t == "null" {
			nullable = true
			continue
		}
		if t == "" {
			t = guessType(m)
		}
		if t == "" {
			// 没有任何线索说明这个分支是什么：联合去掉，节点没有类型，之后同其他缺 type 的节点一样补 type
			delete(s, key)
			return discard(true)
		}
		m["type"] = t
		if typ != "" && typ != t {
			return discard(false)
		}
		typ = t
		typed = append(typed, m)
	}
	if len(typed) == 0 || len(typed) == 1 && nullable {
		// 可空的单个 schema 是 Copilot 的 Gemini 接受的写法，但联合旁边不能再写 type（400 invalid request body）
		if _, has := s["type"]; has && len(typed) == 1 {
			delete(s, "type")
			return discard(true)
		}
		return discard(false)
	}
	merged := map[string]any{}
	for _, m := range typed {
		for k, v := range m {
			switch k {
			case "properties":
				props, _ := merged[k].(map[string]any)
				if props == nil {
					props = map[string]any{}
				}
				pm, _ := v.(map[string]any)
				for n, pv := range pm {
					if have, has := props[n]; !has {
						props[n] = pv
					} else {
						props[n] = joinEnums(have, pv)
					}
				}
				merged[k] = props
			case "enum":
				es, _ := merged[k].([]any)
				for _, e := range asList(v) {
					dup := false
					for _, x := range es {
						dup = dup || reflect.DeepEqual(x, e)
					}
					if !dup {
						es = append(es, e)
					}
				}
				merged[k] = es
			case "required":
				// 只保留每个分支都要求的字段，见下
			default:
				if _, has := merged[k]; !has {
					merged[k] = v
				}
			}
		}
	}
	for _, m := range typed {
		if _, has := m["enum"]; !has {
			delete(merged, "enum") // 有分支没有 enum，等于接受任何值
		}
	}
	if req := commonRequired(typed); len(req) > 0 {
		merged["required"] = req
	}
	delete(s, key)
	for k, v := range merged {
		if _, has := s[k]; !has {
			s[k] = v
		}
	}
	if nullable {
		inner := map[string]any{}
		for _, k := range []string{"type", "properties", "required", "items", "enum"} {
			if v, ok := s[k]; ok {
				inner[k] = v
				delete(s, k)
			}
		}
		s[key] = []any{inner, map[string]any{"type": "null"}}
	}
	return true
}

// commonRequired 返回每个分支都 required 的字段名（排序）。
func commonRequired(typed []map[string]any) []any {
	count := map[string]int{}
	for _, m := range typed {
		seen := map[string]bool{}
		for _, r := range asList(m["required"]) {
			if n, ok := r.(string); ok && !seen[n] {
				seen[n] = true
				count[n]++
			}
		}
	}
	var names []string
	for n, c := range count {
		if c == len(typed) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

func asList(v any) []any { l, _ := v.([]any); return l }

// guessType 返回没有 type 的分支由其关键字说出的类型：有 properties 是 object，有 items 是 array，enum 全是字符串或 const 是字符串
// 是 string；说不出时返回空串。
func guessType(m map[string]any) string {
	switch {
	case m["properties"] != nil:
		return "object"
	case m["items"] != nil:
		return "array"
	}
	if es, ok := m["enum"].([]any); ok && len(es) > 0 && allStrings(es) {
		return "string"
	}
	if _, ok := m["const"].(string); ok {
		return "string"
	}
	return ""
}

// derefLocal 返回本地引用（"#/$defs/X" 或 "#/definitions/X"）在 sc.top 中指向的 schema 的拷贝；引用是其他形式、目标不存在、
// 拷贝里还有引用（环不会被展开）或内联预算已用尽（derefBudget）时返回 nil。每次成功返回消耗一份预算（调用方丢弃拷贝时负责恢复）。
// 目标的序列化结果按 $ref 缓存，每次成功仍解码出一份独立的拷贝（调用方会改写它）。
func derefLocal(sc *shapeScope, ref string) map[string]any {
	if sc.derefs <= 0 {
		return nil
	}
	t, ok := sc.targets[ref]
	if !ok || t.version != sc.version {
		t = derefTarget{raw: marshalDefinition(sc.top, ref), version: sc.version}
		sc.targets[ref] = t
	}
	if t.raw == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(t.raw))
	dec.UseNumber()
	var out map[string]any
	if dec.Decode(&out) != nil {
		return nil
	}
	sc.derefs--
	return out
}

// marshalDefinition 返回本地引用 ref 在 top 中指向的 schema 的 JSON 原文；引用是其他形式、目标不存在或其中还有 $ref 时返回 nil。
func marshalDefinition(top map[string]any, ref string) []byte {
	var defs any
	switch {
	case strings.HasPrefix(ref, "#/$defs/"):
		defs, ref = top["$defs"], strings.TrimPrefix(ref, "#/$defs/")
	case strings.HasPrefix(ref, "#/definitions/"):
		defs, ref = top["definitions"], strings.TrimPrefix(ref, "#/definitions/")
	default:
		return nil
	}
	m, _ := defs.(map[string]any)
	target, _ := m[ref].(map[string]any)
	if target == nil {
		return nil
	}
	raw, ok := marshalLiteral(target)
	if !ok || bytes.Contains(raw, []byte(`"$ref"`)) {
		return nil
	}
	return raw
}

// joinEnums 返回两个分支都有的同名属性的合并结果：二者都是字符串 enum 时取并集（判别字段的 "project" 与 "none"）；
// 有一边没有 enum 就是接受任何值，去掉 enum；类型不同时取第一个。
func joinEnums(a, b any) any {
	am, _ := a.(map[string]any)
	bm, _ := b.(map[string]any)
	// type 可能是数组（["string","null"]），不能直接用 != 比较两个 any（同为切片会 panic）
	if am == nil || bm == nil || !reflect.DeepEqual(am["type"], bm["type"]) {
		return a
	}
	ae, bem := asList(am["enum"]), asList(bm["enum"])
	_, aHas := am["enum"]
	_, bHas := bm["enum"]
	if aHas != bHas {
		out := map[string]any{}
		for k, v := range am {
			if k != "enum" {
				out[k] = v
			}
		}
		return out
	}
	if !aHas {
		return a
	}
	out := map[string]any{}
	for k, v := range am {
		out[k] = v
	}
	joined := append([]any{}, ae...)
	for _, e := range bem {
		dup := false
		for _, x := range joined {
			dup = dup || reflect.DeepEqual(x, e)
		}
		if !dup {
			joined = append(joined, e)
		}
	}
	out["enum"] = joined
	return out
}
