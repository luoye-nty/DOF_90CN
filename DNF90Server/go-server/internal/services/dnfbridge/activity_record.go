package dnfbridge

// activity_record.go 提供审计记录的落盘与检索：点券消费、对话、副本、团本、
// 金币寄售、拍卖行、场景加载诊断（类别清单见 recordKinds）。
//
// 为什么是 JSONL 文件而不是数据库表
//
//	这些记录是纯追加、只读、按时间检索的日志型数据，不需要事务、不需要与角色数据强一致。
//	走仓储层要多写一套接口 + MySQL/SQLite/memory 三份实现（而那几处正是别人在改的文件），
//	收益却只是"能写 SQL"。JSONL 的好处是：字段可以随时加（"信息尽可能详细"这条要求由此满足）、
//	写入不阻塞玩法逻辑、跨库模式（MySQL/SQLite）行为完全一致、出问题直接用记事本就能看。
//
// 文件布局
//
//	<记录目录>/cera-YYYYMMDD-NNN.jsonl         点券消费
//	<记录目录>/chat-YYYYMMDD-NNN.jsonl         对话
//	<记录目录>/dungeon-YYYYMMDD-NNN.jsonl      副本
//	<记录目录>/raid-YYYYMMDD-NNN.jsonl         团本
//	<记录目录>/gold_consign-YYYYMMDD-NNN.jsonl 金币寄售
//	<记录目录>/auction-YYYYMMDD-NNN.jsonl      拍卖行
//	<记录目录>/scene-YYYYMMDD-NNN.jsonl        场景加载诊断
//
//	记录目录默认取 packet_log 的同级 logs/records（可由 DNFBRIDGE_RECORD_DIR 覆盖），
//	这样"日志放哪、记录就放哪"，不会在服务端目录里散落两处。
//
// 每行一个 JSON 对象，至少含 ts / kind 两个字段，其余由各埋点自行补充。
// 写入失败只记 warn，绝不影响玩法——记录是旁路，不能因为磁盘满就把玩家操作搞失败。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 记录类别。
const (
	recordKindCera    = "cera"
	recordKindChat    = "chat"
	recordKindDungeon = "dungeon"
	// recordKindRaid 是团本（攻坚战）专用记录。攻坚事件以前全部混在 packet_log 里，
	// 一天几 GB 的文本里翻几十条；单独成类后写 records/raid-*.jsonl，
	// 字段由 raid_log.go 统一组装（谁在等谁、名单/就绪人数、驱动者、失败原因）。
	recordKindRaid = "raid"
	// recordKindGoldConsign 是金币寄售（拍卖行里的金币档位面板）专用记录。
	//
	// 为什么金币寄售不并进拍卖行记录：两者共用 op183/184/185 三个操作码，
	// 但一笔寄售**不产生**拍卖行的 listing/竞价/流拍概念（没有押金、没有竞拍、
	// 没有一口价，只有档位金币与点券标价），字段集合几乎不重叠；混在一个文件里，
	// 运营查"这单金币是谁卖给谁的"要先按 mode 过滤，反而更难读。
	recordKindGoldConsign = "gold_consign"
	// recordKindAuction 是拍卖行（物品竞拍 / 一口价）专用记录。
	recordKindAuction = "auction"
	// recordKindScene 是场景加载/引导包（scene bootstrap tail）诊断专用记录。
	//
	// 为什么单独成类：这批诊断（scene_tail_deferred 一族）**与团本毫无关系**，
	// 原本却写进了 raid 类别，于是出现两个后果：
	//  1. 只要有一个玩家在选人选场景/进图，每秒就会产生好几条，把真正的团本事件
	//     彻底淹没在「记录查询 → 团本记录」里，运营根本翻不到攻坚那几十条；
	//  2. 更要命的是轮转——记录器对每个类别只保留最近 recordRetentionPerKind 个文件，
	//     这批高频写入会飞快地把 raid-*.jsonl 顶到上限，把真正的团本记录连同旧文件
	//     一起挤出保留窗口，等于把团本记录的可查性毁掉。
	//
	// 之所以是"搬家"而不是"删除"：这批埋点当初就是为了排查"卡在加载"加的，
	// 而 packet_log 在高负载下实测会丢约 25% 的行，不能只靠它。所以给它们一个
	// 自己的类别，让团本记录恢复成只有团本事件，诊断本身一条不少。
	recordKindScene = "scene"
)

