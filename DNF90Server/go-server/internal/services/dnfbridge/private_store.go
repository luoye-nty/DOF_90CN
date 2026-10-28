package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemtrade "longheng.io/server/internal/modules/dnf/itemtrade"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentPrivateStoreCompleteOpcode = uint16(87)
	currentPrivateStoreMaxRows        = 14
	currentPrivateStoreMaxTitleBytes  = 80
	currentPrivateStorePremiumType    = premium.TypePrivateStore
	currentPrivateStoreError          = byte(4)

	// op87 completion error codes the current client actually renders. Its
	// mapper at sub_3040AC0 subtracts 0x11 and indexes a 198-entry table, so
	// every code below 17 - including the generic 4 used everywhere else -
	// closes the stall window with no message at all. That silence is what a
	// player sees as "the stall just never opened".
	//   17  dstr 1332  含有不存在的物品
	//   22  dstr 44346 人偶商店道具不足
	//   57  dstr 1326  错误的个人商店
	//   58  dstr 1325  不存在的个人商店
	//   104 dstr 44483 无法上架超过使用期限的物品。
	//   159 dstr 281   『包含无法使用的文字』
	//   214 dstr 42232 在物品已锁定状态下， 您无法进行当前操作。
	currentPrivateStoreErrorNoSuchItem    = byte(17)
	currentPrivateStoreErrorMissingTicket = byte(22)
	currentPrivateStoreErrorWrongStore    = byte(57)
	currentPrivateStoreErrorNoStore       = byte(58)
	currentPrivateStoreErrorExpiredItem   = byte(104)
	currentPrivateStoreErrorBadTitle      = byte(159)
	currentPrivateStoreErrorLockedItem    = byte(214)

	// op82 purchase error codes, from the client's own mapper sub_3040740
	// (code-1 indexes a 104-entry table). Anything outside the table is silent,
	// and the generic 4 happens to mean "物品栏空间不足", which is why every
	// unrelated purchase failure was reported to the buyer as a full bag.
	//   1   dstr 1331  请再试一次
	//   4   dstr 1330  物品栏空间不足
	//   10  dstr 1329  所持金币不足
	//   17  dstr 1327  不存在的物品
	//   21  dstr 1355  没有个人商店
	//   57  dstr 1326  错误的个人商店
	//   58  dstr 1325  不存在的个人商店
	//   104 dstr 44484 无法购买超过使用期限的物品。
	currentPrivateStoreBuyErrorRetry       = byte(1)
	currentPrivateStoreBuyErrorNoRoom      = byte(4)
	currentPrivateStoreBuyErrorNoGold      = byte(10)
	currentPrivateStoreBuyErrorNoSuchItem  = byte(17)
	currentPrivateStoreBuyErrorNoStore     = byte(21)
	currentPrivateStoreBuyErrorExpiredItem = byte(104)

	currentPrivateStoreCreateNotification      = uint16(78)
	currentPrivateStoreRemoveNotification      = uint16(79)
	currentPrivateStoreForceRemoveNotification = uint16(80)
	currentPrivateStoreTitleNotification       = uint16(81)
	currentPrivateStorePurchaseNotification    = uint16(82)
	currentPrivateStoreCountNotification       = uint16(83)
	currentPrivateStoreListNotification        = uint16(84)
)

var errCurrentPrivateStoreInvalid = errors.New("current private store request is invalid")

// currentPrivateStoreAccountSharedListType is the container id the current
// client uses for the account-owned crystal/soul warehouse. The server keeps
// those cells as ordinary list-0 slots 354..365 and merges them into the list
// the client is shown, so an op87 row naming list 36 is normalized back to
// list 0 and validated against the same merged view.
const currentPrivateStoreAccountSharedListType = byte(36)

// currentPrivateStoreDisplayStack projects one frozen row as the shopper must
// see it: the quantity actually put up for sale, not the seller's whole stack.
// The op83 contents and the op405 confirmation both used the raw source stack,
// so a crystal listed 200-at-a-time out of a 1066 warehouse cell advertised all
// 1066 and the buy dialog offered the wrong amount. Equipment is unaffected -
// its +0x06 dword is the quality seed, which currentItemListStackAmount still
// returns ahead of any count.
func currentPrivateStoreDisplayStack(row currentPrivateStoreRow, stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	display := stack
	if row.Count > 0 {
		display.Count = int64(row.Count)
	}
	return display
}

// accountIDForCharacter resolves the durable account that owns one character.
// The account warehouse is keyed by account, so a transfer that touches it has
// to name the owner even when the counterparty's session is not at hand.
func (s *Service) accountIDForCharacter(ctx context.Context, repositories dnfrepo.Group, characterID uint16) string {
	if repositories.Character == nil || characterID == 0 {
		return ""
	}
	record, found, err := repositories.Character.Load(ctx, strconv.FormatUint(uint64(characterID), 10))
	if err != nil || !found {
		return ""
	}
	return strings.TrimSpace(record.AccountID)
}

