package copilot

// 账号代理在账号池中的行为（见 docs/proxy.md「行为」「故障切换」）：账号的所有上游请求都走其代理，
// 无代理的账号直连；代理失败按网络错误处理并切换账号。

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// TestProxyAccountRequestsAllGoThroughProxy 带代理账号的 session 交换、模型列表、生成请求与额度刷新都经过其 SOCKS5 代理
// （代理要求用户名密码认证）：目标是域名，注入客户端的直连 DialContext 从未被调用（直连一旦发生会因被拒绝而失败）。
func TestProxyAccountRequestsAllGoThroughProxy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		run         func(t *testing.T, p *Pool)
		wantTargets []string
	}{
		{"模型列表（含 session 交换）", func(t *testing.T, p *Pool) {
			if w := poolRequest(p, "GET", "/v1/models", ""); w.Code != http.StatusOK {
				t.Fatalf("GET /v1/models status=%d body=%s", w.Code, w.Body.String())
			}
		}, []string{"api.github.com:443", "api.githubcopilot.com:443"}},
		{"生成请求（含 session 交换与模型列表）", func(t *testing.T, p *Pool) {
			if w := proxyChat(p); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oauth-alice") {
				t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
			}
		}, []string{"api.github.com:443", "api.githubcopilot.com:443"}},
		{"额度刷新", func(t *testing.T, p *Pool) {
			w := mgmtCall(p, "POST", "/v0/management/quota/refresh", `{"id":"alice"}`, mgmtKey)
			if w.Code != http.StatusOK {
				t.Fatalf("quota/refresh status=%d body=%s", w.Code, w.Body.String())
			}
			if view := mgmtAccountByID(t, mgmtAccounts(t, p), "alice"); view.Quota == nil {
				t.Fatalf("额度刷新后应有额度数据: %+v", view)
			}
		}, []string{"api.github.com:443"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newProxyEnv(t)
			socks := env.newSocks("proxy-user", proxyTestSecret)
			p, _ := env.newPool(proxyAccount("alice", socks.url("proxy-user", proxyTestSecret)))

			tc.run(t, p)

			for _, want := range tc.wantTargets {
				if !socks.hasTarget(want) {
					t.Fatalf("代理收到的 CONNECT 目标 = %v, want 包含 %s", socks.connectTargets(), want)
				}
			}
			if n := env.dial.callCount(); n != 0 {
				t.Fatalf("直连 DialContext 被调用 %d 次: %v, want 0", n, env.dial.dialedAddrs())
			}
		})
	}
}

// TestProxyOnlyAppliesToConfiguredAccount 同一个池中，带代理的账号经代理，无代理的账号直连，互不影响：
// 无代理账号的请求不会经过别人的代理。
func TestProxyOnlyAppliesToConfiguredAccount(t *testing.T) {
	env := newProxyEnv(t)
	env.allowDirect() // bob 没有代理，直连会被转发到假上游；直连次数仍被记录
	socks := env.newSocks()
	alice := proxyAccount("alice", socks.url())
	alice.Priority = 10 // 优先级高，先由 alice 处理
	p, _ := env.newPool(alice, proxyAccount("bob", ""))

	if w := proxyChat(p); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oauth-alice") {
		t.Fatalf("chat status=%d body=%s, want 由 alice 处理", w.Code, w.Body.String())
	}
	if socks.connectCount() == 0 || env.dial.callCount() != 0 {
		t.Fatalf("alice 的请求应只经代理: 代理 CONNECT=%d 直连=%d", socks.connectCount(), env.dial.callCount())
	}

	if w := cooldownPatch(p, `{"id":"alice","disabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("禁用 alice status=%d body=%s", w.Code, w.Body.String())
	}
	connectsBefore := socks.connectCount()
	if w := proxyChat(p); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oauth-bob") {
		t.Fatalf("chat status=%d body=%s, want 由 bob 处理", w.Code, w.Body.String())
	}
	if env.dial.callCount() == 0 {
		t.Fatal("无代理的 bob 应直连")
	}
	if socks.connectCount() != connectsBefore {
		t.Fatalf("bob 的请求不应经过 alice 的代理: CONNECT %d -> %d", connectsBefore, socks.connectCount())
	}
}

// TestProxyFailureFailsOverToAnotherAccount 带代理账号的代理不可达时，session 获取失败，请求切换到另一个账号并成功；
// 另一个账号使用自己的代理，仍不算绕过（直连从未发生）。单次失败不触发冷却。
func TestProxyFailureFailsOverToAnotherAccount(t *testing.T) {
	env := newProxyEnv(t)
	socks := env.newSocks()
	// 文件名 alice.json 排在 bob.json 之前，同档轮询先选 alice。
	p, _ := env.newPool(proxyAccount("alice", socksURL(deadProxyAddr(t))), proxyAccount("bob", socks.url()))

	w := proxyChat(p)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oauth-bob") {
		t.Fatalf("chat status=%d body=%s, want 切换到 bob 并成功", w.Code, w.Body.String())
	}
	assertGenerationOrder(t, env.up.pool, "oauth-bob")
	if n := env.dial.callCount(); n != 0 {
		t.Fatalf("直连被调用 %d 次: %v, want 0", n, env.dial.dialedAddrs())
	}
	if view := cooldownView(t, p, "alice"); view.CooldownUntil != nil {
		t.Fatalf("单次代理失败不应使 alice 冷却: %+v", view)
	}
}

// TestProxyFailureOnlyAccountReturnsProxyError 只有一个账号且代理不可达时，请求返回 502，错误含代理失败原因，
// 不含用户名与密码（响应与日志都是），也不回退直连；按网络错误规则计数：连续第 3 次进入冷却，原因为 network，之后返回 503。
func TestProxyFailureOnlyAccountReturnsProxyError(t *testing.T) {
	logs := captureSlog(t, slog.LevelDebug)
	env := newProxyEnv(t)
	dead := deadProxyAddr(t)
	p, _ := env.newPool(proxyAccount("alice", socksURL(dead, "proxy-user", proxyTestSecret)))

	for i := 1; i <= 3; i++ {
		w := proxyChat(p)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("第 %d 次 status=%d body=%s, want 502", i, w.Code, w.Body.String())
		}
		if want := "代理 " + dead + " 连接失败"; !strings.Contains(w.Body.String(), want) {
			t.Fatalf("第 %d 次响应 %s 应包含 %q", i, w.Body.String(), want)
		}
		for _, leaked := range []string{"proxy-user", proxyTestSecret} {
			if strings.Contains(w.Body.String(), leaked) {
				t.Fatalf("响应泄露了 %q: %s", leaked, w.Body.String())
			}
		}
		view := cooldownView(t, p, "alice")
		if (view.CooldownUntil != nil) != (i == 3) {
			t.Fatalf("第 %d 次后冷却 = %v, want 仅第 3 次后冷却（网络错误连续 3 次）", i, view.CooldownUntil)
		}
	}
	if view := cooldownView(t, p, "alice"); view.CooldownReason != "network" || view.Stats.Errors != 3 {
		t.Fatalf("冷却原因=%q 错误数=%d, want network 与 3", view.CooldownReason, view.Stats.Errors)
	}
	assertNoAccount(t, proxyChat(p))
	if n := env.dial.callCount(); n != 0 {
		t.Fatalf("代理失败后回退直连 %d 次, want 0", n)
	}
	p.WaitCooldownEffects()
	for _, leaked := range []string{"proxy-user", proxyTestSecret} {
		if strings.Contains(logs.String(), leaked) {
			t.Fatalf("日志泄露了 %q", leaked)
		}
	}
}
