//go:build !windows

package locale

import (
	"os"
	"strings"
)

// Detect 从环境变量粗略判断系统语言（桌面 Linux/macOS 下 LANG 通常已设置）。
func Detect() Lang {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := strings.ToLower(os.Getenv(key))
		if i := strings.IndexByte(v, '.'); i >= 0 {
			v = v[:i] // 去掉编码后缀，如 zh_CN.UTF-8
		}
		if v == "" {
			continue
		}
		if strings.HasPrefix(v, "zh") {
			return Zh
		}
		return En
	}
	return En
}
