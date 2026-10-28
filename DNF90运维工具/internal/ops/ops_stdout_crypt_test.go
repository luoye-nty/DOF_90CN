package ops

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dnf90ops/internal/logcrypt"
)

// 2026-09-30 起，运维工具拉起服务端时，服务端 stdout/stderr 落盘的是**密文**。
//
// 这条测试用一个真实子进程走完整链路：StartWithEnv → 管道 → pipeToFile → 加密写入器，
// 断言磁盘上只有 logcrypt 容器、且**找不到任何明文**。
// （能不能解开由运营本机的私钥决定，跨模块解密验收见 internal/logcrypt 的
// TestAcceptanceArtifact + 服务端 logtool。）
func TestStartWithEnvWritesEncryptedServerStdout(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("运维工具只在 Windows 上跑")
	}
	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	if _, err := os.Stat(comspec); err != nil {
		t.Skipf("cmd.exe 不可用: %v", err)
	}
	// 确保走的是固化公钥，而不是本机环境里可能存在的覆盖项。
	t.Setenv(logcrypt.PublicKeyEnvVar, "")

	workDir := t.TempDir()
	manager := NewProcessManager(nil)
	const marker = "OPS-STDOUT-MARKER"
	if err := manager.StartWithEnv(comspec, workDir,
		[]string{"/c", "echo " + marker + " & echo second-line"}, nil); err != nil {
		t.Fatalf("StartWithEnv: %v", err)
	}
	t.Cleanup(func() { manager.Kill() })

	waitForExit(t, manager, 20*time.Second)
	logsDir := filepath.Join(workDir, "logs", "server")
	files := waitForContainer(t, logsDir, 20*time.Second)
	if len(files) == 0 {
		t.Fatalf("没有在 %s 下找到任何密文分片", logsDir)
	}

	sawStdout := false
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if bytes.Contains(raw, []byte(marker)) || bytes.Contains(raw, []byte("second-line")) {
			t.Fatalf("%s 里出现了明文", file)
		}
		if bytes.HasPrefix(raw, []byte("LHLOG2")) && strings.Contains(file, "server-stdout") {
			sawStdout = true
			// 子进程输出两行：至少要有两条记录（每条 = 明文 + 4 + 12 + 16 字节）。
			if info, statErr := os.Stat(file); statErr == nil {
				if min := int64(119 + 2*(1+4+12+16)); info.Size() < min {
					t.Fatalf("%s 只有 %d 字节，少于两条记录的下限 %d", file, info.Size(), min)
				}
			}
		}
	}
	if !sawStdout {
		t.Fatalf("没有找到 server-stdout 密文分片：%v", files)
	}
}

// waitForExit 等子进程结束（Status 由退出监控 goroutine 复位）。
func waitForExit(t *testing.T, manager *ProcessManager, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if running, _ := manager.Status(); !running {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("子进程在 %s 内没有结束", timeout)
}

// waitForContainer 等加密分片落盘并写完（大小连续两次采样不变即认为收尾完成）。
func waitForContainer(t *testing.T, logsDir string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []string
	var lastSize int64 = -1
	for time.Now().Before(deadline) {
		var files []string
		walkErr := filepath.WalkDir(logsDir, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log.enc") {
				return nil
			}
			files = append(files, path)
			return nil
		})
		if walkErr == nil && len(files) > 0 {
			var total int64
			for _, file := range files {
				if info, statErr := os.Stat(file); statErr == nil {
					total += info.Size()
				}
			}
			// 容器头 119 字节 + 至少一条记录，才算真的写进去了。
			if total > 119 && total == lastSize {
				return files
			}
			last, lastSize = files, total
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last
}
