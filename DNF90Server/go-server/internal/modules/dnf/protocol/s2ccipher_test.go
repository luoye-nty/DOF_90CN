package protocol

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// 固定向量：**这就是跨实现契约**。
// 同一组 (master key, 连接 nonce, msgID, seq, body) 在 90CN.dll 的 s2ccrypt 自检里
// 必须算出完全一样的密文（Go 侧与 C 侧各跑一遍比对，见 s2ccrypt_selftest）。
const (
	testVectorMasterKeyBase64 = "ToXJkeLbeccbEN0a2fK2DjRhct6nx8KTYisGp49Levs="
	testVectorConnNonceHex    = "00112233445566778899aabbccddeeff"
	testVectorMsgID           = uint16(38)
	testVectorSeq             = uint16(4242)
	testVectorBodyHex         = "0b000000ff00cafe0102030405060708090a"
	testVectorCipherHex       = "e32cd1aa9c3ac15f46145c774a701eb6bfe0"
)

func testVectorInputs(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	key, err := ParseS2CMasterKey([]byte(testVectorMasterKeyBase64))
	if err != nil {
		t.Fatalf("parse master key: %v", err)
	}
	nonce, err := hex.DecodeString(testVectorConnNonceHex)
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	body, err := hex.DecodeString(testVectorBodyHex)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return key, nonce, body
}

// 钉死密文：任何"顺手改一下 KDF / 计数器布局"的改动都会在这里失败，
// 而不是等到玩家客户端收到乱码（现象是"卡住"，极难定位）。
func TestS2CCipherMatchesPinnedVector(t *testing.T) {
	key, nonce, body := testVectorInputs(t)
	s2c, err := NewS2CCipher(key, nonce)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	sealed := append([]byte(nil), body...)
	s2c.Apply(sealed, testVectorMsgID, testVectorSeq)
	if got := hex.EncodeToString(sealed); got != testVectorCipherHex {
		t.Fatalf("ciphertext = %s, want %s", got, testVectorCipherHex)
	}
	s2c.Apply(sealed, testVectorMsgID, testVectorSeq)
	if !bytes.Equal(sealed, body) {
		t.Fatalf("apply twice must return the plaintext: %s", hex.EncodeToString(sealed))
	}
}

// 计数器块布局是两端唯一的共享约定，这里逐字节钉住它。
func TestS2CCounterBlockLayout(t *testing.T) {
	key, nonce, _ := testVectorInputs(t)
	s2c, err := NewS2CCipher(key, nonce)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	block := s2c.counterBlock(0x1234, 0xabcd)
	want := append(append([]byte(nil), nonce[:8]...), 0x12, 0x34, 0xab, 0xcd, 0, 0, 0, 0)
	if !bytes.Equal(block[:], want) {
		t.Fatalf("counter block = %x, want %x", block[:], want)
	}
	if !bytes.Equal(s2c.NoncePrefix(), nonce[:8]) {
		t.Fatalf("nonce prefix = %x, want %x", s2c.NoncePrefix(), nonce[:8])
	}
}

// 等长：客户端 ProxyCipherDecodeFunction 的契约是 `*outLength = bodyLength`。
func TestS2CCipherIsLengthPreserving(t *testing.T) {
	key, nonce, _ := testVectorInputs(t)
	s2c, err := NewS2CCipher(key, nonce)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	for _, size := range []int{1, 15, 16, 17, 43, 233, 2048} {
		body := bytes.Repeat([]byte{0xA5}, size)
		original := append([]byte(nil), body...)
		s2c.Apply(body, 38, 7)
		if len(body) != size {
			t.Fatalf("size %d became %d", size, len(body))
		}
		if bytes.Equal(body, original) {
			t.Fatalf("size %d was not transformed", size)
		}
	}
}

// 每连接一把密钥：不同连接 nonce 必须给出不同密文（否则跨连接会共用密钥流）。
func TestS2CCipherIsBoundToConnectionNonce(t *testing.T) {
	key, nonce, body := testVectorInputs(t)
	first, err := NewS2CCipher(key, nonce)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	otherNonce := append([]byte(nil), nonce...)
	otherNonce[0] ^= 0x01
	second, err := NewS2CCipher(key, otherNonce)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	a := append([]byte(nil), body...)
	b := append([]byte(nil), body...)
	first.Apply(a, 38, 1)
	second.Apply(b, 38, 1)
	if bytes.Equal(a, b) {
		t.Fatal("two connections produced the same keystream")
	}
}