// currentPrivateStoreInventoryView returns the seller's list-0 inventory as the
// client sees it: the character record with the account warehouse overlaid.
// Every private-store path must read it, otherwise a row sourced from the
// crystal/soul warehouse looks like an empty slot.
func (s *Service) currentPrivateStoreInventoryView(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
	accountID string,
) (dnfrepo.InventoryRecord, bool, error) {
	record, found, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil || !found {
		return dnfrepo.InventoryRecord{}, found, err
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || repositories.AccountInventory == nil {
		return record, true, nil
	}
	accountRecord, accountFound, err := repositories.AccountInventory.Load(ctx, accountID)
	if err != nil {
		return dnfrepo.InventoryRecord{}, false, err
	}
	if !accountFound {
		return record, true, nil
	}
	return dnfrepo.MergeAccountSharedInventory(record, accountRecord), true, nil
}

type currentPrivateStoreRow struct {
	DisplayIndex    uint16
	Price           uint32
	ListType        byte
	Slot            int16
	Count           uint32
	ItemID          int64
	InstanceValue   uint32
	ExpectedBind    bool
	AllowSealed     bool
	PurchaseVersion uint64
}

type currentPrivateStore struct {
	OwnerID        uint16
	OwnerName      string
	OwnerAccountID string
	OwnerSession   *gameSession
	Village        byte
	Area           byte
	InstanceKey    uint32
	X              uint16
	Y              uint16
	DollCreated    bool
	Title          []byte
	Rows           []currentPrivateStoreRow
	Active         bool
	Generation     uint64
}

type currentPrivateStorePurchasePublication struct {
	mu                 sync.Mutex
	RequestBody        []byte
	BuyerResponse      []byte
	BuyerRefresh       []byte
	SellerID           uint16
	SellerGold         uint32
	SellerNotification []byte
	SellerUpdate       []byte
	DisplayIndex       uint16
	SoldQuantity       uint32
	SoldOut            bool
	Store              *currentPrivateStore
	buyerResponseSent  bool
	buyerRefreshSent   bool
	sellerNoticeSent   bool
	sellerUpdateSent   bool
	storeUpdateSent    bool
}

func cloneCurrentPrivateStore(store *currentPrivateStore) *currentPrivateStore {
	if store == nil {
		return nil
	}
	clone := *store
	clone.Title = append([]byte(nil), store.Title...)
	clone.Rows = append([]currentPrivateStoreRow(nil), store.Rows...)
	return &clone
}

func (s *Service) handleCurrentPrivateStoreCommand(session *gameSession, opcode uint16, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	switch opcode {
	case uint16(dnfenum.CmdPacketCreatePrivateStore):
		return s.createCurrentPrivateStore(session, body)
	case currentPrivateStoreCompleteOpcode:
		return s.completeCurrentPrivateStore(session, body)
	case uint16(dnfenum.CmdPacketRemovePrivateStore):
		return s.closeCurrentPrivateStoreCommand(session, opcode, body)
	case uint16(dnfenum.CmdPacketEnterPrivateStore):
		return s.enterCurrentPrivateStore(session, body)
	case uint16(dnfenum.CmdPacketExitPrivateStore):
		if len(body) != 0 {
			return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
		}
		s.privateStoreMu.Lock()
		_, entered := s.privateStoreVisitors[session]
		delete(s.privateStoreVisitors, session)
		s.privateStoreMu.Unlock()
		if !entered {
			return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
		}
		return s.sendGameUpperSuccess(session, opcode, nil)
	case uint16(dnfenum.CmdPacketBuyPrivateStoreItem):
		return s.buyCurrentPrivateStoreItem(session, body)
	case uint16(dnfenum.CmdPacketVerifyPrivateStoreItem):
		return s.verifyCurrentPrivateStoreItem(session, body)
	default:
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
}

func (s *Service) createCurrentPrivateStore(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketCreatePrivateStore)
	if len(body) != 8 || s == nil || s.onlinePlayers == nil || currentExpertJobSessionInDungeon(session) || runtimePartyStateSnapshot(session).PartyID != 0 {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	permissionSlot := binary.LittleEndian.Uint16(body[6:8])
	allowed, err := s.currentPrivateStoreEntitled(session)
	if err != nil || !allowed {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	player, ok := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID)
	if !ok || player.Session != session || player.TownID == 0 || player.TownID != body[0] || player.AreaID != body[1] {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	// Current NoPack sends the companion/store placement coordinates here.
	// They are scene-pixel coordinates and are not the op35/op36 town movement
	// coordinates cached by onlinePlayers. Requiring exact equality rejected the
	// live request `1f 01 e2 02 29 01 ff ff` even though the actor was in town 31,
	// area 1, leaving the client in its create-store waiting state.
	storeX := binary.LittleEndian.Uint16(body[2:4])
	storeY := binary.LittleEndian.Uint16(body[4:6])
	if storeX == 0 && storeY == 0 {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	s.expertJobStoreMu.Lock()
	_, expertStoreExists := s.expertJobStores[session.selectedCharacterID]
	s.expertJobStoreMu.Unlock()
	if expertStoreExists {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	store := &currentPrivateStore{
		OwnerID:        session.selectedCharacterID,
		OwnerName:      player.Name,
		OwnerAccountID: strings.TrimSpace(s.accountIDForSession(session)),
		OwnerSession:   session,
		Village:        player.TownID,
		Area:           player.AreaID,
		InstanceKey:    player.InstanceKey,
		X:              storeX,
		Y:              storeY,
		DollCreated:    permissionSlot != math.MaxUint16,
		Generation:     s.nextPrivateStoreGeneration.Add(1),
	}
	s.privateStoreOpMu.Lock()
	s.privateStoreMu.Lock()
	if s.privateStores == nil {
		s.privateStores = make(map[uint16]*currentPrivateStore)
	}
	_, exists := s.privateStores[store.OwnerID]
	if !exists {
		s.privateStores[store.OwnerID] = store
	}
	s.privateStoreMu.Unlock()
	s.privateStoreOpMu.Unlock()
	if exists {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	if err := s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		s.closeCurrentPrivateStoreSession(session, false)
		return err
	}
	if err := s.publishCurrentPrivateStoreCreated(store); err != nil {
		s.closeCurrentPrivateStoreSession(session, false)
		return err
	}
	return nil
}

func (s *Service) currentPrivateStoreEntitled(session *gameSession) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	return s.currentPrivateStoreEntitledForAccount(ctx, strings.TrimSpace(s.accountIDForSession(session)))
}

// currentPrivateStoreEntitledForAccount resolves the actor owner's durable
// contract. Mode0 scene projection also builds peer actors, so it must not use
// the viewing session's account when deciding whether the private-store
// companion is visible.
func (s *Service) currentPrivateStoreEntitledForAccount(ctx context.Context, accountID string) (bool, error) {
	repositories, ok := s.projectionRepositories(ctx)
	if !ok || repositories.Account == nil {
		return false, errors.New("account repository is unavailable")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil || !found {
		return false, err
	}
	now := time.Now().Unix()
	return premium.ExpireAtRaw(account, currentPrivateStorePremiumType) > now ||
		premium.ExpireAtRaw(account, premium.TypeAuction) > now, nil
}

func (s *Service) currentPrivateStoreItemEligible(itemID uint32) bool {
	if itemID == 0 {
		return false
	}
	if s.privateStoreItemEligibility != nil {
		return s.privateStoreItemEligibility(itemID)
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return false
	}
	definition, err := catalog.ResolveItem(itemID)
	return err == nil && currentDungeonDiscardTradeable(definition)
}

// The current EXE writes one polymorphic u32 at the end of every op87 row.
// Stackable rows carry the offered quantity. Equipment rows carry the exact
// inventory instance value (raw item bytes +0x06) and always offer one item.
func (s *Service) resolveCurrentPrivateStoreRowStack(listType byte, stack dnfrepo.ItemStack, wireValue uint32) (uint32, uint32, bool, bool, bool) {
	if stack.ItemID <= 0 || uint64(stack.ItemID) > math.MaxUint32 || stack.Count <= 0 {
		return 0, 0, false, false, false
	}
	kind := dungeonDropItemKind("")
	expectedBind := false
	allowSealed := false
	if s.privateStoreItemEligibility != nil {
		if !s.privateStoreItemEligibility(uint32(stack.ItemID)) {
			return 0, 0, false, false, false
		}
		// Test/robot composition may provide only the eligibility seam. Infer an
		// equipment row only when its exact persisted instance is present.
		if stack.Count == 1 && wireValue != 0 && currentPrivateStoreEquipmentInstance(listType, stack) == wireValue {
			kind = dungeonDropItemEquipment
			expectedBind = stack.Bind
			allowSealed = stack.Bind
		} else {
			kind = dungeonDropItemStackable
		}
	} else {
		catalog, err := s.currentPVFItemCatalog()
		if err != nil {
			return 0, 0, false, false, false
		}
		definition, err := catalog.ResolveItem(uint32(stack.ItemID))
		if err != nil {
			return 0, 0, false, false, false
		}
		kind = definition.Kind
		var eligible bool
		expectedBind, allowSealed, eligible = currentPrivateStoreRegistrationPolicyForDefinition(definition, stack)
		if !eligible {
			return 0, 0, false, false, false
		}
	}
	switch kind {
	case dungeonDropItemEquipment:
		// The inventory list projected to the current client takes the equipment
		// instance from the durable quality seed before it consults raw_entry.
		// Card/settlement rewards can legitimately have no 119-byte raw row while
		// still carrying that seed in Extra.  Requiring raw_entry here rejected the
		// whole store as soon as one such equipment row was mixed with stackables.
		instanceValue := currentPrivateStoreEquipmentInstance(listType, stack)
		if stack.Count != 1 || wireValue == 0 || instanceValue != wireValue {
			return 0, 0, false, false, false
		}
		return 1, instanceValue, expectedBind, allowSealed, true
	case dungeonDropItemStackable:
		if stack.Bind || wireValue == 0 || int64(wireValue) > stack.Count {
			return 0, 0, false, false, false
		}
		return wireValue, 0, false, false, true
	default:
		return 0, 0, false, false, false
	}
}

// Private stores use the item's ordinary tradeability rather than the auction
// catalog flag. Stackables must be unbound. Equipment follows the same
// `[attach type]` whitelist as the auction (only `[free]` and `[sealing]`),
// `[sealing]` additionally has to be sealed, and rarity plays no part. The
// item-trade owner rechecks the frozen bind authority atomically.
func currentPrivateStoreRegistrationPolicyForDefinition(definition dungeonDropItemDefinition, stack dnfrepo.ItemStack) (expectedBind bool, allowSealed bool, eligible bool) {
	if stack.ItemID != int64(definition.ItemID) || stack.Count <= 0 || !currentDungeonDiscardTradeable(definition) {
		return false, false, false
	}
	switch definition.Kind {
	case dungeonDropItemStackable:
		return false, false, !stack.Bind
	case dungeonDropItemEquipment:
		if stack.Count != 1 {
			return false, false, false
		}
		// `[attach type]` 白名单已在上面判过；`[sealing]` 再要求封装态，
		// `[free]` 不看封装状态。品级 [rarity] 不参与判断。
		if currentDungeonDiscardSealRequired(definition) && !currentDungeonDiscardSealed(stack) {
			return false, false, false
		}
		// 冻结的必须是 escrow owner 回查的那一位 stack.Bind
		// （itemtrade/owner.go: `stack.Bind != offer.ExpectedBind`），不能用封装位
		// 顶替，否则「已封装但 bind_flag=0」的装备在交接那一步会被打回。
		return stack.Bind, stack.Bind, true
	default:
		return false, false, false
	}
}

func currentPrivateStoreInstanceMatches(listType byte, stack dnfrepo.ItemStack, instanceValue uint32) bool {
	return instanceValue == 0 || currentPrivateStoreEquipmentInstance(listType, stack) == instanceValue
}

// currentPrivateStoreEquipmentInstance returns the dword the current client
// echoes back in the op87 row tail for an equipment item.
//
// The client does not invent that value: it replays the dword it received at
// +0x06 of the projected item row. The expectation therefore has to come from
// the same projection, not from a fixed RawEntry offset. Reading
// RawEntry[6:10] only agreed with the wire for rows already stored as a full
// 0x77 entry; a legacy 43-byte row (or a row with no raw entry at all) keeps a
// different field there, so the comparison produced 0 while the client had
// been shown - and returned - the projected amount. Every such equipment row
// failed the op87 completion with client error 4 and the stall never opened.
func currentPrivateStoreEquipmentInstance(listType byte, stack dnfrepo.ItemStack) uint32 {
	return currentItemListStackAmount(listType, stack)
}

// logPrivateStoreCompleteRejected records exactly why one op87 row was refused.
// The completion path previously answered client error 4 from thirteen
// different branches without a single log line, so a failed stall left no
// server-side trace at all and every diagnosis had to start from a packet
// capture. Keep the request body, the parsed row, and the slot the row names
// together: a row whose named slot is empty and a row whose instance dword
// disagrees are the two failures that look identical to the player.
func (s *Service) logPrivateStoreCompleteRejected(
	session *gameSession,
	body []byte,
	row currentPrivateStoreRow,
	stack dnfrepo.ItemStack,
	slotOccupied bool,
	reason string,
) {
	if s == nil {
		return
	}
	fields := []any{
		"character_id", selectedCharacterIDForLog(session),
		"reason", reason,
		"body_len", len(body),
		"body_hex", strings.ToUpper(hex.EncodeToString(body)),
		"row_display_index", row.DisplayIndex,
		"row_price", row.Price,
		"row_list_type", row.ListType,
		"row_slot", row.Slot,
		"row_wire_instance", row.InstanceValue,
		"slot_occupied", slotOccupied,
	}
	if slotOccupied {
		fields = append(fields,
			"slot_item_id", stack.ItemID,
			"slot_count", stack.Count,
			"slot_bind", stack.Bind,
			"slot_projected_instance", currentPrivateStoreEquipmentInstance(row.ListType, stack),
		)
	}
	s.logGameEvent(session, "game-private-store-complete-rejected", fields...)
}

func (s *Service) completeCurrentPrivateStore(session *gameSession, body []byte) error {
	title, next, ok := readCurrentRawDstr(body, 0)
	if !ok || len(title) == 0 || len(title) > currentPrivateStoreMaxTitleBytes || next >= len(body) {
		s.logPrivateStoreCompleteRejected(session, body, currentPrivateStoreRow{}, dnfrepo.ItemStack{}, false, "title_invalid")
		return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorBadTitle)
	}
	count := int(body[next])
	next++
	if count == 0 || count > currentPrivateStoreMaxRows || len(body) != next+count*13+2 {
		s.logPrivateStoreCompleteRejected(session, body, currentPrivateStoreRow{}, dnfrepo.ItemStack{}, false, "row_count_or_body_size")
		return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorNoSuchItem)
	}
	permissionSlot := binary.LittleEndian.Uint16(body[len(body)-2:])
	rows := make([]currentPrivateStoreRow, count)
	seenDisplay := make(map[uint16]struct{}, count)
	seenSlot := make(map[string]struct{}, count)
	for index := range rows {
		offset := next + index*13
		wireValue := binary.LittleEndian.Uint32(body[offset+9 : offset+13])
		row := currentPrivateStoreRow{
			DisplayIndex:  binary.LittleEndian.Uint16(body[offset : offset+2]),
			Price:         binary.LittleEndian.Uint32(body[offset+2 : offset+6]),
			ListType:      body[offset+6],
			Slot:          int16(binary.LittleEndian.Uint16(body[offset+7 : offset+9])),
			InstanceValue: wireValue,
		}
		// The client names the account-owned crystal/soul warehouse list 36 but
		// keeps the merged list-0 slot number. Normalize it back to the list the
		// repository and every op14 refresh use; anything else in list 36 is not
		// a cell this owner knows.
		if row.ListType == currentPrivateStoreAccountSharedListType {
			if !dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, row.Slot) {
				s.logPrivateStoreCompleteRejected(session, body, row, dnfrepo.ItemStack{}, false, "account_shared_slot_out_of_range")
				return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorNoSuchItem)
			}
			row.ListType = dnfrepo.MainInventoryListType
		}
		key := fmt.Sprintf("%d:%d", row.ListType, row.Slot)
		if row.DisplayIndex >= currentPrivateStoreMaxRows || row.Price == 0 || wireValue == 0 || row.Slot < 0 ||
			(row.ListType != 0 && row.ListType != 1) {
			// A row from a container the stall does not own lands here.
			s.logPrivateStoreCompleteRejected(session, body, row, dnfrepo.ItemStack{}, false, "row_fields_unsupported")
			return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorNoSuchItem)
		}
		if _, duplicate := seenDisplay[row.DisplayIndex]; duplicate {
			s.logPrivateStoreCompleteRejected(session, body, row, dnfrepo.ItemStack{}, false, "duplicate_display_index")
			return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorNoSuchItem)
		}
		if _, duplicate := seenSlot[key]; duplicate {
			s.logPrivateStoreCompleteRejected(session, body, row, dnfrepo.ItemStack{}, false, "duplicate_source_slot")
			return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorNoSuchItem)
		}
		seenDisplay[row.DisplayIndex] = struct{}{}
		seenSlot[key] = struct{}{}
		rows[index] = row
	}
	allowed, err := s.currentPrivateStoreEntitled(session)
	if err != nil || !allowed {
		s.logPrivateStoreCompleteRejected(session, body, currentPrivateStoreRow{}, dnfrepo.ItemStack{}, false, "trade_licence_missing")
		return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorMissingTicket)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		s.logPrivateStoreCompleteRejected(session, body, currentPrivateStoreRow{}, dnfrepo.ItemStack{}, false, "inventory_repository_unavailable")
		return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorNoSuchItem)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	inventory, found, err := s.currentPrivateStoreInventoryView(
		ctx, repositories, selectedCharacterIDString(session), s.accountIDForSession(session))
	if err != nil || !found {
		s.logPrivateStoreCompleteRejected(session, body, currentPrivateStoreRow{}, dnfrepo.ItemStack{}, false, "inventory_load_failed")
		return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorNoSuchItem)
	}
	now := time.Now()
	for index := range rows {
		stack, exists := inventory.Slots[fmt.Sprintf("%d:%d", rows[index].ListType, rows[index].Slot)]
		switch {
		case exists && stack.ItemID > 0 && currentDungeonDiscardStackLocked(stack):
			s.logPrivateStoreCompleteRejected(session, body, rows[index], stack, exists, "row_source_stack_locked")
			return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorLockedItem)
		case exists && stack.ItemID > 0 && currentDungeonDiscardStackExpired(stack, now):
			s.logPrivateStoreCompleteRejected(session, body, rows[index], stack, exists, "row_source_stack_expired")
			return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorExpiredItem)
		case !exists || stack.ItemID <= 0 || !s.currentPrivateStoreItemEligible(uint32(stack.ItemID)):
			s.logPrivateStoreCompleteRejected(session, body, rows[index], stack, exists, "row_source_stack_unusable")
			return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorNoSuchItem)
		}
		count, instanceValue, expectedBind, allowSealed, valid := s.resolveCurrentPrivateStoreRowStack(rows[index].ListType, stack, rows[index].InstanceValue)
		if !valid {
			s.logPrivateStoreCompleteRejected(session, body, rows[index], stack, exists, "row_stack_resolution_failed")
			return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, currentPrivateStoreErrorNoSuchItem)
		}
		rows[index].Count = count
		rows[index].InstanceValue = instanceValue
		rows[index].ExpectedBind = expectedBind
		rows[index].AllowSealed = allowSealed
		rows[index].ItemID = stack.ItemID
	}
	s.privateStoreOpMu.Lock()
	s.privateStoreMu.Lock()
	store := s.privateStores[session.selectedCharacterID]
	storeDrifted := false
	if store == nil || store.OwnerSession != session {
		store = nil
	} else if store.DollCreated != (permissionSlot != math.MaxUint16) {
		store, storeDrifted = nil, true
	} else if !store.Active {
		store.Title = append([]byte(nil), title...)
		store.Rows = append([]currentPrivateStoreRow(nil), rows...)
		store.Active = true
		store = cloneCurrentPrivateStore(store)
	} else if currentPrivateStoreCompletionMatches(store, title, rows) {
		// The recovered 20260825 robot retries CompletePrivateStoreDisplay when
		// its first completion response/projection is lost.  Treat only the
		// exact frozen title and row set as an idempotent replay; any drift still
		// fails closed.  Re-publishing op81 repairs both a lost owner projection
		// and a peer that entered the area between the two attempts.
		store = cloneCurrentPrivateStore(store)
	} else {
		store = nil
	}
	s.privateStoreMu.Unlock()
	s.privateStoreOpMu.Unlock()
	if store == nil {
		code := currentPrivateStoreErrorNoStore
		reason := "store_missing_or_not_owned"
		if storeDrifted {
			code, reason = currentPrivateStoreErrorWrongStore, "store_doll_flag_drifted"
		}
		s.logPrivateStoreCompleteRejected(session, body, currentPrivateStoreRow{}, dnfrepo.ItemStack{}, false, reason)
		return s.sendGameUpperFailure(session, currentPrivateStoreCompleteOpcode, code)
	}
	if err := s.publishCurrentPrivateStoreCompleted(store); err != nil {
		s.closeCurrentPrivateStoreSession(session, false)
		return err
	}
	if err := s.sendGameUpperSuccess(session, currentPrivateStoreCompleteOpcode, []byte{boolByte(store.DollCreated)}); err != nil {
		s.closeCurrentPrivateStoreSession(session, false)
		return err
	}
	return nil
}

