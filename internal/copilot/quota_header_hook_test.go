package copilot

// 本文件覆盖响应头额度快照合并的接线（review M1）：钩子 onResponseHeaders 由 newPoolAccount 装配为
// account.applyQuotaHeaders，并在 Service.handleGeneration 拿到上游响应后（400 分支与流式透传之前）调用。
// 用例都经真实 HTTP 入口（testRequest / poolRequest）走 handleGeneration，断言账号缓存额度被合并：
// 钩子被删除、移到 400 分支的 return 之后、或装配遗漏时，合并都不会发生，断言随即失败。
// 覆盖 200 非流式/流式、400、429、500 的响应头合并，/responses 输入项失效重发后取最终响应的头，
// 以及经 Pool 创建的账号（newAccount → newPoolAccount，管理接口新增账号同路径）钩子已装配。

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// hookHeaderValue 是生成响应头里的额度快照：主额度剩余 42.5%，Additional Budget 已用 3（分母 100，见 headerTestQuota）。
const hookHeaderValue = "ent=300&rem=42.5&ov=3&ovPerm=true&rst=2026-11-01T00%3A00%3A00Z"

// hookReply 是测试上游对某次生成请求的应答，header 会原样写入响应头。
type hookReply struct {
	status      int
	body        string
	contentType string
	header      http.Header
}

// hookUpstream 模拟上游：按路径与到达序号返回应答，支撑 handleGeneration 的完整链路（session、模型列表、生成）。
type hookUpstream struct {
	mu      sync.Mutex
	calls   map[string]int
	respond func(path string, n int) hookReply
}

func (u *hookUpstream) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			testSession(w, "", time.Now().Add(time.Hour))
		case "/models":
			_, _ = io.WriteString(w, testModels)
		case "/responses", "/chat/completions", "/v1/messages":
			u.mu.Lock()
			u.calls[r.URL.Path]++
			reply := u.respond(r.URL.Path, u.calls[r.URL.Path])
			u.mu.Unlock()
			for key, values := range reply.header {
				w.Header()[key] = values
			}
			if reply.contentType != "" {
				w.Header().Set("Content-Type", reply.contentType)
			}
			w.WriteHeader(reply.status)
			_, _ = io.WriteString(w, reply.body)
		default:
			t.Errorf("意外的上游路径 %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func (u *hookUpstream) client(t *testing.T) *http.Client {
	return testClient(t, u.serve(t))
}

// hookService 创建使用 hookUpstream 的 Service，经 newPoolAccount 装配钩子（与生产同路径），返回账号供读写缓存额度。
func hookService(t *testing.T, respond func(path string, n int) hookReply) (*Service, *poolAccount) {
	t.Helper()
	u := &hookUpstream{calls: map[string]int{}, respond: respond}
	s := testService(t, Config{APIKey: testAPIKey, Token: "oauth-hook", User: "hook", Client: u.client(t)})
	return s, newPoolAccount(s, "hook")
}

// hookSeedQuota 给账号写入初始缓存额度（加锁）：主额度剩余 0.8，Additional Budget 剩余 0.75。
// 合并是 no-op 的前提是有缓存数据，因此必须先播种。
func hookSeedQuota(t *testing.T, account *poolAccount) {
	t.Helper()
	account.quota.mu.Lock()
	account.quota.data = headerTestQuota("Copilot Pro")
	account.quota.mu.Unlock()
}

// hookAssertMerged 断言 hookHeaderValue 已合并进缓存额度：主额度 0.425、Additional Budget 1-3/100=0.97。
func hookAssertMerged(t *testing.T, account *poolAccount) {
	t.Helper()
	quota, _ := account.quota.snapshot()
	if quota == nil {
		t.Fatal("缓存额度为空，钩子没有合并响应头")
	}
	if main := quotaTestBucket(t, *quota, "premium_requests"); !quotaTestApprox(main.RemainingFraction, 0.425) {
		t.Fatalf("主额度剩余比例 = %v, want 0.425（响应头 rem=42.5 未合并）", main.RemainingFraction)
	}
	if budget := quotaTestBucket(t, *quota, "additional_budget"); !quotaTestApprox(budget.RemainingFraction, 0.97) {
		t.Fatalf("Additional Budget 剩余比例 = %v, want 0.97（响应头 ov=3 未合并）", budget.RemainingFraction)
	}
}

