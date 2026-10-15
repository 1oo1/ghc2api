package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const oauthClientID = "Iv1.b507a08c87ecfe98"

// errCodeExpired 表示验证码在用户授权前过期；CLI 与 Web 登录都返回这一中文提示。
var errCodeExpired = errors.New("验证码已过期，请重新登录")

// loginEndError 返回轮询提前结束的原因。父上下文取消时返回其错误（如 context.Canceled）；
// 否则只能是验证码超时（pollCtx 的截止时间到达），返回 errCodeExpired，而不是 context.DeadlineExceeded。
func loginEndError(parent context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	return errCodeExpired
}

func oauthJSON(ctx context.Context, client *http.Client, method, endpoint, token string, form url.Values, value any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := readLimited(res.Body, 1<<20)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub 登录请求失败（HTTP %d）", res.StatusCode)
	}
	if json.Unmarshal(data, value) != nil {
		return errors.New("GitHub 登录响应不是有效 JSON")
	}
	return nil
}

// deviceCode 是 GitHub 设备授权接口返回的验证码信息。
type deviceCode struct {
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`
	URI        string `json:"verification_uri"`
	ExpiresIn  int    `json:"expires_in"`
	Interval   int    `json:"interval"`
}

// Login 完成 device flow，只有用户身份及订阅检查成功后才保存 token。
func Login(ctx context.Context, host, path string, client *http.Client, out io.Writer) error {
	host, err := NormalizeHost(host)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	client = safeClient(client)
	if out == nil {
		out = io.Discard
	}
	device, page, err := startDeviceFlow(ctx, client, host)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "请打开 %s 并输入验证码：%s\n", page, device.UserCode); err != nil {
		return err
	}
	token, err := pollDeviceToken(ctx, client, host, device)
	if err != nil {
		return err
	}
	finishCtx, finishCancel := context.WithTimeout(ctx, time.Minute)
	defer finishCancel()
	return finishLogin(finishCtx, client, host, path, token, out)
}

// startDeviceFlow 向 GitHub 申请设备验证码，并返回用户应打开的验证页地址。
// 验证页固定为 GitHub 主机的 /login/device，不展示上游响应中指向其他主机的链接。
func startDeviceFlow(ctx context.Context, client *http.Client, host string) (deviceCode, string, error) {
	var device deviceCode
	if err := oauthJSON(ctx, client, http.MethodPost, oauthBase(host)+"/login/device/code", "",
		url.Values{"client_id": {oauthClientID}, "scope": {"read:user"}}, &device); err != nil {
		return deviceCode{}, "", err
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.ExpiresIn <= 0 {
		return deviceCode{}, "", errors.New("GitHub 未返回有效的 device code")
	}
	return device, oauthBase(host) + "/login/device", nil
}

// pollDeviceToken 按 GitHub 给出的间隔轮询，直到用户授权、验证码过期或失败；成功时返回 OAuth token。
// 连续 5 次请求失败即放弃，slow_down 时放慢轮询间隔。
func pollDeviceToken(ctx context.Context, client *http.Client, host string, device deviceCode) (string, error) {
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
	defer cancel()
	interval := time.Duration(max(device.Interval, 1)) * time.Second
	misses := 0
	for {
		timer := time.NewTimer(interval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			return "", loginEndError(ctx)
		case <-timer.C:
		}
		var result struct {
			Token string `json:"access_token"`
			Error string `json:"error"`
		}
		err := oauthJSON(pollCtx, client, http.MethodPost, oauthBase(host)+"/login/oauth/access_token", "",
			url.Values{"client_id": {oauthClientID}, "device_code": {device.DeviceCode},
				"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}, &result)
		if pollCtx.Err() != nil {
			return "", loginEndError(ctx)
		}
		if err != nil {
			misses++
			if misses >= 5 {
				return "", fmt.Errorf("连续 5 次获取 GitHub token 失败: %w", err)
			}
			continue
		}
		misses = 0
		switch result.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "expired_token":
			return "", errCodeExpired
		case "access_denied":
			return "", errors.New("GitHub 登录授权被拒绝")
		case "":
		default:
			return "", errors.New("GitHub 无法完成 device 授权")
		}
		if strings.TrimSpace(result.Token) == "" {
			return "", errors.New("GitHub 未返回 OAuth token")
		}
		return result.Token, nil
	}
}

// verifyGitHubAccount 用 token 查询 GitHub 登录名，并确认 Copilot 订阅信息可以读取；返回登录名。
// 错误信息不包含 token。
func verifyGitHubAccount(ctx context.Context, client *http.Client, host, token string) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := oauthJSON(ctx, client, http.MethodGet, githubBase(host)+"/user", token, nil, &user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", errors.New("GitHub 未返回账号名称")
	}
	var subscription map[string]json.RawMessage
	if err := oauthJSON(ctx, client, http.MethodGet, githubBase(host)+"/copilot_internal/user", token, nil, &subscription); err != nil {
		return "", fmt.Errorf("Copilot 订阅检查失败: %w", err)
	}
	if subscription == nil {
		return "", errors.New("GitHub 未返回 Copilot 订阅信息")
	}
	return user.Login, nil
}

// finishLogin 校验 token 后保存凭证并输出结果；校验或保存失败时不写入凭证。
func finishLogin(ctx context.Context, client *http.Client, host, path, token string, out io.Writer) error {
	login, err := verifyGitHubAccount(ctx, client, host, token)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := SaveCredential(path, Credential{Token: token, User: login, Host: host}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "账号 %s 登录成功，凭证已写入 %s\n", login, path)
	return err
}
