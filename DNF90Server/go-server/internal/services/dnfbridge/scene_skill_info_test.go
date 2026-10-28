package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"longheng.io/server/internal/modules/dnf/alignedcmd"
	darkcombo "longheng.io/server/internal/modules/dnf/darkknightcombo"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	"longheng.io/server/internal/modules/dnf/skillcmd"
)

func TestSendCurrentSceneSkillInfoBackfillsEachJobsOwnPVFInitialSkills(t *testing.T) {
	catalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":     "0 `job0.lst`\n1 `job1.lst`\n",
		"skill/job0.lst":          "46 `job0/active_a.skl`\n47 `job0/active_b.skl`\n48 `job0/active_c.skl`\n49 `job0/passive.skl`\n",
		"skill/job1.lst":          "10 `job1/active_a.skl`\n20 `job1/active_b.skl`\n30 `job1/active_c.skl`\n40 `job1/passive.skl`\n",
		"skill/job0/active_a.skl": "[skill type]\n`active`\n",
		"skill/job0/active_b.skl": "[skill type]\n`active`\n",
		"skill/job0/active_c.skl": "[skill type]\n`active`\n",
		"skill/job0/passive.skl":  "[skill type]\n`passive`\n",
		"skill/job1/active_a.skl": "[skill type]\n`active`\n",
		"skill/job1/active_b.skl": "[skill type]\n`active`\n",
		"skill/job1/active_c.skl": "[skill type]\n`active`\n",
		"skill/job1/passive.skl":  "[skill type]\n`passive`\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		job       byte
		entries   []initialSkillEntry
		wantSlots map[int]uint16
		rowExists bool
	}{
		{
			name: "swordman missing row",
			job:  0,
			entries: []initialSkillEntry{
				{SkillID: 46, Level: 1}, {SkillID: 47, Level: 1}, {SkillID: 48, Level: 1}, {SkillID: 49, Level: 1},
			},
			wantSlots: map[int]uint16{0: 46, 1: 47, 2: 48, 54: 49},
		},
		{
			name: "fighter empty row",
			job:  1,
			entries: []initialSkillEntry{
				{SkillID: 10, Level: 1}, {SkillID: 20, Level: 1}, {SkillID: 30, Level: 1}, {SkillID: 40, Level: 1},
			},
			wantSlots: map[int]uint16{0: 10, 1: 20, 2: 30, 54: 40},
			rowExists: true,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			characterID := string(rune('1' + index))
			character := dnfrepo.CharacterRecord{CharacterID: characterID, Job: string(rune('0' + test.job)), Level: 1}
			if test.rowExists {
				if err := repositories.Skill.Save(context.Background(), dnfrepo.SkillRecord{CharacterID: characterID}); err != nil {
					t.Fatal(err)
				}
			}
			service := &Service{
				repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
				initialSkillsByJob: map[byte][]initialSkillEntry{test.job: test.entries},
				initialSPTable:     map[int]int{1: 20},
				skillCatalog:       catalog,
			}
			connection := &bufferConn{}
			session := &gameSession{conn: connection}
			if err := service.sendCurrentSceneSkillInfo(session, context.Background(), character, "test_all_job_backfill"); err != nil {
				t.Fatal(err)
			}
			packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(rest) != 0 || packet.Header.Classification != 0 || packet.Header.MsgID != currentSkillInfoMsgID {
				t.Fatalf("packet=%+v rest=%x", packet.Header, rest)
			}
			if got := currentSkillInfoFirstTreeSlots(t, packet.Body); !reflect.DeepEqual(got, test.wantSlots) {
				t.Fatalf("job %d slots=%v, want %v", test.job, got, test.wantSlots)
			}

			saved, found, err := repositories.Skill.Load(context.Background(), characterID)
			if err != nil || !found {
				t.Fatalf("saved found=%t err=%v", found, err)
			}
			if len(saved.Skills) != len(test.entries) || saved.Points.TotalSP != 20 || saved.Points.RemainingSP != 20 || saved.Points.SyncedLevel != 1 {
				t.Fatalf("saved=%+v", saved)
			}
			if !reflect.DeepEqual(saved.Layouts[currentSkillInfoTreeIndex], dnfrepo.SkillLayout(test.wantSlots)) {
				t.Fatalf("saved layout=%v, want %v", saved.Layouts[currentSkillInfoTreeIndex], test.wantSlots)
			}
		})
	}
}

