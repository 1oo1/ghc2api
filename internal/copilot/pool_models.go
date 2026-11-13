package copilot

// 本文件实现 Pool 的模型列表（GET /models、/v1/models）：按生成请求同样的规则逐个选账号尝试，首个成功即返回该账号的列表。
// 前提是各账号的模型相同；不同时只返回首个成功账号的列表，不取并集（并集要等所有账号，没有收益）。
// 生成请求按模型选号见 Pool.chooseFor。

import (
	"bytes"
	"log/slog"
	"maps"
	"net/http"
)

// serveModels 按 choose("", tried) 逐个选账号（无会话键、无模型过滤：分档加 SWRR，排除禁用、冷却、耗尽与本请求已试过的账号，
// 与生成请求共用轮询状态）尝试 queryModels：账号返回 2xx 时把它的响应（状态码、头、正文）原样写给客户端并结束，不再访问其他账号；
// 否则记入 tried，继续选下一个。全部失败时原样写出最后一次失败的响应；一个账号都选不出时返回 503。
// 客户端已取消时停止尝试，不写响应。没有会话键，因此没有风控记录与亲和绑定。
func (p *Pool) serveModels(w http.ResponseWriter, r *http.Request) {
	tried := map[*poolAccount]bool{}
	var last *capturedResponse
	for {
		account := p.choose("", tried)
		if account == nil {
			break
		}
		tried[account] = true
		res := p.queryModels(r, account)
		if res.success() {
			p.log(r.Context(), slog.LevelInfo, "模型列表已返回", "attempts", len(tried), "succeeded", true)
			res.writeTo(w)
			return
		}
		last = res
		if r.Context().Err() != nil {
			return
		}
	}
	if last == nil {
		writeNoAccount(w)
		return
	}
	p.log(r.Context(), slog.LevelWarn, "模型列表查询全部失败", "attempts", len(tried), "succeeded", false)
	last.writeTo(w)
}

// queryModels 让账号处理一次模型列表请求，响应写入返回值而不是客户端。统计、连续错误、冷却与 session 失败计数规则与
// 经 Pool 把 /models 请求发给单个账号时一致（见 pickSession、serve）：先取 session，失败时按 session 错误计入
// （客户端已取消时不计），并以 handleError 的响应作为该账号的结果；成功后复用账号的 handleModels（含缓存与被拒模型隐藏）。
// 每次尝试计该账号一次请求。
func (p *Pool) queryModels(r *http.Request, account *poolAccount) *capturedResponse {
	ctx := p.accountContext(r.Context(), account)
	res := &capturedResponse{header: http.Header{}}
	if _, err := account.service.getSession(ctx); err != nil {
		if ctx.Err() == nil {
			p.noteSessionFailure(ctx, "", account, err)
		}
		// session 失败已在上面计入，这里只更新统计（countErrors 为 false）。
		account.record(ctx, res, func(w http.ResponseWriter) { account.service.handleError(ctx, w, err) }, false)
		return res
	}
	account.record(ctx, res, func(w http.ResponseWriter) { account.routes.mux.ServeHTTP(w, r.WithContext(ctx)) }, true)
	return res
}

// capturedResponse 在内存中接收账号处理请求的响应，由 serveModels 决定是否原样写给客户端。
type capturedResponse struct {
	header http.Header
	status int // 0 表示尚未写入
	body   bytes.Buffer
}

func (c *capturedResponse) Header() http.Header { return c.header }

func (c *capturedResponse) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *capturedResponse) Write(data []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	return c.body.Write(data)
}

// code 返回响应的状态码；没有写入任何内容时按 200。
func (c *capturedResponse) code() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}

// success 报告响应是否成功（2xx）。
func (c *capturedResponse) success() bool {
	return c.code()/100 == 2
}

// writeTo 把捕获的响应原样写给客户端（状态码、响应头与正文）。
func (c *capturedResponse) writeTo(w http.ResponseWriter) {
	maps.Copy(w.Header(), c.header)
	w.WriteHeader(c.code())
	_, _ = w.Write(c.body.Bytes())
}
