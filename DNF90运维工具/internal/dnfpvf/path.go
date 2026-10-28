// 来源: DNF90GM工具_独立版 vendor/longheng.io/server/internal (modules/dnf/pvf) 于 2026-09-06 复制。
// 维护纪律: 以 GM 工具源码为唯一事实源; 同步升级时逐文件对比; import path 保持本端 module 内部路径。
package pvf

import (
	"path"
	"strings"
)

func cleanPath(value string) string {
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
	return strings.ToLower(cleanPath(value))
}

func sectionKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func matchPrefix(filePath string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return false
	}
	key := pathKey(filePath)
	for _, prefix := range prefixes {
		prefixKey := pathKey(prefix)
		if prefixKey == "" {
			continue
		}
		if key == prefixKey || strings.HasPrefix(key, prefixKey+"/") {
			return true
		}
	}
	return false
}
