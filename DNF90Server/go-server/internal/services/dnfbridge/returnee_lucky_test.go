package dnfbridge

import (
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestReturneeEligibilityProgressAndClaims(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	c := &dnfonlineevent.ReturneeCatalog{}
	for i := range c.Missions {
		c.Missions[i] = dnfonlineevent.ReturneeMission{ID: uint32(i + 1), Condition: "clear quest", Required: 2, Reward: dnfonlineevent.AttendancePVFItemReward{ItemID: 100, Count: 1}}
	}
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "a"}); err != nil {
		t.Fatal(err)
	}
	ch := dnfrepo.CharacterRecord{AccountID: "a", CharacterID: "7", Level: 50, Stats: map[string]int64{}}
	if err := repos.Character.Save(ctx, ch); err != nil {
		t.Fatal(err)
	}
	cmd := dnfonlineevent.ReturneeCommand{AccountID: "a", CharacterID: "7", Now: now, Login: true, Deliver: func(box dnfrepo.MailboxRepository, r dnfonlineevent.AttendancePVFItemReward, key string) (string, error) {
		return dnfrepo.AppendSystemMail(ctx, box, dnfrepo.SystemMailDelivery{RecipientCharacterID: "7", Title: "returnee", Body: "reward", Source: "test", CompletionKey: key, CreatedAt: now, Attachments: []dnfrepo.MailAttachment{{ItemID: r.ItemID, Count: r.Count}}})
	}}
	settle := func() dnfonlineevent.ReturneeState {
		t.Helper()
		s, _, err := dnfonlineevent.SettleReturnee(ctx, repos, c, cmd)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if settle().Active() {
		t.Fatal("first observation manufactured an offline history")
	}
	cmd.Now = now.Add(24*time.Hour - time.Nanosecond)
	if settle().Active() {
		t.Fatal("less than 24 hours qualified")
	}
	// The previous login reset the elapsed-offline anchor.
	cmd.Now = cmd.Now.Add(24 * time.Hour)
	s := settle()
	if !s.Active() {
		t.Fatal("exactly 24 hours did not qualify")
	}
	cmd.Login = false
	cmd.Claim = 1
	if _, _, err := dnfonlineevent.SettleReturnee(ctx, repos, c, cmd); err == nil {
		t.Fatal("incomplete mission claimed")
	}
	cmd.Claim = 0
	ch, _, _ = repos.Character.Load(ctx, "7")
	ch.Stats[dnfonlineevent.ReturneeQuestCountKey] = 2
	if err := repos.Character.Save(ctx, ch); err != nil {
		t.Fatal(err)
	}
	s = settle()
	if s.Progress[0] != 2 {
		t.Fatalf("progress=%d", s.Progress[0])
	}
	if settle().Progress[0] != 2 {
		t.Fatal("same observation counted twice")
	}
	cmd.Claim = 1
	for i := 0; i < 2; i++ {
		s = settle()
		if s.Claimed != 1 {
			t.Fatal("claim not persisted")
		}
	}
	box, _, err := repos.Mailbox.Load(ctx, "7")
	if err != nil || len(box.Mails) != 1 {
		t.Fatalf("duplicate award: mails=%d err=%v", len(box.Mails), err)
	}
	b := buildCurrentReturneeBody(c, s)
	if binary.LittleEndian.Uint16(b[:]) != 4 || binary.LittleEndian.Uint32(b[16:]) != 1 || binary.LittleEndian.Uint32(b[20:]) != 2 || b[28] != 1 || b[29] != 1 {
		t.Fatalf("native mission row=%x", b[:32])
	}
	for cmd.Claim = 2; cmd.Claim <= 10; cmd.Claim++ {
		s = settle()
	}
	if !s.ActiveAt(cmd.Now) || s.Claimed != 0x3ff {
		t.Fatal("last reward prematurely ended the returnee window")
	}
	cmd.Claim = 10
	settle() // Replayed last reward must keep the receipt and grant no new mail.
	box, _, err = repos.Mailbox.Load(ctx, "7")
	if err != nil || len(box.Mails) != 10 {
		t.Fatalf("last reward replay: mails=%d err=%v", len(box.Mails), err)
	}
	cmd.Now = s.ExpiresAt
	if _, _, err := dnfonlineevent.SettleReturnee(ctx, repos, c, cmd); err == nil {
		t.Fatal("expired returnee period accepted a reward request")
	}
	cmd.CharacterID = "99"
	if _, _, err := dnfonlineevent.SettleReturnee(ctx, repos, c, cmd); err == nil {
		t.Fatal("foreign/missing character accepted")
	}
}

