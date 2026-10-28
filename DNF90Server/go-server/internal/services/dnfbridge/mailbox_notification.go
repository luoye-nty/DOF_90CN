package dnfbridge

import (
	"context"
	"fmt"
	"strconv"

	"longheng.io/server/internal/modules/dnf/mail"
)

// sendMailboxAlarmForSession projects a mailbox's durable unread state onto
// one already-active game session. It is used for both a live recipient and
// the first selected-town scene, which must explicitly clear a prior actor's
// indicator by sending a zero count as well.
func (s *Service) sendMailboxAlarmForSession(session *gameSession, characterID uint16, source string) error {
	if session == nil || characterID == 0 {
		return nil
	}
	repos, available := s.repositoryGroup()
	if !available {
		return fmt.Errorf("mailbox alarm repository is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := mail.NewOwner(repos).Open(ctx, strconv.FormatUint(uint64(characterID), 10))
	if err != nil {
		return fmt.Errorf("load mailbox alarm state: %w", err)
	}
	if err := s.sendGameUpperRawClass(
		session,
		mail.MailboxAlarmNotificationMessageID,
		mail.BuildAlarmNotification(result.Unread),
		0,
	); err != nil {
		return err
	}
	s.logGameEvent(session, "game-mailbox-alarm-sent",
		"source", source,
		"char_id", characterID,
		"unread", result.Unread)
	return nil
}

// sendMailboxAlarmToOnlineRecipient is deliberately a bridge-owned delivery
// projection: mailbox ownership has already committed in the mail module, and
// an offline recipient receives the same class0/op63 snapshot during the
// selected-town bootstrap. The game-session index and its wire mutex make the
// optional live notice safe without giving the domain module transport
// ownership.
func (s *Service) sendMailboxAlarmToOnlineRecipient(characterID uint16) error {
	session, online := s.onlineGameSession(characterID)
	if !online {
		return nil
	}
	return s.sendMailboxAlarmForSession(session, characterID, "online_recipient_mail_delivery")
}

// sendMailboxListRefreshToOnlineRecipient pushes the complete class0/op0x61
// mailbox page (the same body the client's own op96 gets) to an online
// character.
//
// WHY the alarm alone is not enough - read off NoPack.exe:
//
//	class0/0x63 -> FUN_01d7ff70 (registered by FUN_02262980(0x63, 0x1d7ff70, 0)
//	in the class-0 table FUN_01d927e0). It reads exactly one u16 and does
//	exactly two things:
//	    01d7ff7f  CALL FUN_034e39e0          ; count u16
//	    01d7ff8c  JLE 0x01d80008             ; count <= 0 -> no banner
//	    01d7ffcd  GetString(0x811)            ; 2065 = 收到新邮件, drawn as a
//	                                          ; floating notice at 0xff4e78ff
//	    01d8000d  CALL FUN_0270be70           ; the lazily built singleton
//	    01d80014  CALL FUN_0270b330           ; **(this+4) = count; *(this+4)[1] = 0
//	Nothing in that path reloads or repaints the mailbox list, so a letter that
//	arrives while the mailbox is open stays invisible until the player closes and
//	reopens it. The list is only ever rebuilt by the class0/0x61 reader
//	FUN_01d913c0, whose tail calls the singleton and then FUN_0270bc30 ->
//	FUN_0270b7f0 to rebuild the list from its container.
//
// That pair is not an invention: the GM mailbox-clear refresh in gm_admin.go
// already publishes alarm-then-page for exactly this reason, and op96 answers
// with this very snapshot. FUN_0270b7f0 only sorts and refills the mail
// container, so pushing it without an open mailbox cannot pop a window open.
//
// Deliberately NOT wired into the item auction's delivery path: that path must
// stay byte-identical, so this is only called by the 金币寄售 letter sender.
func (s *Service) sendMailboxListRefreshToOnlineRecipient(characterID uint16) error {
	if s == nil || characterID == 0 {
		return nil
	}
	session, online := s.onlineGameSession(characterID)
	if !online {
		return nil
	}
	repos, available := s.repositoryGroup()
	if !available {
		return fmt.Errorf("mailbox list refresh repository is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	opened, err := mail.NewOwner(repos).Open(ctx, strconv.FormatUint(uint64(characterID), 10))
	if err != nil {
		return fmt.Errorf("load mailbox list refresh state: %w", err)
	}
	snapshot, err := mail.BuildListNotification(opened)
	if err != nil {
		return fmt.Errorf("build mailbox list refresh page: %w", err)
	}
	if err := s.sendGameUpperRawClass(session, mail.MailboxListNotificationMessageID, snapshot, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-mailbox-list-refreshed",
		"source", "online_recipient_mail_delivery",
		"char_id", characterID,
		"mails", len(opened.Mails),
		"unread", opened.Unread,
		"page_len", len(snapshot))
	return nil
}
