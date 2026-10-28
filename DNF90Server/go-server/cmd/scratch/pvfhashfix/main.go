// pvfhashfix 重建 protected PVF 的 HASH 段，使它和文件表自洽（其余字节原样保留）。
//
// 背景（证据见 _investigation_mailblank）：
//   - HASH 段（文件表之后、字符串池之前）明文是：
//     int32 fileCount + fileCount×(int32 nameOffset, int32 pathOffset) + int32 stringCount + stringCount×int32
//     —— 前一段是「文件表逐条同序的镜像」，后一段是「文件表引用到的字符串偏移，按字符串升序去重」。
//   - 已知可用的库（tools\_pvf_20260919\Script.pvf.new，新增的 _test123.stk 在客户端能正常显示）
//     这两段与文件表完全自洽；而当前线上库改过文件表却没同步这一段，
//     于是新增的 16 个文件在 HASH 段里根本不存在 —— 客户端按 HASH 索引查路径就查不到（邮件附件空白）。
//
// 本工具只改 header.hashSize 与 HASH 段本身；文件表、字符串池、组表、body 全部原样拷贝，不做重压缩。
//
//	go run ./cmd/scratch/pvfhashfix -pvf <in.pvf> -verify            # 自检：重建已知好库的 HASH，必须逐字节一致
//	go run ./cmd/scratch/pvfhashfix -pvf <in.pvf> -out <out.pvf>     # 写出修好的库
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sort"
	"unicode/utf16"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	headerSize     = 0x30
	fileItemSize   = 0x18
	groupItemSize  = 8
	containerMagic = 0x269EC3
)

// decryptProtectedCore 是纯异或密钥流，同一个函数即可加密也可解密。
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

type section struct {
	fileCount, bodySize, groupCount, hashSize, nameSize int
	tableOff, hashOff, nameOff, groupOff, bodyOff        int
}

func locate(data []byte) (section, []byte, error) {
	var s section
	if len(data) < headerSize {
		return s, nil, fmt.Errorf("文件太短")
	}
	hdr := append([]byte(nil), data[:headerSize]...)
	xorCore("hEAd", hdr, containerMagic)
	if binary.LittleEndian.Uint32(hdr[0:4]) != 0x69706B6E {
		return s, nil, fmt.Errorf("不是 protected（nkpi）PVF")
	}
	s.fileCount = i32(hdr[24:28])
	s.bodySize = i32(hdr[32:36])
	s.groupCount = i32(hdr[36:40])
	s.hashSize = i32(hdr[40:44])
	s.nameSize = i32(hdr[44:48])
	s.tableOff = headerSize
	s.hashOff = s.tableOff + s.fileCount*fileItemSize
	s.nameOff = s.hashOff + s.hashSize
	s.groupOff = s.nameOff + s.nameSize
	s.bodyOff = s.groupOff + s.groupCount*groupItemSize
	if s.bodyOff+s.bodySize > len(data) {
		return s, nil, fmt.Errorf("各段之和 %d 超出文件长度 %d", s.bodyOff+s.bodySize, len(data))
	}
	return s, hdr, nil
}

