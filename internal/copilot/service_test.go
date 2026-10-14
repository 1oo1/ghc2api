package copilot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 保留生产请求的地址供断言，只把网络目的地改为本地测试服务。
func testClient(t *testing.T, fn http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewServer(fn)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		copy.Header.Set("Test-Original-Host", u.Host)
		u.Scheme, u.Host = target.Scheme, target.Host
		copy.URL = &u
		return http.DefaultTransport.RoundTrip(copy)
	})}
}

func testJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func testSession(w http.ResponseWriter, api string, expiry time.Time) {
	testJSON(w, map[string]any{
		"token": "session-secret", "expires_at": expiry.Unix(),
		"endpoints": map[string]string{"api": api},
	})
}

type persistedSessionForTest struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	API       string `json:"api"`
}

const testModels = `{"data":[{"id":"gpt-4.1","name":"GPT 4.1","model_picker_enabled":true,"is_chat_fallback":true,"capabilities":{"type":"chat","supports":{"reasoning_effort":["low","high"]}},"supported_endpoints":["/chat/completions","/responses","/v1/messages"],"policy":{"state":"enabled"}},{"id":"terms-model","model_picker_enabled":true,"capabilities":{"type":"chat"},"policy":{"state":"disabled","terms":"Accept terms"}},{"id":"blocked","model_picker_enabled":true,"capabilities":{"type":"chat"},"policy":{"state":"disabled"}},{"id":"embed","model_picker_enabled":true,"capabilities":{"type":"embeddings"}},{"id":"copilot-search","model_picker_enabled":true,"capabilities":{"type":"chat"}},{"id":"gpt-secondary","model_picker_enabled":true,"capabilities":{"type":"chat"}},{"id":"hidden","capabilities":{"type":"chat"}},{"id":"experimental","vendor":"Experimental","model_picker_enabled":true,"capabilities":{"type":"chat"}}]}`

func testService(t *testing.T, config Config) *Service {
	t.Helper()
	if strings.TrimSpace(config.CacheDir) == "" {
		config.CacheDir = t.TempDir()
	}
	s, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func captureSlog(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	output := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return output
}

func captureJSONSlog(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	output := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return output
}

type debugBodyLogEvent struct {
	URL       string `json:"url"`
	Body      string `json:"body"`
	BodyBytes int64  `json:"body_bytes"`
}

func debugBodyLogEvents(t *testing.T, output []byte, endpoint string) []debugBodyLogEvent {
	t.Helper()
	var events []debugBodyLogEvent
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var event debugBodyLogEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("invalid JSON log entry: %v", err)
		}
		if strings.Contains(event.URL, endpoint) && event.BodyBytes > 0 {
			events = append(events, event)
		}
	}
	return events
}

func testRequest(s *Service, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	for _, config := range []Config{
		{}, {Token: "  "},
		{Token: "token", Host: "evil.example"},
		{Token: "token", Host: "github.com:443"},
		{Token: "token", Host: "https://user" + "@" + "github.com"},
		{Token: "token", Host: "https://github.com/path"},
		{Token: "token", Host: "api.acme.ghe.com"},
	} {
		t.Run(fmt.Sprintf("%s_%s", config.Token, config.Host), func(t *testing.T) {
			if _, err := New(config); err == nil {
				t.Fatalf("New(%+v) must reject invalid configuration", config)
			}
		})
	}
}

func TestConfigDoesNotExposeTokenMode(t *testing.T) {
	if _, ok := reflect.TypeOf(Config{}).FieldByName("TokenMode"); ok {
		t.Fatal("Config must not expose token_mode")
	}
}

func TestLocalAuthenticationAndHealth(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/copilot_internal/v2/token" {
			testSession(w, "https://api.githubcopilot.com", time.Now().Add(time.Hour))
		} else {
			_, _ = io.WriteString(w, testModels)
		}
	})
	s := testService(t, Config{Token: "github-secret", APIKey: "local-secret", Client: client})
	if w := testRequest(s, "GET", "/healthz", ""); w.Code != 200 {
		t.Fatalf("health status = %d", w.Code)
	}
	for _, path := range []string{"/v1/models", "/models", "/usage", "/v1/usage", "/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		if w := testRequest(s, "GET", path, ""); w.Code != 401 {
			t.Fatalf("%s without key: status = %d", path, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unauthenticated requests reached upstream")
	}
	for _, value := range []string{"local-secret", "Basic local-secret", "Bearer wrong-secret", "Bearer local-secret trailing"} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", value)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("invalid authorization %q accepted: status=%d", value, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid authorization reached upstream")
	}
	for _, header := range []string{"Authorization", "X-Api-Key"} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		value := "local-secret"
		if header == "Authorization" {
			value = "Bearer " + value
		}
		r.Header.Set(header, value)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s authenticated status = %d, body = %s", header, w.Code, w.Body.String())
		}
	}
}