func TestReturneeLuckyMonsterAndQuestExperience(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		pvp, hardcore bool
		want          uint32
	}{{"ordinary", false, false, 20}, {"pvp", true, false, 0}, {"hardcore", false, true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, repos := prepareCurrentDungeonMonsterExperienceTest(t)
			ch, _, _ := repos.Character.Load(ctx, "99")
			ch.Job = "0"
			ch.Stats["grow_type"] = 0
			if tc.hardcore {
				ch.Stats["is_hardcore_mode"] = 1
			}
			if err := repos.Character.Save(ctx, ch); err != nil {
				t.Fatal(err)
			}
			s.luckyCharacterCatalog = &currentLuckyCatalog{Jobs: [][2]uint32{{0, 0}}, Monster: 10}
			session := &gameSession{selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: r}}
			if tc.pvp {
				session.channel.Type = 4
			}
			result, err := s.awardCurrentDungeonMonsterExperience(ctx, session, r, r.Room.Snapshot().Monsters[0])
			if err != nil {
				t.Fatal(err)
			}
			if result.LuckyCharacterBonus != tc.want || result.Character.Stats["exp"] != int64(200+tc.want) {
				t.Fatalf("bonus=%d exp=%d", result.LuckyCharacterBonus, result.Character.Stats["exp"])
			}
		})
	}
	repos := finishBridgeSeed(t)
	catalog := finishBridgeCatalog(t)
	ch, _, _ := repos.Character.Load(ctx, "19")
	ch.Stats["is_return_user"] = 1
	if err := repos.Character.Save(ctx, ch); err != nil {
		t.Fatal(err)
	}
	s := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}, questCatalog: catalog, repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }, luckyCharacterCatalog: &currentLuckyCatalog{Jobs: [][2]uint32{{2, 0}}, Quest: 10}}
	session := &gameSession{conn: &bufferConn{}, selectedCharacterID: 19, initialTownRouteCharacterID: 19, initialTownRouteStage: currentInitialTownRoutePlayerStateSent}
	request := dnfquest.FinishQuestRequest{QuestID: 3145, RewardSelectIndex: ^uint16(0), Multiplier: 1}
	for i := 0; i < 2; i++ {
		if err := s.handleCurrentFinishQuestWithResources(session, request, repos, catalog, finishBridgeProgression(t), finishBridgeAllocator(nil)); err != nil {
			t.Fatal(err)
		}
		ch, _, _ := repos.Character.Load(ctx, "19")
		if ch.Stats["exp"] != 110 || ch.Stats[dnfonlineevent.ReturneeQuestCountKey] != 1 {
			t.Fatalf("quest replay %d exp=%d count=%d", i, ch.Stats["exp"], ch.Stats[dnfonlineevent.ReturneeQuestCountKey])
		}
	}
}

