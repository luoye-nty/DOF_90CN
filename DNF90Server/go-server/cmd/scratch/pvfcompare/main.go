// Command pvfcompare 独立比对两个 PVF 归档，用于验收另一套（C#）PVF 写回/重打包实现。
//
// 用法：
//
//	go run ./cmd/scratch/pvfcompare -a <pvfA> -b <pvfB> [-limit N] [-max-examples 20] [-quiet]
//
// 退出码：0 = 完全一致；1 = 存在差异；2 = 参数/IO 错误；3 = 因 -limit 只比对了部分内容且未发现差异。
//
// # 为什么这份输出算「独立证据」
//
// 本工具只通过本仓库 internal/platform/pvf 的 Go 读取实现取得事实：
// Open / Format / Snapshot / Files / FileCount / RangePaths / FindFile / FindFileIndex / FileRaw。
// 被验收的写回实现（C#）的字段布局、压缩、比对逻辑一概不参与，也没有被参考，
// 因此这里的结论不依赖被验收方的自述。
//
// # 为什么「解压后原始字节」相等就足以证明内容一致
//
// PVF 的 body 是按 chunk 压缩存储的：同一份文件内容在不同归档里可能落在不同
// chunk、不同偏移，压缩等级、chunk 边界、重打包顺序都会让「.pvf 文件本身的字节」
// 和「压缩流」发生变化。所以容器字节或压缩流都不能作为内容一致性的依据。
// 读取侧还原出的文件原始字节是归档语义上真正承载内容的部分，它与 chunk 划分和
// 压缩方式无关：逐文件字节相等 <=> 内容相等。
// 另外 read.go 的 readRawIndex 按 item.dataSize 精确切出字节并复制返回，
// 所以 len(raw) 就是文件表声明的大小；工具再单独比较 Size 字段，等于交叉校验了一次。
//
// # 性能取向（避免对同一 chunk 反复解密解压）
//
// 逐文件比对按 A 的 File.Index 顺序遍历文件表：同一 chunk 内的文件在表里通常相邻，
// 顺序读能让 chunk LRU 持续命中，而不是每个文件都重新解压一次 chunk。
// A 侧按索引读取（FileRaw(idx)）；B 侧先按路径查一次索引，随后同样按索引读取。
// 结尾打印两侧 chunk 缓存的命中/未命中/淘汰计数，用来佐证「没有反复解压同一 chunk」。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pathA := flag.String("a", "", "PVF A 路径（基准）")
	pathB := flag.String("b", "", "PVF B 路径（待比对）")
	limit := flag.Int("limit", 0, "最多对多少条公共路径做逐文件内容比对（0 = 不限）")
	maxExamples := flag.Int("max-examples", 20, "每类差异最多列出的示例路径数")
	quiet := flag.Bool("quiet", false, "只输出最终汇总行")
	flag.Parse()

	if strings.TrimSpace(*pathA) == "" || strings.TrimSpace(*pathB) == "" {
		fmt.Fprintln(os.Stderr, "用法: pvfcompare -a <pvfA> -b <pvfB> [-limit N] [-max-examples 20] [-quiet]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	if *limit < 0 || *maxExamples < 0 {
		fmt.Fprintln(os.Stderr, "-limit 与 -max-examples 不能为负数")
		os.Exit(2)
	}

	// report 在 -quiet 下全部静默，只留下最后的汇总行。
	report := func(format string, args ...any) {
		if *quiet {
			return
		}
		fmt.Printf(format, args...)
	}

	total := time.Now()

	sideA, openA, err := openSide("A", *pathA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开 A 失败: %v\n", err)
		os.Exit(2)
	}
	sideB, openB, err := openSide("B", *pathB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开 B 失败: %v\n", err)
		os.Exit(2)
	}

	// ---------- 1. 容器级 ----------
	report("=== 1. 容器级 ===\n")
	printSide(sideA, report)
	printSide(sideB, report)

	containerDiff := 0
	if sideA.snapshot.FileCount != sideB.snapshot.FileCount {
		containerDiff++
		report("  !! 文件总数不一致: A=%d B=%d (差 %+d)\n",
			sideA.snapshot.FileCount, sideB.snapshot.FileCount,
			sideB.snapshot.FileCount-sideA.snapshot.FileCount)
	}
	if sideA.snapshot.Format != sideB.snapshot.Format {
		containerDiff++
		report("  !! 归档格式不一致: A=%q B=%q\n", sideA.snapshot.Format, sideB.snapshot.Format)
	}
	if sideA.snapshot.GroupCount != sideB.snapshot.GroupCount {
		// 组数只是容器形态：重打包会重新切 chunk，组数变化本身不代表内容变化。
		report("  -- 组数不同（仅供信息，重打包会改变 chunk 划分，不计入差异判定）: A=%d B=%d\n",
			sideA.snapshot.GroupCount, sideB.snapshot.GroupCount)
	}
	if sideA.snapshot.Checksum == sideB.snapshot.Checksum {
		report("  -- 两个 .pvf 的 SHA-256 完全相同（容器字节级同源）\n")
	}
	if containerDiff == 0 {
		report("  容器级: 文件总数/格式一致\n")
	}

	// 归一化路径键少于文件表条目 => 表里有重复（或仅大小写不同）的路径。
	// 读取层 FindFile/ReadRaw 是「同名取第一条」，这会影响按路径解析 B 侧条目的结果，
	// 所以必须显式提示，而不是悄悄按第一条比对。
	for _, side := range []*side{sideA, sideB} {
		if side.uniquePaths != side.snapshot.FileCount {
			report("  !! %s 的文件表有 %d 条记录但只有 %d 个唯一归一化路径键（存在重复/大小写冲突路径）；按路径查找只会命中第一条\n",
				side.label, side.snapshot.FileCount, side.uniquePaths)
		}
	}

	// ---------- 2. 路径集合 ----------
	pathStart := time.Now()
	aOnly, bOnly, common := diffPaths(sideA, sideB, *maxExamples)
	pathElapsed := time.Since(pathStart)

	report("\n=== 2. 路径集合（按读取层归一化后的小写查找键统计） ===\n")
	report("  A 唯一路径键: %d\n", sideA.uniquePaths)
	report("  B 唯一路径键: %d\n", sideB.uniquePaths)
	report("  共有路径键:   %d\n", common)
	report("  A 有 B 没有:  %d\n", aOnly.count)
	printPathExamples(aOnly, *maxExamples, report)
	report("  B 有 A 没有:  %d\n", bOnly.count)
	printPathExamples(bOnly, *maxExamples, report)
	if bySet := sideB.uniquePaths - bOnly.count; bySet != common {
		report("  !! 交叉校验不一致: 由 B 侧推出的共有数=%d 与 A 侧推出的 %d 不同\n", bySet, common)
	}

	// ---------- 3. 逐文件比对 ----------
	report("\n=== 3. 逐文件比对（DataType / Size / 解压后原始字节） ===\n")
	compareStart := time.Now()
	stats := compareFiles(sideA.archive, sideB.archive, *limit, *maxExamples)
	compareElapsed := time.Since(compareStart)
	stats.common = common

	report("  A 文件表条目:  %d（按 File.Index 顺序遍历）\n", sideA.snapshot.FileCount)
	report("  命中公共路径:  %d\n", stats.common)
	report("  实际比对:      %d（-limit=%d）\n", stats.compared, *limit)
	report("  完全相同:      %d\n", stats.identical)
	report("  dataType 不同: %d\n", stats.dataTypeDiff)
	report("  大小不同:      %d\n", stats.sizeDiff)
	report("  内容不同:      %d\n", stats.contentDiff)
	report("  读取失败:      %d\n", stats.readErrors)
	// 表顺序是「仅重打包」的另一个观察维度：路径配对看不出条目顺序被换过。
	report("  文件表顺序:    A/B 下标完全对应=%t（错位 %d 条，逆序 %d 处；只作信息，不参与差异判定）\n",
		stats.indexMismatch == 0, stats.indexMismatch, stats.orderInversions)
	printExamples("内容不同示例", stats.contentExamples, stats.contentDiff, *maxExamples, report)
	printExamples("dataType 不同示例", stats.dataTypeExamples, stats.dataTypeDiff, *maxExamples, report)
	printExamples("大小不同示例", stats.sizeExamples, stats.sizeDiff, *maxExamples, report)
	if len(stats.errorSamples) > 0 {
		report("  读取失败示例 (最多 %d 条, 共 %d 条):\n", *maxExamples, stats.readErrors)
		for _, item := range stats.errorSamples {
			report("    - %s\n", item)
		}
	}
	// ---------- 4. 读取缓存 ----------
	report("\n=== 4. 读取缓存（佐证 chunk 没有被反复解密解压） ===\n")
	printCache(sideA, report)
	printCache(sideB, report)

	report("\n=== 5. 耗时 ===\n")
	report("  open:     A=%.3fs B=%.3fs\n", openA.Seconds(), openB.Seconds())
	report("  路径集合: %.3fs\n", pathElapsed.Seconds())
	report("  逐文件:   %.3fs\n", compareElapsed.Seconds())
	report("  合计:     %.3fs\n", time.Since(total).Seconds())

	// ---------- 6. 汇总 + 退出码 ----------
	differences := aOnly.count + bOnly.count + stats.dataTypeDiff + stats.sizeDiff +
		stats.contentDiff + stats.readErrors + containerDiff
	truncated := stats.compared < stats.common

	report("\n=== 6. 汇总 ===\n")
	report("  公共路径 %d；完全相同 %d；A 独有 %d；B 独有 %d；dataType 不同 %d；大小不同 %d；内容不同 %d；读取失败 %d；容器级差异 %d\n",
		stats.common, stats.identical, aOnly.count, bOnly.count,
		stats.dataTypeDiff, stats.sizeDiff, stats.contentDiff, stats.readErrors, containerDiff)
	if truncated {
		report("  注意: -limit=%d 只比对了 %d/%d 条公共路径，结论不覆盖全部文件\n",
			*limit, stats.compared, stats.common)
	}

	switch {
	case differences == 0 && !truncated:
		fmt.Printf("IDENTICAL: %d files\n", stats.compared)
	case differences == 0:
		fmt.Printf("PARTIAL: %d of %d files compared (limit), no differences found\n",
			stats.compared, stats.common)
	default:
		fmt.Printf("DIFFERENCES: files_compared=%d identical=%d datatype=%d size=%d content=%d a_only=%d b_only=%d read_errors=%d container=%d\n",
			stats.compared, stats.identical, stats.dataTypeDiff, stats.sizeDiff,
			stats.contentDiff, aOnly.count, bOnly.count, stats.readErrors, containerDiff)
	}
	if !*quiet {
		fmt.Printf("elapsed: %.3fs\n", time.Since(total).Seconds())
	}

	switch {
	case differences == 0 && !truncated:
		os.Exit(0)
	case differences == 0:
		os.Exit(3)
	default:
		os.Exit(1)
	}
}

