package copilot

import (
	"errors"
	"log/slog"
	"net/http"
)

// ConfigStore 由 cmd 层实现，管理接口通过它读取、校验与保存配置文件。
type ConfigStore interface {
	Load() (ConfigSnapshot, error)
	Validate(content string) ConfigValidation
	// Save 在 revision 与当前文件一致且内容有效时写入文件，返回需要立即应用的运行时设置。
	// 冲突返回包裹 ErrConfigConflict 的错误，内容无效返回包裹 ErrConfigInvalid 的错误。
	Save(content, revision string) (ConfigSaveResult, error)
}

// ConfigSnapshot 是配置文件的当前内容。RestartRequired 是磁盘内容中已修改但需重启才生效的字段，永不为 nil。
type ConfigSnapshot struct {
	Path            string   `json:"path"`
	Content         string   `json:"content"`
	Revision        string   `json:"revision"`
	Writable        bool     `json:"writable"`
	RestartRequired []string `json:"restart_required"`
}

// ConfigValidation 是配置内容的校验结果；无效配置也是正常结果。
type ConfigValidation struct {
	Valid           bool     `json:"valid"`
	Error           string   `json:"error,omitempty"`
	Line            int      `json:"line,omitempty"` // 错误所在行（从 1 开始），未知为 0
	RestartRequired []string `json:"restart_required"`
}

// ConfigSaveResult 是一次保存的结果。Settings 只供 Pool 应用，不输出到响应。
type ConfigSaveResult struct {
	Revision        string          `json:"revision"`
	Changed         []string        `json:"changed"`
	RestartRequired []string        `json:"restart_required"`
	Settings        RuntimeSettings `json:"-"`
}

var (
	ErrConfigConflict = errors.New("配置文件已被修改，请重新加载后再保存")
	ErrConfigInvalid  = errors.New("配置无效")
)

// configStore 返回配置存储；未配置时已写入 404 并返回 false。
func (p *Pool) configStore(w http.ResponseWriter) (ConfigStore, bool) {
	if p.config.ConfigStore == nil {
		writeError(w, http.StatusNotFound, "未配置配置文件")
		return nil, false
	}
	return p.config.ConfigStore, true
}

// handleGetConfig 返回配置文件内容、revision、是否可写与待重启字段。
func (p *Pool) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	store, ok := p.configStore(w)
	if !ok {
		return
	}
	snapshot, err := store.Load()
	if err != nil {
		p.log(r.Context(), slog.LevelError, "管理接口：读取配置文件失败", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "读取配置文件失败")
		return
	}
	// 响应含明文密钥，禁止浏览器或中间代理缓存。
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, snapshot)
}

// handleValidateConfig 校验请求体中的配置内容；无效配置也返回 200，valid 为 false。
func (p *Pool) handleValidateConfig(w http.ResponseWriter, r *http.Request) {
	store, ok := p.configStore(w)
	if !ok {
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if !decodeManagementJSON(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, store.Validate(req.Content))
}

// handlePutConfig 保存配置并立即应用其中可热更新的字段。
// 冲突（文件已被修改）返回 409，内容无效或 revision 缺失返回 400。日志只记录变化的字段名，不含配置值。
func (p *Pool) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	store, ok := p.configStore(w)
	if !ok {
		return
	}
	var req struct {
		Content  string `json:"content"`
		Revision string `json:"revision"`
	}
	if !decodeManagementJSON(w, r, &req) {
		return
	}
	if req.Revision == "" {
		writeError(w, http.StatusBadRequest, "请求必须包含 revision")
		return
	}
	// configMu 保证 Save 与 ApplySettings 作为一个整体、按同一顺序生效（并发保存不会交错应用）；
	// 与 cmd 层 configFile.mu 分工不同：后者只保护文件读写与 running 值，不涉及 Pool 的运行时设置。
	p.configMu.Lock()
	result, err := store.Save(req.Content, req.Revision)
	if err == nil {
		p.ApplySettings(result.Settings)
	}
	p.configMu.Unlock()
	switch {
	case errors.Is(err, ErrConfigConflict):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, ErrConfigInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		p.log(r.Context(), slog.LevelError, "管理接口：保存配置失败", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
		return
	}
	// JSON 中的数组字段永不为 null。
	if result.Changed == nil {
		result.Changed = []string{}
	}
	if result.RestartRequired == nil {
		result.RestartRequired = []string{}
	}
	p.log(r.Context(), slog.LevelInfo, "配置已更新", "changed", result.Changed, "restart_required", result.RestartRequired)
	writeJSON(w, http.StatusOK, result)
}
