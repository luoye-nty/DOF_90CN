package dnfbridge

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestRuntimeRaidRewardReceiptReplayDoesNotGrantTwice(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, false)
	first, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now,
	)
	if err != nil {
		t.Fatalf("first reward commit: %v", err)
	}
	if first.Token == 0 || len(first.RewardBody) == 0 || len(first.InventoryRefreshes) != 2 {
		t.Fatalf("first delivery=%+v", first)
	}
	afterFirst := loadRuntimeRaidRewardTestInventories(t, fixture.repositories)
	clearRuntimeRaidRewardTestDeliveryCache(fixture.service, fixture.raidKey)

	replayed, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("receipt replay: %v", err)
	}
	if replayed.Token != first.Token || !reflect.DeepEqual(replayed.Rewards, first.Rewards) ||
		!reflect.DeepEqual(replayed.RewardBody, first.RewardBody) {
		t.Fatalf("replayed delivery=%+v first=%+v", replayed, first)
	}
	afterReplay := loadRuntimeRaidRewardTestInventories(t, fixture.repositories)
	if !reflect.DeepEqual(afterReplay, afterFirst) {
		t.Fatalf("receipt replay granted twice:\nfirst=%+v\nreplay=%+v", afterFirst, afterReplay)
	}
}

func TestRuntimeRaidRewardCreditsVouchersToAccountWallet(t *testing.T) {
	cases := []struct {
		name        string
		itemID      uint32
		metadataKey string
	}{
		{name: "token_cera", itemID: 2681762, metadataKey: "account_token_cera"},
		{name: "happy_token_cera", itemID: 2681917, metadataKey: "account_happy_token_cera"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRuntimeRaidRewardTestFixture(t, true)
			catalog := fixture.service.raidAntonReward
			pool, found := catalog.Pool(0, "party_card")
			if !found || len(pool.Entries) == 0 {
				t.Fatal("party_card pool missing")
			}
			definitionID := pool.Entries[0].DefinitionItemID
			definition, found := catalog.definitions[definitionID]
			if !found {
				t.Fatalf("definition %d missing", definitionID)
			}
			const boosterPath = "stackable/test/raid_voucher_booster.stk"
			voucherPath := "cash/test/raid_voucher.stk"
			fixture.pvfSource.documents[boosterPath] = "[name]\n`raid voucher booster`\n[stackable type]\n`[booster]` 0\n[booster info]\n[etc]\n1 " +
				strconv.FormatUint(uint64(tc.itemID), 10) + " 100 3000\n[/etc]\n[/booster info]\n"
			fixture.pvfSource.documents["stackable/"+voucherPath] = "[name]\n`voucher`\n[stackable type]\n`[material]`\n[stack limit]\n1000000000\n"
			catalog.itemRefs[tc.itemID] = dungeonDropItemReference{
				kind: dungeonDropItemStackable,
				path: voucherPath,
			}
			definition.RewardType = "booster"
			definition.DirectFinal = false
			definition.PVFPath = boosterPath
			definition.Entries = []antonRewardPVFEntry{{ItemID: tc.itemID, Weight: 1, Count: 3000}}
			definition.TotalWeight = 1
			catalog.definitions[definitionID] = definition

			delivery, err := fixture.service.commitRuntimeRaidSettlementRewards(
				fixture.raidKey,
				runtimeRaidDungeonPhaseOne,
				fixture.now,
			)
			if err != nil {
				t.Fatalf("reward commit: %v", err)
			}
			if len(delivery.WalletRefreshes) != 2 {
				t.Fatalf("wallet refreshes=%+v", delivery.WalletRefreshes)
			}
			for _, characterID := range []uint16{1001, 1002} {
				accountID := "account-" + strconv.Itoa(int(characterID))
				account, found, loadErr := fixture.repositories.Account.Load(context.Background(), accountID)
				if loadErr != nil || !found {
					t.Fatalf("load %s found=%t err=%v", accountID, found, loadErr)
				}
				if got := account.Metadata[tc.metadataKey]; got != "3100" {
					t.Fatalf("character %d %s=%s want=3100", characterID, tc.metadataKey, got)
				}
				inventory, found, loadErr := fixture.repositories.Inventory.Load(context.Background(), strconv.Itoa(int(characterID)))
				if loadErr != nil || !found {
					t.Fatalf("load inventory %d found=%t err=%v", characterID, found, loadErr)
				}
				for key, stack := range inventory.Slots {
					if stack.ItemID == int64(tc.itemID) {
						t.Fatalf("voucher leaked into inventory character=%d slot=%s stack=%+v", characterID, key, stack)
					}
				}
			}

			if err := fixture.service.sendRuntimeRaidSettlementWalletRefreshes(delivery.WalletRefreshes); err != nil {
				t.Fatalf("send wallet refresh: %v", err)
			}
			for name, connection := range map[string]*bufferConn{
				"leader": fixture.leader.conn.(*bufferConn),
				"member": fixture.member.conn.(*bufferConn),
			} {
				counts := runtimeRaidRewardPacketCounts(t, connection.write.Bytes())
				if len(counts) != 1 || counts[currentCeraShopWalletMsgID] != 1 {
					t.Fatalf("%s wallet refresh packet counts=%v", name, counts)
				}
			}

			clearRuntimeRaidRewardTestDeliveryCache(fixture.service, fixture.raidKey)
			if _, err := fixture.service.commitRuntimeRaidSettlementRewards(
				fixture.raidKey,
				runtimeRaidDungeonPhaseOne,
				fixture.now.Add(time.Minute),
			); err != nil {
				t.Fatalf("receipt replay: %v", err)
			}
			for _, characterID := range []string{"1001", "1002"} {
				account, _, loadErr := fixture.repositories.Account.Load(context.Background(), "account-"+characterID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if got := account.Metadata[tc.metadataKey]; got != "3100" {
					t.Fatalf("receipt replay credited twice: character=%s %s=%s", characterID, tc.metadataKey, got)
				}
			}
		})
	}
}

