package copilot

// 本文件覆盖 hasVision 的语义（请求体是否含图片部件）以及大请求体的分配量回归。

import (
	"strings"
	"testing"
)

// TestHasVisionDetectsImageParts 请求体是有效 JSON，且任意深度的对象中 "type" 字段值为 image、image_url 或 input_image 时为 true。
// 无效 JSON 为 false；数组元素或其他键的字符串 "image" 不算图片；"type" 的值必须是字符串且区分大小写。
func TestHasVisionDetectsImageParts(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "chat-image-url", want: true,
			body: `{"model":"gpt-4.1","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`},
		{name: "anthropic-image", want: true,
			body: `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"abc"}}]}]}`},
		{name: "anthropic-tool-result-with-image", want: true,
			body: `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","data":"abc"}}]}]}]}`},
		{name: "responses-input-image", want: true,
			body: `{"model":"gpt-4.1","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`},
		{name: "object-at-top-level", want: true, body: `{"type":"image"}`},
		{name: "image-object-inside-top-level-array", want: true, body: `[{"type":"image_url"}]`},
		{name: "image-under-key-named-image", want: true, body: `{"image":{"type":"image"}}`},
		{name: "deeply-nested-image", want: true,
			body: strings.Repeat(`{"a":[`, 500) + `{"type":"input_image"}` + strings.Repeat(`]}`, 500)},
		{name: "image-after-large-array", want: true,
			body: "[" + strings.Repeat("1,", 10000) + `{"type":"image_url"}]`},
		// 键与值中的转义字符按解码后的内容判断，与解析为通用对象后的语义一致。
		{name: "escaped-type-key", want: true, body: `{"` + "\x5c" + `u0074ype":"image"}`},
		{name: "escaped-type-value", want: true, body: `{"type":"` + "\x5c" + `u0069mage"}`},
		// 键与冒号、冒号与值之间的 JSON 空白（空格、换行、制表符、回车换行）不影响判断。
		{name: "space-around-colon", want: true, body: `{"type" : "image"}`},
		{name: "newline-around-colon", want: true, body: "{\"type\" :\n \"image\"}"},
		{name: "tab-around-colon", want: true, body: "{\"type\"\t:\t\"input_image\"}"},
		{name: "crlf-around-colon-in-array", want: true, body: "[{\r\n\"type\"\r\n:\r\n\"image_url\"}]"},
		{name: "array-string-image", want: false, body: `{"tags":["image"]}`},
		{name: "text-part-with-image-text", want: false,
			body: `{"model":"gpt-4.1","messages":[{"role":"user","content":[{"type":"text","text":"image"}]}]}`},
		{name: "key-named-image-with-string-value", want: false, body: `{"image":"data"}`},
		{name: "plain-text-mentions-image-url", want: false,
			body: `{"model":"gpt-4.1","messages":[{"role":"user","content":"The key \"image_url\" is mentioned as plain text"}]}`},
		{name: "similar-type-names", want: false, body: `{"type":"image_urls"}`},
		{name: "type-case-sensitive", want: false, body: `{"type":"Image"}`},
		{name: "type-not-string", want: false, body: `{"type":["image"]}`},
		{name: "large-numeric-array-without-image", want: false, body: "[" + strings.Repeat("1,", 10000) + "1]"},
		{name: "invalid-json-truncated", want: false, body: `{"type":"image"`},
		{name: "invalid-json-trailing-garbage", want: false, body: `{"type":"image"}x`},
		{name: "empty-body", want: false, body: ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasVision([]byte(tc.body)); got != tc.want {
				t.Fatalf("hasVision() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHasVisionDuplicateTypeKey 重复键时，只要任一出现的 "type" 取值为图片类型即返回 true。
// 解析为通用对象时只保留最后一个值（{"type":"image","type":"text"} 会被当作 text），这里是有意的差异：从严判断，
// 避免用重复键绕过图片检测。
func TestHasVisionDuplicateTypeKey(t *testing.T) {
	for _, body := range []string{`{"type":"image","type":"text"}`, `{"type":"text","type":"image"}`} {
		if !hasVision([]byte(body)) {
			t.Errorf("hasVision(%s) = false, want true：重复的 type 键中出现了图片类型", body)
		}
	}
}

// TestHasVisionSkipsLargeBodyWithoutImageSubstring 回归测试：约 16 MiB、只含数字且不含 "image" 子串的请求体，
// hasVision 不应把整个请求体解析成通用的 any 树，分配量（TotalAlloc 增量）必须小于 1 MiB。
func TestHasVisionSkipsLargeBodyWithoutImageSubstring(t *testing.T) {
	body := []byte("[" + strings.Repeat("1,", (16<<20)/2) + "1]")
	var got bool
	delta := allocDelta(func() { got = hasVision(body) })
	if got {
		t.Fatal("不含图片的大请求体被判定为含图片")
	}
	if delta >= 1<<20 {
		t.Fatalf("hasVision 分配了 %d 字节, want 小于 1 MiB", delta)
	}
}

// TestHasVisionSkipsEscapedBodyWithoutImageSubstring 回归测试：约 15 MiB、含大量 \n 与 \" 转义，但没有 image 子串与 \u 的请求体。
// 这些转义不能写出 image、type 等字母，因此 hasVision 应直接返回 false，分配量必须小于 1 MiB。
func TestHasVisionSkipsEscapedBodyWithoutImageSubstring(t *testing.T) {
	text := strings.Repeat(`line\n\"q\" `, (15<<20)/12)
	body := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"` + text + `"}]}`)
	var got bool
	delta := allocDelta(func() { got = hasVision(body) })
	if got {
		t.Fatal("不含图片的转义请求体被判定为含图片")
	}
	if delta >= 1<<20 {
		t.Fatalf("hasVision 分配了 %d 字节, want 小于 1 MiB", delta)
	}
}
