package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ghc2api/web"
)

// maxManagementBody 是管理接口请求体的上限。
const maxManagementBody = 1 << 20

// quotaRefreshTimeout 是一次额度刷新（列表中的并发刷新、后台定时刷新、请求失败后的异步刷新或单账号强制刷新）的总超时。
// 声明为变量以便测试临时缩短，实现中不得改为常量。
var quotaRefreshTimeout = 10 * time.Second

// errAccountExists 表示账号（登录名忽略大小写或 Host+Token）已在池中，或凭证目录中已有同名文件。
var errAccountExists = errors.New("账号已存在")

// consoleCSP 限制控制台页面可加载的资源并禁止被嵌入。控制台是单文件构建，内联了 script 与 style，因此允许 'unsafe-inline'。
const consoleCSP = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; frame-ancestors 'none'"

// AccountView 是管理接口返回的账号视图，不包含 OAuth token。
type AccountView struct {
	ID            string     `json:"id"`
	User          string     `json:"user"`
	Host          string     `json:"host"` // 空表示 github.com
	Disabled      bool       `json:"disabled"`
	Priority      int        `json:"priority"`                 // 优先级，取值 MinPriority 到 MaxPriority；数值越大越优先被选择
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"` // 仅在冷却中时输出
	Stats         struct {
		Requests   uint64     `json:"requests"`
		Errors     uint64     `json:"errors"`
		LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	} `json:"stats"`
	Quota      *Quota `json:"quota,omitempty"`
	QuotaError string `json:"quota_error,omitempty"`
}

// managementAPIPrefix 是需要管理密钥的接口路径前缀；控制台页面 /management 不需要鉴权。
const managementAPIPrefix = "/v0/management/"

// newManagementHandler 注册管理路由。控制台页面无需鉴权，managementAPIPrefix 下的接口要求 Authorization: Bearer <管理密钥>。
// 鉴权位于请求日志之外：未通过鉴权的请求只输出鉴权告警；其余请求（包括 ServeMux 自身产生的 405、404 与路径清理重定向）都输出请求日志。
func (p *Pool) newManagementHandler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /v0/management/version", handleVersion)
	api.HandleFunc("GET /v0/management/accounts", p.handleListAccounts)
	api.HandleFunc("POST /v0/management/accounts", p.handleImportAccount)
	api.HandleFunc("PATCH /v0/management/accounts", p.handlePatchAccount)
	api.HandleFunc("DELETE /v0/management/accounts", p.handleDeleteAccount)
	api.HandleFunc("POST /v0/management/quota/refresh", p.handleRefreshQuota)
	api.HandleFunc("POST /v0/management/login", p.handleStartLogin)
	api.HandleFunc("GET /v0/management/login/status", p.handleLoginStatus)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /management", serveConsole)
	mux.Handle(managementAPIPrefix, api)
	return requireManagementKey(p.config.ManagementKey, p.log, withRequestLog(p.log, mux))
}

// isManagementPath 报告路径是否属于管理接口（控制台页面或 managementAPIPrefix 下的接口）。
func isManagementPath(path string) bool {
	return path == "/management" || strings.HasPrefix(path, managementAPIPrefix)
}

// requireManagementKey 校验管理接口的管理密钥，只作用于 managementAPIPrefix 下的路径，其他路径直接交给 next。
// 只接受 Authorization: Bearer，不接受 X-Api-Key，因此服务访问密钥无法用来访问管理接口。
// 校验在 next（含请求日志）之外，因此路径清理等 ServeMux 行为不会绕过它。
// key 为空时拒绝所有管理接口请求（空密钥与 "Bearer " 后的空值哈希相同，不能交给 secretMatches）。
func requireManagementKey(key string, logf logFunc, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, managementAPIPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		value, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" || !found || !secretMatches(key, value) {
			logAuthFailure(logf, r, "管理接口鉴权失败")
			writeError(w, http.StatusUnauthorized, "管理密钥无效或缺失")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveConsole 返回内嵌的管理控制台页面。页面本身不含密钥，密钥由用户输入后随请求发送。
// 同时设置 CSP 与 X-Frame-Options，防止控制台被其他站点嵌入（点击劫持）。
func serveConsole(w http.ResponseWriter, _ *http.Request) {
	page, err := web.Index()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "管理控制台不可用")
		return
	}
	w.Header().Set("Content-Security-Policy", consoleCSP)
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// decodeManagementJSON 解码管理接口的 JSON 请求体（上限 1 MiB）。失败时已写入错误响应并返回 false。
func decodeManagementJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManagementBody)).Decode(value)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "请求体超过 1 MiB")
	} else {
		writeError(w, http.StatusBadRequest, "请求体必须是合法的 JSON")
	}
	return false
}

// handleListAccounts 返回全部账号。距上次尝试已超过刷新间隔的账号并发刷新（总超时 quotaRefreshTimeout），刷新失败不影响结果。
func (p *Pool) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), quotaRefreshTimeout)
	defer cancel()
	refreshQuotas(ctx, p.accountsSnapshot())
	writeJSON(w, http.StatusOK, p.accountViews())
}

