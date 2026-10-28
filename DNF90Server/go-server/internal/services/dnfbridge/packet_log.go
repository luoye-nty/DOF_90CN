package dnfbridge

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"longheng.io/server/internal/platform/logcrypt"
	"longheng.io/server/internal/platform/logkey"
)

const (
	// packetLogHexLimit 是单行记录的 hex 截断长度（未变）。
	packetLogHexLimit = 2048
	// 下面三个沿用既有名字，值来自 logcrypt 的默认值，并在 openPacketLogger 里
	// 显式传进配置——测试与诊断代码一直以它们为"多久会落盘"的依据。
	packetLogQueueSize     = logcrypt.DefaultQueueSize
	packetLogBufferSize    = logcrypt.DefaultBufferSize
	packetLogFlushInterval = logcrypt.DefaultFlushInterval

	// packetLogArchiveDirName 是锚点同级下的归档目录名。
	packetLogArchiveDirName = "packet"
	// packetLogArchiveExt 是加密归档的扩展名；.enc 明确表示"这是密文，别用文本工具开"。
	packetLogArchiveExt = ".log.enc"
	// packetLogDropMarkerKind 与旧实现保持同名，解密后的事件流与历史一致。
	packetLogDropMarkerKind = "packet-log-backpressure-dropped"

	// 默认密钥路径，**相对于服务进程工作目录**（也就是 runtime/），刻意放在 logs/ 之外：
	// 只归档 logs/ 不会连带把密钥一起带走。公钥随分发包分发；私钥**绝不**放进分发包。
	defaultLogPublicKeyFile  = "keys/log-public.key"
	defaultLogPrivateKeyFile = "keys/log-private.key"
)

// packetLogger 把既有的日志行格式交给 logcrypt 做逐行加密与轮转。
//
// 落盘布局（锚点 path 默认是 logs/packet_log.txt）：
//
//	logs/packet/<YYYYMMDD>/packet_log-<YYYYMMDD>-NNN.log.enc   密文分片，单文件上限 100 MB
//	logs/packet/current.txt                                    当前分片指针（解密工具默认读它）
//
// 收件人公钥固化在二进制里（internal/platform/logkey），**服务端不需要任何密钥文件**。
//
// 为什么不把锚点本身当输出文件：records 目录是从锚点路径的**父目录**推导出来的
// （defaultRecordDirFor），保持父目录不变，logs/records 才不会被搬走或孤立。
//
// 解密后的文本与加密前的历史格式逐字节一致，因此仓库里那些"从 packet_log 抄下来的"
// 现场注释与既有分析脚本在解密之后仍然适用。
type packetLogger struct {
	writer *logcrypt.Writer
}

func openPacketLogger(path string) (*packetLogger, error) {
	if path == "" {
		return nil, nil
	}
	anchorDir := filepath.Dir(path)
	archiveDir := filepath.Join(anchorDir, packetLogArchiveDirName)
	prefix := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if prefix == "" || prefix == "." {
		prefix = "packet"
	}

	// 收件人公钥**固化在二进制里**（internal/platform/logkey）：用服务端的人不需要
	// 理解、放置或配置任何密钥文件，拿到就能跑。公钥只能加密、不能解密，写死是安全的。
	// 只有显式设了 DNFBRIDGE_LOG_PUBLIC_KEY_FILE 才用它覆盖（轮换/测试用）。
	recipient, err := logkey.ResolveRecipient()
	if err != nil {
		return nil, err
	}
	writer, err := logcrypt.Open(logcrypt.Config{
		Dir:            archiveDir,
		Prefix:         prefix,
		Ext:            packetLogArchiveExt,
		Recipient:      recipient,
		MaxBytes:       int64(envInt("DNFBRIDGE_LOG_MAX_BYTES", logcrypt.DefaultMaxBytes)),
		RetentionDays:  envInt("DNFBRIDGE_LOG_RETENTION_DAYS", logcrypt.DefaultRetentionDays),
		QueueSize:      packetLogQueueSize,
		BufferSize:     packetLogBufferSize,
		FlushInterval:  packetLogFlushInterval,
		DropMarkerKind: packetLogDropMarkerKind,
		Logger:         slog.Default(),
	})
	if err != nil {
		return nil, err
	}
	logger := &packetLogger{writer: writer}
	logger.writeLine("=== LongHeng Go dnfbridge packet log started " + time.Now().UTC().Format(time.RFC3339) + " ===")
	return logger, nil
}

func (l *packetLogger) Close() error {
	if l == nil || l.writer == nil {
		return nil
	}
	return l.writer.Close()
}

// Dropped 返回因写队列溢出被丢弃的行数。诊断用它判断"某段日志里的计数是不是下限"。
func (l *packetLogger) Dropped() uint64 {
	if l == nil || l.writer == nil {
		return 0
	}
	return l.writer.Dropped()
}

// Path 当前分片路径（测试与诊断用）。
func (l *packetLogger) Path() string {
	if l == nil || l.writer == nil {
		return ""
	}
	return l.writer.Path()
}

func (l *packetLogger) Log(direction string, kind string, data []byte, fields ...any) {
	if l == nil || l.writer == nil {
		return
	}
	hexData := data
	truncated := ""
	if len(hexData) > packetLogHexLimit {
		hexData = hexData[:packetLogHexLimit]
		truncated = " truncated=true"
	}
	line := fmt.Sprintf("%s kind=%s", direction, kind)
	for i := 0; i+1 < len(fields); i += 2 {
		line += fmt.Sprintf(" %v=%v", fields[i], fields[i+1])
	}
	line += fmt.Sprintf(" len=%d%s hex=%s", len(data), truncated, hex.EncodeToString(hexData))
	l.writeLine(line)
}

func (l *packetLogger) Event(kind string, fields ...any) {
	if l == nil || l.writer == nil {
		return
	}
	line := "EVENT kind=" + kind
	for i := 0; i+1 < len(fields); i += 2 {
		line += fmt.Sprintf(" %v=%v", fields[i], fields[i+1])
	}
	l.writeLine(line)
}

func (l *packetLogger) writeLine(line string) {
	if l == nil || l.writer == nil {
		return
	}
	l.writer.Write(time.Now().UTC().Format(time.RFC3339Nano) + " " + line + "\n")
}

// logPrivateKeyPath 解析私钥路径。
//
// **服务端运行不需要私钥**（写日志只要公钥）。这里只为两种情况保留：
// 测试解密断言，以及运营在自己服务器上做本机回读。分发包里绝不放私钥。
func logPrivateKeyPath(anchorPath string) string {
	if explicit := strings.TrimSpace(os.Getenv("DNFBRIDGE_LOG_PRIVATE_KEY_FILE")); explicit != "" {
		return explicit
	}
	anchorDir := filepath.Dir(anchorPath)
	if anchorDir == "" || anchorDir == "." {
		return filepath.Join("keys", logcrypt.PrivateKeyFileName)
	}
	return filepath.Join(filepath.Dir(anchorDir), "keys", logcrypt.PrivateKeyFileName)
}
