// Command logtool 是 dnfbridge 加密日志的运维侧工具（运营本机使用）。
//
// 服务端自 2026-09-30 起**所有日志落盘即密文**，采用公钥信封：服务端只带收件人**公钥**，
// 私钥只在你本机。因此排障流程是：把日志目录拉回本机 → 用 logtool 解密检索。
//
// 生成密钥对（只做一次；私钥绝不放上服务器、绝不进分发包）：
//
//	logtool keygen -o D:\dnf-keys
//	  → D:\dnf-keys\log-public.key   随服务端包分发
//	  → D:\dnf-keys\log-private.key  只留本机
//
// 读日志（把服务器上的 runtime\logs 整个拉下来即可，工具会递归找全部密文分片）：
//
//	logtool list   -dir logs -key D:\dnf-keys\log-private.key
//	logtool verify -dir logs -key ...
//	logtool tail   -n 200 -dir logs -key ...
//	logtool grep   "monster-death-blocked" -dir logs -key ...        # 内存解密，不落明文
//	logtool grep   -i "conflicting" -n -dir logs -key ...
//	logtool decrypt <分片> -o out -key ...                          # 会落明文，用完请删
//
// 服务端里的公钥是编译期固化的（服务端使用方无感，不需要任何密钥文件），
// 换机器/换版本前先确认手上私钥配对成功：
//
//	logtool keycheck -key D:\dnf-keys\log-private.key
//	logtool keycheck -key ... -dir logs        # 顺便列出历史分片各由哪把钥匙加密
//
// 私钥来源：-key 参数 > DNFBRIDGE_LOG_PRIVATE_KEY_FILE 环境变量 > keys/log-private.key。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"longheng.io/server/internal/platform/logcrypt"
	"longheng.io/server/internal/platform/logkey"
)

