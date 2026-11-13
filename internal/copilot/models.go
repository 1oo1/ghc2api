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
	// Endpoints 记录每个 chat 模型（含隐藏模型）支持的路径；旧缓存没有该字段。
	Endpoints map[string][]string `json:"endpoints"`
	Waiting   map[string]bool     `json:"waiting"`
	// Disabled 记录策略明确不是 enabled 且没有条款的 chat 模型（不论是否内部 id、Experimental、picker 启用）：
	// 它们不进可见列表，只在 Endpoints 里留有路径记录，账号并不能用，modelAvailability 据此不算它们 listed。旧缓存没有该字段，视为空。
	Disabled map[string]bool `json:"disabled"`
	At       time.Time       `json:"at"`
}

type refusedModel struct {
	at      time.Time
	version uint64
}

func (s *Service) getModels(ctx context.Context, current session) (modelList, error) {
	if err := s.modelsMu.lock(ctx); err != nil {
		return modelList{}, err
	}
	defer s.modelsMu.unlock()
	if modelCacheFresh(s.models.At, s.modelsCacheTTL.Load()) {
		s.log(ctx, slog.LevelDebug, "复用缓存的 Copilot 模型列表", "model_count", len(s.models.Data))
		return s.models, nil
	}
	if path := s.cacheFile("models"); path != "" {
		var cached modelList
		if err := readCacheFile(path, &cached); err == nil {
			if modelCacheFresh(cached.At, s.modelsCacheTTL.Load()) {
				s.storeModels(cached)
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
	list := modelList{Data: []json.RawMessage{}, Endpoints: map[string][]string{}, Waiting: map[string]bool{}, Disabled: map[string]bool{}, At: time.Now()}
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
		// 策略明确不是 enabled 且没有条款：账号不能用该模型，无论下面是否展示都记入 Disabled（见 modelList.Disabled）。
		if m.Policy != nil && m.Policy.State != "enabled" && m.Policy.Terms == "" {
			list.Disabled[m.ID] = true
		}
		// 内部 id 一律不展示；picker 未启用的模型在没有 category 或属于 Experimental 时不展示。
		// picker 启用的 Experimental 模型与其他模型一样按下面的策略规则展示。
		if internalModel.MatchString(m.ID) || (!m.Picker && (m.Category == "" || m.Vendor == "Experimental")) {
			continue
		}
		// 策略已启用（或无策略且在选择器中）的模型直接可见；待接受条款的模型可见并记入 Waiting；其余跳过。
		enabled := (m.Policy != nil && m.Policy.State == "enabled") || (m.Policy == nil && m.Picker)
		if !enabled {
			if m.Policy == nil || m.Policy.Terms == "" {
				continue
			}
			list.Waiting[m.ID] = true
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
	s.storeModels(list)
	if err := s.writeCacheUnlessCooling("models", list); err != nil {
		s.log(ctx, slog.LevelWarn, "写入磁盘模型缓存失败", "error", s.redactString(err.Error()))
	}
	s.log(ctx, slog.LevelInfo, "Copilot 模型列表刷新完成", "visible_count", len(list.Data))
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
	s.modelIDs.Store(nil) // 列表回到未知
	return s.removeCacheFile("models")
}

// storeModels 加载了模型列表（刷新或从磁盘缓存读入）：替换内存中的列表，并同步更新 modelAvailability 读取的 id 快照
// （Data 中的模型，加上 Endpoints 中未被策略禁用（Disabled）的隐藏 chat 模型）。调用方须持有 modelsMu。
// forgetModels 清空列表时自行把快照置回 nil；acceptTerms 只改 Waiting 与 At，不影响 id 集合，不需要更新快照。
func (s *Service) storeModels(list modelList) {
	s.models = list
	ids := make(map[string]struct{}, len(list.Data)+len(list.Endpoints))
	for _, raw := range list.Data {
		var m model
		if json.Unmarshal(raw, &m) == nil && m.ID != "" {
			ids[m.ID] = struct{}{}
		}
	}
	for id := range list.Endpoints {
		if !list.Disabled[id] {
			ids[id] = struct{}{}
		}
	}
	s.modelIDs.Store(&ids)
}

// modelAvailability 报告账号最近一次加载的模型列表（不论是否过期）是否含有模型 id，供 Pool 选号时排除没有该模型的账号。
// known 为 false 表示列表未知（从未加载，或已被 forgetModels 清空），此时 listed 无意义，调用方不应排除该账号；
// listed 为 true 当且仅当 id 在可见列表中，或在隐藏 chat 模型中且未被策略禁用（Disabled），且账号当前未拒绝它（isRefused）。
// 只读原子快照，不访问上游，也不等待 modelsMu：选号时持有 Pool.mu，不能被模型列表的刷新阻塞。
func (s *Service) modelAvailability(id string) (listed, known bool) {
	ids := s.modelIDs.Load()
	if ids == nil {
		return false, false
	}
	if _, ok := (*ids)[id]; !ok {
		return false, true
	}
	return !s.isRefused(id), true
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

func (s *Service) refusalVersion(model string) uint64 {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()
	return s.refused[model].version
}

func (s *Service) isRefused(model string) bool {
	s.refusedMu.Lock()
	at := s.refused[model].at
	s.refusedMu.Unlock()
	return !at.IsZero() && time.Since(at) < 6*time.Hour
}

func (s *Service) refuse(model string) {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()
	s.refusalSeq++
	s.refused[model] = refusedModel{at: time.Now(), version: s.refusalSeq}
}

// unrefuse 仅清除请求发出前观察到的拒绝，避免覆盖请求期间产生的新拒绝。
func (s *Service) unrefuse(model string, observed uint64) {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()
	if refusal, ok := s.refused[model]; ok && refusal.version == observed {
		delete(s.refused, model)
	}
}
