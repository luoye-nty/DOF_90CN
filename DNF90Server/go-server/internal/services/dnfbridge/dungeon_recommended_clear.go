package dnfbridge

func currentDungeonMatchesRecommendedLevel(runtime *runtimeDungeonState) bool {
	if runtime == nil || runtime.Dungeon.ID <= 0 || runtime.Character.Level <= 0 {
		return false
	}
	levels := runtime.Dungeon.Metadata.RecommendedLevels
	minimum, maximum := int64(0), int64(0)
	switch {
	case len(levels) != 0:
		if len(levels) < 2 || levels[0] <= 0 || levels[1] < levels[0] {
			return false
		}
		minimum, maximum = levels[0], levels[1]
	case runtime.Dungeon.Metadata.MinimumRequiredLevel.Set &&
		runtime.Dungeon.Metadata.BasisLevel.Set &&
		runtime.Dungeon.Metadata.MinimumRequiredLevel.Value > 0 &&
		runtime.Dungeon.Metadata.BasisLevel.Value >= runtime.Dungeon.Metadata.MinimumRequiredLevel.Value:
		minimum = runtime.Dungeon.Metadata.MinimumRequiredLevel.Value
		maximum = runtime.Dungeon.Metadata.BasisLevel.Value
	default:
		return false
	}
	// 必须用**进图那一刻**的等级快照。本局小怪与翻牌经验会把等级推过推荐区间
	// 上限，用实时等级会让"推荐副本"在打完最后一张图时判成不推荐，于是月度签到 /
	// 周签到 / 签到弹珠 / 婚姻成长胶囊这些挂在"推荐副本通关"上的活动全部漏记。
	// 快照为 0 时（手工构造的 runtime）回落到实时等级。
	level := int64(runtime.entryCharacterLevel)
	if level == 0 {
		level = int64(runtime.Character.Level)
	}
	return level >= minimum && level <= maximum
}

func currentDungeonIsRecommendedClear(runtime *runtimeDungeonState) bool {
	return runtime != nil &&
		runtime.ordinaryFinalRoomClearAccepted &&
		runtime.ProtocolMode == currentDungeonProtocolModeNormal &&
		!runtime.Dungeon.Metadata.NoFatigue &&
		runtime.tournament == nil &&
		currentDungeonHasSelectedMaze(runtime) &&
		currentDungeonMatchesRecommendedLevel(runtime)
}

func currentDungeonHasSelectedMaze(runtime *runtimeDungeonState) bool {
	return runtime != nil && runtime.MazeIndex >= 0 && runtime.MazeIndex < len(runtime.Dungeon.Mazes)
}
