package copilot

// 管理接口中的账号代理（见 docs/proxy.md「管理接口」）：账号视图、导入、设备登录与 PATCH 修改代理。

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// proxyJSON 把值编码为 JSON 文本，用作请求体。
func proxyJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// patchAccount 以 fields 为请求体发送 PATCH /v0/management/accounts。
func patchAccount(t *testing.T, p *Pool, fields map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return mgmtCall(p, "PATCH", "/v0/management/accounts", proxyJSON(t, fields), mgmtKey)
}

// proxyUserInfo 解析代理 URL，返回主机 host:port、用户名、密码，便于不依赖字符串格式地比较。
func proxyUserInfo(t *testing.T, raw string) (hostPort, user, pass string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析代理地址 %q: %v", raw, err)
	}
	pass, _ = u.User.Password()
	return u.Host, u.User.Username(), pass
}

// splitHostPort 把 host:port 拆成主机与端口号。
func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return host, n
}

// assertViewProxy 断言账号视图的代理为 host:port（无认证），不带用户名与密码。
func assertViewProxy(t *testing.T, view AccountView, addr string) {
	t.Helper()
	host, port := splitHostPort(t, addr)
	if view.Proxy == nil || view.Proxy.Host != host || view.Proxy.Port != port || view.Proxy.Scheme != "socks5" {
		t.Fatalf("账号视图的代理 = %+v, want socks5://%s", view.Proxy, addr)
	}
}

