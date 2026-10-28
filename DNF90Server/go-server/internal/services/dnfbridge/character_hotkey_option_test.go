package dnfbridge

import (
	"bytes"
	"context"
	"os"
	"testing"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestParseCurrentPVFHotkeyVer2KeepsIdentifiersAndPhysicalKeys(t *testing.T) {
	text := "[key] `move up` `I01` `system` `creator` 87\n" +
		"[key] `move left` `I02` `system` `creator` 65\n" +
		"[key] `change skill bar` `K00` `skill` `creator` 67\n"
	entries, err := parseCurrentPVFHotkeyVer2Entries(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].identifier != "I01" || entries[0].externalKey != 87 ||
		entries[1].identifier != "I02" || entries[1].externalKey != 65 ||
		entries[2].identifier != "K00" || entries[2].externalKey != 67 {
		t.Fatalf("creator ver2 entries=%+v", entries)
	}
}

func TestBuildCurrentCreatorHotkeyProfileUsesWASDAndDigitSkills(t *testing.T) {
	normal := []currentPVFHotkeyEntry{
		{identifier: "I01", externalKey: 38},
		{identifier: "I02", externalKey: 37},
		{identifier: "I03", externalKey: 40},
		{identifier: "I04", externalKey: 39},
		{identifier: "I05", externalKey: 32},
		{identifier: "I28", externalKey: 87},
		{identifier: "I06", externalKey: 67},
		{identifier: "I21", externalKey: 65},
		{identifier: "I22", externalKey: 83},
		{identifier: "I23", externalKey: 68},
		{identifier: "I33", externalKey: 49},
		{identifier: "I34", externalKey: 50},
		{identifier: "I35", externalKey: 51},
		{identifier: "I36", externalKey: 52},
		{identifier: "I37", externalKey: 53},
		{identifier: "K02", externalKey: 9},
	}
	creator := []currentPVFHotkeyEntry{
		{identifier: "I01", externalKey: 87},
		{identifier: "I02", externalKey: 65},
		{identifier: "I03", externalKey: 83},
		{identifier: "I04", externalKey: 68},
		{identifier: "I05", externalKey: 32},
		{identifier: "I21", externalKey: 81},
		{identifier: "I22", externalKey: 69},
		{identifier: "I23", externalKey: 82},
		{identifier: "I24", externalKey: 70},
		{identifier: "I25", externalKey: 71},
		{identifier: "I33", externalKey: 49},
		{identifier: "I34", externalKey: 50},
		{identifier: "I35", externalKey: 51},
		{identifier: "I36", externalKey: 52},
		{identifier: "I37", externalKey: 53},
		{identifier: "K02", externalKey: 9},
	}
	profile, err := buildCurrentCreatorHotkeyProfile(normal, creator, csharpDefaultHotkeySlots)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint16{
		"I01": 77,
		"I02": 55,
		"I03": 73,
		"I04": 58,
		"I05": 78,
		"I21": 46,
		"I22": 47,
		"I23": 48,
		"I24": 49,
		"I25": 50,
	}
	for identifier, wantKey := range want {
		got, ok := currentDefaultInternalHotkey(profile.slots, identifier)
		if !ok || got != wantKey {
			t.Fatalf("creator %s=%d ok=%t, want %d", identifier, got, ok, wantKey)
		}
	}
	for _, row := range [][]byte{
		[]byte("I01\t77\n"), []byte("I02\t55\n"), []byte("I03\t73\n"), []byte("I04\t58\n"), []byte("I05\t78\n"),
		[]byte("I21\t46\n"), []byte("I22\t47\n"), []byte("I23\t48\n"), []byte("I24\t49\n"), []byte("I25\t50\n"),
	} {
		if !bytes.Contains(profile.keymap, row) {
			t.Fatalf("creator keymap missing %q in %q", row, profile.keymap)
		}
	}
}

func TestCurrentCharacterHotkeySlotsReadsCreatorRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNFBRIDGE_REAL_PVF_SMOKE is not set")
	}
	service := &Service{options: options{pvfPath: pvfPath}}
	slots, keyType, source, err := service.currentCharacterHotkeySlots(context.Background(), csharpCreatorMageJob)
	if err != nil {
		t.Fatal(err)
	}
	if keyType != 1 || source != "pvf_clientonly_hotkeysystemforcreator_ver2_indexed" || bytes.Equal(slots, csharpDefaultHotkeySlots) {
		t.Fatalf("creator hotkeys type=%d source=%q slots=%d", keyType, source, len(slots)/2)
	}
	want := map[string]uint16{
		"I01": 77,
		"I02": 55,
		"I03": 73,
		"I04": 58,
		"I05": 78,
		"I21": 46,
		"I22": 47,
		"I23": 48,
		"I24": 49,
		"I25": 50,
	}
	for identifier, wantKey := range want {
		got, ok := currentDefaultInternalHotkey(slots, identifier)
		if !ok || got != wantKey {
			t.Fatalf("creator real PVF %s=%d ok=%t, want %d", identifier, got, ok, wantKey)
		}
	}
}

