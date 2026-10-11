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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/znsoftm/RapidProxy/internal/applog"
	"github.com/znsoftm/RapidProxy/internal/config"
	"github.com/znsoftm/RapidProxy/internal/openai"
	"github.com/znsoftm/RapidProxy/internal/stats"
	"github.com/znsoftm/RapidProxy/internal/store"
	"github.com/znsoftm/RapidProxy/internal/upstream"
)

// ErrNoAccount 表示目标上游还没有登录任何账号。
var ErrNoAccount = errors.New("该上游尚未登录账号，请先在界面中完成登录")

// ErrAllPaused 表示该上游的账号都被暂停了。
var ErrAllPaused = errors.New("该上游的所有账号都已暂停，请在账号页恢复至少一个")

// upstreamHTTPError 携带上游的真实 HTTP 状态码与文案。
//
// 换号后新账号回的 4xx（模型没权限、内容超长等）要原样透传给下游，
// 不能一律压成 502，否则调用方会把「客户端错误」当成「可重试的临时故障」
// 反复重发同一个坏请求；更不能把 err 写成 nil 让 handleChat 吐出
// 「200 + 空 body」。
type upstreamHTTPError struct {
	Status  int
	Message string
}

func (e *upstreamHTTPError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("上游 HTTP %d", e.Status)
}

// maxBodyBytes 限制下游请求体大小。
const maxBodyBytes = 64 << 20

// Server 是 OpenAI 兼容代理服务。
type Server struct {
	mu          sync.RWMutex
	startStopMu sync.Mutex // 串行化 Start/Stop 整体过程，避免两者交错导致「丢失停止」或端口争用
	cfg         *config.Config
	store       *store.Store
	log         *applog.Logger
	clients     map[string]*upstream.Client

	modelsMu sync.RWMutex
	models   map[string][]upstream.ModelInfo

	// stats 记录按组按模型的 token 流量，落盘到 <dataDir>/traffic.json。
	stats *stats.Recorder

	// syncMu 保证同一时刻只有一轮模型同步在跑（见 RefreshModels）。
	syncMu sync.Mutex

	// modelGen / entryCache 复用模型汇总结果。
	// 模型列表与每次 chat 路由都要汇总一遍（每个模型都要分配一个 modelEntry），
	// 而结果只在模型缓存或登录状态变化时才变，按版本号缓存即可避免重复计算。
	modelGen   uint64
	entryMu    sync.Mutex
	entryGen   uint64
	entryCache []modelEntry

	// creds 缓存凭据清单：一次扫描供多个请求复用，见 loadCreds 与 hasCredential。
	credsMu sync.Mutex
	creds   *credCache

	// balState 保存平滑加权轮询的中间权重，key 为 "<provider>/<credential>"。
	balMu    sync.Mutex
	balState map[string]int

	// cool 记录账号的限流冷却状态，key 为 credential ID。
	// 冷却是运行时状态：重启即恢复，不写进凭据文件。
	coolMu sync.Mutex
	cool   map[string]*cooldownState

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
		cfg:      cfg,
		store:    st,
		log:      logger,
		models:   map[string][]upstream.ModelInfo{},
		balState: map[string]int{},
		cool:     map[string]*cooldownState{},
	}
	// 流量统计文件与凭据同目录的父级（即 dataDir），凭据目录是 dataDir/accounts。
	if st != nil {
		s.stats = stats.New(filepath.Dir(st.Root()))
	}
	s.mu.Lock()
	s.rebuildClientsLocked()
	s.mu.Unlock()
	return s
}

// Stats 返回流量统计记录器（可能为 nil，仅在数据目录可用时创建）。
func (s *Server) Stats() *stats.Recorder { return s.stats }

// recordUsage 把一次成功请求的 token 用量计入流量统计。
func (s *Server) recordUsage(provider, model string, u stats.Usage) {
	if s.stats == nil || !u.Observed {
		return
	}
	s.stats.Record(stats.Entry{
		Provider: provider,
		Model:    model,
		At:       time.Now(),
		In:       u.In,
		Out:      u.Out,
		Total:    u.Total,
	})
}

// extractUsageString 从一段 SSE data JSON 里提取 usage；没有则返回 observed=false。
//
// 上游不一定每个 chunk 都带 usage，但最后的 usage 事件会覆盖先前的，所以调用方
// 只需保留「最近一次 observed 的」即可。total 缺失时退化为 prompt+completion。
func extractUsageString(data string) (stats.Usage, bool) {
	var v struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil || v.Usage == nil {
		return stats.Usage{}, false
	}
	total := v.Usage.TotalTokens
	if total == 0 {
		total = v.Usage.PromptTokens + v.Usage.CompletionTokens
	}
	return stats.Usage{
		In:       v.Usage.PromptTokens,
		Out:      v.Usage.CompletionTokens,
		Total:    total,
		Observed: true,
	}, true
}

