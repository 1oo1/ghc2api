package copilot

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

const (
	imageCapModel      = "gemini-4-carbon"
	imageCapOtherModel = "gemini-3.8-flash"
	imageCapUpstream   = "/chat/completions"
	imageCapURLPrefix  = "https://example.invalid/screenshot-"
	imageCap400        = `{"error":{"message":"too many images: maximum allowed for model gemini-4-carbon is 10, got 12","code":"invalid_request_body"}}`
	imageCapOK         = `{"id":"chatcmpl-imagecap","object":"chat.completion","choices":[]}`
)

func imageCapPart(index int) string {
	return fmt.Sprintf(`{"type":"image_url" ,"image_url":{"url":"%s%02d.png"}}`, imageCapURLPrefix, index)
}

func imageCapChat(model string, history, latest int) string {
	var body strings.Builder
	fmt.Fprintf(&body, "{\n  \"model\" : %q,\n  \"stream\" : false,\n  \"messages\" : [", model)
	for index := range history {
		fmt.Fprintf(&body, `{"role":"user","content":[{"type":"text","text":"old screenshot %d"},%s]},{"role":"assistant","content":"seen %d"},`, index, imageCapPart(index), index)
	}
	body.WriteString(`{"role":"user" ,"content":[{"type":"text","text":"latest screenshots"}`)
	for index := range latest {
		body.WriteByte(',')
		body.WriteString(imageCapPart(90 + index))
	}
	body.WriteString("]}],\n  \"temperature\" : 1.0\n}\n")
	return body.String()
}

func imageCapProtectedChat() string {
	return strings.ReplaceAll(imageCapChat(imageCapModel, 1, 11), `"type":"image_url" ,`, `"type":"image_url",`)
}

func imageCapService(t *testing.T, account string, respond func(string, int, []byte) staleReply) (*Service, *staleUpstream) {
	t.Helper()
	upstream := &staleUpstream{bodies: map[string][][]byte{}, headers: map[string][]http.Header{}, respond: respond}
	serve := upstream.serve(t)
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			serve(w, r)
			return
		}
		const carbon = `{"id":"gemini-4-carbon","model_picker_enabled":true,"capabilities":{"type":"chat","limits":{"max_prompt_images":1}},"supported_endpoints":["/chat/completions","/responses"],"policy":{"state":"enabled"}}`
		_, _ = io.WriteString(w, `{"data":[`+carbon+`,`+modelEntry(imageCapOtherModel, "Gemini Flash")+`,`+modelEntry("gpt-4.1", "GPT 4.1")+`]}`)
	})
	return testService(t, Config{APIKey: testAPIKey, Token: "imagecap-oauth-" + account, Client: client}), upstream
}

func assertImageCapTurn(t *testing.T, service *Service, upstream *staleUpstream, path, body string, status int, reply string, attempts int) [][]byte {
	t.Helper()
	before := len(upstream.sent(path))
	response := testRequest(service, http.MethodPost, "/v1"+path, body)
	sent := upstream.sent(path)[before:]
	if response.Code != status || response.Body.String() != reply || len(sent) != attempts {
		t.Fatalf("上游请求 %d 次，客户端收到 %d %s；期望请求 %d 次并返回 %d %s", len(sent), response.Code, response.Body.String(), attempts, status, reply)
	}
	return sent
}

func assertImageCapUnchanged(t *testing.T, got []byte, original string) {
	t.Helper()
	if string(got) != original {
		t.Fatalf("请求体应逐字节透传:\n got=%s\nwant=%s", got, original)
	}
}

func assertImageCapTrimmed(t *testing.T, got []byte, original string) {
	t.Helper()
	if !strings.Contains(string(got), "\n  \"temperature\" : 1.0\n}\n") {
		t.Fatalf("裁剪应保留顶层 temperature 的原始排版与数字字面量: %s", got)
	}
	var request struct {
		Messages []struct{ Content json.RawMessage }
	}
	if err := json.Unmarshal(got, &request); err != nil || len(request.Messages) < 3 {
		t.Fatalf("裁剪后请求缺少历史消息: %s (%v)", got, err)
	}
	want := original
	for index := range 2 {
		var parts []struct{ Type, Text string }
		if err := json.Unmarshal(request.Messages[index*2].Content, &parts); err != nil || len(parts) != 2 {
			t.Fatalf("历史图片 %d 应在原位置替换为文本: %s (%v)", index, request.Messages[index*2].Content, err)
		}
		placeholder := strings.TrimSpace(parts[1].Text)
		if parts[1].Type != "text" || len([]rune(placeholder)) > 160 || !regexp.MustCompile(`[A-Za-z]`).MatchString(placeholder) {
			t.Fatalf("历史图片 %d 应替换为简短英文提示，得到 %+v", index, parts[1])
		}
		text, _ := json.Marshal(parts[1].Text)
		want = strings.Replace(want, imageCapPart(index), `{"type":"text","text":`+string(text)+`}`, 1)
	}
	assertSameJSON(t, got, want)
}

