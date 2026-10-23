package copilot

// 本文件覆盖冷却副作用的守卫条件：
//   - 记录写入与解除之间的比较（CAS）：解除冷却之后，用旧截止时间写入记录不会复活冷却；
//     解除冷却停在记录锁之前时，若期间产生了新冷却，解除只清除旧冷却，新冷却保持有效；
//   - 清空缓存的步骤在冷却已解除（still 返回 false）时不改动内存与磁盘上的缓存。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestStaleRecordWriteAfterLiftIsSkipped 解除冷却后，用旧截止时间调用 writeRecord 不得写回 cooldown.json，冷却不会复活。
func TestStaleRecordWriteAfterLiftIsSkipped(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()
	cooldownChat(p)
	cooldownAwaitRecord(t, p, "a")
	until := *cooldownView(t, p, "a").CooldownUntil
	path := cooldownFile(t, p, "a", "cooldown")

	cooldownDecodeView(t, cooldownPatch(p, `{"id":"a","clear_cooldown":true}`))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("解除后 cooldown.json 应已删除（err=%v）", err)
	}

	p.findAccount("a").writeRecord(context.Background(), until, "401")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("解除后用旧截止时间写入记录，cooldown.json 被写回（冷却复活）: err=%v", err)
	}
	if v := cooldownView(t, p, "a"); v.CooldownUntil != nil {
		t.Fatalf("解除后用旧截止时间写入记录，内存冷却复活至 %s", v.CooldownUntil)
	}
}

// TestNewCooldownSurvivesLiftThatWaitedOnRecordLock 解除冷却停在记录锁之前时，旧冷却已到期且期间产生了新冷却：
// 释放后解除只清除旧冷却（比较-交换保护），新冷却保持有效，cooldown.json 记录的是新的截止时间。
func TestNewCooldownSurvivesLiftThatWaitedOnRecordLock(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.config.AccountCooldown = 300 * time.Millisecond
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()
	cooldownChat(p)
	cooldownAwaitRecord(t, p, "a")
	oldUntil := *cooldownView(t, p, "a").CooldownUntil
	time.Sleep(time.Until(oldUntil) + 50*time.Millisecond) // 旧冷却到期

	a := p.findAccount("a")
	a.errs.fileMu.Lock() // 记录锁被占用：解除冷却停在锁前
	liftDone := make(chan int, 1)
	go func() {
		liftDone <- cooldownPatch(p, `{"id":"a","clear_cooldown":true}`).Code
	}()
	// 没有可观察的信号表示解除已经停在记录锁前，这里留出时间（解除的第一步只读取截止时间，不会阻塞）。
	time.Sleep(200 * time.Millisecond)

	if w := cooldownChat(p); w.Code != http.StatusUnauthorized {
		t.Fatalf("旧冷却到期后的请求 status=%d, want 上游的 401（产生新冷却）", w.Code)
	}
	newUntil := cooldownView(t, p, "a").CooldownUntil
	if newUntil == nil || !newUntil.After(oldUntil) {
		t.Fatalf("旧冷却到期后的 401 应产生新冷却，实际 until=%v（旧 %v）", newUntil, oldUntil)
	}

	a.errs.fileMu.Unlock()
	select {
	case <-liftDone:
	case <-time.After(10 * time.Second):
		t.Fatal("解除冷却请求未结束")
	}
	cooldownSettle(p)

	v := cooldownView(t, p, "a")
	if v.CooldownUntil == nil || !v.CooldownUntil.Equal(*newUntil) || v.CooldownReason != "401" {
		t.Fatalf("解除只应清除旧冷却，新冷却应保留：until=%v reason=%q, want %v 与 401", v.CooldownUntil, v.CooldownReason, newUntil)
	}
	fields := cooldownFields(t, cooldownFile(t, p, "a", "cooldown"))
	if got := persistTimeField(t, fields, "until"); !got.Equal(newUntil.UTC().Truncate(time.Second)) {
		t.Fatalf("cooldown.json 记录的截止时间 = %s, want 新冷却 %s", got, newUntil.UTC().Truncate(time.Second))
	}
}

// TestForgetCachesRespectStillCheck 清空缓存的步骤（forgetSession、forgetModels、forgetAuto）在 still 返回 false（冷却已解除）时，
// 不改动内存与磁盘上的缓存；still 返回 true 时才清空（对照）。
func TestForgetCachesRespectStillCheck(t *testing.T) {
	f := newCooldownFixture(t, "a")
	p := f.start()
	s := p.findAccount("a").service
	sessionPath := s.cacheFile("session")
	modelsPath := s.cacheFile("models")

	s.session = session{Token: "keep-token", ExpiresAt: time.Now().Add(time.Hour).Unix(), API: "https://api.githubcopilot.com"}
	if err := s.writeCache("session", cachedSession{Token: "keep-token", ExpiresAt: s.session.ExpiresAt, API: s.session.API}); err != nil {
		t.Fatal(err)
	}
	s.models = modelList{Data: []json.RawMessage{json.RawMessage(`{"id":"gpt-4.1"}`)}, At: time.Now()}
	if err := s.writeCache("models", s.models); err != nil {
		t.Fatal(err)
	}
	s.auto = autoSession{Token: "auto-token", Model: "gpt-4.1", ExpiresAt: time.Now().Add(time.Hour).Unix()}

	never := func() bool { return false }
	if err := s.forgetSession(never); err != nil {
		t.Fatal(err)
	}
	if err := s.forgetModels(never); err != nil {
		t.Fatal(err)
	}
	s.forgetAuto(never)
	if s.session.Token != "keep-token" {
		t.Fatalf("still 为 false 时内存 session 被清空: %q", s.session.Token)
	}
	if len(s.models.Data) != 1 {
		t.Fatalf("still 为 false 时内存模型列表被清空: 条数 %d", len(s.models.Data))
	}
	if s.auto.Model != "gpt-4.1" {
		t.Fatalf("still 为 false 时内存 Auto 会话被清空: %q", s.auto.Model)
	}
	if !cooldownSessionFileHasToken(sessionPath) {
		t.Fatal("still 为 false 时磁盘 session.json 被删除或清空")
	}
	if !cooldownModelsFileHasData(modelsPath) {
		t.Fatal("still 为 false 时磁盘 models.json 被删除或清空")
	}

	always := func() bool { return true }
	if err := s.forgetSession(always); err != nil {
		t.Fatal(err)
	}
	if err := s.forgetModels(always); err != nil {
		t.Fatal(err)
	}
	s.forgetAuto(always)
	if s.session.Token != "" || len(s.models.Data) != 0 || s.auto.Model != "" {
		t.Fatalf("still 为 true 时应清空内存缓存：session=%q models=%d auto=%q", s.session.Token, len(s.models.Data), s.auto.Model)
	}
	if cooldownSessionFileHasToken(sessionPath) || cooldownModelsFileHasData(modelsPath) {
		t.Fatal("still 为 true 时应清空磁盘缓存")
	}
}
