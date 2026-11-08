package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func buildMonsterCardSynthesisTunnelTestBody(request []byte) []byte {
	body := make([]byte, monsterCardSynthesisTunnelHeader+len(request))
	binary.LittleEndian.PutUint32(body[0:4], monsterCardSynthesisTunnelMagic)
	body[4] = monsterCardSynthesisTunnelVersion
	body[5] = 0
	binary.LittleEndian.PutUint16(body[6:8], 414)
	binary.LittleEndian.PutUint16(body[8:10], uint16(len(request)))
	copy(body[monsterCardSynthesisTunnelHeader:], request)
	return body
}

func TestParseMonsterCardSynthesisTunnel(t *testing.T) {
	request := []byte{0x62, 0x00, 0x02, 0xEC, 0x00, 0xEC, 0x00}
	raw, ok := parseMonsterCardSynthesisTunnel(request)
	if !ok || !bytes.Equal(raw, request) {
		t.Fatalf("raw tunnel fallback ok=%t body=% X", ok, raw)
	}
	body, ok := parseMonsterCardSynthesisTunnel(
		buildMonsterCardSynthesisTunnelTestBody(request),
	)
	if !ok || !bytes.Equal(body, request) {
		t.Fatalf("tunnel parse ok=%t body=% X", ok, body)
	}

	for _, mutate := range []func([]byte){
		func(value []byte) { value[0] ^= 0xFF },
		func(value []byte) { value[4]++ },
		func(value []byte) { value[5] = 1 },
		func(value []byte) { binary.LittleEndian.PutUint16(value[6:8], 413) },
		func(value []byte) { value[8]++ },
		func(value []byte) { value[9]++ },
	} {
		candidate := buildMonsterCardSynthesisTunnelTestBody(request)
		mutate(candidate)
		if _, ok := parseMonsterCardSynthesisTunnel(candidate); ok {
			t.Fatalf("invalid tunnel accepted: % X", candidate)
		}
	}
}
