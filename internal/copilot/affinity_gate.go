package copilot

import (
	"context"
	"sync"
)

// keyGate 按会话键串行化一段临界区：同一键同时只有一个持有者，其余调用者排队等待；不同键互不影响。
// 零值可用。条目只在有持有者或等待者时存在，全部结束后从 m 中删除，因此 m 的大小不随历史键数增长。
type keyGate struct {
	mu sync.Mutex
	m  map[string]*gateEntry // 会话键 -> 门条目；m 在第一次获取时懒初始化
}

// gateEntry 是一个会话键的门。token 是容量为 1 的令牌通道：令牌在通道中表示门空闲，取走即为持有，放回即为释放。
type gateEntry struct {
	waiters int           // 阻塞中的等待者数，由 keyGate.mu 保护
	token   chan struct{} // 容量 1；持有期间通道为空
}

// acquire 获取 key 的门。空 key 不互斥，直接返回空操作的 release。
// 门被其他调用者持有时阻塞，直到对方 release 或 ctx 取消；ctx 取消（含获取前已取消，或与取得门同时发生）时返回 ctx.Err() 且不持有门。
// 返回的 release 只释放本次获取的门，重复调用为空操作。
func (g *keyGate) acquire(ctx context.Context, key string) (release func(), err error) {
	if key == "" {
		return func() {}, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*gateEntry)
	}
	e := g.m[key]
	if e == nil {
		e = &gateEntry{token: make(chan struct{}, 1)}
		e.token <- struct{}{}
		g.m[key] = e
	}
	if len(e.token) == 1 { // 门空闲，直接取得
		<-e.token
		g.mu.Unlock()
		return g.releaser(key, e), nil
	}
	e.waiters++
	g.mu.Unlock()

	select {
	case <-e.token:
		g.mu.Lock()
		e.waiters--
		g.mu.Unlock()
		release := g.releaser(key, e)
		// 令牌与 ctx 取消同时就绪时 select 可能选中令牌：仍按取消处理，立即释放刚取得的门（清理与 release 相同），不返回门。
		if ctx.Err() != nil {
			release()
			return nil, ctx.Err()
		}
		return release, nil
	case <-ctx.Done():
		// 未取得令牌，不持有门；取消与释放同时发生时令牌留在通道中，由 dropIfIdleLocked 处理。
		g.mu.Lock()
		e.waiters--
		g.dropIfIdleLocked(key, e)
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

// releaser 返回 key 门的释放函数。通过 sync.Once 保证只释放一次，因此重复调用不会释放之后获取门的其他调用者。
func (g *keyGate) releaser(key string, e *gateEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			e.token <- struct{}{} // 持有期间通道为空，此处不会阻塞
			g.dropIfIdleLocked(key, e)
		})
	}
}

// dropIfIdleLocked 在门空闲（令牌在通道中）且没有等待者时删除条目。调用方须持有 g.mu。
func (g *keyGate) dropIfIdleLocked(key string, e *gateEntry) {
	if e.waiters == 0 && len(e.token) == 1 {
		delete(g.m, key)
	}
}
