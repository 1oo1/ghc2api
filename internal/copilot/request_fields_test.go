package copilot

// 本文件固定一条已接受的语义：消息对象或内容块中出现字段类型错误时，整项按无效处理并跳过，不影响同一数组中的其他项
// （见 eachMessage）。这是有意的行为，规格方已接受；这些用例在当前实现上应当直接通过。

import (
	"testing"
)

// TestInvalidFieldTypeItemsAreSkipped 字段类型错误的消息被跳过，其余有效项照常参与判断。
func TestInvalidFieldTypeItemsAreSkipped(t *testing.T) {
	t.Run("initiator-skips-message-with-non-string-role", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"a"},{"role":true,"content":"z"}]}`)
		if got := initiator(body); got != "user" {
			t.Fatalf("initiator() = %q, want \"user\"：role 类型错误的消息应被跳过，最后一条有效消息为 user", got)
		}
	})

	t.Run("session-key-skips-message-with-non-string-role", func(t *testing.T) {
		invalid := []byte(`{"messages":[{"role":1,"content":"x"},{"role":"user","content":"y"}]}`)
		valid := []byte(`{"messages":[{"role":"user","content":"y"}]}`)
		keyInvalid := sessionKey(affinityRequest("/v1/chat/completions", "client-a", ""), invalid, "/chat/completions")
		keyValid := sessionKey(affinityRequest("/v1/chat/completions", "client-a", ""), valid, "/chat/completions")
		affinityAssertKeyShape(t, keyValid)
		if keyInvalid != keyValid {
			t.Fatalf("会话键 = %s, want 与仅含 user 消息的请求相同 %s：role 类型错误的消息应被跳过", keyInvalid, keyValid)
		}
	})
}
