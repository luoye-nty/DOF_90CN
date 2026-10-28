package main

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// lastLogLine 取工具日志缓冲的最后一行（去掉 logf 加的时间戳前缀）。
func lastLogLine(a *App) string {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	if len(a.logBuf) == 0 {
		return ""
	}
	line := a.logBuf[len(a.logBuf)-1]
	// logf 写入的格式是 "[15:04:05] 正文"，这里只关心正文。
	if idx := strings.Index(line, "] "); idx >= 0 && strings.HasPrefix(line, "[") {
		return line[idx+2:]
	}
	return line
}

// TestSlogBridgeFormatsRecord slog 记录必须变成带级别与 key=value 的单行文本。
func TestSlogBridgeFormatsRecord(t *testing.T) {
	app := &App{}
	handler := newAppLogHandler(app)
	logger := slog.New(handler)

	logger.Info("admin request", "scope", "dnf-gm", "status", 200)
	got := lastLogLine(app)
	for _, want := range []string{"[INFO]", "admin request", "scope=dnf-gm", "status=200"} {
		if !strings.Contains(got, want) {
			t.Fatalf("slog 桥接输出缺少 %q：%s", want, got)
		}
	}

	logger.Error("register failed", "error", "boom")
	got = lastLogLine(app)
	if !strings.Contains(got, "[ERROR]") || !strings.Contains(got, "error=boom") {
		t.Fatalf("ERROR 级记录格式不对：%s", got)
	}
}

// TestSlogBridgeWithAttrsAndGroup With 累积的属性与分组必须保留，且不影响原 handler。
func TestSlogBridgeWithAttrsAndGroup(t *testing.T) {
	app := &App{}
	base := newAppLogHandler(app)

	child := base.WithAttrs([]slog.Attr{slog.String("svc", "manage")}).WithGroup("http")
	if child == base {
		t.Fatal("WithAttrs/WithGroup 应返回新的 handler")
	}
	slog.New(child).Warn("slow", "ms", 1500)
	got := lastLogLine(app)
	for _, want := range []string{"[WARN]", "slow", "svc=manage", "http.ms=1500"} {
		if !strings.Contains(got, want) {
			t.Fatalf("子 handler 输出缺少 %q：%s", want, got)
		}
	}

	// 原 handler 不应被污染。
	slog.New(base).Info("plain")
	got = lastLogLine(app)
	if strings.Contains(got, "svc=manage") {
		t.Fatalf("原 handler 被 WithAttrs 污染了：%s", got)
	}
}

// TestSlogBridgeQuotesSpacedStrings 含空格的字符串要加引号，否则日志里分不清字段边界。
func TestSlogBridgeQuotesSpacedStrings(t *testing.T) {
	app := &App{}
	slog.New(newAppLogHandler(app)).Info("msg", "path", `C:\Program Files\dnf`)
	got := lastLogLine(app)
	if !strings.Contains(got, `path="C:\Program Files\dnf"`) {
		t.Fatalf("含空格的属性值未加引号：%s", got)
	}
}

// TestSlogBridgeRendersTimeAndDuration 时间与时长不能退化成原始结构体打印。
func TestSlogBridgeRendersTimeAndDuration(t *testing.T) {
	app := &App{}
	at := time.Date(2026, 9, 14, 18, 30, 0, 0, time.Local)
	slog.New(newAppLogHandler(app)).Info("tick",
		"at", at, "elapsed", 1500*time.Millisecond)
	got := lastLogLine(app)
	if !strings.Contains(got, "at=2026-09-14 18:30:00.000") {
		t.Fatalf("时间未按预期渲染：%s", got)
	}
	if !strings.Contains(got, "elapsed=1.5s") {
		t.Fatalf("时长未按预期渲染：%s", got)
	}
}
