package copilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

func copyRequestHeaders(dst, src http.Header) {
	// 只转发协议协商头，不允许调用方覆盖 Copilot 身份或请求轮次。
	hopHeaders := make(map[string]bool)
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			hopHeaders[http.CanonicalHeaderKey(strings.TrimSpace(token))] = true
		}
	}
	for _, key := range []string{"Accept", "Anthropic-Version", "Anthropic-Beta", "Openai-Beta"} {
		if hopHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, value := range src.Values(key) {
			dst.Add(key, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	blocked := map[string]bool{
		"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
		"Proxy-Authorization": true, "Te": true, "Trailer": true,
		"Transfer-Encoding": true, "Upgrade": true, "Set-Cookie": true,
	}
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			blocked[http.CanonicalHeaderKey(strings.TrimSpace(token))] = true
		}
	}
	for key, values := range src {
		if !blocked[http.CanonicalHeaderKey(key)] {
			dst[key] = append([]string(nil), values...)
		}
	}
}

// generationWriteTimeout 是生成接口向客户端写响应时的写超时。每次写入（含刷新）前都会续期，
// 因此它限制的是单次写入的等待时间，而不是整个生成的时长；客户端长时间不读取时写入失败，请求随之结束并释放并发名额。
// 不能用 http.Server 的全局 WriteTimeout 代替，否则会截断长时间的生成。包级变量便于测试缩短，每次续期都重新读取。
var generationWriteTimeout = 60 * time.Second

// generationWriter 包装生成接口的 ResponseWriter，每次 Write 与刷新前续期写截止时间。
// 请求结束时不清除截止时间，依据是 net/http 自身的行为：serve 循环在每个请求的 finishRequest 之后调用
// c.rwc.SetWriteDeadline(time.Time{})（Go 源码 net/http/server.go，1.26.10 与 1.27 中均如此，项目最低版本为 1.26.10）。
// 因此 handler 返回后 net/http 的最后一次 flush 仍受超时保护，之后复用该连接的请求也不会遇到过期的截止时间。
type generationWriter struct {
	http.ResponseWriter
	// controller 包装底层 ResponseWriter 而不是 generationWriter 自身，使 FlushError 调用它时不会递归回来。
	controller *http.ResponseController
}

func newGenerationWriter(w http.ResponseWriter) *generationWriter {
	return &generationWriter{ResponseWriter: w, controller: http.NewResponseController(w)}
}

// extendDeadline 把写截止时间设为现在起 generationWriteTimeout 之后。
// 底层不支持截止时间时（如 httptest.ResponseRecorder）忽略错误，由写入本身报告真实的错误。
func (w *generationWriter) extendDeadline() {
	_ = w.controller.SetWriteDeadline(time.Now().Add(generationWriteTimeout))
}

func (w *generationWriter) Write(p []byte) (int, error) {
	w.extendDeadline()
	return w.ResponseWriter.Write(p)
}

// FlushError 是 http.ResponseController 识别的刷新方法（比 Flush 多返回错误），刷新前同样续期。
func (w *generationWriter) FlushError() error {
	w.extendDeadline()
	return w.controller.Flush()
}

