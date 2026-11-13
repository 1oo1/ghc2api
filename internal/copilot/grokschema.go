package copilot

// 本文件把 function 工具的 parameters 合并为根节点是 object 的 schema，移植自 magpie 的 internal/provider/grok.go 的
// ObjectRoot（objectRootIn、nestedUnion、appendDistinct、grokRef）。Copilot 前面的 Grok 后端要求 parameters 的根节点是 object
// （"tool parameter root must be an object type"），Codex desktop 的 automation_update 等工具的参数根是 anyOf/oneOf/allOf。

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// objectRoot 让工具的输入 schema ps 成为根节点是普通 object、没有 anyOf/oneOf/allOf 的 schema（就地修改），返回是否有改动：
//   - allOf 的所有分支合并，properties 与 required 都合并；
//   - anyOf 与 oneOf 的对象分支合并 properties，每个分支都要求的字段仍是必填，其余分支丢弃；
//   - 本身是联合的分支（zod 把 discriminated union 嵌在另一个里）先同样折叠，其字段才不丢；
//   - 各分支给同一字段不同 schema 时取其中任一种（anyOf 并列），根节点自己定义的字段不被覆盖。
//
// 根节点已是不含联合的 object 时不改动。
func objectRoot(ps map[string]any) bool {
	return objectRootIn(ps, ps, 0, &refExpansion{stack: map[string]bool{}, budget: objectRootRefBudget})
}

// objectRootRefBudget 是一次 objectRoot 调用最多展开的联合分支 $ref 的次数（每个工具的 parameters 各算一次调用）。
// 正常 schema（包括 Codex 的 automation_update）只展开个位数，远低于它；无环但每层扇出很大的 $defs 链
// （8 层、每层 8 个指向下一层的分支）没有预算时要展开 8^7 次，单次调用超过 10 秒。预算用尽后再遇到的 $ref 分支按不可解析处理。
const objectRootRefBudget = 256

// refExpansion 是一次 objectRoot 调用中沿递归共享的 $ref 展开状态。
type refExpansion struct {
	stack  map[string]bool // 当前展开栈：正在折叠的联合分支的 $ref；栈上已有的引用是循环引用，不再展开
	budget int             // 还能展开的 $ref 次数，见 objectRootRefBudget
}

