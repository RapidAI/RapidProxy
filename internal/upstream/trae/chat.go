package trae

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Chat 把 OpenAI 形状的请求体翻译成 SOLO llm_utils_chat 并发到区的对话主机，
// 把上游事件流实时翻译回 OpenAI chunk 流。
//
// 返回的响应永远是 text/event-stream（除非上游给了非 200）。非流式调用由
// 代理服务层折叠，流内错误经 OpenAI error 帧暴露，换号/冷却逻辑照常生效。
// machineID/deviceID/uid 是凭据里存的登录身份（设备对 + 账号 uid）：
// refresh token 系于该设备对，为空时回退到 claims/token 派生。
func Chat(ctx context.Context, hc *http.Client, profile Profile, accessToken, machineID, deviceID, uid string, body []byte) (*http.Response, error) {
	// 解析一次得到体对象与模型名；每次池尝试只改写 function/request_id
	// 后序列化，session_id 保持不变（重试仍是同一会话）。
	solo, model, _ := prepareBody(body, profile)
	for i, fn := range chatFunctionOrder {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		solo["function"] = fn
		solo["request_id"] = randomUUID()
		sent, err := json.Marshal(solo)
		if err != nil {
			return nil, err
		}
		outgoing, err := http.NewRequestWithContext(ctx, http.MethodPost, profile.ChatHost+ChatPath, bytes.NewReader(sent))
		if err != nil {
			return nil, err
		}
		// 全新请求不会带任何默认头：SOLO 头族之外必须显式声明 JSON 体。
		outgoing.Header.Set("Content-Type", "application/json")
		applyChatHeaders(profile, outgoing.Header, accessToken, machineID, deviceID, uid, true)
		outgoing.ContentLength = int64(len(sent))
		outgoing.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(sent)), nil
		}
		resp, err := hc.Do(outgoing)
		if err != nil || resp == nil {
			return resp, err
		}
		// 4001/4023 表示该池的目录不含 config_name，换下一个池；
		// 其它状态就是最终答案。
		if resp.StatusCode != http.StatusOK || i == len(chatFunctionOrder)-1 {
			return finalizeResponse(ctx, resp, profile, model, nil, false), nil
		}
		// consumeFunctionRejection 返回它已消费的字节（不再缝回 body，
		// 由 finalizeResponse 统一负责拼接，避免前缀重复进流）。
		reject, head, jsonMode, peekErr := consumeFunctionRejection(resp)
		if peekErr != nil {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			return upstreamFailure(resp, peekErr.Error()), nil
		}
		if !reject {
			return finalizeResponse(ctx, resp, profile, model, head, jsonMode), nil
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	return nil, fmt.Errorf("trae: no upstream response")
}