// TestAccountViewProxyField 账号视图的 proxy 对象：含 scheme、host（IPv6 不带方括号）、port、username（为空时省略）与
// has_password（始终输出），绝不含密码；没有代理时整个 proxy 字段省略。
func TestAccountViewProxyField(t *testing.T) {
	env := newProxyEnv(t)
	dead := deadProxyAddr(t)
	deadHost, deadPort := splitHostPort(t, dead)
	p, _ := env.newPool(
		proxyAccount("alice", "socks5h://alice-proxy:"+proxyTestSecret+"@"+dead),
		proxyAccount("bob", "socks5://[::1]:1081"),
		proxyAccount("carol", ""),
	)

	w := mgmtCall(p, "GET", "/v0/management/accounts", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("GET accounts status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), proxyTestSecret) {
		t.Fatalf("账号列表泄露了代理密码: %s", w.Body.String())
	}
	var raw []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	keysOf := func(m map[string]any) []string {
		keys := make([]string, 0, len(m))
		for key := range m {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys
	}
	proxyOf := map[string]map[string]any{}
	for _, item := range raw {
		id := item["id"].(string)
		if object, ok := item["proxy"].(map[string]any); ok {
			proxyOf[id] = object
		} else if _, present := item["proxy"]; present {
			t.Fatalf("账号 %s 的 proxy 应为对象: %v", id, item["proxy"])
		}
	}
	if _, present := proxyOf["carol"]; present {
		t.Fatalf("无代理的账号不应输出 proxy: %v", proxyOf["carol"])
	}
	if got, want := keysOf(proxyOf["alice"]), []string{"has_password", "host", "port", "scheme", "username"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("alice 的 proxy 字段 = %v, want %v", got, want)
	}
	if got, want := keysOf(proxyOf["bob"]), []string{"has_password", "host", "port", "scheme"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bob 的 proxy 字段 = %v, want %v（username 为空时省略）", got, want)
	}

	views := mgmtAccounts(t, p)
	alice := mgmtAccountByID(t, views, "alice").Proxy
	if alice == nil || alice.Scheme != "socks5h" || alice.Host != deadHost || alice.Port != deadPort || alice.Username != "alice-proxy" || !alice.HasPassword {
		t.Fatalf("alice 的代理 = %+v", alice)
	}
	bob := mgmtAccountByID(t, views, "bob").Proxy
	if bob == nil || bob.Scheme != "socks5" || bob.Host != "::1" || bob.Port != 1081 || bob.Username != "" || bob.HasPassword {
		t.Fatalf("bob 的代理 = %+v", bob)
	}
}

// TestImportAccountWithProxyVerifiesAndSavesThroughProxy 导入带 proxy 的凭证：token 校验通过该代理进行（没有直连），
// 保存的凭证文件含 proxy，响应不含密码，新账号之后的请求也走该代理。
func TestImportAccountWithProxyVerifiesAndSavesThroughProxy(t *testing.T) {
	env := newProxyEnv(t)
	socks := env.newSocks("proxy-user", proxyTestSecret)
	proxy := socks.url("proxy-user", proxyTestSecret)
	p, dir := env.newPool()
	env.up.setLogin("oauth-newbie", "newbie")

	body := proxyJSON(t, map[string]any{"credential": map[string]any{"oauth_token": "oauth-newbie", "proxy": proxy}})
	w := mgmtCall(p, "POST", "/v0/management/accounts", body, mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("导入 status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), proxyTestSecret) {
		t.Fatalf("导入响应泄露了代理密码: %s", w.Body.String())
	}
	var view AccountView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	host, port := splitHostPort(t, socks.addr())
	if view.ID != "newbie" || view.Proxy == nil || view.Proxy.Host != host || view.Proxy.Port != port ||
		view.Proxy.Username != "proxy-user" || !view.Proxy.HasPassword {
		t.Fatalf("导入后的账号视图 = %+v", view)
	}
	if !socks.hasTarget("api.github.com:443") {
		t.Fatalf("token 校验应经过代理: %v", socks.connectTargets())
	}
	if got := loadCredentialOf(t, dir, "newbie").Proxy; got != proxy {
		t.Fatalf("凭证文件中的 proxy = %q, want %q", got, proxy)
	}

	if w := proxyChat(p); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oauth-newbie") {
		t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
	}
	if !socks.hasTarget("api.githubcopilot.com:443") {
		t.Fatalf("新账号的生成请求应经过代理: %v", socks.connectTargets())
	}
	if n := env.dial.callCount(); n != 0 {
		t.Fatalf("直连被调用 %d 次: %v, want 0", n, env.dial.dialedAddrs())
	}
}

// TestImportAccountRejectsBadProxy 代理格式非法返回 400（不访问上游）；代理不可达时校验失败同样返回 400；
// 两种情况都不写凭证文件，错误信息不含密码。
func TestImportAccountRejectsBadProxy(t *testing.T) {
	dead := deadProxyAddr(t)
	for _, tc := range []struct{ name, proxy, want string }{
		{"格式非法", "http://proxy-user:" + proxyTestSecret + "@127.0.0.1:1080", "代理地址无效"},
		{"代理不可达", socksURL(dead, "proxy-user", proxyTestSecret), "代理 " + dead + " 连接失败"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newProxyEnv(t)
			p, dir := env.newPool()
			env.up.setLogin("oauth-newbie", "newbie")

			body := proxyJSON(t, map[string]any{"credential": map[string]any{"oauth_token": "oauth-newbie", "proxy": tc.proxy}})
			w := mgmtCall(p, "POST", "/v0/management/accounts", body, mgmtKey)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("导入 status=%d body=%s, want 400 且含 %q", w.Code, w.Body.String(), tc.want)
			}
			for _, leaked := range []string{"proxy-user", proxyTestSecret} {
				if strings.Contains(w.Body.String(), leaked) {
					t.Fatalf("响应泄露了 %q: %s", leaked, w.Body.String())
				}
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("校验失败不应写凭证文件: %v", entries)
			}
			if n := env.dial.callCount(); n != 0 {
				t.Fatalf("直连被调用 %d 次, want 0", n)
			}
		})
	}
}

