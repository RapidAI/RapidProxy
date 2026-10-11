package lobsterai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Chat 把 OpenAI 形状的请求体转发到 LobsterAI 对话端点。
//
// 上游只支持流式（stream:false 返回 HTTP 500），因此这里强制 stream:true；
// 非 流式调用由代理服务层聚合。返回的响应体是标准 OpenAI SSE——业务错误
// （额度、未知模型等藏在 HTTP 200 流首）已经由 streamErrorFilter 翻译成
// OpenAI error 帧。HTTP 层错误原样透传，供调用方按状态码分类。
func Chat(ctx context.Context, hc *http.Client, accessToken string, body []byte) (*http.Response, error) {
	payload := prepareBody(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, APIBase+ChatPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if key := strings.TrimSpace(accessToken); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("X-LobsterAI-Client-Capabilities", ClientCapabilities)
	req.Header.Set("X-LobsterAI-Client-Version", ClientVersion)
	req.Header.Set("User-Agent", UserAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		// 流首错误帧过滤：错误被替换成一个 OpenAI error chunk + [DONE]，
		// 正常流逐字节透传。
		resp.Body = newStreamErrorFilter(resp.Body)
	}
	return resp, nil
}

// prepareBody 强制流式并规范 tool_choice。请求体本来就是 OpenAI 形状，
// 其它字段原样透传。
func prepareBody(payload []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		return payload
	}
	obj["stream"] = true
	if choice, ok := obj["tool_choice"]; ok {
		switch v := choice.(type) {
		case string:
			if strings.TrimSpace(v) == "" || strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
			}
		case nil:
			delete(obj, "tool_choice")
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// quotaCodes 是上游业务错误码里与额度/模型可用性相关的已知值。
// 代理服务靠错误文案关键词做冷却分类（额度耗尽要长冷却），
// 上游不一定带中文文案，这里补上可识别的描述。
var quotaCodes = map[int64]string{
	40201: "额度已用尽",
	40202: "额度已用尽",
	40300: "模型不可用",
}

// annotateErrorFrame 在错误帧文案里补充已知错误码的中文描述，
// 让代理层的冷却分类（额度/限流）能命中关键词。
func annotateErrorFrame(detail string, code int64) string {
	if text, ok := quotaCodes[code]; ok {
		return fmt.Sprintf("%s（%s）", detail, text)
	}
	return detail
}
