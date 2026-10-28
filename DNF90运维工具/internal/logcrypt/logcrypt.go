// Package logcrypt —— **只写不读**的加密日志落盘实现（运维工具侧副本）。
//
// 为什么这里会有一份副本：
//
//	运维工具是**独立模块**（go.mod: dnf90ops），不能 import 服务端的
//	internal/platform/logcrypt；而线上的服务端 stdout/stderr 恰恰是**运维工具**
//	落盘的（见 internal/ops/ops.go），所以这条路必须是运维工具自己加密。
//
// 硬约束（改动前必读）：
//
//   - 文件格式必须与服务端 `go-server/internal/platform/logcrypt` 的 **v2 公钥信封**
//     **逐字节兼容**（magic "LHLOG2"、version 2、头部布局、记录布局、AAD=整个文件头）。
//     任何一方改了格式，另一方都要同步，并用 `logtool verify` 实测通过。
//   - 这里刻意**没有任何解密代码**：运维工具只持有收件人公钥，拿不到也解不开日志。
//     （因此本包也不提供私钥解析/DEK 解封，别"顺手"补上。）
//   - **绝不自动生成密钥**；没有公钥时调用方必须关掉该通道，绝不退化成明文。
package logcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// 与服务端一致的格式常量。
const (
	// DEKSize 是文件级数据密钥（AES-256）长度。
	DEKSize = 32
	// PublicKeySize 是 X25519 公钥长度。
	PublicKeySize = 32
	// NonceSize 是 GCM nonce 长度。
	NonceSize = 12
	// keyIDSize 是用作密钥标识的 SHA-256 前缀字节数（hex 后 16 字符）。
	keyIDSize = 8
	// envelopeInfo 是信封密钥派生的域分隔串（必须与服务端一致）。
	envelopeInfo = "longheng:log-envelope:v1"

	// fileMagic / fileVersion 是容器标识（必须与服务端一致）。
	fileMagic   = "LHLOG2"
	fileVersion = 2
	// recordOverhead 是每条记录在明文之外多占的字节：长度前缀 + nonce + GCM tag。
	recordOverhead = 4 + NonceSize + 16
	// headerFixedSize 是文件头里除 keyID 与 wrappedDEK 之外的固定部分。
	headerFixedSize = len(fileMagic) + 1 + 1 + 1 + PublicKeySize + NonceSize + 2
)

var (
	// ErrKeyInvalid 表示密钥材料长度/编码不合法。
	ErrKeyInvalid = errors.New("logcrypt: key material is invalid")
	// ErrRecipientMissing 表示没有配置收件人公钥。
	ErrRecipientMissing = errors.New("logcrypt: recipient public key is required (log output stays off rather than writing plaintext)")
)

// Recipient 是收件人身份：只有公钥，只能加密。
type Recipient struct {
	keyID string
	pub   *ecdh.PublicKey
}

// KeyID 返回收件人公钥标识（SHA-256 前 8 字节的 hex），写在每个文件头里。
func (r *Recipient) KeyID() string {
	if r == nil {
		return ""
	}
	return r.keyID
}

// PublicKeyBytes 返回原始 32 字节公钥。
func (r *Recipient) PublicKeyBytes() []byte {
	if r == nil {
		return nil
	}
	return append([]byte(nil), r.pub.Bytes()...)
}

// ParseRecipientPublicKey 解析公钥：32 字节原始、44 字符 base64 或 64 字符 hex。
func ParseRecipientPublicKey(data []byte) (*Recipient, error) {
	raw, err := decodeKeyMaterial(data, PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: public key: %w", err)
	}
	pub, err := parsePublicKey(raw)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &Recipient{keyID: hex.EncodeToString(sum[:keyIDSize]), pub: pub}, nil
}

// RecipientFromBase64 由 base64 文本构造收件人（供固化公钥使用）。
func RecipientFromBase64(text string) (*Recipient, error) {
	return ParseRecipientPublicKey([]byte(strings.TrimSpace(text)))
}