// 团本全队翻牌抽到稀有奖励不变金色 —— 移植上游 257a5b8（2026-09-25）。
//
// 一条奖励行的首字节就是它的 [STATE REWARD] 标记：客户端把它存在牌对象 +0x44，
// sub_167FE00 在全队板（resultType 3）上按 +0x44 == 1 画金牌（0x0167FE1E）。
// 稀有的 squad_item 必须以 1 同时出现在翻牌包体与 resultType-3 全队板上，
// 普通 party_card 保持 0。
func TestRuntimeRaidRewardRareRowCarriesItsPVFFlagForTheGoldCard(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, false)
	// 一阶段的 squad_item 权重是 50/1/1/48、标记 0/1/1/1：99 落在最后那一行（稀有）；
	// party_card 只有一条标记为 0 的行。每个成员先掷 party_card（选池 + 取内容）
	// 再掷 squad_item，所以每四次里只有第三次（roll%4 == 2）会选中稀有行，
	// 取内容那两次留在夹具的第一条上。
	rolls := 0
	fixture.service.raidRewardRandom = func() uint64 {
		roll := rolls
		rolls++
		if roll%4 == 2 {
			return 99
		}
		return 0
	}
	delivery, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now,
	)
	if err != nil {
		t.Fatalf("reward commit: %v", err)
	}
	for _, member := range delivery.Rewards.Members {
		if len(member.Rewards) != 2 {
			t.Fatalf("member %d rows=%+v want party_card+squad_item", member.MemberKey, member.Rewards)
		}
		if got := member.Rewards[0].Kind; got != 0 {
			t.Fatalf("member %d party_card kind=%d want 0", member.MemberKey, got)
		}
		if got := member.Rewards[1].Kind; got != 1 {
			t.Fatalf("member %d rare squad_item kind=%d want 1", member.MemberKey, got)
		}
	}
	board, ok, err := runtimeRaidTeamSquadRewardBody(runtimeRaidSettlementRewardDelivery{Rewards: delivery.Rewards})
	if err != nil || !ok {
		t.Fatalf("team board ok=%t err=%v", ok, err)
	}
	// u8 resultType, u8 count，然后每人 u16 key、u8 card、u8 rows、{u8 kind, u32 item, u16 count}。
	if board[0] != runtimeRaidTeamRewardResultType || int(board[1]) != len(delivery.Rewards.Members) {
		t.Fatalf("team board header=%x", board[:2])
	}
	for index := 0; index < int(board[1]); index++ {
		row := board[2+index*11:]
		if row[3] != 1 || row[4] != 1 {
			t.Fatalf("team board member %d rows=%d kind=%d want one rare row", index, row[3], row[4])
		}
	}
}

func TestRuntimeRaidRewardReceiptReplayRejectsStaleInventoryPostState(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, false)
	delivery, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now,
	)
	if err != nil {
		t.Fatalf("first reward commit: %v", err)
	}
	if len(delivery.InventoryRefreshes) == 0 || len(delivery.InventoryRefreshes[0].Slots) == 0 {
		t.Fatalf("delivery has no committed slots: %+v", delivery)
	}

	characterID := delivery.InventoryRefreshes[0].CharacterID
	characterKey := runtimeRaidRewardTestCharacterKey(characterID)
	inventory, found, err := fixture.repositories.Inventory.Load(context.Background(), characterKey)
	if err != nil || !found {
		t.Fatalf("load committed inventory character=%s found=%t err=%v", characterKey, found, err)
	}
	slot := delivery.InventoryRefreshes[0].Slots[0]
	slotKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
	stack, found := inventory.Slots[slotKey]
	if !found {
		t.Fatalf("committed slot %s missing from %+v", slotKey, inventory.Slots)
	}
	stack.Count++
	inventory.Slots[slotKey] = stack
	if err := fixture.repositories.Inventory.Save(context.Background(), inventory); err != nil {
		t.Fatalf("mutate post-state: %v", err)
	}
	clearRuntimeRaidRewardTestDeliveryCache(fixture.service, fixture.raidKey)

	if _, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now.Add(time.Minute),
	); err == nil {
		t.Fatal("stale receipt replay accepted a changed committed inventory slot")
	}
}

