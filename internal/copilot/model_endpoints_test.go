package copilot

// 本文件覆盖“隐藏模型与 Auto 选中模型的路径知识”：
// getModels 记录 /models 中每个 chat 模型（含隐藏、内部 id、Experimental）支持的路径，
// 没有 supported_endpoints 的 chat 模型视为只支持 /chat/completions；非 chat 模型不记录。
// 生成请求校验路径时按以下顺序取模型支持的路径：
//  1. 可见模型（出现在 /v1/models 中）：使用 /models 的 supported_endpoints，/auto 返回的路径不覆盖；
//  2. Auto 选中的模型且 /auto 的 selected_model 对象带非空 supported_endpoints：使用这些路径；
//  3. /models 中的隐藏 chat 模型：使用 /models 的路径（为空时视为只支持 /chat/completions）；
//  4. Auto（/auto 或 /models/session）选中但 /models 不认识的模型：视为只支持 /chat/completions；
//  5. 其余（手选的未知模型或非 chat 模型）：400。
// /v1/models 的输出不变。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	Token string // Copilot-Session-Token 请求头
}

// endpointTestUpstream 是假上游：/auto 与 /models/session 的响应由调用方给出（为空时返回 404），
// 生成接口记录请求并返回 200。
type endpointTestUpstream struct {
	t       *testing.T
	auto    string // /auto 的响应正文；为空表示 404
	session string // /models/session 的响应正文；为空表示 404

	mu    sync.Mutex
	calls []endpointTestCall
}

func (u *endpointTestUpstream) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/copilot_internal/v2/token":
		testSession(w, "", time.Now().Add(time.Hour))
	case "/models":
		_, _ = io.WriteString(w, endpointTestModels)
	case "/auto":
		if u.auto == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, u.auto)
	case "/models/session":
		if u.session == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, u.session)
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

// endpointTestAutoBody 构造 /auto 或 /models/session 的响应正文，selected 为 selected_model 的原始 JSON。
func endpointTestAutoBody(token, selected string, available ...string) string {
	availableJSON, _ := json.Marshal(available)
	if available == nil {
		availableJSON = []byte("[]")
	}
	return fmt.Sprintf(`{"session_token":%q,"selected_model":%s,"available_models":%s,"expires_at":%d}`,
		token, selected, availableJSON, time.Now().Add(time.Hour).Unix())
}

// endpointTestExpectForwarded 断言请求被转发到对应上游路径，模型与 session 头符合预期。
func endpointTestExpectForwarded(t *testing.T, s *Service, u *endpointTestUpstream, path, requestModel, wantModel, wantToken string) {
	t.Helper()
	w := testRequest(s, http.MethodPost, path, endpointTestBody(path, requestModel))
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s model=%s status=%d body=%s, want 200", path, requestModel, w.Code, w.Body.String())
	}
	calls := u.generationCalls()
	if len(calls) != 1 {
		t.Fatalf("上游生成请求次数 = %d (%+v), want 1", len(calls), calls)
	}
	want := endpointTestCall{Path: endpointTestUpstreamPath(path), Model: wantModel, Token: wantToken}
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
			endpointTestExpectForwarded(t, s, u, tc.path, tc.model, tc.model, "")
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

// TestAutoPickUsesAutoSelectedEndpoints /auto 以对象形式返回非空 supported_endpoints 时，
// 不在 /models 中或在 /models 中隐藏的选中模型按这些路径校验（覆盖隐藏模型在 /models 中声明的路径）。
func TestAutoPickUsesAutoSelectedEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected string
		path     string
		model    string
		forward  bool
	}{
		{name: "unknown-id-responses", selected: `{"id":"auto-only","supported_endpoints":["/responses"]}`,
			path: "/v1/responses", model: "auto-only", forward: true},
		{name: "unknown-id-chat-rejected", selected: `{"id":"auto-only","supported_endpoints":["/responses"]}`,
			path: "/v1/chat/completions", forward: false},
		// /auto 的路径覆盖隐藏模型在 /models 中的路径：hidden-noep 未声明路径（视为只支持 /chat/completions）。
		{name: "override-hidden-noep-responses", selected: `{"id":"hidden-noep","supported_endpoints":["/responses"]}`,
			path: "/v1/responses", model: "hidden-noep", forward: true},
		{name: "override-hidden-noep-chat-rejected", selected: `{"id":"hidden-noep","supported_endpoints":["/responses"]}`,
			path: "/v1/chat/completions", forward: false},
		// hidden-resp 在 /models 中只支持 /responses，/auto 声明只支持 /chat/completions。
		{name: "override-hidden-resp-chat", selected: `{"id":"hidden-resp","supported_endpoints":["/chat/completions"]}`,
			path: "/v1/chat/completions", model: "hidden-resp", forward: true},
		{name: "override-hidden-resp-responses-rejected", selected: `{"id":"hidden-resp","supported_endpoints":["/chat/completions"]}`,
			path: "/v1/responses", forward: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &endpointTestUpstream{auto: endpointTestAutoBody("auto-secret", tc.selected)}
			s := newEndpointTestService(t, u)
			if tc.forward {
				endpointTestExpectForwarded(t, s, u, tc.path, "auto", tc.model, "auto-secret")
			} else {
				endpointTestExpectRejected(t, s, u, tc.path, "auto")
			}
		})
	}
}

