package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestGuildWarRivalsCapturedRequests(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 12, 4, 0, 0, 0, time.UTC)
			repos := dnfrepomemory.NewMemoryGroup()
			failure := &guildFailSettlement{base: repos.CharacterSettlement}
			repos.CharacterSettlement = failure
			r := dnfguild.NewRegistry()
			master := dnfrepo.CharacterRecord{CharacterID: "16", AccountID: "16", Name: "master", Level: 90}
			if err := repos.Character.Save(ctx, master); err != nil {
				t.Fatal(err)
			}
			own, err := r.Create(master, "own", now)
			if err != nil {
				t.Fatal(err)
			}
			own.GuardianGP = 1000
			own.ActiveGuardian = 1
			own.Guardians = []dnfguild.Guardian{{ID: 1, Level: 22}}
			other, err := r.Create(dnfrepo.CharacterRecord{CharacterID: "17", Name: "other"}, "other", now)
			if err != nil {
				t.Fatal(err)
			}
			if err = dnfguild.Save(ctx, repos.Settings, r, now); err != nil {
				t.Fatal(err)
			}
			items, err := newPVFDungeonDropCatalog(warMissionSource())
			if err != nil {
				t.Fatal(err)
			}
			s := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}, gameplayTimers: &runtimeRaidPhaseTestQueue{now: now}, pvfItemCatalog: items, repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }}
			conn := &bufferConn{}
			ss := &gameSession{conn: conn, selectedCharacterID: 16, accountID: "16", initialTownRouteCharacterID: 16, initialTownRouteStage: currentInitialTownRoutePlayerStateSent}
			send := func(body []byte, code byte, count, gp uint32) {
				t.Helper()
				conn.write.Reset()
				h := guildGameplayModule().UpperHandlers[1364]
				if legacy {
					h = guildGameplayModule().LegacyHandlers[1364]
				}
				if err := h(s, ss, gameplayRequest{Opcode: 1364, Classification: 1, Body: body}); err != nil {
					t.Fatal(err)
				}
				ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
				want := []byte{1}
				if code != 0 {
					want = []byte{0, code}
				}
				if ack.Header.MsgID != 1364 || ack.Header.Classification != 1 || !bytes.Equal(ack.Body, want) {
					t.Fatalf("request=%x ack=%x want=%x", body, ack.Body, want)
				}
				if code == 0 {
					overview, tail := splitGameServerUpperPacket(t, rest)
					if overview.Header.MsgID != 1297 || binary.LittleEndian.Uint32(overview.Body[76:]) != count || overview.Body[80+27] != 13 {
						t.Fatal("successful selection omitted native rival count/mark")
					}
					personal, tail := splitGameServerUpperPacket(t, tail)
					if personal.Header.MsgID != 784 || len(tail) != 0 {
						t.Fatal("successful selection refresh order")
					}
				} else if len(rest) != 0 {
					t.Fatal("rejected selection sent success state")
				}
				persisted, err := dnfguild.Load(ctx, repos.Settings)
				if err != nil {
					t.Fatal(err)
				}
				g := persisted.Guilds[own.ID]
				if uint32(len(g.War.Rivals)) != count || g.GuardianGP != gp {
					t.Fatalf("request=%x rivals=%v GP=%d want=%d/%d", body, g.War.Rivals, g.GuardianGP, count, gp)
				}
			}
			// Captured at 06:02:30 (initial) and 06:04:04 (purchase).
			initial, _ := hex.DecodeString("0154050e000000750ad48cd30001")
			purchase, _ := hex.DecodeString("0154050e0000003ecb1dc8f90000")
			// The other persisted guild has no guardian, matching the live failure.
			send(initial[13:], 254, 0, 1000)
			send(purchase[13:], 254, 0, 1000)
			for _, malformed := range [][]byte{nil, {0, 0}, {1, 0, 0, 0}, {0, 0, 0, 0}} {
				send(malformed, 250, 0, 1000)
			}
			for value := 2; value < 256; value++ {
				send([]byte{byte(value)}, 250, 0, 1000)
			}
			other.ActiveGuardian = 1
			other.Guardians = []dnfguild.Guardian{{ID: 1, Level: 1}}
			other.Mark = 13
			for id := 18; id < 27; id++ {
				g, err := r.Create(dnfrepo.CharacterRecord{CharacterID: fmt.Sprint(id), Name: fmt.Sprint(id)}, fmt.Sprint("other", id), now)
				if err != nil {
					t.Fatal(err)
				}
				g.ActiveGuardian = 1
				g.Guardians = []dnfguild.Guardian{{ID: 1, Level: 1}}
				g.Mark = 13
			}
			if err = dnfguild.Save(ctx, repos.Settings, r, now); err != nil {
				t.Fatal(err)
			}
			failure.fail = true
			send(initial[13:], 253, 0, 1000)
			failure.fail = false
			send(initial[13:], 0, 8, 1000)
			send(initial[13:], 0, 8, 1000)
			failure.fail = true
			send(purchase[13:], 253, 8, 1000)
			failure.fail = false
			send(purchase[13:], 0, 10, 900)
			send(purchase[13:], 254, 10, 900)
		})
	}
}

func TestGuildWarRivalGuardianDetail(t *testing.T) {
	r := dnfguild.NewRegistry()
	own := &dnfguild.Guild{ID: 1, War: dnfguild.WarState{Rivals: []uint32{2}}}
	other := &dnfguild.Guild{ID: 2, Name: "十二", Mark: 13, ActiveGuardian: 2,
		Guardians: []dnfguild.Guardian{{ID: 1, Level: 22}, {ID: 2, Level: 31}}}
	r.Guilds[1], r.Guilds[2] = own, other
	for _, result := range []uint32{0, 1, 2} {
		own.War.Results = map[uint32]uint32{2: result}
		b := buildCurrentGuildWarOverviewBody(own, r)
		// D19F44/D19F7D expect the active ID and combined 1..50 level, not
		// the first summoned guardian or a separately encoded star level.
		if binary.LittleEndian.Uint32(b[120:]) != 2 || b[124] != 31 || binary.LittleEndian.Uint32(b[125:]) != result {
			t.Fatalf("guardian detail/result overlap: %x", b[120:129])
		}
		if !bytes.Equal(b[129:178], make([]byte, 49)) {
			t.Fatal("partial page fabricated another guild")
		}
	}
	other.ActiveGuardian = 0
	b := buildCurrentGuildWarOverviewBody(own, r)
	if !bytes.Equal(b[120:125], make([]byte, 5)) {
		t.Fatal("inactive guardian advertised as defending")
	}
}
