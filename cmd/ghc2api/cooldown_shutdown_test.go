package main

// 本文件覆盖关闭时等待进入冷却的异步副作用有上界。
// 副作用被阻塞（这里让模型列表的读取持有模型锁，使清理无法完成）时，saveSessionAffinity 不会无限等待：
// 超时后输出 Warn 并继续保存会话亲和表。测试只通过 copilot 的公开 API 与 saveSessionAffinity 观察，
// 因此不依赖等待的具体实现（WaitCooldownEffectsTimeout 或 cmd 侧的 goroutine+select 均可）。

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ghc2api/internal/copilot"
)

// shutdownAPIKey 是关闭测试的服务访问密钥。
const shutdownAPIKey = "shutdown-test-api-key"

// shutdownUpstream 模拟 GitHub 与 Copilot 的上游。session 交换在 failing 之前成功（有效期不足 2 分钟，每次取 session 都会重新交换）；
// /models 在 block 开启时阻塞：第一次到达时关闭 started 并把 failing 置为 true，之后的 session 交换都返回 401，
// 阻塞直到 release 被关闭（阻塞期间持有该账号的模型锁）。
type shutdownUpstream struct {
	mu      sync.Mutex
	block   bool
	failing bool
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (u *shutdownUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/copilot_internal/v2/token"):
		u.mu.Lock()
		failing := u.failing
		u.mu.Unlock()
		if failing {
			return shutdownResponse(r, http.StatusUnauthorized, `{"message":"revoked"}`), nil
		}
		expires := time.Now().Add(time.Minute).Unix()
		body, _ := json.Marshal(map[string]any{
			"token": "session-oauth-alice", "expires_at": expires, "endpoints": map[string]string{"api": ""},
		})
		return shutdownResponse(r, http.StatusOK, string(body)), nil
	case r.URL.Path == "/models":
		u.mu.Lock()
		blocked := u.block
		u.mu.Unlock()
		if blocked {
			u.mu.Lock()
			u.failing = true
			u.mu.Unlock()
			u.once.Do(func() { close(u.started) })
			<-u.release
		}
		return shutdownResponse(r, http.StatusOK, `{"data":[]}`), nil
	default:
		return shutdownResponse(r, http.StatusNotFound, `{}`), nil
	}
}

// shutdownResponse 构造模拟的上游响应。
func shutdownResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

// TestSaveSessionAffinityBoundsWaitForCooldownEffects 进入冷却的副作用被阻塞时，关闭时的保存不会无限等待：
// 在有限时间内返回，输出 Warn，并仍然保存会话亲和表文件。
func TestSaveSessionAffinityBoundsWaitForCooldownEffects(t *testing.T) {
	up := &shutdownUpstream{block: true, started: make(chan struct{}), release: make(chan struct{})}
	releaseOnce := sync.OnceFunc(func() { close(up.release) })
	cacheDir := t.TempDir()
	pool, err := copilot.NewManagedPool(copilot.PoolConfig{
		APIKey:             shutdownAPIKey,
		CacheDir:           cacheDir,
		SessionAffinityTTL: copilot.DefaultSessionAffinityTTL,
		ModelsCacheTTL:     time.Nanosecond, // 每次都读取模型列表，使模型锁的阻塞可控
		Client:             &http.Client{Transport: up},
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, []copilot.PoolAccount{{Token: "oauth-alice", User: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	// 后台 goroutine 的通道在清理之前声明：清理时必须等待它们结束，否则可能在 TempDir 删除时仍写入缓存。
	listed := make(chan struct{})
	saved := make(chan struct{})
	t.Cleanup(func() {
		releaseOnce()
		pool.WaitCooldownEffects()
		for name, ch := range map[string]chan struct{}{"/v1/models 请求": listed, "关闭保存": saved} {
			select {
			case <-ch:
			case <-time.After(10 * time.Second):
				t.Errorf("%s 未结束", name)
			}
		}
	})

	// 第一个请求阻塞在模型列表的上游请求中，持有模型锁。
	go func() {
		defer close(listed)
		shutdownRequest(pool, "GET", "/v1/models")
	}()
	select {
	case <-up.started:
	case <-time.After(5 * time.Second):
		t.Fatal("/v1/models 未到达模型列表上游")
	}
	// 第二个请求的 session 交换返回 401：账号进入冷却，清理副作用等待模型锁，无法完成。
	if w := shutdownRequest(pool, "GET", "/v1/models"); w.Code != http.StatusUnauthorized {
		t.Fatalf("冷却触发请求 status=%d body=%s, want 401", w.Code, w.Body.String())
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	done := saved
	start := time.Now()
	go func() {
		defer close(done)
		saveSessionAffinity(pool, true, logger)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("关闭时等待冷却副作用无上界：应在超时（5 秒）后继续保存")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("等待冷却副作用耗时 %s，应有上界", elapsed)
	}

	if shutdownWarnCount(t, logs.String()) == 0 {
		t.Errorf("等待冷却副作用超时应输出 Warn 日志；输出: %s", logs.String())
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "session_affinity.json")); err != nil {
		t.Errorf("超时后仍应保存会话亲和表文件: %v", err)
	}
}

// shutdownWarnCount 统计日志（JSON 行）中级别为 WARN 的条目数。
func shutdownWarnCount(t *testing.T, output string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSON log line %q: %v", line, err)
		}
		if entry["level"] == "WARN" {
			n++
		}
	}
	return n
}

// shutdownRequest 通过池的 HTTP 入口发送请求，携带服务访问密钥。
func shutdownRequest(pool *copilot.Pool, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+shutdownAPIKey)
	w := httptest.NewRecorder()
	pool.Handler().ServeHTTP(w, r)
	return w
}
