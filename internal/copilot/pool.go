package copilot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AccountCooldown 是账号失败后暂停参与选择的时长。
const AccountCooldown = time.Minute

// PoolConfig 是账号池的服务级配置，账号无关的设置都放在这里。
type PoolConfig struct {
	APIKey             string        // 客户端访问密钥，空表示不鉴权
	ManagementKey      string        // 管理密钥；空表示禁用管理接口和控制台（对应路径按普通 404 处理）
	CredentialsDir     string        // 管理接口新增/删除/禁用账号时读写凭证文件的目录
	Host               string        // 顶层 host 覆盖；非空时新增账号与设备登录都使用它
	CacheDir           string        // 缓存根目录；各账号 Service 在其下按账号哈希分子目录（测试必须指向 t.TempDir()）
	ModelsCacheTTL     time.Duration // 非正值使用默认的模型列表缓存有效期
	SessionAffinityTTL time.Duration // <=0 表示关闭会话亲和（不创建绑定表）
	Client             *http.Client  // 访问 GitHub/Copilot 的客户端（测试注入），新增账号与设备登录也使用它
	Logger             *slog.Logger
}

// PoolAccount 是启动时载入的一个账号。
type PoolAccount struct {
	Token    string
	Host     string
	User     string // GitHub 用户名，必填（为空返回错误）
	Disabled bool
	Priority int // 优先级，取值 MinPriority 到 MaxPriority；数值越大越优先被选择
}

// poolAccount 是池内一个账号的运行时状态。id、service 与 routes 创建后不再变化；disabled 与 priority 由 Pool.mu 保护。
type poolAccount struct {
	id       string        // 账号 ID：GitHub 用户名的小写形式；无用户名时为初始下标
	service  *Service      // 账号的 Copilot 服务
	routes   serviceRoutes // 账号的业务路由，闭包捕获 service
	disabled bool
	priority int // 优先级，见 PoolAccount.Priority

	requests atomic.Uint64 // 分发到该账号的需要账号的请求数（含用量与生成）
	failures atomic.Uint64 // 其中响应状态 >= 400 的数量
	lastUsed atomic.Int64  // 最近一次使用的 UnixNano，0 表示从未使用

	quota quotaCache
}

// newPoolAccount 为 Service 创建账号运行时状态，并从缓存目录恢复额度（见 restoreQuota）。
func newPoolAccount(service *Service, id string) *poolAccount {
	account := &poolAccount{id: id, service: service, routes: service.routes()}
	account.restoreQuota()
	return account
}

// Pool 在多个 GitHub 账号之间分发请求：按优先级与额度权重选择最优的可用账号，并负责会话亲和、账号的运行时增删、禁用与调整优先级。
type Pool struct {
	apiKey     string // 所有账号共用的服务访问密钥
	logger     *slog.Logger
	config     PoolConfig     // 服务级配置，新增账号与设备登录时使用
	client     *http.Client   // 新增账号与设备登录访问 GitHub 的客户端
	idle       serviceRoutes  // 不访问上游的路由（健康检查、重定向、404/405），不依赖任何账号
	affinity   *affinityTable // 会话绑定表；关闭时为 nil
	management http.Handler   // 管理接口；ManagementKey 为空时为 nil

	requestSeq atomic.Uint64

	adminMu  sync.Mutex // 串行化账号的增删、禁用、优先级与凭证文件写入
	mu       sync.Mutex // 保护 accounts 与各账号的 disabled、priority
	accounts []*poolAccount

	loginMu sync.Mutex
	logins  map[string]*deviceLogin // 设备登录状态，见 management_login.go
}

