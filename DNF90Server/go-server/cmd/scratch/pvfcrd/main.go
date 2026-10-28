// pvfcrd —— protected PVF 的「增 / 删」文件操作，并在**同一次写盘里**重建 HASH 段，
// 让包始终保持自洽。只做元数据级改动，body 里的数据字节一律不重压缩。
//
// 为什么需要它：第三方 pvfUtility 增删文件后会留下陈旧的 HASH 段（见
// tools\_pvf_20260919\根因-HASH段未同步_20260921.md），客户端就查不到新条目。
// 这个工具把「改文件表」和「重建 HASH」绑成一次原子操作，从根上避免失同步。
//
// 用法:
//
//	# 删（纯元数据删除：条目从文件表与 HASH 移除，数据字节留在原 chunk 成为孤儿）
//	go run ./cmd/scratch/pvfcrd -pvf <in> -out <out> -mode delete -path stackable/10000001/_x.stk
//
//	# 增（把已有文件的原始字节与 dataType 原样复制成新路径，最稳的一条路）
//	go run ./cmd/scratch/pvfcrd -pvf <in> -out <out> -mode add \
//	    -src stackable/10000001/10000219.stk -dst stackable/10000001/ab_B.stk
//
//	# 增 + 同时在 .lst 注册新 id（路径相对 .lst 所在目录）
//	go run ./cmd/scratch/pvfcrd -pvf <in> -out <out> -mode add \
//	    -src stackable/10000001/10000219.stk -dst stackable/abtest/ab_A.stk \
//	    -list stackable/stackable.lst -list-id 202600901 -list-rel abtest/ab_A.stk
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf16"

	platformpvf "longheng.io/server/internal/platform/pvf"
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

func i32(b []byte) int   { return int(int32(binary.LittleEndian.Uint32(b))) }
func put(b []byte, v int) { binary.LittleEndian.PutUint32(b, uint32(int32(v))) }

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

// row 是文件表一行的 6 个 int32。
type row [6]int

func parseTable(data []byte, s section) []row {
	out := make([]row, s.fileCount)
	for i := 0; i < s.fileCount; i++ {
		off := s.tableOff + i*fileItemSize
		for k := 0; k < 6; k++ {
			out[i][k] = i32(data[off+k*4 : off+k*4+4])
		}
	}
	return out
}

func encodeTable(rows []row) []byte {
	out := make([]byte, len(rows)*fileItemSize)
	for i, r := range rows {
		for k := 0; k < 6; k++ {
			put(out[i*fileItemSize+k*4:i*fileItemSize+k*4+4], r[k])
		}
	}
	return out
}

// rebuildHash 由文件表重算 HASH 段密文；keepExtras 保留旧字典里不再被引用的陈旧项。
func rebuildHash(data []byte, s section, rows []row, resolve func(int) string, keepExtras bool) []byte {
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
	for _, r := range rows {
		consider(r[0])
		consider(r[1])
	}
	if keepExtras && s.hashSize > 0 && s.hashOff+s.hashSize <= len(data) {
		old := append([]byte(nil), data[s.hashOff:s.hashOff+s.hashSize]...)
		xorCore("hash", old, containerMagic)
		intCount := len(old) / 4
		if intCount >= 2 {
			mc := i32(old[0:4])
			if mc >= 0 && 1+mc*2+1 <= intCount {
				dp := 1 + mc*2
				sc := i32(old[dp*4 : dp*4+4])
				if sc >= 0 && dp+1+sc <= intCount {
					for i := 0; i < sc; i++ {
						consider(i32(old[(dp+1+i)*4 : (dp+1+i)*4+4]))
					}
				}
			}
		}
	}
	ordered := make([]kv, 0, len(byOff))
	for off, text := range byOff {
		ordered = append(ordered, kv{off, text})
	}
	sort.Slice(ordered, func(a, b int) bool {
		if ordered[a].text != ordered[b].text {
			return ordered[a].text < ordered[b].text
		}
		return ordered[a].off < ordered[b].off
	})
	out := make([]byte, (1+len(rows)*2+1+len(ordered))*4)
	put(out[0:4], len(rows))
	for i, r := range rows {
		put(out[(1+i*2)*4:(1+i*2)*4+4], r[0])
		put(out[(2+i*2)*4:(2+i*2)*4+4], r[1])
	}
	dp := 1 + len(rows)*2
	put(out[dp*4:dp*4+4], len(ordered))
	for i, e := range ordered {
		put(out[(dp+1+i)*4:(dp+1+i)*4+4], e.off)
	}
	xorCore("hash", out, containerMagic)
	return out
}