// refreshQuotas 并发刷新账号额度；是否真正请求上游由各账号的缓存决定。错误已记录在账号的额度缓存中。
func refreshQuotas(ctx context.Context, accounts []*poolAccount) {
	var wg sync.WaitGroup
	for _, account := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = account.refresh(ctx, false)
		}()
	}
	wg.Wait()
}

// handleImportAccount 导入凭证：校验 token 与 host，用 token 查询 GitHub 登录名并确认订阅，
// 通过后以登录名保存凭证文件并追加到池末尾。上游校验失败时错误信息不包含 token。
func (p *Pool) handleImportAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Credential Credential `json:"credential"`
	}
	if !decodeManagementJSON(w, r, &req) {
		return
	}
	token := strings.TrimSpace(req.Credential.Token)
	if token == "" {
		writeError(w, http.StatusBadRequest, "凭证缺少 oauth_token")
		return
	}
	host := p.config.Host
	if host == "" {
		host = req.Credential.Host
	}
	host, err := NormalizeHost(host)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	login, err := verifyGitHubAccount(ctx, p.client, host, token)
	cancel()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if user := strings.TrimSpace(req.Credential.User); user != "" && !strings.EqualFold(user, login) {
		writeError(w, http.StatusBadRequest, "凭证中的用户名与 GitHub 登录名不一致")
		return
	}
	if !ValidGitHubUserName(login) {
		writeError(w, http.StatusBadRequest, "GitHub 登录名不是合法的用户名，不能用作凭证文件名")
		return
	}
	account, err := p.addVerifiedAccount(token, host, login)
	if errors.Is(err, errAccountExists) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存凭证失败: "+err.Error())
		return
	}
	p.log(r.Context(), slog.LevelInfo, "管理接口：账号已导入", "id", account.id)
	writeJSON(w, http.StatusOK, p.accountView(account))
}

// handlePatchAccount 更新账号的 disabled 与 priority，两者至少提供一个，未提供的字段保持不变。
// 先重写凭证文件，成功后才更新内存状态；写入失败时返回 500 且状态不变。
func (p *Pool) handlePatchAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Disabled *bool  `json:"disabled"`
		Priority *int   `json:"priority"`
	}
	if !decodeManagementJSON(w, r, &req) {
		return
	}
	if req.ID == "" || (req.Disabled == nil && req.Priority == nil) {
		writeError(w, http.StatusBadRequest, "请求必须包含 id，且 disabled 与 priority 至少提供一个")
		return
	}
	if req.Priority != nil {
		if err := checkPriority(*req.Priority); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	p.adminMu.Lock()
	defer p.adminMu.Unlock()
	account := p.findAccount(req.ID)
	if account == nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	// adminMu 串行化了所有修改，此处读到的 disabled 与 priority 在写入期间不会被其他请求改变。
	p.mu.Lock()
	disabled, priority := account.disabled, account.priority
	p.mu.Unlock()
	if req.Disabled != nil {
		disabled = *req.Disabled
	}
	if req.Priority != nil {
		priority = *req.Priority
	}
	if err := setCredentialState(p.credentialPath(account), disabled, priority); err != nil {
		writeError(w, http.StatusInternalServerError, "写入凭证失败: "+err.Error())
		return
	}
	p.mu.Lock()
	account.disabled, account.priority = disabled, priority
	p.mu.Unlock()
	p.log(r.Context(), slog.LevelInfo, "管理接口：账号已更新", "id", account.id, "disabled", disabled, "priority", priority)
	writeJSON(w, http.StatusOK, p.accountView(account))
}

// handleDeleteAccount 删除账号：先删除凭证文件（文件已不存在也视为成功），再从池中移除。
// 正在处理的请求不受影响。
func (p *Pool) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少账号 id 参数")
		return
	}
	p.adminMu.Lock()
	defer p.adminMu.Unlock()
	account := p.findAccount(id)
	if account == nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if err := os.Remove(p.credentialPath(account)); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "删除凭证失败: "+err.Error())
		return
	}
	p.removeAccount(account)
	// 缓存目录中保存该账号的 Copilot session 与模型列表，删除账号时一并清理；之后进行中的请求不再写入缓存。
	if err := account.service.removeCache(); err != nil {
		p.log(r.Context(), slog.LevelWarn, "清理已删除账号的缓存失败", "id", account.id, "error", err.Error())
	}
	p.log(r.Context(), slog.LevelInfo, "管理接口：账号已删除", "id", account.id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRefreshQuota 强制刷新单个账号的额度。限流时保留旧数据并返回 200；其他错误在有旧数据时返回 200 并带 quota_error，
// 没有旧数据时返回 502。
func (p *Pool) handleRefreshQuota(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodeManagementJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "请求必须包含 id")
		return
	}
	account := p.findAccount(req.ID)
	if account == nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), quotaRefreshTimeout)
	err := account.refresh(ctx, true)
	cancel()
	if err != nil && !account.quota.hasData() {
		writeError(w, http.StatusBadGateway, account.quotaErrorText(err))
		return
	}
	writeJSON(w, http.StatusOK, p.accountView(account))
}