// SetConfig 用新配置替换当前配置（监听地址变更需要重启服务）。
func (s *Server) SetConfig(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.rebuildClientsLocked()
	// 上游启停变了，对外暴露的模型清单随之改变。
	s.bumpModelGen()
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
//
// 整体过程由 startStopMu 串行化：与 Stop 不会交错，避免「停止请求先于启动生效」
// 或「新监听与旧监听端口争用」这类不一致状态。
func (s *Server) Start() error {
	s.startStopMu.Lock()
	defer s.startStopMu.Unlock()
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
//
// 与 Start 共用 startStopMu：若 Stop 已抢到锁并开始 Shutdown，后来的 Start 会
// 排队等 Shutdown 完成后才拿到锁，此时 running 已是 false，不会重复监听端口。
func (s *Server) Stop() error {
	s.startStopMu.Lock()
	defer s.startStopMu.Unlock()
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
	// 服务停止时把流量统计落盘并结束后台刷盘协程：Close 内部会先 Flush 再
	// 关闭 stop 通道并等待 run() 退出，避免进程退出时该协程仍在后台空转。
	if s.stats != nil {
		s.stats.Close()
	}
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
	s.bumpModelGen()
}

// bumpModelGen 让模型汇总缓存失效。
//
// 模型缓存、登录状态、上游启停变了之后，对外暴露的模型清单会随之改变，
// 缓存的汇总结果必须重算，否则列表和路由会用上一版的快照。
func (s *Server) bumpModelGen() {
	atomic.AddUint64(&s.modelGen, 1)
}

// credCacheTTL 是凭据清单的缓存有效期。
//
// 判断登录状态、挑选账号都要读凭据目录，而模型列表与每次 chat 请求都会用到；
// 每个请求都把全部凭据文件读一遍并解析 JSON 会让磁盘成为瓶颈，所以扫一次
// 复用两秒。登录、删除、暂停、改权重这些操作会主动失效缓存。
const credCacheTTL = 2 * time.Second

// credCache 是一次扫描的结果，按上游分组保存凭据的副本。
//
// 存的是值而不是指针：调用方需要改动自己的那份（刷新令牌、改权重），
// 共享指针会让并发请求互相踩。
type credCache struct {
	at         time.Time
	byProvider map[string][]upstream.Credential
}

// loadCreds 返回某上游的凭据（provider 为空表示全部）。返回的是副本，调用方可随意改动。
func (s *Server) loadCreds(providerID string) ([]*upstream.Credential, error) {
	s.credsMu.Lock()
	defer s.credsMu.Unlock()
	if s.creds == nil || time.Since(s.creds.at) > credCacheTTL {
		if err := s.scanCredsLocked(); err != nil {
			if s.creds == nil {
				return nil, err
			}
			// 扫描失败但手上还有旧快照：凑合用总比让整个请求失败好。
			s.log.Warnf("读取凭据失败，沿用上次快照: %v", err)
		}
	}
	return copyCreds(s.creds.byProvider[providerID]), nil
}

// scanCredsLocked 扫描凭据目录并替换快照，调用方必须持有 credsMu。
func (s *Server) scanCredsLocked() error {
	// 数据目录不可用（store.New 失败）时 store 为 nil，这里明确报错，
	// 而不是让 s.store.List 在 nil 接收者上解引用崩溃。
	if s.store == nil {
		return errors.New("凭据仓库不可用")
	}
	list, err := s.store.List("")
	if err != nil {
		return fmt.Errorf("读取凭据失败: %w", err)
	}
	next := &credCache{
		at:         time.Now(),
		byProvider: make(map[string][]upstream.Credential, len(list)),
	}
	for _, c := range list {
		next.byProvider[c.Provider] = append(next.byProvider[c.Provider], *c)
	}
	s.creds = next
	return nil
}

func copyCreds(src []upstream.Credential) []*upstream.Credential {
	if len(src) == 0 {
		return nil
	}
	out := make([]*upstream.Credential, 0, len(src))
	for i := range src {
		cp := src[i]
		out = append(out, &cp)
	}
	return out
}

// updateCachedCred 把刚落盘的凭据写回缓存。
//
// 不写回的话，下一次请求在缓存过期前拿到的还是旧令牌，会白白多一次 401 重试。
func (s *Server) updateCachedCred(cred *upstream.Credential) {
	if cred == nil || cred.Provider == "" || cred.ID == "" {
		return
	}
	s.credsMu.Lock()
	defer s.credsMu.Unlock()
	if s.creds == nil {
		return
	}
	list := s.creds.byProvider[cred.Provider]
	for i := range list {
		if list[i].ID == cred.ID {
			list[i] = *cred
			return
		}
	}
	// 第一次登录的账号：缓存里还没有，补进去，省掉一次重新扫描。
	s.creds.byProvider[cred.Provider] = append(list, *cred)
}

// hasCredential 报告指定上游是否至少登录了一个账号。
func (s *Server) hasCredential(providerID string) bool {
	creds, err := s.loadCreds(providerID)
	if err != nil {
		s.log.Warnf("读取 %s 凭据失败: %v", providerID, err)
		return false
	}
	return len(creds) > 0
}

// InvalidateAccounts 让凭据缓存立即失效（账号增删、暂停、改权重后调用）。
func (s *Server) InvalidateAccounts() {
	s.credsMu.Lock()
	s.creds = nil
	s.credsMu.Unlock()
	s.bumpModelGen()
}

// loggedInProfiles 返回「已启用且至少登录了一个账号」的上游。
//
// 这是中外分版的边界：模型只在对应版本登录后才会出现——只登录国内版就只列
// 国内版模型，只登录国际版就只列国际版模型，两边都登录才同时列出。
func (s *Server) loggedInProfiles() []config.Profile {
	enabled := s.Config().EnabledProfiles()
	out := make([]config.Profile, 0, len(enabled))
	for _, p := range enabled {
		if s.hasCredential(p.ID) {
			out = append(out, p)
		}
	}
	return out
}

// modelsFor 返回某上游的模型列表；没有缓存时退回内置白名单。
//
// 未登录任何账号的上游返回空列表：内置白名单是「该版本可能有哪些模型」，
// 而不是「当前可用」，未登录时就暴露会让两个版本的模型混在一起。
func (s *Server) modelsFor(providerID string) []upstream.ModelInfo {
	if !s.hasCredential(providerID) {
		return nil
	}
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
	// 同步还没跑完时的兜底：要带上配置里的增删项，否则这一版的模型列表
	// 会和首次同步完成后的结果不一致（多出被禁用的、少了手动添加的）。
	profile, _ := s.Config().ProfileByID(providerID)
	return client.BuildModelList(profile.ExtraModels, profile.DisabledModels, nil)
}

// ModelsFor 返回某个上游的模型列表（未登录时为空）。
func (s *Server) ModelsFor(providerID string) []upstream.ModelInfo {
	return s.modelsFor(providerID)
}

// modelEntry 是一条对外暴露的模型，附带它应该被转发到哪个上游、用什么名字。
type modelEntry struct {
	openai.Model
	// profileID 是目标上游；upstreamID 是转发给上游时使用的模型名。
	profileID  string
	upstreamID string
}

// modelEntries 汇总所有已登录上游的模型。
//
// 命名规则：多个版本同时登录时，国际版（workbuddy）的模型统一加 "intl:"
// 前缀，国内版保持裸模型名（裸名默认路由到国内版）；只登录单个版本时
// 全部保持原样，方便客户端直接填裸模型名。
func (s *Server) modelEntries() []modelEntry {
	gen := atomic.LoadUint64(&s.modelGen)
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	if s.entryCache != nil && s.entryGen == gen {
		return s.entryCache
	}
	entries := s.buildModelEntries()
	s.entryCache = entries
	s.entryGen = gen
	return entries
}

// buildModelEntries 实际汇总模型；返回的切片之后不再修改，可以被多个请求共享。
//
// 调用方必须持有 entryMu。
func (s *Server) buildModelEntries() []modelEntry {
	active := s.loggedInProfiles()

	// 多版本场景下国际版需要前缀区分。
	tag := map[string]bool{}
	if len(active) > 1 {
		for _, p := range active {
			if p.ID == intlProviderID {
				tag[p.ID] = true
			}
		}
	}

	out := make([]modelEntry, 0, 32)
	for _, p := range active {
		for _, m := range s.modelsFor(p.ID) {
			id := m.ID
			if tag[p.ID] {
				id = intlModelPrefix + ":" + m.ID
			}
			out = append(out, modelEntry{
				Model: openai.Model{
					ID:              id,
					Object:          "model",
					Created:         m.Created,
					OwnedBy:         m.OwnedBy,
					Provider:        p.ID,
					DisplayName:     m.Name,
					ContextLength:   m.ContextLength,
					MaxOutputTokens: m.MaxOutputTokens,
					SupportsImages:  m.SupportsImages,
				},
				profileID:  p.ID,
				upstreamID: m.ID,
			})
		}
	}
	return out
}

// AllModels 汇总所有已登录上游的模型。
func (s *Server) AllModels() []openai.Model {
	entries := s.modelEntries()
	out := make([]openai.Model, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Model)
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
//
// 只处理已登录的上游，并且合并该上游下所有账号返回的模型：同一个版本
// （国内版 / 国际版）可以登录多个账号，各账号可用模型未必完全一致，
// 只取第一个账号会把其他账号的模型漏掉。
// 未登录的上游清空缓存，避免账号删除后旧模型残留在列表里。
//
// 手动点同步、保存设置、登录后、周期任务都可能同时触发。这里做串行化：
// 同一时刻只跑一轮，后来的直接跳过——重复同步除了浪费上游配额，
// 还会互相争抢账号锁，把正在服务的请求堵在门外。
func (s *Server) RefreshModels(ctx context.Context) {
	if !s.syncMu.TryLock() {
		s.log.Infof("已有模型同步在进行中，跳过本次触发")
		return
	}
	defer s.syncMu.Unlock()

	cfg := s.Config()
	for _, p := range cfg.EnabledProfiles() {
		client, err := s.ClientFor(p.ID)
		if err != nil {
			continue
		}
		creds, err := s.loadCreds(p.ID)
		if err != nil {
			s.log.Warnf("读取 %s 凭据失败: %v", p.ID, err)
		}
		if len(creds) == 0 {
			s.setModels(p.ID, nil)
			s.InvalidateAccounts()
			continue
		}

		var upstreamModels []upstream.UpstreamModel
		seen := map[string]bool{}
		if p.SyncModels == nil || *p.SyncModels {
			for _, cred := range creds {
				if err := client.EnsureToken(ctx, cred); err != nil {
					s.log.Warnf("账号 %s 令牌不可用，跳过其模型同步: %v", cred.DisplayName(), err)
					continue
				}
				models, err := client.FetchUpstreamModels(ctx, cred)
				if err != nil {
					s.log.Warnf("同步 %s 账号 %s 的模型失败: %v", p.ID, cred.DisplayName(), err)
					continue
				}
				// 只在落盘这一步持账号锁：把模型同步的网络 I/O（可能数秒）挡在锁外，
				// 否则正在服务该账号的聊天请求会被堵在门外。锁只在亚毫秒级的磁盘写期间持有。
				// 代价是刷新令牌与并发聊天刷新可能重复一次（幂等，可忽略）。
				unlock := s.lockCredential(cred.ID)
				if s.store != nil {
					_ = s.store.SavePreservingLocalFields(cred)
				}
				s.updateCachedCred(cred)
				unlock()
				for _, m := range models {
					if m.ID == "" || seen[m.ID] {
						continue
					}
					seen[m.ID] = true
					upstreamModels = append(upstreamModels, m)
				}
			}
		}
		models := client.BuildModelList(p.ExtraModels, p.DisabledModels, upstreamModels)
		s.setModels(p.ID, models)
		s.log.Infof("上游 %s 可用模型 %d 个（%d 个账号）", p.ID, len(models), len(creds))
	}
	s.InvalidateAccounts()
}

const (
	// modelSyncTimeout 限制单轮同步的最长耗时。
	modelSyncTimeout = 2 * time.Minute
	// syncIntervalPoll 是「定时同步已关闭」时的轮询间隔：用户把周期改回非 0 后，
	// 最多等这么久就能生效。必须保留这个轮询而不能直接退出协程，
	// 否则关一次同步就等于永久关掉，除非重启程序。
	syncIntervalPoll = 30 * time.Second
)

// refreshModelsWithTimeout 跑一轮模型同步；单轮超时，且随 stop 立即取消。
func (s *Server) refreshModelsWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, modelSyncTimeout)
	defer cancel()
	s.RefreshModels(ctx)
}

// modelSyncLoop 定期同步上游模型列表。
//
// 三处容易做错的地方都在这里：
//   - 同步周期每次醒来都重新读配置，设置页改周期后不需要重启程序；
//   - 周期配置为 0（关闭同步）时不退出协程，只是原地等待，改回来仍然生效；
//   - 同步用的 context 挂在 stop 上，Stop() 不必干等一轮同步跑完
//     （否则最长要卡 2 分钟才退出）。
func (s *Server) modelSyncLoop(stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			cancel()
		case <-done:
		}
	}()

	s.refreshModelsWithTimeout(ctx)

	for {
		interval := s.Config().SyncInterval()
		if interval <= 0 {
			interval = syncIntervalPoll
		}
		timer := time.NewTimer(interval)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.refreshModelsWithTimeout(ctx)
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
	p := strings.TrimSuffix(r.URL.Path, "/")
	// 容错：不少客户端会把 base_url 配成 http://host/v1/models 之类，
	// 再在后面拼接 /chat/completions。只要尾巴对得上就直接按对应端点处理。
	if strings.HasSuffix(p, "/chat/completions") && r.Method == http.MethodPost {
		s.handleChat(w, r)
		return
	}
	if strings.HasSuffix(p, "/models") && r.Method == http.MethodGet {
		s.handleModels(w, r)
		return
	}
	if strings.HasSuffix(p, "/chat/completions") || strings.HasSuffix(p, "/models") {
		writeError(w, http.StatusMethodNotAllowed, "该端点不支持 "+r.Method+" 请求", "invalid_request_error", "method_not_allowed")
		return
	}
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound,
			"未知路径: "+r.URL.Path+"（提示：base_url 应填 "+s.Config().OpenAIBaseURL()+"，不要带上 /models 或 /chat/completions）",
			"invalid_request_error", "not_found")
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
		status := http.StatusBadRequest
		// 「没登录 / 全暂停」是鉴权问题，不是模型名写错，返回 401 更贴合客户端处理。
		if errors.Is(err, ErrNoAccount) || errors.Is(err, ErrAllPaused) {
			status = http.StatusUnauthorized
		}
		writeError(w, status, err.Error(), "invalid_request_error", "model_not_found")
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
		s.writeChatError(w, err, profile.ID, requestedModel)
		return
	}
	defer resp.Body.Close()

	// 上游可能带着 text/event-stream 的 Content-Type 回一个错误响应。
	// 这时绝不能进 SSE 分支：一律写 200 会让客户端以为请求成功，
	// 而真正的错误信息被当成正常增量吐了出去。
	if resp.StatusCode >= 400 {
		s.writeNonStreamUpstream(w, resp, requestedModel, profile.ID, started)
		return
	}

	if !isSSE(resp) {
		// 上游以 200 + 非 SSE 形态返回（罕见）：writeNonStreamUpstream 透传，
		// 并在带 usage 时记录流量。
		s.writeNonStreamUpstream(w, resp, requestedModel, profile.ID, started)
		return
	}

	if wantStream {
		// 流式：边透传边抽取 usage，请求正常结束后计入统计。
		usage := s.pipeSSE(w, resp)
		s.recordUsage(profile.ID, requestedModel, usage)
		s.log.Infof("[%s] %s 流式转发完成，耗时 %s（账号 %s）", profile.ID, requestedModel, time.Since(started).Round(time.Millisecond), cred.DisplayName())
		return
	}

	// 非流式：把上游的流折叠成一个完整响应。
	//
	// 折叠本身也可能失败——上游会以 200 + SSE 的形式回报额度耗尽 / 限流
	// （状态码完全正常，错误只出现在流里）。这时必须像 HTTP 层失败那样
	// 换同组下一个账号重来，否则组里明明还有能用的账号，请求却直接失败了。
	result, cred, err := s.aggregateWithFailover(r.Context(), profile.ID, resp, cred, body, r, requestedModel, upstreamModel)
	if err != nil {
		s.writeChatError(w, err, profile.ID, requestedModel)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
	// 折叠后的完整响应里带着 usage，提取并计入流量统计。
	if u, ok := extractUsageString(string(result)); ok {
		s.recordUsage(profile.ID, requestedModel, u)
	}
	s.log.Infof("[%s] %s 非流式转发完成，耗时 %s（账号 %s）", profile.ID, requestedModel, time.Since(started).Round(time.Millisecond), cred.DisplayName())
}

