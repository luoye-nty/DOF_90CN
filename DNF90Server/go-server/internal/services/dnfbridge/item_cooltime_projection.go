package dnfbridge

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"

	dnfitemcooldown "longheng.io/server/internal/modules/dnf/itemcooldown"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// currentLoadCooltimeItemInfoMsgID 是当前客户端的**道具冷却列表**通知（类 0）。
//
// 86 参考服务端把同一形状的两条通知成对注册（_86ref
// Network/Builders/Init/ItemStateListBodyBuilder.cs：0x00AC 装 CooltimeItemStates、
// 0x00AE 装 EffectItemStates，body 都是 `[count u8]` + 每项 `[itemID u32][时间 u32]`），
// 而本树已经在用的 174（= 0x00AE）正是那个 builder 的另一半（见
// experience_consumable_projection.go，现场校准过）。所以 0xAC(172) 就是冷却列表这一条。
// 时间字段沿用本树 174 的口径：**剩余秒数**，不是绝对到期时间——如果实机发现客户端把
// 它当绝对时间，画面会显示成"没有冷却"，把这里换成 ExpireAt 即可（一处改动）。
const currentLoadCooltimeItemInfoMsgID uint16 = 172

// sendCurrentItemCooltimeProjection 在角色容器可见之后重放仍在冷却中的道具。
//
// 客户端的冷却门是进程内内存：用过之后它自己会画冷却圈，但**小退重上就没了**，于是 1 天
// 冷却的消耗品重上又能用（现场 2026-09-17 运营反馈）。服务端已经把到期时间落进角色
// stats（inventory.Owner.ReserveItemCooltime），这里把它翻译成客户端能吃的列表。
//
// 没有冷却时不发包：登录时客户端本来就是空的，而回城那一发如果发空列表反而会把客户端
// 自己刚起算的本地冷却抹掉。
func (s *Service) sendCurrentItemCooltimeProjection(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 || session.itemCooltimeProjectionSent {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		return err
	}
	now := s.gameplayNow().UTC()
	nowUnix := now.Unix()
	entries := dnfitemcooldown.ActiveEntries(character.Stats, nowUnix)
	// 过期的行顺手清掉：冷却表只增不减的话，每次重上都会多背几条垃圾 stat。
	if dnfitemcooldown.Prune(character.Stats, nowUnix) {
		character.UpdatedAt = now
		if err := dnfrepo.SaveCharacterFields(ctx, repositories.Character, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
	}
	body := buildCurrentItemCooltimeBody(entries, nowUnix)
	if len(body) == 0 {
		return nil
	}
	if err := s.sendGameUpperRawClass(session, currentLoadCooltimeItemInfoMsgID, body, 0); err != nil {
		return err
	}
	session.itemCooltimeProjectionSent = true
	s.logPacketEvent("game-item-cooltime-restore-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"entry_count", int(body[0]),
		"body_len", len(body),
		"sequence", "class0/op172-load-cooltime-item-info")
	return nil
}

// logAlignedItemCooltimeStates records the durable window a command just started.
//
// 这一发**不发包**：客户端消耗道具时自己按同一份 PVF 起本地冷却，重上之后的恢复走
// 通知 172。这里只把窗口写进游戏日志，作为"服务端确实记住了这次冷却"的证据（运营现场
// 的验收就是看日志有没有这条，然后小退重上确认仍然被拦）。
func (s *Service) logAlignedItemCooltimeStates(session *gameSession, result alignedcmd.Result) {
	if s == nil || session == nil || len(result.ItemCooltimeStates) == 0 {
		return
	}
	nowUnix := s.gameplayNow().UTC().Unix()
	for _, state := range result.ItemCooltimeStates {
		s.logGameEvent(session, "game-item-cooltime-started",
			"operation", result.Operation,
			"item_id", state.ItemID,
			"expire_at", state.ExpireAt,
			"remaining_seconds", state.ExpireAt-nowUnix,
			"sequence", "durable_window_recorded_client_starts_its_own_local_cooldown")
	}
}

// buildCurrentItemCooltimeBody encodes `[count u8]` + one `[itemID u32][remaining u32]`
// per still-cooling item. Expired entries are dropped before the count is written
// so the count byte always matches the rows that follow.
func buildCurrentItemCooltimeBody(entries []dnfitemcooldown.Entry, nowUnix int64) []byte {
	if len(entries) == 0 {
		return nil
	}
	rows := make([]dnfitemcooldown.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.ItemID <= 0 || entry.ItemID > math.MaxUint32 || entry.ExpireAt <= nowUnix {
			continue
		}
		rows = append(rows, entry)
		if len(rows) == 0xFF {
			break
		}
	}
	if len(rows) == 0 {
		return nil
	}
	body := make([]byte, 0, 1+8*len(rows))
	body = append(body, byte(len(rows)))
	for _, entry := range rows {
		remaining := entry.ExpireAt - nowUnix
		if remaining > math.MaxUint32 {
			remaining = math.MaxUint32
		}
		body = binary.LittleEndian.AppendUint32(body, uint32(entry.ItemID))
		body = binary.LittleEndian.AppendUint32(body, uint32(remaining))
	}
	return body
}