func TestModelsFilterAndPreserveMetadata(t *testing.T) {
	var modelCalls, tokenCalls atomic.Int32
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			tokenCalls.Add(1)
			if r.Header.Get("Authorization") != "token github-secret" || r.Header.Get("Editor-Version") != "vscode/1.140.0" {
				t.Errorf("token exchange headers = %v", r.Header)
			}
			testSession(w, "https://api.individual.githubcopilot.com", time.Now().Add(time.Hour))
		case "/models":
			modelCalls.Add(1)
			if r.Header.Get("Test-Original-Host") != "api.individual.githubcopilot.com" || r.Header.Get("Authorization") != "Bearer session-secret" {
				t.Errorf("model request host or authorization = %v", r.Header)
			}
			_, _ = io.WriteString(w, testModels)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})})
	for _, path := range []string{"/v1/models", "/models"} {
		w := testRequest(s, "GET", path, "")
		if w.Code != 200 {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var got struct {
			Object string           `json:"object"`
			Data   []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Object != "list" {
			t.Fatalf("object = %q", got.Object)
		}
		ids := map[string]bool{}
		for _, m := range got.Data {
			id, _ := m["id"].(string)
			ids[id] = true
			if id == "gpt-4.1" {
				if !reflect.DeepEqual(m["supported_endpoints"], []any{"/chat/completions", "/responses", "/v1/messages"}) || m["capabilities"] == nil || m["name"] != "GPT 4.1" {
					t.Errorf("model metadata lost: %#v", m)
				}
			}
		}
		if !reflect.DeepEqual(ids, map[string]bool{"gpt-4.1": true, "terms-model": true, "auto": true}) {
			t.Fatalf("visible IDs = %v", ids)
		}
	}
	if tokenCalls.Load() != 1 || modelCalls.Load() != 1 {
		t.Fatalf("cache calls: token=%d models=%d", tokenCalls.Load(), modelCalls.Load())
	}
}

func TestModelsCachePersistsAcrossServiceInstances(t *testing.T) {
	var modelCalls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			modelCalls.Add(1)
			_, _ = io.WriteString(w, testModels)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	config := Config{
		Token: "github-secret", CacheDir: t.TempDir(), ModelsCacheTTL: time.Hour, Client: client,
	}

	var firstService *Service
	for range 2 {
		s := testService(t, config)
		if firstService == nil {
			firstService = s
		}
		w := testRequest(s, "GET", "/v1/models", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if got := modelCalls.Load(); got != 1 {
		t.Fatalf("model upstream calls = %d, want 1 across Service instances", got)
	}
	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(firstService.cacheDir)
		if err != nil {
			t.Fatalf("stat account cache directory: %v", err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Errorf("account cache directory permissions = %04o, want 0700", got)
		}
		fileInfo, err := os.Stat(firstService.cacheFile("models"))
		if err != nil {
			t.Fatalf("stat model cache file: %v", err)
		}
		if got := fileInfo.Mode().Perm(); got != 0o600 {
			t.Errorf("model cache file permissions = %04o, want 0600", got)
		}
	}
}

func TestModelsCacheRefreshesAfterTTL(t *testing.T) {
	const ttl = 30 * time.Millisecond
	var modelCalls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			modelCalls.Add(1)
			_, _ = io.WriteString(w, testModels)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	config := Config{
		Token: "github-secret", CacheDir: t.TempDir(), ModelsCacheTTL: ttl, Client: client,
	}

	s := testService(t, config)
	if w := testRequest(s, "GET", "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("initial request status=%d body=%s", w.Code, w.Body.String())
	}
	time.Sleep(ttl + 20*time.Millisecond)

	if w := testRequest(s, "GET", "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("request after TTL status=%d body=%s", w.Code, w.Body.String())
	}
	if got := modelCalls.Load(); got != 2 {
		t.Fatalf("model upstream calls after TTL = %d, want 2", got)
	}
}

func TestGenerationRoutesHeadersAndOriginalBody(t *testing.T) {
	for _, tc := range []struct{ local, upstream, body string }{
		{"/v1/chat/completions", "/chat/completions", `{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}],"temperature":0.7,"extra":{"keep":true}}`},
		{"/chat/completions", "/chat/completions", `{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`},
		{"/v1/responses", "/responses", `{"model":"gpt-4.1","input":"hello","reasoning":{"effort":"high"}}`},
		{"/responses", "/responses", `{"model":"gpt-4.1","input":"hello"}`},
		{"/v1/messages", "/v1/messages", `{"model":"gpt-4.1","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`},
	} {
		t.Run(tc.local, func(t *testing.T) {
			var generationCalls int
			s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/copilot_internal/v2/token":
					testSession(w, "https://api.githubcopilot.com", time.Now().Add(time.Hour))
				case "/models":
					_, _ = io.WriteString(w, testModels)
				case tc.upstream:
					generationCalls++
					if r.Method != "POST" || r.URL.RawQuery != "beta=true&foo=bar" {
						t.Errorf("request method/query = %s %s", r.Method, r.URL.RawQuery)
					}
					b, _ := io.ReadAll(r.Body)
					if string(b) != tc.body {
						t.Errorf("manual body changed: %s", b)
					}
					for key, want := range map[string]string{
						"Authorization": "Bearer session-secret", "Editor-Version": "vscode/1.140.0",
						"Editor-Plugin-Version": "copilot-chat/0.68.0", "Copilot-Integration-Id": "vscode-chat",
						"User-Agent": "GitHubCopilotChat/0.68.0", "X-GitHub-Api-Version": "2026-01-09",
						"Openai-Intent": "conversation-panel", "X-Initiator": "user", "Anthropic-Version": "2023-06-01",
					} {
						if got := r.Header.Get(key); got != want {
							t.Errorf("%s = %q, want %q", key, got, want)
						}
					}
					for _, key := range []string{"Cookie", "X-Api-Key", "Proxy-Authorization", "X-Hop", "Copilot-Session-Token"} {
						if got := r.Header.Get(key); got != "" {
							t.Errorf("unsafe caller header %s leaked: %q", key, got)
						}
					}
					w.Header().Set("X-Upstream", "preserved")
					w.Header().Set("Connection", "X-Secret")
					w.Header().Set("X-Secret", "private")
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":"quota exhausted"}`)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})})
			r := httptest.NewRequest("POST", tc.local+"?beta=true&foo=bar", strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer caller")
			r.Header.Set("X-Api-Key", "caller")
			r.Header.Set("Cookie", "private=1")
			r.Header.Set("Proxy-Authorization", "private")
			r.Header.Set("Connection", "X-Hop")
			r.Header.Set("X-Hop", "private")
			r.Header.Set("Anthropic-Version", "2023-06-01")
			r.Header.Set("Copilot-Session-Token", "injected")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 429 || w.Body.String() != `{"error":"quota exhausted"}` || w.Header().Get("X-Upstream") != "preserved" || generationCalls != 1 {
				t.Fatalf("response = %d %s headers=%v calls=%d", w.Code, w.Body.String(), w.Header(), generationCalls)
			}
			if w.Header().Get("X-Secret") != "" || w.Header().Get("Connection") != "" {
				t.Fatalf("upstream hop-by-hop headers leaked: %v", w.Header())
			}
		})
	}
}

func TestInitiatorAndStructuredVision(t *testing.T) {
	for _, tc := range []struct{ name, path, body, initiator, vision string }{
		{"chat-user-image", "/v1/chat/completions", `{"model":"gpt-4.1","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`, "user", "true"},
		{"chat-tool", "/v1/chat/completions", `{"model":"gpt-4.1","messages":[{"role":"tool","content":"result"}]}`, "agent", ""},
		{"anthropic-result", "/v1/messages", `{"model":"gpt-4.1","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}]}`, "agent", ""},
		{"anthropic-mixed", "/v1/messages", `{"model":"gpt-4.1","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"},{"type":"text","text":"new question"}]}]}`, "user", ""},
		{"anthropic-image", "/v1/messages", `{"model":"gpt-4.1","messages":[{"role":"user","content":[{"type": "image","source":{"type":"base64","data":"abc"}}]}]}`, "user", "true"},
		{"response-text", "/v1/responses", `{"model":"gpt-4.1","input":"hello"}`, "user", ""},
		{"response-tool", "/v1/responses", `{"model":"gpt-4.1","input":[{"role":"user","content":"hello"},{"type":"function_call_output","call_id":"1","output":"ok"}]}`, "agent", ""},
		{"response-image", "/v1/responses", `{"model":"gpt-4.1","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/image.png"}]}]}`, "user", "true"},
		{"text-not-image", "/v1/chat/completions", `{"model":"gpt-4.1","messages":[{"role":"user","content":"The key \"image_url\" is mentioned as plain text"}]}`, "user", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/copilot_internal/v2/token":
					testSession(w, "", time.Now().Add(time.Hour))
				case "/models":
					_, _ = io.WriteString(w, testModels)
				default:
					if got := r.Header.Get("X-Initiator"); got != tc.initiator {
						t.Errorf("X-Initiator = %q, want %q", got, tc.initiator)
					}
					if got := r.Header.Get("Copilot-Vision-Request"); got != tc.vision {
						t.Errorf("vision = %q, want %q", got, tc.vision)
					}
					testJSON(w, map[string]string{"ok": "yes"})
				}
			})})
			if w := testRequest(s, "POST", tc.path, tc.body); w.Code != 200 {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDebugLogsRequestStagesAndRedactsSecrets(t *testing.T) {
	logs := captureSlog(t, slog.LevelDebug)
	var events []string
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		events = append(events, r.URL.Path)
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			if r.Header.Get("Authorization") != "token github-oauth-secret" {
				t.Errorf("OAuth exchange authorization = %q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"copilot-session-secret","expires_at":%d,"endpoints":{"api":"https://api.githubcopilot.com"}}`,
				time.Now().Add(time.Hour).Unix())
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/auto":
			testJSON(w, map[string]any{
				"session_token":  "copilot-auto-session-secret",
				"selected_model": "gpt-4.1",
				"expires_at":     time.Now().Add(time.Hour).Unix(),
			})
		case "/chat/completions":
			if r.Header.Get("Authorization") != "Bearer copilot-session-secret" {
				t.Errorf("generation authorization = %q", r.Header.Get("Authorization"))
			}
			if r.Header.Get("Copilot-Session-Token") != "copilot-auto-session-secret" {
				t.Errorf("Auto session header = %q", r.Header.Get("Copilot-Session-Token"))
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if !strings.Contains(string(body), "upstream-request-marker") {
				t.Errorf("generation request body = %s", body)
			}
			_, _ = io.WriteString(w, `{"result":"upstream-response-marker"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	s := testService(t, Config{
		Token:  "github-oauth-secret",
		APIKey: "service-api-key-secret",
		Client: client,
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"upstream-request-marker"}]}`))
	request.Header.Set("Authorization", "Bearer service-api-key-secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, request)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "upstream-response-marker") {
		t.Fatalf("generation response = %d %s", w.Code, w.Body.String())
	}

	wantEvents := []string{"/copilot_internal/v2/token", "/models", "/auto", "/chat/completions"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("upstream stages = %v, want %v", events, wantEvents)
	}
	logOutput := logs.String()
	for _, stage := range []string{
		"/v1/chat/completions",
		"/copilot_internal/v2/token",
		"/models",
		"/auto",
		"/chat/completions",
	} {
		if !strings.Contains(logOutput, stage) {
			t.Errorf("request stage %q missing from logs: %s", stage, logOutput)
		}
	}
	for _, marker := range []string{"upstream-request-marker", "upstream-response-marker"} {
		if !strings.Contains(logOutput, marker) {
			t.Errorf("debug API body marker %q missing from logs: %s", marker, logOutput)
		}
	}
	for _, secret := range []string{
		"github-oauth-secret",
		"copilot-session-secret",
		"copilot-auto-session-secret",
		"service-api-key-secret",
	} {
		if strings.Contains(logOutput, secret) {
			t.Errorf("sensitive value %q leaked into logs: %s", secret, logOutput)
		}
	}
}

func TestGenerationLogsRedactModelMatchingOAuthToken(t *testing.T) {
	logs := captureSlog(t, slog.LevelDebug)
	const oauthToken = "oauth-model-token-secret"
	s := testService(t, Config{
		Token: oauthToken,
		Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/copilot_internal/v2/token":
				testSession(w, "https://api.githubcopilot.com", time.Now().Add(time.Hour))
			case "/models":
				_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"],"policy":{"state":"enabled"}}]}`, oauthToken)
			case "/chat/completions":
				_, _ = io.WriteString(w, `{"ok":true}`)
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}),
	})

	requestBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, oauthToken)
	w := testRequest(s, http.MethodPost, "/v1/chat/completions", requestBody)
	if w.Code != http.StatusOK {
		t.Fatalf("generation response = %d %s", w.Code, w.Body.String())
	}
	if logOutput := logs.String(); strings.Contains(logOutput, oauthToken) {
		t.Fatalf("OAuth token leaked through the client model field: %s", logOutput)
	}
}

func TestGenerationUpstreamFailureStatusesAreLoggedAtFailureLevel(t *testing.T) {
	for _, tc := range []struct {
		status int
		level  string
	}{
		{http.StatusTooManyRequests, "WARN"},
		{http.StatusInternalServerError, "ERROR"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			logs := captureSlog(t, slog.LevelDebug)
			body := fmt.Sprintf(`{"error":"upstream status %d"}`, tc.status)
			s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/copilot_internal/v2/token":
					testSession(w, "https://api.githubcopilot.com", time.Now().Add(time.Hour))
				case "/models":
					_, _ = io.WriteString(w, testModels)
				case "/chat/completions":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, body)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			})})

			w := testRequest(s, http.MethodPost, "/v1/chat/completions",
				`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`)
			if w.Code != tc.status || w.Body.String() != body {
				t.Fatalf("upstream response = %d %s, want %d %s", w.Code, w.Body.String(), tc.status, body)
			}

			wantLevel := "level=" + tc.level
			wantStatus := fmt.Sprintf("status=%d", tc.status)
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, wantLevel) && strings.Contains(line, wantStatus) {
					return
				}
			}
			t.Fatalf("no %s log event recorded for upstream HTTP %d", tc.level, tc.status)
		})
	}
}