func currentPrivateStoreCompletionMatches(store *currentPrivateStore, title []byte, rows []currentPrivateStoreRow) bool {
	if store == nil || !store.Active || !bytes.Equal(store.Title, title) || len(store.Rows) != len(rows) {
		return false
	}
	for index := range rows {
		if store.Rows[index] != rows[index] {
			return false
		}
	}
	return true
}

func (s *Service) closeCurrentPrivateStoreCommand(session *gameSession, opcode uint16, body []byte) error {
	if len(body) != 0 {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	store := s.removeCurrentPrivateStore(session.selectedCharacterID, session)
	if store == nil {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	if err := s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		return err
	}
	s.broadcastCurrentPrivateStoreClose(store, false)
	s.refreshCurrentPrivateStoreSourceSlots(session, store)
	return nil
}

// refreshCurrentPrivateStoreSourceSlots republishes every cell the closed stall
// was selling from.
//
// A stall never moves its items: the rows name live inventory cells and the
// durable inventory keeps them the whole time. The client, though, drops the
// listed stacks out of the bag view once the stall is up, so closing it left the
// unsold leftovers invisible until the player reselected the character - the
// live report. Re-reading the cells from the repository and pushing them as
// ordinary op14 rows is what puts them back: an unsold row comes back with its
// real id, a sold-out cell comes back as the -1 delete row, and a cell that
// already matches is an idempotent in-place update.
//
// The rows are the closed store's own sources, so this can never touch a slot
// the seller did not list.
func (s *Service) refreshCurrentPrivateStoreSourceSlots(session *gameSession, store *currentPrivateStore) {
	if s == nil || session == nil || store == nil || len(store.Rows) == 0 {
		return
	}
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(store.Rows))
	for _, row := range store.Rows {
		if row.Slot < 0 {
			continue
		}
		listType := row.ListType
		if listType == currentPrivateStoreAccountSharedListType {
			listType = dnfrepo.MainInventoryListType
		}
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: listType, SlotIndex: row.Slot})
	}
	if len(refreshes) == 0 {
		return
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "private_store_close_return", refreshes); err != nil {
		// The stall is already gone; a failed repaint must not turn a successful
		// close into an error the player sees.
		s.logGameEvent(session, "game-private-store-close-refresh-failed",
			"char_id", session.selectedCharacterID, "rows", len(refreshes), "error", err)
	}
}

