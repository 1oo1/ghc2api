package copilot

// 本文件覆盖 /responses 输入项 id 的规整（plainItemIDs，见 inputitemids.go）。
//
// 背景：Copilot 会给输出项发 ~450 字符的不透明 id（Base64，含 + 与 /，无类型前缀），Codex 下一轮原样回传时，
// Copilot 以空消息的 400 {"error":{"message":"","code":"invalid_request_body"}} 或 "string too long ... maximum length 64"
// 拒绝整个请求；别家厂商的 id 也可能含冒号等其他字符。规则（与 magpie 的 plainItemIDs 一致）：
//   - 只看顶层 input 数组中 id 为非空字符串的对象项；id 不超过 64 字节且只含 [A-Za-z0-9_-] 的项不动；
//   - type 为 reasoning 且 id 含其他字符：整项删除（别家的 reasoning，encrypted_content 也读不了）；
//   - 否则 id 超过 64 字节：idOptional 类型（含缺 type）删去 id，其余按类型前缀 + sha256 十六进制绑定到总长 64；
//   - 否则（不超过 64 字节但含其他字符）：每个其他字符换成 "_"；
//   - call_id 一律不动；无改动、请求体不是 JSON 对象、input 不是数组时返回原切片，且不修改传入切片；
//   - 重编码同 withoutLoneToolChoice：数字原文保留、不转义 HTML，只重编码改动过的项与顶层对象。
//
// 实现者必须提供的内部名字（测试按此书写）：
//   - func plainItemIDs(body []byte) []byte
//
// 期望值参照 magpie internal/gateway/item_ids_test.go（TestPlainItemIDsBoundItemIDs、TestPlainItemIDsBindsOtherKinds、
// TestCopilotTakesItsOwnOpaqueCallID、TestCopilotTakesItsOwnOpaqueSearchCallID 等）。

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// itemIDChars 是上游 /responses 接受的 id 字符集。
var itemIDChars = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// itemIDOpaque 是 Copilot 在直连路径上发出的函数调用 id：339 字节的标准 Base64（452 字符），含 + 与 /，没有 fc_ 前缀。
var itemIDOpaque = func() string {
	b := make([]byte, 339)
	for i := range b {
		b[i] = byte(i*131 + 7)
	}
	return base64.StdEncoding.EncodeToString(b)
}()

// itemIDOpaqueSearch 是另一个 452 字符的 Base64 id（web_search_call 用），与 itemIDOpaque 不同。
var itemIDOpaqueSearch = func() string {
	b := make([]byte, 339)
	for i := range b {
		b[i] = byte(i*197 + 3)
	}
	return base64.StdEncoding.EncodeToString(b)
}()

const (
	// itemIDMuse 是别家厂商（Muse Spark）的 reasoning id：两个 id 用冒号相连，63 字节，含冒号。
	itemIDMuse = "rs_6aca2fc7533c979d5f0e4f7a:rs_01a125caa544754e9a3c70080100b009"
	// itemIDMuseCall 是同一轮 function_call 的 id（63 字节，含冒号）。
	itemIDMuseCall = "fc_6aca2fc7533c979d5f0e4f7b:fc_01a125caa544754e9a3c70080100b00a"
	// itemIDMuseMessage 是同一轮 message 的 id：65 字节，含冒号，超过 64。
	itemIDMuseMessage = "msg_6aca2fc7533c979d5f0e4f7c:msg_01a125caa544754e9a3c70080100b00b"
)

// itemIDDigestID 按规格计算绑定后的 id：prefix + sha256(id) 的十六进制，截断到总长 64。
func itemIDDigestID(prefix, id string) string {
	sum := sha256.Sum256([]byte(id))
	return prefix + hex.EncodeToString(sum[:])[:64-len(prefix)]
}

// itemIDQuote 返回 s 的 JSON 字符串字面量（不转义 HTML）。
func itemIDQuote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// runPlainItemIDs 调用 plainItemIDs，并断言输入切片未被修改。
func runPlainItemIDs(t *testing.T, body string) []byte {
	t.Helper()
	in := []byte(body)
	original := append([]byte(nil), in...)
	out := plainItemIDs(in)
	if !bytes.Equal(in, original) {
		t.Fatalf("输入切片被修改:\n got=%s\nwant=%s", in, original)
	}
	return out
}

// assertSameSlice 断言 got 是 want 的同一个切片（同内容、同底层数组起点），即 "返回原切片"。
func assertSameSlice(t *testing.T, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("请求体应原样返回:\n got=%s\nwant=%s", got, want)
	}
	if len(got) > 0 && &got[0] != &want[0] {
		t.Fatalf("应返回传入的原切片，而不是内容相同的副本")
	}
}

