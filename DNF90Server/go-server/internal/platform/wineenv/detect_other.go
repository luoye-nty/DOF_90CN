//go:build !windows

package wineenv

// Detected is false for native non-Windows builds. Running a Windows build
// through Wine selects the windows implementation instead.
func Detected() bool {
	return false
}