// aggregateRetries 是非流式请求因流内错误而换号重试的上限。
//
// 不做成无限：账号数量有限，每轮 doChatExcluding 至少会排除一个账号，
// 所以轮数只是防止逻辑意外时无限循环的兜底。
const aggregateRetries = 3

// aggregateWithFailover 聚合上游的流；流里夹带错误时冷却该账号并换号重试。
//
// 与 doChat 的 HTTP 层换号是同一套冷却/退避机制：这里失败同样要让账号
// 退避，否则一个额度耗尽的账号会被反复选中，每次都白等一次聚合。
//
// resp/cred 是首轮 doChat 的结果；换号后由本函数持有并负责关闭旧响应体，
// 调用方只负责 defer 关闭最初那个。
func (s *Server) aggregateWithFailover(
	ctx context.Context,
	providerID string,
	resp *http.Response,
	cred *upstream.Credential,
	body []byte,
	r *http.Request,
	requestedModel, upstreamModel string,
) ([]byte, *upstream.Credential, error) {
	exclude := map[string]bool{}

	for attempt := 0; ; attempt++ {
		// 每轮都负责关掉自己读的那个 body。
		//
		// 调用方的 defer 只捕获了最初那个 resp：换号后拿到的响应体如果
		// 不在这里关掉，连接就永远不会归还连接池。非流式 + 换号是常见组合
		// （额度用尽时重试），泄漏会随请求数持续累积。
		result, err := func() ([]byte, error) {
			defer resp.Body.Close()
			return openai.Aggregate(resp.Body, upstreamModel)
		}()
		if err == nil {
			return result, cred, nil
		}

		// 只对「上游明确报错」换号。读取流失败（网络中断等）重试没有意义，
		// 换号重来只会把一次抖动放大成多次。
		var serr *openai.StreamError
		if !errors.As(err, &serr) || cred == nil {
			return nil, cred, err
		}
		if kind := classifyMessage(serr.Message); kind != failNone {
			until := s.markCooldown(cred.ID, kind, 0)
			s.log.Warnf("账号 %s %s，冷却至 %s（%s）", cred.DisplayName(), failReason(kind),
				until.Format("15:04:05"), truncate(serr.Message, 120))
		}
		exclude[cred.ID] = true
		s.log.Warnf("[%s] %s 上游流中返回错误，换号重试: %s", providerID, requestedModel, truncate(serr.Message, 200))
		if attempt >= aggregateRetries-1 {
			return nil, cred, &openai.StreamError{Message: "上游返回错误: " + serr.Message}
		}

		next, nextCred, err := s.doChatExcluding(ctx, providerID, body, r, exclude)
		if err != nil {
			// 组内没有别的账号可换时，doChatExcluding 会返回 ErrNoAccount。
			// 直接透传会把「额度已用尽」说成「尚未登录账号」，用户会去
			// 反复检查登录状态，而真正的原因是额度或限流——原始文案更有用。
			if errors.Is(err, ErrNoAccount) || errors.Is(err, ErrAllPaused) {
				return nil, cred, &openai.StreamError{Message: "上游返回错误: " + serr.Message}
			}
			// 其它错误（如整组冷却、组内账号都不健康）比原始文案更贴近实际。
			return nil, cred, err
		}
		// 换号成功不代表请求成功：新账号可能回的是 400 / 404 / 422 这类
		// 客户端错误（模型在别的账号上没开、内容超长等），doChatExcluding
		// 对这类错误不做退避，直接把响应交回来。
		//
		// 必须把上游的真实错误原样带回去——这里绝不能 `return nil, cred, err`，
		// 因为 err 在 doChatExcluding 成功时已被置成 nil，调用方只判 err != nil，
		// 会写出「200 + 空 body」：客户端以为请求成功，拿到空响应后只能报一句
		// 无头无尾的解析错误。用 upstreamHTTPError 把状态码和文案一起带回去，
		// writeChatError 会按原状态码（保留 4xx）回给下游。
		if next.StatusCode >= 400 {
			kind, detail, retryAfter := inspectUpstreamFailure(next)
			_ = next.Body.Close()
			if kind != failNone && nextCred != nil {
				until := s.markCooldown(nextCred.ID, kind, retryAfter)
				s.log.Warnf("账号 %s %s，冷却至 %s（%s）", nextCred.DisplayName(), failReason(kind),
					until.Format("15:04:05"), truncate(detail, 120))
			}
			msg := strings.TrimSpace(detail)
			if msg == "" {
				msg = fmt.Sprintf("上游 HTTP %d", next.StatusCode)
			}
			return nil, cred, &upstreamHTTPError{Status: next.StatusCode, Message: msg}
		}
		resp, cred = next, nextCred
	}
}