// NewPool 校验所有账号配置并创建账号池，账号顺序即加入顺序（选择只看优先级、额度权重与文件名，与顺序无关）。会话亲和与管理接口均关闭。
// 所有账号的服务访问密钥去除首尾空白后必须一致，GitHub 账号（Host 规范化后与 Token）不能重复。
func NewPool(configs []Config) (*Pool, error) {
	if len(configs) == 0 {
		return nil, errors.New("至少需要一个 GitHub Copilot 账号")
	}
	p := &Pool{idle: idleRoutes()}
	for i, config := range configs {
		config.APIKey = strings.TrimSpace(config.APIKey)
		if i == 0 {
			p.apiKey = config.APIKey
		} else if config.APIKey != p.apiKey {
			return nil, errors.New("所有账号的服务访问密钥必须一致")
		}
		if err := p.addConfigured(i+1, config, false, 0); err != nil {
			return nil, err
		}
	}
	p.logger = p.accounts[0].service.logger
	return p, nil
}

// NewManagedPool 按服务级配置创建可在运行时管理账号的池。
// 允许 0 个账号，但仅当 ManagementKey 非空时（否则返回错误“至少需要一个 GitHub Copilot 账号”）。
// 每个账号的用户名必填且须通过 ValidGitHubUserName；账号重复（Host+Token 相同）或用户名忽略大小写重复时返回错误。
// ManagementKey 与 APIKey 去除首尾空白后相同时返回错误。
func NewManagedPool(config PoolConfig, accounts []PoolAccount) (*Pool, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.ManagementKey = strings.TrimSpace(config.ManagementKey)
	host, err := NormalizeHost(config.Host)
	if err != nil {
		return nil, err
	}
	config.Host = host
	if config.ManagementKey == "" && len(accounts) == 0 {
		return nil, errors.New("至少需要一个 GitHub Copilot 账号")
	}
	if config.ManagementKey != "" && strings.TrimSpace(config.CredentialsDir) == "" {
		return nil, errors.New("启用管理接口时必须指定凭证目录")
	}
	if config.ManagementKey != "" && config.ManagementKey == config.APIKey {
		return nil, errors.New("管理密钥不能与服务访问密钥相同")
	}
	p := newPool(config)
	for i, account := range accounts {
		user := strings.TrimSpace(account.User)
		if !ValidGitHubUserName(user) {
			return nil, fmt.Errorf("第 %d 个 GitHub 账号的用户名无效", i+1)
		}
		err := p.addConfigured(i+1, Config{
			Token: account.Token, Host: account.Host, User: user, APIKey: p.apiKey,
			CacheDir: config.CacheDir, ModelsCacheTTL: config.ModelsCacheTTL,
			Client: config.Client, Logger: p.logger,
		}, account.Disabled, account.Priority)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

// addConfigured 在构造阶段为配置创建账号并追加到池末尾。账号 ID 为用户名的小写形式；没有用户名时为下标。
// index 是账号在输入中的序号（从 1 开始），只用于错误信息；错误信息不包含 token。
// disabled 与 priority 是账号的初始状态。
func (p *Pool) addConfigured(index int, config Config, disabled bool, priority int) error {
	service, err := New(config)
	if err != nil {
		return fmt.Errorf("第 %d 个 GitHub 账号配置无效: %w", index, err)
	}
	id := strconv.Itoa(index - 1)
	if user := strings.TrimSpace(config.User); user != "" {
		id = strings.ToLower(user)
	}
	if p.conflicts(service, id) {
		return fmt.Errorf("第 %d 个 GitHub 账号与之前的账号重复", index)
	}
	account := newPoolAccount(service, id)
	account.disabled = disabled
	account.priority = priority
	p.accounts = append(p.accounts, account)
	return nil
}

// newPool 按服务级配置创建空池，并按配置启用会话亲和与管理接口。
func newPool(config PoolConfig) *Pool {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	p := &Pool{
		apiKey: config.APIKey, logger: logger, config: config, client: safeClient(config.Client),
		idle: idleRoutes(),
	}
	if config.SessionAffinityTTL > 0 {
		p.affinity = newAffinityTable(config.SessionAffinityTTL)
	}
	if config.ManagementKey != "" {
		p.logins = make(map[string]*deviceLogin)
		p.management = p.newManagementHandler()
	}
	return p
}

// idleRoutes 登记不访问上游的路由。这些路由（healthz、重定向、404/405）不依赖任何账号，
// 因此只用一份不绑定 Service 的路由表处理；路由表本身不会访问 Service。
func idleRoutes() serviceRoutes {
	return (*Service)(nil).routes()
}

// conflicts 报告新账号是否与池中已有账号的 Host+Token 或 ID 相同。调用方须保证账号列表不被并发修改。
func (p *Pool) conflicts(service *Service, id string) bool {
	identity := service.config.Host + "\x00" + service.config.Token
	for _, a := range p.accounts {
		if a.id == id || a.service.config.Host+"\x00"+a.service.config.Token == identity {
			return true
		}
	}
	return false
}

// Handler 返回与单账号 Service 相同的 HTTP 接口：统一鉴权，业务请求按账号选择规则交给某个账号处理。
// 启用管理接口时，/management 与 /v0/management/ 下的路径改用管理密钥鉴权，不受 api_key 影响。
func (p *Pool) Handler() http.Handler {
	business := requireAPIKey(p.apiKey, p.log, http.HandlerFunc(p.dispatch))
	var handler http.Handler = business
	if p.management != nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isManagementPath(r.URL.Path) {
				p.management.ServeHTTP(w, r)
				return
			}
			business.ServeHTTP(w, r)
		})
	}
	return withRequestLog(p.logger, &p.requestSeq, p.log, handler)
}