// finalizeResponse 决定最终应答的形态：非 200 原样透传；200 且开头是
// 带业务码的 JSON 信封时按业务码映射成 4xx/502 错误（模型不在该池/额度等）；
// JSON 信封但无错误码时原样透传；其余进入实时 SSE 翻译管道。
//
// head/jsonMode 是 consumeFunctionRejection 已经预读并判定的开头与形态；
// 两者为零值时（最后一池 / 非 200）由本函数自己预读。head 字节尚未缝回
// body，本函数用 prefixBody 恰好拼接一次。
func finalizeResponse(ctx context.Context, resp *http.Response, profile Profile, model string, head []byte, jsonMode bool) *http.Response {
	if resp.StatusCode != http.StatusOK {
		return resp
	}
	if model == "" {
		model = profile.DefaultModel
	}
	if head == nil {
		var err error
		head, jsonMode, err = peekShape(resp, peekLimit)
		if err != nil {
			// 预读阶段就断了流：按上游故障处理，服务层会冷却该账号并换号。
			_ = resp.Body.Close()
			return upstreamFailure(resp, err.Error())
		}
	}
	if code, detail, ok := businessEnvelope(head); ok {
		_ = resp.Body.Close()
		// 业务码映射成对应 HTTP 语义：额度类 402（长冷却）、认证类
		// 401/403（触发服务层刷新重试），其余 502。一律压成 502 会让
		// 额度耗尽的账号只进入秒级的服务器故障冷却。
		return upstreamFailureWithStatus(resp, envelopeStatus(code),
			fmt.Sprintf("Trae 上游错误 code=%s: %s", code, annotateErrorCode(code, detail)))
	}
	if jsonMode {
		// 非错误码的 JSON 应答（罕见）：原样透传，保持代理透明。
		resp.Body = &prefixBody{prefix: head, rest: resp.Body}
		return resp
	}
	reader, writer := io.Pipe()
	translated := *resp
	translated.Body = reader
	translated.ContentLength = -1
	translated.Uncompressed = true
	translated.Header = resp.Header.Clone()
	translated.Header.Set("Content-Type", "text/event-stream")
	translated.Header.Del("Content-Length")
	translated.Header.Del("Content-Encoding")
	go func() {
		err := translateSSE(ctx, &prefixBody{prefix: head, rest: resp.Body}, writer, model)
		_ = resp.Body.Close()
		_ = writer.CloseWithError(err)
	}()
	return &translated
}

// peekLimit 限定形态预读的累计字节数上限。
const peekLimit = 64 << 10

// peekShape 预读响应头部，直到能判定形态为止；读出的字节之后必须回放
// （调用方用 prefixBody 缝合）。
//
// 判定规则：跳过前导空白行后首个非空字符是 '{' 视为 JSON 信封，继续累积
// 到 json.Valid 或上限——信封体本身很小，不存在等待问题；其余
// （"event:"/"data:" 等）视为 SSE，读到该行换行立即返回。SSE 分支绝不为
// 凑满缓冲而等待，否则长回答的首字节延迟会被预读拖到上游产出 64KB 之后。
func peekShape(resp *http.Response, limit int) (head []byte, jsonMode bool, err error) {
	if resp == nil || resp.Body == nil {
		return nil, false, nil
	}
	buf := make([]byte, 4096)
	var out []byte
	readMore := func() (bool, error) {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if rerr != nil {
			if rerr == io.EOF {
				return false, nil
			}
			return false, rerr
		}
		return true, nil
	}
	// 首个非空行完整（含换行）或触顶即停：只看第一个 \n 会把
	// "空白行 + JSON" 的流误判成 SSE。
	for {
		trimmed := strings.TrimLeft(string(out), " \t\r\n")
		if (trimmed != "" && strings.IndexByte(trimmed, '\n') >= 0) || len(out) >= limit {
			break
		}
		more, rerr := readMore()
		if rerr != nil {
			return out, false, rerr
		}
		if !more {
			break
		}
	}
	if !strings.HasPrefix(strings.TrimLeft(string(out), " \t\r\n"), "{") {
		return out, false, nil
	}
	// JSON 信封：累积到可解析或触顶。
	for !json.Valid(out) {
		if len(out) >= limit {
			return out, true, nil
		}
		more, rerr := readMore()
		if rerr != nil {
			return out, json.Valid(out), rerr
		}
		if !more {
			return out, json.Valid(out), nil
		}
	}
	return out, true, nil
}

// prefixBody 先回放已经从上游拉出的字节，再交出剩余部分。
type prefixBody struct {
	prefix []byte
	rest   io.ReadCloser
	off    int
}

func (p *prefixBody) Read(b []byte) (int, error) {
	if p.off < len(p.prefix) {
		n := copy(b, p.prefix[p.off:])
		p.off += n
		return n, nil
	}
	if p.rest == nil {
		return 0, io.EOF
	}
	return p.rest.Read(b)
}

func (p *prefixBody) Close() error {
	if p.rest == nil {
		return nil
	}
	return p.rest.Close()
}

