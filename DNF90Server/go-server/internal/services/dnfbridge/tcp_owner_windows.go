//go:build windows

package dnfbridge

import (
	"encoding/binary"
	"math/bits"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	tcpTableOwnerPIDAll = 5
)

var (
	iphlpapiDLL             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTCPTable = iphlpapiDLL.NewProc("GetExtendedTcpTable")
)

func localTCPConnectionOwnerPID(conn net.Conn) (uint32, bool) {
	if conn == nil {
		return 0, false
	}
	local, localOK := conn.LocalAddr().(*net.TCPAddr)
	remote, remoteOK := conn.RemoteAddr().(*net.TCPAddr)
	if !localOK || !remoteOK || local.IP.To4() == nil || remote.IP.To4() == nil {
		return 0, false
	}
	// The accepted socket's local->remote row belongs to DNF90Server.exe.
	// Reverse the port tuple to select the peer row, whose owning PID is the
	// local DNF.exe process authenticated by the launcher.
	return lookupTCP4ConnectionOwnerPID(uint16(remote.Port), uint16(local.Port))
}

// localProcessImagePath returns the executable path of a local process. It
// only needs PROCESS_QUERY_LIMITED_INFORMATION, which is granted for the
// player's own DNF.exe without elevation.
func localProcessImagePath(pid uint32) (string, bool) {
	if pid == 0 {
		return "", false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", false
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil || size == 0 {
		return "", false
	}
	return windows.UTF16ToString(buffer[:size]), true
}

func lookupTCP4ConnectionOwnerPID(localPort, remotePort uint16) (uint32, bool) {
	var size uint32
	result, _, _ := procGetExtendedTCPTable.Call(
		0,
		uintptr(unsafe.Pointer(&size)),
		0,
		uintptr(syscall.AF_INET),
		tcpTableOwnerPIDAll,
		0,
	)
	if result != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || size < 4 {
		return 0, false
	}
	table := make([]byte, size)
	result, _, _ = procGetExtendedTCPTable.Call(
		uintptr(unsafe.Pointer(&table[0])),
		uintptr(unsafe.Pointer(&size)),
		0,
		uintptr(syscall.AF_INET),
		tcpTableOwnerPIDAll,
		0,
	)
	if result != 0 || size < 4 {
		return 0, false
	}
	count := int(binary.LittleEndian.Uint32(table[:4]))
	const rowSize = 24
	for index := 0; index < count; index++ {
		offset := 4 + index*rowSize
		if offset+rowSize > int(size) || offset+rowSize > len(table) {
			break
		}
		row := table[offset : offset+rowSize]
		rowLocalPort := bits.ReverseBytes16(uint16(binary.LittleEndian.Uint32(row[8:12])))
		rowRemotePort := bits.ReverseBytes16(uint16(binary.LittleEndian.Uint32(row[16:20])))
		if rowLocalPort == localPort && rowRemotePort == remotePort {
			pid := binary.LittleEndian.Uint32(row[20:24])
			return pid, pid != 0
		}
	}
	return 0, false
}

// clientAccountProcessLineage returns the socket-owning process followed by
// its parent chain.  The launcher registers the process it starts, while the
// current client may create a child process that owns the game sockets.
func clientAccountProcessLineage(pid uint32) []uint32 {
	if pid == 0 {
		return nil
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return []uint32{pid}
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	parents := make(map[uint32]uint32)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return []uint32{pid}
	}
	for {
		parents[entry.ProcessID] = entry.ParentProcessID
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}
	lineage := []uint32{pid}
	seen := map[uint32]struct{}{pid: {}}
	current := pid
	for range 32 {
		parent, found := parents[current]
		if !found || parent == 0 {
			break
		}
		if _, duplicate := seen[parent]; duplicate {
			break
		}
		lineage = append(lineage, parent)
		seen[parent] = struct{}{}
		current = parent
	}
	return lineage
}
