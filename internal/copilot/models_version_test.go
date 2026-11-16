package copilot

// 本文件覆盖 Copilot API 版本头 X-GitHub-Api-Version 的分流：
// 只有 GET /models 使用 2026-08-01（该版本起 /models 列出模型的完整上下文窗口，并在 billing.token_prices 给出
// default 与 long_context 两档；2026-01-09 下只列出默认档窗口）；
// 生成请求（chat/completions、responses、v1/messages）与模型条款接受（POST /models/{id}/policy）仍为 2026-01-09。
// 模型元数据对外原样透传（只补 object、owned_by，以及空 supported_endpoints 时补 /chat/completions），
// 因此 2026-08-01 形状的模型数据不应被丢弃或改写。

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// 只有 GET /models 使用的版本。
	modelsListAPIVersion = "2026-08-01"
	// 生成请求与其他 Copilot API 请求使用的版本。
	defaultAPIVersion = "2026-01-09"
)

// apiVersionCall 记录假上游收到的一次 Copilot API 请求及其版本头。
type apiVersionCall struct {
	Method  string
	Path    string
	Version string
}

// apiVersionUpstream 是假上游：按 models 响应 /models，记录除 session 交换外的全部请求的版本头。
// 条款接受与生成接口一律返回 200 与空 JSON 对象。
type apiVersionUpstream struct {
	t      *testing.T
	models string

	mu    sync.Mutex
	calls []apiVersionCall
}

func (u *apiVersionUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/copilot_internal/v2/token" {
		testSession(w, "", time.Now().Add(time.Hour))
		return
	}
	u.mu.Lock()
	u.calls = append(u.calls, apiVersionCall{Method: r.Method, Path: r.URL.Path, Version: r.Header.Get("X-GitHub-Api-Version")})
	u.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/models":
		_, _ = io.WriteString(w, u.models)
	case r.Method == http.MethodPost && (r.URL.Path == "/models/terms-model/policy" ||
		r.URL.Path == "/chat/completions" || r.URL.Path == "/responses" || r.URL.Path == "/v1/messages"):
		_, _ = io.WriteString(w, `{}`)
	default:
		u.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// versionsOf 返回指定方法与路径的全部请求的版本头，按到达顺序。
func (u *apiVersionUpstream) versionsOf(method, path string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var versions []string
	for _, call := range u.calls {
		if call.Method == method && call.Path == path {
			versions = append(versions, call.Version)
		}
	}
	return versions
}

func newAPIVersionService(t *testing.T, upstream *apiVersionUpstream) *Service {
	t.Helper()
	return testService(t, Config{APIKey: testAPIKey, Token: "github-secret", Client: testClient(t, upstream.handle)})
}

// 刷新模型列表时，GET /models 必须带 2026-08-01。
func TestModelsListUsesModelsAPIVersion(t *testing.T) {
	upstream := &apiVersionUpstream{t: t, models: testModels}
	s := newAPIVersionService(t, upstream)

	if w := testRequest(s, http.MethodGet, "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d body=%s", w.Code, w.Body.String())
	}
	versions := upstream.versionsOf(http.MethodGet, "/models")
	if len(versions) != 1 {
		t.Fatalf("上游 GET /models 次数 = %d, want 1", len(versions))
	}
	if versions[0] != modelsListAPIVersion {
		t.Errorf("GET /models 的 X-GitHub-Api-Version = %q, want %q", versions[0], modelsListAPIVersion)
	}
}

// 版本头只对 GET /models 切换：同一服务中，生成请求与条款接受仍使用 2026-01-09。
func TestOnlyModelsListUsesNewAPIVersion(t *testing.T) {
	upstream := &apiVersionUpstream{t: t, models: testModels}
	s := newAPIVersionService(t, upstream)

	// terms-model 在 testModels 中待接受条款：生成请求会先接受条款（POST /models/terms-model/policy），再转发到 /chat/completions。
	// 接受条款会使模型列表缓存失效，后续请求会再次刷新 GET /models，其版本头同样应为 2026-08-01。
	for _, tc := range []struct{ local, body string }{
		{"/v1/chat/completions", `{"model":"terms-model","messages":[{"role":"user","content":"hello"}]}`},
		{"/v1/responses", `{"model":"gpt-4.1","input":"hello"}`},
		{"/v1/messages", `{"model":"gpt-4.1","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`},
	} {
		if w := testRequest(s, http.MethodPost, tc.local, tc.body); w.Code != http.StatusOK {
			t.Fatalf("POST %s status = %d body=%s", tc.local, w.Code, w.Body.String())
		}
	}

	for _, tc := range []struct{ method, path, want string }{
		{http.MethodGet, "/models", modelsListAPIVersion},
		{http.MethodPost, "/models/terms-model/policy", defaultAPIVersion},
		{http.MethodPost, "/chat/completions", defaultAPIVersion},
		{http.MethodPost, "/responses", defaultAPIVersion},
		{http.MethodPost, "/v1/messages", defaultAPIVersion},
	} {
		versions := upstream.versionsOf(tc.method, tc.path)
		if len(versions) == 0 {
			t.Errorf("上游没有收到 %s %s", tc.method, tc.path)
			continue
		}
		for _, got := range versions {
			if got != tc.want {
				t.Errorf("%s %s 的 X-GitHub-Api-Version = %q, want %q", tc.method, tc.path, got, tc.want)
			}
		}
	}
}