func decodeKeyMaterial(data []byte, want int) ([]byte, error) {
	if len(data) == want {
		return append([]byte(nil), data...), nil
	}
	text := strings.TrimSpace(string(data))
	if decoded, err := hex.DecodeString(text); err == nil && len(decoded) == want {
		return decoded, nil
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(text); err == nil && len(decoded) == want {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("%w: want %d bytes, input is %d bytes", ErrKeyInvalid, want, len(data))
}

func parsePublicKey(raw []byte) (*ecdh.PublicKey, error) {
	if len(raw) != PublicKeySize {
		return nil, fmt.Errorf("%w: public key must be %d bytes, got %d", ErrKeyInvalid, PublicKeySize, len(raw))
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: public key: %w", err)
	}
	return pub, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: new cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// deriveWrapKey 从 X25519 共享秘密派生信封密钥（salt 含双方公钥，info 做域分隔）。
func deriveWrapKey(shared, ephemeralPublic, recipientPublic []byte) ([]byte, error) {
	salt := make([]byte, 0, len(ephemeralPublic)+len(recipientPublic))
	salt = append(salt, ephemeralPublic...)
	salt = append(salt, recipientPublic...)
	key, err := hkdf.Key(sha256.New, shared, salt, envelopeInfo, DEKSize)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: derive envelope key: %w", err)
	}
	return key, nil
}

// wrapDEK 生成一把随机 DEK 并用收件人公钥封装。
func (r *Recipient) wrapDEK(random io.Reader) (dek, ephemeralPublic, nonce, wrapped []byte, err error) {
	if r == nil || r.pub == nil {
		return nil, nil, nil, nil, ErrRecipientMissing
	}
	if random == nil {
		return nil, nil, nil, nil, errors.New("logcrypt: random source is nil")
	}
	dek = make([]byte, DEKSize)
	if _, err = io.ReadFull(random, dek); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("logcrypt: data key: %w", err)
	}
	ephemeral, err := ecdh.X25519().GenerateKey(random)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("logcrypt: ephemeral key: %w", err)
	}
	shared, err := ephemeral.ECDH(r.pub)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("logcrypt: envelope ecdh: %w", err)
	}
	ephemeralPublic = append([]byte(nil), ephemeral.PublicKey().Bytes()...)
	wrapKey, err := deriveWrapKey(shared, ephemeralPublic, r.pub.Bytes())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	aead, err := newAEAD(wrapKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	nonce = make([]byte, NonceSize)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("logcrypt: wrap nonce: %w", err)
	}
	wrapped = aead.Seal(nil, nonce, dek, r.pub.Bytes())
	return dek, ephemeralPublic, nonce, wrapped, nil
}

// buildHeader 组装文件头（布局必须与服务端一致）。
func buildHeader(recipient *Recipient, ephemeralPublic, wrapNonce, wrappedDEK []byte) []byte {
	out := make([]byte, 0, headerFixedSize+len(recipient.keyID)+len(wrappedDEK))
	out = append(out, fileMagic...)
	out = append(out, fileVersion)
	out = append(out, byte(len(recipient.keyID)))
	out = append(out, recipient.keyID...)
	out = append(out, '\n')
	out = append(out, ephemeralPublic...)
	out = append(out, wrapNonce...)
	var length [2]byte
	binary.LittleEndian.PutUint16(length[:], uint16(len(wrappedDEK)))
	out = append(out, length[:]...)
	out = append(out, wrappedDEK...)
	return out
}

// appendRecord 把一条明文记录密封后追加到 dst（记录用整个文件头做 AAD）。
func appendRecord(dst []byte, aead cipher.AEAD, aad []byte, plain []byte, random io.Reader) ([]byte, error) {
	if int64(len(plain)) > int64(^uint32(0)) {
		return nil, fmt.Errorf("logcrypt: record too large: %d bytes", len(plain))
	}
	if random == nil {
		return nil, errors.New("logcrypt: random source is nil")
	}
	var nonce [NonceSize]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return nil, fmt.Errorf("logcrypt: nonce: %w", err)
	}
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(plain)))
	dst = append(dst, size[:]...)
	dst = append(dst, nonce[:]...)
	return aead.Seal(dst, nonce[:], plain, aad), nil
}

func defaultRandom() io.Reader { return cryptorand.Reader }
