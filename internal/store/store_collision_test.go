package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/znsoftm/RapidProxy/internal/fsx"
	"github.com/znsoftm/RapidProxy/internal/upstream"
)

// safeName 把非法字符替换成 "_" 会引入撞名：`a/b` 与 `a\b` 都变成 `a_b`，
// 两个账号写进同一个文件，后保存的静默覆盖先保存的（令牌随之错乱）。
// UID 来自上游、不可控，所以这是会真实发生的。
func TestSafeNameNoCollision(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &upstream.Credential{
		Provider: "workbuddy", ID: `a/b`,
		Auth: upstream.Tokens{AccessToken: "TOKEN-A"},
	}
	b := &upstream.Credential{
		Provider: "workbuddy", ID: `a\b`,
		Auth: upstream.Tokens{AccessToken: "TOKEN-B"},
	}
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(b); err != nil {
		t.Fatal(err)
	}

	gotA, err := s.Get("workbuddy", `a/b`)
	if err != nil {
		t.Fatalf("读取 a/b 失败: %v", err)
	}
	if gotA.Auth.AccessToken != "TOKEN-A" {
		t.Errorf("a/b 的凭据被覆盖了，实际 token=%s", gotA.Auth.AccessToken)
	}
	gotB, err := s.Get("workbuddy", `a\b`)
	if err != nil {
		t.Fatalf("读取 a\\b 失败: %v", err)
	}
	if gotB.Auth.AccessToken != "TOKEN-B" {
		t.Errorf("a\\b 的凭据被覆盖了，实际 token=%s", gotB.Auth.AccessToken)
	}

	// 两个账号必须都在列表里，且各出现一次。
	creds, err := s.List("workbuddy")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Fatalf("应有 2 个账号，实际 %d", len(creds))
	}
}

// 命名规则升级不能弄丢已有凭据：旧版写下的文件必须还能读到。
//
// safeName 改算法后，含非法字符的 ID 落盘文件名会变，
// 如果不做兼容，这些账号在用户升级后会「凭空消失」——里面存着有效的登录态。
func TestLegacyCredentialFileStillReadable(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 按旧规则手工落盘：ID `a/b` 写进 a_b.json。
	legacyPath := filepath.Join(dir, "accounts", "workbuddy", "a_b.json")
	raw, _ := json.MarshalIndent(&upstream.Credential{
		Provider: "workbuddy", ID: `a/b`,
		Auth: upstream.Tokens{AccessToken: "LEGACY-TOKEN"},
	}, "", "  ")
	if err := fsx.AtomicWrite(legacyPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	// 新代码必须能读回它。
	got, err := s.Get("workbuddy", `a/b`)
	if err != nil {
		t.Fatalf("旧格式的凭据读不到了: %v", err)
	}
	if got.Auth.AccessToken != "LEGACY-TOKEN" {
		t.Errorf("读到的令牌不对: %s", got.Auth.AccessToken)
	}

	// 也要出现在 List 里，且只出现一次。
	creds, _ := s.List("workbuddy")
	if len(creds) != 1 || creds[0].Auth.AccessToken != "LEGACY-TOKEN" {
		t.Fatalf("List 结果不对: %+v", creds)
	}

	// 保存后应迁移到新文件名，并清掉旧文件（否则 List 会读出两份）。
	if err := s.Save(got); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); err == nil {
		t.Error("旧文件未清理，List 会把同一个账号读出两次")
	}
	creds, _ = s.List("workbuddy")
	if len(creds) != 1 {
		t.Fatalf("迁移后仍应只有 1 个账号，实际 %d", len(creds))
	}
	if migrated, err := s.Get("workbuddy", `a/b`); err != nil || migrated.Auth.AccessToken != "LEGACY-TOKEN" {
		t.Errorf("迁移后读不到凭据: %+v err=%v", migrated, err)
	}
}

// 旧格式的凭据也必须能被删除（否则界面上「删除账号」对老用户失效）。
func TestDeleteLegacyCredentialFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	legacyPath := filepath.Join(dir, "accounts", "workbuddy", "a_b.json")
	raw, _ := json.MarshalIndent(&upstream.Credential{
		Provider: "workbuddy", ID: `a/b`,
		Auth: upstream.Tokens{AccessToken: "LEGACY"},
	}, "", "  ")
	if err := fsx.AtomicWrite(legacyPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete("workbuddy", `a/b`); err != nil {
		t.Fatalf("删除旧格式凭据失败: %v", err)
	}
	if _, err := os.Stat(legacyPath); err == nil {
		t.Error("旧文件未被删除")
	}
	creds, _ := s.List("workbuddy")
	if len(creds) != 0 {
		t.Errorf("删除后仍列出 %d 个账号", len(creds))
	}
}

// 普通 ID（不含非法字符）的文件名不能变，否则所有用户的凭据都要迁移。
func TestSafeNameKeepsPlainIDsUnchanged(t *testing.T) {
	cases := map[string]string{
		"workbuddy-abc123": "workbuddy-abc123",
		"":                 "unknown",
		".":                "unknown",
		"..":               "unknown",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q，期望 %q", in, got, want)
		}
	}
	// legacyName 与 safeName 对普通 ID 必须一致（无需迁移）。
	if legacyName("workbuddy-abc123") != safeName("workbuddy-abc123") {
		t.Error("普通 ID 不应触发迁移")
	}
}

// 登录时间相同的账号（同毫秒内批量登录）排序必须稳定，
// 否则界面上的账号顺序会在两次刷新之间跳动。
func TestListOrderStableWhenTimestampsEqual(t *testing.T) {
	s, _ := New(t.TempDir())
	same := time.Now()
	ids := []string{"z", "y", "x", "w", "v"}
	for _, id := range ids {
		if err := s.Save(&upstream.Credential{
			Provider: "workbuddy", ID: id, CreatedAt: same,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var first string
	for i := 0; i < 10; i++ {
		creds, _ := s.List("workbuddy")
		order := ""
		for _, c := range creds {
			order += c.ID + ","
		}
		if i == 0 {
			first = order
			continue
		}
		if order != first {
			t.Fatalf("第 %d 次顺序变了: %s != %s", i, order, first)
		}
	}
}