// dispatch 按路由登记的上游访问方式分发请求：不访问上游的请求直接处理且不参与账号选择；
// 需要账号的请求按选择规则选择账号；需要 session 的请求进入 forward。
func (p *Pool) dispatch(w http.ResponseWriter, r *http.Request) {
	entry := p.routeOf(r)
	switch entry.need {
	case needNone:
		p.idle.mux.ServeHTTP(w, r)
	case needAccount:
		account := p.choose("", nil)
		if account == nil {
			writeNoAccount(w)
			return
		}
		p.serve(w, r.WithContext(p.accountContext(r.Context(), account)), account)
	default:
		p.forward(w, r, p.affinityKey(r, entry.upstream))
	}
}

// routeOf 返回请求命中的业务路由；未命中（404、405）或路径不一致（如重定向 //models）时返回 needNone。
// 路径必须与登记的路径一致：重定向也会返回原路由的 pattern，但不应被当作该路由处理。
func (p *Pool) routeOf(r *http.Request) routeEntry {
	_, pattern := p.idle.mux.Handler(r)
	entry, ok := p.idle.entries[pattern]
	if !ok || r.URL.Path != entry.path {
		return routeEntry{need: needNone}
	}
	return entry
}

// affinityKey 计算生成请求的会话键。未开启亲和或非生成接口时不读取请求体。
// 请求体最多读取 maxRequestBody 字节，读出的部分与剩余内容拼接后原样放回 r.Body，下游仍能读到完整请求体；
// 超过上限时不计算会话键，下游会照常返回 413。
func (p *Pool) affinityKey(r *http.Request, upstream string) string {
	if upstream == "" || p.affinity == nil || r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
	if err != nil || len(body) > maxRequestBody {
		return ""
	}
	return sessionKey(r, body, upstream)
}

// forward 处理需要 session 的请求。先按会话亲和（已有绑定）或账号选择规则选出账号并预取 session；
// session 获取失败时冷却该账号，并在同一请求内改用其他尚未尝试的账号，每个账号最多尝试一次。
// 全部失败时按最后一个账号的 handleError 规则返回错误；没有任何可选账号时返回 503。
// 客户端已取消时不冷却账号。
// 请求体校验（生成接口）在预取 session 之后才执行，因此全部账号的 session 都失败时，
// 无效请求体会收到 session 错误而不是 400，这是有意的取舍。
func (p *Pool) forward(w http.ResponseWriter, r *http.Request, key string) {
	var (
		ctx     context.Context
		last    *poolAccount
		lastErr error
	)
	tried := map[*poolAccount]bool{}
	for {
		account := p.choose(key, tried)
		if account == nil {
			break
		}
		tried[account] = true
		ctx = p.accountContext(r.Context(), account)
		last = account
		if _, err := account.service.getSession(ctx); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			account.service.setCooldown(time.Now().Add(AccountCooldown))
			account.service.log(ctx, slog.LevelWarn, "账号会话获取失败", "error", err, "cooldown", AccountCooldown.String())
			continue
		}
		p.bind(ctx, key, account)
		p.serve(w, r.WithContext(ctx), account)
		return
	}
	if last == nil {
		writeNoAccount(w)
		return
	}
	// 所有账号的 session 都失败：错误计入最后尝试的账号，之前失败的账号不计数。
	last.track(r.Context(), w, func(w http.ResponseWriter) { last.service.handleError(ctx, w, lastErr) })
}