func TestSendCurrentSceneSkillInfoDoesNotReplaceExistingLearnedSkills(t *testing.T) {
	catalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":    "0 `job0.lst`\n",
		"skill/job0.lst":         "46 `job0/initial.skl`\n99 `job0/learned.skl`\n",
		"skill/job0/initial.skl": "[skill type]\n`active`\n",
		"skill/job0/learned.skl": "[skill type]\n`active`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	existing := dnfrepo.SkillRecord{
		CharacterID: "77",
		Skills:      map[int64]dnfrepo.SkillState{99: {Level: 3, Enabled: true}},
		Layouts:     map[int]dnfrepo.SkillLayout{0: {4: 99}},
	}
	if err := repositories.Skill.Save(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		initialSkillsByJob: map[byte][]initialSkillEntry{0: {{SkillID: 46, Level: 1}}},
		initialSPTable:     map[int]int{1: 20},
		skillCatalog:       catalog,
	}
	connection := &bufferConn{}
	if err := service.sendCurrentSceneSkillInfo(&gameSession{conn: connection}, context.Background(), dnfrepo.CharacterRecord{CharacterID: "77", Job: "0", Level: 1}, "test_preserve_existing"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("rest=%x", rest)
	}
	if got := currentSkillInfoFirstTreeSlots(t, packet.Body); !reflect.DeepEqual(got, map[int]uint16{4: 99}) {
		t.Fatalf("wire slots=%v", got)
	}
	saved, found, err := repositories.Skill.Load(context.Background(), "77")
	alternate, alternateFound := dnfrepo.SkillTree(saved, 1)
	if err != nil || !found || !reflect.DeepEqual(saved.Skills, existing.Skills) ||
		!reflect.DeepEqual(saved.Layouts[0], existing.Layouts[0]) ||
		!alternateFound || alternate.Skills[46].Level != 1 || saved.Layouts[1][0] != 46 {
		t.Fatalf("saved found=%t err=%v record=%+v", found, err, saved)
	}
}

func TestSendCurrentSceneSkillInfoRestoresProfessionBaselineAcrossBothTrees(t *testing.T) {
	profiles, catalog := finishBridgeProfessionResources(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	record := dnfrepo.SkillRecord{
		CharacterID: "77",
		Skills:      map[int64]dnfrepo.SkillState{1: {Level: 1, Enabled: true}},
		Points:      dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 40, SyncedLevel: 1},
	}
	if err := repositories.Skill.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		initialSkillsByJob: map[byte][]initialSkillEntry{2: {{SkillID: 1, Level: 1}}},
		initialSPTable:     map[int]int{1: 100},
		skillCatalog:       catalog,
		professionProfiles: profiles,
	}
	connection := &bufferConn{}
	character := dnfrepo.CharacterRecord{
		CharacterID: "77",
		Job:         "2",
		Level:       1,
		Stats:       map[string]int64{"grow_type": 2},
	}
	if err := service.sendCurrentSceneSkillInfo(&gameSession{conn: connection}, context.Background(), character, "test_profession_baseline"); err != nil {
		t.Fatal(err)
	}
	saved, found, err := repositories.Skill.Load(context.Background(), "77")
	if err != nil || !found {
		t.Fatalf("saved found=%t err=%v", found, err)
	}
	primary, primaryFound := dnfrepo.SkillTree(saved, 0)
	alternate, alternateFound := dnfrepo.SkillTree(saved, 1)
	for _, tree := range []struct {
		name  string
		state dnfrepo.SkillTreeState
		found bool
	}{
		{name: "primary", state: primary, found: primaryFound},
		{name: "alternate", state: alternate, found: alternateFound},
	} {
		if !tree.found || len(tree.state.Skills) != 3 || tree.state.Skills[1].Level != 1 ||
			tree.state.Skills[2].Level != 1 || tree.state.Skills[197].Level != 1 {
			t.Fatalf("%s profession baseline=%+v found=%t", tree.name, tree.state, tree.found)
		}
	}
	if primary.Points.RemainingSP != 40 || alternate.Points.RemainingSP != 100 {
		t.Fatalf("profession baseline changed skill ledgers primary=%+v alternate=%+v", primary.Points, alternate.Points)
	}
}

