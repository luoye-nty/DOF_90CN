package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dnf90ops/internal/config"
)

// TestRedactSecretsMasksCredentials 脱敏必须真的抹掉凭据类内容，
// 且不能把普通日志误伤成大花脸（否则日志就失去排查价值）。
func TestRedactSecretsMasksCredentials(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		leak    []string // 脱敏后不允许再出现的内容
		keep    []string // 脱敏后必须保留的内容
		changed bool
	}{
		{
			name:    "adminToken",
			input:   "read instance.json adminToken=adm_1bd03046463540efca525a194d22f7e6 ok",
			leak:    []string{"adm_1bd03046463540efca525a194d22f7e6"},
			keep:    []string{"instance.json", "adminToken"},
			changed: true,
		},
		{
			name:    "mysql DSN",
			input:   `mysql_dsn = "root:aa123456@tcp(127.0.0.1:13306)/dnf_local?parseTime=true"`,
			leak:    []string{"aa123456"},
			keep:    []string{"127.0.0.1:13306", "dnf_local", "@tcp("},
			changed: true,
		},
		{
			name:    "json password field",
			input:   `{"host":"127.0.0.1","password":"aa123456","dbname":"dnf_local"}`,
			leak:    []string{"aa123456"},
			keep:    []string{"127.0.0.1", "dnf_local"},
			changed: true,
		},
		{
			name:    "pairing code",
			input:   `配对码已生成 pairing_code_plain: "A1B2C3D4E5F60718"`,
			leak:    []string{"A1B2C3D4E5F60718"},
			keep:    []string{"配对码已生成"},
			changed: true,
		},
		{
			name:    "internal token long hex",
			input:   `internal token f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff00112233445566778899aabbccddeeff used`,
			leak:    []string{"f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff00112233445566778899aabbccddeeff"},
			keep:    []string{"internal token", "used"},
			changed: true,
		},
		{
			name:    "url query token",
			input:   "GET https://127.0.0.1:18113/manage/v1/status?token=abcdef123456&limit=10",
			leak:    []string{"abcdef123456"},
			keep:    []string{"limit=10", "/manage/v1/status"},
			changed: true,
		},
		{
			name:    "大小写变体的 Password",
			input:   `{"Password":"aa123456","DbName":"dnf_local"}`,
			leak:    []string{"aa123456"},
			keep:    []string{"dnf_local"},
			changed: true,
		},
		{
			name:    "大写 TICKET",
			input:   "TICKET=ABCDEF123456 已签发",
			leak:    []string{"ABCDEF123456"},
			keep:    []string{"已签发"},
			changed: true,
		},
		{
			name:    "大写 SECRET 与 Pwd",
			input:   "SECRET=zzz999 Pwd=qqq888",
			leak:    []string{"zzz999", "qqq888"},
			changed: true,
		},
		{
			name:    "救援代码 rescue_code",
			input:   "rescue_code=482913 已设置",
			leak:    []string{"482913"},
			keep:    []string{"已设置"},
			changed: true,
		},
		{
			name:    "账号ID必须保留（不是凭据，是排查用的关联键）",
			input:   "CDK 已自动导出 batch=1 account_id=dnf:270b488b79c558606e0df3654bf14e0b1bd03046463540efca525a194d22f7e ok",
			leak:    []string{redactMask},
			keep:    []string{"dnf:270b488b79c558606e0df3654bf14e0b1bd03046463540efca525a194d22f7e"},
			changed: false, // 这一行没有可抹的凭据，账号 ID 被保留 → 输出与输入相同
		},
		{
			name:    "普通日志不应被改动",
			input:   "[ops] started pid=4242, exe=DNF90Server.exe, workdir=runtime",
			keep:    []string{"[ops] started pid=4242", "DNF90Server.exe", "workdir=runtime"},
			changed: false,
		},
		{
			name:    "频道日志不应被改动",
			input:   "已关闭频道 12,13；当前启用 116 / 共 118",
			keep:    []string{"已关闭频道 12,13", "116", "118"},
			changed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.input)
			for _, leak := range tc.leak {
				if strings.Contains(got, leak) {
					t.Fatalf("脱敏后仍包含机密内容 %q：%s", leak, got)
				}
			}
			for _, keep := range tc.keep {
				if !strings.Contains(got, keep) {
					t.Fatalf("脱敏后丢失了应保留的内容 %q：%s", keep, got)
				}
			}
			if tc.changed && got == tc.input {
				t.Fatalf("该行应当被改动但没有：%s", got)
			}
			if !tc.changed && got != tc.input {
				t.Fatalf("该行不该被改动却被改成了：%s", got)
			}
		})
	}
}

