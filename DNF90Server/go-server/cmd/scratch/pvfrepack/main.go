// Command pvfrepack rebuilds an existing DNF PVF with raw file replacements.
// It never edits the base archive in place and does not add or delete paths.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	toolVersion          = "2026.08.17"
	defaultMaxFileBytes  = int64(256 * 1024 * 1024)
	defaultModifiedDir   = "PVF修改文件"
	defaultOutputPVFName = "Script_重新打包.pvf"
)

type packOptions struct {
	basePath        string
	modifiedRoot    string
	outputPath      string
	maxBytes        int64
	maxFileBytes    int64
	overwrite       bool
	dryRun          bool
	continueOnError bool
}

type scanResult struct {
	scanned      int
	unchanged    int
	failed       int
	replacements []platformpvf.RawReplacement
}

func main() {
	prepareConsole()
	if len(os.Args) == 1 {
		reader := bufio.NewReader(os.Stdin)
		err := runInteractive(reader, os.Stdout, os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "pvfrepack:", err)
		}
		fmt.Fprint(os.Stdout, "\n按回车键退出……")
		_, _ = reader.ReadString('\n')
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "pvfrepack:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("pvfrepack", flag.ContinueOnError)
	flags.SetOutput(stderr)
	base := flags.String("pvf", "", "作为基底的原始 Script.pvf（只读）")
	modified := flags.String("dir", "", "按 PVF 相对路径存放的 raw 修改文件目录")
	output := flags.String("out", "", "新 PVF 输出路径")
	maxBytes := flags.Int64("max-bytes", platformpvf.DefaultMaxBytes, "允许读取的最大 PVF 字节数")
	maxFileBytes := flags.Int64("max-file-bytes", defaultMaxFileBytes, "单个替换文件的最大字节数")
	overwrite := flags.Bool("overwrite", false, "输出已存在时备份旧文件并写入新文件")
	dryRun := flags.Bool("dry-run", false, "只扫描和比较修改，不生成 PVF")
	continueOnError := flags.Bool("continue-on-error", false, "扫描时记录单文件错误并继续；存在错误时不会打包")
	version := flags.Bool("version", false, "显示工具版本")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "PVF 打包工具 %s（raw 修改文件）\n\n", toolVersion)
		fmt.Fprintln(flags.Output(), "用法：")
		fmt.Fprintln(flags.Output(), "  PVF打包工具.exe -pvf <原始 Script.pvf> -dir <修改文件目录> -out <新 PVF>")
		fmt.Fprintln(flags.Output(), "\n修改目录必须来自解包工具的 raw 模式，并保持 PVF 内部相对路径。")
		fmt.Fprintln(flags.Output(), "decoded 可读文本不能直接重新封包。工具不会覆盖基底 PVF，也不新增或删除路径。")
		fmt.Fprintln(flags.Output(), "\n选项：")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *version {
		fmt.Fprintf(stdout, "pvfrepack %s\n", toolVersion)
		return nil
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("不支持位置参数：%s", strings.Join(flags.Args(), " "))
	}

	options := packOptions{
		basePath:        strings.TrimSpace(*base),
		modifiedRoot:    strings.TrimSpace(*modified),
		outputPath:      strings.TrimSpace(*output),
		maxBytes:        *maxBytes,
		maxFileBytes:    *maxFileBytes,
		overwrite:       *overwrite,
		dryRun:          *dryRun,
		continueOnError: *continueOnError,
	}
	return pack(options, stdout, stderr)
}

