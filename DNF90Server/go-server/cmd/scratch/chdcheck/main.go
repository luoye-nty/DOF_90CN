package main

// 诊断工具：用真实 channel_info.etc 复现频道相关的判定。
// 用法: go run ./cmd/scratch/chdcheck <channel_info.etc 路径>
import (
	"fmt"
	"os"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/channelinfo"
)

func main() {
	path := `runtime\data\dnf\channel_info.etc`
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	index, err := channelinfo.LoadFile(path)
	if err != nil {
		fmt.Println("LoadFile error:", err)
		return
	}
	for _, serverID := range []int{0, 1} {
		catalog, err := channelcatalog.New(index, channelcatalog.Options{ServerID: serverID})
		if err != nil {
			fmt.Printf("serverID=%d catalog error: %v\n", serverID, err)
			continue
		}
		channels := catalog.Channels()
		fmt.Printf("serverID=%d channels=%d first=%d last=%d\n", serverID, len(channels), channels[0].ID, channels[len(channels)-1].ID)
		byType := map[uint8][]string{}
		for _, channel := range channels {
			if len(byType[channel.Type]) < 4 {
				byType[channel.Type] = append(byType[channel.Type], fmt.Sprintf("%d/%s", channel.ID, channel.Group))
			}
		}
		fmt.Printf("   types:")
		for t, sample := range byType {
			fmt.Printf(" %d(%v)", t, sample)
		}
		fmt.Println()
		for _, id := range []int{19, 121, 181, 182, 253} {
			channel, found := catalog.Channel(id)
			if !found {
				fmt.Printf("   id=%d: NOT FOUND\n", id)
				continue
			}
			resident, eligible := catalog.ResidentFor(channel)
			ok := found && eligible && resident.ID == channel.ID && channel.ID <= 255
			fmt.Printf("   id=%d group=%q type=%d serverID=%d port=%d name=%q -> resident=%d eligible=%t handler_ok=%t\n",
				id, channel.Group, channel.Type, channel.ServerID, channel.Port, channel.Name, resident.ID, eligible, ok)
		}
	}
}
