package stats

import (
	"testing"
	"time"
)

// rec 是构造一条测试流量的简写。
func rec(provider, model string, total int64, virtual string) Entry {
	return Entry{
		Provider: provider, Model: model, Virtual: virtual,
		In: total, Total: total, At: time.Now(),
	}
}

// 一次虚拟组请求会同时写真实维度与虚拟维度（见 Record）。
// 跨组汇总必须只算一次，否则界面上「全局用量」会整整翻一倍。
func TestGlobalAggregateIgnoresVirtualShadow(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	r.Record(rec("workbuddy", "gpt-5.4", 100, "fast"))

	if got := r.Query("", "", "day", time.Now()).Summary.Total; got != 100 {
		t.Fatalf("跨组汇总应为 100（一次请求只算一次），实际 %d", got)
	}
	// 指定模型时同样只算真实维度那一条。
	if got := r.Query("", "gpt-5.4", "day", time.Now()).Summary.Total; got != 100 {
		t.Fatalf("按模型跨组汇总应为 100，实际 %d", got)
	}
}

// 真实维度与虚拟维度各自独立累加，互不干扰。
func TestVirtualDimensionIsSeparate(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	r.Record(rec("workbuddy", "gpt-5.4", 100, "fast"))
	r.Record(rec("codebuddy", "glm-5.3", 50, "fast"))

	if got := r.Query("fast", "", "day", time.Now()).Summary.Total; got != 150 {
		t.Errorf("虚拟组总量应为 150，实际 %d", got)
	}
	if got := r.Query("workbuddy", "", "day", time.Now()).Summary.Total; got != 100 {
		t.Errorf("workbuddy 应为 100，实际 %d", got)
	}
	if got := r.Query("", "", "day", time.Now()).Summary.Total; got != 150 {
		t.Errorf("全局应为 150，实际 %d", got)
	}
}

// 虚拟组的判定不能依赖「该组是否已有数据」：刚建好还没流量的组，
// 前端也要拿到 kind=virtual，否则控件会在两种形态间跳。
func TestQueryVirtualKindIsDeterministic(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	// 没有任何数据时，带前缀的查询仍应识别为虚拟组。
	res := r.Query(VirtualKey("brandnew"), "", "day", time.Now())
	if res.Kind != KindVirtual {
		t.Errorf("带 virtual: 前缀应判定为虚拟组，实际 %s", res.Kind)
	}
	if res.Provider != "brandnew" {
		t.Errorf("provider 应去掉前缀，实际 %s", res.Provider)
	}

	// 裸组名在有数据后也识别为虚拟组。
	r.Record(rec("workbuddy", "gpt-5.4", 10, "fast"))
	if res := r.Query("fast", "", "day", time.Now()); res.Kind != KindVirtual {
		t.Errorf("裸组名应判定为虚拟组，实际 %s", res.Kind)
	}
	// 真实上游不受影响。
	if res := r.Query("workbuddy", "", "day", time.Now()); res.Kind != KindProvider {
		t.Errorf("真实上游应判定为 provider，实际 %s", res.Kind)
	}
}

// 成员明细按 total 降序，并正确切分「上游/模型」键。
func TestMemberUsageSplitsProviderAndModel(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	r.Record(rec("workbuddy", "gpt-5.4", 100, "fast"))
	r.Record(rec("codebuddy", "glm-5.3", 300, "fast"))

	res := r.Query("fast", "", "day", time.Now())
	if len(res.Members) != 2 {
		t.Fatalf("应有 2 个成员明细，实际 %+v", res.Members)
	}
	// 降序：300 的在前。
	if res.Members[0].Provider != "codebuddy" || res.Members[0].Model != "glm-5.3" {
		t.Errorf("应按 total 降序，实际 %+v", res.Members)
	}
	if res.Members[0].Total != 300 {
		t.Errorf("首位成员 total 应为 300，实际 %d", res.Members[0].Total)
	}
	// 指定单个成员查询时不返回成员明细（那是组级视图才需要的）。
	if got := r.Query("fast", "workbuddy/gpt-5.4", "day", time.Now()); len(got.Members) != 0 {
		t.Errorf("指定成员时不应返回明细，实际 %+v", got.Members)
	}
}

// 改名把历史流量整段搬到新名，且旧名不再有数据。
func TestRenameVirtualMovesAllBuckets(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	r.Record(rec("workbuddy", "gpt-5.4", 100, "fast"))
	r.Record(rec("codebuddy", "glm-5.3", 50, "fast"))
	r.RenameVirtual("fast", "quick")

	if got := r.Query("fast", "", "day", time.Now()).Summary.Total; got != 0 {
		t.Errorf("旧名不应再有数据，实际 %d", got)
	}
	res := r.Query("quick", "", "day", time.Now())
	if res.Summary.Total != 150 {
		t.Errorf("新名应继承 150，实际 %d", res.Summary.Total)
	}
	if len(res.Members) != 2 {
		t.Errorf("成员明细应随迁移保留，实际 %+v", res.Members)
	}
}

