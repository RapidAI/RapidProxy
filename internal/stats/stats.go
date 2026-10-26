// Package stats 记录并持久化「按组（上游）按模型」的流量统计。
//
// 流量以 token 用量衡量：输入(prompt) / 输出(completion) / 总量(total) 与请求次数。
// 数据以「小时桶」为最小粒度落盘，日 / 周 / 月 三种周期在查询时再聚合，
// 这样既能给出按时段的趋势，又不必为每种周期各存一份。
package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znsoftm/RapidProxy/internal/fsx"
)

// Usage 是一次请求里提取到的 token 用量。Observed 为 false 表示响应里没有
// usage 字段，此时不应计入统计（模型未上报用量时强行记 0 只会污染趋势图）。
type Usage struct {
	In       int64 `json:"in"`
	Out      int64 `json:"out"`
	Total    int64 `json:"total"`
	Observed bool  `json:"-"`
}

// Entry 是一条待记录的流量记录。
type Entry struct {
	Provider string
	Model    string
	At       time.Time
	In       int64
	Out      int64
	Total    int64
	// Virtual 非空表示这条记录同时归属某个虚拟模型组：Provider/Model 仍是
	// 真实承接请求的上游与模型，Virtual 是该虚拟组的名字。
	//
	// 真实维度与虚拟维度分开记，是因为两者回答的问题不同：真实维度用来
	// 核对上游额度消耗，虚拟维度用来观察「用户请求的那个别名」用了多少。
	// 一次请求会同时产生两条记录，两条各自独立累加。
	Virtual string
}

// Bucket 是一个小时桶内的累计用量。
type Bucket struct {
	In    int64 `json:"in"`
	Out   int64 `json:"out"`
	Total int64 `json:"total"`
	Count int64 `json:"count"`
}

// providerStats 是「模型 -> 小时键 -> 桶」。
type providerStats map[string]map[string]Bucket

// Point 是趋势图里的一个数据点。
type Point struct {
	Label string `json:"label"`
	In    int64  `json:"in"`
	Out   int64  `json:"out"`
	Total int64  `json:"total"`
	Count int64  `json:"count"`
}

// Summary 是某个周期内的汇总。
type Summary struct {
	In    int64 `json:"in"`
	Out   int64 `json:"out"`
	Total int64 `json:"total"`
	Count int64 `json:"count"`
}

// Result 是 Query 的完整返回：汇总 + 趋势序列 + 元信息。
type Result struct {
	Period string `json:"period"` // day / week / month
	Unit   string `json:"unit"`   // hour / day，趋势序列的粒度
	// Kind 是这条统计的来源：provider（真实上游）或 virtual（虚拟模型组）。
	Kind       string  `json:"kind"`
	Provider   string  `json:"provider"` // 上游（组）ID，或虚拟模型名
	Model      string  `json:"model"`    // 模型 ID；空串表示本组全部模型汇总
	Summary    Summary `json:"summary"`
	Series     []Point `json:"series"`
	StartLabel string  `json:"startLabel"`
	EndLabel   string  `json:"endLabel"`
	// Members 是虚拟组内各成员的分摊明细（按 total 降序）。
	// 只有查询虚拟组且未指定单个成员时才有值，用于观察组内流量分布。
	Members []MemberUsage `json:"members,omitempty"`
}

// MemberUsage 是虚拟组内一个成员的累计用量（全时段，不受 period 影响）。
type MemberUsage struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	In       int64  `json:"in"`
	Out      int64  `json:"out"`
	Total    int64  `json:"total"`
	Count    int64  `json:"count"`
}