// TestLoginWithProxyUsesProxyForDeviceFlowAndSavesIt 带 proxy 的设备登录：申请 device code、轮询 token、查询 /user 都走该代理
// （没有直连），成功保存的凭证含 proxy。
func TestLoginWithProxyUsesProxyForDeviceFlowAndSavesIt(t *testing.T) {
	env := newProxyEnv(t)
	socks := env.newSocks("proxy-user", proxyTestSecret)
	proxy := socks.url("proxy-user", proxyTestSecret)
	p, dir := env.newPool()
	env.up.setDeviceLogin("oauth-dev", "devuser")

	w := mgmtCall(p, "POST", "/v0/management/login", proxyJSON(t, map[string]any{"proxy": proxy}), mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", w.Code, w.Body.String())
	}
	var start mgmtLoginStart
	if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil || start.State == "" {
		t.Fatalf("login 响应 = %s, err=%v", w.Body.String(), err)
	}
	if !socks.hasTarget("github.com:443") {
		t.Fatalf("申请 device code 应经过代理: %v", socks.connectTargets())
	}
	result := mgmtPollLogin(t, p, start.State)
	if result.Status != "success" || result.ID != "devuser" {
		t.Fatalf("登录结果 = %+v, want success/devuser", result)
	}
	if !socks.hasTarget("api.github.com:443") {
		t.Fatalf("查询 /user 应经过代理: %v", socks.connectTargets())
	}
	if n := env.dial.callCount(); n != 0 {
		t.Fatalf("直连被调用 %d 次: %v, want 0", n, env.dial.dialedAddrs())
	}
	if got := loadCredentialOf(t, dir, "devuser").Proxy; got != proxy {
		t.Fatalf("凭证文件中的 proxy = %q, want %q", got, proxy)
	}
	if view := mgmtAccountByID(t, mgmtAccounts(t, p), "devuser"); view.Proxy == nil || !view.Proxy.HasPassword {
		t.Fatalf("登录后的账号视图 = %+v, want 带代理", view)
	}
}

// TestLoginWithoutBodyStillWorks 请求体为空的 POST login 仍可用（直连），保存的凭证不含 proxy。
func TestLoginWithoutBodyStillWorks(t *testing.T) {
	env := newProxyEnv(t)
	env.allowDirect()
	p, dir := env.newPool()
	env.up.setDeviceLogin("oauth-dev", "devuser")

	w := mgmtCall(p, "POST", "/v0/management/login", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", w.Code, w.Body.String())
	}
	var start mgmtLoginStart
	if err := json.Unmarshal(w.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if result := mgmtPollLogin(t, p, start.State); result.Status != "success" || result.ID != "devuser" {
		t.Fatalf("登录结果 = %+v, want success/devuser", result)
	}
	if env.dial.callCount() == 0 {
		t.Fatal("没有代理时应直连")
	}
	if got := loadCredentialOf(t, dir, "devuser").Proxy; got != "" {
		t.Fatalf("凭证文件中的 proxy = %q, want 空", got)
	}
}

// TestLoginRejectsBadProxy 登录时代理格式非法返回 400，不发起任何连接；代理不可达时申请 device code 失败返回 502，
// 错误含代理失败原因；两者的响应都不含密码。
func TestLoginRejectsBadProxy(t *testing.T) {
	dead := deadProxyAddr(t)
	for _, tc := range []struct {
		name, proxy, want string
		status            int
	}{
		{"格式非法", "http://proxy-user:" + proxyTestSecret + "@127.0.0.1:1080", "代理地址无效", http.StatusBadRequest},
		{"代理不可达", socksURL(dead, "proxy-user", proxyTestSecret), "代理 " + dead + " 连接失败", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newProxyEnv(t)
			p, _ := env.newPool()
			w := mgmtCall(p, "POST", "/v0/management/login", proxyJSON(t, map[string]any{"proxy": tc.proxy}), mgmtKey)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("login status=%d body=%s, want %d 且含 %q", w.Code, w.Body.String(), tc.status, tc.want)
			}
			for _, leaked := range []string{"proxy-user", proxyTestSecret} {
				if strings.Contains(w.Body.String(), leaked) {
					t.Fatalf("响应泄露了 %q: %s", leaked, w.Body.String())
				}
			}
			if n := env.dial.callCount(); n != 0 {
				t.Fatalf("直连被调用 %d 次, want 0", n)
			}
		})
	}
}

