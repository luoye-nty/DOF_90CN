// Package logcrypt 提供"落盘即密文"的日志写入器，采用**公钥信封**方案。
//
// 为什么是公钥而不是对称密钥（2026-09-30 与运营确认）：
//
//   - 服务端要**分发出去**。若把对称密钥放在服务端，拿到服务端包的人就能解开全部日志。
//   - 因此服务端只持有**收件人公钥**（只能加密，无法解密）；**私钥只留在运营本机**，
//     排障时把日志拉回本机用 logtool 解密。
//
// 关键约定：
//
//   - **绝不自动生成收件人密钥对**。密钥对只由运营在本机执行 `logtool keygen` 生成一次；
//     公钥随服务端包分发，私钥永不分发。缺少公钥时各日志通道直接关闭（绝不退化成明文）。
//   - 逐条独立加密（每条记录自带随机 nonce），文件被截断只有最后一条解不开。
//   - 每个文件用一把随机 DEK（数据密钥），DEK 用公钥信封封装后写在文件头里。
package logcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DEKSize 是文件级数据密钥（AES-256）长度。
	DEKSize = 32
	// PublicKeySize 是 X25519 公钥长度。
	PublicKeySize = 32
	// PrivateKeySize 是 X25519 私钥长度。
	PrivateKeySize = 32
	// NonceSize 是 GCM nonce 长度。
	NonceSize = 12
	// keyIDSize 是用作密钥标识的 SHA-256 前缀字节数（hex 后 16 字符）。
	keyIDSize = 8
	// envelopeInfo 是信封密钥派生的域分隔串。
	envelopeInfo = "longheng:log-envelope:v1"

	// PublicKeyFileName / PrivateKeyFileName 是 `logtool keygen` 的默认输出名。
	PublicKeyFileName  = "log-public.key"
	PrivateKeyFileName = "log-private.key"
)

var (
	// ErrKeyInvalid 表示密钥材料长度/编码不合法。
	ErrKeyInvalid = errors.New("logcrypt: key material is invalid")
	// ErrNotEncrypted 表示文件不是 logcrypt 容器。
	ErrNotEncrypted = errors.New("logcrypt: file is not a logcrypt container")
	// ErrRecipientMissing 表示没有配置收件人公钥。
	ErrRecipientMissing = errors.New("logcrypt: recipient public key is required (log output stays off rather than writing plaintext)")
)

// Recipient 是收件人身份：只有公钥。服务端持有它，只能加密。
type Recipient struct {
	keyID string
	pub   *ecdh.PublicKey
}

// KeyID 返回收件人公钥标识（SHA-256 前 8 字节的 hex）。
//
// 它写在每个文件头里：换过密钥对之后，旧文件依然能被对应的旧私钥解开，
// 解密工具按文件头里的 keyID 选私钥，而不是假定"当前私钥一定对"。
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

// RecipientKeyID 由原始公钥计算 keyID。
func RecipientKeyID(publicKey []byte) (string, error) {
	parsed, err := parsePublicKey(publicKey)
	if err != nil {
		return "", err
	}
	_ = parsed
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:keyIDSize]), nil
}

// GenerateKeyPair 生成一对 X25519 密钥（仅供运营本机 keygen 使用，服务端从不调用）。
func GenerateKeyPair() (publicKey []byte, privateKey []byte, err error) {
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("logcrypt: generate key pair: %w", err)
	}
	return append([]byte(nil), private.PublicKey().Bytes()...),
		append([]byte(nil), private.Bytes()...), nil
}

// PublicKeyOf 由私钥推导公钥，用于确认"手上的私钥"和"服务端固化的公钥"是不是同一对。
func PublicKeyOf(privateKey []byte) ([]byte, error) {
	raw, err := ParsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	private, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: private key: %w", err)
	}
	return append([]byte(nil), private.PublicKey().Bytes()...), nil
}

// PrivateKeyKeyID 由私钥算出对应的 keyID（与同对公钥算出的 keyID 相同）。
func PrivateKeyKeyID(privateKey []byte) (string, error) {
	publicKey, err := PublicKeyOf(privateKey)
	if err != nil {
		return "", err
	}
	return RecipientKeyID(publicKey)
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

// LoadRecipientPublicKey 从文件读取收件人公钥。
//
// 文件不存在时返回 ErrRecipientMissing——调用方据此关闭日志通道，**绝不写明文**。
func LoadRecipientPublicKey(path string) (*Recipient, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, ErrRecipientMissing
	}
	raw, err := os.ReadFile(trimmed) //nolint:gosec // G304：路径来自部署配置。
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrRecipientMissing, trimmed)
		}
		return nil, fmt.Errorf("logcrypt: read public key: %w", err)
	}
	recipient, err := ParseRecipientPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: public key file %s: %w", trimmed, err)
	}
	return recipient, nil
}

