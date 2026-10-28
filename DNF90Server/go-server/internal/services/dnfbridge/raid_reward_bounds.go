package dnfbridge

// raidRewardJournalLimit 是 raidRewardCompleted / raidRewardPublished 两张表的
// 容量上限。
//
// 这两张表按 {RaidKey, Phase, Token} 记录每次结算的幂等凭据。Token 每次结算递增、
// RaidKey 由单调的 nextRaidSeq 生成且永不复用，所以**每次结算都是一个新键**：
// 没有上限就会随进程生命周期内已结算的攻坚次数一直涨（估算 4~24 MB/天）。
//
// 更麻烦的是它的读取方式：completedSingleRuntimeRaidForLeave（raid_online.go:1824）
// 与 completedTeamRuntimeRaidForLeave（raid_online.go:1866）为了按角色找到那场已结束
// 的攻坚，每次退攻坚请求都要**全表遍历**这张 map，代价 O(已完成攻坚数)。累积一万次
// 结算之后，每次退攻坚都要扫一万项并对命中项做 cloneRuntimeRaidState 深拷贝。
//
// 上限把「内存」和「退攻坚开销」一起封顶。语义上只丢弃很久以前就已结束、客户端
// 早已不可能再点「退出攻坚队」的历史记录；最近 1024 次结算的幂等凭据全部保留。
const raidRewardJournalLimit = 1024

// storeRaidRewardCompletionLocked 写入一条结算凭据，并在超出上限时淘汰最旧的一条。
// 调用方必须已持有 raidRewardCommitMu。
func (s *Service) storeRaidRewardCompletionLocked(
	key runtimeRaidRewardCompletionKey,
	completion runtimeRaidSettlementRewardCompletion,
) {
	if s.raidRewardCompleted == nil {
		s.raidRewardCompleted = make(map[runtimeRaidRewardCompletionKey]runtimeRaidSettlementRewardCompletion)
	}
	if _, exists := s.raidRewardCompleted[key]; !exists {
		s.raidRewardCompletedOrder = append(s.raidRewardCompletedOrder, key)
	}
	s.raidRewardCompleted[key] = completion
	for len(s.raidRewardCompleted) > raidRewardJournalLimit &&
		len(s.raidRewardCompletedOrder) > 0 {
		oldest := s.raidRewardCompletedOrder[0]
		s.raidRewardCompletedOrder = s.raidRewardCompletedOrder[1:]
		delete(s.raidRewardCompleted, oldest)
	}
}

// storeRaidRewardPublicationLocked 同上，作用于 raidRewardPublished。
// 调用方必须已持有 raidRewardFlowMu。
func (s *Service) storeRaidRewardPublicationLocked(
	key runtimeRaidRewardCompletionKey,
	publication *runtimeRaidRewardPublication,
) {
	if s.raidRewardPublished == nil {
		s.raidRewardPublished = make(map[runtimeRaidRewardCompletionKey]*runtimeRaidRewardPublication)
	}
	if _, exists := s.raidRewardPublished[key]; !exists {
		s.raidRewardPublishedOrder = append(s.raidRewardPublishedOrder, key)
	}
	s.raidRewardPublished[key] = publication
	for len(s.raidRewardPublished) > raidRewardJournalLimit &&
		len(s.raidRewardPublishedOrder) > 0 {
		oldest := s.raidRewardPublishedOrder[0]
		s.raidRewardPublishedOrder = s.raidRewardPublishedOrder[1:]
		delete(s.raidRewardPublished, oldest)
	}
}
