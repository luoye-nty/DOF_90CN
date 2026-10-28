package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/inventory"
	"longheng.io/server/internal/modules/dnf/itemtrade"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentItemTradeListType   byte   = 4
	currentItemTradeChangeMsg  uint16 = 15
	currentItemTradeCancelMsg  uint16 = 16
	currentItemTradeStateMsg   uint16 = 17
	currentItemTradeFinishMsg  uint16 = 18
	currentItemTradeMoveMsg    uint16 = 19
	currentItemTradeReadyMsg   uint16 = uint16(dnfenum.CmdPacketSetItemtradeState)
	currentItemTradeAddingDone byte   = 1
	currentItemTradeOfferAck   byte   = 2
	currentItemTradeReady      byte   = 3
	currentItemTradeAddRequest byte   = 5
	// currentItemTradeWindowClosed 是「本侧关闭了交易窗口」的唯一可用信号。
	//
	// 当前客户端关窗时不发任何原生包:它唯一的解锁发送器 sub_23418C0(flag)
	// (发 op24 + state 0/1) 在本客户端里只被「添加完成」按钮以 flag=1 调用,
	// state 0 那条路径不可达。因此 state 0 在协议上是空闲取值,由 90CN.dll 在
	// 交易窗口拆卸路径补发,用于表达"我的交易窗口关了"。
	currentItemTradeWindowClosed byte   = 0
	currentItemTradeOfferFirst   uint16 = 3
	currentItemTradeOfferLast    uint16 = 11
)

type onlineItemTradeOffer struct {
	tradeSlot  uint16
	sourceList byte
	sourceSlot int16
	count      int32
	itemID     int64
	stack      dnfrepo.ItemStack
}

type onlineItemTrade struct {
	participants [2]uint16
	offers       map[uint16]map[uint16]onlineItemTradeOffer
	gold         map[uint16]int64
	states       map[uint16]byte
	committing   bool
	// createdAt 用于区分「客户端开窗复位」与「用户真的关窗」两种 state 0。
	// 客户端在打开交易窗口时会复位窗口,该复位路径会补发 state 0;此时交易
	// 刚建立且完全为空,因此这类 state 0 必须在时间窗内被忽略。
	createdAt time.Time
}

// currentItemTradeOpenResetGrace 是「开窗复位误报」的时间窗。
//
// 90CN.dll 只在真关窗时才补发 state 0,但它判定关窗的依据是"窗口显示标志为 0
// 且交易已绑定对方";交易刚建立、窗口尚未显示的那一瞬间也满足这个组合,故仍留
// 一个很短的窗口作为防御。 实测误报只出现在建立后 ≤100ms,而真实关窗最快也要
// 人看到窗口之后(实测 671~1644ms),因此 500ms 既能挡住误报又不会吞掉真关窗。
// 早期版本用 2 秒,会吞掉"开窗后立刻关窗"这类真实取消。
const currentItemTradeOpenResetGrace = 500 * time.Millisecond

// onlineItemTradeHasContent 判定这笔交易是否已经承载任何玩家意图。
//
// 空交易 = 双方都没有报价、没有金币、state 都为 0。 只有「刚创建 + 完全为空」
// 的 state 0 才是开窗复位;一旦有任何内容,state 0 都按真实取消处理。
func onlineItemTradeHasContent(trade *onlineItemTrade) bool {
	if trade == nil {
		return false
	}
	for _, characterID := range trade.participants {
		if trade.states[characterID] != 0 || trade.gold[characterID] != 0 ||
			len(trade.offers[characterID]) > 0 {
			return true
		}
	}
	return false
}

func (t *onlineItemTrade) other(characterID uint16) (uint16, bool) {
	if t == nil {
		return 0, false
	}
	if t.participants[0] == characterID {
		return t.participants[1], true
	}
	if t.participants[1] == characterID {
		return t.participants[0], true
	}
	return 0, false
}

func (s *Service) beginOnlineItemTrade(first, second *gameSession) {
	if s == nil || first == nil || second == nil || first == second || first.selectedCharacterID == 0 || second.selectedCharacterID == 0 {
		return
	}
	firstID, secondID := first.selectedCharacterID, second.selectedCharacterID
	if s.onlinePlayers == nil || !s.onlinePlayers.PeerInSameArea(firstID, secondID) {
		return
	}
	s.tradeMu.Lock()
	defer s.tradeMu.Unlock()
	if s.itemTrades == nil {
		s.itemTrades = make(map[uint16]*onlineItemTrade)
	}
	for _, id := range []uint16{firstID, secondID} {
		if old := s.itemTrades[id]; old != nil {
			delete(s.itemTrades, old.participants[0])
			delete(s.itemTrades, old.participants[1])
		}
	}
	trade := &onlineItemTrade{
		participants: [2]uint16{firstID, secondID},
		offers: map[uint16]map[uint16]onlineItemTradeOffer{
			firstID:  make(map[uint16]onlineItemTradeOffer),
			secondID: make(map[uint16]onlineItemTradeOffer),
		},
		gold:      map[uint16]int64{firstID: 0, secondID: 0},
		states:    map[uint16]byte{firstID: 0, secondID: 0},
		createdAt: time.Now(),
	}
	s.itemTrades[firstID] = trade
	s.itemTrades[secondID] = trade
}

