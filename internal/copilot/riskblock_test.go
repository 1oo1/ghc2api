package copilot

// 风控拦截（riskblock）的测试：记录触发 401/403 冷却时的会话亲和键与状态码，之后同键请求直接返回该状态码
// 与固定错误消息，不选账号、不访问上游；记录持久化到 <CacheDir>/risk_blocks.json，每条有效期 2 天，重启后加载。
// 单元部分直接检验 riskTable；集成部分经 HTTP 入口与测试上游观察行为。
//
// 实现者必须提供的新内部名字（测试按此书写，见 riskblock.go）：
//   - const riskBlockTTL = 48 * time.Hour：每条记录的有效期（截止时刻及之后视为不存在，命中不续期）。
//   - const riskBlockMessage = "别再重试了，已经触发风控了"：拦截时写给客户端的错误消息。
//   - const riskBlockFileName = "risk_blocks.json"：记录文件名，位于 PoolConfig.CacheDir 根目录（与 session_affinity.json 同级）。
//   - type riskTable、func newRiskTable() *riskTable，方法 add(key string, status int, now time.Time)、
//     lookup(key string, now time.Time) (int, bool)、save(path string, now time.Time) error、
//     load(path string, now time.Time) error，全部并发安全。
//   - Pool.risk *riskTable：仅在开启会话亲和（SessionAffinityTTL > 0）时创建，关闭亲和时为 nil；
//     CacheDir 非空时池创建（newPool）即加载记录文件，加载失败输出含「风控」的 Warn，以空表启动。
//
// 文件格式固定为 {"version":1,"blocks":[{"key":"...","status":403,"expires":"RFC3339"}]}，权限 0600（经 writeCacheFile 原子写入）。
// 记录时机与拦截时机见 docs/pool.md「风控记录」。

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// riskWantMessage 是拦截响应必须返回的错误消息（规格固定）。
const riskWantMessage = "别再重试了，已经触发风控了"

// riskNow 返回 riskTable 单元测试的基准时刻（整秒，避免持久化序列化的精度差异）。
func riskNow() time.Time {
	return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
}

// riskTestFile 是 risk_blocks.json 的持久化结构（字段名与实现一致）。
type riskTestFile struct {
	Version int             `json:"version"`
	Blocks  []riskTestEntry `json:"blocks"`
}

// riskTestEntry 是记录文件中的一条风控记录。
type riskTestEntry struct {
	Key     string    `json:"key"`
	Status  int       `json:"status"`
	Expires time.Time `json:"expires"`
}

// riskMustHit 断言 key 在 now 时命中且状态码为 status。
func riskMustHit(t *testing.T, table *riskTable, key string, now time.Time, status int) {
	t.Helper()
	got, ok := table.lookup(key, now)
	if !ok || got != status {
		t.Fatalf("lookup(%q, %s) = (%d, %v), want (%d, true)", key, now, got, ok, status)
	}
}

// riskMustMiss 断言 key 在 now 时未命中。
func riskMustMiss(t *testing.T, table *riskTable, key string, now time.Time) {
	t.Helper()
	if got, ok := table.lookup(key, now); ok {
		t.Fatalf("lookup(%q, %s) = (%d, true), want 未命中", key, now, got)
	}
}

// riskReadFile 读取并解析记录文件；文件不存在或不是合法 JSON 时终止测试。
func riskReadFile(t *testing.T, path string) riskTestFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	var file riskTestFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("%s 不是合法的记录文件: %v; contents=%s", path, err, data)
	}
	return file
}

// riskWriteFile 以记录文件格式写入 blocks；version 可指定，用于构造版本不符的文件。
func riskWriteFile(t *testing.T, path string, version int, blocks []riskTestEntry) {
	t.Helper()
	data, err := json.Marshal(riskTestFile{Version: version, Blocks: blocks})
	if err != nil {
		t.Fatal(err)
	}
	cooldownWriteFile(t, path, string(data))
}

