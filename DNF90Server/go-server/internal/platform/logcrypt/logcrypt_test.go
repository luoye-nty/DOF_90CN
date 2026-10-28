package logcrypt

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testIdentity 生成一对测试密钥，并返回收件人与私钥。
func testIdentity(t *testing.T) (*Recipient, []byte) {
	t.Helper()
	publicKey, privateKey, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate key pair: %v", err)
	}
	recipient, err := ParseRecipientPublicKey(publicKey)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	return recipient, privateKey
}

func TestKeyPairRoundTripAndEncoding(t *testing.T) {
	recipient, privateKey := testIdentity(t)
	if recipient.KeyID() == "" || len(recipient.KeyID()) != 16 {
		t.Fatalf("key id = %q", recipient.KeyID())
	}
	// 公钥接受 base64 / hex / 原始字节
	publicBytes := recipient.PublicKeyBytes()
	for name, payload := range map[string][]byte{
		"raw":    publicBytes,
		"base64": EncodeKeyFile(publicBytes),
	} {
		parsed, err := ParseRecipientPublicKey(payload)
		if err != nil {
			t.Fatalf("%s: parse public key: %v", name, err)
		}
		if parsed.KeyID() != recipient.KeyID() {
			t.Fatalf("%s: key id mismatch", name)
		}
	}
	if _, err := ParseRecipientPublicKey([]byte("too-short")); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("short public key error = %v, want ErrKeyInvalid", err)
	}
	if _, err := ParsePrivateKey(EncodeKeyFile(privateKey)); err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	if _, err := ParsePrivateKey([]byte("nope")); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("short private key error = %v, want ErrKeyInvalid", err)
	}
}

// 契约：**没有收件人公钥就不许写**——绝不退化成明文。
func TestOpenWithoutRecipientIsRefused(t *testing.T) {
	if _, err := Open(Config{Dir: t.TempDir(), Prefix: "packet", Ext: ".log.enc"}); !errors.Is(err, ErrRecipientMissing) {
		t.Fatalf("open without recipient = %v, want ErrRecipientMissing", err)
	}
	if _, err := NewEncoder(nil, &bytes.Buffer{}, nil); !errors.Is(err, ErrRecipientMissing) {
		t.Fatalf("encoder without recipient = %v, want ErrRecipientMissing", err)
	}
	if _, err := NewStreamWriter(nil, &bytes.Buffer{}, nil); !errors.Is(err, ErrRecipientMissing) {
		t.Fatalf("stream writer without recipient = %v, want ErrRecipientMissing", err)
	}
}

func TestWriteKeyPairRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	publicPath, privatePath, keyID, err := WriteKeyPair(dir, false)
	if err != nil {
		t.Fatalf("write key pair: %v", err)
	}
	if keyID == "" {
		t.Fatal("empty key id")
	}
	if _, _, _, err := WriteKeyPair(dir, false); err == nil {
		t.Fatal("second WriteKeyPair without force must refuse")
	}
	if _, _, _, err := WriteKeyPair(dir, true); err != nil {
		t.Fatalf("forced rewrite: %v", err)
	}
	for _, path := range []string{publicPath, privatePath} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat %s: %v", path, statErr)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", path)
		}
	}
}

// 核心契约：磁盘上不得出现明文，且用私钥解密后与写入内容逐字节一致。
func TestWriterNeverWritesPlaintextAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	recipient, privateKey := testIdentity(t)
	writer, err := Open(Config{Dir: dir, Prefix: "packet", Ext: ".log.enc", Recipient: recipient})
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	secret := "SEND kind=game-upper conn_id=game-000001 hex=DEADBEEFCAFE"
	writer.Write("EVENT kind=starts " + secret)
	writer.Write("EVENT kind=second index=1")
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	path := writer.Path()
	if path == "" {
		path = CurrentPath(dir)
	}
	if path == "" {
		t.Fatal("no current file path recorded")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read container: %v", err)
	}
	for _, needle := range []string{"DEADBEEF", "conn_id", "game-upper", "EVENT kind"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Fatalf("plaintext %q found on disk in %s", needle, path)
		}
	}
	if !IsContainer(raw) {
		t.Fatal("written file is not a logcrypt container")
	}
	if keyID, err := ContainerKeyID(raw); err != nil || keyID != recipient.KeyID() {
		t.Fatalf("container key id = %q err=%v want %q", keyID, err, recipient.KeyID())
	}

	plain, records, truncated, err := DecryptAll(privateKey, raw)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if truncated || records != 2 {
		t.Fatalf("records=%d truncated=%t", records, truncated)
	}
	text := string(plain)
	if !strings.Contains(text, secret) || !strings.Contains(text, "EVENT kind=second index=1") {
		t.Fatalf("round-trip mismatch:\n%s", text)
	}
}