// recordKinds 全部受支持的类别（顺序即 UI 展示顺序）。
//
// 顺序即管理端「记录查询」里类别下拉的顺序：把两个交易类记录排在最后，
// 前面的点券/对话/副本/团本四个类别的位置保持不变（运营已经按老顺序找惯了）。
// recordKindScene 排在**最末**：它是诊断类而非玩法/交易类，不该插在运营已经
// 找惯的团本与交易类别中间。
var recordKinds = []string{
	recordKindCera,
	recordKindChat,
	recordKindDungeon,
	recordKindRaid,
	recordKindGoldConsign,
	recordKindAuction,
	recordKindScene,
}

// recordKindTitles 类别中文名（GM 接口/管理端展示用）。
var recordKindTitles = map[string]string{
	recordKindCera:        "点券消费记录",
	recordKindChat:        "对话记录",
	recordKindDungeon:     "副本记录",
	recordKindRaid:        "团本记录",
	recordKindGoldConsign: "金币寄售记录",
	recordKindAuction:     "拍卖行记录",
	recordKindScene:       "场景加载诊断",
}

// recordRetentionPerKind 每个类别保留的最近文件数。
const recordRetentionPerKind = 14

// recordMaxFileBytes 单个记录文件的大小上限（超过就换新文件）。
const recordMaxFileBytes = 32 << 20

// recordQueryReadBudgetBytes 查询时最多回读的字节数（防止一次把几十天记录全读进内存）。
const recordQueryReadBudgetBytes = 64 << 20

// recordMaxQueueDepth 写入队列深度。
//
// 队列存在的理由：聊天、买商城、进出副本这些埋点全都在玩法主路径上，其中一部分
// （例如副本运行时的 retire）还握着 session.dungeon.mu。直接同步写盘会在这些锁内
// 引入磁盘延迟。改为"调用方只入队、单个协程负责写盘"后，主路径的代价就是一次
// channel 发送。
const recordMaxQueueDepth = 4096

// activityRecorder 追加写 JSONL 记录器。零值不可用，用 newActivityRecorder 构造。
type activityRecorder struct {
	mu       sync.Mutex
	dir      string
	handles  map[string]*os.File
	sizes    map[string]int64
	days     map[string]string
	seq      map[string]int
	disabled map[string]bool

	logger *slog.Logger
	// queue 里放的是已经序列化好的整行（含换行）与类别，写入协程不再做 JSON 编码。
	queue   chan recordJob
	barrier chan chan struct{}
	done    chan struct{}
	// closed 置位后新的记录不再入队（关闭途中的记录允许丢弃，见 Close）。
	closed    atomic.Bool
	closeOnce sync.Once
	wg        sync.WaitGroup
	written   atomic.Int64
	dropped   atomic.Int64
}

// recordJob 一条待落盘记录。kind 单独带着，不去解析 JSON——map 的 JSON 键是字典序，
// "kind" 并不在行首，值里也可能出现同样的字面量。
type recordJob struct {
	kind string
	line []byte
}

// newActivityRecorder 在 dir 下创建记录器；dir 为空时返回 nil（调用方按"记录关闭"处理）。
func newActivityRecorder(dir string, logger *slog.Logger) *activityRecorder {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if logger != nil {
			logger.Warn("dnfbridge activity record dir unavailable", "dir", dir, "error", err)
		}
		return nil
	}
	recorder := &activityRecorder{
		dir:      dir,
		handles:  make(map[string]*os.File),
		sizes:    make(map[string]int64),
		days:     make(map[string]string),
		seq:      make(map[string]int),
		disabled: make(map[string]bool),
		logger:   logger,
		queue:    make(chan recordJob, recordMaxQueueDepth),
		barrier:  make(chan chan struct{}),
		done:     make(chan struct{}),
	}
	recorder.wg.Add(1)
	go recorder.run()
	return recorder
}

