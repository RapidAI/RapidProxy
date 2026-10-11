// Package hx 是新协议族（lobsterai / trae / qoder）适配包共用的 HTTP 小工具。
package hx

import "net/http"

// NoFollow 返回一个不跟随重定向的客户端副本。
//
// 令牌交换、刷新与轮询必须用不跟随重定向的客户端：307 不能把
// refreshToken 或设备码重放到其它主机上。传输层与超时沿用原客户端，
// 因此代理配置保持一致。
func NoFollow(hc *http.Client) *http.Client {
	if hc == nil {
		hc = &http.Client{Transport: http.DefaultTransport}
	}
	clone := *hc
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}