// riskFilePath 返回缓存根目录下的记录文件路径（与 session_affinity.json 同级）。
func riskFilePath(f *cooldownFixture) string {
	return filepath.Join(f.config.CacheDir, riskBlockFileName)
}

// riskSessionKey 计算 riskChat 发送的请求对应的会话键（X-Session-ID 信号，携带 testAPIKey）。
func riskSessionKey(t *testing.T, session string) string {
	t.Helper()
	r := affinityRequest("/v1/chat/completions", testAPIKey, "")
	r.Header.Set("X-Session-ID", session)
	key := sessionKey(r, []byte(poolChatBody), "/chat/completions")
	affinityAssertKeyShape(t, key)
	return key
}

// riskChat 发送带 X-Session-ID 会话信号的 chat 请求（信号使会话键非空）。
func riskChat(p *Pool, session string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(poolChatBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Session-ID", session)
	w := httptest.NewRecorder()
	withTestAPIKey(p.Handler()).ServeHTTP(w, r)
	return w
}

// riskAssertBlocked 断言响应是风控拦截：状态码为 status，正文为 writeError 格式且消息为固定的风控提示。
func riskAssertBlocked(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("拦截响应 status=%d body=%s, want %d", w.Code, w.Body.String(), status)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("拦截响应不是 JSON: %v; body=%s", err, w.Body.String())
	}
	if body.Error.Message != riskWantMessage || body.Error.Type != "copilot_proxy_error" {
		t.Fatalf("拦截响应 error = %+v, want message=%q type=copilot_proxy_error", body.Error, riskWantMessage)
	}
}

// TestRiskTableAddLookup add 写入的记录可由 lookup 按状态码取回；不同键互不影响，未记录的键未命中。
func TestRiskTableAddLookup(t *testing.T) {
	table := newRiskTable()
	now := riskNow()
	riskMustMiss(t, table, "unknown", now)

	table.add("k403", http.StatusForbidden, now)
	riskMustHit(t, table, "k403", now, http.StatusForbidden)

	table.add("k401", http.StatusUnauthorized, now)
	riskMustHit(t, table, "k401", now, http.StatusUnauthorized)
	riskMustHit(t, table, "k403", now, http.StatusForbidden)
	riskMustMiss(t, table, "k404", now)
}

// TestRiskTableLookupExpiresAtDeadline 有效期为 2 天：截止时刻之前命中，截止时刻及之后（含边界）视为不存在。
func TestRiskTableLookupExpiresAtDeadline(t *testing.T) {
	table := newRiskTable()
	now := riskNow()
	table.add("k", http.StatusForbidden, now)

	riskMustHit(t, table, "k", now, http.StatusForbidden)
	riskMustHit(t, table, "k", now.Add(47*time.Hour), http.StatusForbidden)
	riskMustMiss(t, table, "k", now.Add(48*time.Hour)) // 截止时刻即失效
	riskMustMiss(t, table, "k", now.Add(49*time.Hour))
}

// TestRiskTableAddOverwritesExistingEntry 同一键重复 add 覆盖状态码，并把截止时刻重置为新的 now+2 天。
func TestRiskTableAddOverwritesExistingEntry(t *testing.T) {
	table := newRiskTable()
	t0 := riskNow()
	table.add("k", http.StatusUnauthorized, t0)

	t1 := t0.Add(time.Hour)
	table.add("k", http.StatusForbidden, t1)
	riskMustHit(t, table, "k", t1, http.StatusForbidden)

	// 覆盖后截止时刻从 t1 重新计算：旧截止时刻（t0+2 天）时仍有效，t1+2 天时失效。
	riskMustHit(t, table, "k", t0.Add(riskBlockTTL), http.StatusForbidden)
	riskMustMiss(t, table, "k", t1.Add(riskBlockTTL))
}

