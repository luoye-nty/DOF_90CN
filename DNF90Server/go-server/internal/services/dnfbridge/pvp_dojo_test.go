package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvp "longheng.io/server/internal/modules/dnf/pvp"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/skillcmd"
)

func TestPVPDojoNativeSkillSnapshot(t *testing.T) {
	record := dnfrepo.SkillRecord{Skills: map[int64]dnfrepo.SkillState{46: {Level: 7}}, Points: dnfrepo.SkillPointState{RemainingSP: 105, RemainingTP: 9},
		Trees:   map[int]dnfrepo.SkillTreeState{1: {Skills: map[int64]dnfrepo.SkillState{47: {Level: 3}}, Points: dnfrepo.SkillPointState{RemainingSP: 93, RemainingTP: 6}}},
		Layouts: map[int]dnfrepo.SkillLayout{0: {198: 46}, 1: {150: 47}}}
	body, err := buildCurrentPVPDojoSkills(record)
	if err != nil || len(body) != 18 || body[0] != 1 || body[1] != 198 || binary.LittleEndian.Uint16(body[2:]) != 46 || body[4] != 7 || body[5] != 1 || body[6] != 150 || binary.LittleEndian.Uint16(body[7:]) != 47 || body[9] != 3 {
		t.Fatalf("dojo snapshot=%x %v", body, err)
	}
	for i, want := range []uint16{105, 93, 9, 6} {
		if got := binary.LittleEndian.Uint16(body[10+2*i:]); got != want {
			t.Fatal("dojo SP/TP order incorrect")
		}
	}
	record.Points.RemainingSP = 65536
	if _, err := buildCurrentPVPDojoSkills(record); err == nil {
		t.Fatal("overflow accepted")
	}
}

func TestCurrentPVPDojoSkillModeRestoreAndPersistence(t *testing.T) {
	s := pvpRoomTestService(t)
	session := &gameSession{conn: &bufferConn{}, accountID: "pvp-29", selectedCharacterID: 29, channel: s.catalog.Channels()[0]}
	check := func(want byte) {
		t.Helper()
		p, rest := splitCurrentGameServerUpperPacketAuto(t, session.conn.(*bufferConn).write.Bytes())
		if p.Header.MsgID != 1229 || p.Header.Classification != 0 || len(p.Body) != 1 || p.Body[0] != want || len(rest) != 0 {
			t.Fatalf("native dojo state=%+v", p)
		}
		session.conn.(*bufferConn).write.Reset()
	}
	if err := s.sendCurrentPVPDojoSkillMode(session); err != nil {
		t.Fatal(err)
	}
	check(0)
	for _, mode := range []byte{1, 2} {
		if err := s.handleCurrentPVPDojoSkillMode(session, []byte{mode}); err != nil {
			t.Fatal(err)
		}
		check(mode)
		if err := s.sendCurrentPVPDojoSkillMode(session); err != nil {
			t.Fatal(err)
		}
		check(mode)
	}
	for _, body := range [][]byte{nil, {0}, {3}, {1, 2}} {
		if err := s.handleCurrentPVPDojoSkillMode(session, body); err != nil || session.conn.(*bufferConn).write.Len() != 0 {
			t.Fatal("malformed dojo mode accepted")
		}
	}
	session.accountID = "other"
	if err := s.handleCurrentPVPDojoSkillMode(session, []byte{1}); err == nil {
		t.Fatal("unowned character changed")
	}
	repos, _ := s.repositoryGroup()
	character, _, _ := repos.Character.Load(context.Background(), "29")
	if dnfpvp.DojoSkillMode(character) != 2 {
		t.Fatal("invalid request changed saved mode")
	}
	session.accountID, session.selectedCharacterID = "pvp-30", 30
	if err := s.sendCurrentPVPDojoSkillMode(session); err != nil {
		t.Fatal(err)
	}
	check(0)
}

func TestPVPDojoResetRefreshUsesNativeManagerAfterAck(t *testing.T) {
	s := pvpRoomTestService(t)
	repos, _ := s.repositoryGroup()
	ctx := context.Background()
	record, _, err := repos.Skill.Load(ctx, "29")
	if err != nil {
		t.Fatal(err)
	}
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {0: 46}, 1: {150: 46}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := skillcmd.PrepareDojoSkills(ctx, repos, "29"); err != nil {
		t.Fatal(err)
	}
	record, _, err = repos.Skill.Load(ctx, "29")
	if err != nil {
		t.Fatal(err)
	}
	dnfrepo.SetSkillTree(&record, 2, dnfrepo.SkillTreeState{
		Skills: map[int64]dnfrepo.SkillState{47: {Level: 3, Enabled: true}},
		Points: dnfrepo.SkillPointState{RemainingSP: 93, RemainingTP: 6},
	})
	record.Layouts[2] = dnfrepo.SkillLayout{0: 47}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	session := &gameSession{conn: &bufferConn{}, accountID: "pvp-29", selectedCharacterID: 29,
		townPositionSnapshot: currentTownPositionSnapshot{CharacterID: 29, PositionValid: true, TownID: 10, AreaID: 4}}
	result := alignedcmd.Result{Operation: "skill_init", UpperResponses: []alignedcmd.UpperResponse{{
		MsgID: uint16(dnfenum.CmdPacketSkillInit), Classification: 1, Body: []byte{1, 0, 3},
	}}, PostActions: []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedActorSkills}}
	if err := s.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatal(err)
	}
	ack, wire := splitCurrentGameServerUpperPacketAuto(t, session.conn.(*bufferConn).write.Bytes())
	refresh, wire := splitCurrentGameServerUpperPacketAuto(t, wire)
	want, _, err := buildCurrentSceneSkillInfoBody(skillcmd.DojoSkillProjection(record), skillcmd.DojoSkillProjection(record).Layouts, 0)
	if err != nil || ack.Header.MsgID != uint16(dnfenum.CmdPacketSkillInit) || ack.Header.Classification != 1 ||
		refresh.Header.MsgID != 1231 || refresh.Header.Classification != 0 || !bytes.Equal(refresh.Body, want) || len(wire) != 0 {
		t.Fatalf("dojo reset route ack=%+v refresh=%+v error=%v", ack.Header, refresh.Header, err)
	}
	if slots := currentSkillInfoFirstTreeSlots(t, refresh.Body); slots[0] != 47 {
		t.Fatalf("dojo refresh loaded ordinary skills: %v", slots)
	}
	session.townPositionSnapshot.AreaID = 1
	if active, err := s.currentPVPDojoSkillsActive(session); err != nil || active {
		t.Fatalf("dojo mode leaked outside area4: %v %v", active, err)
	}
}