// run 是唯一的写盘协程：串行消费队列，并在屏障请求时先把已排队内容落盘再应答。
//
// 单写者带来的另一个好处是文件句柄、轮转状态、序号全部由它独占，不需要跨协程同步。
func (r *activityRecorder) run() {
	defer r.wg.Done()
	for {
		select {
		case job := <-r.queue:
			r.writeLine(job)
		case ack := <-r.barrier:
			// 屏障请求可能与队列同时就绪，所以这里必须再排空一次，
			// 否则"查询前 flush"会漏掉刚入队但还没被选中的记录。
			r.drainQueue()
			close(ack)
		case <-r.done:
			r.drainQueue()
			return
		}
	}
}

// drainQueue 把当前队列里已排队的记录全部写完（非阻塞）。
func (r *activityRecorder) drainQueue() {
	for {
		select {
		case job := <-r.queue:
			r.writeLine(job)
		default:
			return
		}
	}
}

// flush 等待队列排空，供查询/元信息接口使用，保证刚发生的操作能立刻查到。
func (r *activityRecorder) flush() {
	if r == nil || r.closed.Load() {
		return
	}
	ack := make(chan struct{})
	select {
	case r.barrier <- ack:
	case <-r.done:
		return
	}
	select {
	case <-ack:
	case <-r.done:
	}
}

// writeLine 落一行记录（只由写盘协程调用）。
func (r *activityRecorder) writeLine(job recordJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kind := job.kind
	if r.disabled[kind] {
		return
	}
	handle, err := r.handleLocked(kind, r.logger)
	if err != nil {
		r.disabled[kind] = true
		if r.logger != nil {
			r.logger.Warn("dnfbridge activity record disabled", "kind", kind, "dir", r.dir, "error", err)
		}
		return
	}
	written, err := handle.Write(job.line)
	if err != nil {
		r.disabled[kind] = true
		if r.logger != nil {
			r.logger.Warn("dnfbridge activity record write failed", "kind", kind, "error", err)
		}
		return
	}
	r.sizes[kind] += int64(written)
	r.written.Add(1)
}

