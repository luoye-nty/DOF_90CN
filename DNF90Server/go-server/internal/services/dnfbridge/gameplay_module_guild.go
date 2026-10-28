package dnfbridge

import (
	"encoding/hex"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func guildGameplayModule() gameplayModuleDefinition {
	handlers := map[uint16]func(*Service, *gameSession, []byte) error{
		uint16(dnfenum.CmdPacketGuildMemerList):                 (*Service).handleCurrentGuildMemberListLegacy,
		uint16(dnfenum.CmdPacketCallGuildCreateRight):           (*Service).handleCurrentGuildCreate,
		uint16(dnfenum.CmdPacketGuildInfo):                      (*Service).handleCurrentGuildInfo,
		uint16(dnfenum.CmdPacketSetSubGuildMaster):              (*Service).handleCurrentGuildLegacyChangeGrade,
		uint16(dnfenum.CmdPacketGuildMemerListNext):             (*Service).handleCurrentGuildMemberListLegacyNext,
		uint16(dnfenum.CmdPacketGuildAllMemberList):             (*Service).handleCurrentGuildMemberList,
		uint16(dnfenum.CmdPacketGuildAllMemberListNext):         (*Service).handleCurrentGuildMemberListNext,
		uint16(dnfenum.CmdPacketCallGuildInvite):                (*Service).handleCurrentGuildInvite,
		uint16(dnfenum.CmdPacketReplyGuildInvite):               (*Service).handleCurrentGuildInviteReply,
		uint16(dnfenum.CmdPacketReqGuildSecede):                 (*Service).handleCurrentGuildLeave,
		uint16(dnfenum.CmdPacketNotifyMessageToGuild):           (*Service).handleCurrentGuildNotify,
		uint16(dnfenum.CmdPacketGuildMasterDelegate):            (*Service).handleCurrentGuildDelegate,
		uint16(dnfenum.CmdPacketCheckGuildNameDouble):           (*Service).handleCurrentGuildNameCheck,
		uint16(dnfenum.CmdPacketWriteGuildMemberMemo):           (*Service).handleCurrentGuildMemberMemo,
		uint16(dnfenum.CmdPacketBreakGuild):                     (*Service).handleCurrentGuildDisband,
		uint16(dnfenum.CmdPacketRefreshGuildInfo):               (*Service).handleCurrentGuildRefreshInfo,
		uint16(dnfenum.CmdPacketOpenGuildBoard):                 (*Service).handleCurrentGuildBoardOpen,
		uint16(dnfenum.CmdPacketRefreshGuildboard):              (*Service).handleCurrentGuildBoardRefresh,
		uint16(dnfenum.CmdPacketCheckJoinGuild):                 (*Service).handleCurrentGuildJoinCheck,
		uint16(dnfenum.CmdPacketRequestJoinGuild):               (*Service).handleCurrentGuildApply,
		uint16(dnfenum.CmdPacketCancelJoinGuild):                (*Service).handleCurrentGuildCancelApply,
		uint16(dnfenum.CmdPacketApproveJoinGuild):               (*Service).handleCurrentGuildApprove,
		uint16(dnfenum.CmdPacketDenyJoinGuild):                  (*Service).handleCurrentGuildDeny,
		uint16(dnfenum.CmdPacketGuildJoinList):                  (*Service).handleCurrentGuildApplicationList,
		uint16(dnfenum.CmdPacketGuildAttendanceInfo):            (*Service).handleCurrentGuildAttendance,
		uint16(dnfenum.CmdPacketJoinGuildInfo):                  (*Service).handleCurrentGuildApplicationNotice,
		uint16(dnfenum.CmdPacketCheckGuildCreatePromoteMsg):     (*Service).handleCurrentGuildPromoterCheck,
		uint16(dnfenum.CmdPacketModifyGuildPromoteMsg):          (*Service).handleCurrentGuildPromotion,
		uint16(dnfenum.CmdPacketRequestGuildCreatePermit):       (*Service).handleCurrentGuildCreatePermit,
		uint16(dnfenum.CmdPacketReplyGuildCreatePermit):         (*Service).handleCurrentGuildCreatePermitReply,
		uint16(dnfenum.CmdPacketCancelGuildCreate):              (*Service).handleCurrentGuildCreateCancel,
		uint16(dnfenum.CmdPacketReqGuildInfoOfMyChars):          (*Service).handleCurrentGuildAccountList,
		uint16(dnfenum.CmdPacketReqGuildSerchForJoin):           (*Service).handleCurrentGuildDetail,
		uint16(dnfenum.CmdPacketTodayGuildAttendanceDetailInfo): (*Service).handleCurrentGuildAttendanceDetail,
		uint16(dnfenum.CmdPacketReqGuildMileageHistory):         (*Service).handleCurrentGuildMileageHistory,
		uint16(dnfenum.CmdPacketChangeGuildGrade):               (*Service).handleCurrentGuildChangeGrade,
		uint16(dnfenum.CmdPacketChangeGuildGradeName):           (*Service).handleCurrentGuildChangeGradeName,
		uint16(dnfenum.CmdPacketChangeGuildMark):                (*Service).handleCurrentGuildMark,
		uint16(dnfenum.CmdPacketSetGuildFlagAura):               (*Service).handleCurrentGuildAura,
		uint16(dnfenum.CmdPacketBuyGuildContents):               (*Service).handleCurrentGuildContentsPurchase,
		uint16(dnfenum.CmdPacketCheckCreateGuildAgit):           (*Service).handleCurrentGuildAgitCheck,
		uint16(dnfenum.CmdPacketCreateGuildAgit):                (*Service).handleCurrentGuildAgitCreate,
		uint16(dnfenum.CmdPacketDeleteGuildAgit):                (*Service).handleCurrentGuildAgitDelete,
		uint16(dnfenum.CmdPacketUpgradeGuildAgit):               (*Service).handleCurrentGuildAgitUpgrade,
		uint16(dnfenum.CmdPacketContractOfGuild):                (*Service).handleCurrentGuildContractClaim,
		uint16(dnfenum.CmdPacketChangeGuildName):                (*Service).handleCurrentGuildRename,
		uint16(dnfenum.CmdPacketSetGuildRecommandChannel):       (*Service).handleCurrentGuildRecommendedChannel,
		uint16(dnfenum.CmdPacketReqRecommendGuild):              (*Service).handleCurrentGuildRecommend,
		uint16(dnfenum.CmdPacketSearchGuildList):                (*Service).handleCurrentGuildSearch,
		uint16(dnfenum.CmdPacketRequestAccountGuildList):        (*Service).handleCurrentGuildAccountListIndex,
		uint16(dnfenum.CmdPacketGuildAllMemberGrade):            (*Service).handleCurrentGuildAllGrades,
		uint16(dnfenum.CmdPacketGuildAllMemberGradeNext):        (*Service).handleCurrentGuildAllGradesNext,
		// 公会金库 (guild cargo), ported from the upstream guild branch. These five
		// were the only cargo opcodes the upstream module registered and this one
		// did not; see guild_cargo.go for the wire formats.
		uint16(dnfenum.CmdPacketGuildCargo):         (*Service).handleCurrentGuildCargo,
		uint16(dnfenum.CmdPacketGuildCargoHistory):  (*Service).handleCurrentGuildCargoHistory,
		uint16(dnfenum.CmdPacketGuildCargoPushItem): (*Service).handleCurrentGuildCargoPush,
		uint16(dnfenum.CmdPacketGuildCargoPopItem):  (*Service).handleCurrentGuildCargoPop,
		uint16(dnfenum.CmdPacketGuildCargoMoveItem): (*Service).handleCurrentGuildCargoMove,
		// 公会红包 (guild hongbao) list reads, same upstream branch. They read the
		// shared 公会贡献度 ledger; see guild_hongbao.go and guild_contribution.go.
		uint16(dnfenum.CmdPacketGetGuildHongbaoList):      (*Service).handleCurrentGuildHongbaoList,
		uint16(dnfenum.CmdPacketGetGuildHongbaoPointList): (*Service).handleCurrentGuildHongbaoPoints,
		// 公会战 (agit war) + 守护神, from the same upstream branch. opcode 1345 is
		// special: it is shared with the "town scene ready" ACK this tree used to
		// handle inline in game_dispatch_legacy.go, and handleCurrentGuildWarInfo
		// now owns that check -- see its head for why the order matters.
		uint16(dnfenum.CmdPacketAgitWarGuardian):             (*Service).handleCurrentGuildGuardian,
		uint16(dnfenum.CmdPacketAgitWarExtend):               (*Service).handleCurrentGuildAreaExtend,
		uint16(dnfenum.CmdPacketAgitWarUpgradeBuilding):      (*Service).handleCurrentGuildWarShopUpgrade,
		uint16(dnfenum.CmdPacketAgitWarShop):                 (*Service).handleCurrentGuildWarShopBuy,
		uint16(dnfenum.CmdPacketAgitWarInfo):                 (*Service).handleCurrentGuildWarInfo,
		uint16(dnfenum.CmdPacketAgitWarMissionReward):        (*Service).handleCurrentGuildWarMissionReward,
		uint16(dnfenum.CmdPacketAgitWarMatching):             (*Service).handleCurrentGuildWarMatching,
		uint16(dnfenum.CmdPacketAgitWarMatchingCancel):       (*Service).handleCurrentGuildWarCancel,
		uint16(dnfenum.CmdPacketAgitWarSelectChallengeGuild): (*Service).handleCurrentGuildWarRivals,
		uint16(dnfenum.CmdPacketReqRepresentCharacter):       (*Service).handleCurrentGuildRepresentative,
	}
	legacy := make(map[uint16]gameplayHandler, len(handlers))
	upper := make(map[uint16]gameplayHandler, len(handlers))
	for opcode, route := range handlers {
		// Guild requests are logged on arrival. Every rejection in this module
		// funnels into the same generic failure byte, so without this an
		// unhandled UI action and a rejected one look identical from the log,
		// and "the client sent nothing at all" is indistinguishable from
		// either -- which is exactly the ambiguity that made the grade and
		// permission bugs take a round trip each to place.
		opcode, route := opcode, route
		route = func(inner func(*Service, *gameSession, []byte) error) func(*Service, *gameSession, []byte) error {
			return func(service *Service, session *gameSession, body []byte) error {
				service.logGameEvent(session, "game-guild-request",
					"opcode", opcode, "runtime_cmd_name", dnfenum.CmdPacketName(opcode),
					"body_len", len(body), "body", hex.EncodeToString(body))
				return inner(service, session, body)
			}
		}(route)
		legacy[opcode] = func(service *Service, session *gameSession, request gameplayRequest) error {
			return route(service, session, request.Body)
		}
		upper[opcode] = defaultClassGameplayHandler(
			"game-guild-classification-rejected",
			"current_exe_guild_command_requires_default_classification",
			route,
		)
	}
	return gameplayModuleDefinition{Name: "guild", LegacyHandlers: legacy, UpperHandlers: upper}
}
