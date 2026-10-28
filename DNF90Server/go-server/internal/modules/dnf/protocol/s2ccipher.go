// S2C 回包二次加密：**等长流密码**（只加密 body，长度、头、checksum 布局全不变）。
//
// 设计见 `服务端回包二次加密_客户端DLL解密_设计文档_20260930.md`。要点：
//
//   - 加密边界是**协议消息**，不是 TCP 写边界：合批（upperWriteBatch）与 Nagle 行为不受影响；
//   - **等长**：客户端 `ProxyCipherDecodeFunction` 的契约是 `*outLength = bodyLength`，
//     带 tag 的 AEAD 会破坏它，所以这里不用 GCM；
//   - **无跨包状态**：每包从 `(连接 nonce, msgID, seq)` 重新起流，丢包/重发/跳号都不会失步
//     （有状态密钥流一旦错位就是"全部乱码 + 客户端卡住"，是必须避免的故障形状）；
//   - 密钥按连接派生：HKDF-SHA256(ikm = 固化主密钥, salt = LANA 握手里那 16 字节随机 nonce)，
//     因此**不需要任何额外往返**，也天然做到"每连接一把"。
//
// 计数器块（16 字节，AES-CTR 的初始计数块）：
//
//	[0:8]   连接 nonce 前 8 字节（每连接随机，来自 LANA challenge）
//	[8:10]  msgID（大端）—— 明文可读，用于降低"同 seq 复用密钥流"的概率
//	[10:12] seq（大端）—— 明文可读
//	[12:16] 0，包内块计数器从这里自增
//
// ⚠️ 已知边界：同一个连接里，`(msgID, seq)` 会在 65536 个包之后重复，
// 那时两包会用到同一段密钥流（CTR 复用）。这是为了"绝不失步"付出的代价：
// 加包序号纪元（epoch）能消除它，但服务端在写失败时也会 `upperSeq++`，
// 客户端观测不到那次自增 → 纪元错位 → **全部乱码**，比理论上的密钥流复用危险得多。
// 若要消除，必须先有一个两端都能可靠对齐的通道（协议字段或可靠的连接内信令）。
package protocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	// S2CMasterKeySize 是固化主密钥长度（HKDF 的 ikm）。
	S2CMasterKeySize = 32
	// S2CCipherKeySize 是每连接派生出的 AES 密钥长度。
	S2CCipherKeySize = 16
	// S2CCipherCodecName 是回包 body 的 codec 标签，写进 game-upper-send-meta 便于排障。
	S2CCipherCodecName = "s2c-ctr-v1"
	// s2cKeyInfo 是 HKDF 的域分隔串。
	s2cKeyInfo = "dnf:s2c:ctr:v1"

	// DefaultS2CMasterKeyBase64 是**固化主密钥**（32 字节 base64）。
	//
	// 同一份常量必须同时存在于服务端与 90CN.dll 的 s2ccrypt.h 里——两边不一致 =
	// 所有回包变成乱码。轮换步骤见设计文档：换这一行 + 改 DLL 常量 + 重新打包，两侧同时上线。
	// 服务端也认 DNFBRIDGE_GAME_S2C_KEY_BASE64 覆盖（仅供本地联调/灰度）。
	DefaultS2CMasterKeyBase64 = "ToXJkeLbeccbEN0a2fK2DjRhct6nx8KTYisGp49Levs="
)

var (
	// ErrS2CMasterKeyInvalid 表示主密钥长度或编码不合法。
	ErrS2CMasterKeyInvalid = errors.New("dnf protocol: s2c master key is invalid")
	// ErrS2CNonceInvalid 表示连接 nonce 长度不合法。
	ErrS2CNonceInvalid = errors.New("dnf protocol: s2c connection nonce is invalid")
)

// S2CCipher 是一条游戏连接上的 S2C body 加解密器（加密与解密是同一个变换）。
//
// 它是**无状态**的：Apply 只依赖入参与连接级的 block nonce，可以被并发调用。
type S2CCipher struct {
	block cipher.Block
	// nonce 是连接 nonce 的前 8 字节，来自 LANA challenge 的随机数。
	nonce [8]byte
}

// ParseS2CMasterKey 解析 32 字节主密钥（base64 / hex / 原始字节）。
func ParseS2CMasterKey(data []byte) ([]byte, error) {
	if len(data) == S2CMasterKeySize {
		return append([]byte(nil), data...), nil
	}
	text := strings.TrimSpace(string(data))
	if decoded, err := hex.DecodeString(text); err == nil && len(decoded) == S2CMasterKeySize {
		return decoded, nil
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(text); err == nil && len(decoded) == S2CMasterKeySize {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("%w: want %d bytes, got %d input bytes", ErrS2CMasterKeyInvalid, S2CMasterKeySize, len(data))
}

// DefaultS2CMasterKey 返回固化主密钥。
func DefaultS2CMasterKey() []byte {
	key, err := ParseS2CMasterKey([]byte(DefaultS2CMasterKeyBase64))
	if err != nil {
		// 常量被改坏时这是构建期就能发现的错误；运行期只可能出现在有人手改常量之后。
		panic("dnf protocol: baked S2C master key is invalid: " + err.Error())
	}
	return key
}

// NewS2CCipher 用主密钥与连接 nonce 派生一条连接的 S2C 加解密器。
//
// connectionNonce 必须**每连接不同**（服务端用 LANA challenge 里那 16 字节随机数），
// 否则不同连接会共用密钥流。
func NewS2CCipher(masterKey, connectionNonce []byte) (*S2CCipher, error) {
	parsed, err := ParseS2CMasterKey(masterKey)
	if err != nil {
		return nil, err
	}
	if len(connectionNonce) != 16 {
		return nil, fmt.Errorf("%w: want 16 bytes, got %d", ErrS2CNonceInvalid, len(connectionNonce))
	}
	salt := append([]byte(nil), connectionNonce...)
	// HKDF-Extract/Expand；salt 取连接 nonce，info 做域分隔。
	derived, err := hkdf.Key(sha256.New, parsed, salt, s2cKeyInfo, S2CCipherKeySize)
	if err != nil {
		return nil, fmt.Errorf("dnf protocol: derive s2c key: %w", err)
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, fmt.Errorf("dnf protocol: new s2c cipher: %w", err)
	}
	cipher := &S2CCipher{block: block}
	copy(cipher.nonce[:], connectionNonce[:8])
	return cipher, nil
}

// Apply 就地变换 body（加密与解密用同一函数）。长度不变。
//
// msgID 与 seq 必须取**线上 13 字节头里**的值：客户端在两个端点都读得到它们，
// 因此不需要任何额外字段来对齐密钥流。
func (c *S2CCipher) Apply(body []byte, msgID uint16, seq uint16) {
	if c == nil || c.block == nil || len(body) == 0 {
		return
	}
	block := c.counterBlock(msgID, seq)
	stream := cipher.NewCTR(c.block, block[:])
	stream.XORKeyStream(body, body)
}

// counterBlock 组装该包的 AES-CTR 初始计数块。
func (c *S2CCipher) counterBlock(msgID uint16, seq uint16) [16]byte {
	var block [16]byte
	copy(block[0:8], c.nonce[:])
	block[8] = byte(msgID >> 8)
	block[9] = byte(msgID)
	block[10] = byte(seq >> 8)
	block[11] = byte(seq)
	return block
}

// NoncePrefix 返回连接 nonce 前 8 字节（诊断用；客户端用同一段拼计数器块）。
func (c *S2CCipher) NoncePrefix() []byte {
	if c == nil {
		return nil
	}
	return append([]byte(nil), c.nonce[:]...)
}