// TestRiskTableLookupDoesNotRenew 命中不续期：截止时刻仍按 add 的时刻计算。
func TestRiskTableLookupDoesNotRenew(t *testing.T) {
	table := newRiskTable()
	t0 := riskNow()
	table.add("k", http.StatusForbidden, t0)

	riskMustHit(t, table, "k", t0.Add(riskBlockTTL-time.Hour), http.StatusForbidden) // 命中
	riskMustHit(t, table, "k", t0.Add(riskBlockTTL-time.Second), http.StatusForbidden)
	riskMustMiss(t, table, "k", t0.Add(riskBlockTTL)) // 命中没有把截止时刻推后
}

// TestRiskTableSaveLoadRoundTrip save/load 往返后恢复状态码，未超过封顶的截止时刻原样恢复
// （k401 截止于 now+49h，load 时被封顶为 now+riskBlockTTL，封顶规则见 TestRiskTableLoadCapsExpiresAtTTL）；
// 文件格式固定为 {"version":1,"blocks":[{"key","status","expires"}]}。
func TestRiskTableSaveLoadRoundTrip(t *testing.T) {
	table := newRiskTable()
	now := riskNow()
	table.add("k403", http.StatusForbidden, now)
	table.add("k401", http.StatusUnauthorized, now.Add(time.Hour))

	path := filepath.Join(t.TempDir(), riskBlockFileName)
	if err := table.save(path, now); err != nil {
		t.Fatalf("save() error = %v", err)
	}

	// 文件格式：顶层恰好 version 与 blocks，每条恰好 key、status、expires。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("记录文件不是 JSON 对象: %v; contents=%s", err, data)
	}
	if len(raw) != 2 || raw["version"] == nil || raw["blocks"] == nil {
		t.Fatalf("记录文件顶层字段 = %v, want 恰好 version 与 blocks", raw)
	}
	var version int
	if err := json.Unmarshal(raw["version"], &version); err != nil || version != 1 {
		t.Fatalf("记录文件 version = %s, want 1", raw["version"])
	}
	var rawBlocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw["blocks"], &rawBlocks); err != nil {
		t.Fatalf("blocks 不是数组: %v", err)
	}
	for i, entry := range rawBlocks {
		if len(entry) != 3 || entry["key"] == nil || entry["status"] == nil || entry["expires"] == nil {
			t.Fatalf("blocks[%d] 字段 = %v, want 恰好 key、status、expires", i, entry)
		}
	}

	restored := newRiskTable()
	if err := restored.load(path, now); err != nil {
		t.Fatalf("load() error = %v", err)
	}
	riskMustHit(t, restored, "k403", now, http.StatusForbidden)
	riskMustHit(t, restored, "k401", now, http.StatusUnauthorized)
	riskMustMiss(t, restored, "unknown", now)
	// 截止时刻原样恢复：k403 的截止时刻仍是 add 时的 now+2 天。
	riskMustMiss(t, restored, "k403", now.Add(riskBlockTTL))
}