func pack(options packOptions, stdout, stderr io.Writer) error {
	resolved, err := resolvePackOptions(options)
	if err != nil {
		return err
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{
		Path:     resolved.basePath,
		MaxBytes: resolved.maxBytes,
	})
	if err != nil {
		return err
	}
	snapshot := archive.Snapshot()
	fmt.Fprintf(stdout, "base format=%s files=%d groups=%d size=%d sha256=%s\n",
		snapshot.Format, snapshot.FileCount, snapshot.GroupCount, snapshot.Size, snapshot.Checksum)

	scan, err := scanModifications(archive, resolved, stdout, stderr)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "scan scanned=%d changed=%d unchanged=%d failed=%d\n",
		scan.scanned, len(scan.replacements), scan.unchanged, scan.failed)
	if scan.failed != 0 {
		return fmt.Errorf("有 %d 个修改文件无效，未生成 PVF", scan.failed)
	}
	if len(scan.replacements) == 0 {
		fmt.Fprintln(stdout, "没有检测到内容变化，未生成 PVF。")
		return nil
	}
	if resolved.dryRun {
		fmt.Fprintln(stdout, "dry-run 完成，未生成 PVF。")
		return nil
	}

	fmt.Fprintf(stdout, "repack changed_files=%d ...\n", len(scan.replacements))
	repacked, err := archive.RepackRaw(scan.replacements)
	if err != nil {
		return fmt.Errorf("重建 PVF: %w", err)
	}
	temporary, outputHash, err := writeTemporaryCandidate(resolved.outputPath, repacked)
	if err != nil {
		return err
	}
	repacked = nil
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporary)
		}
	}()

	if err := verifyCandidate(temporary, scan.replacements, resolved.maxBytes); err != nil {
		return err
	}
	backup, err := commitCandidate(temporary, resolved.outputPath, resolved.overwrite)
	if err != nil {
		return err
	}
	committed = true
	info, err := os.Stat(resolved.outputPath)
	if err != nil {
		return fmt.Errorf("读取输出 PVF: %w", err)
	}
	fmt.Fprintf(stdout, "done changed=%d output=%s size=%d sha256=%s\n",
		len(scan.replacements), resolved.outputPath, info.Size(), outputHash)
	if backup != "" {
		fmt.Fprintf(stdout, "previous_output_backup=%s\n", backup)
	}
	return nil
}

func resolvePackOptions(options packOptions) (packOptions, error) {
	if options.basePath == "" {
		return options, errors.New("必须提供 -pvf")
	}
	if options.modifiedRoot == "" {
		return options, errors.New("必须提供 -dir")
	}
	if options.outputPath == "" && !options.dryRun {
		return options, errors.New("必须提供 -out；只检查可使用 -dry-run")
	}
	if options.maxBytes <= 0 || options.maxFileBytes <= 0 {
		return options, errors.New("大小限制必须大于 0")
	}
	var err error
	options.basePath, err = filepath.Abs(trimWrappingQuotes(options.basePath))
	if err != nil {
		return options, fmt.Errorf("解析基底 PVF: %w", err)
	}
	options.modifiedRoot, err = filepath.Abs(trimWrappingQuotes(options.modifiedRoot))
	if err != nil {
		return options, fmt.Errorf("解析修改目录: %w", err)
	}
	if options.outputPath != "" {
		options.outputPath, err = filepath.Abs(trimWrappingQuotes(options.outputPath))
		if err != nil {
			return options, fmt.Errorf("解析输出 PVF: %w", err)
		}
		if sameWindowsPath(options.basePath, options.outputPath) {
			return options, errors.New("输出路径不能与基底 PVF 相同；工具禁止原地覆盖")
		}
		outputInfo, outputErr := os.Stat(options.outputPath)
		switch {
		case outputErr == nil && outputInfo.IsDir():
			return options, errors.New("输出路径是目录，必须指向 .pvf 文件")
		case outputErr == nil && !options.overwrite && !options.dryRun:
			return options, fmt.Errorf("输出已存在：%s；使用 -overwrite 才会备份并替换", options.outputPath)
		case outputErr != nil && !errors.Is(outputErr, os.ErrNotExist):
			return options, fmt.Errorf("检查输出 PVF: %w", outputErr)
		}
	}
	baseInfo, err := os.Stat(options.basePath)
	if err != nil {
		return options, fmt.Errorf("读取基底 PVF: %w", err)
	}
	if !baseInfo.Mode().IsRegular() {
		return options, errors.New("基底 PVF 不是普通文件")
	}
	modifiedInfo, err := os.Stat(options.modifiedRoot)
	if err != nil {
		return options, fmt.Errorf("读取修改目录: %w", err)
	}
	if !modifiedInfo.IsDir() {
		return options, errors.New("-dir 必须是目录")
	}
	return options, nil
}

