package main

// 本文件覆盖 serve 退出阶段的会话亲和保存：收到退出信号并关闭 HTTP 服务后保存绑定文件（成功输出 Info，失败只输出 Warn），
// 未开启会话亲和时不生成绑定文件。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// startServiceWithStop 在后台运行 run，并等待 /healthz 就绪。返回的 stop 取消上下文、等待服务退出，并返回 run 的结果；
// 日志（JSON 行）写入返回的缓冲区。测试结束时若尚未 stop，会自动停止服务。
func startServiceWithStop(t *testing.T, configPath, addr string) (func() error, *synchronizedBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &synchronizedBuffer{}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = run(ctx, []string{"-c", configPath}, io.Discard, stderr)
	}()

	baseURL := "http://" + addr
	client := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for ready := false; !ready; {
		select {
		case <-done:
			cancel()
			t.Fatalf("服务未能启动: %v; stderr=%s", runErr, stderr.String())
		default:
		}
		if res, err := client.Get(baseURL + "/healthz"); err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			ready = res.StatusCode == http.StatusOK
		}
		if !ready {
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("配置的监听地址未启动; stderr=%s", stderr.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	var stopOnce sync.Once
	var stopErr error
	stop := func() error {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-done:
				stopErr = runErr
			case <-time.After(15 * time.Second):
				stopErr = errors.New("服务在取消上下文后 15 秒内未退出")
			}
		})
		return stopErr
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("服务关闭错误: %v", err)
		}
	})
	return stop, stderr
}

// logEntriesAfterShutdown 返回“收到退出信号”之后输出的 JSON 日志条目（退出信号本身不计入）。
func logEntriesAfterShutdown(t *testing.T, output string) []map[string]any {
	t.Helper()
	var after []map[string]any
	shutdown := false
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSON log line %q: %v", line, err)
		}
		if shutdown {
			after = append(after, entry)
		}
		if msg, _ := entry["msg"].(string); msg == "收到退出信号，正在关闭 HTTP 服务" {
			shutdown = true
		}
	}
	if !shutdown {
		t.Fatalf("日志中没有退出信号: %s", output)
	}
	return after
}

// hasLogEntry 报告 entries 中是否有级别为 level、消息包含 msgPart 的日志。
func hasLogEntry(entries []map[string]any, level, msgPart string) bool {
	for _, entry := range entries {
		msg, _ := entry["msg"].(string)
		if entry["level"] == level && strings.Contains(msg, msgPart) {
			return true
		}
	}
	return false
}

// writeAffinityServiceConfig 写入只有一个账号的配置，并返回配置路径与缓存目录（data/cache）。
// extra 为附加的 YAML 设置，例如 session_affinity: false。
func writeAffinityServiceConfig(t *testing.T, addr, extra string) (configPath, cacheDir string) {
	t.Helper()
	configDir := isolatedConfigDir(t)
	writeCredentialFile(t, filepath.Join(configDir, "data", "credentials", "alice.json"), accountJSON("affinity-shutdown-token"))
	configPath = writeConfigInDir(t, configDir, appConfigYAML(addr, "", "", "info")+extra)
	return configPath, filepath.Join(configDir, "data", "cache")
}

// TestServeSavesSessionAffinityOnShutdown 收到退出信号并关闭 HTTP 服务后，serve 保存会话亲和：
// 缓存目录下生成合法 JSON 的 session_affinity.json（权限 0600），退出阶段输出 Info「会话亲和：已保存」，serve 返回 nil。
// 注意：cmd 层没有注入访问 GitHub 的客户端，无法在这里完成一次带会话信号的生成请求，因此绑定表为空；
// 绑定内容的保存与恢复由 internal/copilot 的 Pool 级测试覆盖。
func TestServeSavesSessionAffinityOnShutdown(t *testing.T) {
	addr := freeLoopbackAddressForTest(t)
	configPath, cacheDir := writeAffinityServiceConfig(t, addr, "")
	stop, stderr := startServiceWithStop(t, configPath, addr)

	if err := stop(); err != nil {
		t.Fatalf("serve 返回错误: %v; stderr=%s", err, stderr.String())
	}
	path := filepath.Join(cacheDir, "session_affinity.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("退出后缺少绑定文件 %q: %v; stderr=%s", path, err, stderr.String())
	}
	if !json.Valid(data) {
		t.Fatalf("绑定文件不是合法 JSON: %q", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("绑定文件权限 = %04o, want 0600", got)
		}
	}
	if !hasLogEntry(logEntriesAfterShutdown(t, stderr.String()), "INFO", "会话亲和：已保存") {
		t.Fatalf("退出阶段缺少 Info「会话亲和：已保存」日志: %s", stderr.String())
	}
}

// TestServeSkipsSessionAffinitySaveWhenDisabled 关闭会话亲和（session_affinity: false）时，退出阶段不生成绑定文件，
// 也不输出「会话亲和：已保存」。
func TestServeSkipsSessionAffinitySaveWhenDisabled(t *testing.T) {
	addr := freeLoopbackAddressForTest(t)
	configPath, cacheDir := writeAffinityServiceConfig(t, addr, "session_affinity: false\n")
	stop, stderr := startServiceWithStop(t, configPath, addr)

	if err := stop(); err != nil {
		t.Fatalf("serve 返回错误: %v; stderr=%s", err, stderr.String())
	}
	path := filepath.Join(cacheDir, "session_affinity.json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("关闭会话亲和时不应生成绑定文件，stat error = %v", err)
	}
	if hasLogEntry(logEntriesAfterShutdown(t, stderr.String()), "INFO", "会话亲和：已保存") {
		t.Fatalf("关闭会话亲和时不应输出「会话亲和：已保存」: %s", stderr.String())
	}
}

// TestServeWarnsWhenSessionAffinitySaveFails 保存绑定失败只输出 Warn，不改变 serve 的返回值（仍为 nil）。
// 通过把绑定文件的路径占用为目录来制造写入失败：临时文件无法重命名为该路径。
func TestServeWarnsWhenSessionAffinitySaveFails(t *testing.T) {
	addr := freeLoopbackAddressForTest(t)
	configPath, cacheDir := writeAffinityServiceConfig(t, addr, "")
	if err := os.MkdirAll(filepath.Join(cacheDir, "session_affinity.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	stop, stderr := startServiceWithStop(t, configPath, addr)

	if err := stop(); err != nil {
		t.Fatalf("保存失败不应改变 serve 的返回值: %v; stderr=%s", err, stderr.String())
	}
	entries := logEntriesAfterShutdown(t, stderr.String())
	if !hasLogEntry(entries, "WARN", "会话亲和") {
		t.Fatalf("退出阶段保存失败时缺少含“会话亲和”的 Warn 日志: %s", stderr.String())
	}
	if hasLogEntry(entries, "INFO", "会话亲和：已保存") {
		t.Fatalf("保存失败时不应输出「会话亲和：已保存」: %s", stderr.String())
	}
}