// 缔造者的 keymap 分两种情况，两者都必须成立：
//  1. 玩家在游戏里改过技能快捷键（存进角色作用域的是一份**完整的缔造者表**）
//     → 登录必须下发**玩家改过的那份**。活体 2026-09-15：库里
//     `character:13:game_option_keymap` = I21=30/I22=25，而登录时被 PVF 默认表
//     （I21..I25 = 46..50）覆盖，玩家看到"小退重上恢复原样"。
//  2. 教学期回显进来的临时普通 ASDFG 表（缺 I21..I25 这些缔造者行）
//     → 继续下发 PVF 表，绝不能让那份快照变成权威。
func TestCurrentGameOptionKeymapForCreatorHonoursAStoredCreatorRebind(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNFBRIDGE_REAL_PVF_SMOKE is not set")
	}
	ctx := context.Background()
	// 玩家改过的绑定：I21 指到内部键 30、I22 指到 25（就是库里那份）。
	// I01..I05 必须保持缔造者那组（77/55/73/58/78）—— 玩家改技能键不会动它们。
	playerRebind := []byte("I01\t77\nI02\t55\nI03\t73\nI04\t58\nI05\t78\n" +
		"I21\t30\nI22\t25\nI23\t48\nI24\t49\nI25\t50\n")

	t.Run("player rebind wins over the pvf default", func(t *testing.T) {
		repositories := testRepositoryGroup()
		if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
			CharacterID: "13",
			AccountID:   "account-a",
			Job:         "10",
		}); err != nil {
			t.Fatal(err)
		}
		if err := saveCurrentGameOptionKeymapToScope(
			ctx,
			repositories.Settings,
			currentCharacterGameOptionKeymapScope("13"),
			playerRebind,
		); err != nil {
			t.Fatal(err)
		}
		service := &Service{options: options{pvfPath: pvfPath, accountID: "account-a"}}
		session := &gameSession{accountID: "account-a", selectedCharacterID: 13}
		keymap, source, err := service.currentGameOptionKeymapForSelectedCharacter(
			ctx, repositories, session, []byte("I01\t2\nI21\t40\n"),
		)
		if err != nil {
			t.Fatal(err)
		}
		if source != "character_scoped_creator_type198_keymap" {
			t.Fatalf("creator keymap source=%q want the stored character-scoped rebind", source)
		}
		if !bytes.Equal(keymap, playerRebind) {
			t.Fatalf("creator keymap=%q want the player's own rebind %q", keymap, playerRebind)
		}
	})

	t.Run("tutorial echo still falls back to the pvf table", func(t *testing.T) {
		repositories := testRepositoryGroup()
		if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
			CharacterID: "13",
			AccountID:   "account-a",
			Job:         "10",
		}); err != nil {
			t.Fatal(err)
		}
		// 直进教学副本的缔造者会把这个临时普通表回显进 op198：没有 I21..I25。
		if err := saveCurrentGameOptionKeymapToScope(
			ctx,
			repositories.Settings,
			currentCharacterGameOptionKeymapScope("13"),
			[]byte("I01\t2\nI02\t0\n"),
		); err != nil {
			t.Fatal(err)
		}
		service := &Service{options: options{pvfPath: pvfPath, accountID: "account-a"}}
		session := &gameSession{accountID: "account-a", selectedCharacterID: 13}
		keymap, source, err := service.currentGameOptionKeymapForSelectedCharacter(
			ctx, repositories, session, []byte("I01\t2\nI21\t40\n"),
		)
		if err != nil {
			t.Fatal(err)
		}
		if source != "pvf_creator_ver2_wasd_space_digit_skill_keymap_locked" {
			t.Fatalf("creator keymap source=%q want the pvf profile for a tutorial echo", source)
		}
		for _, row := range [][]byte{
			[]byte("I21\t46\n"), []byte("I22\t47\n"), []byte("I23\t48\n"), []byte("I24\t49\n"), []byte("I25\t50\n"),
		} {
			if !bytes.Contains(keymap, row) {
				t.Fatalf("creator keymap missing %q in %q", row, keymap)
			}
		}
	})
}

