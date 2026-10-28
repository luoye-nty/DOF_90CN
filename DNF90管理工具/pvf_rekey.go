package main

// PVF 密钥轮换重打包（Go 移植版，逐字节对齐 pvf_protect.py 的 rekey）。
// 输入：标准密钥（或上一轮 96hex key_set）加密的 Script.pvf；
// 输出：随机（或指定）新密钥重打包的 Script.pvf，文件尺寸与区段布局保持不变。
// 仅供 PVF 加密页（pvf_tab.go）调用；纯标准库实现。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

const (
	lcgMain  = 2531011
	lcgStr   = 2531017
	pvfMagic = 0x69706B6E // "nkpi" little-endian
	xorA     = 0xAA74472E
	xorB     = 0x9A82F037
)

// 标准密钥 16B 宽 blob（源自 pvf_codec.py / dnf.exe key blobs 逆向）。
var stdWideBlobs = map[string]string{
	"head": "0054512a9c4c349a35f07e4fcd9b0000",
	"hash": "0054512a9c4c109a07f05e4ffff90000",
	"grpi": "0054512a934c039a03f02e4fbd2b0000",
	"body": "0054512a964c3e9a2af05d4f565a0000",
	"stra": "0054512aa74c059a0df0284fb3e50000",
	"strb": "0054512aa74c059a0df03e4fb3e50000",
}

// keyOrder 96hex 分段顺序（与三端 EInfo 契约、服务端 NormalizeKeySetHex 对齐）。
var keyOrder = []string{"head", "hash", "grpi", "body", "stra", "strb"}

// wideToPlain 将 16B 宽加密 blob 解为 8B 明文 key（逆向自 dnf.exe 的宽 blob 编解码）。
func wideToPlain(blob []byte) ([]byte, error) {
	if len(blob) < 4 {
		return nil, errors.New("wide blob too short")
	}
	v7 := uint32(blob[1] & 0xFE)
	encLen := binary.LittleEndian.Uint16(blob[2:4])
	nchars := uint16((encLen ^ uint16(v7|(v7<<7))) & 0xFFFF)
	dl := int(nchars) * 2
	if 4+dl > len(blob) {
		return nil, errors.New("wide blob len mismatch")
	}
	key := (v7 | 0x9A714CA0) & 0xFFFFFFFF
	out := make([]byte, dl)
	nd := dl / 4
	for i := 0; i < nd; i++ {
		c := binary.LittleEndian.Uint32(blob[4+i*4:])
		p := key ^ c
		binary.LittleEndian.PutUint32(out[i*4:], p)
		key = (p + 65599*key) & 0xFFFFFFFF
	}
	for j := 0; j < dl&3; j++ {
		c := uint32(blob[4+nd*4+j])
		p := (key & 0xFF) ^ c
		out[nd*4+j] = byte(p)
		key = (p + 263*key) & 0xFFFFFFFF
	}
	return out, nil
}

// standardOldKeys 返回 6 组标准密钥（8B/组，供 lcgCrypt 种子）。
// 注：宽 blob 解密明文可能带 wide 串尾（>8B），但种子只取前 8B，截断一致。
func standardOldKeys() (map[string][]byte, error) {
	m := map[string][]byte{}
	for k, h := range stdWideBlobs {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("decode std wide blob %s: %w", k, err)
		}
		plain, err := wideToPlain(raw)
		if err != nil {
			return nil, fmt.Errorf("decode std key %s: %w", k, err)
		}
		if len(plain) < 8 {
			return nil, fmt.Errorf("std key %s too short %d", k, len(plain))
		}
		m[k] = plain[:8]
	}
	return m, nil
}

// parseKeySet 将 96 hex key_set 按 head/hash/grpi/body/stra/strb 拆成 6 组 8B。
func parseKeySet(hexSet string) (map[string][]byte, error) {
	if len(hexSet) != 96 {
		return nil, errors.New("key_set must be 96 hex chars")
	}
	raw, err := hex.DecodeString(hexSet)
	if err != nil || len(raw) != 48 {
		return nil, errors.New("key_set must be 96 hex chars")
	}
	m := map[string][]byte{}
	for i, name := range keyOrder {
		m[name] = raw[i*8 : i*8+8]
	}
	return m, nil
}

func seedFromKey(key []byte) uint32 {
	if len(key) < 8 {
		return 0
	}
	w0 := uint64(binary.LittleEndian.Uint16(key[0:]))
	w1 := uint64(binary.LittleEndian.Uint16(key[2:]))
	w2 := uint64(binary.LittleEndian.Uint16(key[4:]))
	w3 := uint64(binary.LittleEndian.Uint16(key[6:]))
	return uint32((866031377*w0 + 915*(w3+915*(w2+915*w1))) & 0xFFFFFFFF)
}

