package copilot

// 本文件把 Copilot 的 Grok 对 /responses 的 2xx 回复里模型的调用还原成客户端（如 Codex）声明工具时的样子，
// 与 groktools.go 的请求改写相对：请求里自定义工具被当 function 发、namespace 被摊平，模型调用它们时回复里是
// type function_call、flat 名、arguments 的形式。移植自 magpie 的 internal/gateway/responses.go（callTo、itemPrefix、流式事件发出）、
// custom_tool.go（customInput、customStream）与 grok_namespace.go（nsTidy 逐事件改写）。
// 只在请求的调用表 calls 非空时进行（见 handleGeneration、streamResponse），每个请求一个 grokRestorer，调用表不跨请求共享。

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// customInput 是模型对当 function 发的自定义工具的调用里的自由文本：arguments 是 {"input": 字符串} 时取 input；
// 否则 arguments 本身是 JSON 字符串时取其值；否则原文。
func customInput(args string) string {
	var a struct {
		Input *string `json:"input"`
	}
	if json.Unmarshal([]byte(args), &a) == nil && a.Input != nil {
		return *a.Input
	}
	var str string
	if json.Unmarshal([]byte(args), &str) == nil {
		return str
	}
	return args
}

// customStream 随调用的 arguments 一段一段到来，逐段给出自定义工具的 input：{"input":"…"}（或单独一个 JSON 字符串）里
// input 字符串解码后的文本，即客户端一边读一边看到模型写补丁的 custom_tool_call_input 增量。读不全的转义与字符留给下一段。
// 完成项的 input 仍是整个 arguments 的 customInput；其他形状的 arguments 不输出任何增量。零值可直接使用，每个调用各用一个。
type customStream struct {
	pend  []byte // input 字符串里已读到、还没给出的部分
	depth int
	str   bool   // 在不是 input 的字符串里
	esc   bool   // 在它的反斜杠之后
	key   []byte // 正在读的深度 1 的键，原文
	inKey bool   // 正在读的字符串就是那个键
	want  bool   // 深度 1 上下一个是键
	last  string // 最近一个深度 1 的键
	value bool   // 深度 1 上下一个值是 input 的
	in    bool   // 在 input 字符串里
	over  bool   // input 已给完，或没有 input
}

// feed 读入 arguments 的下一段，返回它给 input 增加的文本。
func (c *customStream) feed(s string) string {
	if c.over {
		return ""
	}
	if c.in {
		c.pend = append(c.pend, s...)
		return c.take()
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case c.str:
			if c.inKey {
				c.key = append(c.key, b)
			}
			switch {
			case c.esc:
				c.esc = false
			case b == '\\':
				c.esc = true
			case b == '"':
				c.str = false
				if c.inKey {
					c.inKey = false
					c.last = ""
					_ = json.Unmarshal(append([]byte{'"'}, c.key...), &c.last)
				}
			}
		case b == '"' && (c.depth == 0 || c.depth == 1 && c.value):
			c.in = true
			c.pend = append(c.pend, s[i+1:]...)
			return c.take()
		case b == '"':
			c.str = true
			if c.depth == 1 && c.want {
				c.inKey, c.key, c.want = true, c.key[:0], false
			}
		case b == '{' || b == '[':
			c.depth++
			if c.depth == 1 && b == '{' {
				c.want = true
			}
		case b == '}' || b == ']':
			if c.depth--; c.depth <= 0 {
				c.over = true
				return ""
			}
		case b == ':' && c.depth == 1:
			c.value = c.last == "input"
		case b == ',' && c.depth == 1:
			c.want, c.value = true, false
		case b == ' ' || b == '\t' || b == '\n' || b == '\r':
		case c.depth == 0:
			c.over = true // 不是这里读的 JSON
			return ""
		case c.depth == 1:
			c.value = false // input 不是字符串
		}
	}
	return ""
}

// take 把 input 字符串目前读得全的部分（到它的结束引号为止）解码后返回。
func (c *customStream) take() string {
	n, end := 0, false
scan:
	for n < len(c.pend) {
		switch c.pend[n] {
		case '"':
			end = true
			break scan
		case '\\':
			switch {
			case n+1 >= len(c.pend):
				break scan
			case c.pend[n+1] != 'u':
				n += 2
			case n+6 > len(c.pend):
				break scan
			case !isHighSurrogate(c.pend[n+2 : n+6]):
				n += 6
			case n+8 > len(c.pend):
				break scan
			case c.pend[n+6] != '\\' || c.pend[n+7] != 'u':
				n += 6 // 孤立的一半，按 JSON 的读法读
			case n+12 > len(c.pend):
				break scan // 低半还没到
			default:
				n += 12
			}
		default:
			n++
		}
	}
	// 被切开的字符同样等下一段
	for k := 0; k < utf8.UTFMax-1 && n > 0 && !end && !utf8.Valid(c.pend[:n]); k++ {
		n--
	}
	var text string
	if json.Unmarshal(append(append([]byte{'"'}, c.pend[:n]...), '"'), &text) != nil {
		c.over = true // 不是 JSON 读得了的字符串：以完成项为准
		return ""
	}
	c.pend = c.pend[n:]
	if end {
		c.over, c.pend = true, nil
	}
	return text
}