func (s *Service) removeCurrentPrivateStore(ownerID uint16, ownerSession *gameSession) *currentPrivateStore {
	if s == nil || ownerID == 0 {
		return nil
	}
	s.privateStoreOpMu.Lock()
	defer s.privateStoreOpMu.Unlock()
	s.privateStoreMu.Lock()
	defer s.privateStoreMu.Unlock()
	store := s.privateStores[ownerID]
	if store == nil || (ownerSession != nil && store.OwnerSession != ownerSession) {
		return nil
	}
	delete(s.privateStores, ownerID)
	for visitor, sellerID := range s.privateStoreVisitors {
		if sellerID == ownerID {
			delete(s.privateStoreVisitors, visitor)
		}
	}
	return cloneCurrentPrivateStore(store)
}

func (s *Service) closeCurrentPrivateStoreSession(session *gameSession, includeOwner bool) {
	if s == nil || session == nil {
		return
	}
	store := s.removeCurrentPrivateStore(session.selectedCharacterID, session)
	s.privateStoreMu.Lock()
	delete(s.privateStoreVisitors, session)
	s.privateStoreMu.Unlock()
	if store != nil && store.Active {
		s.broadcastCurrentPrivateStoreClose(store, includeOwner)
	}
	clearCurrentPrivateStorePending(session)
}