// assemble 把 header（改过 fileCount/hashSize）+ 新文件表 + 新 HASH + 其余段原样 拼起来。
func assemble(data []byte, s section, hdr []byte, rows []row, hash []byte) []byte {
	nh := append([]byte(nil), hdr...)
	put(nh[24:28], len(rows))
	put(nh[40:44], len(hash))
	xorCore("hEAd", nh, containerMagic)

	out := make([]byte, 0, len(data)+len(hash)-s.hashSize)
	out = append(out, nh...)
	out = append(out, encodeTable(rows)...)
	out = append(out, hash...)
	out = append(out, data[s.nameOff:s.groupOff]...)
	out = append(out, data[s.groupOff:s.bodyOff]...)
	out = append(out, data[s.bodyOff:s.bodyOff+s.bodySize]...)
	return out
}

func main() {
	pvfPath := flag.String("pvf", "", "输入 Script.pvf")
	outPath := flag.String("out", "", "输出 Script.pvf")
	mode := flag.String("mode", "", "add | delete")
	delPath := flag.String("path", "", "delete: 要删除的归档路径")
	src := flag.String("src", "", "add: 源文件（内容与 dataType 原样复制）")
	dst := flag.String("dst", "", "add: 新路径")
	listPath := flag.String("list", "", "add: 要追加条目的 .lst（可空）")
	listID := flag.Int("list-id", 0, "add: .lst 里的 id")
	listRel := flag.String("list-rel", "", "add: .lst 里的相对路径")
	flag.Parse()

	if *pvfPath == "" || *outPath == "" || *mode == "" {
		fmt.Fprintln(os.Stderr, "-pvf -out -mode are required")
		os.Exit(2)
	}
	orig, err := os.ReadFile(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	switch *mode {
	case "delete":
		runDelete(orig, *pvfPath, *outPath, *delPath)
	case "add":
		runAdd(orig, *pvfPath, *outPath, *src, *dst, *listPath, *listID, *listRel)
	default:
		fmt.Fprintln(os.Stderr, "未知模式:", *mode)
		os.Exit(2)
	}
}

func runDelete(orig []byte, pvfPath, outPath, delPath string) {
	if delPath == "" {
		fmt.Fprintln(os.Stderr, "delete 需要 -path")
		os.Exit(2)
	}
	s, hdr, err := locate(orig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "locate:", err)
		os.Exit(1)
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	idx := archive.FindFileIndex(delPath)
	if idx < 0 {
		fmt.Fprintf(os.Stderr, "归档里没有 %s\n", delPath)
		os.Exit(1)
	}
	rows := parseTable(orig, s)
	fmt.Printf("删除: %s (idx=%d)  文件表 %d -> %d\n", delPath, idx, len(rows), len(rows)-1)
	rows = append(rows[:idx:idx], rows[idx+1:]...)

	hash := rebuildHash(orig, s, rows, archive.ResolveString, true)
	out := assemble(orig, s, hdr, rows, hash)
	write2(outPath, out)
	selfCheck(out, pvfPath, delPath, len(rows), true)
}

func runAdd(orig []byte, pvfPath, outPath, src, dst, listPath string, listID int, listRel string) {
	if src == "" || dst == "" {
		fmt.Fprintln(os.Stderr, "add 需要 -src 与 -dst")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	if archive.FindFileIndex(dst) >= 0 {
		fmt.Fprintf(os.Stderr, "目标路径已存在，拒绝覆盖: %s\n", dst)
		os.Exit(1)
	}
	srcIdx := archive.FindFileIndex(src)
	if srcIdx < 0 {
		fmt.Fprintf(os.Stderr, "源文件不存在: %s\n", src)
		os.Exit(1)
	}
	srcFiles := archive.Files()
	srcEntry := srcFiles[srcIdx]
	fmt.Printf("源: %s (idx=%d type=%d size=%d)\n", src, srcIdx, srcEntry.DataType, srcEntry.Size)
	fmt.Printf("目标: %s\n", dst)

	// 1) 先把新路径需要的两个字符串追加进池（只追加，旧偏移全部保持有效）。
	dstDir := path.Dir(strings.ReplaceAll(dst, "\\", "/"))
	dstName := path.Base(strings.ReplaceAll(dst, "\\", "/"))
	if dstDir == "." {
		dstDir = ""
	}
	appends := []platformpvf.StringAppend{{Value: dstName, Wide: false}}
	if dstDir != "" {
		appends = append([]platformpvf.StringAppend{{Value: dstDir, Wide: false}}, appends...)
	}
	step1, err := archive.RepackAppendStrings(appends)
	if err != nil {
		fmt.Fprintln(os.Stderr, "RepackAppendStrings:", err)
		os.Exit(1)
	}
	a1, err := platformpvf.OpenBytes(step1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reopen(1):", err)
		os.Exit(1)
	}
	offs := a1.FindStringMagicOffsets(appends2values(appends))
	nameOff, ok1 := offs[dstName]
	pathOff, ok2 := offs[dstDir]
	if !ok1 {
		fmt.Fprintln(os.Stderr, "找不到新文件名偏移")
		os.Exit(1)
	}
	if dstDir != "" && !ok2 {
		fmt.Fprintln(os.Stderr, "找不到新目录偏移")
		os.Exit(1)
	}
	if dstDir == "" {
		pathOff = -1
	}

	// 2) 在文件表尾部追加一行，数据直接**别名**源文件那一份字节（不重压缩）。
	s1, hdr1, err := locate(step1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "locate(1):", err)
		os.Exit(1)
	}
	rows := parseTable(step1, s1)
	srcRow := rows[srcIdx]
	newRow := row{nameOff, pathOff, srcRow[2], srcRow[3], srcRow[4], srcRow[5]}
	rows = append(rows, newRow)
	fmt.Printf("文件表 %d -> %d（新条目别名源数据: chunk=%d off=%d size=%d type=%d）\n",
		len(rows)-1, len(rows), newRow[2], newRow[3], newRow[4], newRow[5])

	hash := rebuildHash(step1, s1, rows, a1.ResolveString, true)
	step2 := assemble(step1, s1, hdr1, rows, hash)

	// 3) 可选：往 .lst 追加一条 id + 反引号路径（令牌流：0x00+int32 / 0x06+int32 偏移）。
	final := step2
	if listPath != "" {
		final = appendListEntry(step2, listPath, listID, listRel)
	}

	write2(outPath, final)
	selfCheck(final, pvfPath, dst, len(rows), false)
}

func appends2values(a []platformpvf.StringAppend) []string {
	out := make([]string, 0, len(a))
	for _, x := range a {
		out = append(out, x.Value)
	}
	return out
}

func appendListEntry(in []byte, listPath string, id int, rel string) []byte {
	if rel == "" {
		fmt.Fprintln(os.Stderr, "给了 -list 就必须给 -list-rel")
		os.Exit(2)
	}
	a, err := platformpvf.OpenBytes(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open(list):", err)
		os.Exit(1)
	}
	old, err := a.ReadRaw(listPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读不到 %s: %v\n", listPath, err)
		os.Exit(1)
	}
	// 字符串用反斜杠还是斜杠看现有习惯，这里照抄 .lst 里的写法。
	appended, err := a.RepackAppendStrings([]platformpvf.StringAppend{{Value: rel, Wide: false}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "append lst string:", err)
		os.Exit(1)
	}
	a2, err := platformpvf.OpenBytes(appended)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reopen(list):", err)
		os.Exit(1)
	}
	off, ok := a2.FindStringMagicOffsets([]string{rel})[rel]
	if !ok {
		fmt.Fprintln(os.Stderr, "找不到 lst 路径字符串偏移")
		os.Exit(1)
	}
	// 覆盖 .lst 之前，必须先按 a2 的视图重新取一次旧字节（池变大后 body 位置变了）。
	old2, err := a2.ReadRaw(listPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "重读 lst:", err)
		os.Exit(1)
	}
	_ = old
	tok := make([]byte, 0, 20)
	t := make([]byte, 5)
	t[0] = 0x00
	put(t[1:5], id)
	tok = append(tok, t...)
	t2 := make([]byte, 5)
	t2[0] = 0x06
	put(t2[1:5], off)
	tok = append(tok, t2...)
	next, err := a2.RepackRaw([]platformpvf.RawReplacement{{Path: listPath, Data: append(append([]byte(nil), old2...), tok...)}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "RepackRaw(lst):", err)
		os.Exit(1)
	}
	fmt.Printf("已在 %s 追加: %d `%s`\n", listPath, id, rel)
	return next
}