// TestFileLoggerWritesAndRedacts 落盘日志要真的写文件，并且写盘内容已脱敏。
func TestFileLoggerWritesAndRedacts(t *testing.T) {
	base := t.TempDir()
	logger := newFileLogger(&config.LogFileConfig{
		Enabled: true, Dir: "logs", MaxFileMB: 1, KeepFiles: 5, Redact: true,
	}, base)
	if logger == nil {
		t.Fatal("fileLogger 创建失败")
	}
	defer logger.Close()

	logger.Write("INFO", "启动完成 adminToken=adm_deadbeefdeadbeefdeadbeef")
	logger.Write("INFO", "普通一行日志 没有任何凭据")

	raw, err := os.ReadFile(logger.Path())
	if err != nil {
		t.Fatalf("读日志文件: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "adm_deadbeefdeadbeefdeadbeef") {
		t.Fatalf("日志文件里仍残留 adminToken：%s", text)
	}
	if !strings.Contains(text, "普通一行日志 没有任何凭据") {
		t.Fatalf("日志文件缺少普通行：%s", text)
	}
	if !strings.Contains(text, "[INFO]") {
		t.Fatalf("日志行缺少级别标记：%s", text)
	}
	if logger.Size() == 0 {
		t.Fatal("Size() 报告 0，与文件实际大小不符")
	}
}

// TestFileLoggerRotatesAndPrunes 超限轮转后必须只保留最近 KeepFiles 个文件。
func TestFileLoggerRotatesAndPrunes(t *testing.T) {
	base := t.TempDir()
	// MaxFileMB 最小按 1MB 计；直接灌到超过 1MB 会拖慢用例，
	// 所以这里手工把 maxBytes 调小后验证轮转与清理逻辑本身。
	logger := newFileLogger(&config.LogFileConfig{
		Enabled: true, Dir: "logs", MaxFileMB: 1, KeepFiles: 2, Redact: false,
	}, base)
	if logger == nil {
		t.Fatal("fileLogger 创建失败")
	}
	defer logger.Close()
	logger.mu.Lock()
	logger.maxBytes = 512
	logger.mu.Unlock()

	for i := 0; i < 40; i++ {
		logger.Write("INFO", strings.Repeat("x", 100))
	}
	entries, err := os.ReadDir(logger.Dir())
	if err != nil {
		t.Fatalf("读日志目录: %v", err)
	}
	files := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "ops-") {
			files++
		}
	}
	if files == 0 {
		t.Fatal("没有生成任何日志文件")
	}
	if files > 2 {
		t.Fatalf("保留文件数 %d 超过 KeepFiles=2", files)
	}
	if logger.Size() > 512 {
		t.Fatalf("当前文件大小 %d 超过 maxBytes=512，轮转没生效", logger.Size())
	}
}

// TestLogFileConfigDefaultsSurviveLoad 旧配置（没有 log_file / memory_clean 段）加载后必须有默认值。
func TestLogFileConfigDefaultsSurviveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	legacy := `{"server_root":"","database_mode":"mysql","registration_mode":"open",
		"mysql":{"host":"127.0.0.1","port":13306,"user":"root","dbname":"dnf_local"},
		"public":{"listen_addr":":18112"},"internal":{"listen_addr":"127.0.0.1:19091"},
		"dnfbridge":{"status_port":7001},"log_max_lines":2000}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("写旧配置: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogFile == nil || !cfg.LogFile.Enabled {
		t.Fatalf("旧配置加载后 log_file 未补默认（应为启用）：%+v", cfg.LogFile)
	}
	if cfg.LogFile.MaxFileMB <= 0 || cfg.LogFile.KeepFiles <= 0 {
		t.Fatalf("log_file 数值字段未补默认：%+v", cfg.LogFile)
	}
	if cfg.MemoryClean == nil || cfg.MemoryClean.IntervalMinutes <= 0 {
		t.Fatalf("旧配置加载后 memory_clean 未补默认：%+v", cfg.MemoryClean)
	}
	if cfg.MemoryClean.Enabled {
		t.Fatalf("memory_clean 默认应为关闭（不擅自改动服务端进程）：%+v", cfg.MemoryClean)
	}
}

// TestHumanBytes 单位换算（日志里到处在用，错了会误导排查）。
func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:          "0B",
		512:        "512B",
		2048:       "2.0KB",
		5 << 20:    "5.0MB",
		3 << 30:    "3.0GB",
		-(1 << 20): "0B",
	}
	for input, want := range cases {
		if got := humanBytes(input); got != want {
			t.Fatalf("humanBytes(%d) = %s，期望 %s", input, got, want)
		}
	}
}
