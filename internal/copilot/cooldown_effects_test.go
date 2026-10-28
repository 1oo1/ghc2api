package copilot

// 本文件覆盖冷却副作用的等待：
//   - WaitCooldownEffectsTimeout 的有界等待：副作用仍被阻塞时短超时返回 false，副作用结束后返回 true。

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
