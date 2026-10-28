#define _WINSOCK_DEPRECATED_NO_WARNINGS
#include <winsock2.h>
#pragma comment(lib, "ws2_32.lib")
// 90CN / 86JP.dll - current NoPack TCLS launch compatibility plus the
// current-EXE upper-body plaintext codec bypass.
//
// This DLL keeps the six current-EXE TCLS launch helpers needed by the direct
// launcher and patches the proven sub_33AAFE0/sub_33AB0A0 upper-body codec
// boundaries plus bounded DPROTO and inbound-route transport compatibility.
// It also applies bounded current-EXE UI/state compatibility for native views
// whose managers and actor records consume separate server projections. It
// does not hook send/recv, chain-load old DLLs, replay server packet bodies, or
// own gameplay state.

#include "90CN.h"
#include "client_assets.h"
#include "wedding_tree.h"
// 公会 (guild) compatibility headers, ported from the upstream guild branch. They
// are self-contained: they use only BytesMatch / WriteCodePatch / LogLine / g_dnfBase /
// g_originalSceneUiOpen / CurrentActorFn and the RVA constants this file already
// defines, with identical signatures and identical RVAs -- so same EXE, no drift.
#include "guild_member_menu.h"
#include "guild_member_actor.h"
#include "guild_ranking.h"
#include "guild_war_rows.h"
#include "assault_result.h"

#include <windows.h>
#include <stdint.h>
#include <stdio.h>

#include <stdarg.h>
#include <string.h>
#include <time.h>
#include <wchar.h>
#include <intrin.h>
#include <ctype.h>
#include <shellapi.h>

// ============================================================================
//  COMPILE-TIME CONFIGURATION -- this DLL reads no INI file. The only runtime
//  inputs are the three remote-authentication variables the launcher passes to
//  the game process (see the NOTE further down and LoadRemoteAuthConfiguration).
//
//  Everything below used to be an external switch: an environment variable set
//  by the launcher, or an INI file next to DNF.exe. Those inputs are gone; each
//  value is a constant here, taken from what the deployed client actually ran
//  with (90CN_trace.log, 2026-09-17 22:43:35):
//
//      transport DLL installed codec=1 decode=1 route=1 dproto=1 ... route=1
//      [multi] isolated audited instance mutex / existing-window branch  (taken)
//      optional Lua plugin loaded module=53EF0000
//      protocol transport trace enabled=0
//      custom region name resolver ... region=1 name_length=4 source=builtin
//
//  CAUTION: only switches belong here. Anything the launcher supplies per run
//  (the LANA ticket above all) must stay a runtime input; snapshotting a logged
//  "disabled" state into a constant is what broke channel entry on 2026-09-18.
//
//  Change a value and rebuild; nothing outside the binary can change it.
// ============================================================================
constexpr bool kMultiClientLaunch = true;         // was DNF_MULTI_CLIENT=1
constexpr bool kSocketTraceEnabled = false;       // was 90CN_socket_trace.ini [trace] enabled
constexpr bool kProtocolTraceEnabled = false;     // was DNF_PROTOCOL_TRACE=1
// 战斗力与调试浮层：整体停用（2026-09-26）。服务端不需要战斗力，客户端也就不再算。
// 关掉之后：mode1/mode3 的属性与装备快照不再缓存、战斗力面板不再更新、调试浮层不再创建。
// **城镇共现与客户端提示不受影响** —— 提示用的是另一个 overlay 窗口类（kLuaNoticeOverlay*）。
constexpr bool kCombatPowerEnabled = false;       // 战斗力：属性/装备/附魔快照 + 面板
constexpr bool kDebugPanelEnabled = false;        // 开发者调试浮层
constexpr bool kCipherPassthroughEnabled = true;  // was DNF_CIPHER_PASSTHROUGH!=0
constexpr bool kDprotoCompatEnabled = true;       // was DNF_DPROTO_COMPAT!=0
constexpr bool kSceneRouteCompatEnabled = true;   // was DNF_ROUTE_COMPAT!=0
constexpr bool kQuestSingleTargetEnabled = true;  // was DNF_QUEST_SINGLE_TARGET!=0
constexpr bool kOptionalLuaPluginEnabled = true;  // was DNF_LUA_PLUGIN!=0
constexpr unsigned long kResidentChannelID = 11;  // was DNF_RESIDENT_CHANNEL_ID
// ── 商城网页改向（2026-10-17）────────────────────────────────────────────
// 官服里商城右下角的"点券充值 / 黑钻会员"是"打开腾讯充值/会员网页"的入口；
// 私服没有那些后台，点了毫无反馈。这里把两个按钮统一指到我们自己的页面。
// 地址可被客户端目录下的 90CN.ini 覆盖，不改就能重编改默认值：
//     [web]
//     shop_url=https://你的地址
constexpr bool kShopWebRedirectEnabled = true;   // 总开关
constexpr bool kShopWebHookAwesomium = true;     // 同时钩内嵌浏览器的 URL 构造
constexpr wchar_t kShopWebUrlDefault[] = L"https://68n.cn/R9oOj";
// NOTE: remote authentication (the LANA challenge) is deliberately NOT a constant.
// Its three inputs are runtime values that the launcher passes to this game process
// (DNF_REMOTE_AUTH_TICKET / _HOST / _PORTS). The ticket is issued per login by the
// server, so it can never be baked into the binary -- freezing it here is exactly
// what silently disabled remote authentication and broke channel entry.
// The optional fourth input, DNF_LID_LEASE, is the launcher's signed identity
// proof. It is forwarded verbatim and never verified here: the private key lives in
// the launcher, so a player who copies this DLL still cannot mint a lease.
// See LoadRemoteAuthConfiguration() below.

namespace
{
constexpr uintptr_t kCipherEncodeRva = 0x02FAAFE0; // sub_33AAFE0
constexpr uintptr_t kCipherDecodeRva = 0x02FAB0A0; // sub_33AB0A0
constexpr uintptr_t kCipherEncodeCallRva = 0x030569A0; // upper_pkt_flush_send13 call-site
constexpr uintptr_t kCodecSetKey0Rva = 0x03077B30;
constexpr uintptr_t kCodecSetKey2Rva = 0x03076E90;
constexpr uintptr_t kCodecSetKey3Rva = 0x03077E30;
constexpr uintptr_t kCodecSetKey7Rva = 0x03075900;
constexpr uintptr_t kCodecSetKey8Rva = 0x03078260;
constexpr uintptr_t kTclsParseRva = 0x015AA570;
constexpr uintptr_t kTclsFetchLoginRva = 0x01AF7C40;
constexpr uintptr_t kTclsFetchTextRva = 0x01AF6970;
constexpr uintptr_t kTclsFetchCryptoRva = 0x01AF6A50;
constexpr uintptr_t kTclsFetchTailRva = 0x01AF6DE0;
constexpr uintptr_t kTclsTailRva = 0x01AF6F50;
constexpr uintptr_t kGameWideAssignRva = 0x00038270;
constexpr uintptr_t kChecksumRva = 0x030545B0;
constexpr uintptr_t kDprotoRoutePredicateRva = 0x030734E0; // sub_34734E0
constexpr uintptr_t kDprotoOutgoingGateRva = 0x030738FA;   // JZ after mode-7 predicate
constexpr uintptr_t kDprotoOutgoingCloneRva = 0x0307391C;  // first mode-7 call
constexpr uintptr_t kDprotoOutgoingResumeRva = 0x03073921; // after stolen 5 bytes at clone site
constexpr uintptr_t kDprotoOutgoingReturnRva = 0x03073A2E;
constexpr uintptr_t kDprotoFalseResumeRva = 0x03073A2F;    // native predicate-false epilogue
constexpr uintptr_t kQuestAutoCompleteCallContextRva = 0x02C4DBC8; // sub_304DB90 selected quest confirmation
constexpr uintptr_t kQuestAutoCompleteCallRva = 0x02C4DBD2;
constexpr uintptr_t kQuestAutoCompleteCaptureRva = 0x02C4E12E; // sub_304E070 stores selected quest at callback+0x30
constexpr uintptr_t kQuestAutoCompleteSendRva = 0x015EE300; // sub_19EE300
constexpr uintptr_t kGameAllocatorRva = 0x00669630;
constexpr uintptr_t kGameMemcpyRva = 0x037106F0;
constexpr uintptr_t kChannelScriptDownloadRva = 0x0189FB00; // sub_1C9FB00
constexpr uintptr_t kChannelScriptLoadRva = 0x014A5360;     // sub_18A5360
constexpr uintptr_t kChannelDirectoryApplyRva = 0x018A2440; // sub_1CA2440
constexpr uintptr_t kChannelRuntimeLoadRva = 0x014A2780;     // sub_18A2780
constexpr uintptr_t kChannelCategoryInsertRva = 0x014A1FF0;  // sub_18A1FF0
constexpr uintptr_t kChannelConnectRva = 0x018A0DA0;         // sub_1CA0DA0
// The current EXE's class0/op3 handler can receive a user-state update before
// the matching UI object exists. Its third lookup misses the null guard used by
// the neighbouring paths and later dereferences a null `this` pointer.
constexpr uintptr_t kUserStateUiGuardRva = 0x01988B91;       // mov edi, eax
constexpr uintptr_t kUserStateUiResumeRva = 0x01988B98;      // push eax
constexpr uintptr_t kUserStateUiSkipRva = 0x01988BA4;        // native null label
constexpr uintptr_t kUserStateUiLookupRva = 0x02294670;      // sub_2694670
// The current EXE's class-0/op312 handler (wire opcode 0x0138, the server's
// "complete-condition pass gate" that arms the event-monster trap family) does
// a second scene lookup and immediately dereferences the result at +0x1840:
//
//   call sub_1D93480            ; esi = scene lookup
//   mov  esi, eax               ; 0x01D8A5C7
//   mov  ecx, esi
//   mov  [ebp-0x24], esi
//   call sub_17C4F80            ; mov eax,[ecx+0x1840]  <-- 0xC0000005 when esi==0
//   ...
//   test esi, esi               ; 0x01D8A5FE - the guard runs 0x30 bytes late
//   je   0x01D8A842             ; native null epilogue
//
// Reproduced live: the settlement page's "select another dungeon" re-entry
// skips the town round trip, the lookup misses, and the client dies at
// DNF.exe+0x13C4F80 with ecx=0 / access_address=0x1840. The five-byte call is
// therefore replaced by a stub that bails to the handler's own null epilogue,
// which is exactly what the native guard at 0x01D8A600 does. Pure control
// flow - no native state is written.
constexpr uintptr_t kDungeonEventMonsterConditionLookupCallRva = 0x0198A5C2; // call sub_1D93480
constexpr uintptr_t kDungeonEventMonsterConditionLookupRva     = 0x01993480; // sub_1D93480 (RVA!)
constexpr uintptr_t kDungeonEventMonsterConditionResumeRva     = 0x0198A5C7; // mov esi, eax
constexpr uintptr_t kDungeonEventMonsterConditionNullSkipRva   = 0x0198A842; // native null epilogue
constexpr uintptr_t kSelectedChannelPointerRva = 0x04D88FA8; // dword_5188FA8
constexpr uintptr_t kResidentChannelRva = 0x04D89178;        // unk_5189178
constexpr uintptr_t kChannelScriptLookupRva = 0x032B1910;    // sub_36B1910
constexpr uintptr_t kChannelScriptStoreRva = 0x04D88FF0;     // unk_5188FF0
constexpr uintptr_t kPvpChannelQueryRva = 0x01498C30;        // sub_1898C30
// sub_2272670 is the current EXE's single region-id text resolver. Native
// behavior maps id N to localized resource 71700+N. A bounded optional hook
// lets the local profile use one custom display name consistently in the
// friend list, guild region selector, character creation and every other
// native region label while retaining the original table for all other ids.
constexpr uintptr_t kRegionNameResolverRva = 0x01E72670;     // sub_2272670
constexpr uintptr_t kPvpQueryType8ReturnRva = 0x00DB6E81;
constexpr uintptr_t kPvpQueryType31ReturnRvaA = 0x00DB7823;
constexpr uintptr_t kPvpQueryType41ReturnRvaA = 0x00DB7840;
constexpr uintptr_t kPvpQueryType31ReturnRvaB = 0x00DB1B84;
constexpr uintptr_t kPvpQueryType41ReturnRvaB = 0x00DB1BCD;
constexpr uintptr_t kHudServerIndexDescriptorRva = 0x04DB0EE4; // dword_51B0EE4
constexpr uintptr_t kHudServerIndexValueRva = 0x04DB0EE8;      // dword_51B0EE8
constexpr uintptr_t kHudServerIndexLockRva = 0x04DB0EEC;       // dword_51B0EEC
constexpr uintptr_t kHudChannelIndexDescriptorRva = 0x04DB0EF0; // dword_51B0EF0
constexpr uintptr_t kHudChannelIndexValueRva = 0x04DB0EF4;      // dword_51B0EF4
constexpr uintptr_t kHudChannelIndexLockRva = 0x04DB0EF8;       // dword_51B0EF8
constexpr uintptr_t kServerClockDescriptorRva = 0x04DB0F20; // dword_51B0F20
constexpr uintptr_t kServerClockValueRva = 0x04DB0F24;      // dword_51B0F24
constexpr uintptr_t kLocalClockDescriptorRva = 0x04DB0F28;  // dword_51B0F28
constexpr uintptr_t kLocalClockValueRva = 0x04DB0F2C;       // dword_51B0F2C
constexpr uintptr_t kServerTickDescriptorRva = 0x04DB0F30;  // dword_51B0F30
constexpr uintptr_t kServerTickValueRva = 0x04DB0F34;       // dword_51B0F34
constexpr uintptr_t kAtomicSpinLockRva = 0x030DB470;         // sub_34DB470
constexpr uintptr_t kAtomicSpinUnlockRva = 0x030DB4A0;       // sub_34DB4A0
constexpr uintptr_t kObfuscatedStoreCase0Rva = 0x030DB910;   // sub_34DB910
constexpr uintptr_t kObfuscatedStoreCase4Rva = 0x030DB990;   // sub_34DB990
constexpr uintptr_t kObfuscatedStoreCase8Rva = 0x030DBA10;   // sub_34DBA10
constexpr uintptr_t kSelectedPageApplyRva = 0x013D6BE0;      // sub_17D6BE0
// sub_16A2660 handles the Cera-shop VIP button. This UI variant leaves the
// control at [esi+0x248B0] null, so the native click path crashes/hangs.
constexpr uintptr_t kCeraShopVipClickRva = 0x012A2660;       // sub_16A2660
constexpr uintptr_t kSelectorCreateTickRva = 0x02FE9640;     // sub_33E9640
constexpr uintptr_t kSelectorCreateTransitionRva = 0x02FE8390; // sub_33E8390
constexpr uintptr_t kSelectorRestrictionRva = 0x04DB27D0;    // byte_51B27D0
constexpr uintptr_t kUIEventStampRva = 0x04CE0E04;           // dword_50E0E04
constexpr uintptr_t kCreateUIClickRva = 0x01AAC2A0;          // sub_1EAC2A0
constexpr uintptr_t kCreateUIOpenRva = 0x01AAC3E0;           // sub_1EAC3E0
constexpr uintptr_t kUpperCreateSendRva = 0x02FD4990;        // sub_33D4990
constexpr uintptr_t kClass0DispatchRva = 0x01E5CA00;         // sub_225CA00
constexpr uintptr_t kClass1DispatchRva = 0x01E5C8A0;         // sub_225C8A0
constexpr uintptr_t kClass0RegistryRva = 0x01E62A80;         // sub_2262A80
constexpr uintptr_t kClass1RegistryRva = 0x01E628F0;         // sub_22628F0
constexpr uintptr_t kDispatchLookupRva = 0x01E62430;         // sub_2262430
// The current EXE's generic newbie-guide alarm entry. It receives the guide
// index as its first stack argument under __thiscall (manager in ECX) and is
// shared by every entry in etc/newbieguidesystem.etc.
constexpr uintptr_t kNewbieGuideShowRva = 0x005AA6C0;        // sub_9AA6C0
constexpr unsigned short kNewbieGuideOptionOpcode = 1431;
constexpr unsigned int kNewbieGuideMaxIndex = 4095;
constexpr unsigned int kNewbieGuideSeenReset = 0xFFFFFFFFu;
// Class-0/op1378 only stores the server's last accepted story level at
// StoryDigestManager+0x44. The native selected-scene finalizer later calls
// sub_11DADD0(manager, 0), which evaluates the current actor level, starts the
// matching story video, writes the accepted level, and sends bodyless op1445.
// Run that same evaluator after a freshly acknowledged quest finish instead
// of waiting for a later character-selection or dungeon-scene finalizer.
constexpr uintptr_t kStoryDigestManagerGetterRva = 0x00DDBDD0; // sub_11DBDD0
constexpr uintptr_t kStoryDigestEvaluatorRva = 0x00DDADD0;     // sub_11DADD0
constexpr uintptr_t kStoryDigestManagerPointerRva = 0x04D80420; // dword_5180420
constexpr size_t kStoryDigestLastLevelOffset = 0x44;
constexpr unsigned short kQuestFinishResponseOpcode = 34;
constexpr unsigned short kStoryDigestStateOpcode = 1378;
constexpr unsigned int kStoryDigestStatePacketLength = 20;
constexpr ULONGLONG kStoryDigestQuestFinishWindowMs = 2000;
// CmdPacket 1456 is one of the two holes in the current runtime enum table.
// The compatibility DLL uses it only as a C2S capability announcement after
// native class-0/op24 has completed. The server never reflects this private
// packet to the client, so no absent response handler is invoked.
constexpr unsigned short kCommandCapabilityOpcode = 1456;
constexpr unsigned int kCommandCapabilityMagic = 0x314B4341; // "ACK1"
constexpr unsigned short kCommandCapabilityVersion = 1;
constexpr unsigned short kCommandEnumEnd = 1519;
constexpr unsigned short kCommandCapabilityBitmapLength =
    (kCommandEnumEnd + 7) / 8;
constexpr unsigned int kBossMutationRequestMagic = 0x31554D42; // "BMU1"
constexpr unsigned char kBossMutationRequestVersion = 1;
constexpr unsigned short kBossMutationPrivateOpcode = 413;
constexpr unsigned int kBossMutationPrivatePacketLength = 28;
constexpr unsigned char kBossMutationPrivateType = 3;
constexpr unsigned char kBossMutationPrivateVersion = 1;
// The current EXE saves the 512-byte cooldown-reminder table from this global
// object's +0x288 field, but registers no native class-0/391 restore callback.
constexpr uintptr_t kCharacterOptionManagerRva = 0x04D97A98;
constexpr size_t kCharacterOptionTableOffset = 0x288;
constexpr uintptr_t kCharacterOptionRuntimeRefreshRva = 0x01B9E080;
constexpr uintptr_t kCharacterOptionReminderApplyRva = 0x01B9D8D0;
constexpr unsigned short kCharacterOptionRestoreOpcode = 391;
constexpr size_t kCharacterOptionBodyLength = 516;
constexpr size_t kCharacterOptionTableLength = 512;
// Current NoPack event 2347 globals. These are read only after the native
// class0 handlers return, so the trace can distinguish route rejection from a
// UI/resource creation failure without mutating client-owned activity state.
constexpr uintptr_t kSpendTimeCatalogPointerRva = 0x04D78A30; // dword_5178A30
constexpr uintptr_t kSpendTimeUiSharedRva = 0x04D78A80;       // dword_5178A80
// A marked op863 state projection must not reach sub_1D25590: that result
// handler only creates the one-shot [11,8] unlock effect. Current NoPack's
// actual lock gate is sub_26905A0, which reads bit 3 from the current actor's
// state block at resolver-result+0x1AC. Mirror the marked durable state there
// before owner 0xC9 is constructed so reopening the avatar panel is silent.
constexpr uintptr_t kSceneUiOpenRva = 0x02BDD1A0;             // sub_2FDD1A0
constexpr uintptr_t kSceneUiIsOpenRva = 0x02BD4B40;           // sub_2FD4B40
// Party-teleport consent window (dialog 0x1F4). The server publishes the request
// state as phase 0 and, once every member accepted, ends it with phase 2; a refused
// or expired request is published as phase 3 instead, which the client renders as
// its own timeout text and which must keep the window. The client only closes the
// dialog from its 377 reader, and that path is skipped whenever [game+0xAC] == 1
// (the town case), so the window used to stay on screen after a successful
// teleport. This patch restores the close: phase 2 means "the party moved".
constexpr uintptr_t kPartyTeleportDialogStateSetRva = 0x01DCD9C0; // sub_21CD9C0
constexpr uintptr_t kPartyTeleportDialogCloseRva = 0x02BD85D0;    // sub_2FD85D0
constexpr uintptr_t kDialogManagerPointerRva = 0x04DB2764;        // dword_51B2764
constexpr int kPartyTeleportDialogID = 0x1F4;
constexpr int kPartyTeleportEndedPhase = 2;
// The current EXE calls sub_22645B0 from owner 609's native open gate while
// the transplanted NPC conversation is still in a blocked scene sub-state.
// That early return happens before op1291 and before the owner is promoted
// from its fallback vector. Override only that exact return address; all other
// callers retain the native scene-state decision.
constexpr uintptr_t kJoustSceneBlockCheckRva = 0x01E645B0;    // sub_22645B0
constexpr uintptr_t kJoustOpenGateReturnRva = 0x00B2D0E7;     // sub_F2D0A0+0x47
// sub_F2D0A0 checks whether the type-73 state string has a historical
// weekend/10:00 schedule before it emits the joust-opening request.  The
// local server publishes this activity as all-day; retain the native request
// and UI creation but skip only that retired client-side early return.
constexpr uintptr_t kJoustHistoricTimeGateRva = 0x00B2D126;   // jnz loc_F2D15C
// "Channel follow" (class-1/op804) resolves the target's channel with the native
// channel-follow move path: sub_3250DC0 stores the server's (serverId, channelId)
// into the follow-state object, sub_3266250 feeds the channel id to
// sub_1898AC0 (filter by server id) and sub_324E3F0 (find by channel id).
// sub_324E3F0 accepts a matched channel only when its channel_info.etc type is
// 0x16 / 3 / 0 / 0x21 / 0x28 (or the special-context predicate at 0x226DC80
// passes); every ordinary channel (type 11 in the live ETC, 91 of 118 entries)
// falls through to `mov dword [status], 1` and the native UI then reports string
// 0x8224 ("目标角色处在普通或者非特殊区域").  A local server can move a
// character between ordinary channels, so the last type gate is rewritten to an
// unconditional branch: the native request, the native channel lookup, the
// native move and every server-side validation stay untouched, and only the
// retired client-side type whitelist stops rejecting ordinary channels.
// Address convention: every constant in this file is a module-relative RVA and
// is consumed as g_dnfBase + RVA.  The disassembly labels in the channel-follow
// RE notes name the VA instead (image base 0x00400000), so a note address must
// have 0x00400000 subtracted before it is used here: the gate below is named
// loc_3266E52C / VA 0x0324E516 in the notes and is RVA 0x02E4E516 in this file.
constexpr uintptr_t kChannelFollowChannelTypeGateRva = 0x02E4E516; // va 0x0324E516, je loc_3266E52C
// The client's own follow request sender (sub_325F590) and the request body
// (u8 region + DSTR name) are unchanged; the server response layout is
// (mode, flag, DSTR name, serverId, channelId).
// NPC 326 (Naen Sig) owns the native Anton raid shop, while NPC 460 (Erika)
// owns the native Luke raid/material shop.  The current EXE requires retired
// activity-date entries both while building those two NPC menus and when
// owners 1001/1002 handle their Shop buttons.  The active PVF still publishes
// both shops, so override only the four proven caller/entry pairs; every other
// activity-date lookup remains native.
constexpr uintptr_t kActivityDateEntryExistsRva = 0x01C41F70; // sub_2041F70
constexpr uintptr_t kNaenRaidShopNpcGateReturnRva = 0x02E5FEAE;
constexpr uintptr_t kNaenRaidShopButtonGateReturnRva = 0x00CD9E8E;
constexpr int kNaenRaidShopActivityDateEntry = 181;
constexpr uintptr_t kErikaRaidShopNpcGateReturnRva = 0x02E5FF29;
constexpr uintptr_t kErikaRaidShopButtonGateReturnRva = 0x00CD9DDA;
constexpr int kErikaRaidShopActivityDateEntry = 282;
constexpr uintptr_t kPartyDirectoryOwnerGuardRva = 0x02E6CF2C;
constexpr uintptr_t kPartyDirectoryOpenOwnerRva = 0x02E6CF72;
constexpr uintptr_t kPartyDirectoryCloseOwnerARva = 0x02E6D435;
constexpr uintptr_t kPartyDirectoryCloseOwnerBRva = 0x02E6D4B1;
constexpr uintptr_t kPartyDirectoryUiOwner = 9;
constexpr unsigned short kPartyDirectoryRefreshOpcode = 98;
constexpr unsigned char kPartyDirectoryRefreshModeFull = 0;
// sub_17B1DE0 is the native "create attack party" entry. The current build
// hard-codes UI owner 0x76 (RaidCreateWindow) there even though owner 0x7A
// (RaidSelectSingleModeWindow) and both of its button callbacks are complete.
// Patch only the push-immediate byte; the selection window stays entirely
// native and its buttons continue to pass mode 1/0 into owner 0x76 locally.
constexpr uintptr_t kAntonRaidModeSelectContextRva = 0x013B1E02;
constexpr uintptr_t kAntonRaidModeSelectOwnerRva = 0x013B1E0D;
constexpr unsigned char kRaidCreateUiOwner = 0x76;
constexpr unsigned char kRaidModeSelectUiOwner = 0x7A;
// The native class-0/op884 public-directory handler resolves its raid-service
// owner through connection slot 15. The local direct-route topology does not
// create that separate service connection, so the legitimate resolver result
// is null. Unlike the neighboring op844 handler, op884 omitted the native null
// check before sub_235CA60 reads owner+0x2B4. Preserve slot 15 and add only the
// missing guard, returning through op884's existing no-owner cleanup path.
constexpr uintptr_t kRaidDirectoryOwnerContextRva = 0x0194C390;
constexpr uintptr_t kRaidDirectoryOwnerGuardRva = 0x0194C39E;
constexpr uintptr_t kRaidDirectoryOwnerPresentResumeRva = 0x0194C3A7;
constexpr uintptr_t kRaidDirectoryOwnerMissingResumeRva = 0x0194C593;
// Raid-panel interface call whose vtable slot is null while the raid object is
// only partially materialized. Live 2026-09-17 18:31:39.721 (local), 300 ms
// after the client entered hatchery 221:
//   0x025BD2F4  mov eax,[ecx]            ; ecx = [ebp+1C], a raid-side object
//   0x025BD2F6  mov edx,[eax+0x238]      ; virtual slot, measured null
//   0x025BD2FC  push esi
//   0x025BD2FD  call edx                 ; eip=0, exception stack ret0=0x025BD2FF
// The native code already null-checks ecx; only the slot is unchecked.
constexpr uintptr_t kRaidPanelNullCallRva = 0x021BD2F4;
constexpr uintptr_t kRaidPanelNullCallResumeRva = 0x021BD2FF;
// Optional raid-instance sub-object read after clearing a hatchery and
// returning to town. sub_22644B0 -> sub_26B7840 returns [slot+0xCC], which is
// null before that object is built; the +0x140 offset thunk then turns it into
// 0x140 and the compare faults at 0x140+0xA58 = 0xB98. Live 2026-09-17
// 11:49:22 / 16:52:12 / 17:49:43, all `offset=0x01B95C63 access_address=0xB98`.
constexpr uintptr_t kRaidInstanceFlagReadRva = 0x01B95C63;
constexpr uintptr_t kRaidInstanceFlagCallRva = 0x01B79560;
constexpr uintptr_t kRaidInstanceFlagResumeRva = 0x01B95C73;
// The same block repeats twice earlier in the same function; the middle one is
// the site that faulted at 2026-09-17 19:29:00.739 after the first had already
// been skipped.
constexpr uintptr_t kRaidInstanceFlagReadRva1 = 0x01B95611;
constexpr uintptr_t kRaidInstanceFlagCallRva1 = 0x01B60340;
constexpr uintptr_t kRaidInstanceFlagResumeRva1 = 0x01B95621;
constexpr uintptr_t kRaidInstanceFlagReadRva2 = 0x01B9568A;
constexpr uintptr_t kRaidInstanceFlagCallRva2 = 0x01B790E0;
constexpr uintptr_t kRaidInstanceFlagResumeRva2 = 0x01B9569C;
constexpr uintptr_t kRaidInstanceFlagByteOffset = 0xA58;
// Upper bound for "is this address inside the game image": the current DNF.exe
// maps a little under 128 MB, and the guard only needs to tell the client apart
// from an injected module.
constexpr uintptr_t kClientImageSpan = 0x8000000;
constexpr unsigned short kAuraSkinSlotOpcode = 863;
constexpr uintptr_t kAvatarPanelUiOwner = 0xC9;
constexpr uintptr_t kJoustUiOwner = 0x261;
// Current-EXE live owner-vector evidence: 0xC9 is the equipment/inventory
// panel used by the aura compatibility path, while 0xD9 owns the native
// personal-information panel opened by the character-info shortcut.
constexpr uintptr_t kPersonalInfoUiOwner = 0xD9;
constexpr size_t kSceneUiOwnerVectorOffset = 0x108;
constexpr size_t kSceneUiOwnerVectorStride = 20;
constexpr size_t kSceneUiOwnerVectorBeginOffset = 4;
constexpr size_t kSceneUiOwnerVectorEndOffset = 8;
// The personal-info object owns its root D3D widget at +0x30. The current
// widget stores the content rectangle as x/y/width/height at +0x64..+0x70.
// Its skinned frame extends 8 pixels past the content on the right and 13
// pixels above it; those are the exact sidecar attachment edges.
constexpr size_t kPersonalPanelRootWidgetOffset = 0x30;
constexpr size_t kPersonalPanelWidgetXOffset = 0x64;
constexpr size_t kPersonalPanelWidgetYOffset = 0x68;
constexpr size_t kPersonalPanelWidgetWidthOffset = 0x6C;
constexpr size_t kPersonalPanelWidgetHeightOffset = 0x70;
// Current-EXE sub_35009C0/sub_34FFA60 read the active render-canvas width
// and height from qword_50E34B8.  A window can retain a 1600x900 Win32
// client while its D3D canvas is still 1067x600 after a live resolution
// change.  Native widgets use canvas coordinates, whereas owned popup
// overlays use Win32 client pixels, so every overlay anchor and extent must
// be mapped through this per-process canvas instead of assuming 1:1 pixels.
constexpr uintptr_t kNativeRenderCanvasSizeRva = 0x04CE34B8;
// The root widget excludes roughly five pixels of the visible personal-panel
// frame.  Thirteen therefore leaves the screenshot-matched six-pixel gap
// between the two visible borders instead of welding both frames together.
constexpr int kPersonalPanelFrameRight = 13;
constexpr int kPersonalPanelFrameTop = 13;
constexpr unsigned int kAuraSkinSilentRestorePacketLength = 21;
constexpr size_t kAuraSkinActorStateOwnerOffset = 0x4C8;
constexpr size_t kAuraSkinStateResolverVtableOffset = 0xA98;
constexpr size_t kAuraSkinStateFlagsOffset = 0x1AC;
constexpr unsigned int kAuraSkinUnlockedMask = 1u << 3;
constexpr uintptr_t kGameSingletonPointerRva = 0x04EEA4BC;   // dword_52EA4BC
constexpr uintptr_t kGameDispatchGateOffset = 0x002BD068;    // sub_3454600
constexpr uintptr_t kObjectManagerPointerRva = 0x04DB2728;   // dword_51B2728
constexpr uintptr_t kSceneActorManagerPointerRva = 0x04DB2738; // dword_51B2738
constexpr uintptr_t kControlledActorPointerRva = 0x04EB5BB0; // dword_52B5BB0
constexpr uintptr_t kSceneRootPointerRva = 0x04DB2764;       // dword_51B2764
// sub_1FE6D00 prepares both native skill-window catalog lists through
// sub_268D380.  The catalog intentionally contains preview rows for every
// grow type; sub_265D680 is the current EXE's authoritative
// [skill fitness growtype]/[skill fitness second growtype] predicate.
constexpr uintptr_t kSkillCatalogCollectRva = 0x0228D380;    // sub_268D380
constexpr uintptr_t kSkillFitnessPredicateRva = 0x0225D680; // sub_265D680
constexpr uintptr_t kSkillWindowCollectReturnARva = 0x01BE6DA3;
constexpr uintptr_t kSkillWindowCollectReturnBRva = 0x01BE6F91;
constexpr size_t kSkillFitnessGrowTypeBeginOffset = 3208;
constexpr size_t kSkillFitnessGrowTypeEndOffset = 3212;
// The stock client exposes six normal and six extension quick slots. The
// server preserves every historical index and appends normal-7/extension-7.
constexpr int kSkillQuickSlotNormal7 = 204;
constexpr int kSkillQuickSlotExtension7 = 205;
constexpr int kSkillQuickSlotCount = 206;
constexpr int kSkillQuickSlotNormalBase = 0;
constexpr int kSkillQuickSlotExtensionBase = 198;
constexpr int kSkillQuickSlotDragKind = 7;
// The current EXE's hotkey catalog accepts action IDs only through 108.
// 103/104 are unused in both active hotkey catalogs, so reuse them instead
// of adding out-of-range 109/110 rows that the settings UI silently drops.
constexpr unsigned char kSkillQuickSlotNormal7Action = 103;
constexpr unsigned char kSkillQuickSlotExtension7Action = 104;
constexpr uintptr_t kSkillQuickSlotDisplayMapRva = 0x01BE4310; // sub_1FE4310
constexpr uintptr_t kSkillQuickSlotHotkeyDispatchRva = 0x01B7690E;
constexpr uintptr_t kSkillQuickSlotHotkeyResumeRva = 0x01B76914;
constexpr uintptr_t kResolveHotkeyActionRva = 0x0224E340; // sub_264E340
constexpr uintptr_t kHotkeyActionPressedRva = 0x03111EF0; // sub_3511EF0
constexpr uintptr_t kSkillQuickSlotControlActiveRva = 0x03111E30; // sub_3511E30
constexpr uintptr_t kReleaseSharedHandleRva = 0x022D0EA0; // sub_26D0EA0
constexpr uintptr_t kSkillQuickSlotPredicateRva = 0x0228B000; // sub_268B000
constexpr uintptr_t kSkillQuickSlotLookupRva = 0x0228B5B0; // sub_268B5B0
constexpr uintptr_t kSkillQuickSlotMoveRva = 0x0228DA30; // sub_268DA30
constexpr uintptr_t kSkillQuickSlotNormalRenderRva = 0x01B78980; // sub_1F78980
constexpr uintptr_t kSkillQuickSlotExtensionRenderRva = 0x01B4A140; // sub_1F4A140
constexpr uintptr_t kSkillQuickSlotKeyRenderRva = 0x01B78310; // sub_1F78310
constexpr uintptr_t kSkillQuickSlotHudConstructRva = 0x01B7BCF0; // sub_1F7BCF0
constexpr uintptr_t kSkillQuickSlotControlFactoryRva = 0x031138F0; // sub_35138F0
constexpr uintptr_t kSkillQuickSlotControlTypeRva = 0x030E7E10; // sub_34E7E10
constexpr uintptr_t kSkillQuickSlotControlPositionRva = 0x030E78E0; // sub_34E78E0
constexpr uintptr_t kSkillQuickSlotHudParentRva = 0x01B72DE0; // sub_1F72DE0
constexpr uintptr_t kSkillQuickSlotCatalogRva = 0x01BA6EF0; // sub_1FA6EF0
constexpr uintptr_t kSkillQuickSlotKeyCodeRva = 0x01B9D500; // sub_1F9D500
constexpr uintptr_t kSkillQuickSlotMouseXRva = 0x03122CB0; // sub_3522CB0
constexpr uintptr_t kSkillQuickSlotMouseYRva = 0x03122CD0; // sub_3522CD0
constexpr uintptr_t kSkillQuickSlotExtensionEnabledRva = 0x01BA1E50; // sub_1FA1E50
constexpr uintptr_t kSkillQuickSlotSetActorRva = 0x0225A200; // sub_265A200
constexpr uintptr_t kSkillQuickSlotGameTimeRva = 0x01E6F870; // sub_226F870
constexpr uintptr_t kSkillQuickSlotAvailableRva = 0x0226E660; // sub_266E660
constexpr uintptr_t kSkillQuickSlotSetAlphaRva = 0x0312FA60; // sub_352FA60
constexpr uintptr_t kSkillQuickSlotDrawIconRva = 0x02275520; // sub_2675520
constexpr uintptr_t kSkillQuickSlotTextBuildRva = 0x030FDF40; // sub_34FDF40
constexpr uintptr_t kSkillQuickSlotTextDrawRva = 0x030E9EB0; // sub_34E9EB0
constexpr uintptr_t kSkillQuickSlotTextRendererRva = 0x04F18C90; // dword_5318C90
constexpr uintptr_t kSkillQuickSlotDefaultAlphaRva = 0x04CC68E8; // dword_50C68E8
constexpr size_t kSkillQuickSlotNormalControlsOffset = 0x90;
constexpr size_t kSkillQuickSlotExtensionControlsOffset = 0xF0;
// The current HUD class owns a third, otherwise-unused six-entry shared
// control array at +0xC0. Its native constructor/destructor already construct
// and release every entry. Store the two appended interactive controls here;
// extending the +0xF0 array in place would overwrite the live +0x120 control.
constexpr size_t kSkillQuickSlotAddedNormalControlOffset = 0xC0;
constexpr size_t kSkillQuickSlotAddedExtensionControlOffset = 0xC8;
constexpr size_t kSkillQuickSlotNativeColumns = 6;
constexpr size_t kSkillQuickSlotTotalColumns = 7;
constexpr size_t kSkillQuickSlotControlStride = 8;
// These are the exact coordinates consumed by the native quick-slot renderer
// through sub_36AA940/sub_12DBFB0. Use the same pair for the appended-cell
// mouse rectangle so its visual and interactive areas cannot diverge.
constexpr size_t kSkillQuickSlotControlXOffset = 0x7C;
constexpr size_t kSkillQuickSlotControlYOffset = 0x80;
constexpr size_t kSkillQuickSlotControlVisibleOffset = 0x61;
constexpr int kSkillQuickSlotControlSize = 28;
constexpr size_t kSkillQuickSlotKeyTableOffset = 0x13A;
constexpr unsigned int kSkillQuickSlotInvalidKeyCode = 0x86;
constexpr unsigned short kSkillQuickSlotNormal7DefaultKey = VK_MENU;
constexpr unsigned short kSkillQuickSlotExtension7DefaultKey = VK_CONTROL;
constexpr size_t kSkillQuickSlotHudAlphaOffset = 0x73C;
constexpr size_t kSkillQuickSlotHudTextManagerOffset = 0xBD0;
constexpr int kSkillQuickSlotColumnWidth = 30;
constexpr int kSkillQuickSlotAddedControlX = 712;
constexpr int kSkillQuickSlotNormalControlY = 62;
constexpr int kSkillQuickSlotExtensionControlY = 24;
// The current HUD anchors the stock MP module over the first appended column:
// the first 30-pixel shift widens the frame, while the second clears the live
// slot-204/205 draw position. Move both native MP layers and the independently
// rendered SP value together. These are push-imm32 operands in the current
// EXE; the installer validates the complete instruction before changing it.
constexpr int kSkillQuickSlotRightHudShift =
    kSkillQuickSlotColumnWidth * 2;
constexpr uintptr_t kSkillQuickSlotMpBaseXPatchRva = 0x01B50A4F;
constexpr uintptr_t kSkillQuickSlotMpFillXPatchRva = 0x01B50B5A;
constexpr uintptr_t kSkillQuickSlotSpXPatchRva = 0x01B94BD6;
// Audited current-EXE loop limits whose old upper bound is literal 204. Only
// the low immediate byte changes (0xCC -> 0xCE); skill-tree indices 6..197
// and the established extension indices 198..203 remain unchanged.
constexpr uintptr_t kSkillExtensionOccupiedLimitByteRva = 0x0228B7E5;
constexpr uintptr_t kSkillFirstEmptyLimitByteRva = 0x0228B895;
constexpr uintptr_t kSkillFirstEmptyMemberLimitByteRva = 0x0228B8F0;
constexpr uintptr_t kSkillQuickMembershipLimitByteRva = 0x0228C3BF;
constexpr uintptr_t kSkillQuickRefreshLimitByteRva = 0x0228C688;
constexpr uintptr_t kSkillCollectAllLimitByteRva = 0x0228D317;
constexpr uintptr_t kSkillSerializeQuickLimitByteRva = 0x0233EE66;
constexpr uintptr_t kSkillSerializeTreeSkipLimitByteRva = 0x0233EEAF;
constexpr uintptr_t kSkillClassChangeLimitByteRva = 0x0089923C;
constexpr uintptr_t kCurrentActorRva = 0x0229A050;           // sub_269A050
constexpr uintptr_t kActorByObjectKeyRva = 0x02299A30;       // sub_2699A30
constexpr uintptr_t kSceneActorByIndexRva = 0x0229FF00;      // sub_269FF00
constexpr uintptr_t kActorByContextRva = 0x013B6AA0;         // sub_17B6AA0
constexpr size_t kCurrentActorCacheOffset = 0x70;             // sub_269A050 object-manager cache
constexpr uintptr_t kResolveActorInContextRva = 0x022A2000;  // sub_26A2000
constexpr uintptr_t kActorVisualDestroyRva = 0x02294460;      // sub_2694460
constexpr uintptr_t kActorVisualCreateRva = 0x02294890;       // sub_2694890
constexpr size_t kActorVisualOffset = 0x4C4;
// The current actor owns two adjacent visuals. +0x4C4 is its container/lifecycle
// visual, while +0x4C8 is the final local-character renderer that actually
// reaches sub_23B9520. Blood Frenzy's first fixed status slot supplies the
// native smooth alpha waveform; only its RGB is remapped for that exact final
// renderer, for the duration of one draw.
constexpr uintptr_t kBerserkerFrenzyActorRenderRva = 0x01FB9520;
constexpr size_t kActorRenderVisualOffset = 0x4C8;
constexpr uintptr_t kBerserkerFrenzyFixedStatusContainerRva = 0x04D82F60;
constexpr size_t kBerserkerFrenzyFixedStatusSlotStride = 8;
constexpr size_t kBerserkerFrenzyWaveformSlot = 1;
constexpr size_t kBerserkerFrenzyWaveformColorOffset = 0x2B0;
constexpr size_t kBerserkerFrenzyRenderColorOffset = 0x1F8;
constexpr size_t kBerserkerFrenzyRenderMaskOffset = 0x1FC;
constexpr unsigned int kBerserkerFrenzyAlphaMask = 0xFF000000u;
constexpr unsigned int kBerserkerFrenzyWhiteRgb = 0x00FFFFFFu;
constexpr unsigned int kBerserkerFrenzyRedRgb = 0x004646FFu;
constexpr size_t kActorPartyOwnerOffset = 0x498;
// Current-EXE Lua control entrypoints. sub_1D39E30 proves that sub_19F8DD0 is
// thiscall(motion_manager, motion_id, forced): it loads dword_51B2798 into ECX,
// pushes the two explicit arguments, and the callee returns with retn 8.
// sub_19F8DD0 itself identifies 3055..3057 as its three explicit special
// motions. sub_2002A20 is the authoritative mood packet handler and establishes
// the exact store/visual/effect sequence.
constexpr uintptr_t kMotionManagerPointerRva = 0x04DB2798;  // dword_51B2798
constexpr uintptr_t kSelfPlayMotionRva = 0x015F8DD0;          // sub_19F8DD0
constexpr uintptr_t kActorMoodStoreRva = 0x02290680;          // sub_2690680
constexpr uintptr_t kActorMoodVisualRva = 0x02294670;         // sub_2694670
constexpr uintptr_t kActorMoodEffectRva = 0x02E463C0;         // sub_32463C0
constexpr uintptr_t kActorMoodAlternateEffectRva = 0x02E460B0; // sub_32460B0
constexpr size_t kActorObjectKeyOffset = 0x0C;
constexpr size_t kActorXOffset = 0x1C;
constexpr size_t kActorYOffset = 0x20;
constexpr size_t kActorZOffset = 0x24;
// sub_22287BE can receive a null optional local-actor child during the
// current client's mode-1 bootstrap. Its callee is otherwise only a setter
// for child+0x538, so guard the null receiver and retain the exact native
// setter for every real child object.
constexpr uintptr_t kLocalActorBindingSetterRva = 0x0228FEB0;
// Current NoPack class0/op205 updates the auxiliary-profession manager. This
// module only needs the opcode so the dispatch proxy can recognise the packet;
// it never reads or writes the manager and never touches the local actor's
// sub-profession state (see the removal note in the installer area).
constexpr unsigned short kExpertJobInfoOpcode = 205;
constexpr uintptr_t kActorStateResolverRva = 0x0364C1A0; // sub_3A4C1A0
// UI registry helpers, all __thiscall on the UI manager whose pointer sits at
// VA 0x051B2764 (the client's own call sites load it with
// "MOV ECX,dword ptr [0x051B2764]"):
//   FUN_02fd4b40(manager, id)            -- does slot id hold any element
//   FUN_02fd5200(manager, id, index)     -- element `index` of slot id
//   FUN_02fd85d0(manager, id, filter, v) -- apply v to every element of slot id
// The client itself closes the expert-job window with FUN_02fd85d0(0x1a0,-1,0).
constexpr uintptr_t kUiManagerPointerRva = 0x04DB2764;
constexpr uintptr_t kUiSlotPresentRva = 0x02BD4B40; // FUN_02fd4b40
constexpr uintptr_t kUiSlotLookupRva = 0x02BD5200;  // FUN_02fd5200
constexpr uintptr_t kUiSlotNotifyRva = 0x02BD85D0;  // FUN_02fd85d0
typedef unsigned int (__thiscall* UiSlotPresentFn)(void*, unsigned int);
typedef void* (__thiscall* UiSlotLookupFn)(void*, int, unsigned int);
typedef void (__thiscall* UiSlotNotifyFn)(void*, int, int, int);
// How long to keep closing the expert-job window after a state projection: the
// client rebuilds it during the login/scene sequence, so one close is not enough.
constexpr LONG kExpertJobWindowCloseWindowMs = 5000;
// Live UI-registry census at login (log 2026-10-03 00:18:42) shows exactly one
// occupied slot: 0x1AA, whose element's vtable 0x044FFFCC is the same class family
// as the expert-job window class (vtable 0x044C1BEC) -- identical from +0x0C on,
// differing only in the class-specific overrides at the front. That is the window.
// Slot 0x1A0, which the client's op205 reader looks up, holds a NULL element, so
// the earlier attempts to close it were no-ops.
constexpr unsigned int kExpertJobWindowUiSlots[] = { 0x1AA, 0x1A0 };
constexpr uintptr_t kEncodedU32SetAlign0Rva = 0x030DB910;   // sub_34DB910
constexpr uintptr_t kEncodedU32SetAlign4Rva = 0x030DB990;   // sub_34DB990
constexpr uintptr_t kEncodedU32SetAlign8Rva = 0x030DBA10;   // sub_34DBA10
constexpr uintptr_t kEncodedU32SetAlign12Rva = 0x030DBA90;  // sub_34DBA90
constexpr uintptr_t kEncodedU32SetDefaultRva = 0x030DB7F0;  // sub_34DB7F0
// Current live packet evidence identifies dungeon entry as one typed class-0
// op28 dungeon-info packet, the first op29 start-map packet, and then the
// exact op3 local-user activation. Later room changes emit op29 without op3.
constexpr unsigned short kDungeonUserStateOpcode = 3;
constexpr unsigned short kDungeonInfoOpcode = 28;
constexpr unsigned short kDungeonStartMapOpcode = 29;
constexpr unsigned short kSelectDungeonOpcode = 16;
// Current NoPack's op29/op211 readers overwrite the two direct Hell Party
// wire flags for every room, but only propagate a non-zero fog flag into the
// two derived dungeon controllers.  A later ordinary room therefore retains
// both derived bytes at one unless compatibility explicitly mirrors the zero.
constexpr uintptr_t kDungeonStartMapHandlerRva = 0x019479A0; // sub_1D479A0 / op29
constexpr uintptr_t kBloodAltarStartMapHandlerRva = 0x01949E40; // sub_1D49E40 / op211
constexpr uintptr_t kDungeonHellPartyFogFlagRva = 0x04DB27B6; // byte_51B27B6
constexpr uintptr_t kDungeonDerivedFogStateARva = 0x04EB8F54; // byte_52B8F54
constexpr uintptr_t kDungeonDerivedFogStateBRva = 0x04D9CEFC; // byte_519CEFC
// Current NoPack sub_233A5B0 suppresses an item click before its op44 writer
// while the current actor's byte at +0x8BE is one.  Native class-0/op42 is
// the authoritative reset notification and sub_26906D0 is its exact setter.
// Some direct-route sessions reach the outer class-0 dispatcher without the
// registered op42 callback clearing the newly rebound actor, so mirror only
// that accepted server notification after native dispatch.
constexpr unsigned short kConsumableUseGateResetOpcode = 42;
constexpr uintptr_t kActorConsumableUseGateSetterRva = 0x022906D0; // sub_26906D0
constexpr size_t kActorConsumableUseGateOffset = 0x8BE;
// Current NoPack class0/op9 updates the global party slot table first, then
// refreshes HUD state 1 through sub_27310B0 only when its owner/object lookup
// resolves to the native current actor. The local bridge's established owner
// context 0 is remapped at mode-0 creation time, so a later op9 can safely
// populate the slots but miss that final pointer-equality branch. Repair only
// the op9 immediately following a successful class1/op12 SET_PARTY_INFO ACK,
// using the exact native HUD path from sub_1D64CA0.
constexpr uintptr_t kSceneHudContextRva = 0x0141E750;         // sub_181E750
// Current-EXE selector reason handling feeds the same text to the dungeon
// selector and the native chat line. These entrypoints are version-locked to
// client-patch/bin/DNF.exe and validated before every use.
constexpr uintptr_t kSelectorNoticeConstructRva = 0x015EA040; // sub_19EA040
constexpr uintptr_t kSelectorNoticeManagerRva = 0x015EB900;   // sub_19EB900
constexpr uintptr_t kSelectorNoticeEnqueueRva = 0x015EA5B0;   // sub_19EA5B0
constexpr uintptr_t kSelectorNoticeDestroyRva = 0x0059F660;   // sub_99F660
constexpr uintptr_t kChatNoticeEnqueueRva = 0x02328160;       // sub_2728160
constexpr uintptr_t kPartyHudRefreshRva = 0x023310B0;         // sub_27310B0
constexpr unsigned short kSetPartyInfoOpcode = 12;
constexpr unsigned short kPartyActorProjectionOpcode = 9;
constexpr unsigned short kPartyActorProjectionScene = 9999;
constexpr uintptr_t kLocalActorCreateRva = 0x01C036C0;       // sub_20036C0
constexpr uintptr_t kMode0OwnerCompareRva = 0x01C092A5;      // sub_2009160 scene/local channel cmp
constexpr uintptr_t kMode0OwnerRemoteRva = 0x01C092A9;
constexpr uintptr_t kMode0OwnerLocalRva = 0x01C092EB;
// sub_2008600 resolves the mode-3 actor with the same owner/context pair as
// mode 0.  The bridge's established context-0 sentinel must resolve to the
// current scene actor here as well; otherwise sub_2002FC0 returns null and the
// native mismatch path dereferences it at sub_24A0920.
constexpr uintptr_t kMode3OwnerResolveRva = 0x01C08737;
constexpr uintptr_t kMode3OwnerRemoteResumeRva = 0x01C08741;
constexpr uintptr_t kMode3OwnerLocalResumeRva = 0x01C0877A;
// sub_2008600 compares the same local-owner byte with the current scene
// channel a second time after resolving the actor.  Context 0 must take the
// local branch here too; otherwise sub_24A0920 marks the selected actor as a
// remote actor even though the first comparison resolved it locally.
constexpr uintptr_t kMode3OwnerFinalizeRva = 0x01C0884F;
constexpr uintptr_t kMode3OwnerFinalizeRemoteResumeRva = 0x01C0885A;
constexpr uintptr_t kMode3OwnerFinalizeLocalResumeRva = 0x01C08863;
// sub_1D84FE0's final current-creature record update assumes the equipped
// creature key is present in the op105 map. Imported/current-profile state can
// legitimately miss that lookup; the native handler otherwise dereferences
// null at sub_34DEA20(this=0x14) after it has already updated the actor and
// live creature objects.
constexpr uintptr_t kCreatureRenameMapNullCheckRva = 0x0198515F;
constexpr uintptr_t kCreatureRenameMapUpdateRva = 0x01985165;
constexpr uintptr_t kCreatureRenameMapDoneRva = 0x0198516A;
// sub_2602260 refreshes the equipped creature's town-side stat projection.
// Its first optional op105-map lookup is passed directly to sub_25FCAF0,
// unlike the neighboring current-EXE paths which test the lookup result first.
// During a dungeon-to-town actor rebuild that missing entry otherwise becomes
// this=0x38 and faults at sub_3508290 while reading address 0x50. Guard only
// this call site and preserve the native predicate for every real map record.
constexpr uintptr_t kTownCreatureStatMapNullCheckRva = 0x02202336;
constexpr uintptr_t kTownCreatureStatMapPredicateRva = 0x021FCAF0;
constexpr uintptr_t kTownCreatureStatMapResumeRva = 0x0220233D;
// sub_2E59620 caches the chat target actor's name handle at this+0x4C through
// the [arg1+0x38C] accessor (sub_26908C0). Clicking an offline or
// cross-channel player name reaches this call with arg1 == NULL while the
// cache is still empty; the native jne only skips the accessor once the cache
// is populated. Guard both: preserve the populated-cache skip and additionally
// treat a NULL target pointer as missing, leaving the cache empty so the
// caller keeps its existing offline/channel name fallback.
constexpr uintptr_t kChatNameCacheGuardRva = 0x02E5968E;       // cmp [esi+0x4C],0
constexpr uintptr_t kChatNameCachePresentResumeRva = 0x02E59696; // call accessor
constexpr uintptr_t kChatNameCacheMissingResumeRva = 0x02E5969E; // native jne target
// Current NoPack sub_1D73120 parses every class0/op14 raw item row into the
// complete native dynamic-state object.  Its same-template fast path then
// updates only the count/serial and a creature trade flag.  For list-7 pet
// bodies (slots 0..139) that leaves an already-created pet object holding the
// old enchant-card fields.  Slots 140..188 are pet equipment and 189..238 are
// pet consumables; invoking a pet-body virtual setter on those object types
// corrupts the op14 dispatch frame (observed with feed slot 193).
// even though raw+0x0E/+0x12 and the server repository contain the replacement
// card.  The different-template branch already applies the parsed state with
// the item's native vtable +0x158 method.  Reuse that exact call only on the
// list-7 pet-body same-template path without replacing the item pointer.
constexpr uintptr_t kPetItemUpdateDynamicStateRva = 0x019739B9;
constexpr uintptr_t kPetItemUpdateDynamicStateResumeRva = 0x019739C0;
// sub_1D73120 has already inserted a new op14 item and applied its complete
// 0x77 dynamic state before this call.  Its following sub_228FB70 acquisition
// notification crashes the current EXE for the old five-tuple booster box
// 490701728 received from mail.  Keep the inventory mutation and suppress only
// that proven unsafe notification; every other template still calls native.
constexpr uintptr_t kOp14AcquireNotificationCallRva = 0x0197415A;
constexpr uintptr_t kItemAcquireNotificationRva = 0x01E8FB70; // sub_228FB70
constexpr uint32_t kUnsafeMailAcquireNotificationItemID = 490701728;
// sub_20695F0 scans ordinary equipment first, then the optional auxiliary
// equipment container at player+0x646C for slots 3..8. The latter is absent
// during a valid dungeon-entry window, but this one call site invokes
// sub_2326860 without the null check used by the other current-EXE callers.
// An active op903 auto-repair service reaches this window and otherwise
// dereferences 0x00000044 in sub_34DBD90.
constexpr uintptr_t kAutoRepairAuxLookupRva = 0x01C6977F;
constexpr uintptr_t kAutoRepairAuxLookupResumeRva = 0x01C69786;
constexpr uintptr_t kAutoRepairAuxLoopDoneRva = 0x01C698EA;
constexpr uintptr_t kOp24SceneModeRva = 0x01E64FD0;          // sub_2264FD0
constexpr uintptr_t kOp24LoadingGateRva = 0x022C8DA0;        // sub_26C8DA0
// Current EXE clears the native 256-entry keyboard table and rebuilds the
// keyboard/IME context in its WM_CREATE input initializer.  Returning from a
// dungeon rebuilds scene UI without rerunning that initializer, so confirm
// dialogs can retain the prior scene's keyboard state.
constexpr uintptr_t kNativeInputResetRva = 0x01D23320;       // sub_2123320
constexpr uintptr_t kNativeKeyboardStateTableRva = 0x05CD0710;
constexpr size_t kNativeKeyboardStateCount = 0x100;
constexpr size_t kVirtualKeySpace = 0x20;
constexpr uintptr_t kTownEntryStateRva = 0x04D9A290;         // unk_519A290
constexpr uintptr_t kLocalOwnerServerRva = 0x04D99C4A;       // unk_5199C4A
constexpr uintptr_t kLocalOwnerChannelRva = 0x04D99C49;      // byte_5199C49
// The current actor stores its native item/equipment container at +0x646C.
// sub_249CCE0 creates this exact object with sub_759030(260) followed by
// sub_233CFD0(memory, 0, 366).  Both audited manual pickup paths refuse to call
// sub_232F810 (the native op43 writer) while this field is null.
constexpr uintptr_t kPickupContainerGetterRva = 0x020A1BC0;   // sub_24A1BC0
constexpr uintptr_t kManualPickupRva = 0x020B2840;            // sub_24B2840
constexpr uintptr_t kPickupContainerAllocatorRva = 0x00359030; // sub_759030
constexpr uintptr_t kPickupContainerConstructorRva = 0x01F3CFD0; // sub_233CFD0
constexpr size_t kPickupContainerOffset = 0x646C;
constexpr size_t kPickupContainerBytes = 260;
constexpr int kPickupContainerKind = 366;
constexpr unsigned int kDungeonTownEntryState = 3;
constexpr unsigned short kGamePortBase = 10000;
constexpr unsigned short kCrackBootstrapPort = 10019;
// Passive socket-open diagnostics.  These current-NoPack functions are never
// used to alter packets or item state: the row parser only caches exact input
// bytes, and the panel callback only logs the matching cached row.
constexpr uintptr_t kCurrentItemRowParseRva = 0x01D792A0; // sub_21792A0
constexpr uintptr_t kSocketPanelSelectRva = 0x01D12CF0;  // sub_2112CF0
constexpr uintptr_t kSocketOpenWriterRva = 0x01D12D50;  // sub_2112D50
// Current main-inventory right-click paths that serialize class1/op44.
// The fallback hooks these exact current-EXE functions and only activates for
// item ids proven by this compatibility unit's premiumlist_new.etc.
constexpr uintptr_t kInventoryUseUIARva = 0x011280C0; // sub_15280C0
constexpr uintptr_t kInventoryUseUIBRva = 0x01772FD0; // sub_1B72FD0
// These two current item-action virtual callbacks sit before the panel writer.
// sub_275A140 first asks the native 0x7D4 eligibility service and can return
// before sub_2759FD0/sub_2331CE0 is reached; sub_275A060 is its sibling route.
constexpr uintptr_t kInventoryUseGateARva = 0x0235A060; // sub_275A060
constexpr uintptr_t kInventoryUseGateBRva = 0x0235A140; // sub_275A140
// The active-coupon panel has its own native op44 writer.  Unlike the two
// generic inventory callbacks above it receives the selected template ID and
// main-bag slot directly, then applies a global UI gate before writing op44.
constexpr uintptr_t kInventoryUsePanelRva = 0x01F31CE0; // sub_2331CE0
constexpr uintptr_t kInventorySelectionManagerRva = 0x04DB2768; // dword_51B2768
constexpr uintptr_t kInventorySelectionCtorRva = 0x032EFB70; // sub_36EFB70
constexpr uintptr_t kInventorySelectionCollectRva = 0x01F31F70; // sub_2331F70
constexpr uintptr_t kInventorySelectionDtorRva = 0x030D9490; // sub_34D9490
constexpr uintptr_t kInventorySelectionLookupRva = 0x01F26860; // sub_2326860
constexpr uintptr_t kCurrentItemTemplateReadRva = 0x003CA070; // sub_7CA070
constexpr uintptr_t kCurrentItemIdentityReadRva = 0x01C64A20; // sub_2064A20
// The current main-inventory tooltip owner keeps the wrapper under the mouse
// here.  Native right-click eligibility is already true for the reproduced
// contracts, but their item class has no current-EXE op44 dispatch route.
constexpr uintptr_t kHoveredInventoryItemRva = 0x04EE1294; // dword_52E1294
constexpr uintptr_t kUpperWriterValueRva = 0x03054640; // sub_3454640
constexpr uintptr_t kUpperWriterU16Rva = 0x03054EB0; // sub_3454EB0
constexpr uintptr_t kUpperWriterU8Rva = 0x03054FC0; // sub_3454FC0
constexpr uintptr_t kUpperWriterI16Rva = 0x03054FF0; // sub_3454FF0
constexpr uintptr_t kUpperWriterU32Rva = 0x03055020; // sub_3455020
constexpr uintptr_t kUpperWriterFlushRva = 0x030566F0; // sub_34566F0
// sub_3038260 is the current "disjoint all" selection loop. It calls
// sub_2330630 once per already-validated selected equipment. The native upper
// command queue releases only one op26 for each preceding ACK, which makes the
// selected rows disappear one at a time.
constexpr uintptr_t kEquipmentDisjointBatchLoopRva = 0x02C38260; // sub_3038260
constexpr uintptr_t kEquipmentDisjointSendRva = 0x01F30630; // sub_2330630
// sub_3039340 builds the native system-disassembler candidate set. The
// audited branch at this RVA rejects equipment part 13 (title) while the
// following independent branch continues to reject reserved part 31.
constexpr uintptr_t kEquipmentDisjointTitleRejectJumpRva = 0x02C394DC;
constexpr unsigned short kUseStackableOpcode = 44;
constexpr unsigned short kEquipmentDisjointOpcode = 26;
constexpr uint32_t kEquipmentDisjointBatchMarker = 0x01363242; // "B26\x01"
constexpr unsigned int kEquipmentDisjointBatchMaxItems = 127;
constexpr uintptr_t kEquipmentDisjointSelectionBeginOffset = 0x424;
constexpr uintptr_t kEquipmentDisjointSelectionEndOffset = 0x428;
constexpr int kMainInventorySelectionGroup = 0x39;
constexpr unsigned char kMainInventoryListType = 0;
constexpr int kCurrentMainInventoryFirstItemSlot = 3;
constexpr int kCurrentMainInventoryLastItemSlot = 359;
constexpr size_t kCurrentItemIdentityOffset = 0x0E;
// sub_229ACD0 gates the two optional post-row blobs in scene op14/list3.
// It is traced only to classify the already-selected template; it does not
// alter the decision or packet reader state.
constexpr uintptr_t kSocketOp14ExtensionGateRva = 0x01E9ACD0; // sub_229ACD0
constexpr size_t kCurrentItemRawBytes = 0x77;
constexpr LONG kSocketTraceCacheRows = 2048;
constexpr LONG kSocketTraceMaxSelections = 8;
constexpr LONG kSocketTraceTemplateMatches = 8;
constexpr LONG kSocketTraceMaxWriterHits = 8;
constexpr LONG kSocketTraceMaxExtensionGateHits = 8;
// The latest confirmed current-client op914 selection is list-3 slot 9.
// Keep this exact filter narrow so the scene-reader hook is not a hot-path logger.
constexpr uint32_t kSocketTraceSelectedTemplateID = 100270515;

// Current-EXE global-minimize branches in sub_2201FE0.  Every IAT proxy checks
// the exact return address so unrelated UI and COM calls retain native behavior.
constexpr uintptr_t kToggleDesktopCoCreateReturnRva = 0x01A0227F;
constexpr uintptr_t kForeignShowWindowReturnRva = 0x01A02528;
constexpr uintptr_t kMinimizeAllSendMessageReturnRva = 0x01A02629;
// Current DNF.exe sub_384BEB0 calls CreateMutexW at RVA 0x0304BEFF,
// then compares GetLastError() with ERROR_ALREADY_EXISTS.  The return RVA
// keeps the compatibility scoped to that audited single-instance branch.
constexpr uintptr_t kMultiClientCreateMutexReturnRva = 0x0304BF05;
// Current DNF.exe sub_38AD160 calls FindWindowW at RVA 0x030AD1CA,
// focuses the existing window and returns false when it succeeds.  Returning
// no match only at this call lets the additional process create its own
// per-process registered window, matching the client's supported dual-launch
// path without changing unrelated window searches.
constexpr uintptr_t kMultiClientFindWindowReturnRva = 0x030AD1D0;

// Lua notices use an owned popup overlay instead of an unproved game UI call.
// Read-only current-EXE evidence and live screenshots rejected sub_226F9C0:
// its downstream manager hashes the string into a registered sound/resource
// table, so an arbitrary UTF-16 string is accepted without becoming visible.
// The existing DNF window procedure remains the only cross-thread boundary.
constexpr size_t kLuaNoticeMaximumCharacters = 511;
constexpr size_t kLuaNoticeQueueCapacity = 32;
constexpr unsigned char kSelectDungeonPartyFatiguePromptReason = 0xFE;
constexpr unsigned char kSelectDungeonFatiguePromptReason = 0xFF;
constexpr unsigned int kClientNoticeStylePopup = 0;
constexpr unsigned int kClientNoticeStyleDungeonSelect = 1;
constexpr wchar_t kSelectDungeonFatiguePromptText[] =
    L"疲劳值不足，无法进入地下城。";
constexpr wchar_t kSelectDungeonPartyFatiguePromptText[] =
    L"队伍内有成员疲劳值不足，无法进入地下城。";
constexpr size_t kLuaClientEventQueueCapacity = 32;
constexpr size_t kLuaGameCommandQueueCapacity = 64;
constexpr size_t kBossMutationActorCapacity = 255;
constexpr size_t kBossMutationDecisionQueueCapacity = 32;
constexpr wchar_t kLuaNoticeWindowMessageName[] =
    L"DNF90.90CNLua.ClientNotice.90cn-decode-bypass-v1";
constexpr wchar_t kLuaGameCommandWindowMessageName[] =
    L"DNF90.90CNLua.GameCommand.90cn-decode-bypass-v1";
constexpr wchar_t kLuaNoticeOverlayClassName[] =
    L"DNF90.90CNLua.NoticeOverlay.90cn-decode-bypass-v1";
constexpr wchar_t kCombatPanelWindowMessageName[] =
    L"DNF90.90CNLua.CombatPanelUpdate.90cn-decode-bypass-v1";
constexpr wchar_t kCombatPanelOverlayClassName[] =
    L"DNF90.90CNLua.CombatPanel.90cn-decode-bypass-v1";
constexpr wchar_t kDebugPanelWindowMessageName[] =
    L"DNF90.90CNLua.DebugPanelUpdate.90cn-decode-bypass-v1";
constexpr wchar_t kDebugPanelOverlayClassName[] =
    L"DNF90.90CNLua.DebugPanel.90cn-decode-bypass-v1";
// The DLL-private class0/op413 envelope is kept only for the combat-power
// sidecar snapshot. Seria-luck HUD rendering must stay owned by the native EXE
// path, so type-0 lookalikes are no longer consumed here.
constexpr unsigned short kCombatPowerAffixPrivateOpcode = 413;
constexpr unsigned int kCombatPowerAffixPrivatePacketLength = 90;
constexpr unsigned char kCombatPowerAffixPrivateType = 1;
constexpr unsigned char kCombatPowerAffixPrivateVersion = 5;
constexpr UINT_PTR kLuaNoticeOverlayTimer = 0x90C1;
constexpr UINT_PTR kCombatPanelPollTimer = 0x90C2;
constexpr UINT_PTR kDungeonPickupRepairTimer = 0x90C3;
constexpr UINT kLuaNoticeOverlayDurationMs = 4000;
constexpr UINT kCombatPanelPollIntervalMs = 100;
constexpr UINT kDungeonPickupRepairIntervalMs = 100;
constexpr int kCombatPanelWidth = 118;
constexpr int kCombatPanelSkinHeight = 386;
constexpr int kCombatPanelHeight = 442;
constexpr int kCombatRankIconSize = 80;
constexpr int kCombatRankIconX = 19;
constexpr int kCombatRankIconY = 37;
constexpr int kCombatPanelRankTooltipWidth = 230;
constexpr int kCombatPanelRankTooltipHeight = 260;
constexpr int kCombatPanelRankHoverTop = 24;
constexpr int kCombatPanelRankHoverBottom = 158;
constexpr int kCombatPanelUpgradeButtonLeft = 26;
constexpr int kCombatPanelUpgradeButtonTop = 177;
constexpr int kCombatPanelUpgradeButtonRight = 93;
constexpr int kCombatPanelUpgradeButtonBottom = 204;
constexpr int kCombatPanelCloseButtonLeft = 101;
constexpr int kCombatPanelCloseButtonTop = 1;
constexpr int kCombatPanelCloseButtonRight = 118;
constexpr int kCombatPanelCloseButtonBottom = 21;
constexpr int kCombatPanelGuideWidth = 292;
constexpr int kCombatPanelGuideHeight = 238;
constexpr int kDebugPanelWidth = 218;
constexpr int kDebugPanelHeight = 206;
struct CurrentItemRowTrace {
    LONG sequence;
    uintptr_t parserObject;
    unsigned char raw[kCurrentItemRawBytes];
};

struct LuaClientNotice {
    unsigned int length;
    unsigned int style;
    wchar_t text[kLuaNoticeMaximumCharacters + 1];
};

struct LuaClientEventContext {
    unsigned int flags;
    unsigned int dungeonID;
    unsigned int roomX;
    unsigned int roomY;
    unsigned int roomLayerFlag;
    unsigned int mapID;
    unsigned int previousRoomX;
    unsigned int previousRoomY;
    unsigned int previousRoomLayerFlag;
    unsigned int previousMapID;
};

struct BossMutationActorRow {
    unsigned short objectKey;
    unsigned int monsterID;
    unsigned char type;
};

struct BossMutationPendingRequest {
    unsigned short token;
    unsigned short objectKey;
    unsigned int monsterID;
    unsigned int roomGeneration;
};

enum LuaDungeonEntryStage : unsigned int {
    kLuaDungeonEntryIdle = 0,
    kLuaDungeonEntryAwaitStartMap = 1,
    kLuaDungeonEntryAwaitUserState = 2,
    kLuaDungeonEntryActive = 3,
};

uintptr_t g_dnfBase = 0;
uintptr_t g_checksumFn = 0;
uintptr_t g_gameAllocatorFn = 0;
uintptr_t g_gameMemcpyFn = 0;
uintptr_t g_dprotoOutgoingResume = 0;
uintptr_t g_dprotoOutgoingReturn = 0;
uintptr_t g_dprotoFalseResume = 0;
// Trade-window close notification.  The current client sends no packet at all
// when the player closes the trade window, so the server cannot learn about the
// cancel: the peer window stays open, both sides stay in the trade state, and
// items already placed in the trade container do not return to the bag.  These
// hold the current-EXE natives this DLL reuses to emit the client's own native
// trade-state packet, plus the gate-clear call the patched site replaces.
uintptr_t g_tradePacketFieldGetter = 0;
uintptr_t g_tradePacketWriteU16 = 0;
uintptr_t g_tradePacketWriteU8 = 0;
uintptr_t g_tradePacketFlush = 0;
uintptr_t g_tradeWindowGateClear = 0;
// 交易窗口对象指针与关窗上报闩锁。
//
// 窗口初始化时（交易窗口 vtable 槽位 16）捕获指针，随后由主窗口消息钩子上的
// 100ms 定时器读取 [窗口 + 0x14]（框架的显示标志：开窗=1、关窗=0，活体实测
// 唯一变化点）判定用户是否关窗。 交易管理器 +0x04 是对方角色 id，0xFFFF 为
// 未绑定哨兵，用它区分"交易进行中"与"已结束"。
uintptr_t g_tradeWindowObject = 0;
LONG g_tradeWindowCloseSeenVisible = 0;
LONG g_tradeWindowCloseReported = 0;
uintptr_t g_mode0OwnerRemoteResume = 0;
uintptr_t g_mode0OwnerLocalResume = 0;
uintptr_t g_mode3OwnerRemoteResume = 0;
uintptr_t g_mode3OwnerLocalResume = 0;
uintptr_t g_mode3OwnerFinalizeRemoteResume = 0;
uintptr_t g_mode3OwnerFinalizeLocalResume = 0;
uintptr_t g_mode3LocalOwnerChannelAddress = 0;
uintptr_t g_creatureRenameMapUpdateResume = 0;
uintptr_t g_creatureRenameMapDoneResume = 0;
uintptr_t g_townCreatureStatMapPredicate = 0;
uintptr_t g_townCreatureStatMapResume = 0;
uintptr_t g_chatNameCachePresentResume = 0;
uintptr_t g_chatNameCacheMissingResume = 0;
uintptr_t g_petItemUpdateDynamicStateResume = 0;
uintptr_t g_creatureTransportActionVectorSource = 0;
uintptr_t g_creatureTransportActionVectorResume = 0;
uintptr_t g_creatureTransportActionLookupPresentResume = 0;
uintptr_t g_creatureTransportActionLookupMissingResume = 0;
uintptr_t g_creatureTransportActionLocalDestructor = 0;
LONG g_creatureTransportActionRewriteCount = 0;
LONG g_creatureTransportActionRewriteFaultCount = 0;
LONG g_creatureTransportActionLookupMissCount = 0;
uintptr_t g_raidDirectoryOwnerPresentResume = 0;
uintptr_t g_raidDirectoryOwnerMissingResume = 0;
uintptr_t g_raidPanelNullCallResume = 0;
LONG g_raidPanelNullCallMissingCount = 0;
LONG g_raidInstanceFlagMissingCount = 0;
uintptr_t g_autoRepairAuxLookupFn = 0;
uintptr_t g_autoRepairAuxLookupResume = 0;
uintptr_t g_autoRepairAuxLoopDone = 0;
uintptr_t g_questAutoCompleteSendFn = 0;
uintptr_t g_originalCipherEncodeFn = 0;
void* g_originalSelectedPageApply = nullptr;
void* g_originalSelectorCreateTick = nullptr;
void* g_originalSelectorCreateTransition = nullptr;
void* g_originalCreateUIClick = nullptr;
void* g_originalCreateUIOpen = nullptr;
void* g_originalUpperCreateSend = nullptr;
void* g_originalClass0Dispatch = nullptr;
void* g_originalClass1Dispatch = nullptr;
void* g_originalNewbieGuideShow = nullptr;
unsigned char g_newbieGuideSeen[kNewbieGuideMaxIndex + 1] = {};
volatile LONG g_newbieGuideSeenCount = 0;
volatile LONG g_newbieGuideLastSuppressed = -1;
void* g_originalSceneUiOpen = nullptr;
void* g_originalJoustSceneBlockCheck = nullptr;
void* g_originalActivityDateEntryExists = nullptr;
void* g_originalActorByObjectKey = nullptr;
void* g_originalLocalActorCreate = nullptr;
void* g_originalLocalActorBindingSetter = nullptr;
void* g_originalPickupContainerGetter = nullptr;
void* g_originalManualPickup = nullptr;
void* g_originalDungeonStartMapHandler = nullptr;
void* g_originalBloodAltarStartMapHandler = nullptr;
void* g_originalOp24SceneMode = nullptr;
void* g_originalOp24LoadingGate = nullptr;
void* g_originalInventoryUseUIA = nullptr;
void* g_originalInventoryUseUIB = nullptr;
void* g_originalInventoryUseGateA = nullptr;
void* g_originalInventoryUseGateB = nullptr;
void* g_originalInventoryUsePanel = nullptr;
void LogLine(const char* format, ...);


// 2026-09-13 账号晶体/灵魂面板刷新修正。
// sub_1E7FFD0 刷新右侧面板时按槽位查物品，查不到就把 0 写进格子：
//   01E8012B CALL 0x2326860        ; eax = lookup(manager, slot)
//   01E80130 CMP  EAX, EBX(=0)
//   01E80132 JE   01E8013F         ; 查不到 → EDI 保持 0
//   01E80134 MOV  EDX,[EAX]        ; 查到：走虚调用取出真正要写的值
//   01E80136 MOV  ECX,EAX
//   01E80138 MOV  EAX,[EDX+0x48]
//   01E8013B CALL EAX
//   01E8013D MOV  EDI,EAX
//   01E8013F MOV  ECX,[EBP-0x20]
//   01E80142 CALL 0x1F3D6A0        ; eax = 目标格子
//   01E80147 MOV  ECX,[EBP-0x14]   ; ★ 01E8014D 消费的 ECX 装载点
//   01E8014A MOV  [EAX+0x4], EDI   ; ★ 把 0 写进格子 ⇒ 整栏被清空
//   01E8014D MOV  ECX,[ECX+0x1B8]  ; 消费 ECX
//
// 服务端无法阻止这一步（纯客户端本地行为），因此让 JE 落到 01E8014D 以跳过那次写入。
// v1 以为这只是"一个字节"的位移改动，但 01E8013F..01E8014A 是一整块 4 条指令、
// 11 字节：其中 01E80147 的 `mov ecx,[ebp-0x14]` 正是 01E8014D 需要的 ECX 来源。
// 跳过它之后 ECX 保持调用残余（实测 =1），`mov ecx,[ecx+0x1B8]` 于是在 0x1B9 上
// 读越界 → 0xC0000005 eip=0x01E8014D access_address=0x1B9，点开材料栏/账号仓库
// 立刻闪退（2026-09-13 12:03 那次崩溃同型，且早于 event-monster 守卫存在）。
// v2 保留"跳过零写入"的意图，但把 01E8014D 自身改写成先重装 ECX 再解引用，
// 于是无论从哪条路径到达它都能拿到正确的 ECX。两处改写都在整块 40 字节上下文
// 校验通过后才执行；校验不过就一个字节都不改，并写日志。
void ApplyAccountWarehousePanelKeepPatch()
{
    if (!g_dnfBase) return;
    // The entire block is verified before a single byte is written: 40 bytes, from
    // the lookup call through the instruction that consumes ECX.
    constexpr uintptr_t kBlockRva    = 0x01A8012B; // call 0x2326860
    constexpr uintptr_t kJeRelRva    = 0x01A80133; // the rel8 byte of `je 01E8013F`
    constexpr uintptr_t kDerefRva    = 0x01A8014D; // mov ecx, [ecx+0x1B8]
    constexpr size_t    kDerefLength = 6;
    constexpr uintptr_t kResumeRva   = 0x01A80153; // cmp ecx, ebx

    unsigned char* block = reinterpret_cast<unsigned char*>(g_dnfBase + kBlockRva);
    static const unsigned char kExpectedBlock[] = {
        0xE8, 0x30, 0x67, 0x4A, 0x00,      // 01E8012B call 0x2326860
        0x3B, 0xC3,                        // 01E80130 cmp  eax, ebx
        0x74, 0x0B,                        // 01E80132 je   01E8013F
        0x8B, 0x10,                        // 01E80134 mov  edx, [eax]
        0x8B, 0xC8,                        // 01E80136 mov  ecx, eax
        0x8B, 0x42, 0x48,                  // 01E80138 mov  eax, [edx+0x48]
        0xFF, 0xD0,                        // 01E8013B call eax
        0x8B, 0xF8,                        // 01E8013D mov  edi, eax
        0x8B, 0x4D, 0xE0,                  // 01E8013F mov  ecx, [ebp-0x20]
        0xE8, 0x59, 0xD5, 0x0B, 0x00,      // 01E80142 call 0x1F3D6A0
        0x8B, 0x4D, 0xEC,                  // 01E80147 mov  ecx, [ebp-0x14]
        0x89, 0x78, 0x04,                  // 01E8014A mov  [eax+4], edi
        0x8B, 0x89, 0xB8, 0x01, 0x00, 0x00 // 01E8014D mov  ecx, [ecx+0x1B8]
    };
    static_assert(sizeof(kExpectedBlock) == (kResumeRva - kBlockRva),
        "expected block must end exactly where the resume instruction begins");

    if (memcmp(block, kExpectedBlock, sizeof(kExpectedBlock)) != 0) {
        LogLine("warehouse-panel keep patch skipped: context mismatch at 0x%08X",
            reinterpret_cast<unsigned int>(block));
        return;
    }

    // 01E8014D reads [ebp-0x14] through ECX. The retargeted JE arrives there without
    // running 01E80147, so the load has to be replayed. A rel32 jump reaches the
    // cave; the cave re-runs both the reload and the dereference.
    unsigned char* cave = static_cast<unsigned char*>(VirtualAlloc(nullptr, 32,
        MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!cave) {
        LogLine("warehouse-panel keep patch failed cave allocation error=%lu", GetLastError());
        return;
    }

    size_t at = 0;
    cave[at++] = 0x8B; cave[at++] = 0x4D; cave[at++] = 0xEC; // mov ecx, [ebp-0x14]
    cave[at++] = 0x8B; cave[at++] = 0x89;                    // mov ecx, [ecx+0x1B8]
    *reinterpret_cast<int32_t*>(cave + at) = 0x1B8;
    at += 4;
    cave[at++] = 0xE9;                                       // jmp 01E80153
    *reinterpret_cast<int32_t*>(cave + at) = static_cast<int32_t>(
        static_cast<intptr_t>(g_dnfBase + kResumeRva) - reinterpret_cast<intptr_t>(cave + at + 4));
    at += 4;

    DWORD oldProtect = 0;
    if (!VirtualProtect(block, sizeof(kExpectedBlock), PAGE_EXECUTE_READWRITE, &oldProtect)) {
        LogLine("warehouse-panel keep patch failed VirtualProtect error=%lu", GetLastError());
        VirtualFree(cave, 0, MEM_RELEASE);
        return;
    }

    // 1) Skip only the zero store: `je` lands on 01E8014D instead of 01E8013F.
    block[kJeRelRva - kBlockRva] = 0x19;

    // 2) Make 01E8014D self-sufficient, so landing there without 01E80147 is safe.
    //    On the native path this is a no-op: 01E80147 sets ECX from [ebp-0x14] and
    //    nothing between it and 01E8014D writes ECX.
    unsigned char* deref = block + (kDerefRva - kBlockRva);
    deref[0] = 0xE9;
    *reinterpret_cast<int32_t*>(deref + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(cave) - reinterpret_cast<intptr_t>(deref + 5));
    for (size_t i = 5; i < kDerefLength; ++i) {
        deref[i] = 0x90;
    }

    DWORD ignored = 0;
    VirtualProtect(block, sizeof(kExpectedBlock), oldProtect, &ignored);
    FlushInstructionCache(GetCurrentProcess(), block, sizeof(kExpectedBlock));
    FlushInstructionCache(GetCurrentProcess(), cave, at);
    LogLine("warehouse-panel keep patch applied site=0x%08X je=0x19 deref=0x%08X "
        "cave=%p resume=0x%08X scope=skip-zero-store-only",
        reinterpret_cast<unsigned int>(block),
        static_cast<unsigned int>(g_dnfBase + kDerefRva), cave,
        static_cast<unsigned int>(g_dnfBase + kResumeRva));
}

// ============================================================================
// 2026-09-13  绝望之塔楼层显示修复（副本选择卡片永远显示 1 层）
// ----------------------------------------------------------------------------
// 客户端把 op348 注册给 sub_15B20A0（注册代码 `push 019B20A0` 后紧跟
// `push 0x15C`=348），把 op1263 注册给 sub_15B2570（`push 019B2570` 后紧跟
// `push 0x4EF`=1263）。两者是孪生体，偏移逐条对齐：
//
//   +0x111  call sub_1E64FD0      两个处理器调用的是同一个函数
//   +0x116  cmp  eax, 1
//   +0x119  jne  <else 分支>      只有 eax==1 才走"按层号算副本 id"的分支
//   +0x11B  movzx esi, word [ebp-0x14]     ; 报文里的层号 Floor
//   +0x121  add  esi, 0x2AFF (绝望=11007) / 0x13FA (悲鸣=5114)
//   +0x12E  jmp  公共尾（两条分支在此汇合，尾部把副本 id 提交给选择状态）
//
// 实测：悲鸣该分支执行（层号 1 -> 5115），绝望从不执行，于是卡片一直用默认值
// 显示 1 层。服务端送的是 Floor=3（报文体 00000000 0300 00），客户端收到了，
// 只是没走这条分支。
//
// 修复方式只改这一个 call 的 rel32（仍是 5 字节 call，ESP 不变），指向本模块的
// 小跳板，跳板语义等价于：
//     call sub_1E64FD0 ; mov eax, 1 ; ret
// 即保留原调用（副作用不变），再把返回值固定成 1，让后面的 `cmp eax,1` 恒成立。
// 这样绝望处理器走的就是悲鸣那条**已经被验证可用**的成功路径，其余字节一律不动。
// 先校验 16 字节指纹，不匹配就跳过并写日志（fail-closed）。
// ============================================================================
void ApplyTowerOfDespairFloorSelectionFix()
{
    if (!g_dnfBase) return;

    constexpr uintptr_t kCallRva = 0x015B21B1; // 绝望处理器 +0x111: call sub_1E64FD0
    constexpr size_t    kCallLen = 5;
    constexpr uintptr_t kDiscriminatorRva = 0x01E64FD0; // 两个处理器共用的判别函数

    unsigned char* site = reinterpret_cast<unsigned char*>(g_dnfBase + kCallRva);
    static const unsigned char kExpected[] = {
        0xE8, 0x1A, 0x2E, 0x8B, 0x00, // 015B21B1 call 01E64FD0
        0x83, 0xF8, 0x01,             // 015B21B6 cmp  eax, 1
        0x75, 0x15,                   // 015B21B9 jne  019B21D0  (else 分支)
        0x0F, 0xB7, 0x75, 0xEC,       // 015B21BB movzx esi, word [ebp-0x14]
        0x50, 0x50                    // 015B21BF push eax / push eax
    };
    static_assert(sizeof(kExpected) == 16, "fingerprint must stay 16 bytes");

    if (memcmp(site, kExpected, sizeof(kExpected)) != 0) {
        LogLine("tower-of-despair floor fix skipped: context mismatch at 0x%08X",
            reinterpret_cast<unsigned int>(site));
        return;
    }

    unsigned char* cave = static_cast<unsigned char*>(VirtualAlloc(nullptr, 32,
        MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!cave) {
        LogLine("tower-of-despair floor fix failed cave allocation error=%lu", GetLastError());
        return;
    }

    size_t at = 0;
    cave[at++] = 0xE8; // call 01E64FD0  (保留原调用的全部副作用)
    *reinterpret_cast<int32_t*>(cave + at) = static_cast<int32_t>(
        static_cast<intptr_t>(g_dnfBase + kDiscriminatorRva) -
        reinterpret_cast<intptr_t>(cave + at + 4));
    at += 4;
    cave[at++] = 0xB8; // mov eax, 1    (`cmp eax,1` 恒成立 -> 走成功分支)
    *reinterpret_cast<uint32_t*>(cave + at) = 1u;
    at += 4;
    cave[at++] = 0xC3; // ret

    DWORD oldProtect = 0;
    if (!VirtualProtect(site, kCallLen, PAGE_EXECUTE_READWRITE, &oldProtect)) {
        LogLine("tower-of-despair floor fix failed VirtualProtect error=%lu", GetLastError());
        VirtualFree(cave, 0, MEM_RELEASE);
        return;
    }

    site[0] = 0xE8;
    *reinterpret_cast<int32_t*>(site + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(cave) - reinterpret_cast<intptr_t>(site + 5));

    DWORD ignored = 0;
    VirtualProtect(site, kCallLen, oldProtect, &ignored);
    FlushInstructionCache(GetCurrentProcess(), site, kCallLen);
    FlushInstructionCache(GetCurrentProcess(), cave, at);
    LogLine("tower-of-despair floor fix applied site=0x%08X cave=%p "
        "discriminator=0x%08X scope=force-verified-grief-path-only",
        reinterpret_cast<unsigned int>(site), cave,
        static_cast<unsigned int>(g_dnfBase + kDiscriminatorRva));
}

void* g_originalEquipmentDisjointBatchLoop = nullptr;
void* g_originalEquipmentDisjointSend = nullptr;
void* g_originalSkillCatalogCollect = nullptr;
void* g_originalSkillQuickSlotDisplayMap = nullptr;
void* g_originalSkillQuickSlotPredicate = nullptr;
void* g_originalSkillQuickSlotLookup = nullptr;
void* g_originalSkillQuickSlotNormalRender = nullptr;
void* g_originalSkillQuickSlotExtensionRender = nullptr;
void* g_originalSkillQuickSlotKeyRender = nullptr;
void* g_originalSkillQuickSlotHudConstruct = nullptr;
void* g_originalSkillQuickSlotKeyCode = nullptr;
void* g_originalResolveHotkeyAction = nullptr;
void* g_originalBerserkerFrenzyActorRender = nullptr;
volatile LONG g_berserkerFrenzyRenderHitLogged = 0;
volatile LONG g_berserkerFrenzyDiagnosticDungeonActive = 0;
volatile LONG g_berserkerFrenzyCurrentBaseJob = -1;
volatile LONG g_berserkerFrenzyCurrentGrowType = -1;
void* volatile g_hotkeyActionManager = nullptr;
void* volatile g_skillQuickSlotLookupManager = nullptr;
PVOID volatile g_skillQuickSlotHud = nullptr;
volatile LONG g_skillQuickSlotLookupCapture = 0;
volatile LONG g_skillQuickSlotIconDrawMask = 0;
volatile LONG g_skillQuickSlotKeyDrawMask = 0;
volatile LONG g_skillQuickSlotDefaultKeyLogMask = 0;
volatile LONG g_skillQuickSlotDragLogMask = 0;
volatile LONG g_skillQuickSlotMouseDragSource = -1;
void* g_skillQuickSlotHotkeyResume = nullptr;
WNDPROC g_originalDnfWindowProc = nullptr;
HWND g_dnfWindow = nullptr;
PVOID volatile g_luaNoticeBridgeWindow = nullptr;
volatile LONG g_luaNoticeWindowMessage = 0;
HWND g_luaNoticeOverlayWindow = nullptr;
wchar_t g_luaNoticeOverlayText[kLuaNoticeMaximumCharacters + 1] = {};
SRWLOCK g_luaNoticeLock = SRWLOCK_INIT;
LuaClientNotice g_luaNoticeQueue[kLuaNoticeQueueCapacity] = {};
size_t g_luaNoticeHead = 0;
size_t g_luaNoticeCount = 0;
volatile LONG g_luaNoticeQueuedCount = 0;
volatile LONG g_luaNoticeDispatchCount = 0;
volatile LONG g_luaNoticeRejectedCount = 0;
volatile LONG g_luaGameCommandWindowMessage = 0;
SRWLOCK g_luaGameCommandLock = SRWLOCK_INIT;
DNF90GameCommand g_luaGameCommandQueue[kLuaGameCommandQueueCapacity] = {};
size_t g_luaGameCommandHead = 0;
size_t g_luaGameCommandCount = 0;
volatile LONG g_luaGameCommandSequence = 0;
volatile LONG g_luaGameCommandRejectedCount = 0;
SRWLOCK g_bossMutationActorLock = SRWLOCK_INIT;
BossMutationActorRow g_bossMutationActors[kBossMutationActorCapacity] = {};
size_t g_bossMutationActorCount = 0;
BossMutationPendingRequest
    g_bossMutationPending[kBossMutationDecisionQueueCapacity] = {};
size_t g_bossMutationPendingCount = 0;
unsigned int g_bossMutationRoomGeneration = 0;
SRWLOCK g_bossMutationDecisionLock = SRWLOCK_INIT;
DNF90BossMutationDecision
    g_bossMutationDecisionQueue[kBossMutationDecisionQueueCapacity] = {};
size_t g_bossMutationDecisionHead = 0;
size_t g_bossMutationDecisionCount = 0;
volatile LONG g_bossMutationDecisionGeneration = 0;
SRWLOCK g_actorSnapshotLock = SRWLOCK_INIT;
DNF90ActorSnapshot g_actorSnapshot = {};
volatile LONG g_actorSnapshotGeneration = 0;
bool g_luaGameControlEntrypointsValid = false;
SRWLOCK g_luaClientEventLock = SRWLOCK_INIT;
DNF90ClientEvent g_luaClientEventQueue[kLuaClientEventQueueCapacity] = {};
size_t g_luaClientEventHead = 0;
size_t g_luaClientEventCount = 0;
volatile LONG g_luaClientEventSequence = 0;
volatile LONG g_luaClientEventRejectedCount = 0;
volatile LONG g_luaUiReadyEventQueued = 0;
volatile LONG g_luaUiClosedEventQueued = 0;
volatile LONG g_combatPanelWindowMessage = 0;
HWND g_combatPanelOverlayWindow = nullptr;
HWND g_combatPanelRankTooltipWindow = nullptr;
HWND g_combatPanelGuideWindow = nullptr;
bool g_combatPanelPersonalInfoOpen = false;
bool g_combatPanelRankTooltipVisible = false;
bool g_combatPanelGuideVisible = false;
volatile LONG g_combatPanelCurrentCharacterID = 0;
volatile LONG g_combatPanelSuppressedCharacterID = 0;
SRWLOCK g_characterStatSnapshotLock = SRWLOCK_INIT;
DNF90CharacterStatSnapshot g_characterStatSnapshot = {};
volatile LONG g_characterStatGeneration = 0;
volatile LONG g_characterStatRejectLogCount = 0;
SRWLOCK g_equipmentSnapshotLock = SRWLOCK_INIT;
DNF90EquipmentSnapshot g_equipmentSnapshot = {};
volatile LONG g_equipmentSnapshotGeneration = 0;
volatile LONG g_equipmentSnapshotRejectLogCount = 0;
SRWLOCK g_damageAffixSnapshotLock = SRWLOCK_INIT;
DNF90DamageAffixSnapshot g_damageAffixSnapshot = {};
volatile LONG g_damageAffixSnapshotGeneration = 0;
volatile LONG g_damageAffixSnapshotUpdateCount = 0;
SRWLOCK g_combatPanelStateLock = SRWLOCK_INIT;
DNF90CombatPanelState g_combatPanelState = {
    sizeof(DNF90CombatPanelState), 0, 0,
    DNF90_COMBAT_PANEL_ENABLED, 1, 0, 0, 0,
    0, 0, 0, 0, 0, 0,
};
volatile LONG g_debugPanelWindowMessage = 0;
HWND g_debugPanelOverlayWindow = nullptr;
SRWLOCK g_debugPanelStateLock = SRWLOCK_INIT;
DNF90DebugPanelState g_debugPanelState = {
    // Keep the overlay hidden until Lua explicitly opts into the visual aid.
    // Server-side local debug behavior is independent from this UI flag.
    sizeof(DNF90DebugPanelState), 0, 0,
};
bool g_contractRButtonPending = false;
int g_contractRButtonPendingSlot = -1;
uint32_t g_contractRButtonPendingTemplateID = 0;
uint32_t g_contractRButtonPendingIdentity = 0;
LONG g_contractRButtonPendingOp44Count = 0;
volatile LONG g_cipherCallCount = 0;
volatile LONG g_clientOp44SendCount = 0;
volatile LONG g_protocolTraceEnabled = 0;
volatile LONG g_contractUseUIAHitCount = 0;
volatile LONG g_contractUseUIBHitCount = 0;
volatile LONG g_contractUseGateAHitCount = 0;
volatile LONG g_contractUseGateBHitCount = 0;
volatile LONG g_contractUsePanelHitCount = 0;
volatile LONG g_contractRButtonFallbackCount = 0;
volatile LONG g_auraSkinEntitlementState = -1;
volatile LONG g_auraSkinSilentRestoreCount = 0;
volatile LONG g_auraSkinDeferredApplyCount = 0;
volatile LONG g_partyHudRefreshPending = 0;
volatile LONG g_partyHudRefreshApplyCount = 0;
volatile LONG g_raidDirectoryMissingOwnerCount = 0;
volatile LONG g_autoRepairAuxNullCount = 0;
volatile LONG g_pickupContainerRepairCount = 0;
volatile LONG g_questAutoCompleteCallCount = 0;
volatile LONG g_socketTraceRowSequence = 0;
volatile LONG g_socketTraceSelectionCount = 0;
volatile LONG g_socketTraceWriterCount = 0;
volatile LONG g_socketTraceExtensionGateCount = 0;
volatile LONG g_selectorTraceInitialized = 0;
volatile LONG g_selectorTraceMouseDown = 0;
uintptr_t g_selectorTraceSelf = 0;
uintptr_t g_selectorTraceButton = 0;
unsigned int g_selectorTraceReady = 0;
unsigned int g_selectorTracePending = 0;
unsigned int g_selectorTraceRestriction = 0;
unsigned int g_selectorTraceButtonEnabled = 0;
unsigned int g_selectorTraceButtonStamp = 0;
unsigned int g_selectorTraceGlobalStamp = 0;
int g_selectorTraceMode = -1;
int g_selectorTraceCount = -1;
void* volatile g_currentTclsParser = nullptr;
void* volatile g_originalSendMessageW = nullptr;
void* volatile g_originalShowWindow = nullptr;
void* volatile g_originalCoCreateInstance = nullptr;
void* volatile g_originalCreateMutexW = nullptr;
void* volatile g_originalFindWindowW = nullptr;
PVOID g_exceptionTraceHandle = nullptr;
PVOID g_nullPointerCallGuardHandle = nullptr;
thread_local bool g_traceOp24 = false;
thread_local bool g_inExceptionTrace = false;
thread_local bool g_contractUseFallbackActive = false;
struct EquipmentDisjointBatchCapture {
    bool active;
    bool fallback;
    void* sender;
    int context;
    short toolSlot;
    unsigned int expectedCount;
    unsigned int count;
    short slots[kEquipmentDisjointBatchMaxItems];
};
thread_local EquipmentDisjointBatchCapture g_equipmentDisjointBatch = {};
thread_local bool g_pickupContainerRepairActive = false;
thread_local bool g_townActorContextLookupActive = false;
thread_local unsigned short g_townActorContextLookupKey = 0;
thread_local unsigned int g_op24SceneModeCalls = 0;
thread_local unsigned int g_op24LoadingGateCalls = 0;
thread_local bool g_op24SceneModeEarlyReturn = false;
thread_local bool g_op24LoadingGateEarlyReturn = false;
thread_local LuaDungeonEntryStage g_luaDungeonEntryStage =
    kLuaDungeonEntryIdle;
thread_local unsigned int g_luaDungeonID = 0;
thread_local unsigned char g_luaDungeonBossX = 0;
thread_local unsigned char g_luaDungeonBossY = 0;
thread_local bool g_luaDungeonRoomKnown = false;
thread_local unsigned char g_luaDungeonRoomX = 0;
thread_local unsigned char g_luaDungeonRoomY = 0;
thread_local unsigned char g_luaDungeonRoomLayerFlag = 0;
thread_local unsigned int g_luaDungeonMapID = 0;
thread_local ULONGLONG g_storyDigestQuestFinishTickMs = 0;
thread_local unsigned int g_storyDigestQuestFinishPacketLength = 0;
volatile LONG g_storyDigestRealtimeReady = 0;
SRWLOCK g_logWriteLock = SRWLOCK_INIT;
SRWLOCK g_socketTraceLock = SRWLOCK_INIT;
CurrentItemRowTrace g_socketTraceRows[kSocketTraceCacheRows] = {};
// Class0/op2 identifies remote town actors by an owner byte plus their stable
// uint16 object key, while op22/op23 carry the key alone. The current EXE keeps
// remotely-owned actors out of the global key table, so retain only the owner
// byte required to resolve the existing context actor for later movement and
// user interaction. The entry is cleared before native op6 leave handling.
unsigned char g_townActorOwnerByObjectKey[0x10000] = {};
unsigned char g_townActorLookupLoggedByObjectKey[0x10000] = {};
unsigned short g_currentActorObjectKey = 0;
volatile LONG g_expertJobWindowCloseUntilTick = 0;
volatile LONG g_expertJobWindowCloseCount = 0;
volatile LONG g_expertJobWindowDescribeCount = 0;

typedef bool (__thiscall* TclsParseFn)(void*, const wchar_t*);
typedef bool (__thiscall* TclsFetchLoginFn)(void*, void*, void*, void*);
typedef bool (__thiscall* TclsFetchTextFn)(void*, int, void*);
typedef bool (__thiscall* TclsFetchOneFn)(void*, void*);
typedef bool (__stdcall* TclsTailFn)(void*);
typedef void* (__thiscall* GameWideAssignFn)(void*, const wchar_t*, size_t);
typedef int (__thiscall* CodecSetKeyFn)(void*, int, int);
typedef LRESULT (WINAPI* SendMessageWFn)(HWND, UINT, WPARAM, LPARAM);
typedef BOOL (WINAPI* ShowWindowFn)(HWND, int);
typedef HANDLE (WINAPI* CreateMutexWFn)(LPSECURITY_ATTRIBUTES, BOOL, LPCWSTR);
typedef HWND (WINAPI* FindWindowWFn)(LPCWSTR, LPCWSTR);
typedef HRESULT (WINAPI* CoCreateInstanceFn)(
    const GUID&, LPUNKNOWN, DWORD, const GUID&, LPVOID*);
typedef void (__thiscall* SelectedPageApplyFn)(void*);
typedef uintptr_t (__thiscall* SelectorCreateTickFn)(void*);
typedef uintptr_t (__thiscall* SelectorCreateTransitionFn)(void*);
typedef int (__thiscall* CreateUIClickFn)(void*);
typedef unsigned char (__thiscall* CreateUIOpenFn)(void*, int*, int);
typedef int (__thiscall* UpperCreateSendFn)(void*);
typedef int (__stdcall* Class0DispatchFn)(int, int);
typedef int (__stdcall* Class1DispatchFn)(int, int, int);
typedef void* (__cdecl* StoryDigestManagerGetterFn)();
typedef unsigned char (__thiscall* StoryDigestEvaluatorFn)(void*, int);
typedef void (__thiscall* CharacterOptionRuntimeRefreshFn)(void*, const void*);
typedef void (__thiscall* CharacterOptionReminderApplyFn)(void*, const void*);
typedef void* (__thiscall* SceneUiOpenFn)(void*, uintptr_t, int, unsigned int);
typedef bool (__thiscall* SceneUiIsOpenFn)(void*, uintptr_t);
typedef bool (__thiscall* JoustSceneBlockCheckFn)(void*);
typedef bool (__thiscall* ActivityDateEntryExistsFn)(void*, int);
typedef void* (__thiscall* AuraSkinStateResolverFn)(void*);
typedef void* (__cdecl* DispatchRegistryFn)();
typedef void* (__thiscall* DispatchLookupFn)(void*, unsigned int);
typedef void* (__thiscall* CurrentActorFn)(void*);
typedef void (__thiscall* ActorConsumableUseGateSetterFn)(
    void*, unsigned char);
typedef void* (__cdecl* ActorByObjectKeyFn)(unsigned short);
typedef void* (__cdecl* ActorByContextFn)(unsigned short, int);
typedef void* (__thiscall* SceneActorByIndexFn)(void*, unsigned int);
typedef int (__thiscall* SelfPlayMotionFn)(void*, int, unsigned char);
typedef short (__thiscall* ActorMoodStoreFn)(void*, short);
typedef void* (__thiscall* ActorMoodVisualFn)(void*, void*);
typedef void (__cdecl* ActorMoodEffectFn)(void*, void*, int, unsigned char);
typedef void* (__thiscall* ResolveActorInContextFn)(
    void*, unsigned char, unsigned short);
typedef void* (__thiscall* ActorVisualDestroyFn)(void*);
typedef int (__thiscall* ActorVisualCreateFn)(void*, int);
typedef void (__thiscall* ActorRenderFn)(void*);
typedef void* (__thiscall* SceneHudContextFn)(void*);
typedef void* (__cdecl* SelectorNoticeConstructFn)(
    void*, const wchar_t*, int, unsigned int, int, int, int);
typedef void** (__cdecl* SelectorNoticeManagerFn)(void**);
typedef int (__thiscall* SelectorNoticeEnqueueFn)(void*, void*);
typedef void (__thiscall* SelectorNoticeDestroyFn)(void*);
typedef int (__thiscall* ChatNoticeEnqueueFn)(
    void*, const wchar_t*, unsigned int, int, int, int,
    unsigned char, unsigned char);
typedef void (__thiscall* PartyHudRefreshFn)(void*, int);
typedef int (__cdecl* LocalActorCreateFn)(int);
typedef void (__thiscall* LocalActorBindingSetterFn)(void*, void*);
typedef void* (__thiscall* PickupContainerGetterFn)(void*);
typedef void* (__thiscall* ManualPickupFn)(void*, void*);
typedef int (__cdecl* DungeonStartMapHandlerFn)();
typedef void* (__cdecl* PickupContainerAllocatorFn)(size_t);
typedef void* (__thiscall* PickupContainerConstructorFn)(void*, int, int);
typedef int (__thiscall* Op24SceneModeFn)(void*);
typedef unsigned char (__cdecl* Op24LoadingGateFn)();
typedef void* (__thiscall* ActorStateResolverFn)(void*);
typedef void (__thiscall* EncodedU32SetterFn)(
    unsigned int*, const unsigned int*, unsigned int*);
typedef bool (__thiscall* ChannelObjectPredicateFn)(void*);
typedef int (__thiscall* ChannelObjectIntFn)(void*);
typedef unsigned short (__thiscall* ChannelObjectPortFn)(void*);
typedef const wchar_t* (__thiscall* ChannelObjectNameFn)(void*);
typedef int (__fastcall* CurrentItemRowParseFn)(int, int, char*);
typedef char (__thiscall* SocketPanelSelectFn)(void*, int, int);
typedef char (__thiscall* SocketOpenWriterFn)(void*);
typedef bool (__cdecl* SocketOp14ExtensionGateFn)(int);
typedef int (__thiscall* InventoryUseUIAFn)(void*, int, int, int, int);
typedef void (__cdecl* InventoryUseUIBFn)();
typedef uintptr_t (__thiscall* InventoryUseGateFn)(void*, int, int, int);
typedef unsigned char (__thiscall* InventoryUsePanelFn)(void*, uint32_t, int);
typedef void (__thiscall* InventorySelectionCtorFn)(void*);
typedef void (__thiscall* InventorySelectionCollectFn)(void*, void*, int);
typedef void (__thiscall* InventorySelectionDtorFn)(void*);
typedef void* (__thiscall* InventorySelectionLookupFn)(void*, int);
typedef void* (__thiscall* CurrentItemDataFn)(void*);
typedef uint32_t (__thiscall* CurrentItemTemplateReadFn)(void*);
typedef uint32_t (__cdecl* CurrentItemIdentityReadFn)(uint32_t);
typedef char (__cdecl* ItemAcquireNotificationFn)(void*, int, int, int);
typedef void* (__cdecl* UpperWriterValueFn)();
typedef void (__thiscall* UpperWriterScalarFn)(void*, uint32_t);
typedef void (__cdecl* UpperWriterFlushFn)();
typedef void (__thiscall* EquipmentDisjointBatchLoopFn)(void*);
typedef bool (__thiscall* EquipmentDisjointSendFn)(void*, int, int);
struct NativePointerVector
{
    void** begin;
    void** end;
    void** capacity;
};
typedef void* (__thiscall* SkillCatalogCollectFn)(
    void*, NativePointerVector*, int);
typedef unsigned char (__thiscall* SkillFitnessPredicateFn)(void*, void*);
typedef int (__stdcall* SkillQuickSlotDisplayMapFn)(int);
typedef unsigned int* (__thiscall* ResolveHotkeyActionFn)(
    void*, unsigned int*, char);
typedef bool (__thiscall* HotkeyActionPressedFn)(void*);
typedef bool (__thiscall* SkillQuickSlotControlActiveFn)(void*);
typedef void (__thiscall* ReleaseSharedHandleFn)(unsigned int*);
typedef unsigned char (__stdcall* SkillQuickSlotPredicateFn)(int);
typedef void* (__thiscall* SkillQuickSlotLookupFn)(void*, int);
typedef char (__thiscall* SkillQuickSlotMoveFn)(
    void*, int, int, int, int);
typedef int (__thiscall* SkillQuickSlotRenderFn)(void*);
typedef unsigned int (__thiscall* SkillQuickSlotKeyRenderFn)(void*);
typedef void* (__thiscall* SkillQuickSlotHudConstructFn)(void*, int);
struct NativeSharedControl
{
    void* object;
    void* owner;
};
typedef NativeSharedControl* (__cdecl* SkillQuickSlotControlFactoryFn)(
    NativeSharedControl*);
typedef void (__thiscall* SkillQuickSlotControlTypeFn)(void*, int);
typedef int (__thiscall* SkillQuickSlotControlPositionFn)(void*, int, int);
typedef NativeSharedControl* (__thiscall* SkillQuickSlotHudParentFn)(
    void*, NativeSharedControl*);
typedef void (__thiscall* SkillQuickSlotAttachChildFn)(
    void*, NativeSharedControl);
typedef void (__thiscall* SkillQuickSlotReleaseSharedFn)(
    NativeSharedControl*);
typedef void* (__cdecl* SkillQuickSlotCatalogFn)();
typedef unsigned int (__thiscall* SkillQuickSlotKeyCodeFn)(void*, int);
typedef int (__cdecl* SkillQuickSlotMouseCoordinateFn)(int);
typedef short (__thiscall* SkillQuickSlotExtensionEnabledFn)(void*, int);
typedef int (__thiscall* SkillQuickSlotSetActorFn)(void*, void*);
typedef double (__cdecl* SkillQuickSlotGameTimeFn)();
typedef char (__thiscall* SkillQuickSlotAvailableFn)(void*, void*, int);
typedef int (__thiscall* SkillQuickSlotSetAlphaFn)(void*, int);
typedef void (__thiscall* SkillQuickSlotDrawIconFn)(
    void*, void*, int, int, int, int, int, int, int, int, int);
typedef void* (__thiscall* SkillQuickSlotTextBuildFn)(
    void*, unsigned int*, unsigned int);
typedef int (__thiscall* SkillQuickSlotTextDrawFn)(
    void*, int, int, unsigned int, unsigned int, int, unsigned int);
int SkillQuickSlotAtCursor();
void BeginSkillQuickSlotMouseDrag();
void CompleteSkillQuickSlotMouseDrag();
TclsParseFn g_originalTclsParse = nullptr;
TclsFetchLoginFn g_originalTclsFetchLogin = nullptr;
TclsFetchTextFn g_originalTclsFetchText = nullptr;
TclsFetchOneFn g_originalTclsFetchCrypto = nullptr;
TclsFetchOneFn g_originalTclsFetchTail = nullptr;
TclsTailFn g_originalTclsTail = nullptr;
GameWideAssignFn g_gameWideAssign = nullptr;
CodecSetKeyFn g_originalCodecSetKey0 = nullptr;
SocketOp14ExtensionGateFn g_originalSocketOp14ExtensionGate = nullptr;
CodecSetKeyFn g_originalCodecSetKey2 = nullptr;
CodecSetKeyFn g_originalCodecSetKey3 = nullptr;
CodecSetKeyFn g_originalCodecSetKey7 = nullptr;
CodecSetKeyFn g_originalCodecSetKey8 = nullptr;
CurrentItemRowParseFn g_originalCurrentItemRowParse = nullptr;
SocketPanelSelectFn g_originalSocketPanelSelect = nullptr;
SocketOpenWriterFn g_originalSocketOpenWriter = nullptr;
typedef bool (__thiscall* ChannelScriptDownloadFn)(void*);
typedef bool (__thiscall* ChannelScriptLoadFn)(void*, const char*);
typedef bool (__cdecl* ChannelDirectoryApplyFn)();
typedef void* (__thiscall* ChannelRuntimeLoadFn)(void*, const void*);
typedef void* (__thiscall* ChannelScriptLookupFn)(void*, int, int);
typedef int (__stdcall* ChannelCategoryInsertFn)(int, void*);
typedef bool (__thiscall* ChannelConnectFn)(void*, int, unsigned short);
typedef unsigned int* (__stdcall* ChannelQueryFn)(unsigned int*, int);
typedef wchar_t* (__cdecl* RegionNameResolverFn)(int);
typedef void (__thiscall* AtomicSpinFn)(void*);
typedef void (__thiscall* ObfuscatedStoreFn)(void*, const int*, int*);
ChannelScriptDownloadFn g_originalChannelScriptDownload = nullptr;
ChannelScriptLoadFn g_originalChannelScriptLoad = nullptr;
ChannelDirectoryApplyFn g_originalChannelDirectoryApply = nullptr;
ChannelRuntimeLoadFn g_originalChannelRuntimeLoad = nullptr;
ChannelCategoryInsertFn g_originalChannelCategoryInsert = nullptr;
ChannelConnectFn g_originalChannelConnect = nullptr;
ChannelQueryFn g_originalChannelQuery = nullptr;
RegionNameResolverFn g_originalRegionNameResolver = nullptr;
wchar_t g_customRegionName[64] = {};
int g_customRegionID = -1;

bool BytesMatch(const unsigned char* address, const unsigned char* expected, size_t count);
bool WriteCodePatch(uintptr_t target, const unsigned char* patch, size_t count, const char* label);
bool InstallKnownInlineHook(uintptr_t target, const unsigned char* expected,
    size_t patchLength, void* detour, void** original, const char* label);
void LogLine(const char* format, ...);
// Defined next to the trade-window hooks; polled from the main window message
// hook's 100 ms timer, which is declared much earlier in this file.
void CheckTradeWindowClosed();
// Guild compatibility consumers. They are defined with the guild installs further
// down the file, but the class0 / class1 dispatch proxies above poll them.
bool TryConsumePrivateGuildRanking(int packet);
bool TryConsumeGuildWarErrorNotice(int packet);
bool QueueDungeonSelectFatiguePrompt(unsigned char reason);
bool RejectLuaGameCommand(const char* reason);
void QueueLuaEnterTownEventIfAccepted(
    int packet,
    unsigned int sceneModeCalls,
    unsigned int loadingGateCalls,
    bool sceneModeEarlyReturn,
    bool loadingGateEarlyReturn);
void UpdateLuaDungeonEntryStateAfterDispatch(
    int packet, unsigned int opcode);
// Defined next to InstallDungeonEventMonsterConditionGuard. Polled at the top
// of every class-0 packet so a guard hit is reported without having to call
// back into the patched op312 handler itself.
void ReportDungeonEventMonsterConditionGuardHits();
// Same contract for the town-entry scene-accessor guard (see
// InstallTownEntrySceneAccessorNullGuard): polled from the class-0 dispatcher so a
// saved town entry cannot lose the signal behind an early return.
void ReportTownEntrySceneAccessorNullGuardHits();
bool IsSkillQuickSlotReadable(const void* address, size_t length);
__declspec(noinline) void LogProtocolPacket(
    const char* direction, int messageType, const void* body, int bodyLength);
__declspec(noinline) void __cdecl LogClientToServerPacket(
    int messageType, const void* body, int bodyLength);
__declspec(noinline) void __cdecl LogServerToClientPacket(
    int messageType, const void* body, int bodyLength);

__declspec(noinline) LRESULT WINAPI ProxySendMessageW(
    HWND hWnd, UINT msg, WPARAM wParam, LPARAM lParam)
{
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    if (caller == g_dnfBase + kMinimizeAllSendMessageReturnRva &&
        msg == WM_COMMAND && wParam == 0x19F && lParam == 0) {
        LogLine("[nomin] blocked audited SendMessageW branch caller_rva=0x%08X hwnd=%p",
            kMinimizeAllSendMessageReturnRva, hWnd);
        return 0;
    }

    SendMessageWFn original = reinterpret_cast<SendMessageWFn>(g_originalSendMessageW);
    if (original) return original(hWnd, msg, wParam, lParam);
    LogLine("[nomin] SendMessageW original missing caller=%p", _ReturnAddress());
    return 0;
}

__declspec(noinline) BOOL WINAPI ProxyShowWindow(HWND hWnd, int command)
{
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    if (caller == g_dnfBase + kForeignShowWindowReturnRva &&
        command == SW_MINIMIZE && hWnd) {
        DWORD ownerProcessID = 0;
        GetWindowThreadProcessId(hWnd, &ownerProcessID);
        if (ownerProcessID && ownerProcessID != GetCurrentProcessId()) {
            LogLine("[nomin] blocked audited ShowWindow branch caller_rva=0x%08X hwnd=%p pid=%lu",
                kForeignShowWindowReturnRva, hWnd, ownerProcessID);
            return TRUE;
        }
    }

    ShowWindowFn original = reinterpret_cast<ShowWindowFn>(g_originalShowWindow);
    if (original) return original(hWnd, command);
    LogLine("[nomin] ShowWindow original missing caller=%p", _ReturnAddress());
    return FALSE;
}

bool IsMultiClientLaunch()
{
    // Was DNF_MULTI_CLIENT=1 (see clientConfig at the top of this file).
    return kMultiClientLaunch;
}

__declspec(noinline) HANDLE WINAPI ProxyCreateMutexW(
    LPSECURITY_ATTRIBUTES attributes, BOOL initialOwner, LPCWSTR name)
{
    CreateMutexWFn original =
        reinterpret_cast<CreateMutexWFn>(g_originalCreateMutexW);
    if (!original) {
        LogLine("[multi] CreateMutexW original missing caller=%p", _ReturnAddress());
        SetLastError(ERROR_PROC_NOT_FOUND);
        return nullptr;
    }

    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    if (caller == g_dnfBase + kMultiClientCreateMutexReturnRva &&
        IsMultiClientLaunch() && name) {
        wchar_t uniqueName[96] = {};
        _snwprintf_s(
            uniqueName,
            _countof(uniqueName),
            _TRUNCATE,
            L"Local\\DNF90.CurrentEXE.MultiClient.%lu",
            GetCurrentProcessId());
        HANDLE result = original(attributes, initialOwner, uniqueName);
        DWORD lastError = GetLastError();
        LogLine(
            "[multi] isolated audited instance mutex caller_rva=0x%08X pid=%lu result=%p gle=%lu",
            kMultiClientCreateMutexReturnRva,
            GetCurrentProcessId(),
            result,
            lastError);
        SetLastError(lastError);
        return result;
    }

    return original(attributes, initialOwner, name);
}

__declspec(noinline) HWND WINAPI ProxyFindWindowW(
    LPCWSTR className, LPCWSTR windowName)
{
    FindWindowWFn original =
        reinterpret_cast<FindWindowWFn>(g_originalFindWindowW);
    if (!original) {
        LogLine("[multi] FindWindowW original missing caller=%p", _ReturnAddress());
        SetLastError(ERROR_PROC_NOT_FOUND);
        return nullptr;
    }

    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    if (caller == g_dnfBase + kMultiClientFindWindowReturnRva &&
        IsMultiClientLaunch()) {
        LogLine(
            "[multi] isolated audited existing-window branch caller_rva=0x%08X pid=%lu",
            kMultiClientFindWindowReturnRva,
            GetCurrentProcessId());
        SetLastError(ERROR_SUCCESS);
        return nullptr;
    }
    return original(className, windowName);
}

static const GUID kShellApplicationClsid =
    {0x13709620, 0xC279, 0x11CE, {0xA4, 0x9E, 0x44, 0x45, 0x53, 0x54, 0x00, 0x00}};
static const GUID kShellDispatch4Iid =
    {0xEFD84B2D, 0x4BCF, 0x4298, {0xBE, 0x25, 0xEB, 0x54, 0x2A, 0x59, 0xFB, 0xDA}};

__declspec(noinline) HRESULT WINAPI ProxyCoCreateInstance(
    const GUID& classID, LPUNKNOWN outer, DWORD context,
    const GUID& interfaceID, LPVOID* object)
{
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    if (caller == g_dnfBase + kToggleDesktopCoCreateReturnRva &&
        IsEqualGUID(classID, kShellApplicationClsid) &&
        IsEqualGUID(interfaceID, kShellDispatch4Iid)) {
        if (object) *object = nullptr;
        LogLine("[nomin] blocked audited Shell.ToggleDesktop branch caller_rva=0x%08X",
            kToggleDesktopCoCreateReturnRva);
        return E_FAIL;
    }

    CoCreateInstanceFn original =
        reinterpret_cast<CoCreateInstanceFn>(g_originalCoCreateInstance);
    if (original) return original(classID, outer, context, interfaceID, object);
    if (object) *object = nullptr;
    LogLine("[nomin] CoCreateInstance original missing caller=%p", _ReturnAddress());
    return E_FAIL;
}

void __fastcall ProxySelectedPageApply(void* self, void* /*unused*/)
{
    if (!self) {
        LogLine("selected page apply skipped null this rva=0x%08X", kSelectedPageApplyRva);
        return;
    }
    SelectedPageApplyFn original = reinterpret_cast<SelectedPageApplyFn>(g_originalSelectedPageApply);
    if (original) original(self);
}

struct SelectorCreateTraceState {
    uintptr_t self;
    uintptr_t button;
    unsigned int ready;
    unsigned int pending;
    unsigned int restriction;
    unsigned int buttonEnabled;
    unsigned int buttonStamp;
    unsigned int globalStamp;
    int mode;
    int count;
    bool click;
};

bool ReadSelectorCreateTraceState(void* self, SelectorCreateTraceState* state)
{
    if (!state) return false;
    memset(state, 0, sizeof(*state));
    state->self = reinterpret_cast<uintptr_t>(self);
    state->mode = -1;
    state->count = -1;
    if (!self) return false;

    __try {
        unsigned char* bytes = static_cast<unsigned char*>(self);
        unsigned char* button = *reinterpret_cast<unsigned char**>(bytes + 0xD8);
        state->button = reinterpret_cast<uintptr_t>(button);
        state->ready = bytes[0x9C];
        state->pending = bytes[0xCC];
        state->mode = *reinterpret_cast<int*>(bytes + 0x8C);
        state->restriction =
            *reinterpret_cast<unsigned char*>(g_dnfBase + kSelectorRestrictionRva);
        state->globalStamp =
            *reinterpret_cast<unsigned int*>(g_dnfBase + kUIEventStampRva);

        uintptr_t begin = *reinterpret_cast<uintptr_t*>(bytes + 0xA4);
        uintptr_t end = *reinterpret_cast<uintptr_t*>(bytes + 0xA8);
        if (end >= begin && (end - begin) % sizeof(uintptr_t) == 0 &&
            (end - begin) / sizeof(uintptr_t) <= 32) {
            state->count = static_cast<int>((end - begin) / sizeof(uintptr_t));
        }

        if (button) {
            state->buttonEnabled = button[0x61];
            state->buttonStamp = *reinterpret_cast<unsigned int*>(button + 0x10C);
            state->click =
                state->buttonEnabled != 0 && state->buttonStamp == state->globalStamp;
        }
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("selector-create-state read exception self=%p code=0x%08X",
            self, GetExceptionCode());
        return false;
    }
}

uintptr_t __fastcall ProxySelectorCreateTick(void* self, void* /*unused*/)
{
    SelectorCreateTraceState before = {};
    bool stateReady = ReadSelectorCreateTraceState(self, &before);
    LONG mouseDown = (GetAsyncKeyState(VK_LBUTTON) & 0x8000) != 0 ? 1 : 0;
    LONG previousMouseDown = InterlockedExchange(&g_selectorTraceMouseDown, mouseDown);
    bool mousePressed = mouseDown != 0 && previousMouseDown == 0;
    bool changed = stateReady &&
        (InterlockedCompareExchange(&g_selectorTraceInitialized, 1, 0) == 0 ||
         before.self != g_selectorTraceSelf ||
         before.button != g_selectorTraceButton ||
         before.ready != g_selectorTraceReady ||
         before.pending != g_selectorTracePending ||
         before.restriction != g_selectorTraceRestriction ||
         before.buttonEnabled != g_selectorTraceButtonEnabled ||
         before.buttonStamp != g_selectorTraceButtonStamp ||
         before.globalStamp != g_selectorTraceGlobalStamp ||
         before.mode != g_selectorTraceMode ||
         before.count != g_selectorTraceCount);

    if (changed || before.click || mousePressed) {
        LogLine("selector-create-gate phase=before self=%p button=%p ready=%u "
            "pending=%u restriction=%u button_enabled=%u button_stamp=%u "
            "global_stamp=%u click=%u mouse_pressed=%u mode=%d count=%d",
            self, reinterpret_cast<void*>(before.button), before.ready, before.pending,
            before.restriction, before.buttonEnabled, before.buttonStamp,
            before.globalStamp, before.click ? 1u : 0u, mousePressed ? 1u : 0u,
            before.mode, before.count);
    }

    if (stateReady) {
        g_selectorTraceSelf = before.self;
        g_selectorTraceButton = before.button;
        g_selectorTraceReady = before.ready;
        g_selectorTracePending = before.pending;
        g_selectorTraceRestriction = before.restriction;
        g_selectorTraceButtonEnabled = before.buttonEnabled;
        g_selectorTraceButtonStamp = before.buttonStamp;
        g_selectorTraceGlobalStamp = before.globalStamp;
        g_selectorTraceMode = before.mode;
        g_selectorTraceCount = before.count;
    }

    SelectorCreateTickFn original =
        reinterpret_cast<SelectorCreateTickFn>(g_originalSelectorCreateTick);
    uintptr_t result = original ? original(self) : 0;

    SelectorCreateTraceState after = {};
    if ((before.click || mousePressed) &&
        ReadSelectorCreateTraceState(self, &after)) {
        LogLine("selector-create-gate phase=after self=%p ready=%u pending=%u "
            "restriction=%u mode=%d count=%d result=0x%08X",
            self, after.ready, after.pending, after.restriction, after.mode,
            after.count, static_cast<unsigned int>(result));
    }
    return result;
}

uintptr_t __fastcall ProxySelectorCreateTransition(void* self, void* /*unused*/)
{
    SelectorCreateTraceState before = {};
    ReadSelectorCreateTraceState(self, &before);
    unsigned int autoGuard = 0;
    __try {
        if (self) {
            autoGuard = static_cast<unsigned char*>(self)[0xBD4];
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        autoGuard = 0xFFFFFFFFu;
    }
    LogLine("selector-create-transition phase=enter self=%p ready=%u pending=%u "
        "restriction=%u mode=%d count=%d auto_guard=%u",
        self, before.ready, before.pending, before.restriction, before.mode,
        before.count, autoGuard);

    SelectorCreateTransitionFn original =
        reinterpret_cast<SelectorCreateTransitionFn>(g_originalSelectorCreateTransition);
    uintptr_t result = original ? original(self) : 0;

    SelectorCreateTraceState after = {};
    ReadSelectorCreateTraceState(self, &after);
    LogLine("selector-create-transition phase=leave self=%p mode_before=%d "
        "mode_after=%d count=%d result=0x%08X",
        self, before.mode, after.mode, after.count, static_cast<unsigned int>(result));
    return result;
}

void ReadCreateUIState(
    void* self, void** button, int* mode, int* group,
    unsigned int* classReady, unsigned int* pending)
{
    if (button) *button = nullptr;
    if (mode) *mode = -1;
    if (group) *group = -1;
    if (classReady) *classReady = 0;
    if (pending) *pending = 0;
    if (!self) return;

    __try {
        unsigned char* bytes = static_cast<unsigned char*>(self);
        if (button) *button = *reinterpret_cast<void**>(bytes + 0x17C);
        if (group) *group = *reinterpret_cast<int*>(bytes + 0x1C4);
        if (classReady) *classReady = bytes[0x1C8];
        if (mode) *mode = *reinterpret_cast<int*>(bytes + 0x1CC);
        if (pending) *pending = bytes[0x301];
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("create-ui-state read exception self=%p code=0x%08X",
            self, GetExceptionCode());
    }
}

int __fastcall ProxyCreateUIClick(void* self, void* /*unused*/)
{
    void* button = nullptr;
    int mode = -1;
    int group = -1;
    unsigned int classReady = 0;
    unsigned int pending = 0;
    ReadCreateUIState(self, &button, &mode, &group, &classReady, &pending);
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    LogLine("create-ui-click phase=enter self=%p button=%p mode=%d group=%d "
        "class_ready=%u pending=%u caller=0x%08X caller_rva=0x%08X",
        self, button, mode, group, classReady, pending, caller,
        caller >= g_dnfBase ? caller - g_dnfBase : 0);

    CreateUIClickFn original = reinterpret_cast<CreateUIClickFn>(g_originalCreateUIClick);
    int result = original ? original(self) : 0;
    ReadCreateUIState(self, nullptr, &mode, nullptr, nullptr, &pending);
    LogLine("create-ui-click phase=leave self=%p result=0x%08X mode=%d pending=%u",
        self, result, mode, pending);
    return result;
}

unsigned char __fastcall ProxyCreateUIOpen(
    void* self, void* /*unused*/, int* requestedMode, int group)
{
    int requested = -1;
    __try {
        if (requestedMode) requested = *requestedMode;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        requested = -2;
    }
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    LogLine("create-ui-open phase=enter self=%p mode_ptr=%p requested_mode=%d "
        "group=%d caller=0x%08X caller_rva=0x%08X",
        self, requestedMode, requested, group, caller,
        caller >= g_dnfBase ? caller - g_dnfBase : 0);

    CreateUIOpenFn original = reinterpret_cast<CreateUIOpenFn>(g_originalCreateUIOpen);
    unsigned char result = original ? original(self, requestedMode, group) : 0;
    int mode = -1;
    int currentGroup = -1;
    ReadCreateUIState(self, nullptr, &mode, &currentGroup, nullptr, nullptr);
    LogLine("create-ui-open phase=leave self=%p result=%u mode=%d group=%d",
        self, static_cast<unsigned int>(result), mode, currentGroup);
    return result;
}

int __fastcall ProxyUpperCreateSend(void* self, void* /*unused*/)
{
    int mode = -1;
    int group = -1;
    unsigned int job = 0;
    unsigned int pending = 0;
    ReadCreateUIState(self, nullptr, &mode, &group, &job, &pending);
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    LogLine("upper-create-send phase=enter self=%p job=%u mode=%d group=%d "
        "pending=%u caller=0x%08X caller_rva=0x%08X",
        self, job, mode, group, pending, caller,
        caller >= g_dnfBase ? caller - g_dnfBase : 0);

    UpperCreateSendFn original =
        reinterpret_cast<UpperCreateSendFn>(g_originalUpperCreateSend);
    int result = original ? original(self) : 0;
    ReadCreateUIState(self, nullptr, nullptr, nullptr, nullptr, &pending);
    LogLine("upper-create-send phase=leave self=%p result=0x%08X pending=%u",
        self, result, pending);
    return result;
}

void LogLine(const char* format, ...)
{
    wchar_t exePath[MAX_PATH] = { 0 };
    GetModuleFileNameW(nullptr, exePath, MAX_PATH);
    wchar_t* slash = wcsrchr(exePath, L'\\');
    if (slash) slash[1] = L'\0';

    wchar_t logPath[MAX_PATH] = { 0 };
    _snwprintf(logPath, MAX_PATH - 1, L"%s90CN_trace.log", exePath);

    char line[2048] = { 0 };
    SYSTEMTIME now;
    GetLocalTime(&now);
    int offset = _snprintf(line, sizeof(line) - 2,
        "[%04d-%02d-%02d %02d:%02d:%02d.%03d] [TRACE] [pid=%u tid=%u] ",
        now.wYear, now.wMonth, now.wDay, now.wHour, now.wMinute, now.wSecond,
        now.wMilliseconds, GetCurrentProcessId(), GetCurrentThreadId());
    if (offset < 0) offset = 0;

    va_list args;
    va_start(args, format);
    int wrote = _vsnprintf(line + offset, sizeof(line) - 2 - offset, format, args);
    va_end(args);
    int length = wrote >= 0 ? offset + wrote : static_cast<int>(sizeof(line) - 2);
    line[length++] = '\n';

    AcquireSRWLockExclusive(&g_logWriteLock);
    HANDLE file = CreateFileW(logPath, FILE_APPEND_DATA, FILE_SHARE_READ | FILE_SHARE_WRITE,
        nullptr, OPEN_ALWAYS, FILE_ATTRIBUTE_NORMAL, nullptr);
    if (file != INVALID_HANDLE_VALUE) {
        DWORD ignored = 0;
        WriteFile(file, line, static_cast<DWORD>(length), &ignored, nullptr);
        CloseHandle(file);
    }
    ReleaseSRWLockExclusive(&g_logWriteLock);
}

__declspec(noinline) void LogProtocolPacket(
    const char* direction, int messageType, const void* body, int bodyLength)
{
    if (InterlockedCompareExchange(&g_protocolTraceEnabled, 0, 0) == 0) {
        return;
    }

    wchar_t exePath[MAX_PATH] = { 0 };
    GetModuleFileNameW(nullptr, exePath, MAX_PATH);
    wchar_t* slash = wcsrchr(exePath, L'\\');
    if (slash) slash[1] = L'\0';

    wchar_t logPath[MAX_PATH] = { 0 };
    _snwprintf(logPath, MAX_PATH - 1, L"%s90CN_protocol.log", exePath);

    SYSTEMTIME now;
    GetLocalTime(&now);
    char header[384] = { 0 };
    int headerLength = _snprintf(header, sizeof(header) - 1,
        "[%04d-%02d-%02d %02d:%02d:%02d.%03d] [PROTO] [packet] [pid=%u tid=%u] "
        "direction=%s msg=%d body_len=%d body=",
        now.wYear, now.wMonth, now.wDay, now.wHour, now.wMinute, now.wSecond,
        now.wMilliseconds, GetCurrentProcessId(), GetCurrentThreadId(),
        direction ? direction : "unknown", messageType, bodyLength);
    if (headerLength < 0) return;

    // Packet logging is diagnostic and must never turn an invalid native
    // pointer into a client crash.  Current create/roster packets are small;
    // cap large scene payloads while retaining their true body length.
    constexpr int kProtocolBodyLogLimit = 4096;
    int loggedLength = bodyLength;
    if (loggedLength < 0) loggedLength = 0;
    if (loggedLength > kProtocolBodyLogLimit) loggedLength = kProtocolBodyLogLimit;

    AcquireSRWLockExclusive(&g_logWriteLock);
    HANDLE file = CreateFileW(logPath, FILE_APPEND_DATA, FILE_SHARE_READ | FILE_SHARE_WRITE,
        nullptr, OPEN_ALWAYS, FILE_ATTRIBUTE_NORMAL, nullptr);
    if (file == INVALID_HANDLE_VALUE) {
        ReleaseSRWLockExclusive(&g_logWriteLock);
        return;
    }

    DWORD ignored = 0;
    WriteFile(file, header, static_cast<DWORD>(headerLength), &ignored, nullptr);

    bool readFailed = false;
    const unsigned char* bytes = static_cast<const unsigned char*>(body);
    static const char kHex[] = "0123456789ABCDEF";
    for (int base = 0; base < loggedLength && !readFailed; base += 64) {
        int count = loggedLength - base;
        if (count > 64) count = 64;
        char hex[64 * 3] = { 0 };
        int used = 0;
        __try {
            for (int i = 0; i < count; ++i) {
                unsigned char value = bytes[base + i];
                if (base != 0 || i != 0) hex[used++] = ' ';
                hex[used++] = kHex[value >> 4];
                hex[used++] = kHex[value & 0x0F];
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            readFailed = true;
        }
        if (!readFailed && used > 0) {
            WriteFile(file, hex, static_cast<DWORD>(used), &ignored, nullptr);
        }
    }

    if (readFailed) {
        static const char kReadFailure[] = " <body-read-exception>";
        WriteFile(file, kReadFailure, sizeof(kReadFailure) - 1, &ignored, nullptr);
    } else if (bodyLength > loggedLength) {
        char suffix[96] = { 0 };
        int suffixLength = _snprintf(suffix, sizeof(suffix) - 1,
            " ... <truncated logged=%d total=%d>", loggedLength, bodyLength);
        if (suffixLength > 0) {
            WriteFile(file, suffix, static_cast<DWORD>(suffixLength), &ignored, nullptr);
        }
    }
    static const char kNewline[] = "\r\n";
    WriteFile(file, kNewline, sizeof(kNewline) - 1, &ignored, nullptr);
    CloseHandle(file);
    ReleaseSRWLockExclusive(&g_logWriteLock);
}

__declspec(noinline) void __cdecl LogClientToServerPacket(
    int messageType, const void* body, int bodyLength)
{
    // The premium-contract fallback must distinguish a native op44 emitted
    // during the same right-click cycle from a missing native dispatch. Keep
    // this counter independent from optional packet-body tracing.
    if (messageType == kUseStackableOpcode) {
        InterlockedIncrement(&g_clientOp44SendCount);
    }
    LogProtocolPacket("C2S", messageType, body, bodyLength);
}

bool TryGetDungeonSelectFatiguePromptReason(
    int messageType, const void* body, int bodyLength,
    unsigned char* reason)
{
    if (messageType != kSelectDungeonOpcode || !body || bodyLength != 3 ||
        !reason) {
        return false;
    }
    __try {
        const unsigned char* bytes =
            static_cast<const unsigned char*>(body);
        if (bytes[0] != 0 || bytes[2] != 0 ||
            (bytes[1] != kSelectDungeonFatiguePromptReason &&
             bytes[1] != kSelectDungeonPartyFatiguePromptReason)) {
            return false;
        }
        *reason = bytes[1];
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("dungeon-select fatigue prompt decode match exception code=0x%08X",
            GetExceptionCode());
        return false;
    }
}

__declspec(noinline) void __cdecl LogServerToClientPacket(
    int messageType, const void* body, int bodyLength)
{
    LogProtocolPacket("S2C", messageType, body, bodyLength);
    unsigned char reason = 0;
    if (TryGetDungeonSelectFatiguePromptReason(
            messageType, body, bodyLength, &reason)) {
        QueueDungeonSelectFatiguePrompt(reason);
    }
}

void LogCrashAddressOwner(const char* tag, ULONG_PTR address)
{
    HMODULE module = nullptr;
    if (GetModuleHandleExA(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
            GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
            reinterpret_cast<LPCSTR>(address), &module)) {
        char modulePath[MAX_PATH] = { 0 };
        DWORD nameLen = GetModuleFileNameA(module, modulePath, MAX_PATH);
        const char* shortName = modulePath;
        if (nameLen) {
            const char* slash = strrchr(modulePath, '\\');
            if (slash) shortName = slash + 1;
        }
        LogLine("crash %s addr=0x%08lX module='%s' base=0x%08lX offset=0x%08lX",
            tag, static_cast<unsigned long>(address),
            shortName[0] ? shortName : "?",
            static_cast<unsigned long>(reinterpret_cast<ULONG_PTR>(module)),
            static_cast<unsigned long>(address - reinterpret_cast<ULONG_PTR>(module)));
    } else {
        LogLine("crash %s addr=0x%08lX module=?", tag,
            static_cast<unsigned long>(address));
    }
}

LONG WINAPI TraceUnhandledClientException(PEXCEPTION_POINTERS exceptionInfo)
{
    if (!exceptionInfo || !exceptionInfo->ExceptionRecord || !exceptionInfo->ContextRecord ||
        g_inExceptionTrace) {
        return EXCEPTION_CONTINUE_SEARCH;
    }

    DWORD code = exceptionInfo->ExceptionRecord->ExceptionCode;
    switch (code) {
    case EXCEPTION_ACCESS_VIOLATION:
    case EXCEPTION_ARRAY_BOUNDS_EXCEEDED:
    case EXCEPTION_ILLEGAL_INSTRUCTION:
    case EXCEPTION_INT_DIVIDE_BY_ZERO:
    case EXCEPTION_PRIV_INSTRUCTION:
    case EXCEPTION_STACK_OVERFLOW:
        break;
    default:
        return EXCEPTION_CONTINUE_SEARCH;
    }

    g_inExceptionTrace = true;
    CONTEXT* context = exceptionInfo->ContextRecord;
    ULONG_PTR accessKind = 0;
    ULONG_PTR accessAddress = 0;
    if (code == EXCEPTION_ACCESS_VIOLATION &&
        exceptionInfo->ExceptionRecord->NumberParameters >= 2) {
        accessKind = exceptionInfo->ExceptionRecord->ExceptionInformation[0];
        accessAddress = exceptionInfo->ExceptionRecord->ExceptionInformation[1];
    }
    LogLine("exception code=0x%08X address=%p access_kind=%lu access_address=0x%08lX "
        "eip=0x%08lX esp=0x%08lX ebp=0x%08lX eax=0x%08lX ebx=0x%08lX "
        "ecx=0x%08lX edx=0x%08lX esi=0x%08lX edi=0x%08lX",
        code, exceptionInfo->ExceptionRecord->ExceptionAddress,
        static_cast<unsigned long>(accessKind), static_cast<unsigned long>(accessAddress),
        context->Eip, context->Esp, context->Ebp, context->Eax, context->Ebx,
        context->Ecx, context->Edx, context->Esi, context->Edi);
    LogCrashAddressOwner("eip", context->Eip);

    __try {
        const DWORD* stack = reinterpret_cast<const DWORD*>(context->Esp);
        LogLine("exception stack=%08lX %08lX %08lX %08lX %08lX %08lX %08lX %08lX "
            "%08lX %08lX %08lX %08lX %08lX %08lX %08lX %08lX",
            stack[0], stack[1], stack[2], stack[3], stack[4], stack[5], stack[6], stack[7],
            stack[8], stack[9], stack[10], stack[11], stack[12], stack[13], stack[14], stack[15]);
        LogCrashAddressOwner("ret0", stack[0]);
        LogCrashAddressOwner("ret9", stack[9]);
        LogCrashAddressOwner("ret10", stack[10]);
        LogCrashAddressOwner("ret12", stack[12]);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("exception stack unreadable esp=0x%08lX", context->Esp);
    }
    g_inExceptionTrace = false;
    return EXCEPTION_CONTINUE_SEARCH;
}

// Generic guard for "call through a null pointer".
//
// After entering a hatchery the raid client walks a partially materialized
// manager/object graph and calls virtual slots that are still null. Measured
// signature, three times on 2026-09-17 (18:31:39.721, 19:05:52.377, and the
// 0x025BD2F4 site eight times just before it): eip == 0, access_kind == 8,
// access_address == 0, and `exception stack[0]` = the return address of the
// `call reg` that faulted, inside DNF.exe.
//
// A `call reg` pushes its return address before transferring control, so at the
// fault the stack already holds exactly that slot. Returning to it and dropping
// the slot skips the call; a thiscall with no stack argument (the common shape
// here: `mov ecx,[...]; mov eax,[ecx]; mov edx,[eax+slot]; call edx`) then leaves
// the frame perfectly balanced. The one measured site that pushes a stack
// argument first (0x025BD2F4, `push esi`) has its own guard and is listed here so
// the generic path never has to guess.
LONG g_nullPointerCallSkipCount = 0;
LONG g_nullPointerCallForeignCount = 0;

LONG WINAPI GuardNullPointerCall(PEXCEPTION_POINTERS exceptionInfo)
{
    if (!exceptionInfo || !exceptionInfo->ExceptionRecord ||
        !exceptionInfo->ContextRecord || !g_dnfBase) {
        return EXCEPTION_CONTINUE_SEARCH;
    }
    if (exceptionInfo->ExceptionRecord->ExceptionCode != EXCEPTION_ACCESS_VIOLATION) {
        return EXCEPTION_CONTINUE_SEARCH;
    }
    CONTEXT* context = exceptionInfo->ContextRecord;
    if (context->Eip >= 0x1000) {
        return EXCEPTION_CONTINUE_SEARCH;
    }

    ULONG_PTR returnAddress = 0;
    __try {
        returnAddress = *reinterpret_cast<ULONG_PTR*>(context->Esp);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return EXCEPTION_CONTINUE_SEARCH;
    }
    if (returnAddress < g_dnfBase || returnAddress >= g_dnfBase + kClientImageSpan) {
        LONG hit = InterlockedIncrement(&g_nullPointerCallForeignCount);
        if (hit <= 8) {
            LogLine("null pointer call outside the client left alone ret=0x%08lX esp=0x%08lX hit=%ld",
                static_cast<unsigned long>(returnAddress),
                static_cast<unsigned long>(context->Esp), hit);
        }
        return EXCEPTION_CONTINUE_SEARCH;
    }

    // The site guard at 0x021BD2F4 replays `push esi` before its call, so the
    // frame owes those four bytes as well. Every other measured site is a bare
    // `call edx`.
    const ULONG_PTR stackDelta =
        returnAddress == g_dnfBase + kRaidPanelNullCallResumeRva ? 8 : 4;
    const ULONG_PTR contextBefore = context->Ecx;
    const ULONG_PTR accumulatorBefore = context->Eax;
    const ULONG_PTR slotBefore = context->Edx;
    context->Eip = static_cast<DWORD>(returnAddress);
    context->Esp += static_cast<DWORD>(stackDelta);
    LONG hit = InterlockedIncrement(&g_nullPointerCallSkipCount);
    if (hit <= 32) {
        LogLine("null pointer call skipped ret=0x%08lX esp_delta=%lu ecx=0x%08lX "
            "eax=0x%08lX edx=0x%08lX hit=%ld",
            static_cast<unsigned long>(returnAddress),
            static_cast<unsigned long>(stackDelta),
            static_cast<unsigned long>(contextBefore),
            static_cast<unsigned long>(accumulatorBefore),
            static_cast<unsigned long>(slotBefore), hit);
    }
    return EXCEPTION_CONTINUE_EXECUTION;
}

void FormatHexBytes(const void* data, int length, char* output, size_t outputSize)
{
    if (!output || outputSize == 0) return;
    output[0] = '\0';
    if (!data || length <= 0) return;

    const unsigned char* bytes = static_cast<const unsigned char*>(data);
    size_t used = 0;
    int cappedLength = length > 96 ? 96 : length;
    static const char kHex[] = "0123456789ABCDEF";

    __try {
        for (int i = 0; i < cappedLength && used + 3 < outputSize; ++i) {
            unsigned char value = bytes[i];
            output[used++] = kHex[value >> 4];
            output[used++] = kHex[value & 0x0F];
            if (i + 1 < cappedLength && used + 1 < outputSize) output[used++] = ' ';
        }
        output[used] = '\0';
        if (length > cappedLength && used + 4 < outputSize) {
            output[used++] = ' ';
            output[used++] = '.';
            output[used++] = '.';
            output[used++] = '.';
            output[used] = '\0';
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        _snprintf(output, outputSize - 1, "<read-exception-0x%08X>", GetExceptionCode());
        output[outputSize - 1] = '\0';
    }
}

bool IsObservedDispatchOpcode(unsigned int classID, unsigned int opcode)
{
    // The current EXE uses these handlers around the Seria-room return-village
    // flow. Keep the capture read-only so we can compare a native successful
    // portal initialization with the character-list Seria login route.
    if (opcode == 22 || opcode == 23 || opcode == 356 || opcode == 467 ||
        opcode == 469 || opcode == 470 || opcode == 494 || opcode == 512 ||
        opcode == 1425) {
        return true;
    }
    if (classID == 1) {
        return opcode == 4 || opcode == 7 || opcode == 10 || opcode == 11 ||
            opcode == 19 || opcode == 85 || opcode == 254 ||
            opcode == 81 || opcode == 1021 ||
            // 2026-09-13: 宝珠附魔(272) 与晶体契约状态(898) 的抓取开关。
            // 现场表现为"宝珠附魔成功后账号晶体/灵魂面板立刻被清空、小退恢复"，
            // 服务端确认没有下发 354..365 的删除行，因此需要看客户端处理 272 时
            // 的容器访问，据此定位是否走了"按列表索引取物品"的同一套逻辑。
            opcode == 272 || opcode == 898;
    }
    // class-0 交易族：0x0E 列表同步 / 0x0F 物品变更 / 0x10 取消 / 0x11 状态 /
    // 0x12 完成 / 0x13 移入物品。加进来的目的是拿到客户端【交易完成】处理器的
    // 地址（日志里的 callback=+0x...），据此反汇编出交易结果弹窗
    // （交易金额 / 手续费 / 获得金额）在 op18 包体里的精确字段布局。
    if (opcode == 14 || opcode == 15 || opcode == 16 || opcode == 17 ||
        opcode == 18 || opcode == 19) {
        return true;
    }
    return opcode == 1 || opcode == 2 || opcode == 5 || opcode == 9 ||
        opcode == 24 || opcode == 108 || opcode == 120 || opcode == 124 ||
        opcode == 376 || opcode == 689 || opcode == 693 || opcode == 754 ||
        opcode == 631 || opcode == 800 || opcode == 896 || opcode == 897 || opcode == 1027 ||
        opcode == 1206;
}

void TraceDispatchRegistryEntry(unsigned int classID, unsigned int opcode)
{
    void* registry = nullptr;
    void* record = nullptr;
    uintptr_t callback = 0;
    uintptr_t context = 0;
    __try {
        DispatchRegistryFn registryFn = reinterpret_cast<DispatchRegistryFn>(
            g_dnfBase + (classID == 0 ? kClass0RegistryRva : kClass1RegistryRva));
        DispatchLookupFn lookupFn = reinterpret_cast<DispatchLookupFn>(
            g_dnfBase + kDispatchLookupRva);
        registry = registryFn ? registryFn() : nullptr;
        record = registry && lookupFn ? lookupFn(registry, opcode) : nullptr;
        if (record) {
            callback = *reinterpret_cast<uintptr_t*>(record);
            context = *(reinterpret_cast<uintptr_t*>(record) + 1);
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("dispatch-registry read exception class=%u opcode=%u code=0x%08X",
            classID, opcode, GetExceptionCode());
        return;
    }
    LogLine("dispatch-registry class=%u opcode=%u registry=%p record=%p callback=+0x%08X context=%p",
        classID, opcode, registry, record,
        callback ? static_cast<unsigned int>(callback - g_dnfBase) : 0,
        reinterpret_cast<void*>(context));
}

bool SendCurrentCommandResponseCapabilities()
{
    if (!g_dnfBase) return false;

    unsigned char bitmap[kCommandCapabilityBitmapLength] = { 0 };
    unsigned int handlerCount = 0;
    unsigned int fingerprint = 2166136261u;
    __try {
        DispatchRegistryFn registryFn = reinterpret_cast<DispatchRegistryFn>(
            g_dnfBase + kClass1RegistryRva);
        DispatchLookupFn lookupFn = reinterpret_cast<DispatchLookupFn>(
            g_dnfBase + kDispatchLookupRva);
        void* registry = registryFn ? registryFn() : nullptr;
        if (!registry || !lookupFn) {
            LogLine("command-capability scan deferred registry=%p lookup=%p",
                registry, lookupFn);
            return false;
        }

        for (unsigned int opcode = 0; opcode < kCommandEnumEnd; ++opcode) {
            void* record = lookupFn(registry, opcode);
            if (!record) continue;
            bitmap[opcode >> 3] |= static_cast<unsigned char>(1u << (opcode & 7));
            ++handlerCount;
        }
        if (handlerCount == 0) {
            LogLine("command-capability scan deferred registry=%p handlers=0",
                registry);
            return false;
        }

        UpperWriterValueFn value = reinterpret_cast<UpperWriterValueFn>(
            g_dnfBase + kUpperWriterValueRva);
        UpperWriterScalarFn writeU16 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU16Rva);
        UpperWriterScalarFn writeU8 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU8Rva);
        UpperWriterScalarFn writeU32 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU32Rva);
        UpperWriterFlushFn flush = reinterpret_cast<UpperWriterFlushFn>(
            g_dnfBase + kUpperWriterFlushRva);
        if (!value || !writeU16 || !writeU8 || !writeU32 || !flush) {
            LogLine("command-capability send deferred writer boundary unavailable");
            return false;
        }

        writeU16(value(), kCommandCapabilityOpcode);
        writeU32(value(), kCommandCapabilityMagic);
        writeU16(value(), kCommandCapabilityVersion);
        writeU16(value(), kCommandEnumEnd);
        writeU16(value(), kCommandCapabilityBitmapLength);
        for (unsigned int index = 0;
             index < kCommandCapabilityBitmapLength; ++index) {
            writeU8(value(), bitmap[index]);
            fingerprint ^= bitmap[index];
            fingerprint *= 16777619u;
        }
        flush();
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("command-capability scan/send exception code=0x%08X",
            GetExceptionCode());
        return false;
    }

    LogLine("command-capability sent opcode=%u version=%u enum_end=%u bytes=%u handlers=%u fingerprint=0x%08X source=post-class0-op24",
        static_cast<unsigned int>(kCommandCapabilityOpcode),
        static_cast<unsigned int>(kCommandCapabilityVersion),
        static_cast<unsigned int>(kCommandEnumEnd),
        static_cast<unsigned int>(kCommandCapabilityBitmapLength),
        handlerCount, fingerprint);
    return true;
}

void TraceDispatchState(const char* phase, unsigned int classID, int packet)
{
    unsigned int opcode = 0;
    unsigned int packetLength = 0;
    unsigned int bodyLead = 0;
    uintptr_t game = 0;
    int gate = -1;
    void* registry = nullptr;
    void* record = nullptr;
    uintptr_t callback = 0;
    uintptr_t context = 0;
    uintptr_t objectManager = 0;
    uintptr_t currentActor = 0;
    uintptr_t controlledActor = 0;
    uintptr_t sceneRoot = 0;
    unsigned int actorObjectKey = 0;
    uintptr_t actorByObjectKey = 0;
    int townEntryState = -1;
    unsigned int localOwnerServer = 0;
    unsigned int localOwnerChannel = 0;
    char gateBytes[128] = { 0 };
    char tracedBody[192] = { 0 };

    __try {
        if (!packet) return;
        const unsigned char* bytes = reinterpret_cast<const unsigned char*>(packet);
        opcode = *reinterpret_cast<const unsigned short*>(bytes + 1);
        if (!IsObservedDispatchOpcode(classID, opcode)) return;
        packetLength = *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (packetLength >= 20) {
            bodyLead = *reinterpret_cast<const unsigned int*>(bytes + 16);
        }
        if ((opcode == 22 || opcode == 23 || opcode == 356 || opcode == 467 ||
                opcode == 469 || opcode == 470 || opcode == 494 || opcode == 512 ||
                opcode == 1425 ||
                opcode == 14 || opcode == 15 || opcode == 16 || opcode == 17 ||
                opcode == 18 || opcode == 19) &&
            packetLength >= 16 && packetLength <= 4096) {
            const unsigned int bodyLength = packetLength - 16;
            const int tracedLength = static_cast<int>(
                bodyLength > 56 ? 56 : bodyLength);
            FormatHexBytes(bytes + 16, tracedLength, tracedBody, sizeof(tracedBody));
        }

        game = *reinterpret_cast<uintptr_t*>(g_dnfBase + kGameSingletonPointerRva);
        if (game) {
            const unsigned char* gateAddress = reinterpret_cast<const unsigned char*>(
                game + kGameDispatchGateOffset);
            gate = *gateAddress;
            FormatHexBytes(gateAddress - 8, 24, gateBytes, sizeof(gateBytes));
        }

        DispatchRegistryFn registryFn = reinterpret_cast<DispatchRegistryFn>(
            g_dnfBase + (classID == 0 ? kClass0RegistryRva : kClass1RegistryRva));
        DispatchLookupFn lookupFn = reinterpret_cast<DispatchLookupFn>(
            g_dnfBase + kDispatchLookupRva);
        registry = registryFn ? registryFn() : nullptr;
        record = registry && lookupFn ? lookupFn(registry, opcode) : nullptr;
        if (record) {
            callback = *reinterpret_cast<uintptr_t*>(record);
            context = *(reinterpret_cast<uintptr_t*>(record) + 1);
        }

        objectManager = *reinterpret_cast<uintptr_t*>(g_dnfBase + kObjectManagerPointerRva);
        controlledActor = *reinterpret_cast<uintptr_t*>(g_dnfBase + kControlledActorPointerRva);
        sceneRoot = *reinterpret_cast<uintptr_t*>(g_dnfBase + kSceneRootPointerRva);
        townEntryState = *reinterpret_cast<int*>(g_dnfBase + kTownEntryStateRva);
        localOwnerServer = *reinterpret_cast<const unsigned char*>(g_dnfBase + kLocalOwnerServerRva);
        localOwnerChannel = *reinterpret_cast<const unsigned char*>(g_dnfBase + kLocalOwnerChannelRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(g_dnfBase + kCurrentActorRva);
        currentActor = objectManager && currentActorFn
            ? reinterpret_cast<uintptr_t>(currentActorFn(reinterpret_cast<void*>(objectManager)))
            : 0;

        // A mode-0 row stores its actor object key after the 0x47-byte
        // descriptor. Querying the native object table is read-only and lets
        // us distinguish a fresh create from a stale-key short circuit.
        if (classID == 0 && opcode == 2 && bytes[16] == 0 &&
            *reinterpret_cast<const unsigned short*>(bytes + 17) != 0) {
            actorObjectKey = *reinterpret_cast<const unsigned short*>(bytes + 16 + 76);
            ActorByObjectKeyFn actorByObjectKeyFn = reinterpret_cast<ActorByObjectKeyFn>(
                g_dnfBase + kActorByObjectKeyRva);
            actorByObjectKey = actorByObjectKeyFn
                ? reinterpret_cast<uintptr_t>(actorByObjectKeyFn(static_cast<unsigned short>(actorObjectKey)))
                : 0;
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("dispatch-state read exception phase=%s class=%u opcode=%u code=0x%08X",
            phase, classID, opcode, GetExceptionCode());
        return;
    }

    LogLine("dispatch-state phase=%s class=%u opcode=%u packet_len=%u body_lead=0x%08X traced_body=%s game=%p gate=%d town_entry_state=%d local_owner=%u/%u registry=%p record=%p callback=+0x%08X context=%p object_manager=%p current_actor=%p controlled_actor=%p scene_root=%p actor_key=%u actor_by_key=%p gate_bytes=%s",
        phase, classID, opcode, packetLength, bodyLead, tracedBody,
        reinterpret_cast<void*>(game), gate,
        townEntryState, localOwnerServer, localOwnerChannel, registry, record,
        callback ? static_cast<unsigned int>(callback - g_dnfBase) : 0,
        reinterpret_cast<void*>(context), reinterpret_cast<void*>(objectManager),
        reinterpret_cast<void*>(currentActor), reinterpret_cast<void*>(controlledActor),
        reinterpret_cast<void*>(sceneRoot), actorObjectKey,
        reinterpret_cast<void*>(actorByObjectKey), gateBytes);

    if (classID == 0 && (opcode == 108 || opcode == 1206)) {
        __try {
            uintptr_t catalog = *reinterpret_cast<uintptr_t*>(
                g_dnfBase + kSpendTimeCatalogPointerRva);
            uintptr_t uiObject = *reinterpret_cast<uintptr_t*>(
                g_dnfBase + kSpendTimeUiSharedRva);
            uintptr_t uiControl = *reinterpret_cast<uintptr_t*>(
                g_dnfBase + kSpendTimeUiSharedRva + sizeof(uintptr_t));
            unsigned int totalSeconds = catalog
                ? *reinterpret_cast<unsigned int*>(catalog + 5 * sizeof(uintptr_t))
                : 0;
            unsigned int rewardCount = catalog
                ? *reinterpret_cast<unsigned int*>(catalog + 6 * sizeof(uintptr_t))
                : 0;
            unsigned int completedCount = catalog
                ? *reinterpret_cast<unsigned int*>(catalog + 7 * sizeof(uintptr_t))
                : 0;
            LogLine("spend-time native phase=%s opcode=%u catalog=%p total=%u rewards=%u completed=%u ui_object=%p ui_control=%p",
                phase, opcode, reinterpret_cast<void*>(catalog), totalSeconds,
                rewardCount, completedCount, reinterpret_cast<void*>(uiObject),
                reinterpret_cast<void*>(uiControl));
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            LogLine("spend-time native read exception phase=%s opcode=%u code=0x%08X",
                phase, opcode, GetExceptionCode());
        }
    }
}

// Read-only current-EXE evidence for town co-presence. The server can prove
// delivery, but only the native object tables can distinguish a rejected
// mode0 create from a later display-registration failure. Keep this bounded to
// the four actor-state messages and never mutate an actor or scene manager.
void TraceTownCopresenceDispatchState(const char* phase, int packet)
{
    unsigned int opcode = 0;
    unsigned int mode = 0xFF;
    unsigned int ownerContext = 0;
    unsigned int objectKey = 0;
    unsigned int recordKind = 0xFF;
    uintptr_t byObjectKey = 0;
    uintptr_t byContext = 0;
    uintptr_t sceneContext = 0;
    uintptr_t currentActor = 0;

    __try {
        if (!packet) return;
        const unsigned char* bytes = reinterpret_cast<const unsigned char*>(packet);
        opcode = *reinterpret_cast<const unsigned short*>(bytes + 1);
        unsigned int packetLength = *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (packetLength < 16) return;
        const unsigned char* body = bytes + 16;
        const size_t bodyLength = packetLength - 16;

        if (opcode == 2 && bodyLength >= 5) {
            mode = body[0];
            ownerContext = body[4];
            if (mode == 0 && bodyLength >= 0x4E) {
                objectKey = *reinterpret_cast<const unsigned short*>(body + 0x4C);
            } else if (mode == 1 && bodyLength >= 0x17) {
                objectKey = *reinterpret_cast<const unsigned short*>(body + 0x15);
            } else {
                return;
            }
        } else if (opcode == 9 && bodyLength >= 10 &&
                   *reinterpret_cast<const unsigned short*>(body) != 0) {
            objectKey = *reinterpret_cast<const unsigned short*>(body + 4);
            ownerContext = body[7];
            recordKind = body[9];
        } else if (opcode == 23 && bodyLength >= 2) {
            objectKey = *reinterpret_cast<const unsigned short*>(body);
        } else if (opcode == 24 && bodyLength >= 4) {
            // Op24 has no owner context. Log its selected final row only; the
            // bridge separately records every roster row on the wire.
            unsigned int count = *reinterpret_cast<const unsigned short*>(body + 2);
            if (count != 0 && bodyLength >= 4 + count * 8) {
                objectKey = *reinterpret_cast<const unsigned short*>(
                    body + 4 + (count - 1) * 8);
            }
        } else {
            return;
        }

        uintptr_t objectManager =
            *reinterpret_cast<uintptr_t*>(g_dnfBase + kObjectManagerPointerRva);
        uintptr_t sceneActorManager =
            *reinterpret_cast<uintptr_t*>(g_dnfBase + kSceneActorManagerPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_dnfBase + kCurrentActorRva);
        ActorByObjectKeyFn actorByObjectKeyFn = reinterpret_cast<ActorByObjectKeyFn>(
            g_dnfBase + kActorByObjectKeyRva);
        ActorByContextFn actorByContextFn = reinterpret_cast<ActorByContextFn>(
            g_dnfBase + kActorByContextRva);
        ResolveActorInContextFn resolveActorInContextFn =
            reinterpret_cast<ResolveActorInContextFn>(
                g_dnfBase + kResolveActorInContextRva);

        currentActor = objectManager && currentActorFn
            ? reinterpret_cast<uintptr_t>(
                currentActorFn(reinterpret_cast<void*>(objectManager)))
            : 0;
        byObjectKey = objectKey && actorByObjectKeyFn
            ? reinterpret_cast<uintptr_t>(
                actorByObjectKeyFn(static_cast<unsigned short>(objectKey)))
            : 0;
        byContext = objectKey && actorByContextFn
            ? reinterpret_cast<uintptr_t>(actorByContextFn(
                static_cast<unsigned short>(objectKey),
                static_cast<int>(ownerContext)))
            : 0;
        sceneContext = sceneActorManager && objectKey && resolveActorInContextFn
            ? reinterpret_cast<uintptr_t>(resolveActorInContextFn(
                reinterpret_cast<void*>(sceneActorManager),
                static_cast<unsigned char>(ownerContext),
                static_cast<unsigned short>(objectKey)))
            : 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("town-copresence-dispatch read exception phase=%s opcode=%u code=0x%08X",
            phase, opcode, GetExceptionCode());
        return;
    }

    LogLine("town-copresence-dispatch phase=%s opcode=%u mode=%u owner=%u key=%u kind=%u by_key=%p by_context=%p scene_context=%p current_actor=%p",
        phase, opcode, mode, ownerContext, objectKey, recordKind,
        reinterpret_cast<void*>(byObjectKey),
        reinterpret_cast<void*>(byContext),
        reinterpret_cast<void*>(sceneContext),
        reinterpret_cast<void*>(currentActor));
}

void* __cdecl ProxyTownActorByObjectKey(unsigned short objectKey)
{
    ActorByObjectKeyFn original = reinterpret_cast<ActorByObjectKeyFn>(
        g_originalActorByObjectKey);
    void* actor = original ? original(objectKey) : nullptr;
    if (actor || objectKey == 0) return actor;

    unsigned char ownerContext = g_townActorOwnerByObjectKey[objectKey];
    if (ownerContext == 0) return nullptr;

    ActorByContextFn actorByContextFn = reinterpret_cast<ActorByContextFn>(
        g_dnfBase + kActorByContextRva);
    actor = actorByContextFn
        ? actorByContextFn(objectKey, static_cast<int>(ownerContext))
        : nullptr;
    bool packetScoped = g_townActorContextLookupActive &&
        objectKey == g_townActorContextLookupKey;
    if (actor && !packetScoped) {
        void* visual = *reinterpret_cast<void**>(
            reinterpret_cast<uintptr_t>(actor) + kActorVisualOffset);
        if (!visual) return nullptr;
    }
    if (actor && !g_townActorLookupLoggedByObjectKey[objectKey]) {
        g_townActorLookupLoggedByObjectKey[objectKey] = 1;
        uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
        LogLine("town co-presence actor lookup fallback key=%u owner=%u actor=%p packet_scoped=%d caller=+0x%08X",
            objectKey, ownerContext, actor, packetScoped ? 1 : 0,
            caller >= g_dnfBase ? static_cast<unsigned int>(caller - g_dnfBase) : 0);
    }
    return actor;
}

// Current NoPack's remote mode-0 branch stores the actor in the owner-context
// map but does not construct its actor+0x4C4 visual component. Live acceptance
// proved that sub_26A2980 is not a general town-presence list: adding an
// ungrouped peer to its second slot makes the native client render that peer as
// a blue-name party member. Call only the exact visual constructor reached by
// that function, leaving native party-slot membership untouched.
bool EnsureTownRemoteActorVisual(
    unsigned short objectKey, unsigned char ownerContext, void* actor)
{
    if (!objectKey || !ownerContext || !actor) return false;

    __try {
        uintptr_t objectManager =
            *reinterpret_cast<uintptr_t*>(g_dnfBase + kObjectManagerPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_dnfBase + kCurrentActorRva);
        void* currentActor = objectManager && currentActorFn
            ? currentActorFn(reinterpret_cast<void*>(objectManager))
            : nullptr;
        if (actor == currentActor) return false;

        void** visual = reinterpret_cast<void**>(
            reinterpret_cast<uintptr_t>(actor) + kActorVisualOffset);
        if (*visual) {
            LogLine("town co-presence actor visual already exists key=%u owner=%u actor=%p visual=%p",
                objectKey, ownerContext, actor, *visual);
            return true;
        }

        ActorVisualCreateFn createFn = reinterpret_cast<ActorVisualCreateFn>(
            g_dnfBase + kActorVisualCreateRva);
        if (!createFn) return false;
        createFn(actor, 0);
        LogLine("town co-presence actor visual create key=%u owner=%u actor=%p visual=%p",
            objectKey, ownerContext, actor, *visual);
        return *visual != nullptr;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("town co-presence actor visual create exception key=%u owner=%u actor=%p code=0x%08X",
            objectKey, ownerContext, actor, GetExceptionCode());
        return false;
    }
}

void RemoveTownRemoteActorVisual(
    unsigned short objectKey, unsigned char ownerContext)
{
    if (!objectKey || !ownerContext) return;

    __try {
        ActorByContextFn actorByContextFn = reinterpret_cast<ActorByContextFn>(
            g_dnfBase + kActorByContextRva);
        void* actor = actorByContextFn
            ? actorByContextFn(objectKey, static_cast<int>(ownerContext))
            : nullptr;
        if (!actor) return;

        void** visual = reinterpret_cast<void**>(
            reinterpret_cast<uintptr_t>(actor) + kActorVisualOffset);
        if (!*visual) return;
        ActorVisualDestroyFn destroyFn = reinterpret_cast<ActorVisualDestroyFn>(
            g_dnfBase + kActorVisualDestroyRva);
        if (!destroyFn) return;
        void* visualBefore = *visual;
        destroyFn(actor);
        LogLine("town co-presence actor visual remove key=%u owner=%u actor=%p visual_before=%p visual_after=%p",
            objectKey, ownerContext, actor, visualBefore, *visual);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("town co-presence actor visual remove exception key=%u owner=%u code=0x%08X",
            objectKey, ownerContext, GetExceptionCode());
    }
}

void RepairTownCurrentActor(unsigned short objectKey)
{
    __try {
        uintptr_t objectManager = *reinterpret_cast<uintptr_t*>(
            g_dnfBase + kObjectManagerPointerRva);
        if (!objectManager) {
            LogLine("town current actor repair skipped key=%u reason=no-object-manager",
                objectKey);
            return;
        }

        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_dnfBase + kCurrentActorRva);
        void* currentBefore = currentActorFn
            ? currentActorFn(reinterpret_cast<void*>(objectManager))
            : nullptr;

        ActorByObjectKeyFn originalActorByObjectKey =
            reinterpret_cast<ActorByObjectKeyFn>(g_originalActorByObjectKey);
        void* actor = originalActorByObjectKey
            ? originalActorByObjectKey(objectKey)
            : nullptr;
        const char* source = "global";
        if (!actor) {
            ActorByContextFn actorByContextFn = reinterpret_cast<ActorByContextFn>(
                g_dnfBase + kActorByContextRva);
            actor = actorByContextFn
                ? actorByContextFn(objectKey, 0)
                : nullptr;
            source = "context0";
        }

        void** cache = reinterpret_cast<void**>(
            objectManager + kCurrentActorCacheOffset);
        void* cacheBefore = *cache;
        bool repaired = false;
        if (!currentBefore && actor) {
            // sub_269A050 owns this exact cache slot and lazily fills it with
            // sub_2699A30's result. The context-0 bridge row can create the
            // selected actor after that first lazy lookup has cached null, so
            // complete the same native assignment after op2 has initialized it.
            *cache = actor;
            repaired = true;
        }

        void* currentAfter = currentActorFn
            ? currentActorFn(reinterpret_cast<void*>(objectManager))
            : *cache;
        LogLine("town current actor repair key=%u source=%s actor=%p cache_before=%p current_before=%p current_after=%p repaired=%d verified=%d",
            objectKey, source, actor, cacheBefore, currentBefore, currentAfter,
            repaired ? 1 : 0, actor && currentAfter == actor ? 1 : 0);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("town current actor repair exception key=%u code=0x%08X",
            objectKey, GetExceptionCode());
    }
}

LONG g_localActorClaimRejectedCount = 0;

// A mode-0 row whose key resolves to an actor object other than the one this
// client controls is a peer's actor install, not a local refresh. The log line
// keeps the guard auditable: if the local actor's own row were ever rejected
// here, every reader of g_currentActorObjectKey would stay stale, and this is
// the only place that would say so.
void LogActorClaimRejected(unsigned short objectKey, unsigned char ownerContext,
    void* packetActor, void* currentActor, unsigned short claimedKey)
{
    LONG hit = InterlockedIncrement(&g_localActorClaimRejectedCount);
    if (hit <= 16) {
        LogLine("local actor claim rejected key=%u owner=%u packet_actor=%p "
            "current_actor=%p claimed_key=%u hit=%ld "
            "reason=packet-actor-is-not-current-actor",
            objectKey, ownerContext, packetActor, currentActor, claimedKey, hit);
    }
}

// Formerly ReapplyCurrentActorExpertJobTypeAfterMode0: the expert-job
// projection it carried is gone, only the local actor key claim remains.
void ClaimLocalActorFromMode0Row(
    unsigned short objectKey, unsigned char ownerContext)
{
    if (!g_dnfBase || !objectKey) return;

    __try {
        // A local mode-0 row carries the current channel as its owner.  The
        // native key lookup can still return null while the actor is being
        // rebuilt, so use the owner-channel proof before falling back to the
        // already resolved current actor.  Remote owner contexts remain
        // excluded from this projection.
        bool localOwnerContext = ownerContext == 0;
        if (!localOwnerContext && kLocalOwnerChannelRva != 0) {
            const unsigned char localOwner = *reinterpret_cast<const unsigned char*>(
                g_dnfBase + kLocalOwnerChannelRva);
            localOwnerContext = localOwner != 0 && localOwner == ownerContext;
        }
        if (!localOwnerContext) return;

        void* objectManager = *reinterpret_cast<void**>(
            g_dnfBase + kObjectManagerPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_dnfBase + kCurrentActorRva);
        void* currentActor = objectManager && currentActorFn
            ? currentActorFn(objectManager)
            : nullptr;
        if (!currentActor) return;

        ActorByObjectKeyFn originalActorByObjectKey =
            reinterpret_cast<ActorByObjectKeyFn>(g_originalActorByObjectKey);
        void* packetActor = originalActorByObjectKey
            ? originalActorByObjectKey(objectKey)
            : nullptr;
        if (!packetActor && ownerContext != 0) {
            ActorByContextFn actorByContextFn = reinterpret_cast<ActorByContextFn>(
                g_dnfBase + kActorByContextRva);
            packetActor = actorByContextFn
                ? actorByContextFn(objectKey, static_cast<int>(ownerContext))
                : nullptr;
        }
        // A channel-owned mode-0 row is NOT proof of the local actor: a raid
        // mate's actor install arrives with the same owner byte (the receiving
        // channel) and resolves to that mate's own actor object.  Letting such a
        // row claim the local key poisons every reader that asks this module
        // "which character am I": live 2026-09-17 18:26:12.840 the berserker's
        // own client cached key=1 job=0 grow=0x21 (eligible=0) from the peer's
        // install and lost its own red aura, while the mirrored install on the
        // other client (18:26:23.459 key=22 grow=0x23 eligible=1) put the red
        // aura onto 1111.  Only a row that resolves to the actor this client
        // already controls may claim the local key.
        const bool packetActorMatches = packetActor == currentActor;
        if (packetActor && !packetActorMatches) {
            LogActorClaimRejected(objectKey, ownerContext, packetActor, currentActor,
                g_currentActorObjectKey);
            return;
        }

        // An unresolved row (both lookups missed) keeps the benefit of the
        // doubt: during the native mode-0 rebuild the lookups can miss while
        // currentActor is already the local actor.
        //
        // This claim and its guard are the whole remaining job of this function:
        // they feed the berserker Blood Frenzy aura and the town co-presence
        // bookkeeping. The expert-job projection that used to run here is gone -
        // writing the local actor's sub-profession state is exactly what raised
        // the 「副职业资格认证」 window, and the profession is displayed from the
        // server's own character record instead.
        g_currentActorObjectKey = objectKey;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("local actor claim exception key=%u owner=%u code=0x%08X",
            objectKey, ownerContext, GetExceptionCode());
    }
}

void CacheCurrentCharacterStatsFromClass0Packet(int packet);

bool TryConsumePrivateCombatPowerAffixes(int packet)
{
    DNF90DamageAffixSnapshot snapshot = {};
    bool matched = false;
    __try {
        if (packet) {
            const unsigned char* bytes =
                reinterpret_cast<const unsigned char*>(packet);
            const unsigned short opcode =
                *reinterpret_cast<const unsigned short*>(bytes + 1);
            const unsigned int packetLength =
                *reinterpret_cast<const unsigned int*>(bytes + 3);
            if (opcode == kCombatPowerAffixPrivateOpcode &&
                packetLength == kCombatPowerAffixPrivatePacketLength &&
                bytes[16] == kCombatPowerAffixPrivateType &&
                bytes[17] == kCombatPowerAffixPrivateVersion) {
                const unsigned char* body = bytes + 16;
                snapshot.size = sizeof(snapshot);
                snapshot.generation = static_cast<unsigned int>(
                    InterlockedIncrement(&g_damageAffixSnapshotGeneration));
                snapshot.validFlags =
                    DNF90_DAMAGE_AFFIX_SNAPSHOT_VALUES_VALID |
                    DNF90_DAMAGE_AFFIX_SNAPSHOT_IDENTITY_VALID |
                    DNF90_DAMAGE_AFFIX_SNAPSHOT_THREE_ATTACKS_VALID |
                    DNF90_DAMAGE_AFFIX_SNAPSHOT_EQUIPMENT_SCORE_VALID;
                snapshot.version = body[1];
                snapshot.whiteDamageTenths =
                    *reinterpret_cast<const unsigned short*>(body + 2);
                snapshot.yellowDamageTenths =
                    *reinterpret_cast<const unsigned short*>(body + 4);
                snapshot.criticalDamageTenths =
                    *reinterpret_cast<const unsigned short*>(body + 6);
                snapshot.yellowAdditionalTenths =
                    *reinterpret_cast<const unsigned short*>(body + 8);
                snapshot.criticalAdditionalTenths =
                    *reinterpret_cast<const unsigned short*>(body + 10);
                snapshot.allAttackTenths =
                    *reinterpret_cast<const unsigned short*>(body + 12);
                snapshot.equippedItemCount =
                    *reinterpret_cast<const unsigned short*>(body + 14);
                snapshot.activeSetCount =
                    *reinterpret_cast<const unsigned short*>(body + 16);
                snapshot.job = body[18];
                snapshot.growType = body[19];
                snapshot.level = body[20];
                const unsigned int professionLength = body[21];
                snapshot.physicalAttack =
                    *reinterpret_cast<const unsigned int*>(body + 22);
                snapshot.magicalAttack =
                    *reinterpret_cast<const unsigned int*>(body + 26);
                snapshot.independentAttack =
                    *reinterpret_cast<const unsigned int*>(body + 30);
                snapshot.pvfEquipmentScore =
                    *reinterpret_cast<const unsigned int*>(body + 66);
                snapshot.characterID =
                    *reinterpret_cast<const unsigned int*>(body + 70);
                if (professionLength > 0 &&
                    professionLength < sizeof(snapshot.professionUtf8)) {
                    memcpy(snapshot.professionUtf8, body + 34,
                        professionLength);
                    snapshot.professionUtf8[professionLength] = '\0';
                }
                matched = snapshot.equippedItemCount <=
                        DNF90_EQUIPMENT_SNAPSHOT_MAX_ITEMS &&
                    snapshot.level > 0 && snapshot.characterID > 0 &&
                    professionLength > 0 &&
                    professionLength < sizeof(snapshot.professionUtf8);
            }
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
    if (!matched) return false;

    AcquireSRWLockExclusive(&g_damageAffixSnapshotLock);
    g_damageAffixSnapshot = snapshot;
    ReleaseSRWLockExclusive(&g_damageAffixSnapshotLock);

    const LONG newCharacterID = static_cast<LONG>(snapshot.characterID);
    const LONG oldCharacterID = InterlockedExchange(
        &g_combatPanelCurrentCharacterID, newCharacterID);
    const LONG suppressedCharacterID = InterlockedCompareExchange(
        &g_combatPanelSuppressedCharacterID, 0, 0);
    if (oldCharacterID != 0 && oldCharacterID != newCharacterID &&
        suppressedCharacterID != 0) {
        InterlockedCompareExchange(&g_combatPanelSuppressedCharacterID, 0,
            suppressedCharacterID);
        LogLine("combat-panel character changed old=%ld new=%ld "
            "suppression_cleared=1", oldCharacterID, newCharacterID);
    }
    LONG hit = InterlockedIncrement(&g_damageAffixSnapshotUpdateCount);
    if (hit <= 8) {
        LogLine("combat-power affix private projection consumed hit=%ld generation=%u white=%u yellow=%u critical=%u yellow_add=%u critical_add=%u all_attack=%u items=%u sets=%u job=%u grow=%u level=%u physical=%u magical=%u independent=%u pvf_equipment_score=%u character_id=%u profession=%s",
            hit, snapshot.generation, snapshot.whiteDamageTenths,
            snapshot.yellowDamageTenths, snapshot.criticalDamageTenths,
            snapshot.yellowAdditionalTenths,
            snapshot.criticalAdditionalTenths,
            snapshot.allAttackTenths,
            snapshot.equippedItemCount, snapshot.activeSetCount,
            snapshot.job, snapshot.growType, snapshot.level,
            snapshot.physicalAttack, snapshot.magicalAttack,
            snapshot.independentAttack, snapshot.pvfEquipmentScore,
            snapshot.characterID,
            snapshot.professionUtf8);
    }
    return true;
}

// DLL-private class0/op413 mood projection. The combat-power sidecar uses the
// same opcode with a different packet length/type; the two consumers are
// intentionally ordered and independently length-gated.
constexpr unsigned int kMoodProjectionPrivatePacketLength = 20;
constexpr unsigned char kMoodProjectionPrivateType = 2;
constexpr unsigned char kMoodProjectionPrivateVersion = 1;
constexpr uintptr_t kMoodSettingsManagerPointerRva = 0x04DB2764;
constexpr uintptr_t kMoodSettingsVectorRva = 0x02BD42C0;
constexpr uintptr_t kMoodSettingsApplyRva = 0x02E007F0;
constexpr unsigned int kMoodSettingsVectorIndex = 0x1C4;
constexpr uintptr_t kMoodActorStoreRva = 0x02290680;
constexpr uintptr_t kMoodActorVisualRva = 0x02294670;
constexpr uintptr_t kMoodActorOrdinaryEffectRva = 0x02E463C0;
constexpr uintptr_t kMoodActorAlternateEffectRva = 0x02E460B0;
constexpr size_t kMoodActorStoredValueOffset = 0x8BC;

typedef void* (__thiscall* MoodSettingsVectorFn)(void*, unsigned int);
typedef void (__thiscall* MoodSettingsApplyFn)(void*, unsigned int);
typedef short (__thiscall* MoodActorStoreFn)(void*, short);
typedef void* (__thiscall* MoodActorVisualFn)(void*, int);
typedef void (__cdecl* MoodActorEffectFn)(void*, void*, int, unsigned char);

bool TryConsumePrivateMoodProjection(int packet)
{
    bool matched = false;
    bool actorApplied = false;
    bool panelSynced = false;
    unsigned int moodValue = 0;
    const char* state = "not-matched";
    const char* panelState = "not-checked";
    void* actor = nullptr;
    void* actorVisual = nullptr;
    void* manager = nullptr;
    void* vector = nullptr;
    void* owner = nullptr;
    __try {
        if (!packet) return false;
        const unsigned char* bytes = reinterpret_cast<const unsigned char*>(packet);
        const unsigned short opcode = *reinterpret_cast<const unsigned short*>(bytes + 1);
        const unsigned int packetLength = *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (opcode != kCombatPowerAffixPrivateOpcode ||
            packetLength != kMoodProjectionPrivatePacketLength ||
            bytes[16] != kMoodProjectionPrivateType ||
            bytes[17] != kMoodProjectionPrivateVersion) {
            return false;
        }
        matched = true;
        moodValue = *reinterpret_cast<const unsigned short*>(bytes + 18);
        void* objectManager = *reinterpret_cast<void**>(g_dnfBase + kObjectManagerPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(g_dnfBase + kCurrentActorRva);
        MoodActorStoreFn storeActorMood = reinterpret_cast<MoodActorStoreFn>(g_dnfBase + kMoodActorStoreRva);
        MoodActorVisualFn actorVisualForMood = reinterpret_cast<MoodActorVisualFn>(g_dnfBase + kMoodActorVisualRva);
        MoodActorEffectFn applyOrdinaryMood = reinterpret_cast<MoodActorEffectFn>(g_dnfBase + kMoodActorOrdinaryEffectRva);
        MoodActorEffectFn applyAlternateMood = reinterpret_cast<MoodActorEffectFn>(g_dnfBase + kMoodActorAlternateEffectRva);
        actor = objectManager && currentActorFn ? currentActorFn(objectManager) : nullptr;
        if (actor && storeActorMood && actorVisualForMood && applyOrdinaryMood && applyAlternateMood) {
            storeActorMood(actor, static_cast<short>(moodValue));
            actorVisual = actorVisualForMood(actor, 0);
            if (actorVisual) {
                MoodActorEffectFn applyMoodEffect = moodValue >= 1000 ? applyAlternateMood : applyOrdinaryMood;
                applyMoodEffect(actor, actorVisual, static_cast<int>(moodValue), 1);
                actorApplied = true;
                state = "actor-applied";
            } else {
                state = "actor-mood-stored-visual-missing";
            }
        } else {
            state = actor ? "actor-mood-functions-unavailable" : "current-actor-missing";
        }
        manager = *reinterpret_cast<void**>(g_dnfBase + kMoodSettingsManagerPointerRva);
        MoodSettingsVectorFn vectorForOwner = reinterpret_cast<MoodSettingsVectorFn>(g_dnfBase + kMoodSettingsVectorRva);
        MoodSettingsApplyFn applyMood = reinterpret_cast<MoodSettingsApplyFn>(g_dnfBase + kMoodSettingsApplyRva);
        if (!manager || !vectorForOwner || !applyMood) {
            panelState = "native-resolver-unavailable";
        } else if (!(vector = vectorForOwner(manager, kMoodSettingsVectorIndex))) {
            panelState = "settings-vector-missing";
        } else {
            uintptr_t begin = *reinterpret_cast<uintptr_t*>(reinterpret_cast<unsigned char*>(vector) + 4);
            uintptr_t end = *reinterpret_cast<uintptr_t*>(reinterpret_cast<unsigned char*>(vector) + 8);
            if (begin && end > begin && (owner = *reinterpret_cast<void**>(begin))) {
                applyMood(owner, moodValue);
                panelSynced = true;
                panelState = "panel-synced";
            } else {
                panelState = "settings-vector-empty";
            }
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        if (matched) LogLine("mood private projection exception mood=%u state=%s panel_state=%s code=0x%08X", moodValue, state, panelState, GetExceptionCode());
        return matched;
    }
    if (matched) LogLine("mood private projection consumed mood=%u actor_applied=%d panel_synced=%d state=%s panel_state=%s actor=%p visual=%p manager=%p vector=%p owner=%p", moodValue, actorApplied ? 1 : 0, panelSynced ? 1 : 0, state, panelState, actor, actorVisual, manager, vector, owner);
    return matched;
}

bool TryConsumePrivateBossMutationDecision(int packet)
{
    DNF90BossMutationDecision decision = {};
    __try {
        if (!packet) return false;
        const unsigned char* bytes =
            reinterpret_cast<const unsigned char*>(packet);
        const unsigned short opcode =
            *reinterpret_cast<const unsigned short*>(bytes + 1);
        const unsigned int packetLength =
            *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (opcode != kBossMutationPrivateOpcode ||
            packetLength != kBossMutationPrivatePacketLength ||
            bytes[16] != kBossMutationPrivateType ||
            bytes[17] != kBossMutationPrivateVersion) {
            return false;
        }
        const unsigned char* body = bytes + 16;
        decision.size = sizeof(decision);
        decision.generation = static_cast<unsigned int>(
            InterlockedIncrement(&g_bossMutationDecisionGeneration));
        decision.token =
            *reinterpret_cast<const unsigned short*>(body + 2);
        decision.monsterID =
            *reinterpret_cast<const unsigned int*>(body + 4);
        decision.objectKey =
            *reinterpret_cast<const unsigned short*>(body + 8);
        decision.allowed = body[10];
        decision.reason = body[11];
        if (decision.token == 0 || decision.monsterID == 0 ||
            decision.objectKey == 0 || decision.allowed > 1 ||
            (decision.allowed == 1 && decision.reason != 0)) {
            LogLine("boss-mutation private response rejected token=%u monster=%u key=%u allowed=%u reason=%u",
                decision.token, decision.monsterID, decision.objectKey,
                decision.allowed, decision.reason);
            return true;
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("boss-mutation private response exception code=0x%08X",
            GetExceptionCode());
        return true;
    }

    bool pendingMatched = false;
    AcquireSRWLockExclusive(&g_bossMutationActorLock);
    for (size_t index = 0; index < g_bossMutationPendingCount; ++index) {
        const BossMutationPendingRequest& pending =
            g_bossMutationPending[index];
        if (pending.token != decision.token ||
            pending.objectKey != decision.objectKey ||
            pending.monsterID != decision.monsterID ||
            pending.roomGeneration != g_bossMutationRoomGeneration) {
            continue;
        }
        pendingMatched = true;
        for (size_t move = index + 1;
             move < g_bossMutationPendingCount; ++move) {
            g_bossMutationPending[move - 1] =
                g_bossMutationPending[move];
        }
        --g_bossMutationPendingCount;
        g_bossMutationPending[g_bossMutationPendingCount] = {};
        break;
    }
    ReleaseSRWLockExclusive(&g_bossMutationActorLock);
    if (!pendingMatched) {
        LogLine("boss-mutation private response dropped reason=no-current-room-request token=%u monster=%u key=%u",
            decision.token, decision.monsterID, decision.objectKey);
        return true;
    }

    AcquireSRWLockExclusive(&g_bossMutationDecisionLock);
    if (g_bossMutationDecisionCount >=
        kBossMutationDecisionQueueCapacity) {
        ReleaseSRWLockExclusive(&g_bossMutationDecisionLock);
        LogLine("boss-mutation private response dropped reason=queue-full token=%u monster=%u key=%u",
            decision.token, decision.monsterID, decision.objectKey);
        return true;
    }
    const size_t tail = (g_bossMutationDecisionHead +
        g_bossMutationDecisionCount) % kBossMutationDecisionQueueCapacity;
    g_bossMutationDecisionQueue[tail] = decision;
    ++g_bossMutationDecisionCount;
    ReleaseSRWLockExclusive(&g_bossMutationDecisionLock);
    LogLine("boss-mutation private response queued generation=%u token=%u monster=%u key=%u allowed=%u reason=%u",
        decision.generation, decision.token, decision.monsterID,
        decision.objectKey, decision.allowed, decision.reason);
    return true;
}

void ApplyCharacterOptionRestore(const void* body, size_t bodyLength)
{
    if (!body || bodyLength != kCharacterOptionBodyLength) return;
    const unsigned char* source = static_cast<const unsigned char*>(body);
    unsigned int declaredLength = 0;
    void* manager = reinterpret_cast<void*>(
        g_dnfBase + kCharacterOptionManagerRva);
    unsigned char* destination = static_cast<unsigned char*>(manager) +
        kCharacterOptionTableOffset;
    __try {
        declaredLength = *reinterpret_cast<const unsigned int*>(source);
        if (declaredLength != kCharacterOptionTableLength) return;
        memcpy(destination, source + sizeof(declaredLength),
            kCharacterOptionTableLength);
        CharacterOptionRuntimeRefreshFn refresh =
            reinterpret_cast<CharacterOptionRuntimeRefreshFn>(
                g_dnfBase + kCharacterOptionRuntimeRefreshRva);
        refresh(manager, nullptr);
        CharacterOptionReminderApplyFn apply =
            reinterpret_cast<CharacterOptionReminderApplyFn>(
                g_dnfBase + kCharacterOptionReminderApplyRva);
        apply(manager, nullptr);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("character-option restore apply exception code=0x%08X",
            GetExceptionCode());
        return;
    }
    LogLine("character-option restore applied bytes=%u target=manager+0x%03X refresh=0x%08X reminder_apply=0x%08X",
        declaredLength, static_cast<unsigned int>(kCharacterOptionTableOffset),
        static_cast<unsigned int>(kCharacterOptionRuntimeRefreshRva),
        static_cast<unsigned int>(kCharacterOptionReminderApplyRva));
}

void ApplyAuthoritativeConsumableUseGateReset(const char* source)
{
    if (!g_dnfBase) return;

    void* actor = nullptr;
    unsigned char before = 0;
    unsigned char after = 0;
    __try {
        void* objectManager = *reinterpret_cast<void**>(
            g_dnfBase + kObjectManagerPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_dnfBase + kCurrentActorRva);
        actor = objectManager && currentActorFn
            ? currentActorFn(objectManager)
            : nullptr;
        if (!actor) {
            LogLine("consumable use gate reset deferred source=%s reason=no-current-actor",
                source ? source : "unknown");
            return;
        }

        volatile unsigned char* gate =
            reinterpret_cast<volatile unsigned char*>(actor) +
            kActorConsumableUseGateOffset;
        before = *gate;
        ActorConsumableUseGateSetterFn setter =
            reinterpret_cast<ActorConsumableUseGateSetterFn>(
                g_dnfBase + kActorConsumableUseGateSetterRva);
        setter(actor, 0);
        after = *gate;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("consumable use gate reset fault source=%s actor=%p code=0x%08X",
            source ? source : "unknown", actor, GetExceptionCode());
        return;
    }

    LogLine("consumable use gate reset applied source=%s actor=%p before=%u after=%u",
        source ? source : "unknown", actor, before, after);
}

void ArmRealtimeStoryDigestFromQuestFinish(int packet, int nativeResult)
{
    if (!packet || !nativeResult ||
        InterlockedCompareExchange(&g_storyDigestRealtimeReady, 0, 0) == 0) {
        return;
    }

    unsigned int opcode = 0;
    unsigned int packetLength = 0;
    unsigned int success = 0;
    __try {
        const unsigned char* bytes =
            reinterpret_cast<const unsigned char*>(packet);
        opcode = *reinterpret_cast<const unsigned short*>(bytes + 1);
        if (opcode != kQuestFinishResponseOpcode) return;

        packetLength = *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (packetLength < 17) {
            LogLine("story-digest realtime arm skipped opcode=%u packet_len=%u reason=short-packet",
                opcode, packetLength);
            return;
        }
        success = bytes[16];
        if (success != 1) {
            LogLine("story-digest realtime arm skipped opcode=%u packet_len=%u success=%u reason=quest-finish-rejected",
                opcode, packetLength, success);
            return;
        }

        g_storyDigestQuestFinishPacketLength = packetLength;
        g_storyDigestQuestFinishTickMs = GetTickCount64();
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        g_storyDigestQuestFinishTickMs = 0;
        g_storyDigestQuestFinishPacketLength = 0;
        LogLine("story-digest realtime arm exception opcode=%u packet_len=%u code=0x%08X",
            opcode, packetLength, GetExceptionCode());
        return;
    }

    LogLine("story-digest realtime armed opcode=%u packet_len=%u success=%u window_ms=%llu",
        opcode, packetLength, success,
        static_cast<unsigned long long>(kStoryDigestQuestFinishWindowMs));
}

void ApplyRealtimeStoryDigestAfterNativeState(
    unsigned int opcode, unsigned int packetLength, int nativeResult)
{
    if (opcode != kStoryDigestStateOpcode) return;

    const ULONGLONG armedTickMs = g_storyDigestQuestFinishTickMs;
    const unsigned int questPacketLength =
        g_storyDigestQuestFinishPacketLength;
    if (armedTickMs == 0) return;

    // Consume the finish marker before entering native story code. A later
    // selection-time op1378 must remain a plain state restore even if this
    // attempt finds that the current actor is no longer available.
    g_storyDigestQuestFinishTickMs = 0;
    g_storyDigestQuestFinishPacketLength = 0;

    const ULONGLONG nowMs = GetTickCount64();
    const ULONGLONG ageMs = nowMs >= armedTickMs
        ? nowMs - armedTickMs
        : ~static_cast<ULONGLONG>(0);
    if (!nativeResult) {
        LogLine("story-digest realtime skipped opcode=%u packet_len=%u quest_packet_len=%u age_ms=%llu reason=native-state-rejected",
            opcode, packetLength, questPacketLength,
            static_cast<unsigned long long>(ageMs));
        return;
    }
    if (packetLength != kStoryDigestStatePacketLength) {
        LogLine("story-digest realtime skipped opcode=%u packet_len=%u quest_packet_len=%u age_ms=%llu reason=unexpected-state-length",
            opcode, packetLength, questPacketLength,
            static_cast<unsigned long long>(ageMs));
        return;
    }
    if (ageMs > kStoryDigestQuestFinishWindowMs) {
        LogLine("story-digest realtime skipped opcode=%u packet_len=%u quest_packet_len=%u age_ms=%llu reason=expired-finish-window",
            opcode, packetLength, questPacketLength,
            static_cast<unsigned long long>(ageMs));
        return;
    }
    if (InterlockedCompareExchange(&g_storyDigestRealtimeReady, 0, 0) == 0) {
        LogLine("story-digest realtime skipped opcode=%u packet_len=%u quest_packet_len=%u age_ms=%llu reason=native-signature-unavailable",
            opcode, packetLength, questPacketLength,
            static_cast<unsigned long long>(ageMs));
        return;
    }

    void* actor = nullptr;
    void* manager = nullptr;
    void* singleton = nullptr;
    unsigned int beforeLevel = 0;
    unsigned int afterLevel = 0;
    unsigned int evaluated = 0;
    __try {
        void* objectManager = *reinterpret_cast<void**>(
            g_dnfBase + kObjectManagerPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_dnfBase + kCurrentActorRva);
        actor = objectManager && currentActorFn
            ? currentActorFn(objectManager)
            : nullptr;
        if (!actor) {
            LogLine("story-digest realtime skipped opcode=%u packet_len=%u quest_packet_len=%u age_ms=%llu reason=no-current-actor",
                opcode, packetLength, questPacketLength,
                static_cast<unsigned long long>(ageMs));
            return;
        }

        StoryDigestManagerGetterFn getter =
            reinterpret_cast<StoryDigestManagerGetterFn>(
                g_dnfBase + kStoryDigestManagerGetterRva);
        manager = getter ? getter() : nullptr;
        singleton = *reinterpret_cast<void**>(
            g_dnfBase + kStoryDigestManagerPointerRva);
        if (!manager || manager != singleton) {
            LogLine("story-digest realtime skipped opcode=%u packet_len=%u quest_packet_len=%u age_ms=%llu manager=%p singleton=%p reason=manager-unavailable",
                opcode, packetLength, questPacketLength,
                static_cast<unsigned long long>(ageMs), manager, singleton);
            return;
        }

        volatile unsigned int* lastLevel =
            reinterpret_cast<volatile unsigned int*>(
                reinterpret_cast<unsigned char*>(manager) +
                kStoryDigestLastLevelOffset);
        beforeLevel = *lastLevel;
        StoryDigestEvaluatorFn evaluator =
            reinterpret_cast<StoryDigestEvaluatorFn>(
                g_dnfBase + kStoryDigestEvaluatorRva);
        evaluated = evaluator && evaluator(manager, 0) ? 1u : 0u;
        afterLevel = *lastLevel;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("story-digest realtime evaluator exception opcode=%u packet_len=%u quest_packet_len=%u age_ms=%llu actor=%p manager=%p before=%u code=0x%08X",
            opcode, packetLength, questPacketLength,
            static_cast<unsigned long long>(ageMs), actor, manager,
            beforeLevel, GetExceptionCode());
        return;
    }

    LogLine("story-digest realtime evaluated opcode=%u packet_len=%u quest_packet_len=%u age_ms=%llu actor=%p manager=%p before=%u after=%u started=%u ack_opcode=1445",
        opcode, packetLength, questPacketLength,
        static_cast<unsigned long long>(ageMs), actor, manager,
        beforeLevel, afterLevel, evaluated);
}

void* ResolveUiManager()
{
    if (!g_dnfBase) return nullptr;
    __try {
        return *reinterpret_cast<void**>(g_dnfBase + kUiManagerPointerRva);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return nullptr;
    }
}

// Close the expert-job qualification window (inert safety net).
//
// 2026-10-03: the expert-job state projection that used to arm this close window
// is permanently removed, because writing the local actor's sub-profession field
// is exactly what makes this client raise 「副职业资格认证 / 您学会新技能了」, and
// no timing trick separated the profession display from that window. Nothing
// arms g_expertJobWindowCloseUntilTick any more, so ServiceExpertJobWindowClose()
// now returns immediately on every packet; the close path is kept only so that a
// stray window can still be closed by hand if one is ever observed again.
//
// Census of the UI registry: list every slot that currently holds a non-null
// element, with that element's vtable. Taken twice around a projection, the diff
// is exactly the window the client opens for it, which is what has to be closed.
// ---- ui registry change tracker ---------------------------------------------
//
// The expert-job window is UI registry slot 0x1AA and it is already occupied by the
// time the op205 projection runs, so the packet that creates/shows it is an earlier
// one in the login sequence. This snapshots the whole registry on every class0
// packet and logs a diff the moment it changes, together with the opcode being
// dispatched -- which names the offending packet directly.
volatile LONG g_uiCensusArmedUntil = 0;
volatile LONG g_uiCensusChangeCount = 0;
volatile LONG g_uiCensusSnapshotCount = 0;
volatile LONG g_uiCensusAutoArmed = 0;
unsigned char g_uiCensusPresent[0x487] = { 0 };
bool g_uiCensusHaveSnapshot = false;

void UiRegistrySnapshot(unsigned int opcode, unsigned int packetLength)
{
    if (!g_dnfBase) return;
    void* manager = ResolveUiManager();
    if (!manager) return;
    UiSlotPresentFn present = reinterpret_cast<UiSlotPresentFn>(
        g_dnfBase + kUiSlotPresentRva);
    UiSlotLookupFn lookup = reinterpret_cast<UiSlotLookupFn>(
        g_dnfBase + kUiSlotLookupRva);
    if (!present) return;

    char line[1200];
    int used = 0;
    line[0] = '\0';
    int changes = 0;

    for (unsigned int slot = 0; slot < 0x487; slot++) {
        unsigned char now = 0;
        __try {
            now = present(manager, slot) ? 1 : 0;
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            now = 0;
        }
        if (!g_uiCensusHaveSnapshot) {
            g_uiCensusPresent[slot] = now;
            continue;
        }
        if (now == g_uiCensusPresent[slot]) continue;
        g_uiCensusPresent[slot] = now;
        changes++;
        if (used < 900) {
            unsigned int vtable = 0;
            if (now && lookup) {
                __try {
                    void* element = lookup(manager, static_cast<int>(slot), 0);
                    if (element) {
                        vtable = static_cast<unsigned int>(
                            *reinterpret_cast<uintptr_t*>(element));
                    }
                }
                __except (EXCEPTION_EXECUTE_HANDLER) {
                    vtable = 0;
                }
            }
            int w = _snprintf_s(line + used, sizeof(line) - used, _TRUNCATE,
                " %s%X:%08X", now ? "+" : "-", slot, vtable);
            if (w <= 0) break;
            used += w;
        }
    }
    g_uiCensusHaveSnapshot = true;
    const LONG snaps = InterlockedIncrement(&g_uiCensusSnapshotCount);
    if (snaps % 50 == 0) {
        LogLine("ui registry snapshots=%d changes=%d last_opcode=%u",
            snaps, InterlockedCompareExchange(&g_uiCensusChangeCount, 0, 0),
            opcode);
    }
    if (changes != 0) {
        const LONG n = InterlockedIncrement(&g_uiCensusChangeCount);
        LogLine("ui registry change opcode=%u packet_len=%u changes=%d count=%d%s",
            opcode, packetLength, changes, n, line);
    }
}

void ArmUiRegistryTracking(const char* source)
{
    InterlockedExchange(&g_uiCensusArmedUntil,
        static_cast<LONG>(GetTickCount() + 90000));
    LogLine("ui registry tracking armed source=%s", source ? source : "unknown");
}

bool UiRegistryTrackingActive()
{
    // Arm on the very first class0 packet of the process: the window is already
    // built by the time the op205 projection runs, so tracking has to start
    // earlier than that to see it appear.
    if (InterlockedCompareExchange(&g_uiCensusAutoArmed, 1, 0) == 0) {
        InterlockedExchange(&g_uiCensusArmedUntil,
            static_cast<LONG>(GetTickCount() + 180000));
        LogLine("ui registry tracking auto-armed on first class0 packet");
    }
    const LONG until = InterlockedCompareExchange(&g_uiCensusArmedUntil, 0, 0);
    if (until == 0) return false;
    return static_cast<LONG>(static_cast<LONG>(GetTickCount()) - until) < 0;
}

// Census of the UI registry: list every slot that currently holds a non-null
// element, with that element's vtable. Taken twice around a projection, the diff
// is exactly the window the client opens for it, which is what has to be closed.
void CensusUiSlots(const char* tag)
{
    if (!g_dnfBase) return;
    void* manager = ResolveUiManager();
    if (!manager) return;

    UiSlotPresentFn present = reinterpret_cast<UiSlotPresentFn>(
        g_dnfBase + kUiSlotPresentRva);
    UiSlotLookupFn lookup = reinterpret_cast<UiSlotLookupFn>(
        g_dnfBase + kUiSlotLookupRva);
    if (!present || !lookup) return;

    static char line[2048];
    int used = 0;
    line[0] = '\0';
    for (unsigned int slot = 0; slot < 0x487 && used < 1800; slot++) {
        __try {
            if (present(manager, slot) == 0) continue;
            void* element = lookup(manager, static_cast<int>(slot), 0);
            if (!element) continue;
            uintptr_t vtable = *reinterpret_cast<uintptr_t*>(element);
            int w = _snprintf_s(line + used, sizeof(line) - used, _TRUNCATE,
                " %X:%08X", slot, static_cast<unsigned int>(vtable));
            if (w <= 0) break;
            used += w;
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
        }
    }
    LogLine("ui slot census %s:%s", tag, line[0] ? line : " (empty)");
}

void CloseExpertJobUiSlots(const char* source)
{
    if (!g_dnfBase) return;
    void* manager = ResolveUiManager();
    if (!manager) return;

    UiSlotPresentFn present = reinterpret_cast<UiSlotPresentFn>(
        g_dnfBase + kUiSlotPresentRva);
    UiSlotLookupFn lookup = reinterpret_cast<UiSlotLookupFn>(
        g_dnfBase + kUiSlotLookupRva);
    UiSlotNotifyFn notify = reinterpret_cast<UiSlotNotifyFn>(
        g_dnfBase + kUiSlotNotifyRva);

    for (unsigned int slot : kExpertJobWindowUiSlots) {
        __try {
            if (!present || present(manager, slot) == 0) continue;

            // Describe the object the client keeps putting back: its vtable names
            // the window class, whose own method is what actually hides it.
            if (g_expertJobWindowDescribeCount < 4 && lookup) {
                void* element = lookup(manager, static_cast<int>(slot), 0);
                if (element) {
                    InterlockedIncrement(&g_expertJobWindowDescribeCount);
                    uintptr_t vtable = *reinterpret_cast<uintptr_t*>(element);
                    LogLine("expert-job window element slot=0x%X ptr=%p vtable=0x%08X",
                        slot, element, static_cast<unsigned int>(vtable));
                    for (int i = 0; i < 16; i++) {
                        LogLine("expert-job window vtable[%02d]=0x%08X", i,
                            static_cast<unsigned int>(
                                *reinterpret_cast<uintptr_t*>(
                                    vtable + i * sizeof(void*))));
                    }
                }
            }

            if (notify) notify(manager, static_cast<int>(slot), -1, 1);
            const LONG n = InterlockedIncrement(&g_expertJobWindowCloseCount);
            LogLine("expert-job window closed slot=0x%X source=%s count=%d",
                slot, source ? source : "unknown", n);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            LogLine("expert-job window close exception slot=0x%X source=%s "
                "code=0x%08X", slot, source ? source : "unknown",
                GetExceptionCode());
        }
    }
}

// Called from the class0 dispatch hook on every packet while the close window is
// armed, so the client cannot simply rebuild the window a moment later.
void ServiceExpertJobWindowClose()
{
    const LONG until = InterlockedCompareExchange(
        &g_expertJobWindowCloseUntilTick, 0, 0);
    if (until == 0) return;
    const LONG now = static_cast<LONG>(GetTickCount());
    if (static_cast<LONG>(now - until) >= 0) {
        InterlockedExchange(&g_expertJobWindowCloseUntilTick, 0);
        return;
    }
    // A few censuses while the window is being (re)built, to catch the diff.
    const LONG n = InterlockedIncrement(&g_expertJobWindowCloseCount);
    if (n == 3 || n == 25 || n == 80) {
        CensusUiSlots("during-projection");
    }
    CloseExpertJobUiSlots("class0-dispatch");
}

int __stdcall ProxyTownCopresenceClass0Dispatch(int a1, int packet)
{
    // Report a fired op312 null guard before anything in this packet can return
    // early, so the signal cannot be lost behind the private-envelope paths.
    ReportDungeonEventMonsterConditionGuardHits();
    ReportTownEntrySceneAccessorNullGuardHits();
    if (TryConsumePrivateCombatPowerAffixes(packet)) return 1;
    if (TryConsumePrivateMoodProjection(packet)) return 1;
    if (TryConsumePrivateGuildRanking(packet)) return 1;
    if (TryConsumePrivateBossMutationDecision(packet)) return 1;

    TraceTownCopresenceDispatchState("before", packet);
    // 交易族（0x0E..0x13）需要 registry 里的处理器地址：TracedDispatchState 会用
    // class0 的 dispatch registry 反查 opcode 对应的 callback，日志里的
    // callback=+0x... 就是客户端【交易完成】处理器的地址，据此反汇编出交易结果
    // 弹窗（交易金额 / 手续费 / 获得金额）在 op18 包体里的精确字段偏移。
    // 只读日志，不改变任何行为。
    TraceDispatchState("before", 0, packet);
    unsigned int opcode = 0;
    unsigned int mode = 0xFF;
    unsigned int packetLength = 0;
    unsigned short objectKey = 0;
    unsigned char ownerContext = 0;
    unsigned int mode0BaseJob = 0;
    unsigned int mode0GrowType = 0;
    bool mode0ProfessionPresent = false;
    __try {
        if (packet) {
            const unsigned char* bytes = reinterpret_cast<const unsigned char*>(packet);
            opcode = *reinterpret_cast<const unsigned short*>(bytes + 1);
            packetLength = *reinterpret_cast<const unsigned int*>(bytes + 3);
            if (packetLength >= 16) {
                const unsigned char* body = bytes + 16;
                const size_t bodyLength = packetLength - 16;
                if ((opcode == 22 || opcode == 23 || opcode == 6) && bodyLength >= 2) {
                    objectKey = *reinterpret_cast<const unsigned short*>(body);
                } else if (opcode == 2 && bodyLength >= 5) {
                    mode = body[0];
                    ownerContext = body[4];
                    if (mode == 0 && bodyLength >= 0x4E) {
                        objectKey = *reinterpret_cast<const unsigned short*>(body + 0x4C);
                        constexpr size_t kMode0NameLengthOffset = 0x4E;
                        constexpr size_t kMode0NameDataOffset = 0x52;
                        if (bodyLength >= kMode0NameDataOffset) {
                            unsigned int nameLength = 0;
                            memcpy(&nameLength,
                                body + kMode0NameLengthOffset,
                                sizeof(nameLength));
                            if (nameLength <=
                                    bodyLength - kMode0NameDataOffset) {
                                size_t professionOffset =
                                    kMode0NameDataOffset + nameLength;
                                if (professionOffset <= bodyLength - 2) {
                                    mode0BaseJob = body[professionOffset];
                                    mode0GrowType = body[professionOffset + 1];
                                    mode0ProfessionPresent = true;
                                }
                            }
                        }
                    }
                }
            }
        }
    }
	__except (EXCEPTION_EXECUTE_HANDLER) {
        opcode = 0;
        mode = 0xFF;
        objectKey = 0;
		ownerContext = 0;
		mode0BaseJob = 0;
		mode0GrowType = 0;
		mode0ProfessionPresent = false;
	}
	if (opcode == kCharacterOptionRestoreOpcode &&
		packetLength == 16 + kCharacterOptionBodyLength) {
		ApplyCharacterOptionRestore(
			reinterpret_cast<const unsigned char*>(packet) + 16,
			kCharacterOptionBodyLength);
	}

	bool previousTraceOp24 = g_traceOp24;
    unsigned int previousSceneModeCalls = g_op24SceneModeCalls;
    unsigned int previousLoadingGateCalls = g_op24LoadingGateCalls;
    bool previousSceneModeEarlyReturn = g_op24SceneModeEarlyReturn;
    bool previousLoadingGateEarlyReturn = g_op24LoadingGateEarlyReturn;
    bool currentOp24 = opcode == 24;
    g_traceOp24 = currentOp24;
    if (currentOp24) {
        g_op24SceneModeCalls = 0;
        g_op24LoadingGateCalls = 0;
        g_op24SceneModeEarlyReturn = false;
        g_op24LoadingGateEarlyReturn = false;
    }

    bool previousLookupActive = g_townActorContextLookupActive;
    unsigned short previousLookupKey = g_townActorContextLookupKey;
    bool enableContextLookup =
        (opcode == 22 || opcode == 23) && objectKey != 0 &&
        g_townActorOwnerByObjectKey[objectKey] != 0;
    g_townActorContextLookupActive = enableContextLookup;
    g_townActorContextLookupKey = enableContextLookup ? objectKey : 0;

    unsigned char removeOwnerContext =
        opcode == 6 && objectKey != 0
        ? g_townActorOwnerByObjectKey[objectKey]
        : 0;
    if (removeOwnerContext != 0) {
        RemoveTownRemoteActorVisual(objectKey, removeOwnerContext);
        g_townActorOwnerByObjectKey[objectKey] = 0;
        g_townActorLookupLoggedByObjectKey[objectKey] = 0;
    }

    Class0DispatchFn original = reinterpret_cast<Class0DispatchFn>(
        g_originalClass0Dispatch);
    if (UiRegistryTrackingActive()) {
        UiRegistrySnapshot(opcode, packetLength);
    }
    ServiceExpertJobWindowClose();
    int result = original ? original(a1, packet) : 0;
    if (UiRegistryTrackingActive()) {
        UiRegistrySnapshot(opcode, packetLength);
    }
    ApplyRealtimeStoryDigestAfterNativeState(opcode, packetLength, result);
    if (opcode == kConsumableUseGateResetOpcode) {
        ApplyAuthoritativeConsumableUseGateReset("class0-op42-post-native");
    }
    // This is the active current-profile class0 entrypoint. Cache the already
    // accepted local mode1/mode3 stat blob here; ProxyClass0Dispatch belongs
    // to the fallback profile and is not installed alongside this hook.
    CacheCurrentCharacterStatsFromClass0Packet(packet);
    unsigned int currentSceneModeCalls = currentOp24
        ? g_op24SceneModeCalls
        : 0;
    unsigned int currentLoadingGateCalls = currentOp24
        ? g_op24LoadingGateCalls
        : 0;
    bool currentSceneModeEarlyReturn = currentOp24 &&
        g_op24SceneModeEarlyReturn;
    bool currentLoadingGateEarlyReturn = currentOp24 &&
        g_op24LoadingGateEarlyReturn;
    g_traceOp24 = previousTraceOp24;
    g_op24SceneModeCalls = previousSceneModeCalls;
    g_op24LoadingGateCalls = previousLoadingGateCalls;
    g_op24SceneModeEarlyReturn = previousSceneModeEarlyReturn;
    g_op24LoadingGateEarlyReturn = previousLoadingGateEarlyReturn;
    g_townActorContextLookupActive = previousLookupActive;
    g_townActorContextLookupKey = previousLookupKey;


    // No expert-job state projection: op205 reaches the native handler
    // untouched and the profession is displayed from the server's own
    // character record.  See the removal note in this file's installer area.

    if (opcode == 2 && mode == 0 && objectKey != 0 && ownerContext == 0) {
        RepairTownCurrentActor(objectKey);
    }
    if (opcode == 2 && mode == 0 && objectKey != 0) {
        ClaimLocalActorFromMode0Row(objectKey, ownerContext);
        if (mode0ProfessionPresent &&
            g_currentActorObjectKey == objectKey) {
            LONG previousJob = InterlockedExchange(
                &g_berserkerFrenzyCurrentBaseJob,
                static_cast<LONG>(mode0BaseJob));
            LONG previousGrow = InterlockedExchange(
                &g_berserkerFrenzyCurrentGrowType,
                static_cast<LONG>(mode0GrowType));
            if (previousJob != static_cast<LONG>(mode0BaseJob) ||
                previousGrow != static_cast<LONG>(mode0GrowType)) {
                LogLine("berserker Blood Frenzy local profession cached key=%u job=%u grow=0x%02X eligible=%u",
                    objectKey, mode0BaseJob, mode0GrowType,
                    mode0BaseJob == 0 &&
                        (mode0GrowType & 0x0Fu) == 0x03u ? 1u : 0u);
            }
        }
    }
    if (opcode == 2 && mode == 0 && objectKey != 0 && ownerContext != 0) {
        ActorByContextFn actorByContextFn = reinterpret_cast<ActorByContextFn>(
            g_dnfBase + kActorByContextRva);
        void* actor = actorByContextFn
            ? actorByContextFn(objectKey, static_cast<int>(ownerContext))
            : nullptr;
        bool globalActor = false;
        if (!actor) {
            ActorByObjectKeyFn originalActorByObjectKey =
                reinterpret_cast<ActorByObjectKeyFn>(g_originalActorByObjectKey);
            actor = originalActorByObjectKey
                ? originalActorByObjectKey(objectKey)
                : nullptr;
            globalActor = actor != nullptr;
        }
        if (actor) {
            if (!globalActor) {
                g_townActorOwnerByObjectKey[objectKey] = ownerContext;
                g_townActorLookupLoggedByObjectKey[objectKey] = 0;
            }
            LogLine("town co-presence actor registered key=%u owner=%u actor=%p global=%d",
                objectKey, ownerContext, actor, globalActor ? 1 : 0);
            EnsureTownRemoteActorVisual(objectKey, ownerContext, actor);
        }
    } else if (opcode == 6 && objectKey != 0) {
        g_townActorOwnerByObjectKey[objectKey] = 0;
        if (g_currentActorObjectKey == objectKey) {
            g_currentActorObjectKey = 0;
            InterlockedExchange(&g_berserkerFrenzyCurrentBaseJob, -1);
            InterlockedExchange(&g_berserkerFrenzyCurrentGrowType, -1);
        }
        LogLine("town co-presence actor context removed key=%u", objectKey);
    }

    TraceTownCopresenceDispatchState("after", packet);
    UpdateLuaDungeonEntryStateAfterDispatch(packet, opcode);
    if (currentOp24) {
        QueueLuaEnterTownEventIfAccepted(
            packet,
            currentSceneModeCalls,
            currentLoadingGateCalls,
            currentSceneModeEarlyReturn,
            currentLoadingGateEarlyReturn);
    }
    return result;
}

bool ReadEquipmentWireBytes(const unsigned char** cursor,
    const unsigned char* end, void* output, size_t size)
{
    if (!cursor || !*cursor || !end || *cursor > end ||
        size > static_cast<size_t>(end - *cursor)) {
        return false;
    }
    if (output && size != 0) memcpy(output, *cursor, size);
    *cursor += size;
    return true;
}

bool SkipEquipmentWireBytes(const unsigned char** cursor,
    const unsigned char* end, size_t size)
{
    return ReadEquipmentWireBytes(cursor, end, nullptr, size);
}

bool SkipEquipmentWireRawBlock(const unsigned char** cursor,
    const unsigned char* end)
{
    unsigned int length = 0;
    if (!ReadEquipmentWireBytes(cursor, end, &length, sizeof(length)) ||
        length > 1024) {
        return false;
    }
    return SkipEquipmentWireBytes(cursor, end, length);
}

bool IsCombatPowerEquipmentSlot(unsigned char actorSlot)
{
    // Current mode1/PVF evidence: 12 is weapon, 13 is title, 14..18 are
    // armor, 19..21 are accessories, 22/23 are support/magic stone, and 25
    // is earring. Slots 0..11 are avatar/aura objects; 26+ are creature,
    // medal, gems, and other independent systems.
    return (actorSlot >= 12 && actorSlot <= 23) || actorSlot == 25;
}

bool ParseCurrentEquipmentSnapshot(const unsigned char* body,
    size_t bodyLength, size_t statDataOffset,
    unsigned int sourceStatGeneration,
    DNF90EquipmentSnapshot* output)
{
    if (!body || !output) return false;

    constexpr size_t kCurrentActorStatWireBytes = 92;
    const size_t countOffset = statDataOffset +
        kCurrentActorStatWireBytes + 1; // extra-equipment-slot bitset
    if (countOffset >= bodyLength) return false;

    const unsigned char* cursor = body + countOffset;
    const unsigned char* end = body + bodyLength;
    unsigned char rowCount = 0;
    if (!ReadEquipmentWireBytes(&cursor, end, &rowCount,
            sizeof(rowCount)) ||
        rowCount > DNF90_EQUIPMENT_SNAPSHOT_MAX_ITEMS) {
        return false;
    }

    DNF90EquipmentSnapshot snapshot = {};
    snapshot.size = sizeof(snapshot);
    snapshot.sourceStatGeneration = sourceStatGeneration;
    snapshot.validFlags = DNF90_EQUIPMENT_SNAPSHOT_ROWS_VALID;
    snapshot.itemCount = rowCount;
    uint64_t seenSlots = 0;

    for (unsigned int index = 0; index < rowCount; ++index) {
        unsigned char actorSlot = 0;
        unsigned int itemID = 0;
        unsigned int qualitySeed = 0;
        unsigned char extData = 0;
        unsigned short durability = 0;
        unsigned int linkedItemID = 0;
        unsigned int auxiliaryValue = 0;
        unsigned char auxiliaryByte = 0;
        unsigned char amplifyType = 0;
        unsigned short amplifyValue = 0;
        if (!ReadEquipmentWireBytes(&cursor, end, &actorSlot,
                sizeof(actorSlot)) ||
            !ReadEquipmentWireBytes(&cursor, end, &itemID,
                sizeof(itemID)) ||
            !ReadEquipmentWireBytes(&cursor, end, &qualitySeed,
                sizeof(qualitySeed)) ||
            !ReadEquipmentWireBytes(&cursor, end, &extData,
                sizeof(extData)) ||
            !ReadEquipmentWireBytes(&cursor, end, &durability,
                sizeof(durability)) ||
            !ReadEquipmentWireBytes(&cursor, end, &linkedItemID,
                sizeof(linkedItemID)) ||
            actorSlot > 32 || itemID == 0 ||
            (seenSlots & (uint64_t(1) << actorSlot)) != 0) {
            return false;
        }
        seenSlots |= uint64_t(1) << actorSlot;

        if (linkedItemID != 0 && actorSlot == 9 &&
            !SkipEquipmentWireRawBlock(&cursor, end)) {
            return false;
        }
        if (!ReadEquipmentWireBytes(&cursor, end, &auxiliaryValue,
                sizeof(auxiliaryValue)) ||
            !ReadEquipmentWireBytes(&cursor, end, &auxiliaryByte,
                sizeof(auxiliaryByte)) ||
            !ReadEquipmentWireBytes(&cursor, end, &amplifyType,
                sizeof(amplifyType)) ||
            !ReadEquipmentWireBytes(&cursor, end, &amplifyValue,
                sizeof(amplifyValue))) {
            return false;
        }

        // Current item types 0..11 read the two avatar raw blocks; type 26
        // additionally reads a durability/instance override dword.
        if (actorSlot <= 0x0B &&
            (!SkipEquipmentWireRawBlock(&cursor, end) ||
                !SkipEquipmentWireRawBlock(&cursor, end))) {
            return false;
        }
        if (actorSlot == 26 &&
            !SkipEquipmentWireBytes(&cursor, end, sizeof(unsigned int))) {
            return false;
        }

        unsigned char vectorRecordCount = 0;
        unsigned char dwordVectorCount = 0;
        unsigned char indexedRecordCount = 0;
        if (!ReadEquipmentWireBytes(&cursor, end, &vectorRecordCount,
                sizeof(vectorRecordCount)) ||
            vectorRecordCount > 2 ||
            !SkipEquipmentWireBytes(&cursor, end,
                static_cast<size_t>(vectorRecordCount) * 8) ||
            !SkipEquipmentWireBytes(&cursor, end, sizeof(unsigned int)) ||
            !ReadEquipmentWireBytes(&cursor, end, &dwordVectorCount,
                sizeof(dwordVectorCount)) ||
            dwordVectorCount > 2 ||
            !SkipEquipmentWireBytes(&cursor, end,
                static_cast<size_t>(dwordVectorCount) * 4) ||
            !SkipEquipmentWireBytes(&cursor, end, sizeof(unsigned short)) ||
            !ReadEquipmentWireBytes(&cursor, end, &indexedRecordCount,
                sizeof(indexedRecordCount)) ||
            indexedRecordCount > 3 ||
            !SkipEquipmentWireBytes(&cursor, end,
                static_cast<size_t>(indexedRecordCount) * 3)) {
            return false;
        }
        if (indexedRecordCount != 0) {
            unsigned char activeValue = 0;
            unsigned char selectedIndex = 0;
            if (!ReadEquipmentWireBytes(&cursor, end, &activeValue,
                    sizeof(activeValue)) ||
                !ReadEquipmentWireBytes(&cursor, end, &selectedIndex,
                    sizeof(selectedIndex)) ||
                (selectedIndex != 0xFF &&
                    !SkipEquipmentWireBytes(&cursor, end, 4))) {
                return false;
            }
        }

        // state120/state160/state128/state140/state144/resource and
        // state168..170 are ten fixed bytes before the final raw blocks.
        if (!SkipEquipmentWireBytes(&cursor, end, 10) ||
            !SkipEquipmentWireRawBlock(&cursor, end) ||
            !SkipEquipmentWireRawBlock(&cursor, end) ||
            !SkipEquipmentWireBytes(&cursor, end, 1) ||
            !SkipEquipmentWireRawBlock(&cursor, end)) {
            return false;
        }

        DNF90EquippedItemSnapshot& item = snapshot.items[index];
        item.itemID = itemID;
        item.qualitySeed = qualitySeed;
        item.durability = durability;
        item.amplifyValue = amplifyValue;
        item.actorSlot = actorSlot;
        item.upgradeLevel = extData & 0x1F;
        item.amplifyType = amplifyType;
    }

    snapshot.generation = static_cast<unsigned int>(
        InterlockedIncrement(&g_equipmentSnapshotGeneration));
    *output = snapshot;
    return true;
}

void CacheCurrentCharacterStatsFromClass0Packet(int packet)
{
    if (!packet) return;

    __try {
        const unsigned char* bytes =
            reinterpret_cast<const unsigned char*>(packet);
        const unsigned int opcode =
            *reinterpret_cast<const unsigned short*>(bytes + 1);
        const unsigned int packetLength =
            *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (opcode != 2 || packetLength < 16 + 0x17) return;

        const unsigned char* body = bytes + 16;
        const unsigned int bodyLength = packetLength - 16;
        const unsigned char mode = body[0];
        size_t statLengthOffset = 0;
        size_t statDataOffset = 0;
        size_t objectKeyOffset = 0;
        if (mode == 1) {
            statLengthOffset = 0x1B;
            statDataOffset = 0x1F;
            objectKeyOffset = 0x15;
        } else if (mode == 3) {
            statLengthOffset = 0x13;
            statDataOffset = 0x17;
            objectKeyOffset = 0x0D;
        } else {
            return;
        }
        constexpr unsigned int kCurrentActorStatWireBytes = 92;
        if (bodyLength < statDataOffset + kCurrentActorStatWireBytes ||
            bodyLength < statLengthOffset + sizeof(unsigned int) ||
            bodyLength < objectKeyOffset + sizeof(unsigned short) ||
            *reinterpret_cast<const unsigned int*>(
                body + statLengthOffset) != kCurrentActorStatWireBytes) {
            return;
        }

        const unsigned short objectKey =
            *reinterpret_cast<const unsigned short*>(body + objectKeyOffset);
        if (objectKey == 0) return;

        // The current local actor can be installed by the context-aware town
        // route without populating g_currentActorObjectKey. Verify the exact
        // native actor pointer instead of rejecting that accepted mode1 row.
        bool currentActorMatch = objectKey == g_currentActorObjectKey;
        void* currentActor = nullptr;
        void* packetActor = nullptr;
        void* objectManager = *reinterpret_cast<void**>(
            g_dnfBase + kObjectManagerPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_dnfBase + kCurrentActorRva);
        if (objectManager && currentActorFn) {
            currentActor = currentActorFn(objectManager);
        }
        ActorByObjectKeyFn originalActorByObjectKey =
            reinterpret_cast<ActorByObjectKeyFn>(g_originalActorByObjectKey);
        if (originalActorByObjectKey) {
            packetActor = originalActorByObjectKey(objectKey);
        }
        if (!packetActor && bodyLength >= 5) {
            ActorByContextFn actorByContextFn =
                reinterpret_cast<ActorByContextFn>(
                    g_dnfBase + kActorByContextRva);
            if (actorByContextFn) {
                packetActor = actorByContextFn(
                    objectKey, static_cast<int>(body[4]));
            }
        }
        if (currentActor && packetActor == currentActor) {
            currentActorMatch = true;
            g_currentActorObjectKey = objectKey;
        }
        if (!currentActorMatch) {
            LONG rejection = InterlockedIncrement(
                &g_characterStatRejectLogCount);
            if (rejection <= 8) {
                LogLine("combat-power base snapshot ignored mode=%u key=%u "
                    "cached_key=%u current_actor=%p packet_actor=%p",
                    static_cast<unsigned int>(mode), objectKey,
                    static_cast<unsigned int>(g_currentActorObjectKey),
                    currentActor, packetActor);
            }
            return;
        }

        // 战斗力停用：到这里就结束。
        // 注意：上面的 g_currentActorObjectKey = objectKey 必须保留 —— 城镇共现注册、
        // 狂暴者光环的本地职业缓存与 actor 修复都读它；在那个赋值之前 return 会连带
        // 弄坏那些功能。这里只切掉快照缓存这一段。
        if (!kCombatPowerEnabled) {
            return;
        }

        const unsigned char* stat = body + statDataOffset;
        DNF90CharacterStatSnapshot snapshot = {};
        snapshot.size = sizeof(snapshot);
        snapshot.generation = static_cast<unsigned int>(
            InterlockedIncrement(&g_characterStatGeneration));
        snapshot.validFlags = DNF90_CHARACTER_STATS_BASE_VALID |
            DNF90_CHARACTER_STATS_SPEED_VALID;
        snapshot.hpMax = *reinterpret_cast<const unsigned int*>(stat + 0);
        snapshot.mpMax = *reinterpret_cast<const unsigned int*>(stat + 4);
        snapshot.strength =
            *reinterpret_cast<const short*>(stat + 8) / 10;
        snapshot.vitality =
            *reinterpret_cast<const short*>(stat + 10) / 10;
        snapshot.intelligence =
            *reinterpret_cast<const short*>(stat + 12) / 10;
        snapshot.spirit =
            *reinterpret_cast<const short*>(stat + 14) / 10;
        snapshot.moveSpeed =
            *reinterpret_cast<const unsigned int*>(stat + 68);
        snapshot.attackSpeed =
            *reinterpret_cast<const unsigned short*>(stat + 72);
        snapshot.castSpeed =
            *reinterpret_cast<const unsigned short*>(stat + 74);

        AcquireSRWLockExclusive(&g_characterStatSnapshotLock);
        g_characterStatSnapshot = snapshot;
        ReleaseSRWLockExclusive(&g_characterStatSnapshotLock);
        LogLine("combat-power base snapshot generation=%u mode=%u key=%u "
            "hp=%u mp=%u str=%d vit=%d int=%d spi=%d",
            snapshot.generation, static_cast<unsigned int>(mode), objectKey,
            snapshot.hpMax, snapshot.mpMax, snapshot.strength,
            snapshot.vitality, snapshot.intelligence, snapshot.spirit);

        DNF90EquipmentSnapshot equipment = {};
        if (!ParseCurrentEquipmentSnapshot(body, bodyLength, statDataOffset,
                snapshot.generation, &equipment)) {
            LONG rejection = InterlockedIncrement(
                &g_equipmentSnapshotRejectLogCount);
            if (rejection <= 8) {
                LogLine("combat-power equipment snapshot ignored mode=%u "
                    "key=%u body_len=%u stat_generation=%u",
                    static_cast<unsigned int>(mode), objectKey, bodyLength,
                    snapshot.generation);
            }
            return;
        }

        unsigned int combatItems = 0;
        unsigned int upgradedItems = 0;
        unsigned int amplifiedItems = 0;
        for (unsigned int index = 0; index < equipment.itemCount; ++index) {
            const DNF90EquippedItemSnapshot& item = equipment.items[index];
            if (!IsCombatPowerEquipmentSlot(item.actorSlot)) continue;
            ++combatItems;
            if (item.upgradeLevel != 0) ++upgradedItems;
            if (item.amplifyType >= 1 && item.amplifyType <= 4 &&
                item.amplifyValue != 0) {
                ++amplifiedItems;
            }
        }
        AcquireSRWLockExclusive(&g_equipmentSnapshotLock);
        g_equipmentSnapshot = equipment;
        ReleaseSRWLockExclusive(&g_equipmentSnapshotLock);
        LogLine("combat-power equipment snapshot generation=%u "
            "stat_generation=%u mode=%u key=%u rows=%u combat_items=%u "
            "upgraded_items=%u amplified_items=%u",
            equipment.generation, equipment.sourceStatGeneration,
            static_cast<unsigned int>(mode), objectKey,
            equipment.itemCount, combatItems, upgradedItems, amplifiedItems);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("combat-power base snapshot exception code=0x%08X",
            GetExceptionCode());
    }
}

int __stdcall ProxyClass0Dispatch(int a1, int packet)
{
    TraceDispatchState("before", 0, packet);
    bool previousTraceOp24 = g_traceOp24;
    unsigned int previousSceneModeCalls = g_op24SceneModeCalls;
    unsigned int previousLoadingGateCalls = g_op24LoadingGateCalls;
    __try {
        g_traceOp24 = packet && *reinterpret_cast<const unsigned short*>(packet + 1) == 24;
        if (g_traceOp24) {
            g_op24SceneModeCalls = 0;
            g_op24LoadingGateCalls = 0;
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        g_traceOp24 = false;
    }
    Class0DispatchFn original = reinterpret_cast<Class0DispatchFn>(g_originalClass0Dispatch);
    ServiceExpertJobWindowClose();
    int result = original ? original(a1, packet) : 0;
    g_traceOp24 = previousTraceOp24;
    g_op24SceneModeCalls = previousSceneModeCalls;
    g_op24LoadingGateCalls = previousLoadingGateCalls;

    // The native handler has already accepted and applied this packet. Cache
    // only the complete local mode1/mode3 stat block for the Lua calculator;
    // remote town actors and incomplete bodies are deliberately ignored.
    CacheCurrentCharacterStatsFromClass0Packet(packet);

    unsigned int opcode = 0;
    unsigned int packetLength = 0;
    unsigned int recordCount = 0;
    unsigned int scene = 0;
    unsigned int recordKind = 0;
    unsigned int recordObjectKey = 0;
    unsigned int recordOwnerContext = 0;
    unsigned int memberCount = 0;
    bool recordContainsSelfSlot = false;
    __try {
        if (packet) {
            const unsigned char* bytes = reinterpret_cast<const unsigned char*>(packet);
            opcode = *reinterpret_cast<const unsigned short*>(bytes + 1);
            packetLength = *reinterpret_cast<const unsigned int*>(bytes + 3);
            if (opcode == kPartyActorProjectionOpcode && packetLength >= 26) {
                recordCount = *reinterpret_cast<const unsigned short*>(bytes + 16);
                scene = *reinterpret_cast<const unsigned short*>(bytes + 18);
                recordObjectKey = *reinterpret_cast<const unsigned short*>(bytes + 20);
                recordOwnerContext = bytes[23];
                recordKind = bytes[25];

                // The current compatibility profile's kind-0 row contains an
                // outer dstr at body+12, 13 fixed bytes, then the party slots.
                // Every slot carries the cross-channel identity block before
                // its three state bytes. Walk the bounded row so a native
                // pointer repair is allowed only when this actor's own object
                // key is actually present in the received party table.
                const size_t bodyLength = packetLength - 16;
                const unsigned char* body = bytes + 16;
                if (recordKind == 0 && bodyLength >= 33) {
                    size_t offset = 12;
                    unsigned int outerNameLength =
                        *reinterpret_cast<const unsigned int*>(body + offset);
                    offset += 4;
                    if (outerNameLength <= bodyLength - offset) {
                        offset += outerNameLength;
                        if (offset <= bodyLength && bodyLength - offset >= 14) {
                            offset += 13;
                            memberCount = body[offset++];
                            if (memberCount <= 8) {
                                bool bounded = true;
                                for (unsigned int i = 0; i < memberCount; ++i) {
                                    if (bodyLength - offset < 10) {
                                        bounded = false;
                                        break;
                                    }
                                    offset += 1;
                                    unsigned int memberObjectKey =
                                        *reinterpret_cast<const unsigned short*>(body + offset);
                                    offset += 2;
                                    offset += 4;
                                    unsigned int memberNameLength =
                                        *reinterpret_cast<const unsigned int*>(body + offset);
                                    offset += 4;
                                    if (memberNameLength > bodyLength - offset ||
                                        bodyLength - offset - memberNameLength < 3) {
                                        bounded = false;
                                        break;
                                    }
                                    offset += memberNameLength;
                                    offset += 3;
                                    if (memberObjectKey == recordObjectKey) {
                                        recordContainsSelfSlot = true;
                                    }
                                }
                                if (!bounded) {
                                    memberCount = 0;
                                    recordContainsSelfSlot = false;
                                }
                            }
                        }
                    }
                }
            }
        }

        if (opcode == kPartyActorProjectionOpcode &&
            recordCount != 0 &&
            scene == kPartyActorProjectionScene &&
            recordKind == 0 &&
            InterlockedCompareExchange(&g_partyHudRefreshPending, 0, 1) == 1) {
            uintptr_t objectManager =
                *reinterpret_cast<uintptr_t*>(g_dnfBase + kObjectManagerPointerRva);
            uintptr_t sceneActorManager =
                *reinterpret_cast<uintptr_t*>(
                    g_dnfBase + kSceneActorManagerPointerRva);
            CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
                g_dnfBase + kCurrentActorRva);
            ActorByObjectKeyFn actorByObjectKeyFn = reinterpret_cast<ActorByObjectKeyFn>(
                g_dnfBase + kActorByObjectKeyRva);
            ActorByContextFn actorByContextFn = reinterpret_cast<ActorByContextFn>(
                g_dnfBase + kActorByContextRva);
            ResolveActorInContextFn resolveActorInContextFn =
                reinterpret_cast<ResolveActorInContextFn>(
                    g_dnfBase + kResolveActorInContextRva);
            uintptr_t currentActor = objectManager && currentActorFn
                ? reinterpret_cast<uintptr_t>(
                    currentActorFn(reinterpret_cast<void*>(objectManager)))
                : 0;
            uintptr_t localMemberActor = recordObjectKey && actorByObjectKeyFn
                ? reinterpret_cast<uintptr_t>(
                    actorByObjectKeyFn(static_cast<unsigned short>(recordObjectKey)))
                : 0;
            uintptr_t contextMemberActor =
                recordObjectKey && actorByContextFn
                ? reinterpret_cast<uintptr_t>(
                    actorByContextFn(
                        static_cast<unsigned short>(recordObjectKey),
                        static_cast<int>(recordOwnerContext)))
                : 0;
            uintptr_t recordActor =
                sceneActorManager && recordObjectKey && resolveActorInContextFn
                ? reinterpret_cast<uintptr_t>(
                    resolveActorInContextFn(
                        reinterpret_cast<void*>(sceneActorManager),
                        static_cast<unsigned char>(recordOwnerContext),
                        static_cast<unsigned short>(recordObjectKey)))
                : 0;
            uintptr_t contextPartyOwnerBefore = contextMemberActor
                ? *reinterpret_cast<uintptr_t*>(
                    contextMemberActor + kActorPartyOwnerOffset)
                : 0;
            uintptr_t hudPartyOwnerBefore = currentActor
                ? *reinterpret_cast<uintptr_t*>(
                    currentActor + kActorPartyOwnerOffset)
                : 0;
            bool repairedContextPartyOwner = false;
            if (currentActor &&
                localMemberActor == currentActor &&
                contextMemberActor &&
                recordActor &&
                recordContainsSelfSlot &&
                contextPartyOwnerBefore == 0) {
                // sub_269EE50's official invariant is memberActor+0x498 =
                // the outer party record resolved by sub_26A2000(ownerContext,
                // objectKey). With this profile's context-0 scene record and a
                // nonzero selected channel, the native function resolves that
                // member through sub_17B6AA0(objectKey, ownerContext), not
                // sub_2699A30. Restore that exact validated self-member target.
                *reinterpret_cast<uintptr_t*>(
                    contextMemberActor + kActorPartyOwnerOffset) = recordActor;
                repairedContextPartyOwner = true;
            }
            bool repairedHudPartyOwner = false;
            hudPartyOwnerBefore = currentActor
                ? *reinterpret_cast<uintptr_t*>(
                    currentActor + kActorPartyOwnerOffset)
                : 0;
            if (currentActor &&
                localMemberActor == currentActor &&
                recordActor &&
                recordContainsSelfSlot &&
                hudPartyOwnerBefore == 0) {
                // The context-0 compatibility remap keeps the visible local
                // actor in sub_2699A30 as well. Its HUD state-1 path reads the
                // same +0x498 field, so preserve that existing compatibility
                // binding when the exact context member is a separate object.
                *reinterpret_cast<uintptr_t*>(
                    currentActor + kActorPartyOwnerOffset) = recordActor;
                repairedHudPartyOwner = true;
            }
            uintptr_t contextPartyOwnerAfter = contextMemberActor
                ? *reinterpret_cast<uintptr_t*>(
                    contextMemberActor + kActorPartyOwnerOffset)
                : 0;
            uintptr_t hudPartyOwnerAfter = currentActor
                ? *reinterpret_cast<uintptr_t*>(
                    currentActor + kActorPartyOwnerOffset)
                : 0;
            LogLine("party-hud actor bind opcode=%u packet_len=%u "
                "record_key=%u owner_context=%u members=%u self_slot=%u "
                "current_actor=%p local_member_actor=%p "
                "context_member_actor=%p record_actor=%p "
                "context_owner_before=%p context_owner_after=%p "
                "hud_owner_before=%p hud_owner_after=%p "
                "context_repaired=%u hud_repaired=%u",
                opcode, packetLength, recordObjectKey, recordOwnerContext,
                memberCount,
                recordContainsSelfSlot ? 1u : 0u,
                reinterpret_cast<void*>(currentActor),
                reinterpret_cast<void*>(localMemberActor),
                reinterpret_cast<void*>(contextMemberActor),
                reinterpret_cast<void*>(recordActor),
                reinterpret_cast<void*>(contextPartyOwnerBefore),
                reinterpret_cast<void*>(contextPartyOwnerAfter),
                reinterpret_cast<void*>(hudPartyOwnerBefore),
                reinterpret_cast<void*>(hudPartyOwnerAfter),
                repairedContextPartyOwner ? 1u : 0u,
                repairedHudPartyOwner ? 1u : 0u);

            uintptr_t sceneRoot =
                *reinterpret_cast<uintptr_t*>(g_dnfBase + kSceneRootPointerRva);
            SceneHudContextFn contextFn = reinterpret_cast<SceneHudContextFn>(
                g_dnfBase + kSceneHudContextRva);
            PartyHudRefreshFn refreshFn = reinterpret_cast<PartyHudRefreshFn>(
                g_dnfBase + kPartyHudRefreshRva);
            void* hudContext = sceneRoot && contextFn
                ? contextFn(reinterpret_cast<void*>(sceneRoot))
                : nullptr;
            if (hudContext && refreshFn) {
                refreshFn(hudContext, 1);
                LONG hit = InterlockedIncrement(&g_partyHudRefreshApplyCount);
                LogLine("party-hud native refresh applied hit=%ld opcode=%u "
                    "packet_len=%u records=%u scene=%u kind=%u "
                    "source=successful-class1-op12-next-class0-op9",
                    hit, opcode, packetLength, recordCount, scene, recordKind);
            } else {
                InterlockedExchange(&g_partyHudRefreshPending, 1);
                LogLine("party-hud native refresh deferred opcode=%u "
                    "packet_len=%u scene_root=%p hud_context=%p",
                    opcode, packetLength, reinterpret_cast<void*>(sceneRoot),
                    hudContext);
            }
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        InterlockedExchange(&g_partyHudRefreshPending, 1);
        LogLine("party-hud native refresh exception opcode=%u packet_len=%u "
            "records=%u scene=%u kind=%u code=0x%08X pending_restored=1",
            opcode, packetLength, recordCount, scene, recordKind,
            GetExceptionCode());
    }

    // Native op24 has finished rebuilding the selected town actor and the
    // current EXE's response registries are now stable. Announce the complete
    // class-1 callback bitmap on every town/reconnect op24 so the new server
    // session can reject unsupported interactive requests without guessing.
    if (opcode == 24) {
        SendCurrentCommandResponseCapabilities();
    }

    TraceDispatchState("after", 0, packet);
    return result;
}

void CachePartyHudRefreshFromClass1Packet(int packet)
{
    if (!packet || !g_dnfBase) return;

    unsigned int opcode = 0;
    unsigned int packetLength = 0;
    unsigned int success = 0;
    __try {
        const unsigned char* bytes = reinterpret_cast<const unsigned char*>(packet);
        opcode = *reinterpret_cast<const unsigned short*>(bytes + 1);
        if (opcode != kSetPartyInfoOpcode) return;

        packetLength = *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (packetLength < 17) {
            LogLine("party-hud refresh ACK rejected opcode=%u packet_len=%u "
                "reason=short-packet",
                opcode, packetLength);
            return;
        }

        success = bytes[16];
        if (success != 1) {
            InterlockedExchange(&g_partyHudRefreshPending, 0);
            LogLine("party-hud refresh ACK rejected opcode=%u packet_len=%u "
                "success=%u reason=server-rejected-set-party-info",
                opcode, packetLength, success);
            return;
        }

        InterlockedExchange(&g_partyHudRefreshPending, 1);
        LogLine("party-hud refresh armed opcode=%u packet_len=%u success=%u "
            "target=next-class0-op9",
            opcode, packetLength, success);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        InterlockedExchange(&g_partyHudRefreshPending, 0);
        LogLine("party-hud refresh ACK cache exception opcode=%u packet_len=%u "
            "success=%u code=0x%08X",
            opcode, packetLength, success, GetExceptionCode());
    }
}

bool ApplyPersistentAuraSkinState(const char* source)
{
    LONG entitlement = InterlockedCompareExchange(
        &g_auraSkinEntitlementState, 0, 0);
    if (!g_dnfBase || entitlement < 0) return false;

    uintptr_t objectManager = 0;
    void* actor = nullptr;
    void* stateOwner = nullptr;
    void* stateBlock = nullptr;
    LONG previous = 0;
    LONG updated = 0;
    __try {
        objectManager = *reinterpret_cast<uintptr_t*>(
            g_dnfBase + kObjectManagerPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_dnfBase + kCurrentActorRva);
        actor = objectManager && currentActorFn
            ? currentActorFn(reinterpret_cast<void*>(objectManager))
            : nullptr;
        if (!actor) return false;

        stateOwner = *reinterpret_cast<void**>(
            reinterpret_cast<uintptr_t>(actor) +
            kAuraSkinActorStateOwnerOffset);
        if (!stateOwner) return false;
        uintptr_t vtable = *reinterpret_cast<uintptr_t*>(stateOwner);
        if (!vtable) return false;
        AuraSkinStateResolverFn resolveState =
            *reinterpret_cast<AuraSkinStateResolverFn*>(
                vtable + kAuraSkinStateResolverVtableOffset);
        stateBlock = resolveState ? resolveState(stateOwner) : nullptr;
        if (!stateBlock) return false;

        volatile LONG* flags = reinterpret_cast<volatile LONG*>(
            reinterpret_cast<uintptr_t>(stateBlock) +
            kAuraSkinStateFlagsOffset);
        previous = InterlockedCompareExchange(flags, 0, 0);
        for (;;) {
            updated = entitlement != 0
                ? previous | static_cast<LONG>(kAuraSkinUnlockedMask)
                : previous & ~static_cast<LONG>(kAuraSkinUnlockedMask);
            LONG observed = InterlockedCompareExchange(flags, updated, previous);
            if (observed == previous) break;
            previous = observed;
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("aura-skin persistent state exception state=%ld source=%s "
            "actor=%p owner=%p block=%p code=0x%08X",
            entitlement, source ? source : "unknown", actor, stateOwner,
            stateBlock, GetExceptionCode());
        return false;
    }

    LONG hit = InterlockedIncrement(&g_auraSkinDeferredApplyCount);
    LogLine("aura-skin persistent state applied hit=%ld state=%ld changed=%d "
        "previous=0x%08X updated=0x%08X source=%s actor=%p owner=%p block=%p",
        hit, entitlement, previous != updated ? 1 : 0,
        static_cast<unsigned int>(previous),
        static_cast<unsigned int>(updated),
        source ? source : "unknown", actor, stateOwner, stateBlock);
    return true;
}

bool CacheAuraSkinPersistentStateFromClass1Packet(int packet)
{
    if (!packet || !g_dnfBase) return false;

    unsigned int opcode = 0;
    unsigned int packetLength = 0;
    unsigned int success = 0;
    __try {
        const unsigned char* bytes = reinterpret_cast<const unsigned char*>(packet);
        opcode = *reinterpret_cast<const unsigned short*>(bytes + 1);
        if (opcode != kAuraSkinSlotOpcode) return false;

        packetLength = *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (packetLength != kAuraSkinSilentRestorePacketLength) return false;

        success = bytes[16];
        if (success > 1 ||
            bytes[17] != 'A' || bytes[18] != 'U' ||
            bytes[19] != 'R' || bytes[20] != 'A') {
            return false;
        }

        InterlockedExchange(&g_auraSkinEntitlementState,
            static_cast<LONG>(success));
        LONG hit = InterlockedIncrement(&g_auraSkinSilentRestoreCount);
        bool applied = ApplyPersistentAuraSkinState(
            "class1-op863-marked-state");
        LogLine("aura-skin persistent state cached hit=%ld opcode=%u "
            "packet_len=%u state=%u applied=%d source=class1-op863-marker",
            hit, opcode, packetLength, success, applied ? 1 : 0);
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("aura-skin persistent state cache exception opcode=%u "
            "packet_len=%u state=%u code=0x%08X",
            opcode, packetLength, success, GetExceptionCode());
        return false;
    }
}

bool TryConsumeNewbieGuideSeenSync(int packet)
{
    if (!packet) return false;
    __try {
        const unsigned char* bytes =
            reinterpret_cast<const unsigned char*>(packet);
        const unsigned short opcode =
            *reinterpret_cast<const unsigned short*>(bytes + 1);
        if (opcode != kNewbieGuideOptionOpcode) return false;
        const unsigned int packetLength =
            *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (packetLength != 16 + 4) return false;
        const unsigned int guideIndex =
            *reinterpret_cast<const unsigned int*>(bytes + 16);
        if (guideIndex == kNewbieGuideSeenReset) {
            memset(g_newbieGuideSeen, 0, sizeof(g_newbieGuideSeen));
            InterlockedExchange(&g_newbieGuideSeenCount, 0);
            InterlockedExchange(&g_newbieGuideLastSuppressed, -1);
            LogLine("newbie-guide seen sync reset source=class1-op1431");
            return true;
        }
        if (guideIndex > kNewbieGuideMaxIndex) {
            LogLine("newbie-guide seen sync ignored index=%u reason=out-of-range",
                guideIndex);
            return true;
        }
        if (!g_newbieGuideSeen[guideIndex]) {
            g_newbieGuideSeen[guideIndex] = 1;
            LONG count = InterlockedIncrement(&g_newbieGuideSeenCount);
            LogLine("newbie-guide seen sync index=%u count=%ld source=class1-op1431",
                guideIndex, count);
        }
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("newbie-guide seen sync exception type=1431 code=0x%08X",
            GetExceptionCode());
        return true;
    }
}

typedef int (__thiscall* NewbieGuideShowFn)(
    void* manager, unsigned int guideIndex, int arg1, int arg2);

int __fastcall ProxyNewbieGuideShow(
    void* manager, void* unused, unsigned int guideIndex, int arg1, int arg2)
{
    (void)unused;
    if (guideIndex <= kNewbieGuideMaxIndex &&
        g_newbieGuideSeen[guideIndex]) {
        if (InterlockedExchange(
                &g_newbieGuideLastSuppressed,
                static_cast<LONG>(guideIndex)) !=
            static_cast<LONG>(guideIndex)) {
            LogLine("newbie-guide display suppressed index=%u source=native-show-hook",
                guideIndex);
        }
        return 0;
    }
    NewbieGuideShowFn original =
        reinterpret_cast<NewbieGuideShowFn>(g_originalNewbieGuideShow);
    return original ? original(manager, guideIndex, arg1, arg2) : 0;
}

bool InstallNewbieGuideSeenSuppression()
{
    if (!g_dnfBase || g_originalNewbieGuideShow) return false;
    static const unsigned char kShowPrologue[] = {
        0x55, 0x8B, 0xEC, 0x56, 0x8B, 0xF1, 0x83, 0x7E, 0x50, 0x00
    };
    uintptr_t target = g_dnfBase + kNewbieGuideShowRva;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
            kShowPrologue, sizeof(kShowPrologue))) {
        LogLine("newbie-guide show hook target mismatch target=0x%08X",
            target);
        return false;
    }
    void* original = nullptr;
    if (!InstallKnownInlineHook(target, kShowPrologue,
            sizeof(kShowPrologue),
            reinterpret_cast<void*>(&ProxyNewbieGuideShow), &original,
            "newbie-guide account seen suppression")) {
        return false;
    }
    g_originalNewbieGuideShow = original;
    LogLine("newbie-guide show hook installed target=0x%08X trampoline=%p",
        target, original);
    return true;
}

int __stdcall ProxyClass1Dispatch(int packet, int a2, int a3)
{
    TraceDispatchState("before", 1, packet);
    if (TryConsumeNewbieGuideSeenSync(packet)) {
        TraceDispatchState("after-newbie-guide-seen-sync", 1, packet);
        return 1;
    }
    if (CacheAuraSkinPersistentStateFromClass1Packet(packet)) {
        TraceDispatchState("after-persistent-aura-state", 1, packet);
        return 1;
    }
    // 公会战 refusals (codes 240..255) carry no native UI; open the native
    // announcement window instead of letting the native path discard them.
    if (TryConsumeGuildWarErrorNotice(packet)) {
        TraceDispatchState("after-guild-war-error", 1, packet);
        return 1;
    }
    Class1DispatchFn original = reinterpret_cast<Class1DispatchFn>(g_originalClass1Dispatch);
    int result = original ? original(packet, a2, a3) : 0;
    ArmRealtimeStoryDigestFromQuestFinish(packet, result);
    TraceDispatchState("after", 1, packet);
    return result;
}

bool SendCurrentPartyDirectoryRefresh()
{
    if (!g_dnfBase) return false;
    __try {
        UpperWriterValueFn value = reinterpret_cast<UpperWriterValueFn>(
            g_dnfBase + kUpperWriterValueRva);
        UpperWriterScalarFn writeU16 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU16Rva);
        UpperWriterScalarFn writeU8 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU8Rva);
        UpperWriterFlushFn flush = reinterpret_cast<UpperWriterFlushFn>(
            g_dnfBase + kUpperWriterFlushRva);

        // Exact current-EXE writer sequence at 0x032289E4:
        // upper_u16(98), upper_u8(0), upper_flush().
        writeU16(value(), kPartyDirectoryRefreshOpcode);
        writeU8(value(), kPartyDirectoryRefreshModeFull);
        flush();
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("party-directory refresh request exception opcode=%u mode=%u code=0x%08X",
            kPartyDirectoryRefreshOpcode,
            static_cast<unsigned int>(kPartyDirectoryRefreshModeFull),
            GetExceptionCode());
        return false;
    }
    LogLine("party-directory refresh request sent opcode=%u mode=%u source=owner9-open",
        kPartyDirectoryRefreshOpcode,
        static_cast<unsigned int>(kPartyDirectoryRefreshModeFull));
    return true;
}

bool __fastcall ProxyJoustSceneBlockCheck(void* self, void* /*unused*/)
{
    JoustSceneBlockCheckFn original = reinterpret_cast<JoustSceneBlockCheckFn>(
        g_originalJoustSceneBlockCheck);
    bool blocked = original ? original(self) : true;
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    uintptr_t callerRva = caller >= g_dnfBase ? caller - g_dnfBase : 0;
    if (callerRva != kJoustOpenGateReturnRva) {
        return blocked;
    }

    // The owner-609 gate itself performs the remaining native checks: it
    // requests op1291, verifies the type-73 activity state, constructs the
    // native controls, and only then lets sub_2FDD1A0 promote the owner.
    LogLine("joust scene-state gate caller=+0x%08X native_blocked=%d override=allow owner=%u",
        static_cast<unsigned int>(callerRva), blocked ? 1 : 0,
        static_cast<unsigned int>(kJoustUiOwner));
    return false;
}

bool __fastcall ProxyActivityDateEntryExists(
    void* self, void* /*unused*/, int entryID)
{
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    uintptr_t callerRva = caller >= g_dnfBase ? caller - g_dnfBase : 0;
    const bool naenRaidShop =
        entryID == kNaenRaidShopActivityDateEntry &&
        (callerRva == kNaenRaidShopNpcGateReturnRva ||
         callerRva == kNaenRaidShopButtonGateReturnRva);
    const bool erikaRaidShop =
        entryID == kErikaRaidShopActivityDateEntry &&
        (callerRva == kErikaRaidShopNpcGateReturnRva ||
         callerRva == kErikaRaidShopButtonGateReturnRva);
    if (naenRaidShop || erikaRaidShop) {
        LogLine("%s raid-shop retired date gate caller=+0x%08X entry=%d override=active",
            naenRaidShop ? "Naen" : "Erika",
            static_cast<unsigned int>(callerRva), entryID);
        return true;
    }

    ActivityDateEntryExistsFn original =
        reinterpret_cast<ActivityDateEntryExistsFn>(
            g_originalActivityDateEntryExists);
    return original ? original(self, entryID) : false;
}

// ClosePartyTeleportConsentDialog closes dialog 0x1F4 through the client's own
// dialog manager, exactly as its 377 reader does (push 0, -1, 0x1F4 and call the
// close at VA 0x02FD85D0).
void ClosePartyTeleportConsentDialog()
{
    if (!g_dnfBase) return;
    __try {
        uintptr_t manager = *reinterpret_cast<uintptr_t*>(
            g_dnfBase + kDialogManagerPointerRva);
        if (!manager) return;
        typedef int(__fastcall* PartyTeleportDialogCloseFn)(
            void*, void*, int, int, int);
        PartyTeleportDialogCloseFn close =
            reinterpret_cast<PartyTeleportDialogCloseFn>(
                g_dnfBase + kPartyTeleportDialogCloseRva);
        if (!close) return;
        close(reinterpret_cast<void*>(manager), nullptr,
            kPartyTeleportDialogID, -1, 0);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("party teleport consent close exception code=0x%08X",
            GetExceptionCode());
    }
}

typedef int(__fastcall* PartyTeleportDialogStateSetFn)(
    void*, void*, int, int);
PartyTeleportDialogStateSetFn g_originalPartyTeleportDialogStateSet = nullptr;

int __fastcall ProxyPartyTeleportDialogStateSet(
    void* self, void* /*unused*/, int phase, int extra)
{
    PartyTeleportDialogStateSetFn original =
        g_originalPartyTeleportDialogStateSet;
    int result = original ? original(self, nullptr, phase, extra) : 0;
    if (phase == kPartyTeleportEndedPhase) {
        // The server only publishes this phase once every member accepted and the
        // party has already been moved, so the prompt is done.
        LogLine("party teleport consent request ended phase=%d extra=%d closing dialog",
            phase, extra);
        ClosePartyTeleportConsentDialog();
    }
    return result;
}

void* __fastcall ProxyAuraSkinSceneUiOpen(    void* self, void* /*unused*/, uintptr_t uiOwner, int mode, unsigned int group)
{
    if (uiOwner == kAvatarPanelUiOwner) {
        ApplyPersistentAuraSkinState("avatar-panel-before-create");
    }
    SceneUiOpenFn original = reinterpret_cast<SceneUiOpenFn>(g_originalSceneUiOpen);
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    void* result = original ? original(self, uiOwner, mode, group) : nullptr;
    if (uiOwner == kJoustUiOwner) {
        SceneUiIsOpenFn isOpen = g_dnfBase
            ? reinterpret_cast<SceneUiIsOpenFn>(g_dnfBase + kSceneUiIsOpenRva)
            : nullptr;
        bool active = false;
        __try {
            active = isOpen && isOpen(self, uiOwner);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            LogLine("joust scene-ui state read exception code=0x%08X",
                GetExceptionCode());
        }
        LogLine("joust scene-ui open caller=+0x%08X owner=%u mode=%d group=%u result=%p active=%d",
            caller >= g_dnfBase
                ? static_cast<unsigned int>(caller - g_dnfBase)
                : 0,
            static_cast<unsigned int>(uiOwner), mode, group, result,
            active ? 1 : 0);
    }
    if (!result) {
        return result;
    }

    if (uiOwner == kPartyDirectoryUiOwner) {
        SendCurrentPartyDirectoryRefresh();
    }
    return result;
}

void* __fastcall ProxySceneUiOpen(
    void* self, void* /*unused*/, uintptr_t uiOwner, int mode, unsigned int group)
{
    if (uiOwner == 0x298) {
        uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
        char preview[192] = { 0 };
        unsigned int previewLength = 0;
        unsigned int previewError = 0;
        __try {
            const wchar_t* text = reinterpret_cast<const wchar_t*>(
                static_cast<uintptr_t>(static_cast<unsigned int>(mode)));
            wchar_t widePreview[64] = { 0 };
            while (text && previewLength + 1 < _countof(widePreview) &&
                text[previewLength] != L'\0') {
                widePreview[previewLength] = text[previewLength];
                ++previewLength;
            }
            if (previewLength != 0) {
                WideCharToMultiByte(
                    CP_UTF8, 0, widePreview, static_cast<int>(previewLength),
                    preview, static_cast<int>(sizeof(preview) - 1),
                    nullptr, nullptr);
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            previewError = GetExceptionCode();
        }
        LogLine("scene-ui announcement open caller=+0x%08X owner=%u "
            "text_ptr=%p text_len=%u text=%s group=%u read_error=0x%08X",
            caller >= g_dnfBase
                ? static_cast<unsigned int>(caller - g_dnfBase)
                : 0,
            static_cast<unsigned int>(uiOwner),
            reinterpret_cast<void*>(
                static_cast<uintptr_t>(static_cast<unsigned int>(mode))),
            previewLength, preview, group, previewError);
    }

    if (uiOwner == kAvatarPanelUiOwner) {
        ApplyPersistentAuraSkinState("avatar-panel-before-create-debug");
    }
    SceneUiOpenFn original = reinterpret_cast<SceneUiOpenFn>(g_originalSceneUiOpen);
    void* result = original ? original(self, uiOwner, mode, group) : nullptr;
    return result;
}

int __cdecl ProxyLocalActorCreate(int objectKey)
{
    LogLine("local-actor-create phase=before object_key=%u", static_cast<unsigned int>(objectKey) & 0xFFFFu);
    LocalActorCreateFn original = reinterpret_cast<LocalActorCreateFn>(g_originalLocalActorCreate);
    int result = original ? original(objectKey) : 0;

    uintptr_t objectManager = 0;
    uintptr_t currentActor = 0;
    uintptr_t controlledActor = 0;
    __try {
        objectManager = *reinterpret_cast<uintptr_t*>(g_dnfBase + kObjectManagerPointerRva);
        controlledActor = *reinterpret_cast<uintptr_t*>(g_dnfBase + kControlledActorPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(g_dnfBase + kCurrentActorRva);
        currentActor = objectManager && currentActorFn
            ? reinterpret_cast<uintptr_t>(currentActorFn(reinterpret_cast<void*>(objectManager)))
            : 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("local-actor-create state read exception object_key=%u code=0x%08X",
            static_cast<unsigned int>(objectKey) & 0xFFFFu, GetExceptionCode());
    }

    LogLine("local-actor-create phase=after object_key=%u result=%p current_actor=%p controlled_actor=%p",
        static_cast<unsigned int>(objectKey) & 0xFFFFu, reinterpret_cast<void*>(result),
        reinterpret_cast<void*>(currentActor), reinterpret_cast<void*>(controlledActor));
    if (result && objectKey > 0 && objectKey <= 0xFFFF) {
        ClaimLocalActorFromMode0Row(
            static_cast<unsigned short>(objectKey), 0);
    }
    return result;
}

void __fastcall ProxyLocalActorBindingSetter(
    void* self, void* /*unused*/, void* value)
{
    if (!self) {
        LogLine("local actor optional binding skipped null receiver caller=+0x%08X value=%p",
            static_cast<unsigned int>(
                reinterpret_cast<uintptr_t>(_ReturnAddress()) - g_dnfBase),
            value);
        return;
    }

    LocalActorBindingSetterFn original =
        reinterpret_cast<LocalActorBindingSetterFn>(
            g_originalLocalActorBindingSetter);
    if (original) {
        original(self, value);
    }
}

void SyncDungeonStartMapDerivedFogState(const char* source)
{
    if (!g_dnfBase) return;
    __try {
        volatile unsigned char* direct = reinterpret_cast<volatile unsigned char*>(
            g_dnfBase + kDungeonHellPartyFogFlagRva);
        volatile unsigned char* derivedA = reinterpret_cast<volatile unsigned char*>(
            g_dnfBase + kDungeonDerivedFogStateARva);
        volatile unsigned char* derivedB = reinterpret_cast<volatile unsigned char*>(
            g_dnfBase + kDungeonDerivedFogStateBRva);
        const unsigned char value = *direct ? 1 : 0;
        const unsigned char beforeA = *derivedA;
        const unsigned char beforeB = *derivedB;
        *derivedA = value;
        *derivedB = value;
        if (beforeA != value || beforeB != value) {
            LogLine("dungeon start-map derived fog state synchronized source=%s direct=%u before=%u/%u after=%u/%u",
                source ? source : "unknown", value, beforeA, beforeB,
                *derivedA, *derivedB);
        }
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("dungeon start-map derived fog state synchronization fault source=%s code=0x%08X",
            source ? source : "unknown", GetExceptionCode());
    }
}

int __cdecl ProxyDungeonStartMapHandler()
{
    DungeonStartMapHandlerFn original =
        reinterpret_cast<DungeonStartMapHandlerFn>(g_originalDungeonStartMapHandler);
    const int result = original ? original() : 0;
    SyncDungeonStartMapDerivedFogState("op29");
    return result;
}

int __cdecl ProxyBloodAltarStartMapHandler()
{
    DungeonStartMapHandlerFn original =
        reinterpret_cast<DungeonStartMapHandlerFn>(g_originalBloodAltarStartMapHandler);
    const int result = original ? original() : 0;
    SyncDungeonStartMapDerivedFogState("op211");
    return result;
}

void __cdecl TraceMode0OwnerCompare(unsigned int sceneChannel, unsigned int localOwnerChannel)
{
    bool currentSceneContextRemap =
        sceneChannel != 0 && localOwnerChannel == 0;
    LogLine("mode0-owner-compare scene_channel=%u local_owner_channel=%u branch=%s",
        sceneChannel, localOwnerChannel,
        sceneChannel == localOwnerChannel || currentSceneContextRemap
            ? (currentSceneContextRemap ? "current-scene-context0-remap" : "current-scene-create")
            : "remote-actor");
}

// The local bridge's context-0 actor rows belong to the currently selected
// scene. Once the real HUD channel index is restored, translate only that
// established context-0 sentinel to the current scene branch. Every nonzero
// owner context retains the native CMP/JZ result.
__declspec(naked) void ProxyMode0OwnerCompare()
{
    __asm {
        pushfd
        pushad
        push ecx
        push esi
        call TraceMode0OwnerCompare
        add esp, 8
        popad
        popfd

        cmp esi, ecx
        je mode0_owner_local
        test ecx, ecx
        jnz mode0_owner_remote
        test esi, esi
        jnz mode0_owner_local

    mode0_owner_remote:
        jmp dword ptr [g_mode0OwnerRemoteResume]

    mode0_owner_local:
        jmp dword ptr [g_mode0OwnerLocalResume]
    }
}

void __cdecl TraceMode3OwnerResolve(unsigned int currentSceneChannel, unsigned int packetOwnerContext)
{
    bool currentSceneContextRemap =
        currentSceneChannel != 0 && packetOwnerContext == 0;
    LogLine("mode3-owner-resolve scene_channel=%u packet_owner=%u branch=%s",
        currentSceneChannel, packetOwnerContext,
        currentSceneChannel == packetOwnerContext || currentSceneContextRemap
            ? (currentSceneContextRemap ? "current-scene-context0-remap" : "current-scene")
            : "remote-actor");
}

// Resolve the actor with the same context-0/current-scene translation as mode
// 0.  The second comparison below must make the same decision.
__declspec(naked) void ProxyMode3OwnerResolve()
{
    __asm {
        pushfd
        pushad
        push ecx
        push esi
        call TraceMode3OwnerResolve
        add esp, 8
        popad
        popfd

        cmp ecx, esi
        je mode3_owner_local
        test ecx, ecx
        jnz mode3_owner_remote
        test esi, esi
        jnz mode3_owner_local

    mode3_owner_remote:
        // Complete the displaced remote-path instruction before resuming.
        mov esi, dword ptr [ebp-4B0h]
        jmp dword ptr [g_mode3OwnerRemoteResume]

    mode3_owner_local:
        jmp dword ptr [g_mode3OwnerLocalResume]
    }
}

__declspec(naked) void ProxyMode3OwnerFinalize()
{
    __asm {
        mov ecx, dword ptr [g_mode3LocalOwnerChannelAddress]
        movzx eax, byte ptr [ecx]
        test eax, eax
        jz mode3_finalize_local
        cmp eax, ebx
        je mode3_finalize_local

        jmp dword ptr [g_mode3OwnerFinalizeRemoteResume]

    mode3_finalize_local:
        jmp dword ptr [g_mode3OwnerFinalizeLocalResume]
    }
}

// The actor's pickup-container field is not always either "a live container" or
// NULL. Live captures on 2026-09-22 showed it holding plain data values
// (0x45E145E1 / 0x00680070 / 0xEAA7F89D) at dungeon entry, and the null-only
// test below accepted them and handed them to the native manual pickup -- the
// "item lies on the ground, key press does nothing, no op43 is sent" symptom.
//
// The predicate deliberately tests only properties that a live container is
// guaranteed to have, because rejecting a real container would be a regression:
//   * 4-byte alignment       -- any object with pointer members has it
//                               (rejects 0x45E145E1 and 0xEAA7F89D)
//   * user address range     -- DNF.exe sets IMAGE_FILE_LARGE_ADDRESS_AWARE, so
//                               the bound is 0xFFFEFFFF, not 0x7FFEFFFF
//   * mapped, committed, non-guard page -- VirtualQuery only, no access to the
//                               memory itself
//   * not part of a module image -- heap blocks are never MEM_IMAGE (rejects
//                               0x00680070, which sits inside DNF.exe)
// Nothing is assumed about the object's contents: the container constructor at
// 0x233CFD0 never writes offset 0, so the first dword may legitimately be zero or
// a non-pointer -- and the replacement containers this plugin installs are built
// by that same allocator/constructor pair, so they must pass here too.
bool LooksLikeLivePickupContainer(void* candidate)
{
    if (!candidate || !g_dnfBase) return false;
    const uintptr_t value = reinterpret_cast<uintptr_t>(candidate);
    if ((value & 3u) != 0) return false;
    if (value < 0x00010000u || value > 0xFFFEFFFFu) return false;

    MEMORY_BASIC_INFORMATION region = {};
    if (!VirtualQuery(candidate, &region, sizeof(region))) return false;
    if (region.State != MEM_COMMIT) return false;
    if ((region.Protect & (PAGE_GUARD | PAGE_NOACCESS)) != 0) return false;
    if (region.Type == MEM_IMAGE) return false;

    // The value must also leave room for the field inside its own region, so a
    // pointer into the last bytes of a mapping is not mistaken for an object.
    const unsigned long long regionEnd =
        static_cast<unsigned long long>(
            reinterpret_cast<uintptr_t>(region.BaseAddress)) +
        region.RegionSize;
    if (static_cast<unsigned long long>(value) + sizeof(uintptr_t) > regionEnd) {
        return false;
    }
    return true;
}
void* EnsureDungeonPickupContainer(void* actor, const char* source)
{
    if (!actor || !g_dnfBase || g_pickupContainerRepairActive) return nullptr;

    void** containerSlot = reinterpret_cast<void**>(
        reinterpret_cast<uintptr_t>(actor) + kPickupContainerOffset);
    void* container = nullptr;
    unsigned int townEntryState = 0;
    void* controlledActor = nullptr;

    __try {
        container = *containerSlot;
        if (container) {
            if (LooksLikeLivePickupContainer(container)) return container;
            // Non-null but provably not an object (see the predicate above).
            // Treat it exactly like the missing-container case and let the shared
            // repair path below replace it, instead of handing the raw value to
            // the native manual pickup.
            LogLine("pickup-container rejected non-object value source=%s actor=%p value=%p",
                source ? source : "unknown", actor, container);
            container = nullptr;
        }

        controlledActor = *reinterpret_cast<void**>(
            g_dnfBase + kControlledActorPointerRva);
        townEntryState = *reinterpret_cast<unsigned int*>(
            g_dnfBase + kTownEntryStateRva);
        if (controlledActor != actor || townEntryState != kDungeonTownEntryState) {
            return nullptr;
        }

        g_pickupContainerRepairActive = true;
        PickupContainerAllocatorFn allocator =
            reinterpret_cast<PickupContainerAllocatorFn>(
                g_dnfBase + kPickupContainerAllocatorRva);
        PickupContainerConstructorFn constructor =
            reinterpret_cast<PickupContainerConstructorFn>(
                g_dnfBase + kPickupContainerConstructorRva);
        void* memory = allocator ? allocator(kPickupContainerBytes) : nullptr;
        if (!memory) {
            g_pickupContainerRepairActive = false;
            LogLine("pickup-container repair failed source=%s actor=%p reason=allocation",
                source ? source : "unknown", actor);
            return nullptr;
        }

        container = constructor
            ? constructor(memory, 0, kPickupContainerKind)
            : nullptr;
        if (!container) {
            g_pickupContainerRepairActive = false;
            LogLine("pickup-container repair failed source=%s actor=%p reason=constructor",
                source ? source : "unknown", actor);
            return nullptr;
        }

        // The native actor initializer owns this same field and the matching
        // teardown paths destroy it. Pickup runs on the game thread, so install
        // it only after the guarded null check and let native ownership resume.
        *containerSlot = container;
        g_pickupContainerRepairActive = false;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        g_pickupContainerRepairActive = false;
        LogLine("pickup-container repair exception source=%s actor=%p code=0x%08X",
            source ? source : "unknown", actor, GetExceptionCode());
        return nullptr;
    }

    LONG repair = InterlockedIncrement(&g_pickupContainerRepairCount);
    LogLine("pickup-container repaired source=%s actor=%p container=%p state=%u repair=%ld",
        source ? source : "unknown", actor, container, townEntryState, repair);
    return container;
}

void* __fastcall ProxyPickupContainerGetter(void* self, void* /*unused*/)
{
    PickupContainerGetterFn original =
        reinterpret_cast<PickupContainerGetterFn>(g_originalPickupContainerGetter);
    // sub_24A1BC0 is a general actor-container getter.  It is reached while a
    // town actor is being installed as well as by the dungeon pickup path, and
    // the current EXE reports town-entry state 3 in both situations.  Creating
    // a replacement container here splits dword_51B2768 from the UI alias at
    // dword_51B2790 and leaves the crystal/soul warehouse counters reading an
    // empty manager.  Keep this proxy side-effect free; the narrowly scoped
    // sub_24B2840 hook below owns the missing-container repair immediately
    // before an actual manual pickup.
    return original ? original(self) : nullptr;
}

void* __fastcall ProxyManualPickup(void* self, void* /*unused*/, void* objects)
{
    EnsureDungeonPickupContainer(self, "manual-pickup");
    ManualPickupFn original = reinterpret_cast<ManualPickupFn>(g_originalManualPickup);
    return original ? original(self, objects) : nullptr;
}

// Preserve sub_1D84FE0's native stack contract. The wide-name pointer was
// already placed on the stack before the equipped-creature key lookup. The
// original add esp,4 removes only sub_25FD8D0's cdecl key argument; on a
// successful lookup execution resumes at the native sub_34DEA20 call, while a
// missing map entry skips that optional third copy and returns through the
// original epilogue. The actor and live creature name copies earlier in the
// same handler remain untouched.
__declspec(naked) void ProxyCreatureRenameMapNullCheck()
{
    __asm {
        add esp, 4
        test eax, eax
        jz creature_rename_map_done
        lea ecx, [eax+14h]
        jmp dword ptr [g_creatureRenameMapUpdateResume]

    creature_rename_map_done:
        jmp dword ptr [g_creatureRenameMapDoneResume]
    }
}

// Entry contract at sub_2602260+0xD6:
//   eax = result of the optional equipped-creature op105-map lookup
// The displaced native instructions are mov ecx,eax; call sub_25FCAF0. A null
// result represents a temporarily absent optional record during actor rebuild,
// so return the same false predicate consumed by the following native test al.
__declspec(naked) void ProxyTownCreatureStatMapNullCheck()
{
    __asm {
        test eax, eax
        jz town_creature_stat_map_missing
        mov ecx, eax
        call dword ptr [g_townCreatureStatMapPredicate]
        jmp dword ptr [g_townCreatureStatMapResume]

    town_creature_stat_map_missing:
        xor eax, eax
        jmp dword ptr [g_townCreatureStatMapResume]
    }
}

// Entry contract at sub_2E59620+0x6E:
//   esi = chat-scene host (cache lives at this+0x4C)
//   ebx = arg1 = target actor pointer for the name lookup
// Displaced native instructions are cmp [esi+0x4C],0; jne missing.  When the
// cache is already populated the native jne bypasses the accessor entirely,
// and a NULL arg1 must take that same bypass (the accessor would otherwise
// dereference arg1+0x38C and fault at sub_26908C0).  Populated cache and NULL
// arg1 both jump to missing; a live arg1 with an empty cache replays the
// displaced mov ecx,ebx and resumes at the native accessor call.
__declspec(naked) void ProxyChatNameCacheNullGuard()
{
    __asm {
        cmp dword ptr [esi+4Ch], 0
        jne chat_name_cache_missing
        test ebx, ebx
        jz chat_name_cache_missing
        mov ecx, ebx
        jmp dword ptr [g_chatNameCachePresentResume]

    chat_name_cache_missing:
        jmp dword ptr [g_chatNameCacheMissingResume]
    }
}

// Entry contract at sub_1D73120+0x899 after the native same-template path has
// applied the row's serial/count:
//   esi = existing item object
//   ebp-0x17D = list type
//   ebp-0x17C = slot
//   ebp-0x104 = complete dynamic state parsed from the incoming 0x77 row
// Invoke only for list-7 pet bodies (slots 0..139) or the equipped creature at
// list-3/slot-26.  In particular, never call it for list-7 pet consumables at
// slots 189..238: their same-template op14 is the normal count-decrement path.
// The native dynamic-state setter is already used later in this same handler.
// The displaced movsx is restored before the original slot-refresh call, and
// the existing item pointer is never destroyed or replaced.
__declspec(naked) void ProxyPetItemUpdateDynamicState()
{
    __asm {
        cmp byte ptr [ebp-17Dh], 7
        jne pet_item_update_check_equipped
        cmp word ptr [ebp-17Ch], 139
        jbe pet_item_update_apply
        jmp pet_item_update_resume

    pet_item_update_check_equipped:
        cmp byte ptr [ebp-17Dh], 3
        jne pet_item_update_resume
        cmp word ptr [ebp-17Ch], 26
        jne pet_item_update_resume

    pet_item_update_apply:
        mov edx, [esi]
        lea eax, [ebp-104h]
        push eax
        mov ecx, esi
        call dword ptr [edx+158h]

    pet_item_update_resume:
        movsx eax, word ptr [ebp-17Ch]
        jmp dword ptr [g_petItemUpdateDynamicStateResume]
    }
}

void __cdecl TraceAutoRepairAuxContainerMissing(void* actor)
{
    LONG hit = InterlockedIncrement(&g_autoRepairAuxNullCount);
    if (hit <= 8) {
        LogLine("auto-repair auxiliary equipment scan skipped actor=%p reason=null_container hit=%ld",
            actor, hit);
    }
}

void __cdecl TraceRaidDirectoryOwnerMissing()
{
    LONG hit = InterlockedIncrement(&g_raidDirectoryMissingOwnerCount);
    if (hit <= 8) {
        LogLine("Anton raid public-directory response kept native empty UI reason=missing_dedicated_connection_owner hit=%ld",
            hit);
    }
}

// Entry contract at sub_1D4C060+0x33E, immediately after the native slot-15
// resolver:
//   eax = raid-service owner or null
//   edi = native zero value used by both helper arguments
// A present owner replays the displaced instructions and continues at the
// native helper call. A missing owner follows the handler's existing cleanup
// block at 0x1D4C593 with ebx==esi==0, matching op844's neighboring null-owner
// behavior while retaining the already-created empty public-directory UI.
__declspec(naked) void ProxyRaidDirectoryOwnerGuard()
{
    __asm {
        mov ebx, eax
        mov [ebp-4Ch], ebx
        test ebx, ebx
        jz raid_directory_owner_missing
        push edi
        push edi
        mov ecx, ebx
        jmp dword ptr [g_raidDirectoryOwnerPresentResume]

    raid_directory_owner_missing:
        pushfd
        pushad
        call TraceRaidDirectoryOwnerMissing
        popad
        popfd
        xor esi, esi
        jmp dword ptr [g_raidDirectoryOwnerMissingResume]
    }
}

// ---------------------------------------------------------------------------
// Pet transport stone (item 490700208) -- native action-type registry gap.
//
// The shipped PVF declares
//
//   [action type]
//   `[creature transport stone]` 16
//   [/action type]
//
// but this client build's action-type registry -- the table the transport
// routine reaches through sub_391A240 -- knows 4 and 4012 and not 16. The
// standalone probe measured it directly (petfix\PetFixCrash.log, 2026-10-04):
//
//   [probe #3] vector@... begin=... end=... count=1 first=0x00000010
//   [key #3]   client asked for 16 (0x10) - this client build does not know it,
//              remapped to 4 (0x4) -> hit 0x69C91910
//   [key #2]   searched for 4012 (0xFAC) -> hit 0x69C90810
//
// A miss returns NULL and the routine dereferences it unchecked:
//
//   0x01E78DC9  call 0x02313210        ; eax = object holding the collected vector
//   0x01E78DCE  mov  ecx, [eax+0x424]  ; vector begin  (the collected action values)
//   0x01E78DD4  mov  eax, [eax+0x428]  ; vector end
//   0x01E78DF0  mov  ecx, [ecx]        ; key = element[0]
//   0x01E78DF3  call 0x0391A240        ; registry lookup -> NULL for 16
//   0x01E78E03  mov  edi, eax
//   0x01E78E29  mov  ecx, [edi]        ; access violation reading 0x00000000
//
// Two hooks, both carried over byte-for-byte from the in-game validated
// standalone DLL, with the diagnostics dropped:
//
//   1. at 0x01A78DC9 the collected value is normalised in place (16 -> 4) before
//      the routine and the window read it. Remapping only the lookup key was
//      measured insufficient: the window opened but the pet could not be
//      dropped in, because both re-read the value from this same vector.
//   2. at 0x01A78E29 the null guard: a miss runs the destructor block the
//      skipped code would have run, reports failure (bl = 0) and takes the
//      native epilogue instead of dereferencing NULL. It cannot change anything
//      that used to work, because natively edi == 0 is an instant crash.
// ---------------------------------------------------------------------------
constexpr unsigned int kCreatureTransportActionValueUnknown = 16;
constexpr unsigned int kCreatureTransportActionValueKnown = 4;
constexpr unsigned int kCreatureTransportActionVectorBeginOffset = 0x424;
constexpr unsigned int kCreatureTransportActionVectorEndOffset = 0x428;

void __cdecl TraceCreatureTransportActionLookupMiss()
{
    LONG hit = InterlockedIncrement(&g_creatureTransportActionLookupMissCount);
    if (hit <= 8) {
        LogLine("pet transport stone registry lookup missed; native cleanup taken instead of a NULL dereference hit=%ld",
            hit);
    }
}

// Rewrites the collected action value the transport routine is about to look
// up. Reads exactly what the native reader reads next -- element [0] of the
// vector at +0x424/+0x428 -- plus the empty-vector check the native code
// replaces with an inlined _STL_VERIFY, so this cannot fault anywhere the very
// next native instruction would not fault first.
void __cdecl NormalizeCreatureTransportActionValue(void* transportObject)
{
    if (!transportObject) return;
    unsigned int* rewritten = nullptr;
    unsigned int previous = 0;
    __try {
        unsigned char* object = static_cast<unsigned char*>(transportObject);
        unsigned int* begin = *reinterpret_cast<unsigned int**>(
            object + kCreatureTransportActionVectorBeginOffset);
        unsigned int* end = *reinterpret_cast<unsigned int**>(
            object + kCreatureTransportActionVectorEndOffset);
        if (!begin || end <= begin) return;
        if (*begin != kCreatureTransportActionValueUnknown) return;
        previous = *begin;
        *begin = kCreatureTransportActionValueKnown;
        rewritten = begin;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LONG fault = InterlockedIncrement(
            &g_creatureTransportActionRewriteFaultCount);
        if (fault <= 4) {
            LogLine("pet transport stone action value normalisation skipped object=%p code=0x%08X",
                transportObject, GetExceptionCode());
        }
        return;
    }

    if (rewritten) {
        LONG hit = InterlockedIncrement(&g_creatureTransportActionRewriteCount);
        if (hit <= 8) {
            LogLine("pet transport stone action value normalised %u -> %u slot=%p hit=%ld",
                previous, kCreatureTransportActionValueKnown, rewritten, hit);
        }
    }
}

// Entry contract at sub_1D78DC9. The displaced instruction is the five-byte
// cdecl call to sub_2313210; the hook is a jmp, so there is no return address
// and ESP points straight at the caller's argument. The argument is left on the
// stack for the native `add esp,0x14`, and the call's result is handed back in
// EAX through pushad's saved-EAX slot ([esp+0x1C] after pushad, argument at
// [esp+0x20]).
__declspec(naked) void ProxyCreatureTransportActionVectorFix()
{
    __asm {
        pushad
        mov eax, dword ptr [esp+20h]
        push eax
        call dword ptr [g_creatureTransportActionVectorSource]
        add esp, 4
        mov dword ptr [esp+1Ch], eax
        push eax
        call NormalizeCreatureTransportActionValue
        add esp, 4
        popad
        jmp dword ptr [g_creatureTransportActionVectorResume]
    }
}

// Entry contract at sub_1D78E29, immediately after the registry lookup:
//   edi = lookup result (null on a miss, which is what crashed the client)
//   eax = the object the displaced `mov eax,[eax]` reads
// A hit replays the displaced five bytes and resumes at the native call. A miss
// runs the two local-object destructors the skipped block would have run, marks
// the SEH state dead, returns false through bl and jumps to the native
// epilogue -- the routine then reports the failure instead of dying.
__declspec(naked) void ProxyCreatureTransportActionLookupGuard()
{
    __asm {
        test edi, edi
        jz creature_transport_action_lookup_missing

        mov ecx, dword ptr [edi]
        mov eax, dword ptr [eax]
        push ecx
        jmp dword ptr [g_creatureTransportActionLookupPresentResume]

    creature_transport_action_lookup_missing:
        pushfd
        pushad
        call TraceCreatureTransportActionLookupMiss
        popad
        popfd
        mov dword ptr [ebp-4], 0FFFFFFFFh
        lea ecx, [ebp-28h]
        call dword ptr [g_creatureTransportActionLocalDestructor]
        lea ecx, [ebp-20h]
        call dword ptr [g_creatureTransportActionLocalDestructor]
        xor bl, bl
        jmp dword ptr [g_creatureTransportActionLookupMissingResume]
    }
}

void __cdecl TraceRaidPanelNullCallMissing()
{
    LONG hit = InterlockedIncrement(&g_raidPanelNullCallMissingCount);
    if (hit <= 8) {
        LogLine("raid panel interface call skipped reason=null_vtable_slot hit=%ld",
            hit);
    }
}

// Entry contract at 0x025BD2F4 (raid-side panel/manager interface call):
//   ecx = [ebp+1C], a raid object that the native code already null-checked
//   esi = value the native call site pushes as its argument
// The displaced instructions are replayed, then the virtual slot at
// +0x238 is loaded and tested. A non-null slot reproduces the native
// push/call exactly. A null slot (the measured hatchery-entry crash, eip=0 with
// return address 0x025BD2FF) skips the call -- including its pushed argument,
// so the stack stays balanced -- and resumes at the next native call. The
// skipped call's result is not consumed: 0x025BD2FF is itself a call that sets
// eax before the following test al,al.
__declspec(naked) void ProxyRaidPanelNullCallGuard()
{
    __asm {
        mov eax, dword ptr [ecx]
        test eax, eax
        jz raid_panel_null_call_missing
        mov edx, dword ptr [eax+238h]
        test edx, edx
        jz raid_panel_null_call_missing
        push esi
        call edx
        jmp dword ptr [g_raidPanelNullCallResume]

    raid_panel_null_call_missing:
        pushfd
        pushad
        call TraceRaidPanelNullCallMissing
        popad
        popfd
        jmp dword ptr [g_raidPanelNullCallResume]
    }
}

void __cdecl TraceRaidInstanceFlagMissing()
{
    LONG hit = InterlockedIncrement(&g_raidInstanceFlagMissingCount);
    if (hit <= 16) {
        LogLine("raid instance optional flag read skipped reason=null_sub_object hit=%ld",
            hit);
    }
}

// The raid-instance teardown function at 0x01F95460 repeats one block three
// times (live 2026-09-17 19:29:00, the return-to-town after clearing hatchery
// 221):
//
//   call 0x1CF2A70                     ; singleton [0x51B1290]
//   mov  ecx, eax
//   call 0x22644B0                     ; -> [sub-object + 0xCC]
//   mov  ecx, eax
//   call 0x23591E0                     ; lea eax,[ecx+0x140] ; ret
//   cmp  byte ptr [eax+0xA58], 0       ; <<< faults when eax was 0x140
//   jne  <resume>
//   [push 0] ; mov ecx, esi ; call <optional>
//   <resume>
//
// `eax` is 0x140 when the sub-object behind the singleton is still null, so the
// compare reads 0x140 + 0xA58 = 0xB98 -- the same access address as the three
// 2026-09-17 crashes at 11:49:22 / 16:52:12 / 17:49:43, and again at
// 19:29:00.739 (site 0x01F95611) 13 ms after the 0x01F95C63 site had already
// been skipped. Taking the "already present" outcome skips exactly the optional
// call, which is the branch the native code takes when the flag is set.
//
// Each site keeps its own resume and optional-call target; only the pushed
// argument count differs (site 0x01F9568A pushes one dword).
constexpr int kRaidInstanceFlagSiteCount = 3;
uintptr_t g_raidInstanceFlagResumes[kRaidInstanceFlagSiteCount] = {};
uintptr_t g_raidInstanceFlagCalls[kRaidInstanceFlagSiteCount] = {};
LONG g_raidInstanceFlagSiteHits[kRaidInstanceFlagSiteCount] = {};

void __cdecl TraceRaidInstanceFlagSiteMissing(int site)
{
    if (site < 0 || site >= kRaidInstanceFlagSiteCount) {
        return;
    }
    LONG hit = InterlockedIncrement(&g_raidInstanceFlagSiteHits[site]);
    if (hit <= 8) {
        LONG total = InterlockedIncrement(&g_raidInstanceFlagMissingCount);
        LogLine("raid instance optional flag read skipped reason=null_sub_object site=%d hit=%ld total=%ld",
            site, hit, total);
    }
}

// site 0: 0x01F95C63, resume 0x01F95C73, call 0x01F79560, no pushed argument.
__declspec(naked) void ProxyRaidInstanceFlagNullGuard0()
{
    __asm {
        cmp eax, 10000h
        jb raid_instance_flag0_missing
        cmp byte ptr [eax+0A58h], 0
        jne raid_instance_flag0_resume
        mov ecx, esi
        call dword ptr [g_raidInstanceFlagCalls]
        jmp raid_instance_flag0_resume

    raid_instance_flag0_missing:
        pushfd
        pushad
        push 0
        call TraceRaidInstanceFlagSiteMissing
        add esp, 4
        popad
        popfd

    raid_instance_flag0_resume:
        jmp dword ptr [g_raidInstanceFlagResumes]
    }
}

// site 1: 0x01F95611, resume 0x01F95621, call 0x01F60340, no pushed argument.
// This is the site that killed the client at 19:29:00.739.
__declspec(naked) void ProxyRaidInstanceFlagNullGuard1()
{
    __asm {
        cmp eax, 10000h
        jb raid_instance_flag1_missing
        cmp byte ptr [eax+0A58h], 0
        jne raid_instance_flag1_resume
        mov ecx, esi
        call dword ptr [g_raidInstanceFlagCalls+4]
        jmp raid_instance_flag1_resume

    raid_instance_flag1_missing:
        pushfd
        pushad
        push 1
        call TraceRaidInstanceFlagSiteMissing
        add esp, 4
        popad
        popfd

    raid_instance_flag1_resume:
        jmp dword ptr [g_raidInstanceFlagResumes+4]
    }
}

// site 2: 0x01F9568A, resume 0x01F9569C, call 0x01F790E0, one pushed dword.
__declspec(naked) void ProxyRaidInstanceFlagNullGuard2()
{
    __asm {
        cmp eax, 10000h
        jb raid_instance_flag2_missing
        cmp byte ptr [eax+0A58h], 0
        jne raid_instance_flag2_resume
        push 0
        mov ecx, esi
        call dword ptr [g_raidInstanceFlagCalls+8]
        jmp raid_instance_flag2_resume

    raid_instance_flag2_missing:
        pushfd
        pushad
        push 2
        call TraceRaidInstanceFlagSiteMissing
        add esp, 4
        popad
        popfd

    raid_instance_flag2_resume:
        jmp dword ptr [g_raidInstanceFlagResumes+8]
    }
}

// Entry contract at sub_20695F0+0x18F:
//   eax = sub_24A1BC0(controlled_actor)
//   [esp] = auxiliary slot index already pushed by the native loop
// A present container follows the exact native sub_2326860 call and resumes at
// 0x2069786. A missing optional container discards that pending argument and
// skips only the auxiliary slots; the ordinary equipment durability pass has
// already completed.
__declspec(naked) void ProxyAutoRepairAuxContainerLookup()
{
    __asm {
        test eax, eax
        jz auto_repair_aux_missing
        mov ecx, eax
        call dword ptr [g_autoRepairAuxLookupFn]
        jmp dword ptr [g_autoRepairAuxLookupResume]

    auto_repair_aux_missing:
        pushfd
        pushad
        push edi
        call TraceAutoRepairAuxContainerMissing
        add esp, 4
        popad
        popfd
        add esp, 4
        jmp dword ptr [g_autoRepairAuxLoopDone]
    }
}

int __fastcall ProxyOp24SceneMode(void* self, void* /*unused*/)
{
    Op24SceneModeFn original = reinterpret_cast<Op24SceneModeFn>(g_originalOp24SceneMode);
    int result = original ? original(self) : 0;
    if (g_traceOp24) {
        if (result == 5) g_op24SceneModeEarlyReturn = true;
        if (g_op24SceneModeCalls++ == 0) {
            LogLine("op24-native-gate name=scene_mode self=%p result=%d early_return=%d",
                self, result, result == 5 ? 1 : 0);
        }
    }
    return result;
}

unsigned char __cdecl ProxyOp24LoadingGate()
{
    Op24LoadingGateFn original = reinterpret_cast<Op24LoadingGateFn>(g_originalOp24LoadingGate);
    unsigned char result = original ? original() : 0;
    if (g_traceOp24) {
        if (result != 0) g_op24LoadingGateEarlyReturn = true;
        if (g_op24LoadingGateCalls++ == 0) {
            LogLine("op24-native-gate name=loading_gate result=%u early_return=%d",
                static_cast<unsigned int>(result), result ? 1 : 0);
        }
    }
    return result;
}

bool TryCopyTraceBytes(const void* source, void* destination, size_t length)
{
    if (!source || !destination || length == 0) return false;
    __try {
        const unsigned char* from = static_cast<const unsigned char*>(source);
        unsigned char* to = static_cast<unsigned char*>(destination);
        for (size_t i = 0; i < length; ++i) to[i] = from[i];
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

void FormatTraceBytes(const unsigned char* bytes, size_t length, char* output, size_t outputSize)
{
    if (!output || outputSize == 0) return;
    output[0] = '\0';
    if (!bytes || length == 0) return;

    static const char kHex[] = "0123456789ABCDEF";
    size_t used = 0;
    for (size_t i = 0; i < length && used + 3 < outputSize; ++i) {
        unsigned char value = bytes[i];
        output[used++] = kHex[value >> 4];
        output[used++] = kHex[value & 0x0F];
        if (i + 1 < length && used + 1 < outputSize) output[used++] = ' ';
    }
    output[used] = '\0';
}

bool IsSocketTraceEnabled()
{
    // Was <DNF.exe dir>\90CN_socket_trace.ini [trace] enabled; this DLL reads no
    // INI file any more (see clientConfig at the top of this file).
    return kSocketTraceEnabled;
}

void CacheCurrentItemRow(uintptr_t itemObject, const unsigned char* raw)
{
    if (!itemObject || !raw) return;
    LONG sequence = InterlockedIncrement(&g_socketTraceRowSequence);
    LONG slot = sequence % kSocketTraceCacheRows;
    if (slot < 0) slot += kSocketTraceCacheRows;

    AcquireSRWLockExclusive(&g_socketTraceLock);
    g_socketTraceRows[slot].sequence = sequence;
    g_socketTraceRows[slot].parserObject = itemObject;
    memcpy(g_socketTraceRows[slot].raw, raw, kCurrentItemRawBytes);
    ReleaseSRWLockExclusive(&g_socketTraceLock);
}

uint16_t CurrentItemRowSlot(const unsigned char* raw)
{
    if (!raw) return 0;
    return static_cast<uint16_t>(raw[0]) |
        (static_cast<uint16_t>(raw[1]) << 8);
}

uint32_t CurrentItemRowTemplateID(const unsigned char* raw)
{
    if (!raw) return 0;
    return static_cast<uint32_t>(raw[2]) |
        (static_cast<uint32_t>(raw[3]) << 8) |
        (static_cast<uint32_t>(raw[4]) << 16) |
        (static_cast<uint32_t>(raw[5]) << 24);
}

uint32_t CurrentItemRowIdentity(const unsigned char* raw)
{
    if (!raw) return 0;
    return static_cast<uint32_t>(raw[kCurrentItemIdentityOffset]) |
        (static_cast<uint32_t>(raw[kCurrentItemIdentityOffset + 1]) << 8) |
        (static_cast<uint32_t>(raw[kCurrentItemIdentityOffset + 2]) << 16) |
        (static_cast<uint32_t>(raw[kCurrentItemIdentityOffset + 3]) << 24);
}

bool IsCurrentPremiumContractID(uint32_t templateID)
{
    // Generated from the pinned runtime Script.pvf
    // etc/premiumlist_new.etc [item] rows.  Keeping the allow-list in this
    // current-client compatibility unit prevents an all-stackable bypass.
    static const uint32_t kContractIDs[] = {
        30, 31, 32, 33, 34, 43, 44, 45, 46,
        193, 194, 196, 198, 720, 741, 742, 743, 744, 820, 901, 917, 920,
        2660012, 2660013, 2660050, 2660051, 2660290, 2660354, 2660358,
        2660409, 2660410, 2660411, 2660543, 2660545, 2660703, 2660704,
        2660705, 2682763, 2683452, 2683453, 2683454, 2683455, 2683456,
        2683457, 2683458, 2683459, 2749321,
        10000388, 10000389, 10000390, 10000391,
        10012576, 10012577, 10012578, 10012579,
        10092497, 10096109, 10096110, 10096111, 10096112, 10096113,
        10151653, 10157955,
        490700021, 490700022, 490700023,
        490700028, 490700029, 490700030, 490700031, 490700032,
        490700033, 490700034, 490700035, 490700036
    };
    for (size_t i = 0; i < sizeof(kContractIDs) / sizeof(kContractIDs[0]); ++i) {
        if (kContractIDs[i] == templateID) return true;
    }
    return false;
}

uint32_t ReadCurrentItemTemplateID(void* item)
{
    if (!item || !g_dnfBase) return 0;
    __try {
        void** vtable = *reinterpret_cast<void***>(item);
        if (!vtable) return 0;
        CurrentItemDataFn itemDataFn =
            reinterpret_cast<CurrentItemDataFn>(vtable[0x28 / sizeof(void*)]);
        if (!itemDataFn) return 0;
        void* itemData = itemDataFn(item);
        if (!itemData) return 0;
        CurrentItemTemplateReadFn readTemplate =
            reinterpret_cast<CurrentItemTemplateReadFn>(
                g_dnfBase + kCurrentItemTemplateReadRva);
        return readTemplate(
            reinterpret_cast<unsigned char*>(itemData) + 0x14);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("item-template read exception item=%p code=0x%08X",
            item, GetExceptionCode());
        return 0;
    }
}

char __cdecl ProxyItemAcquireNotification(
    void* item, int amount, int actor, int finalFlag)
{
    uint32_t templateID = ReadCurrentItemTemplateID(item);
    if (templateID == kUnsafeMailAcquireNotificationItemID) {
        LogLine("op14 acquisition notification suppressed item=%u amount=%d "
            "reason=current-exe-old-booster-toast-crash inventory_update=preserved",
            templateID, amount);
        return 0;
    }
    ItemAcquireNotificationFn nativeNotification =
        reinterpret_cast<ItemAcquireNotificationFn>(
            g_dnfBase + kItemAcquireNotificationRva);
    return nativeNotification
        ? nativeNotification(item, amount, actor, finalFlag)
        : 0;
}

struct CurrentContractSelection {
    int slot;
    uint32_t templateID;
    uint32_t identity;
};

bool ResolveCurrentPremiumContractSelection(CurrentContractSelection* output)
{
    if (!output || !g_dnfBase) return false;
    output->slot = -1;
    output->templateID = 0;
    output->identity = 0;

    struct NativeIntVector {
        int* begin;
        int* end;
        int* capacity;
    } selected = {};
    bool constructed = false;
    void* manager = nullptr;
    int slot = -1;
    __try {
        manager = *reinterpret_cast<void**>(
            g_dnfBase + kInventorySelectionManagerRva);
        if (!manager) return false;
        InventorySelectionCtorFn ctor = reinterpret_cast<InventorySelectionCtorFn>(
            g_dnfBase + kInventorySelectionCtorRva);
        InventorySelectionCollectFn collect =
            reinterpret_cast<InventorySelectionCollectFn>(
                g_dnfBase + kInventorySelectionCollectRva);
        InventorySelectionDtorFn dtor = reinterpret_cast<InventorySelectionDtorFn>(
            g_dnfBase + kInventorySelectionDtorRva);
        ctor(&selected);
        constructed = true;
        collect(manager, &selected, kMainInventorySelectionGroup);
        if (selected.begin && selected.end && selected.end > selected.begin) {
            slot = *selected.begin;
        }
        dtor(&selected);
        constructed = false;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        if (constructed) {
            __try {
                InventorySelectionDtorFn dtor =
                    reinterpret_cast<InventorySelectionDtorFn>(
                        g_dnfBase + kInventorySelectionDtorRva);
                dtor(&selected);
            }
            __except (EXCEPTION_EXECUTE_HANDLER) {
            }
        }
        LogLine("contract-use selection read exception code=0x%08X",
            GetExceptionCode());
        return false;
    }

    uint32_t templateID = 0;
    uint32_t identity = 0;
    __try {
        InventorySelectionLookupFn lookup =
            reinterpret_cast<InventorySelectionLookupFn>(
                g_dnfBase + kInventorySelectionLookupRva);
        void* selectedItem = lookup(manager, slot);
        if (!selectedItem) return false;
        void** vtable = *reinterpret_cast<void***>(selectedItem);
        if (!vtable) return false;
        CurrentItemDataFn itemDataFn =
            reinterpret_cast<CurrentItemDataFn>(vtable[0x28 / sizeof(void*)]);
        if (!itemDataFn) return false;
        void* itemData = itemDataFn(selectedItem);
        if (!itemData) return false;

        // This is the exact current-EXE read sequence used immediately before
        // both native op44 writers.  Reading the selected wrapper avoids
        // confusing same-numbered slots from the equipment, material, avatar,
        // pet, and consumable lists in the passive parser cache.
        CurrentItemTemplateReadFn readTemplate =
            reinterpret_cast<CurrentItemTemplateReadFn>(
                g_dnfBase + kCurrentItemTemplateReadRva);
        templateID = readTemplate(
            reinterpret_cast<unsigned char*>(itemData) + 0x14);
        if (!IsCurrentPremiumContractID(templateID)) return false;

        CurrentItemIdentityReadFn readIdentity =
            reinterpret_cast<CurrentItemIdentityReadFn>(
                g_dnfBase + kCurrentItemIdentityReadRva);
        identity = readIdentity(templateID);
        if (identity == 0) return false;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("contract-use selected-item read exception slot=%d code=0x%08X",
            slot, GetExceptionCode());
        return false;
    }

    output->slot = slot;
    output->templateID = templateID;
    output->identity = identity;
    return true;
}

// sub_2331CE0 writes its own item and slot arguments into op44 and obtains
// the same instance identity through sub_2064A20.  Resolve the fallback from
// those native arguments rather than the generic selection manager: the
// active-coupon panel does not need to keep the latter's UI selection alive.
bool ResolveCurrentPremiumContractPanelSelection(
    uint32_t templateID, int slot, CurrentContractSelection* output)
{
    if (!output) return false;
    output->slot = -1;
    output->templateID = 0;
    output->identity = 0;
    if (!g_dnfBase || slot < 0 || slot > 0x7FFF ||
        !IsCurrentPremiumContractID(templateID)) {
        return false;
    }

    uint32_t identity = 0;
    __try {
        CurrentItemIdentityReadFn readIdentity =
            reinterpret_cast<CurrentItemIdentityReadFn>(
                g_dnfBase + kCurrentItemIdentityReadRva);
        identity = readIdentity(templateID);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("contract-use panel item-identity read exception slot=%d item=%lu code=0x%08X",
            slot, static_cast<unsigned long>(templateID), GetExceptionCode());
        return false;
    }
    if (identity == 0) return false;

    output->slot = slot;
    output->templateID = templateID;
    output->identity = identity;
    return true;
}

bool SendCurrentPremiumContractUse(const CurrentContractSelection& selection)
{
    if (selection.slot < 0 || selection.slot > 0x7FFF ||
        !IsCurrentPremiumContractID(selection.templateID) ||
        selection.identity == 0 || !g_dnfBase) {
        return false;
    }
    __try {
        UpperWriterValueFn value = reinterpret_cast<UpperWriterValueFn>(
            g_dnfBase + kUpperWriterValueRva);
        UpperWriterScalarFn writeU16 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU16Rva);
        UpperWriterScalarFn writeU8 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU8Rva);
        UpperWriterScalarFn writeI16 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterI16Rva);
        UpperWriterScalarFn writeU32 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU32Rva);
        UpperWriterFlushFn flush = reinterpret_cast<UpperWriterFlushFn>(
            g_dnfBase + kUpperWriterFlushRva);

        writeU16(value(), kUseStackableOpcode);
        writeI16(value(), static_cast<uint16_t>(selection.slot));
        writeU8(value(), kMainInventoryListType);
        writeU32(value(), selection.identity);
        writeU32(value(), selection.templateID);
        writeU32(value(), 0);
        flush();
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("contract-use fallback send exception slot=%d item=%lu identity=%lu code=0x%08X",
            selection.slot, static_cast<unsigned long>(selection.templateID),
            static_cast<unsigned long>(selection.identity), GetExceptionCode());
        return false;
    }
    LogLine("contract-use fallback sent opcode=44 slot=%d list=0 item=%lu identity=%lu",
        selection.slot, static_cast<unsigned long>(selection.templateID),
        static_cast<unsigned long>(selection.identity));
    return true;
}

bool SendCurrentEquipmentDisjointBatch(
    const EquipmentDisjointBatchCapture& capture)
{
    if (!g_dnfBase || !capture.sender || capture.count < 2 ||
        capture.count > kEquipmentDisjointBatchMaxItems ||
        capture.count != capture.expectedCount) {
        return false;
    }
    __try {
        UpperWriterValueFn value = reinterpret_cast<UpperWriterValueFn>(
            g_dnfBase + kUpperWriterValueRva);
        UpperWriterScalarFn writeU16 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU16Rva);
        UpperWriterScalarFn writeU8 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU8Rva);
        UpperWriterScalarFn writeI16 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterI16Rva);
        UpperWriterScalarFn writeU32 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU32Rva);
        UpperWriterFlushFn flush = reinterpret_cast<UpperWriterFlushFn>(
            g_dnfBase + kUpperWriterFlushRva);

        // Queue one marked op26 for all native-selected slots. The server
        // expands the marked command into
        // contiguous ordinary op26 success bodies, so the stock result reader
        // removes every item and opens its aggregate popup exactly once.
        writeU16(value(), kEquipmentDisjointOpcode);
        writeI16(value(), 0xFFFFu);
        writeU8(value(), kMainInventoryListType);
        writeI16(value(), static_cast<unsigned short>(capture.toolSlot));
        writeU32(value(), static_cast<unsigned int>(capture.context));
        writeU32(value(), kEquipmentDisjointBatchMarker);
        writeU8(value(), capture.count);
        for (unsigned int index = 0; index < capture.count; ++index) {
            writeI16(value(), static_cast<unsigned short>(capture.slots[index]));
        }
        flush();
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("equipment-disjoint batch send exception selected=%u code=0x%08X",
            capture.count, GetExceptionCode());
        return false;
    }
    LogLine("equipment-disjoint batch queued selected=%u tool=%d context=%d",
        capture.count,
        static_cast<int>(capture.toolSlot), capture.context);
    return true;
}

void ReplayCurrentEquipmentDisjointCapturedItems(
    const EquipmentDisjointBatchCapture& capture)
{
    EquipmentDisjointSendFn original =
        reinterpret_cast<EquipmentDisjointSendFn>(g_originalEquipmentDisjointSend);
    if (!original || !capture.sender || capture.count == 0) return;
    for (unsigned int index = 0; index < capture.count; ++index) {
        original(capture.sender, capture.slots[index], capture.context);
    }
    LogLine("equipment-disjoint batch fallback replayed=%u",
        capture.count);
}

bool __fastcall ProxyCurrentEquipmentDisjointSend(
    void* self, void* /*unused*/, int sourceSlot, int context)
{
    EquipmentDisjointSendFn original =
        reinterpret_cast<EquipmentDisjointSendFn>(g_originalEquipmentDisjointSend);
    if (!original) return false;
    EquipmentDisjointBatchCapture& capture = g_equipmentDisjointBatch;
    if (!capture.active || capture.fallback) {
        return original(self, sourceSlot, context);
    }
    if (!self || sourceSlot <= 0 || sourceSlot > 0x7FFF) {
        capture.fallback = true;
        return original(self, sourceSlot, context);
    }

    if (capture.count == 0) {
        short toolSlot = -1;
        __try {
            toolSlot = *reinterpret_cast<short*>(
                reinterpret_cast<unsigned char*>(self) + 0x98);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            capture.fallback = true;
            LogLine("equipment-disjoint batch tool-slot read exception slot=%d code=0x%08X",
                sourceSlot, GetExceptionCode());
            return original(self, sourceSlot, context);
        }
        capture.sender = self;
        capture.context = context;
        capture.toolSlot = toolSlot;
        capture.slots[0] = static_cast<short>(sourceSlot);
        capture.count = 1;
        return true;
    }

    if (capture.sender != self || capture.context != context ||
        capture.count >= kEquipmentDisjointBatchMaxItems) {
        capture.fallback = true;
        return original(self, sourceSlot, context);
    }
    for (unsigned int index = 0; index < capture.count; ++index) {
        if (capture.slots[index] == sourceSlot) {
            capture.fallback = true;
            return original(self, sourceSlot, context);
        }
    }
    capture.slots[capture.count++] = static_cast<short>(sourceSlot);
    return true;
}

void __fastcall ProxyCurrentEquipmentDisjointBatchLoop(
    void* self, void* /*unused*/)
{
    EquipmentDisjointBatchLoopFn original =
        reinterpret_cast<EquipmentDisjointBatchLoopFn>(
            g_originalEquipmentDisjointBatchLoop);
    if (!original) return;
    if (g_equipmentDisjointBatch.active) {
        original(self);
        return;
    }

    unsigned int selectedCount = 0;
    __try {
        uintptr_t selectionBegin = *reinterpret_cast<uintptr_t*>(
            reinterpret_cast<unsigned char*>(self) +
            kEquipmentDisjointSelectionBeginOffset);
        uintptr_t selectionEnd = *reinterpret_cast<uintptr_t*>(
            reinterpret_cast<unsigned char*>(self) +
            kEquipmentDisjointSelectionEndOffset);
        if (selectionEnd < selectionBegin ||
            (selectionEnd - selectionBegin) % sizeof(uintptr_t) != 0) {
            original(self);
            return;
        }
        selectedCount = static_cast<unsigned int>(
            (selectionEnd - selectionBegin) / sizeof(uintptr_t));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        original(self);
        return;
    }
    if (selectedCount < 2 ||
        selectedCount > kEquipmentDisjointBatchMaxItems) {
        original(self);
        return;
    }

    memset(&g_equipmentDisjointBatch, 0, sizeof(g_equipmentDisjointBatch));
    g_equipmentDisjointBatch.active = true;
    g_equipmentDisjointBatch.expectedCount = selectedCount;
    original(self);
    g_equipmentDisjointBatch.active = false;

    EquipmentDisjointBatchCapture capture = g_equipmentDisjointBatch;
    memset(&g_equipmentDisjointBatch, 0, sizeof(g_equipmentDisjointBatch));
    if (capture.fallback || capture.count < 2 ||
        !SendCurrentEquipmentDisjointBatch(capture)) {
        ReplayCurrentEquipmentDisjointCapturedItems(capture);
    }
}

// ---------------------------------------------------------------------------
// Expert-job (player) disassemble shop: batch the "disassemble all" pass
//
// The NPC machine above and this shop machine drive two different native
// passes: sub_3038260/sub_2330630 send op26, and sub_3038840/sub_2330B80 send
// op235 once per selected item. The shop pass is structurally the same walk,
// so it is batched the same way: the per-item op235 packets are captured and
// replaced by one marked op235 carrying every selected slot.
//
// Why it matters: the stock per-item path makes the client queue N requests
// and open one result popup per request, and each request costs the server a
// full inventory-list refresh. One marked command is settled by the server as
// a single transaction and answered with a single merged op235 body
// (u8 success | u16 slot | u8 list | u16 ownerID | u8 rows | rows | u32 gold |
// u32 endurance), so the player sees the same one-shot behaviour as the NPC
// machine.
//
// Writer bindings, audited from this build and identical to the op26 batch
// above (the names are inherited from the op26 block and are easy to misread):
//   kUpperWriterU16Rva (sub_3454EB0) BEGINS a packet. It stores the opcode in
//     the 13-byte header record and RESETS the body byte counter at
//     writer+0x2BCC2C to zero. It must be called exactly once per packet.
//   kUpperWriterI16Rva (sub_3454FF0) / U8Rva (sub_3454FC0) / U32Rva
//     (sub_3455020) append ONE body value and add 2/1/4 to that counter.
// Writing a body value through the u16 entry would restart the body on every
// write, which puts an op235 with an empty body on the wire.
// ---------------------------------------------------------------------------
constexpr uintptr_t kExpertJobStoreDisjointBatchLoopRva = 0x02C38840; // sub_3038840
constexpr uintptr_t kExpertJobStoreDisjointSendRva = 0x01F30B80; // sub_2330B80
constexpr unsigned short kExpertJobStoreDisjointOpcode = 235;
// "B35\x01" -- mirrors the op26 "B62\x01" marker. The server only treats an
// op235 as a batch when this marker sits at body[5..8], so it also proves the
// envelope was written in the expected order.
constexpr uint32_t kExpertJobStoreDisjointBatchMarker = 0x01353342; // "B35\x01"
constexpr unsigned int kExpertJobStoreDisjointBatchMaxItems = 127;
// Shop-machine window fields. Read natively by the probe and re-checked here:
// an empty selection is just a UI tick, and a non-zero pending pointer is the
// single-item path, which must never be batched.
constexpr uintptr_t kExpertJobStoreDisjointStateOffset = 0x1B8;   // 1 = disassemble-all pass
constexpr uintptr_t kExpertJobStoreDisjointPendingOffset = 0x1EC; // non-zero = single-item path
constexpr uintptr_t kExpertJobStoreDisjointSelectionBeginOffset = 0x424;
constexpr uintptr_t kExpertJobStoreDisjointSelectionEndOffset = 0x428;
// writer+0x2BCC2C is the assembled body byte count; see the writer notes above.
constexpr uintptr_t kUpperWriterBodyCounterOffset = 0x2BCC2C;
constexpr uintptr_t kUpperWriterReadyOffset = 0x8;
constexpr bool kExpertJobStoreDisjointBatchEnabled = true;

struct ExpertJobStoreDisjointBatchCapture {
    bool active;
    bool fallback;
    void* sender;
    unsigned int ownerId;
    unsigned int expectedCount;
    unsigned int count;
    short slots[kExpertJobStoreDisjointBatchMaxItems];
};
thread_local ExpertJobStoreDisjointBatchCapture g_expertJobStoreDisjointBatch = {};
void* g_originalExpertJobStoreDisjointSend = nullptr;
void* g_originalExpertJobStoreDisjointBatchLoop = nullptr;

typedef void (__thiscall* ExpertJobStoreDisjointBatchLoopFn)(void*);
typedef bool (__thiscall* ExpertJobStoreDisjointSendFn)(void*, int, int);

bool SendExpertJobStoreDisjointBatch(
    const ExpertJobStoreDisjointBatchCapture& capture)
{
    if (!g_dnfBase || !capture.sender || capture.count < 2 ||
        capture.count > kExpertJobStoreDisjointBatchMaxItems ||
        capture.count != capture.expectedCount) {
        return false;
    }
    __try {
        UpperWriterValueFn value = reinterpret_cast<UpperWriterValueFn>(
            g_dnfBase + kUpperWriterValueRva);
        UpperWriterScalarFn beginPacket = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU16Rva);
        UpperWriterScalarFn writeU8 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU8Rva);
        UpperWriterScalarFn writeBodyI16 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterI16Rva);
        UpperWriterScalarFn writeU32 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU32Rva);
        UpperWriterFlushFn flush = reinterpret_cast<UpperWriterFlushFn>(
            g_dnfBase + kUpperWriterFlushRva);

        void* writer = value();
        if (!writer ||
            *reinterpret_cast<unsigned char*>(
                reinterpret_cast<unsigned char*>(writer) +
                kUpperWriterReadyOffset) == 0) {
            LogLine("expert-job-store disjoint batch writer not ready");
            return false;
        }

        // Body the server parses, exactly 10 + 2*count bytes:
        //   i16 ownerID | i16 0xFFFF | u8 list | u32 "B35\1" | u8 count | i16 slots[]
        beginPacket(writer, kExpertJobStoreDisjointOpcode);
        writeBodyI16(value(), static_cast<unsigned short>(capture.ownerId & 0xFFFFu));
        writeBodyI16(value(), 0xFFFFu);
        writeU8(value(), kMainInventoryListType);
        writeU32(value(), kExpertJobStoreDisjointBatchMarker);
        writeU8(value(), static_cast<unsigned short>(capture.count));
        for (unsigned int index = 0; index < capture.count; ++index) {
            writeBodyI16(value(),
                static_cast<unsigned short>(capture.slots[index]));
        }

        // beginPacket zeroed the counter and every body write added its own
        // width, so this comparison is an exact check that the whole envelope
        // reached the body. Without it a malformed batch would still flush and
        // the server could only reject it.
        const unsigned int expectedBytes = 10u + 2u * capture.count;
        const unsigned int bodyBytes = *reinterpret_cast<unsigned int*>(
            reinterpret_cast<unsigned char*>(writer) +
            kUpperWriterBodyCounterOffset);
        if (bodyBytes != expectedBytes) {
            LogLine("expert-job-store disjoint batch body=%u want=%u -> replaying natively",
                bodyBytes, expectedBytes);
            return false;
        }
        flush();
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("expert-job-store disjoint batch send exception selected=%u code=0x%08X",
            capture.count, GetExceptionCode());
        return false;
    }
    LogLine("expert-job-store disjoint batch queued selected=%u owner=%u",
        capture.count, capture.ownerId);
    return true;
}

void ReplayExpertJobStoreDisjointCapturedItems(
    const ExpertJobStoreDisjointBatchCapture& capture)
{
    ExpertJobStoreDisjointSendFn original =
        reinterpret_cast<ExpertJobStoreDisjointSendFn>(
            g_originalExpertJobStoreDisjointSend);
    if (!original || !capture.sender || capture.count == 0) return;
    for (unsigned int index = 0; index < capture.count; ++index) {
        original(capture.sender, capture.slots[index],
            static_cast<int>(capture.ownerId));
    }
    LogLine("expert-job-store disjoint batch fallback replayed=%u",
        capture.count);
}

bool __fastcall ProxyExpertJobStoreDisjointSend(
    void* self, void* /*unused*/, int slot, int ownerId)
{
    ExpertJobStoreDisjointSendFn original =
        reinterpret_cast<ExpertJobStoreDisjointSendFn>(
            g_originalExpertJobStoreDisjointSend);
    if (!original) return false;
    ExpertJobStoreDisjointBatchCapture& capture = g_expertJobStoreDisjointBatch;
    if (!capture.active || capture.fallback) {
        return original(self, slot, ownerId);
    }
    // Same acceptance window the native sender uses; anything else is not a
    // plain selected-item packet and must go out untouched.
    if (!self || slot <= 0 || slot > 0x7FFF) {
        capture.fallback = true;
        return original(self, slot, ownerId);
    }
    if (capture.count == 0) {
        capture.sender = self;
        capture.ownerId = static_cast<unsigned int>(ownerId) & 0xFFFFu;
        capture.slots[0] = static_cast<short>(slot);
        capture.count = 1;
        return true;
    }
    if (capture.sender != self ||
        capture.ownerId != (static_cast<unsigned int>(ownerId) & 0xFFFFu) ||
        capture.count >= kExpertJobStoreDisjointBatchMaxItems) {
        capture.fallback = true;
        return original(self, slot, ownerId);
    }
    for (unsigned int index = 0; index < capture.count; ++index) {
        if (capture.slots[index] == static_cast<short>(slot)) {
            capture.fallback = true;
            return original(self, slot, ownerId);
        }
    }
    capture.slots[capture.count++] = static_cast<short>(slot);
    return true;
}

void __fastcall ProxyExpertJobStoreDisjointBatchLoop(
    void* self, void* /*unused*/)
{
    ExpertJobStoreDisjointBatchLoopFn original =
        reinterpret_cast<ExpertJobStoreDisjointBatchLoopFn>(
            g_originalExpertJobStoreDisjointBatchLoop);
    if (!original) return;
    if (g_expertJobStoreDisjointBatch.active) {
        original(self);
        return;
    }

    unsigned int selectedCount = 0;
    __try {
        unsigned char* window = reinterpret_cast<unsigned char*>(self);
        const unsigned int state = *reinterpret_cast<unsigned int*>(
            window + kExpertJobStoreDisjointStateOffset);
        const unsigned int pending = *reinterpret_cast<unsigned int*>(
            window + kExpertJobStoreDisjointPendingOffset);
        uintptr_t selectionBegin = *reinterpret_cast<uintptr_t*>(
            window + kExpertJobStoreDisjointSelectionBeginOffset);
        uintptr_t selectionEnd = *reinterpret_cast<uintptr_t*>(
            window + kExpertJobStoreDisjointSelectionEndOffset);
        // Only the plain "disassemble all" pass is batchable. Everything else
        // -- an empty selection acting as a UI tick, or the single-item path
        // -- must keep the stock per-item behaviour.
        if (state != 1 || pending != 0 ||
            selectionEnd < selectionBegin ||
            (selectionEnd - selectionBegin) % sizeof(uintptr_t) != 0) {
            original(self);
            return;
        }
        selectedCount = static_cast<unsigned int>(
            (selectionEnd - selectionBegin) / sizeof(uintptr_t));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        original(self);
        return;
    }
    if (selectedCount < 2 ||
        selectedCount > kExpertJobStoreDisjointBatchMaxItems) {
        original(self);
        return;
    }

    memset(&g_expertJobStoreDisjointBatch, 0,
        sizeof(g_expertJobStoreDisjointBatch));
    g_expertJobStoreDisjointBatch.active = true;
    g_expertJobStoreDisjointBatch.expectedCount = selectedCount;
    original(self);
    g_expertJobStoreDisjointBatch.active = false;

    ExpertJobStoreDisjointBatchCapture capture = g_expertJobStoreDisjointBatch;
    memset(&g_expertJobStoreDisjointBatch, 0,
        sizeof(g_expertJobStoreDisjointBatch));
    // Two very different outcomes land in the replay branch and both must end
    // with the player's items actually disassembled:
    //   * the native pass never called the sender (its own distance or
    //     eligibility gate refused) -> count is 0, nothing was suppressed, and
    //     the replay is a no-op;
    //   * a send was suppressed but the batch could not be built or written ->
    //     every suppressed slot is put back on the wire one by one.
    if (capture.fallback || capture.count < 2 ||
        capture.count != capture.expectedCount ||
        !SendExpertJobStoreDisjointBatch(capture)) {
        ReplayExpertJobStoreDisjointCapturedItems(capture);
    }
}

// ---------------------------------------------------------------------------
// 【2026-09-25】op416 副职业提取器（附魔师 / 炼金师 / 控偶师）批量提取
//
// 与上面 op26 / op235 两套批量完全同构。客户端侧取证（本机 DNF.exe 实测）：
//   提取窗口的三个模式（模式即 [win+0x1B0]）17 / 18 / 19 都经同一个跳表桩
//   0x2C3A8AD 分发到同一个函数 sub_2C38DA0；它对着选中集合 [+0x420..+0x428]
//   循环、每件调用一次单件发送器 sub_1F2E0B0，循环体内没有任何 ACK 闸门；
//   函数退出后才写 [+0x1B8]=2 与 [+0x434]=选中数。也就是说客户端本来就是
//   把 N 件一趟发出去的，慢在服务端"每件一个事务 + 每件一次整容器重建"——
//   与当年 op235 改造前一模一样。
//
// 三个职业共用同一条协议，差别只有 body[0] 的职业类型：
//   mode 17 → 1 附魔师 / mode 18 → 2 炼金师（炼金提取器）
//   mode 19 → 4 控偶师（灵魂提取器）
//
// 单件发包体（6 字节，服务端 parseCurrentExpertJobExtractionRequest）：
//   u8 extractorType | i16 extractorSlot | u8 targetList | i16 targetSlot
// 批量信封（11 + 2*count 字节，服务端 parseCurrentExpertJobExtractionBatchRequest）：
//   u8 extractorType | i16 extractorSlot | u8 targetList | i16 0xFFFF 哨兵
//   | u32 "B41\x01" | u8 count | i16 slots[count]
// 哨兵占住单件请求的 targetSlot 位置（服务端单件解析器本来就拒绝负 targetSlot），
// 所以老客户端的 6 字节请求逐字节不受影响；服务端不认识信封时会回失败码，
// 因此服务端必须先于本 DLL 上线（见部署说明）。
// ---------------------------------------------------------------------------
constexpr uintptr_t kExpertJobExtractionBatchLoopRva = 0x02C38DA0; // sub_2C38DA0
constexpr uintptr_t kExpertJobExtractionSendRva = 0x01F2E0B0; // sub_1F2E0B0
constexpr unsigned short kExpertJobExtractionOpcode = 416;
// "B41\x01" 小端读作 0x01313442，与服务端 currentExpertJobExtractionBatchMarker 逐字节一致。
constexpr uint32_t kExpertJobExtractionBatchMarker = 0x01313442; // "B41\x01"
constexpr unsigned int kExpertJobExtractionBatchMaxItems = 127;
// 提取机器窗口字段（与 op235 同一对象、同一布局）：
//   +0x1B8 = 1 表示这一趟可以发；+0x1EC 非 0 是"单件定向"，绝不能批量。
constexpr uintptr_t kExpertJobExtractionStateOffset = 0x1B8;
constexpr uintptr_t kExpertJobExtractionPendingOffset = 0x1EC;
constexpr uintptr_t kExpertJobExtractionSelectionBeginOffset = 0x424;
constexpr uintptr_t kExpertJobExtractionSelectionEndOffset = 0x428;
// 信封字节数 = 11(头) + 2*count。
constexpr unsigned int kExpertJobExtractionBatchHeaderBytes = 11;
constexpr bool kExpertJobExtractionBatchEnabled = true;

struct ExpertJobExtractionBatchCapture {
    bool active;
    bool fallback;
    void* sender;              // 目标装备所在容器的 listMgr（循环内应恒定）
    unsigned char extractorType; // 1 附魔师 / 2 炼金师 / 4 控偶师
    short extractorSlot;
    unsigned int expectedCount;
    unsigned int count;
    short slots[kExpertJobExtractionBatchMaxItems];
};
thread_local ExpertJobExtractionBatchCapture g_expertJobExtractionBatch = {};
void* g_originalExpertJobExtractionSend = nullptr;
void* g_originalExpertJobExtractionBatchLoop = nullptr;

typedef void (__thiscall* ExpertJobExtractionBatchLoopFn)(void*);
typedef bool (__thiscall* ExpertJobExtractionSendFn)(void*, int, int, int);

bool SendExpertJobExtractionBatch(
    const ExpertJobExtractionBatchCapture& capture)
{
    if (!g_dnfBase || !capture.sender || capture.count < 2 ||
        capture.count > kExpertJobExtractionBatchMaxItems ||
        capture.count != capture.expectedCount) {
        return false;
    }
    __try {
        UpperWriterValueFn value = reinterpret_cast<UpperWriterValueFn>(
            g_dnfBase + kUpperWriterValueRva);
        UpperWriterScalarFn beginPacket = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU16Rva);
        UpperWriterScalarFn writeU8 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU8Rva);
        UpperWriterScalarFn writeBodyI16 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterI16Rva);
        UpperWriterScalarFn writeU32 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU32Rva);
        UpperWriterFlushFn flush = reinterpret_cast<UpperWriterFlushFn>(
            g_dnfBase + kUpperWriterFlushRva);

        void* writer = value();
        if (!writer ||
            *reinterpret_cast<unsigned char*>(
                reinterpret_cast<unsigned char*>(writer) +
                kUpperWriterReadyOffset) == 0) {
            LogLine("expert-job extraction batch writer not ready");
            return false;
        }

        // Body the server parses, exactly 11 + 2*count bytes (see the block
        // comment above). The sentinel occupies the single-request targetSlot
        // position so the two shapes can never be confused.
        beginPacket(writer, kExpertJobExtractionOpcode);
        writeU8(value(), static_cast<unsigned short>(capture.extractorType));
        writeBodyI16(value(), static_cast<unsigned short>(capture.extractorSlot & 0xFFFFu));
        writeU8(value(), kMainInventoryListType);
        writeBodyI16(value(), 0xFFFFu);
        writeU32(value(), kExpertJobExtractionBatchMarker);
        writeU8(value(), static_cast<unsigned short>(capture.count));
        for (unsigned int index = 0; index < capture.count; ++index) {
            writeBodyI16(value(),
                static_cast<unsigned short>(capture.slots[index]));
        }

        // beginPacket zeroed the counter and every body write added its own
        // width, so this comparison proves the whole envelope reached the body.
        // Without it a malformed batch would still flush and the player would
        // get a server rejection instead of the per-item fallback.
        const unsigned int expectedBytes =
            kExpertJobExtractionBatchHeaderBytes + 2u * capture.count;
        const unsigned int bodyBytes = *reinterpret_cast<unsigned int*>(
            reinterpret_cast<unsigned char*>(writer) +
            kUpperWriterBodyCounterOffset);
        if (bodyBytes != expectedBytes) {
            LogLine("expert-job extraction batch body=%u want=%u -> replaying natively",
                bodyBytes, expectedBytes);
            return false;
        }
        flush();
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("expert-job extraction batch send exception selected=%u code=0x%08X",
            capture.count, GetExceptionCode());
        return false;
    }
    LogLine("expert-job extraction batch queued selected=%u type=%u extractor=%d",
        capture.count, static_cast<unsigned int>(capture.extractorType),
        static_cast<int>(capture.extractorSlot));
    return true;
}

void ReplayExpertJobExtractionCapturedItems(
    const ExpertJobExtractionBatchCapture& capture)
{
    ExpertJobExtractionSendFn original =
        reinterpret_cast<ExpertJobExtractionSendFn>(
            g_originalExpertJobExtractionSend);
    if (!original || !capture.sender || capture.count == 0) return;
    for (unsigned int index = 0; index < capture.count; ++index) {
        original(capture.sender,
            static_cast<int>(capture.extractorType),
            static_cast<int>(capture.extractorSlot),
            static_cast<int>(capture.slots[index]));
    }
    LogLine("expert-job extraction batch fallback replayed=%u", capture.count);
}

bool __fastcall ProxyExpertJobExtractionSend(
    void* self, void* /*unused*/, int extractorType, int extractorSlot,
    int targetSlot)
{
    ExpertJobExtractionSendFn original =
        reinterpret_cast<ExpertJobExtractionSendFn>(
            g_originalExpertJobExtractionSend);
    if (!original) return false;
    ExpertJobExtractionBatchCapture& capture = g_expertJobExtractionBatch;
    if (!capture.active || capture.fallback) {
        return original(self, extractorType, extractorSlot, targetSlot);
    }
    // Same acceptance window the server enforces (type 1/2/4, non-negative
    // slots). Anything else is not a plain selected-item packet and must go out
    // untouched.
    const bool typeSupported = extractorType == 1 || extractorType == 2 ||
        extractorType == 4;
    if (!self || !typeSupported || extractorSlot < 0 || extractorSlot > 0x7FFF ||
        targetSlot < 0 || targetSlot > 0x7FFF) {
        capture.fallback = true;
        return original(self, extractorType, extractorSlot, targetSlot);
    }
    if (capture.count == 0) {
        capture.sender = self;
        capture.extractorType = static_cast<unsigned char>(extractorType);
        capture.extractorSlot = static_cast<short>(extractorSlot);
        capture.slots[0] = static_cast<short>(targetSlot);
        capture.count = 1;
        return true;
    }
    // The whole batch shares one extractor and one target container. A pass
    // that breaks either invariant is not batchable.
    if (capture.sender != self ||
        static_cast<int>(capture.extractorType) != extractorType ||
        static_cast<int>(capture.extractorSlot) != extractorSlot ||
        capture.count >= kExpertJobExtractionBatchMaxItems) {
        capture.fallback = true;
        return original(self, extractorType, extractorSlot, targetSlot);
    }
    for (unsigned int index = 0; index < capture.count; ++index) {
        if (capture.slots[index] == static_cast<short>(targetSlot)) {
            capture.fallback = true;
            return original(self, extractorType, extractorSlot, targetSlot);
        }
    }
    capture.slots[capture.count++] = static_cast<short>(targetSlot);
    return true;
}

void __fastcall ProxyExpertJobExtractionBatchLoop(
    void* self, void* /*unused*/)
{
    ExpertJobExtractionBatchLoopFn original =
        reinterpret_cast<ExpertJobExtractionBatchLoopFn>(
            g_originalExpertJobExtractionBatchLoop);
    if (!original) return;
    if (g_expertJobExtractionBatch.active) {
        original(self);
        return;
    }

    unsigned int selectedCount = 0;
    __try {
        unsigned char* window = reinterpret_cast<unsigned char*>(self);
        const unsigned int state = *reinterpret_cast<unsigned int*>(
            window + kExpertJobExtractionStateOffset);
        const unsigned int pending = *reinterpret_cast<unsigned int*>(
            window + kExpertJobExtractionPendingOffset);
        uintptr_t selectionBegin = *reinterpret_cast<uintptr_t*>(
            window + kExpertJobExtractionSelectionBeginOffset);
        uintptr_t selectionEnd = *reinterpret_cast<uintptr_t*>(
            window + kExpertJobExtractionSelectionEndOffset);
        // Only the plain "extract everything selected" pass is batchable:
        // an empty selection acting as a UI tick, a window that is not in the
        // ready state, or the single-item path must keep stock behaviour.
        if (state != 1 || pending != 0 ||
            selectionEnd < selectionBegin ||
            (selectionEnd - selectionBegin) % sizeof(uintptr_t) != 0) {
            original(self);
            return;
        }
        selectedCount = static_cast<unsigned int>(
            (selectionEnd - selectionBegin) / sizeof(uintptr_t));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        original(self);
        return;
    }
    if (selectedCount < 2 ||
        selectedCount > kExpertJobExtractionBatchMaxItems) {
        original(self);
        return;
    }

    memset(&g_expertJobExtractionBatch, 0,
        sizeof(g_expertJobExtractionBatch));
    g_expertJobExtractionBatch.active = true;
    g_expertJobExtractionBatch.expectedCount = selectedCount;
    original(self);
    g_expertJobExtractionBatch.active = false;

    ExpertJobExtractionBatchCapture capture = g_expertJobExtractionBatch;
    memset(&g_expertJobExtractionBatch, 0,
        sizeof(g_expertJobExtractionBatch));
    // Two outcomes land in the replay branch and both must still extract the
    // player's items:
    //   * the native pass never called the sender (its own eligibility gate
    //     refused) -> count is 0, nothing was suppressed, replay is a no-op;
    //   * a send was suppressed but the envelope could not be built or written
    //     -> every suppressed slot is put back on the wire one by one.
    if (capture.fallback || capture.count < 2 ||
        capture.count != capture.expectedCount ||
        !SendExpertJobExtractionBatch(capture)) {
        ReplayExpertJobExtractionCapturedItems(capture);
    }
}

LONG FindCurrentItemRowsByTemplate(uint32_t templateID, CurrentItemRowTrace* output, LONG outputCapacity)
{
    if (templateID == 0 || !output || outputCapacity <= 0) return 0;
    LONG found = 0;
    AcquireSRWLockShared(&g_socketTraceLock);
    for (LONG i = 0; i < kSocketTraceCacheRows; ++i) {
        const CurrentItemRowTrace& row = g_socketTraceRows[i];
        if (row.sequence == 0 || CurrentItemRowTemplateID(row.raw) != templateID) continue;
        memcpy(&output[found], &row, sizeof(row));
        ++found;
        if (found == outputCapacity) break;
    }
    ReleaseSRWLockShared(&g_socketTraceLock);
    return found;
}

bool ResolveHoveredPremiumContractSelection(CurrentContractSelection* output)
{
    if (!output || !g_dnfBase) return false;
    output->slot = -1;
    output->templateID = 0;
    output->identity = 0;

    void* hovered = nullptr;
    void* hoveredItemData = nullptr;
    uint32_t templateID = 0;
    __try {
        hovered = *reinterpret_cast<void**>(
            g_dnfBase + kHoveredInventoryItemRva);
        if (!hovered) return false;
        void** vtable = *reinterpret_cast<void***>(hovered);
        if (!vtable) return false;
        CurrentItemDataFn itemDataFn =
            reinterpret_cast<CurrentItemDataFn>(
                vtable[0x28 / sizeof(void*)]);
        if (!itemDataFn) return false;
        void* itemData = itemDataFn(hovered);
        if (!itemData) return false;
        hoveredItemData = itemData;
        CurrentItemTemplateReadFn readTemplate =
            reinterpret_cast<CurrentItemTemplateReadFn>(
                g_dnfBase + kCurrentItemTemplateReadRva);
        templateID = readTemplate(
            reinterpret_cast<unsigned char*>(itemData) + 0x14);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("contract-use hovered-item read exception code=0x%08X",
            GetExceptionCode());
        return false;
    }
    if (!IsCurrentPremiumContractID(templateID)) return false;

    // The native selection group is authoritative when the inventory UI has
    // populated it.  Some current item classes reject right-click dispatch
    // before that group is committed, so the passive op14 row cache provides
    // the exact slot as the bounded fallback.
    CurrentContractSelection selected = {};
    if (ResolveCurrentPremiumContractSelection(&selected) &&
        selected.templateID == templateID) {
        *output = selected;
        return true;
    }

    // The passive row parser also observes warehouse and other container
    // rows, which can carry the same template and slot number as list 0.
    // Resolve the hovered template through the current main-inventory manager
    // instead.  Prefer the exact wrapper/item-data object under the mouse; a
    // single same-template main-bag row is the bounded fallback.
    int exactSlot = -1;
    int uniqueSlot = -1;
    int nativeMatchCount = 0;
    __try {
        void* manager = *reinterpret_cast<void**>(
            g_dnfBase + kInventorySelectionManagerRva);
        if (!manager) return false;
        InventorySelectionLookupFn lookup =
            reinterpret_cast<InventorySelectionLookupFn>(
                g_dnfBase + kInventorySelectionLookupRva);
        CurrentItemTemplateReadFn readTemplate =
            reinterpret_cast<CurrentItemTemplateReadFn>(
                g_dnfBase + kCurrentItemTemplateReadRva);
        for (int slot = kCurrentMainInventoryFirstItemSlot;
             slot <= kCurrentMainInventoryLastItemSlot; ++slot) {
            void* candidate = lookup(manager, slot);
            if (!candidate) continue;
            void** vtable = *reinterpret_cast<void***>(candidate);
            if (!vtable) continue;
            CurrentItemDataFn itemDataFn =
                reinterpret_cast<CurrentItemDataFn>(
                    vtable[0x28 / sizeof(void*)]);
            if (!itemDataFn) continue;
            void* candidateItemData = itemDataFn(candidate);
            if (!candidateItemData) continue;
            uint32_t candidateTemplateID = readTemplate(
                reinterpret_cast<unsigned char*>(candidateItemData) + 0x14);
            if (candidateTemplateID != templateID) continue;
            ++nativeMatchCount;
            uniqueSlot = slot;
            if (candidate == hovered || candidateItemData == hoveredItemData) {
                exactSlot = slot;
                break;
            }
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("contract-use main-inventory scan exception item=%lu code=0x%08X",
            static_cast<unsigned long>(templateID), GetExceptionCode());
        return false;
    }
    int slot = exactSlot >= 0
        ? exactSlot
        : (nativeMatchCount == 1 ? uniqueSlot : -1);
    if (slot < 0) {
        LogLine("contract-use main-inventory resolve rejected item=%lu native_matches=%d exact_slot=%d",
            static_cast<unsigned long>(templateID), nativeMatchCount, exactSlot);
        return false;
    }

    uint32_t identity = 0;
    __try {
        CurrentItemIdentityReadFn readIdentity =
            reinterpret_cast<CurrentItemIdentityReadFn>(
                g_dnfBase + kCurrentItemIdentityReadRva);
        identity = readIdentity(templateID);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("contract-use hovered identity read exception item=%lu code=0x%08X",
            static_cast<unsigned long>(templateID), GetExceptionCode());
        return false;
    }
    if (identity == 0) return false;

    output->slot = slot;
    output->templateID = templateID;
    output->identity = identity;
    LogLine("contract-use main-inventory resolved slot=%d item=%lu native_matches=%d exact_hover=%d",
        slot, static_cast<unsigned long>(templateID), nativeMatchCount,
        exactSlot >= 0 ? 1 : 0);
    return true;
}

bool RejectLuaClientNotice(const char* reason)
{
    LONG hit = InterlockedIncrement(&g_luaNoticeRejectedCount);
    if (hit <= 8) {
        LogLine("lua client notice rejected hit=%ld reason=%s", hit, reason);
    }
    return false;
}

const unsigned char* LoadEmbeddedPixelResource(
    int resourceID, size_t expectedBytes)
{
    HMODULE module = GetModuleHandleW(L"90CN.dll");
    if (!module) return nullptr;
    HRSRC resource = FindResourceW(
        module, MAKEINTRESOURCEW(resourceID), RT_RCDATA);
    if (!resource || SizeofResource(module, resource) != expectedBytes) {
        return nullptr;
    }
    HGLOBAL loaded = LoadResource(module, resource);
    return loaded
        ? reinterpret_cast<const unsigned char*>(LockResource(loaded))
        : nullptr;
}

void BlendPremultipliedPixels(
    unsigned char* destination, int destinationWidth,
    int destinationHeight, int destinationX, int destinationY,
    const unsigned char* source, int sourceWidth, int sourceHeight,
    int copyWidth, int copyHeight)
{
    if (!destination || !source || destinationWidth <= 0 ||
        destinationHeight <= 0 || sourceWidth <= 0 || sourceHeight <= 0 ||
        copyWidth <= 0 || copyHeight <= 0 || destinationX < 0 ||
        destinationY < 0 || destinationX + copyWidth > destinationWidth ||
        destinationY + copyHeight > destinationHeight ||
        copyWidth > sourceWidth || copyHeight > sourceHeight) {
        return;
    }

    for (int y = 0; y < copyHeight; ++y) {
        unsigned char* destinationRow = destination +
            ((destinationY + y) * destinationWidth + destinationX) * 4;
        const unsigned char* sourceRow = source + y * sourceWidth * 4;
        for (int x = 0; x < copyWidth; ++x) {
            unsigned char* destinationPixel = destinationRow + x * 4;
            const unsigned char* sourcePixel = sourceRow + x * 4;
            const unsigned int sourceAlpha = sourcePixel[3];
            const unsigned int inverseAlpha = 255 - sourceAlpha;
            for (int channel = 0; channel < 3; ++channel) {
                unsigned int value = sourcePixel[channel] +
                    (destinationPixel[channel] * inverseAlpha + 127) / 255;
                destinationPixel[channel] = static_cast<unsigned char>(
                    value > 255 ? 255 : value);
            }
            unsigned int alpha = sourceAlpha +
                (destinationPixel[3] * inverseAlpha + 127) / 255;
            destinationPixel[3] = static_cast<unsigned char>(
                alpha > 255 ? 255 : alpha);
        }
    }
}

LRESULT CALLBACK LuaNoticeOverlayWindowProc(
    HWND window, UINT message, WPARAM wParam, LPARAM lParam)
{
    switch (message) {
    case WM_ERASEBKGND:
        return 1;
    case WM_NCHITTEST:
        return HTTRANSPARENT;
    case WM_MOUSEACTIVATE:
        return MA_NOACTIVATE;
    case WM_TIMER:
        if (wParam == kLuaNoticeOverlayTimer) {
            KillTimer(window, kLuaNoticeOverlayTimer);
            ShowWindow(window, SW_HIDE);
            return 0;
        }
        break;
    case WM_PAINT: {
        PAINTSTRUCT paint = {};
        HDC dc = BeginPaint(window, &paint);
        RECT client = {};
        GetClientRect(window, &client);

        const COLORREF colorKey = RGB(1, 1, 1);
        HBRUSH clearBrush = CreateSolidBrush(colorKey);
        FillRect(dc, &client, clearBrush);
        DeleteObject(clearBrush);

        HBRUSH panelBrush = CreateSolidBrush(RGB(24, 28, 36));
        HPEN borderPen = CreatePen(PS_SOLID, 2, RGB(230, 184, 74));
        HGDIOBJ oldBrush = SelectObject(dc, panelBrush);
        HGDIOBJ oldPen = SelectObject(dc, borderPen);
        RoundRect(dc, client.left + 2, client.top + 2,
            client.right - 2, client.bottom - 2, 18, 18);
        SelectObject(dc, oldPen);
        SelectObject(dc, oldBrush);
        DeleteObject(borderPen);
        DeleteObject(panelBrush);

        HFONT font = CreateFontW(-24, 0, 0, 0, FW_BOLD, FALSE, FALSE, FALSE,
            DEFAULT_CHARSET, OUT_DEFAULT_PRECIS, CLIP_DEFAULT_PRECIS,
            CLEARTYPE_QUALITY, DEFAULT_PITCH | FF_DONTCARE,
            L"Microsoft YaHei");
        bool ownsFont = font != nullptr;
        if (!font) {
            font = reinterpret_cast<HFONT>(GetStockObject(DEFAULT_GUI_FONT));
        }
        HGDIOBJ oldFont = SelectObject(dc, font);
        SetBkMode(dc, TRANSPARENT);
        SetTextColor(dc, RGB(255, 248, 224));
        RECT textRect = client;
        textRect.left += 18;
        textRect.right -= 18;
        DrawTextW(dc, g_luaNoticeOverlayText, -1, &textRect,
            DT_CENTER | DT_VCENTER | DT_SINGLELINE | DT_END_ELLIPSIS |
            DT_NOPREFIX);
        SelectObject(dc, oldFont);
        if (ownsFont) DeleteObject(font);
        EndPaint(window, &paint);
        return 0;
    }
    case WM_NCDESTROY:
        if (g_luaNoticeOverlayWindow == window) {
            g_luaNoticeOverlayWindow = nullptr;
        }
        break;
    default:
        break;
    }
    return DefWindowProcW(window, message, wParam, lParam);
}

bool EnsureLuaNoticeOverlayWindow(HWND parent)
{
    if (g_luaNoticeOverlayWindow &&
        IsWindow(g_luaNoticeOverlayWindow)) {
        return true;
    }

    HINSTANCE module = reinterpret_cast<HINSTANCE>(
        GetModuleHandleW(L"90CN.dll"));
    WNDCLASSEXW windowClass = {};
    windowClass.cbSize = sizeof(windowClass);
    windowClass.style = CS_HREDRAW | CS_VREDRAW;
    windowClass.lpfnWndProc = &LuaNoticeOverlayWindowProc;
    windowClass.hInstance = module;
    windowClass.lpszClassName = kLuaNoticeOverlayClassName;
    if (!RegisterClassExW(&windowClass) &&
        GetLastError() != ERROR_CLASS_ALREADY_EXISTS) {
        LogLine("lua client notice overlay class registration failed error=%lu",
            GetLastError());
        return false;
    }

    HWND overlay = CreateWindowExW(
        WS_EX_LAYERED | WS_EX_TRANSPARENT | WS_EX_NOACTIVATE |
            WS_EX_TOOLWINDOW,
        kLuaNoticeOverlayClassName, L"", WS_POPUP,
        0, 0, 0, 0, parent, nullptr, module, nullptr);
    if (!overlay) {
        LogLine("lua client notice overlay creation failed error=%lu",
            GetLastError());
        return false;
    }
    if (!SetLayeredWindowAttributes(
            overlay, RGB(1, 1, 1), 238, LWA_COLORKEY | LWA_ALPHA)) {
        LogLine("lua client notice overlay alpha setup failed error=%lu",
            GetLastError());
        DestroyWindow(overlay);
        return false;
    }
    g_luaNoticeOverlayWindow = overlay;
    return true;
}

bool ShowLuaClientNoticeOverlay(
    HWND parent, const LuaClientNotice& notice)
{
    if (!EnsureLuaNoticeOverlayWindow(parent)) return false;

    memcpy_s(g_luaNoticeOverlayText, sizeof(g_luaNoticeOverlayText),
        notice.text, (notice.length + 1) * sizeof(wchar_t));

    RECT parentClient = {};
    if (!GetClientRect(parent, &parentClient)) return false;
    int parentWidth = parentClient.right - parentClient.left;
    int parentHeight = parentClient.bottom - parentClient.top;
    int width = parentWidth - 40;
    if (width > 900) width = 900;
    if (width < 280) width = 280;
    int height = 58;
    int x = (parentWidth - width) / 2;
    if (x < 0) x = 0;
    int y = parentHeight >= 400 ? 70 : 20;
    POINT screenPosition = { x, y };
    if (!ClientToScreen(parent, &screenPosition)) return false;

    InvalidateRect(g_luaNoticeOverlayWindow, nullptr, TRUE);
    SetWindowPos(g_luaNoticeOverlayWindow, parent,
        screenPosition.x, screenPosition.y, width, height,
        SWP_NOACTIVATE | SWP_SHOWWINDOW);
    UpdateWindow(g_luaNoticeOverlayWindow);
    SetTimer(g_luaNoticeOverlayWindow, kLuaNoticeOverlayTimer,
        kLuaNoticeOverlayDurationMs, nullptr);
    return true;
}

bool ValidateNativeDungeonSelectNoticeEntrypoints()
{
    if (!g_dnfBase) return false;
    static const unsigned char kSehPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    static const unsigned char kReleasePrologue[] = {
        0x56, 0x8B, 0x71, 0x04, 0x85, 0xF6
    };
    static const unsigned char kSceneContextBody[] = {
        0x8B, 0x41, 0x50, 0xC3
    };
    static const unsigned char kChatPrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x40
    };
    return BytesMatch(reinterpret_cast<const unsigned char*>(
                          g_dnfBase + kSelectorNoticeConstructRva),
               kSehPrologue, sizeof(kSehPrologue)) &&
        BytesMatch(reinterpret_cast<const unsigned char*>(
                       g_dnfBase + kSelectorNoticeManagerRva),
            kSehPrologue, sizeof(kSehPrologue)) &&
        BytesMatch(reinterpret_cast<const unsigned char*>(
                       g_dnfBase + kSelectorNoticeEnqueueRva),
            kSehPrologue, sizeof(kSehPrologue)) &&
        BytesMatch(reinterpret_cast<const unsigned char*>(
                       g_dnfBase + kSelectorNoticeDestroyRva),
            kSehPrologue, sizeof(kSehPrologue)) &&
        BytesMatch(reinterpret_cast<const unsigned char*>(
                       g_dnfBase + kReleaseSharedHandleRva),
            kReleasePrologue, sizeof(kReleasePrologue)) &&
        BytesMatch(reinterpret_cast<const unsigned char*>(
                       g_dnfBase + kSceneHudContextRva),
            kSceneContextBody, sizeof(kSceneContextBody)) &&
        BytesMatch(reinterpret_cast<const unsigned char*>(
                       g_dnfBase + kChatNoticeEnqueueRva),
            kChatPrologue, sizeof(kChatPrologue));
}

bool ShowNativeDungeonSelectFatiguePrompt(const LuaClientNotice& notice)
{
    if (notice.style != kClientNoticeStyleDungeonSelect ||
        !ValidateNativeDungeonSelectNoticeEntrypoints()) {
        LogLine("dungeon-select native prompt rejected reason=entrypoint-validation style=%u",
            notice.style);
        return false;
    }

    SelectorNoticeConstructFn constructNotice =
        reinterpret_cast<SelectorNoticeConstructFn>(
            g_dnfBase + kSelectorNoticeConstructRva);
    SelectorNoticeManagerFn acquireManager =
        reinterpret_cast<SelectorNoticeManagerFn>(
            g_dnfBase + kSelectorNoticeManagerRva);
    SelectorNoticeEnqueueFn enqueueNotice =
        reinterpret_cast<SelectorNoticeEnqueueFn>(
            g_dnfBase + kSelectorNoticeEnqueueRva);
    ReleaseSharedHandleFn releaseManager =
        reinterpret_cast<ReleaseSharedHandleFn>(
            g_dnfBase + kReleaseSharedHandleRva);
    SelectorNoticeDestroyFn destroyNotice =
        reinterpret_cast<SelectorNoticeDestroyFn>(
            g_dnfBase + kSelectorNoticeDestroyRva);
    SceneHudContextFn getChatContext =
        reinterpret_cast<SceneHudContextFn>(
            g_dnfBase + kSceneHudContextRva);
    ChatNoticeEnqueueFn enqueueChat =
        reinterpret_cast<ChatNoticeEnqueueFn>(
            g_dnfBase + kChatNoticeEnqueueRva);

    unsigned int noticeObject[25] = {};
    void* managerHandle[2] = {};
    bool selectorQueued = false;
    bool chatQueued = false;
    __try {
        void* sceneOwner = *reinterpret_cast<void**>(
            g_dnfBase + kSceneRootPointerRva);
        void* chatContext = sceneOwner
            ? getChatContext(sceneOwner)
            : nullptr;
        if (chatContext) {
            enqueueChat(chatContext, notice.text, 0xFF9BC8E6u,
                22, 0, 0, 0, 0);
            chatQueued = true;
        }

        constructNotice(noticeObject, notice.text,
            2000, 0xFF93C5DDu, 5, 1, 1000);
        acquireManager(managerHandle);
        if (managerHandle[0]) {
            enqueueNotice(managerHandle[0], noticeObject);
            selectorQueued = true;
        }
        releaseManager(reinterpret_cast<unsigned int*>(managerHandle));
        destroyNotice(noticeObject);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("dungeon-select native prompt exception code=0x%08X selector=%u chat=%u",
            GetExceptionCode(), selectorQueued ? 1u : 0u,
            chatQueued ? 1u : 0u);
        return false;
    }

    LogLine("dungeon-select native prompt selector=%u chat=%u chars=%u",
        selectorQueued ? 1u : 0u, chatQueued ? 1u : 0u,
        notice.length);
    return selectorQueued && chatQueued;
}

const wchar_t* CombatPowerRankName(unsigned int score)
{
    if (score < 3100) return L"探索";
    if (score < 8100) return L"开拓";
    if (score < 23000) return L"无畏";
    if (score < 52000) return L"征服";
    if (score < 65000) return L"战绝";
    if (score < 130000) return L"英杰";
    return L"武炼";
}

bool TryComputeCombatPowerEquipmentBonusHundredths(
    const DNF90CombatPanelState& state, unsigned int* output)
{
    if (!output || state.baseAttributeScore == 0 ||
        (state.validFlags & DNF90_COMBAT_PANEL_BASE_SCORE_VALID) == 0 ||
        (state.validFlags & DNF90_COMBAT_PANEL_EQUIPMENT_SCORE_VALID) == 0 ||
        (state.validFlags & DNF90_COMBAT_PANEL_DAMAGE_AFFIXES_VALID) == 0) {
        return false;
    }

    // Lua's equipment score contains both the three visible damage-affix
    // multiplier score and the remaining equipment-derived score. Remove the
    // white/yellow/critical part first so the "equipment" percentage is an
    // independent row and never counts the displayed affixes twice.
    const unsigned int yellowTenths = state.yellowDamageTenths +
        state.yellowAdditionalTenths;
    const unsigned int criticalTenths = state.criticalDamageTenths +
        state.criticalAdditionalTenths;
    const long double multiplier =
        (1.0L + static_cast<long double>(state.whiteDamageTenths) / 1000.0L) *
        (1.0L + static_cast<long double>(yellowTenths) / 1000.0L) *
        (1.0L + static_cast<long double>(criticalTenths) / 1000.0L);
    const long double affixValue =
        static_cast<long double>(state.baseAttributeScore) *
        (multiplier - 1.0L);
    unsigned int affixScore = 0;
    if (affixValue >= static_cast<long double>(state.equipmentScore)) {
        affixScore = state.equipmentScore;
    } else if (affixValue > 0.0L) {
        affixScore = static_cast<unsigned int>(affixValue);
    }
    const unsigned int equipmentOnlyScore =
        state.equipmentScore - affixScore;
    const unsigned long long scaled =
        static_cast<unsigned long long>(equipmentOnlyScore) * 10000ull;
    const unsigned long long hundredths =
        scaled / state.baseAttributeScore;
    if (hundredths > 999999ull) return false;
    *output = static_cast<unsigned int>(hundredths);
    return true;
}

int CombatPowerRankIconResource(unsigned int score)
{
    if (score < 3100) return IDR_COMBAT_RANK_EXPLORE_V3;
    if (score < 8100) return IDR_COMBAT_RANK_PIONEER_V3;
    if (score < 23000) return IDR_COMBAT_RANK_FEARLESS_V3;
    if (score < 52000) return IDR_COMBAT_RANK_CONQUER_V3;
    if (score < 65000) return IDR_COMBAT_RANK_BATTLE_PINNACLE_V3;
    if (score < 130000) return IDR_COMBAT_RANK_HEROIC_V3;
    return IDR_COMBAT_RANK_MARTIAL_MASTERY_V3;
}

HFONT CreateCombatPanelFont(int pixelHeight, int weight)
{
    HFONT font = CreateFontW(-pixelHeight, 0, 0, 0, weight,
        FALSE, FALSE, FALSE, DEFAULT_CHARSET, OUT_DEFAULT_PRECIS,
        CLIP_DEFAULT_PRECIS, CLEARTYPE_QUALITY,
        DEFAULT_PITCH | FF_DONTCARE, L"Microsoft YaHei");
    return font ? font : reinterpret_cast<HFONT>(
        GetStockObject(DEFAULT_GUI_FONT));
}

void DrawCombatPanelText(HDC dc, const wchar_t* text, RECT bounds,
    int pixelHeight, int weight, COLORREF color, UINT format)
{
    HFONT font = CreateCombatPanelFont(pixelHeight, weight);
    HGDIOBJ oldFont = SelectObject(dc, font);
    SetBkMode(dc, TRANSPARENT);
    SetTextColor(dc, color);
    DrawTextW(dc, text, -1, &bounds, format | DT_NOPREFIX);
    SelectObject(dc, oldFont);
    if (font != GetStockObject(DEFAULT_GUI_FONT)) DeleteObject(font);
}

void DrawCombatPanelTextShadowed(HDC dc, const wchar_t* text, RECT bounds,
    int pixelHeight, int weight, COLORREF color, UINT format)
{
    RECT shadow = bounds;
    OffsetRect(&shadow, 1, 1);
    DrawCombatPanelText(dc, text, shadow, pixelHeight, weight,
        RGB(0, 0, 0), format);
    DrawCombatPanelText(dc, text, bounds, pixelHeight, weight, color, format);
}

bool DrawEmbeddedBgraResource(HDC dc, int resourceID, int width, int height)
{
    const size_t expectedBytes = static_cast<size_t>(width) * height * 4;
    const unsigned char* pixels = LoadEmbeddedPixelResource(
        resourceID, expectedBytes);
    if (!pixels) return false;

    BITMAPINFO bitmapInfo = {};
    bitmapInfo.bmiHeader.biSize = sizeof(bitmapInfo.bmiHeader);
    bitmapInfo.bmiHeader.biWidth = width;
    bitmapInfo.bmiHeader.biHeight = -height;
    bitmapInfo.bmiHeader.biPlanes = 1;
    bitmapInfo.bmiHeader.biBitCount = 32;
    bitmapInfo.bmiHeader.biCompression = BI_RGB;
    return StretchDIBits(dc,
        0, 0, width, height,
        0, 0, width, height,
        pixels, &bitmapInfo, DIB_RGB_COLORS, SRCCOPY) != GDI_ERROR;
}

bool DrawCombatPanelSkinWithRankIcon(HDC dc, unsigned int score)
{
    const size_t panelBytes = static_cast<size_t>(kCombatPanelWidth) *
        kCombatPanelSkinHeight * 4;
    const size_t iconBytes = static_cast<size_t>(kCombatRankIconSize) *
        kCombatRankIconSize * 4;
    const unsigned char* panel = LoadEmbeddedPixelResource(
        IDR_COMBAT_POWER_PANEL_SKIN_V2, panelBytes);
    const unsigned char* icon = LoadEmbeddedPixelResource(
        CombatPowerRankIconResource(score), iconBytes);
    if (!panel || !icon) return false;

    unsigned char* composed = static_cast<unsigned char*>(
        HeapAlloc(GetProcessHeap(), 0, panelBytes));
    if (!composed) return false;
    memcpy(composed, panel, panelBytes);
    BlendPremultipliedPixels(
        composed, kCombatPanelWidth, kCombatPanelSkinHeight,
        kCombatRankIconX, kCombatRankIconY,
        icon, kCombatRankIconSize, kCombatRankIconSize,
        kCombatRankIconSize, kCombatRankIconSize);

    BITMAPINFO bitmapInfo = {};
    bitmapInfo.bmiHeader.biSize = sizeof(bitmapInfo.bmiHeader);
    bitmapInfo.bmiHeader.biWidth = kCombatPanelWidth;
    bitmapInfo.bmiHeader.biHeight = -kCombatPanelSkinHeight;
    bitmapInfo.bmiHeader.biPlanes = 1;
    bitmapInfo.bmiHeader.biBitCount = 32;
    bitmapInfo.bmiHeader.biCompression = BI_RGB;
    const bool drawn = StretchDIBits(dc,
        0, 0, kCombatPanelWidth, kCombatPanelSkinHeight,
        0, 0, kCombatPanelWidth, kCombatPanelSkinHeight,
        composed, &bitmapInfo, DIB_RGB_COLORS, SRCCOPY) != GDI_ERROR;
    HeapFree(GetProcessHeap(), 0, composed);
    return drawn;
}

void DrawCombatPanelAffixExtension(HDC dc)
{
    // The stock skin ends after the fourth affix row. Continue its dark metal
    // rail in GDI so the original bitmap remains byte-for-byte compatible and
    // the two newly separated PVF categories scale with the logical panel.
    RECT background = {
        0, kCombatPanelSkinHeight - 1,
        kCombatPanelWidth, kCombatPanelHeight,
    };
    HBRUSH outer = CreateSolidBrush(RGB(3, 8, 13));
    FillRect(dc, &background, outer);
    DeleteObject(outer);

    RECT inner = {
        3, kCombatPanelSkinHeight - 1,
        kCombatPanelWidth - 3, kCombatPanelHeight - 3,
    };
    HBRUSH innerBrush = CreateSolidBrush(RGB(5, 18, 29));
    FillRect(dc, &inner, innerBrush);
    DeleteObject(innerBrush);

    HPEN rail = CreatePen(PS_SOLID, 1, RGB(60, 103, 130));
    HGDIOBJ oldPen = SelectObject(dc, rail);
    MoveToEx(dc, 1, kCombatPanelSkinHeight - 1, nullptr);
    LineTo(dc, 1, kCombatPanelHeight - 1);
    LineTo(dc, kCombatPanelWidth - 1, kCombatPanelHeight - 1);
    LineTo(dc, kCombatPanelWidth - 1, kCombatPanelSkinHeight - 1);
    SelectObject(dc, oldPen);
    DeleteObject(rail);

    HPEN separator = CreatePen(PS_SOLID, 1, RGB(33, 65, 85));
    oldPen = SelectObject(dc, separator);
    MoveToEx(dc, 7, 414, nullptr);
    LineTo(dc, 111, 414);
    SelectObject(dc, oldPen);
    DeleteObject(separator);
}

void DrawCombatPanelProgressBar(HDC dc, int left, int top, int width,
    unsigned int numerator, unsigned int denominator, COLORREF fillColor)
{
    if (width <= 2) return;
    HBRUSH track = CreateSolidBrush(RGB(3, 11, 19));
    RECT trackRect = { left, top, left + width, top + 6 };
    FillRect(dc, &trackRect, track);
    DeleteObject(track);

    HPEN border = CreatePen(PS_SOLID, 1, RGB(47, 84, 105));
    HGDIOBJ oldPen = SelectObject(dc, border);
    HGDIOBJ oldBrush = SelectObject(dc, GetStockObject(NULL_BRUSH));
    Rectangle(dc, trackRect.left, trackRect.top,
        trackRect.right, trackRect.bottom);
    SelectObject(dc, oldBrush);
    SelectObject(dc, oldPen);
    DeleteObject(border);

    if (denominator == 0 || numerator == 0) return;
    unsigned int fill = static_cast<unsigned int>(
        (static_cast<unsigned long long>(width - 2) * numerator) /
        denominator);
    if (fill > static_cast<unsigned int>(width - 2)) fill = width - 2;
    HBRUSH value = CreateSolidBrush(fillColor);
    RECT valueRect = { left + 1, top + 1,
        left + 1 + static_cast<int>(fill), top + 5 };
    FillRect(dc, &valueRect, value);
    DeleteObject(value);
}

#if 0
void DrawCombatPanelMainLegacy(HDC dc, const DNF90CombatPanelState& state)
{
    const bool baseValid =
        (state.validFlags & DNF90_COMBAT_PANEL_BASE_SCORE_VALID) != 0;
    const bool affixesValid =
        (state.validFlags & DNF90_COMBAT_PANEL_DAMAGE_AFFIXES_VALID) != 0;
    const bool skinDrawn = baseValid
        ? DrawCombatPanelSkinWithRankIcon(dc, state.totalScore)
        : DrawEmbeddedBgraResource(dc, IDR_COMBAT_POWER_PANEL_SKIN_V2,
            kCombatPanelWidth, kCombatPanelSkinHeight);
    if (!skinDrawn) {
        RECT fallback = { 0, 0, kCombatPanelWidth, kCombatPanelHeight };
        HBRUSH background = CreateSolidBrush(RGB(5, 17, 28));
        FillRect(dc, &fallback, background);
        DeleteObject(background);
    }
    DrawCombatPanelAffixExtension(dc);

    wchar_t line[64] = {};

    RECT title = { 7, 5, 113, 28 };
    DrawCombatPanelTextShadowed(dc, L"我的战斗力值", title, 13, FW_BOLD,
        RGB(225, 242, 251), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
    RECT help = { 105, 25, 122, 42 };
    DrawCombatPanelTextShadowed(dc, L"?", help, 11, FW_BOLD,
        RGB(245, 202, 91), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    if (!baseValid) {
        RECT medalText = { 37, 43, 91, 101 };
        DrawCombatPanelTextShadowed(dc, L"战", medalText, 27, FW_BOLD,
            RGB(232, 247, 255), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
    }
    if (baseValid) {
        swprintf_s(line, L"%u", state.totalScore);
    } else {
        wcscpy_s(line, L"--");
    }
    RECT total = { 8, 116, 120, 143 };
    DrawCombatPanelTextShadowed(dc, line, total, 19, FW_BOLD,
        RGB(255, 225, 101), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    const wchar_t* rank = baseValid
        ? CombatPowerRankName(state.totalScore)
        : L"--";
    // The selected TGP badge has a dedicated empty nameplate below the
    // emblem.  Keep the rank inside that plate instead of spending a separate
    // row below the total score.
    RECT rankRect = { 34, 96, 94, 114 };
    DrawCombatPanelTextShadowed(dc, rank, rankRect, 9, FW_BOLD,
        RGB(245, 226, 171), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    RECT baseLabel = { 9, 158, 119, 176 };
    DrawCombatPanelTextShadowed(dc, L"基础属性加成", baseLabel, 10, FW_BOLD,
        RGB(184, 224, 242), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
    if (baseValid) swprintf_s(line, L"%u", state.baseAttributeScore);
    else wcscpy_s(line, L"--");
    RECT baseScore = { 9, 174, 119, 195 };
    DrawCombatPanelTextShadowed(dc, line, baseScore, 12, FW_BOLD,
        RGB(103, 218, 255), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    RECT equipmentLabel = { 9, 198, 119, 225 };
    DrawCombatPanelTextShadowed(dc, L"装备加成", equipmentLabel, 10,
        FW_BOLD, RGB(184, 224, 242),
        DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    RECT affix = { 12, 227, 116, 252 };
    if (affixesValid) {
        swprintf_s(line, L"白字  %u.%u0%%",
            state.whiteDamageTenths / 10,
            state.whiteDamageTenths % 10);
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(225, 236, 242), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
        swprintf_s(line, L"黄字  %u.%u0%%",
            state.yellowDamageTenths / 10,
            state.yellowDamageTenths % 10);
        affix = { 12, 252, 116, 277 };
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(255, 216, 95), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
        swprintf_s(line, L"爆伤  %u.%u0%%",
            state.criticalDamageTenths / 10,
            state.criticalDamageTenths % 10);
        affix = { 12, 277, 116, 302 };
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(255, 174, 104), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
        swprintf_s(line, L"黄追  %u.%u0%%",
            state.yellowAdditionalTenths / 10,
            state.yellowAdditionalTenths % 10);
        affix = { 12, 302, 116, 327 };
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(249, 232, 145), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
        swprintf_s(line, L"爆追  %u.%u0%%",
            state.criticalAdditionalTenths / 10,
            state.criticalAdditionalTenths % 10);
        affix = { 12, 327, 116, 352 };
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(246, 190, 132), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
        swprintf_s(line, L"全攻  %u.%u0%%",
            state.allAttackTenths / 10,
            state.allAttackTenths % 10);
        affix = { 12, 352, 116, 377 };
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(161, 216, 241), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
    } else {
        const wchar_t* pendingRows[] = {
            L"白字  --", L"黄字  --", L"爆伤  --",
            L"黄追  --", L"爆追  --", L"全攻  --",
        };
        for (size_t index = 0;
             index < sizeof(pendingRows) / sizeof(pendingRows[0]); ++index) {
            affix.top = 227 + static_cast<LONG>(index) * 25;
            affix.bottom = affix.top + 25;
            DrawCombatPanelTextShadowed(dc, pendingRows[index], affix, 10,
                FW_NORMAL, RGB(143, 193, 215),
                DT_CENTER | DT_VCENTER | DT_SINGLELINE);
        }
    }
}

#endif

void DrawCombatPanelMain(HDC dc, const DNF90CombatPanelState& state)
{
    const bool baseValid =
        (state.validFlags & DNF90_COMBAT_PANEL_BASE_SCORE_VALID) != 0;
    const bool affixesValid =
        (state.validFlags & DNF90_COMBAT_PANEL_DAMAGE_AFFIXES_VALID) != 0;
    const bool skinDrawn = baseValid
        ? DrawCombatPanelSkinWithRankIcon(dc, state.totalScore)
        : DrawEmbeddedBgraResource(dc, IDR_COMBAT_POWER_PANEL_SKIN_V2,
            kCombatPanelWidth, kCombatPanelSkinHeight);
    if (!skinDrawn) {
        RECT fallback = { 0, 0, kCombatPanelWidth, kCombatPanelHeight };
        HBRUSH background = CreateSolidBrush(RGB(5, 17, 28));
        FillRect(dc, &fallback, background);
        DeleteObject(background);
    }
    DrawCombatPanelAffixExtension(dc);

    // 标题、问号、分区标题和金属格子来自原版位图；这里只绘制实时数据。
    wchar_t line[64] = {};
    if (baseValid) swprintf_s(line, L"%u", state.totalScore);
    else wcscpy_s(line, L"--");
    RECT total = { 5, 128, 113, 153 };
    DrawCombatPanelTextShadowed(dc, line, total, 18, FW_BOLD,
        RGB(255, 225, 101), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    const wchar_t* rank = baseValid
        ? CombatPowerRankName(state.totalScore)
        : L"--";
    HBRUSH rankPlate = CreateSolidBrush(RGB(78, 52, 25));
    RECT rankPlateRect = { 35, 102, 83, 117 };
    FillRect(dc, &rankPlateRect, rankPlate);
    DeleteObject(rankPlate);
    // The copper nameplate is visually one pixel right of the panel's
    // mathematical center. Center against the plate itself and use the next
    // font size so two-character ranks fill it like the original TGP panel.
    RECT rankRect = { 32, 100, 88, 120 };
    DrawCombatPanelTextShadowed(dc, rank, rankRect, 10, FW_BOLD,
        RGB(245, 226, 171), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    wchar_t profession[32] = L"--";
    const bool identityValid =
        (state.validFlags & DNF90_COMBAT_PANEL_IDENTITY_VALID) != 0;
    if (identityValid && state.professionUtf8[0] != '\0') {
        wchar_t converted[32] = {};
        const int convertedCount = MultiByteToWideChar(
            CP_UTF8, MB_ERR_INVALID_CHARS, state.professionUtf8, -1,
            converted, static_cast<int>(_countof(converted)));
        if (convertedCount > 0) wcscpy_s(profession, converted);
    }
    if (identityValid) swprintf_s(line, L"%s  Lv.%u", profession, state.level);
    else wcscpy_s(line, L"--  Lv.--");
    RECT identity = { 5, 153, 113, 177 };
    DrawCombatPanelTextShadowed(dc, line, identity, 10, FW_NORMAL,
        RGB(220, 229, 232), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    if (baseValid) swprintf_s(line, L"%u", state.baseAttributeScore);
    else wcscpy_s(line, L"--");
    RECT baseScore = { 5, 224, 113, 251 };
    DrawCombatPanelTextShadowed(dc, line, baseScore, 12, FW_BOLD,
        RGB(103, 218, 255), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    RECT affix = { 7, 274, 111, 302 };
    if (affixesValid) {
        swprintf_s(line, L"白字  %u.%u0%%",
            state.whiteDamageTenths / 10,
            state.whiteDamageTenths % 10);
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(225, 236, 242), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        swprintf_s(line, L"黄字  %u.%u0%%",
            state.yellowDamageTenths / 10,
            state.yellowDamageTenths % 10);
        affix = { 7, 302, 111, 330 };
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(255, 216, 95), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        unsigned int equipmentBonusHundredths = 0;
        affix = { 7, 330, 111, 358 };
        if (TryComputeCombatPowerEquipmentBonusHundredths(
                state, &equipmentBonusHundredths)) {
            swprintf_s(line, L"装备  %u.%02u%%",
                equipmentBonusHundredths / 100,
                equipmentBonusHundredths % 100);
        } else {
            wcscpy_s(line, L"装备  --");
        }
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(255, 174, 104), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        swprintf_s(line, L"爆伤  %u.%u0%%",
            state.criticalDamageTenths / 10,
            state.criticalDamageTenths % 10);
        affix = { 7, 358, 111, 386 };
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(139, 218, 244), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        swprintf_s(line, L"黄追  %u.%u0%%",
            state.yellowAdditionalTenths / 10,
            state.yellowAdditionalTenths % 10);
        affix = { 7, 386, 111, 414 };
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(255, 202, 77), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        swprintf_s(line, L"爆追  %u.%u0%%",
            state.criticalAdditionalTenths / 10,
            state.criticalAdditionalTenths % 10);
        affix = { 7, 414, 111, 442 };
        DrawCombatPanelTextShadowed(dc, line, affix, 10, FW_NORMAL,
            RGB(125, 205, 246), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
    } else {
        const wchar_t* pendingRows[] = {
            L"白字  --", L"黄字  --", L"装备  --", L"爆伤  --",
            L"黄追  --", L"爆追  --",
        };
        for (size_t index = 0;
             index < sizeof(pendingRows) / sizeof(pendingRows[0]); ++index) {
            affix.top = 274 + static_cast<LONG>(index) * 28;
            affix.bottom = affix.top + 28;
            DrawCombatPanelTextShadowed(dc, pendingRows[index], affix, 10,
                FW_NORMAL, RGB(143, 193, 215),
                DT_CENTER | DT_VCENTER | DT_SINGLELINE);
        }
    }
}

void DrawCombatPanelRankTooltip(HDC dc,
    const DNF90CombatPanelState& state)
{
    if (!DrawEmbeddedBgraResource(dc, IDR_COMBAT_POWER_RANK_TOOLTIP_V2,
            kCombatPanelRankTooltipWidth,
            kCombatPanelRankTooltipHeight)) {
        RECT fallback = { 0, 0, kCombatPanelRankTooltipWidth,
            kCombatPanelRankTooltipHeight };
        HBRUSH background = CreateSolidBrush(RGB(5, 17, 28));
        FillRect(dc, &fallback, background);
        DeleteObject(background);
    }

    RECT rangeHeader = { 19, 17, 112, 43 };
    RECT rankHeader = { 116, 17, 212, 43 };
    DrawCombatPanelTextShadowed(dc, L"战斗力区间", rangeHeader, 12,
        FW_BOLD, RGB(211, 229, 239),
        DT_CENTER | DT_VCENTER | DT_SINGLELINE);
    DrawCombatPanelTextShadowed(dc, L"段位等级", rankHeader, 12,
        FW_BOLD, RGB(211, 229, 239),
        DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    struct RankRow {
        unsigned int lower;
        unsigned int upper;
        const wchar_t* range;
        const wchar_t* name;
    };
    const RankRow rows[] = {
        { 0, 3099, L"0 - 3100", L"探索" },
        { 3100, 8099, L"3100 - 8100", L"开拓" },
        { 8100, 22999, L"8100 - 23000", L"无畏" },
        { 23000, 51999, L"23000 - 52000", L"征服" },
        { 52000, 64999, L"52000 - 65000", L"战绝" },
        { 65000, 129999, L"65000 - 130000", L"英杰" },
        { 130000, 0xFFFFFFFFu, L"130000 以上", L"武炼" },
    };
    for (size_t index = 0; index < sizeof(rows) / sizeof(rows[0]); ++index) {
        const int top = 43 + static_cast<int>(index) * 29;
        const int bottom = top + 29;
        if ((state.validFlags & DNF90_COMBAT_PANEL_BASE_SCORE_VALID) != 0 &&
            state.totalScore >= rows[index].lower &&
            state.totalScore <= rows[index].upper) {
            HPEN highlight = CreatePen(PS_SOLID, 1, RGB(95, 200, 240));
            HGDIOBJ oldPen = SelectObject(dc, highlight);
            HGDIOBJ oldBrush = SelectObject(dc, GetStockObject(NULL_BRUSH));
            Rectangle(dc, 17, top + 1, 213, bottom);
            SelectObject(dc, oldBrush);
            SelectObject(dc, oldPen);
            DeleteObject(highlight);
        }
        RECT range = { 20, top, 112, bottom };
        RECT name = { 116, top, 211, bottom };
        DrawCombatPanelTextShadowed(dc, rows[index].range, range, 11,
            FW_NORMAL, RGB(190, 211, 224),
            DT_CENTER | DT_VCENTER | DT_SINGLELINE);
        DrawCombatPanelTextShadowed(dc, rows[index].name, name, 11,
            FW_BOLD, RGB(236, 218, 170),
            DT_CENTER | DT_VCENTER | DT_SINGLELINE);
    }
}

void DrawCombatPanelGuide(HDC dc, const DNF90CombatPanelState& state)
{
    RECT client = { 0, 0, kCombatPanelGuideWidth, kCombatPanelGuideHeight };
    HBRUSH background = CreateSolidBrush(RGB(5, 16, 27));
    FillRect(dc, &client, background);
    DeleteObject(background);

    HBRUSH inner = CreateSolidBrush(RGB(8, 29, 45));
    HPEN outerPen = CreatePen(PS_SOLID, 1, RGB(123, 177, 202));
    HGDIOBJ oldBrush = SelectObject(dc, inner);
    HGDIOBJ oldPen = SelectObject(dc, outerPen);
    Rectangle(dc, 1, 1, client.right - 1, client.bottom - 1);
    SelectObject(dc, oldPen);
    SelectObject(dc, oldBrush);
    DeleteObject(outerPen);
    DeleteObject(inner);

    HBRUSH titleBrush = CreateSolidBrush(RGB(12, 55, 80));
    RECT titleBackground = { 4, 4, client.right - 4, 32 };
    FillRect(dc, &titleBackground, titleBrush);
    DeleteObject(titleBrush);
    RECT title = { 8, 4, client.right - 8, 32 };
    DrawCombatPanelTextShadowed(dc, L"战力提升攻略", title, 14,
        FW_BOLD, RGB(229, 242, 247),
        DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    wchar_t summary[96] = {};
    const bool baseValid =
        (state.validFlags & DNF90_COMBAT_PANEL_BASE_SCORE_VALID) != 0;
    unsigned int equipmentBonusHundredths = 0;
    const bool equipmentBonusValid =
        TryComputeCombatPowerEquipmentBonusHundredths(
            state, &equipmentBonusHundredths);
    if (baseValid && equipmentBonusValid) {
        swprintf_s(summary, L"当前：%u（%s）    装备：%u.%02u%%",
            state.totalScore, CombatPowerRankName(state.totalScore),
            equipmentBonusHundredths / 100,
            equipmentBonusHundredths % 100);
    } else if (baseValid) {
        swprintf_s(summary, L"当前：%u（%s）",
            state.totalScore, CombatPowerRankName(state.totalScore));
    } else {
        wcscpy_s(summary, L"正在读取当前角色战力...");
    }
    RECT summaryRect = { 12, 38, client.right - 12, 64 };
    DrawCombatPanelTextShadowed(dc, summary, summaryRect, 11,
        FW_BOLD, RGB(255, 219, 105),
        DT_CENTER | DT_VCENTER | DT_SINGLELINE);

    HPEN separator = CreatePen(PS_SOLID, 1, RGB(38, 85, 110));
    oldPen = SelectObject(dc, separator);
    MoveToEx(dc, 10, 67, nullptr);
    LineTo(dc, client.right - 10, 67);
    SelectObject(dc, oldPen);
    DeleteObject(separator);

    const wchar_t* guideLines[] = {
        L"1. 优先补齐全部装备、称号、时装与光环",
        L"2. 宠物和所有宠物装备都会计入评分",
        L"3. 强化、增幅、套装和高品级装备可提分",
        L"4. 白字可叠加；普通黄字、爆伤只取最高",
        L"5. 黄追、爆追可叠加；所有攻击归入三攻",
    };
    for (size_t index = 0;
         index < sizeof(guideLines) / sizeof(guideLines[0]); ++index) {
        RECT line = { 15, 73 + static_cast<LONG>(index) * 28,
            client.right - 12, 99 + static_cast<LONG>(index) * 28 };
        DrawCombatPanelTextShadowed(dc, guideLines[index], line, 11,
            FW_NORMAL, RGB(205, 226, 236),
            DT_LEFT | DT_VCENTER | DT_SINGLELINE);
    }

    RECT footer = { 10, client.bottom - 27,
        client.right - 10, client.bottom - 7 };
    DrawCombatPanelTextShadowed(dc, L"再次点击“战力提升”关闭", footer, 10,
        FW_NORMAL, RGB(105, 172, 202),
        DT_CENTER | DT_VCENTER | DT_SINGLELINE);
}

DNF90CombatPanelState CopyCombatPanelState()
{
    DNF90CombatPanelState state = {};
    AcquireSRWLockShared(&g_combatPanelStateLock);
    state = g_combatPanelState;
    ReleaseSRWLockShared(&g_combatPanelStateLock);
    return state;
}

struct CombatPanelViewport {
    int clientWidth;
    int clientHeight;
    int renderWidth;
    int renderHeight;
};

bool TryGetCombatPanelViewport(HWND parent, CombatPanelViewport* viewport)
{
    if (!parent || !viewport) return false;
    RECT client = {};
    if (!GetClientRect(parent, &client)) return false;
    const int clientWidth = client.right - client.left;
    const int clientHeight = client.bottom - client.top;
    if (clientWidth <= 0 || clientHeight <= 0) return false;

    int renderWidth = clientWidth;
    int renderHeight = clientHeight;
    if (g_dnfBase) {
        __try {
            const int candidateWidth = *reinterpret_cast<const int*>(
                g_dnfBase + kNativeRenderCanvasSizeRva);
            const int candidateHeight = *reinterpret_cast<const int*>(
                g_dnfBase + kNativeRenderCanvasSizeRva + sizeof(int));
            // Reject transient or corrupt canvas state during device reset.
            // The ratio bound still permits every supported shrink/stretch
            // transition while preventing an invalid global from launching a
            // popup far outside the owning game window.
            if (candidateWidth >= 320 && candidateHeight >= 240 &&
                candidateWidth <= 16384 && candidateHeight <= 16384 &&
                static_cast<long long>(clientWidth) * 8 >= candidateWidth &&
                static_cast<long long>(candidateWidth) * 8 >= clientWidth &&
                static_cast<long long>(clientHeight) * 8 >= candidateHeight &&
                static_cast<long long>(candidateHeight) * 8 >= clientHeight) {
                renderWidth = candidateWidth;
                renderHeight = candidateHeight;
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            renderWidth = clientWidth;
            renderHeight = clientHeight;
        }
    }

    viewport->clientWidth = clientWidth;
    viewport->clientHeight = clientHeight;
    viewport->renderWidth = renderWidth;
    viewport->renderHeight = renderHeight;
    return true;
}

int ScaleCombatPanelX(int value, const CombatPanelViewport& viewport)
{
    return MulDiv(value, viewport.clientWidth, viewport.renderWidth);
}

int ScaleCombatPanelY(int value, const CombatPanelViewport& viewport)
{
    return MulDiv(value, viewport.clientHeight, viewport.renderHeight);
}

bool ConfigureCombatPanelLogicalPaint(
    HDC dc, HWND window, int logicalWidth, int logicalHeight)
{
    if (!dc || !window || logicalWidth <= 0 || logicalHeight <= 0) {
        return false;
    }
    RECT client = {};
    if (!GetClientRect(window, &client)) return false;
    const int width = client.right - client.left;
    const int height = client.bottom - client.top;
    if (width <= 0 || height <= 0) return false;
    if (!SetMapMode(dc, MM_ANISOTROPIC)) return false;
    if (!SetWindowExtEx(dc, logicalWidth, logicalHeight, nullptr) ||
        !SetViewportExtEx(dc, width, height, nullptr)) {
        return false;
    }
    SetStretchBltMode(dc, HALFTONE);
    SetBrushOrgEx(dc, 0, 0, nullptr);
    return true;
}

bool MapCombatPanelPointToLogical(HWND window, POINT* point)
{
    if (!window || !point) return false;
    RECT client = {};
    if (!GetClientRect(window, &client)) return false;
    const int width = client.right - client.left;
    const int height = client.bottom - client.top;
    if (width <= 0 || height <= 0) return false;
    point->x = MulDiv(point->x, kCombatPanelWidth, width);
    point->y = MulDiv(point->y, kCombatPanelHeight, height);
    return true;
}

bool TryGetClientScreenRect(
    HWND parent, const CombatPanelViewport& viewport, RECT* output)
{
    if (!parent || !output) return false;
    POINT origin = { 0, 0 };
    if (!ClientToScreen(parent, &origin)) return false;
    output->left = origin.x;
    output->top = origin.y;
    output->right = origin.x + viewport.clientWidth;
    output->bottom = origin.y + viewport.clientHeight;
    return true;
}

void SetCombatPanelGuideVisible(HWND parent, bool visible);
void SetCombatPanelRankTooltipVisible(HWND parent, bool visible);
DNF90DebugPanelState CopyDebugPanelState();
void DrawDebugPanel(HDC dc, const DNF90DebugPanelState& state);

bool IsCombatPanelUpgradeButtonPoint(POINT point)
{
    RECT button = {
        kCombatPanelUpgradeButtonLeft,
        kCombatPanelUpgradeButtonTop,
        kCombatPanelUpgradeButtonRight,
        kCombatPanelUpgradeButtonBottom,
    };
    return PtInRect(&button, point) != FALSE;
}

bool IsCombatPanelCloseButtonPoint(POINT point)
{
    RECT button = {
        kCombatPanelCloseButtonLeft,
        kCombatPanelCloseButtonTop,
        kCombatPanelCloseButtonRight,
        kCombatPanelCloseButtonBottom,
    };
    return PtInRect(&button, point) != FALSE;
}

void HideCombatPanelWindows(HWND parent)
{
    if (g_combatPanelOverlayWindow) {
        ShowWindow(g_combatPanelOverlayWindow, SW_HIDE);
    }
    SetCombatPanelRankTooltipVisible(parent, false);
    SetCombatPanelGuideVisible(parent, false);
}

void SuppressCombatPanelForCurrentCharacter(HWND parent)
{
    const LONG characterID = InterlockedCompareExchange(
        &g_combatPanelCurrentCharacterID, 0, 0);
    if (characterID <= 0) return;
    InterlockedExchange(&g_combatPanelSuppressedCharacterID, characterID);
    HideCombatPanelWindows(parent);
    g_combatPanelPersonalInfoOpen = false;
    LogLine("combat-panel closed character_id=%ld restore=character-switch",
        characterID);
}

LRESULT CALLBACK CombatPanelOverlayWindowProc(
    HWND window, UINT message, WPARAM wParam, LPARAM lParam)
{
    switch (message) {
    case WM_ERASEBKGND:
        return 1;
    case WM_NCHITTEST: {
        if (window == g_combatPanelOverlayWindow) {
            POINT point = {
                static_cast<short>(LOWORD(lParam)),
                static_cast<short>(HIWORD(lParam)),
            };
            if (ScreenToClient(window, &point) &&
                MapCombatPanelPointToLogical(window, &point) &&
                (IsCombatPanelUpgradeButtonPoint(point) ||
                 IsCombatPanelCloseButtonPoint(point))) {
                return HTCLIENT;
            }
        }
        return HTTRANSPARENT;
    }
    case WM_MOUSEACTIVATE:
        return MA_NOACTIVATE;
    case WM_SETCURSOR:
        if (window == g_combatPanelOverlayWindow) {
            POINT point = {};
            if (GetCursorPos(&point) && ScreenToClient(window, &point) &&
                MapCombatPanelPointToLogical(window, &point) &&
                (IsCombatPanelUpgradeButtonPoint(point) ||
                 IsCombatPanelCloseButtonPoint(point))) {
                SetCursor(LoadCursorW(nullptr, IDC_HAND));
                return TRUE;
            }
        }
        break;
    case WM_LBUTTONUP:
        if (window == g_combatPanelOverlayWindow) {
            POINT point = {
                static_cast<short>(LOWORD(lParam)),
                static_cast<short>(HIWORD(lParam)),
            };
            if (!MapCombatPanelPointToLogical(window, &point)) break;
            if (IsCombatPanelCloseButtonPoint(point)) {
                SuppressCombatPanelForCurrentCharacter(g_dnfWindow);
                return 0;
            }
            if (IsCombatPanelUpgradeButtonPoint(point)) {
                SetCombatPanelGuideVisible(
                    g_dnfWindow, !g_combatPanelGuideVisible);
                return 0;
            }
        }
        break;
    case WM_PAINT: {
        PAINTSTRUCT paint = {};
        HDC dc = BeginPaint(window, &paint);
        RECT client = {};
        GetClientRect(window, &client);
        DNF90CombatPanelState state = CopyCombatPanelState();

        int logicalWidth = kCombatPanelWidth;
        int logicalHeight = kCombatPanelHeight;
        if (window == g_debugPanelOverlayWindow) {
            logicalWidth = kDebugPanelWidth;
            logicalHeight = kDebugPanelHeight;
        } else if (window == g_combatPanelRankTooltipWindow) {
            logicalWidth = kCombatPanelRankTooltipWidth;
            logicalHeight = kCombatPanelRankTooltipHeight;
        } else if (window == g_combatPanelGuideWindow) {
            logicalWidth = kCombatPanelGuideWidth;
            logicalHeight = kCombatPanelGuideHeight;
        }
        const int savedDC = SaveDC(dc);
        ConfigureCombatPanelLogicalPaint(
            dc, window, logicalWidth, logicalHeight);

        if (window == g_debugPanelOverlayWindow) {
            DrawDebugPanel(dc, CopyDebugPanelState());
        } else if (window == g_combatPanelRankTooltipWindow) {
            DrawCombatPanelRankTooltip(dc, state);
        } else if (window == g_combatPanelGuideWindow) {
            DrawCombatPanelGuide(dc, state);
        } else {
            DrawCombatPanelMain(dc, state);
        }
        if (savedDC != 0) RestoreDC(dc, savedDC);
        EndPaint(window, &paint);
        return 0;

#if 0  // Preserved only as the V1 fallback layout reference.

        HBRUSH background = CreateSolidBrush(RGB(7, 18, 29));
        FillRect(dc, &client, background);
        DeleteObject(background);

        HBRUSH inner = CreateSolidBrush(RGB(11, 32, 49));
        HPEN outerPen = CreatePen(PS_SOLID, 2, RGB(48, 154, 204));
        HGDIOBJ oldBrush = SelectObject(dc, inner);
        HGDIOBJ oldPen = SelectObject(dc, outerPen);
        RoundRect(dc, 1, 1, client.right - 1, client.bottom - 1, 8, 8);
        SelectObject(dc, oldPen);
        SelectObject(dc, oldBrush);
        DeleteObject(outerPen);
        DeleteObject(inner);

        HBRUSH titleBrush = CreateSolidBrush(RGB(13, 48, 72));
        RECT titleBackground = { 3, 3, client.right - 3, 29 };
        FillRect(dc, &titleBackground, titleBrush);
        DeleteObject(titleBrush);
        RECT title = { 7, 4, client.right - 7, 28 };
        DrawCombatPanelText(dc, L"我的战斗力", title, 14, FW_BOLD,
            RGB(224, 245, 255), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        // The first implementation deliberately uses an owned GDI emblem.
        // It is independent from PVF resources and therefore cannot break the
        // native personal panel when a custom texture is absent.
        HBRUSH medalOuter = CreateSolidBrush(RGB(181, 119, 46));
        HPEN medalPen = CreatePen(PS_SOLID, 2, RGB(248, 207, 112));
        oldBrush = SelectObject(dc, medalOuter);
        oldPen = SelectObject(dc, medalPen);
        Ellipse(dc, 34, 36, 98, 100);
        SelectObject(dc, oldPen);
        SelectObject(dc, oldBrush);
        DeleteObject(medalPen);
        DeleteObject(medalOuter);
        HBRUSH medalInner = CreateSolidBrush(RGB(41, 126, 183));
        oldBrush = SelectObject(dc, medalInner);
        Ellipse(dc, 43, 45, 89, 91);
        SelectObject(dc, oldBrush);
        DeleteObject(medalInner);
        RECT medalText = { 43, 45, 89, 91 };
        DrawCombatPanelText(dc, L"战", medalText, 24, FW_BOLD,
            RGB(238, 251, 255), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        const bool baseValid =
            (state.validFlags & DNF90_COMBAT_PANEL_BASE_SCORE_VALID) != 0;
        wchar_t line[64] = {};
        if (baseValid) {
            swprintf_s(line, L"%u", state.totalScore);
        } else {
            wcscpy_s(line, L"读取中...");
        }
        RECT total = { 5, 103, client.right - 5, 132 };
        DrawCombatPanelText(dc, line, total, 24, FW_BOLD,
            RGB(255, 224, 116), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        const wchar_t* rank = baseValid
            ? CombatPowerRankName(state.totalScore)
            : L"等待角色属性";
        RECT rankRect = { 5, 132, client.right - 5, 153 };
        DrawCombatPanelText(dc, rank, rankRect, 13, FW_NORMAL,
            RGB(178, 216, 236), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        HPEN separator = CreatePen(PS_SOLID, 1, RGB(30, 84, 116));
        oldPen = SelectObject(dc, separator);
        MoveToEx(dc, 7, 158, nullptr);
        LineTo(dc, client.right - 7, 158);
        SelectObject(dc, oldPen);
        DeleteObject(separator);

        RECT baseLabel = { 8, 164, client.right - 8, 184 };
        DrawCombatPanelText(dc, L"基础属性分", baseLabel, 13, FW_BOLD,
            RGB(166, 222, 246), DT_LEFT | DT_VCENTER | DT_SINGLELINE);
        if (baseValid) {
            swprintf_s(line, L"%u", state.baseAttributeScore);
        } else {
            wcscpy_s(line, L"--");
        }
        RECT baseScore = { 8, 184, client.right - 8, 207 };
        DrawCombatPanelText(dc, line, baseScore, 17, FW_BOLD,
            RGB(112, 221, 255), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        RECT equipmentLabel = { 8, 211, client.right - 8, 231 };
        DrawCombatPanelText(dc, L"装备加成", equipmentLabel, 13, FW_BOLD,
            RGB(166, 222, 246), DT_LEFT | DT_VCENTER | DT_SINGLELINE);
        if ((state.validFlags &
                DNF90_COMBAT_PANEL_EQUIPMENT_SCORE_VALID) != 0) {
            swprintf_s(line, L"%u", state.equipmentScore);
        } else {
            wcscpy_s(line, L"待接入");
        }
        RECT equipmentScore = { 8, 231, client.right - 8, 253 };
        DrawCombatPanelText(dc, line, equipmentScore, 14, FW_BOLD,
            RGB(244, 190, 93), DT_CENTER | DT_VCENTER | DT_SINGLELINE);

        if (baseValid) {
            swprintf_s(line, L"力量  %d", state.strength);
            RECT statLine = { 10, 260, client.right - 8, 279 };
            DrawCombatPanelText(dc, line, statLine, 12, FW_NORMAL,
                RGB(202, 226, 238), DT_LEFT | DT_VCENTER | DT_SINGLELINE);
            swprintf_s(line, L"智力  %d", state.intelligence);
            statLine.top += 20; statLine.bottom += 20;
            DrawCombatPanelText(dc, line, statLine, 12, FW_NORMAL,
                RGB(202, 226, 238), DT_LEFT | DT_VCENTER | DT_SINGLELINE);
            swprintf_s(line, L"体力  %d", state.vitality);
            statLine.top += 20; statLine.bottom += 20;
            DrawCombatPanelText(dc, line, statLine, 12, FW_NORMAL,
                RGB(202, 226, 238), DT_LEFT | DT_VCENTER | DT_SINGLELINE);
            swprintf_s(line, L"精神  %d", state.spirit);
            statLine.top += 20; statLine.bottom += 20;
            DrawCombatPanelText(dc, line, statLine, 12, FW_NORMAL,
                RGB(202, 226, 238), DT_LEFT | DT_VCENTER | DT_SINGLELINE);
        }

        swprintf_s(line, L"自定义公式 V%u", state.formulaVersion);
        RECT formula = { 6, client.bottom - 24,
            client.right - 6, client.bottom - 6 };
        DrawCombatPanelText(dc, line, formula, 10, FW_NORMAL,
            RGB(91, 145, 170), DT_CENTER | DT_VCENTER | DT_SINGLELINE);
        EndPaint(window, &paint);
        return 0;
#endif
    }
    case WM_NCDESTROY:
        if (g_combatPanelOverlayWindow == window) {
            g_combatPanelOverlayWindow = nullptr;
        }
        if (g_combatPanelRankTooltipWindow == window) {
            g_combatPanelRankTooltipWindow = nullptr;
            g_combatPanelRankTooltipVisible = false;
        }
        if (g_combatPanelGuideWindow == window) {
            g_combatPanelGuideWindow = nullptr;
            g_combatPanelGuideVisible = false;
        }
        if (g_debugPanelOverlayWindow == window) {
            g_debugPanelOverlayWindow = nullptr;
        }
        break;
    default:
        break;
    }
    return DefWindowProcW(window, message, wParam, lParam);
}

bool EnsureCombatPanelOverlayWindow(HWND parent)
{
    if (g_combatPanelOverlayWindow &&
        IsWindow(g_combatPanelOverlayWindow) &&
        g_combatPanelRankTooltipWindow &&
        IsWindow(g_combatPanelRankTooltipWindow) &&
        g_combatPanelGuideWindow &&
        IsWindow(g_combatPanelGuideWindow)) {
        return true;
    }

    HINSTANCE module = reinterpret_cast<HINSTANCE>(
        GetModuleHandleW(L"90CN.dll"));
    WNDCLASSEXW windowClass = {};
    windowClass.cbSize = sizeof(windowClass);
    windowClass.style = CS_HREDRAW | CS_VREDRAW;
    windowClass.lpfnWndProc = &CombatPanelOverlayWindowProc;
    windowClass.hInstance = module;
    windowClass.lpszClassName = kCombatPanelOverlayClassName;
    if (!RegisterClassExW(&windowClass) &&
        GetLastError() != ERROR_CLASS_ALREADY_EXISTS) {
        LogLine("combat-panel overlay class registration failed error=%lu",
            GetLastError());
        return false;
    }

    const DWORD interactiveStyle = WS_EX_LAYERED |
        WS_EX_NOACTIVATE | WS_EX_TOOLWINDOW;
    const DWORD passiveStyle = interactiveStyle | WS_EX_TRANSPARENT;
    if (!g_combatPanelOverlayWindow ||
        !IsWindow(g_combatPanelOverlayWindow)) {
        HWND overlay = CreateWindowExW(
            interactiveStyle, kCombatPanelOverlayClassName, L"", WS_POPUP,
            0, 0, 0, 0, parent, nullptr, module, nullptr);
        if (!overlay) {
            LogLine("combat-panel overlay creation failed error=%lu",
                GetLastError());
            return false;
        }
        if (!SetLayeredWindowAttributes(overlay, 0, 250, LWA_ALPHA)) {
            LogLine("combat-panel overlay alpha setup failed error=%lu",
                GetLastError());
            DestroyWindow(overlay);
            return false;
        }
        g_combatPanelOverlayWindow = overlay;
    }
    if (!g_combatPanelRankTooltipWindow ||
        !IsWindow(g_combatPanelRankTooltipWindow)) {
        HWND tooltip = CreateWindowExW(
            passiveStyle, kCombatPanelOverlayClassName, L"", WS_POPUP,
            0, 0, 0, 0, parent, nullptr, module, nullptr);
        if (!tooltip) {
            LogLine("combat-panel rank tooltip creation failed error=%lu",
                GetLastError());
            return false;
        }
        if (!SetLayeredWindowAttributes(tooltip, 0, 252, LWA_ALPHA)) {
            LogLine("combat-panel rank tooltip alpha setup failed error=%lu",
                GetLastError());
            DestroyWindow(tooltip);
            return false;
        }
        g_combatPanelRankTooltipWindow = tooltip;
    }
    if (!g_combatPanelGuideWindow ||
        !IsWindow(g_combatPanelGuideWindow)) {
        HWND guide = CreateWindowExW(
            passiveStyle, kCombatPanelOverlayClassName, L"", WS_POPUP,
            0, 0, 0, 0, parent, nullptr, module, nullptr);
        if (!guide) {
            LogLine("combat-panel guide creation failed error=%lu",
                GetLastError());
            return false;
        }
        if (!SetLayeredWindowAttributes(guide, 0, 252, LWA_ALPHA)) {
            LogLine("combat-panel guide alpha setup failed error=%lu",
                GetLastError());
            DestroyWindow(guide);
            return false;
        }
        g_combatPanelGuideWindow = guide;
    }
    return true;
}

bool IsNativePersonalInfoPanelOpen()
{
    if (!g_dnfBase) return false;
    __try {
        void* sceneRoot = *reinterpret_cast<void**>(
            g_dnfBase + kSceneRootPointerRva);
        SceneUiIsOpenFn isOpen = reinterpret_cast<SceneUiIsOpenFn>(
            g_dnfBase + kSceneUiIsOpenRva);
        // Opening equipment while personal information is still registered
        // hides the latter natively. Suppress the sidecar for that overlap so
        // it never floats above the inventory window.
        return sceneRoot && isOpen &&
            isOpen(sceneRoot, kPersonalInfoUiOwner) &&
            !isOpen(sceneRoot, kAvatarPanelUiOwner);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("combat-panel native personal visibility exception code=0x%08X",
            GetExceptionCode());
        return false;
    }
}

bool TryGetNativePersonalPanelRect(
    int parentWidth, int parentHeight,
    int* panelX, int* panelY, int* panelWidth, int* panelHeight)
{
    if (!g_dnfBase || !panelX || !panelY ||
        !panelWidth || !panelHeight) {
        return false;
    }

    __try {
        uintptr_t sceneRoot = *reinterpret_cast<uintptr_t*>(
            g_dnfBase + kSceneRootPointerRva);
        if (!sceneRoot) return false;
        uintptr_t vector = sceneRoot + kSceneUiOwnerVectorOffset +
            kPersonalInfoUiOwner * kSceneUiOwnerVectorStride;
        uintptr_t begin = *reinterpret_cast<uintptr_t*>(
            vector + kSceneUiOwnerVectorBeginOffset);
        uintptr_t end = *reinterpret_cast<uintptr_t*>(
            vector + kSceneUiOwnerVectorEndOffset);
        if (!begin || end <= begin || end - begin > 64 ||
            ((end - begin) % sizeof(uintptr_t)) != 0) {
            return false;
        }
        uintptr_t panel = *reinterpret_cast<uintptr_t*>(begin);
        uintptr_t widget = panel
            ? *reinterpret_cast<uintptr_t*>(
                panel + kPersonalPanelRootWidgetOffset)
            : 0;
        if (!widget) return false;

        int x = *reinterpret_cast<int*>(
            widget + kPersonalPanelWidgetXOffset);
        int y = *reinterpret_cast<int*>(
            widget + kPersonalPanelWidgetYOffset);
        int width = *reinterpret_cast<int*>(
            widget + kPersonalPanelWidgetWidthOffset);
        int height = *reinterpret_cast<int*>(
            widget + kPersonalPanelWidgetHeightOffset);
        if (x < -64 || y < -64 || width < 160 || height < 200 ||
            width > 640 || height > 760 ||
            x > parentWidth || y > parentHeight) {
            return false;
        }
        *panelX = x;
        *panelY = y;
        *panelWidth = width;
        *panelHeight = height;
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

bool PositionCombatPanelOverlay(HWND parent)
{
    if (!g_combatPanelOverlayWindow) return false;
    CombatPanelViewport viewport = {};
    if (!TryGetCombatPanelViewport(parent, &viewport)) return false;
    const int parentWidth = viewport.clientWidth;
    const int parentHeight = viewport.clientHeight;
    int panelWidth = ScaleCombatPanelX(kCombatPanelWidth, viewport);
    int panelHeight = ScaleCombatPanelY(kCombatPanelHeight, viewport);
    int marginX = ScaleCombatPanelX(4, viewport);
    int marginY = ScaleCombatPanelY(4, viewport);
    if (panelWidth < 1) panelWidth = 1;
    if (panelHeight < 1) panelHeight = 1;
    if (marginX < 1) marginX = 1;
    if (marginY < 1) marginY = 1;

    // Log only a real per-process viewport transition.  This is deliberately
    // not keyed by the selected resolution label: two concurrently running
    // clients can own equal 1600x900 HWNDs while one still renders a
    // 1067x600 canvas and therefore needs a 1.5x sidecar.
    static int lastClientWidth = 0;
    static int lastClientHeight = 0;
    static int lastRenderWidth = 0;
    static int lastRenderHeight = 0;
    if (lastClientWidth != viewport.clientWidth ||
        lastClientHeight != viewport.clientHeight ||
        lastRenderWidth != viewport.renderWidth ||
        lastRenderHeight != viewport.renderHeight) {
        LogLine("combat-panel viewport client=%dx%d render=%dx%d "
                "scale_x_milli=%d scale_y_milli=%d",
            viewport.clientWidth, viewport.clientHeight,
            viewport.renderWidth, viewport.renderHeight,
            MulDiv(1000, viewport.clientWidth, viewport.renderWidth),
            MulDiv(1000, viewport.clientHeight, viewport.renderHeight));
        lastClientWidth = viewport.clientWidth;
        lastClientHeight = viewport.clientHeight;
        lastRenderWidth = viewport.renderWidth;
        lastRenderHeight = viewport.renderHeight;
    }

    // Use the native D3D widget rectangle so the sidecar follows a dragged
    // personal-information panel. Retain the old bounded center fallback only
    // if the current object layout fails its strict sanity checks.
    int x = parentWidth / 2 + 40;
    int y = parentHeight / 2 - 110;
    int nativeX = 0;
    int nativeY = 0;
    int nativeWidth = 0;
    int nativeHeight = 0;
    if (TryGetNativePersonalPanelRect(
            viewport.renderWidth, viewport.renderHeight,
            &nativeX, &nativeY, &nativeWidth, &nativeHeight)) {
        x = ScaleCombatPanelX(
            nativeX + nativeWidth + kPersonalPanelFrameRight, viewport);
        y = ScaleCombatPanelY(
            nativeY - kPersonalPanelFrameTop, viewport);
        if (x + panelWidth > parentWidth - marginX) {
            x = ScaleCombatPanelX(nativeX, viewport) - panelWidth;
        }
    }
    if (x + panelWidth > parentWidth - marginX) {
        x = parentWidth - panelWidth - marginX;
    }
    if (x < marginX) x = marginX;
    if (y + panelHeight > parentHeight - marginY) {
        y = parentHeight - panelHeight - marginY;
    }
    if (y < marginY) y = marginY;

    POINT screenPosition = { x, y };
    if (!ClientToScreen(parent, &screenPosition)) return false;
    // hWndInsertAfter=parent places this popup behind the DirectX owner. Keep
    // it at the top of the active process's non-topmost band instead.
    return SetWindowPos(g_combatPanelOverlayWindow, HWND_TOP,
        screenPosition.x, screenPosition.y,
        panelWidth, panelHeight,
        SWP_NOACTIVATE | SWP_SHOWWINDOW) != FALSE;
}

void SetCombatPanelGuideVisible(HWND parent, bool visible)
{
    if (!visible || !parent || !g_combatPanelOverlayWindow ||
        !g_combatPanelGuideWindow) {
        if (g_combatPanelGuideWindow) {
            ShowWindow(g_combatPanelGuideWindow, SW_HIDE);
        }
        g_combatPanelGuideVisible = false;
        return;
    }

    CombatPanelViewport viewport = {};
    RECT panel = {};
    RECT parentRect = {};
    if (!TryGetCombatPanelViewport(parent, &viewport) ||
        !GetWindowRect(g_combatPanelOverlayWindow, &panel) ||
        !TryGetClientScreenRect(parent, viewport, &parentRect)) {
        return;
    }
    int marginX = ScaleCombatPanelX(4, viewport);
    int marginY = ScaleCombatPanelY(4, viewport);
    int guideWidth = ScaleCombatPanelX(kCombatPanelGuideWidth, viewport);
    int guideHeight = ScaleCombatPanelY(kCombatPanelGuideHeight, viewport);
    if (marginX < 1) marginX = 1;
    if (marginY < 1) marginY = 1;
    if (guideWidth < 1) guideWidth = 1;
    if (guideHeight < 1) guideHeight = 1;
    int x = panel.right + marginX;
    int y = panel.top +
        ScaleCombatPanelY(kCombatPanelUpgradeButtonTop - 4, viewport);
    if (x + guideWidth > parentRect.right - marginX) {
        x = panel.left - guideWidth - marginX;
    }
    if (x < parentRect.left + marginX) x = parentRect.left + marginX;
    if (y + guideHeight > parentRect.bottom - marginY) {
        y = parentRect.bottom - guideHeight - marginY;
    }
    if (y < parentRect.top + marginY) y = parentRect.top + marginY;

    SetCombatPanelRankTooltipVisible(parent, false);
    const bool wasVisible =
        IsWindowVisible(g_combatPanelGuideWindow) != FALSE;
    SetWindowPos(g_combatPanelGuideWindow, HWND_TOP,
        x, y, guideWidth, guideHeight,
        SWP_NOACTIVATE | SWP_SHOWWINDOW);
    if (!wasVisible) {
        InvalidateRect(g_combatPanelGuideWindow, nullptr, FALSE);
        UpdateWindow(g_combatPanelGuideWindow);
    }
    g_combatPanelGuideVisible = true;
}

void SetCombatPanelRankTooltipVisible(HWND parent, bool visible)
{
    if (!visible || !parent || !g_combatPanelOverlayWindow ||
        !g_combatPanelRankTooltipWindow) {
        if (g_combatPanelRankTooltipWindow) {
            ShowWindow(g_combatPanelRankTooltipWindow, SW_HIDE);
        }
        g_combatPanelRankTooltipVisible = false;
        return;
    }

    CombatPanelViewport viewport = {};
    RECT panel = {};
    RECT parentRect = {};
    if (!TryGetCombatPanelViewport(parent, &viewport) ||
        !GetWindowRect(g_combatPanelOverlayWindow, &panel) ||
        !TryGetClientScreenRect(parent, viewport, &parentRect)) {
        return;
    }
    int marginX = ScaleCombatPanelX(4, viewport);
    int marginY = ScaleCombatPanelY(4, viewport);
    int tooltipWidth = ScaleCombatPanelX(
        kCombatPanelRankTooltipWidth, viewport);
    int tooltipHeight = ScaleCombatPanelY(
        kCombatPanelRankTooltipHeight, viewport);
    if (marginX < 1) marginX = 1;
    if (marginY < 1) marginY = 1;
    if (tooltipWidth < 1) tooltipWidth = 1;
    if (tooltipHeight < 1) tooltipHeight = 1;
    int x = panel.right + marginX;
    int y = panel.top + ScaleCombatPanelY(34, viewport);
    if (x + tooltipWidth > parentRect.right - marginX) {
        x = panel.left - tooltipWidth - marginX;
    }
    if (x < parentRect.left + marginX) x = parentRect.left + marginX;
    if (y + tooltipHeight > parentRect.bottom - marginY) {
        y = parentRect.bottom - tooltipHeight - marginY;
    }
    if (y < parentRect.top + marginY) y = parentRect.top + marginY;

    const bool wasVisible =
        IsWindowVisible(g_combatPanelRankTooltipWindow) != FALSE;
    SetWindowPos(g_combatPanelRankTooltipWindow, HWND_TOP,
        x, y, tooltipWidth, tooltipHeight,
        SWP_NOACTIVATE | SWP_SHOWWINDOW);
    if (!wasVisible) {
        InvalidateRect(g_combatPanelRankTooltipWindow, nullptr, FALSE);
        UpdateWindow(g_combatPanelRankTooltipWindow);
    }
    g_combatPanelRankTooltipVisible = true;
}

void RefreshCombatPanelRankTooltip(HWND parent)
{
    if (g_combatPanelGuideVisible || !g_combatPanelOverlayWindow ||
        !IsWindowVisible(g_combatPanelOverlayWindow)) {
        SetCombatPanelRankTooltipVisible(parent, false);
        return;
    }
    POINT cursor = {};
    if (!GetCursorPos(&cursor) ||
        !ScreenToClient(g_combatPanelOverlayWindow, &cursor) ||
        !MapCombatPanelPointToLogical(
            g_combatPanelOverlayWindow, &cursor)) {
        SetCombatPanelRankTooltipVisible(parent, false);
        return;
    }
    RECT hover = {
        0,
        kCombatPanelRankHoverTop,
        kCombatPanelWidth,
        kCombatPanelRankHoverBottom,
    };
    SetCombatPanelRankTooltipVisible(parent,
        PtInRect(&hover, cursor) != FALSE);
}

void RefreshCombatPanelOverlay(HWND parent)
{
    if (!kCombatPowerEnabled) {
        return;
    }
    if (!parent || IsIconic(parent) || GetForegroundWindow() != parent) {
        if (g_combatPanelOverlayWindow) {
            ShowWindow(g_combatPanelOverlayWindow, SW_HIDE);
        }
        SetCombatPanelRankTooltipVisible(parent, false);
        SetCombatPanelGuideVisible(parent, false);
        return;
    }

    const LONG currentCharacterID = InterlockedCompareExchange(
        &g_combatPanelCurrentCharacterID, 0, 0);
    const LONG suppressedCharacterID = InterlockedCompareExchange(
        &g_combatPanelSuppressedCharacterID, 0, 0);
    if (currentCharacterID > 0 &&
        suppressedCharacterID == currentCharacterID) {
        HideCombatPanelWindows(parent);
        g_combatPanelPersonalInfoOpen = false;
        return;
    }

    const bool personalInfoOpen = IsNativePersonalInfoPanelOpen();
    DNF90CombatPanelState state = CopyCombatPanelState();
    const bool enabled =
        (state.validFlags & DNF90_COMBAT_PANEL_ENABLED) != 0;
    if (!personalInfoOpen || !enabled) {
        if (g_combatPanelOverlayWindow &&
            IsWindowVisible(g_combatPanelOverlayWindow)) {
            ShowWindow(g_combatPanelOverlayWindow, SW_HIDE);
        }
        SetCombatPanelRankTooltipVisible(parent, false);
        SetCombatPanelGuideVisible(parent, false);
        if (g_combatPanelPersonalInfoOpen != personalInfoOpen) {
            LogLine("combat-panel personal visibility open=%d enabled=%d",
                personalInfoOpen ? 1 : 0, enabled ? 1 : 0);
        }
        g_combatPanelPersonalInfoOpen = personalInfoOpen;
        return;
    }

    if (!EnsureCombatPanelOverlayWindow(parent)) return;
    const bool wasVisible =
        IsWindowVisible(g_combatPanelOverlayWindow) != FALSE;
    PositionCombatPanelOverlay(parent);
    if (g_combatPanelGuideVisible) {
        SetCombatPanelGuideVisible(parent, true);
    }
    RefreshCombatPanelRankTooltip(parent);
    if (!wasVisible) {
        InvalidateRect(g_combatPanelOverlayWindow, nullptr, FALSE);
    }
    if (!g_combatPanelPersonalInfoOpen) {
        LogLine("combat-panel shown revision=%u generation=%u score=%u "
            "base_valid=%d equipment_valid=%d",
            state.revision, state.sourceGeneration, state.totalScore,
            (state.validFlags &
                DNF90_COMBAT_PANEL_BASE_SCORE_VALID) ? 1 : 0,
            (state.validFlags &
                DNF90_COMBAT_PANEL_EQUIPMENT_SCORE_VALID) ? 1 : 0);
    }
    g_combatPanelPersonalInfoOpen = true;
}

DNF90DebugPanelState CopyDebugPanelState()
{
    DNF90DebugPanelState state = {};
    AcquireSRWLockShared(&g_debugPanelStateLock);
    state = g_debugPanelState;
    ReleaseSRWLockShared(&g_debugPanelStateLock);
    return state;
}

const wchar_t* DebugPanelEventName(unsigned int type)
{
    switch (type) {
    case 1: return L"UI 就绪";
    case 2: return L"进入城镇";
    case 3: return L"进入副本";
    case 4: return L"切换房间";
    case 5: return L"离开副本";
    default: return L"等待事件";
    }
}

const wchar_t* DebugPanelContextName(unsigned int context)
{
    switch (context) {
    case 1: return L"城镇";
    case 2: return L"副本";
    default: return L"未确认";
    }
}

void DrawDebugPanel(HDC dc, const DNF90DebugPanelState& state)
{
    RECT client = { 0, 0, kDebugPanelWidth, kDebugPanelHeight };
    HBRUSH background = CreateSolidBrush(RGB(12, 17, 22));
    FillRect(dc, &client, background);
    DeleteObject(background);

    HPEN borderPen = CreatePen(PS_SOLID, 1, RGB(57, 145, 118));
    HGDIOBJ oldPen = SelectObject(dc, borderPen);
    HGDIOBJ oldBrush = SelectObject(dc, GetStockObject(HOLLOW_BRUSH));
    Rectangle(dc, 0, 0, client.right, client.bottom);
    SelectObject(dc, oldBrush);
    SelectObject(dc, oldPen);
    DeleteObject(borderPen);

    HBRUSH headerBackground = CreateSolidBrush(RGB(22, 66, 57));
    RECT header = { 1, 1, client.right - 1, 27 };
    FillRect(dc, &header, headerBackground);
    DeleteObject(headerBackground);
    DrawCombatPanelText(dc, L"本地调试", { 10, 3, 118, 25 }, 14,
        FW_BOLD, RGB(226, 246, 238), DT_LEFT | DT_VCENTER | DT_SINGLELINE);
    DrawCombatPanelText(dc, L"只读", { 158, 4, client.right - 10, 24 }, 10,
        FW_NORMAL, RGB(161, 221, 197), DT_RIGHT | DT_VCENTER | DT_SINGLELINE);

    const bool eventValid =
        (state.validFlags & DNF90_DEBUG_PANEL_EVENT_VALID) != 0;
    const bool characterValid =
        (state.validFlags & DNF90_DEBUG_PANEL_CHARACTER_VALID) != 0;
    const bool dungeonValid =
        (state.validFlags & DNF90_DEBUG_PANEL_DUNGEON_VALID) != 0;
    const bool combatValid =
        (state.validFlags & DNF90_DEBUG_PANEL_COMBAT_SCORE_VALID) != 0;
    wchar_t line[96] = {};

    if (eventValid) {
        swprintf_s(line, L"事件 #%u  %s", state.latestEventSequence,
            DebugPanelEventName(state.latestEventType));
    } else {
        wcscpy_s(line, L"事件 等待客户端状态");
    }
    DrawCombatPanelText(dc, line, { 10, 35, client.right - 10, 53 }, 11,
        FW_NORMAL, RGB(192, 214, 223), DT_LEFT | DT_VCENTER | DT_SINGLELINE);

    swprintf_s(line, L"场景 %s", DebugPanelContextName(state.context));
    DrawCombatPanelText(dc, line, { 10, 56, client.right - 10, 74 }, 12,
        FW_BOLD, state.context == 2 ? RGB(255, 205, 110) : RGB(132, 211, 247),
        DT_LEFT | DT_VCENTER | DT_SINGLELINE);

    if (dungeonValid) {
        swprintf_s(line, L"副本 %u  图 %u  房 %u,%u", state.dungeonID,
            state.mapID, state.roomX, state.roomY);
    } else {
        wcscpy_s(line, L"副本 上下文未确认");
    }
    DrawCombatPanelText(dc, line, { 10, 78, client.right - 10, 96 }, 10,
        FW_NORMAL, RGB(168, 196, 207), DT_LEFT | DT_VCENTER | DT_SINGLELINE);

    if (dungeonValid) {
        swprintf_s(line, L"层 %u  已过房间 %u", state.roomLayerFlag,
            state.roomCount);
    } else {
        wcscpy_s(line, L"层 --  已过房间 --");
    }
    DrawCombatPanelText(dc, line, { 10, 98, client.right - 10, 116 }, 10,
        FW_NORMAL, RGB(132, 165, 180), DT_LEFT | DT_VCENTER | DT_SINGLELINE);

    if (characterValid) {
        swprintf_s(line, L"HP %u  MP %u", state.hpMax, state.mpMax);
        DrawCombatPanelText(dc, line, { 10, 121, client.right - 10, 139 }, 11,
            FW_NORMAL, RGB(224, 235, 241), DT_LEFT | DT_VCENTER | DT_SINGLELINE);
        swprintf_s(line, L"力 %d  智 %d", state.strength, state.intelligence);
        DrawCombatPanelText(dc, line, { 10, 140, 112, 158 }, 11,
            FW_NORMAL, RGB(224, 235, 241), DT_LEFT | DT_VCENTER | DT_SINGLELINE);
        swprintf_s(line, L"移 %u 攻 %u 施 %u", state.moveSpeed,
            state.attackSpeed, state.castSpeed);
        DrawCombatPanelText(dc, line, { 112, 140, client.right - 10, 158 }, 10,
            FW_NORMAL, RGB(180, 218, 235), DT_RIGHT | DT_VCENTER | DT_SINGLELINE);
    } else {
        DrawCombatPanelText(dc, L"角色属性 等待完整快照", { 10, 126,
            client.right - 10, 146 }, 11, FW_NORMAL, RGB(145, 164, 173),
            DT_LEFT | DT_VCENTER | DT_SINGLELINE);
    }

    swprintf_s(line, L"装备 %u  战斗力 %s", state.equippedItemCount,
        combatValid ? L"已同步" : L"等待");
    DrawCombatPanelText(dc, line, { 10, 166, client.right - 10, 184 }, 11,
        FW_NORMAL, RGB(202, 227, 219), DT_LEFT | DT_VCENTER | DT_SINGLELINE);
    if (combatValid) {
        swprintf_s(line, L"%u", state.combatScore);
        DrawCombatPanelText(dc, line, { 154, 164, client.right - 10, 185 }, 13,
            FW_BOLD, RGB(255, 216, 129), DT_RIGHT | DT_VCENTER | DT_SINGLELINE);
    }
    DrawCombatPanelText(dc, L"Lua -> 90CN UI 线程", { 10, 187,
        client.right - 10, 202 }, 9, FW_NORMAL, RGB(103, 150, 137),
        DT_CENTER | DT_VCENTER | DT_SINGLELINE);
}

LRESULT CALLBACK DebugPanelOverlayWindowProc(
    HWND window, UINT message, WPARAM /*wParam*/, LPARAM /*lParam*/)
{
    switch (message) {
    case WM_ERASEBKGND:
        return 1;
    case WM_NCHITTEST:
        return HTTRANSPARENT;
    case WM_MOUSEACTIVATE:
        return MA_NOACTIVATE;
    case WM_PAINT: {
        PAINTSTRUCT paint = {};
        HDC dc = BeginPaint(window, &paint);
        DrawDebugPanel(dc, CopyDebugPanelState());
        EndPaint(window, &paint);
        return 0;
    }
    case WM_NCDESTROY:
        if (g_debugPanelOverlayWindow == window) {
            g_debugPanelOverlayWindow = nullptr;
        }
        break;
    default:
        break;
    }
    return DefWindowProcW(window, message, 0, 0);
}

bool EnsureDebugPanelOverlayWindow(HWND parent)
{
    if (g_debugPanelOverlayWindow && IsWindow(g_debugPanelOverlayWindow)) {
        return true;
    }
    HINSTANCE module = reinterpret_cast<HINSTANCE>(
        GetModuleHandleW(L"90CN.dll"));
    WNDCLASSEXW windowClass = {};
    windowClass.cbSize = sizeof(windowClass);
    windowClass.style = CS_HREDRAW | CS_VREDRAW;
    // Reuse the already registered combat-panel class. Its window procedure
    // now dispatches the debug window by handle, avoiding a second class
    // registration path in the current client's UI process.
    windowClass.lpfnWndProc = &CombatPanelOverlayWindowProc;
    windowClass.hInstance = module;
    windowClass.lpszClassName = kCombatPanelOverlayClassName;
    if (!RegisterClassExW(&windowClass) &&
        GetLastError() != ERROR_CLASS_ALREADY_EXISTS) {
        LogLine("debug-panel overlay class registration failed error=%lu",
            GetLastError());
        return false;
    }

    HWND overlay = CreateWindowExW(
        WS_EX_LAYERED | WS_EX_TRANSPARENT | WS_EX_NOACTIVATE | WS_EX_TOOLWINDOW,
        kCombatPanelOverlayClassName, L"", WS_POPUP,
        0, 0, 0, 0, parent, nullptr, module, nullptr);
    if (!overlay) {
        LogLine("debug-panel overlay creation failed error=%lu", GetLastError());
        return false;
    }
    if (!SetLayeredWindowAttributes(overlay, 0, 238, LWA_ALPHA)) {
        LogLine("debug-panel overlay alpha setup failed error=%lu", GetLastError());
        DestroyWindow(overlay);
        return false;
    }
    g_debugPanelOverlayWindow = overlay;
    return true;
}

bool PositionDebugPanelOverlay(HWND parent)
{
    if (!parent || !g_debugPanelOverlayWindow) return false;
    RECT parentClient = {};
    if (!GetClientRect(parent, &parentClient)) return false;
    const int width = parentClient.right - parentClient.left;
    const int height = parentClient.bottom - parentClient.top;
    int x = 12;
    int y = 12;
    if (width < kDebugPanelWidth + 24) x = 4;
    if (height < kDebugPanelHeight + 24) y = 4;
    POINT screenPosition = { x, y };
    if (!ClientToScreen(parent, &screenPosition)) return false;
    return SetWindowPos(g_debugPanelOverlayWindow, HWND_TOP,
        screenPosition.x, screenPosition.y,
        kDebugPanelWidth, kDebugPanelHeight,
        SWP_NOACTIVATE | SWP_SHOWWINDOW) != FALSE;
}

void RefreshDebugPanelOverlay(HWND parent)
{
    if (!kDebugPanelEnabled) {
        return;
    }
    DNF90DebugPanelState state = CopyDebugPanelState();
    const bool enabled =
        (state.validFlags & DNF90_DEBUG_PANEL_ENABLED) != 0;
    if (!parent || !enabled || IsIconic(parent) || GetForegroundWindow() != parent) {
        if (g_debugPanelOverlayWindow) {
            ShowWindow(g_debugPanelOverlayWindow, SW_HIDE);
        }
        return;
    }
    if (!EnsureDebugPanelOverlayWindow(parent)) return;
    const bool wasVisible = IsWindowVisible(g_debugPanelOverlayWindow) != FALSE;
    PositionDebugPanelOverlay(parent);
    if (!wasVisible) {
        InvalidateRect(g_debugPanelOverlayWindow, nullptr, FALSE);
    }
}

bool QueueClientNoticeInternal(
    const wchar_t* text, unsigned int length, unsigned int style)
{
    if (!text || length == 0 || length > kLuaNoticeMaximumCharacters) {
        return RejectLuaClientNotice("invalid-length");
    }
    if (style != kClientNoticeStylePopup &&
        style != kClientNoticeStyleDungeonSelect) {
        return RejectLuaClientNotice("invalid-style");
    }
    HWND bridgeWindow = reinterpret_cast<HWND>(
        InterlockedCompareExchangePointer(
            &g_luaNoticeBridgeWindow, nullptr, nullptr));
    UINT bridgeMessage = static_cast<UINT>(
        InterlockedCompareExchange(&g_luaNoticeWindowMessage, 0, 0));
    if (!bridgeWindow || !bridgeMessage) {
        return RejectLuaClientNotice("main-thread-bridge-not-ready");
    }

    LuaClientNotice pending = {};
    pending.length = length;
    pending.style = style;
    __try {
        for (unsigned int index = 0; index < length; ++index) {
            wchar_t value = text[index];
            if (value == L'\0') {
                return RejectLuaClientNotice("embedded-nul");
            }
            pending.text[index] = value;
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return RejectLuaClientNotice("unreadable-text");
    }
    pending.text[length] = L'\0';

    AcquireSRWLockExclusive(&g_luaNoticeLock);
    if (g_luaNoticeCount >= kLuaNoticeQueueCapacity) {
        ReleaseSRWLockExclusive(&g_luaNoticeLock);
        return RejectLuaClientNotice("queue-full");
    }
    size_t tail = (g_luaNoticeHead + g_luaNoticeCount) %
        kLuaNoticeQueueCapacity;
    g_luaNoticeQueue[tail] = pending;
    ++g_luaNoticeCount;
    ReleaseSRWLockExclusive(&g_luaNoticeLock);

    LONG queued = InterlockedIncrement(&g_luaNoticeQueuedCount);
    if (!PostMessageW(bridgeWindow, bridgeMessage, 0, 0)) {
        LogLine("lua client notice PostMessageW failed queued=%ld error=%lu",
            queued, GetLastError());
        return false;
    }
    LogLine("client notice queued=%ld chars=%u style=%u caller_tid=%lu",
        queued, length, style, GetCurrentThreadId());
    return true;
}

bool QueueLuaClientNoticeInternal(const wchar_t* text, unsigned int length)
{
    return QueueClientNoticeInternal(
        text, length, kClientNoticeStylePopup);
}

bool QueueDungeonSelectFatiguePrompt(unsigned char reason)
{
    const bool party = reason == kSelectDungeonPartyFatiguePromptReason;
    if (!party && reason != kSelectDungeonFatiguePromptReason) {
        return RejectLuaClientNotice("invalid-dungeon-fatigue-reason");
    }
    const wchar_t* text = party
        ? kSelectDungeonPartyFatiguePromptText
        : kSelectDungeonFatiguePromptText;
    const unsigned int length = party
        ? static_cast<unsigned int>(
              _countof(kSelectDungeonPartyFatiguePromptText) - 1)
        : static_cast<unsigned int>(
              _countof(kSelectDungeonFatiguePromptText) - 1);
    const bool queued = QueueClientNoticeInternal(
        text, length, kClientNoticeStyleDungeonSelect);
    LogLine("dungeon-select fatigue prompt queued=%d reason=%u scope=%s chars=%u channel=native-selector-feed+native-chat",
        queued ? 1 : 0,
        static_cast<unsigned int>(reason),
        party ? "party" : "solo",
        length);
    return queued;
}

bool QueueLuaGameCommandInternal(
    const DNF90GameCommand* command, unsigned int commandSize)
{
    if (!command || commandSize != sizeof(DNF90GameCommand)) {
        return RejectLuaGameCommand("invalid-size");
    }

    DNF90GameCommand pending = {};
    __try {
        pending = *command;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return RejectLuaGameCommand("unreadable-command");
    }
    if (pending.size != sizeof(pending) || pending.flags != 0 ||
        pending.reserved != 0 || pending.objectKey > 0xFFFFu) {
        return RejectLuaGameCommand("invalid-fields");
    }
    switch (pending.type) {
    case DNF90_GAME_COMMAND_SELF_PLAY_MOTION:
        if (pending.objectKey != 0 || pending.value < 3055 ||
            pending.value > 3057 || !g_luaGameControlEntrypointsValid) {
            return RejectLuaGameCommand("invalid-self-motion");
        }
        break;
    case DNF90_GAME_COMMAND_APPLY_ACTOR_MOOD:
        if (pending.value < 0 || pending.value > 0x7FFF ||
            !g_luaGameControlEntrypointsValid) {
            return RejectLuaGameCommand("invalid-actor-mood");
        }
        break;
    case DNF90_GAME_COMMAND_REFRESH_ACTORS:
        if (pending.value != 0) {
            return RejectLuaGameCommand("invalid-actor-refresh");
        }
        break;
    case DNF90_GAME_COMMAND_REQUEST_BOSS_MUTATION:
        if (pending.objectKey == 0 || pending.value <= 0) {
            return RejectLuaGameCommand("invalid-boss-mutation-request");
        }
        break;
    default:
        return RejectLuaGameCommand("unsupported-type");
    }

    HWND bridgeWindow = reinterpret_cast<HWND>(
        InterlockedCompareExchangePointer(
            &g_luaNoticeBridgeWindow, nullptr, nullptr));
    UINT bridgeMessage = static_cast<UINT>(
        InterlockedCompareExchange(&g_luaGameCommandWindowMessage, 0, 0));
    if (!bridgeWindow || !bridgeMessage) {
        return RejectLuaGameCommand("main-thread-bridge-not-ready");
    }

    AcquireSRWLockExclusive(&g_luaGameCommandLock);
    if (g_luaGameCommandCount >= kLuaGameCommandQueueCapacity) {
        ReleaseSRWLockExclusive(&g_luaGameCommandLock);
        return RejectLuaGameCommand("queue-full");
    }
    size_t tail = (g_luaGameCommandHead + g_luaGameCommandCount) %
        kLuaGameCommandQueueCapacity;
    g_luaGameCommandQueue[tail] = pending;
    ++g_luaGameCommandCount;
    ReleaseSRWLockExclusive(&g_luaGameCommandLock);

    LONG sequence = InterlockedIncrement(&g_luaGameCommandSequence);
    if (!PostMessageW(bridgeWindow, bridgeMessage, 0, 0)) {
        LogLine("lua game command PostMessageW failed sequence=%ld type=%u error=%lu",
            sequence, pending.type, GetLastError());
        return false;
    }
    LogLine("lua game command queued sequence=%ld type=%u key=%u value=%d caller_tid=%lu",
        sequence, pending.type, pending.objectKey, pending.value,
        GetCurrentThreadId());
    return true;
}

bool DequeueLuaClientNotice(LuaClientNotice* output)
{
    if (!output) return false;
    AcquireSRWLockExclusive(&g_luaNoticeLock);
    if (g_luaNoticeCount == 0) {
        ReleaseSRWLockExclusive(&g_luaNoticeLock);
        return false;
    }
    *output = g_luaNoticeQueue[g_luaNoticeHead];
    g_luaNoticeQueue[g_luaNoticeHead] = {};
    g_luaNoticeHead = (g_luaNoticeHead + 1) % kLuaNoticeQueueCapacity;
    --g_luaNoticeCount;
    ReleaseSRWLockExclusive(&g_luaNoticeLock);
    return true;
}

LuaClientEventContext BuildLuaDungeonClientEventContext(
    bool previousRoomKnown = false,
    unsigned int previousRoomX = 0,
    unsigned int previousRoomY = 0,
    unsigned int previousRoomLayerFlag = 0,
    unsigned int previousMapID = 0)
{
    LuaClientEventContext context = {};
    if (g_luaDungeonID != 0) {
        context.flags |= DNF90_CLIENT_EVENT_CONTEXT_DUNGEON_VALID;
        context.dungeonID = g_luaDungeonID;
    }
    if (g_luaDungeonRoomKnown) {
        context.flags |= DNF90_CLIENT_EVENT_CONTEXT_ROOM_VALID;
        context.roomX = g_luaDungeonRoomX;
        context.roomY = g_luaDungeonRoomY;
        context.roomLayerFlag = g_luaDungeonRoomLayerFlag;
        context.mapID = g_luaDungeonMapID;
        if (g_luaDungeonRoomX == g_luaDungeonBossX &&
            g_luaDungeonRoomY == g_luaDungeonBossY) {
            context.flags |= DNF90_CLIENT_EVENT_CONTEXT_BOSS_ROOM;
        }
    }
    if (previousRoomKnown) {
        context.flags |= DNF90_CLIENT_EVENT_CONTEXT_PREVIOUS_ROOM_VALID;
        context.previousRoomX = previousRoomX;
        context.previousRoomY = previousRoomY;
        context.previousRoomLayerFlag = previousRoomLayerFlag;
        context.previousMapID = previousMapID;
    }
    return context;
}

bool QueueLuaClientEventInternal(
    DNF90ClientEventType type, DWORD uiThreadID,
    const LuaClientEventContext* context = nullptr)
{
    DNF90ClientEvent pending = {};
    pending.size = sizeof(pending);
    pending.type = static_cast<unsigned int>(type);
    pending.sequence = static_cast<unsigned int>(
        InterlockedIncrement(&g_luaClientEventSequence));
    pending.processID = GetCurrentProcessId();
    pending.uiThreadID = uiThreadID;
    if (context) {
        pending.contextFlags = context->flags;
        pending.dungeonID = context->dungeonID;
        pending.roomX = context->roomX;
        pending.roomY = context->roomY;
        pending.roomLayerFlag = context->roomLayerFlag;
        pending.mapID = context->mapID;
        pending.previousRoomX = context->previousRoomX;
        pending.previousRoomY = context->previousRoomY;
        pending.previousRoomLayerFlag = context->previousRoomLayerFlag;
        pending.previousMapID = context->previousMapID;
    }

    AcquireSRWLockExclusive(&g_luaClientEventLock);
    if (g_luaClientEventCount >= kLuaClientEventQueueCapacity) {
        ReleaseSRWLockExclusive(&g_luaClientEventLock);
        LONG rejected = InterlockedIncrement(
            &g_luaClientEventRejectedCount);
        LogLine("lua client event rejected=%ld type=%u sequence=%u reason=queue-full",
            rejected, pending.type, pending.sequence);
        return false;
    }
    size_t tail = (g_luaClientEventHead + g_luaClientEventCount) %
        kLuaClientEventQueueCapacity;
    g_luaClientEventQueue[tail] = pending;
    ++g_luaClientEventCount;
    ReleaseSRWLockExclusive(&g_luaClientEventLock);

    LogLine("lua client event queued type=%u sequence=%u ui_tid=%lu pid=%lu "
        "context=0x%X dungeon=%u room=%u:%u layer=%u map=%u",
        pending.type, pending.sequence,
        static_cast<unsigned long>(pending.uiThreadID),
        static_cast<unsigned long>(pending.processID), pending.contextFlags,
        pending.dungeonID, pending.roomX, pending.roomY,
        pending.roomLayerFlag, pending.mapID);
    return true;
}

bool ParseEvidenceBackedDungeonStartMapBody(
    const unsigned char* body,
    size_t bodyLength,
    bool requireInitialRoom,
    unsigned char* roomX,
    unsigned char* roomY,
    unsigned char* roomLayerFlag,
    unsigned int* mapID)
{
    // The smallest valid current op29 body is 23 bytes. Offset 13 is the
    // initial room-state flag, offset 14 the map id, and offset 18 the first
    // variable-table count. Initial entry requires flag 1; later first visits
    // use flag 1 and revisits use flag 0. Parse every counted row so a
    // truncated packet cannot publish a lifecycle event merely by sharing the
    // same opcode.
    if (!body || !roomX || !roomY || !roomLayerFlag || !mapID ||
        bodyLength < 23 || body[2] > 1 || body[13] > 1 ||
        (requireInitialRoom && body[13] != 1)) {
        return false;
    }
    unsigned int parsedMapID =
        *reinterpret_cast<const unsigned int*>(body + 14);
    if (parsedMapID == 0) return false;

    size_t offset = 19;
    size_t actorCount = body[18];
    if (actorCount > (bodyLength - offset) / 21) return false;
    offset += actorCount * 21;

    if (offset >= bodyLength) return false;
    size_t extraCount = body[offset++];
    if (extraCount > (bodyLength - offset) / 21) return false;
    offset += extraCount * 21;

    // Hell-party fog byte followed by the ridable-object group table.
    if (bodyLength - offset < 2) return false;
    ++offset;
    size_t groupCount = body[offset++];
    for (size_t group = 0; group < groupCount; ++group) {
        if (offset >= bodyLength) return false;
        size_t entryCount = body[offset++];
        if (entryCount > (bodyLength - offset) / 20) return false;
        offset += entryCount * 20;
    }

    // Exactly one trailing party-member index completes the typed body.
    if (bodyLength - offset != 1) return false;
    *roomX = body[0];
    *roomY = body[1];
    *roomLayerFlag = body[2];
    *mapID = parsedMapID;
    return true;
}

void ResetCurrentDungeonBossMutationRoomState()
{
    AcquireSRWLockExclusive(&g_bossMutationActorLock);
    memset(g_bossMutationActors, 0, sizeof(g_bossMutationActors));
    g_bossMutationActorCount = 0;
    memset(g_bossMutationPending, 0, sizeof(g_bossMutationPending));
    g_bossMutationPendingCount = 0;
    ++g_bossMutationRoomGeneration;
    if (g_bossMutationRoomGeneration == 0) {
        g_bossMutationRoomGeneration = 1;
    }
    ReleaseSRWLockExclusive(&g_bossMutationActorLock);

    AcquireSRWLockExclusive(&g_bossMutationDecisionLock);
    memset(g_bossMutationDecisionQueue, 0,
        sizeof(g_bossMutationDecisionQueue));
    g_bossMutationDecisionHead = 0;
    g_bossMutationDecisionCount = 0;
    ReleaseSRWLockExclusive(&g_bossMutationDecisionLock);
}

void CacheCurrentDungeonBossMutationActors(
    const unsigned char* body, size_t bodyLength)
{
    if (!body || bodyLength < 23) return;
    const size_t actorCount = body[18];
    if (actorCount > kBossMutationActorCapacity ||
        actorCount > (bodyLength - 19) / 21) {
        return;
    }

    BossMutationActorRow rows[kBossMutationActorCapacity] = {};
    size_t count = 0;
    const unsigned char* actor = body + 19;
    for (size_t index = 0; index < actorCount; ++index, actor += 21) {
        const unsigned short objectKey =
            *reinterpret_cast<const unsigned short*>(actor + 6);
        const unsigned int monsterID =
            *reinterpret_cast<const unsigned int*>(actor + 8);
        if (objectKey == 0 || monsterID == 0) continue;
        rows[count].objectKey = objectKey;
        rows[count].monsterID = monsterID;
        rows[count].type = actor[13];
        ++count;
    }

    ResetCurrentDungeonBossMutationRoomState();
    AcquireSRWLockExclusive(&g_bossMutationActorLock);
    memcpy(g_bossMutationActors, rows,
        count * sizeof(BossMutationActorRow));
    g_bossMutationActorCount = count;
    const unsigned int generation = g_bossMutationRoomGeneration;
    ReleaseSRWLockExclusive(&g_bossMutationActorLock);
    LogLine("boss-mutation op29 actor cache generation=%u actors=%u",
        generation, static_cast<unsigned int>(count));
}

void UpdateLuaDungeonEntryStateAfterDispatch(
    int packet, unsigned int opcode)
{
    if (opcode != kDungeonInfoOpcode &&
        opcode != kDungeonStartMapOpcode &&
        opcode != kDungeonUserStateOpcode) {
        return;
    }

    unsigned int packetLength = 0;
    size_t bodyLength = 0;
    unsigned int dungeonID = 0;
    unsigned int mapID = 0;
    unsigned char roomX = 0;
    unsigned char roomY = 0;
    unsigned char roomLayerFlag = 0;
    unsigned short objectKey = 0;
    uintptr_t currentActor = 0;
    uintptr_t selectedActor = 0;
    __try {
        if (!packet) return;
        const unsigned char* bytes =
            reinterpret_cast<const unsigned char*>(packet);
        packetLength = *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (packetLength < 16) return;
        const unsigned char* body = bytes + 16;
        bodyLength = packetLength - 16;

        if (opcode == kDungeonInfoOpcode) {
            // The evidence-backed typed body is at least 36 bytes; optional
            // counted groups may extend it. A nonzero dungeon id is required.
            if (bodyLength < 36) return;
            dungeonID = *reinterpret_cast<const unsigned int*>(body);
            if (dungeonID == 0) return;
            g_luaDungeonEntryStage = kLuaDungeonEntryAwaitStartMap;
            g_luaDungeonID = dungeonID;
            g_luaDungeonBossX = body[8];
            g_luaDungeonBossY = body[9];
            g_luaDungeonRoomKnown = false;
            g_luaDungeonRoomX = 0;
            g_luaDungeonRoomY = 0;
            g_luaDungeonRoomLayerFlag = 0;
            g_luaDungeonMapID = 0;
            ResetCurrentDungeonBossMutationRoomState();
            LogLine("lua enter_dungeon armed stage=op28 packet_len=%u "
                "dungeon=%u boss=%u:%u",
                packetLength, dungeonID,
                g_luaDungeonBossX, g_luaDungeonBossY);
            return;
        }

        if (opcode == kDungeonStartMapOpcode) {
            bool initialRoom = g_luaDungeonEntryStage ==
                kLuaDungeonEntryAwaitStartMap;
            bool changedRoom = g_luaDungeonEntryStage ==
                kLuaDungeonEntryActive;
            if (!initialRoom && !changedRoom) {
                return;
            }
            if (!ParseEvidenceBackedDungeonStartMapBody(
                    body, bodyLength, initialRoom,
                    &roomX, &roomY, &roomLayerFlag, &mapID)) {
                LogLine("lua dungeon lifecycle waiting stage=op29-invalid "
                    "packet_len=%u body_len=%u initial=%u",
                    packetLength, static_cast<unsigned int>(bodyLength),
                    initialRoom ? 1u : 0u);
                return;
            }
            CacheCurrentDungeonBossMutationActors(body, bodyLength);
            if (initialRoom) {
                g_luaDungeonRoomKnown = true;
                g_luaDungeonRoomX = roomX;
                g_luaDungeonRoomY = roomY;
                g_luaDungeonRoomLayerFlag = roomLayerFlag;
                g_luaDungeonMapID = mapID;
                g_luaDungeonEntryStage = kLuaDungeonEntryAwaitUserState;
                LogLine("lua enter_dungeon armed stage=op29 packet_len=%u "
                    "room=%u:%u layer=%u map=%u",
                    packetLength, roomX, roomY, roomLayerFlag, mapID);
                return;
            }

            if (g_luaDungeonRoomKnown &&
                g_luaDungeonRoomX == roomX &&
                g_luaDungeonRoomY == roomY &&
                g_luaDungeonRoomLayerFlag == roomLayerFlag &&
                g_luaDungeonMapID == mapID) {
                LogLine("lua dungeon_room_changed suppressed reason=duplicate "
                    "packet_len=%u room=%u:%u layer=%u map=%u",
                    packetLength, roomX, roomY, roomLayerFlag, mapID);
                return;
            }
            unsigned char previousRoomX = g_luaDungeonRoomX;
            unsigned char previousRoomY = g_luaDungeonRoomY;
            unsigned char previousRoomLayerFlag =
                g_luaDungeonRoomLayerFlag;
            unsigned int previousMapID = g_luaDungeonMapID;
            bool previousRoomKnown = g_luaDungeonRoomKnown;
            g_luaDungeonRoomKnown = true;
            g_luaDungeonRoomX = roomX;
            g_luaDungeonRoomY = roomY;
            g_luaDungeonRoomLayerFlag = roomLayerFlag;
            g_luaDungeonMapID = mapID;
            void* controlledActor = nullptr;
            __try {
                controlledActor = *reinterpret_cast<void**>(
                    g_dnfBase + kControlledActorPointerRva);
            }
            __except (EXCEPTION_EXECUTE_HANDLER) {
                controlledActor = nullptr;
            }
            void* pickupContainer = EnsureDungeonPickupContainer(
                controlledActor, "dungeon-room-change");
            LuaClientEventContext context =
                BuildLuaDungeonClientEventContext(
                    previousRoomKnown,
                    previousRoomX, previousRoomY,
                    previousRoomLayerFlag, previousMapID);
            bool queued = QueueLuaClientEventInternal(
                DNF90_CLIENT_EVENT_DUNGEON_ROOM_CHANGED,
                GetCurrentThreadId(), &context);
            LogLine("lua dungeon_room_changed confirmed packet_len=%u "
                "previous_known=%u previous_room=%u:%u previous_layer=%u "
                "previous_map=%u room=%u:%u layer=%u map=%u queued=%u",
                packetLength, previousRoomKnown ? 1u : 0u,
                previousRoomX, previousRoomY, previousRoomLayerFlag,
                previousMapID, roomX, roomY, roomLayerFlag, mapID,
                queued ? 1u : 0u);
            LogLine("dungeon pickup lifecycle room=%u:%u actor=%p container=%p",
                roomX, roomY, controlledActor, pickupContainer);
            return;
        }

        if (g_luaDungeonEntryStage !=
            kLuaDungeonEntryAwaitUserState || bodyLength != 4 ||
            body[0] != 1 || body[3] != 1) {
            return;
        }
        objectKey = *reinterpret_cast<const unsigned short*>(body + 1);
        if (objectKey == 0) return;

        uintptr_t objectManager = *reinterpret_cast<uintptr_t*>(
            g_dnfBase + kObjectManagerPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_dnfBase + kCurrentActorRva);
        ActorByObjectKeyFn actorByObjectKeyFn =
            reinterpret_cast<ActorByObjectKeyFn>(
                g_dnfBase + kActorByObjectKeyRva);
        currentActor = objectManager && currentActorFn
            ? reinterpret_cast<uintptr_t>(
                currentActorFn(reinterpret_cast<void*>(objectManager)))
            : 0;
        selectedActor = actorByObjectKeyFn
            ? reinterpret_cast<uintptr_t>(actorByObjectKeyFn(objectKey))
            : 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("lua enter_dungeon waiting reason=read-exception "
            "opcode=%u packet_len=%u body_len=%u key=%u code=0x%08X",
            opcode, packetLength, static_cast<unsigned int>(bodyLength),
            objectKey, GetExceptionCode());
        return;
    }

    if (!currentActor || selectedActor != currentActor) {
        LogLine("lua enter_dungeon waiting reason=current-actor-mismatch "
            "packet_len=%u key=%u selected_actor=%p current_actor=%p",
            packetLength, objectKey,
            reinterpret_cast<void*>(selectedActor),
            reinterpret_cast<void*>(currentActor));
        return;
    }

    // Mark active even if the bounded queue is full: this exact transition
    // has already happened and a later room packet must never replay it.
    g_luaDungeonEntryStage = kLuaDungeonEntryActive;
    InterlockedExchange(&g_berserkerFrenzyRenderHitLogged, 0);
    InterlockedExchange(&g_berserkerFrenzyDiagnosticDungeonActive, 1);
    void* pickupContainer = EnsureDungeonPickupContainer(
        reinterpret_cast<void*>(currentActor), "dungeon-entry");
    UINT_PTR pickupTimer = g_dnfWindow
        ? SetTimer(g_dnfWindow, kDungeonPickupRepairTimer,
            kDungeonPickupRepairIntervalMs, nullptr)
        : 0;
    LuaClientEventContext context =
        BuildLuaDungeonClientEventContext();
    bool queued = QueueLuaClientEventInternal(
        DNF90_CLIENT_EVENT_ENTER_DUNGEON,
        GetCurrentThreadId(), &context);
    LogLine("lua enter_dungeon confirmed packet_len=%u key=%u actor=%p "
        "container=%p pickup_timer=%p queued=%u",
        packetLength, objectKey, reinterpret_cast<void*>(currentActor),
        pickupContainer, reinterpret_cast<void*>(pickupTimer),
        queued ? 1u : 0u);
}

// sub_2123320 is the current client's private WM_CREATE input initializer.
// It expects ESI to contain the game HWND, so call it through a register-
// preserving trampoline rather than treating it as a normal cdecl function.
__declspec(naked) void InvokeNativeInputResetForTownReturn()
{
    __asm {
        pushad
        mov esi, dword ptr [g_dnfWindow]
        mov eax, dword ptr [g_dnfBase]
        add eax, 01D23320h
        call eax
        popad
        ret
    }
}

void ResetNativeInputAfterDungeonReturn()
{
    if (!g_dnfBase || !g_dnfWindow) {
        LogLine("dungeon-return input reset skipped reason=missing-context "
            "base=0x%08X window=%p",
            static_cast<unsigned int>(g_dnfBase),
            reinterpret_cast<void*>(g_dnfWindow));
        return;
    }

    static const unsigned char kExpectedInputResetPrologue[] = {
        0x6A, 0x00, 0xFF, 0x15, 0x64, 0x28, 0x1A, 0x04,
        0x68, 0x00, 0x01, 0x00, 0x00, 0x6A, 0x00, 0x68,
        0x10, 0x07, 0x0D, 0x06
    };
    const unsigned char* target = reinterpret_cast<const unsigned char*>(
        g_dnfBase + kNativeInputResetRva);
    if (!BytesMatch(target, kExpectedInputResetPrologue,
            sizeof(kExpectedInputResetPrologue))) {
        LogLine("dungeon-return input reset skipped reason=signature-mismatch "
            "target=0x%08X expected=%02X%02X%02X%02X",
            static_cast<unsigned int>(g_dnfBase + kNativeInputResetRva),
            target[0], target[1], target[2], target[3]);
        return;
    }

    volatile unsigned char* keyboardState =
        reinterpret_cast<volatile unsigned char*>(
            g_dnfBase + kNativeKeyboardStateTableRva);
    unsigned char spaceBefore = 0;
    unsigned char spaceAfter = 0;
    __try {
        spaceBefore = keyboardState[kVirtualKeySpace];
        InvokeNativeInputResetForTownReturn();
        spaceAfter = keyboardState[kVirtualKeySpace];
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("dungeon-return input reset fault window=%p code=0x%08X",
            reinterpret_cast<void*>(g_dnfWindow), GetExceptionCode());
        return;
    }

    LogLine("dungeon-return input reset applied window=%p "
        "space_before=%u space_after=%u",
        reinterpret_cast<void*>(g_dnfWindow),
        static_cast<unsigned int>(spaceBefore),
        static_cast<unsigned int>(spaceAfter));
}

void QueueLuaEnterTownEventIfAccepted(
    int packet,
    unsigned int sceneModeCalls,
    unsigned int loadingGateCalls,
    bool sceneModeEarlyReturn,
    bool loadingGateEarlyReturn)
{
    if (sceneModeEarlyReturn || loadingGateEarlyReturn) {
        LogLine("lua enter_town suppressed reason=native-early-return "
            "scene_calls=%u loading_calls=%u scene_rejected=%u loading_rejected=%u",
            sceneModeCalls, loadingGateCalls,
            sceneModeEarlyReturn ? 1u : 0u,
            loadingGateEarlyReturn ? 1u : 0u);
        return;
    }

    unsigned int packetLength = 0;
    unsigned int rowCount = 0;
    unsigned int selectedObjectKey = 0;
    uintptr_t currentActor = 0;
    uintptr_t selectedActor = 0;
    __try {
        if (!packet) return;
        const unsigned char* bytes =
            reinterpret_cast<const unsigned char*>(packet);
        unsigned int opcode =
            *reinterpret_cast<const unsigned short*>(bytes + 1);
        packetLength = *reinterpret_cast<const unsigned int*>(bytes + 3);
        if (opcode != 24 || packetLength < 20) return;

        const unsigned char* body = bytes + 16;
        size_t bodyLength = packetLength - 16;
        rowCount = *reinterpret_cast<const unsigned short*>(body + 2);
        if (rowCount == 0 || rowCount > (bodyLength - 4) / 8) return;
        selectedObjectKey = *reinterpret_cast<const unsigned short*>(
            body + 4 + (rowCount - 1) * 8);
        if (selectedObjectKey == 0) return;

        uintptr_t objectManager = *reinterpret_cast<uintptr_t*>(
            g_dnfBase + kObjectManagerPointerRva);
        CurrentActorFn currentActorFn = reinterpret_cast<CurrentActorFn>(
            g_dnfBase + kCurrentActorRva);
        ActorByObjectKeyFn actorByObjectKeyFn =
            reinterpret_cast<ActorByObjectKeyFn>(
                g_dnfBase + kActorByObjectKeyRva);
        currentActor = objectManager && currentActorFn
            ? reinterpret_cast<uintptr_t>(
                currentActorFn(reinterpret_cast<void*>(objectManager)))
            : 0;
        selectedActor = actorByObjectKeyFn
            ? reinterpret_cast<uintptr_t>(actorByObjectKeyFn(
                static_cast<unsigned short>(selectedObjectKey)))
            : 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("lua enter_town suppressed reason=read-exception "
            "packet_len=%u rows=%u key=%u code=0x%08X",
            packetLength, rowCount, selectedObjectKey, GetExceptionCode());
        return;
    }

    if (!currentActor || selectedActor != currentActor) {
        LogLine("lua enter_town suppressed reason=current-actor-mismatch "
            "packet_len=%u rows=%u key=%u selected_actor=%p current_actor=%p",
            packetLength, rowCount, selectedObjectKey,
            reinterpret_cast<void*>(selectedActor),
            reinterpret_cast<void*>(currentActor));
        return;
    }

    // An accepted town transition closes any pending or active dungeon-entry
    // lifecycle. The next event must begin with a fresh typed op28 packet.
    LuaDungeonEntryStage previousDungeonStage = g_luaDungeonEntryStage;
    if (previousDungeonStage == kLuaDungeonEntryActive) {
        ResetNativeInputAfterDungeonReturn();
    }
    LuaClientEventContext leaveContext =
        BuildLuaDungeonClientEventContext();
    g_luaDungeonEntryStage = kLuaDungeonEntryIdle;
    InterlockedExchange(&g_berserkerFrenzyDiagnosticDungeonActive, 0);
    if (g_dnfWindow) {
        KillTimer(g_dnfWindow, kDungeonPickupRepairTimer);
    }
    g_luaDungeonID = 0;
    g_luaDungeonBossX = 0;
    g_luaDungeonBossY = 0;
    g_luaDungeonRoomKnown = false;
    g_luaDungeonRoomX = 0;
    g_luaDungeonRoomY = 0;
    g_luaDungeonRoomLayerFlag = 0;
    g_luaDungeonMapID = 0;
    ResetCurrentDungeonBossMutationRoomState();
    bool leaveQueued = false;
    if (previousDungeonStage == kLuaDungeonEntryActive) {
        leaveQueued = QueueLuaClientEventInternal(
            DNF90_CLIENT_EVENT_LEAVE_DUNGEON,
            GetCurrentThreadId(), &leaveContext);
        LogLine("lua leave_dungeon confirmed packet_len=%u key=%u actor=%p queued=%u",
            packetLength, selectedObjectKey,
            reinterpret_cast<void*>(currentActor), leaveQueued ? 1u : 0u);
    }
    bool townQueued = QueueLuaClientEventInternal(
        DNF90_CLIENT_EVENT_ENTER_TOWN, GetCurrentThreadId());
    LogLine("lua enter_town confirmed packet_len=%u rows=%u key=%u actor=%p "
        "scene_calls=%u loading_calls=%u dungeon_stage=%u "
        "leave_queued=%u queued=%u",
        packetLength, rowCount, selectedObjectKey,
        reinterpret_cast<void*>(currentActor), sceneModeCalls,
        loadingGateCalls, static_cast<unsigned int>(previousDungeonStage),
        leaveQueued ? 1u : 0u, townQueued ? 1u : 0u);
}

bool DequeueLuaClientEventInternal(DNF90ClientEvent* output)
{
    if (!output) return false;
    AcquireSRWLockExclusive(&g_luaClientEventLock);
    if (g_luaClientEventCount == 0) {
        ReleaseSRWLockExclusive(&g_luaClientEventLock);
        return false;
    }
    *output = g_luaClientEventQueue[g_luaClientEventHead];
    g_luaClientEventQueue[g_luaClientEventHead] = {};
    g_luaClientEventHead =
        (g_luaClientEventHead + 1) % kLuaClientEventQueueCapacity;
    --g_luaClientEventCount;
    ReleaseSRWLockExclusive(&g_luaClientEventLock);
    return true;
}

bool ValidateLuaGameControlEntrypoints()
{
    if (!g_dnfBase) return false;
    static const unsigned char kMotionPrologue[] = {
        0x55, 0x8B, 0xEC, 0x80, 0x7D, 0x0C, 0x00
    };
    static const unsigned char kMoodStorePrologue[] = {
        0x55, 0x8B, 0xEC, 0x66, 0x8B, 0x45, 0x08
    };
    static const unsigned char kMoodVisualPrologue[] = {
        0x55, 0x8B, 0xEC, 0x56, 0x8B, 0xF1
    };
    static const unsigned char kMoodEffectPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    return BytesMatch(reinterpret_cast<const unsigned char*>(
                          g_dnfBase + kSelfPlayMotionRva),
               kMotionPrologue, sizeof(kMotionPrologue)) &&
        BytesMatch(reinterpret_cast<const unsigned char*>(
                       g_dnfBase + kActorMoodStoreRva),
            kMoodStorePrologue, sizeof(kMoodStorePrologue)) &&
        BytesMatch(reinterpret_cast<const unsigned char*>(
                       g_dnfBase + kActorMoodVisualRva),
            kMoodVisualPrologue, sizeof(kMoodVisualPrologue)) &&
        BytesMatch(reinterpret_cast<const unsigned char*>(
                       g_dnfBase + kActorMoodEffectRva),
            kMoodEffectPrologue, sizeof(kMoodEffectPrologue)) &&
        BytesMatch(reinterpret_cast<const unsigned char*>(
                       g_dnfBase + kActorMoodAlternateEffectRva),
            kMoodEffectPrologue, sizeof(kMoodEffectPrologue));
}

bool RejectLuaGameCommand(const char* reason)
{
    LONG rejected = InterlockedIncrement(&g_luaGameCommandRejectedCount);
    if (rejected <= 16 || (rejected % 100) == 0) {
        LogLine("lua game command rejected=%ld reason=%s caller_tid=%lu",
            rejected, reason ? reason : "unknown", GetCurrentThreadId());
    }
    return false;
}

void* ResolveCurrentActorForLua()
{
    uintptr_t objectManager = *reinterpret_cast<uintptr_t*>(
        g_dnfBase + kObjectManagerPointerRva);
    CurrentActorFn currentActor = reinterpret_cast<CurrentActorFn>(
        g_dnfBase + kCurrentActorRva);
    return objectManager && currentActor
        ? currentActor(reinterpret_cast<void*>(objectManager))
        : nullptr;
}

bool AddLuaActorSnapshotRow(
    DNF90ActorSnapshot* snapshot, void* actor, unsigned int flags)
{
    if (!snapshot || !actor) return false;
    unsigned int objectKey = *reinterpret_cast<unsigned short*>(
        reinterpret_cast<uintptr_t>(actor) + kActorObjectKeyOffset);
    if (objectKey == 0) return false;

    for (unsigned int index = 0; index < snapshot->actorCount; ++index) {
        if (snapshot->actors[index].objectKey == objectKey) {
            snapshot->actors[index].flags |= flags;
            return true;
        }
    }
    if (snapshot->actorCount >= DNF90_ACTOR_SNAPSHOT_MAX_ACTORS) {
        return false;
    }

    DNF90ActorSnapshotRow& row = snapshot->actors[snapshot->actorCount++];
    row.objectKey = objectKey;
    row.flags = flags;
    row.x = *reinterpret_cast<int*>(
        reinterpret_cast<uintptr_t>(actor) + kActorXOffset);
    row.y = *reinterpret_cast<int*>(
        reinterpret_cast<uintptr_t>(actor) + kActorYOffset);
    row.z = *reinterpret_cast<int*>(
        reinterpret_cast<uintptr_t>(actor) + kActorZOffset);
    return true;
}

void RefreshLuaActorSnapshotOnUiThread(unsigned int requestedObjectKey)
{
    DNF90ActorSnapshot snapshot = {};
    snapshot.size = sizeof(snapshot);
    snapshot.generation = static_cast<unsigned int>(
        InterlockedIncrement(&g_actorSnapshotGeneration));
    snapshot.uiThreadID = GetCurrentThreadId();
    snapshot.capturedTick = GetTickCount();

    __try {
        void* currentActor = ResolveCurrentActorForLua();
        AddLuaActorSnapshotRow(&snapshot, currentActor, DNF90_ACTOR_CURRENT);

        uintptr_t sceneManager = *reinterpret_cast<uintptr_t*>(
            g_dnfBase + kSceneActorManagerPointerRva);
        SceneActorByIndexFn actorByIndex =
            reinterpret_cast<SceneActorByIndexFn>(
                g_dnfBase + kSceneActorByIndexRva);
        if (sceneManager && actorByIndex) {
            for (unsigned int index = 0; index < 8; ++index) {
                void* actor = actorByIndex(
                    reinterpret_cast<void*>(sceneManager), index);
                AddLuaActorSnapshotRow(
                    &snapshot, actor, DNF90_ACTOR_SCENE_SLOT);
            }
        }

        if (requestedObjectKey != 0 && requestedObjectKey <= 0xFFFFu) {
            ActorByObjectKeyFn actorByObjectKey =
                reinterpret_cast<ActorByObjectKeyFn>(
                    g_dnfBase + kActorByObjectKeyRva);
            void* actor = actorByObjectKey
                ? actorByObjectKey(
                      static_cast<unsigned short>(requestedObjectKey))
                : nullptr;
            AddLuaActorSnapshotRow(
                &snapshot, actor, DNF90_ACTOR_REQUESTED);
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("lua actor snapshot exception code=0x%08X requested_key=%u ui_tid=%lu",
            GetExceptionCode(), requestedObjectKey, GetCurrentThreadId());
    }

    AcquireSRWLockExclusive(&g_actorSnapshotLock);
    g_actorSnapshot = snapshot;
    ReleaseSRWLockExclusive(&g_actorSnapshotLock);
    LogLine("lua actor snapshot generation=%u actors=%u requested_key=%u ui_tid=%u",
        snapshot.generation, snapshot.actorCount, requestedObjectKey,
        snapshot.uiThreadID);
}

bool DequeueLuaGameCommand(DNF90GameCommand* output)
{
    if (!output) return false;
    AcquireSRWLockExclusive(&g_luaGameCommandLock);
    if (g_luaGameCommandCount == 0) {
        ReleaseSRWLockExclusive(&g_luaGameCommandLock);
        return false;
    }
    *output = g_luaGameCommandQueue[g_luaGameCommandHead];
    g_luaGameCommandQueue[g_luaGameCommandHead] = {};
    g_luaGameCommandHead =
        (g_luaGameCommandHead + 1) % kLuaGameCommandQueueCapacity;
    --g_luaGameCommandCount;
    ReleaseSRWLockExclusive(&g_luaGameCommandLock);
    return true;
}

void RemoveBossMutationPendingRequest(
    unsigned short token, unsigned short objectKey, unsigned int monsterID)
{
    AcquireSRWLockExclusive(&g_bossMutationActorLock);
    for (size_t index = 0; index < g_bossMutationPendingCount; ++index) {
        const BossMutationPendingRequest& pending =
            g_bossMutationPending[index];
        if (pending.token != token || pending.objectKey != objectKey ||
            pending.monsterID != monsterID) {
            continue;
        }
        for (size_t move = index + 1;
             move < g_bossMutationPendingCount; ++move) {
            g_bossMutationPending[move - 1] =
                g_bossMutationPending[move];
        }
        --g_bossMutationPendingCount;
        g_bossMutationPending[g_bossMutationPendingCount] = {};
        break;
    }
    ReleaseSRWLockExclusive(&g_bossMutationActorLock);
}

bool SendBossMutationRequestOnUiThread(
    unsigned int token, unsigned int monsterID)
{
    if (!g_dnfBase || token == 0 || token > 0xFFFFu || monsterID == 0 ||
        g_luaDungeonEntryStage != kLuaDungeonEntryActive) {
        return RejectLuaGameCommand("boss-mutation-context-inactive");
    }

    unsigned short objectKey = 0;
    unsigned int roomGeneration = 0;
    size_t matches = 0;
    AcquireSRWLockExclusive(&g_bossMutationActorLock);
    for (size_t index = 0; index < g_bossMutationActorCount; ++index) {
        const BossMutationActorRow& row = g_bossMutationActors[index];
        if (row.type != 3 || row.monsterID != monsterID) continue;
        objectKey = row.objectKey;
        ++matches;
    }
    if (matches != 1 || g_bossMutationPendingCount >=
            kBossMutationDecisionQueueCapacity) {
        ReleaseSRWLockExclusive(&g_bossMutationActorLock);
        return RejectLuaGameCommand(matches == 0
            ? "boss-mutation-op29-target-missing"
            : (matches > 1
                ? "boss-mutation-op29-target-ambiguous"
                : "boss-mutation-pending-full"));
    }
    for (size_t index = 0; index < g_bossMutationPendingCount; ++index) {
        if (g_bossMutationPending[index].token == token) {
            ReleaseSRWLockExclusive(&g_bossMutationActorLock);
            return RejectLuaGameCommand("boss-mutation-token-pending");
        }
    }
    roomGeneration = g_bossMutationRoomGeneration;
    BossMutationPendingRequest& pending =
        g_bossMutationPending[g_bossMutationPendingCount++];
    pending.token = static_cast<unsigned short>(token);
    pending.objectKey = objectKey;
    pending.monsterID = monsterID;
    pending.roomGeneration = roomGeneration;
    ReleaseSRWLockExclusive(&g_bossMutationActorLock);

    __try {
        UpperWriterValueFn value = reinterpret_cast<UpperWriterValueFn>(
            g_dnfBase + kUpperWriterValueRva);
        UpperWriterScalarFn writeU16 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU16Rva);
        UpperWriterScalarFn writeU8 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU8Rva);
        UpperWriterScalarFn writeU32 = reinterpret_cast<UpperWriterScalarFn>(
            g_dnfBase + kUpperWriterU32Rva);
        UpperWriterFlushFn flush = reinterpret_cast<UpperWriterFlushFn>(
            g_dnfBase + kUpperWriterFlushRva);
        if (!value || !writeU16 || !writeU8 || !writeU32 || !flush) {
            RemoveBossMutationPendingRequest(
                static_cast<unsigned short>(token), objectKey, monsterID);
            return RejectLuaGameCommand("boss-mutation-writer-unavailable");
        }
        writeU16(value(), kCommandCapabilityOpcode);
        writeU32(value(), kBossMutationRequestMagic);
        writeU8(value(), kBossMutationRequestVersion);
        writeU8(value(), 0);
        writeU16(value(), token);
        writeU16(value(), objectKey);
        writeU16(value(), 0);
        writeU32(value(), monsterID);
        flush();
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        RemoveBossMutationPendingRequest(
            static_cast<unsigned short>(token), objectKey, monsterID);
        LogLine("boss-mutation request send exception token=%u monster=%u key=%u code=0x%08X",
            token, monsterID, objectKey, GetExceptionCode());
        return false;
    }
    LogLine("boss-mutation request sent token=%u monster=%u key=%u room_generation=%u",
        token, monsterID, objectKey, roomGeneration);
    return true;
}

void ExecuteLuaGameCommandOnUiThread(const DNF90GameCommand& command)
{
    if (command.type == DNF90_GAME_COMMAND_REFRESH_ACTORS) {
        RefreshLuaActorSnapshotOnUiThread(command.objectKey);
        return;
    }
    if (command.type == DNF90_GAME_COMMAND_REQUEST_BOSS_MUTATION) {
        SendBossMutationRequestOnUiThread(
            command.objectKey, static_cast<unsigned int>(command.value));
        return;
    }
    if (!g_luaGameControlEntrypointsValid) {
        RejectLuaGameCommand("native-entrypoints-invalid");
        return;
    }

    __try {
        if (command.type == DNF90_GAME_COMMAND_SELF_PLAY_MOTION) {
            void* currentActor = ResolveCurrentActorForLua();
            void* motionManager = *reinterpret_cast<void**>(
                g_dnfBase + kMotionManagerPointerRva);
            SelfPlayMotionFn playMotion = reinterpret_cast<SelfPlayMotionFn>(
                g_dnfBase + kSelfPlayMotionRva);
            if (!currentActor || !motionManager || !playMotion) {
                RejectLuaGameCommand("self-motion-context-unavailable");
                return;
            }
            int result = playMotion(
                motionManager, command.value, static_cast<unsigned char>(1));
            LogLine("lua self motion executed motion=%d result=%d ui_tid=%lu",
                command.value, result, GetCurrentThreadId());
            return;
        }

        if (command.type == DNF90_GAME_COMMAND_APPLY_ACTOR_MOOD) {
            void* actor = nullptr;
            if (command.objectKey == 0) {
                actor = ResolveCurrentActorForLua();
            } else {
                ActorByObjectKeyFn actorByObjectKey =
                    reinterpret_cast<ActorByObjectKeyFn>(
                        g_dnfBase + kActorByObjectKeyRva);
                actor = actorByObjectKey
                    ? actorByObjectKey(
                          static_cast<unsigned short>(command.objectKey))
                    : nullptr;
            }
            if (!actor) {
                RejectLuaGameCommand("mood-actor-unavailable");
                return;
            }

            ActorMoodStoreFn storeMood = reinterpret_cast<ActorMoodStoreFn>(
                g_dnfBase + kActorMoodStoreRva);
            ActorMoodVisualFn resolveVisual =
                reinterpret_cast<ActorMoodVisualFn>(
                    g_dnfBase + kActorMoodVisualRva);
            ActorMoodEffectFn applyEffect =
                reinterpret_cast<ActorMoodEffectFn>(g_dnfBase +
                    (command.value < 1000
                            ? kActorMoodEffectRva
                            : kActorMoodAlternateEffectRva));
            storeMood(actor, static_cast<short>(command.value));
            void* visual = resolveVisual(actor, nullptr);
            applyEffect(actor, visual, command.value,
                static_cast<unsigned char>(1));
            LogLine("lua actor mood executed key=%u mood=%d actor=%p ui_tid=%lu",
                command.objectKey, command.value, actor,
                GetCurrentThreadId());
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("lua game command exception type=%u key=%u value=%d code=0x%08X ui_tid=%lu",
            command.type, command.objectKey, command.value,
            GetExceptionCode(), GetCurrentThreadId());
    }
}

void DrainLuaGameCommandsOnUiThread()
{
    for (size_t index = 0; index < kLuaGameCommandQueueCapacity; ++index) {
        DNF90GameCommand command = {};
        if (!DequeueLuaGameCommand(&command)) break;
        ExecuteLuaGameCommandOnUiThread(command);
    }
}

void QueueLuaUiClosedEventOnce(DWORD uiThreadID)
{
    if (InterlockedCompareExchange(
            &g_luaUiClosedEventQueued, 1, 0) != 0) {
        return;
    }
    if (!QueueLuaClientEventInternal(
            DNF90_CLIENT_EVENT_UI_CLOSED, uiThreadID)) {
        InterlockedExchange(&g_luaUiClosedEventQueued, 0);
    }
}

LRESULT CALLBACK ProxyDnfWindowProc(
    HWND hWnd, UINT message, WPARAM wParam, LPARAM lParam)
{
    WNDPROC original = g_originalDnfWindowProc;
    if (!original) return DefWindowProcW(hWnd, message, wParam, lParam);

    // Do not treat WM_CLOSE as final because the client may cancel a close
    // request. WM_DESTROY is the first confirmed shutdown boundary and
    // WM_NCDESTROY is its idempotent fallback.
    if (message == WM_DESTROY || message == WM_NCDESTROY) {
        QueueLuaUiClosedEventOnce(GetCurrentThreadId());
        if (message == WM_DESTROY) {
            KillTimer(hWnd, kCombatPanelPollTimer);
            KillTimer(hWnd, kDungeonPickupRepairTimer);
            if (g_combatPanelGuideWindow &&
                IsWindow(g_combatPanelGuideWindow)) {
                DestroyWindow(g_combatPanelGuideWindow);
            }
            if (g_combatPanelRankTooltipWindow &&
                IsWindow(g_combatPanelRankTooltipWindow)) {
                DestroyWindow(g_combatPanelRankTooltipWindow);
            }
            if (g_combatPanelOverlayWindow &&
                IsWindow(g_combatPanelOverlayWindow)) {
                DestroyWindow(g_combatPanelOverlayWindow);
            }
            if (g_debugPanelOverlayWindow &&
                IsWindow(g_debugPanelOverlayWindow)) {
                DestroyWindow(g_debugPanelOverlayWindow);
            }
            g_combatPanelGuideWindow = nullptr;
            g_combatPanelGuideVisible = false;
            g_combatPanelRankTooltipWindow = nullptr;
            g_combatPanelRankTooltipVisible = false;
            g_combatPanelOverlayWindow = nullptr;
            g_combatPanelPersonalInfoOpen = false;
            g_debugPanelOverlayWindow = nullptr;
        }
        if (message == WM_NCDESTROY) {
            InterlockedExchangePointer(&g_luaNoticeBridgeWindow, nullptr);
            InterlockedExchange(&g_luaNoticeWindowMessage, 0);
            InterlockedExchange(&g_luaGameCommandWindowMessage, 0);
            InterlockedExchange(&g_combatPanelWindowMessage, 0);
            InterlockedExchange(&g_debugPanelWindowMessage, 0);
            g_dnfWindow = nullptr;
        }
    }

    if (message == WM_TIMER && wParam == kCombatPanelPollTimer) {
        RefreshCombatPanelOverlay(hWnd);
        RefreshDebugPanelOverlay(hWnd);
        // 交易窗口关窗检测：客户端自身不发送任何关窗信号（其解锁发送器在本
        // 版本里不可达），必须由这里按框架显示标志判定并补发 op24/状态 0。
        CheckTradeWindowClosed();
        return 0;
    }

    if (message == WM_TIMER && wParam == kDungeonPickupRepairTimer) {
        if (g_luaDungeonEntryStage != kLuaDungeonEntryActive) {
            KillTimer(hWnd, kDungeonPickupRepairTimer);
            return 0;
        }
        void* controlledActor = nullptr;
        __try {
            controlledActor = *reinterpret_cast<void**>(
                g_dnfBase + kControlledActorPointerRva);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            controlledActor = nullptr;
        }
        EnsureDungeonPickupContainer(
            controlledActor, "dungeon-active-timer");
        return 0;
    }

    if ((message == WM_MOVE || message == WM_SIZE) &&
        g_combatPanelOverlayWindow &&
        IsWindowVisible(g_combatPanelOverlayWindow)) {
        PositionCombatPanelOverlay(hWnd);
        if (g_combatPanelGuideVisible) {
            SetCombatPanelGuideVisible(hWnd, true);
        }
    }
    if ((message == WM_MOVE || message == WM_SIZE) &&
        g_debugPanelOverlayWindow &&
        IsWindowVisible(g_debugPanelOverlayWindow)) {
        PositionDebugPanelOverlay(hWnd);
    }

    UINT gameCommandMessage = static_cast<UINT>(
        InterlockedCompareExchange(&g_luaGameCommandWindowMessage, 0, 0));
    if (gameCommandMessage && message == gameCommandMessage) {
        DrainLuaGameCommandsOnUiThread();
        return 0;
    }

    UINT noticeMessage = static_cast<UINT>(
        InterlockedCompareExchange(&g_luaNoticeWindowMessage, 0, 0));
    if (noticeMessage && message == noticeMessage) {
        LuaClientNotice notice = {};
        if (!DequeueLuaClientNotice(&notice)) return 0;

        __try {
            const bool dungeonSelect =
                notice.style == kClientNoticeStyleDungeonSelect;
            const bool rendered = dungeonSelect
                ? ShowNativeDungeonSelectFatiguePrompt(notice)
                : ShowLuaClientNoticeOverlay(hWnd, notice);
            if (rendered) {
                LONG dispatched = InterlockedIncrement(
                    &g_luaNoticeDispatchCount);
                LogLine("client notice dispatched=%ld chars=%u style=%u ui_tid=%lu renderer=%s",
                    dispatched, notice.length, notice.style,
                    GetCurrentThreadId(), dungeonSelect
                        ? "native-selector-feed+native-chat"
                        : "owned-window-overlay");
            } else {
                RejectLuaClientNotice(dungeonSelect
                    ? "native-selector-chat-render-failed"
                    : "overlay-render-failed");
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            LogLine("lua client notice dispatch exception code=0x%08X chars=%u ui_tid=%lu",
                GetExceptionCode(), notice.length, GetCurrentThreadId());
        }
        return 0;
    }

    UINT combatPanelMessage = static_cast<UINT>(
        InterlockedCompareExchange(&g_combatPanelWindowMessage, 0, 0));
    if (combatPanelMessage && message == combatPanelMessage) {
        RefreshCombatPanelOverlay(hWnd);
        if (g_combatPanelOverlayWindow) {
            InvalidateRect(g_combatPanelOverlayWindow, nullptr, FALSE);
        }
        if (g_combatPanelRankTooltipWindow &&
            IsWindowVisible(g_combatPanelRankTooltipWindow)) {
            InvalidateRect(g_combatPanelRankTooltipWindow, nullptr, FALSE);
        }
        if (g_combatPanelGuideWindow &&
            IsWindowVisible(g_combatPanelGuideWindow)) {
            InvalidateRect(g_combatPanelGuideWindow, nullptr, FALSE);
        }
        return 0;
    }

    UINT debugPanelMessage = static_cast<UINT>(
        InterlockedCompareExchange(&g_debugPanelWindowMessage, 0, 0));
    if (debugPanelMessage && message == debugPanelMessage) {
        RefreshDebugPanelOverlay(hWnd);
        if (g_debugPanelOverlayWindow) {
            InvalidateRect(g_debugPanelOverlayWindow, nullptr, FALSE);
        }
        return 0;
    }

    if (message == WM_LBUTTONDOWN) {
        // The stock drag dispatcher enumerates only twelve quick-slot cells.
        // Keep an independent source for the two appended native controls so
        // moves involving slot 204/205 still enter the authoritative client
        // move sender after the original window procedure finishes.
        BeginSkillQuickSlotMouseDrag();
        return CallWindowProcW(original, hWnd, message, wParam, lParam);
    }

    if (message == WM_LBUTTONUP) {
        LRESULT result = CallWindowProcW(
            original, hWnd, message, wParam, lParam);
        CompleteSkillQuickSlotMouseDrag();
        return result;
    }

    if (message == WM_RBUTTONDOWN || message == WM_RBUTTONDBLCLK) {
        LONG before = InterlockedCompareExchange(
            &g_clientOp44SendCount, 0, 0);
        CurrentContractSelection selection = {};
        bool selected = ResolveHoveredPremiumContractSelection(&selection);
        LRESULT result = CallWindowProcW(
            original, hWnd, message, wParam, lParam);
        if (!selected) {
            selected = ResolveHoveredPremiumContractSelection(&selection);
        }

        g_contractRButtonPending = selected;
        g_contractRButtonPendingSlot = selected ? selection.slot : -1;
        g_contractRButtonPendingTemplateID =
            selected ? selection.templateID : 0;
        g_contractRButtonPendingIdentity = selected ? selection.identity : 0;
        g_contractRButtonPendingOp44Count = before;
        if (selected) {
            LogLine("contract-use right-button down slot=%d item=%lu identity=%lu before=%ld",
                selection.slot,
                static_cast<unsigned long>(selection.templateID),
                static_cast<unsigned long>(selection.identity), before);
        }
        return result;
    }

    if (message == WM_RBUTTONUP) {
        LRESULT result = CallWindowProcW(
            original, hWnd, message, wParam, lParam);
        if (!g_contractRButtonPending) return result;

        CurrentContractSelection selection = {};
        selection.slot = g_contractRButtonPendingSlot;
        selection.templateID = g_contractRButtonPendingTemplateID;
        selection.identity = g_contractRButtonPendingIdentity;
        LONG before = g_contractRButtonPendingOp44Count;
        g_contractRButtonPending = false;
        g_contractRButtonPendingSlot = -1;
        g_contractRButtonPendingTemplateID = 0;
        g_contractRButtonPendingIdentity = 0;

        LONG after = InterlockedCompareExchange(
            &g_clientOp44SendCount, 0, 0);
        if (after == before && GetForegroundWindow() == hWnd) {
            g_contractUseFallbackActive = true;
            bool sent = SendCurrentPremiumContractUse(selection);
            g_contractUseFallbackActive = false;
            if (sent) {
                LONG hit = InterlockedIncrement(
                    &g_contractRButtonFallbackCount);
                LogLine("contract-use right-button fallback hit=%ld slot=%d item=%lu identity=%lu",
                    hit, selection.slot,
                    static_cast<unsigned long>(selection.templateID),
                    static_cast<unsigned long>(selection.identity));
            }
        }
        return result;
    }

    return CallWindowProcW(original, hWnd, message, wParam, lParam);
}

BOOL CALLBACK FindCurrentProcessMainWindow(HWND hWnd, LPARAM parameter)
{
    DWORD processID = 0;
    GetWindowThreadProcessId(hWnd, &processID);
    if (processID != GetCurrentProcessId() || !IsWindowVisible(hWnd) ||
        GetWindow(hWnd, GW_OWNER) != nullptr) {
        return TRUE;
    }
    *reinterpret_cast<HWND*>(parameter) = hWnd;
    return FALSE;
}

bool InstallContractUseWindowProc()
{
    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    HWND window = nullptr;
    while (!window && waitedMs < kWaitLimitMs) {
        EnumWindows(&FindCurrentProcessMainWindow,
            reinterpret_cast<LPARAM>(&window));
        if (window) break;
        Sleep(250);
        waitedMs += 250;
    }
    if (!window) {
        LogLine("contract-use main window unavailable after wait_ms=%d",
            waitedMs);
        return false;
    }

    WNDPROC original = reinterpret_cast<WNDPROC>(
        GetWindowLongPtrW(window, GWLP_WNDPROC));
    if (!original) {
        LogLine("contract-use main window has no window procedure hwnd=%p",
            window);
        return false;
    }
    g_originalDnfWindowProc = original;
    SetLastError(ERROR_SUCCESS);
    LONG_PTR previous = SetWindowLongPtrW(
        window, GWLP_WNDPROC,
        reinterpret_cast<LONG_PTR>(&ProxyDnfWindowProc));
    DWORD error = GetLastError();
    if (!previous && error != ERROR_SUCCESS) {
        g_originalDnfWindowProc = nullptr;
        LogLine("contract-use window procedure install failed hwnd=%p error=%lu",
            window, error);
        return false;
    }
    g_dnfWindow = window;
    g_originalDnfWindowProc = reinterpret_cast<WNDPROC>(previous);
    InterlockedExchangePointer(&g_luaNoticeBridgeWindow, window);

    UINT noticeMessage = RegisterWindowMessageW(kLuaNoticeWindowMessageName);
    if (noticeMessage != 0) {
        InterlockedExchange(&g_luaNoticeWindowMessage,
            static_cast<LONG>(noticeMessage));
        DWORD windowProcessID = 0;
        DWORD windowThreadID = GetWindowThreadProcessId(
            window, &windowProcessID);
        LogLine("lua client notice bridge ready hwnd=%p message=0x%04X ui_tid=%lu pid=%lu renderer=owned-popup-overlay",
            window, noticeMessage, windowThreadID,
            windowProcessID);
        if (InterlockedCompareExchange(
                &g_luaUiReadyEventQueued, 1, 0) == 0 &&
            !QueueLuaClientEventInternal(
                DNF90_CLIENT_EVENT_UI_READY, windowThreadID)) {
            InterlockedExchange(&g_luaUiReadyEventQueued, 0);
        }
    } else {
        LogLine("lua client notice bridge unavailable message=0x%04X error=%lu",
            noticeMessage,
            GetLastError());
    }

    UINT gameCommandMessage = RegisterWindowMessageW(
        kLuaGameCommandWindowMessageName);
    if (gameCommandMessage != 0) {
        InterlockedExchange(&g_luaGameCommandWindowMessage,
            static_cast<LONG>(gameCommandMessage));
        LogLine("lua game-command bridge ready hwnd=%p message=0x%04X",
            window, gameCommandMessage);
    } else {
        LogLine("lua game-command bridge unavailable error=%lu",
            GetLastError());
    }

    UINT combatPanelMessage = RegisterWindowMessageW(
        kCombatPanelWindowMessageName);
    if (combatPanelMessage != 0) {
        InterlockedExchange(&g_combatPanelWindowMessage,
            static_cast<LONG>(combatPanelMessage));
        UINT_PTR timer = SetTimer(window, kCombatPanelPollTimer,
            kCombatPanelPollIntervalMs, nullptr);
        LogLine("combat-panel UI bridge ready hwnd=%p message=0x%04X "
            "timer=%p interval_ms=%u owner=%u is_open=+0x%08X",
            window, combatPanelMessage,
            reinterpret_cast<void*>(timer), kCombatPanelPollIntervalMs,
            static_cast<unsigned int>(kPersonalInfoUiOwner),
            static_cast<unsigned int>(kSceneUiIsOpenRva));
    } else {
        LogLine("combat-panel UI bridge unavailable error=%lu",
            GetLastError());
    }
    UINT debugPanelMessage = RegisterWindowMessageW(
        kDebugPanelWindowMessageName);
    if (debugPanelMessage != 0) {
        InterlockedExchange(&g_debugPanelWindowMessage,
            static_cast<LONG>(debugPanelMessage));
        LogLine("debug-panel UI bridge ready hwnd=%p message=0x%04X ",
            window, debugPanelMessage);
    } else {
        LogLine("debug-panel UI bridge unavailable error=%lu", GetLastError());
    }
    LogLine("contract-use window procedure installed hwnd=%p original=%p wait_ms=%d",
        window, g_originalDnfWindowProc, waitedMs);
    return g_originalDnfWindowProc != nullptr;
}

int __fastcall ProxyCurrentItemRowParse(int itemObject, int unused, char* raw)
{
    unsigned char rawCopy[kCurrentItemRawBytes] = { 0 };
    bool captured = TryCopyTraceBytes(raw, rawCopy, sizeof(rawCopy));
    int result = g_originalCurrentItemRowParse
        ? g_originalCurrentItemRowParse(itemObject, unused, raw)
        : 0;
    if (captured) CacheCurrentItemRow(static_cast<uintptr_t>(itemObject), rawCopy);
    return result;
}

int __fastcall ProxyInventoryUseUIA(
    void* self, void* /*unused*/, int eventCode, int eventDetail, int arg2, int arg3)
{
    InventoryUseUIAFn original =
        reinterpret_cast<InventoryUseUIAFn>(g_originalInventoryUseUIA);
    if (!original) return 0;
    if (g_contractUseFallbackActive ||
        eventCode != 0x12C || eventDetail != 0x0D) {
        return original(self, eventCode, eventDetail, arg2, arg3);
    }

    LONG hit = InterlockedIncrement(&g_contractUseUIAHitCount);
    CurrentContractSelection selection = {};
    bool contractSelected = ResolveCurrentPremiumContractSelection(&selection);
    LONG before = InterlockedCompareExchange(&g_clientOp44SendCount, 0, 0);
    LogLine("contract-use callback A hit=%ld selected=%d slot=%d item=%lu identity=%lu before=%ld",
        hit, contractSelected ? 1 : 0, selection.slot,
        static_cast<unsigned long>(selection.templateID),
        static_cast<unsigned long>(selection.identity), before);
    g_contractUseFallbackActive = true;
    int result = original(self, eventCode, eventDetail, arg2, arg3);
    LONG after = InterlockedCompareExchange(&g_clientOp44SendCount, 0, 0);
    if (contractSelected && after == before) {
        SendCurrentPremiumContractUse(selection);
    }
    g_contractUseFallbackActive = false;
    return result;
}

void __cdecl ProxyInventoryUseUIB()
{
    InventoryUseUIBFn original =
        reinterpret_cast<InventoryUseUIBFn>(g_originalInventoryUseUIB);
    if (!original) return;
    if (g_contractUseFallbackActive) {
        original();
        return;
    }

    LONG hit = InterlockedIncrement(&g_contractUseUIBHitCount);
    CurrentContractSelection selection = {};
    bool contractSelected = ResolveCurrentPremiumContractSelection(&selection);
    LONG before = InterlockedCompareExchange(&g_clientOp44SendCount, 0, 0);
    LogLine("contract-use callback B hit=%ld selected=%d slot=%d item=%lu identity=%lu before=%ld",
        hit, contractSelected ? 1 : 0, selection.slot,
        static_cast<unsigned long>(selection.templateID),
        static_cast<unsigned long>(selection.identity), before);
    g_contractUseFallbackActive = true;
    original();
    LONG after = InterlockedCompareExchange(&g_clientOp44SendCount, 0, 0);
    if (contractSelected && after == before) {
        SendCurrentPremiumContractUse(selection);
    }
    g_contractUseFallbackActive = false;
}

uintptr_t RunInventoryUseGateFallback(
    InventoryUseGateFn original, volatile LONG* hitCounter, const char* label,
    void* self, int arg0, int arg1, int arg2)
{
    if (!original) return 0;
    if (g_contractUseFallbackActive) {
        return original(self, arg0, arg1, arg2);
    }

    LONG hit = InterlockedIncrement(hitCounter);
    CurrentContractSelection selection = {};
    bool contractSelected = ResolveCurrentPremiumContractSelection(&selection);
    LONG before = InterlockedCompareExchange(&g_clientOp44SendCount, 0, 0);
    LogLine("contract-use %s hit=%ld selected=%d slot=%d item=%lu identity=%lu before=%ld args=%08X/%08X/%08X",
        label, hit, contractSelected ? 1 : 0, selection.slot,
        static_cast<unsigned long>(selection.templateID),
        static_cast<unsigned long>(selection.identity), before,
        static_cast<unsigned int>(arg0), static_cast<unsigned int>(arg1),
        static_cast<unsigned int>(arg2));

    // The current client can reject a contract in sub_275A140's native 0x7D4
    // eligibility check before any of the three lower op44 callbacks runs.
    // Keep the whole native callback first; only recover an allow-listed,
    // selected main-bag contract when that callback emitted no op44.
    g_contractUseFallbackActive = true;
    uintptr_t result = original(self, arg0, arg1, arg2);
    if (!contractSelected) {
        contractSelected = ResolveCurrentPremiumContractSelection(&selection);
    }
    LONG after = InterlockedCompareExchange(&g_clientOp44SendCount, 0, 0);
    if (contractSelected && after == before) {
        SendCurrentPremiumContractUse(selection);
    }
    g_contractUseFallbackActive = false;
    return result;
}

uintptr_t __fastcall ProxyInventoryUseGateA(
    void* self, void* /*unused*/, int arg0, int arg1, int arg2)
{
    return RunInventoryUseGateFallback(
        reinterpret_cast<InventoryUseGateFn>(g_originalInventoryUseGateA),
        &g_contractUseGateAHitCount, "gate-A", self, arg0, arg1, arg2);
}

uintptr_t __fastcall ProxyInventoryUseGateB(
    void* self, void* /*unused*/, int arg0, int arg1, int arg2)
{
    return RunInventoryUseGateFallback(
        reinterpret_cast<InventoryUseGateFn>(g_originalInventoryUseGateB),
        &g_contractUseGateBHitCount, "gate-B", self, arg0, arg1, arg2);
}

unsigned char __fastcall ProxyInventoryUsePanel(
    void* self, void* /*unused*/, uint32_t templateID, int slot)
{
    InventoryUsePanelFn original =
        reinterpret_cast<InventoryUsePanelFn>(g_originalInventoryUsePanel);
    if (!original) return 0;
    if (g_contractUseFallbackActive) {
        return original(self, templateID, slot);
    }

    LONG hit = InterlockedIncrement(&g_contractUsePanelHitCount);
    CurrentContractSelection selection = {};
    bool contractSelected = ResolveCurrentPremiumContractPanelSelection(
        templateID, slot, &selection);
    LONG before = InterlockedCompareExchange(&g_clientOp44SendCount, 0, 0);
    LogLine("contract-use panel hit=%ld selected=%d slot=%d item=%lu identity=%lu before=%ld",
        hit, contractSelected ? 1 : 0, selection.slot,
        static_cast<unsigned long>(selection.templateID),
        static_cast<unsigned long>(selection.identity), before);

    // Preserve the native panel behavior first.  The fallback only covers its
    // audited global-gate branch when no C2S op44 reached the packet logger.
    g_contractUseFallbackActive = true;
    unsigned char result = original(self, templateID, slot);
    LONG after = InterlockedCompareExchange(&g_clientOp44SendCount, 0, 0);
    if (contractSelected && after == before) {
        SendCurrentPremiumContractUse(selection);
    }
    g_contractUseFallbackActive = false;
    return result;
}

void LogSocketPanelSelection(int selectedItem)
{
    LONG selection = InterlockedIncrement(&g_socketTraceSelectionCount);
    if (selection > kSocketTraceMaxSelections) return;

    // Current NoPack's selected item wrapper uses vtable+0x28
    // (sub_105FB00) to return this+8.  Its +0x18 field is the exact u32
    // template value consumed by sub_2112D50.  sub_21792A0 itself receives
    // a stack-local parser object, so its pointer cannot be compared to this
    // persistent selected wrapper after scene-row parsing has returned.
    uintptr_t itemData = static_cast<uintptr_t>(selectedItem) + 8;
    uint32_t templateID = 0;
    bool templateReadable = TryCopyTraceBytes(reinterpret_cast<const void*>(itemData + 0x18),
        &templateID, sizeof(templateID));
    CurrentItemRowTrace matches[kSocketTraceTemplateMatches] = {};
    LONG matchCount = templateReadable
        ? FindCurrentItemRowsByTemplate(templateID, matches, kSocketTraceTemplateMatches)
        : 0;
    unsigned char objectBytes[0xC4] = { 0 };
    bool objectReadable = TryCopyTraceBytes(reinterpret_cast<const void*>(itemData),
        objectBytes, sizeof(objectBytes));
    char objectHex[640] = { 0 };
    if (objectReadable) FormatTraceBytes(objectBytes, sizeof(objectBytes), objectHex, sizeof(objectHex));

    // The selected pointer itself is the polymorphic wrapper consumed by
    // sub_2112F70.  Record its vtable and only the two narrow ranges needed to
    // map the lock renderer back to the current raw row.  This is passive
    // evidence collection: no wrapper, vtable, packet or item byte is changed.
    uintptr_t selectedVtable = 0;
    bool selectedVtableReadable = TryCopyTraceBytes(reinterpret_cast<const void*>(selectedItem),
        &selectedVtable, sizeof(selectedVtable));
    unsigned char wrapperHead[0x40] = { 0 };
    bool wrapperHeadReadable = TryCopyTraceBytes(reinterpret_cast<const void*>(selectedItem),
        wrapperHead, sizeof(wrapperHead));
    char wrapperHeadHex[256] = { 0 };
    if (wrapperHeadReadable) {
        FormatTraceBytes(wrapperHead, sizeof(wrapperHead), wrapperHeadHex, sizeof(wrapperHeadHex));
    }
    unsigned char wrapperState[0x120] = { 0 };
    bool wrapperStateReadable = TryCopyTraceBytes(
        reinterpret_cast<const void*>(static_cast<uintptr_t>(selectedItem) + 0x2E0),
        wrapperState, sizeof(wrapperState));
    char wrapperStateHex[896] = { 0 };
    if (wrapperStateReadable) {
        FormatTraceBytes(wrapperState, sizeof(wrapperState), wrapperStateHex, sizeof(wrapperStateHex));
    }
    unsigned char vtableBytes[0xE0] = { 0 };
    bool vtableReadable = selectedVtableReadable && TryCopyTraceBytes(
        reinterpret_cast<const void*>(selectedVtable), vtableBytes, sizeof(vtableBytes));
    char vtableHex[704] = { 0 };
    if (vtableReadable) {
        FormatTraceBytes(vtableBytes, sizeof(vtableBytes), vtableHex, sizeof(vtableHex));
    }

    LogLine("socket-trace panel-select ordinal=%ld item_wrapper=0x%08X item_data=0x%08X template_read=%d template_id=%lu raw_template_matches=%ld cached_rows=%ld",
        selection, selectedItem, itemData, templateReadable ? 1 : 0,
        static_cast<unsigned long>(templateID), matchCount, g_socketTraceRowSequence);
    for (LONG i = 0; i < matchCount; ++i) {
        char rawHex[384] = { 0 };
        FormatTraceBytes(matches[i].raw, sizeof(matches[i].raw), rawHex, sizeof(rawHex));
        LogLine("socket-trace raw-template-match ordinal=%ld match=%ld parse_sequence=%ld parser_object=0x%08X raw_slot=%u raw_template_id=%lu raw77=%s",
            selection, i + 1, matches[i].sequence, matches[i].parserObject,
            CurrentItemRowSlot(matches[i].raw),
            static_cast<unsigned long>(CurrentItemRowTemplateID(matches[i].raw)), rawHex);
    }
    LogLine("socket-trace panel-object ordinal=%ld item_data=0x%08X bytes_c4=%s",
        selection, itemData, objectReadable ? objectHex : "<read-failed>");
    LogLine("socket-trace selected-wrapper ordinal=%ld wrapper=0x%08X vtable_read=%d vtable=0x%08X head_40=%s",
        selection, selectedItem, selectedVtableReadable ? 1 : 0, selectedVtable,
        wrapperHeadReadable ? wrapperHeadHex : "<read-failed>");
    LogLine("socket-trace selected-wrapper-state ordinal=%ld wrapper=0x%08X range=+0x2E0..+0x3FF bytes_120=%s",
        selection, selectedItem, wrapperStateReadable ? wrapperStateHex : "<read-failed>");
    LogLine("socket-trace selected-vtable ordinal=%ld vtable=0x%08X bytes_e0=%s",
        selection, selectedVtable, vtableReadable ? vtableHex : "<read-failed>");
}

char __fastcall ProxySocketPanelSelect(void* self, void* /*unused*/, int selectedItem, int arg)
{
    char result = g_originalSocketPanelSelect
        ? g_originalSocketPanelSelect(self, selectedItem, arg)
        : 0;
    LogSocketPanelSelection(selectedItem);
    return result;
}

char __fastcall ProxySocketOpenWriter(void* self, void* /*unused*/)
{
    LONG hit = InterlockedIncrement(&g_socketTraceWriterCount);
    uintptr_t selectedItem = 0;
    bool selectedReadable = self && TryCopyTraceBytes(
        reinterpret_cast<const void*>(reinterpret_cast<uintptr_t>(self) + 0x1B4),
        &selectedItem, sizeof(selectedItem));
    if (hit <= kSocketTraceMaxWriterHits) {
        LogLine("socket-trace writer-enter ordinal=%ld ui=0x%08X selected_read=%d selected_wrapper=0x%08X",
            hit, self, selectedReadable ? 1 : 0, selectedItem);
    }

    char result = g_originalSocketOpenWriter ? g_originalSocketOpenWriter(self) : 0;
    if (hit <= kSocketTraceMaxWriterHits) {
        LogLine("socket-trace writer-return ordinal=%ld result=%d", hit,
            static_cast<int>(static_cast<unsigned char>(result)));
    }
    return result;
}

bool __cdecl ProxySocketOp14ExtensionGate(int templateID)
{
    bool accepted = g_originalSocketOp14ExtensionGate
        ? g_originalSocketOp14ExtensionGate(templateID)
        : false;
    // Do not turn a broad scene-reader hook into a logging hot path.  The
    // passive socket trace already established this selected normal-equipment
    // template; record only its actual current-client gate result.
    if (static_cast<uint32_t>(templateID) == kSocketTraceSelectedTemplateID) {
        LONG hit = InterlockedIncrement(&g_socketTraceExtensionGateCount);
        if (hit <= kSocketTraceMaxExtensionGateHits) {
            LogLine("socket-trace op14-list3-extension-gate ordinal=%ld template_id=%lu accepted=%d",
                hit, static_cast<unsigned long>(static_cast<uint32_t>(templateID)),
                accepted ? 1 : 0);
        }
    }
    return accepted;
}

void LogCodecSetKey(int index, void* self, int keyPtr, int keyLength, int result, void* returnAddress)
{
    char hex[360] = { 0 };
    FormatHexBytes(reinterpret_cast<const void*>(keyPtr), keyLength, hex, sizeof(hex));

    uintptr_t vtable = 0;
    uintptr_t state = 0;
    __try {
        vtable = self ? *reinterpret_cast<uintptr_t*>(self) : 0;
        state = self ? *(reinterpret_cast<uintptr_t*>(self) + 2) : 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        vtable = 0;
        state = 0;
    }

    LogLine("codec-setkey idx=%d self=%p vt=0x%08X state=0x%08X key=%p len=%d ret=0x%08X caller=0x%08X bytes=%s",
        index, self, vtable, state, reinterpret_cast<void*>(keyPtr), keyLength,
        result, reinterpret_cast<uintptr_t>(returnAddress), hex);
}

bool IsTclsTrimSpace(wchar_t value)
{
    return value == L' ' || value == L'\t' || value == L'\r' || value == L'\n';
}

bool WideSliceEquals(const wchar_t* text, size_t length, const wchar_t* expected)
{
    if (!text || !expected) return false;
    size_t expectedLength = wcslen(expected);
    return length == expectedLength && wmemcmp(text, expected, length) == 0;
}

// The launch parser owns a vector<wchar_t*> at +0xC4/+0xC8.  Its entries are
// direct NUL-terminated strings.  The fetch output objects are a different
// type and are written only through the current EXE's native assign method.
bool TryReadTclsSlot(unsigned int slot, const wchar_t** text, size_t* length)
{
    if (!text || !length) return false;
    *text = nullptr;
    *length = 0;

    __try {
        unsigned char* parser = static_cast<unsigned char*>(g_currentTclsParser);
        if (!parser) return false;

        void** begin = *reinterpret_cast<void***>(parser + 0xC4);
        void** end = *reinterpret_cast<void***>(parser + 0xC8);
        if (!begin || !end || end < begin) return false;

        size_t count = static_cast<size_t>(end - begin);
        if (count == 0 || count > 64 || slot >= count) return false;

        const wchar_t* valueText = reinterpret_cast<const wchar_t*>(begin[slot]);
        if (!valueText) return false;

        size_t valueLength = 0;
        while (valueLength < 4096 && valueText[valueLength] != L'\0') ++valueLength;
        if (valueLength == 0 || valueLength == 4096) return false;

        size_t first = 0;
        size_t last = valueLength;
        while (first < last && IsTclsTrimSpace(valueText[first])) ++first;
        while (last > first && IsTclsTrimSpace(valueText[last - 1])) --last;
        if (last == first) return false;

        valueText += first;
        valueLength = last - first;
        if (WideSliceEquals(valueText, valueLength, L"<bad-wide>") ||
            WideSliceEquals(valueText, valueLength, L"<null>")) {
            return false;
        }

        *text = valueText;
        *length = valueLength;
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

bool IsDirectTclsLaunch()
{
    const wchar_t* marker = nullptr;
    size_t markerLength = 0;
    return TryReadTclsSlot(0, &marker, &markerLength) &&
        WideSliceEquals(marker, markerLength, L"99");
}

bool AssignTclsSlot(void* output, unsigned int slot)
{
    if (!output || !g_gameWideAssign || !IsDirectTclsLaunch()) return false;

    const wchar_t* value = nullptr;
    size_t valueLength = 0;
    if (!TryReadTclsSlot(slot, &value, &valueLength)) return false;

    __try {
        g_gameWideAssign(output, value, valueLength);
        LogLine("tcls native fallback slot=%u len=%u", slot, static_cast<unsigned int>(valueLength));
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("tcls native fallback exception slot=%u code=0x%08X", slot, GetExceptionCode());
        return false;
    }
}

size_t ReadGameWideLength(void* value)
{
    if (!value) return 0;
    __try {
        return *reinterpret_cast<unsigned int*>(static_cast<unsigned char*>(value) + 0x14);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return 0;
    }
}

bool __fastcall ProxyTclsParse(void* self, void* /*unused*/, const wchar_t* text)
{
    g_currentTclsParser = self;
    bool result = g_originalTclsParse ? g_originalTclsParse(self, text) : false;
    g_currentTclsParser = nullptr;
    LogLine("tcls parse result=%d", result ? 1 : 0);
    return result;
}

bool __fastcall ProxyTclsFetchLogin(void* self, void* /*unused*/, void* outIp, void* outPort, void* outChannel)
{
    bool result = g_originalTclsFetchLogin
        ? g_originalTclsFetchLogin(self, outIp, outPort, outChannel)
        : false;
    if (!result) {
        // Match the native compatibility behavior: attempt all three writes
        // even if an earlier field fails, then report aggregate success.
        bool ipAssigned = AssignTclsSlot(outIp, 1);
        bool portAssigned = AssignTclsSlot(outPort, 2);
        bool channelAssigned = AssignTclsSlot(outChannel, 5);
        result = ipAssigned && portAssigned && channelAssigned;
    }
    LogLine("tcls fetch login result=%d", result ? 1 : 0);
    return result;
}

bool __fastcall ProxyTclsFetchText(void* self, void* /*unused*/, int key, void* output)
{
    bool result = g_originalTclsFetchText ? g_originalTclsFetchText(self, key, output) : false;
    if (!result && key == 2) result = AssignTclsSlot(output, 3);
    LogLine("tcls fetch text key=%d result=%d", key, result ? 1 : 0);
    return result;
}

bool __fastcall ProxyTclsFetchCrypto(void* self, void* /*unused*/, void* output)
{
    bool result = g_originalTclsFetchCrypto ? g_originalTclsFetchCrypto(self, output) : false;
    if (!result) result = AssignTclsSlot(output, 4);
    LogLine("tcls fetch crypto result=%d", result ? 1 : 0);
    return result;
}

bool __fastcall ProxyTclsFetchTail(void* self, void* /*unused*/, void* output)
{
    bool result = g_originalTclsFetchTail ? g_originalTclsFetchTail(self, output) : false;
    if (!result) result = AssignTclsSlot(output, 15);
    LogLine("tcls fetch tail result=%d", result ? 1 : 0);
    return result;
}

bool __stdcall ProxyTclsTail(void* output)
{
    bool result = g_originalTclsTail ? g_originalTclsTail(output) : false;
    if (!result || ReadGameWideLength(output) == 0) {
        if (AssignTclsSlot(output, 12)) result = true;
    }
    LogLine("tcls tail result=%d len=%u", result ? 1 : 0,
        static_cast<unsigned int>(ReadGameWideLength(output)));
    return result;
}

int __fastcall ProxyCodecSetKey0(void* self, void* /*unused*/, int keyPtr, int keyLength)
{
    int result = g_originalCodecSetKey0
        ? g_originalCodecSetKey0(self, keyPtr, keyLength)
        : 1879048192;
    LogCodecSetKey(0, self, keyPtr, keyLength, result, _ReturnAddress());
    return result;
}

int __fastcall ProxyCodecSetKey2(void* self, void* /*unused*/, int keyPtr, int keyLength)
{
    int result = g_originalCodecSetKey2
        ? g_originalCodecSetKey2(self, keyPtr, keyLength)
        : 1879048192;
    LogCodecSetKey(2, self, keyPtr, keyLength, result, _ReturnAddress());
    return result;
}

int __fastcall ProxyCodecSetKey3(void* self, void* /*unused*/, int keyPtr, int keyLength)
{
    int result = g_originalCodecSetKey3
        ? g_originalCodecSetKey3(self, keyPtr, keyLength)
        : 1879048192;
    LogCodecSetKey(3, self, keyPtr, keyLength, result, _ReturnAddress());
    return result;
}

int __fastcall ProxyCodecSetKey7(void* self, void* /*unused*/, int keyPtr, int keyLength)
{
    int result = g_originalCodecSetKey7
        ? g_originalCodecSetKey7(self, keyPtr, keyLength)
        : 1879048192;
    LogCodecSetKey(7, self, keyPtr, keyLength, result, _ReturnAddress());
    return result;
}

int __fastcall ProxyCodecSetKey8(void* self, void* /*unused*/, int keyPtr, int keyLength)
{
    int result = g_originalCodecSetKey8
        ? g_originalCodecSetKey8(self, keyPtr, keyLength)
        : 1879048192;
    LogCodecSetKey(8, self, keyPtr, keyLength, result, _ReturnAddress());
    return result;
}

void* __fastcall ProxySkillCatalogCollect(
    void* self, void* /*unused*/, NativePointerVector* output, int selector)
{
    // Capture this before invoking the trampoline: after the call it would no
    // longer identify which native catalog consumer requested the list.
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    SkillCatalogCollectFn original =
        reinterpret_cast<SkillCatalogCollectFn>(g_originalSkillCatalogCollect);
    void* result = original ? original(self, output, selector) : nullptr;

    uintptr_t callerRva = caller >= g_dnfBase ? caller - g_dnfBase : 0;
    if (callerRva != kSkillWindowCollectReturnARva &&
        callerRva != kSkillWindowCollectReturnBRva) {
        return result;
    }

    // The same collector serves non-skill-window systems.  Only the two
    // sub_1FE6D00 call sites above are filtered, and every malformed or
    // unavailable native state fails open to the original list.
    void* actor = nullptr;
    void** begin = nullptr;
    void** end = nullptr;
    size_t count = 0;
    bool snapshotValid = false;
    __try {
        actor = *reinterpret_cast<void**>(g_dnfBase + kControlledActorPointerRva);
        if (output) {
            begin = output->begin;
            end = output->end;
            uintptr_t beginAddress = reinterpret_cast<uintptr_t>(begin);
            uintptr_t endAddress = reinterpret_cast<uintptr_t>(end);
            if (beginAddress && endAddress >= beginAddress &&
                (endAddress - beginAddress) % sizeof(void*) == 0) {
                count = (endAddress - beginAddress) / sizeof(void*);
                snapshotValid = count <= 4096;
            } else if (!beginAddress && !endAddress) {
                snapshotValid = true;
            }
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        snapshotValid = false;
    }
    if (!snapshotValid || !actor || count == 0) return result;

    void** kept = static_cast<void**>(HeapAlloc(
        GetProcessHeap(), 0, count * sizeof(void*)));
    if (!kept) return result;

    SkillFitnessPredicateFn fitness =
        reinterpret_cast<SkillFitnessPredicateFn>(
            g_dnfBase + kSkillFitnessPredicateRva);
    size_t keptCount = 0;
    bool filterCompleted = false;
    __try {
        for (size_t i = 0; i < count; ++i) {
            void* skill = begin[i];
            bool keep = true;
            if (skill) {
                uintptr_t fitnessBegin = *reinterpret_cast<uintptr_t*>(
                    static_cast<unsigned char*>(skill) +
                    kSkillFitnessGrowTypeBeginOffset);
                uintptr_t fitnessEnd = *reinterpret_cast<uintptr_t*>(
                    static_cast<unsigned char*>(skill) +
                    kSkillFitnessGrowTypeEndOffset);

                // An empty first-grow-type vector marks a common skill.  A
                // non-empty, structurally valid vector is delegated to the
                // EXE's own grow-type/awakening predicate.
                if (fitnessBegin != fitnessEnd) {
                    bool validFitnessVector =
                        fitnessBegin != 0 && fitnessEnd > fitnessBegin &&
                        (fitnessEnd - fitnessBegin) % 8 == 0 &&
                        (fitnessEnd - fitnessBegin) / 8 <= 64;
                    if (validFitnessVector) {
                        keep = fitness(skill, actor) != 0;
                    }
                }
            }
            if (keep) kept[keptCount++] = skill;
        }
        if (keptCount != count) {
            memcpy(begin, kept, keptCount * sizeof(void*));
            output->end = begin + keptCount;
        }
        filterCompleted = true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        filterCompleted = false;
    }

    HeapFree(GetProcessHeap(), 0, kept);
    if (filterCompleted) {
        LogLine("skill-tree profession visibility caller_rva=0x%08X selector=%d before=%u after=%u filtered=%u",
            callerRva, selector, static_cast<unsigned int>(count),
            static_cast<unsigned int>(keptCount),
            static_cast<unsigned int>(count - keptCount));
    } else {
        LogLine("skill-tree profession visibility failed open caller_rva=0x%08X selector=%d count=%u",
            callerRva, selector, static_cast<unsigned int>(count));
    }
    return result;
}

int AddedSkillQuickSlotAtCursor(int displayIndex);

int __stdcall ProxySkillQuickSlotDisplayMap(int displayIndex)
{
    SkillQuickSlotDisplayMapFn original =
        reinterpret_cast<SkillQuickSlotDisplayMapFn>(
            g_originalSkillQuickSlotDisplayMap);
    if (!original) return -1;

    // The stock mouse-selection manager owns only six controls per row, so
    // it cannot manufacture display indices 6/13 for the appended controls.
    // Resolve the two genuine native control rectangles before translating
    // the manager's legacy index. This makes the cells valid drag sources and
    // destinations without changing any of the original twelve mappings.
    int addedSlot = AddedSkillQuickSlotAtCursor(displayIndex);
    if (addedSlot >= 0) return addedSlot;

    // Existing 6-column clients number the extension row 0..5 and the normal
    // row 6..11. The widened frame is row-major 7 columns, so insert one new
    // extension cell at display 6 and one new normal cell at display 13.
    if (displayIndex < 6) return original(displayIndex);

    // Creator mode and characters without an enabled extension bar use a
    // different native mapping. Preserve it exactly instead of manufacturing
    // indices that their UI does not expose.
    if (original(5) != 203) return original(displayIndex);

    if (displayIndex == 6) return kSkillQuickSlotExtension7;
    if (displayIndex >= 7 && displayIndex <= 12) {
        return original(displayIndex - 1);
    }
    if (displayIndex == 13) return kSkillQuickSlotNormal7;
    return original(displayIndex);
}

unsigned char __stdcall ProxySkillQuickSlotPredicate(int slot)
{
    if (slot == kSkillQuickSlotNormal7 ||
        slot == kSkillQuickSlotExtension7) {
        return 1;
    }
    SkillQuickSlotPredicateFn original =
        reinterpret_cast<SkillQuickSlotPredicateFn>(
            g_originalSkillQuickSlotPredicate);
    return original ? original(slot) : 0;
}

bool IsSkillQuickSlotReadable(const void* address, size_t length)
{
    if (!address || length == 0) return false;
    MEMORY_BASIC_INFORMATION region = {};
    if (!VirtualQuery(address, &region, sizeof(region)) ||
        region.State != MEM_COMMIT ||
        (region.Protect & (PAGE_GUARD | PAGE_NOACCESS)) != 0) {
        return false;
    }
    uintptr_t start = reinterpret_cast<uintptr_t>(address);
    uintptr_t end = start + length;
    uintptr_t regionEnd = reinterpret_cast<uintptr_t>(region.BaseAddress) +
        region.RegionSize;
    return end >= start && end <= regionEnd;
}

unsigned short SkillQuickSlotDefaultKey(int action)
{
    if (action == kSkillQuickSlotNormal7Action) {
        return kSkillQuickSlotNormal7DefaultKey;
    }
    if (action == kSkillQuickSlotExtension7Action) {
        return kSkillQuickSlotExtension7DefaultKey;
    }
    return 0;
}

unsigned int EnsureSkillQuickSlotDefaultKey(void* catalog, int action)
{
    unsigned short defaultKey = SkillQuickSlotDefaultKey(action);
    if (!catalog || !defaultKey) return kSkillQuickSlotInvalidKeyCode;

    unsigned char* entryAddress = static_cast<unsigned char*>(catalog) +
        kSkillQuickSlotKeyTableOffset +
        static_cast<size_t>(action) * sizeof(unsigned short);
    if (!IsSkillQuickSlotReadable(entryAddress, sizeof(unsigned short))) {
        return kSkillQuickSlotInvalidKeyCode;
    }

    unsigned short keyCode = kSkillQuickSlotInvalidKeyCode;
    __try {
        keyCode = *reinterpret_cast<unsigned short*>(entryAddress);
        if (keyCode == 0 || keyCode >= kSkillQuickSlotInvalidKeyCode) {
            *reinterpret_cast<unsigned short*>(entryAddress) = defaultKey;
            keyCode = defaultKey;
            LONG mask = action == kSkillQuickSlotNormal7Action ? 1 : 2;
            LONG previous = InterlockedOr(
                &g_skillQuickSlotDefaultKeyLogMask, mask);
            if ((previous & mask) == 0) {
                LogLine("skill quick-slot 7 default key registered action=%d key_code=%u",
                    action, static_cast<unsigned int>(keyCode));
            }
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return kSkillQuickSlotInvalidKeyCode;
    }
    return keyCode;
}

unsigned int __fastcall ProxySkillQuickSlotKeyCode(
    void* self, void*, int action)
{
    SkillQuickSlotKeyCodeFn original =
        reinterpret_cast<SkillQuickSlotKeyCodeFn>(
            g_originalSkillQuickSlotKeyCode);
    unsigned int keyCode = original ? original(self, action) :
        kSkillQuickSlotInvalidKeyCode;
    if ((action == kSkillQuickSlotNormal7Action ||
            action == kSkillQuickSlotExtension7Action) &&
        (keyCode == 0 || keyCode >= kSkillQuickSlotInvalidKeyCode)) {
        return EnsureSkillQuickSlotDefaultKey(self, action);
    }
    return keyCode;
}

void* SkillQuickSlotControl(void* hud, size_t controlsOffset, size_t column)
{
    if (!hud || column >= kSkillQuickSlotTotalColumns) return nullptr;
    size_t storageOffset = 0;
    if (column < kSkillQuickSlotNativeColumns) {
        storageOffset = controlsOffset +
            column * kSkillQuickSlotControlStride;
    }
    else if (controlsOffset == kSkillQuickSlotNormalControlsOffset) {
        storageOffset = kSkillQuickSlotAddedNormalControlOffset;
    }
    else if (controlsOffset == kSkillQuickSlotExtensionControlsOffset) {
        storageOffset = kSkillQuickSlotAddedExtensionControlOffset;
    }
    else {
        return nullptr;
    }
    unsigned char* slot = static_cast<unsigned char*>(hud) + storageOffset;
    if (!IsSkillQuickSlotReadable(slot, sizeof(void*))) return nullptr;
    return *reinterpret_cast<void**>(slot);
}

bool ReadSkillQuickSlotControlPosition(void* control, int* x, int* y);

bool AcquireNativeSharedControl(const NativeSharedControl& control)
{
    if (!control.owner) return true;
    volatile LONG* strong = reinterpret_cast<volatile LONG*>(
        static_cast<unsigned char*>(control.owner) + sizeof(void*));
    LONG observed = *strong;
    while (observed > 0) {
        LONG previous = InterlockedCompareExchange(
            strong, observed + 1, observed);
        if (previous == observed) return true;
        observed = previous;
    }
    return false;
}

void ReleaseNativeSharedControl(NativeSharedControl* control)
{
    if (!control || (!control->object && !control->owner)) return;
    SkillQuickSlotReleaseSharedFn release =
        reinterpret_cast<SkillQuickSlotReleaseSharedFn>(
            g_dnfBase + kReleaseSharedHandleRva);
    release(control);
}

bool AttachNativeSkillQuickSlotControl(
    void* hud, NativeSharedControl* control)
{
    if (!hud || !control || !control->object || !control->owner) return false;

    NativeSharedControl parent = {};
    NativeSharedControl childArgument = {};
    bool childArgumentAcquired = false;
    bool attached = false;
    __try {
        SkillQuickSlotHudParentFn getParent =
            reinterpret_cast<SkillQuickSlotHudParentFn>(
                g_dnfBase + kSkillQuickSlotHudParentRva);
        getParent(hud, &parent);
        if (!parent.object || !parent.owner) __leave;

        childArgument = *control;
        childArgumentAcquired = AcquireNativeSharedControl(childArgument);
        if (!childArgumentAcquired) __leave;

        void** vtable = *reinterpret_cast<void***>(parent.object);
        SkillQuickSlotAttachChildFn attach =
            reinterpret_cast<SkillQuickSlotAttachChildFn>(
                vtable[0x40 / sizeof(void*)]);
        if (!attach) __leave;
        attach(parent.object, childArgument);
        // The native vtable method receives the shared_ptr by value and owns
        // destruction of that argument, exactly like sub_1F7BCF0. Do not
        // release the copied argument a second time after a successful call.
        childArgumentAcquired = false;
        childArgument.object = nullptr;
        childArgument.owner = nullptr;
        attached = true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        attached = false;
    }

    if (childArgumentAcquired) {
        __try {
            ReleaseNativeSharedControl(&childArgument);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
        }
    }
    __try {
        ReleaseNativeSharedControl(&parent);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
    }
    return attached;
}

bool CreateNativeSkillQuickSlotControl(
    void* hud, size_t storageOffset, int x, int y, bool normal)
{
    if (!hud) return false;
    NativeSharedControl* storage = reinterpret_cast<NativeSharedControl*>(
        static_cast<unsigned char*>(hud) + storageOffset);
    if (!IsSkillQuickSlotReadable(storage, sizeof(*storage))) return false;
    if (storage->object || storage->owner) {
        LogLine("skill quick-slot 7 native storage already occupied offset=0x%X object=0x%08X owner=0x%08X",
            static_cast<unsigned int>(storageOffset),
            reinterpret_cast<uintptr_t>(storage->object),
            reinterpret_cast<uintptr_t>(storage->owner));
        return false;
    }

    NativeSharedControl created = {};
    bool installed = false;
    __try {
        SkillQuickSlotControlFactoryFn factory =
            reinterpret_cast<SkillQuickSlotControlFactoryFn>(
                g_dnfBase + kSkillQuickSlotControlFactoryRva);
        factory(&created);
        if (!created.object || !created.owner) __leave;

        // Move the factory result into the HUD-owned, natively constructed
        // shared_ptr slot. The HUD's stock destructor will release it.
        *storage = created;
        created.object = nullptr;
        created.owner = nullptr;

        void** vtable = *reinterpret_cast<void***>(storage->object);
        typedef void (__thiscall* SetControlSizeFn)(void*, int, int);
        SetControlSizeFn setSize = reinterpret_cast<SetControlSizeFn>(
            vtable[0x60 / sizeof(void*)]);
        setSize(storage->object, 28, 28);

        if (normal) {
            SkillQuickSlotControlTypeFn setType =
                reinterpret_cast<SkillQuickSlotControlTypeFn>(
                    g_dnfBase + kSkillQuickSlotControlTypeRva);
            setType(storage->object, 5);
        }

        SkillQuickSlotControlPositionFn setPosition =
            reinterpret_cast<SkillQuickSlotControlPositionFn>(
                g_dnfBase + kSkillQuickSlotControlPositionRva);
        setPosition(storage->object, x, y);

        if (!normal) {
            SkillQuickSlotCatalogFn catalogFn =
                reinterpret_cast<SkillQuickSlotCatalogFn>(
                    g_dnfBase + kSkillQuickSlotCatalogRva);
            SkillQuickSlotExtensionEnabledFn enabled =
                reinterpret_cast<SkillQuickSlotExtensionEnabledFn>(
                    g_dnfBase + kSkillQuickSlotExtensionEnabledRva);
            typedef void (__thiscall* SetControlVisibleFn)(void*, int);
            SetControlVisibleFn setVisible =
                reinterpret_cast<SetControlVisibleFn>(
                    vtable[0x08 / sizeof(void*)]);
            setVisible(storage->object, enabled(catalogFn(), 14) != 0);
        }

        installed = AttachNativeSkillQuickSlotControl(hud, storage);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        installed = false;
    }

    if (!installed) {
        __try {
            ReleaseNativeSharedControl(&created);
            ReleaseNativeSharedControl(storage);
            storage->object = nullptr;
            storage->owner = nullptr;
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
        }
        LogLine("skill quick-slot 7 native control creation failed offset=0x%X x=%d y=%d",
            static_cast<unsigned int>(storageOffset), x, y);
        return false;
    }

    int absoluteX = 0;
    int absoluteY = 0;
    ReadSkillQuickSlotControlPosition(
        storage->object, &absoluteX, &absoluteY);
    LogLine("skill quick-slot 7 native control attached row=%s offset=0x%X object=0x%08X local=(%d,%d) absolute=(%d,%d)",
        normal ? "normal" : "extension",
        static_cast<unsigned int>(storageOffset),
        reinterpret_cast<uintptr_t>(storage->object), x, y,
        absoluteX, absoluteY);
    return true;
}

void* __fastcall ProxySkillQuickSlotHudConstruct(
    void* self, void*, int context)
{
    SkillQuickSlotHudConstructFn original =
        reinterpret_cast<SkillQuickSlotHudConstructFn>(
            g_originalSkillQuickSlotHudConstruct);
    void* result = original ? original(self, context) : self;
    InterlockedExchangePointer(&g_skillQuickSlotHud, self);
    __try {
        SkillQuickSlotCatalogFn catalogFn =
            reinterpret_cast<SkillQuickSlotCatalogFn>(
                g_dnfBase + kSkillQuickSlotCatalogRva);
        void* catalog = catalogFn();
        EnsureSkillQuickSlotDefaultKey(
            catalog, kSkillQuickSlotNormal7Action);
        EnsureSkillQuickSlotDefaultKey(
            catalog, kSkillQuickSlotExtension7Action);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
    }
    CreateNativeSkillQuickSlotControl(self,
        kSkillQuickSlotAddedNormalControlOffset,
        kSkillQuickSlotAddedControlX, kSkillQuickSlotNormalControlY, true);
    CreateNativeSkillQuickSlotControl(self,
        kSkillQuickSlotAddedExtensionControlOffset,
        kSkillQuickSlotAddedControlX, kSkillQuickSlotExtensionControlY, false);
    return result;
}

bool ReadSkillQuickSlotControlPosition(void* control, int* x, int* y)
{
    if (!control || !x || !y ||
        !IsSkillQuickSlotReadable(control,
            kSkillQuickSlotControlYOffset + sizeof(int))) {
        return false;
    }
    unsigned char* bytes = static_cast<unsigned char*>(control);
    if (!bytes[kSkillQuickSlotControlVisibleOffset]) return false;
    *x = *reinterpret_cast<int*>(bytes + kSkillQuickSlotControlXOffset);
    *y = *reinterpret_cast<int*>(bytes + kSkillQuickSlotControlYOffset);
    return true;
}

bool ReadSkillQuickSlotMousePosition(int* x, int* y)
{
    if (!x || !y || !g_dnfBase) return false;
    SkillQuickSlotMouseCoordinateFn mouseXFn =
        reinterpret_cast<SkillQuickSlotMouseCoordinateFn>(
            g_dnfBase + kSkillQuickSlotMouseXRva);
    SkillQuickSlotMouseCoordinateFn mouseYFn =
        reinterpret_cast<SkillQuickSlotMouseCoordinateFn>(
            g_dnfBase + kSkillQuickSlotMouseYRva);
    __try {
        *x = mouseXFn(0);
        *y = mouseYFn(0);
        return *x >= 0 && *y >= 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        *x = 0;
        *y = 0;
        return false;
    }
}

int SkillQuickSlotAtCursor()
{
    void* hud = InterlockedCompareExchangePointer(
        &g_skillQuickSlotHud, nullptr, nullptr);
    if (!hud) return -1;

    int mouseX = 0;
    int mouseY = 0;
    if (!ReadSkillQuickSlotMousePosition(&mouseX, &mouseY)) return -1;

    const struct {
        size_t controlsOffset;
        int stockBase;
        int addedSlot;
    } rows[] = {
        { kSkillQuickSlotExtensionControlsOffset,
          kSkillQuickSlotExtensionBase, kSkillQuickSlotExtension7 },
        { kSkillQuickSlotNormalControlsOffset,
          kSkillQuickSlotNormalBase, kSkillQuickSlotNormal7 },
    };
    for (size_t row = 0; row < sizeof(rows) / sizeof(rows[0]); ++row) {
        for (size_t column = 0;
             column < kSkillQuickSlotTotalColumns; ++column) {
            void* control = SkillQuickSlotControl(
                hud, rows[row].controlsOffset, column);
            int x = 0;
            int y = 0;
            if (!ReadSkillQuickSlotControlPosition(control, &x, &y)) {
                continue;
            }
            if (mouseX < x ||
                mouseX >= x + kSkillQuickSlotControlSize ||
                mouseY < y ||
                mouseY >= y + kSkillQuickSlotControlSize) {
                continue;
            }
            return column < kSkillQuickSlotNativeColumns ?
                rows[row].stockBase + static_cast<int>(column) :
                rows[row].addedSlot;
        }
    }
    return -1;
}

bool IsAddedSkillQuickSlot(int slot)
{
    return slot == kSkillQuickSlotNormal7 ||
        slot == kSkillQuickSlotExtension7;
}

void BeginSkillQuickSlotMouseDrag()
{
    int source = SkillQuickSlotAtCursor();
    void* manager = g_skillQuickSlotLookupManager;
    SkillQuickSlotLookupFn lookup =
        reinterpret_cast<SkillQuickSlotLookupFn>(
            g_originalSkillQuickSlotLookup);
    bool hasSkill = false;
    if (source >= 0 && manager && lookup) {
        __try {
            hasSkill = lookup(manager, source) != nullptr;
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            hasSkill = false;
        }
    }
    InterlockedExchange(&g_skillQuickSlotMouseDragSource,
        hasSkill ? source : -1);
    if (hasSkill && IsAddedSkillQuickSlot(source)) {
        LogLine("skill quick-slot 7 mouse drag begin source=%d manager=0x%08X",
            source, reinterpret_cast<uintptr_t>(manager));
    }
}

void CompleteSkillQuickSlotMouseDrag()
{
    int source = static_cast<int>(InterlockedExchange(
        &g_skillQuickSlotMouseDragSource, -1));
    if (source < 0) return;
    int destination = SkillQuickSlotAtCursor();
    if (destination < 0 || destination == source ||
        (!IsAddedSkillQuickSlot(source) &&
            !IsAddedSkillQuickSlot(destination))) {
        return;
    }

    void* manager = g_skillQuickSlotLookupManager;
    SkillQuickSlotMoveFn move = reinterpret_cast<SkillQuickSlotMoveFn>(
        g_dnfBase + kSkillQuickSlotMoveRva);
    bool sent = false;
    if (manager && move) {
        __try {
            sent = move(manager, source, destination,
                kSkillQuickSlotDragKind, 1) != 0;
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            sent = false;
        }
    }
    LogLine("skill quick-slot 7 mouse drag complete source=%d destination=%d sent=%d manager=0x%08X",
        source, destination, sent ? 1 : 0,
        reinterpret_cast<uintptr_t>(manager));
}

int AddedSkillQuickSlotAtCursor(int displayIndex)
{
    void* hud = InterlockedCompareExchangePointer(
        &g_skillQuickSlotHud, nullptr, nullptr);
    if (!hud || !g_dnfBase) return -1;

    SkillQuickSlotMouseCoordinateFn mouseXFn =
        reinterpret_cast<SkillQuickSlotMouseCoordinateFn>(
            g_dnfBase + kSkillQuickSlotMouseXRva);
    SkillQuickSlotMouseCoordinateFn mouseYFn =
        reinterpret_cast<SkillQuickSlotMouseCoordinateFn>(
            g_dnfBase + kSkillQuickSlotMouseYRva);
    SkillQuickSlotControlActiveFn activeFn =
        reinterpret_cast<SkillQuickSlotControlActiveFn>(
            g_dnfBase + kSkillQuickSlotControlActiveRva);
    HotkeyActionPressedFn pressedFn =
        reinterpret_cast<HotkeyActionPressedFn>(
            g_dnfBase + kHotkeyActionPressedRva);

    int mouseX = 0;
    int mouseY = 0;
    bool mouseValid = false;
    __try {
        mouseX = mouseXFn(0);
        mouseY = mouseYFn(0);
        mouseValid = mouseX >= 0 && mouseY >= 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        mouseValid = false;
    }

    const struct {
        size_t controlsOffset;
        int slot;
        LONG logMask;
    } addedControls[] = {
        { kSkillQuickSlotExtensionControlsOffset,
          kSkillQuickSlotExtension7, 2 },
        { kSkillQuickSlotNormalControlsOffset,
          kSkillQuickSlotNormal7, 1 },
    };
    for (size_t i = 0;
         i < sizeof(addedControls) / sizeof(addedControls[0]); ++i) {
        void* control = SkillQuickSlotControl(
            hud, addedControls[i].controlsOffset,
            kSkillQuickSlotTotalColumns - 1);
        int x = 0;
        int y = 0;
        if (!ReadSkillQuickSlotControlPosition(control, &x, &y)) continue;
        bool nativeHit = false;
        __try {
            nativeHit = activeFn(control) || pressedFn(control);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            nativeHit = false;
        }
        bool coordinateHit = mouseValid &&
            mouseX >= x && mouseX < x + kSkillQuickSlotControlSize &&
            mouseY >= y && mouseY < y + kSkillQuickSlotControlSize;
        if (!nativeHit && !coordinateHit) {
            continue;
        }
        LONG previous = InterlockedOr(
            &g_skillQuickSlotDragLogMask, addedControls[i].logMask);
        if ((previous & addedControls[i].logMask) == 0) {
            LogLine("skill quick-slot 7 drag hit slot=%d native=%d cursor=(%d,%d) rect=(%d,%d,%d,%d) legacy_display=%d",
                addedControls[i].slot, nativeHit ? 1 : 0,
                mouseX, mouseY, x, y,
                x + kSkillQuickSlotControlSize,
                y + kSkillQuickSlotControlSize, displayIndex);
        }
        return addedControls[i].slot;
    }
    return -1;
}

void DrawAddedSkillQuickSlotIcon(
    void* hud, void* manager, int slot, size_t controlsOffset, LONG mask)
{
    if (!hud || !manager) return;
    void* control = SkillQuickSlotControl(
        hud, controlsOffset, kSkillQuickSlotTotalColumns - 1);
    int x = 0;
    int y = 0;
    if (!ReadSkillQuickSlotControlPosition(control, &x, &y)) return;

    SkillQuickSlotLookupFn lookup = reinterpret_cast<SkillQuickSlotLookupFn>(
        g_originalSkillQuickSlotLookup);
    SkillQuickSlotSetActorFn setActor =
        reinterpret_cast<SkillQuickSlotSetActorFn>(
            g_dnfBase + kSkillQuickSlotSetActorRva);
    SkillQuickSlotGameTimeFn gameTime =
        reinterpret_cast<SkillQuickSlotGameTimeFn>(
            g_dnfBase + kSkillQuickSlotGameTimeRva);
    SkillQuickSlotAvailableFn available =
        reinterpret_cast<SkillQuickSlotAvailableFn>(
            g_dnfBase + kSkillQuickSlotAvailableRva);
    SkillQuickSlotSetAlphaFn setAlpha =
        reinterpret_cast<SkillQuickSlotSetAlphaFn>(
            g_dnfBase + kSkillQuickSlotSetAlphaRva);
    SkillQuickSlotDrawIconFn drawIcon =
        reinterpret_cast<SkillQuickSlotDrawIconFn>(
            g_dnfBase + kSkillQuickSlotDrawIconRva);

    __try {
        void* actor = *reinterpret_cast<void**>(
            g_dnfBase + kControlledActorPointerRva);
        void* skill = lookup ? lookup(manager, slot) : nullptr;
        if (!actor || !skill) return;
        setActor(skill, actor);
        int tick = static_cast<int>(gameTime());
        int disabled = available(skill, actor, tick) ? 0 : 1;
        int alpha = *reinterpret_cast<int*>(
            static_cast<unsigned char*>(hud) +
            kSkillQuickSlotHudAlphaOffset);
        setAlpha(skill, alpha);
        drawIcon(skill, actor, x, y,
            -1, disabled, 0, 1, 1, 0, 0);
        int defaultAlpha = *reinterpret_cast<int*>(
            g_dnfBase + kSkillQuickSlotDefaultAlphaRva);
        setAlpha(skill, defaultAlpha);
        LONG previous = InterlockedOr(&g_skillQuickSlotIconDrawMask, mask);
        if ((previous & mask) == 0) {
            LogLine("skill quick-slot 7 icon draw active slot=%d x=%d y=%d",
                slot, x, y);
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
    }
}

void DrawAddedSkillQuickSlotKey(
    void* hud, unsigned char action, size_t controlsOffset, LONG mask)
{
    if (!hud) return;
    void* control = SkillQuickSlotControl(
        hud, controlsOffset, kSkillQuickSlotTotalColumns - 1);
    int x = 0;
    int y = 0;
    if (!ReadSkillQuickSlotControlPosition(control, &x, &y)) return;

    SkillQuickSlotCatalogFn catalogFn =
        reinterpret_cast<SkillQuickSlotCatalogFn>(
            g_dnfBase + kSkillQuickSlotCatalogRva);
    SkillQuickSlotKeyCodeFn keyCodeFn =
        reinterpret_cast<SkillQuickSlotKeyCodeFn>(
            g_dnfBase + kSkillQuickSlotKeyCodeRva);
    SkillQuickSlotTextBuildFn textBuild =
        reinterpret_cast<SkillQuickSlotTextBuildFn>(
            g_dnfBase + kSkillQuickSlotTextBuildRva);
    SkillQuickSlotTextDrawFn textDraw =
        reinterpret_cast<SkillQuickSlotTextDrawFn>(
            g_dnfBase + kSkillQuickSlotTextDrawRva);
    ReleaseSharedHandleFn release = reinterpret_cast<ReleaseSharedHandleFn>(
        g_dnfBase + kReleaseSharedHandleRva);

    unsigned int handle[2] = {};
    bool consumed = false;
    __try {
        void* catalog = catalogFn();
        unsigned int keyCode = keyCodeFn(catalog, action);
        if (keyCode > 0x6A) return;
        void* textManager = *reinterpret_cast<void**>(
            static_cast<unsigned char*>(hud) +
            kSkillQuickSlotHudTextManagerOffset);
        void* renderer = *reinterpret_cast<void**>(
            g_dnfBase + kSkillQuickSlotTextRendererRva);
        if (!textManager || !renderer) return;
        textBuild(textManager, handle, keyCode);
        unsigned int color =
            (static_cast<unsigned int>(*reinterpret_cast<unsigned char*>(
                static_cast<unsigned char*>(hud) +
                kSkillQuickSlotHudAlphaOffset)) << 24) |
            0x00FFFFFF;
        textDraw(renderer, x, y,
            handle[0], handle[1], 0, color);
        consumed = true;
        LONG previous = InterlockedOr(&g_skillQuickSlotKeyDrawMask, mask);
        if ((previous & mask) == 0) {
            LogLine("skill quick-slot 7 key draw active action=%u key_code=%u x=%d y=%d",
                action, keyCode, x, y);
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
    }
    if (!consumed && handle[1]) {
        __try {
            release(handle);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
        }
    }
}

void* __fastcall ProxySkillQuickSlotLookup(
    void* self, void*, int slot)
{
    if (InterlockedCompareExchange(
            &g_skillQuickSlotLookupCapture, 0, 0) != 0) {
        g_skillQuickSlotLookupManager = self;
    }
    SkillQuickSlotLookupFn original =
        reinterpret_cast<SkillQuickSlotLookupFn>(
            g_originalSkillQuickSlotLookup);
    return original ? original(self, slot) : nullptr;
}

int __fastcall ProxySkillQuickSlotNormalRender(void* self, void*)
{
    g_skillQuickSlotLookupManager = nullptr;
    InterlockedExchange(&g_skillQuickSlotLookupCapture, 1);
    SkillQuickSlotRenderFn original =
        reinterpret_cast<SkillQuickSlotRenderFn>(
            g_originalSkillQuickSlotNormalRender);
    int result = original ? original(self) : 0;
    InterlockedExchange(&g_skillQuickSlotLookupCapture, 0);
    void* manager = g_skillQuickSlotLookupManager;
    DrawAddedSkillQuickSlotIcon(self, manager,
        kSkillQuickSlotNormal7, kSkillQuickSlotNormalControlsOffset, 1);
    return result;
}

int __fastcall ProxySkillQuickSlotExtensionRender(void* self, void*)
{
    g_skillQuickSlotLookupManager = nullptr;
    InterlockedExchange(&g_skillQuickSlotLookupCapture, 1);
    SkillQuickSlotRenderFn original =
        reinterpret_cast<SkillQuickSlotRenderFn>(
            g_originalSkillQuickSlotExtensionRender);
    int result = original ? original(self) : 0;
    InterlockedExchange(&g_skillQuickSlotLookupCapture, 0);
    void* manager = g_skillQuickSlotLookupManager;
    DrawAddedSkillQuickSlotIcon(self, manager,
        kSkillQuickSlotExtension7, kSkillQuickSlotExtensionControlsOffset, 2);
    return result;
}

unsigned int __fastcall ProxySkillQuickSlotKeyRender(void* self, void*)
{
    SkillQuickSlotKeyRenderFn original =
        reinterpret_cast<SkillQuickSlotKeyRenderFn>(
            g_originalSkillQuickSlotKeyRender);
    unsigned int result = original ? original(self) : 0;
    DrawAddedSkillQuickSlotKey(self, kSkillQuickSlotNormal7Action,
        kSkillQuickSlotNormalControlsOffset, 1);

    SkillQuickSlotCatalogFn catalogFn =
        reinterpret_cast<SkillQuickSlotCatalogFn>(
            g_dnfBase + kSkillQuickSlotCatalogRva);
    SkillQuickSlotExtensionEnabledFn enabled =
        reinterpret_cast<SkillQuickSlotExtensionEnabledFn>(
            g_dnfBase + kSkillQuickSlotExtensionEnabledRva);
    __try {
        void* catalog = catalogFn();
        if (enabled(catalog, 14)) {
            DrawAddedSkillQuickSlotKey(self,
                kSkillQuickSlotExtension7Action,
                kSkillQuickSlotExtensionControlsOffset, 2);
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
    }
    return result;
}

unsigned int* __fastcall ProxyResolveHotkeyAction(
    void* self, void*, unsigned int* result, char action)
{
    // sub_264E340 is a __thiscall method even though the old IDA prototype
    // exposed only its two stack arguments. Remember the live manager so the
    // two appended actions use the same PVF-backed, user-rebindable state as
    // every native shortcut.
    g_hotkeyActionManager = self;
    ResolveHotkeyActionFn original =
        reinterpret_cast<ResolveHotkeyActionFn>(
            g_originalResolveHotkeyAction);
    return original ? original(self, result, action) : result;
}

__declspec(noinline) int __stdcall ResolveAddedSkillQuickSlotHotkey()
{
    ResolveHotkeyActionFn resolve = reinterpret_cast<ResolveHotkeyActionFn>(
        g_originalResolveHotkeyAction);
    void* manager = g_hotkeyActionManager;
    HotkeyActionPressedFn pressed = reinterpret_cast<HotkeyActionPressedFn>(
        g_dnfBase + kHotkeyActionPressedRva);
    ReleaseSharedHandleFn release = reinterpret_cast<ReleaseSharedHandleFn>(
        g_dnfBase + kReleaseSharedHandleRva);

    if (!resolve || !manager || !pressed || !release) return -1;

    const unsigned char actions[2] = {
        kSkillQuickSlotNormal7Action,
        kSkillQuickSlotExtension7Action,
    };
    const int slots[2] = {
        kSkillQuickSlotNormal7,
        kSkillQuickSlotExtension7,
    };

    for (int i = 0; i < 2; ++i) {
        unsigned int handle[2] = {};
        bool isPressed = false;
        __try {
            resolve(manager, handle, static_cast<char>(actions[i]));
            isPressed = handle[0] != 0 &&
                pressed(reinterpret_cast<void*>(handle[0]));
            release(handle);
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            if (handle[1]) {
                __try {
                    release(handle);
                }
                __except (EXCEPTION_EXECUTE_HANDLER) {
                }
            }
            isPressed = false;
        }
        if (isPressed) return slots[i];
    }
    return -1;
}

__declspec(naked) void ProxySkillQuickSlotHotkeyDispatch()
{
    __asm {
        // sub_1F76800 stores the selected native skill slot at [ebp-1Ch].
        // Keep the stock 6+6 scan authoritative and only test the two new PVF
        // actions when it found no legacy key.
        cmp dword ptr [ebp - 1Ch], -1
        jne skill_quick_slot_hotkey_resume
        pushfd
        pushad
        call ResolveAddedSkillQuickSlotHotkey
        cmp eax, -1
        je skill_quick_slot_hotkey_restore
        mov dword ptr [ebp - 1Ch], eax
    skill_quick_slot_hotkey_restore:
        popad
        popfd
    skill_quick_slot_hotkey_resume:
        // Replay the two complete instructions replaced at 0x1F7690E.
        mov edi, dword ptr [ebp - 20h]
        mov ecx, dword ptr [edi + 3Ch]
        jmp dword ptr [g_skillQuickSlotHotkeyResume]
    }
}

bool PatchSkillQuickSlotLimitByte(uintptr_t rva, const char* label)
{
    uintptr_t target = g_dnfBase + rva;
    const unsigned char expected = 0xCC;
    const unsigned char replacement =
        static_cast<unsigned char>(kSkillQuickSlotCount);
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target), &expected, 1)) {
        LogLine("%s native limit mismatch at 0x%08X actual=0x%02X",
            label, target, *reinterpret_cast<unsigned char*>(target));
        return false;
    }
    return WriteCodePatch(target, &replacement, 1, label);
}

bool PatchSkillQuickSlotHudX(
    uintptr_t rva, int expectedX, int replacementX, const char* label)
{
    uintptr_t target = g_dnfBase + rva;
    unsigned char expected[5] = { 0x68, 0, 0, 0, 0 };
    unsigned char replacement[5] = { 0x68, 0, 0, 0, 0 };
    memcpy(expected + 1, &expectedX, sizeof(expectedX));
    memcpy(replacement + 1, &replacementX, sizeof(replacementX));
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
            expected, sizeof(expected))) {
        LogLine("%s native coordinate mismatch at 0x%08X expected_x=%d",
            label, target, expectedX);
        return false;
    }
    return WriteCodePatch(target, replacement, sizeof(replacement), label);
}

bool InstallSkillQuickSlot7Compatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t keyCodeTarget =
        g_dnfBase + kSkillQuickSlotKeyCodeRva;
    static const unsigned char kKeyCodePrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08
    };
    uintptr_t hudConstructTarget =
        g_dnfBase + kSkillQuickSlotHudConstructRva;
    static const unsigned char kHudConstructPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    uintptr_t displayMapTarget =
        g_dnfBase + kSkillQuickSlotDisplayMapRva;
    static const unsigned char kDisplayMapPrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x0D
    };
    uintptr_t predicateTarget =
        g_dnfBase + kSkillQuickSlotPredicateRva;
    static const unsigned char kPredicatePrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08
    };
    uintptr_t resolveHotkeyActionTarget =
        g_dnfBase + kResolveHotkeyActionRva;
    static const unsigned char kResolveHotkeyActionPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    uintptr_t hotkeyTarget =
        g_dnfBase + kSkillQuickSlotHotkeyDispatchRva;
    static const unsigned char kHotkeyExpected[] = {
        0x8B, 0x7D, 0xE0, 0x8B, 0x4F, 0x3C
    };
    uintptr_t lookupTarget =
        g_dnfBase + kSkillQuickSlotLookupRva;
    static const unsigned char kLookupPrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x51, 0x60
    };
    uintptr_t normalRenderTarget =
        g_dnfBase + kSkillQuickSlotNormalRenderRva;
    static const unsigned char kNormalRenderPrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x1C
    };
    uintptr_t extensionRenderTarget =
        g_dnfBase + kSkillQuickSlotExtensionRenderRva;
    static const unsigned char kExtensionRenderPrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x14
    };
    uintptr_t keyRenderTarget =
        g_dnfBase + kSkillQuickSlotKeyRenderRva;
    static const unsigned char kKeyRenderPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    const struct {
        uintptr_t rva;
        const char* label;
    } limitPatches[] = {
        { kSkillExtensionOccupiedLimitByteRva,
          "skill quick-slot occupied upper bound" },
        { kSkillFirstEmptyLimitByteRva,
          "skill quick-slot first-empty upper bound" },
        { kSkillFirstEmptyMemberLimitByteRva,
          "skill quick-slot member first-empty upper bound" },
        { kSkillQuickMembershipLimitByteRva,
          "skill quick-slot membership upper bound" },
        { kSkillQuickRefreshLimitByteRva,
          "skill quick-slot refresh upper bound" },
        { kSkillCollectAllLimitByteRva,
          "skill quick-slot collect-all upper bound" },
        { kSkillSerializeQuickLimitByteRva,
          "skill quick-slot serialization upper bound" },
        { kSkillSerializeTreeSkipLimitByteRva,
          "skill quick-slot tree exclusion upper bound" },
        { kSkillClassChangeLimitByteRva,
          "skill quick-slot class-change upper bound" },
    };
    const struct {
        uintptr_t rva;
        int expectedX;
        int replacementX;
        const char* label;
    } hudCoordinatePatches[] = {
        { kSkillQuickSlotMpBaseXPatchRva, 708,
          708 + kSkillQuickSlotRightHudShift,
          "skill quick-slot 7 MP base alignment" },
        { kSkillQuickSlotMpFillXPatchRva, 705,
          705 + kSkillQuickSlotRightHudShift,
          "skill quick-slot 7 MP fill alignment" },
        { kSkillQuickSlotSpXPatchRva, 752,
          752 + kSkillQuickSlotRightHudShift,
          "skill quick-slot 7 SP alignment" },
    };

    // Validate the whole compatibility unit before changing any executable
    // byte. A single version mismatch must leave the native client untouched.
    if (!BytesMatch(reinterpret_cast<unsigned char*>(keyCodeTarget),
            kKeyCodePrologue, sizeof(kKeyCodePrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(hudConstructTarget),
            kHudConstructPrologue, sizeof(kHudConstructPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(displayMapTarget),
            kDisplayMapPrologue, sizeof(kDisplayMapPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(predicateTarget),
            kPredicatePrologue, sizeof(kPredicatePrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(resolveHotkeyActionTarget),
            kResolveHotkeyActionPrologue,
            sizeof(kResolveHotkeyActionPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(hotkeyTarget),
            kHotkeyExpected, sizeof(kHotkeyExpected)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(lookupTarget),
            kLookupPrologue, sizeof(kLookupPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(normalRenderTarget),
            kNormalRenderPrologue, sizeof(kNormalRenderPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(extensionRenderTarget),
            kExtensionRenderPrologue,
            sizeof(kExtensionRenderPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(keyRenderTarget),
            kKeyRenderPrologue, sizeof(kKeyRenderPrologue))) {
        LogLine("skill quick-slot 7 preflight prologue mismatch key_code=0x%08X construct=0x%08X display=0x%08X predicate=0x%08X resolver=0x%08X dispatch=0x%08X lookup=0x%08X normal=0x%08X extension=0x%08X keys=0x%08X",
            keyCodeTarget, hudConstructTarget, displayMapTarget, predicateTarget,
            resolveHotkeyActionTarget, hotkeyTarget, lookupTarget,
            normalRenderTarget, extensionRenderTarget, keyRenderTarget);
        return false;
    }
    const unsigned char expectedLimit = 0xCC;
    for (size_t i = 0; i < sizeof(limitPatches) / sizeof(limitPatches[0]); ++i) {
        uintptr_t target = g_dnfBase + limitPatches[i].rva;
        if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
                &expectedLimit, 1)) {
            LogLine("%s preflight mismatch at 0x%08X actual=0x%02X",
                limitPatches[i].label, target,
                *reinterpret_cast<unsigned char*>(target));
            return false;
        }
    }
    for (size_t i = 0;
         i < sizeof(hudCoordinatePatches) / sizeof(hudCoordinatePatches[0]);
         ++i) {
        unsigned char expected[5] = { 0x68, 0, 0, 0, 0 };
        memcpy(expected + 1, &hudCoordinatePatches[i].expectedX,
            sizeof(hudCoordinatePatches[i].expectedX));
        uintptr_t target = g_dnfBase + hudCoordinatePatches[i].rva;
        if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
                expected, sizeof(expected))) {
            LogLine("%s preflight mismatch at 0x%08X expected_x=%d",
                hudCoordinatePatches[i].label, target,
                hudCoordinatePatches[i].expectedX);
            return false;
        }
    }

    void* keyCodeOriginal = nullptr;
    if (!InstallKnownInlineHook(keyCodeTarget,
            kKeyCodePrologue, sizeof(kKeyCodePrologue),
            reinterpret_cast<void*>(&ProxySkillQuickSlotKeyCode),
            &keyCodeOriginal,
            "skill quick-slot 7 default key registration")) {
        return false;
    }
    g_originalSkillQuickSlotKeyCode = keyCodeOriginal;

    void* hudConstructOriginal = nullptr;
    if (!InstallKnownInlineHook(hudConstructTarget,
            kHudConstructPrologue, sizeof(kHudConstructPrologue),
            reinterpret_cast<void*>(&ProxySkillQuickSlotHudConstruct),
            &hudConstructOriginal,
            "skill quick-slot 7 native control construction")) {
        return false;
    }
    g_originalSkillQuickSlotHudConstruct = hudConstructOriginal;

    void* displayMapOriginal = nullptr;
    if (!InstallKnownInlineHook(displayMapTarget,
            kDisplayMapPrologue, sizeof(kDisplayMapPrologue),
            reinterpret_cast<void*>(&ProxySkillQuickSlotDisplayMap),
            &displayMapOriginal, "skill quick-slot 7 display mapping")) {
        return false;
    }
    g_originalSkillQuickSlotDisplayMap = displayMapOriginal;

    void* predicateOriginal = nullptr;
    if (!InstallKnownInlineHook(predicateTarget,
            kPredicatePrologue, sizeof(kPredicatePrologue),
            reinterpret_cast<void*>(&ProxySkillQuickSlotPredicate),
            &predicateOriginal, "skill quick-slot 7 predicate")) {
        return false;
    }
    g_originalSkillQuickSlotPredicate = predicateOriginal;

    void* resolveHotkeyActionOriginal = nullptr;
    if (!InstallKnownInlineHook(resolveHotkeyActionTarget,
            kResolveHotkeyActionPrologue,
            sizeof(kResolveHotkeyActionPrologue),
            reinterpret_cast<void*>(&ProxyResolveHotkeyAction),
            &resolveHotkeyActionOriginal,
            "skill quick-slot 7 action manager capture")) {
        return false;
    }
    g_originalResolveHotkeyAction = resolveHotkeyActionOriginal;

    void* lookupOriginal = nullptr;
    if (!InstallKnownInlineHook(lookupTarget,
            kLookupPrologue, sizeof(kLookupPrologue),
            reinterpret_cast<void*>(&ProxySkillQuickSlotLookup),
            &lookupOriginal, "skill quick-slot 7 lookup capture")) {
        return false;
    }
    g_originalSkillQuickSlotLookup = lookupOriginal;

    void* normalRenderOriginal = nullptr;
    if (!InstallKnownInlineHook(normalRenderTarget,
            kNormalRenderPrologue, sizeof(kNormalRenderPrologue),
            reinterpret_cast<void*>(&ProxySkillQuickSlotNormalRender),
            &normalRenderOriginal, "skill quick-slot 7 normal icon render")) {
        return false;
    }
    g_originalSkillQuickSlotNormalRender = normalRenderOriginal;

    void* extensionRenderOriginal = nullptr;
    if (!InstallKnownInlineHook(extensionRenderTarget,
            kExtensionRenderPrologue, sizeof(kExtensionRenderPrologue),
            reinterpret_cast<void*>(&ProxySkillQuickSlotExtensionRender),
            &extensionRenderOriginal,
            "skill quick-slot 7 extension icon render")) {
        return false;
    }
    g_originalSkillQuickSlotExtensionRender = extensionRenderOriginal;

    void* keyRenderOriginal = nullptr;
    if (!InstallKnownInlineHook(keyRenderTarget,
            kKeyRenderPrologue, sizeof(kKeyRenderPrologue),
            reinterpret_cast<void*>(&ProxySkillQuickSlotKeyRender),
            &keyRenderOriginal, "skill quick-slot 7 key-label render")) {
        return false;
    }
    g_originalSkillQuickSlotKeyRender = keyRenderOriginal;

    g_skillQuickSlotHotkeyResume = reinterpret_cast<void*>(
        g_dnfBase + kSkillQuickSlotHotkeyResumeRva);
    unsigned char hotkeyPatch[sizeof(kHotkeyExpected)] = {
        0xE9, 0, 0, 0, 0, 0x90
    };
    *reinterpret_cast<int32_t*>(hotkeyPatch + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(&ProxySkillQuickSlotHotkeyDispatch) -
        static_cast<intptr_t>(hotkeyTarget + 5));
    if (!WriteCodePatch(hotkeyTarget, hotkeyPatch, sizeof(hotkeyPatch),
            "skill quick-slot 7 hotkey dispatch")) {
        return false;
    }

    for (size_t i = 0; i < sizeof(limitPatches) / sizeof(limitPatches[0]); ++i) {
        if (!PatchSkillQuickSlotLimitByte(
                limitPatches[i].rva, limitPatches[i].label)) {
            return false;
        }
    }
    for (size_t i = 0;
         i < sizeof(hudCoordinatePatches) / sizeof(hudCoordinatePatches[0]);
         ++i) {
        if (!PatchSkillQuickSlotHudX(hudCoordinatePatches[i].rva,
                hudCoordinatePatches[i].expectedX,
                hudCoordinatePatches[i].replacementX,
                hudCoordinatePatches[i].label)) {
            return false;
        }
    }

    LogLine("skill quick-slot 7 compatibility installed normal_slot=%d extension_slot=%d normal_action=%u extension_action=%u slot_width=%d right_hud_shift=%d",
        kSkillQuickSlotNormal7, kSkillQuickSlotExtension7,
        kSkillQuickSlotNormal7Action, kSkillQuickSlotExtension7Action,
        kSkillQuickSlotColumnWidth, kSkillQuickSlotRightHudShift);
    return true;
}

bool InstallKnownInlineHook(uintptr_t target, const unsigned char* expected, size_t patchLength,
    void* detour, void** original, const char* label)
{
    if (!target || !expected || patchLength < 5 || !detour || !original) return false;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target), expected, patchLength)) {
        LogLine("%s native prologue mismatch at 0x%08X", label, target);
        return false;
    }

    unsigned char* trampoline = static_cast<unsigned char*>(VirtualAlloc(nullptr, patchLength + 5,
        MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!trampoline) {
        LogLine("%s trampoline allocation failed: %u", label, GetLastError());
        return false;
    }

    memcpy(trampoline, reinterpret_cast<void*>(target), patchLength);
    trampoline[patchLength] = 0xE9;
    *reinterpret_cast<int32_t*>(trampoline + patchLength + 1) = static_cast<int32_t>(
        static_cast<intptr_t>(target + patchLength) -
        reinterpret_cast<intptr_t>(trampoline + patchLength + 5));

    DWORD oldProtection = 0;
    if (!VirtualProtect(reinterpret_cast<void*>(target), patchLength, PAGE_EXECUTE_READWRITE, &oldProtection)) {
        LogLine("%s VirtualProtect failed: %u", label, GetLastError());
        VirtualFree(trampoline, 0, MEM_RELEASE);
        return false;
    }

    unsigned char jump[5] = { 0xE9 };
    *reinterpret_cast<int32_t*>(jump + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(detour) - static_cast<intptr_t>(target + 5));
    memcpy(reinterpret_cast<void*>(target), jump, sizeof(jump));
    for (size_t i = sizeof(jump); i < patchLength; ++i) {
        *reinterpret_cast<unsigned char*>(target + i) = 0x90;
    }

    DWORD ignoredProtection = 0;
    VirtualProtect(reinterpret_cast<void*>(target), patchLength, oldProtection, &ignoredProtection);
    FlushInstructionCache(GetCurrentProcess(), reinterpret_cast<void*>(target), patchLength);
    FlushInstructionCache(GetCurrentProcess(), trampoline, patchLength + 5);
    *original = trampoline;
    LogLine("%s installed target=0x%08X trampoline=%p", label, target, trampoline);
    return true;
}

#if 0
// Historical white-submission diagnostics retained only as reverse-engineering
// evidence. They are deliberately excluded from the shipping build now that
// the exact local-actor glow submission is known.
void LogBerserkerFrenzyWhiteCandidate(
    void* self, uintptr_t caller, unsigned int mask, unsigned int color)
{
    unsigned int alpha = color >> 24;
    if (InterlockedCompareExchange(
            &g_berserkerFrenzyDiagnosticDungeonActive, 0, 0) == 0 ||
        (color & 0x00FFFFFFu) != 0x00FFFFFFu ||
        alpha == 0 || alpha == 0xFFu ||
        InterlockedIncrement(&g_berserkerFrenzyWhiteCandidateLogCount) > 64) {
        return;
    }

    void* currentVisual = nullptr;
    void* objectManager = nullptr;
    void* currentActor = nullptr;
    void* actorVisual = nullptr;
    void* vtable = nullptr;
    unsigned int colorBefore = 0;
    unsigned int maskBefore = 0;
    int actorToSelfOffset = -1;
    int selfToActorOffset = -1;
    int selfToVisualOffset = -1;
    __try {
        currentVisual = *reinterpret_cast<void**>(
            g_dnfBase + kCurrentActorVisualPointerRva);
        objectManager = *reinterpret_cast<void**>(
            g_dnfBase + kObjectManagerPointerRva);
        if (objectManager) {
            currentActor = *reinterpret_cast<void**>(
                reinterpret_cast<uintptr_t>(objectManager) +
                kCurrentActorCacheOffset);
        }
        if (currentActor) {
            actorVisual = *reinterpret_cast<void**>(
                reinterpret_cast<uintptr_t>(currentActor) +
                kActorVisualOffset);
        }
        if (self) {
            vtable = *reinterpret_cast<void**>(self);
            colorBefore = *reinterpret_cast<unsigned int*>(
                reinterpret_cast<uintptr_t>(self) + 0x1F8);
            maskBefore = *reinterpret_cast<unsigned int*>(
                reinterpret_cast<uintptr_t>(self) + 0x1FC);
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
    }

    if (currentActor && self) {
        __try {
            for (int offset = 0; offset <= 0x1000; offset += 4) {
                if (*reinterpret_cast<void**>(
                        reinterpret_cast<uintptr_t>(currentActor) + offset) ==
                    self) {
                    actorToSelfOffset = offset;
                    break;
                }
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
        }
    }
    if (self) {
        __try {
            for (int offset = 0; offset <= 0x400; offset += 4) {
                void* value = *reinterpret_cast<void**>(
                    reinterpret_cast<uintptr_t>(self) + offset);
                if (selfToActorOffset < 0 && value == currentActor) {
                    selfToActorOffset = offset;
                }
                if (selfToVisualOffset < 0 && value == currentVisual) {
                    selfToVisualOffset = offset;
                }
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
        }
    }

    void* frames[8] = {};
    USHORT frameCount = CaptureStackBackTrace(
        0, static_cast<DWORD>(_countof(frames)), frames, nullptr);
    uintptr_t frameRvas[8] = {};
    for (USHORT index = 0; index < frameCount; ++index) {
        uintptr_t frame = reinterpret_cast<uintptr_t>(frames[index]);
        if (frame >= g_dnfBase) {
            frameRvas[index] = frame - g_dnfBase;
        }
    }

    LogLine("berserker Blood Frenzy white candidate index=%ld visual=%p vtable=%p caller_rva=0x%08X mask=0x%08X color=0x%08X color_before=0x%08X mask_before=0x%08X current_visual=%p object_manager=%p current_actor=%p actor_visual=%p local_global=%d local_actor_visual=%d actor_to_self=0x%X self_to_actor=0x%X self_to_visual=0x%X stack=%08X,%08X,%08X,%08X,%08X,%08X,%08X,%08X",
        g_berserkerFrenzyWhiteCandidateLogCount, self, vtable,
        caller >= g_dnfBase ? caller - g_dnfBase : caller,
        mask, color, colorBefore, maskBefore, currentVisual,
        objectManager, currentActor, actorVisual,
        self == currentVisual ? 1 : 0,
        self == actorVisual ? 1 : 0,
        actorToSelfOffset, selfToActorOffset, selfToVisualOffset,
        frameRvas[0], frameRvas[1], frameRvas[2], frameRvas[3],
        frameRvas[4], frameRvas[5], frameRvas[6], frameRvas[7]);
}

int __fastcall ProxyBerserkerFrenzyActorColorSet(
    void* self, void* /*unused*/, unsigned int mask, unsigned int color)
{
    ActorColorSetFn original = reinterpret_cast<ActorColorSetFn>(
        g_originalBerserkerFrenzyActorColorSet);
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    LogBerserkerFrenzyWhiteCandidate(self, caller, mask, color);
    bool replace = caller ==
            g_dnfBase + kBerserkerFrenzyActorColorSetCallerRva &&
        IsCurrentActorVisual(self) &&
        mask == kBerserkerFrenzyAlphaMask &&
        color == kBerserkerFrenzyWhiteColor;
    unsigned int effectiveMask = replace
        ? kBerserkerFrenzyAllColorMask
        : mask;
    unsigned int replacement = replace
        ? (color & kBerserkerFrenzyAlphaMask) |
            kBerserkerFrenzyRedRgb
        : color;
    int result = original
        ? original(self, effectiveMask, replacement)
        : static_cast<int>(replacement & effectiveMask);

    if (replace && InterlockedCompareExchange(
            &g_berserkerFrenzyActorColorSetHitLogged, 1, 0) == 0) {
        LogLine("berserker Blood Frenzy actor-color set first-hit visual=%p caller_rva=0x%08X source_mask=0x%08X source=0x%08X replacement_mask=0x%08X replacement=0x%08X",
            self, caller - g_dnfBase, mask, color,
            effectiveMask, replacement);
    }
    return result;
}

int __fastcall ProxyBerserkerFrenzyActorColorClear(
    void* self, void* /*unused*/, unsigned int mask)
{
    ActorColorClearFn original = reinterpret_cast<ActorColorClearFn>(
        g_originalBerserkerFrenzyActorColorClear);
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    bool replace = caller ==
            g_dnfBase + kBerserkerFrenzyActorColorClearCallerRva &&
        IsCurrentActorVisual(self) &&
        mask == kBerserkerFrenzyAlphaMask;
    unsigned int effectiveMask = replace
        ? kBerserkerFrenzyAllColorMask
        : mask;
    int result = original ? original(self, effectiveMask) : 0;

    if (replace && InterlockedCompareExchange(
            &g_berserkerFrenzyActorColorClearHitLogged, 1, 0) == 0) {
        LogLine("berserker Blood Frenzy actor-color clear first-hit visual=%p caller_rva=0x%08X source_mask=0x%08X replacement_mask=0x%08X",
            self, caller - g_dnfBase, mask, effectiveMask);
    }
    return result;
}

int __fastcall ProxyBerserkerFrenzySpriteColorStore(
    void* self, void* /*unused*/, unsigned int color)
{
    SpriteColorStoreFn original = reinterpret_cast<SpriteColorStoreFn>(
        g_originalBerserkerFrenzySpriteColorStore);
    uintptr_t caller = reinterpret_cast<uintptr_t>(_ReturnAddress());
    unsigned int alpha = color >> 24;
    bool candidate = InterlockedCompareExchange(
            &g_berserkerFrenzyDiagnosticDungeonActive, 0, 0) != 0 &&
        (color & 0x00FFFFFFu) == 0x00FFFFFFu &&
        alpha != 0 && alpha != 0xFFu;
    if (candidate && InterlockedIncrement(
            &g_berserkerFrenzySpriteWhiteCandidateLogCount) <= 128) {
        void* currentVisual = nullptr;
        void* currentActor = nullptr;
        void* vtable = nullptr;
        unsigned int colorBefore = 0;
        __try {
            currentVisual = *reinterpret_cast<void**>(
                g_dnfBase + kCurrentActorVisualPointerRva);
            void* objectManager = *reinterpret_cast<void**>(
                g_dnfBase + kObjectManagerPointerRva);
            if (objectManager) {
                currentActor = *reinterpret_cast<void**>(
                    reinterpret_cast<uintptr_t>(objectManager) +
                    kCurrentActorCacheOffset);
            }
            if (self) {
                vtable = *reinterpret_cast<void**>(self);
                colorBefore = *reinterpret_cast<unsigned int*>(
                    reinterpret_cast<uintptr_t>(self) + 0x60);
            }
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
        }

        void* frames[8] = {};
        USHORT frameCount = CaptureStackBackTrace(
            0, static_cast<DWORD>(_countof(frames)), frames, nullptr);
        uintptr_t frameRvas[8] = {};
        for (USHORT index = 0; index < frameCount; ++index) {
            uintptr_t frame = reinterpret_cast<uintptr_t>(frames[index]);
            if (frame >= g_dnfBase) {
                frameRvas[index] = frame - g_dnfBase;
            }
        }
        LogLine("berserker Blood Frenzy sprite white candidate index=%ld sprite=%p vtable=%p caller_rva=0x%08X color=0x%08X color_before=0x%08X current_visual=%p current_actor=%p stack=%08X,%08X,%08X,%08X,%08X,%08X,%08X,%08X",
            g_berserkerFrenzySpriteWhiteCandidateLogCount,
            self, vtable,
            caller >= g_dnfBase ? caller - g_dnfBase : caller,
            color, colorBefore, currentVisual, currentActor,
            frameRvas[0], frameRvas[1], frameRvas[2], frameRvas[3],
            frameRvas[4], frameRvas[5], frameRvas[6], frameRvas[7]);
    }
    return original ? original(self, color) : static_cast<int>(color);
}

bool InstallBerserkerFrenzyRenderCompatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t setTarget =
        g_dnfBase + kBerserkerFrenzyActorColorSetRva;
    uintptr_t clearTarget =
        g_dnfBase + kBerserkerFrenzyActorColorClearRva;
    uintptr_t spriteTarget =
        g_dnfBase + kBerserkerFrenzySpriteColorStoreRva;
    static const unsigned char kActorColorPrologue[] = {
        0x55, 0x8B, 0xEC, 0x56, 0x57
    };
    static const unsigned char kSpriteColorStorePrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08
    };

    // The packed EXE restores these functions after DLL injection. Wait for
    // the same complete five-byte instruction boundary at both functions.
    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(setTarget),
                kActorColorPrologue,
                sizeof(kActorColorPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(clearTarget),
                kActorColorPrologue,
                sizeof(kActorColorPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(spriteTarget),
                kSpriteColorStorePrologue,
                sizeof(kSpriteColorStorePrologue))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("berserker Blood Frenzy color diagnostic boundary unavailable set=0x%08X clear=0x%08X sprite=0x%08X",
            setTarget, clearTarget, spriteTarget);
        return false;
    }

    if (!InstallKnownInlineHook(setTarget,
            kActorColorPrologue,
            sizeof(kActorColorPrologue),
            reinterpret_cast<void*>(&ProxyBerserkerFrenzyActorColorSet),
            &g_originalBerserkerFrenzyActorColorSet,
            "berserker Blood Frenzy actor color set")) {
        return false;
    }

    if (!InstallKnownInlineHook(clearTarget,
            kActorColorPrologue,
            sizeof(kActorColorPrologue),
            reinterpret_cast<void*>(&ProxyBerserkerFrenzyActorColorClear),
            &g_originalBerserkerFrenzyActorColorClear,
            "berserker Blood Frenzy actor color clear")) {
        return false;
    }
    if (!InstallKnownInlineHook(spriteTarget,
            kSpriteColorStorePrologue,
            sizeof(kSpriteColorStorePrologue),
            reinterpret_cast<void*>(&ProxyBerserkerFrenzySpriteColorStore),
            &g_originalBerserkerFrenzySpriteColorStore,
            "berserker Blood Frenzy sprite color store diagnostic")) {
        return false;
    }
    LogLine("berserker Blood Frenzy paired actor-color override and dungeon white diagnostic installed set=0x%08X clear=0x%08X sprite=0x%08X set_caller_rva=0x%08X clear_caller_rva=0x%08X source_mask=0x%08X replacement_mask=0x%08X source=0x%08X replacement_rgb=0x%06X wait_ms=%d scope=current-local-visual diagnostic=translucent-white-in-dungeon",
        setTarget, clearTarget, spriteTarget,
        kBerserkerFrenzyActorColorSetCallerRva,
        kBerserkerFrenzyActorColorClearCallerRva,
        kBerserkerFrenzyAlphaMask,
        kBerserkerFrenzyAllColorMask,
        kBerserkerFrenzyWhiteColor,
        kBerserkerFrenzyRedRgb, waitedMs);
    return true;
}
#endif

bool IsBerserkerFrenzyRenderEligible()
{
    if (!g_dnfBase || InterlockedCompareExchange(
            &g_berserkerFrenzyDiagnosticDungeonActive, 0, 0) == 0) {
        return false;
    }
    LONG currentBaseJob = InterlockedCompareExchange(
        &g_berserkerFrenzyCurrentBaseJob, -1, -1);
    LONG currentGrowType = InterlockedCompareExchange(
        &g_berserkerFrenzyCurrentGrowType, -1, -1);
    return currentBaseJob == 0 && (currentGrowType & 0x0F) == 0x03;
}

void* ResolveCurrentActorRenderVisual()
{
    if (!g_dnfBase) return nullptr;
    __try {
        uintptr_t objectManager = *reinterpret_cast<uintptr_t*>(
            g_dnfBase + kObjectManagerPointerRva);
        if (!objectManager) return nullptr;
        uintptr_t currentActor = *reinterpret_cast<uintptr_t*>(
            objectManager + kCurrentActorCacheOffset);
        if (!currentActor) return nullptr;
        return *reinterpret_cast<void**>(
            currentActor + kActorRenderVisualOffset);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return nullptr;
    }
}

bool ResolveBerserkerFrenzyWaveform(unsigned int* waveform)
{
    if (waveform) *waveform = 0;
    if (!waveform || !g_dnfBase) return false;
    __try {
        uintptr_t slotAddress =
            g_dnfBase + kBerserkerFrenzyFixedStatusContainerRva +
            kBerserkerFrenzyWaveformSlot *
                kBerserkerFrenzyFixedStatusSlotStride;
        uintptr_t owner = *reinterpret_cast<uintptr_t*>(slotAddress);
        if (!owner) return false;
        unsigned int value = *reinterpret_cast<unsigned int*>(
            owner + kBerserkerFrenzyWaveformColorOffset);
        if ((value & kBerserkerFrenzyWhiteRgb) !=
                kBerserkerFrenzyWhiteRgb ||
            (value & kBerserkerFrenzyAlphaMask) == 0) {
            return false;
        }
        *waveform = value;
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

void __fastcall ProxyBerserkerFrenzyActorRender(
    void* self, void* /*unused*/)
{
    ActorRenderFn original = reinterpret_cast<ActorRenderFn>(
        g_originalBerserkerFrenzyActorRender);
    unsigned int waveform = 0;
    unsigned int originalColor = 0;
    unsigned int originalMask = 0;
    unsigned int replacementColor = 0;
    bool replaced = false;

    if (self && IsBerserkerFrenzyRenderEligible() &&
        self == ResolveCurrentActorRenderVisual() &&
        ResolveBerserkerFrenzyWaveform(&waveform)) {
        __try {
            unsigned int intensity = waveform >> 24;
            // Interpolate the red/green channels from native white (0xFF) to
            // the EXE-proved bleeding-red value (0x46). The blue byte remains
            // 0xFF in the client's packed BGR order. This preserves the
            // native smooth waveform without making the actor translucent.
            unsigned int redGreen = 0xFFu -
                ((0xB9u * intensity + 0x7Fu) / 0xFFu);
            unsigned int replacementRgb =
                (redGreen << 16) | (redGreen << 8) | 0xFFu;
            originalColor = *reinterpret_cast<unsigned int*>(
                reinterpret_cast<uintptr_t>(self) +
                kBerserkerFrenzyRenderColorOffset);
            originalMask = *reinterpret_cast<unsigned int*>(
                reinterpret_cast<uintptr_t>(self) +
                kBerserkerFrenzyRenderMaskOffset);
            replacementColor =
                (originalColor & kBerserkerFrenzyAlphaMask) |
                replacementRgb;
            *reinterpret_cast<unsigned int*>(
                reinterpret_cast<uintptr_t>(self) +
                kBerserkerFrenzyRenderColorOffset) = replacementColor;
            *reinterpret_cast<unsigned int*>(
                reinterpret_cast<uintptr_t>(self) +
                kBerserkerFrenzyRenderMaskOffset) =
                    originalMask | kBerserkerFrenzyWhiteRgb;
            replaced = true;
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
            replaced = false;
        }
    }

    if (original) original(self);

    if (replaced) {
        __try {
            *reinterpret_cast<unsigned int*>(
                reinterpret_cast<uintptr_t>(self) +
                kBerserkerFrenzyRenderColorOffset) = originalColor;
            *reinterpret_cast<unsigned int*>(
                reinterpret_cast<uintptr_t>(self) +
                kBerserkerFrenzyRenderMaskOffset) = originalMask;
        }
        __except (EXCEPTION_EXECUTE_HANDLER) {
        }
        if (InterlockedCompareExchange(
                &g_berserkerFrenzyRenderHitLogged, 1, 0) == 0) {
            LogLine("berserker Blood Frenzy local final-render first-hit visual=%p waveform=0x%08X original_color=0x%08X original_mask=0x%08X replacement_color=0x%08X",
                self, waveform, originalColor, originalMask,
                replacementColor);
        }
    }
}

bool InstallBerserkerFrenzyRenderCompatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t renderTarget =
        g_dnfBase + kBerserkerFrenzyActorRenderRva;
    static const unsigned char kDrawPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(renderTarget),
                kDrawPrologue, sizeof(kDrawPrologue))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("berserker Blood Frenzy local final-render boundary unavailable target=0x%08X",
            renderTarget);
        return false;
    }

    if (!InstallKnownInlineHook(renderTarget,
            kDrawPrologue, sizeof(kDrawPrologue),
            reinterpret_cast<void*>(&ProxyBerserkerFrenzyActorRender),
            &g_originalBerserkerFrenzyActorRender,
            "berserker Blood Frenzy local final actor render")) {
        return false;
    }

    LogLine("berserker Blood Frenzy local final-render waveform override installed render=0x%08X actor_visual_offset=0x%X container=0x%08X waveform_slot=%u waveform_color_offset=0x%X source_rgb=0x%06X peak_rgb=0x%06X wait_ms=%d scope=confirmed-dungeon-local-berserker-job0-grow3-current-actor-final-render",
        renderTarget, kActorRenderVisualOffset,
        g_dnfBase + kBerserkerFrenzyFixedStatusContainerRva,
        static_cast<unsigned int>(kBerserkerFrenzyWaveformSlot),
        static_cast<unsigned int>(kBerserkerFrenzyWaveformColorOffset),
        kBerserkerFrenzyWhiteRgb, kBerserkerFrenzyRedRgb,
        waitedMs);
    return true;
}

// Restore the missing null check in the class0/op3 user-state handler. The
// guard reuses the native handler's own null branch, before it pushes the two
// arguments for the call that otherwise carries a null `this` pointer.
bool InstallUserStateNullUiGuard()
{
    static const unsigned char kGuardSite[] = {
        0x8B, 0xF8, 0xE8, 0xD8, 0xBA, 0x90, 0x00
    };
    const size_t patchLength = sizeof(kGuardSite);
    uintptr_t target = g_dnfBase + kUserStateUiGuardRva;
    uintptr_t resume = g_dnfBase + kUserStateUiResumeRva;
    uintptr_t skip = g_dnfBase + kUserStateUiSkipRva;
    uintptr_t lookup = g_dnfBase + kUserStateUiLookupRva;

    if (!g_dnfBase || !BytesMatch(reinterpret_cast<unsigned char*>(target),
            kGuardSite, patchLength)) {
        LogLine("user-state null UI guard site verification failed target=0x%08X",
            target);
        return false;
    }

    unsigned char* stub = static_cast<unsigned char*>(VirtualAlloc(nullptr, 32,
        MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!stub) {
        LogLine("user-state null UI guard stub allocation failed: %u", GetLastError());
        return false;
    }

    size_t at = 0;
    stub[at++] = 0x8B; stub[at++] = 0xF8; // mov edi,eax
    stub[at++] = 0xE8;                    // call native lookup
    *reinterpret_cast<int32_t*>(stub + at) = static_cast<int32_t>(
        static_cast<intptr_t>(lookup) - reinterpret_cast<intptr_t>(stub + at + 4));
    at += 4;
    stub[at++] = 0x85; stub[at++] = 0xFF; // test edi,edi
    stub[at++] = 0x74; stub[at++] = 0x05; // jz skip jump
    stub[at++] = 0xE9;
    *reinterpret_cast<int32_t*>(stub + at) = static_cast<int32_t>(
        static_cast<intptr_t>(resume) - reinterpret_cast<intptr_t>(stub + at + 4));
    at += 4;
    stub[at++] = 0xE9;
    *reinterpret_cast<int32_t*>(stub + at) = static_cast<int32_t>(
        static_cast<intptr_t>(skip) - reinterpret_cast<intptr_t>(stub + at + 4));
    at += 4;

    DWORD oldProtection = 0;
    if (!VirtualProtect(reinterpret_cast<void*>(target), patchLength,
            PAGE_EXECUTE_READWRITE, &oldProtection)) {
        LogLine("user-state null UI guard VirtualProtect failed: %u", GetLastError());
        VirtualFree(stub, 0, MEM_RELEASE);
        return false;
    }
    unsigned char jump[5] = { 0xE9 };
    *reinterpret_cast<int32_t*>(jump + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(stub) - static_cast<intptr_t>(target + 5));
    memcpy(reinterpret_cast<void*>(target), jump, sizeof(jump));
    for (size_t i = sizeof(jump); i < patchLength; ++i) {
        *reinterpret_cast<unsigned char*>(target + i) = 0x90;
    }
    DWORD ignoredProtection = 0;
    VirtualProtect(reinterpret_cast<void*>(target), patchLength, oldProtection,
        &ignoredProtection);
    FlushInstructionCache(GetCurrentProcess(), reinterpret_cast<void*>(target), patchLength);
    FlushInstructionCache(GetCurrentProcess(), stub, at);
    LogLine("user-state null UI guard installed target=0x%08X stub=%p", target, stub);
    return true;
}

LONG g_dungeonEventMonsterConditionNullHits = 0;
LONG g_dungeonEventMonsterConditionNullHitsReported = 0;

void ReportDungeonEventMonsterConditionGuardHits()
{
    const LONG hits = g_dungeonEventMonsterConditionNullHits;
    if (hits == g_dungeonEventMonsterConditionNullHitsReported) {
        return;
    }
    g_dungeonEventMonsterConditionNullHitsReported = hits;
    LogLine("dungeon event-monster condition null scene skipped hits=%d "
        "opcode=0x0138 lookup=0x%08X resume=0x%08X null_skip=0x%08X "
        "scope=class0-op312-restored-native-null-check",
        static_cast<int>(hits),
        g_dnfBase + kDungeonEventMonsterConditionLookupRva,
        g_dnfBase + kDungeonEventMonsterConditionResumeRva,
        g_dnfBase + kDungeonEventMonsterConditionNullSkipRva);
}

bool InstallDungeonEventMonsterConditionGuard()
{
    if (!g_dnfBase) return false;

    uintptr_t target = g_dnfBase + kDungeonEventMonsterConditionLookupCallRva;
    uintptr_t lookup = g_dnfBase + kDungeonEventMonsterConditionLookupRva;
    uintptr_t resume = g_dnfBase + kDungeonEventMonsterConditionResumeRva;
    uintptr_t nullSkip = g_dnfBase + kDungeonEventMonsterConditionNullSkipRva;

    // Fail closed on any other DNF.exe: the lookup call, the `mov esi, eax`
    // that consumes it and the `mov [ebp-0x24], esi` that follows must all
    // match before the call is replaced.
    static const unsigned char kNativeContext[] = {
        0xE8, 0xB9, 0x8E, 0x00, 0x00,   // call sub_1D93480
        0x8B, 0xF0,                     // mov esi, eax
        0x8B, 0xCE,                     // mov ecx, esi
        0x89, 0x75, 0xDC                // mov [ebp-0x24], esi
    };
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
            kNativeContext, sizeof(kNativeContext))) {
        LogLine("dungeon event-monster condition null guard rejected: context mismatch at 0x%08X",
            target);
        return false;
    }

    // The five live bytes must actually encode a call to the lookup we think
    // they do. Without this check a constant written as a VA instead of an RVA
    // silently calls 0x400000 bytes away, into the middle of unrelated code.
    const int32_t encodedRel = *reinterpret_cast<const int32_t*>(target + 1);
    const uintptr_t encodedTarget = static_cast<uintptr_t>(
        static_cast<intptr_t>(target) + 5 + encodedRel);
    if (encodedTarget != lookup) {
        LogLine("dungeon event-monster condition null guard rejected: call target mismatch site=0x%08X encoded=0x%08X expected=0x%08X",
            target, encodedTarget, lookup);
        return false;
    }

    unsigned char* stub = static_cast<unsigned char*>(VirtualAlloc(nullptr, 32,
        MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!stub) {
        LogLine("dungeon event-monster condition null guard stub allocation failed: %u",
            GetLastError());
        return false;
    }

    // Stub layout (offsets in bytes):
    //   +0x00 call <sub_1D93480>
    //   +0x05 test eax, eax
    //   +0x07 jnz  +0x0C  -> +0x15
    //   +0x09 lock inc dword ptr [g_dungeonEventMonsterConditionNullHits]
    //   +0x10 jmp  <handler null epilogue 0x01D8A842>
    //   +0x15 jmp  <resume 0x01D8A5C7 / mov esi, eax>
    // Flags are free to clobber: the replaced call did the same.
    size_t at = 0;
    stub[at++] = 0xE8;
    *reinterpret_cast<int32_t*>(stub + at) = static_cast<int32_t>(
        static_cast<intptr_t>(lookup) - reinterpret_cast<intptr_t>(stub + at + 4));
    at += 4;
    stub[at++] = 0x85; stub[at++] = 0xC0;
    stub[at++] = 0x75; stub[at++] = 0x0C;
    stub[at++] = 0xF0; stub[at++] = 0xFF; stub[at++] = 0x05;
    *reinterpret_cast<uintptr_t*>(stub + at) =
        reinterpret_cast<uintptr_t>(&g_dungeonEventMonsterConditionNullHits);
    at += 4;
    stub[at++] = 0xE9;
    *reinterpret_cast<int32_t*>(stub + at) = static_cast<int32_t>(
        static_cast<intptr_t>(nullSkip) - reinterpret_cast<intptr_t>(stub + at + 4));
    at += 4;
    stub[at++] = 0xE9;
    *reinterpret_cast<int32_t*>(stub + at) = static_cast<int32_t>(
        static_cast<intptr_t>(resume) - reinterpret_cast<intptr_t>(stub + at + 4));
    at += 4;

    const size_t patchLength = 5;
    DWORD oldProtection = 0;
    if (!VirtualProtect(reinterpret_cast<void*>(target), patchLength,
            PAGE_EXECUTE_READWRITE, &oldProtection)) {
        LogLine("dungeon event-monster condition null guard VirtualProtect failed: %u",
            GetLastError());
        VirtualFree(stub, 0, MEM_RELEASE);
        return false;
    }
    unsigned char jump[5] = { 0xE9 };
    *reinterpret_cast<int32_t*>(jump + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(stub) - static_cast<intptr_t>(target + 5));
    memcpy(reinterpret_cast<void*>(target), jump, sizeof(jump));
    DWORD ignoredProtection = 0;
    VirtualProtect(reinterpret_cast<void*>(target), patchLength, oldProtection,
        &ignoredProtection);
    FlushInstructionCache(GetCurrentProcess(), reinterpret_cast<void*>(target), patchLength);
    FlushInstructionCache(GetCurrentProcess(), stub, at);
    LogLine("dungeon event-monster condition null guard installed target=0x%08X stub=%p resume=0x%08X null_skip=0x%08X null_hits=%p scope=class0-op312-restored-native-null-check",
        target, stub, resume, nullSkip,
        reinterpret_cast<void*>(&g_dungeonEventMonsterConditionNullHits));
    return true;
}

// 2026-09-13 进城镇闪退修正（材料栏那次修好后露出来的既有 bug，同族第三个成员）。
//
// 进城镇处理器 sub_15B20A0 的 0x015B21D0 分支：先按全局 0x51B275C 取对象，再调
// sub_1D933D0 取场景对象，然后把 [场景对象+0x140+0x5C] 当作 arg2 传给 sub_1A1DD60。
//
//   015B21D0 MOV  EDX,[0x51B275C]
//   015B21D6 PUSH EDX
//   015B21D7 CALL sub_1D93A40      ; 该调用不弹参，留一个槽给下一条消费
//   015B21DC MOV  ECX,EAX
//   015B21DE CALL sub_1D933D0      ; ★ 明确会返回 0（入参为 0，或虚调用返回 0）
//   015B21E3 PUSH 1
//   015B21E5 PUSH 1
//   015B21E7 MOV  ESI,EAX          ; ★ 不判空
//   015B21E9 CALL sub_1CAF560      ; 结尾是普通 RET，不弹参
//   015B21EE PUSH EAX
//   015B21EF MOV  ECX,ESI
//   015B21F1 CALL sub_23591E0      ; ★ 本体 = LEA EAX,[ECX+0x140]; RET
//   015B21F6 MOV  EAX,[EAX+0x5C]   ; ★ ESI==0 时 = [0x140+0x5C] = [0x19C] 崩
//   015B21F9 PUSH EAX              ; arg2
//   015B21FA MOV  ECX,[0x51B2798]
//   015B2200 PUSH 1                ; arg1
//   015B2202 CALL sub_1A1DD60      ; ret 0x14，两条返回都是 0x14
//   015B2207 MOV  ECX,[0x51B2764]  ; ← 本子步骤之后
//
// 实测崩溃：eip=0x019B21F6 eax=0x00000140 esi=0x00000000 access_address=0x0000019C
// （0x140+0x5C）。触发时机是 opcode 5 进城镇。
//
// sub_23591E0 只是个"取成员地址"的内联访问器（LEA + RET，RVA 0x1F591E0），
// 被大量调用点共用，不能整体改。所以只改 015B21F1 那一条 call 的 rel32，
// 让它落到一个空安全的替身；替身与原生唯一差别是 ECX==0 时改返回一块
// 全零字段区的地址，于是 015B21F6 读到的 arg2 就是 NULL。
//
// arg2 是对象指针，而 sub_1A1DD60 内部本来就判空（0x0161E168 `mov eax,[ebp+0xC]`
// / `test eax,eax` / `je 0x1a1e55a`，多条分支同理），所以传 NULL 是它已支持的输入。
//
// 改写仍然是一条完整 call 指令（只换 rel32），栈行为与原生逐字节一致，
// 不涉及任何 ESP 推理；跳转目标、指令边界与整块上下文都在改之前校验。
constexpr uintptr_t kSceneAccessorFnRva = 0x01F591E0; // sub_23591E0: LEA EAX,[ECX+0x140]; RET

// Every site below is the same misuse, repeated across the binary: the object comes
// from sub_1D933D0 (which returns 0 on two of its paths), is never null-checked, and
// the shared accessor's result is dereferenced at +0x5C. The 16 bytes are a
// fingerprint taken from the original executable; a site whose bytes do not match is
// left untouched and logged.
struct SceneAccessorGuardSite {
    uintptr_t callRva;
    const char* name;
    unsigned char expected[16];
};

static const SceneAccessorGuardSite kSceneAccessorGuardSites[] = {
    { 0x015B21F1, "town-entry A", { 0xE8, 0xEA, 0x6F, 0x9A, 0x00, 0x8B, 0x40, 0x5C, 0x50, 0x8B, 0x0D, 0x98, 0x27, 0x1B, 0x05, 0x6A } },
    { 0x015B26C1, "town-entry B", { 0xE8, 0x1A, 0x6B, 0x9A, 0x00, 0x8B, 0x40, 0x5C, 0x50, 0x8B, 0x0D, 0x98, 0x27, 0x1B, 0x05, 0x6A } },
    { 0x013AA064, "scene-crystal A", { 0xE8, 0x77, 0xF1, 0xBA, 0x00, 0x8B, 0x50, 0x5C, 0x8B, 0x0D, 0x98, 0x27, 0x1B, 0x05, 0x52, 0x6A } },
    { 0x013EE7F8, "scene-crystal B", { 0xE8, 0xE3, 0xA9, 0xB6, 0x00, 0x8B, 0x40, 0x5C, 0x8B, 0x0D, 0x98, 0x27, 0x1B, 0x05, 0x50, 0x6A } },
    { 0x013F185A, "scene-crystal C", { 0xE8, 0x81, 0x79, 0xB6, 0x00, 0x8B, 0x40, 0x5C, 0x8B, 0x0D, 0x98, 0x27, 0x1B, 0x05, 0x50, 0x6A } },
    { 0x015EE583, "scene-panel D",   { 0xE8, 0x58, 0xAC, 0x96, 0x00, 0x8B, 0x78, 0x5C, 0x8B, 0x96, 0x70, 0x03, 0x00, 0x00, 0x8B, 0x86 } },
    { 0x015F1357, "scene-panel E",   { 0xE8, 0x84, 0x7E, 0x96, 0x00, 0x8B, 0x5D, 0x0C, 0x8B, 0x78, 0x5C, 0x6A, 0x02, 0x8B, 0xCB, 0xE8 } },
};
constexpr size_t kSceneAccessorGuardSiteCount =
    sizeof(kSceneAccessorGuardSites) / sizeof(kSceneAccessorGuardSites[0]);

// Backing store for the substituted +0x5C source. Only that one dword is ever read
// from it and nothing writes through it, so a zero-filled block is enough.
unsigned char g_townEntrySceneNullField[0x60] = {};
LONG g_townEntrySceneAccessorNullHits = 0;
LONG g_townEntrySceneAccessorNullHitsReported = 0;

void ReportTownEntrySceneAccessorNullGuardHits()
{
    const LONG hits = g_townEntrySceneAccessorNullHits;
    if (hits == g_townEntrySceneAccessorNullHitsReported) {
        return;
    }
    g_townEntrySceneAccessorNullHitsReported = hits;
    LogLine("scene object missing, null arg2 substituted hits=%d accessor=0x%08X "
        "scope=scene-accessor-null-arg2-only",
        static_cast<int>(hits),
        static_cast<unsigned int>(g_dnfBase + kSceneAccessorFnRva));
}


bool InstallTownEntrySceneAccessorNullGuard()
{
    if (!g_dnfBase) return false;

    // One stub serves every site: the replaced call is reproduced verbatim, so the
    // stack/ESP behaviour at each site stays byte-identical to native (still a 5-byte
    // call, only the rel32 changes).
    //   +0x00 test ecx, ecx
    //   +0x02 jz   +0x0E
    //   +0x04 lea  eax, [ecx+0x140]      ; native behaviour when ecx != 0
    //   +0x0A ret
    //   +0x0B nop x3
    //   +0x0E lock inc dword ptr [hits]
    //   +0x15 mov  eax, <null field>     ; ecx == 0 -> [eax+0x5C] reads 0
    //   +0x1A ret
    unsigned char* stub = static_cast<unsigned char*>(VirtualAlloc(nullptr, 32,
        MEM_COMMIT | MEM_RESERVE, PAGE_EXECUTE_READWRITE));
    if (!stub) {
        LogLine("scene accessor null guard stub allocation failed: %lu", GetLastError());
        return false;
    }

    size_t at = 0;
    stub[at++] = 0x85; stub[at++] = 0xC9;                          // test ecx, ecx
    stub[at++] = 0x74; stub[at++] = 0x0A;                          // jz  +0x0E
    stub[at++] = 0x8D; stub[at++] = 0x81;                          // lea eax, [ecx+0x140]
    *reinterpret_cast<int32_t*>(stub + at) = 0x140;
    at += 4;
    stub[at++] = 0xC3;                                             // ret
    stub[at++] = 0x90; stub[at++] = 0x90; stub[at++] = 0x90;       // pad to +0x0E
    stub[at++] = 0xF0; stub[at++] = 0xFF; stub[at++] = 0x05;       // lock inc [hits]
    *reinterpret_cast<uintptr_t*>(stub + at) =
        reinterpret_cast<uintptr_t>(&g_townEntrySceneAccessorNullHits);
    at += 4;
    stub[at++] = 0xB8;                                             // mov eax, imm32
    *reinterpret_cast<uintptr_t*>(stub + at) =
        reinterpret_cast<uintptr_t>(&g_townEntrySceneNullField[0]);
    at += 4;
    stub[at++] = 0xC3;                                             // ret
    if (at != 0x1B) {
        LogLine("scene accessor null guard stub layout drift: %u bytes", at);
        VirtualFree(stub, 0, MEM_RELEASE);
        return false;
    }
    FlushInstructionCache(GetCurrentProcess(), stub, at);

    size_t patched = 0;
    size_t skipped = 0;
    for (size_t i = 0; i < kSceneAccessorGuardSiteCount; ++i) {
        const SceneAccessorGuardSite& site = kSceneAccessorGuardSites[i];
        unsigned char* callSite =
            reinterpret_cast<unsigned char*>(g_dnfBase + site.callRva);

        // Fingerprint first: a site that does not look exactly like the original
        // executable is left completely untouched.
        if (memcmp(callSite, site.expected, sizeof(site.expected)) != 0) {
            LogLine("scene accessor null guard site '%s' skipped: context mismatch at 0x%08X",
                site.name, static_cast<unsigned int>(g_dnfBase + site.callRva));
            ++skipped;
            continue;
        }
        const int32_t encodedRel = *reinterpret_cast<const int32_t*>(callSite + 1);
        const uintptr_t encodedTarget = static_cast<uintptr_t>(
            reinterpret_cast<intptr_t>(callSite) + 5 + encodedRel);
        if (encodedTarget != g_dnfBase + kSceneAccessorFnRva) {
            LogLine("scene accessor null guard site '%s' skipped: call target mismatch "
                "encoded=0x%08X expected=0x%08X", site.name,
                static_cast<unsigned int>(encodedTarget),
                static_cast<unsigned int>(g_dnfBase + kSceneAccessorFnRva));
            ++skipped;
            continue;
        }

        DWORD oldProtection = 0;
        if (!VirtualProtect(callSite, 5, PAGE_EXECUTE_READWRITE, &oldProtection)) {
            LogLine("scene accessor null guard site '%s' VirtualProtect failed: %lu",
                site.name, GetLastError());
            ++skipped;
            continue;
        }
        *reinterpret_cast<int32_t*>(callSite + 1) = static_cast<int32_t>(
            reinterpret_cast<intptr_t>(stub) - reinterpret_cast<intptr_t>(callSite + 5));
        DWORD ignoredProtection = 0;
        VirtualProtect(callSite, 5, oldProtection, &ignoredProtection);
        FlushInstructionCache(GetCurrentProcess(), callSite, 5);
        LogLine("scene accessor null guard site '%s' patched call_site=0x%08X stub=%p",
            site.name, static_cast<unsigned int>(g_dnfBase + site.callRva), stub);
        ++patched;
    }

    LogLine("scene accessor null guard installed stub=%p accessor=0x%08X null_field=%p "
        "null_hits=%p patched=%u skipped=%u scope=scene-accessor-null-arg2-only",
        stub, static_cast<unsigned int>(g_dnfBase + kSceneAccessorFnRva),
        reinterpret_cast<void*>(&g_townEntrySceneNullField[0]),
        reinterpret_cast<void*>(&g_townEntrySceneAccessorNullHits),
        static_cast<unsigned int>(patched), static_cast<unsigned int>(skipped));
    if (patched == 0) {
        VirtualFree(stub, 0, MEM_RELEASE);
        return false;
    }
    return true;
}
// ════════════════════════════════════════════════════════════════════════════
//  商城网页改向：点券充值 / 黑钻会员 → 我们自己的页面
//
//  官方客户端里这两个按钮是"打开腾讯充值/会员网页"的入口。私服没有那套后台：
//  点了没有任何反馈；而"黑钻会员"更早还被本 DLL 打成 no-op —— 因为原生点击路径
//  在这个 UI 变体里会读空的 [esi+0x248B0]，会崩/卡（原注释见 kCeraShopVipClickRva）。
//
//  当前 EXE 的导入表里，可能被这两个按钮用到的"打开网页"入口是：
//      SHELL32!ShellExecuteW / ShellExecuteExW / ShellExecuteA
//      KERNEL32!CreateProcessA / CreateProcessW        （只记录，不改行为）
//      awesomium!WebString::CreateFromUTF8              （内嵌浏览器构造 URL 字符串）
//  这里替换导入表指针做 IAT hook：不写代码字节、没有 5 字节长度限制、随时可还原。
//  命中"付款/会员"特征的地址换成 kShopWebUrl；不匹配的网页原样放行，不打扰其它功能。
//
//  排障：所有命中都会写 90CN_trace.log（shop-web: ...），换 URL/关开关只需重编。
// ════════════════════════════════════════════════════════════════════════════
wchar_t g_shopWebUrl[512] = { 0 };
char g_shopWebUrlAnsi[512] = { 0 };
bool g_shopWebUrlLoaded = false;

void ToNarrowUtf8(const wchar_t* wide, char* out, size_t outSize)
{
    if (!out || outSize == 0) return;
    out[0] = '\0';
    if (!wide) return;
    WideCharToMultiByte(CP_UTF8, 0, wide, -1, out, static_cast<int>(outSize), nullptr, nullptr);
    out[outSize - 1] = '\0';
}

// 读取 90CN.ini（客户端目录）里的 [web] shop_url，缺省用编译期常量。
void LoadShopWebUrl()
{
    if (g_shopWebUrlLoaded) return;
    g_shopWebUrlLoaded = true;
    wcsncpy_s(g_shopWebUrl, kShopWebUrlDefault, _TRUNCATE);

    wchar_t exePath[MAX_PATH] = { 0 };
    if (GetModuleFileNameW(nullptr, exePath, MAX_PATH) != 0) {
        wchar_t* slash = wcsrchr(exePath, L'\\');
        if (slash) {
            slash[1] = L'\0';
            wchar_t iniPath[MAX_PATH] = { 0 };
            _snwprintf(iniPath, MAX_PATH - 1, L"%s90CN.ini", exePath);
            if (GetFileAttributesW(iniPath) != INVALID_FILE_ATTRIBUTES) {
                wchar_t buffer[512] = { 0 };
                GetPrivateProfileStringW(L"web", L"shop_url", kShopWebUrlDefault,
                    buffer, 511, iniPath);
                if (buffer[0]) wcsncpy_s(g_shopWebUrl, buffer, _TRUNCATE);
            }
        }
    }
    ToNarrowUtf8(g_shopWebUrl, g_shopWebUrlAnsi, sizeof(g_shopWebUrlAnsi));
    LogLine("shop-web: url=%s", g_shopWebUrlAnsi);
}

// 我们要换掉哪些网页地址：
//   · 空地址 / 非 http(s) —— 死链，直接换；
//   · 命中付款/会员特征（腾讯充值页、会员页、点券相关域名…）；
//   · 已经是我们的地址 —— 放行。
//  其余网页（公告、活动、复看等）一律放行，不打扰。
bool ShopWebUrlShouldRedirectAnsi(const char* url)
{
    // ShellExecute is also used for ordinary local programs and documents.
    // Never turn a local path (for example a launcher's 7z.exe) into a web
    // redirect merely because it is not an HTTP URL.
    if (!url || !*url) return false;
    if (_strnicmp(url, "http://", 7) != 0 && _strnicmp(url, "https://", 8) != 0) return false;

    char lower[1024] = { 0 };
    size_t length = strlen(url);
    if (length > sizeof(lower) - 1) length = sizeof(lower) - 1;
    for (size_t i = 0; i < length; ++i) lower[i] = static_cast<char>(tolower(static_cast<unsigned char>(url[i])));

    if (strstr(lower, "68n.cn") != nullptr) return false;
    static const char* const kPatterns[] = {
        "pay", "charge", "ipg", "diamond", "vip", "member", "tenpay",
        "qq.com", "dnfcity", "dnfchina", "dnf.163", "17game",
    };
    for (const char* pattern : kPatterns) {
        if (strstr(lower, pattern) != nullptr) return true;
    }
    return false;
}

bool ShopWebUrlShouldRedirectW(const wchar_t* url)
{
    char narrow[1024] = { 0 };
    ToNarrowUtf8(url, narrow, sizeof(narrow));
    return ShopWebUrlShouldRedirectAnsi(narrow);
}

// ── IAT 助手：在已加载模块的导入表里找/替换某个函数的槽位 ───────────────────
void** FindImportedSlot(HMODULE module, const char* moduleName, const char* functionName)
{
    if (!module || !moduleName || !functionName) return nullptr;
    unsigned char* base = reinterpret_cast<unsigned char*>(module);
    __try {
        auto dos = reinterpret_cast<IMAGE_DOS_HEADER*>(base);
        if (dos->e_magic != IMAGE_DOS_SIGNATURE) return nullptr;
        auto nt = reinterpret_cast<IMAGE_NT_HEADERS*>(base + dos->e_lfanew);
        if (nt->Signature != IMAGE_NT_SIGNATURE) return nullptr;
        const IMAGE_DATA_DIRECTORY& dir =
            nt->OptionalHeader.DataDirectory[IMAGE_DIRECTORY_ENTRY_IMPORT];
        if (!dir.VirtualAddress) return nullptr;
        auto descriptor = reinterpret_cast<IMAGE_IMPORT_DESCRIPTOR*>(base + dir.VirtualAddress);
        for (; descriptor->Name; ++descriptor) {
            const char* name = reinterpret_cast<const char*>(base + descriptor->Name);
            if (_stricmp(name, moduleName) != 0) continue;
            auto thunk = reinterpret_cast<IMAGE_THUNK_DATA*>(base + descriptor->FirstThunk);
            auto lookup = descriptor->OriginalFirstThunk
                ? reinterpret_cast<IMAGE_THUNK_DATA*>(base + descriptor->OriginalFirstThunk)
                : thunk;
            for (; thunk->u1.Function; ++thunk, ++lookup) {
                if (lookup->u1.Ordinal & IMAGE_ORDINAL_FLAG) continue;
                auto import = reinterpret_cast<IMAGE_IMPORT_BY_NAME*>(base + lookup->u1.AddressOfData);
                if (strcmp(reinterpret_cast<const char*>(import->Name), functionName) == 0) {
                    return reinterpret_cast<void**>(&thunk->u1.Function);
                }
            }
            return nullptr;
        }
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        return nullptr;
    }
    return nullptr;
}

bool PatchImportedSlot(void** slot, void* replacement, const char* label)
{
    if (!slot || !replacement) return false;
    DWORD oldProtection = 0;
    if (!VirtualProtect(slot, sizeof(void*), PAGE_READWRITE, &oldProtection)) {
        LogLine("shop-web: %s VirtualProtect failed: %u", label, GetLastError());
        return false;
    }
    *slot = replacement;
    DWORD ignoredProtection = 0;
    VirtualProtect(slot, sizeof(void*), oldProtection, &ignoredProtection);
    LogLine("shop-web: %s iat hook installed", label);
    return true;
}

typedef HINSTANCE (WINAPI *ShopShellExecuteWFn)(HWND, LPCWSTR, LPCWSTR, LPCWSTR, LPCWSTR, INT);
typedef HINSTANCE (WINAPI *ShopShellExecuteAFn)(HWND, LPCSTR, LPCSTR, LPCSTR, LPCSTR, INT);
typedef BOOL (WINAPI *ShopShellExecuteExWFn)(SHELLEXECUTEINFOW*);
typedef BOOL (WINAPI *ShopCreateProcessAFn)(LPCSTR, LPSTR, LPSECURITY_ATTRIBUTES,
    LPSECURITY_ATTRIBUTES, BOOL, DWORD, LPVOID, LPCSTR, LPSTARTUPINFOA, LPPROCESS_INFORMATION);
typedef BOOL (WINAPI *ShopCreateProcessWFn)(LPCWSTR, LPWSTR, LPSECURITY_ATTRIBUTES,
    LPSECURITY_ATTRIBUTES, BOOL, DWORD, LPVOID, LPCWSTR, LPSTARTUPINFOW, LPPROCESS_INFORMATION);
typedef void* (__cdecl *ShopWebStringFromUtf8Fn)(void*, const char*, unsigned int);

ShopShellExecuteWFn g_realShopShellExecuteW = nullptr;
ShopShellExecuteAFn g_realShopShellExecuteA = nullptr;
ShopShellExecuteExWFn g_realShopShellExecuteExW = nullptr;
ShopCreateProcessAFn g_realShopCreateProcessA = nullptr;
ShopCreateProcessWFn g_realShopCreateProcessW = nullptr;
ShopWebStringFromUtf8Fn g_realShopWebStringFromUtf8 = nullptr;

void LogWideUrl(const char* label, const wchar_t* url)
{
    char narrow[1024] = { 0 };
    ToNarrowUtf8(url, narrow, sizeof(narrow));
    LogLine("shop-web: %s url=%s", label, narrow);
}

// 用系统默认浏览器打开我们的页面（始终调用"真"的 ShellExecuteW，避免自递归）。
void OpenShopWebPage(const char* source)
{
    LoadShopWebUrl();
    LogLine("shop-web: %s -> open %s", source, g_shopWebUrlAnsi);
    ShopShellExecuteWFn execute = g_realShopShellExecuteW;
    if (!execute) {
        HMODULE shell = GetModuleHandleW(L"shell32.dll");
        execute = shell
            ? reinterpret_cast<ShopShellExecuteWFn>(GetProcAddress(shell, "ShellExecuteW"))
            : nullptr;
    }
    if (execute) {
        execute(nullptr, L"open", g_shopWebUrl, nullptr, nullptr, SW_SHOWNORMAL);
    } else {
        LogLine("shop-web: ShellExecuteW unavailable, url not opened");
    }
}

HINSTANCE WINAPI ProxyShopShellExecuteW(HWND hwnd, LPCWSTR operation, LPCWSTR file,
    LPCWSTR parameters, LPCWSTR directory, INT show)
{
    const bool redirect = ShopWebUrlShouldRedirectW(file);
    LogWideUrl("ShellExecuteW", file);
    if (redirect) {
        OpenShopWebPage("ShellExecuteW");
        return reinterpret_cast<HINSTANCE>(33);  // >32 = 成功
    }
    if (g_realShopShellExecuteW) {
        return g_realShopShellExecuteW(hwnd, operation, file, parameters, directory, show);
    }
    return reinterpret_cast<HINSTANCE>(2);  // ERROR_FILE_NOT_FOUND
}

HINSTANCE WINAPI ProxyShopShellExecuteA(HWND hwnd, LPCSTR operation, LPCSTR file,
    LPCSTR parameters, LPCSTR directory, INT show)
{
    const bool redirect = ShopWebUrlShouldRedirectAnsi(file);
    LogLine("shop-web: ShellExecuteA file=%s redirect=%d", file ? file : "", redirect ? 1 : 0);
    if (redirect) {
        OpenShopWebPage("ShellExecuteA");
        return reinterpret_cast<HINSTANCE>(33);
    }
    if (g_realShopShellExecuteA) {
        return g_realShopShellExecuteA(hwnd, operation, file, parameters, directory, show);
    }
    return reinterpret_cast<HINSTANCE>(2);
}

BOOL WINAPI ProxyShopShellExecuteExW(SHELLEXECUTEINFOW* info)
{
    if (!info) {
        return g_realShopShellExecuteExW ? g_realShopShellExecuteExW(info) : FALSE;
    }
    const bool redirect = ShopWebUrlShouldRedirectW(info->lpFile);
    LogWideUrl("ShellExecuteExW", info->lpFile);
    if (redirect) {
        OpenShopWebPage("ShellExecuteExW");
        return TRUE;
    }
    return g_realShopShellExecuteExW ? g_realShopShellExecuteExW(info) : FALSE;
}

// CreateProcess 只记录：客户端用它启动官方启动器/浏览器时，我们不该改它的行为。
BOOL WINAPI ProxyShopCreateProcessA(LPCSTR application, LPSTR commandLine,
    LPSECURITY_ATTRIBUTES processAttributes, LPSECURITY_ATTRIBUTES threadAttributes,
    BOOL inheritHandles, DWORD creationFlags, LPVOID environment, LPCSTR currentDirectory,
    LPSTARTUPINFOA startupInfo, LPPROCESS_INFORMATION processInformation)
{
    LogLine("shop-web: CreateProcessA app=%s cmd=%s", application ? application : "",
        commandLine ? commandLine : "");
    if (g_realShopCreateProcessA) {
        return g_realShopCreateProcessA(application, commandLine, processAttributes,
            threadAttributes, inheritHandles, creationFlags, environment, currentDirectory,
            startupInfo, processInformation);
    }
    return FALSE;
}

BOOL WINAPI ProxyShopCreateProcessW(LPCWSTR application, LPWSTR commandLine,
    LPSECURITY_ATTRIBUTES processAttributes, LPSECURITY_ATTRIBUTES threadAttributes,
    BOOL inheritHandles, DWORD creationFlags, LPVOID environment, LPCWSTR currentDirectory,
    LPSTARTUPINFOW startupInfo, LPPROCESS_INFORMATION processInformation)
{
    LogWideUrl("CreateProcessW app", application);
    if (g_realShopCreateProcessW) {
        return g_realShopCreateProcessW(application, commandLine, processAttributes,
            threadAttributes, inheritHandles, creationFlags, environment, currentDirectory,
            startupInfo, processInformation);
    }
    return FALSE;
}

// Awesomium 的 WebString::CreateFromUTF8（静态成员，返回对象走隐藏返回指针）：
// 默认原样透传，只在字符串是"要被换掉的网页地址"时改成我们的地址。
void* __cdecl ProxyShopWebStringFromUtf8(void* result, const char* utf8, unsigned int length)
{
    if (utf8 && length && ShopWebUrlShouldRedirectAnsi(utf8)) {
        LogLine("shop-web: WebString.CreateFromUTF8 redirect from=%.200s", utf8);
        if (g_realShopWebStringFromUtf8) {
            return g_realShopWebStringFromUtf8(result, g_shopWebUrlAnsi,
                static_cast<unsigned int>(strlen(g_shopWebUrlAnsi)));
        }
    }
    if (g_realShopWebStringFromUtf8) {
        return g_realShopWebStringFromUtf8(result, utf8, length);
    }
    return result;
}

bool InstallShopWebRedirect()
{
    static bool s_attempted = false;
    static bool s_hooksInstalled = false;
    // 单次安装：导出的槽位一旦被我们换过，再进来读到的"真函数"就是自己的代理，
    // 会造成无限自递归 —— 所以这里必须只跑一次。
    if (s_attempted) return s_hooksInstalled;
    s_attempted = true;
    if (!kShopWebRedirectEnabled) return false;
    if (!g_dnfBase) return false;
    LoadShopWebUrl();
    HMODULE exe = reinterpret_cast<HMODULE>(g_dnfBase);
    int hooks = 0;

    if (void** slot = FindImportedSlot(exe, "SHELL32.dll", "ShellExecuteW")) {
        g_realShopShellExecuteW = reinterpret_cast<ShopShellExecuteWFn>(*slot);
        if (PatchImportedSlot(slot, reinterpret_cast<void*>(&ProxyShopShellExecuteW),
                "ShellExecuteW")) {
            ++hooks;
        }
    } else {
        LogLine("shop-web: ShellExecuteW import not found");
    }
    if (void** slot = FindImportedSlot(exe, "SHELL32.dll", "ShellExecuteA")) {
        g_realShopShellExecuteA = reinterpret_cast<ShopShellExecuteAFn>(*slot);
        if (PatchImportedSlot(slot, reinterpret_cast<void*>(&ProxyShopShellExecuteA),
                "ShellExecuteA")) {
            ++hooks;
        }
    }
    if (void** slot = FindImportedSlot(exe, "SHELL32.dll", "ShellExecuteExW")) {
        g_realShopShellExecuteExW = reinterpret_cast<ShopShellExecuteExWFn>(*slot);
        if (PatchImportedSlot(slot, reinterpret_cast<void*>(&ProxyShopShellExecuteExW),
                "ShellExecuteExW")) {
            ++hooks;
        }
    }
    if (void** slot = FindImportedSlot(exe, "KERNEL32.dll", "CreateProcessA")) {
        g_realShopCreateProcessA = reinterpret_cast<ShopCreateProcessAFn>(*slot);
        PatchImportedSlot(slot, reinterpret_cast<void*>(&ProxyShopCreateProcessA),
            "CreateProcessA(log-only)");
    }
    if (void** slot = FindImportedSlot(exe, "KERNEL32.dll", "CreateProcessW")) {
        g_realShopCreateProcessW = reinterpret_cast<ShopCreateProcessWFn>(*slot);
        PatchImportedSlot(slot, reinterpret_cast<void*>(&ProxyShopCreateProcessW),
            "CreateProcessW(log-only)");
    }
    if (kShopWebHookAwesomium) {
        if (void** slot = FindImportedSlot(exe, "awesomium.dll",
                "?CreateFromUTF8@WebString@Awesomium@@SA?AV12@PBDI@Z")) {
            g_realShopWebStringFromUtf8 =
                reinterpret_cast<ShopWebStringFromUtf8Fn>(*slot);
            if (PatchImportedSlot(slot,
                    reinterpret_cast<void*>(&ProxyShopWebStringFromUtf8),
                    "awesomium WebString.CreateFromUTF8")) {
                ++hooks;
            }
        } else {
            LogLine("shop-web: awesomium CreateFromUTF8 import not found");
        }
    }
    LogLine("shop-web: redirect installed hooks=%d url=%s", hooks, g_shopWebUrlAnsi);
    s_hooksInstalled = hooks > 0;
    return s_hooksInstalled;
}

// 加壳客户端可能在 DllMain 之后才把真实导入表补齐，所以钩不上时由主窗口的
// 100 ms 定时器再试几次（最多 40 次 ≈ 4 秒）。成功或放弃后都不再执行。
void TryDeferredShopWebRedirect()
{
    static bool installed = false;
    static int attempts = 0;
    if (installed || attempts >= 40) return;
    ++attempts;
    installed = InstallShopWebRedirect();
    if (installed) {
        LogLine("shop-web: redirect installed on deferred attempt %d", attempts);
    }
}

// "黑钻会员"按钮的原生处理（sub_16A2660）：不跑原生实现（它会读空的 [esi+0x248B0]
// 而崩/卡），只打开我们的页面然后返回 0。
int __cdecl ProxyCeraShopVipButtonClick()
{
    OpenShopWebPage("cera-shop-vip-button");
    return 0;
}

bool InstallCeraShopVipButtonCompatibility()
{
    if (!g_dnfBase) return false;
    LoadShopWebUrl();
    uintptr_t target = g_dnfBase + kCeraShopVipClickRva;
    static const unsigned char kCeraShopVipPrologue[] = {
        0x56, 0x8B, 0xF1, 0x80, 0xBE, 0x64, 0x41, 0x02, 0x00, 0x00
    };
    if (!BytesMatch(reinterpret_cast<const unsigned char*>(target),
            kCeraShopVipPrologue, sizeof(kCeraShopVipPrologue))) {
        LogLine("cera shop VIP button compatibility skipped: prologue mismatch at 0x%08X",
            target);
        return false;
    }
    // 2026-10-17：原来是 no-op 防崩（31 C0 C3）。现在改成"打开我们的商城/充值页"，
    // 原生实现依旧跳过 —— 它在这个 UI 变体里会读空的 [esi+0x248B0]。
    static void* s_originalCeraShopVipClick = nullptr;
    if (!InstallKnownInlineHook(target, kCeraShopVipPrologue,
            sizeof(kCeraShopVipPrologue),
            reinterpret_cast<void*>(&ProxyCeraShopVipButtonClick),
            &s_originalCeraShopVipClick,
            "cera shop VIP button -> shop web url")) {
        return false;
    }
    LogLine("cera shop VIP button target=0x%08X now opens %s", target, g_shopWebUrlAnsi);
    return true;
}


bool InstallSkillTreeProfessionVisibilityCompatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t target = g_dnfBase + kSkillCatalogCollectRva;
    static const unsigned char kCatalogCollectPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(target),
                kCatalogCollectPrologue, sizeof(kCatalogCollectPrologue))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("skill-tree profession visibility target unavailable collect=0x%08X",
            target);
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(target,
            kCatalogCollectPrologue, sizeof(kCatalogCollectPrologue),
            reinterpret_cast<void*>(&ProxySkillCatalogCollect), &original,
            "skill-tree profession visibility")) {
        return false;
    }
    g_originalSkillCatalogCollect = original;
    LogLine("skill-tree profession visibility installed collect=0x%08X predicate=0x%08X wait_ms=%d scope=sub_1FE6D00-only",
        target, g_dnfBase + kSkillFitnessPredicateRva, waitedMs);
    return true;
}

// 情侣树（结婚系统）剩余时间显示。
//
// 原生窗口 sub_20C7A00 把 now + 生命周期 - 过期时间（即"已过"秒数）当成剩余时间
// 显示。这里只把那段算术换成 max(到期 - 现在, 0)，保留权威的到期时间与原生 UI。
// 与其它兼容项一样：先按签名核对原始字节，对不上就只记日志返回 false，绝不盲改。
bool InstallWeddingTreeTimeCompatibility()
{
    // The native window labels elapsed seconds (now + lifetime - expiry)
    // as remaining time. Keep the authoritative expiry and the native UI.
    static const unsigned char expected[] = { 0x8B, 0x7D, 0xE8, 0x2B, 0xFB, 0x03, 0xF8 };
    uintptr_t target = g_dnfBase + 0x1CC7B3C;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target), expected, sizeof(expected))) {
        LogLine("wedding tree remaining time signature mismatch");
        return false;
    }
    unsigned char patch[] = { 0x50, 0x53, 0xE8, 0, 0, 0, 0 };
    *reinterpret_cast<int32_t*>(patch + 3) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(&WeddingTreeRemainingSeconds) -
        static_cast<intptr_t>(target + sizeof(patch)));
    return WriteCodePatch(target, patch, sizeof(patch), "wedding tree remaining time");
}

bool InstallPartyDirectoryFullPageCompatibility()
{
    // sub_326D450 opens owner 0x17B for ordinary channels. That owner is the
    // six-row regional summary and never sends the full-directory request.
    // The current EXE's native full party directory is owner 9 and its only
    // native open site passes owner 9. Keep the controller lifecycle symmetric
    // by changing its owner guard, open owner, and both owner-0x17B close paths
    // as one validated patch set. Preserve the controller's existing mode/group
    // arguments; owner 9's current-EXE open handler does not read either value.
    static const unsigned char kOwner9[] = {
        0x68, 0x09, 0x00, 0x00, 0x00
    };
    static const unsigned char kGuardContext[] = {
        0x55, 0x8B, 0xEC, 0x57, 0x8B, 0xF9, 0x8B, 0x0D,
        0x64, 0x27, 0x1B, 0x05, 0x68, 0x7B, 0x01, 0x00,
        0x00, 0xE8, 0xDA, 0x7A, 0xD6, 0xFF, 0x84, 0xC0,
        0x75, 0x46, 0x83, 0x3D, 0xE4, 0x0A, 0x2E, 0x05,
        0x00, 0x75, 0x3D, 0x56, 0x6A, 0x00, 0x6A, 0xFF,
        0x6A, 0xFF, 0x6A, 0x00, 0x6A
    };
    static const unsigned char kOpenContext[] = {
        0x8B, 0x75, 0x08, 0x8B, 0x0D, 0x64, 0x27, 0x1B,
        0x05, 0x83, 0xC4, 0x1C, 0x56, 0x6A, 0x00, 0x68,
        0x7B, 0x01, 0x00, 0x00, 0xE8, 0x24, 0x02, 0xD7,
        0xFF, 0x89, 0x77, 0x34
    };
    static const unsigned char kCloseContextA[] = {
        0x8B, 0x0D, 0x64, 0x27, 0x1B, 0x05, 0x6A, 0x00,
        0x6A, 0xFF, 0x68, 0x7C, 0x01, 0x00, 0x00, 0xE8,
        0xA5, 0xB1, 0xD6, 0xFF, 0x8B, 0x0D, 0x64, 0x27,
        0x1B, 0x05, 0x6A, 0x00, 0x6A, 0xFF, 0x68, 0x7B,
        0x01, 0x00, 0x00, 0xE8, 0x91, 0xB1, 0xD6, 0xFF,
        0x5F
    };
    static const unsigned char kCloseContextB[] = {
        0x8B, 0x0D, 0x64, 0x27, 0x1B, 0x05, 0x6A, 0x00,
        0x6A, 0xFF, 0x68, 0x7C, 0x01, 0x00, 0x00, 0xE8,
        0x29, 0xB1, 0xD6, 0xFF, 0x8B, 0x0D, 0x64, 0x27,
        0x1B, 0x05, 0x6A, 0x00, 0x6A, 0xFF, 0x68, 0x7B,
        0x01, 0x00, 0x00, 0xE8, 0x15, 0xB1, 0xD6, 0xFF,
        0x5E
    };
    struct Context {
        uintptr_t target;
        const unsigned char* expected;
        size_t length;
        const char* label;
    };
    Context contexts[] = {
        {
            g_dnfBase + 0x02E6CF20,
            kGuardContext, sizeof(kGuardContext), "owner guard context"
        },
        {
            g_dnfBase + 0x02E6CF63,
            kOpenContext, sizeof(kOpenContext), "open context"
        },
        {
            g_dnfBase + 0x02E6D417,
            kCloseContextA, sizeof(kCloseContextA), "close context A"
        },
        {
            g_dnfBase + 0x02E6D493,
            kCloseContextB, sizeof(kCloseContextB), "close context B"
        },
    };
    struct Patch {
        uintptr_t target;
        const unsigned char* replacement;
        size_t length;
    };
    Patch patches[] = {
        {
            g_dnfBase + kPartyDirectoryOwnerGuardRva,
            kOwner9, sizeof(kOwner9)
        },
        {
            g_dnfBase + kPartyDirectoryOpenOwnerRva,
            kOwner9, sizeof(kOwner9)
        },
        {
            g_dnfBase + kPartyDirectoryCloseOwnerARva,
            kOwner9, sizeof(kOwner9)
        },
        {
            g_dnfBase + kPartyDirectoryCloseOwnerBRva,
            kOwner9, sizeof(kOwner9)
        },
    };

    for (size_t i = 0; i < _countof(contexts); ++i) {
        if (!BytesMatch(reinterpret_cast<unsigned char*>(contexts[i].target),
                contexts[i].expected, contexts[i].length)) {
            LogLine("party-directory full-page compatibility rejected: %s "
                "bytes mismatch at 0x%08X",
                contexts[i].label, contexts[i].target);
            return false;
        }
    }

    uintptr_t patchStart = patches[0].target;
    uintptr_t patchEnd =
        patches[_countof(patches) - 1].target +
        patches[_countof(patches) - 1].length;
    DWORD oldProtection = 0;
    if (!VirtualProtect(reinterpret_cast<void*>(patchStart),
            patchEnd - patchStart, PAGE_EXECUTE_READWRITE, &oldProtection)) {
        LogLine("party-directory full-page compatibility VirtualProtect failed: %u",
            GetLastError());
        return false;
    }
    for (size_t i = 0; i < _countof(patches); ++i) {
        memcpy(reinterpret_cast<void*>(patches[i].target),
            patches[i].replacement, patches[i].length);
    }
    DWORD ignoredProtection = 0;
    VirtualProtect(reinterpret_cast<void*>(patchStart),
        patchEnd - patchStart, oldProtection, &ignoredProtection);
    FlushInstructionCache(GetCurrentProcess(),
        reinterpret_cast<void*>(patchStart), patchEnd - patchStart);
    LogLine("party-directory full-page compatibility installed "
        "guard=0x%08X open=0x%08X close_a=0x%08X close_b=0x%08X",
        patches[0].target, patches[1].target,
        patches[2].target, patches[3].target);
    return true;
}

bool InstallAntonRaidModeSelectCompatibility()
{
    // VA 0x017B1E02:
    //   mov ecx, [0x51B2764]
    //   push 0
    //   push 0
    //   push 0x76
    //   call sub_2FDD1A0
    //   ret
    // Validate the complete native sequence before changing its one-byte UI
    // owner. This rejects a different DNF.exe instead of patching by RVA alone.
    static const unsigned char kNativeContext[] = {
        0x8B, 0x0D, 0x64, 0x27, 0x1B, 0x05,
        0x6A, 0x00, 0x6A, 0x00, 0x6A, kRaidCreateUiOwner,
        0xE8, 0x8D, 0xB3, 0x82, 0x01, 0xC3
    };
    static const unsigned char kInstalledContext[] = {
        0x8B, 0x0D, 0x64, 0x27, 0x1B, 0x05,
        0x6A, 0x00, 0x6A, 0x00, 0x6A, kRaidModeSelectUiOwner,
        0xE8, 0x8D, 0xB3, 0x82, 0x01, 0xC3
    };

    uintptr_t context = g_dnfBase + kAntonRaidModeSelectContextRva;
    uintptr_t owner = g_dnfBase + kAntonRaidModeSelectOwnerRva;
    if (BytesMatch(reinterpret_cast<unsigned char*>(context),
            kInstalledContext, sizeof(kInstalledContext))) {
        LogLine("Anton raid native mode-select UI already installed owner=0x%02X target=0x%08X",
            kRaidModeSelectUiOwner, owner);
        return true;
    }
    if (!BytesMatch(reinterpret_cast<unsigned char*>(context),
            kNativeContext, sizeof(kNativeContext))) {
        LogLine("Anton raid native mode-select UI rejected: context mismatch at 0x%08X",
            context);
        return false;
    }

    static const unsigned char kModeSelectOwner[] = { kRaidModeSelectUiOwner };
    if (!WriteCodePatch(owner, kModeSelectOwner, sizeof(kModeSelectOwner),
            "Anton raid native mode-select UI owner")) {
        return false;
    }
    LogLine("Anton raid native mode-select UI installed entry=0x%08X owner=0x%02X",
        context, kRaidModeSelectUiOwner);
    return true;
}

bool InstallRaidDirectoryOwnerNullGuard()
{
    // VA 0x01D4C390 in the native class-0/op884 handler. Validate the fixed
    // slot-15 resolver, returned owner assignment, helper call, and immediate
    // post-call use before replacing the nine-byte owner setup with a guarded
    // equivalent. This fails closed on any different DNF.exe.
    static const unsigned char kNativeContext[] = {
        0x6A, 0x0F,
        0xE8, 0xA9, 0x76, 0x04, 0x00,
        0x8B, 0xC8,
        0xE8, 0x22, 0x75, 0x04, 0x00,
        0x8B, 0xD8, 0x57, 0x57, 0x8B, 0xCB, 0x89, 0x5D, 0xB4,
        0xE8, 0xB4, 0x06, 0x61, 0x00,
        0x8B, 0xF0, 0x8B, 0xCE,
        0xE8, 0xBB, 0x01, 0x62, 0x00
    };

    uintptr_t context = g_dnfBase + kRaidDirectoryOwnerContextRva;
    uintptr_t target = g_dnfBase + kRaidDirectoryOwnerGuardRva;
    g_raidDirectoryOwnerPresentResume =
        g_dnfBase + kRaidDirectoryOwnerPresentResumeRva;
    g_raidDirectoryOwnerMissingResume =
        g_dnfBase + kRaidDirectoryOwnerMissingResumeRva;

    unsigned char guardPatch[9] = { 0xE9 };
    intptr_t relative =
        reinterpret_cast<intptr_t>(&ProxyRaidDirectoryOwnerGuard) -
        static_cast<intptr_t>(target + 5);
    *reinterpret_cast<int32_t*>(guardPatch + 1) =
        static_cast<int32_t>(relative);
    for (size_t i = 5; i < sizeof(guardPatch); ++i) {
        guardPatch[i] = 0x90;
    }
    if (BytesMatch(reinterpret_cast<unsigned char*>(target),
            guardPatch, sizeof(guardPatch))) {
        LogLine("Anton raid public-directory owner null guard already installed target=0x%08X",
            target);
        return true;
    }
    if (!BytesMatch(reinterpret_cast<unsigned char*>(context),
            kNativeContext, sizeof(kNativeContext))) {
        LogLine("Anton raid public-directory owner null guard rejected: context mismatch at 0x%08X",
            context);
        return false;
    }

    if (!WriteCodePatch(target, guardPatch, sizeof(guardPatch),
            "Anton raid public-directory owner null guard")) {
        return false;
    }
    LogLine("Anton raid public-directory owner null guard installed target=0x%08X present=0x%08X missing=0x%08X",
        target, g_raidDirectoryOwnerPresentResume,
        g_raidDirectoryOwnerMissingResume);
    return true;
}

// Pet transport stone (item 490700208). See the block above the two proxies for
// the full native trace and the two live probe readings that pin every address.
constexpr uintptr_t kCreatureTransportActionContextBeforeRva = 0x01A78DB7;
constexpr uintptr_t kCreatureTransportActionVectorCallRva = 0x01A78DC9;
constexpr uintptr_t kCreatureTransportActionVectorResumeRva = 0x01A78DCE;
constexpr uintptr_t kCreatureTransportActionRegistrySourceRva = 0x01F13210;
constexpr uintptr_t kCreatureTransportActionLookupGuardRva = 0x01A78E29;
constexpr uintptr_t kCreatureTransportActionLookupPresentResumeRva = 0x01A78E2E;
constexpr uintptr_t kCreatureTransportActionLookupMissingResumeRva = 0x01A78E84;
constexpr uintptr_t kCreatureTransportActionLocalDestructorRva = 0x022D0EA0;

bool InstallCreatureTransportStoneCompatibility()
{
    if (!g_dnfBase) return false;

    // The native window around both sites, copied out of the shipped client:
    // 0x1E78DB7..0x1E78DC8 stages the argument the transport object accessor is
    // called with, and 0x1E78DCE..0x1E78E28 is the vector count, the inlined
    // _STL_VERIFY, the registry lookup with its argument staging and the object
    // construction that precedes the guarded dereference. Any different
    // DNF.exe fails this and nothing is patched.
    static const unsigned char kCreatureTransportContextBefore[] = {
        0x6A, 0x01, 0x6A, 0x32, 0x8D, 0x78, 0x18, 0x6A, 0x04, 0x57, 0xE8, 0x0A,
        0xFA, 0xC6, 0x01, 0x8B, 0x3F, 0x57,
    };
    static const unsigned char kCreatureTransportContextAfter[] = {
        0x8B, 0x88, 0x24, 0x04, 0x00, 0x00, 0x8B, 0x80, 0x28, 0x04, 0x00, 0x00,
        0x2B, 0xC1, 0xC1, 0xF8, 0x02, 0x83, 0xC4, 0x14, 0x85, 0xC0, 0x75, 0x0A,
        0x68, 0x7C, 0x2D, 0x1D, 0x04, 0xE8, 0xD2, 0x2C, 0xC8, 0x01, 0x8B, 0x09,
        0x51, 0xE8, 0x48, 0x14, 0xAA, 0x01, 0x83, 0xC4, 0x04, 0x6A, 0x01, 0x8D,
        0x55, 0xE0, 0x52, 0x8B, 0xCE, 0x8B, 0xF8, 0xE8, 0x96, 0x6C, 0x1A, 0x01,
        0x8B, 0xC8, 0xE8, 0x7F, 0x72, 0x86, 0x00, 0xC7, 0x45, 0xFC, 0x08, 0x00,
        0x00, 0x00, 0x50, 0x8D, 0x45, 0xD8, 0x50, 0xE8, 0xCE, 0xDB, 0x8F, 0xFE,
        0x83, 0xC4, 0x08, 0xC6, 0x45, 0xFC, 0x09,
    };
    static const unsigned char kCreatureTransportVectorCallPrologue[] = {
        0xE8, 0x42, 0xA4, 0x49, 0x00,
    };
    static const unsigned char kCreatureTransportLookupGuardPrologue[] = {
        0x8B, 0x0F, 0x8B, 0x00, 0x51,
    };

    uintptr_t vectorFixTarget = g_dnfBase + kCreatureTransportActionVectorCallRva;
    uintptr_t lookupGuardTarget = g_dnfBase + kCreatureTransportActionLookupGuardRva;
    g_creatureTransportActionVectorSource =
        g_dnfBase + kCreatureTransportActionRegistrySourceRva;
    g_creatureTransportActionVectorResume =
        g_dnfBase + kCreatureTransportActionVectorResumeRva;
    g_creatureTransportActionLookupPresentResume =
        g_dnfBase + kCreatureTransportActionLookupPresentResumeRva;
    g_creatureTransportActionLookupMissingResume =
        g_dnfBase + kCreatureTransportActionLookupMissingResumeRva;
    g_creatureTransportActionLocalDestructor =
        g_dnfBase + kCreatureTransportActionLocalDestructorRva;

    unsigned char vectorFixPatch[5] = { 0xE9 };
    intptr_t relative =
        reinterpret_cast<intptr_t>(&ProxyCreatureTransportActionVectorFix) -
        static_cast<intptr_t>(vectorFixTarget + 5);
    *reinterpret_cast<int32_t*>(vectorFixPatch + 1) = static_cast<int32_t>(relative);

    unsigned char lookupGuardPatch[5] = { 0xE9 };
    relative =
        reinterpret_cast<intptr_t>(&ProxyCreatureTransportActionLookupGuard) -
        static_cast<intptr_t>(lookupGuardTarget + 5);
    *reinterpret_cast<int32_t*>(lookupGuardPatch + 1) = static_cast<int32_t>(relative);

    bool vectorFixInstalled = BytesMatch(
        reinterpret_cast<unsigned char*>(vectorFixTarget),
        vectorFixPatch, sizeof(vectorFixPatch));
    bool lookupGuardInstalled = BytesMatch(
        reinterpret_cast<unsigned char*>(lookupGuardTarget),
        lookupGuardPatch, sizeof(lookupGuardPatch));
    if (vectorFixInstalled && lookupGuardInstalled) {
        LogLine("pet transport stone compatibility already installed vector_fix=0x%08X lookup_guard=0x%08X",
            vectorFixTarget, lookupGuardTarget);
        return true;
    }

    if (!BytesMatch(
            reinterpret_cast<unsigned char*>(
                g_dnfBase + kCreatureTransportActionContextBeforeRva),
            kCreatureTransportContextBefore,
            sizeof(kCreatureTransportContextBefore)) ||
        !BytesMatch(
            reinterpret_cast<unsigned char*>(
                g_dnfBase + kCreatureTransportActionVectorResumeRva),
            kCreatureTransportContextAfter,
            sizeof(kCreatureTransportContextAfter)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(vectorFixTarget),
            kCreatureTransportVectorCallPrologue,
            sizeof(kCreatureTransportVectorCallPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(lookupGuardTarget),
            kCreatureTransportLookupGuardPrologue,
            sizeof(kCreatureTransportLookupGuardPrologue))) {
        LogLine("pet transport stone compatibility rejected: native context mismatch at 0x%08X / 0x%08X",
            vectorFixTarget, lookupGuardTarget);
        return false;
    }

    if (!WriteCodePatch(vectorFixTarget, vectorFixPatch, sizeof(vectorFixPatch),
            "pet transport stone collected action value normalisation") ||
        !WriteCodePatch(lookupGuardTarget, lookupGuardPatch, sizeof(lookupGuardPatch),
            "pet transport stone action registry lookup null guard")) {
        return false;
    }
    LogLine("pet transport stone compatibility installed vector_fix=0x%08X lookup_guard=0x%08X source=0x%08X present=0x%08X missing=0x%08X dtor=0x%08X unknown_value=%u known_value=%u",
        vectorFixTarget, lookupGuardTarget,
        g_creatureTransportActionVectorSource,
        g_creatureTransportActionLookupPresentResume,
        g_creatureTransportActionLookupMissingResume,
        g_creatureTransportActionLocalDestructor,
        kCreatureTransportActionValueUnknown,
        kCreatureTransportActionValueKnown);
    return true;
}

bool __fastcall ProxyChannelScriptDownload(void* self, void* /*unused*/)
{
    LogLine("channel script download enter self=%p", self);
    bool result = g_originalChannelScriptDownload
        ? g_originalChannelScriptDownload(self)
        : false;
    LogLine("channel script download return result=%d", result ? 1 : 0);
    return result;
}

bool __fastcall ProxyChannelScriptLoad(void* self, void* /*unused*/, const char* text)
{
    size_t length = 0;
    char hex[360] = { 0 };
    __try {
        if (text) {
            while (length < 0x80000 && text[length] != '\0') ++length;
            FormatHexBytes(text, static_cast<int>(length), hex, sizeof(hex));
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        length = 0;
        _snprintf(hex, sizeof(hex) - 1, "<read-exception-0x%08X>", GetExceptionCode());
    }
    LogLine("channel script load enter self=%p len=%u bytes=%s",
        self, static_cast<unsigned int>(length), hex);
    bool result = g_originalChannelScriptLoad
        ? g_originalChannelScriptLoad(self, text)
        : false;
    LogLine("channel script load return result=%d", result ? 1 : 0);
    return result;
}

void* __fastcall ProxyChannelRuntimeLoad(void* self, void* /*unused*/, const void* key)
{
    unsigned int serverID = 0;
    unsigned int channelID = 0;
    __try {
        if (key) {
            const unsigned int* values = static_cast<const unsigned int*>(key);
            serverID = values[0];
            channelID = values[1];
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        serverID = 0xFFFFFFFFu;
        channelID = 0xFFFFFFFFu;
    }

    void* scriptRecord = nullptr;
    unsigned int scriptType = 0xFFFFFFFFu;
    __try {
        if (serverID != 0xFFFFFFFFu && channelID != 0xFFFFFFFFu) {
            ChannelScriptLookupFn lookup = reinterpret_cast<ChannelScriptLookupFn>(
                g_dnfBase + kChannelScriptLookupRva);
            scriptRecord = lookup(reinterpret_cast<void*>(g_dnfBase + kChannelScriptStoreRva),
                static_cast<int>(serverID), static_cast<int>(channelID));
            if (scriptRecord) {
                scriptType = *reinterpret_cast<unsigned int*>(
                    static_cast<unsigned char*>(scriptRecord) + 0x20);
            }
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        scriptRecord = nullptr;
        scriptType = 0xFFFFFFFFu;
    }

    void* result = g_originalChannelRuntimeLoad
        ? g_originalChannelRuntimeLoad(self, key)
        : nullptr;
    LogLine("channel runtime load server=%u channel=%u script=%p type=%u result=%p",
        serverID, channelID, scriptRecord, scriptType, result);
    return result;
}

int __stdcall ProxyChannelCategoryInsert(int category, void* channel)
{
    unsigned int serverID = 0xFFFFFFFFu;
    unsigned int channelID = 0xFFFFFFFFu;
    unsigned int channelType = 0xFFFFFFFFu;
    __try {
        if (channel) {
            const unsigned int* fields = static_cast<const unsigned int*>(channel);
            serverID = fields[1];
            channelType = fields[2];
            channelID = fields[0x70 / sizeof(unsigned int)];
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
    }
    LogLine("channel category insert category=%d server=%u channel=%u type=%u object=%p",
        category, serverID, channelID, channelType, channel);
    return g_originalChannelCategoryInsert
        ? g_originalChannelCategoryInsert(category, channel)
        : 0;
}

bool StoreNativeHudState(uintptr_t descriptorRva, uintptr_t valueRva,
    uintptr_t lockRva, uintptr_t storeRva, int value)
{
    AtomicSpinFn lock = reinterpret_cast<AtomicSpinFn>(
        g_dnfBase + kAtomicSpinLockRva);
    AtomicSpinFn unlock = reinterpret_cast<AtomicSpinFn>(
        g_dnfBase + kAtomicSpinUnlockRva);
    ObfuscatedStoreFn store = reinterpret_cast<ObfuscatedStoreFn>(
        g_dnfBase + storeRva);
    void* descriptor = reinterpret_cast<void*>(g_dnfBase + descriptorRva);
    int* encryptedValue = reinterpret_cast<int*>(g_dnfBase + valueRva);
    void* stateLock = reinterpret_cast<void*>(g_dnfBase + lockRva);

    bool stored = false;
    lock(stateLock);
    __try {
        store(descriptor, &value, encryptedValue);
        stored = true;
    }
    __finally {
        unlock(stateLock);
    }
    return stored;
}

bool CommitNativeHudChannelState(int serverID, int channelID)
{
    if (serverID < 0 || channelID <= 0) {
        return false;
    }

    bool serverStored = StoreNativeHudState(
        kHudServerIndexDescriptorRva, kHudServerIndexValueRva,
        kHudServerIndexLockRva, kObfuscatedStoreCase4Rva, serverID);
    bool channelStored = StoreNativeHudState(
        kHudChannelIndexDescriptorRva, kHudChannelIndexValueRva,
        kHudChannelIndexLockRva, kObfuscatedStoreCase0Rva, channelID);
    LogLine("native channel HUD state committed server=%d channel=%d channel_store=%d server_store=%d",
        serverID, channelID, channelStored ? 1 : 0, serverStored ? 1 : 0);
    return channelStored && serverStored;
}

bool StoreNativeClockValue(
    uintptr_t descriptorRva, uintptr_t valueRva, uintptr_t storeRva, int value)
{
    ObfuscatedStoreFn store = reinterpret_cast<ObfuscatedStoreFn>(
        g_dnfBase + storeRva);
    void* descriptor = reinterpret_cast<void*>(g_dnfBase + descriptorRva);
    int* encryptedValue = reinterpret_cast<int*>(g_dnfBase + valueRva);
    store(descriptor, &value, encryptedValue);
    return true;
}

bool CommitNativeServerClockState()
{
    __time32_t currentTime = _time32(nullptr);
    if (currentTime <= 0) {
        LogLine("native server clock state skipped invalid unix time=%d",
            static_cast<int>(currentTime));
        return false;
    }

    DWORD tick = GetTickCount();
    typedef DWORD (WINAPI* TimeGetTimeFn)();
    HMODULE winmm = GetModuleHandleW(L"winmm.dll");
    TimeGetTimeFn timeGetTimeFn = winmm
        ? reinterpret_cast<TimeGetTimeFn>(
            GetProcAddress(winmm, "timeGetTime"))
        : nullptr;
    if (timeGetTimeFn) {
        tick = timeGetTimeFn();
    }

    bool serverStored = false;
    bool localStored = false;
    bool tickStored = false;
    __try {
        // Current NoPack sub_1D70130 initializes these three obfuscated
        // values from CHANNELINFO field 11, _time32, and timeGetTime. The
        // compatibility connection path commits the same native state after
        // a verified successful channel connection.
        serverStored = StoreNativeClockValue(
            kServerClockDescriptorRva, kServerClockValueRva,
            kObfuscatedStoreCase0Rva, static_cast<int>(currentTime));
        localStored = StoreNativeClockValue(
            kLocalClockDescriptorRva, kLocalClockValueRva,
            kObfuscatedStoreCase8Rva, static_cast<int>(currentTime));
        tickStored = StoreNativeClockValue(
            kServerTickDescriptorRva, kServerTickValueRva,
            kObfuscatedStoreCase0Rva, static_cast<int>(tick));
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("native server clock state failed with structured exception");
        return false;
    }

    LogLine("native server clock state committed unix=%d tick=%u server_store=%d local_store=%d tick_store=%d",
        static_cast<int>(currentTime), static_cast<unsigned int>(tick),
        serverStored ? 1 : 0, localStored ? 1 : 0, tickStored ? 1 : 0);
    return serverStored && localStored && tickStored;
}

bool CommitResidentChannelSnapshot(unsigned short connectedPort)
{
    if (!g_gameWideAssign) {
        LogLine("channel resident snapshot skipped: wide-string assign is unavailable");
        return false;
    }

    __try {
        void* selected = *reinterpret_cast<void**>(g_dnfBase + kSelectedChannelPointerRva);
        if (!selected) {
            LogLine("channel resident snapshot skipped: selected channel is null");
            return false;
        }

        uintptr_t* vtable = *reinterpret_cast<uintptr_t**>(selected);
        ChannelObjectPredicateFn predicateA =
            reinterpret_cast<ChannelObjectPredicateFn>(vtable[20 / sizeof(uintptr_t)]);
        ChannelObjectPredicateFn predicateB =
            reinterpret_cast<ChannelObjectPredicateFn>(vtable[24 / sizeof(uintptr_t)]);
        bool predicateAResult = predicateA(selected);
        bool predicateBResult = predicateB(selected);

        ChannelObjectNameFn getName =
            reinterpret_cast<ChannelObjectNameFn>(vtable[68 / sizeof(uintptr_t)]);
        ChannelObjectIntFn getServerID =
            reinterpret_cast<ChannelObjectIntFn>(vtable[76 / sizeof(uintptr_t)]);
        ChannelObjectIntFn getAddress =
            reinterpret_cast<ChannelObjectIntFn>(vtable[128 / sizeof(uintptr_t)]);
        ChannelObjectPortFn getPort =
            reinterpret_cast<ChannelObjectPortFn>(vtable[132 / sizeof(uintptr_t)]);

        const wchar_t* name = getName(selected);
        if (!name) {
            LogLine("channel resident snapshot skipped: selected=%p name is null", selected);
            return false;
        }
        size_t nameLength = wcsnlen(name, 256);
        if (nameLength == 256) {
            LogLine("channel resident snapshot skipped: selected=%p name is unterminated", selected);
            return false;
        }

        int serverID = getServerID(selected);
        int address = getAddress(selected);
        unsigned short port = getPort(selected);
        int channelID = *reinterpret_cast<int*>(
            static_cast<unsigned char*>(selected) + 0x70);
        unsigned int expectedPort = channelID > 0
            ? static_cast<unsigned int>(kGamePortBase) + static_cast<unsigned int>(channelID)
            : 0;
        if (channelID <= 0 || expectedPort > 0xFFFF ||
            port != static_cast<unsigned short>(expectedPort) ||
            (connectedPort != 0 && port != connectedPort)) {
            LogLine("channel resident snapshot skipped: selected=%p channel=%d port=%u expected_port=%u connected_port=%u predicate_a=%d predicate_b=%d",
                selected, channelID, static_cast<unsigned int>(port), expectedPort,
                static_cast<unsigned int>(connectedPort),
                predicateAResult ? 1 : 0, predicateBResult ? 1 : 0);
            return false;
        }
        void* resident = reinterpret_cast<void*>(g_dnfBase + kResidentChannelRva);
        g_gameWideAssign(resident, name, nameLength);
        *reinterpret_cast<int*>(static_cast<unsigned char*>(resident) + 32) = address;
        *reinterpret_cast<int*>(static_cast<unsigned char*>(resident) + 36) =
            static_cast<int>(port);

        if (!CommitNativeHudChannelState(serverID, channelID)) {
            LogLine("channel resident snapshot skipped HUD commit server=%d channel=%d",
                serverID, channelID);
        }

        LogLine("channel resident snapshot committed selected=%p server=%d channel=%d name_length=%u address=0x%08X port=%u predicate_a=%d predicate_b=%d",
            selected, serverID, channelID, static_cast<unsigned int>(nameLength),
            static_cast<unsigned int>(address),
            static_cast<unsigned int>(port), predicateAResult ? 1 : 0, predicateBResult ? 1 : 0);
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("channel resident snapshot failed with structured exception");
        return false;
    }
}

bool __fastcall ProxyChannelConnect(void* self, void* /*unused*/, int address, unsigned short port)
{
    unsigned short targetPort = port;
    // Was DNF_RESIDENT_CHANNEL_ID (default 11). That launcher override is gone,
    // so the redirect always uses the constant at the top of this file; a value
    // of 0 keeps it off, exactly as it did before.
    constexpr unsigned long channelID = kResidentChannelID;
    const char* configurationSource = channelID > 0 ? "configuration" : "disabled";

    if (port == kCrackBootstrapPort && channelID > 0) {
        unsigned long candidatePort = static_cast<unsigned long>(kGamePortBase) + channelID;
        if (candidatePort <= 0xFFFF) {
            targetPort = static_cast<unsigned short>(candidatePort);
            LogLine("channel bootstrap redirect source=%s source_port=%u target_channel=%lu target_port=%u",
                configurationSource, static_cast<unsigned int>(port), channelID,
                static_cast<unsigned int>(targetPort));
        }
    }

    bool result = g_originalChannelConnect
        ? g_originalChannelConnect(self, address, targetPort)
        : false;
    LogLine("channel connect address=0x%08X requested_port=%u effective_port=%u result=%d",
        static_cast<unsigned int>(address), static_cast<unsigned int>(port),
        static_cast<unsigned int>(targetPort), result ? 1 : 0);
    return result;
}

bool __cdecl ProxyChannelDirectoryApply()
{
    LogLine("channel directory apply enter");
    bool result = g_originalChannelDirectoryApply
        ? g_originalChannelDirectoryApply()
        : false;
    LogLine("channel directory apply return result=%d", result ? 1 : 0);
    return result;
}

bool IsPvpChannelQueryCall(uintptr_t callerRva, unsigned int requestedType)
{
    return (callerRva == kPvpQueryType8ReturnRva && requestedType == 8) ||
        ((callerRva == kPvpQueryType31ReturnRvaA ||
             callerRva == kPvpQueryType31ReturnRvaB) &&
            requestedType == 31) ||
        ((callerRva == kPvpQueryType41ReturnRvaA ||
             callerRva == kPvpQueryType41ReturnRvaB) &&
            requestedType == 41);
}

unsigned int* __stdcall ProxyChannelQuery(unsigned int* output, int type)
{
    uintptr_t callerRva =
        reinterpret_cast<uintptr_t>(_ReturnAddress()) - g_dnfBase;
    unsigned int requestedType = static_cast<unsigned int>(type);
    bool remapped = IsPvpChannelQueryCall(callerRva, requestedType);
    int queryType = remapped ? 24 : type;
    unsigned int* result = g_originalChannelQuery
        ? g_originalChannelQuery(output, queryType)
        : output;
    if (!remapped) {
        return result;
    }

    unsigned int count = 0;
    __try {
        if (output && output[2] >= output[1]) {
            count = (output[2] - output[1]) / sizeof(void*);
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        count = 0xFFFFFFFFu;
    }
    LogLine("pvp channel query remapped caller_rva=0x%08X requested_type=%u effective_type=24 count=%u output=%p",
        callerRva, requestedType, count, output);
    return result;
}

// ---------------------------------------------------------------------------
// 首次选角资源加载的空 this 守卫。
//
// 崩溃现场（2026-09-15 00:02:08，客户端启动后第一次进入角色）：
//     exception code=0xC0000005 access_address=0x00001840 eip=0x017D6C0E
//     ecx=0x00000000  ->  crash eip DNF.exe+0x013D6C0E
// 该地址位于 sub_13D6BE0 内部，指令为
//     0x013D6C0E  mov ebx, dword ptr [ecx + 0x1840]
// 即 this==0 时读取成员。函数自身**已经**防御了「成员为空」这一层：
//     0x013D6C17  test ebx, ebx
//     0x013D6C19  je   0x013D762F        ; 原生收尾：不进入函数体，eax 保持不变
// 却没有防御 this==0。本守卫把同一层防御往上补一级：ecx==0 时按完全相同的
// 「对象不存在」语义直接收尾返回，不进入函数体，也不改动 eax。
//
// 该函数在整个 DNF.exe 中没有任何立即数引用（call / jmp / push offset /
// mov reg,offset 全部为 0 处），只能通过运行时构造的指针被间接调用，因此无法在
// 调用方下钩，只能在入口拦。函数体 0x013D6BE0..0x013D7640 只有一个 `ret`
// （__thiscall 且无栈参数），所以守卫直接 ret 即可，无需模仿 epilogue 的 ret N。
// ---------------------------------------------------------------------------
constexpr uintptr_t kSelectLoadNullThisTargetRva = 0x013D6BE0;
void* g_selectLoadNullThisResume = nullptr;
void(__cdecl* g_selectLoadNullThisRecorder)(uintptr_t) = nullptr;
volatile LONG g_selectLoadNullThisHits = 0;
uintptr_t g_selectLoadNullThisCallers[8] = {};
int g_selectLoadNullThisCallerCount = 0;

void __cdecl RecordSelectLoadNullThisCaller(uintptr_t returnAddress)
{
    const LONG hits = InterlockedIncrement(&g_selectLoadNullThisHits);
    if (g_dnfBase != 0 && returnAddress > g_dnfBase) {
        returnAddress -= g_dnfBase;
    }
    for (int index = 0; index < g_selectLoadNullThisCallerCount; ++index) {
        if (g_selectLoadNullThisCallers[index] == returnAddress) {
            return;
        }
    }
    if (g_selectLoadNullThisCallerCount >=
            static_cast<int>(sizeof(g_selectLoadNullThisCallers) /
                sizeof(g_selectLoadNullThisCallers[0]))) {
        return;
    }
    g_selectLoadNullThisCallers[g_selectLoadNullThisCallerCount++] = returnAddress;
    LogLine("select-load null-this guard hit caller_rva=0x%08X hits=%u (native resource load skipped)",
        static_cast<unsigned>(returnAddress), static_cast<unsigned>(hits));
}

__declspec(naked) void ProxySelectLoadNullThisGuard()
{
    __asm {
        test ecx, ecx
        jz select_load_null_this

        jmp dword ptr [g_selectLoadNullThisResume]

    select_load_null_this:
        push eax
        push ecx
        push edx
        mov eax, dword ptr [esp + 12]
        push eax
        call dword ptr [g_selectLoadNullThisRecorder]
        add esp, 4
        pop edx
        pop ecx
        pop eax
        ret
    }
}

bool InstallSelectLoadNullThisGuard()
{
    if (!g_dnfBase) return false;

    static const unsigned char kSelectLoadPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    uintptr_t target = g_dnfBase + kSelectLoadNullThisTargetRva;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
            kSelectLoadPrologue, sizeof(kSelectLoadPrologue))) {
        LogLine("select-load null-this guard target verification failed target=0x%08X",
            target);
        return false;
    }

    g_selectLoadNullThisRecorder = &RecordSelectLoadNullThisCaller;
    if (!InstallKnownInlineHook(target, kSelectLoadPrologue,
            sizeof(kSelectLoadPrologue),
            reinterpret_cast<void*>(&ProxySelectLoadNullThisGuard),
            &g_selectLoadNullThisResume,
            "select-load null-this guard")) {
        return false;
    }
    LogLine("select-load null-this guard installed target=0x%08X rva=0x%08X",
        target, kSelectLoadNullThisTargetRva);
    return true;
}

// ---------------------------------------------------------------------------
// 同一根因的第二个消费点：sub_21AC970 —— "这个指针在不在该对象的列表里"。
//
//     0x021AC970  push ebp
//     0x021AC971  mov  ebp, esp
//     0x021AC973  mov  eax, dword ptr [ecx + 0x10]   <<< 崩溃点(ecx=0)
//     0x021AC977  mov  esi, dword ptr [ecx + 0xc]
//                 ... esi..eax 之间按 4 字节步长线性查找 [ebp+8]
//     0x021AC9A4  ret  4      ; 返回 al = 是否找到
//
// 首次选角时同一个对象可能还是 0：此时"对象不在列表里"就是正确语义
// （客户端第一段自己的 test eax,eax / je 空检查也说明作者本意是空即跳过），
// 因此 ecx==0 时直接按原生约定返回 al=0（未找到），保留 ret 4 的栈清理。
//
// 该函数同样没有任何静态调用点（运行时指针间接调用），只能在入口拦。
// 序言前 6 字节被偷：55 8B EC 8B 41 10 —— 必须整条覆盖 mov eax,[ecx+0x10]，
// 只偷 5 字节会把该指令劈开，trampoline 回跳会落在指令中间。
// ---------------------------------------------------------------------------
constexpr uintptr_t kActorListContainsGuardRva = 0x021AC970;
void* g_actorListContainsResume = nullptr;
volatile LONG g_actorListContainsHits = 0;

__declspec(naked) void ProxyActorListContainsGuard()
{
    __asm {
        test ecx, ecx
        jz actor_list_null_container

        jmp dword ptr [g_actorListContainsResume]

    actor_list_null_container:
        lock inc dword ptr [g_actorListContainsHits]
        xor al, al
        ret 4
    }
}

bool InstallActorListContainsGuard()
{
    if (!g_dnfBase) return false;

    static const unsigned char kActorListContainsPrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x41, 0x10
    };
    uintptr_t target = g_dnfBase + kActorListContainsGuardRva;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
            kActorListContainsPrologue, sizeof(kActorListContainsPrologue))) {
        LogLine("actor-list contains null guard target verification failed target=0x%08X",
            target);
        return false;
    }

    if (!InstallKnownInlineHook(target, kActorListContainsPrologue,
            sizeof(kActorListContainsPrologue),
            reinterpret_cast<void*>(&ProxyActorListContainsGuard),
            &g_actorListContainsResume,
            "actor-list contains null guard")) {
        return false;
    }
    LogLine("actor-list contains null guard installed target=0x%08X rva=0x%08X",
        target, kActorListContainsGuardRva);
    return true;
}

bool InstallChannelUiCompatibility()
{
    static const unsigned char kSehPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    uintptr_t queryTarget = g_dnfBase + kPvpChannelQueryRva;

    if (!BytesMatch(reinterpret_cast<unsigned char*>(queryTarget),
            kSehPrologue, sizeof(kSehPrologue))) {
        LogLine("channel UI compatibility target verification failed query=0x%08X",
            queryTarget);
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(queryTarget, kSehPrologue,
            sizeof(kSehPrologue), reinterpret_cast<void*>(&ProxyChannelQuery),
            &original, "pvp channel query remap")) {
        return false;
    }
    g_originalChannelQuery = reinterpret_cast<ChannelQueryFn>(original);

    LogLine("channel UI compatibility installed query=0x%08X pvp_call_sites=5 effective_type=24 query_type_width=32",
        queryTarget);
    return true;
}

// ---------------------------------------------------------------------------
// 区服名解析探针。
//
// sub_2272670(regionID) 返回该区服在客户端里的显示名。 好友、公会、交易这类「社交」
// 界面把它拼成「<区服名>-<角色名>」；原生资源表对查询到的 ID 返回空串时，界面上就
// 只剩一个孤零零的 `-`。 实测：两个客户端的交易窗口都显示 `-1111` / `-2222`，而
// 服务端的区服身份一直是 region=1，且客户端日志显示补丁处于
// 「custom region name disabled; native resource table retained」。
//
// 本探针只**观察**，不改语义：先照原样调用原生解析器，把 (id, 结果, 长度) 记一行
// 日志，然后再套用运营商覆盖。 覆盖存在时仍返回覆盖名；不存在时仍返回原生结果
// (原生返回空指针时回落到 g_customRegionName，与旧实现一致)。 每个 regionID 只记
// 一次，避免逐帧刷日志。
// ---------------------------------------------------------------------------
constexpr int kRegionNameProbeMaxIDs = 32;
int g_regionNameProbeIDs[kRegionNameProbeMaxIDs] = {};
int g_regionNameProbeCount = 0;

int BoundedWideLength(const wchar_t* value, int limit)
{
    if (value == nullptr) {
        return -1;
    }
    int length = 0;
    while (length < limit && value[length] != L'\0') {
        ++length;
    }
    return length;
}

void LogRegionNameResolution(int regionID, const wchar_t* resolved)
{
    for (int index = 0; index < g_regionNameProbeCount; ++index) {
        if (g_regionNameProbeIDs[index] == regionID) {
            return;
        }
    }
    if (g_regionNameProbeCount >= kRegionNameProbeMaxIDs) {
        return;
    }
    g_regionNameProbeIDs[g_regionNameProbeCount++] = regionID;

    const int length = BoundedWideLength(resolved, 64);
    char text[192] = {};
    if (resolved == nullptr || length < 0) {
        _snprintf(text, sizeof(text) - 1, "<null>");
    } else {
        char utf8[160] = {};
        int converted = WideCharToMultiByte(CP_UTF8, 0, resolved, length, utf8,
            static_cast<int>(sizeof(utf8)) - 1, nullptr, nullptr);
        if (converted < 0) {
            converted = 0;
        }
        utf8[converted] = '\0';
        _snprintf(text, sizeof(text) - 1, "%s", utf8);
    }
    LogLine("region-name probe id=%d length=%d resolved=\"%s\" custom_id=%d custom_configured=%d",
        regionID, length, text, g_customRegionID, g_customRegionName[0] ? 1 : 0);
}

// 探针实测（2026-09-11，两个客户端一致）：
//     region-name probe id=1 length=3 resolved="广东1"
//     region-name probe id=0 length=0 resolved=""
// id=0 是客户端保留的资源表项（go-server 的 options.go 也写着 "Zero is also kept
// out of operator configuration because it is a client-reserved resource-table
// entry"），原生解析器对它就返回空串。 客户端用「<区服名>-<角色名>」拼社交标签，
// 前缀空掉之后就只剩一个孤零零的 `-`（交易申请/交易窗口/物品数量提示都是它）。
//
// 因此覆盖规则是两条，而不只是 id 相等：
//   1. 命中运营商配置的 id → 直接返回配置名；
//   2. 原生结果为 NULL 或空串 → 同样回落到配置名，保证社交标签始终有前缀。
// 原生表能给出名字的其它 id 一律保持原样，行为不变。
wchar_t* __cdecl ProxyRegionNameResolver(int regionID)
{
    wchar_t* resolved = g_originalRegionNameResolver
        ? g_originalRegionNameResolver(regionID)
        : nullptr;
    LogRegionNameResolution(regionID, resolved);
    if (g_customRegionName[0] != L'\0') {
        if (regionID == g_customRegionID) {
            return g_customRegionName;
        }
        if (resolved == nullptr || resolved[0] == L'\0') {
            return g_customRegionName;
        }
    }
    return resolved != nullptr ? resolved : g_customRegionName;
}

// The region name and id this patch publishes.
//
// They used to be overridable at runtime -- DNF_REGION_NAME / DNF_REGION_ID from
// the launcher, then <DNF.exe dir>\90CN_region.ini, then these two constants.
// This DLL reads neither an environment variable nor an INI file any more (see
// clientConfig at the top of this file), so the constants are the only source.
// Renaming the server means editing them and rebuilding.
constexpr wchar_t kBuiltInRegionName[] = L"奇迹暖暖";
constexpr int kBuiltInRegionID = 1;

bool InstallCustomRegionNameCompatibility()
{
    wchar_t configuredName[_countof(g_customRegionName)] = {};
    if (kBuiltInRegionName[0] == L'\0') {
        LogLine("custom region name disabled; native resource table retained");
        return true;
    }
    wcsncpy_s(configuredName, _countof(configuredName), kBuiltInRegionName, _TRUNCATE);
    const int configuredRegionID = kBuiltInRegionID;
    const DWORD nameLength = static_cast<DWORD>(wcslen(configuredName));

    static const unsigned char kResolverPrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08
    };
    uintptr_t target = g_dnfBase + kRegionNameResolverRva;
    void* original = nullptr;
    if (!InstallKnownInlineHook(
            target,
            kResolverPrologue,
            sizeof(kResolverPrologue),
            reinterpret_cast<void*>(&ProxyRegionNameResolver),
            &original,
            "custom region name resolver")) {
        return false;
    }
    g_originalRegionNameResolver =
        reinterpret_cast<RegionNameResolverFn>(original);
    g_customRegionID = configuredRegionID;
    wcsncpy_s(
        g_customRegionName,
        _countof(g_customRegionName),
        configuredName,
        _TRUNCATE);
    LogLine("custom region name resolver installed target=0x%08X region=%d name_length=%u source=builtin",
        target, g_customRegionID, static_cast<unsigned int>(nameLength));
    return true;
}

// ---------------------------------------------------------------------------
// 鍏細 (guild) compatibility, ported from the upstream guild branch.
//
// Six independent installs plus two packet consumers. Every install verifies its
// target bytes first and refuses (log + false) on mismatch, matching the rest of
// this file. The two consumers use the same "consume and return 1" shape as
// TryConsumePrivateMoodProjection / TryConsumePrivateBossMutationDecision, so a
// packet that belongs to this DLL never reaches the native handler.
// ---------------------------------------------------------------------------

static SRWLOCK g_guildRankLock = SRWLOCK_INIT;
static GuildRankSnapshot g_guildRankSnapshot = {};

// Private class0/413 subtype4 version1. The gate is deliberately strict: msgID
// 413, declared total length 28, then b[16]==4 && b[17]==1. The body sits at
// offset 16 because the deployed instance.json uses the 16-byte gameUpperHeader
// ("server16"), and 16 + the 12-byte body is exactly the 28 the gate wants.
//
// A packet that clears the gate but fails to decode is still CONSUMED: it is ours
// either way, and handing an unknown 413 to the native handler is what we are
// trying to avoid.
bool TryConsumePrivateGuildRanking(int packet)
{
    __try {
        if (!packet) return false;
        const unsigned char* bytes = reinterpret_cast<const unsigned char*>(packet);
        if (*reinterpret_cast<const unsigned short*>(bytes + 1) != 413 ||
            *reinterpret_cast<const unsigned int*>(bytes + 3) != 28 ||
            bytes[16] != 4 || bytes[17] != 1) return false;
        GuildRankSnapshot value = {};
        if (!DecodeGuildRankBody(bytes + 16, 12, &value)) return true;
        AcquireSRWLockExclusive(&g_guildRankLock);
        g_guildRankSnapshot = value;
        ReleaseSRWLockExclusive(&g_guildRankLock);
        LogLine("guild experience rank received character=%u rank=%u experience=%u",
                value.character, value.rank, value.experience);
        return true;
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("guild rank packet exception=0x%08X", GetExceptionCode());
        return true;
    }
}

// The native guild_ranking label has no packet setter. This runs from the naked
// frame proxy just before the native getter and repaints that label, but only
// when the snapshot addresses the actor this client is currently controlling --
// otherwise the rank of somebody else's character would be shown as ours.
void __stdcall UpdateNativeGuildRanking(void* window)
{
    GuildRankSnapshot value = {};
    AcquireSRWLockShared(&g_guildRankLock);
    value = g_guildRankSnapshot;
    ReleaseSRWLockShared(&g_guildRankLock);
    __try {
        if (!window) return;
        void* label = *reinterpret_cast<void**>(static_cast<unsigned char*>(window) + 0x2E8);
        if (!label) return;
        void* manager = *reinterpret_cast<void**>(g_dnfBase + kObjectManagerPointerRva);
        CurrentActorFn getActor = reinterpret_cast<CurrentActorFn>(g_dnfBase + kCurrentActorRva);
        void* actor = manager ? getActor(manager) : nullptr;
        wchar_t text[24] = L"-";
        if (actor && value.character == *reinterpret_cast<const uint16_t*>(
                static_cast<const unsigned char*>(actor) + 12) && value.rank) {
            _snwprintf_s(text, _countof(text), _TRUNCATE, L"%u", value.rank);
        }
        typedef void (__thiscall* SetTextFn)(void*, const wchar_t*);
        SetTextFn setText = *reinterpret_cast<SetTextFn*>(
            *reinterpret_cast<uintptr_t*>(label) + 0xE0);
        setText(label, text);
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        // UI teardown can coincide with a refresh. The following native frame
        // retains ownership of the window; no cached UI pointers are stored.
    }
}

// Class1 guild-war refusals: an 18-byte class1 packet with b[16]==0 whose b[17] is
// one of the native codes 240..255. The native path discards these, so the player
// would see nothing at all; OpenGuildWarErrorNotice opens the native
// announcement window (owner 0x298) with the matching Chinese text instead.
bool TryConsumeGuildWarErrorNotice(int packet)
{
    if (!packet || !g_dnfBase || !g_originalSceneUiOpen) return false;
    __try {
        const unsigned char* b = reinterpret_cast<const unsigned char*>(packet);
        if (OpenGuildWarErrorNotice(b, *reinterpret_cast<const unsigned int*>(b + 3),
                *reinterpret_cast<void**>(g_dnfBase + kSceneRootPointerRva),
                reinterpret_cast<GuildWarNoticeOpenFn>(g_originalSceneUiOpen))) {
            LogLine("guild war error native announcement opcode=%u code=%u owner=0x298",
                *reinterpret_cast<const uint16_t*>(b + 1), b[17]);
            return true;
        }
        return false;
    } __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

// 鍏細鍒涘缓鍖烘湇 0 瑙ｉ攣. The directory already advertises region 0; only the guild
// UI excluded it. 0x2D52113 is mov edi,1 -> mov edi,0; 0x2D54ED5 widens the
// accepted range from 0..248 to 0..249. The second write rolls the first back.
bool InstallGuildCreateRegionCompatibility()
{
    static const unsigned char kLoopBefore[] = { 0xBF, 1, 0, 0, 0 };
    static const unsigned char kLoopAfter[] = { 0xBF, 0, 0, 0, 0 };
    static const unsigned char kPermitBefore[] = { 0x8D, 0x47, 0xFF, 0x3D, 0xF8, 0, 0, 0 };
    static const unsigned char kPermitAfter[] = { 0x8B, 0xC7, 0x90, 0x3D, 0xF9, 0, 0, 0 };
    uintptr_t loop = g_dnfBase + 0x2D52113;
    uintptr_t permit = g_dnfBase + 0x2D54ED5;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(loop), kLoopBefore, sizeof(kLoopBefore)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(permit), kPermitBefore, sizeof(kPermitBefore))) {
        LogLine("guild create region compatibility signature mismatch");
        return false;
    }
    if (!WriteCodePatch(loop, kLoopAfter, sizeof(kLoopAfter), "guild region loop")) {
        return false;
    }
    if (!WriteCodePatch(permit, kPermitAfter, sizeof(kPermitAfter), "guild permit region")) {
        WriteCodePatch(loop, kLoopBefore, sizeof(kLoopBefore), "guild region rollback");
        return false;
    }
    LogLine("guild create regions 0..249 enabled; native directory membership and names retained");
    return true;
}

bool InstallGuildMemberActorCompatibility()
{
    // The upstream branch reads an optional DNF_REGION_ID environment variable
    // here and falls back to 1. This tree deliberately removed environment and INI
    // configuration (see the region constants above InstallCustomRegionNameCompat),
    // so the built-in constant is the only source. It must still land in the
    // 1..249 band the social commands use, or the install is refused outright
    // rather than binding the local actor to a region nobody can address.
    if (kBuiltInRegionID < 1 || kBuiltInRegionID >= 250) {
        LogLine("guild member actor binding rejected out-of-range built-in region id=%d",
            kBuiltInRegionID);
        return false;
    }
    g_guildMemberLocalRegion = kBuiltInRegionID;
    // Exact op67/140 parser call, with its region push and member+60 store.
    static const unsigned char expected[] = {
        0x52, 0x89, 0x85, 0xB8, 0xFD, 0xFF, 0xFF,
        0xE8, 0xEA, 0x77, 0x97, 0x00,
        0x8B, 0x95, 0x60, 0xFD, 0xFF, 0xFF,
        0x8B, 0x8D, 0x50, 0xFD, 0xFF, 0xFF,
        0x89, 0x85, 0xBC, 0xFD, 0xFF, 0xFF
    };
    static const unsigned char refreshExpected[] = {
        0x50, 0x0F, 0xB6, 0x86, 0x82, 0x01, 0x00, 0x00, 0x50,
        0xE8, 0x57, 0x2B, 0xFF, 0xFF, 0x89, 0x46, 0x3C, 0x85, 0xC0
    };
    uintptr_t target = g_dnfBase + 0x192231A;
    uintptr_t refresh = g_dnfBase + 0x22A6FAB;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target), expected, sizeof(expected)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(refresh), refreshExpected, sizeof(refreshExpected))) {
        LogLine("guild member actor binding signature mismatch");
        return false;
    }
    g_guildMemberActorLookup = reinterpret_cast<GuildMemberActorLookupFn>(g_dnfBase + 0x2299B10);
    unsigned char call[] = { 0xE8, 0, 0, 0, 0 };
    *reinterpret_cast<int32_t*>(call + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(&ProxyGuildMemberActorLookup) -
        static_cast<intptr_t>(target + 12));
    if (!WriteCodePatch(target + 7, call, sizeof(call), "guild member local actor binding")) {
        return false;
    }
    // sub_26A3990 immediately refreshes every parsed row via virtual +12.
    // sub_26A6F80 repeats the lookup and overwrites member+60; it must use
    // the same local binding rule, including later native member refreshes.
    *reinterpret_cast<int32_t*>(call + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(&ProxyGuildMemberActorLookup) -
        static_cast<intptr_t>(refresh + 14));
    if (!WriteCodePatch(refresh + 9, call, sizeof(call), "guild member refreshed local actor binding")) {
        WriteCodePatch(target + 7, expected + 7, sizeof(call), "guild member binding rollback");
        return false;
    }
    LogLine("guild member local actor binding installed social_region=%d scene_region=0 calls=0x%08X,0x%08X",
        g_guildMemberLocalRegion, target + 7, refresh + 9);
    return true;
}

bool InstallGuildMemberMenuCompatibility()
{
    static const unsigned char expected[] = {
        0x8B, 0xCB, 0xE8, 0x25, 0x72, 0x43, 0xFF, 0x89, 0x46, 0x4C
    };
    uintptr_t target = g_dnfBase + 0x2E59694;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target), expected, sizeof(expected))) {
        LogLine("guild member menu signature mismatch");
        return false;
    }
    unsigned char call[] = { 0xE8, 0, 0, 0, 0 };
    *reinterpret_cast<int32_t*>(call + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(&ProxyGuildMenuActorRegion) -
        static_cast<intptr_t>(target + 7));
    return WriteCodePatch(target + 2, call, sizeof(call), "guild member menu null actor region");
}

bool InstallGuildWarRowsCompatibility()
{
    static const unsigned char expected[] = {
        0xB9,0x0C,0,0,0,0x8D,0x7D,0x8C,0xF3,0xA5,0x8B,0xCB,0xA4,
        0xE8,0x59,0x71,0xB7,0,0x8B,0x80,0x41,0x2E,0,0,0x39,0x45,0x8C,
        0x8B,0x5D,0x84,0x8B,0x8B,0x54,7,0,0,0x0F,0x94,0x45,0x8B
    };
    static const unsigned char epilogue[] = {
        0x8B,0x4D,0xF4,0x64,0x89,0x0D,0,0,0,0,0x59,0x5F,0x5E,0x5B,
        0x8B,0x4D,0xF0,0x33,0xCD,0xE8,0x2B,0xB0,0xDF,2,0x8B,0xE5,0x5D,0xC2,0x10,0
    };
    const uintptr_t context = g_dnfBase + 0x914F75;
    const uintptr_t finish = g_dnfBase + 0x9152D1;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(context), expected, sizeof(expected)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(finish), epilogue, sizeof(epilogue))) {
        LogLine("guild war empty row signature mismatch");
        return false;
    }
    const uintptr_t target = context + 18;
    g_guildWarRowResume = target + 9;
    g_guildWarRowReturn = finish;
    unsigned char patch[] = {0xE9,0,0,0,0,0x90,0x90,0x90,0x90};
    *reinterpret_cast<int32_t*>(patch + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(&ProxyGuildWarRow) - static_cast<intptr_t>(target + 5));
    return WriteCodePatch(target, patch, sizeof(patch), "guild war empty rows");
}

bool InstallGuildRankingCompatibility()
{
    static const unsigned char expected[] = {
        0x8B,0x9E,0x70,0x02,0x00,0x00,0x85,0xDB,0x74,0x18,
        0x8B,0x8D,0x7C,0xFF,0xFF,0xFF,0x8B,0x3B,0xE8,0x19,
        0x92,0x6F,0x01,0x50,0x8B,0x87,0xE0,0x00,0x00,0x00,
        0x8B,0xCB,0xFF,0xD0
    };
    const uintptr_t context = g_dnfBase + 0xBB5480;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(context), expected, sizeof(expected))) {
        LogLine("guild ranking frame signature mismatch");
        return false;
    }
    g_originalGuildRankFrameGetter = reinterpret_cast<void*>(g_dnfBase + 0x22AE6B0);
    g_guildRankFrameUpdate = &UpdateNativeGuildRanking;
    unsigned char patch[] = {0xE8,0,0,0,0};
    const uintptr_t target = context + 18;
    *reinterpret_cast<int32_t*>(patch + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(&ProxyGuildRankFrame) - static_cast<intptr_t>(target + 5));
    return WriteCodePatch(target, patch, sizeof(patch), "guild experience ranking frame");
}

// Only sub_33CA2C0 state 6, shared by street and guild assault results.
bool InstallAssaultResultCompatibility()
{
    static const unsigned char expected[] = {
        0x6A, 0, 0x6A, 0, 0x8B, 0xCE, 0xE8, 0x82, 0x42, 0x26, 0xFF,
        0x8B, 0xC8, 0xE8, 0xBB, 0x2B, 0x0E, 0xFF, 0x8B, 0xCE,
        0xE8, 0x74, 0x42, 0x26, 0xFF
    };
    uintptr_t context = g_dnfBase + 0x2FCA933;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(context), expected, sizeof(expected))) {
        LogLine("assault result signature mismatch");
        return false;
    }
    g_assaultActorReset = reinterpret_cast<AssaultActorReset>(g_dnfBase + 0x20AD500);
    uintptr_t target = context + 13;
    unsigned char call[] = { 0xE8, 0, 0, 0, 0 };
    *reinterpret_cast<int32_t*>(call + 1) = static_cast<int32_t>(
        reinterpret_cast<intptr_t>(&ProxyAssaultResultActorReset) -
        static_cast<intptr_t>(target + sizeof(call)));
    return WriteCodePatch(target, call, sizeof(call), "assault result preserve dead actor");
}

bool InstallChannelDiagnostic()
{
    static const unsigned char kSehPrologue[] = { 0x55, 0x8B, 0xEC, 0x6A, 0xFF };
    uintptr_t downloadTarget = g_dnfBase + kChannelScriptDownloadRva;
    uintptr_t loadTarget = g_dnfBase + kChannelScriptLoadRva;
    uintptr_t directoryApplyTarget = g_dnfBase + kChannelDirectoryApplyRva;
    uintptr_t runtimeLoadTarget = g_dnfBase + kChannelRuntimeLoadRva;
    uintptr_t categoryInsertTarget = g_dnfBase + kChannelCategoryInsertRva;
    uintptr_t connectTarget = g_dnfBase + kChannelConnectRva;
    if (!BytesMatch(reinterpret_cast<unsigned char*>(downloadTarget), kSehPrologue, sizeof(kSehPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(loadTarget), kSehPrologue, sizeof(kSehPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(directoryApplyTarget), kSehPrologue, sizeof(kSehPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(runtimeLoadTarget), kSehPrologue, sizeof(kSehPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(categoryInsertTarget), kSehPrologue, sizeof(kSehPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(connectTarget), kSehPrologue, sizeof(kSehPrologue))) {
        LogLine("channel diagnostic target verification failed download=0x%08X load=0x%08X apply=0x%08X runtime=0x%08X category=0x%08X connect=0x%08X",
            downloadTarget, loadTarget, directoryApplyTarget, runtimeLoadTarget,
            categoryInsertTarget, connectTarget);
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(connectTarget, kSehPrologue, sizeof(kSehPrologue),
            reinterpret_cast<void*>(&ProxyChannelConnect), &original,
            "channel connect")) return false;
    g_originalChannelConnect = reinterpret_cast<ChannelConnectFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(runtimeLoadTarget, kSehPrologue, sizeof(kSehPrologue),
            reinterpret_cast<void*>(&ProxyChannelRuntimeLoad), &original,
            "channel runtime load")) return false;
    g_originalChannelRuntimeLoad = reinterpret_cast<ChannelRuntimeLoadFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(categoryInsertTarget, kSehPrologue, sizeof(kSehPrologue),
            reinterpret_cast<void*>(&ProxyChannelCategoryInsert), &original,
            "channel category insert")) return false;
    g_originalChannelCategoryInsert = reinterpret_cast<ChannelCategoryInsertFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(directoryApplyTarget, kSehPrologue, sizeof(kSehPrologue),
            reinterpret_cast<void*>(&ProxyChannelDirectoryApply), &original,
            "channel directory apply")) return false;
    g_originalChannelDirectoryApply = reinterpret_cast<ChannelDirectoryApplyFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(loadTarget, kSehPrologue, sizeof(kSehPrologue),
            reinterpret_cast<void*>(&ProxyChannelScriptLoad), &original,
            "channel script load")) return false;
    g_originalChannelScriptLoad = reinterpret_cast<ChannelScriptLoadFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(downloadTarget, kSehPrologue, sizeof(kSehPrologue),
            reinterpret_cast<void*>(&ProxyChannelScriptDownload), &original,
            "channel script download")) return false;
    g_originalChannelScriptDownload = reinterpret_cast<ChannelScriptDownloadFn>(original);
    return true;
}

bool InstallTclsCompatibility()
{
    static const unsigned char kSehPrologue[] = { 0x55, 0x8B, 0xEC, 0x6A, 0xFF };
    static const unsigned char kFetchTextPrologue[] = { 0x55, 0x8B, 0xEC, 0x81, 0xEC, 0x08, 0x01, 0x00, 0x00 };
    static const unsigned char kFetchTailPrologue[] = { 0x55, 0x8B, 0xEC, 0x81, 0xEC, 0x8C, 0x04, 0x00, 0x00 };

    uintptr_t parseTarget = g_dnfBase + kTclsParseRva;
    uintptr_t loginTarget = g_dnfBase + kTclsFetchLoginRva;
    uintptr_t textTarget = g_dnfBase + kTclsFetchTextRva;
    uintptr_t cryptoTarget = g_dnfBase + kTclsFetchCryptoRva;
    uintptr_t fetchTailTarget = g_dnfBase + kTclsFetchTailRva;
    uintptr_t tailTarget = g_dnfBase + kTclsTailRva;
    g_gameWideAssign = reinterpret_cast<GameWideAssignFn>(g_dnfBase + kGameWideAssignRva);

    // Validate the complete set before changing any entry point.  These are
    // current-EXE local launch helpers, never send/recv or packet parsers.
    if (!BytesMatch(reinterpret_cast<unsigned char*>(parseTarget), kSehPrologue, sizeof(kSehPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(loginTarget), kSehPrologue, sizeof(kSehPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(textTarget), kFetchTextPrologue, sizeof(kFetchTextPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(cryptoTarget), kSehPrologue, sizeof(kSehPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(fetchTailTarget), kFetchTailPrologue, sizeof(kFetchTailPrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(tailTarget), kSehPrologue, sizeof(kSehPrologue))) {
        LogLine("TCLS current-EXE target verification failed; no TCLS hook installed");
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(loginTarget, kSehPrologue, sizeof(kSehPrologue),
            reinterpret_cast<void*>(&ProxyTclsFetchLogin), &original, "tcls fetch login")) return false;
    g_originalTclsFetchLogin = reinterpret_cast<TclsFetchLoginFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(textTarget, kFetchTextPrologue, sizeof(kFetchTextPrologue),
            reinterpret_cast<void*>(&ProxyTclsFetchText), &original, "tcls fetch text")) return false;
    g_originalTclsFetchText = reinterpret_cast<TclsFetchTextFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(cryptoTarget, kSehPrologue, sizeof(kSehPrologue),
            reinterpret_cast<void*>(&ProxyTclsFetchCrypto), &original, "tcls fetch crypto")) return false;
    g_originalTclsFetchCrypto = reinterpret_cast<TclsFetchOneFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(fetchTailTarget, kFetchTailPrologue, sizeof(kFetchTailPrologue),
            reinterpret_cast<void*>(&ProxyTclsFetchTail), &original, "tcls fetch tail")) return false;
    g_originalTclsFetchTail = reinterpret_cast<TclsFetchOneFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(tailTarget, kSehPrologue, sizeof(kSehPrologue),
            reinterpret_cast<void*>(&ProxyTclsTail), &original, "tcls tail")) return false;
    g_originalTclsTail = reinterpret_cast<TclsTailFn>(original);

    // Parse is installed last because all fetches occur recursively inside
    // the original parse call and must already have valid trampolines.
    original = nullptr;
    if (!InstallKnownInlineHook(parseTarget, kSehPrologue, sizeof(kSehPrologue),
            reinterpret_cast<void*>(&ProxyTclsParse), &original, "tcls parse")) return false;
    g_originalTclsParse = reinterpret_cast<TclsParseFn>(original);

    LogLine("TCLS local launch compatibility installed (six native helpers)");
    return true;
}

bool InstallCodecSetKeyTrace()
{
    static const unsigned char kSetKey0Prologue[] = { 0x55, 0x8B, 0xEC, 0x8B, 0x49, 0x08 };
    static const unsigned char kSetKey2Prologue[] = { 0x55, 0x8B, 0xEC, 0x8B, 0x51, 0x08 };
    static const unsigned char kSetKey3Prologue[] = { 0x55, 0x8B, 0xEC, 0x56, 0x8B, 0xF1 };
    static const unsigned char kSetKey7Prologue[] = { 0x55, 0x8B, 0xEC, 0x6A, 0xFF };
    static const unsigned char kSetKey8Prologue[] = { 0x55, 0x8B, 0xEC, 0x56, 0x8B, 0xF1 };

    uintptr_t target0 = g_dnfBase + kCodecSetKey0Rva;
    uintptr_t target2 = g_dnfBase + kCodecSetKey2Rva;
    uintptr_t target3 = g_dnfBase + kCodecSetKey3Rva;
    uintptr_t target7 = g_dnfBase + kCodecSetKey7Rva;
    uintptr_t target8 = g_dnfBase + kCodecSetKey8Rva;

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(target0), kSetKey0Prologue, sizeof(kSetKey0Prologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(target2), kSetKey2Prologue, sizeof(kSetKey2Prologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(target3), kSetKey3Prologue, sizeof(kSetKey3Prologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(target7), kSetKey7Prologue, sizeof(kSetKey7Prologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(target8), kSetKey8Prologue, sizeof(kSetKey8Prologue))) {
            break;
        }
        Sleep(50);
        waitedMs += 50;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("codec setkey trace targets did not reach verified bytes; no setkey hooks installed");
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(target0, kSetKey0Prologue, sizeof(kSetKey0Prologue),
            reinterpret_cast<void*>(&ProxyCodecSetKey0), &original, "codec setkey idx0")) return false;
    g_originalCodecSetKey0 = reinterpret_cast<CodecSetKeyFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(target2, kSetKey2Prologue, sizeof(kSetKey2Prologue),
            reinterpret_cast<void*>(&ProxyCodecSetKey2), &original, "codec setkey idx2")) return false;
    g_originalCodecSetKey2 = reinterpret_cast<CodecSetKeyFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(target3, kSetKey3Prologue, sizeof(kSetKey3Prologue),
            reinterpret_cast<void*>(&ProxyCodecSetKey3), &original, "codec setkey idx3")) return false;
    g_originalCodecSetKey3 = reinterpret_cast<CodecSetKeyFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(target7, kSetKey7Prologue, sizeof(kSetKey7Prologue),
            reinterpret_cast<void*>(&ProxyCodecSetKey7), &original, "codec setkey idx7")) return false;
    g_originalCodecSetKey7 = reinterpret_cast<CodecSetKeyFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(target8, kSetKey8Prologue, sizeof(kSetKey8Prologue),
            reinterpret_cast<void*>(&ProxyCodecSetKey8), &original, "codec setkey idx8")) return false;
    g_originalCodecSetKey8 = reinterpret_cast<CodecSetKeyFn>(original);

    LogLine("codec setkey trace installed wait_ms=%d targets=0x%08X,0x%08X,0x%08X,0x%08X,0x%08X",
        waitedMs, target0, target2, target3, target7, target8);
    return true;
}

int __cdecl TryUpperBodyEncodeBypass(int messageType, char* input, int bodyLength, char* output, int* outLength)
{
    LONG callNumber = InterlockedIncrement(&g_cipherCallCount);
    int capacity = 0;
    __try {
        capacity = outLength ? *outLength : 0;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        capacity = 0;
    }

    int emitLength = bodyLength;
    if (messageType == 4) emitLength = 16;
    if (messageType != 4 && messageType != 5) {
        return 0;
    }
    if (!input || !output || !outLength || bodyLength <= 0 || bodyLength > 0x200 || capacity < emitLength) {
        if (callNumber <= 32) {
            LogLine("upper-body-encode-bypass-skip call=%ld msg=%d len=%d cap=%d emit=%d input=%p output=%p out_len=%p",
                callNumber, messageType, bodyLength, capacity, emitLength, input, output, outLength);
        }
        return 0;
    }

    __try {
        if (messageType == 4) {
            memset(output, 0, 16);
            int copyLength = bodyLength < 16 ? bodyLength : 16;
            memcpy(output, input, static_cast<size_t>(copyLength));
        } else {
            memcpy(output, input, static_cast<size_t>(bodyLength));
        }
        *outLength = emitLength;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("upper-body-encode-bypass exception call=%ld msg=%d len=%d code=0x%08X",
            callNumber, messageType, bodyLength, GetExceptionCode());
        return 0;
    }

    if (callNumber <= 64) {
        char hex[360] = { 0 };
        FormatHexBytes(output, emitLength, hex, sizeof(hex));
        LogLine("upper-body-encode-bypass call=%ld msg=%d len=%d emit_len=%d cap=%d body=%s",
            callNumber, messageType, bodyLength, emitLength, capacity, hex);
    }
    return 1;
}

unsigned int __cdecl ReadSelectedQuestFromAutoCompleteCallback(void* callback)
{
    unsigned int selector = 0;
    __try {
        if (callback) {
            selector = *reinterpret_cast<unsigned int*>(
                static_cast<unsigned char*>(callback) + 0x30);
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("quest-auto-complete selected read exception callback=%p code=0x%08X",
            callback, GetExceptionCode());
        return 0;
    }

    LONG callNumber = InterlockedIncrement(&g_questAutoCompleteCallCount);
    LogLine("quest-auto-complete selected call=%ld callback=%p selector=%u mode=%s",
        callNumber, callback, selector, selector ? "single" : "native-zero-fallback");
    return selector;
}

// The current task window captures the selected row's exact quest id at
// callback+0x30 in sub_304E070, but its confirmation callback discards that
// value and calls sub_19EE300(mode=1, selector=-1), which serializes u32(0).
// Rewrite only this verified call-site to the native single-row mode.  When a
// selector cannot be read, preserve the original zero-selector call so the
// server's ambiguity guard fails closed without mutating any quest.
__declspec(naked) void ProxyQuestAutoCompleteSelected()
{
    __asm {
        push ecx
        push edx
        push edi
        call ReadSelectedQuestFromAutoCompleteCallback
        add esp, 4
        pop edx
        pop ecx

        test eax, eax
        jz quest_auto_complete_fallback
        mov dword ptr [esp + 4], 0
        mov dword ptr [esp + 8], eax

    quest_auto_complete_fallback:
        jmp dword ptr [g_questAutoCompleteSendFn]
    }
}

// Function-head proxy for sub_33AAFE0.  This matches the current-EXE universal
// upper-body codec boundary: the caller has already prepared the packet header
// immediately before `input`.  Copy the semantic plaintext body to the native
// output buffer, repair the total length and checksum, and return success using
// the native 5-stack-argument cleanup contract.  This covers the whole
// opcode%14 codec family without per-opcode filters or socket hooks.
__declspec(naked) void ProxyCipherEncodeFunction()
{
    __asm {
        // Preserve the native register/flags contract while recording the
        // plaintext semantic body before it is copied into the send buffer.
        pushfd
        pushad
        mov eax, [esp + 28h]      // messageType
        mov ecx, [esp + 2Ch]      // input
        mov edx, [esp + 30h]      // bodyLength
        push edx
        push ecx
        push eax
        call LogClientToServerPacket
        add esp, 0Ch
        popad
        popfd

        push esi
        push edi
        push ebx

        // Entry stack before preserving registers:
        // [esp+04]=msg, [esp+08]=input, [esp+0C]=bodyLength,
        // [esp+10]=output, [esp+14]=outLength*
        mov ebx, [esp + 14h]      // input after three pushes
        mov ecx, [esp + 18h]      // bodyLength
        test ecx, ecx
        jle cipher_zero_case

        mov esi, ebx
        mov edi, [esp + 1Ch]      // output
        rep movsb                 // memcpy(output, input, bodyLength)

        mov eax, [esp + 20h]      // outLength*
        mov ecx, [esp + 18h]
        mov [eax], ecx

        lea edx, [ecx + 0Dh]
        mov [ebx - 0Ah], edx      // packet total length = bodyLength + 13

        lea eax, [ecx + 02h]
        push eax                  // crc length = bodyLength + sequence(2)
        lea eax, [ebx - 02h]
        push eax                  // crc begins at sequence
        call dword ptr [g_checksumFn]
        add esp, 08h
        mov [ebx - 06h], eax

        mov eax, 1
        pop ebx
        pop edi
        pop esi
        ret 14h

    cipher_zero_case:
        mov eax, [esp + 20h]
        mov dword ptr [eax], 0
        mov eax, 1
        pop ebx
        pop edi
        pop esi
        ret 14h
    }
}

// Function-head proxy for sub_33AB0A0, the current-EXE S2C upper-body decode
// boundary.  The server-side chain is intentionally plaintext while this DLL
// is active, so copy the received semantic body into the native output buffer
// and report success using the same 5-stack-argument cleanup contract.  No
// packet headers, socket bytes, opcodes, or routing decisions are touched here.
__declspec(naked) void ProxyCipherDecodeFunction()
{
    __asm {
        // Log the exact plaintext body handed to the native S2C dispatcher.
        pushfd
        pushad
        mov eax, [esp + 28h]      // messageType
        mov ecx, [esp + 2Ch]      // input
        mov edx, [esp + 30h]      // bodyLength
        push edx
        push ecx
        push eax
        call LogServerToClientPacket
        add esp, 0Ch
        popad
        popfd

        push esi
        push edi
        push ebx

        // Entry stack before preserving registers:
        // [esp+04]=msg, [esp+08]=input, [esp+0C]=bodyLength,
        // [esp+10]=output, [esp+14]=outLength*
        mov ebx, [esp + 14h]      // input after three pushes
        mov ecx, [esp + 18h]      // bodyLength
        test ecx, ecx
        jle cipher_decode_zero_case

        mov esi, ebx
        mov edi, [esp + 1Ch]      // output
        rep movsb                 // memcpy(output, input, bodyLength)

        mov eax, [esp + 20h]      // outLength*
        mov ecx, [esp + 18h]
        mov [eax], ecx

        mov eax, 1
        pop ebx
        pop edi
        pop esi
        ret 14h

    cipher_decode_zero_case:
        mov eax, [esp + 20h]
        mov dword ptr [eax], 0
        mov eax, 1
        pop ebx
        pop edi
        pop esi
        ret 14h
    }
}

// DPROTO compatibility mode, outbound direction.
//
// This is entered after sub_34738E0 has finalized the ordinary inner packet.
// The original function would call the mode-7 client protector and wrap the
// result as op1517.  We instead clone that finalized packet into the existing
// output ownership contract, for every opcode.  There is deliberately no
// opcode comparison, allow list, network hook, or captured body involved.
__declspec(naked) void ProxyDprotoSendDirect()
{
    __asm {
        // Load the opcode only for log/probe parity with the historical PS1
        // trampoline. Do not let it affect the cloned packet pointer.
        movzx eax, word ptr [esi + 1]

        // The native CRC call immediately before this entry is cdecl; discard
        // its two arguments exactly as the original path does at 0x3473924.
        add esp, 8

        // EDI = finalized inner packet length; ESI = source inner packet.
        push edi
        call dword ptr [g_gameAllocatorFn]
        add esp, 4
        test eax, eax
        jz dproto_oom

        // memcpy(destination=eax, source=esi, length=edi)
        push edi
        push esi
        push eax
        call dword ptr [g_gameMemcpyFn]
        add esp, 0Ch

        // Restore the sequence expected by upper_pkt_flush_send13 and
        // recompute the copied packet's checksum over sequence + body.
        mov dx, word ptr [ebp + 10h]
        mov word ptr [eax + 0Bh], dx
        push eax
        lea edx, [eax + 0Bh]
        mov ecx, edi
        sub ecx, 0Bh
        push ecx
        push edx
        call dword ptr [g_checksumFn]
        add esp, 8
        pop edx
        mov dword ptr [edx + 7], eax

        mov ecx, dword ptr [ebp + 14h]
        mov dword ptr [ecx], edx
        jmp dword ptr [g_dprotoOutgoingReturn]

    dproto_oom:
        mov edx, dword ptr [ebp + 14h]
        mov dword ptr [edx], 0
        jmp dword ptr [g_dprotoOutgoingReturn]
    }
}

// Predicate-false DPROTO compatibility. The native branch at 0x34738FA skips
// the TerSafe wrapper for some requests; only commands proven by the PS1 route
// are cloned here. Every other opcode resumes the native false epilogue.
__declspec(naked) void ProxyDprotoFalseSelective()
{
    __asm {
        movzx eax, word ptr [esi + 1]

        cmp ax, 64
        je dproto_false_clone
        cmp ax, 16
        je dproto_false_clone
        cmp ax, 19
        je dproto_false_clone
        cmp ax, 28
        je dproto_false_clone
        cmp ax, 29
        je dproto_false_clone
        cmp ax, 31
        je dproto_false_clone
        cmp ax, 33
        je dproto_false_clone
        cmp ax, 34
        je dproto_false_clone
        cmp ax, 35
        je dproto_false_clone
        cmp ax, 36
        je dproto_false_clone
        cmp ax, 39
        je dproto_false_clone
        cmp ax, 40
        je dproto_false_clone
        cmp ax, 41
        je dproto_false_clone
        cmp ax, 42
        je dproto_false_clone
        cmp ax, 43
        je dproto_false_clone
        cmp ax, 44
        je dproto_false_clone
        cmp ax, 45
        je dproto_false_clone
        cmp ax, 46
        je dproto_false_clone
        cmp ax, 71
        je dproto_false_clone
        cmp ax, 72
        je dproto_false_clone
        cmp ax, 117
        je dproto_false_clone
        cmp ax, 123
        je dproto_false_clone
        cmp ax, 132
        je dproto_false_clone
        cmp ax, 143
        je dproto_false_clone
        cmp ax, 491
        je dproto_false_clone
        cmp ax, 1437
        je dproto_false_clone
        cmp ax, 1443
        je dproto_false_clone
        cmp ax, 1456
        je dproto_false_clone
        cmp ax, 999
        je dproto_false_clone
        cmp ax, 1000
        je dproto_false_clone

        jmp dword ptr [g_dprotoFalseResume]

    dproto_false_clone:
        push edi
        mov edi, dword ptr [ebp + 0Ch]

        push edi
        call dword ptr [g_gameAllocatorFn]
        add esp, 4
        test eax, eax
        jz dproto_false_oom

        // memcpy(destination=eax, source=[ebp+08h], length=edi)
        push edi
        mov edx, dword ptr [ebp + 08h]
        push edx
        push eax
        call dword ptr [g_gameMemcpyFn]
        add esp, 0Ch

        // False path packets store total length at +3, then sequence/CRC.
        mov dword ptr [eax + 03h], edi
        mov dx, word ptr [ebp + 10h]
        mov word ptr [eax + 0Bh], dx
        mov esi, eax

        lea edx, [edi - 0Bh]
        push edx
        lea edx, [esi + 0Bh]
        push edx
        call dword ptr [g_checksumFn]
        add esp, 8
        mov dword ptr [esi + 07h], eax

        mov ecx, dword ptr [ebp + 14h]
        mov dword ptr [ecx], esi
        pop edi
        jmp dword ptr [g_dprotoFalseResume]

    dproto_false_oom:
        pop edi
        jmp dword ptr [g_dprotoFalseResume]
    }
}

bool __stdcall IsSceneRouteOpcodeAllowed(uint16_t opcode)
{
    switch (opcode) {
    // Login / character / scene bootstrap packets that are built by the Go
    // server with current-EXE bodies.
    case 2:
    case 3:
    case 4:
    case 5:
    case 6:
    case 7:
    case 8:
    case 9:
    // Current EXE class1 handlers sub_1D11A80/sub_1D12480 own the town peer
    // request/response handshake used by the remote-player context menu.
    case 10:
    case 11:
    case 12:
    case 13:
    case 14:
    case 15:
    case 16:
    case 18:
    // Current EXE class0/op19 installs the complete learned-skill trees and
    // quick slots. The same opcode's class1 handler owns MOVE_ITEMSPACE ACKs
    // and performs the native equipment/avatar/creature slot relocation.
    case 19:
    case 20:
    case 21:
    case 22:
    case 23:
    case 24:
    // Current EXE class-1/op25 owns the native design-recipe result dialog.
    // The server follows it with class-0/op14 authoritative item rows.
    case 25:
    case 26:
    case 27:
    case 28:
    case 29:
    case 30:
    case 31:
    case 32:
    case 33:
    case 34:
    case 35:
    case 36:
    case 37:
    case 38:
    case 39:
    case 40:
    case 41:
    case 42:
    case 43:
    case 44:
    case 45:
    case 46:
    // CmdPacketDropItem ACK clears the current EXE's pending-discard action.
    // Without direct-route admission the packet reaches the decoded S2C
    // boundary but never enters the registered class-1 handler, suppressing
    // later manual pickup and discard input for the rest of the room.
    case 47:
    case 48:
    case 53:
    case 63:
    case 64:
    case 66:
    case 80:
    case 81: // CmdPacketResetItemAttr - equipment grade-adjust result popup
    // CmdPacketCreatePrivateStore enters native scene/input state 18 before
    // sending. Current NoPack sub_1D0B630 owns the class-1 result and begins by
    // restoring state 0 through sub_30405F0 -> sub_2FDEF50. If op85 is left on
    // the historical route predicate, the verified one-byte success can be
    // discarded before the registered handler runs, freezing movement and
    // preventing the client from ever sending op87 for the store editor.
    case 85:
    case 94:
    case 95:
    case 96:
    case 99:
    case 100:
    case 101:
    case 102:
    // Current NoPack sub_1D57AB0 consumes scene class0/op105 as the
    // creature-status table. Without route admission, slot26 relocation can
    // succeed through op19 while the creature UI remains null/uninitialized.
    case 105:
    // Current NoPack sub_20457F0 owns the activity descriptor and
    // sub_E713E0 owns event progress. Event 2347 lazily creates the cumulative
    // online-time UI from this ordered op108 -> op1206 pair.
    case 108:
    case 115:
    case 120:
    case 124:
    case 132:
    case 138:
    case 143:
    case 149:
    case 153:
    case 160:
    case 166:
    case 173:
    case 174:
    case 191:
    case 200:
    case 201:
    case 202:
    case 206:
    case 208:
    case 237:
    case 256:
    case 260:
    case 267:
    case 268:
    case 269:
    case 272:
    case 273:
    case 295:
    case 305:
    case 306:
    case 307:
    case 308:
    case 331:
    case 332:
    case 356:
    case 357:
    case 358:
    case 359:
    case 376:
    case 391:
    case 392:
    case 400:
    case 412:
    case 413:
    case 448:
    case 491:
    case 518:
    case 523:
    case 558:
    case 559:
    case 560:
    case 574:
	case 87:  // CmdPacketCompleteDisplay - ordinary party-directory snapshot
	case 753: // Native Find Raid object page (sub_1D7AA10)
	case 884: // Generic cross-channel page container
    // Anton public-team flow. These are native current-client raid messages.
    // The map/participation packets must use this same direct route; otherwise
    // the raid object exists but the portal falls through to ordinary maps.
    case 583: // RAID_DO_BEHAVIOR
    case 584: // RAID_SET_SYMBOL
    case 585: // RAID_DUNGEON_PARTICIPATION_INFO
    case 586: // RAID_DUNGEON_STATE
    case 587: // RAID_CHANGE_DUNGEON_STATE
    case 588: // RAID_STATE
    case 589: // RAID_MESSAGE
    case 590: // RAID_MEMBER_STATE
    case 591: // RAID_MEMBER_REFRESH
    case 592: // RAID_MODIFY/CREATE
    case 593: // RAID_WAITING_LIST
    case 594: // RAID_WAITING_MODIFY
    case 595: // RAID_PREPARE_START
    case 596: // START_RAID
    case 597: // RAID_READY/ATTACK_TIMER
    case 598: // RAID_REMAIN_TIME
    case 599: // RAID_ENTRY_COST
    case 600: // RAID_MOVIE_SKIP
    case 601: // RAID_REWARD_LIST
    case 602: // RAID_RESULT
    case 627: // CLEAR_RAID_BUFF
    case 634: // RAID_BUFF_STATE
    case 635: // RAID_MONSTER_HP
    // Matching class-1 responses used by the current online raid handlers.
    case 658: // RAID_ENTRY_COST_INFO
    case 659: // RAID_MOVIE_SKIP
    case 660: // SELECT_RAID_REWARD_CARD
    case 661: // RAID_DO_BEHAVIOR
    case 662: // RAID_SET_SYMBOL
    case 663: // RAID_MESSAGE
    case 664: // CREATE_RAID
    case 665: // LEAVE_RAID
    case 666: // START_RAID
    case 667: // SET_RAID_WAITING
    case 668: // REJOIN_RAID
    case 669: // RAID_MANAGER_WORK
    case 670: // MODIFY_RAID_INFO
    case 694: // RAID_BUFF_SYSTEM
    case 695: // RAID_MONSTER_HP
    case 823: // RAID_MEMBER_CHANGE_STATE
    // REQUEST_RAID_INFO is the created-raid lookup path handled by
    // sub_1D3A150 -> sub_A42AB0. Public-directory cooldown initialization is
    // the distinct native op1228 route and must remain on the EXE predicate.
    case 1359: // REQUEST_RAID_INFO
    case 1367: // REQUEST_RAID_ENTRANCE_INFO
    case 692:
    // NPC shops containing [daily purchase limit] products issue op715 before
    // opening. Current NoPack sub_1D0E810 consumes the ACK as four u32-counted
    // purchase maps; it must reach that native handler instead of the fallback.
    case 715: // SHOP_PURCHASE_COUNT
    case 734:
    case 800:
    // The current collection-box handlers own the state table and expiry cache.
    case 896:
    case 897:
    case 1027:
    case 899:
    case 913:
    case 914: // CmdPacketAddEquipmentSocket — equipment socket open ACK
    // Weapon-effect rune success ACK. The native handler applies the selected
    // rune to the target weapon from the acknowledged request body; blocking
    // this opcode at the DPROTO route predicate leaves the server mutation
    // durable but prevents the client from updating its equipped item object.
    case 951: // CmdPacketAddEquipmentEffect
    case 985:
    case 993:
    case 994:
    case 997:
    case 1128: // CmdPacketUseRandomboxItemExpand — random-box ten-open result UI
    case 1206:
    case 1220:
    case 1307:
    case 1340:
    case 1346:
    case 1371:
    case 1378:
    case 1403:
    case 1425:
    case 1429:
    case 1430:
    case 1437:
    case 1443:
        return true;
    default:
        return false;
    }
}

// Current-scene inbound route compatibility. This is deliberately narrower
// than ProxyDprotoAcceptDirect: proven scene/UI opcodes are admitted to the
// direct upper reader, and every other opcode tail-calls the native predicate.
// The opcode argument is at [esp+4], matching the live PS1 probe.
__declspec(naked) void ProxySceneRouteAllowList()
{
    __asm {
        movzx eax, word ptr [esp + 4]
        push ecx
        push eax
        call IsSceneRouteOpcodeAllowed
        pop ecx
        test al, al
        jnz scene_route_allow

        // Native fallback: routeObject = [ecx+0x0c], then tail-call vtable+0x40.
        mov ecx, dword ptr [ecx + 0Ch]
        mov eax, dword ptr [ecx]
        mov eax, dword ptr [eax + 40h]
        jmp eax

    scene_route_allow:
        mov eax, 1
        ret 4
    }
}

bool BytesMatch(const unsigned char* address, const unsigned char* expected, size_t count)
{
    __try {
        for (size_t i = 0; i < count; ++i) {
            if (address[i] != expected[i]) return false;
        }
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        return false;
    }
}

bool TryReadByte(const unsigned char* address, unsigned char* value)
{
    __try {
        *value = *address;
        return true;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        *value = 0;
        return false;
    }
}

bool WriteCodePatch(uintptr_t target, const unsigned char* patch, size_t count, const char* label)
{
    DWORD oldProtection = 0;
    if (!VirtualProtect(reinterpret_cast<void*>(target), count, PAGE_EXECUTE_READWRITE, &oldProtection)) {
        LogLine("%s VirtualProtect failed: %u", label, GetLastError());
        return false;
    }

    memcpy(reinterpret_cast<void*>(target), patch, count);
    DWORD ignoredProtection = 0;
    VirtualProtect(reinterpret_cast<void*>(target), count, oldProtection, &ignoredProtection);
    FlushInstructionCache(GetCurrentProcess(), reinterpret_cast<void*>(target), count);
    return true;
}

bool HookMainModuleImport(const char* targetDll, const char* targetName, void* detour, void** original)
{
    if (!targetDll || !targetName || !detour || !original) return false;
    HMODULE module = GetModuleHandleW(nullptr);
    if (!module) return false;

    __try {
        auto* dos = reinterpret_cast<IMAGE_DOS_HEADER*>(module);
        if (dos->e_magic != IMAGE_DOS_SIGNATURE) return false;
        auto* nt = reinterpret_cast<IMAGE_NT_HEADERS*>(
            reinterpret_cast<unsigned char*>(module) + dos->e_lfanew);
        if (nt->Signature != IMAGE_NT_SIGNATURE) return false;

        IMAGE_DATA_DIRECTORY importDirectory =
            nt->OptionalHeader.DataDirectory[IMAGE_DIRECTORY_ENTRY_IMPORT];
        if (!importDirectory.VirtualAddress || !importDirectory.Size) {
            LogLine("iat hook skipped name=%s reason=no-import-directory", targetName);
            return false;
        }

        auto* descriptor = reinterpret_cast<IMAGE_IMPORT_DESCRIPTOR*>(
            reinterpret_cast<unsigned char*>(module) + importDirectory.VirtualAddress);
        for (; descriptor->Name; ++descriptor) {
            const char* dllName = reinterpret_cast<const char*>(
                reinterpret_cast<unsigned char*>(module) + descriptor->Name);
            if (!dllName || _stricmp(dllName, targetDll) != 0) continue;

            auto* nameThunk = reinterpret_cast<IMAGE_THUNK_DATA*>(
                reinterpret_cast<unsigned char*>(module) + descriptor->OriginalFirstThunk);
            auto* addressThunk = reinterpret_cast<IMAGE_THUNK_DATA*>(
                reinterpret_cast<unsigned char*>(module) + descriptor->FirstThunk);
            if (!descriptor->OriginalFirstThunk) nameThunk = addressThunk;

            for (; nameThunk->u1.AddressOfData && addressThunk->u1.Function; ++nameThunk, ++addressThunk) {
                if (IMAGE_SNAP_BY_ORDINAL(nameThunk->u1.Ordinal)) continue;
                auto* importByName = reinterpret_cast<IMAGE_IMPORT_BY_NAME*>(
                    reinterpret_cast<unsigned char*>(module) + nameThunk->u1.AddressOfData);
                const char* importName = reinterpret_cast<const char*>(importByName->Name);
                if (!importName || strcmp(importName, targetName) != 0) continue;

                void* current = reinterpret_cast<void*>(addressThunk->u1.Function);
                if (current == detour) {
                    LogLine("iat hook already installed name=%s slot=%p", targetName, addressThunk);
                    return true;
                }

                DWORD oldProtection = 0;
                if (!VirtualProtect(&addressThunk->u1.Function, sizeof(addressThunk->u1.Function),
                        PAGE_EXECUTE_READWRITE, &oldProtection)) {
                    LogLine("iat hook protect failed name=%s slot=%p gle=%lu",
                        targetName, addressThunk, GetLastError());
                    return false;
                }

                *original = current;
                addressThunk->u1.Function = reinterpret_cast<ULONG_PTR>(detour);
                DWORD ignoredProtection = 0;
                VirtualProtect(&addressThunk->u1.Function, sizeof(addressThunk->u1.Function),
                    oldProtection, &ignoredProtection);
                FlushInstructionCache(GetCurrentProcess(), &addressThunk->u1.Function,
                    sizeof(addressThunk->u1.Function));
                LogLine("iat hook name=%s dll=%s slot=%p old=%p detour=%p",
                    targetName, dllName, addressThunk, current, detour);
                return true;
            }
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("iat hook exception name=%s code=0x%08X", targetName, GetExceptionCode());
        return false;
    }

    LogLine("iat hook skipped name=%s dll=%s reason=not-imported", targetName, targetDll);
    return false;
}


// ==================== 90 remote-mode LANA authentication ====================
// dnfbridge challenges each game connection with a LANA frame before the DNF
// protocol starts. The launcher injects DNF_REMOTE_AUTH_TICKET / _HOST /
// _PORTS / _MACHINE; this narrowly-scoped send hook drains that challenge and
// answers with the ticket. No hook is installed unless the launcher supplies the
// environment variables, so the local path stays byte-identical.
//
// _MACHINE carries the launcher's hardware fingerprint and becomes the machine
// part of the frame; without it the frame keeps the historical "90CN" literal.
//
// Launcher identity binding appends a signed lease to that answer when the
// server asks for it. The lease is produced by the launcher, never here: this
// DLL only forwards bytes it was handed, which is what keeps the private key
// out of a file that ships to every player. Frame versions:
//
//   v1 body: ticket(64) + machine_id(<=128)
//   v2 body: ticket(64) + machine_id(<=128) + lease + lease_len(2B BE)
//
// The length trails the lease because machine_id is the only field with no
// declared length, so the reader recovers every field by working backwards.
// A v1 challenge is still answered with a v1 frame, which is what keeps this
// build able to connect to servers that predate the binding.

typedef int (WSAAPI* RemoteAuthSendFunction)(SOCKET, const char*, int, int);
typedef int (WSAAPI* RemoteAuthRecvFunction)(SOCKET, char*, int, int);
typedef int (WSAAPI* RemoteAuthCloseSocketFunction)(SOCKET);

constexpr size_t kRemoteAuthMaxPorts = 256;
constexpr size_t kRemoteAuthSocketStateCount = 256;
constexpr size_t kLanaTicketHexSize = 64;
constexpr size_t kLanaMaxMachineId = 128;
constexpr size_t kLanaLeaseTrailerSize = 2;
// Mirrors launcherIdentityMaxLeaseBytes in dnfbridge: the longest legal lease is
// a 128 byte account plus a 108 byte base64 signature, all well under this.
constexpr size_t kLanaMaxLeaseSize = 640;
constexpr size_t kLanaMaxFrameSize =
    7 + kLanaTicketHexSize + kLanaMaxMachineId + kLanaMaxLeaseSize + kLanaLeaseTrailerSize;

struct RemoteAuthConfiguration
{
    bool enabled;
    unsigned char ticket[kLanaTicketHexSize];
    unsigned char machineId[kLanaMaxMachineId];
    size_t machineIdLen;
    unsigned long serverIPv4;
    unsigned short ports[kRemoteAuthMaxPorts];
    size_t portCount;
    unsigned char lease[kLanaMaxLeaseSize];
    size_t leaseLen;
};

struct RemoteAuthSocketState
{
    SOCKET socket;
    bool frameReady;
    bool frameSent;
    size_t sentBytes;
    size_t frameLen;
    unsigned char frame[kLanaMaxFrameSize];
};

RemoteAuthConfiguration g_remoteAuth = {};
RemoteAuthSocketState g_remoteAuthSockets[kRemoteAuthSocketStateCount] = {};
SRWLOCK g_remoteAuthSocketLock = SRWLOCK_INIT;
RemoteAuthSendFunction g_remoteAuthSend = nullptr;
RemoteAuthRecvFunction g_originalRemoteAuthRecv = nullptr;
RemoteAuthCloseSocketFunction g_originalRemoteAuthCloseSocket = nullptr;

bool IsRemoteAuthHexDigit(wchar_t character)
{
    return (character >= L'0' && character <= L'9') ||
           (character >= L'a' && character <= L'f') ||
           (character >= L'A' && character <= L'F');
}

bool LoadRemoteAuthConfiguration()
{
    // The three inputs arrive as environment variables of this game process:
    // the launcher sets them right before CreateProcess (see LaunchGameAsync).
    // Nothing is read from an ini file or from disk, so the client keeps its
    // "no external configuration" property; the local single-player path simply
    // never has these variables and stays remote-auth disabled.
    wchar_t ticketText[65] = {};
    wchar_t hostText[64] = {};
    wchar_t portsText[2048] = {};
    DWORD ticketLength = GetEnvironmentVariableW(L"DNF_REMOTE_AUTH_TICKET", ticketText, 65);
    DWORD hostLength = GetEnvironmentVariableW(L"DNF_REMOTE_AUTH_HOST", hostText, 64);
    DWORD portsLength = GetEnvironmentVariableW(L"DNF_REMOTE_AUTH_PORTS", portsText, 2048);
    if (!ticketLength && !hostLength && !portsLength) {
        LogLine("[remote-auth] disabled: launcher environment is absent (local single-player mode)");
        return true;
    }
    if (!ticketLength || !hostLength || !portsLength) {
        // Partially supplied: the launcher intended to enable remote authentication,
        // so name the missing input instead of leaving one opaque message behind.
        LogLine("[remote-auth] disabled: launcher environment is incomplete (ticket=%s host=%s ports=%s)",
            ticketLength ? "present" : "MISSING",
            hostLength ? "present" : "MISSING",
            portsLength ? "present" : "MISSING");
        return true;
    }
    if (ticketLength != kLanaTicketHexSize || hostLength == 0 || hostLength >= 64 ||
        portsLength == 0 || portsLength >= 2048) {
        LogLine("[remote-auth] invalid launcher environment lengths");
        return false;
    }

    RemoteAuthConfiguration parsed = {};
    for (size_t i = 0; i < kLanaTicketHexSize; ++i) {
        if (!IsRemoteAuthHexDigit(ticketText[i])) {
            LogLine("[remote-auth] ticket is not hexadecimal");
            return false;
        }
        parsed.ticket[i] = static_cast<unsigned char>(ticketText[i]);
    }

    char hostASCII[64] = {};
    int hostBytes = WideCharToMultiByte(CP_ACP, WC_NO_BEST_FIT_CHARS, hostText,
        static_cast<int>(hostLength), hostASCII, static_cast<int>(sizeof(hostASCII) - 1), nullptr, nullptr);
    if (hostBytes <= 0) {
        LogLine("[remote-auth] server IPv4 conversion failed");
        return false;
    }
    hostASCII[hostBytes] = '\0';
    parsed.serverIPv4 = inet_addr(hostASCII);
    if (parsed.serverIPv4 == INADDR_NONE) {
        LogLine("[remote-auth] server host must be an IPv4 address");
        return false;
    }

    wchar_t* cursor = portsText;
    while (*cursor) {
        wchar_t* end = nullptr;
        unsigned long port = wcstoul(cursor, &end, 10);
        if (end == cursor || port == 0 || port > 65535 || parsed.portCount >= kRemoteAuthMaxPorts) {
            LogLine("[remote-auth] invalid Game port list");
            return false;
        }
        bool duplicate = false;
        for (size_t i = 0; i < parsed.portCount; ++i) {
            if (parsed.ports[i] == static_cast<unsigned short>(port)) duplicate = true;
        }
        if (!duplicate) parsed.ports[parsed.portCount++] = static_cast<unsigned short>(port);
        if (!*end) break;
        if (*end != L',') {
            LogLine("[remote-auth] malformed Game port separator");
            return false;
        }
        cursor = end + 1;
    }
    if (parsed.portCount == 0) {
        LogLine("[remote-auth] empty Game port list");
        return false;
    }

    // Machine id carried in the auth frame.
    //
    // dnfbridge compares this value byte-for-byte against the machine bans of the
    // ops tool (game_kick.go sessionMatchesBan / checkBans), so a hard-coded
    // literal made every session on every PC look identical: machine bans could
    // never target one player, and banning that literal would kick the whole
    // server. The launcher now injects the real hardware fingerprint
    // (DNF_REMOTE_AUTH_MACHINE, "hw:<32 hex>", see the launcher's
    // Services/HardwareId.cs).
    //
    // Missing / oversized / non-ASCII input keeps the old literal, so a launcher
    // build that predates the variable stays byte-compatible.
    static const unsigned char kLegacyMachineId[4] = { '9', '0', 'C', 'N' };
    wchar_t machineText[kLanaMaxMachineId + 1] = {};
    DWORD machineLength = GetEnvironmentVariableW(L"DNF_REMOTE_AUTH_MACHINE", machineText,
        static_cast<DWORD>(kLanaMaxMachineId + 1));
    bool machineAccepted = machineLength > 0 && machineLength <= kLanaMaxMachineId;
    if (machineAccepted) {
        for (DWORD i = 0; i < machineLength; ++i) {
            const wchar_t character = machineText[i];
            // Printable ASCII only: the frame is assembled as ASCII and dnfbridge
            // parses machine_id as ASCII (launcher_auth.go readLauncherAuthFrame).
            if (character < 0x21 || character > 0x7e) {
                machineAccepted = false;
                break;
            }
            parsed.machineId[i] = static_cast<unsigned char>(character);
        }
    }
    if (machineAccepted) {
        parsed.machineIdLen = static_cast<size_t>(machineLength);
    } else {
        if (machineLength) {
            LogLine("[remote-auth] machine id rejected (len=%lu); using legacy literal",
                static_cast<unsigned long>(machineLength));
        }
        memcpy(parsed.machineId, kLegacyMachineId, sizeof(kLegacyMachineId));
        parsed.machineIdLen = sizeof(kLegacyMachineId);
    }
    SecureZeroMemory(machineText, sizeof(machineText));

    // The launcher identity lease is optional and opaque here: this hook only
    // carries it. An empty value means the launcher has no identity to prove,
    // which is what an older launcher build produces and what a version 1
    // challenge is answered with anyway.
    wchar_t leaseText[kLanaMaxLeaseSize + 1] = {};
    DWORD leaseLength = GetEnvironmentVariableW(L"DNF_LID_LEASE", leaseText,
        static_cast<DWORD>(kLanaMaxLeaseSize + 1));
    if (leaseLength > 0) {
        if (leaseLength > kLanaMaxLeaseSize) {
            LogLine("[remote-auth] launcher identity lease is too long");
            SecureZeroMemory(leaseText, sizeof(leaseText));
            return false;
        }
        for (DWORD i = 0; i < leaseLength; ++i) {
            wchar_t character = leaseText[i];
            // The lease is an ASCII statement plus a base64 signature. Anything
            // outside that range means the launcher handed over the wrong value.
            if (character < 0x21 || character > 0x7e) {
                LogLine("[remote-auth] launcher identity lease is not printable ASCII");
                SecureZeroMemory(leaseText, sizeof(leaseText));
                return false;
            }
            parsed.lease[i] = static_cast<unsigned char>(character);
        }
        parsed.leaseLen = static_cast<size_t>(leaseLength);
    }

    parsed.enabled = true;
    g_remoteAuth = parsed;
    SecureZeroMemory(ticketText, sizeof(ticketText));
    SecureZeroMemory(leaseText, sizeof(leaseText));
    SetEnvironmentVariableW(L"DNF_REMOTE_AUTH_TICKET", nullptr);
    SetEnvironmentVariableW(L"DNF_REMOTE_AUTH_HOST", nullptr);
    SetEnvironmentVariableW(L"DNF_REMOTE_AUTH_PORTS", nullptr);
    SetEnvironmentVariableW(L"DNF_REMOTE_AUTH_MACHINE", nullptr);
    SetEnvironmentVariableW(L"DNF_LID_LEASE", nullptr);
    LogLine("[remote-auth] configured host=%s game_ports=%u lease=%s machine=%s", hostASCII,
        static_cast<unsigned int>(parsed.portCount),
        parsed.leaseLen ? "present" : "absent",
        machineAccepted ? "launcher-env" : "legacy-90CN");
    return true;
}

bool IsRemoteAuthGamePeer(SOCKET socket, unsigned short* destinationPort)
{
    if (!g_remoteAuth.enabled) return false;
    sockaddr_in peer = {};
    int peerLength = sizeof(peer);
    if (getpeername(socket, reinterpret_cast<sockaddr*>(&peer), &peerLength) == SOCKET_ERROR ||
        peer.sin_family != AF_INET || peer.sin_addr.s_addr != g_remoteAuth.serverIPv4) {
        return false;
    }
    unsigned short port = ntohs(peer.sin_port);
    for (size_t i = 0; i < g_remoteAuth.portCount; ++i) {
        if (g_remoteAuth.ports[i] == port) {
            if (destinationPort) *destinationPort = port;
            return true;
        }
    }
    return false;
}

RemoteAuthSocketState* FindRemoteAuthSocketState(SOCKET socket, bool create)
{
    RemoteAuthSocketState* empty = nullptr;
    for (size_t i = 0; i < kRemoteAuthSocketStateCount; ++i) {
        if (g_remoteAuthSockets[i].socket == socket) return &g_remoteAuthSockets[i];
        if (!empty && g_remoteAuthSockets[i].socket == 0) empty = &g_remoteAuthSockets[i];
    }
    if (!create || !empty) return nullptr;
    empty->socket = socket;
    return empty;
}

bool RecvRemoteAuthExact(SOCKET socket, unsigned char* buffer, size_t length)
{
    size_t offset = 0;
    while (offset < length) {
        int received = g_originalRemoteAuthRecv(socket, reinterpret_cast<char*>(buffer + offset),
            static_cast<int>(length - offset), 0);
        if (received > 0) {
            offset += static_cast<size_t>(received);
            continue;
        }
        if (received == 0) {
            WSASetLastError(WSAECONNRESET);
            return false;
        }
        int error = WSAGetLastError();
        if (error == WSAEWOULDBLOCK) {
            fd_set readSet;
            FD_ZERO(&readSet);
            FD_SET(socket, &readSet);
            timeval timeout = { 5, 0 };
            int ready = select(0, &readSet, nullptr, nullptr, &timeout);
            if (ready > 0) continue;
            WSASetLastError(ready == 0 ? WSAETIMEDOUT : WSAGetLastError());
        }
        return false;
    }
    return true;
}

bool RecvLanaChallenge(SOCKET socket, unsigned char* challengeVersion)
{
    unsigned char header[7] = {};
    if (!RecvRemoteAuthExact(socket, header, sizeof(header))) return false;
    static const unsigned char magic[4] = { 'L', 'A', 'N', 'A' };
    // Version 1 is the original frame and version 2 asks for a launcher identity
    // lease. The challenge body is identical in both, so only the reply differs.
    if (memcmp(header, magic, sizeof(magic)) != 0 || (header[4] != 1 && header[4] != 2)) {
        LogLine("[remote-auth] invalid LANA challenge header");
        return false;
    }
    size_t bodyLength = (static_cast<size_t>(header[5]) << 8) | header[6];
    if (bodyLength < 20 || bodyLength > 1024) {
        LogLine("[remote-auth] invalid LANA challenge length");
        return false;
    }
    unsigned char body[1024] = {};
    if (!RecvRemoteAuthExact(socket, body, bodyLength)) return false;
    if (challengeVersion) *challengeVersion = header[4];
    return true;
}

bool BuildLanaAuthFrame(RemoteAuthSocketState* state, unsigned char challengeVersion)
{
    if (!state) return false;
    static const unsigned char magic[4] = { 'L', 'A', 'N', 'A' };
    // A version 1 challenge predates the lease, so answering it with a version 2
    // frame would be rejected outright by that server. The lease is silently
    // dropped instead; the server enforcing the binding always asks for v2.
    bool withLease = challengeVersion >= 2;
    size_t leaseLength = withLease ? g_remoteAuth.leaseLen : 0;
    size_t trailerLength = withLease ? leaseLength + kLanaLeaseTrailerSize : 0;
    size_t bodyLength = kLanaTicketHexSize + g_remoteAuth.machineIdLen + trailerLength;
    if (bodyLength > 0xffff) return false;
    memcpy(state->frame, magic, sizeof(magic));
    state->frame[4] = withLease ? 2 : 1;
    state->frame[5] = static_cast<unsigned char>((bodyLength >> 8) & 0xff);
    state->frame[6] = static_cast<unsigned char>(bodyLength & 0xff);
    size_t offset = 7;
    memcpy(state->frame + offset, g_remoteAuth.ticket, kLanaTicketHexSize);
    offset += kLanaTicketHexSize;
    memcpy(state->frame + offset, g_remoteAuth.machineId, g_remoteAuth.machineIdLen);
    offset += g_remoteAuth.machineIdLen;
    if (withLease) {
        if (leaseLength) memcpy(state->frame + offset, g_remoteAuth.lease, leaseLength);
        offset += leaseLength;
        // The length trails the lease so the reader can recover machine_id
        // without a length field of its own.
        state->frame[offset] = static_cast<unsigned char>((leaseLength >> 8) & 0xff);
        state->frame[offset + 1] = static_cast<unsigned char>(leaseLength & 0xff);
        offset += kLanaLeaseTrailerSize;
    }
    state->frameLen = offset;
    state->sentBytes = 0;
    state->frameReady = true;
    return true;
}

int WSAAPI ProxyRemoteAuthRecv(SOCKET socket, char* buffer, int length, int flags)
{
    if (!g_originalRemoteAuthRecv || !g_remoteAuth.enabled) {
        return g_originalRemoteAuthRecv ? g_originalRemoteAuthRecv(socket, buffer, length, flags) : SOCKET_ERROR;
    }
    unsigned short destinationPort = 0;
    if (!IsRemoteAuthGamePeer(socket, &destinationPort)) {
        return g_originalRemoteAuthRecv(socket, buffer, length, flags);
    }

    AcquireSRWLockExclusive(&g_remoteAuthSocketLock);
    RemoteAuthSocketState* state = FindRemoteAuthSocketState(socket, true);
    if (!state) {
        ReleaseSRWLockExclusive(&g_remoteAuthSocketLock);
        return g_originalRemoteAuthRecv(socket, buffer, length, flags);
    }
    if (!state->frameSent) {
        // dnfbridge sends the LANA challenge immediately after accept; drain it,
        // answer with the ticket, then let the real DNF payload reach the caller.
        unsigned char challengeVersion = 1;
        if (!RecvLanaChallenge(socket, &challengeVersion)) {
            ReleaseSRWLockExclusive(&g_remoteAuthSocketLock);
            WSASetLastError(WSAEACCES);
            return SOCKET_ERROR;
        }
        if (!state->frameReady && !BuildLanaAuthFrame(state, challengeVersion)) {
            ReleaseSRWLockExclusive(&g_remoteAuthSocketLock);
            WSASetLastError(WSAEACCES);
            return SOCKET_ERROR;
        }
        while (state->sentBytes < state->frameLen) {
            int sent = g_remoteAuthSend(socket,
                reinterpret_cast<const char*>(state->frame + state->sentBytes),
                static_cast<int>(state->frameLen - state->sentBytes), 0);
            if (sent > 0) {
                state->sentBytes += static_cast<size_t>(sent);
                continue;
            }
            if (sent == SOCKET_ERROR && WSAGetLastError() == WSAEWOULDBLOCK) {
                fd_set writeSet;
                FD_ZERO(&writeSet);
                FD_SET(socket, &writeSet);
                timeval timeout = { 5, 0 };
                int ready = select(0, nullptr, &writeSet, nullptr, &timeout);
                if (ready > 0) continue;
                WSASetLastError(ready == 0 ? WSAETIMEDOUT : WSAGetLastError());
            } else if (sent == 0) {
                WSASetLastError(WSAECONNRESET);
            }
            ReleaseSRWLockExclusive(&g_remoteAuthSocketLock);
            return SOCKET_ERROR;
        }
        state->frameSent = true;
    }
    ReleaseSRWLockExclusive(&g_remoteAuthSocketLock);
    return g_originalRemoteAuthRecv(socket, buffer, length, flags);
}

int WSAAPI ProxyRemoteAuthCloseSocket(SOCKET socket)
{
    if (!g_originalRemoteAuthCloseSocket) {
        WSASetLastError(WSAENOTSOCK);
        return SOCKET_ERROR;
    }
    int result = g_originalRemoteAuthCloseSocket(socket);
    if (result == 0 && g_remoteAuth.enabled) {
        AcquireSRWLockExclusive(&g_remoteAuthSocketLock);
        RemoteAuthSocketState* state = FindRemoteAuthSocketState(socket, false);
        if (state) SecureZeroMemory(state, sizeof(*state));
        ReleaseSRWLockExclusive(&g_remoteAuthSocketLock);
    }
    return result;
}

bool HookMainModuleImportEntry(const char* targetDll, const char* targetName,
    unsigned short targetOrdinal, void* detour, void** original)
{
    if (!targetDll || (!targetName && targetOrdinal == 0) || !detour || !original) return false;
    const char* label = targetName ? targetName : "ordinal";
    HMODULE module = GetModuleHandleW(nullptr);
    if (!module) return false;

    __try {
        auto* dos = reinterpret_cast<IMAGE_DOS_HEADER*>(module);
        if (dos->e_magic != IMAGE_DOS_SIGNATURE) return false;
        auto* nt = reinterpret_cast<IMAGE_NT_HEADERS*>(
            reinterpret_cast<unsigned char*>(module) + dos->e_lfanew);
        if (nt->Signature != IMAGE_NT_SIGNATURE) return false;

        IMAGE_DATA_DIRECTORY importDirectory =
            nt->OptionalHeader.DataDirectory[IMAGE_DIRECTORY_ENTRY_IMPORT];
        if (!importDirectory.VirtualAddress || !importDirectory.Size) {
            LogLine("iat hook skipped name=%s reason=no-import-directory", label);
            return false;
        }

        auto* descriptor = reinterpret_cast<IMAGE_IMPORT_DESCRIPTOR*>(
            reinterpret_cast<unsigned char*>(module) + importDirectory.VirtualAddress);
        for (; descriptor->Name; ++descriptor) {
            const char* dllName = reinterpret_cast<const char*>(
                reinterpret_cast<unsigned char*>(module) + descriptor->Name);
            if (!dllName || _stricmp(dllName, targetDll) != 0) continue;

            auto* nameThunk = reinterpret_cast<IMAGE_THUNK_DATA*>(
                reinterpret_cast<unsigned char*>(module) + descriptor->OriginalFirstThunk);
            auto* addressThunk = reinterpret_cast<IMAGE_THUNK_DATA*>(
                reinterpret_cast<unsigned char*>(module) + descriptor->FirstThunk);
            if (!descriptor->OriginalFirstThunk) nameThunk = addressThunk;

            for (; nameThunk->u1.AddressOfData && addressThunk->u1.Function; ++nameThunk, ++addressThunk) {
                if (IMAGE_SNAP_BY_ORDINAL(nameThunk->u1.Ordinal)) {
                    if (targetOrdinal == 0 || IMAGE_ORDINAL(nameThunk->u1.Ordinal) != targetOrdinal) continue;
                } else {
                    if (targetOrdinal != 0) continue;
                    auto* importByName = reinterpret_cast<IMAGE_IMPORT_BY_NAME*>(
                        reinterpret_cast<unsigned char*>(module) + nameThunk->u1.AddressOfData);
                    const char* importName = reinterpret_cast<const char*>(importByName->Name);
                    if (!importName || strcmp(importName, targetName) != 0) continue;
                }

                void* current = reinterpret_cast<void*>(addressThunk->u1.Function);
                if (current == detour) {
                    LogLine("iat hook already installed name=%s ordinal=%u slot=%p",
                        label, targetOrdinal, addressThunk);
                    return true;
                }

                DWORD oldProtection = 0;
                if (!VirtualProtect(&addressThunk->u1.Function, sizeof(addressThunk->u1.Function),
                        PAGE_EXECUTE_READWRITE, &oldProtection)) {
                    LogLine("iat hook protect failed name=%s ordinal=%u slot=%p gle=%lu",
                        label, targetOrdinal, addressThunk, GetLastError());
                    return false;
                }

                *original = current;
                addressThunk->u1.Function = reinterpret_cast<ULONG_PTR>(detour);
                DWORD ignoredProtection = 0;
                VirtualProtect(&addressThunk->u1.Function, sizeof(addressThunk->u1.Function),
                    oldProtection, &ignoredProtection);
                FlushInstructionCache(GetCurrentProcess(), &addressThunk->u1.Function,
                    sizeof(addressThunk->u1.Function));
                LogLine("iat hook name=%s ordinal=%u dll=%s slot=%p old=%p detour=%p",
                    label, targetOrdinal, dllName, addressThunk, current, detour);
                return true;
            }
        }
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("iat hook exception name=%s ordinal=%u code=0x%08X",
            label, targetOrdinal, GetExceptionCode());
        return false;
    }

    LogLine("iat hook skipped name=%s ordinal=%u dll=%s reason=not-imported",
        label, targetOrdinal, targetDll);
    return false;
}

bool HookMainModuleImportOrdinal(const char* targetDll, unsigned short targetOrdinal,
    void* detour, void** original)
{
    return HookMainModuleImportEntry(targetDll, nullptr, targetOrdinal, detour, original);
}

bool InstallRemoteGameAuthenticationHook()
{
    if (!LoadRemoteAuthConfiguration()) return false;
    if (!g_remoteAuth.enabled) return true;

    // 发 auth 帧用 send，从 ws2_32 动态获取（无需 hook send）。
    HMODULE ws2 = GetModuleHandleW(L"ws2_32.dll");
    if (!ws2) {
        LogLine("[remote-auth] ws2_32.dll not loaded");
        return false;
    }
    g_remoteAuthSend = reinterpret_cast<RemoteAuthSendFunction>(GetProcAddress(ws2, "send"));
    if (!g_remoteAuthSend) {
        LogLine("[remote-auth] send export not found");
        return false;
    }

    void* closeOriginal = nullptr;
    // Current DNF.exe imports Winsock 1.1 functions by ordinal: closesocket=3,
    // recv=16. Name-only matching skips both IAT entries.
    bool closeInstalled = HookMainModuleImportOrdinal("WS2_32.dll", 3,
        reinterpret_cast<void*>(&ProxyRemoteAuthCloseSocket), &closeOriginal);
    if (closeInstalled && closeOriginal && !g_originalRemoteAuthCloseSocket) {
        g_originalRemoteAuthCloseSocket = reinterpret_cast<RemoteAuthCloseSocketFunction>(closeOriginal);
    }
    if (!closeInstalled || !g_originalRemoteAuthCloseSocket) {
        LogLine("[remote-auth] closesocket hook is required before recv hook");
        return false;
    }
    void* recvOriginal = nullptr;
    bool recvInstalled = HookMainModuleImportOrdinal("WS2_32.dll", 16,
        reinterpret_cast<void*>(&ProxyRemoteAuthRecv), &recvOriginal);
    if (recvInstalled && recvOriginal && !g_originalRemoteAuthRecv) {
        g_originalRemoteAuthRecv = reinterpret_cast<RemoteAuthRecvFunction>(recvOriginal);
    }
    LogLine("[remote-auth] IAT hooks recv=%d closesocket=%d", recvInstalled ? 1 : 0,
        closeInstalled ? 1 : 0);
    return recvInstalled && closeInstalled;
}

bool InstallUiCompatibilityHook()
{
    void* sendMessageOriginal = nullptr;
    bool sendMessageInstalled = HookMainModuleImport("USER32.dll", "SendMessageW",
        reinterpret_cast<void*>(&ProxySendMessageW), &sendMessageOriginal);
    if (sendMessageInstalled && sendMessageOriginal && !g_originalSendMessageW) {
        g_originalSendMessageW = sendMessageOriginal;
    }

    void* showWindowOriginal = nullptr;
    bool showWindowInstalled = HookMainModuleImport("USER32.dll", "ShowWindow",
        reinterpret_cast<void*>(&ProxyShowWindow), &showWindowOriginal);
    if (showWindowInstalled && showWindowOriginal && !g_originalShowWindow) {
        g_originalShowWindow = showWindowOriginal;
    }

    void* coCreateOriginal = nullptr;
    bool coCreateInstalled = HookMainModuleImport("ole32.dll", "CoCreateInstance",
        reinterpret_cast<void*>(&ProxyCoCreateInstance), &coCreateOriginal);
    if (coCreateInstalled && coCreateOriginal && !g_originalCoCreateInstance) {
        g_originalCoCreateInstance = coCreateOriginal;
    }

    bool installed = sendMessageInstalled && showWindowInstalled && coCreateInstalled;
    LogLine("[nomin] audited current-EXE IAT branches send=%d show=%d toggle_desktop=%d",
        sendMessageInstalled ? 1 : 0,
        showWindowInstalled ? 1 : 0,
        coCreateInstalled ? 1 : 0);
    return installed;
}

bool InstallMultiClientCompatibilityHook()
{
    if (!IsMultiClientLaunch()) {
        LogLine("[multi] compatibility disabled for normal client launch");
        return true;
    }

    void* createMutexOriginal = nullptr;
    bool installed = HookMainModuleImport(
        "KERNEL32.dll",
        "CreateMutexW",
        reinterpret_cast<void*>(&ProxyCreateMutexW),
        &createMutexOriginal);
    if (installed && createMutexOriginal && !g_originalCreateMutexW) {
        g_originalCreateMutexW = createMutexOriginal;
    }
    void* findWindowOriginal = nullptr;
    bool findWindowInstalled = HookMainModuleImport(
        "USER32.dll",
        "FindWindowW",
        reinterpret_cast<void*>(&ProxyFindWindowW),
        &findWindowOriginal);
    if (findWindowInstalled && findWindowOriginal && !g_originalFindWindowW) {
        g_originalFindWindowW = findWindowOriginal;
    }
    LogLine(
        "[multi] audited current-EXE hooks mutex=%d window=%d mutex_return_rva=0x%08X window_return_rva=0x%08X",
        installed ? 1 : 0,
        findWindowInstalled ? 1 : 0,
        kMultiClientCreateMutexReturnRva,
        kMultiClientFindWindowReturnRva);
    return installed && findWindowInstalled;
}

bool InstallSocketOpenTrace()
{
    // Both signatures include whole instructions only.  A mismatch means this
    // executable build is not the audited NoPack image, so leave it untouched.
    static const unsigned char kItemRowParsePrologue[] = { 0x55, 0x8B, 0xEC, 0x6A, 0xFF };
    static const unsigned char kPanelSelectPrologue[] = { 0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08 };
    // 53 56 8B F1 is followed by a six-byte mov; keep whole instructions in
    // the trampoline patch rather than splitting that instruction at byte 5.
    static const unsigned char kSocketWriterPrologue[] = {
        0x53, 0x56, 0x8B, 0xF1, 0x8B, 0x86, 0xB4, 0x01, 0x00, 0x00
    };
    // push ebp; mov ebp,esp; mov eax,[ebp+8]; mov ecx,[imm32]
    static const unsigned char kSocketOp14ExtensionGatePrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08, 0x8B, 0x0D, 0xB8, 0x26, 0x1B, 0x05
    };
    uintptr_t itemRowParseTarget = g_dnfBase + kCurrentItemRowParseRva;
    uintptr_t panelSelectTarget = g_dnfBase + kSocketPanelSelectRva;
    uintptr_t socketWriterTarget = g_dnfBase + kSocketOpenWriterRva;
    uintptr_t socketOp14ExtensionGateTarget = g_dnfBase + kSocketOp14ExtensionGateRva;

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(itemRowParseTarget),
                kItemRowParsePrologue, sizeof(kItemRowParsePrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(panelSelectTarget),
                kPanelSelectPrologue, sizeof(kPanelSelectPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(socketWriterTarget),
                kSocketWriterPrologue, sizeof(kSocketWriterPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(socketOp14ExtensionGateTarget),
                kSocketOp14ExtensionGatePrologue, sizeof(kSocketOp14ExtensionGatePrologue))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("socket-trace targets did not reach audited bytes; no trace hook installed row=0x%08X panel=0x%08X writer=0x%08X gate=0x%08X",
            itemRowParseTarget, panelSelectTarget, socketWriterTarget, socketOp14ExtensionGateTarget);
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(itemRowParseTarget, kItemRowParsePrologue,
            sizeof(kItemRowParsePrologue), reinterpret_cast<void*>(&ProxyCurrentItemRowParse),
            &original, "socket-trace item-row parser")) {
        return false;
    }
    g_originalCurrentItemRowParse = reinterpret_cast<CurrentItemRowParseFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(panelSelectTarget, kPanelSelectPrologue,
            sizeof(kPanelSelectPrologue), reinterpret_cast<void*>(&ProxySocketPanelSelect),
            &original, "socket-trace panel selection")) {
        LogLine("socket-trace panel hook failed; parser remains passive and silent");
        return false;
    }
    g_originalSocketPanelSelect = reinterpret_cast<SocketPanelSelectFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(socketWriterTarget, kSocketWriterPrologue,
            sizeof(kSocketWriterPrologue), reinterpret_cast<void*>(&ProxySocketOpenWriter),
            &original, "socket-trace writer")) {
        LogLine("socket-trace writer hook failed; parser and panel hooks remain passive");
        return false;
    }
    g_originalSocketOpenWriter = reinterpret_cast<SocketOpenWriterFn>(original);

    original = nullptr;
    if (!InstallKnownInlineHook(socketOp14ExtensionGateTarget, kSocketOp14ExtensionGatePrologue,
            sizeof(kSocketOp14ExtensionGatePrologue), reinterpret_cast<void*>(&ProxySocketOp14ExtensionGate),
            &original, "socket-trace op14/list3 extension gate")) {
        LogLine("socket-trace extension-gate hook failed; existing row/panel/writer hooks remain passive");
        return false;
    }
    g_originalSocketOp14ExtensionGate = reinterpret_cast<SocketOp14ExtensionGateFn>(original);
    LogLine("socket-trace installed row=0x%08X panel=0x%08X writer=0x%08X gate=0x%08X wait_ms=%d max_selections=%ld max_writer_hits=%ld max_gate_hits=%ld",
        itemRowParseTarget, panelSelectTarget, socketWriterTarget, socketOp14ExtensionGateTarget, waitedMs,
        kSocketTraceMaxSelections, kSocketTraceMaxWriterHits, kSocketTraceMaxExtensionGateHits);
    return true;
}

bool InstallEquipmentDisjointBatchCompatibility()
{
    static const unsigned char kBatchLoopPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0x28, 0xAF, 0xF1, 0x03
    };
    static const unsigned char kDisjointSendPrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x28,
        0xA1, 0x3C, 0x67, 0x0E, 0x05
    };
    static const unsigned char kWriterValue[] = {
        0xA1, 0xBC, 0xA4, 0x2E, 0x05, 0xC3
    };
    static const unsigned char kWriterU16Prologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    static const unsigned char kWriterScalarPrologue[] = {
        0x55, 0x8B, 0xEC, 0x80, 0x79, 0x08, 0x00
    };
    static const unsigned char kWriterFlushPrologue[] = {
        0x55, 0x8B, 0xEC, 0xB8, 0x50, 0x00, 0x04, 0x00
    };
    static const unsigned char kTitleRejectJump[] = {
        0x0F, 0x84, 0x9B, 0x06, 0x00, 0x00
    };
    static const unsigned char kTitleAllowPatch[] = {
        0x90, 0x90, 0x90, 0x90, 0x90, 0x90
    };

    uintptr_t batchLoopTarget = g_dnfBase + kEquipmentDisjointBatchLoopRva;
    uintptr_t disjointSendTarget = g_dnfBase + kEquipmentDisjointSendRva;
    uintptr_t writerValueTarget = g_dnfBase + kUpperWriterValueRva;
    uintptr_t writerU16Target = g_dnfBase + kUpperWriterU16Rva;
    uintptr_t writerU8Target = g_dnfBase + kUpperWriterU8Rva;
    uintptr_t writerI16Target = g_dnfBase + kUpperWriterI16Rva;
    uintptr_t writerU32Target = g_dnfBase + kUpperWriterU32Rva;
    uintptr_t writerFlushTarget = g_dnfBase + kUpperWriterFlushRva;
    uintptr_t titleRejectJumpTarget =
        g_dnfBase + kEquipmentDisjointTitleRejectJumpRva;

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        bool ready =
            BytesMatch(reinterpret_cast<unsigned char*>(batchLoopTarget),
                kBatchLoopPrologue, sizeof(kBatchLoopPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(disjointSendTarget),
                kDisjointSendPrologue, sizeof(kDisjointSendPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerValueTarget),
                kWriterValue, sizeof(kWriterValue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerU16Target),
                kWriterU16Prologue, sizeof(kWriterU16Prologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerU8Target),
                kWriterScalarPrologue, sizeof(kWriterScalarPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerI16Target),
                kWriterScalarPrologue, sizeof(kWriterScalarPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerU32Target),
                kWriterScalarPrologue, sizeof(kWriterScalarPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerFlushTarget),
                kWriterFlushPrologue, sizeof(kWriterFlushPrologue)) &&
            (BytesMatch(reinterpret_cast<unsigned char*>(titleRejectJumpTarget),
                 kTitleRejectJump, sizeof(kTitleRejectJump)) ||
             BytesMatch(reinterpret_cast<unsigned char*>(titleRejectJumpTarget),
                 kTitleAllowPatch, sizeof(kTitleAllowPatch)));
        if (ready) break;
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("equipment-disjoint compatibility targets unavailable loop=0x%08X send=0x%08X title=0x%08X",
            batchLoopTarget, disjointSendTarget, titleRejectJumpTarget);
        return false;
    }

    if (BytesMatch(reinterpret_cast<unsigned char*>(titleRejectJumpTarget),
            kTitleRejectJump, sizeof(kTitleRejectJump)) &&
        !WriteCodePatch(titleRejectJumpTarget, kTitleAllowPatch,
            sizeof(kTitleAllowPatch),
            "equipment-disjoint title candidate eligibility")) {
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(disjointSendTarget, kDisjointSendPrologue,
            sizeof(kDisjointSendPrologue),
            reinterpret_cast<void*>(&ProxyCurrentEquipmentDisjointSend),
            &original, "equipment-disjoint selected-item sender")) {
        return false;
    }
    g_originalEquipmentDisjointSend = original;

    original = nullptr;
    if (!InstallKnownInlineHook(batchLoopTarget, kBatchLoopPrologue,
            sizeof(kBatchLoopPrologue),
            reinterpret_cast<void*>(&ProxyCurrentEquipmentDisjointBatchLoop),
            &original, "equipment-disjoint batch selection loop")) {
        return false;
    }
    g_originalEquipmentDisjointBatchLoop = original;
    LogLine("equipment-disjoint compatibility installed loop=0x%08X send=0x%08X title=0x%08X wait_ms=%d",
        batchLoopTarget, disjointSendTarget, titleRejectJumpTarget, waitedMs);
    return true;
}

bool InstallExpertJobStoreDisjointBatchCompatibility()
{
    if (!kExpertJobStoreDisjointBatchEnabled) return false;
    // push ebp; mov ebp,esp; push -1; push <SEH handler>
    static const unsigned char kStoreBatchLoopPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0xB8, 0x0A, 0xED, 0x03
    };
    // push ebp; mov ebp,esp; sub esp,0x18; push esi; mov esi,ecx.
    // Whole instructions only: InstallKnownInlineHook consumes them as the
    // patch length, and the `ret 8` of this target is what fixes the proxy's
    // (slot, ownerID) stack arguments.
    static const unsigned char kStoreDisjointSendPrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x18, 0x56, 0x8B, 0xF1
    };

    uintptr_t batchLoopTarget =
        g_dnfBase + kExpertJobStoreDisjointBatchLoopRva;
    uintptr_t disjointSendTarget =
        g_dnfBase + kExpertJobStoreDisjointSendRva;

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(batchLoopTarget),
                kStoreBatchLoopPrologue, sizeof(kStoreBatchLoopPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(disjointSendTarget),
                kStoreDisjointSendPrologue,
                sizeof(kStoreDisjointSendPrologue))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("expert-job-store disjoint batch targets unavailable loop=0x%08X send=0x%08X",
            batchLoopTarget, disjointSendTarget);
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(disjointSendTarget,
            kStoreDisjointSendPrologue, sizeof(kStoreDisjointSendPrologue),
            reinterpret_cast<void*>(&ProxyExpertJobStoreDisjointSend),
            &original, "expert-job-store disjoint selected-item sender")) {
        return false;
    }
    g_originalExpertJobStoreDisjointSend = original;

    original = nullptr;
    if (!InstallKnownInlineHook(batchLoopTarget, kStoreBatchLoopPrologue,
            sizeof(kStoreBatchLoopPrologue),
            reinterpret_cast<void*>(&ProxyExpertJobStoreDisjointBatchLoop),
            &original, "expert-job-store disjoint batch selection loop")) {
        return false;
    }
    g_originalExpertJobStoreDisjointBatchLoop = original;
    LogLine("expert-job-store disjoint batch installed loop=0x%08X send=0x%08X wait_ms=%d",
        batchLoopTarget, disjointSendTarget, waitedMs);
    return true;
}

bool InstallExpertJobExtractionBatchCompatibility()
{
    if (!kExpertJobExtractionBatchEnabled) return false;
    // push ebp; mov ebp,esp; push -1; push <SEH handler>
    static const unsigned char kExtractionBatchLoopPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0x58, 0x59, 0xDC, 0x03
    };
    // push ebp; mov ebp,esp; sub esp,0x18; push esi; mov esi,ecx.
    // Byte-identical to the op235 selected-item sender prologue; this target's
    // `ret 0xc` is what fixes the proxy's (type, extractorSlot, targetSlot)
    // stack arguments.
    static const unsigned char kExtractionSendPrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x18, 0x56, 0x8B, 0xF1
    };

    uintptr_t batchLoopTarget =
        g_dnfBase + kExpertJobExtractionBatchLoopRva;
    uintptr_t extractionSendTarget =
        g_dnfBase + kExpertJobExtractionSendRva;

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(batchLoopTarget),
                kExtractionBatchLoopPrologue, sizeof(kExtractionBatchLoopPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(extractionSendTarget),
                kExtractionSendPrologue,
                sizeof(kExtractionSendPrologue))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("expert-job extraction batch targets unavailable loop=0x%08X send=0x%08X",
            batchLoopTarget, extractionSendTarget);
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(extractionSendTarget,
            kExtractionSendPrologue, sizeof(kExtractionSendPrologue),
            reinterpret_cast<void*>(&ProxyExpertJobExtractionSend),
            &original, "expert-job extraction selected-item sender")) {
        return false;
    }
    g_originalExpertJobExtractionSend = original;

    original = nullptr;
    if (!InstallKnownInlineHook(batchLoopTarget, kExtractionBatchLoopPrologue,
            sizeof(kExtractionBatchLoopPrologue),
            reinterpret_cast<void*>(&ProxyExpertJobExtractionBatchLoop),
            &original, "expert-job extraction batch selection loop")) {
        return false;
    }
    g_originalExpertJobExtractionBatchLoop = original;
    LogLine("expert-job extraction batch installed loop=0x%08X send=0x%08X wait_ms=%d",
        batchLoopTarget, extractionSendTarget, waitedMs);
    return true;
}

bool InstallContractUseCompatibility()
{
    static const unsigned char kInventoryUseUIAPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0x48, 0xC2, 0xCA, 0x03
    };
    static const unsigned char kInventoryUseUIBPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0xF9, 0xCD, 0xD3, 0x03
    };
    // push ebp; mov ebp,esp; mov eax,[ebp+0C]; mov edx,[ebp+10];
    // push esi; mov esi,ecx.
    static const unsigned char kInventoryUseGateAPrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x0C, 0x8B, 0x55, 0x10,
        0x56, 0x8B, 0xF1
    };
    // push ebp; mov ebp,esp; push esi; push 0x7D4; mov esi,ecx.
    static const unsigned char kInventoryUseGateBPrologue[] = {
        0x55, 0x8B, 0xEC, 0x56, 0x68, 0xD4, 0x07, 0x00, 0x00,
        0x8B, 0xF1
    };
    // push ebp; mov ebp,esp; push ecx; push ebx; push edi; mov edi,ecx.
    // This is the whole-instruction prefix of sub_2331CE0, whose `ret 8`
    // proves its (template ID, slot) stack arguments.
    static const unsigned char kInventoryUsePanelPrologue[] = {
        0x55, 0x8B, 0xEC, 0x51, 0x53, 0x57, 0x8B, 0xF9
    };
    static const unsigned char kSelectionCtorPrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x10
    };
    static const unsigned char kSelectionCollectPrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x14
    };
    static const unsigned char kSelectionDtorPrologue[] = {
        0x56, 0x8B, 0xF1, 0x8B, 0x46, 0x04
    };
    static const unsigned char kSelectionLookupPrologue[] = {
        0x55, 0x8B, 0xEC, 0x51, 0x56
    };
    static const unsigned char kTemplateReadPrologue[] = {
        0x56, 0x6A, 0x01, 0x6A, 0x32
    };
    static const unsigned char kIdentityReadPrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x1C
    };
    static const unsigned char kWriterValue[] = {
        0xA1, 0xBC, 0xA4, 0x2E, 0x05, 0xC3
    };
    static const unsigned char kWriterU16Prologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    static const unsigned char kWriterScalarPrologue[] = {
        0x55, 0x8B, 0xEC, 0x80, 0x79, 0x08, 0x00
    };
    static const unsigned char kWriterFlushPrologue[] = {
        0x55, 0x8B, 0xEC, 0xB8, 0x50, 0x00, 0x04, 0x00
    };

    uintptr_t uiATarget = g_dnfBase + kInventoryUseUIARva;
    uintptr_t uiBTarget = g_dnfBase + kInventoryUseUIBRva;
    uintptr_t gateATarget = g_dnfBase + kInventoryUseGateARva;
    uintptr_t gateBTarget = g_dnfBase + kInventoryUseGateBRva;
    uintptr_t panelTarget = g_dnfBase + kInventoryUsePanelRva;
    uintptr_t selectionCtorTarget = g_dnfBase + kInventorySelectionCtorRva;
    uintptr_t selectionCollectTarget = g_dnfBase + kInventorySelectionCollectRva;
    uintptr_t selectionDtorTarget = g_dnfBase + kInventorySelectionDtorRva;
    uintptr_t selectionLookupTarget = g_dnfBase + kInventorySelectionLookupRva;
    uintptr_t templateReadTarget = g_dnfBase + kCurrentItemTemplateReadRva;
    uintptr_t identityReadTarget = g_dnfBase + kCurrentItemIdentityReadRva;
    uintptr_t writerValueTarget = g_dnfBase + kUpperWriterValueRva;
    uintptr_t writerU16Target = g_dnfBase + kUpperWriterU16Rva;
    uintptr_t writerU8Target = g_dnfBase + kUpperWriterU8Rva;
    uintptr_t writerI16Target = g_dnfBase + kUpperWriterI16Rva;
    uintptr_t writerU32Target = g_dnfBase + kUpperWriterU32Rva;
    uintptr_t writerFlushTarget = g_dnfBase + kUpperWriterFlushRva;

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        bool ready =
            BytesMatch(reinterpret_cast<unsigned char*>(uiATarget),
                kInventoryUseUIAPrologue, sizeof(kInventoryUseUIAPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(uiBTarget),
                kInventoryUseUIBPrologue, sizeof(kInventoryUseUIBPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(gateATarget),
                kInventoryUseGateAPrologue, sizeof(kInventoryUseGateAPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(gateBTarget),
                kInventoryUseGateBPrologue, sizeof(kInventoryUseGateBPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(panelTarget),
                kInventoryUsePanelPrologue, sizeof(kInventoryUsePanelPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(selectionCtorTarget),
                kSelectionCtorPrologue, sizeof(kSelectionCtorPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(selectionCollectTarget),
                kSelectionCollectPrologue, sizeof(kSelectionCollectPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(selectionDtorTarget),
                kSelectionDtorPrologue, sizeof(kSelectionDtorPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(selectionLookupTarget),
                kSelectionLookupPrologue, sizeof(kSelectionLookupPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(templateReadTarget),
                kTemplateReadPrologue, sizeof(kTemplateReadPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(identityReadTarget),
                kIdentityReadPrologue, sizeof(kIdentityReadPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerValueTarget),
                kWriterValue, sizeof(kWriterValue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerU16Target),
                kWriterU16Prologue, sizeof(kWriterU16Prologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerU8Target),
                kWriterScalarPrologue, sizeof(kWriterScalarPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerI16Target),
                kWriterScalarPrologue, sizeof(kWriterScalarPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerU32Target),
                kWriterScalarPrologue, sizeof(kWriterScalarPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(writerFlushTarget),
                kWriterFlushPrologue, sizeof(kWriterFlushPrologue));
        if (ready) break;
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("contract-use compatibility targets did not reach audited bytes; no bypass installed uiA=0x%08X uiB=0x%08X gateA=0x%08X gateB=0x%08X panel=0x%08X",
            uiATarget, uiBTarget, gateATarget, gateBTarget, panelTarget);
        return false;
    }

    if (!InstallContractUseWindowProc()) {
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(uiATarget, kInventoryUseUIAPrologue,
            sizeof(kInventoryUseUIAPrologue),
            reinterpret_cast<void*>(&ProxyInventoryUseUIA), &original,
            "contract-use main inventory callback A")) {
        return false;
    }
    g_originalInventoryUseUIA = original;

    original = nullptr;
    if (!InstallKnownInlineHook(uiBTarget, kInventoryUseUIBPrologue,
            sizeof(kInventoryUseUIBPrologue),
            reinterpret_cast<void*>(&ProxyInventoryUseUIB), &original,
            "contract-use main inventory callback B")) {
        return false;
    }
    g_originalInventoryUseUIB = original;

    original = nullptr;
    if (!InstallKnownInlineHook(gateATarget, kInventoryUseGateAPrologue,
            sizeof(kInventoryUseGateAPrologue),
            reinterpret_cast<void*>(&ProxyInventoryUseGateA), &original,
            "contract-use pre-panel gate A")) {
        return false;
    }
    g_originalInventoryUseGateA = original;

    original = nullptr;
    if (!InstallKnownInlineHook(gateBTarget, kInventoryUseGateBPrologue,
            sizeof(kInventoryUseGateBPrologue),
            reinterpret_cast<void*>(&ProxyInventoryUseGateB), &original,
            "contract-use pre-panel gate B")) {
        return false;
    }
    g_originalInventoryUseGateB = original;

    original = nullptr;
    if (!InstallKnownInlineHook(panelTarget, kInventoryUsePanelPrologue,
            sizeof(kInventoryUsePanelPrologue),
            reinterpret_cast<void*>(&ProxyInventoryUsePanel), &original,
            "contract-use active-coupon panel callback")) {
        return false;
    }
    g_originalInventoryUsePanel = original;

    LogLine("contract-use compatibility installed uiA=0x%08X uiB=0x%08X gateA=0x%08X gateB=0x%08X panel=0x%08X lookup=0x%08X template=0x%08X identity=0x%08X wait_ms=%d scope=premiumlist-only-native-container-scan",
        uiATarget, uiBTarget, gateATarget, gateBTarget, panelTarget,
        selectionLookupTarget, templateReadTarget, identityReadTarget, waitedMs);

    (void)selectionLookupTarget;
    // 2026-09-14：绝望之塔层数调查用的全部客户端诊断探针已从源码整块删除
    // （GetString 全量普查 / 卡片行钩子 / 控件文本钩子 / 塔尾钩子 / 宽字符串
    //   内存扫描 / 上层派发 / 塔层来源 / 塔参数 / 卡片层号等探针）。
    // 这个文件里已不含任何探针代码，也不再安装任何诊断钩子。
    ApplyAccountWarehousePanelKeepPatch();
    // 2026-09-13：绝望之塔副本选择卡片永远显示 1 层。见函数头注释。
    ApplyTowerOfDespairFloorSelectionFix();
    return true;
}

bool InstallPartyTeleportPromptCloseCompatibility()
{
    if (!g_dnfBase) return false;
    static const unsigned char kPartyTeleportDialogStateSetPrologue[] = {
        0x55, 0x8B, 0xEC, 0x56, 0x8B, 0xB1, 0xA8, 0x01, 0x00, 0x00
    };
    void* original = nullptr;
    if (!InstallKnownInlineHook(
            g_dnfBase + kPartyTeleportDialogStateSetRva,
            kPartyTeleportDialogStateSetPrologue,
            sizeof(kPartyTeleportDialogStateSetPrologue),
            reinterpret_cast<void*>(&ProxyPartyTeleportDialogStateSet),
            &original, "party teleport consent prompt close")) {
        return false;
    }
    g_originalPartyTeleportDialogStateSet =
        reinterpret_cast<PartyTeleportDialogStateSetFn>(original);
    return true;
}
bool InstallPremiumStateCompatibility()
{
    if (!g_dnfBase) return false;
    uintptr_t class1DispatchTarget = g_dnfBase + kClass1DispatchRva;
    uintptr_t sceneUiOpenTarget = g_dnfBase + kSceneUiOpenRva;
    uintptr_t joustSceneBlockCheckTarget = g_dnfBase + kJoustSceneBlockCheckRva;
    uintptr_t joustHistoricTimeGateTarget = g_dnfBase + kJoustHistoricTimeGateRva;
    static const unsigned char kClass1DispatchPrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08
    };
    static const unsigned char kSceneUiOpenPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    static const unsigned char kJoustSceneBlockCheckPrologue[] = {
        0x8B, 0x81, 0xC0, 0x0F, 0x00, 0x00
    };
    static const unsigned char kJoustHistoricTimeGate[] = { 0x75, 0x34 };
    static const unsigned char kJoustHistoricTimeGatePatch[] = { 0xEB, 0x34 };

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(class1DispatchTarget),
                kClass1DispatchPrologue, sizeof(kClass1DispatchPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(sceneUiOpenTarget),
                kSceneUiOpenPrologue, sizeof(kSceneUiOpenPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(joustSceneBlockCheckTarget),
                kJoustSceneBlockCheckPrologue,
                sizeof(kJoustSceneBlockCheckPrologue)) &&
            BytesMatch(reinterpret_cast<unsigned char*>(joustHistoricTimeGateTarget),
                kJoustHistoricTimeGate, sizeof(kJoustHistoricTimeGate))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("premium-state compatibility targets unavailable class1=0x%08X scene_ui=0x%08X joust_scene_gate=0x%08X joust_time_gate=0x%08X",
            class1DispatchTarget, sceneUiOpenTarget, joustSceneBlockCheckTarget,
            joustHistoricTimeGateTarget);
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(class1DispatchTarget,
            kClass1DispatchPrologue, sizeof(kClass1DispatchPrologue),
            reinterpret_cast<void*>(&ProxyClass1Dispatch), &original,
            "aura state class1 cache")) {
        return false;
    }
    g_originalClass1Dispatch = original;

    original = nullptr;
    if (!InstallKnownInlineHook(sceneUiOpenTarget,
            kSceneUiOpenPrologue, sizeof(kSceneUiOpenPrologue),
            reinterpret_cast<void*>(&ProxyAuraSkinSceneUiOpen), &original,
            "aura state deferred avatar panel apply")) {
        return false;
    }
    g_originalSceneUiOpen = original;

    original = nullptr;
    if (!InstallKnownInlineHook(joustSceneBlockCheckTarget,
            kJoustSceneBlockCheckPrologue,
            sizeof(kJoustSceneBlockCheckPrologue),
            reinterpret_cast<void*>(&ProxyJoustSceneBlockCheck), &original,
            "joust owner609 NPC-dialog scene gate")) {
        return false;
    }
    g_originalJoustSceneBlockCheck = original;
    if (!WriteCodePatch(joustHistoricTimeGateTarget,
            kJoustHistoricTimeGatePatch, sizeof(kJoustHistoricTimeGatePatch),
            "joust retired weekend time gate")) {
        return false;
    }
    LogLine("premium-state compatibility installed class1=0x%08X scene_ui=0x%08X joust_scene_gate=0x%08X joust_time_gate=0x%08X joust_caller=+0x%08X wait_ms=%d scope=native-op898-marked-op863-owner609-dialog-and-all-day-joust-gates",
        class1DispatchTarget, sceneUiOpenTarget, joustSceneBlockCheckTarget,
        joustHistoricTimeGateTarget,
        static_cast<unsigned int>(kJoustOpenGateReturnRva), waitedMs);
    return true;
}

bool InstallChannelFollowTypeGateCompatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t target = g_dnfBase + kChannelFollowChannelTypeGateRva;
    // Native: 74 14 = je loc_3266E52C (accept only when the channel type == 0x28).
    // Patched: EB 14 = jmp loc_3266E52C, so every channel type reaches the same
    // native exit and the ordinary-channel rejection disappears.  The two bytes
    // are verified before writing, exactly like the joust time gate above.
    static const unsigned char kNativeGate[] = { 0x74, 0x14 };
    static const unsigned char kAnyTypeGate[] = { 0xEB, 0x14 };
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
            kNativeGate, sizeof(kNativeGate))) {
        const unsigned char* actual = reinterpret_cast<const unsigned char*>(target);
        LogLine("channel-follow type gate unavailable target=0x%08X bytes=%02X %02X %02X %02X expected=74 14",
            target, actual[0], actual[1], actual[2], actual[3]);
        return false;
    }
    if (!WriteCodePatch(target, kAnyTypeGate, sizeof(kAnyTypeGate),
            "channel-follow ordinary-channel type gate")) {
        return false;
    }
    LogLine("channel-follow type gate installed target=0x%08X scope=accept-every-channel-type native-lookup-and-move-unchanged",
        target);
    return true;
}

bool InstallRaidShopDateCompatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t target = g_dnfBase + kActivityDateEntryExistsRva;
    static const unsigned char kNativePrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x18
    };
    void* original = nullptr;
    if (!InstallKnownInlineHook(target,
            kNativePrologue, sizeof(kNativePrologue),
            reinterpret_cast<void*>(&ProxyActivityDateEntryExists), &original,
            "Anton/Luke raid-shop retired activity-date gates")) {
        return false;
    }
    g_originalActivityDateEntryExists = original;
    LogLine("raid-shop date compatibility installed target=0x%08X Naen(entry=%d npc=+0x%08X button=+0x%08X) Erika(entry=%d npc=+0x%08X button=+0x%08X) scope=NPC326/460-only",
        target, kNaenRaidShopActivityDateEntry,
        static_cast<unsigned int>(kNaenRaidShopNpcGateReturnRva),
        static_cast<unsigned int>(kNaenRaidShopButtonGateReturnRva),
        kErikaRaidShopActivityDateEntry,
        static_cast<unsigned int>(kErikaRaidShopNpcGateReturnRva),
        static_cast<unsigned int>(kErikaRaidShopButtonGateReturnRva));
    return true;
}

bool InstallTownCopresenceCompatibility()
{
    uintptr_t actorLookupTarget = g_dnfBase + kActorByObjectKeyRva;
    uintptr_t op24SceneModeTarget = g_dnfBase + kOp24SceneModeRva;
    uintptr_t op24LoadingGateTarget = g_dnfBase + kOp24LoadingGateRva;
    uintptr_t storyDigestGetterTarget =
        g_dnfBase + kStoryDigestManagerGetterRva;
    uintptr_t storyDigestEvaluatorTarget =
        g_dnfBase + kStoryDigestEvaluatorRva;
    // push ebp; mov ebp,esp; mov eax,[dword_51B2728]. All eight bytes are
    // complete instructions and the absolute load is safe in the trampoline.
    static const unsigned char kActorByObjectKeyPrologue[] = {
        0x55, 0x8B, 0xEC, 0xA1, 0x28, 0x27, 0x1B, 0x05
    };
    static const unsigned char kOp24SceneModePrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x08
    };
    static const unsigned char kOp24LoadingGatePrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    static const unsigned char kStoryDigestGetterPrologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF
    };
    static const unsigned char kStoryDigestEvaluatorPrologue[] = {
        0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x0C, 0x53, 0x8B, 0xD9
    };
    if (!BytesMatch(reinterpret_cast<unsigned char*>(op24SceneModeTarget),
            kOp24SceneModePrologue, sizeof(kOp24SceneModePrologue)) ||
        !BytesMatch(reinterpret_cast<unsigned char*>(op24LoadingGateTarget),
            kOp24LoadingGatePrologue, sizeof(kOp24LoadingGatePrologue))) {
        LogLine("town lifecycle op24 gate signature mismatch scene=0x%08X loading=0x%08X",
            op24SceneModeTarget, op24LoadingGateTarget);
        return false;
    }
    bool storyDigestNativeReady =
        BytesMatch(reinterpret_cast<unsigned char*>(storyDigestGetterTarget),
            kStoryDigestGetterPrologue,
            sizeof(kStoryDigestGetterPrologue)) &&
        BytesMatch(reinterpret_cast<unsigned char*>(storyDigestEvaluatorTarget),
            kStoryDigestEvaluatorPrologue,
            sizeof(kStoryDigestEvaluatorPrologue));
    InterlockedExchange(&g_storyDigestRealtimeReady,
        storyDigestNativeReady ? 1 : 0);
    LogLine("story-digest realtime native validation result=%d getter=0x%08X evaluator=0x%08X manager_global=0x%08X last_level_offset=0x%02X",
        storyDigestNativeReady ? 1 : 0,
        storyDigestGetterTarget, storyDigestEvaluatorTarget,
        g_dnfBase + kStoryDigestManagerPointerRva,
        static_cast<unsigned int>(kStoryDigestLastLevelOffset));
    void* original = nullptr;
    if (!InstallKnownInlineHook(
            actorLookupTarget,
            kActorByObjectKeyPrologue,
            sizeof(kActorByObjectKeyPrologue),
            reinterpret_cast<void*>(&ProxyTownActorByObjectKey),
            &original,
            "town co-presence scoped actor-context lookup")) {
        return false;
    }
    g_originalActorByObjectKey = original;

    original = nullptr;
    if (!InstallKnownInlineHook(
            op24SceneModeTarget,
            kOp24SceneModePrologue,
            sizeof(kOp24SceneModePrologue),
            reinterpret_cast<void*>(&ProxyOp24SceneMode),
            &original,
            "town lifecycle op24 scene-mode gate")) {
        return false;
    }
    g_originalOp24SceneMode = original;

    original = nullptr;
    if (!InstallKnownInlineHook(
            op24LoadingGateTarget,
            kOp24LoadingGatePrologue,
            sizeof(kOp24LoadingGatePrologue),
            reinterpret_cast<void*>(&ProxyOp24LoadingGate),
            &original,
            "town lifecycle op24 loading gate")) {
        return false;
    }
    g_originalOp24LoadingGate = original;

    uintptr_t target = g_dnfBase + kClass0DispatchRva;
    static const unsigned char kClass0DispatchPrologue[] = {
        0x55, 0x8B, 0xEC, 0x53, 0x8B, 0x5D, 0x0C
    };
    original = nullptr;
    if (!InstallKnownInlineHook(
            target,
            kClass0DispatchPrologue,
            sizeof(kClass0DispatchPrologue),
            reinterpret_cast<void*>(&ProxyTownCopresenceClass0Dispatch),
            &original,
            "town co-presence class0 owner-context bridge")) {
        return false;
    }
    g_originalClass0Dispatch = original;


    uintptr_t mode0OwnerCompareTarget = g_dnfBase + kMode0OwnerCompareRva;
    static const unsigned char kMode0OwnerCompare[] = {
        0x3B, 0xF1, 0x74, 0x42, 0x0F, 0xB6, 0xD0
    };
    if (!BytesMatch(reinterpret_cast<unsigned char*>(mode0OwnerCompareTarget),
            kMode0OwnerCompare, sizeof(kMode0OwnerCompare))) {
        LogLine("town co-presence mode0 owner compare signature mismatch target=0x%08X",
            mode0OwnerCompareTarget);
        return false;
    }

    g_mode0OwnerRemoteResume = g_dnfBase + kMode0OwnerRemoteRva;
    g_mode0OwnerLocalResume = g_dnfBase + kMode0OwnerLocalRva;
    unsigned char mode0OwnerComparePatch[7] = { 0xE9 };
    intptr_t relative = reinterpret_cast<intptr_t>(&ProxyMode0OwnerCompare) -
        static_cast<intptr_t>(mode0OwnerCompareTarget + 5);
    *reinterpret_cast<int32_t*>(mode0OwnerComparePatch + 1) =
        static_cast<int32_t>(relative);
    mode0OwnerComparePatch[5] = 0x90;
    mode0OwnerComparePatch[6] = 0x90;
    if (!WriteCodePatch(mode0OwnerCompareTarget, mode0OwnerComparePatch,
            sizeof(mode0OwnerComparePatch), "town mode0 current-scene owner compare")) {
        return false;
    }

    LogLine("town co-presence compatibility installed class0=0x%08X actor_lookup=0x%08X mode0_owner=0x%08X op24_scene_gate=0x%08X op24_loading_gate=0x%08X scope=context-fallback-native-visual-interaction-current-actor-and-lua-enter-town",
        target, actorLookupTarget, mode0OwnerCompareTarget,
        op24SceneModeTarget, op24LoadingGateTarget);
    return true;
}

bool InstallLocalActorBindingNullGuard()
{
    uintptr_t target = g_dnfBase + kLocalActorBindingSetterRva;
    // push ebp; mov ebp,esp; mov eax,[ebp+8]. The six copied bytes are whole
    // instructions, and the trampoline resumes at the native store to +0x538.
    static const unsigned char kBindingSetterPrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08
    };

    void* original = nullptr;
    if (!InstallKnownInlineHook(
            target, kBindingSetterPrologue, sizeof(kBindingSetterPrologue),
            reinterpret_cast<void*>(&ProxyLocalActorBindingSetter),
            &original, "local actor optional binding null guard")) {
        return false;
    }
    g_originalLocalActorBindingSetter = original;
    LogLine("local actor optional binding null guard installed target=0x%08X "
        "scope=null-this-setter-only", target);
    return true;
}

bool InstallDungeonPickupCompatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t manualPickupTarget = g_dnfBase + kManualPickupRva;
    static const unsigned char kManualPickupPrologue[] = {
        0x55, 0x8B, 0xEC, 0x81, 0xEC, 0x24, 0x02, 0x00, 0x00
    };

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(manualPickupTarget),
                kManualPickupPrologue, sizeof(kManualPickupPrologue))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("dungeon pickup compatibility target unavailable manual=0x%08X",
            manualPickupTarget);
        return false;
    }

    void* original = nullptr;
    if (!InstallKnownInlineHook(manualPickupTarget,
            kManualPickupPrologue, sizeof(kManualPickupPrologue),
            reinterpret_cast<void*>(&ProxyManualPickup), &original,
            "dungeon manual pickup container repair")) {
        return false;
    }
    g_originalManualPickup = original;

    LogLine("dungeon pickup compatibility installed manual=0x%08X native_ctor=0x%08X wait_ms=%d scope=manual-pickup-controlled-actor-null-container-only",
        manualPickupTarget,
        g_dnfBase + kPickupContainerConstructorRva, waitedMs);
    return true;
}

bool InstallDungeonStartMapStateCompatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t op29Target = g_dnfBase + kDungeonStartMapHandlerRva;
    uintptr_t op211Target = g_dnfBase + kBloodAltarStartMapHandlerRva;
    static const unsigned char kOp29Prologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0x55, 0x81, 0xD6, 0x03
    };
    static const unsigned char kOp211Prologue[] = {
        0x55, 0x8B, 0xEC, 0x6A, 0xFF, 0x68, 0xB2, 0x82, 0xD6, 0x03
    };

    void* original = nullptr;
    if (!InstallKnownInlineHook(op29Target, kOp29Prologue,
            sizeof(kOp29Prologue),
            reinterpret_cast<void*>(&ProxyDungeonStartMapHandler), &original,
            "dungeon op29 derived fog state synchronization")) {
        return false;
    }
    g_originalDungeonStartMapHandler = original;

    original = nullptr;
    if (!InstallKnownInlineHook(op211Target, kOp211Prologue,
            sizeof(kOp211Prologue),
            reinterpret_cast<void*>(&ProxyBloodAltarStartMapHandler), &original,
            "dungeon op211 derived fog state synchronization")) {
        return false;
    }
    g_originalBloodAltarStartMapHandler = original;

    LogLine("dungeon start-map state compatibility installed op29=0x%08X op211=0x%08X direct_fog=0x%08X derived=0x%08X/0x%08X scope=post-native-zero-and-one-mirror",
        op29Target, op211Target,
        g_dnfBase + kDungeonHellPartyFogFlagRva,
        g_dnfBase + kDungeonDerivedFogStateARva,
        g_dnfBase + kDungeonDerivedFogStateBRva);
    return true;
}

bool InstallCreatureRenameCompatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t target = g_dnfBase + kCreatureRenameMapNullCheckRva;
    static const unsigned char kExpected[] = {
        0x83, 0xC4, 0x04, 0x8D, 0x48, 0x14, 0xE8
    };

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(target),
                kExpected, sizeof(kExpected))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("creature rename compatibility target unavailable null_guard=0x%08X",
            target);
        return false;
    }

    g_creatureRenameMapUpdateResume =
        g_dnfBase + kCreatureRenameMapUpdateRva;
    g_creatureRenameMapDoneResume =
        g_dnfBase + kCreatureRenameMapDoneRva;

    unsigned char patch[5] = { 0xE9 };
    intptr_t relative =
        reinterpret_cast<intptr_t>(&ProxyCreatureRenameMapNullCheck) -
        static_cast<intptr_t>(target + 5);
    *reinterpret_cast<int32_t*>(patch + 1) = static_cast<int32_t>(relative);
    if (!WriteCodePatch(target, patch, sizeof(patch),
            "creature rename map null guard")) {
        return false;
    }

    LogLine("creature rename compatibility installed null_guard=0x%08X native_update=0x%08X native_done=0x%08X wait_ms=%d scope=optional-op105-map-copy-only",
        target, g_creatureRenameMapUpdateResume,
        g_creatureRenameMapDoneResume, waitedMs);
    return true;
}

bool InstallTownCreatureStatMapNullGuard()
{
    if (!g_dnfBase) return false;

    uintptr_t target = g_dnfBase + kTownCreatureStatMapNullCheckRva;
    // mov ecx,eax; call sub_25FCAF0
    static const unsigned char kExpected[] = {
        0x8B, 0xC8, 0xE8, 0xB3, 0xA7, 0xFF, 0xFF
    };

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(target),
                kExpected, sizeof(kExpected))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("town creature stat-map null guard target unavailable call_site=0x%08X",
            target);
        return false;
    }

    g_townCreatureStatMapPredicate =
        g_dnfBase + kTownCreatureStatMapPredicateRva;
    g_townCreatureStatMapResume =
        g_dnfBase + kTownCreatureStatMapResumeRva;

    unsigned char patch[7] = { 0xE9 };
    intptr_t relative =
        reinterpret_cast<intptr_t>(&ProxyTownCreatureStatMapNullCheck) -
        static_cast<intptr_t>(target + 5);
    *reinterpret_cast<int32_t*>(patch + 1) = static_cast<int32_t>(relative);
    patch[5] = 0x90;
    patch[6] = 0x90;
    if (!WriteCodePatch(target, patch, sizeof(patch),
            "town equipped-creature stat-map null guard")) {
        return false;
    }

    LogLine("town creature stat-map null guard installed call_site=0x%08X native_predicate=0x%08X native_resume=0x%08X wait_ms=%d scope=sub_2602260-first-optional-op105-lookup-only",
        target, g_townCreatureStatMapPredicate,
        g_townCreatureStatMapResume, waitedMs);
    return true;
}

bool InstallChatNameCacheNullGuard()
{
    if (!g_dnfBase) return false;

    uintptr_t target = g_dnfBase + kChatNameCacheGuardRva;
    // cmp dword ptr [esi+0x4C],0; jne +0x0A; mov ecx,ebx
    static const unsigned char kExpected[] = {
        0x83, 0x7E, 0x4C, 0x00, 0x75, 0x0A, 0x8B
    };

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(target),
                kExpected, sizeof(kExpected))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("chat name-cache null guard target unavailable call_site=0x%08X",
            target);
        return false;
    }

    g_chatNameCachePresentResume =
        g_dnfBase + kChatNameCachePresentResumeRva;
    g_chatNameCacheMissingResume =
        g_dnfBase + kChatNameCacheMissingResumeRva;

    unsigned char patch[7] = { 0xE9 };
    intptr_t relative =
        reinterpret_cast<intptr_t>(&ProxyChatNameCacheNullGuard) -
        static_cast<intptr_t>(target + 5);
    *reinterpret_cast<int32_t*>(patch + 1) = static_cast<int32_t>(relative);
    patch[5] = 0x90;
    patch[6] = 0x90;
    if (!WriteCodePatch(target, patch, sizeof(patch),
            "chat-scene offline/cross-channel name-cache null guard")) {
        return false;
    }

    LogLine("chat name-cache null guard installed call_site=0x%08X present_resume=0x%08X missing_resume=0x%08X wait_ms=%d scope=sub_2E59620-name-cache-fill-only",
        target, g_chatNameCachePresentResume,
        g_chatNameCacheMissingResume, waitedMs);
    return true;
}

bool InstallRaidPanelNullCallGuard()
{
    if (!g_dnfBase) return false;

    uintptr_t target = g_dnfBase + kRaidPanelNullCallRva;
    // mov eax,[ecx]; mov edx,[eax+0x238]; push esi; call edx
    static const unsigned char kExpected[] = {
        0x8B, 0x01,
        0x8B, 0x90, 0x38, 0x02, 0x00, 0x00,
        0x56,
        0xFF, 0xD2
    };

    if (BytesMatch(reinterpret_cast<unsigned char*>(target),
            kExpected, sizeof(kExpected))) {
        g_raidPanelNullCallResume =
            g_dnfBase + kRaidPanelNullCallResumeRva;

        unsigned char patch[sizeof(kExpected)] = { 0xE9 };
        intptr_t relative =
            reinterpret_cast<intptr_t>(&ProxyRaidPanelNullCallGuard) -
            static_cast<intptr_t>(target + 5);
        *reinterpret_cast<int32_t*>(patch + 1) = static_cast<int32_t>(relative);
        for (size_t i = 5; i < sizeof(patch); ++i) {
            patch[i] = 0x90;
        }
        if (WriteCodePatch(target, patch, sizeof(patch),
                "raid panel null vtable-slot call guard")) {
            LogLine("raid panel null call guard installed call_site=0x%08X resume=0x%08X scope=0x021BD2F4-raid-manager-interface-slot-0x238",
                target, g_raidPanelNullCallResume);
            return true;
        }
        return false;
    }

    // Already guarded by an earlier install in this process.
    unsigned char probe[sizeof(kExpected)] = {};
    memcpy(probe, reinterpret_cast<void*>(target), sizeof(probe));
    if (probe[0] == 0xE9) {
        g_raidPanelNullCallResume =
            g_dnfBase + kRaidPanelNullCallResumeRva;
        LogLine("raid panel null call guard already installed call_site=0x%08X",
            target);
        return true;
    }
    LogLine("raid panel null call guard rejected: context mismatch at 0x%08X",
        target);
    return false;
}

bool InstallRaidInstanceFlagNullGuard()
{
    if (!g_dnfBase) return false;

    // cmp byte ptr [eax+0xA58],0
    static const unsigned char kExpected[] = {
        0x80, 0xB8, 0x58, 0x0A, 0x00, 0x00, 0x00
    };
    struct Site {
        uintptr_t readRva;
        uintptr_t resumeRva;
        uintptr_t callRva;
        void* proxy;
        const char* label;
    };
    const Site sites[kRaidInstanceFlagSiteCount] = {
        { kRaidInstanceFlagReadRva, kRaidInstanceFlagResumeRva, kRaidInstanceFlagCallRva,
          reinterpret_cast<void*>(&ProxyRaidInstanceFlagNullGuard0),
          "0x01F95C63" },
        { kRaidInstanceFlagReadRva1, kRaidInstanceFlagResumeRva1, kRaidInstanceFlagCallRva1,
          reinterpret_cast<void*>(&ProxyRaidInstanceFlagNullGuard1),
          "0x01F95611" },
        { kRaidInstanceFlagReadRva2, kRaidInstanceFlagResumeRva2, kRaidInstanceFlagCallRva2,
          reinterpret_cast<void*>(&ProxyRaidInstanceFlagNullGuard2),
          "0x01F9568A" },
    };

    bool allInstalled = true;
    for (int index = 0; index < kRaidInstanceFlagSiteCount; ++index) {
        uintptr_t target = g_dnfBase + sites[index].readRva;
        g_raidInstanceFlagResumes[index] = g_dnfBase + sites[index].resumeRva;
        g_raidInstanceFlagCalls[index] = g_dnfBase + sites[index].callRva;

        if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
                kExpected, sizeof(kExpected))) {
            if (*reinterpret_cast<unsigned char*>(target) == 0xE9) {
                LogLine("raid instance flag null guard already installed site=%d call_site=0x%08X",
                    index, target);
                continue;
            }
            LogLine("raid instance flag null guard rejected: context mismatch at site=%d call_site=0x%08X",
                index, target);
            allInstalled = false;
            continue;
        }

        unsigned char patch[sizeof(kExpected)] = { 0xE9 };
        intptr_t relative =
            reinterpret_cast<intptr_t>(sites[index].proxy) -
            static_cast<intptr_t>(target + 5);
        *reinterpret_cast<int32_t*>(patch + 1) = static_cast<int32_t>(relative);
        patch[5] = 0x90;
        patch[6] = 0x90;
        if (!WriteCodePatch(target, patch, sizeof(patch),
                "raid instance null sub-object flag guard")) {
            allInstalled = false;
            continue;
        }
        LogLine("raid instance flag null guard installed site=%d call_site=0x%08X scope=%s resume=0x%08X native_call=0x%08X",
            index, target, sites[index].label,
            g_raidInstanceFlagResumes[index], g_raidInstanceFlagCalls[index]);
    }
    return allInstalled;
}

bool InstallPetEnchantDisplayCompatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t target = g_dnfBase + kPetItemUpdateDynamicStateRva;
    static const unsigned char kExpected[] = {
        0x0F, 0xBF, 0x85, 0x84, 0xFE, 0xFF, 0xFF
    };

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(target),
                kExpected, sizeof(kExpected))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("pet enchant display compatibility target unavailable same_item=0x%08X",
            target);
        return false;
    }

    g_petItemUpdateDynamicStateResume =
        g_dnfBase + kPetItemUpdateDynamicStateResumeRva;
    unsigned char patch[7] = { 0xE9 };
    intptr_t relative =
        reinterpret_cast<intptr_t>(&ProxyPetItemUpdateDynamicState) -
        static_cast<intptr_t>(target + 5);
    *reinterpret_cast<int32_t*>(patch + 1) = static_cast<int32_t>(relative);
    patch[5] = 0x90;
    patch[6] = 0x90;
    if (!WriteCodePatch(target, patch, sizeof(patch),
            "pet-body list-7 slots 0..139 or equipped slot-26 op14 dynamic-state refresh")) {
        return false;
    }

    LogLine("pet enchant display compatibility installed same_item=0x%08X native_resume=0x%08X wait_ms=%d scope=list7-slots0-139-or-list3-slot26-same-template-op14-native-dynamic-state",
        target, g_petItemUpdateDynamicStateResume, waitedMs);
    return true;
}

bool InstallOp14AcquireNotificationCompatibility()
{
    if (!g_dnfBase) return false;

    uintptr_t target = g_dnfBase + kOp14AcquireNotificationCallRva;
    // NoPack.exe call sub_228FB70 at VA 0x1D7415A.
    static const unsigned char kExpected[] = {
        0xE8, 0x11, 0xBA, 0x51, 0x00
    };
    if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
            kExpected, sizeof(kExpected))) {
        LogLine("op14 acquisition notification compatibility target unavailable "
            "call_site=0x%08X", target);
        return false;
    }

    unsigned char patch[5] = { 0xE8 };
    intptr_t relative =
        reinterpret_cast<intptr_t>(&ProxyItemAcquireNotification) -
        static_cast<intptr_t>(target + 5);
    *reinterpret_cast<int32_t*>(patch + 1) = static_cast<int32_t>(relative);
    if (!WriteCodePatch(target, patch, sizeof(patch),
            "op14 item-specific acquisition notification compatibility")) {
        return false;
    }

    LogLine("op14 acquisition notification compatibility installed "
        "call_site=0x%08X native=0x%08X item=%u scope=toast-only",
        target, g_dnfBase + kItemAcquireNotificationRva,
        kUnsafeMailAcquireNotificationItemID);
    return true;
}

// Historical research installer retained as non-building evidence only. It
// mixed UI, scene, actor, inventory, quest, contract, and debugger behavior
// into the transport DLL. Production must never compile or call this path.
#if 0
DWORD WINAPI InstallWorkerInner(void*)
{
    g_dnfBase = reinterpret_cast<uintptr_t>(GetModuleHandleW(nullptr));
    g_luaGameControlEntrypointsValid = ValidateLuaGameControlEntrypoints();
    if (!g_exceptionTraceHandle) {
        g_exceptionTraceHandle = AddVectoredExceptionHandler(1, TraceUnhandledClientException);
    }
    LogLine("90CN trace enabled exception_handler=%p module_base=0x%08X",
        g_exceptionTraceHandle, g_dnfBase);
    InstallEmbeddedDebugCompatibility();
    LogProtocolPacket("LOGGER", 0, nullptr, 0);

    g_checksumFn = g_dnfBase + kChecksumRva;
    g_gameAllocatorFn = g_dnfBase + kGameAllocatorRva;
    g_gameMemcpyFn = g_dnfBase + kGameMemcpyRva;
    g_dprotoOutgoingResume = g_dnfBase + kDprotoOutgoingResumeRva;
    g_dprotoOutgoingReturn = g_dnfBase + kDprotoOutgoingReturnRva;
    g_dprotoFalseResume = g_dnfBase + kDprotoFalseResumeRva;
    g_mode0OwnerRemoteResume = g_dnfBase + kMode0OwnerRemoteRva;
    g_mode0OwnerLocalResume = g_dnfBase + kMode0OwnerLocalRva;
    g_mode3OwnerRemoteResume = g_dnfBase + kMode3OwnerRemoteResumeRva;
    g_mode3OwnerLocalResume = g_dnfBase + kMode3OwnerLocalResumeRva;
    g_mode3OwnerFinalizeRemoteResume =
        g_dnfBase + kMode3OwnerFinalizeRemoteResumeRva;
    g_mode3OwnerFinalizeLocalResume =
        g_dnfBase + kMode3OwnerFinalizeLocalResumeRva;
    g_mode3LocalOwnerChannelAddress = g_dnfBase + kLocalOwnerChannelRva;
    g_creatureRenameMapUpdateResume = g_dnfBase + kCreatureRenameMapUpdateRva;
    g_creatureRenameMapDoneResume = g_dnfBase + kCreatureRenameMapDoneRva;
    g_autoRepairAuxLookupFn = g_dnfBase + kInventorySelectionLookupRva;
    g_autoRepairAuxLookupResume = g_dnfBase + kAutoRepairAuxLookupResumeRva;
    g_autoRepairAuxLoopDone = g_dnfBase + kAutoRepairAuxLoopDoneRva;
    g_questAutoCompleteSendFn = g_dnfBase + kQuestAutoCompleteSendRva;

    uintptr_t cipherTarget = g_dnfBase + kCipherEncodeRva;
    uintptr_t cipherDecodeTarget = g_dnfBase + kCipherDecodeRva;
    uintptr_t cipherCallTarget = g_dnfBase + kCipherEncodeCallRva;
    uintptr_t routePredicateTarget = g_dnfBase + kDprotoRoutePredicateRva;
    uintptr_t outgoingGateTarget = g_dnfBase + kDprotoOutgoingGateRva;
    uintptr_t outgoingCloneTarget = g_dnfBase + kDprotoOutgoingCloneRva;
    uintptr_t selectedPageApplyTarget = g_dnfBase + kSelectedPageApplyRva;
    uintptr_t selectorCreateTickTarget = g_dnfBase + kSelectorCreateTickRva;
    uintptr_t selectorCreateTransitionTarget =
        g_dnfBase + kSelectorCreateTransitionRva;
    uintptr_t createUIClickTarget = g_dnfBase + kCreateUIClickRva;
    uintptr_t createUIOpenTarget = g_dnfBase + kCreateUIOpenRva;
    uintptr_t upperCreateSendTarget = g_dnfBase + kUpperCreateSendRva;
    uintptr_t class0DispatchTarget = g_dnfBase + kClass0DispatchRva;
    uintptr_t class1DispatchTarget = g_dnfBase + kClass1DispatchRva;
    uintptr_t sceneUiOpenTarget = g_dnfBase + kSceneUiOpenRva;
    uintptr_t localActorCreateTarget = g_dnfBase + kLocalActorCreateRva;
    uintptr_t mode0OwnerCompareTarget = g_dnfBase + kMode0OwnerCompareRva;
    uintptr_t mode3OwnerResolveTarget = g_dnfBase + kMode3OwnerResolveRva;
    uintptr_t mode3OwnerFinalizeTarget = g_dnfBase + kMode3OwnerFinalizeRva;
    uintptr_t pickupContainerGetterTarget =
        g_dnfBase + kPickupContainerGetterRva;
    uintptr_t manualPickupTarget = g_dnfBase + kManualPickupRva;
    uintptr_t creatureRenameMapNullCheckTarget =
        g_dnfBase + kCreatureRenameMapNullCheckRva;
    uintptr_t autoRepairAuxLookupTarget =
        g_dnfBase + kAutoRepairAuxLookupRva;
    uintptr_t op24SceneModeTarget = g_dnfBase + kOp24SceneModeRva;
    uintptr_t op24LoadingGateTarget = g_dnfBase + kOp24LoadingGateRva;
    uintptr_t questAutoCompleteCallContext = g_dnfBase + kQuestAutoCompleteCallContextRva;
    uintptr_t questAutoCompleteCallTarget = g_dnfBase + kQuestAutoCompleteCallRva;
    uintptr_t questAutoCompleteCaptureTarget = g_dnfBase + kQuestAutoCompleteCaptureRva;
    // The op1128 handler's unconditional trailing toast uses legacy string
    // ids that this build maps to an unrelated cross-class purchase warning.
    // The native result window already owns the complete ten-open result.
    uintptr_t magicBoxFollowupToastTarget = g_dnfBase + 0x019081CF;
    static const unsigned char kNativePrologue[] = { 0x55, 0x8B, 0xEC, 0x53, 0x8B, 0x5D };
    static const unsigned char kSelectedPageApplyPrologue[] = { 0x55, 0x8B, 0xEC, 0x6A, 0xFF };
    static const unsigned char kSelectorCreateTickPrologue[] = {
        0x56, 0x8B, 0xF1, 0x8B, 0x0D, 0x64, 0x27, 0x1B, 0x05
    };
    static const unsigned char kSelectorCreateTransitionPrologue[] = {
        0x56, 0x57, 0x8B, 0xF9, 0x8B, 0xB7, 0xA8, 0x00, 0x00, 0x00
    };
    static const unsigned char kCreateUIClickPrologue[] = {
        0x55, 0x8B, 0xEC, 0x51, 0x53, 0x56, 0x57
    };
    static const unsigned char kCreateUIOpenPrologue[] = { 0x55, 0x8B, 0xEC, 0x6A, 0xFF };
    static const unsigned char kUpperCreateSendPrologue[] = { 0x55, 0x8B, 0xEC, 0x51, 0x56 };
    static const unsigned char kClass0DispatchPrologue[] = { 0x55, 0x8B, 0xEC, 0x53, 0x8B, 0x5D, 0x0C };
    static const unsigned char kClass1DispatchPrologue[] = { 0x55, 0x8B, 0xEC, 0x8B, 0x45, 0x08 };
    static const unsigned char kSceneUiOpenPrologue[] = { 0x55, 0x8B, 0xEC, 0x6A, 0xFF };
    static const unsigned char kLocalActorCreatePrologue[] = { 0x55, 0x8B, 0xEC, 0x51, 0x57 };
    static const unsigned char kMode0OwnerCompare[] = { 0x3B, 0xF1, 0x74, 0x42, 0x0F, 0xB6, 0xD0 };
    static const unsigned char kMode3OwnerResolve[] = {
        0x3B, 0xCE, 0x74, 0x3F, 0x8B, 0xB5, 0x50, 0xFB, 0xFF, 0xFF
    };
    static const unsigned char kMode3OwnerFinalize[] = {
        0x0F, 0xB6, 0x05, 0x49, 0x9C, 0x19, 0x05, 0x3B, 0xC3, 0x74, 0x09
    };
    static const unsigned char kPickupContainerGetterPrologue[] = {
        0x8B, 0x81, 0x6C, 0x64, 0x00, 0x00
    };
    static const unsigned char kManualPickupPrologue[] = {
        0x55, 0x8B, 0xEC, 0x81, 0xEC, 0x24, 0x02, 0x00, 0x00
    };
    static const unsigned char kCreatureRenameMapNullCheck[] = {
        0x83, 0xC4, 0x04, 0x8D, 0x48, 0x14, 0xE8
    };
    static const unsigned char kAutoRepairAuxLookup[] = {
        0x8B, 0xC8, 0xE8, 0xDA, 0xD0, 0x2B, 0x00
    };
    static const unsigned char kOp24SceneModePrologue[] = { 0x55, 0x8B, 0xEC, 0x83, 0xEC, 0x08 };
    static const unsigned char kOp24LoadingGatePrologue[] = { 0x55, 0x8B, 0xEC, 0x6A, 0xFF };
    static const unsigned char kCipherEncodeCall[] = { 0xE8, 0x3B, 0x46, 0xF5, 0xFF };
    static const unsigned char kRoutePredicatePrologue[] = { 0x55, 0x8B, 0xEC, 0x8B, 0x49, 0x0C };
    static const unsigned char kOutgoingGate[] = { 0x0F, 0x84, 0x2F, 0x01, 0x00, 0x00 };
    static const unsigned char kOutgoingClonePrologue[] = { 0x8B, 0x4B, 0x0C, 0x8B, 0x01 };
    static const unsigned char kQuestAutoCompleteCallContext[] = {
        0x8B, 0x0D, 0x98, 0x27, 0x1B, 0x05, 0x6A, 0xFF, 0x6A, 0x01,
        0xE8, 0x29, 0x07, 0x9A, 0xFE
    };
    static const unsigned char kQuestAutoCompleteCapture[] = { 0x89, 0x5E, 0x30 };
    static const unsigned char kMagicBoxFollowupToastCall[] = {
        0xE8, 0xEC, 0x77, 0x56, 0x00
    };

    LogLine("cipher worker started (module base 0x%08X)", g_dnfBase);
    InstallUiCompatibilityHook();
    InstallMultiClientCompatibilityHook();

    // TCLS launch argument fetches run before the first network connection.
    // Install only these six local helpers immediately.
    bool tclsInstalled = InstallTclsCompatibility();
    LogLine("TCLS local launch compatibility result=%d", tclsInstalled ? 1 : 0);

    bool channelDiagnosticInstalled = InstallChannelDiagnostic();
    LogLine("channel diagnostic result=%d", channelDiagnosticInstalled ? 1 : 0);
    bool channelUiCompatibilityInstalled = InstallChannelUiCompatibility();
    LogLine("channel UI compatibility result=%d",
        channelUiCompatibilityInstalled ? 1 : 0);

    LogLine("TCLS installed; continuing to install source-built sub_33AAFE0/sub_33AB0A0 upper-body plaintext bypass plus bounded scene route allow-list");

    // These four used to be environment switches (DNF_CIPHER_PASSTHROUGH,
    // DNF_DPROTO_COMPAT, DNF_ROUTE_COMPAT, DNF_QUEST_SINGLE_TARGET). They are
    // constants now -- see clientConfig at the top of this file.
    constexpr bool enableCipherPassthrough = kCipherPassthroughEnabled;
    if (!enableCipherPassthrough) {
        LogLine("cipher passthrough disabled (compile-time configuration)");
        return 0;
    }

    // DPROTO is not an ordinary body codec. The bridge must receive the
    // current EXE's finalized inner packets so dungeon combat, drops,
    // settlement, inventory, and UI routes can be handled by the server
    // without the external route/DPROTO PS1 script. This DLL ports the proven
    // PS1 shape: clone the predicate-true finalized packet, clone only the
    // measured predicate-false opcodes, and keep inbound route admission on the
    // bounded scene allow-list.
    constexpr bool enableDprotoCompat = kDprotoCompatEnabled;

    // The narrow inbound scene/UI route predicate. This is not a send/recv hook
    // and it does not modify packet bytes; it only admits proven current-scene
    // opcodes to the native direct upper readers.
    constexpr bool enableSceneRouteCompat = kSceneRouteCompatEnabled;

    // The bottom task-completion confirmation already captures the selected row
    // id, so the exact-row sender is what this DLL sends.
    constexpr bool enableQuestSingleTarget = kQuestSingleTargetEnabled;

    // Install as soon as the packed EXE has restored the verified native
    // prologues.  A fixed post-TCLS sleep misses the early GET_USERINFO path:
    // the client has already emitted the role-select handshake before the
    // plaintext upper-body proxy is active, so the server receives a legacy
    // packet stream and the selector renders black.  The byte-signature wait
    // below is the safety gate; do not add an unconditional startup delay here.
    constexpr int kBootstrapDelayMs = 0;
    if (kBootstrapDelayMs > 0) {
        Sleep(kBootstrapDelayMs);
    }

    // The packed EXE restores .text late. Refuse to patch unknown code.
    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        bool cipherReady = BytesMatch(reinterpret_cast<unsigned char*>(cipherTarget), kNativePrologue, sizeof(kNativePrologue));
        bool cipherDecodeReady = BytesMatch(reinterpret_cast<unsigned char*>(cipherDecodeTarget), kNativePrologue, sizeof(kNativePrologue));
        bool selectedPageApplyReady = BytesMatch(reinterpret_cast<unsigned char*>(selectedPageApplyTarget),
            kSelectedPageApplyPrologue, sizeof(kSelectedPageApplyPrologue));
        bool createUIClickReady = BytesMatch(reinterpret_cast<unsigned char*>(createUIClickTarget),
            kCreateUIClickPrologue, sizeof(kCreateUIClickPrologue));
        bool createUIOpenReady = BytesMatch(reinterpret_cast<unsigned char*>(createUIOpenTarget),
            kCreateUIOpenPrologue, sizeof(kCreateUIOpenPrologue));
        bool upperCreateSendReady = BytesMatch(reinterpret_cast<unsigned char*>(upperCreateSendTarget),
            kUpperCreateSendPrologue, sizeof(kUpperCreateSendPrologue));
        bool localActorCreateReady = BytesMatch(reinterpret_cast<unsigned char*>(localActorCreateTarget),
            kLocalActorCreatePrologue, sizeof(kLocalActorCreatePrologue));
        bool mode0OwnerCompareReady = BytesMatch(reinterpret_cast<unsigned char*>(mode0OwnerCompareTarget),
            kMode0OwnerCompare, sizeof(kMode0OwnerCompare));
        bool mode3OwnerResolveReady = BytesMatch(reinterpret_cast<unsigned char*>(mode3OwnerResolveTarget),
            kMode3OwnerResolve, sizeof(kMode3OwnerResolve));
        bool mode3OwnerFinalizeReady = BytesMatch(
            reinterpret_cast<unsigned char*>(mode3OwnerFinalizeTarget),
            kMode3OwnerFinalize, sizeof(kMode3OwnerFinalize));
        bool pickupContainerGetterReady = BytesMatch(
            reinterpret_cast<unsigned char*>(pickupContainerGetterTarget),
            kPickupContainerGetterPrologue, sizeof(kPickupContainerGetterPrologue));
        bool manualPickupReady = BytesMatch(
            reinterpret_cast<unsigned char*>(manualPickupTarget),
            kManualPickupPrologue, sizeof(kManualPickupPrologue));
        bool creatureRenameMapNullCheckReady = BytesMatch(
            reinterpret_cast<unsigned char*>(creatureRenameMapNullCheckTarget),
            kCreatureRenameMapNullCheck, sizeof(kCreatureRenameMapNullCheck));
        bool autoRepairAuxLookupReady = BytesMatch(
            reinterpret_cast<unsigned char*>(autoRepairAuxLookupTarget),
            kAutoRepairAuxLookup, sizeof(kAutoRepairAuxLookup));
        bool op24SceneModeReady = BytesMatch(reinterpret_cast<unsigned char*>(op24SceneModeTarget),
            kOp24SceneModePrologue, sizeof(kOp24SceneModePrologue));
        bool op24LoadingGateReady = BytesMatch(reinterpret_cast<unsigned char*>(op24LoadingGateTarget),
            kOp24LoadingGatePrologue, sizeof(kOp24LoadingGatePrologue));
        bool routeReady = !enableSceneRouteCompat ||
            BytesMatch(reinterpret_cast<unsigned char*>(routePredicateTarget), kRoutePredicatePrologue, sizeof(kRoutePredicatePrologue));
        bool dprotoReady = !enableDprotoCompat ||
            (BytesMatch(reinterpret_cast<unsigned char*>(routePredicateTarget), kRoutePredicatePrologue, sizeof(kRoutePredicatePrologue)) &&
             BytesMatch(reinterpret_cast<unsigned char*>(outgoingGateTarget), kOutgoingGate, sizeof(kOutgoingGate)) &&
             BytesMatch(reinterpret_cast<unsigned char*>(outgoingCloneTarget), kOutgoingClonePrologue, sizeof(kOutgoingClonePrologue)));
        bool questSingleTargetReady = !enableQuestSingleTarget ||
            (BytesMatch(reinterpret_cast<unsigned char*>(questAutoCompleteCallContext),
                kQuestAutoCompleteCallContext, sizeof(kQuestAutoCompleteCallContext)) &&
             BytesMatch(reinterpret_cast<unsigned char*>(questAutoCompleteCaptureTarget),
                kQuestAutoCompleteCapture, sizeof(kQuestAutoCompleteCapture)));
        bool magicBoxReady =
            BytesMatch(reinterpret_cast<unsigned char*>(magicBoxFollowupToastTarget),
                kMagicBoxFollowupToastCall, sizeof(kMagicBoxFollowupToastCall));
        if (cipherReady && cipherDecodeReady && selectedPageApplyReady &&
            createUIClickReady && createUIOpenReady && upperCreateSendReady &&
            localActorCreateReady &&
            mode0OwnerCompareReady && mode3OwnerResolveReady &&
            mode3OwnerFinalizeReady && pickupContainerGetterReady &&
            manualPickupReady &&
            creatureRenameMapNullCheckReady &&
            autoRepairAuxLookupReady &&
            op24SceneModeReady && op24LoadingGateReady &&
            routeReady && dprotoReady && questSingleTargetReady && magicBoxReady) break;

        unsigned char firstByte = 0;
        if (TryReadByte(reinterpret_cast<unsigned char*>(cipherTarget), &firstByte) && firstByte == 0xE9) {
            LogLine("cipher encode function already hooked; fresh DNF process required for this build");
            return 0;
        }
        if (TryReadByte(reinterpret_cast<unsigned char*>(cipherDecodeTarget), &firstByte) && firstByte == 0xE9) {
            LogLine("cipher decode function already hooked; fresh DNF process required for this build");
            return 0;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("native targets did not reach verified bytes; hook not installed route=%d dproto=%d quest_single=%d",
            enableSceneRouteCompat ? 1 : 0, enableDprotoCompat ? 1 : 0,
            enableQuestSingleTarget ? 1 : 0);
        return 0;
    }

    unsigned char entryPatch[5] = { 0xE8 };
    entryPatch[0] = 0xE9;
    intptr_t relative = reinterpret_cast<intptr_t>(&ProxyCipherEncodeFunction) - static_cast<intptr_t>(cipherTarget + 5);
    *reinterpret_cast<int32_t*>(entryPatch + 1) = static_cast<int32_t>(relative);
    if (!WriteCodePatch(cipherTarget, entryPatch, sizeof(entryPatch), "upper body encode function")) return 0;

    unsigned char decodePatch[5] = { 0xE9 };
    relative = reinterpret_cast<intptr_t>(&ProxyCipherDecodeFunction) - static_cast<intptr_t>(cipherDecodeTarget + 5);
    *reinterpret_cast<int32_t*>(decodePatch + 1) = static_cast<int32_t>(relative);
    if (!WriteCodePatch(cipherDecodeTarget, decodePatch, sizeof(decodePatch), "upper body decode function")) return 0;

    if (enableSceneRouteCompat) {
        unsigned char routeAllowPatch[6] = { 0xE9 };
        intptr_t routeRelative = reinterpret_cast<intptr_t>(&ProxySceneRouteAllowList) - static_cast<intptr_t>(routePredicateTarget + 5);
        *reinterpret_cast<int32_t*>(routeAllowPatch + 1) = static_cast<int32_t>(routeRelative);
        routeAllowPatch[5] = 0x90;
        if (!WriteCodePatch(routePredicateTarget, routeAllowPatch, sizeof(routeAllowPatch), "scene route allow-list")) return 0;
    }

    if (enableDprotoCompat) {
        unsigned char clonePatch[5] = { 0xE9 };
        intptr_t cloneRelative = reinterpret_cast<intptr_t>(&ProxyDprotoSendDirect) - static_cast<intptr_t>(outgoingCloneTarget + 5);
        *reinterpret_cast<int32_t*>(clonePatch + 1) = static_cast<int32_t>(cloneRelative);

        unsigned char falsePatch[6] = { 0x0F, 0x84 };
        intptr_t falseRelative = reinterpret_cast<intptr_t>(&ProxyDprotoFalseSelective) - static_cast<intptr_t>(outgoingGateTarget + 6);
        *reinterpret_cast<int32_t*>(falsePatch + 2) = static_cast<int32_t>(falseRelative);

        if (!WriteCodePatch(outgoingCloneTarget, clonePatch, sizeof(clonePatch), "dproto outbound predicate-true clone") ||
            !WriteCodePatch(outgoingGateTarget, falsePatch, sizeof(falsePatch), "dproto outbound predicate-false selective")) {
            LogLine("DPROTO compatibility patch incomplete; fresh DNF process required");
            return 0;
        }
    }

    static const unsigned char kMagicBoxToastNops[] = {
        0x90, 0x90, 0x90, 0x90, 0x90
    };
    if (!WriteCodePatch(magicBoxFollowupToastTarget,
            kMagicBoxToastNops, sizeof(kMagicBoxToastNops),
            "seria ten-open stale follow-up toast")) {
        return 0;
    }
    LogLine("seria ten-open compatibility installed toast=0x%08X",
        magicBoxFollowupToastTarget);

    if (enableQuestSingleTarget) {
        unsigned char questSingleTargetPatch[5] = { 0xE8 };
        intptr_t questRelative = reinterpret_cast<intptr_t>(&ProxyQuestAutoCompleteSelected) -
            static_cast<intptr_t>(questAutoCompleteCallTarget + 5);
        *reinterpret_cast<int32_t*>(questSingleTargetPatch + 1) = static_cast<int32_t>(questRelative);
        if (!WriteCodePatch(questAutoCompleteCallTarget, questSingleTargetPatch,
                sizeof(questSingleTargetPatch), "quest selected single-target confirmation")) {
            LogLine("quest selected single-target patch failed; native zero-selector remains");
            return 0;
        }
        LogLine("quest selected single-target installed call=0x%08X capture=0x%08X native_send=0x%08X",
            questAutoCompleteCallTarget, questAutoCompleteCaptureTarget, g_questAutoCompleteSendFn);
    }

    unsigned char mode0OwnerComparePatch[7] = { 0xE9 };
    relative = reinterpret_cast<intptr_t>(&ProxyMode0OwnerCompare) -
        static_cast<intptr_t>(mode0OwnerCompareTarget + 5);
    *reinterpret_cast<int32_t*>(mode0OwnerComparePatch + 1) = static_cast<int32_t>(relative);
    mode0OwnerComparePatch[5] = 0x90;
    mode0OwnerComparePatch[6] = 0x90;
    if (!WriteCodePatch(mode0OwnerCompareTarget, mode0OwnerComparePatch,
            sizeof(mode0OwnerComparePatch), "mode0 owner compare diagnostic")) {
        return 0;
    }

    unsigned char mode3OwnerResolvePatch[10] = { 0xE9 };
    relative = reinterpret_cast<intptr_t>(&ProxyMode3OwnerResolve) -
        static_cast<intptr_t>(mode3OwnerResolveTarget + 5);
    *reinterpret_cast<int32_t*>(mode3OwnerResolvePatch + 1) =
        static_cast<int32_t>(relative);
    for (size_t i = 5; i < sizeof(mode3OwnerResolvePatch); ++i) {
        mode3OwnerResolvePatch[i] = 0x90;
    }
    if (!WriteCodePatch(mode3OwnerResolveTarget, mode3OwnerResolvePatch,
            sizeof(mode3OwnerResolvePatch), "mode3 current-scene owner resolve")) {
        return 0;
    }

    unsigned char mode3OwnerFinalizePatch[11] = { 0xE9 };
    relative = reinterpret_cast<intptr_t>(&ProxyMode3OwnerFinalize) -
        static_cast<intptr_t>(mode3OwnerFinalizeTarget + 5);
    *reinterpret_cast<int32_t*>(mode3OwnerFinalizePatch + 1) =
        static_cast<int32_t>(relative);
    for (size_t i = 5; i < sizeof(mode3OwnerFinalizePatch); ++i) {
        mode3OwnerFinalizePatch[i] = 0x90;
    }
    if (!WriteCodePatch(mode3OwnerFinalizeTarget, mode3OwnerFinalizePatch,
            sizeof(mode3OwnerFinalizePatch),
            "mode3 current-scene owner finalization")) {
        return 0;
    }
    LogLine("mode3 current-scene compatibility installed resolve=0x%08X finalize=0x%08X local=0x%08X remote=0x%08X",
        mode3OwnerResolveTarget, mode3OwnerFinalizeTarget,
        g_mode3OwnerFinalizeLocalResume, g_mode3OwnerFinalizeRemoteResume);

    void* pickupContainerGetterOriginal = nullptr;
    if (!InstallKnownInlineHook(pickupContainerGetterTarget,
            kPickupContainerGetterPrologue,
            sizeof(kPickupContainerGetterPrologue),
            reinterpret_cast<void*>(&ProxyPickupContainerGetter),
            &pickupContainerGetterOriginal,
            "dungeon pickup container getter repair")) {
        return 0;
    }
    g_originalPickupContainerGetter = pickupContainerGetterOriginal;

    void* manualPickupOriginal = nullptr;
    if (!InstallKnownInlineHook(manualPickupTarget,
            kManualPickupPrologue, sizeof(kManualPickupPrologue),
            reinterpret_cast<void*>(&ProxyManualPickup),
            &manualPickupOriginal,
            "dungeon manual pickup container repair")) {
        return 0;
    }
    g_originalManualPickup = manualPickupOriginal;
    LogLine("dungeon pickup compatibility installed getter=0x%08X manual=0x%08X native_ctor=0x%08X",
        pickupContainerGetterTarget, manualPickupTarget,
        g_dnfBase + kPickupContainerConstructorRva);

    unsigned char creatureRenameNullPatch[5] = { 0xE9 };
    relative = reinterpret_cast<intptr_t>(&ProxyCreatureRenameMapNullCheck) -
        static_cast<intptr_t>(creatureRenameMapNullCheckTarget + 5);
    *reinterpret_cast<int32_t*>(creatureRenameNullPatch + 1) =
        static_cast<int32_t>(relative);
    if (!WriteCodePatch(creatureRenameMapNullCheckTarget,
            creatureRenameNullPatch, sizeof(creatureRenameNullPatch),
            "creature rename map null guard")) {
        return 0;
    }
    LogLine("creature rename compatibility installed null_guard=0x%08X native_update=0x%08X",
        creatureRenameMapNullCheckTarget, g_creatureRenameMapUpdateResume);

    unsigned char autoRepairAuxNullPatch[7] = { 0xE9 };
    relative = reinterpret_cast<intptr_t>(&ProxyAutoRepairAuxContainerLookup) -
        static_cast<intptr_t>(autoRepairAuxLookupTarget + 5);
    *reinterpret_cast<int32_t*>(autoRepairAuxNullPatch + 1) =
        static_cast<int32_t>(relative);
    autoRepairAuxNullPatch[5] = 0x90;
    autoRepairAuxNullPatch[6] = 0x90;
    if (!WriteCodePatch(autoRepairAuxLookupTarget,
            autoRepairAuxNullPatch, sizeof(autoRepairAuxNullPatch),
            "op903 auto-repair auxiliary equipment null guard")) {
        return 0;
    }
    LogLine("auto-repair auxiliary equipment compatibility installed null_guard=0x%08X native_lookup=0x%08X",
        autoRepairAuxLookupTarget, g_autoRepairAuxLookupFn);

    void* selectedPageApplyOriginal = nullptr;
    if (!InstallKnownInlineHook(selectedPageApplyTarget,
            kSelectedPageApplyPrologue, sizeof(kSelectedPageApplyPrologue),
            reinterpret_cast<void*>(&ProxySelectedPageApply), &selectedPageApplyOriginal,
            "selected page null guard")) {
        return 0;
    }
    g_originalSelectedPageApply = selectedPageApplyOriginal;

    if (BytesMatch(reinterpret_cast<unsigned char*>(selectorCreateTickTarget),
            kSelectorCreateTickPrologue, sizeof(kSelectorCreateTickPrologue))) {
        void* selectorCreateTickOriginal = nullptr;
        if (InstallKnownInlineHook(selectorCreateTickTarget,
                kSelectorCreateTickPrologue, sizeof(kSelectorCreateTickPrologue),
                reinterpret_cast<void*>(&ProxySelectorCreateTick), &selectorCreateTickOriginal,
                "selector create gate trace")) {
            g_originalSelectorCreateTick = selectorCreateTickOriginal;
        } else {
            LogLine("selector create gate trace unavailable; compatibility remains active");
        }
    } else {
        LogLine("selector create gate trace skipped: prologue mismatch");
    }

    if (BytesMatch(reinterpret_cast<unsigned char*>(selectorCreateTransitionTarget),
            kSelectorCreateTransitionPrologue,
            sizeof(kSelectorCreateTransitionPrologue))) {
        void* selectorCreateTransitionOriginal = nullptr;
        if (InstallKnownInlineHook(selectorCreateTransitionTarget,
                kSelectorCreateTransitionPrologue,
                sizeof(kSelectorCreateTransitionPrologue),
                reinterpret_cast<void*>(&ProxySelectorCreateTransition),
                &selectorCreateTransitionOriginal,
                "selector create transition trace")) {
            g_originalSelectorCreateTransition = selectorCreateTransitionOriginal;
        } else {
            LogLine("selector create transition trace unavailable; compatibility remains active");
        }
    } else {
        LogLine("selector create transition trace skipped: prologue mismatch");
    }

    void* createUIClickOriginal = nullptr;
    if (!InstallKnownInlineHook(createUIClickTarget,
            kCreateUIClickPrologue, sizeof(kCreateUIClickPrologue),
            reinterpret_cast<void*>(&ProxyCreateUIClick), &createUIClickOriginal,
            "create UI click trace")) {
        return 0;
    }
    g_originalCreateUIClick = createUIClickOriginal;

    void* createUIOpenOriginal = nullptr;
    if (!InstallKnownInlineHook(createUIOpenTarget,
            kCreateUIOpenPrologue, sizeof(kCreateUIOpenPrologue),
            reinterpret_cast<void*>(&ProxyCreateUIOpen), &createUIOpenOriginal,
            "create UI open trace")) {
        return 0;
    }
    g_originalCreateUIOpen = createUIOpenOriginal;

    void* upperCreateSendOriginal = nullptr;
    if (!InstallKnownInlineHook(upperCreateSendTarget,
            kUpperCreateSendPrologue, sizeof(kUpperCreateSendPrologue),
            reinterpret_cast<void*>(&ProxyUpperCreateSend), &upperCreateSendOriginal,
            "upper create send trace")) {
        return 0;
    }
    g_originalUpperCreateSend = upperCreateSendOriginal;

    void* class0DispatchOriginal = nullptr;
    if (!InstallKnownInlineHook(class0DispatchTarget,
            kClass0DispatchPrologue, sizeof(kClass0DispatchPrologue),
            reinterpret_cast<void*>(&ProxyClass0Dispatch), &class0DispatchOriginal,
            "class0 dispatch diagnostic")) {
        return 0;
    }
    g_originalClass0Dispatch = class0DispatchOriginal;


    void* class1DispatchOriginal = nullptr;
    if (!InstallKnownInlineHook(class1DispatchTarget,
            kClass1DispatchPrologue, sizeof(kClass1DispatchPrologue),
            reinterpret_cast<void*>(&ProxyClass1Dispatch), &class1DispatchOriginal,
            "class1 dispatch diagnostic")) {
        return 0;
    }
    g_originalClass1Dispatch = class1DispatchOriginal;

    void* sceneUiOpenOriginal = nullptr;
    if (!InstallKnownInlineHook(sceneUiOpenTarget,
            kSceneUiOpenPrologue, sizeof(kSceneUiOpenPrologue),
            reinterpret_cast<void*>(&ProxySceneUiOpen), &sceneUiOpenOriginal,
            "aura skin deferred avatar panel unlock")) {
        return 0;
    }
    g_originalSceneUiOpen = sceneUiOpenOriginal;

    void* localActorCreateOriginal = nullptr;
    if (!InstallKnownInlineHook(localActorCreateTarget,
            kLocalActorCreatePrologue, sizeof(kLocalActorCreatePrologue),
            reinterpret_cast<void*>(&ProxyLocalActorCreate), &localActorCreateOriginal,
            "local actor create diagnostic")) {
        return 0;
    }
    g_originalLocalActorCreate = localActorCreateOriginal;

    void* op24SceneModeOriginal = nullptr;
    if (!InstallKnownInlineHook(op24SceneModeTarget,
            kOp24SceneModePrologue, sizeof(kOp24SceneModePrologue),
            reinterpret_cast<void*>(&ProxyOp24SceneMode), &op24SceneModeOriginal,
            "op24 scene mode diagnostic")) {
        return 0;
    }
    g_originalOp24SceneMode = op24SceneModeOriginal;

    void* op24LoadingGateOriginal = nullptr;
    if (!InstallKnownInlineHook(op24LoadingGateTarget,
            kOp24LoadingGatePrologue, sizeof(kOp24LoadingGatePrologue),
            reinterpret_cast<void*>(&ProxyOp24LoadingGate), &op24LoadingGateOriginal,
            "op24 loading gate diagnostic")) {
        return 0;
    }
    g_originalOp24LoadingGate = op24LoadingGateOriginal;

    // REQUEST_PEER/RESPONSE_PEER are the current EXE's town-player
    // interaction handshake. Log both registries before the server admits a
    // new response body so their concrete callbacks can be audited without
    // executing an unproved packet.
    TraceDispatchRegistryEntry(0, 10);
    TraceDispatchRegistryEntry(1, 10);
    TraceDispatchRegistryEntry(0, 11);
    TraceDispatchRegistryEntry(1, 11);
    LogLine("compatibility DLL installed upper_body_bypass=1 upper_decode_bypass=1 scene_route=%d encode_function=0x%08X decode_function=0x%08X dproto=%d dproto_true=0x%08X dproto_false=0x%08X route=0x%08X quest_single=%d quest_call=0x%08X bootstrap_delay_ms=%d native_wait_ms=%d",
        enableSceneRouteCompat ? 1 : 0, cipherTarget, cipherDecodeTarget,
        enableDprotoCompat ? 1 : 0, outgoingCloneTarget, outgoingGateTarget,
        routePredicateTarget, enableQuestSingleTarget ? 1 : 0,
        questAutoCompleteCallTarget, kBootstrapDelayMs, waitedMs);

    if (IsSocketTraceEnabled()) {
        bool socketTraceInstalled = InstallSocketOpenTrace();
        LogLine("socket-trace configuration enabled installed=%d", socketTraceInstalled ? 1 : 0);
    } else {
        LogLine("socket-trace disabled (kSocketTraceEnabled is false)");
    }
    bool contractUseInstalled = InstallContractUseCompatibility();
    LogLine("contract-use compatibility result=%d", contractUseInstalled ? 1 : 0);
    return 0;
}
#endif

bool LoadOptionalLuaPlugin()
{
    // Was DNF_LUA_PLUGIN=0 to opt out; see clientConfig at the top of this file.
    if (!kOptionalLuaPluginEnabled) {
        LogLine("optional Lua plugin disabled by compile-time configuration");
        return false;
    }

    HMODULE selfModule = nullptr;
    if (!GetModuleHandleExW(
            GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
                GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
            reinterpret_cast<LPCWSTR>(&LoadOptionalLuaPlugin), &selfModule)) {
        LogLine("optional Lua plugin could not resolve 90CN module error=%u",
            GetLastError());
        return false;
    }

    wchar_t pluginPath[32768] = {};
    DWORD pathLength = GetModuleFileNameW(
        selfModule, pluginPath, static_cast<DWORD>(_countof(pluginPath)));
    if (pathLength == 0 || pathLength >= _countof(pluginPath)) {
        LogLine("optional Lua plugin could not resolve module path error=%u",
            GetLastError());
        return false;
    }
    wchar_t* separator = wcsrchr(pluginPath, L'\\');
    if (!separator) {
        LogLine("optional Lua plugin module path has no directory separator");
        return false;
    }
    separator[1] = L'\0';
    if (wcscat_s(pluginPath, L"90CNLua.dll") != 0) {
        LogLine("optional Lua plugin path is too long");
        return false;
    }

    DWORD attributes = GetFileAttributesW(pluginPath);
    if (attributes == INVALID_FILE_ATTRIBUTES ||
        (attributes & FILE_ATTRIBUTE_DIRECTORY) != 0) {
        LogLine("optional Lua plugin is enabled but sibling 90CNLua.dll is missing");
        return false;
    }

    HMODULE plugin = LoadLibraryW(pluginPath);
    if (!plugin) {
        LogLine("optional Lua plugin LoadLibraryW failed error=%u",
            GetLastError());
        return false;
    }
    using InstallLuaPlugin = BOOL(WINAPI*)();
    auto install = reinterpret_cast<InstallLuaPlugin>(
        GetProcAddress(plugin, "Install90CNLua"));
    if (!install || !install()) {
        LogLine("optional Lua plugin install export failed error=%u",
            GetLastError());
        return false;
    }
    LogLine("optional Lua plugin loaded module=%p", plugin);
    return true;
}

// Production boundary: connection bootstrap, packet codec, packet transport,
// bounded protocol routing, one validated native party-directory UI/refresh
// patch,
// and narrowly allow-listed compatibility for dungeon pickup, creature rename,
// premium-contract op44 dispatch, and server-owned Crystal/Aura state projection.
// ---------------------------------------------------------------------------
// Trade window counterpart column reveal.
//
// The current EXE's trade window renders the counterpart column from the trade
// manager's own second item list (get(1)), which the server already fills with
// class-0/op15 rows from the moment the counterpart places each item.  The six
// trade-UI gate checks, however, only draw that column while
// byte [tradeManager+0x54] is nonzero, and an exhaustive scan of this EXE
// proves that no code ever writes that byte: the only writer helper sub_2340E50
// (`mov byte [ecx+0x54],1`) has exactly one call site, 0x01D415FC, and that
// site targets a sub-object of dword_51B2764 whose vtable is 0x456B36C - never
// the trade manager (vtable 0x45B7ECC).  Both identities were confirmed against
// live process memory during an active trade.  The helper address also appears
// nowhere in .rdata/.data, so no indirect or virtual call can reach it either.
// The counterpart column therefore stays blank for every trade even though the
// items are already present in the list.
//
// The native intent is recoverable from the manager's own per-side state.  The
// class-0/op17 handler sub_1D59D00 computes side = (wire charId != local
// charId) and calls sub_2340B10(this, side, state); the server projects
// "adding complete" onto state 1.  So state[1] == 1 means exactly "the
// counterpart has clicked adding complete", which per the reference flow is the
// moment the counterpart's column must become visible.  Every other trade state
// - including the reset that ends a trade, where state[1] returns to 0 - keeps
// the stock byte and the stock behavior, so no explicit set/clear bookkeeping
// is needed.  The seven call sites of sub_2340E70 whose receiver is not the
// trade manager are untouched.
//
// Each hooked site loads dword_51B2794 into ecx immediately before the stock
// call, so the proxy needs no absolute-address comparison.  The stock accessor
// writes only al; this proxy preserves exactly that register contract.
//
// The six sites are patched with a five-byte `call` - never a `jmp`.  This
// proxy replaces the whole callee and ends in `ret`, so the return address must
// be pushed by the patched instruction itself.  The other entry patches in this
// file use `jmp` because their proxies replay the stolen prologue bytes and
// then jump to a resume address inside the original function; a `jmp` here
// would leave no return address for `ret` to pop and would return into the
// caller of the surrounding trade-UI function, faulting at an invalid address.
// ---------------------------------------------------------------------------
__declspec(naked) void ProxyTradeCounterpartColumnGate()
{
    __asm {
        cmp dword ptr [ecx + 0Ch], 1
        jne stock_gate_byte
        mov al, 1
        ret
    stock_gate_byte:
        mov al, byte ptr [ecx + 54h]
        ret
    }
}

bool InstallTradeCounterpartColumnReveal()
{
    struct GateSite {
        uintptr_t rva;
        unsigned char expected[5];
        const char* label;
    };
    // All six sites are `call sub_2340E70` immediately preceded by
    // `mov ecx, dword ptr [0x51B2794]`.
    static const GateSite kSites[] = {
        { 0x02C3B791, { 0xE8, 0xDA, 0x56, 0x30, 0xFF },
            "trade counterpart column gate 1" },
        { 0x02C3B8C4, { 0xE8, 0xA7, 0x55, 0x30, 0xFF },
            "trade counterpart column gate 2" },
        { 0x02C3F4F9, { 0xE8, 0x72, 0x19, 0x30, 0xFF },
            "trade counterpart column gate 3" },
        { 0x02C3FCF9, { 0xE8, 0x72, 0x11, 0x30, 0xFF },
            "trade counterpart column gate 4" },
        { 0x02C3FE0F, { 0xE8, 0x5C, 0x10, 0x30, 0xFF },
            "trade counterpart column gate 5" },
        { 0x02C3FEFB, { 0xE8, 0x70, 0x0F, 0x30, 0xFF },
            "trade counterpart column gate 6" },
    };

    // The packed EXE restores .text late, so wait for the verified calls.
    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        bool ready = true;
        for (const GateSite& site : kSites) {
            if (!BytesMatch(
                    reinterpret_cast<unsigned char*>(g_dnfBase + site.rva),
                    site.expected, sizeof(site.expected))) {
                ready = false;
                break;
            }
        }
        if (ready) break;
        Sleep(250);
        waitedMs += 250;
    }

    for (const GateSite& site : kSites) {
        uintptr_t target = g_dnfBase + site.rva;

        // Expected bytes after this installer has already run: the same five
        // byte call, retargeted at the proxy.  Both the stock instruction and
        // the patched one begin with 0xE8, so only the full five bytes can tell
        // them apart.
        unsigned char patched[5] = { 0xE8 };
        intptr_t patchedRelative =
            reinterpret_cast<intptr_t>(&ProxyTradeCounterpartColumnGate) -
            static_cast<intptr_t>(target + 5);
        *reinterpret_cast<int32_t*>(patched + 1) =
            static_cast<int32_t>(patchedRelative);

        if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
                site.expected, sizeof(site.expected))) {
            if (BytesMatch(reinterpret_cast<unsigned char*>(target),
                    patched, sizeof(patched))) {
                LogLine("%s already hooked; fresh DNF process required for this build",
                    site.label);
                return false;
            }
            LogLine("%s unexpected bytes at 0x%08X; reveal not installed",
                site.label, target);
            return false;
        }

        if (!WriteCodePatch(target, patched, sizeof(patched), site.label)) {
            return false;
        }
    }

    LogLine("trade counterpart column reveal installed sites=%d rule=peer_state_1",
        static_cast<int>(sizeof(kSites) / sizeof(kSites[0])));
    return true;
}

// ---------------------------------------------------------------------------
// Trade-window close notification.
//
// Closing the trade window is a purely local act in this client: it emits no
// packet at all.  Byte-level evidence from a live two-client session with items
// placed on both sides: the last trade packet is the offer ack, and after the
// window is closed there is no cancel, no state update and not even an op19
// withdrawal of the placed items.
//
// The client does own an "unlock" sender, sub_23418C0(flag), which would write
// class-0/op24 with state 0 or 1 - but its only call site in the whole binary is
// 0x0303B816, the "adding complete" button, which passes 1.  The state-0 path is
// unreachable, which is why the server has never received a client state 0.
//
// Without that signal the server cannot cancel: the op17 state upload is the
// only trade input it accepts besides item moves, and its state-0 case
// deliberately did not end the session.  The observed result is that the peer's
// window never closes, both players stay in the trade state (so the NPC shop
// refuses to sell), and items already moved into the trade container never
// return to the bag until a relog.
//
// This hook fires on the trade window's own teardown path - the single site that
// clears the counterpart-column gate ([tradeManager+0x54] = 0 via sub_2340E60).
// At that point ecx already holds the trade manager, loaded by the preceding
// instruction, so the proxy needs no absolute comparison.  It replays the
// replaced call unchanged and, ahead of it, emits the client's own native
// trade-state packet carrying state 0:
//
//     WriteU16(0x18); WriteU8(0); Flush()
//
// byte-for-byte the sequence sub_23418C0 and sub_2340E20 already use, so no wire
// format is invented here.  Both writers clean their single stack argument with
// `ret 4` and the flush takes none, so the call sequence is stack-neutral.  The
// emit runs under pushad/popad because the stock instruction only touches
// memory while the surrounding teardown continues with live registers.
//
// A player who never opened a trade still closes a window; that emit carries
// state 0 with no trade on the server, where it is ignored.  The server cancels
// only when a trade actually exists for that character.
// ---------------------------------------------------------------------------
// 交易窗口初始化钩子。
//
// 进入时 ecx = 交易窗口对象；该函数在 0x0303B3E9 处把它存进 esi，此后**全程
// 不再修改 esi**（已逐条核对），因此挂载点处 esi 就是窗口对象指针。 本代理
// 只做一件事：把窗口对象记到全局，供 100ms 定时器轮询可见性。
//
// 这里**不发任何包**：早期版本误把这条调用当成"关窗事件"，但服务端收包时间线
// 证明它只在交易建立后 1~11 毫秒出现一次（即开窗初始化），关窗时从不触发。
__declspec(naked) void ProxyTradeWindowInitCapture()
{
    __asm {
        pushad
        mov  dword ptr [g_tradeWindowObject], esi
        popad
        call dword ptr [g_tradeWindowGateClear]
        ret
    }
}

// 上报「本侧关闭了交易窗口」= class-0/op24 + 状态 0。
//
// 复用客户端自己的写包链，与 sub_23418C0 / sub_2340E20 是**完全同一序列**：
//   WriteU16(0x18); WriteU8(0); Flush()
// 两个写入器各自用 `ret 4` 清理唯一的栈参数，flush 无参数，故整段栈平衡。
__declspec(naked) void SendOnlineItemTradeWindowClosedState()
{
    __asm {
        push 18h
        call dword ptr [g_tradePacketFieldGetter]
        mov  ecx, eax
        call dword ptr [g_tradePacketWriteU16]
        push 0
        call dword ptr [g_tradePacketFieldGetter]
        mov  ecx, eax
        call dword ptr [g_tradePacketWriteU8]
        call dword ptr [g_tradePacketFlush]
        ret
    }
}

// 交易窗口对象里框架的显示标志偏移。
// 活体实测：开窗时 0→1，关窗时 1→0（X 按钮与 ESC 两条路径都是），整局只有
// 这两次变化；该字段不是窗口初始化函数写的，属于 UI 框架的可见性状态。
constexpr uintptr_t kTradeWindowVisibleOffset = 0x14;

// 关窗检测，由主窗口消息钩子上的 100ms 定时器周期调用。
// 运行在 UI 线程，与客户端自己的按钮处理同线程，因此发包是安全的。
//
// 判定状态机：只看交易窗口的显示标志，做成"显示 → 隐藏"的边沿检测。
//   标志 != 0（窗口显示中）→ 记下"显示过"，允许下一次上报
//   标志 == 0 且"显示过"  → 上报一次 state 0，并清掉"显示过"
//
// 刻意**不要求交易管理器仍绑定对方**：实测 ESC 关窗这条路径会把对方 id 一并
// 清成 0xFFFF，若拿它当前置条件，ESC 关窗就永远不会上报（X 按钮路径不清，
// 所以只有 ESC 失效）。 也因此"显示过"这个闩锁是必需的：客户端启动时、以及
// 交易刚建立而窗口尚未显示的那一瞬间，标志同样是 0，没有它就会误报成关窗。
//
// 上报后服务端若已无交易（例如正常交易完成后窗口关闭）会直接忽略，因此这里
// 不需要再区分"窗口是因何而关"。
void CheckTradeWindowClosed()
{
    // 商城网页改向的延后重试（导入表在加壳客户端里可能晚于 DllMain 才就绪）。
    TryDeferredShopWebRedirect();
    if (!g_tradeWindowGateClear || !g_tradePacketFlush || g_tradeWindowObject == 0) {
        return;
    }
    __try {
        unsigned int visible = *reinterpret_cast<unsigned int*>(
            g_tradeWindowObject + kTradeWindowVisibleOffset);
        if (visible != 0) {
            InterlockedExchange(&g_tradeWindowCloseSeenVisible, 1);
            InterlockedExchange(&g_tradeWindowCloseReported, 0);
            return;
        }
        if (InterlockedCompareExchange(&g_tradeWindowCloseSeenVisible, 1, 1) == 0) {
            return;
        }
        if (InterlockedCompareExchange(&g_tradeWindowCloseReported, 1, 0) != 0) {
            return;
        }
        InterlockedExchange(&g_tradeWindowCloseSeenVisible, 0);
        SendOnlineItemTradeWindowClosedState();
        LogLine("trade window hidden: op24 state 0 reported");
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        g_tradeWindowObject = 0;
        InterlockedExchange(&g_tradeWindowCloseSeenVisible, 0);
        InterlockedExchange(&g_tradeWindowCloseReported, 0);
    }
}

bool InstallTradeWindowCloseNotify()
{
    // `call sub_2340E60` on the trade window teardown path; ecx is loaded with
    // dword_51B2794 by the immediately preceding instruction.
    constexpr uintptr_t kCloseCallRva = 0x02C3B584;
    static const unsigned char kCloseCall[] = { 0xE8, 0xD7, 0x58, 0x30, 0xFF };

    constexpr uintptr_t kFieldGetterRva = 0x03054640;
    constexpr uintptr_t kWriteU16Rva = 0x03054EB0;
    constexpr uintptr_t kWriteU8Rva = 0x03054FC0;
    constexpr uintptr_t kFlushRva = 0x030566F0;
    constexpr uintptr_t kGateClearRva = 0x01F40E60;

    // The packed EXE restores .text late, so wait for the verified call.
    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(g_dnfBase + kCloseCallRva),
                kCloseCall, sizeof(kCloseCall))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }

    uintptr_t target = g_dnfBase + kCloseCallRva;

    // Expected bytes after this installer has already run: the same five-byte
    // call, retargeted at the proxy.  Stock and patched both begin with 0xE8,
    // so only the full five bytes can tell them apart.
    unsigned char patched[5] = { 0xE8 };
    intptr_t patchedRelative =
        reinterpret_cast<intptr_t>(&ProxyTradeWindowInitCapture) -
        static_cast<intptr_t>(target + 5);
    *reinterpret_cast<int32_t*>(patched + 1) =
        static_cast<int32_t>(patchedRelative);

    if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
            kCloseCall, sizeof(kCloseCall))) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(target),
                patched, sizeof(patched))) {
            LogLine("trade window close notify already hooked; fresh DNF process required");
            return false;
        }
        LogLine("trade window close notify unexpected bytes at 0x%08X; not installed",
            target);
        return false;
    }

    g_tradePacketFieldGetter = g_dnfBase + kFieldGetterRva;
    g_tradePacketWriteU16 = g_dnfBase + kWriteU16Rva;
    g_tradePacketWriteU8 = g_dnfBase + kWriteU8Rva;
    g_tradePacketFlush = g_dnfBase + kFlushRva;
    g_tradeWindowGateClear = g_dnfBase + kGateClearRva;

    // A five-byte `call` into the proxy - never a `jmp`.  The proxy ends in
    // `ret`, and only a `call` pushes the return address that `ret` pops.
    if (!WriteCodePatch(target, patched, sizeof(patched), "trade window close notify")) {
        return false;
    }

    LogLine("trade window close detection installed init_site=0x%08X "
        "visible_offset=0x%X poll_ms=100 state_report=0",
        target, static_cast<unsigned int>(kTradeWindowVisibleOffset));
    return true;
}

// ---------------------------------------------------------------------------
// 附魔师摆摊: the enchant result window renders an item other than the one that
// was actually enchanted.
//
// Which item the window draws is decided entirely by the client global
// 0x518B798, not by the op601 reply. The op601 handler sub_1D0E470 reads ten
// bytes (u8 success / u32 experience / u8 reserved / u32 endurance) and then:
//
//     mov edx, [0x518B798]        ; target key
//     mov ecx, [0x51B2768]        ; item manager
//     call sub_2326860            ; resolve by key
//     push 0x3D9                  ; UI owner 985 = result window
//     call sub_2FDD1A0            ; kSceneUiOpen(window, resolved, success)
//
// The body therefore supplies the outcome and the endurance only - it cannot
// say *which* item. In the whole binary 0x518B798 has exactly one writer,
// 0x01A84EFD (`mov eax,[ebx+8]; mov [0x518B798],eax` inside sub_1A84D20, where
// ebx is the UI manager 0x51B2764 plus 0xB7CC, the global drag state), and
// sub_1A84D20 is reached from 0x01A8E6DA alone - event 30 of the same UI class's
// dispatcher, that is, "item dropped into the NPC enchant machine".
//
// The stall path takes a different branch: from 0x01A83230 it resolves the item
// through [esi+0x74] for the stall window (sub_1B0F600) and hands
// [esi+0x60]/[esi+0x74]/[esi+0x70] to the stall window's recorder sub_1B0F680,
// which stores them at [window+0x238/+0x23C/+0x240]; sub_1B0F6B0 then sends
// op601 from those three fields. Nothing on that path writes 0x518B798.
//
// So when a stall enchant finishes, the result window resolves the stale key
// left behind by the previous NPC-machine enchant (or zero) and shows another
// item. The server enchants the correct one - the slot carried in op601 - but
// the two sides take "which item" from unrelated sources and only one of them
// is stall-aware. That is also why re-sending the target slot's op14 from the
// server measured as no effect: it never takes part in item selection.
//
// Fix: write the same value into 0x518B798 at the moment the stall window
// records the slot. The hook site is `call sub_1B0F680` at 0x01A83263, where
// the three arguments are already pushed and arg2 ([ebp+0xC], i.e. [esi+0x74])
// is exactly the value later stored at [window+0x23C] and sent as the op601
// target slot - the same value 0x01A83235 resolves the displayed item with. The
// proxy writes one extra global and tail-jumps to the original function;
// sub_1B0F680 is thiscall with `ret 0xC`, and pushad/popad preserve ecx (this)
// and every argument, so the stack stays neutral.
//
// Five-byte `call`, not `jmp`: the proxy relies on the return address the
// original `call` pushed, so that sub_1B0F680's `ret 0xC` returns to the
// original call site. The NPC-machine path does not pass through this call
// site, so its behaviour is unchanged.
//
// The server half of this fix is not here: the enchant ack must also be sent
// after the container refresh, or the window resolves a row that is still
// pre-enchant. See expert_job_store.go `expert_job_store_enchant_before_ack`.
// ---------------------------------------------------------------------------
uintptr_t g_enchantStoreResultWindowTarget = 0;  // &0x518B798
uintptr_t g_enchantStoreSlotRecorder = 0;        // sub_1B0F680

__declspec(naked) void ProxyEnchantStoreRecordResultWindowTarget()
{
    __asm {
        pushad
        // pushad pushed 0x20 bytes; skipping the return address (4) and arg1
        // (4) lands on arg2.
        mov  eax, dword ptr [esp + 28h]
        mov  ecx, dword ptr [g_enchantStoreResultWindowTarget]
        mov  dword ptr [ecx], eax
        popad
        jmp  dword ptr [g_enchantStoreSlotRecorder]
    }
}

bool InstallEnchantStoreResultWindowTarget()
{
    // `call sub_1B0F680` - the stall enchant window's slot recorder, the only
    // call site of that function in the whole binary.
    constexpr uintptr_t kRecordCallRva = 0x01683263;
    static const unsigned char kRecordCall[] = { 0xE8, 0x18, 0xC4, 0x08, 0x00 };
    constexpr uintptr_t kSlotRecorderRva = 0x0170F680;        // sub_1B0F680
    constexpr uintptr_t kResultWindowTargetRva = 0x04D8B798;  // 0x518B798

    // The packed EXE restores .text late, so wait for the verified call.
    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(g_dnfBase + kRecordCallRva),
                kRecordCall, sizeof(kRecordCall))) {
            break;
        }
        Sleep(250);
        waitedMs += 250;
    }

    uintptr_t target = g_dnfBase + kRecordCallRva;

    unsigned char patched[5] = { 0xE8 };
    intptr_t patchedRelative =
        reinterpret_cast<intptr_t>(&ProxyEnchantStoreRecordResultWindowTarget) -
        static_cast<intptr_t>(target + 5);
    *reinterpret_cast<int32_t*>(patched + 1) =
        static_cast<int32_t>(patchedRelative);

    if (!BytesMatch(reinterpret_cast<unsigned char*>(target),
            kRecordCall, sizeof(kRecordCall))) {
        if (BytesMatch(reinterpret_cast<unsigned char*>(target),
                patched, sizeof(patched))) {
            LogLine("enchant store result-window target already hooked; fresh DNF process required");
            return false;
        }
        LogLine("enchant store result-window target unexpected bytes at 0x%08X; not installed",
            target);
        return false;
    }

    g_enchantStoreResultWindowTarget = g_dnfBase + kResultWindowTargetRva;
    g_enchantStoreSlotRecorder = g_dnfBase + kSlotRecorderRva;

    if (!WriteCodePatch(target, patched, sizeof(patched),
            "enchant store result-window target")) {
        return false;
    }

    LogLine("enchant store result-window target installed site=0x%08X global=0x%08X recorder=0x%08X",
        target, g_enchantStoreResultWindowTarget, g_enchantStoreSlotRecorder);
    return true;
}

DWORD WINAPI InstallTransportWorkerInner(void*)
{
    g_dnfBase = reinterpret_cast<uintptr_t>(GetModuleHandleW(nullptr));
    g_luaGameControlEntrypointsValid = ValidateLuaGameControlEntrypoints();
    if (!g_exceptionTraceHandle) {
        g_exceptionTraceHandle =
            AddVectoredExceptionHandler(1, TraceUnhandledClientException);
    }
    // Installed after the tracer so it runs first: a skippable null call is a
    // recovered fault, not a crash, and would otherwise spam the crash lines.
    if (!g_nullPointerCallGuardHandle) {
        g_nullPointerCallGuardHandle =
            AddVectoredExceptionHandler(1, GuardNullPointerCall);
    }
    LogLine("90CN transport worker started exception_handler=%p null_call_guard=%p module_base=0x%08X",
        g_exceptionTraceHandle, g_nullPointerCallGuardHandle, g_dnfBase);
    LogLine("lua game-control native entrypoints valid=%d",
        g_luaGameControlEntrypointsValid ? 1 : 0);

    bool luaPluginLoaded = LoadOptionalLuaPlugin();
    LogLine("optional Lua plugin result=%d", luaPluginLoaded ? 1 : 0);

    // Was DNF_PROTOCOL_TRACE=1; see clientConfig at the top of this file. The log
    // line below still reports the value that took effect.
    InterlockedExchange(
        &g_protocolTraceEnabled,
        kProtocolTraceEnabled ? 1 : 0);
    LogLine("protocol transport trace enabled=%d",
        InterlockedCompareExchange(&g_protocolTraceEnabled, 0, 0) != 0 ? 1 : 0);

    g_checksumFn = g_dnfBase + kChecksumRva;
    g_gameAllocatorFn = g_dnfBase + kGameAllocatorRva;
    g_gameMemcpyFn = g_dnfBase + kGameMemcpyRva;
    g_dprotoOutgoingResume = g_dnfBase + kDprotoOutgoingResumeRva;
    g_dprotoOutgoingReturn = g_dnfBase + kDprotoOutgoingReturnRva;
    g_dprotoFalseResume = g_dnfBase + kDprotoFalseResumeRva;

    // The launcher opts in with DNF_MULTI_CLIENT=1. Keep this limited to the
    // two audited current-EXE single-instance checks; normal launches are no-op.
    bool multiClientInstalled = InstallMultiClientCompatibilityHook();
    InstallRemoteGameAuthenticationHook();
    LogLine("multi-client startup compatibility result=%d",
        multiClientInstalled ? 1 : 0);

    // 首次选角资源加载的空 this 守卫。必须在第一次选角之前装好，因此放在启动
    // 安装流的最前面：sub_13D6BE0 在 ecx==0 时会于 +0x2E 处读 [ecx+0x1840]
    // 触发 0xC0000005（客户端启动后第一次进入角色必现）。
    bool selectLoadNullThisGuardInstalled = InstallSelectLoadNullThisGuard();
    LogLine("select-load null-this guard result=%d",
        selectLoadNullThisGuardInstalled ? 1 : 0);
    bool actorListContainsGuardInstalled = InstallActorListContainsGuard();
    LogLine("actor-list contains null guard result=%d",
        actorListContainsGuardInstalled ? 1 : 0);

    bool tclsInstalled = InstallTclsCompatibility();
    LogLine("TCLS transport bootstrap result=%d", tclsInstalled ? 1 : 0);

    // The channel hooks only observe directory loading and adapt the bootstrap
    // connection port. They do not write HUD, clock, scene, or actor state.
    bool channelInstalled = InstallChannelDiagnostic();
    LogLine("channel transport compatibility result=%d",
        channelInstalled ? 1 : 0);

    bool customRegionInstalled = InstallCustomRegionNameCompatibility();
    LogLine("custom friend/guild region name compatibility result=%d",
        customRegionInstalled ? 1 : 0);

    // 公会 (guild) compatibility, in the upstream install order: the create-region
    // unlock first, then the member-UI bindings, then the two packet consumers
    // (ranking frame, war rows), then the shared assault-result actor reset.
    bool guildRegionInstalled = InstallGuildCreateRegionCompatibility();
    LogLine("guild create region compatibility result=%d", guildRegionInstalled ? 1 : 0);

    bool guildMemberMenuInstalled = InstallGuildMemberMenuCompatibility();
    LogLine("guild member menu compatibility result=%d", guildMemberMenuInstalled ? 1 : 0);

    LogLine("guild member actor binding compatibility result=%d",
        InstallGuildMemberActorCompatibility() ? 1 : 0);
    LogLine("guild experience ranking compatibility result=%d",
        InstallGuildRankingCompatibility() ? 1 : 0);
    LogLine("guild war empty rows compatibility result=%d",
        InstallGuildWarRowsCompatibility() ? 1 : 0);

    bool assaultResultInstalled = InstallAssaultResultCompatibility();
    LogLine("assault result compatibility result=%d", assaultResultInstalled ? 1 : 0);

    bool userStateUiGuardInstalled = InstallUserStateNullUiGuard();
    LogLine("user-state null UI guard result=%d",
        userStateUiGuardInstalled ? 1 : 0);
    bool dungeonEventMonsterConditionGuardInstalled =
        InstallDungeonEventMonsterConditionGuard();
    LogLine("dungeon event-monster condition null guard result=%d",
        dungeonEventMonsterConditionGuardInstalled ? 1 : 0);

    bool townEntrySceneAccessorGuardInstalled =
        InstallTownEntrySceneAccessorNullGuard();
    LogLine("town-entry scene accessor null guard result=%d",
        townEntrySceneAccessorGuardInstalled ? 1 : 0);






    bool shopWebRedirectInstalled = InstallShopWebRedirect();
    LogLine("shop web redirect result=%d", shopWebRedirectInstalled ? 1 : 0);

    bool ceraShopVipInstalled = InstallCeraShopVipButtonCompatibility();
    LogLine("cera shop VIP button compatibility result=%d",
        ceraShopVipInstalled ? 1 : 0);

    bool weddingTreeTimeInstalled = InstallWeddingTreeTimeCompatibility();
    LogLine("wedding tree remaining time compatibility result=%d",
        weddingTreeTimeInstalled ? 1 : 0);

    bool partyDirectoryUiInstalled =
        InstallPartyDirectoryFullPageCompatibility();
    LogLine("party-directory full-page UI compatibility result=%d",
        partyDirectoryUiInstalled ? 1 : 0);
    bool skillTreeProfessionVisibilityInstalled =
        InstallSkillTreeProfessionVisibilityCompatibility();
    LogLine("skill-tree profession visibility compatibility result=%d",
        skillTreeProfessionVisibilityInstalled ? 1 : 0);
    bool partyTeleportPromptCloseInstalled =
        InstallPartyTeleportPromptCloseCompatibility();
    LogLine("party teleport consent prompt close compatibility result=%d",
        partyTeleportPromptCloseInstalled ? 1 : 0);
    bool berserkerFrenzyRenderInstalled =
        InstallBerserkerFrenzyRenderCompatibility();
    LogLine("berserker Blood Frenzy red render compatibility result=%d",
        berserkerFrenzyRenderInstalled ? 1 : 0);
    // Temporarily retain the audited implementation in source but do not
    // install it while the client uses the original six-column HUD NPK. This
    // prevents slot-204/205 icons, key labels and right-HUD relocation from
    // rendering outside the restored frame without rolling back unrelated
    // compatibility repairs from the current DLL.
    bool skillQuickSlot7Installed = false;
    LogLine("skill quick-slot 7 compatibility result=0 disabled=original-six-column-hud");
    bool equipmentDisjointBatchInstalled =
        InstallEquipmentDisjointBatchCompatibility();
    LogLine("equipment-disjoint batch compatibility result=%d",
        equipmentDisjointBatchInstalled ? 1 : 0);
    bool expertJobStoreDisjointBatchInstalled =
        InstallExpertJobStoreDisjointBatchCompatibility();
    LogLine("expert-job-store disjoint batch compatibility result=%d",
        expertJobStoreDisjointBatchInstalled ? 1 : 0);
    bool expertJobExtractionBatchInstalled =
        InstallExpertJobExtractionBatchCompatibility();
    LogLine("expert-job extraction batch compatibility result=%d",
        expertJobExtractionBatchInstalled ? 1 : 0);
    bool antonRaidModeSelectUiInstalled =
        InstallAntonRaidModeSelectCompatibility();
    LogLine("Anton raid native mode-select UI compatibility result=%d",
        antonRaidModeSelectUiInstalled ? 1 : 0);
    bool raidDirectoryOwnerGuardInstalled =
        InstallRaidDirectoryOwnerNullGuard();
    LogLine("Anton raid public-directory owner null guard result=%d",
        raidDirectoryOwnerGuardInstalled ? 1 : 0);
    bool raidPanelNullCallGuardInstalled =
        InstallRaidPanelNullCallGuard();
    LogLine("raid panel null vtable-slot call guard result=%d",
        raidPanelNullCallGuardInstalled ? 1 : 0);
    bool raidInstanceFlagGuardInstalled =
        InstallRaidInstanceFlagNullGuard();
    LogLine("raid instance optional flag null guard result=%d",
        raidInstanceFlagGuardInstalled ? 1 : 0);
    bool townCopresenceInstalled =
        InstallTownCopresenceCompatibility();
    LogLine("town co-presence actor-context compatibility result=%d",
        townCopresenceInstalled ? 1 : 0);
    bool localActorBindingNullGuardInstalled =
        InstallLocalActorBindingNullGuard();
    LogLine("local actor optional binding null guard result=%d",
        localActorBindingNullGuardInstalled ? 1 : 0);
    bool dungeonPickupInstalled = InstallDungeonPickupCompatibility();
    LogLine("dungeon pickup compatibility result=%d",
        dungeonPickupInstalled ? 1 : 0);
    bool dungeonStartMapStateInstalled =
        InstallDungeonStartMapStateCompatibility();
    LogLine("dungeon start-map state compatibility result=%d",
        dungeonStartMapStateInstalled ? 1 : 0);
    bool creatureRenameInstalled = InstallCreatureRenameCompatibility();
    LogLine("creature rename compatibility result=%d",
        creatureRenameInstalled ? 1 : 0);
    bool townCreatureStatMapNullGuardInstalled =
        InstallTownCreatureStatMapNullGuard();
    LogLine("town creature stat-map null guard result=%d",
        townCreatureStatMapNullGuardInstalled ? 1 : 0);
    bool chatNameCacheNullGuardInstalled =
        InstallChatNameCacheNullGuard();
    LogLine("chat name-cache null guard result=%d",
        chatNameCacheNullGuardInstalled ? 1 : 0);
    bool petEnchantDisplayInstalled =
        InstallPetEnchantDisplayCompatibility();
    LogLine("pet enchant display compatibility result=%d",
        petEnchantDisplayInstalled ? 1 : 0);
    bool creatureTransportStoneInstalled =
        InstallCreatureTransportStoneCompatibility();
    LogLine("pet transport stone compatibility result=%d",
        creatureTransportStoneInstalled ? 1 : 0);
    bool op14AcquireNotificationInstalled =
        InstallOp14AcquireNotificationCompatibility();
    LogLine("op14 acquisition notification compatibility result=%d",
        op14AcquireNotificationInstalled ? 1 : 0);
    bool naenRaidShopDateInstalled =
        InstallRaidShopDateCompatibility();
    LogLine("Naen raid-shop retired date compatibility result=%d",
        naenRaidShopDateInstalled ? 1 : 0);

    // Same three constants as the worker above; see clientConfig at the top of
    // this file. Nothing outside the binary can change them any more.
    if (!kCipherPassthroughEnabled) {
        LogLine("cipher passthrough disabled (compile-time configuration); "
            "packet codec hook disabled");
        return 0;
    }

    constexpr bool enableDprotoCompat = kDprotoCompatEnabled;
    constexpr bool enableSceneRouteCompat = kSceneRouteCompatEnabled;

    uintptr_t cipherTarget = g_dnfBase + kCipherEncodeRva;
    uintptr_t cipherDecodeTarget = g_dnfBase + kCipherDecodeRva;
    uintptr_t routePredicateTarget = g_dnfBase + kDprotoRoutePredicateRva;
    uintptr_t outgoingGateTarget = g_dnfBase + kDprotoOutgoingGateRva;
    uintptr_t outgoingCloneTarget = g_dnfBase + kDprotoOutgoingCloneRva;

    static const unsigned char kNativePrologue[] = {
        0x55, 0x8B, 0xEC, 0x53, 0x8B, 0x5D
    };
    static const unsigned char kRoutePredicatePrologue[] = {
        0x55, 0x8B, 0xEC, 0x8B, 0x49, 0x0C
    };
    static const unsigned char kOutgoingGate[] = {
        0x0F, 0x84, 0x2F, 0x01, 0x00, 0x00
    };
    static const unsigned char kOutgoingClonePrologue[] = {
        0x8B, 0x4B, 0x0C, 0x8B, 0x01
    };

    constexpr int kWaitLimitMs = 120000;
    int waitedMs = 0;
    while (waitedMs < kWaitLimitMs) {
        bool cipherReady = BytesMatch(
            reinterpret_cast<unsigned char*>(cipherTarget),
            kNativePrologue, sizeof(kNativePrologue));
        bool cipherDecodeReady = BytesMatch(
            reinterpret_cast<unsigned char*>(cipherDecodeTarget),
            kNativePrologue, sizeof(kNativePrologue));
        bool routeReady = !enableSceneRouteCompat ||
            BytesMatch(reinterpret_cast<unsigned char*>(routePredicateTarget),
                kRoutePredicatePrologue, sizeof(kRoutePredicatePrologue));
        bool dprotoReady = !enableDprotoCompat ||
            (BytesMatch(reinterpret_cast<unsigned char*>(routePredicateTarget),
                 kRoutePredicatePrologue, sizeof(kRoutePredicatePrologue)) &&
             BytesMatch(reinterpret_cast<unsigned char*>(outgoingGateTarget),
                 kOutgoingGate, sizeof(kOutgoingGate)) &&
             BytesMatch(reinterpret_cast<unsigned char*>(outgoingCloneTarget),
                 kOutgoingClonePrologue, sizeof(kOutgoingClonePrologue)));
        if (cipherReady && cipherDecodeReady && routeReady && dprotoReady) {
            break;
        }

        unsigned char firstByte = 0;
        if (TryReadByte(reinterpret_cast<unsigned char*>(cipherTarget),
                &firstByte) && firstByte == 0xE9) {
            LogLine("cipher encode already hooked; fresh DNF process required");
            return 0;
        }
        if (TryReadByte(reinterpret_cast<unsigned char*>(cipherDecodeTarget),
                &firstByte) && firstByte == 0xE9) {
            LogLine("cipher decode already hooked; fresh DNF process required");
            return 0;
        }
        Sleep(250);
        waitedMs += 250;
    }
    if (waitedMs >= kWaitLimitMs) {
        LogLine("transport targets unavailable route=%d dproto=%d",
            enableSceneRouteCompat ? 1 : 0,
            enableDprotoCompat ? 1 : 0);
        return 0;
    }

    unsigned char encodePatch[5] = { 0xE9 };
    intptr_t relative =
        reinterpret_cast<intptr_t>(&ProxyCipherEncodeFunction) -
        static_cast<intptr_t>(cipherTarget + 5);
    *reinterpret_cast<int32_t*>(encodePatch + 1) =
        static_cast<int32_t>(relative);
    if (!WriteCodePatch(cipherTarget, encodePatch, sizeof(encodePatch),
            "upper body encode transport")) {
        return 0;
    }

    unsigned char decodePatch[5] = { 0xE9 };
    relative = reinterpret_cast<intptr_t>(&ProxyCipherDecodeFunction) -
        static_cast<intptr_t>(cipherDecodeTarget + 5);
    *reinterpret_cast<int32_t*>(decodePatch + 1) =
        static_cast<int32_t>(relative);
    if (!WriteCodePatch(cipherDecodeTarget, decodePatch, sizeof(decodePatch),
            "upper body decode transport")) {
        return 0;
    }

    if (enableSceneRouteCompat) {
        unsigned char routePatch[6] = { 0xE9 };
        relative = reinterpret_cast<intptr_t>(&ProxySceneRouteAllowList) -
            static_cast<intptr_t>(routePredicateTarget + 5);
        *reinterpret_cast<int32_t*>(routePatch + 1) =
            static_cast<int32_t>(relative);
        routePatch[5] = 0x90;
        if (!WriteCodePatch(routePredicateTarget, routePatch,
                sizeof(routePatch), "inbound protocol route")) {
            return 0;
        }
    }

    if (enableDprotoCompat) {
        unsigned char clonePatch[5] = { 0xE9 };
        relative = reinterpret_cast<intptr_t>(&ProxyDprotoSendDirect) -
            static_cast<intptr_t>(outgoingCloneTarget + 5);
        *reinterpret_cast<int32_t*>(clonePatch + 1) =
            static_cast<int32_t>(relative);

        unsigned char falsePatch[6] = { 0x0F, 0x84 };
        relative = reinterpret_cast<intptr_t>(&ProxyDprotoFalseSelective) -
            static_cast<intptr_t>(outgoingGateTarget + 6);
        *reinterpret_cast<int32_t*>(falsePatch + 2) =
            static_cast<int32_t>(relative);

        if (!WriteCodePatch(outgoingCloneTarget, clonePatch,
                sizeof(clonePatch), "outbound DPROTO direct transport") ||
            !WriteCodePatch(outgoingGateTarget, falsePatch,
                sizeof(falsePatch), "outbound DPROTO selective transport")) {
            LogLine("DPROTO transport patch incomplete; fresh DNF process required");
            return 0;
        }
    }

    bool premiumStateInstalled = InstallPremiumStateCompatibility();
    LogLine("premium-state compatibility result=%d",
        premiumStateInstalled ? 1 : 0);
    bool newbieGuideSeenInstalled = InstallNewbieGuideSeenSuppression();
    LogLine("newbie-guide seen suppression result=%d",
        newbieGuideSeenInstalled ? 1 : 0);
    bool contractUseInstalled = InstallContractUseCompatibility();
    LogLine("contract-use compatibility result=%d",
        contractUseInstalled ? 1 : 0);

    // Last, on purpose: this installer waits for its own byte signatures, and
    // placing it at the end of the sequence guarantees that a mismatch can
    // never delay any other hook in this function.
    bool tradeCounterpartColumnInstalled =
        InstallTradeCounterpartColumnReveal();
    LogLine("trade counterpart column reveal result=%d",
        tradeCounterpartColumnInstalled ? 1 : 0);
    bool tradeWindowCloseNotifyInstalled =
        InstallTradeWindowCloseNotify();
    LogLine("trade window close notify result=%d",
        tradeWindowCloseNotifyInstalled ? 1 : 0);

    // Also waits for its own byte signature at the end of the sequence, for the
    // same reason.
    bool enchantStoreResultWindowInstalled =
        InstallEnchantStoreResultWindowTarget();
    LogLine("enchant store result-window target result=%d",
        enchantStoreResultWindowInstalled ? 1 : 0);

    bool channelFollowTypeGateInstalled =
        InstallChannelFollowTypeGateCompatibility();
    LogLine("channel-follow type gate result=%d",
        channelFollowTypeGateInstalled ? 1 : 0);

    LogLine("transport DLL installed codec=1 decode=1 route=%d dproto=%d party_ui=%d berserker_frenzy_red=%d equipment_disjoint_batch=%d expert_job_store_disjoint_batch=%d expert_job_extraction_batch=%d raid_directory_owner_guard=%d raid_panel_null_call_guard=%d raid_instance_flag_guard=%d town_copresence=%d local_actor_binding_guard=%d expert_job_native_ui=%d dungeon_pickup=%d creature_rename=%d naen_raid_shop_date=%d premium_state=%d contract_use=%d wait_ms=%d",
        enableSceneRouteCompat ? 1 : 0,
        enableDprotoCompat ? 1 : 0,
        partyDirectoryUiInstalled ? 1 : 0,
        berserkerFrenzyRenderInstalled ? 1 : 0,
        equipmentDisjointBatchInstalled ? 1 : 0,
        expertJobStoreDisjointBatchInstalled ? 1 : 0,
        expertJobExtractionBatchInstalled ? 1 : 0,
        raidDirectoryOwnerGuardInstalled ? 1 : 0,
        raidPanelNullCallGuardInstalled ? 1 : 0,
        raidInstanceFlagGuardInstalled ? 1 : 0,
        townCopresenceInstalled ? 1 : 0,
        localActorBindingNullGuardInstalled ? 1 : 0,
        dungeonPickupInstalled ? 1 : 0,
        creatureRenameInstalled ? 1 : 0,
        naenRaidShopDateInstalled ? 1 : 0,
        premiumStateInstalled ? 1 : 0,
        contractUseInstalled ? 1 : 0,
        waitedMs);
    return 0;
}

// The Anton raid diagnostic probe (raid_probe.cpp). It used to be RaidProbe.dll,
// injected after this module; InstallWorker calls this once every 90CN hook is in
// place, which is what its SceneUiOpen tail-jump chains onto.
extern "C" void InstallRaidProbe();

DWORD WINAPI InstallWorker(void*)
{
    DWORD result = 0;
    __try {
        result = InstallTransportWorkerInner(nullptr);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("worker exception code=0x%08X", GetExceptionCode());
    }
    // The Anton raid probe installs LAST, and from this thread on purpose.
    //
    // It used to be a separate RaidProbe.dll that the operator injected after
    // 90CN.dll, so its SceneUiOpen observer could chain onto the E9 90CN had
    // already written there (see InstallTailJumpHook in raid_probe.cpp). Merged
    // into this DLL the same ordering has to be explicit: everything 90CN
    // installs -- including the SceneUiOpen hook -- has happened by the time
    // InstallTransportWorkerInner returns, and the probe refuses to install
    // without that chain target, so a failure above degrades the probe exactly
    // as it would have degraded the injected module. It runs even when the
    // inner install bailed out early, because the probe's own hooks do not
    // depend on the transport patches.
    InstallRaidProbe();
    return result;
}
} // namespace

#pragma comment(linker, "/EXPORT:Queue90CNClientNotice=_Queue90CNClientNotice@8")
#pragma comment(linker, "/EXPORT:Dequeue90CNClientEvent=_Dequeue90CNClientEvent@8")
#pragma comment(linker, "/EXPORT:Query90CNCharacterStatSnapshot=_Query90CNCharacterStatSnapshot@8")
#pragma comment(linker, "/EXPORT:Query90CNEquipmentSnapshot=_Query90CNEquipmentSnapshot@8")
#pragma comment(linker, "/EXPORT:Query90CNDamageAffixSnapshot=_Query90CNDamageAffixSnapshot@8")
#pragma comment(linker, "/EXPORT:Update90CNCombatPanel=_Update90CNCombatPanel@8")
#pragma comment(linker, "/EXPORT:Update90CNDebugPanel=_Update90CNDebugPanel@8")
#pragma comment(linker, "/EXPORT:Queue90CNGameCommand=_Queue90CNGameCommand@8")
#pragma comment(linker, "/EXPORT:Query90CNActorSnapshot=_Query90CNActorSnapshot@8")
#pragma comment(linker, "/EXPORT:Dequeue90CNBossMutationDecision=_Dequeue90CNBossMutationDecision@8")
#pragma comment(linker, "/EXPORT:Log90CNLuaDiagnostic=_Log90CNLuaDiagnostic@8")
// The merged Anton raid probe's own entry point (raid_probe.cpp), exported for
// one reason: a host that is not DNF.exe can then run the probe's dry-run
// self-test directly --
//
//     LoadLibraryW(L"90CN.dll");
//     GetProcAddress(module, "Install90CNRaidProbe")();
//
// -- without Start90CNPatch, which waits up to two minutes for the game's
// signatures before it gives up. probe\selftest_raidprobe.ps1 is that host.
#pragma comment(linker, "/EXPORT:Install90CNRaidProbe=_Install90CNRaidProbe")
EXTERN_C void Install90CNRaidProbe()
{
    InstallRaidProbe();
}

EXTERN_C BOOL WINAPI Queue90CNClientNotice(
    const wchar_t* text, unsigned int length)
{
    return QueueLuaClientNoticeInternal(text, length) ? TRUE : FALSE;
}

EXTERN_C BOOL WINAPI Dequeue90CNClientEvent(
    DNF90ClientEvent* output, unsigned int outputSize)
{
    if (!output || outputSize < DNF90_CLIENT_EVENT_V1_SIZE) return FALSE;

    DNF90ClientEvent event = {};
    if (!DequeueLuaClientEventInternal(&event)) return FALSE;
    const unsigned int copySize = outputSize >= sizeof(event)
        ? static_cast<unsigned int>(sizeof(event))
        : DNF90_CLIENT_EVENT_V1_SIZE;
    event.size = copySize;
    __try {
        memcpy(output, &event, copySize);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("lua client event output write exception code=0x%08X",
            GetExceptionCode());
        return FALSE;
    }
    return TRUE;
}

EXTERN_C BOOL WINAPI Queue90CNGameCommand(
    const DNF90GameCommand* command, unsigned int commandSize)
{
    return QueueLuaGameCommandInternal(command, commandSize) ? TRUE : FALSE;
}

EXTERN_C BOOL WINAPI Query90CNActorSnapshot(
    DNF90ActorSnapshot* output, unsigned int outputSize)
{
    if (!output || outputSize < sizeof(DNF90ActorSnapshot)) return FALSE;

    DNF90ActorSnapshot snapshot = {};
    AcquireSRWLockShared(&g_actorSnapshotLock);
    snapshot = g_actorSnapshot;
    ReleaseSRWLockShared(&g_actorSnapshotLock);
    if (snapshot.size != sizeof(snapshot) || snapshot.generation == 0 ||
        snapshot.actorCount > DNF90_ACTOR_SNAPSHOT_MAX_ACTORS) {
        return FALSE;
    }

    __try {
        *output = snapshot;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("lua actor snapshot output write exception code=0x%08X",
            GetExceptionCode());
        return FALSE;
    }
    return TRUE;
}

EXTERN_C BOOL WINAPI Dequeue90CNBossMutationDecision(
    DNF90BossMutationDecision* output, unsigned int outputSize)
{
    if (!output || outputSize < sizeof(DNF90BossMutationDecision)) {
        return FALSE;
    }
    DNF90BossMutationDecision decision = {};
    AcquireSRWLockExclusive(&g_bossMutationDecisionLock);
    if (g_bossMutationDecisionCount == 0) {
        ReleaseSRWLockExclusive(&g_bossMutationDecisionLock);
        return FALSE;
    }
    decision = g_bossMutationDecisionQueue[g_bossMutationDecisionHead];
    g_bossMutationDecisionQueue[g_bossMutationDecisionHead] = {};
    g_bossMutationDecisionHead = (g_bossMutationDecisionHead + 1) %
        kBossMutationDecisionQueueCapacity;
    --g_bossMutationDecisionCount;
    ReleaseSRWLockExclusive(&g_bossMutationDecisionLock);
    __try {
        *output = decision;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("boss-mutation decision output write exception code=0x%08X",
            GetExceptionCode());
        return FALSE;
    }
    return TRUE;
}

EXTERN_C BOOL WINAPI Log90CNLuaDiagnostic(
    const char* text, unsigned int length)
{
    if (!text || length == 0 || length > 1023) return FALSE;

    char message[1024] = {};
    __try {
        memcpy(message, text, length);
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("90CNLua diagnostic input read exception code=0x%08X",
            GetExceptionCode());
        return FALSE;
    }
    for (unsigned int index = 0; index < length; ++index) {
        if (message[index] == '\r' || message[index] == '\n') {
            message[index] = ' ';
        }
    }
    LogLine("90CNLua %s", message);
    return TRUE;
}

EXTERN_C BOOL WINAPI Query90CNCharacterStatSnapshot(
    DNF90CharacterStatSnapshot* output, unsigned int outputSize)
{
    if (!kCombatPowerEnabled) {
        return FALSE;
    }
    if (!output || outputSize < sizeof(DNF90CharacterStatSnapshot)) {
        return FALSE;
    }

    DNF90CharacterStatSnapshot snapshot = {};
    AcquireSRWLockShared(&g_characterStatSnapshotLock);
    snapshot = g_characterStatSnapshot;
    ReleaseSRWLockShared(&g_characterStatSnapshotLock);
    if (snapshot.size != sizeof(snapshot) || snapshot.generation == 0 ||
        (snapshot.validFlags & DNF90_CHARACTER_STATS_BASE_VALID) == 0) {
        return FALSE;
    }

    __try {
        *output = snapshot;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("combat-power snapshot output write exception code=0x%08X",
            GetExceptionCode());
        return FALSE;
    }
    return TRUE;
}

EXTERN_C BOOL WINAPI Query90CNEquipmentSnapshot(
    DNF90EquipmentSnapshot* output, unsigned int outputSize)
{
    if (!kCombatPowerEnabled) {
        return FALSE;
    }
    if (!output || outputSize < sizeof(DNF90EquipmentSnapshot)) {
        return FALSE;
    }

    DNF90EquipmentSnapshot snapshot = {};
    AcquireSRWLockShared(&g_equipmentSnapshotLock);
    snapshot = g_equipmentSnapshot;
    ReleaseSRWLockShared(&g_equipmentSnapshotLock);
    if (snapshot.size != sizeof(snapshot) || snapshot.generation == 0 ||
        snapshot.itemCount > DNF90_EQUIPMENT_SNAPSHOT_MAX_ITEMS ||
        (snapshot.validFlags & DNF90_EQUIPMENT_SNAPSHOT_ROWS_VALID) == 0) {
        return FALSE;
    }

    __try {
        *output = snapshot;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("combat-power equipment snapshot output write exception "
            "code=0x%08X", GetExceptionCode());
        return FALSE;
    }
    return TRUE;
}

EXTERN_C BOOL WINAPI Query90CNDamageAffixSnapshot(
    DNF90DamageAffixSnapshot* output, unsigned int outputSize)
{
    if (!kCombatPowerEnabled) {
        return FALSE;
    }
    if (!output || outputSize < sizeof(DNF90DamageAffixSnapshot)) {
        return FALSE;
    }

    DNF90DamageAffixSnapshot snapshot = {};
    AcquireSRWLockShared(&g_damageAffixSnapshotLock);
    snapshot = g_damageAffixSnapshot;
    ReleaseSRWLockShared(&g_damageAffixSnapshotLock);
    if (snapshot.size != sizeof(snapshot) || snapshot.generation == 0 ||
        snapshot.version != kCombatPowerAffixPrivateVersion ||
        snapshot.equippedItemCount > DNF90_EQUIPMENT_SNAPSHOT_MAX_ITEMS ||
        (snapshot.validFlags &
            (DNF90_DAMAGE_AFFIX_SNAPSHOT_VALUES_VALID |
             DNF90_DAMAGE_AFFIX_SNAPSHOT_IDENTITY_VALID |
             DNF90_DAMAGE_AFFIX_SNAPSHOT_THREE_ATTACKS_VALID |
             DNF90_DAMAGE_AFFIX_SNAPSHOT_EQUIPMENT_SCORE_VALID)) !=
            (DNF90_DAMAGE_AFFIX_SNAPSHOT_VALUES_VALID |
             DNF90_DAMAGE_AFFIX_SNAPSHOT_IDENTITY_VALID |
             DNF90_DAMAGE_AFFIX_SNAPSHOT_THREE_ATTACKS_VALID |
             DNF90_DAMAGE_AFFIX_SNAPSHOT_EQUIPMENT_SCORE_VALID)) {
        return FALSE;
    }

    __try {
        *output = snapshot;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("combat-power damage-affix snapshot output write exception code=0x%08X",
            GetExceptionCode());
        return FALSE;
    }
    return TRUE;
}

EXTERN_C BOOL WINAPI Update90CNCombatPanel(
    const DNF90CombatPanelState* state, unsigned int stateSize)
{
    if (!kCombatPowerEnabled) {
        return FALSE;
    }
    if (!state || stateSize != sizeof(DNF90CombatPanelState)) return FALSE;

    DNF90CombatPanelState pending = {};
    __try {
        pending = *state;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("combat-panel state input read exception code=0x%08X",
            GetExceptionCode());
        return FALSE;
    }
    constexpr unsigned int kKnownFlags =
        DNF90_COMBAT_PANEL_ENABLED |
        DNF90_COMBAT_PANEL_BASE_SCORE_VALID |
        DNF90_COMBAT_PANEL_EQUIPMENT_SCORE_VALID |
        DNF90_COMBAT_PANEL_DAMAGE_AFFIXES_VALID |
        DNF90_COMBAT_PANEL_IDENTITY_VALID |
        DNF90_COMBAT_PANEL_THREE_ATTACKS_VALID;
    pending.professionUtf8[_countof(pending.professionUtf8) - 1] = '\0';
    const bool identityValid =
        (pending.validFlags & DNF90_COMBAT_PANEL_IDENTITY_VALID) != 0;
    if (pending.size != sizeof(pending) ||
        (pending.validFlags & ~kKnownFlags) != 0 ||
        pending.formulaVersion == 0 ||
        pending.totalScore > 2000000000u ||
        pending.baseAttributeScore > 2000000000u ||
        pending.equipmentScore > 2000000000u ||
        pending.whiteDamageTenths > 65535u ||
        pending.yellowDamageTenths > 65535u ||
        pending.criticalDamageTenths > 65535u ||
        pending.yellowAdditionalTenths > 65535u ||
        pending.criticalAdditionalTenths > 65535u ||
        pending.allAttackTenths > 65535u ||
        pending.equippedItemCount > DNF90_EQUIPMENT_SNAPSHOT_MAX_ITEMS ||
        (identityValid && (pending.level == 0 ||
            pending.professionUtf8[0] == '\0' ||
            MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS,
                pending.professionUtf8, -1, nullptr, 0) <= 0))) {
        return FALSE;
    }

    AcquireSRWLockExclusive(&g_combatPanelStateLock);
    g_combatPanelState = pending;
    ReleaseSRWLockExclusive(&g_combatPanelStateLock);

    HWND bridgeWindow = reinterpret_cast<HWND>(
        InterlockedCompareExchangePointer(
            &g_luaNoticeBridgeWindow, nullptr, nullptr));
    UINT bridgeMessage = static_cast<UINT>(
        InterlockedCompareExchange(&g_combatPanelWindowMessage, 0, 0));
    if (!bridgeWindow || !bridgeMessage ||
        !PostMessageW(bridgeWindow, bridgeMessage, 0, 0)) {
        return FALSE;
    }
    return TRUE;
}

EXTERN_C BOOL WINAPI Update90CNDebugPanel(
    const DNF90DebugPanelState* state, unsigned int stateSize)
{
    if (!kDebugPanelEnabled) {
        return FALSE;
    }
    if (!state || stateSize != sizeof(DNF90DebugPanelState)) return FALSE;

    DNF90DebugPanelState pending = {};
    __try {
        pending = *state;
    }
    __except (EXCEPTION_EXECUTE_HANDLER) {
        LogLine("debug-panel state input read exception code=0x%08X",
            GetExceptionCode());
        return FALSE;
    }

    constexpr unsigned int kKnownFlags =
        DNF90_DEBUG_PANEL_ENABLED |
        DNF90_DEBUG_PANEL_CHARACTER_VALID |
        DNF90_DEBUG_PANEL_DUNGEON_VALID |
        DNF90_DEBUG_PANEL_COMBAT_SCORE_VALID |
        DNF90_DEBUG_PANEL_EVENT_VALID;
    const bool characterValid =
        (pending.validFlags & DNF90_DEBUG_PANEL_CHARACTER_VALID) != 0;
    const bool dungeonValid =
        (pending.validFlags & DNF90_DEBUG_PANEL_DUNGEON_VALID) != 0;
    const bool combatValid =
        (pending.validFlags & DNF90_DEBUG_PANEL_COMBAT_SCORE_VALID) != 0;
    const bool eventValid =
        (pending.validFlags & DNF90_DEBUG_PANEL_EVENT_VALID) != 0;
    if (pending.size != sizeof(pending) ||
        (pending.validFlags & ~kKnownFlags) != 0 ||
        pending.latestEventType > 5 || pending.context > 2 ||
        pending.dungeonID > 100000000u || pending.mapID > 100000000u ||
        pending.roomX > 100000u || pending.roomY > 100000u ||
        pending.roomLayerFlag > 1000u || pending.roomCount > 100000u ||
        pending.hpMax > 2000000000u || pending.mpMax > 2000000000u ||
        pending.strength < -2000000 || pending.strength > 2000000 ||
        pending.intelligence < -2000000 || pending.intelligence > 2000000 ||
        pending.moveSpeed > 10000000u || pending.attackSpeed > 10000000u ||
        pending.castSpeed > 10000000u ||
        pending.equippedItemCount > DNF90_EQUIPMENT_SNAPSHOT_MAX_ITEMS ||
        pending.combatScore > 2000000000u ||
        (eventValid && (pending.latestEventSequence == 0 ||
            pending.latestEventType == 0)) ||
        (dungeonValid && (pending.context != 2 ||
            pending.dungeonID == 0)) ||
        (characterValid && (pending.hpMax == 0 || pending.mpMax == 0)) ||
        (!combatValid && pending.combatScore != 0)) {
        return FALSE;
    }

    AcquireSRWLockExclusive(&g_debugPanelStateLock);
    g_debugPanelState = pending;
    ReleaseSRWLockExclusive(&g_debugPanelStateLock);

    HWND bridgeWindow = reinterpret_cast<HWND>(
        InterlockedCompareExchangePointer(
            &g_luaNoticeBridgeWindow, nullptr, nullptr));
    UINT bridgeMessage = static_cast<UINT>(
        InterlockedCompareExchange(&g_debugPanelWindowMessage, 0, 0));
    if (!bridgeWindow || !bridgeMessage ||
        !PostMessageW(bridgeWindow, bridgeMessage, 0, 0)) {
        return FALSE;
    }
    return TRUE;
}

void Start90CNPatch()
{
    // Do not chain-load the previous client hook.  This DLL owns only the
    // common codec and bounded current-scene route predicate boundary. It does
    // not hook send/recv or replay packet bodies.
    HANDLE worker = CreateThread(nullptr, 0, InstallWorker, nullptr, 0, nullptr);
    if (worker) CloseHandle(worker);
    else LogLine("CreateThread failed: %u", GetLastError());
}
