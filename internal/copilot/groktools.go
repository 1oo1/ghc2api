package copilot

// 本文件让 Copilot 前面的 Grok 模型接受 /responses 请求中的工具。Grok 后端对下面几类内容只回裸的 400/422
// （"422 Unprocessable Entity"，Codex 每个带工具的轮次都失败，magpie #4374、#2951、#3392）：
//   - 自定义工具（type custom，如 Codex 的 apply_patch、code mode 的 exec）；
//   - namespace 工具（如 mcp__codex_app、collaboration）；
//   - 参数根节点是 anyOf/oneOf/allOf 的 function（如 codex_app 的 automation_update）。
//
// magpie 对 Copilot 的 Grok 的做法是：自定义工具当 function 发，namespace 摊平，根联合合并为 object，模型的调用在回复里还原。
// 本项目没有翻译层，按下面的请求改写（grokRequest）与回复还原（grokreply.go）实现同样的语义。
// 移植自 magpie 的 internal/provider/grok.go（GrokFlatBody、grokFlat、flatCall、FlatName）与
// internal/gateway/responses.go（customSchema、customDescription、工具提供与历史项转换）。
// 由 handleGeneration 在 path == "/responses" 且 grokModel(model) 时调用；本文件的函数不检查模型。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// liteNamespace 是 Codex 的 Responses Lite 给自己的函数分组用的 namespace，Codex 视为没有 namespace。
const liteNamespace = "functions"

// grokModel 报告模型 id 是否是 Grok 模型：取最后一个 "/" 之后的部分（兼容 xai/grok-4.5、x-ai/grok-4 这类带厂商前缀的 id），
// 转小写后以 grok 开头。
func grokModel(id string) bool {
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	return strings.HasPrefix(strings.ToLower(id), "grok")
}

// grokCall 是请求改写后发给上游的一个工具（或历史里的调用）与客户端声明它时的样子：回复里模型对它的调用据此还原。
type grokCall struct {
	Namespace string // 客户端声明时所在的 namespace；空表示没有（或是 functions）
	Name      string // 客户端声明时的名字
	Custom    bool   // 客户端声明的是自定义工具，上游当 function 看
}

// grokCalls 是一个请求的调用表，键为发给上游的名字（摊平后的名字，或自定义工具的原名）。只属于这一个请求，不跨请求共享。
type grokCalls map[string]grokCall

// flatName 是 namespace 里的工具发给只认平名字的模型时的名字：ns__name（Codex 给 MCP 服务的工具起名的方式）。
// 超过 64 字节时截成前 55 字节，加 "_" 与整个 ns+"\x00"+name 的 sha256 前 4 字节的十六进制，使不同的名字不会撞到同一个结果。
func flatName(namespace, name string) string {
	flat := namespace + "__" + name
	if len(flat) <= 64 {
		return flat
	}
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	return flat[:55] + "_" + hex.EncodeToString(sum[:4])
}

// offeredName 返回 namespace ns 里名为 name 的工具发给上游时的名字，以及是否改了名：ns 为空或为 functions 时保持原名，其余摊平。
func offeredName(ns, name string) (offered string, renamed bool) {
	if ns == "" || ns == liteNamespace {
		return name, false
	}
	return flatName(ns, name), true
}

// customSchema 是自定义工具当 function 发时的 parameters：只有一个 input 字符串。
const customSchema = `{"type":"object","properties":{"input":{"type":"string","description":"The tool's free-form input, the whole of it, exactly as it is to be run."}},"required":["input"],"additionalProperties":false}`

// customFormat 是自定义工具的 format：text，或某种语法（lark、regex）的 grammar 与其定义。
type customFormat struct {
	Type       string `json:"type"`
	Syntax     string `json:"syntax"`
	Definition string `json:"definition"`
}

