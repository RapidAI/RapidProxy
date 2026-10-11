// /algo 流式线路（签名信封）。请求体是普通 OpenAI 形状的 JSON 对象，由
// wasm 编码；响应是 SSE 流，其 data 行包裹标准 OpenAI chunk：
//
//	data:{"headers":{…},"body":"{\"choices\":[{\"delta\":{\"content\":…}}]}"}
//	data:{"body":"{\"choices\":[{…,\"finish_reason\":\"stop\"}]}"}
//	data:[DONE]
//	data:{"firstTokenDuration":…}   ← 遥测尾巴，忽略
package qoder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/znsoftm/RapidProxy/internal/upstream/qoder/cosy"
)

// Chat 把 OpenAI 形状的请求体翻译成签名的 /algo agent_chat_generation 请求，
// 并把上游 SSE 信封实时解回标准 OpenAI chunk 流。
//
// 返回的响应在 HTTP 200 时永远是 text/event-stream；HTTP 层错误原样透传，
// 供调用方按状态码分类。uid 是签名材料里的用户标识（凭据里的 Account.UID）。
func Chat(ctx context.Context, hc *http.Client, profile Profile, uid, accessToken string, body []byte) (*http.Response, error) {
	signing, err := cosy.New(MachineID(), ClientVersion, strings.TrimSpace(uid))
	if err != nil {
		return nil, fmt.Errorf("Qoder 签名上下文初始化失败: %w", err)
	}
	defer signing.Close()
	if err := signing.RefreshAuthFields(strings.TrimSpace(accessToken)); err != nil {
		return nil, fmt.Errorf("Qoder 签名材料注入失败: %w", err)
	}

	// 单次解析同时产出线体与模型 key：编程 Agent 的载荷可达数 MB，
	// 不能为了取模型名再把序列化结果重新解析一遍。
	bodyJSON, model, _ := prepareAgentChatBody(body)
	prepared, err := signing.PrepareInferRequest(strings.TrimRight(profile.ChatOrigin, "/"), bodyJSON, model, "system")
	if err != nil {
		return nil, fmt.Errorf("Qoder 签名请求构建失败: %w", err)
	}

	outgoing, err := http.NewRequestWithContext(ctx, http.MethodPost, prepared.URL, strings.NewReader(prepared.Body))
	if err != nil {
		return nil, err
	}
	for key, value := range prepared.Headers {
		outgoing.Header.Set(key, value)
	}
	outgoing.Header.Set("Accept", "text/event-stream")
	outgoing.Header.Set("User-Agent", UserAgent)
	outgoing.ContentLength = int64(len(prepared.Body))
	outgoing.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(prepared.Body)), nil
	}

	resp, err := hc.Do(outgoing)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	// 实时解信封：错误在流内翻译成 OpenAI error 帧，换号/冷却逻辑照常生效。
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
		err := unwrapSSE(ctx, resp.Body, writer)
		_ = resp.Body.Close()
		_ = writer.CloseWithError(err)
	}()
	return &translated, nil
}

// prepareAgentChatBody 把 OpenAI chat 载荷合并进 /algo 线路体，返回
// （线体 JSON, 规范化后的模型 key, 调用方是否要求流式）。
func prepareAgentChatBody(payload []byte) (string, string, bool) {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		obj = map[string]any{}
	}
	originalStream, _ := obj["stream"].(bool)
	model := ""
	if text, _ := obj["model"].(string); strings.TrimSpace(text) != "" {
		model = canonicalQoderModelKey(text)
	}
	if model == "" {
		// 缺省模型同样要走 key 规范化：签名材料里的 X-Model-Key 必须是
		// 目录 key（qmodel_38max），不能是展示名（qwen3.8-max）。
		model = canonicalQoderModelKey(DefaultModel)
	}
	obj["model"] = model
	obj["stream"] = true
	// agent_chat_generation 节点期望的会话元数据。接受每次请求新生成的 id。
	if _, ok := obj["request_id"]; !ok {
		obj["request_id"] = newUUID()
	}
	if _, ok := obj["request_set_id"]; !ok {
		obj["request_set_id"] = newUUID()
	}
	if _, ok := obj["session_id"]; !ok {
		obj["session_id"] = newUUID()
	}
	obj["task_id"] = ""
	obj["business"] = map[string]any{"type": "agent", "scene": "cli"}
	obj["model_config"] = map[string]any{"key": model, "source": "system"}
	obj["data_policy_agreed"] = true
	out, err := marshalAgentChatJSON(obj)
	if err != nil {
		return string(payload), model, originalStream
	}
	return string(out), model, originalStream
}

