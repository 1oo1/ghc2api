package copilot

// 本文件覆盖“隐藏模型的路径知识”：
// getModels 记录 /models 中每个 chat 模型（含隐藏、内部 id、Experimental）支持的路径，
// 没有 supported_endpoints 的 chat 模型视为只支持 /chat/completions；非 chat 模型不记录。
// 生成请求校验路径时按以下顺序取模型支持的路径：
//  1. 可见模型（出现在 /v1/models 中）：使用 /models 的 supported_endpoints；
//  2. /models 中的隐藏 chat 模型：使用 /models 的路径（为空时视为只支持 /chat/completions）；
//  3. 其余（手选的未知模型或非 chat 模型）：400。
// /v1/models 的输出只包含可见模型。model 为 "auto" 与其他手选模型一致：模型列表中没有则 400，且不访问 /auto 与 /models/session。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// endpointTestModels 是本文件使用的 /models 响应：
// visible-chat 可见且只支持 /chat/completions；hidden-resp 隐藏且只支持 /responses；hidden-noep 隐藏且未声明路径；
// gpt-x-secondary 为内部 id、exp-model 为 Experimental，二者都只支持 /responses；embed-model 不是 chat 模型。
const endpointTestModels = `{"data":[` +
	`{"id":"visible-chat","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"],"policy":{"state":"enabled"}},` +
	`{"id":"hidden-resp","model_picker_enabled":false,"capabilities":{"type":"chat"},"supported_endpoints":["/responses"]},` +
	`{"id":"hidden-noep","model_picker_enabled":false,"capabilities":{"type":"chat"}},` +
	`{"id":"hidden-empty-ep","model_picker_enabled":false,"capabilities":{"type":"chat"},"supported_endpoints":[]},` +
	`{"id":"gpt-x-secondary","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/responses"]},` +
	`{"id":"exp-model","vendor":"Experimental","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/responses"]},` +
	`{"id":"embed-model","model_picker_enabled":false,"capabilities":{"type":"embeddings"},"supported_endpoints":["/responses","/chat/completions"]}` +
	`]}`

// endpointTestCall 记录假上游收到的一次生成请求。
type endpointTestCall struct {
	Path  string
	Model string
	Token string // Copilot-Session-Token 请求头，应始终为空
}

// endpointTestUpstream 是假上游：生成接口记录请求并返回 200，其余未知路径视为错误。
type endpointTestUpstream struct {
	t *testing.T

	mu    sync.Mutex
	calls []endpointTestCall
	paths []string // 收到的全部请求路径，按到达顺序
}

func (u *endpointTestUpstream) handle(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.paths = append(u.paths, r.URL.Path)
	u.mu.Unlock()
	switch r.URL.Path {
	case "/copilot_internal/v2/token":
		testSession(w, "", time.Now().Add(time.Hour))
	case "/models":
		_, _ = io.WriteString(w, endpointTestModels)
	case "/chat/completions", "/responses", "/v1/messages":
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.calls = append(u.calls, endpointTestCall{Path: r.URL.Path, Model: body.Model, Token: r.Header.Get("Copilot-Session-Token")})
		u.mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	default:
		u.t.Errorf("unexpected path %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// requestedPaths 返回假上游收到的全部请求路径。
func (u *endpointTestUpstream) requestedPaths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

func (u *endpointTestUpstream) generationCalls() []endpointTestCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]endpointTestCall(nil), u.calls...)
}

// newEndpointTestService 创建使用假上游的服务。
func newEndpointTestService(t *testing.T, u *endpointTestUpstream) *Service {
	t.Helper()
	u.t = t
	return testService(t, Config{APIKey: testAPIKey, Token: "github-secret", Client: testClient(t, u.handle)})
}

// endpointTestBody 按 API 路径构造最小的生成请求体。
func endpointTestBody(path, model string) string {
	switch path {
	case "/v1/responses":
		return fmt.Sprintf(`{"model":%q,"input":"hello"}`, model)
	case "/v1/messages":
		return fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, model)
	default:
		return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)
	}
}

// endpointTestUpstreamPath 返回客户端路径对应的上游生成路径。
func endpointTestUpstreamPath(path string) string {
	switch path {
	case "/v1/responses":
		return "/responses"
	case "/v1/messages":
		return "/v1/messages"
	default:
		return "/chat/completions"
	}
}

// endpointTestExpectForwarded 断言请求被转发到对应上游路径，模型不变且不带 Copilot-Session-Token 头。
func endpointTestExpectForwarded(t *testing.T, s *Service, u *endpointTestUpstream, path, model string) {
	t.Helper()
	w := testRequest(s, http.MethodPost, path, endpointTestBody(path, model))
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s model=%s status=%d body=%s, want 200", path, model, w.Code, w.Body.String())
	}
	calls := u.generationCalls()
	if len(calls) != 1 {
		t.Fatalf("上游生成请求次数 = %d (%+v), want 1", len(calls), calls)
	}
	want := endpointTestCall{Path: endpointTestUpstreamPath(path), Model: model}
	if calls[0] != want {
		t.Fatalf("上游收到 %+v, want %+v", calls[0], want)
	}
}

// endpointTestExpectRejected 断言请求返回 400 且上游生成接口未被调用。
func endpointTestExpectRejected(t *testing.T, s *Service, u *endpointTestUpstream, path, requestModel string) {
	t.Helper()
	w := testRequest(s, http.MethodPost, path, endpointTestBody(path, requestModel))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST %s model=%s status=%d body=%s, want 400", path, requestModel, w.Code, w.Body.String())
	}
	if calls := u.generationCalls(); len(calls) != 0 {
		t.Fatalf("400 时上游生成接口不应被调用，实际 %+v", calls)
	}
}