// writeChatError 把转发阶段的错误映射成合适的 HTTP 状态码。
func (s *Server) writeChatError(w http.ResponseWriter, err error, providerID, requestedModel string) {
	s.log.Errorf("[%s] %s 转发失败: %v", providerID, requestedModel, err)
	// 换号后新账号回的 4xx 要按原状态码回给下游（模型没权限、内容超长等），
	// 不能一律压成 502——否则调用方会把「客户端错误」当成「可重试的临时故障」。
	if uhe, ok := err.(*upstreamHTTPError); ok {
		writeError(w, uhe.Status, uhe.Message, "upstream_error", "upstream_error")
		return
	}
	status := http.StatusBadGateway
	var fail *upstreamFailError
	switch {
	case errors.As(err, &fail):
		// 整组账号都在限流 / 额度冷却中：这是可重试的，按 429 回给客户端，
		// 并带上 Retry-After，让 SDK 自己退避比瞎撞更合适。
		status = http.StatusTooManyRequests
		if fail.retryAfter > 0 {
			secs := int(fail.retryAfter.Seconds())
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
	case errors.Is(err, ErrNoAccount), errors.Is(err, ErrAllPaused):
		status = http.StatusUnauthorized
	}
	writeError(w, status, err.Error(), "upstream_error", "upstream_error")
}

// writeNonStreamUpstream 处理上游返回非 SSE 响应的情况：正常响应透传，错误响应转成 OpenAI 错误。
//
// 上游偶尔给错误响应也带上 text/event-stream 的 Content-Type，
// 这里同样要摘出首个 data 事件里的错误信息，不能把 "data: {...}" 原样透传给客户端。
//
// 关键：上游明确返回成功（< 400）时，无论是不是我们预期的 JSON 形态，都原样透传——
// 代理应当保持透明，绝不能把上游的 200 改写成 502。先前只有「带 choices 的 JSON」
// 才透传，于是上游的 200 + 其它合法 JSON 形态（甚至非 choices 的正常应答）会被误判成
// 错误、被强行改成 502，并把一整段原始 JSON 塞进 error.message 丢给下游。
func (s *Server) writeNonStreamUpstream(w http.ResponseWriter, resp *http.Response, model, provider string, started time.Time) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 400 {
		// 上游已表态成功：原样回传，Content-Type 也沿用上游的，
		// 只是兜底补一个 application/json 防止上游漏写导致客户端解析异常。
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		// 成功响应若带 usage（部分上游以非 SSE 形态返回完整 JSON），一并计入流量。
		if u, ok := extractUsageString(string(raw)); ok {
			s.recordUsage(provider, model, u)
		}
		return
	}

	message := strings.TrimSpace(string(raw))
	if isSSE(resp) {
		message = firstSSEErrorMessage(message)
	}
	// 上游的 JSON 错误通常是 {"error":{"message":"..."}} 形态，顶层没有 code/msg。
	// 不提取的话客户端会看到一整段原始 JSON，可读性很差。
	if !isSSE(resp) {
		var jerr struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &jerr) == nil && jerr.Error != nil && jerr.Error.Message != "" {
			message = jerr.Error.Message
		}
	}
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
	// 上游明确报的是客户端错误（400 / 404 / 422 …）时按原状态码回给下游：
	// 一律报 502 会让调用方以为这是可以重试的临时故障，从而反复重发同一个坏请求。
	// 5xx 属于上游临时故障，归一成 502 更贴合「网关」语义。
	status := resp.StatusCode
	if status >= 500 {
		status = http.StatusBadGateway
	}
	writeError(w, status, message, "upstream_error", "upstream_error")
}

