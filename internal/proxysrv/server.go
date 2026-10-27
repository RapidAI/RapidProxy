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
	"bufio"
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
	"regexp"
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
//
// virtualName 非空时同时记入虚拟组维度（见 stats.Entry.Virtual）：真实维度
// 用于核对上游额度，虚拟维度用于观察别名维度的用量，两者各自累加。
func (s *Server) recordUsage(provider, model, virtualName string, u stats.Usage) {
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
		Virtual:  virtualName,
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
	// virtualName 非空表示这是一条虚拟组条目（此时 profileID/upstreamID 为空）。
	virtualName string
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
//
// 命名消歧规则（多上游同时登录时）：
//   - 国际版 workbuddy 的模型统一加 "intl:" 前缀（既有约定，裸名默认路由
//     到国内版）；
//   - 其它上游：每个裸模型名归属于第一个（配置顺序）暴露它的上游，其余
//     上游的同名模型改用 "上游ID:模型名" 前缀暴露，保证都能被显式寻址
//     （resolveModel 支持 "上游ID:模型名" 语法）；
//   - 只登录单个上游时全部保持原样，客户端直接填裸模型名。
func (s *Server) buildModelEntries() []modelEntry {
	active := s.loggedInProfiles()

	// 每个上游的模型列表只取一次：hasCredential → loadCreds 虽有 2 秒缓存，
	// 汇总在每次路由与 /v1/models 都会跑，仍要避免成倍的重复扫描。
	modelSets := make([][]upstream.ModelInfo, len(active))
	for i, p := range active {
		modelSets[i] = s.modelsFor(p.ID)
	}

	// 裸模型名的归属表：跳过多上游场景下整体加前缀的 workbuddy，
	// 让国内/国内系上游优先持有裸名。
	firstOwner := map[string]string{}
	if len(active) > 1 {
		for i, p := range active {
			if p.ID == intlProviderID {
				continue
			}
			for _, m := range modelSets[i] {
				if _, ok := firstOwner[m.ID]; !ok {
					firstOwner[m.ID] = p.ID
				}
			}
		}
	}

	out := make([]modelEntry, 0, 32)
	for i, p := range active {
		for _, m := range modelSets[i] {
			id := m.ID
			if len(active) > 1 {
				switch {
				case p.ID == intlProviderID:
					id = intlModelPrefix + ":" + m.ID
				case firstOwner[m.ID] != p.ID:
					id = p.ID + ":" + m.ID
				}
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
	out = append(out, s.virtualModelEntries(modelSets, active, out)...)
	return out
}

// virtualModelEntries 把已启用、且有可用成员的虚拟组转成模型条目。
//
// 只有「至少一个成员的上游已登录」时才暴露：未登录时列出一个必然失败的
// 模型名，只会让客户端在列表里看到一堆调不通的选项。这与真实模型
// 「未登录上游不暴露模型」的既有约定一致。
//
// 元信息取成员中最宽的那份（上下文最大、输出上限最大、任一支持图片即支持）。
//
// active/modelSets 是调用方已经取好的「已登录上游 + 各自模型列表」，
// 直接复用而不重新查：modelsFor 每次都走一遍凭据扫描，汇总在每次路由与
// /v1/models 都会跑，重复取等于把这项开销翻倍。
//
// taken 是已产出的真实模型条目：虚拟名与某个真实模型 ID 相同时跳过。
// 单上游场景下真实模型用裸名，与虚拟名撞车会让 /v1/models 出现重复 ID，
// 客户端（以及按 ID 去重的客户端缓存）会拿到两条互相矛盾的条目。
// 虚拟组本身仍然可路由——resolveRoute 里虚拟组优先，只是列表里不重复列。
func (s *Server) virtualModelEntries(modelSets [][]upstream.ModelInfo, active []config.Profile, taken []modelEntry) []modelEntry {
	cfg := s.Config()
	vms := cfg.EnabledVirtualModels()
	if len(vms) == 0 {
		return nil
	}
	used := make(map[string]bool, len(taken))
	for _, e := range taken {
		used[e.ID] = true
	}
	// 成员可能指向「已登录但全局停用」的上游（虚拟组允许这种用法），
	// 而 active/modelSets 只覆盖已启用的上游。这里补上那些上游的模型，
	// 否则它们的成员会被误判为「模型不存在」而被漏掉。
	index := make(map[string]upstream.ModelInfo, len(active)*8)
	addSet := func(p config.Profile, models []upstream.ModelInfo) {
		for _, m := range models {
			index[p.ID+"/"+m.ID] = m
		}
	}
	for i, p := range active {
		if i >= len(modelSets) {
			break
		}
		addSet(p, modelSets[i])
	}
	// 成员引用到的、但不在 active 里的上游（即未启用的），按需补查一次。
	// 只查真正被成员引用到的上游，不做全量扫描。
	for _, vm := range vms {
		for _, m := range vm.Members {
			if _, ok := index[m.Key()]; ok {
				continue
			}
			p, ok := cfg.ProfileByID(m.Provider)
			if !ok {
				continue
			}
			if !s.hasCredential(p.ID) {
				continue
			}
			addSet(p, s.modelsFor(p.ID))
		}
	}

	out := make([]modelEntry, 0, len(vms))
	for _, vm := range vms {
		if used[vm.Name] {
			continue
		}
		var best upstream.ModelInfo
		usable := 0
		for _, m := range vm.Members {
			info, ok := index[m.Key()]
			if !ok {
				continue
			}
			if usable == 0 {
				// 第一个可用成员提供基线（名称、创建时间等）。
				//
				// 不能每轮都整体赋值：后一个成员的较小值会把前面的较大值
				// 冲掉，最终结果取决于成员顺序而不是真实能力。
				best = info
			}
			usable++
			if info.ContextLength > best.ContextLength {
				best.ContextLength = info.ContextLength
			}
			if info.MaxOutputTokens > best.MaxOutputTokens {
				best.MaxOutputTokens = info.MaxOutputTokens
			}
			if info.SupportsImages {
				best.SupportsImages = true
			}
		}
		if usable == 0 {
			continue
		}
		out = append(out, modelEntry{
			Model: openai.Model{
				ID:       vm.Name,
				Object:   "model",
				Created:  best.Created,
				OwnedBy:  "virtual",
				Provider: "virtual",
				// DisplayName 只放组名本身：这是 API 响应字段，客户端可能
				// 直接展示，也可能按语言自行标注。在这里拼「（虚拟组）」
				// 会让英文界面的用户看到中文后缀。前端按 provider=virtual
				// 自行渲染本地化标签。
				DisplayName:     vm.Name,
				ContextLength:   best.ContextLength,
				MaxOutputTokens: best.MaxOutputTokens,
				SupportsImages:  best.SupportsImages,
			},
			virtualName: vm.Name,
		})
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

// hasCachedModels 报告某上游是否已有非空的模型缓存。
//
// 用于区分「首次同步还没成功」与「曾经成功、这次临时失败」：
// 前者用白名单兜底是合理的，后者必须保留旧数据。
func (s *Server) hasCachedModels(providerID string) bool {
	s.modelsMu.RLock()
	defer s.modelsMu.RUnlock()
	return len(s.models[providerID]) > 0
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

// LoggedInModels 返回「所有已登录上游」的模型，不区分上游是否全局启用。
//
// 与 AllModels 的区别：AllModels 只包含已启用上游的模型（那是对外暴露的
// 清单），而虚拟组编辑器需要看到「已登录但没启用」的上游也能用哪些模型——
// 否则用户明明登录了账号，却在组编辑器里选不到任何模型。
//
// 返回的 ID 是该上游的原始模型 ID（不带 "intl:" / "上游ID:" 前缀），
// 与 VirtualMember.Model 的存储形式一致。
func (s *Server) LoggedInModels(providerID string) []string {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return nil
	}
	cfg := s.Config()
	profile, ok := cfg.ProfileByID(providerID)
	if !ok || !s.hasCredential(providerID) {
		return nil
	}
	models := s.modelsFor(profile.ID)
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
	// 同步范围是「所有已登录账号的上游」，不只是已启用的。
	//
	// 虚拟组允许使用已登录但全局停用的上游（见 usableVirtualMembers），
	// 那些上游的模型若不同步，modelsFor 只能退回内置白名单——白名单可能
	// 缺少上游新上线的模型，也可能残留已下线的模型，用户在虚拟组编辑器里
	// 就会看到一份过时的清单。
	//
	// 未登录的上游仍然跳过：没有账号就无从拉取，白名单兜底已经够用。
	for _, p := range cfg.Profiles {
		if !s.hasCredential(p.ID) {
			// 之前有账号、现在没有：清掉旧缓存，避免账号删除后模型残留。
			s.setModels(p.ID, nil)
			continue
		}
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
		// 所有账号都没拉到模型时（上游故障、令牌全失效），不要把上一次成功
		// 同步的真实清单覆盖成纯白名单：上游只是临时不可达，模型并没有下线。
		// 覆盖会让虚拟组里引用了「白名单之外模型」的成员凭空变成不可用，
		// 用户看到的是「我的模型怎么没了」。
		if len(upstreamModels) == 0 && s.hasCachedModels(p.ID) {
			s.log.Warnf("上游 %s 本轮未取到模型，保留上次缓存", p.ID)
			continue
		}
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

	profile, upstreamModel, virtual, err := s.resolveRoute(requestedModel, r)
	if err != nil {
		status := http.StatusBadRequest
		// 「没登录 / 全暂停」是鉴权问题，不是模型名写错，返回 401 更贴合客户端处理。
		if errors.Is(err, ErrNoAccount) || errors.Is(err, ErrAllPaused) {
			status = http.StatusUnauthorized
		}
		writeError(w, status, err.Error(), "invalid_request_error", "model_not_found")
		return
	}

	started := time.Now()
	if virtual != nil {
		s.handleVirtualChat(w, r, payload, *virtual, requestedModel, wantStream, started)
		return
	}

	payload["model"] = upstreamModel
	body, err := s.prepareBody(payload, profile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "构造上游请求失败: "+err.Error(), "invalid_request_error", "invalid_body")
		return
	}

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
		//
		// 上游会以 200 + SSE 的形式回报额度耗尽 / 限流（状态码完全正常，
		// 错误在流的第一个事件里）。pipeSSEWithFailover 在写给下游任何字节
		// 之前先窥探首事件：是错误帧就冷却该账号、换同组下一个账号重来，
		// 组里还有能用的账号时客户端全程无感。
		usage, cred, err := s.pipeSSEWithFailover(r.Context(), w, profile.ID, resp, cred, body, r, requestedModel)
		if err != nil {
			// 只有在还没写给下游任何字节时才会走到这里，可以安全回错误码。
			s.writeChatError(w, err, profile.ID, requestedModel)
			return
		}
		s.recordUsage(profile.ID, requestedModel, "", usage)
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
		s.recordUsage(profile.ID, requestedModel, "", u)
	}
	s.log.Infof("[%s] %s 非流式转发完成，耗时 %s（账号 %s）", profile.ID, requestedModel, time.Since(started).Round(time.Millisecond), cred.DisplayName())
}

// prepareBody 按目标上游的协议族改写请求体。
//
// 必须按「每次转发实际使用的上游」分别调用，不能在进入转发前只准备一次：
// 虚拟组里的成员可能来自不同协议族，而两族的改写规则互相冲突——
// WorkBuddy 族要求首条 system 消息、stream_options 与模板句改写，
// Qoder 的签名体多一个未知字段都可能被上游拒绝。
func (s *Server) prepareBody(payload map[string]any, profile config.Profile) ([]byte, error) {
	// WorkBuddy 协议族要求首条 system 消息、stream_options 与模板句改写；
	// 其它协议族（lobsterai / trae / qoder）的请求体由各自适配层处理，
	// 这里保持客户端原样（仅强制流式）。强制思考档只对 hy3 系列有意义，
	// 同样限定在 WorkBuddy 协议族——qoder 的签名体多一个未知字段都可能
	// 被上游拒绝。
	kind := upstream.ProfileKind(profile)
	workBuddyFamily := kind == upstream.KindWorkBuddy
	// 智谱编程网关有两处硬性要求：developer 角色要写成 system、空的 user
	// 内容会被拒绝（需补占位符）。只在 OpenAI 线协议的智谱预设上做，避免
	// 影响其它上游。
	if kind == upstream.KindCustom && (profile.ID == "zhipu" || profile.ID == "zcode") &&
		!strings.EqualFold(strings.TrimSpace(profile.Protocol), "anthropic") {
		normalizeGLMCodingPayload(payload)
	}
	// 只有确认支持 stream_options 的 OpenAI 直连预设（xAI / ChatGPT）才显式
	// 索取 usage：智谱编程网关与用户自建端点可能拒绝该字段（aicoder 也采取
	// 「上游拒绝后停用」的策略），在关键路径上不冒险。Anthropic 线协议由翻译
	// 层自产 usage 帧，也不需要它。
	openAIUsage := profile.ID == "xai" || profile.ID == "chatgpt"
	body, _, err := openai.PrepareRequestBody(mustJSON(payload), openai.TransformOptions{
		Sanitize:           workBuddyFamily && s.Config().SanitizeEnabled(),
		ForceMaxThinking:   workBuddyFamily && s.Config().MaxThinkingEnabled(),
		EnsureSystemPrompt: workBuddyFamily,
		AddStreamOptions:   workBuddyFamily || openAIUsage,
	})
	return body, err
}

// virtualRetryLimit 返回一轮请求内最多尝试的成员数。
//
// 不写成固定常量：每轮循环至少排除一个成员，所以「成员数」本身就是
// 天然的终止条件。用固定值（早先是 6）会让成员多于 6 个的组永远试不到
// 后面的成员——排在尾部的成员等于白配。
//
// 仍留一道上限兜底，防止成员表异常（同一成员反复被选中）时死循环。
func virtualRetryLimit(memberCount int) int {
	const ceiling = 32
	if memberCount < 1 {
		return 1
	}
	if memberCount > ceiling {
		return ceiling
	}
	return memberCount
}

// handleVirtualChat 处理命中虚拟组的请求：在成员之间按权重分摊，
// 某个成员整体不可用时自动换下一个。
//
// 「成员整体不可用」比账号级失败更宽：它可能是该上游未登录、账号全暂停、
// 全部冷却，也可能是该上游没有这个模型。这几种情况对用户而言都是
// 「这个别名此刻走不通这条路」，换成员才是对的动作。
//
// 流式与非流式的差别只在最后一步：流式需要先窥探首事件确认上游没有
// 在流首报错，否则一旦写出字节就无法再换成员。
func (s *Server) handleVirtualChat(
	w http.ResponseWriter,
	r *http.Request,
	payload map[string]any,
	vm config.VirtualModel,
	requestedModel string,
	wantStream bool,
	started time.Time,
) {
	cfg := s.Config()
	// X-RapidProxy-Account 钉住的账号必须属于组内某个成员的上游，否则无论
	// 换到哪个成员都匹配不上。提前挡住并回 400，比让每个成员各打一次上游、
	// 最后返回一个 502 更准确——这是客户端传错了参数，不是上游故障。
	if want := strings.TrimSpace(r.Header.Get("X-RapidProxy-Account")); want != "" {
		if !s.virtualGroupHasAccount(cfg, &vm, want) {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("虚拟模型 %s 的成员里没有 ID 或 UID 为 %q 的账号，请检查 X-RapidProxy-Account", vm.Name, want),
				"invalid_request_error", "invalid_account")
			return
		}
	}
	exclude := map[string]bool{}
	var lastErr error
	var clientErr *upstreamHTTPError
	limit := virtualRetryLimit(len(vm.Members))

	for attempt := 0; attempt < limit; attempt++ {
		members := s.usableVirtualMembers(cfg, &vm, exclude)
		if len(members) == 0 {
			break
		}
		member, ok := s.pickVirtualMember(vm.Name, members, nil)
		if !ok {
			break
		}
		exclude[member.Key()] = true

		profile, ok := cfg.ProfileByID(member.Provider)
		if !ok {
			continue
		}
		// 每次换成员都要按该成员的协议族重新准备请求体（见 prepareBody）。
		payload["model"] = member.Model
		body, err := s.prepareBody(payload, profile)
		if err != nil {
			writeError(w, http.StatusBadRequest, "构造上游请求失败: "+err.Error(), "invalid_request_error", "invalid_body")
			return
		}

		resp, cred, err := s.doChat(r.Context(), profile.ID, body, r)
		if err != nil {
			lastErr = err
			// 「该上游没有这个模型」这类客户端错误留着兜底带回：
			// 全部成员都接不了时，它的文案最能说明问题。
			var uhe *upstreamHTTPError
			if errors.As(err, &uhe) {
				clientErr = uhe
			}
			s.log.Warnf("[虚拟 %s] 成员 %s 不可用，换下一个: %v", vm.Name, member.Key(), err)
			continue
		}

		// 防御性分支：doChatExcluding 的契约是「>= 400 都在组内跳过换号」，
		// 只在全部账号都无法承接时返回错误，因此正常路径下这里拿到的 resp
		// 一定 < 400。真出现时按「该成员接不了这单」处理：换下一个成员，
		// 并把上游的真实错误留着兜底带回。
		//
		// 这里刻意不调 markCooldown：限流 / 额度的冷却已在 doChatExcluding
		// 内部按账号记过一次，再记一次会让指数退避连加两级，把账号打得更死。
		if resp.StatusCode >= 400 {
			_, detail, _ := inspectUpstreamFailure(resp)
			_ = resp.Body.Close()
			clientErr = &upstreamHTTPError{Status: resp.StatusCode, Message: detail}
			lastErr = clientErr
			s.log.Warnf("[虚拟 %s] 成员 %s 被上游拒绝（HTTP %d），换下一个: %s",
				vm.Name, member.Key(), resp.StatusCode, truncate(detail, 120))
			continue
		}

		if !isSSE(resp) {
			// 上游以 200 + 非 SSE 形态返回：原样透传，带 usage 时记流量。
			// 非流式聚合与流式透传都会自己关闭 body。
			s.writeNonStreamUpstreamVirtual(w, resp, requestedModel, profile.ID, vm.Name, started)
			return
		}

		if wantStream {
			// pipeSSEWithFailover 内部负责关闭 resp.Body（含换号后的每一个响应体），
			// 这里不能再关一次：错误路径上它可能已经关过了。
			usage, _, err := s.pipeSSEWithFailover(r.Context(), w, profile.ID, resp, cred, body, r, requestedModel)
			if err != nil {
				lastErr = err
				s.log.Warnf("[虚拟 %s] 成员 %s 流首报错，换下一个: %v", vm.Name, member.Key(), err)
				continue
			}
			s.recordUsage(profile.ID, member.Model, vm.Name, usage)
			s.log.Infof("[虚拟 %s] %s → %s/%s 流式转发完成，耗时 %s（账号 %s）",
				vm.Name, requestedModel, profile.ID, member.Model,
				time.Since(started).Round(time.Millisecond), cred.DisplayName())
			return
		}

		// 同 aggregateWithFailover 的约定：它负责关闭自己读过的每一个 body。
		result, _, err := s.aggregateWithFailover(r.Context(), profile.ID, resp, cred, body, r, requestedModel, member.Model)
		if err != nil {
			lastErr = err
			s.log.Warnf("[虚拟 %s] 成员 %s 聚合失败，换下一个: %v", vm.Name, member.Key(), err)
			continue
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result)
		if u, ok := extractUsageString(string(result)); ok {
			s.recordUsage(profile.ID, member.Model, vm.Name, u)
		}
		s.log.Infof("[虚拟 %s] %s → %s/%s 非流式转发完成，耗时 %s（账号 %s）",
			vm.Name, requestedModel, profile.ID, member.Model,
			time.Since(started).Round(time.Millisecond), cred.DisplayName())
		return
	}

	// 组内没有任何成员能承接：优先带回「客户端错误」（模型没开通等），
	// 它的状态码与文案对排查最有用；否则按可退避错误处理。
	if clientErr != nil {
		s.writeChatError(w, clientErr, "虚拟:"+vm.Name, requestedModel)
		return
	}
	if lastErr == nil {
		// 一个成员都没试过就退出了：不是没登录，就是账号全暂停、全在冷却，
		// 或者组里根本没有成员。这些都不是「上游故障」，按 401 回更贴合
		// 客户端的处理方式（与 ErrNoAccount / ErrAllPaused 的口径一致），
		// 502 会让调用方误以为是可以重试的网关抖动而反复重发。
		lastErr = &virtualUnavailableError{name: vm.Name, reason: s.virtualUnavailableReason(cfg, &vm)}
	}
	s.writeChatError(w, lastErr, "虚拟:"+vm.Name, requestedModel)
}

// virtualUnavailableError 表示虚拟组此刻没有可承接请求的成员。
//
// 单独一个类型是为了让 writeChatError 能把它映射成 401 而不是 502：
// 「组里没人能接」是配置/登录状态问题，重试同一个请求不会变好。
type virtualUnavailableError struct {
	name   string
	reason string
}

func (e *virtualUnavailableError) Error() string {
	return fmt.Sprintf("虚拟模型 %s 暂时不可用：%s", e.name, e.reason)
}

// virtualGroupHasAccount 报告被钉住的账号是否属于组内某个成员的上游。
//
// 只查成员所属的上游：一个账号属于不在组里的上游时，钉它没有任何意义
// （那个上游的模型不在这个组里）。
func (s *Server) virtualGroupHasAccount(cfg *config.Config, vm *config.VirtualModel, want string) bool {
	checked := make(map[string]bool, len(vm.Members))
	for _, m := range vm.Members {
		if checked[m.Provider] {
			continue
		}
		checked[m.Provider] = true
		profile, ok := cfg.ProfileByID(m.Provider)
		if !ok || !profile.Enabled {
			continue
		}
		creds, err := s.loadCreds(m.Provider)
		if err != nil {
			continue
		}
		for _, c := range creds {
			if c.ID == want || c.Account.UID == want {
				return true
			}
		}
	}
	return false
}

// virtualUnavailableReason 给出一句能指导用户下一步动作的原因。
//
// 按「最可能是哪个环节断了」的顺序归类：上游没启用 → 没登录 → 账号不可用。
// 三种情况用户的处理动作完全不同，笼统一句「请检查配置」等于没说。
func (s *Server) virtualUnavailableReason(cfg *config.Config, vm *config.VirtualModel) string {
	if len(vm.Members) == 0 {
		return "该组还没有配置成员"
	}
	var disabled, noAccount, noUsable int
	for _, m := range vm.Members {
		profile, ok := cfg.ProfileByID(m.Provider)
		if !ok || !profile.Enabled {
			disabled++
			continue
		}
		creds, err := s.loadCreds(m.Provider)
		if err != nil || len(creds) == 0 {
			noAccount++
			continue
		}
		// 有账号但都不可用（全暂停 / 全在冷却）。
		if avail, _ := s.AccountStats(m.Provider); avail == 0 {
			noUsable++
		}
	}
	total := len(vm.Members)
	switch {
	case disabled == total:
		return "成员所属上游都未启用，请在设置中启用"
	case noAccount == total:
		return "成员所属上游还没有登录账号，请先登录"
	case noAccount+disabled+noUsable == total:
		return "成员所属上游的账号都已暂停或正在冷却，请稍后重试或恢复账号"
	default:
		return "请检查成员所属上游是否已启用并登录账号"
	}
}

// writeNonStreamUpstreamVirtual 与 writeNonStreamUpstream 相同，但把流量
// 同时记入虚拟组维度，并自己关闭响应体。
//
// 自己关是必须的：虚拟路径上没有 handleChat 那个 defer resp.Body.Close()
// 兜底（那里只认识最初的那个 resp，换成员后拿到的响应体不在它的范围内），
// 不关就会漏连接，随请求数持续累积。
func (s *Server) writeNonStreamUpstreamVirtual(w http.ResponseWriter, resp *http.Response, model, provider, virtual string, started time.Time) {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 400 {
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		if u, ok := extractUsageString(string(raw)); ok {
			s.recordUsage(provider, model, virtual, u)
		}
		return
	}
	// 走到这里说明成员已判定失败（调用方已按失败处理），不应再写响应。
	// 保留兜底：把上游的真实错误按原状态码带回。
	message := extractUpstreamErrorMessage(resp, raw)
	status := resp.StatusCode
	if status >= 500 {
		status = http.StatusBadGateway
	}
	s.log.Errorf("[%s] %s 上游异常: %s", provider, model, truncate(message, 200))
	writeError(w, status, message, "upstream_error", "upstream_error")
}

// aggregateRetries 是「上游以流的形式报错」时换号重试的上限，非流式
// （aggregateWithFailover）与流式（pipeSSEWithFailover）共用。
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
			// 流首错误帧与 HTTP 层同一口径：正文里带了额度窗口重置时刻
			// （code=6004 之类）就冷却到那个时刻，而不是几十秒的退避。
			until := s.markCooldown(cred.ID, kind, parseUsageResetAt(serr.Message))
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
			// 组内没有别的账号可换时，doChatExcluding 会返回 ErrNoAccount
			// （账号被删光 / 全暂停）。直接透传会把「额度已用尽」说成
			// 「尚未登录账号」，用户会去反复检查登录状态——原始文案更有用。
			if errors.Is(err, ErrNoAccount) || errors.Is(err, ErrAllPaused) {
				return nil, cred, &openai.StreamError{Message: "上游返回错误: " + serr.Message}
			}
			// 整组都在冷却（限流 / 额度）：按 429 + Retry-After 带回是对的，
			// 但组级汇总只说「暂时不可用」，用户看不出真正原因——把上游的
			// 原始错误拼回文案（原因在前，恢复时间在后），上限 500 字符
			// 防止病态长的错误文案翻倍灌进 error.message。
			var ufe *upstreamFailError
			if errors.As(err, &ufe) {
				return nil, cred, &upstreamFailError{
					msg:        "上游返回错误: " + truncate(serr.Message, 500) + "；" + ufe.msg,
					retryAfter: ufe.retryAfter,
				}
			}
			// 其它错误（如组内账号都不健康）比原始文案更贴近实际。
			return nil, cred, err
		}
		// 防御性分支：doChatExcluding 只会在状态码 < 400 时返回响应
		//（>= 400 的都在组内跳过换号了），正常情况下走不到这里。
		// 真出现时照样把上游的真实错误原样带回去——绝不能 return nil 且 err 为 nil，
		// 否则调用方会写出「200 + 空 body」：客户端以为请求成功，拿到空响应后
		// 只能报一句无头无尾的解析错误。
		if next.StatusCode >= 400 {
			kind, detail, retryAfter := inspectUpstreamFailure(next)
			_ = next.Body.Close()
			if kind != failNone && nextCred != nil {
				until := s.markCooldown(nextCred.ID, kind, retryAfter)
				s.log.Warnf("账号 %s %s，冷却至 %s（%s）", nextCred.DisplayName(), failReason(kind),
					until.Format("15:04:05"), truncate(detail, 120))
			}
			return nil, cred, &upstreamHTTPError{Status: next.StatusCode, Message: detail}
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
	var vunavail *virtualUnavailableError
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
	case errors.As(err, &vunavail):
		// 虚拟组里没人能接：与「未登录 / 全暂停」同属鉴权与配置问题，
		// 回 502 会被调用方当成可重试的网关抖动而反复重发同一个请求。
		status = http.StatusUnauthorized
	}
	writeError(w, status, err.Error(), "upstream_error", "upstream_error")
}

// maxUpstreamMessageRunes 限制带回给下游的上游错误文案长度。
//
// 上游错误体上限 8MB（见 inspectUpstreamFailure）：不截断的话，一个行为不端的
// 上游能把整段 HTML / JSON 塞进 error.message，客户端收到几 MB 的「错误」。
// 真正的错误文案都在几百字符以内，2000 足够保留可读信息。
const maxUpstreamMessageRunes = 2000

// extractUpstreamErrorMessage 从上游的错误响应体里提取一句可读文案。
//
// 上游的错误形态五花八门：OpenAI 的 {"error":{"message":...}}、信封式的
// {"code":N,"msg":...}、甚至带着 text/event-stream Content-Type 的 SSE 帧。
// 不提取的话客户端看到的是一整段原始 JSON 或带 "data:" 前缀的半截文本。
// raw 是已经读到的响应体（上限 8MB，见 inspectUpstreamFailure）；返回值
// 按 maxUpstreamMessageRunes 截断，分类用的关键词匹配在调用方基于原文进行，
// 不受截断影响。
func extractUpstreamErrorMessage(resp *http.Response, raw []byte) string {
	message := strings.TrimSpace(string(raw))
	if isSSE(resp) {
		return truncate(firstSSEErrorMessage(message), maxUpstreamMessageRunes)
	}
	// 上游的 JSON 错误通常是 {"error":{"message":"..."}} 形态，顶层没有 code/msg。
	var jerr struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &jerr) == nil && jerr.Error != nil && jerr.Error.Message != "" {
		message = jerr.Error.Message
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
		return truncate(fmt.Sprintf("上游返回 code=%d msg=%s", env.Code, env.Msg), maxUpstreamMessageRunes)
	}
	if message == "" {
		return fmt.Sprintf("上游 HTTP %d", resp.StatusCode)
	}
	return truncate(message, maxUpstreamMessageRunes)
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
			s.recordUsage(provider, model, "", u)
		}
		return
	}

	message := extractUpstreamErrorMessage(resp, raw)
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

