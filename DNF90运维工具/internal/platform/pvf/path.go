// 来源: DNF90GM工具_独立版 vendor/longheng.io/server/internal (platform/pvf) 于 2026-09-06 复制。
// 维护纪律: 以 GM 工具源码为唯一事实源; 同步升级时逐文件对比; import path 保持本端 module 内部路径。
package pvf

import (
	"path"
	"strings"
)

func normalizePath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	for strings.HasPrefix(value, "./") || strings.HasPrefix(value, "/") {
		value = strings.TrimPrefix(value, "./")
		value = strings.TrimPrefix(value, "/")
	}
	if value == "" {
		return ""
	}
	clean := strings.TrimSuffix(path.Clean(value), "/")
	if clean == "." {
		return ""
	}
	return clean
}

func pathKey(value string) string {
	return strings.ToLower(normalizePath(value))
}

func joinArchivePath(dir, name string) string {
	if strings.TrimSpace(dir) == "" {
		return normalizePath(name)
	}
	if strings.TrimSpace(name) == "" {
		return normalizePath(dir)
	}
	return normalizePath(dir + "/" + name)
}
