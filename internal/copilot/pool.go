package copilot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PoolConfig 是账号池的服务级配置，账号无关的设置都放在这里。
type PoolConfig struct {
	APIKey               string        // 客户端访问密钥，必填：去除首尾空白后不能为空
	ManagementKey        string        // 管理密钥；空表示禁用管理接口和控制台（对应路径按普通 404 处理）
	CredentialsDir       string        // 管理接口新增/删除/禁用账号时读写凭证文件的目录
	Host                 string        // 顶层 host 覆盖；非空时新增账号与设备登录都使用它
	CacheDir             string        // 缓存根目录；各账号 Service 在其下按账号哈希分子目录（测试必须指向 t.TempDir()）
	ModelsCacheTTL       time.Duration // 非正值使用默认的模型列表缓存有效期
	QuotaRefreshInterval time.Duration // 非正值使用默认的额度刷新间隔（DefaultQuotaRefreshInterval）
	SessionAffinityTTL   time.Duration // <=0 表示关闭会话亲和（不创建绑定表）
	// SessionAffinityMaxPerAccount 是每个账号的有效会话绑定数上限：新会话与故障切换时优先排除已达上限的账号，
	// 排除后没有候选时忽略上限（见 Pool.choose）。
	// <=0 表示不限制；缺省值 DefaultSessionAffinityMaxPerAccount 由 cmd 层设置。
	SessionAffinityMaxPerAccount int
	// AccountCooldown 是账号进入冷却后暂停参与选择的时长；<=0 使用 DefaultAccountCooldown（2 小时）。
	// NewPool（只接受账号配置）固定使用缺省值。冷却的触发规则见 cooldown.go。
	AccountCooldown time.Duration
	// MaxConcurrentRequests 是同时处理的生成请求数上限（命中生成接口的请求）；<=0 表示不限制。
	// 名额已满时立即返回 503，不读取请求体、不选账号；名额在请求处理结束（含流式响应结束）后释放。
	// 模型列表、健康检查与管理接口不占用名额。缺省值由 cmd 层设置（32）。
	MaxConcurrentRequests int
	Client                *http.Client // 访问 GitHub/Copilot 的客户端（测试注入），新增账号与设备登录也使用它
	Logger                *slog.Logger
}

// PoolAccount 是启动时载入的一个账号。
type PoolAccount struct {
	Token    string
	Host     string
	User     string // GitHub 用户名，必填（为空返回错误）
	Disabled bool
	Priority int // 优先级，取值 MinPriority 到 MaxPriority；数值越大越优先被选择
}

// poolAccount 是池内一个账号的运行时状态。id、service、routes、cooldown 与 affinity 创建后不再变化；disabled、priority 与 swrrCurrent 由 Pool.mu 保护。
type poolAccount struct {
	id          string        // 账号 ID：GitHub 用户名的小写形式；无用户名时为初始下标
	service     *Service      // 账号的 Copilot 服务
	routes      serviceRoutes // 账号的业务路由，闭包捕获 service
	disabled    bool
	priority    int // 优先级，见 PoolAccount.Priority
	swrrCurrent int // 平滑加权轮询的当前值，见 swrrPickLocked

	cooldown time.Duration    // 冷却时长，见 PoolConfig.AccountCooldown
	affinity *affinityTable   // 会话绑定表，冷却时删除指向本账号的绑定；未开启亲和时为 nil
	effects  *cooldownEffects // 进入冷却的异步副作用计数，由池持有（见 Pool.WaitCooldownEffects）
	errs     cooldownTracker  // 连续错误计数与冷却原因，见 cooldown.go

	requests atomic.Uint64 // 分发到该账号的请求数（含模型列表与生成）
	failures atomic.Uint64 // 其中响应状态 >= 400 的数量
	lastUsed atomic.Int64  // 最近一次使用的 UnixNano，0 表示从未使用

	quota quotaCache
}

// newPoolAccount 为 Service 创建账号运行时状态，并从缓存目录恢复额度（见 restoreQuota）与未到期的冷却（见 restoreCooldown）。
// 冷却时长与会话绑定表由池在 Pool.newAccount 中填入。
func newPoolAccount(service *Service, id string) *poolAccount {
	account := &poolAccount{id: id, service: service, routes: service.routes(), cooldown: DefaultAccountCooldown, effects: new(cooldownEffects)}
	account.restoreQuota()
	account.restoreCooldown(time.Now())
	return account
}