// objectRootIn 对 schema ps 做 objectRoot；root 是 ps 里局部 $ref 所指的 $defs 所在的根 schema，depth 限制联合中的联合的展开深度。
// 上游只靠深度上限，挡不住分支多的病态 schema，这里另有两道限制，触发时该 $ref 分支按不可解析的 $ref 处理（丢弃）：
// 已在展开栈上的引用不再展开（循环引用的联合，如 $defs.a 的分支指回 a，逐层展开会指数膨胀）；
// 展开次数用尽 objectRootRefBudget 后不再展开（无环但扇出很大的链）。正常 schema 不受影响，结果与上游一致。
func objectRootIn(root, ps map[string]any, depth int, st *refExpansion) bool {
	_, hasAnyOf := ps["anyOf"]
	_, hasOneOf := ps["oneOf"]
	_, hasAllOf := ps["allOf"]
	if ps["type"] == "object" && !hasAnyOf && !hasOneOf && !hasAllOf {
		return false
	}
	props, _ := ps["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	var required []any
	if r, ok := ps["required"].([]any); ok {
		required = r
	}
	allOf, _ := ps["allOf"].([]any)
	for _, b := range allOf {
		branch, _ := b.(map[string]any)
		if branch = grokRef(root, branch); branch == nil {
			continue
		}
		branchProps, _ := branch["properties"].(map[string]any)
		for k, v := range branchProps {
			if _, ok := props[k]; !ok {
				props[k] = v
			}
		}
		branchRequired, _ := branch["required"].([]any)
		required = append(required, branchRequired...)
	}
	delete(ps, "allOf")
	var branches []map[string]any
	for _, key := range []string{"anyOf", "oneOf"} {
		list, _ := ps[key].([]any)
		for _, b := range list {
			branch, _ := b.(map[string]any)
			ref, _ := branch["$ref"].(string)
			if ref != "" && (st.stack[ref] || st.budget <= 0) {
				continue // 循环引用，或展开预算用尽
			}
			if branch = grokRef(root, branch); branch == nil {
				continue
			}
			if ref != "" {
				st.budget--
			}
			if nestedUnion(branch) && depth < 8 {
				// 在副本上折叠：分支可能是别处也引用的 $def
				branch = maps.Clone(branch)
				if ref != "" {
					st.stack[ref] = true
				}
				objectRootIn(root, branch, depth+1, st)
				delete(st.stack, ref)
			}
			if _, has := branch["properties"]; branch["type"] != "object" && !has {
				continue
			}
			branches = append(branches, branch)
		}
		delete(ps, key)
	}
	// 每个分支都要求的字段仍是必填，与根节点和 allOf 自己的 required 并列（它们任何分支都不能去掉）
	var common []any
	// 各分支给根节点没有定义的字段的 schema
	from := map[string][]any{}
	var order []string
	for i, b := range branches {
		branchProps, _ := b["properties"].(map[string]any)
		for _, k := range slices.Sorted(maps.Keys(branchProps)) {
			if _, own := props[k]; own {
				continue
			}
			if _, seen := from[k]; !seen {
				order = append(order, k)
			}
			from[k] = appendDistinct(from[k], branchProps[k])
		}
		branchRequired, _ := b["required"].([]any)
		if i == 0 {
			common = append(common, branchRequired...)
			continue
		}
		in := map[any]bool{}
		for _, r := range branchRequired {
			if hashable(r) {
				in[r] = true
			}
		}
		kept := common[:0]
		for _, r := range common {
			if hashable(r) && in[r] {
				kept = append(kept, r)
			}
		}
		common = kept
	}
	for _, k := range order {
		if schemas := from[k]; len(schemas) == 1 {
			props[k] = schemas[0]
		} else {
			props[k] = map[string]any{"anyOf": schemas}
		}
	}
	seen := map[string]bool{}
	var merged []any
	for _, r := range append(required, common...) {
		if name, ok := r.(string); ok {
			if seen[name] {
				continue
			}
			seen[name] = true
		}
		merged = append(merged, r)
	}
	required = merged
	ps["type"] = "object"
	ps["properties"] = props
	if len(required) > 0 {
		ps["required"] = required
	} else {
		delete(ps, "required")
	}
	return true
}

// hashable 报告 v 能否作 map 的键：required 里的非法条目（数组、对象）不能，否则会 panic。
func hashable(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return false
	}
	return true
}

// nestedUnion 报告联合的分支本身是否也是联合。
func nestedUnion(b map[string]any) bool {
	for _, k := range []string{"anyOf", "oneOf", "allOf"} {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}

// appendDistinct 把 v 加到 vs，已有相同的 schema 时不加。
func appendDistinct(vs []any, v any) []any {
	vb, _ := json.Marshal(v)
	for _, w := range vs {
		if wb, _ := json.Marshal(w); bytes.Equal(vb, wb) {
			return vs
		}
	}
	return append(vs, v)
}

// grokRef 返回 schema 的一个分支 b；b 是局部 $ref（#/$defs/x 或 #/definitions/x）时返回它在 root 里指向的定义，
// 指不到（外部引用、不存在）时返回 nil。
func grokRef(root, b map[string]any) map[string]any {
	ref, _ := b["$ref"].(string)
	if ref == "" {
		return b
	}
	for _, k := range []string{"$defs", "definitions"} {
		if name, ok := strings.CutPrefix(ref, "#/"+k+"/"); ok {
			defs, _ := root[k].(map[string]any)
			d, _ := defs[name].(map[string]any)
			return d
		}
	}
	return nil
}