func (s *Service) handleOnlineItemTradeCommand(session *gameSession, typ uint16, body []byte) (bool, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return false, nil
	}
	trade := s.currentOnlineItemTrade(session.selectedCharacterID)
	if trade == nil {
		return false, nil
	}
	switch typ {
	case currentItemTradeMoveMsg:
		request, err := inventory.DecodeMoveItemspaceRequest(body)
		if err != nil {
			return true, s.sendGameUpperFailure(session, typ, 1)
		}
		if request.SourceListType != currentItemTradeListType && request.DestinationListType != currentItemTradeListType {
			return false, nil
		}
		return true, s.handleOnlineItemTradeMove(session, request)
	case currentItemTradeReadyMsg:
		if len(body) != 1 {
			return true, s.sendGameUpperFailure(session, typ, 1)
		}
		return true, s.handleOnlineItemTradeState(session, body[0])
	default:
		return false, nil
	}
}

func (s *Service) currentOnlineItemTrade(characterID uint16) *onlineItemTrade {
	s.tradeMu.Lock()
	defer s.tradeMu.Unlock()
	return s.itemTrades[characterID]
}

func (s *Service) handleOnlineItemTradeMove(session *gameSession, request inventory.MoveItemspaceRequest) error {
	if request.DestinationListType == currentItemTradeListType && request.SourceListType != currentItemTradeListType {
		if isOnlineItemTradeGoldDeposit(request) {
			return s.addOnlineItemTradeGold(session, request)
		}
		return s.addOnlineItemTradeOffer(session, request)
	}
	if request.SourceListType == currentItemTradeListType && request.DestinationListType != currentItemTradeListType {
		if isOnlineItemTradeGoldWithdrawal(request) {
			return s.removeOnlineItemTradeGold(session, request)
		}
		return s.removeOnlineItemTradeOffer(session, request)
	}
	return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
}

// onlineItemTradeOffersEditable 判定「本侧是否仍可修改报价」（放入/取回物品、存入/取回金币）。
//
// 语义与时序依据（配合 handleOnlineItemTradeState 的状态投影表）：
//   - states[char] == 0：本侧尚未提交「添加完成」，报价可自由增删；
//   - states[char] == currentItemTradeAddingDone(1)：客户端已上报 5 并被投影为「添加完成」，
//     该侧界面进入锁定态（此后只剩「最终确认 / 取消」两个动作）。此时若仍允许改报价，
//     会出现「本侧已锁定却继续增删报价 → 服务端权威报价与本侧界面、对方界面三者不一致」，
//     故锁定后必须禁止修改；
//   - states[char] == 2（报价确认）/ 3（最终确认）以及 trade.committing：均处于锁定之后，
//     同样不可改；
//   - 客户端可重新上报 state 0 解锁（见 handleOnlineItemTradeState 的 case 0），解锁后方可再改。
//
// 物品报价与金币报价共用本判定，保证两侧锁定语义完全一致。
func onlineItemTradeOffersEditable(trade *onlineItemTrade, characterID uint16) bool {
	if trade == nil || trade.committing {
		return false
	}
	return trade.states[characterID] == 0
}

func isOnlineItemTradeGoldDeposit(request inventory.MoveItemspaceRequest) bool {
	return request.SourceListType == dnfrepo.MainInventoryListType &&
		request.SourceSlotIndex == 0 && request.SourceInstanceValue == 0 &&
		request.MoveCount > 0 && request.DestinationListType == currentItemTradeListType &&
		request.DestinationSlotIndex == 0
}

func isOnlineItemTradeGoldWithdrawal(request inventory.MoveItemspaceRequest) bool {
	return request.SourceListType == currentItemTradeListType && request.SourceSlotIndex == 0 &&
		request.DestinationListType == dnfrepo.MainInventoryListType && request.DestinationSlotIndex == 0
}