// TestPatchSetProxyVerifiesThroughNewProxy 给无代理的账号设置代理：先通过新代理用账号 token 请求 /user 验证，
// 通过后写入凭证文件并更新账号视图，之后的请求走新代理。
func TestPatchSetProxyVerifiesThroughNewProxy(t *testing.T) {
	env := newProxyEnv(t)
	next := env.newSocks()
	p, dir := env.newPool(proxyAccount("alice", ""))

	w := patchAccount(t, p, map[string]any{"id": "alice", "proxy": next.url()})
	view := cooldownDecodeView(t, w)
	assertViewProxy(t, view, next.addr())
	if view.Proxy.HasPassword || view.Proxy.Username != "" {
		t.Fatalf("新代理无认证, 视图 = %+v", view.Proxy)
	}
	if !next.hasTarget("api.github.com:443") {
		t.Fatalf("验证应通过新代理访问 api.github.com: %v", next.connectTargets())
	}
	if got := loadCredentialOf(t, dir, "alice").Proxy; got != next.url() {
		t.Fatalf("凭证文件中的 proxy = %q, want %q", got, next.url())
	}

	if w := proxyChat(p); w.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
	}
	if !next.hasTarget("api.githubcopilot.com:443") {
		t.Fatalf("修改后的生成请求应走新代理: %v", next.connectTargets())
	}
	if n := env.dial.callCount(); n != 0 {
		t.Fatalf("直连被调用 %d 次, want 0", n)
	}
}

// TestPatchChangeProxyRoutesNewRequestsAndKeepsSession 修改代理后新发起的请求走新代理、旧代理不再收到连接；
// 旧客户端的空闲连接被关闭；session 缓存不清除（不重新交换 session）。
func TestPatchChangeProxyRoutesNewRequestsAndKeepsSession(t *testing.T) {
	env := newProxyEnv(t)
	old, next := env.newSocks(), env.newSocks()
	p, _ := env.newPool(proxyAccount("alice", old.url()))

	if w := proxyChat(p); w.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
	}
	oldConnects := old.connectCount()
	if oldConnects == 0 || env.up.pool.sessionCount("oauth-alice") != 1 {
		t.Fatalf("前提不成立: 旧代理 CONNECT=%d session 交换=%d", oldConnects, env.up.pool.sessionCount("oauth-alice"))
	}

	cooldownDecodeView(t, patchAccount(t, p, map[string]any{"id": "alice", "proxy": next.url()}))
	if w := proxyChat(p); w.Code != http.StatusOK {
		t.Fatalf("修改后 chat status=%d body=%s", w.Code, w.Body.String())
	}
	if !next.hasTarget("api.githubcopilot.com:443") {
		t.Fatalf("修改后的生成请求应走新代理: %v", next.connectTargets())
	}
	if old.connectCount() != oldConnects {
		t.Fatalf("旧代理在修改后又收到连接: %d -> %d", oldConnects, old.connectCount())
	}
	if n := env.up.pool.sessionCount("oauth-alice"); n != 1 {
		t.Fatalf("session 交换次数 = %d, want 1（修改代理不清除 session 缓存）", n)
	}
	cooldownEventually(t, "旧代理的空闲连接被关闭", func() bool { return old.activeConns() == 0 })
	if n := env.dial.callCount(); n != 0 {
		t.Fatalf("直连被调用 %d 次, want 0", n)
	}
}