// itemIDInput 解析输出请求体的 input 数组（数字保持原文）。
func itemIDInput(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	raw, ok := decodeNumberMap(t, body)["input"].([]any)
	if !ok {
		t.Fatalf("输出的 input 不是数组: %s", body)
	}
	items := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		item, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("input 项不是对象: %v", v)
		}
		items = append(items, item)
	}
	return items
}

// TestPlainItemIDsLeavesAcceptableRequestsAlone 没有需要改的 id、input 不是数组、请求体不是 JSON 对象时，
// 返回传入的原切片（逐字节一致），不修改输入。
func TestPlainItemIDsLeavesAcceptableRequestsAlone(t *testing.T) {
	long := strings.Repeat("A", 300)
	at64 := "msg_" + strings.Repeat("a", 60)
	for _, tc := range []struct{ name, body string }{
		{"id-of-exactly-64-chars", `{"model":"m","input":[{"type":"message","role":"user","id":"` + at64 + `","content":"hi"}]}`},
		{"short-ids", `{"model":"m","input":[{"type":"message","id":"msg_1","role":"user","content":"hi"},{"type":"reasoning","id":"rs_1","summary":[]}]}`},
		{"every-allowed-char", `{"model":"m","input":[{"type":"message","id":"AZaz09_-","role":"user","content":"hi"}]}`},
		{"empty-id", `{"model":"m","input":[{"type":"message","id":"","role":"user","content":"hi"}]}`},
		{"non-string-id", `{"model":"m","input":[{"type":"message","id":123},{"type":"message","id":null},{"type":"message","id":{"a":"` + long + `"}},{"type":"message","id":["` + long + `"]}]}`},
		{"no-id", `{"model":"m","input":[{"type":"message","role":"user","content":"hi"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`},
		{"non-object-items", `{"model":"m","input":["` + long + `",1,null,true,[{"id":"` + long + `"}]]}`},
		{"nested-ids-untouched", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","id":"` + long + `:x","text":"hi"}]},` +
			`{"type":"function_call_output","call_id":"c1","output":{"id":"` + long + `","items":[{"id":"a:b"}]}}]}`},
		{"top-level-id-untouched", `{"id":"` + long + `:x","model":"m","input":[{"type":"message","id":"msg_1"}]}`},
		{"metadata-input-untouched", `{"model":"m","metadata":{"input":[{"type":"message","id":"` + long + `"}]},"input":"hi"}`},
		{"call-id-never-touched", `{"model":"m","input":[{"type":"function_call","id":"fc_1","call_id":"` + long + `:/+","name":"f","arguments":"{}"}]}`},
		{"input-empty-array", `{"model":"m","input":[]}`},
		{"input-string", `{"model":"m","input":"hi","id":"` + long + `"}`},
		{"input-object", `{"model":"m","input":{"type":"message","id":"` + long + `"}}`},
		{"input-null", `{"model":"m","input":null}`},
		{"input-number", `{"model":"m","input":1}`},
		{"input-absent", `{"model":"m","previous_response_id":"resp_1"}`},
		{"spaced-json-untouched", "{ \"model\" : \"m\" ,\n \"input\" : [ { \"type\" : \"message\" , \"id\" : \"msg_1\" , \"n\" : 1.0 } ] }"},
		{"invalid-json", `{"model":"m","input":[{"type":"message","id":"` + long + `"}]`},
		{"invalid-json-trailing", `{"model":"m","input":[{"type":"message","id":"` + long + `"}]} x`},
		{"json-array", `[{"type":"message","id":"` + long + `"}]`},
		{"json-string", `"id"`},
		{"json-null", `null`},
		{"not-json", `not json "id"`},
		{"empty", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.body)
			original := append([]byte(nil), in...)
			out := plainItemIDs(in)
			if !bytes.Equal(in, original) {
				t.Fatalf("输入切片被修改:\n got=%s\nwant=%s", in, original)
			}
			assertSameSlice(t, out, in)
		})
	}
	t.Run("nil", func(t *testing.T) {
		if out := plainItemIDs(nil); len(out) != 0 {
			t.Fatalf("nil 输入应返回空切片，得到 %q", out)
		}
	})
}

