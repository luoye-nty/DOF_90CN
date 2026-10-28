// pvfhashcorrupt 对 protected PVF 的 HASH 段做**受控破坏**，用来验证 pvfhashfix 的通用性。
//
// 思路：拿一个已知好库，人为制造出「我们实际遇到过的那几类失同步」，
// 再让 pvfhashfix 去修；如果修完的输出与原始好库**逐字节完全相同**，就说明
// 这类破坏是可逆的、修复逻辑不依赖任何特定库的巧合。
//
// 只改 HASH 段内部的字节，段长度与所有其它段完全不变（就地解密→改→就地加密）。
//
//	go run ./cmd/scratch/pvfhashcorrupt -pvf <好库> -out <坏库> -mode <模式> -n <N>
//
// 模式:
//	mirror-rot   镜像行整体循环右移 N 行            （模拟「顺序漂移」）
//	mirror-dup   末尾 N 行镜像覆盖成它前面 N 行     （模拟「新增条目没进镜像」）
//	dict-dup     末尾 N 条字典项覆盖成第 0 条       （模拟「字典缺项」）
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"unicode/utf16"
)

const (
	headerSize     = 0x30
	fileItemSize   = 0x18
	groupItemSize  = 8
	containerMagic = 0x269EC3
)

func xorCore(key string, buf []byte, magic uint32) {
	if key == "" || len(buf) == 0 {
		return
	}
	words := utf16.Encode([]rune(key))
	if len(words) < 4 {
		return
	}
	size := len(buf)
	tail := size
	seed := uint32(0x339E9711)*uint32(words[0]) +
		0x393*(uint32(words[3])+0x393*(uint32(words[2])+0x393*uint32(words[1])))
	if size >= 4 {
		quadCount := size >> 2
		tail = size - (quadCount << 2)
		for idx := 0; idx < quadCount; idx++ {
			t1 := 0x343FD*seed + magic
			seed = 0x343FD*t1 + magic
			xorKey := (t1 & 0xFFFF0000) + (seed >> 16)
			off := idx << 2
			binary.LittleEndian.PutUint32(buf[off:off+4], binary.LittleEndian.Uint32(buf[off:off+4])^xorKey)
		}
	}
	if tail > 0 {
		t1 := 0x343FD*seed + magic
		t2 := 0x343FD*t1 + magic
		finalKey := (t1 & 0xFFFF0000) + (t2 >> 16)
		var kb [4]byte
		binary.LittleEndian.PutUint32(kb[:], finalKey)
		start := size - tail
		for idx := 0; idx < tail; idx++ {
			buf[start+idx] ^= kb[idx]
		}
	}
}

func i32(b []byte) int { return int(int32(binary.LittleEndian.Uint32(b))) }
func put(b []byte, v int) { binary.LittleEndian.PutUint32(b, uint32(int32(v))) }

func main() {
	pvfPath := flag.String("pvf", "", "输入（已知好库）")
	outPath := flag.String("out", "", "输出（受控破坏后的库）")
	mode := flag.String("mode", "", "mirror-rot | mirror-dup | dict-dup")
	n := flag.Int("n", 8, "破坏多少行/条")
	flag.Parse()
	if *pvfPath == "" || *outPath == "" || *mode == "" {
		fmt.Fprintln(os.Stderr, "-pvf -out -mode are required")
		os.Exit(2)
	}

	data, err := os.ReadFile(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	hdr := append([]byte(nil), data[:headerSize]...)
	xorCore("hEAd", hdr, containerMagic)
	if binary.LittleEndian.Uint32(hdr[0:4]) != 0x69706B6E {
		fmt.Fprintln(os.Stderr, "不是 protected（nkpi）PVF")
		os.Exit(2)
	}
	fileCount := i32(hdr[24:28])
	hashSize := i32(hdr[40:44])
	hashOff := headerSize + fileCount*fileItemSize
	if hashOff+hashSize > len(data) {
		fmt.Fprintln(os.Stderr, "HASH 段越界")
		os.Exit(2)
	}

	sec := append([]byte(nil), data[hashOff:hashOff+hashSize]...)
	xorCore("hash", sec, containerMagic)

	intCount := len(sec) / 4
	mirrorCount := i32(sec[0:4])
	dictPos := 1 + mirrorCount*2
	if mirrorCount < 0 || dictPos+1 > intCount {
		fmt.Fprintln(os.Stderr, "镜像是坏的，先修好再拿来当基准")
		os.Exit(2)
	}
	stringCount := i32(sec[dictPos*4 : dictPos*4+4])

	nn := *n
	fmt.Printf("输入: %s\n  文件数=%d 镜像=%d 字典=%d 模式=%s n=%d\n",
		*pvfPath, fileCount, mirrorCount, stringCount, *mode, nn)

	switch *mode {
	case "mirror-rot":
		if nn <= 0 || nn >= mirrorCount {
			fmt.Fprintln(os.Stderr, "n 越界")
			os.Exit(2)
		}
		// 把镜像行整体循环右移 nn 行：集合不变、顺序全乱。
		tmp := make([]byte, nn*8)
		copy(tmp, sec[4:4+nn*8])
		copy(sec[4:4+(mirrorCount-nn)*8], sec[4+nn*8:4+mirrorCount*8])
		copy(sec[4+(mirrorCount-nn)*8:4+mirrorCount*8], tmp)

	case "mirror-dup":
		if nn <= 0 || nn*2 > mirrorCount {
			fmt.Fprintln(os.Stderr, "n 越界")
			os.Exit(2)
		}
		// 末尾 nn 行覆盖成它前面 nn 行：末尾 nn 条文件表条目在镜像里就"不存在"了
		// —— 这正是线上库那个毛病的形态。
		copy(sec[4+(mirrorCount-nn)*8:4+mirrorCount*8],
			sec[4+(mirrorCount-2*nn)*8:4+(mirrorCount-nn)*8])

	case "dict-dup":
		if nn <= 0 || nn > stringCount {
			fmt.Fprintln(os.Stderr, "n 越界")
			os.Exit(2)
		}
		// 末尾 nn 条字典项覆盖成第 0 条：字典里丢了 nn 个真实偏移。
		first := append([]byte(nil), sec[(dictPos+1)*4:(dictPos+2)*4]...)
		for i := stringCount - nn; i < stringCount; i++ {
			copy(sec[(dictPos+1+i)*4:(dictPos+2+i)*4], first)
		}

	default:
		fmt.Fprintln(os.Stderr, "未知模式:", *mode)
		os.Exit(2)
	}

	xorCore("hash", sec, containerMagic)
	out := append([]byte(nil), data...)
	copy(out[hashOff:hashOff+hashSize], sec)
	if err := os.WriteFile(*outPath, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("已写出: %s (%d 字节，与原文件等长)\n", *outPath, len(out))
}
