package dnfbridge

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 客户端悬停读到的五个数就是这条式子的五个点（2026-09-26 现场实测）。这条断言是
// 规则本身的回归闸：改动它必须先有新的客户端读数。
func TestHellAdmissionInvitationCountMatchesMeasuredClientValues(t *testing.T) {
	for _, test := range []struct {
		dungeonID int64
		name      string
		level     int64
		want      int64
	}{
		{86, "列车上的海贼", 71, 20},
		{87, "夺回西部线", 72, 20},
		{92, "雾都赫伊斯", 73, 21},
		{93, "阿登高地", 74, 21},
		{193, "能源熔炉", 86, 26},
		// 旧树 changlog 记的 (40·90−1800)/100+10 = 28 是同一个式子在 Lv90 的取值。
		{0, "Lv90 参照", 90, 28},
	} {
		if got := currentHellAdmissionInvitationCountForLevel(test.level); got != test.want {
			t.Errorf("%d %s Lv%d -> %d want %d", test.dungeonID, test.name, test.level, got, test.want)
		}
	}
	// 等级未知时返回 0（调用方必须当成"算不出来"而不是"不要门票"）；低于 45 级
	// 的式子是负数，钳到 1 张。
	if got := currentHellAdmissionInvitationCountForLevel(0); got != 0 {
		t.Fatalf("unknown level -> %d want 0", got)
	}
	for _, level := range []int64{1, 20, 44, 45, 46} {
		if got := currentHellAdmissionInvitationCountForLevel(level); got != 1 && got != 10 {
			t.Fatalf("Lv%d -> %d", level, got)
		}
	}
	if got := currentHellAdmissionInvitationCountForLevel(1); got != currentHellAdmissionMinCount {
		t.Fatalf("Lv1 -> %d want clamp %d", got, currentHellAdmissionMinCount)
	}
	if got := currentHellAdmissionInvitationCountForLevel(45); got != 10 {
		t.Fatalf("Lv45 -> %d want 10", got)
	}
}

func TestHellAdmissionInvitationCountPrefersThePVFDeclaration(t *testing.T) {
	declared := worldmap.Dungeon{ID: 310, Metadata: worldmap.DungeonMetadata{
		HellPartyRequiredItemCount: worldmap.OptionalInt{Value: 28, Set: true},
		MinimumRequiredLevel:       worldmap.OptionalInt{Value: 87, Set: true},
	}}
	count, source, err := currentDungeonHellAdmissionInvitationCount(declared)
	if err != nil || count != 28 || source != currentHellAdmissionCostSourceDeclared {
		t.Fatalf("declared count=%d source=%q err=%v", count, source, err)
	}

	derived := worldmap.Dungeon{ID: 193, Metadata: worldmap.DungeonMetadata{
		MinimumRequiredLevel: worldmap.OptionalInt{Value: 86, Set: true},
	}}
	count, source, err = currentDungeonHellAdmissionInvitationCount(derived)
	if err != nil || count != 26 || source != currentHellAdmissionCostSourceLevel {
		t.Fatalf("derived count=%d source=%q err=%v", count, source, err)
	}

	// Neither the declaration nor a level: the plan must fail loudly (the caller
	// turns it into an op16 failure), never silently price the entry at zero.
	if _, _, err := currentDungeonHellAdmissionInvitationCount(worldmap.Dungeon{ID: 9000}); err == nil {
		t.Fatal("dungeon with neither cost source must not be priced")
	}
}