func TestRuntimeRaidRewardReceiptReplayDoesNotRequirePVFAfterCommit(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, false)
	first, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now,
	)
	if err != nil {
		t.Fatalf("first reward commit: %v", err)
	}
	afterFirst := loadRuntimeRaidRewardTestInventories(t, fixture.repositories)
	clearRuntimeRaidRewardTestDeliveryCache(fixture.service, fixture.raidKey)
	fixture.service.raidAntonRewardMu.Lock()
	fixture.service.raidAntonReward = nil
	fixture.service.raidAntonRewardErr = errors.New("PVF changed after durable reward commit")
	fixture.service.raidAntonRewardMu.Unlock()

	replayed, err := fixture.service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("durable receipt replay depended on PVF: %v", err)
	}
	if replayed.Token != first.Token || !reflect.DeepEqual(replayed.RewardBody, first.RewardBody) {
		t.Fatalf("PVF-independent replay=%+v first=%+v", replayed, first)
	}
	if afterReplay := loadRuntimeRaidRewardTestInventories(t, fixture.repositories); !reflect.DeepEqual(afterReplay, afterFirst) {
		t.Fatalf("PVF-independent replay changed inventory:\nfirst=%+v\nreplay=%+v", afterFirst, afterReplay)
	}
}

func TestRuntimeRaidRewardListWriteFailureRetriesWithoutDuplicateGrant(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	wantWriteErr := errors.New("raid reward-list write failed")
	failingLeader := &failNthDungeonWriteConn{failAt: 2, err: wantWriteErr}
	fixture.leader.conn = failingLeader
	fixture.member.conn = &bufferConn{}

	state := cloneRuntimeRaidState(*fixture.service.raids[fixture.raidKey])
	if err := fixture.service.commitAndPublishRuntimeRaidSettlementRewards(state, fixture.service, fixture.now); !errors.Is(err, wantWriteErr) {
		t.Fatalf("first publish error=%v want=%v", err, wantWriteErr)
	}
	afterFailedWrite := loadRuntimeRaidRewardTestInventories(t, fixture.repositories)
	committed := fixture.service.raids[fixture.raidKey]
	if committed.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards ||
		!committed.Lifecycle.RewardDeliveryCommitted || committed.Lifecycle.RewardDelivery == nil {
		t.Fatalf("failed write lost durable delivery: %+v", committed.Lifecycle)
	}
	// Per-squad scoping (86JP RaidHandler.Rewards.cs:346-361 / :513-528): the
	// fixture puts 1001 in squad 1 and 1002 in squad 2, so the retry must hand each
	// of them only its own squad's row of the same settlement.
	squadRewards := committed.Lifecycle.RewardDelivery.Rewards
	wantRewardBodies := make(map[uint16][]byte, len(squadRewards.Members))
	for _, row := range squadRewards.Members {
		body, buildErr := raid.BuildRaidSettlementRewardsBody(raid.RaidSettlementRewards{
			ResultType: squadRewards.ResultType,
			Members:    []raid.RaidSettlementMemberRewards{row},
		})
		if buildErr != nil {
			t.Fatalf("build squad reward body for %d: %v", row.MemberKey, buildErr)
		}
		wantRewardBodies[row.MemberKey] = body
	}
	if len(wantRewardBodies) != 2 || bytes.Equal(wantRewardBodies[1001], wantRewardBodies[1002]) {
		t.Fatalf("fixture squads share one reward body: %d distinct", len(wantRewardBodies))
	}

	leaderRetry := &bufferConn{}
	memberRetry := &bufferConn{}
	fixture.leader.conn = leaderRetry
	fixture.member.conn = memberRetry
	retryState := cloneRuntimeRaidState(*committed)
	if err := fixture.service.commitAndPublishRuntimeRaidSettlementRewards(
		retryState,
		fixture.service,
		fixture.now.Add(time.Minute),
	); err != nil {
		t.Fatalf("reward-list retry: %v", err)
	}
	afterRetry := loadRuntimeRaidRewardTestInventories(t, fixture.repositories)
	if !reflect.DeepEqual(afterRetry, afterFailedWrite) {
		t.Fatalf("reward-list retry granted twice:\nfailed=%+v\nretry=%+v", afterFailedWrite, afterRetry)
	}
	standby := fixture.service.raids[fixture.raidKey]
	if standby == nil || standby.Lifecycle.Status != runtimeRaidLifecycleStandby || standby.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		t.Fatalf("retry did not enter phase-one standby: %+v", standby)
	}
	wantTimer := raid.BuildRaidTimerBody(runtimeRaidStandbyTimer(*standby))
	// The whole-raid team row is one resultType-3 body carrying every settled
	// member's team item; both squads receive the identical bytes.
	teamRows := make([]raid.RaidSettlementMemberRewards, 0, len(squadRewards.Members))
	for _, row := range squadRewards.Members {
		if len(row.Rewards) == 0 {
			continue
		}
		teamRows = append(teamRows, raid.RaidSettlementMemberRewards{
			MemberKey: row.MemberKey,
			CardIndex: 2,
			Rewards:   []raid.RaidSettlementReward{row.Rewards[len(row.Rewards)-1]},
		})
	}
	wantTeamRows, err := raid.BuildRaidSettlementRewardsBody(raid.RaidSettlementRewards{
		ResultType: runtimeRaidTeamRewardResultType,
		Members:    teamRows,
	})
	if err != nil {
		t.Fatalf("build expected team rewards: %v", err)
	}
	for _, stream := range []struct {
		name   string
		raw    []byte
		charID uint16
	}{
		{name: "leader", raw: leaderRetry.write.Bytes(), charID: 1001},
		{name: "member", raw: memberRetry.write.Bytes(), charID: 1002},
	} {
		inventoryPacket, rest := splitGameServerUpperPacket(t, stream.raw)
		if inventoryPacket.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
			t.Fatalf("%s first retry packet msg=%d want op14", stream.name, inventoryPacket.Header.MsgID)
		}
		assertRuntimeRaidRewardAndTimerPackets(t, rest, wantRewardBodies[stream.charID], wantTeamRows, wantTimer)
	}
}

