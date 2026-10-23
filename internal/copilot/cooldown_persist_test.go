package copilot

// 本文件覆盖冷却的持久化：账号缓存目录下的 cooldown.json 格式与权限、重启后恢复未到期的冷却、
// 忽略已过期或损坏的记录（损坏时输出 Warn）、没有缓存目录时只保存在内存中。

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestCooldownFileLayoutAndPermission cooldown.json 位于账号缓存目录，内容恰为 until（RFC3339）与 reason，权限为 0600。
func TestCooldownFileLayoutAndPermission(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusForbidden, cooldownBody(403)})
	p := f.start()

	before := time.Now()
	cooldownChat(p)
	after := time.Now()
	cooldownAwaitRecord(t, p, "a")

	path := cooldownFile(t, p, "a", "cooldown")
	fields := cooldownFields(t, path)
	if len(fields) != 2 {
		t.Fatalf("cooldown.json 字段 = %d 个, want 恰好 until 与 reason: %v", len(fields), fields)
	}
	var reason string
	if err := json.Unmarshal(fields["reason"], &reason); err != nil || reason != "403" {
		t.Fatalf("cooldown.json 的 reason = %s, want \"403\"", fields["reason"])
	}
	cooldownAssertWindow(t, persistTimeField(t, fields, "until"), before, after, DefaultAccountCooldown)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("cooldown.json 权限 = %o, want 600", got)
		}
	}
}

// TestCooldownRestoredAfterRestart 重启后仍在冷却：截止时间与文件中的一致，原因保留，冷却期内不参与选择。
func TestCooldownRestoredAfterRestart(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusForbidden, cooldownBody(403)})
	before := f.start()
	cooldownChat(before)
	cooldownAwaitRecord(t, before, "a")
	fields := cooldownFields(t, cooldownFile(t, before, "a", "cooldown"))
	wantUntil := persistTimeField(t, fields, "until")

	p := f.start() // 重启
	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil || !v.CooldownUntil.Equal(wantUntil) {
		t.Fatalf("重启后冷却截止时间 = %v, want 文件中的 %s", v.CooldownUntil, wantUntil)
	}
	if v.CooldownReason != "403" {
		t.Fatalf("重启后冷却原因 = %q, want 403", v.CooldownReason)
	}
	calls := len(f.up.pool.generationOrder())
	assertNoAccount(t, cooldownChat(p))
	if got := len(f.up.pool.generationOrder()); got != calls {
		t.Fatalf("重启后的冷却期内账号仍收到了生成请求: %d -> %d", calls, got)
	}
}

// TestCooldownExpiredRecordIgnoredOnStart 已过期的 cooldown.json 启动时被忽略：账号可用，视图中没有冷却与原因。
func TestCooldownExpiredRecordIgnoredOnStart(t *testing.T) {
	f := newCooldownFixture(t, "a")
	p := f.start()
	expired := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	cooldownWriteFile(t, cooldownFile(t, p, "a", "cooldown"), `{"until":"`+expired+`","reason":"403"}`)

	p = f.start()
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil || v.CooldownReason != "" {
		t.Fatalf("已过期的冷却记录仍生效: until=%v reason=%q", v.CooldownUntil, v.CooldownReason)
	}
	if w := cooldownChat(p); w.Code != http.StatusOK {
		t.Fatalf("已过期的冷却记录不应阻止请求，status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestCooldownCorruptRecordIgnoredWithWarning 损坏的 cooldown.json 启动时被忽略并输出 Warn，池照常启动且账号可用。
func TestCooldownCorruptRecordIgnoredWithWarning(t *testing.T) {
	f := newCooldownFixture(t, "a")
	p := f.start()
	cooldownWriteFile(t, cooldownFile(t, p, "a", "cooldown"), `{"until": not-json`)

	logs := captureJSONSlog(t, slog.LevelWarn)
	p = f.start()
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil || v.CooldownReason != "" {
		t.Fatalf("损坏的冷却记录不应生效: until=%v reason=%q", v.CooldownUntil, v.CooldownReason)
	}
	if !cooldownHasWarn(t, logs.Bytes()) {
		t.Fatalf("损坏的冷却记录应输出 Warn 日志；输出: %s", logs.String())
	}
	if w := cooldownChat(p); w.Code != http.StatusOK {
		t.Fatalf("损坏的冷却记录不应阻止请求，status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestCooldownCorruptRecordIsDeletedAfterWarning 损坏的 cooldown.json 启动时记 Warn 并删除，之后再次启动不再告警。
func TestCooldownCorruptRecordIsDeletedAfterWarning(t *testing.T) {
	f := newCooldownFixture(t, "a")
	p := f.start()
	path := cooldownFile(t, p, "a", "cooldown")
	cooldownWriteFile(t, path, `{"until": not-json`)

	logs := captureJSONSlog(t, slog.LevelWarn)
	f.start()
	if !cooldownHasWarn(t, logs.Bytes()) {
		t.Fatalf("损坏的冷却记录应输出 Warn 日志；输出: %s", logs.String())
	}
	cooldownEventually(t, "损坏的 cooldown.json 应被删除", func() bool {
		_, err := os.Stat(path)
		return errors.Is(err, os.ErrNotExist)
	})

	logs = captureJSONSlog(t, slog.LevelWarn)
	f.start()
	if n := len(cooldownWarnings(t, logs.Bytes())); n != 0 {
		t.Fatalf("记录已删除后再次启动仍输出了 %d 条冷却相关告警: %s", n, logs.String())
	}
}

// TestCooldownStaysInMemoryWithoutCacheDir 没有可用的缓存目录时冷却只保存在内存中：同一进程内生效，重启后不恢复。
func TestCooldownStaysInMemoryWithoutCacheDir(t *testing.T) {
	// os.UserCacheDir 依赖 HOME 与 XDG_CACHE_HOME；两者为空时账号没有缓存目录。
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	f := newCooldownFixture(t, "a")
	f.config.CacheDir = ""
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusForbidden, cooldownBody(403)})
	p := f.start()
	cooldownChat(p)
	if v := cooldownView(t, p, "a"); v.CooldownUntil == nil {
		t.Fatal("无缓存目录时冷却应在内存中生效")
	}
	assertNoAccount(t, cooldownChat(p))

	p = f.start() // 重启：内存状态丢失，账号应重新可用（请求到达上游并得到 403）
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
		t.Fatalf("无缓存目录时重启后不应恢复冷却，实际 until=%v", v.CooldownUntil)
	}
	if w := cooldownChat(p); w.Code != http.StatusForbidden {
		t.Fatalf("重启后账号应重新参与选择，status=%d body=%s, want 上游的 403", w.Code, w.Body.String())
	}
}