// side 保存一侧归档的元信息与已打开的归档句柄。
type side struct {
	label       string
	path        string
	archive     *platformpvf.Archive
	snapshot    platformpvf.ArchiveSnapshot
	uniquePaths int
}

func openSide(label, path string) (*side, time.Duration, error) {
	start := time.Now()
	archive, err := platformpvf.Open(path)
	if err != nil {
		return nil, time.Since(start), err
	}
	elapsed := time.Since(start)
	// RangePaths 只遍历读取层已建好的路径索引，不复制文件表、不解析字符串。
	unique := 0
	archive.RangePaths(func(string) bool {
		unique++
		return true
	})
	return &side{
		label:       label,
		path:        path,
		archive:     archive,
		snapshot:    archive.Snapshot(),
		uniquePaths: unique,
	}, elapsed, nil
}

// printSide 打印一侧的容器级信息。输出走 report，因此 -quiet 下会连同段落标题一起静默。
func printSide(s *side, report func(string, ...any)) {
	snap := s.snapshot
	report("[%s] %s\n", s.label, s.path)
	report("    bytes=%d sha256=%s format=%s can_read_file_data=%t\n",
		snap.Size, snap.Checksum, snap.Format, s.archive.CanReadFileData())
	// 读取层只暴露 file_count / group_count；body/hash/name 段的字节数没有公共 API，
	// 这里如实说明，而不是在本工具里重新实现一遍归档解析（那会引入第二套「事实来源」）。
	report("    header: file_count=%d group_count=%d   (body/hash/name 段字节数未在 platform/pvf 公共 API 暴露)\n",
		snap.FileCount, snap.GroupCount)
	report("    unique_path_keys=%d chunk_cache_limit=%d text_cache_limit=%d\n",
		s.uniquePaths, snap.ChunkCacheLimit, snap.TextCacheLimit)
}

