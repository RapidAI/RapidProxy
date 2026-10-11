//go:build !windows

package store

// 非 Windows 平台没有 OS 级密钥保管，这里退化为「密钥文件」（权限 0o600）。
// 这只能挡住「只拿走了凭据文件、却没同时拿走密钥文件」的情况；同源恶意软件
// 仍能同时读到两者。生产部署请仅在 Windows 上使用本代理（走 DPAPI 路径）。
func platformProtect(plain []byte) ([]byte, error) { return plain, nil }

func platformUnprotect(blob []byte) ([]byte, error) { return blob, nil }
