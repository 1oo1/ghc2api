package copilot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AccountCooldown 是账号失败后暂停参与轮询的时长。
const AccountCooldown = time.Minute

// Pool 以 round-robin 方式在多个账号 Service 之间分发请求。
type Pool struct {
	services []*Service
	routes   []serviceRoutes // 与 services 一一对应的账号业务路由
	apiKey   string          // 所有账号共用的服务访问密钥
	logger   *slog.Logger

	requestSeq atomic.Uint64
	mu         sync.Mutex // 保护 cursor
	cursor     int        // 下一次轮询的起始账号下标
}

// NewPool 校验所有账号配置并创建轮询池，账号顺序即轮询顺序。
// 所有账号的服务访问密钥去除首尾空白后必须一致，GitHub 账号（Host 规范化后与 Token）不能重复。
func NewPool(configs []Config) (*Pool, error) {
	if len(configs) == 0 {
		return nil, errors.New("至少需要一个 GitHub Copilot 账号")
	}
	p := &Pool{}
	seen := make(map[string]struct{}, len(configs))
	for i, config := range configs {
		config.APIKey = strings.TrimSpace(config.APIKey)
		if i == 0 {
			p.apiKey = config.APIKey
		} else if config.APIKey != p.apiKey {
			return nil, errors.New("所有账号的服务访问密钥必须一致")
		}
		service, err := New(config)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个 GitHub 账号配置无效: %w", i+1, err)
		}
		// New 已去除 Token 空白并规范化 Host，因此这里的键与账号身份一致；错误信息不包含 token。
		identity := service.config.Host + "\x00" + service.config.Token
		if _, duplicate := seen[identity]; duplicate {
			return nil, fmt.Errorf("第 %d 个 GitHub 账号与之前的账号重复", i+1)
		}
		seen[identity] = struct{}{}
		p.services = append(p.services, service)
		p.routes = append(p.routes, service.routes())
	}
	p.logger = p.services[0].logger
	return p, nil
}

// Handler 返回与单账号 Service 相同的 HTTP 接口：统一鉴权，业务请求按轮询交给某个账号处理。
func (p *Pool) Handler() http.Handler {
	return withRequestLog(p.logger, &p.requestSeq, p.log,
		requireAPIKey(p.apiKey, p.log, http.HandlerFunc(p.dispatch)))
}

// dispatch 按路由登记的上游访问方式分发请求：不访问上游的请求由 routes[0] 直接处理且不推进轮询；
// 需要账号的请求按轮询选择账号；需要 session 的请求进入 forward。
func (p *Pool) dispatch(w http.ResponseWriter, r *http.Request) {
	switch p.needOf(r) {
	case needNone:
		p.routes[0].mux.ServeHTTP(w, r)
	case needAccount:
		account := p.next()
		p.routes[account].mux.ServeHTTP(w, r.WithContext(p.accountContext(r.Context(), account)))
	default:
		p.forward(w, r)
	}
}

// needOf 返回请求命中的路由所需的上游访问方式；未命中路由（404、405）为 needNone。
// 路径必须与登记的路径一致：重定向（如 //models）也会返回原路由的 pattern，但不应被当作该路由处理。
func (p *Pool) needOf(r *http.Request) routeNeed {
	_, pattern := p.routes[0].mux.Handler(r)
	entry, ok := p.routes[0].entries[pattern]
	if !ok || r.URL.Path != entry.path {
		return needNone
	}
	return entry.need
}

// forward 处理需要 session 的路由：按轮询选择账号并预取 session。会话获取失败时冷却该账号，
// 并在同一请求内改用其他尚未尝试的账号；每个账号在同一请求内最多尝试一次，全部失败时按最后一个账号的
// handleError 规则返回错误。客户端已取消时不冷却账号。
// 请求体校验（生成接口）在预取 session 之后才执行，因此全部账号的 session 都失败时，
// 无效请求体会收到 session 错误而不是 400，这是有意的取舍。
func (p *Pool) forward(w http.ResponseWriter, r *http.Request) {
	var ctx context.Context
	var lastService *Service
	var lastErr error
	tried := make([]bool, len(p.services))
	for range p.services {
		account := p.pick(tried)
		tried[account] = true
		service := p.services[account]
		ctx = p.accountContext(r.Context(), account)
		lastService = service
		if _, err := service.getSession(ctx); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			service.setCooldown(time.Now().Add(AccountCooldown))
			service.log(ctx, slog.LevelWarn, "账号会话获取失败", "error", err, "cooldown", AccountCooldown.String())
			continue
		}
		p.routes[account].mux.ServeHTTP(w, r.WithContext(ctx))
		return
	}
	lastService.handleError(ctx, w, lastErr)
}

// accountContext 返回绑定了账号下标的请求上下文，之后经由 Service 输出的日志都会带 account 字段。
func (p *Pool) accountContext(ctx context.Context, account int) context.Context {
	logger := contextLogger(ctx, p.logger).With("account", account)
	if user := p.services[account].config.User; user != "" {
		logger = logger.With("user", user)
	}
	return bindRequestLogger(ctx, logger)
}

// log 输出 Pool 自身的请求级日志。路径等客户端可控字段用所有账号的密钥脱敏。
func (p *Pool) log(ctx context.Context, level slog.Level, message string, args ...any) {
	safe := append([]any(nil), args...)
	for i := 1; i < len(safe); i += 2 {
		safe[i] = p.redactLogValue(safe[i])
	}
	contextLogger(ctx, p.logger).Log(ctx, level, message, safe...)
}

// redactLogValue 依次用每个账号的 secret 脱敏同一个值，结果等价于取所有账号 secret 的并集。
func (p *Pool) redactLogValue(value any) any {
	for _, service := range p.services {
		value = service.redactLogValue(value)
	}
	return value
}

// next 返回下一个账号下标（不排除任何账号），见 pick。
func (p *Pool) next() int {
	return p.pick(nil)
}

// pick 从游标开始选择账号并把游标移到所选账号之后。候选按优先级排列：
// 先选未冷却且未被 tried 排除的账号，再选已冷却但未被排除的账号作为最后候选；两类内部都保持轮询顺序。
// tried 为本请求已尝试过的账号（nil 表示不排除），调用方须保证仍有候选，否则返回 -1。
func (p *Pool) pick(tried []bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := len(p.services)
	chosen := -1
	for _, allowCooling := range []bool{false, true} {
		for offset := range n {
			j := (p.cursor + offset) % n
			if tried != nil && tried[j] {
				continue
			}
			if allowCooling || p.services[j].available(now) {
				chosen = j
				break
			}
		}
		if chosen >= 0 {
			break
		}
	}
	if chosen < 0 {
		return -1
	}
	p.cursor = (chosen + 1) % n
	return chosen
}

// setCooldown 让账号在 until 之前不参与轮询。
func (s *Service) setCooldown(until time.Time) {
	s.cooldownUntil.Store(until.UnixNano())
}

// available 报告账号在 now 时是否可参与轮询；冷却截止时刻本身即视为可用，从未冷却的账号始终可用。
func (s *Service) available(now time.Time) bool {
	until := s.cooldownUntil.Load()
	return until == 0 || now.UnixNano() >= until
}
