package copilot

// 账号代理（见 docs/proxy.md）测试的公共夹具。
//
// 本组测试（proxy_*_test.go）依赖实现提供的新符号，这些符号即接口约定：
//   - func proxyClient(base *http.Client, proxy string) (*http.Client, error)（proxy.go）。
//   - func normalizeProxy(raw string) (string, error)：去除首尾空白后校验代理 URL，返回去除空白后的字符串（scheme 保持原样），
//     空串返回 ""；不合法时错误以 "代理地址无效" 开头，且不含密码。LoadCredential、SaveCredential、管理接口、New 共用它。
//   - Credential.Proxy string（json:"proxy,omitempty"）、Config.Proxy string、PoolAccount.Proxy string。
//   - AccountView.Proxy *ProxyView（json:"proxy,omitempty"）；ProxyView 含 Scheme、Host（IPv6 不带方括号）、Port int、
//     Username（omitempty）与 HasPassword bool（json:"has_password"，始终输出），没有密码字段。
//   - 管理接口 PATCH 的 proxy（字符串）与 proxy_keep_password（布尔），POST login 的可选请求体 {"proxy": "..."}，
//     POST accounts 的 credential.proxy。

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// proxyTestSecret 是测试用代理密码，用于断言任何错误、日志与响应都不含它。
const proxyTestSecret = "pwZq9x7hidden"

// socksServer 是测试用的最小 SOCKS5 服务器：支持无认证与 RFC 1929 用户名/密码认证，只支持 CONNECT。
// 记录每次 CONNECT 请求的目标地址（域名原样记录，不解析），并把所有连接转发到固定的 forward 地址（测试用 TLS 服务器），
// 因此客户端请求 api.github.com:443 时实际连到本地假上游，同时能证明域名是交给代理端解析的。
type socksServer struct {
	listener net.Listener
	forward  string
	user     string // 非空时要求用户名/密码认证
	pass     string

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	targets  []string    // 按到达顺序记录的 CONNECT 目标 host:port
	authFail int         // 认证失败次数
	attempts [][2]string // 每次收到的用户名/密码认证请求（用户名、密码）
	active   int         // 当前尚未结束的客户端连接数
	wg       sync.WaitGroup
}

// newSocksServer 启动服务器。credentials 为空表示无认证，否则必须恰为 用户名、密码 两个值。测试结束时自动关闭。
func newSocksServer(t *testing.T, forward string, credentials ...string) *socksServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socksServer{listener: listener, forward: forward, conns: map[net.Conn]struct{}{}}
	if len(credentials) == 2 {
		s.user, s.pass = credentials[0], credentials[1]
	} else if len(credentials) != 0 {
		t.Fatal("newSocksServer 的 credentials 必须为空或恰为用户名与密码")
	}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.close)
	return s
}

func (s *socksServer) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// addr 返回服务器监听地址 host:port。
func (s *socksServer) addr() string { return s.listener.Addr().String() }

// url 返回指向本服务器的代理 URL；credentials 同 socksURL。
func (s *socksServer) url(credentials ...string) string {
	return socksURL(s.addr(), credentials...)
}

// socksURL 拼装 socks5 代理 URL，credentials 为空、仅用户名，或 用户名、密码（按 URL 规则编码）。
func socksURL(addr string, credentials ...string) string {
	u := url.URL{Scheme: "socks5", Host: addr}
	switch len(credentials) {
	case 1:
		u.User = url.User(credentials[0])
	case 2:
		u.User = url.UserPassword(credentials[0], credentials[1])
	}
	return u.String()
}

// connectTargets 返回已收到的 CONNECT 目标（host:port）副本。
func (s *socksServer) connectTargets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.targets...)
}

// connectCount 返回已收到的 CONNECT 次数。
func (s *socksServer) connectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.targets)
}

// hasTarget 报告是否收到过以 target（host:port）为目标的 CONNECT。
func (s *socksServer) hasTarget(target string) bool {
	for _, got := range s.connectTargets() {
		if got == target {
			return true
		}
	}
	return false
}

