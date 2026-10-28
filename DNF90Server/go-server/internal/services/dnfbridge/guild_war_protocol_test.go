package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestGuildWarNativeRoutesRankingAndTimeout(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 12, 4, 0, 0, 0, time.UTC)
			queue := &runtimeRaidPhaseTestQueue{now: now}
			repos := dnfrepomemory.NewMemoryGroup()
			failure := &guildFailSettlement{base: repos.CharacterSettlement}
			repos.CharacterSettlement = failure
			r := dnfguild.NewRegistry()
			var g, enemy *dnfguild.Guild
			for i := 16; i < 19; i++ {
				c := dnfrepo.CharacterRecord{CharacterID: fmt.Sprint(i), AccountID: fmt.Sprint(i), Name: fmt.Sprint(i), Level: 90, Stats: map[string]int64{}}
				if err := repos.Character.Save(ctx, c); err != nil {
					t.Fatal(err)
				}
				v, err := r.Create(c, fmt.Sprint("guild", i), now)
				if err != nil {
					t.Fatal(err)
				}
				v.ActiveGuardian = 1
				v.Guardians = []dnfguild.Guardian{{ID: 1, Level: 11, Skills: [5]byte{2, 1}}}
				v.Mark = 13
				v.Experience = 100
				if i == 16 {
					g = v
					v.Experience = 50
				} else {
					enemy = v
				}
			}
			if err := dnfguild.Save(ctx, repos.Settings, r, now); err != nil {
				t.Fatal(err)
			}
			items, err := newPVFDungeonDropCatalog(warMissionSource())
			if err != nil {
				t.Fatal(err)
			}
			s := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}, gameplayTimers: queue, pvfItemCatalog: items, repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }}
			ss := &gameSession{conn: &bufferConn{}, selectedCharacterID: 16, accountID: "16", initialTownRouteCharacterID: 16, initialTownRouteStage: currentInitialTownRoutePlayerStateSent}
			conn := ss.conn.(*bufferConn)
			send := func(op uint16, body []byte, want byte) []byte {
				t.Helper()
				conn.write.Reset()
				h := guildGameplayModule().UpperHandlers[op]
				if legacy {
					h = guildGameplayModule().LegacyHandlers[op]
				}
				if err := h(s, ss, gameplayRequest{Opcode: op, Classification: 1, Body: body}); err != nil {
					t.Fatal(err)
				}
				ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
				value := []byte{1}
				if want != 0 {
					value = []byte{0, want}
				}
				if ack.Header.MsgID != op || !bytes.Equal(ack.Body, value) {
					t.Fatalf("op=%d ack=%x want=%x", op, ack.Body, value)
				}
				return rest
			}
			rest := send(1345, []byte{0, 0, 0, 0}, 0)
			for _, op := range []uint16{1297, 784, 1283} {
				p, tail := splitGameServerUpperPacket(t, rest)
				if p.Header.MsgID != op {
					t.Fatal(p.Header, op)
				}
				rest = tail
			}
			if len(rest) != 0 {
				t.Fatal("query tail")
			}
			send(1364, []byte{1}, 0)
			r, _ = dnfguild.Load(ctx, repos.Settings)
			if len(r.Guilds[g.ID].War.Rivals) != 2 {
				t.Fatal("real rivals not returned")
			}
			b := buildCurrentGuildWarOverviewBody(r.Guilds[g.ID], r)
			if b[80+27] != 13 {
				t.Fatal("rival mark")
			}
			match := make([]byte, 21)
			binary.LittleEndian.PutUint32(match[13:], 1)
			binary.LittleEndian.PutUint32(match[17:], enemy.ID)
			failure.fail = true
			send(841, match, 253)
			failure.fail = false
			r, _ = dnfguild.Load(ctx, repos.Settings)
			if r.Guilds[g.ID].War.Battle != nil {
				t.Fatal("failed match persisted")
			}
			rest = send(841, match, 0)
			for _, op := range []uint16{1297, 784, 779, 1261} {
				p, tail := splitGameServerUpperPacket(t, rest)
				if p.Header.MsgID != op {
					t.Fatal(p.Header, op)
				}
				if op == 779 && (len(p.Body) != 13983 || p.Body[23] != 13 || binary.LittleEndian.Uint32(p.Body[0x2E1D+68:]) != 1) {
					t.Fatal("enemy native snapshot")
				}
				if op == 1261 && (len(p.Body) != 68 || binary.LittleEndian.Uint32(p.Body[40:]) != 2 || binary.LittleEndian.Uint32(p.Body[48:]) != 2) {
					t.Fatal("inactive cannon/support advertised a countdown without a deadline")
				}
				rest = tail
			}
			if len(rest) != 0 || queue.callback == nil || queue.scheduledDelay != 900*time.Second {
				t.Fatal("matching timer/packets")
			}
			expire := queue.callback
			r, _ = dnfguild.Load(ctx, repos.Settings)
			key := r.Guilds[g.ID].War.Battle.Key
			send(841, match, 0)
			r, _ = dnfguild.Load(ctx, repos.Settings)
			if r.Guilds[g.ID].War.Battle.Key != key {
				t.Fatal("match retry replaced battle")
			}
			conn.write.Reset()
			if err = s.writeCurrentGuildRanking(ss, g); err != nil {
				t.Fatal(err)
			}
			p, tail := splitGameServerUpperPacket(t, conn.write.Bytes())
			if p.Header.MsgID != 413 || len(tail) != 0 || !bytes.Equal(p.Body, buildCurrentGuildRankingBody(16, 3, 50)) {
				t.Fatalf("rank %x", p.Body)
			}
			conn.write.Reset()
			if err = s.writeCurrentGuildRanking(ss, nil); err != nil {
				t.Fatal(err)
			}
			p, _ = splitGameServerUpperPacket(t, conn.write.Bytes())
			if !bytes.Equal(p.Body, buildCurrentGuildRankingBody(16, 0, 0)) {
				t.Fatal("left guild rank retained")
			}
			queue.now = now.Add(901 * time.Second)
			expire(queue.now)
			expire(queue.now)
			r, _ = dnfguild.Load(ctx, repos.Settings)
			if r.Guilds[g.ID].War.Battle != nil || r.Guilds[g.ID].War.AttackLosses != 1 || r.Guilds[enemy.ID].War.DefenceWins != 1 {
				t.Fatal("expiry double count")
			}
			// A queued old callback must not cancel the next matching generation.
			r.Guilds[g.ID].War.Results = nil
			if err = dnfguild.Save(ctx, repos.Settings, r, queue.now); err != nil {
				t.Fatal(err)
			}
			send(841, match, 0)
			expire(queue.now.Add(901 * time.Second))
			r, _ = dnfguild.Load(ctx, repos.Settings)
			if r.Guilds[g.ID].War.Battle == nil || r.Guilds[g.ID].War.Battle.Key == key {
				t.Fatal("stale timer ended new battle")
			}
			ss.accountID = "wrong"
			send(841, match, 253)
			ss.accountID = "16"
			send(841, match[:20], 250)
		})
	}
}
