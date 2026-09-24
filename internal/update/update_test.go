package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"v1.1.0", "1.0.9", 1},
		{"V2.0", "1.9.9", 1},
		{"1.0", "1.0.0", 0},        // 缺段按 0
		{"1.0.0-beta", "1.0.0", 0}, // 后缀忽略
		{"1.10.0", "1.9.0", 1},     // 数字比较而非字典序
		{"1.0.0", "1.0.0.1", -1},
		{"", "0.0.1", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// assetFor 必须精确匹配当前平台的安装包文件名。
func TestAssetFor(t *testing.T) {
	assets := []ghAsset{
		{Name: "RapidProxy-darwin-universal.pkg", BrowserDownloadURL: "https://x/darwin.pkg"},
		{Name: "RapidProxy-linux-amd64.AppImage", BrowserDownloadURL: "https://x/linux.AppImage"},
		{Name: "RapidProxy-windows-amd64-setup.exe", BrowserDownloadURL: "https://x/win.exe"},
		{Name: "RapidProxy-windows-amd64.zip", BrowserDownloadURL: "https://x/win.zip"}, // 便携包不可选
	}
	if a := assetFor(assets, "windows", "amd64"); a == nil || a.Name != "RapidProxy-windows-amd64-setup.exe" {
		t.Errorf("windows 选包错误: %+v", a)
	}
	if a := assetFor(assets, "darwin", "arm64"); a == nil || !strings.HasSuffix(a.Name, "universal.pkg") {
		t.Errorf("darwin 应选 universal pkg: %+v", a)
	}
	if a := assetFor(assets, "linux", "amd64"); a == nil || !strings.HasSuffix(a.Name, ".AppImage") {
		t.Errorf("linux 应选 AppImage: %+v", a)
	}
	if a := assetFor(assets, "linux", "arm64"); a != nil {
		t.Errorf("linux/arm64 无资产，应返回 nil: %+v", a)
	}
	// 缺少 windows 资产时返回 nil
	if a := assetFor(assets[:2], "windows", "amd64"); a != nil {
		t.Errorf("缺资产时应返回 nil: %+v", a)
	}
}

// platformAsset 返回当前测试平台的安装包资产 JSON；name 为空时返回不含资产的片段。
func platformAsset(name, url string) string {
	if name == "" {
		return ""
	}
	return `{"name":"` + name + `","browser_download_url":"` + url + `","size":20}`
}

// expectedAsset 按 runtime.GOOS 返回 assetFor 应选中的资产名。
func expectedAsset() string {
	switch runtime.GOOS {
	case "windows":
		return "RapidProxy-windows-amd64-setup.exe"
	case "darwin":
		return "RapidProxy-darwin-universal.pkg"
	default:
		return "RapidProxy-linux-amd64.AppImage"
	}
}

// anotherAsset 返回一个「别的平台」的资产名（用于测试缺平台资产）。
func anotherAsset() string {
	if runtime.GOOS == "windows" {
		return "RapidProxy-linux-amd64.AppImage"
	}
	return "RapidProxy-windows-amd64-setup.exe"
}

// Check：有新版返回 hasUpdate=true；同版本 false；平台无资产报错。
func TestCheck(t *testing.T) {
	mk := func(tag string, withPlatformAsset bool) *httptest.Server {
		want := ""
		if withPlatformAsset {
			want = platformAsset(expectedAsset(), "https://x/match")
		} else {
			want = platformAsset(anotherAsset(), "https://x/other")
		}
		body := `{"tag_name":"` + tag + `","html_url":"https://github.com/r/r/releases/tag/` + tag + `","body":"notes","assets":[` +
			`{"name":"RapidProxy-darwin-universal.pkg","browser_download_url":"https://x/darwin.pkg","size":10},` +
			want +
			`]}`
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	}

	// 新版本且包含当前平台的安装包
	srv := mk("v9.9.9", true)
	rel, has, err := Check(context.Background(), srv.Client(), srv.URL, "r/r", "1.0.0")
	if err != nil || !has {
		t.Fatalf("应检出更新: has=%v err=%v", has, err)
	}
	if rel.Version != "9.9.9" || rel.Asset.Name != expectedAsset() || rel.Asset.URL != "https://x/match" {
		t.Errorf("解析错误: %+v", rel)
	}

	// 同版本
	rel, has, err = Check(context.Background(), srv.Client(), srv.URL, "r/r", "9.9.9")
	if err != nil || has {
		t.Fatalf("同版本不应更新: has=%v err=%v", has, err)
	}
	srv.Close()

	// 新版本但缺当前平台的资产 → 报错提示
	srv2 := mk("v9.9.9", false)
	_, has, err = Check(context.Background(), srv2.Client(), srv2.URL, "r/r", "1.0.0")
	if err == nil || has {
		t.Fatalf("缺平台资产应报错: has=%v err=%v", has, err)
	}
	if !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("错误信息应包含平台名: %v", err)
	}

	// 404
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if _, _, err := Check(context.Background(), srv404.Client(), srv404.URL, "r/r", "1.0.0"); err == nil {
		t.Fatal("404 应返回错误")
	}
}

// Download：写入内容、进度回调、.part 清理、大小校验。
func TestDownload(t *testing.T) {
	content := strings.Repeat("RapidProxyInstallerBytes!", 400) // 10KB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000000") // 与实际不符 → 应校验失败
		_, _ = w.Write([]byte(content))
	}))
	// 上面 ContentLength 不匹配的场景单独测；先测正常场景
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(content))
	}))
	defer srvOK.Close()

	dest := filepath.Join(t.TempDir(), "setup.exe")
	var lastDone int64
	asset := Asset{Name: "setup.exe", URL: srvOK.URL + "/setup.exe", Size: int64(len(content))}
	if err := Download(context.Background(), srvOK.Client(), asset, dest, func(d, tot int64) {
		lastDone = d
		if tot != int64(len(content)) {
			t.Errorf("total 应为已知大小 %d，实际 %d", len(content), tot)
		}
	}); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("内容不一致: %d vs %d 字节", len(got), len(content))
	}
	if lastDone != int64(len(content)) {
		t.Errorf("进度回调最终值错误: %d", lastDone)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error(".part 临时文件应被清理")
	}

	// ContentLength 与实际不符 → 报错且不落地
	if err := Download(context.Background(), srv.Client(), Asset{URL: srv.URL + "/bad"}, dest+".bad", nil); err == nil {
		t.Error("大小不符应报错")
	}
	if _, err := os.Stat(dest + ".bad"); !os.IsNotExist(err) {
		t.Error("失败的下载不应留下载文件")
	}
}