// validStoredCreatorKeymap 只认形状：必须同时有 I01（普攻）和 I21..I25（五个技能选择键）。
// 这份判定决定了"玩家自己的绑定"和"教学期临时表"谁有权威地位，所以逐条钉住。
//
// 注意：光有形状还不够 —— 线上那张客户端"恢复默认"的普通表形状也是完整的
// （I01..I25 全都有），所以还要再过 keymapHasCreatorInputModel（I01..I05 是不是
// 缔造者那组 77/55/73/58/78）。本用例只钉形状这一层。
func TestValidStoredCreatorKeymapOnlyAcceptsACompleteCreatorTable(t *testing.T) {
	cases := []struct {
		name   string
		keymap string
		want   bool
	}{
		{"player rebind", "I01\t77\nI21\t30\nI22\t25\nI23\t48\nI24\t49\nI25\t50\n", true},
		{"full table with extras", "I01\t77\nI02\t55\nI21\t46\nI22\t47\nI23\t48\nI24\t49\nI25\t50\nK02\t31\n", true},
		{"crlf rows", "I01\t77\r\nI21\t30\r\nI22\t25\r\nI23\t48\r\nI24\t49\r\nI25\t50\r\n", true},
		{"tutorial echo (no skill selectors)", "I01\t2\nI02\t0\n", false},
		{"missing attack row", "I21\t30\nI22\t25\nI23\t48\nI24\t49\nI25\t50\n", false},
		{"missing one selector", "I01\t77\nI21\t30\nI22\t25\nI23\t48\nI24\t49\n", false},
		{"empty", "", false},
		{"whitespace only", "\n\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validStoredCreatorKeymap([]byte(tc.keymap)); got != tc.want {
				t.Fatalf("validStoredCreatorKeymap = %t want %t for %q", got, tc.want, tc.keymap)
			}
		})
	}
}

// 玩家在游戏里点"恢复默认按键"时，客户端会把**普通角色的**键位表原样 op198 发上来。
// 对缔造者那份表是错的输入模型（技能键落到普通角色的位置上），既不能存、也不能当成
// "玩家自己的绑定"在下次登录发下去。
//
// 判据是"与账号作用域的普通表逐行相同"。下面这段正文就是线上库里真实存下来的那张表
// （2026-09-15，角色 13 的角色作用域 = 该账号的账号作用域表，客户端只发了前 150 字节）。
const currentCreatorOrdinaryDefaultKeymapFixture = "I01\t2\nI02\t0\nI03\t3\nI04\t1\nI05\t78\n" +
	"I06\t57\nI07\t80\nI08\t13\nI09\t76\nI10\t67\nI11\t65\nI12\t63\nI13\t69\nI14\t82\nI15\t75\n" +
	"I16\t85\nI17\t68\nI18\t86\nI19\t84\nI20\t81\n" +
	"I21\t55\nI22\t73\n"

// 同一账号里的普通表更长（后面还有 I23..I25 等行）：截断副本必须照样被认出来。
const currentOrdinaryAccountKeymapFixture = currentCreatorOrdinaryDefaultKeymapFixture +
	"I23\t58\nI24\t60\nI25\t61\nI26\t62\nI27\t71\n"