func TestDebugLogsCompleteLargeAPIRequestInBoundedEvents(t *testing.T) {
	const endpoint = "/debug/request-body"
	const tailMarker = "debug-request-body-tail-marker"
	const maxLogEventSize = 1 << 20
	requestBody := `{"payload":"` + strings.Repeat("x", 2<<20) + `","tail":"` + tailMarker + `"}`
	if !json.Valid([]byte(requestBody)) {
		t.Fatal("large request body is not valid JSON")
	}

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(got, []byte(requestBody)) {
			t.Errorf("transport received %d request bytes, want %d", len(got), len(requestBody))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    r,
		}, nil
	})}
	s := testService(t, Config{Token: "github-secret", Client: client, Logger: logger})

	request, err := http.NewRequest(http.MethodPost,
		"https://api.githubcopilot.com"+endpoint, strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.doAPI(context.Background(), request)
	if err != nil {
		t.Fatalf("doAPI() error = %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Errorf("close upstream response body: %v", err)
	}

	type requestBodyEvent struct {
		body     string
		bodySize int64
	}
	type logFields struct {
		URL       string `json:"url"`
		Body      string `json:"body"`
		BodyBytes int64  `json:"body_bytes"`
		Request   struct {
			URL       string `json:"url"`
			Body      string `json:"body"`
			BodyBytes int64  `json:"body_bytes"`
		} `json:"request"`
	}
	var events []requestBodyEvent
	for _, line := range bytes.Split(logs.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var fields logFields
		if err := json.Unmarshal(line, &fields); err != nil {
			t.Fatalf("invalid JSON log entry: %v", err)
		}
		eventURL := fields.URL
		if eventURL == "" {
			eventURL = fields.Request.URL
		}
		if !strings.Contains(eventURL, endpoint) {
			continue
		}
		body, bodySize := fields.Body, fields.BodyBytes
		if body == "" {
			body, bodySize = fields.Request.Body, fields.Request.BodyBytes
		}
		if body == "" {
			continue
		}
		events = append(events, requestBodyEvent{body: body, bodySize: bodySize})
		if len(line) >= maxLogEventSize {
			t.Errorf("request body log event is %d bytes, want less than 1 MiB", len(line))
		}
		if len(body) >= maxLogEventSize {
			t.Errorf("request body chunk is %d bytes, want less than 1 MiB", len(body))
		}
	}
	if len(events) < 2 {
		t.Fatalf("large request body should be split across multiple debug log events, got %d", len(events))
	}

	var loggedBody strings.Builder
	var totalBytes int64
	for _, event := range events {
		totalBytes += event.bodySize
		loggedBody.WriteString(event.body)
		if event.bodySize != 0 && event.bodySize != int64(len(event.body)) {
			t.Errorf("request body event reports %d bytes for a %d-byte chunk", event.bodySize, len(event.body))
		}
	}
	if totalBytes != int64(len(requestBody)) {
		t.Errorf("request body log chunks total %d bytes, want %d", totalBytes, len(requestBody))
	}
	if !strings.Contains(loggedBody.String(), tailMarker) {
		t.Errorf("reassembled request body is missing tail marker %q", tailMarker)
	}
	if loggedBody.String() != requestBody {
		t.Errorf("reassembled request body differs from all %d input bytes", len(requestBody))
	}
}

func TestDebugLogsCompleteLargeAPIResponse(t *testing.T) {
	logs := captureJSONSlog(t, slog.LevelDebug)
	const firstMarker = "debug-response-first-marker"
	const middleMarker = "debug-response-middle-marker"
	const tailMarker = "debug-response-tail-marker"
	responseLength := maxDebugBodySize + 1
	middleOffset := responseLength/2 + 137
	responseBody := firstMarker +
		strings.Repeat("x", middleOffset-len(firstMarker)) +
		middleMarker +
		strings.Repeat("x", responseLength-middleOffset-len(middleMarker)) +
		tailMarker
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "https://api.githubcopilot.com", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/chat/completions":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, responseBody)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})})

	w := testRequest(s, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusOK || w.Body.String() != responseBody {
		t.Fatalf("large upstream response was not fully proxied: status=%d bytes=%d", w.Code, w.Body.Len())
	}

	events := debugBodyLogEvents(t, logs.Bytes(), "/chat/completions")
	if len(events) < 2 {
		t.Fatalf("large response should be represented by multiple debug log chunks, got %d", len(events))
	}
	var totalBytes int64
	var loggedBody strings.Builder
	for _, event := range events {
		totalBytes += event.BodyBytes
		loggedBody.WriteString(event.Body)
	}
	if totalBytes != int64(len(responseBody)) {
		t.Fatalf("debug response chunks total %d bytes, want %d", totalBytes, len(responseBody))
	}
	for _, marker := range []string{firstMarker, middleMarker, tailMarker} {
		if !strings.Contains(loggedBody.String(), marker) {
			t.Errorf("debug response chunks are missing marker %q", marker)
		}
	}
	if loggedBody.String() != responseBody {
		t.Fatalf("concatenated debug response chunks do not match all %d response bytes", len(responseBody))
	}
}

func TestDebugLogsDoNotLeakRegisteredSecretsAcrossChunkBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		oauth  string
		apiKey string
		secret string
	}{
		{
			name:   "oauth",
			oauth:  "oauth-credential-cross-chunk-secret",
			apiKey: "local-api-key",
			secret: "oauth-credential-cross-chunk-secret",
		},
		{
			name:   "api-key",
			oauth:  "oauth-credential",
			apiKey: "api-key-cross-chunk-secret",
			secret: "api-key-cross-chunk-secret",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureJSONSlog(t, slog.LevelDebug)
			responseBody := strings.Repeat("x", debugLogChunkSize-len(tc.secret)/2) + tc.secret
			responseBody += strings.Repeat("y", debugLogChunkSize*4-len(responseBody))
			s := testService(t, Config{
				Token:  tc.oauth,
				APIKey: tc.apiKey,
				Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/copilot_internal/v2/token":
						testSession(w, "https://api.githubcopilot.com", time.Now().Add(time.Hour))
					case "/models":
						_, _ = io.WriteString(w, testModels)
					case "/chat/completions":
						w.Header().Set("Content-Type", "text/plain")
						_, _ = io.WriteString(w, responseBody)
					default:
						t.Errorf("unexpected path %s", r.URL.Path)
						w.WriteHeader(http.StatusInternalServerError)
					}
				}),
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`))
			request.Header.Set("Authorization", "Bearer "+tc.apiKey)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, request)
			if w.Code != http.StatusOK || w.Body.String() != responseBody {
				t.Fatalf("upstream response = %d with %d bytes, want %d bytes",
					w.Code, w.Body.Len(), len(responseBody))
			}

			events := debugBodyLogEvents(t, logs.Bytes(), "/chat/completions")
			var loggedBody strings.Builder
			for _, event := range events {
				loggedBody.WriteString(event.Body)
			}
			if strings.Contains(loggedBody.String(), tc.secret) {
				t.Fatalf("registered %s secret can be reconstructed from debug chunks", tc.name)
			}
			if strings.Contains(logs.String(), tc.secret) {
				t.Fatalf("registered %s secret appears in debug logs", tc.name)
			}
		})
	}
}

func TestDebugLogsKeepTailAfterOversizedSensitiveJSONField(t *testing.T) {
	logs := captureJSONSlog(t, slog.LevelDebug)
	const sensitiveValueLength = (1 << 20) + 1
	const tailMarker = "debug-tail-after-oversized-sensitive-field"
	sensitiveValue := strings.Repeat("s", sensitiveValueLength)
	responseBody := `{"session_token":"` + sensitiveValue + `","tail":"` + tailMarker + `"}`
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "https://api.githubcopilot.com", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, responseBody)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})})

	w := testRequest(s, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusOK || w.Body.String() != responseBody {
		t.Fatalf("upstream response = %d with %d bytes, want %d bytes",
			w.Code, w.Body.Len(), len(responseBody))
	}

	events := debugBodyLogEvents(t, logs.Bytes(), "/chat/completions")
	var loggedBody strings.Builder
	for _, event := range events {
		loggedBody.WriteString(event.Body)
	}
	body := loggedBody.String()
	if strings.Contains(body, sensitiveValue) {
		t.Error("debug response logs contain the oversized sensitive value")
	}
	if !strings.Contains(body, `"[REDACTED]"`) {
		t.Error("debug response logs do not show the sensitive JSON field as redacted")
	}
	if !strings.Contains(body, tailMarker) {
		t.Errorf("debug response logs dropped the body tail marker %q", tailMarker)
	}
}

func TestDebugLogsCompleteOversizedMetadataResponse(t *testing.T) {
	logs := captureJSONSlog(t, slog.LevelDebug)
	const tailMarker = "metadata-response-tail-marker"
	responseBody := strings.Repeat("m", maxMetadataBody+1) + tailMarker
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "https://api.githubcopilot.com", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, responseBody)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})})

	w := testRequest(s, http.MethodGet, "/v1/models", "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("oversized metadata response status = %d, want %d: %s",
			w.Code, http.StatusBadGateway, w.Body.String())
	}

	events := debugBodyLogEvents(t, logs.Bytes(), "/models")
	if len(events) == 0 {
		t.Fatal("debug logs contain no body chunks for the oversized /models response")
	}
	var totalBytes int64
	var loggedBody strings.Builder
	for _, event := range events {
		totalBytes += event.BodyBytes
		loggedBody.WriteString(event.Body)
	}
	if totalBytes != int64(len(responseBody)) {
		t.Fatalf("debug metadata response chunks total %d bytes, want %d", totalBytes, len(responseBody))
	}
	if !strings.Contains(loggedBody.String(), tailMarker) {
		t.Fatalf("debug metadata response log is missing tail marker %q", tailMarker)
	}
	if loggedBody.String() != responseBody {
		t.Fatalf("debug metadata response chunks do not contain all %d bytes", len(responseBody))
	}
}

func TestDebugLogsCompleteLargeBufferedMetadataResponseInBoundedEvents(t *testing.T) {
	const endpoint = "/models"
	const tailMarker = "buffered-metadata-tail-marker"
	const maxLogEventSize = 1 << 20
	responseBody := `{"data":[{"id":"` + strings.Repeat("m", 2<<20) +
		`"}],"tail":"` + tailMarker + `"}`
	if !json.Valid([]byte(responseBody)) {
		t.Fatal("large metadata response is not valid JSON")
	}

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != endpoint {
			return nil, fmt.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    r,
		}, nil
	})}
	s := testService(t, Config{Token: "github-secret", Client: client, Logger: logger})

	request, err := http.NewRequest(http.MethodGet, "https://api.githubcopilot.com"+endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := s.doAPI(context.Background(), request)
	if err != nil {
		t.Fatalf("doAPI() error = %v", err)
	}
	got, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		t.Fatalf("read complete metadata response: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close metadata response: %v", closeErr)
	}
	if !bytes.Equal(got, []byte(responseBody)) {
		t.Fatalf("metadata response was not fully consumed: got %d bytes, want %d", len(got), len(responseBody))
	}

	events := debugBodyLogEvents(t, logs.Bytes(), endpoint)
	if len(events) < 2 {
		t.Errorf("large buffered metadata body should be split across debug log events, got %d", len(events))
	}
	for _, event := range events {
		if event.BodyBytes >= maxLogEventSize {
			t.Errorf("metadata body event contains %d bytes, want less than 1 MiB", event.BodyBytes)
		}
		if len(event.Body) >= maxLogEventSize {
			t.Errorf("metadata body chunk is %d bytes, want less than 1 MiB", len(event.Body))
		}
	}
	for _, line := range bytes.Split(logs.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var fields struct {
			URL       string `json:"url"`
			BodyBytes int64  `json:"body_bytes"`
		}
		if err := json.Unmarshal(line, &fields); err != nil {
			t.Fatalf("invalid JSON log entry: %v", err)
		}
		if strings.Contains(fields.URL, endpoint) && fields.BodyBytes > 0 && len(line) >= maxLogEventSize {
			t.Errorf("metadata JSONL event is %d bytes, want less than 1 MiB", len(line))
		}
	}

	var loggedBody strings.Builder
	var totalBytes int64
	for _, event := range events {
		totalBytes += event.BodyBytes
		loggedBody.WriteString(event.Body)
	}
	if totalBytes != int64(len(responseBody)) {
		t.Errorf("metadata body log chunks total %d bytes, want %d", totalBytes, len(responseBody))
	}
	if !strings.Contains(loggedBody.String(), tailMarker) {
		t.Errorf("reassembled metadata body is missing tail marker %q", tailMarker)
	}
	if loggedBody.String() != responseBody {
		t.Errorf("reassembled metadata body differs from all %d response bytes", len(responseBody))
	}
}

func TestInfoLogsRequestStagesWithoutDebugBodies(t *testing.T) {
	logs := captureSlog(t, slog.LevelInfo)
	s := testService(t, Config{Token: "github-oauth-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/chat/completions":
			_, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, `{"result":"info-response-body-marker"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4.1","messages":[{"role":"user","content":"info-request-body-marker"}]}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("generation response = %d %s", w.Code, w.Body.String())
	}
	logOutput := logs.String()
	for _, stage := range []string{"/v1/chat/completions", "/copilot_internal/v2/token", "/models", "/chat/completions"} {
		if !strings.Contains(logOutput, stage) {
			t.Errorf("info request stage %q missing from logs: %s", stage, logOutput)
		}
	}
	for _, marker := range []string{"info-request-body-marker", "info-response-body-marker"} {
		if strings.Contains(logOutput, marker) {
			t.Errorf("info logs contain debug body marker %q: %s", marker, logOutput)
		}
	}
}

func TestTermsAcceptedBeforeGenerationAndFailureDoesNotBlock(t *testing.T) {
	for _, policyStatus := range []int{200, 403} {
		t.Run(fmt.Sprint(policyStatus), func(t *testing.T) {
			var events []string
			s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
				events = append(events, r.URL.Path)
				switch r.URL.Path {
				case "/copilot_internal/v2/token":
					testSession(w, "", time.Now().Add(time.Hour))
				case "/models":
					_, _ = io.WriteString(w, testModels)
				case "/models/terms-model/policy":
					body, _ := io.ReadAll(r.Body)
					if r.Method != "POST" || !bytes.Equal(bytes.TrimSpace(body), []byte(`{"state":"enabled"}`)) {
						t.Errorf("policy request = %s %s", r.Method, body)
					}
					w.WriteHeader(policyStatus)
				case "/chat/completions":
					testJSON(w, map[string]string{"result": "served"})
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})})
			body := `{"model":"terms-model","messages":[{"role":"user","content":"hello"}]}`
			if w := testRequest(s, "POST", "/v1/chat/completions", body); w.Code != 200 {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			want := []string{"/copilot_internal/v2/token", "/models", "/models/terms-model/policy", "/chat/completions"}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events = %v, want %v", events, want)
			}
		})
	}
}

func TestManualModelSurvivesModelListFailureAndDoesNotRetry(t *testing.T) {
	var requests atomic.Int32
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			w.WriteHeader(503)
		case "/chat/completions":
			requests.Add(1)
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"code":"model_not_supported"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(500)
		}
	})})
	w := testRequest(s, "POST", "/v1/chat/completions", `{"model":"manual","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != 400 || requests.Load() != 1 || w.Body.String() != `{"error":{"code":"model_not_supported"}}` {
		t.Fatalf("status=%d body=%s calls=%d", w.Code, w.Body.String(), requests.Load())
	}
}

func TestAutoRequiresModelListAfterInitialFetchFailure(t *testing.T) {
	for _, tc := range []struct {
		name            string
		refreshedStatus int
		refreshedBody   string
		wantStatus      int
	}{
		{
			name:            "reject model that does not support path",
			refreshedStatus: http.StatusOK,
			refreshedBody:   `{"data":[{"id":"gpt-4.1","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"],"policy":{"state":"enabled"}}]}`,
			wantStatus:      http.StatusBadRequest,
		},
		{
			name:            "reject when model list refresh fails",
			refreshedStatus: http.StatusServiceUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var modelCalls, autoCalls, generationCalls atomic.Int32
			s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/copilot_internal/v2/token":
					testSession(w, "", time.Now().Add(time.Hour))
				case "/models":
					n := modelCalls.Add(1)
					if n == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if n != 2 {
						t.Errorf("model list request count exceeded 2: %d", n)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(tc.refreshedStatus)
					if tc.refreshedBody != "" {
						_, _ = io.WriteString(w, tc.refreshedBody)
					}
				case "/auto":
					autoCalls.Add(1)
					testJSON(w, map[string]any{
						"session_token":  "auto-secret",
						"selected_model": "gpt-4.1",
						"expires_at":     time.Now().Add(time.Hour).Unix(),
					})
				case "/responses":
					generationCalls.Add(1)
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			})})

			w := testRequest(s, "POST", "/v1/responses", `{"model":"auto","input":"hello"}`)
			if tc.wantStatus != 0 && w.Code != tc.wantStatus {
				t.Errorf("status=%d body=%s, want %d", w.Code, w.Body.String(), tc.wantStatus)
			} else if tc.wantStatus == 0 && w.Code < http.StatusBadRequest {
				t.Errorf("status=%d body=%s, want Auto request rejected before generation", w.Code, w.Body.String())
			}
			if got := modelCalls.Load(); got != 2 {
				t.Errorf("model list calls=%d, want 2", got)
			}
			if got := autoCalls.Load(); got != 1 {
				t.Errorf("Auto calls=%d, want 1", got)
			}
			if got := generationCalls.Load(); got != 0 {
				t.Errorf("generation calls=%d, want 0", got)
			}
		})
	}
}

