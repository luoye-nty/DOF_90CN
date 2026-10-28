// Package ops 实现 DNF90Server/dnfbridge 进程启停：
// 以注入 DNFBRIDGE_LAUNCHER_AUTH_*、DNFBRIDGE_SERVER_IP 等环境变量的方式拉起子进程，
// 采集 stdout/stderr 滚动输出，支持优雅结束子进程树。
package ops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"dnf90ops/internal/config"
	"dnf90ops/internal/logcrypt"
)

// LineFunc 日志行回调（GUI 订阅）。
type LineFunc func(line string)

// ProcessManager 子进程管理。
type ProcessManager struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	pid     int
	running bool
	onLine  LineFunc

	stopOnce  sync.Once
	cancelLog context.CancelFunc
}

// NewProcessManager 创建管理器；onLine 可空。
func NewProcessManager(onLine LineFunc) *ProcessManager {
	return &ProcessManager{onLine: onLine}
}

// DNFBridgeEnv 由 config 派生注入的环境变量（登录器认证回调）。
func DNFBridgeEnv(cfg *config.Config) []string {
	verifyURL := "http://" + cfg.Internal.ListenAddr + "/internal/verify-game-ticket"
	timeout := "3s"
	if cfg.TicketTTL() < time.Minute {
		timeout = "1s"
	}
	return []string{
		"DNFBRIDGE_LAUNCHER_AUTH_ENABLED=true",
		"DNFBRIDGE_LAUNCHER_AUTH_VERIFY_URL=" + verifyURL,
		"DNFBRIDGE_LAUNCHER_AUTH_INTERNAL_TOKEN=" + cfg.Internal.Token,
		"DNFBRIDGE_LAUNCHER_AUTH_TIMEOUT=" + timeout,
	}
}

// Start 按 config 启动 dnfbridge（兼容旧入口：认证服务联动）。
func (m *ProcessManager) Start(cfg *config.Config) error {
	if cfg.DNFBridge.Executable == "" {
		return errors.New("dnfbridge executable not configured")
	}
	env := append(DNFBridgeEnv(cfg), cfg.DNFBridge.ExtraEnv...)
	return m.StartWithEnv(cfg.DNFBridge.Executable, cfg.ResolveDNBWorkDir(), cfg.DNFBridge.ExtraArgs, env)
}

// StartWithEnv 启动任意可执行文件（不注入 DNFBRIDGE_LAUNCHER_AUTH_*，由调用方组装 env）。
// 用于"一键启动"页动态选择服务端根目录后直启 runtime\bin\DNF90Server.exe。
func (m *ProcessManager) StartWithEnv(exe, workDir string, args, env []string) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return errors.New("process already running")
	}
	if exe == "" {
		m.mu.Unlock()
		return errors.New("executable not configured")
	}
	if _, err := os.Stat(exe); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("executable not found: %w", err)
	}
	if workDir != "" {
		if st, err := os.Stat(workDir); err != nil || !st.IsDir() {
			m.mu.Unlock()
			return fmt.Errorf("working dir invalid: %s", workDir)
		}
	}

	cmd := exec.Command(exe, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = append(os.Environ(), env...)
	// 隐藏服务端 console 黑框；stdout/stderr 仍经管道回传 GUI 日志框。
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	// 服务端 stdout/stderr 改为落盘，不再逐行 emit 到 GUI 输出框。
	//
	// 原来两条 pipeReader 会把服务端每一行都推给界面：日志量大时输出框被刷爆，
	// 而且工具一关什么都不剩。2026-09-26 在线上排查攻坚掉线时就吃过这个亏——
	// runtime\logs 下只有 mysql-ops-*.stdout.log，没有 server-*.stdout.log，
	// 于是 'event processing exceeded slow threshold' 这类行既没法 grep 也不留档。
	//
	// 刻意不改成 cmd.Stdout = 文件：那会绕开管道，而 m.cancelLog 与 Stop 的收尾语义
	// 都挂在管道上。这里只换目的地，不动生命周期。
	//
	// 2026-09-30 起目的地是**密文**（公钥信封，与服务端 cmd/server/control 的通道同一格式）：
	// 公钥编译期固化，服务端使用者无感；能解开的私钥只在运营本机，
	// 排障时把 runtime\logs 拉回本机用 logtool 解密检索。
	//
	// 失败只降级：拿不到公钥或建不了文件就退回原来的 GUI 输出，
	// **绝不退化成明文**，也绝不让服务端启动因此失败。
	sep := string(os.PathSeparator)
	logsDir := workDir + sep + "logs" + sep + "server"

	recipient, recipientErr := logcrypt.ResolveRecipient()
	var stdoutLog, stderrLog *logcrypt.Writer
	var stdoutErr, stderrErr error
	if recipientErr != nil {
		stdoutErr, stderrErr = recipientErr, recipientErr
	} else {
		stdoutLog, stdoutErr = logcrypt.Open(logcrypt.Config{
			Dir:            logsDir,
			Prefix:         "server-stdout",
			Ext:            ".log.enc",
			Recipient:      recipient,
			DropMarkerKind: "server-stdout-backpressure-dropped",
		})
		stderrLog, stderrErr = logcrypt.Open(logcrypt.Config{
			Dir:            logsDir,
			Prefix:         "server-stderr",
			Ext:            ".log.enc",
			Recipient:      recipient,
			DropMarkerKind: "server-stderr-backpressure-dropped",
		})
	}

	// pipeToFile 与 pipeReader 同构（同样的 scanner 上限、同样的 ctx 退出检查），
	// 只把目的地从 m.emit 换成加密写入器。抽成闭包让改动集中在一处，便于回退。
	// 写入器在这里 defer 关闭：scanner 因进程退出而 EOF 时即排空队列并收尾。
	pipeToFile := func(ctx context.Context, rc io.Reader, dst *logcrypt.Writer) {
		if dst == nil {
			return
		}
		defer func() { _ = dst.Close() }()
		scanner := bufio.NewScanner(rc)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line := strings.TrimRight(scanner.Text(), "\r\n")
			if line == "" {
				continue
			}
			dst.Write(line)
		}
	}

	logCtx, logCancel := context.WithCancel(context.Background())
	m.cancelLog = logCancel
	if err := cmd.Start(); err != nil {
		logCancel()
		if stdoutLog != nil {
			_ = stdoutLog.Close()
		}
		if stderrLog != nil {
			_ = stderrLog.Close()
		}
		m.mu.Unlock()
		return fmt.Errorf("start process: %w", err)
	}
	m.cmd = cmd
	m.pid = cmd.Process.Pid
	m.running = true
	m.mu.Unlock()

	m.emit(fmt.Sprintf("[ops] started pid=%d, exe=%s, workdir=%s", m.pid, exe, workDir))
	if stdoutErr == nil && stdoutLog != nil {
		m.emit("[ops] 服务端 stdout 落盘（密文）：" + logsDir + sep + "<日期>" + sep + "server-stdout-*.log.enc")
		go pipeToFile(logCtx, stdout, stdoutLog)
	} else {
		m.emit("[ops] 服务端 stdout 落盘失败（拿不到公钥或建不了文件），退回输出框")
		go m.pipeReader(logCtx, "stdout", stdout)
	}
	if stderrErr == nil && stderrLog != nil {
		go pipeToFile(logCtx, stderr, stderrLog)
	} else {
		go m.pipeReader(logCtx, "stderr", stderr)
	}

	// 退出监控：进程自然结束时重置 running。
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		m.running = false
		if m.cmd == cmd {
			m.cmd = nil
			m.pid = 0
		}
		m.mu.Unlock()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				m.emit(fmt.Sprintf("[ops] process exited, code=%d", ee.ExitCode()))
			} else {
				m.emit(fmt.Sprintf("[ops] process exited: %v", err))
			}
		} else {
			m.emit("[ops] process exited")
		}
	}()
	return nil
}