// 判据分两层：①"客户端默认指纹"（移动组 2/0/3/1 + 跳跃 57）；②与账号作用域表逐行相同。
// 老数据里存下来的那张 150 字节截断副本满足 ②，线上完整那张两层都满足。
func TestCurrentCreatorSaveIsOrdinaryKeymapDefaultRecognisesTheResetButton(t *testing.T) {
	playerRebind := "I01\t77\nI02\t55\nI03\t73\nI04\t58\nI05\t78\n" +
		"I21\t30\nI22\t25\nI23\t48\nI24\t49\nI25\t50\n"
	cases := []struct {
		name    string
		keymap  string
		account string
		want    bool
	}{
		{
			name:    "client restore-default (real live row, truncated copy)",
			keymap:  currentCreatorOrdinaryDefaultKeymapFixture,
			account: currentOrdinaryAccountKeymapFixture,
			want:    true,
		},
		{
			name:    "same table without truncation",
			keymap:  currentOrdinaryAccountKeymapFixture,
			account: currentOrdinaryAccountKeymapFixture,
			want:    true,
		},
		{
			name:    "player rebind is neither the fingerprint nor the account table",
			keymap:  playerRebind,
			account: currentOrdinaryAccountKeymapFixture,
			want:    false,
		},
		{
			name:    "intentionally unbound is not a default request",
			keymap:  "I01\t134\nI02\t134\nI03\t134\nI04\t134\nI06\t134\n",
			account: currentOrdinaryAccountKeymapFixture,
			want:    false,
		},
		{
			name:    "empty save",
			keymap:  "",
			account: currentOrdinaryAccountKeymapFixture,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := currentCreatorSaveIsClientDefaultTable([]byte(tc.keymap)) ||
				(len(tc.account) != 0 && keymapBodyMatchesPrefix([]byte(tc.keymap), []byte(tc.account)))
			if got != tc.want {
				t.Fatalf("default-request detection = %t want %t", got, tc.want)
			}
		})
	}
}

// 真正决定"这份表能不能给缔造者用"的判据：I01..I05 是不是缔造者那组
// （77/55/73/58/78）。玩家改技能键只动 I21..I25，依然满足；客户端"恢复默认"给的普通表
// （2/0/3/1/78）怎么都不满足。用线上真实两张表钉住。
func TestKeymapHasCreatorInputModelUsesTheAttackRowsAsFingerprint(t *testing.T) {
	// 职业默认表（PVF 缔造者表）里的 I01..I05 就是那五个值。
	profile := []byte("I01\t77\nI02\t55\nI03\t73\nI04\t58\nI05\t78\n" +
		"I21\t46\nI22\t47\nI23\t48\nI24\t49\nI25\t50\n")
	cases := []struct {
		name   string
		keymap string
		want   bool
	}{
		{
			name:   "pvf creator table",
			keymap: string(profile),
			want:   true,
		},
		{
			name: "player rebind keeps the creator attack rows",
			keymap: "I01\t77\nI02\t55\nI03\t73\nI04\t58\nI05\t78\n" +
				"I21\t30\nI22\t25\nI23\t48\nI24\t49\nI25\t50\n",
			want: true,
		},
		{
			name:   "live client restore-default table (94 rows, real)",
			keymap: currentCreatorOrdinaryDefaultKeymapFixture + "I23\t58\nI24\t60\nI25\t61\nI26\t62\nI27\t71\n",
			want:   false,
		},
		{
			name:   "truncated ordinary table",
			keymap: currentCreatorOrdinaryDefaultKeymapFixture,
			want:   false,
		},
		{
			name:   "tutorial echo without the creator rows",
			keymap: "I01\t2\nI02\t0\n",
			want:   false,
		},
		{
			name:   "empty",
			keymap: "",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keymapHasCreatorInputModel([]byte(tc.keymap), profile); got != tc.want {
				t.Fatalf("creator input model = %t want %t", got, tc.want)
			}
		})
	}
}

