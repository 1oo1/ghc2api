package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ghc2api/internal/copilot"
)

// roundTripLogin 是企业托管用户（EMU）形式的登录名，同时包含 "-" 与 "_"。
const roundTripLogin = "mona-cat_octo"

// roundTripManagementKey 是 roundTripSetup 使用的管理密钥。
const roundTripManagementKey = "roundtrip-management-key"

// roundTripGitHub 是模拟 GitHub 的 RoundTripper：/user 返回 roundTripLogin，
// 设备登录第一次轮询返回 authorization_pending，之后返回 token。
type roundTripGitHub struct {
	token string

	mu    sync.Mutex
	polls int
}

func (g *roundTripGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	var body string
	switch r.URL.Path {
	case "/user":
		if r.Header.Get("Authorization") != "token "+g.token {
			return roundTripResponse(r, http.StatusUnauthorized, ""), nil
		}
		body = `{"login":"` + roundTripLogin + `"}`
	case "/copilot_internal/user":
		body = `{"copilot_plan":"individual"}`
	case "/login/device/code":
		body = `{"device_code":"roundtrip-device-code","user_code":"WXYZ-1234","verification_uri":"https://github.com/login/device","expires_in":30,"interval":1}`
	case "/login/oauth/access_token":
		g.mu.Lock()
		g.polls++
		first := g.polls == 1
		g.mu.Unlock()
		if first {
			body = `{"error":"authorization_pending"}`
		} else {
			body = `{"access_token":"` + g.token + `"}`
		}
	default:
		return roundTripResponse(r, http.StatusNotFound, ""), nil
	}
	return roundTripResponse(r, http.StatusOK, body), nil
}

func roundTripResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

// roundTripSetup 按 serve 的目录布局创建管理池：凭证目录为 configDir/data/credentials，并返回配置文件路径。
func roundTripSetup(t *testing.T, token string) (configPath string, pool *copilot.Pool, credentials string) {
	t.Helper()
	configDir := isolatedConfigDir(t)
	credentials = filepath.Join(configDir, "data", "credentials")
	configPath = writeConfigInDir(t, configDir, "management_key: "+roundTripManagementKey+"\n")
	pool, err := copilot.NewManagedPool(copilot.PoolConfig{
		ManagementKey:  roundTripManagementKey,
		CredentialsDir: credentials,
		CacheDir:       t.TempDir(),
		Client:         &http.Client{Transport: &roundTripGitHub{token: token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return configPath, pool, credentials
}

// assertRoundTripAccountLoads 检查凭证文件已写出，且启动加载（loadConfig）能读取该账号及其 token。
func assertRoundTripAccountLoads(t *testing.T, configPath, credentials, token string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(credentials, roundTripLogin+".json")); err != nil {
		t.Fatalf("凭证文件 %s.json 未写出: %v", roundTripLogin, err)
	}
	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("管理接口写出的凭证无法被启动加载: %v", err)
	}
	if account := accountByUser(t, config, roundTripLogin); account.Token != token {
		t.Fatalf("启动加载的 token 与导入的不一致")
	}
}

// TestManagementWrittenAccountsLoadAtStartup 管理接口导入或设备登录写出的凭证（登录名含 "-" 与 "_"）
// 能被启动时的 loadConfig 正常读取，并加入账号列表。
func TestManagementWrittenAccountsLoadAtStartup(t *testing.T) {
	const token = "gho_roundtrip_token"

	t.Run("import", func(t *testing.T) {
		configPath, pool, credentials := roundTripSetup(t, token)
		r := httptest.NewRequest(http.MethodPost, "/v0/management/accounts",
			strings.NewReader(`{"credential":{"oauth_token":"`+token+`"}}`))
		r.Header.Set("Authorization", "Bearer "+roundTripManagementKey)
		w := httptest.NewRecorder()
		pool.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("导入 status=%d body=%s", w.Code, w.Body.String())
		}
		assertRoundTripAccountLoads(t, configPath, credentials, token)
	})

	t.Run("device-login", func(t *testing.T) {
		configPath, pool, credentials := roundTripSetup(t, token)
		start := httptest.NewRecorder()
		login := httptest.NewRequest(http.MethodPost, "/v0/management/login", nil)
		login.Header.Set("Authorization", "Bearer "+roundTripManagementKey)
		pool.Handler().ServeHTTP(start, login)
		var begun struct {
			State string `json:"state"`
		}
		if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &begun) != nil {
			t.Fatalf("发起设备登录 status=%d body=%s", start.Code, start.Body.String())
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			status := httptest.NewRecorder()
			poll := httptest.NewRequest(http.MethodGet, "/v0/management/login/status?state="+begun.State, nil)
			poll.Header.Set("Authorization", "Bearer "+roundTripManagementKey)
			pool.Handler().ServeHTTP(status, poll)
			var result struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}
			if status.Code != http.StatusOK || json.Unmarshal(status.Body.Bytes(), &result) != nil {
				t.Fatalf("登录状态 status=%d body=%s", status.Code, status.Body.String())
			}
			if result.Status == "success" {
				break
			}
			if result.Status != "pending" || time.Now().After(deadline) {
				t.Fatalf("设备登录结束于 %q（%s），want success", result.Status, result.Error)
			}
			time.Sleep(50 * time.Millisecond)
		}
		assertRoundTripAccountLoads(t, configPath, credentials, token)
	})
}
