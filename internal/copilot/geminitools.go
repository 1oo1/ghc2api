package copilot

// 本文件让 Copilot 的 Gemini 模型接受 /chat/completions 请求中的工具 schema。Copilot 的 Gemini 对某些工具 schema 整个请求返回 400
// "invalid request body"，请求体里的其他内容没有问题。适配分三步，由 handleGeneration 在 /chat/completions 且 geminiModel(model) 时
// 依次调用，函数本身不检查模型：
//  1. geminiToolsFit（本文件）：工具 schema 的 required 列出了其 properties 里没有的字段（MCP 服务的 schema 常见），或两个工具同名；
//  2. geminiToolShapes（geminishapes.go）：几种形状的 schema（无 properties 的对象、分支同类型的 anyOf、数组上的 enum、根上的联合、
//     元组数组等）改写成 Gemini 接受的最接近的 schema；
//  3. geminiToolTypes（geminitypes.go）：缺 type 的 schema 补上 type（Vertex 要求，缺一处整个请求 400）。
//
// 注意：不把 anyOf 改名为 oneOf——Copilot 的 Gemini 对 oneOf 返回 200 却丢掉分支（模型于是发出字符串），
// 所以可证明不相交的对象联合由第 2 步折叠成一个对象。
// 关于非法输入：geminiToolsFit 不校验整个请求体（handleGeneration 解析 model 时已校验），只在 tools 数组本身非法时返回原切片；
// geminiToolShapes、geminiToolTypes 经 geminiToolsArray 先校验整个请求体，非法时返回原切片。三个函数对非法输入都不 panic。

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
	return replaceTools(body, toolsAt, len(tools), fitted)
}

// geminiToolsArray 返回请求体顶层 tools 数组的原文及其在 body 中的下标，供 geminiToolShapes、geminiToolTypes 使用；
// 请求体不是合法的 JSON 对象、没有 tools 或其值不是数组时返回 nil 切片（调用方应原样返回请求体）。
func geminiToolsArray(body []byte) (at int, tools []byte) {
	at, tools = topLevelArray(body, "tools")
	if tools == nil || !json.Valid(body) {
		return 0, nil
	}
	return at, tools
}

// replaceTools 返回 body 的副本，其中从 at 起长 n 字节的 tools 数组原文换成 tools，其余字节不变。
func replaceTools(body []byte, at, n int, tools []byte) []byte {
	out := make([]byte, 0, len(body)-n+len(tools))
	out = append(out, body[:at]...)
	out = append(out, tools...)
	return append(out, body[at+n:]...)
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
