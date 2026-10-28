//go:build !paid

package main

// verifyLicenseGate 免费版：不带在线授权逻辑，直接放行进入主界面。
func verifyLicenseGate(a *App) error {
	return nil
}
