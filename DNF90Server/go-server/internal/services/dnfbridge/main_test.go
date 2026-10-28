package dnfbridge

import (
	"os"
	"testing"

	"longheng.io/server/internal/platform/logcrypt"
)

// TestMain 为整个包统一准备一对"日志收件人"密钥。
//
// 背景（2026-09-30）：包日志与审计记录改为**落盘即密文**，采用公钥信封方案——
// 服务端只持有收件人公钥（可加密、不可解密）。按设计，**没有配置公钥时日志通道会
// 直接关闭，绝不退化成明文**，这正是分发包忘记带公钥时的期望行为。
//
// 于是凡是调用 openPacketLogger 的测试都必须先有公钥。逐个测试准备既啰嗦又容易漏，
// 这里在包级装一对即可；私钥同时通过环境变量暴露，供 packetLogPlaintext 解密断言。
//
// 需要验证"没有公钥时必须拒绝写"的测试用 t.Setenv 覆盖这两个变量即可
// （见 packet_log_crypt_test.go 的 TestPacketLoggerRefusesToWriteWithoutPublicKey）。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dnfbridge-logkeys-")
	if err != nil {
		panic("dnfbridge: create test key dir: " + err.Error())
	}
	publicPath, privatePath, _, err := logcrypt.WriteKeyPair(dir, false)
	if err != nil {
		panic("dnfbridge: write test key pair: " + err.Error())
	}
	if err := os.Setenv("DNFBRIDGE_LOG_PUBLIC_KEY_FILE", publicPath); err != nil {
		panic("dnfbridge: set public key env: " + err.Error())
	}
	if err := os.Setenv("DNFBRIDGE_LOG_PRIVATE_KEY_FILE", privatePath); err != nil {
		panic("dnfbridge: set private key env: " + err.Error())
	}

	code := m.Run()

	_ = os.RemoveAll(dir)
	os.Exit(code)
}