// firstSSEErrorMessage 从 SSE 格式的错误正文里取出第一个可读的错误信息。
//
// 上游用 text/event-stream 回报错误时，正文是若干 "data: {json}" 行。
// 直接透传会让客户端看到带 "data:" 前缀的半截 JSON，这里还原成一句正常文案；
// 解析不出来时原样返回，交给调用方兜底。
func firstSSEErrorMessage(body string) string {
	var first string
	// ReadSSEEvents 只在回调返回错误时停止，所以用一个哨兵错误提前退出，
	// 不必把整个错误正文扫完。
	errStop := errors.New("stop")
	_ = openai.ReadSSEEvents(strings.NewReader(body), func(data string) error {
		first = data
		return errStop
	})
	if first == "" {
		return strings.TrimSpace(body)
	}
	// data 可能是 {"error":{"message":"..."}} 或 {"code":N,"msg":"..."}。
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(first), &envelope); err == nil {
		if envelope.Error != nil && envelope.Error.Message != "" {
			return envelope.Error.Message
		}
		if envelope.Msg != "" {
			if envelope.Code != 0 {
				return fmt.Sprintf("上游返回 code=%d msg=%s", envelope.Code, envelope.Msg)
			}
			return envelope.Msg
		}
	}
	return first
}

// pipeSSE 把上游的流式响应逐块透传给下游，并返回流里携带的 token 用量。
//
// 调用前必须确认响应确实是成功的流（状态码 < 400 且 Content-Type 是 SSE）：
// 这里会无条件写 200，一旦上游回的是错误响应，客户端就会以为请求成功。
func (s *Server) pipeSSE(w http.ResponseWriter, resp *http.Response) stats.Usage {
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
	var usage stats.Usage
	err := openai.ReadSSEEvents(resp.Body, func(data string) error {
		// 顺手从每个 data 事件里抽取 usage，最后一个带 usage 的事件覆盖先前的。
		if u, ok := extractUsageString(data); ok {
			usage = u
		}
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
	return usage
}

// -----------------------------------------------------------------------------
// 模型解析与账号选择
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// 模型命名与路由别名
// -----------------------------------------------------------------------------

const (
	// intlModelPrefix 是国际版模型在多上游场景下的前缀；
	// 国内版不使用前缀（裸模型名默认路由到国内版）。
	intlModelPrefix = "intl"
	// intlProviderID 是国际版的上游 ID。
	intlProviderID = "workbuddy"
)

// profileByIDOrAlias 按上游 ID 解析配置，兼容国际版的 "intl" 别名。
func (s *Server) profileByIDOrAlias(cfg *config.Config, id string) (config.Profile, bool) {
	if p, ok := cfg.ProfileByID(id); ok {
		return p, true
	}
	if id == intlModelPrefix {
		return cfg.ProfileByID(intlProviderID)
	}
	return config.Profile{}, false
}

// resolveModel 把客户端传来的模型名映射到具体上游。
//
// 支持三种写法：
//  1. 直接写模型名（按已启用上游的模型列表匹配）
//  2. "上游ID:模型名" 或 "上游ID/模型名" 强制指定（国际版可用 "intl:模型名"）
//  3. 请求头 X-RapidProxy-Provider 或查询参数 ?provider= 强制指定（同样兼容 intl）
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
		if p, ok := s.profileByIDOrAlias(cfg, model[:idx]); ok && p.Enabled {
			forced = p.ID
			model = model[idx+1:]
		}
	}
	if forced != "" {
		profile, ok := s.profileByIDOrAlias(cfg, forced)
		if !ok {
			return config.Profile{}, "", fmt.Errorf("未知的上游: %s", forced)
		}
		if !profile.Enabled {
			return config.Profile{}, "", fmt.Errorf("上游 %s 未启用", forced)
		}
		if !s.hasCredential(profile.ID) {
			return config.Profile{}, "", fmt.Errorf("上游 %s 还没有登录账号，请先登录", profile.ID)
		}
		return profile, model, nil
	}

	// 裸模型名只在「对外暴露的模型名」里精确匹配。
	//
	// 不能用各上游的原始模型名去匹配：国内版与国际版有一批同名模型
	// （minimax-m3、glm-5.3、kimi-k2.7 …），按上游顺序逐个匹配会把裸名
	// 固定打到排在最前面的那个版本上，与列表里展示的归属不一致。
	// 以 modelEntries 为准才能保证「列表里看到哪个 ID，就转发到哪个版本」。
	for _, e := range s.modelEntries() {
		if e.ID != model {
			continue
		}
		profile, ok := cfg.ProfileByID(e.profileID)
		if !ok || !profile.Enabled {
			continue
		}
		return profile, e.upstreamID, nil
	}
	// 未匹配到具体上游时交给第一个已登录的上游，避免客户端因别名而失败。
	active := s.loggedInProfiles()
	if len(active) == 0 {
		return config.Profile{}, "", ErrNoAccount
	}
	s.log.Warnf("模型 %s 不在已知列表中，按默认上游 %s 转发", model, active[0].ID)
	return active[0], model, nil
}

