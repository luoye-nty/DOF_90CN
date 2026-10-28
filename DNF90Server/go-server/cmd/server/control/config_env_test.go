package main

import (
	"strings"
	"testing"
)

// 控制程序启动服务端时用的是 cmd.Env = runtimeEnvironment(cfg)：它先把继承环境里的
// DNFBRIDGE_* / DNF_* / DFO_* / LONGHENG_DNF_CONFIG 全部剔除，再按 instance.json 拼一份。
//
// 这个测试把两件事钉死（都是排查"环境变量为什么没生效"时最容易踩的）：
//  1. DNFBRIDGE_* 覆盖项在正常启动路径下**不会**传给服务端（例如 DNFBRIDGE_RECORD_DIR：
//     想改记录目录必须用直接运行 DNF90Server.exe 的方式，或用 instance.json）。
//  2. 不匹配这些前缀的变量会原样保留（限购窗口的 DNF90_PURCHASE_LIMIT_CLOCK_OFFSET_HOURS
//     就走这条路——名字里是 DNF90_ 而不是 DNF_，只差一个字符，容易被误删）。
func TestRuntimeEnvironmentScrubsBridgeOverridesButKeepsOthers(t *testing.T) {
	t.Setenv("DNFBRIDGE_RECORD_DIR", `D:\somewhere\records`)
	t.Setenv("DNFBRIDGE_PACKET_LOG_PATH", `logs\other.txt`)
	t.Setenv("DNF_PACKET_LOG", "true")
	t.Setenv("DFO_DEBUG", "1")
	t.Setenv("DNF90_PURCHASE_LIMIT_CLOCK_OFFSET_HOURS", "200")
	t.Setenv("PATH", `C:\Windows\System32`)

	env := runtimeEnvironment(instanceConfig{})
	lookup := func(key string) (string, bool) {
		for _, entry := range env {
			name, value, ok := strings.Cut(entry, "=")
			if ok && strings.EqualFold(name, key) {
				return value, true
			}
		}
		return "", false
	}

	for _, scrubbed := range []string{"DNFBRIDGE_RECORD_DIR", "DNF_PACKET_LOG", "DFO_DEBUG"} {
		if value, found := lookup(scrubbed); found {
			t.Fatalf("%s 不该被传给服务端，实际值=%q（继承环境必须被 instance.json 覆盖）", scrubbed, value)
		}
	}
	// 控制器自己会设置的同名项：保留的是它推导出来的值，继承值必须被顶掉。
	if got, found := lookup("DNFBRIDGE_PACKET_LOG_PATH"); !found || got != "logs/packet_log.txt" {
		t.Fatalf("DNFBRIDGE_PACKET_LOG_PATH=%q found=%t want logs/packet_log.txt（继承的 logs\\other.txt 必须被覆盖）", got, found)
	}
	value, found := lookup("DNF90_PURCHASE_LIMIT_CLOCK_OFFSET_HOURS")
	if !found || value != "200" {
		t.Fatalf("限购窗口时间旅行开关没传下去: found=%t value=%q", found, value)
	}
	if _, found := lookup("PATH"); !found {
		t.Fatal("基础环境（PATH 之类）应当保留")
	}
}
