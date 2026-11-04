package copilot

// 本文件覆盖配置管理接口（GET/POST validate/PUT /v0/management/config）的路由行为：
// 以假的 ConfigStore 验证 JSON 形状、状态码映射、鉴权、成功后 ApplySettings 生效、日志不含配置值，
// 以及并发保存按顺序串行执行。配置文件本身的读写由 cmd 层测试覆盖。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// 配置接口测试使用的新服务密钥，库层不校验长度。
	cfgNewAPIKey = "config-test-new-api-key"
	cfgOldRev    = "rev-old"
	cfgNewRev    = "rev-new"
)

// fakeConfigStore 是可编排的 ConfigStore，并记录收到的调用。
type fakeConfigStore struct {
	mu            sync.Mutex
	snapshot      ConfigSnapshot
	loadErr       error
	validation    ConfigValidation
	saveResult    ConfigSaveResult
	saveErr       error
	saveDelay     time.Duration
	validateCalls []string
	saveContents  []string
	saveRevisions []string

	saveInFlight    atomic.Int32
	saveMaxInFlight atomic.Int32
}

func (f *fakeConfigStore) Load() (ConfigSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot, f.loadErr
}

func (f *fakeConfigStore) Validate(content string) ConfigValidation {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validateCalls = append(f.validateCalls, content)
	return f.validation
}

func (f *fakeConfigStore) Save(content, revision string) (ConfigSaveResult, error) {
	n := f.saveInFlight.Add(1)
	defer f.saveInFlight.Add(-1)
	for {
		max := f.saveMaxInFlight.Load()
		if n <= max || f.saveMaxInFlight.CompareAndSwap(max, n) {
			break
		}
	}
	if f.saveDelay > 0 {
		time.Sleep(f.saveDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveContents = append(f.saveContents, content)
	f.saveRevisions = append(f.saveRevisions, revision)
	return f.saveResult, f.saveErr
}

// newConfigAPIPool 创建启用管理接口、没有账号的托管池；store 为 nil 表示不提供配置接口。
func newConfigAPIPool(t *testing.T, store ConfigStore, logger *slog.Logger) *Pool {
	t.Helper()
	config := PoolConfig{
		APIKey: testAPIKey, ManagementKey: mgmtKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(), Logger: logger,
	}
	if store != nil {
		config.ConfigStore = store
	}
	p, err := NewManagedPool(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)
	return p
}

// cfgJSON 把响应体解析为 map，便于同时断言字段值与字段是否存在。
func cfgJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v; body=%s", err, w.Body.String())
	}
	return body
}

// cfgAuthorized 报告 key 是否通过业务接口的 api_key 鉴权（/healthz 免鉴权，因此用 /v1/models：
// 鉴权失败为 401，通过后即使没有可用账号也不会是 401）。
func cfgAuthorized(p *Pool, key string) bool {
	code, _ := settingsCall(p.Handler(), http.MethodGet, "/v1/models", "", key)
	return code != http.StatusUnauthorized
}

