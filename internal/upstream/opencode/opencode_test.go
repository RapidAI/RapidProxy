package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// officialSessionID 匹配官方 Identifier.create("session") 的形状：
// ses_ + 12 位十六进制时间 + 14 位 base62。
var officialSessionID = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func TestClientUserAgentConstant(t *testing.T) {
	if ClientUserAgent != "opencode/1.18.31" {
		t.Fatalf("ClientUserAgent = %q", ClientUserAgent)
	}
	if PresetUserAgent != "opencode" {
		t.Fatalf("PresetUserAgent = %q", PresetUserAgent)
	}
	// 网关拒绝低于 1.17.0 的版本号：把 semver 解析出来比对，防止手滑改错。
	version := strings.TrimPrefix(ClientUserAgent, "opencode/")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		t.Fatalf("版本号不是三段式 semver: %q", version)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("版本号含非数字: %q", version)
		}
		nums[i] = n
	}
	if nums[0] < 1 || (nums[0] == 1 && nums[1] < 17) {
		t.Errorf("版本号 %s 低于网关要求的 1.17.0", version)
	}
}

func TestWireUserAgent(t *testing.T) {
	cases := map[string]string{
		"":                 ClientUserAgent, // 预设身份
		"opencode":         ClientUserAgent, // 裸 opencode 被拒，必须改写
		"  opencode  ":     ClientUserAgent, // 前后空白不影响判定
		"OpenCode":         ClientUserAgent, // 大小写不敏感
		"OPENCODE":         ClientUserAgent,
		"opencode/1.18.31": "opencode/1.18.31", // 自定义身份原样保留
		"custom-agent/9.9": "custom-agent/9.9",
	}
	for saved, want := range cases {
		if got := WireUserAgent(saved); got != want {
			t.Errorf("WireUserAgent(%q) = %q, want %q", saved, got, want)
		}
	}
}

