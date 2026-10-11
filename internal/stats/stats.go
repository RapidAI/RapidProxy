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
	Period     string  `json:"period"`   // day / week / month
	Unit       string  `json:"unit"`     // hour / day，趋势序列的粒度
	Provider   string  `json:"provider"` // 上游（组）ID
	Model      string  `json:"model"`    // 模型 ID；空串表示本组全部模型汇总
	Summary    Summary `json:"summary"`
	Series     []Point `json:"series"`
	StartLabel string  `json:"startLabel"`
	EndLabel   string  `json:"endLabel"`
}

// Scope 是「有统计数据的 (组, 模型)」组合，供前端默认选中。
type Scope struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	In       int64  `json:"in"`
	Out      int64  `json:"out"`
	Total    int64  `json:"total"`
	Count    int64  `json:"count"`
}

// 小时键格式（本地时间）：2006-01-02T15。
const hourKeyLayout = "2006-01-02T15"

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

	ps, ok := r.data[e.Provider]
	if !ok {
		ps = providerStats{}
		r.data[e.Provider] = ps
	}
	ms, ok := ps[e.Model]
	if !ok {
		ms = map[string]Bucket{}
		ps[e.Model] = ms
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
// 周期定义（本地时间，相对 now）：
//   - day：当天 0 点起的 24 个小时点，趋势粒度 hour；
//   - week：最近 7 天（含今天），趋势粒度 day；
//   - month：最近 30 天（含今天），趋势粒度 day。
func (r *Recorder) Query(provider, model, period string, now time.Time) Result {
	r.mu.Lock()
	defer r.mu.Unlock()

	now = now.Local()
	res := Result{Period: period, Provider: provider, Model: model, Unit: "hour"}
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

	buckets := r.collectLocked(provider, model)

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
	return res
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
		// 跨组汇总：遍历所有组（model 过滤在 aggregate 内按组生效）。
		for _, ps := range r.data {
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
func (r *Recorder) Scopes() []Scope {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Scope, 0)
	for provider, ps := range r.data {
		for model, ms := range ps {
			s := Scope{Provider: provider, Model: model}
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
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return false
	})
	return out
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