// serve 把请求交给已选定的账号处理，并记录该账号的统计。
func (p *Pool) serve(w http.ResponseWriter, r *http.Request, account *poolAccount) {
	account.track(r.Context(), w, func(w http.ResponseWriter) { account.routes.mux.ServeHTTP(w, r) })
}

// track 记录账号处理了一次请求：请求数加 1，最近使用时间更新；响应状态 >= 400 时失败数加 1。
// 客户端已取消（reqCtx 为 context.Canceled）时的失败并非账号的问题，不计入失败数；账号也不会因此冷却。
func (a *poolAccount) track(reqCtx context.Context, w http.ResponseWriter, handle func(http.ResponseWriter)) {
	a.requests.Add(1)
	a.lastUsed.Store(time.Now().UnixNano())
	writer := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	handle(writer)
	if writer.status >= http.StatusBadRequest && !errors.Is(reqCtx.Err(), context.Canceled) {
		a.failures.Add(1)
	}
}

// bind 把会话键绑定（或续期）到实际处理请求的账号。键为空或未开启亲和时不做任何事。
// 绑定前先查询原绑定，并按结果输出亲和日志：没有有效原绑定为 Info「新建绑定」，原绑定即该账号为 Debug「命中」，
// 原绑定是其他账号为 Info「切换账号」。日志只含会话键前 12 个字符与账号 ID，不含完整会话键。
// lookup 与 bind 之间的并发竞争只影响日志，可以忽略。
func (p *Pool) bind(ctx context.Context, key string, account *poolAccount) {
	if key == "" || p.affinity == nil {
		return
	}
	now := time.Now()
	previous, bound := p.affinity.lookup(key, now)
	p.affinity.bind(key, account.id, now)
	prefix := key[:12]
	switch {
	case !bound:
		p.log(ctx, slog.LevelInfo, "会话亲和：新建绑定", "session", prefix, "bound_account", account.id)
	case previous == account.id:
		p.log(ctx, slog.LevelDebug, "会话亲和：命中", "session", prefix, "bound_account", account.id)
	default:
		p.log(ctx, slog.LevelInfo, "会话亲和：切换账号", "session", prefix, "from", previous, "to", account.id)
	}
}

// writeNoAccount 在没有任何可选账号（0 个或全部禁用）时返回 503。
func writeNoAccount(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "没有可用的 GitHub Copilot 账号")
}

// accountContext 返回绑定了账号信息的请求上下文，之后经由 Service 输出的日志都会带 account 与 user 字段。
func (p *Pool) accountContext(ctx context.Context, account *poolAccount) context.Context {
	logger := contextLogger(ctx, p.logger).With("account", p.indexOf(account))
	if user := account.service.config.User; user != "" {
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
	for _, service := range p.services() {
		value = service.redactLogValue(value)
	}
	return value
}

// services 返回当前所有账号的 Service 快照，供不需要持有锁的操作使用。
func (p *Pool) services() []*Service {
	p.mu.Lock()
	defer p.mu.Unlock()
	services := make([]*Service, 0, len(p.accounts))
	for _, a := range p.accounts {
		services = append(services, a.service)
	}
	return services
}

// choose 选择处理本次请求的账号。会话键已绑定且该账号仍可用（未禁用、未冷却、本请求未尝试过）时优先使用它；
// 否则按 pickLocked 的排序选择最优账号。没有候选时返回 nil。
func (p *Pool) choose(key string, tried map[*poolAccount]bool) *poolAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if key != "" && p.affinity != nil {
		if id, ok := p.affinity.lookup(key, now); ok {
			if account := p.findLocked(id); account != nil && !account.disabled &&
				!tried[account] && account.service.available(now) {
				return account
			}
		}
	}
	return p.pickLocked(now, tried)
}

