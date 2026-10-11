package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCanonicalQoderModelKey(t *testing.T) {
	cases := map[string]string{
		"qwen3.8-max":   "qmodel_38max",
		"Qwen 3.8 Max":  "qmodel_38max",
		"qwen3.8-flash": "qfmodel",
		"qmodel_38max":  "qmodel_38max",
		"unknown-model": "unknown-model",
	}
	for in, want := range cases {
		if got := canonicalQoderModelKey(in); got != want {
			t.Errorf("canonicalQoderModelKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrepareAgentChatBody(t *testing.T) {
	out, model, _ := prepareAgentChatBody([]byte(`{"model":"qwen3.8-max","stream":true,"messages":[{"role":"user","content":"hi & <bye>"}]}`))
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatal(err)
	}
	if model != "qmodel_38max" {
		t.Errorf("返回的模型 key 应规范化: %q", model)
	}
	if obj["model"] != "qmodel_38max" {
		t.Errorf("模型 key 应规范化: %v", obj["model"])
	}
	if obj["stream"] != true {
		t.Error("stream 应强制为 true")
	}
	if obj["task_id"] != "" {
		t.Error("task_id 应为空串")
	}
	biz, _ := obj["business"].(map[string]any)
	if biz["type"] != "agent" || biz["scene"] != "cli" {
		t.Errorf("business 元数据不对: %v", biz)
	}
	mc, _ := obj["model_config"].(map[string]any)
	if mc["key"] != "qmodel_38max" || mc["source"] != "system" {
		t.Errorf("model_config 不对: %v", mc)
	}
	if obj["data_policy_agreed"] != true {
		t.Error("data_policy_agreed 应为 true")
	}
	for _, key := range []string{"request_id", "request_set_id", "session_id"} {
		if v, _ := obj[key].(string); v == "" {
			t.Errorf("%s 应自动生成", key)
		}
	}
	// HTML 转义会改变被签名的字节：必须保持字面量。
	if !strings.Contains(out, "hi & <bye>") {
		t.Errorf("输出不应做 HTML 转义: %s", out)
	}
	// 缺省模型回退 CLI 兜底 key。
	_, defModel, _ := prepareAgentChatBody([]byte(`{"stream":true,"messages":[]}`))
	if defModel != "qmodel_38max" {
		t.Errorf("缺省模型应回退 %s，实际 %q", DefaultModel, defModel)
	}
	// 重复调用应复用已生成的会话 id（幂等）。
	out2, _, _ := prepareAgentChatBody([]byte(out))
	var obj2 map[string]any
	_ = json.Unmarshal([]byte(out2), &obj2)
	if obj2["session_id"] != obj["session_id"] {
		t.Error("再次准备不应覆盖已有会话 id")
	}
}

func TestEnvelopeUnwrapStream(t *testing.T) {
	inner1 := `{"choices":[{"delta":{"content":"你"}}]}`
	inner2 := `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`
	upstream := "data: " + envelopeJSON(inner1) + "\n\n" +
		"data: " + envelopeJSON(inner2) + "\n\n" +
		"data: [DONE]\n\n"
	var out strings.Builder
	if err := unwrapSSE(context.Background(), strings.NewReader(upstream), &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, `"content":"你"`) {
		t.Fatalf("内层 chunk 应被解出: %s", text)
	}
	if strings.Contains(text, "headers") {
		t.Fatalf("信封外壳不应透传: %s", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("应以 [DONE] 收尾: %s", text)
	}
}

// envelopeJSON 构造一行信封：body 字段是内层 chunk 的字符串化。
func envelopeJSON(inner string) string {
	env := map[string]any{"headers": map[string]any{}, "body": inner}
	raw, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func TestEnvelopeErrorFrames(t *testing.T) {
	// body 级错误 chunk → OpenAI error 帧；额度哨兵要补充「额度」关键词。
	upstream := "data: " + innerJSON(map[string]any{"code": "provider_error", "message": "rejected"}) + "\n\n"
	var out strings.Builder
	if err := unwrapSSE(context.Background(), strings.NewReader(upstream), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "provider_error") {
		t.Fatalf("错误应译成 error 帧: %s", out.String())
	}

	upstream2 := "data: " + innerJSON(map[string]any{"code": "x", "message": "EXCEED_QUOTA"}) + "\n\n"
	var out2 strings.Builder
	_ = unwrapSSE(context.Background(), strings.NewReader(upstream2), &out2)
	if !strings.Contains(out2.String(), "额度已用尽") {
		t.Fatalf("额度哨兵应补充中文文案: %s", out2.String())
	}

	// 普通 JSON 失败尾巴（msgCode 非 0）同样是错误。
	upstream3 := "data: {\"success\":false,\"msgCode\":500,\"message\":\"boom\"}\n\n"
	var out3 strings.Builder
	_ = unwrapSSE(context.Background(), strings.NewReader(upstream3), &out3)
	if !strings.Contains(out3.String(), "boom") {
		t.Fatalf("失败尾巴应译成 error 帧: %s", out3.String())
	}

	// 遥测尾巴本身不转发，但零产出的流收尾必须补 error 帧：
	// 否则客户端拿到 200 + 空内容，把上游失败当成成功。
	upstream4 := "data: {\"firstTokenDuration\":12}\n\n"
	var out4 strings.Builder
	_ = unwrapSSE(context.Background(), strings.NewReader(upstream4), &out4)
	if !strings.Contains(out4.String(), "未返回任何模型内容") {
		t.Fatalf("零产出流应收尾补 error 帧: %q", out4.String())
	}

	// 正常产出的流不受影响：不补额外帧。
	upstream5 := "data: " + envelopeJSON(`{"choices":[{"delta":{"content":"hi"}}]}`) + "\n\n" +
		"data: {\"firstTokenDuration\":12}\n\n" +
		"data: [DONE]\n\n"
	var out5 strings.Builder
	_ = unwrapSSE(context.Background(), strings.NewReader(upstream5), &out5)
	if strings.Count(out5.String(), "data:") != 2 { // 一个 chunk + [DONE]
		t.Fatalf("正常流不应附加错误帧: %q", out5.String())
	}
}

func innerJSON(v any) string {
	raw, err := json.Marshal(map[string]any{"body": mustJSONString(v)})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func mustJSONString(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func TestPkceVerifierShape(t *testing.T) {
	verifier, err := pkceVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(verifier); n < 43 || n > 128 {
		t.Fatalf("verifier 长度应在 43-128 之间: %d", n)
	}
	for _, r := range verifier {
		if !strings.ContainsRune(pkceAlphabet, r) {
			t.Fatalf("verifier 字符超出 RFC7636 字母表: %q", r)
		}
	}
	// challenge 是 base64url 的 S256。
	if ch := pkceChallenge(verifier); strings.Contains(ch, "=") {
		t.Fatalf("challenge 不应带 padding: %s", ch)
	}
}

func TestDeviceLoginAuthURLOrder(t *testing.T) {
	login, err := StartLogin("machine-1")
	if err != nil {
		t.Fatal(err)
	}
	urlText := login.AuthURL(CNProfile())
	if !strings.HasPrefix(urlText, "https://qoder.cn/device/selectAccounts?") {
		t.Fatalf("授权页 URL 不对: %s", urlText)
	}
	// 官方 CLI 固定参数顺序：challenge 在前，client_id 在最后。
	idxChallenge := strings.Index(urlText, "challenge=")
	idxNonce := strings.Index(urlText, "nonce=")
	idxMachine := strings.Index(urlText, "machine_id=")
	idxClient := strings.Index(urlText, "client_id=")
	if !(idxChallenge < idxNonce && idxNonce < idxMachine && idxMachine < idxClient) {
		t.Fatalf("参数顺序与 CLI 不一致: %s", urlText)
	}
}

func TestPollPendingOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	profile := CNProfile()
	profile.OpenAPIBase = srv.URL
	login, _ := StartLogin("m")
	_, err := Poll(context.Background(), http.DefaultClient, profile, login)
	if err == nil || !strings.Contains(err.Error(), "尚未批准") {
		t.Fatalf("404 应返回等待中: %v", err)
	}
}

func TestPollParsesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"dev-token","refresh_token":"ref-1","user_id":"u1","user_name":"n1","expires_at":4102444800}`)
	}))
	defer srv.Close()
	profile := CNProfile()
	profile.OpenAPIBase = srv.URL
	login, _ := StartLogin("m")
	token, err := Poll(context.Background(), http.DefaultClient, profile, login)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "dev-token" || token.RefreshToken != "ref-1" || token.UserID != "u1" {
		t.Fatalf("token 解析不对: %+v", token)
	}
	if token.ExpiresAt != 4102444800 {
		t.Fatalf("绝对过期时间应直接采用: %d", token.ExpiresAt)
	}
}

func TestChatSignsAndUnwraps(t *testing.T) {
	// 本地服务无法校验 COSY 签名本身，但能确认：请求打到 prepared.URL
	//（wasm 生成的 /algo 路径），响应信封被解回标准 OpenAI 流。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/algo/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		inner := `{"choices":[{"delta":{"content":"hi"}}]}`
		env, _ := json.Marshal(map[string]any{"headers": map[string]any{}, "body": inner})
		_, _ = io.WriteString(w, "data: "+string(env)+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	profile := GlobalProfile()
	profile.ChatOrigin = srv.URL
	resp, err := Chat(context.Background(), srv.Client(), profile, "uid-1", "device-token", []byte(`{"model":"qwen3.8-max","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if !strings.Contains(text, `"content":"hi"`) {
		t.Fatalf("信封应被解回 OpenAI 流: %s", text)
	}
	if strings.Contains(text, "headers") {
		t.Fatalf("外壳不应透传: %s", text)
	}
}

// 传输层抖动（打不通目标，状态码 0）按「继续等待」处理：
// 一次网络抖动不应毁掉用户已打开的授权页（对齐官方 CLI 行为）。
func TestPollToleratesTransportBlip(t *testing.T) {
	profile := CNProfile()
	profile.OpenAPIBase = "http://127.0.0.1:1" // 必然连接失败
	login, _ := StartLogin("m")
	if _, err := Poll(context.Background(), http.DefaultClient, profile, login); !errors.Is(err, ErrPending) {
		t.Fatalf("传输失败应按等待中处理，实际: %v", err)
	}
}