func write2(outPath string, out []byte) {
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("已写出: %s (%d 字节)\n", outPath, len(out))
}

func selfCheck(out []byte, origPath, probe string, wantCount int, wantAbsent bool) {
	a, err := platformpvf.OpenBytes(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "自检 open:", err)
		os.Exit(1)
	}
	fmt.Printf("自检: 文件数 = %d (期望 %d) %s\n", a.FileCount(), wantCount,
		map[bool]string{true: "OK", false: "FAIL"}[a.FileCount() == wantCount])
	if a.FileCount() != wantCount {
		os.Exit(1)
	}
	found := a.FindFileIndex(probe) >= 0
	if wantAbsent && found {
		fmt.Fprintf(os.Stderr, "自检 FAIL: %s 仍在\n", probe)
		os.Exit(1)
	}
	if !wantAbsent && !found {
		fmt.Fprintf(os.Stderr, "自检 FAIL: %s 不在\n", probe)
		os.Exit(1)
	}
	if !wantAbsent {
		if t, err := a.ReadText(probe); err == nil {
			prev := strings.Join(strings.Fields(t), " ")
			if len(prev) > 120 {
				prev = prev[:120] + "…"
			}
			fmt.Printf("自检: %s 可读 -> %s\n", probe, prev)
		} else {
			fmt.Fprintf(os.Stderr, "自检 FAIL: 读 %s: %v\n", probe, err)
			os.Exit(1)
		}
	}
	fmt.Println("自检通过。内容一致性与 HASH 自洽性请另跑 pvfcompare / pvfhashcheck。")
}
