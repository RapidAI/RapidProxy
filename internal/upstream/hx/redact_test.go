package hx

import (
	"strings"
	"testing"
)

// 响应体会被拼进错误文案并写进日志：凭据键的值必须整段抹掉，
// 非 JSON 文本与普通键保持原样。
func TestRedactTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "常见键名",
			in:   `{"error":"boom","accessToken":"sk-live-1","refresh_token":"rt-2"}`,
			want: `{"error":"boom","accessToken":"[redacted]","refresh_token":"[redacted]"}`,
		},
		{
			name: "大小写不敏感",
			in:   `{"JWT":"a.b.c","Api_Key":"k"}`,
			want: `{"JWT":"[redacted]","Api_Key":"[redacted]"}`,
		},
		{
			name: "嵌套对象里的键",
			in:   `{"Result":{"data":{"sessionKey":"abc"},"msg":"ok"}}`,
			want: `{"Result":{"data":{"sessionKey":"[redacted]"},"msg":"ok"}}`,
		},
		{
			name: "转义值整段抹掉",
			in:   `{"token":"a\"bcd","ok":1}`,
			want: `{"token":"[redacted]","ok":1}`,
		},
		{
			name: "普通键不动",
			in:   `{"message":"配额不足","code":402,"model":"glm-5.2"}`,
			want: `{"message":"配额不足","code":402,"model":"glm-5.2"}`,
		},
		{
			name: "非 JSON 文本原样返回",
			in:   `<html>gateway error</html>`,
			want: `<html>gateway error</html>`,
		},
		{
			name: "空串",
			in:   ``,
			want: ``,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactTokens(tc.in); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// 值里出现特殊替换字符（$ 等）时不能破坏输出。
func TestRedactTokensDollarInValue(t *testing.T) {
	got := RedactTokens(`{"token":"a$b$1"}`)
	if strings.Contains(got, "$") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("含 $ 的值应被整体抹掉: %s", got)
	}
}
