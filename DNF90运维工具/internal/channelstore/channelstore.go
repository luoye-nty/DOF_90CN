// Package channelstore 负责读写 DNF90 服务端的活配置 channel_info.etc，
// 提供"按频道号开启/关闭"这一运维动作的落盘实现。
//
// 为什么是改文件而不是调服务端接口：
//
//	go-server 的 channelcatalog 是"从 channel_info.etc 派生的只读目录"，
//	频道目录（Catalog）与下发给客户端的频道脚本（channelScript）在 Service.Start
//	时一次性构建并缓存（internal/services/dnfbridge/service.go loadChannelAssets），
//	运行期没有任何 setter。因此关掉一个频道的做法就是把它的行从 [server] 段里删掉，
//	服务端下次启动时自然既不再监听该频道、也不再把它写进客户端频道表——
//	玩家重登后在频道列表里就看不到这个频道了。
//
// 生效条件（UI 必须如实提示）：需要重启 DNF90Server。改动只落盘，不热更。
//
// 文件保真原则：
//
//	文件里除 [server] 段之外还有大量 [dungeon] 段、注释与缩进，且第 1 行带 UTF-8 BOM。
//	因此本包只做"整行增删"：不重新序列化整个文件，不碰其它行，换行风格（CRLF/LF）
//	与 BOM 原样保留，行内缩进也保持原样。频道行文本另存侧车清单，用于无损恢复。
package channelstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// defaultChannelInfoRel instance.json 缺失或未配置时的回退相对路径（相对 <root>/runtime）。
	defaultChannelInfoRel = "data/dnf/channel_info.etc"
	// instanceRel instance.json 相对服务端根目录的位置。
	instanceRel = "config/instance.json"
	// sidecarSuffix 频道行清单侧车文件后缀：记录每个频道号最后一次已知的原始行文本，
	// 关闭后再开启时用它逐字还原（含原始缩进），避免"开启了但行文本走样"。
	sidecarSuffix = ".channels.json"
	// bootstrapChannelID / defaultGameChannelID 与 go-server dnfenum 对齐：
	// 19=BootstrapChannelID、11=DefaultGameChannelID。
	//
	// 它们现在只用于**标记与展示**（Entry.Required → 表格"操作权限"列），不再是关闭禁令：
	// 服务端自 2026-09-30 起把"缺 19 就起不来""缺 11 就拒绝热重载"都降级为告警
	// （DNFBRIDGE_CHANNEL_TABLE_STRICT=1 恢复），所以运维可以只留任意一个频道。
	// 保留这两个号的特殊标记，是为了在 UI 上提醒"关掉它们会有一批客户端停在选频道界面"。
	bootstrapChannelID   = 19
	defaultGameChannelID = 11
	// ordinaryChannelType 普通城镇频道类型（channel_info.etc 里的 type 字段）。
	// 与文件头注释"普通区域使用 type 11"以及 channelcatalog.isResidentChannel 的判定一致。
	ordinaryChannelType = 11
	// specialAreaChannelType 特殊区域频道类型：线上表里 121（ancient 远古）与 171（deathtower
	// 绝望之塔）都是 type 22。服主要求**任何时刻必须至少有一个 type 22 频道处于开启状态**，
	// 所以它是"最后一个特殊区域频道"保护规则的判据（见 checkDisableTargets）。
	specialAreaChannelType = 22
)

// ErrChannelInfoMissing channel_info.etc 不存在。
var ErrChannelInfoMissing = errors.New("channel_info.etc 不存在")

// Entry 一个频道的状态。
type Entry struct {
	ID       int    `json:"id"`
	ServerID int    `json:"server_id"`
	Type     int    `json:"type"`
	Group    string `json:"group"`
	NameKey  string `json:"name_key"`
	Enabled  bool   `json:"enabled"`
	// Required 标记默认/引导频道（19 引导、11 默认城镇）。2026-09-30 起它**不再是关闭禁令**，
	// 只用于 UI 提醒"关掉后会有一批客户端停在选频道界面"（服务端已把两条硬校验降级为告警）。
	Required bool `json:"required"`
	// Ordinary 是否普通城镇频道（type 11）。
	Ordinary bool `json:"ordinary"`
	// SpecialArea 是否特殊区域频道（type 22，线上为 121 远古 / 171 绝望之塔）。
	// 它受"任何时刻至少留一个开启"的保护（checkDisableTargets）。
	SpecialArea bool `json:"special_area"`
}