// authFailures 返回用户名/密码认证失败的次数。
func (s *socksServer) authFailures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authFail
}

// authAttempts 返回收到的用户名/密码认证请求副本（每项为 用户名、密码）。
func (s *socksServer) authAttempts() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]string(nil), s.attempts...)
}

// activeConns 返回尚未结束的客户端连接数（客户端关闭连接后会减少）。
func (s *socksServer) activeConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *socksServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.active++
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
			_ = conn.Close()
			s.mu.Lock()
			delete(s.conns, conn)
			s.active--
			s.mu.Unlock()
		}()
	}
}

// handle 处理一个客户端连接：协商方法、可选认证、CONNECT，然后双向转发到 forward。
func (s *socksServer) handle(conn net.Conn) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, header[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	want := byte(0x00)
	if s.user != "" {
		want = 0x02
	}
	if !bytes.Contains(methods, []byte{want}) {
		_, _ = conn.Write([]byte{5, 0xff})
		return
	}
	if _, err := conn.Write([]byte{5, want}); err != nil {
		return
	}
	if want == 0x02 && !s.authenticate(conn) {
		return
	}
	target, ok := readSocksConnect(conn)
	if !ok {
		_, _ = conn.Write([]byte{5, 0x07, 0, 1, 0, 0, 0, 0, 0, 0}) // command not supported
		return
	}
	s.mu.Lock()
	s.targets = append(s.targets, target)
	s.mu.Unlock()
	upstream, err := net.Dial("tcp", s.forward)
	if err != nil {
		_, _ = conn.Write([]byte{5, 0x05, 0, 1, 0, 0, 0, 0, 0, 0}) // connection refused
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
	_ = conn.Close()
	_ = upstream.Close()
	<-done
}

// authenticate 完成 RFC 1929 用户名/密码认证，失败时回复失败并返回 false。
func (s *socksServer) authenticate(conn net.Conn) bool {
	version := make([]byte, 2) // VER, ULEN
	if _, err := io.ReadFull(conn, version); err != nil || version[0] != 1 {
		return false
	}
	user := make([]byte, version[1])
	if _, err := io.ReadFull(conn, user); err != nil {
		return false
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(conn, plen); err != nil {
		return false
	}
	pass := make([]byte, plen[0])
	if _, err := io.ReadFull(conn, pass); err != nil {
		return false
	}
	s.mu.Lock()
	s.attempts = append(s.attempts, [2]string{string(user), string(pass)})
	s.mu.Unlock()
	if string(user) != s.user || string(pass) != s.pass {
		s.mu.Lock()
		s.authFail++
		s.mu.Unlock()
		_, _ = conn.Write([]byte{1, 1})
		return false
	}
	_, err := conn.Write([]byte{1, 0})
	return err == nil
}

// readSocksConnect 读取 CONNECT 请求并返回目标 host:port；域名（ATYP 3）原样返回。
func readSocksConnect(conn net.Conn) (string, bool) {
	head := make([]byte, 4) // VER CMD RSV ATYP
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 || head[1] != 1 {
		return "", false
	}
	var host string
	switch head[3] {
	case 1:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return "", false
		}
		host = net.IP(ip).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", false
		}
		name := make([]byte, length[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return "", false
		}
		host = string(name)
	case 4:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return "", false
		}
		host = net.IP(ip).String()
	default:
		return "", false
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return "", false
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1]))), true
}

// deadProxyAddr 返回一个没有服务监听的本地地址（先监听再关闭），用于模拟代理不可达。
func deadProxyAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

// dialRecorder 是注入客户端 Transport 的 DialContext：记录每次直连尝试。
// 默认拒绝直连（证明请求没有绕过代理）；设置 forward 后把直连转发到该地址（用于池中无代理账号的直连）。
type dialRecorder struct {
	mu      sync.Mutex
	forward string
	calls   []string
}

func (d *dialRecorder) setForward(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.forward = addr
}

