//go:build windows

package dnfbridge

import (
	"net"
	"net/netip"
	"os"
	"testing"
)

func TestLocalUDPEndpointOwnerPID(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.UDPAddr)
	endpoint := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(local.Port))
	pid, found := localUDPEndpointOwnerPID(endpoint)
	if !found || pid != uint32(os.Getpid()) {
		t.Fatalf("UDP owner pid=%d found=%t want=%d endpoint=%v", pid, found, os.Getpid(), endpoint)
	}
}