func printCache(s *side, report func(string, ...any)) {
	snap := s.archive.Snapshot()
	report("  [%s] cached_chunks=%d cached_chunk_bytes=%d hits=%d misses=%d evictions=%d\n",
		s.label, snap.CachedChunks, snap.CachedChunkBytes, snap.CacheHits, snap.CacheMisses, snap.CacheEvictions)
}

// pathDiff 记录某一侧独有的路径：只保留最多 maxExamples 条示例，总数始终累计，
// 这样即使差集高达百万条也不会把内存吃光。
type pathDiff struct {
	count    int
	examples []string
}

func (d *pathDiff) add(value string) {
	d.count++
	if len(d.examples) < cap(d.examples) {
		d.examples = append(d.examples, value)
	}
}

func newPathDiff(maxExamples int) pathDiff {
	return pathDiff{examples: make([]string, 0, maxExamples)}
}

func printPathExamples(diff pathDiff, maxExamples int, report func(string, ...any)) {
	for _, item := range diff.examples {
		report("    - %s\n", item)
	}
	if diff.count > len(diff.examples) {
		report("    ... 其余 %d 条未列出（-max-examples=%d）\n", diff.count-len(diff.examples), maxExamples)
	}
}

// diffPaths 统计两侧路径集合的差集。
// A 独有的判定方式是「在 B 的路径索引里查不到」，直接用读取层的 FindFileIndex，
// 不自己复制一份路径表（106 万条路径的额外 map 会白白多占几百 MB）。
// 示例路径按字典序排序，保证同一份输入每次输出一致，便于脚本化比对。
func diffPaths(sideA, sideB *side, maxExamples int) (pathDiff, pathDiff, int) {
	aOnly := newPathDiff(maxExamples)
	bOnly := newPathDiff(maxExamples)
	sideA.archive.RangePaths(func(key string) bool {
		if sideB.archive.FindFileIndex(key) < 0 {
			aOnly.add(displayPath(sideA.archive, key))
		}
		return true
	})
	sideB.archive.RangePaths(func(key string) bool {
		if sideA.archive.FindFileIndex(key) < 0 {
			bOnly.add(displayPath(sideB.archive, key))
		}
		return true
	})
	sort.Strings(aOnly.examples)
	sort.Strings(bOnly.examples)

	// 共有数由 A 的唯一键数减去 A 独有数得到；调用方再用 B 侧做一次交叉校验。
	return aOnly, bOnly, sideA.uniquePaths - aOnly.count
}

