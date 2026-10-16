package copilot

// 本文件覆盖生成请求的并发上限（PoolConfig.MaxConcurrentRequests）：名额已满时立即返回 503，且不读取请求体、
// 不选账号、不访问上游；模型列表、健康检查与管理接口不受限制；名额在请求处理结束后释放，包括流式响应结束、
// 全部账号的 session 获取失败、客户端中途断开，以及客户端长时间不读取导致写超时（generationWriteTimeout）。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldUpstream 模拟长时间占用名额的生成请求：生成请求到达后先写出一个 SSE 数据块并刷新，然后阻塞直到 releaseAll 被调用，
// 最后写出 [DONE] 结束流。其余路径交给 poolUpstream 处理。
type heldUpstream struct {
	pool    *poolUpstream
	client  *http.Client
	arrived chan struct{} // 每个到达上游的生成请求发送一次；缓冲足够大，不会阻塞上游
	release chan struct{}
	once    sync.Once

	mu          sync.Mutex
	generations int // 到达上游的生成请求数
}

func newHeldUpstream(t *testing.T) *heldUpstream {
	t.Helper()
	h := &heldUpstream{
		pool:    newPoolUpstream(t),
		arrived: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	h.client = testClient(t, h.serve)
	// 后注册的清理先执行：先释放阻塞中的请求，再由 testClient 关闭测试服务器。
	t.Cleanup(h.releaseAll)
	return h
}

func (h *heldUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/chat/completions" {
		h.pool.serve(w, r)
		return
	}
	h.mu.Lock()
	h.generations++
	h.mu.Unlock()
	h.arrived <- struct{}{}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"served_by\":\"held\"}\n\n")
	_ = http.NewResponseController(w).Flush()
	select {
	case <-h.release:
	case <-r.Context().Done():
		return
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// releaseAll 让所有被阻塞的生成请求继续并结束流；可重复调用。
func (h *heldUpstream) releaseAll() {
	h.once.Do(func() { close(h.release) })
}

// awaitArrival 等待一个生成请求到达上游；超时则终止测试。
func (h *heldUpstream) awaitArrival(t *testing.T) {
	t.Helper()
	select {
	case <-h.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("生成请求未到达上游")
	}
}

func (h *heldUpstream) generationCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.generations
}

// readSpy 包装一个请求体，并记录它是否被读取过。
type readSpy struct {
	body io.Reader
	read bool
}

func (s *readSpy) Read(p []byte) (int, error) {
	s.read = true
	return s.body.Read(p)
}

// newLimitedPool 创建只有一个账号（oauth-a）、并发上限为 limit 的托管池。会话亲和开启，使生成请求的体在选账号前会被读取，
// 从而能验证名额检查早于读取请求体。
func newLimitedPool(t *testing.T, client *http.Client, limit int) *Pool {
	t.Helper()
	return newLimitedPoolUsers(t, client, limit, "a")
}