func TestGenerationRejectsModelsUnavailableForPath(t *testing.T) {
	for _, tc := range []struct {
		name          string
		path          string
		body          string
		autoModel     string
		wantAutoCalls int32
	}{
		{
			name: "manual model does not support path",
			path: "/v1/responses",
			body: `{"model":"terms-model","input":"hello"}`,
		},
		{
			name: "manual model is not in visible list",
			path: "/v1/chat/completions",
			body: `{"model":"hidden","messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "manual unknown model",
			path: "/v1/chat/completions",
			body: `{"model":"unknown-model","messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name:          "auto selects model that does not support path",
			path:          "/v1/responses",
			body:          `{"model":"auto","input":"hello"}`,
			autoModel:     "terms-model",
			wantAutoCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var modelCalls, autoCalls, generationCalls atomic.Int32
			s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/copilot_internal/v2/token":
					testSession(w, "", time.Now().Add(time.Hour))
				case "/models":
					modelCalls.Add(1)
					_, _ = io.WriteString(w, testModels)
				case "/models/terms-model/policy":
					w.WriteHeader(http.StatusOK)
				case "/auto":
					autoCalls.Add(1)
					testJSON(w, map[string]any{
						"session_token":  "auto-secret",
						"selected_model": tc.autoModel,
						"expires_at":     time.Now().Add(time.Hour).Unix(),
					})
				case "/chat/completions", "/responses", "/v1/messages":
					generationCalls.Add(1)
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			})})

			w := testRequest(s, "POST", tc.path, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status=%d body=%s, want 400", w.Code, w.Body.String())
			}
			if got := modelCalls.Load(); got != 1 {
				t.Errorf("model list calls=%d, want 1", got)
			}
			if got := autoCalls.Load(); got != tc.wantAutoCalls {
				t.Errorf("auto calls=%d, want %d", got, tc.wantAutoCalls)
			}
			if got := generationCalls.Load(); got != 0 {
				t.Errorf("generation calls=%d, want 0", got)
			}
		})
	}
}

