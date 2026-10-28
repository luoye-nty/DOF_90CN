package raid

import (
	"bytes"
	"testing"
)

func TestCurrentRaidResultFlowMessageIDs(t *testing.T) {
	if RaidMovieMsgID != 0x0257 || RaidRewardListMsgID != 0x0258 || RaidResultMsgID != 0x0259 {
		t.Fatalf("S2C ids = %#04x/%#04x/%#04x", RaidMovieMsgID, RaidRewardListMsgID, RaidResultMsgID)
	}
	if RaidResultStageRequestMsgID != 0x0293 || RaidRewardCardRequestMsgID != 0x0294 ||
		RaidDoBehaviorRequestMsgID != 0x0295 || RaidMessageRequestMsgID != 0x0297 {
		t.Fatalf("C2S ids = %#04x/%#04x/%#04x/%#04x", RaidResultStageRequestMsgID, RaidRewardCardRequestMsgID, RaidDoBehaviorRequestMsgID, RaidMessageRequestMsgID)
	}
}

func TestRaidDoBehaviorRequestIsExactU32U16WithoutAck(t *testing.T) {
	request, err := DecodeRaidDoBehaviorRequest([]byte{0x44, 0x33, 0x22, 0x11, 0x66, 0x55})
	if err != nil {
		t.Fatalf("decode behavior: %v", err)
	}
	if request.Value0 != 0x11223344 || request.Value1 != 0x5566 {
		t.Fatalf("behavior = %+v", request)
	}
	for _, body := range [][]byte{nil, make([]byte, 5), make([]byte, 7)} {
		if _, err := DecodeRaidDoBehaviorRequest(body); err != errRaidDoBehaviorBodyInvalid {
			t.Fatalf("behavior body len=%d error=%v", len(body), err)
		}
	}
}

func TestRaidMessageRequestIsExactU8DSTRU8WithoutAck(t *testing.T) {
	body := []byte{2, 3, 0, 0, 0, 'a', 'b', 'c', 7}
	request, err := DecodeRaidMessageRequest(body)
	if err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if request.Mode != 2 || request.Tail != 7 || !bytes.Equal(request.MessageBytes, []byte("abc")) {
		t.Fatalf("message = %+v", request)
	}
	request.MessageBytes[0] = 'z'
	if body[5] != 'a' {
		t.Fatalf("decoded message aliases caller body")
	}

	for _, body := range [][]byte{nil, {0, 0, 0, 0, 0}, {0, 2, 0, 0, 0, 'a', 0}} {
		if _, err := DecodeRaidMessageRequest(body); err != errRaidMessageBodyInvalid && err != errRaidMessageDSTRTruncated {
			t.Fatalf("message body % X error=%v", body, err)
		}
	}
	if _, err := DecodeRaidMessageRequest([]byte{0, 1, 0, 0, 0, 'a', 0, 0}); err != errRaidMessageBodyInvalid {
		t.Fatalf("message trailing byte error=%v", err)
	}
}

func TestBuildRaidMovieSkipBodyIsExactlyEmpty(t *testing.T) {
	if got := BuildRaidMovieSkipBody(); got == nil || len(got) != 0 {
		t.Fatalf("movie-skip body = % X, want non-nil empty body", got)
	}
}

func TestBuildRaidSettlementResultBodyExactTenBytes(t *testing.T) {
	got, err := BuildRaidSettlementResultBody(RaidSettlementResult{
		ResultType:   2,
		PhaseIndex:   1,
		ClearSeconds: 0x11223344,
		DeathCount:   0x5566,
		Score:        0x77,
		Flags:        0x88,
	})
	if err != nil {
		t.Fatalf("BuildRaidSettlementResultBody error = %v", err)
	}
	want := []byte{2, 1, 0x44, 0x33, 0x22, 0x11, 0x66, 0x55, 0x77, 0x88}
	if !bytes.Equal(got, want) {
		t.Fatalf("result body = % X, want % X", got, want)
	}
	if len(got) != 10 {
		t.Fatalf("result body len = %d, want 10", len(got))
	}

	minimal, err := BuildRaidSettlementResultBody(RaidSettlementResult{ClearSeconds: 300})
	if err != nil {
		t.Fatalf("minimal result error = %v", err)
	}
	if want := []byte{0, 0, 0x2c, 0x01, 0, 0, 0, 0, 0, 0}; !bytes.Equal(minimal, want) {
		t.Fatalf("minimal result body = % X, want % X", minimal, want)
	}
}

func TestBuildRaidSettlementResultBodyRejectsNativeRangeOverflow(t *testing.T) {
	if _, err := BuildRaidSettlementResultBody(RaidSettlementResult{ResultType: 4}); err != errRaidResultTypeInvalid {
		t.Fatalf("result type error = %v, want %v", err, errRaidResultTypeInvalid)
	}
	if _, err := BuildRaidSettlementResultBody(RaidSettlementResult{PhaseIndex: 2}); err != errRaidResultPhaseInvalid {
		t.Fatalf("phase error = %v, want %v", err, errRaidResultPhaseInvalid)
	}
}

func TestBuildRaidSettlementRewardsBodyExactNestedBytes(t *testing.T) {
	got, err := BuildRaidSettlementRewardsBody(RaidSettlementRewards{
		ResultType: 1,
		Members: []RaidSettlementMemberRewards{
			{
				MemberKey: 0x1234,
				CardIndex: 5,
				Rewards: []RaidSettlementReward{
					{Kind: 0, ItemIDOrValue: 0x11223344, CountOrAux: 0x5566},
					{},
				},
			},
			{MemberKey: 0xabcd, CardIndex: 9},
		},
	})
	if err != nil {
		t.Fatalf("BuildRaidSettlementRewardsBody error = %v", err)
	}
	want := []byte{
		1, 2,
		0x34, 0x12, 5, 2,
		0, 0x44, 0x33, 0x22, 0x11, 0x66, 0x55,
		0, 0, 0, 0, 0, 0, 0,
		0xcd, 0xab, 9, 0,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("reward body = % X, want % X", got, want)
	}
}