// TestPlainItemIDsDropsIDOfOptionalKinds idOptional 类型（含缺 type、type 为 null、非字符串、空串）的项 id 超过 64 字节时删去 id 键，
// 其余字段（含 call_id）原样保留；id 只超过一个字节、或含 + / 的 Base64 都一样。
func TestPlainItemIDsDropsIDOfOptionalKinds(t *testing.T) {
	optional := []string{
		"message", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output",
		"tool_search_call", "tool_search_output", "computer_call_output", "shell_call", "shell_call_output",
		"apply_patch_call", "apply_patch_call_output", "mcp_approval_response", "additional_tools", "compaction",
	}
	for _, id := range []string{strings.Repeat("a", 65), itemIDOpaque, "fc_" + strings.Repeat("x", 300)} {
		t.Run(fmt.Sprintf("id-%d-bytes", len(id)), func(t *testing.T) {
			var in, want []string
			for _, typ := range optional {
				in = append(in, fmt.Sprintf(`{"type":%q,"id":%s,"call_id":"call_%s","k":1}`, typ, itemIDQuote(id), typ))
				want = append(want, fmt.Sprintf(`{"type":%q,"call_id":"call_%s","k":1}`, typ, typ))
			}
			// 缺 type、type 为 null、非字符串、空串，都视为 ""，同属 idOptional。
			for _, fields := range []string{`"role":"user","content":"hi"`, `"type":null,"k":1`, `"type":5,"k":1`, `"type":"","k":1`} {
				in = append(in, fmt.Sprintf(`{"id":%s,%s}`, itemIDQuote(id), fields))
				want = append(want, fmt.Sprintf(`{%s}`, fields))
			}
			out := runPlainItemIDs(t, `{"model":"m","input":[`+strings.Join(in, ",")+`]}`)
			assertSameJSON(t, out, `{"model":"m","input":[`+strings.Join(want, ",")+`]}`)
		})
	}
}

// TestPlainItemIDsBindsOtherKinds 不属于 idOptional 的类型，id 超过 64 字节时绑定为 前缀 + sha256 十六进制，总长 64：
// 前缀依次取 itemKindPrefix、否则 id 自带的前缀（第一个 "_" 在 1..4 位且之前只含合法字符），都没有则为纯 64 位十六进制。
func TestPlainItemIDsBindsOtherKinds(t *testing.T) {
	long := strings.Repeat("Z", 300)
	for _, tc := range []struct{ name, typ, id, prefix string }{
		{"reasoning", "reasoning", long, "rs_"},
		{"web-search-call", "web_search_call", long, "ws_"},
		{"file-search-call", "file_search_call", long, "fs_"},
		{"computer-call", "computer_call", long, "cu_"},
		{"image-generation-call", "image_generation_call", long, "ig_"},
		{"code-interpreter-call", "code_interpreter_call", long, "ci_"},
		{"mcp-list-tools", "mcp_list_tools", long, "mcpl_"},
		{"mcp-approval-request", "mcp_approval_request", long, "mcpr_"},
		{"mcp-call", "mcp_call", long, "mcp_"},
		// Copilot 的不透明 Base64 id：含 + 与 /，没有类型前缀。
		{"opaque-base64-web-search", "web_search_call", itemIDOpaqueSearch, "ws_"},
		{"opaque-base64-computer-call", "computer_call", itemIDOpaque, "cu_"},
		// 已有同类前缀但超长的 id 也要重新绑定，不能原样保留。
		{"kind-prefix-already-there", "reasoning", "rs_" + strings.Repeat("b", 417), "rs_"},
		// 类型前缀优先于 id 自带的前缀。
		{"kind-prefix-beats-own-prefix", "web_search_call", "xk_" + long, "ws_"},
		// 未知类型：沿用 id 自带的前缀。
		{"own-prefix-3-chars", "item_kind_x", "xk_" + long, "xk_"},
		{"own-prefix-1-char", "item_kind_x", "a_" + long, "a_"},
		{"own-prefix-4-chars", "item_kind_x", "abcd_" + long, "abcd_"},
		{"own-prefix-with-dash", "item_kind_x", "ab-c_" + long, "ab-c_"},
		{"own-prefix-first-underscore-wins", "item_kind_x", "ab_cd_" + long, "ab_"},
		{"own-prefix-is-msg", "local_shell_call", "msg_" + long, "msg_"},
		{"own-prefix-too-long-5-chars", "item_kind_x", "abcde_" + long, ""},
		{"own-prefix-empty-leading-underscore", "item_kind_x", "_" + long, ""},
		{"own-prefix-has-illegal-char", "item_kind_x", "a+b_" + long, ""},
		{"no-underscore", "item_kind_y", long, ""},
		{"item-reference", "item_reference", long, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","input":[{"type":` + itemIDQuote(tc.typ) + `,"id":` + itemIDQuote(tc.id) + `,"x":1.0}]}`
			out := runPlainItemIDs(t, body)
			items := itemIDInput(t, out)
			if len(items) != 1 {
				t.Fatalf("input 项数 = %d, want 1: %s", len(items), out)
			}
			got, _ := items[0]["id"].(string)
			want := itemIDDigestID(tc.prefix, tc.id)
			if got != want {
				t.Fatalf("绑定后的 id = %q, want %q", got, want)
			}
			if len(got) != 64 || !itemIDChars.MatchString(got) {
				t.Fatalf("绑定后的 id %q 不是 64 个合法字符", got)
			}
			if items[0]["type"] != tc.typ || fmt.Sprint(items[0]["x"]) != "1.0" {
				t.Fatalf("其他字段被改动: %v", items[0])
			}
		})
	}
}

