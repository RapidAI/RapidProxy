package hx

import (
	"regexp"
	"strings"
)

// secretValueRe 匹配 JSON 里键名带凭据意味的字符串键值对。
// 值部分容忍 \" 转义，避免转义值只被抹掉一半。
var secretValueRe = regexp.MustCompile(
	`(?i)("[^"]*(?:token|jwt|secret|authorization|api[_-]?key|credential|session[_-]?key)[^"]*"\s*:\s*)"((?:[^"\\]|\\.)*)"`)

// RedactTokens 把 JSON 文本中凭据键的值替换为占位符。
//
// 非 200 的上游响应体会被拼进错误文案，进而写进日志、出现在界面提示里。
// 错误响应正常不带凭据，但网关在半途出错时可能把请求/响应体（含 token）
// 原样回显，所以凡是准备进错误文案的响应体都应先过这里。
// 误伤（把普通同名键也抹掉）是可接受的：这些文本只用于展示。
func RedactTokens(text string) string {
	if text == "" || !strings.Contains(text, "\"") {
		return text
	}
	return secretValueRe.ReplaceAllString(text, `${1}"[redacted]"`)
}
