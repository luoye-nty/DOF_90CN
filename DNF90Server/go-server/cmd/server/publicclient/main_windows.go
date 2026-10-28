//go:build windows

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const clientArgument = "99?127.0.0.1?7001?0?de509f65e9ccaae621cb7278fc2b8e6c?01?1?0?0?0?0?1?9n2b1c8r3w7y?0?0?19847"

var (
	sshTargetPattern  = regexp.MustCompile(`^[A-Za-z0-9_.@:-]{1,255}$`)
	remotePathPattern = regexp.MustCompile(`^/[A-Za-z0-9_./-]{1,511}$`)
)

type connectorConfig struct {
	SchemaVersion     int    `json:"schemaVersion"`
	ClientDirectory   string `json:"clientDirectory"`
	SSHTarget         string `json:"sshTarget"`
	RemoteControlPath string `json:"remoteControlPath"`
	RegionID          int    `json:"regionId"`
	RegionName        string `json:"regionName"`
	MultiInstance     bool   `json:"multiInstance"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "连接失败：", err)
		fmt.Fprintln(os.Stderr, "按回车键退出。")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "connector configuration JSON")
	flag.Parse()
	path, err := resolveConfigPath(*configPath)
	if err != nil {
		return err
	}
	cfg, err := loadConnectorConfig(path)
	if err != nil {
		return err
	}
	ssh, err := exec.LookPath("ssh.exe")
	if err != nil {
		return errors.New("找不到 Windows OpenSSH 客户端 ssh.exe；请在 Windows 可选功能中安装 OpenSSH 客户端")
	}
	tunnel, err := ensureTunnel(ssh, cfg.SSHTarget)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success && tunnel != nil {
			_ = tunnel.Process.Kill()
			_, _ = tunnel.Process.Wait()
		}
	}()
	username, password, err := readCredentials()
	if err != nil {
		return err
	}
	executable, err := findClientExecutable(cfg.ClientDirectory)
	if err != nil {
		return err
	}
	// Bind the WireGuard source address before DNF can open a game socket. The
	// remote Windows PID is not a Linux process identity, so zero is a valid
	// pre-launch audit value for this route.
	if err := bindRemoteAccount(ssh, cfg, username, password, 0); err != nil {
		return err
	}
	client := exec.Command(executable, clientArgument)
	client.Dir = cfg.ClientDirectory
	multi := "0"
	if cfg.MultiInstance {
		multi = "1"
	}
	client.Env = mergeEnvironment(os.Environ(), map[string]string{
		"DNF_HOOK_CREATE":  "1",
		"DNF_MULTI_CLIENT": multi,
		"DNF_REGION_ID":    strconv.Itoa(cfg.RegionID),
		"DNF_REGION_NAME":  cfg.RegionName,
	})
	client.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := client.Start(); err != nil {
		return fmt.Errorf("启动游戏客户端：%w", err)
	}
	pid := client.Process.Pid
	password = ""
	if err := client.Process.Release(); err != nil {
		return fmt.Errorf("释放游戏进程句柄：%w", err)
	}
	if tunnel != nil {
		// The stock client may hand its window to a child process shortly after
		// launch, so the connector cannot reliably wait on the initial PID. Keep
		// this key-authenticated loopback forward alive after the connector exits;
		// a later connector invocation reuses the listener. It carries only the
		// local channel port, never the admin endpoint or MySQL.
		if err := tunnel.Process.Release(); err != nil {
			return fmt.Errorf("保留 SSH 频道转发：%w", err)
		}
	}
	success = true
	fmt.Printf("登录已提交：账号=%s，客户端 PID=%d。\n", username, pid)
	return nil
}

func resolveConfigPath(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return filepath.Abs(explicit)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(executable), "public-client.json"), nil
}

func loadConnectorConfig(path string) (connectorConfig, error) {
	var cfg connectorConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("读取公网客户端配置 %s：%w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("解析公网客户端配置：%w", err)
	}
	if cfg.SchemaVersion != 1 {
		return cfg, errors.New("public-client.json 的 schemaVersion 必须是 1")
	}
	cfg.ClientDirectory = strings.TrimSpace(cfg.ClientDirectory)
	cfg.SSHTarget = strings.TrimSpace(cfg.SSHTarget)
	cfg.RemoteControlPath = strings.TrimSpace(cfg.RemoteControlPath)
	cfg.RegionName = strings.TrimSpace(cfg.RegionName)
	if cfg.ClientDirectory == "" {
		return cfg, errors.New("请先在 public-client.json 填写 clientDirectory")
	}
	if !sshTargetPattern.MatchString(cfg.SSHTarget) {
		return cfg, errors.New("sshTarget 格式无效，应为 user@公网地址或 SSH 配置别名")
	}
	if !remotePathPattern.MatchString(cfg.RemoteControlPath) {
		return cfg, errors.New("remoteControlPath 必须是无空格的 Linux 绝对路径")
	}
	if cfg.RegionID < 1 || cfg.RegionID >= 250 {
		return cfg, errors.New("regionId 必须在 1 到 249 之间")
	}
	return cfg, nil
}

func ensureTunnel(ssh, target string) (*exec.Cmd, error) {
	if connection, err := net.DialTimeout("tcp", "127.0.0.1:7001", 300*time.Millisecond); err == nil {
		_ = connection.Close()
		return nil, nil
	}
	cmd := exec.Command(ssh,
		"-N", "-T", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30", "-L", "127.0.0.1:7001:127.0.0.1:7001", target,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 SSH 转发：%w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if connection, err := net.DialTimeout("tcp", "127.0.0.1:7001", 300*time.Millisecond); err == nil {
			_ = connection.Close()
			return cmd, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	return nil, fmt.Errorf("SSH 转发未就绪：%s", strings.TrimSpace(stderr.String()))
}

func readCredentials() (string, string, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("账号：")
	username, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return "", "", errors.New("账号不能为空")
	}
	fmt.Print("密码：")
	handle, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return "", "", err
	}
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return "", "", err
	}
	if err := windows.SetConsoleMode(handle, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return "", "", err
	}
	password, readErr := reader.ReadString('\n')
	restoreErr := windows.SetConsoleMode(handle, mode)
	fmt.Println()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return "", "", readErr
	}
	if restoreErr != nil {
		return "", "", restoreErr
	}
	password = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
	if password == "" {
		return "", "", errors.New("密码不能为空")
	}
	return username, password, nil
}

func findClientExecutable(directory string) (string, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"DNF.exe", "NoPack.exe"} {
		path := filepath.Join(directory, name)
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("客户端目录中没有 DNF.exe 或 NoPack.exe：%s", directory)
}

func bindRemoteAccount(ssh string, cfg connectorConfig, username, password string, pid int) error {
	cmd := exec.Command(ssh, "-T", "-o", "BatchMode=yes", cfg.SSHTarget,
		cfg.RemoteControlPath, "account", "bind-client", "--username", username,
		"--pid", strconv.Itoa(pid), "--password-stdin",
	)
	cmd.Stdin = strings.NewReader(password + "\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("服务端账号校验失败：%s", strings.TrimSpace(string(output)))
	}
	return nil
}

func mergeEnvironment(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, overridden := overrides[strings.ToUpper(key)]; overridden {
				continue
			}
		}
		result = append(result, entry)
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}