// lcgCrypt LCG XOR 流加解密（对称），与 pvf_protect.py 逐字节一致。
func lcgCrypt(data, key []byte, inc uint32) []byte {
	seed := seedFromKey(key)
	out := make([]byte, len(data))
	copy(out, data)
	n := len(data)
	i := 0
	for ; i+4 <= n; i += 4 {
		v6 := uint32((214013*uint64(seed) + uint64(inc)) & 0xFFFFFFFF)
		seed = uint32((214013*uint64(v6) + uint64(inc)) & 0xFFFFFFFF)
		xork := ((uint64(seed) >> 16) + uint64(v6&0xFFFF0000)) & 0xFFFFFFFF
		d := binary.LittleEndian.Uint32(out[i:]) ^ uint32(xork)
		binary.LittleEndian.PutUint32(out[i:], d)
	}
	rem := n - i
	if rem > 0 {
		v6 := uint32((214013*uint64(seed) + uint64(inc)) & 0xFFFFFFFF)
		v7 := uint32((uint64(v6&0xFFFF0000) + ((214013*uint64(v6)+uint64(inc))&0xFFFFFFFF)>>16) & 0xFFFFFFFF)
		var kb [4]byte
		binary.LittleEndian.PutUint32(kb[:], v7)
		for j := 0; j < rem; j++ {
			out[i+j] ^= kb[j]
		}
	}
	return out
}

// pvfLayout 用给定密钥解析 PVF 区段布局。
type pvfLayout struct {
	data      []byte
	keys      map[string][]byte
	f         [12]uint32
	nFiles    uint32
	nBlocks   uint32
	st1Size   uint32
	st2Size   uint32
	treeOff   int
	st1Off    int
	st2Off    int
	pairOff   int
	bodyOff   int
	st2Hdr    []byte
	segAZ     []byte
	segBZ     []byte
	compA     uint32
	decompA   uint32
	compB     uint32
	decompB   uint32
	pairPlain []byte
	tree      []byte
}

func leU32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

func parsePVF(data []byte, keys map[string][]byte) (*pvfLayout, error) {
	if len(data) < 0x30 {
		return nil, errors.New("pvf too short")
	}
	hdr := lcgCrypt(data[0:0x30], keys["head"], lcgMain)
	var f [12]uint32
	for i := 0; i < 12; i++ {
		f[i] = leU32(hdr[i*4:])
	}
	if f[0] != pvfMagic {
		return nil, fmt.Errorf("header magic mismatch %#x (source keys not match?)", f[0])
	}
	l := &pvfLayout{data: data, keys: keys, f: f}
	l.nFiles = f[6]
	l.nBlocks = f[9]
	l.st1Size = f[10]
	l.st2Size = f[11]
	l.treeOff = 0x30
	l.st1Off = l.treeOff + int(l.nFiles)*24
	l.st2Off = l.st1Off + int(l.st1Size)
	l.pairOff = l.st2Off + int(l.st2Size)
	l.bodyOff = l.pairOff + int(l.nBlocks)*8
	if l.bodyOff > len(data) || l.st2Off > len(data) || l.pairOff > len(data) {
		return nil, errors.New("pvf offsets out of range")
	}
	st2 := data[l.st2Off : l.st2Off+int(l.st2Size)]
	if len(st2) < 16 {
		return nil, errors.New("strtab2 too short")
	}
	pos := 8
	l.compA = leU32(st2[pos:]) ^ xorA
	l.decompA = leU32(st2[pos+4:]) ^ l.compA
	pos += 8
	if pos+int(l.compA) > len(st2) {
		return nil, errors.New("segA out of range")
	}
	l.segAZ = lcgCrypt(st2[pos:pos+int(l.compA)], keys["stra"], lcgStr)
	pos += int(l.compA)
	if pos+8 > len(st2) {
		return nil, errors.New("segB hdr out of range")
	}
	l.compB = leU32(st2[pos:]) ^ xorB
	l.decompB = leU32(st2[pos+4:]) ^ l.compB
	pos += 8
	if pos+int(l.compB) > len(st2) {
		return nil, errors.New("segB out of range")
	}
	l.segBZ = lcgCrypt(st2[pos:pos+int(l.compB)], keys["strb"], lcgStr)
	l.st2Hdr = st2[0:8]
	l.pairPlain = lcgCrypt(data[l.pairOff:l.pairOff+int(l.nBlocks)*8], keys["grpi"], lcgMain)
	l.tree = data[l.treeOff:l.st1Off]
	return l, nil
}