func scanModifications(
	archive *platformpvf.Archive,
	options packOptions,
	stdout io.Writer,
	stderr io.Writer,
) (scanResult, error) {
	var result scanResult
	err := filepath.WalkDir(options.modifiedRoot, func(localPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("修改目录不允许符号链接：%s", localPath)
		}
		if entry.IsDir() {
			return nil
		}
		absolute, err := filepath.Abs(localPath)
		if err != nil {
			return err
		}
		if sameWindowsPath(absolute, options.basePath) ||
			(options.outputPath != "" && sameWindowsPath(absolute, options.outputPath)) {
			return nil
		}
		relative, err := filepath.Rel(options.modifiedRoot, absolute)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("修改文件越过根目录：%s", localPath)
		}
		archivePath := filepath.ToSlash(relative)
		result.scanned++
		file, found := archive.FindFile(archivePath)
		if !found {
			return handleScanError(options.continueOnError, stderr, &result,
				fmt.Errorf("修改文件在基底 PVF 中不存在：%s", archivePath))
		}
		info, err := entry.Info()
		if err != nil {
			return handleScanError(options.continueOnError, stderr, &result, err)
		}
		if info.Size() > options.maxFileBytes {
			return handleScanError(options.continueOnError, stderr, &result,
				fmt.Errorf("修改文件过大：%s size=%d limit=%d", archivePath, info.Size(), options.maxFileBytes))
		}
		modified, err := os.ReadFile(absolute)
		if err != nil {
			return handleScanError(options.continueOnError, stderr, &result, err)
		}
		original, err := archive.FileRaw(file.Index)
		if err != nil {
			return handleScanError(options.continueOnError, stderr, &result,
				fmt.Errorf("读取基底文件 %s: %w", archivePath, err))
		}
		if bytes.Equal(original, modified) {
			result.unchanged++
			return nil
		}
		if err := validateRawReplacement(file, original, modified); err != nil {
			return handleScanError(options.continueOnError, stderr, &result,
				fmt.Errorf("%s: %w", archivePath, err))
		}
		result.replacements = append(result.replacements, platformpvf.RawReplacement{
			Path: archivePath,
			Data: modified,
		})
		fmt.Fprintf(stdout, "changed\t%d\t%d->%d\t%s\n", file.DataType, len(original), len(modified), archivePath)
		if result.scanned%1000 == 0 {
			fmt.Fprintf(stdout, "scanned=%d changed=%d\n", result.scanned, len(result.replacements))
		}
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("扫描修改目录: %w", err)
	}
	return result, nil
}

func handleScanError(continueOnError bool, stderr io.Writer, result *scanResult, err error) error {
	if !continueOnError {
		return err
	}
	result.failed++
	fmt.Fprintf(stderr, "[错误] %v\n", err)
	return nil
}

func validateRawReplacement(file platformpvf.File, original, modified []byte) error {
	switch file.DataType {
	case 1:
		if len(modified)%5 != 0 {
			return errors.New("type-1 脚本不是五字节 token 流；疑似使用了 decoded 文本，请重新以 raw 模式解包")
		}
		allowed := map[byte]struct{}{0: {}, 2: {}, 3: {}, 5: {}, 6: {}, 7: {}}
		for offset := 0; offset+5 <= len(original); offset += 5 {
			allowed[original[offset]] = struct{}{}
		}
		for offset := 0; offset+5 <= len(modified); offset += 5 {
			if _, ok := allowed[modified[offset]]; !ok {
				return fmt.Errorf("type-1 token 偏移 %d 的类型 %d 无效；疑似 decoded 文本", offset, modified[offset])
			}
			if (modified[offset] == 3 || modified[offset] == 6) &&
				int32(binary.LittleEndian.Uint32(modified[offset+1:offset+5])) < 0 {
				return fmt.Errorf("type-1 字符串 token 偏移 %d 使用负引用", offset)
			}
		}
	case 3:
		if len(modified)%2 != 0 {
			return errors.New("type-3 UTF-16LE 文件字节数不是偶数；疑似使用了 decoded UTF-8 文本")
		}
	}
	return nil
}

func writeTemporaryCandidate(outputPath string, data []byte) (string, string, error) {
	directory := filepath.Dir(outputPath)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", "", fmt.Errorf("创建输出目录: %w", err)
	}
	file, err := os.CreateTemp(directory, ".pvfrepack-*.pvf")
	if err != nil {
		return "", "", fmt.Errorf("创建临时 PVF: %w", err)
	}
	temporary := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, hash), bytes.NewReader(data)); err != nil {
		return "", "", fmt.Errorf("写入临时 PVF: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", "", fmt.Errorf("同步临时 PVF: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", "", fmt.Errorf("关闭临时 PVF: %w", err)
	}
	keep = true
	return temporary, strings.ToUpper(hex.EncodeToString(hash.Sum(nil))), nil
}

func verifyCandidate(path string, replacements []platformpvf.RawReplacement, maxBytes int64) error {
	verified, err := platformpvf.LoadArchive(platformpvf.Options{Path: path, MaxBytes: maxBytes})
	if err != nil {
		return fmt.Errorf("重新打开候选 PVF: %w", err)
	}
	for _, replacement := range replacements {
		raw, err := verified.ReadRaw(replacement.Path)
		if err != nil {
			return fmt.Errorf("校验 %s: %w", replacement.Path, err)
		}
		if !bytes.Equal(raw, replacement.Data) {
			return fmt.Errorf("校验 %s: 成品字节与修改文件不一致", replacement.Path)
		}
	}
	return nil
}

func commitCandidate(temporary, outputPath string, overwrite bool) (string, error) {
	_, err := os.Stat(outputPath)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(temporary, outputPath); err != nil {
			return "", fmt.Errorf("发布输出 PVF: %w", err)
		}
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("检查输出 PVF: %w", err)
	}
	if !overwrite {
		return "", fmt.Errorf("输出已存在：%s；使用 -overwrite 才会备份并替换", outputPath)
	}
	backup := fmt.Sprintf("%s.previous-%s", outputPath, time.Now().Format("20060102-150405"))
	if _, err := os.Stat(backup); !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("输出备份路径已存在：%s", backup)
	}
	if err := os.Rename(outputPath, backup); err != nil {
		return "", fmt.Errorf("备份旧输出 PVF: %w", err)
	}
	if err := os.Rename(temporary, outputPath); err != nil {
		_ = os.Rename(backup, outputPath)
		return "", fmt.Errorf("发布新 PVF: %w", err)
	}
	return backup, nil
}

