package guild

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestRegistryLifecyclePersistsAndEnforcesAuthority(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.Local)
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	applicant := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:1", Name: "member", Level: 90}

	registry := NewRegistry()
	created, err := registry.Create(master, "guild-one", now)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 1 || registry.NameAvailable("GUILD-ONE") {
		t.Fatalf("created=%+v available=%t", created, registry.NameAvailable("GUILD-ONE"))
	}
	if err := registry.Apply(created.ID, applicant, "join", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DecideApplication(created.ID, applicant.CharacterID, applicant.CharacterID, true, now); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("unauthorized approval error=%v", err)
	}
	member, err := registry.DecideApplication(created.ID, master.CharacterID, applicant.CharacterID, true, now.Add(2*time.Minute))
	if err != nil || member.Grade != DefaultMemberGrade {
		t.Fatalf("approved member=%+v error=%v", member, err)
	}
	if err := registry.ChangeGrade(master.CharacterID, applicant.CharacterID, 2, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Delegate(master.CharacterID, applicant.CharacterID, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := Save(ctx, repositories.Settings, registry, now); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	got, _, found := reloaded.GuildForCharacter(applicant.CharacterID)
	if !found || got.MasterCharacterID != applicant.CharacterID || got.MasterName != applicant.Name {
		t.Fatalf("reloaded guild=%+v found=%t", got, found)
	}
}

func TestRegistryInviteReplyAndDailyAttendance(t *testing.T) {
	now := time.Date(2026, 8, 19, 9, 0, 0, 0, time.Local)
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	target := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:1", Name: "target", Level: 80}
	registry := NewRegistry()
	guild, err := registry.Create(master, "guild-two", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Invite(master.CharacterID, target, now); err != nil {
		t.Fatal(err)
	}
	joined, err := registry.ReplyInvitation(target, true, now.Add(time.Minute))
	if err != nil || joined.ID != guild.ID {
		t.Fatalf("joined=%+v error=%v", joined, err)
	}
	if attended, err := registry.Attend(target.CharacterID, now.Add(2*time.Minute)); err != nil || attended {
		t.Fatalf("same-day attendance=%t error=%v", attended, err)
	}
	if attended, err := registry.Attend(target.CharacterID, now.Add(25*time.Hour)); err != nil || !attended {
		t.Fatalf("next-day attendance=%t error=%v", attended, err)
	}
}

func TestRegistryMemoAuthority(t *testing.T) {
	now := time.Date(2026, 8, 19, 9, 0, 0, 0, time.Local)
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	member := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:1", Name: "member", Level: 80}
	registry := NewRegistry()
	guild, err := registry.Create(master, "guild-memo", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Invite(master.CharacterID, member, now); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ReplyInvitation(member, true, now); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetMemo(member.CharacterID, member.CharacterID, "self", now); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetMemo(member.CharacterID, master.CharacterID, "forged", now); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("member editing master memo error=%v", err)
	}
	if err := registry.SetMemo(master.CharacterID, member.CharacterID, "reviewed", now); err != nil {
		t.Fatal(err)
	}
	if got := guild.Members[member.CharacterID].Memo; got != "reviewed" {
		t.Fatalf("member memo=%q", got)
	}
}

func TestRegistryMembershipTransitionsClearApplicationsInEveryGuild(t *testing.T) {
	now := time.Date(2026, 8, 22, 14, 0, 0, 0, time.Local)
	masterOne := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master-one", Level: 90}
	applicant := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:1", Name: "applicant", Level: 90}
	masterTwo := dnfrepo.CharacterRecord{CharacterID: "3", AccountID: "dnf:1", Name: "master-two", Level: 90}

	t.Run("create", func(t *testing.T) {
		registry := NewRegistry()
		guildOne, err := registry.Create(masterOne, "guild-one", now)
		if err != nil {
			t.Fatal(err)
		}
		guildTwo, err := registry.Create(masterTwo, "guild-two", now)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.Apply(guildOne.ID, applicant, "one", now); err != nil {
			t.Fatal(err)
		}
		if err := registry.Apply(guildTwo.ID, applicant, "two", now); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Create(applicant, "guild-three", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, found := guildOne.Applications[applicant.CharacterID]; found {
			t.Fatal("guild-one retained the creator's stale application")
		}
		if _, found := guildTwo.Applications[applicant.CharacterID]; found {
			t.Fatal("guild-two retained the creator's stale application")
		}
	})

	t.Run("approve", func(t *testing.T) {
		registry := NewRegistry()
		guildOne, err := registry.Create(masterOne, "guild-one", now)
		if err != nil {
			t.Fatal(err)
		}
		guildTwo, err := registry.Create(masterTwo, "guild-two", now)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.Apply(guildOne.ID, applicant, "one", now); err != nil {
			t.Fatal(err)
		}
		if err := registry.Apply(guildTwo.ID, applicant, "two", now); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.DecideApplication(guildOne.ID, masterOne.CharacterID, applicant.CharacterID, true, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, found := guildTwo.Applications[applicant.CharacterID]; found {
			t.Fatal("another guild retained the new member's stale application")
		}
	})
}
