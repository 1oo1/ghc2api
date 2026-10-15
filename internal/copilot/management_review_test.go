package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNewManagedPoolRejectsManagementKeyEqualToAPIKey 管理密钥（去除首尾空白后）与服务访问密钥相同时拒绝创建池。
func TestNewManagedPoolRejectsManagementKeyEqualToAPIKey(t *testing.T) {
	for _, tc := range []struct {
		name          string
		managementKey string
		apiKey        string
		wantErr       bool
	}{
		{name: "same", managementKey: "shared-key", apiKey: "shared-key", wantErr: true},
		{name: "same-after-trimming-management-key", managementKey: " shared-key ", apiKey: "shared-key", wantErr: true},
		{name: "same-after-trimming-api-key", managementKey: "shared-key", apiKey: " shared-key ", wantErr: true},
		{name: "different", managementKey: mgmtKey, apiKey: mgmtAPIKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewManagedPool(PoolConfig{
				ManagementKey: tc.managementKey, APIKey: tc.apiKey,
				CredentialsDir: t.TempDir(), CacheDir: t.TempDir(),
			}, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NewManagedPool() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestManagementRejectsInvalidGitHubLogin GitHub 返回的登录名不能作为凭证文件名（含空格、"."、连字符位置错误等）时，
// 导入与设备登录都返回失败、不写入文件，也不加入池。
func TestManagementRejectsInvalidGitHubLogin(t *testing.T) {
	for _, login := range []string{"bad name", "bad.name", "-leading", "trailing-", "double--dash", "a/b"} {
		t.Run("import/"+login, func(t *testing.T) {
			up := newMgmtUpstream(t)
			up.setLogin("oauth-odd", login)
			p, dir := mgmtNewPool(t, up, 0, "alice")
			w := mgmtCall(p, "POST", "/v0/management/accounts", `{"credential":{"oauth_token":"oauth-odd"}}`, mgmtKey)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("导入非法登录名 status=%d, want 400; body=%s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "oauth-odd") {
				t.Fatalf("响应包含 OAuth token: %s", w.Body.String())
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
				t.Fatalf("凭证目录条目 = %d, %v; want 只有 alice.json", len(entries), err)
			}
			if got := len(mgmtAccounts(t, p)); got != 1 {
				t.Fatalf("导入失败后账号数 = %d, want 1", got)
			}
		})
	}

	t.Run("device-login", func(t *testing.T) {
		up := newMgmtUpstream(t)
		up.setDeviceLogin("oauth-odd", "bad.name")
		p, dir := mgmtNewPool(t, up, 0)
		w := mgmtCall(p, "POST", "/v0/management/login", "", mgmtKey)
		var start mgmtLoginStart
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &start) != nil {
			t.Fatalf("发起登录 status=%d body=%s", w.Code, w.Body.String())
		}
		if result := mgmtPollLogin(t, p, start.State); result.Status != "error" {
			t.Fatalf("非法登录名的设备登录结果 = %+v, want status=error", result)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("非法登录名写入了凭证目录: %d 个条目", len(entries))
		}
		if got := len(mgmtAccounts(t, p)); got != 0 {
			t.Fatalf("非法登录名加入了 %d 个账号", got)
		}
	})
}

// TestManagementQuotaRefreshTimeoutKeepsOldDataWithQuotaError 强制刷新超过 quotaRefreshTimeout 时，
// 保留旧额度，返回 200 并带非空 quota_error。超时同样计为一次尝试，60 秒内的列表不再请求上游。
func TestManagementQuotaRefreshTimeoutKeepsOldDataWithQuotaError(t *testing.T) {
	previous := quotaRefreshTimeout
	quotaRefreshTimeout = 300 * time.Millisecond
	t.Cleanup(func() { quotaRefreshTimeout = previous })

	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice")
	mgmtAccounts(t, p) // 先成功获取一次，作为旧数据保留
	up.holdQuota("oauth-alice")

	w := mgmtCall(p, "POST", "/v0/management/quota/refresh", `{"id":"alice"}`, mgmtKey)
	var view AccountView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("刷新响应不是 JSON: %v; body=%s", err, w.Body.String())
	}
	if w.Code != http.StatusOK || view.Quota == nil || view.QuotaError != quotaTimeoutMessage {
		t.Fatalf("超时刷新 status=%d view=%+v, want 200、保留旧额度并带 quota_error=%q", w.Code, view, quotaTimeoutMessage)
	}
	before := up.quotaCount("oauth-alice")
	alice := mgmtAccountByID(t, mgmtAccounts(t, p), "alice")
	if got := up.quotaCount("oauth-alice"); got != before {
		t.Fatalf("超时后 60 秒内列表重复请求了上游: %d -> %d", before, got)
	}
	if alice.Quota == nil || alice.QuotaError != quotaTimeoutMessage {
		t.Fatalf("超时后列表视图 = %+v, want 保留旧额度且带 quota_error", alice)
	}
}

// TestManagementQuotaFailedAttemptIsThrottled 失败的额度请求同样计为一次尝试：60 秒内列表不会重复请求上游，错误仍然展示。
func TestManagementQuotaFailedAttemptIsThrottled(t *testing.T) {
	up := newMgmtUpstream(t)
	up.failQuota("oauth-alice", http.StatusInternalServerError, `{"error":"down"}`)
	p, _ := mgmtNewPool(t, up, 0, "alice")
	first := mgmtAccountByID(t, mgmtAccounts(t, p), "alice")
	second := mgmtAccountByID(t, mgmtAccounts(t, p), "alice")
	if got := up.quotaCount("oauth-alice"); got != 1 {
		t.Fatalf("失败后 60 秒内额度请求 %d 次, want 1", got)
	}
	if first.QuotaError == "" || second.QuotaError == "" {
		t.Fatalf("额度失败未展示 quota_error: first=%+v second=%+v", first, second)
	}
}

// TestManagementDeleteKeepsCacheDirRemovedWhenSessionCompletesLate 删除账号后，进行中的请求即使之后才完成 session 获取，
// 也不会重新创建该账号的缓存目录。
func TestManagementDeleteKeepsCacheDirRemovedWhenSessionCompletesLate(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice", "bob")
	account := p.findAccount("alice")
	if account == nil || account.service.cacheDir == "" {
		t.Fatal("测试前提不成立：alice 应有非空的缓存目录")
	}
	cacheDir := account.service.cacheDir

	hold := up.holdSessions("oauth-alice")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- poolRequest(p, "POST", "/v1/chat/completions", poolChatBody) }()
	select {
	case <-hold.started:
	case <-time.After(10 * time.Second):
		t.Fatal("请求未到达 alice 的 session 交换")
	}

	if w := mgmtCall(p, "DELETE", "/v0/management/accounts?id=alice", "", mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("删除 alice status=%d body=%s", w.Code, w.Body.String())
	}
	hold.free()
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("进行中的请求没有完成")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("进行中的请求 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if _, err := os.Stat(cacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("删除账号后缓存目录被重新创建（stat 错误 = %v）", err)
	}
}

// TestManagementSessionFailuresCountOnlyLastTriedAccount 所有账号的 session 都失败时，错误计入最后尝试的账号
// （requests 与 errors 各加 1），其他账号不计数。
func TestManagementSessionFailuresCountOnlyLastTriedAccount(t *testing.T) {
	up := newMgmtUpstream(t)
	for _, user := range []string{"alice", "bob", "carol"} {
		up.pool.failSession("oauth-"+user, poolFailure{http.StatusInternalServerError, `{"message":"down"}`})
	}
	p, _ := mgmtNewPool(t, up, 0, "alice", "bob", "carol")
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusInternalServerError {
		t.Fatalf("全部 session 失败 status=%d, want 500; body=%s", w.Code, w.Body.String())
	}
	views := mgmtAccounts(t, p)
	for _, user := range []string{"alice", "bob"} {
		if v := mgmtAccountByID(t, views, user); v.Stats.Requests != 0 || v.Stats.Errors != 0 {
			t.Fatalf("%s 统计 = %+v, want 0 次请求、0 次错误", user, v.Stats)
		}
	}
	if v := mgmtAccountByID(t, views, "carol"); v.Stats.Requests != 1 || v.Stats.Errors != 1 {
		t.Fatalf("carol 统计 = %+v, want 1 次请求、1 次错误", v.Stats)
	}
}

// TestManagementImportRefusesExistingCredentialFile 凭证目录中已有同名文件（忽略大小写，即使运行期间放入、未被加载）时，
// 导入与设备登录都返回 409 或失败，且原文件内容保持不变。
func TestManagementImportRefusesExistingCredentialFile(t *testing.T) {
	original := []byte(`{"oauth_token":"oauth-old-carol","user":"carol"}` + "\n")
	setup := func(t *testing.T) (*mgmtUpstream, *Pool, string) {
		t.Helper()
		up := newMgmtUpstream(t)
		up.setLogin("oauth-carol", "Carol")
		p, dir := mgmtNewPool(t, up, 0, "alice")
		if err := os.WriteFile(filepath.Join(dir, "carol.json"), original, 0o600); err != nil {
			t.Fatal(err)
		}
		return up, p, dir
	}
	assertUnchanged := func(t *testing.T, dir string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(dir, "carol.json"))
		if err != nil || !bytes.Equal(got, original) {
			t.Fatalf("已有凭证文件被改写: %q, %v", got, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 2 {
			t.Fatalf("凭证目录条目 = %d, want 2（alice.json 与原 carol.json）", len(entries))
		}
	}

	t.Run("import", func(t *testing.T) {
		_, p, dir := setup(t)
		w := mgmtCall(p, "POST", "/v0/management/accounts", `{"credential":{"oauth_token":"oauth-carol"}}`, mgmtKey)
		if w.Code != http.StatusConflict {
			t.Fatalf("导入同名文件 status=%d, want 409; body=%s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "oauth-carol") {
			t.Fatalf("响应包含 OAuth token: %s", w.Body.String())
		}
		if got := len(mgmtAccounts(t, p)); got != 1 {
			t.Fatalf("导入失败后账号数 = %d, want 1", got)
		}
		assertUnchanged(t, dir)
	})

	t.Run("device-login", func(t *testing.T) {
		up, p, dir := setup(t)
		up.setDeviceLogin("oauth-carol", "Carol")
		w := mgmtCall(p, "POST", "/v0/management/login", "", mgmtKey)
		var start mgmtLoginStart
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &start) != nil {
			t.Fatalf("发起登录 status=%d body=%s", w.Code, w.Body.String())
		}
		result := mgmtPollLogin(t, p, start.State)
		if result.Status != "error" || strings.Contains(result.Error, "oauth-carol") {
			t.Fatalf("设备登录结果 = %+v, want status=error 且不含 token", result)
		}
		assertUnchanged(t, dir)
	})
}

// TestManagementConsoleSetsFramingProtection 控制台页面禁止被其他站点嵌入（防点击劫持）。
func TestManagementConsoleSetsFramingProtection(t *testing.T) {
	p, err := NewManagedPool(PoolConfig{ManagementKey: mgmtKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := mgmtCall(p, "GET", "/management", "", "")
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q, want 包含 default-src 'self' 与 frame-ancestors 'none'", csp)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q, want DENY", got)
	}
}

// TestManagementConcurrentOperationsAreRaceFree 生成请求与禁用/启用、导入/删除、账号列表、额度刷新并发执行。
// 在 -race 下不应出现数据竞争或 panic；alice 始终启用且不会被删除，因此生成请求都应成功。
func TestManagementConcurrentOperationsAreRaceFree(t *testing.T) {
	up := newMgmtUpstream(t)
	up.setLogin("oauth-carol", "carol")
	p, _ := mgmtNewPool(t, up, DefaultSessionAffinityTTL, "alice", "bob")
	const rounds = 15

	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				body := fmt.Sprintf(`{"model":"gpt-4.1","messages":[{"role":"user","content":"worker %d round %d"}]}`, worker, i)
				if w := poolRequest(p, "POST", "/v1/chat/completions", body); w.Code != http.StatusOK {
					t.Errorf("并发生成请求 status=%d body=%s", w.Code, w.Body.String())
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range rounds {
			body := fmt.Sprintf(`{"id":"bob","disabled":%t}`, i%2 == 0)
			if w := mgmtCall(p, "PATCH", "/v0/management/accounts", body, mgmtKey); w.Code != http.StatusOK {
				t.Errorf("并发禁用/启用 status=%d body=%s", w.Code, w.Body.String())
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range rounds {
			if w := mgmtCall(p, "POST", "/v0/management/accounts", `{"credential":{"oauth_token":"oauth-carol"}}`, mgmtKey); w.Code != http.StatusOK {
				t.Errorf("并发导入 status=%d body=%s", w.Code, w.Body.String())
			}
			if w := mgmtCall(p, "DELETE", "/v0/management/accounts?id=carol", "", mgmtKey); w.Code != http.StatusOK {
				t.Errorf("并发删除 status=%d body=%s", w.Code, w.Body.String())
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range rounds {
			if w := mgmtCall(p, "GET", "/v0/management/accounts", "", mgmtKey); w.Code != http.StatusOK {
				t.Errorf("并发账号列表 status=%d body=%s", w.Code, w.Body.String())
			}
			if w := mgmtCall(p, "POST", "/v0/management/quota/refresh", `{"id":"alice"}`, mgmtKey); w.Code != http.StatusOK {
				t.Errorf("并发额度刷新 status=%d body=%s", w.Code, w.Body.String())
			}
		}
	}()
	wg.Wait()
}

// quotaTimeoutMessage 是额度查询超时时写入 quota_error 的中文说明（不含 URL 等上游细节）。
const quotaTimeoutMessage = "额度查询超时"

// reviewAwait 等待 ch 关闭或发送，超过 10 秒即终止测试，避免测试因实现错误而挂起。
func reviewAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s 超时", what)
	}
}

// TestManagementRefreshCancelledByClientKeepsCacheAndRetriesNextList 调用方 ctx 被取消（例如客户端断开）时，
// 额度刷新既不更新 checkedAt，也不写入 quota_error；下一次列表请求会重新请求上游。
func TestManagementRefreshCancelledByClientKeepsCacheAndRetriesNextList(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice")
	mgmtAccounts(t, p) // 先成功获取一次，作为旧数据保留
	hold := up.holdQuota("oauth-alice")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/v0/management/quota/refresh", strings.NewReader(`{"id":"alice"}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+mgmtKey)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Handler().ServeHTTP(w, r)
	}()
	reviewAwait(t, hold.started, "强制刷新未到达上游")
	cancel()
	reviewAwait(t, done, "调用方取消后的强制刷新")

	// 取消不代表上游状态：响应中的账号视图不得带 quota_error，账号上也不得记录错误文本。
	var view AccountView
	if json.Unmarshal(w.Body.Bytes(), &view) == nil && view.QuotaError != "" {
		t.Fatalf("调用方取消后响应带了 quota_error=%q", view.QuotaError)
	}
	if _, errText := p.findAccount("alice").quota.snapshot(); errText != "" {
		t.Fatalf("调用方取消后账号记录了错误文本 %q", errText)
	}

	before := up.quotaCount("oauth-alice")
	hold.free()
	mgmtAccounts(t, p)
	if got := up.quotaCount("oauth-alice"); got != before+1 {
		t.Fatalf("调用方取消后下一次列表请求的上游次数 = %d, want %d（应重新请求）", got, before+1)
	}
}

// TestManagementClientCancelDuringSessionDoesNotCountError 客户端在 session 获取期间取消：账号不冷却，
// 也不计入 errors。requests 可以计入（请求确实分发给了该账号），这里不强制要求。
func TestManagementClientCancelDuringSessionDoesNotCountError(t *testing.T) {
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice")
	hold := up.holdSessions("oauth-alice")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(poolChatBody)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Handler().ServeHTTP(w, r)
	}()
	reviewAwait(t, hold.started, "请求未到达 session 交换")
	cancel()
	reviewAwait(t, done, "客户端取消后的请求")

	view := mgmtAccountByID(t, mgmtAccounts(t, p), "alice")
	if view.Stats.Errors != 0 {
		t.Fatalf("客户端取消的请求计入了 errors = %d, want 0", view.Stats.Errors)
	}
	if view.Stats.Requests > 1 {
		t.Fatalf("一次请求 requests = %d, want 至多 1", view.Stats.Requests)
	}
	if view.CooldownUntil != nil {
		t.Fatalf("客户端取消导致账号冷却至 %s", view.CooldownUntil)
	}
}

// TestManagementQuotaRefreshTimeoutWithoutOldDataIsBadGateway 强制刷新超时且账号没有任何旧额度时返回 502，
// error.message 精确为“额度查询超时”，且不含 URL 等上游细节。
func TestManagementQuotaRefreshTimeoutWithoutOldDataIsBadGateway(t *testing.T) {
	previous := quotaRefreshTimeout
	quotaRefreshTimeout = 300 * time.Millisecond
	t.Cleanup(func() { quotaRefreshTimeout = previous })

	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, 0, "alice")
	up.holdQuota("oauth-alice") // 从未成功获取过额度，上游挂起直到超时

	w := mgmtCall(p, "POST", "/v0/management/quota/refresh", `{"id":"alice"}`, mgmtKey)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("无旧数据的超时刷新 status=%d, want 502; body=%s", w.Code, w.Body.String())
	}
	if msg := mgmtErrorMessage(t, w); msg != quotaTimeoutMessage {
		t.Fatalf("error.message = %q, want %q", msg, quotaTimeoutMessage)
	}
	if strings.Contains(w.Body.String(), "://") || strings.Contains(w.Body.String(), "api.github.com") {
		t.Fatalf("超时错误包含上游 URL: %s", w.Body.String())
	}
}