func TestIsOpenCodeHost(t *testing.T) {
	cases := map[string]bool{
		"opencode.ai":             true,
		"api.opencode.ai":         true,
		"a.b.opencode.ai":         true,
		"  opencode.ai  ":         true,
		"OPENCODE.AI":             true,
		"opencode.ai.":            true, // FQDN 尾点：url.Hostname() 不会替我们去掉了
		"api.opencode.ai.":        true,
		"opencode.ai.evil.com":    false, // 后缀匹配必须落在域名边界上
		"notopencode.ai":          false,
		"opencode.com":            false,
		"opencode.ai.example.com": false,
		"":                        false,
		"localhost":               false,
		"127.0.0.1":               false,
	}
	for host, want := range cases {
		if got := IsOpenCodeHost(host); got != want {
			t.Errorf("IsOpenCodeHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestIsOpenCodeURL(t *testing.T) {
	cases := map[string]bool{
		"https://opencode.ai/zen/v1":                            true,
		"https://opencode.ai/zen/v1/":                           true,
		"https://api.opencode.ai/v1/models":                     true,
		"http://opencode.ai:8080/auth":                          true,
		"https://open.bigmodel.cn/api/paas/v4/chat/completions": false, // GLM：精确匹配裸 opencode 的那家
		"https://opencode.ai.evil.com/x":                        false,
		"not a url":                                             false,
		"":                                                      false,
	}
	for raw, want := range cases {
		if got := IsOpenCodeURL(raw); got != want {
			t.Errorf("IsOpenCodeURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestClientSessionIDShape(t *testing.T) {
	for _, parts := range [][]string{
		{"conversation-a"},
		{"conversation-a", "model-x"},
		{},
	} {
		id := ClientSessionID(parts...)
		if !officialSessionID.MatchString(id) {
			t.Errorf("ClientSessionID(%v) = %q 不符合官方形状", parts, id)
		}
		if !IsOfficialSessionID(id) {
			t.Errorf("IsOfficialSessionID(%q) = false，自产自销的 ID 必须被认可", id)
		}
	}
}

func TestClientSessionIDStableWithinHour(t *testing.T) {
	base := time.Date(2026, 10, 15, 12, 34, 56, 789, time.UTC)
	// clientSessionIDAt 对同一 (now, parts) 完全确定：时间部分取 now 的毫秒，
	// 随机部分取 parts 的哈希。
	first := clientSessionIDAt(base, "seed-1")
	if second := clientSessionIDAt(base, "seed-1"); first != second {
		t.Errorf("同一 (now, parts) 应得到同一个会话号: %q vs %q", first, second)
	}
	// ClientSessionID 把当前时间截到整点：同一小时内重复调用同号
	// （测试在整点边界上穿过的概率约 10ms/1h，可忽略）。
	if a, b := ClientSessionID("seed-1"), ClientSessionID("seed-1"); a != b {
		t.Errorf("同一小时内会话号应稳定: %q vs %q", a, b)
	}
	// 空白 parts 不参与哈希：等价于不传。
	if trimmed := clientSessionIDAt(base, "seed-1", "  ", ""); trimmed != first {
		t.Errorf("空白 parts 被计入哈希: %q vs %q", trimmed, first)
	}
	// 跨小时必须换新（时间段与哈希都不同）。
	nextHour := clientSessionIDAt(base.Add(time.Hour), "seed-1")
	if nextHour == first {
		t.Error("跨小时后会话号应变化")
	}
	// 同一小时内不同对话必须分开。
	other := clientSessionIDAt(base, "seed-2")
	if other == first {
		t.Error("不同对话的会话号不应相同")
	}
	// parts 的顺序参与哈希（0 字节分隔）。
	reversed := clientSessionIDAt(base, "b", "a")
	forward := clientSessionIDAt(base, "a", "b")
	if reversed == forward {
		t.Error("parts 顺序不应影响会话号（0 字节分隔缺失）")
	}
}

func TestClientSessionIDRandomWhenNoParts(t *testing.T) {
	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := clientSessionIDAt(base)
		if !officialSessionID.MatchString(id) {
			t.Fatalf("无 parts 的会话号形状异常: %q", id)
		}
		if seen[id] {
			t.Fatalf("无 parts 时会话号应随机，出现重复: %q", id)
		}
		seen[id] = true
	}
}

func TestSessionTimeHex(t *testing.T) {
	cases := map[int64]string{
		0:           "000000000000",
		1:           "000000001000", // 1<<12 = 0x1000 → 00 00 00 00 10 00
		0x123:       "000000123000", // 0x123<<12 = 0x123000 → 00 00 00 12 30 00
		0xfffffffff: "fffffffff000", // 36 位全 1 左移 12 位：高 36 位为 f，低 12 位为 0
	}
	for ms, want := range cases {
		if got := sessionTimeHex(ms); got != want {
			t.Errorf("sessionTimeHex(%d) = %q, want %q", ms, got, want)
		}
	}
	// 任意毫秒值都编码成 12 个小写十六进制字符。
	for _, ms := range []int64{1, 1000, 1 << 20, time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC).UnixMilli()} {
		got := sessionTimeHex(ms)
		if len(got) != 12 {
			t.Fatalf("sessionTimeHex(%d) 长度 = %d", ms, len(got))
		}
		for _, c := range got {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("sessionTimeHex(%d) = %q 含非小写十六进制字符", ms, got)
			}
		}
	}
	// 同一小时内编码确定；相邻小时不同（会话号跨小时换新的另一半保障）。
	hour := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	if sessionTimeHex(hour.UnixMilli()) == sessionTimeHex(hour.Add(time.Hour).UnixMilli()) {
		t.Error("相邻小时的时间编码应不同")
	}
}

func TestIsOfficialSessionID(t *testing.T) {
	cases := map[string]bool{
		"ses_000000000000" + strings.Repeat("a", 14):       true,
		"ses_ffffffffffffABCDEFGHIJKLMN":                   true,
		"ses_00000000000" + strings.Repeat("a", 14):        false, // 时间部分 11 位
		"ses_0000000000000" + strings.Repeat("a", 14):      false, // 时间部分 13 位
		"ses_000000000000" + strings.Repeat("a", 13):       false, // 尾部 13 位
		"ses_000000000000" + strings.Repeat("a", 15):       false, // 尾部 15 位
		"ses_00000000000G" + strings.Repeat("a", 14):       false, // 时间部分含非十六进制
		"ses_000000000000" + strings.Repeat("-", 14):       false, // 尾部含非 base62
		"ses_000000000000" + strings.Repeat("a", 13) + "_": false,
		"msg_000000000000" + strings.Repeat("a", 14):       false, // 前缀不对
		"550e8400-e29b-41d4-a716-446655440000":             false, // UUID 被拒
		"":                                                 false,
	}
	for id, want := range cases {
		if got := IsOfficialSessionID(id); got != want {
			t.Errorf("IsOfficialSessionID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestWireSessionID(t *testing.T) {
	official := "ses_000000000000" + strings.Repeat("Zz9", 5)[:14]
	if got := WireSessionID(official); got != official {
		t.Errorf("已是官方形状的 ID 应原样保留: %q", got)
	}
	// UUID 折叠成官方形状，且同一段对话在一小时内稳定。
	uuid := "550e8400-e29b-41d4-a716-446655440000"
	folded := WireSessionID(uuid)
	if !IsOfficialSessionID(folded) {
		t.Errorf("UUID 应折叠成官方形状，实际 %q", folded)
	}
	if again := WireSessionID(uuid); again != folded {
		t.Errorf("同一 UUID 在一小时内应得到同一个会话号: %q vs %q", folded, again)
	}
	// 空值直接生成新的官方 ID，且每次不同。
	empty := WireSessionID("")
	if !IsOfficialSessionID(empty) {
		t.Errorf("空值应生成官方形状，实际 %q", empty)
	}
	if empty == WireSessionID("") {
		t.Error("空值两次生成不应相同")
	}
}

func TestConversationSeed(t *testing.T) {
	body := func(v any) []byte {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"字符串 content", body(map[string]any{
			"model":    "big-pickle",
			"messages": []any{map[string]any{"role": "system", "content": "ignored"}, map[string]any{"role": "user", "content": "你好"}},
		}), "你好"},
		{"分段数组 content", body(map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "第一段"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x"}},
				map[string]any{"type": "text", "text": "第二段"},
			}}},
		}), "第一段第二段"},
		{"嵌套 content", body(map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": map[string]any{"content": "深层文本"}}},
		}), "深层文本"},
		{"取不到 user 消息", body(map[string]any{
			"messages": []any{map[string]any{"role": "assistant", "content": "hi"}},
		}), ""},
		{"空 user content 跳到下一条", body(map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": ""}, map[string]any{"role": "user", "content": "真正的首条"}},
		}), "真正的首条"},
		{"不是合法 JSON", []byte("{oops"), ""},
		{"空 body", nil, ""},
		{"messages 不是数组", body(map[string]any{"messages": "hi"}), ""},
	}
	for _, c := range cases {
		if got := ConversationSeed(c.body); got != c.want {
			t.Errorf("%s: ConversationSeed = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestConversationSeedTruncatesAtUTF8Boundary(t *testing.T) {
	// 正好 8192 字节（8189 个 ASCII + 一个三字节字符）：未超限，原样返回。
	exact := strings.Repeat("a", seedByteLimit-3) + "界"
	if len(exact) != seedByteLimit {
		t.Fatalf("构造的种子长度 = %d", len(exact))
	}
	body, err := json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": exact}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ConversationSeed(body); got != exact {
		t.Errorf("未超限的种子应原样返回（长度 %d）", len(got))
	}

	// 8194 字节（8191 个 ASCII + 一个三字节字符）：超限，截断点回退到
	// 8191，整个多字节字符被丢弃而不是切成乱码。
	over := strings.Repeat("a", seedByteLimit-1) + "界"
	body2, _ := json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": over}},
	})
	got2 := ConversationSeed(body2)
	if len(got2) != seedByteLimit-1 {
		t.Errorf("截断后长度 = %d, want %d", len(got2), seedByteLimit-1)
	}
	if strings.ContainsRune(got2, '界') {
		t.Error("跨截断点的多字节字符应被整体丢弃")
	}
	if !utf8.ValidString(got2) {
		t.Error("截断结果含无效 UTF-8")
	}
}

func TestApplyWireHeadersOnOpenCodeHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "opencode")
	ApplyWireHeaders(req, "opencode", "同一段对话")

	if got := req.Header.Get("User-Agent"); got != ClientUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, ClientUserAgent)
	}
	session := req.Header.Get(SessionHeader)
	if !IsOfficialSessionID(session) {
		t.Fatalf("x-opencode-session = %q 不是官方形状", session)
	}
	// 同一 seed 一小时内的第二个请求落在同一个会话上。
	req2 := httptest.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	ApplyWireHeaders(req2, "opencode", "同一段对话")
	if req2.Header.Get(SessionHeader) != session {
		t.Errorf("同一对话的会话号应稳定: %q vs %q", session, req2.Header.Get(SessionHeader))
	}
	// 自定义身份不被改写。
	req3 := httptest.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	ApplyWireHeaders(req3, "my-gateway/2.0", "seed")
	if got := req3.Header.Get("User-Agent"); got != "my-gateway/2.0" {
		t.Errorf("自定义 User-Agent 被改写: %q", got)
	}
	ApplyWireHeaders(nil, "opencode", "seed") // 不能 panic
}

// 设置里保存的裸 "opencode" 必须原样发给非 OpenCode 上游——GLM 正是按这个
// 值精确匹配 opencode 客户端的，一个字节都不能动。
func TestApplyWireHeadersIgnoresOtherHosts(t *testing.T) {
	for _, target := range []string{
		"https://open.bigmodel.cn/api/paas/v4/chat/completions",
		"https://api.workbuddy.ai/v1/chat/completions",
		"http://127.0.0.1:8787/v1/chat/completions",
	} {
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req.Header.Set("User-Agent", "opencode")
		ApplyWireHeaders(req, "opencode", "seed")
		if got := req.Header.Get("User-Agent"); got != "opencode" {
			t.Errorf("%s: User-Agent 被改写为 %q", target, got)
		}
		if got := req.Header.Get(SessionHeader); got != "" {
			t.Errorf("%s: 不应携带 %s: %q", target, SessionHeader, got)
		}
	}
}

