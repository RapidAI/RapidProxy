// 模型目录：走官方 CLI 的签名契约——wasm COSY bearer + 站点推理主机上
// /algo 前缀的目录路径。响应是 scene → 条目数组的 map，按普通 JSON 解析；
// 返回加密载荷的部署会让解析失败，调用方保留兜底默认值。
package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/znsoftm/RapidProxy/internal/upstream/hx"
	"github.com/znsoftm/RapidProxy/internal/upstream/qoder/cosy"
)

// Model 是一条目录条目。
type Model struct {
	Key         string   `json:"key"`
	DisplayName string   `json:"display_name"`
	IsDefault   bool     `json:"is_default"`
	Enabled     bool     `json:"enabled"`
	Reasoning   bool     `json:"is_reasoning"`
	Vision      bool     `json:"is_vl"`
	MaxInput    int      `json:"max_input_tokens"`
	MaxOutput   int      `json:"max_output_tokens"`
	Tags        []string `json:"tags"`
	Scene       string   `json:"scene"`
}

// ListModels 通过官方 CLI 的签名契约取全量服务端目录：wasm COSY 上下文
// （uid + 设备 token 折进签名材料）在站点推理主机上准备
// /algo/api/v2/model/list 请求。返回解析出的模型与默认模型 key。
func ListModels(ctx context.Context, hc *http.Client, profile Profile, uid, accessToken string) ([]Model, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, "", fmt.Errorf("未登录，无法获取 Qoder 模型列表")
	}
	// 与 trae/lobsterai 的目录读取一致：目录是同步链路的一环，
	// 单次 30 秒兜底，避免挂住的连接吃掉整轮同步窗口。
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	signed, err := cosy.New(MachineID(), ClientVersion, strings.TrimSpace(uid))
	if err != nil {
		return nil, "", fmt.Errorf("Qoder 签名上下文初始化失败: %w", err)
	}
	defer signed.Close()
	if err := signed.RefreshAuthFields(strings.TrimSpace(accessToken)); err != nil {
		return nil, "", fmt.Errorf("Qoder 签名材料注入失败: %w", err)
	}
	prepared, err := signed.PrepareRequest(strings.TrimRight(profile.InferBase, "/"), "/algo/api/v2/model/list", http.MethodGet, "auth", "", "")
	if err != nil {
		return nil, "", fmt.Errorf("Qoder 签名请求构建失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prepared.URL, nil)
	if err != nil {
		return nil, "", err
	}
	for key, value := range prepared.Headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("User-Agent", UserAgent)
	// 目录请求携带 COSY 签名头：不跟随重定向，防止 307 把令牌重放到
	// 其它主机。
	resp, err := hx.NoFollow(hc).Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("获取 Qoder 模型列表失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", fmt.Errorf("读取 Qoder 模型列表响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("获取 Qoder 模型列表失败 (HTTP %d): %s", resp.StatusCode, truncateForError(body))
	}
	return parseCatalog(body)
}

// parseCatalog 接受 {scene: [entry...]}，scene 名不限。最早出现的启用
// is_default 条目胜出；scene 按稳定顺序遍历，默认值不依赖 Go map 迭代顺序。
func parseCatalog(body []byte) ([]Model, string, error) {
	var scenes map[string]json.RawMessage
	if err := json.Unmarshal(body, &scenes); err != nil {
		return nil, "", fmt.Errorf("Qoder 模型列表响应无法解析: %w", err)
	}
	var models []Model
	defaultKey := ""
	fallbackKey := ""
	for _, scene := range sortedScenes(scenes) {
		var entries []json.RawMessage
		if err := json.Unmarshal(scenes[scene], &entries); err != nil {
			continue
		}
		for _, rawEntry := range entries {
			model, ok := parseModel(rawEntry, scene)
			if !ok {
				continue
			}
			models = append(models, model)
			if !model.Enabled {
				continue
			}
			if model.IsDefault && defaultKey == "" {
				defaultKey = model.Key
			}
			if fallbackKey == "" {
				fallbackKey = model.Key
			}
		}
	}
	if len(models) == 0 {
		return nil, "", fmt.Errorf("Qoder 模型列表为空")
	}
	if defaultKey == "" {
		defaultKey = fallbackKey
	}
	return models, defaultKey, nil
}

// sortedScenes 以确定性的顺序排列目录 scene：设备账号 scene 优先，
// 其次 CLI scene，其余按名字。
func sortedScenes(scenes map[string]json.RawMessage) []string {
	priority := map[string]int{"assistant": 0, "agent": 1, "cli": 2}
	names := make([]string, 0, len(scenes))
	for scene := range scenes {
		names = append(names, scene)
	}
	sort.Slice(names, func(i, j int) bool {
		pi, pj := priority[names[i]], priority[names[j]]
		if pi == pj {
			return names[i] < names[j]
		}
		return pi < pj
	})
	return names
}

type rawModel struct {
	Key         string   `json:"key"`
	ModelKey    string   `json:"model_key"`
	DisplayName string   `json:"display_name"`
	Name        string   `json:"name"`
	IsDefault   bool     `json:"is_default"`
	Enable      any      `json:"enable"` // 旧版服务端可能是 bool 或 0/1
	IsReasoning bool     `json:"is_reasoning"`
	IsVL        bool     `json:"is_vl"`
	MaxInput    int      `json:"max_input_tokens"`
	MaxOutput   int      `json:"max_output_tokens"`
	Tags        []string `json:"tags"`
}

func truthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case int:
		return v != 0
	default:
		return true
	}
}

func parseModel(raw json.RawMessage, scene string) (Model, bool) {
	if len(raw) == 0 {
		return Model{}, false
	}
	var entry rawModel
	if err := json.Unmarshal(raw, &entry); err != nil {
		return Model{}, false
	}
	key := strings.TrimSpace(entry.Key)
	if key == "" {
		key = strings.TrimSpace(entry.ModelKey)
	}
	if key == "" {
		return Model{}, false
	}
	display := strings.TrimSpace(entry.DisplayName)
	if display == "" {
		display = strings.TrimSpace(entry.Name)
	}
	enabled := truthy(entry.Enable)
	return Model{
		Key:         key,
		DisplayName: display,
		IsDefault:   entry.IsDefault,
		Enabled:     enabled,
		Reasoning:   entry.IsReasoning,
		Vision:      entry.IsVL,
		MaxInput:    entry.MaxInput,
		MaxOutput:   entry.MaxOutput,
		Tags:        entry.Tags,
		Scene:       scene,
	}, true
}
