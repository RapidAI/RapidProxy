package oauthprov

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Model 是模型目录的一条记录。
type Model struct {
	ID   string
	Name string
}

// ListModels 拉取 OAuth 上游的模型目录。
//
// 三家形态不同：
//
//	claude  ：GET {base}/v1/models，x-api-key + anthropic-version
//	xai     ：GET {base}/v1/models，Bearer + X-XAI-Token-Auth
//	chatgpt ：GET {base}/codex/models，Bearer + Codex 头（含 chatgpt-account-id）
func ListModels(ctx context.Context, hc *http.Client, provider Provider, base, accessToken, accountID, userAgent string) ([]Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("需要已登录的访问令牌")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	endpoint := ""
	switch provider {
	case ProviderClaude:
		endpoint = appendVersionedPath(base, "/models")
	case ProviderXAI:
		endpoint = appendVersionedPath(base, "/models")
	case ProviderChatGPT:
		endpoint = codexModelsEndpoint(base)
	default:
		return nil, fmt.Errorf("未知的 OAuth 服务商: %s", provider)
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if ua := strings.TrimSpace(userAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	switch provider {
	case ProviderClaude:
		// OAuth 令牌只走 Bearer + beta 头（不是平台 API Key）。
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("anthropic-version", "2023-06-01")
		addAnthropicBeta(req, claudeOAuthBeta)
	case ProviderXAI:
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	case ProviderChatGPT:
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("originator", "codex_cli_rs")
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		if id := strings.TrimSpace(accountID); id != "" {
			req.Header.Set("chatgpt-account-id", id)
		}
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取模型目录失败: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return parseModelList(raw), nil
}

// codexModelsEndpoint 拼接 ChatGPT 订阅线路的模型目录端点。
func codexModelsEndpoint(base string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	trimmed = strings.TrimSuffix(trimmed, "/codex/models")
	trimmed = strings.TrimSuffix(trimmed, "/codex")
	trimmed = strings.TrimSuffix(trimmed, "/models")
	trimmed = strings.TrimRight(trimmed, "/")
	return trimmed + "/codex/models"
}

// parseModelList 解析模型目录，兼容 {data:[...]}、{models:[...]} 与裸数组。
func parseModelList(raw []byte) []Model {
	type entry struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Slug        string `json:"slug"`
	}
	toModels := func(entries []entry) []Model {
		out := make([]Model, 0, len(entries))
		for _, e := range entries {
			id := strings.TrimSpace(e.ID)
			if id == "" {
				id = strings.TrimSpace(e.Slug)
			}
			if id == "" {
				continue
			}
			name := strings.TrimSpace(e.DisplayName)
			if name == "" {
				name = strings.TrimSpace(e.Name)
			}
			if name == "" {
				name = id
			}
			out = append(out, Model{ID: id, Name: name})
		}
		return out
	}

	var envelope struct {
		Data   []entry `json:"data"`
		Models []entry `json:"models"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		if len(envelope.Data) > 0 {
			return toModels(envelope.Data)
		}
		if len(envelope.Models) > 0 {
			return toModels(envelope.Models)
		}
	}
	var bare []entry
	if err := json.Unmarshal(raw, &bare); err == nil {
		return toModels(bare)
	}
	return nil
}
