package qoder

import "fmt"

// uuidFormat 把 16 个原始字节渲染成标准带连字符形态。
func uuidFormat(raw []byte) string {
	if len(raw) != 16 {
		return "rapidproxy-device"
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}
