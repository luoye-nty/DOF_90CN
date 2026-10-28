package town

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const defaultNPCList = "npc/npc.lst"

func Load(ctx context.Context, source Source, options Options) (*Table, error) {
	if source == nil {
		return nil, ErrSourceRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	listPath := strings.TrimSpace(options.ListPath)
	if listPath == "" {
		listPath = DefaultList
	}
	listText, err := source.ReadText(listPath)
	if err != nil {
		return nil, err
	}
	listDoc, err := dnfpvf.Parse(listPath, listText)
	if err != nil {
		return nil, err
	}
	entries := dnfpvf.ParseList(listDoc)
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrListEmpty, listPath)
	}

	table := &Table{towns: make(map[int64]Town, len(entries))}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		documentPath := townDocumentPath(listPath, entry.Path)
		text, err := source.ReadText(documentPath)
		if err != nil {
			return nil, fmt.Errorf("read town %d %s: %w", entry.ID, documentPath, err)
		}
		doc, err := dnfpvf.Parse(documentPath, text)
		if err != nil {
			return nil, fmt.Errorf("parse town %d %s: %w", entry.ID, documentPath, err)
		}
		value, err := parseTown(entry.ID, documentPath, doc)
		if err != nil {
			return nil, err
		}
		if _, exists := table.towns[value.ID]; !exists {
			table.towns[value.ID] = value
		}
	}
	loadTownMapNPCs(ctx, source, table)
	return table, nil
}

func loadTownMapNPCs(ctx context.Context, source Source, table *Table) {
	if source == nil || table == nil || len(table.towns) == 0 {
		return
	}
	enterTargets := make(map[int64]struct{})
	exitTargets := make(map[int64]struct{})
	if listText, err := source.ReadText(defaultNPCList); err == nil {
		if listDoc, parseErr := dnfpvf.Parse(defaultNPCList, listText); parseErr == nil {
			for _, entry := range dnfpvf.ParseList(listDoc) {
				if ctx.Err() != nil {
					return
				}
				documentPath := townDocumentPath(defaultNPCList, entry.Path)
				text, readErr := source.ReadText(documentPath)
				if readErr != nil {
					continue
				}
				doc, documentErr := dnfpvf.Parse(documentPath, text)
				if documentErr != nil {
					continue
				}
				enterTarget, hasEnterTarget, hasExitRole := parseTownRoles(doc)
				if hasEnterTarget {
					enterTargets[enterTarget] = struct{}{}
				}
				if hasExitRole {
					exitTargets[entry.ID] = struct{}{}
				}
			}
		}
	}
	for townID, value := range table.towns {
		for areaIndex := range value.Areas {
			area := &value.Areas[areaIndex]
			mapPath, text, readErr := readTownMapText(source, area.MapPath)
			if readErr != nil {
				continue
			}
			doc, parseErr := dnfpvf.Parse(mapPath, text)
			if parseErr != nil {
				continue
			}
			area.QuestConditions = repeatedSectionInts(doc, "quest condition")
			mapValue := worldmap.ParseMap(0, mapPath, doc)
			if trigger, ok := seriaRoomPortalTrigger(doc); ok {
				if point, found := townPortalReturnPoint(trigger, mapValue.VirtualMovableArea); found {
					area.SeriaRoomReturn = &point
				}
			}
			// 婚房门外的安全落点（见 wedding_room_return.go）。与 SeriaRoomReturn
			// 同样只在装载期推导，找不到 101 传送门时保持 nil，对既有城镇零影响。
			if trigger, ok := weddingRoomPortalTrigger(doc); ok {
				if point, found := weddingRoomReturnPoint(trigger, mapValue.VirtualMovableArea); found {
					area.WeddingRoomReturn = &point
				}
			}
			area.NPCs = make([]NPCSpawn, 0, len(mapValue.NPCs))
			for _, spawn := range mapValue.NPCs {
				area.NPCs = append(area.NPCs, NPCSpawn{
					NPCID: spawn.NPCID, Direction: spawn.Direction,
					X: spawn.Position.X, Y: spawn.Position.Y, Z: spawn.Position.Z,
				})
			}
			area.MovableAreas = make([]MovableArea, 0, len(mapValue.TownMovableAreas))
			for _, movable := range mapValue.TownMovableAreas {
				area.MovableAreas = append(area.MovableAreas, MovableArea{
					X: movable.X, Y: movable.Y, Width: movable.Width, Height: movable.Height,
				})
			}
			seen := make(map[int64]struct{})
			for _, spawn := range mapValue.NPCs {
				if _, ok := enterTargets[spawn.NPCID]; !ok {
					continue
				}
				if _, ok := exitTargets[spawn.NPCID]; !ok {
					continue
				}
				if _, ok := seen[spawn.NPCID]; ok {
					continue
				}
				seen[spawn.NPCID] = struct{}{}
				area.TownExitTargetNPCIDs = append(area.TownExitTargetNPCIDs, spawn.NPCID)
			}
			sort.Slice(area.TownExitTargetNPCIDs, func(i, j int) bool {
				return area.TownExitTargetNPCIDs[i] < area.TownExitTargetNPCIDs[j]
			})
		}
		table.towns[townID] = value
	}
}