// TestAutoPickVisibleModelKeepsModelsEndpoints /auto 选中可见模型时，即使 selected_model 对象声明了不同的路径，
// 仍按 /models 的 supported_endpoints 校验：visible-chat 在 /models 中只支持 /chat/completions。
func TestAutoPickVisibleModelKeepsModelsEndpoints(t *testing.T) {
	const selected = `{"id":"visible-chat","supported_endpoints":["/responses"]}`
	for _, tc := range []struct {
		name    string
		path    string
		forward bool
	}{
		{name: "chat-forwarded", path: "/v1/chat/completions", forward: true},
		{name: "responses-rejected", path: "/v1/responses", forward: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &endpointTestUpstream{auto: endpointTestAutoBody("auto-secret", selected)}
			s := newEndpointTestService(t, u)
			if tc.forward {
				endpointTestExpectForwarded(t, s, u, tc.path, "auto", "visible-chat", "auto-secret")
			} else {
				endpointTestExpectRejected(t, s, u, tc.path, "auto")
			}
		})
	}
}

// TestAutoPickFallsBackToModelsOrChatCompletions /auto 未给出路径时：/models 认识该模型则用 /models 的路径（含隐藏模型）；
// /models 完全不认识时视为只支持 /chat/completions。
func TestAutoPickFallsBackToModelsOrChatCompletions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected string
		path     string
		model    string
		forward  bool
	}{
		{name: "unknown-string-chat", selected: `"auto-unknown"`, path: "/v1/chat/completions", model: "auto-unknown", forward: true},
		{name: "unknown-string-responses-rejected", selected: `"auto-unknown"`, path: "/v1/responses", forward: false},
		{name: "unknown-object-no-endpoints-chat", selected: `{"id":"auto-unknown"}`, path: "/v1/chat/completions", model: "auto-unknown", forward: true},
		{name: "unknown-object-empty-endpoints-responses-rejected", selected: `{"id":"auto-unknown","supported_endpoints":[]}`,
			path: "/v1/responses", forward: false},
		{name: "hidden-known-responses", selected: `"hidden-resp"`, path: "/v1/responses", model: "hidden-resp", forward: true},
		{name: "hidden-known-chat-rejected", selected: `"hidden-resp"`, path: "/v1/chat/completions", forward: false},
		{name: "hidden-known-object-responses", selected: `{"id":"hidden-resp"}`, path: "/v1/responses", model: "hidden-resp", forward: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &endpointTestUpstream{auto: endpointTestAutoBody("auto-secret", tc.selected)}
			s := newEndpointTestService(t, u)
			if tc.forward {
				endpointTestExpectForwarded(t, s, u, tc.path, "auto", tc.model, "auto-secret")
			} else {
				endpointTestExpectRejected(t, s, u, tc.path, "auto")
			}
		})
	}
}

// TestLegacyAutoPickUsesEndpointKnowledge /auto 返回 404 时走 /models/session：
// 选中模型为隐藏 chat 模型时用 /models 的路径，/models 不认识时视为只支持 /chat/completions。
func TestLegacyAutoPickUsesEndpointKnowledge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session string
		path    string
		model   string
		forward bool
	}{
		{name: "selected-hidden-known-responses", session: endpointTestAutoBody("legacy-secret", `"hidden-resp"`),
			path: "/v1/responses", model: "hidden-resp", forward: true},
		{name: "selected-unknown-chat", session: endpointTestAutoBody("legacy-secret", `"session-unknown"`),
			path: "/v1/chat/completions", model: "session-unknown", forward: true},
		{name: "selected-unknown-responses-rejected", session: endpointTestAutoBody("legacy-secret", `"session-unknown"`),
			path: "/v1/responses", forward: false},
		{name: "available-hidden-known-responses", session: endpointTestAutoBody("legacy-secret", `null`, "hidden-resp"),
			path: "/v1/responses", model: "hidden-resp", forward: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &endpointTestUpstream{session: tc.session}
			s := newEndpointTestService(t, u)
			if tc.forward {
				endpointTestExpectForwarded(t, s, u, tc.path, "auto", tc.model, "legacy-secret")
			} else {
				endpointTestExpectRejected(t, s, u, tc.path, "auto")
			}
		})
	}
}

// TestModelsEndpointStillHidesHiddenModels 记录隐藏模型的路径不改变 /v1/models 的输出：只列出可见模型与 auto。
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
	want := map[string]bool{"visible-chat": true, "auto": true}
	if len(ids) != len(want) || !ids["visible-chat"] || !ids["auto"] {
		t.Fatalf("/v1/models IDs = %v, want %v", ids, want)
	}
}
