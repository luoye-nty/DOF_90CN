package dnfbridge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"longheng.io/server/internal/platform/logcrypt"
)

// packetLogTestKeyEnv 为一条测试生成独立的收件人密钥对，并通过环境变量交给
// openPacketLogger：公钥用于加密，私钥只留在测试里用于解密。
//
// 这样测试既不会往仓库里写密钥，也不会互相干扰；同时顺带钉住
// "没有配置公钥时日志通道必须关闭而不是写明文"这条契约。
func packetLogTestKeyEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	publicPath, privatePath, _, err := logcrypt.WriteKeyPair(dir, false)
	if err != nil {
		t.Fatalf("write test key pair: %v", err)
	}
	t.Setenv("DNFBRIDGE_LOG_PUBLIC_KEY_FILE", publicPath)
	t.Setenv("DNFBRIDGE_LOG_PRIVATE_KEY_FILE", privatePath)
}

// packetLogPlaintext 解开锚点对应的全部密文分片，返回拼接后的明文。
//
// 包日志自 2026-09-30 起落盘即密文（公钥信封），因此断言内容必须先解密——
// 这也正是运维侧 `logtool` 的同一套路径（LoadPrivateKey + DecryptAll），测试顺带把它钉住。
func packetLogPlaintext(t *testing.T, anchorPath string) string {
	t.Helper()
	text, err := packetLogPlaintextSoft(anchorPath)
	if err != nil {
		t.Fatalf("read encrypted packet log for %s: %v", anchorPath, err)
	}
	return text
}

// packetLogPlaintextSoft 与 packetLogPlaintext 相同，但把错误交回调用方，
// 让"日志读不到"只降级成诊断信息、而不是直接判定测试失败。
func packetLogPlaintextSoft(anchorPath string) (string, error) {
	archiveDir := filepath.Join(filepath.Dir(anchorPath), packetLogArchiveDirName)
	prefix := strings.TrimSuffix(filepath.Base(anchorPath), filepath.Ext(anchorPath))
	files, err := logcrypt.OpenFiles(archiveDir, prefix)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no encrypted packet log under %s", archiveDir)
	}
	privateKey, err := logcrypt.LoadPrivateKey(logPrivateKeyPath(anchorPath))
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, file := range files {
		raw, readErr := os.ReadFile(file)
		if readErr != nil {
			return "", readErr
		}
		plain, _, truncated, decErr := logcrypt.DecryptAll(privateKey, raw)
		if decErr != nil {
			return "", fmt.Errorf("%s: %w", file, decErr)
		}
		if truncated {
			return "", fmt.Errorf("%s has a truncated trailing record", file)
		}
		out.Write(plain)
	}
	return out.String(), nil
}

// TestPacketLoggerWritesCiphertextOnly 钉住"磁盘上不出现明文"这条硬约束：
// 内容可解、可读，但原始字节里不含任何一个关键字。
func TestPacketLoggerWritesCiphertextOnly(t *testing.T) {
	packetLogTestKeyEnv(t)
	anchor := filepath.Join(t.TempDir(), "packet_log.txt")
	logger, err := openPacketLogger(anchor)
	if err != nil {
		t.Fatalf("open packet logger: %v", err)
	}
	logger.Log("SEND", "game-secret-kind", []byte{0xDE, 0xAD, 0xBE, 0xEF}, "conn_id", "game-000777")
	logger.Event("event-secret-kind", "marker", "SENTINEL-VALUE")
	if err := logger.Close(); err != nil {
		t.Fatalf("close packet logger: %v", err)
	}

	archiveDir := filepath.Join(filepath.Dir(anchor), packetLogArchiveDirName)
	files, err := logcrypt.OpenFiles(archiveDir, "packet_log")
	if err != nil || len(files) == 0 {
		t.Fatalf("list encrypted packet log: files=%v err=%v", files, err)
	}
	for _, file := range files {
		raw, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Fatalf("read %s: %v", file, readErr)
		}
		if !logcrypt.IsContainer(raw) {
			t.Fatalf("%s is not a logcrypt container", file)
		}
		for _, needle := range []string{
			"game-secret-kind", "event-secret-kind", "SENTINEL-VALUE", "deadbeef", "game-000777",
		} {
			if strings.Contains(string(raw), needle) {
				t.Fatalf("plaintext %q leaked into %s", needle, file)
			}
		}
	}

	text := packetLogPlaintext(t, anchor)
	for _, want := range []string{
		"SEND kind=game-secret-kind",
		"conn_id=game-000777",
		"hex=deadbeef",
		"EVENT kind=event-secret-kind",
		"SENTINEL-VALUE",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("decrypted packet log missing %q:\n%s", want, text)
		}
	}
}

// TestPacketLoggerRefusesToWriteWithoutPublicKey 钉住"没有公钥就不许写"：
// 分发包忘了带公钥时必须关掉日志通道并报错，绝不能退化成明文。
func TestPacketLoggerRefusesToWriteWithoutPublicKey(t *testing.T) {
	anchorDir := t.TempDir()
	t.Setenv("DNFBRIDGE_LOG_PUBLIC_KEY_FILE", filepath.Join(anchorDir, "keys", "absent-public.key"))
	anchor := filepath.Join(anchorDir, "packet_log.txt")
	logger, err := openPacketLogger(anchor)
	if err == nil {
		t.Fatal("openPacketLogger must fail when no recipient public key is configured")
	}
	if logger != nil {
		t.Fatal("logger must be nil when the public key is missing")
	}
	if _, statErr := os.Stat(filepath.Join(anchorDir, packetLogArchiveDirName)); !os.IsNotExist(statErr) {
		t.Fatal("no log directory may be created when there is no public key")
	}
}

// TestPacketLoggerRotatesByDayDirectory 确认归档落在 <锚点父目录>/packet/<YYYYMMDD>/ 下，
// 且锚点父目录本身没被动过（logs/records 的推导依赖它）。
func TestPacketLoggerRotatesByDayDirectory(t *testing.T) {
	packetLogTestKeyEnv(t)
	anchorDir := t.TempDir()
	anchor := filepath.Join(anchorDir, "packet_log.txt")
	logger, err := openPacketLogger(anchor)
	if err != nil {
		t.Fatalf("open packet logger: %v", err)
	}
	logger.Event("rotate-probe")
	if err := logger.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	day := packetLogDayDir(filepath.Join(anchorDir, packetLogArchiveDirName))
	if day == "" {
		t.Fatalf("no day directory under %s", filepath.Join(anchorDir, packetLogArchiveDirName))
	}
	if filepath.Dir(day) != filepath.Join(anchorDir, packetLogArchiveDirName) {
		t.Fatalf("day dir %s is not a direct child of the archive dir", day)
	}
	// records 目录推导：锚点父目录不变，因此仍然是 <anchorDir>/records。
	if got := defaultRecordDirFor(anchor); got != filepath.Join(anchorDir, "records") {
		t.Fatalf("defaultRecordDirFor = %q, records dir must not move", got)
	}
}

func packetLogDayDir(archiveDir string) string {
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() && len(entry.Name()) == 8 {
			return filepath.Join(archiveDir, entry.Name())
		}
	}
	return ""
}
