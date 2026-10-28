package dnfbridge

import (
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const currentCloneAvatarCategory = "clear avatar"

// currentAuroraAvatarPVFType is the PVF `[equipment type]` of an aura. A
// 转职光环 is an aura that is ALSO a `clear avatar` item, which is why it used
// to be classified as a plain clone avatar and handed the covered aura's model.
const currentAuroraAvatarPVFType = "[aurora avatar]"

// currentClassifyAvatarItem derives both avatar identity and clone identity
// from the active runtime PVF. Names and fixed item-ID tables are deliberately
// not accepted as authority.
func (s *Service) currentClassifyAvatarItem(itemID uint32) (bool, bool) {
	if s == nil || itemID == 0 {
		return false, false
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return false, false
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil || definition.Kind != dungeonDropItemEquipment {
		return false, false
	}
	doc, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
	if err != nil {
		return false, false
	}
	equipmentType, ok := doc.Text("equipment type")
	if !ok {
		return false, false
	}
	rule, ok := currentEquipmentPlacementRuleForPVFType(equipmentType)
	if !ok || rule.class != currentEquipmentPlacementClassAvatar {
		return false, false
	}
	return true, currentPVFDocumentHasText(doc, "item category", currentCloneAvatarCategory)
}

// currentIsAuroraLookReplaceAvatar reports whether the item is a 转职光环: an
// `[aurora avatar]` that also carries the `clear avatar` item category.
//
// Such an item replaces the worn aura's LOOK while the covered aura keeps
// supplying stats, so it must never borrow the covered aura's appearance the
// way a 透明克隆时装 does. The predicate is deliberately narrower than the A21
// reference's (`[aurora avatar]` + (`clear avatar` OR `[aurora virtual motion]`)):
// scanning the live Script.pvf, the two sets are identical -- all 3 documents
// with `[aurora virtual motion]` (113590006/113590008/113590009) are also
// `clear avatar`, and no other `clear avatar` document is an aura. Keeping the
// narrower form means an unrecognised item falls back to today's behaviour
// instead of silently changing appearance.
func (s *Service) currentIsAuroraLookReplaceAvatar(itemID uint32) bool {
	if s == nil || itemID == 0 {
		return false
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return false
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil || definition.Kind != dungeonDropItemEquipment {
		return false
	}
	doc, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
	if err != nil {
		return false
	}
	equipmentType, ok := doc.Text("equipment type")
	if !ok {
		return false
	}
	rule, ok := currentEquipmentPlacementRuleForPVFType(equipmentType)
	if !ok || rule.pvfType != currentAuroraAvatarPVFType {
		return false
	}
	return currentPVFDocumentHasText(doc, "item category", currentCloneAvatarCategory)
}

func currentPVFDocumentHasText(doc *dnfpvf.Document, sectionName, want string) bool {
	if doc == nil {
		return false
	}
	want = normalizeCloneAvatarPVFText(want)
	for _, section := range doc.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), strings.TrimSpace(sectionName)) ||
			section.Start < 0 || section.End > len(doc.Tokens) || section.Start > section.End {
			continue
		}
		for _, token := range doc.Tokens[section.Start:section.End] {
			if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
				continue
			}
			if normalizeCloneAvatarPVFText(token.Value) == want {
				return true
			}
		}
	}
	return false
}

func normalizeCloneAvatarPVFText(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(value), "`'\"")))
}
