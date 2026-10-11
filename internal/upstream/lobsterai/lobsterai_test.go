package lobsterai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newStubServer 起一个返回固定状态码与正文的本地服务。
func newStubServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBuildLoginURLCarriesStateAndRedirect(t *testing.T) {
	url := BuildLoginURL("http://127.0.0.1:19090/auth/callback", "abc123")
	if !strings.HasPrefix(url, PortalBase+LoginPath) {
		t.Fatalf("登录链接前缀不对: %s", url)
	}
	if !strings.Contains(url, "redirect_uri=http%3A%2F%2F127.0.0.1%3A19090%2Fauth%2Fcallback") {
		t.Errorf("redirect_uri 未编码进链接: %s", url)
	}
	if !strings.Contains(url, "state=abc123") {
		t.Errorf("state 未编码进链接: %s", url)
	}
}

func TestLoginSessionWaitAndCancel(t *testing.T) {
	session := &LoginSession{State: "s1", notify: make(chan struct{})}
	go session.Cancel()
	if _, err := session.Wait(context.Background()); err == nil {
		t.Fatal("取消后的 Wait 应返回错误")
	}
	// done 之后重复 Cancel 不应 panic。
	session.Cancel()
}

func TestLoginSessionStateMismatchRejected(t *testing.T) {
	session := &LoginSession{State: "expected", notify: make(chan struct{})}
	session.deliver("code-x", "回调与本机发起的登录不匹配（state 校验失败），已拒绝；请重新发起登录")
	if _, err := session.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("state 不匹配应被拒绝，实际: %v", err)
	}
}

func TestPrepareBodyForcesStreamAndCleansToolChoice(t *testing.T) {
	payload := []byte(`{"model":"glm-5.3","stream":false,"tool_choice":"none","messages":[{"role":"user","content":"hi"}]}`)
	out := prepareBody(payload)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if stream, _ := obj["stream"].(bool); !stream {
		t.Error("stream 应被强制为 true")
	}
	if _, ok := obj["tool_choice"]; ok {
		t.Error("空/none 的 tool_choice 应被剔除")
	}
	if _, ok := obj["messages"]; !ok {
		t.Error("messages 应原样保留")
	}
}

func TestSSEErrorFrameExtraction(t *testing.T) {
	raw := []byte("data: {\"error\":{\"message\":\"余额不足\",\"code\":40201}}\n\n")
	frame := sseErrorFrame(raw)
	if !strings.Contains(frame, "40201") {
		t.Fatalf("错误帧应携带业务码: %s", frame)
	}
	if !strings.Contains(frame, "额度已用尽") {
		t.Fatalf("已知额度码应补充中文描述用于冷却分类: %s", frame)
	}
	if sseErrorFrame([]byte("data: {\"choices\":[]}\n\n")) != "" {
		t.Fatal("正常帧不应被当成错误")
	}
}

func TestStreamErrorFilterConvertsLeadingError(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"error\":{\"message\":\"额度不足\",\"code\":40201}}\n\ndata: {\"choices\":[]}\n\n"))
	filter := newStreamErrorFilter(body)
	raw, err := io.ReadAll(filter)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "\"error\"") {
		t.Fatalf("应输出 OpenAI error 帧: %s", text)
	}
	if strings.Contains(text, "choices") {
		t.Fatal("错误判定后的正常帧应被丢弃")
	}
	if !strings.Contains(text, "[DONE]") {
		t.Fatal("合成错误体应以 [DONE] 结尾")
	}
	_ = filter.Close()
}

func TestStreamErrorFilterPassthroughByteExact(t *testing.T) {
	source := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\ndata: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	filter := newStreamErrorFilter(io.NopCloser(strings.NewReader(source)))
	raw, err := io.ReadAll(filter)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != source {
		t.Fatalf("正常流应逐字节透传:\n got %q\nwant %q", string(raw), source)
	}
	_ = filter.Close()
}

func TestStreamErrorFilterCommentHeadDoesNotDecideEarly(t *testing.T) {
	// SSE 注释先行：错误帧跟在注释后，仍要被翻译。
	source := ": ping\n\ndata: {\"error\":{\"message\":\"boom\"}}\n\n"
	filter := newStreamErrorFilter(io.NopCloser(strings.NewReader(source)))
	raw, err := io.ReadAll(filter)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "boom") || !strings.Contains(string(raw), "error") {
		t.Fatalf("注释头之后的首个错误帧应被翻译: %s", string(raw))
	}
	_ = filter.Close()
}

func TestQuotaCodeAnnotation(t *testing.T) {
	if got := annotateErrorFrame("code=40201 denied", 40201); !strings.Contains(got, "额度已用尽") {
		t.Fatalf("40201 应标注额度: %s", got)
	}
	if got := annotateErrorFrame("unknown", 99999); got != "unknown" {
		t.Fatalf("未知码不应改写: %s", got)
	}
}

