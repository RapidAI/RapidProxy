package oauthflow

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGenerateVerifierAndChallenge(t *testing.T) {
	verifier, err := GenerateVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if len(verifier) != verifierLength {
		t.Errorf("verifier 长度 = %d, want %d", len(verifier), verifierLength)
	}
	for _, r := range verifier {
		if !strings.ContainsRune(verifierChars, r) {
			t.Fatalf("verifier 含非法字符 %q", r)
		}
	}
	// RFC 7636 的参考用例：S256("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")。
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != want {
		t.Errorf("Challenge = %q, want %q", got, want)
	}
}

func TestNormalizeCode(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"abc123", "abc123"},
		{"  abc123  ", "abc123"},
		// Anthropic 的回调页显示 "code#state"。
		{"abc123#verifier-value", "abc123"},
		// 整段回调 URL。
		{"https://console.anthropic.com/oauth/code/callback?code=abc123&state=v", "abc123"},
		{"code=abc123&state=v", "abc123"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := NormalizeCode(tc.raw); got != tc.want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestJWTAccountID(t *testing.T) {
	// 构造一个 payload 为 {"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}
	// 的 JWT（签名部分不参与解析）。
	const payload = `eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdC0xIn19`
	token := "header." + payload + ".sig"
	if got := JWTAccountID(token); got != "acct-1" {
		t.Errorf("JWTAccountID = %q, want acct-1", got)
	}
	// 非 JWT 形态不应 panic，也不应返回垃圾值。
	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.!!!.c"} {
		if got := JWTAccountID(bad); got != "" {
			t.Errorf("JWTAccountID(%q) = %q, want empty", bad, got)
		}
	}
}

func TestStartCallbackReceivesCode(t *testing.T) {
	cb, err := StartCallback("/auth/callback", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Stop()

	redirect := cb.RedirectURI("localhost")
	if !strings.HasPrefix(redirect, "http://localhost:") {
		t.Fatalf("redirect_uri 形态异常: %q", redirect)
	}

	// 探活请求（不带 code/error）不应结束等待。
	resp, err := http.Get(redirect)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("探活请求应返回 204，实际 %d", resp.StatusCode)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		resp, err := http.Get(redirect + "?code=abc&state=xyz")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := cb.Wait(ctx)
	if err != nil {
		t.Fatalf("等待回调失败: %v", err)
	}
	if res.Code != "abc" || res.State != "xyz" {
		t.Errorf("回调结果异常: %+v", res)
	}
}

func TestStartCallbackReportsOAuthError(t *testing.T) {
	cb, err := StartCallback("/callback", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Stop()

	go func() {
		time.Sleep(20 * time.Millisecond)
		resp, err := http.Get(cb.RedirectURI("127.0.0.1") + "?error=access_denied&error_description=nope")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cb.Wait(ctx); err == nil {
		t.Fatal("授权被拒绝时应返回错误")
	}
}

func TestStartCallbackPortFallback(t *testing.T) {
	first, err := StartCallback("/cb", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Stop()

	// 指定一个已被占用的端口：应向上顺延而不是直接失败。
	second, err := StartCallback("/cb", first.Port())
	if err != nil {
		t.Fatalf("端口占用时应顺延: %v", err)
	}
	defer second.Stop()
	if second.Port() == first.Port() {
		t.Error("端口占用时不应复用同一端口")
	}
}

// 浏览器可能把 localhost 解析成 ::1 或 127.0.0.1，两族都要能收到回调。
func TestStartCallbackAcceptsBothLoopbackFamilies(t *testing.T) {
	cb, err := StartCallback("/cb", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Stop()

	urls := []string{
		fmt.Sprintf("http://127.0.0.1:%d/cb?code=c4", cb.Port()),
		fmt.Sprintf("http://[::1]:%d/cb?code=c6", cb.Port()),
	}
	for _, u := range urls {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		go func(target string) {
			time.Sleep(20 * time.Millisecond)
			resp, err := http.Get(target)
			if err == nil {
				_ = resp.Body.Close()
			}
		}(u)
		res, err := cb.Wait(ctx)
		cancel()
		if err != nil {
			t.Errorf("%s: 等待回调失败: %v", u, err)
			continue
		}
		if res.Code == "" {
			t.Errorf("%s: 未收到授权码", u)
		}
	}
}
