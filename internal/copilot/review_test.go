package copilot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 元数据请求正在刷新时，排队的已取消请求不应继续等待网络响应。
func TestReviewCancelledMetadataWaiterReturnsPromptly(t *testing.T) {
	for _, kind := range []string{"session", "models", "auto"} {
		t.Run(kind, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			response := func(body string) *http.Response {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
			}
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				switch kind {
				case "session":
					return response(fmt.Sprintf(`{"token":"session","expires_at":%d}`, time.Now().Add(time.Hour).Unix())), nil
				case "models":
					return response(testModels), nil
				default:
					return response(fmt.Sprintf(`{"session_token":"auto","selected_model":"gpt-4.1","expires_at":%d}`, time.Now().Add(time.Hour).Unix())), nil
				}
			})}
			s := testService(t, Config{APIKey: testAPIKey, Token: "github-secret", Client: client})
			current := session{Token: "session", API: "https://api.githubcopilot.com"}
			call := func(ctx context.Context) error {
				switch kind {
				case "session":
					_, err := s.getSession(ctx)
					return err
				case "models":
					_, err := s.getModels(ctx, current)
					return err
				default:
					_, err := s.resolveAuto(ctx, current, "hi")
					return err
				}
			}
			first := make(chan error, 1)
			go func() { first <- call(context.Background()) }()
			<-started
			ctx, cancel := context.WithCancel(context.Background())
			waiter := make(chan error, 1)
			waiting := make(chan struct{})
			go func() {
				close(waiting)
				waiter <- call(ctx)
			}()
			<-waiting
			// 先确认第二个请求受首请求阻塞，再取消它；首请求仍不释放。
			select {
			case err := <-waiter:
				cancel()
				close(release)
				<-first
				t.Fatalf("刷新仍未完成，第二个请求却提前返回 %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			cancel()
			timedOut := false
			select {
			case err := <-waiter:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("取消等待返回 %v，预期 context.Canceled", err)
				}
			case <-time.After(time.Second):
				timedOut = true
			}
			close(release)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if timedOut {
				err := <-waiter
				t.Fatalf("已取消的 %s 请求一直等待另一请求完成，随后返回 %v", kind, err)
			}
		})
	}
}

// 重试额度耗尽也应丢弃失效的 Auto session，避免污染下一个请求。
func TestReviewLastAutoRejectionDoesNotPoisonNextRequest(t *testing.T) {
	picks, generations := 0, 0
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, "{}"
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			body = fmt.Sprintf(`{"token":"session","expires_at":%d}`, time.Now().Add(time.Hour).Unix())
		case "/models":
			body = testModels
		case "/auto":
			picks++
			body = fmt.Sprintf(`{"session_token":"auto-%d","selected_model":"gpt-4.1","expires_at":%d}`, picks, time.Now().Add(time.Hour).Unix())
		case "/chat/completions":
			generations++
			if r.Header.Get("Copilot-Session-Token") != "auto-4" {
				status, body = http.StatusBadRequest, `{"error":{"message":"not available for session"}}`
			}
		default:
			t.Fatalf("意外上游路径：%s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	s := testService(t, Config{APIKey: testAPIKey, Token: "github-secret", Client: client})
	body := `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`
	if w := testRequest(s, "POST", "/v1/chat/completions", body); w.Code != http.StatusBadRequest || generations != 3 {
		t.Fatalf("首次失败请求：状态=%d，生成次数=%d", w.Code, generations)
	}
	if w := testRequest(s, "POST", "/v1/chat/completions", body); w.Code != http.StatusOK {
		t.Fatalf("下一个请求应恢复：状态=%d，响应=%s", w.Code, w.Body.String())
	}
	if picks != 4 || generations != 4 {
		t.Fatalf("下一个请求重复使用了失效 session：选取次数=%d，生成次数=%d；预期均为 4", picks, generations)
	}
}
