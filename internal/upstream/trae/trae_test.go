package trae

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildAuthorizeURLShape(t *testing.T) {
	raw := BuildAuthorizeURL(CNProfile(), LoginOptions{
		MachineID: "m1",
		DeviceID:  "d1",
		TraceID:   "t1",
		Port:      18080,
	})
	if !strings.HasPrefix(raw, "https://www.trae.cn/authorization?") {
		t.Fatalf("授权 URL 前缀不对: %s", raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if got := len(q); got != 18 {
		t.Fatalf("授权 URL 应为 18 参数，实际 %d: %v", got, q)
	}
	// 真机校准：不带 PKCE challenge，带了会被引向需要设备证明的新协议。
	for _, banned := range []string{"code_challenge", "code_challenge_method", "hide_saas_login", "x_env", "channel_name"} {
		if _, ok := q[banned]; ok {
			t.Errorf("授权 URL 不应携带 %s", banned)
		}
	}
	if q.Get("auth_from") != "solo" || q.Get("login_channel") != "native_ide" {
		t.Errorf("auth_from/login_channel 不对: %v", q)
	}
	if q.Get("client_id") != SOLOClientID {
		t.Errorf("client_id 不对: %v", q.Get("client_id"))
	}
	if q.Get("machine_id") != "m1" || q.Get("device_id") != "d1" || q.Get("x_machine_id") != "m1" {
		t.Errorf("设备对未写入: %v", q)
	}
	if q.Get("auth_callback_url") != "http://127.0.0.1:18080/authorize" {
		t.Errorf("回调地址不对: %v", q.Get("auth_callback_url"))
	}
}

func TestProfileByStoreIDAndAuthHosts(t *testing.T) {
	cn, ok := ProfileByStoreID(StoreCN)
	if !ok || cn.ChatHost != "https://trae-api-cn.mchost.guru" {
		t.Fatalf("国内区配置不对: %+v", cn)
	}
	if hosts := cn.AuthHosts(); len(hosts) != 2 {
		t.Fatalf("国内区应双 host 并存: %v", hosts)
	}
	gl, ok := ProfileByStoreID(StoreGlobal)
	if !ok || gl.ChatHost != "https://coresg-normal.trae.ai" {
		t.Fatalf("国际区配置不对: %+v", gl)
	}
	if hosts := gl.AuthHosts(); len(hosts) != 1 {
		t.Fatalf("国际区应单 host: %v", hosts)
	}
}

func TestPrepareBodyTranslatesOpenAIToSolo(t *testing.T) {
	payload := []byte(`{"model":"glm-5.2","stream":true,"max_tokens":128,"temperature":0.5,
		"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}],
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{}}}}],
		"tool_choice":{"type":"function","function":{"name":"f"}},"reasoning_effort":"high"}`)
	solo, model, _ := prepareBody(payload, CNProfile())
	if model != "glm-5.2" {
		t.Fatalf("model 应取请求里的模型名: %q", model)
	}
	// llm_utils_chat 只接受白名单字段：OpenAI 特有字段必须被丢弃。
	for _, banned := range []string{"reasoning_effort", "stream_options", "max_completion_tokens"} {
		if _, ok := solo[banned]; ok {
			t.Errorf("SOLO 体不应携带 %s", banned)
		}
	}
	if solo["function"] != soloChatFunction {
		t.Errorf("function 应钉在首选池: %v", solo["function"])
	}
	if solo["config_name"] != "glm-5.2" {
		t.Errorf("config_name 应取模型名: %v", solo["config_name"])
	}
	if solo["stream"] != true {
		t.Error("stream 应强制为 true")
	}
	// prepareBody 直通 map：值为 int；序列化到线体后与 float64 输出一致。
	if n, ok := solo["max_tokens"].(int); !ok || n != 128 {
		t.Errorf("max_tokens 应透传: %v", solo["max_tokens"])
	}
	// content 字符串要变成 text chip。
	msgs := solo["messages"].([]any)
	first := msgs[0].(map[string]any)
	chip, ok := first["content"].([]any)
	if !ok || chip[0].(map[string]any)["type"] != "text" {
		t.Errorf("content 应转为 text chip: %v", first["content"])
	}
	// assistant tool_calls 换成 function_call 形态。
	second := msgs[1].(map[string]any)
	call := second["tool_calls"].([]any)[0].(map[string]any)
	if _, ok := call["function_call"]; !ok {
		t.Errorf("tool_calls 应换成 function_call: %v", call)
	}
	// tools 的 parameters 要序列化成字符串。
	tool := solo["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if _, isStr := tool["parameters"].(string); !isStr {
		t.Errorf("parameters 应序列化为字符串: %T", tool["parameters"])
	}
	// tool_choice 对象形态拍平成函数名字符串。
	if solo["tool_choice"] != "f" {
		t.Errorf("tool_choice 应拍平: %v", solo["tool_choice"])
	}
}

// 两次池尝试共享同一会话（session_id 稳定），function/request_id 逐次刷新。
func TestPoolAttemptsShareSession(t *testing.T) {
	solo, _, _ := prepareBody([]byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`), CNProfile())
	marshal := func(fn string) map[string]any {
		solo["function"] = fn
		solo["request_id"] = randomUUID()
		raw, err := json.Marshal(solo)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	first := marshal(soloChatFunction)
	second := marshal("chat_v3")
	if first["session_id"] != second["session_id"] {
		t.Fatalf("session_id 应跨池尝试保持同一会话: %v vs %v", first["session_id"], second["session_id"])
	}
	if second["function"] != "chat_v3" {
		t.Errorf("function 应随池改写: %v", second["function"])
	}
	if first["request_id"] == second["request_id"] {
		t.Error("request_id 每次尝试都应重新生成")
	}
	if first["model"] != "glm-5.2" || second["config_name"] != "glm-5.2" {
		t.Fatalf("model/config_name 应稳定: %v/%v", first["model"], second["config_name"])
	}
}

func TestTranslateSSEStreamsOpenAIChunks(t *testing.T) {
	upstream := strings.NewReader("event: output\ndata: {\"response\":\"你\"}\n\n" +
		"event: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	var out strings.Builder
	if err := translateSSE(context.Background(), upstream, &out, "glm-5.2"); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, `"content":"你"`) {
		t.Fatalf("应输出内容增量: %s", text)
	}
	if !strings.Contains(text, `"prompt_tokens":3`) {
		t.Fatalf("usage 应附在收尾帧: %s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("应输出 finish 帧: %s", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatal("流应以 [DONE] 结尾")
	}
}

func TestTranslateSSEErrorBecomesOpenAIErrorFrame(t *testing.T) {
	upstream := strings.NewReader("event: error\ndata: {\"code\":1005,\"message\":\"over quota\"}\n\n")
	var out strings.Builder
	if err := translateSSE(context.Background(), upstream, &out, "glm-5.2"); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "\"error\"") {
		t.Fatalf("错误应译成 OpenAI error 帧: %s", text)
	}
	// 已知错误码要带中文描述，冷却分类才能命中「额度」关键词。
	if !strings.Contains(text, "额度") {
		t.Fatalf("1005 应补充额度文案: %s", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatal("错误流也应以 [DONE] 收尾")
	}
}

func TestSSEScannerAcrossChunkBoundaries(t *testing.T) {
	scanner := &sseScanner{}
	var events []*soloEvent
	events = append(events, scanner.step([]byte("event: out"))...)
	events = append(events, scanner.step([]byte("put\ndata: {\"res"))...)
	events = append(events, scanner.step([]byte("ponse\":\"好\"}\n\n"))...)
	events = append(events, scanner.close()...)
	if len(events) != 1 || events[0].kind != "output" || events[0].response != "好" {
		t.Fatalf("跨块事件应正确拼装: %+v", events)
	}
}

func TestParseModelCatalogFiltersNonChatEntries(t *testing.T) {
	raw := []byte(`{"Result":{"config_info_list":[
		{"config_name":"auto","display_name":"Auto","usage":"chat_completion","config_switch":true},
		{"config_name":"glm-5.2","display_config":{"display_name":"GLM-5.2"},"usage":"chat_completion","config_switch":true},
		{"config_name":"custom_model_1","usage":"chat_completion","config_switch":true},
		{"config_name":"summary-helper","usage":"summary","config_switch":true},
		{"config_name":"off-model","usage":"chat_completion","config_switch":false}
	]}}`)
	models, err := parseModelCatalog("Trae 国内版", raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "glm-5.2" || models[0].DisplayName != "GLM-5.2" {
		t.Fatalf("应只保留可对话的 config: %+v", models)
	}
}

func TestParseModelCatalogHonoursInvisibleOnGlobal(t *testing.T) {
	raw := []byte(`{"config_info_list":[
		{"config_name":"a","usage":"chat_completion","config_switch":true},
		{"config_name":"b","usage":"chat_completion","config_switch":true,"is_invisible_to_user":true}
	]}`)
	cn, err := parseModelCatalog("Trae 国内版", raw)
	if err != nil || len(cn) != 2 {
		t.Fatalf("国内区不做 invisible 过滤: %+v err=%v", cn, err)
	}
	gl, err := parseModelCatalog(NameGlobal, raw)
	if err != nil || len(gl) != 1 || gl[0].ID != "a" {
		t.Fatalf("国际区应剔除不可见条目: %+v err=%v", gl, err)
	}
}

func TestWrongFunctionCode(t *testing.T) {
	if !wrongFunctionCode("4001") || !wrongFunctionCode("4023") {
		t.Fatal("4001/4023 是池成员拒绝")
	}
	if wrongFunctionCode("1005") {
		t.Fatal("1005 是额度拒绝，不能换池")
	}
}

func TestAnnotateErrorCode(t *testing.T) {
	if got := annotateErrorCode("1005", "plan limit"); !strings.Contains(got, "额度") {
		t.Fatalf("1005 应补充额度文案: %s", got)
	}
	if got := annotateErrorCode("4001", ""); !strings.Contains(got, "参数") {
		t.Fatalf("4001 应有描述: %s", got)
	}
	if got := annotateErrorCode("9999", "raw"); got != "raw" {
		t.Fatalf("未知码不改写: %s", got)
	}
}

func TestChatPinsHeadersAndFallsThroughPools(t *testing.T) {
	// 首池回答 4001 成员拒绝，第二池给真答案：Chat 应自动落到下一池。
	requests := 0
	var authHeader, appID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		authHeader = r.Header.Get("Authorization")
		appID = r.Header.Get("X-App-Id")
		body, _ := io.ReadAll(r.Body)
		var solo map[string]any
		_ = json.Unmarshal(body, &solo)
		w.Header().Set("Content-Type", "text/event-stream")
		if solo["function"] == soloChatFunction {
			_, _ = w.Write([]byte("event: error\ndata: {\"code\":4001,\"message\":\"param is invalid\"}\n\n"))
			return
		}
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n"))
	}))
	defer srv.Close()
	profile := CNProfile()
	profile.ChatHost = srv.URL
	resp, err := Chat(context.Background(), srv.Client(), profile, "token", "", "", "uid-9", []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if requests != 2 {
		t.Fatalf("应按池顺序重试，实际请求 %d 次", requests)
	}
	if !strings.Contains(text, `"content":"ok"`) {
		t.Fatalf("应输出第二池的翻译结果: %s", text)
	}
	// 回归：consumeFunctionRejection 预读过的字节不得重复进流
	//（首段内容恰好出现一次，而不是两段拼接）。
	if strings.Count(text, `"content":"ok"`) != 1 {
		t.Fatalf("预读前缀被重复拼进流里: %s", text)
	}
	if authHeader != "Cloud-IDE-JWT token" {
		t.Fatalf("应使用 Cloud-IDE-JWT 头: %v", authHeader)
	}
	if appID != AppID {
		t.Fatal("应携带 SOLO 应用标识")
	}
}

func TestChatJSONEnvelopeMapsStatus(t *testing.T) {
	// 业务码映射成对应 HTTP 语义：1005 额度类 → 402（长冷却）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1005,"message":"plan quota"}`))
	}))
	defer srv.Close()
	profile := CNProfile()
	profile.ChatHost = srv.URL
	resp, err := Chat(context.Background(), srv.Client(), profile, "token", "", "", "uid-9", []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("1005 业务信封应合成为 402，实际 %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "额度") {
		t.Fatalf("1005 应带额度文案: %s", raw)
	}

	// 4001（模型不在该池且池已轮尽）→ 502。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":4001,"message":"param is invalid"}`))
	}))
	defer srv2.Close()
	profile2 := CNProfile()
	profile2.ChatHost = srv2.URL
	resp2, err := Chat(context.Background(), srv2.Client(), profile2, "token", "", "", "", []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("4001 业务信封应合成为 502，实际 %d", resp2.StatusCode)
	}
}

