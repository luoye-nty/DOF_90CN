//go:build !paid

package server

// 免费版不内置 yh 回调密钥、不装配回调（保持与改造前一致）。
// 免费版无外网版登录器接入，无需 yh 核销；避免把共用回调 key 泄露给非付费副本。
func yhBuiltinSecrets() (url, key, pin string) {
	return "", "", ""
}