func (l *pvfLayout) blockZ(blk uint32) ([]byte, error) {
	c0 := uint32(0)
	if blk > 0 {
		c0 = leU32(l.pairPlain[(blk-1)*8:])
	}
	c1 := leU32(l.pairPlain[blk*8:])
	if c1 < c0 || int(c1) > len(l.data)-l.bodyOff {
		return nil, errors.New("block range out of bounds")
	}
	raw := l.data[l.bodyOff+int(c0) : l.bodyOff+int(c1)]
	return lcgCrypt(raw, l.keys["body"], lcgMain), nil
}

// rekeyPVF 将 srcData 按 newKeys 重打包（源需可由 oldKeys 解密）。
// 输出与 pvf_protect.py rekey() 字节级一致：header f[7] 修正为真实 body_off。
func rekeyPVF(srcData []byte, oldKeys, newKeys map[string][]byte, newXorA, newXorB uint32) ([]byte, error) {
	old, err := parsePVF(srcData, oldKeys)
	if err != nil {
		return nil, fmt.Errorf("parse src with old keys: %w", err)
	}
	if newXorA == 0 {
		newXorA = xorA
	}
	if newXorB == 0 {
		newXorB = xorB
	}
	out := make([]byte, 0, len(srcData))

	// 1. header：明文 -> 新 key；f[7] = 真实 body_off。
	f := old.f
	f[7] = uint32(old.bodyOff)
	var hdr [48]byte
	for i := 0; i < 12; i++ {
		putU32(hdr[i*4:], f[i])
	}
	out = append(out, lcgCrypt(hdr[:], newKeys["head"], lcgMain)...)

	// 2. tree：明文原样。
	out = append(out, srcData[old.treeOff:old.st1Off]...)

	// 3. strtab1：旧 key 剥明文 -> 新 key 加密。
	st1Plain := lcgCrypt(srcData[old.st1Off:old.st2Off], oldKeys["hash"], lcgMain)
	out = append(out, lcgCrypt(st1Plain, newKeys["hash"], lcgMain)...)

	// 4. strtab2：8B 头原样；段 A/B 分别换 key；段头按新掩码重写。
	out = append(out, old.st2Hdr...)
	var headA [8]byte
	putU32(headA[0:], old.compA^newXorA)
	putU32(headA[4:], old.compA^old.decompA)
	out = append(out, headA[:]...)
	out = append(out, lcgCrypt(old.segAZ, newKeys["stra"], lcgStr)...)
	var headB [8]byte
	putU32(headB[0:], old.compB^newXorB)
	putU32(headB[4:], old.compB^old.decompB)
	out = append(out, headB[:]...)
	out = append(out, lcgCrypt(old.segBZ, newKeys["strb"], lcgStr)...)

	// 5. pair：明文 -> 新 key。
	out = append(out, lcgCrypt(old.pairPlain, newKeys["grpi"], lcgMain)...)

	// 6. body：逐块剥 zlib 流 -> 新 key（顺序/长度不变）。
	bodyNew := make([]byte, 0, len(srcData)-old.bodyOff)
	for blk := uint32(0); blk < old.nBlocks; blk++ {
		z, err := old.blockZ(blk)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", blk, err)
		}
		bodyNew = append(bodyNew, lcgCrypt(z, newKeys["body"], lcgMain)...)
	}
	out = append(out, bodyNew...)
	return out, nil
}

// newRandomKeySet 生成 96 hex 随机 key_set。
func newRandomKeySet() (string, error) {
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// protectPVFFile 加密轮换 Script.pvf（文件级入口）。
// srcPath：源 Script.pvf（标准密钥或 oldKeySet 加密的版本）；
// dstPath：输出文件路径；
// oldKeySet：96hex 源文件密钥；空串 = 内置标准密钥；
// newKeySet：96hex 新密钥；空串 = 随机生成。
// 返回（新密钥 96hex，输出文件 sha256，错误）。
func protectPVFFile(srcPath, dstPath, oldKeySet, newKeySet string) (string, string, error) {
	var oldKeys map[string][]byte
	var err error
	if oldKeySet != "" {
		oldKeys, err = parseKeySet(oldKeySet)
	} else {
		oldKeys, err = standardOldKeys()
	}
	if err != nil {
		return "", "", err
	}
	if newKeySet == "" {
		newKeySet, err = newRandomKeySet()
		if err != nil {
			return "", "", err
		}
	}
	newKeys, err := parseKeySet(newKeySet)
	if err != nil {
		return "", "", err
	}
	srcData, err := os.ReadFile(srcPath)
	if err != nil {
		return "", "", fmt.Errorf("read src: %w", err)
	}
	outData, err := rekeyPVF(srcData, oldKeys, newKeys, xorA, xorB)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(dstPath, outData, 0o644); err != nil {
		return "", "", fmt.Errorf("write dst: %w", err)
	}
	sum := sha256.Sum256(outData)
	return newKeySet, hex.EncodeToString(sum[:]), nil
}

// sha256File 计算文件 sha256（hex 小写）。
func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