func TestIdentityDerivedPerUser(t *testing.T) {
	a := IdentityForUserID("100")
	b := IdentityForUserID("200")
	if a.MachineID == b.MachineID || a.DeviceID == b.DeviceID {
		t.Fatal("不同账号应派生不同设备指纹（风控要求）")
	}
	again := IdentityForUserID("100")
	if again.MachineID != a.MachineID {
		t.Fatal("同一账号的指纹应稳定")
	}
	if len(a.DeviceID) != 16 || a.DeviceID[0] == '0' {
		t.Fatalf("设备 id 应为 16 位十进制且首位非零: %s", a.DeviceID)
	}
}

func TestChatSendsContentType(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n"))
	}))
	defer srv.Close()
	profile := CNProfile()
	profile.ChatHost = srv.URL
	resp, err := Chat(context.Background(), srv.Client(), profile, "token", "", "", "uid-9", []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if gotCT != "application/json" {
		t.Fatalf("对话请求必须声明 JSON 体，实际 Content-Type: %q", gotCT)
	}
}

// 长流式回答的首字节不能被形态预读阻塞：上游只发一小段然后挂住不发，
// Chat 也必须立刻把响应交回调用方（回归 64KB 预读凑满才返回的延迟 bug）。
func TestChatFirstByteNotBlockedByShapePeek(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"首段\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // 之后不再发送，模拟漫长的生成过程
	}))
	// defer 是 LIFO：close(release) 必须后注册（先执行），否则 srv.Close
	// 会先等一个永远阻塞的请求而死锁。
	defer srv.Close()
	defer close(release)
	profile := CNProfile()
	profile.ChatHost = srv.URL
	done := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := Chat(context.Background(), srv.Client(), profile, "token", "", "", "uid-9", []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			errCh <- err
			return
		}
		done <- resp
	}()
	select {
	case err := <-errCh:
		t.Fatalf("Chat 失败: %v", err)
	case resp := <-done:
		resp.Body.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("响应被形态预读阻塞：上游未产出 64KB 前不返回首字节")
	}
}