func (s *Service) enterCurrentPrivateStore(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketEnterPrivateStore)
	if len(body) != 4 || s.onlinePlayers == nil || currentExpertJobSessionInDungeon(session) {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	ownerValue := binary.LittleEndian.Uint32(body)
	if ownerValue == 0 || ownerValue > math.MaxUint16 {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	ownerID := uint16(ownerValue)
	visitor, visitorOK := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID)
	s.privateStoreMu.Lock()
	store := cloneCurrentPrivateStore(s.privateStores[ownerID])
	s.privateStoreMu.Unlock()
	if !visitorOK || visitor.Session != session || store == nil || !store.Active || store.OwnerSession == nil ||
		store.Village != visitor.TownID || store.Area != visitor.AreaID || store.InstanceKey != visitor.InstanceKey ||
		!s.currentSocialPeerAvailable(session.selectedCharacterID, ownerID) {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	inventory, found, err := s.currentPrivateStoreInventoryView(
		ctx, repositories, strconv.FormatUint(uint64(ownerID), 10), store.OwnerAccountID)
	if err != nil || !found {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	var writer packetWriter
	writer.writeUint16(ownerID)
	writer.writeByte(byte(len(store.Rows)))
	for _, row := range store.Rows {
		stack, exists := inventory.Slots[fmt.Sprintf("%d:%d", row.ListType, row.Slot)]
		if !exists || stack.ItemID != row.ItemID || stack.Count < int64(row.Count) || stack.Bind != row.ExpectedBind ||
			(stack.Bind && !row.AllowSealed) || currentDungeonDiscardStackLocked(stack) ||
			currentDungeonDiscardStackExpired(stack, time.Now()) || !s.currentPrivateStoreItemEligible(uint32(stack.ItemID)) {
			return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
		}
		// The native op83 reader passes raw[0:2] to the private-store
		// container as the destination cell.  The op87 row freezes that cell
		// separately from the seller's inventory source slot.  Reusing the
		// source slot here makes otherwise valid items disappear whenever the
		// inventory slot is outside the store's 0..13 display range.
		entry := currentItemListEntryFromStack(row.ListType, int16(row.DisplayIndex), currentPrivateStoreDisplayStack(row, stack))
		writer.writeUint32(row.Price)
		writer.writeBytes(entry.data[:])
	}
	s.privateStoreMu.Lock()
	if current := s.privateStores[ownerID]; current == nil || !current.Active || current.Generation != store.Generation {
		s.privateStoreMu.Unlock()
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	if s.privateStoreVisitors == nil {
		s.privateStoreVisitors = make(map[*gameSession]uint16)
	}
	s.privateStoreVisitors[session] = ownerID
	s.privateStoreMu.Unlock()
	return s.sendGameUpperSuccess(session, opcode, writer.bytes())
}

// logPrivateStoreBuyRejected and currentPrivateStoreBuyFailureCode close the
// same observability gap the op87 completion had: the purchase path answered
// the generic code 4 from a dozen branches with no log line, and the client
// renders that code as "物品栏空间不足", so every refusal was reported to the
// buyer as a full bag.
func (s *Service) logPrivateStoreBuyRejected(session *gameSession, body []byte, reason string, cause error, fields ...any) {
	if s == nil {
		return
	}
	args := []any{
		"character_id", selectedCharacterIDForLog(session),
		"reason", reason,
		"body_hex", strings.ToUpper(hex.EncodeToString(body)),
	}
	if cause != nil {
		args = append(args, "error", cause.Error())
	}
	args = append(args, fields...)
	s.logGameEvent(session, "game-private-store-buy-rejected", args...)
}

func currentPrivateStoreBuyFailureCode(err error) byte {
	switch {
	case err == nil:
		return currentPrivateStoreBuyErrorRetry
	case errors.Is(err, dnfitemtrade.ErrInventoryFull):
		return currentPrivateStoreBuyErrorNoRoom
	case errors.Is(err, dnfitemtrade.ErrGoldUnavailable), errors.Is(err, dnfitemtrade.ErrGoldOverflow):
		return currentPrivateStoreBuyErrorNoGold
	case errors.Is(err, dnfitemtrade.ErrItemUnavailable), errors.Is(err, dnfitemtrade.ErrItemNotTradeable),
		errors.Is(err, dnfitemtrade.ErrInvalidOffer):
		return currentPrivateStoreBuyErrorNoSuchItem
	default:
		return currentPrivateStoreBuyErrorRetry
	}
}

func (s *Service) buyCurrentPrivateStoreItem(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketBuyPrivateStoreItem)
	if len(body) != 6 {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	if pending := currentPrivateStorePending(session); pending != nil {
		if !bytes.Equal(pending.RequestBody, body) {
			s.logPrivateStoreBuyRejected(session, body, "pending_purchase_body_mismatch", nil)
			return s.sendGameUpperFailure(session, opcode, currentPrivateStoreBuyErrorRetry)
		}
		return s.publishCurrentPrivateStorePurchase(session, pending)
	}
	displayIndex := binary.LittleEndian.Uint16(body[:2])
	wireValue := binary.LittleEndian.Uint32(body[2:])
	s.privateStoreOpMu.Lock()
	defer s.privateStoreOpMu.Unlock()
	s.privateStoreMu.Lock()
	sellerID := s.privateStoreVisitors[session]
	store := cloneCurrentPrivateStore(s.privateStores[sellerID])
	s.privateStoreMu.Unlock()
	if sellerID == 0 || store == nil || !store.Active || sellerID == session.selectedCharacterID || s.onlinePlayers == nil ||
		!s.onlinePlayers.PeerInSameArea(session.selectedCharacterID, sellerID) {
		s.logPrivateStoreBuyRejected(session, body, "store_unreachable", nil, "seller_id", sellerID, "store_present", store != nil)
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreBuyErrorNoStore)
	}
	var selected currentPrivateStoreRow
	rowIndex := -1
	for index := range store.Rows {
		if store.Rows[index].DisplayIndex == displayIndex {
			selected = store.Rows[index]
			rowIndex = index
			break
		}
	}
	quantity := wireValue
	if selected.InstanceValue != 0 {
		if wireValue != selected.InstanceValue {
			s.logPrivateStoreBuyRejected(session, body, "instance_mismatch", nil, "row_instance", selected.InstanceValue, "wire_value", wireValue)
			return s.sendGameUpperFailure(session, opcode, currentPrivateStoreBuyErrorNoSuchItem)
		}
		quantity = 1
	}
	if rowIndex < 0 || quantity == 0 || quantity > selected.Count || uint64(selected.Price)*uint64(quantity) > math.MaxInt64 ||
		!s.currentPrivateStoreItemEligible(uint32(selected.ItemID)) {
		s.logPrivateStoreBuyRejected(session, body, "row_or_quantity_invalid", nil,
			"row_index", rowIndex, "quantity", quantity, "row_count", selected.Count, "row_item_id", selected.ItemID)
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreBuyErrorNoSuchItem)
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		s.logPrivateStoreBuyRejected(session, body, "repository_unavailable", nil)
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreBuyErrorRetry)
	}
	owner, err := dnfitemtrade.NewOwner(repositories)
	if err != nil {
		s.logPrivateStoreBuyRejected(session, body, "trade_owner_unavailable", err)
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreBuyErrorRetry)
	}
	buyerID := selectedCharacterIDString(session)
	sellerCharacterID := strconv.FormatUint(uint64(store.OwnerID), 10)
	total := int64(selected.Price) * int64(quantity)
	completionKey := fmt.Sprintf("private-store:%d:%d:%d:%d:%d:%d", store.Generation, selected.PurchaseVersion, store.OwnerID, session.selectedCharacterID, displayIndex, quantity)
	trade := func() (dnfitemtrade.Result, error) {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		return owner.ExchangeWithCompletionKey(ctx, completionKey, buyerID,
			dnfitemtrade.Participant{CharacterID: sellerCharacterID, AccountID: store.OwnerAccountID, Offers: []dnfitemtrade.Offer{{TradeSlot: displayIndex, SourceList: selected.ListType, SourceSlot: selected.Slot, Count: int64(quantity), ExpectedItem: selected.ItemID, ExpectedInstance: selected.InstanceValue, ExpectedBind: selected.ExpectedBind, AllowSealed: selected.AllowSealed}}},
			dnfitemtrade.Participant{CharacterID: buyerID, AccountID: strings.TrimSpace(s.accountIDForSession(session)), Gold: total},
		)
	}
	result, err := trade()
	if err != nil {
		// A storage backend can commit and still return an indeterminate error.
		// Repeating the identical receipt key converts that case into a replay;
		// deterministic validation failures remain failures.
		result, err = trade()
	}
	if err != nil || len(result.Received[buyerID]) != 1 {
		s.logPrivateStoreBuyRejected(session, body, "atomic_exchange_failed", err,
			"seller_char_id", sellerCharacterID, "seller_account_id", store.OwnerAccountID,
			"buyer_char_id", buyerID, "source_list", selected.ListType, "source_slot", selected.Slot,
			"quantity", quantity, "transfers", len(result.Received[buyerID]))
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreBuyFailureCode(err))
	}
	remaining := selected.Count - quantity
	s.privateStoreMu.Lock()
	current := s.privateStores[sellerID]
	if current == nil || !current.Active || current.Generation != store.Generation || rowIndex >= len(current.Rows) ||
		current.Rows[rowIndex].DisplayIndex != displayIndex || current.Rows[rowIndex].PurchaseVersion != selected.PurchaseVersion || current.Rows[rowIndex].Count != selected.Count {
		s.privateStoreMu.Unlock()
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	if remaining == 0 {
		current.Rows = append(current.Rows[:rowIndex], current.Rows[rowIndex+1:]...)
		if len(current.Rows) == 0 {
			current.Active = false
		}
	} else {
		current.Rows[rowIndex].Count = remaining
		current.Rows[rowIndex].PurchaseVersion++
	}
	updatedStore := cloneCurrentPrivateStore(current)
	if !current.Active {
		delete(s.privateStores, sellerID)
		for visitor, ownerID := range s.privateStoreVisitors {
			if ownerID == sellerID {
				delete(s.privateStoreVisitors, visitor)
			}
		}
	}
	s.privateStoreMu.Unlock()

	transfer := result.Received[buyerID][0]
	buyerInventory := result.Inventories[buyerID]
	buyerStack, ok := buyerInventory.Slots[fmt.Sprintf("%d:%d", transfer.DestinationList, transfer.DestinationSlot)]
	if !ok {
		s.logPrivateStoreBuyRejected(session, body, "buyer_destination_missing", nil)
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreBuyErrorRetry)
	}
	// The op82 row is what the buyer just received: the client prints its count
	// as dstr 42346 "购买了[%s] %d个". Sending the destination cell's new total
	// made a purchase of 3 into a stack of 193 announce "196个". The cell's real
	// total follows as an ordinary absolute op14 row.
	entry := currentItemListEntryFromStack(transfer.DestinationList, transfer.DestinationSlot, transfer.Stack)
	buyerRefresh := buildCurrentItemUpdateBody(
		transfer.DestinationList,
		[]currentItemListEntry{currentItemListEntryFromStack(transfer.DestinationList, transfer.DestinationSlot, buyerStack)},
	)
	walletCtx, walletCancel := context.WithTimeout(context.Background(), createWriteTimeout)
	buyerCera := s.loadCurrentAccountCera(walletCtx, repositories, session)
	walletCancel()
	var response packetWriter
	response.writeUint32(sceneInventoryUint32FromInt64(result.Characters[buyerID].Stats["gold"]))
	response.writeUint32(sceneInventoryUint32FromInt64(result.Characters[buyerID].Stats["sp"]))
	response.writeUint32(sceneInventoryUint32FromInt64(buyerCera))
	response.writeBytes(entry.data[:])
	sellerInventory := result.Inventories[sellerCharacterID]
	sellerStack, sellerStackExists := sellerInventory.Slots[fmt.Sprintf("%d:%d", selected.ListType, selected.Slot)]
	var sourceUpdate currentItemListEntry
	switch {
	case sellerStackExists && sellerStack.ItemID > 0:
		sourceUpdate = currentItemListEntryFromStack(selected.ListType, selected.Slot, sellerStack)
	case dnfrepo.IsAccountSharedInventorySlot(selected.ListType, selected.Slot):
		// The crystal/soul panel wants a zero pair for "empty"; it never
		// recognises 0xFFFFFFFF (see sendSelectedIncrementalItemSlotRefreshes).
		sourceUpdate.patchCore(selected.Slot, 0, 0)
	default:
		// -1 is the only value the client's op14 reader treats as a delete
		// (sub_1D73120 at 0x1D73912). A zero here leaves the sold object alive
		// in the seller's cell, and the next genuine row for that cell is then
		// thrown away by the id-changed check - the same trap
		// sendCurrentAuctionInventorySlot documents.
		sourceUpdate.patchCore(selected.Slot, math.MaxUint32, 0)
	}
	buyerName := result.Characters[buyerID].Name
	if buyerName == "" {
		if player, ok := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID); ok {
			buyerName = player.Name
		}
	}
	publication := &currentPrivateStorePurchasePublication{
		RequestBody:        append([]byte(nil), body...),
		BuyerResponse:      response.bytes(),
		BuyerRefresh:       buyerRefresh,
		SellerID:           sellerID,
		SellerGold:         sceneInventoryUint32FromInt64(result.Characters[sellerCharacterID].Stats["gold"]),
		SellerNotification: buildCurrentPrivateStorePurchaseNotification(selected.ItemID, displayIndex, quantity, buyerName, selected.Price, quantity, result.Characters[sellerCharacterID].Stats["gold"]),
		SellerUpdate:       buildCurrentItemUpdateBody(selected.ListType, []currentItemListEntry{sourceUpdate}),
		DisplayIndex:       displayIndex,
		SoldQuantity:       quantity,
		SoldOut:            !updatedStore.Active,
		Store:              updatedStore,
	}
	setCurrentPrivateStorePending(session, publication)
	return s.publishCurrentPrivateStorePurchase(session, publication)
}