func TestSendCurrentSceneSkillInfoPersistsInitialLayoutWithAtMostThreeActiveQuickSlots(t *testing.T) {
	catalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":    "0 `job0.lst`\n",
		"skill/job0.lst":         "5 `job0/a.skl`\n8 `job0/b.skl`\n46 `job0/c.skl`\n108 `job0/d.skl`\n174 `job0/passive.skl`\n",
		"skill/job0/a.skl":       "[skill type]\n`active`\n",
		"skill/job0/b.skl":       "[skill type]\n`active`\n",
		"skill/job0/c.skl":       "[skill type]\n`active`\n",
		"skill/job0/d.skl":       "[skill type]\n`active`\n",
		"skill/job0/passive.skl": "[skill type]\n`passive`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := []initialSkillEntry{
		{SkillID: 5, Level: 1}, {SkillID: 8, Level: 1}, {SkillID: 46, Level: 1}, {SkillID: 108, Level: 1}, {SkillID: 174, Level: 1},
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	record := dnfrepo.SkillRecord{CharacterID: "99", Skills: map[int64]dnfrepo.SkillState{}}
	for _, entry := range entries {
		record.Skills[entry.SkillID] = dnfrepo.SkillState{Level: entry.Level, Enabled: true}
	}
	if err := repositories.Skill.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		initialSkillsByJob: map[byte][]initialSkillEntry{0: entries},
		initialSPTable:     map[int]int{1: 20},
		skillCatalog:       catalog,
	}
	connection := &bufferConn{}
	if err := service.sendCurrentSceneSkillInfo(&gameSession{conn: connection}, context.Background(), dnfrepo.CharacterRecord{CharacterID: "99", Job: "0", Level: 1}, "test_initial_layout"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("rest=%x", rest)
	}
	want := map[int]uint16{0: 5, 1: 8, 2: 46, 54: 174, 150: 108}
	if got := currentSkillInfoFirstTreeSlots(t, packet.Body); !reflect.DeepEqual(got, want) {
		t.Fatalf("wire slots=%v, want %v", got, want)
	}
	saved, found, err := repositories.Skill.Load(context.Background(), "99")
	if err != nil || !found || !reflect.DeepEqual(saved.Layouts[0], dnfrepo.SkillLayout(want)) {
		t.Fatalf("saved found=%t err=%v layout=%v", found, err, saved.Layouts[0])
	}
}

