package dnfbridge

import (
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func TestDecodeCurrentSaveQuestNotifyBodyMatchesCurrentEXEVector(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want []uint32
		ok   bool
	}{
		{name: "clear", body: []byte{0}, want: []uint32{}, ok: true},
		{name: "one", body: []byte{1, 0x0f, 0x0f, 0, 0}, want: []uint32{3855}, ok: true},
		{name: "four", body: []byte{4, 1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 4, 0, 0, 0}, want: []uint32{1, 2, 3, 4}, ok: true},
		{name: "empty", body: nil, ok: false},
		{name: "truncated", body: []byte{1}, ok: false},
		{name: "over-limit", body: []byte{5}, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := decodeCurrentSaveQuestNotifyBody(test.body)
			if ok != test.ok || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("decode=%v ok=%t want=%v ok=%t", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestQuestModuleAcceptsLegacySaveQuestNotifyWithoutACK(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{}
	session := &gameSession{conn: connection, selectedCharacterID: 1}
	handled, err := currentGameplayModules.DispatchLegacy(
		service,
		session,
		uint16(dnfenum.CmdPacketSaveQuestNotify),
		[]byte{1, 0x0f, 0x0f, 0, 0},
	)
	if err != nil || !handled {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	if got := connection.write.Bytes(); len(got) != 0 {
		t.Fatalf("save quest notify wrote unproved ACK=%x", got)
	}
}