// selectKey 是账号参与选择时的排序键：已冷却的账号靠后，其余字段数值大者优先、文件名小者优先。字段顺序即比较顺序，见 before。
type selectKey struct {
	cooling  bool        // 已冷却的账号排在未冷却的账号之后
	priority int         // 优先级，数值大者在前
	weight   quotaWeight // 额度权重，见 weigh
	file     string      // 凭证文件名（用户名 + ".json"，无用户名时用 ID），字节序升序
}

// keyOf 计算账号的排序键。调用方须持有 p.mu（读取 priority）。额度权重只读取缓存，不访问上游。
func keyOf(a *poolAccount, now time.Time) selectKey {
	name := a.service.config.User
	if name == "" {
		name = a.id
	}
	quota, _ := a.quota.snapshot()
	return selectKey{
		cooling:  !a.service.available(now),
		priority: a.priority,
		weight:   weigh(quota),
		file:     name + ".json",
	}
}

// before 报告 k 是否应排在 o 之前：未冷却优先，其次优先级高者在前，再次额度权重高者在前，最后凭证文件名字节序升序。
func (k selectKey) before(o selectKey) bool {
	if k.cooling != o.cooling {
		return !k.cooling
	}
	if k.priority != o.priority {
		return k.priority > o.priority
	}
	switch {
	case k.weight.better(o.weight):
		return true
	case o.weight.better(k.weight):
		return false
	}
	return k.file < o.file
}

// pickLocked 选出本次应使用的账号，调用方须持有 p.mu。候选为未禁用且未被 tried 排除的账号，
// 按 selectKey 排序取最优者。冷却中的账号排在未冷却账号之后，只有没有未冷却候选时才会被选中。没有候选时返回 nil。
func (p *Pool) pickLocked(now time.Time, tried map[*poolAccount]bool) *poolAccount {
	var best *poolAccount
	var bestKey selectKey
	for _, a := range p.accounts {
		if a.disabled || tried[a] {
			continue
		}
		key := keyOf(a, now)
		if best == nil || key.before(bestKey) {
			best, bestKey = a, key
		}
	}
	return best
}

// findLocked 按 ID 查找账号，调用方须持有 p.mu。
func (p *Pool) findLocked(id string) *poolAccount {
	for _, a := range p.accounts {
		if a.id == id {
			return a
		}
	}
	return nil
}

// findAccount 按 ID 查找账号（忽略大小写）；不存在时返回 nil。
func (p *Pool) findAccount(id string) *poolAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.findLocked(strings.ToLower(id))
}

// indexOf 返回账号在当前列表中的下标；账号已被删除时返回 -1。
func (p *Pool) indexOf(target *poolAccount) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accounts {
		if a == target {
			return i
		}
	}
	return -1
}

// accountsSnapshot 返回账号列表的副本，供并发刷新等不持有锁的操作使用。
func (p *Pool) accountsSnapshot() []*poolAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.accounts)
}

// removeAccount 从池中移除账号；正在处理的请求仍持有该账号指针，不受影响。
func (p *Pool) removeAccount(target *poolAccount) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts = slices.DeleteFunc(p.accounts, func(a *poolAccount) bool { return a == target })
}

// setCooldown 让账号在 until 之前不参与选择。
func (s *Service) setCooldown(until time.Time) {
	s.cooldownUntil.Store(until.UnixNano())
}

// available 报告账号在 now 时是否可参与选择；冷却截止时刻本身即视为可用，从未冷却的账号始终可用。
func (s *Service) available(now time.Time) bool {
	until := s.cooldownUntil.Load()
	return until == 0 || now.UnixNano() >= until
}
