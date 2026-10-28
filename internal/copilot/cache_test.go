package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcceptTermsDoesNotReuseStalePersistentModelCache(t *testing.T) {
	enabledModels := strings.Replace(
		testModels,
		`"policy":{"state":"disabled","terms":"Accept terms"}`,
		`"policy":{"state":"enabled"}`,
		1,
	)
	if enabledModels == testModels {
		t.Fatal("enabled model fixture was not updated")
	}

	var modelCalls, acceptCalls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			switch modelCalls.Add(1) {
			case 1:
				_, _ = io.WriteString(w, testModels)
			case 2:
				_, _ = io.WriteString(w, enabledModels)
			default:
				t.Errorf("unexpected extra model request")
				w.WriteHeader(http.StatusInternalServerError)
			}
		case "/models/terms-model/policy":
			acceptCalls.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read accept-terms request: %v", err)
			}
			if r.Method != http.MethodPost || !bytes.Equal(bytes.TrimSpace(body), []byte(`{"state":"enabled"}`)) {
				t.Errorf("accept-terms request = %s %s", r.Method, body)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	config := Config{
		APIKey: testAPIKey,
		Token:  "github-secret", CacheDir: t.TempDir(), ModelsCacheTTL: time.Hour, Client: client,
	}
	current := session{Token: "session-secret", API: "https://api.githubcopilot.com"}

	firstService := testService(t, config)
	initial, err := firstService.getModels(context.Background(), current)
	if err != nil {
		t.Fatalf("initial getModels: %v", err)
	}
	if !initial.Waiting["terms-model"] {
		t.Fatal("terms-model should require accepting terms before acceptance")
	}
	if _, err := os.Stat(firstService.cacheFile("models")); err != nil {
		t.Fatalf("initial model cache was not persisted: %v", err)
	}

	firstService.acceptTerms(context.Background(), current, "terms-model")
	if got := acceptCalls.Load(); got != 1 {
		t.Fatalf("accept-terms calls = %d, want 1", got)
	}

	secondService := testService(t, config)
	refreshed, err := secondService.getModels(context.Background(), current)
	if err != nil {
		t.Fatalf("getModels after accepting terms: %v", err)
	}
	if got := modelCalls.Load(); got != 2 {
		t.Fatalf("model upstream calls = %d, want a refresh after accepting terms", got)
	}

	// 接受条款后 terms-model 应出现在可见模型列表（Data）中。
	if !modelListHasID(refreshed, "terms-model") {
		t.Fatalf("terms-model missing from refreshed model list: %s", refreshed.Data)
	}
	if refreshed.Waiting["terms-model"] {
		t.Fatal("terms-model is still waiting for terms after the refreshed enabled model list")
	}
}

// modelListHasID 判断可见模型列表（Data）中是否含有指定 id。
func modelListHasID(list modelList, id string) bool {
	for _, raw := range list.Data {
		var m model
		if json.Unmarshal(raw, &m) == nil && m.ID == id {
			return true
		}
	}
	return false
}

// TestModelsCacheHasNoPicksField 刷新模型列表后落盘的缓存不含 picks 字段；
// 旧版本留下的含 picks 字段的缓存文件仍可正常加载（未知字段被忽略），且不再访问上游。
func TestModelsCacheHasNoPicksField(t *testing.T) {
	var modelCalls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		modelCalls.Add(1)
		_, _ = io.WriteString(w, testModels)
	})
	config := Config{APIKey: testAPIKey, Token: "github-secret", CacheDir: t.TempDir(), ModelsCacheTTL: time.Hour, Client: client}
	current := session{Token: "session-secret", API: "https://api.githubcopilot.com"}

	s := testService(t, config)
	if _, err := s.getModels(context.Background(), current); err != nil {
		t.Fatalf("getModels: %v", err)
	}
	data, err := os.ReadFile(s.cacheFile("models"))
	if err != nil {
		t.Fatalf("读取模型缓存文件: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("模型缓存不是 JSON 对象: %v; body=%s", err, data)
	}
	if _, ok := fields["picks"]; ok {
		t.Fatalf("模型缓存不应再含 picks 字段: %s", data)
	}
	if _, ok := fields["data"]; !ok {
		t.Fatalf("模型缓存应含 data 字段: %s", data)
	}

	// 旧缓存：含 picks 字段，仍能加载并直接复用。
	oldConfig := config
	oldConfig.CacheDir = t.TempDir()
	old := testService(t, oldConfig)
	legacy := `{"data":[{"id":"gpt-4.1","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"]}],` +
		`"picks":["gpt-4.1"],"endpoints":{"gpt-4.1":["/chat/completions"]},"waiting":{},"at":"` + time.Now().Format(time.RFC3339Nano) + `"}`
	if err := os.MkdirAll(filepath.Dir(old.cacheFile("models")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old.cacheFile("models"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	before := modelCalls.Load()
	list, err := old.getModels(context.Background(), current)
	if err != nil {
		t.Fatalf("加载含 picks 的旧缓存: %v", err)
	}
	if got := modelCalls.Load(); got != before {
		t.Fatalf("旧缓存仍新鲜，不应访问上游，/models 调用 %d -> %d", before, got)
	}
	if !modelListHasID(list, "gpt-4.1") || !list.supportsEndpoint("gpt-4.1", "/chat/completions") {
		t.Fatalf("旧缓存内容未被正确加载: %s", list.Data)
	}
}