func (s *Service) addOnlineItemTradeGold(session *gameSession, request inventory.MoveItemspaceRequest) error {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	character, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found || int64(request.MoveCount) > character.Stats["gold"] {
		return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
	}

	s.tradeMu.Lock()
	trade := s.itemTrades[session.selectedCharacterID]
	peerID, inTrade := trade.other(session.selectedCharacterID)
	// 金币报价同受「添加完成即锁定」约束：只有本侧仍处于可编辑态（state 0）才允许存入。
	valid := inTrade && onlineItemTradeOffersEditable(trade, session.selectedCharacterID)
	previousGold := int64(0)
	if valid {
		previousGold = trade.gold[session.selectedCharacterID]
		trade.gold[session.selectedCharacterID] = int64(request.MoveCount)
	}
	s.tradeMu.Unlock()
	peer, peerOnline := s.onlineGameSession(peerID)
	if !valid || !peerOnline || s.onlinePlayers == nil || !s.onlinePlayers.PeerInSameArea(session.selectedCharacterID, peerID) {
		if valid {
			s.tradeMu.Lock()
			if current := s.itemTrades[session.selectedCharacterID]; current == trade && !current.committing {
				current.gold[session.selectedCharacterID] = previousGold
			}
			s.tradeMu.Unlock()
		}
		return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
	}
	if err := s.sendGameUpperSuccess(session, currentItemTradeMoveMsg, buildOnlineItemTradeMoveAck(request)); err != nil {
		return err
	}
	entry := currentGoldWalletItemListEntry(int64(request.MoveCount))
	if err := s.sendGameUpperRawClassCodec(peer, currentItemTradeChangeMsg, entry.data[:], 0, true); err != nil {
		return err
	}
	s.logGameEvent(session, "game-item-trade-gold-added",
		"char_id", session.selectedCharacterID, "peer_char_id", peerID, "gold", request.MoveCount)
	return nil
}

func (s *Service) removeOnlineItemTradeGold(session *gameSession, request inventory.MoveItemspaceRequest) error {
	s.tradeMu.Lock()
	trade := s.itemTrades[session.selectedCharacterID]
	peerID, inTrade := trade.other(session.selectedCharacterID)
	gold := int64(0)
	// 本侧已锁定（state 1/2/3）或交易进入提交阶段时不得再取回金币，保持金币报价与锁定态一致。
	if inTrade && onlineItemTradeOffersEditable(trade, session.selectedCharacterID) {
		gold = trade.gold[session.selectedCharacterID]
		if gold > 0 {
			trade.gold[session.selectedCharacterID] = 0
		}
	}
	s.tradeMu.Unlock()
	if gold <= 0 {
		return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
	}
	if err := s.sendGameUpperSuccess(session, currentItemTradeMoveMsg, buildOnlineItemTradeMoveAck(request)); err != nil {
		return err
	}
	peer, peerOnline := s.onlineGameSession(peerID)
	if !peerOnline {
		return nil
	}
	return s.sendGameUpperRawClassCodec(peer, currentItemTradeChangeMsg, buildOnlineItemTradeRemovedEntry(0), 0, true)
}

func (s *Service) addOnlineItemTradeOffer(session *gameSession, request inventory.MoveItemspaceRequest) error {
	// The account-owned crystal/soul warehouse reaches the trade window under
	// the client's own container id; the slot number is still the merged list-0
	// cell, so normalize it back before touching the repository.
	if request.SourceListType == currentPrivateStoreAccountSharedListType &&
		dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, request.SourceSlotIndex) {
		request.SourceListType = dnfrepo.MainInventoryListType
	}
	if request.MoveCount <= 0 || request.DestinationSlotIndex < 0 || request.SourceSlotIndex < 0 ||
		(request.SourceListType != dnfrepo.MainInventoryListType && request.SourceListType != 1) {
		return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	record, found, err := s.currentPrivateStoreInventoryView(
		ctx, repositories, strconv.Itoa(int(session.selectedCharacterID)), s.accountIDForSession(session))
	if err != nil || !found {
		return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
	}
	sourceKey := fmt.Sprintf("%d:%d", request.SourceListType, request.SourceSlotIndex)
	stack, found := record.Slots[sourceKey]
	if !found || stack.ItemID <= 0 || stack.Count < int64(request.MoveCount) || stack.Bind ||
		(!stack.ExpireAt.IsZero() && !stack.ExpireAt.After(time.Now())) {
		return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
	}
	// The request's source-instance field is an opaque value read from the
	// current client's live item object.  It is not the PVF item ID and is not
	// stable across every equipment/stackable representation.  The repository
	// slot and authoritative count above are sufficient to prevent item
	// injection, so a projection mismatch must not reject a valid trade.
	projectedSourceValue := currentItemListStackAmount(request.SourceListType, stack)
	if request.SourceInstanceValue != 0 && uint32(request.SourceInstanceValue) != projectedSourceValue {
		s.logGameEvent(session, "game-item-trade-source-instance-projection-mismatch",
			"char_id", session.selectedCharacterID,
			"source_list", request.SourceListType,
			"source_slot", request.SourceSlotIndex,
			"client_value", request.SourceInstanceValue,
			"projected_value", projectedSourceValue)
	}

	s.tradeMu.Lock()
	trade := s.itemTrades[session.selectedCharacterID]
	peerID, inTrade := trade.other(session.selectedCharacterID)
	// 本侧提交「添加完成」后界面进入锁定态，锁定后不得再新增/替换报价（见 onlineItemTradeOffersEditable）。
	valid := inTrade && onlineItemTradeOffersEditable(trade, session.selectedCharacterID)
	tradeSlot := uint16(0)
	slotAvailable := false
	if valid {
		// Current NoPack's trade container has twelve native slots. Live current-
		// EXE memory and sub_2339FB0 show 0..2 are occupied pseudo rows (gold and
		// the other trade currencies); the nine visible item rows are 3..11.
		// Targeting 0 renders a count-one item as +1 gold, while 1/2 collide with
		// the remaining pseudo rows and leave the item list blank.
		for slot := currentItemTradeOfferFirst; slot <= currentItemTradeOfferLast; slot++ {
			if _, occupied := trade.offers[session.selectedCharacterID][slot]; !occupied {
				tradeSlot = slot
				slotAvailable = true
				break
			}
		}
		for _, existing := range trade.offers[session.selectedCharacterID] {
			if existing.sourceList == request.SourceListType && existing.sourceSlot == request.SourceSlotIndex {
				valid = false
				break
			}
		}
	}
	valid = valid && slotAvailable
	offer := onlineItemTradeOffer{
		tradeSlot: tradeSlot, sourceList: request.SourceListType, sourceSlot: request.SourceSlotIndex,
		count: request.MoveCount, itemID: stack.ItemID, stack: cloneOnlineItemTradeStack(stack),
	}
	if valid {
		trade.offers[session.selectedCharacterID][tradeSlot] = offer
	}
	s.tradeMu.Unlock()
	peer, peerOnline := s.onlineGameSession(peerID)
	if !valid || !peerOnline || s.onlinePlayers == nil || !s.onlinePlayers.PeerInSameArea(session.selectedCharacterID, peerID) {
		if valid {
			s.removeOnlineItemTradeOfferState(session.selectedCharacterID, tradeSlot)
		}
		return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
	}

	// The request always targets pseudo-container slot 0 because the client does
	// not allocate the visible row itself. Echo the server-assigned native item
	// row in the success ACK; 0..2 are pseudo rows and item rows are 3..11.
	ackRequest := request
	ackRequest.DestinationSlotIndex = int16(tradeSlot)
	if err := s.sendGameUpperSuccess(session, currentItemTradeMoveMsg, buildOnlineItemTradeMoveAck(ackRequest)); err != nil {
		return err
	}
	offer.stack.Count = int64(offer.count)
	entry := currentItemListEntryFromStack(offer.sourceList, int16(offer.tradeSlot), offer.stack)
	if err := s.sendGameUpperRawClassCodec(peer, currentItemTradeChangeMsg, entry.data[:], 0, true); err != nil {
		return err
	}
	s.logGameEvent(session, "game-item-trade-offer-added",
		"char_id", session.selectedCharacterID, "peer_char_id", peerID,
		"source_list", offer.sourceList, "source_slot", offer.sourceSlot,
		"trade_slot", offer.tradeSlot, "item_id", offer.itemID, "count", offer.count)
	return nil
}

