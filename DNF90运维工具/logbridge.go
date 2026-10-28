package main

// DNF90 运维工具 —— 把标准库 slog 的输出接进工具日志（免费版/赞助版共用，无 build tag）。
//
// 为什么需要这个文件：
//
//	认证服务（internal/server）、远程管理服务（internal/manage）、备份（internal/backup）
//	都是用 log/slog 打日志的。slog.Default() 默认写 os.Stderr，而本工具是
//	`-H windowsgui` 构建、**没有 console**，stderr 直接丢弃 —— 也就是说这些模块的
//	Info/Warn/Error 一行都到不了 GUI，更到不了落盘文件。
//	排查"服务起不来""配对码登录被拒""备份失败"时，缺的恰好就是这些行。
//
// 这里装一个自定义 Handler，把 slog 记录转成一行文本喂给 App.logf：
// 于是它自动获得 GUI 显示 + 落盘 + 脱敏 + 崩溃转储附带的全部能力。
//
// 注意不要把 logf 再打回 slog（本文件不这样做），否则会形成回环。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// appLogHandler 把 slog.Record 转成 App.logf 的一行文本。
type appLogHandler struct {
	app *App
	// 预置属性（With 累积）与分组前缀，遵循 slog.Handler 语义。
	mu     sync.Mutex
	attrs  []slog.Attr
	groups []string
}

// newAppLogHandler 构造指向该 App 的 slog 处理器。
func newAppLogHandler(app *App) slog.Handler {
	return &appLogHandler{app: app}
}

func (h *appLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

// WithAttrs 复制一份并追加属性（slog 要求 Handler 不可变式使用）。
func (h *appLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := &appLogHandler{app: h.app}
	h.mu.Lock()
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	next.groups = append([]string(nil), h.groups...)
	h.mu.Unlock()
	return next
}

// WithGroup 进入一个属性分组。
func (h *appLogHandler) WithGroup(name string) slog.Handler {
	name = strings.TrimSpace(name)
	if name == "" {
		return h
	}
	next := &appLogHandler{app: h.app}
	h.mu.Lock()
	next.attrs = append([]slog.Attr(nil), h.attrs...)
	next.groups = append(append([]string(nil), h.groups...), name)
	h.mu.Unlock()
	return next
}

// Handle 把一条记录渲染成 "[LEVEL] msg key=value ..." 交给 App.logf。
func (h *appLogHandler) Handle(_ context.Context, record slog.Record) error {
	if h == nil || h.app == nil {
		return nil
	}
	var builder strings.Builder
	builder.WriteString("[")
	builder.WriteString(slogLevelText(record.Level))
	builder.WriteString("] ")
	builder.WriteString(record.Message)

	h.mu.Lock()
	prefix := ""
	if len(h.groups) > 0 {
		prefix = strings.Join(h.groups, ".") + "."
	}
	preAttrs := append([]slog.Attr(nil), h.attrs...)
	h.mu.Unlock()

	for _, attr := range preAttrs {
		appendSlogAttr(&builder, prefix, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		appendSlogAttr(&builder, prefix, attr)
		return true
	})

	h.app.logf("%s", builder.String())
	return nil
}

// appendSlogAttr 按 key=value 追加一个属性；分组属性展开成 group.key 形式。
func appendSlogAttr(builder *strings.Builder, prefix string, attr slog.Attr) {
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		group := value.Group()
		if len(group) == 0 {
			return
		}
		next := prefix + attr.Key + "."
		if attr.Key == "" {
			next = prefix
		}
		for _, item := range group {
			appendSlogAttr(builder, next, item)
		}
		return
	}
	if attr.Equal(slog.Attr{}) {
		return
	}
	builder.WriteByte(' ')
	builder.WriteString(prefix)
	builder.WriteString(attr.Key)
	builder.WriteByte('=')
	builder.WriteString(slogValueText(value))
}

// slogValueText 按类型渲染属性值；字符串含空格时加引号，避免日志里分不清字段边界。
func slogValueText(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		text := value.String()
		if strings.ContainsAny(text, " \t") {
			// 刻意不用 %q：Windows 路径会被转义成 C:\\Program Files\\dnf，日志里很难读。
			// 只把内嵌的双引号换成单引号，保证引号成对即可。
			return `"` + strings.ReplaceAll(text, `"`, `'`) + `"`
		}
		return text
	case slog.KindTime:
		return value.Time().Format("2006-01-02 15:04:05.000")
	case slog.KindDuration:
		return value.Duration().String()
	default:
		return fmt.Sprint(value.Any())
	}
}

// slogLevelText 日志级别短名（GUI 与文件都按这个前缀显示，便于 grep）。
func slogLevelText(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN"
	case level >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}
