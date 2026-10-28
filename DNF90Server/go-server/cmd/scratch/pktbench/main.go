// Command pktbench 临时基准：量化「服务端回包二次加密」的每包开销，
// 与同一条路径上**已经在做**的 CRC 逐字节循环对比。用完即删。
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"time"

	"golang.org/x/crypto/chacha20"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

const rounds = 2000000

func main() {
	sizes := []int{43, 73, 233, 1024, 2048}
	fmt.Printf("rounds per case = %d\n\n", rounds)
	fmt.Printf("%6s | %-22s | %-22s | %-22s | %-22s | %-22s\n",
		"bytes", "AES-128-CTR in-place", "AES-128-GCM (reuse dst)", "ChaCha20 XOR", "existing CRC32 loop", "GCM with alloc")
	fmt.Println("-------|------------------------|------------------------|------------------------|------------------------|----------------------")

	for _, size := range sizes {
		src := make([]byte, size)
		if _, err := rand.Read(src); err != nil {
			panic(err)
		}
		buf := make([]byte, size)
		dst := make([]byte, 0, size+32)

		block, err := aes.NewCipher(make([]byte, 16))
		if err != nil {
			panic(err)
		}
		ctr := cipher.NewCTR(block, make([]byte, aes.BlockSize))
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			panic(err)
		}
		chacha, err := chacha20.NewUnauthenticatedCipher(make([]byte, 32), make([]byte, chacha20.NonceSize))
		if err != nil {
			panic(err)
		}

		ctrNS := timeIt(func() {
			ctr.XORKeyStream(buf, src)
		})
		gcmReuseNS := timeIt(func() {
			_ = gcm.Seal(dst[:0], buf[:12], src, nil)
		})
		gcmAllocNS := timeIt(func() {
			out := gcm.Seal(nil, buf[:12], src, nil)
			sink += len(out)
		})
		chachaNS := timeIt(func() {
			chacha.XORKeyStream(buf, src)
		})
		crcNS := timeIt(func() {
			sum, err := dnfproto.ChecksumRange(src, 0, len(src))
			if err != nil {
				panic(err)
			}
			sink ^= int(sum & 1)
		})

		fmt.Printf("%6d | %8.1f ns (%6.0f MB/s) | %8.1f ns (%6.0f MB/s) | %8.1f ns (%6.0f MB/s) | %8.1f ns (%6.0f MB/s) | %8.1f ns\n",
			size,
			ctrNS, mbps(size, ctrNS),
			gcmReuseNS, mbps(size, gcmReuseNS),
			chachaNS, mbps(size, chachaNS),
			crcNS, mbps(size, crcNS),
			gcmAllocNS)
	}
	fmt.Printf("\nsink=%d\n", sink&1)
}

var sink int

func timeIt(fn func()) float64 {
	// 预热
	for i := 0; i < 20000; i++ {
		fn()
	}
	start := time.Now()
	for i := 0; i < rounds; i++ {
		fn()
	}
	return float64(time.Since(start).Nanoseconds()) / float64(rounds)
}

func mbps(size int, ns float64) float64 {
	if ns <= 0 {
		return 0
	}
	return float64(size) / ns * 1000 // bytes/ns → MB/s (10^6)
}
