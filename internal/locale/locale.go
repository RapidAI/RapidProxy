// Package locale 提供极简的界面文案双语支持（简体中文 / English）。
//
// 设计约束：
//   - 只覆盖 Go 侧「用户可见」的文案：托盘菜单、系统对话框、更新与登录
//     提示、有效期描述等；运行日志保持中文不做翻译（日志面向排障）。
//   - 前端（webview）文案不经过这里，由 frontend/dist/i18n.js 独立维护。
//   - 文案缺失时回退中文，仍缺失则原样返回 key，便于发现漏配。
//
// 当前语言在包初始化时按系统语言探测；应用启动读入配置后调用 Set 覆盖。
package locale

import (
	"fmt"
	"sync"
)

// Lang 是界面语言代码。
type Lang string

const (
	// Zh 简体中文。
	Zh Lang = "zh"
	// En 英文。
	En Lang = "en"
)

// 语言偏好合法值（与配置文件、前端下拉框约定一致）。
const (
	PrefSystem = "system"
	PrefZh     = "zh"
	PrefEn     = "en"
)

var (
	mu      sync.RWMutex
	current = Detect()
)

// Set 设置当前语言。
func Set(l Lang) {
	if l != En {
		l = Zh
	}
	mu.Lock()
	current = l
	mu.Unlock()
}

