// pvfhashcheck 校验 protected PVF 的 HASH 段与文件表是否自洽（只读）。
//
// HASH 段（文件表之后、字符串池之前）明文结构（见 ProtectedPvfHashSection.cs）：
//
//	int32 fileCount
//	fileCount × (int32 nameOffset, int32 pathOffset)   // 文件表逐条同序的镜像
//	int32 stringCount
//	stringCount × int32 stringOffset                   // 文件表引用到的偏移，按字符串升序去重
//
// 若工具改了文件表却没有同步维护这一段，包就自相矛盾；客户端若按这一段查路径，
// 新增文件就会「存在但查不到」——表现为邮件附件空白。
//
//	go run ./cmd/scratch/pvfhashcheck -pvf <Script.pvf>
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sort"
	"unicode/utf16"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	headerSize    = 0x30
	fileItemSize  = 0x18
	containerMagic = 0x269EC3
)

func decryptProtectedCore(key string, buf []byte, magic uint32) {
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

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf 路径")
	maxShow := flag.Int("show", 20, "最多打印多少条不一致示例")
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

	hdr := append([]byte(nil), data[:headerSize]...)
	decryptProtectedCore("hEAd", hdr, containerMagic)
	fileCount := i32(hdr[24:28])
	hashSize := i32(hdr[40:44])

	tableOff := headerSize
	hashOff := tableOff + fileCount*fileItemSize
	fmt.Printf("文件: %s (%d 字节)\n", *pvfPath, len(data))
	fmt.Printf("fileCount(header) = %d\nhashSize = %d  (= %d 个 int32)\n\n", fileCount, hashSize, hashSize/4)

	// ---- 文件表 ----
	type tf struct{ nameOff, pathOff int }
	table := make([]tf, fileCount)
	for i := 0; i < fileCount; i++ {
		off := tableOff + i*fileItemSize
		table[i] = tf{nameOff: i32(data[off : off+4]), pathOff: i32(data[off+4 : off+8])}
	}

	// ---- HASH 段 ----
	if hashSize <= 0 || hashOff+hashSize > len(data) {
		fmt.Println("HASH 段越界，无法解析 ❌")
		os.Exit(1)
	}
	plain := append([]byte(nil), data[hashOff:hashOff+hashSize]...)
	decryptProtectedCore("hash", plain, containerMagic)

	intCount := len(plain) / 4
	mirrorCount := i32(plain[0:4])
	fmt.Printf("=== HASH 段 ===\nint32 总数 = %d\n镜像条数 mirrorCount = %d\n", intCount, mirrorCount)
	fmt.Printf("镜像条数 vs header.fileCount: %d vs %d  -> %s\n", mirrorCount, fileCount,
		map[bool]string{true: "相等 ✅", false: "不相等 ❌"}[mirrorCount == fileCount])

	if 1+mirrorCount*2+1 > intCount {
		fmt.Println("镜像区越界 ❌")
		os.Exit(1)
	}

	// 逐条比对镜像
	mismatch := 0
	firstBad := -1
	var samples []string
	n := mirrorCount
	if fileCount < n {
		n = fileCount
	}
	for i := 0; i < n; i++ {
		mn := i32(plain[(1+i*2)*4 : (1+i*2)*4+4])
		mp := i32(plain[(2+i*2)*4 : (2+i*2)*4+4])
		if mn != table[i].nameOff || mp != table[i].pathOff {
			mismatch++
			if firstBad < 0 {
				firstBad = i
			}
			if len(samples) < *maxShow {
				samples = append(samples, fmt.Sprintf("  idx=%d  表(name=%d,path=%d)  HASH镜像(name=%d,path=%d)",
					i, table[i].nameOff, table[i].pathOff, mn, mp))
			}
		}
	}
	fmt.Printf("镜像区与文件表逐条比对: 比对 %d 条，不一致 %d 条  -> %s\n", n, mismatch,
		map[bool]string{true: "完全一致 ✅", false: "不一致 ❌"}[mismatch == 0])
	if firstBad >= 0 {
		fmt.Printf("首个不一致下标: %d\n", firstBad)
	}
	for _, s := range samples {
		fmt.Println(s)
	}

	// 镜像多出来的尾部（若 mirrorCount > fileCount）
	if mirrorCount > fileCount {
		fmt.Printf("\n镜像区比文件表多 %d 条（尾部为陈旧条目）:\n", mirrorCount-fileCount)
		for i := fileCount; i < mirrorCount && i < fileCount+*maxShow; i++ {
			fmt.Printf("  mirror[%d] name=%d path=%d\n", i,
				i32(plain[(1+i*2)*4:(1+i*2)*4+4]), i32(plain[(2+i*2)*4:(2+i*2)*4+4]))
		}
	}

	// ---- 字典区 ----
	dictPos := 1 + mirrorCount*2
	stringCount := i32(plain[dictPos*4 : dictPos*4+4])
	fmt.Printf("\n=== 字典区 ===\nstringCount = %d\n", stringCount)
	expect := intCount - dictPos - 1
	fmt.Printf("按长度推算应为 %d  -> %s\n", expect,
		map[bool]string{true: "一致 ✅", false: "不一致 ❌"}[stringCount == expect])
	if dictPos+1+stringCount > intCount {
		fmt.Println("字典区越界 ❌")
		os.Exit(1)
	}

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	// 把「文件表 vs 镜像」的前几条差异解析成字符串，便于人眼核对。
	if mismatch > 0 {
		fmt.Printf("\n前几条差异解析出的字符串（文件表 / 镜像各是什么）:\n")
		shown := 0
		for i := 0; i < n && shown < 6; i++ {
			mn := i32(plain[(1+i*2)*4 : (1+i*2)*4+4])
			mp := i32(plain[(2+i*2)*4 : (2+i*2)*4+4])
			if mn == table[i].nameOff && mp == table[i].pathOff {
				continue
			}
			fmt.Printf("  idx=%d\n", i)
			fmt.Printf("      文件表: dir=%q name=%q\n", archive.ResolveString(table[i].pathOff), archive.ResolveString(table[i].nameOff))
			fmt.Printf("      HASH镜像: dir=%q name=%q\n", archive.ResolveString(mp), archive.ResolveString(mn))
			shown++
		}
	}

	dict := make([]int, stringCount)
	for i := 0; i < stringCount; i++ {
		dict[i] = i32(plain[(dictPos+1+i)*4 : (dictPos+1+i)*4+4])
	}
	// 去重检查（按偏移），以及「按字符串升序」检查。
	// 注意：字典区是按**字符串值**排序的，不是按偏移数值 —— 所以不能拿偏移比大小。
	dup := 0
	for i := 1; i < len(dict); i++ {
		if dict[i] == dict[i-1] {
			dup++
		}
	}
	asc, tieBreak := 0, 0
	for i := 1; i < len(dict); i++ {
		prev := archive.ResolveString(dict[i-1])
		cur := archive.ResolveString(dict[i])
		if cur < prev {
			asc++
		} else if cur == prev && dict[i] < dict[i-1] {
			tieBreak++
		}
	}
	fmt.Printf("字典区重复项(按偏移): %d  -> %s\n", dup,
		map[bool]string{true: "无重复 ✅", false: "有重复 ❌"}[dup == 0])
	fmt.Printf("字典区按字符串值非升序点: %d，同值未按偏移递增: %d  -> %s\n", asc, tieBreak,
		map[bool]string{true: "按字符串升序 ✅", false: "顺序有问题 ❌"}[asc == 0 && tieBreak == 0])

	// 字典区是否 == 文件表引用到的偏移集合
	want := map[int]bool{}
	for _, t := range table {
		if t.nameOff >= 0 {
			want[t.nameOff] = true
		}
		if t.pathOff >= 0 {
			want[t.pathOff] = true
		}
	}
	have := map[int]bool{}
	for _, v := range dict {
		have[v] = true
	}
	missingFromDict := 0
	var missSamples []string
	for v := range want {
		if !have[v] {
			missingFromDict++
			if len(missSamples) < *maxShow {
				missSamples = append(missSamples, fmt.Sprintf("  文件表用了偏移 %d (%q) 但字典区没有",
					v, archive.ResolveString(v)))
			}
		}
	}
	extraInDict := 0
	for v := range have {
		if !want[v] {
			extraInDict++
		}
	}
	fmt.Printf("文件表引用到的不同偏移数: %d，字典区条数: %d\n", len(want), len(have))
	fmt.Printf("字典区缺失(文件表要用但没有): %d  %s\n", missingFromDict,
		map[bool]string{true: "✅", false: "❌"}[missingFromDict == 0])
	fmt.Printf("字典区多余(文件表不用但有): %d\n", extraInDict)
	for _, s := range missSamples {
		fmt.Println(s)
	}

	// ---- 结论 ----
	fmt.Printf("\n===== 结论 =====\n")
	bad := mismatch > 0 || mirrorCount != fileCount || missingFromDict > 0 || asc > 0 || tieBreak > 0 || dup > 0
	if bad {
		fmt.Println("HASH 段与文件表【不自洽】❌ —— 改过文件表却没有同步这一段。")
	} else {
		fmt.Println("HASH 段与文件表【自洽】✅")
	}

	// 若不自洽，找出文件表里有、镜像区里没有的那些条目
	if mismatch > 0 || mirrorCount != fileCount {
		fmt.Printf("\n=== 定位：文件表中未进入镜像区（或镜像错位）的条目 ===\n")
		shown := 0
		for i := 0; i < fileCount && shown < *maxShow; i++ {
			if i >= mirrorCount {
				fmt.Printf("  idx=%d 表(name=%d,path=%d) 镜像区不存在（镜像只有 %d 条）\n",
					i, table[i].nameOff, table[i].pathOff, mirrorCount)
				shown++
				continue
			}
			mn := i32(plain[(1+i*2)*4 : (1+i*2)*4+4])
			mp := i32(plain[(2+i*2)*4 : (2+i*2)*4+4])
			if mn != table[i].nameOff || mp != table[i].pathOff {
				fmt.Printf("  idx=%d 表(name=%d,path=%d) 镜像(name=%d,path=%d)\n", i,
					table[i].nameOff, table[i].pathOff, mn, mp)
				shown++
			}
		}
	}

	// 统计镜像区的偏移值是否覆盖了全部文件表偏移
	if mismatch > 0 {
		mirrorSet := map[[2]int]bool{}
		for i := 0; i < mirrorCount; i++ {
			mirrorSet[[2]int{
				i32(plain[(1+i*2)*4 : (1+i*2)*4+4]),
				i32(plain[(2+i*2)*4 : (2+i*2)*4+4]),
			}] = true
		}
		notInMirror := 0
		var paths []string
		files := archive.Files()
		for i := 0; i < fileCount; i++ {
			key := [2]int{table[i].nameOff, table[i].pathOff}
			if !mirrorSet[key] {
				notInMirror++
				if len(paths) < *maxShow {
					if i < len(files) {
						paths = append(paths, files[i].ArchivePath)
					}
				}
			}
		}
		fmt.Printf("\n文件表条目中「在镜像区里找不到完全相同的一对」共 %d 条\n", notInMirror)
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Printf("  %s\n", p)
		}
	}
}
