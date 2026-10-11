// Qoder 设备登录的稳定每安装 machine 身份。官方 CLI 把批准绑定到它自己的
// machine code；本程序在数据目录里保存一个本地生成的 UUID。
package qoder

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	machineMu       sync.Mutex
	machineCachedID string
)

// MachineID 返回稳定的每安装设备 id，首次使用时创建。
// 默认落在 ~/.rapidproxy/qoder-machine-id，可用环境变量
// QODER_MACHINE_ID_FILE 覆盖。
func MachineID() string {
	machineMu.Lock()
	defer machineMu.Unlock()
	if machineCachedID != "" {
		return machineCachedID
	}
	path := machineIDPath()
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if id := sanitizeMachineID(string(data)); id != "" {
				machineCachedID = id
				return id
			}
		}
	}
	id := newUUID()
	machineCachedID = id
	if path == "" {
		return id
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		_ = os.WriteFile(path, []byte(id), 0o600)
	}
	return id
}

func sanitizeMachineID(raw string) string {
	id := strings.TrimSpace(raw)
	if len(id) > 128 {
		return ""
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F', r == '-':
		default:
			return ""
		}
	}
	return id
}

func machineIDPath() string {
	if path := strings.TrimSpace(os.Getenv("QODER_MACHINE_ID_FILE")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".", ".rapidproxy", "qoder-machine-id")
	}
	return filepath.Join(home, ".rapidproxy", "qoder-machine-id")
}

func newUUID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "rapidproxy-device"
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return uuidFormat(raw)
}