// TestPatchProxyInFlightRequestFinishesOnOldProxy 修改代理时进行中的请求继续在旧连接上完成（旧连接不被关闭）。
func TestPatchProxyInFlightRequestFinishesOnOldProxy(t *testing.T) {
	env := newProxyEnv(t)
	old, next := env.newSocks(), env.newSocks()
	p, _ := env.newPool(proxyAccount("alice", old.url()))
	hold := env.up.holdGenerations("oauth-alice")

	done := quotaGenerateAsync(p, context.Background())
	select {
	case <-hold.started:
	case <-time.After(5 * time.Second):
		t.Fatal("生成请求没有到达上游")
	}
	cooldownDecodeView(t, patchAccount(t, p, map[string]any{"id": "alice", "proxy": next.url()}))
	if old.activeConns() == 0 {
		t.Fatal("进行中的请求所在的旧连接不应被关闭")
	}
	hold.free()
	if w := quotaAwaitResponse(t, done); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "oauth-alice") {
		t.Fatalf("进行中的请求 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
}

// TestPatchProxyVerificationFailureChangesNothing 新代理验证失败返回 502，错误含代理失败原因且不含密码；
// 凭证文件与内存都不变，账号继续使用旧代理。
func TestPatchProxyVerificationFailureChangesNothing(t *testing.T) {
	env := newProxyEnv(t)
	old := env.newSocks()
	dead := deadProxyAddr(t)
	p, dir := env.newPool(proxyAccount("alice", old.url()))
	before := readCredentialFile(t, dir, "alice")

	w := patchAccount(t, p, map[string]any{"id": "alice", "proxy": socksURL(dead, "proxy-user", proxyTestSecret)})
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "代理 "+dead+" 连接失败") {
		t.Fatalf("PATCH status=%d body=%s, want 502 且含代理失败原因", w.Code, w.Body.String())
	}
	for _, leaked := range []string{"proxy-user", proxyTestSecret} {
		if strings.Contains(w.Body.String(), leaked) {
			t.Fatalf("响应泄露了 %q: %s", leaked, w.Body.String())
		}
	}
	if after := readCredentialFile(t, dir, "alice"); string(after) != string(before) {
		t.Fatalf("验证失败后凭证文件被修改:\n%s\n->\n%s", before, after)
	}
	assertViewProxy(t, cooldownView(t, p, "alice"), old.addr())
	if w := proxyChat(p); w.Code != http.StatusOK || !old.hasTarget("api.githubcopilot.com:443") {
		t.Fatalf("验证失败后账号应继续使用旧代理: status=%d targets=%v", w.Code, old.connectTargets())
	}
}

// TestPatchProxyLoginMismatchIsBadRequest 新代理返回的登录名与账号 ID 不一致时返回 400，凭证与内存都不变；
// 比较忽略大小写。
func TestPatchProxyLoginMismatchIsBadRequest(t *testing.T) {
	env := newProxyEnv(t)
	next := env.newSocks()
	p, dir := env.newPool(proxyAccount("alice", ""))
	before := readCredentialFile(t, dir, "alice")

	env.up.setLogin("oauth-alice", "someone-else")
	if w := patchAccount(t, p, map[string]any{"id": "alice", "proxy": next.url()}); w.Code != http.StatusBadRequest {
		t.Fatalf("登录名不一致 status=%d body=%s, want 400", w.Code, w.Body.String())
	}
	if after := readCredentialFile(t, dir, "alice"); string(after) != string(before) {
		t.Fatalf("400 后凭证文件被修改: %s", after)
	}
	if view := cooldownView(t, p, "alice"); view.Proxy != nil {
		t.Fatalf("400 后账号仍应无代理: %+v", view.Proxy)
	}

	env.up.setLogin("oauth-alice", "ALICE")
	cooldownDecodeView(t, patchAccount(t, p, map[string]any{"id": "alice", "proxy": next.url()}))
}

