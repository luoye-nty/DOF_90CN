package logcrypt

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 文件格式（v2：公钥信封）
//
//	header := "LHLOG2" | version(1) | keyIDLen(1) | keyID(keyIDLen) | "\n"
//	          | ephemeralPub(32) | wrapNonce(12) | wrappedLen(u16 LE) | wrappedDEK(wrappedLen)
//	record := plainLen(u32 LE) | nonce(12) | AES-256-GCM_DEK(plaintext) + tag(16)
//
// 设计要点：
//
//   - 每个文件一把随机 DEK（数据密钥），逐条记录用它做 AES-256-GCM；DEK 用**收件人公钥**
//     封装后写在文件头里。服务端因此只需要公钥，**拿到服务端包也解不开任何日志**。
//   - 逐条独立密封：文件被截断时只有最后一条未写完的记录解不开，前面的全部完好；
//     也不需要"整文件才能读"。
//   - keyID 写在文件头里：换过密钥对之后，旧文件依然能被对应的旧私钥解开。
//   - 记录用整个文件头做 AAD，使记录与"它属于哪个文件、哪把 DEK"绑定，
//     避免记录被跨文件搬运拼接。
const (
	fileMagic   = "LHLOG2"
	fileVersion = 2
	// recordOverhead 是每条记录在明文之外多占的字节：长度前缀 + nonce + GCM tag。
	recordOverhead = 4 + NonceSize + 16
	// headerFixedSize 是文件头里除 keyID 与 wrappedDEK 之外的固定部分。
	headerFixedSize = len(fileMagic) + 1 + 1 + 1 + PublicKeySize + NonceSize + 2
)

// ErrRecordTruncated 表示遇到未写完/损坏的记录；之前的记录仍然有效。
var ErrRecordTruncated = errors.New("logcrypt: trailing record is truncated or corrupted")

// headerLayout 是解析出来的文件头。
type headerLayout struct {
	// dataOffset 是记录区起始偏移。
	dataOffset int
	// keyID 是收件人公钥标识。
	keyID string
	// ephemeralPublic / wrapNonce / wrappedDEK 用于解开文件级 DEK。
	ephemeralPublic []byte
	wrapNonce       []byte
	wrappedDEK      []byte
}

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

func parseHeader(data []byte) (headerLayout, error) {
	var layout headerLayout
	base := len(fileMagic)
	if len(data) < headerFixedSize || string(data[:base]) != fileMagic {
		return layout, ErrNotEncrypted
	}
	if data[base] != fileVersion {
		return layout, fmt.Errorf("logcrypt: unsupported container version %d", data[base])
	}
	idLen := int(data[base+1])
	offset := base + 2
	if idLen <= 0 || idLen > 64 || len(data) < offset+idLen+1 {
		return layout, ErrNotEncrypted
	}
	layout.keyID = string(data[offset : offset+idLen])
	offset += idLen
	if data[offset] != '\n' {
		return layout, ErrNotEncrypted
	}
	offset++
	if len(data) < offset+PublicKeySize+NonceSize+2 {
		return layout, ErrNotEncrypted
	}
	layout.ephemeralPublic = append([]byte(nil), data[offset:offset+PublicKeySize]...)
	offset += PublicKeySize
	layout.wrapNonce = append([]byte(nil), data[offset:offset+NonceSize]...)
	offset += NonceSize
	wrappedLen := int(binary.LittleEndian.Uint16(data[offset : offset+2]))
	offset += 2
	if wrappedLen <= 0 || len(data) < offset+wrappedLen {
		return layout, ErrNotEncrypted
	}
	layout.wrappedDEK = append([]byte(nil), data[offset:offset+wrappedLen]...)
	offset += wrappedLen
	layout.dataOffset = offset
	return layout, nil
}

// appendRecord 把一条明文记录密封后追加到 dst。
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

// openDEK 用私钥解开文件头里的 DEK，并返回以该 DEK 构造的 AEAD 与记录区起点。
func openDEK(privateKey, data []byte) (cipher.AEAD, headerLayout, error) {
	layout, err := parseHeader(data)
	if err != nil {
		return nil, layout, err
	}
	dek, err := unwrapDEK(privateKey, layout.ephemeralPublic, layout.wrapNonce, layout.wrappedDEK)
	if err != nil {
		return nil, layout, err
	}
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, layout, err
	}
	return aead, layout, nil
}

// DecryptAll 用私钥解开一整份容器内容。
//
// 返回值：
//   - plaintext：已成功解出的明文（多份记录按写入顺序拼接）
//   - records：成功解出的记录条数
//   - truncated：为 true 表示末尾有一段未写完/损坏的记录被丢弃（前面的仍然有效）
//
// 只有"文件头不合法"或"首条记录就解不开"这类情况才返回 error——那种情况下
// plaintext 为空，说明私钥不对或者文件不是本格式。
func DecryptAll(privateKey, data []byte) (plaintext []byte, records int, truncated bool, err error) {
	if len(privateKey) != PrivateKeySize {
		return nil, 0, false, fmt.Errorf("%w: private key must be %d bytes", ErrKeyInvalid, PrivateKeySize)
	}
	aead, layout, err := openDEK(privateKey, data)
	if err != nil {
		return nil, 0, false, err
	}
	offset := layout.dataOffset
	aad := data[:offset]
	out := make([]byte, 0, len(data))
	for offset < len(data) {
		if len(data)-offset < recordOverhead {
			truncated = true
			break
		}
		plainLen := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
		if plainLen < 0 {
			truncated = true
			break
		}
		recordLen := recordOverhead + plainLen
		if len(data)-offset < recordLen {
			truncated = true
			break
		}
		nonce := data[offset+4 : offset+4+NonceSize]
		sealed := data[offset+4+NonceSize : offset+recordLen]
		plain, openErr := aead.Open(nil, nonce, sealed, aad)
		if openErr != nil {
			if records == 0 {
				return nil, 0, false, fmt.Errorf("logcrypt: cannot decrypt: %w", openErr)
			}
			truncated = true
			break
		}
		out = append(out, plain...)
		records++
		offset += recordLen
	}
	return out, records, truncated, nil
}

// ContainerKeyID 只读文件头，返回该文件使用的收件人公钥标识。用于挑选私钥。
func ContainerKeyID(data []byte) (string, error) {
	layout, err := parseHeader(data)
	if err != nil {
		return "", err
	}
	return layout.keyID, nil
}

// IsContainer 判断一份字节是否是 logcrypt 容器。回读侧用它区分
// "已加密的记录文件"与"历史遗留的明文文件"。
func IsContainer(data []byte) bool {
	_, err := parseHeader(data)
	return err == nil
}