func TestAutoSelectionForUnsupportedPathIsNotCached(t *testing.T) {
	var autoCalls, generationCalls atomic.Int32
	var forwardedModel atomic.Value
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/auto":
			n := autoCalls.Add(1)
			selected := "terms-model"
			if n == 2 {
				selected = "gpt-4.1"
			}
			testJSON(w, map[string]any{
				"session_token":  fmt.Sprintf("auto-%d", n),
				"selected_model": selected,
				"expires_at":     time.Now().Add(time.Hour).Unix(),
			})
		case "/responses":
			generationCalls.Add(1)
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode generation request: %v", err)
			}
			forwardedModel.Store(body.Model)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})})
	const requestBody = `{"model":"auto","input":"hello"}`

	first := testRequest(s, "POST", "/v1/responses", requestBody)
	if first.Code != http.StatusBadRequest {
		t.Fatalf("first status=%d body=%s, want 400", first.Code, first.Body.String())
	}
	if got := autoCalls.Load(); got != 1 {
		t.Fatalf("Auto calls after first request=%d, want 1", got)
	}
	if got := generationCalls.Load(); got != 0 {
		t.Fatalf("generation calls after first request=%d, want 0", got)
	}

	second := testRequest(s, "POST", "/v1/responses", requestBody)
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s, want 200", second.Code, second.Body.String())
	}
	if got := autoCalls.Load(); got != 2 {
		t.Errorf("total Auto calls=%d, want 2", got)
	}
	if got := generationCalls.Load(); got != 1 {
		t.Errorf("total generation calls=%d, want 1", got)
	}
	if got, _ := forwardedModel.Load().(string); got != "gpt-4.1" {
		t.Errorf("forwarded model=%q, want gpt-4.1", got)
	}
}