func runInteractive(reader *bufio.Reader, stdout, stderr io.Writer) error {
	fmt.Fprintf(stdout, "PVF 打包工具 %s（raw 修改文件）\n", toolVersion)
	fmt.Fprintln(stdout, "不会原地修改基底 PVF；decoded 文本不能直接重新封包。")

	defaultBase, defaultRoot := adjacentDefaults()
	basePrompt := "\n请输入或拖入原始 Script.pvf："
	if defaultBase != "" {
		basePrompt = fmt.Sprintf("\n基底 PVF [默认 %s]：", defaultBase)
	}
	base, err := promptLine(reader, stdout, basePrompt)
	if err != nil {
		return err
	}
	base = trimWrappingQuotes(base)
	if base == "" {
		base = defaultBase
	}
	if base == "" {
		return errors.New("必须提供基底 PVF")
	}

	modifiedDefault := filepath.Join(defaultRoot, defaultModifiedDir)
	modified, err := promptLine(reader, stdout, fmt.Sprintf("raw 修改文件目录 [默认 %s]：", modifiedDefault))
	if err != nil {
		return err
	}
	modified = trimWrappingQuotes(modified)
	if modified == "" {
		modified = modifiedDefault
	}
	outputDefault := filepath.Join(defaultRoot, defaultOutputPVFName)
	output, err := promptLine(reader, stdout, fmt.Sprintf("新 PVF 输出路径 [默认 %s]：", outputDefault))
	if err != nil {
		return err
	}
	output = trimWrappingQuotes(output)
	if output == "" {
		output = outputDefault
	}
	overwrite, err := promptLine(reader, stdout, "输出存在时备份并替换？y/N：")
	if err != nil {
		return err
	}
	dryRun, err := promptLine(reader, stdout, "只检查修改、不生成 PVF？y/N：")
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\n基底：%s\n修改目录：%s\n输出：%s\n", base, modified, output)
	fmt.Fprintln(stdout, "修改目录只能放 raw 模式导出的文件，并保持原 PVF 相对路径。")
	confirm, err := promptLine(reader, stdout, "输入 PACK 开始：")
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(confirm), "PACK") {
		fmt.Fprintln(stdout, "已取消，没有生成 PVF。")
		return nil
	}

	args := []string{"-pvf", base, "-dir", modified, "-out", output, "-continue-on-error"}
	if answerIsYes(overwrite, false) {
		args = append(args, "-overwrite")
	}
	if answerIsYes(dryRun, false) {
		args = append(args, "-dry-run")
	}
	return run(args, stdout, stderr)
}

func adjacentDefaults() (string, string) {
	executable, err := os.Executable()
	if err != nil {
		current, _ := os.Getwd()
		return "", current
	}
	root := filepath.Dir(executable)
	candidate := filepath.Join(root, "Script.pvf")
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() {
		return "", root
	}
	return candidate, root
}

func promptLine(reader *bufio.Reader, output io.Writer, prompt string) (string, error) {
	fmt.Fprint(output, prompt)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) && value == "" {
		return "", errors.New("输入已关闭")
	}
	return strings.TrimSpace(value), nil
}

func trimWrappingQuotes(value string) string {
	value = strings.TrimSpace(strings.TrimPrefix(value, "\uFEFF"))
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') ||
		(value[0] == '\'' && value[len(value)-1] == '\'')) {
		return strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

func answerIsYes(value string, defaultValue bool) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue
	}
	return strings.EqualFold(value, "y") || strings.EqualFold(value, "yes")
}

func sameWindowsPath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
