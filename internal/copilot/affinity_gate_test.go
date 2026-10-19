package copilot

// 本文件覆盖 keyGate（affinity_gate.go）：同一会话键同时只有一个持有者，其他调用者排队等待；
// 不同键互不阻塞；空键不互斥；等待中或获取前被取消都返回 ctx.Err() 且不持有门、不残留条目；
// 取消的等待者不挡住后续等待者；release 幂等；使用完毕后内部不残留条目。
// 同步只用 channel；唯一例外是 gateAwaitWaiters 轮询 keyGate 的内部等待计数（有超时保护），用于确认等待者已排队。
// time 只用于两类保护：负向断言的观察窗口（断言“不应发生”）与超时保护（防止错误实现挂起）。

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateNegativeWindow 是负向断言的观察窗口：门被持有期间，等待者不得获得门。
// 正确实现下这个断言永远不会误报；错误实现只可能在窗口很短时漏报。
const gateNegativeWindow = 20 * time.Millisecond

// gateGuardTimeout 是等待异步结果的超时保护，只在实现挂起时才会触发。
const gateGuardTimeout = 10 * time.Second

// gateWaiterTimeout 是等待“等待者已排队”的超时保护。排队只需毫秒级，远小于该值；只在实现错误（如缺门）时触发。
const gateWaiterTimeout = 2 * time.Second

// gateResult 是一次异步 acquire 的结果。
type gateResult struct {
	release func()
	err     error
}

// gateAcquireAsync 在新 goroutine 中调用 acquire，结果通过返回的 channel 交付（缓冲 1，goroutine 不会因无人接收而阻塞）。
func gateAcquireAsync(g *keyGate, ctx context.Context, key string) <-chan gateResult {
	ch := make(chan gateResult, 1)
	go func() {
		release, err := g.acquire(ctx, key)
		ch <- gateResult{release: release, err: err}
	}()
	return ch
}

// gateAwait 等待异步 acquire 返回；超时则终止测试。只能在测试 goroutine 中调用。
func gateAwait(t *testing.T, ch <-chan gateResult) gateResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(gateGuardTimeout):
		t.Fatal("acquire 长时间未返回")
		return gateResult{}
	}
}

// gateMustAcquire 同步获取门，要求立即成功（超时即失败），返回 release。
func gateMustAcquire(t *testing.T, g *keyGate, key string) func() {
	t.Helper()
	r := gateAwait(t, gateAcquireAsync(g, context.Background(), key))
	if r.err != nil {
		t.Fatalf("acquire(%q) 返回错误: %v", key, r.err)
	}
	return r.release
}

// gateAwaitWaiters 轮询 keyGate 内部，等待 key 的等待者数恰好为 n（有超时保护，只能在测试 goroutine 中调用）。
// 用于确认等待者已经排队，之后再取消或释放，以稳定复现排队中的时序；不使用固定时长的 sleep。
func gateAwaitWaiters(t *testing.T, g *keyGate, key string, n int) {
	t.Helper()
	deadline := time.Now().Add(gateWaiterTimeout)
	for {
		g.mu.Lock()
		got := 0
		if e := g.m[key]; e != nil {
			got = e.waiters
		}
		g.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("key %q 的等待者数 = %d, want %d", key, got, n)
		}
		runtime.Gosched()
	}
}

// TestKeyGateSameKeyBlocksUntilRelease 门被持有期间，同一 key 的第二个调用者必须阻塞；release 之后才能获得。
func TestKeyGateSameKeyBlocksUntilRelease(t *testing.T) {
	var g keyGate
	release := gateMustAcquire(t, &g, "session-a")

	waiter := gateAcquireAsync(&g, context.Background(), "session-a")
	select {
	case r := <-waiter:
		t.Fatalf("门被持有期间，同一 key 的等待者提前返回 (err=%v)", r.err)
	case <-time.After(gateNegativeWindow):
	}

	release()
	r := gateAwait(t, waiter)
	if r.err != nil {
		t.Fatalf("release 之后等待者获取门失败: %v", r.err)
	}
	r.release()
}

// TestKeyGateSameKeyIsMutuallyExclusive 多个 goroutine 争抢同一 key：任一时刻最多只有一个持有者，结束后内部不残留条目。
func TestKeyGateSameKeyIsMutuallyExclusive(t *testing.T) {
	var g keyGate
	const workers = 32
	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := g.acquire(context.Background(), "shared")
			if err != nil {
				t.Errorf("acquire 返回错误: %v", err)
				return
			}
			n := inside.Add(1)
			for {
				m := maxInside.Load()
				if n <= m || maxInside.CompareAndSwap(m, n) {
					break
				}
			}
			runtime.Gosched() // 让其他 goroutine 有机会在错误实现下进入临界区
			inside.Add(-1)
			release()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(gateGuardTimeout):
		t.Fatal("同一 key 的争抢长时间未结束")
	}
	if got := maxInside.Load(); got != 1 {
		t.Fatalf("同一 key 同时有 %d 个持有者, want 1", got)
	}
	if n := len(g.m); n != 0 {
		t.Fatalf("所有持有者结束后内部仍有 %d 个条目, want 0", n)
	}
}

// TestKeyGateDifferentKeysDoNotBlock 持有 "a" 时，获取 "b" 必须立即成功。
func TestKeyGateDifferentKeysDoNotBlock(t *testing.T) {
	var g keyGate
	releaseA := gateMustAcquire(t, &g, "a")
	releaseB := gateMustAcquire(t, &g, "b")
	releaseB()
	releaseA()
}