func TestBuildChatRequest(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`)
	req, err := BuildChatRequest(context.Background(), "https://opencode.ai/zen/v1/", "sk-test-key", "opencode", body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost {
		t.Errorf("方法 = %s", req.Method)
	}
	if req.URL.String() != "https://opencode.ai/zen/v1/chat/completions" {
		t.Errorf("端点 = %s", req.URL)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json, text/event-stream, */*" {
		t.Errorf("Accept = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test-key" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != ClientUserAgent {
		t.Errorf("User-Agent = %q", got)
	}
	if got := req.Header.Get(SessionHeader); !IsOfficialSessionID(got) {
		t.Errorf("%s = %q 不是官方形状", SessionHeader, got)
	}
	gotBody, _ := io.ReadAll(req.Body)
	if string(gotBody) != string(body) {
		t.Errorf("请求体被改动: %s", gotBody)
	}
	// 空密钥不带 Authorization 头。
	req2, err := BuildChatRequest(context.Background(), APIBase, "  ", "opencode", body)
	if err != nil {
		t.Fatal(err)
	}
	if got := req2.Header.Get("Authorization"); got != "" {
		t.Errorf("空密钥不应带 Authorization: %q", got)
	}
}

func TestChatRoundTrip(t *testing.T) {
	var gotBody []byte
	var gotUA, gotSession, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotUA = r.Header.Get("User-Agent")
		gotSession = r.Header.Get(SessionHeader)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"ok\":true}\n\n")
	}))
	defer srv.Close()

	body := []byte(`{"model":"big-pickle","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := Chat(context.Background(), srv.Client(), srv.URL, "sk-key", "opencode", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if string(gotBody) != string(body) {
		t.Errorf("转发体被改动: %s", gotBody)
	}
	// httptest 监听 127.0.0.1：不是 OpenCode 主机。设置里保存的 UA 原样
	// 发出（GLM 那条精确匹配要的就是这个值），且不携带会话头。
	if gotUA != "opencode" {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if gotSession != "" {
		t.Errorf("不应携带 %s: %q", SessionHeader, gotSession)
	}
	if gotAuth != "Bearer sk-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d", resp.StatusCode)
	}
}

func TestListModels(t *testing.T) {
	var gotAuth, gotAccept, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotPath = r.URL.Path
		io.WriteString(w, `{"data":[{"id":"big-pickle","name":"Big Pickle"},{"id":"gpt-5","name":""}]}`)
	}))
	defer srv.Close()

	models, err := ListModels(context.Background(), srv.Client(), srv.URL+"/", "sk-key", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/models" {
		t.Errorf("路径 = %q", gotPath)
	}
	if gotAuth != "Bearer sk-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if len(models) != 2 {
		t.Fatalf("模型数量 = %d", len(models))
	}
	if models[0].ID != "big-pickle" || models[0].Name != "Big Pickle" {
		t.Errorf("条目解析异常: %+v", models[0])
	}
	// name 缺失时回退成 id。
	if models[1].Name != "gpt-5" {
		t.Errorf("name 回退异常: %+v", models[1])
	}
}

func TestListModelsToleratesBareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"m1","name":"M1"},{"id":"","name":"忽略空 id"}]`)
	}))
	defer srv.Close()
	models, err := ListModels(context.Background(), srv.Client(), srv.URL, "sk-key", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("裸数组解析异常: %+v", models)
	}
}

func TestListModelsErrors(t *testing.T) {
	if _, err := ListModels(context.Background(), http.DefaultClient, APIBase, "  ", "opencode"); err == nil {
		t.Error("空密钥应报错")
	}
	// 401/403 是「密钥不认」的明确信号，必须能被 ErrKeyRejected 识别。
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			io.WriteString(w, "bad key")
		}))
		_, err := ListModels(context.Background(), srv.Client(), srv.URL, "sk-bad", "opencode")
		srv.Close()
		if !errors.Is(err, ErrKeyRejected) {
			t.Errorf("HTTP %d 应包上 ErrKeyRejected，实际 %v", code, err)
		}
		var statusErr *StatusError
		if !errors.As(err, &statusErr) || statusErr.Code != code {
			t.Errorf("HTTP %d 应同时携带 StatusError，实际 %v", code, err)
		}
	}
	// 其它状态码只是普通错误，不判定为密钥被拒。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	}))
	defer srv.Close()
	_, err := ListModels(context.Background(), srv.Client(), srv.URL, "sk-key", "opencode")
	if err == nil || errors.Is(err, ErrKeyRejected) {
		t.Errorf("500 不应判定为密钥被拒，实际 %v", err)
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusInternalServerError {
		t.Errorf("500 应携带 StatusError，实际 %v", err)
	}
}

func TestNormalizeKey(t *testing.T) {
	cases := map[string]string{
		"sk-opencode-123456":                        "sk-opencode-123456",
		"  sk-opencode-123456":                      "sk-opencode-123456",
		"\"sk-opencode-123456\"":                    "sk-opencode-123456",
		"'sk-opencode-123456'":                      "sk-opencode-123456",
		"Bearer sk-opencode-123456":                 "sk-opencode-123456",
		"bearer sk-opencode-123456":                 "sk-opencode-123456",
		`{"key":"sk-opencode-123456"}`:              "sk-opencode-123456",
		`{"opencode":{"key":"sk-opencode-123456"}}`: "sk-opencode-123456",
		`{"OPENCODE_API_KEY":"sk-opencode-123456"}`: "sk-opencode-123456",
		// JSON 里的值也可能带着 bearer 前缀：剥离顺序必须覆盖到。
		`{"key":"Bearer sk-opencode-123456"}`: "sk-opencode-123456",
		// 数组形态（从文档里整段复制的列表）也要能取出密钥。
		`[{"key":"sk-opencode-123456"}]`: "sk-opencode-123456",
	}
	for raw, want := range cases {
		got, err := NormalizeKey(raw)
		if err != nil {
			t.Errorf("NormalizeKey(%q) 报错: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeKey(%q) = %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{
		"", "   ", "short", "sk-with space", "sk-with\ttab",
		`{"nope":1}`,                  // 太短，且取不出密钥
		`{"foo":"barbarbar"}`,         // 长度够但取不出密钥：JSON 本身就是粘贴噪声
		`{"opencode":{"type":"api"}}`, // 有 opencode 段但没有 key 字段
		`[{"foo":"barbarbar"}]`,       // 数组里同样取不出密钥
	} {
		_, err := NormalizeKey(raw)
		if err == nil {
			t.Errorf("NormalizeKey(%q) 应报错", raw)
			continue
		}
		if !errors.Is(err, ErrKeyInvalid) {
			t.Errorf("NormalizeKey(%q) 的错误应为 ErrKeyInvalid，实际 %v", raw, err)
		}
	}
}

func TestKeyLabel(t *testing.T) {
	if got := KeyLabel("sk-opencode-abcd"); got != "OpenCode · abcd" {
		t.Errorf("KeyLabel = %q", got)
	}
	if got := KeyLabel("abc"); got != "OpenCode Key" {
		t.Errorf("过短密钥的 KeyLabel = %q", got)
	}
}

func TestAPIBaseAndAuthURL(t *testing.T) {
	if APIBase != "https://opencode.ai/zen/v1" {
		t.Errorf("APIBase = %q", APIBase)
	}
	if AuthURL != "https://opencode.ai/auth" {
		t.Errorf("AuthURL = %q", AuthURL)
	}
	if !IsOpenCodeURL(APIBase) || !IsOpenCodeURL(AuthURL) {
		t.Error("内置端点必须指向 OpenCode 主机，否则身份改写不会触发")
	}
}

func TestSessionHeaderName(t *testing.T) {
	if SessionHeader != "x-opencode-session" {
		t.Errorf("SessionHeader = %q", SessionHeader)
	}
}

func ExampleClientSessionID() {
	// 同一段对话（同一首条 user 消息）在一小时内得到同一个会话号。
	fmt.Println(IsOfficialSessionID(ClientSessionID("你好，帮我重构这个函数")))
	// Output: true
}