// applyChatHeaders 打上 SOLO 请求头族。账号身份优先使用凭据里存的登录
// 身份（设备对 + uid，token 家族 mint 时所系）——必须先于家族函数落头，
// 否则半缺的存储对会与派生兜底混成错误配对；版本头按请求路由到的区填写。
func applyChatHeaders(profile Profile, h http.Header, accessToken, machineID, deviceID, uid string, stream bool) {
	key := strings.TrimSpace(accessToken)
	h.Set("Authorization", "Cloud-IDE-JWT "+key)
	h.Set("X-Cloudide-Token", key)
	if id := strings.TrimSpace(machineID); id != "" {
		h.Set("X-Machine-Id", id)
	}
	if id := strings.TrimSpace(deviceID); id != "" {
		h.Set("X-Device-Id", id)
	}
	if id := strings.TrimSpace(uid); id != "" {
		h.Set("X-Uid", id)
	}
	stampSoloAccountFamily(profile, h, key)
	trace := randomHex(16)
	if trace != "" {
		h.Set("X-Custom-Trace-Id", trace)
		span := trace
		if len(span) > 16 {
			span = span[:16]
		}
		h.Set("X-Flow-Traceparent", fmt.Sprintf("04-%s-%s-01", trace, span))
	}
	if stream {
		h.Set("Accept", "text/event-stream")
		if requestID := randomHex(16); requestID != "" {
			h.Set("X-Request-ID", requestID)
			h.Set("X-Trae-Request-ID", requestID)
		}
	}
}

// stampSoloAccountFamily 是所有 SOLO 调用共享的头集合：桌面身份、各区 IDE
// 版本、按 token claims 派生的设备对兜底。聊天与目录调用方必须一致，
// 否则上游会把两个端点当成两个客户端。
func stampSoloAccountFamily(profile Profile, h http.Header, sessionKey string) {
	version := strings.TrimSpace(profile.ClientVersion)
	if version == "" {
		version = profile.IDEVersion
	}
	versionCode := strings.TrimSpace(profile.ClientVersionCode)
	if versionCode == "" {
		versionCode = profile.IDEVersionCode
	}
	h.Set("User-Agent", UserAgentPrefix+version)
	h.Set("x-app-id", AppID)
	h.Set("X-App-Version", version)
	h.Set("X-Ide-Version", version)
	h.Set("X-Ide-Version-Code", versionCode)
	h.Set("X-App-Version-Code", versionCode)
	h.Set("X-Ide-Version-Type", "stable")
	h.Set("X-Device-Type", "windows")
	h.Set("X-OS-Version", "1.0")
	h.Set("X-Device-Brand", "PC")
	h.Set("Request-Traffic-Type", "prod")
	// 凭据里没有设备对时按 claims 派生兜底，重试之间保持一致。
	if h.Get("X-Uid") == "" {
		claims := parseJWTClaims(sessionKey)
		if uid := firstNonEmpty(claims.UserID, claims.UID); uid != "" {
			h.Set("X-Uid", uid)
		}
	}
	if h.Get("X-Machine-Id") == "" {
		// 派生键必须是账号身份：uid 优先；claims 都解不出时退回 token
		// 本身——绝不能用空串，否则多个 claims 异常的账号会共享同一
		// 设备指纹，正是上游风控打击的形态。
		key := h.Get("X-Uid")
		if key == "" {
			key = sessionKey
		}
		identity := IdentityForUserID(key)
		h.Set("X-Machine-Id", identity.MachineID)
		h.Set("X-Device-Id", identity.DeviceID)
	}
}

// soloChatFunction 是本客户端首选询问的 SOLO Work 池。config_name 只在
// 列出它的池里有效。
const soloChatFunction = "solo_work_lite"

// chatFunctionOrder 是模型主机上的全部对话池。主机在 config_name 不属于
// 指定池时回答 4001（glm-5.2 当前在国内目录由 chat_v3 提供，在 SOLO Work
// 目录由 solo_work_lite 提供）。
var chatFunctionOrder = []string{
	soloChatFunction,
	"chat_v3",
	"solo_agent",
	"solo_agent_lite",
}