func (d *dialRecorder) DialContext(ctx context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.calls = append(d.calls, addr)
	forward := d.forward
	d.mu.Unlock()
	if forward == "" {
		return nil, errors.New("测试：不允许直连 " + addr)
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", forward)
}

// callCount 返回直连尝试次数。
func (d *dialRecorder) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// dialedAddrs 返回直连尝试的目标地址副本。
func (d *dialRecorder) dialedAddrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// transport 返回以 d 为 DialContext、跳过证书校验的 *http.Transport（代理客户端要求 base 为 nil 或 *http.Transport）。
func (d *dialRecorder) transport() *http.Transport {
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DialContext: d.DialContext}
}

// client 返回使用 transport 的客户端。
func (d *dialRecorder) client() *http.Client {
	return &http.Client{Transport: d.transport()}
}

// proxyEnv 是带代理测试的环境：一个 TLS 假上游（复用管理测试的 mgmtUpstream 逻辑，按路径应答）、
// 一个默认拒绝直连的注入客户端，以及按需创建的 SOCKS5 服务器。
type proxyEnv struct {
	t      *testing.T
	up     *mgmtUpstream
	server *httptest.Server
	dial   *dialRecorder
}

func newProxyEnv(t *testing.T) *proxyEnv {
	t.Helper()
	up := newMgmtUpstream(t)
	server := httptest.NewTLSServer(http.HandlerFunc(up.serve))
	t.Cleanup(server.Close)
	return &proxyEnv{t: t, up: up, server: server, dial: &dialRecorder{}}
}

// newSocks 创建转发到假上游的 SOCKS5 服务器。
func (e *proxyEnv) newSocks(credentials ...string) *socksServer {
	return newSocksServer(e.t, e.server.Listener.Addr().String(), credentials...)
}

// allowDirect 让注入客户端的直连转发到假上游（用于池中没有代理的账号）；直连次数仍由 dial 记录。
func (e *proxyEnv) allowDirect() {
	e.dial.setForward(e.server.Listener.Addr().String())
}

// proxyAccount 返回账号配置：用户名为 user，token 为 "oauth-<user>"（与 mgmtNewPool 一致）。
func proxyAccount(user, proxy string) PoolAccount {
	return PoolAccount{Token: "oauth-" + user, User: user, Proxy: proxy}
}

// newPool 为每个账号写入凭证文件（含 proxy）、登记登录名，并创建启用管理接口、关闭会话亲和的托管池。
// 注入的客户端默认拒绝直连。返回池与凭证目录。
func (e *proxyEnv) newPool(accounts ...PoolAccount) (*Pool, string) {
	e.t.Helper()
	dir := e.t.TempDir()
	for _, account := range accounts {
		if err := SaveCredential(filepath.Join(dir, account.User+".json"),
			Credential{Token: account.Token, User: account.User, Proxy: account.Proxy, Priority: account.Priority}); err != nil {
			e.t.Fatal(err)
		}
		e.up.setLogin(account.Token, account.User)
	}
	p, err := NewManagedPool(PoolConfig{
		APIKey: testAPIKey, ManagementKey: mgmtKey, CredentialsDir: dir, CacheDir: e.t.TempDir(), Client: e.dial.client(),
	}, accounts)
	if err != nil {
		e.t.Fatal(err)
	}
	quotaAwaitIdle(e.t, p)
	e.t.Cleanup(p.WaitCooldownEffects)
	return p, dir
}

// proxyChat 通过池的 HTTP 入口发送一次 chat 请求。
func proxyChat(p *Pool) *httptest.ResponseRecorder {
	return poolRequest(p, "POST", "/v1/chat/completions", poolChatBody)
}

// readCredentialFile 读取凭证文件原文。
func readCredentialFile(t *testing.T, dir, user string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, user+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// loadCredentialOf 读取并解析凭证文件。
func loadCredentialOf(t *testing.T, dir, user string) Credential {
	t.Helper()
	c, err := LoadCredential(filepath.Join(dir, user+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// replaceCredentialWithDir 用目录替换凭证文件，使之后对该文件的读写都失败（与 cooldown_mgmt_test.go 的做法一致）。
func replaceCredentialWithDir(t *testing.T, dir, user string) string {
	t.Helper()
	path := filepath.Join(dir, user+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
