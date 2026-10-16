package copilot

// 本文件覆盖请求体字节扫描函数对非法输入的健壮性：输入不是合法 JSON（截断、括号错位、未闭合的字符串等）时，
// 扫描函数必须在有限时间内返回，且不得 panic。返回值不做要求。
// 每次调用都在独立 goroutine 中执行：超时视为死循环，recover 捕获 panic，两者都报告为失败。
// 超时的 goroutine 无法被强制结束，会一直占用 CPU 直到测试进程退出，因此实现修复之前这些用例会留下运行中的 goroutine。

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// scanTimeout 是单次扫描允许的最长时间。合法的小输入在微秒级完成，2 秒足以排除调度延迟。
const scanTimeout = 2 * time.Second

// maxReportedScanFailures 限制每个扫描函数报告的失败输入数，避免一个函数在大量输入上失败时输出过长。
const maxReportedScanFailures = 5

// invalidScanInputs 是固定的非法输入：括号错位（[}]、[1,}、[1}）、未闭合的字符串与对象（["abc、[{"a":"b）、
// 只剩开头（[、{"type":）、截断的转义（"\u12）、未闭合的对象（[{"type":"image"），以及空输入、单个引号与截断的转义等边界值。
var invalidScanInputs = []string{
	`[}]`, `[1,}`, `[1}`, `["abc`, `[{"a":"b`, `[`, `{"type":`, `"\u12`, `[{"type":"image"`,
	`"`, `"\`, `"\u`, `[{"a":]`, `[,]`, ``,
}

// scanSourceBodies 是随机截断的来源，分别覆盖 chat（含图片数组与转义字符串）、Responses（含 \u 转义的键值）、
// Claude（tool_result 嵌套与字符串化的 user_id）以及顶层数组（含数字、字面量与嵌套对象）。
var scanSourceBodies = []string{
	`{"model":"gpt-4.1","messages":[{"role":"user","content":[{"type":"text","text":"a \"quoted\" é"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},{"role":"assistant","content":"ok"}]}`,
	`{"model":"gpt-4.1","prompt_cache_key":"k1","input":[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`,
	`{"model":"claude-x","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","data":"AAAA"}}]}]}],"metadata":{"user_id":"{\"session_id\":\"11111111-2222-4333-8444-555555555555\"}"}}`,
	`[{"type":"image_url","n":[1,2.5e3,-4]},1,"s",null,true,{"type":"text","text":"t"}]`,
}

// scanInputs 返回固定的非法输入，以及每个来源正文的 64 个随机截断。截断位置取自 [0, len)，因此截断结果都是非法输入；种子固定，结果可复现。
func scanInputs(t *testing.T) [][]byte {
	t.Helper()
	inputs := make([][]byte, 0, len(invalidScanInputs)+64*len(scanSourceBodies))
	for _, s := range invalidScanInputs {
		inputs = append(inputs, []byte(s))
	}
	rng := rand.New(rand.NewSource(1))
	for _, body := range scanSourceBodies {
		if !json.Valid([]byte(body)) {
			t.Fatalf("来源正文不是合法 JSON，截断结果将失去意义: %s", body)
		}
		for range 64 {
			inputs = append(inputs, []byte(body[:rng.Intn(len(body))]))
		}
	}
	return inputs
}

// scanFunc 是一个接受任意字节输入的扫描入口。
type scanFunc struct {
	name string
	run  func(input []byte)
}

// scanFuncs 列出遍历字节的函数，以及它们的入口。字符串字面量相关的函数（decodeASCIIString、isVisionType）只在首字节为引号时调用，
// 与生产代码中的调用方式一致；其余输入由它们自身的边界处理覆盖。
var scanFuncs = []scanFunc{
	{"eachObject", func(in []byte) { eachObject(in, func([]byte) bool { return true }) }},
	{"eachMessage", func(in []byte) { eachMessage(in, func(message) bool { return true }) }},
	{"valueEnd", func(in []byte) {
		for i := 0; i <= len(in); i++ {
			valueEnd(in, i)
		}
	}},
	{"jsonStringEnd", func(in []byte) {
		for i, c := range in {
			if c == '"' {
				jsonStringEnd(in, i)
			}
		}
	}},
	{"arrayBody", func(in []byte) { arrayBody(in) }},
	{"hasElements", func(in []byte) { hasElements(in) }},
	{"skipJSONSpace", func(in []byte) { skipJSONSpace(in, 0) }},
	{"firstByte", func(in []byte) { firstByte(in) }},
	{"onlyToolResults", func(in []byte) { onlyToolResults(in) }},
	{"contentText", func(in []byte) { contentText(in, "\n") }},
	{"decodeASCIIString", func(in []byte) {
		var buf [16]byte
		if len(in) > 0 && in[0] == '"' {
			decodeASCIIString(in, buf[:])
		}
	}},
	{"isVisionType", func(in []byte) {
		if len(in) > 0 && in[0] == '"' {
			isVisionType(in)
		}
	}},
	{"hasVision", func(in []byte) { hasVision(in) }},
	{"requestInputs", func(in []byte) { requestInputs(in) }},
	{"initiator", func(in []byte) { initiator(in) }},
	{"promptFrom", func(in []byte) { promptFrom(in) }},
}

// runScanOnce 在独立 goroutine 中执行 run(input)。返回空字符串表示按时返回且未 panic，否则返回失败原因。
func runScanOnce(run func([]byte), input []byte) string {
	done := make(chan string, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				done <- fmt.Sprintf("panic: %v", v)
			}
		}()
		run(input)
		done <- ""
	}()
	select {
	case msg := <-done:
		return msg
	case <-time.After(scanTimeout):
		return fmt.Sprintf("超过 %v 未返回，疑似死循环", scanTimeout)
	}
}

// TestScannersTerminateOnInvalidInput 每个扫描函数对每个非法输入都必须在 scanTimeout 内返回，且不 panic。
// 回归背景（修复前的问题）：eachObject（及其上层的 eachMessage、onlyToolResults、contentText）在 [}]、[1,}、[1} 上不前进而死循环；
// valueEnd 与 jsonStringEnd 在未闭合的字符串上越界 panic；decodeASCIIString 与 isVisionType 在 `"` 与 `"\u` 上越界 panic。
func TestScannersTerminateOnInvalidInput(t *testing.T) {
	inputs := scanInputs(t)
	for _, sc := range scanFuncs {
		t.Run(sc.name, func(t *testing.T) {
			failures := 0
			for _, input := range inputs {
				msg := runScanOnce(sc.run, input)
				if msg == "" {
					continue
				}
				failures++
				if failures <= maxReportedScanFailures {
					t.Errorf("输入 %q：%s", input, msg)
				}
			}
			if failures > maxReportedScanFailures {
				t.Errorf("另有 %d 个输入失败", failures-maxReportedScanFailures)
			}
		})
	}
}
