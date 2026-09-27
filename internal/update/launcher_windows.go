//go:build windows

package update

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/znsoftm/RapidProxy/internal/locale"
)

// shellExecuteInfoW 对应 Win32 的 SHELLEXECUTEINFOW 结构（x64 布局，
// dwHotKey 后是与 HICON/hMonitor 的 8 字节 union）。
type shellExecuteInfoW struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	hIcon        uintptr
	hProcess     uintptr
}

const (
	// seeMaskNoAsync：调用方（本程序）启动安装器后很快退出，
	// 不允许把 DDE 会话挂在本进程上。
	seeMaskNoAsync = 0x00001000
	swShowNormal   = 1
)

var (
	modShell32        = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteW = modShell32.NewProc("ShellExecuteExW")
)

// launchWindows 通过 ShellExecuteEx 的 runas 动词启动安装包：弹出 UAC
// 授权框后由系统启动提权的安装进程。NSIS 安装包需要管理员权限，直接
// exec 会被 Windows 拒绝（ERROR_ELEVATION_REQUIRED，即界面上的
// "The requested operation requires elevation"）。
func launchWindows(path string) error {
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	info := shellExecuteInfoW{
		cbSize: uint32(unsafe.Sizeof(shellExecuteInfoW{})),
		fMask:  seeMaskNoAsync,
		lpVerb: verb,
		lpFile: file,
		nShow:  swShowNormal,
	}
	ret, _, errNo := procShellExecuteW.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		if errNo == windows.ERROR_CANCELLED {
			return fmt.Errorf("%s", locale.T("launcher.cancelled"))
		}
		return fmt.Errorf("%s", locale.T("launcher.failed", errNo))
	}
	return nil
}