// 缔造者点"恢复默认按键"：客户端本地已经换成普通角色的表了，服务端必须
//
//  1. 丢掉角色作用域里存的自定义绑定（"恢复默认"就该把改动一并丢掉），
//  2. 把**这个职业自己的** PVF 默认表沿 op454 推回客户端。
//
// 只拒绝保存是不够的 —— 那样界面仍是错的（现场 2026-09-15：点完没反应）。
func TestCreatorRestoreDefaultPushesThePVFTableAndDropsTheStoredBinding(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNFBRIDGE_REAL_PVF_SMOKE is not set")
	}
	ctx := context.Background()
	repositories := testRepositoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "13",
		AccountID:   "account-a",
		Job:         "10",
	}); err != nil {
		t.Fatal(err)
	}
	// 账号作用域里放一份普通角色的键位表：这就是客户端"恢复默认"会照抄的那张。
	if err := saveCurrentGameOptionKeymap(
		ctx, repositories.Settings, "account-a", []byte(currentOrdinaryAccountKeymapFixture),
	); err != nil {
		t.Fatal(err)
	}
	playerRebind := []byte("I01\t77\nI02\t55\nI03\t73\nI04\t58\nI05\t78\n" +
		"I21\t30\nI22\t25\nI23\t48\nI24\t49\nI25\t50\n")
	if err := saveCurrentGameOptionKeymapToScope(
		ctx, repositories.Settings, currentCharacterGameOptionKeymapScope("13"), playerRebind,
	); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			pvfPath:            pvfPath,
			accountID:          "account-a",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		accountID:           "account-a",
		selectedCharacterID: 13,
	}
	if err := service.handleCurrentSaveGameOptionKeymap(
		session,
		buildCurrentGameOptionKeymapSaveBody([]byte(currentCreatorOrdinaryDefaultKeymapFixture)),
	); err != nil {
		t.Fatal(err)
	}
	// (1) 客户端必须收到一条 op454，正文是 PVF 的缔造者表（回推），不是那张普通表。
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != currentGameOptionKeymapMsgID || packet.Header.Classification != 0 {
		t.Fatalf("restore-default packet=%+v rest=%x", packet.Header, rest)
	}
	// op454 的正文 = u8 mode + u32 长度前缀的 keymap。
	if len(packet.Body) < 5 || packet.Body[0] != currentGameOptionKeymapMode {
		t.Fatalf("restore-default body head=%x", packet.Body[:min(8, len(packet.Body))])
	}
	pushed := packet.Body[1:]
	for _, row := range [][]byte{[]byte("I01\t77\n"), []byte("I21\t46\n"), []byte("I22\t47\n"), []byte("I25\t50\n")} {
		if !bytes.Contains(pushed, row) {
			t.Fatalf("pushed creator table missing %q", row)
		}
	}
	if bytes.Contains(pushed, []byte("I21\t55\n")) {
		t.Fatalf("the ordinary default table was pushed back to the client: %q", pushed)
	}
	// (2) 存的自定义绑定必须被丢掉。
	if stored, found, err := loadCurrentGameOptionKeymapFromScope(
		ctx, repositories.Settings, currentCharacterGameOptionKeymapScope("13")); err != nil || found {
		t.Fatalf("stored binding survived restore-default: found=%t err=%v body=%q", found, err, stored)
	}
	// (3) 之后登录回到 PVF 默认表。
	keymap, source, err := service.currentGameOptionKeymapForSelectedCharacter(
		ctx, repositories, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source != "pvf_creator_ver2_wasd_space_digit_skill_keymap_locked" {
		t.Fatalf("post-reset source=%q want the pvf creator profile", source)
	}
	if bytes.Contains(keymap, []byte("I21\t30\n")) {
		t.Fatalf("the dropped rebind was restored: %q", keymap)
	}
	// (4) 非缔造者不受这条路径影响：账号作用域照旧能存。
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "14",
		AccountID:   "account-a",
		Job:         "0",
	}); err != nil {
		t.Fatal(err)
	}
	connection2 := &bufferConn{}
	ordinarySession := &gameSession{conn: connection2, accountID: "account-a", selectedCharacterID: 14}
	if err := service.handleCurrentSaveGameOptionKeymap(
		ordinarySession,
		buildCurrentGameOptionKeymapSaveBody([]byte(currentCreatorOrdinaryDefaultKeymapFixture)),
	); err != nil {
		t.Fatal(err)
	}
	if connection2.write.Len() != 0 {
		t.Fatalf("ordinary character received a restore packet: %x", connection2.write.Bytes())
	}
	accountStored, accountFound, err := loadCurrentGameOptionKeymap(ctx, repositories.Settings, "account-a")
	if err != nil || !accountFound || !bytes.Equal(accountStored, []byte(currentCreatorOrdinaryDefaultKeymapFixture)) {
		t.Fatalf("ordinary character save was blocked: found=%t err=%v body=%q", accountFound, err, accountStored)
	}
}

