// Package update 实现基于 GitHub Releases 的在线更新：
// 检查新版本 → 下载安装包 → 启动安装（由调用方负责退出当前程序）。
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo 是默认的 GitHub 仓库（owner/repo）。
const DefaultRepo = "RapidAI/RapidProxy"

// DefaultAPIBase 是 GitHub API 地址（测试时可替换）。
const DefaultAPIBase = "https://api.github.com"

// downloadTimeout 单个安装包的最大下载时长。
const downloadTimeout = 30 * time.Minute

// maxAssetSize 安装包大小上限（防呆）。
const maxAssetSize = int64(512) << 20

// Asset 是一个可下载的安装包。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"url"` // browser_download_url
	Size int64  `json:"size"`
}

// Release 是一个 GitHub Release 摘要。
type Release struct {
	Version string `json:"version"` // 不含 v 前缀，如 1.1.0
	Page    string `json:"page"`    // Release 页面链接
	Notes   string `json:"notes"`
	Asset   Asset  `json:"asset"` // 与当前平台匹配的安装包
}

// ghRelease 是 GitHub API 的响应结构（只取需要的字段）。
type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Body    string    `json:"body"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Check 查询仓库的 latest release，并返回是否存在更新。
//
// currentVersion 形如 "1.0.0"（可带 v 前缀）。网络错误、无匹配资产、
// 版本不高于当前时都返回 hasUpdate=false（前两者带 error）。
func Check(ctx context.Context, client *http.Client, apiBase, repo, currentVersion string) (*Release, bool, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimSuffix(apiBase, "/"), repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("访问 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, fmt.Errorf("仓库 %s 没有已发布的版本", repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("GitHub 返回 %d", resp.StatusCode)
	}

	var gh ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&gh); err != nil {
		return nil, false, fmt.Errorf("解析 Release 信息失败: %w", err)
	}

	rel := &Release{
		Version: strings.TrimPrefix(strings.TrimPrefix(gh.TagName, "v"), "V"),
		Page:    gh.HTMLURL,
		Notes:   gh.Body,
	}
	if asset := assetFor(gh.Assets, runtime.GOOS, runtime.GOARCH); asset != nil {
		rel.Asset = *asset
	}

	if CompareVersions(rel.Version, currentVersion) <= 0 {
		return rel, false, nil
	}
	if rel.Asset.URL == "" {
		// 有新版本但当前平台没有安装包资产
		return rel, false, fmt.Errorf("版本 %s 未提供适用于 %s/%s 的安装包", rel.Version, runtime.GOOS, runtime.GOARCH)
	}
	return rel, true, nil
}

// assetFor 按平台选择安装包资产，找不到返回 nil。
func assetFor(assets []ghAsset, goos, goarch string) *Asset {
	// wants 按优先级排列：Windows 安装包内含 amd64+386（架构由安装程序
	// 自动选择），并兼容仅 amd64 的旧版命名。
	var wants []string
	switch {
	case goos == "windows":
		wants = []string{"RapidProxy-windows-setup.exe", "RapidProxy-windows-amd64-setup.exe"}
	case goos == "darwin": // universal 包同时覆盖 amd64 / arm64
		wants = []string{"RapidProxy-darwin-universal.pkg"}
	case goos == "linux" && goarch == "amd64":
		wants = []string{"RapidProxy-linux-amd64.AppImage"}
	default:
		return nil
	}
	for _, want := range wants {
		for i := range assets {
			if assets[i].Name == want {
				return &Asset{Name: assets[i].Name, URL: assets[i].BrowserDownloadURL, Size: assets[i].Size}
			}
		}
	}
	return nil
}

// CompareVersions 比较 x[.y[.z...]] 数字版本号：a>b 返回 1，相等 0，a<b 返回 -1。
// 忽略 v/V 前缀；无法解析的段按 0 处理。
func CompareVersions(a, b string) int {
	as := splitVersion(a)
	bs := splitVersion(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// splitVersion 解析 "v1.2.3-beta" → [1,2,3]（忽略后缀与非数字段）。
func splitVersion(v string) []int {
	v = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(v), "v"), "V")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		// 取每段开头的连续数字（容忍 "3rc1" 之类）
		j := 0
		for j < len(p) && p[j] >= '0' && p[j] <= '9' {
			j++
		}
		n, _ := strconv.Atoi(p[:j])
		out = append(out, n)
	}
	return out
}

// Download 把资产下载到 dest，进度通过 progress 汇报（done/total 字节，
// total 未知时为 0）。先写 .part 临时文件，成功后原子改名。
func Download(ctx context.Context, client *http.Client, a Asset, dest string, progress func(done, total int64)) error {
	if client == nil {
		client = http.DefaultClient
	}
	if a.URL == "" {
		return fmt.Errorf("下载地址为空")
	}
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 %d", resp.StatusCode)
	}

	total := resp.ContentLength
	if total < 0 {
		total = a.Size
	}
	if total > maxAssetSize {
		return fmt.Errorf("安装包过大（%d MB），放弃下载", total>>20)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	var done int64
	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(tmp)
				return fmt.Errorf("写入失败: %w", werr)
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("下载中断: %w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if total > 0 && done != total {
		os.Remove(tmp)
		return fmt.Errorf("下载不完整: %d/%d 字节", done, total)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// LaunchInstaller 启动已下载的安装包（不等待安装完成）。
//
//   - windows：直接运行 NSIS 安装向导（调用方随后应退出本程序，避免文件被占用）
//   - darwin：用 open 打开 PKG 安装器
//   - linux：AppImage 不走安装器，由调用方做原地替换（返回 ErrNoInstaller）
func LaunchInstaller(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command(path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return ErrNoInstaller
	}
}

// ErrNoInstaller 表示当前平台没有「安装器」概念（Linux AppImage 走原地替换）。
var ErrNoInstaller = fmt.Errorf("linux 平台通过替换 AppImage 更新，无需安装器")
