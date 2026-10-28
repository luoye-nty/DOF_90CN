package dnfbridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentRobotSessionBindOpcode        uint16 = 0xfff0
	currentRobotSessionBindVersion       byte   = 1
	currentRobotSessionBindSignatureLen         = sha256.Size
	currentRobotSessionBindMaxAccountLen        = 96
	currentRobotSessionBindClockSkew            = 30 * time.Second
	currentRobotSessionBindReplayTTL            = 2 * time.Minute
	currentRobotSessionBindReplayLimit          = 4096
)

const (
	currentRobotSessionBindErrorMalformed byte = 1
	currentRobotSessionBindErrorDisabled  byte = 2
	currentRobotSessionBindErrorAuth      byte = 3
	currentRobotSessionBindErrorAccount   byte = 4
	currentRobotSessionBindErrorReplay    byte = 5
)

type currentRobotSessionBindRequest struct {
	TimestampUnix      int64
	Nonce              uint64
	DesiredCharacterID uint16
	AccountID          string
	Signed             []byte
	Signature          [currentRobotSessionBindSignatureLen]byte
}

func parseCurrentRobotSessionBindRequest(body []byte) (currentRobotSessionBindRequest, error) {
	const fixedPrefix = 1 + 8 + 8 + 2 + 1
	if len(body) < fixedPrefix+currentRobotSessionBindSignatureLen || body[0] != currentRobotSessionBindVersion {
		return currentRobotSessionBindRequest{}, errors.New("invalid robot session bind prefix")
	}
	accountLen := int(body[fixedPrefix-1])
	if accountLen <= 0 || accountLen > currentRobotSessionBindMaxAccountLen || len(body) != fixedPrefix+accountLen+currentRobotSessionBindSignatureLen {
		return currentRobotSessionBindRequest{}, errors.New("invalid robot session bind account length")
	}
	accountID := strings.TrimSpace(string(body[fixedPrefix : fixedPrefix+accountLen]))
	if accountID == "" || len(accountID) != accountLen || strings.IndexByte(accountID, 0) >= 0 {
		return currentRobotSessionBindRequest{}, errors.New("invalid robot session bind account")
	}
	request := currentRobotSessionBindRequest{
		TimestampUnix:      int64(binary.LittleEndian.Uint64(body[1:9])),
		Nonce:              binary.LittleEndian.Uint64(body[9:17]),
		DesiredCharacterID: binary.LittleEndian.Uint16(body[17:19]),
		AccountID:          accountID,
		Signed:             append([]byte(nil), body[:fixedPrefix+accountLen]...),
	}
	copy(request.Signature[:], body[fixedPrefix+accountLen:])
	return request, nil
}

func (s *Service) handleCurrentRobotSessionBind(session *gameSession, body []byte) error {
	if s == nil || session == nil {
		return errors.New("robot session bind service is unavailable")
	}
	request, err := parseCurrentRobotSessionBindRequest(body)
	if err != nil {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorMalformed)
	}
	if len(s.robotSessionSecret) == 0 {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorDisabled)
	}
	if !currentRobotSessionRemoteAllowed(session.conn) {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorAuth)
	}
	now := time.Now().UTC()
	timestamp := time.Unix(request.TimestampUnix, 0).UTC()
	if timestamp.Before(now.Add(-currentRobotSessionBindClockSkew)) || timestamp.After(now.Add(currentRobotSessionBindClockSkew)) {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorAuth)
	}
	mac := hmac.New(sha256.New, s.robotSessionSecret)
	_, _ = mac.Write(request.Signed)
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(expected, request.Signature[:]) != 1 {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorAuth)
	}
	if !s.reserveCurrentRobotSessionBindReplay(request.Signature, now) {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorReplay)
	}

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorAccount)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characters, err := repositories.Character.ListByAccount(ctx, request.AccountID, dnfrepo.MaxCharacterSlotLimit)
	if err != nil || len(characters) == 0 {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorAccount)
	}
	character, ok := selectCurrentRobotSessionCharacter(characters, request.DesiredCharacterID)
	if !ok {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorAccount)
	}
	characterID := numericCharacterID(character)
	if characterID <= 0 || characterID > int(^uint16(0)) || character.Slot < 0 || character.Slot > int(^uint8(0)) {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorAccount)
	}
	if existing := strings.TrimSpace(session.accountID); existing != "" && existing != request.AccountID {
		return s.sendCurrentRobotSessionBindFailure(session, currentRobotSessionBindErrorAuth)
	}
	session.accountID = request.AccountID
	session.robotSessionBound = true
	bodyOut := []byte{1, byte(character.Slot), 0, 0}
	binary.LittleEndian.PutUint16(bodyOut[2:4], uint16(characterID))
	s.logGameEvent(session, "game-robot-session-bound",
		"char_id", characterID,
		"slot", character.Slot,
		"source", "hmac_private_lan")
	return s.sendGame(session, byte(dnfenum.GameCmdCommand), currentRobotSessionBindOpcode, bodyOut)
}

