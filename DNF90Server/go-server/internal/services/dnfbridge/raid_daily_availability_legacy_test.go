package dnfbridge

import (
	"bytes"
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestCurrentRaidDailyAvailabilityUsesStructuredLegacyInitBody(t *testing.T) {
	repo := fakeLegacyUserInfoRepo{rows: map[string][]dnfrepo.LegacyUserInfoRow{
		"legacy_character_userinfo_fixed_raws": {{
			"noti_type": "1303",
			"payload":   string(bytes.Repeat([]byte{0xff}, 12)),
		}},
	}}
	var service Service
	body, ok := service.buildCSharpLegacyUserInfoBody(
		context.Background(), nil, repo, 1, currentRaidDailyAvailabilityMsgID,
	)
	if !ok {
		t.Fatal("structured op1303 was skipped")
	}
	want, err := buildCurrentRaidDailyAvailabilityBody(defaultCurrentRaidDailyAvailabilityRows())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("op1303 body=%x want structured=%x", body, want)
	}
	if _, raw := csharpRawFixedUserInfoLength(currentRaidDailyAvailabilityMsgID); raw {
		t.Fatal("op1303 remains registered as a legacy fixed-length raw packet")
	}
}

func TestCurrentRaidDailyAvailabilityRemainsInLegacyInitStreamOnce(t *testing.T) {
	count := 0
	for _, packet := range csharpLegacyUserInfoInitPackets() {
		if packet.msgID == currentRaidDailyAvailabilityMsgID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("op1303 init packet count=%d want=1", count)
	}
}