// 库里已经躺着的那张普通默认表（保存侧拦截上线之前存进去的）必须自愈：
// 它虽然形状完整，但内容是错的，登录时要落到 PVF 表而不是发这张表。
func TestStoredOrdinaryDefaultKeymapDoesNotBecomeCreatorAuthoritative(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNFBRIDGE_REAL_PVF_SMOKE is not set")
	}
	ctx := context.Background()
	repositories := testRepositoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "13",
		AccountID:   "account-a",
		Job:         "10",
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveCurrentGameOptionKeymap(
		ctx, repositories.Settings, "account-a", []byte(currentOrdinaryAccountKeymapFixture),
	); err != nil {
		t.Fatal(err)
	}
	if err := saveCurrentGameOptionKeymapToScope(
		ctx, repositories.Settings, currentCharacterGameOptionKeymapScope("13"),
		[]byte(currentCreatorOrdinaryDefaultKeymapFixture),
	); err != nil {
		t.Fatal(err)
	}
	service := &Service{options: options{pvfPath: pvfPath, accountID: "account-a"}}
	session := &gameSession{accountID: "account-a", selectedCharacterID: 13}
	keymap, source, err := service.currentGameOptionKeymapForSelectedCharacter(
		ctx, repositories, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source != "pvf_creator_ver2_wasd_space_digit_skill_keymap_locked" {
		t.Fatalf("source=%q want the pvf creator profile for a client-default store", source)
	}
	if bytes.Contains(keymap, []byte("I21\t55\n")) {
		t.Fatalf("the ordinary default keymap was served to a Creator: %q", keymap)
	}
	for _, row := range [][]byte{[]byte("I21\t46\n"), []byte("I22\t47\n"), []byte("I25\t50\n")} {
		if !bytes.Contains(keymap, row) {
			t.Fatalf("creator keymap missing %q in %q", row, keymap)
		}
	}
}

