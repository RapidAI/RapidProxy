// Package proxysrv 实现 OpenAI 兼容的 HTTP 服务：
//
//	GET  /v1/models              模型列表
//	POST /v1/chat/completions    对话补全（支持流式与非流式）
//	GET  /healthz                健康检查
//	GET  /                       浏览器可读的接入信息页
//
// 服务按需启停，账号在多个凭据之间轮询，遇到 401/403 会自动刷新令牌重试。
package proxysrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/znsoftm/RapidProxy/internal/applog"
	"github.com/znsoftm/RapidProxy/internal/config"
	"github.com/znsoftm/RapidProxy/internal/openai"
	"github.com/znsoftm/RapidProxy/internal/store"
	"github.com/znsoftm/RapidProxy/internal/upstream"
)

// ErrNoAccount 表示目标上游还没有登录任何账号。
var ErrNoAccount = errors.New("该上游尚未登录账号，请先在界面中完成登录")

// maxBodyBytes 限制下游请求体大小。
const maxBodyBytes = 64 << 20

// Server 是 OpenAI 兼容代理服务。
type Server struct {
	mu      sync.RWMutex
	cfg     *config.Config
	store   *store.Store
	log     *applog.Logger
	clients map[string]*upstream.Client

	modelsMu sync.RWMutex
	models   map[string][]upstream.ModelInfo

	cursorMu sync.Mutex
	cursor   map[string]uint64

	credLocks sync.Map // credential id -> *sync.Mutex

	srv      *http.Server
	listener net.Listener
	running  bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// New 创建代理服务实例。
func New(cfg *config.Config, st *store.Store, logger *applog.Logger) *Server {
	s := &Server{
		cfg:    cfg,
		store:  st,
		log:    logger,
		models: map[string][]upstream.ModelInfo{},
		cursor: map[string]uint64{},
	}
	s.mu.Lock()
	s.rebuildClientsLocked()
	s.mu.Unlock()
	return s
}

// SetConfig 用新配置替换当前配置（监听地址变更需要重启服务）。
func (s *Server) SetConfig(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.rebuildClientsLocked()
}

func (s *Server) rebuildClientsLocked() {
	clients := make(map[string]*upstream.Client, len(s.cfg.Profiles))
	for _, p := range s.cfg.Profiles {
		c, err := upstream.NewClient(p, s.cfg.Platform, s.cfg.Product)
		if err != nil {
			s.log.Errorf("初始化上游 %s 失败: %v", p.ID, err)
			continue
		}
		clients[p.ID] = c
	}
	s.clients = clients
}

// ClientFor 返回指定上游的协议客户端。
func (s *Server) ClientFor(profileID string) (*upstream.Client, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.clients[profileID]
	if !ok {
		return nil, fmt.Errorf("未知的上游: %s", profileID)
	}
	return c, nil
}

// Config 返回当前配置快照。
func (s *Server) Config() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// -----------------------------------------------------------------------------
// 生命周期
// -----------------------------------------------------------------------------

// Start 启动 HTTP 服务与模型同步循环。
func (s *Server) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("监听 %s 失败: %w", s.cfg.Listen, err)
	}
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 20 * time.Second,
	}
	s.listener = ln
	s.running = true
	s.stopCh = make(chan struct{})
	stopCh := s.stopCh
	srv := s.srv
	s.mu.Unlock()

	s.log.Infof("代理服务已启动: %s （OpenAI base_url: %s）", ln.Addr().String(), s.cfg.OpenAIBaseURL())

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Errorf("HTTP 服务异常退出: %v", err)
		}
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.modelSyncLoop(stopCh)
	}()
	return nil
}

// Stop 停止服务。
func (s *Server) Stop() error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	srv := s.srv
	close(s.stopCh)
	s.running = false
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := srv.Shutdown(ctx)
	s.wg.Wait()
	s.log.Infof("代理服务已停止")
	return err
}

// Running 返回服务是否在运行。
func (s *Server) Running() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener == nil {
		return s.cfg.Listen
	}
	return s.listener.Addr().String()
}

// -----------------------------------------------------------------------------
// 模型缓存
// -----------------------------------------------------------------------------

func (s *Server) setModels(providerID string, models []upstream.ModelInfo) {
	s.modelsMu.Lock()
	s.models[providerID] = models
	s.modelsMu.Unlock()
}