func (s *Server) lockCredential(id string) func() {
	value, _ := s.credLocks.LoadOrStore(id, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// UpdateCredential 在账号锁内读-改-写一份凭据。
//
// 不能在外面「store.Get → 改字段 → store.Save」：doChat 每请求都可能刷新令牌
// 并落盘，界面上的暂停 / 改权重如果同时写回自己读到的旧副本，
// 就会把刚刷新的令牌覆盖掉（反过来也一样），于是账号莫名 401。
// 这里复用 doChat 那把 per-credential 锁，两边就串行了。
func (s *Server) UpdateCredential(provider, id string, mutate func(*upstream.Credential) error) error {
	if mutate == nil {
		return errors.New("缺少更新逻辑")
	}
	if s.store == nil {
		return errors.New("凭据仓库不可用")
	}
	unlock := s.lockCredential(id)
	defer unlock()

	cred, err := s.store.Get(provider, id)
	if err != nil {
		return err
	}
	if err := mutate(cred); err != nil {
		return err
	}
	if err := s.store.Save(cred); err != nil {
		return err
	}
	s.updateCachedCred(cred)
	// 运行时改了权重/暂停：清掉该账号的 SWRR 中间偏移，避免旧偏移（按旧权重累积）
	// 在新权重下造成短暂的分配倾斜。清零后下一轮会按新权重重新收敛。
	balKey := provider + "/" + id
	s.balMu.Lock()
	if s.balState != nil {
		s.balState[balKey] = 0
	}
	s.balMu.Unlock()
	return nil
}

// -----------------------------------------------------------------------------
// 限流 / 额度冷却
// -----------------------------------------------------------------------------

// 冷却时长：上游抖动是秒级的，限流是分钟级的，
// 额度耗尽短期重试没有意义，停得更久。
const (
	serverCooldownBase    = 2 * time.Second
	serverCooldownMax     = 1 * time.Minute
	rateLimitCooldownBase = 30 * time.Second
	rateLimitCooldownMax  = 10 * time.Minute
	creditCooldownBase    = 1 * time.Hour
	creditCooldownMax     = 6 * time.Hour
	// cooldownCeiling 是 Retry-After 能被采纳的上限，避免上游给个荒谬的值把账号锁死。
	cooldownCeiling = 24 * time.Hour
)

// cooldownState 是一个账号的冷却状态与连续失败计数。
//
// 三种原因各记各的连续次数：账号先是额度用尽、后来又被限流，
// 不该沿用额度那套小时级的退避。
type cooldownState struct {
	Until      time.Time
	RateHits   int // 连续限流次数
	CreditHits int // 连续额度耗尽次数
	ServerHits int // 连续上游不可用次数
}

// failKind 区分上游失败原因，决定账号要冷却多久。
type failKind int

const (
	failNone      failKind = iota
	failServer             // 上游 502/503/504：通常是暂时的，短暂冷却后即可恢复
	failRateLimit          // 限流：短暂冷却后即可恢复
	failCredit             // 额度 / 余额耗尽：短时间内重试没有意义，冷却更久
)

// creditKeywords 与 rateLimitKeywords 用于从错误正文里辨认失败原因：
// 上游并不总用标准状态码，配额类错误常裹在 400 / 403 里。
var creditKeywords = []string{
	"insufficient_quota", "insufficient quota", "quota_exceeded", "quota exceeded",
	"out of credits", "no credits", "insufficient credits", "credit balance",
	"insufficient_balance", "insufficient balance", "credits exhausted",
	"额度", "余额不足", "配额", "用量已用尽", "计费",
}

var rateLimitKeywords = []string{
	"rate limit", "rate_limit", "rate-limit", "ratelimit",
	"too many requests", "too_many_requests",
	"限流", "请求过于频繁", "频率",
}

// serverKeywords 辨认「上游暂时不可用」：网关类错误未必用标准状态码。
var serverKeywords = []string{
	"service unavailable", "service_unavailable", "bad gateway",
	"gateway timeout", "overloaded", "capacity", "temporarily unavailable",
	"服务不可用", "服务繁忙", "上游不可用", "暂时不可用", "稍后重试",
}

// inspectUpstreamFailure 判断一次上游失败是不是限流、额度耗尽或上游不可用。
//
// 只在状态码 >= 400 时才有意义：此时响应还没交给下游，body 可以安全读取。
// 上游偶尔会用 text/event-stream 的 Content-Type 回一个错误响应，
// 这种 body 同样要读出来判断，否则限流不会触发换号，客户端还会收到一个 200。
// 返回 failNone 表示这不是可退避的错误（按原有逻辑处理，例如认证失败）；
// 这时读过的 body 会被塞回去，不影响后续处理。
func inspectUpstreamFailure(resp *http.Response) (failKind, string, time.Duration) {
	if resp == nil || resp.StatusCode < 400 {
		return failNone, "", 0
	}
	// 上限与 writeNonStreamUpstream 的 8MB 对齐：failNone 分支要把 body 原样
	// 还回去交给下游透传，若读到的比下游能读到的还少，大体积错误正文（>64KB）
	// 就会被截断，甚至因截断成非法 JSON 而被下游当成「上游 HTTP 400」丢失原意。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	msg := strings.TrimSpace(string(raw))
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))

	kind := failNone
	switch resp.StatusCode {
	case http.StatusPaymentRequired:
		kind = failCredit
	case http.StatusTooManyRequests:
		kind = failRateLimit
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		// 上游自己挂了：多半是暂时的，换同组别的账号试试。
		kind = failServer
	default:
		kind = classifyMessage(msg)
	}
	if kind == failNone {
		// 不是可退避的错误，把完整 body 还回去供后续逻辑读取。
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return failNone, msg, 0
	}
	return kind, truncate(msg, 200), retryAfter
}