func TestBuildCurrentSceneSkillInfoBodyEncodesIndependentDirectLevelsAcrossBothTrees(t *testing.T) {
	record := dnfrepo.SkillRecord{
		Skills: map[int64]dnfrepo.SkillState{
			1:   {Level: 1, Enabled: true},
			179: {Level: 7, Enabled: true},
		},
		Points: dnfrepo.SkillPointState{RemainingSP: 12, RemainingTP: 3},
		Trees: map[int]dnfrepo.SkillTreeState{
			1: {
				Skills: map[int64]dnfrepo.SkillState{
					50:  {Level: 2, Enabled: true},
					511: {Level: 4, Enabled: true},
				},
				Points: dnfrepo.SkillPointState{RemainingSP: 8, RemainingTP: 1},
			},
		},
	}
	layouts := map[int]dnfrepo.SkillLayout{
		0: {0: 1, 7: 179},
		1: {198: 50, 199: 511},
	}
	body, activeSlots, err := buildCurrentSceneSkillInfoBody(record, layouts, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 4 || int(binary.LittleEndian.Uint32(body[:4])) != len(body)-4 {
		t.Fatalf("skill-info length prefix=%x body_len=%d", body[:4], len(body))
	}
	if !reflect.DeepEqual(activeSlots, []int{0}) {
		t.Fatalf("active quick slots=%v, want [0]", activeSlots)
	}

	outerVarints, outerMessages := consumeCurrentSkillInfoFields(t, body[4:])
	if got := outerVarints[1]; !reflect.DeepEqual(got, []uint64{currentSkillInfoMessageType}) {
		t.Fatalf("outer field1=%v", got)
	}
	if len(outerMessages[2]) != currentSkillInfoTreeCount {
		t.Fatalf("skill tree count=%d, want %d", len(outerMessages[2]), currentSkillInfoTreeCount)
	}
	wantPoints := [][2]uint64{{12, 3}, {8, 1}}
	wantSkills := []map[int][2]uint64{
		{0: {1, 1}, 7: {179, 7}},
		{198: {50, 2}, 199: {511, 4}},
	}
	for treeIndex, rawTree := range outerMessages[2] {
		treeVarints, treeMessages := consumeCurrentSkillInfoFields(t, rawTree)
		if !reflect.DeepEqual(treeVarints[1], []uint64{wantPoints[treeIndex][0]}) || !reflect.DeepEqual(treeVarints[2], []uint64{wantPoints[treeIndex][1]}) {
			t.Fatalf("tree %d points sp=%v tp=%v", treeIndex, treeVarints[1], treeVarints[2])
		}
		if len(treeMessages[3]) != len(wantSkills[treeIndex]) {
			t.Fatalf("tree %d learned skill count=%d, want %d", treeIndex, len(treeMessages[3]), len(wantSkills[treeIndex]))
		}
		got := make(map[int][2]uint64, len(treeMessages[3]))
		for _, raw := range treeMessages[3] {
			fields, nested := consumeCurrentSkillInfoFields(t, raw)
			if len(nested) != 0 || len(fields[1]) != 1 || len(fields[2]) != 1 || len(fields[3]) != 1 || len(fields[4]) != 0 {
				t.Fatalf("tree %d invalid skill fields=%v nested=%v", treeIndex, fields, nested)
			}
			got[int(fields[1][0])] = [2]uint64{fields[2][0], fields[3][0]}
		}
		if !reflect.DeepEqual(got, wantSkills[treeIndex]) {
			t.Fatalf("tree %d skill slot map=%v, want %v", treeIndex, got, wantSkills[treeIndex])
		}
	}
}

func TestBuildCurrentSceneSkillInfoBodyRejectsRetiredSeventhSlots(t *testing.T) {
	record := dnfrepo.SkillRecord{
		Skills: map[int64]dnfrepo.SkillState{231: {Level: 8, Enabled: true}},
		Trees:  map[int]dnfrepo.SkillTreeState{1: {Skills: map[int64]dnfrepo.SkillState{}}},
	}
	for _, slot := range []int{204, 205} {
		_, _, err := buildCurrentSceneSkillInfoBody(record, map[int]dnfrepo.SkillLayout{0: {slot: 231}}, 0)
		if err == nil {
			t.Fatalf("retired seventh slot %d was serialized for stock client", slot)
		}
	}
}

func TestBuildCurrentSceneSkillInfoBodyRestoresPersistedCommandBytes(t *testing.T) {
	record := dnfrepo.SkillRecord{
		Skills:   map[int64]dnfrepo.SkillState{46: {Level: 3, Enabled: true}},
		Trees:    map[int]dnfrepo.SkillTreeState{1: {Skills: map[int64]dnfrepo.SkillState{46: {Level: 2, Enabled: true}}}},
		Commands: map[int64][]byte{46: {0, 4, 0xff}},
	}
	body, _, err := buildCurrentSceneSkillInfoBody(record, map[int]dnfrepo.SkillLayout{0: {0: 46}, 1: {150: 46}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, outerMessages := consumeCurrentSkillInfoFields(t, body[4:])
	for treeIndex, rawTree := range outerMessages[2] {
		_, treeMessages := consumeCurrentSkillInfoFields(t, rawTree)
		if len(treeMessages[3]) != 1 {
			t.Fatalf("tree %d skill count=%d", treeIndex, len(treeMessages[3]))
		}
		fields, nested := consumeCurrentSkillInfoFields(t, treeMessages[3][0])
		if len(nested) != 0 || !reflect.DeepEqual(fields[4], []uint64{0, 4, 0xff}) {
			t.Fatalf("tree %d command field=%v nested=%v", treeIndex, fields[4], nested)
		}
	}
}

func TestSkillCommandSaveRoundTripRehydratesSceneProjection(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "account-command",
		Job:         "0",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77",
		Skills: map[int64]dnfrepo.SkillState{
			248: {Level: 1, Enabled: true},
		},
		Trees: map[int]dnfrepo.SkillTreeState{
			1: {Skills: map[int64]dnfrepo.SkillState{}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Current NoPack capture: one modified record, skill 248, command bytes
	// 03 01 08. The leading 01 is the record count, not a tree selector.
	body := []byte{1, 248, 0, 3, 3, 1, 8}
	result, err := skillcmd.NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketSkillCommandCustomizing),
		Body:                body,
		AccountID:           "account-command",
		SelectedCharacterID: 77,
		Repositories:        repositories,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 {
		t.Fatalf("save result=%+v", result)
	}

	persisted, found, err := repositories.Skill.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load persisted skill found=%t err=%v", found, err)
	}
	projected, _, err := buildCurrentSceneSkillInfoBody(
		persisted,
		map[int]dnfrepo.SkillLayout{0: {0: 248}, 1: {}},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, outerMessages := consumeCurrentSkillInfoFields(t, projected[4:])
	if len(outerMessages[2]) != currentSkillInfoTreeCount {
		t.Fatalf("tree count=%d", len(outerMessages[2]))
	}
	_, treeMessages := consumeCurrentSkillInfoFields(t, outerMessages[2][0])
	if len(treeMessages[3]) != 1 {
		t.Fatalf("active tree skill count=%d", len(treeMessages[3]))
	}
	fields, nested := consumeCurrentSkillInfoFields(t, treeMessages[3][0])
	if len(nested) != 0 || !reflect.DeepEqual(fields[4], []uint64{3, 1, 8}) {
		t.Fatalf("rehydrated command=%v nested=%v", fields[4], nested)
	}
}

func TestBuildCurrentSceneSkillInfoBodyKeepsPersistedCommandBytes(t *testing.T) {
	record := dnfrepo.SkillRecord{
		Skills: map[int64]dnfrepo.SkillState{
			118: {Level: 1, Enabled: true},
			46:  {Level: 1, Enabled: true},
		},
		Trees: map[int]dnfrepo.SkillTreeState{1: {Skills: map[int64]dnfrepo.SkillState{
			118: {Level: 1, Enabled: true},
			5:   {Level: 1, Enabled: true},
		}}},
		Commands: map[int64][]byte{
			118: {0xff},
			46:  {4},
		},
	}
	body, _, err := buildCurrentSceneSkillInfoBody(
		record,
		map[int]dnfrepo.SkillLayout{
			0: {0: 118, 150: 46},
			1: {0: 118, 150: 5},
		},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, outerMessages := consumeCurrentSkillInfoFields(t, body[4:])
	if len(outerMessages[2]) != currentSkillInfoTreeCount {
		t.Fatalf("tree count=%d", len(outerMessages[2]))
	}
	for treeIndex, rawTree := range outerMessages[2] {
		_, treeMessages := consumeCurrentSkillInfoFields(t, rawTree)
		fieldsBySkill := make(map[uint16]map[protowire.Number][]uint64)
		for _, rawSkill := range treeMessages[3] {
			fields, _ := consumeCurrentSkillInfoFields(t, rawSkill)
			fieldsBySkill[uint16(fields[2][0])] = fields
		}
		if !reflect.DeepEqual(fieldsBySkill[118][4], []uint64{0xff}) {
			t.Fatalf("tree %d root command field=%v want=[255]", treeIndex, fieldsBySkill[118][4])
		}
		if treeIndex == 0 && !reflect.DeepEqual(fieldsBySkill[46][4], []uint64{4}) {
			t.Fatalf("ordinary command field=%v want=[4]", fieldsBySkill[46][4])
		}
	}
}

func TestBuildCurrentSceneSkillInfoBodyUsesPerTreeDarkKnightComboChildren(t *testing.T) {
	record := dnfrepo.SkillRecord{
		Skills: map[int64]dnfrepo.SkillState{
			118: {Level: 1, Enabled: true},
			46:  {Level: 1, Enabled: true},
		},
		Trees: map[int]dnfrepo.SkillTreeState{1: {Skills: map[int64]dnfrepo.SkillState{
			118: {Level: 1, Enabled: true},
			5:   {Level: 1, Enabled: true},
		}}},
		Commands: map[int64][]byte{
			118: {0xff},
			46:  {4},
		},
	}
	pages := []darkcombo.Page{
		{Index: 0, Roots: []darkcombo.Root{{SkillID: 118, Children: []uint16{46, 300}}}},
		{Index: 1, Roots: []darkcombo.Root{{SkillID: 118, Children: []uint16{5}}}},
	}
	body, _, err := buildCurrentSceneSkillInfoBodyWithDarkKnightCombo(
		record,
		map[int]dnfrepo.SkillLayout{0: {0: 118, 150: 46}, 1: {0: 118, 150: 5}},
		0,
		pages,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, outerMessages := consumeCurrentSkillInfoFields(t, body[4:])
	wantRootChildren := [][]uint64{{46, 300}, {5}}
	for treeIndex, rawTree := range outerMessages[2] {
		_, treeMessages := consumeCurrentSkillInfoFields(t, rawTree)
		fieldsBySkill := make(map[uint16]map[protowire.Number][]uint64)
		for _, rawSkill := range treeMessages[3] {
			fields, _ := consumeCurrentSkillInfoFields(t, rawSkill)
			fieldsBySkill[uint16(fields[2][0])] = fields
		}
		if !reflect.DeepEqual(fieldsBySkill[118][4], wantRootChildren[treeIndex]) {
			t.Fatalf("tree %d root children=%v want=%v", treeIndex, fieldsBySkill[118][4], wantRootChildren[treeIndex])
		}
		if treeIndex == 0 && !reflect.DeepEqual(fieldsBySkill[46][4], []uint64{4}) {
			t.Fatalf("ordinary command field=%v want=[4]", fieldsBySkill[46][4])
		}
	}
}

func TestBuildCurrentSceneSkillInfoBodyUsesEachPersistedTreeLayout(t *testing.T) {
	record := dnfrepo.SkillRecord{
		Skills: map[int64]dnfrepo.SkillState{46: {Level: 3, Enabled: true}},
		Trees: map[int]dnfrepo.SkillTreeState{
			1: {Skills: map[int64]dnfrepo.SkillState{99: {Level: 2, Enabled: true}}},
		},
	}
	body, activeSlots, err := buildCurrentSceneSkillInfoBody(record, map[int]dnfrepo.SkillLayout{
		0: {0: 46},
		1: {198: 99},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(activeSlots, []int{198}) {
		t.Fatalf("active tree quick slots=%v, want [198]", activeSlots)
	}
	_, outerMessages := consumeCurrentSkillInfoFields(t, body[4:])
	want := []map[int]uint16{{0: 46}, {198: 99}}
	for treeIndex, rawTree := range outerMessages[2] {
		_, treeMessages := consumeCurrentSkillInfoFields(t, rawTree)
		got := make(map[int]uint16, len(treeMessages[3]))
		for _, rawSkill := range treeMessages[3] {
			fields, _ := consumeCurrentSkillInfoFields(t, rawSkill)
			got[int(fields[1][0])] = uint16(fields[2][0])
		}
		if !reflect.DeepEqual(got, want[treeIndex]) {
			t.Fatalf("tree %d layout=%v, want %v", treeIndex, got, want[treeIndex])
		}
	}
}

func TestDarkKnightSkillProjectionInstallsFixedRootsAndMovesOrdinarySkillsToQWERTY(t *testing.T) {
	ctx := context.Background()
	catalog, err := buildSkillCatalogFromSource(ctx, initialEquipmentMemSource{
		"skill/skilllist.lst": "9 `demonicswordman/skilllist.lst`\n",
		"skill/demonicswordman/skilllist.lst": "5 `five.skl`\n" +
			"8 `eight.skl`\n" +
			"46 `forty_six.skl`\n" +
			"118 `comboset0.skl`\n" +
			"119 `comboset1.skl`\n",
		"skill/demonicswordman/five.skl":      "[skill type]\n`active`\n",
		"skill/demonicswordman/eight.skl":     "[skill type]\n`active`\n",
		"skill/demonicswordman/forty_six.skl": "[skill type]\n`active`\n",
		"skill/demonicswordman/comboset0.skl": "[combo set]\n5 8\n[/combo set]\n",
		"skill/demonicswordman/comboset1.skl": "[combo set]\n46\n[/combo set]\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "29",
		AccountID:   "account-1",
		Job:         "9",
		Level:       1,
		Stats:       map[string]int64{"skill_tree_index": 0},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	states := map[int64]dnfrepo.SkillState{
		5:   {Level: 1, Enabled: true},
		8:   {Level: 1, Enabled: true},
		46:  {Level: 1, Enabled: true},
		118: {Level: 1, Enabled: true},
		119: {Level: 1, Enabled: true},
	}
	points := dnfrepo.SkillPointState{SyncedLevel: 1}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "29",
		Skills:      states,
		Points:      points,
		Trees: map[int]dnfrepo.SkillTreeState{
			1: {Skills: states, Points: points},
		},
		Layouts: map[int]dnfrepo.SkillLayout{
			0: {0: 5, 1: 8, 2: 46, 150: 118, 151: 119},
			1: {0: 46, 1: 8, 2: 5, 150: 118, 151: 119},
		},
	}); err != nil {
		t.Fatal(err)
	}
	pages := []darkcombo.Page{
		{Index: 0, Roots: []darkcombo.Root{{SkillID: 118, Children: []uint16{5, 8}}, {SkillID: 119}}},
		{Index: 1, Roots: []darkcombo.Root{{SkillID: 118}, {SkillID: 119, Children: []uint16{46}}}},
	}
	if err := darkcombo.SavePages(ctx, repositories.Settings, "29", pages, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		skillCatalog:       catalog,
		initialSPTable:     map[int]int{1: 0},
		initialTPTable:     map[int]int{1: 0},
	}
	projection, ready, err := service.buildCurrentSceneSkillInfoProjection(
		&gameSession{accountID: "account-1", selectedCharacterID: 29},
		ctx,
		character,
		"test_dark_knight_exclusive_combo_slots",
	)
	if err != nil || !ready {
		t.Fatalf("ready=%t error=%v", ready, err)
	}
	if !reflect.DeepEqual(projection.activeSlots, []int{0, 1, 198, 199, 200}) ||
		!reflect.DeepEqual(projection.quickSkillIDs, []uint16{118, 119, 5, 8, 46}) {
		t.Fatalf("active slots=%v skills=%v", projection.activeSlots, projection.quickSkillIDs)
	}
	persisted, found, err := repositories.Skill.Load(ctx, "29")
	if err != nil || !found {
		t.Fatalf("found=%t error=%v", found, err)
	}
	for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
		if persisted.Layouts[tree][0] != 118 || persisted.Layouts[tree][1] != 119 {
			t.Fatalf("tree %d fixed roots=%v", tree, persisted.Layouts[tree])
		}
		for slot := 2; slot < 6; slot++ {
			if _, occupied := persisted.Layouts[tree][slot]; occupied {
				t.Fatalf("tree %d retained ordinary skill in unlearned combo slot %d: %v", tree, slot, persisted.Layouts[tree])
			}
		}
		for _, slot := range []int{198, 199, 200} {
			if _, occupied := persisted.Layouts[tree][slot]; !occupied {
				t.Fatalf("tree %d did not migrate ordinary shortcut to QWERTY slot %d: %v", tree, slot, persisted.Layouts[tree])
			}
		}
	}
}

func TestPreparedInitialTownSkillInfoCommitsWithoutDuplicateOp19(t *testing.T) {
	connection := &bufferConn{}
	session := &gameSession{
		conn:                         connection,
		selectedCharacterID:          29,
		initialTownSkillInfoPrepared: true,
		initialTownSkillInfoSent:     true,
		initialTownSkillInfo: currentSceneSkillInfoProjection{
			body:        []byte{1, 0, 0, 0, 0x08, 0x21},
			characterID: "29",
			activeTree:  1,
		},
	}
	service := &Service{}
	if err := service.commitPreparedInitialTownSkillInfoAfterTransition(session, "test_post_op24"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("post-op24 commit emitted duplicate skill packets=%x", connection.write.Bytes())
	}
	if session.initialTownSkillInfoPrepared || !session.initialTownSkillInfoSent || len(session.initialTownSkillInfo.body) != 0 {
		t.Fatalf("prepared=%t sent=%t retained=%d", session.initialTownSkillInfoPrepared, session.initialTownSkillInfoSent, len(session.initialTownSkillInfo.body))
	}
}

func TestPreparedInitialTownSkillInfoRestoresDarkKnightComboAfterTransition(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{CharacterID: "29", AccountID: "account-1", Job: "9", Level: 1}
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(context.Background(), currentDarkKnightComboNativeTestRecord("29")); err != nil {
		t.Fatal(err)
	}
	pages := []darkcombo.Page{{Index: 0, Roots: []darkcombo.Root{{SkillID: 118, Children: []uint16{46}}}}}
	if err := darkcombo.SavePages(context.Background(), repositories.Settings, "29", pages, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                         connection,
		accountID:                    "account-1",
		selectedCharacterID:          29,
		initialTownSkillInfoPrepared: true,
		initialTownSkillInfo: currentSceneSkillInfoProjection{
			body:        []byte{1, 0, 0, 0, 0x08, 0x21},
			characterID: "29",
		},
	}
	service := &Service{
		options:            options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		skillCatalog:       &dnfskill.Table{},
	}
	if err := service.commitPreparedInitialTownSkillInfoAfterTransition(session, "test_dark_knight_post_op24"); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || packet.Header.Classification != 0 || packet.Header.MsgID != currentDarkKnightComboInitializeMsgID {
		t.Fatalf("header=%+v trailing=%x", packet.Header, trailing)
	}
	want, err := darkcombo.EncodeNotificationBody(pages)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("body=%x want=%x", packet.Body, want)
	}
	if session.initialTownSkillInfoPrepared || !session.initialTownSkillInfoSent {
		t.Fatalf("prepared=%t sent=%t", session.initialTownSkillInfoPrepared, session.initialTownSkillInfoSent)
	}
}

func TestSyncCurrentSkillPointStateRepairsHistoricalLedgerAndPreservesSpentPoints(t *testing.T) {
	tests := []struct {
		name    string
		current dnfrepo.SkillPointState
		target  dnfrepo.SkillPointState
		want    dnfrepo.SkillPointState
	}{
		{
			name:    "forced level90 historical zero ledger",
			current: dnfrepo.SkillPointState{SyncedLevel: 1},
			target:  dnfrepo.SkillPointState{TotalSP: 11970, RemainingSP: 11970, TotalTP: 41, RemainingTP: 41, SyncedLevel: 90},
			want:    dnfrepo.SkillPointState{TotalSP: 11970, RemainingSP: 11970, TotalTP: 41, RemainingTP: 41, SyncedLevel: 90},
		},
		{
			name:    "spent points remain spent",
			current: dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 70, TotalTP: 10, RemainingTP: 8, SyncedLevel: 50},
			target:  dnfrepo.SkillPointState{TotalSP: 200, RemainingSP: 200, TotalTP: 20, RemainingTP: 20, SyncedLevel: 60},
			want:    dnfrepo.SkillPointState{TotalSP: 200, RemainingSP: 170, TotalTP: 20, RemainingTP: 18, SyncedLevel: 60},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, changed, err := syncCurrentSkillPointState(test.current, test.target)
			if err != nil {
				t.Fatal(err)
			}
			if !changed || got != test.want {
				t.Fatalf("sync got=%+v changed=%t, want=%+v", got, changed, test.want)
			}
		})
	}
}

func consumeCurrentSkillInfoFields(t *testing.T, raw []byte) (map[protowire.Number][]uint64, map[protowire.Number][][]byte) {
	t.Helper()
	varints := make(map[protowire.Number][]uint64)
	messages := make(map[protowire.Number][][]byte)
	for len(raw) > 0 {
		number, wireType, n := protowire.ConsumeTag(raw)
		if n < 0 {
			t.Fatalf("consume tag: %v", protowire.ParseError(n))
		}
		raw = raw[n:]
		switch wireType {
		case protowire.VarintType:
			value, consumed := protowire.ConsumeVarint(raw)
			if consumed < 0 {
				t.Fatalf("consume field %d varint: %v", number, protowire.ParseError(consumed))
			}
			varints[number] = append(varints[number], value)
			raw = raw[consumed:]
		case protowire.BytesType:
			value, consumed := protowire.ConsumeBytes(raw)
			if consumed < 0 {
				t.Fatalf("consume field %d bytes: %v", number, protowire.ParseError(consumed))
			}
			messages[number] = append(messages[number], append([]byte(nil), value...))
			raw = raw[consumed:]
		default:
			t.Fatalf("unexpected field %d wire type %d", number, wireType)
		}
	}
	return varints, messages
}

func currentSkillInfoFirstTreeSlots(t *testing.T, body []byte) map[int]uint16 {
	t.Helper()
	if len(body) < 4 || int(binary.LittleEndian.Uint32(body[:4])) != len(body)-4 {
		t.Fatalf("invalid skill-info body=%x", body)
	}
	_, outerMessages := consumeCurrentSkillInfoFields(t, body[4:])
	if len(outerMessages[2]) != currentSkillInfoTreeCount {
		t.Fatalf("skill tree count=%d", len(outerMessages[2]))
	}
	_, treeMessages := consumeCurrentSkillInfoFields(t, outerMessages[2][0])
	out := make(map[int]uint16, len(treeMessages[3]))
	for _, raw := range treeMessages[3] {
		fields, _ := consumeCurrentSkillInfoFields(t, raw)
		if len(fields[1]) != 1 || len(fields[2]) != 1 {
			t.Fatalf("invalid skill row fields=%v", fields)
		}
		out[int(fields[1][0])] = uint16(fields[2][0])
	}
	return out
}
