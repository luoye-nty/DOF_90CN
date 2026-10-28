package dnfrobot

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	robotBindOpcode    uint16 = 0xfff0
	robotBindVersion          = byte(1)
	maximumFrameSize          = 1024 * 1024
	outerHeaderSize           = 12
	innerHeaderSize           = 9
	businessHeaderSize        = 7
)

type packet struct {
	Classification byte
	Opcode         uint16
	Body           []byte
}

func buildPacket(opcode, sequence uint16, body []byte) ([]byte, error) {
	total := 13 + len(body)
	if total > maximumFrameSize {
		return nil, fmt.Errorf("dnf robot packet too large: %d", total)
	}
	result := make([]byte, total)
	result[0] = 1
	binary.LittleEndian.PutUint16(result[1:3], opcode)
	binary.LittleEndian.PutUint32(result[3:7], uint32(total))
	binary.LittleEndian.PutUint16(result[11:13], sequence)
	copy(result[13:], body)
	return result, nil
}

func buildBindBody(secret []byte, now time.Time, characterID uint16, accountID string) ([]byte, error) {
	accountID = strings.TrimSpace(accountID)
	if len(secret) == 0 || accountID == "" || len(accountID) > 96 || strings.IndexByte(accountID, 0) >= 0 {
		return nil, errors.New("invalid dnf robot bind identity")
	}
	var nonce [8]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return nil, err
	}
	body := make([]byte, 1+8+8+2+1+len(accountID)+sha256.Size)
	body[0] = robotBindVersion
	binary.LittleEndian.PutUint64(body[1:9], uint64(now.Unix()))
	copy(body[9:17], nonce[:])
	binary.LittleEndian.PutUint16(body[17:19], characterID)
	body[19] = byte(len(accountID))
	copy(body[20:], accountID)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body[:20+len(accountID)])
	copy(body[20+len(accountID):], mac.Sum(nil))
	return body, nil
}

func nextFrame(buffer []byte) (size int, outer bool, ready bool, valid bool) {
	if len(buffer) < 4 {
		return 0, false, false, true
	}
	outerSize := int(binary.LittleEndian.Uint16(buffer[2:4]))
	if binary.LittleEndian.Uint16(buffer[:2]) == 0 && outerSize >= outerHeaderSize+innerHeaderSize+businessHeaderSize {
		if outerSize > maximumFrameSize {
			return 0, false, false, false
		}
		if len(buffer) < outerHeaderSize+innerHeaderSize {
			return 0, true, false, true
		}
		businessSize := int(binary.LittleEndian.Uint16(buffer[17:19]))
		if buffer[12] == 2 && businessSize >= businessHeaderSize && outerHeaderSize+innerHeaderSize+businessSize <= outerSize {
			return outerSize, true, len(buffer) >= outerSize, true
		}
	}
	if len(buffer) < 7 {
		return 0, false, false, true
	}
	rawSize := int(binary.LittleEndian.Uint32(buffer[3:7]))
	if rawSize < 13 || rawSize > maximumFrameSize {
		return 0, false, false, false
	}
	return rawSize, false, len(buffer) >= rawSize, true
}

func decodeFrame(frame []byte, outer bool) ([]packet, error) {
	if !outer {
		if len(frame) < 13 {
			return nil, errors.New("short dnf robot raw frame")
		}
		headerSize := 13
		opcode := binary.LittleEndian.Uint16(frame[1:3])
		if opcode == 8 && frame[0] == 0 && len(frame) >= 15 {
			headerSize = 15
		} else if len(frame) >= 16 && frame[13] == 0 && frame[14] == 0 && frame[15] == 0 {
			headerSize = 16
		}
		return []packet{{Classification: frame[0], Opcode: opcode, Body: append([]byte(nil), frame[headerSize:]...)}}, nil
	}
	if len(frame) < outerHeaderSize {
		return nil, errors.New("short dnf robot outer frame")
	}
	limit := int(binary.LittleEndian.Uint16(frame[2:4]))
	if limit > len(frame) {
		return nil, errors.New("invalid dnf robot outer length")
	}
	result := make([]packet, 0, 2)
	for offset := outerHeaderSize; offset+innerHeaderSize <= limit; {
		if frame[offset] != 2 {
			return nil, errors.New("invalid dnf robot inner frame")
		}
		businessSize := int(binary.LittleEndian.Uint16(frame[offset+5 : offset+7]))
		start := offset + innerHeaderSize
		end := start + businessSize
		if businessSize < businessHeaderSize || end > limit {
			return nil, errors.New("invalid dnf robot business frame")
		}
		business := frame[start:end]
		result = append(result, packet{
			Classification: business[0],
			Opcode:         binary.LittleEndian.Uint16(business[1:3]),
			Body:           append([]byte(nil), business[7:]...),
		})
		offset = end
	}
	return result, nil
}