// TestPlainItemIDsReasoningFromOtherVendorsGoes reasoning 项：id 含合法字符集以外的字符时整项删除（不论长短），
// 只是超长（字符都合法）则保留并绑定为 rs_ 前缀，id 合法的原样保留。
func TestPlainItemIDsReasoningFromOtherVendorsGoes(t *testing.T) {
	longSafe := "rs_" + strings.Repeat("b", 417)
	longUnsafe := "rs_" + strings.Repeat("b", 100) + ":x"
	body := `{"model":"m","input":[` +
		`{"type":"message","role":"user","content":"hi"},` +
		`{"type":"reasoning","id":"` + itemIDMuse + `","summary":[],"encrypted_content":"muse-sealed"},` +
		`{"type":"reasoning","id":` + itemIDQuote(itemIDOpaque) + `,"summary":[],"encrypted_content":"base64-sealed"},` +
		`{"type":"reasoning","id":"` + longUnsafe + `","summary":[],"encrypted_content":"long-unsafe-sealed"},` +
		`{"type":"reasoning","id":"` + longSafe + `","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"gAAAA"},` +
		`{"type":"reasoning","id":"rs_keep","summary":[],"encrypted_content":"keep-sealed"},` +
		`{"type":"message","role":"assistant","content":"ok"}]}`
	out := runPlainItemIDs(t, body)
	want := `{"model":"m","input":[` +
		`{"type":"message","role":"user","content":"hi"},` +
		`{"type":"reasoning","id":"` + itemIDDigestID("rs_", longSafe) + `","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"gAAAA"},` +
		`{"type":"reasoning","id":"rs_keep","summary":[],"encrypted_content":"keep-sealed"},` +
		`{"type":"message","role":"assistant","content":"ok"}]}`
	assertSameJSON(t, out, want)
	for _, gone := range []string{"muse-sealed", "base64-sealed", "long-unsafe-sealed"} {
		if bytes.Contains(out, []byte(gone)) {
			t.Fatalf("别家的 reasoning 应整项删除，输出仍含 %q: %s", gone, out)
		}
	}
}

