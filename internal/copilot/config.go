package copilot

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Config 配置单个 Copilot 账号；APIKey 是本服务密钥，不是 GitHub token。
type Config struct {
	Token  string
	Host   string
	APIKey string
	Client *http.Client
	Logger *slog.Logger
}

var enterpriseLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeHost 限制登录目标，避免将 GitHub 凭证发送到任意主机。
func NormalizeHost(host string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(h, "://") {
		u, err := url.Parse(h)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
			u.User != nil || u.Port() != "" || (u.Path != "" && u.Path != "/") ||
			u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
			return "", errors.New("无效的 GitHub 登录域名")
		}
		h = u.Host
	}
	h = strings.TrimSuffix(h, "/")
	if h == "" || h == "github.com" || h == "www.github.com" {
		return "", nil
	}
	if name, ok := strings.CutSuffix(h, ".ghe.com"); ok && enterpriseLabel.MatchString(name) {
		return h, nil
	}
	return "", errors.New("只支持 github.com 或 <name>.ghe.com 登录域名")
}

func githubBase(host string) string {
	if host == "" {
		return "https://api.github.com"
	}
	return "https://api." + host
}

func oauthBase(host string) string {
	if host == "" {
		return "https://github.com"
	}
	return "https://" + host
}

func apiBase(host string) string {
	if host == "" {
		return "https://api.githubcopilot.com"
	}
	return "https://copilot-api." + host
}

func validateAPI(endpoint, host string) (string, error) {
	if endpoint == "" {
		return apiBase(host), nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("Copilot 返回了不安全的 API 地址")
	}
	allowed := u.Host == "api.githubcopilot.com" ||
		u.Host == "api.individual.githubcopilot.com" ||
		u.Host == "api.business.githubcopilot.com" ||
		u.Host == "api.enterprise.githubcopilot.com" ||
		(host != "" && u.Host == "copilot-api."+host)
	if !allowed {
		return "", errors.New("Copilot API 地址不在允许的主机列表中")
	}
	return "https://" + u.Host, nil
}

func safeClient(client *http.Client) *http.Client {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = time.Minute
		client = &http.Client{Transport: transport}
	}
	copy := *client
	// 即使调用方允许重定向，也不能把账号身份头带到另一台服务器。
	copy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	copy.Jar = nil
	return &copy
}