// displayPath 把归一化小写键换回归档里记录的原始大小写路径，仅对要打印的少数示例调用。
func displayPath(archive *platformpvf.Archive, key string) string {
	if file, ok := archive.FindFile(key); ok && file.ArchivePath != "" {
		return file.ArchivePath
	}
	return key
}

// fileExample 是一条逐文件差异的示例记录。
type fileExample struct {
	path  string
	aType int
	bType int
	aSize int
	bSize int
	note  string // 内容差异的补充说明（首个不同字节偏移 / 长度不同）
}

// compareStats 是逐文件比对的分类计数。
// 各类计数互相独立：同一个文件可以同时命中 dataType 不同和内容不同，
// 这样每类数字都如实反映问题，而不是互相掩盖。
type compareStats struct {
	common           int
	compared         int
	identical        int
	dataTypeDiff     int
	sizeDiff         int
	contentDiff      int
	readErrors       int
	contentExamples  []fileExample
	dataTypeExamples []fileExample
	sizeExamples     []fileExample
	errorSamples     []string

	// 文件表顺序信息（只作信息，不参与差异判定）：
	// 上面的逐文件比对是按路径配对的，看不出重打包有没有把表项顺序换了。
	// 顺着 A 的 Index 顺序走时，若同名条目的 B 侧下标一路递增，说明 B 的表顺序与 A 一致。
	indexMismatch   int
	orderInversions int
	prevBIndex      int
	hasPrevB        bool
}

