//go:build !windows

package dnfbridge

import "net"

func localTCPConnectionOwnerPID(net.Conn) (uint32, bool) {
	return 0, false
}

func localProcessImagePath(uint32) (string, bool) {
	return "", false
}

func clientAccountProcessLineage(pid uint32) []uint32 {
	if pid == 0 {
		return nil
	}
	return []uint32{pid}
}
