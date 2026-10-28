package main

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSummarizeControlErrorUsesBoundedSingleLine(t *testing.T) {
	got := summarizeControlError(
		"first line\r\n"+strings.Repeat("错误", 200),
		errors.New("exit status 1"),
	)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("summary contains a newline: %q", got)
	}
	if utf8.RuneCountInString(got) > 290 {
		t.Fatalf("summary is not bounded: %d runes", utf8.RuneCountInString(got))
	}
}

func TestSummarizeControlErrorFallsBackToCommandError(t *testing.T) {
	got := summarizeControlError("", errors.New("exit status 1"))
	if got != "操作失败，请检查服务配置和运行日志。" {
		t.Fatalf("summary = %q", got)
	}
}

func TestSummarizeControlErrorLocalizesCommonLauncherFailures(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "bad credentials", output: "FAILED: username or password is incorrect", want: "账号或密码错误，请重新输入。"},
		{name: "duplicate account", output: "FAILED: this username is already registered", want: "该账号已注册，请直接登录或更换账号名。"},
		{name: "missing client DLL", output: "FAILED: client file is missing; expected one of: 90CN.dll", want: "游戏客户端文件缺失，需要以下文件之一：90CN.dll"},
		{name: "service not ready", output: "FAILED: DNF90 server is not ready; run control start first", want: "本地游戏服务尚未就绪，请稍后重试。"},
		{name: "timeout", output: "FAILED: context deadline exceeded", want: "操作超时，请检查本地服务状态后重试。"},
		{name: "client needs elevation", output: "FAILED: start client: The requested operation requires elevation.", want: "游戏客户端要求管理员权限。请关闭本登录器，再右键“一键登录.bat”选择“以管理员身份运行”。"},
		{name: "client blocked by antivirus", output: "FAILED: start client: Operation did not complete successfully because the file contains a virus or potentially unwanted software.", want: "游戏客户端被Windows安全中心或杀毒软件拦截。请检查保护历史记录，确认文件可信后恢复，并把DNF客户端目录加入排除项。"},
		{name: "client access denied", output: "FAILED: start client: Access is denied.", want: "Windows拒绝启动游戏客户端。请以管理员身份运行“一键登录.bat”，并检查杀毒软件是否隔离了DNF.exe或补丁DLL。"},
		{name: "client invalid executable", output: "FAILED: start client: %1 is not a valid Win32 application.", want: "游戏客户端EXE格式无效或文件已损坏，请换用完整的23.4.15.0客户端。"},
		{name: "client other start error", output: "FAILED: start client: The system cannot execute the specified program.", want: "启动游戏客户端失败。Windows返回：The system cannot execute the specified program."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := summarizeControlError(test.output, errors.New("exit status 1")); got != test.want {
				t.Fatalf("summary = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSummarizeControlErrorDoesNotMislabelClientExitAsDatabaseFailure(t *testing.T) {
	output := "Database: portable root@127.0.0.1:13306/dnf_local FAILED: additional client PID 1234 exited during the 5-second startup check"
	got := summarizeControlError(output, errors.New("exit status 1"))
	if !strings.Contains(got, "\u63d0\u524d\u9000\u51fa") {
		t.Fatalf("client exit summary = %q", got)
	}
	if strings.Contains(got, "\u6570\u636e\u5e93") {
		t.Fatalf("client exit was mislabeled as database failure: %q", got)
	}
}

func TestSummarizeControlErrorRecognizesRealMySQLFailure(t *testing.T) {
	got := summarizeControlError("FAILED: mysql is not reachable: connection refused", errors.New("exit status 1"))
	if !strings.Contains(got, "\u6570\u636e\u5e93") {
		t.Fatalf("mysql summary = %q", got)
	}
}
