package main

// clientLogf 客户端诊断日志 —— 2026-09-28 加，同日晚按要求**停用**。
//
// 现状：函数体是空的（什么都不写）。所有调用点（api_client.go 的请求耗时、
// reg_admin_tab.go 的注册码刷新过程）都保留着，所以：
//   - 平时：不产生任何日志文件、没有任何副作用；
//   - 要排障时：把下面 /* */ 里的实现放回来、并恢复文件头的 import 即可，调用点一行都不用动。
//
// 原实现位置：<管理工具.exe 同目录>\logs\client-YYYYMMDD.log（超过 8MB 自动转 .1）。
//
// 为什么当初加它：管理工具原来什么都不落盘，界面卡在"注册码：正在查询中"时，
// 服务端日志全绿（实测同一条请求 47ms 返回 21 枚注册码），只能靠猜；
// 有了这个文件就能一眼看出卡在哪一步（取证书指纹 / 业务请求 / 还是压根没发出去）。

/*
import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var clientLogMu sync.Mutex

func clientLogPath() string {
	dir := filepath.Join(filepath.Dir(DefaultConfigPath()), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return filepath.Join(dir, "client-"+time.Now().Format("20060102")+".log")
}

func clientLogf(format string, args ...any) {
	clientLogMu.Lock()
	defer clientLogMu.Unlock()

	p := clientLogPath()
	if p == "" {
		return
	}
	if fi, err := os.Stat(p); err == nil && fi.Size() > 8<<20 {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	msg := fmt.Sprintf(format, args...)
	_, _ = f.WriteString("[" + time.Now().Format("2006-01-02 15:04:05.000") + "] " + msg + "\r\n")
}

// ClientLogPathForUI 给界面/提示用：当前日志文件路径（可能为空）。
func ClientLogPathForUI() string { return clientLogPath() }
*/

// clientLogf 空实现：调用点在，行为是"什么都不做"（要恢复日志只需放开上面的实现）。
func clientLogf(format string, args ...any) {}
