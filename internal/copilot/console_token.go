package copilot

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// consoleTokenTTL 是控制台令牌的有效期。控制台每次打开都会用旧令牌换新令牌（滑动续期）。
const consoleTokenTTL = 14 * 24 * time.Hour

// consoleTokenVersion 是令牌格式版本，也是令牌的第一段。
const consoleTokenVersion = "v1"

// consoleTokenDomain 是签名消息的前缀，避免同一管理密钥签出的其他 HMAC 与令牌签名混用。
const consoleTokenDomain = "ghc2api-console-token:"

// consoleTokenSignature 计算 "v1.<exp>" 的签名：以管理密钥为 HMAC-SHA256 密钥，结果用 base64 RawURL 编码。
func consoleTokenSignature(key, payload string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(consoleTokenDomain + payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// issueConsoleToken 签发控制台令牌，格式为 "v1.<过期时间 Unix 秒>.<签名>"。
// 令牌不含管理密钥本身，服务端无需保存状态；更换管理密钥即令所有已签发令牌失效。
func issueConsoleToken(key string, now time.Time) (string, time.Time) {
	expiresAt := now.Truncate(time.Second).Add(consoleTokenTTL)
	payload := consoleTokenVersion + "." + strconv.FormatInt(expiresAt.Unix(), 10)
	return payload + "." + consoleTokenSignature(key, payload), expiresAt
}

// verifyConsoleToken 校验令牌签名与有效期。过期时间晚于 now+consoleTokenTTL 的令牌同样拒绝（防御性检查）。
func verifyConsoleToken(key, token string, now time.Time) bool {
	if key == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != consoleTokenVersion {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	payload := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(consoleTokenSignature(key, payload))) {
		return false
	}
	expiresAt := time.Unix(exp, 0)
	return now.Before(expiresAt) && !expiresAt.After(now.Add(consoleTokenTTL))
}

// handleCreateSession 签发新的控制台令牌：用管理密钥调用即登录，用有效令牌调用即续期。
// 鉴权已由 requireManagementKey 完成。
func handleCreateSession(key string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		token, expiresAt := issueConsoleToken(key, time.Now())
		writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires_at": expiresAt.UTC().Format(time.RFC3339)})
	}
}