// isHighSurrogate 报告 \u 转义的四位十六进制数字是否是代理对的前半。
func isHighSurrogate(hex []byte) bool {
	r, err := strconv.ParseUint(string(hex), 16, 16)
	return err == nil && r >= 0xD800 && r < 0xDC00
}

// customItemID 把自定义调用项的 id 的 fc_ 前缀换成 ctc_（OpenAI 认 custom_tool_call 的 id 以 ctc_ 开头，Codex 会把项回放回去）；
// 其他 id 不动。
func customItemID(id string) string {
	if rest, ok := strings.CutPrefix(id, "fc_"); ok {
		return "ctc_" + rest
	}
	return id
}

// grokRestorer 还原一个请求的 Grok 回复里模型对 calls 中工具的调用。SSE 与 JSON 回复都支持：
//   - type 为 function_call 且 name 在 calls 中的项（output_item.added/done 的 item、response.* 事件的 response.output[]、
//     JSON 回复的 output[]）：自定义调用变 custom_tool_call（name 为原名，arguments 换成 input，id 的 fc_ 换成 ctc_，有 namespace 时带上）；
//     namespace 里的 function 还原名字与 namespace，并补上空的 encrypted_function_args（Codex 把没有这个列表的 namespace 调用当作已加密）；
//   - 流式事件按 output_index 关联到 added 里被识别的调用：自定义调用的 function_call_arguments.delta 变 custom_tool_call_input.delta
//     （逐段解码，解码为空的整条丢弃），done 变 custom_tool_call_input.done；namespace 调用的 delta 不动，done 带 name 时还原名字；
//   - 其余事件、非 JSON 数据、错误回复逐字节透传；改写的事件重编码方式同 itemids.go（不转义 HTML、数字原文保留）。
//
// 它在 itemIDTidy 之前处理事件，因此 id 统一看到的是改写后的事件。SSE 只输出完整事件，不完整的尾部在上游结束时原样写出（flush）。
type grokRestorer struct {
	calls grokCalls
	open  map[int64]*grokOpenCall // output_index → added 里被识别的调用
	buf   []byte                  // SSE 中还不完整的尾部
}

// grokOpenCall 是流式回复中一个进行中的被识别的调用。
type grokOpenCall struct {
	call   grokCall
	stream customStream // 自定义调用的 arguments 逐段解码
}

func newGrokRestorer(calls grokCalls) *grokRestorer {
	return &grokRestorer{calls: calls, open: map[int64]*grokOpenCall{}}
}

// write 追加上游的 SSE 字节，返回其中已完整的事件（可能经过改写）；不完整的部分留待后续。
func (r *grokRestorer) write(b []byte) []byte {
	r.buf = append(r.buf, b...)
	out, rest := rewriteEvents(r.buf, r.event)
	r.buf = append(r.buf[:0], r.buf[rest:]...)
	return out
}

// flush 原样返回剩余的不完整字节并清空缓冲。
func (r *grokRestorer) flush() []byte {
	rest := r.buf
	r.buf = nil
	return rest
}

// event 还原单个完整的 SSE 事件；没有需要改的时原样返回 ev，返回 nil 表示丢弃该事件。
func (r *grokRestorer) event(ev []byte) []byte {
	data := eventData(ev)
	if !bytes.Contains(data, []byte("function_call")) {
		return ev
	}
	fields := objectFields(data)
	if fields == nil {
		return ev
	}
	typ, _ := stringValue(fields["type"])
	index, hasIndex := rawInt(fields["output_index"])
	changed, newType := false, ""
	switch typ {
	case "response.output_item.added", "response.output_item.done":
		item, call, ok := r.restoreItem(fields["item"])
		if ok {
			fields["item"], changed = item, true
		}
		if typ == "response.output_item.added" && hasIndex {
			if ok {
				r.open[index] = &grokOpenCall{call: call}
			} else {
				delete(r.open, index)
			}
		}
	case "response.function_call_arguments.delta":
		open := r.open[index]
		delta, isString := stringValue(fields["delta"])
		if !hasIndex || open == nil || !open.call.Custom || !isString {
			break
		}
		text := open.stream.feed(delta)
		if text == "" {
			return nil
		}
		fields["delta"] = quoteJSON(text)
		changed, newType = true, "response.custom_tool_call_input.delta"
	case "response.function_call_arguments.done":
		open := r.open[index]
		if !hasIndex || open == nil {
			break
		}
		if open.call.Custom {
			arguments, _ := stringValue(fields["arguments"])
			delete(fields, "arguments")
			delete(fields, "name")
			fields["input"] = quoteJSON(customInput(arguments))
			changed, newType = true, "response.custom_tool_call_input.done"
		} else if _, hasName := stringValue(fields["name"]); hasName {
			fields["name"], fields["namespace"] = quoteJSON(open.call.Name), quoteJSON(open.call.Namespace)
			if _, has := fields["encrypted_function_args"]; !has {
				fields["encrypted_function_args"] = json.RawMessage("[]")
			}
			changed = true
		}
	}
	if newType != "" {
		fields["type"] = quoteJSON(newType)
		if id, ok := stringValue(fields["item_id"]); ok {
			fields["item_id"] = quoteJSON(customItemID(id))
		}
	}
	if response, ok := r.restoreResponse(fields["response"]); ok {
		fields["response"], changed = response, true
	}
	if !changed {
		return ev
	}
	rewritten, ok := marshalLiteral(fields)
	if !ok {
		return ev
	}
	out := withData(ev, rewritten)
	if newType != "" {
		out = withEventType(out, newType)
	}
	return out
}

