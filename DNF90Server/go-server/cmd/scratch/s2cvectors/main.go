// Command s2cvectors 打印 C 侧自检要用的固定向量（用完即留：它是跨实现契约的一部分）。
//
// 用法：go run ./cmd/scratch/s2cvectors
// 输出必须与 client-patch/s2ccrypt.c 内建向量、protocol/s2ccipher_test.go 的常量三者一致。
package main

import (
	"encoding/hex"
	"fmt"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func main() {
	key, err := dnfproto.ParseS2CMasterKey([]byte(dnfproto.DefaultS2CMasterKeyBase64))
	if err != nil {
		panic(err)
	}
	nonce, _ := hex.DecodeString("00112233445566778899aabbccddeeff")

	fmt.Printf("master key      : %s\n", dnfproto.DefaultS2CMasterKeyBase64)
	fmt.Printf("master key hex  : %s\n", hex.EncodeToString(key))
	fmt.Printf("connection nonce: %s\n\n", hex.EncodeToString(nonce))

	cases := []struct {
		name  string
		msgID uint16
		seq   uint16
		body  []byte
	}{
		{"short op38 body", 38, 4242, mustHex("0b000000ff00cafe0102030405060708090a")},
		{"multi-block 64B", 45, 777, sequential(64)},
		{"block aligned 32B", 29, 1, sequential(32)},
		{"one byte", 1, 65535, []byte{0x5a}},
		{"2048 bytes", 1518, 2, sequential(2048)},
	}
	for _, testCase := range cases {
		cipher, err := dnfproto.NewS2CCipher(key, nonce)
		if err != nil {
			panic(err)
		}
		sealed := append([]byte(nil), testCase.body...)
		cipher.Apply(sealed, testCase.msgID, testCase.seq)
		fmt.Printf("%-18s msgID=%-5d seq=%-5d len=%-5d\n", testCase.name, testCase.msgID, testCase.seq, len(testCase.body))
		fmt.Printf("  plain  : %s\n", printableOrHex(testCase.body))
		fmt.Printf("  cipher : %s\n", printableOrHex(sealed))
	}
}

func mustHex(text string) []byte {
	raw, err := hex.DecodeString(text)
	if err != nil {
		panic(err)
	}
	return raw
}

func sequential(size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}

// 短向量直接给十六进制；超过 80 字节的只给前 64 字节（C 侧自检按同样规则比对）。
func printableOrHex(data []byte) string {
	if len(data) <= 80 {
		return hex.EncodeToString(data)
	}
	return fmt.Sprintf("first64=%s pattern=byte(i)", hex.EncodeToString(data[:64]))
}
