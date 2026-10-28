package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestDecodeCurrentDungeonAltarDeathSequencesStrictCurrentEXELayout(t *testing.T) {
	body := []byte{3, 1, 0, 0x34, 0x12, 0xff, 0xff}
	sequences, err := decodeCurrentDungeonAltarDeathSequences(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint16{1, 0x1234, 0xffff}
	if len(sequences) != len(want) {
		t.Fatalf("sequences=%v want=%v", sequences, want)
	}
	for index := range want {
		if sequences[index] != want[index] {
			t.Fatalf("sequences=%v want=%v", sequences, want)
		}
	}

	malformed := [][]byte{
		nil,
		{0},
		{1, 1},
		{1, 0, 0},
		{2, 1, 0, 1, 0},
		append([]byte{101}, make([]byte, 202)...),
	}
	for _, candidate := range malformed {
		if _, err := decodeCurrentDungeonAltarDeathSequences(candidate); !errors.Is(err, errCurrentDungeonAltarRequestMalformed) {
			t.Fatalf("malformed body=%x error=%v", candidate, err)
		}
	}
}

func TestBuildCurrentDungeonAltarStartMapCurrentEXEGolden(t *testing.T) {
	scene := worldmap.DungeonRoomScene{
		Coordinate: worldmap.RoomCoordinate{X: 2, Y: 3},
		Map:        worldmap.ResolvedMap{Map: worldmap.Map{ID: 17030}},
	}
	first, err := buildCurrentDungeonAltarStartMapBody(scene, 0x12345678, false)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := []byte{2, 3, 0x78, 0x56, 0x34, 0x12, 0, 1, 0x86, 0x42, 0, 0, 0, 0, 0}
	if !bytes.Equal(first, wantFirst) {
		t.Fatalf("first op211=%x want=%x", first, wantFirst)
	}
	revisit, err := buildCurrentDungeonAltarStartMapBody(scene, 0x12345678, true)
	if err != nil {
		t.Fatal(err)
	}
	wantRevisit := []byte{2, 3, 0x78, 0x56, 0x34, 0x12, 0, 0}
	if !bytes.Equal(revisit, wantRevisit) {
		t.Fatalf("revisit op211=%x want=%x", revisit, wantRevisit)
	}
}

func TestBuildCurrentDungeonAltarKnownClass0Bodies(t *testing.T) {
	crazy, err := buildCurrentDungeonAltarCrazyMonsterBody(currentDungeonAltarCrazyMonster{
		MonsterCode: 61010,
		Variant:     18,
	})
	if err != nil || !bytes.Equal(crazy, []byte{0x52, 0xee, 0, 0, 18}) {
		t.Fatalf("op213=%x err=%v", crazy, err)
	}
	interval := buildCurrentDungeonAltarRoundIntervalBody(4, 6000)
	if !bytes.Equal(interval, []byte{5, 0x70, 0x17, 0, 0}) {
		t.Fatalf("op215=%x", interval)
	}
	prompt := buildCurrentDungeonAltarDifficultyPromptBody(3, 10)
	if !bytes.Equal(prompt, []byte{0, 3, 10, 0}) {
		t.Fatalf("op297 prompt=%x", prompt)
	}
	resolved, err := buildCurrentDungeonAltarDifficultyResolvedBody(worldmap.BloodAltarDifficultyB)
	if err != nil || !bytes.Equal(resolved, []byte{2}) {
		t.Fatalf("op297 resolved=%x err=%v", resolved, err)
	}

	ranking := buildCurrentDungeonAltarRankingBody(2, 3000, 4, 5000, 7, 6000)
	wantRanking := []uint32{3000, 2, 5000, 4, 7, 6000}
	if len(ranking) != 24 {
		t.Fatalf("op208 len=%d body=%x", len(ranking), ranking)
	}
	for index, want := range wantRanking {
		if got := binary.LittleEndian.Uint32(ranking[index*4 : index*4+4]); got != want {
			t.Fatalf("op208 field[%d]=%d want=%d body=%x", index, got, want, ranking)
		}
	}

	reward, err := buildCurrentDungeonAltarRewardBody(3, 7, [4][]currentDungeonAltarRewardEntry{
		{{ItemID: 11, Amount: 2}}, nil, {{ItemID: 22, Amount: 3}}, nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantReward := []byte{
		3, 7,
		1, 11, 0, 0, 0, 2, 0, 0, 0,
		0,
		1, 22, 0, 0, 0, 3, 0, 0, 0,
		0,
	}
	if !bytes.Equal(reward, wantReward) {
		t.Fatalf("op209=%x want=%x", reward, wantReward)
	}
}

func TestBuildCurrentDungeonAltarMonsterSpawnBodyWidth(t *testing.T) {
	wave := currentDungeonAltarWave{
		Spawns: []currentDungeonAltarSpawn{{
			Variant: 19, Sequence: 7, MonsterCode: 56017, MonsterType: 3,
			Level: 70, Scale: 1500, X: 440, Y: 222, Z: 0,
		}},
		Tail: 1,
	}
	body, err := buildCurrentDungeonAltarMonsterSpawnBody(wave)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 21 || binary.LittleEndian.Uint16(body[:2]) != 1 || body[2] != 19 ||
		binary.LittleEndian.Uint16(body[3:5]) != 7 || binary.LittleEndian.Uint32(body[5:9]) != 56017 ||
		body[9] != 3 || body[10] != 70 || binary.LittleEndian.Uint16(body[11:13]) != 1500 ||
		binary.LittleEndian.Uint16(body[13:15]) != 440 || binary.LittleEndian.Uint16(body[15:17]) != 222 ||
		binary.LittleEndian.Uint16(body[19:21]) != 1 {
		t.Fatalf("op210 body=%x", body)
	}
}
