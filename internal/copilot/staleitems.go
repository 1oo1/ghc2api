package copilot

// 本文件处理上游 /responses 因“输出项绑定在原连接上”而拒绝回放的情况。
//
// Copilot /responses 返回的输出项 id（以及 reasoning 项的 encrypted_content）绑定在产生它的上游连接上。
// 客户端回放这些项时，若请求落到另一个账号或连接已变，上游会返回：
//   - 401 "input item does not belong to this connection"；
//   - 400 "The encrypted content for item rs_... could not be verified. ..."。
//
// 这是请求内容与连接不匹配，不是账号凭证问题：Service 去掉这些项后在同一账号、同一 session、同一模型上重发一次；
// 无法重试或重试后仍失败时返回上游响应（401 改为 500，正文不变），但不计账号错误、不冷却、不写风控记录（见 retryStaleItems）。
// 401 改写为 500 是因为服务前端对过多的 401 会封禁客户端 IP，而这类错误不代表客户端鉴权失败。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// staleItemError 报告上游响应是否为“输入项与连接不匹配”：
// 401 且正文含 "does not belong to this connection"，或 400 且正文同时含 "encrypted content" 与 "could not be verified"。
// 正文匹配不区分大小写。
func staleItemError(status int, body []byte) bool {
	text := strings.ToLower(string(body))
	switch status {
	case http.StatusUnauthorized:
		return strings.Contains(text, "does not belong to this connection")
	case http.StatusBadRequest:
		return strings.Contains(text, "encrypted content") && strings.Contains(text, "could not be verified")
	}
	return false
}

// withoutConnectionItems 去掉 input 数组中与连接绑定的内容：丢弃 type 为 reasoning 或 item_reference 的对象项，
// 其余对象项删除顶层 id，非对象元素原样保留。请求体不是 JSON 对象、input 不是数组或没有任何改动时返回原 body 与 false。
// 不修改传入的切片；重编码方式同 withoutLoneToolChoice（数字原文保留，不转义 HTML）。
func withoutConnectionItems(body []byte) ([]byte, bool) {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return body, false
	}
	var items []json.RawMessage
	if json.Unmarshal(request["input"], &items) != nil || items == nil {
		return body, false
	}
	cleaned := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, raw := range items {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			cleaned = append(cleaned, raw) // 非对象元素原样保留
			continue
		}
		var itemType string
		_ = json.Unmarshal(object["type"], &itemType)
		if itemType == "reasoning" || itemType == "item_reference" {
			changed = true
			continue
		}
		if _, ok := object["id"]; ok {
			delete(object, "id")
			encoded, ok := marshalLiteral(object)
			if !ok {
				return body, false
			}
			raw = encoded
			changed = true
		}
		cleaned = append(cleaned, raw)
	}
	if !changed {
		return body, false
	}
	input, ok := marshalLiteral(cleaned)
	if !ok {
		return body, false
	}
	request["input"] = input
	out, ok := marshalLiteral(request)
	if !ok {
		return body, false
	}
	return out, true
}

// errBody 是读取即返回固定错误的响应正文，用于保留 peekErrorBody 遇到的读取错误。
type errBody struct{ err error }

func (e errBody) Read([]byte) (int, error) { return 0, e.err }
func (e errBody) Close() error             { return nil }

// peekErrorBody 读取 400/401 响应的正文用于判断，并把正文放回 res.Body，后续处理照常读取。
// 读取失败时返回空正文，res.Body 改为再次读取即返回同一错误，使后续分支仍能看到读取失败。
func peekErrorBody(res *http.Response) []byte {
	data, err := readLimited(res.Body, maxMetadataBody)
	_ = res.Body.Close()
	if err != nil {
		res.Body = errBody{err}
		return nil
	}
	res.Body = io.NopCloser(bytes.NewReader(data))
	return data
}

// rejectedStaleItems 报告 /responses 的响应是否为失效项错误；检查后 res.Body 仍可读取。
func rejectedStaleItems(res *http.Response) bool {
	if res.StatusCode != http.StatusUnauthorized && res.StatusCode != http.StatusBadRequest {
		return false
	}
	return staleItemError(res.StatusCode, peekErrorBody(res))
}

// retryStaleItems 处理上游 /responses 的首次响应 res：不是失效项错误时原样返回；
// 是则清理 outBody 并用 send 重发一次（同一账号、session 与模型，头部与首次一致），因此每个请求最多重试一次。
// 最终响应仍是失效项错误（无法清理，或重试后仍是）时由 ignoreStaleItems 标记 failureIgnored 并把 401 改为 500：
// 正文原样返回给客户端，但不计账号错误、不冷却、不写风控记录。
func (s *Service) retryStaleItems(ctx context.Context, w http.ResponseWriter, res *http.Response, outBody []byte, model string,
	send func(body []byte) (*http.Response, error)) (*http.Response, error) {
	if !rejectedStaleItems(res) {
		return res, nil
	}
	cleaned, canRetry := withoutConnectionItems(outBody)
	// 日志不含请求正文。
	s.log(ctx, slog.LevelWarn, "上游拒绝了与连接绑定的输入项", "status", res.StatusCode, "model", model, "retry", canRetry)
	if !canRetry {
		ignoreStaleItems(w, res)
		return res, nil
	}
	_ = res.Body.Close()
	retry, err := send(cleaned)
	if err != nil {
		return nil, err
	}
	if rejectedStaleItems(retry) {
		s.log(ctx, slog.LevelWarn, "清理输入项后上游仍拒绝", "status", retry.StatusCode, "model", model, "retry", false)
		ignoreStaleItems(w, retry)
	}
	return retry, nil
}

// ignoreStaleItems 把最终仍是失效项错误的响应 res 标记为 failureIgnored，并把 401 改为 500（400 不变）：
// 服务前端会因过多的 401 封禁客户端 IP，而这里的 401 只是输入项与连接不匹配。正文与其他头部不变。
func ignoreStaleItems(w http.ResponseWriter, res *http.Response) {
	markFailure(w, failureIgnored)
	if res.StatusCode == http.StatusUnauthorized {
		res.StatusCode = http.StatusInternalServerError
		res.Status = "500 Internal Server Error"
	}
}