// 只有公钥的人（= 拿到服务端包的人）解不开。
func TestPublicKeyAloneCannotDecrypt(t *testing.T) {
	dir := t.TempDir()
	recipient, _ := testIdentity(t)
	writer, err := Open(Config{Dir: dir, Prefix: "packet", Ext: ".log.enc", Recipient: recipient})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	writer.Write("EVENT kind=secret")
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw, err := os.ReadFile(writer.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 用另一对密钥的私钥必须失败。
	_, otherPrivate := testIdentity(t)
	if _, _, _, err := DecryptAll(otherPrivate, raw); err == nil {
		t.Fatal("decrypt with an unrelated private key succeeded")
	}
	// 公钥本身不是私钥，也必须失败。
	if _, _, _, err := DecryptAll(recipient.PublicKeyBytes(), raw); err == nil {
		t.Fatal("decrypt using the public key as private key succeeded")
	}
}

func TestWriterRotatesBySizeAndKeepsEveryFileReadable(t *testing.T) {
	dir := t.TempDir()
	recipient, privateKey := testIdentity(t)
	writer, err := Open(Config{
		Dir: dir, Prefix: "packet", Ext: ".log.enc", Recipient: recipient,
		MaxBytes: 4096,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const lines = 300
	for index := 0; index < lines; index++ {
		writer.Write(strings.Repeat("x", 64) + " index=" + itoa(index))
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	files, err := OpenFiles(dir, "packet")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) < 2 {
		t.Fatalf("expected rotation into multiple files, got %d", len(files))
	}
	total := 0
	for _, file := range files {
		raw, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Fatalf("read %s: %v", file, readErr)
		}
		if int64(len(raw)) > 4096+8192 {
			t.Fatalf("%s exceeded the cap badly: %d bytes", file, len(raw))
		}
		plain, records, truncated, decErr := DecryptAll(privateKey, raw)
		if decErr != nil || truncated {
			t.Fatalf("decrypt %s: records=%d truncated=%t err=%v", file, records, truncated, decErr)
		}
		if records > 0 && !strings.Contains(string(plain), "index=") {
			t.Fatalf("%s decrypted without the expected record body", file)
		}
		total += records
	}
	if total != lines {
		t.Fatalf("records across files = %d, want %d", total, lines)
	}
}

func TestWriterRotatesOnDayBoundaryAndPrunesOldDirs(t *testing.T) {
	dir := t.TempDir()
	recipient, privateKey := testIdentity(t)
	day := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	now := day
	writer, err := Open(Config{
		Dir: dir, Prefix: "packet", Ext: ".log.enc", Recipient: recipient,
		RetentionDays: 7,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	writer.Write("EVENT kind=day-one")
	waitForDir(t, filepath.Join(dir, "20260301"), true)

	now = day.AddDate(0, 0, 1) // 20260302，保留窗口从 20260223 起
	writer.Write("EVENT kind=day-two")
	waitForDir(t, filepath.Join(dir, "20260302"), true)
	if _, statErr := os.Stat(filepath.Join(dir, "20260301")); statErr != nil {
		t.Fatalf("20260301 must survive while inside the retention window: %v", statErr)
	}

	now = day.AddDate(0, 0, 9) // 20260310，截止 20260303
	writer.Write("EVENT kind=day-nine")
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	waitForDir(t, filepath.Join(dir, "20260310"), true)
	files, err := OpenFiles(dir, "packet")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, file := range files {
		if !strings.Contains(file, "20260310") {
			continue
		}
		found = true
		raw, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Fatalf("read %s: %v", file, readErr)
		}
		plain, _, truncated, decErr := DecryptAll(privateKey, raw)
		if decErr != nil || truncated || !strings.Contains(string(plain), "EVENT kind=day-nine") {
			t.Fatalf("decrypt %s: truncated=%t err=%v", file, truncated, decErr)
		}
	}
	if !found {
		t.Fatal("no container written for the current day")
	}
	waitForDir(t, filepath.Join(dir, "20260301"), false)
	waitForDir(t, filepath.Join(dir, "20260302"), false)
}

func waitForDir(t *testing.T, path string, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := os.Stat(path)
		exists := err == nil
		if exists == want {
			return
		}
		if time.Now().After(deadline) {
			if want {
				t.Fatalf("directory %s never appeared: %v", path, err)
			}
			t.Fatalf("directory %s was not pruned", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDecryptAllToleratesTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	recipient, privateKey := testIdentity(t)
	writer, err := Open(Config{Dir: dir, Prefix: "packet", Ext: ".log.enc", Recipient: recipient})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	writer.Write("EVENT kind=first")
	writer.Write("EVENT kind=second")
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw, err := os.ReadFile(writer.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	cut := raw[:len(raw)-9] // 砍掉最后一条的尾巴，模拟掉电半行
	plain, records, truncated, err := DecryptAll(privateKey, cut)
	if err != nil {
		t.Fatalf("decrypt truncated: %v", err)
	}
	if !truncated || records != 1 || !strings.Contains(string(plain), "EVENT kind=first") {
		t.Fatalf("records=%d truncated=%t plain=%q", records, truncated, string(plain))
	}
}

func TestCurrentPointerFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	recipient, _ := testIdentity(t)
	writer, err := Open(Config{Dir: dir, Prefix: "packet", Ext: ".log.enc", Recipient: recipient, MaxBytes: 2048})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for index := 0; index < 100; index++ {
		writer.Write(strings.Repeat("y", 96) + " i=" + itoa(index))
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	pointer := CurrentPath(dir)
	if pointer == "" {
		t.Fatal("current.txt missing")
	}
	if _, err := os.Stat(pointer); err != nil {
		t.Fatalf("pointer target missing: %v", err)
	}
	if filepath.Base(pointer) != filepath.Base(writer.Path()) {
		t.Fatalf("pointer target %q != last written %q", pointer, writer.Path())
	}
}

// gatedReader 在 release 关闭前阻塞，用来确定性地制造写队列积压。
type gatedReader struct {
	release chan struct{}
}

func (g *gatedReader) Read(p []byte) (int, error) {
	<-g.release
	return defaultRandom().Read(p)
}

func TestDropMarkerIsEncryptedToo(t *testing.T) {
	dir := t.TempDir()
	recipient, privateKey := testIdentity(t)
	gate := &gatedReader{release: make(chan struct{})}
	writer, err := Open(Config{
		Dir: dir, Prefix: "packet", Ext: ".log.enc", Recipient: recipient,
		QueueSize:      1,
		DropMarkerKind: "packet-log-backpressure-dropped",
		Random:         gate,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for index := 0; index < 200; index++ {
		writer.Write("EVENT kind=flood index=" + itoa(index))
	}
	if writer.Dropped() == 0 {
		t.Fatal("expected the bounded queue to drop under a stalled writer")
	}
	close(gate.release)
	writer.Write("EVENT kind=after-release")
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw, err := os.ReadFile(writer.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.Contains(raw, []byte("backpressure-dropped")) {
		t.Fatal("drop marker leaked to disk in plaintext")
	}
	plain, _, _, err := DecryptAll(privateKey, raw)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !strings.Contains(string(plain), "EVENT kind=packet-log-backpressure-dropped") {
		t.Fatal("drop marker missing from the decrypted stream")
	}
}

// StreamWriter 用于 os/exec 的 stdout/stderr：行边界可能被切开，必须按行重组。
func TestStreamWriterSplitsPartialLines(t *testing.T) {
	dir := t.TempDir()
	recipient, privateKey := testIdentity(t)
	path := filepath.Join(dir, "stream.log.enc")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stream, err := NewStreamWriter(recipient, file, nil)
	if err != nil {
		t.Fatalf("new stream writer: %v", err)
	}
	// 故意在一行中间切开
	if _, err := stream.Write([]byte("hello wo")); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := stream.Write([]byte("rld\nsecond line\nthird")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
	plain, records, _, err := ReadFilePlaintext(privateKey, path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	// 三条记录：完整的两行 + Close 时落下的残行
	if records != 3 {
		t.Fatalf("records = %d, want 3 (plain=%q)", records, string(plain))
	}
	text := string(plain)
	// 末尾残行原样保留（不加换行）：解开后的拼接必须与子进程写出的字节流一致。
	if text != "hello world\nsecond line\nthird" {
		t.Fatalf("stream text = %q", text)
	}
	for _, want := range []string{"hello world\n", "second line\n", "third"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
}

func TestOptionalPrivateKeyMissingIsNotAnError(t *testing.T) {
	key, err := OptionalPrivateKey(filepath.Join(t.TempDir(), "absent.key"))
	if err != nil || key != nil {
		t.Fatalf("missing optional key = (%v, %v), want (nil, nil)", key, err)
	}
	key, err = OptionalPrivateKey("")
	if err != nil || key != nil {
		t.Fatalf("empty optional key = (%v, %v), want (nil, nil)", key, err)
	}
}

func TestReadFilePlaintextRejectsNonContainer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(path, []byte("not encrypted\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, privateKey := testIdentity(t)
	if _, _, _, err := ReadFilePlaintext(privateKey, path); !errors.Is(err, ErrNotEncrypted) {
		t.Fatalf("err = %v, want ErrNotEncrypted", err)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for value > 0 {
		pos--
		buf[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[pos:])
}
