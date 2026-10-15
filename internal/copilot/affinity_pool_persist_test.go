package copilot

// 本文件覆盖 Pool 级的会话亲和持久化：同一会话在 TTL 内每次请求都续期（TTL 从最后一次请求计算）、重启后恢复绑定、
// 未开启亲和或缓存目录为空时 SaveSessionAffinity 不做任何事、绑定文件损坏时池仍能以空表启动并输出 Warn。

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// affinityPoolKey 计算经 Pool 发送的 chat 请求（不带客户端密钥）对应的会话键。
func affinityPoolKey(t *testing.T, body string) string {
	t.Helper()
	key := sessionKey(affinityRequest("/v1/chat/completions", "", ""), []byte(body), "/chat/completions")
	affinityAssertKeyShape(t, key)
	return key
}

// affinityPersistPool 创建不启用管理接口的池，账号的缓存与会话亲和绑定文件都位于 cacheDir，有效期为默认值。
func affinityPersistPool(t *testing.T, up *mgmtUpstream, cacheDir string, accounts []PoolAccount) *Pool {
	t.Helper()
	p, err := NewManagedPool(PoolConfig{
		CacheDir: cacheDir, Client: up.client, SessionAffinityTTL: DefaultSessionAffinityTTL,
	}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// affinityPersistLogger 返回把 JSON 日志写入 buf、级别不低于 level 的日志器。
func affinityPersistLogger(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level}))
}

// affinityPersistWarnings 返回日志中级别为 WARN 且消息含“会话亲和”的消息文本。
func affinityPersistWarnings(t *testing.T, output []byte) []string {
	t.Helper()
	var messages []string
	for _, entry := range jsonLogEntries(t, output) {
		msg, _ := entry["msg"].(string)
		if entry["level"] == "WARN" && strings.Contains(msg, "会话亲和") {
			messages = append(messages, msg)
		}
	}
	return messages
}

// TestAffinityPoolRenewsBindingFromLastRequest 同一会话在 TTL 内的每次请求都把绑定截止时刻推后到“本次请求时刻+TTL”，
// 即 TTL 从最后一次请求计算。每轮先把截止时刻人为设为 1 分钟后（仍有效但接近过期），再发送请求并检查续期结果。
func TestAffinityPoolRenewsBindingFromLastRequest(t *testing.T) {
	const ttl = time.Hour
	up := newMgmtUpstream(t)
	p, _ := mgmtNewPool(t, up, ttl, "alice", "bob")
	body := affinityChatBody(t, "renew the session")
	key := affinityPoolKey(t, body)
	if res := poolRequest(p, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
		t.Fatalf("首次请求 status=%d body=%s", res.Code, res.Body.String())
	}
	for round := 1; round <= 3; round++ {
		// 把绑定设为“1 分钟后截止”，模拟上一次命中之后已经过去了接近 TTL 的时间。
		p.affinity.bind(key, "alice", time.Now().Add(time.Minute-ttl))
		before := time.Now()
		if res := poolRequest(p, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求 status=%d body=%s", round, res.Code, res.Body.String())
		}
		after := time.Now()
		// 续期后截止时刻不早于 before+TTL，因此在 before+TTL-1 分钟时仍有效。
		if _, ok := p.affinity.lookup(key, before.Add(ttl-time.Minute)); !ok {
			t.Fatalf("第 %d 次请求后绑定没有续期：截止时刻仍是请求前设定的值", round)
		}
		// 截止时刻不晚于 after+TTL，因此在 after+TTL 时已失效，有效期没有超过 TTL。
		if _, ok := p.affinity.lookup(key, after.Add(ttl)); ok {
			t.Fatalf("第 %d 次请求后绑定的有效期超过了 TTL", round)
		}
	}
	assertGenerationOrder(t, up.pool, mgmtOrder("alice", "alice", "alice", "alice")...)
}