func TestRuntimeRaidRewardCommitPrecedesSuccessAckAndAckRetryDoesNotGrantTwice(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	wantWriteErr := errors.New("raid card ACK write failed")
	fixture.leader.conn = &failNthDungeonWriteConn{failAt: 1, err: wantWriteErr}
	fixture.member.conn = &bufferConn{}

	if err := fixture.service.handleRuntimeRaidRewardCard(fixture.leader, []byte{0, 2}); !errors.Is(err, wantWriteErr) {
		t.Fatalf("first mode0 error=%v want=%v", err, wantWriteErr)
	}
	afterFailedACK := loadRuntimeRaidRewardTestInventories(t, fixture.repositories)
	committed := fixture.service.raids[fixture.raidKey]
	if committed == nil || committed.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards ||
		!committed.Lifecycle.RewardDeliveryCommitted || committed.Lifecycle.RewardDelivery == nil {
		t.Fatalf("failed ACK did not retain durable reward delivery: %+v", committed)
	}
	if fixture.member.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("reward published before requester ACK succeeded")
	}

	leaderRetry := &bufferConn{}
	memberRetry := &bufferConn{}
	fixture.leader.conn = leaderRetry
	fixture.member.conn = memberRetry
	if err := fixture.service.handleRuntimeRaidRewardCard(fixture.leader, []byte{0, 2}); err != nil {
		t.Fatalf("mode0 ACK retry: %v", err)
	}
	if afterRetry := loadRuntimeRaidRewardTestInventories(t, fixture.repositories); !reflect.DeepEqual(afterRetry, afterFailedACK) {
		t.Fatalf("ACK retry granted twice:\nfailed=%+v\nretry=%+v", afterFailedACK, afterRetry)
	}
	leaderCounts := runtimeRaidRewardPacketCounts(t, leaderRetry.write.Bytes())
	// Two 0x0258 per member now: the squad-scoped reveal row and the whole-raid
	// team row that follows it in the same pass (runtimeRaidTeamSquadRewardBody).
	if leaderCounts[raid.RaidRewardCardRequestMsgID] != 1 ||
		leaderCounts[uint16(dnfenum.CmdPacketWalkoutPartyMember)] != 1 ||
		leaderCounts[raid.RaidRewardListMsgID] != 2 || leaderCounts[raid.RaidTimerMsgID] != 1 {
		t.Fatalf("leader retry packet counts=%v", leaderCounts)
	}
	memberCounts := runtimeRaidRewardPacketCounts(t, memberRetry.write.Bytes())
	if memberCounts[uint16(dnfenum.CmdPacketWalkoutPartyMember)] != 1 ||
		memberCounts[raid.RaidRewardListMsgID] != 2 || memberCounts[raid.RaidTimerMsgID] != 1 {
		t.Fatalf("member retry packet counts=%v", memberCounts)
	}
}

