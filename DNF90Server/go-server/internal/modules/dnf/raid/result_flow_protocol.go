package raid

import (
	"encoding/binary"
	"errors"
)

const (
	// Current-client C2S command ids used by the native raid result flow.
	RaidResultStageRequestMsgID uint16 = 0x0293
	RaidRewardCardRequestMsgID  uint16 = 0x0294
	RaidDoBehaviorRequestMsgID  uint16 = 0x0295
	RaidMessageRequestMsgID     uint16 = 0x0297

	maxRaidResultType     byte = 3
	maxRaidResultPhase    byte = 1
	maxRaidRewardCardMode byte = 2
)

// The raid banner is NOT built here. It is the class-0 notification opcode 119,
// sub_1D88DE0, whose body is op12's twin; dnfbridge owns that builder
// (buildCurrentRaidAnnouncementBody) next to the chat notification it mirrors.
// 0x024C is a different surface entirely and raises no banner.

var (
	errRaidResultTypeInvalid       = errors.New("raid result type must be between 0 and 3")
	errRaidResultPhaseInvalid      = errors.New("raid result phase must be 0 or 1")
	errRaidResultStageBodyInvalid  = errors.New("raid result stage request body must contain exactly one byte")
	errRaidResultStageInvalid      = errors.New("raid result stage must be 0 or 1")
	errRaidRewardCardBodyInvalid   = errors.New("raid reward card request body must contain exactly two bytes")
	errRaidRewardCardModeInvalid   = errors.New("raid reward card mode must be between 0 and 2")
	errRaidDoBehaviorBodyInvalid   = errors.New("raid behavior request body must contain exactly six bytes")
	errRaidMessageBodyInvalid      = errors.New("raid message request body must contain u8,dstr,u8 exactly")
	errRaidMessageDSTRTruncated    = errors.New("raid message request dstr is truncated")
	errRaidNormalRewardItemInvalid = errors.New("raid normal item reward requires both a non-zero item id and count")
)

// RaidSettlementResult is the fixed-width 0x0259 raid result body consumed by
// the current client.
type RaidSettlementResult struct {
	ResultType   byte
	PhaseIndex   byte
	ClearSeconds uint32
	DeathCount   uint16
	Score        byte
	Flags        byte
}

// RaidSettlementReward is one seven-byte inner row in a 0x0258 member group.
// Kind zero is the conservative normal-item form. A fully zero row is the
// native empty sentinel.
type RaidSettlementReward struct {
	Kind          byte
	ItemIDOrValue uint32
	CountOrAux    uint16
}

// RaidSettlementMemberRewards is one member/card group in 0x0258.
type RaidSettlementMemberRewards struct {
	MemberKey uint16
	CardIndex byte
	Rewards   []RaidSettlementReward
}

// RaidSettlementRewards is the complete nested 0x0258 body.
type RaidSettlementRewards struct {
	ResultType byte
	Members    []RaidSettlementMemberRewards
}

// RaidResultStageRequest is the exact one-byte C2S 659 request.
type RaidResultStageRequest struct {
	Stage byte
}

// RaidRewardCardRequest is the exact two-byte C2S 660 request.
type RaidRewardCardRequest struct {
	Mode      byte
	CardIndex byte
}

// RaidDoBehaviorRequest is the exact fixed-width C2S 661 report. The current
// client has no corresponding ACK handler, so the two fields remain neutral
// until their gameplay meanings are proved separately.
type RaidDoBehaviorRequest struct {
	Value0 uint32
	Value1 uint16
}

// RaidMessageRequest is the exact C2S 663 report. MessageBytes preserves the
// current DSTR bytes without assuming an encoding at the protocol boundary.
// The current client has no corresponding ACK handler.
type RaidMessageRequest struct {
	Mode         byte
	MessageBytes []byte
	Tail         byte
}

// BuildRaidMovieSkipBody builds S2C 0x0257. Its native body is empty.
func BuildRaidMovieSkipBody() []byte {
	return []byte{}
}

// BuildRaidSettlementResultBody builds the exact ten-byte S2C 0x0259 body.
func BuildRaidSettlementResultBody(result RaidSettlementResult) ([]byte, error) {
	if result.ResultType > maxRaidResultType {
		return nil, errRaidResultTypeInvalid
	}
	if result.PhaseIndex > maxRaidResultPhase {
		return nil, errRaidResultPhaseInvalid
	}

	var writer packetWriter
	writer.writeByte(result.ResultType)
	writer.writeByte(result.PhaseIndex)
	writer.writeUint32(result.ClearSeconds)
	writer.writeUint16(result.DeathCount)
	writer.writeByte(result.Score)
	writer.writeByte(result.Flags)
	return writer.bytes(), nil
}