// TestAffinityPoolBindingSurvivesRestart 保存绑定后用同一缓存目录重建池：同一会话的下一个请求仍落到原账号，
// 而新会话按当前优先级选择。重建时 bob 的优先级更高，若绑定没有恢复，原会话会改由 bob 处理。
func TestAffinityPoolBindingSurvivesRestart(t *testing.T) {
	up := newMgmtUpstream(t)
	cacheDir := t.TempDir()
	alice := PoolAccount{Token: "oauth-alice", User: "alice"}
	bob := PoolAccount{Token: "oauth-bob", User: "bob"}
	first := affinityPersistPool(t, up, cacheDir, []PoolAccount{alice, bob})
	body := affinityChatBody(t, "restart session")
	// 首次请求时 alice 与 bob 优先级相同，按文件名选中 alice，并建立绑定。
	if res := poolRequest(first, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
		t.Fatalf("重启前请求 status=%d body=%s", res.Code, res.Body.String())
	}
	if err := first.SaveSessionAffinity(); err != nil {
		t.Fatalf("SaveSessionAffinity() error = %v", err)
	}

	bob.Priority = 10
	restarted := affinityPersistPool(t, up, cacheDir, []PoolAccount{alice, bob})
	if res := poolRequest(restarted, "POST", "/v1/chat/completions", body); res.Code != http.StatusOK {
		t.Fatalf("重启后同一会话请求 status=%d body=%s", res.Code, res.Body.String())
	}
	if res := poolRequest(restarted, "POST", "/v1/chat/completions", affinityChatBody(t, "new session")); res.Code != http.StatusOK {
		t.Fatalf("重启后新会话请求 status=%d body=%s", res.Code, res.Body.String())
	}
	// 同一会话恢复到 alice；新会话按优先级由 bob 处理。
	assertGenerationOrder(t, up.pool, mgmtOrder("alice", "alice", "bob")...)
}

// TestSaveSessionAffinityIsNoOpWithoutAffinityOrCacheDir 未开启亲和或 CacheDir 为空时，SaveSessionAffinity 返回 nil 且不写文件；
// 未开启亲和时启动也不读取已有的绑定文件。
func TestSaveSessionAffinityIsNoOpWithoutAffinityOrCacheDir(t *testing.T) {
	t.Run("affinity-disabled", func(t *testing.T) {
		cacheDir := t.TempDir()
		path := filepath.Join(cacheDir, "session_affinity.json")
		corrupt := []byte("{not json")
		if err := os.WriteFile(path, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		p, err := NewManagedPool(PoolConfig{
			ManagementKey: mgmtKey, CredentialsDir: t.TempDir(), CacheDir: cacheDir,
			SessionAffinityTTL: 0, Logger: affinityPersistLogger(&logs, slog.LevelDebug),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(logs.String(), "会话亲和") {
			t.Fatalf("未开启亲和时不应读取绑定文件: %s", logs.String())
		}
		if err := p.SaveSessionAffinity(); err != nil {
			t.Fatalf("未开启亲和时 SaveSessionAffinity() error = %v, want nil", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, corrupt) {
			t.Fatalf("未开启亲和时不应改写绑定文件: err=%v contents=%q", err, got)
		}
	})

	t.Run("cache-dir-empty", func(t *testing.T) {
		p, err := NewManagedPool(PoolConfig{
			ManagementKey: mgmtKey, CredentialsDir: t.TempDir(), SessionAffinityTTL: time.Hour,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		p.affinity.bind("session", "alice", time.Now())
		if err := p.SaveSessionAffinity(); err != nil {
			t.Fatalf("CacheDir 为空时 SaveSessionAffinity() error = %v, want nil", err)
		}
		// 守卫：不应把绑定文件写到工作目录（测试进程的当前目录）。
		if _, err := os.Stat("session_affinity.json"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("CacheDir 为空时不应写入绑定文件: stat error = %v", err)
		}
	})
}

// TestNewManagedPoolWarnsAndStartsEmptyOnCorruptAffinityFile 绑定文件损坏时 NewManagedPool 仍然成功，
// 绑定表以空表启动，输出含“会话亲和”的 Warn 日志，并且池可以正常处理请求。
func TestNewManagedPoolWarnsAndStartsEmptyOnCorruptAffinityFile(t *testing.T) {
	up := newMgmtUpstream(t)
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "session_affinity.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	p, err := NewManagedPool(PoolConfig{
		CacheDir: cacheDir, Client: up.client, SessionAffinityTTL: DefaultSessionAffinityTTL,
		Logger: affinityPersistLogger(&logs, slog.LevelWarn),
	}, []PoolAccount{{Token: "oauth-alice", User: "alice"}})
	if err != nil {
		t.Fatalf("绑定文件损坏不应导致启动失败: %v", err)
	}
	if got := p.affinity.count(); got != 0 {
		t.Fatalf("绑定文件损坏时 count() = %d, want 0", got)
	}
	if warnings := affinityPersistWarnings(t, logs.Bytes()); len(warnings) == 0 {
		t.Fatalf("绑定文件损坏时没有含“会话亲和”的 Warn 日志: %s", logs.String())
	}
	if res := poolRequest(p, "POST", "/v1/chat/completions", affinityChatBody(t, "after corrupt file")); res.Code != http.StatusOK {
		t.Fatalf("空表启动后的请求 status=%d body=%s", res.Code, res.Body.String())
	}
}