// TestRiskTableSaveUses0600Permission 记录文件权限为 0600（Windows 不支持 POSIX 权限位，跳过）。
func TestRiskTableSaveUses0600Permission(t *testing.T) {
	table := newRiskTable()
	table.add("k", http.StatusForbidden, riskNow())
	path := filepath.Join(t.TempDir(), riskBlockFileName)
	if err := table.save(path, riskNow()); err != nil {
		t.Fatalf("save() error = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("记录文件权限 = %o, want 600", got)
	}
}

// TestRiskTableSaveSkipsExpired save 只写入 now 时刻仍有效的记录，已过期项不落盘。
func TestRiskTableSaveSkipsExpired(t *testing.T) {
	table := newRiskTable()
	now := riskNow()
	table.add("live", http.StatusForbidden, now.Add(-time.Hour))
	table.add("dead", http.StatusUnauthorized, now.Add(-riskBlockTTL)) // 截止时刻即 now，已失效

	path := filepath.Join(t.TempDir(), riskBlockFileName)
	if err := table.save(path, now); err != nil {
		t.Fatalf("save() error = %v", err)
	}
	file := riskReadFile(t, path)
	if len(file.Blocks) != 1 || file.Blocks[0].Key != "live" {
		t.Fatalf("save 写入的记录 = %+v, want 只有 live", file.Blocks)
	}
}

// TestRiskTableLoadDropsInvalidEntries load 丢弃已过期（含截止时刻恰为 now）的项、key 为空的项与状态码不是 401/403 的项。
func TestRiskTableLoadDropsInvalidEntries(t *testing.T) {
	now := riskNow()
	path := filepath.Join(t.TempDir(), riskBlockFileName)
	riskWriteFile(t, path, 1, []riskTestEntry{
		{Key: "good403", Status: http.StatusForbidden, Expires: now.Add(time.Hour)},
		{Key: "good401", Status: http.StatusUnauthorized, Expires: now.Add(2 * time.Hour)},
		{Key: "expired", Status: http.StatusForbidden, Expires: now.Add(-time.Second)},
		{Key: "at-deadline", Status: http.StatusForbidden, Expires: now}, // 截止时刻即失效
		{Key: "", Status: http.StatusForbidden, Expires: now.Add(time.Hour)},
		{Key: "status429", Status: http.StatusTooManyRequests, Expires: now.Add(time.Hour)},
		{Key: "status500", Status: http.StatusInternalServerError, Expires: now.Add(time.Hour)},
		{Key: "status200", Status: http.StatusOK, Expires: now.Add(time.Hour)},
	})

	table := newRiskTable()
	if err := table.load(path, now); err != nil {
		t.Fatalf("load() error = %v", err)
	}
	riskMustHit(t, table, "good403", now, http.StatusForbidden)
	riskMustHit(t, table, "good401", now, http.StatusUnauthorized)
	for _, key := range []string{"expired", "at-deadline", "", "status429", "status500", "status200"} {
		riskMustMiss(t, table, key, now)
	}
}

// TestRiskTableLoadCapsExpiresAtTTL load 把每条的截止时刻封顶于 now+2 天，文件中的更晚时刻不生效。
func TestRiskTableLoadCapsExpiresAtTTL(t *testing.T) {
	now := riskNow()
	path := filepath.Join(t.TempDir(), riskBlockFileName)
	riskWriteFile(t, path, 1, []riskTestEntry{
		{Key: "far", Status: http.StatusForbidden, Expires: now.Add(100 * time.Hour)},
	})

	table := newRiskTable()
	if err := table.load(path, now); err != nil {
		t.Fatalf("load() error = %v", err)
	}
	riskMustHit(t, table, "far", now.Add(riskBlockTTL-time.Second), http.StatusForbidden)
	riskMustMiss(t, table, "far", now.Add(riskBlockTTL))
}

// TestRiskTableLoadMissingFileYieldsEmptyTable 文件不存在时 load 返回 nil，表被清空（首次启动即此状态）。
func TestRiskTableLoadMissingFileYieldsEmptyTable(t *testing.T) {
	table := newRiskTable()
	table.add("k", http.StatusForbidden, riskNow())

	path := filepath.Join(t.TempDir(), riskBlockFileName)
	if err := table.load(path, riskNow()); err != nil {
		t.Fatalf("文件不存在时 load() error = %v, want nil", err)
	}
	riskMustMiss(t, table, "k", riskNow())
}

// TestRiskTableLoadErrorKeepsTableUnchanged 文件损坏或版本不符时 load 返回错误，表保持加载前的内容不变。
func TestRiskTableLoadErrorKeepsTableUnchanged(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		now := riskNow()
		path := filepath.Join(t.TempDir(), riskBlockFileName)
		cooldownWriteFile(t, path, "{not json")

		table := newRiskTable()
		table.add("keep", http.StatusUnauthorized, now)
		if err := table.load(path, now); err == nil {
			t.Fatal("文件损坏时 load() error = nil, want 错误")
		}
		riskMustHit(t, table, "keep", now, http.StatusUnauthorized) // 表不变
		riskMustMiss(t, table, "fresh", now)                        // 文件内容未混入
	})

	t.Run("version", func(t *testing.T) {
		now := riskNow()
		path := filepath.Join(t.TempDir(), riskBlockFileName)
		riskWriteFile(t, path, 2, []riskTestEntry{
			{Key: "fresh", Status: http.StatusForbidden, Expires: now.Add(time.Hour)},
		})

		table := newRiskTable()
		table.add("keep", http.StatusUnauthorized, now)
		if err := table.load(path, now); err == nil {
			t.Fatal("版本不符时 load() error = nil, want 错误")
		}
		riskMustHit(t, table, "keep", now, http.StatusUnauthorized) // 表不变
		riskMustMiss(t, table, "fresh", now)                        // 文件内容未混入
	})
}