func TestSessionReuseRefreshAndEnterpriseHost(t *testing.T) {
	for _, ttl := range []time.Duration{time.Hour, time.Minute} {
		t.Run(ttl.String(), func(t *testing.T) {
			var exchanges atomic.Int32
			s := testService(t, Config{Token: "github-secret", Host: "https://acme.ghe.com/", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/copilot_internal/v2/token":
					exchanges.Add(1)
					if r.Header.Get("Test-Original-Host") != "api.acme.ghe.com" {
						t.Errorf("enterprise token host = %q", r.Header.Get("Test-Original-Host"))
					}
					testSession(w, "https://copilot-api.acme.ghe.com", time.Now().Add(ttl))
				case "/models":
					_, _ = io.WriteString(w, testModels)
				case "/chat/completions":
					if r.Header.Get("Test-Original-Host") != "copilot-api.acme.ghe.com" {
						t.Errorf("enterprise generation host = %q", r.Header.Get("Test-Original-Host"))
					}
					_, _ = io.WriteString(w, `{}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			})})
			for range 2 {
				if w := testRequest(s, "POST", "/v1/chat/completions", `{"model":"gpt-4.1","messages":[]}`); w.Code != 200 {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
			}
			if ttl == time.Hour && exchanges.Load() != 1 {
				t.Fatalf("fresh session exchanges = %d", exchanges.Load())
			}
			if ttl == time.Minute && exchanges.Load() < 2 {
				t.Fatalf("expiring session not refreshed, exchanges=%d", exchanges.Load())
			}
		})
	}
}

func TestSessionCachePersistsAcrossServiceInstances(t *testing.T) {
	var exchanges atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/v2/token" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		exchanges.Add(1)
		testSession(w, "https://api.githubcopilot.com", time.Now().Add(time.Hour))
	})
	config := Config{Token: "github-secret", CacheDir: t.TempDir(), Client: client}

	firstService := testService(t, config)
	first, err := firstService.getSession(context.Background())
	if err != nil {
		t.Fatalf("first getSession: %v", err)
	}
	if remaining := time.Until(time.Unix(first.ExpiresAt, 0)); remaining <= 2*time.Minute {
		t.Fatalf("test session remaining lifetime = %s, want more than the 2-minute refresh window", remaining)
	}

	cachePath := firstService.cacheFile("session")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Errorf("read persisted session cache %q: %v", cachePath, err)
	} else {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Errorf("decode persisted session cache: %v", err)
		} else {
			for _, field := range []string{"token", "expires_at", "api"} {
				if _, ok := fields[field]; !ok {
					t.Errorf("persisted session cache is missing JSON field %q: %s", field, data)
				}
			}
			var persisted persistedSessionForTest
			if err := json.Unmarshal(data, &persisted); err != nil {
				t.Errorf("decode persisted session fields: %v", err)
			} else if persisted.Token != first.Token || persisted.ExpiresAt != first.ExpiresAt || persisted.API != first.API {
				t.Errorf("persisted session = %+v, want token/expires_at/api matching %+v", persisted, first)
			}
		}
	}

	secondService := testService(t, config)
	second, err := secondService.getSession(context.Background())
	if err != nil {
		t.Fatalf("second getSession: %v", err)
	}
	if second.Token != first.Token || second.ExpiresAt != first.ExpiresAt || second.API != first.API {
		t.Errorf("second service session = %+v, want persisted session %+v", second, first)
	}
	if got := exchanges.Load(); got != 1 {
		t.Errorf("OAuth exchanges across Service instances = %d, want 1", got)
	}
}

func TestSessionCacheRefreshesWhenExpiresAtIsPast(t *testing.T) {
	var exchanges atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/v2/token" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		exchanges.Add(1)
		testSession(w, "https://api.githubcopilot.com", time.Now().Add(time.Hour))
	})
	config := Config{Token: "github-secret", CacheDir: t.TempDir(), Client: client}

	firstService := testService(t, config)
	first, err := firstService.getSession(context.Background())
	if err != nil {
		t.Fatalf("initial getSession: %v", err)
	}
	cachePath := firstService.cacheFile("session")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read persisted session cache %q: %v", cachePath, err)
	}
	var cached persistedSessionForTest
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatalf("decode persisted session cache: %v", err)
	}
	if cached.Token != first.Token || cached.API != first.API {
		t.Fatalf("persisted session = %+v, want initial session %+v", cached, first)
	}
	cached.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	data, err = json.Marshal(cached)
	if err != nil {
		t.Fatalf("encode expired session cache: %v", err)
	}
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatalf("expire session cache: %v", err)
	}

	newService := testService(t, config)
	got, err := newService.getSession(context.Background())
	if err != nil {
		t.Fatalf("getSession with expired disk cache: %v", err)
	}
	if got.Token != "session-secret" {
		t.Errorf("session after expired cache = %+v, want a newly fetched session", got)
	}
	if exchanges.Load() != 2 {
		t.Errorf("OAuth exchanges after expiring persisted session = %d, want 2", exchanges.Load())
	}
}

func TestRejectUnsafeSessionEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"http://api.githubcopilot.com", "https://evil.example", "https://evil.githubcopilot.com",
		"https://api.githubcopilot.com.evil.example", "https://user" + "@" + "api.githubcopilot.com",
		"https://api.githubcopilot.com:443", "https://api.githubcopilot.com/path",
		"https://api.githubcopilot.com?key=secret", "https://api.githubcopilot.com#fragment",
		"https://copilot-api.other.ghe.com",
	} {
		t.Run(endpoint, func(t *testing.T) {
			var downstream atomic.Int32
			s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/copilot_internal/v2/token" {
					testSession(w, endpoint, time.Now().Add(time.Hour))
					return
				}
				downstream.Add(1)
				_, _ = io.WriteString(w, testModels)
			})})
			w := testRequest(s, "GET", "/v1/models", "")
			if w.Code < 400 || downstream.Load() != 0 {
				t.Fatalf("unsafe endpoint accepted: status=%d downstream=%d", w.Code, downstream.Load())
			}
		})
	}
}

func TestRedirectNeverForwardsSecrets(t *testing.T) {
	var redirected atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Test-Original-Host") != "api.github.com" {
			redirected.Add(1)
			w.WriteHeader(200)
			return
		}
		http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
	})
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
	s := testService(t, Config{Token: "github-secret", Client: client})
	if w := testRequest(s, "GET", "/v1/models", ""); w.Code < 400 {
		t.Fatalf("redirect status = %d", w.Code)
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect followed %d times", redirected.Load())
	}
}

func TestNetworkFailureReturnsBadGateway(t *testing.T) {
	s := testService(t, Config{Token: "github-secret", Client: &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("test network failure")
	})}})
	if w := testRequest(s, "GET", "/v1/models", ""); w.Code != 502 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestGenerationNetworkFailureRedactsSensitiveQueryFromLogs(t *testing.T) {
	logs := captureSlog(t, slog.LevelDebug)
	const querySecret = "client-query-token-secret"
	baseClient := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	})
	baseTransport := baseClient.Transport
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/chat/completions" {
			if got := r.URL.Query().Get("access_token"); got != querySecret {
				t.Errorf("upstream access_token = %q, want query value to be forwarded", got)
			}
			return nil, errors.New("dial tcp: network is unreachable")
		}
		return baseTransport.RoundTrip(r)
	})}
	s := testService(t, Config{Token: "github-secret", Client: client})

	w := testRequest(s, http.MethodPost,
		"/v1/chat/completions?access_token="+querySecret,
		`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s, want %d", w.Code, w.Body.String(), http.StatusBadGateway)
	}

	logOutput := logs.String()
	if strings.Contains(logOutput, querySecret) {
		t.Errorf("sensitive query value leaked into logs")
	}
	for _, stage := range []string{"上游 API 请求失败", "请求处理失败"} {
		if !strings.Contains(logOutput, stage) {
			t.Errorf("failure stage %q missing from logs: %s", stage, logOutput)
		}
	}
}

func TestConnectionNominatedProtocolHeaderIsRemoved(t *testing.T) {
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/v1/messages":
			if got := r.Header.Get("Anthropic-Version"); got != "" {
				t.Errorf("Connection-nominated protocol header forwarded: %q", got)
			}
			_, _ = io.WriteString(w, `{}`)
		}
	})})
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"gpt-4.1","messages":[]}`))
	r.Header.Set("Connection", "Anthropic-Version")
	r.Header.Set("Anthropic-Version", "2023-06-01")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestConcurrentGenerationSharesSessionAndModelCache(t *testing.T) {
	var exchanges, models, generations atomic.Int32
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			exchanges.Add(1)
			time.Sleep(10 * time.Millisecond)
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			models.Add(1)
			time.Sleep(10 * time.Millisecond)
			_, _ = io.WriteString(w, testModels)
		case "/chat/completions":
			generations.Add(1)
			_, _ = io.WriteString(w, `{}`)
		}
	})})
	var wait sync.WaitGroup
	for range 24 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			w := testRequest(s, "POST", "/v1/chat/completions", `{"model":"gpt-4.1","messages":[]}`)
			if w.Code != 200 {
				t.Errorf("concurrent request=%d %s", w.Code, w.Body.String())
			}
		}()
	}
	wait.Wait()
	if exchanges.Load() != 1 || models.Load() != 1 || generations.Load() != 24 {
		t.Fatalf("concurrent counts token=%d models=%d generations=%d", exchanges.Load(), models.Load(), generations.Load())
	}
}

func TestCancelledRequestDoesNotReachUpstream(t *testing.T) {
	var calls atomic.Int32
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Error("cancelled request reached upstream")
		w.WriteHeader(500)
	})})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4.1","messages":[]}`))
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code < 400 || calls.Load() != 0 {
		t.Fatalf("cancelled request=%d, upstream calls=%d", w.Code, calls.Load())
	}
}

