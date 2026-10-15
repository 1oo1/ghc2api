package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// persistFileName 是账号额度在缓存目录中的文件名，由 writeCache("quota", ...) 写入。
const persistFileName = "quota.json"

// persistFixture 是一组可“重启”的托管池：凭证、缓存目录与上游保持不变，每次 start 相当于一次进程重启。
type persistFixture struct {
	config   PoolConfig
	accounts []PoolAccount
}

// newPersistFixture 为每个用户写入凭证（token 为 "oauth-<user>"，登录名即用户名），缓存目录为临时目录。
func newPersistFixture(t *testing.T, up *mgmtUpstream, users ...string) *persistFixture {
	t.Helper()
	dir := t.TempDir()
	f := &persistFixture{config: PoolConfig{
		ManagementKey: mgmtKey, CredentialsDir: dir, CacheDir: t.TempDir(), Client: up.client,
	}}
	for _, user := range users {
		token := "oauth-" + user
		if err := SaveCredential(filepath.Join(dir, user+".json"), Credential{Token: token, User: user}); err != nil {
			t.Fatal(err)
		}
		up.setLogin(token, user)
		f.accounts = append(f.accounts, PoolAccount{Token: token, User: user})
	}
	return f
}

// start 使用同一缓存目录与账号创建托管池，相当于重启进程后重新加载账号。
func (f *persistFixture) start(t *testing.T) *Pool {
	t.Helper()
	p, err := NewManagedPool(f.config, f.accounts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// persistQuotaPath 返回池内账号的 quota.json 路径（缓存目录由账号 Service 决定）。
func persistQuotaPath(t *testing.T, p *Pool, id string) string {
	t.Helper()
	account := p.findAccount(id)
	if account == nil {
		t.Fatalf("账号 %q 不存在", id)
	}
	return filepath.Join(account.service.cacheDir, persistFileName)
}

// persistReadFile 读取 quota.json 并解析为字段映射；文件不存在或不是 JSON 对象时终止测试。
func persistReadFile(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 quota.json: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		t.Fatalf("quota.json 不是 JSON 对象: %v; body=%s", err, data)
	}
	return fields
}

// persistWriteFile 以 0600 权限写入 quota.json 的原始内容，目录不存在时创建。
func persistWriteFile(t *testing.T, dir string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, persistFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// persistWriteQuota 手写 quota.json，字段名固定为 quota、checked_at、error。quota 为 nil 或 errText 为空时省略对应字段。
func persistWriteQuota(t *testing.T, dir string, quota *Quota, checkedAt time.Time, errText string) {
	t.Helper()
	value := map[string]any{"checked_at": checkedAt.Format(time.RFC3339)}
	if quota != nil {
		value["quota"] = quota
	}
	if errText != "" {
		value["error"] = errText
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	persistWriteFile(t, dir, data)
}

// persistTimeField 读取 RFC3339 时间字段，缺失或格式错误时终止测试。
func persistTimeField(t *testing.T, fields map[string]json.RawMessage, key string) time.Time {
	t.Helper()
	var text string
	if err := json.Unmarshal(fields[key], &text); err != nil {
		t.Fatalf("%s 字段缺失或不是字符串: %v", key, err)
	}
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("%s = %q, want RFC3339 时间: %v", key, text, err)
	}
	return at
}

// persistAccount 返回 GET accounts 中指定账号的视图。
func persistAccount(t *testing.T, p *Pool, id string) AccountView {
	t.Helper()
	return mgmtAccountByID(t, mgmtAccounts(t, p), id)
}

// persistRefresh 强制刷新账号额度并返回响应中的账号视图；响应必须是 200。
func persistRefresh(t *testing.T, p *Pool, id string) AccountView {
	t.Helper()
	w := mgmtCall(p, "POST", "/v0/management/quota/refresh", `{"id":"`+id+`"}`, mgmtKey)
	var view AccountView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatalf("强制刷新 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	return view
}

// persistPlan 返回额度的计划名；没有额度时返回空字符串。
func persistPlan(q *Quota) string {
	if q == nil || q.Subscription == nil {
		return ""
	}
	return q.Subscription.Plan
}

// persistSameQuota 断言两份额度的计划、汇总、窗口、Stale 与 ObservedAt 一致。
func persistSameQuota(t *testing.T, got, want *Quota) {
	t.Helper()
	if got == nil || want == nil {
		t.Fatalf("额度 got=%+v want=%+v, 两者都应存在", got, want)
	}
	if !got.ObservedAt.Equal(want.ObservedAt) || got.Stale != want.Stale ||
		!reflect.DeepEqual(got.Subscription, want.Subscription) ||
		!reflect.DeepEqual(got.Groups, want.Groups) || !reflect.DeepEqual(got.Summary, want.Summary) {
		t.Fatalf("重启后的额度 = %+v, want %+v", got, want)
	}
}

// TestQuotaFileLayoutAndPermission 额度写入账号缓存目录的 quota.json：字段名固定，权限为 0600。
// 成功时有 quota 与 checked_at 且没有错误；失败且没有旧数据时只有 checked_at 与非空 error。
func TestQuotaFileLayoutAndPermission(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		up := newMgmtUpstream(t)
		p := newPersistFixture(t, up, "alice").start(t)
		mgmtAccounts(t, p)
		path := persistQuotaPath(t, p, "alice")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("成功刷新后缺少 quota.json: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("quota.json 权限 = %o, want 600", perm)
		}
		fields := persistReadFile(t, path)
		if raw, ok := fields["quota"]; !ok || string(raw) == "null" {
			t.Fatalf("成功后缺少 quota 字段: %s", fields["quota"])
		}
		if checkedAt := persistTimeField(t, fields, "checked_at"); time.Since(checkedAt) > time.Minute {
			t.Fatalf("checked_at = %s, want 本次刷新的时间", checkedAt)
		}
		if raw, ok := fields["error"]; ok && string(raw) != `""` {
			t.Fatalf("成功后仍有 error 字段: %s", raw)
		}
	})

	t.Run("failure-without-old-data", func(t *testing.T) {
		up := newMgmtUpstream(t)
		up.failQuota("oauth-alice", http.StatusInternalServerError, `{"error":"down"}`)
		p := newPersistFixture(t, up, "alice").start(t)
		mgmtAccounts(t, p)
		fields := persistReadFile(t, persistQuotaPath(t, p, "alice"))
		if raw, ok := fields["quota"]; ok && string(raw) != "null" {
			t.Fatalf("没有旧数据的失败不应写入 quota: %s", raw)
		}
		var text string
		if err := json.Unmarshal(fields["error"], &text); err != nil || text == "" {
			t.Fatalf("失败后 error = %s, want 非空文本", fields["error"])
		}
		persistTimeField(t, fields, "checked_at")
	})
}

// TestQuotaRestoredAfterRestartWithoutUpstreamCall 刷新成功后重启：额度（含 ObservedAt）从缓存恢复，
// 2 小时节流窗口内 GET accounts 不再请求上游。
func TestQuotaRestoredAfterRestartWithoutUpstreamCall(t *testing.T) {
	up := newMgmtUpstream(t)
	f := newPersistFixture(t, up, "alice")
	before := persistAccount(t, f.start(t), "alice")
	if before.Quota == nil {
		t.Fatal("前提不成立：首次列表应拉取到额度")
	}
	calls := up.quotaCount("oauth-alice")

	after := persistAccount(t, f.start(t), "alice")
	if got := up.quotaCount("oauth-alice"); got != calls {
		t.Fatalf("重启后 GET accounts 请求了上游额度接口: %d -> %d, want 不变", calls, got)
	}
	persistSameQuota(t, after.Quota, before.Quota)
}

// TestQuotaStalePersistsAcrossRestart 429 限流后重启：保留旧数据并保持 Stale=true，节流窗口内不请求上游。
// 没有旧数据时，限流说明同样保存，重启后不再重复请求。
func TestQuotaStalePersistsAcrossRestart(t *testing.T) {
	t.Run("keeps-old-data-marked-stale", func(t *testing.T) {
		up := newMgmtUpstream(t)
		f := newPersistFixture(t, up, "alice")
		p1 := f.start(t)
		fresh := persistAccount(t, p1, "alice")
		if fresh.Quota == nil {
			t.Fatal("前提不成立：首次列表应拉取到额度")
		}
		up.failQuota("oauth-alice", http.StatusTooManyRequests, `{"error":"limited"}`)
		if view := persistRefresh(t, p1, "alice"); view.Quota == nil || !view.Quota.Stale {
			t.Fatalf("限流刷新后的视图 = %+v, want 保留旧数据且 stale=true", view.Quota)
		}
		calls := up.quotaCount("oauth-alice")

		after := persistAccount(t, f.start(t), "alice")
		want := *fresh.Quota
		want.Stale = true
		persistSameQuota(t, after.Quota, &want)
		if got := up.quotaCount("oauth-alice"); got != calls {
			t.Fatalf("限流后重启请求了上游 %d -> %d, want 不变", calls, got)
		}
	})

	t.Run("rate-limited-without-old-data", func(t *testing.T) {
		up := newMgmtUpstream(t)
		up.failQuota("oauth-alice", http.StatusTooManyRequests, `{"error":"limited"}`)
		f := newPersistFixture(t, up, "alice")
		before := persistAccount(t, f.start(t), "alice")
		if before.Quota != nil || before.QuotaError == "" {
			t.Fatalf("限流且没有旧数据时视图 = %+v, want 无额度且带 quota_error", before)
		}
		calls := up.quotaCount("oauth-alice")

		after := persistAccount(t, f.start(t), "alice")
		if after.Quota != nil || after.QuotaError != before.QuotaError {
			t.Fatalf("重启后视图 = %+v, want 无额度且 quota_error=%q", after, before.QuotaError)
		}
		if got := up.quotaCount("oauth-alice"); got != calls {
			t.Fatalf("限流错误重启后请求了上游 %d -> %d, want 不变", calls, got)
		}
	})
}

// TestQuotaErrorPersistsAcrossRestart 刷新失败后重启：有旧数据时旧数据与 quota_error 都保留；
// 没有旧数据时 quota_error 保留，且节流窗口内不再重复请求。
func TestQuotaErrorPersistsAcrossRestart(t *testing.T) {
	t.Run("keeps-old-data-and-error", func(t *testing.T) {
		up := newMgmtUpstream(t)
		f := newPersistFixture(t, up, "alice")
		p1 := f.start(t)
		fresh := persistAccount(t, p1, "alice")
		if fresh.Quota == nil {
			t.Fatal("前提不成立：首次列表应拉取到额度")
		}
		up.failQuota("oauth-alice", http.StatusInternalServerError, `{"error":"down"}`)
		failed := persistRefresh(t, p1, "alice")
		if failed.QuotaError == "" {
			t.Fatal("刷新失败后 quota_error 为空")
		}
		calls := up.quotaCount("oauth-alice")

		after := persistAccount(t, f.start(t), "alice")
		if after.QuotaError != failed.QuotaError {
			t.Fatalf("重启后 quota_error = %q, want %q", after.QuotaError, failed.QuotaError)
		}
		persistSameQuota(t, after.Quota, fresh.Quota)
		if got := up.quotaCount("oauth-alice"); got != calls {
			t.Fatalf("失败后重启请求了上游 %d -> %d, want 不变", calls, got)
		}
	})

	t.Run("error-without-old-data", func(t *testing.T) {
		up := newMgmtUpstream(t)
		up.failQuota("oauth-alice", http.StatusInternalServerError, `{"error":"down"}`)
		f := newPersistFixture(t, up, "alice")
		before := persistAccount(t, f.start(t), "alice")
		if before.Quota != nil || before.QuotaError == "" {
			t.Fatalf("失败且没有旧数据时视图 = %+v, want 无额度且带 quota_error", before)
		}
		calls := up.quotaCount("oauth-alice")

		after := persistAccount(t, f.start(t), "alice")
		if after.Quota != nil || after.QuotaError != before.QuotaError {
			t.Fatalf("重启后视图 = %+v, want 无额度且 quota_error=%q", after, before.QuotaError)
		}
		if got := up.quotaCount("oauth-alice"); got != calls {
			t.Fatalf("失败后重启请求了上游 %d -> %d, want 不变", calls, got)
		}
	})
}

// TestQuotaCheckedAtWindowAcrossRestart 以 checked_at 为准判断节流：距今不足 2 小时时重启后直接使用恢复的额度，
// 超过 2 小时则重启后的列表重新请求上游。
func TestQuotaCheckedAtWindowAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		age         time.Duration // checked_at 距重启的时长
		wantRequest bool          // 重启后的 GET accounts 是否请求上游
	}{
		{name: "within-two-hours", age: quotaCacheTTL - time.Minute},
		{name: "beyond-two-hours", age: quotaCacheTTL + time.Minute, wantRequest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			f := newPersistFixture(t, up, "alice")
			p1 := f.start(t)
			checkedAt := time.Now().Add(-tc.age)
			restored := Quota{
				Subscription: &QuotaSubscription{Plan: "Copilot Student", TierName: "Copilot Student"},
				ObservedAt:   checkedAt,
			}
			persistWriteQuota(t, p1.findAccount("alice").service.cacheDir, &restored, checkedAt, "")

			view := persistAccount(t, f.start(t), "alice")
			got := up.quotaCount("oauth-alice")
			if !tc.wantRequest {
				if got != 0 {
					t.Fatalf("checked_at 在 2 小时内时重启后请求了上游 %d 次, want 0", got)
				}
				if plan := persistPlan(view.Quota); plan != "Copilot Student" {
					t.Fatalf("应直接返回恢复的额度，计划 = %q, want Copilot Student", plan)
				}
				return
			}
			if got != 1 {
				t.Fatalf("checked_at 超过 2 小时后重启请求了上游 %d 次, want 1", got)
			}
			if plan := persistPlan(view.Quota); plan != "Copilot Pro" {
				t.Fatalf("超过 2 小时后应重新拉取额度，计划 = %q, want Copilot Pro", plan)
			}
		})
	}
}

