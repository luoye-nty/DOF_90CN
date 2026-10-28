package dnf90

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	appkit "longheng.io/server/internal/platform/app"
	"longheng.io/server/internal/services/dnfbridge"
)

func TestGMAdminRoutesReadAndWrite(t *testing.T) {
	t.Parallel()
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "dnf:test", State: "active"}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "7",
		AccountID:   "dnf:test",
		Name:        "GM Route",
		Job:         "0",
		Level:       1,
		Stats:       map[string]int64{"gold": 10, "exp": 0},
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	service := dnfbridge.NewWithOptions(&appkit.Env{}, dnfbridge.ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})
	env := &appkit.Env{AdminMux: http.NewServeMux()}
	registerGMAdminRoutes(env, service)

	listRecorder := httptest.NewRecorder()
	env.AdminMux.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/dnf/gm/accounts/dnf:test/characters", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listed struct {
		Success bool                           `json:"success"`
		Data    []dnfbridge.GMCharacterSummary `json:"data"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listed); err != nil || !listed.Success || len(listed.Data) != 1 {
		t.Fatalf("list response = %+v, %v", listed, err)
	}

	writeRecorder := httptest.NewRecorder()
	env.AdminMux.ServeHTTP(writeRecorder, httptest.NewRequest(
		http.MethodPost,
		"/dnf/gm/characters/7/gold",
		strings.NewReader(`{"value":888}`),
	))
	if writeRecorder.Code != http.StatusOK {
		t.Fatalf("write status = %d body=%s", writeRecorder.Code, writeRecorder.Body.String())
	}
	character, found, err := repositories.Character.Load(ctx, "7")
	if err != nil || !found || character.Stats["gold"] != 888 {
		t.Fatalf("written character = %+v, found=%v err=%v", character, found, err)
	}
}

func TestGMAdminMailboxListAndForcedDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "7", AccountID: "dnf:test", Name: "Mailbox GM", Job: "0", Level: 1,
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repositories.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "7",
		Mails: map[string]dnfrepo.MailRecord{
			"12": {
				MailID: "12", SenderName: "GM", Title: "未领取附件",
				Attachments: []dnfrepo.MailAttachment{{ItemID: 5001, Count: 1}},
			},
		},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}
	service := dnfbridge.NewWithOptions(&appkit.Env{}, dnfbridge.ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})
	env := &appkit.Env{AdminMux: http.NewServeMux()}
	registerGMAdminRoutes(env, service)

	listRecorder := httptest.NewRecorder()
	env.AdminMux.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/dnf/gm/characters/7/mailbox", nil))
	if listRecorder.Code != http.StatusOK || !strings.Contains(listRecorder.Body.String(), `"mail_id":"12"`) {
		t.Fatalf("mailbox list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}

	rejectRecorder := httptest.NewRecorder()
	env.AdminMux.ServeHTTP(rejectRecorder, httptest.NewRequest(
		http.MethodPost, "/dnf/gm/characters/7/mailbox/delete", strings.NewReader(`{"mail_ids":["12"],"force":false}`),
	))
	if rejectRecorder.Code != http.StatusBadRequest {
		t.Fatalf("non-forced delete status=%d body=%s", rejectRecorder.Code, rejectRecorder.Body.String())
	}

	deleteRecorder := httptest.NewRecorder()
	env.AdminMux.ServeHTTP(deleteRecorder, httptest.NewRequest(
		http.MethodPost, "/dnf/gm/characters/7/mailbox/delete", strings.NewReader(`{"mail_ids":["12"],"force":true}`),
	))
	if deleteRecorder.Code != http.StatusOK || !strings.Contains(deleteRecorder.Body.String(), `"deleted":1`) {
		t.Fatalf("forced delete status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "7")
	if err != nil || !found {
		t.Fatalf("load mailbox found=%v err=%v", found, err)
	}
	mail := mailbox.Mails["12"]
	if !mail.Deleted || len(mail.Attachments) != 0 || mail.Gold != 0 {
		t.Fatalf("deleted mail retained assets: %+v", mail)
	}
}

func TestGMAdminUnlocksExtraEquipmentSlots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "9", AccountID: "dnf:test", Name: "Slot GM", Job: "0", Level: 90,
		Stats: map[string]int64{"ex_equip_slot_stat": 0},
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	service := dnfbridge.NewWithOptions(&appkit.Env{}, dnfbridge.ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})
	env := &appkit.Env{AdminMux: http.NewServeMux()}
	registerGMAdminRoutes(env, service)

	request := httptest.NewRequest(
		http.MethodPost,
		"/dnf/gm/characters/9/equipment-slots/unlock",
		strings.NewReader(`{"slots":["support","magic_stone","earring"]}`),
	)
	recorder := httptest.NewRecorder()
	env.AdminMux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unlock status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool                                `json:"success"`
		Data    dnfbridge.GMExtraEquipmentSlotState `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Success || response.Data.Mask != dnfquest.ExEquipSlotAll ||
		!response.Data.SupportUnlocked || !response.Data.MagicStoneUnlocked || !response.Data.EarringUnlocked {
		t.Fatalf("unlock response=%+v", response)
	}
	character, found, err := repositories.Character.Load(ctx, "9")
	if err != nil || !found {
		t.Fatalf("load character found=%v err=%v", found, err)
	}
	if got := character.Stats["ex_equip_slot_stat"]; got != int64(dnfquest.ExEquipSlotAll) {
		t.Fatalf("ex_equip_slot_stat=%#x want=%#x", got, dnfquest.ExEquipSlotAll)
	}
}