// prepareBody 把一份 OpenAI chat 载荷转换为 SOLO llm_utils_chat 的体对象。
// llm_utils_chat 拒绝它未定义的 OpenAI 字段（thinking、reasoning_effort、
// enable_thinking 报 4001 "param is invalid"），因此只透传白名单字段。
// content 字符串变成 text chip，model 兼作 config_name，并钉住 SOLO 池。
//
// 返回体对象而非序列化结果：调用方在每次池尝试时改写 function/request_id
// 后再 marshal——序列化属于该次 HTTP 请求，解析只做一次（载荷可达数 MB）。
func prepareBody(payload []byte, profile Profile) (map[string]any, string, bool) {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		obj = map[string]any{}
	}
	originalStream, _ := obj["stream"].(bool)
	rewriteMessages(obj)
	normalizeToolChoice(obj)
	normalizeTools(obj)
	model := strings.TrimSpace(asString(obj["model"]))
	if model == "" {
		model = profile.DefaultModel
	}
	solo := map[string]any{
		"model":       model,
		"config_name": model,
		"function":    soloChatFunction,
		"stream":      true,
		"request_id":  randomUUID(),
		"session_id":  randomUUID(),
	}
	if messages, ok := obj["messages"]; ok {
		solo["messages"] = messages
	}
	if n, ok := positiveTokenLimit(obj["max_tokens"]); ok {
		solo["max_tokens"] = n
	} else if n, ok := positiveTokenLimit(obj["max_completion_tokens"]); ok {
		solo["max_tokens"] = n
	}
	if temp, ok := obj["temperature"].(float64); ok {
		solo["temperature"] = temp
	}
	if tools, ok := obj["tools"]; ok {
		solo["tools"] = tools
	}
	if choice, ok := obj["tool_choice"]; ok {
		solo["tool_choice"] = choice
	}
	if parallel, ok := obj["parallel_tool_calls"].(bool); ok {
		if _, hasTools := solo["tools"]; hasTools {
			solo["parallel_tool_calls"] = parallel
		}
	}
	return solo, model, originalStream
}

func positiveTokenLimit(raw any) (int, bool) {
	number, ok := raw.(float64)
	if !ok || number <= 0 || number >= 1e9 {
		return 0, false
	}
	return int(number), true
}

func randomUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return randomHex(16)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

// wrongFunctionCode 报告「该 function 的目录不含 config_name」的业务码。
// 1005 是套餐额度拒绝，不在此列。
func wrongFunctionCode(code string) bool {
	switch strings.TrimSpace(code) {
	case "4001", "4023":
		return true
	default:
		return false
	}
}

// errorTextByCode 把已知的流内错误码翻成可读文案。代理服务按文案关键词
// 做冷却分类（额度耗尽要小时级冷却），上游不一定给中文。
var errorTextByCode = map[int64]string{
	1005: "套餐额度不足",
	1001: "认证已失效，请重新登录",
	4017: "设备风控，请重新登录",
	4001: "参数或模型名无效",
}

// annotateErrorCode 在错误文案里补充已知错误码的中文描述。
func annotateErrorCode(code, detail string) string {
	number, err := strconv.ParseInt(strings.TrimSpace(code), 10, 64)
	if err != nil {
		return detail
	}
	if text, ok := errorTextByCode[number]; ok {
		if strings.TrimSpace(detail) == "" {
			return text
		}
		return text + "：" + detail
	}
	return detail
}

