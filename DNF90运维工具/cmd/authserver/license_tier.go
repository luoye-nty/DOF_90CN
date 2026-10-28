// license_tier.go —— 授权卡档位（月/季/年/永久）与到期判定。
//
// 设计要点（与作者确认过的口径）：
//   - 永久卡：kind 为空或 "forever"，ExpireAt 为空，永不到期（老 data.json 记录零值即永久，无需迁移）。
//   - 限时卡：kind = month/quarter/year，Days = 30/90/365（固定天数）。
//   - 起算点：首次 /api/verify 全项通过时写入 FirstActivatedAt，ExpireAt = 该时刻 + Days；
//     此后重启、换绑（rebind）都不重置，只写一次。
//   - 到期判定：deny() 是 verify 与 heartbeat 共用的唯一判定入口——只在 verify 判的话，
//     客户端长期不重启即可白嫖，故心跳必须同样判定；两条路径共用同一函数也保证语义不会漂移。
//   - Status（active/revoked/unbound）是存储态，"已到期"是派生显示态，绝不回写 Status，
//     否则续期后无法自动恢复可用，且会与封禁/解绑语义打架。
//   - 时间统一存 UTC RFC3339；老记录的本地时区 RFC3339 由 time.Parse 正常解析。
//
// 失效方向（Fail-closed）：本文件所有判定遵循"证明不了有效即拒绝"。
// 例如 ExpireAt 有值但解析失败（手改/损坏/格式变更）、档位名无法识别且无时长，
// 一律按不可用处理，绝不因为"算不出到期时间"而退化成永久卡——那等于白送授权。
package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	// clientProtoCurrent 当前客户端协议版本。
	//   2 = 携带 proto 上报、支持月/季/年限时卡的新版赞助版运维工具；
	//   1 及以下（含完全不发 proto 字段的 2026-09 之前构建）在最小版本要求为 2 时被拒绝登录。
	// 老客户端表现为：verify 阶段直接被拦并弹出服务端中文说明；
	// 已在运行的老客户端心跳失败 3 次（约 90 秒）后以「连接中断」文案退出——行为正确、文案不准，
	// 但会命中该路径的只有「新版之后才可能到期的限时卡」客户，其必然持有新构建，故不做兼容 hack。
	clientProtoCurrent = 2

	kindForever = "forever"
	kindMonth   = "month"
	kindQuarter = "quarter"
	kindYear    = "year"
)

// cardDays 档位 → 时长（天）。fixed days，不做自然月，规避 1/31、2/29 之类边界。
var cardDays = map[string]int{
	kindMonth:   30,
	kindQuarter: 90,
	kindYear:    365,
}

// cardLabels 档位中文名（日志与响应兜底展示）。
var cardLabels = map[string]string{
	kindForever: "永久卡",
	kindMonth:   "月卡",
	kindQuarter: "季卡",
	kindYear:    "年卡",
}

// normalizeKind 归一化档位：空串视为永久卡（保证老 data.json 与不带 kind 的老管理端都生成永久卡）。
func normalizeKind(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return kindForever
	}
	return s
}

// kindValid 档位是否受支持。
func kindValid(kind string) bool {
	kind = normalizeKind(kind)
	return kind == kindForever || cardDays[kind] > 0
}

// kindLabel 档位中文名；未知档位原样返回。
func kindLabel(kind string) string {
	if l, ok := cardLabels[normalizeKind(kind)]; ok {
		return l
	}
	return kind
}

// nowUTC 服务端权威时间，UTC RFC3339（统一存储格式）。
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

// addDaysUTC 在 UTC 上按自然日推算到期时间：固定 24h×days。
// 先归一到 UTC 再 AddDate，避免服务器时区/夏令时导致"30 天 = 719 或 721 小时"。
func addDaysUTC(t time.Time, days int) string {
	return t.UTC().AddDate(0, 0, days).Format(time.RFC3339)
}

// parseTime 解析 RFC3339；空串或格式错误返回 ok=false。
func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// isForever 是否永久卡（kind 为权威判据；空 = 永久，兼容老记录）。
func (c *CDK) isForever() bool {
	return c == nil || normalizeKind(c.Kind) == kindForever
}

// effectiveDays 该卡的有效时长（天）：优先 Days，缺失/非正时回退档位定义；仍算不出返回 0。
func (c *CDK) effectiveDays() int {
	if c == nil {
		return 0
	}
	if c.Days > 0 {
		return c.Days
	}
	return cardDays[normalizeKind(c.Kind)]
}

// misconfigured 限时卡但档位无法识别且无时长：既算不出到期时间、也无法激活。
// 这种卡必须直接拒绝——否则它会变成"永远不过期"的永久卡（白送授权）。
func (c *CDK) misconfigured() bool {
	return c != nil && !c.isForever() && c.effectiveDays() <= 0
}