// TestPatchProxyKeepPassword proxy_keep_password 的规则：为 true、新 URL 不含密码，且主机、端口、用户名都与当前代理相同
// （主机与 scheme 忽略大小写）时沿用当前密码（当前无密码则仍无密码）；新 URL 自带密码时以新 URL 为准；默认（false）不沿用。
// 验证通过新代理进行，新代理要求的凭据即实际使用的凭据；结果相同（未变化）时不验证，保存的密码仍应是沿用的旧密码。
func TestPatchProxyKeepPassword(t *testing.T) {
	for _, tc := range []struct {
		name         string
		current      func(next *socksServer) string // 当前代理
		patch        func(next *socksServer) string // PATCH 的新 URL（不含密码，除非该用例要测新密码）
		serverAuth   []string                       // 新代理要求的 用户名、密码；为空表示无认证
		keep         bool
		wantPassword string
	}{
		{"keep 为 true 且主机、端口、用户名相同：沿用当前密码",
			func(n *socksServer) string { return n.url("pu", "oldpw") },
			func(n *socksServer) string { return n.url("pu") },
			[]string{"pu", "oldpw"}, true, "oldpw"},
		{"keep 为 true 且主机与 scheme 仅大小写不同：沿用当前密码",
			func(n *socksServer) string { return "socks5://pu:oldpw@localhost:" + portOf(n) },
			func(n *socksServer) string { return "SOCKS5://pu@LOCALHOST:" + portOf(n) },
			[]string{"pu", "oldpw"}, true, "oldpw"},
		{"keep 为 true 但新 URL 自带密码：以新 URL 为准",
			func(*socksServer) string { return "socks5://pu:oldpw@127.0.0.1:1" },
			func(n *socksServer) string { return n.url("pu", "newpw") },
			[]string{"pu", "newpw"}, true, "newpw"},
		{"keep 为 false：不沿用当前密码",
			func(n *socksServer) string { return n.url("pu", "oldpw") },
			func(n *socksServer) string { return n.url("pu") },
			nil, false, ""},
		{"keep 为 true 但当前无密码：仍无密码",
			func(n *socksServer) string { return n.url("pu") },
			func(n *socksServer) string { return n.url("pu") },
			nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newProxyEnv(t)
			next := env.newSocks(tc.serverAuth...)
			p, dir := env.newPool(proxyAccount("alice", tc.current(next)))

			fields := map[string]any{"id": "alice", "proxy": tc.patch(next)}
			if tc.keep {
				fields["proxy_keep_password"] = true
			}
			view := cooldownDecodeView(t, patchAccount(t, p, fields))
			if view.Proxy == nil || view.Proxy.Username != "pu" || view.Proxy.HasPassword != (tc.wantPassword != "") {
				t.Fatalf("账号视图的代理 = %+v, want 用户名 pu, has_password=%v", view.Proxy, tc.wantPassword != "")
			}
			_, user, pass := proxyUserInfo(t, loadCredentialOf(t, dir, "alice").Proxy)
			if user != "pu" || pass != tc.wantPassword {
				t.Fatalf("保存的代理 user=%s password=%q, want pu %q", user, pass, tc.wantPassword)
			}
		})
	}
}

// portOf 返回 SOCKS5 测试服务器的端口文本。
func portOf(s *socksServer) string {
	_, port, _ := net.SplitHostPort(s.addr())
	return port
}

// TestPatchProxyKeepPasswordNotCarriedToDifferentEndpoint proxy_keep_password 为 true 且新 URL 不含密码时，
// 主机、端口、用户名任一与当前代理不同就不沿用旧密码（避免把旧密码发给另一台代理）：
// 新代理要求 pu/oldpw，实际收到的认证只能是无密码的，验证因此失败返回 502，凭证文件不变，响应不含旧密码。
func TestPatchProxyKeepPasswordNotCarriedToDifferentEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current func(next *socksServer) string // 与新代理（next）在主机、端口或用户名上不同的当前代理
	}{
		{"端口不同", func(*socksServer) string { return socksURL(deadProxyAddr(t), "pu", "oldpw") }},
		{"主机不同", func(n *socksServer) string { return "socks5://pu:oldpw@localhost:" + portOf(n) }},
		{"用户名不同", func(n *socksServer) string { return n.url("other", "oldpw") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newProxyEnv(t)
			next := env.newSocks("pu", "oldpw")
			p, dir := env.newPool(proxyAccount("alice", tc.current(next)))
			before := readCredentialFile(t, dir, "alice")

			w := patchAccount(t, p, map[string]any{"id": "alice", "proxy": next.url("pu"), "proxy_keep_password": true})
			if w.Code != http.StatusBadGateway {
				t.Fatalf("PATCH status=%d body=%s, want 502（旧密码不应被发给不同的代理）", w.Code, w.Body.String())
			}
			attempts := next.authAttempts()
			if len(attempts) == 0 {
				t.Fatal("前提不成立：新代理应收到认证请求")
			}
			for _, attempt := range attempts {
				if attempt[1] != "" {
					t.Fatalf("新代理收到了密码 %q, want 无密码认证", attempt[1])
				}
			}
			if strings.Contains(w.Body.String(), "oldpw") {
				t.Fatalf("响应泄露了旧密码: %s", w.Body.String())
			}
			if after := readCredentialFile(t, dir, "alice"); string(after) != string(before) {
				t.Fatalf("验证失败后凭证文件被修改: %s", after)
			}
		})
	}
}

