package dnfrobot

import dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"

var defaultAmbientChatMessages = []string{
	"今天也要努力冒险",
	"有人一起刷图吗",
	"整理一下背包",
	"城镇里真热闹",
	"休息一会再出发",
	"祝大家都出好装备",
	"准备下一次冒险",
	"这里风景不错",
}

var defaultAmbientMegaphoneMessages = []string{
	"冒险家们好，城镇里见",
	"需要材料或装备可以来找机器人交易",
	"祝大家今天也能出好装备",
}

// ambientAutomationPolicy makes robots without an expert profession live in
// town independently of the fleet target-online scheduler. Movement, local
// chat and server-megaphone announcements remain configurable through the
// normal automation policy; automatic private/expert stores are disabled.
func ambientAutomationPolicy(policy dnfrobotdomain.AutomationPolicy) dnfrobotdomain.AutomationPolicy {
	policy.Enabled = true
	policy.TargetOnline = 0
	policy.ScaleIntervalSeconds = 0
	policy.ScaleUpBatch = 0
	policy.ScaleDownBatch = 0
	if policy.MoveIntervalMinSeconds <= 0 || policy.MoveIntervalMaxSeconds <= 0 {
		policy.MoveIntervalMinSeconds = 8
		policy.MoveIntervalMaxSeconds = 30
	}
	if policy.MoveRadius == 0 {
		policy.MoveRadius = 120
	}
	if policy.ChatIntervalMinSeconds <= 0 || policy.ChatIntervalMaxSeconds <= 0 {
		policy.ChatIntervalMinSeconds = 60
		policy.ChatIntervalMaxSeconds = 180
	}
	if policy.ChatMode == 0 {
		policy.ChatMode = 3
	}
	if len(policy.ChatMessages) == 0 {
		policy.ChatMessages = append([]string(nil), defaultAmbientChatMessages...)
	}
	if len(policy.MegaphoneMessages) == 0 {
		policy.MegaphoneMessages = append([]string(nil), defaultAmbientMegaphoneMessages...)
	}
	policy.StoreIntervalMinSeconds = 0
	policy.StoreIntervalMaxSeconds = 0
	policy.StoreDurationSeconds = 0
	policy.StoreConcurrent = 0
	policy.StoreMaximumRows = 0
	policy.StoreUnitPrice = 0
	policy.StoreTitle = ""
	return policy
}