// consumeFunctionRejection 只读取 200 响应的开头，区分「成员拒绝」（JSON
// 信封或前导错误事件，尚无输出）与真实回答。
//
// 返回 (reject, head, jsonMode, err)：head 是已从 body 消费掉的字节，调用方
// 必须负责拼接（finalizeResponse 用 prefixBody 恰好拼一次）。这里不再把字节
// 缝回 body——上层若再对 body 做形态预读，会把同一段前缀重复送进翻译器。
func consumeFunctionRejection(resp *http.Response) (reject bool, head []byte, jsonMode bool, err error) {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		return false, nil, false, nil
	}
	var buf bytes.Buffer
	tmp := make([]byte, 2048)
	for buf.Len() < 64<<10 {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		eof := rerr == io.EOF
		if buf.Len() > 0 {
			decided, isReject, isJSON := classifyFunctionPrefix(buf.Bytes(), eof)
			if decided {
				return isReject, append([]byte(nil), buf.Bytes()...), isJSON, nil
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				return false, nil, false, rerr
			}
			return false, append([]byte(nil), buf.Bytes()...), false, nil
		}
		if n == 0 {
			// 不报告进度的 reader 不能让循环空转。
			return false, append([]byte(nil), buf.Bytes()...), jsonShaped(buf.Bytes()), nil
		}
	}
	return false, append([]byte(nil), buf.Bytes()...), jsonShaped(buf.Bytes()), nil
}

// jsonShaped 报告一段未决开头是否为 JSON 形态（决定透传分支）。
func jsonShaped(raw []byte) bool {
	return strings.HasPrefix(strings.TrimLeft(string(raw), " \t\r\n"), "{")
}

// classifyFunctionPrefix 判定已读开头：是否可判定、是否成员拒绝、
// 是否 JSON 形态（决定 finalizeResponse 的透传分支）。
func classifyFunctionPrefix(raw []byte, eof bool) (decided, reject, jsonMode bool) {
	text := strings.TrimLeft(string(raw), " \t\r\n")
	if text == "" {
		return eof, false, false
	}
	if text[0] == '{' {
		if !json.Valid([]byte(text)) {
			return eof, false, true
		}
		var envelope struct {
			Code any `json:"code"`
		}
		if json.Unmarshal([]byte(text), &envelope) != nil {
			return true, false, true
		}
		return true, wrongFunctionCode(anyTrim(envelope.Code)), true
	}
	if !strings.HasPrefix(text, "event:") && !strings.HasPrefix(text, "data:") && !strings.HasPrefix(text, "id:") {
		return true, false, false
	}
	if !strings.Contains(text, "\n\n") && !eof {
		return false, false, false
	}
	scanner := &sseScanner{}
	events := scanner.step([]byte(text))
	if eof {
		events = append(events, scanner.close()...)
	}
	if len(events) == 0 {
		return eof, false, false
	}
	for _, event := range events {
		if event == nil {
			continue
		}
		switch event.kind {
		case "error":
			return true, wrongFunctionCode(fmt.Sprintf("%d", event.errorCode)), false
		case "output", "done":
			return true, false, false
		}
	}
	// metadata 和 token_usage 可能先于错误事件出现。在这里判定会把池钉死
	// 在主机表态之前。
	return eof, false, false
}

func asString(raw any) string {
	if text, ok := raw.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// rewriteMessages 把字符串内容转成 SOLO 期望的 text chip 数组，并把
// OpenAI 的 assistant tool_calls 换成 function_call 形态。
func rewriteMessages(obj map[string]any) {
	messages, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role == "assistant" {
			rewriteAssistantToolCalls(message)
		}
		switch content := message["content"].(type) {
		case string:
			message["content"] = []any{map[string]any{"type": "text", "text": content}}
		}
	}
}

func rewriteAssistantToolCalls(message map[string]any) {
	calls, ok := message["tool_calls"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(calls))
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := call["function"].(map[string]any); ok {
			if _, hasCall := call["function_call"]; !hasCall {
				call["function_call"] = fn
				delete(call, "function")
			}
		}
		if fc, ok := call["function_call"].(map[string]any); ok {
			if strings.TrimSpace(asString(fc["name"])) == "" {
				continue
			}
		}
		kept = append(kept, call)
	}
	if len(kept) == 0 {
		delete(message, "tool_calls")
		return
	}
	message["tool_calls"] = kept
}

