// pvfstruct 打印 protected PVF 的容器结构：header 各段尺寸、HASH 段样本、
// 文件表原始字段与排序状况（只读）。
//
// 动机：客户端可能不是线性扫文件表，而是按 HASH 段或按「排序后的表」做查找。
// 若新增条目被追加到表尾而表本来是排序的，查找就会失效（新增道具查不到 → 邮件附件空白）。
//
//	go run ./cmd/scratch/pvfstruct -pvf <Script.pvf> [-tail 16] [-hashdump 256]
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf16"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	headerSize   = 0x30
	fileItemSize = 0x18
	groupItemSize = 8
	magicSignature = 0x69706B6E
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
	tail := flag.Int("tail", 16, "打印文件表尾部多少条")
	hashDump := flag.Int("hashdump", 256, "打印 HASH 段前多少字节")
	order := flag.Int("order", 12, "打印前多少个排序破坏点")
	rng := flag.String("range", "", "dump 文件表某区间，格式 from:to")
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
	fmt.Printf("文件: %s\n大小: %d 字节\n", *pvfPath, len(data))

	hdr := append([]byte(nil), data[:headerSize]...)
	decryptProtectedCore("hEAd", hdr, 0x269EC3)
	sig := binary.LittleEndian.Uint32(hdr[0:4])
	fmt.Printf("\n=== header ===\nmagic: 0x%08X (%s)\n", sig, map[bool]string{true: "nkpi ✅", false: "不匹配 ❌"}[sig == magicSignature])

	fileCount := i32(hdr[24:28])
	bodySize := i32(hdr[32:36])
	groupCount := i32(hdr[36:40])
	hashSize := i32(hdr[40:44])
	nameSize := i32(hdr[44:48])
	fmt.Printf("fileCount  = %d\n", fileCount)
	fmt.Printf("bodySize   = %d\n", bodySize)
	fmt.Printf("groupCount = %d\n", groupCount)
	fmt.Printf("hashSize   = %d\n", hashSize)
	fmt.Printf("nameSize   = %d\n", nameSize)

	tableOff := headerSize
	hashOff := tableOff + fileCount*fileItemSize
	nameOff := hashOff + hashSize
	groupOff := nameOff + nameSize
	bodyOff := groupOff + groupCount*groupItemSize
	fmt.Printf("\n=== 段偏移 ===\n文件表: %d .. %d  (%d 项 × 0x18)\n", tableOff, hashOff, fileCount)
	fmt.Printf("HASH  : %d .. %d  (%d 字节)\n", hashOff, nameOff, hashSize)
	fmt.Printf("字符串: %d .. %d  (%d 字节)\n", nameOff, groupOff, nameSize)
	fmt.Printf("组表  : %d .. %d  (%d 项 × 8)\n", groupOff, bodyOff, groupCount)
	fmt.Printf("body  : %d .. %d  (声明 %d)\n", bodyOff, bodyOff+bodySize, bodySize)
	fmt.Printf("段之和 = %d，实际文件 = %d  -> %s\n", bodyOff+bodySize, len(data),
		map[bool]string{true: "一致 ✅", false: "不一致 ❌"}[bodyOff+bodySize == len(data)])

	// ---- HASH 段样本 ----
	if hashSize > 0 {
		n := *hashDump
		if n > hashSize {
			n = hashSize
		}
		fmt.Printf("\n=== HASH 段前 %d 字节（原样，未解密）===\n", n)
		for off := 0; off < n; off += 16 {
			end := off + 16
			if end > n {
				end = n
			}
			var hexPart, asciiPart strings.Builder
			for i := off; i < end; i++ {
				fmt.Fprintf(&hexPart, "%02X ", data[hashOff+i])
				if c := data[hashOff+i]; c >= 32 && c < 127 {
					asciiPart.WriteByte(c)
				} else {
					asciiPart.WriteByte('.')
				}
			}
			fmt.Printf("  %08X  %-48s %s\n", off, hexPart.String(), asciiPart.String())
		}
		// 全零检查
		allZero := true
		for i := 0; i < hashSize; i++ {
			if data[hashOff+i] != 0 {
				allZero = false
				break
			}
		}
		fmt.Printf("HASH 段是否全零: %v\n", allZero)
		fmt.Printf("HASH 段 / 24 = %.3f  (若接近整数，可能是每文件一项)\n", float64(hashSize)/float64(fileItemSize))
		fmt.Printf("HASH 段 / 文件数 = %.3f\n", float64(hashSize)/float64(fileCount))
	}

	// ---- 文件表尾部原始字段 ----
	fmt.Printf("\n=== 文件表尾部 %d 条（原始字段）===\n", *tail)
	fmt.Printf("%-9s %-10s %-10s %-6s %-10s %-8s %-4s\n", "idx", "nameOff", "pathOff", "chunk", "dataOff", "dataSize", "type")
	start := fileCount - *tail
	if start < 0 {
		start = 0
	}
	for idx := start; idx < fileCount; idx++ {
		off := tableOff + idx*fileItemSize
		fmt.Printf("%-9d %-10d %-10d %-6d %-10d %-8d %-4d\n", idx,
			i32(data[off:off+4]), i32(data[off+4:off+8]), i32(data[off+8:off+12]),
			i32(data[off+12:off+16]), i32(data[off+16:off+20]), i32(data[off+20:off+24]))
	}

	// ---- 用归档 API 看排序 ----
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	files := archive.Files()
	fmt.Printf("\n=== 文件表排序检查（用解析出的路径）===\n")
	if len(files) != fileCount {
		fmt.Printf("注意: archive.Files() 数量 %d != header.fileCount %d\n", len(files), fileCount)
	}

	type row struct {
		idx  int
		dir  string
		name string
		full string
	}
	rows := make([]row, 0, len(files))
	for _, f := range files {
		rows = append(rows, row{idx: f.Index, dir: f.Path, name: f.Name, full: f.ArchivePath})
	}

	check := func(label string, key func(row) string) {
		breaks := 0
		var samples []string
		for i := 1; i < len(rows); i++ {
			if key(rows[i-1]) > key(rows[i]) {
				breaks++
				if len(samples) < *order {
					samples = append(samples, fmt.Sprintf("idx%d %q  >  idx%d %q",
						rows[i-1].idx, trunc(key(rows[i-1])), rows[i].idx, trunc(key(rows[i]))))
				}
			}
		}
		fmt.Printf("%s: 逆序点 %d / %d\n", label, breaks, len(rows)-1)
		for _, s := range samples {
			fmt.Printf("    %s\n", s)
		}
	}
	check("按完整路径(小写)递增", func(r row) string { return strings.ToLower(r.full) })
	check("按目录(小写)递增", func(r row) string { return strings.ToLower(r.dir) })
	check("按文件名(小写)递增", func(r row) string { return strings.ToLower(r.name) })

	// 目录是否成块（同一目录是否连续）
	seen := map[string]bool{}
	blocks, reopens := 0, 0
	prev := ""
	for _, r := range rows {
		if r.dir != prev {
			blocks++
			if seen[r.dir] {
				reopens++
			}
			seen[r.dir] = true
			prev = r.dir
		}
	}
	fmt.Printf("目录块数: %d，其中「目录再次出现」次数: %d\n", blocks, reopens)

	// 末尾新增的目录
	dirCount := map[string]int{}
	for _, r := range rows {
		dirCount[r.dir]++
	}
	var dirs []string
	for d := range dirCount {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	fmt.Printf("目录总数: %d\n", len(dirs))

	fmt.Printf("\n=== 尾部 20 条解析出的路径 ===\n")
	for i := len(rows) - 20; i < len(rows); i++ {
		if i < 0 {
			continue
		}
		fmt.Printf("  idx=%-9d dir=%-28q name=%q\n", rows[i].idx, rows[i].dir, rows[i].name)
	}

	// ---- 指定区间 dump ----
	if *rng != "" {
		var from, to int
		if _, scanErr := fmt.Sscanf(*rng, "%d:%d", &from, &to); scanErr != nil {
			fmt.Printf("\n-range 格式应为 from:to（%v）\n", scanErr)
		} else {
			if from < 0 {
				from = 0
			}
			if to > len(rows) {
				to = len(rows)
			}
			fmt.Printf("\n=== 文件表区间 [%d, %d) ===\n", from, to)
			for i := from; i < to && i < len(rows); i++ {
				fmt.Printf("  idx=%-9d %s\n", rows[i].idx, rows[i].full)
			}
		}
	}
}

func trunc(s string) string {
	if len(s) > 46 {
		return s[:46] + "…"
	}
	return s
}
