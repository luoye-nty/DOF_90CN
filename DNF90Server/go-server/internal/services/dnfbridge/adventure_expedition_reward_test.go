package dnfbridge

import (
	"context"
	"encoding/binary"
	"os"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealAdventureExpeditionRewardOldSaveAndReplay(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	tables, err := adventuregroup.LoadComplete(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, balance := range []uint32{171, 9999} {
		t.Run(strconv.FormatUint(uint64(balance), 10), func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 13, 6, 3, 19, 0, time.UTC)
			repos := dnfrepomemory.NewMemoryGroup()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Slot: 0, Job: "0", Level: 90, Name: "expedition", Stats: map[string]int64{"grow_type": 0x22, "honor_expert_level": 41}}))
			state := adventuregroup.RuntimeState{Version: 1}
			state.Normalize(tables.Runtime(), now)
			state.ShopPoints[adventuregroup.ShopPointGlory] = balance
			encoded, err := state.Marshal()
			must(err)
			must(repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "dnf:1", UpdatedAt: now, Metadata: map[string]string{adventuregroup.RuntimeStateMetadataKey: encoded}}))
			conn := &bufferConn{}
			s := &Service{options: options{accountID: "dnf:1", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain}, adventureGroupTable: tables, repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }, gameplayTimers: &processGameplayTimeQueue{now: func() time.Time { return now }}}
			session := &gameSession{conn: conn, selectedCharacterID: 1}
			must(s.handleCurrentAdventureExpeditionStart(session, []byte{1, 0x60, 0x54, 0, 0, 1, 0, 0, 0, 2}))
			ack, _ := splitLongHengGameServerUpperPacket(t, conn.write.Bytes())
			if ack.Header.MsgID != 442 || ack.Body[0] != 1 {
				t.Fatalf("start=%x", ack.Body)
			}
			account, _, err := repos.Account.Load(ctx, "dnf:1")
			must(err)
			state, err = adventuregroup.ParseRuntimeState(account, tables.Runtime(), now)
			must(err)
			expedition := state.Expeditions["1"]
			if expedition.Reward != 1 {
				t.Fatalf("departure estimate=%d", expedition.Reward)
			}
			conn.write.Reset()
			must(s.handleCurrentAdventureExpeditionReward(session, []byte{1}))
			ack, _ = splitLongHengGameServerUpperPacket(t, conn.write.Bytes())
			if ack.Body[0] != 0 {
				t.Fatal("claimed before completion")
			}
			// Reproduce the persisted estimate written by the previous binary.
			expedition.Reward = 171
			state.Expeditions["1"] = expedition
			encoded, err = state.Marshal()
			must(err)
			account.Metadata[adventuregroup.RuntimeStateMetadataKey] = encoded
			must(repos.Account.Save(ctx, account))
			now = now.Add(6 * time.Hour)
			fresh := &Service{options: s.options, adventureGroupTable: tables, repositoryProvider: s.repositoryProvider, gameplayTimers: s.gameplayTimers}
			conn.write.Reset()
			must(fresh.handleCurrentAdventureExpeditionReward(session, []byte{1}))
			ack, rest := splitLongHengGameServerUpperPacket(t, conn.write.Bytes())
			grant := min(uint32(1), 9999-balance)
			if ack.Header.MsgID != 1408 || len(ack.Body) != 5 || ack.Body[0] != 1 || binary.LittleEndian.Uint32(ack.Body[1:]) != grant {
				t.Fatalf("claim=%x want=%d", ack.Body, grant)
			}
			push, rest := splitLongHengGameServerUpperPacket(t, rest)
			if push.Header.MsgID != 1342 {
				t.Fatal("missing expedition refresh")
			}
			push, _ = splitLongHengGameServerUpperPacket(t, rest)
			if push.Header.MsgID != 1340 {
				t.Fatal("missing balance refresh")
			}
			account, _, err = repos.Account.Load(ctx, "dnf:1")
			must(err)
			state, err = adventuregroup.ParseRuntimeState(account, tables.Runtime(), now)
			must(err)
			if len(state.Expeditions) != 0 || state.ShopPoints[adventuregroup.ShopPointGlory] != balance+grant {
				t.Fatalf("claimed state=%+v", state)
			}
			conn.write.Reset()
			must(fresh.handleCurrentAdventureExpeditionReward(session, []byte{1}))
			ack, _ = splitLongHengGameServerUpperPacket(t, conn.write.Bytes())
			if ack.Body[0] != 0 {
				t.Fatal("duplicate claim accepted")
			}
			after, _, err := repos.Account.Load(ctx, "dnf:1")
			must(err)
			if after.Metadata[adventuregroup.RuntimeStateMetadataKey] != account.Metadata[adventuregroup.RuntimeStateMetadataKey] {
				t.Fatal("replay changed balance")
			}
		})
	}
}
