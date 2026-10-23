package copilot

// 本文件覆盖冷却副作用的等待与 Auto 会话的清理：
//   - WaitCooldownEffectsTimeout 的有界等待：副作用仍被阻塞时短超时返回 false，副作用结束后返回 true；
//   - 进入冷却后，内存中的 Auto 会话在副作用完成后被清空。

import (
	"net/http"
	"testing"
	"time"
)

// TestWaitCooldownEffectsTimeoutFollowsPendingEffects 副作用仍被阻塞（清空 session 等待会话锁）时，有界等待在超时后返回 false；
// 另一 goroutine 以较长的超时等待，副作用结束后返回 true。
func TestWaitCooldownEffectsTimeoutFollowsPendingEffects(t *testing.T) {
	f := newCooldownFixture(t, "a")
	f.up.pool.setSessionTTL(time.Minute)
	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	p := f.start()

	first, release := cooldownSessionLockedDuring401(t, f, p)
	t.Cleanup(release)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("触发冷却的 401 响应未返回")
	}

	if p.WaitCooldownEffectsTimeout(100 * time.Millisecond) {
		t.Fatal("副作用仍被阻塞时，短超时的等待应返回 false")
	}
	result := make(chan bool, 1)
	go func() {
		result <- p.WaitCooldownEffectsTimeout(10 * time.Second)
	}()
	release() // 释放会话锁，副作用结束（release 内部也会等待副作用完成）
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("副作用结束后，较长超时的等待应返回 true")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("等待冷却副作用的 goroutine 未返回")
	}
}

// cooldownAutoModel 在 Auto 锁内读取内存中的 Auto 会话所选模型。
func cooldownAutoModel(s *Service) string {
	s.autoState.Lock()
	defer s.autoState.Unlock()
	return s.auto.Model
}

// TestCooldownClearsInMemoryAutoSession 进入冷却后，内存中的 Auto 会话在副作用完成后被清空。
func TestCooldownClearsInMemoryAutoSession(t *testing.T) {
	const autoBody = `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
	f := newCooldownFixture(t, "a")
	p := f.start()
	s := p.findAccount("a").service

	if w := poolRequest(p, "POST", "/v1/chat/completions", autoBody); w.Code != http.StatusOK {
		t.Fatalf("Auto 请求 status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if cooldownAutoModel(s) == "" {
		t.Fatal("前提不成立：Auto 请求后内存中应有 Auto 会话")
	}

	f.up.pool.failGeneration("oauth-a", poolFailure{http.StatusUnauthorized, cooldownBody(401)})
	if w := poolRequest(p, "POST", "/v1/chat/completions", autoBody); w.Code != http.StatusUnauthorized {
		t.Fatalf("触发冷却的 Auto 请求 status=%d body=%s, want 401", w.Code, w.Body.String())
	}
	p.WaitCooldownEffects()
	if m := cooldownAutoModel(s); m != "" {
		t.Fatalf("进入冷却后内存中的 Auto 会话未清空（模型 %q）", m)
	}
}