// TestPatchDeleteProxySkipsVerification proxy 为空串表示删除代理，不做验证（即使原代理不可达也成功）：
// 凭证文件不再有 proxy，账号视图省略 proxy，之后的请求直连。
func TestPatchDeleteProxySkipsVerification(t *testing.T) {
	env := newProxyEnv(t)
	dead := deadProxyAddr(t)
	p, dir := env.newPool(proxyAccount("alice", socksURL(dead, "proxy-user", proxyTestSecret)))

	w := patchAccount(t, p, map[string]any{"id": "alice", "proxy": ""})
	view := cooldownDecodeView(t, w)
	if view.Proxy != nil || strings.Contains(w.Body.String(), `"proxy"`) {
		t.Fatalf("删除后账号视图不应含 proxy: %s", w.Body.String())
	}
	if data := readCredentialFile(t, dir, "alice"); strings.Contains(string(data), "proxy") {
		t.Fatalf("删除后凭证文件不应含 proxy: %s", data)
	}
	if n := env.dial.callCount(); n != 0 {
		t.Fatalf("删除代理不应发起任何连接, 直连 %d 次", n)
	}

	env.allowDirect()
	if w := proxyChat(p); w.Code != http.StatusOK {
		t.Fatalf("删除代理后 chat status=%d body=%s", w.Code, w.Body.String())
	}
	if env.dial.callCount() == 0 {
		t.Fatal("删除代理后应直连")
	}
}

// TestPatchSameProxyDoesNotVerifyOrWrite 新代理与当前值相同（包括 proxy_keep_password 展开后相同）视为未变化：
// 不验证（原代理不可达也成功）、不写凭证文件（文件被替换为目录也返回 200）。
func TestPatchSameProxyDoesNotVerifyOrWrite(t *testing.T) {
	env := newProxyEnv(t)
	dead := deadProxyAddr(t)
	current := socksURL(dead, "pu", "oldpw")
	p, dir := env.newPool(proxyAccount("alice", current))
	replaceCredentialWithDir(t, dir, "alice")

	for _, fields := range []map[string]any{
		{"id": "alice", "proxy": current},
		{"id": "alice", "proxy": socksURL(dead, "pu"), "proxy_keep_password": true},
	} {
		view := cooldownDecodeView(t, patchAccount(t, p, fields))
		if view.Proxy == nil || view.Proxy.Username != "pu" || !view.Proxy.HasPassword {
			t.Fatalf("未变化的 PATCH 后账号视图 = %+v", view.Proxy)
		}
	}
	if n := env.dial.callCount(); n != 0 {
		t.Fatalf("直连被调用 %d 次, want 0", n)
	}
}

// TestPatchProxyCombinesWithDisabledAndPriority proxy 可与 disabled、priority 一起提供，凭证文件一次写入全部字段；
// 之后只改 disabled 时凭证中的 proxy 保持不变。
func TestPatchProxyCombinesWithDisabledAndPriority(t *testing.T) {
	env := newProxyEnv(t)
	next := env.newSocks()
	p, dir := env.newPool(proxyAccount("alice", ""))

	view := cooldownDecodeView(t, patchAccount(t, p, map[string]any{
		"id": "alice", "proxy": next.url(), "disabled": true, "priority": 7,
	}))
	assertViewProxy(t, view, next.addr())
	if !view.Disabled || view.Priority != 7 {
		t.Fatalf("账号视图 = %+v, want 已禁用且优先级 7", view)
	}
	cred := loadCredentialOf(t, dir, "alice")
	if cred.Proxy != next.url() || !cred.Disabled || cred.Priority != 7 {
		t.Fatalf("凭证 = %+v, want 同时含 proxy、disabled 与 priority", cred)
	}

	cooldownDecodeView(t, patchAccount(t, p, map[string]any{"id": "alice", "disabled": false}))
	if cred := loadCredentialOf(t, dir, "alice"); cred.Proxy != next.url() || cred.Disabled || cred.Priority != 7 {
		t.Fatalf("只改 disabled 后凭证 = %+v, want proxy 与 priority 保持不变", cred)
	}
}