// Scope 是「有统计数据的 (组, 模型)」组合，供前端默认选中。
type Scope struct {
	// Kind 区分真实上游与虚拟模型组，前端据此分组展示。
	Kind     string `json:"kind"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	In       int64  `json:"in"`
	Out      int64  `json:"out"`
	Total    int64  `json:"total"`
	Count    int64  `json:"count"`
}

// 小时键格式（本地时间）：2006-01-02T15。
const hourKeyLayout = "2006-01-02T15"

// virtualPrefix 是虚拟模型组在存储里的命名空间前缀。
//
// 虚拟组与真实上游共用同一个三层 map（provider -> model -> hour -> bucket），
// 用前缀把两者隔开：上游 ID 由代码里固定的一小组预设决定（workbuddy、
// qoder-cn…），不可能以 "virtual:" 开头，因此不会撞名；同时旧的 traffic.json
// 不需要任何迁移——它里面本来就没有这个前缀的键。
const virtualPrefix = "virtual:"

// KindProvider / KindVirtual 是统计维度的两种来源。
const (
	KindProvider = "provider"
	KindVirtual  = "virtual"
)

// VirtualKey 返回虚拟组在存储里的键。
func VirtualKey(name string) string { return virtualPrefix + name }

// IsVirtualKey 报告一个存储键是否属于虚拟组命名空间。
func IsVirtualKey(key string) bool { return strings.HasPrefix(key, virtualPrefix) }

// retentionDays 是历史数据的保留天数，超过则裁剪，避免文件无限膨胀。
const retentionDays = 540

// Recorder 记录流量并在后台防抖落盘。
type Recorder struct {
	mu        sync.Mutex
	path      string
	data      map[string]providerStats // provider -> model -> hourKey -> bucket
	dirty     bool
	lastFlush time.Time
	stop      chan struct{}
	done      chan struct{}
}

// New 创建记录器并加载已有数据，同时启动后台刷盘协程。
func New(dataDir string) *Recorder {
	r := &Recorder{
		path: filepath.Join(dataDir, "traffic.json"),
		data: map[string]providerStats{},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if raw, err := os.ReadFile(r.path); err == nil {
		var snap struct {
			V       int                      `json:"v"`
			Buckets map[string]providerStats `json:"buckets"`
		}
		if err := json.Unmarshal(raw, &snap); err == nil && snap.Buckets != nil {
			r.data = snap.Buckets
		}
	}
	// pruneLocked 要求调用方持有 r.mu；构造期虽然还没有其它 goroutine 能拿到
	// r，但这里显式加锁以满足前置条件并保持一致，避免日后并发构造时踩坑。
	r.mu.Lock()
	r.pruneLocked()
	r.mu.Unlock()
	r.lastFlush = time.Now()
	go r.run()
	return r
}

// run 是后台刷盘循环：每 15 秒落盘一次，关闭时再落一次。
func (r *Recorder) run() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	defer close(r.done)
	for {
		select {
		case <-ticker.C:
			r.Flush()
		case <-r.stop:
			r.Flush()
			return
		}
	}
}

// Close 停止后台协程并最后一次落盘。
func (r *Recorder) Close() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
}

// Record 把一条流量记录累加进对应的小时桶。
//
// 一条记录最多落在两个维度上：真实上游（Provider/Model）与虚拟组
// （Virtual）。前者用于核对上游额度，后者用于观察别名维度的用量。
func (r *Recorder) Record(e Entry) {
	if e.Provider == "" || e.Model == "" {
		return
	}
	at := e.At.Local()
	if at.IsZero() {
		at = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.addLocked(e.Provider, e.Model, at, e)
	if e.Virtual != "" {
		// 虚拟维度以组名为模型、以组名本身为 provider 之外的第二层：
		// 用 VirtualKey 作为 provider 层、成员标识作为 model 层，
		// 这样「虚拟组总量」与「组内成员分摊」都能直接从同一结构读出来。
		r.addLocked(VirtualKey(e.Virtual), e.Provider+"/"+e.Model, at, e)
	}
}

// addLocked 把用量累加进 (provider, model, hour) 桶，调用方必须持有 r.mu。
func (r *Recorder) addLocked(provider, model string, at time.Time, e Entry) {
	ps, ok := r.data[provider]
	if !ok {
		ps = providerStats{}
		r.data[provider] = ps
	}
	ms, ok := ps[model]
	if !ok {
		ms = map[string]Bucket{}
		ps[model] = ms
	}
	hour := at.Format(hourKeyLayout)
	b := ms[hour]
	b.In += e.In
	b.Out += e.Out
	b.Total += e.Total
	b.Count++
	ms[hour] = b
	r.dirty = true
}

// Flush 若数据有变动则原子落盘。
func (r *Recorder) Flush() {
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return
	}
	r.pruneLocked()
	snap := struct {
		V       int                      `json:"v"`
		Buckets map[string]providerStats `json:"buckets"`
	}{V: 1, Buckets: r.data}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		r.mu.Unlock()
		return
	}
	path := r.path
	r.dirty = false
	r.lastFlush = time.Now()
	r.mu.Unlock()

	if err := fsx.AtomicWrite(path, append(raw, '\n'), 0o600); err != nil {
		// 落盘失败只记日志（调用方无合适日志器），下次周期再试。
		return
	}
}

// pruneLocked 删除过期桶与空壳分组，调用方必须持有 r.mu。
func (r *Recorder) pruneLocked() {
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Format(hourKeyLayout)
	for provider, ps := range r.data {
		for model, ms := range ps {
			for hour := range ms {
				if hour < cutoff {
					delete(ms, hour)
				}
			}
			if len(ms) == 0 {
				delete(ps, model)
			}
		}
		if len(ps) == 0 {
			delete(r.data, provider)
		}
	}
}

// Query 按周期聚合 (provider, model) 的流量。model 为空表示本组全部模型汇总。
//
// provider 既可以是真实上游 ID，也可以是虚拟模型名——后者会自动加上
// virtual: 前缀去查（见 VirtualKey）。
//
// 周期定义（本地时间，相对 now）：
//   - day：当天 0 点起的 24 个小时点，趋势粒度 hour；
//   - week：最近 7 天（含今天），趋势粒度 day；
//   - month：最近 30 天（含今天），趋势粒度 day。
func (r *Recorder) Query(provider, model, period string, now time.Time) Result {
	r.mu.Lock()
	defer r.mu.Unlock()

	now = now.Local()
	res := Result{Period: period, Provider: provider, Model: model, Unit: "hour", Kind: KindProvider}
	key := provider
	// 虚拟组的判定必须是确定性的，不能依赖「该组有没有数据」：
	// 否则刚建好、还没产生流量的组会被当成真实上游（kind 不一致），
	// 前端据此渲染的控件就会来回跳。
	//
	// 裸名的歧义（真实上游 ID 与虚拟组名相同）优先判给真实上游：
	// 配置层禁止这种重名，但手改配置或历史数据可能出现，此时裸 ID 指
	// 真实上游更符合直觉；虚拟组仍可用 "virtual:组名" 精确寻址。
	switch {
	case IsVirtualKey(provider):
		res.Kind = KindVirtual
		res.Provider = strings.TrimPrefix(provider, virtualPrefix)
	case r.data[provider] == nil && r.data[VirtualKey(provider)] != nil:
		key = VirtualKey(provider)
		res.Kind = KindVirtual
	}
	var start, end time.Time
	switch period {
	case "week":
		start = startOfDay(now).AddDate(0, 0, -6)
		end = startOfDay(now).AddDate(0, 0, 7) // 不含
		res.Unit = "day"
	case "month":
		start = startOfDay(now).AddDate(0, 0, -29)
		end = startOfDay(now).AddDate(0, 0, 1)
		res.Unit = "day"
	default: // day
		start = startOfDay(now)
		end = start.AddDate(0, 0, 1)
	}

	buckets := r.collectLocked(key, model)

	if res.Unit == "hour" {
		day := start.Format("2006-01-02")
		for h := 0; h < 24; h++ {
			key := day + "T" + pad2(h)
			b := buckets[key]
			res.Series = append(res.Series, Point{
				Label: pad2(h) + ":00",
				In:    b.In, Out: b.Out, Total: b.Total, Count: b.Count,
			})
			addSummary(&res.Summary, b)
		}
	} else {
		for d := start; !d.After(end.Add(-time.Second)); d = d.AddDate(0, 0, 1) {
			dateKey := d.Format("2006-01-02")
			b := sumDay(buckets, dateKey)
			res.Series = append(res.Series, Point{
				Label: d.Format("1/2"),
				In:    b.In, Out: b.Out, Total: b.Total, Count: b.Count,
			})
			addSummary(&res.Summary, b)
		}
	}

	if len(res.Series) > 0 {
		res.StartLabel = res.Series[0].Label
		res.EndLabel = res.Series[len(res.Series)-1].Label
	}
	// 虚拟组未指定单个成员时，附上成员分摊明细：用户要看的正是
	// 「这个别名背后流量落到谁身上了」。
	if res.Kind == KindVirtual && model == "" {
		res.Members = r.memberUsageLocked(key)
	}
	return res
}

// memberUsageLocked 汇总某虚拟组内各成员的累计用量（全时段），按 total 降序。
// 调用方必须持有 r.mu。
func (r *Recorder) memberUsageLocked(virtualKey string) []MemberUsage {
	ps := r.data[virtualKey]
	if len(ps) == 0 {
		return nil
	}
	out := make([]MemberUsage, 0, len(ps))
	for member, ms := range ps {
		// 成员键形如 "上游ID/模型ID"；按第一个 "/" 切分即可，
		// 模型 ID 本身不含 "/"（含 "/" 的会被 resolveModel 当强制路由前缀）。
		provider, model, _ := strings.Cut(member, "/")
		mu := MemberUsage{Provider: provider, Model: model}
		for _, b := range ms {
			mu.In += b.In
			mu.Out += b.Out
			mu.Total += b.Total
			mu.Count += b.Count
		}
		if mu.Count > 0 {
			out = append(out, mu)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// collectLocked 取出 (provider, model) 的小时桶；provider 或 model 为空时
// 分别表示跨组汇总、本组全部模型汇总（两者皆空则为全局汇总）。
// 调用方必须持有 r.mu。
func (r *Recorder) collectLocked(provider, model string) map[string]Bucket {
	out := map[string]Bucket{}
	aggregate := func(ps providerStats) {
		if model != "" {
			for k, b := range ps[model] {
				out[k] = b
			}
			return
		}
		for _, ms := range ps {
			for k, b := range ms {
				cur := out[k]
				cur.In += b.In
				cur.Out += b.Out
				cur.Total += b.Total
				cur.Count += b.Count
				out[k] = cur
			}
		}
	}
	if provider == "" {
		// 跨组汇总：遍历所有真实上游，但必须跳过虚拟组命名空间。
		//
		// 虚拟维度的数据是真实维度的「影子副本」——同一次请求在两边各记一条
		// （见 Record）。把它也算进跨组汇总，总量就会整整翻一倍。真实上游 ID
		// 不可能以 virtual: 开头，所以按前缀排除既准确又不需要额外标记。
		for key, ps := range r.data {
			if IsVirtualKey(key) {
				continue
			}
			aggregate(ps)
		}
		return out
	}
	ps := r.data[provider]
	if ps == nil {
		return out
	}
	aggregate(ps)
	return out
}

// Scopes 返回「有数据」的 (provider, model) 组合及其累计量（全时段）。
//
// 虚拟组以组名作为 provider、以空 model 返回一条汇总记录（成员明细由
// Query 的 Members 字段给出）：统计页的下拉框只需要「这个虚拟组有流量」，
// 不需要把每个成员各列一条。
func (r *Recorder) Scopes() []Scope {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Scope, 0)
	for provider, ps := range r.data {
		if IsVirtualKey(provider) {
			s := Scope{Kind: KindVirtual, Provider: strings.TrimPrefix(provider, virtualPrefix)}
			for _, ms := range ps {
				for _, b := range ms {
					s.In += b.In
					s.Out += b.Out
					s.Total += b.Total
					s.Count += b.Count
				}
			}
			if s.Count > 0 {
				out = append(out, s)
			}
			continue
		}
		for model, ms := range ps {
			s := Scope{Kind: KindProvider, Provider: provider, Model: model}
			for _, b := range ms {
				s.In += b.In
				s.Out += b.Out
				s.Total += b.Total
				s.Count += b.Count
			}
			if s.Count > 0 {
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			// 真实上游排在虚拟组前面：统计页默认选中真实上游。
			return out[i].Kind == KindProvider
		}
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// RenameVirtual 把虚拟组的历史流量从旧名迁到新名。
//
// 不迁的话，改个名字就等于历史流量凭空消失：用户看到的是「新组从零开始」，
// 而旧组的数据永远留在文件里、界面上又选不到。
func (r *Recorder) RenameVirtual(oldName, newName string) {
	oldName, newName = strings.TrimSpace(oldName), strings.TrimSpace(newName)
	if oldName == "" || newName == "" || oldName == newName {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	src := r.data[VirtualKey(oldName)]
	if len(src) == 0 {
		return
	}
	dstKey := VirtualKey(newName)
	dst, ok := r.data[dstKey]
	if !ok {
		dst = providerStats{}
		r.data[dstKey] = dst
	}
	// 新名下可能已有数据（先删后建同名组、或并发写入）：按桶相加，
	// 不能直接覆盖，否则新组的少量数据会把历史整段顶掉。
	for model, ms := range src {
		cur, ok := dst[model]
		if !ok {
			cur = map[string]Bucket{}
			dst[model] = cur
		}
		for hour, b := range ms {
			acc := cur[hour]
			acc.In += b.In
			acc.Out += b.Out
			acc.Total += b.Total
			acc.Count += b.Count
			cur[hour] = acc
		}
	}
	delete(r.data, VirtualKey(oldName))
	r.dirty = true
}

// DropVirtual 删除虚拟组的历史流量。
//
// 删组时一并清掉，避免「删掉再建同名组」把上一个组的流量继承过来——
// 那会让新组的统计从一开始就是错的。
func (r *Recorder) DropVirtual(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := VirtualKey(name)
	if _, ok := r.data[key]; !ok {
		return
	}
	delete(r.data, key)
	r.dirty = true
}

// addSummary 把一个桶累加进汇总。
func addSummary(s *Summary, b Bucket) {
	s.In += b.In
	s.Out += b.Out
	s.Total += b.Total
	s.Count += b.Count
}

// sumDay 把某一天内的所有小时桶相加。dateKey 形如 2006-01-02。
func sumDay(buckets map[string]Bucket, dateKey string) Bucket {
	var b Bucket
	prefix := dateKey + "T"
	for k, v := range buckets {
		if strings.HasPrefix(k, prefix) {
			b.In += v.In
			b.Out += v.Out
			b.Total += v.Total
			b.Count += v.Count
		}
	}
	return b
}

// pad2 把 0-99 的整数补成两位字符串。
func pad2(n int) string {
	return fmt.Sprintf("%02d", n)
}

// startOfDay 返回 t 所在本地日的 0 点。
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