func (s *Service) removeOnlineItemTradeOffer(session *gameSession, request inventory.MoveItemspaceRequest) error {
	tradeSlot := uint16(request.SourceSlotIndex)
	s.tradeMu.Lock()
	trade := s.itemTrades[session.selectedCharacterID]
	peerID, inTrade := trade.other(session.selectedCharacterID)
	var offer onlineItemTradeOffer
	var found bool
	// 锁定后不得取回报价：物品报价在双方「添加完成」时即视为最终报价，此后仅剩最终确认或整单取消。
	if inTrade && onlineItemTradeOffersEditable(trade, session.selectedCharacterID) {
		offer, found = trade.offers[session.selectedCharacterID][tradeSlot]
		if found {
			delete(trade.offers[session.selectedCharacterID], tradeSlot)
		}
	}
	s.tradeMu.Unlock()
	if !found || request.DestinationListType != offer.sourceList {
		return s.sendGameUpperFailure(session, currentItemTradeMoveMsg, 1)
	}
	if err := s.sendGameUpperSuccess(session, currentItemTradeMoveMsg, buildOnlineItemTradeMoveAck(request)); err != nil {
		return err
	}
	peer, peerOnline := s.onlineGameSession(peerID)
	if !peerOnline {
		return nil
	}
	return s.sendGameUpperRawClassCodec(peer, currentItemTradeChangeMsg, buildOnlineItemTradeRemovedEntry(tradeSlot), 0, true)
}

func buildOnlineItemTradeRemovedEntry(tradeSlot uint16) []byte {
	removed := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(removed[0:2], tradeSlot)
	binary.LittleEndian.PutUint32(removed[2:6], ^uint32(0))
	return removed
}

func (s *Service) removeOnlineItemTradeOfferState(characterID, tradeSlot uint16) {
	s.tradeMu.Lock()
	defer s.tradeMu.Unlock()
	if trade := s.itemTrades[characterID]; trade != nil && !trade.committing {
		delete(trade.offers[characterID], tradeSlot)
	}
}