func (s *Service) publishCurrentPrivateStorePurchase(session *gameSession, publication *currentPrivateStorePurchasePublication) error {
	if publication == nil {
		return errCurrentPrivateStoreInvalid
	}
	publication.mu.Lock()
	defer publication.mu.Unlock()

	var buyerErr error
	if !publication.buyerResponseSent {
		if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketBuyPrivateStoreItem), publication.BuyerResponse); err != nil {
			buyerErr = err
		} else {
			publication.buyerResponseSent = true
		}
	}
	// The op82 row only carries the delivered quantity, so the destination cell
	// still needs its absolute total. op14 is the ordinary row refresh already
	// used for the seller's source slot.
	if publication.buyerResponseSent && !publication.buyerRefreshSent && len(publication.BuyerRefresh) > 0 {
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), publication.BuyerRefresh, 0); err == nil {
			publication.buyerRefreshSent = true
		}
	}
	if sellerSession, online := s.onlineGameSession(publication.SellerID); online {
		if !publication.sellerNoticeSent {
			if err := s.sendGameUpperRawClass(sellerSession, currentPrivateStorePurchaseNotification, publication.SellerNotification, 0); err == nil {
				publication.sellerNoticeSent = true
			}
		}
		if !publication.sellerUpdateSent {
			if err := s.sendGameUpperRawClass(sellerSession, uint16(dnfenum.CmdPacketWalkoutPartyMember), publication.SellerUpdate, 0); err == nil {
				publication.sellerUpdateSent = true
			}
		}
	}
	if !publication.storeUpdateSent {
		if publication.SoldOut {
			s.broadcastCurrentPrivateStoreClose(publication.Store, true)
		} else {
			s.broadcastCurrentPrivateStoreCount(publication.Store, publication.DisplayIndex, publication.SoldQuantity)
		}
		publication.storeUpdateSent = true
	}
	if publication.buyerResponseSent {
		clearCurrentPrivateStorePending(session)
	}
	return buyerErr
}