// 同一连接内不同 (msgID, seq) 必须走不同密钥流；相同输入必须可复现（无跨包状态）。
func TestS2CCipherIsStatelessAcrossPackets(t *testing.T) {
	key, nonce, body := testVectorInputs(t)
	s2c, err := NewS2CCipher(key, nonce)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	first := append([]byte(nil), body...)
	s2c.Apply(first, 38, 100)
	second := append([]byte(nil), body...)
	s2c.Apply(second, 39, 100) // 只改 msgID
	if bytes.Equal(first, second) {
		t.Fatal("msgID did not affect the keystream")
	}
	third := append([]byte(nil), body...)
	s2c.Apply(third, 38, 101) // 只改 seq
	if bytes.Equal(first, third) {
		t.Fatal("seq did not affect the keystream")
	}
	// 无状态：中间插入别的包不影响同一 (msgID, seq) 的结果（丢包/重发安全）。
	s2c.Apply(append([]byte(nil), body...), 40, 102)
	replay := append([]byte(nil), body...)
	s2c.Apply(replay, 38, 100)
	if !bytes.Equal(first, replay) {
		t.Fatal("cipher must be stateless: the same (msgID, seq) must reproduce the same keystream")
	}
}

func TestS2CMasterKeyParsing(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(testVectorMasterKeyBase64)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if parsed, err := ParseS2CMasterKey(raw); err != nil || !bytes.Equal(parsed, raw) {
		t.Fatalf("raw: %v %x", err, parsed)
	}
	if parsed, err := ParseS2CMasterKey([]byte(testVectorMasterKeyBase64)); err != nil || !bytes.Equal(parsed, raw) {
		t.Fatalf("base64: %v", err)
	}
	if parsed, err := ParseS2CMasterKey([]byte(hex.EncodeToString(raw))); err != nil || !bytes.Equal(parsed, raw) {
		t.Fatalf("hex: %v", err)
	}
	if _, err := ParseS2CMasterKey([]byte("short")); err == nil {
		t.Fatal("a short key must be rejected")
	}
}

func TestNewS2CCipherRejectsBadInputs(t *testing.T) {
	key, nonce, _ := testVectorInputs(t)
	if _, err := NewS2CCipher(key, nonce[:8]); err == nil {
		t.Fatal("a truncated connection nonce must be rejected")
	}
	if _, err := NewS2CCipher([]byte("nope"), nonce); err == nil {
		t.Fatal("an invalid master key must be rejected")
	}
}

// 固化主密钥必须能解析（改坏常量会在测试阶段就炸，而不是线上全服乱码）。
func TestBakedMasterKeyIsUsable(t *testing.T) {
	key := DefaultS2CMasterKey()
	if len(key) != S2CMasterKeySize {
		t.Fatalf("baked master key length = %d", len(key))
	}
	if _, err := NewS2CCipher(key, make([]byte, 16)); err != nil {
		t.Fatalf("baked master key must derive a cipher: %v", err)
	}
}

// 计数器块必须真的被 AES-CTR 用作初值（否则"每包独立"就是空话）。
func TestS2CCipherUsesCounterBlockAsInitialCounter(t *testing.T) {
	key, nonce, body := testVectorInputs(t)
	s2c, err := NewS2CCipher(key, nonce)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	derived, err := hkdf.Key(sha256.New, key, nonce, s2cKeyInfo, S2CCipherKeySize)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	counter := s2c.counterBlock(38, 4242)
	stream := cipher.NewCTR(block, counter[:])
	want := append([]byte(nil), body...)
	stream.XORKeyStream(want, want)
	got := append([]byte(nil), body...)
	s2c.Apply(got, 38, 4242)
	if !bytes.Equal(got, want) {
		t.Fatalf("Apply did not use counterBlock as the CTR initial counter")
	}
}