// Status channel_info.etc 的当前频道状态快照。
type Status struct {
	Path      string  `json:"path"`
	ServerID  int     `json:"server_id"`
	Total     int     `json:"total"`
	Enabled   int     `json:"enabled"`
	Disabled  int     `json:"disabled"`
	Channels  []Entry `json:"channels"`
	Backup    string  `json:"backup,omitempty"`
	CheckedAt string  `json:"checked_at"`
}

// manifest 侧车清单：频道号 → 原始行文本。
type manifest struct {
	// Rows 频道号（十进制字符串）→ 该频道在 channel_info.etc 里最后一次出现时的原始整行文本。
	Rows map[string]string `json:"rows"`
	// Type / Group / NameKey / ServerID 一并记住，用于在"行文本丢失"时重建并给出可读信息。
	Meta map[string]manifestMeta `json:"meta"`
}

type manifestMeta struct {
	ServerID int    `json:"server_id"`
	Type     int    `json:"type"`
	Group    string `json:"group"`
	NameKey  string `json:"name_key"`
}

// fileLine 一行原始文本；Text 不含换行，EOL 保留原样（"\r\n" / "\n" / ""）。
type fileLine struct {
	Text string
	EOL  string
}

// field 行内一个字段及其在行内的字节区间（用于整块切出频道记录的原始文本）。
type field struct {
	Text       string
	Start, End int
}

// record 行内一条频道记录。
type record struct {
	id         int
	serverID   int
	typeID     int
	group      string
	nameKey    string
	lineIndex  int
	fieldStart int
	fieldEnd   int
	raw        string // 原始字段串（保留原始空格与反引号），恢复时逐字写回
}

// applyMu 串行化"读-改-写"整段流程。
//
// 为什么必须在包内加锁，而不是靠调用方：
//
//	同一个 channel_info.etc 有两条完全独立的写入链——
//	  ① 本地「频道管理」页（channel_tab.go → App.channelsApply），只受 runAsync 的 busy 约束；
//	  ② 远程管理工具（/manage/v1/channels/apply → Bridge.ChannelsApply → App.channelsApply），
//	     走的是 manage 的 HTTPS 处理协程，既不查 busy 也不持任何锁。
//	两者并发时：一方会把另一方写了一半的临时文件 Rename 成 channel_info.etc，
//	发布出**截断/半行**的频道表；而这是服务端启动时构建频道目录与客户端频道表所依赖的文件，
//	半行会让频道构建失败或整段丢频道。即便不截断，也会出现"后写覆盖前写"的静默丢失。
//
// 锁只包住 Load/Apply 这类会写盘的入口，读接口不加锁（读到的要么是改名前的完整旧文件，
// 要么是改名后的完整新文件——rename 在 Windows 上对同目录目标是原子的）。
var applyMu sync.Mutex

// ResolvePath 解析服务端根目录下 channel_info.etc 的绝对路径。
// 优先读 <root>/runtime/config/instance.json 的 game.channelInfoPath，缺失时回退默认相对路径。
func ResolvePath(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("未配置服务端根目录")
	}
	runtimeDir := filepath.Join(root, "runtime")
	rel := defaultChannelInfoRel
	raw, err := os.ReadFile(filepath.Join(runtimeDir, filepath.FromSlash(instanceRel)))
	if err == nil {
		var probe struct {
			Game struct {
				ChannelInfoPath string `json:"channelInfoPath"`
			} `json:"game"`
		}
		if json.Unmarshal(raw, &probe) == nil {
			if configured := strings.TrimSpace(probe.Game.ChannelInfoPath); configured != "" {
				rel = configured
			}
		}
	}
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel), nil
	}
	return filepath.Join(runtimeDir, filepath.FromSlash(rel)), nil
}

