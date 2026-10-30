package copilot

// proxyClient 的行为测试（见 docs/proxy.md「行为」）：经 SOCKS5 访问、DNS 由代理端解析、认证、不绕过、错误信息脱敏。

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// proxyPingServer 返回一个对任何路径回复 pong 的 TLS 服务器，作为 SOCKS5 服务器转发的终点。
func proxyPingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pong")
	}))
	t.Cleanup(server.Close)
	return server
}

// proxyGet 用 client 请求 url，成功时返回正文。
func proxyGet(client *http.Client, target string) (string, error) {
	res, err := client.Get(target)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	return string(body), err
}

// TestProxyClientRoutesThroughSocksWithRemoteDNS 请求经 SOCKS5 代理到达目标；目标以域名原样交给代理（本地不解析），
// 保留 base 的 TLS 配置；注入的直连 DialContext 从未被调用。socks5 与 socks5h 行为相同。
func TestProxyClientRoutesThroughSocksWithRemoteDNS(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			target := proxyPingServer(t)
			socks := newSocksServer(t, target.Listener.Addr().String())
			dial := &dialRecorder{}
			client, err := proxyClient(dial.client(), scheme+"://"+socks.addr())
			if err != nil {
				t.Fatal(err)
			}
			body, err := proxyGet(client, "https://api.github.com/ping")
			if err != nil || body != "pong" {
				t.Fatalf("经代理请求 = %q, %v; want pong", body, err)
			}
			if got := socks.connectTargets(); len(got) != 1 || got[0] != "api.github.com:443" {
				t.Fatalf("代理收到的 CONNECT 目标 = %v, want [api.github.com:443]（域名应原样交给代理解析）", got)
			}
			if n := dial.callCount(); n != 0 {
				t.Fatalf("直连 DialContext 被调用 %d 次, want 0", n)
			}
		})
	}
}

// TestProxyClientSocksAuthentication 用户名密码经 RFC 1929 认证（含需要百分号编码的特殊字符）；
// 密码错误时连接失败，错误为“代理 <host:port> 连接失败”，不含用户名与密码。
func TestProxyClientSocksAuthentication(t *testing.T) {
	user, pass := "proxy-user@x", proxyTestSecret+":/ w"
	target := proxyPingServer(t)
	socks := newSocksServer(t, target.Listener.Addr().String(), user, pass)

	client, err := proxyClient((&dialRecorder{}).client(), socks.url(user, pass))
	if err != nil {
		t.Fatal(err)
	}
	if body, err := proxyGet(client, "https://api.github.com/ping"); err != nil || body != "pong" {
		t.Fatalf("正确凭据请求 = %q, %v; want pong", body, err)
	}

	wrong, err := proxyClient((&dialRecorder{}).client(), socks.url(user, "wrong-password"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = proxyGet(wrong, "https://api.github.com/ping")
	if err == nil {
		t.Fatal("密码错误时请求必须失败")
	}
	if want := "代理 " + socks.addr() + " 连接失败"; !strings.Contains(err.Error(), want) {
		t.Fatalf("错误 %q 应包含 %q", err, want)
	}
	for _, leaked := range []string{user, "proxy-user", "wrong-password", proxyTestSecret} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("错误信息泄露了 %q: %v", leaked, err)
		}
	}
	if socks.authFailures() != 1 {
		t.Fatalf("认证失败次数 = %d, want 1", socks.authFailures())
	}
}

// TestProxyClientNeverBypassesProxy 走代理的客户端忽略 base 的 Proxy 函数、DialTLSContext 与环境变量代理，
// 所有连接只经 SOCKS5：这些绕过路径一旦被调用即视为失败。
func TestProxyClientNeverBypassesProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	var proxyFuncCalls, dialTLSCalls atomic.Int32
	dial := &dialRecorder{}
	transport := dial.transport()
	// Proxy 函数不依赖进程内缓存的环境变量，保证“被忽略”的断言不会空转。
	transport.Proxy = func(*http.Request) (*url.URL, error) {
		proxyFuncCalls.Add(1)
		return url.Parse("http://127.0.0.1:1")
	}
	transport.DialTLSContext = func(_ context.Context, _, addr string) (net.Conn, error) {
		dialTLSCalls.Add(1)
		return nil, errors.New("测试：不允许直连 TLS " + addr)
	}

	target := proxyPingServer(t)
	socks := newSocksServer(t, target.Listener.Addr().String())
	client, err := proxyClient(&http.Client{Transport: transport}, socks.url())
	if err != nil {
		t.Fatal(err)
	}
	if body, err := proxyGet(client, "https://api.github.com/ping"); err != nil || body != "pong" {
		t.Fatalf("请求 = %q, %v; want pong", body, err)
	}
	if proxyFuncCalls.Load() != 0 || dialTLSCalls.Load() != 0 || dial.callCount() != 0 {
		t.Fatalf("绕过路径被调用：Proxy=%d DialTLSContext=%d DialContext=%d, want 全为 0",
			proxyFuncCalls.Load(), dialTLSCalls.Load(), dial.callCount())
	}
	if socks.connectCount() != 1 {
		t.Fatalf("代理 CONNECT 次数 = %d, want 1", socks.connectCount())
	}
}