// Stop 优雅停止：先 taskkill /T（无 /F，先尝试正常关闭），超时后强制。
func (m *ProcessManager) Stop(timeout time.Duration) error {
	m.mu.Lock()
	if !m.running || m.pid == 0 {
		m.mu.Unlock()
		return errors.New("process not running")
	}
	pid := m.pid
	m.mu.Unlock()

	m.emit(fmt.Sprintf("[ops] stopping process (pid=%d)...", pid))

	// 第一击：向整棵进程树发关闭信号（不强制）。
	_ = runTaskkill(pid, false)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !m.processAlive(pid) {
			m.emit("[ops] process stopped gracefully")
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	// 超时仍存活：强制结束进程树。
	m.emit("[ops] graceful stop timeout, forcing kill")
	_ = runTaskkill(pid, true)
	for i := 0; i < 10; i++ {
		if !m.processAlive(pid) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

// Kill 强制终止（兜底）。
func (m *ProcessManager) Kill() {
	m.mu.Lock()
	pid := m.pid
	m.mu.Unlock()
	if pid > 0 {
		_ = runTaskkill(pid, true)
	}
}

// Status 返回 {running, pid}。
func (m *ProcessManager) Status() (running bool, pid int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running || m.pid == 0 {
		return false, 0
	}
	if !m.processAlive(m.pid) {
		// 让退出监控 goroutine 统一收尾；此处仅报告瞬时状态。
		return false, 0
	}
	return true, m.pid
}

// ProbePort 探测 addr 是否在监听。
func ProbePort(addr string, timeout time.Duration) bool {
	if addr == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// GameUp 按 config 的 status_port 判断游戏服务监听状态。
func GameUp(cfg *config.Config) bool {
	if cfg.DNFBridge.StatusPort <= 0 {
		return false
	}
	return ProbePort("127.0.0.1:"+strconv.Itoa(cfg.DNFBridge.StatusPort), 400*time.Millisecond)
}

func (m *ProcessManager) pipeReader(ctx context.Context, name string, rc io.Reader) {
	br := bufio.NewScanner(rc)
	br.Buffer(make([]byte, 64*1024), 1024*1024)
	for br.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := strings.TrimRight(br.Text(), "\r\n")
		if line != "" {
			m.emit(line)
		}
	}
	// 管道 EOF：若为强制终止导致，直接返回。
	select {
	case <-ctx.Done():
	default:
		m.emit(fmt.Sprintf("[ops] %s pipe closed", name))
	}
}

func (m *ProcessManager) emit(line string) {
	if m.onLine != nil {
		m.onLine(line)
	}
}

// processAlive Windows 下用 tasklist 精确匹配 PID 判断进程是否存在。
func (m *ProcessManager) processAlive(pid int) bool {
	return processExistsWindows(pid)
}

// processExistsWindows 通过 tasklist 精确匹配 PID 判断存活。
func processExistsWindows(pid int) bool {
	cmd := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	text := string(out)
	return strings.Contains(text, strconv.Itoa(pid))
}

func runTaskkill(pid int, force bool) error {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	cmd := exec.Command("taskkill", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}
