package copilot

// 本文件覆盖上游错误正文读取失败时的计数规则。
//   - 上游返回错误状态（≥400）时，读取其正文失败不计为网络错误，而按上游状态码的规则计数：
//     401/403 一次冷却（原因为该状态码）；429/5xx 计入连续错误（原因为该状态码）；400 与其他 4xx 既不计数也不清零。
//     只有上游状态 < 400 的流式成功响应在读取失败时才标记为网络错误。覆盖生成接口（流式与非流式）、session 交换与模型列表。
//   - 生成接口的网络错误与写超时包装的覆盖见 cooldown_count_test.go 的
//     TestCooldownGenerationNetworkErrorsCount：错误经 generationWriter 包装后仍能沿 Unwrap 找到统计用的 statusResponseWriter。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCooldownStreamedErrorBodyFailureCountsByStatus 生成接口的上游错误响应以 SSE 形式返回，且正文中途失败：按上游状态码计数，
// 而不是计为网络错误。401 一次冷却；429 连续第 3 次冷却，原因均为该状态码。
func TestCooldownStreamedErrorBodyFailureCountsByStatus(t *testing.T) {
	t.Run("401-cools-at-once", func(t *testing.T) {
		f := newCooldownFixture(t, "a")
		f.config.Client = cooldownPlanned(f.up, "oauth-a", []cooldownStep{
			{kind: stepBrokenStatus, status: http.StatusUnauthorized, stream: true},
		})
		p := f.start()
		cooldownChat(p)
		if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "401" {
			t.Fatalf("401 的正文中断后 until=%v reason=%q, want 立即冷却且原因 401", v.CooldownUntil, v.CooldownReason)
		}
	})
	t.Run("429-counts-to-third", func(t *testing.T) {
		f := newCooldownFixture(t, "a")
		f.config.Client = cooldownPlanned(f.up, "oauth-a", []cooldownStep{
			{kind: stepBrokenStatus, status: http.StatusTooManyRequests, stream: true},
			{kind: stepBrokenStatus, status: http.StatusTooManyRequests, stream: true},
			{kind: stepBrokenStatus, status: http.StatusTooManyRequests, stream: true},
		})
		p := f.start()
		for i := 1; i <= 2; i++ {
			cooldownChat(p)
			if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
				t.Fatalf("第 %d 次 429 的正文中断后已冷却：应按连续错误计数，第 3 次才冷却", i)
			}
		}
		cooldownChat(p)
		if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "429" {
			t.Fatalf("第 3 次 429 的正文中断后 until=%v reason=%q, want 冷却且原因 429", v.CooldownUntil, v.CooldownReason)
		}
	})
}

// TestCooldownErrorBodyFailure400NeitherCountsNorResets 生成接口的上游 400 正文读取失败（非流式，400 时需读取正文以判断是否为模型拒绝）：
// 不计数也不清零。序列 429、429、400（正文中断）、429 中，只有最后一次 429 触发冷却，原因为 429。
func TestCooldownErrorBodyFailure400NeitherCountsNorResets(t *testing.T) {
	t.Run("never-cools-alone", func(t *testing.T) {
		f := newCooldownFixture(t, "a")
		steps := make([]cooldownStep, 5)
		for i := range steps {
			steps[i] = cooldownStep{kind: stepBrokenStatus, status: http.StatusBadRequest}
		}
		f.config.Client = cooldownPlanned(f.up, "oauth-a", steps)
		p := f.start()
		for i := range steps {
			cooldownChat(p)
			if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
				t.Fatalf("第 %d 次上游 400 正文中断后账号冷却至 %s：400 不计数", i+1, v.CooldownUntil)
			}
		}
	})
	t.Run("does-not-reset-streak", func(t *testing.T) {
		f := newCooldownFixture(t, "a")
		f.config.Client = cooldownPlanned(f.up, "oauth-a", []cooldownStep{
			cooldownStatusStep(http.StatusTooManyRequests),
			cooldownStatusStep(http.StatusTooManyRequests),
			{kind: stepBrokenStatus, status: http.StatusBadRequest},
			cooldownStatusStep(http.StatusTooManyRequests),
		})
		p := f.start()
		cooldownChat(p)
		cooldownChat(p)
		cooldownChat(p) // 400 正文中断：不清零
		if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
			t.Fatalf("400 正文中断之后两次 429 已冷却：400 不应清零计数（且不应计数）")
		}
		cooldownChat(p)
		if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "429" {
			t.Fatalf("第 3 次连续 429 后 until=%v reason=%q, want 冷却且原因 429", v.CooldownUntil, v.CooldownReason)
		}
	})
}

