package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

func summarizeControlError(output string, commandErr error) string {
	output = strings.TrimSpace(output)
	if output == "" && commandErr != nil {
		output = commandErr.Error()
	}
	output = strings.ReplaceAll(output, "\r", " ")
	output = strings.ReplaceAll(output, "\n", " ")
	output = strings.Join(strings.Fields(output), " ")
	if localized := localizeControlError(output); localized != "" {
		return localized
	}
	return "操作失败，请检查服务配置和运行日志。"
}

func localizeControlError(detail string) string {
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "this username is already registered"):
		return "该账号已注册，请直接登录或更换账号名。"
	case strings.Contains(lower, "username or password is incorrect"):
		return "账号或密码错误，请重新输入。"
	case strings.Contains(lower, "this account is disabled"):
		return "该账号已被停用。"
	case strings.Contains(lower, "username must contain 3 to 32 characters"):
		return "账号长度必须为 3 到 32 个字符。"
	case strings.Contains(lower, "username may contain only"):
		return "账号只能使用字母、数字、下划线、短横线或句点。"
	case strings.Contains(lower, "password must contain 8 to 72"):
		return "密码长度必须为 8 到 72 个 UTF-8 字节。"
	case strings.Contains(lower, "password must be valid utf-8"):
		return "密码包含无效字符，请重新输入。"
	case strings.Contains(lower, "password must not contain control characters"):
		return "密码不能包含控制字符。"
	case strings.Contains(lower, "client directory is not configured"):
		return "尚未配置游戏客户端目录，请检查 runtime/config/instance.json 中的 client.directory。"
	case strings.Contains(lower, "client directory does not exist:"):
		return "游戏客户端目录不存在：" + detailAfter(detail, "client directory does not exist:")
	case strings.Contains(lower, "client file is missing; expected one of:"):
		return "游戏客户端文件缺失，需要以下文件之一：" + detailAfter(detail, "client file is missing; expected one of:")
	case strings.Contains(lower, "dnf.exe or nopack.exe was not found"):
		return "游戏客户端目录中找不到 DNF.exe 或 NoPack.exe。"
	case strings.Contains(lower, "dnf90 server is not running"), strings.Contains(lower, "server is not running"):
		return "本地游戏服务尚未启动，请先启动服务。"
	case strings.Contains(lower, "dnf90 server is not ready"), strings.Contains(lower, "server is not ready"):
		return "本地游戏服务尚未就绪，请稍后重试。"
	case strings.Contains(lower, "requires elevation"),
		strings.Contains(lower, "requested operation requires elevation"),
		strings.Contains(lower, "error 740"):
		return "游戏客户端要求管理员权限。请关闭本登录器，再右键“一键登录.bat”选择“以管理员身份运行”。"
	case strings.Contains(lower, "virus or potentially unwanted software"),
		strings.Contains(lower, "operation did not complete successfully because the file contains a virus"),
		strings.Contains(lower, "error 225"):
		return "游戏客户端被Windows安全中心或杀毒软件拦截。请检查保护历史记录，确认文件可信后恢复，并把DNF客户端目录加入排除项。"
	case strings.Contains(lower, "access is denied"), strings.Contains(lower, "permission denied"):
		return "Windows拒绝启动游戏客户端。请以管理员身份运行“一键登录.bat”，并检查杀毒软件是否隔离了DNF.exe或补丁DLL。"
	case strings.Contains(lower, "not a valid win32 application"),
		strings.Contains(lower, "exec format error"),
		strings.Contains(lower, "error 193"):
		return "游戏客户端EXE格式无效或文件已损坏，请换用完整的23.4.15.0客户端。"
	case strings.Contains(lower, "start client:"):
		return "启动游戏客户端失败。Windows返回：" + detailAfter(detail, "start client:")
	case strings.Contains(lower, "timed out"), strings.Contains(lower, "deadline exceeded"):
		return "操作超时，请检查本地服务状态后重试。"
	case (strings.Contains(lower, "connection refused") || strings.Contains(lower, "actively refused")) && !strings.Contains(lower, "mysql"):
		return "本地服务连接失败，请确认服务已经启动。"
	case strings.Contains(lower, "additional client pid") && strings.Contains(lower, "exited"):
		return "游戏客户端启动后提前退出，请检查客户端目录中的 DNF.exe、补丁 DLL 和安全软件拦截记录。"
	case strings.Contains(lower, "mysql is not reachable"),
		strings.Contains(lower, "mysql schema"),
		strings.Contains(lower, "database initialization failed"),
		strings.Contains(lower, "database connection failed"):
		return "数据库服务异常，请检查 MySQL 状态和运行日志。"
	case strings.Contains(lower, "preflight failed"), strings.Contains(lower, "runtime asset"), strings.Contains(lower, "manifest"):
		return "启动前检查失败，请检查运行文件和配置。"
	case strings.Contains(lower, "cannot find"), strings.Contains(lower, "not found"), strings.Contains(lower, "system cannot find the file"):
		return "所需程序或文件不存在，请检查安装目录。"
	}
	if containsChinese(detail) {
		const maxCharacters = 280
		if utf8.RuneCountInString(detail) > maxCharacters {
			runes := []rune(detail)
			detail = string(runes[len(runes)-maxCharacters:])
		}
		return "操作失败：" + detail
	}
	return ""
}

func detailAfter(detail, marker string) string {
	index := strings.Index(strings.ToLower(detail), strings.ToLower(marker))
	if index < 0 {
		return ""
	}
	return strings.TrimSpace(detail[index+len(marker):])
}

func containsChinese(value string) bool {
	for _, character := range value {
		if unicode.Is(unicode.Han, character) {
			return true
		}
	}
	return false
}
