package copilot

// 本文件覆盖进入冷却时的副作用：清空该账号的 session 与模型列表缓存（内存与磁盘）、
// 删除指向该账号的会话绑定、保留额度缓存、请求统计与 cooldown.json，以及每次进入冷却输出一条 Warn 日志。

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCooldownEntryClearsSessionAndModelCaches 进入冷却时清空该账号的 session 与模型列表缓存（内存与磁盘），
// 同时保留额度缓存（quota.json）、请求统计与 cooldown.json。
func TestCooldownEntryClearsSessionAndModelCaches(t *testing.T) {
	f := newCooldownFixture(t, "a")
	p := f.start()
	a := p.findAccount("a")
	// 列表会拉取一次额度；之后的错误触发的异步刷新受节流限制，额度不会因此变化。
	mgmtAccounts(t, p)
	if w := cooldownChat(p); w.Code != http.StatusOK {
		t.Fatalf("前提请求 status=%d body=%s", w.Code, w.Body.String())
	}

	sessionPath := a.service.cacheFile("session")
	modelsPath := a.service.cacheFile("models")
	if a.service.session.Token == "" || len(a.service.models.Data) == 0 {
		t.Fatal("前提不成立：成功请求后应有内存中的 session 与模型列表")
	}
	for _, path := range []string{sessionPath, modelsPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("前提不成立：成功请求后应有磁盘缓存 %s: %v", path, err)
		}
	}
	quotaBefore := cooldownView(t, p, "a").Quota
	if quotaBefore == nil {
		t.Fatal("前提不成立：账号应有额度缓存")
	}

	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	if w := cooldownChat(p); w.Code != http.StatusUnauthorized {
		t.Fatalf("触发冷却的请求 status=%d, want 401", w.Code)
	}
	if cooldownView(t, p, "a").CooldownUntil == nil {
		t.Fatal("401 后账号应冷却")
	}

	// 清理是异步副作用：等待记录写入后，内存与磁盘的 session、模型列表最终被清空。
	cooldownAwaitRecord(t, p, "a")
	cooldownEventually(t, "内存中的 session 与模型列表清空", func() bool {
		return cooldownMemorySession(a) == "" && cooldownMemoryModelsCleared(a)
	})
	cooldownEventually(t, "磁盘上的 session 与模型列表清空", func() bool {
		return !cooldownSessionFileHasToken(sessionPath) && !cooldownModelsFileHasData(modelsPath)
	})

	if _, err := os.Stat(a.service.cacheFile("quota")); err != nil {
		t.Fatalf("冷却后额度缓存 quota.json 应保留: %v", err)
	}
	if _, err := os.Stat(a.service.cacheFile("cooldown")); err != nil {
		t.Fatalf("清空缓存不得删除 cooldown.json: %v", err)
	}
	v := cooldownView(t, p, "a")
	persistSameQuota(t, v.Quota, quotaBefore)
	if v.Stats.Requests != 2 || v.Stats.Errors != 1 {
		t.Fatalf("冷却后统计 requests=%d errors=%d, want 2 与 1（统计保留）", v.Stats.Requests, v.Stats.Errors)
	}
}

// TestCooldownEntryRemovesAffinityBindingsOfCooledAccount 进入冷却时删除指向该账号的全部会话绑定，
// 指向其他账号的绑定保留；随后保存的亲和表文件中也不再包含该账号的绑定。
func TestCooldownEntryRemovesAffinityBindingsOfCooledAccount(t *testing.T) {
	f := newCooldownFixture(t, "a", "b")
	f.config.SessionAffinityTTL = DefaultSessionAffinityTTL
	p := f.start()

	body := affinityChatBody(t, "cooldown binding")
	key := affinityPoolKey(t, body)
	now := time.Now()
	p.affinity.bind(key, "a", now)
	p.affinity.bind("extra-bound-to-a", "a", now)
	p.affinity.bind("kept-bound-to-b", "b", now)

	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	if w := poolRequest(p, "POST", "/v1/chat/completions", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("绑定到 a 的请求 status=%d body=%s, want 401", w.Code, w.Body.String())
	}
	if cooldownView(t, p, "a").CooldownUntil == nil {
		t.Fatal("401 后账号应冷却")
	}

	// 删除绑定是异步副作用：等待其完成。
	cooldownEventually(t, "指向 a 的会话绑定删除", func() bool {
		_, okKey := p.affinity.lookup(key, time.Now())
		_, okExtra := p.affinity.lookup("extra-bound-to-a", time.Now())
		return !okKey && !okExtra && p.affinity.activeCounts(time.Now())["a"] == 0
	})
	if id, ok := p.affinity.lookup("kept-bound-to-b", time.Now()); !ok || id != "b" {
		t.Fatalf("指向 b 的绑定应保留，实际 id=%q ok=%v", id, ok)
	}

	if err := p.SaveSessionAffinity(); err != nil {
		t.Fatal(err)
	}
	var file affinityFile
	if err := readCacheFile(filepath.Join(p.config.CacheDir, sessionAffinityFileName), &file); err != nil {
		t.Fatalf("读取亲和表文件: %v", err)
	}
	kept := false
	for _, entry := range file.Bindings {
		if entry.Account == "a" {
			t.Fatalf("保存的亲和表仍含指向 a 的绑定 %q", entry.Key)
		}
		if entry.Key == "kept-bound-to-b" && entry.Account == "b" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("保存的亲和表应保留指向 b 的绑定")
	}
}

// TestCooldownEntryWarnsOncePerCooldown 每次进入冷却输出恰好一条 Warn 日志：含账号、原因与截止时间，不含 OAuth token。
func TestCooldownEntryWarnsOncePerCooldown(t *testing.T) {
	logs := captureJSONSlog(t, slog.LevelWarn)
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusForbidden, cooldownBody(403)})
	p := f.start()
	cooldownChat(p)

	cooldownEventually(t, "进入冷却的 Warn 日志", func() bool { return len(cooldownWarnings(t, logs.Bytes())) >= 1 })
	warnings := cooldownWarnings(t, logs.Bytes())
	if len(warnings) != 1 {
		t.Fatalf("进入冷却的 Warn 日志 %d 条, want 1；输出: %s", len(warnings), logs.String())
	}
	entry := warnings[0]
	if !cooldownLogValueIs(entry, "403") {
		t.Errorf("冷却日志缺少原因 403: %v", entry)
	}
	if !cooldownLogHasDeadline(entry) {
		t.Errorf("冷却日志缺少截止时间（RFC3339）: %v", entry)
	}
	if !cooldownLogNamesAccount(entry, 0, "a") {
		t.Errorf("冷却日志未带账号信息: %v", entry)
	}
	if strings.Contains(logs.String(), "oauth-a") {
		t.Errorf("日志包含 OAuth token: %s", logs.String())
	}
}
