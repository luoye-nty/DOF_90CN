package dnfbridge

import (
	"path/filepath"
	"sort"
	"testing"

	dnfchannelinfo "longheng.io/server/internal/modules/dnf/channelinfo"
)

// currentGuildMemberChannelByteExemptChannels are the advertised channels whose
// id deliberately stays above the guild member row's one-byte field. It is
// empty: every group was renumbered into 1..254 once the same limit turned out
// to break town co-presence as well, not just this row -- `[deathtower]`
// 508 -> 171, `[pvp]` 501..507 -> 191..197, `[fiendwar]` 261..263 -> 184..186.
// Adding an entry here again means accepting that members in that channel show
// "online, channel unknown" AND that they cannot own a town actor, so
// advertisableChannels would refuse to publish it in the first place.
var currentGuildMemberChannelByteExemptChannels = map[int]string{}

// The guild member-list row carries the channel in one byte and 0xff is the
// client's offline sentinel (sub_1D21FD0 stores member+0x34 == -1 for it and
// the window then renders no row at all). This pins two things about the
// shipped catalog: no advertised channel may ever encode as that sentinel, and
// the set of channels that do not fit 1..254 stays exactly the documented
// exemptions — a new out-of-range channel has to be a conscious decision,
// because its members lose their channel number in the guild window.
func TestCurrentChannelCatalogFitsGuildMemberChannelByte(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "channel_info.etc")
	index, err := dnfchannelinfo.LoadFile(path)
	if err != nil {
		t.Fatalf("load shipped channel table %s: %v", path, err)
	}
	channels := index.Channels()
	if len(channels) == 0 {
		t.Fatalf("shipped channel table %s advertises no channel", path)
	}
	seen := make(map[int]struct{}, len(channels))
	unexpected := make([]int, 0)
	for _, channel := range channels {
		seen[channel.ID] = struct{}{}
		encoded := currentGuildMemberChannelByte(channel.ID)
		// An advertised channel must never read as offline: that removes the
		// member's whole row from the window.
		if encoded == currentGuildMemberOfflineChannel {
			t.Errorf("channel %d (server %d, area %q) encodes as the offline sentinel",
				channel.ID, channel.ServerID, channel.AreaKey)
		}
		if channel.ID > 0 && channel.ID <= 254 {
			if encoded != byte(channel.ID) {
				t.Errorf("channel %d encodes as %#x instead of itself", channel.ID, encoded)
			}
			continue
		}
		if _, exempt := currentGuildMemberChannelByteExemptChannels[channel.ID]; !exempt {
			unexpected = append(unexpected, channel.ID)
		}
	}
	sort.Ints(unexpected)
	if len(unexpected) != 0 {
		t.Errorf("channels %v are outside the member-row byte range 1..254 and are not documented exemptions: "+
			"either renumber them into 1..254 or add them to currentGuildMemberChannelByteExemptChannels", unexpected)
	}
	// A renumbering has to land in free slots; a collision would make two
	// different channels share a game port (Port = base + ID).
	if len(seen) != len(channels) {
		t.Errorf("shipped channel table has duplicate ids: %d rows, %d distinct", len(channels), len(seen))
	}
	// Each renumbering below fixed a reported bug; pin the old id out and the
	// new id in so a table refresh cannot quietly reintroduce either.
	for _, renumbered := range []struct {
		group    string
		oldIDs   []int
		newIDs   []int
		symptoms string
	}{
		{"[deathtower]", []int{508}, []int{171},
			"member row vanished from the guild window"},
		{"[pvp]", []int{501, 502, 503, 504, 505, 506, 507},
			[]int{191, 192, 193, 194, 195, 196, 197},
			"town actor owner uncommitted: peers invisible in the arena"},
		{"[fiendwar]", []int{261, 262, 263}, []int{184, 185, 186},
			"town actor owner uncommitted: peers invisible in the arena"},
	} {
		for _, id := range renumbered.oldIDs {
			if _, present := seen[id]; present {
				t.Errorf("channel %d is back: %s must stay renumbered (%s)",
					id, renumbered.group, renumbered.symptoms)
			}
		}
		for _, id := range renumbered.newIDs {
			if _, present := seen[id]; !present {
				t.Errorf("channel %d is missing: %s should be advertised there",
					id, renumbered.group)
			}
		}
	}
}
