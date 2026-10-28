//go:build windows

// Package wineenv reports whether the current Windows process is hosted by
// Wine. Callers use it only to select compatibility paths for Win32 APIs whose
// Wine implementations intentionally differ from native Windows.
package wineenv

import (
	"sync"

	"golang.org/x/sys/windows"
)

var (
	detectOnce sync.Once
	detected   bool
)

// Detected reports whether ntdll exports Wine's documented runtime marker.
// Native Windows does not export wine_get_version, so no environment variable
// or user-controlled configuration can accidentally enable this path.
func Detected() bool {
	detectOnce.Do(func() {
		ntdll := windows.NewLazySystemDLL("ntdll.dll")
		detected = ntdll.NewProc("wine_get_version").Find() == nil
	})
	return detected
}
