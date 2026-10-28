// Command channelinfodump prints the server/channel pairs the shipped
// channel_info.etc actually declares, so a runtime overlay (Options.ServerID)
// can be compared against what the client is told.
package main

import (
	"fmt"
	"os"
	"sort"

	"longheng.io/server/internal/modules/dnf/channelinfo"
)

func main() {
	path := "runtime/data/dnf/channel_info.etc"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	index, err := channelinfo.LoadFile(path)
	if err != nil {
		fmt.Println("load:", err)
		os.Exit(1)
	}
	channels := index.Channels()
	byServer := map[int]int{}
	ids := map[int][]int{}
	for _, channel := range channels {
		byServer[channel.ServerID]++
		ids[channel.ServerID] = append(ids[channel.ServerID], channel.ID)
	}
	servers := make([]int, 0, len(byServer))
	for server := range byServer {
		servers = append(servers, server)
	}
	sort.Ints(servers)
	fmt.Printf("total channels=%d, distinct server ids=%v\n", len(channels), servers)
	for _, server := range servers {
		list := ids[server]
		sort.Ints(list)
		head := list
		if len(head) > 12 {
			head = head[:12]
		}
		fmt.Printf("  server %-4d channels=%-4d head=%v\n", server, byServer[server], head)
	}
	fmt.Println("---- every entry whose channel id is 41 or 183 ----")
	for _, channel := range channels {
		if channel.ID == 41 || channel.ID == 183 {
			fmt.Printf("  server=%-4d channel=%-4d type=%-3d areakey=%-12s namekey=%-16s raw=%v\n",
				channel.ServerID, channel.ID, channel.Type, channel.AreaKey, channel.NameKey, channel.RawFields)
		}
	}
	fmt.Println("---- Index.Channel(41) / ServerChannel(1,41) / ServerChannel(11,41) ----")
	if channel, ok := index.Channel(41); ok {
		fmt.Printf("  Channel(41)        -> server=%d type=%d areakey=%s\n", channel.ServerID, channel.Type, channel.AreaKey)
	} else {
		fmt.Println("  Channel(41)        -> MISSING")
	}
	for _, server := range []int{1, 11} {
		if channel, ok := index.ServerChannel(server, 41); ok {
			fmt.Printf("  ServerChannel(%d,41) -> type=%d areakey=%s\n", server, channel.Type, channel.AreaKey)
		} else {
			fmt.Printf("  ServerChannel(%d,41) -> MISSING\n", server)
		}
	}
}
