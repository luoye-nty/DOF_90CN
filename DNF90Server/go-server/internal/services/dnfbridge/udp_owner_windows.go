//go:build windows

package dnfbridge

import (
	"encoding/binary"
	"math/bits"
	"net/netip"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const udpTableOwnerPID = 1

var (
	udpIPHelperDLL          = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedUDPTable = udpIPHelperDLL.NewProc("GetExtendedUdpTable")
)

// localUDPEndpointOwnerPID identifies the local DNF.exe that owns a reflected
// probe source port. This disambiguates two clients running on the server PC,
// where both sessions necessarily advertise the same private IPv4 address.
func localUDPEndpointOwnerPID(endpoint netip.AddrPort) (uint32, bool) {
	if !endpoint.IsValid() || !endpoint.Addr().Is4() || endpoint.Port() == 0 {
		return 0, false
	}
	var size uint32
	result, _, _ := procGetExtendedUDPTable.Call(
		0,
		uintptr(unsafe.Pointer(&size)),
		0,
		uintptr(syscall.AF_INET),
		udpTableOwnerPID,
		0,
	)
	if result != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || size < 4 {
		return 0, false
	}
	table := make([]byte, size)
	result, _, _ = procGetExtendedUDPTable.Call(
		uintptr(unsafe.Pointer(&table[0])),
		uintptr(unsafe.Pointer(&size)),
		0,
		uintptr(syscall.AF_INET),
		udpTableOwnerPID,
		0,
	)
	if result != 0 || size < 4 {
		return 0, false
	}
	count := int(binary.LittleEndian.Uint32(table[:4]))
	const rowSize = 12
	for index := 0; index < count; index++ {
		offset := 4 + index*rowSize
		if offset+rowSize > int(size) || offset+rowSize > len(table) {
			break
		}
		row := table[offset : offset+rowSize]
		port := bits.ReverseBytes16(uint16(binary.LittleEndian.Uint32(row[4:8])))
		if port != endpoint.Port() {
			continue
		}
		pid := binary.LittleEndian.Uint32(row[8:12])
		return pid, pid != 0
	}
	return 0, false
}