// pipeSSEFrom 把上游的流式响应逐块透传给下游，并返回流里携带的 token 用量。
// first 非空表示首个事件已被预读（流式故障转移的窥探结果）：先转发它，
// 再从 br 继续读剩余流——客户端看到的仍是完整连续的流。
//
// 调用前必须确认响应确实是健康的流（状态码 < 400 且首事件不是错误帧）：
// 这里会无条件写 200，一旦上游回的是错误响应，客户端就会以为请求成功。
func (s *Server) pipeSSEFrom(w http.ResponseWriter, resp *http.Response, br *bufio.Reader, first string) stats.Usage {
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
	deliver := func(data string) error {
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
	}
	if first != "" {
		if err := deliver(first); err != nil {
			s.log.Warnf("读取上游流失败: %v", err)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return usage
		}
	}
	err := openai.ReadSSEEvents(br, deliver)
	if err != nil {
		s.log.Warnf("读取上游流失败: %v", err)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	return usage
}

// pipeSSEWithFailover 透传流式响应；上游在流的第一个事件就回报错误时
// （额度 / 限流常以 200 + SSE 的形态出现，状态码完全正常），在写给下游
// 任何字节之前冷却该账号、换同组下一个账号重来——客户端全程无感。
//
// 与 aggregateWithFailover（非流式）是同一套机制：只对「上游明确报错」
// 换号，读取流失败（网络中断等）重试没有意义。已经开始透传之后才出现的
// 错误无法换号（字节已经给下游了），按原有行为只在日志里记一笔。
func (s *Server) pipeSSEWithFailover(
	ctx context.Context,
	w http.ResponseWriter,
	providerID string,
	resp *http.Response,
	cred *upstream.Credential,
	body []byte,
	r *http.Request,
	requestedModel string,
) (stats.Usage, *upstream.Credential, error) {
	exclude := map[string]bool{}
	for attempt := 0; ; attempt++ {
		// 防御性分支：doChatExcluding 契约上只返回 < 400 的响应（>= 400 的都在
		// 组内跳过换号了，handleChat 也会先拦一道），正常情况下走不到这里。
		// 真出现时（含测试直接驱动本函数）必须把上游错误按 HTTP 错误带回：
		// 一个 ≥400 的响应体里没有 data 事件，PeekFirstEvent 会当它健康流，
		// 于是客户端收到 200 + 原始错误体——以为成功，拿到的却是错误。
		// 与 aggregateWithFailover 的同名分支保持同一口径。
		if resp.StatusCode >= 400 {
			kind, detail, retryAfter := inspectUpstreamFailure(resp)
			_ = resp.Body.Close()
			if kind != failNone && cred != nil {
				until := s.markCooldown(cred.ID, kind, retryAfter)
				s.log.Warnf("账号 %s %s，冷却至 %s（%s）", cred.DisplayName(), failReason(kind),
					until.Format("15:04:05"), truncate(detail, 120))
			}
			return stats.Usage{}, cred, &upstreamHTTPError{Status: resp.StatusCode, Message: detail}
		}
		br := bufio.NewReader(resp.Body)
		first, err := openai.PeekFirstEvent(br)
		var serr *openai.StreamError
		if !errors.As(err, &serr) || cred == nil {
			// 健康流（或读不出事件就中断的空流）：正常透传。
			//
			// body 由本函数收口关闭：换号后拿到的响应体不在调用方的 defer
			// 捕获范围内（那个 defer 只认识最初那个 resp），不关就会漏连接。
			usage := s.pipeSSEFrom(w, resp, br, first)
			_ = resp.Body.Close()
			return usage, cred, nil
		}

		// 流首错误：此时还没给下游写过一个字节，可以安全换号。
		_ = resp.Body.Close()
		if kind := classifyMessage(serr.Message); kind != failNone {
			// 流首错误帧与 HTTP 层同一口径：正文里带了额度窗口重置时刻
			// （code=6004 之类）就冷却到那个时刻，而不是几十秒的退避。
			until := s.markCooldown(cred.ID, kind, parseUsageResetAt(serr.Message))
			s.log.Warnf("账号 %s %s，冷却至 %s（%s）", cred.DisplayName(), failReason(kind),
				until.Format("15:04:05"), truncate(serr.Message, 120))
		}
		exclude[cred.ID] = true
		s.log.Warnf("[%s] %s 上游流首返回错误，换号重试: %s", providerID, requestedModel, truncate(serr.Message, 200))
		if attempt >= aggregateRetries-1 {
			return stats.Usage{}, cred, &openai.StreamError{Message: "上游返回错误: " + serr.Message}
		}

		next, nextCred, err := s.doChatExcluding(ctx, providerID, body, r, exclude)
		if err != nil {
			// 组内没有别的账号可换时，doChatExcluding 会返回 ErrNoAccount。
			// 直接透传会把「额度已用尽」说成「尚未登录账号」——原始文案更有用。
			if errors.Is(err, ErrNoAccount) || errors.Is(err, ErrAllPaused) {
				return stats.Usage{}, cred, &openai.StreamError{Message: "上游返回错误: " + serr.Message}
			}
			// 整组都在冷却：与 aggregateWithFailover 同一口径——429 + Retry-After
			// 带回，文案里保留上游的原始错误（原因在前，恢复时间在后）。
			var ufe *upstreamFailError
			if errors.As(err, &ufe) {
				return stats.Usage{}, cred, &upstreamFailError{
					msg:        "上游返回错误: " + truncate(serr.Message, 500) + "；" + ufe.msg,
					retryAfter: ufe.retryAfter,
				}
			}
			// 其它错误（如组内账号都不健康）比原始文案更贴近实际。
			return stats.Usage{}, cred, err
		}
		resp, cred = next, nextCred
	}
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
//
// 虚拟模型组不在这里解析：它的名字不指向单个上游，由 resolveRoute 单独处理。
// 命中虚拟组时显式报错，而不是返回一个 ID 为空的 Profile——后者会让调用方
// 拿到「看起来成功、实际没有目标」的结果，比直接失败更难排查。
func (s *Server) resolveModel(name string, r *http.Request) (config.Profile, string, error) {
	profile, model, vm, err := s.resolveRoute(name, r)
	if err != nil {
		return profile, model, err
	}
	if vm != nil {
		return config.Profile{}, "", fmt.Errorf("%s 是虚拟模型组，不能按单个上游解析", vm.Name)
	}
	return profile, model, nil
}

// resolveRoute 是 resolveModel 的完整版，额外返回命中的虚拟组（未命中时为 nil）。
//
// 虚拟组的优先级低于显式强制路由：客户端写了 "qoder-cn:xxx" 或带了
// X-RapidProxy-Provider 头，就说明它明确要打到那个上游，此时即使存在同名
// 虚拟组也应当按强制路由走。
func (s *Server) resolveRoute(name string, r *http.Request) (config.Profile, string, *config.VirtualModel, error) {
	cfg := s.Config()
	// 不能因为「没有启用任何上游」就提前失败：虚拟组允许使用已登录但未全局
	// 启用的上游（见 usableVirtualMembers），只由这类成员组成的组仍应可用。
	// 各分支会各自判断自己需要的条件。
	if len(cfg.Profiles) == 0 {
		return config.Profile{}, "", nil, errors.New("没有配置任何上游")
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
			return config.Profile{}, "", nil, fmt.Errorf("未知的上游: %s", forced)
		}
		if !profile.Enabled {
			return config.Profile{}, "", nil, fmt.Errorf("上游 %s 未启用", forced)
		}
		if !s.hasCredential(profile.ID) {
			return config.Profile{}, "", nil, fmt.Errorf("上游 %s 还没有登录账号，请先登录", profile.ID)
		}
		return profile, model, nil, nil
	}

	// 裸模型名先看是不是虚拟组：虚拟名是用户自己起的，优先级高于真实模型名，
	// 否则「虚拟组名恰好等于某个真实模型 ID」时用户永远调不到自己的组。
	// ValidateVirtualModel 已禁止与上游 ID 冲突，但真实模型 ID 是上游动态返回的，
	// 无法在保存时穷举，只能在这里按「虚拟优先」定序。
	if vm, ok := cfg.VirtualModelByName(model); ok && vm.Enabled && len(vm.Members) > 0 {
		return config.Profile{}, model, &vm, nil
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
		// 虚拟条目在上面的 VirtualModelByName 分支已处理过（能走到这里说明
		// 该组已停用或没有成员）；跳过它，免得把一个空的组当成真实上游。
		if e.virtualName != "" {
			continue
		}
		profile, ok := cfg.ProfileByID(e.profileID)
		if !ok || !profile.Enabled {
			continue
		}
		return profile, e.upstreamID, nil, nil
	}
	// 未匹配到具体上游时交给第一个已登录的上游，避免客户端因别名而失败。
	active := s.loggedInProfiles()
	if len(active) == 0 {
		return config.Profile{}, "", nil, ErrNoAccount
	}
	s.log.Warnf("模型 %s 不在已知列表中，按默认上游 %s 转发", model, active[0].ID)
	return active[0], model, nil, nil
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
	"exceed_quota", "超出配额",
	"额度", "余额不足", "配额", "用量已用尽", "计费",
}

var rateLimitKeywords = []string{
	"rate limit", "rate_limit", "rate-limit", "ratelimit",
	"too many requests", "too_many_requests",
	// "usage exceeds frequency limit"（上游 code=6004）这类窗口限流，
	// 关键词在文案开头就得能命中，不能依赖句子尾部恰好出现 "rate limit"。
	"frequency limit", "frequency_limit",
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
// 返回 failNone 表示这不是可退避的错误（按原有逻辑处理，例如认证失败、
// 模型没在这个账号开通）；调用方读完 detail 后必须自己关闭 body。
//
// detail 一律是提取后的可读文案（见 extractUpstreamErrorMessage），分类用的
// 则是原文——限流 / 额度关键词在 JSON 与 SSE 两种形态里都出现在正文中。
// 返回的冷却时长取 Retry-After 头与正文里额度窗口重置时刻（如 code=6004 的
// "usage will reset at ..."，见 parseUsageResetAt）中较长的一个，交由
// markCooldown 择优采纳。
func inspectUpstreamFailure(resp *http.Response) (failKind, string, time.Duration) {
	if resp == nil || resp.StatusCode < 400 {
		return failNone, "", 0
	}
	// 上限与 writeNonStreamUpstream 的 8MB 对齐：读满即可判定，
	// 更大的错误正文没有额外信息。
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
	detail := extractUpstreamErrorMessage(resp, raw)
	if kind == failNone {
		return failNone, detail, 0
	}
	// 上游在正文里明确给出额度窗口的重置时间（如 code=6004 的
	// "usage will reset at 2026-10-26 17:36:44 UTC+8"）时也纳入参照：
	// 这类窗口限流动辄几小时，按几十秒的限流退避只会让账号在窗口内
	// 被反复选中、反复失败。与 Retry-After 头取更长的那个（markCooldown
	// 只在超过退避时长时采纳，且受 cooldownCeiling 上限约束）。
	if d := parseUsageResetAt(msg); d > retryAfter {
		retryAfter = d
	}
	return kind, truncate(detail, 200), retryAfter
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

// usageResetRe 匹配上游错误正文里「额度窗口的重置时刻」，如 code=6004 的
// "usage will reset at 2026-10-26 17:36:44 UTC+8"（兼容 resets at）。
// UTC 偏移允许整小时或带 / 不带冒号的分秒两种形态（UTC+8 / UTC+08:00 / UTC+0800）。
var usageResetRe = regexp.MustCompile(
	`(?i)resets?\s+at\s+(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2}:\d{2})\s*UTC\s*([+-]\d{1,2})(?::?(\d{2}))?`)

// parseUsageResetAt 从错误正文里解析额度窗口的重置时刻，返回距现在的时长。
//
// code=6004 "usage exceeds frequency limit" 这类窗口限流不是秒级抖动：
// 上游明确说用量要到几点才重置，按限流的几十秒退避处理，账号会在窗口内
// 被反复选中、反复失败，每次都白打一单上游再换号。能解析出时刻且尚未
// 到达时返回该时长（冷却上限仍由 markCooldown 的 cooldownCeiling 把关），
// 解析不出或时刻已过则返回 0，走原有退避。
func parseUsageResetAt(msg string) time.Duration {
	m := usageResetRe.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	hours, err := strconv.Atoi(m[3])
	if err != nil {
		return 0
	}
	offs := hours * 3600
	if m[4] != "" {
		mins, err := strconv.Atoi(m[4])
		if err != nil {
			return 0
		}
		// 符号看匹配到的原文："UTC-0:30" 的小时位是 "-0"，Atoi 后丢符号，
		// 按数值正负判断会把 -30 分钟偏移当成 +30 分钟。
		if strings.HasPrefix(m[3], "-") {
			offs -= mins * 60
		} else {
			offs += mins * 60
		}
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1]+" "+m[2], time.FixedZone("", offs))
	if err != nil {
		return 0
	}
	d := time.Until(t)
	if d <= 0 {
		return 0
	}
	return d
}

// markCooldown 记录一次限流或额度耗尽，返回冷却截止时间。
//
// 同一个原因连续触发时按指数退避延长，避免把账号打得更死；
// 上游给了明确的恢复时间时以其为准（受 cooldownCeiling 限制）——
// 来源可以是 Retry-After 头，也可以是错误正文里的额度窗口重置时刻
// （如 code=6004 的 "usage will reset at ..."，见 parseUsageResetAt），
// 这类窗口限流动辄几小时，几十秒的指数退避形同虚设。
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

// -----------------------------------------------------------------------------
// 虚拟模型组
// -----------------------------------------------------------------------------

// virtualPick 是一次虚拟组成员选择的结果。
type virtualPick struct {
	Profile config.Profile
	Model   string // 转发给上游时使用的模型名（该上游的原始 ID）
	Member  config.VirtualMember
}

// virtualMemberKey 是虚拟组内某成员的 SWRR 状态键。
//
// 与账号级 balState 共用一张表但命名空间不同：账号键是 "上游/账号ID"，
// 这里是 "virtual:组名/上游/模型"，两者不可能相撞（账号 ID 不会以
// "virtual:" 开头，上游 ID 也不会）。
func virtualMemberKey(virtualName string, m config.VirtualMember) string {
	return stats.VirtualKey(virtualName) + "/" + m.Key()
}

// usableVirtualMembers 返回虚拟组里此刻能承接请求的成员。
//
// 判据是「该上游是否还有可用账号」（非暂停、非冷却）。这些条件任一不满足
// 就跳过——用户看到的应是「组里还有谁能接」，而不是「配置里写了几个成员」。
//
// 刻意不检查上游的全局启用开关：虚拟组是用户对「用哪个上游的哪个模型」
// 的显式选择，优先级高于上游的全局开关。想掐断整组流量就停用这个组本身
// （VirtualModel.Enabled），而不是去关上游——关上游会连带影响所有裸模型名
// 的调用，语义过重。这与 /v1/models 的暴露规则也是自洽的：全局停用的上游
// 不会让它的全部模型出现在列表里，但虚拟组把它单独拎出来用是用户的意思。
//
// exclude 里的成员不再考虑，供成员失败后依次换人重试。
func (s *Server) usableVirtualMembers(cfg *config.Config, vm *config.VirtualModel, exclude map[string]bool) []config.VirtualMember {
	if vm == nil {
		return nil
	}
	// 可用账号数按上游缓存一次：成员失败重试时本函数会被反复调用，
	// 同上游的多个成员也常指向同一账号池，逐成员查询纯属重复扫描。
	avails := make(map[string]int, 4)
	available := func(providerID string) bool {
		n, ok := avails[providerID]
		if !ok {
			n, _ = s.AccountStats(providerID)
			avails[providerID] = n
		}
		return n > 0
	}
	out := make([]config.VirtualMember, 0, len(vm.Members))
	for _, m := range vm.Members {
		if exclude[m.Key()] {
			continue
		}
		// 上游必须存在（成员可能在配置被手改后指向已删除的上游）。
		if _, ok := cfg.ProfileByID(m.Provider); !ok {
			continue
		}
		if !available(m.Provider) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// pickVirtualMember 按权重从虚拟组里挑一个成员（平滑加权轮询）。
//
// 与 pickCredential 是同一套算法，只是层级从「账号」上移到了「成员」：
// 成员默认同权，可单独调权重；不可用的成员在 usableVirtualMembers 里
// 已被排除，这里只做分摊。
func (s *Server) pickVirtualMember(virtualName string, members []config.VirtualMember, exclude map[string]bool) (config.VirtualMember, bool) {
	usable := make([]config.VirtualMember, 0, len(members))
	total := 0
	for _, m := range members {
		if exclude[m.Key()] {
			continue
		}
		usable = append(usable, m)
		total += m.EffectiveWeight()
	}
	if len(usable) == 0 || total <= 0 {
		return config.VirtualMember{}, false
	}

	s.balMu.Lock()
	defer s.balMu.Unlock()
	if s.balState == nil {
		s.balState = map[string]int{}
	}
	best, bestCW := -1, 0
	for i, m := range usable {
		key := virtualMemberKey(virtualName, m)
		cw := s.balState[key] + m.EffectiveWeight()
		s.balState[key] = cw
		if best < 0 || cw > bestCW {
			best, bestCW = i, cw
		}
	}
	if best < 0 {
		return config.VirtualMember{}, false
	}
	chosen := usable[best]
	s.balState[virtualMemberKey(virtualName, chosen)] = bestCW - total
	return chosen, true
}

// ClearVirtualState 清掉某个虚拟组的分摊状态（改名、改成员后调用）。
//
// 不清的话，改名后新名字从零开始累积偏移、旧名字的条目永远留在表里；
// 改权重后旧偏移（按旧权重累积）还可能让分配短暂倾斜。
func (s *Server) ClearVirtualState(name string) {
	prefix := stats.VirtualKey(name) + "/"
	s.balMu.Lock()
	defer s.balMu.Unlock()
	for key := range s.balState {
		if strings.HasPrefix(key, prefix) {
			delete(s.balState, key)
		}
	}
}

// VirtualAvailableMembers 返回某个虚拟组此刻可承接请求的成员数。
//
// 界面侧由 app.virtualModelViews 统一计算（它还要逐成员状态，顺手就能
// 数出可用数），这里保留为独立查询入口，便于排查与测试。
func (s *Server) VirtualAvailableMembers(name string) int {
	cfg := s.Config()
	vm, ok := cfg.VirtualModelByName(name)
	if !ok {
		return 0
	}
	return len(s.usableVirtualMembers(cfg, &vm, nil))
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
//
// 组内故障转移的总口径：账号有问题就跳过它换下一个，不把错误透传给下游——
// 限流 / 额度 / 上游不可用冷却后换号；模型没开通、参数不对这类「这台账号
// 接不了这单」的错误同样换号（不冷却，只在本请求内排除）。只有整组都试过
// 仍无人能接，才把最后的错误带回：有客户端错误时按原状态码带回，否则汇总
// 成带 Retry-After 的 429。用户侧因此只在整个组都不可用时才看到错误。
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
	// clientErr 是组内账号回过的「不可退避错误」：模型没在这个账号开通、
	// 参数不对、404……换账号是对路的，但不能冷却账号（不是账号的错）。
	// 穷尽整组时按原状态码带回，调用方照旧拿到可排查的文案。
	var clientErr *upstreamHTTPError
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
		respKind, respDetail, respRetryAfter := inspectUpstreamFailure(resp)
		if respKind != failNone {
			_ = resp.Body.Close()
			until := s.markCooldown(cred.ID, respKind, respRetryAfter)
			unlock()
			backed++
			lastErr = fmt.Errorf("账号 %s %s，冷却至 %s", cred.DisplayName(), failReason(respKind), until.Format("15:04:05"))
			s.log.Warnf("账号 %s %s，冷却至 %s（%s）", cred.DisplayName(), failReason(respKind),
				until.Format("15:04:05"), truncate(respDetail, 120))
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
			retryKind, retryDetail, retryAfter := inspectUpstreamFailure(retry)
			if retryKind != failNone {
				_ = retry.Body.Close()
				until := s.markCooldown(cred.ID, retryKind, retryAfter)
				unlock()
				backed++
				lastErr = fmt.Errorf("账号 %s %s，冷却至 %s", cred.DisplayName(), failReason(retryKind), until.Format("15:04:05"))
				s.log.Warnf("账号 %s %s，冷却至 %s（%s）", cred.DisplayName(), failReason(retryKind),
					until.Format("15:04:05"), truncate(retryDetail, 120))
				continue
			}
			if retry.StatusCode == http.StatusUnauthorized || retry.StatusCode == http.StatusForbidden {
				_ = retry.Body.Close()
				unlock()
				lastErr = fmt.Errorf("账号 %s 认证失败（已尝试刷新令牌）", cred.DisplayName())
				continue
			}
			// 刷新令牌后重试仍被上游拒绝（模型没开通、参数不对等）：同样
			// 跳过换号，不透传给下游——组里换个有权限的账号请求就成了。
			if retry.StatusCode >= 400 {
				_ = retry.Body.Close()
				unlock()
				clientErr = &upstreamHTTPError{Status: retry.StatusCode, Message: retryDetail}
				lastErr = clientErr
				s.log.Warnf("账号 %s 刷新后仍被上游拒绝（HTTP %d），换同组下一个账号: %s",
					cred.DisplayName(), retry.StatusCode, truncate(retryDetail, 120))
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

		// 走到这里 resp 一定不是 401/403（上面整段处理过），也不是可退避错误
		// （同上面整段处理过）：若仍是 >= 400，就是「这台账号接不了这单」——
		// 模型没在这个账号开通、参数不对、404……跳过它换同组下一个账号，
		// 不把错误透传给下游：组里换个有权限的账号，请求就成了。
		//
		// 不冷却账号：这不是账号的错，只在本请求内排除（tried）。
		// 穷尽整组都没有能用的账号时，最后的客户端错误按原状态码带回
		// （见函数收尾），调用方照旧拿到可排查的文案。
		if resp.StatusCode >= 400 {
			_ = resp.Body.Close()
			unlock()
			clientErr = &upstreamHTTPError{Status: resp.StatusCode, Message: respDetail}
			lastErr = clientErr
			s.log.Warnf("账号 %s 请求被上游拒绝（HTTP %d），换同组下一个账号: %s",
				cred.DisplayName(), resp.StatusCode, truncate(respDetail, 120))
			continue
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
	//
	// 组里出现过「客户端错误」（模型没开通、参数不对等）时优先按原状态码
	// 带回：这类错误换多少个账号都一样，状态码与文案对调用方排查最有意义
	// （也避免被误当成可重试的临时故障而反复重发同一个坏请求）。
	// 都是可退避错误（限流 / 额度 / 上游不可用）时才汇总成 429 + Retry-After，
	// 告诉客户端多久后重试，而不是笼统报 502。
	if clientErr != nil {
		return nil, nil, clientErr
	}
	// 可退避的情况有两种：本次调用里退避过（backed > 0），或者还没打出去
	// 就无号可挑（pickCredential 直接返回 nil）——剩下的账号不是暂停就是
	// 全在冷却中。后者绝不能落进 ErrNoAccount：账号明明在线，只是都在
	// 冷却，报「尚未登录账号」会把用户引去反复检查登录状态。窗口限流
	// （code=6004 之类）按重置时刻冷却动辄几小时，整组撞进同一个窗口
	// 后这条路径就是新请求的常态归宿。
	// 判定口径：非暂停账号里，「在冷却」或「已被 exclude 排除」要覆盖
	// 全部，且至少有一个真的在冷却——exclude 里的账号刚在本请求上游
	// 失败过（可能已进冷却），只数非排除的会把单账号组漏成 ErrNoAccount。
	cooling := 0
	groupDown := true
	for _, c := range creds {
		switch {
		case c.Paused:
			// 不参与转发，不碍事。
		case s.cooldownLeft(c.ID) > 0:
			cooling++
		case exclude[c.ID]:
			// 本请求里已试过且未冷却（不可退避的客户端错误）：不推翻整组判定。
		default:
			groupDown = false
		}
	}
	if backed > 0 || (cooling > 0 && groupDown) {
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
//
// 只统计尚未过期的条目：过期条目要等下次 markCooldown 才被回收，
// 直接采用会把 Retry-After 算成 0 甚至负数，文案里还带着过去的时刻。
func (s *Server) earliestCooldown(creds []*upstream.Credential) (time.Time, bool) {
	s.coolMu.Lock()
	defer s.coolMu.Unlock()
	var best time.Time
	now := time.Now()
	for _, c := range creds {
		st, ok := s.cool[c.ID]
		if !ok || !st.Until.After(now) {
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

// normalizeGLMCodingPayload 适配智谱编程（GLM coding-plan）网关的两处硬性
// 要求：developer 角色必须写成 system；空的 user 内容会被拒绝，补一个占位符。
func normalizeGLMCodingPayload(payload map[string]any) {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if strings.EqualFold(role, "developer") {
			msg["role"] = "system"
			role = "system"
		}
		if strings.EqualFold(role, "user") && contentIsEmpty(msg["content"]) {
			msg["content"] = "[No user content provided]"
		}
	}
}

// contentIsEmpty 判断消息内容是否为空（nil / 空串 / 空数组）。
func contentIsEmpty(v any) bool {
	switch c := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(c) == ""
	case []any:
		return len(c) == 0
	}
	return false
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