// TestRiskBlockRecordedOnGenerationAuthFailure 带会话信号的生成请求因 401/403 使账号本次进入冷却时，
// 记录会话键→状态码，并在请求返回前把记录写入 <CacheDir>/risk_blocks.json。
func TestRiskBlockRecordedOnGenerationAuthFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newCooldownFixture(t, "a", "b")
			f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
			p := f.start()
			cooldownSetGeneration(f.up, "oauth-a", status)

			before := time.Now()
			w := riskChat(p, "record-gen")
			after := time.Now()
			if w.Code != status || w.Body.String() != cooldownBody(status) {
				t.Fatalf("触发冷却的请求 status=%d body=%s, want 原样返回 %d", w.Code, w.Body.String(), status)
			}
			if v := cooldownView(t, p, "a"); v.CooldownUntil == nil {
				t.Fatal("401/403 后账号应冷却")
			}

			key := riskSessionKey(t, "record-gen")
			riskMustHit(t, p.risk, key, after, status)

			// 记录文件在请求返回前已同步写入：不需要轮询。
			file := riskReadFile(t, riskFilePath(f))
			if len(file.Blocks) != 1 {
				t.Fatalf("记录文件条数 = %d, want 1: %+v", len(file.Blocks), file.Blocks)
			}
			entry := file.Blocks[0]
			if entry.Key != key || entry.Status != status {
				t.Fatalf("记录文件条目 = %+v, want key=%q status=%d", entry, key, status)
			}
			cooldownAssertWindow(t, entry.Expires, before, after, riskBlockTTL)
		})
	}
}

