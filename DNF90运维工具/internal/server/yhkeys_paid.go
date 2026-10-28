//go:build paid

package server

// 【已去授权】本文件原先内置了作者的验证服务器回调配置（回调地址 / X-Yh-Callback-Key /
// 证书 pin，三串 XOR 混淆密文），启动时自动装配，使赞助版无需外部配置即可回连验证服务器。
//
// 去授权后这些内置密文**已全部移除**：
//   - 源码里不再残留任何验证服务器地址、回调密钥或证书指纹（混淆密文也算，它能被解出来）；
//   - yhBuiltinSecrets 返回空串 → NewYhCheckAuto 返回 nil → 不装配回调；
//   - 即使装配了也不会生效：server.go 的 SetYhCheck 已是空实现、yhCheckDeny 恒放行。
//
// 若将来要恢复在线授权：重新生成密文并同步 authserver 侧的 YH_CALLBACK_KEY，
// 同时把 server.go 的 SetYhCheck / yhCheckDeny 改回核销实现。

// yhBuiltinSecrets 返回内置的回调配置（地址、密钥、证书 pin）。
// 去授权后恒返回空串，调用方据此判定"未配置"，从而不装配回调。
func yhBuiltinSecrets() (url, key, pin string) {
	return "", "", ""
}
