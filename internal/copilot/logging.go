package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// maxDebugBodySize 是回归测试使用的历史边界；debug 正文现在不设截断上限。
const maxDebugBodySize = 16 << 20
const debugLogChunkSize = 32 << 10
const debugRedactionOverlap = 64 << 10
const maxSensitiveFieldHold = 1 << 20

type requestLoggerContextKey struct{}

func (s *Service) addSecret(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	s.secretsMu.Lock()
	s.secrets[value] = struct{}{}
	s.secretsMu.Unlock()
}

// logFunc 输出请求级日志；实现必须对敏感值脱敏。
type logFunc func(ctx context.Context, level slog.Level, message string, args ...any)

// withRequestLog 为每个客户端请求分配 request_id（取自 seq 递增），把绑定该 ID 的日志器写入上下文，
// 记录收到与完成日志后调用 next。Service 与 Pool 共用此包装，只在日志器、序号和输出函数上不同。
func withRequestLog(logger *slog.Logger, seq *atomic.Uint64, logf logFunc, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := bindRequestLogger(r.Context(), logger.With("request_id", seq.Add(1)))
		r = r.WithContext(ctx)
		writer := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		logf(ctx, slog.LevelInfo, "收到客户端请求", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
		next.ServeHTTP(writer, r)
		logf(ctx, slog.LevelInfo, "客户端请求处理完成", "method", r.Method, "path", r.URL.Path,
			"status", writer.status, "duration_ms", time.Since(started).Milliseconds())
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.status = status
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// bindRequestLogger 把已带 request_id（及可选 account）的日志器写入请求上下文，
// 之后 Service.log 会使用该日志器输出，Pool 选定账号后的日志也经由此处生效。
func bindRequestLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, requestLoggerContextKey{}, logger)
}

