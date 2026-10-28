package main

import (
	"path"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// readItemText 复刻 dnfbridge.readDungeonDropText 的候选顺序：原样路径 →
// 按 root 前缀 → (r) 修订名回退。
func readItemText(archive *platformpvf.Archive, root, listed string) (string, string, error) {
	clean := cleanPath(listed)
	candidates := []string{clean}
	if !strings.HasPrefix(strings.ToLower(clean), strings.ToLower(root)+"/") {
		candidates = append(candidates, path.Join(root, clean))
	}
	for _, candidate := range append([]string(nil), candidates...) {
		dir, base := path.Split(candidate)
		if base == "" || strings.HasPrefix(strings.ToLower(base), "(r)") {
			continue
		}
		candidates = append(candidates, dir+"(r)"+base)
	}
	var lastErr error
	for _, candidate := range candidates {
		text, err := archive.ReadText(candidate)
		if err == nil {
			return candidate, text, nil
		}
		lastErr = err
	}
	return "", "", lastErr
}

func cleanPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = strings.TrimPrefix(value, "./")
	return strings.TrimPrefix(value, "/")
}

// normalizeType 与 normalizeMagicBoxPVFType 保持一致。
func normalizeType(value string) string {
	return strings.ToLower(strings.Trim(value, "` []"))
}
