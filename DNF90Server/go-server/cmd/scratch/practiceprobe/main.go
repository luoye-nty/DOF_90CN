// Command practiceprobe answers one question for the 练习模式 fatigue fix:
// which dungeons this data set actually admits into the isolated practice
// policy, i.e. what validatePracticeDungeonRoute and selectPracticeDungeonMaze
// would decide. Temporary scratch tool - delete after use.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path")
	dungeonIDs := flag.String("dungeons", "", "comma-separated dungeon ids")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		log.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		log.Fatal(err)
	}
	for _, part := range strings.Split(*dungeonIDs, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			log.Fatal(err)
		}
		dungeon, found := table.FindDungeon(id)
		if !found {
			fmt.Printf("dungeon=%d MISSING\n", id)
			continue
		}
		metadata := dungeon.Metadata
		nonQuest := 0
		questBound := 0
		for index := range dungeon.Mazes {
			if len(dungeon.Mazes[index].QuestConnection) == 0 {
				nonQuest++
			} else {
				questBound++
			}
		}
		fmt.Printf("dungeon=%d name=%q path=%s\n", dungeon.ID, metadata.Name, dungeon.Path)
		fmt.Printf("  mazes=%d non_quest_mazes=%d quest_bound_mazes=%d\n", len(dungeon.Mazes), nonQuest, questBound)
		fmt.Printf("  prohibit_practice=%v no_fatigue=%v tutorial=%v tournament=%v\n",
			metadata.IntegerValues["prohibit practice"],
			metadata.NoFatigue,
			metadata.TutorialDungeon,
			metadata.Flags["tournament dungeon"])
		fmt.Printf("  enter_without_fatigue=%v minimum_level=%+v party_limit=%+v fatigue_cost=%v\n",
			metadata.Flags["enter without fatigue"],
			metadata.MinimumRequiredLevel,
			metadata.LimitPartyCount,
			metadata.IntegerValues["fatigue"])
		if nonQuest > 0 {
			for index := range dungeon.Mazes {
				if len(dungeon.Mazes[index].QuestConnection) == 0 {
					fmt.Printf("  practice_pickable_maze index=%d quest_connection=%v\n",
						dungeon.Mazes[index].Index, dungeon.Mazes[index].QuestConnection)
					break
				}
			}
		}
	}
}