func TestGeminiImageCapLearnsAndRetries(t *testing.T) {
	service, upstream := imageCapService(t, "first", func(_ string, _ int, body []byte) staleReply {
		var request struct{ Model string }
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("上游收到无效 JSON: %v", err)
		}
		if request.Model == imageCapModel && strings.Count(string(body), imageCapURLPrefix) > 10 {
			return staleReply{status: http.StatusBadRequest, body: imageCap400, contentType: "application/json"}
		}
		return staleReply{status: http.StatusOK, body: imageCapOK, contentType: "application/json"}
	})
	over := imageCapChat(imageCapModel, 9, 3)
	sent := assertImageCapTurn(t, service, upstream, imageCapUpstream, over, http.StatusOK, imageCapOK, 2)
	assertImageCapUnchanged(t, sent[0], over)
	assertImageCapTrimmed(t, sent[1], over)
	headers := upstream.sentHeaders(imageCapUpstream)
	if headers[0].Get("Authorization") == "" || headers[0].Get("Authorization") != headers[1].Get("Authorization") || upstream.sessionExchanges() != 1 {
		t.Fatal("重试应沿用首次发送的账号 session")
	}

	sent = assertImageCapTurn(t, service, upstream, imageCapUpstream, over, http.StatusOK, imageCapOK, 1)
	assertImageCapTrimmed(t, sent[0], over)

	atLimit := imageCapChat(imageCapModel, 7, 3)
	sent = assertImageCapTurn(t, service, upstream, imageCapUpstream, atLimit, http.StatusOK, imageCapOK, 1)
	assertImageCapUnchanged(t, sent[0], atLimit)

	protected := imageCapProtectedChat()
	sent = assertImageCapTurn(t, service, upstream, imageCapUpstream, protected, http.StatusBadRequest, imageCap400, 1)
	assertImageCapUnchanged(t, sent[0], protected)

	otherModel := imageCapChat(imageCapOtherModel, 9, 3)
	sent = assertImageCapTurn(t, service, upstream, imageCapUpstream, otherModel, http.StatusOK, imageCapOK, 1)
	assertImageCapUnchanged(t, sent[0], otherModel)

	otherService, otherUpstream := imageCapService(t, "second", staleSequence(staleReply{status: http.StatusOK, body: imageCapOK, contentType: "application/json"}))
	sent = assertImageCapTurn(t, otherService, otherUpstream, imageCapUpstream, over, http.StatusOK, imageCapOK, 1)
	assertImageCapUnchanged(t, sent[0], over)
}

func TestGeminiImageCapPassesThroughWithoutRetry(t *testing.T) {
	for _, testcase := range []struct {
		name, path, request, reply string
		status                     int
	}{
		{"latest-user-over-cap", imageCapUpstream, imageCapProtectedChat(), imageCap400, http.StatusBadRequest},
		{"unrelated-400", imageCapUpstream, imageCapChat(imageCapModel, 9, 3), `{"error":{"message":"invalid image URL","code":"invalid_request_body"}}`, http.StatusBadRequest},
		{"thousands-separator", imageCapUpstream, imageCapChat(imageCapModel, 11, 1), strings.Replace(imageCap400, "is 10,", "is 1,000,", 1), http.StatusBadRequest},
		{"not-400", imageCapUpstream, imageCapChat(imageCapModel, 9, 3), imageCap400, http.StatusUnprocessableEntity},
		{"not-gemini", imageCapUpstream, imageCapChat("gpt-4.1", 9, 3), strings.ReplaceAll(imageCap400, imageCapModel, "gpt-4.1"), http.StatusBadRequest},
		{"not-chat-completions", "/responses", imageCapChat(imageCapModel, 9, 3), imageCap400, http.StatusBadRequest},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			service, upstream := imageCapService(t, "first", staleSequence(staleReply{status: testcase.status, body: testcase.reply, contentType: "application/json"}))
			sent := assertImageCapTurn(t, service, upstream, testcase.path, testcase.request, testcase.status, testcase.reply, 1)
			assertImageCapUnchanged(t, sent[0], testcase.request)
		})
	}
}

func TestGeminiImageCapRetriesOnlyOnce(t *testing.T) {
	const retry400 = `{"error":{"message":"too many images: maximum allowed for model gemini-4-carbon is 9, got 10","code":"invalid_request_body"}}`
	service, upstream := imageCapService(t, "first", staleSequence(
		staleReply{status: http.StatusBadRequest, body: imageCap400, contentType: "application/json"},
		staleReply{status: http.StatusBadRequest, body: retry400, contentType: "application/json"},
		staleReply{status: http.StatusOK, body: imageCapOK, contentType: "application/json"},
	))
	over := imageCapChat(imageCapModel, 9, 3)
	sent := assertImageCapTurn(t, service, upstream, imageCapUpstream, over, http.StatusBadRequest, retry400, 2)
	assertImageCapUnchanged(t, sent[0], over)
	assertImageCapTrimmed(t, sent[1], over)

	sent = assertImageCapTurn(t, service, upstream, imageCapUpstream, over, http.StatusOK, imageCapOK, 1)
	if count := strings.Count(string(sent[0]), imageCapURLPrefix); count != 9 {
		t.Fatalf("下一轮首次转发应恰好发送 9 张图片，实际发送 %d 张", count)
	}
}