// TestPatchProxyDoesNotClearCooldown 修改代理不自动解除冷却；与 clear_cooldown 一起提供时在修改代理之后解除。
func TestPatchProxyDoesNotClearCooldown(t *testing.T) {
	env := newProxyEnv(t)
	first, second, third := env.newSocks(), env.newSocks(), env.newSocks()
	env.up.pool.failGeneration("oauth-alice", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p, _ := env.newPool(proxyAccount("alice", first.url()))
	proxyChat(p)
	cooldownAwaitRecord(t, p, "alice")

	view := cooldownDecodeView(t, patchAccount(t, p, map[string]any{"id": "alice", "proxy": second.url()}))
	assertViewProxy(t, view, second.addr())
	if view.CooldownUntil == nil || view.CooldownReason != "401" {
		t.Fatalf("只修改代理后冷却 = %v/%q, want 保持 401 冷却", view.CooldownUntil, view.CooldownReason)
	}

	view = cooldownDecodeView(t, patchAccount(t, p, map[string]any{"id": "alice", "proxy": third.url(), "clear_cooldown": true}))
	assertViewProxy(t, view, third.addr())
	if view.CooldownUntil != nil {
		t.Fatalf("clear_cooldown 后仍冷却至 %s", view.CooldownUntil)
	}
}

// TestPatchProxyWriteFailureChangesNothing 凭证写入失败（文件被目录替换）时返回 500，内存中的代理与 HTTP 客户端都不变：
// 修复凭证后账号仍使用旧代理，再次修改才生效。
func TestPatchProxyWriteFailureChangesNothing(t *testing.T) {
	env := newProxyEnv(t)
	old, next := env.newSocks(), env.newSocks()
	p, dir := env.newPool(proxyAccount("alice", old.url()))
	path := replaceCredentialWithDir(t, dir, "alice")

	if w := patchAccount(t, p, map[string]any{"id": "alice", "proxy": next.url()}); w.Code != http.StatusInternalServerError {
		t.Fatalf("写入失败 status=%d body=%s, want 500", w.Code, w.Body.String())
	}
	assertViewProxy(t, cooldownView(t, p, "alice"), old.addr())

	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := SaveCredential(path, Credential{Token: "oauth-alice", User: "alice", Proxy: old.url()}); err != nil {
		t.Fatal(err)
	}
	if w := proxyChat(p); w.Code != http.StatusOK || !old.hasTarget("api.githubcopilot.com:443") || next.hasTarget("api.githubcopilot.com:443") {
		t.Fatalf("写入失败后账号应继续使用旧代理: status=%d 旧=%v 新=%v", w.Code, old.connectTargets(), next.connectTargets())
	}
	assertViewProxy(t, cooldownDecodeView(t, patchAccount(t, p, map[string]any{"id": "alice", "proxy": next.url()})), next.addr())
}

// TestPatchInvalidProxyIsBadRequest 代理格式非法返回 400（错误不含密码），不验证、不改任何状态；账号不存在返回 404。
func TestPatchInvalidProxyIsBadRequest(t *testing.T) {
	env := newProxyEnv(t)
	old := env.newSocks()
	p, dir := env.newPool(proxyAccount("alice", old.url()))
	before := readCredentialFile(t, dir, "alice")
	connects := old.connectCount()

	w := patchAccount(t, p, map[string]any{"id": "alice", "proxy": "http://pu:" + proxyTestSecret + "@127.0.0.1:1080"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "代理地址无效") || strings.Contains(w.Body.String(), proxyTestSecret) {
		t.Fatalf("PATCH status=%d body=%s, want 400、代理地址无效且不含密码", w.Code, w.Body.String())
	}
	if after := readCredentialFile(t, dir, "alice"); string(after) != string(before) {
		t.Fatalf("400 后凭证文件被修改: %s", after)
	}
	assertViewProxy(t, cooldownView(t, p, "alice"), old.addr())
	if old.connectCount() != connects || env.dial.callCount() != 0 {
		t.Fatalf("400 不应发起任何连接")
	}

	if w := patchAccount(t, p, map[string]any{"id": "nobody", "proxy": old.url()}); w.Code != http.StatusNotFound {
		t.Fatalf("账号不存在 status=%d, want 404", w.Code)
	}
}