// marshalAgentChatJSON 按 JSON.stringify 的方式输出 agent_chat 对象。
// encoding/json 默认的 HTML 转义会改写工具描述与提示词里的 '&'、'<'、'>'，
// 这会改变被签名的字节而不改变解析后的文档。
func marshalAgentChatJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 响应信封 → OpenAI SSE
// ─────────────────────────────────────────────────────────────────────────────

// unwrapSSE 把信封事件流实时解回普通 OpenAI chunk。
//
// 上游的失败帧（event:error → data:{stackTrace,…}）不携带 body 字段，会被
// 当作遥测尾巴静默略过——这样的流结束时一个 chunk 都没有产出。若就此收尾，
// 客户端会拿到 200 + 空内容（以为成功却什么都没发生）；因此在零产出的流
// 末尾补一个 OpenAI error 帧，让服务层的换号/冷却逻辑接管。
func unwrapSSE(ctx context.Context, upstream io.Reader, sink io.Writer) error {
	reader := bufio.NewReaderSize(upstream, 32*1024)
	writer := &envelopeWriter{w: sink}
	buf := make([]byte, 16*1024)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if !writer.ingest(string(buf[:n])) {
				break
			}
		}
		if readErr != nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if writer.forwarded == 0 {
		writer.writeErrorFrame("Qoder 未返回任何模型内容（上游流为空或只携带失败帧）")
		return nil
	}
	return nil
}

// envelopeWriter 缓存不完整行并转发翻译后的帧。
type envelopeWriter struct {
	w    io.Writer
	head strings.Builder
	// forwarded 是已向下游写出的帧数（内容 chunk 与 error 帧都算）。
	// 零产出的流说明上游失败帧被静默略过，收尾时要补错误帧。
	forwarded int
}

// ingest 消费一段上游 chunk；返回 false 表示终止（终局事件）。
func (e *envelopeWriter) ingest(chunk string) bool {
	e.head.WriteString(chunk)
	for {
		text := e.head.String()
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			return true
		}
		line := strings.TrimRight(text[:idx], "\r")
		e.head.Reset()
		e.head.WriteString(text[idx+1:])
		if !e.line(line) {
			return false
		}
	}
}

func (e *envelopeWriter) line(line string) bool {
	if !strings.HasPrefix(line, "data:") {
		// event: 行与空行不携带载荷；错误事件折进其后的 data 帧
		// （或末尾的普通 JSON）。
		return true
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" {
		return true
	}
	if payload == "[DONE]" {
		_, err := io.WriteString(e.w, "data: [DONE]\n\n")
		return err == nil
	}
	// 普通 JSON 失败尾巴（{"success":false,"msgCode":500,…}）在信封解析
	// 之前判别：它同样能解进 chatEnvelope（未知字段被忽略），先按空 body
	// 处理会把上游失败当成遥测尾巴静默吞掉。
	var failure struct {
		Message string `json:"message"`
		MsgInfo int    `json:"msgCode"`
	}
	if json.Unmarshal([]byte(payload), &failure) == nil && failure.MsgInfo != 0 {
		return e.writeErrorFrame(fmt.Sprintf("Qoder 上游错误 (HTTP %d): %s", failure.MsgInfo, failure.Message))
	}
	var envelope chatEnvelope
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return true
	}
	if envelope.Body == "" {
		// 遥测尾巴（firstTokenDuration…）不携带 chunk。
		return true
	}
	var chunk chatChunk
	if err := json.Unmarshal([]byte(envelope.Body), &chunk); err != nil {
		return true
	}
	if chunk.Code != "" {
		return e.writeErrorFrame(fmt.Sprintf("Qoder 上游错误 (%s): %s", chunk.Code, annotateQuotaSentinel(chunk.Message)))
	}
	if _, err := fmt.Fprintf(e.w, "data: %s\n\n", envelope.Body); err != nil {
		return false
	}
	e.forwarded++
	return true
}

func (e *envelopeWriter) writeErrorFrame(message string) bool {
	payload, err := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}})
	if err != nil {
		return true
	}
	_, _ = e.w.Write([]byte("data: " + string(payload) + "\n\n"))
	e.forwarded++
	return false
}

// annotateQuotaSentinel 补充额度哨兵文案，让代理层的冷却分类能命中
// 「额度」关键词（EXCEED_QUOTA / NOT_EXCEED_QUOTA 是上游的哨兵词）。
func annotateQuotaSentinel(message string) string {
	if strings.Contains(message, "EXCEED_QUOTA") {
		return message + "（额度已用尽）"
	}
	return message
}

type chatEnvelope struct {
	Headers json.RawMessage `json:"headers"`
	Body    string          `json:"body"`
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
	// 服务端错误 chunk（{code:"provider_error", …}）以 body 级 code/message
	// 对出现，而不是 choices 数组。
	Code    string `json:"code"`
	Message string `json:"message"`
}