// TestPlainItemIDsReplacesIllegalCharacters 不超过 64 字节但含其他字符的 id：每个其他字符（按字符，不是字节）换成 "_"，
// 项的其余内容不变（idOptional 类型也只是替换，不删除）；超过 64 字节的按长度规则处理（字节数，不是字符数）。
func TestPlainItemIDsReplacesIllegalCharacters(t *testing.T) {
	cjk20 := strings.Repeat("中", 20) // 60 字节
	cjk30 := strings.Repeat("中", 30) // 90 字节，只有 30 个字符
	for _, tc := range []struct{ name, item, want string }{
		{"function-call-with-colon", `{"type":"function_call","id":"` + itemIDMuseCall + `","call_id":"call_1","name":"f"}`,
			`{"type":"function_call","id":"fc_6aca2fc7533c979d5f0e4f7b_fc_01a125caa544754e9a3c70080100b00a","call_id":"call_1","name":"f"}`},
		{"every-illegal-char-replaced-once", `{"type":"web_search_call","id":"a b:c/d+e=f.g"}`,
			`{"type":"web_search_call","id":"a_b_c_d_e_f_g"}`},
		{"quote-and-backslash", `{"type":"message","id":"a\"b\\c"}`, `{"type":"message","id":"a_b_c"}`},
		{"escaped-colon-decodes-first", `{"type":"message","id":"msg\u003aone"}`, `{"type":"message","id":"msg_one"}`},
		{"multibyte-char-is-one-underscore", `{"type":"message","id":"msg_é"}`, `{"type":"message","id":"msg__"}`},
		{"cjk-chars-one-underscore-each", `{"type":"web_search_call","id":"ws_中文"}`, `{"type":"web_search_call","id":"ws___"}`},
		{"exactly-64-bytes-with-illegal-char", `{"type":"web_search_call","id":"msg_` + strings.Repeat("a", 58) + `:x"}`,
			`{"type":"web_search_call","id":"msg_` + strings.Repeat("a", 58) + `_x"}`},
		{"64-bytes-by-multibyte-is-still-replaced", `{"type":"web_search_call","id":"msg_` + cjk20 + `"}`,
			`{"type":"web_search_call","id":"msg_` + strings.Repeat("_", 20) + `"}`},
		{"over-64-bytes-by-multibyte-is-bound", `{"type":"web_search_call","id":"ws_` + cjk30 + `"}`,
			`{"type":"web_search_call","id":"` + itemIDDigestID("ws_", "ws_"+cjk30) + `"}`},
		{"over-64-bytes-by-multibyte-optional-kind-drops-id", `{"type":"message","id":"msg_` + cjk30 + `","role":"user"}`,
			`{"type":"message","role":"user"}`},
		{"65-bytes-with-illegal-char-optional-kind-drops-id", `{"type":"message","id":"` + strings.Repeat("a", 64) + `:","role":"user"}`,
			`{"type":"message","role":"user"}`},
		{"65-bytes-with-illegal-char-other-kind-is-bound", `{"type":"file_search_call","id":"` + strings.Repeat("a", 64) + `:"}`,
			`{"type":"file_search_call","id":"` + itemIDDigestID("fs_", strings.Repeat("a", 64)+":") + `"}`},
		{"call-id-keeps-its-illegal-chars", `{"type":"function_call","id":"fc_a:b","call_id":"call:/+=1","name":"f"}`,
			`{"type":"function_call","id":"fc_a_b","call_id":"call:/+=1","name":"f"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runPlainItemIDs(t, `{"model":"m","input":[`+tc.item+`]}`)
			assertSameJSON(t, out, `{"model":"m","input":[`+tc.want+`]}`)
			// 规整后的请求体再规整不会有改动。
			assertSameSlice(t, plainItemIDs(out), out)
		})
	}
}

// TestPlainItemIDsMuseConversation 别家厂商（Muse Spark）的一轮对话：reasoning 整项删除，63 字节含冒号的 function_call id 换成下划线，
// 65 字节的 message id（idOptional，超长）删去，call_id 与函数输出原样保留，其余项与顶层字段不变。
func TestPlainItemIDsMuseConversation(t *testing.T) {
	body := `{"model":"gpt-6.1","store":false,"include":["reasoning.encrypted_content"],"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the login bug"}]},` +
		`{"type":"reasoning","id":"` + itemIDMuse + `","summary":[],"encrypted_content":"muse-sealed"},` +
		`{"type":"function_call","id":"` + itemIDMuseCall + `","call_id":"call_muse_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},` +
		`{"type":"function_call_output","call_id":"call_muse_1","output":"main.go"},` +
		`{"type":"message","id":"` + itemIDMuseMessage + `","role":"assistant","content":[{"type":"output_text","text":"I fixed main.go"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"now add a test"}]}]}`
	want := `{"model":"gpt-6.1","store":false,"include":["reasoning.encrypted_content"],"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the login bug"}]},` +
		`{"type":"function_call","id":"fc_6aca2fc7533c979d5f0e4f7b_fc_01a125caa544754e9a3c70080100b00a","call_id":"call_muse_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},` +
		`{"type":"function_call_output","call_id":"call_muse_1","output":"main.go"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I fixed main.go"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"now add a test"}]}]}`
	assertSameJSON(t, runPlainItemIDs(t, body), want)
}

// TestPlainItemIDsKeepsCallID 调用与其输出靠 call_id 配对：不论 id 如何处理（删除、绑定、替换），call_id 一律不动，
// 即使它本身超长或含其他字符。
func TestPlainItemIDsKeepsCallID(t *testing.T) {
	callID := strings.Repeat("c", 100) + ":/+="
	for _, typ := range []string{"function_call", "function_call_output", "computer_call", "computer_call_output", "shell_call", "custom_tool_call", "item_kind_x"} {
		for _, id := range []string{strings.Repeat("a", 300), "a:b"} {
			t.Run(fmt.Sprintf("%s-%d", typ, len(id)), func(t *testing.T) {
				body := `{"model":"m","input":[{"type":"` + typ + `","id":"` + id + `","call_id":` + itemIDQuote(callID) + `}]}`
				items := itemIDInput(t, runPlainItemIDs(t, body))
				if len(items) != 1 || items[0]["call_id"] != callID {
					t.Fatalf("call_id 被改动: %v", items)
				}
			})
		}
	}
}

// TestPlainItemIDsIsStableAndCollisionFree 同一个 id 每次得到同一个结果；以相同的前 N 个字符开头的不同 id 得到不同结果；
// 同一个 id 在不同类型下得到各自的前缀；规整结果再规整不再变化（已绑定的 id 不会被再次绑定）。
func TestPlainItemIDsIsStableAndCollisionFree(t *testing.T) {
	long := "msg_" + strings.Repeat("A", 440)
	like := "msg_" + strings.Repeat("A", 435) + "1111"
	other := "msg_" + strings.Repeat("A", 435) + "2222"
	body := `{"model":"m","input":[` +
		`{"type":"web_search_call","id":"` + like + `","status":"completed"},` +
		`{"type":"web_search_call","id":"` + other + `","status":"completed"},` +
		`{"type":"web_search_call","id":"` + like + `","status":"completed"},` +
		`{"type":"file_search_call","id":"` + like + `","status":"completed"},` +
		`{"type":"reasoning","id":"` + long + `","summary":[]},` +
		`{"type":"item_kind_x","id":"` + long + `"}]}`
	first := runPlainItemIDs(t, body)
	items := itemIDInput(t, first)
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i], _ = item["id"].(string)
		if len(ids[i]) != 64 || !itemIDChars.MatchString(ids[i]) {
			t.Fatalf("items[%d] 的 id %q 不是 64 个合法字符", i, ids[i])
		}
	}
	if ids[0] == ids[1] {
		t.Errorf("前缀相同的两个 id 得到同一个结果: %q", ids[0])
	}
	if ids[0] != ids[2] {
		t.Errorf("同一个 id 得到不同结果: %q 与 %q", ids[0], ids[2])
	}
	if ids[0] == ids[3] || !strings.HasPrefix(ids[0], "ws_") || !strings.HasPrefix(ids[3], "fs_") {
		t.Errorf("同一个 id 在不同类型下应各取自己的前缀: %q %q", ids[0], ids[3])
	}
	if !strings.HasPrefix(ids[4], "rs_") || !strings.HasPrefix(ids[5], "msg_") {
		t.Errorf("reasoning 与未知类型的前缀不对: %q %q", ids[4], ids[5])
	}
	// 同一输入再来一次，输出完全相同；对输出再规整，不再有改动。
	if second := runPlainItemIDs(t, body); !bytes.Equal(first, second) {
		t.Errorf("同一输入两次的输出不同:\n%s\n%s", first, second)
	}
	assertSameSlice(t, plainItemIDs(first), first)
}

