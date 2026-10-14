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
	var device struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		URI        string `json:"verification_uri"`
		ExpiresIn  int    `json:"expires_in"`
		Interval   int    `json:"interval"`
	}
	if err := oauthJSON(ctx, client, http.MethodPost, oauthBase(host)+"/login/device/code", "",
		url.Values{"client_id": {oauthClientID}, "scope": {"read:user"}}, &device); err != nil {
		return err
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.ExpiresIn <= 0 {
		return errors.New("GitHub 未返回有效的 device code")
	}
	page := oauthBase(host) + "/login/device"
	// 不展示上游响应中指向非 GitHub 主机的验证链接。
	if device.URI == page {
		page = device.URI
	}
	if _, err := fmt.Fprintf(out, "请打开 %s 并输入验证码：%s\n", page, device.UserCode); err != nil {
		return err
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
	defer cancel()
	interval := time.Duration(max(device.Interval, 1)) * time.Second
	misses := 0
	for {
		timer := time.NewTimer(interval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			return pollCtx.Err()
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
			return pollCtx.Err()
		}
		if err != nil {
			misses++
			if misses >= 5 {
				return fmt.Errorf("连续 5 次获取 GitHub token 失败: %w", err)
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
			return errors.New("验证码已过期，请重新登录")
		case "access_denied":
			return errors.New("GitHub 登录授权被拒绝")
		case "":
		default:
			return errors.New("GitHub 无法完成 device 授权")
		}
		if strings.TrimSpace(result.Token) == "" {
			return errors.New("GitHub 未返回 OAuth token")
		}
		finishCtx, finishCancel := context.WithTimeout(ctx, time.Minute)
		err = finishLogin(finishCtx, client, host, path, result.Token, out)
		finishCancel()
		return err
	}
}

func finishLogin(ctx context.Context, client *http.Client, host, path, token string, out io.Writer) error {
	var user struct {
		Login string `json:"login"`
	}
	if err := oauthJSON(ctx, client, http.MethodGet, githubBase(host)+"/user", token, nil, &user); err != nil {
		return err
	}
	if user.Login == "" {
		return errors.New("GitHub 未返回账号名称")
	}
	var subscription map[string]json.RawMessage
	if err := oauthJSON(ctx, client, http.MethodGet, githubBase(host)+"/copilot_internal/user", token, nil, &subscription); err != nil {
		return fmt.Errorf("Copilot 订阅检查失败: %w", err)
	}
	if subscription == nil {
		return errors.New("GitHub 未返回 Copilot 订阅信息")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := SaveCredential(path, Credential{Token: token, User: user.Login, Host: host}); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "账号 %s 登录成功，凭证已写入 %s\n", user.Login, path)
	return err
}
