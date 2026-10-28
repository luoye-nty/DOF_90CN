package logkey

import (
	"os"
	"path/filepath"
	"testing"

	"longheng.io/server/internal/platform/logcrypt"
)

// TestBakedInPublicKeyIsUsable 是这套固化方案的安全网。
//
// 公钥是**写死在二进制里**的：一旦这个常量被改坏（截断、少一个字符、粘贴成私钥），
// 服务端不会写明文，而是把全部日志通道静默关掉——那会是一次很难发现的运维事故。
// 所以这里在构建阶段就把"常量可用"钉死。
func TestBakedInPublicKeyIsUsable(t *testing.T) {
	recipient, err := Recipient()
	if err != nil {
		t.Fatalf("固化公钥不可用：%v（日志通道会被关闭）", err)
	}
	if recipient.KeyID() != "751edccc24cdd076" {
		t.Fatalf("keyID = %q，与常量注释里记录的 751edccc24cdd076 不一致；换钥匙时请同步更新注释",
			recipient.KeyID())
	}
	if len(recipient.PublicKeyBytes()) != logcrypt.PublicKeySize {
		t.Fatalf("public key size = %d", len(recipient.PublicKeyBytes()))
	}
}

// TestBakedInKeyRoundTripsThroughTheRealWriter 用真实写入器验证"固化公钥能加密"，
// 并用一对临时密钥证明它解不开（即：拿到二进制的人读不出日志）。
func TestBakedInKeyRoundTripsThroughTheRealWriter(t *testing.T) {
	recipient, err := Recipient()
	if err != nil {
		t.Fatalf("resolve baked-in key: %v", err)
	}
	dir := t.TempDir()
	writer, err := logcrypt.Open(logcrypt.Config{
		Dir: dir, Prefix: "probe", Ext: ".log.enc", Recipient: recipient,
	})
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	writer.Write("EVENT kind=baked-in-probe")
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw, err := os.ReadFile(writer.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if keyID, err := logcrypt.ContainerKeyID(raw); err != nil || keyID != recipient.KeyID() {
		t.Fatalf("container key id = %q err=%v", keyID, err)
	}
	// 分发包里只有公钥：用任意别的私钥都必须解不开。
	_, otherPrivate, err := logcrypt.GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate unrelated pair: %v", err)
	}
	if _, _, _, err := logcrypt.DecryptAll(otherPrivate, raw); err == nil {
		t.Fatal("用无关私钥竟然解开了：固化公钥方案失效")
	}
}

func TestResolveRecipientPrefersExplicitFile(t *testing.T) {
	dir := t.TempDir()
	publicPath, _, keyID, err := logcrypt.WriteKeyPair(dir, false)
	if err != nil {
		t.Fatalf("write key pair: %v", err)
	}
	t.Setenv("DNFBRIDGE_LOG_PUBLIC_KEY_FILE", publicPath)
	recipient, err := ResolveRecipient()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if recipient.KeyID() != keyID {
		t.Fatalf("keyID = %q want %q（显式指定的公钥应当覆盖固化公钥）", recipient.KeyID(), keyID)
	}
}

// 显式设置了路径却读不到时必须报错：静默回退会让"换钥匙写错路径"毫无察觉。
func TestResolveRecipientFailsOnExplicitMissingFile(t *testing.T) {
	t.Setenv("DNFBRIDGE_LOG_PUBLIC_KEY_FILE", filepath.Join(t.TempDir(), "absent.key"))
	if _, err := ResolveRecipient(); err == nil {
		t.Fatal("显式指定了不存在的公钥文件时必须报错，不能静默回退到固化公钥")
	}
}
