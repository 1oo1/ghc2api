package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// loginExpiredMessage 是验证码在授权前过期时返回的错误文本，与 GitHub 返回 expired_token 时相同。
const loginExpiredMessage = "验证码已过期，请重新登录"

// loginExpiryClient 模拟 GitHub 设备登录：验证码只有 1 秒有效，授权始终处于 authorization_pending，
// 因此轮询只能因验证码过期而结束。
func loginExpiryClient(t *testing.T) *http.Client {
	t.Helper()
	return testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			testJSON(w, map[string]any{
				"device_code": "expiring-device-code", "user_code": "EXPI-RING",
				"verification_uri": "https://github.com/login/device", "expires_in": 1, "interval": 1,
			})
		case "/login/oauth/access_token":
			testJSON(w, map[string]string{"error": "authorization_pending"})
		default:
			t.Errorf("意外的上游路径 %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
}

// TestLoginReportsExpiredCodeWhenNotAuthorizedInTime 验证码在用户授权前过期（父上下文未取消）时，
// CLI 登录返回与 expired_token 相同的中文错误，且不写入凭证。
func TestLoginReportsExpiredCodeWhenNotAuthorizedInTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Login(ctx, "", path, loginExpiryClient(t), io.Discard)
	if err == nil || err.Error() != loginExpiredMessage {
		t.Fatalf("Login() error = %v, want %q", err, loginExpiredMessage)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("过期的登录写入了凭证: %v", statErr)
	}
}

// TestLoginParentCancellationIsNotReportedAsExpiredCode 父上下文先于验证码过期而取消时，返回上下文错误，
// 而不是“验证码已过期”。
func TestLoginParentCancellationIsNotReportedAsExpiredCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(300*time.Millisecond, cancel)
	defer timer.Stop()
	err := Login(ctx, "", filepath.Join(t.TempDir(), "credential.json"), loginExpiryClient(t), io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("父上下文取消时 Login() error = %v, want context.Canceled", err)
	}
	if err != nil && err.Error() == loginExpiredMessage {
		t.Fatal("父上下文取消不应被报告为验证码过期")
	}
}

// TestManagementDeviceLoginReportsExpiredCodeInChinese 设备登录（Web）在验证码过期时，login/status 返回 status=error，
// 且 error 为同样的中文提示。
func TestManagementDeviceLoginReportsExpiredCodeInChinese(t *testing.T) {
	dir := t.TempDir()
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, CredentialsDir: dir, CacheDir: t.TempDir(), Client: loginExpiryClient(t),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := mgmtCall(p, "POST", "/v0/management/login", "", mgmtKey)
	var start mgmtLoginStart
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &start) != nil {
		t.Fatalf("发起登录 status=%d body=%s", w.Code, w.Body.String())
	}
	result := mgmtPollLogin(t, p, start.State)
	if result.Status != "error" || result.Error != loginExpiredMessage {
		t.Fatalf("设备登录结果 = %+v, want status=error 且 error=%q", result, loginExpiredMessage)
	}
	if got := len(mgmtAccounts(t, p)); got != 0 {
		t.Fatalf("过期的登录加入了 %d 个账号", got)
	}
}
