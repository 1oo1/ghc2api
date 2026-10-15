package copilot

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// affinityLogClientKey 是亲和日志测试请求携带的客户端密钥（Bearer），日志中不得出现。
const affinityLogClientKey = "affinity-log-client-secret"

// affinityLogPool 创建 alice、bob 两个账号的托管池，日志以 JSON 写入返回的缓冲区，级别为 level。
// 两个账号都没有额度与优先级差异，按凭证文件名排序，alice 最小，因此首个请求由 alice 处理。
func affinityLogPool(t *testing.T, up *mgmtUpstream, level slog.Level, ttl time.Duration) (*Pool, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	var accounts []PoolAccount
	for _, user := range []string{"alice", "bob"} {
		token := "oauth-" + user
		if err := SaveCredential(filepath.Join(dir, user+".json"), Credential{Token: token, User: user}); err != nil {
			t.Fatal(err)
		}
		up.setLogin(token, user)
		accounts = append(accounts, PoolAccount{Token: token, User: user})
	}
	logs := &bytes.Buffer{}
	p, err := NewManagedPool(PoolConfig{
		ManagementKey: mgmtKey, CredentialsDir: dir, CacheDir: t.TempDir(), Client: up.client,
		SessionAffinityTTL: ttl, Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: level})),
	}, accounts)
	if err != nil {
		t.Fatal(err)
	}
	return p, logs
}

// affinityLogChat 发送带 Bearer 客户端密钥的 chat 请求，返回响应。
func affinityLogChat(t *testing.T, p *Pool, body string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+affinityLogClientKey)
	w := httptest.NewRecorder()
	p.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("生成请求 status=%d body=%s", w.Code, w.Body.String())
	}
}

// affinityLogSessionKey 计算与 affinityLogChat 请求对应的完整会话键。
func affinityLogSessionKey(t *testing.T, body string) string {
	t.Helper()
	key := sessionKey(affinityRequest("/v1/chat/completions", affinityLogClientKey, ""), []byte(body), "/chat/completions")
	affinityAssertKeyShape(t, key)
	return key
}

// affinityLogEntries 返回消息以“会话亲和”开头的日志条目，按输出顺序。
func affinityLogEntries(t *testing.T, output []byte) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, entry := range jsonLogEntries(t, output) {
		if msg, _ := entry["msg"].(string); strings.HasPrefix(msg, "会话亲和") {
			entries = append(entries, entry)
		}
	}
	return entries
}

// affinityLogDump 把亲和日志条目序列化为一行文本，供失败信息使用（不输出无关的调试日志）。
func affinityLogDump(entries []map[string]any) string {
	data, _ := json.Marshal(entries)
	return string(data)
}

// affinityLogWant 描述一条期望的亲和日志：消息、级别，以及需要比较的属性。
type affinityLogWant struct {
	msg   string
	level string
	attrs map[string]string
}

// assertAffinityLogs 断言亲和日志的条数、顺序、消息、级别与列出的属性。
func assertAffinityLogs(t *testing.T, output []byte, want ...affinityLogWant) {
	t.Helper()
	got := affinityLogEntries(t, output)
	if len(got) != len(want) {
		t.Fatalf("亲和日志 %d 条, want %d: %s", len(got), len(want), affinityLogDump(got))
	}
	for i, w := range want {
		entry := got[i]
		if entry["msg"] != w.msg || entry["level"] != w.level {
			t.Fatalf("第 %d 条亲和日志 msg=%v level=%v, want msg=%q level=%s", i, entry["msg"], entry["level"], w.msg, w.level)
		}
		for key, value := range w.attrs {
			if entry[key] != value {
				t.Fatalf("第 %d 条亲和日志的 %s = %v, want %q", i, key, entry[key], value)
			}
		}
	}
}

// assertAffinityLogsRedacted 断言日志中不出现完整会话键与客户端密钥；亲和日志行也不出现用户消息原文。
func assertAffinityLogsRedacted(t *testing.T, output []byte, key string) {
	t.Helper()
	if strings.Contains(string(output), key) {
		t.Fatalf("日志包含完整会话键: %s", output)
	}
	if strings.Contains(string(output), affinityLogClientKey) {
		t.Fatalf("日志包含客户端密钥: %s", output)
	}
	for _, entry := range affinityLogEntries(t, output) {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(line), "first question") {
			t.Fatalf("亲和日志包含请求正文: %s", line)
		}
	}
}

// TestAffinityLogNewBindingThenHit 首次请求输出 Info「新建绑定」，同会话再次请求输出 Debug「命中」。
// 属性只含会话键前 12 个字符与账号 ID；日志不含完整会话键、客户端密钥或请求正文。
func TestAffinityLogNewBindingThenHit(t *testing.T) {
	up := newMgmtUpstream(t)
	p, logs := affinityLogPool(t, up, slog.LevelDebug, DefaultSessionAffinityTTL)
	body := affinityChatBody(t, "first question")
	key := affinityLogSessionKey(t, body)
	affinityLogChat(t, p, body)
	affinityLogChat(t, p, body)

	prefix := key[:12]
	assertAffinityLogs(t, logs.Bytes(),
		affinityLogWant{msg: "会话亲和：新建绑定", level: "INFO", attrs: map[string]string{"session": prefix, "bound_account": "alice"}},
		affinityLogWant{msg: "会话亲和：命中", level: "DEBUG", attrs: map[string]string{"session": prefix, "bound_account": "alice"}},
	)
	assertAffinityLogsRedacted(t, logs.Bytes(), key)
}

