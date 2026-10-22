package copilot

import (
	"bytes"
	"encoding/json"
)

// itemIDOf 记录某个输出项在 response.output_item.added 中的 id 与 type。
type itemIDOf struct {
	id  string
	typ string
}

// itemIDTidy 整理转发的 Responses SSE 流中的输出项 id。
// Copilot 上游在同一输出项的每个事件里都换一个新 id，客户端（如 Codex）按 added 的 id 开始、按 done 的 id 结束，
// 会导致消息重复、web search 一直处于开始状态。这里把同一 output_index 的 id 统一改回 added 时的 id；
// id 本已一致的流逐字节透传。零值可直接使用。
type itemIDTidy struct {
	buf []byte
	ids map[int64]itemIDOf // output_index → 添加时的 id 与 type
}

// write 追加上游字节，返回其中已完整的事件（可能经过改写）；不完整的部分留待后续。
func (t *itemIDTidy) write(b []byte) []byte {
	t.buf = append(t.buf, b...)
	var out []byte
	start := 0
	for i := 0; i < len(t.buf); i++ {
		if t.buf[i] != '\n' {
			continue
		}
		end := 0
		if i+1 < len(t.buf) && t.buf[i+1] == '\n' {
			end = i + 2
		} else if i+2 < len(t.buf) && t.buf[i+1] == '\r' && t.buf[i+2] == '\n' {
			end = i + 3
		}
		if end == 0 {
			continue
		}
		out = append(out, t.event(t.buf[start:end])...)
		start = end
		i = end - 1
	}
	t.buf = append(t.buf[:0], t.buf[start:]...)
	return out
}

// flush 原样返回剩余的不完整字节并清空缓冲。
func (t *itemIDTidy) flush() []byte {
	rest := t.buf
	t.buf = nil
	return rest
}

// eventData 按 SSE 规则取出事件的 data 行内容，多行以 "\n" 连接。
func eventData(ev []byte) []byte {
	var parts [][]byte
	for _, line := range bytes.Split(ev, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			parts = append(parts, bytes.TrimPrefix(rest, []byte(" ")))
		}
	}
	return bytes.Join(parts, []byte("\n"))
}

// withData 用新的 data 重建事件：第一条 data 行替换为新内容并保留其行尾，其余 data 行删除，其他行不变。
func withData(ev, data []byte) []byte {
	var out []byte
	replaced := false
	for _, line := range bytes.SplitAfter(ev, []byte("\n")) {
		body := bytes.TrimRight(line, "\r\n")
		if !bytes.HasPrefix(body, []byte("data:")) {
			out = append(out, line...)
			continue
		}
		if replaced {
			continue
		}
		replaced = true
		out = append(out, "data: "...)
		out = append(out, data...)
		out = append(out, line[len(body):]...)
	}
	return out
}

// rawString 把原始 JSON 解析为字符串；不是字符串时 ok 为 false。
func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// itemFields 是输出项中参与比较的字段。
type itemFields struct {
	ID   json.RawMessage `json:"id"`
	Type json.RawMessage `json:"type"`
}

// parseItem 解析输出项的 id 与 type；不是对象时两者都不存在。
func parseItem(raw json.RawMessage) (id string, hasID bool, typ string) {
	var item itemFields
	if len(raw) == 0 || json.Unmarshal(raw, &item) != nil {
		return "", false, ""
	}
	id, hasID = rawString(item.ID)
	typ, _ = rawString(item.Type)
	return id, hasID, typ
}

// event 整理单个完整事件，返回新的字节切片（不与 buf 共享）。
func (t *itemIDTidy) event(ev []byte) []byte {
	unchanged := func() []byte { return append([]byte(nil), ev...) }
	data := eventData(ev)
	if len(data) == 0 || !json.Valid(data) {
		return unchanged()
	}
	var head struct {
		Type        json.RawMessage `json:"type"`
		OutputIndex json.RawMessage `json:"output_index"`
		ItemID      json.RawMessage `json:"item_id"`
		Item        json.RawMessage `json:"item"`
		Response    json.RawMessage `json:"response"`
	}
	if json.Unmarshal(data, &head) != nil {
		return unchanged()
	}
	typ, _ := rawString(head.Type)
	var index int64
	hasIndex := len(head.OutputIndex) > 0 && json.Unmarshal(head.OutputIndex, &index) == nil
	itemID, hasItemID, itemType := parseItem(head.Item)

	switch typ {
	case "response.output_item.added":
		if hasItemID && itemID != "" && hasIndex {
			if t.ids == nil {
				t.ids = map[int64]itemIDOf{}
			}
			t.ids[index] = itemIDOf{id: itemID, typ: itemType}
		}
		return unchanged()
	case "response.completed", "response.incomplete", "response.failed":
		return t.summary(ev, data, head.Response)
	}

	if !hasIndex {
		return unchanged()
	}
	known, ok := t.ids[index]
	if !ok {
		return unchanged()
	}
	topID, hasTopID := rawString(head.ItemID)
	needTop := hasTopID && topID != known.id
	needItem := hasItemID && itemID != known.id && (known.typ == "" || itemType == known.typ)
	if !needTop && !needItem {
		return unchanged()
	}
	m, ok := decodeObject(data)
	if !ok {
		return unchanged()
	}
	if needTop {
		m["item_id"] = known.id
	}
	if needItem {
		if item, ok := m["item"].(map[string]any); ok {
			item["id"] = known.id
		}
	}
	return encodeEvent(ev, m)
}

// summary 整理 completed/incomplete/failed 事件中 response.output 各项的 id。
func (t *itemIDTidy) summary(ev, data []byte, response json.RawMessage) []byte {
	unchanged := func() []byte { return append([]byte(nil), ev...) }
	var resp struct {
		Output []json.RawMessage `json:"output"`
	}
	if len(response) == 0 || json.Unmarshal(response, &resp) != nil {
		return unchanged()
	}
	fix := map[int]string{}
	for i, raw := range resp.Output {
		known, ok := t.ids[int64(i)]
		if !ok {
			continue
		}
		id, hasID, typ := parseItem(raw)
		if hasID && id != known.id && (known.typ == "" || typ == known.typ) {
			fix[i] = known.id
		}
	}
	if len(fix) == 0 {
		return unchanged()
	}
	m, ok := decodeObject(data)
	if !ok {
		return unchanged()
	}
	r, _ := m["response"].(map[string]any)
	output, _ := r["output"].([]any)
	for i, id := range fix {
		if i < len(output) {
			if item, ok := output[i].(map[string]any); ok {
				item["id"] = id
			}
		}
	}
	return encodeEvent(ev, m)
}

// decodeObject 以 UseNumber 解码 JSON 对象，保留数字原有写法。
func decodeObject(data []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// encodeEvent 重新编码 data（不转义 HTML 字符）并写回事件；编码失败时原样返回。
func encodeEvent(ev []byte, m map[string]any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(m) != nil {
		return append([]byte(nil), ev...)
	}
	return withData(ev, bytes.TrimRight(b.Bytes(), "\n"))
}