// normalizeToolChoice 把 OpenAI 对象形态的 tool_choice 拍平成 SOLO 接受的
// 字符串；"none" 直接去掉工具。
func normalizeToolChoice(obj map[string]any) {
	raw, present := obj["tool_choice"]
	if !present {
		return
	}
	suppress := func() {
		delete(obj, "tool_choice")
		delete(obj, "tools")
		delete(obj, "functions")
	}
	switch choice := raw.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(choice), "none") {
			suppress()
		}
	case map[string]any:
		kind := strings.ToLower(strings.TrimSpace(asString(choice["type"])))
		switch kind {
		case "none":
			suppress()
		case "auto", "required":
			obj["tool_choice"] = kind
		case "function":
			name := ""
			if fn, ok := choice["function"].(map[string]any); ok {
				name = asString(fn["name"])
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeTools 适配 OpenAI 工具定义：SOLO 要求 function.parameters 里的
// JSON schema 序列化成字符串。
func normalizeTools(obj map[string]any) {
	rawTools, present := obj["tools"]
	if !present {
		return
	}
	list, ok := rawTools.([]any)
	if !ok || len(list) == 0 {
		delete(obj, "tools")
		return
	}
	out := make([]any, 0, len(list))
	for _, rawItem := range list {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := item["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if serialized, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(serialized)
				}
			}
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}

func upstreamFailure(resp *http.Response, message string) *http.Response {
	return upstreamFailureWithStatus(resp, http.StatusBadGateway, message)
}

// envelopeStatus 把 SOLO 的业务码映射成 HTTP 语义，供服务层的失败分类
// （冷却时长/刷新重试）做出正确反应。
func envelopeStatus(code string) int {
	switch strings.TrimSpace(code) {
	case "1005": // 套餐额度不足 → 额度耗尽冷却
		return http.StatusPaymentRequired
	case "1001": // 认证失效 → 服务层会先刷新令牌再重试
		return http.StatusUnauthorized
	case "4017": // 设备风控 → 认证失败处理
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}

func upstreamFailureWithStatus(resp *http.Response, status int, message string) *http.Response {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "upstream_error"},
	})
	if err != nil {
		payload = []byte(`{"error":{"message":"upstream error"}}`)
	}
	resp.StatusCode = status
	resp.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	return replaceBody(resp, payload, "application/json")
}

func replaceBody(resp *http.Response, payload []byte, contentType string) *http.Response {
	resp.Body = io.NopCloser(bytes.NewReader(payload))
	resp.ContentLength = int64(len(payload))
	resp.Header.Del("Content-Encoding")
	if contentType != "" {
		resp.Header.Set("Content-Type", contentType)
	}
	return resp
}

// ─────────────────────────────────────────────────────────────────────────────
// SOLO 事件流 → OpenAI chunk
// ─────────────────────────────────────────────────────────────────────────────

// soloEvent 是一个上游事件（event: + data: 对）。
type soloEvent struct {
	kind         string
	response     string
	reasoning    string
	toolCalls    json.RawMessage
	usage        map[string]any
	finishReason string
	errorCode    int64
	errorMessage string
}

func parseSoloLine(eventName, dataLine string) (*soloEvent, error) {
	event := &soloEvent{kind: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return event, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return nil, err
	}
	switch event.kind {
	case "output":
		event.response, _ = raw["response"].(string)
		event.reasoning, _ = raw["reasoning_content"].(string)
		if value, ok := raw["tool_calls"]; ok && value != nil {
			event.toolCalls, _ = json.Marshal(value)
		}
	case "token_usage":
		event.usage = raw
	case "done":
		event.finishReason, _ = raw["finish_reason"].(string)
	case "error":
		// code 可能是 JSON 数字或字符串；留在字符串会丢掉 4001。
		if n, err := strconv.ParseInt(anyTrim(raw["code"]), 10, 64); err == nil {
			event.errorCode = n
		}
		event.errorMessage = strings.TrimSpace(asString(raw["message"]))
		if event.errorMessage == "" {
			event.errorMessage = strings.TrimSpace(asString(raw["msg"]))
		}
	}
	return event, nil
}

