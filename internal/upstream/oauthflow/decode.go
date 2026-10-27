package oauthflow

import "encoding/json"

// decodeJSON 是 json.Unmarshal 的薄封装，集中处理「响应体不是预期 JSON」的
// 报错文案（各家 token 端点偶尔会回 HTML 错误页）。
func decodeJSON(raw []byte, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return &DecodeError{Raw: raw, Err: err}
	}
	return nil
}

// DecodeError 表示响应体无法解析成预期结构。
type DecodeError struct {
	Raw []byte
	Err error
}

func (e *DecodeError) Error() string {
	return "响应不是预期的 JSON: " + truncateForError(e.Raw) + " (" + e.Err.Error() + ")"
}

func (e *DecodeError) Unwrap() error { return e.Err }

// truncateForError 把响应体截断成可读的错误片段（按 rune 截断，避免把
// UTF-8 序列切坏成乱码）。
func truncateForError(raw []byte) string {
	const limit = 200
	runes := []rune(string(raw))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}