// TestPlainItemIDsKeepsUntouchedItemsAndLiterals 有改动时只重编码改动过的项与顶层对象：未改动的项保留原文（键顺序、数字、字符串），
// 改动过的项数字原文保留、不转义 < > &，顶层其他字段语义不变。
func TestPlainItemIDsKeepsUntouchedItemsAndLiterals(t *testing.T) {
	const untouched = `{"type":"message","role":"user","id":"msg_keep","content":"<b> & c > d","n":1.0,"big":12345678901234567890,"e":1e3}`
	const untouchedNoID = `{"z":1,"type":"function_call_output","call_id":"c1","output":"2.50","a":[3.10,null]}`
	body := `{"model":"m","temperature":1.0,"seed":12345678901234567890,"instructions":"be <nice> & kind","input":[` +
		untouched + `,` +
		`{"type":"function_call","id":"` + strings.Repeat("x", 100) + `","call_id":"c1","name":"f","arguments":"<a>&","n":3.10,"big":98765432109876543210},` +
		untouchedNoID + `]}`
	out := runPlainItemIDs(t, body)
	for _, literal := range []string{untouched, untouchedNoID} {
		if !bytes.Contains(out, []byte(literal)) {
			t.Fatalf("未改动的项应保留原文 %s\n输出: %s", literal, out)
		}
	}
	for _, literal := range []string{`"arguments":"<a>&"`, `3.10`, `98765432109876543210`, `"temperature":1.0`, `12345678901234567890`, `be <nice> & kind`} {
		if !bytes.Contains(out, []byte(literal)) {
			t.Fatalf("输出缺少原样片段 %q（数字原文保留，不转义 HTML）: %s", literal, out)
		}
	}
	for _, escaped := range []string{"\\u00" + "3c", "\\u00" + "3e", "\\u00" + "26"} {
		if bytes.Contains(out, []byte(escaped)) {
			t.Fatalf("输出含 HTML 转义 %q: %s", escaped, out)
		}
	}
	want := `{"model":"m","temperature":1.0,"seed":12345678901234567890,"instructions":"be <nice> & kind","input":[` +
		untouched + `,` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"<a>&","n":3.10,"big":98765432109876543210},` +
		untouchedNoID + `]}`
	assertSameJSON(t, out, want)
}

// TestPlainItemIDsKeepsItemOrderAndOtherTopLevelFields 删除 reasoning、删去 id 之后，剩余项的相对顺序不变，顶层其他字段原样保留。
func TestPlainItemIDsKeepsItemOrderAndOtherTopLevelFields(t *testing.T) {
	body := `{"model":"m","store":false,"tools":[{"type":"web_search"}],"metadata":{"id":"` + strings.Repeat("m", 100) + `","input":[1]},"input":[` +
		`{"type":"message","role":"user","content":"1"},` +
		`{"type":"reasoning","id":"a:b","summary":[]},` +
		`{"type":"message","id":"` + strings.Repeat("a", 70) + `","role":"user","content":"2"},` +
		`"plain string item",` +
		`{"type":"reasoning","id":"rs_ok","summary":[]},` +
		`{"type":"web_search_call","id":"ws_x:y","status":"completed"}]}`
	want := `{"model":"m","store":false,"tools":[{"type":"web_search"}],"metadata":{"id":"` + strings.Repeat("m", 100) + `","input":[1]},"input":[` +
		`{"type":"message","role":"user","content":"1"},` +
		`{"type":"message","role":"user","content":"2"},` +
		`"plain string item",` +
		`{"type":"reasoning","id":"rs_ok","summary":[]},` +
		`{"type":"web_search_call","id":"ws_x_y","status":"completed"}]}`
	assertSameJSON(t, runPlainItemIDs(t, body), want)
}

// itemIDPrefixOf 是端到端测试中假 Copilot 要求各类型 id 必须带的前缀。
var itemIDPrefixOf = map[string]string{"message": "msg_", "function_call": "fc_", "reasoning": "rs_", "web_search_call": "ws_"}

// itemIDRejectingCopilot 返回扮演 Copilot /responses 的上游应答：input 项带 id 时，id 必须不超过 64 字符、只含合法字符、
// 且带该类型的前缀，否则整个请求被空消息的 400 拒绝（与 magpie 测试里的 opaqueCopilot 一致，且不认任何 "自己发出的" 不透明 id）。
func itemIDRejectingCopilot() func(string, int, []byte) staleReply {
	return func(_ string, _ int, body []byte) staleReply {
		var q struct {
			Input []map[string]any `json:"input"`
		}
		_ = json.Unmarshal(body, &q)
		for _, it := range q.Input {
			id, ok := it["id"].(string)
			if !ok {
				continue
			}
			typ, _ := it["type"].(string)
			prefix := itemIDPrefixOf[typ]
			if len(id) > 64 || !itemIDChars.MatchString(id) || (prefix != "" && !strings.HasPrefix(id, prefix)) {
				return staleReply{status: http.StatusBadRequest, contentType: "application/json", body: `{"error":{"message":"","code":"invalid_request_body"}}`}
			}
		}
		return staleReply{status: http.StatusOK, contentType: "application/json", body: `{"id":"resp_1"}`}
	}
}

// TestResponsesForwardsOpaqueItemIDsInAcceptedForm Copilot 的 452 字符 Base64 id（函数调用、web 搜索调用）原样回传时会被上游整个拒绝；
// 经 /responses 转发后函数调用不带 id、搜索调用带 ws_ 前缀的 64 字符 id，请求成功，call_id 保持不变。
func TestResponsesForwardsOpaqueItemIDsInAcceptedForm(t *testing.T) {
	if len(itemIDOpaque) != 452 || !strings.ContainsAny(itemIDOpaque, "+/") || len(itemIDOpaqueSearch) != 452 || !strings.ContainsAny(itemIDOpaqueSearch, "+/") {
		t.Fatalf("测试数据不是 Copilot 的形状: %d %d", len(itemIDOpaque), len(itemIDOpaqueSearch))
	}
	for _, path := range []string{"/responses", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			s, up := staleServiceWith(t, itemIDRejectingCopilot())
			body := `{"model":"gpt-4.1","store":false,"input":[` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"list the files"}]},` +
				`{"type":"function_call","id":"` + itemIDOpaque + `","call_id":"call_Xq7","name":"shell","arguments":"{\"cmd\":\"ls\"}"},` +
				`{"type":"function_call_output","call_id":"call_Xq7","output":"main.go"},` +
				`{"type":"web_search_call","id":"` + itemIDOpaqueSearch + `","status":"completed","action":{"type":"search","query":"Go 1.26"}},` +
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`
			w := testRequest(s, http.MethodPost, path, body)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
			}
			sent := up.sent("/responses")
			if len(sent) != 1 {
				t.Fatalf("上游 /responses 请求数 = %d, want 1", len(sent))
			}
			items := itemIDInput(t, sent[0])
			if len(items) != 5 {
				t.Fatalf("转发的 input 项数 = %d, want 5: %s", len(items), sent[0])
			}
			if _, has := items[1]["id"]; has {
				t.Errorf("函数调用的超长 id 应被删去: %v", items[1]["id"])
			}
			if items[1]["call_id"] != "call_Xq7" || items[2]["call_id"] != "call_Xq7" {
				t.Errorf("函数调用与其输出的 call_id 应保持不变: %v %v", items[1]["call_id"], items[2]["call_id"])
			}
			search, _ := items[3]["id"].(string)
			if search != itemIDDigestID("ws_", itemIDOpaqueSearch) {
				t.Errorf("搜索调用的 id = %q, want %q", search, itemIDDigestID("ws_", itemIDOpaqueSearch))
			}
		})
	}
}

// TestResponsesForwardsOtherVendorItemIDsInAcceptedForm 别家厂商的一轮对话经 /responses 转发：reasoning 整项删除，其余 id 规整后请求成功。
func TestResponsesForwardsOtherVendorItemIDsInAcceptedForm(t *testing.T) {
	s, up := staleServiceWith(t, itemIDRejectingCopilot())
	body := `{"model":"gpt-4.1","store":false,"input":[` +
		`{"type":"message","role":"user","content":"hi"},` +
		`{"type":"reasoning","id":"` + itemIDMuse + `","summary":[],"encrypted_content":"muse-sealed"},` +
		`{"type":"function_call","id":"` + itemIDMuseCall + `","call_id":"call_muse_1","name":"shell","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_muse_1","output":"main.go"}]}`
	w := testRequest(s, http.MethodPost, "/v1/responses", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	sent := up.sent("/responses")
	if len(sent) != 1 || bytes.Contains(sent[0], []byte("muse-sealed")) {
		t.Fatalf("别家的 reasoning 应在转发前删除: %q", sent)
	}
	items := itemIDInput(t, sent[0])
	if len(items) != 3 || items[1]["id"] != "fc_6aca2fc7533c979d5f0e4f7b_fc_01a125caa544754e9a3c70080100b00a" {
		t.Fatalf("转发的 input = %v", items)
	}
}

// TestChatAndMessagesDoNotRewriteItemIDs 输入项 id 的规整只作用于 /responses：Chat Completions 与 /v1/messages 的请求体逐字节透传，
// 即使里面有超长 id（含碰巧叫 input 的数组，以及 tool_calls 的超长 id）。
func TestChatAndMessagesDoNotRewriteItemIDs(t *testing.T) {
	inputItems := `[{"type":"function_call","id":"` + itemIDOpaque + `","call_id":"call_Xq7"},` +
		`{"type":"reasoning","id":"` + itemIDMuse + `","summary":[]},` +
		`{"type":"web_search_call","id":"` + itemIDOpaqueSearch + `"}]`
	for _, tc := range []struct{ name, path, body string }{
		{"chat-completions-input-array", "/v1/chat/completions",
			`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"input":` + inputItems + `}`},
		{"chat-completions-without-v1", "/chat/completions",
			`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"input":` + inputItems + `}`},
		{"chat-completions-tool-call-ids", "/v1/chat/completions",
			`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"},` +
				`{"role":"assistant","tool_calls":[{"id":"` + itemIDOpaque + `","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"` + itemIDOpaque + `","content":"ok"}]}`},
		{"messages-input-array", "/v1/messages",
			`{"model":"gpt-4.1","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"input":` + inputItems + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, up := staleServiceWith(t, func(string, int, []byte) staleReply {
				return staleReply{status: http.StatusOK, contentType: "application/json", body: `{}`}
			})
			w := testRequest(s, http.MethodPost, tc.path, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
			}
			upstreamPath := "/chat/completions"
			if tc.path == "/v1/messages" {
				upstreamPath = "/v1/messages"
			}
			sent := up.sent(upstreamPath)
			if len(sent) != 1 || !bytes.Equal(sent[0], []byte(tc.body)) {
				t.Fatalf("转发的请求体应与客户端发送的逐字节一致:\n got=%q\nwant=%s", sent, tc.body)
			}
		})
	}
}
