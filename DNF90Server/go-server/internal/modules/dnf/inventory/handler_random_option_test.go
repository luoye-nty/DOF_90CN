package inventory

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func handleRandomOptionRequest(t *testing.T, repos dnfrepo.Group, opcode dnfenum.CmdPacket, body []byte, resolver alignedcmd.RandomOptionResolver) alignedcmd.Result {
	t.Helper()
	result, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
		Opcode:               uint16(opcode),
		Body:                 body,
		SelectedCharacterID:  77,
		Repositories:         repos,
		RandomOptionResolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHandlerRandomOptionSuccessOrdersUpdateThenACK(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveRandomOptionFixture(t, ctx, repos, 1000, nil, nil)
	got := handleRandomOptionRequest(t, repos, dnfenum.CmdPacketUnsealRandomOption, []byte{9, 0, 0xFF, 0xFF}, staticRandomOptionResolver(testRandomOptionResolution()))
	if !got.Handled || !got.ResponseAllowed || got.Operation != "unseal_random_option" || len(got.UpperResponses) != 2 {
		t.Fatalf("result = %+v", got)
	}
	// 解封要收解封费用：ACK 里没有放金币的字段，必须挂一次主列表钱包刷新，
	// 否则客户端要小退重登才看到扣完的余额（2026-09-18 玩家实测）。
	if !reflect.DeepEqual(got.PostActions, []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedMainWallets}) {
		t.Fatalf("post actions = %+v, want main-wallet refresh after spending gold", got.PostActions)
	}
	update := got.UpperResponses[0]
	if update.Classification != 0 || update.MsgID != msgItemListUpdate || len(update.Body) != 3+currentItemListEntrySize || update.Body[0] != listTypeMain || update.Body[1] != 1 {
		t.Fatalf("update = %+v body=% X", update, update.Body)
	}
	if update.Body[3] != 9 || update.Body[3+randomOptionCountOffset] != 3 {
		t.Fatalf("update row header/options = % X", update.Body)
	}
	ack := got.UpperResponses[1]
	if ack.Classification != 1 || ack.MsgID != uint16(dnfenum.CmdPacketUnsealRandomOption) || !bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("ACK = %+v", ack)
	}
}

// 变更（op437）同样收手续费，因此同样要把新余额推给客户端。
func TestHandlerChangeRandomOptionPushesSpentGold(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, currentItemListEntrySize)
	raw[randomOptionCountOffset] = 1
	raw[randomOptionTypeOffset] = 11
	raw[randomOptionValue1Offset] = 21
	raw[randomOptionValue2Offset] = 31
	saveRandomOptionFixture(t, ctx, repos, 1000, raw, nil)
	got := handleRandomOptionRequest(t, repos, dnfenum.CmdPacketChangeRandomOption, []byte{9, 0, 0, 0, 0}, staticRandomOptionResolver(testRandomOptionResolution()))
	if !got.Handled || !got.ResponseAllowed || got.Operation != "change_random_option" || len(got.UpperResponses) != 2 {
		t.Fatalf("result = %+v", got)
	}
	if !reflect.DeepEqual(got.PostActions, []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedMainWallets}) {
		t.Fatalf("post actions = %+v, want main-wallet refresh after spending gold", got.PostActions)
	}
}

// 不收费的路径不该多发钱包刷新包（例如自定义 PVF 把费用配成 0）。
func TestHandlerRandomOptionSkipsWalletRefreshWhenFree(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveRandomOptionFixture(t, ctx, repos, 1000, nil, nil)
	resolution := testRandomOptionResolution()
	resolution.BreakSealGoldCost = 0
	got := handleRandomOptionRequest(t, repos, dnfenum.CmdPacketUnsealRandomOption, []byte{9, 0, 0xFF, 0xFF}, staticRandomOptionResolver(resolution))
	if !got.Handled || len(got.PostActions) != 0 {
		t.Fatalf("result = %+v, want no post action", got)
	}
}