// newLimitedPoolUsers 与 newLimitedPool 相同，但账号由 users 指定（每个账号的 token 为 "oauth-" + 用户名）。
func newLimitedPoolUsers(t *testing.T, client *http.Client, limit int, users ...string) *Pool {
	t.Helper()
	accounts := make([]PoolAccount, 0, len(users))
	for _, user := range users {
		accounts = append(accounts, PoolAccount{Token: "oauth-" + user, User: user})
	}
	p, err := NewManagedPool(PoolConfig{
		APIKey:        testAPIKey,
		ManagementKey: securityMgmtKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(), Client: client,
		SessionAffinityTTL: DefaultSessionAffinityTTL, MaxConcurrentRequests: limit,
	}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	quotaAwaitIdle(t, p)
	return p
}

// TestPoolRejectsGenerationWhenConcurrencyIsFull 名额为 1 时，第一个流式生成请求占用唯一名额；
// 第二个生成请求立即得到 503（writeError 格式的 JSON，消息含"并发"），且没有读取请求体、没有选账号、没有访问上游。
// 模型列表、健康检查与管理接口不受限制；第一个请求的流结束后名额释放，之后的生成请求成功。
func TestPoolRejectsGenerationWhenConcurrencyIsFull(t *testing.T) {
	up := newHeldUpstream(t)
	p := newLimitedPool(t, up.client, 1)

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- poolRequest(p, http.MethodPost, "/v1/chat/completions", poolChatBody) }()
	up.awaitArrival(t)
	requestsBefore := p.accounts[0].requests.Load()
	upstreamBefore := up.generationCount()

	spy := &readSpy{body: strings.NewReader(poolChatBody)}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", spy)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	withTestAPIKey(p.Handler()).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("名额已满时 status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || !strings.Contains(w.Body.String(), `"type":"copilot_proxy_error"`) {
		t.Errorf("503 应沿用 writeError 的 JSON 格式: Content-Type=%q body=%s", ct, w.Body.String())
	}
	if msg := mgmtErrorMessage(t, w); !strings.Contains(msg, "并发") {
		t.Errorf("错误消息 = %q, want 包含 并发", msg)
	}
	if spy.read {
		t.Error("名额已满时不应读取请求体")
	}
	if got := p.accounts[0].requests.Load(); got != requestsBefore {
		t.Errorf("名额已满的请求选中了账号: requests %d -> %d", requestsBefore, got)
	}
	if got := up.generationCount(); got != upstreamBefore {
		t.Errorf("名额已满的请求访问了上游: 生成请求数 %d -> %d", upstreamBefore, got)
	}

	for _, path := range []string{"/v1/models", "/healthz"} {
		if w := poolRequest(p, http.MethodGet, path, ""); w.Code != http.StatusOK {
			t.Errorf("名额已满时 GET %s status = %d, want 200; body=%s", path, w.Code, w.Body.String())
		}
	}
	if w := mgmtCall(p, http.MethodGet, "/v0/management/accounts", "", securityMgmtKey); w.Code != http.StatusOK {
		t.Errorf("名额已满时管理接口 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	up.releaseAll()
	select {
	case w := <-first:
		if w.Code != http.StatusOK {
			t.Fatalf("第一个流式请求 status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("第一个流式请求在释放后仍未结束")
	}
	if w := poolRequest(p, http.MethodPost, "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
		t.Fatalf("名额释放后的生成请求 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestPoolZeroConcurrencyLimitIsUnlimited MaxConcurrentRequests 为 0 表示不限制：三个流式生成请求同时到达上游。
func TestPoolZeroConcurrencyLimitIsUnlimited(t *testing.T) {
	up := newHeldUpstream(t)
	p := newLimitedPool(t, up.client, 0)

	const concurrent = 3
	results := make(chan *httptest.ResponseRecorder, concurrent)
	for range concurrent {
		go func() { results <- poolRequest(p, http.MethodPost, "/v1/chat/completions", poolChatBody) }()
	}
	for range concurrent {
		up.awaitArrival(t)
	}
	up.releaseAll()
	for range concurrent {
		select {
		case w := <-results:
			if w.Code != http.StatusOK {
				t.Errorf("不限制并发时生成请求 status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("并发生成请求在释放后仍未结束")
		}
	}
}

// TestPoolReleasesConcurrencySlotWhenAllSessionsFail 名额为 1、两个账号的 session 都获取失败时，请求以上游错误结束，
// 名额随之释放：连续的失败请求都不会因名额被占用而得到"并发数已达上限"的 503。
func TestPoolReleasesConcurrencySlotWhenAllSessionsFail(t *testing.T) {
	up := newPoolUpstream(t)
	for _, user := range []string{"a", "b"} {
		up.failSession("oauth-"+user, poolFailure{status: http.StatusUnauthorized, body: `{"message":"bad token"}`})
	}
	p := newLimitedPoolUsers(t, up.client(), 1, "a", "b")
	for i := range 2 {
		w := poolRequest(p, http.MethodPost, "/v1/chat/completions", poolChatBody)
		if w.Code == http.StatusOK {
			t.Fatalf("第 %d 次请求在 session 全部失败时返回 200", i+1)
		}
		if strings.Contains(w.Body.String(), "并发") {
			t.Fatalf("第 %d 次请求被名额占用拒绝，说明 session 失败后名额未释放: %s", i+1, w.Body.String())
		}
	}
	if up.sessionCount("oauth-a")+up.sessionCount("oauth-b") == 0 {
		t.Fatal("session 交换从未发生，测试没有覆盖失败路径")
	}
}

// TestPoolReleasesConcurrencySlotWhenClientDisconnects 名额为 1 时，客户端在流式响应中途断开（请求上下文取消）后，
// 请求结束并释放名额：之后的生成请求成功，而不是得到 503。
func TestPoolReleasesConcurrencySlotWhenClientDisconnects(t *testing.T) {
	up := newHeldUpstream(t)
	p := newLimitedPool(t, up.client, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(poolChatBody)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		withTestAPIKey(p.Handler()).ServeHTTP(httptest.NewRecorder(), r)
	}()
	up.awaitArrival(t)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("客户端断开后请求仍未结束")
	}
	up.releaseAll()
	if w := poolRequest(p, http.MethodPost, "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
		t.Fatalf("客户端断开后的生成请求 status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// endlessUpstream 模拟持续输出大量数据的生成请求：生成请求到达后不停地写入并刷新，直到写入失败（代理端已关闭连接）才返回，
// 返回时向 stopped 发送一次。其余路径交给 poolUpstream 处理。
type endlessUpstream struct {
	pool        *poolUpstream
	contentType string
	stopped     chan struct{}
}

func (e *endlessUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/chat/completions" {
		e.pool.serve(w, r)
		return
	}
	defer func() { e.stopped <- struct{}{} }()
	w.Header().Set("Content-Type", e.contentType)
	chunk := bytes.Repeat([]byte("x"), 32<<10)
	for {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
	}
}

// postChatHeadersOnly 通过真实 TCP 连接发送一个生成请求，只读取响应头，不读取响应体；调用方负责关闭响应体。
func postChatHeadersOnly(t *testing.T, baseURL string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(poolChatBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestGenerationWriteTimeoutReleasesSlot 客户端长时间不读取生成响应（流式与非流式）时，每次写入前续期的写截止时间到期，
// 写入失败，请求结束并释放名额：上游随之停止，之后的生成请求不再得到 503。
// 使用真实 TCP 连接，并把 generationWriteTimeout 改为 200ms，以便测试快速完成。
func TestGenerationWriteTimeoutReleasesSlot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
	}{
		{name: "stream", contentType: "text/event-stream"},
		{name: "non-stream", contentType: "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 先注册的清理后执行：先恢复变量，再关闭服务器。
			previous := generationWriteTimeout
			generationWriteTimeout = 200 * time.Millisecond
			t.Cleanup(func() { generationWriteTimeout = previous })

			up := &endlessUpstream{pool: newPoolUpstream(t), contentType: tc.contentType, stopped: make(chan struct{}, 16)}
			p := newLimitedPool(t, testClient(t, up.serve), 1)
			front := httptest.NewServer(withTestAPIKey(p.Handler()))
			t.Cleanup(front.Close)

			first := postChatHeadersOnly(t, front.URL)
			t.Cleanup(func() { _ = first.Body.Close() })
			if first.StatusCode != http.StatusOK {
				t.Fatalf("第一个生成请求 status = %d, want 200", first.StatusCode)
			}

			select {
			case <-up.stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("客户端长时间不读取时，上游写入没有因写超时而结束")
			}

			// 循环直到名额释放；最后一次请求必须得到 200，而不是其他状态码。
			deadline := time.Now().Add(5 * time.Second)
			var last *http.Response
			for {
				res := postChatHeadersOnly(t, front.URL)
				t.Cleanup(func() { _ = res.Body.Close() })
				if res.StatusCode != http.StatusServiceUnavailable {
					last = res
					break
				}
				_, _ = io.Copy(io.Discard, res.Body)
				if time.Now().After(deadline) {
					t.Fatal("写超时后名额仍未释放：生成请求持续得到 503")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if last.StatusCode != http.StatusOK {
				t.Fatalf("名额释放后的生成请求 status = %d, want 200", last.StatusCode)
			}
		})
	}
}