func (s *Service) handleOnlineItemTradeState(session *gameSession, state byte) error {
	if state != 0 && state != currentItemTradeAddingDone && state != currentItemTradeOfferAck && state != currentItemTradeReady && state != currentItemTradeAddRequest {
		return s.sendGameUpperFailure(session, currentItemTradeReadyMsg, 1)
	}
	characterID := session.selectedCharacterID
	s.tradeMu.Lock()
	trade := s.itemTrades[characterID]
	peerID, inTrade := trade.other(characterID)
	if !inTrade || trade.committing {
		s.tradeMu.Unlock()
		return s.sendGameUpperFailure(session, currentItemTradeReadyMsg, 1)
	}
	peerState := trade.states[peerID]
	if state == currentItemTradeOfferAck {
		currentState := trade.states[characterID]
		if currentState == currentItemTradeReady {
			s.tradeMu.Unlock()
			return s.sendGameUpperFailure(session, currentItemTradeReadyMsg, 1)
		}
		s.tradeMu.Unlock()
		s.logGameEvent(session, "game-item-trade-offer-change-acknowledged",
			"char_id", characterID,
			"peer_char_id", peerID,
			"retained_state", currentState,
			"peer_state", peerState)
		return s.sendGameUpperSuccess(session, currentItemTradeReadyMsg, nil)
	}
	projectedState := state
	// Live current-EXE traffic distinguishes the two buttons even though the
	// server-facing state table does not: "adding complete" submits 5 and must
	// be projected to class0/op17 state 1; choosing Yes in resource 727 submits
	// 3 and must be projected as state 3.  A second value 5 is retained only as
	// a compatibility spelling of final consent for older clients.  Neither
	// spelling may skip the shared state-1 confirmation boundary.
	switch state {
	case currentItemTradeAddRequest:
		if trade.states[characterID] == 0 {
			projectedState = currentItemTradeAddingDone
		} else if trade.states[characterID] == currentItemTradeAddingDone &&
			(peerState == currentItemTradeAddingDone || peerState == currentItemTradeReady) {
			projectedState = currentItemTradeReady
		} else {
			s.tradeMu.Unlock()
			return s.sendGameUpperFailure(session, currentItemTradeReadyMsg, 1)
		}
	case currentItemTradeReady:
		if trade.states[characterID] != currentItemTradeAddingDone ||
			(peerState != currentItemTradeAddingDone && peerState != currentItemTradeReady) {
			s.tradeMu.Unlock()
			return s.sendGameUpperFailure(session, currentItemTradeReadyMsg, 1)
		}
	case currentItemTradeAddingDone:
		if trade.states[characterID] != 0 {
			s.tradeMu.Unlock()
			return s.sendGameUpperFailure(session, currentItemTradeReadyMsg, 1)
		}
	case currentItemTradeWindowClosed:
		// 本侧交易窗口复位/关闭。 90CN.dll 在窗口"打开"与"关闭"两条路径上都会
		// 补发这条 state 0(原生复位函数是同一个),因此这里必须先排除开窗:
		// 刚建立且完全为空的交易收到 state 0 = 客户端开窗复位,不是用户取消。
		// 否则交易窗口一开就会被自己取消。
		age := time.Since(trade.createdAt)
		if !onlineItemTradeHasContent(trade) && age < currentItemTradeOpenResetGrace {
			s.tradeMu.Unlock()
			s.logGameEvent(session, "game-item-trade-window-open-reset-ignored",
				"char_id", characterID,
				"peer_char_id", peerID,
				"age_ms", age.Milliseconds())
			return s.sendGameUpperSuccess(session, currentItemTradeReadyMsg, nil)
		}
		// 真实取消:整单取消,而不是像旧行为那样"广播 0 但不取消会话"。
		// 不取消的后果是:对方窗口一直不关、双方停在交易态导致 NPC 商店拒绝
		// 购买、客户端已搬进交易容器的物品不回背包(服务端从未移动过物品,
		// 只有客户端以为物品在交易容器里)。
		s.tradeMu.Unlock()
		if err := s.sendGameUpperSuccess(session, currentItemTradeReadyMsg, nil); err != nil {
			return err
		}
		s.cancelOnlineItemTrade(trade, "client_closed_trade_window")
		return nil
	}
	trade.states[characterID] = projectedState
	peerState = trade.states[peerID]
	ready := projectedState == currentItemTradeReady && peerState == currentItemTradeReady
	addingDone := projectedState == currentItemTradeAddingDone && peerState == currentItemTradeAddingDone
	// 双方都提交「添加完成（锁定）」是本侧与对侧报价的最终定稿时刻：此刻双方都不能再改报价，
	// 因此在下面 ACK 之后需要立刻向双方各重放一次「对方」的报价快照，补齐客户端
	// 「双方锁定后交易界面显示对方物品」的缺失环节。重放必须等 tradeMu 释放后再发包。
	replayCounterpartOffers := addingDone
	if ready {
		trade.committing = true
	}
	s.tradeMu.Unlock()
	s.logGameEvent(session, "game-item-trade-state-updated",
		"char_id", characterID,
		"peer_char_id", peerID,
		"inbound_state", state,
		"projected_state", projectedState,
		"peer_state", peerState,
		"both_adding_done", addingDone,
		"both_ready", ready)

	if err := s.sendGameUpperSuccess(session, currentItemTradeReadyMsg, nil); err != nil {
		return err
	}
	if replayCounterpartOffers {
		// 时序：此处 tradeMu 已在上一步释放，满足「发包必须在 tradeMu 释放之后」的硬约束。
		// 先给本侧重放对方报价（本侧刚锁定，界面需要立刻补齐对方物品），再给对方重放本侧报价。
		// 硬约束：物品内容只走 op15（class 0），绝不挂到 op17；发包前逐一判定对端是否在线。
		if err := s.replayOnlineItemTradeCounterpartOffers(trade, session); err != nil {
			return err
		}
		if peer, peerOnline := s.onlineGameSession(peerID); peerOnline {
			if err := s.replayOnlineItemTradeCounterpartOffers(trade, peer); err != nil {
				return err
			}
		}
	}
	// The current client moves itself to "waiting for the other participant's
	// final confirmation" as soon as it submits state 3. Broadcasting op17/3
	// here would make sub_2340B10 close resource 727 on the peer that has not
	// answered yet, preventing the second consent. Keep final consent server-
	// side only; the second consent is followed directly by the atomic op18
	// completion (or the existing op16 rollback on failure).
	if projectedState == currentItemTradeReady {
		if ready {
			return s.commitOnlineItemTrade(trade)
		}
		return nil
	}
	stateBody := buildOnlineItemTradeStateBody(characterID, projectedState)
	if err := s.sendGameUpperRawClassCodec(session, currentItemTradeStateMsg, stateBody, 0, true); err != nil {
		return err
	}
	peer, peerOnline := s.onlineGameSession(peerID)
	if peerOnline {
		if err := s.sendGameUpperRawClassCodec(peer, currentItemTradeStateMsg, stateBody, 0, true); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) commitOnlineItemTrade(trade *onlineItemTrade) error {
	if trade == nil {
		return nil
	}
	firstID, secondID := trade.participants[0], trade.participants[1]
	firstOffers, secondOffers := snapshotOnlineItemTradeOffers(trade, firstID), snapshotOnlineItemTradeOffers(trade, secondID)
	firstGold, secondGold := snapshotOnlineItemTradeGold(trade, firstID), snapshotOnlineItemTradeGold(trade, secondID)
	repositories, ok := s.repositoryGroup()
	if !ok {
		s.cancelOnlineItemTrade(trade, "repository_unavailable")
		return nil
	}
	owner, err := itemtrade.NewOwner(repositories)
	if err != nil {
		s.cancelOnlineItemTrade(trade, "transaction_owner_unavailable")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	result, err := owner.Exchange(ctx,
		itemtrade.Participant{CharacterID: strconv.Itoa(int(firstID)), AccountID: s.accountIDForCharacter(ctx, repositories, firstID), Offers: itemTradeDomainOffers(firstOffers), Gold: firstGold},
		itemtrade.Participant{CharacterID: strconv.Itoa(int(secondID)), AccountID: s.accountIDForCharacter(ctx, repositories, secondID), Offers: itemTradeDomainOffers(secondOffers), Gold: secondGold},
	)
	if err != nil {
		s.logInfo("dnfbridge item trade atomic commit rejected", "first_char_id", firstID, "second_char_id", secondID, "error", err)
		s.cancelOnlineItemTrade(trade, "atomic_commit_rejected")
		return nil
	}
	s.clearOnlineItemTrade(trade)
	for _, characterID := range []uint16{firstID, secondID} {
		session, online := s.onlineGameSession(characterID)
		if !online {
			continue
		}
		// 这个角色本次收到了多少金币 = 对方报价的金币。
		peerCharID := firstID
		receivedGold := firstGold
		if characterID == firstID {
			peerCharID = secondID
			receivedGold = secondGold
		}
		character, goldChanged := result.Characters[strconv.Itoa(int(characterID))]

		// 钱包行必须在结果弹窗【之前】先下发一次。
		//
		// 客户端的「获得 XXXX 金币」系统提示只在钱包行的数值变多时触发；结果弹窗一旦
		// 出现就会把这条提示盖掉/抑制掉。实测：补上金币行让弹窗正常弹出之后，这条提示
		// 就再没出现过。先发一次让提示照常触发，弹窗之后再发一次同样的值兜底 ——
		// 无论客户端是否按 op18 的金币行自行结算，最终钱包数值都一定是正确的。
		if goldChanged {
			if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(character.Stats["gold"]), 0); err != nil {
				return err
			}
		}

		body := buildOnlineItemTradeFinishBody(
			result.Received[strconv.Itoa(int(characterID))], receivedGold)
		s.logPacketEvent("game-item-trade-finish-rows",
			"char_id", characterID,
			"peer_char_id", peerCharID,
			"item_rows", len(result.Received[strconv.Itoa(int(characterID))]),
			"gold_row", boolByte(receivedGold > 0),
			"received_gold", receivedGold,
			"gold_state_before_finish", boolByte(goldChanged),
			"rows", len(body)/4)
		if err := s.sendGameUpperRawClassCodec(session, currentItemTradeFinishMsg, body, 0, true); err != nil {
			return err
		}
		if goldChanged {
			if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(character.Stats["gold"]), 0); err != nil {
				return err
			}
		}
		if err := s.sendSelectedCurrentContainerListsRefresh(session, "current_exe_item_trade_atomic_commit"); err != nil {
			return err
		}
	}
	s.logInfo("dnfbridge item trade atomic commit completed", "first_char_id", firstID, "second_char_id", secondID,
		"first_offer_count", len(firstOffers), "second_offer_count", len(secondOffers),
		"first_gold", firstGold, "second_gold", secondGold)
	return nil
}

