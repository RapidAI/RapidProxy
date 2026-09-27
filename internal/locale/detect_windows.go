//go:build windows

package locale

import "syscall"

var (
	procGetUserDefaultUILanguage = syscall.NewLazyDLL("kernel32.dll").NewProc("GetUserDefaultUILanguage")
)

// Detect 读取系统默认 UI 语言。zh 系（简体/繁体/各区域变体）主语言 ID 均为
// 0x0004，统一视为中文；探测失败按英文处理。
func Detect() Lang {
	if procGetUserDefaultUILanguage.Find() != nil {
		return En
	}
	id, _, _ := procGetUserDefaultUILanguage.Call()
	if uint16(id)&0x03FF == 0x0004 {
		return Zh
	}
	return En
}