// 194/205 个深渊区域副本没有 [hell party required item count]；193 号（能源熔炉 Lv86）
// 就是现场那个点不下去的副本。计划必须按等级式定价成 26 张 3330，而不是报错。
func TestCurrentDungeonEntryPricesHellEntryFromMinimumRequiredLevel(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	service.worldMapTable = currentDungeonEntryAdmissionTestWorldMap(t, `
[dungeon]
	193 -1
[/dungeon]
[hell dungeon]
1
[item condition]
3330
[/item condition]
`)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9": {ItemID: 3330, Count: 40},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{
		Request: dungeoncmd.SelectDungeonRequest{SelectionMode: 1, RuntimeToken: 0xffff},
		Dungeon: worldmap.Dungeon{ID: 193, Metadata: worldmap.DungeonMetadata{
			MinimumRequiredLevel: worldmap.OptionalInt{Value: 86, Set: true},
		}},
	}

	plan, err := service.resolveCurrentDungeonEntryAdmissionPlan(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Requirements) != 1 ||
		plan.Requirements[0] != (worldmap.TicketItem{ItemID: 3330, Count: 26}) ||
		plan.AdmissionCostSource != currentHellAdmissionCostSourceLevel {
		t.Fatalf("hell admission plan=%+v", plan)
	}

	updates, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || admissionUpdateSlot(updates[0]) != 9 || admissionUpdateCount(updates[0]) != 14 {
		t.Fatalf("hell material updates=%+v", updates)
	}
	after, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found || after.Slots["0:9"].Count != 14 {
		t.Fatalf("after hell material consume=%+v found=%t err=%v", after, found, err)
	}
}

func TestCurrentDungeonEntryItemFailureReasonSelectsHellInvitationNotice(t *testing.T) {
	for _, test := range []struct {
		name    string
		runtime *runtimeDungeonState
		want    byte
	}{
		{name: "nil runtime", runtime: nil, want: currentSelectDungeonEntryItemMissingReason},
		{
			name:    "ordinary dungeon",
			runtime: &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5006}},
			want:    currentSelectDungeonEntryItemMissingReason,
		},
		{
			name: "inferno",
			runtime: &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 7539, Metadata: worldmap.DungeonMetadata{
				InfernoDungeon: true,
			}}},
			want: currentSelectDungeonHellInvitationMissingReason,
		},
		{
			name: "selected hell party",
			runtime: &runtimeDungeonState{
				Request: dungeoncmd.SelectDungeonRequest{SelectionMode: 1, RuntimeToken: 0xffff},
				Dungeon: worldmap.Dungeon{ID: 193, Metadata: worldmap.DungeonMetadata{
					IntegerValues: map[string]int64{"hell dungeon": 1},
				}},
			},
			want: currentSelectDungeonHellInvitationMissingReason,
		},
		{
			name: "particular hell keeps the generic notice",
			runtime: &runtimeDungeonState{
				Request: dungeoncmd.SelectDungeonRequest{SelectionMode: 1, RuntimeToken: 0xffff},
				Dungeon: worldmap.Dungeon{ID: 316, Metadata: worldmap.DungeonMetadata{
					DungeonType: "[particular hell]",
				}},
			},
			want: currentSelectDungeonEntryItemMissingReason,
		},
	} {
		if got := currentDungeonEntryItemFailureReason(test.runtime); got != test.want {
			t.Errorf("%s: reason=%d want %d", test.name, got, test.want)
		}
	}
}

// 入场计划建立不起来时也必须回一发 op16 失败：以前这条路径 return nil，服务端一个字
// 都不回，客户端停在选择页上（2026-09-26 现场 193 号副本卡了 26 秒）。
func TestCurrentDungeonEntryAdmissionPlanFailureAlwaysAnswersTheClient(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	hellRuntime := &runtimeDungeonState{
		Request: dungeoncmd.SelectDungeonRequest{SelectionMode: 1, RuntimeToken: 0xffff},
		Dungeon: worldmap.Dungeon{ID: 193, Metadata: worldmap.DungeonMetadata{
			IntegerValues: map[string]int64{"hell dungeon": 1},
		}},
	}
	cause := errors.New("invalid PVF hell admission cost dungeon=193")
	if err := service.sendCurrentDungeonEntryAdmissionPlanFailure(session, hellRuntime, cause); err != nil {
		t.Fatal(err)
	}
	packet, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	if !bytes.Equal(packet.Body, []byte{0, currentSelectDungeonHellInvitationMissingReason, 0}) {
		t.Fatalf("hell plan failure body=%x", packet.Body)
	}
	if len(remaining) != 0 {
		t.Fatalf("plan failure must only answer op16, got %x", remaining)
	}

	// The refusal also arms the escape guard: the client's own post-failure state
	// migration sends a bodyless op132 which must not be read as a player return.
	if session.dungeonEntryItemRefusedAt.IsZero() {
		t.Fatal("plan failure must mark the entry refusal for the escape guard")
	}
}