func snapshotOnlineItemTradeGold(trade *onlineItemTrade, characterID uint16) int64 {
	if trade == nil {
		return 0
	}
	return trade.gold[characterID]
}

func snapshotOnlineItemTradeOffers(trade *onlineItemTrade, characterID uint16) []onlineItemTradeOffer {
	offers := make([]onlineItemTradeOffer, 0, len(trade.offers[characterID]))
	for _, offer := range trade.offers[characterID] {
		offers = append(offers, offer)
	}
	sort.Slice(offers, func(i, j int) bool { return offers[i].tradeSlot < offers[j].tradeSlot })
	return offers
}

// replayOnlineItemTradeCounterpartOffers 向 recipient 重放「对方」已放入的报价行与金币行，
// 用于补齐「双方都点击添加完成（锁定）后，客户端交易界面显示对方物品」的缺失环节。
//
// 协议依据（与放入瞬间的逐件下发完全同构，保证客户端按同一路径解析）：
//   - 统一走 op15 currentItemTradeChangeMsg 的 class-0 通道，包体为单条 currentItemListEntry
//     （0x77 字节），不参与 op17（op17 只承载 charID + state，禁止挂物品内容）；
//   - 物品行：currentItemListEntryFromStack(offer.sourceList, offer.tradeSlot, stack)，槽位使用
//     服务端分配的 tradeSlot（3..11，与 addOnlineItemTradeOffer 的分配规则一致）；
//   - 行内数量必须取「本次放入数量」offer.count，不能直接用源背包槽的完整堆叠数量
//     （与 addOnlineItemTradeOffer 中 offer.stack.Count = int64(offer.count) 同理）；
//   - 金币行：currentGoldWalletItemListEntry(gold) 写入伪行 0（与 addOnlineItemTradeGold 一致）；
//     对方当前没有金币报价时，补发 buildOnlineItemTradeRemovedEntry(0) 做幂等清行
//     （与 removeOnlineItemTradeGold 的撤回下发同构），避免重复重放残留旧的对方金币显示。
//
// 重复调用是幂等的：同一槽位按行覆盖，客户端重复收到同一行不会重复计数。
//
// 时序与锁：调用方必须已释放 tradeMu（sendGameUpperRawClassCodec 内部还要取 wire 锁），
// 并且必须先确认 recipient 在线；本函数只在取快照时短暂加锁，随后逐行发包。
func (s *Service) replayOnlineItemTradeCounterpartOffers(trade *onlineItemTrade, recipient *gameSession) error {
	if s == nil || trade == nil || recipient == nil || recipient.selectedCharacterID == 0 {
		return nil
	}
	peerID, ok := trade.other(recipient.selectedCharacterID)
	if !ok {
		return nil
	}
	var offers []onlineItemTradeOffer
	var gold int64
	s.tradeMu.Lock()
	// 交易若已进入提交（对方已完成最终确认，界面即将由 op18 收尾）或已被取消/替换，
	// 则不再补发 op15，避免与提交结果竞争。
	if s.itemTrades[recipient.selectedCharacterID] == trade && !trade.committing {
		offers = snapshotOnlineItemTradeOffers(trade, peerID)
		gold = snapshotOnlineItemTradeGold(trade, peerID)
	}
	s.tradeMu.Unlock()
	for _, offer := range offers {
		stack := offer.stack
		stack.Count = int64(offer.count)
		entry := currentItemListEntryFromStack(offer.sourceList, int16(offer.tradeSlot), stack)
		if err := s.sendGameUpperRawClassCodec(recipient, currentItemTradeChangeMsg, entry.data[:], 0, true); err != nil {
			return err
		}
	}
	if gold > 0 {
		entry := currentGoldWalletItemListEntry(gold)
		return s.sendGameUpperRawClassCodec(recipient, currentItemTradeChangeMsg, entry.data[:], 0, true)
	}
	return s.sendGameUpperRawClassCodec(recipient, currentItemTradeChangeMsg, buildOnlineItemTradeRemovedEntry(0), 0, true)
}

