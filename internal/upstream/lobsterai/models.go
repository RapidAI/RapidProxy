package lobsterai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znsoftm/RapidProxy/internal/upstream/hx"
)

// UpstreamModel 是一条 LobsterAI 目录条目。
type UpstreamModel struct {
	ID   string
	Name string
}

// allowlist 是 2026-09 真机 GET /api/models/available 实测节选（含 1M 上下文
// 窗口的主力模型）。远端目录优先；这份兜底只在目录请求失败时提供默认值。
var allowlist = []UpstreamModel{
	{ID: "glm-5.3", Name: "GLM-5.3"},
	{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash"},
	{ID: "glm-5.3-flashx", Name: "GLM-5.3-FlashX"},
	{ID: "glm-5.2", Name: "GLM-5.2"},
	{ID: "deepseek-flash", Name: "DeepSeek-V4.1-Flash"},
	{ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro"},
	{ID: "deepseek-v4-flash", Name: "DeepSeek-V4-Flash"},
}

// ModelList 返回内置兜底目录。
func ModelList() []UpstreamModel {
	return append([]UpstreamModel(nil), allowlist...)
}

// ListModels 读取远端目录；失败时由调用方回退到 allowlist。
func ListModels(ctx context.Context, hc *http.Client, accessToken string) ([]UpstreamModel, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, errors.New("需要已登录的访问令牌")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	endpoint := APIBase + ModelsPath + "?keyfrom=" + url.QueryEscape(unixMillis())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("X-LobsterAI-Client-Capabilities", ClientCapabilities)
	req.Header.Set("X-LobsterAI-Client-Version", ClientVersion)
	resp, err := hx.NoFollow(hc).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, summarizeBody(raw))
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, errors.New("模型目录响应无法解析")
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("模型目录 code=%d %s", envelope.Code, envelope.Msg)
	}
	var entries []struct {
		ModelID   string `json:"modelId"`
		ModelName string `json:"modelName"`
	}
	if err := json.Unmarshal(envelope.Data, &entries); err != nil {
		return nil, fmt.Errorf("模型目录响应无法解析: %w", err)
	}
	models := make([]UpstreamModel, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ModelID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(entry.ModelName)
		if name == "" {
			name = id
		}
		models = append(models, UpstreamModel{ID: id, Name: name})
	}
	return models, nil
}
