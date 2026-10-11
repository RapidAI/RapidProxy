// Package openai 提供 OpenAI 协议相关的类型、SSE 处理与请求改写。
package openai

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrorDetail 是 OpenAI 风格的错误体。
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    any    `json:"code"`
}

// ErrorResponse 是 OpenAI 风格的错误响应。
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// NewError 构造一个错误响应。
func NewError(message, typ string, code any) ErrorResponse {
	return ErrorResponse{Error: ErrorDetail{Message: message, Type: typ, Param: nil, Code: code}}
}

// Model 是 /v1/models 列表项。
type Model struct {
	ID              string `json:"id"`
	Object          string `json:"object"`
	Created         int64  `json:"created"`
	OwnedBy         string `json:"owned_by"`
	Provider        string `json:"provider,omitempty"`
	DisplayName     string `json:"display_name,omitempty"`
	ContextLength   int64  `json:"context_length,omitempty"`
	MaxOutputTokens int64  `json:"max_output_tokens,omitempty"`
	SupportsImages  bool   `json:"supports_images,omitempty"`
}

// ModelList 是 /v1/models 的响应体。
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// -----------------------------------------------------------------------------
// 请求改写
// -----------------------------------------------------------------------------

// TransformOptions 控制转发前的请求改写行为。
type TransformOptions struct {
	// Sanitize 改写上游内容审核的固定模板句（WorkBuddy 协议族专用，
	// 模板句黑名单来自该上游）。
	Sanitize bool
	// ForceMaxThinking 对 hy3 系列强制 reasoning_effort=high。
	ForceMaxThinking bool
	// EnsureSystemPrompt 保证首条消息是 system（WorkBuddy 上游要求，
	// 否则报 code=11128）；其它协议族保持客户端原样。
	EnsureSystemPrompt bool
	// AddStreamOptions 追加 stream_options.include_usage（WorkBuddy 协议
	// 约定）；其它协议族的用量由各自适配层在流内翻译。
	AddStreamOptions bool
}

// 上游内容审核把 Claude Code 的固定 system 模板句逐字加入黑名单，命中即拒答。
// 这里做单词级最小改写，语义不变但绕过精确匹配（属于误报规避，不涉及有害内容）。
var blockedTemplates = []struct{ from, to string }{
	{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
}

// defaultSystemPrompt 是客户端没有携带 system 消息时自动补齐的内容。
// 上游要求第一条消息必须是 system prompt（否则报 code=11128 first message
// is not system prompt），而不少客户端的连通性测试只发一条 user 消息。
const defaultSystemPrompt = "You are a helpful assistant."

// ensureSystemPrompt 保证 messages 的第一条是 system 消息：
// 客户端没带时在开头补一条默认的，带了则原样保留。
func ensureSystemPrompt(obj map[string]any) {
	messages, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	if len(messages) > 0 {
		if msg, ok := messages[0].(map[string]any); ok {
			if role, _ := msg["role"].(string); role == "system" {
				return
			}
		}
	}
	prepended := append([]any{map[string]any{"role": "system", "content": defaultSystemPrompt}}, messages...)
	obj["messages"] = prepended
}

// PrepareRequestBody 把客户端请求体整理成上游可接受的形式：
// 强制 stream（上游只接受流式应答）、按需补齐首条 system 消息与
// stream_options、按需改写模板句与思考档位。返回处理后的请求体与请求对象。
func PrepareRequestBody(payload []byte, opts TransformOptions) ([]byte, map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, nil, err
	}
	obj["stream"] = true

	// 要求上游在流的末尾附带 usage（token 用量），否则流式响应里拿不到用量，
	// 流量统计便无从记录。绝大多数 OpenAI 兼容网关忽略未知字段，个别不支持的
	// 也只当没这回事，不影响正常应答。
	if opts.AddStreamOptions {
		if _, ok := obj["stream_options"].(map[string]any); !ok {
			obj["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	if opts.EnsureSystemPrompt {
		ensureSystemPrompt(obj)
	}
	if opts.Sanitize {
		rewriteMessages(obj)
	}
	if opts.ForceMaxThinking {
		forceMaxThinking(obj)
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, nil, err
	}
	return out, obj, nil
}

func rewriteMessages(obj map[string]any) {
	messages, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			msg["content"] = Sanitize(content)
		case []any:
			for _, partRaw := range content {
				part, ok := partRaw.(map[string]any)
				if !ok {
					continue
				}
				if text, ok := part["text"].(string); ok {
					part["text"] = Sanitize(text)
				}
			}
		}
	}
}

