package copilot

import (
	"bytes"
	"encoding/json"
	"slices"
)

// withoutOutputReplay 去掉 Codex output token replay 带来的 encrypted_content：Codex 在 provider 名为 "OpenAI" 时，
// 会在顶层 include 加入 "output.encrypted_content"，并让 input 中 message、function_call 等项带上 encrypted_content，
// Copilot 的 /responses 对此返回 400（Invalid value: 'output.encrypted_content'）。Codex 对非 OpenAI provider 本就会去掉这两样
// （只保留 reasoning 与 compaction 类项的 encrypted_content），这里补上同样的处理：
//   - 顶层 include 为字符串数组时，删除其中所有 "output.encrypted_content"，删空则删去 include 键；
//   - 顶层 input 为数组时，删除对象项上除 reasoning、compaction 类（compaction、compaction_summary、context_compaction）之外的顶层 encrypted_content。
//
// 无改动、请求体不是合法 JSON 对象时返回原切片；不修改传入的切片；重编码方式同 request.go 的 withoutLoneToolChoice，且只重编码改动过的 input 项。
func withoutOutputReplay(body []byte) []byte {
	if !bytes.Contains(body, []byte("encrypted_content")) {
		return body
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return body
	}
	changed := false

	if raw, ok := request["include"]; ok {
		// 用 *string 解码：数组里出现 null、数字等非字符串元素时，null 得到 nil，其余解码失败，都视为“不是字符串数组”而不动。
		var values []*string
		if json.Unmarshal(raw, &values) == nil && !slices.Contains(values, nil) {
			kept := make([]string, 0, len(values))
			for _, value := range values {
				if *value != "output.encrypted_content" {
					kept = append(kept, *value)
				}
			}
			if len(kept) != len(values) {
				changed = true
				if len(kept) == 0 {
					delete(request, "include")
				} else if out, ok := marshalLiteral(kept); ok {
					request["include"] = out
				} else {
					return body
				}
			}
		}
	}

	if raw, ok := request["input"]; ok {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) == nil {
			itemsChanged := false
			for i, item := range items {
				if stripped, ok := withoutItemEncryptedContent(item); ok {
					items[i] = stripped
					itemsChanged = true
				}
			}
			if itemsChanged {
				out, ok := marshalLiteral(items)
				if !ok {
					return body
				}
				request["input"] = out
				changed = true
			}
		}
	}

	if !changed {
		return body
	}
	out, ok := marshalLiteral(request)
	if !ok {
		return body
	}
	return out
}

// withoutItemEncryptedContent 删除 input 对象项的顶层 encrypted_content，返回重编码后的项；
// 项不是对象、没有该键，或 type 为 reasoning、compaction 类（缺 type 或非字符串 type 不算）时 ok 为 false。
func withoutItemEncryptedContent(item json.RawMessage) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil {
		return nil, false
	}
	if _, has := fields["encrypted_content"]; !has {
		return nil, false
	}
	var typ string
	if raw, has := fields["type"]; has && json.Unmarshal(raw, &typ) == nil {
		switch typ {
		case "reasoning", "compaction", "compaction_summary", "context_compaction":
			return nil, false
		}
	}
	delete(fields, "encrypted_content")
	return marshalLiteral(fields)
}
