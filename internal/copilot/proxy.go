package copilot

// 账号代理（SOCKS5）：地址解析与校验、构造走代理的客户端。行为见 docs/proxy.md。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// proxyDialTimeout 是连接代理服务器本身的超时。
const proxyDialTimeout = 10 * time.Second

// maxSocksAuthField 是 RFC 1929 规定的用户名、密码长度上限（字节）。
const maxSocksAuthField = 255

// parsedProxy 是通过校验的代理地址。
type parsedProxy struct {
	scheme   string // 保持 URL 中的原写法
	host     string // IPv6 不带方括号
	port     int
	username string
	password string
	// rawPassword 是 URL 中的原始（百分号编码）密码，与 password 不同时也需要脱敏。
	rawPassword string
}

// addr 返回代理服务器的 host:port。
func (p parsedProxy) addr() string { return net.JoinHostPort(p.host, strconv.Itoa(p.port)) }

// parseProxy 校验并解析代理 URL（规则见 docs/proxy.md「凭证字段」）。raw 须已去除首尾空白且非空。
// 错误信息以“代理地址无效”开头，不包含 URL 的任何部分，避免泄露密码。
func parseProxy(raw string) (parsedProxy, error) {
	invalid := func(reason string) (parsedProxy, error) {
		return parsedProxy{}, errors.New("代理地址无效: " + reason)
	}
	// url.Parse 的错误会带上完整 URL，这里不使用其错误文本。
	u, err := url.Parse(raw)
	if err != nil {
		return invalid("格式错误")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "socks5" && scheme != "socks5h" {
		return invalid("scheme 只能是 socks5 或 socks5h")
	}
	if u.Host == "" || u.Hostname() == "" {
		return invalid("缺少主机")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return invalid("端口必须在 1 到 65535 之间")
	}
	if (u.Path != "" && u.Path != "/") || u.Opaque != "" || strings.ContainsAny(raw, "?#") {
		return invalid("不能包含路径、query 或 fragment")
	}
	// url.Parse 会把 scheme 转成小写，这里取原文，使视图与凭证文件中的写法一致。
	rawScheme, _, _ := strings.Cut(raw, "://")
	result := parsedProxy{scheme: rawScheme, host: u.Hostname(), port: port}
	if u.User != nil {
		var hasPassword bool
		result.username = u.User.Username()
		result.password, hasPassword = u.User.Password()
		if hasPassword && result.username == "" {
			return invalid("有密码时必须有用户名")
		}
		if len(result.username) > maxSocksAuthField || len(result.password) > maxSocksAuthField {
			return invalid("用户名与密码各不能超过 255 字节")
		}
		if hasPassword {
			result.rawPassword = rawProxyPassword(raw)
		}
	}
	return result, nil
}

// rawProxyPassword 返回 URL userinfo 中未解码的密码；不存在时返回空串。
func rawProxyPassword(raw string) string {
	_, rest, _ := strings.Cut(raw, "://")
	authority, _, _ := strings.Cut(rest, "/")
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return ""
	}
	_, password, _ := strings.Cut(authority[:at], ":")
	return password
}

// normalizeProxy 去除首尾空白后校验代理 URL，返回去除空白后的字符串（scheme 保持原样）；空串表示无代理。
func normalizeProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if _, err := parseProxy(raw); err != nil {
		return "", err
	}
	return raw, nil
}

// proxySecrets 返回代理 URL 中需要脱敏的密码写法（解码后与原始编码）；无密码或地址非法时返回 nil。
func proxySecrets(proxy string) []string {
	parsed, err := parseProxy(strings.TrimSpace(proxy))
	if err != nil || parsed.password == "" {
		return nil
	}
	return []string{parsed.password, parsed.rawPassword}
}