// newAccount 为池创建账号运行时状态，并填入池级的冷却时长与会话绑定表。
func (p *Pool) newAccount(service *Service, id string) *poolAccount {
	account := newPoolAccount(service, id)
	account.cooldown = p.cooldownDuration()
	account.affinity = p.affinity
	account.effects = &p.effects
	return account
}

// cooldownDuration 返回账号冷却时长：配置未设置或非正时为 DefaultAccountCooldown。
func (p *Pool) cooldownDuration() time.Duration {
	if p.config.AccountCooldown > 0 {
		return p.config.AccountCooldown
	}
	return DefaultAccountCooldown
}

// Pool 在多个 GitHub 账号之间分发请求：按优先级与额度档位分出最优档，档内做平滑加权轮询选择可用账号，并负责会话亲和（含每账号绑定数上限）、账号的运行时增删、禁用与调整优先级。
type Pool struct {
	apiKey     string // 所有账号共用的服务访问密钥
	logger     *slog.Logger
	config     PoolConfig     // 服务级配置，新增账号与设备登录时使用
	client     *http.Client   // 新增账号与设备登录访问 GitHub 的客户端
	idle       serviceRoutes  // 不访问上游的路由（健康检查、重定向、404/405），不依赖任何账号
	affinity   *affinityTable // 会话绑定表；关闭时为 nil
	risk       *riskTable     // 风控记录表（见 riskblock.go），随会话亲和开启；关闭时为 nil
	gate       keyGate        // 同一会话键的选账号、取 session、绑定串行化（见 pickSession），零值可用
	management http.Handler   // 管理接口；ManagementKey 为空时为 nil
	// generations 是生成请求的并发名额（缓冲 channel 作信号量），容量为 MaxConcurrentRequests；不限制时为 nil。
	generations chan struct{}

	requestSeq atomic.Uint64

	effects cooldownEffects // 进入冷却的异步副作用，见 Pool.WaitCooldownEffects

	adminMu  sync.Mutex // 串行化账号的增删、禁用、优先级与凭证文件写入
	mu       sync.Mutex // 保护 accounts 与各账号的 disabled、priority、swrrCurrent（选择会推进轮询状态，因此 choose 也持有它）
	accounts []*poolAccount

	loginMu sync.Mutex
	logins  map[string]*deviceLogin // 设备登录状态，见 management_login.go
}

