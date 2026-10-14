package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

var editorHeaders = map[string]string{
	"Editor-Version": "vscode/1.140.0", "Editor-Plugin-Version": "copilot-chat/0.68.0",
	"Copilot-Integration-Id": "vscode-chat", "User-Agent": "GitHubCopilotChat/0.68.0",
}

type session struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	Endpoints struct {
		API string `json:"api"`
	} `json:"endpoints"`
	API string
}

func (session session) setHeaders(header http.Header) {
	header.Set("Authorization", "Bearer "+session.Token)
	header.Set("X-GitHub-Api-Version", "2026-01-09")
	for key, value := range editorHeaders {
		header.Set(key, value)
	}
}

func (s *Service) githubRequest(ctx context.Context, path string, editor bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubBase(s.config.Host)+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+s.config.Token)
	req.Header.Set("Accept", "application/json")
	if editor {
		for key, value := range editorHeaders {
			req.Header.Set(key, value)
		}
	}
	return s.doAPI(ctx, req)
}

func (s *Service) getSession(ctx context.Context) (session, error) {
	if err := s.sessionMu.lock(ctx); err != nil {
		return session{}, err
	}
	defer s.sessionMu.unlock()
	if s.session.Token != "" && time.Until(time.Unix(s.session.ExpiresAt, 0)) > 2*time.Minute {
		s.log(ctx, slog.LevelDebug, "复用缓存的 Copilot session", "expires_at", s.session.ExpiresAt)
		return s.session, nil
	}
	s.log(ctx, slog.LevelInfo, "开始通过 OAuth 获取 Copilot session")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := s.githubRequest(ctx, "/copilot_internal/v2/token", true)
	if err != nil {
		return session{}, errors.New("Copilot 登录请求失败: " + err.Error())
	}
	defer res.Body.Close()
	data, err := readLimited(res.Body, maxMetadataBody)
	if err != nil {
		return session{}, err
	}
	if res.StatusCode != http.StatusOK {
		return session{}, &upstreamError{res.StatusCode, data, res.Header}
	}
	var fresh session
	if json.Unmarshal(data, &fresh) != nil {
		return session{}, errors.New("Copilot 返回了无效的 session JSON")
	}
	if fresh.Token == "" || fresh.ExpiresAt <= time.Now().Unix() {
		return session{}, errors.New("Copilot session 缺少 token 或已过期")
	}
	fresh.API, err = validateAPI(fresh.Endpoints.API, s.config.Host)
	if err != nil {
		return session{}, err
	}
	s.session = fresh
	s.addSecret(fresh.Token)
	s.log(ctx, slog.LevelInfo, "Copilot session 获取成功", "expires_at", fresh.ExpiresAt, "api_host", fresh.API)
	return fresh, nil
}