// Current 返回当前语言。
func Current() Lang {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// String 实现 fmt.Stringer。
func (l Lang) String() string { return string(l) }

// FromPref 把语言偏好（system / zh / en）解析成具体语言。
// "system" 及未知值按系统语言探测。
func FromPref(pref string) Lang {
	switch pref {
	case PrefZh:
		return Zh
	case PrefEn:
		return En
	default:
		return Detect()
	}
}

type entry struct{ zh, en string }

// catalog 是 Go 侧用户可见文案表。占位符用 fmt.Sprintf 动词（%s/%d/%v）。
var catalog = map[string]entry{
	// ---- 单实例提示 ----
	"dlg.alreadyRunning": {
		"%s 已经在运行了（见系统托盘）。\n无需重复启动。",
		"%s is already running (see the system tray).\nNo need to start it again.",
	},
	"notify.ok": {"好", "OK"},

	// ---- 托盘菜单 ----
	"tray.show":        {"显示主界面", "Show Main Window"},
	"tray.showTip":     {"打开 RapidProxy 窗口", "Open the RapidProxy window"},
	"tray.copyURL":     {"复制接口地址", "Copy API URL"},
	"tray.copyURLTip":  {"复制 OpenAI base_url 到剪贴板", "Copy the OpenAI base_url to the clipboard"},
	"tray.resetWindow": {"重置窗口位置", "Reset Window Position"},
	"tray.resetTip":    {"窗口超出屏幕或找不到时重置尺寸并居中", "Re-fit and center the window when it goes off-screen or goes missing"},
	"tray.statusIdle":  {"服务：未启动", "Service: Stopped"},
	"tray.statusRun":   {"服务：运行中 %s", "Service: Running %s"},
	"tray.statusTip":   {"代理服务当前状态", "Proxy service status"},
	"tray.start":       {"启动服务", "Start Service"},
	"tray.startTip":    {"启动或停止代理服务", "Start or stop the proxy service"},
	"tray.stop":        {"停止服务", "Stop Service"},
	"tray.tipIdle":     {"RapidProxy · 未启动", "RapidProxy · Stopped"},
	"tray.tipRun":      {"RapidProxy · 运行中 %s", "RapidProxy · Running %s"},
	"tray.quit":        {"退出", "Quit"},
	"tray.quitTip":     {"退出 RapidProxy", "Quit RapidProxy"},

	// ---- 最小化到托盘提示 ----
	"dlg.minimized.title": {"RapidProxy 仍在运行", "RapidProxy Is Still Running"},
	"dlg.minimized.msg": {
		"程序已最小化到系统托盘，代理服务继续在后台运行。\n\n要重新打开界面，请点击或双击托盘图标；要完全退出，请在托盘图标右键菜单中选择「退出」。",
		"The app has been minimized to the system tray; the proxy service keeps running in the background.\n\nTo reopen the window, click or double-click the tray icon. To quit completely, right-click the tray icon and choose \"Quit\".",
	},
	"dlg.minimized.ok": {"知道了", "Got it"},

	// ---- 登录 ----
	"login.browserOpened":      {"已在系统浏览器打开官方登录页，请在该窗口完成授权", "The official sign-in page was opened in your default browser — finish authorizing there"},
	"login.importing":          {"已找到本机保存的 OpenCode 密钥（%s），等待确认", "Found the OpenCode key saved on this device (%s) — waiting for confirmation"},
	"login.pasteKey":           {"请在浏览器中创建 OpenCode 密钥，并粘贴到下方输入框", "Create an OpenCode key in your browser, then paste it into the box below"},
	"login.pasteKey.custom":    {"请在 %s 控制台创建 API Key，并粘贴到下方输入框", "Create an API key in the %s console, then paste it into the box below"},
	"login.browserOAuth":       {"已在系统浏览器打开 %s 授权页，完成授权后会自动登录", "The %s sign-in page was opened in your browser — you'll be signed in automatically once you approve"},
	"login.pasteCode":          {"已在系统浏览器打开 %s 授权页：完成授权后，把页面显示的授权码粘贴到下方输入框", "The %s sign-in page was opened in your browser — after approving, paste the authorization code shown there into the box below"},
	"login.codeEmpty":          {"请输入授权码", "Enter the authorization code"},
	"login.codeNoFlow":         {"没有等待授权码的登录流程，请重新发起登录", "There is no sign-in waiting for a code — please start again"},
	"login.oauthNoKey":         {"%s 使用浏览器授权登录，不支持粘贴 API Key", "%s signs in through the browser and does not accept a pasted API key"},
	"login.keyInvalid":         {"OpenCode 密钥无效，请检查后重试", "That OpenCode key looks invalid — please check it and try again"},
	"login.keyEmpty":           {"请输入 OpenCode 密钥，或直接确认使用本机已保存的密钥", "Enter an OpenCode key, or confirm the one saved on this device"},
	"login.keyEmpty.custom":    {"请输入 %s 的 API Key", "Enter the %s API key"},
	"login.keyRejected":        {"OpenCode 密钥被上游拒绝，请确认密钥是否正确、未过期", "The OpenCode key was rejected by the server — make sure it is correct and still valid"},
	"login.keyInvalid.custom":  {"密钥无效，请检查后重试", "That key looks invalid — please check it and try again"},
	"login.keyRejected.custom": {"密钥被上游拒绝，请确认密钥是否正确、未过期", "The key was rejected by the server — make sure it is correct and still valid"},
	"login.timeout":            {"登录超时或已取消", "Sign-in timed out or was cancelled"},
	"login.expired":            {"登录链接已过期，请重新发起登录", "The sign-in link has expired — please start again"},
	"login.saveFailed":         {"保存凭据失败: %s", "Failed to save credentials: %s"},
	"login.success":            {"登录成功", "Signed in"},
	"login.unknownProvider":    {"未知的上游: %s", "Unknown provider: %s"},

	// ---- 自定义服务商 ----
	"provider.nameEmpty":  {"服务商名称不能为空", "The provider name must not be empty"},
	"provider.urlEmpty":   {"接口地址（Base URL）不能为空", "The base URL must not be empty"},
	"provider.urlInvalid": {"接口地址无效，需为 http(s):// 开头的完整地址", "Invalid base URL — it must be a full http(s):// URL"},
	"provider.modelEmpty": {"模型 ID 不能为空", "The model ID must not be empty"},

	// ---- API Key ----
	"key.empty":     {"API Key 不能为空", "The API key must not be empty"},
	"key.duplicate": {"该 API Key 已存在", "This API key already exists"},
	"key.notFound":  {"未找到该 API Key", "API key not found"},

	// ---- 在线更新 ----
	"upd.availableSettings": {"发现新版本 v%s，可在「关于」页下载安装", "New version v%s found — download and install it from the About page"},
	"upd.latest":            {"当前已是最新版本", "You are already on the latest version"},
	"upd.available":         {"发现新版本 v%s，可下载安装包（%s）", "New version v%s is available (%s)"},
	"upd.downloadStart":     {"开始下载 %s", "Starting download of %s"},
	"upd.downloading":       {"下载中 %s / %s", "Downloading %s / %s"},
	"upd.downloadFailed":    {"下载失败：%s", "Download failed: %s"},
	"upd.launching":         {"正在启动安装程序…", "Launching the installer…"},
	"upd.launchFailed":      {"启动安装程序失败：%s", "Failed to launch the installer: %s"},
	"upd.replacingAppImage": {"正在替换 AppImage…", "Replacing the AppImage…"},
	"upd.chmodFailed":       {"设置执行权限失败：%s", "Failed to set execute permission: %s"},
	"upd.replaceFailed":     {"替换程序失败：%s", "Failed to replace the app: %s"},
	"upd.appImageReplaced":  {"AppImage 已替换为 v%s，正在重启", "AppImage replaced with v%s — restarting"},
	"upd.doneRestarting":    {"更新完成，正在重启…", "Update complete — restarting…"},
	"upd.manualInstall":     {"安装包已下载到 %s，请手动运行安装", "The installer was downloaded to %s — run it to finish the update"},
	"upd.checkFirst":        {"请先检查更新", "Check for updates first"},

	// ---- 启动安装包（UAC）----
	"launcher.cancelled":  {"已取消安装授权（UAC）", "Installation authorization (UAC) was cancelled"},
	"launcher.failed":     {"启动安装包失败: %v", "Failed to launch the installer: %v"},
	"launcher.notWindows": {"仅 Windows 支持 runas 启动安装包", "Only Windows can launch the installer via runas"},

	// ---- 有效期描述 ----
	"dur.expired": {"已过期", "Expired"},
	"dur.dayHour": {"%d 天 %d 小时后过期", "Expires in %dd %dh"},
	"dur.hourMin": {"%d 小时 %d 分钟后过期", "Expires in %dh %dmin"},
	"dur.minute":  {"%d 分钟后过期", "Expires in %dmin"},

	// ---- 语言设置 ----
	"lang.invalid": {"不支持的语言: %s（可选 system / zh / en）", "Unsupported language: %s (expected system / zh / en)"},
}

// T 返回当前语言的文案；args 非空时按 fmt.Sprintf 格式化。
// 缺失的 key 回退中文，仍缺失则原样返回 key 本身。
func T(id string, args ...any) string {
	lang := Current()
	e, ok := catalog[id]
	if !ok {
		return id
	}
	s := e.zh
	if lang == En {
		s = e.en
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}
