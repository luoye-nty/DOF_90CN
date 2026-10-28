package dnf90

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	appkit "longheng.io/server/internal/platform/app"
	"longheng.io/server/internal/services/dnfbridge"
)

const gmAdminScope = "dnf-gm"

const (
	gmGoldOperation             = "POST /dnf/gm/characters/{character_id}/gold"
	gmLevelOperation            = "POST /dnf/gm/characters/{character_id}/level"
	gmMailOperation             = "POST /dnf/gm/characters/{character_id}/mail"
	gmMailboxDeleteOperation    = "POST /dnf/gm/characters/{character_id}/mailbox/delete"
	gmEquipmentUnlockOperation  = "POST /dnf/gm/characters/{character_id}/equipment-slots/unlock"
	gmCareerOperation           = "POST /dnf/gm/characters/{character_id}/career"
	gmQuestAcceptOperation      = "POST /dnf/gm/characters/{character_id}/quests/{quest_id}/accept"
	gmQuestReadyOperation       = "POST /dnf/gm/characters/{character_id}/quests/{quest_id}/ready"
	gmQuestGiveUpOperation      = "POST /dnf/gm/characters/{character_id}/quests/{quest_id}/give-up"
	gmQuestCompleteOperation    = "POST /dnf/gm/characters/{character_id}/quests/{quest_id}/complete"
	gmQuestUncompleteOperation  = "POST /dnf/gm/characters/{character_id}/quests/{quest_id}/uncomplete"
	gmAccountValueOperation     = "POST /dnf/gm/accounts/{account_id}/value"
	gmSkillBonusOperation       = "POST /dnf/gm/characters/{character_id}/skill-points"
	gmInventoryDeleteOperation  = "POST /dnf/gm/characters/{character_id}/inventory/delete"
	gmInventoryClearOperation   = "POST /dnf/gm/characters/{character_id}/inventory/clear"
	gmEquipmentUnequipOperation = "POST /dnf/gm/characters/{character_id}/equipment/unequip"
	gmEquipmentDeleteOperation  = "POST /dnf/gm/characters/{character_id}/equipment/delete"
	gmAccountCubeOperation      = "POST /dnf/gm/characters/{character_id}/account-cubes"
	gmQuestBatchOperation       = "POST /dnf/gm/characters/{character_id}/quests/complete-batch"
	gmDungeonDifficultyUnlock   = "POST /dnf/gm/characters/{character_id}/dungeons/unlock-difficulties"
	gmHellChallengeUnlock       = "POST /dnf/gm/characters/{character_id}/dungeons/unlock-hell"
	gmRaidCounterResetOperation = "POST /dnf/gm/characters/{character_id}/raid-counters/reset"
	gmHellDropRateOperation     = "POST /dnf/gm/gameplay/hell-drop-rate"
	gmRaidRewardRateOperation   = "POST /dnf/gm/gameplay/raid-reward-rate"
	// gmEpicPotionTestMultiplierOperation 是"城镇增益药剂临时测试倍率"（0 = 关闭，
	// 按 PVF 表走）。测试用，勿当生产配置。
	gmEpicPotionTestMultiplierOperation = "POST /dnf/gm/gameplay/epic-potion-test-multiplier"
	// gmChannelReloadOperation 频道热重载：重读 channel_info.etc 并热替换频道表 +
	// 对账游戏端口监听，不需要重启进程。
	gmChannelReloadOperation = "POST /dnf/gm/channels/reload"
	// gmRecordQueryOperation 记录查询。只读操作，但沿用统一的操作串便于审计日志对齐。
	gmRecordQueryOperation = "POST /dnf/gm/records/query"
)

type gmAPIResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// gmRecordMaxOffset 记录查询允许的最大偏移。分页深度需要上限：偏移越大，
// 服务端就越要把前面的命中全部扫一遍再丢掉。10 万条足够翻遍实际保留期内的记录。
const gmRecordMaxOffset = 100000