// hasAny 报告正文里是否出现任一关键词（调用方负责先把大小写统一）。
func hasAny(text string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// classifyMessage 只凭错误文案判断失败原因。
//
// 用于「拿不到可靠状态码」的场景：上游把额度 / 限流裹在 400 里，
// 或者以 200 + SSE 的形式回报（状态码根本看不出失败）。
// 调用方需先把文案转成小写；额度优先于限流——「额度不足，触发了限流」
// 这类文案里真正该做的是长时间冷却而不是退避几十秒。
func classifyMessage(msg string) failKind {
	lower := strings.ToLower(msg)
	switch {
	case hasAny(lower, creditKeywords):
		return failCredit
	case hasAny(lower, rateLimitKeywords):
		return failRateLimit
	case hasAny(lower, serverKeywords):
		return failServer
	}
	return failNone
}

// failReason 返回失败原因的中文描述，供日志使用。
func failReason(kind failKind) string {
	switch kind {
	case failCredit:
		return "额度已用尽"
	case failServer:
		return "上游暂时不可用"
	default:
		return "被限流"
	}
}

// parseRetryAfter 解析 Retry-After 头（秒数或 HTTP 日期），失败返回 0。
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// markCooldown 记录一次限流或额度耗尽，返回冷却截止时间。
//
// 同一个原因连续触发时按指数退避延长，避免把账号打得更死；
// 上游给了明确的 Retry-After 时以其为准（受 cooldownCeiling 限制）。
func (s *Server) markCooldown(credID string, kind failKind, retryAfter time.Duration) time.Time {
	s.coolMu.Lock()
	defer s.coolMu.Unlock()
	if s.cool == nil {
		s.cool = map[string]*cooldownState{}
	}
	st, ok := s.cool[credID]
	if !ok {
		st = &cooldownState{}
		s.cool[credID] = st
	}

	var base, max time.Duration
	var hits int
	switch kind {
	case failCredit:
		st.CreditHits++
		st.RateHits, st.ServerHits = 0, 0
		base, max, hits = creditCooldownBase, creditCooldownMax, st.CreditHits
	case failServer:
		st.ServerHits++
		st.RateHits, st.CreditHits = 0, 0
		base, max, hits = serverCooldownBase, serverCooldownMax, st.ServerHits
	default:
		st.RateHits++
		st.CreditHits, st.ServerHits = 0, 0
		base, max, hits = rateLimitCooldownBase, rateLimitCooldownMax, st.RateHits
	}
	d := base
	for i := 1; i < hits && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	if retryAfter > d {
		if retryAfter > cooldownCeiling {
			retryAfter = cooldownCeiling
		}
		d = retryAfter
	}
	st.Until = time.Now().Add(d)
	// 顺带回收过期条目。
	//
	// 必须无条件做：以前挂在 `len(s.cool) > 32` 之后，账号少于 33 个时
	// 这段代码永远不执行——用户删掉的账号其冷却状态会一直留在表里，
	// 而账号 ID 是 provider+uid，重新登录同一账号会凭空带着旧冷却。
	// 账号数是十几个量级，多扫一遍的成本可以忽略。
	now := time.Now()
	for id, c := range s.cool {
		if c.Until.Before(now) {
			delete(s.cool, id)
		}
	}
	return st.Until
}

// cooldownLeft 返回账号剩余冷却时间，0 表示未在冷却。
func (s *Server) cooldownLeft(credID string) time.Duration {
	s.coolMu.Lock()
	defer s.coolMu.Unlock()
	st, ok := s.cool[credID]
	if !ok {
		return 0
	}
	left := time.Until(st.Until)
	if left <= 0 {
		return 0
	}
	return left
}

// clearCooldown 在账号成功完成一次请求后清除其冷却与连续失败计数。
func (s *Server) clearCooldown(credID string) {
	s.coolMu.Lock()
	if s.cool != nil {
		delete(s.cool, credID)
	}
	s.coolMu.Unlock()
}

// CooldownInfo 返回账号的剩余冷却时间与原因（供界面展示）。
//
// 原因取值为 ""（未冷却）、"rate"（限流）、"credit"（额度耗尽）或
// "server"（上游暂时不可用）：额度耗尽以小时计，界面必须明确区分，
// 否则用户会以为账号坏了。
func (s *Server) CooldownInfo(credID string) (time.Duration, string) {
	s.coolMu.Lock()
	defer s.coolMu.Unlock()
	st, ok := s.cool[credID]
	if !ok {
		return 0, ""
	}
	left := time.Until(st.Until)
	if left <= 0 {
		// 冷却已过：顺手清掉，避免账号删除后条目永久留在表里。
		delete(s.cool, credID)
		return 0, ""
	}
	switch {
	case st.CreditHits > 0:
		return left, "credit"
	case st.ServerHits > 0:
		return left, "server"
	}
	return left, "rate"
}

// AccountStats 返回某上游的账号统计。
//
// available 是此刻能真正承接请求的账号数（排除已暂停与冷却中的），
// total 是该上游已登录的账号总数。模型列表据此展示「这个模型还有几个账号可用」。
func (s *Server) AccountStats(providerID string) (available, total int) {
	creds, err := s.loadCreds(providerID)
	if err != nil {
		return 0, 0
	}
	total = len(creds)
	for _, c := range creds {
		if c.Paused || s.cooldownLeft(c.ID) > 0 {
			continue
		}
		available++
	}
	return available, total
}

// pickCredential 按权重从账号里挑一个（平滑加权轮询）。
//
// 同一上游（同一版本）登录多个账号时，同名模型在这些账号之间按权重分摊：
// 权重 <=0 的账号按 1 处理，即组内默认同权；已暂停的账号不参与；
// 处于限流 / 额度冷却中的账号同样跳过，让流量自动落到还能用的账号上。
// exclude 里的账号不再考虑，供请求失败后依次换号重试。
func (s *Server) pickCredential(providerID string, creds []*upstream.Credential, exclude map[string]bool) *upstream.Credential {
	usable := make([]*upstream.Credential, 0, len(creds))
	total := 0
	for _, c := range creds {
		if c.Paused || exclude[c.ID] {
			continue
		}
		if s.cooldownLeft(c.ID) > 0 {
			continue
		}
		usable = append(usable, c)
		total += c.EffectiveWeight()
	}
	if len(usable) == 0 || total <= 0 {
		return nil
	}

	s.balMu.Lock()
	defer s.balMu.Unlock()
	if s.balState == nil {
		s.balState = map[string]int{}
	}
	best, bestCW := -1, 0
	for i, c := range usable {
		key := providerID + "/" + c.ID
		cw := s.balState[key] + c.EffectiveWeight()
		s.balState[key] = cw
		if best < 0 || cw > bestCW {
			best, bestCW = i, cw
		}
	}
	if best < 0 {
		return nil
	}
	chosen := usable[best]
	s.balState[providerID+"/"+chosen.ID] = bestCW - total
	return chosen
}

// pruneBalanceState 回收某上游下已不存在的账号的 SWRR 中间权重。
//
// balState 只增不减地留着会有两个问题：账号删除后再登录同一 ID 会带着历史
// 偏移重新加入，短时间内连续中标、分配明显倾斜；条目也随登录过的账号数一直涨。
//
// 必须在「知道该上游完整账号列表」的地方调用，不能放在 pickCredential 里：
// 请求头 X-RapidProxy-Account 可以把流量钉在单个账号上，此时传入的候选集
// 只有它一个，拿这个当"存活集合"会把同组其它账号的偏移全清掉——一次调试
// 请求就毁掉整组的分摊状态。
//
// 组内账号数以个位数计，多扫一遍的成本可以忽略。
func (s *Server) pruneBalanceState(providerID string, all []*upstream.Credential) {
	if len(all) == 0 {
		return
	}
	live := make(map[string]bool, len(all))
	for _, c := range all {
		live[c.ID] = true
	}
	prefix := providerID + "/"
	s.balMu.Lock()
	defer s.balMu.Unlock()
	for key := range s.balState {
		if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
			continue
		}
		if !live[key[len(prefix):]] {
			delete(s.balState, key)
		}
	}
}

// doChat 转发请求；账号之间按权重分摊，认证失败先刷新令牌再换号重试。
func (s *Server) doChat(ctx context.Context, providerID string, body []byte, r *http.Request) (*http.Response, *upstream.Credential, error) {
	return s.doChatExcluding(ctx, providerID, body, r, nil)
}