// TestProxyClientUnreachableProxy 代理不可达时请求失败，错误为“代理 <host:port> 连接失败: ...”，不含密码，也不回退直连。
func TestProxyClientUnreachableProxy(t *testing.T) {
	addr := deadProxyAddr(t)
	dial := &dialRecorder{}
	client, err := proxyClient(dial.client(), socksURL(addr, "proxy-user", proxyTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	_, err = proxyGet(client, "https://api.github.com/ping")
	if err == nil {
		t.Fatal("代理不可达时请求必须失败")
	}
	if want := "代理 " + addr + " 连接失败"; !strings.Contains(err.Error(), want) {
		t.Fatalf("错误 %q 应包含 %q", err, want)
	}
	for _, leaked := range []string{"proxy-user", proxyTestSecret} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("错误信息泄露了 %q: %v", leaked, err)
		}
	}
	if dial.callCount() != 0 {
		t.Fatalf("代理失败后回退直连 %d 次, want 0", dial.callCount())
	}
}

// TestProxyClientEmptyProxyEqualsSafeClient proxy 为空时等同 safeClient：保留 base 的 Transport（可以是任意 RoundTripper），
// 禁止重定向、去掉 Jar，且不修改 base。
func TestProxyClientEmptyProxyEqualsSafeClient(t *testing.T) {
	var used atomic.Bool
	base := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		used.Store(true)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	}), CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}

	client, err := proxyClient(base, "")
	if err != nil {
		t.Fatalf("proxy 为空时 base 可以是任意 Transport: %v", err)
	}
	if _, err := client.Get("https://api.github.com/x"); err != nil || !used.Load() {
		t.Fatalf("应使用 base 的 Transport: used=%v err=%v", used.Load(), err)
	}
	if client.CheckRedirect(nil, nil) != http.ErrUseLastResponse || client.Jar != nil {
		t.Fatal("返回的客户端必须禁止重定向且无 Jar")
	}
	if base.CheckRedirect(nil, nil) != nil {
		t.Fatal("不得修改传入的 base 客户端")
	}
	if got, err := proxyClient(nil, ""); err != nil || got == nil {
		t.Fatalf("proxyClient(nil, \"\") = %v, %v; want 默认客户端", got, err)
	}
}

// TestProxyClientRejectsNonHTTPTransport 配置了代理而 base.Transport 不是 *http.Transport 时返回错误（无法保证不绕过代理）。
func TestProxyClientRejectsNonHTTPTransport(t *testing.T) {
	base := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("不应被调用")
	})}
	if client, err := proxyClient(base, "socks5://127.0.0.1:1080"); err == nil {
		t.Fatalf("proxyClient() = %v, want 错误", client)
	}
}

// TestProxyClientBuildsFromDefaultAndDoesNotMutateBase base 为 nil 时以默认 Transport 的克隆为底；
// 结果的 Proxy 与 DialTLSContext 为空、DialContext 为 SOCKS5 dialer，同样禁止重定向、无 Jar；传入的 base 不被修改。
func TestProxyClientBuildsFromDefaultAndDoesNotMutateBase(t *testing.T) {
	client, err := proxyClient(nil, "socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.DialTLSContext != nil || transport.DialContext == nil {
		t.Fatalf("Transport = %#v, want Proxy 与 DialTLSContext 为空、DialContext 非空的 *http.Transport", client.Transport)
	}
	if client.CheckRedirect(nil, nil) != http.ErrUseLastResponse || client.Jar != nil {
		t.Fatal("返回的客户端必须禁止重定向且无 Jar")
	}

	dial := &dialRecorder{}
	baseTransport := dial.transport()
	baseTransport.Proxy = http.ProxyFromEnvironment
	if _, err := proxyClient(&http.Client{Transport: baseTransport}, "socks5://127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	if baseTransport.Proxy == nil || baseTransport.DialContext == nil {
		t.Fatal("不得修改传入的 base Transport")
	}
}

// TestProxyClientRejectsInvalidProxy 非法代理地址返回“代理地址无效”，错误不含密码。
func TestProxyClientRejectsInvalidProxy(t *testing.T) {
	_, err := proxyClient(nil, "http://u:"+proxyTestSecret+"@127.0.0.1:1080")
	if err == nil || !strings.Contains(err.Error(), "代理地址无效") || strings.Contains(err.Error(), proxyTestSecret) {
		t.Fatalf("proxyClient(非法地址) 错误 = %v, want 代理地址无效且不含密码", err)
	}
}