// TestAffinityLogSwitchAccountWhenBindingUnavailable 绑定账号被禁用、删除、冷却或 session 获取失败后，
// 会话切换到其他账号：输出 Info「切换账号」，并记录原账号（from）与新账号（to）；之后同会话命中新账号。
func TestAffinityLogSwitchAccountWhenBindingUnavailable(t *testing.T) {
	for _, reason := range []string{"disabled", "deleted", "cooldown", "session-failure"} {
		t.Run(reason, func(t *testing.T) {
			up := newMgmtUpstream(t)
			up.pool.setSessionTTL(time.Minute) // 会话很快过期，使 session 失败在下一次请求时生效。
			p, logs := affinityLogPool(t, up, slog.LevelDebug, DefaultSessionAffinityTTL)
			body := affinityChatBody(t, "first question")
			key := affinityLogSessionKey(t, body)
			affinityLogChat(t, p, body)

			switch reason {
			case "disabled":
				if w := mgmtCall(p, "PATCH", "/v0/management/accounts", `{"id":"alice","disabled":true}`, mgmtKey); w.Code != http.StatusOK {
					t.Fatalf("禁用 alice status=%d body=%s", w.Code, w.Body.String())
				}
			case "deleted":
				if w := mgmtCall(p, "DELETE", "/v0/management/accounts?id=alice", "", mgmtKey); w.Code != http.StatusOK {
					t.Fatalf("删除 alice status=%d body=%s", w.Code, w.Body.String())
				}
			case "cooldown":
				p.findAccount("alice").service.setCooldown(time.Now().Add(time.Hour))
			case "session-failure":
				up.pool.failSession("oauth-alice", poolFailure{http.StatusInternalServerError, `{"message":"alice down"}`})
			}
			affinityLogChat(t, p, body)
			affinityLogChat(t, p, body)

			prefix := key[:12]
			assertAffinityLogs(t, logs.Bytes(),
				affinityLogWant{msg: "会话亲和：新建绑定", level: "INFO", attrs: map[string]string{"session": prefix, "bound_account": "alice"}},
				affinityLogWant{msg: "会话亲和：切换账号", level: "INFO", attrs: map[string]string{"session": prefix, "from": "alice", "to": "bob"}},
				affinityLogWant{msg: "会话亲和：命中", level: "DEBUG", attrs: map[string]string{"session": prefix, "bound_account": "bob"}},
			)
			assertAffinityLogsRedacted(t, logs.Bytes(), key)
		})
	}
}

// TestAffinityLogAbsentWithoutSessionSignal 没有会话信号（只有 model，或没有 user 消息）的请求不使用亲和，
// 也不输出任何亲和日志。
func TestAffinityLogAbsentWithoutSessionSignal(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{name: "model-only", body: `{"model":"gpt-4.1","messages":[]}`},
		{name: "no-user-message", body: `{"model":"gpt-4.1","messages":[{"role":"assistant","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newMgmtUpstream(t)
			p, logs := affinityLogPool(t, up, slog.LevelDebug, DefaultSessionAffinityTTL)
			affinityLogChat(t, p, tc.body)
			affinityLogChat(t, p, tc.body)

			if !strings.Contains(logs.String(), "收到客户端请求") {
				t.Fatal("没有捕获到请求日志，无法验证没有亲和日志")
			}
			assertAffinityLogs(t, logs.Bytes())
		})
	}
}

// TestAffinityLogAbsentWhenAffinityDisabled SessionAffinityTTL<=0 时关闭亲和，相同会话的请求也不输出任何亲和日志。
func TestAffinityLogAbsentWhenAffinityDisabled(t *testing.T) {
	up := newMgmtUpstream(t)
	p, logs := affinityLogPool(t, up, slog.LevelDebug, 0)
	body := affinityChatBody(t, "first question")
	affinityLogChat(t, p, body)
	affinityLogChat(t, p, body)

	if !strings.Contains(logs.String(), "收到客户端请求") {
		t.Fatal("没有捕获到请求日志，无法验证没有亲和日志")
	}
	assertAffinityLogs(t, logs.Bytes())
}

// TestAffinityHitLogIsHiddenAtInfoLevel 日志级别为 Info 时，Debug 级别的「命中」不输出，「新建绑定」仍然输出。
func TestAffinityHitLogIsHiddenAtInfoLevel(t *testing.T) {
	up := newMgmtUpstream(t)
	p, logs := affinityLogPool(t, up, slog.LevelInfo, DefaultSessionAffinityTTL)
	body := affinityChatBody(t, "first question")
	key := affinityLogSessionKey(t, body)
	affinityLogChat(t, p, body)
	affinityLogChat(t, p, body)

	assertAffinityLogs(t, logs.Bytes(),
		affinityLogWant{msg: "会话亲和：新建绑定", level: "INFO", attrs: map[string]string{"session": key[:12], "bound_account": "alice"}},
	)
}
