package copilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxRequestBody = 16 << 20
const maxMetadataBody = 4 << 20

// Service 独立保存一个账号的 session、模型和 Auto 缓存。
type Service struct {
	config Config
	client *http.Client
	logger *slog.Logger

	cacheDir       string
	modelsCacheTTL time.Duration
	// quotaRefreshInterval 是额度非强制刷新的节流窗口（见 quota.go 的 refresh）。
	quotaRefreshInterval time.Duration
	// cacheMu 串行化缓存写入与账号删除，cacheRemoved 为账号已删除的标记（见 writeCache、removeCache）。
	cacheMu      sync.RWMutex
	cacheRemoved bool

	requestSeq atomic.Uint64
	// cooldownUntil 是账号冷却截止时间（UnixNano），0 表示从未冷却，见 cooldown.go。
	cooldownUntil atomic.Int64
	secretsMu     sync.RWMutex
	secrets       map[string]struct{}

	sessionMu contextMutex
	session   session
	modelsMu  contextMutex
	models    modelList
	autoMu    contextMutex
	autoState sync.Mutex
	auto      autoSession
	refusedMu sync.Mutex
	refused   map[string]time.Time
}

// New 不发网络请求；凭证的有效性在首次访问上游时检查。
func New(config Config) (*Service, error) {
	config.Token = strings.TrimSpace(config.Token)
	if config.Token == "" {
		return nil, errors.New("缺少 GitHub OAuth token")
	}
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, errors.New("缺少服务访问密钥 APIKey")
	}
	host, err := NormalizeHost(config.Host)
	if err != nil {
		return nil, err
	}
	config.Host = host
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	cacheRoot := strings.TrimSpace(config.CacheDir)
	if cacheRoot == "" {
		var cacheErr error
		cacheRoot, cacheErr = os.UserCacheDir()
		if cacheErr != nil {
			config.Logger.Warn("无法确定本地缓存目录，缓存将只保存在内存中", "error", cacheErr)
			cacheRoot = ""
		} else {
			cacheRoot = filepath.Join(cacheRoot, "ghc2api")
		}
	}
	cacheDir := ""
	if cacheRoot != "" {
		accountKey := sha256.Sum256([]byte(config.Host + "\x00" + config.Token))
		cacheDir = filepath.Join(cacheRoot, fmt.Sprintf("%x", accountKey))
	}
	modelsCacheTTL := config.ModelsCacheTTL
	if modelsCacheTTL <= 0 {
		modelsCacheTTL = DefaultModelsCacheTTL
	}
	quotaRefreshInterval := config.QuotaRefreshInterval
	if quotaRefreshInterval <= 0 {
		quotaRefreshInterval = DefaultQuotaRefreshInterval
	}
	service := &Service{
		config: config, client: safeClient(config.Client), refused: make(map[string]time.Time),
		logger: config.Logger, secrets: make(map[string]struct{}),
		cacheDir: cacheDir, modelsCacheTTL: modelsCacheTTL, quotaRefreshInterval: quotaRefreshInterval,
		sessionMu: make(contextMutex, 1), modelsMu: make(contextMutex, 1), autoMu: make(contextMutex, 1),
	}
	service.addSecret(config.Token)
	service.addSecret(config.APIKey)
	return service, nil
}

// Handler 返回原协议 HTTP 接口，不在三种生成协议之间转换。鉴权在请求日志之外：未通过鉴权的请求不输出请求日志。
func (s *Service) Handler() http.Handler {
	return withRequestID(s.logger, &s.requestSeq, requireAPIKey(s.config.APIKey, s.log, withRequestLog(s.log, s.routes().mux)))
}

// routeNeed 描述路由访问上游的方式，决定 Pool 如何分发请求。
type routeNeed int

const (
	needNone    routeNeed = iota // 不访问上游（健康检查、未注册路由、重定向等）：由 Pool 的 idle 路由直接处理，不参与账号选择
	needSession                  // 需要 Copilot session（模型列表、生成请求）：预取 session，失败时切换账号
)

// routeEntry 登记一条业务路由的路径及其上游访问方式。
type routeEntry struct {
	path     string
	need     routeNeed
	upstream string // 生成接口的上游路径（用于会话亲和）；其他路由为空
}