func TestOversizedGenerationBodyDoesNotReachUpstream(t *testing.T) {
	var calls atomic.Int32
	s := testService(t, Config{Token: "github-secret", Client: &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("oversized body must not reach upstream")
	})}})
	body := `{"model":"gpt-4.1","input":"` + strings.Repeat("a", 16<<20) + `"}`
	w := testRequest(s, "POST", "/v1/responses", body)
	if w.Code != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
		t.Fatalf("oversized request = %d, upstream calls=%d", w.Code, calls.Load())
	}
}

func TestExpiringAutoSessionIsRefreshed(t *testing.T) {
	var picks atomic.Int32
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/auto":
			n := picks.Add(1)
			testJSON(w, map[string]any{"session_token": fmt.Sprint("auto-", n), "selected_model": "gpt-4.1", "expires_at": time.Now().Add(time.Minute).Unix()})
		case "/chat/completions":
			_, _ = io.WriteString(w, `{}`)
		}
	})})
	for range 2 {
		if w := testRequest(s, "POST", "/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`); w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if picks.Load() != 2 {
		t.Fatalf("expiring auto session reused, picks=%d", picks.Load())
	}
}

func TestAutoSelectionPreservesProtocolAndCachesSession(t *testing.T) {
	for _, selected := range []string{`"gpt-4.1"`, `{"id":"gpt-4.1","supported_endpoints":["/responses"]}`} {
		for _, tc := range []struct{ path, upstream, body string }{
			{"/v1/chat/completions", "/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"pick me"}],"temperature":0.7,"stream":false,"custom":{"keep":"yes"}}`},
			{"/v1/responses", "/responses", `{"model":"auto","input":"pick me","reasoning":{"effort":"high"},"store":false}`},
			{"/v1/messages", "/v1/messages", `{"model":"auto","max_tokens":42,"messages":[{"role":"user","content":[{"type":"text","text":"pick me"}]}],"system":"keep me"}`},
		} {
			t.Run(tc.path+selected, func(t *testing.T) {
				var autoCalls, generationCalls atomic.Int32
				s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/copilot_internal/v2/token":
						testSession(w, "", time.Now().Add(time.Hour))
					case "/models":
						_, _ = io.WriteString(w, testModels)
					case "/auto":
						autoCalls.Add(1)
						var got map[string]any
						_ = json.NewDecoder(r.Body).Decode(&got)
						if got["prompt"] != "pick me" || got["tier"] != "balance" || r.Header.Get("X-GitHub-Api-Version") != "2026-08-01" {
							t.Errorf("auto request body/headers = %v %v", got, r.Header)
						}
						_, _ = fmt.Fprintf(w, `{"session_token":"auto-secret","selected_model":%s,"expires_at":%d}`, selected, time.Now().Add(24*time.Hour).Unix())
					case tc.upstream:
						generationCalls.Add(1)
						if r.Header.Get("Copilot-Session-Token") != "auto-secret" {
							t.Errorf("auto session header = %q", r.Header.Get("Copilot-Session-Token"))
						}
						var before, after map[string]any
						_ = json.Unmarshal([]byte(tc.body), &before)
						_ = json.NewDecoder(r.Body).Decode(&after)
						before["model"] = "gpt-4.1"
						if !reflect.DeepEqual(before, after) {
							t.Errorf("body fields lost: got=%v want=%v", after, before)
						}
						_, _ = io.WriteString(w, `{}`)
					default:
						t.Errorf("unexpected endpoint %s", r.URL.Path)
						w.WriteHeader(500)
					}
				})})
				for range 2 {
					if w := testRequest(s, "POST", tc.path, tc.body); w.Code != 200 {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
				}
				if autoCalls.Load() != 1 || generationCalls.Load() != 2 {
					t.Fatalf("auto=%d generation=%d", autoCalls.Load(), generationCalls.Load())
				}
			})
		}
	}
}