// modelsFor 返回某上游的模型列表；没有缓存时退回内置白名单。
func (s *Server) modelsFor(providerID string) []upstream.ModelInfo {
	s.modelsMu.RLock()
	cached := s.models[providerID]
	s.modelsMu.RUnlock()
	if len(cached) > 0 {
		return cached
	}
	client, err := s.ClientFor(providerID)
	if err != nil {
		return nil
	}
	return client.BuildModelList(nil, nil, nil)
}

// ModelsFor 返回某个上游的模型列表（无缓存时退回内置白名单）。
func (s *Server) ModelsFor(providerID string) []upstream.ModelInfo {
	return s.modelsFor(providerID)
}

// AllModels 汇总所有已启用上游的模型（同名模型以配置中靠前的上游为准）。
func (s *Server) AllModels() []openai.Model {
	cfg := s.Config()
	seen := map[string]bool{}
	out := make([]openai.Model, 0, 32)
	for _, p := range cfg.EnabledProfiles() {
		for _, m := range s.modelsFor(p.ID) {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, openai.Model{
				ID:              m.ID,
				Object:          "model",
				Created:         m.Created,
				OwnedBy:         m.OwnedBy,
				Provider:        p.ID,
				DisplayName:     m.Name,
				ContextLength:   m.ContextLength,
				MaxOutputTokens: m.MaxOutputTokens,
				SupportsImages:  m.SupportsImages,
			})
		}
	}
	return out
}

// ModelIDs 返回所有可用模型 ID。
func (s *Server) ModelIDs() []string {
	models := s.AllModels()
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// RefreshModels 从上游同步模型列表；失败时保留内置白名单。
func (s *Server) RefreshModels(ctx context.Context) {
	cfg := s.Config()
	for _, p := range cfg.EnabledProfiles() {
		client, err := s.ClientFor(p.ID)
		if err != nil {
			continue
		}
		var upstreamModels []upstream.UpstreamModel
		if p.SyncModels == nil || *p.SyncModels {
			if cred := s.firstCredential(p.ID); cred != nil {
				unlock := s.lockCredential(cred.ID)
				if err := client.EnsureToken(ctx, cred); err == nil {
					models, err := client.FetchUpstreamModels(ctx, cred)
					if err == nil {
						upstreamModels = models
						_ = s.store.Save(cred)
					} else {
						s.log.Warnf("同步 %s 模型失败: %v", p.ID, err)
					}
				}
				unlock()
			}
		}
		models := client.BuildModelList(p.ExtraModels, p.DisabledModels, upstreamModels)
		s.setModels(p.ID, models)
		s.log.Infof("上游 %s 可用模型 %d 个", p.ID, len(models))
	}
}

func (s *Server) modelSyncLoop(stop <-chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	s.RefreshModels(ctx)
	cancel()

	interval := s.Config().SyncInterval()
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			s.RefreshModels(ctx)
			cancel()
		}
	}
}

// -----------------------------------------------------------------------------
// 路由
// -----------------------------------------------------------------------------