// 凭据里存的登录身份（设备对 + uid）必须整体优先于 claims/token 派生兜底：
// 不能出现"存储 machine + 派生 device"的错配指纹（token 家族从未 mint 过）。
func TestChatStoredIdentityWinsOverDerived(t *testing.T) {
	var machine, device, uid string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		machine, device, uid = r.Header.Get("X-Machine-Id"), r.Header.Get("X-Device-Id"), r.Header.Get("X-Uid")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n"))
	}))
	defer srv.Close()
	profile := CNProfile()
	profile.ChatHost = srv.URL
	resp, err := Chat(context.Background(), srv.Client(), profile, "token", "stored-machine", "stored-device", "stored-uid",
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if machine != "stored-machine" || device != "stored-device" || uid != "stored-uid" {
		t.Fatalf("存储身份应整体优先于派生兜底: machine=%q device=%q uid=%q", machine, device, uid)
	}
	// 派生兜底是确定性指纹：存储值绝不应等于派生值（否则混用不可察觉）。
	derived := IdentityForUserID("stored-uid")
	if machine == derived.MachineID && device == derived.DeviceID {
		t.Fatal("存储对不应与 uid 派生对重合")
	}
}

// 只存了设备对、没存 uid 时：设备对保持存储值，uid 走 claims 兜底，
// 不产生错配。
func TestChatPartialStoredIdentity(t *testing.T) {
	var machine, device, uid string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		machine, device, uid = r.Header.Get("X-Machine-Id"), r.Header.Get("X-Device-Id"), r.Header.Get("X-Uid")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n"))
	}))
	defer srv.Close()
	profile := CNProfile()
	profile.ChatHost = srv.URL
	// 带 uid claim 的伪 JWT：设备对只存了一半时，uid 走 claims 兜底。
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"userId":"claim-uid"}`))
	jwt := "hdr." + claims + ".sig"
	resp, err := Chat(context.Background(), srv.Client(), profile, jwt, "stored-machine", "", "",
		[]byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if machine != "stored-machine" {
		t.Fatalf("存储 machine 应保留: %q", machine)
	}
	// 兜底派生是全有或全无（与上游协议一致）：machine 已有存储值时
	// 绝不单独派生 device 拼出 token 家族从未 mint 过的错配对；
	// device 缺席优于错配。
	derivedFromUID := IdentityForUserID("claim-uid")
	if device == derivedFromUID.DeviceID {
		t.Fatalf("device 不得与存储 machine 混成派生配对: %q", device)
	}
	if uid != "claim-uid" {
		t.Fatalf("uid 应有 claims 兜底: %q", uid)
	}
}

// 流以空白行开头、JSON 信封在第二行时仍要判为 JSON（回归首行即断的误判）。
func TestChatJSONAfterLeadingBlankLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("\r\n{\"code\":1005,\"message\":\"plan quota\"}"))
	}))
	defer srv.Close()
	profile := CNProfile()
	profile.ChatHost = srv.URL
	resp, err := Chat(context.Background(), srv.Client(), profile, "token", "", "", "", []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("空白行后的 JSON 信封应按业务码映射（402），实际 %d: %s", resp.StatusCode, raw)
	}
}

// 回调 CSRF：loginTraceID 是唯一防线。不带或对不上 trace 的回调一律不采信
// ——之前「当作拒绝处理」会把会话整个打断，等于给本机可达的任意网页一个
// 打断登录或投递自己 refreshToken 的开关。
func TestReceiveCallbackIgnoresUntrustedTrace(t *testing.T) {
	newSession := func() *LoginSession {
		return &LoginSession{TraceID: "trace-1", MachineID: "m", DeviceID: "d", notify: make(chan struct{})}
	}

	// 对不上 trace 的凭据回调：忽略，会话保持等待。
	s := newSession()
	if s.receiveCallback(map[string]string{"refreshToken": "attacker", "loginTraceID": "trace-2"}) {
		t.Fatal("trace 不匹配的凭据回调应被忽略")
	}
	if s.done {
		t.Fatal("trace 不匹配的回调不应结束会话")
	}

	// 不带 trace 的凭据回调（攻击者可伪造的最简形态）：同样忽略。
	s = newSession()
	if s.receiveCallback(map[string]string{"refreshToken": "attacker"}) {
		t.Fatal("不带 trace 的凭据回调应被忽略")
	}
	if s.done {
		t.Fatal("不带 trace 的回调不应结束会话")
	}

	// 不带 trace 的拒绝回调（?code=x&error=denied 可打断登录）：忽略。
	s = newSession()
	if s.receiveCallback(map[string]string{"code": "x", "error": "denied"}) {
		t.Fatal("不带 trace 的拒绝回调应被忽略")
	}
	if s.done {
		t.Fatal("不带 trace 的拒绝回调不应结束会话")
	}

	// 带上正确 trace 的凭据回调：正常停靠（返回值 false = 未被拒绝）。
	s = newSession()
	if denied := s.receiveCallback(map[string]string{"refreshToken": "real", "loginTraceID": "trace-1"}); denied {
		t.Fatal("trace 匹配的凭据回调不应标记为拒绝")
	}
	if !s.done || s.details == nil || s.details.RefreshToken != "real" {
		t.Fatalf("trace 匹配的回调应携带凭据: done=%v details=%v", s.done, s.details)
	}

	// 带正确 trace 的拒绝回调：如实上报（真实用户的「取消授权」路径）。
	// 注意回调里同时带 code 时会先按 AuthCode 处理，deny 只有无凭据时生效。
	s = newSession()
	if denied := s.receiveCallback(map[string]string{"error": "denied", "loginTraceID": "trace-1"}); !denied {
		t.Fatal("trace 匹配的拒绝回调应上报拒绝")
	}
	if _, err := s.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("拒绝原因应进入会话错误: %v", err)
	}
}

// 错误摘要先脱敏再截断：网关半途出错时可能把带 token 的响应体回显，
// 这些文本会进日志。
func TestSummarizeBodyRedactsSecrets(t *testing.T) {
	raw := []byte(`{"error":"boom","accessToken":"sk-live-123","refreshToken":"rt-456","message":"配额不足"}`)
	got := summarizeBody(raw)
	if strings.Contains(got, "sk-live-123") || strings.Contains(got, "rt-456") {
		t.Fatalf("摘要不应包含 token 原文: %s", got)
	}
	if !strings.Contains(got, "[redacted]") || !strings.Contains(got, "配额不足") {
		t.Fatalf("摘要应保留非敏感内容并标注已脱敏: %s", got)
	}
	// 带转义引号的值也要整段抹掉，不能留下后半截。
	if got := summarizeBody([]byte(`{"token":"a\"bcd","ok":1}`)); strings.Contains(got, "bcd") {
		t.Fatalf("转义值应整段脱敏: %s", got)
	}
}