// Dir 记录目录（供 GM 接口展示）。
func (r *activityRecorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// record 追加一条记录。kind 必须是 recordKinds 之一；fields 里的 ts/kind 会被覆盖成权威值。
//
// 这个方法会被玩法主路径调用（聊天、购买、进副本），所以它必须：
//   - 不 panic（任何序列化失败都直接丢弃）；
//   - 不碰磁盘（只做一次 channel 发送，真正写盘的是 run 协程）；
//   - 不丢记录：队列满时退化为阻塞等待，宁可让调用方多等一会儿，
//     也不允许审计记录静默消失（那种情况只在磁盘彻底卡死时才会出现）。
func (r *activityRecorder) record(kind string, fields map[string]any, logger *slog.Logger) {
	if r == nil || !isRecordKind(kind) || r.closed.Load() {
		return
	}
	entry := make(map[string]any, len(fields)+3)
	for key, value := range fields {
		entry[key] = value
	}
	entry["ts"] = time.Now().Format(time.RFC3339Nano)
	entry["kind"] = kind
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')

	job := recordJob{kind: kind, line: line}
	select {
	case r.queue <- job:
		return
	default:
	}
	select {
	case r.queue <- job:
	case <-r.done:
		// 记录器正在关闭：允许丢弃这一条，绝不让调用方永久阻塞。
		r.dropped.Add(1)
		if logger != nil {
			logger.Warn("dnfbridge activity record dropped during shutdown", "kind", kind)
		}
	}
}

// handleLocked 返回当前应写入的文件句柄，必要时轮转（按天 + 按大小）。
func (r *activityRecorder) handleLocked(kind string, logger *slog.Logger) (*os.File, error) {
	today := time.Now().Format("20060102")
	needRotate := r.handles[kind] == nil ||
		r.days[kind] != today ||
		r.sizes[kind] >= recordMaxFileBytes
	if !needRotate {
		return r.handles[kind], nil
	}
	if r.handles[kind] != nil {
		_ = r.handles[kind].Close()
		r.handles[kind] = nil
	}
	if r.days[kind] != today {
		// 跨天重置序号，让文件名回到 -001 起。
		r.seq[kind] = 0
	}
	r.days[kind] = today
	for attempt := 0; attempt < 1000; attempt++ {
		r.seq[kind]++
		name := fmt.Sprintf("%s-%s-%03d.jsonl", kind, today, r.seq[kind])
		path := filepath.Join(r.dir, name)
		if _, statErr := os.Stat(path); statErr == nil {
			// 该序号已被占用（例如上次运行留下的），继续往下找。
			continue
		}
		handle, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		info, statErr := handle.Stat()
		size := int64(0)
		if statErr == nil {
			size = info.Size()
		}
		r.handles[kind] = handle
		r.sizes[kind] = size
		r.pruneLocked(kind, logger)
		return handle, nil
	}
	return nil, fmt.Errorf("no free record file name for kind %s", kind)
}

// pruneLocked 只保留该类别最近的 recordRetentionPerKind 个文件。
func (r *activityRecorder) pruneLocked(kind string, logger *slog.Logger) {
	matches, err := filepath.Glob(filepath.Join(r.dir, kind+"-*.jsonl"))
	if err != nil || len(matches) <= recordRetentionPerKind {
		return
	}
	// 文件名里带日期与序号，字典序即时间序。
	sort.Strings(matches)
	keepFrom := len(matches) - recordRetentionPerKind
	for _, path := range matches[:keepFrom] {
		if err := os.Remove(path); err != nil && logger != nil {
			logger.Warn("dnfbridge activity record prune failed", "path", path, "error", err)
		}
	}
}

// Close 停止写盘协程并关闭全部文件句柄（服务停止时调用）。可重复调用。
//
// 关闭顺序：先置 closed（新记录不再入队）→ 通知写盘协程排空队列后退出 → 关句柄。
// 关闭瞬间仍可能有协程已经入队成功但写盘协程已经退出，这一条会丢；服务停止时
// 玩家早已离线，这个窗口可以接受，而"先落盘再退出"的顺序保证不会丢正常的记录。
func (r *activityRecorder) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		close(r.done)
		r.wg.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		for kind, handle := range r.handles {
			if handle != nil {
				_ = handle.Close()
			}
			delete(r.handles, kind)
		}
	})
}

// stats 返回写入/丢弃计数（供 GM 元信息接口展示健康度）。
func (r *activityRecorder) stats() (written, dropped int64) {
	if r == nil {
		return 0, 0
	}
	return r.written.Load(), r.dropped.Load()
}

// defaultRecordDirFor 由包日志路径推导记录目录：与日志同级的 records/。
// 包日志默认是 "logs/packet_log.txt"，于是记录默认落在 "logs/records"。
func defaultRecordDirFor(packetLogPath string) string {
	dir := filepath.Dir(strings.TrimSpace(packetLogPath))
	if dir == "" || dir == "." {
		return filepath.Join("logs", "records")
	}
	return filepath.Join(dir, "records")
}
func isRecordKind(kind string) bool {
	for _, known := range recordKinds {
		if kind == known {
			return true
		}
	}
	return false
}

// ================= 查询 =================

// recordQuery 记录查询条件。零值表示不过滤。
//
// Offset 是"跳过最新的多少条匹配记录"，配合 Limit 做分页：一次请求返回的条数必须有上限
// （否则一次查询可能吐出几十 MB JSON，工具侧的读上限与内存都吃不消），
// 想"看全部"就由调用方翻页，直到返回结果里的 Limited=false。
type recordQuery struct {
	Kind          string
	Limit         int
	Offset        int
	Since         time.Time
	Until         time.Time
	CharacterID   string
	CharacterName string
	AccountID     string
	Keyword       string
}

// recordFileInfo 单个记录文件的元信息（管理端用来展示"记录落到哪、有多大"）。
type recordFileInfo struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
	Lines   int    `json:"lines,omitempty"`
}

