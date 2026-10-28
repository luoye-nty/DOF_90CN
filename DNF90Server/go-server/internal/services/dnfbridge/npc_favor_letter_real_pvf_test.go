package dnfbridge

// 回归：「充满爱慕的信」（490700963，`[action type] [use favor point] 1000`）用 op812 选中
// NPC 后必须真的加好感度、扣道具、让客户端关弹窗。
//
// 现场（2026-10-16，角色在线抓包）：客户端发的就是 op812 的送礼形状
// `body=0002000000246601`（action=0 / npc=2 / container=36 / slot=358），但服务端按
// "送礼"规则要求玩家站在该 NPC 所在区域，直接回 code=100 → 客户端弹窗不关、道具不扣。
// 信件语义是：消耗道具本身、只按道具自己的 `[npc select category]` 白名单校验 NPC、不看位置。

import (
	"context"
	"encoding/binary"
	"os"
	"testing"

	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealPVFNPCFavorLetterGrantsPointsFromAnywhere(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to validate the 充满爱慕的信 op812 route")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, session, _ := newTownMoveTest(t)
	s.initialEquipmentArchive = archive
	s.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	repos := dnfrepomemory.NewMemoryGroup()
	s.repositoryProvider = func() (dnfrepo.Group, bool) { return repos, true }

	const (
		characterID = "29"
		letterID    = 490700963
		letterSlot  = int16(358)
		npc         = uint32(2)
	)
	character, found, err := repos.Character.Load(ctx, characterID)
	if err != nil || !found {
		// newTownMoveTest 用的是自己的内存组；这里按契约自己建一条一致的角色/账号。
		character = dnfrepo.CharacterRecord{CharacterID: characterID, AccountID: "dnf:favor-letter", Job: "11", Level: 90}
	}
	character.AccountID = "dnf:favor-letter"
	character.Stats = map[string]int64{}
	if err = repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err = repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: character.AccountID}); err != nil {
		t.Fatal(err)
	}
	session.accountID = character.AccountID
	session.selectedCharacterID = 29
	// 故意不给 town 位置：信件不该受"是否站在 NPC 所在区域"限制。
	session.townPositionSnapshot = currentTownPositionSnapshot{}
	if err = repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: character.AccountID,
		Slots:     map[string]dnfrepo.ItemStack{dnfrepo.AccountSharedInventorySlotKey(letterSlot): {ItemID: letterID, Count: 3}},
	}); err != nil {
		t.Fatal(err)
	}
	conn := session.conn.(*bufferConn)

	// 报文照抄线上抓包：action=0 / npc=2 / container=36 / slot=358。
	body := []byte{0, 2, 0, 0, 0, 36, 0, 0}
	binary.LittleEndian.PutUint16(body[6:], uint16(letterSlot))
	if err = s.handleCurrentReturneeNPCGift(session, body); err != nil {
		t.Fatal(err)
	}
	update, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if update.Header.MsgID != 14 {
		t.Fatalf("expected item row refresh, got %+v body=%x", update.Header, update.Body)
	}
	ack, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("unexpected trailing packets=%x", trailing)
	}
	if ack.Header.MsgID != 812 || ack.Header.Classification != 1 || len(ack.Body) != 13 || ack.Body[0] != 1 {
		t.Fatalf("letter ACK=%+v body=%x", ack.Header, ack.Body)
	}
	if got := binary.LittleEndian.Uint32(ack.Body[2:6]); got != npc {
		t.Fatalf("ACK npc=%d want %d", got, npc)
	}
	cap, err := dnfonlineevent.LoadNPCFavorCap(archive, npc)
	if err != nil {
		t.Fatal(err)
	}
	wantAdded := min(int64(1000), cap)
	if got := int64(binary.LittleEndian.Uint16(ack.Body[6:8])); got != wantAdded {
		t.Fatalf("ACK affection gain=%d want %d (cap=%d)", got, wantAdded, cap)
	}
	if got := int64(binary.LittleEndian.Uint32(ack.Body[8:12])); got != wantAdded {
		t.Fatalf("ACK affection total=%d want %d", got, wantAdded)
	}
	// 道具扣 1 个、好感度落库
	stored, _, err := repos.AccountInventory.Load(ctx, character.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Slots[dnfrepo.AccountSharedInventorySlotKey(letterSlot)]; got.ItemID != letterID || got.Count != 2 {
		t.Fatalf("letter row after use=%+v want item=%d count=2", got, letterID)
	}
	persisted, _, err := repos.Character.Load(ctx, characterID)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Stats["npc_favor_2_points"]; got != wantAdded {
		t.Fatalf("npc_favor_2_points=%d want %d", got, wantAdded)
	}

	// NPC 不在该道具的 `[npc select category]` 白名单里时必须拒绝（列表外的取值）。
	conn.write.Reset()
	outside := []byte{0, 0xFE, 0x00, 0x00, 0x00, 36, 0, 0}
	binary.LittleEndian.PutUint16(outside[6:], uint16(letterSlot))
	if err = s.handleCurrentReturneeNPCGift(session, outside); err != nil {
		t.Fatal(err)
	}
	reject, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	if reject.Header.MsgID != 812 || len(reject.Body) == 0 || reject.Body[0] != 0 {
		t.Fatalf("out-of-list NPC ACK=%+v body=%x", reject.Header, reject.Body)
	}
}
