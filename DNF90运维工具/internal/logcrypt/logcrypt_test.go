package logcrypt

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 这两个值必须与服务端 `go-server/internal/platform/logkey` 和
// `go-server/cmd/server/logtool` 完全一致：任何一方轮换密钥而另一方没跟上，
// 这条测试就该失败——否则线上会出现"日志拉回来了却解不开"。
const (
	wantBakedKeyID     = "751edccc24cdd076"
	wantBakedPublicKey = "EwIqXTU/VZsozviqge5jq8wlQiTmD6zCVXmSbxWGYm4="
)

func TestBakedKeyMatchesServerSideConstant(t *testing.T) {
	if BakedPublicKeyBase64 != wantBakedPublicKey {
		t.Fatalf("BakedPublicKeyBase64 = %q, want %q", BakedPublicKeyBase64, wantBakedPublicKey)
	}
	recipient, err := BakedRecipient()
	if err != nil {
		t.Fatalf("BakedRecipient: %v", err)
	}
	if got := recipient.KeyID(); got != wantBakedKeyID {
		t.Fatalf("keyID = %s, want %s", got, wantBakedKeyID)
	}
}

// 固定公钥 + 固定随机源 → 文件头应当是确定的；这里把 v2 布局钉死，
// 防止本副本与服务端的格式悄悄漂移（漂移的代价是日志全部作废）。
func TestHeaderLayoutIsStableV2(t *testing.T) {
	recipient, err := BakedRecipient()
	if err != nil {
		t.Fatalf("BakedRecipient: %v", err)
	}
	random := bytes.NewReader(bytes.Repeat([]byte{0x5A}, 4096))
	dek, ephemeral, nonce, wrapped, err := recipient.wrapDEK(random)
	if err != nil {
		t.Fatalf("wrapDEK: %v", err)
	}
	if len(dek) != DEKSize || len(ephemeral) != PublicKeySize || len(nonce) != NonceSize {
		t.Fatalf("sizes: dek=%d ephemeral=%d nonce=%d", len(dek), len(ephemeral), len(nonce))
	}
	if len(wrapped) != DEKSize+16 {
		t.Fatalf("wrapped length = %d, want %d", len(wrapped), DEKSize+16)
	}
	header := buildHeader(recipient, ephemeral, nonce, wrapped)
	if !bytes.HasPrefix(header, []byte("LHLOG2\x02")) {
		t.Fatalf("magic/version drifted: % x", header[:8])
	}
	if header[7] != byte(len(wantBakedKeyID)) {
		t.Fatalf("keyID length byte = %d, want %d", header[7], len(wantBakedKeyID))
	}
	if got := string(header[8 : 8+len(wantBakedKeyID)]); got != wantBakedKeyID {
		t.Fatalf("header keyID = %q, want %q", got, wantBakedKeyID)
	}
	if header[8+len(wantBakedKeyID)] != '\n' {
		t.Fatal("keyID separator is not a newline")
	}
	if wantLen := headerFixedSize + len(wantBakedKeyID) + len(wrapped); len(header) != wantLen {
		t.Fatalf("header length = %d, want %d", len(header), wantLen)
	}
}

