package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znsoftm/RapidProxy/internal/upstream"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("创建凭据仓库失败: %v", err)
	}
	return st
}

func cred(provider, id, uid string) *upstream.Credential {
	return &upstream.Credential{
		Provider: provider,
		ID:       id,
		Label:    provider + " · " + uid,
		Auth: upstream.Tokens{
			AccessToken:  "access-" + id,
			RefreshToken: "refresh-" + id,
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		},
		Account: upstream.Account{UID: uid, Nickname: "用户 " + uid},
	}
}

func TestSaveListGetDelete(t *testing.T) {
	st := newStore(t)

	if err := st.Save(cred("workbuddy", "workbuddy-1", "1")); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := st.Save(cred("codebuddy", "codebuddy-2", "2")); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	all, err := st.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("应有 2 个凭据，实际 %d", len(all))
	}

	onlyWorkBuddy, err := st.List("workbuddy")
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyWorkBuddy) != 1 || onlyWorkBuddy[0].ID != "workbuddy-1" {
		t.Fatalf("按上游过滤失败: %+v", onlyWorkBuddy)
	}

	got, err := st.Get("workbuddy", "workbuddy-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Auth.AccessToken != "access-workbuddy-1" {
		t.Errorf("凭据内容不匹配: %+v", got.Auth)
	}
	if got.CreatedAt.IsZero() {
		t.Error("保存时应写入 CreatedAt")
	}

	if err := st.Delete("workbuddy", "workbuddy-1"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := st.Get("workbuddy", "workbuddy-1"); !os.IsNotExist(err) {
		t.Errorf("删除后应读不到，实际 err=%v", err)
	}
	if st.Count("codebuddy") != 1 {
		t.Error("删除其它上游的凭据不应受影响")
	}
}

func TestSaveRequiresIdentity(t *testing.T) {
	st := newStore(t)
	if err := st.Save(&upstream.Credential{Provider: "workbuddy"}); err == nil {
		t.Error("缺少 ID 时应报错")
	}
	if err := st.Save(nil); err == nil {
		t.Error("空凭据应报错")
	}
}

func TestSensitiveNamesCannotEscapeDirectory(t *testing.T) {
	st := newStore(t)
	credential := cred("workbuddy", "../../evil", "x")
	if err := st.Save(credential); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	root := st.Root()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	// 不能出现仓库目录之外的新目录
	if len(entries) != 1 || entries[0].Name() != "workbuddy" {
		t.Fatalf("凭据目录结构异常: %+v", entries)
	}
	files, _ := os.ReadDir(filepath.Join(root, "workbuddy"))
	if len(files) != 1 {
		t.Fatalf("凭据文件数量异常: %+v", files)
	}
	name := files[0].Name()
	// 路径分隔符必须被转义掉，否则 "../../evil" 会写到目录之外。
	if strings.ContainsAny(strings.TrimSuffix(name, ".json"), `/\`) {
		t.Errorf("文件名仍含路径分隔符: %q", name)
	}
	// 含非法字符的 ID 会追加原始值的短哈希，避免 a/b 与 a\b 撞成同一文件。
	if want := safeName("../../evil") + ".json"; name != want {
		t.Errorf("文件名 = %q，期望 %q", name, want)
	}
	// 无论怎么转义，文件都必须落在凭据目录内。
	if filepath.Dir(filepath.Join(root, "workbuddy", name)) != filepath.Join(root, "workbuddy") {
		t.Errorf("凭据文件写到了目录之外: %q", name)
	}
}

func TestListMissingDirectoryIsEmpty(t *testing.T) {
	st := newStore(t)
	creds, err := st.List("")
	if err != nil {
		t.Fatalf("空仓库不应报错: %v", err)
	}
	if len(creds) != 0 {
		t.Errorf("空仓库应返回 0 个凭据，实际 %d", len(creds))
	}
}

// 重新登录同一账号不能把用户设的权重 / 暂停状态清掉。
//
// 凭据 ID 是 provider+uid，重新登录算出同一个 ID，Save 又是全量覆盖。
// 不做保留的话，用户每次重新登录都要把权重和「暂停」重设一遍。
func TestSavePreservingLocalFieldsKeepsWeightAndPaused(t *testing.T) {
	st := newStore(t)
	cred := &upstream.Credential{
		Provider: "workbuddy", ID: "wb1",
		Auth:    upstream.Tokens{AccessToken: "tok-1"},
		Account: upstream.Account{UID: "u1"},
		Weight:  7, Paused: true,
	}
	if err := st.Save(cred); err != nil {
		t.Fatal(err)
	}

	// 模拟重新登录：全新的凭据对象，权重和暂停都是零值。
	relogin := &upstream.Credential{
		Provider: "workbuddy", ID: "wb1",
		Auth:    upstream.Tokens{AccessToken: "tok-2"},
		Account: upstream.Account{UID: "u1"},
	}
	if err := st.SavePreservingLocalFields(relogin); err != nil {
		t.Fatal(err)
	}

	got, err := st.Get("workbuddy", "wb1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Weight != 7 || !got.Paused {
		t.Errorf("重新登录应保留权重与暂停状态，实际 weight=%d paused=%v", got.Weight, got.Paused)
	}
	if got.Auth.AccessToken != "tok-2" {
		t.Errorf("令牌应被更新为新值，实际 %q", got.Auth.AccessToken)
	}
}

// 普通 Save 必须原样落盘：UpdateCredential 之类的读-改-写靠它生效，
// 任何「保留旧值」的启发式都会把用户刚改的字段顶回去。
//
// 尤其权重 0 —— 它是合法取值（意为默认同权），不能被当成「未设置」。
func TestSaveDoesNotMergeOldLocalFields(t *testing.T) {
	st := newStore(t)
	cred := &upstream.Credential{
		Provider: "workbuddy", ID: "wb1",
		Auth:   upstream.Tokens{AccessToken: "tok-1"},
		Weight: 7, Paused: true,
	}
	if err := st.Save(cred); err != nil {
		t.Fatal(err)
	}

	// 用户把权重改回 0（默认同权）并恢复账号。
	updated := &upstream.Credential{
		Provider: "workbuddy", ID: "wb1",
		Auth: upstream.Tokens{AccessToken: "tok-1"},
	}
	if err := st.Save(updated); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get("workbuddy", "wb1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Weight != 0 || got.Paused {
		t.Errorf("Save 应原样落盘，实际 weight=%d paused=%v", got.Weight, got.Paused)
	}
}
