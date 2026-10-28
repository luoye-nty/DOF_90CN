package dnfbridge

import (
	"os"
	"path/filepath"

	"longheng.io/server/internal/platform/logcrypt"
	"longheng.io/server/internal/platform/logkey"
)

// goroutineDumpFileName 是 goroutine 快照文件名。
//
// 自 2026-09-30 起与其它日志一样**落盘即密文**（扩展名 .enc 由 logcrypt 容器决定），
// 快照里含函数名、源码路径与内存地址，属于不该外泄的现场信息。
const goroutineDumpFileName = "goroutine_dump.enc"

// writeEncryptedGoroutineDump 原子地覆盖写入一份加密快照。
//
// 收件人公钥固化在二进制里（internal/platform/logkey），服务端不需要密钥文件。
// 每轮都重建容器（新 DEK），因此**必然是新文件**：先写 .tmp 再 rename，
// 保证读到的永远是完整容器，不会看到写了一半的快照。
func writeEncryptedGoroutineDump(dir string, payload []byte) error {
	recipient, err := logkey.ResolveRecipient()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	target := filepath.Join(dir, goroutineDumpFileName)
	temp := target + ".tmp"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	encoder, err := logcrypt.NewEncoder(recipient, file, nil)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return err
	}
	if err := encoder.WriteRecord(payload); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return os.Rename(temp, target)
}
