//go:build windows

package serverdir

import "syscall"

// hideWindowAttr 让被拉起的子进程不弹控制台窗口。
// HideWindow 是 Windows 专有字段，所以这一对函数按平台分成两个文件
// （procattr_windows.go / procattr_other.go），否则 cmd/serve 无法交叉编译到 Linux。
func hideWindowAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true}
}

// hideWindowNewGroupAttr 在 hideWindowAttr 的基础上让子进程自成进程组
// （Windows 的 CREATE_NEW_PROCESS_GROUP）：mysqld 这类常驻进程不该被父进程带走。
func hideWindowNewGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