// TestCooldownSessionErrorBodyFailureCountsByStatus session 交换返回的错误正文中途失败：按上游状态码计数。
// 401 一次冷却，原因为 401；400 不计数。
func TestCooldownSessionErrorBodyFailureCountsByStatus(t *testing.T) {
	t.Run("401-cools-at-once", func(t *testing.T) {
		f := newCooldownFixture(t, "a")
		f.config.Client = cooldownWrap(f.up, func(r *http.Request) (*http.Response, bool, error) {
			if r.URL.Path == "/copilot_internal/v2/token" {
				return cooldownHTTPResponse(r, http.StatusUnauthorized, "application/json", &cooldownBrokenBody{}), true, nil
			}
			return nil, false, nil
		})
		p := f.start()
		cooldownChat(p)
		if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "401" {
			t.Fatalf("session 交换 401 的正文中断后 until=%v reason=%q, want 立即冷却且原因 401", v.CooldownUntil, v.CooldownReason)
		}
	})
	t.Run("400-never-cools", func(t *testing.T) {
		f := newCooldownFixture(t, "a")
		f.config.Client = cooldownWrap(f.up, func(r *http.Request) (*http.Response, bool, error) {
			if r.URL.Path == "/copilot_internal/v2/token" {
				return cooldownHTTPResponse(r, http.StatusBadRequest, "application/json", &cooldownBrokenBody{}), true, nil
			}
			return nil, false, nil
		})
		p := f.start()
		for i := range 5 {
			cooldownChat(p)
			if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
				t.Fatalf("第 %d 次 session 交换 400 正文中断后账号冷却：400 不计数", i+1)
			}
		}
	})
}

// TestCooldownModelsErrorBodyFailureCountsByStatus 模型列表的上游错误正文中途失败：按上游状态码计数，
// 连续第 3 次 429 冷却，原因为 429（而不是 network）。
func TestCooldownModelsErrorBodyFailureCountsByStatus(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.Client = cooldownWrap(f.up, func(r *http.Request) (*http.Response, bool, error) {
		if r.URL.Path == "/models" {
			return cooldownHTTPResponse(r, http.StatusTooManyRequests, "application/json", &cooldownBrokenBody{}), true, nil
		}
		return nil, false, nil
	})
	p := f.start()
	for i := 1; i <= 3; i++ {
		poolRequest(p, "GET", "/v1/models", "")
		cooled := cooldownView(t, p, "a").CooldownUntil != nil
		if want := i == 3; cooled != want {
			t.Fatalf("第 %d 次模型列表 429 正文中断后冷却 = %v, want %v", i, cooled, want)
		}
	}
	if v := cooldownView(t, p, "a"); v.CooldownReason != "429" {
		t.Fatalf("模型列表 429 正文中断的冷却原因 = %q, want 429（不是 network）", v.CooldownReason)
	}
}

// cooldownFailingBody 第一次读取即失败：模拟上游在返回响应头之后立即断开，正文为空。
type cooldownFailingBody struct{}

func (cooldownFailingBody) Read([]byte) (int, error) {
	return 0, errors.New("unexpected connection close")
}

func (cooldownFailingBody) Close() error { return nil }