func TestHandlerResetRandomOptionSuccessOrdersRowsThenACK(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveResetFixture(t, ctx, repos, []RandomOptionValue{{Type: 72, Value1: 5, Value2: 39}}, 15, 3)
	got := handleRandomOptionRequest(t, repos, dnfenum.CmdPacketResetRandomOption,
		[]byte{byte(testResetTargetSlot), 0, byte(testResetMaterialSlot), 0, 0, 0},
		staticRandomOptionResolver(testResetResolution()))
	if !got.Handled || !got.ResponseAllowed || got.Operation != "reset_random_option" || len(got.UpperResponses) != 2 {
		t.Fatalf("result = %+v", got)
	}
	update := got.UpperResponses[0]
	if update.Classification != 0 || update.MsgID != msgItemListUpdate || len(update.Body) != 3+2*currentItemListEntrySize {
		t.Fatalf("update = %+v body=% X", update, update.Body)
	}
	if update.Body[0] != listTypeMain || update.Body[1] != 2 {
		t.Fatalf("update header = % X", update.Body[:3])
	}
	targetRow := update.Body[3 : 3+currentItemListEntrySize]
	materialRow := update.Body[3+currentItemListEntrySize:]
	if targetRow[0] != byte(testResetTargetSlot) || targetRow[randomOptionCountOffset] != 1 {
		t.Fatalf("target row = % X", targetRow[:8])
	}
	if targetRow[randomOptionValue1Offset] != targetRow[randomOptionValue2Offset] {
		t.Fatalf("target row percent = %d/%d", targetRow[randomOptionValue1Offset], targetRow[randomOptionValue2Offset])
	}
	if materialRow[0] != byte(testResetMaterialSlot) || materialRow[2] != 15 || materialRow[6] != 2 {
		t.Fatalf("material row = % X", materialRow[:8])
	}
	ack := got.UpperResponses[1]
	if ack.Classification != 1 || ack.MsgID != uint16(dnfenum.CmdPacketResetRandomOption) || !bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("ACK = %+v", ack)
	}
}

// The current 90 client blocks its whole inventory UI until the pending CMD
// answers, so every op455 rejection must still emit class1 {0, error}.
func TestHandlerResetRandomOptionAlwaysAnswersWithErrorCode(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     []byte
		resolver alignedcmd.RandomOptionResolver
		wantCode byte
	}{
		{name: "short body", body: []byte{9, 0, 20, 0, 0}, resolver: staticRandomOptionResolver(testResetResolution()), wantCode: randomOptionResetErrorInvalidMaterial},
		{name: "resolver unavailable", body: []byte{9, 0, 20, 0, 0, 0}, resolver: nil, wantCode: randomOptionResetErrorInvalidMaterial},
		{name: "option index outside the row", body: []byte{9, 0, 20, 0, 2, 0}, resolver: staticRandomOptionResolver(testResetResolution()), wantCode: randomOptionResetErrorNoSealOption},
		{name: "material slot is not an adjustment box", body: []byte{9, 0, 21, 0, 0, 0}, resolver: staticRandomOptionResolver(testResetResolution()), wantCode: randomOptionResetErrorInvalidMaterial},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			saveResetFixture(t, ctx, repos, []RandomOptionValue{{Type: 72, Value1: 5, Value2: 39}}, 15, 3)
			got := handleRandomOptionRequest(t, repos, dnfenum.CmdPacketResetRandomOption, test.body, test.resolver)
			if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
				t.Fatalf("result = %+v", got)
			}
			ack := got.UpperResponses[0]
			if ack.Classification != 1 || ack.MsgID != uint16(dnfenum.CmdPacketResetRandomOption) {
				t.Fatalf("ACK = %+v", ack)
			}
			if len(ack.Body) != 2 || ack.Body[0] != 0 || ack.Body[1] != test.wantCode {
				t.Fatalf("ACK body = % X want 00 %02X", ack.Body, test.wantCode)
			}
		})
	}
}

func TestHandlerRandomOptionFailureReturnsSameOpcodeStatusOnly(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	for _, test := range []struct {
		opcode dnfenum.CmdPacket
		body   []byte
	}{
		{opcode: dnfenum.CmdPacketUnsealRandomOption, body: []byte{9, 0, 0}},
		{opcode: dnfenum.CmdPacketChangeRandomOption, body: []byte{9, 0, 1, 0, 0}},
	} {
		got := handleRandomOptionRequest(t, repos, test.opcode, test.body, nil)
		if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 || len(got.PostActions) != 0 {
			t.Fatalf("result = %+v", got)
		}
		ack := got.UpperResponses[0]
		if ack.Classification != 1 || ack.MsgID != uint16(test.opcode) || !bytes.Equal(ack.Body, []byte{0}) {
			t.Fatalf("ACK = %+v", ack)
		}
	}
}

func TestHandlerRandomOptionInsufficientGoldRequestsVisibleNotice(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveRandomOptionFixture(t, ctx, repos, 99, nil, nil)

	got := handleRandomOptionRequest(
		t,
		repos,
		dnfenum.CmdPacketUnsealRandomOption,
		[]byte{9, 0, 0xFF, 0xFF},
		staticRandomOptionResolver(testRandomOptionResolution()),
	)
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 || len(got.PostActions) != 1 {
		t.Fatalf("result = %+v", got)
	}
	ack := got.UpperResponses[0]
	if ack.Classification != 1 || ack.MsgID != uint16(dnfenum.CmdPacketUnsealRandomOption) || !bytes.Equal(ack.Body, []byte{0}) {
		t.Fatalf("ACK = %+v", ack)
	}
	if got.PostActions[0] != alignedcmd.PostActionNotifyRandomOptionGoldInsufficient {
		t.Fatalf("post actions = %+v", got.PostActions)
	}
	if !strings.Contains(got.Reason, "insufficient gold required=100 available=99") {
		t.Fatalf("reason = %q", got.Reason)
	}
}