// NewPool 校验所有账号配置并创建账号池，账号顺序即加入顺序（选择只看优先级、额度与文件名，与顺序无关）。会话亲和与管理接口均关闭。
// 所有账号的服务访问密钥去除首尾空白后必须一致且非空，GitHub 账号（Host 规范化后与 Token）不能重复。
func NewPool(configs []Config) (*Pool, error) {
	if len(configs) == 0 {
		return nil, errors.New("至少需要一个 GitHub Copilot 账号")
	}
	if strings.TrimSpace(configs[0].APIKey) == "" {
		return nil, errors.New("缺少服务访问密钥 APIKey")
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
// APIKey 去除首尾空白后为空，或 ManagementKey 与 APIKey 去除首尾空白后相同时返回错误。
func NewManagedPool(config PoolConfig, accounts []PoolAccount) (*Pool, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.ManagementKey = strings.TrimSpace(config.ManagementKey)
	if config.APIKey == "" {
		return nil, errors.New("缺少服务访问密钥 APIKey")
	}
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
			QuotaRefreshInterval: config.QuotaRefreshInterval, Client: config.Client, Logger: p.logger,
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
	account := p.newAccount(service, id)
	account.disabled = disabled
	account.priority = priority
	p.accounts = append(p.accounts, account)
	return nil
}

// newPool 按服务级配置创建空池，并按配置启用会话亲和、生成请求并发上限与管理接口。
// 开启会话亲和时同时启用风控记录；CacheDir 非空时启动即加载绑定文件与风控记录文件（见 loadSessionAffinity、loadRiskBlocks）。
func newPool(config PoolConfig) *Pool {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	p := &Pool{
		apiKey: config.APIKey, logger: logger, config: config, client: safeClient(config.Client),
		idle: idleRoutes(),
	}
	if config.MaxConcurrentRequests > 0 {
		p.generations = make(chan struct{}, config.MaxConcurrentRequests)
	}
	if config.SessionAffinityTTL > 0 {
		p.affinity = newAffinityTable(config.SessionAffinityTTL)
		p.loadSessionAffinity()
		p.risk = newRiskTable()
		p.loadRiskBlocks()
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
// 请求日志位于各自的鉴权之内（见 withRequestLog），未通过鉴权的请求不输出请求日志。
func (p *Pool) Handler() http.Handler {
	business := requireAPIKey(p.apiKey, p.log, withRequestLog(p.log, http.HandlerFunc(p.dispatch)))
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
	return withRequestID(p.logger, &p.requestSeq, handler)
}

// dispatch 按路由登记的上游访问方式分发请求：不访问上游的请求直接处理且不参与账号选择；
// 需要 session 的请求进入 forward。生成请求（登记了 upstream 的路由）先占用一个并发名额，名额已满时返回 503。
func (p *Pool) dispatch(w http.ResponseWriter, r *http.Request) {
	entry := p.routeOf(r)
	switch entry.need {
	case needNone:
		p.idle.mux.ServeHTTP(w, r)
	default:
		if entry.upstream != "" {
			if !p.acquireGeneration() {
				p.log(r.Context(), slog.LevelWarn, "生成请求并发数已达上限", "limit", p.config.MaxConcurrentRequests)
				writeError(w, http.StatusServiceUnavailable, "生成请求并发数已达上限，请稍后重试")
				return
			}
			defer p.releaseGeneration()
		}
		key, ok := p.affinityKey(r, entry.upstream)
		if !ok {
			p.log(r.Context(), slog.LevelWarn, "无法计算会话键")
			writeError(w, http.StatusInternalServerError, "无法计算模型缓存 key")
			return
		}
		p.forward(w, r, key)
	}
}

// acquireGeneration 尝试占用一个生成请求的名额，不等待；未限制时总是成功。
func (p *Pool) acquireGeneration() bool {
	if p.generations == nil {
		return true
	}
	select {
	case p.generations <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseGeneration 释放 acquireGeneration 占用的名额；只能在成功占用后调用。
func (p *Pool) releaseGeneration() {
	if p.generations != nil {
		<-p.generations
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

// affinityKey 计算生成请求的会话键。未开启亲和或非生成接口时不读取请求体，返回空键。
// 请求体最多读取 maxRequestBody 字节，读出的部分与剩余内容拼接后原样放回 r.Body，下游仍能读到完整请求体；
// 读取失败或超过上限时不计算会话键，返回空键，下游会照常返回错误（如 413）。
// 请求体读取成功但没有任何会话信号时 ok 为 false，调用方应拒绝该请求。
func (p *Pool) affinityKey(r *http.Request, upstream string) (key string, ok bool) {
	if upstream == "" || p.affinity == nil || r.Body == nil {
		return "", true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
	if err != nil || len(body) > maxRequestBody {
		return "", true
	}
	key = sessionKey(r, body, upstream)
	return key, key != ""
}

// forward 处理需要 session 的请求。先由 pickSession 选出账号、取得 session 并绑定会话键，再把请求交给该账号处理。
// 全部失败时按最后一个账号的 handleError 规则返回错误；没有任何可选账号时返回 503。
// 取 session 期间客户端已取消时不冷却账号；等待会话门时客户端已取消则不写响应。
// 请求体校验（生成接口）在预取 session 之后才执行，因此全部账号的 session 都失败时，
// 无效请求体会收到 session 错误而不是 400，这是有意的取舍。
//
// 会话键命中风控记录（见 riskblock.go）时，在会话门与选账号之前直接返回记录的状态码，不访问上游。
func (p *Pool) forward(w http.ResponseWriter, r *http.Request, key string) {
	if p.riskBlocked(r.Context(), w, key) {
		return
	}
	pick, err := p.pickSession(r, key)
	if err != nil {
		// 等待同会话门时客户端已取消：不选账号、不冷却、不计数，也不写响应。
		return
	}
	if pick.account != nil {
		p.serve(w, r.WithContext(pick.ctx), pick.account, key)
		return
	}
	if pick.last == nil {
		writeNoAccount(w)
		return
	}
	// 所有可尝试账号的 session 都失败：每次失败已在 pickSession 中计入，这里只更新最后尝试账号的统计并写出错误，不再计数。
	last := pick.last
	last.record(r.Context(), w, func(w http.ResponseWriter) { last.service.handleError(pick.ctx, w, pick.err) }, false)
}

// sessionPick 是 pickSession 的结果。
type sessionPick struct {
	account *poolAccount    // 取得 session 并已绑定的账号；nil 表示所有尝试的账号都失败，或没有可选账号
	ctx     context.Context // account 的请求上下文（带账号日志字段）；未尝试任何账号时为 nil
	last    *poolAccount    // 最后尝试的账号；nil 表示没有任何可选账号
	err     error           // last 的 session 错误
}

// pickSession 为请求选出账号、取得 session 并绑定会话键，供 forward 使用。
// 开启亲和且键非空时，选择、取 session、绑定都在同一会话键的门（keyGate）之内执行：同键的后到请求排队，
// 先到者完成绑定后，后到者按已有绑定选择同一账号，从而不会因竞争各选各的账号。
// 门通过 defer 在返回前释放，因此只覆盖选择、取 session 与绑定，不会持有到响应结束。
// 等待门时 ctx 被取消则返回错误，此时没有选择任何账号。
// 账号 session 获取失败时按错误规则计入（见 countSessionFailure），并在同一请求内改用其他尚未尝试的账号，每个账号最多尝试一次。
func (p *Pool) pickSession(r *http.Request, key string) (sessionPick, error) {
	if p.affinity != nil && key != "" {
		release, err := p.gate.acquire(r.Context(), key)
		if err != nil {
			return sessionPick{}, err
		}
		defer release()
	}

	var pick sessionPick
	tried := map[*poolAccount]bool{}
	for {
		account := p.choose(key, tried)
		if account == nil {
			return pick, nil
		}
		tried[account] = true
		pick.ctx = p.accountContext(r.Context(), account)
		pick.last = account
		if _, err := account.service.getSession(pick.ctx); err != nil {
			pick.err = err
			if pick.ctx.Err() != nil {
				return pick, nil
			}
			account.service.log(pick.ctx, slog.LevelWarn, "账号会话获取失败", "error", err)
			status, cooled := account.countSessionFailure(pick.ctx, err)
			p.recordRisk(pick.ctx, key, status, cooled)
			if !isUpstreamRateLimit(err) {
				account.refreshAfterError()
			}
			continue
		}
		// 取得 session 之后账号可能已因另一个请求的错误进入冷却：冷却账号不得被绑定。
		// 本次请求仍由它处理（它在冷却前已被选中，属于在途请求），之后的请求不会再选中它。
		if account.service.available(time.Now()) {
			p.bind(pick.ctx, key, account)
		}
		pick.account = account
		return pick, nil
	}
}

// serve 把请求交给已选定的账号处理，并记录该账号的统计；响应的 401/403 使账号进入冷却时记录会话键 key 的风控。
func (p *Pool) serve(w http.ResponseWriter, r *http.Request, account *poolAccount, key string) {
	status, cooled := account.record(r.Context(), w, func(w http.ResponseWriter) { account.routes.mux.ServeHTTP(w, r) }, true)
	p.recordRisk(r.Context(), key, status, cooled)
}

// record 记录账号处理了一次请求：请求数加 1，最近使用时间更新，然后按结果更新统计与连续错误计数：
//   - 客户端已取消（reqCtx 为 context.Canceled）：不计数、不清零、不刷新额度，账号也不会因此冷却；
//   - 上游的非错误状态（3xx，或标记为 failureIgnored）：既不计数也不清零；
//   - 网络错误（写出的 502 或上游成功响应的正文中断，标记为 failureNetwork）：失败数 +1，原因为 network 计入连续错误，异步刷新额度；
//   - 成功（< 300）：清零连续错误计数；
//   - 其他 >= 400：失败数 +1，按状态码计入连续错误（见 ruleOf）；非 429 时异步刷新额度，429 是限流，不刷新。
//
// countErrors 为 false 时只更新统计：会话获取失败已在 pickSession 中计入，同一请求不得再计一次。
// 返回写给客户端的状态码与本次是否使账号进入冷却。
func (a *poolAccount) record(reqCtx context.Context, w http.ResponseWriter, handle func(http.ResponseWriter), countErrors bool) (status int, cooled bool) {
	a.requests.Add(1)
	a.lastUsed.Store(time.Now().UnixNano())
	writer := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	handle(writer)
	if errors.Is(reqCtx.Err(), context.Canceled) {
		return writer.status, false
	}
	switch {
	case writer.failure == failureIgnored, writer.status >= http.StatusMultipleChoices && writer.status < http.StatusBadRequest:
		// 上游的重定向等非错误状态：既不计数也不清零。
	case writer.failure == failureNetwork:
		a.failures.Add(1)
		if countErrors {
			cooled = a.countFailure(reqCtx, ruleCounted, cooldownReasonNetwork)
		}
		a.refreshAfterError()
	case writer.status < http.StatusMultipleChoices:
		a.resetStreak()
	default:
		a.failures.Add(1)
		if countErrors {
			cooled = a.countFailure(reqCtx, ruleOf(writer.status), strconv.Itoa(writer.status))
		}
		if writer.status != http.StatusTooManyRequests {
			a.refreshAfterError()
		}
	}
	return writer.status, cooled
}

// bind 把会话键绑定（或续期）到实际处理请求的账号。键为空或未开启亲和时不做任何事。
// 绑定前先查询原绑定，并按结果输出亲和日志：没有有效原绑定为 Info「新建绑定」，原绑定即该账号为 Debug「命中」，
// 原绑定是其他账号为 Info「切换账号」。日志只含会话键前 12 个字符与账号 ID，不含完整会话键。
// 调用方（pickSession）须持有该会话键的门，使同键的 lookup 与 bind 不交错。
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

// writeNoAccount 在没有任何可选账号（0 个、全部禁用或全部耗尽）时返回 503。
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

// choose 选择处理本次请求的账号。键非空且开启亲和时：会话已绑定且该账号仍可用（未禁用、未冷却、未耗尽、本请求未尝试过）则直接使用它，且不推进轮询；
// 否则为无可用绑定的会话（新会话或故障切换）选择：先取最优档，排除有效绑定数已达上限的账号后再在档内轮询，见下方说明。
// 键为空或未开启亲和时按 pickLocked 选择。没有候选（包括全部冷却）时返回 nil。
func (p *Pool) choose(key string, tried map[*poolAccount]bool) *poolAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if key == "" || p.affinity == nil {
		return p.pickLocked(now, tried)
	}
	if id, ok := p.affinity.lookup(key, now); ok {
		if account := p.findLocked(id); account != nil && !account.disabled &&
			!tried[account] && account.service.available(now) && !account.exhausted(now) {
			return account
		}
	}
	// 排除已满账号后仍有候选时采用排除后的结果；没有候选（例如其余账号都已禁用、冷却或耗尽）时忽略上限，仍按原候选轮询，
	// 因此上限不会让会话落到没有候选的情况，也不会把会话推向冷却中的账号（冷却账号不在候选内）。上限为 0 时 overCapLocked 返回 nil，不做排除。
	group := p.topGroupLocked(now, tried)
	if skip := p.overCapLocked(now, tried); skip != nil {
		if capped := p.topGroupLocked(now, skip); len(capped) > 0 {
			group = capped
		}
	}
	return swrrPickLocked(group)
}

// selectKey 是账号参与选择时的排序信息：priority 与 weight.hasCredits 决定档位（见 tierBefore）；
// weight.fraction 决定档内 SWRR 的权重；file 是 SWRR 当前值并列时的先后。冷却中的账号不参与选择，因此不需要冷却状态。
type selectKey struct {
	priority int         // 优先级，数值大者在前
	weight   quotaWeight // 额度权重，见 weigh
	file     string      // 凭证文件名（用户名 + ".json"，无用户名时用 ID），字节序升序
}

// keyOf 计算账号的排序信息。调用方须持有 p.mu（读取 priority）。额度权重只读取缓存，不访问上游。
func keyOf(a *poolAccount, now time.Time) selectKey {
	name := a.service.config.User
	if name == "" {
		name = a.id
	}
	quota, _ := a.quota.snapshot()
	return selectKey{
		priority: a.priority,
		weight:   weigh(quota),
		file:     name + ".json",
	}
}

// tierBefore 报告 k 的档位是否严格优于 o：优先级高者在前，再次有 Credits 者在前。
// 档位相同时返回 false，档内的先后由 SWRR 决定。
func (k selectKey) tierBefore(o selectKey) bool {
	if k.priority != o.priority {
		return k.priority > o.priority
	}
	return k.weight.hasCredits && !o.weight.hasCredits
}

// candidate 是一次选择中参与比较的账号及其排序信息。
type candidate struct {
	account *poolAccount
	key     selectKey
}

// topGroupLocked 返回最优档的全部候选，调用方须持有 p.mu。候选为未禁用、未冷却、未被 skip 排除且未耗尽（见 poolAccount.exhausted）的账号；
// 按 tierBefore 找出最优档，返回与之同档的全部候选，顺序为账号在池中的顺序。没有候选时返回 nil。只读取状态，不推进轮询。
func (p *Pool) topGroupLocked(now time.Time, skip map[*poolAccount]bool) []candidate {
	var group []candidate
	for _, a := range p.accounts {
		if a.disabled || skip[a] || !a.service.available(now) || a.exhausted(now) {
			continue
		}
		c := candidate{account: a, key: keyOf(a, now)}
		if len(group) > 0 && group[0].key.tierBefore(c.key) {
			continue // 比最优档差
		}
		if len(group) == 0 || c.key.tierBefore(group[0].key) {
			group = group[:0] // 出现更优的档，之前的候选全部作废
		}
		group = append(group, c)
	}
	return group
}

// swrrPickLocked 在同档的 group 内做一次平滑加权轮询（算法同 nginx），返回被选中的账号；group 为空时返回 nil。调用方须持有 p.mu。
// 每个成员的 swrrCurrent 加上自身权重（见 quotaWeight.swrrWeight），取 swrrCurrent 最大者，并列时取文件名字节序最小者；
// 被选中者的 swrrCurrent 再减去组内总权重。只修改组内成员的 swrrCurrent，状态只在内存中，不会因此重置。
func swrrPickLocked(group []candidate) *poolAccount {
	if len(group) == 0 {
		return nil
	}
	total := 0
	var best *candidate
	for i := range group {
		c := &group[i]
		weight := c.key.weight.swrrWeight()
		c.account.swrrCurrent += weight
		total += weight
		if best == nil || c.account.swrrCurrent > best.account.swrrCurrent ||
			(c.account.swrrCurrent == best.account.swrrCurrent && c.key.file < best.key.file) {
			best = c
		}
	}
	best.account.swrrCurrent -= total
	return best.account
}

// pickLocked 选出本次应使用的账号，调用方须持有 p.mu：先由 topGroupLocked 取最优档，再由 swrrPickLocked 在档内轮询。
// 冷却中与已禁用、已耗尽的账号不参与选择。没有候选时返回 nil。
func (p *Pool) pickLocked(now time.Time, tried map[*poolAccount]bool) *poolAccount {
	return swrrPickLocked(p.topGroupLocked(now, tried))
}

// overCapLocked 返回不应接收无可用绑定会话的账号集合：本请求已尝试过的账号，以及有效绑定数已达 SessionAffinityMaxPerAccount 的账号。
// 上限 <=0 或未开启亲和时返回 nil，表示不做排除。有效绑定数来自 affinityTable.activeCounts。调用方须持有 p.mu（锁顺序为先 p.mu 后 affinity.mu）。
func (p *Pool) overCapLocked(now time.Time, tried map[*poolAccount]bool) map[*poolAccount]bool {
	limit := p.config.SessionAffinityMaxPerAccount
	if limit <= 0 || p.affinity == nil {
		return nil
	}
	counts := p.affinity.activeCounts(now)
	skip := make(map[*poolAccount]bool, len(tried))
	maps.Copy(skip, tried)
	for _, a := range p.accounts {
		if counts[a.id] >= limit {
			skip[a] = true
		}
	}
	return skip
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