// 副本边界不能冲掉玩家改过的绑定。
//
// 现场 2026-09-15：缔造者改好技能键后**一进副本就变回默认** —— 因为副本每次重建 HUD 都会
// 走 sendCurrentCreatorKeymapInitialization（日志 source=...after_final_scene_skill_manager），
// 而它原来无条件下发 PVF 职业默认表。职业默认本身是对的（新建缔造者按键就靠它），
// 错的是"无论玩家改没改都覆盖"。这里钉住：存过绑定就推绑定，没存过才推职业默认。
func TestCreatorDungeonBoundaryKeepsThePlayersRebind(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNFBRIDGE_REAL_PVF_SMOKE is not set")
	}
	ctx := context.Background()
	repositories := testRepositoryGroup()
	character := dnfrepo.CharacterRecord{CharacterID: "15", AccountID: "account-a", Job: "10"}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	playerRebind := []byte("I01\t77\nI02\t55\nI03\t73\nI04\t58\nI05\t78\n" +
		"I06\t21\nI21\t30\nI22\t25\nI23\t48\nI24\t49\nI25\t50\n")

	t.Run("stored rebind wins", func(t *testing.T) {
		if err := saveCurrentGameOptionKeymapToScope(
			ctx, repositories.Settings, currentCharacterGameOptionKeymapScope("15"), playerRebind,
		); err != nil {
			t.Fatal(err)
		}
		connection := &bufferConn{}
		service := &Service{
			options: options{
				pvfPath:            pvfPath,
				accountID:          "account-a",
				gameUpperHeader:    gameUpperHeaderServer16,
				gameUpperBodyCodec: gameUpperBodyCodecPlain,
			},
			repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		}
		session := &gameSession{conn: connection, accountID: "account-a", selectedCharacterID: 15}
		if err := service.sendCurrentCreatorKeymapInitialization(session, ctx, character, "unit_test_dungeon"); err != nil {
			t.Fatal(err)
		}
		packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
		if len(rest) != 0 || packet.Header.MsgID != currentGameOptionKeymapMsgID {
			t.Fatalf("dungeon boundary packet=%+v rest=%x", packet.Header, rest)
		}
		pushed := packet.Body[1:]
		for _, row := range [][]byte{[]byte("I21\t30\n"), []byte("I22\t25\n"), []byte("I01\t77\n")} {
			if !bytes.Contains(pushed, row) {
				t.Fatalf("副本边界没有保留玩家绑定，缺 %q", row)
			}
		}
		if bytes.Contains(pushed, []byte("I21\t46\n")) {
			t.Fatalf("副本边界把玩家绑定推回了职业默认: %q", pushed)
		}
	})

	t.Run("no stored rebind falls back to the pvf default", func(t *testing.T) {
		if err := clearCurrentSpecialJobKeymapStore(ctx, repositories.Settings, "15"); err != nil {
			t.Fatal(err)
		}
		connection := &bufferConn{}
		service := &Service{
			options: options{
				pvfPath:            pvfPath,
				accountID:          "account-a",
				gameUpperHeader:    gameUpperHeaderServer16,
				gameUpperBodyCodec: gameUpperBodyCodecPlain,
			},
			repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		}
		session := &gameSession{conn: connection, accountID: "account-a", selectedCharacterID: 15}
		if err := service.sendCurrentCreatorKeymapInitialization(session, ctx, character, "unit_test_dungeon_fresh"); err != nil {
			t.Fatal(err)
		}
		packet, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
		pushed := packet.Body[1:]
		for _, row := range [][]byte{[]byte("I21\t46\n"), []byte("I22\t47\n"), []byte("I01\t77\n")} {
			if !bytes.Contains(pushed, row) {
				t.Fatalf("没存过绑定时应推职业默认，缺 %q", row)
			}
		}
	})
}

// "恢复默认"的判定必须认客户端默认指纹，而不是"只要不像缔造者表就算"：
// 玩家手动把所有键清空（整表 134）是合法状态，不能被当成恢复默认而回滚。
func TestCreatorDefaultDetectionIgnoresAnIntentionallyEmptyTable(t *testing.T) {
	clientDefault := "I01\t2\nI02\t0\nI03\t3\nI04\t1\nI05\t78\nI06\t57\n" +
		"I21\t55\nI22\t73\nI23\t58\nI24\t60\nI25\t61\n"
	unbound := "I01\t134\nI02\t134\nI03\t134\nI04\t134\nI05\t134\nI06\t134\n" +
		"I21\t134\nI22\t134\nI23\t134\nI24\t134\nI25\t134\n"
	playerRebind := "I01\t77\nI02\t55\nI03\t73\nI04\t58\nI05\t78\nI06\t21\n" +
		"I21\t30\nI22\t25\nI23\t48\nI24\t49\nI25\t50\n"
	cases := []struct {
		name   string
		keymap string
		want   bool
	}{
		{"client default table", clientDefault, true},
		{"player rebind", playerRebind, false},
		{"intentionally unbound everywhere", unbound, false},
		{"only movement matches", "I01\t2\nI02\t0\nI03\t3\nI04\t1\nI06\t21\n", false},
		{"movement matches but jump differs", "I01\t2\nI02\t0\nI03\t3\nI04\t1\nI06\t21\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := currentCreatorSaveIsClientDefaultTable([]byte(tc.keymap)); got != tc.want {
				t.Fatalf("client-default detection = %t want %t", got, tc.want)
			}
		})
	}
}

