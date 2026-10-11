package lobsterai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// streamFilterHead 限定过滤器在判定流首之前最多检查的字节数。上游错误帧
// 总是出现在流的最前部（实测远小于 200 字节），这个上限只是对病态流的防护。
const streamFilterHead = 512 * 1024

// newStreamErrorFilter 包装 SSE 响应体：流首的错误帧被替换成一个 OpenAI
// error chunk + [DONE]；其余流逐字节透传（判定期间已消费的字节先回放）。
// 业务错误帧只出现在 HTTP 200 流的首部（实测），其后内容在错误判定下被丢弃。
func newStreamErrorFilter(body io.ReadCloser) io.ReadCloser {
	return &streamErrorFilter{source: bufio.NewReaderSize(body, 16*1024), body: body}
}

type streamErrorFilter struct {
	source *bufio.Reader
	body   io.ReadCloser
	// head 存放判定期间消费的字节；透传情形下要先回放给调用方。
	head []byte
	// tail 存放合成的错误响应体（错误情形）；透传时为 nil。
	tail    []byte
	decided bool
}

func (f *streamErrorFilter) Read(p []byte) (int, error) {
	if !f.decided {
		if err := f.decide(); err != nil {
			return 0, err
		}
	}
	// 判定为错误：交付合成响应体，然后 EOF。真实数据源连同剩余内容直接关闭。
	if f.tail != nil {
		taken := copy(p, f.tail)
		f.tail = f.tail[taken:]
		if len(f.tail) == 0 {
			f.tail = nil
			f.closeSource()
		}
		return taken, nil
	}
	// 透传：先回放已消费的头部字节，再交给数据源。
	if len(f.head) > 0 {
		taken := copy(p, f.head)
		f.head = f.head[taken:]
		return taken, nil
	}
	return f.source.Read(p)
}

func (f *streamErrorFilter) Close() error {
	f.head = nil
	f.tail = nil
	return f.body.Close()
}

func (f *streamErrorFilter) closeSource() {
	_ = f.body.Close()
}

// decide 判定流首的帧。持续读取直到一条完整的 "data:" 行可以判定
// （错误对象切换到合成错误体；任何其它 JSON 帧确认透传）。SSE 注释行
// 本身不触发判定——真正的错误帧可能跟在后面——EOF 时按整个头部做终判。
func (f *streamErrorFilter) decide() error {
	f.decided = true
	buf := make([]byte, 16*1024)
	var head []byte
	// 检查 head 里已完整的 "data:" 行。1=发现错误帧，0=发现正常帧，-1=尚不可判定。
	survey := func() int {
		lines := strings.Split(string(head), "\n")
		for i := 0; i < len(lines)-1; i++ { // 最后一段可能还在增长
			text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "data:"))
			if text == "" || text == "[DONE]" {
				continue
			}
			var probe map[string]any
			if json.Unmarshal([]byte(text), &probe) != nil {
				continue // SSE event/注释行不是 JSON 帧
			}
			if frame := sseErrorFrame([]byte("data: " + text + "\n")); frame != "" {
				f.adoptError(frame)
				return 1
			}
			return 0
		}
		return -1
	}
	for len(head) < streamFilterHead {
		n, err := f.source.Read(buf)
		head = append(head, buf[:n]...)
		if verdict := survey(); verdict >= 0 {
			if verdict == 1 {
				return nil // adoptError 已重接流
			}
			f.head = append(f.head, head...)
			return nil
		}
		if err != nil {
			// EOF：整个头部是最终判词（孤立的错误帧即使没有结尾空行也要翻译）。
			if frame := sseErrorFrame(head); frame != "" {
				f.adoptError(frame)
				return nil
			}
			f.head = append(f.head, head...)
			return nil
		}
	}
	if frame := sseErrorFrame(head); frame != "" {
		f.adoptError(frame)
		return nil
	}
	f.head = append(f.head, head...)
	return nil
}

// adoptError 把流替换为合成的 OpenAI 错误响应体。
func (f *streamErrorFilter) adoptError(frame string) {
	f.head = nil
	payload, err := json.Marshal(map[string]any{"error": map[string]any{"message": frame, "type": "upstream_error"}})
	if err != nil {
		payload = []byte(`{"error":{"message":"upstream error","type":"upstream_error"}}`)
	}
	f.tail = []byte("data: " + string(payload) + "\n\ndata: [DONE]\n\n")
}

// sseErrorFrame 从流首的 {"error":{...}} SSE 数据帧提取可读文案。
// 业务拒绝（额度、未知模型）藏在 HTTP 200 里。
func sseErrorFrame(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(text, "{") {
			continue
		}
		var chunk struct {
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    int64  `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(text), &chunk) == nil && chunk.Error != nil {
			detail := chunk.Error.Message
			if detail == "" {
				detail = chunk.Error.Type
			}
			if chunk.Error.Code != 0 {
				detail = fmt.Sprintf("code=%d %s", chunk.Error.Code, detail)
			}
			return fmt.Sprintf("LobsterAI 上游错误: %s", annotateErrorFrame(detail, chunk.Error.Code))
		}
	}
	return ""
}