// 改名到一个已有数据的新名时按桶相加，不能覆盖已有数据。
func TestRenameVirtualMergesIntoExisting(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	r.Record(rec("workbuddy", "gpt-5.4", 100, "old"))
	r.Record(rec("workbuddy", "gpt-5.4", 40, "new"))
	r.RenameVirtual("old", "new")

	if got := r.Query("new", "", "day", time.Now()).Summary.Total; got != 140 {
		t.Errorf("应按桶相加为 140，实际 %d", got)
	}
}

// 删除虚拟组要清掉它的历史，避免重建同名组时继承旧数据。
func TestDropVirtualClearsHistory(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	r.Record(rec("workbuddy", "gpt-5.4", 100, "fast"))
	r.DropVirtual("fast")

	if got := r.Query("fast", "", "day", time.Now()).Summary.Total; got != 0 {
		t.Errorf("删除后不应再有数据，实际 %d", got)
	}
	// 真实维度的数据不受影响：删的只是虚拟组视图。
	if got := r.Query("workbuddy", "", "day", time.Now()).Summary.Total; got != 100 {
		t.Errorf("真实维度应保留 100，实际 %d", got)
	}
}

// Scopes 里虚拟组只出现一条汇总（model 为空），真实上游排在前面。
func TestScopesSeparatesVirtualAndProvider(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	r.Record(rec("workbuddy", "gpt-5.4", 100, "fast"))
	r.Record(rec("codebuddy", "glm-5.3", 50, ""))

	scopes := r.Scopes()
	var virtual, providers int
	for _, s := range scopes {
		switch s.Kind {
		case KindVirtual:
			virtual++
			if s.Provider != "fast" || s.Model != "" {
				t.Errorf("虚拟组汇总条目应只有组名，实际 %+v", s)
			}
			if s.Total != 100 {
				t.Errorf("虚拟组总量应为 100，实际 %d", s.Total)
			}
		case KindProvider:
			providers++
		}
	}
	if virtual != 1 {
		t.Errorf("应有 1 条虚拟组汇总，实际 %d（%+v）", virtual, scopes)
	}
	if providers != 2 {
		t.Errorf("应有 2 条真实上游记录，实际 %d", providers)
	}
	if len(scopes) > 0 && scopes[0].Kind != KindProvider {
		t.Errorf("真实上游应排在前面，实际首条 %+v", scopes[0])
	}
}

// 不带 Virtual 的记录只写真实维度，不会凭空产生虚拟组数据。
func TestRecordWithoutVirtualLeavesNoVirtualData(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	r.Record(rec("workbuddy", "gpt-5.4", 100, ""))

	for _, s := range r.Scopes() {
		if s.Kind == KindVirtual {
			t.Fatalf("不应出现虚拟组条目，实际 %+v", s)
		}
	}
}

// 真实上游 ID 与虚拟组名同时存在时，统计页要能分别选中。
// 虚拟组用 virtual: 前缀寻址，真实上游用裸 ID。
func TestStatsDistinguishesProviderAndVirtualWithSameName(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	// 构造极端情况：有个上游 ID 恰好等于某个虚拟组名。
	// （配置层禁止，但手改配置或历史数据可能出现。）
	r.Record(Entry{Provider: "fast", Model: "real-model", Total: 100, At: time.Now()})
	r.Record(Entry{Provider: "workbuddy", Model: "gpt-5.4", Total: 50, At: time.Now(), Virtual: "fast"})

	real := r.Query("fast", "", "day", time.Now())
	virt := r.Query(VirtualKey("fast"), "", "day", time.Now())

	t.Logf("real kind=%s total=%d; virtual kind=%s total=%d",
		real.Kind, real.Summary.Total, virt.Kind, virt.Summary.Total)

	if real.Kind != KindProvider || real.Summary.Total != 100 {
		t.Errorf("裸 ID 应查到真实上游的 100，实际 kind=%s total=%d", real.Kind, real.Summary.Total)
	}
	if virt.Kind != KindVirtual || virt.Summary.Total != 50 {
		t.Errorf("virtual: 前缀应查到虚拟组的 50，实际 kind=%s total=%d", virt.Kind, virt.Summary.Total)
	}
}

// 前端始终用 virtual: 前缀寻址虚拟组，所以「无数据的新组」也必须
// 稳定返回 kind=virtual——这是统计页禁用模型下拉的依据。
func TestEmptyVirtualGroupWithPrefixIsVirtual(t *testing.T) {
	r := New(t.TempDir())
	defer r.Close()

	res := r.Query(VirtualKey("brandnew"), "", "day", time.Now())
	if res.Kind != KindVirtual {
		t.Fatalf("带前缀的空组应判定为虚拟组，实际 %s", res.Kind)
	}
	if res.Provider != "brandnew" {
		t.Errorf("provider 应去前缀，实际 %s", res.Provider)
	}
	if res.Summary.Total != 0 {
		t.Errorf("空组总量应为 0，实际 %d", res.Summary.Total)
	}
}
