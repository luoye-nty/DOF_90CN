//go:build windows

package main

import "syscall"

func prepareConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	_, _, _ = kernel32.NewProc("SetConsoleCP").Call(65001)
	_, _, _ = kernel32.NewProc("SetConsoleOutputCP").Call(65001)
}