// TestKeyGateEmptyKeyIsNotExclusive 空 key 不互斥：可以同时获得多次，release 为空操作，且不在内部创建条目。
func TestKeyGateEmptyKeyIsNotExclusive(t *testing.T) {
	var g keyGate
	first := gateMustAcquire(t, &g, "")
	second := gateMustAcquire(t, &g, "")
	first()
	second()
	if n := len(g.m); n != 0 {
		t.Fatalf("空 key 在内部创建了 %d 个条目, want 0", n)
	}
}

// TestKeyGateWaitCanceledReturnsContextError 等待中 ctx 被取消：返回 ctx.Err()，且不持有门。
// 之后原持有者释放，门必须能被正常获取，并且内部不残留条目。
func TestKeyGateWaitCanceledReturnsContextError(t *testing.T) {
	var g keyGate
	release := gateMustAcquire(t, &g, "k")

	ctx, cancel := context.WithCancel(context.Background())
	waiter := gateAcquireAsync(&g, ctx, "k")
	cancel()
	r := gateAwait(t, waiter)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("等待中取消 err = %v, want context.Canceled", r.err)
	}

	// 被取消的等待者不能偷偷持有门：原持有者释放后，新调用者必须立即获得。
	release()
	again := gateMustAcquire(t, &g, "k")
	again()
	if n := len(g.m); n != 0 {
		t.Fatalf("取消与释放之后内部仍有 %d 个条目, want 0", n)
	}
}

// TestKeyGatePreCanceledContextFailsWithoutEntry 获取前 ctx 已取消：立即返回 ctx.Err()，内部不创建条目；
// 门被占用时同样不排队（不残留等待者计数，持有者释放后条目被删除）。
func TestKeyGatePreCanceledContextFailsWithoutEntry(t *testing.T) {
	var g keyGate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := g.acquire(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("预先取消的 ctx err = %v, want context.Canceled", err)
	}
	if n := len(g.m); n != 0 {
		t.Fatalf("预先取消的 ctx 在内部创建了 %d 个条目, want 0", n)
	}
	release := gateMustAcquire(t, &g, "k") // 门空闲，之前的失败没有影响后续获取
	release()

	releaseHolder := gateMustAcquire(t, &g, "k")
	if _, err := g.acquire(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("门被占用且 ctx 已取消 err = %v, want context.Canceled", err)
	}
	releaseHolder()
	if n := len(g.m); n != 0 {
		t.Fatalf("预先取消的等待者残留了 %d 个条目, want 0", n)
	}
}

// TestKeyGateCanceledWaiterDoesNotBlockNextWaiter 持有者之后依次排了两个等待者（A 先于 B）：A 排队中被取消，
// 随后释放持有者，B 必须获得门（已取消的等待者不能占住门的交接），最终内部不残留条目。
func TestKeyGateCanceledWaiterDoesNotBlockNextWaiter(t *testing.T) {
	var g keyGate
	releaseHolder := gateMustAcquire(t, &g, "k")

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	waiterA := gateAcquireAsync(&g, ctxA, "k")
	gateAwaitWaiters(t, &g, "k", 1)
	waiterB := gateAcquireAsync(&g, context.Background(), "k")
	gateAwaitWaiters(t, &g, "k", 2)

	cancelA()
	if r := gateAwait(t, waiterA); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("排队中被取消的等待者 err = %v, want context.Canceled", r.err)
	}
	releaseHolder()

	r := gateAwait(t, waiterB)
	if r.err != nil {
		t.Fatalf("取消者之后的等待者获取门失败: %v", r.err)
	}
	r.release()
	if n := len(g.m); n != 0 {
		t.Fatalf("全部结束后内部仍有 %d 个条目, want 0", n)
	}
}

// TestKeyGateReleaseIsIdempotent 重复调用 release 无害：第一个持有者的 release 被多次调用，不得释放第二个持有者的门。
func TestKeyGateReleaseIsIdempotent(t *testing.T) {
	var g keyGate
	release1 := gateMustAcquire(t, &g, "k")
	release1()
	release2 := gateMustAcquire(t, &g, "k") // 第一次释放后门空闲，第二个持有者立即获得

	release1() // 重复释放，不能影响第二个持有者

	waiter := gateAcquireAsync(&g, context.Background(), "k")
	select {
	case r := <-waiter:
		t.Fatalf("重复释放使第二个持有者的门被释放了 (err=%v)", r.err)
	case <-time.After(gateNegativeWindow):
	}

	release2()
	r := gateAwait(t, waiter)
	if r.err != nil {
		t.Fatalf("第二个持有者释放后等待者获取门失败: %v", r.err)
	}
	r.release()
	r.release() // 等待者的 release 同样可以重复调用
	if n := len(g.m); n != 0 {
		t.Fatalf("全部释放后内部仍有 %d 个条目, want 0", n)
	}
}

// TestKeyGateRemovesEntriesAfterUse 大量不同 key 依次获取与释放后，内部不残留任何条目（零值可直接使用）。
func TestKeyGateRemovesEntriesAfterUse(t *testing.T) {
	var g keyGate
	for i := range 200 {
		release := gateMustAcquire(t, &g, fmt.Sprintf("session-%d", i))
		release()
	}
	if n := len(g.m); n != 0 {
		t.Fatalf("依次使用 200 个 key 后内部仍有 %d 个条目, want 0", n)
	}
}