// Load 读取并解析当前频道状态，同时把已知频道行合并进侧车清单（便于日后无损恢复）。
func Load(root string) (Status, error) {
	path, err := ResolvePath(root)
	if err != nil {
		return Status{}, err
	}
	return LoadFile(path)
}

// LoadFile 按绝对路径读取频道状态。
func LoadFile(path string) (Status, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Status{}, fmt.Errorf("%w: %s", ErrChannelInfoMissing, path)
		}
		return Status{}, err
	}
	lines := splitLines(raw)
	records, serverID := collectRecords(lines)
	// 先把当前文件里的频道行并入侧车清单：这一步必须发生在任何删除之前，
	// 否则被关掉的频道行文本就丢了，之后再也开不回来。
	_ = mergeManifest(path, records)
	entries := buildEntries(path, records)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })

	status := Status{
		Path:      path,
		ServerID:  serverID,
		Total:     len(entries),
		Channels:  entries,
		CheckedAt: nowStamp(),
	}
	for _, entry := range entries {
		if entry.Enabled {
			status.Enabled++
		} else {
			status.Disabled++
		}
	}
	return status, nil
}

// buildEntries 合并"文件里存在的频道"（启用）与"只存在于侧车清单的频道"（已关闭）。
func buildEntries(path string, records []record) []Entry {
	entries := make([]Entry, 0, len(records)+8)
	seen := make(map[int]struct{}, len(records))
	for _, rec := range records {
		seen[rec.id] = struct{}{}
		entries = append(entries, entryOf(rec))
	}
	rows, meta := readManifest(path)
	ids := make([]int, 0, len(rows))
	for key := range rows {
		id, err := strconv.Atoi(key)
		if err != nil || id <= 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		info := meta[strconv.Itoa(id)]
		entries = append(entries, Entry{
			ID:          id,
			ServerID:    info.ServerID,
			Type:        info.Type,
			Group:       info.Group,
			NameKey:     info.NameKey,
			Enabled:     false,
			Required:    id == bootstrapChannelID || id == defaultGameChannelID,
			Ordinary:    info.Type == ordinaryChannelType,
			SpecialArea: info.Type == specialAreaChannelType,
		})
	}
	return entries
}

// Apply 把 channelIDs 批量设为 enabled（true=开启 / false=关闭）。
// 返回本次真正改动的频道号与改动后的完整状态；任何保护规则不满足时直接报错且不落盘。
func Apply(root string, channelIDs []int, enabled bool) ([]int, Status, error) {
	path, err := ResolvePath(root)
	if err != nil {
		return nil, Status{}, err
	}
	return ApplyFile(path, channelIDs, enabled)
}

// ApplyFile 按绝对路径执行开关。
func ApplyFile(path string, channelIDs []int, enabled bool) ([]int, Status, error) {
	// 读-改-写整段串行化，避免本地页与远程管理端并发改出截断文件（见 applyMu 说明）。
	applyMu.Lock()
	defer applyMu.Unlock()

	if len(channelIDs) == 0 {
		return nil, Status{}, errors.New("未指定要操作的频道")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Status{}, fmt.Errorf("%w: %s", ErrChannelInfoMissing, path)
		}
		return nil, Status{}, err
	}
	lines := splitLines(raw)
	records, serverID := collectRecords(lines)
	// 关闭动作会删掉行，所以必须先把行文本并入侧车清单再改文件。
	if err := mergeManifest(path, records); err != nil {
		return nil, Status{}, fmt.Errorf("写入频道行清单失败（开启时要用它还原行文本）: %w", err)
	}
	known := make(map[int]record, len(records))
	for _, rec := range records {
		known[rec.id] = rec
	}

	// 目标集合去重 + 排序，保证结果稳定、日志可读。
	// 注意要在归一化之后再判空：只传了 0/负数这类非法频道号时 targets 会是空集，
	// 若按原始入参判空就会"静默什么都不做"地返回成功，调用方看不出问题。
	targets := uniqueSorted(channelIDs)
	if len(targets) == 0 {
		return nil, Status{}, fmt.Errorf("没有有效的频道号可操作（收到 %v；频道号必须为正整数）", channelIDs)
	}
	if enabled {
		if err := checkEnableTargets(path, targets, known); err != nil {
			return nil, Status{}, err
		}
	} else {
		if err := checkDisableTargets(targets, known); err != nil {
			return nil, Status{}, err
		}
	}

	changed := make([]int, 0, len(targets))
	backup := ""
	// 先算出新行集合，全部成功后再落盘，避免中途报错留下半成品。
	newLines, applied, err := rewrite(lines, known, targets, enabled, serverID, path)
	if err != nil {
		return nil, Status{}, err
	}
	if len(applied) == 0 {
		status, loadErr := LoadFile(path)
		return nil, status, loadErr
	}
	if backup, err = backupFile(path, raw); err != nil {
		return nil, Status{}, fmt.Errorf("备份 channel_info.etc 失败: %w", err)
	}
	if err := writeLines(path, newLines, hasBOM(raw)); err != nil {
		return nil, Status{}, fmt.Errorf("写入 channel_info.etc 失败: %w", err)
	}
	changed = applied
	status, err := LoadFile(path)
	if err != nil {
		return changed, Status{}, err
	}
	status.Backup = backup
	return changed, status, nil
}