// TestHiddenChatModelForwardedWhenEndpointSupported 手选隐藏的 chat 模型（含内部 id 与 Experimental）时，
// 按 /models 声明的路径放行；未声明路径时视为只支持 /chat/completions。
func TestHiddenChatModelForwardedWhenEndpointSupported(t *testing.T) {
	for _, tc := range []struct {
		name, path, model string
	}{
		{name: "hidden-responses", path: "/v1/responses", model: "hidden-resp"},
		{name: "hidden-no-endpoints-chat", path: "/v1/chat/completions", model: "hidden-noep"},
		{name: "hidden-empty-endpoints-chat", path: "/v1/chat/completions", model: "hidden-empty-ep"},
		{name: "internal-id-responses", path: "/v1/responses", model: "gpt-x-secondary"},
		{name: "experimental-responses", path: "/v1/responses", model: "exp-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &endpointTestUpstream{}
			s := newEndpointTestService(t, u)
			endpointTestExpectForwarded(t, s, u, tc.path, tc.model)
		})
	}
}

// TestHiddenChatModelRejectedWhenEndpointUnsupported 隐藏模型不支持当前路径、非 chat 模型、
// 以及 /models 中完全不存在的模型，仍返回 400 且不转发（回归保护）。
func TestHiddenChatModelRejectedWhenEndpointUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name, path, model string
	}{
		{name: "hidden-responses-on-chat", path: "/v1/chat/completions", model: "hidden-resp"},
		{name: "hidden-no-endpoints-on-responses", path: "/v1/responses", model: "hidden-noep"},
		{name: "visible-chat-on-responses", path: "/v1/responses", model: "visible-chat"},
		{name: "non-chat-model", path: "/v1/responses", model: "embed-model"},
		{name: "unknown-model-chat", path: "/v1/chat/completions", model: "not-in-models"},
		{name: "unknown-model-responses", path: "/v1/responses", model: "not-in-models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &endpointTestUpstream{}
			s := newEndpointTestService(t, u)
			endpointTestExpectRejected(t, s, u, tc.path, tc.model)
		})
	}
}

// TestModelsEndpointStillHidesHiddenModels 记录隐藏模型的路径不改变 /v1/models 的输出：只列出可见模型，不含 auto。
func TestModelsEndpointStillHidesHiddenModels(t *testing.T) {
	u := &endpointTestUpstream{}
	s := newEndpointTestService(t, u)
	w := testRequest(s, http.MethodGet, "/v1/models", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range got.Data {
		ids[m.ID] = true
	}
	want := map[string]bool{"visible-chat": true}
	if len(ids) != len(want) || !ids["visible-chat"] || ids["auto"] {
		t.Fatalf("/v1/models IDs = %v, want %v（不应追加 auto）", ids, want)
	}
}

// TestAutoModelIsTreatedAsManualModel 模型名 "auto" 与其他手选模型一致：模型列表中没有 auto 时返回 400，
// 提示模型不存在或当前账号不可用，不转发生成请求，也不访问 /auto 与 /models/session。
func TestAutoModelIsTreatedAsManualModel(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			u := &endpointTestUpstream{}
			s := newEndpointTestService(t, u)
			w := testRequest(s, http.MethodPost, path, endpointTestBody(path, "auto"))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("POST %s model=auto status=%d body=%s, want 400", path, w.Code, w.Body.String())
			}
			if want := `模型 \"auto\" 不存在或当前账号不可用`; !strings.Contains(w.Body.String(), want) {
				t.Fatalf("响应 %s 应包含 %s", w.Body.String(), want)
			}
			if calls := u.generationCalls(); len(calls) != 0 {
				t.Fatalf("400 时上游生成接口不应被调用，实际 %+v", calls)
			}
			for _, p := range u.requestedPaths() {
				if p == "/auto" || p == "/models/session" {
					t.Fatalf("不应访问上游 %s，实际请求路径 %v", p, u.requestedPaths())
				}
			}
		})
	}
}

// TestUnknownModelAndUnsupportedPathMessages 模型列表加载成功时区分两种 400：
// 模型完全不在列表中（既非可见模型也非隐藏 chat 模型，含非 chat 模型）提示“不存在或当前账号不可用”；
// 模型存在（可见或隐藏）但不支持当前路径时提示“不支持 API 路径”。两种情况都不转发生成请求。
func TestUnknownModelAndUnsupportedPathMessages(t *testing.T) {
	for _, tc := range []struct {
		name, path, model, want string
	}{
		{"unknown-chat", "/v1/chat/completions", "not-in-models", `模型 \"not-in-models\" 不存在或当前账号不可用`},
		{"unknown-responses", "/v1/responses", "not-in-models", `模型 \"not-in-models\" 不存在或当前账号不可用`},
		{"non-chat-model", "/v1/responses", "embed-model", `模型 \"embed-model\" 不存在或当前账号不可用`},
		{"visible-unsupported-path", "/v1/responses", "visible-chat", `模型 \"visible-chat\" 不支持 API 路径 /responses`},
		{"hidden-unsupported-path", "/v1/chat/completions", "hidden-resp", `模型 \"hidden-resp\" 不支持 API 路径 /chat/completions`},
		{"hidden-noep-unsupported-path", "/v1/responses", "hidden-noep", `模型 \"hidden-noep\" 不支持 API 路径 /responses`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &endpointTestUpstream{}
			s := newEndpointTestService(t, u)
			w := testRequest(s, http.MethodPost, tc.path, endpointTestBody(tc.path, tc.model))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("响应 %s 应包含 %s", w.Body.String(), tc.want)
			}
			if calls := u.generationCalls(); len(calls) != 0 {
				t.Fatalf("400 时上游生成接口不应被调用，实际 %+v", calls)
			}
		})
	}
}
