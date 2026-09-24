package store

import (
	"os"
	"path/filepath"
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
	if name := files[0].Name(); name != ".._.._evil.json" {
		t.Errorf("非法字符未被转义: %q", name)
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
