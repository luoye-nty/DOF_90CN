package dnfbridge

import (
	"context"
	"os"
	"strings"
	"testing"

	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealScriptPVFJob11SelectedSkillRules records the runtime PVF rules used
// by the skill owner for the live female-slayer character. It is opt-in so the
// ordinary test suite does not depend on a 100 MB local archive.
func TestRealScriptPVFJob11SelectedSkillRules(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to run real Script.pvf skill smoke")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := buildSkillCatalogFromSource(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	selected := map[uint16]struct{}{2: {}, 3: {}, 31: {}, 161: {}, 186: {}}
	for _, definition := range catalog.SkillsForJob(11) {
		_, byID := selected[definition.ID]
		byName := strings.Contains(definition.Name, "蛇腹剑")
		if !byID && !byName {
			continue
		}
		t.Logf("id=%d name=%q path=%q kind=%q active=%t required=%d max=%d class=%d grow=%v feature=%d prerequisites=%+v purchase=%v special=%v",
			definition.ID, definition.Name, definition.Path, definition.Kind, definition.Active,
			definition.RequiredLevel, definition.MaximumLevel, definition.SkillClass,
			definition.GrowTypes, definition.FeatureSkillType, definition.Prerequisites,
			definition.PurchaseCost, definition.SpecialPurchaseCost)
	}
}

func TestRealScriptPVFSoulBenderFearAuraActorPrebindContract(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the Soul Bender Fear Aura")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := buildSkillCatalogFromSource(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	fearAura, found := catalog.Find(currentSwordmanJob, uint16(currentSoulBenderFearAuraSkillID))
	if !found {
		t.Fatalf("job %d Fear Aura skill %d missing", currentSwordmanJob, currentSoulBenderFearAuraSkillID)
	}
	if fearAura.Path != "skill/swordman/darkaura.skl" || !strings.Contains(strings.ToLower(fearAura.Kind), "passive") || fearAura.Active ||
		fearAura.RequiredLevel != 48 || fearAura.SkillClass != 3 ||
		len(fearAura.PurchaseCost) == 0 || fearAura.PurchaseCost[0] != 45 ||
		len(fearAura.SecondGrowTypes) == 0 || fearAura.SecondGrowTypes[0] != 1 {
		t.Fatalf("unexpected real Fear Aura definition: %+v", fearAura)
	}
	text, err := archive.ReadText(fearAura.Path)
	if err != nil {
		t.Fatal(err)
	}
	document, err := dnfpvf.Parse(fearAura.Path, text)
	if err != nil {
		t.Fatal(err)
	}
	staticData := document.Numbers("static data")
	levelInfo := document.Numbers("level info")
	if len(staticData) != 1 || staticData[0] != 550 || len(levelInfo) < 3 ||
		!strings.Contains(text, "光环范围") ||
		!strings.Contains(text, "敌人攻击速度减少量") ||
		!strings.Contains(text, "敌人移动速度减少量") {
		t.Fatalf("Fear Aura range/speed data static=%v level_values=%d", staticData, len(levelInfo))
	}
	profiles, err := dnfprofession.LoadProfiles(context.Background(), archive, catalog)
	if err != nil {
		t.Fatal(err)
	}
	rawGrants, ok := profiles.FreeGrants(currentSwordmanJob, currentSoulBenderFirstGrowType, currentSoulBenderFirstAwakeningStage)
	if !ok || !professionGrantsContainSkill(rawGrants, uint16(currentSoulBenderFearAuraSkillID)) {
		t.Fatalf("Soul Bender first-awakening PVF grants=%v supported=%t", rawGrants, ok)
	}
	protected, ok := profiles.FreeGrantsAtLevel(catalog, currentSwordmanJob, 0x12, 90)
	if !ok {
		t.Fatal("Soul Bender first-awakening profile unsupported")
	}
	if professionGrantsContainSkill(protected, uint16(currentSoulBenderFearAuraSkillID)) {
		t.Fatalf("paid Fear Aura was incorrectly promoted into the free/protected baseline: %v", protected)
	}
}

func professionGrantsContainSkill(grants []dnfprofession.Grant, skillID uint16) bool {
	for _, grant := range grants {
		if grant.SkillID == skillID {
			return true
		}
	}
	return false
}

func TestRealScriptPVFActorPassiveDungeonEntryContract(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit actor-passive risk evidence")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := buildSkillCatalogFromSource(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []struct {
		job      byte
		id       uint16
		growType byte
	}{
		{job: 6, id: 225, growType: 0x03},
		{job: 7, id: 228, growType: 0x23},
		{job: 8, id: 141, growType: 0x25},
		{job: 11, id: 31, growType: 0x03},
		{job: 11, id: 234, growType: 0x03},
		{job: 12, id: 120, growType: 0x13},
		{job: 12, id: 149, growType: 0x23},
		{job: 12, id: 153, growType: 0x24},
	} {
		definition, found := catalog.Find(key.job, key.id)
		if !found {
			t.Fatalf("risk skill job=%d id=%d missing", key.job, key.id)
		}
		if !strings.Contains(strings.ToLower(definition.Kind), "passive") ||
			!definition.SupportsCharacterGrowth(int(key.growType)) {
			t.Fatalf("entry passive job=%d id=%d does not support grow=%#x: %+v", key.job, key.id, key.growType, definition)
		}
		text, err := archive.ReadText(definition.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, "进入地下城") {
			t.Fatalf("entry passive job=%d id=%d path=%q no longer declares dungeon-entry initialization", key.job, key.id, definition.Path)
		}
	}
}