func (s *Server) registerRoutes(mux *http.ServeMux) {
	h := func(fn http.HandlerFunc) http.HandlerFunc {
		return s.withCORS(s.withAuth(fn))
	}
	mux.HandleFunc("/v1/models", h(s.handleModels))
	mux.HandleFunc("/models", h(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", h(s.handleChat))
	mux.HandleFunc("/chat/completions", h(s.handleChat))
	mux.HandleFunc("/healthz", h(s.handleHealth))
	mux.HandleFunc("/", h(s.handleIndex))
}

func (s *Server) withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Config().CORSEnabled() {
			if origin := s.allowOrigin(r); origin != "" {
				header := w.Header()
				header.Set("Access-Control-Allow-Origin", origin)
				header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, X-RapidProxy-Provider, X-RapidProxy-Account")
				header.Set("Access-Control-Max-Age", "86400")
				// 响应随 Origin 变化，提醒中间层不要共用缓存。
				header.Set("Vary", "Origin")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// allowOrigin 决定这次请求要给浏览器返回什么样的 Access-Control-Allow-Origin，
// 返回空串表示不开放跨域（浏览器会拦截响应）。
//
// 为什么不能无条件返回 "*"：代理默认监听 127.0.0.1，如果允许任意来源，
// 用户随便打开的一个网页就能调用本代理、并且读到完整响应，等于把已登录的
// WorkBuddy / CodeBuddy 账号免费开放给互联网。
//
// 规则：
//   - 非浏览器请求（没有 Origin 头）：返回 "*"，与 curl / SDK 的行为保持一致；
//   - 已配置 API Key：回显来源，持有密钥即视为授权；
//   - 未配置密钥：只放行本地来源（localhost / 127.0.0.1 / ::1），
//     也就是用户自己机器上跑的本地网页应用。
func (s *Server) allowOrigin(r *http.Request) string {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return "*"
	}
	if s.Config().RequiresAPIKey() {
		return origin
	}
	if isLocalOrigin(origin) {
		return origin
	}
	return ""
}

// isLocalOrigin 判断来源是否为本机（含 file:// 这类 Origin: null 的本地页面）。
func isLocalOrigin(origin string) bool {
	if origin == "null" {
		// 本地 HTML 文件（file://）由浏览器发送。
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	host = strings.ToLower(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.Config()
		if cfg.RequiresAPIKey() {
			if key := clientKey(r); !cfg.CheckAPIKey(key) {
				writeError(w, http.StatusUnauthorized, "API Key 无效，请在 RapidProxy 界面查看或重新生成", "invalid_request_error", "invalid_api_key")
				return
			}
		}
		next(w, r)
	}
}

func clientKey(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if _, token, ok := strings.Cut(v, " "); ok {
			return strings.TrimSpace(token)
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(r.URL.Query().Get("key"))
}

// -----------------------------------------------------------------------------
// 处理器
// -----------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"running": s.Running(),
		"models":  len(s.AllModels()),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, openai.ModelList{Object: "list", Data: s.AllModels()})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "未知路径: "+r.URL.Path, "invalid_request_error", "not_found")
		return
	}
	cfg := s.Config()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML(cfg.OpenAIBaseURL(), cfg.RequiresAPIKey(), len(s.AllModels())))
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error(), "invalid_request_error", "invalid_body")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error(), "invalid_request_error", "invalid_body")
		return
	}
	requestedModel, _ := payload["model"].(string)
	if requestedModel == "" {
		writeError(w, http.StatusBadRequest, "缺少 model 字段", "invalid_request_error", "missing_model")
		return
	}
	wantStream, _ := payload["stream"].(bool)

	profile, upstreamModel, err := s.resolveModel(requestedModel, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "model_not_found")
		return
	}
	payload["model"] = upstreamModel
	body, _, err := openai.PrepareRequestBody(mustJSON(payload), openai.TransformOptions{
		Sanitize:         s.Config().SanitizeEnabled(),
		ForceMaxThinking: s.Config().MaxThinkingEnabled(),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "构造上游请求失败: "+err.Error(), "invalid_request_error", "invalid_body")
		return
	}

	started := time.Now()
	resp, cred, err := s.doChat(r.Context(), profile.ID, body, r)
	if err != nil {
		s.log.Errorf("[%s] %s 转发失败: %v", profile.ID, requestedModel, err)
		status := http.StatusBadGateway
		if errors.Is(err, ErrNoAccount) {
			status = http.StatusUnauthorized
		}
		writeError(w, status, err.Error(), "upstream_error", "upstream_error")
		return
	}
	defer resp.Body.Close()

	if !isSSE(resp) {
		s.writeNonStreamUpstream(w, resp, requestedModel, profile.ID, started)
		return
	}

	if wantStream {
		s.pipeSSE(w, resp)
		s.log.Infof("[%s] %s 流式转发完成，耗时 %s（账号 %s）", profile.ID, requestedModel, time.Since(started).Round(time.Millisecond), cred.DisplayName())
		return
	}
	result, err := openai.Aggregate(resp.Body, upstreamModel)
	if err != nil {
		writeError(w, http.StatusBadGateway, "聚合上游响应失败: "+err.Error(), "upstream_error", "upstream_error")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
	s.log.Infof("[%s] %s 非流式转发完成，耗时 %s（账号 %s）", profile.ID, requestedModel, time.Since(started).Round(time.Millisecond), cred.DisplayName())
}

