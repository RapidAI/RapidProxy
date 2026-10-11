package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znsoftm/RapidProxy/internal/upstream/hx"
)

// UpstreamModel 是一条 SOLO 目录条目（config 名 + 显示名）。
type UpstreamModel struct {
	ID          string
	DisplayName string
}

// ListModels 读取已登录区的 SOLO 模型目录。每个对话池都有自己的
// get_detail_param 列表；一次并发问齐所有池，取并集（按池顺序去重）。
// 所有池都拿不到时回退错误，由调用方决定是否使用内置默认模型。
// machineID/deviceID/uid 是凭据里存的登录身份，为空时回退派生。
func ListModels(ctx context.Context, hc *http.Client, profile Profile, accessToken, machineID, deviceID, uid string) ([]UpstreamModel, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("%s 模型目录需要已登录的访问令牌", profile.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	outcomes := make([]catalogOutcome, len(chatFunctionOrder))
	var wg sync.WaitGroup
	for i, fn := range chatFunctionOrder {
		wg.Add(1)
		go func(i int, fn string) {
			defer wg.Done()
			outcomes[i].models, outcomes[i].err, outcomes[i].stop = fetchModelCatalog(ctx, hc, profile, accessToken, machineID, deviceID, uid, fn)
		}(i, fn)
	}
	wg.Wait()
	var models []UpstreamModel
	seen := map[string]struct{}{}
	var hard, soft error
	for _, outcome := range outcomes {
		if outcome.err != nil {
			if outcome.stop {
				if hard == nil {
					hard = outcome.err
				}
			} else if soft == nil {
				soft = outcome.err
			}
			continue
		}
		for _, model := range outcome.models {
			if _, ok := seen[model.ID]; ok {
				continue
			}
			seen[model.ID] = struct{}{}
			models = append(models, model)
		}
	}
	if len(models) > 0 {
		return models, nil
	}
	if hard != nil {
		return nil, hard
	}
	if soft != nil {
		return nil, soft
	}
	return nil, fmt.Errorf("%s 模型目录为空", profile.Name)
}

// catalogOutcome 是一个池的目录读取结果。stop 标记主机/传输/额度/认证失败；
// 4001/4023 的成员拒绝不算 stop：其它池可能仍列有模型。
type catalogOutcome struct {
	models []UpstreamModel
	err    error
	stop   bool
}

// fetchModelCatalog 读取一个池。
func fetchModelCatalog(ctx context.Context, hc *http.Client, profile Profile, accessToken, machineID, deviceID, uid, fn string) ([]UpstreamModel, error, bool) {
	body, err := json.Marshal(map[string]any{
		"function":            fn,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	if err != nil {
		return nil, err, true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, profile.ChatHost+ModelsPath, bytes.NewReader(body))
	if err != nil {
		return nil, err, true
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	sessionKey := accessToken
	h.Set("Authorization", "Cloud-IDE-JWT "+sessionKey)
	h.Set("X-Cloudide-Token", sessionKey)
	// 已知登录身份先落头（家族函数只补空缺），避免半缺存储对与
	// 派生兜底混成错误配对。
	if id := strings.TrimSpace(machineID); id != "" {
		h.Set("X-Machine-Id", id)
	}
	if id := strings.TrimSpace(deviceID); id != "" {
		h.Set("X-Device-Id", id)
	}
	if id := strings.TrimSpace(uid); id != "" {
		h.Set("X-Uid", id)
	}
	stampSoloAccountFamily(profile, h, sessionKey)
	// 目录请求携带 Cloud-IDE-JWT：不跟随重定向，防止 307 把令牌重放到
	// 其它主机。
	resp, err := hx.NoFollow(hc).Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 模型目录失败: %w", profile.Name, err), true
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 模型目录失败: %w", profile.Name, err), true
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("读取 %s 模型目录失败 (HTTP %d): %s", profile.Name, resp.StatusCode, summarizeBody(raw)), true
	}
	models, err := parseModelCatalog(profile.Name, raw)
	if err == nil {
		return models, nil, false
	}
	// 4001/4023 表示该池不含此目录。额度/认证码是账号层面的回答，换池无效。
	if code, detail, ok := businessEnvelope(raw); ok {
		return nil, fmt.Errorf("%s 模型目录上游错误 code=%s: %s", profile.Name, code, detail), !wrongFunctionCode(code)
	}
	return nil, err, false
}

// businessEnvelope 报告非成功值的顶层业务码。
func businessEnvelope(raw []byte) (code, detail string, ok bool) {
	var envelope struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return "", "", false
	}
	code = strings.TrimSpace(anyTrim(envelope.Code))
	if code == "" || code == "0" {
		return "", "", false
	}
	detail = strings.TrimSpace(envelope.Message)
	if detail == "" {
		detail = strings.TrimSpace(envelope.Msg)
	}
	return code, detail, true
}

// parseModelCatalog 从目录载荷中提取 chat config 名/显示名对。
// profileName 用于错误信息；国际区还会剔除 IDE 标记为不可见的条目。
func parseModelCatalog(profileName string, raw []byte) ([]UpstreamModel, error) {
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("%s 模型目录无法解析", profileName)
	}
	configList := findKeyed(payload, "config_info_list", "configInfoList", "ConfigInfoList")
	entries, ok := catalogEntries(configList)
	if !ok {
		return nil, fmt.Errorf("%s 模型目录缺少 config_info_list (%s)", profileName, payloadKeyList(payload))
	}
	hideInvisible := profileName == NameGlobal
	models := make([]UpstreamModel, 0, len(entries))
	for _, entry := range entries {
		if !catalogChatEntry(entry, hideInvisible) {
			continue
		}
		id := strings.TrimSpace(asString(entry["config_name"]))
		name := catalogDisplayName(entry)
		if name == "" {
			name = id
		}
		models = append(models, UpstreamModel{ID: id, DisplayName: name})
	}
	return models, nil
}

// catalogChatEntry 报告一个 llm_utils_chat 可以调用的 config。同一列表还
// 携带 IDE 自己的辅助配置（摘要、快速应用、标题生成）、已下线的 config 与
// 自定义模型槽位——按名字去调它们只会得到 4001。
func catalogChatEntry(entry map[string]any, hideInvisible bool) bool {
	id := strings.TrimSpace(asString(entry["config_name"]))
	if id == "" || strings.EqualFold(id, "auto") {
		return false
	}
	if strings.HasPrefix(strings.ToLower(id), "custom_model") {
		return false
	}
	if usage := strings.TrimSpace(asString(entry["usage"])); usage != "" && usage != "chat_completion" {
		return false
	}
	if off, ok := entry["config_switch"].(bool); ok && !off {
		return false
	}
	if catalogCustomModel(entry) {
		return false
	}
	if hideInvisible {
		if hidden, ok := entry["is_invisible_to_user"].(bool); ok && hidden {
			return false
		}
	}
	return true
}

func catalogCustomModel(entry map[string]any) bool {
	if custom, ok := entry["is_custom_model"].(bool); ok && custom {
		return true
	}
	display, ok := entry["display_config"].(map[string]any)
	if !ok {
		return false
	}
	custom, ok := display["is_custom_model"].(bool)
	return ok && custom
}

// findKeyed 返回任一键名下的第一个值（包括数组）。这里不能用只认对象的
// findObject：get_detail_param 把 config_info_list 以 JSON 数组返回。
func findKeyed(payload any, keys ...string) any {
	switch tree := payload.(type) {
	case map[string]any:
		for _, key := range keys {
			if raw, ok := tree[key]; ok && raw != nil {
				return raw
			}
		}
		// map 迭代顺序随机。响应可能携带多个嵌套列表；按稳定名字遍历，
		// 优先主机实际包裹目录的容器。
		names := make([]string, 0, len(tree))
		for name := range tree {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool {
			if rank := catalogContainerRank(names[i]) - catalogContainerRank(names[j]); rank != 0 {
				return rank < 0
			}
			return names[i] < names[j]
		})
		for _, name := range names {
			if nested := findKeyed(tree[name], keys...); nested != nil {
				return nested
			}
		}
	case []any:
		for _, child := range tree {
			if nested := findKeyed(child, keys...); nested != nil {
				return nested
			}
		}
	}
	return nil
}

func catalogContainerRank(name string) int {
	switch name {
	case "Result", "result", "data", "Data", "response", "Response":
		return 0
	default:
		return 1
	}
}

// catalogEntries 接受活动数组与单个 config 对象。
func catalogEntries(raw any) ([]map[string]any, bool) {
	return catalogEntriesDepth(raw, 0)
}

func catalogEntriesDepth(raw any, depth int) ([]map[string]any, bool) {
	if depth > 4 {
		return nil, false
	}
	switch value := raw.(type) {
	case []any:
		entries := make([]map[string]any, 0, len(value))
		for _, item := range value {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			entries = append(entries, obj)
		}
		return entries, true
	case map[string]any:
		switch nested := findKeyed(value, "config_info_list", "configInfoList", "ConfigInfoList").(type) {
		case []any:
			return catalogEntriesDepth(nested, depth+1)
		case map[string]any:
			if strings.TrimSpace(asString(nested["config_name"])) != "" {
				return []map[string]any{nested}, true
			}
		}
		if strings.TrimSpace(asString(value["config_name"])) == "" {
			return nil, false
		}
		return []map[string]any{value}, true
	default:
		return nil, false
	}
}

func catalogDisplayName(entry map[string]any) string {
	if display, ok := entry["display_config"].(map[string]any); ok {
		if name := strings.TrimSpace(asString(display["display_name"])); name != "" {
			return name
		}
	}
	return strings.TrimSpace(asString(entry["display_name"]))
}

func payloadKeyList(payload any) string {
	obj, ok := payload.(map[string]any)
	if !ok || len(obj) == 0 {
		return "empty"
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
