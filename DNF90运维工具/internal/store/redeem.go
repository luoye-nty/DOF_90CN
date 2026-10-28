// CDK 兑换码（redeem_*）数据访问：CDK 主表 redeem_codes 与兑换流水 redeem_redemptions。
// 与 config.License.CDK（运维工具授权卡密）命名隔离，本文件一律使用 redeem_* 前缀。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

// CDK 业务错误哨兵（server 层转 JSON code）。
var (
	ErrRedeemCodeNotFound    = errors.New("redeem-code-not-found")
	ErrRedeemAlreadyRedeemed = errors.New("redeem-already-redeemed")
	ErrRedeemExhausted       = errors.New("redeem-exhausted")
	ErrRedeemInvalidInput    = errors.New("redeem-invalid-input")
)

// redeemCodeCharset 20 位随机码字符集：去掉易混淆的 0/O/1/I，全大写。
const redeemCodeCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// redeemCodeChars 格式 XXXX-XXXX-XXXX-XXXX-XXXX（5 组 × 4 字符，共 20 位有效字符 + 4 个连字符）。
const redeemCodeSegmentCount = 5
const redeemCodeSegmentLen = 4

// redeemDDL 幂等建表语句（追加进 EnsureSchema 流程自动建表）。
var redeemDDL = []string{
	// CDK 主表：max_redeems=0 表示无限可兑换；enabled=0 表示逻辑作废（保留流水，禁止兑换）。
	`CREATE TABLE IF NOT EXISTS redeem_codes (
  id            BIGINT       NOT NULL AUTO_INCREMENT,
  redeem_code   VARCHAR(32)  NOT NULL,
  name          VARCHAR(64)  NOT NULL,
  content_raw   VARCHAR(512) NOT NULL,
  max_redeems   INT          NOT NULL DEFAULT 0,
  redeemed_count INT         NOT NULL DEFAULT 0,
  reward_json   JSON         NULL,
  enabled       TINYINT(1)   NOT NULL DEFAULT 1,
  remark        VARCHAR(255) NULL,
  created_by    VARCHAR(64)  NULL,
  created_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uk_redeem_code (redeem_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	// 兑换流水表：唯一键 (account_id, redeem_code) 天然实现“每账号每 CDK 限兑 1 次”。
	`CREATE TABLE IF NOT EXISTS redeem_redemptions (
  id             BIGINT       NOT NULL AUTO_INCREMENT,
  redeem_code    VARCHAR(32)  NOT NULL,
  account_id     VARCHAR(128) NOT NULL,
  username       VARCHAR(64)  NULL,
  character_id   VARCHAR(128) NULL,
  character_name VARCHAR(64)  NULL,
  mail_status    VARCHAR(16)  NOT NULL DEFAULT 'pending',
  mail_error     VARCHAR(255) NULL,
  remote_ip      VARCHAR(64)  NULL,
  redeemed_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uk_redeem_account_code (account_id, redeem_code),
  KEY idx_redemptions_code (redeem_code, redeemed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

// RedeemCode CDK 主表行。
type RedeemCode struct {
	ID            int64
	RedeemCode    string
	Name          string
	ContentRaw    string
	MaxRedeems    int
	RedeemedCount int
	RewardJSON    string
	Enabled       bool
	Remark        string
	CreatedBy     string
	CreatedAt     time.Time
}

// RedeemRedemption 兑换流水行。
type RedeemRedemption struct {
	ID            int64
	RedeemCode    string
	AccountID     string
	Username      string
	CharacterID   string
	CharacterName string
	MailStatus    string
	MailError     string
	RemoteIP      string
	RedeemedAt    time.Time
}

// RedeemReceipt 核销成功回执（供 server 层发邮件后回写流水）。
type RedeemReceipt struct {
	RedemptionID int64
	Name         string
	RewardJSON   string
}

// EnsureRedeemSchema 幂等创建两张 CDK 表（认证服务 / GUI 启动 EnsureSchema 流程统一调用）。
func (s *Store) EnsureRedeemSchema(ctx context.Context) error {
	for _, ddl := range redeemDDL {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("ensure redeem schema: %w", err)
		}
	}
	return nil
}

// NormalizeRedeemCode 规范化兑换码输入：全角→半角、去空白/连字符、转大写、按 5×4 重排。
//
// 2026-10 扩容（玩家反馈"兑换码带中文不能自动识别"）：发卡网的一键复制长这样 ——
//
//	卡号：ABCD-EFGH-JKLM-NPQR-STUV 密码：1234
//	兑换码：ABCD-EFGH-JKLM-NPQR-STUV 请及时使用
//
// 老实现只做 ToUpper + 去 '-'/' ' + 按**字节**长度校验，于是：
//   - 中文一个字 3 字节，len() 永远不等于 20（这是"带中文必失败"的元凶之一）；
//   - 中文标签留着不走，直接被当成非法字符拒掉。
//
// 现在改成"容忍 + 抠码"：与登录器 Services\CdkCodeExtractor.cs 同构 ——
// 唯一确定就采用，有歧义就不猜。**只放宽输入容忍度，不放宽最终字符集断言**：
// 返回的永远是标准形状的 20 位码，调用方（查流水 / 作废 / GUI）拿到的口径与以前完全一致。
//
// 注意：redeemCodeCharset / redeemCodeSegmentCount / redeemCodeSegmentLen /
// generateRedeemCode 一行都不能改 —— 改了会作废存量已发出的兑换码。
func NormalizeRedeemCode(raw string) (string, error) {
	code, ok := extractRedeemCode(raw)
	if !ok {
		return "", ErrRedeemInvalidInput
	}
	return code, nil
}

// extractRedeemCode 从任意文本里抠出 20 位兑换码（返回标准形状 XXXX-XXXX-XXXX-XXXX-XXXX）。
//
// 步骤与登录器 CdkCodeExtractor.TryExtract 一一对应：
//  1. 全角→半角（U+FF01..U+FF5E 减 0xFEE0，U+3000→空格）后转大写；
//  2. 分词：以"非 [A-Z0-9-] 的任意字符"为分隔符 —— 中文标签、冒号、【】、换行、
//     全角空格、NBSP 全部自然成为分隔；
//  3. 每个 run 去掉分隔符（空白 + 各种横线）后：正好 20 位且全部合法 → 候选；
//     超过 20 位再按"长度 20 的窗口"补一轮，**只有窗口唯一时才采用**；
//  4. 候选恰好 1 个 → 采用；多个 → 优先"横线呈 5×4"的那一个，仍多个取文本顺序第一个；
//     0 个 → 不认。
func extractRedeemCode(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "", false
	}

	text := strings.ToUpper(toHalfWidthRedeem(raw))

	// 分词 + 逐段攒候选（按文本出现顺序）。
	var (
		candidates  []string
		segmented   []bool
		runStart    = -1
		sawAnyToken = false
	)
	flush := func(end int) {
		if runStart < 0 {
			return
		}
		sawAnyToken = true
		run := text[runStart:end]
		for _, cand := range redeemCandidatesOfRun(run) {
			candidates = append(candidates, cand.code)
			segmented = append(segmented, cand.segmented)
		}
		runStart = -1
	}
	for i, r := range text {
		if isRedeemRunChar(r) {
			if runStart < 0 {
				runStart = i
			}
			continue
		}
		flush(i)
	}
	if runStart >= 0 {
		flush(len(text))
	}
	if !sawAnyToken {
		return "", false
	}

	// 兜底：整段文本本身就是一个码（玩家把码按 5×4 敲成 "ABCD EFGH JKLM NPQR STUV"，
	// 或从聊天窗格复制回来时带了空格）—— 老实现是先全局去空白再校验，这条路必须保住。
	// 它同时天然覆盖"只有唯一窗口"：按空白切出来的分段各自都 < 20 位，只有整段合起来才凑得齐。
	if len(candidates) == 0 {
		candidates, segmented = redeemWholeTextCandidate(text)
	}
	if len(candidates) == 0 {
		return "", false
	}

	// 恰好一个候选：直接用（最常见路径）。
	if len(candidates) == 1 {
		return formatRedeemCode(candidates[0]), true
	}

	// 多个候选：优先"横线正好呈 5×4"的那个（管理工具/发卡网导出的标准形状最可信）。
	for i, cand := range candidates {
		if segmented[i] {
			return formatRedeemCode(cand), true
		}
	}

	// 仍分不出高低：取文本顺序第一个（不猜内容，只按出现先后取）。
	return formatRedeemCode(candidates[0]), true
}

// redeemWholeTextCandidate 整段文本当**一个** token 再试一次。
//
// 用途：玩家把码按 5×4 敲成 "ABCD EFGH JKLM NPQR STUV" 时，分词会把空白当分隔符切出
// 五个 4 位短段（各自都不够 20 位），只有整段合起来才是那个码。老实现是"先全局去空白
// 再按长度校验"，正好吃这一种；这里用一条兜底路径把它等价地保住。
func redeemWholeTextCandidate(text string) ([]string, []bool) {
	stripped, hyphenCount := stripRedeemSeparators(text)
	if !isRedeemCodeChars(stripped) {
		return nil, nil
	}

	runes := []rune(text)
	segmented := len(runes) == redeemCodeSegmentCount*redeemCodeSegmentLen+(redeemCodeSegmentCount-1) &&
		hyphenCount == redeemCodeSegmentCount-1 &&
		hasRedeemSegmentHyphens(runes)

	return []string{stripped}, []bool{segmented}
}

// redeemCandidate 运行段里抠出的一个候选。
type redeemCandidate struct {
	code string // 去横线后的 20 位码
	// segmented 该 run 的横线是否正好落在 5×4 的分组位置。
	segmented bool
}

// redeemCandidatesOfRun 单个 run（已全角→半角、已大写）里的候选：
//   - 去分隔符后正好 20 位且全部合法 → 直接就是候选；
//   - 去分隔符后 > 20 位 → 滑一个长度 20 的窗口，**窗口必须唯一**，否则不猜；
//   - 去分隔符后 < 20 位 → 没有候选。
func redeemCandidatesOfRun(run string) []redeemCandidate {
	if run == "" {
		return nil
	}

	stripped, hyphenCount := stripRedeemSeparators(run)
	if utf8.RuneCountInString(stripped) < redeemCodeSegmentCount*redeemCodeSegmentLen {
		return nil
	}

	runRunes := []rune(run)
	segmented := len(runRunes) == redeemCodeSegmentCount*redeemCodeSegmentLen+(redeemCodeSegmentCount-1) &&
		hyphenCount == redeemCodeSegmentCount-1 &&
		hasRedeemSegmentHyphens(runRunes)

	if utf8.RuneCountInString(stripped) == redeemCodeSegmentCount*redeemCodeSegmentLen {
		if !isRedeemCodeChars(stripped) {
			return nil
		}
		return []redeemCandidate{{code: stripped, segmented: segmented}}
	}

	// 去分隔符后超过 20 位：滑一个"正好 20 个合法字符"的窗口。
	// 例：订单20260930{码} 粘成一段时，只有从码首开始的窗口才凑得齐 20 位合法字符。
	strippedRunes := []rune(stripped)
	limit := redeemCodeSegmentCount * redeemCodeSegmentLen
	windows := make([]string, 0, 1)
	seen := make(map[string]struct{}, 1)
	for start := 0; start+limit <= len(strippedRunes); start++ {
		window := string(strippedRunes[start : start+limit])
		if !isRedeemCodeChars(window) {
			continue
		}
		if _, dup := seen[window]; dup {
			continue
		}
		seen[window] = struct{}{}
		windows = append(windows, window)
	}

	// 有歧义（同一段里能凑出多个不同窗口）就不猜。
	if len(windows) != 1 {
		return nil
	}
	// 从长串里切出来的候选，横线形状已不可考，一律按未分组处理。
	return []redeemCandidate{{code: windows[0], segmented: false}}
}

// isRedeemRunChar 分词只认 [A-Z0-9-]；其余（中文、冒号、【】、空白…）都是分隔符。
func isRedeemRunChar(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-'
}

// stripRedeemSeparators 去掉空白（unicode.IsSpace 一次覆盖 \t/\n/\u3000/\u00A0）
// 与各种横线（半角 '-'、全角 '－'、非断行连字符 '‑'），并回传横线个数。
func stripRedeemSeparators(s string) (string, int) {
	var b strings.Builder
	b.Grow(len(s))
	hyphenCount := 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		switch r {
		case '-', '\uFF0D', '\u2011':
			hyphenCount++
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), hyphenCount
}

// hasRedeemSegmentHyphens 横线是否正好落在 5×4 的分组位置（第 4/9/14/19 个字符处）。
func hasRedeemSegmentHyphens(runes []rune) bool {
	for group := 1; group < redeemCodeSegmentCount; group++ {
		if runes[group*redeemCodeSegmentLen+(group-1)] != '-' {
			return false
		}
	}
	return true
}

// isRedeemCodeChars 最终字符集断言：20 位且全部属于 redeemCodeCharset。
func isRedeemCodeChars(s string) bool {
	if utf8.RuneCountInString(s) != redeemCodeSegmentCount*redeemCodeSegmentLen {
		return false
	}
	for _, ch := range s {
		if !strings.ContainsRune(redeemCodeCharset, ch) {
			return false
		}
	}
	return true
}

// formatRedeemCode 按 5×4 重排出标准形状（筹码已校验过字符集与长度）。
func formatRedeemCode(raw string) string {
	runes := []rune(raw)
	var b strings.Builder
	for i := 0; i < redeemCodeSegmentCount; i++ {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(string(runes[i*redeemCodeSegmentLen : (i+1)*redeemCodeSegmentLen]))
	}
	return b.String()
}

// toHalfWidthRedeem 全角→半角：U+FF01..U+FF5E 减 0xFEE0，U+3000 与各种"看着像空格"的
// 字符统一成半角空格。一步解决 Ａ→A、１→1、：→:、－→-。
func toHalfWidthRedeem(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	changed := false
	for _, r := range s {
		switch {
		case r >= '\uFF01' && r <= '\uFF5E':
			changed = true
			b.WriteRune(r - 0xFEE0)
		case r == '\u3000':
			changed = true
			b.WriteByte(' ')
		case r == '\u00A0' || r == '\u2007' || r == '\u202F' || r == '\uFEFF':
			changed = true
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	if !changed {
		return s
	}
	return b.String()
}

// generateRedeemCode 生成一张随机 20 位码（已按 XXXX-XXXX-XXXX-XXXX-XXXX 格式化）。
func generateRedeemCode() (string, error) {
	raw := make([]byte, redeemCodeSegmentCount*redeemCodeSegmentLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for i := 0; i < redeemCodeSegmentCount; i++ {
		if i > 0 {
			b.WriteByte('-')
		}
		for j := 0; j < redeemCodeSegmentLen; j++ {
			idx := int(raw[i*redeemCodeSegmentLen+j]) % len(redeemCodeCharset)
			b.WriteByte(redeemCodeCharset[idx])
		}
	}
	return b.String(), nil
}

// isDuplicateEntry 判断是否为唯一键冲突（MySQL 1062）。
func isDuplicateEntry(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

// CreateRedeemCode 生成单张 CDK 并入库；name 供登录器回显，contentRaw 为原始内容串
// （{次数}#{名称}#{物品串}），rewardJSON 为结构化奖励数组文本。uk_redeem_code 冲突自动重试（最多 5 次）。
func (s *Store) CreateRedeemCode(ctx context.Context, name, contentRaw string, max int, rewardJSON, remark, createdBy string) (string, error) {
	name = strings.TrimSpace(name)
	contentRaw = strings.TrimSpace(contentRaw)
	if name == "" || contentRaw == "" {
		return "", ErrRedeemInvalidInput
	}
	if max < 0 {
		max = 0
	}
	rewardVal := sql.NullString{String: strings.TrimSpace(rewardJSON), Valid: strings.TrimSpace(rewardJSON) != ""}
	remarkVal := nullString(remark)
	createdByVal := nullString(createdBy)

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		code, err := generateRedeemCode()
		if err != nil {
			return "", err
		}
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO redeem_codes (redeem_code, name, content_raw, max_redeems, redeemed_count, reward_json, enabled, remark, created_by)
			 VALUES (?, ?, ?, ?, 0, ?, 1, ?, ?)`,
			code, name, contentRaw, max, rewardVal, remarkVal, createdByVal)
		if err == nil {
			return code, nil
		}
		if !isDuplicateEntry(err) {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("create redeem code: %w (unique collision)", lastErr)
}

// CreateRedeemCodeBatch 批量生成 count 张同名 CDK，事务内逐条插入，返回全部新码。
func (s *Store) CreateRedeemCodeBatch(ctx context.Context, count int, name, contentRaw string, max int, rewardJSON, remark, createdBy string) ([]string, error) {
	if count <= 0 {
		return nil, ErrRedeemInvalidInput
	}
	name = strings.TrimSpace(name)
	contentRaw = strings.TrimSpace(contentRaw)
	if name == "" || contentRaw == "" {
		return nil, ErrRedeemInvalidInput
	}
	if max < 0 {
		max = 0
	}
	rewardVal := sql.NullString{String: strings.TrimSpace(rewardJSON), Valid: strings.TrimSpace(rewardJSON) != ""}
	remarkVal := nullString(remark)
	createdByVal := nullString(createdBy)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		code, err := s.insertRedeemCodeTx(ctx, tx, name, contentRaw, max, rewardVal, remarkVal, createdByVal)
		if err != nil {
			return nil, err
		}
		out = append(out, code)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// insertRedeemCodeTx 单条码插入（唯一键冲突时重试 5 次）。
func (s *Store) insertRedeemCodeTx(ctx context.Context, tx *sql.Tx, name, contentRaw string, max int, rewardJSON, remark, createdBy any) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		code, err := generateRedeemCode()
		if err != nil {
			return "", err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO redeem_codes (redeem_code, name, content_raw, max_redeems, redeemed_count, reward_json, enabled, remark, created_by)
			 VALUES (?, ?, ?, ?, 0, ?, 1, ?, ?)`,
			code, name, contentRaw, max, rewardJSON, remark, createdBy)
		if err == nil {
			return code, nil
		}
		if !isDuplicateEntry(err) {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("insert redeem code: %w (unique collision)", lastErr)
}

// ListRedeemCodes 分页列出 CDK（keyword 模糊匹配码/名称，空则全部），按创建时间倒序。
func (s *Store) ListRedeemCodes(ctx context.Context, keyword string, page, pageSize int) ([]RedeemCode, error) {
	keyword = strings.TrimSpace(keyword)
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 100
	}
	query := `SELECT id, redeem_code, name, content_raw, max_redeems, redeemed_count,
	                 COALESCE(reward_json,''), enabled, COALESCE(remark,''), COALESCE(created_by,''), created_at
	          FROM redeem_codes`
	args := []any{}
	if keyword != "" {
		query += ` WHERE redeem_code LIKE ? OR name LIKE ?`
		like := "%" + keyword + "%"
		args = append(args, like, like)
	}
	query += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, pageSize, (page-1)*pageSize)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RedeemCode
	for rows.Next() {
		var c RedeemCode
		var enabled int
		if err := rows.Scan(&c.ID, &c.RedeemCode, &c.Name, &c.ContentRaw, &c.MaxRedeems, &c.RedeemedCount,
			&c.RewardJSON, &enabled, &c.Remark, &c.CreatedBy, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Enabled = enabled == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListRedeemRedemptions 列出兑换流水：code 为空取全部，否则限定单码；按兑换时间倒序，limit<=0 取 200。
func (s *Store) ListRedeemRedemptions(ctx context.Context, code string, limit int) ([]RedeemRedemption, error) {
	if limit <= 0 {
		limit = 200
	}
	query := `SELECT id, redeem_code, account_id, COALESCE(username,''), COALESCE(character_id,''),
	                 COALESCE(character_name,''), COALESCE(mail_status,'pending'), COALESCE(mail_error,''),
	                 COALESCE(remote_ip,''), redeemed_at
	          FROM redeem_redemptions`
	args := []any{}
	if code = strings.TrimSpace(code); code != "" {
		norm, err := NormalizeRedeemCode(code)
		if err != nil {
			return nil, err
		}
		query += ` WHERE redeem_code = ?`
		args = append(args, norm)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RedeemRedemption
	for rows.Next() {
		var r RedeemRedemption
		if err := rows.Scan(&r.ID, &r.RedeemCode, &r.AccountID, &r.Username, &r.CharacterID,
			&r.CharacterName, &r.MailStatus, &r.MailError, &r.RemoteIP, &r.RedeemedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DisableRedeemCode 作废 CDK（enabled=0，保留流水可追溯；物理删除由 UI 另行确认）。
func (s *Store) DisableRedeemCode(ctx context.Context, code string) error {
	norm, err := NormalizeRedeemCode(code)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE redeem_codes SET enabled=0 WHERE redeem_code=? AND enabled=1`, norm)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrRedeemCodeNotFound
	}
	return nil
}

// RedeemCodeTx 核销事务：锁定 CDK 行 → 校验未作废/次数 → 插入流水（唯一键防同账号重复）
// → 累加 redeemed_count → 提交。返回回执供 handler 发邮件后回写 mail_status。
// 说明：本方法只负责“占用一次兑换”，系统邮件由调用方负责，二者通过流水 mail_status 解耦。
func (s *Store) RedeemCodeTx(ctx context.Context, code, accountID, username, characterID, characterName, remoteIP string) (*RedeemReceipt, error) {
	norm, err := NormalizeRedeemCode(code)
	if err != nil {
		return nil, ErrRedeemCodeNotFound
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, ErrRedeemInvalidInput
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 行锁串行化同码并发兑换：先校验 enabled 与剩余次数，再占流水、累加计数。
	var id int64
	var name, rewardJSON string
	var maxRedeeems, redeemedCount int
	err = tx.QueryRowContext(ctx,
		`SELECT id, name, max_redeems, redeemed_count, COALESCE(reward_json,'')
		 FROM redeem_codes WHERE redeem_code=? AND enabled=1 FOR UPDATE`,
		norm).Scan(&id, &name, &maxRedeeems, &redeemedCount, &rewardJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRedeemCodeNotFound
		}
		return nil, err
	}
	if maxRedeeems > 0 && redeemedCount >= maxRedeeems {
		return nil, ErrRedeemExhausted
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO redeem_redemptions (redeem_code, account_id, username, character_id, character_name, mail_status, remote_ip)
		 VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
		norm, accountID, nullString(username), nullString(characterID), nullString(characterName), nullString(remoteIP))
	if err != nil {
		if isDuplicateEntry(err) {
			return nil, ErrRedeemAlreadyRedeemed
		}
		return nil, err
	}
	redemptionID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE redeem_codes SET redeemed_count=redeemed_count+1 WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &RedeemReceipt{RedemptionID: redemptionID, Name: name, RewardJSON: rewardJSON}, nil
}

// SetRedemptionMailResult 回写流水邮件状态（sent / failed），mailError 为空字符串时置 NULL。
func (s *Store) SetRedemptionMailResult(ctx context.Context, redemptionID int64, status, mailError string) error {
	if redemptionID <= 0 {
		return ErrRedeemInvalidInput
	}
	if status == "" {
		status = "failed"
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE redeem_redemptions SET mail_status=?, mail_error=? WHERE id=?`,
		status, nullString(mailError), redemptionID)
	return err
}

// GameCharacter 游戏角色精简视图：直查游戏侧 dnf_characters 表得到。
// JSON tag 与旧 GM 回环透传的 character_id/name 兼容，level 为附加只读字段。
type GameCharacter struct {
	CharacterID string `json:"character_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
}

// listGameCharactersByAccountSQL 登录器「选择角色」列表的口径。
//
// delete_flag = 0 不是可选项，它是一条线上事故的边界（2026-09-30）：
// 游戏侧删号是**软删除** —— 只把 dnf_characters.delete_flag 置 1（并把 slot 改成负数），
// 那一行**还在表里**。少了这个条件，已删除的角色会照旧出现在登录器的角色选择列表里，
// 玩家选中它去兑换 CDK：归属校验一路放行 → 兑换码先被消耗掉 → 奖励（邮件/点券/黑钻）
// 发给一张服务端已经不认的角色 → 全部静默失败，玩家手里只剩一次被烧掉的兑换。
//
// 口径必须与游戏自己一致：服务端的选角列表（dnf 角色仓库 ListByAccount）同样是
// `WHERE account_id = ? AND delete_flag = 0`。工具这边少一个条件，
// 就等于凭空多出几个"玩家看不见、却能被选中"的幽灵角色。
const listGameCharactersByAccountSQL = `SELECT character_id, name, level
		 FROM dnf_characters
		 WHERE account_id = ? AND delete_flag = 0
		 ORDER BY character_id`

// ListGameCharactersByAccount 直查游戏现有表 dnf_characters，按账号（launcher
// account_id，形如 dnf:sha256hex，与 dnf_characters.account_id 同源）取该账号全部**可用**角色
// （口径见 listGameCharactersByAccountSQL：已删除的角色不返回）。
// 同一账号多角色时天然返回多行；无角色（含"角色已被删光"）返回空切片（非 nil）。
func (s *Store) ListGameCharactersByAccount(ctx context.Context, accountID string) ([]GameCharacter, error) {
	rows, err := s.db.QueryContext(ctx, listGameCharactersByAccountSQL, strings.TrimSpace(accountID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]GameCharacter, 0, 4)
	for rows.Next() {
		var c GameCharacter
		if err := rows.Scan(&c.CharacterID, &c.Name, &c.Level); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// gameCharacterTargetSQL 发放目标定位：按 (account_id, character_id) 精确取回那一行。
//
// 为什么"已删除"这一格**没有**写进 WHERE：这个函数是"能不能把东西发给这个角色"的
// **权威判据**，四类拒绝理由（不存在 / 已删除 / 不属于该账号 / 行不是这一个角色）
// 必须在同一处显式判出来、并被无库单测逐格覆盖。条件写进 WHERE 之后，"已删除"会
// 与"不存在"一起退化成 sql.ErrNoRows，两种状态再也分不出来，测试也就无从下手。
// 定位本身仍然是索引点查（account_id + character_id），不会多捞行。
const gameCharacterTargetSQL = `SELECT character_id, name, level, account_id, delete_flag
		 FROM dnf_characters
		 WHERE account_id = ? AND character_id = ?`

// gameCharacterTargetRow dnf_characters 的一行原始读结果（判据需要的全部列）。
type gameCharacterTargetRow struct {
	CharacterID string
	Name        string
	Level       int
	AccountID   string
	// DeleteFlag 游戏侧软删除标记（TINYINT UNSIGNED NOT NULL DEFAULT 0）。
	// 用 NullInt64 是为了把"列缺失/NULL"也纳入判据（见 judgeGameCharacterTarget）。
	DeleteFlag sql.NullInt64
}

// judgeGameCharacterTarget 发放目标判据（纯函数，不碰数据库，便于无库单测）。
//
// 四格全部通过才算可用目标：命中了行、行就是请求的那个角色、行属于该账号、未删除。
// 注意"不存在"与"不属于该账号"在 SQL 侧都会落成"没有行"（found=false），
// 对玩家而言也是同一句话（回去重新选角色），所以这里合并成同一个 false。
func judgeGameCharacterTarget(row gameCharacterTargetRow, found bool, accountID, characterID string) (GameCharacter, bool) {
	if !found {
		return GameCharacter{}, false
	}
	if strings.TrimSpace(row.CharacterID) != strings.TrimSpace(characterID) ||
		strings.TrimSpace(row.AccountID) != strings.TrimSpace(accountID) {
		return GameCharacter{}, false
	}
	// delete_flag 必须**恰好为 0**：NULL/列缺失一律按"已删除"处理（保守口径）——
	// 宁可让运营去核实一次，也不能把奖励发给一张玩家自己都看不见的角色；
	// 而且登录器列表口径（delete_flag = 0）同样不认 NULL，两边必须一致，
	// 否则会出现"列表里选得到、兑换却被拒"或者"选不到却能兑换"这种错位。
	if !row.DeleteFlag.Valid || row.DeleteFlag.Int64 != 0 {
		return GameCharacter{}, false
	}
	return GameCharacter{CharacterID: row.CharacterID, Name: row.Name, Level: row.Level}, true
}

// GameCharacterActive 发放目标的**权威判据**：一次性校验"该角色存在 && 未删除 && 属于该账号"，
// 命中则返回角色行（只读，不改任何数据）。未命中返回 (零值, false, nil) —— 角色不能作为
// 发放目标是业务状态，不是故障，由调用方决定怎么提示玩家。
//
// 为什么要有它（而不是让调用方"拉角色列表 + 在内存里比对"）：
// CDK 兑换的硬约束是**任何拒绝都必须发生在核销之前**。判据一旦漏掉"已删除"，
// 兑换码会先被消耗掉（落 redeem_redemptions + redeemed_count+1），
// 奖励却发不出去 —— 这正是 2026-09-30 线上事故。判断口径只留一个实现、一个调用点，
// 才能保证它不再漂移：漂移一次，烧掉的就是玩家手里的兑换码。
func (s *Store) GameCharacterActive(ctx context.Context, accountID, characterID string) (GameCharacter, bool, error) {
	accountID = strings.TrimSpace(accountID)
	characterID = strings.TrimSpace(characterID)
	if accountID == "" || characterID == "" {
		return GameCharacter{}, false, nil
	}
	var row gameCharacterTargetRow
	err := s.db.QueryRowContext(ctx, gameCharacterTargetSQL, accountID, characterID).
		Scan(&row.CharacterID, &row.Name, &row.Level, &row.AccountID, &row.DeleteFlag)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 不存在，或这个 character_id 不属于该账号：都不给发。
		// 仍走同一个判据（而不是直接写 false），保证只有一处口径。
		active, ok := judgeGameCharacterTarget(gameCharacterTargetRow{}, false, accountID, characterID)
		return active, ok, nil
	case err != nil:
		return GameCharacter{}, false, err
	}
	active, ok := judgeGameCharacterTarget(row, true, accountID, characterID)
	return active, ok, nil
}

// GameBlackDiamond 直查游戏库得到的黑钻现状（只读）。
//
// 这是**服务端只读 GM 接口不可用时的兜底**（老版本 DNF90Server.exe 没有该接口）：
// 黑钻的权威存储是账号级 metadata 的 premium_expire_1000（记的是到期 unix 秒）；
// 疲劳与疲劳上限是 dnf_characters 的**列**（fatigue / fatigue_limit，默认 156，
// 黑钻生效时上限是 188）。注意别写成 dnf_character_stats 的 KV 行 ——
// 那张表只放补充属性，实机库（角色 1）里根本没有 fatigue/fatigue_limit 这两行，
// 用它会让工具把疲劳显示成 0/0。
type GameBlackDiamond struct {
	CharacterID   string `json:"character_id"`
	CharacterName string `json:"character_name"`
	AccountID     string `json:"account_id"`
	Active        bool   `json:"active"`
	ExpiresAt     int64  `json:"expires_at"`
	// RemainingSeconds 按**本机当前时间**换算；调用方如果拿到了服务端时间，
	// 应当用服务端时间重算（见 server.GMBlackDiamondStatus.ServerNow 的处理）。
	RemainingSeconds int64 `json:"remaining_seconds"`
	Fatigue          int64 `json:"fatigue"`
	FatigueLimit     int64 `json:"fatigue_limit"`
}

// BlackDiamondStatus 按角色 ID 直查游戏库里的黑钻现状（只读，不改任何数据）。
//
// 这里**刻意不带** delete_flag = 0：它不是"发放目标"查询，只是把运营在界面上选中的
// 那个角色的现状回显出来（黑钻的发放走服务端 GM 接口，由服务端自己校验角色，
// 见 gm_admin_state.go 的 gmCharacter）。加上过滤只会把"角色已删除"伪装成
// "游戏库里没有这个角色"，运营反而分不清是删号了还是库/账号对不上。
// "发东西给谁"是另一套口径，见 ListGameCharactersByAccount 与 GameCharacterActive。
func (s *Store) BlackDiamondStatus(ctx context.Context, characterID string) (GameBlackDiamond, bool, error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return GameBlackDiamond{}, false, ErrRedeemInvalidInput
	}
	var out GameBlackDiamond
	err := s.db.QueryRowContext(ctx,
		`SELECT character_id, name, account_id, fatigue, fatigue_limit
		 FROM dnf_characters WHERE character_id = ?`, characterID).
		Scan(&out.CharacterID, &out.CharacterName, &out.AccountID, &out.Fatigue, &out.FatigueLimit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GameBlackDiamond{}, false, nil
		}
		return GameBlackDiamond{}, false, err
	}
	// premium_expire_1000 不存在表示从未开过黑钻（ExpireAtRaw 语义：0 = 无权益）。
	var raw string
	err = s.db.QueryRowContext(ctx,
		`SELECT entry_value FROM dnf_account_metadata WHERE account_id = ? AND entry_key = 'premium_expire_1000'`,
		out.AccountID).Scan(&raw)
	switch {
	case err == nil:
		if parsed, parseErr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); parseErr == nil && parsed > 0 {
			out.ExpiresAt = parsed
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return GameBlackDiamond{}, false, err
	}
	now := time.Now().Unix()
	out.Active = out.ExpiresAt > now
	if out.RemainingSeconds = out.ExpiresAt - now; out.RemainingSeconds < 0 {
		out.RemainingSeconds = 0
	}
	return out, true, nil
}