func TestCurrentGameOptionKeymapForCreatorUsesPVFInsteadOfAccountDefault(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNFBRIDGE_REAL_PVF_SMOKE is not set")
	}
	ctx := context.Background()
	repositories := testRepositoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "13",
		AccountID:   "account-a",
		Job:         "10",
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{options: options{pvfPath: pvfPath, accountID: "account-a"}}
	session := &gameSession{accountID: "account-a", selectedCharacterID: 13}
	// A direct-to-tutorial Creator can save the temporary ordinary HUD table
	// before its class profile arrives. The stale character-scoped row must not
	// override the locked Creator controls on the next restore.
	// 缔造者**没有**存过自己的角色作用域表时，必须落到 PVF 表，而不是账号默认值。
	// （教学期回显进来的不完整表也算"没有" —— 见上面那个用例的第二个子用例。）
	keymap, source, err := service.currentGameOptionKeymapForSelectedCharacter(
		ctx,
		repositories,
		session,
		[]byte("I01\t2\nI21\t40\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if source != "pvf_creator_ver2_wasd_space_digit_skill_keymap_locked" {
		t.Fatalf("creator keymap source=%q", source)
	}
	for _, row := range [][]byte{
		[]byte("I01\t77\n"), []byte("I02\t55\n"), []byte("I03\t73\n"), []byte("I04\t58\n"), []byte("I05\t78\n"),
		[]byte("I21\t46\n"), []byte("I22\t47\n"), []byte("I23\t48\n"), []byte("I24\t49\n"), []byte("I25\t50\n"),
	} {
		if !bytes.Contains(keymap, row) {
			t.Fatalf("creator keymap missing %q in %q", row, keymap)
		}
	}
	if bytes.Contains(keymap, []byte("I01\t2\n")) {
		t.Fatalf("creator keymap was overwritten by account default: %q", keymap)
	}
}

func TestSendCurrentCreatorKeymapInitializationAfterDungeonHUDReady(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNFBRIDGE_REAL_PVF_SMOKE is not set")
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			pvfPath:            pvfPath,
			accountID:          "account-a",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
	}
	session := &gameSession{
		conn:                connection,
		accountID:           "account-a",
		selectedCharacterID: 13,
	}
	character := dnfrepo.CharacterRecord{CharacterID: "13", AccountID: "account-a", Job: "10"}
	if err := service.sendCurrentCreatorKeymapInitialization(session, context.Background(), character, "test_tutorial_dungeon_hud_ready"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacketWithHeader(t, connection.write.Bytes(), dnfproto.GameServerUpperHeaderSize16)
	if len(rest) != 0 || packet.Header.Classification != 0 || packet.Header.MsgID != currentGameOptionKeymapMsgID {
		t.Fatalf("creator dungeon keymap packet=%+v rest=%x", packet.Header, rest)
	}
	profile, err := service.currentCreatorHotkeyProfile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, err := buildCurrentGameOptionKeymapTransportBody(profile.keymap)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Body, want) || !bytes.Equal(session.currentGameOptionNativeApplyKeymapBody, profile.keymap) {
		t.Fatalf("creator dungeon keymap body_len=%d want=%d snapshot_len=%d", len(packet.Body), len(want), len(session.currentGameOptionNativeApplyKeymapBody))
	}
	for _, row := range [][]byte{
		[]byte("I01\t77\n"), []byte("I02\t55\n"), []byte("I03\t73\n"), []byte("I04\t58\n"), []byte("I05\t78\n"),
		[]byte("I21\t46\n"), []byte("I22\t47\n"), []byte("I23\t48\n"), []byte("I24\t49\n"), []byte("I25\t50\n"),
	} {
		if !bytes.Contains(packet.Body, row) {
			t.Fatalf("creator dungeon keymap missing %q", row)
		}
	}
}

func TestSendCurrentCreatorKeymapInitializationLeavesOtherJobsUntouched(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{options: options{
		gameUpperHeader:    gameUpperHeaderServer16,
		gameUpperBodyCodec: gameUpperBodyCodecPlain,
	}}
	session := &gameSession{conn: connection, selectedCharacterID: 13}
	character := dnfrepo.CharacterRecord{CharacterID: "13", Job: "9"}
	if err := service.sendCurrentCreatorKeymapInitialization(session, context.Background(), character, "test_non_creator"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 || len(session.currentGameOptionNativeApplyKeymapBody) != 0 {
		t.Fatalf("non-Creator received keymap packet=%x snapshot=%q", connection.write.Bytes(), session.currentGameOptionNativeApplyKeymapBody)
	}
}
