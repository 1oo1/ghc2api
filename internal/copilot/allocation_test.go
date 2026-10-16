package copilot

// 本文件是请求体分配量的回归测试：messages、input 或 content 数组中含有大量非对象元素（如约 800 万个数字）时，
// 会话键、initiator、promptFrom 与 hasVision 不得按元素数量分配内存（不得为每个元素建切片项或做 json.Unmarshal）。

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

const (
	// allocBodyBytes 是恶意请求体的大小，与生成接口的请求体上限同为 16 MiB。
	allocBodyBytes = 16 << 20
	// allocParseLimit 是 sessionKey、initiator、promptFrom 单次调用允许的 TotalAlloc 增量上限，取 6 倍请求体。
	// 实测（数字数组分别作为 messages、content、Responses input）：三个函数的增量约为请求体的 1 到 2 倍。
	// 增量来自 json.Unmarshal 为 RawMessage 字段复制的字节：messages 或 input 数组整体复制一次（约 1 倍）；
	// content 位于消息对象内部时，消息对象解码时再复制一次 content（合计约 2 倍）。6 倍留出余量。
	// 按元素数量分配（约 800 万个元素，平均每个元素超过 12 字节即超过此上限）或对每个元素做 json.Unmarshal 都会远超此上限。
	allocParseLimit = 6 * allocBodyBytes
	// allocScanLimit 是 hasVision 单次调用允许的上限，取 2 倍请求体。hasVision 只做字节级扫描，实测增量约 2 KiB，
	// 不应分配与元素数量相关的内存。
	allocScanLimit = 2 * allocBodyBytes
)

// allocBody 返回约 allocBodyBytes 字节的请求体：prefix 之后是大量 "1," 数字元素，再接一个字符串 "image"，最后是 suffix。
// 结尾的 "image" 子串使 hasVision 无法走"不含 image 子串"的快路径，从而真正遍历整个数组；它本身不是图片部件。
func allocBody(prefix, suffix string) []byte {
	count := (allocBodyBytes - len(prefix) - len(suffix) - len(`"image"`)) / 2
	return []byte(prefix + strings.Repeat("1,", count) + `"image"` + suffix)
}

// allocDelta 返回 fn 执行期间 TotalAlloc 的增量。调用前后各执行一次 runtime.GC()，使测量起点与终点稳定。
// TotalAlloc 是进程级的累计值，后台 goroutine（如未关闭的测试服务器、额度刷新）的分配也会计入，
// 因此本文件的测试不并行执行，阈值也远高于这类噪声。
func allocDelta(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.GC()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestRequestBodyArrayAllocationIsBounded 三种恶意请求体（数字数组作为 messages、作为 content、作为 Responses input）上，
// 会话键、initiator、promptFrom 的 TotalAlloc 增量必须低于 allocParseLimit，hasVision 必须低于 allocScanLimit。
func TestRequestBodyArrayAllocationIsBounded(t *testing.T) {
	const chat = `{"model":"gpt-4.1","messages":[`
	bodies := []struct {
		name     string
		body     []byte
		upstream string // 会话键计算使用的上游路径
	}{
		{name: "messages-numbers", body: allocBody(chat, `]}`), upstream: "/chat/completions"},
		{name: "content-numbers", body: allocBody(`{"model":"gpt-4.1","messages":[{"role":"user","content":[`, `]}]}`),
			upstream: "/chat/completions"},
		{name: "responses-input-numbers", body: allocBody(`{"model":"gpt-4.1","input":[`, `]}`), upstream: "/responses"},
	}
	for _, tc := range bodies {
		r := httptest.NewRequest(http.MethodPost, tc.upstream, nil)
		calls := []struct {
			name  string
			limit uint64
			call  func()
		}{
			{name: "sessionKey", limit: allocParseLimit, call: func() { sessionKey(r, tc.body, tc.upstream) }},
			{name: "initiator", limit: allocParseLimit, call: func() { initiator(tc.body) }},
			{name: "promptFrom", limit: allocParseLimit, call: func() { promptFrom(tc.body) }},
			{name: "hasVision", limit: allocScanLimit, call: func() { hasVision(tc.body) }},
		}
		for _, c := range calls {
			t.Run(tc.name+"/"+c.name, func(t *testing.T) {
				delta := allocDelta(c.call)
				t.Logf("TotalAlloc 增量 = %d 字节（%.2f MiB），阈值 %d 字节", delta, float64(delta)/(1<<20), c.limit)
				if delta >= c.limit {
					t.Fatalf("%s 分配了 %d 字节（%.2f MiB），want 小于 %d 字节：分配量随数组元素数量增长",
						c.name, delta, float64(delta)/(1<<20), c.limit)
				}
			})
		}
	}
}