func TestAutoLegacyAndManualFallback(t *testing.T) {
	for _, legacyAvailable := range []bool{true, false} {
		t.Run(fmt.Sprint(legacyAvailable), func(t *testing.T) {
			var legacyCalls atomic.Int32
			s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/copilot_internal/v2/token":
					testSession(w, "", time.Now().Add(time.Hour))
				case "/models":
					_, _ = io.WriteString(w, testModels)
				case "/auto":
					w.WriteHeader(404)
				case "/models/session":
					legacyCalls.Add(1)
					if r.Header.Get("X-GitHub-Api-Version") != "2025-10-01" {
						t.Errorf("legacy API version = %q", r.Header.Get("X-GitHub-Api-Version"))
					}
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if !reflect.DeepEqual(body, map[string]any{"auto_mode": map[string]any{"model_hints": []any{"auto"}}}) {
						t.Errorf("legacy body = %v", body)
					}
					if legacyAvailable {
						testJSON(w, map[string]any{"session_token": "legacy-secret", "available_models": []string{"unknown", "gpt-4.1"}, "expires_at": time.Now().Add(time.Hour).Unix()})
					} else {
						w.WriteHeader(404)
					}
				case "/chat/completions":
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["model"] != "gpt-4.1" {
						t.Errorf("fallback model = %v", body["model"])
					}
					wantToken := ""
					if legacyAvailable {
						wantToken = "legacy-secret"
					}
					if r.Header.Get("Copilot-Session-Token") != wantToken {
						t.Errorf("fallback session header = %q", r.Header.Get("Copilot-Session-Token"))
					}
					_, _ = io.WriteString(w, `{}`)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})})
			if w := testRequest(s, "POST", "/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`); w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if legacyCalls.Load() != 1 {
				t.Fatalf("legacy calls = %d", legacyCalls.Load())
			}
		})
	}
}

func TestAutoRetryIsBounded(t *testing.T) {
	retryModels := strings.TrimSuffix(testModels, "]}") + `,
		{"id":"picked-1","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"]},
		{"id":"picked-2","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"]},
		{"id":"picked-3","model_picker_enabled":true,"capabilities":{"type":"chat"},"supported_endpoints":["/chat/completions"]}
	]}`
	for _, rejection := range []string{`{"error":{"code":"model_not_supported"}}`, `{"error":{"message":"not available for session"}}`} {
		for _, succeeds := range []bool{true, false} {
			t.Run(rejection+fmt.Sprint(succeeds), func(t *testing.T) {
				var generationCalls, autoCalls atomic.Int32
				s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/copilot_internal/v2/token":
						testSession(w, "", time.Now().Add(time.Hour))
					case "/models":
						_, _ = io.WriteString(w, retryModels)
					case "/auto":
						n := autoCalls.Add(1)
						testJSON(w, map[string]any{"session_token": fmt.Sprint("auto-", n), "selected_model": fmt.Sprint("picked-", n), "expires_at": time.Now().Add(time.Hour).Unix()})
					case "/chat/completions":
						n := generationCalls.Add(1)
						if succeeds && n == 2 {
							_, _ = io.WriteString(w, `{"ok":true}`)
						} else {
							w.WriteHeader(400)
							_, _ = io.WriteString(w, rejection)
						}
					default:
						t.Errorf("unexpected endpoint %s", r.URL.Path)
						w.WriteHeader(500)
					}
				})})
				w := testRequest(s, "POST", "/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)
				if succeeds && (w.Code != 200 || generationCalls.Load() != 2) {
					t.Fatalf("retry status=%d calls=%d body=%s", w.Code, generationCalls.Load(), w.Body.String())
				}
				if !succeeds && (w.Code != 400 || generationCalls.Load() < 2 || generationCalls.Load() > 3) {
					t.Fatalf("bounded retry status=%d calls=%d", w.Code, generationCalls.Load())
				}
				if autoCalls.Load() != generationCalls.Load() {
					t.Fatalf("auto selections=%d generation attempts=%d", autoCalls.Load(), generationCalls.Load())
				}
			})
		}
	}
}

func TestSSEIsFlushedBeforeUpstreamFinishes(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	s := testService(t, Config{Token: "github-secret", Client: testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			<-release
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
	})})
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	defer finish()
	result := make(chan error, 1)
	go func() {
		res, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-4.1","messages":[],"stream":true}`))
		if err != nil {
			result <- err
			return
		}
		defer res.Body.Close()
		line, err := bufio.NewReader(res.Body).ReadString('\n')
		if err != nil {
			result <- err
		} else if line != "data: first\n" {
			result <- fmt.Errorf("first SSE line = %q", line)
		} else {
			result <- nil
		}
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		finish()
		t.Fatal("SSE buffered until upstream completed")
	}
	finish()
}