func TestReturneeLuckyClearRewardAndReplay(t *testing.T) {
	ctx := context.Background()
	for _, party := range []bool{false, true} {
		t.Run(map[bool]string{false: "solo", true: "party"}[party], func(t *testing.T) {
			s, r, session, _, _ := prepareCompletedSettlementRuntime(t)
			repos, _ := s.repositoryGroup()
			ch, _, _ := repos.Character.Load(ctx, "99")
			ch.Job = "0"
			ch.Level = 1
			ch.Stats["grow_type"] = 0
			ch.Stats["exp"] = 90
			if err := repos.Character.Save(ctx, ch); err != nil {
				t.Fatal(err)
			}
			if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "99", Points: dnfrepo.SkillPointState{TotalSP: 10, RemainingSP: 10, SyncedLevel: 1}}); err != nil {
				t.Fatal(err)
			}
			if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "99", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
				t.Fatal(err)
			}
			s.luckyCharacterCatalog = &currentLuckyCatalog{Jobs: [][2]uint32{{0, 0}}, Clear: 15, Party: 10}
			r.Character = ch
			r.Dungeon.Metadata.BasisLevel.Set = true
			r.Dungeon.Metadata.BasisLevel.Value = 1
			r.Dungeon.Metadata.ExperienceIncreasingPoint.Set = true
			r.Dungeon.Metadata.ExperienceIncreasingPoint.Value = 1
			r.Request.Difficulty = 1
			r.DropOwner = newRuntimeDungeonDropOwner()
			r.settlementPlayResultReceived = true
			r.settlementPlayResultBody = make([]byte, currentDungeonPlayResultBaseSize)
			r.settlementPlayResultBody[currentDungeonPlayResultClientRankPointOffset] = 73
			r.startedAt = time.Unix(1700000000, 0)
			r.clearMapCompletionAt = r.startedAt.Add(12 * time.Second)
			want := int64(340)
			if party {
				r.partyDungeonOwner = &runtimePartyDungeonOwner{members: []runtimePartyDungeonMember{{userID: 99}, {userID: 98}}}
				want = 360
			}
			// Card allocation is covered by the card suite; freeze its state so this
			// check isolates the real progression transaction and party EXP roster.
			r.settlementCardRewardState = &dungeonCardState{}
			resources := currentDungeonSettlementTestResources(t)
			for i := 0; i < 2; i++ {
				r.settlementResultPlan = nil
				if i == 1 {
					// Eligibility can change while retrying the committed packet plan.
					s.luckyCharacterCatalog = nil
				}
				if err := s.produceCurrentDungeonSettlementPlanWithResourcesLocked(ctx, session, r, resources); err != nil {
					t.Fatal(err)
				}
				got, _, _ := repos.Character.Load(ctx, "99")
				if got.Stats["exp"] != want {
					t.Fatalf("replay %d exp=%d want=%d", i, got.Stats["exp"], want)
				}
				reward := r.settlementResultPlan.clearRewardSnapshot
				// ⚠ 本树口径与参考版不同（有意）：Base[0] 是**通关基础经验**
				// （= gain 200），不是"已提交总额减去已扣项"。活动加成（幸运角色
				// 30/50）并入已提交总额，不改写 Base[0]。
				//
				// 参考版那份断言是 Base[0]==230、Base[2]==组队 20，配的是它自己的
				// take() 拆分与另一套 Base 槽位含义；同一套 take() 在本树的成长契约
				// 用例里会算出 240，而那份断言要 200，两者不能同时成立，所以没有照搬。
				//
				// 这里钉住的是**重试不改写已提交分类**这一真正要点：第 1 次重试前
				// 幸运角色目录被清空（资格已变），Base 与已提交总额都必须与第 0 次一致。
				if reward.Base[0] != 200 {
					t.Fatalf("replay %d clear base=%v want base[0]=200", i, reward.Base)
				}
				// 已提交总额必须与第 0 次一致：幸运 15%（单人）或 15%+10%（组队）。
				// 组队那次的 50 是一次性取整的结果（floor(200*25%)），不是 30+20。
				wantCommitted := uint32(250)
				if party {
					wantCommitted = 270
				}
				if reward.CommittedExperienceGain != wantCommitted {
					t.Fatalf("replay %d committed=%d want=%d base=%v", i, reward.CommittedExperienceGain, wantCommitted, reward.Base)
				}
			}
		})
	}
}

func TestReturneeLuckyRealPVF(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := dnfonlineevent.LoadReturneeCatalog(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	gifts, err := dnfonlineevent.LoadReturneeGiftRules(archive, 2)
	if err != nil {
		t.Fatal(err)
	}
	if gifts.Level != 20 || gifts.Count != 100 || gifts.Limit != 5 || gifts.MaxPoints != 40000 || len(gifts.Points) != 6 {
		t.Fatalf("gift rules=%+v", gifts)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range c.Missions {
		d, err := items.ResolveItem(uint32(m.Reward.ItemID))
		if err != nil {
			t.Fatal(err)
		}
		d, err = currentPVFItemDefinitionForGrantAt(d, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("reward %d kind=%s deadline=%v", d.ItemID, d.Kind, d.ExpirationDate)
	}
	want := []uint32{2, 25, 23, 13, 24, 12, 7, 20, 21, 22}
	for i, id := range want {
		if c.Missions[i].ID != id {
			t.Fatalf("mission[%d]=%d", i, c.Missions[i].ID)
		}
	}
	text, err := archive.ReadText("etc/chn_activatecharacter.etc")
	if err != nil {
		t.Fatal(err)
	}
	lucky, err := parseCurrentLuckyCharacterCatalog(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(lucky.Jobs) != 178 || lucky.Monster != 10 || lucky.Clear != 15 || lucky.Party != 10 || lucky.Quest != 10 {
		t.Fatalf("unexpected lucky catalog: %+v", lucky)
	}
	body := buildCurrentLuckyCharacterBody(lucky)
	if len(body) != 4+178*8 || binary.LittleEndian.Uint32(body) != 178 {
		t.Fatal("lucky list wire layout")
	}
	ch := dnfrepo.CharacterRecord{CharacterID: "7", Job: "0", Level: 90, Stats: map[string]int64{"grow_type": 33}}
	if !currentLuckyCharacterMatches(lucky, ch) {
		t.Fatal("90-level second awakening should qualify")
	}
	ch.Stats["is_hardcore_mode"] = 1
	if currentLuckyCharacterMatches(lucky, ch) {
		t.Fatal("hardcore qualified")
	}
	if len(buildCurrentLuckyCharacterBody(nil)) != 4 {
		t.Fatal("empty list must clear previous state")
	}
}
