// peer_response_refusal.go owns the nine-byte "player pressed 否" form of the
// class-1/op11 peer response.
//
// Live capture 2026-09-26 08:32:39 and 08:32:48 -- two presses of 否 in the
// party-application prompt -- plus 07:24:24 before them:
//
//	点「否」 body = 16 00 | 00 | 00 00 00 00 | 85 00   (9 bytes)
//	点「是」 body = 16 00 | 00 | 00 00 00 00           (7 bytes)
//
// party.DecodeResponsePeerRequest insists on exactly seven bytes and the only
// nine-byte fallback was runtimeRaidPeerResponseDeclined, which gated on mode
// 10. A refused PARTY application therefore never decoded: the server logged
// `game-party-response-peer-body-short` and returned, so the pending invite was
// never consumed and the applicant received nothing at all.
package dnfbridge

import (
	"longheng.io/server/internal/modules/dnf/party"
)

// currentPeerResponseAnswer is one op11 answer, refusal included.
type currentPeerResponseAnswer struct {
	Request party.ResponsePeerRequest
	Refused bool
	Reason  uint16
}

// decodeCurrentPeerResponseAnswer accepts both wire forms of op11: the plain
// seven-byte answer, and the nine-byte refusal. It returns the original decode
// error when neither matches, so the caller can still log the short-body case.
func decodeCurrentPeerResponseAnswer(body []byte) (currentPeerResponseAnswer, error) {
	parsed, err := party.DecodeResponsePeerRequest(body)
	if err == nil {
		return currentPeerResponseAnswer{Request: parsed}, nil
	}
	if refusal, reason, ok := party.DecodeResponsePeerRefusal(body); ok {
		return currentPeerResponseAnswer{Request: refusal, Refused: true, Reason: reason}, nil
	}
	return currentPeerResponseAnswer{}, err
}

// A refusal is now consumed, logged, and then deliberately left unanswered.
//
// Two live attempts on 2026-09-26 both failed, and the client reverse-engineering
// explains why:
//
//   - class-0/op8 (09:43) made the requester's client print 玩家[%s]已接受组队请求
//     -- the opposite of what happened. That line is string 624 (trade version
//     625) and the client has no refusal twin for it, so op8 cannot express a
//     refusal at all.
//   - class-1/op10 failure (09:50, code 0) was received -- the client trace shows
//     `class=1 opcode=10 packet_len=18` right after our two-byte body -- and did
//     nothing. A request takes exactly one answer and the success ACK already
//     spent it.
//
// The message the operator expected (对方不在线，或拒绝了您的组队请求) exists twice in
// the client's string table, but neither copy is reachable in this build: the
// bytes `push 0x111A6` / `push 0x111A7` occur nowhere in DNF.exe, and 70054 does
// not appear as a 4-byte value in any section. The scanner behind that claim was
// validated against ids that ARE referenced -- `push 0x11205` (70149) has 26
// sites, and `push 0x112e1` (70369) resolves to 0x1911B39, the exact instruction
// found by hand. The same holds for the trade pair 70056/70057, so the whole
// quartet is dead in this client.
//
// This matches the reference server: 86JP's PartyHandler cancels the invite on a
// type-0 refusal and sends the inviter nothing.