// proxyClient 返回账号使用的 HTTP 客户端。proxy 为空时等同 safeClient(base)；否则所有连接都只经 SOCKS5 代理，
// 忽略 Proxy 函数、DialTLSContext 与环境变量代理，域名交给代理端解析，base 不会被修改。
// base.Transport 不是 *http.Transport（且非 nil）时返回错误，因为无法保证不绕过代理。
func proxyClient(base *http.Client, proxy string) (*http.Client, error) {
	proxy, err := normalizeProxy(proxy)
	if err != nil {
		return nil, err
	}
	client := safeClient(base)
	if proxy == "" {
		return client, nil
	}
	parsed, err := parseProxy(proxy)
	if err != nil {
		return nil, err
	}
	var transport *http.Transport
	switch t := client.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = t.Clone()
	default:
		return nil, errors.New("无法为自定义 Transport 配置代理: 无法保证请求不绕过代理")
	}
	var auth *xproxy.Auth
	if parsed.username != "" {
		auth = &xproxy.Auth{User: parsed.username, Password: parsed.password}
	}
	address := parsed.addr()
	dialer, err := xproxy.SOCKS5("tcp", address, auth, &net.Dialer{Timeout: proxyDialTimeout})
	if err != nil {
		return nil, fmt.Errorf("代理 %s 初始化失败: %w", address, err)
	}
	socks := dialer.(xproxy.ContextDialer)
	transport.Proxy = nil
	transport.DialTLSContext = nil
	transport.Dial, transport.DialTLS = nil, nil // 废弃字段同样可能绕过代理
	transport.DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
		conn, err := socks.DialContext(ctx, network, target)
		if err != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("代理 %s 连接失败: %w", address, err)
		}
		return conn, err
	}
	client.Transport = transport
	return client, nil
}

// ProxyView 是账号视图中的代理信息，不含密码。
type ProxyView struct {
	Scheme      string `json:"scheme"`
	Host        string `json:"host"` // IPv6 不带方括号
	Port        int    `json:"port"`
	Username    string `json:"username,omitempty"`
	HasPassword bool   `json:"has_password"`
}

// proxyView 返回代理 URL 对应的视图；无代理或地址非法时返回 nil。
func proxyView(proxy string) *ProxyView {
	if proxy == "" {
		return nil
	}
	parsed, err := parseProxy(proxy)
	if err != nil {
		return nil
	}
	return &ProxyView{
		Scheme: parsed.scheme, Host: parsed.host, Port: parsed.port,
		Username: parsed.username, HasPassword: parsed.password != "",
	}
}

// sameProxy 报告两个代理 URL 是否指向同一代理（scheme、主机、端口、用户名、密码都相同）；二者都为空也算相同。
// 写法上的差异（如尾随斜杠、密码的编码方式）不算变化。二者都应已通过 normalizeProxy。
func sameProxy(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	pa, errA := parseProxy(a)
	pb, errB := parseProxy(b)
	if errA != nil || errB != nil {
		return a == b
	}
	pa.rawPassword, pb.rawPassword = "", ""
	pa.scheme, pb.scheme = strings.ToLower(pa.scheme), strings.ToLower(pb.scheme)
	pa.host, pb.host = strings.ToLower(pa.host), strings.ToLower(pb.host)
	return pa == pb
}

// mergeProxyPassword 在 next 带用户名而不带密码、current 有密码，且二者的主机（忽略大小写）、端口、用户名都相同时，
// 返回 next 加上 current 的密码；其他情况原样返回 next，避免把密码发给另一台代理。
// 用于 PATCH 的 proxy_keep_password：控制台不回显密码，只改 scheme 等无关写法时沿用原密码。
func mergeProxyPassword(next, current string) string {
	if next == "" || current == "" {
		return next
	}
	parsedNext, errNext := parseProxy(next)
	parsedCurrent, errCurrent := parseProxy(current)
	if errNext != nil || errCurrent != nil || parsedNext.username == "" || parsedNext.password != "" || parsedCurrent.password == "" ||
		!strings.EqualFold(parsedNext.host, parsedCurrent.host) || parsedNext.port != parsedCurrent.port || parsedNext.username != parsedCurrent.username {
		return next
	}
	// 只替换 userinfo：scheme 与主机部分保持原写法。userinfo 中的 @ 必为 %40，因此最后一个 @ 即分隔符。
	scheme, rest, _ := strings.Cut(next, "://")
	at := strings.LastIndex(rest, "@")
	userinfo := url.UserPassword(parsedNext.username, parsedCurrent.password).String()
	return scheme + "://" + userinfo + "@" + rest[at+1:]
}