// TestRiskBlockRejectsRetryOfRecordedSession 命中记录的同键请求直接返回记录的状态码与固定消息：
// 上游不再收到请求（生成次数不增加），其他账号也未被选，且不计入任何账号的请求数。
func TestRiskBlockRejectsRetryOfRecordedSession(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	p := f.start()
	cooldownSetGeneration(f.up, "oauth-a", http.StatusForbidden)

	if w := riskChat(p, "retry-block"); w.Code != http.StatusForbidden {
		t.Fatalf("触发请求 status=%d body=%s, want 403", w.Code, w.Body.String())
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil {
		t.Fatal("前提不成立：403 后账号应冷却")
	}

	riskAssertBlocked(t, riskChat(p, "retry-block"), http.StatusForbidden)
	assertGenerationOrder(t, f.up.pool, mgmtOrder("a")...) // 上游只有触发那一次，b 未被选
	if v := cooldownView(t, p, "a"); v.Stats.Requests != 1 || v.Stats.Errors != 1 {
		t.Fatalf("拦截请求计入了 a 的统计: requests=%d errors=%d", v.Stats.Requests, v.Stats.Errors)
	}
	if v := cooldownView(t, p, "b"); v.Stats.Requests != 0 {
		t.Fatalf("拦截请求计入了 b 的统计: requests=%d", v.Stats.Requests)
	}
}

// TestRiskBlockDifferentSessionAndModelListUnaffected 不同会话键的请求与模型列表等无会话键的请求不受记录影响，照常处理。
func TestRiskBlockDifferentSessionAndModelListUnaffected(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	p := f.start()
	cooldownSetGeneration(f.up, "oauth-a", http.StatusForbidden)

	if w := riskChat(p, "blocked-session"); w.Code != http.StatusForbidden {
		t.Fatalf("触发请求 status=%d body=%s, want 403", w.Code, w.Body.String())
	}

	// 不同键不受影响：a 已冷却，改由 b 处理。
	if w := riskChat(p, "other-session"); w.Code != http.StatusOK {
		t.Fatalf("不同会话键请求 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	// 无会话键的模型列表不受影响。
	if w := poolRequest(p, "GET", "/v1/models", ""); w.Code != http.StatusOK {
		t.Fatalf("模型列表 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	assertGenerationOrder(t, f.up.pool, mgmtOrder("a", "b")...)
	// 原键仍被拦截。
	riskAssertBlocked(t, riskChat(p, "blocked-session"), http.StatusForbidden)
}

// TestRiskBlockRecordedOnSessionExchangeAuthFailure pickSession 中 session 交换返回 401/403 并使账号进入冷却时同样记录；
// 之后同键请求被拦截，不再发起 session 交换。
func TestRiskBlockRecordedOnSessionExchangeAuthFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
			f.up.pool.failSession("oauth-a", poolFailure{status, cooldownBody(status)})
			p := f.start()

			w := riskChat(p, "record-sess")
			if w.Code != status || w.Body.String() != cooldownBody(status) {
				t.Fatalf("session 失败的请求 status=%d body=%s, want 原样返回 %d", w.Code, w.Body.String(), status)
			}
			if v := cooldownView(t, p, "a"); v.CooldownUntil == nil {
				t.Fatalf("%d 后账号应冷却", status)
			}
			key := riskSessionKey(t, "record-sess")
			riskMustHit(t, p.risk, key, time.Now(), status)

			// 记录文件在请求返回前已同步写入。
			file := riskReadFile(t, riskFilePath(f))
			if len(file.Blocks) != 1 || file.Blocks[0].Key != key || file.Blocks[0].Status != status {
				t.Fatalf("记录文件 = %+v, want 1 条 key=%q status=%d 的记录", file.Blocks, key, status)
			}

			before := f.up.pool.sessionCount("oauth-a")
			riskAssertBlocked(t, riskChat(p, "record-sess"), status)
			if got := f.up.pool.sessionCount("oauth-a"); got != before {
				t.Fatalf("拦截请求仍发起了 session 交换：%d -> %d", before, got)
			}
		})
	}
}

// TestRiskBlockNotRecordedOnStreakCooldown 429/5xx 连续 3 次触发的冷却不记录：同键请求不会被拦截（没有可用账号时返回 503）。
func TestRiskBlockNotRecordedOnStreakCooldown(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newCooldownFixture(t, "a")
			f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
			p := f.start()
			cooldownSetGeneration(f.up, "oauth-a", status)

			for i := 1; i <= 3; i++ {
				if w := riskChat(p, "streak-sess"); w.Code != status {
					t.Fatalf("第 %d 次请求 status=%d body=%s, want %d", i, w.Code, w.Body.String(), status)
				}
			}
			if v := cooldownView(t, p, "a"); v.CooldownUntil == nil {
				t.Fatal("连续 3 次后账号应冷却")
			}
			riskMustMiss(t, p.risk, riskSessionKey(t, "streak-sess"), time.Now())
			assertNoAccount(t, riskChat(p, "streak-sess")) // 未被拦截，只是没有可用账号
		})
	}
}

