//go:build !windows

package dnfbridge

import "net/netip"

func localUDPEndpointOwnerPID(netip.AddrPort) (uint32, bool) {
	return 0, false
}