// BuildRaidSettlementRewardsBody builds S2C 0x0258 in the current client's
// nested member/card/reward order.
func BuildRaidSettlementRewardsBody(rewards RaidSettlementRewards) ([]byte, error) {
	if err := validateRaidByteCount(len(rewards.Members)); err != nil {
		return nil, err
	}
	for _, member := range rewards.Members {
		if err := validateRaidByteCount(len(member.Rewards)); err != nil {
			return nil, err
		}
		for _, reward := range member.Rewards {
			if reward.Kind == 0 {
				emptySentinel := reward.ItemIDOrValue == 0 && reward.CountOrAux == 0
				completeItem := reward.ItemIDOrValue != 0 && reward.CountOrAux != 0
				if !emptySentinel && !completeItem {
					return nil, errRaidNormalRewardItemInvalid
				}
			}
		}
	}

	var writer packetWriter
	writer.writeByte(rewards.ResultType)
	writer.writeByte(byte(len(rewards.Members)))
	for _, member := range rewards.Members {
		writer.writeUint16(member.MemberKey)
		writer.writeByte(member.CardIndex)
		writer.writeByte(byte(len(member.Rewards)))
		for _, reward := range member.Rewards {
			writer.writeByte(reward.Kind)
			writer.writeUint32(reward.ItemIDOrValue)
			writer.writeUint16(reward.CountOrAux)
		}
	}
	return writer.bytes(), nil
}

// BuildRaidResultStageRequestBody builds the exact C2S 659 body used in byte
// fixtures and protocol clients.
func BuildRaidResultStageRequestBody(request RaidResultStageRequest) ([]byte, error) {
	if request.Stage > maxRaidResultPhase {
		return nil, errRaidResultStageInvalid
	}
	return []byte{request.Stage}, nil
}

// DecodeRaidResultStageRequest parses the exact C2S 659 body.
func DecodeRaidResultStageRequest(body []byte) (RaidResultStageRequest, error) {
	if len(body) != 1 {
		return RaidResultStageRequest{}, errRaidResultStageBodyInvalid
	}
	request := RaidResultStageRequest{Stage: body[0]}
	if request.Stage > maxRaidResultPhase {
		return RaidResultStageRequest{}, errRaidResultStageInvalid
	}
	return request, nil
}

// BuildRaidResultStageAckBody builds the empty ACK body consumed by the
// current client's no-op C2S 659 ACK handler.
func BuildRaidResultStageAckBody() []byte {
	return []byte{}
}

// BuildRaidRewardCardRequestBody builds the exact C2S 660 body used in byte
// fixtures and protocol clients.
func BuildRaidRewardCardRequestBody(request RaidRewardCardRequest) ([]byte, error) {
	if request.Mode > maxRaidRewardCardMode {
		return nil, errRaidRewardCardModeInvalid
	}
	return []byte{request.Mode, request.CardIndex}, nil
}

// DecodeRaidRewardCardRequest parses the exact C2S 660 body.
func DecodeRaidRewardCardRequest(body []byte) (RaidRewardCardRequest, error) {
	if len(body) != 2 {
		return RaidRewardCardRequest{}, errRaidRewardCardBodyInvalid
	}
	request := RaidRewardCardRequest{Mode: body[0], CardIndex: body[1]}
	if request.Mode > maxRaidRewardCardMode {
		return RaidRewardCardRequest{}, errRaidRewardCardModeInvalid
	}
	return request, nil
}

// BuildRaidRewardCardAckBody builds the current C2S 660 ACK. Success consumes
// only one byte; failure consumes the reason byte as well.
func BuildRaidRewardCardAckBody(success bool, reason byte) []byte {
	if success {
		return []byte{1}
	}
	return []byte{0, reason}
}

// DecodeRaidDoBehaviorRequest parses the exact u32,u16 C2S 661 body.
func DecodeRaidDoBehaviorRequest(body []byte) (RaidDoBehaviorRequest, error) {
	if len(body) != 6 {
		return RaidDoBehaviorRequest{}, errRaidDoBehaviorBodyInvalid
	}
	return RaidDoBehaviorRequest{
		Value0: binary.LittleEndian.Uint32(body[:4]),
		Value1: binary.LittleEndian.Uint16(body[4:]),
	}, nil
}

// DecodeRaidMessageRequest parses the exact u8,DSTR,u8 C2S 663 body.
func DecodeRaidMessageRequest(body []byte) (RaidMessageRequest, error) {
	if len(body) < 6 {
		return RaidMessageRequest{}, errRaidMessageBodyInvalid
	}
	messageLength := uint64(binary.LittleEndian.Uint32(body[1:5]))
	messageEnd := uint64(5) + messageLength
	if messageEnd > uint64(len(body)) {
		return RaidMessageRequest{}, errRaidMessageDSTRTruncated
	}
	if messageEnd+1 != uint64(len(body)) {
		return RaidMessageRequest{}, errRaidMessageBodyInvalid
	}
	return RaidMessageRequest{
		Mode:         body[0],
		MessageBytes: append([]byte(nil), body[5:int(messageEnd)]...),
		Tail:         body[messageEnd],
	}, nil
}