// compareFiles 按 A 的文件表顺序逐条比对共有路径。
// limit > 0 时读满 limit 条就停止（公共路径总数由 diffPaths 单独统计），便于快速抽样。
func compareFiles(a, b *platformpvf.Archive, limit, maxExamples int) compareStats {
	var stats compareStats
	for _, fileA := range a.Files() {
		// 只比对共有路径；A 独有的路径已经在路径集合一节的差集里统计过了。
		fileB, ok := b.FindFile(fileA.ArchivePath)
		if !ok {
			continue
		}
		stats.common++
		if stats.hasPrevB && fileB.Index < stats.prevBIndex {
			stats.orderInversions++
		}
		stats.hasPrevB = true
		stats.prevBIndex = fileB.Index
		if fileB.Index != fileA.Index {
			stats.indexMismatch++
		}
		if limit > 0 && stats.compared >= limit {
			break
		}
		stats.compared++

		// 两侧都按文件表索引读取：FileRaw 返回的就是该条目的解压后原始字节副本，
		// 与 chunk 划分、偏移、压缩方式无关，因此它才是内容一致性的证据。
		rawA, errA := a.FileRaw(fileA.Index)
		if errA != nil {
			stats.readErrors++
			stats.errorSamples = appendCappedString(stats.errorSamples,
				fmt.Sprintf("%s: A: %v", fileA.ArchivePath, errA), maxExamples)
			continue
		}
		rawB, errB := b.FileRaw(fileB.Index)
		if errB != nil {
			stats.readErrors++
			stats.errorSamples = appendCappedString(stats.errorSamples,
				fmt.Sprintf("%s: B: %v", fileA.ArchivePath, errB), maxExamples)
			continue
		}

		base := fileExample{
			path:  fileA.ArchivePath,
			aType: fileA.DataType, bType: fileB.DataType,
			aSize: fileA.Size, bSize: fileB.Size,
		}
		same := true
		if fileA.DataType != fileB.DataType {
			same = false
			stats.dataTypeDiff++
			stats.dataTypeExamples = appendCapped(stats.dataTypeExamples, base, maxExamples)
		}
		// Size 既比文件表声明的 dataSize，也比实际取回的字节长度：
		// 两者本应相等（readRawIndex 按 dataSize 切片），不等就说明读取层与表不一致。
		if fileA.Size != fileB.Size || len(rawA) != len(rawB) {
			same = false
			stats.sizeDiff++
			stats.sizeExamples = appendCapped(stats.sizeExamples, base, maxExamples)
		}
		if !bytes.Equal(rawA, rawB) {
			same = false
			stats.contentDiff++
			content := base
			if len(rawA) == len(rawB) {
				content.note = fmt.Sprintf("首差偏移=%d", firstDiffOffset(rawA, rawB))
			} else {
				content.note = fmt.Sprintf("长度不同 A=%d B=%d 首差偏移=%d",
					len(rawA), len(rawB), firstDiffOffset(rawA, rawB))
			}
			stats.contentExamples = appendCapped(stats.contentExamples, content, maxExamples)
		}
		if same {
			stats.identical++
		}
	}
	return stats
}

func appendCapped(list []fileExample, item fileExample, maxExamples int) []fileExample {
	if len(list) >= maxExamples {
		return list
	}
	return append(list, item)
}

func appendCappedString(list []string, item string, maxExamples int) []string {
	if len(list) >= maxExamples {
		return list
	}
	return append(list, item)
}

// firstDiffOffset 返回两个字节串第一个不同的下标；前缀相同时返回公共长度。
func firstDiffOffset(left, right []byte) int {
	size := len(left)
	if len(right) < size {
		size = len(right)
	}
	for index := 0; index < size; index++ {
		if left[index] != right[index] {
			return index
		}
	}
	return size
}

func printExamples(title string, examples []fileExample, total, maxExamples int, report func(string, ...any)) {
	if total == 0 {
		return
	}
	report("  %s (最多 %d 条, 共 %d 条):\n", title, maxExamples, total)
	for _, item := range examples {
		note := item.note
		if note != "" {
			note = " " + note
		}
		report("    - %s  A(type=%d size=%d) B(type=%d size=%d)%s\n",
			item.path, item.aType, item.aSize, item.bType, item.bSize, note)
	}
}