func TestRuntimeRaidRewardRetryOnlyPublishesRewardListToUnfinishedMember(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	wantWriteErr := errors.New("member reward-list write failed")
	leaderFirst := &bufferConn{}
	memberFirst := &failNthDungeonWriteConn{failAt: 2, err: wantWriteErr}
	fixture.leader.conn = leaderFirst
	fixture.member.conn = memberFirst

	state := cloneRuntimeRaidState(*fixture.service.raids[fixture.raidKey])
	if err := fixture.service.commitAndPublishRuntimeRaidSettlementRewards(state, fixture.service, fixture.now); !errors.Is(err, wantWriteErr) {
		t.Fatalf("first publish error=%v want=%v", err, wantWriteErr)
	}
	firstLeaderCounts := runtimeRaidRewardPacketCounts(t, leaderFirst.write.Bytes())
	if firstLeaderCounts[uint16(dnfenum.CmdPacketWalkoutPartyMember)] != 1 ||
		firstLeaderCounts[raid.RaidRewardListMsgID] != 1 || firstLeaderCounts[raid.RaidTimerMsgID] != 0 {
		t.Fatalf("first leader packet counts=%v", firstLeaderCounts)
	}

	leaderRetry := &bufferConn{}
	memberRetry := &bufferConn{}
	fixture.leader.conn = leaderRetry
	fixture.member.conn = memberRetry
	retryState := cloneRuntimeRaidState(*fixture.service.raids[fixture.raidKey])
	if err := fixture.service.commitAndPublishRuntimeRaidSettlementRewards(retryState, fixture.service, fixture.now.Add(time.Minute)); err != nil {
		t.Fatalf("reward-list retry: %v", err)
	}
	leaderRetryCounts := runtimeRaidRewardPacketCounts(t, leaderRetry.write.Bytes())
	// The leader's reveal was delivered before the member's write failed, so the
	// retry only owes it the whole-raid team row that the aborted pass never sent.
	if leaderRetryCounts[uint16(dnfenum.CmdPacketWalkoutPartyMember)] != 0 ||
		leaderRetryCounts[raid.RaidRewardListMsgID] != 1 || leaderRetryCounts[raid.RaidTimerMsgID] != 1 {
		t.Fatalf("leader retry packet counts=%v", leaderRetryCounts)
	}
	memberRetryCounts := runtimeRaidRewardPacketCounts(t, memberRetry.write.Bytes())
	if memberRetryCounts[uint16(dnfenum.CmdPacketWalkoutPartyMember)] != 1 ||
		memberRetryCounts[raid.RaidRewardListMsgID] != 2 || memberRetryCounts[raid.RaidTimerMsgID] != 1 {
		t.Fatalf("member retry packet counts=%v", memberRetryCounts)
	}
}

func TestRuntimeRaidRewardRetryOnlyPublishesStandbyTimerToUnfinishedMember(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	wantWriteErr := errors.New("member standby-timer write failed")
	leaderFirst := &bufferConn{}
	// Writes to the member are: 1 inventory refresh, 2 squad reveal, 3 whole-raid
	// team row, 4 standby timer. Fail the timer so the retry path is exercised.
	memberFirst := &failNthDungeonWriteConn{failAt: 4, err: wantWriteErr}
	fixture.leader.conn = leaderFirst
	fixture.member.conn = memberFirst

	state := cloneRuntimeRaidState(*fixture.service.raids[fixture.raidKey])
	if err := fixture.service.commitAndPublishRuntimeRaidSettlementRewards(state, fixture.service, fixture.now); !errors.Is(err, wantWriteErr) {
		t.Fatalf("first timer publish error=%v want=%v", err, wantWriteErr)
	}
	standby := fixture.service.raids[fixture.raidKey]
	if standby == nil || standby.Lifecycle.Status != runtimeRaidLifecycleStandby {
		t.Fatalf("timer write failure lost standby state: %+v", standby)
	}
	firstLeaderCounts := runtimeRaidRewardPacketCounts(t, leaderFirst.write.Bytes())
	if firstLeaderCounts[uint16(dnfenum.CmdPacketWalkoutPartyMember)] != 1 ||
		firstLeaderCounts[raid.RaidRewardListMsgID] != 2 || firstLeaderCounts[raid.RaidTimerMsgID] != 1 {
		t.Fatalf("first leader packet counts=%v", firstLeaderCounts)
	}

	leaderRetry := &bufferConn{}
	memberRetry := &bufferConn{}
	fixture.leader.conn = leaderRetry
	fixture.member.conn = memberRetry
	retryState := cloneRuntimeRaidState(*standby)
	if err := fixture.service.commitAndPublishRuntimeRaidSettlementRewards(retryState, fixture.service, fixture.now.Add(time.Minute)); err != nil {
		t.Fatalf("standby-timer retry: %v", err)
	}
	if leaderRetry.write.Len() != 0 {
		t.Fatalf("leader received duplicate publication bytes=%d", leaderRetry.write.Len())
	}
	memberRetryCounts := runtimeRaidRewardPacketCounts(t, memberRetry.write.Bytes())
	if len(memberRetryCounts) != 2 || memberRetryCounts[raid.RaidTimerMsgID] != 1 ||
		memberRetryCounts[raid.RaidRemainTimeMsgID] != 1 {
		t.Fatalf("member timer retry packet counts=%v", memberRetryCounts)
	}
}