// TestUpstreamErrorWithUnreadableBodyIsForwardedCleanly 上游错误响应（声明了 Content-Length）的正文无法读取：
// 客户端收到上游的 401，能完整读取响应体（不出现 unexpected EOF），响应头中的长度与实际正文一致；
// 正文为空时写出 JSON 错误体，账号按 401 冷却（原因 401）。使用真实的 HTTP 服务，才能观察到响应头与正文是否一致。
func TestUpstreamErrorWithUnreadableBodyIsForwardedCleanly(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.Client = cooldownWrap(f.up, func(r *http.Request) (*http.Response, bool, error) {
		if r.URL.Path != "/copilot_internal/v2/token" {
			return nil, false, nil
		}
		resp := cooldownHTTPResponse(r, http.StatusUnauthorized, "application/json", cooldownFailingBody{})
		resp.Header.Set("Content-Length", "100") // 声明的长度与实际写出的正文不一致
		return resp, true, nil
	})
	p := f.start()
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("响应体应能完整读取，实际读取出错（响应头与正文不一致）: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 上游的 401", resp.StatusCode, body)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %s，与实际正文长度 %d 不一致", cl, len(body))
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error.Message == "" {
		t.Fatalf("正文应为 JSON 错误体（与 writeError 格式一致），实际 %q", body)
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil || v.CooldownReason != "401" {
		t.Fatalf("冷却状态 until=%v reason=%q, want 冷却且原因 401", v.CooldownUntil, v.CooldownReason)
	}
}

// TestUpstreamRedirectStatusIsNeverCounted 上游返回 3xx：既不计数，也不清零，更不标记为网络错误（与 session 路径的忽略规则一致）。
func TestUpstreamRedirectStatusIsNeverCounted(t *testing.T) {
	t.Run("three-redirects-never-cool", func(t *testing.T) {
		f := newCooldownFixture(t, "a")
		f.config.Client = cooldownWrap(f.up, func(r *http.Request) (*http.Response, bool, error) {
			if r.URL.Path == "/models" {
				return cooldownHTTPResponse(r, http.StatusFound, "text/plain", io.NopCloser(strings.NewReader(""))), true, nil
			}
			return nil, false, nil
		})
		p := f.start()
		for i := 1; i <= 3; i++ {
			poolRequest(p, "GET", "/v1/models", "")
		}
		if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
			t.Fatalf("三次 3xx 后账号冷却至 %s：3xx 不计数", v.CooldownUntil)
		}
	})
	t.Run("does-not-reset-streak", func(t *testing.T) {
		f := newCooldownFixture(t, "a")
		statuses := []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusFound, http.StatusTooManyRequests}
		next := 0
		f.config.Client = cooldownWrap(f.up, func(r *http.Request) (*http.Response, bool, error) {
			if r.URL.Path != "/models" {
				return nil, false, nil
			}
			st := statuses[next]
			next++
			return cooldownHTTPResponse(r, st, "application/json", io.NopCloser(strings.NewReader(cooldownBody(st)))), true, nil
		})
		p := f.start()
		for i := range statuses {
			poolRequest(p, "GET", "/v1/models", "")
			cooled := cooldownView(t, p, "a").CooldownUntil != nil
			if want := i == len(statuses)-1; cooled != want {
				t.Fatalf("第 %d 次请求后冷却 = %v, want %v（3xx 不得清零计数）", i+1, cooled, want)
			}
		}
		if v := cooldownView(t, p, "a"); v.CooldownReason != "429" {
			t.Fatalf("冷却原因 = %q, want 429（不得是 network）", v.CooldownReason)
		}
	})
}

// TestStreamedErrorResponseLengthIsConsistent 生成接口的上游错误响应为 SSE 且声明了与正文一致的 Content-Length 时，
// 客户端收到完整的错误正文，响应头中的长度与实际正文一致（不出现截断或 unexpected EOF）。
func TestStreamedErrorResponseLengthIsConsistent(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			body := cooldownBody(status)
			f.config.Client = cooldownWrap(f.up, func(r *http.Request) (*http.Response, bool, error) {
				if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer session-oauth-a" {
					return nil, false, nil
				}
				resp := cooldownHTTPResponse(r, status, "text/event-stream", io.NopCloser(strings.NewReader(body)))
				resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
				return resp, true, nil
			})
			p := f.start()
			srv := httptest.NewServer(p.Handler())
			t.Cleanup(srv.Close)

			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(poolChatBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("响应体应能完整读取: %v", err)
			}
			if resp.StatusCode != status || string(got) != body {
				t.Fatalf("status=%d body=%q, want %d %q", resp.StatusCode, got, status, body)
			}
			if cl := resp.Header.Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(got)) {
				t.Fatalf("Content-Length = %s，与实际正文长度 %d 不一致", cl, len(got))
			}
		})
	}
}