// customDescription 是自定义工具当 function 发时的 description：它自己的描述，input 是什么，以及 input 遵循的 grammar。
func customDescription(description string, format *customFormat) string {
	d := strings.TrimSpace(description)
	if d != "" {
		d += "\n\n"
	}
	d += "Put the tool's whole free-form input, as it is to be run, in the `input` string; it is passed on as written, so it is not itself JSON."
	if format != nil && format.Type == "grammar" && strings.TrimSpace(format.Definition) != "" {
		syntax := format.Syntax
		if syntax == "" {
			syntax = "the"
		}
		d += "\n\nThe input follows this " + syntax + " grammar:\n" + strings.TrimSpace(format.Definition)
	}
	return d
}

// grokMayChange 报告请求体里是否出现需要改写的内容的字样，没有就不必解析。
func grokMayChange(body []byte) bool {
	for _, marker := range []string{`"custom"`, `"namespace"`, `"anyOf"`, `"oneOf"`, `"allOf"`, `custom_tool_call`} {
		if bytes.Contains(body, []byte(marker)) {
			return true
		}
	}
	return false
}

// grokRequest 改写 /responses 请求体里 Grok 后端不接受的内容，并返回调用表（回复里据此还原模型的调用）：
//   - 顶层 tools：custom 当 function（customDescription、customSchema）；namespace 在原位置换成其内部的 function 与 custom（其他类型丢弃），
//     名字摊平为 flatName（namespace 为空或 functions 时保持原名）；function 的 parameters 做 objectRoot；其他类型不动；
//   - tool_choice：type 为 custom 或 function 的对象改为 function，有 namespace 时名字摊平并删去 namespace；allowed_tools 对其 tools 的每项同样处理；
//   - 顶层 input 的对象项：custom_tool_call 变 function_call（arguments 为 {"input": input}，删去 input、namespace、id），
//     custom_tool_call_output 变 function_call_output（删去 id），带 namespace 的 function_call 名字摊平并删去 namespace；
//     这些调用都记入调用表，即使本次请求没有再声明对应的工具。
//
// 无改动、请求体不是 JSON 对象时返回原切片与 nil；不修改传入的切片。重编码方式同 outputreplay.go：不转义 HTML、数字原文保留，
// 未改动的 tools 元素与 input 项保留原文。
func grokRequest(body []byte) ([]byte, grokCalls) {
	if !grokMayChange(body) {
		return body, nil
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return body, nil
	}
	calls := grokCalls{}
	changed := false
	if tools, ok := grokTools(request["tools"], calls); ok {
		request["tools"], changed = tools, true
	}
	if choice, ok := grokToolChoice(request["tool_choice"]); ok {
		request["tool_choice"], changed = choice, true
	}
	if input, ok := grokInput(request["input"], calls); ok {
		request["input"], changed = input, true
	}
	if !changed {
		return body, nil
	}
	out, ok := marshalLiteral(request)
	if !ok {
		return body, nil
	}
	return out, calls
}

// quoteJSON 返回字符串 s 的 JSON 字面量（不转义 HTML）。
func quoteJSON(s string) json.RawMessage {
	out, _ := marshalLiteral(s)
	return out
}

// stringValue 解码 JSON 字符串 raw；不是字符串（含缺失、null）时 ok 为 false。
func stringValue(raw json.RawMessage) (s string, ok bool) {
	ok = decodeJSONString(raw, &s)
	return s, ok
}

// grokTools 改写 tools 数组 raw，返回新的数组与是否有改动；raw 不是数组或没有改动时 ok 为 false。
func grokTools(raw json.RawMessage, calls grokCalls) (out json.RawMessage, ok bool) {
	var tools []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &tools) != nil {
		return nil, false
	}
	rewritten := make([]json.RawMessage, 0, len(tools))
	changed := false
	for _, tool := range tools {
		replacement, toolChanged := grokTool(tool, calls)
		rewritten = append(rewritten, replacement...)
		changed = changed || toolChanged
	}
	if !changed {
		return nil, false
	}
	out, ok = marshalLiteral(rewritten)
	return out, ok
}

