package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type autoSession struct {
	Token     string
	Model     string
	ExpiresAt int64
}

type autoWire struct {
	Token     string          `json:"session_token"`
	Selected  json.RawMessage `json:"selected_model"`
	Available []string        `json:"available_models"`
	ExpiresAt int64           `json:"expires_at"`
}

func selectedModel(raw json.RawMessage) string {
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return id
	}
	var selected struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &selected)
	return selected.ID
}

func (s *Service) autoRequest(ctx context.Context, current session, path, version string, body []byte) (autoWire, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := s.apiRequest(ctx, current, http.MethodPost, path, body)
	if err != nil {
		return autoWire{}, err
	}
	req.Header.Set("X-GitHub-Api-Version", version)
	res, err := s.client.Do(req)
	if err != nil {
		return autoWire{}, err
	}
	defer res.Body.Close()
	data, err := readLimited(res.Body, maxMetadataBody)
	if err != nil {
		return autoWire{}, err
	}
	if res.StatusCode/100 != 2 {
		return autoWire{}, &upstreamError{res.StatusCode, data, res.Header}
	}
	var wire autoWire
	if json.Unmarshal(data, &wire) != nil || wire.Token == "" {
		return autoWire{}, errors.New("Copilot Auto 未返回有效的 session")
	}
	return wire, nil
}

func (s *Service) holdAuto(wire autoWire, model string) autoSession {
	expiry := time.Now().Add(time.Hour).Unix()
	if wire.ExpiresAt > 0 && wire.ExpiresAt < expiry {
		expiry = wire.ExpiresAt
	}
	s.autoState.Lock()
	defer s.autoState.Unlock()
	s.auto = autoSession{Token: wire.Token, Model: model, ExpiresAt: expiry}
	return s.auto
}

func (s *Service) resolveAuto(ctx context.Context, current session, prompt string) (autoSession, error) {
	if err := s.autoMu.lock(ctx); err != nil {
		return autoSession{}, err
	}
	defer s.autoMu.unlock()
	s.autoState.Lock()
	cached := s.auto
	s.autoState.Unlock()
	if cached.Model != "" && time.Until(time.Unix(cached.ExpiresAt, 0)) > 2*time.Minute && !s.isRefused(cached.Model) {
		return cached, nil
	}
	body, _ := json.Marshal(map[string]string{"prompt": prompt, "tier": "balance"})
	wire, err := s.autoRequest(ctx, current, "/auto", "2026-08-01", body)
	if err == nil {
		if model := selectedModel(wire.Selected); model != "" {
			if !s.isRefused(model) {
				return s.holdAuto(wire, model), nil
			}
			// 新 Auto 已经明确选出被拒模型，不再用旧会话重复同一个选择。
			return s.manualFallback(ctx, current)
		}
	}
	if ctx.Err() != nil {
		return autoSession{}, ctx.Err()
	}
	wire, err = s.autoRequest(ctx, current, "/models/session", "2025-10-01",
		[]byte(`{"auto_mode":{"model_hints":["auto"]}}`))
	if err == nil {
		list, _ := s.getModels(ctx, current)
		model := selectedModel(wire.Selected)
		if s.isRefused(model) {
			model = ""
		}
		if model == "" {
			for _, id := range wire.Available {
				if list.Known[id] && !s.isRefused(id) {
					model = id
					break
				}
			}
		}
		if model == "" {
			for _, id := range wire.Available {
				if !s.isRefused(id) {
					model = id
					break
				}
			}
		}
		if model != "" {
			return s.holdAuto(wire, model), nil
		}
	} else {
		var upstream *upstreamError
		// 不把鉴权、限流或上游故障误当成未提供 Auto。
		if !errors.As(err, &upstream) || upstream.status != http.StatusNotFound {
			return autoSession{}, err
		}
	}
	return s.manualFallback(ctx, current)
}

// 调用方持有 autoMu；手选 fallback 不发送 Copilot-Session-Token。
func (s *Service) manualFallback(ctx context.Context, current session) (autoSession, error) {
	list, err := s.getModels(ctx, current)
	if err != nil {
		return autoSession{}, err
	}
	for _, id := range list.Picks {
		if !s.isRefused(id) {
			s.autoState.Lock()
			s.auto = autoSession{Model: id, ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}
			pick := s.auto
			s.autoState.Unlock()
			return pick, nil
		}
	}
	return autoSession{}, errors.New("Copilot 没有可用的 Auto 或手选模型，请检查订阅权限")
}

func (s *Service) dropAuto(pick autoSession) {
	s.autoState.Lock()
	defer s.autoState.Unlock()
	if s.auto == pick {
		s.auto = autoSession{}
	}
}

func modelRefusal(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte(`"model_not_supported"`)) ||
		strings.Contains(string(lower), "requested model is not supported")
}
