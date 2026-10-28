package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"time"
)

var internalModel = regexp.MustCompile(`^(copilot-search|exec-agent|trajectory)|-(secondary|tertiary|4th|free-auto)$`)

type model struct {
	ID           string   `json:"id"`
	Vendor       string   `json:"vendor"`
	Picker       bool     `json:"model_picker_enabled"`
	Category     string   `json:"model_picker_category"`
	Endpoints    []string `json:"supported_endpoints"`
	Capabilities struct {
		Type string `json:"type"`
	} `json:"capabilities"`
	Policy *struct {
		State string `json:"state"`
		Terms string `json:"terms"`
	} `json:"policy"`
}

type modelList struct {
	Data []json.RawMessage `json:"data"`
	// Picks 是可选（策略已启用）模型的 id 列表。
	Picks []string `json:"picks"`
	// Endpoints 记录每个 chat 模型（含隐藏模型）支持的路径；旧缓存没有该字段。
	Endpoints map[string][]string `json:"endpoints"`
	Waiting   map[string]bool     `json:"waiting"`
	At        time.Time           `json:"at"`
}

func (s *Service) getModels(ctx context.Context, current session) (modelList, error) {
	if err := s.modelsMu.lock(ctx); err != nil {
		return modelList{}, err
	}
	defer s.modelsMu.unlock()
	if modelCacheFresh(s.models.At, s.modelsCacheTTL) {
		s.log(ctx, slog.LevelDebug, "复用缓存的 Copilot 模型列表", "model_count", len(s.models.Data))
		return s.models, nil
	}
	if path := s.cacheFile("models"); path != "" {
		var cached modelList
		if err := readCacheFile(path, &cached); err == nil {
			if modelCacheFresh(cached.At, s.modelsCacheTTL) {
				s.models = cached
				s.log(ctx, slog.LevelDebug, "复用磁盘中的 Copilot 模型列表", "model_count", len(cached.Data))
				return cached, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			s.log(ctx, slog.LevelWarn, "读取磁盘模型缓存失败", "error", s.redactString(err.Error()))
		}
	}
	s.log(ctx, slog.LevelInfo, "开始刷新 Copilot 模型列表")
	var wire struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := s.fetchJSON(ctx, current, http.MethodGet, "/models", nil, &wire); err != nil {
		s.log(ctx, slog.LevelWarn, "刷新 Copilot 模型列表失败", "error", s.redactString(err.Error()))
		return modelList{}, err
	}
	list := modelList{Data: []json.RawMessage{}, Endpoints: map[string][]string{}, Waiting: map[string]bool{}, At: time.Now()}
	for _, raw := range wire.Data {
		var m model
		if json.Unmarshal(raw, &m) != nil || m.ID == "" || m.Capabilities.Type != "chat" {
			continue
		}
		if len(m.Endpoints) == 0 {
			list.Endpoints[m.ID] = []string{"/chat/completions"}
		} else {
			list.Endpoints[m.ID] = m.Endpoints
		}
		if internalModel.MatchString(m.ID) || m.Vendor == "Experimental" || (!m.Picker && m.Category == "") {
			continue
		}
		if (m.Policy != nil && m.Policy.State == "enabled") || (m.Policy == nil && m.Picker) {
			list.Picks = append(list.Picks, m.ID)
		} else if m.Policy != nil && m.Policy.Terms != "" {
			list.Waiting[m.ID] = true
		} else {
			continue
		}
		var metadata map[string]any
		_ = json.Unmarshal(raw, &metadata)
		metadata["object"] = "model"
		metadata["owned_by"] = "github-copilot"
		if len(m.Endpoints) == 0 {
			metadata["supported_endpoints"] = []string{"/chat/completions"}
		}
		normalized, _ := json.Marshal(metadata)
		list.Data = append(list.Data, normalized)
	}
	s.models = list
	if err := s.writeCacheUnlessCooling("models", list); err != nil {
		s.log(ctx, slog.LevelWarn, "写入磁盘模型缓存失败", "error", s.redactString(err.Error()))
	}
	s.log(ctx, slog.LevelInfo, "Copilot 模型列表刷新完成", "visible_count", len(list.Data), "selectable_count", len(list.Picks))
	return list, nil
}

// forgetModels 清空账号的模型列表缓存（内存与磁盘），用于进入冷却时。等待正在进行的刷新结束后再检查 still()，
// 仅在账号仍处于本次冷却时清空（见 settleCooldown）。
func (s *Service) forgetModels(still func() bool) error {
	_ = s.modelsMu.lock(context.Background()) // Background 不会被取消，lock 只会阻塞直到获取结束
	defer s.modelsMu.unlock()
	if !still() {
		return nil
	}
	s.models = modelList{}
	return s.removeCacheFile("models")
}

func modelCacheFresh(at time.Time, ttl time.Duration) bool {
	age := time.Since(at)
	return !at.IsZero() && age >= 0 && age < ttl
}

// supportsEndpoint 判断模型是否支持 path。
func (list modelList) supportsEndpoint(id, path string) bool {
	return slices.Contains(list.endpoints(id), path)
}

// endpoints 依次取：可见模型的 /models 路径、隐藏 chat 模型的路径，其余返回 nil。
func (list modelList) endpoints(id string) []string {
	for _, raw := range list.Data {
		var entry model
		if json.Unmarshal(raw, &entry) == nil && entry.ID == id {
			return entry.Endpoints
		}
	}
	return list.Endpoints[id]
}

func (s *Service) handleModels(w http.ResponseWriter, r *http.Request) {
	s.log(r.Context(), slog.LevelInfo, "开始查询 Copilot 模型列表")
	current, err := s.getSession(r.Context())
	if err != nil {
		s.handleError(r.Context(), w, err)
		return
	}
	list, err := s.getModels(r.Context(), current)
	if err != nil {
		s.handleError(r.Context(), w, err)
		return
	}
	data := make([]json.RawMessage, 0, len(list.Data))
	for _, raw := range list.Data {
		var m model
		_ = json.Unmarshal(raw, &m)
		if !s.isRefused(m.ID) {
			data = append(data, raw)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	s.log(r.Context(), slog.LevelInfo, "Copilot 模型列表已返回", "visible_count", len(data))
}

func (s *Service) acceptTerms(ctx context.Context, current session, model string) {
	if s.modelsMu.lock(ctx) != nil {
		return
	}
	defer s.modelsMu.unlock()
	if !s.models.Waiting[model] {
		return
	}
	s.log(ctx, slog.LevelInfo, "开始接受模型使用条款", "model", model)
	if s.fetchJSON(ctx, current, http.MethodPost, "/models/"+url.PathEscape(model)+"/policy",
		[]byte(`{"state":"enabled"}`), nil) == nil {
		delete(s.models.Waiting, model)
		s.models.At = time.Time{}
		if err := s.writeCacheUnlessCooling("models", s.models); err != nil {
			s.log(ctx, slog.LevelWarn, "写入磁盘模型缓存失败", "error", s.redactString(err.Error()))
			if path := s.cacheFile("models"); path != "" {
				if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					s.log(ctx, slog.LevelWarn, "清除失效的磁盘模型缓存失败",
						"error", s.redactString(removeErr.Error()))
				}
			}
		}
		s.log(ctx, slog.LevelInfo, "模型使用条款已接受", "model", model)
	} else {
		s.log(ctx, slog.LevelWarn, "接受模型使用条款失败", "model", model)
	}
}

func (s *Service) isRefused(model string) bool {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()
	at, ok := s.refused[model]
	return ok && time.Since(at) < 6*time.Hour
}

func (s *Service) refuse(model string) {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()
	s.refused[model] = time.Now()
}
