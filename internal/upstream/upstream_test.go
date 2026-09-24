package upstream

import (
	"testing"

	"github.com/znsoftm/RapidProxy/internal/config"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(config.WorkBuddyPreset(), config.DefaultPlatform, config.DefaultProduct)
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	return c
}

// 上游 /v3/config 常常漏掉一部分实际可调用的模型，这时必须以内置白名单里的
// 名称与上下文/输出上限为准，不能退化成 0 与默认值。
func TestBuildModelListKeepsBuiltinSpecs(t *testing.T) {
	c := newTestClient(t)

	models := c.BuildModelList(nil, nil, nil)
	if len(models) == 0 {
		t.Fatal("内置白名单不应为空")
	}

	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}

	specs := Allowlist("workbuddy")
	if len(models) != len(specs) {
		t.Errorf("模型数量应为白名单长度 %d，实际 %d", len(specs), len(models))
	}
	for _, spec := range specs {
		got, ok := byID[spec.ID]
		if !ok {
			t.Fatalf("白名单模型 %s 缺失", spec.ID)
		}
		if got.Name != spec.Name {
			t.Errorf("%s 名称应为 %q，实际 %q", spec.ID, spec.Name, got.Name)
		}
		if got.ContextLength != spec.ContextLength {
			t.Errorf("%s 上下文应为 %d，实际 %d", spec.ID, spec.ContextLength, got.ContextLength)
		}
		if got.MaxOutputTokens != spec.MaxOutput {
			t.Errorf("%s 输出上限应为 %d，实际 %d", spec.ID, spec.MaxOutput, got.MaxOutputTokens)
		}
	}
}

// 上游返回的信息只用于补充/覆盖，不能把白名单里已有的值抹掉。
func TestBuildModelListMergesUpstreamInfo(t *testing.T) {
	c := newTestClient(t)

	upstream := []UpstreamModel{
		{ID: "gpt-5.4", Name: "GPT-5.4 上游名", MaxInputTokens: 400000, MaxOutputTok: 64000, SupportsImages: true},
		{ID: "brand-new-model", Name: "全新模型", MaxInputTokens: 128000, MaxOutputTok: 16000},
	}
	models := c.BuildModelList(nil, nil, upstream)
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}

	if got := byID["gpt-5.4"]; got.Name != "GPT-5.4 上游名" || got.ContextLength != 400000 ||
		got.MaxOutputTokens != 64000 || !got.SupportsImages {
		t.Errorf("上游信息未生效: %+v", got)
	}
	if got, ok := byID["brand-new-model"]; !ok {
		t.Error("上游新增模型未出现在列表里")
	} else if got.MaxOutputTokens != 16000 {
		t.Errorf("新增模型输出上限应为 16000，实际 %d", got.MaxOutputTokens)
	}
}

func TestBuildModelListHonoursDisabledAndExtra(t *testing.T) {
	c := newTestClient(t)

	models := c.BuildModelList([]string{"custom-model", "gpt-5.4"}, []string{"gpt-5.5"}, nil)
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if _, ok := byID["gpt-5.5"]; ok {
		t.Error("被禁用的模型不应出现")
	}
	if got, ok := byID["custom-model"]; !ok {
		t.Error("额外模型未加入")
	} else if got.MaxOutputTokens != defaultMaxOutput {
		t.Errorf("额外模型应有兜底输出上限 %d，实际 %d", defaultMaxOutput, got.MaxOutputTokens)
	}
	// 重复出现不应产生两条
	if len(byID) != len(models) {
		t.Errorf("模型列表存在重复: %d 条 / %d 个 ID", len(models), len(byID))
	}
}

func TestBuildModelListIsStableAndUnique(t *testing.T) {
	c := newTestClient(t)
	first := c.BuildModelList(nil, nil, nil)
	second := c.BuildModelList(nil, nil, nil)
	if len(first) != len(second) {
		t.Fatalf("两次构建结果长度不一致: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("第 %d 项顺序不稳定: %s vs %s", i, first[i].ID, second[i].ID)
		}
	}
}