func TestRuntimeRaidRewardConcurrentDuplicateMode0PublishesOncePerMember(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errorsSeen <- fixture.service.handleRuntimeRaidRewardCard(fixture.leader, []byte{0, 2})
		}()
	}
	close(start)
	for range 2 {
		if err := <-errorsSeen; err != nil {
			t.Fatalf("concurrent mode0: %v", err)
		}
	}

	leaderCounts := runtimeRaidRewardPacketCounts(t, fixture.leader.conn.(*bufferConn).write.Bytes())
	// One squad-scoped reveal plus one whole-raid team row per member; the duplicate
	// mode0 must not add a third.
	if leaderCounts[raid.RaidRewardCardRequestMsgID] != 2 ||
		leaderCounts[uint16(dnfenum.CmdPacketWalkoutPartyMember)] != 1 ||
		leaderCounts[raid.RaidRewardListMsgID] != 2 || leaderCounts[raid.RaidTimerMsgID] != 1 {
		t.Fatalf("leader concurrent packet counts=%v", leaderCounts)
	}
	memberCounts := runtimeRaidRewardPacketCounts(t, fixture.member.conn.(*bufferConn).write.Bytes())
	if memberCounts[uint16(dnfenum.CmdPacketWalkoutPartyMember)] != 1 ||
		memberCounts[raid.RaidRewardListMsgID] != 2 || memberCounts[raid.RaidTimerMsgID] != 1 {
		t.Fatalf("member concurrent packet counts=%v", memberCounts)
	}
}

