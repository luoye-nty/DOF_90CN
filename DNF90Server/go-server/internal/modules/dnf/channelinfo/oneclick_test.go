package channelinfo

import "testing"

func TestBuildOneClickChannelInfoKeepsOneOrdinaryAndArenaChannel(t *testing.T) {
	data := []byte("[dungeon]\n`[granfloris]` `forest` 1\n[/dungeon]\n" +
		"[dungeon]\n`[pvp]` `arena`\n[/dungeon]\n" +
		"[server] 1\n" +
		"10 `forest` 11 `[granfloris]` 5 0 0 ``\n" +
		"19 `forest` 11 `[granfloris]` 5 0 0 ``\n" +
		"21 `sky` 11 `[sky_catle]` 5 0 0 ``\n" +
		"23 `raid` 23 `[anton_raid]` 5 0 0 ``\n" +
		"501 `arena1` 24 `[pvp]` 0 0 0 ``\n" +
		"502 `arena2` 24 `[pvp]` 0 0 0 ``\n" +
		"[/server]\n")

	result, err := BuildOneClickChannelInfo(data, OneClickProfile{
		ServerID:          1,
		OrdinaryChannelID: 19,
		OrdinaryAreaKey:   "granfloris",
		ArenaChannelID:    501,
		ArenaAreaKey:      "pvp",
	})
	if err != nil {
		t.Fatal(err)
	}
	index, err := Parse(result)
	if err != nil {
		t.Fatal(err)
	}
	channels := index.ChannelsForServer(1)
	if len(channels) != 3 {
		t.Fatalf("one-click channels = %+v, want ordinary, raid, and arena", channels)
	}
	for idx, want := range []int{19, 23, 501} {
		if channels[idx].ID != want {
			t.Fatalf("one-click channel[%d] = %d, want %d", idx, channels[idx].ID, want)
		}
	}
}

func TestBuildOneClickChannelInfoRejectsMismatchedRequiredChannel(t *testing.T) {
	data := []byte("[server] 1\n19 `forest` 11 `[granfloris]` 0 ``\n501 `arena` 24 `[pvp]` 0 ``\n[/server]\n")
	_, err := BuildOneClickChannelInfo(data, OneClickProfile{
		ServerID:          1,
		OrdinaryChannelID: 10,
		OrdinaryAreaKey:   "granfloris",
		ArenaChannelID:    501,
		ArenaAreaKey:      "pvp",
	})
	if err == nil {
		t.Fatal("missing required ordinary channel was accepted")
	}
}
