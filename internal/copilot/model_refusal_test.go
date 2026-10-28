package copilot

// 本文件覆盖模型拒绝记录的版本号机制：unrefuse 只清除请求发出前观察到的拒绝，
// 请求在途期间产生的新拒绝不会被旧请求的成功响应覆盖。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func modelRefusalVersionService(t *testing.T) *Service {
	t.Helper()
	return testService(t, Config{
		APIKey: testAPIKey, Token: "test-token", CacheDir: t.TempDir(),
	})
}

// TestUnrefuseOnlyClearsObservedRefusal unrefuse 只清除版本号与请求发出前观察到的一致的拒绝：
// 期间产生了新拒绝，或记录被删除后重建，旧版本都不得清除它。
func TestUnrefuseOnlyClearsObservedRefusal(t *testing.T) {
	const model = "gpt-4.1"
	s := modelRefusalVersionService(t)
	s.refuse(model)
	first := s.refusalVersion(model)
	s.refuse(model)
	second := s.refusalVersion(model)
	if second <= first {
		t.Fatalf("第二次拒绝版本=%d, 必须大于第一次版本=%d", second, first)
	}

	s.unrefuse(model, first)
	if !s.isRefused(model) || s.refusalVersion(model) != second {
		t.Fatal("旧版本不应清除新的拒绝")
	}
	s.unrefuse(model, second)
	if s.isRefused(model) {
		t.Fatal("版本一致时应清除拒绝")
	}

	// 删除后重建的拒绝版本号继续递增，旧版本同样不能清除它。
	s.refuse(model)
	third := s.refusalVersion(model)
	if third <= second {
		t.Fatalf("重建拒绝版本=%d, 必须大于已删除版本=%d", third, second)
	}
	s.unrefuse(model, second)
	if !s.isRefused(model) || s.refusalVersion(model) != third {
		t.Fatal("旧版本不应清除删除后重建的拒绝")
	}
	s.unrefuse(model, third)
	if s.isRefused(model) {
		t.Fatal("版本一致时应清除拒绝")
	}
}

// modelRefusalVisibleModels 返回 /v1/models 中可见的模型 ID；其他模型不应受拒绝影响。
func modelRefusalVisibleModels(t *testing.T, s *Service) map[string]bool {
	t.Helper()
	w := testRequest(s, http.MethodGet, "/v1/models", "")
	if w.Code != http.StatusOK {
		t.Fatalf("模型列表 status=%d body=%s", w.Code, w.Body.String())
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析模型列表: %v", err)
	}
	ids := make(map[string]bool)
	for _, model := range result.Data {
		ids[model.ID] = true
	}
	if !ids["terms-model"] {
		t.Errorf("其他模型不应被隐藏: %v", ids)
	}
	return ids
}

// modelRefusalAwait 等待异步请求返回，超时视为测试失败。
func modelRefusalAwait(t *testing.T, done <-chan *httptest.ResponseRecorder, phase string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-done:
		return response
	case <-time.After(10 * time.Second):
		t.Fatalf("等待%s超时", phase)
		return nil
	}
}

// TestRefusalOutlivesEarlierSuccess 拒绝之前发起的请求成功返回，不得恢复模型显示；拒绝之后发起的成功请求才会恢复。
func TestRefusalOutlivesEarlierSuccess(t *testing.T) {
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOld) }) }
	defer release()

	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/chat/completions":
			var request struct {
				Marker string `json:"test_marker"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("解析上游生成请求: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch request.Marker {
			case "old":
				startedOnce.Do(func() { close(oldStarted) })
				select {
				case <-releaseOld:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, `{}`)
			case "refuse":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":"model_not_supported"}}`)
			case "new":
				_, _ = io.WriteString(w, `{}`)
			default:
				t.Errorf("未知的生成请求标记: %q", request.Marker)
				w.WriteHeader(http.StatusInternalServerError)
			}
		default:
			t.Errorf("意外的上游路径: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	s := testService(t, Config{APIKey: testAPIKey, Token: "github-secret", CacheDir: t.TempDir(), Client: client})
	if ids := modelRefusalVisibleModels(t, s); !ids["gpt-4.1"] {
		t.Fatalf("拒绝之前模型应可见: %v", ids)
	}
	generation := func(marker string) <-chan *httptest.ResponseRecorder {
		body := `{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}],"test_marker":"` + marker + `"}`
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- testRequest(s, http.MethodPost, "/v1/chat/completions", body) }()
		return done
	}

	old := generation("old")
	select {
	case <-oldStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("等待旧请求到达上游超时")
	}
	refused := modelRefusalAwait(t, generation("refuse"), "拒绝响应")
	if refused.Code != http.StatusBadRequest || refused.Body.String() != `{"error":{"code":"model_not_supported"}}` {
		t.Fatalf("拒绝响应 status=%d body=%s", refused.Code, refused.Body.String())
	}
	if ids := modelRefusalVisibleModels(t, s); ids["gpt-4.1"] {
		t.Fatalf("拒绝后模型应隐藏: %v", ids)
	}

	release()
	if r := modelRefusalAwait(t, old, "旧请求成功响应"); r.Code != http.StatusOK {
		t.Fatalf("旧请求 status=%d body=%s, want 200", r.Code, r.Body.String())
	}
	if ids := modelRefusalVisibleModels(t, s); ids["gpt-4.1"] {
		t.Errorf("拒绝前发起的旧成功响应不应恢复模型显示: %v", ids)
	}

	if r := modelRefusalAwait(t, generation("new"), "新请求成功响应"); r.Code != http.StatusOK {
		t.Fatalf("新请求 status=%d body=%s, want 200", r.Code, r.Body.String())
	}
	if ids := modelRefusalVisibleModels(t, s); !ids["gpt-4.1"] {
		t.Errorf("拒绝后发起的成功请求应恢复模型显示: %v", ids)
	}
}