// writeNonStreamUpstream 处理上游意外返回非 SSE 的情况：正常响应透传，错误响应转成 OpenAI 错误。
func (s *Server) writeNonStreamUpstream(w http.ResponseWriter, resp *http.Response, model, provider string, started time.Time) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 400 {
		var probe map[string]any
		if json.Unmarshal(raw, &probe) == nil {
			if _, ok := probe["choices"]; ok {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(raw)
				return
			}
		}
	}
	message := strings.TrimSpace(string(raw))
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
		message = fmt.Sprintf("上游返回 code=%d msg=%s", env.Code, env.Msg)
	} else if message == "" {
		message = fmt.Sprintf("上游 HTTP %d", resp.StatusCode)
	}
	s.log.Errorf("[%s] %s 上游异常: %s", provider, model, truncate(message, 200))
	writeError(w, http.StatusBadGateway, message, "upstream_error", "upstream_error")
}

func (s *Server) pipeSSE(w http.ResponseWriter, resp *http.Response) {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	err := openai.ReadSSEEvents(resp.Body, func(data string) error {
		if _, err := io.WriteString(w, "data: "+openai.CleanChunk(data)+"\n\n"); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		s.log.Warnf("读取上游流失败: %v", err)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// -----------------------------------------------------------------------------
// 模型解析与账号选择
// -----------------------------------------------------------------------------

// resolveModel 把客户端传来的模型名映射到具体上游。
//
// 支持三种写法：
//  1. 直接写模型名（按已启用上游的模型列表匹配）
//  2. "上游ID:模型名" 或 "上游ID/模型名" 强制指定
//  3. 请求头 X-RapidProxy-Provider 或查询参数 ?provider= 强制指定
func (s *Server) resolveModel(name string, r *http.Request) (config.Profile, string, error) {
	cfg := s.Config()
	enabled := cfg.EnabledProfiles()
	if len(enabled) == 0 {
		return config.Profile{}, "", errors.New("没有启用任何上游，请先在设置中启用")
	}

	model := strings.TrimSpace(name)
	forced := strings.TrimSpace(r.Header.Get("X-RapidProxy-Provider"))
	if forced == "" {
		forced = strings.TrimSpace(r.URL.Query().Get("provider"))
	}
	if idx := strings.IndexAny(model, ":/"); idx > 0 {
		if p, ok := cfg.ProfileByID(model[:idx]); ok && p.Enabled {
			forced = p.ID
			model = model[idx+1:]
		}
	}
	if forced != "" {
		profile, ok := cfg.ProfileByID(forced)
		if !ok {
			return config.Profile{}, "", fmt.Errorf("未知的上游: %s", forced)
		}
		if !profile.Enabled {
			return config.Profile{}, "", fmt.Errorf("上游 %s 未启用", forced)
		}
		return profile, model, nil
	}

	for _, profile := range enabled {
		for _, m := range s.modelsFor(profile.ID) {
			if m.ID == model {
				return profile, model, nil
			}
		}
	}
	// 未匹配到具体上游时交给配置中第一个启用的上游，避免客户端因别名而失败。
	profile := enabled[0]
	s.log.Warnf("模型 %s 不在已知列表中，按默认上游 %s 转发", model, profile.ID)
	return profile, model, nil
}

func (s *Server) firstCredential(providerID string) *upstream.Credential {
	creds, err := s.store.List(providerID)
	if err != nil || len(creds) == 0 {
		return nil
	}
	return creds[0]
}

func (s *Server) lockCredential(id string) func() {
	value, _ := s.credLocks.LoadOrStore(id, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Server) nextCursor(providerID string) int {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	value := s.cursor[providerID]
	s.cursor[providerID] = value + 1
	return int(value)
}

// doChat 转发请求；账号之间轮询，认证失败先刷新令牌再重试。
func (s *Server) doChat(ctx context.Context, providerID string, body []byte, r *http.Request) (*http.Response, *upstream.Credential, error) {
	client, err := s.ClientFor(providerID)
	if err != nil {
		return nil, nil, err
	}
	creds, err := s.store.List(providerID)
	if err != nil {
		return nil, nil, fmt.Errorf("读取凭据失败: %w", err)
	}
	if want := strings.TrimSpace(r.Header.Get("X-RapidProxy-Account")); want != "" {
		filtered := creds[:0]
		for _, c := range creds {
			if c.ID == want || c.Account.UID == want {
				filtered = append(filtered, c)
			}
		}
		creds = filtered
	}
	if len(creds) == 0 {
		return nil, nil, ErrNoAccount
	}

	start := s.nextCursor(providerID)
	var lastErr error
	for i := 0; i < len(creds); i++ {
		cred := creds[(start+i)%len(creds)]
		unlock := s.lockCredential(cred.ID)

		beforeToken, beforeExpiry := cred.Auth.AccessToken, cred.Auth.ExpiresAt
		if err := client.EnsureToken(ctx, cred); err != nil {
			unlock()
			lastErr = err
			s.log.Warnf("账号 %s 令牌不可用: %v", cred.DisplayName(), err)
			continue
		}

		resp, err := client.Chat(ctx, cred, body)
		if err != nil {
			unlock()
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			_ = resp.Body.Close()
			if refreshErr := client.Refresh(ctx, cred); refreshErr != nil {
				unlock()
				lastErr = fmt.Errorf("账号 %s 认证失败且刷新令牌失败: %w", cred.DisplayName(), refreshErr)
				continue
			}
			retry, retryErr := client.Chat(ctx, cred, body)
			if retryErr == nil && retry.StatusCode != http.StatusUnauthorized && retry.StatusCode != http.StatusForbidden {
				_ = s.store.Save(cred)
				unlock()
				return retry, cred, nil
			}
			if retry != nil {
				_ = retry.Body.Close()
			}
			unlock()
			lastErr = fmt.Errorf("账号 %s 认证失败（已尝试刷新令牌）", cred.DisplayName())
			continue
		}

		if cred.Auth.AccessToken != beforeToken || cred.Auth.ExpiresAt != beforeExpiry {
			_ = s.store.Save(cred)
		}
		unlock()
		return resp, cred, nil
	}
	if lastErr == nil {
		lastErr = ErrNoAccount
	}
	return nil, nil, lastErr
}

// -----------------------------------------------------------------------------
// 工具
// -----------------------------------------------------------------------------

func isSSE(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message, typ string, code any) {
	writeJSON(w, status, openai.NewError(message, typ, code))
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func indexHTML(baseURL string, requireKey bool, modelCount int) string {
	keyHint := "无需密钥"
	if requireKey {
		keyHint = "需要在请求头携带 API Key"
	}
	base := html.EscapeString(baseURL)
	modelsURL := html.EscapeString(baseURL + "/models")
	chatURL := html.EscapeString(baseURL + "/chat/completions")
	return `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>RapidProxy</title>
<style>
 body{font-family:-apple-system,"Segoe UI","Microsoft YaHei",sans-serif;background:#f6f7f9;color:#1f2937;margin:0;padding:48px;line-height:1.7}
 .card{max-width:680px;margin:0 auto;background:#fff;border:1px solid #e5e7eb;border-radius:14px;padding:32px 36px}
 h1{font-size:22px;margin:0 0 4px}
 p{color:#6b7280;margin:6px 0 20px}
 code{background:#f3f4f6;border-radius:6px;padding:2px 7px;font-size:13px}
 pre{background:#0f172a;color:#e2e8f0;border-radius:10px;padding:16px;overflow:auto;font-size:13px}
 .row{display:flex;justify-content:space-between;border-top:1px solid #eef0f3;padding:10px 0}
 .row span:first-child{color:#6b7280}
</style></head><body>
<div class="card">
 <h1>RapidProxy</h1>
 <p>本地址是 OpenAI 兼容接口，可直接填入任意支持自定义 API 的客户端。</p>
 <div class="row"><span>Base URL</span><code>` + base + `</code></div>
 <div class="row"><span>模型列表</span><code>` + modelsURL + `</code></div>
 <div class="row"><span>对话补全</span><code>` + chatURL + `</code></div>
 <div class="row"><span>模型数量</span><code>` + fmt.Sprintf("%d", modelCount) + `</code></div>
 <div class="row"><span>鉴权</span><code>` + keyHint + `</code></div>
 <pre>curl ` + chatURL + ` \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer &lt;API_KEY&gt;" \
  -d '{"model":"gpt-5.4","messages":[{"role":"user","content":"你好"}]}'</pre>
	</div></body></html>`
}
