package dnfbridge

import (
	"encoding/binary"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestParseCurrentCeraShopPurchaseRequestReadsPackageSelectionTail(t *testing.T) {
	body := make([]byte, currentCeraShopRequestHeaderSize+currentCeraShopRequestItemStride+8)
	body[2] = 1
	row := currentCeraShopRequestHeaderSize
	body[row] = currentCeraShopPaymentModeCera
	body[row+1] = 0xff
	binary.LittleEndian.PutUint32(body[row+3:row+7], 101235)
	cursor := row + currentCeraShopRequestItemHeader
	body[cursor] = 0 // avatar choices
	cursor++
	body[cursor] = 1 // package selections
	cursor++
	binary.LittleEndian.PutUint32(body[cursor:cursor+4], 2682779)
	binary.LittleEndian.PutUint16(body[cursor+4:cursor+6], 0)
	binary.LittleEndian.PutUint16(body[cursor+6:cursor+8], 1)
	cursor += 8
	binary.LittleEndian.PutUint32(body[cursor:cursor+4], 0)
	binary.LittleEndian.PutUint16(body[cursor+4:cursor+6], 0)

	request, err := parseCurrentCeraShopPurchaseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Items) != 1 || request.Items[0].CommodityID != 101235 ||
		!reflect.DeepEqual(request.Items[0].PackageSelections, []currentCeraShopPackageSelection{{
			PackageItemID: 2682779,
			GroupIndex:    0,
			ChoiceIndex:   1,
		}}) {
		t.Fatalf("request=%+v", request)
	}

	if _, err := parseCurrentCeraShopPurchaseRequest(body[:len(body)-1]); !errors.Is(err, errCurrentCeraShopRequestMalformed) {
		t.Fatalf("truncated selection error=%v", err)
	}
}