func registerGMAdminRoutes(env *appkit.Env, service *dnfbridge.Service) {
	if env == nil || env.AdminMux == nil || service == nil {
		return
	}
	read := func(handler http.HandlerFunc) http.HandlerFunc {
		if env.AdminAuth != nil {
			return env.AdminAuth(gmAdminScope, handler)
		}
		return handler
	}
	write := func(operation string, handler http.HandlerFunc) http.HandlerFunc {
		if env.AdminDangerous != nil {
			return env.AdminDangerous(gmAdminScope, operation, handler)
		}
		if env.AdminAuth != nil {
			return env.AdminAuth(gmAdminScope, handler)
		}
		return handler
	}

	env.AdminMux.HandleFunc("GET /dnf/gm/status", read(func(w http.ResponseWriter, r *http.Request) {
		writeGMData(w, http.StatusOK, service.GMStatus(r.Context()))
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/channels", read(func(w http.ResponseWriter, r *http.Request) {
		channels, err := service.GMChannels()
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, channels)
	}))
	// 频道热重载：运维侧改完 channel_info.etc 后调这里生效，不必重启 DNF90Server。
	// 属于改运行态的操作，因此走 write()（危险操作包装：强制幂等键，可选确认头）。
	env.AdminMux.HandleFunc("POST /dnf/gm/channels/reload", write(gmChannelReloadOperation, func(w http.ResponseWriter, r *http.Request) {
		result, err := service.GMReloadChannels(r.Context())
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, result)
	}))
	// 记录查询（点券消费/对话/副本）。三类记录都落在服务端的 JSONL 文件里，
	// 这里只做检索，不改任何运行态，所以走 read()。
	env.AdminMux.HandleFunc("GET /dnf/gm/records/meta", read(func(w http.ResponseWriter, r *http.Request) {
		writeGMData(w, http.StatusOK, service.GMRecordMeta())
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/records/query", read(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Kind          string `json:"kind"`
			Limit         int    `json:"limit"`
			Offset        int    `json:"offset"`
			Keyword       string `json:"keyword"`
			CharacterID   string `json:"character_id"`
			CharacterName string `json:"character_name"`
			AccountID     string `json:"account_id"`
			Since         string `json:"since"`
			Until         string `json:"until"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		if request.Offset < 0 || request.Offset > gmRecordMaxOffset {
			writeGMToolError(w, fmt.Errorf("offset must be between 0 and %d", gmRecordMaxOffset))
			return
		}
		since, err := gmParseRecordTime(request.Since)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		until, err := gmParseRecordTime(request.Until)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		page, err := service.GMQueryRecords(dnfbridge.GMRecordQuery{
			Kind:          request.Kind,
			Limit:         request.Limit,
			Offset:        request.Offset,
			Keyword:       request.Keyword,
			CharacterID:   request.CharacterID,
			CharacterName: request.CharacterName,
			AccountID:     request.AccountID,
			Since:         since,
			Until:         until,
		})
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, page)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/gameplay", read(func(w http.ResponseWriter, r *http.Request) {
		settings, err := service.GMGameplaySettings(r.Context())
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, settings)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/gameplay/raid-reward-pools", read(func(w http.ResponseWriter, r *http.Request) {
		pools, err := service.GMRaidRewardPools(r.Context())
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, pools)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/gameplay/raid-reward-rate", read(func(w http.ResponseWriter, r *http.Request) {
		rate, err := service.GMRaidRewardRate(r.Context())
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, rate)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/gameplay/raid-reward-rate", write(gmRaidRewardRateOperation, func(w http.ResponseWriter, r *http.Request) {
		var request dnfbridge.GMRaidRewardRate
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		rate, err := service.GMSetRaidRewardRate(r.Context(), request)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, rate)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/gameplay/hell-drop-rate", write(gmHellDropRateOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Percent int `json:"percent"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		settings, err := service.GMSetHellEpicDropRate(r.Context(), request.Percent)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, settings)
	}))
	// 城镇增益药剂（史诗Buff药剂一族）的**临时测试倍率**：0 = 关闭（按 PVF 表走）。
	// 服主要肉眼确认"用了药剂史诗真的掉得更多"，真实倍率只有 2 倍、看不出来，
	// 所以给一个能随手拧的旋钮。它只替换倍数，不绕过"药剂窗口必须仍在生效"这个前置条件。
	env.AdminMux.HandleFunc("POST /dnf/gm/gameplay/epic-potion-test-multiplier", write(gmEpicPotionTestMultiplierOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Percent int `json:"percent"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		settings, err := service.GMSetEpicDropRatePotionTestMultiplier(r.Context(), request.Percent)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, settings)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/accounts", read(func(w http.ResponseWriter, r *http.Request) {
		limit, err := gmLimit(r, 50)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		accounts, err := service.GMListAccounts(r.Context(), r.URL.Query().Get("q"), limit)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, accounts)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/accounts/{account_id}/characters", read(func(w http.ResponseWriter, r *http.Request) {
		characters, err := service.GMListCharacters(r.Context(), r.PathValue("account_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, characters)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/accounts/{account_id}", read(func(w http.ResponseWriter, r *http.Request) {
		detail, err := service.GMAccount(r.Context(), r.PathValue("account_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, detail)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/accounts/{account_id}/value", write(gmAccountValueOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Field string `json:"field"`
			Value uint64 `json:"value"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		detail, err := service.GMSetAccountValue(r.Context(), r.PathValue("account_id"), request.Field, request.Value)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, detail)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}", read(func(w http.ResponseWriter, r *http.Request) {
		character, err := service.GMCharacter(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, character)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/raid-counters", read(func(w http.ResponseWriter, r *http.Request) {
		state, err := service.GMRaidAdmissionCounters(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, state)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/raid-counters/reset", write(gmRaidCounterResetOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Raid    string `json:"raid"`
			Counter string `json:"counter"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		result, err := service.GMResetRaidAdmissionCounter(
			r.Context(),
			r.PathValue("character_id"),
			request.Raid,
			request.Counter,
		)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, result)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/dungeons", read(func(w http.ResponseWriter, r *http.Request) {
		state, err := service.GMDungeonAccess(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, state)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/dungeons/unlock-difficulties", write(gmDungeonDifficultyUnlock, func(w http.ResponseWriter, r *http.Request) {
		state, err := service.GMUnlockAllDungeonDifficulties(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, state)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/dungeons/unlock-hell", write(gmHellChallengeUnlock, func(w http.ResponseWriter, r *http.Request) {
		state, err := service.GMUnlockAllHellChallenges(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, state)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/detail", read(func(w http.ResponseWriter, r *http.Request) {
		detail, err := service.GMCharacterDetail(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, detail)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/mailbox", read(func(w http.ResponseWriter, r *http.Request) {
		mailbox, err := service.GMMailbox(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, mailbox)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/equipment-slots", read(func(w http.ResponseWriter, r *http.Request) {
		state, err := service.GMExtraEquipmentSlots(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, state)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/equipment-slots/unlock", write(gmEquipmentUnlockOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Slots []string `json:"slots"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		state, err := service.GMUnlockExtraEquipmentSlots(r.Context(), r.PathValue("character_id"), request.Slots)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, state)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/mailbox/delete", write(gmMailboxDeleteOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			MailIDs []string `json:"mail_ids"`
			Force   bool     `json:"force"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		result, err := service.GMDeleteMailboxMails(r.Context(), r.PathValue("character_id"), request.MailIDs, request.Force)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, result)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/skill-points", write(gmSkillBonusOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			BonusSP int64 `json:"bonus_sp"`
			BonusTP int64 `json:"bonus_tp"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		detail, err := service.GMSetCharacterSkillBonus(r.Context(), r.PathValue("character_id"), request.BonusSP, request.BonusTP)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, detail)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/inventory", read(func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := service.GMInventory(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, snapshot)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/inventory/delete", write(gmInventoryDeleteOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Container string `json:"container"`
			Key       string `json:"key"`
			Count     int64  `json:"count"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		snapshot, err := service.GMDeleteInventoryItem(r.Context(), r.PathValue("character_id"), request.Container, request.Key, request.Count)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, snapshot)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/inventory/clear", write(gmInventoryClearOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Container string `json:"container"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		snapshot, err := service.GMClearInventory(r.Context(), r.PathValue("character_id"), request.Container)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, snapshot)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/equipment/unequip", write(gmEquipmentUnequipOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Key string `json:"key"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		snapshot, err := service.GMUnequipEquipment(r.Context(), r.PathValue("character_id"), request.Key)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, snapshot)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/equipment/delete", write(gmEquipmentDeleteOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Key string `json:"key"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		snapshot, err := service.GMDeleteEquipment(r.Context(), r.PathValue("character_id"), request.Key)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, snapshot)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/account-cubes", read(func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := service.GMAccountCubes(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, snapshot)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/account-cubes", write(gmAccountCubeOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Selection int   `json:"selection"`
			Count     int64 `json:"count"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		snapshot, err := service.GMSetAccountCube(r.Context(), r.PathValue("character_id"), request.Selection, request.Count)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, snapshot)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/career", read(func(w http.ResponseWriter, r *http.Request) {
		career, err := service.GMCareer(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, career)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/career", write(gmCareerOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Job       int `json:"job"`
			First     int `json:"first"`
			Awakening int `json:"awakening"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		career, err := service.GMSetCareer(r.Context(), r.PathValue("character_id"), request.Job, request.First, request.Awakening)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, career)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/quests", read(func(w http.ResponseWriter, r *http.Request) {
		overview, err := service.GMQuestOverview(r.Context(), r.PathValue("character_id"))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, overview)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/quests/search", read(func(w http.ResponseWriter, r *http.Request) {
		limit, err := gmLimit(r, 40)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		quests, err := service.GMSearchQuests(r.Context(), r.PathValue("character_id"), r.URL.Query().Get("q"), limit)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, quests)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/characters/{character_id}/quests/{quest_id}/detail", read(func(w http.ResponseWriter, r *http.Request) {
		questID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("quest_id")), 10, 64)
		if err != nil || questID <= 0 {
			writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
			return
		}
		detail, err := service.GMQuestDetail(r.Context(), r.PathValue("character_id"), questID)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, detail)
	}))
	registerGMQuestMutation := func(pattern string, operation string, mutate func(context.Context, string, int64) (dnfbridge.GMQuestMutationResult, error)) {
		env.AdminMux.HandleFunc(pattern, write(operation, func(w http.ResponseWriter, r *http.Request) {
			questID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("quest_id")), 10, 64)
			if err != nil {
				writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
				return
			}
			result, err := mutate(r.Context(), r.PathValue("character_id"), questID)
			if err != nil {
				writeGMToolError(w, err)
				return
			}
			writeGMData(w, http.StatusOK, result)
		}))
	}
	registerGMQuestMutation("POST /dnf/gm/characters/{character_id}/quests/{quest_id}/accept", gmQuestAcceptOperation, service.GMAcceptQuest)
	registerGMQuestMutation("POST /dnf/gm/characters/{character_id}/quests/{quest_id}/ready", gmQuestReadyOperation, service.GMMarkQuestReady)
	registerGMQuestMutation("POST /dnf/gm/characters/{character_id}/quests/{quest_id}/give-up", gmQuestGiveUpOperation, service.GMGiveUpQuest)
	registerGMQuestMutation("POST /dnf/gm/characters/{character_id}/quests/{quest_id}/complete", gmQuestCompleteOperation, service.GMForceCompleteQuest)
	registerGMQuestMutation("POST /dnf/gm/characters/{character_id}/quests/{quest_id}/uncomplete", gmQuestUncompleteOperation, service.GMUncompleteQuest)
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/quests/complete-batch", write(gmQuestBatchOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			QuestIDs []int64 `json:"quest_ids"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		overview, err := service.GMCompleteQuestBatch(r.Context(), r.PathValue("character_id"), request.QuestIDs)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, overview)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/gold", write(gmGoldOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Value int64 `json:"value"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		character, err := service.GMSetCharacterGold(r.Context(), r.PathValue("character_id"), request.Value)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, character)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/level", write(gmLevelOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Level int `json:"level"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		character, err := service.GMSetCharacterLevelForTool(r.Context(), r.PathValue("character_id"), request.Level)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, character)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/items", read(func(w http.ResponseWriter, r *http.Request) {
		limit, err := gmLimit(r, 30)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		items, err := service.GMSearchItems(r.Context(), r.URL.Query().Get("q"), limit)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, items)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/items/page", read(func(w http.ResponseWriter, r *http.Request) {
		limit, err := gmLimit(r, 100)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		parseInt := func(name string, fallback int64) (int64, error) {
			raw := strings.TrimSpace(r.URL.Query().Get(name))
			if raw == "" {
				return fallback, nil
			}
			return strconv.ParseInt(raw, 10, 64)
		}
		minLevel, err := parseInt("min_level", 0)
		if err != nil {
			writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
			return
		}
		maxLevel, err := parseInt("max_level", 0)
		if err != nil {
			writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
			return
		}
		rarity, err := parseInt("rarity", -1)
		if err != nil {
			writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
			return
		}
		offset64, err := parseInt("offset", 0)
		if err != nil || offset64 < 0 || offset64 > int64(^uint(0)>>1) {
			writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
			return
		}
		page, err := service.GMSearchItemPage(r.Context(), dnfbridge.GMItemQuery{
			Query: r.URL.Query().Get("q"), Kind: r.URL.Query().Get("kind"), Tag: r.URL.Query().Get("tag"), Segment: r.URL.Query().Get("segment"), Group: r.URL.Query().Get("group"), Job: r.URL.Query().Get("job"), MinLevel: minLevel,
			MaxLevel: maxLevel, Rarity: rarity, Expiration: r.URL.Query().Get("expiration"), Limit: limit, Offset: int(offset64),
		})
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, page)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/avatar-sets", read(func(w http.ResponseWriter, r *http.Request) {
		limit, err := gmLimit(r, 24)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		offset := 0
		if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
			value, parseErr := strconv.ParseInt(raw, 10, 32)
			if parseErr != nil || value < 0 {
				writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
				return
			}
			offset = int(value)
		}
		page, err := service.GMSearchAvatarSets(r.Context(), dnfbridge.GMAvatarSetQuery{
			Query: r.URL.Query().Get("q"), Job: r.URL.Query().Get("job"), Limit: limit, Offset: offset,
		})
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, page)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/items/{item_id}/detail", read(func(w http.ResponseWriter, r *http.Request) {
		itemID, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("item_id")), 10, 32)
		if err != nil || itemID == 0 {
			writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
			return
		}
		detail, err := service.GMItemDetail(r.Context(), uint32(itemID))
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, detail)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/items/{item_id}/mail-options", read(func(w http.ResponseWriter, r *http.Request) {
		itemID, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("item_id")), 10, 32)
		if err != nil || itemID == 0 {
			writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
			return
		}
		limit, err := gmLimit(r, 40)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		offset := 0
		if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
			offset, err = strconv.Atoi(raw)
		}
		if err != nil || offset < 0 {
			writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
			return
		}
		rarity := int64(-1)
		if raw := strings.TrimSpace(r.URL.Query().Get("rarity")); raw != "" {
			rarity, err = strconv.ParseInt(raw, 10, 64)
		}
		if err != nil || rarity < -1 || rarity > 6 {
			writeGMToolError(w, dnfbridge.ErrGMInvalidRequest)
			return
		}
		page, err := service.GMItemMailOptions(r.Context(), uint32(itemID), r.URL.Query().Get("mode"), r.URL.Query().Get("q"), rarity, limit, offset)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, page)
	}))
	env.AdminMux.HandleFunc("GET /dnf/gm/items/categories", read(func(w http.ResponseWriter, r *http.Request) {
		categories, err := service.GMItemCategories(r.Context())
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusOK, categories)
	}))
	env.AdminMux.HandleFunc("POST /dnf/gm/characters/{character_id}/mail", write(gmMailOperation, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ItemID            uint32   `json:"item_id"`
			Count             int64    `json:"count"`
			Title             string   `json:"title"`
			Body              string   `json:"body"`
			ExpireDays        int      `json:"expire_days"`
			Reinforce         int      `json:"reinforce"`
			AmplifyType       int      `json:"amplify_type"`
			EquipmentQuality  string   `json:"equipment_quality"`
			Forging           int      `json:"forging"`
			EmblemItemIDs     []uint32 `json:"emblem_item_ids"`
			EnchantBeadItemID uint32   `json:"enchant_bead_item_id"`
		}
		if err := decodeGMJSON(w, r, &request); err != nil {
			writeGMToolError(w, err)
			return
		}
		result, err := service.GMSendItemMailWithOptions(
			r.Context(),
			r.PathValue("character_id"),
			request.ItemID,
			request.Count,
			request.Title,
			request.Body,
			dnfbridge.GMMailOptions{
				ExpireDays:        request.ExpireDays,
				Reinforce:         request.Reinforce,
				AmplifyType:       request.AmplifyType,
				EquipmentQuality:  request.EquipmentQuality,
				Forging:           request.Forging,
				EmblemItemIDs:     request.EmblemItemIDs,
				EnchantBeadItemID: request.EnchantBeadItemID,
			},
		)
		if err != nil {
			writeGMToolError(w, err)
			return
		}
		writeGMData(w, http.StatusCreated, result)
	}))
}

func gmLimit(r *http.Request, fallback int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return fallback, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, errors.New("limit must be between 1 and 200")
	}
	return limit, nil
}

// gmParseRecordTime 解析记录查询的时间边界。
//
// 接受两种写法：RFC3339（管理端界面直接给带时区的完整时间），以及 "2006-01-02 15:04:05"
// / "2006-01-02"（手写查询时更省事，按本地时区解释）。空串表示不限。
func gmParseRecordTime(raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if stamp, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return stamp, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid JSON request")
}

func decodeGMJSON(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid JSON request")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON object")
	}
	return nil
}

func writeGMData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(gmAPIResponse{Success: true, Data: data})
}

func writeGMToolError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, dnfbridge.ErrGMCharacterNotFound), errors.Is(err, dnfbridge.ErrGMAccountNotFound):
		status = http.StatusNotFound
	case errors.Is(err, dnfbridge.ErrGMCharacterOnline):
		status = http.StatusConflict
	case errors.Is(err, dnfbridge.ErrGMInvalidRequest):
		status = http.StatusBadRequest
	case errors.Is(err, dnfbridge.ErrGMUnavailable):
		status = http.StatusServiceUnavailable
	default:
		if strings.HasPrefix(err.Error(), "limit must") || strings.HasPrefix(err.Error(), "invalid JSON") || strings.HasPrefix(err.Error(), "request must") {
			status = http.StatusBadRequest
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(gmAPIResponse{Success: false, Error: err.Error()})
}
