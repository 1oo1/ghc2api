package copilot

import "context"

// contextMutex 串行化缓存刷新，但允许排队的请求在取消时立即离开。
type contextMutex chan struct{}

func (m contextMutex) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m contextMutex) unlock() {
	<-m
}