// sseScanState 跨行累积一个 event:/data: 对。
type sseScanState struct {
	event string
	data  strings.Builder
}

func (s *sseScanState) line(line string) *soloEvent {
	switch {
	case line == "":
		if s.event == "" {
			s.data.Reset()
			return nil
		}
		if s.data.Len() == 0 {
			// chunk 边界可能把 "event:X" 与它的 data 行切开；保留事件名等
			// data，而不是发出空事件。
			return nil
		}
		event, err := parseSoloLine(s.event, s.data.String())
		s.event = ""
		s.data.Reset()
		if err != nil {
			return nil
		}
		return event
	case strings.HasPrefix(line, "event:"):
		s.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		s.data.WriteString(strings.TrimPrefix(line, "data:"))
	}
	return nil
}

// sseScanner 跨 Read 块增量解析上游流。
type sseScanner struct {
	state sseScanState
	head  strings.Builder
}

func (s *sseScanner) step(chunk []byte) []*soloEvent {
	s.head.WriteString(string(chunk))
	text := s.head.String()
	s.head.Reset()
	if !strings.Contains(text, "\n") {
		s.head.WriteString(text)
		return nil
	}
	var events []*soloEvent
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		last := i == len(lines)-1
		switch {
		case last && line == "":
			// 源以 \n 结束：冲刷未决的事件边界。
			if event := s.state.line(""); event != nil {
				events = append(events, event)
			}
		case last:
			// 不完整的尾行：留待下一个块。
			s.head.WriteString(line)
		default:
			if event := s.state.line(strings.TrimRight(line, "\r\n")); event != nil {
				events = append(events, event)
			}
		}
	}
	return events
}

func (s *sseScanner) close() []*soloEvent {
	var events []*soloEvent
	if rest := s.head.String(); strings.TrimSpace(rest) != "" {
		s.head.Reset()
		if event := s.state.line(strings.TrimRight(rest, "\r\n")); event != nil {
			events = append(events, event)
		}
	}
	if event := s.state.line(""); event != nil {
		events = append(events, event)
	}
	return events
}

// chunkWriter 产出 OpenAI chat.completion.chunk 帧。
type chunkWriter struct {
	w       *bufio.Writer
	id      string
	model   string
	created int64
}

func (c *chunkWriter) writeChunk(delta map[string]any, finish string, usage map[string]any) error {
	chunk := map[string]any{
		"id":      c.id,
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": delta,
		}},
	}
	choice := chunk["choices"].([]any)[0].(map[string]any)
	if finish != "" {
		choice["finish_reason"] = finish
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := c.w.WriteString("data: " + string(raw) + "\n\n"); err != nil {
		return err
	}
	// SSE chunk 必须随产随达：只在循环后 flush 会丢掉终局分支的帧。
	c.flush()
	return nil
}

func (c *chunkWriter) flush() {
	_ = c.w.Flush()
}

func (c *chunkWriter) writeDone() error {
	_, err := c.w.WriteString("data: [DONE]\n\n")
	if err != nil {
		return err
	}
	c.flush()
	return nil
}

// writeErrorFrame 在流里输出一个 OpenAI 风格的错误对象，
// 让上层能按文案分类失败，而不是看到空回答。
func writeErrorFrame(w *bufio.Writer, message string) {
	payload, err := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}})
	if err != nil {
		return
	}
	_, _ = w.WriteString("data: " + string(payload) + "\n\n")
}

var chunkIDCounter atomic.Int64