// ================= 解析 =================

// splitLines 按 '\n' 切行，保留每行原始换行符（含 CRLF 判定）。
//
// BOM 在这里被**剥离**，而不是留在第 1 行文本里：
// writeLines 会按 hasBOM(原始字节) 单独补一次 BOM，如果 splitLines 又把 BOM 算进 lines[0].Text，
// 每保存一次就会多出一个 BOM（实测"关闭→开启"一轮后头部变成 3 个 BOM）。
// 单一事实源：BOM 的存在与否只由 writeLines 的 bom 参数表达。
func splitLines(raw []byte) []fileLine {
	text := strings.TrimPrefix(string(raw), "\ufeff")
	lines := make([]fileLine, 0, 700)
	for len(text) > 0 {
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			lines = append(lines, fileLine{Text: text})
			break
		}
		body := text[:idx]
		eol := "\n"
		if strings.HasSuffix(body, "\r") {
			body = strings.TrimSuffix(body, "\r")
			eol = "\r\n"
		}
		lines = append(lines, fileLine{Text: body, EOL: eol})
		text = text[idx+1:]
	}
	return lines
}

func hasBOM(raw []byte) bool {
	return len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF
}

// writeLines 把行集合写回文件；bom=true 时在最前补回 UTF-8 BOM。
//
// 用唯一临时名而非固定 "<path>.tmp"：固定名在并发写时会被 O_TRUNC 互相截断，
// 而 Rename 是原子的、对同目录目标生效——所以只要临时名唯一，读到的永远是完整文件。
func writeLines(path string, lines []fileLine, bom bool) error {
	var builder strings.Builder
	if bom {
		builder.WriteString("\ufeff")
	}
	for _, line := range lines {
		builder.WriteString(line.Text)
		builder.WriteString(line.EOL)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.WriteString(builder.String()); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// backupFile 落盘前备份原文件（首次备份不覆盖同名文件）。
func backupFile(path string, raw []byte) (string, error) {
	target := fmt.Sprintf("%s.bak_%s", path, backupStamp())
	if _, err := os.Stat(target); err == nil {
		return target, nil
	}
	if err := os.WriteFile(target, raw, 0o644); err != nil {
		return "", err
	}
	return target, nil
}

// collectRecords 扫描全文，返回全部频道记录（按文件出现顺序）与 [server] 段声明的 server id。
func collectRecords(lines []fileLine) ([]record, int) {
	records := make([]record, 0, 160)
	serverID := 1
	activeServer := 1
	inServer := false
	serverSeen := false
	for index, line := range lines {
		fields := splitFieldsWithOffsets(line.Text)
		if len(fields) > 0 && strings.EqualFold(fields[0].Text, "[server]") {
			serverSeen = true
			inServer = true
			if len(fields) >= 2 {
				if parsed, err := strconv.Atoi(fields[1].Text); err == nil && parsed >= 0 {
					activeServer = parsed
					serverID = parsed
				}
			}
			continue
		}
		if strings.EqualFold(strings.TrimSpace(line.Text), "[/server]") {
			inServer = false
			continue
		}
		if !serverSeen || !inServer {
			continue
		}
		start := 0
		if len(fields) >= 5 && isInt(fields[0].Text) && isChannelStart(fields, 1) {
			activeServer, _ = strconv.Atoi(fields[0].Text)
			start = 1
		}
		firstOnLine := true
		for start < len(fields) {
			if !isChannelStart(fields, start) {
				break
			}
			end := len(fields)
			for next := start + 4; next < len(fields); next++ {
				if isChannelStart(fields, next) {
					end = next
					break
				}
			}
			recordFields := fields[start:end]
			if id, err := strconv.Atoi(recordFields[0].Text); err == nil && id > 0 {
				typeID, _ := strconv.Atoi(recordFields[2].Text)
				records = append(records, record{
					id:         id,
					serverID:   activeServer,
					typeID:     typeID,
					group:      normalizeGroup(recordFields[3].Text),
					nameKey:    normalizeNameKey(recordFields[1].Text),
					lineIndex:  index,
					fieldStart: start,
					fieldEnd:   end,
					raw:        recordRaw(line.Text, recordFields, start, firstOnLine),
				})
			}
			firstOnLine = false
			start = end
		}
	}
	return records, serverID
}

func entryOf(rec record) Entry {
	return Entry{
		ID:          rec.id,
		ServerID:    rec.serverID,
		Type:        rec.typeID,
		Group:       rec.group,
		NameKey:     rec.nameKey,
		Enabled:     true, // collectRecords 只收集"文件里存在"的行，存在即启用
		Required:    rec.id == bootstrapChannelID || rec.id == defaultGameChannelID,
		Ordinary:    rec.typeID == ordinaryChannelType,
		SpecialArea: rec.typeID == specialAreaChannelType,
	}
}

// ================= 改写 =================

// serverSectionMask 标记每一行是否落在 [server] 段**内部**（段头 [server] 行与 [/server] 行本身为 false）。
//
// 存在的理由：频道记录的识别只看"字段形态"（int `str` int `str`），这在多段文件里并不唯一。
// 关闭与插入定位都必须限定在 [server] 段内，否则别的段一旦出现同形态的行，
// 就会发生"从 [dungeon] 段删行"或"把恢复的频道行插到 [server] 之外"（频道看起来开了却永不生效）。
func serverSectionMask(lines []fileLine) []bool {
	mask := make([]bool, len(lines))
	inServer := false
	for index, line := range lines {
		trimmed := strings.TrimSpace(line.Text)
		if fields := splitFieldsWithOffsets(trimmed); len(fields) > 0 && strings.EqualFold(fields[0].Text, "[server]") {
			inServer = true
			continue
		}
		if strings.EqualFold(trimmed, "[/server]") {
			inServer = false
			continue
		}
		mask[index] = inServer
	}
	return mask
}

// rewrite 生成新的行集合。
func rewrite(
	lines []fileLine,
	known map[int]record,
	targets []int,
	enabled bool,
	serverID int,
	path string,
) ([]fileLine, []int, error) {
	out := append([]fileLine(nil), lines...)
	changed := make([]int, 0, len(targets))

	if !enabled {
		// 关闭：从行里摘掉这些记录；整行只剩这一条时直接删行。
		remove := make(map[int]struct{}, len(targets))
		for _, id := range targets {
			if _, ok := known[id]; ok {
				remove[id] = struct{}{}
			}
		}
		if len(remove) == 0 {
			return out, nil, nil
		}
		// 只在 [server] 段内做行判定。不能对每一行都尝试解析频道记录：
		// 别的段（当前文件里的 [dungeon] 段是每字段一行，不会命中）一旦出现
		// `int \`str\` int \`str\`` 形态的行，就会被当成频道记录删掉或改写。
		mask := serverSectionMask(out)
		kept := make([]fileLine, 0, len(out))
		for index, line := range out {
			if !mask[index] {
				kept = append(kept, line)
				continue
			}
			records := recordsOnLine(line, index)
			if len(records) == 0 {
				kept = append(kept, line)
				continue
			}
			remaining := make([]record, 0, len(records))
			for _, rec := range records {
				if _, drop := remove[rec.id]; drop {
					if !containsInt(changed, rec.id) {
						changed = append(changed, rec.id)
					}
					continue
				}
				remaining = append(remaining, rec)
			}
			switch {
			case len(remaining) == 0:
				// 整行都是被关掉的频道：删除整行（连同它的换行）。
				continue
			case len(remaining) == len(records):
				kept = append(kept, line)
			default:
				// 打包行（一行多条记录）：保留剩余记录的原始文本，用单空格重连。
				texts := make([]string, 0, len(remaining))
				for _, rec := range remaining {
					texts = append(texts, rec.raw)
				}
				kept = append(kept, fileLine{Text: strings.Join(texts, " "), EOL: line.EOL})
			}
		}
		return kept, changed, nil
	}

	// 开启：把清单里的原始行插回 [server] 段（按频道号升序，与原文件排序一致）。
	manifestRows, manifestMeta := readManifest(path)
	pending := make([]record, 0, len(targets))
	for _, id := range targets {
		if _, exists := known[id]; exists {
			continue
		}
		raw, ok := manifestRows[strconv.Itoa(id)]
		if !ok || strings.TrimSpace(raw) == "" {
			return nil, nil, fmt.Errorf("频道 %d 的原始行文本不在侧车清单里（channel_info.etc.channels.json 被删或该频道从未在此文件中出现过），无法安全开启", id)
		}
		meta := manifestMeta[strconv.Itoa(id)]
		pending = append(pending, record{
			id:       id,
			serverID: meta.ServerID,
			typeID:   meta.Type,
			group:    meta.Group,
			nameKey:  meta.NameKey,
			raw:      raw,
		})
		changed = append(changed, id)
	}
	if len(pending) == 0 {
		return out, nil, nil
	}

	insertAt := -1
	for index, line := range out {
		if strings.EqualFold(strings.TrimSpace(line.Text), "[/server]") {
			insertAt = index
			break
		}
	}
	if insertAt < 0 {
		return nil, nil, errors.New("[server] 段没有结束标记 [/server]，为安全起见不写入")
	}
	// 找插入点：段内第一条 id 大于待插入 id 的频道行之前；没有则插在 [/server] 之前。
	// 同样只看 [server] 段内的行（见上面关闭分支对 serverSectionMask 的说明）。
	mask := serverSectionMask(out)
	insertIdx := insertAt
	for index := 0; index < insertAt; index++ {
		if !mask[index] {
			continue
		}
		records := recordsOnLine(out[index], index)
		if len(records) == 0 {
			continue
		}
		if records[0].id > pending[0].id {
			insertIdx = index
			break
		}
	}
	inserted := make([]fileLine, 0, len(pending))
	for _, rec := range pending {
		inserted = append(inserted, fileLine{Text: rec.raw, EOL: lineEOL(out)})
	}
	merged := make([]fileLine, 0, len(out)+len(inserted))
	merged = append(merged, out[:insertIdx]...)
	merged = append(merged, inserted...)
	merged = append(merged, out[insertIdx:]...)
	return merged, changed, nil
}

// lineEOL 取文件中实际使用的换行风格（优先 CRLF，占多数者胜）。
func lineEOL(lines []fileLine) string {
	crlf, lf := 0, 0
	for _, line := range lines {
		switch line.EOL {
		case "\r\n":
			crlf++
		case "\n":
			lf++
		}
	}
	if crlf >= lf && crlf > 0 {
		return "\r\n"
	}
	if lf > 0 {
		return "\n"
	}
	return "\r\n"
}

// recordsOnLine 重新解析某一行上的频道记录（带行内偏移，用于改写打包行）。
func recordsOnLine(line fileLine, index int) []record {
	fields := splitFieldsWithOffsets(line.Text)
	if len(fields) == 0 {
		return nil
	}
	start := 0
	if len(fields) >= 5 && isInt(fields[0].Text) && isChannelStart(fields, 1) {
		start = 1
	}
	records := make([]record, 0, 2)
	firstOnLine := true
	for start < len(fields) {
		if !isChannelStart(fields, start) {
			break
		}
		end := len(fields)
		for next := start + 4; next < len(fields); next++ {
			if isChannelStart(fields, next) {
				end = next
				break
			}
		}
		recordFields := fields[start:end]
		if id, err := strconv.Atoi(recordFields[0].Text); err == nil && id > 0 {
			typeID, _ := strconv.Atoi(recordFields[2].Text)
			records = append(records, record{
				id:        id,
				typeID:    typeID,
				group:     normalizeGroup(recordFields[3].Text),
				nameKey:   normalizeNameKey(recordFields[1].Text),
				lineIndex: index,
				raw:       recordRaw(line.Text, recordFields, start, firstOnLine),
			})
		}
		firstOnLine = false
		start = end
	}
	return records
}

// recordRaw 切出一条频道记录的原始文本。
// 行内第一条记录必须把行首缩进一起带上（原文件用 3 个空格对齐，重开时要逐字还原）；
// 同一行里后续记录则从自己的字段起点开始，重连时另用单空格拼接。
// start==0 表示行首没有"服务器号"前缀，此时行首缩进属于这条记录。
func recordRaw(line string, recordFields []field, start int, firstOnLine bool) string {
	rawStart := recordFields[0].Start
	if firstOnLine && start == 0 {
		rawStart = 0
	}
	rawEnd := recordFields[len(recordFields)-1].End
	if rawStart < 0 || rawEnd > len(line) || rawStart > rawEnd {
		return line
	}
	return line[rawStart:rawEnd]
}

// ================= 保护规则 =================

// checkDisableTargets 关闭前的校验。
//
// 两条硬规则：
//  1. 任何时刻至少留一个频道（表空了服务端就没有游戏端口，玩家一个都进不来）。
//  2. **任何时刻至少留一个特殊区域频道（type 22）开启**——服主口径：必须保证有一个
//     22 类型频道可用，所以不能把最后一个 22 类型关掉。
//
// 其余（引导 19 / 默认城镇 11 / 最后一个普通城镇频道 / 最后一个除 22 以外的频道）只告警不拦：
//   - 19 那条硬校验来自服务端 channelinfo.Build90CNOnlineScript，对应官方 PVF 流程
//     （那边引导频道是 id 1/type 2）；实测 1192 次客户端连接里请求端口 10019 一次都没有过，
//     服务端已降级为启动告警（DNFBRIDGE_CHANNEL_TABLE_STRICT=1 可恢复硬拒绝）。
//   - 11 在服务端热重载里同样已降级为告警。
func checkDisableTargets(targets []int, known map[int]record) error {
	remainingTotal := 0
	for id := range known {
		if !containsInt(targets, id) {
			remainingTotal++
		}
	}
	if remainingTotal == 0 {
		return errors.New("不能关闭：至少要保留一个频道")
	}

	// 只在"本次确实要关掉至少一个特殊区域频道"时才检查剩余量：文件本来就没有 type 22 行
	// （手工改过/恢复不全）时，不该把不相关的历史状态算到本次操作头上。
	closingSpecial := false
	for _, id := range targets {
		if rec, ok := known[id]; ok && rec.typeID == specialAreaChannelType {
			closingSpecial = true
			break
		}
	}
	if !closingSpecial {
		return nil
	}
	remainingSpecial := 0
	for id, rec := range known {
		if containsInt(targets, id) {
			continue
		}
		if rec.typeID == specialAreaChannelType {
			remainingSpecial++
		}
	}
	if remainingSpecial == 0 {
		return fmt.Errorf(
			"不能关闭：关闭后就没有任何特殊区域频道（type %d）了，必须至少保留一个开启",
			specialAreaChannelType)
	}
	return nil
}

// checkEnableTargets 开启前的校验：目标必须有可恢复的原始行文本。
func checkEnableTargets(path string, targets []int, known map[int]record) error {
	rows, _ := readManifest(path)
	missing := make([]int, 0, len(targets))
	for _, id := range targets {
		if _, exists := known[id]; exists {
			continue
		}
		if strings.TrimSpace(rows[strconv.Itoa(id)]) == "" {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("频道 %v 缺少可恢复的原始行文本（侧车清单 %s 里没有记录），无法开启", missing, path+sidecarSuffix)
	}
	return nil
}

// ================= 侧车清单 =================

func manifestPath(path string) string { return path + sidecarSuffix }

func readManifest(path string) (map[string]string, map[string]manifestMeta) {
	rows := make(map[string]string)
	meta := make(map[string]manifestMeta)
	raw, err := os.ReadFile(manifestPath(path))
	if err != nil {
		return rows, meta
	}
	var stored manifest
	if json.Unmarshal(raw, &stored) != nil {
		return rows, meta
	}
	if stored.Rows != nil {
		rows = stored.Rows
	}
	if stored.Meta != nil {
		meta = stored.Meta
	}
	return rows, meta
}

// mergeManifest 把当前文件里出现的频道行并入侧车清单（只增不改，永不丢弃历史行）。
func mergeManifest(path string, records []record) error {
	rows, meta := readManifest(path)
	dirty := false
	for _, rec := range records {
		key := strconv.Itoa(rec.id)
		if existing, ok := rows[key]; !ok || existing != rec.raw {
			// 行文本变化时以当前文件为准（例如有人手工调整了缩进），保证开启时写回的是最新样式。
			rows[key] = rec.raw
			dirty = true
		}
		if _, ok := meta[key]; !ok {
			meta[key] = manifestMeta{ServerID: rec.serverID, Type: rec.typeID, Group: rec.group, NameKey: rec.nameKey}
			dirty = true
		}
	}
	if !dirty {
		return nil
	}
	return writeManifestRaw(path, manifest{Rows: rows, Meta: meta})
}

// writeManifestRaw 原子写入侧车清单。
func writeManifestRaw(path string, value manifest) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	target := manifestPath(path)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// ================= 时间戳 =================

// nowStamp 状态快照时间（本地时间，便于和工具日志对照）。
func nowStamp() string {
	return nowFunc().Format("2006-01-02 15:04:05")
}

// backupStamp 备份文件名时间戳（文件名安全，不含空格与冒号）。
func backupStamp() string {
	return nowFunc().Format("20060102-150405")
}

// nowFunc 便于测试注入固定时间。
var nowFunc = func() time.Time { return time.Now() }

// ================= 小工具 =================

func splitFieldsWithOffsets(line string) []field {
	fields := make([]field, 0, 8)
	start := -1
	inBacktick := false
	flush := func(end int) {
		if start < 0 {
			return
		}
		fields = append(fields, field{Text: line[start:end], Start: start, End: end})
		start = -1
	}
	for index, char := range line {
		if char == '`' {
			if start < 0 {
				start = index
			}
			inBacktick = !inBacktick
			continue
		}
		if unicode.IsSpace(char) && !inBacktick {
			flush(index)
			continue
		}
		if start < 0 {
			start = index
		}
	}
	flush(len(line))
	return fields
}

func isInt(value string) bool {
	_, err := strconv.Atoi(value)
	return err == nil
}

func isQuoted(value string) bool {
	return len(value) >= 2 && value[0] == '`' && value[len(value)-1] == '`'
}

func isChannelStart(fields []field, index int) bool {
	return index >= 0 && index+3 < len(fields) &&
		isInt(fields[index].Text) && isQuoted(fields[index+1].Text) &&
		isInt(fields[index+2].Text) && isQuoted(fields[index+3].Text)
}

func normalizeNameKey(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "`")
	value = strings.TrimPrefix(value, "<")
	value = strings.TrimSuffix(value, ">")
	value = strings.TrimPrefix(value, "4::")
	return strings.TrimSpace(value)
}

func normalizeGroup(value string) string {
	value = normalizeNameKey(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	return strings.ToLower(strings.TrimSpace(value))
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func uniqueSorted(values []int) []int {
	seen := make(map[int]struct{}, len(values))
	out := make([]int, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}