// serviceRoutes 是 Service 的业务路由。entries 以 ServeMux 的 pattern（如 "GET /v1/models"）为键，
// 与路由注册在同一处登记，避免另外维护一份路由表。
type serviceRoutes struct {
	mux     *http.ServeMux
	entries map[string]routeEntry
}

// routes 注册业务路由，不含鉴权与请求日志；Pool 复用它在选定账号后分发请求。
func (s *Service) routes() serviceRoutes {
	routes := serviceRoutes{mux: http.NewServeMux(), entries: map[string]routeEntry{}}
	routes.handle("GET", "/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}, routeEntry{need: needNone})
	for _, path := range []string{"/models", "/v1/models"} {
		routes.handle("GET", path, s.handleModels, routeEntry{need: needSession})
	}
	for local, upstream := range map[string]string{
		"/chat/completions": "/chat/completions", "/v1/chat/completions": "/chat/completions",
		"/responses": "/responses", "/v1/responses": "/responses", "/v1/messages": "/v1/messages",
	} {
		routes.handle("POST", local, func(w http.ResponseWriter, r *http.Request) {
			s.handleGeneration(w, r, upstream)
		}, routeEntry{need: needSession, upstream: upstream})
	}
	return routes
}

// handle 注册一条路由并登记其访问上游的方式；entry 的 path 由本函数填入。
func (t serviceRoutes) handle(method, path string, handler http.HandlerFunc, entry routeEntry) {
	pattern := method + " " + path
	t.mux.HandleFunc(pattern, handler)
	entry.path = path
	t.entries[pattern] = entry
}

// requireAPIKey 对除 /healthz、/favicon.ico 外的请求校验服务访问密钥。apiKey 为空时拒绝所有需鉴权的请求（失败即拒绝），
// 构造函数也会拒绝空密钥，这里是第二道防线。
// Service 与 Pool 共用此中间件，未通过时输出鉴权告警（见 logAuthFailure）并返回 401。
func requireAPIKey(apiKey string, logf logFunc, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		skipped := r.URL.Path == "/healthz" || r.URL.Path == "/favicon.ico"
		if !skipped && !authorizedRequest(apiKey, r) {
			logAuthFailure(logf, r, "客户端鉴权失败")
			writeError(w, http.StatusUnauthorized, "服务访问密钥无效或缺失")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorizedRequest 检查请求是否携带与 apiKey 相同的 Bearer 或 X-Api-Key 密钥。
// apiKey 为空时一律返回 false：否则空密钥与缺失的请求头哈希相同，会被当作匹配。
func authorizedRequest(apiKey string, r *http.Request) bool {
	if apiKey == "" {
		return false
	}
	bearer := ""
	if value := r.Header.Get("Authorization"); strings.HasPrefix(value, "Bearer ") {
		bearer = strings.TrimPrefix(value, "Bearer ")
	}
	return secretMatches(apiKey, bearer) || secretMatches(apiKey, r.Header.Get("X-Api-Key"))
}

// secretMatches 以常量时间比较两个密钥（先哈希，使比较时间与长度无关）。
func secretMatches(expected, actual string) bool {
	e := sha256.Sum256([]byte(expected))
	a := sha256.Sum256([]byte(actual))
	return subtle.ConstantTimeCompare(e[:], a[:]) == 1
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": "copilot_proxy_error"}})
}

type upstreamError struct {
	status int
	body   []byte
	header http.Header
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("上游请求失败（HTTP %d）", e.status)
}

// isUpstreamRateLimit 报告错误是否为上游返回的 429（限流）。
func isUpstreamRateLimit(err error) bool {
	var upstream *upstreamError
	return errors.As(err, &upstream) && upstream.status == http.StatusTooManyRequests
}

// handleError 把错误写给客户端，并标记失败性质供账号计数（见 poolAccount.record）：
//   - 上游返回错误状态（>= 400）：原样返回上游的状态码与正文，按上游状态码计数；
//   - 上游返回非错误状态（如 3xx）：以 502 写出，但标记为忽略，既不计数也不清零；
//   - 其他错误（网络错误等）：以 502 写出，标记为网络错误，冷却原因记为 network 而不是 502。
func (s *Service) handleError(ctx context.Context, w http.ResponseWriter, err error) {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		if upstream.status < http.StatusBadRequest {
			s.log(ctx, slog.LevelWarn, "上游返回非错误状态，已按 502 写给客户端", "status", upstream.status)
			markFailure(w, failureIgnored)
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		level := slog.LevelWarn
		if upstream.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		s.log(ctx, level, "Copilot API 返回错误状态", "status", upstream.status)
		writeUpstreamError(w, upstream)
		return
	}
	s.log(ctx, slog.LevelError, "请求处理失败", "error", s.redactString(err.Error()))
	markFailure(w, failureNetwork)
	writeError(w, http.StatusBadGateway, err.Error())
}

// writeUpstreamError 原样返回上游的错误状态与正文。正文为空（包括读取失败）时写 JSON 错误体，格式与 writeError 一致。
// 不转发 Content-Length：正文可能读取不完整，声明的长度与实际写出的正文不一致会让客户端读取出错。
func writeUpstreamError(w http.ResponseWriter, upstream *upstreamError) {
	if len(upstream.body) == 0 {
		writeError(w, upstream.status, fmt.Sprintf("上游返回 %d，但响应正文为空或读取失败", upstream.status))
		return
	}
	copyResponseHeaders(w.Header(), upstream.header)
	w.Header().Del("Content-Length")
	w.WriteHeader(upstream.status)
	_, _ = w.Write(upstream.body)
}

func readLimited(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("上游响应超过大小限制")
	}
	return data, nil
}

func (s *Service) apiRequest(ctx context.Context, session session, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, session.API+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	session.setHeaders(req.Header)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (s *Service) fetchJSON(ctx context.Context, session session, method, path string, body []byte, value any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := s.apiRequest(ctx, session, method, path, body)
	if err != nil {
		return err
	}
	res, err := s.doAPI(ctx, req)
	if err != nil {
		return errors.New("Copilot 网络请求失败: " + err.Error())
	}
	defer res.Body.Close()
	data, err := readLimited(res.Body, maxMetadataBody)
	if res.StatusCode/100 != 2 {
		// 错误状态按上游状态码处理，即使正文读取失败。
		return &upstreamError{res.StatusCode, data, res.Header}
	}
	if err != nil {
		return err
	}
	if value == nil {
		return nil
	}
	if err := json.Unmarshal(data, value); err != nil {
		return errors.New("Copilot 返回了无效的 JSON")
	}
	return nil
}

func (s *Service) handleGeneration(w http.ResponseWriter, r *http.Request, path string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.log(r.Context(), slog.LevelWarn, "生成请求体超过大小限制", "limit_bytes", maxRequestBody)
			writeError(w, http.StatusRequestEntityTooLarge, "请求体超过 16 MiB")
		} else {
			s.log(r.Context(), slog.LevelWarn, "读取生成请求体失败", "error", err)
			writeError(w, http.StatusBadRequest, "无法读取请求体")
		}
		return
	}
	if path == "/responses" {
		body = withoutLoneToolChoice(body)
	}
	var request map[string]json.RawMessage
	var model string
	if json.Unmarshal(body, &request) != nil || request == nil ||
		json.Unmarshal(request["model"], &model) != nil || strings.TrimSpace(model) == "" {
		s.log(r.Context(), slog.LevelWarn, "生成请求 JSON 或 model 字段无效", "bytes", len(body))
		writeError(w, http.StatusBadRequest, "请求必须是 JSON 对象，并包含非空字符串 model")
		return
	}
	// 写超时只针对响应写入，因此包装放在读取请求体之后（读取阶段不写响应）。请求结束时不清除截止时间，见 generationWriter。
	// 注意：http.MaxBytesReader 超限时只有底层 w 实现 requestTooLarge 才会标记关闭连接（net/http 的 *response 实现了它）。
	// 这里的 w 已被 withRequestLog（Pool 另有 record）包装为 statusResponseWriter，不实现该方法，
	// 因此 Service 与 Pool 两条路径下超限都只返回 413，不会由 MaxBytesReader 关闭连接。
	w = newGenerationWriter(w)
	isAuto := model == "auto"
	// X-Initiator 与 Copilot-Vision-Request 只取决于客户端的原始请求体，与 Auto 重试无关，因此在循环外只计算一次。
	xInitiator := initiator(body)
	vision := hasVision(body)
	s.log(r.Context(), slog.LevelInfo, "开始处理生成请求", "model", model, "upstream_path", path, "request_bytes", len(body))
	for attempt := 0; attempt < 3; attempt++ {
		current, err := s.getSession(r.Context())
		if err != nil {
			s.handleError(r.Context(), w, err)
			return
		}
		// 手选模型在列表失败时由上游判断；Auto 会在解析后重试读取列表。
		list, modelErr := s.getModels(r.Context(), current)
		if modelErr != nil {
			s.log(r.Context(), slog.LevelWarn, "读取模型列表失败", "error", s.redactString(modelErr.Error()))
		}
		outBody := body
		var pick autoSession
		if isAuto {
			s.log(r.Context(), slog.LevelInfo, "开始解析 Auto 模型", "attempt", attempt+1)
			pick, err = s.resolveAuto(r.Context(), current, promptFrom(body))
			if err != nil {
				s.handleError(r.Context(), w, err)
				return
			}
			model = pick.Model
			s.log(r.Context(), slog.LevelInfo, "Auto 模型已解析", "model", model)
			request["model"], _ = json.Marshal(model)
			outBody, _ = json.Marshal(request)
		}
		if isAuto && modelErr != nil {
			// Auto 接口可能成功而模型列表失败；重试获取列表，无法校验时不转发。
			list, modelErr = s.getModels(r.Context(), current)
			if modelErr != nil {
				s.log(r.Context(), slog.LevelWarn, "Auto 模型路径校验失败", "error", s.redactString(modelErr.Error()))
				s.handleError(r.Context(), w, modelErr)
				return
			}
		}
		if modelErr == nil && !list.supportsEndpoint(model, path, pick) {
			if isAuto {
				s.dropAuto(pick)
			}
			s.log(r.Context(), slog.LevelWarn, "所选模型不支持当前 API 路径",
				"model", model, "upstream_path", path)
			writeError(w, http.StatusBadRequest, fmt.Sprintf("模型 %q 不支持 API 路径 %s", model, path))
			return
		}
		s.acceptTerms(r.Context(), current, model)
		req, err := s.apiRequest(r.Context(), current, http.MethodPost, path, outBody)
		if err != nil {
			s.handleError(r.Context(), w, err)
			return
		}
		req.URL.RawQuery = r.URL.RawQuery
		copyRequestHeaders(req.Header, r.Header)
		current.setHeaders(req.Header)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Openai-Intent", "conversation-panel")
		req.Header.Set("X-Initiator", xInitiator)
		if vision {
			req.Header.Set("Copilot-Vision-Request", "true")
		}
		if pick.Token != "" {
			req.Header.Set("Copilot-Session-Token", pick.Token)
		}
		res, err := s.doAPI(r.Context(), req)
		if err != nil {
			s.handleError(r.Context(), w, errors.New("Copilot 生成请求失败: "+err.Error()))
			return
		}
		// 只在任何响应尚未发给客户端时重试明确的 Auto 拒绝。
		if res.StatusCode == http.StatusBadRequest {
			data, readErr := readLimited(res.Body, maxMetadataBody)
			_ = res.Body.Close()
			if readErr != nil {
				// 上游 400 的正文读不出来：无法判断是否为模型拒绝，按上游 400 原样返回；不是网络错误，也不计数（见 cooldown.go）。
				s.log(r.Context(), slog.LevelWarn, "读取上游 400 响应正文失败", "error", s.redactString(readErr.Error()))
				writeError(w, http.StatusBadRequest, "上游返回 400，且读取错误详情失败")
				return
			}
			refusal := modelRefusal(data)
			sessionGone := strings.Contains(strings.ToLower(string(data)), "not available for session") && pick.Token != ""
			if refusal {
				s.refuse(model)
			}
			if isAuto && (refusal || sessionGone) {
				s.log(r.Context(), slog.LevelWarn, "Auto 上游拒绝当前模型", "model", model, "attempt", attempt+1, "retry", attempt < 2)
				s.dropAuto(pick)
				if attempt < 2 {
					continue
				}
			}
			copyResponseHeaders(w.Header(), res.Header)
			w.WriteHeader(res.StatusCode)
			_, _ = w.Write(data)
			s.log(r.Context(), slog.LevelWarn, "生成请求返回客户端错误", "status", res.StatusCode, "model", model)
			return
		}
		s.log(r.Context(), apiStatusLogLevel(res.StatusCode), "生成响应开始返回客户端",
			"status", res.StatusCode, "model", model)
		// 429 与其他错误的计数、冷却由 Pool 按写给客户端的状态码处理（见 cooldown.go）；这里原样流式透传。
		streamResponse(w, res, path == "/responses")
		return
	}
}