// addVerifiedAccount 保存已通过 GitHub 校验的凭证，并把账号追加到池末尾。新账号的优先级为 0。
// 入池后立即在后台强制刷新一次额度（不阻塞调用方，失败只记入额度缓存），因此响应中的账号视图可能尚无额度。
// 登录名（忽略大小写）或 Host+Token 已存在、凭证目录中已有同名文件时返回 errAccountExists，且不写入凭证。
// 登录名须通过 ValidGitHubUserName，因为它会用作凭证文件名。
func (p *Pool) addVerifiedAccount(token, host, login string) (*poolAccount, error) {
	if !ValidGitHubUserName(login) {
		return nil, errors.New("GitHub 登录名不是合法的用户名，不能用作凭证文件名")
	}
	p.adminMu.Lock()
	defer p.adminMu.Unlock()
	service, err := New(Config{
		Token: token, Host: host, User: login, APIKey: p.apiKey, CacheDir: p.config.CacheDir,
		ModelsCacheTTL: p.config.ModelsCacheTTL, QuotaRefreshInterval: p.config.QuotaRefreshInterval,
		Client: p.config.Client, Logger: p.logger,
	})
	if err != nil {
		return nil, err
	}
	id := strings.ToLower(login)
	p.mu.Lock()
	duplicate := p.conflicts(service, id)
	p.mu.Unlock()
	if duplicate {
		return nil, errAccountExists
	}
	// 文件可能是运行期间放入、尚未加载的，因此扫描目录，而不是只看账号列表。
	exists, err := p.credentialFileExists(login)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("%w：凭证目录中已存在同名文件", errAccountExists)
	}
	credential := Credential{Token: token, Host: service.config.Host, User: login}
	if err := SaveCredential(p.credentialPathFor(login), credential); err != nil {
		return nil, err
	}
	account := newPoolAccount(service, id)
	p.mu.Lock()
	p.accounts = append(p.accounts, account)
	p.mu.Unlock()
	// 账号可能从缓存恢复了较新的 checked_at，因此不节流，入池后立即强制刷新。
	account.refreshAsync(0)
	return account, nil
}

// credentialFileExists 扫描凭证目录，报告是否已有同名（忽略大小写）的凭证文件。
func (p *Pool) credentialFileExists(login string) (bool, error) {
	entries, err := os.ReadDir(p.config.CredentialsDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), login+".json") {
			return true, nil
		}
	}
	return false, nil
}

// credentialPath 返回账号的凭证文件路径。
func (p *Pool) credentialPath(account *poolAccount) string {
	return p.credentialPathFor(account.service.config.User)
}

// credentialPathFor 返回 GitHub 登录名对应的凭证文件路径：<CredentialsDir>/<登录名>.json，登录名保留原有大小写。
func (p *Pool) credentialPathFor(login string) string {
	return filepath.Join(p.config.CredentialsDir, login+".json")
}

// setCredentialState 重写凭证文件中的 disabled 与 priority 字段，其他字段保持不变；disabled 为 false 时该字段被省略，priority 为 0 时同样省略。
func setCredentialState(path string, disabled bool, priority int) error {
	credential, err := LoadCredential(path)
	if err != nil {
		return err
	}
	credential.Disabled = disabled
	credential.Priority = priority
	return SaveCredential(path, credential)
}

// accountViews 返回全部账号的视图，顺序与账号列表顺序一致。
func (p *Pool) accountViews() []AccountView {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	views := make([]AccountView, 0, len(p.accounts))
	for _, account := range p.accounts {
		views = append(views, account.view(now))
	}
	return views
}

// accountView 返回单个账号的视图。
func (p *Pool) accountView(account *poolAccount) AccountView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return account.view(time.Now())
}

// view 生成账号视图。调用方须持有 p.mu（读取 disabled 与 priority）。
func (a *poolAccount) view(now time.Time) AccountView {
	view := AccountView{
		ID: a.id, User: a.service.config.User, Host: a.service.config.Host, Disabled: a.disabled, Priority: a.priority,
	}
	if until := a.service.cooldownUntil.Load(); until > now.UnixNano() {
		cooldown := time.Unix(0, until).UTC()
		view.CooldownUntil = &cooldown
	}
	view.Stats.Requests = a.requests.Load()
	view.Stats.Errors = a.failures.Load()
	if last := a.lastUsed.Load(); last != 0 {
		used := time.Unix(0, last).UTC()
		view.Stats.LastUsedAt = &used
	}
	view.Quota, view.QuotaError = a.quota.snapshot()
	return view
}
