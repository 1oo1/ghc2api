package copilot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// newLogsTestPool 构造带日志文件路径的托管池；logFile 为空表示未配置日志文件。
func newLogsTestPool(t *testing.T, logFile string) *Pool {
	t.Helper()
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, APIKey: mgmtAPIKey, CredentialsDir: t.TempDir(), CacheDir: t.TempDir(), LogFile: logFile,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// logsTestFile 在临时目录写入日志文件并返回其路径。
func logsTestFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ghc2api.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// logsErrorMessage 解析 writeError 格式的错误响应并返回 error.message。
func logsErrorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var res struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("错误响应不是 writeError 格式的 JSON: %v; body=%s", err, w.Body.String())
	}
	return res.Error.Message
}

// TestManagementLogsRequiresConsoleToken 未带凭证、带错误凭证或带管理密钥（非控制台令牌）都返回 401。
func TestManagementLogsRequiresConsoleToken(t *testing.T) {
	p := newLogsTestPool(t, logsTestFile(t, "{}\n"))
	for _, tc := range []struct{ name, key string }{
		{"无令牌", ""},
		{"错误令牌", "not-a-token"},
		{"管理密钥", mgmtKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := mgmtCallRaw(p, "GET", "/v0/management/logs", "", tc.key)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestManagementLogsReturnsFileWithHeaders 默认读取返回日志原文，响应头含 Content-Type、Cache-Control 与 X-Log-Truncated=false。
func TestManagementLogsReturnsFileWithHeaders(t *testing.T) {
	content := `{"time":"2026-11-03T10:00:00+08:00","level":"INFO","msg":"配置加载完成"}` + "\n" +
		`{"time":"2026-11-03T10:00:01+08:00","level":"INFO","msg":"收到客户端请求","request_id":1}` + "\n"
	p := newLogsTestPool(t, logsTestFile(t, content))
	w := mgmtCall(p, "GET", "/v0/management/logs", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != content {
		t.Fatalf("body = %q, want %q", got, content)
	}
	if got := w.Header().Get("Content-Type"); got != "application/x-ndjson; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := w.Header().Get("X-Log-Truncated"); got != "false" {
		t.Fatalf("X-Log-Truncated = %q, want false", got)
	}
}

// TestManagementLogsIncludesBackups 响应包含滚动备份，按时间顺序（最旧在前）拼接。
func TestManagementLogsIncludesBackups(t *testing.T) {
	path := logsTestFile(t, "new\n")
	if err := os.WriteFile(path+".1", []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newLogsTestPool(t, path)
	w := mgmtCall(p, "GET", "/v0/management/logs", "", mgmtKey)
	if w.Code != http.StatusOK || w.Body.String() != "old\nnew\n" {
		t.Fatalf("status = %d, body = %q, want 200 与 \"old\\nnew\\n\"", w.Code, w.Body.String())
	}
}

// TestManagementLogsEmptyFile 日志文件不存在或为空时返回 200 与空正文，X-Log-Truncated=false。
func TestManagementLogsEmptyFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "ghc2api.log")
	for name, path := range map[string]string{"文件不存在": missing, "空文件": logsTestFile(t, "")} {
		t.Run(name, func(t *testing.T) {
			p := newLogsTestPool(t, path)
			w := mgmtCall(p, "GET", "/v0/management/logs", "", mgmtKey)
			if w.Code != http.StatusOK || w.Body.Len() != 0 {
				t.Fatalf("status = %d, body = %q, want 200 与空正文", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-Log-Truncated"); got != "false" {
				t.Fatalf("X-Log-Truncated = %q, want false", got)
			}
		})
	}
}

// TestManagementLogsMaxBytesApplies 合法的 max_bytes 生效：只返回最后至多 max_bytes 字节内的完整行，并标记 truncated。
func TestManagementLogsMaxBytesApplies(t *testing.T) {
	p := newLogsTestPool(t, logsTestFile(t, "aaa\nbbb\nccc\n"))
	for _, tc := range []struct {
		query     string
		want      string
		truncated string
	}{
		{"max_bytes=8", "bbb\nccc\n", "true"},
		{"max_bytes=6", "ccc\n", "true"},
		{"max_bytes=12", "aaa\nbbb\nccc\n", "false"},
		{"max_bytes=1", "", "true"},
		{"max_bytes=67108864", "aaa\nbbb\nccc\n", "false"}, // 上限本身合法
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := mgmtCall(p, "GET", "/v0/management/logs?"+tc.query, "", mgmtKey)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if w.Body.String() != tc.want {
				t.Fatalf("body = %q, want %q", w.Body.String(), tc.want)
			}
			if got := w.Header().Get("X-Log-Truncated"); got != tc.truncated {
				t.Fatalf("X-Log-Truncated = %q, want %s", got, tc.truncated)
			}
		})
	}
}

// TestManagementLogsDefaultMaxBytesIs8MiB 缺省读取大小为 8 MiB：更大的文件被截断，返回内容不超过 8 MiB 且只含完整行。
func TestManagementLogsDefaultMaxBytesIs8MiB(t *testing.T) {
	if defaultLogReadBytes != 8*1024*1024 || maxLogReadBytes != 64*1024*1024 {
		t.Fatalf("defaultLogReadBytes = %d, maxLogReadBytes = %d, want 8388608 与 67108864", defaultLogReadBytes, maxLogReadBytes)
	}
	line := strings.Repeat("x", 1023) + "\n"
	content := strings.Repeat(line, 9*1024) // 9 MiB
	p := newLogsTestPool(t, logsTestFile(t, content))
	w := mgmtCall(p, "GET", "/v0/management/logs", "", mgmtKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Log-Truncated"); got != "true" {
		t.Fatalf("X-Log-Truncated = %q, want true", got)
	}
	n := w.Body.Len()
	if n == 0 || n > 8*1024*1024 || n%len(line) != 0 {
		t.Fatalf("body 长度 = %d，应为不超过 8 MiB 的整行（每行 %d 字节）", n, len(line))
	}
}

// TestManagementLogsRejectsInvalidMaxBytes 非整数、0、负数、超过 67108864 的 max_bytes 返回 400，错误信息含 max_bytes。
func TestManagementLogsRejectsInvalidMaxBytes(t *testing.T) {
	p := newLogsTestPool(t, logsTestFile(t, "aaa\n"))
	for _, value := range []string{"abc", "", "1.5", "0", "-1", "67108865", strconv.FormatInt(1<<40, 10), "99999999999999999999"} {
		t.Run("max_bytes="+value, func(t *testing.T) {
			w := mgmtCall(p, "GET", "/v0/management/logs?max_bytes="+value, "", mgmtKey)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if msg := logsErrorMessage(t, w); !strings.Contains(msg, "max_bytes") {
				t.Fatalf("错误信息 %q 应包含 max_bytes", msg)
			}
		})
	}
}

// TestManagementLogsNotConfigured LogFile 为空时返回 404，错误信息为“未配置日志文件”。
func TestManagementLogsNotConfigured(t *testing.T) {
	p := newLogsTestPool(t, "")
	w := mgmtCall(p, "GET", "/v0/management/logs", "", mgmtKey)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if msg := logsErrorMessage(t, w); msg != "未配置日志文件" {
		t.Fatalf("错误信息 = %q, want 未配置日志文件", msg)
	}
}

// TestManagementLogsReadFailure 读取失败（文件不可读）返回 500。
func TestManagementLogsReadFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受文件权限限制")
	}
	path := logsTestFile(t, "aaa\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	p := newLogsTestPool(t, path)
	w := mgmtCall(p, "GET", "/v0/management/logs", "", mgmtKey)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}