func (w *generationWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// upstreamBody 包装上游回复的正文。仅当上游状态 < 400（成功响应）时，读取上游出错（非 EOF）才在响应写入器链上标记网络错误，
// 因此流中途断开会计为网络错误。错误状态（>= 400）的正文读取失败不标记，按上游状态码计数。
// 写给客户端失败不经过这里，不会被计为网络错误。
type upstreamBody struct {
	body    io.Reader
	w       http.ResponseWriter
	success bool // 上游状态 < 400
}

func (b upstreamBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && b.success {
		markFailure(b.w, failureNetwork)
	}
	return n, err
}

// streamResponse 把上游回复透传给客户端，SSE 回复每读到一段就 flush。
// tidyItemIDs 为 true 时（仅 /responses），SSE 流经 itemIDTidy 统一输出项 id；非 SSE 回复始终逐字节透传。
func streamResponse(w http.ResponseWriter, res *http.Response, tidyItemIDs bool) {
	defer res.Body.Close()
	copyResponseHeaders(w.Header(), res.Header)
	if res.StatusCode >= http.StatusBadRequest {
		// 错误状态的正文可能读取不完整，不转发 Content-Length（同 writeUpstreamError）。
		w.Header().Del("Content-Length")
	}
	stream := strings.HasPrefix(strings.ToLower(res.Header.Get("Content-Type")), "text/event-stream")
	if stream {
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(res.StatusCode)
	body := upstreamBody{body: res.Body, w: w, success: res.StatusCode < http.StatusBadRequest}
	if !stream {
		_, _ = io.Copy(w, body)
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	var tidy *itemIDTidy
	if tidyItemIDs {
		tidy = &itemIDTidy{}
	}
	// send 写出一段字节并 flush；返回 false 表示客户端已不可写。
	send := func(b []byte) bool {
		if len(b) == 0 {
			return true
		}
		if _, err := w.Write(b); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	buffer := make([]byte, 32<<10)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if tidy != nil {
				chunk = tidy.write(chunk)
			}
			if !send(chunk) {
				return
			}
		}
		if err != nil {
			if tidy != nil {
				send(tidy.flush())
			}
			return
		}
	}
}

// message 是 messages 或 Responses input 数组中的一个对象项，只保留会话判断需要的字段。
type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// requestInputs 返回会话判断所用的输入的原始字节，数组由 eachMessage 逐项读取，这里不构造切片：
// 含元素的 messages 数组优先；否则取 Responses 的 input 数组（即使为空）；两者都不是数组时返回 input 的原始值（它可能是字符串）。
// 请求体不是合法 JSON 时两者都为 nil。
func requestInputs(body []byte) (messages, input json.RawMessage) {
	var value struct {
		Messages json.RawMessage `json:"messages"`
		Input    json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &value) != nil {
		return nil, nil
	}
	if hasElements(value.Messages) {
		return value.Messages, nil
	}
	if firstByte(value.Input) == '[' {
		return value.Input, nil
	}
	return nil, value.Input
}

// hasElements 报告 raw 是否为至少含一个元素（不论类型）的数组。
func hasElements(raw json.RawMessage) bool {
	i := arrayBody(raw)
	if i < 0 {
		return false
	}
	i = skipJSONSpace(raw, i)
	return i < len(raw) && raw[i] != ']'
}

// eachMessage 按顺序把数组中的对象项解码为 message 后交给 fn，fn 返回 false 时停止。
// 字段类型错误的对象项整项跳过（例如 "role": true），不影响同一数组中的其他项；非对象元素同样被跳过。
func eachMessage(array json.RawMessage, fn func(m message) bool) {
	eachObject(array, func(object []byte) bool {
		var m message
		if json.Unmarshal(object, &m) != nil {
			return true
		}
		return fn(m)
	})
}

// initiator 计算 X-Initiator：最后一条消息为 user 且其内容不全是 tool_result 时为 "user"，否则为 "agent"。
// 没有对象消息时，input 为字符串返回 "user"，其他情况返回 "agent"。
// 语义说明（已接受的变化，见 docs/http-api.md）：非对象元素被跳过，例如 messages:[{"role":"user"},1] 的结果为 "user"；
// 字段类型错误的消息整项跳过，例如 messages:[{"role":"user","content":"a"},{"role":true}] 的结果为 "user"。
func initiator(body []byte) string {
	messages, input := requestInputs(body)
	if messages == nil {
		var text string
		if json.Unmarshal(input, &text) == nil {
			return "user"
		}
		return "agent"
	}
	var last message
	found := false
	eachMessage(messages, func(m message) bool {
		last, found = m, true
		return true
	})
	if !found || last.Role != "user" || onlyToolResults(last.Content) {
		return "agent"
	}
	return "user"
}

// onlyToolResults 报告内容数组是否至少含一个内容块且全部为 tool_result，即这条 user 消息只是工具结果。
// 字段类型错误的内容块（如 "type": 5）整项跳过，既不计入数量，也不影响判断。
func onlyToolResults(content json.RawMessage) bool {
	count := 0
	onlyTools := true
	eachObject(content, func(object []byte) bool {
		var block struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(object, &block) != nil {
			return true
		}
		count++
		onlyTools = block.Type == "tool_result"
		return onlyTools
	})
	return count > 0 && onlyTools
}

// contentText 返回消息内容的文本：content 为字符串时直接返回；为数组时按顺序取 text 与 input_text 片段，直接拼接（不加分隔符），忽略其他片段；
// 其他类型返回空字符串。字段类型错误的内容块（如 "text": 5 或 "type": 5）整项跳过，不计入文本。
func contentText(content json.RawMessage) string {
	if firstByte(content) == '"' {
		var text string
		_ = json.Unmarshal(content, &text)
		return text
	}
	var texts []string
	eachObject(content, func(object []byte) bool {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(object, &block) == nil && (block.Type == "text" || block.Type == "input_text") {
			texts = append(texts, block.Text)
		}
		return true
	})
	return strings.Join(texts, "")
}

// hasVision 报告请求体是否含图片部件：有效 JSON 中任意深度的对象，其 "type" 键的值为 image、image_url 或 input_image。
// 无效 JSON 返回 false。键名与取值按解码后的内容比较，因此 \u 转义写法同样生效。
// 键与值的位置由字符串后是否紧跟冒号判断：合法 JSON 中只有对象键后面才会有冒号。
// 重复键时只要任一出现的 "type" 取值为图片类型即返回 true；解析为通用对象时只保留最后一个值，这里从严判断，属于有意的差异。
// 扫描是字节级的平铺扫描，不解析成通用的 any 树，也不逐 token 分配内存，因此大请求体（如数百万个数字）不会放大内存分配。
func hasVision(body []byte) bool {
	// 快路径：不含 image 子串、也不含 \u 转义时，图片取值不可能出现（\u 是唯一能把字母写成转义形式的转义序列）。
	if !bytes.Contains(body, []byte("image")) && !bytes.Contains(body, []byte(`\u`)) {
		return false
	}
	if !json.Valid(body) {
		return false
	}
	var buf [16]byte // 足以容纳键 "type" 与最长的图片取值 "input_image"（11 字节）
	for i := 0; i < len(body); i++ {
		if body[i] != '"' {
			continue
		}
		end := jsonStringEnd(body, i)
		key := body[i : end+1]
		i = end
		colon := skipJSONSpace(body, end+1)
		if colon == len(body) || body[colon] != ':' {
			continue // 字符串后面不是冒号，它是数组元素或值，不是键
		}
		if string(decodeASCIIString(key, buf[:])) != "type" {
			continue
		}
		value := skipJSONSpace(body, colon+1)
		if value < len(body) && body[value] == '"' && isVisionType(body[value:jsonStringEnd(body, value)+1]) {
			return true
		}
	}
	return false
}

// isVisionType 报告 JSON 字符串字面量解码后是否为图片部件的 "type" 取值（区分大小写）。
func isVisionType(literal []byte) bool {
	var buf [16]byte
	switch string(decodeASCIIString(literal, buf[:])) {
	case "image", "image_url", "input_image":
		return true
	}
	return false
}

// withoutLoneToolChoice 去掉没有 tools 时的 tool_choice：Copilot 的 /responses 会以 400 拒绝（如 Codex 压缩上下文的请求）。
func withoutLoneToolChoice(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"tool_choice"`)) {
		return body
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return body
	}
	if _, ok := request["tool_choice"]; !ok {
		return body
	}
	// tools 缺失、为 null 或空数组才视为“没有工具”；其他类型交给上游自行判断。
	if raw, ok := request["tools"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var tools []json.RawMessage
		if json.Unmarshal(raw, &tools) != nil || len(tools) > 0 {
			return body
		}
	}
	delete(request, "tool_choice")
	out, ok := marshalLiteral(request)
	if !ok {
		return body
	}
	return out
}

// marshalLiteral 重编码请求体：不转义 HTML 字符，json.RawMessage 中的数字原文保留（不经 float64）。
func marshalLiteral(value any) ([]byte, bool) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return nil, false
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), true
}