// doChatExcluding 是 doChat 的可指定排除集版本。
//
// 非流式请求要把上游的 SSE 折叠成完整响应，这个过程本身还可能失败
// （流里夹带 error 事件），失败后需要换同组下一个账号重来。exclude 传入
// 上一轮已经用掉的账号，避免在同一账号上反复打转。
func (s *Server) doChatExcluding(ctx context.Context, providerID string, body []byte, r *http.Request, exclude map[string]bool) (*http.Response, *upstream.Credential, error) {
	client, err := s.ClientFor(providerID)
	if err != nil {
		return nil, nil, err
	}
	creds, err := s.loadCreds(providerID)
	if err != nil {
		return nil, nil, fmt.Errorf("读取凭据失败: %w", err)
	}
	// 回收该组里已删除账号的权重状态需要「该上游的完整账号列表」。
	// 这里必须拷贝一份：下面的 pinned 过滤用 creds[:0] 复用同一底层数组并 append，
	// 会改写共享的底层数组；若 all 只是同一个 slice header，过滤后 all 的内容
	// 就被污染了，pruneBalanceState 会把真正在线的账号误当成已删除而清掉其分摊状态。
	all := make([]*upstream.Credential, len(creds))
	copy(all, creds)
	// X-RapidProxy-Account 可以把请求钉在某个账号上（调试或多租户场景）。
	// 指定了却匹配不上时，报「该上游尚未登录账号」会让人以为是登录坏了，
	// 所以这里单独区分。
	pinned := false
	if want := strings.TrimSpace(r.Header.Get("X-RapidProxy-Account")); want != "" {
		pinned = true
		filtered := creds[:0]
		for _, c := range creds {
			if c.ID == want || c.Account.UID == want {
				filtered = append(filtered, c)
			}
		}
		if len(creds) > 0 && len(filtered) == 0 {
			return nil, nil, fmt.Errorf("上游 %s 没有 ID 或 UID 为 %q 的账号，请检查 X-RapidProxy-Account", providerID, want)
		}
		creds = filtered
	}
	s.pruneBalanceState(providerID, all)

	var paused int
	for _, c := range creds {
		if c.Paused {
			paused++
		}
	}
	switch {
	case len(creds) == 0:
		return nil, nil, ErrNoAccount
	case paused == len(creds):
		if pinned {
			return nil, nil, fmt.Errorf("指定账号 %s 已暂停，请先恢复", creds[0].DisplayName())
		}
		return nil, nil, ErrAllPaused
	case paused > 0:
		s.log.Infof("上游 %s 有 %d/%d 个账号已暂停，跳过", providerID, paused, len(creds))
	}

	var lastErr error
	var backed int // 因限流 / 额度耗尽而退避的次数
	tried := make(map[string]bool, len(exclude))
	for id := range exclude {
		tried[id] = true
	}
	for {
		cred := s.pickCredential(providerID, creds, tried)
		if cred == nil {
			break
		}
		tried[cred.ID] = true
		unlock := s.lockCredential(cred.ID)

		beforeToken, beforeExpiry := cred.Auth.AccessToken, cred.Auth.ExpiresAt
		if err := client.EnsureToken(ctx, cred); err != nil {
			// 令牌不可用（从未认证或刷新失败）：这个账号本就无法完成请求，
			// 冷却掉它，避免每个请求都白白挑中它再失败一遍。
			s.markCooldown(cred.ID, failCredit, 0)
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

		// 限流 / 额度耗尽：先让这个账号冷却，再换同组下一个账号。
		// 只要组里还有能用的账号，请求就不会失败在下游面前。
		if kind, detail, retryAfter := inspectUpstreamFailure(resp); kind != failNone {
			_ = resp.Body.Close()
			until := s.markCooldown(cred.ID, kind, retryAfter)
			unlock()
			backed++
			lastErr = fmt.Errorf("账号 %s %s，冷却至 %s", cred.DisplayName(), failReason(kind), until.Format("15:04:05"))
			s.log.Warnf("账号 %s %s，冷却至 %s（%s）", cred.DisplayName(), failReason(kind),
				until.Format("15:04:05"), truncate(detail, 120))
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			_ = resp.Body.Close()
			if refreshErr := client.Refresh(ctx, cred); refreshErr != nil {
				// 刷新令牌也失败：刷新令牌已过期/吊销，这个账号彻底废了，
				// 冷却掉它，避免每个请求都重复挑中它做无谓的刷新尝试。
				s.markCooldown(cred.ID, failCredit, 0)
				unlock()
				lastErr = fmt.Errorf("账号 %s 认证失败且刷新令牌失败: %w", cred.DisplayName(), refreshErr)
				continue
			}
			retry, retryErr := client.Chat(ctx, cred, body)
			if retryErr != nil {
				if retry != nil {
					_ = retry.Body.Close()
				}
				unlock()
				lastErr = retryErr
				continue
			}
			// 刷新令牌后重试的响应也要走同一套失败判定：429/402/5xx 不能当成成功返回，
			// 否则该账号会继续被挑中并一直 429（且不该在非成功时落盘令牌）。
			if kind, detail, retryAfter := inspectUpstreamFailure(retry); kind != failNone {
				_ = retry.Body.Close()
				until := s.markCooldown(cred.ID, kind, retryAfter)
				unlock()
				backed++
				lastErr = fmt.Errorf("账号 %s %s，冷却至 %s", cred.DisplayName(), failReason(kind), until.Format("15:04:05"))
				s.log.Warnf("账号 %s %s，冷却至 %s（%s）", cred.DisplayName(), failReason(kind),
					until.Format("15:04:05"), truncate(detail, 120))
				continue
			}
			if retry.StatusCode == http.StatusUnauthorized || retry.StatusCode == http.StatusForbidden {
				_ = retry.Body.Close()
				unlock()
				lastErr = fmt.Errorf("账号 %s 认证失败（已尝试刷新令牌）", cred.DisplayName())
				continue
			}
			// 刷新令牌后重试成功：落盘新令牌并清掉冷却。
			if s.store != nil {
				_ = s.store.SavePreservingLocalFields(cred)
			}
			s.updateCachedCred(cred)
			s.clearCooldown(cred.ID)
			unlock()
			return retry, cred, nil
		}

		if cred.Auth.AccessToken != beforeToken || cred.Auth.ExpiresAt != beforeExpiry {
			if s.store != nil {
				_ = s.store.SavePreservingLocalFields(cred)
			}
			s.updateCachedCred(cred)
		}
		// 成功：清掉连续失败计数，让冷却时长回到起点。
		s.clearCooldown(cred.ID)
		unlock()
		return resp, cred, nil
	}

	// 走到这里说明没有任何账号能完成请求。
	// 全部因为限流 / 额度耗尽而退避时，告诉客户端多久后重试，而不是笼统报 502。
	if backed > 0 {
		if until, ok := s.earliestCooldown(creds); ok {
			wait := time.Until(until)
			if wait < 0 {
				wait = 0
			}
			return nil, nil, &upstreamFailError{
				msg:        fmt.Sprintf("该组所有账号都暂时不可用（限流或额度耗尽），最快 %s 后恢复（约 %s）", until.Format("15:04:05"), wait.Round(time.Second)),
				retryAfter: wait,
			}
		}
	}
	if lastErr == nil {
		lastErr = ErrNoAccount
	}
	return nil, nil, lastErr
}

// earliestCooldown 返回这组账号里最早的冷却截止时间。
func (s *Server) earliestCooldown(creds []*upstream.Credential) (time.Time, bool) {
	s.coolMu.Lock()
	defer s.coolMu.Unlock()
	var best time.Time
	for _, c := range creds {
		st, ok := s.cool[c.ID]
		if !ok {
			continue
		}
		if best.IsZero() || st.Until.Before(best) {
			best = st.Until
		}
	}
	return best, !best.IsZero()
}

// upstreamFailError 表示整组账号都暂时不可用，携带建议的重试等待时间。
type upstreamFailError struct {
	msg        string
	retryAfter time.Duration
}

func (e *upstreamFailError) Error() string { return e.msg }

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

// truncate 截断到前 n 个字符。
//
// 按字节切会把汉字切出半个 UTF-8 序列，日志和界面上就成了乱码，所以按 rune 计。
// 注意提前返回的条件也必须用 rune 数：拿字节长度去比会让「字符数没超、
// 字节数超了」的字符串白走一遍循环，结果虽然相同但没必要。
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
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
