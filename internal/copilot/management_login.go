package copilot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// deviceLoginTTL 是设备登录结束后状态的保留时长，过期后查询返回 404。
const deviceLoginTTL = 10 * time.Minute

// 设备登录的状态取值。
const (
	loginPending = "pending"
	loginSuccess = "success"
	loginError   = "error"
)

// deviceLogin 是一次设备登录的状态，仅保存在内存中。字段由 Pool.loginMu 保护。
type deviceLogin struct {
	status string    // loginPending、loginSuccess 或 loginError
	id     string    // 成功时为新账号的 ID
	err    string    // 失败原因，不含 token
	doneAt time.Time // 结束时间，仅在非 pending 时有效
}

// handleStartLogin 发起 GitHub 设备登录：返回 state、验证页地址与验证码，之后由后台轮询直到授权完成。
// 设备登录使用顶层 host（未配置时为 github.com）。
func (p *Pool) handleStartLogin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	device, page, err := startDeviceFlow(ctx, p.client, p.config.Host)
	cancel()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	state, err := newLoginState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法生成登录状态")
		return
	}
	login := &deviceLogin{status: loginPending}
	p.loginMu.Lock()
	p.pruneLoginsLocked(time.Now())
	p.logins[state] = login
	p.loginMu.Unlock()
	go p.completeDeviceLogin(login, device)
	writeJSON(w, http.StatusOK, map[string]any{
		"state": state, "url": page, "user_code": device.UserCode,
		"interval": max(device.Interval, 1), "expires_in": device.ExpiresIn,
	})
}

// handleLoginStatus 返回设备登录的当前状态；未知或已过期的 state 返回 404。
func (p *Pool) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	p.loginMu.Lock()
	p.pruneLoginsLocked(time.Now())
	login, ok := p.logins[state]
	var result map[string]string
	if ok {
		result = map[string]string{"status": login.status}
		if login.id != "" {
			result["id"] = login.id
		}
		if login.err != "" {
			result["error"] = login.err
		}
	}
	p.loginMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "登录会话不存在或已过期")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// completeDeviceLogin 在后台完成设备登录，并把结果写入登录状态。
func (p *Pool) completeDeviceLogin(login *deviceLogin, device deviceCode) {
	account, err := p.finishDeviceLogin(device)
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	login.doneAt = time.Now()
	if err != nil {
		login.status = loginError
		login.err = err.Error()
		return
	}
	login.status = loginSuccess
	login.id = account.id
}

// finishDeviceLogin 等待用户授权，校验账号并按导入账号的规则保存、加入池。返回的错误信息不含 token。
func (p *Pool) finishDeviceLogin(device deviceCode) (*poolAccount, error) {
	token, err := pollDeviceToken(context.Background(), p.client, p.config.Host, device)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	user, err := verifyGitHubAccount(ctx, p.client, p.config.Host, token)
	if err != nil {
		return nil, err
	}
	return p.addVerifiedAccount(token, p.config.Host, user)
}

// pruneLoginsLocked 删除已结束超过 deviceLoginTTL 的登录状态。调用方须持有 p.loginMu。
func (p *Pool) pruneLoginsLocked(now time.Time) {
	for state, login := range p.logins {
		if login.status != loginPending && now.Sub(login.doneAt) > deviceLoginTTL {
			delete(p.logins, state)
		}
	}
}

// newLoginState 生成 16 字节随机数的十六进制字符串（32 个字符），作为登录状态的标识。
func newLoginState() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
