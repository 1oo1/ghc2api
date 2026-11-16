package copilot

// 本文件把一个 JSON 值解析成带位置的节点树（jsonNode），给需要在原文上就地改写的代码使用（见 geminitypes.go：
// 给工具 schema 缺 type 的节点在左花括号后插入 type，其余字节不动）。整棵树一次扫描建成，之后按键查找、遍历都不再回头扫描原文。
// 只用于体积很小的值（工具 schema）；请求体、messages 这类可能很大的部分仍用 jsonscan.go 的按需定位，不建树。
// 调用方须保证输入已通过 json.Valid；对非法输入返回 nil 而不 panic。

import "strconv"

// maxJSONTreeDepth 是建树时容器的最大嵌套深度，与 encoding/json 的上限一致；合法 JSON 不会超过它，限制只为非法输入时不耗尽栈。
const maxJSONTreeDepth = 10000

// jsonNode 是 JSON 值在原文中的位置，以及容器的子节点。
type jsonNode struct {
	// kind 是值的种类：'{' 对象、'[' 数组、'"' 字符串、'0' 数字、't' true、'f' false、'n' null。
	kind byte
	at   int         // 值在建树所用原文中的起始下标
	raw  []byte      // 值的原文（原文的子切片，不含前后空白）
	keys []jsonKey   // 对象成员的键，与 kids 一一对应；其他种类为 nil
	kids []*jsonNode // 对象的成员值（保持原有顺序，重复的键都在）或数组的元素；标量为 nil
}

// jsonKey 是对象成员的键。
type jsonKey struct {
	name string // 解码后的键
	raw  []byte // 键的原文（含引号）
}

// parseJSONTree 把 raw 解析成节点树，raw 须整体是一个 JSON 值（前后可有空白）；解析失败时返回 nil。
func parseJSONTree(raw []byte) *jsonNode {
	n, end := parseJSONNode(raw, skipJSONSpace(raw, 0), 0)
	if n == nil || skipJSONSpace(raw, end) != len(raw) {
		return nil
	}
	return n
}

// parseJSONNode 解析 raw 中下标 i 处的一个值，返回节点与值之后的下标；失败时节点为 nil。
func parseJSONNode(raw []byte, i, depth int) (*jsonNode, int) {
	if i >= len(raw) || depth > maxJSONTreeDepth {
		return nil, i
	}
	n := &jsonNode{at: i, kind: raw[i]}
	switch raw[i] {
	case '{', '[':
		isObject, closer := raw[i] == '{', byte(']')
		if isObject {
			closer = '}'
		}
		i = skipJSONSpace(raw, i+1)
		for i < len(raw) && raw[i] != closer {
			if isObject {
				if raw[i] != '"' {
					return nil, i
				}
				keyEnd := jsonStringEnd(raw, i) + 1
				key := jsonKey{name: decodeKey(raw[i:keyEnd]), raw: raw[i:keyEnd]}
				i = skipJSONSpace(raw, keyEnd)
				if i >= len(raw) || raw[i] != ':' {
					return nil, i
				}
				i = skipJSONSpace(raw, i+1)
				n.keys = append(n.keys, key)
			}
			child, end := parseJSONNode(raw, i, depth+1)
			if child == nil {
				return nil, end
			}
			n.kids = append(n.kids, child)
			i = skipJSONSpace(raw, end)
			if i < len(raw) && raw[i] == ',' {
				i = skipJSONSpace(raw, i+1)
			}
		}
		if i >= len(raw) {
			return nil, i
		}
		i++
	case '"':
		end := jsonStringEnd(raw, i)
		if end <= i || raw[end] != '"' {
			return nil, i
		}
		i = end + 1
	default:
		start := i
		for i < len(raw) && !isJSONLiteralEnd(raw[i]) {
			i++
		}
		if i == start {
			return nil, i
		}
		if c := raw[start]; c != 't' && c != 'f' && c != 'n' {
			n.kind = '0'
		}
	}
	n.raw = raw[n.at:i]
	return n, i
}

// isJSONLiteralEnd 报告 c 是否结束一个数字、true、false、null 字面量：逗号、外层的结束括号或空白。
func isJSONLiteralEnd(c byte) bool {
	switch c {
	case ',', ']', '}', ' ', '\t', '\r', '\n':
		return true
	}
	return false
}

// get 返回对象的第一个名为 name 的成员值；n 为 nil、不是对象或没有该成员时返回 nil。
func (n *jsonNode) get(name string) *jsonNode {
	if n == nil || n.kind != '{' {
		return nil
	}
	for i, k := range n.keys {
		if k.name == name {
			return n.kids[i]
		}
	}
	return nil
}

// isObject 报告 n 是否是对象。
func (n *jsonNode) isObject() bool { return n != nil && n.kind == '{' }

// isArray 报告 n 是否是数组。
func (n *jsonNode) isArray() bool { return n != nil && n.kind == '[' }

// elements 返回 n 作为列表时的元素：数组是它的元素，缺失与 null 是空，其他值（对象、标量）是只含它自己的列表。
func (n *jsonNode) elements() []*jsonNode {
	switch {
	case n == nil || n.kind == 'n':
		return nil
	case n.kind == '[':
		return n.kids
	}
	return []*jsonNode{n}
}

// text 返回节点的文本：字符串是解码后的值，null 是空串，其他种类是原文。
func (n *jsonNode) text() string {
	switch n.kind {
	case '"':
		return decodeKey(n.raw)
	case 'n':
		return ""
	}
	return string(n.raw)
}

// pointer 按 JSON 指针的各段在 n 之下取值：对象按键，数组按下标；路径上有一段取不到时返回 nil。
func (n *jsonNode) pointer(tokens []string) *jsonNode {
	for _, tok := range tokens {
		switch {
		case n.isObject():
			n = n.get(tok)
		case n.isArray():
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(n.kids) {
				return nil
			}
			n = n.kids[i]
		default:
			return nil
		}
		if n == nil {
			return nil
		}
	}
	return n
}
