package dnfbridge

import (
	"encoding/binary"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

const (
	monsterCardSynthesisTunnelOpcode  = uint16(1456)
	monsterCardSynthesisTunnelMagic   = uint32(0x3143534D) // "MSC1"
	monsterCardSynthesisTunnelVersion = byte(1)
	monsterCardSynthesisTunnelHeader  = 10
)

// parseMonsterCardSynthesisTunnel recognizes the 90CN client's private C2S
// envelope for op414. The current EXE builds op414 correctly but its
// predicate-false DPROTO result is not handed to the native upper writer, so
// the compatibility DLL forwards the exact slot body through the proven op1456
// hole. The server restores the original command before entering the normal
// inventory/aligned handler; no request or mutation logic is duplicated here.
func parseMonsterCardSynthesisTunnel(body []byte) ([]byte, bool) {
	// The native upper writer on the tested 90CN client begins op1456 but the
	// DPROTO compatibility boundary preserves the original op414 body without
	// the private envelope. No other op1456 owner uses these exact card request
	// lengths, and the inventory owner still validates every slot, template and
	// PVF rule, so accept this observed shape as the compatibility fallback.
	switch len(body) {
	case 7, 9, 11, 13:
		if len(body) >= 4 && string(body[:4]) == "MSC1" {
			return nil, false
		}
		return append([]byte(nil), body...), true
	}
	if len(body) < monsterCardSynthesisTunnelHeader+7 ||
		binary.LittleEndian.Uint32(body[0:4]) != monsterCardSynthesisTunnelMagic ||
		body[4] != monsterCardSynthesisTunnelVersion ||
		body[5] != 0 ||
		binary.LittleEndian.Uint16(body[6:8]) != uint16(dnfenum.CmdPacketMonstercardBind) {
		return nil, false
	}
	declared := int(binary.LittleEndian.Uint16(body[8:10]))
	if declared != len(body)-monsterCardSynthesisTunnelHeader {
		return nil, false
	}
	switch declared {
	case 7, 9, 11, 13:
		return append([]byte(nil), body[monsterCardSynthesisTunnelHeader:]...), true
	default:
		return nil, false
	}
}
