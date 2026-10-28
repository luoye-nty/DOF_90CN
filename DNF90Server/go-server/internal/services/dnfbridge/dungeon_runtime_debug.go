package dnfbridge

import (
	"net/http"
	"sort"

	appkit "longheng.io/server/internal/platform/app"
	"longheng.io/server/internal/platform/httpx"
)

const currentDungeonRuntimeDebugPath = "/debug/dungeon_runtime"

type dungeonRuntimeMemoryEntry struct {
	CharacterID uint16 `json:"character_id"`
	DungeonID   int64  `json:"dungeon_id"`
	MazeIndex   int    `json:"maze_index"`
	Rooms       int    `json:"rooms"`
	LayerChains int    `json:"layer_chains"`
	Payload     bool   `json:"payload_present"`
}

type dungeonRuntimeMemorySnapshot struct {
	OnlineSessions   int                         `json:"online_sessions"`
	ActiveRuntimes   int                         `json:"active_runtimes"`
	PayloadRuntimes  int                         `json:"payload_runtimes"`
	TotalRooms       int                         `json:"total_rooms"`
	TotalLayerChains int                         `json:"total_layer_chains"`
	MaxRooms         int                         `json:"max_rooms"`
	MaxLayerChains   int                         `json:"max_layer_chains"`
	PayloadReleases  uint64                      `json:"payload_releases"`
	Runtimes         []dungeonRuntimeMemoryEntry `json:"runtimes"`
}

// registerDungeonRuntimeDebugRoute exposes bounded, read-only runtime
// counters behind the platform admin authentication wrapper.
func (s *Service) registerDungeonRuntimeDebugRoute(env *appkit.Env) {
	if s == nil || env == nil || env.AdminMux == nil {
		return
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"ok":   true,
			"data": s.dungeonRuntimeMemorySnapshot(),
		})
	})
	if env.AdminAuth != nil {
		handler = env.AdminAuth("dnfbridge", handler.ServeHTTP)
	}
	env.AdminMux.HandleFunc(currentDungeonRuntimeDebugPath, handler.ServeHTTP)
}

func (s *Service) dungeonRuntimeMemorySnapshot() dungeonRuntimeMemorySnapshot {
	snapshot := dungeonRuntimeMemorySnapshot{}
	if s == nil {
		return snapshot
	}
	snapshot.PayloadReleases = dungeonRuntimePayloadReleaseCount.Load()
	s.mu.Lock()
	sessions := make([]*gameSession, 0, len(s.gameSessions))
	for _, session := range s.gameSessions {
		if session != nil {
			sessions = append(sessions, session)
		}
	}
	s.mu.Unlock()
	snapshot.OnlineSessions = len(sessions)
	seen := make(map[*runtimeDungeonState]struct{}, len(sessions))
	for _, session := range sessions {
		session.dungeon.mu.Lock()
		runtime := session.dungeon.runtime
		if runtime == nil {
			session.dungeon.mu.Unlock()
			continue
		}
		if _, duplicate := seen[runtime]; duplicate {
			session.dungeon.mu.Unlock()
			continue
		}
		seen[runtime] = struct{}{}
		entry := dungeonRuntimeMemoryEntry{
			CharacterID: session.selectedCharacterID,
			Rooms:       len(runtime.Rooms),
			LayerChains: len(runtime.LayerChains),
			Payload: runtime.Room != nil || len(runtime.Rooms) != 0 ||
				len(runtime.LayerChains) != 0 || runtime.DropOwner != nil ||
				runtime.tournament != nil || runtime.StoryStages != nil,
			DungeonID: int64(runtime.Dungeon.ID),
			MazeIndex: runtime.MazeIndex,
		}
		session.dungeon.mu.Unlock()
		snapshot.ActiveRuntimes++
		if entry.Payload {
			snapshot.PayloadRuntimes++
		}
		snapshot.TotalRooms += entry.Rooms
		snapshot.TotalLayerChains += entry.LayerChains
		if entry.Rooms > snapshot.MaxRooms {
			snapshot.MaxRooms = entry.Rooms
		}
		if entry.LayerChains > snapshot.MaxLayerChains {
			snapshot.MaxLayerChains = entry.LayerChains
		}
		snapshot.Runtimes = append(snapshot.Runtimes, entry)
	}
	sort.Slice(snapshot.Runtimes, func(i, j int) bool {
		return snapshot.Runtimes[i].CharacterID < snapshot.Runtimes[j].CharacterID
	})
	return snapshot
}