// TestQuotaFutureCheckedAtIsInvalid checked_at 晚于当前时间（时钟回拨或文件来自未来）时视为无效，按从未检查过处理：
// 重启后旧额度仍从缓存恢复并可读，但重启本身不请求上游；随后 GET accounts 请求一次上游，并返回新拉取的额度。
func TestQuotaFutureCheckedAtIsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ahead time.Duration // checked_at 相对当前时间的未来偏移
	}{
		{name: "one-day-ahead", ahead: 24 * time.Hour},
		{name: "one-year-ahead", ahead: 365 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			f := newPersistFixture(t, up, "alice")
			p1 := f.start(t)
			restored := Quota{
				Subscription: &QuotaSubscription{Plan: "Copilot Student", TierName: "Copilot Student"},
				ObservedAt:   time.Now().Add(-time.Hour),
			}
			persistWriteQuota(t, p1.findAccount("alice").service.cacheDir, &restored, time.Now().Add(tc.ahead), "")
			calls := up.quotaCount("oauth-alice")

			p2 := f.start(t)
			// 刷新前：旧额度已从缓存恢复，且重启本身没有请求上游。
			if q, _ := p2.findAccount("alice").quota.snapshot(); persistPlan(q) != "Copilot Student" {
				t.Fatalf("重启后未恢复旧额度，计划 = %q, want Copilot Student", persistPlan(q))
			}
			if got := up.quotaCount("oauth-alice"); got != calls {
				t.Fatalf("重启本身请求了上游 %d -> %d, want 不变", calls, got)
			}

			// 列表：checked_at 视为无效，请求一次上游并返回新拉取的额度。
			view := persistAccount(t, p2, "alice")
			if got := up.quotaCount("oauth-alice"); got != calls+1 {
				t.Fatalf("checked_at 晚于当前时间后列表请求上游 %d 次, want 1", got-calls)
			}
			if plan := persistPlan(view.Quota); plan != "Copilot Pro" {
				t.Fatalf("checked_at 晚于当前时间后应返回新拉取的额度，计划 = %q, want Copilot Pro", plan)
			}
		})
	}
}

