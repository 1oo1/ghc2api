package copilot

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

func streamResponse(w http.ResponseWriter, res *http.Response) {
	defer res.Body.Close()
	copyResponseHeaders(w.Header(), res.Header)
	stream := strings.HasPrefix(strings.ToLower(res.Header.Get("Content-Type")), "text/event-stream")
	if stream {
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(res.StatusCode)
	if !stream {
		_, _ = io.Copy(w, res.Body)
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	buffer := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			if flushErr := controller.Flush(); flushErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

type message struct {
	Role    string          `json:"role"`
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

func requestInputs(body []byte) ([]message, json.RawMessage) {
	var value struct {
		Messages []message       `json:"messages"`
		Input    json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal(body, &value)
	if len(value.Messages) > 0 {
		return value.Messages, nil
	}
	var messages []message
	if json.Unmarshal(value.Input, &messages) == nil {
		return messages, nil
	}
	return nil, value.Input
}

func initiator(body []byte) string {
	messages, input := requestInputs(body)
	if len(messages) == 0 {
		var text string
		if json.Unmarshal(input, &text) == nil {
			return "user"
		}
		return "agent"
	}
	last := messages[len(messages)-1]
	if last.Role != "user" {
		return "agent"
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(last.Content, &blocks) == nil && len(blocks) > 0 {
		onlyTools := true
		for _, block := range blocks {
			onlyTools = onlyTools && block.Type == "tool_result"
		}
		if onlyTools {
			return "agent"
		}
	}
	return "user"
}

func hasVision(body []byte) bool {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return false
	}
	return containsImage(value)
}

func containsImage(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		if t == "image" || t == "image_url" || t == "input_image" {
			return true
		}
		for _, child := range v {
			if containsImage(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if containsImage(child) {
				return true
			}
		}
	}
	return false
}

func promptFrom(body []byte) string {
	messages, input := requestInputs(body)
	var text string
	if json.Unmarshal(input, &text) == nil && strings.TrimSpace(text) != "" {
		return text
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		if json.Unmarshal(messages[i].Content, &text) == nil && strings.TrimSpace(text) != "" {
			return text
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(messages[i].Content, &blocks) == nil {
			var texts []string
			for _, block := range blocks {
				if block.Type == "text" || block.Type == "input_text" {
					texts = append(texts, block.Text)
				}
			}
			if text = strings.Join(texts, "\n"); strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	return "hi"
}