func TestTokenJSONRoundTrip(t *testing.T) {
	// Token 序列化形态变化会影响上层凭据落盘，这里锁一下字段。
	token := Token{AccessToken: "a", RefreshToken: "r", ExpiresAt: 1, UUID: "u", FirstKeyfrom: "f", UserID: "1", Nickname: "n"}
	raw, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	var back Token
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back != token {
		t.Fatalf("往返不一致: %+v vs %+v", back, token)
	}
}

func TestPostJSONRejectionClassification(t *testing.T) {
	// 本地起一个 400 服务，验证 4xx 归类为 upstreamRejection（要求重新登录）。
	srv := newStubServer(t, http.StatusBadRequest, `{"code":400,"msg":"bad request"}`)
	_, err := postJSON(context.Background(), http.DefaultClient, srv.URL+ExchangePath, map[string]any{})
	var stale *upstreamRejection
	if !errors.As(err, &stale) {
		t.Fatalf("4xx 应归类为 upstreamRejection，实际: %v", err)
	}
	// 5xx 保持普通错误（刷新循环可重试）。
	srv2 := newStubServer(t, http.StatusBadGateway, "boom")
	_, err = postJSON(context.Background(), http.DefaultClient, srv2.URL+RefreshPath, map[string]any{})
	if errors.As(err, &stale) {
		t.Fatalf("5xx 不应归类为 upstreamRejection，实际: %v", err)
	}
	if err == nil {
		t.Fatal("5xx 应返回错误")
	}
}

func TestRefreshFallsBackToOldIdentity(t *testing.T) {
	// 刷新响应未携带 refreshToken/uuid/firstKeyfrom 时应沿用旧值。
	srv := newStubServer(t, http.StatusOK, `{"code":0,"msg":"","data":{"accessToken":"new-token","expiresIn":3600}}`)
	apiBaseTestOverride = srv.URL
	t.Cleanup(func() { apiBaseTestOverride = "" })
	token, err := Refresh(context.Background(), http.DefaultClient, "old-refresh", "uuid-1", "kf-1", "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = srv
	if token.RefreshToken != "old-refresh" || token.UUID != "uuid-1" || token.FirstKeyfrom != "kf-1" {
		t.Fatalf("身份字段应沿用旧值: %+v", token)
	}
	if token.AccessToken != "new-token" {
		t.Fatalf("accessToken 应取新值: %+v", token)
	}
}

func TestJWTExpiryFallback(t *testing.T) {
	// expires_in<=0 时回退解析 JWT exp（构造 exp=now+3600 的三段 token）。
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`))
	fake := "header." + payload + ".sig"
	if got := jwtExpiry(fake); got <= time.Now().Unix() {
		t.Fatalf("应解析出未来 exp: %d", got)
	}
	if jwtExpiry("not-a-jwt") != 0 {
		t.Fatal("非 JWT 应返回 0")
	}
}

// 回调 CSRF：state 是唯一防线。对不上 state 的回调（伪造 error、伪造 code、
// 纯探测）一律当噪音忽略——之前不带凭据的 error / 缺授权码不校验 state 就
// deliver，等于给本机可达的任意网页一个一键打断登录的开关。
func TestServeConnIgnoresUntrustedState(t *testing.T) {
	// serve 一次回调并返回 (会话, 应答正文)。
	serve := func(t *testing.T, target string) (*LoginSession, string) {
		t.Helper()
		session := &LoginSession{State: "s1", notify: make(chan struct{})}
		client, server := net.Pipe()
		defer client.Close()
		go serveConn(server, session)
		if _, err := io.WriteString(client, "GET "+target+" HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"); err != nil {
			t.Fatalf("写入请求失败: %v", err)
		}
		raw, err := io.ReadAll(client) // serveConn 返回时会关闭连接
		if err != nil && len(raw) == 0 {
			t.Fatalf("读取应答失败: %v", err)
		}
		return session, string(raw)
	}

	for _, tc := range []struct {
		name   string
		target string
	}{
		{"伪造拒绝", "/?error=denied"},
		{"伪造code加错state", "/?code=attacker&state=wrong"},
		{"只带state没有code", "/?state=wrong"},
		{"纯探测", "/?foo=bar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, _ := serve(t, tc.target)
			if session.done {
				t.Fatalf("%s 不应结束会话", tc.name)
			}
		})
	}

	// state 匹配的正常路径不受影响。
	session, _ := serve(t, "/?code=c1&state=s1")
	if !session.done || session.code != "c1" {
		t.Fatalf("state 匹配的回调应正常停靠: done=%v code=%q", session.done, session.code)
	}
	session, _ = serve(t, "/?error=denied&state=s1")
	if _, err := session.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("state 匹配的拒绝应上报: %v", err)
	}
	session, _ = serve(t, "/?state=s1")
	if _, err := session.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "授权码") {
		t.Fatalf("state 匹配但缺授权码应如实上报: %v", err)
	}
}