// TestRiskBlockNotDuplicatedWhileAccountAlreadyCooling 两个带不同会话键的请求同时在途、因同一 403 使账号进入冷却时：
// 只有触发本次冷却的那一个会话键被记录，另一个请求到达时账号已在冷却中（未触发新冷却），不再记录。
func TestRiskBlockNotDuplicatedWhileAccountAlreadyCooling(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	p := f.start()
	// 先用成功的请求预热模型列表缓存：否则首个生成请求会先刷新模型列表，
	// 与生成请求共用同一上游阻塞点，第 2 个请求会被模型锁挡在上游之前。
	if w := riskChat(p, "warmup-sess"); w.Code != http.StatusOK {
		t.Fatalf("预热请求 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	hold := f.up.holdGenerations("oauth-a")

	// 两个请求依次到达生成上游并阻塞；每到一个消费一次 hold.started，避免缓冲的信号被覆盖而漏等。
	t.Cleanup(func() {
		// 失败路径下也要让在途请求结束，避免 TempDir 清理与请求写文件交错。
		hold.free()
	})
	firstRes := make(chan *httptest.ResponseRecorder, 1)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		firstRes <- riskChat(p, "cooling-1")
	}()
	reviewAwait(t, hold.started, "第 1 个请求未到达生成上游")

	secondRes := make(chan *httptest.ResponseRecorder, 1)
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		secondRes <- riskChat(p, "cooling-2")
	}()
	reviewAwait(t, hold.started, "第 2 个请求未到达生成上游")

	cooldownSetGeneration(f.up, "oauth-a", http.StatusForbidden)
	hold.free()

	reviewAwait(t, firstDone, "第 1 个请求未结束")
	reviewAwait(t, secondDone, "第 2 个请求未结束")
	for i, res := range []*httptest.ResponseRecorder{<-firstRes, <-secondRes} {
		if res.Code != http.StatusForbidden {
			t.Fatalf("第 %d 个请求 status=%d body=%s, want 403", i+1, res.Code, res.Body.String())
		}
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil {
		t.Fatal("403 后账号应冷却")
	}

	_, ok1 := p.risk.lookup(riskSessionKey(t, "cooling-1"), time.Now())
	_, ok2 := p.risk.lookup(riskSessionKey(t, "cooling-2"), time.Now())
	if ok1 == ok2 {
		t.Fatalf("应恰好记录一个会话键: cooling-1=%v cooling-2=%v", ok1, ok2)
	}
}

// TestRiskBlockSurvivesRestart 记录持久化后用同一 CacheDir 重建池（重启）：同键请求仍被拦截，不再访问上游。
func TestRiskBlockSurvivesRestart(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	p := f.start()
	cooldownSetGeneration(f.up, "oauth-a", http.StatusForbidden)

	if w := riskChat(p, "restart-sess"); w.Code != http.StatusForbidden {
		t.Fatalf("触发请求 status=%d body=%s, want 403", w.Code, w.Body.String())
	}
	cooldownSettle(p) // 等待冷却副作用结束，避免与重启后的加载交错

	restarted := f.start()
	key := riskSessionKey(t, "restart-sess")
	riskMustHit(t, restarted.risk, key, time.Now(), http.StatusForbidden)
	riskAssertBlocked(t, riskChat(restarted, "restart-sess"), http.StatusForbidden)
	assertGenerationOrder(t, f.up.pool, mgmtOrder("a")...)
}

// TestRiskBlockExpiredRecordNotLoadedAfterRestart 加载时丢弃已过期的记录：过期项不再拦截，文件中的有效项照常拦截。
func TestRiskBlockExpiredRecordNotLoadedAfterRestart(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	expiredKey := riskSessionKey(t, "expired-sess")
	validKey := riskSessionKey(t, "valid-sess")
	riskWriteFile(t, riskFilePath(f), 1, []riskTestEntry{
		{Key: expiredKey, Status: http.StatusForbidden, Expires: time.Now().Add(-time.Minute)},
		{Key: validKey, Status: http.StatusUnauthorized, Expires: time.Now().Add(time.Hour)},
	})

	p := f.start()
	riskMustMiss(t, p.risk, expiredKey, time.Now())
	riskMustHit(t, p.risk, validKey, time.Now(), http.StatusUnauthorized)

	if w := riskChat(p, "expired-sess"); w.Code != http.StatusOK {
		t.Fatalf("过期记录不应拦截: status=%d body=%s", w.Code, w.Body.String())
	}
	riskAssertBlocked(t, riskChat(p, "valid-sess"), http.StatusUnauthorized)
	assertGenerationOrder(t, f.up.pool, mgmtOrder("a")...)
}

// TestRiskBlockDisabledWithoutSessionAffinity 关闭会话亲和时不创建风控表：401/403 仍冷却账号，但不拦截也不记录。
func TestRiskBlockDisabledWithoutSessionAffinity(t *testing.T) {
	f := newCooldownFixture(t, "a") // SessionAffinityTTL 保持 0：关闭亲和
	p := f.start()
	if p.risk != nil {
		t.Fatal("关闭亲和时不应创建风控表")
	}
	cooldownSetGeneration(f.up, "oauth-a", http.StatusForbidden)

	if w := riskChat(p, "no-affinity"); w.Code != http.StatusForbidden {
		t.Fatalf("触发请求 status=%d body=%s, want 403", w.Code, w.Body.String())
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil {
		t.Fatal("403 后账号仍应冷却（冷却不依赖会话亲和）")
	}
	// 再次请求：无会话键故不拦截，只是账号冷却后没有可用账号。
	assertNoAccount(t, riskChat(p, "no-affinity"))
	if _, err := os.Stat(riskFilePath(f)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("关闭亲和时不应写入记录文件: stat error = %v", err)
	}
}

// TestRiskBlockCorruptFileWarnsAndStartsEmpty 记录文件损坏时池照常启动：输出含「风控」的 Warn，以空表处理请求。
func TestRiskBlockCorruptFileWarnsAndStartsEmpty(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	var logs bytes.Buffer
	f.config.Logger = affinityPersistLogger(&logs, slog.LevelWarn)
	cooldownWriteFile(t, riskFilePath(f), "{not json")

	p := f.start()
	found := false
	for _, entry := range jsonLogEntries(t, logs.Bytes()) {
		msg, _ := entry["msg"].(string)
		if entry["level"] == "WARN" && strings.Contains(msg, "风控") {
			found = true
		}
	}
	if !found {
		t.Fatalf("记录文件损坏时没有含「风控」的 Warn 日志: %s", logs.String())
	}

	if w := riskChat(p, "corrupt-boot"); w.Code != http.StatusOK {
		t.Fatalf("空表启动后的请求 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	riskMustMiss(t, p.risk, riskSessionKey(t, "corrupt-boot"), time.Now())
}

// TestRiskBlockKeptAfterManualCooldownLift 手动解除账号冷却不会清除风控记录：解除后同键请求仍被拦截。
func TestRiskBlockKeptAfterManualCooldownLift(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	p := f.start()
	cooldownSetGeneration(f.up, "oauth-a", http.StatusForbidden)

	if w := riskChat(p, "lift-sess"); w.Code != http.StatusForbidden {
		t.Fatalf("触发请求 status=%d body=%s, want 403", w.Code, w.Body.String())
	}
	cooldownDecodeView(t, cooldownPatch(p, `{"id":"a","clear_cooldown":true}`))
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
		t.Fatalf("解除后仍冷却至 %s", v.CooldownUntil)
	}

	riskAssertBlocked(t, riskChat(p, "lift-sess"), http.StatusForbidden)
	riskMustHit(t, p.risk, riskSessionKey(t, "lift-sess"), time.Now(), http.StatusForbidden)
	assertGenerationOrder(t, f.up.pool, mgmtOrder("a")...)
}