func readTownMapText(source Source, referenced string) (string, string, error) {
	referenced = strings.TrimSpace(strings.ReplaceAll(referenced, "\\", "/"))
	candidates := []string{referenced}
	if !strings.HasPrefix(strings.ToLower(referenced), "map/") {
		candidates = append(candidates, path.Join("map", referenced))
	}
	// 城镇列表里仍然写着**原始**地图名，而实际只有修订版 `(r)` 存在 ——
	// 赛丽亚房间（城镇 38 区域 1）就是这种情况：`new_seria_room.map` 不存在，
	// 只有 `(r)new_seria_room.map`，而通往婚房(101/0)的那扇门只写在修订版里。
	//
	// 不加这条回落，该区域的 NPC / 移动区 / 门全为空，装载期就推导不出
	// WeddingRoomReturn，于是从婚房走左门出来会被判成"找不到门"而卡住出不去。
	//
	// 顺序是安全的：原图候选排在前面，只有原图缺失时才会用到 `(r)` 版。
	for _, candidate := range append([]string(nil), candidates...) {
		dir, base := path.Split(candidate)
		if base != "" && !strings.HasPrefix(strings.ToLower(base), "(r)") {
			candidates = append(candidates, dir+"(r)"+base)
		}
	}
	var lastErr error
	for _, candidate := range candidates {
		text, err := source.ReadText(candidate)
		if err == nil {
			return candidate, text, nil
		}
		lastErr = err
	}
	return "", "", lastErr
}

func parseTownRoles(doc *dnfpvf.Document) (enterTarget int64, hasEnterTarget, hasExitRole bool) {
	tokens, ok := doc.Section("role")
	if !ok {
		return 0, false, false
	}
	for index, token := range tokens {
		if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(token.Value)) {
		case "[enter town]":
			if index+1 < len(tokens) && tokens[index+1].Kind == dnfpvf.TokenInt && tokens[index+1].Int > 0 {
				enterTarget = tokens[index+1].Int
				hasEnterTarget = true
			}
		case "[exit town]":
			hasExitRole = true
		}
	}
	return enterTarget, hasEnterTarget, hasExitRole
}

// repeatedSectionInts collects the positive integers of every section named
// name, in document order and de-duplicated. Town maps declare one
// `[quest condition]` block per gating quest rather than one block with a
// list, so Document.Ints (first section only) would drop all but the first.
func repeatedSectionInts(doc *dnfpvf.Document, name string) []int64 {
	if doc == nil {
		return nil
	}
	key := strings.ToLower(strings.TrimSpace(name))
	values := make([]int64, 0)
	seen := make(map[int64]struct{})
	for _, section := range doc.Sections {
		if strings.ToLower(strings.TrimSpace(section.Name)) != key ||
			section.Start < 0 || section.End > len(doc.Tokens) || section.Start > section.End {
			continue
		}
		for _, token := range doc.Tokens[section.Start:section.End] {
			if token.Kind != dnfpvf.TokenInt || token.Int <= 0 {
				continue
			}
			if _, exists := seen[token.Int]; exists {
				continue
			}
			seen[token.Int] = struct{}{}
			values = append(values, token.Int)
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func townDocumentPath(listPath, referenced string) string {
	referenced = strings.TrimSpace(strings.ReplaceAll(referenced, "\\", "/"))
	if strings.Contains(referenced, "/") {
		return path.Clean(referenced)
	}
	return path.Join(path.Dir(listPath), referenced)
}
