package copilot

import (
	"log/slog"
	"net/http"
	"strconv"
)

const (
	// defaultLogReadBytes 是 GET /v0/management/logs 缺省的读取大小（8 MiB）。
	defaultLogReadBytes int64 = 8 << 20
	// maxLogReadBytes 是 max_bytes 的上限（64 MiB）。
	maxLogReadBytes int64 = 64 << 20
)

// handleGetLogs 返回日志文件（含滚动备份）尾部的原始 NDJSON 文本，解析与聚合由控制台在浏览器中完成。
// 查询参数 max_bytes 限制读取的字节数；响应头 X-Log-Truncated 表示更早的内容是否被截掉。
func (p *Pool) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	if p.config.LogFile == "" {
		writeError(w, http.StatusNotFound, "未配置日志文件")
		return
	}
	maxBytes := defaultLogReadBytes
	if query := r.URL.Query(); query.Has("max_bytes") {
		n, err := strconv.ParseInt(query.Get("max_bytes"), 10, 64)
		if err != nil || n < 1 || n > maxLogReadBytes {
			writeError(w, http.StatusBadRequest, "max_bytes 必须是 1 到 "+strconv.FormatInt(maxLogReadBytes, 10)+" 之间的整数")
			return
		}
		maxBytes = n
	}
	data, truncated, err := readLogTail(p.config.LogFile, maxBytes)
	if err != nil {
		p.log(r.Context(), slog.LevelError, "管理接口：读取日志文件失败", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "读取日志文件失败")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Log-Truncated", strconv.FormatBool(truncated))
	_, _ = w.Write(data)
}
