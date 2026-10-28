// Package logkey 保存**固化在二进制里**的日志加密收件人公钥。
//
// 为什么固化而不是放文件：服务端要分发出去，用服务端的人应该"拿到就能跑"——
// 不该要求他理解、放置或配置任何密钥文件，也不该让他有机会把日志搞坏。
//
// 固化是安全的：收件人**公钥**是非机密材料，只能加密、不能解密。能解开日志的私钥
// 只存在于运营本机（`logtool keygen -o <dir>` 产生的 log-private.key），**永不进分发包**。
// 换句话说：拿到服务端二进制的人可以往日志里写，但一个字都读不出来。
//
// 更换密钥对：
//  1. 运营本机执行 `logtool keygen -o <dir>`；
//  2. 把 log-public.key 的内容（base64）填到下面的 PublicKeyBase64；
//  3. 重新构建服务端。旧分片仍能被旧私钥解开——每个文件头里都记着自己的 keyID。
package logkey

import (
	"os"
	"strings"

	"longheng.io/server/internal/platform/logcrypt"
)

// PublicKeyBase64 是固化进二进制的收件人公钥（X25519，base64 编码，44 字符）。
//
// 当前这把的 keyID 是 751edccc24cdd076 —— `logtool list` / `logtool verify` 会显示
// 每个分片的 keyID，用它确认手上的日志确实出自这把钥匙。
//
// 这个常量被改坏（长度不对/base64 非法）时，服务端不会静默写明文：所有日志通道会
// 直接关闭并留下告警，`logkey` 包自带测试也会在构建阶段就失败。
const PublicKeyBase64 = "EwIqXTU/VZsozviqge5jq8wlQiTmD6zCVXmSbxWGYm4="

// Recipient 解析固化公钥。常量非法时返回错误——调用方据此关闭日志通道，绝不写明文。
func Recipient() (*logcrypt.Recipient, error) {
	return logcrypt.ParseRecipientPublicKey([]byte(PublicKeyBase64))
}

// ResolveRecipient 返回本次运行实际生效的收件人公钥：
//
//  1. 环境变量 DNFBRIDGE_LOG_PUBLIC_KEY_FILE 指向的文件（显式覆盖，供轮换与测试使用）；
//  2. 固化在二进制里的公钥。
//
// 显式设置了路径却读不到时**直接报错、不静默回退**：否则轮换时把路径写错，
// 日志会悄悄用回旧钥匙，而你以为已经换了。
//
// 环境变量名刻意与 coretest 侧一致，启动器会把运维环境里设置的值透传进来。
func ResolveRecipient() (*logcrypt.Recipient, error) {
	if explicit := strings.TrimSpace(os.Getenv("DNFBRIDGE_LOG_PUBLIC_KEY_FILE")); explicit != "" {
		return logcrypt.LoadRecipientPublicKey(explicit)
	}
	return Recipient()
}
