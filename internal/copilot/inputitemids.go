package copilot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// itemIDCap 是 /responses 输入项 id 的最大字节数：Copilot（及其背后的 OpenAI）对更长的 id 返回
// "string too long ... maximum length 64"，或空消息的 400 invalid_request_body。
const itemIDCap = 64

// openaiItemPrefix 是 OpenAI 给两类调用项 id 的前缀。这两类都属于 idOptional，超长时只会删去 id，实际用不到该表；
// 与上游 magpie 保持一致，boundItemID 仍先查它。
var openaiItemPrefix = map[string]string{
	"tool_search_call": "tsc_",
	"custom_tool_call": "ctc_",
}

// idOptional 是 id 可以省略的输入项类型（"" 表示缺 type 的 message）。这些类型的 id 超长时直接删去：
// Copilot 对省略 id 的请求返回 200，而把超长 id 换成无前缀的摘要仍会被整个拒绝。调用与其输出靠 call_id 配对，不受影响。
var idOptional = map[string]bool{
	"": true, "message": true,
	"function_call": true, "function_call_output": true,
	"custom_tool_call": true, "custom_tool_call_output": true,
	"tool_search_call": true, "tool_search_output": true,
	"computer_call_output": true,
	"shell_call":           true, "shell_call_output": true,
	"apply_patch_call": true, "apply_patch_call_output": true,
	"mcp_approval_response": true,
	"additional_tools":      true,
	"compaction":            true,
}

// itemKindPrefix 是必须带 id 的输入项类型的 id 前缀。Copilot 给这些项发的 id 是 450 字符左右、没有类型前缀的 Base64，
// 超长时 boundItemID 用这里的前缀绑定。不在表中的类型（如 item_reference）沿用 id 自带的前缀。
var itemKindPrefix = map[string]string{
	"reasoning":             "rs_",
	"web_search_call":       "ws_",
	"file_search_call":      "fs_",
	"computer_call":         "cu_",
	"image_generation_call": "ig_",
	"code_interpreter_call": "ci_",
	"mcp_call":              "mcp_",
	"mcp_list_tools":        "mcpl_",
	"mcp_approval_request":  "mcpr_",
}

// plainItemIDs 让 /responses 请求体 input 中各项的 id 都是 [A-Za-z0-9_-] 且不超过 itemIDCap 字节，其余字节保持不变。
// Copilot 给输出项发的 id 很长（含 + 与 /），Codex 下一轮原样回传，别家厂商的 id 还可能含冒号等字符，
// 上游会因此拒绝整个请求。只看顶层 input 数组中 id 为非空字符串的对象项，规则依次为：
//   - id 不超过 itemIDCap 字节且只含合法字符：不动；
//   - type 为 reasoning 且 id 含合法字符以外的字符：整项删除（别家的 reasoning，其 encrypted_content 本服务也读不了）；
//   - id 超过 itemIDCap：idOptional 类型删去 id；其余类型改为 boundItemID（类型前缀 + sha256 摘要）；
//   - 其他（不超长但含非法字符）：每个非法字符换成 "_"。
//
// call_id 一律不动。无改动、请求体不是 JSON 对象、input 不是数组时返回原切片；不修改传入的切片。
// 重编码方式同 request.go 的 withoutLoneToolChoice：不转义 HTML、数字原文保留，只重编码改动过的项与顶层对象。
func plainItemIDs(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"id"`)) {
		return body
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return body
	}
	var items []json.RawMessage
	if json.Unmarshal(request["input"], &items) != nil {
		return body
	}
	changed := false
	kept := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		out, keep, itemChanged := plainItemID(item)
		changed = changed || itemChanged
		if keep {
			kept = append(kept, out)
		}
	}
	if !changed {
		return body
	}
	input, ok := marshalLiteral(kept)
	if !ok {
		return body
	}
	request["input"] = input
	out, ok := marshalLiteral(request)
	if !ok {
		return body
	}
	return out
}

// plainItemID 按 plainItemIDs 的规则处理一个 input 项，返回处理后的项、是否保留、是否有改动。
// 项不是对象、没有字符串 id、id 为空或已合规时原样保留；重编码失败时也原样保留。
func plainItemID(item json.RawMessage) (out json.RawMessage, keep, changed bool) {
	if !bytes.Contains(item, []byte(`"id"`)) {
		return item, true, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil {
		return item, true, false
	}
	var id string
	if raw := fields["id"]; len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &id) != nil || id == "" {
		return item, true, false
	}
	safe := safeItemID(id)
	if len(id) <= itemIDCap && safe {
		return item, true, false
	}
	var typ string
	if raw := fields["type"]; len(raw) > 0 && raw[0] == '"' {
		_ = json.Unmarshal(raw, &typ) // 非字符串 type 视为 ""
	}
	if typ == "reasoning" && !safe {
		return nil, false, true
	}
	switch {
	case len(id) > itemIDCap && idOptional[typ]:
		delete(fields, "id")
	case len(id) > itemIDCap:
		fields["id"], _ = marshalLiteral(boundItemID(id, typ))
	default:
		fields["id"], _ = marshalLiteral(strings.Map(replaceUnsafeIDChar, id))
	}
	rewritten, ok := marshalLiteral(fields)
	if !ok {
		return item, true, false
	}
	return rewritten, true, true
}

// safeItemID 报告 id 是否只含 [A-Za-z0-9_-]。
func safeItemID(id string) bool {
	for i := 0; i < len(id); i++ {
		if !safeIDByte(id[i]) {
			return false
		}
	}
	return true
}

func safeIDByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// replaceUnsafeIDChar 把 id 中的非法字符（按字符，不是按字节）换成 '_'。
func replaceUnsafeIDChar(r rune) rune {
	if r < 0x80 && safeIDByte(byte(r)) {
		return r
	}
	return '_'
}

// boundItemID 把超长的 id 绑定为不超过 itemIDCap 字节的 id：类型前缀 + sha256(id) 的十六进制，截断到总长 itemIDCap。
// 同一 id 每次得到同一结果，不同 id 不会撞到同一结果（直接截取 id 前部则可能撞，别家厂商的 id 可以有相同的开头）。
// 前缀依次取 openaiItemPrefix、itemKindPrefix；都没有时，id 的第一个 "_" 在 1 到 4 字节之内且其前只含合法字符，
// 则取 id 到该 "_"（含）为前缀，否则不加前缀。Copilot 自己的不透明 id 没有前缀，只有类型能给出前缀，
// 纯摘要的 id 虽然长度与字符都合规，仍会被整个拒绝。
func boundItemID(id, typ string) string {
	sum := sha256.Sum256([]byte(id))
	digest := hex.EncodeToString(sum[:])
	prefix := openaiItemPrefix[typ]
	if prefix == "" {
		prefix = itemKindPrefix[typ]
	}
	if prefix == "" {
		if i := strings.IndexByte(id, '_'); i > 0 && i <= 4 && safeItemID(id[:i]) {
			prefix = id[:i+1]
		}
	}
	return prefix + digest[:itemIDCap-len(prefix)]
}