func (s *Service) sendCurrentRobotSessionBindFailure(session *gameSession, code byte) error {
	return s.sendGame(session, byte(dnfenum.GameCmdCommand), currentRobotSessionBindOpcode, []byte{0, code})
}

func selectCurrentRobotSessionCharacter(characters []dnfrepo.CharacterRecord, desired uint16) (dnfrepo.CharacterRecord, bool) {
	ordered := append([]dnfrepo.CharacterRecord(nil), characters...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].Slot != ordered[right].Slot {
			return ordered[left].Slot < ordered[right].Slot
		}
		return numericCharacterID(ordered[left]) < numericCharacterID(ordered[right])
	})
	for _, character := range ordered {
		id := numericCharacterID(character)
		if id <= 0 || id > int(^uint16(0)) {
			continue
		}
		if desired == 0 || uint16(id) == desired {
			return character, true
		}
	}
	return dnfrepo.CharacterRecord{}, false
}

func (s *Service) reserveCurrentRobotSessionBindReplay(signature [currentRobotSessionBindSignatureLen]byte, now time.Time) bool {
	s.robotSessionReplayMu.Lock()
	defer s.robotSessionReplayMu.Unlock()
	if s.robotSessionReplays == nil {
		s.robotSessionReplays = make(map[[currentRobotSessionBindSignatureLen]byte]time.Time)
	}
	cutoff := now.Add(-currentRobotSessionBindReplayTTL)
	for digest, seenAt := range s.robotSessionReplays {
		if seenAt.Before(cutoff) {
			delete(s.robotSessionReplays, digest)
		}
	}
	if _, exists := s.robotSessionReplays[signature]; exists {
		return false
	}
	if len(s.robotSessionReplays) >= currentRobotSessionBindReplayLimit {
		var oldestDigest [currentRobotSessionBindSignatureLen]byte
		var oldest time.Time
		for digest, seenAt := range s.robotSessionReplays {
			if oldest.IsZero() || seenAt.Before(oldest) {
				oldestDigest, oldest = digest, seenAt
			}
		}
		delete(s.robotSessionReplays, oldestDigest)
	}
	s.robotSessionReplays[signature] = now
	return true
}

func currentRobotSessionRemoteAllowed(conn net.Conn) bool {
	if conn == nil || conn.RemoteAddr() == nil {
		return false
	}
	var addr netip.Addr
	switch value := conn.RemoteAddr().(type) {
	case *net.TCPAddr:
		addr, _ = netip.AddrFromSlice(value.IP)
	default:
		host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
		if err != nil {
			return false
		}
		addr, err = netip.ParseAddr(strings.Trim(host, "[]"))
		if err != nil {
			return false
		}
	}
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsPrivate()
}

func buildCurrentRobotSessionBindRequest(secret []byte, now time.Time, nonce uint64, desired uint16, accountID string) ([]byte, error) {
	accountID = strings.TrimSpace(accountID)
	if len(secret) == 0 || accountID == "" || len(accountID) > currentRobotSessionBindMaxAccountLen {
		return nil, fmt.Errorf("invalid robot bind fixture")
	}
	body := make([]byte, 1+8+8+2+1+len(accountID)+currentRobotSessionBindSignatureLen)
	body[0] = currentRobotSessionBindVersion
	binary.LittleEndian.PutUint64(body[1:9], uint64(now.Unix()))
	binary.LittleEndian.PutUint64(body[9:17], nonce)
	binary.LittleEndian.PutUint16(body[17:19], desired)
	body[19] = byte(len(accountID))
	copy(body[20:], accountID)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body[:20+len(accountID)])
	copy(body[20+len(accountID):], mac.Sum(nil))
	return body, nil
}