// queryRecords 倒序（最新在前）检索记录。
//
// 实现方式：按文件名从新到旧遍历该类别文件，整体读入后逐行反向匹配。
// 有总字节预算（recordQueryReadBudgetBytes）兜底，避免一次请求把几十天记录读进内存；
// 命中 limit 条即停，query.Offset 表示跳过最新的多少条命中（分页）。
func (r *activityRecorder) queryRecords(query recordQuery) ([]map[string]any, int, error) {
	if r == nil || !isRecordKind(query.Kind) {
		return nil, 0, fmt.Errorf("unknown record kind %q", query.Kind)
	}
	// 先排空队列：调用方刚做完的操作要能立刻查到（这一点在管理端"删完就查"时尤其明显）。
	r.flush()
	if query.Limit <= 0 {
		query.Limit = 200
	}
	if query.Limit > 2000 {
		query.Limit = 2000
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	matches, err := filepath.Glob(filepath.Join(r.dir, query.Kind+"-*.jsonl"))
	if err != nil {
		return nil, 0, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))

	out := make([]map[string]any, 0, query.Limit)
	scanned := 0
	skipped := 0
	budget := int64(recordQueryReadBudgetBytes)
	for _, path := range matches {
		if len(out) >= query.Limit || budget <= 0 {
			break
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		budget -= info.Size()
		lines, readErr := readRecordLines(path)
		if readErr != nil {
			continue
		}
		for index := len(lines) - 1; index >= 0; index-- {
			line := strings.TrimSpace(lines[index])
			if line == "" {
				continue
			}
			scanned++
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) != nil {
				continue
			}
			if !recordMatches(entry, query) {
				continue
			}
			// 分页：跳过更新的那些命中条数后，才开始收集本页。
			if skipped < query.Offset {
				skipped++
				continue
			}
			out = append(out, entry)
			if len(out) >= query.Limit {
				break
			}
		}
	}
	return out, scanned, nil
}

// listRecordFiles 列出记录目录里全部记录文件（新→旧），供管理端展示。
func (r *activityRecorder) listRecordFiles() []recordFileInfo {
	if r == nil {
		return nil
	}
	r.flush()
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	out := make([]recordFileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		kind := entry.Name()
		if index := strings.IndexByte(kind, '-'); index > 0 {
			kind = kind[:index]
		}
		out = append(out, recordFileInfo{
			Name:    entry.Name(),
			Kind:    kind,
			Size:    info.Size(),
			ModTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// readRecordLines 读一个记录文件的全部行。
// 单行设上限，避免被一行超长内容（例如超长聊天）拖垮内存。
func readRecordLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lines := make([]string, 0, 1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// recordMatches 判断一条记录是否满足查询条件。
//
// 字段名对不上就跳过该条件（而不是判定不匹配）：三类记录字段并不完全相同，
// 要求所有字段都存在才能查，会让"按角色查"在部分记录上莫名失效。
func recordMatches(entry map[string]any, query recordQuery) bool {
	if query.CharacterID != "" && recordString(entry, "character_id") != query.CharacterID {
		return false
	}
	if query.CharacterName != "" && !strings.EqualFold(recordString(entry, "character_name"), query.CharacterName) {
		return false
	}
	if query.AccountID != "" && recordString(entry, "account_id") != query.AccountID {
		return false
	}
	if !query.Since.IsZero() || !query.Until.IsZero() {
		stamp, err := time.Parse(time.RFC3339Nano, recordString(entry, "ts"))
		if err != nil {
			return false
		}
		if !query.Since.IsZero() && stamp.Before(query.Since) {
			return false
		}
		if !query.Until.IsZero() && stamp.After(query.Until) {
			return false
		}
	}
	if query.Keyword != "" {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return false
		}
		if !strings.Contains(strings.ToLower(string(encoded)), strings.ToLower(query.Keyword)) {
			return false
		}
	}
	return true
}

// recordString 读一个记录里的字符串字段（不存在或类型不符返回空串）。
func recordString(entry map[string]any, key string) string {
	value, ok := entry[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}