// TestCorruptQuotaFileIsIgnored 损坏（非法 JSON）或超过大小限制的 quota.json 被静默忽略：启动正常、账号没有额度，
// 之后的列表正常拉取，并把合法内容写回。
func TestCorruptQuotaFileIsIgnored(t *testing.T) {
	for _, tc := range []struct {
		name string
		data func(t *testing.T) []byte
	}{
		{name: "invalid-json", data: func(*testing.T) []byte {
			return []byte(`{"quota":{"subscription":`)
		}},
		{name: "oversized", data: func(*testing.T) []byte {
			// 合法 JSON，但超过读取上限；错误文本若被恢复会被发现。
			return []byte(`{"checked_at":"` + time.Now().Format(time.RFC3339) + `","error":"` +
				strings.Repeat("x", maxCacheFileSize) + `"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			f := newPersistFixture(t, up, "alice")
			p1 := f.start(t)
			path := persistQuotaPath(t, p1, "alice")
			persistWriteFile(t, filepath.Dir(path), tc.data(t))

			p2 := f.start(t)
			if q, errText := p2.findAccount("alice").quota.snapshot(); q != nil || errText != "" {
				t.Fatalf("损坏的 quota.json 应被忽略，得到 quota=%+v error=%q", q, errText)
			}
			view := persistAccount(t, p2, "alice")
			if got := up.quotaCount("oauth-alice"); got != 1 {
				t.Fatalf("损坏的缓存后列表请求上游 %d 次, want 1", got)
			}
			if plan := persistPlan(view.Quota); plan != "Copilot Pro" || view.QuotaError != "" {
				t.Fatalf("损坏的缓存后列表视图 = %+v, want 正常拉取的额度", view)
			}
			// 超大文件本身是合法 JSON，因此用 quota 字段确认文件已被本次拉取的结果替换。
			if raw := persistReadFile(t, path)["quota"]; len(raw) == 0 || string(raw) == "null" {
				t.Fatal("拉取成功后 quota.json 应被替换为包含额度的内容")
			}
		})
	}
}

// TestQuotaNotRecreatedAfterAccountDeleted 删除账号后缓存目录不存在；删除期间进行中的刷新与之后的刷新都不会重新创建缓存目录或 quota.json。
func TestQuotaNotRecreatedAfterAccountDeleted(t *testing.T) {
	up := newMgmtUpstream(t)
	p := newPersistFixture(t, up, "alice", "bob").start(t)
	mgmtAccounts(t, p)
	alice := p.findAccount("alice")
	cacheDir := alice.service.cacheDir
	if _, err := os.Stat(filepath.Join(cacheDir, persistFileName)); err != nil {
		t.Fatalf("删除前 alice 的 quota.json 应存在: %v", err)
	}

	// 强制刷新在上游阻塞，删除账号后才完成。
	hold := up.holdQuota("oauth-alice")
	refreshed := make(chan error, 1)
	go func() { refreshed <- alice.refresh(context.Background(), true) }()
	reviewAwait(t, hold.started, "额度刷新未到达上游")
	if w := mgmtCall(p, "DELETE", "/v0/management/accounts?id=alice", "", mgmtKey); w.Code != http.StatusOK {
		t.Fatalf("删除 alice status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(cacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("删除后缓存目录仍存在（stat 错误 = %v）", err)
	}
	hold.free()
	select {
	case err := <-refreshed:
		if err != nil {
			t.Fatalf("删除期间的刷新返回错误: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("进行中的额度刷新没有完成")
	}

	if err := alice.refresh(context.Background(), true); err != nil {
		t.Fatalf("删除后的强制刷新返回错误: %v", err)
	}
	if _, err := os.Stat(cacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("删除账号后缓存目录被重新创建（stat 错误 = %v）", err)
	}
}

// TestCancelledQuotaRefreshDoesNotWriteFile 调用方取消（客户端断开）的额度刷新不写 quota.json，
// 已有文件的内容也保持不变。
func TestCancelledQuotaRefreshDoesNotWriteFile(t *testing.T) {
	t.Run("list-without-old-data", func(t *testing.T) {
		up := newMgmtUpstream(t)
		p := newPersistFixture(t, up, "alice").start(t)
		path := persistQuotaPath(t, p, "alice")
		hold := up.holdQuota("oauth-alice")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := httptest.NewRequest("GET", "/v0/management/accounts", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+mgmtKey)
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.Handler().ServeHTTP(httptest.NewRecorder(), r)
		}()
		reviewAwait(t, hold.started, "额度请求未到达上游")
		cancel()
		reviewAwait(t, done, "客户端取消后的账号列表")

		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("调用方取消后写入了 quota.json（stat 错误 = %v）", err)
		}
	})

	t.Run("refresh-keeps-existing-file", func(t *testing.T) {
		up := newMgmtUpstream(t)
		p := newPersistFixture(t, up, "alice").start(t)
		mgmtAccounts(t, p)
		path := persistQuotaPath(t, p, "alice")
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("前提不成立：成功刷新后应有 quota.json: %v", err)
		}
		hold := up.holdQuota("oauth-alice")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := httptest.NewRequest("POST", "/v0/management/quota/refresh", strings.NewReader(`{"id":"alice"}`)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+mgmtKey)
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.Handler().ServeHTTP(httptest.NewRecorder(), r)
		}()
		reviewAwait(t, hold.started, "强制刷新未到达上游")
		cancel()
		reviewAwait(t, done, "客户端取消后的强制刷新")

		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("调用方取消的强制刷新改写了 quota.json: 前 %s, 后 %s, err=%v", before, after, err)
		}
	})
}

// TestQuotaPersistenceSkippedWithoutCacheDir 缓存目录不可用（账号没有 cacheDir）时不读写 quota.json，也不报错；
// 额度只保存在内存中，因此重启后会重新请求上游。
func TestQuotaPersistenceSkippedWithoutCacheDir(t *testing.T) {
	// 清空 HOME 与 XDG_CACHE_HOME，使 os.UserCacheDir 失败，账号因此没有缓存目录。
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	up := newMgmtUpstream(t)
	dir := t.TempDir()
	up.setLogin("oauth-alice", "alice")
	if err := SaveCredential(filepath.Join(dir, "alice.json"), Credential{Token: "oauth-alice", User: "alice"}); err != nil {
		t.Fatal(err)
	}
	config := PoolConfig{ManagementKey: mgmtKey, CredentialsDir: dir, Client: up.client}
	accounts := []PoolAccount{{Token: "oauth-alice", User: "alice"}}

	p1, err := NewManagedPool(config, accounts)
	if err != nil {
		t.Fatalf("没有缓存目录时创建池失败: %v", err)
	}
	if got := p1.findAccount("alice").service.cacheDir; got != "" {
		t.Fatalf("测试前提不成立：cacheDir = %q, want 空", got)
	}
	if plan := persistPlan(persistAccount(t, p1, "alice").Quota); plan != "Copilot Pro" {
		t.Fatalf("内存中的额度计划 = %q, want Copilot Pro", plan)
	}

	p2, err := NewManagedPool(config, accounts)
	if err != nil {
		t.Fatalf("重启创建池失败: %v", err)
	}
	if plan := persistPlan(persistAccount(t, p2, "alice").Quota); plan != "Copilot Pro" {
		t.Fatalf("重启后额度计划 = %q, want 重新拉取的 Copilot Pro", plan)
	}
	if got := up.quotaCount("oauth-alice"); got != 2 {
		t.Fatalf("没有缓存目录时重启应重新请求上游，实际 %d 次, want 2", got)
	}
}

// TestRestoredQuotaAppliesToImportedAndDeviceLoginAccounts 导入与设备登录新增的账号同样从缓存恢复额度。
// 两种入口在校验时都会请求一次额度接口，因此断言的是列表前后的请求次数不变。
func TestRestoredQuotaAppliesToImportedAndDeviceLoginAccounts(t *testing.T) {
	const token = "oauth-carol"
	for _, tc := range []struct {
		name string
		add  func(t *testing.T, up *mgmtUpstream, p *Pool)
	}{
		{name: "import", add: func(t *testing.T, up *mgmtUpstream, p *Pool) {
			up.setLogin(token, "Carol")
			if w := mgmtCall(p, "POST", "/v0/management/accounts", `{"credential":{"oauth_token":"oauth-carol"}}`, mgmtKey); w.Code != http.StatusOK {
				t.Fatalf("导入 status=%d body=%s", w.Code, w.Body.String())
			}
		}},
		{name: "device-login", add: func(t *testing.T, up *mgmtUpstream, p *Pool) {
			up.setDeviceLogin(token, "Carol")
			w := mgmtCall(p, "POST", "/v0/management/login", "", mgmtKey)
			var start mgmtLoginStart
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &start) != nil {
				t.Fatalf("发起登录 status=%d body=%s", w.Code, w.Body.String())
			}
			if result := mgmtPollLogin(t, p, start.State); result.Status != "success" {
				t.Fatalf("设备登录结果 = %+v, want success", result)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			f := newPersistFixture(t, up)
			p := f.start(t)
			// 账号尚未加入池，因此用同样的 Host 与 Token 计算它的缓存目录。
			seed, err := New(Config{Token: token, CacheDir: f.config.CacheDir})
			if err != nil {
				t.Fatal(err)
			}
			restored := Quota{
				Subscription: &QuotaSubscription{Plan: "Copilot Student", TierName: "Copilot Student"},
				ObservedAt:   time.Now(),
			}
			persistWriteQuota(t, seed.cacheDir, &restored, time.Now(), "")

			tc.add(t, up, p)
			before := up.quotaCount(token)
			view := mgmtAccountByID(t, mgmtAccounts(t, p), "carol")
			if got := up.quotaCount(token); got != before {
				t.Fatalf("列表请求了上游额度接口 %d -> %d, want 不变（应使用恢复的额度）", before, got)
			}
			if plan := persistPlan(view.Quota); plan != "Copilot Student" {
				t.Fatalf("恢复的额度计划 = %q, want Copilot Student", plan)
			}
		})
	}
}