const usage = `logtool - read dnfbridge encrypted logs (public-key envelope)

usage:
  logtool keygen   -o DIR [--force]
  logtool keycheck [-key PRIVATE_KEY] [-dir DIR|FILE]
  logtool list     [-dir DIR|FILE] [-key PRIVATE_KEY]
  logtool verify   [-dir DIR|FILE] [-key PRIVATE_KEY]
  logtool tail     [-n N] [-dir DIR|FILE] [-key PRIVATE_KEY]
  logtool grep     [-i] [-n] [-dir DIR|FILE] [-key PRIVATE_KEY] PATTERN
  logtool decrypt  [-o OUTDIR] [-key PRIVATE_KEY] FILE...

notes:
  * -dir 可以是目录（递归扫描全部 *.enc 分片）或单个分片文件。
  * 选项写在被检索模式前后都可以：grep "x" -dir logs 与 grep -dir logs "x" 等价。
    模式本身以 - 开头时用 -- 分隔，例如 grep -- "-dir" -dir logs。
  * grep / tail / verify 全部在内存中解密，不向磁盘写任何明文。
  * decrypt 会写出明文文件，仅在你确实需要拿文本工具二次处理时使用。
  * keycheck 用来确认"手上私钥"与"服务端版本里固化的公钥"配对，避免拉回日志才发现解不开。
  * 私钥丢失 = 全部历史日志永久不可读；请单独备份，且不要放进分发包。
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	command := os.Args[1]
	args := os.Args[2:]
	switch command {
	case "keygen":
		os.Exit(runKeygen(args))
	case "keycheck":
		os.Exit(runKeycheck(args))
	case "list":
		os.Exit(runList(args))
	case "tail":
		os.Exit(runTail(args))
	case "grep":
		os.Exit(runGrep(args))
	case "verify":
		os.Exit(runVerify(args))
	case "decrypt":
		os.Exit(runDecrypt(args))
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", command, usage)
		os.Exit(2)
	}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parseInterspersed 解析允许"位置参数在前、选项在后"的命令行。
// 标准库 flag 遇到第一个非选项参数就停止解析，而 `logtool grep PATTERN -dir logs`
// 这种写法（也是本工具文档里的示例）会让 -dir/-key 被当成模式的一部分，
// 于是悄悄退回默认路径。这里先把选项及其取值整体挪到前面再交给 flag 包。
// valueFlags 必须登记所有"要吞掉下一个 token"的选项；布尔选项不需要登记。
func parseInterspersed(fs *flag.FlagSet, args []string, valueFlags ...string) error {
	takesValue := make(map[string]bool, len(valueFlags))
	for _, name := range valueFlags {
		takesValue[name] = true
	}
	return fs.Parse(permuteFlags(args, takesValue))
}

func permuteFlags(args []string, takesValue map[string]bool) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	separated := false // 原命令行是否出现过 --：出现过就必须把分隔符一起搬到新位置
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" { // 显式分隔符：其后一律视为位置参数（模式本身以 - 开头时用）
			positional = append(positional, args[i+1:]...)
			separated = true
			break
		}
		if !isFlagToken(arg) {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		hasInlineValue := strings.ContainsRune(arg, '=')
		if takesValue[flagTokenName(arg)] && !hasInlineValue && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if separated {
		// 分隔符必须留着：否则搬到后面的 "-dir" 这类位置参数会被 flag 包重新当成选项。
		flags = append(flags, "--")
	}
	return append(flags, positional...)
}

func isFlagToken(arg string) bool {
	return len(arg) > 1 && arg[0] == '-' && arg != "-"
}

func flagTokenName(arg string) string {
	name := strings.TrimLeft(arg, "-")
	if idx := strings.IndexByte(name, '='); idx >= 0 {
		name = name[:idx]
	}
	return name
}

// runKeygen 生成密钥对。这是**唯一**生成密钥的入口：服务端从不自动生成。
func runKeygen(args []string) int {
	fs := newFlagSet("keygen")
	out := fs.String("o", "", "output directory (required)")
	force := fs.Bool("force", false, "overwrite existing key files")
	if err := parseInterspersed(fs, args, "o"); err != nil {
		return 2
	}
	if strings.TrimSpace(*out) == "" {
		fmt.Fprintln(os.Stderr, "keygen requires -o DIR")
		return 2
	}
	publicPath, privatePath, keyID, err := logcrypt.WriteKeyPair(*out, *force)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("key id        : %s\n", keyID)
	fmt.Printf("public key    : %s   <- 随服务端包分发（只放公钥）\n", publicPath)
	fmt.Printf("private key   : %s   <- 只留本机，切勿放进分发包\n", privatePath)
	fmt.Println()
	fmt.Println("服务端放置：把公钥放到 <runtime>\\keys\\log-public.key（或用 DNFBRIDGE_LOG_PUBLIC_KEY_FILE 指定）。")
	fmt.Println("你自己的服务器如需 GM 记录查询本机回读，可另放私钥；分发包里绝不放。")
	return 0
}

// resolveKey 依次尝试 -key、DNFBRIDGE_LOG_PRIVATE_KEY_FILE、默认相对路径。
func resolveKey(explicit string) ([]byte, error) {
	key, _, err := resolveKeyPath(explicit)
	return key, err
}

// resolveKeyPath 与 resolveKey 等价，另外回报命中的私钥路径，供 keycheck 显示。
func resolveKeyPath(explicit string) ([]byte, string, error) {
	candidates := keyCandidates(explicit)
	var lastErr error
	for _, path := range candidates {
		key, err := logcrypt.LoadPrivateKey(path)
		if err == nil {
			return key, path, nil
		}
		lastErr = fmt.Errorf("%s: %w", path, err)
	}
	return nil, "", fmt.Errorf("no usable private key (tried %s): %w", strings.Join(candidates, ", "), lastErr)
}

func keyCandidates(explicit string) []string {
	candidates := make([]string, 0, 3)
	if strings.TrimSpace(explicit) != "" {
		candidates = append(candidates, explicit)
	}
	if fromEnv := strings.TrimSpace(os.Getenv("DNFBRIDGE_LOG_PRIVATE_KEY_FILE")); fromEnv != "" {
		candidates = append(candidates, fromEnv)
	}
	return append(candidates, filepath.Join("keys", logcrypt.PrivateKeyFileName))
}

// keyHeaderProbeBytes 是探测分片 keyID 时读取的文件头长度：容器头不到 128 字节，
// 读 256 字节足够，不必把上百 MB 的分片整个读进内存。
const keyHeaderProbeBytes = 256

// runKeycheck 回答运维最怕踩的两个问题：
//  1. 我手上这把私钥，和这个服务端版本里固化的公钥是同一对吗？（不是的话，日志拉回来也解不开）
//  2. 手上这堆历史分片，分别是被哪把钥匙加密的？（轮换过密钥时一眼看出来）
func runKeycheck(args []string) int {
	fs := newFlagSet("keycheck")
	dir := fs.String("dir", "", "optional: log directory, to list which key encrypted the history")
	keyFile := fs.String("key", "", "private key file")
	if err := parseInterspersed(fs, args, "dir", "key"); err != nil {
		return 2
	}
	recipient, err := logkey.Recipient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "baked-in public key is unusable: %v\n", err)
		return 1
	}
	bakedKeyID := recipient.KeyID()
	fmt.Printf("服务端固化公钥 : %s\n", logkey.PublicKeyBase64)
	fmt.Printf("固化 keyID     : %s\n", bakedKeyID)

	key, keyPath, keyErr := resolveKeyPath(*keyFile)
	if keyErr != nil {
		fmt.Fprintln(os.Stderr, keyErr)
		return 1
	}
	keyID, idErr := logcrypt.PrivateKeyKeyID(key)
	if idErr != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", keyPath, idErr)
		return 1
	}
	fmt.Printf("私钥           : %s\n", keyPath)
	fmt.Printf("私钥 keyID     : %s\n", keyID)
	status := 0
	if keyID == bakedKeyID {
		fmt.Println("配对结果       : MATCH —— 这把私钥能解开本版本服务端写出的全部日志。")
	} else {
		fmt.Println("配对结果       : MISMATCH —— 不是同一对密钥，日志拉回来也解不开。")
		status = 1
	}

	if strings.TrimSpace(*dir) == "" {
		return status
	}
	target, targetErr := resolveTarget(*dir)
	if targetErr != nil {
		fmt.Fprintln(os.Stderr, targetErr)
		return 1
	}
	files, listErr := collectContainers(target)
	if listErr != nil {
		fmt.Fprintln(os.Stderr, listErr)
		return 1
	}
	perKey := map[string]int{}
	other := 0
	for _, file := range files {
		head, readErr := readFileHead(file, keyHeaderProbeBytes)
		if readErr != nil {
			other++
			continue
		}
		id, idErr := logcrypt.ContainerKeyID(head)
		if idErr != nil {
			other++
			continue
		}
		perKey[id]++
	}
	fmt.Printf("\n历史分片       : %d 个（%s）\n", len(files), target)
	if len(perKey) == 0 && other == 0 {
		fmt.Println("    目录下没有 *.enc 分片")
	}
	for _, id := range sortedKeys(perKey) {
		note := "  <= 与本版本固化公钥不同，需要那一版对应的私钥"
		if id == bakedKeyID {
			note = "  <= 本版本固化公钥"
		}
		fmt.Printf("    key-id=%s  %5d 个分片%s\n", id, perKey[id], note)
	}
	if other > 0 {
		fmt.Printf("    （另有 %d 个文件读不到或不是 logcrypt 容器）\n", other)
	}
	return status
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// readFileHead 只读文件开头一段，用于探测容器头。
func readFileHead(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	buf := make([]byte, limit)
	n, readErr := io.ReadFull(file, buf)
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		return nil, readErr
	}
	return buf[:n], nil
}

// collectContainers 收集待处理的密文分片：目标可以是文件或目录（目录递归）。
func collectContainers(target string) ([]string, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return nil, fmt.Errorf("no log directory given (use -dir)")
	}
	info, err := os.Stat(trimmed)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{trimmed}, nil
	}
	var out []string
	walkErr := filepath.WalkDir(trimmed, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个文件不可读不该中断整次扫描
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".enc") {
			out = append(out, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	// 目录名是 YYYYMMDD，字典序即时间序；同目录内再按文件名排序。
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("no encrypted log containers under %s", trimmed)
	}
	return out, nil
}

func resolveTarget(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	fallback := filepath.Join("logs", "packet")
	if _, err := os.Stat(fallback); err == nil {
		return fallback, nil
	}
	if _, err := os.Stat("logs"); err == nil {
		return "logs", nil
	}
	return "", fmt.Errorf("cannot locate the encrypted log directory; pass -dir")
}

// eachPlainLine 逐个分片解密并按行回调。一次只持有一个分片，避免把整棵日志树读进内存。
func eachPlainLine(target string, key []byte, fn func(file string, line string) bool) error {
	files, err := collectContainers(target)
	if err != nil {
		return err
	}
	for _, file := range files {
		raw, readErr := os.ReadFile(file)
		if readErr != nil {
			return readErr
		}
		plain, _, truncated, decErr := logcrypt.DecryptAll(key, raw)
		if decErr != nil {
			return fmt.Errorf("%s: %w", file, decErr)
		}
		if truncated {
			fmt.Fprintf(os.Stderr, "warning: %s has a truncated trailing record; earlier records are intact\n", filepath.Base(file))
		}
		scanner := bufio.NewScanner(strings.NewReader(string(plain)))
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			if !fn(file, scanner.Text()) {
				return nil
			}
		}
	}
	return nil
}

func runList(args []string) int {
	fs := newFlagSet("list")
	dir := fs.String("dir", "", "log directory or single container")
	keyFile := fs.String("key", "", "private key file")
	if err := parseInterspersed(fs, args, "dir", "key"); err != nil {
		return 2
	}
	if _, err := resolveKey(*keyFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	target, err := resolveTarget(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	files, err := collectContainers(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// 按通道分组显示，便于确认"每一类都被加密了"。
	groups := map[string][]string{}
	var total int64
	for _, file := range files {
		relative, relErr := filepath.Rel(target, file)
		if relErr != nil {
			relative = file
		}
		group := strings.Split(filepath.ToSlash(relative), "/")[0]
		if group == filepath.Base(relative) {
			group = "(root)"
		}
		groups[group] = append(groups[group], file)
		if info, statErr := os.Stat(file); statErr == nil {
			total += info.Size()
		}
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var groupBytes int64
		for _, file := range groups[name] {
			if info, statErr := os.Stat(file); statErr == nil {
				groupBytes += info.Size()
			}
		}
		fmt.Printf("%-12s %3d file(s)  %9.2f MB\n", name, len(groups[name]), float64(groupBytes)/(1<<20))
		for _, file := range groups[name] {
			info, _ := os.Stat(file)
			size := int64(0)
			if info != nil {
				size = info.Size()
			}
			fmt.Printf("    %-56s %8.2f MB\n", filepath.Base(filepath.Dir(file))+"/"+filepath.Base(file), float64(size)/(1<<20))
		}
	}
	fmt.Printf("\ntotal %d container(s), %.2f MB\n", len(files), float64(total)/(1<<20))
	return 0
}

func runTail(args []string) int {
	fs := newFlagSet("tail")
	dir := fs.String("dir", "", "log directory or single container")
	keyFile := fs.String("key", "", "private key file")
	count := fs.Int("n", 100, "number of lines")
	if err := parseInterspersed(fs, args, "dir", "key", "n"); err != nil {
		return 2
	}
	key, err := resolveKey(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	target, err := resolveTarget(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ring := make([]string, 0, *count)
	if err := eachPlainLine(target, key, func(_ string, line string) bool {
		if len(ring) == *count {
			copy(ring, ring[1:])
			ring[len(ring)-1] = line
			return true
		}
		ring = append(ring, line)
		return true
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	out := bufio.NewWriter(os.Stdout)
	defer func() { _ = out.Flush() }()
	for _, line := range ring {
		fmt.Fprintln(out, line)
	}
	return 0
}

func runGrep(args []string) int {
	fs := newFlagSet("grep")
	dir := fs.String("dir", "", "log directory or single container")
	keyFile := fs.String("key", "", "private key file")
	ignoreCase := fs.Bool("i", false, "case-insensitive")
	lineNumbers := fs.Bool("n", false, "show line numbers")
	showFile := fs.Bool("f", false, "prefix each line with its container name")
	if err := parseInterspersed(fs, args, "dir", "key"); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "grep requires a PATTERN")
		return 2
	}
	needle := strings.Join(rest, " ")
	if *ignoreCase {
		needle = strings.ToLower(needle)
	}
	key, err := resolveKey(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	target, err := resolveTarget(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	out := bufio.NewWriter(os.Stdout)
	defer func() { _ = out.Flush() }()
	lineNo := 0
	matches := 0
	if err := eachPlainLine(target, key, func(file string, line string) bool {
		lineNo++
		haystack := line
		if *ignoreCase {
			haystack = strings.ToLower(haystack)
		}
		if !strings.Contains(haystack, needle) {
			return true
		}
		matches++
		prefix := ""
		if *showFile {
			prefix = filepath.Base(file) + ":"
		}
		if *lineNumbers {
			fmt.Fprintf(out, "%s%d:%s\n", prefix, lineNo, line)
		} else {
			fmt.Fprintf(out, "%s%s\n", prefix, line)
		}
		return true
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "\n%d matching line(s)\n", matches)
	if matches == 0 {
		return 1
	}
	return 0
}

func runVerify(args []string) int {
	fs := newFlagSet("verify")
	dir := fs.String("dir", "", "log directory or single container")
	keyFile := fs.String("key", "", "private key file")
	if err := parseInterspersed(fs, args, "dir", "key"); err != nil {
		return 2
	}
	key, err := resolveKey(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	targets := fs.Args()
	if len(targets) == 0 {
		resolved, resolveErr := resolveTarget(*dir)
		if resolveErr != nil {
			fmt.Fprintln(os.Stderr, resolveErr)
			return 1
		}
		files, listErr := collectContainers(resolved)
		if listErr != nil {
			fmt.Fprintln(os.Stderr, listErr)
			return 1
		}
		targets = files
	}
	bad := 0
	for _, file := range targets {
		raw, readErr := os.ReadFile(file)
		if readErr != nil {
			fmt.Printf("FAIL %s: %v\n", file, readErr)
			bad++
			continue
		}
		keyID, idErr := logcrypt.ContainerKeyID(raw)
		if idErr != nil {
			fmt.Printf("FAIL %s: %v\n", file, idErr)
			bad++
			continue
		}
		plain, records, truncated, decErr := logcrypt.DecryptAll(key, raw)
		if decErr != nil {
			fmt.Printf("FAIL %-58s key-id=%s: %v\n", filepath.Base(file), keyID, decErr)
			bad++
			continue
		}
		status := "ok"
		if truncated {
			status = "truncated-tail"
		}
		fmt.Printf("%-15s %-58s key-id=%s records=%-7d bytes=%d\n",
			status, filepath.Base(file), keyID, records, len(plain))
	}
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "\n%d file(s) failed\n", bad)
		return 1
	}
	return 0
}

func runDecrypt(args []string) int {
	fs := newFlagSet("decrypt")
	keyFile := fs.String("key", "", "private key file")
	outDir := fs.String("o", "", "output directory for plaintext (required)")
	if err := parseInterspersed(fs, args, "key", "o"); err != nil {
		return 2
	}
	targets := fs.Args()
	if len(targets) == 0 || strings.TrimSpace(*outDir) == "" {
		fmt.Fprintln(os.Stderr, "decrypt requires FILE... and -o DIR")
		fmt.Fprintln(os.Stderr, "warning: this writes PLAINTEXT to disk; prefer `logtool grep`")
		return 2
	}
	key, err := resolveKey(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.MkdirAll(*outDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	bad := 0
	for _, file := range targets {
		raw, readErr := os.ReadFile(file)
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", file, readErr)
			bad++
			continue
		}
		plain, records, truncated, decErr := logcrypt.DecryptAll(key, raw)
		if decErr != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", file, decErr)
			bad++
			continue
		}
		name := strings.TrimSuffix(filepath.Base(file), ".enc")
		if name == filepath.Base(file) {
			name += ".txt"
		}
		destination := filepath.Join(*outDir, name)
		if writeErr := os.WriteFile(destination, plain, 0o600); writeErr != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", destination, writeErr)
			bad++
			continue
		}
		note := ""
		if truncated {
			note = "  (trailing record dropped as truncated)"
		}
		fmt.Printf("%s -> %s  records=%d bytes=%d%s\n", filepath.Base(file), destination, records, len(plain), note)
	}
	if bad > 0 {
		return 1
	}
	return 0
}
