package server

// HTTP 服务噪声过滤（2026-09-28）。
//
// 背景：Go 的 http.Server 在 TLS 握手失败时会往 ErrorLog 写
// `http: TLS handshake error from <ip>:<port>: EOF`。而这条日志在本项目里
// **几乎全是自己造成的**：
//
//   - 登录器的"服务器在线探测"是**裸 TCP 连接**（LoginWindow.ProbeRemotePort：
//     TcpClient.ConnectAsync 连上 18112 就断开，根本不发 TLS），每台登录器每 10 秒一次；
//   - 公网扫描器也会对 18112 随手续个 TCP 再断。
//
// 而且它**一定会进工具的输出框**：http.Server.ErrorLog 为空时用标准库 log，
// 而 main.go 里 `slog.SetDefault` 会把标准库 log 桥接进 slog —— 于是这行以
// `[INFO] http: TLS handshake error ...` 的形式被 appLogHandler 喂给 App.logf。
// 每台在线登录器每 10 秒一行，人多起来输出框被彻底刷满，真正的业务日志全被顶掉，
// 而这类行**没有任何可操作性**：探测本来就是这么设计的。
//
// 处理：这类行不再逐条输出，改成**每分钟最多汇总一条**（带次数）。
// 既止住刷屏，又保留了"连接量是不是异常暴涨（有人在猛连/被扫）"这个信号。
// 其余错误原样透传，不做任何吞掉。

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// tlsNoiseSummaryInterval 噪声汇总的最小间隔。
const tlsNoiseSummaryInterval = time.Minute

// tlsNoiseMarker 需要被汇总的日志特征。只匹配这一种，别的错误一律照常输出。
const tlsNoiseMarker = "TLS handshake error"

// httpNoiseWriter 把 http.Server.ErrorLog 的字节流接到 slog 上，并汇总 TLS 握手噪声。
type httpNoiseWriter struct {
	report func(string)

	mu         sync.Mutex
	suppressed int
	lastAt     time.Time
}

// Write 实现 io.Writer（http.Server.ErrorLog 要求）。
// 返回值永远等于 len(p)：调用方是标准库日志，不该因为我们过滤而报错。
func (w *httpNoiseWriter) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	if line == "" {
		return len(p), nil
	}
	if !strings.Contains(line, tlsNoiseMarker) {
		w.report(line)
		return len(p), nil
	}

	w.mu.Lock()
	w.suppressed++
	count := w.suppressed
	due := time.Since(w.lastAt) >= tlsNoiseSummaryInterval
	if due {
		w.lastAt = time.Now()
		w.suppressed = 0
	}
	w.mu.Unlock()

	// 只在到点汇总、且确实积了一批时说。偶尔一条不记 ——
	// 那更可能是真的协议不匹配，不该被淹没在这一条汇总里。
	if due && count > 1 {
		w.report(fmt.Sprintf("已汇总屏蔽 %d 条 TLS 握手噪声（登录器在线探测的裸 TCP 连接 / 公网扫描，每分钟最多记一条）", count))
	}
	return len(p), nil
}

// newHTTPErrorLog 构造 http.Server.ErrorLog 用的 logger。
// report 为 nil 时退化成"只吞噪声、其余丢弃"（测试场景）。
func newHTTPErrorLog(report func(string)) *log.Logger {
	if report == nil {
		report = func(string) {}
	}
	return log.New(&httpNoiseWriter{report: report}, "", 0)
}