// rawInt 把 raw 解码为整数；不是整数时 ok 为 false。
func rawInt(raw json.RawMessage) (n int64, ok bool) {
	ok = len(raw) > 0 && json.Unmarshal(raw, &n) == nil
	return n, ok
}

// withEventType 把事件的 event 行（若有）改为 typ，保留行尾；没有 event 行时原样返回。
func withEventType(ev []byte, typ string) []byte {
	var out []byte
	for _, line := range bytes.SplitAfter(ev, []byte("\n")) {
		body := bytes.TrimRight(line, "\r\n")
		if bytes.HasPrefix(body, []byte("event:")) {
			out = append(out, "event: "...)
			out = append(out, typ...)
			out = append(out, line[len(body):]...)
			continue
		}
		out = append(out, line...)
	}
	return out
}

// restoreItem 还原一个输出项：type 为 function_call 且 name 在 calls 中时返回还原后的项与对应的调用；否则 ok 为 false。
func (r *grokRestorer) restoreItem(raw json.RawMessage) (item json.RawMessage, call grokCall, ok bool) {
	fields := objectFields(raw)
	if typ, _ := stringValue(fields["type"]); typ != "function_call" {
		return nil, grokCall{}, false
	}
	name, _ := stringValue(fields["name"])
	call, ok = r.calls[name]
	if !ok {
		return nil, grokCall{}, false
	}
	if call.Custom {
		// 自定义调用带的是自由文本 input，不是 arguments
		arguments, _ := stringValue(fields["arguments"])
		delete(fields, "arguments")
		fields["type"], fields["name"] = quoteJSON("custom_tool_call"), quoteJSON(call.Name)
		input := ""
		if arguments != "" {
			input = customInput(arguments)
		}
		fields["input"] = quoteJSON(input)
		if call.Namespace != "" {
			fields["namespace"] = quoteJSON(call.Namespace)
		}
		if id, ok := stringValue(fields["id"]); ok {
			fields["id"] = quoteJSON(customItemID(id))
		}
	} else {
		fields["name"], fields["namespace"] = quoteJSON(call.Name), quoteJSON(call.Namespace)
		if _, has := fields["encrypted_function_args"]; !has {
			fields["encrypted_function_args"] = json.RawMessage("[]")
		}
	}
	item, ok = marshalLiteral(fields)
	return item, call, ok
}

// restoreOutput 还原 output 数组 raw 里的每个输出项；raw 不是数组或没有改动时 ok 为 false。
func (r *grokRestorer) restoreOutput(raw json.RawMessage) (json.RawMessage, bool) {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	changed := false
	for i, item := range items {
		if restored, _, ok := r.restoreItem(item); ok {
			items[i], changed = restored, true
		}
	}
	if !changed {
		return nil, false
	}
	out, ok := marshalLiteral(items)
	return out, ok
}

// restoreResponse 还原 response 对象 raw 的 output；没有改动时 ok 为 false。
func (r *grokRestorer) restoreResponse(raw json.RawMessage) (json.RawMessage, bool) {
	fields := objectFields(raw)
	output, ok := r.restoreOutput(fields["output"])
	if !ok {
		return nil, false
	}
	fields["output"] = output
	out, ok := marshalLiteral(fields)
	return out, ok
}

// restoreJSON 还原非流式 JSON 回复 body：顶层 object 为 response 时还原其 output；没有改动或不是这样的回复时 ok 为 false。
func (r *grokRestorer) restoreJSON(body []byte) ([]byte, bool) {
	fields := objectFields(body)
	if object, _ := stringValue(fields["object"]); object != "response" {
		return nil, false
	}
	output, ok := r.restoreOutput(fields["output"])
	if !ok {
		return nil, false
	}
	fields["output"] = output
	return marshalLiteral(fields)
}