func TestBuildRaidSettlementRewardsBodyValidatesCountsAndNormalItems(t *testing.T) {
	if _, err := BuildRaidSettlementRewardsBody(RaidSettlementRewards{
		Members: make([]RaidSettlementMemberRewards, maxRaidByteCount+1),
	}); err != errRaidByteCountOverflow {
		t.Fatalf("member overflow error = %v, want %v", err, errRaidByteCountOverflow)
	}
	if _, err := BuildRaidSettlementRewardsBody(RaidSettlementRewards{
		Members: []RaidSettlementMemberRewards{{
			Rewards: make([]RaidSettlementReward, maxRaidByteCount+1),
		}},
	}); err != errRaidByteCountOverflow {
		t.Fatalf("reward overflow error = %v, want %v", err, errRaidByteCountOverflow)
	}

	invalidItems := []RaidSettlementReward{
		{Kind: 0, ItemIDOrValue: 1},
		{Kind: 0, CountOrAux: 1},
	}
	for _, reward := range invalidItems {
		if _, err := BuildRaidSettlementRewardsBody(RaidSettlementRewards{
			Members: []RaidSettlementMemberRewards{{Rewards: []RaidSettlementReward{reward}}},
		}); err != errRaidNormalRewardItemInvalid {
			t.Fatalf("partial normal item %+v error = %v, want %v", reward, err, errRaidNormalRewardItemInvalid)
		}
	}

	if _, err := BuildRaidSettlementRewardsBody(RaidSettlementRewards{
		Members: []RaidSettlementMemberRewards{{Rewards: []RaidSettlementReward{{Kind: 1}}}},
	}); err != nil {
		t.Fatalf("unproved non-zero reward kind should remain encodable: %v", err)
	}
}

func TestRaidResultStageRequestExactBodyAndDecode(t *testing.T) {
	for _, stage := range []byte{0, 1} {
		got, err := BuildRaidResultStageRequestBody(RaidResultStageRequest{Stage: stage})
		if err != nil {
			t.Fatalf("build stage %d error = %v", stage, err)
		}
		if want := []byte{stage}; !bytes.Equal(got, want) {
			t.Fatalf("stage %d body = % X, want % X", stage, got, want)
		}
		decoded, err := DecodeRaidResultStageRequest(got)
		if err != nil || decoded.Stage != stage {
			t.Fatalf("decode stage %d = %+v, %v", stage, decoded, err)
		}
	}
	if _, err := BuildRaidResultStageRequestBody(RaidResultStageRequest{Stage: 2}); err != errRaidResultStageInvalid {
		t.Fatalf("invalid build stage error = %v", err)
	}
	for _, body := range [][]byte{nil, {0, 1}} {
		if _, err := DecodeRaidResultStageRequest(body); err != errRaidResultStageBodyInvalid {
			t.Fatalf("stage body % X error = %v, want %v", body, err, errRaidResultStageBodyInvalid)
		}
	}
	if _, err := DecodeRaidResultStageRequest([]byte{2}); err != errRaidResultStageInvalid {
		t.Fatalf("invalid decoded stage error = %v", err)
	}
	if got := BuildRaidResultStageAckBody(); got == nil || len(got) != 0 {
		t.Fatalf("stage ACK = % X, want non-nil empty body", got)
	}
}

func TestRaidRewardCardRequestExactBodyDecodeAndAck(t *testing.T) {
	for _, request := range []RaidRewardCardRequest{
		{Mode: 0, CardIndex: 0x11},
		{Mode: 1, CardIndex: 0x80},
		{Mode: 2, CardIndex: 0xff},
	} {
		got, err := BuildRaidRewardCardRequestBody(request)
		if err != nil {
			t.Fatalf("build %+v error = %v", request, err)
		}
		if want := []byte{request.Mode, request.CardIndex}; !bytes.Equal(got, want) {
			t.Fatalf("request %+v body = % X, want % X", request, got, want)
		}
		decoded, err := DecodeRaidRewardCardRequest(got)
		if err != nil || decoded != request {
			t.Fatalf("decode % X = %+v, %v; want %+v", got, decoded, err, request)
		}
	}
	if _, err := BuildRaidRewardCardRequestBody(RaidRewardCardRequest{Mode: 3}); err != errRaidRewardCardModeInvalid {
		t.Fatalf("invalid build mode error = %v", err)
	}
	for _, body := range [][]byte{nil, {0}, {0, 1, 2}} {
		if _, err := DecodeRaidRewardCardRequest(body); err != errRaidRewardCardBodyInvalid {
			t.Fatalf("card body % X error = %v, want %v", body, err, errRaidRewardCardBodyInvalid)
		}
	}
	if _, err := DecodeRaidRewardCardRequest([]byte{3, 0}); err != errRaidRewardCardModeInvalid {
		t.Fatalf("invalid decoded mode error = %v", err)
	}
	if got, want := BuildRaidRewardCardAckBody(true, 0xff), []byte{1}; !bytes.Equal(got, want) {
		t.Fatalf("success ACK = % X, want % X", got, want)
	}
	if got, want := BuildRaidRewardCardAckBody(false, 0x7e), []byte{0, 0x7e}; !bytes.Equal(got, want) {
		t.Fatalf("failure ACK = % X, want % X", got, want)
	}
}
