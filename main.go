// RapidProxy —— 把 WorkBuddy / CodeBuddy 背后的模型封装成 OpenAI 兼容服务。
//
// 程序形态：Wails 桌面应用 + 系统托盘。窗口关闭只最小化到托盘，
// 只有在托盘菜单选择「退出」才会真正结束进程。
package main

import (
	"embed"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	application := NewApp()

	// 托盘必须在主事件循环启动之前注册，由 Wails 的事件循环驱动。
	application.startTray()

	err := wails.Run(&options.App{
		Title: "RapidProxy",
		// 尺寸只是期望值：窗口创建后 App.fitWindow 会按当前屏幕的缩放比例与
		// 可用区域收敛一次，避免在高 DPI / 小屏上窗口跑到屏幕外。
		Width:            windowDefaultWidth,
		Height:           windowDefaultHeight,
		MinWidth:         windowMinWidth,
		MinHeight:        windowMinHeight,
		DisableResize:    false,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 244, G: 245, B: 247, A: 255},
		OnStartup:        application.startup,
		OnBeforeClose:    application.beforeClose,
		Bind:             []any{application},
	})
	if err != nil {
		// 界面不可用时（例如缺少 WebView2 运行时）退化为后台服务 + 托盘，
		// 保证 OpenAI 接口仍然可用。
		application.log.Errorf("界面启动失败: %v", err)
		application.runHeadless()
	}
	os.Exit(0)
}