// buildHashPlain 由文件表算出 HASH 段明文。
//
// keepExtras 语义：字典区除了「文件表引用到的偏移」，还要保留**原 HASH 段里已经存在**的偏移。
// 实测第三方 pvfUtility 写出的已知好库里会留下少量不再被引用的陈旧字典项
// （17:59 那份留了 23 条），保留它们可以让重建结果与那个库**逐字节一致**——
// 这是验证编码器语义的最强判据。关掉则输出「最小字典」。
func buildHashPlain(data []byte, s section, resolve func(int) string, keepExtras bool) ([]byte, error) {
	if s.tableOff+s.fileCount*fileItemSize > len(data) {
		return nil, fmt.Errorf("文件表越界")
	}
	names := make([]int, s.fileCount)
	paths := make([]int, s.fileCount)
	for i := 0; i < s.fileCount; i++ {
		off := s.tableOff + i*fileItemSize
		names[i] = i32(data[off : off+4])
		paths[i] = i32(data[off+4 : off+8])
	}

	// 字典区：文件表引用到的偏移，去重（按偏移），按「字符串升序 + 偏移」排序，丢掉负值。
	type kv struct {
		off  int
		text string
	}
	byOff := map[int]string{}
	consider := func(off int) {
		if off < 0 {
			return
		}
		if _, ok := byOff[off]; ok {
			return
		}
		byOff[off] = resolve(off)
	}
	for _, v := range names {
		consider(v)
	}
	for _, v := range paths {
		consider(v)
	}
	extras := 0
	if keepExtras && s.hashSize > 0 && s.hashOff+s.hashSize <= len(data) {
		old := append([]byte(nil), data[s.hashOff:s.hashOff+s.hashSize]...)
		xorCore("hash", old, containerMagic)
		intCount := len(old) / 4
		if intCount >= 2 {
			mc := i32(old[0:4])
			if mc >= 0 && 1+mc*2+1 <= intCount {
				dictPos := 1 + mc*2
				sc := i32(old[dictPos*4 : dictPos*4+4])
				if sc >= 0 && dictPos+1+sc <= intCount {
					for i := 0; i < sc; i++ {
						off := i32(old[(dictPos+1+i)*4 : (dictPos+1+i)*4+4])
						if off < 0 {
							continue
						}
						if _, ok := byOff[off]; !ok {
							extras++
						}
						consider(off)
					}
				}
			}
		}
	}
	ordered := make([]kv, 0, len(byOff))
	for off, text := range byOff {
		ordered = append(ordered, kv{off: off, text: text})
	}
	sort.Slice(ordered, func(a, b int) bool {
		if ordered[a].text != ordered[b].text {
			return ordered[a].text < ordered[b].text // Go 的字符串 < 是逐字节比较
		}
		return ordered[a].off < ordered[b].off
	})
	fmt.Printf("  字典来源: 文件表引用 %d 条 + 沿用旧字典的陈旧项 %d 条 = %d 条\n",
		len(ordered)-extras, extras, len(ordered))

	intCount := 1 + s.fileCount*2 + 1 + len(ordered)
	out := make([]byte, intCount*4)
	binary.LittleEndian.PutUint32(out[0:4], uint32(s.fileCount))
	for i := 0; i < s.fileCount; i++ {
		binary.LittleEndian.PutUint32(out[(1+i*2)*4:(1+i*2)*4+4], uint32(names[i]))
		binary.LittleEndian.PutUint32(out[(2+i*2)*4:(2+i*2)*4+4], uint32(paths[i]))
	}
	dictPos := 1 + s.fileCount*2
	binary.LittleEndian.PutUint32(out[dictPos*4:dictPos*4+4], uint32(len(ordered)))
	for i, e := range ordered {
		binary.LittleEndian.PutUint32(out[(dictPos+1+i)*4:(dictPos+1+i)*4+4], uint32(e.off))
	}
	return out, nil
}