// opus55Model 是按 2026-08-01 形状返回的模型：完整的 1000000 窗口，billing.token_prices 带 default 与 long_context 两档。
// 策略为 enabled 且带条款文本（真实数据即如此），不应触发条款接受。
const opus55Model = `{
	"id":"claude-opus-5.5","name":"Claude Opus 5.5","vendor":"Anthropic","version":"claude-opus-5.5",
	"preview":false,"is_chat_default":false,"is_chat_fallback":false,
	"model_picker_enabled":true,"model_picker_category":"powerful","model_picker_price_category":"high",
	"billing":{
		"auto_discount":0.1,"restricted_to":["pro_plus","business","enterprise","max"],
		"token_prices":{
			"batch_size":1000000,
			"default":{"input_price":400,"output_price":2000,"cache_read_price":20,"cache_write_price":500,"cache_write_1h_price":800,"max_prompt_tokens":200000},
			"long_context":{"input_price":400,"output_price":2000,"cache_read_price":20,"cache_write_price":500,"cache_write_1h_price":800,"max_prompt_tokens":1000000}
		}
	},
	"capabilities":{
		"type":"chat","family":"claude-opus-5.5","object":"model_capabilities",
		"limits":{"max_context_window_tokens":1000000,"max_prompt_tokens":1000000,"max_output_tokens":128000,"max_non_streaming_output_tokens":16000},
		"supports":{"streaming":true,"tool_calls":true,"vision":true,"adaptive_thinking":true,"reasoning_effort":["low","medium","high","xhigh","max"]}
	},
	"policy":{"state":"enabled","terms":"Enable access to the latest Claude Opus 5.5 model from Anthropic."},
	"supported_endpoints":["/v1/messages","/chat/completions"]
}`

// 2026-08-01 形状的模型原样透传 billing 与 capabilities.limits，且可见性与 endpoints 判断正常。
func TestModels20260801ShapePassedThrough(t *testing.T) {
	upstream := &apiVersionUpstream{t: t, models: `{"data":[` + opus55Model + `]}`}
	s := newAPIVersionService(t, upstream)

	w := testRequest(s, http.MethodGet, "/v1/models", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d body=%s", w.Code, w.Body.String())
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("模型列表不是有效 JSON: %v body=%s", err, w.Body.String())
	}
	// 模型可见。
	if len(list.Data) != 1 || list.Data[0]["id"] != "claude-opus-5.5" {
		t.Fatalf("可见模型 = %v, want 仅 claude-opus-5.5", list.Data)
	}
	got := list.Data[0]
	if got["object"] != "model" || got["owned_by"] != "github-copilot" {
		t.Errorf("object/owned_by = %v/%v, want model/github-copilot", got["object"], got["owned_by"])
	}

	// 除补充的 object、owned_by 外，其余字段与上游逐字段一致。
	var want map[string]any
	if err := json.Unmarshal([]byte(opus55Model), &want); err != nil {
		t.Fatal(err)
	}
	want["object"], want["owned_by"] = "model", "github-copilot"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("模型元数据未原样透传\n got=%v\nwant=%v", got, want)
	}

	// 关键字段单独断言：完整窗口与两档价格。
	numberAt := func(root map[string]any, path ...string) any {
		var cur any = root
		for _, key := range path {
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = obj[key]
		}
		return cur
	}
	for _, tc := range []struct {
		path []string
		want float64
	}{
		{[]string{"capabilities", "limits", "max_prompt_tokens"}, 1000000},
		{[]string{"billing", "token_prices", "default", "max_prompt_tokens"}, 200000},
		{[]string{"billing", "token_prices", "long_context", "max_prompt_tokens"}, 1000000},
		{[]string{"billing", "token_prices", "default", "input_price"}, 400},
		{[]string{"billing", "token_prices", "long_context", "cache_read_price"}, 20},
	} {
		if v := numberAt(got, tc.path...); v != tc.want {
			t.Errorf("%s = %v, want %v", strings.Join(tc.path, "."), v, tc.want)
		}
	}

	// endpoints 判断正常：模型支持 /v1/messages，请求能转发到上游；且 enabled 策略不触发条款接受。
	body := `{"model":"claude-opus-5.5","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`
	if w := testRequest(s, http.MethodPost, "/v1/messages", body); w.Code != http.StatusOK {
		t.Fatalf("POST /v1/messages status = %d body=%s", w.Code, w.Body.String())
	}
	if n := len(upstream.versionsOf(http.MethodPost, "/v1/messages")); n != 1 {
		t.Errorf("上游 POST /v1/messages 次数 = %d, want 1", n)
	}
	if n := len(upstream.versionsOf(http.MethodPost, "/models/claude-opus-5.5/policy")); n != 0 {
		t.Errorf("enabled 策略的模型不应接受条款，收到 %d 次 policy 请求", n)
	}
}