func compactID() string {
	return fmt.Sprintf("chatcmpl-trae-%d-%d", time.Now().UnixNano(), chunkIDCounter.Add(1))
}

// translateSSE 把上游事件流实时翻译为 OpenAI chunk 流。
func translateSSE(ctx context.Context, upstream io.Reader, sink io.Writer, model string) error {
	scanner := &sseScanner{}
	writer := &chunkWriter{
		w:       bufio.NewWriterSize(sink, 32*1024),
		id:      compactID(),
		model:   model,
		created: time.Now().Unix(),
	}
	var pendingUsage map[string]any
	sawDone := false
	stopped := false
	handle := func(event soloEvent) bool {
		switch event.kind {
		case "output":
			delta := map[string]any{}
			if event.response != "" {
				delta["content"] = event.response
			}
			if event.reasoning != "" {
				delta["reasoning_content"] = event.reasoning
			}
			if len(event.toolCalls) > 0 && string(event.toolCalls) != "null" {
				var calls []*toolCallFrame
				if json.Unmarshal(event.toolCalls, &calls) == nil {
					deltas := make([]map[string]any, 0, len(calls))
					for _, call := range calls {
						if call == nil {
							continue
						}
						deltas = append(deltas, call.norm())
					}
					if len(deltas) > 0 {
						delta["tool_calls"] = deltas
					}
				}
			}
			if len(delta) > 0 {
				if writer.writeChunk(delta, "", nil) != nil {
					stopped = true
					return false
				}
			}
		case "token_usage":
			if usage := normalizeUsage(event.usage); len(usage) > 0 {
				pendingUsage = usage
			}
		case "done":
			if writer.writeChunk(map[string]any{}, orStop(event.finishReason), pendingUsage) != nil {
				stopped = true
				return false
			}
			sawDone = true
			return false
		case "error":
			message := fmt.Sprintf("Trae 上游错误 code=%d: %s", event.errorCode,
				annotateErrorCode(strconv.FormatInt(event.errorCode, 10), event.errorMessage))
			_ = writer.writeChunk(map[string]any{}, "", nil)
			writeErrorFrame(writer.w, message)
			sawDone = true
			return false
		}
		return true
	}
	serve := func(events []*soloEvent) bool {
		for _, event := range events {
			if event == nil {
				continue
			}
			if !handle(*event) {
				return false
			}
		}
		return true
	}
	reader := bufio.NewReaderSize(upstream, 32*1024)
	buf := make([]byte, 16*1024)
	for !stopped && !sawDone {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if !serve(scanner.step(buf[:n])) {
				break
			}
		}
		if readErr != nil {
			break
		}
		select {
		case <-ctx.Done():
			stopped = true
		default:
		}
	}
	// 每个终局分支都以 [DONE] 收尾。
	if sawDone {
		return writer.writeDone()
	}
	if !serve(scanner.close()) {
		return writer.writeDone()
	}
	finish := "stop"
	if ctx.Err() != nil {
		finish = "abort"
	}
	_ = writer.writeChunk(map[string]any{}, finish, pendingUsage)
	return writer.writeDone()
}

// toolCallFrame 把一条上游 tool_call 映射到 OpenAI delta。
type toolCallFrame map[string]any

func (f toolCallFrame) norm() map[string]any {
	if f == nil {
		return map[string]any{}
	}
	if fc, ok := f["function_call"].(map[string]any); ok {
		f["function"] = fc
		delete(f, "function_call")
	}
	if fn, ok := f["function"].(map[string]any); ok {
		delete(fn, "namespace")
		delete(fn, "partial_arguments")
	}
	return map[string]any(f)
}

// normalizeUsage 只保留能映射到 OpenAI usage 的字段。
func normalizeUsage(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "reasoning_tokens"} {
		if v, ok := raw[key]; ok {
			out[key] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func orStop(finish string) string {
	if strings.TrimSpace(finish) == "" {
		return "stop"
	}
	return strings.TrimSpace(finish)
}