// TestConfigAPIWithoutStoreReturns404 ConfigStore 为 nil 时三个接口都返回 404「未配置配置文件」（令牌有效也一样）。
func TestConfigAPIWithoutStoreReturns404(t *testing.T) {
	p := newConfigAPIPool(t, nil, nil)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v0/management/config", ""},
		{http.MethodPost, "/v0/management/config/validate", `{"content":"a: 1"}`},
		{http.MethodPut, "/v0/management/config", `{"content":"a: 1","revision":"r"}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := mgmtCall(p, tc.method, tc.path, tc.body, mgmtKey)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
			}
			if msg := mgmtErrorMessage(t, w); msg != "未配置配置文件" {
				t.Errorf("错误消息 = %q, want 未配置配置文件", msg)
			}
		})
	}
}

// TestConfigAPIRequiresConsoleToken 配置接口与其他管理接口一致：无令牌、管理密钥本身、伪造令牌都是 401，且不调用 ConfigStore。
func TestConfigAPIRequiresConsoleToken(t *testing.T) {
	store := &fakeConfigStore{}
	p := newConfigAPIPool(t, store, nil)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v0/management/config", ""},
		{http.MethodPost, "/v0/management/config/validate", `{"content":"a: 1"}`},
		{http.MethodPut, "/v0/management/config", `{"content":"a: 1","revision":"r"}`},
	} {
		for name, key := range map[string]string{"无令牌": "", "管理密钥": mgmtKey, "错误令牌": consoleTokenFor("other-key")} {
			t.Run(tc.method+" "+name, func(t *testing.T) {
				if w := mgmtCallRaw(p, tc.method, tc.path, tc.body, key); w.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
				}
			})
		}
	}
	if len(store.validateCalls)+len(store.saveContents) != 0 {
		t.Errorf("未鉴权的请求调用了 ConfigStore: validate=%d save=%d", len(store.validateCalls), len(store.saveContents))
	}
}

// TestConfigAPIGetShape GET 返回 ConfigSnapshot 的 JSON 形状（含空数组而非 null）；Load 失败返回 500。
func TestConfigAPIGetShape(t *testing.T) {
	store := &fakeConfigStore{snapshot: ConfigSnapshot{
		Path: "/etc/ghc2api/config.yaml", Content: "api_key: x\n", Revision: cfgOldRev, Writable: true, RestartRequired: []string{},
	}}
	p := newConfigAPIPool(t, store, nil)

	w := mgmtCall(p, http.MethodGet, "/v0/management/config", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	want := map[string]any{
		"path": "/etc/ghc2api/config.yaml", "content": "api_key: x\n", "revision": cfgOldRev,
		"writable": true, "restart_required": []any{},
	}
	if got := cfgJSON(t, w); !reflect.DeepEqual(got, want) {
		t.Errorf("响应 = %v, want %v", got, want)
	}

	store.mu.Lock()
	store.snapshot.Writable = false
	store.snapshot.RestartRequired = []string{"listen", "data_path"}
	store.mu.Unlock()
	got := cfgJSON(t, mgmtCall(p, http.MethodGet, "/v0/management/config", "", mgmtKey))
	if got["writable"] != false || !reflect.DeepEqual(got["restart_required"], []any{"listen", "data_path"}) {
		t.Errorf("writable/restart_required = %v/%v", got["writable"], got["restart_required"])
	}

	store.mu.Lock()
	store.loadErr = errors.New("读取失败")
	store.mu.Unlock()
	if w := mgmtCall(p, http.MethodGet, "/v0/management/config", "", mgmtKey); w.Code != http.StatusInternalServerError {
		t.Errorf("Load 失败时 status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestConfigAPIValidate 校验接口：有效与无效配置都返回 200；无效时带 error 与 line；请求体非法返回 400。
func TestConfigAPIValidate(t *testing.T) {
	store := &fakeConfigStore{validation: ConfigValidation{Valid: true, RestartRequired: []string{"listen"}}}
	p := newConfigAPIPool(t, store, nil)

	w := mgmtCall(p, http.MethodPost, "/v0/management/config/validate", `{"content":"listen: x"}`, mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("有效配置 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got, want := cfgJSON(t, w), map[string]any{"valid": true, "restart_required": []any{"listen"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("有效配置响应 = %v, want %v（error 与 line 应省略）", got, want)
	}
	if !reflect.DeepEqual(store.validateCalls, []string{"listen: x"}) {
		t.Errorf("Validate 收到 %q", store.validateCalls)
	}

	store.mu.Lock()
	store.validation = ConfigValidation{Valid: false, Error: "yaml: line 3: bad", Line: 3, RestartRequired: []string{}}
	store.mu.Unlock()
	w = mgmtCall(p, http.MethodPost, "/v0/management/config/validate", `{"content":"bad"}`, mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("无效配置 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	want := map[string]any{"valid": false, "error": "yaml: line 3: bad", "line": float64(3), "restart_required": []any{}}
	if got := cfgJSON(t, w); !reflect.DeepEqual(got, want) {
		t.Errorf("无效配置响应 = %v, want %v", got, want)
	}

	if w := mgmtCall(p, http.MethodPost, "/v0/management/config/validate", `{not json`, mgmtKey); w.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON status = %d, want 400", w.Code)
	}
}

// TestConfigAPIPutErrors PUT 的错误映射：非法 JSON 与 revision 为空 400（不调用 Save）；
// ErrConfigConflict 409；ErrConfigInvalid 400（消息为 err.Error()）；其他错误 500。失败时不应用 Settings。
func TestConfigAPIPutErrors(t *testing.T) {
	invalidErr := fmt.Errorf("%w：api_key 过短", ErrConfigInvalid)
	for _, tc := range []struct {
		name      string
		body      string
		saveErr   error
		wantCode  int
		wantMsg   string // 非空时断言错误消息
		wantSaves int
	}{
		{name: "非法 JSON", body: `{oops`, wantCode: 400},
		{name: "revision 缺失", body: `{"content":"a: 1"}`, wantCode: 400},
		{name: "revision 为空串", body: `{"content":"a: 1","revision":""}`, wantCode: 400},
		{name: "冲突", body: `{"content":"a: 1","revision":"r"}`, saveErr: fmt.Errorf("包裹: %w", ErrConfigConflict),
			wantCode: 409, wantSaves: 1},
		{name: "无效配置", body: `{"content":"a: 1","revision":"r"}`, saveErr: invalidErr,
			wantCode: 400, wantMsg: invalidErr.Error(), wantSaves: 1},
		{name: "其他错误", body: `{"content":"a: 1","revision":"r"}`, saveErr: errors.New("写入配置文件失败"),
			wantCode: 500, wantSaves: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeConfigStore{
				saveErr:    tc.saveErr,
				saveResult: ConfigSaveResult{Settings: RuntimeSettings{APIKey: cfgNewAPIKey}},
			}
			p := newConfigAPIPool(t, store, nil)
			w := mgmtCall(p, http.MethodPut, "/v0/management/config", tc.body, mgmtKey)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantMsg != "" {
				if msg := mgmtErrorMessage(t, w); msg != tc.wantMsg {
					t.Errorf("错误消息 = %q, want %q", msg, tc.wantMsg)
				}
			}
			if len(store.saveContents) != tc.wantSaves {
				t.Errorf("Save 调用次数 = %d, want %d", len(store.saveContents), tc.wantSaves)
			}
			if cfgAuthorized(p, cfgNewAPIKey) {
				t.Error("保存失败后新 api_key 不应生效")
			}
			if !cfgAuthorized(p, testAPIKey) {
				t.Error("保存失败后原 api_key 应仍有效")
			}
		})
	}
}

// TestConfigAPIPutSuccessAppliesSettings PUT 成功：响应只含 revision/changed/restart_required；
// Save 收到原样的 content 与 revision；返回的 Settings 立即生效（旧 api_key 401、新 api_key 通过）；
// 日志记录「配置已更新」及 changed/restart_required，但不含任何配置值。
func TestConfigAPIPutSuccessAppliesSettings(t *testing.T) {
	store := &fakeConfigStore{saveResult: ConfigSaveResult{
		Revision: cfgNewRev, Changed: []string{"api_key"}, RestartRequired: []string{"listen"},
		Settings: RuntimeSettings{APIKey: cfgNewAPIKey},
	}}
	var logs bytes.Buffer
	p := newConfigAPIPool(t, store, slog.New(slog.NewJSONHandler(&logs, nil)))
	if cfgAuthorized(p, cfgNewAPIKey) {
		t.Fatal("保存前新 api_key 不应有效")
	}

	body, _ := json.Marshal(map[string]string{"content": "# 注释\napi_key: secret-content\n", "revision": cfgOldRev})
	w := mgmtCall(p, http.MethodPut, "/v0/management/config", string(body), mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	want := map[string]any{"revision": cfgNewRev, "changed": []any{"api_key"}, "restart_required": []any{"listen"}}
	if got := cfgJSON(t, w); !reflect.DeepEqual(got, want) {
		t.Errorf("响应 = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(store.saveContents, []string{"# 注释\napi_key: secret-content\n"}) ||
		!reflect.DeepEqual(store.saveRevisions, []string{cfgOldRev}) {
		t.Errorf("Save 收到 content=%q revision=%q", store.saveContents, store.saveRevisions)
	}
	if cfgAuthorized(p, testAPIKey) {
		t.Error("保存后旧 api_key 应返回 401")
	}
	if !cfgAuthorized(p, cfgNewAPIKey) {
		t.Error("保存后新 api_key 应通过鉴权")
	}

	var entry map[string]any
	for _, e := range jsonLogEntries(t, logs.Bytes()) {
		if e["msg"] == "配置已更新" {
			entry = e
		}
	}
	if entry == nil {
		t.Fatalf("缺少「配置已更新」日志: %s", logs.String())
	}
	if entry["level"] != "INFO" || !reflect.DeepEqual(entry["changed"], []any{"api_key"}) ||
		!reflect.DeepEqual(entry["restart_required"], []any{"listen"}) {
		t.Errorf("日志字段 = %v", entry)
	}
	if strings.Contains(logs.String(), cfgNewAPIKey) || strings.Contains(logs.String(), "secret-content") {
		t.Errorf("日志不应包含配置值: %s", logs.String())
	}
}

// TestConfigAPIConcurrentPutsAreSerialized 并发 PUT 时 Save（及其后的 ApplySettings）在同一把锁内顺序执行。
func TestConfigAPIConcurrentPutsAreSerialized(t *testing.T) {
	store := &fakeConfigStore{saveDelay: 10 * time.Millisecond, saveResult: ConfigSaveResult{Revision: cfgNewRev}}
	p := newConfigAPIPool(t, store, nil)
	token := consoleTokenFor(mgmtKey)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := mgmtCallRaw(p, http.MethodPut, "/v0/management/config", `{"content":"a: 1","revision":"r"}`, token)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	if got := store.saveMaxInFlight.Load(); got != 1 {
		t.Errorf("Save 的最大并发数 = %d, want 1（应串行）", got)
	}
	if len(store.saveContents) != 8 {
		t.Errorf("Save 调用次数 = %d, want 8", len(store.saveContents))
	}
}

// TestConfigAPIGetDisablesCaching 配置内容含明文密钥，GET 响应必须带 Cache-Control: no-store。
func TestConfigAPIGetDisablesCaching(t *testing.T) {
	store := &fakeConfigStore{snapshot: ConfigSnapshot{Content: "api_key: x\n", Revision: cfgOldRev, RestartRequired: []string{}}}
	p := newConfigAPIPool(t, store, nil)
	w := mgmtCall(p, http.MethodGet, "/v0/management/config", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
