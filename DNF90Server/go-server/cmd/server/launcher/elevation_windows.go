//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"longheng.io/server/internal/platform/wineenv"
)

const swShowNormal = 1

var (
	shell32           = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
)

// ensureLauncherElevation keeps the BAT entrypoint simple while still
// allowing CreateProcess to start DNF clients that Windows marks as requiring
// elevation. ShellExecuteW with the runas verb is the native UAC boundary; the
// original unelevated launcher exits after the elevated copy is accepted.
func ensureLauncherElevation(args []string) (bool, error) {
	// Wine has no native Windows UAC boundary. Some versions report a
	// non-elevated token but either reject or recursively relaunch the "runas"
	// verb. The Wine process already has the Unix user's filesystem rights, so
	// continue with those rights and let the normal path/file checks enforce the
	// project boundary.
	if wineenv.Detected() {
		return false, nil
	}
	var token windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_QUERY,
		&token,
	); err != nil {
		return false, fmt.Errorf("检查管理员权限失败：%w", err)
	}
	defer token.Close()
	if token.IsElevated() {
		return false, nil
	}

	executable, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("定位登录器失败：%w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return false, fmt.Errorf("解析登录器路径失败：%w", err)
	}
	workingDirectory := filepath.Dir(executable)
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return false, err
	}
	file, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return false, err
	}
	directory, err := windows.UTF16PtrFromString(workingDirectory)
	if err != nil {
		return false, err
	}
	var parameters *uint16
	if len(args) != 0 {
		escaped := make([]string, 0, len(args))
		for _, arg := range args {
			escaped = append(escaped, syscall.EscapeArg(arg))
		}
		parameters, err = windows.UTF16PtrFromString(strings.Join(escaped, " "))
		if err != nil {
			return false, err
		}
	}

	result, _, callErr := procShellExecuteW.Call(
		uintptr(unsafe.Pointer(parameters)),
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		0,
		uintptr(unsafe.Pointer(directory)),
		swShowNormal,
	)
	if result <= 32 {
		if callErr != nil && callErr != windows.ERROR_SUCCESS {
			return false, fmt.Errorf("无法取得管理员权限（请在UAC窗口中选择“是”）：%w", callErr)
		}
		return false, fmt.Errorf("无法取得管理员权限（ShellExecute返回%d），请右键“一键登录.bat”选择“以管理员身份运行”", result)
	}
	return true, nil
}
