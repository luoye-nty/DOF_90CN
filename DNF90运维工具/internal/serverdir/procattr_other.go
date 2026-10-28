//go:build !windows

package serverdir

import "syscall"

// hideWindowAttr：非 Windows 平台没有"隐藏控制台窗口"这回事，保持默认行为（nil）。
// 这两个函数与 procattr_windows.go 成对出现，目的是让本包能在 Linux 上编译
// （cmd/serve → dnf90auth 的交叉编译依赖它）。
func hideWindowAttr() *syscall.SysProcAttr {
	return nil
}

// hideWindowNewGroupAttr：对应 Windows 的 CREATE_NEW_PROCESS_GROUP ——
// 让子进程自成进程组（Setpgid），父进程退出/收到信号时不会把它一起带走。
func hideWindowNewGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
