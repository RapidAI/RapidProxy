package zcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIsAuthorizeURL(t *testing.T) {
	allowed := []string{
		"https://bigmodel.cn/login?appId=zcode",
		"https://www.bigmodel.cn/login",
		"https://zcode.chatglm.site/login",
		"https://zcode.z.ai/login",
		"http://127.0.0.1:1234/cb",
		"http://localhost:9/x",
	}
	for _, u := range allowed {
		if !IsAuthorizeURL(u) {
			t.Errorf("IsAuthorizeURL(%q) = false, want true", u)
		}
	}
	denied := []string{
		"http://evil.example.com/x",
		"https://zcode.chatglm.site.evil.com/x",
		"javascript:alert(1)",
		"https://user:pass@bigmodel.cn/x",
	}
	for _, u := range denied {
		if IsAuthorizeURL(u) {
			t.Errorf("IsAuthorizeURL(%q) = true, want false", u)
		}
	}
}

func TestPickOrgAndProjectPrefersDefaults(t *testing.T) {
	orgs := []bizOrganization{
		{OrganizationID: "org-other", OrganizationName: "其它机构", Projects: []bizProject{{ProjectID: "p-other", ProjectName: "其它项目"}}},
		{OrganizationID: "org-default", OrganizationName: "默认机构", Projects: []bizProject{
			{ProjectID: "p-other2", ProjectName: "x"},
			{ProjectID: "p-default", ProjectName: "默认项目"},
		}},
	}
	orgID, projectID := pickOrgAndProject(orgs)
	if orgID != "org-default" || projectID != "p-default" {
		t.Fatalf("pickOrgAndProject = %q/%q", orgID, projectID)
	}
	if orgID, projectID := pickOrgAndProject(nil); orgID != "" || projectID != "" {
		t.Fatalf("empty = %q/%q", orgID, projectID)
	}
}

func TestDecodeEnvelopeTolerantSuccess(t *testing.T) {
	payloads := []string{
		`{"code":0,"msg":"ok","data":{"x":1}}`,
		`{"code":200,"msg":"ok","data":{"x":1}}`,
		`{"code":"0","msg":"ok","data":{"x":1}}`,
		`{"code":"200","msg":"ok","data":{"x":1}}`,
		`{"code":null,"msg":"ok","data":{"x":1}}`,
		`{"msg":"ok","data":{"x":1}}`,
	}
	for _, p := range payloads {
		data, err := decodeEnvelope([]byte(p))
		if err != nil {
			t.Fatalf("%s: unexpected error %v", p, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s: empty data", p)
		}
	}
	if _, err := decodeEnvelope([]byte(`{"code":400,"msg":"bad"}`)); err == nil {
		t.Fatal("non-zero code should error")
	}
}

// TestStartLoginFallsBackToInternationalOrigin 验证国内站不可达时能落到国际站，
// 且轮询/换 Key 走同一套业务接口。
func TestStartLoginFallsBackToInternationalOrigin(t *testing.T) {
	// 国内站在初始化阶段返回 500（模拟不可用），国际站返回有效 flow。
	domestic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer domestic.Close()
	international := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth/cli/init"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"flow_id":           "flow-1",
					"authorize_url":     "https://zcode.z.ai/login?flow=flow-1",
					"expires_at":        9999999999,
					"poll_interval_sec": 1,
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer international.Close()

	oldOrigins := endpointOrigins
	endpointOrigins = []string{domestic.URL, international.URL}
	defer func() { endpointOrigins = oldOrigins }()

	login, err := StartLogin(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if login.AuthURL() != "https://zcode.z.ai/login?flow=flow-1" {
		t.Fatalf("auth url = %q", login.AuthURL())
	}
	if login.origin != international.URL {
		t.Fatalf("origin = %q, want %q", login.origin, international.URL)
	}
}

func TestResolveCodingPlanAPIKey(t *testing.T) {
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/customer/getCustomerInfo"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "data": map[string]any{
					"organizations": []any{map[string]any{
						"organizationId": "org-1", "organizationName": "默认机构",
						"projects": []any{map[string]any{"projectId": "p-1", "projectName": "默认项目"}},
					}},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/api_keys") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []any{}})
		case strings.HasSuffix(r.URL.Path, "/api_keys") && r.Method == http.MethodPost:
			created = true
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"apiKey": "abc"}})
		case strings.Contains(r.URL.Path, "/copy/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"secretKey": "sec"}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "not found")
		}
	}))
	defer srv.Close()

	oldOrigin := bigModelOrigin
	bigModelOrigin = srv.URL
	defer func() { bigModelOrigin = oldOrigin }()

	// bigModelOrigin 是裸 origin，路径里已经带 /api/...；把 srv.URL 里的主机
	// 直接当作 origin 即可（约定一致）。用 url.Parse 校验一下避免笔误。
	if _, err := url.Parse(bigModelOrigin); err != nil {
		t.Fatal(err)
	}

	key, err := ResolveCodingPlanAPIKey(context.Background(), srv.Client(), "access-token")
	if err != nil {
		t.Fatalf("ResolveCodingPlanAPIKey: %v", err)
	}
	if !created {
		t.Fatal("expected API key creation when list is empty")
	}
	if key != "abc.sec" {
		t.Fatalf("key = %q, want abc.sec", key)
	}
}
