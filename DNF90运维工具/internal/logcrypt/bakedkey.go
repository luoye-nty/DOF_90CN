package logcrypt

import (
	"fmt"
	"os"
	"strings"
)

// BakedPublicKeyBase64 是**固化在程序里的收件人公钥**（base64 的 32 字节 X25519 公钥）。
//
// 与服务端 `go-server/internal/platform/logkey/logkey.go` 的 PublicKeyBase64 **必须一致**：
// 两边的日志都要能被运营本机那一把私钥解开。对应 keyID = **751edccc24cdd076**
// （logcrypt_test.go 里钉了这两个值，改错会在 `go test` 阶段就炸）。
//
// 公钥是非机密材料（只能加密、不能解密），写在二进制里没有风险；
// 能解开日志的私钥只存在运营本机，永不随包分发。
const BakedPublicKeyBase64 = "EwIqXTU/VZsozviqge5jq8wlQiTmD6zCVXmSbxWGYm4="

// PublicKeyEnvVar 是显式覆盖公钥的环境变量（仅轮换/联调时用）。
const PublicKeyEnvVar = "DNFBRIDGE_LOG_PUBLIC_KEY_FILE"

// BakedRecipient 返回固化公钥对应的收件人。
func BakedRecipient() (*Recipient, error) {
	recipient, err := RecipientFromBase64(BakedPublicKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: baked-in public key is unusable: %w", err)
	}
	return recipient, nil
}

// ResolveRecipient 依次尝试环境变量指定的公钥文件、固化公钥。
//
// 刻意**不静默回落**：显式设了 DNFBRIDGE_LOG_PUBLIC_KEY_FILE 却读不到时直接报错，
// 否则轮换时路径写错会毫无察觉地继续用旧公钥加密。
func ResolveRecipient() (*Recipient, error) {
	if explicit := strings.TrimSpace(os.Getenv(PublicKeyEnvVar)); explicit != "" {
		raw, err := os.ReadFile(explicit)
		if err != nil {
			return nil, fmt.Errorf("logcrypt: read public key %s: %w", explicit, err)
		}
		recipient, parseErr := ParseRecipientPublicKey(raw)
		if parseErr != nil {
			return nil, fmt.Errorf("logcrypt: %s: %w", explicit, parseErr)
		}
		return recipient, nil
	}
	return BakedRecipient()
}