// expired 判定授权是否已到期。永久卡恒为未到期；尚未激活的限时卡（ExpireAt 为空）也未到期。
// ExpireAt 有值但解析失败时按【已到期】处理（fail-closed）：无法证明有效即拒绝。
func (c *CDK) expired(now time.Time) bool {
	if c.isForever() {
		return false
	}
	expStr := strings.TrimSpace(c.ExpireAt)
	if expStr == "" {
		return false // 未激活：等 verify 时写入起算点（误配卡由 misconfigured 拦下）
	}
	exp, ok := parseTime(expStr)
	if !ok {
		log.Printf("警告：CDK id=%s 的 expire_at 无法解析(%q)，按已到期处理（防手改/损坏变永久卡）", c.ID, expStr)
		return true
	}
	return !now.Before(exp)
}

// deny 统一的拒绝判定，返回 (错误码, 提示文案)；空串表示放行。
// verify 与 heartbeat 必须共用此函数，保证两条路径判定完全一致。
func (c *CDK) deny(now time.Time) (code, msg string) {
	if c.misconfigured() {
		log.Printf("警告：CDK id=%s 档位配置异常 kind=%q days=%d，拒绝登录", c.ID, c.Kind, c.Days)
		return "ERR_CDK_INVALID", "授权卡档位配置异常，请联系作者重新发卡"
	}
	if c.expired(now) {
		return "ERR_CDK_EXPIRED", fmt.Sprintf("授权已到期（%s），请联系作者续费", c.expireText())
	}
	return "", ""
}

// activateIfNeeded 首次验证通过时写入起算点与到期时间，只写一次。
// 返回 true 表示本次调用完成了首次激活（用于日志）。
func (c *CDK) activateIfNeeded(now time.Time) bool {
	if c.isForever() {
		return false
	}
	if strings.TrimSpace(c.ExpireAt) != "" {
		return false // 已激活：重启/换绑不重置计时
	}
	days := c.effectiveDays()
	if days <= 0 {
		return false // 误配卡：不激活（deny() 已拒绝，不会走到这里）
	}
	if strings.TrimSpace(c.FirstActivatedAt) == "" {
		c.FirstActivatedAt = now.UTC().Format(time.RFC3339)
	}
	c.ExpireAt = addDaysUTC(now, days)
	return true
}

// remainDays 剩余天数：永久卡 -1；已激活卡按剩余时长向上取整（最小 0）；
// 未激活的限时卡返回整档天数；ExpireAt 异常时返回 0（与 expired() 的 fail-closed 一致）。
func (c *CDK) remainDays(now time.Time) int {
	if c.isForever() {
		return -1
	}
	expStr := strings.TrimSpace(c.ExpireAt)
	if expStr == "" {
		if d := c.effectiveDays(); d > 0 {
			return d
		}
		return 0
	}
	exp, ok := parseTime(expStr)
	if !ok {
		return 0
	}
	d := exp.Sub(now)
	if d <= 0 {
		return 0
	}
	const day = 24 * time.Hour
	return int((d + day - 1) / day)
}

// renew 续期：未到期从原到期日叠加，已到期从当前时刻起算；未激活卡只累加时长。
// 永久卡拒绝续期。只改 ExpireAt/Days，不改 Status（避免把封禁/解绑的卡顺手放出来）。
//
// 附带的自愈能力：ExpireAt 损坏（解析失败）或档位误配（无时长）的卡，
// 续期都会重算出合法 ExpireAt / 补齐 Days，即"点一下续期即修复"。
func (c *CDK) renew(now time.Time, days int) error {
	if c.isForever() {
		return errors.New("永久卡无需续期")
	}
	if days <= 0 {
		return fmt.Errorf("续期天数必须为正数（收到 %d）", days)
	}
	c.Days += days // 累计有效时长（展示/审计；到期判定只看 ExpireAt）
	// 尚未激活：绝不能写绝对到期时间——起算点是"首次激活"，
	// 否则续期会把一张还没开始跑的卡变成"此刻 + 天数"，囤卡客户的时长凭空蒸发。
	if strings.TrimSpace(c.ExpireAt) == "" {
		return nil
	}
	base := now
	if exp, ok := parseTime(c.ExpireAt); ok && exp.After(now) {
		base = exp // 未到期：叠加在原到期日之后，客户不亏
	}
	c.ExpireAt = addDaysUTC(base, days)
	return nil
}

// expireText 到期时间展示文案（本地时区，便于作者/客户核对）；异常值显式标出。
func (c *CDK) expireText() string {
	if c.isForever() {
		return "永久"
	}
	s := strings.TrimSpace(c.ExpireAt)
	if s == "" {
		return "未激活"
	}
	exp, ok := parseTime(s)
	if !ok {
		return s + "（时间异常）"
	}
	return exp.Local().Format("2006-01-02 15:04")
}
