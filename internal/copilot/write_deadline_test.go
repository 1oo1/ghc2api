package copilot

// 本文件覆盖生成请求结束后 keep-alive 连接的可用性。生成请求写入时 generationWriter 每次续期写截止时间，
// 请求结束时不清除它，因此最后一次 flush（handler 返回后由 net/http 写出残留缓冲与分块结束符）也受截止时间保护。
//
// 依赖的 net/http 行为：每个请求在 finishRequest 之后，serve 会调用 c.rwc.SetWriteDeadline(time.Time{}) 清除连接的写截止时间
// （HTTP/1.1 的 conn.serve；已在 Go 1.26.10.0 server.go:2099 与 1.27.1 server.go:2145 源码中确认，属于实现细节而非 API 承诺）。
// 因此 generationWriter 不自行清除截止时间，连接上也不会遗留过期的截止时间，之后复用该连接的请求不受影响。
// 测试使用真实 TCP 连接，并把 generationWriteTimeout 缩短为 200ms。
// 最后一次 flush 是否真正受超时保护难以稳定测试（需要客户端停止读取且缓冲恰好装满），这里不覆盖。

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeDeadlineTestTimeout 是测试中的 generationWriteTimeout。它足够短，使测试快速完成；本地单次写入远快于它，不会误触发超时。
const writeDeadlineTestTimeout = 200 * time.Millisecond

// writeDeadlineCases 是 Service 与 Pool 两种入口，分别搭配流式与非流式的上游生成响应。
var writeDeadlineCases = []struct {
	name        string
	entry       string // "service" 或 "pool"
	contentType string // 上游生成响应的 Content-Type
}{
	{name: "service-stream", entry: "service", contentType: "text/event-stream"},
	{name: "service-json", entry: "service", contentType: "application/json"},
	{name: "pool-stream", entry: "pool", contentType: "text/event-stream"},
	{name: "pool-json", entry: "pool", contentType: "application/json"},
}

// shortenWriteTimeout 把 generationWriteTimeout 改为 writeDeadlineTestTimeout，测试结束时恢复。
// 该变量是包级全局变量，使用它的测试不能并行。
func shortenWriteTimeout(t *testing.T) {
	t.Helper()
	previous := generationWriteTimeout
	generationWriteTimeout = writeDeadlineTestTimeout
	t.Cleanup(func() { generationWriteTimeout = previous })
}

// writeDeadlineResponse 返回上游生成接口的完整响应正文：流式为一个 SSE 数据块加 [DONE]，非流式为一个 JSON 对象。
func writeDeadlineResponse(contentType string) string {
	if contentType == "text/event-stream" {
		return "data: {\"served_by\":\"stream\"}\n\ndata: [DONE]\n\n"
	}
	return `{"served_by":"json"}`
}

// writeDeadlineFront 是 Service 或 Pool 的真实 TCP 入口，并记录服务端为客户端新建的每一条连接。
type writeDeadlineFront struct {
	url    string
	client *http.Client

	mu          sync.Mutex
	serverConns []net.Conn // 服务端接受的连接，用于统计连接数
}

// newWriteDeadlineFront 启动 entry 对应的 HTTP 入口；上游的生成接口返回 contentType 对应的完整响应。
// 客户端只保留一条空闲连接，因此后续请求若复用连接，就是同一条服务端连接。
func newWriteDeadlineFront(t *testing.T, entry, contentType string) *writeDeadlineFront {
	t.Helper()
	upstream := newPoolUpstream(t)
	upstreamClient := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			upstream.serve(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, writeDeadlineResponse(contentType))
	})

	var handler http.Handler
	switch entry {
	case "service":
		handler = withTestAPIKey(testService(t, Config{APIKey: testAPIKey, Token: "oauth-a", Client: upstreamClient}).Handler())
	case "pool":
		handler = withTestAPIKey(newLimitedPool(t, upstreamClient, 0).Handler())
	default:
		t.Fatalf("未知入口 %q", entry)
	}

	f := &writeDeadlineFront{}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		f.mu.Lock()
		f.serverConns = append(f.serverConns, c)
		f.mu.Unlock()
		return ctx
	}
	server.Start()
	t.Cleanup(server.Close)
	transport := &http.Transport{MaxIdleConnsPerHost: 1}
	t.Cleanup(transport.CloseIdleConnections)
	f.url = server.URL
	f.client = &http.Client{Transport: transport}
	return f
}

// do 发送一个请求并读完响应体，返回状态码，以及该请求是否复用了之前的连接。请求或读取失败时记录错误并返回 0 与 false，
// 由调用方的断言给出带原因的失败信息。
func (f *writeDeadlineFront) do(t *testing.T, method, path, body string) (status int, reused bool) {
	t.Helper()
	req, err := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
	res, err := f.client.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil {
		t.Errorf("%s %s 请求失败: %v", method, path, err)
		return 0, false
	}
	defer res.Body.Close()
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Errorf("%s %s 读取响应体失败: %v", method, path, err)
		return 0, false
	}
	return res.StatusCode, reused
}

// connCount 返回服务端接受的连接数。
func (f *writeDeadlineFront) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.serverConns)
}

// TestWriteDeadlineKeepAliveSequence 完成一个生成请求后等待超过 generationWriteTimeout，然后在同一条连接上依次请求
// /healthz 与生成接口：两者都得到 200，并且复用同一条连接。
// 它依赖 net/http 在每个请求结束后清除连接写截止时间（见文件头注释）：若没有这一清除，等待后遗留的过期截止时间会使
// /healthz 的响应写不出去，连接被关闭。若将来 net/http 不再清除截止时间，本测试应失败。
func TestWriteDeadlineKeepAliveSequence(t *testing.T) {
	shortenWriteTimeout(t)
	for _, tc := range writeDeadlineCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWriteDeadlineFront(t, tc.entry, tc.contentType)
			if status, _ := f.do(t, http.MethodPost, "/v1/chat/completions", poolChatBody); status != http.StatusOK {
				t.Fatalf("第一个生成请求 status = %d, want 200", status)
			}
			time.Sleep(2 * generationWriteTimeout)

			if status, reused := f.do(t, http.MethodGet, "/healthz", ""); status != http.StatusOK || !reused {
				t.Fatalf("/healthz status = %d, 复用连接 = %v, want 200 且复用连接", status, reused)
			}
			if status, reused := f.do(t, http.MethodPost, "/v1/chat/completions", poolChatBody); status != http.StatusOK || !reused {
				t.Fatalf("生成请求 status = %d, 复用连接 = %v, want 200 且复用连接", status, reused)
			}
			if n := f.connCount(); n != 1 {
				t.Fatalf("服务端连接数 = %d, want 1（三个请求应共用一条连接）", n)
			}
		})
	}
}
