package onlineevent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

func weeklyTestCatalog() *WeeklyCatalog {
	c := &WeeklyCatalog{RequiredClears: 4, CatchupClears: 20, CatchupLimit: 2, Thresholds: [3]uint32{4, 8, 11}}
	for i := range c.Rewards {
		c.Rewards[i] = AttendancePVFItemReward{ItemID: int64(100 + i), Count: 1}
	}
	for i := range c.Bonus {
		c.Bonus[i] = AttendancePVFItemReward{ItemID: int64(200 + i), Count: 1}
	}
	return c
}

func TestWeeklyAttendanceClaimsCatchupsRollbackAndBoundary(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	c := weeklyTestCatalog()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "a"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2"} {
		if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{AccountID: "a", CharacterID: id}); err != nil {
			t.Fatal(err)
		}
	}
	cmd := WeeklyCommand{AccountID: "a", CharacterID: "1", Now: WeeklyStarts().AddDate(0, 0, 21), Claim: true}
	deliveryFailure := false
	cmd.Deliver = func(mailboxes dnfrepo.MailboxRepository, reward AttendancePVFItemReward, key string) (string, error) {
		if deliveryFailure {
			return "", errors.New("mail write failed")
		}
		return dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{RecipientCharacterID: cmd.CharacterID, Source: "test", CompletionKey: key, Attachments: []dnfrepo.MailAttachment{{ItemID: reward.ItemID, Count: reward.Count}}})
	}
	clear := func(count int) {
		t.Helper()
		a, _, err := repos.Account.Load(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		for range count {
			if err := RecordWeeklyClear(&a, c, cmd.Now); err != nil {
				t.Fatal(err)
			}
		}
		if err := repos.Account.Save(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(total, clears, catchups uint32, mails int) WeeklySnapshot {
		t.Helper()
		s, err := SettleWeeklyAttendance(ctx, repos, c, cmd)
		if err != nil || s.Total != total || s.Clears != clears || s.Catchups != catchups || len(s.MailIDs) != mails {
			t.Fatalf("state=%+v err=%v", s, err)
		}
		return s
	}
	clear(3)
	claim(0, 3, 0, 0)
	clear(1)
	deliveryFailure = true
	if _, err := SettleWeeklyAttendance(ctx, repos, c, cmd); err == nil {
		t.Fatal("mail failure committed")
	}
	mailbox, _, _ := repos.Mailbox.Load(ctx, "1")
	if len(mailbox.Mails) != 0 {
		t.Fatal("failed mail leaked")
	}
	deliveryFailure = false
	s := claim(1, 0, 0, 1)
	if !s.SignedThisWeek || !s.CanCatchup || s.SignedWeeks[0] != 4 {
		t.Fatalf("normal week=%+v", s)
	}
	cmd.CharacterID = "2"
	claim(1, 0, 0, 0)
	clear(19)
	claim(1, 19, 0, 0)
	clear(1)
	s = claim(2, 0, 1, 1)
	if s.SignedWeeks[0] != 1 || s.SignedWeeks[1] != 4 {
		t.Fatalf("catchup target=%+v", s)
	}
	clear(20)
	s = claim(3, 0, 2, 1)
	if s.CanCatchup || s.SignedWeeks[1] != 2 {
		t.Fatalf("catchup limit=%+v", s)
	}
	clear(100)
	claim(3, 44, 2, 0)
	cmd.Now = WeeklyStarts().AddDate(0, 0, 28).Add(-time.Nanosecond)
	claim(3, 44, 2, 0)
	cmd.Now = cmd.Now.Add(time.Nanosecond)
	s = claim(3, 0, 2, 0)
	if s.Week != 5 || s.SignedThisWeek {
		t.Fatalf("weekly reset=%+v", s)
	}
	clear(4)
	claim(4, 0, 2, 1)
	cmd.ClaimBonus = true
	s = claim(4, 0, 2, 1)
	if s.BonusMask != 1<<4 || s.CanBonus {
		t.Fatalf("bonus=%+v", s)
	}
	claim(4, 0, 2, 0)
	cmd.AccountID = "other"
	if _, err := SettleWeeklyAttendance(ctx, repos, c, cmd); err == nil {
		t.Fatal("foreign account accepted")
	}
	cmd.AccountID = "a"
	cmd.Now = WeeklyEnds()
	clear(4)
	s = claim(4, 0, 2, 0)
	if s.Week != 13 || s.CanSign || s.CanCatchup || s.CanBonus {
		t.Fatalf("expired edition=%+v", s)
	}
	if WeeklyWeek(WeeklyStarts().Add(-time.Nanosecond)) != -1 || WeeklyWeek(WeeklyStarts()) != 0 || WeeklyWeek(WeeklyEnds().Add(-time.Nanosecond)) != 11 {
		t.Fatal("edition boundary invalid")
	}
	mailbox, _, _ = repos.Mailbox.Load(ctx, "2")
	seen := map[int64]int{}
	for _, mail := range mailbox.Mails {
		seen[mail.Attachments[0].ItemID]++
	}
	for _, id := range []int64{101, 102, 103, 200} {
		if seen[id] != 1 {
			t.Fatalf("sequential reward %d count=%d", id, seen[id])
		}
	}
}

func TestWeeklyAttendanceConcurrentAccountBonusClaims(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	c := weeklyTestCatalog()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "a", Metadata: map[string]string{WeeklyMetadataKey: `{"signed":2047}`}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"1", "2"} {
		if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{AccountID: "a", CharacterID: id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"1", "2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := SettleWeeklyAttendance(ctx, repos, c, WeeklyCommand{AccountID: "a", CharacterID: id, Now: WeeklyStarts().AddDate(0, 0, 70), ClaimBonus: true,
				Deliver: func(mailboxes dnfrepo.MailboxRepository, reward AttendancePVFItemReward, key string) (string, error) {
					return dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{RecipientCharacterID: id, Source: "test", CompletionKey: key, Attachments: []dnfrepo.MailAttachment{{ItemID: reward.ItemID, Count: reward.Count}}})
				},
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	seen := map[int64]int{}
	for _, id := range []string{"1", "2"} {
		m, _, _ := repos.Mailbox.Load(ctx, id)
		for _, mail := range m.Mails {
			seen[mail.Attachments[0].ItemID]++
		}
	}
	for _, id := range []int64{200, 201, 202} {
		if seen[id] != 1 {
			t.Fatalf("concurrent bonus %d count=%d", id, seen[id])
		}
	}
	a, _, _ := repos.Account.Load(ctx, "a")
	state, err := loadWeeklyState(a, c)
	if err != nil || weeklySnapshot(state, c, 10).BonusMask != 1<<4|1<<8|1<<11 {
		t.Fatalf("bonus flags=%+v err=%v", state, err)
	}
}