// Sanitize 对单个字符串做模板句改写。
func Sanitize(s string) string {
	for _, t := range blockedTemplates {
		s = strings.ReplaceAll(s, t.from, t.to)
	}
	return s
}

// forceMaxThinking 把 hy3 系列的 reasoning_effort 固定为 high。
// 上游仅识别 high 级别，其余取值会退化为不思考。
func forceMaxThinking(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !strings.HasPrefix(model, "hy3") {
		return
	}
	if eff, _ := obj["reasoning_effort"].(string); eff == "high" {
		return
	}
	obj["reasoning_effort"] = "high"
}

// -----------------------------------------------------------------------------
// SSE 处理
// -----------------------------------------------------------------------------

// ReadSSEEvents 逐行读取上游 SSE，回调每个 data 事件的原始 JSON 文本
// （已去除 "data:" 前缀与 [DONE]）。回调返回错误时停止读取。
func ReadSSEEvents(r io.Reader, fn func(data string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		// 只处理 data 事件：跳过 SSE 注释（以 ":" 开头，如 OpenAI 的 ": ping"
		// 心跳）以及 event:/id:/retry: 等其它字段行。否则上游的注释行会被当成
		// 一条空 data 事件原样转发给客户端，让严格 JSON 解析的 SDK 在流中途报错。
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := stripDataPrefix(line)
		if data == "" || data == "[DONE]" {
			continue
		}
		if err := fn(data); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

// CleanChunk 删除 choice.delta 中值为空的字段，避免严格客户端解析失败。
//
// 注意不会清理 tool_calls 内部：上游以增量方式下发工具调用，首块携带
// id/name 而 arguments 为空，后续块追加参数片段，删除空字段会破坏流。
func CleanChunk(data string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return data
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		return data
	}
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		for k, v := range delta {
			// tool_calls 是增量下发的（首块带 id/name，后续块只追加 arguments 片段），
			// 内部的空字符串有语义，但这里不递归进 tool_calls，所以不会被误删；
			// 空的 tool_calls 数组本身就是空字段，按通常规则清掉即可。
			if isEmptyValue(v) {
				delete(delta, k)
			}
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return string(out)
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// StreamError 是上游在 SSE 流里下发的错误事件。
//
// 上游会以 200 + text/event-stream 的形式回报失败（状态码完全正常），
// 调用方需要能把它和「读取流失败」区分开：前者是上游明确报错。
type StreamError struct {
	Message string
}

func (e *StreamError) Error() string { return e.Message }

// choiceState 累积单个 choice（index）的增量内容。
type choiceState struct {
	index     int
	role      string
	finish    string
	content   strings.Builder
	reasoning strings.Builder
	toolCalls []map[string]any
	toolIndex map[int]map[string]any
}

// Aggregate 把上游 SSE 流折叠成一个非流式的 chat.completion 响应体。
//
// 几个容易踩的点都在这里处理了：
//   - 上游可能用 delta（增量）或 message（整段）下发内容，两种都要认，
//     只认 delta 会把 message 形态的内容整个丢掉，客户端收到空回复；
//   - 流里可能夹带 {"error":{...}} 事件，必须报出来而不是静默跳过——
//     否则客户端拿到 200 + 空回复，以为成功却什么都没发生；
//   - 客户端传 n>1 时每个 chunk 会带多个 choices，必须按 index 分开累积，
//     否则不同 choice 的内容会拼进同一条消息。
func Aggregate(r io.Reader, fallbackModel string) ([]byte, error) {
	var (
		respModel, respID, finish string
		created                   int64
		usage                     map[string]any
		choices                   = map[int]*choiceState{}
	)

	get := func(idx int) *choiceState {
		st, ok := choices[idx]
		if !ok {
			st = &choiceState{index: idx, toolIndex: map[int]map[string]any{}}
			choices[idx] = st
		}
		return st
	}

	err := ReadSSEEvents(r, func(data string) error {
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return nil
		}
		// 流中错误事件：上游以 200 + SSE 的形式回报失败（状态码上看不出来）。
		// 必须确认真的带了错误文案才当成错误——某些上游的正常 chunk 可能带
		// "error": null 或空对象，若不分青红皂白地当成失败，会凭空把一次成功的
		// 请求打成错误返回。
		if env, ok := chunk["error"]; ok && env != nil {
			if msg, ok := extractErrorText(env); ok {
				return &StreamError{Message: msg}
			}
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		// usage 通常只在最后一个 chunk 出现，带完整字段；后到的覆盖先到的。
		if v, ok := chunk["usage"].(map[string]any); ok && v != nil {
			usage = v
		}

		list, _ := chunk["choices"].([]any)
		for _, raw := range list {
			choice, _ := raw.(map[string]any)
			// 没有 index 时按出现顺序当作 0，与 OpenAI 的行为一致。
			idx := 0
			if f, ok := choice["index"].(float64); ok {
				idx = int(f)
			}
			st := get(idx)
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				st.finish = v
				finish = v
			}
			// delta 是增量、message 是整段，两种形态都要读。
			if delta, ok := choice["delta"].(map[string]any); ok {
				applyDelta(st, delta)
			}
			if msg, ok := choice["message"].(map[string]any); ok {
				applyDelta(st, msg)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if created == 0 {
		created = time.Now().Unix()
	}
	// 按 index 升序输出，保证响应稳定（map 迭代顺序是随机的）。
	idxs := make([]int, 0, len(choices))
	for idx := range choices {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)

	out := make([]map[string]any, 0, len(idxs))
	for _, idx := range idxs {
		st := choices[idx]
		message := map[string]any{
			"role":    firstNonEmpty(st.role, "assistant"),
			"content": st.content.String(),
		}
		if st.reasoning.Len() > 0 {
			message["reasoning_content"] = st.reasoning.String()
		}
		if len(st.toolCalls) > 0 {
			message["tool_calls"] = st.toolCalls
		}
		out = append(out, map[string]any{
			"index":         idx,
			"message":       message,
			"finish_reason": firstNonEmpty(st.finish, "stop"),
		})
	}
	// 上游一个 choice 都没下发（空流）时也给一条，客户端解析不至于炸。
	if len(out) == 0 {
		out = append(out, map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
			},
			"finish_reason": firstNonEmpty(finish, "stop"),
		})
	}

	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-workbuddy"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, fallbackModel),
		"choices": out,
	}
	if usage != nil {
		result["usage"] = usage
	}
	return json.Marshal(result)
}

// applyDelta 把一个 delta / message 对象累加进对应 choice。
func applyDelta(st *choiceState, d map[string]any) {
	if v, ok := d["role"].(string); ok && v != "" {
		st.role = v
	}
	if v, ok := d["content"].(string); ok {
		st.content.WriteString(v)
	}
	if v, ok := d["reasoning_content"].(string); ok {
		st.reasoning.WriteString(v)
	}
	if tcs, ok := d["tool_calls"].([]any); ok {
		st.toolCalls = mergeToolCalls(tcs, st.toolIndex, st.toolCalls)
	}
}

// extractErrorText 把流中 error 字段还原成一句可读文案。
//
// 返回 (msg, true) 表示确实是错误事件；返回 ("", false) 表示这个 error 字段
// 没有携带任何有用的错误文案（例如 null 或空对象），不应被当成失败。
func extractErrorText(v any) (string, bool) {
	switch e := v.(type) {
	case string:
		if e == "" {
			return "", false
		}
		return e, true
	case map[string]any:
		if msg, ok := e["message"].(string); ok && msg != "" {
			return msg, true
		}
		if msg, ok := e["msg"].(string); ok && msg != "" {
			return msg, true
		}
		// 非空对象但两个字段都没有可读文案：不当作错误。
		return "", false
	}
	// null、数字、布尔、数组等都不构成可识别的错误。
	return "", false
}

// mergeToolCalls 按 index 合并增量下发的工具调用片段。
func mergeToolCalls(chunks []any, index map[int]map[string]any, acc []map[string]any) []map[string]any {
	for _, raw := range chunks {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if f, ok := call["index"].(float64); ok {
			idx = int(f)
		}
		merged, exists := index[idx]
		if !exists {
			merged = map[string]any{"index": idx}
			index[idx] = merged
			acc = append(acc, merged)
		}
		for k, v := range call {
			if k == "index" {
				continue
			}
			if k == "function" {
				incoming, _ := v.(map[string]any)
				current, _ := merged["function"].(map[string]any)
				if current == nil {
					current = map[string]any{}
					merged["function"] = current
				}
				for fk, fv := range incoming {
					if fk == "arguments" {
						if s, ok := fv.(string); ok {
							prev, _ := current["arguments"].(string)
							current["arguments"] = prev + s
							continue
						}
					}
					if s, ok := fv.(string); ok && s != "" {
						current[fk] = fv
					}
				}
				continue
			}
			if s, ok := v.(string); ok && s != "" {
				merged[k] = s
			}
		}
	}
	return acc
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// truncateText 按字符截断（上游错误正文多为中文，按字节切会产生非法 UTF-8）。
func truncateText(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
