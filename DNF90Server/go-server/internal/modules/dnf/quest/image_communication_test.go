package quest

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestActiveImageCommunicationTargetUsesActivePVFMeetNPC(t *testing.T) {
	remote := Definition{
		ID:               3734,
		Type:             "[meet npc]",
		IntData:          []int64{2000},
		ConditionMessage: "利用通信装备与梅尔文联系。（远程通话装置只能在相关任务存在的情况下才能使用）",
	}
	ordinary := Definition{ID: 3735, Type: "[meet npc]", IntData: []int64{59}, ConditionMessage: "与梅尔文对话。"}
	catalog := &Catalog{byID: map[int64]Definition{remote.ID: remote, ordinary.ID: ordinary}}

	target, ok := catalog.ActiveImageCommunicationTarget(dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		3734: {Status: "active"},
		3735: {Status: "active"},
	}})
	if !ok || target != (ImageCommunicationTarget{QuestID: 3734, NPCID: 2000}) {
		t.Fatalf("target=%+v ok=%t", target, ok)
	}
}

func TestActiveImageCommunicationTargetCanonicalStateOverridesStaleProgress(t *testing.T) {
	remote := Definition{
		ID:               3734,
		Type:             "meet npc",
		IntData:          []int64{2000},
		ConditionMessage: "远程通话装置只能在相关任务存在的情况下使用。",
	}
	catalog := &Catalog{byID: map[int64]Definition{remote.ID: remote}}
	record := dnfrepo.QuestRecord{
		States:   map[int64]dnfrepo.QuestState{3734: {Status: "completed"}},
		Progress: map[int64]dnfrepo.QuestState{3734: {Status: "active"}},
	}
	if target, ok := catalog.ActiveImageCommunicationTarget(record); ok {
		t.Fatalf("completed canonical state resolved stale target=%+v", target)
	}
}

func TestActiveImageCommunicationTargetRejectsAmbiguousRemoteQuests(t *testing.T) {
	first := Definition{ID: 3734, Type: "meet npc", IntData: []int64{2000}, ConditionMessage: "远程通话装置只能使用。"}
	second := Definition{ID: 3741, Type: "meet npc", IntData: []int64{2001}, ConditionMessage: "视频通信装置只能使用。"}
	catalog := &Catalog{byID: map[int64]Definition{first.ID: first, second.ID: second}}
	record := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		3734: {Status: "active"},
		3741: {Status: "active"},
	}}
	if target, ok := catalog.ActiveImageCommunicationTarget(record); ok {
		t.Fatalf("ambiguous target=%+v", target)
	}
}
