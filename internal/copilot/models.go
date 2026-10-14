package copilot

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"time"
)

var internalModel = regexp.MustCompile(`^(copilot-search|exec-agent|trajectory)|-(secondary|tertiary|4th|free-auto)$`)

type model struct {
	ID        string   `json:"id"`
	Vendor    string   `json:"vendor"`
	Picker    bool     `json:"model_picker_enabled"`
	Category  string   `json:"model_picker_category"`
	Default   bool     `json:"is_chat_default"`
	Fallback  bool     `json:"is_chat_fallback"`
	Endpoints []string `json:"supported_endpoints"`
	Billing   *struct {
		Premium bool `json:"is_premium"`
	} `json:"billing"`
	Capabilities struct {
		Type string `json:"type"`
	} `json:"capabilities"`
	Policy *struct {
		State string `json:"state"`
		Terms string `json:"terms"`
	} `json:"policy"`
}

type modelList struct {
	Data    []json.RawMessage
	Picks   []string
	Known   map[string]bool
	Waiting map[string]bool
	At      time.Time
}

func (s *Service) getModels(ctx context.Context, current session) (modelList, error) {
	if err := s.modelsMu.lock(ctx); err != nil {
		return modelList{}, err
	}
	defer s.modelsMu.unlock()
	if !s.models.At.IsZero() && time.Since(s.models.At) < 5*time.Minute {
		s.log(ctx, slog.LevelDebug, "复用缓存的 Copilot 模型列表", "model_count", len(s.models.Data))
		return s.models, nil
	}
	s.log(ctx, slog.LevelInfo, "开始刷新 Copilot 模型列表")
	var wire struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := s.fetchJSON(ctx, current, http.MethodGet, "/models", nil, &wire); err != nil {
		s.log(ctx, slog.LevelWarn, "刷新 Copilot 模型列表失败", "error", s.redactString(err.Error()))
		return modelList{}, err
	}
	list := modelList{Data: []json.RawMessage{}, Known: map[string]bool{}, Waiting: map[string]bool{}, At: time.Now()}
	rank := map[string]int{}
	for _, raw := range wire.Data {
		var m model
		if json.Unmarshal(raw, &m) != nil || m.ID == "" || m.Capabilities.Type != "chat" {
			continue
		}
		list.Known[m.ID] = true
		if internalModel.MatchString(m.ID) || m.Vendor == "Experimental" || (!m.Picker && m.Category == "") {
			continue
		}
		if (m.Policy != nil && m.Policy.State == "enabled") || (m.Policy == nil && m.Picker) {
			list.Picks = append(list.Picks, m.ID)
			switch {
			case m.Fallback:
				rank[m.ID] = 0
			case m.Default:
				rank[m.ID] = 1
			case m.ID == "gpt-4.1":
				rank[m.ID] = 2
			case m.Billing != nil && !m.Billing.Premium:
				rank[m.ID] = 3
			default:
				rank[m.ID] = 4
			}
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
	sort.SliceStable(list.Picks, func(i, j int) bool { return rank[list.Picks[i]] < rank[list.Picks[j]] })
	s.models = list
	s.log(ctx, slog.LevelInfo, "Copilot 模型列表刷新完成", "visible_count", len(list.Data), "selectable_count", len(list.Picks))
	return list, nil
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
	data := make([]json.RawMessage, 0, len(list.Data)+1)
	for _, raw := range list.Data {
		var m model
		_ = json.Unmarshal(raw, &m)
		if !s.isRefused(m.ID) {
			data = append(data, raw)
		}
	}
	data = append(data, json.RawMessage(`{"id":"auto","name":"Auto","object":"model","owned_by":"github-copilot"}`))
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