func currentPrivateStorePending(session *gameSession) *currentPrivateStorePurchasePublication {
	if session == nil {
		return nil
	}
	session.privateStoreStateMu.Lock()
	defer session.privateStoreStateMu.Unlock()
	return session.privateStorePending
}

func setCurrentPrivateStorePending(session *gameSession, publication *currentPrivateStorePurchasePublication) {
	if session == nil {
		return
	}
	session.privateStoreStateMu.Lock()
	session.privateStorePending = publication
	session.privateStoreStateMu.Unlock()
}

func clearCurrentPrivateStorePending(session *gameSession) {
	if session == nil {
		return
	}
	session.privateStoreStateMu.Lock()
	session.privateStorePending = nil
	session.privateStoreStateMu.Unlock()
}

func (s *Service) verifyCurrentPrivateStoreItem(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketVerifyPrivateStoreItem)
	if len(body) != 6 {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	displayIndex := binary.LittleEndian.Uint16(body[:2])
	wireValue := binary.LittleEndian.Uint32(body[2:])
	s.privateStoreMu.Lock()
	sellerID := s.privateStoreVisitors[session]
	store := s.privateStores[sellerID]
	var row *currentPrivateStoreRow
	sellerAccountID := ""
	if store != nil && store.Active {
		sellerAccountID = store.OwnerAccountID
		for index := range store.Rows {
			if store.Rows[index].DisplayIndex == displayIndex {
				copyRow := store.Rows[index]
				row = &copyRow
				break
			}
		}
	}
	s.privateStoreMu.Unlock()
	if sellerID == 0 || row == nil || s.onlinePlayers == nil || !s.onlinePlayers.PeerInSameArea(session.selectedCharacterID, sellerID) {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	inventory, found, err := s.currentPrivateStoreInventoryView(
		ctx, repositories, strconv.FormatUint(uint64(sellerID), 10), sellerAccountID)
	if err != nil || !found {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	stack, exists := inventory.Slots[fmt.Sprintf("%d:%d", row.ListType, row.Slot)]
	requestMatches := wireValue > 0 && ((row.InstanceValue == 0 && wireValue <= row.Count) ||
		(row.InstanceValue != 0 && wireValue == row.InstanceValue))
	if !requestMatches || !exists || stack.ItemID != row.ItemID || stack.Count < int64(row.Count) ||
		stack.Bind != row.ExpectedBind || (stack.Bind && !row.AllowSealed) ||
		!currentPrivateStoreInstanceMatches(row.ListType, stack, row.InstanceValue) || currentDungeonDiscardStackLocked(stack) ||
		currentDungeonDiscardStackExpired(stack, time.Now()) || !s.currentPrivateStoreItemEligible(uint32(stack.ItemID)) {
		return s.sendGameUpperFailure(session, opcode, currentPrivateStoreError)
	}
	// The verification dialog indexes the same 0..13 private-store container
	// populated by op83. Keep the authoritative seller stack contents, but use
	// the frozen display cell instead of the seller's main-inventory source
	// slot. Returning source slot 47 for display cell 2 leaves the current
	// client waiting forever after an otherwise successful op405 response.
	entry := currentItemListEntryFromStack(row.ListType, int16(row.DisplayIndex), currentPrivateStoreDisplayStack(*row, stack))
	var writer packetWriter
	writer.writeUint32(row.Price)
	writer.writeBytes(entry.data[:])
	return s.sendGameUpperSuccess(session, opcode, writer.bytes())
}

func buildCurrentPrivateStoreCreateNotification(store *currentPrivateStore) []byte {
	var writer packetWriter
	writer.writeUint16(store.OwnerID)
	writer.writeByte(store.Village)
	writer.writeByte(store.Area)
	writer.writeUint16(store.X)
	writer.writeUint16(store.Y)
	writer.writeByte(boolByte(store.DollCreated))
	return writer.bytes()
}

func buildCurrentPrivateStoreRemoveNotification(ownerID uint16) []byte {
	var writer packetWriter
	writer.writeUint16(ownerID)
	return writer.bytes()
}

func buildCurrentPrivateStoreTitleNotification(store *currentPrivateStore) []byte {
	var writer packetWriter
	writer.writeUint16(store.OwnerID)
	writer.writeRawDstr(store.Title)
	writer.writeByte(boolByte(store.DollCreated))
	return writer.bytes()
}

func buildCurrentPrivateStoreListNotification(stores []*currentPrivateStore) []byte {
	if len(stores) > math.MaxUint8 {
		stores = stores[:math.MaxUint8]
	}
	sort.Slice(stores, func(i, j int) bool { return stores[i].OwnerID < stores[j].OwnerID })
	var writer packetWriter
	writer.writeByte(byte(len(stores)))
	for _, store := range stores {
		writer.writeUint16(store.OwnerID)
		writer.writeRawDstr(store.Title)
		writer.writeByte(1)
		writer.writeByte(store.Village)
		writer.writeByte(store.Area)
		writer.writeUint16(store.X)
		writer.writeUint16(store.Y)
		writer.writeByte(boolByte(store.DollCreated))
	}
	return writer.bytes()
}

// buildCurrentPrivateStoreCountNotification builds the class-0 op83 cell
// update every shopper in the store receives. sub_1D815D0 reads
// `u16 cell, u32 value` and forwards it to the same subtracting vtable+0x7C, so
// the value is the quantity just sold.
func buildCurrentPrivateStoreCountNotification(displayIndex uint16, soldQuantity uint32) []byte {
	var writer packetWriter
	writer.writeUint16(displayIndex)
	writer.writeUint32(soldQuantity)
	return writer.bytes()
}

// buildCurrentPrivateStorePurchaseNotification builds the seller's class-0
// op82 notice. Its dword after the display index is the quantity that was just
// sold, not the remainder: the client hands that value to the store window's
// vtable+0x7C, which *subtracts* it from the cell it already tracks. Sending
// the remainder there corrupted the seller's own bookkeeping - after selling 1
// of 200 the cell became 200-199=1 (and DNF draws no label for a single item,
// so the number looked blank), and the next sale drove it below zero, emptied
// the cell, and sub_3045580 closed the whole stall once all 14 cells read
// empty.
func buildCurrentPrivateStorePurchaseNotification(itemID int64, displayIndex uint16, soldQuantity uint32, buyerName string, unitPrice uint32, quantity uint32, sellerGold int64) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeUint32(sceneInventoryUint32FromInt64(sellerGold))
	writer.writeUint32(uint32(itemID))
	writer.writeUint16(displayIndex)
	writer.writeUint32(soldQuantity)
	writer.writeRawDstr(rosterNameBytes(buyerName))
	writer.writeUint32(unitPrice)
	writer.writeUint32(quantity)
	writer.writeByte(0)
	return writer.bytes()
}

func (s *Service) publishCurrentPrivateStoreCreated(store *currentPrivateStore) error {
	if s == nil || store == nil || s.onlinePlayers == nil {
		return errCurrentPrivateStoreInvalid
	}
	createBody := buildCurrentPrivateStoreCreateNotification(store)
	if store.OwnerSession == nil {
		return errCurrentPrivateStoreInvalid
	}
	// The original CreatePrivateStore success path sends the class-1 ACK first,
	// then broadcasts this class-0 create notification to the complete area.
	// The owner must receive it too: the current client does not advance from its
	// companion click into the store editor after the ACK alone.
	if err := s.sendGameUpperRawClass(store.OwnerSession, currentPrivateStoreCreateNotification, createBody, 0); err != nil {
		return err
	}
	for _, player := range s.onlinePlayers.GetAreaInstancePlayers(
		townPresenceChannelID(store.OwnerSession),
		store.Village, store.Area, store.InstanceKey) {
		if player.Session == nil || player.CharacterID == store.OwnerID {
			continue
		}
		_ = s.sendGameUpperRawClass(player.Session, currentPrivateStoreCreateNotification, createBody, 0)
	}
	return nil
}

func (s *Service) publishCurrentPrivateStoreCompleted(store *currentPrivateStore) error {
	if s == nil || store == nil || store.OwnerSession == nil || s.onlinePlayers == nil {
		return errCurrentPrivateStoreInvalid
	}
	titleBody := buildCurrentPrivateStoreTitleNotification(store)
	// The native CompleteDisplay path broadcasts op81 before returning the
	// class-1 completion ACK. Keep that ordering for the owner and publish the
	// same title/attachment state to area peers.
	if err := s.sendGameUpperRawClass(store.OwnerSession, currentPrivateStoreTitleNotification, titleBody, 0); err != nil {
		return err
	}
	for _, player := range s.onlinePlayers.GetAreaInstancePlayers(
		townPresenceChannelID(store.OwnerSession),
		store.Village, store.Area, store.InstanceKey) {
		if player.Session == nil || player.CharacterID == store.OwnerID {
			continue
		}
		_ = s.sendGameUpperRawClass(player.Session, currentPrivateStoreTitleNotification, titleBody, 0)
	}
	return nil
}

func (s *Service) broadcastCurrentPrivateStoreClose(store *currentPrivateStore, includeOwner bool) {
	if s == nil || store == nil || s.onlinePlayers == nil {
		return
	}
	removeBody := buildCurrentPrivateStoreRemoveNotification(store.OwnerID)
	for _, player := range s.onlinePlayers.GetAreaInstancePlayers(
		townPresenceChannelID(store.OwnerSession),
		store.Village, store.Area, store.InstanceKey) {
		if player.Session == nil {
			continue
		}
		if player.CharacterID == store.OwnerID {
			if includeOwner {
				_ = s.sendGameUpperRawClass(player.Session, currentPrivateStoreForceRemoveNotification, nil, 0)
			}
			continue
		}
		_ = s.sendGameUpperRawClass(player.Session, currentPrivateStoreRemoveNotification, removeBody, 0)
	}
}

func (s *Service) broadcastCurrentPrivateStoreCount(store *currentPrivateStore, displayIndex uint16, soldQuantity uint32) {
	if s == nil || store == nil {
		return
	}
	body := buildCurrentPrivateStoreCountNotification(displayIndex, soldQuantity)
	s.privateStoreMu.Lock()
	visitors := make([]*gameSession, 0)
	for visitor, ownerID := range s.privateStoreVisitors {
		if ownerID == store.OwnerID {
			visitors = append(visitors, visitor)
		}
	}
	s.privateStoreMu.Unlock()
	for _, visitor := range visitors {
		_ = s.sendGameUpperRawClass(visitor, currentPrivateStoreCountNotification, body, 0)
	}
}

func (s *Service) replayCurrentPrivateStores(session *gameSession, townID, areaID byte) {
	if s == nil || session == nil || s.onlinePlayers == nil {
		return
	}
	player, ok := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID)
	if !ok || player.Session != session || player.TownID != townID || player.AreaID != areaID {
		return
	}
	s.privateStoreMu.Lock()
	stores := make([]*currentPrivateStore, 0)
	for _, store := range s.privateStores {
		if store.Active && store.OwnerSession != session && store.Village == townID && store.Area == areaID && store.InstanceKey == player.InstanceKey {
			stores = append(stores, cloneCurrentPrivateStore(store))
		}
	}
	s.privateStoreMu.Unlock()
	if len(stores) == 0 {
		return
	}
	_ = s.sendGameUpperRawClass(session, currentPrivateStoreListNotification, buildCurrentPrivateStoreListNotification(stores), 0)
}