// contextLogger 返回请求上下文中绑定的日志器；没有绑定时使用 fallback。
func contextLogger(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if logger, ok := ctx.Value(requestLoggerContextKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return fallback
}

func (s *Service) requestLogger(ctx context.Context) *slog.Logger {
	return contextLogger(ctx, s.logger)
}

func (s *Service) log(ctx context.Context, level slog.Level, message string, args ...any) {
	if logger := s.requestLogger(ctx); logger != nil {
		safeArgs := append([]any(nil), args...)
		for i, arg := range safeArgs {
			if attribute, ok := arg.(slog.Attr); ok {
				attribute.Value = s.redactSlogValue(attribute.Value)
				safeArgs[i] = attribute
			} else if i%2 == 1 {
				safeArgs[i] = s.redactLogValue(arg)
			}
		}
		logger.Log(ctx, level, message, safeArgs...)
	}
}

func (s *Service) redactLogValue(value any) any {
	switch typed := value.(type) {
	case string:
		return s.redactString(typed)
	case error:
		return s.redactString(typed.Error())
	case []string:
		safe := make([]string, len(typed))
		for i, item := range typed {
			safe[i] = s.redactString(item)
		}
		return safe
	case []any:
		safe := make([]any, len(typed))
		for i, item := range typed {
			safe[i] = s.redactLogValue(item)
		}
		return safe
	case map[string]string:
		safe := make(map[string]string, len(typed))
		for key, item := range typed {
			safe[key] = s.redactString(item)
		}
		return safe
	case map[string]any:
		safe := make(map[string]any, len(typed))
		for key, item := range typed {
			safe[key] = s.redactLogValue(item)
		}
		return safe
	default:
		return value
	}
}

func (s *Service) redactSlogValue(value slog.Value) slog.Value {
	switch value.Kind() {
	case slog.KindString:
		return slog.StringValue(s.redactString(value.String()))
	case slog.KindGroup:
		attrs := value.Group()
		for i := range attrs {
			attrs[i].Value = s.redactSlogValue(attrs[i].Value)
		}
		return slog.GroupValue(attrs...)
	default:
		return value
	}
}

func (s *Service) doAPI(ctx context.Context, req *http.Request) (*http.Response, error) {
	logger := s.requestLogger(ctx)
	rawURL := req.URL.String()
	safeURL := s.redactURL(req.URL)
	s.log(ctx, slog.LevelInfo, "上游 API 请求开始", "method", req.Method, "url", safeURL)
	if logger != nil && logger.Enabled(ctx, slog.LevelDebug) {
		body, bodyErr := requestBodyForLog(req)
		s.logAPIRequestDetail(ctx, logger, req, safeURL, body, bodyErr)
	}

	started := time.Now()
	res, err := s.client.Do(req)
	if err != nil {
		safeErr := s.redactAPIError(err, rawURL, safeURL)
		s.log(ctx, slog.LevelError, "上游 API 请求失败", "method", req.Method,
			"url", safeURL, "duration_ms", time.Since(started).Milliseconds(),
			"error", safeErr)
		return nil, safeErr
	}
	level := apiStatusLogLevel(res.StatusCode)
	if res.StatusCode == http.StatusNotFound && isOptionalAutoEndpoint(req.URL.Path) {
		level = slog.LevelInfo
	}
	s.log(ctx, level, "上游 API 响应已收到", "method", req.Method, "url", safeURL,
		"status", res.StatusCode, "duration_ms", time.Since(started).Milliseconds())
	if logger != nil && logger.Enabled(ctx, slog.LevelDebug) {
		logger.DebugContext(ctx, "上游 API 响应详情", "response", map[string]any{
			"method": req.Method, "url": safeURL, "status": res.StatusCode,
			"headers": s.redactHeaders(res.Header),
		})
		if res.Body != nil {
			bufferLimit := debugBodyBufferLimit(req.URL.Path)
			res.Body = &debugResponseBody{
				ReadCloser: res.Body, service: s, logger: logger, ctx: ctx,
				method: req.Method, url: safeURL, status: res.StatusCode,
				bufferLimit:  bufferLimit,
				drainOnClose: bufferLimit > 0 || res.StatusCode == http.StatusBadRequest,
				streaming:    bufferLimit == 0,
			}
		}
	}
	return res, nil
}

type redactedAPIError struct {
	message string
	cause   error
}

func (e redactedAPIError) Error() string {
	return e.message
}

func (e redactedAPIError) Is(target error) bool {
	return errors.Is(e.cause, target)
}

func (s *Service) redactAPIError(err error, rawURL, safeURL string) error {
	message := strings.ReplaceAll(err.Error(), rawURL, safeURL)
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.URL != "" {
		redactedURL := safeURL
		if parsed, parseErr := url.Parse(urlErr.URL); parseErr == nil {
			redactedURL = s.redactURL(parsed)
		}
		message = strings.ReplaceAll(message, urlErr.URL, redactedURL)
	}
	return redactedAPIError{message: s.redactString(message), cause: err}
}

func requestBodyForLog(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("无法复制请求体供 debug 日志记录")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

func (s *Service) logAPIRequestDetail(
	ctx context.Context, logger *slog.Logger, req *http.Request, safeURL string, body []byte, bodyErr error,
) {
	safeBody := s.redactBody(body)
	detail := map[string]any{
		"method": req.Method, "url": safeURL, "headers": s.redactHeaders(req.Header),
	}
	if bodyErr != nil {
		detail["body_error"] = s.redactString(bodyErr.Error())
	}
	if len(safeBody) <= debugLogChunkSize {
		detail["body"] = safeBody
		logger.DebugContext(ctx, "上游 API 请求详情", "request", detail)
		return
	}

	detail["body_bytes"] = len(safeBody)
	logger.DebugContext(ctx, "上游 API 请求详情", "request", detail)
	for start, index := 0, 1; start < len(safeBody); index++ {
		end := safeLogStringCut(safeBody, start, debugLogChunkSize)
		chunk := safeBody[start:end]
		logger.DebugContext(ctx, "上游 API 请求正文块",
			"method", req.Method, "url", safeURL, "chunk_index", index,
			"body", chunk, "body_bytes", len(chunk))
		start = end
	}
}

func safeLogStringCut(value string, start, limit int) int {
	end := min(start+limit, len(value))
	if end == len(value) {
		return end
	}
	for end > start && !utf8.RuneStart(value[end]) {
		end--
	}
	if end == start {
		return min(start+limit, len(value))
	}
	return end
}

type debugResponseBody struct {
	io.ReadCloser
	service *Service
	logger  *slog.Logger
	ctx     context.Context
	method  string
	url     string
	status  int

	mu            sync.Mutex
	once          sync.Once
	bufferLimit   int
	drainOnClose  bool
	streaming     bool
	buffered      []byte
	pending       []byte
	totalBytes    int64
	chunkIndex    uint64
	complete      bool
	skippingValue bool
	valueSkip     sensitiveJSONValueSkip
	skippingText  bool
	skipSecret    int
}

type sensitiveJSONValueSkip struct {
	mode     byte
	escaped  bool
	inString bool
	stack    []byte
}

func (s *sensitiveJSONValueSkip) begin(data []byte) (int, bool) {
	if len(data) == 0 {
		s.mode = 'p'
		return 0, false
	}
	switch data[0] {
	case '"':
		s.mode = 's'
		count, complete := s.consume(data[1:])
		return count + 1, complete
	case '{':
		s.mode = 'c'
		s.stack = append(s.stack, '}')
		count, complete := s.consume(data[1:])
		return count + 1, complete
	case '[':
		s.mode = 'c'
		s.stack = append(s.stack, ']')
		count, complete := s.consume(data[1:])
		return count + 1, complete
	default:
		s.mode = 'p'
		return s.consume(data)
	}
}

func (s *sensitiveJSONValueSkip) consume(data []byte) (int, bool) {
	switch s.mode {
	case 's':
		for i, ch := range data {
			if s.escaped {
				s.escaped = false
			} else if ch == '\\' {
				s.escaped = true
			} else if ch == '"' {
				s.mode = 0
				return i + 1, true
			}
		}
	case 'c':
		for i, ch := range data {
			if s.inString {
				if s.escaped {
					s.escaped = false
				} else if ch == '\\' {
					s.escaped = true
				} else if ch == '"' {
					s.inString = false
				}
				continue
			}
			switch ch {
			case '"':
				s.inString = true
			case '{':
				s.stack = append(s.stack, '}')
			case '[':
				s.stack = append(s.stack, ']')
			case '}', ']':
				if len(s.stack) == 0 || s.stack[len(s.stack)-1] != ch {
					s.mode = 0
					return i, true
				}
				s.stack = s.stack[:len(s.stack)-1]
				if len(s.stack) == 0 {
					s.mode = 0
					return i + 1, true
				}
			}
		}
	case 'p':
		for i, ch := range data {
			switch ch {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				s.mode = 0
				return i, true
			}
		}
	}
	return len(data), false
}

func (b *debugResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if n > 0 {
		b.totalBytes += int64(n)
		data := p[:n]
		if b.skipSecret > 0 {
			count := min(len(data), b.skipSecret)
			b.skipSecret -= count
			data = data[count:]
		}
		if b.skippingValue && len(data) > 0 {
			count, complete := b.valueSkip.consume(data)
			data = data[count:]
			if complete {
				b.skippingValue = false
				b.valueSkip = sensitiveJSONValueSkip{}
			} else {
				data = nil
			}
		}
		if b.skippingText && len(data) > 0 {
			length := len(data)
			count := 0
			for count < length && isSensitiveTextByte(data[count]) {
				count++
			}
			data = data[count:]
			if count < length {
				b.skippingText = false
			}
		}
		if len(data) > 0 {
			if !b.streaming && len(b.buffered)+len(data) <= b.bufferLimit {
				b.buffered = append(b.buffered, data...)
			} else {
				if !b.streaming {
					b.pending = append(b.pending, b.buffered...)
					b.buffered = nil
					b.streaming = true
				}
				b.pending = append(b.pending, data...)
			}
		}
	}
	if err == io.EOF {
		b.complete = true
	}
	chunks := b.takeChunksLocked(false)
	b.mu.Unlock()
	b.logChunks(chunks)
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *debugResponseBody) Close() error {
	b.mu.Lock()
	complete := b.complete
	b.mu.Unlock()
	if b.drainOnClose && !complete && b.ctx.Err() == nil {
		_, _ = io.Copy(io.Discard, b)
	}
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func (b *debugResponseBody) finish() {
	b.once.Do(func() {
		b.mu.Lock()
		buffered := append([]byte(nil), b.buffered...)
		b.buffered = nil
		chunks := b.takeChunksLocked(true)
		complete := b.complete
		totalBytes := b.totalBytes
		b.mu.Unlock()
		if !b.streaming {
			b.logBody(buffered, complete)
		} else {
			b.logChunks(chunks)
		}
		b.logger.DebugContext(b.ctx, "上游 API 响应正文读取完成", "method", b.method, "url", b.url,
			"status", b.status, "body_total_bytes", totalBytes, "body_complete", complete)
	})
}

func (b *debugResponseBody) takeChunksLocked(final bool) [][]byte {
	if !b.streaming {
		return nil
	}
	overlap := b.service.debugRedactionWindow()
	available := len(b.pending)
	if !final {
		available -= overlap
	}
	if available <= 0 {
		return nil
	}
	var chunks [][]byte
	for available > 0 {
		chunkSize := min(available, debugLogChunkSize)
		chunkSize = b.service.safeDebugLogCut(b.pending, chunkSize)
		if chunkSize <= 0 {
			if !final {
				if len(b.pending) > maxSensitiveFieldHold {
					if extra, ok := b.redactOversizedSensitiveJSONFieldLocked(debugLogChunkSize); ok {
						chunks = append(chunks, extra...)
						available = len(b.pending) - overlap
						continue
					}
					if extra, ok := b.redactOversizedSensitiveTextLocked(debugLogChunkSize); ok {
						chunks = append(chunks, extra...)
						available = len(b.pending) - overlap
						continue
					}
					if extra, ok := b.redactOversizedKnownSecretLocked(debugLogChunkSize); ok {
						chunks = append(chunks, extra...)
						available = len(b.pending) - overlap
						continue
					}
				}
				// 等待敏感值完整进入缓冲区后再脱敏，不能把跨块的前半段先写入日志。
				break
			} else {
				chunkSize = available
			}
		}
		chunks = append(chunks, append([]byte(nil), b.pending[:chunkSize]...))
		b.pending = append(b.pending[:0], b.pending[chunkSize:]...)
		available = len(b.pending)
		if !final {
			available -= overlap
		}
	}
	return chunks
}

func (b *debugResponseBody) redactOversizedSensitiveJSONFieldLocked(limit int) ([][]byte, bool) {
	for _, match := range sensitiveJSONKeyPrefix.FindAllSubmatchIndex(b.pending, -1) {
		if match[0] >= limit {
			break
		}
		quoted := make([]byte, 0, match[3]-match[2]+2)
		quoted = append(quoted, '"')
		quoted = append(quoted, b.pending[match[2]:match[3]]...)
		quoted = append(quoted, '"')
		var name string
		if json.Unmarshal(quoted, &name) != nil || !isSensitiveName(name) {
			continue
		}
		valueEnd, complete := jsonValueEnd(b.pending, match[1])
		if complete && valueEnd <= limit {
			continue
		}

		chunk := append([]byte(nil), b.pending[:match[1]]...)
		chunk = append(chunk, []byte(`"[REDACTED]"`)...)
		skipper := sensitiveJSONValueSkip{}
		count, finished := skipper.begin(b.pending[match[1]:])
		if finished {
			b.pending = append(b.pending[:0], b.pending[match[1]+count:]...)
		} else {
			b.pending = nil
			b.valueSkip = skipper
			b.skippingValue = true
		}
		return [][]byte{chunk}, true
	}
	return nil, false
}

func (b *debugResponseBody) redactOversizedSensitiveTextLocked(limit int) ([][]byte, bool) {
	for _, match := range sensitiveText.FindAllSubmatchIndex(b.pending, -1) {
		if match[0] >= limit {
			break
		}
		if match[1] <= limit {
			continue
		}
		chunk := append([]byte(nil), b.pending[:match[3]]...)
		chunk = append(chunk, []byte(" [REDACTED]")...)
		if match[1] == len(b.pending) {
			b.pending = nil
			b.skippingText = true
		} else {
			b.pending = append(b.pending[:0], b.pending[match[1]:]...)
		}
		return [][]byte{chunk}, true
	}
	return nil, false
}

func (b *debugResponseBody) redactOversizedKnownSecretLocked(limit int) ([][]byte, bool) {
	b.service.secretsMu.RLock()
	secrets := make([][]byte, 0, len(b.service.secrets))
	for secret := range b.service.secrets {
		secrets = append(secrets, []byte(secret))
	}
	b.service.secretsMu.RUnlock()
	for _, secret := range secrets {
		start := bytes.Index(b.pending[:min(len(b.pending), limit+len(secret))], secret)
		if start < 0 || start >= limit {
			continue
		}
		chunk := append([]byte(nil), b.pending[:start]...)
		chunk = append(chunk, []byte("[REDACTED]")...)
		available := len(b.pending) - start
		consumed := min(available, len(secret))
		b.pending = append(b.pending[:0], b.pending[start+consumed:]...)
		if consumed < len(secret) {
			b.skipSecret = len(secret) - consumed
			b.pending = nil
		}
		return [][]byte{chunk}, true
	}
	return nil, false
}

func (b *debugResponseBody) logBody(data []byte, complete bool) {
	body := b.service.redactBody(data)
	if len(body) > debugLogChunkSize {
		for start, index := 0, 1; start < len(body); index++ {
			end := safeLogStringCut(body, start, debugLogChunkSize)
			chunk := body[start:end]
			b.logger.DebugContext(b.ctx, "上游 API 响应正文块",
				"method", b.method, "url", b.url, "status", b.status,
				"chunk_index", index, "body", chunk, "body_bytes", len(chunk),
				"body_complete", complete)
			start = end
		}
		return
	}
	b.logger.DebugContext(b.ctx, "上游 API 响应正文", "method", b.method, "url", b.url,
		"status", b.status, "body", body, "body_bytes", len(body), "body_complete", complete)
}

func (b *debugResponseBody) logChunks(chunks [][]byte) {
	for _, chunk := range chunks {
		body := b.service.redactBodyChunk(chunk)
		if body == "" {
			continue
		}
		b.mu.Lock()
		b.chunkIndex++
		index := b.chunkIndex
		b.mu.Unlock()
		b.logger.DebugContext(b.ctx, "上游 API 响应正文块", "method", b.method, "url", b.url,
			"status", b.status, "chunk_index", index, "body", body, "body_bytes", len(body))
	}
}

func debugBodyBufferLimit(path string) int {
	switch {
	case path == "/copilot_internal/v2/token", path == "/copilot_internal/user",
		path == "/models", path == "/auto", path == "/models/session",
		strings.HasSuffix(path, "/policy"):
		return maxMetadataBody
	default:
		return 0
	}
}

func apiStatusLogLevel(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	case status >= http.StatusMultipleChoices:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func isOptionalAutoEndpoint(path string) bool {
	return path == "/auto" || path == "/models/session"
}

func (s *Service) redactHeaders(headers http.Header) map[string][]string {
	redacted := make(map[string][]string, len(headers))
	for name, values := range headers {
		if isSensitiveName(name) {
			redacted[name] = []string{"[REDACTED]"}
			continue
		}
		safeValues := make([]string, len(values))
		for i, value := range values {
			safeValues[i] = s.redactString(value)
		}
		redacted[name] = safeValues
	}
	return redacted
}

func (s *Service) redactURL(value *url.URL) string {
	if value == nil {
		return ""
	}
	copy := *value
	query := copy.Query()
	for name := range query {
		if isSensitiveName(name) {
			query.Set(name, "[REDACTED]")
		}
	}
	copy.RawQuery = query.Encode()
	return s.redactString(copy.String())
}

func (s *Service) redactBody(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		var trailing any
		if decoder.Decode(&trailing) == io.EOF {
			safe, err := json.Marshal(s.redactJSONValue(value))
			if err == nil {
				return s.redactString(string(safe))
			}
		}
	}
	return s.redactBodyChunk(data)
}

func (s *Service) redactBodyChunk(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	matches := sensitiveJSONKeyPrefix.FindAllSubmatchIndex(data, -1)
	var safe bytes.Buffer
	last := 0
	for _, match := range matches {
		start, prefixEnd := match[0], match[1]
		if start < last {
			continue
		}
		quoted := make([]byte, 0, match[3]-match[2]+2)
		quoted = append(quoted, '"')
		quoted = append(quoted, data[match[2]:match[3]]...)
		quoted = append(quoted, '"')
		var name string
		if json.Unmarshal(quoted, &name) != nil || !isSensitiveName(name) {
			continue
		}
		valueEnd, complete := jsonValueEnd(data, prefixEnd)
		if !complete {
			valueEnd = len(data)
		}
		safe.Write(data[last:prefixEnd])
		safe.WriteString(`"[REDACTED]"`)
		last = valueEnd
	}
	safe.Write(data[last:])
	return s.redactString(safe.String())
}

func (s *Service) safeDebugLogCut(data []byte, limit int) int {
	if limit <= 0 {
		return 0
	}
	scanEnd := min(len(data), limit+s.debugRedactionWindow())
	scanData := data[:scanEnd]
	cut := limit
	for _, match := range sensitiveJSONKeyPrefix.FindAllSubmatchIndex(scanData, -1) {
		if match[0] >= cut {
			break
		}
		quoted := make([]byte, 0, match[3]-match[2]+2)
		quoted = append(quoted, '"')
		quoted = append(quoted, scanData[match[2]:match[3]]...)
		quoted = append(quoted, '"')
		var name string
		if json.Unmarshal(quoted, &name) != nil || !isSensitiveName(name) {
			continue
		}
		valueEnd, complete := jsonValueEnd(scanData, match[1])
		if !complete || valueEnd > cut {
			cut = match[0]
		}
	}
	for _, match := range sensitiveText.FindAllIndex(scanData, -1) {
		if match[0] < cut && match[1] > cut {
			cut = match[0]
		}
	}
	s.secretsMu.RLock()
	for secret := range s.secrets {
		secretBytes := []byte(secret)
		for offset := 0; offset < cut; {
			found := bytes.Index(scanData[offset:], secretBytes)
			if found < 0 {
				break
			}
			start := offset + found
			if start < cut && start+len(secretBytes) > cut {
				cut = start
			}
			offset = start + 1
		}
	}
	s.secretsMu.RUnlock()
	return cut
}

func jsonValueEnd(data []byte, start int) (int, bool) {
	for start < len(data) && (data[start] == ' ' || data[start] == '\t' || data[start] == '\r' || data[start] == '\n') {
		start++
	}
	if start >= len(data) {
		return start, false
	}
	if data[start] == '"' {
		escaped := false
		for i := start + 1; i < len(data); i++ {
			if escaped {
				escaped = false
				continue
			}
			if data[i] == '\\' {
				escaped = true
			} else if data[i] == '"' {
				return i + 1, true
			}
		}
		return len(data), false
	}
	if data[start] == '{' || data[start] == '[' {
		stack := []byte{}
		if data[start] == '{' {
			stack = append(stack, '}')
		} else {
			stack = append(stack, ']')
		}
		inString, escaped := false, false
		for i := start + 1; i < len(data); i++ {
			ch := data[i]
			if inString {
				if escaped {
					escaped = false
				} else if ch == '\\' {
					escaped = true
				} else if ch == '"' {
					inString = false
				}
				continue
			}
			switch ch {
			case '"':
				inString = true
			case '{':
				stack = append(stack, '}')
			case '[':
				stack = append(stack, ']')
			case '}', ']':
				if len(stack) == 0 || stack[len(stack)-1] != ch {
					return i, false
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return i + 1, true
				}
			}
		}
		return len(data), false
	}
	for i := start; i < len(data); i++ {
		switch data[i] {
		case ',', ']', '}', ' ', '\t', '\r', '\n':
			return i, true
		}
	}
	return len(data), true
}

func (s *Service) redactJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		safe := make(map[string]any, len(typed))
		for name, child := range typed {
			if isSensitiveName(name) {
				safe[name] = "[REDACTED]"
			} else {
				safe[name] = s.redactJSONValue(child)
			}
		}
		return safe
	case []any:
		safe := make([]any, len(typed))
		for i, child := range typed {
			safe[i] = s.redactJSONValue(child)
		}
		return safe
	case string:
		return s.redactString(typed)
	default:
		return value
	}
}

func (s *Service) debugRedactionWindow() int {
	window := debugRedactionOverlap
	s.secretsMu.RLock()
	for secret := range s.secrets {
		if len(secret) > window {
			window = len(secret)
		}
	}
	s.secretsMu.RUnlock()
	return window
}

func (s *Service) redactString(value string) string {
	s.secretsMu.RLock()
	secrets := make([]string, 0, len(s.secrets))
	for secret := range s.secrets {
		secrets = append(secrets, secret)
	}
	s.secretsMu.RUnlock()
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return sensitiveText.ReplaceAllString(value, "$1[REDACTED]")
}

func isSensitiveName(name string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(name))
	switch normalized {
	case "maxtokens", "prompttokens", "completiontokens", "totaltokens", "tokens", "tokenusage":
		return false
	}
	return strings.Contains(normalized, "token") ||
		strings.Contains(normalized, "apikey") ||
		strings.Contains(normalized, "authorization") ||
		strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "cookie")
}

func isSensitiveTextByte(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		strings.ContainsRune("._~+/-=", rune(value))
}

var sensitiveText = regexp.MustCompile(`(?i)\b(Bearer|token)\s+[A-Za-z0-9._~+/-]+=*`)
var sensitiveJSONKeyPrefix = regexp.MustCompile(`"((\\.|[^"\\])*)"\s*:\s*`)
