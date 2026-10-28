package quest

import (
	"context"
	"reflect"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

func TestLoadStoryEpisodesUsesEpisodeLocalSequence(t *testing.T) {
	index, err := dnfpvf.Build(context.Background(), catalogTestSource{
		ConnectQuestListPath: "[connect quest unit]\n" +
			"[type]\n`episode quest`\n[parent key]\n7\n[worldmap index]\n7\n" +
			"[level section]\n46 0\n[name]\n`first chapter`\n" +
			"[sequence]\n3367 7123 3368 0\n[/sequence]\n[/connect quest unit]\n" +
			"[connect quest unit]\n[type]\n`episode quest`\n[parent key]\n8\n" +
			"[worldmap index]\n21\n[name]\n`second chapter`\n" +
			"[sequence]\n3427 0 3431 144\n[/sequence]\n[/connect quest unit]",
	}, dnfpvf.BuildOptions{Paths: []string{ConnectQuestListPath}})
	if err != nil {
		t.Fatal(err)
	}
	episodes, err := LoadStoryEpisodes(index)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 || episodes[1].Name != "second chapter" || episodes[1].ParentKey != 8 {
		t.Fatalf("episodes=%+v", episodes)
	}
	if got, ok := StoryEntriesFrom(episodes, 3427); !ok || !reflect.DeepEqual(got, []StoryEntry{{QuestID: 3427}, {QuestID: 3431, DungeonID: 144}}) {
		t.Fatalf("second episode entries=%+v found=%t", got, ok)
	}
	if got, ok := StoryEntriesFrom(episodes, 3368); !ok || !reflect.DeepEqual(got, []StoryEntry{{QuestID: 3368}}) {
		t.Fatalf("first episode tail=%+v found=%t", got, ok)
	}
	if name := StoryEpisodeName(episodes, 3431); name != "second chapter" {
		t.Fatalf("episode name=%q", name)
	}
}

func TestLoadStoryEpisodesRejectsMalformedEpisodeUnits(t *testing.T) {
	index, err := dnfpvf.Build(context.Background(), catalogTestSource{
		ConnectQuestListPath: "[connect quest unit]\n[type]\n`episode quest`\n[parent key]\n7\n" +
			"[worldmap index]\n7\n[sequence]\n3367\n[/sequence]\n[/connect quest unit]",
	}, dnfpvf.BuildOptions{Paths: []string{ConnectQuestListPath}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStoryEpisodes(index); err == nil {
		t.Fatal("malformed episode sequence was accepted")
	}
}
