package dnfbridge

import (
	"path/filepath"
	"strings"
)

// currentGameClientImageName is the executable that owns the game sockets of
// the current client. Launcher discovery probes and the update hub connect to
// the same listeners from their own processes and must keep the probe
// bootstrap; only this image may inherit the instance account without a
// launcher registration.
const currentGameClientImageName = "dnf.exe"

// isCurrentGameClientImage reports whether a process image path names the
// current game client executable.
func isCurrentGameClientImage(imagePath string) bool {
	imagePath = strings.TrimSpace(imagePath)
	if imagePath == "" {
		return false
	}
	base := filepath.Base(strings.ReplaceAll(imagePath, "\\", "/"))
	return strings.EqualFold(base, currentGameClientImageName)
}