// grokTool 改写 tools 中的一项，返回它的替换项（namespace 展开成多项，可能为空）与是否有改动。
func grokTool(tool json.RawMessage, calls grokCalls) ([]json.RawMessage, bool) {
	unchanged := []json.RawMessage{tool}
	fields := objectFields(tool)
	if fields == nil {
		return unchanged, false
	}
	typ, _ := stringValue(fields["type"])
	switch typ {
	case "custom":
		name, _ := stringValue(fields["name"])
		if name == "" {
			return unchanged, false
		}
		calls[name] = grokCall{Name: name, Custom: true}
		return []json.RawMessage{customFunction(name, fields)}, true
	case "namespace":
		return grokNamespace(fields, calls), true
	case "function":
		if applyObjectRoot(fields) {
			rewritten, ok := marshalLiteral(fields)
			return []json.RawMessage{rewritten}, ok
		}
	}
	return unchanged, false
}

// customFunction 把自定义工具（字段 fields）改成名为 name 的 function。原有的 format 等字段不再保留，已折进 description。
func customFunction(name string, fields map[string]json.RawMessage) json.RawMessage {
	description, _ := stringValue(fields["description"])
	var format *customFormat
	if firstByte(fields["format"]) == '{' {
		format = new(customFormat)
		if json.Unmarshal(fields["format"], format) != nil {
			format = nil
		}
	}
	out, _ := marshalLiteral(struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}{"function", name, customDescription(description, format), json.RawMessage(customSchema)})
	return out
}

// applyObjectRoot 在 function 工具（字段 fields）的 parameters 是对象时做 objectRoot，把结果写回 fields["parameters"]，返回是否有改动。
func applyObjectRoot(fields map[string]json.RawMessage) bool {
	ps, ok := decodeObject(fields["parameters"])
	if !ok || !objectRoot(ps) {
		return false
	}
	parameters, ok := marshalLiteral(ps)
	if !ok {
		return false
	}
	fields["parameters"] = parameters
	return true
}

// grokNamespace 把 namespace 工具（字段 fields）展开成其内部的 function 与 custom（其他类型丢弃，没有名字的也丢弃），
// 名字按 offeredName 处理；改了名的、以及自定义工具，记入调用表。
func grokNamespace(fields map[string]json.RawMessage, calls grokCalls) []json.RawMessage {
	space, _ := stringValue(fields["name"])
	var inner []json.RawMessage
	_ = json.Unmarshal(fields["tools"], &inner)
	var out []json.RawMessage
	for _, tool := range inner {
		toolFields := objectFields(tool)
		name, _ := stringValue(toolFields["name"])
		typ, _ := stringValue(toolFields["type"])
		if toolFields == nil || name == "" || (typ != "function" && typ != "custom") {
			continue
		}
		offered, renamed := offeredName(space, name)
		custom := typ == "custom"
		switch {
		case renamed:
			calls[offered] = grokCall{Namespace: space, Name: name, Custom: custom}
		case custom:
			calls[offered] = grokCall{Name: name, Custom: true}
		}
		if custom {
			out = append(out, customFunction(offered, toolFields))
			continue
		}
		function := tool
		if renamed {
			toolFields["name"] = quoteJSON(offered)
		}
		if applyObjectRoot(toolFields) || renamed {
			function, _ = marshalLiteral(toolFields)
		}
		out = append(out, function)
	}
	return out
}