func TestRuntimeRaidSingleRewardPublishesPersonalThenSquadThenUnlocks(t *testing.T) {
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	state := fixture.service.raids[fixture.raidKey]
	state.TailFlag = 1
	state.Members = append([]runtimeRaidMemberState(nil), state.Members[:1]...)
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), state.FrozenMembers[:1]...)
	state.ChargedMembers = append([]uint16(nil), state.ChargedMembers[:1]...)
	delete(state.Lifecycle.RewardSelections, 1002)
	state.Lifecycle.RewardToken = 0x1234
	state.Lifecycle.RewardDeliveryCommitted = true
	state.Lifecycle.RewardDelivery = &runtimeRaidRewardDeliveryCache{
		Phase: runtimeRaidDungeonPhaseOne,
		Token: state.Lifecycle.RewardToken,
	}
	queue := &runtimeRaidPhaseTestQueue{now: fixture.now}
	fixture.service.gameplayTimers = queue
	leaderConnection := &bufferConn{}
	fixture.leader.conn = leaderConnection
	_, completedDungeon, _, _, _ := prepareCompletedSettlementRuntime(t)
	completedDungeon.Character.CharacterID = "1001"
	completedDungeon.raidDungeon = runtimeRaidDungeonBinding{
		RaidKey:           fixture.raidKey,
		Phase:             runtimeRaidDungeonPhaseOne,
		MapID:             5314,
		LifecycleToken:    completedDungeon.lifecycleToken,
		selectedNotified:  true,
		loadedNotified:    true,
		completedNotified: true,
	}
	fixture.leader.dungeon.runtime = completedDungeon

	personalBody, err := raid.BuildRaidSettlementRewardsBody(raid.RaidSettlementRewards{
		ResultType: 1,
		Members: []raid.RaidSettlementMemberRewards{{
			MemberKey: 1001,
			CardIndex: 2,
			Rewards: []raid.RaidSettlementReward{
				{ItemIDOrValue: 150001, CountOrAux: 5},
				{ItemIDOrValue: 150003, CountOrAux: 2},
				{ItemIDOrValue: 150004, CountOrAux: 1},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	teamBody, err := raid.BuildRaidSettlementRewardsBody(raid.RaidSettlementRewards{
		ResultType: 3,
		Members: []raid.RaidSettlementMemberRewards{{
			MemberKey: 1001,
			CardIndex: 2,
			Rewards: []raid.RaidSettlementReward{
				{ItemIDOrValue: 150002, CountOrAux: 1},
				{ItemIDOrValue: 150005, CountOrAux: 1},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	delivery := runtimeRaidSettlementRewardDelivery{
		Raid:  cloneRuntimeRaidState(*state),
		Token: state.Lifecycle.RewardToken,
		Rewards: raid.RaidSettlementRewards{Members: []raid.RaidSettlementMemberRewards{{
			MemberKey: 1001,
			CardIndex: 2,
			Rewards: []raid.RaidSettlementReward{
				{ItemIDOrValue: 150001, CountOrAux: 5},
				{ItemIDOrValue: 150002, CountOrAux: 1},
			},
		}}},
		PersonalRewardBody: personalBody,
		TeamRewardBody:     teamBody,
	}
	if err := fixture.service.publishCommittedRuntimeRaidSettlementRewards(delivery, fixture.service); err != nil {
		t.Fatalf("publish single rewards: %v", err)
	}
	if fixture.service.raids[fixture.raidKey] == nil {
		t.Fatalf("single raid finalized before squad reward presentation")
	}
	personalTypes := runtimeRaidRewardResultTypes(t, leaderConnection.write.Bytes())
	if !reflect.DeepEqual(personalTypes, []byte{1}) {
		t.Fatalf("personal result types=%v want=[1] counts=%v", personalTypes, runtimeRaidRewardPacketCounts(t, leaderConnection.write.Bytes()))
	}
	if rows := fixtureRewardRowKinds(t, personalBody); len(rows) != 1 || len(rows[0]) != 3 {
		t.Fatalf("personal expanded rows=%v want one member with three rewards", rows)
	}
	if queue.scheduledDelay != runtimeRaidSingleSquadRewardDelay || queue.callback == nil {
		t.Fatalf("squad schedule delay=%s callback_nil=%t", queue.scheduledDelay, queue.callback == nil)
	}
	leaderConnection.write.Reset()

	squadCallback := queue.callback
	squadCallback(fixture.now.Add(runtimeRaidSingleSquadRewardDelay))
	squadTypes := runtimeRaidRewardResultTypes(t, leaderConnection.write.Bytes())
	if !reflect.DeepEqual(squadTypes, []byte{3}) {
		t.Fatalf("squad result types=%v want=[3]", squadTypes)
	}
	if rows := fixtureRewardRowKinds(t, teamBody); len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("team expanded rows=%v want one member with two rewards", rows)
	}
	if queue.scheduledDelay != runtimeRaidSingleRewardFinishDelay || queue.callback == nil {
		t.Fatalf("finish schedule delay=%s callback_nil=%t", queue.scheduledDelay, queue.callback == nil)
	}
	leaderConnection.write.Reset()

	finishCallback := queue.callback
	finishCallback(fixture.now.Add(runtimeRaidSingleSquadRewardDelay + runtimeRaidSingleRewardFinishDelay))
	packet, rest := splitGameServerUpperPacket(t, leaderConnection.write.Bytes())
	if packet.Header.MsgID != raid.RaidStateMsgID ||
		!bytes.Equal(packet.Body, []byte{runtimeRaidSettlementStateFinished, 0}) || len(rest) != 0 {
		t.Fatalf("finish packet msg=%#04x body=%x trailing=%d", packet.Header.MsgID, packet.Body, len(rest))
	}
	if fixture.service.raids[fixture.raidKey] != nil {
		t.Fatalf("single raid remained after state6 unlock")
	}
	if !completedDungeon.raidNativeSettlementFinished {
		t.Fatal("single Raid state6 did not release the native dungeon exit gate")
	}
}

func runtimeRaidRewardResultTypes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var result []byte
	for len(raw) != 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		if packet.Header.MsgID != raid.RaidRewardListMsgID {
			continue
		}
		if len(packet.Body) == 0 {
			t.Fatalf("empty raid reward body")
		}
		result = append(result, packet.Body[0])
	}
	return result
}

type runtimeRaidRewardTestFixture struct {
	service      *Service
	repositories dnfrepo.Group
	pvfSource    *antonRewardPVFRecordingSource
	raidKey      uint32
	now          time.Time
	leader       *gameSession
	member       *gameSession
}

// seedRuntimeRaidPhaseClearsForTest registers one cleared gate for each character in
// the phase-clear table, which is the state a phase can only settle from: the
// settlement pays exactly the members its clears registered
// (runtimeRaidRewardEligibleMembersLocked), so a fixture that opens the reward window
// has to carry the registration that produced it.
func seedRuntimeRaidPhaseClearsForTest(lifecycle *runtimeRaidLifecycleState, characterIDs ...uint16) {
	if lifecycle.PhaseClearCounts == nil {
		lifecycle.PhaseClearCounts = make(map[uint16]uint32)
	}
	for _, characterID := range characterIDs {
		lifecycle.PhaseClearCounts[characterID] = 1
	}
}

func newRuntimeRaidRewardTestFixture(t *testing.T, online bool) runtimeRaidRewardTestFixture {
	t.Helper()
	items, source, finalByDefinition := newAntonRewardPVFTestItems(t)
	// 10157777 是一阶段那张稀有 squad_item 行（[STATE REWARD] 标记 1），金牌用例要用它。
	for _, definitionID := range []uint32{10094735, 10157773, 10157777} {
		finalID := finalByDefinition[definitionID]
		reference := items.itemRefs[finalID]
		finalPath, err := antonRewardPVFReferencePath(reference)
		if err != nil {
			t.Fatalf("final item %d path: %v", finalID, err)
		}
		source.documents[finalPath] = "[stackable type]\n`[material]`\n[stack limit]\n999\n"
	}
	config := antonRewardPVFTestConfig()
	config.PhaseBreakSeconds = 600
	config.PhaseTimeoutSeconds = []int{2400, 2400}
	catalog, err := newAntonRewardPVFCatalog(items, config)
	if err != nil {
		t.Fatalf("build Anton reward catalog: %v", err)
	}

	repositories := dnfrepomemory.NewMemoryGroup()
	for _, characterID := range []string{"1001", "1002"} {
		accountID := "account-" + characterID
		if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
			AccountID: accountID,
			Metadata: map[string]string{
				"account_token_cera":       "100",
				"account_happy_token_cera": "100",
			},
		}); err != nil {
			t.Fatalf("seed raid reward account %s: %v", accountID, err)
		}
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: characterID,
			AccountID:   accountID,
		}); err != nil {
			t.Fatalf("seed character %s: %v", characterID, err)
		}
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: characterID,
			Slots:       make(map[string]dnfrepo.ItemStack),
		}); err != nil {
			t.Fatalf("seed inventory %s: %v", characterID, err)
		}
	}
	now := time.Date(2026, 8, 6, 6, 0, 0, 0, time.UTC)
	members := []runtimeRaidMemberState{
		{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0},
		{CharID: 1002, Name: "member", GroupIndex: 2, SlotOrder: 0},
	}
	lifecycle := newRuntimeRaidLifecycleState(now.Add(-time.Hour))
	lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	lifecycle.Phase = runtimeRaidDungeonPhaseOne
	lifecycle.RewardOpenedAt = now.Add(-time.Minute)
	lifecycle.RewardSelections[1001] = runtimeRaidRewardSelection{Mode: 0, Index: 2, ConfirmedAt: now.Add(-time.Minute)}
	lifecycle.RewardSelections[1002] = runtimeRaidRewardSelection{Mode: 0, Index: 3, ConfirmedAt: now.Add(-time.Minute)}
	// A phase only reaches its settlement through clears, and the settlement pays the
	// members those clears registered (runtimeRaidRewardEligibleMembersLocked): the
	// fixture seeds the registration that got it here.
	seedRuntimeRaidPhaseClearsForTest(&lifecycle, 1001, 1002)
	const raidKey = uint32(0x00c803e9)
	service := &Service{
		options:             options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:               make(map[uint32]*runtimeRaidState),
		raidAntonReward:     catalog,
		raidRewardCompleted: make(map[runtimeRaidRewardCompletionKey]runtimeRaidSettlementRewardCompletion),
		raidRewardRandom:    func() uint64 { return 0 },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:        raidKey,
		LeaderID:       1001,
		AntonConfig:    config,
		Members:        append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers:  append([]runtimeRaidMemberState(nil), members...),
		Started:        true,
		EntryCostPaid:  true,
		ChargedMembers: []uint16{1001, 1002},
		StartAttempt:   1,
		Lifecycle:      lifecycle,
	}

	fixture := runtimeRaidRewardTestFixture{
		service: service, repositories: repositories, pvfSource: source, raidKey: raidKey, now: now,
	}
	if online {
		channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10038}
		fixture.leader = &gameSession{conn: &bufferConn{}, channel: channel, accountID: "account-1001"}
		fixture.member = &gameSession{conn: &bufferConn{}, channel: channel, accountID: "account-1002"}
		service.bindGameSessionCharacter(fixture.leader, 1001)
		service.bindGameSessionCharacter(fixture.member, 1002)
	}
	return fixture
}