// TestQuotaHeaderHookMergesOnGenerationResponses 经 handleGeneration 的各状态码出口都会合并响应头额度：
// 200 非流式与流式（流式透传出口）、400（原样返回出口）、429 与 500（错误状态也走流式透传，同样合并）。
func TestQuotaHeaderHookMergesOnGenerationResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		content string
	}{
		{name: "200-json", status: http.StatusOK, body: `{"served_by":"hook"}`, content: "application/json"},
		{name: "200-sse", status: http.StatusOK, body: "data: {\"id\":\"1\"}\n\n", content: "text/event-stream"},
		{name: "400", status: http.StatusBadRequest, body: `{"error":{"message":"bad request"}}`, content: "application/json"},
		{name: "429", status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`, content: "application/json"},
		{name: "500", status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`, content: "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, account := hookService(t, func(string, int) hookReply {
				return hookReply{status: tc.status, body: tc.body, contentType: tc.content,
					header: headerTestHeader(headerPremiumModels, hookHeaderValue)}
			})
			hookSeedQuota(t, account)
			w := testRequest(s, "POST", "/v1/chat/completions", poolChatBody)
			if w.Code != tc.status {
				t.Fatalf("客户端状态 = %d body=%s, want %d", w.Code, w.Body.String(), tc.status)
			}
			if tc.content == "text/event-stream" && !strings.Contains(w.Body.String(), "data:") {
				t.Fatalf("流式响应未透传给客户端: %q", w.Body.String())
			}
			hookAssertMerged(t, account)
		})
	}
}

// TestQuotaHeaderHookAfterStaleRetryUsesFinalHeader /responses 输入项失效（401）重发成功后，
// 合并的是最终那次响应的头：首次响应 rem=80、重发响应 rem=30，最终主额度应为 0.3 而不是 0.8。
// 钩子若在重发之前调用（合并到首次响应的头）或被删除，都会得到 0.8 或初始值 0.8，断言失败。
func TestQuotaHeaderHookAfterStaleRetryUsesFinalHeader(t *testing.T) {
	first := headerTestHeader(headerPremiumModels, "rem=80&ov=1")
	final := headerTestHeader(headerPremiumModels, "rem=30&ov=2")
	s, account := hookService(t, func(_ string, n int) hookReply {
		if n == 1 {
			return hookReply{status: http.StatusUnauthorized, body: stale401Body, contentType: "application/json", header: first}
		}
		return hookReply{status: http.StatusOK, body: staleOK.body, contentType: staleOK.contentType, header: final}
	})
	hookSeedQuota(t, account)
	w := testRequest(s, "POST", "/v1/responses", staleRequestBody)
	if w.Code != http.StatusOK || w.Body.String() != staleOK.body {
		t.Fatalf("status=%d body=%s, want 200 与重发后的应答", w.Code, w.Body.String())
	}
	quota, _ := account.quota.snapshot()
	if quota == nil {
		t.Fatal("缓存额度为空，钩子没有合并响应头")
	}
	main := quotaTestBucket(t, *quota, "premium_requests")
	if quotaTestApprox(main.RemainingFraction, 0.8) || !quotaTestApprox(main.RemainingFraction, 0.3) {
		t.Fatalf("主额度剩余比例 = %v, want 0.3（应取重发后响应的 rem=30，而不是首次的 rem=80）", main.RemainingFraction)
	}
}

// TestQuotaHeaderHookWiredForPoolAccounts 经 Pool 创建的账号（newAccount → newPoolAccount，管理接口新增账号同路径）
// 钩子已装配：池入口的生成请求结束后缓存额度被合并。
func TestQuotaHeaderHookWiredForPoolAccounts(t *testing.T) {
	u := &hookUpstream{calls: map[string]int{}, respond: func(string, int) hookReply {
		return hookReply{status: http.StatusOK, body: `{"served_by":"hook"}`, contentType: "application/json",
			header: headerTestHeader(headerPremiumModels, hookHeaderValue)}
	}}
	p := testPool(t, poolUserConfigs(t, u.client(t), "hook"))
	account := p.accountsSnapshot()[0]
	hookSeedQuota(t, account)
	if w := poolRequest(p, "POST", "/v1/chat/completions", poolChatBody); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	hookAssertMerged(t, account)
}