// 最硬的一条契约：写到磁盘上的字节里不许出现明文。
func TestWriterWritesCiphertextOnly(t *testing.T) {
	dir := t.TempDir()
	recipient, err := BakedRecipient()
	if err != nil {
		t.Fatalf("BakedRecipient: %v", err)
	}
	writer, err := Open(Config{
		Dir:            dir,
		Prefix:         "server-stdout",
		Ext:            ".log.enc",
		Recipient:      recipient,
		Random:         rand.Reader,
		FlushInterval:  5 * time.Millisecond,
		DropMarkerKind: "test-dropped",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const secret = "SECRET-MARKER-运维工具-明文绝不许落盘"
	for i := 0; i < 50; i++ {
		writer.Write(secret)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if path := writer.Path(); path == "" {
		t.Fatal("Path() is empty after Close; callers rely on it to locate the file")
	}
	files := collectEnc(t, dir)
	if len(files) == 0 {
		t.Fatal("no container was written")
	}
	for _, file := range files {
		raw, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Fatalf("read %s: %v", file, readErr)
		}
		if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("SECRET-MARKER")) {
			t.Fatalf("%s contains plaintext", file)
		}
		if !bytes.HasPrefix(raw, []byte("LHLOG2")) {
			t.Fatalf("%s is not a container: % x", file, raw[:8])
		}
	}
}

// 100 MB 上限的轮转：上限设小一点，验证"超了就另起一个文件"。
func TestWriterRotatesBySize(t *testing.T) {
	dir := t.TempDir()
	recipient, err := BakedRecipient()
	if err != nil {
		t.Fatalf("BakedRecipient: %v", err)
	}
	writer, err := Open(Config{
		Dir:           dir,
		Prefix:        "server-stderr",
		Ext:           ".log.enc",
		Recipient:     recipient,
		MaxBytes:      2048,
		Random:        rand.Reader,
		FlushInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 200; i++ {
		writer.Write(strings.Repeat("x", 256))
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if files := collectEnc(t, dir); len(files) < 2 {
		t.Fatalf("expected rotation into multiple shards, got %d", len(files))
	}
	if pointer, err := os.ReadFile(filepath.Join(dir, currentPointerName)); err != nil {
		t.Fatalf("current.txt missing: %v", err)
	} else if strings.TrimSpace(string(pointer)) == "" {
		t.Fatal("current.txt is empty")
	}
}

// 没有公钥就不许写：宁可这个通道关掉，也绝不退化成明文。
func TestOpenWithoutRecipientIsRefused(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(Config{Dir: dir, Prefix: "server-stdout", Ext: ".log.enc"}); err == nil {
		t.Fatal("Open without a recipient must fail")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("refused Open must not create anything, found %d entries", len(entries))
	}
}

// LineWriter 是给 cmd.Stdout 用的 io.Writer：必须原样按行重组成记录，
// 且不完整的尾巴不能丢（进程退出时 Flush 补上）。
func TestLineWriterReassemblesLines(t *testing.T) {
	dir := t.TempDir()
	recipient, err := BakedRecipient()
	if err != nil {
		t.Fatalf("BakedRecipient: %v", err)
	}
	writer, err := Open(Config{
		Dir:           dir,
		Prefix:        "server-stdout",
		Ext:           ".log.enc",
		Recipient:     recipient,
		Random:        rand.Reader,
		FlushInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	lineWriter := NewLineWriter(writer)
	chunks := []string{"2026-09-30 boot", "ing\n2026-09-30 rea", "dy\nlast-without-newline"}
	for _, chunk := range chunks {
		if _, err := lineWriter.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
	}
	lineWriter.Flush()
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 记录条数 = 3 行；密文里数不出明文，所以按格式算：每条记录 = 4+12+明文+16，
	// 明文是"行 + \n"（Writer.Write 会补行尾换行）。
	raw, err := os.ReadFile(writer.Path())
	if err != nil {
		t.Fatalf("read container: %v", err)
	}
	plain := "2026-09-30 booting\n2026-09-30 ready\nlast-without-newline"
	lines := strings.Split(plain, "\n")
	wantLen := headerFixedSize + len(wantBakedKeyID) + (DEKSize + 16)
	for _, line := range lines {
		wantLen += len(line) + 1 + recordOverhead
	}
	if len(raw) != wantLen {
		t.Fatalf("container length = %d, want %d (lines=%d)", len(raw), wantLen, len(lines))
	}
}

// TestAcceptanceArtifact 只在显式设置 DNF90OPS_ACCEPT_DIR 时运行：把一个真实分片
// 留在该目录，交给服务端模块的 logtool 做**跨模块验收**。
//
// 两个模块各有一份 logcrypt（运维工具不能 import 服务端 internal），格式一旦漂移，
// 表现就是"日志加密写了，但运营拉回本机解不开"——所以这条验收必须能随时重跑：
//
//	$env:DNF90OPS_ACCEPT_DIR='D:\accept'; go test ./internal/logcrypt/ -run AcceptanceArtifact
//	logtool verify -dir D:\accept -key <本机私钥>
//	logtool grep   "OPS-ACCEPT-MARKER" -dir D:\accept -key <本机私钥>
func TestAcceptanceArtifact(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("DNF90OPS_ACCEPT_DIR"))
	if dir == "" {
		t.Skip("DNF90OPS_ACCEPT_DIR not set")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	recipient, err := ResolveRecipient()
	if err != nil {
		t.Fatalf("ResolveRecipient: %v", err)
	}
	writer, err := Open(Config{
		Dir:           dir,
		Prefix:        "server-stdout",
		Ext:           ".log.enc",
		Recipient:     recipient,
		Random:        rand.Reader,
		FlushInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	writer.Write("2026-09-30T12:00:00Z OPS-ACCEPT-MARKER ops tool encrypted stdout channel")
	writer.Write("OPS-ACCEPT-MARKER 第二行中文也要能原样解出来")
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Logf("wrote %s (keyID=%s)", writer.Path(), writer.KeyID())
}

func collectEnc(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	walkErr := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".enc") {
			files = append(files, path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", dir, walkErr)
	}
	return files
}