func itemTradeDomainOffers(offers []onlineItemTradeOffer) []itemtrade.Offer {
	rows := make([]itemtrade.Offer, 0, len(offers))
	for _, offer := range offers {
		rows = append(rows, itemtrade.Offer{TradeSlot: offer.tradeSlot, SourceList: offer.sourceList,
			SourceSlot: offer.sourceSlot, Count: int64(offer.count), ExpectedItem: offer.itemID})
	}
	return rows
}

func (s *Service) clearOnlineItemTrade(trade *onlineItemTrade) {
	s.tradeMu.Lock()
	defer s.tradeMu.Unlock()
	if trade != nil {
		for _, characterID := range trade.participants {
			if s.itemTrades[characterID] == trade {
				delete(s.itemTrades, characterID)
			}
		}
	}
}

func (s *Service) cancelOnlineItemTrade(trade *onlineItemTrade, reason string) {
	if trade == nil {
		return
	}
	firstID, secondID := trade.participants[0], trade.participants[1]
	firstOffers, secondOffers := snapshotOnlineItemTradeOffers(trade, firstID), snapshotOnlineItemTradeOffers(trade, secondID)
	firstGold, secondGold := snapshotOnlineItemTradeGold(trade, firstID), snapshotOnlineItemTradeGold(trade, secondID)
	s.clearOnlineItemTrade(trade)
	for _, row := range []struct {
		characterID uint16
		offers      []onlineItemTradeOffer
		gold        int64
	}{{firstID, firstOffers, firstGold}, {secondID, secondOffers, secondGold}} {
		if session, online := s.onlineGameSession(row.characterID); online {
			_ = s.sendGameUpperRawClassCodec(session, currentItemTradeCancelMsg, buildOnlineItemTradeCancelBody(row.offers, row.gold), 0, true)
		}
	}
	s.logInfo("dnfbridge item trade cancelled", "first_char_id", firstID, "second_char_id", secondID, "reason", reason)
}

