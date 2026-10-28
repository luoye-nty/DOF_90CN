package channelinfo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// OneClickProfile describes the deliberately small channel directory shipped
// with the binary-only one-click release. Non-ordinary and non-arena channel
// groups are retained so special and raid entrances remain available.
type OneClickProfile struct {
	ServerID          int
	OrdinaryChannelID int
	OrdinaryAreaKey   string
	ArenaChannelID    int
	ArenaAreaKey      string
}

// BuildOneClickChannelInfo keeps one ordinary channel and one arena channel
// while preserving every special and team channel from the authoritative ETC.
func BuildOneClickChannelInfo(data []byte, profile OneClickProfile) ([]byte, error) {
	if profile.ServerID <= 0 || profile.OrdinaryChannelID <= 0 || profile.ArenaChannelID <= 0 {
		return nil, errors.New("one-click channel profile IDs must be positive")
	}
	ordinaryArea := areaKey(profile.OrdinaryAreaKey)
	arenaArea := areaKey(profile.ArenaAreaKey)
	if ordinaryArea == "" || arenaArea == "" || ordinaryArea == arenaArea {
		return nil, errors.New("one-click ordinary and arena area keys must be distinct")
	}

	index, err := Parse(data)
	if err != nil {
		return nil, err
	}
	channels := index.ChannelsForServer(profile.ServerID)
	if len(channels) == 0 {
		return nil, fmt.Errorf("one-click source server %d has no channels", profile.ServerID)
	}
	ordinary, ok := index.ServerChannel(profile.ServerID, profile.OrdinaryChannelID)
	if !ok || ordinary.Type != 11 || ordinary.AreaKey != ordinaryArea {
		return nil, fmt.Errorf(
			"one-click ordinary channel %d must be type 11 in [%s]",
			profile.OrdinaryChannelID,
			ordinaryArea,
		)
	}
	arena, ok := index.ServerChannel(profile.ServerID, profile.ArenaChannelID)
	if !ok || arena.AreaKey != arenaArea {
		return nil, fmt.Errorf(
			"one-click arena channel %d must be in [%s]",
			profile.ArenaChannelID,
			arenaArea,
		)
	}

	selected := make([]Channel, 0, len(channels))
	for _, channel := range channels {
		switch {
		case channel.Type == 11:
			if channel.ID == profile.OrdinaryChannelID && channel.AreaKey == ordinaryArea {
				selected = append(selected, channel)
			}
		case channel.AreaKey == arenaArea:
			if channel.ID == profile.ArenaChannelID {
				selected = append(selected, channel)
			}
		default:
			selected = append(selected, channel)
		}
	}

	prefix, err := channelInfoPrefix(data)
	if err != nil {
		return nil, err
	}
	var output strings.Builder
	output.WriteString(prefix)
	output.WriteString("[server] ")
	output.WriteString(strconv.Itoa(profile.ServerID))
	output.WriteByte('\n')
	for _, channel := range selected {
		fields := []string{
			strconv.Itoa(channel.ID),
			"`<" + channel.NameKey + ">`",
			strconv.Itoa(channel.Type),
			"`[" + channel.AreaKey + "]`",
		}
		fields = append(fields, normalizedRawFields(channel.RawFields)...)
		output.WriteString("   ")
		output.WriteString(strings.Join(fields, "   "))
		output.WriteByte('\n')
	}
	output.WriteString("[/server]\n")

	result := []byte(output.String())
	compacted, err := Parse(result)
	if err != nil {
		return nil, fmt.Errorf("validate one-click channel info: %w", err)
	}
	ordinaryCount := 0
	arenaCount := 0
	for _, channel := range compacted.ChannelsForServer(profile.ServerID) {
		if channel.Type == 11 {
			ordinaryCount++
		}
		if channel.AreaKey == arenaArea {
			arenaCount++
		}
	}
	if ordinaryCount != 1 || arenaCount != 1 {
		return nil, fmt.Errorf(
			"one-click channel validation failed: ordinary=%d arena=%d",
			ordinaryCount,
			arenaCount,
		)
	}
	return result, nil
}

func channelInfoPrefix(data []byte) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	var prefix strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if _, ok, err := parseServerSectionHeader(strings.TrimSpace(line)); ok {
			if err != nil {
				return "", err
			}
			return prefix.String(), nil
		}
		prefix.WriteString(line)
		prefix.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("one-click channel source has no server section")
}

func normalizedRawFields(fields []string) []string {
	out := append([]string(nil), fields...)
	for len(out) > 0 && out[len(out)-1] == "``" {
		out = out[:len(out)-1]
	}
	return append(out, "``")
}