func TestResolveCurrentCeraShopAutoOpenRewardsExpandsFixedAndSelectedRows(t *testing.T) {
	catalog := mustCurrentCeraShopAutoOpenTestCatalog(t)
	source, err := catalog.items.ResolveItem(5000)
	if err != nil {
		t.Fatal(err)
	}
	rewards, autoOpen, err := resolveCurrentCeraShopAutoOpenRewards(
		catalog,
		source,
		2,
		nil,
		[]currentCeraShopPackageSelection{{PackageItemID: 5000, GroupIndex: 0, ChoiceIndex: 1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []currentCeraShopAutoOpenReward{
		{Definition: mustCurrentCeraShopItem(t, catalog, 6001), Count: 4},
		{Definition: mustCurrentCeraShopItem(t, catalog, 6003), Count: 6},
	}
	if !autoOpen || !reflect.DeepEqual(rewards, want) {
		t.Fatalf("auto_open=%t rewards=%+v want=%+v", autoOpen, rewards, want)
	}

	for _, selections := range [][]currentCeraShopPackageSelection{
		nil,
		{{PackageItemID: 5001, GroupIndex: 0, ChoiceIndex: 0}},
		{{PackageItemID: 5000, GroupIndex: 1, ChoiceIndex: 0}},
		{{PackageItemID: 5000, GroupIndex: 0, ChoiceIndex: 2}},
		{
			{PackageItemID: 5000, GroupIndex: 0, ChoiceIndex: 0},
			{PackageItemID: 5000, GroupIndex: 0, ChoiceIndex: 1},
		},
	} {
		if _, autoOpen, err := resolveCurrentCeraShopAutoOpenRewards(catalog, source, 1, nil, selections); !autoOpen || err == nil {
			t.Fatalf("invalid selections=%+v auto_open=%t err=%v", selections, autoOpen, err)
		}
	}
}

func TestRealScriptPVFCeraShopDailyPackagesAutoOpen(t *testing.T) {
	pvfPath := os.Getenv("DNF_CERASHOP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_CERASHOP_REAL_PVF_SMOKE to audit real Cera-shop daily packages")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFCeraShopCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		commodityID uint32
		itemID      uint32
		price       uint32
		selection   []currentCeraShopPackageSelection
		wantUnits   uint64
	}{
		{
			name:        "hope_star",
			commodityID: 101235,
			itemID:      2682779,
			price:       1680,
			selection: []currentCeraShopPackageSelection{{
				PackageItemID: 2682779,
				GroupIndex:    0,
				ChoiceIndex:   0,
			}},
			wantUnits: 327,
		},
		{
			name:        "bright_heart",
			commodityID: 101236,
			itemID:      2682780,
			price:       1960,
			selection: []currentCeraShopPackageSelection{{
				PackageItemID: 2682780,
				GroupIndex:    0,
				ChoiceIndex:   1,
			}},
			wantUnits: 392,
		},
		{
			name:        "glory_light",
			commodityID: 102290,
			itemID:      2683268,
			price:       2860,
			wantUnits:   431,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			product, found := catalog.Product(test.commodityID)
			if !found || product.ItemID != test.itemID || product.Count != 1 || product.CeraPrice != test.price {
				t.Fatalf("product=%+v found=%t", product, found)
			}
			source, resolveErr := catalog.items.ResolveItem(test.itemID)
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			rewards, autoOpen, resolveErr := resolveCurrentCeraShopAutoOpenRewards(
				catalog,
				source,
				product.Count,
				nil,
				test.selection,
			)
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			var units uint64
			for _, reward := range rewards {
				if reward.Definition.ItemID == test.itemID {
					t.Fatalf("source package leaked into auto-open rewards: %+v", reward)
				}
				units += uint64(reward.Count)
			}
			if !autoOpen || units != test.wantUnits {
				t.Fatalf("auto_open=%t units=%d want=%d rewards=%+v", autoOpen, units, test.wantUnits, rewards)
			}
		})
	}
}

func mustCurrentCeraShopAutoOpenTestCatalog(t *testing.T) *pvfCeraShopCatalog {
	t.Helper()
	source := ceraShopAutoOpenTestSource{
		"etc/cerashop.etc": "[item]\n[/item]\n[premium]\n[/premium]\n[creature]\n[/creature]\n" +
			"[coin]\n[/coin]\n[material]\n[/material]\n[recoveryitem]\n[/recoveryitem]\n" +
			"[regular package]\n100001 5000 1 0 100 `test auto open` 0 0 0 0 0\n[/regular package]\n",
		"monster/monster.lst":    "",
		dungeonDropEquipmentList: "",
		dungeonDropStackableList: "5000 `cash/test_auto_open.stk`\n6001 `material/fixed.stk`\n6002 `material/choice_a.stk`\n6003 `material/choice_b.stk`\n",
		"stackable/cash/test_auto_open.stk": `[name]
` + "`Test Auto Open`" + `
[stackable type]
` + "`[cera package]`" + `
[stack limit]
1
[package data]
6001 2
[/package data]
[package data selection]
6002 3 6003 3
[/package data selection]
`,
		"stackable/material/fixed.stk":    "[stackable type]\n`[material]`\n[stack limit]\n999\n",
		"stackable/material/choice_a.stk": "[stackable type]\n`[material]`\n[stack limit]\n999\n",
		"stackable/material/choice_b.stk": "[stackable type]\n`[material]`\n[stack limit]\n999\n",
	}
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

type ceraShopAutoOpenTestSource map[string]string

func (source ceraShopAutoOpenTestSource) ReadText(relativePath string) (string, error) {
	want := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(relativePath, "\\", "/")))
	for itemPath, text := range source {
		if strings.ToLower(strings.TrimSpace(strings.ReplaceAll(itemPath, "\\", "/"))) == want {
			return text, nil
		}
	}
	return "", dnfpvf.ErrDocNotFound
}

func mustCurrentCeraShopItem(t *testing.T, catalog *pvfCeraShopCatalog, itemID uint32) dungeonDropItemDefinition {
	t.Helper()
	definition, err := catalog.items.ResolveItem(itemID)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}