// grokToolChoice 改写 tool_choice：type 为 custom 或 function 的对象改为 function，有 namespace 时名字摊平并删去 namespace；
// type 为 allowed_tools 时对其 tools 中 type 为 custom 或 function 的每项同样处理。字符串形式与其他类型不动。
// 没有改动时 ok 为 false。
func grokToolChoice(raw json.RawMessage) (json.RawMessage, bool) {
	fields := objectFields(raw)
	if fields == nil {
		return nil, false
	}
	changed := false
	typ, _ := stringValue(fields["type"])
	switch typ {
	case "custom", "function":
		changed = flatChoice(fields)
	case "allowed_tools":
		var tools []json.RawMessage
		if json.Unmarshal(fields["tools"], &tools) != nil {
			return nil, false
		}
		toolsChanged := false
		for i, tool := range tools {
			toolFields := objectFields(tool)
			if kind, _ := stringValue(toolFields["type"]); (kind == "custom" || kind == "function") && flatChoice(toolFields) {
				tools[i], _ = marshalLiteral(toolFields)
				toolsChanged = true
			}
		}
		if toolsChanged {
			fields["tools"], _ = marshalLiteral(tools)
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	out, ok := marshalLiteral(fields)
	return out, ok
}

// flatChoice 把 tool_choice 里指向工具的对象（字段 fields）改成 function 的形式，返回是否有改动：custom 改为 function，
// 有 namespace（字符串）时删去它，并把名字按 offeredName 摊平。
func flatChoice(fields map[string]json.RawMessage) bool {
	changed := false
	if typ, _ := stringValue(fields["type"]); typ == "custom" {
		fields["type"] = quoteJSON("function")
		changed = true
	}
	if space, ok := stringValue(fields["namespace"]); ok {
		delete(fields, "namespace")
		if name, _ := stringValue(fields["name"]); name != "" {
			if offered, renamed := offeredName(space, name); renamed {
				fields["name"] = quoteJSON(offered)
			}
		}
		changed = true
	}
	return changed
}

// grokInput 改写 input 数组 raw 中的 custom_tool_call、custom_tool_call_output 与带 namespace 的 function_call，
// 并把其中的调用记入调用表；raw 不是数组或没有改动时 ok 为 false。
func grokInput(raw json.RawMessage, calls grokCalls) (json.RawMessage, bool) {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	changed := false
	for i, item := range items {
		if rewritten, ok := grokInputItem(item, calls); ok {
			items[i], changed = rewritten, true
		}
	}
	if !changed {
		return nil, false
	}
	out, ok := marshalLiteral(items)
	return out, ok
}

// grokInputItem 改写一个 input 项；不是需要改写的项（或没有改动）时 ok 为 false。
func grokInputItem(item json.RawMessage, calls grokCalls) (json.RawMessage, bool) {
	if !bytes.Contains(item, []byte("custom_tool_call")) && !bytes.Contains(item, []byte(`"namespace"`)) {
		return nil, false
	}
	fields := objectFields(item)
	typ, _ := stringValue(fields["type"])
	switch typ {
	case "custom_tool_call":
		name, _ := stringValue(fields["name"])
		space, _ := stringValue(fields["namespace"])
		input, _ := stringValue(fields["input"])
		offered, renamed := offeredName(space, name)
		if name != "" {
			call := grokCall{Name: name, Custom: true}
			if renamed {
				call.Namespace = space
			}
			calls[offered] = call
		}
		arguments, _ := marshalLiteral(map[string]string{"input": input})
		fields["type"], fields["name"], fields["arguments"] = quoteJSON("function_call"), quoteJSON(offered), quoteJSON(string(arguments))
		delete(fields, "input")
		delete(fields, "namespace")
		delete(fields, "id")
	case "custom_tool_call_output":
		fields["type"] = quoteJSON("function_call_output")
		delete(fields, "id")
	case "function_call":
		space, ok := stringValue(fields["namespace"])
		if !ok {
			return nil, false
		}
		delete(fields, "namespace")
		if name, _ := stringValue(fields["name"]); name != "" {
			if offered, renamed := offeredName(space, name); renamed {
				fields["name"] = quoteJSON(offered)
				calls[offered] = grokCall{Namespace: space, Name: name}
			}
		}
	default:
		return nil, false
	}
	out, ok := marshalLiteral(fields)
	return out, ok
}