// ParsePrivateKey 解析私钥：32 字节原始、44 字符 base64 或 64 字符 hex。
func ParsePrivateKey(data []byte) ([]byte, error) {
	raw, err := decodeKeyMaterial(data, PrivateKeySize)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: private key: %w", err)
	}
	if _, err := ecdh.X25519().NewPrivateKey(raw); err != nil {
		return nil, fmt.Errorf("logcrypt: private key: %w", err)
	}
	return raw, nil
}

// LoadPrivateKey 从文件读取私钥（只有运营本机需要）。
func LoadPrivateKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304：路径来自运营本机命令行。
	if err != nil {
		return nil, fmt.Errorf("logcrypt: read private key: %w", err)
	}
	key, err := ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: private key file %s: %w", path, err)
	}
	return key, nil
}

// EncodeKeyFile 把密钥材料编码成落盘文本（base64 + 换行）。密钥对一律以这种形式保存，
// 便于人工备份与核对；服务端只需要公钥那一份。
func EncodeKeyFile(material []byte) []byte {
	return append([]byte(base64.StdEncoding.EncodeToString(material)), '\n')
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

// deriveWrapKey 从 X25519 共享秘密派生信封密钥。
//
// salt 同时包含临时公钥与收件人公钥，info 做域分隔——与 `crypto/box` 的思路一致，
// 使信封与"发给谁"绑定。
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

// unwrapDEK 用私钥解开文件头里的 DEK。
func unwrapDEK(privateKey, ephemeralPublic, nonce, wrapped []byte) ([]byte, error) {
	curve := ecdh.X25519()
	private, err := curve.NewPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: private key: %w", err)
	}
	ephemeral, err := curve.NewPublicKey(ephemeralPublic)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: ephemeral public key: %w", err)
	}
	shared, err := private.ECDH(ephemeral)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: envelope ecdh: %w", err)
	}
	recipientPublic := private.PublicKey().Bytes()
	wrapKey, err := deriveWrapKey(shared, ephemeralPublic, recipientPublic)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(wrapKey)
	if err != nil {
		return nil, err
	}
	dek, err := aead.Open(nil, nonce, wrapped, recipientPublic)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: cannot unwrap data key (wrong private key?): %w", err)
	}
	if len(dek) != DEKSize {
		return nil, fmt.Errorf("logcrypt: unwrapped data key has length %d", len(dek))
	}
	return dek, nil
}

// WriteKeyPair 把密钥对写到目录下（公钥 0644、私钥 0600）。已存在且未 force 时不覆盖。
func WriteKeyPair(dir string, force bool) (publicPath, privatePath, keyID string, err error) {
	if strings.TrimSpace(dir) == "" {
		return "", "", "", errors.New("logcrypt: key output dir is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", "", fmt.Errorf("logcrypt: create key dir: %w", err)
	}
	publicPath = filepath.Join(dir, PublicKeyFileName)
	privatePath = filepath.Join(dir, PrivateKeyFileName)
	if !force {
		for _, path := range []string{publicPath, privatePath} {
			if _, statErr := os.Stat(path); statErr == nil {
				return "", "", "", fmt.Errorf("logcrypt: %s already exists (pass --force to overwrite)", path)
			}
		}
	}
	publicKey, privateKey, err := GenerateKeyPair()
	if err != nil {
		return "", "", "", err
	}
	if err := os.WriteFile(publicPath, EncodeKeyFile(publicKey), 0o644); err != nil {
		return "", "", "", fmt.Errorf("logcrypt: write public key: %w", err)
	}
	if err := os.WriteFile(privatePath, EncodeKeyFile(privateKey), 0o600); err != nil {
		return "", "", "", fmt.Errorf("logcrypt: write private key: %w", err)
	}
	sum := sha256.Sum256(publicKey)
	return publicPath, privatePath, hex.EncodeToString(sum[:keyIDSize]), nil
}