func TestRealScriptPVFElvenKnightDisarmingFeatureContract(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the Elven Knight feature skill")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := buildSkillCatalogFromSource(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	disarming, found := catalog.Find(12, 12)
	if !found {
		t.Fatal("job12 Disarming skill 12 missing")
	}
	feature, found := catalog.Find(12, 217)
	if !found {
		t.Fatal("job12 Disarming feature skill 217 missing")
	}
	if disarming.FeatureSkillIndex != int(feature.ID) {
		t.Fatalf("Disarming feature index=%d, want %d", disarming.FeatureSkillIndex, feature.ID)
	}
	if feature.FeatureSkillType != 1 || !feature.IsTPSkill() {
		t.Fatalf("Disarming feature definition is not a TP feature: %+v", feature)
	}
}

func TestRealScriptPVFExternalProfessionBranchZeroSkillContract(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify external-profession skills")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := buildSkillCatalogFromSource(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	quickStanding, found := catalog.Find(10, 190)
	if !found || !quickStanding.SupportsCharacterGrowth(0) {
		t.Fatalf("Creator Quick Standing is not learnable on branch zero: found=%t definition=%+v", found, quickStanding)
	}
	darkKnightTP := 0
	for _, definition := range catalog.SkillsForJob(9) {
		if !definition.IsTPSkill() || len(definition.GrowTypeMaximumLevels) == 0 || definition.GrowTypeMaximumLevels[0] <= 0 {
			continue
		}
		darkKnightTP++
		if !definition.SupportsCharacterGrowth(0) {
			t.Fatalf("Dark Knight TP skill %d is not learnable on branch zero: %+v", definition.ID, definition)
		}
	}
	if darkKnightTP == 0 {
		t.Fatal("real PVF exposes no branch-zero Dark Knight TP skills")
	}
}

// TestRealScriptPVFUntypedExecutableSkillsStayQuickSlotEligible protects the
// current PVF's high skills that omit [type] despite declaring normal
// executable states. They must not be treated as passive by the skill-slot
// transaction.
func TestRealScriptPVFUntypedExecutableSkillsStayQuickSlotEligible(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to run real Script.pvf skill smoke")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := buildSkillCatalogFromSource(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		job  byte
		id   uint16
		name string
	}{
		{job: 4, id: 253, name: "毁灭强击"},
		{job: 8, id: 138, name: "游龙惊风破"},
		{job: 8, id: 139, name: "九霄风雷"},
	} {
		definition, found := catalog.Find(want.job, want.id)
		if !found || definition.Name != want.name || !definition.Active {
			t.Fatalf("untyped executable job=%d skill=%d found=%t definition=%+v", want.job, want.id, found, definition)
		}
	}
}