func (s *Service) detachOnlineItemTrade(session *gameSession, reason string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	s.tradeMu.Lock()
	trade := s.itemTrades[session.selectedCharacterID]
	s.tradeMu.Unlock()
	if trade != nil {
		s.cancelOnlineItemTrade(trade, reason)
	}
}

func buildOnlineItemTradeMoveAck(request inventory.MoveItemspaceRequest) []byte {
	var writer packetWriter
	writer.writeByte(request.SourceListType)
	writer.writeUint16(uint16(request.SourceSlotIndex))
	writer.writeInt32(int(request.MoveCount))
	writer.writeByte(request.DestinationListType)
	writer.writeUint16(uint16(request.DestinationSlotIndex))
	return writer.bytes()
}

func buildOnlineItemTradeStateBody(characterID uint16, state byte) []byte {
	var writer packetWriter
	writer.writeUint16(characterID)
	writer.writeByte(state)
	return writer.bytes()
}

func buildOnlineItemTradeCancelBody(offers []onlineItemTradeOffer, gold int64) []byte {
	var writer packetWriter
	count := len(offers)
	if gold > 0 {
		count++
	}
	writer.writeUint16(uint16(count))
	if gold > 0 {
		writer.writeUint16(0)
		writer.writeByte(dnfrepo.MainInventoryListType)
		writer.writeUint16(0)
	}
	for _, offer := range offers {
		writer.writeUint16(offer.tradeSlot)
		writer.writeByte(offer.sourceList)
		writer.writeUint16(uint16(offer.sourceSlot))
	}
	return writer.bytes()
}

// buildOnlineItemTradeFinishBody 构造 op18「交易完成」包体。
//
// 客户端处理器 sub_1D61130 的解析格式（反汇编确认）：
//
//	u16 行数
//	行数 × (u16 交易槽, u16 目标槽)
//
// 之后不再读任何字节 —— 也就是**没有独立的金币字段**。
//
// 实测缺陷：只发物品行时，纯金币交易的行数是 0，客户端据此认为「没有结果」而
// 完全不弹结果窗（收到金币的一方尤其明显）；有物品时行数 >0 才会弹，但金额恒为 0。
//
// 因此这里为**收到金币**的一方补一条金币伪行，让行数至少为 1：
// 交易槽 0（交易界面的 0..2 是保留伪行，0 = 金币）、目标槽 0（钱包伪行），
// 与 op16 取消包体里的金币行同构（buildOnlineItemTradeCancelBody）。
//
// 这是本轮实验：先验证「行数>0 才弹窗」这个条件。金额字段能否一并显示，
// 取决于客户端弹窗的数据来源（它读的是已被清空的交易状态），可能仍需客户端侧改动。
func buildOnlineItemTradeFinishBody(transfers []itemtrade.Transfer, receivedGold int64) []byte {
	rows := len(transfers)
	if receivedGold > 0 {
		rows++
	}
	var writer packetWriter
	writer.writeUint16(uint16(rows))
	for _, transfer := range transfers {
		writer.writeUint16(transfer.TradeSlot)
		writer.writeUint16(uint16(transfer.DestinationSlot))
	}
	if receivedGold > 0 {
		writer.writeUint16(0)
		writer.writeUint16(0)
	}
	return writer.bytes()
}

func cloneOnlineItemTradeStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	if stack.Extra != nil {
		extra := make(map[string]string, len(stack.Extra))
		for key, value := range stack.Extra {
			extra[key] = value
		}
		stack.Extra = extra
	}
	return stack
}