func loadRuntimeRaidRewardTestInventories(t *testing.T, repositories dnfrepo.Group) map[string]dnfrepo.InventoryRecord {
	t.Helper()
	records := make(map[string]dnfrepo.InventoryRecord, 2)
	for _, characterID := range []string{"1001", "1002"} {
		record, found, err := repositories.Inventory.Load(context.Background(), characterID)
		if err != nil || !found {
			t.Fatalf("load inventory %s found=%t err=%v", characterID, found, err)
		}
		records[characterID] = record
	}
	return records
}

func clearRuntimeRaidRewardTestDeliveryCache(service *Service, raidKey uint32) {
	service.raidMu.Lock()
	defer service.raidMu.Unlock()
	state := service.raids[raidKey]
	state.Lifecycle.RewardDelivery = nil
	state.Lifecycle.RewardDeliveryCommitted = false
	state.Lifecycle.RewardDeliveryCommittedAt = time.Time{}
}

func runtimeRaidRewardTestCharacterKey(characterID uint16) string {
	if characterID == 1001 {
		return "1001"
	}
	return "1002"
}

func runtimeRaidRewardPacketCounts(t *testing.T, raw []byte) map[uint16]int {
	t.Helper()
	counts := make(map[uint16]int)
	for len(raw) != 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		counts[packet.Header.MsgID]++
		raw = rest
	}
	return counts
}