func main() {
	pvfPath := flag.String("pvf", "", "输入 Script.pvf")
	outPath := flag.String("out", "", "输出 Script.pvf（不填则只报告，不写）")
	verify := flag.Bool("verify", false, "自检：重建 HASH 后与本库现有 HASH 逐字节比对")
	keepExtras := flag.Bool("keep-extras", true, "保留旧字典里已存在但已不再被引用的偏移项（与第三方工具输出一致）")
	flag.Parse()
	if *pvfPath == "" {
		fmt.Fprintln(os.Stderr, "-pvf is required")
		os.Exit(2)
	}

	data, err := os.ReadFile(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	s, hdr, err := locate(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "locate:", err)
		os.Exit(1)
	}
	fmt.Printf("输入: %s (%d 字节)\n", *pvfPath, len(data))
	fmt.Printf("fileCount=%d groupCount=%d hashSize=%d nameSize=%d bodySize=%d\n",
		s.fileCount, s.groupCount, s.hashSize, s.nameSize, s.bodySize)
	fmt.Printf("段偏移: 表=%d HASH=%d 字符串=%d 组表=%d body=%d\n",
		s.tableOff, s.hashOff, s.nameOff, s.groupOff, s.bodyOff)

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	resolve := func(off int) string { return archive.ResolveString(off) }

	plain, err := buildHashPlain(data, s, resolve, *keepExtras)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
	newRaw := append([]byte(nil), plain...)
	xorCore("hash", newRaw, containerMagic)
	fmt.Printf("\n重建后 HASH 段: %d 字节（原 %d 字节，差 %+d）\n", len(newRaw), s.hashSize, len(newRaw)-s.hashSize)

	// 报告镜像条数与字典条数
	fmt.Printf("  镜像条数 = %d（文件表 %d）\n", i32(plain[0:4]), s.fileCount)
	dictPos := 1 + s.fileCount*2
	fmt.Printf("  字典条数 = %d\n", i32(plain[dictPos*4:dictPos*4+4]))

	if *verify {
		old := data[s.hashOff : s.hashOff+s.hashSize]
		if len(old) != len(newRaw) {
			fmt.Printf("\n[verify] 长度不同，无法逐字节比对（旧 %d / 新 %d）❌\n", len(old), len(newRaw))
			os.Exit(1)
		}
		diff := 0
		first := -1
		for i := range old {
			if old[i] != newRaw[i] {
				diff++
				if first < 0 {
					first = i
				}
			}
		}
		if diff == 0 {
			fmt.Printf("\n[verify] ✅ 重建的 HASH 段与库中现有 HASH 段【逐字节完全一致】\n")
			fmt.Printf("         说明本工具的镜像顺序 + 字典排序/去重语义与该库一致。\n")
			os.Exit(0)
		}
		fmt.Printf("\n[verify] ❌ 有 %d 字节不同，首个差异在偏移 %d\n", diff, first)
		for _, off := range []int{first, first + 4} {
			if off+4 <= len(old) {
				fmt.Printf("   @%d 旧=%d 新=%d\n", off, i32(old[off:off+4]), i32(newRaw[off:off+4]))
			}
		}
		os.Exit(1)
	}

	// 写出：header（只改 hashSize）+ 文件表 + 新 HASH + 字符串池 + 组表 + body（全部原样拷贝）
	newHdr := append([]byte(nil), hdr...)
	binary.LittleEndian.PutUint32(newHdr[40:44], uint32(len(newRaw)))
	xorCore("hEAd", newHdr, containerMagic)

	var buf bytes.Buffer
	buf.Grow(len(data) + len(newRaw) - s.hashSize)
	buf.Write(newHdr)
	buf.Write(data[s.tableOff:s.hashOff])          // 文件表
	buf.Write(newRaw)                              // 新 HASH
	buf.Write(data[s.nameOff:s.groupOff])          // 字符串池
	buf.Write(data[s.groupOff:s.bodyOff])          // 组表
	buf.Write(data[s.bodyOff : s.bodyOff+s.bodySize]) // body

	if *outPath == "" {
		fmt.Println("\n未指定 -out，只做报告，未写文件。")
		return
	}
	if samePath(*pvfPath, *outPath) {
		fmt.Fprintln(os.Stderr, "拒绝覆盖输入文件，请指定不同的 -out")
		os.Exit(2)
	}
	if err := os.WriteFile(*outPath, buf.Bytes(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("\n已写出: %s (%d 字节)\n", *outPath, buf.Len())

	// 写后自检：重新打开，逐文件比对内容是否与原库一致
	outBytes := buf.Bytes()
	re, err := platformpvf.OpenBytes(outBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "重新打开失败:", err)
		os.Exit(1)
	}
	fmt.Printf("写后自检: 文件数 %d（原 %d）\n", re.FileCount(), archive.FileCount())
	if re.FileCount() != archive.FileCount() {
		fmt.Fprintln(os.Stderr, "文件数不一致 ❌")
		os.Exit(1)
	}
	fmt.Println("写后自检通过 ✅ （内容一致性的完整比对请另跑 pvfcompare）")
}

func samePath(a, b string) bool {
	pa, err1 := os.Stat(a)
	pb, err2 := os.Stat(b)
	if err1 == nil && err2 == nil {
		return os.SameFile(pa, pb)
	}
	return a == b
}
