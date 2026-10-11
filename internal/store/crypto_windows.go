//go:build windows

package store

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// cryptProtectUIForbidden 等同 Win32 的 CRYPTPROTECT_UI_FORBIDDEN，
// 禁止弹出任何 UI（本程序在后台自动加解密，不应打扰用户）。
const cryptProtectUIForbidden = 0x1

// platformProtect 用 Windows DPAPI（用户+机器绑定）保护明文。
// 这样即使 DEK blob 被单独拿走，换台机器/换用户也解不开。
func platformProtect(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, windows.ERROR_INVALID_PARAMETER
	}
	in := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, cryptProtectUIForbidden, &out); err != nil {
		return nil, err
	}
	// 必须先拷贝再 LocalFree：直接返回的切片会引用已被释放的缓冲区。
	defer func() { windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(out.Data)))) }()
	return dataBlobBytes(out), nil
}

// platformUnprotect 解开 platformProtect 的产物。
func platformUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, windows.ERROR_INVALID_PARAMETER
	}
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, cryptProtectUIForbidden, &out); err != nil {
		return nil, err
	}
	// 必须先拷贝再 LocalFree：直接返回的切片会引用已被释放的缓冲区。
	defer func() { windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(out.Data)))) }()
	return dataBlobBytes(out), nil
}

func dataBlobBytes(b windows.DataBlob) []byte {
	if b.Data == nil || b.Size == 0 {
		return nil
	}
	// 拷贝一份：b.Data 由 DPAPI 分配、调用方随后 LocalFree，
	// 返回别名会在释放后继续使用（间歇性「数据无效」的根源）。
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(b.Data)), int(b.Size))...)
}
