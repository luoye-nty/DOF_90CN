package main

// 记录查询的字段汉化与取值可读化。
//
// 记录本体是服务端原样透传的 map（字段随类别不同），所以这里做两件事：
//  1. 字段名 → 中文（未收录的字段**保留原名**显示，以后服务端加字段不会变成空白）；
//  2. 取值可读化：布尔 → 是/否、秒数 → "X 分 Y 秒"、账号主键 → 真实账号名。
//
// 字典口径来自 go-server 的记录写入点（activity_hooks.go / cera_shop_purchase.go /
// town_chat*.go）。新增字段时在这里补一行即可。

import (
	"fmt"
	"strings"
)

// recFieldLabels 字段名 → 中文标签。未收录的字段保留原名。
var recFieldLabels = map[string]string{
	// —— 三类记录公共 ——
	"ts":             "时间",
	"kind":           "类别",
	"event":          "事件",
	"event_name":     "事件名",
	"account_id":     "账号",
	"character_id":   "角色ID",
	"character_name": "角色名",
	"channel_id":     "频道",
	"result":         "结果",
	"reason":         "原因",

	// —— 点券消费（cera）——
	"payment_mode":           "支付方式码",
	"payment_name":           "支付方式",
	"cera_spent":             "实扣点券",
	"cera_total_price":       "商品标价合计",
	"cera_before":            "消费前点券",
	"cera_after":             "消费后点券",
	"cera_spent_note":        "消费说明",
	"token_cera_after":       "代币券余额",
	"happy_token_cera_after": "欢乐代币券余额",
	// 券实际扣了多少 + 扣之前多少（2026-10-05 新增）。
	//
	// **这两列就是为了回答"这单到底付没付钱"**：以前只有 cera_spent 和两张券的
	// "之后"余额，于是一笔全额用券支付的正经消费会显示成
	// `实扣点券=0` + `欢乐代币券余额=0` —— 看起来像白拿，
	// 现场为此把 3GB 加密封包日志拉回来解密才定案。
	// 字段只在真用到券时才有（与 gold_* 同口径），缺席即 0。
	"token_cera_spent":        "实扣代币券",
	"token_cera_before":       "消费前代币券",
	"happy_token_cera_spent":  "实扣欢乐代币券",
	"happy_token_cera_before": "消费前欢乐代币券",
	"granted":                 "发放内容",
	// item_count 是**多类别共用**的键：点券消费里是"购买行数"，拍卖行里是"这件道具挂了几件"。
	// 字典一个键只能有一个标签，所以取两边都说得通的说法，别再各自加一份（会 duplicate key 编译不过）。
	"item_count":                  "数量",
	"request_items":               "请求条目",
	"body_len":                    "报文长度",
	"changed_slots":               "变更格子",
	"premium_activations":         "激活权益",
	"main_inventory_expansion":    "主背包扩容",
	"avatar_inventory_expansion":  "装扮栏扩容",
	"personal_cargo_slot_count":   "个人金库格数",
	"account_cargo_selection_key": "账号金库选择键",
	"character_slot_limit":        "角色栏上限",
	"rename_entitlements_added":   "改名次数",
	"operation":                   "操作",
	"source":                      "来源",
	// —— 金币寄售买入（source=gold_consign）——
	// 2026-09 新增：此前金币寄售扣的点券**完全没有埋点**，
	// 「点券消费记录」里查不到，运营对不上账。服务端补记后在这里补汉化。
	"listing_id":    "寄售单号",
	"gold_item_id":  "金币档位道具ID",
	"gold_amount":   "金币数量",
	"seller_id":     "卖家角色ID",
	"seller_name":   "卖家角色名",
	"fee_percent":   "手续费率(%)",
	"proceeds_cera": "卖家实得代币券",
	// —— 金币寄售记录（kind=gold_consign，2026-09 新增独立类别）——
	// 此前只有"买家买入"一条混在点券消费里，上架/下架/成交/手续费/过期全查不到。
	"price_cera":     "售价代币券",
	"fee_cera":       "手续费代币券",
	"buyer_id":       "买家角色ID",
	"buyer_name":     "买家角色名",
	"refund_gold":    "退回金币",
	"duration_hours": "寄售时长(小时)",
	"expires_at":     "到期时间",
	"delivered":      "是否已发货",
	// —— 拍卖行记录（kind=auction，2026-09 新增类别）——
	// 拍卖行此前**零埋点**，只有 packet_log（高负载实测丢约 25% 行），运营一条都查不到。
	"item_name":       "道具名",
	"list_type":       "列表",
	"source_slot":     "来源格",
	"unit_price":      "单价",
	"start_price":     "起拍价",
	"buyout_price":    "一口价",
	"deposit_gold":    "上架押金",
	"fee_gold":        "手续费金币",
	"sale_price":      "成交价",
	"seller_proceeds": "卖家实得金币",
	"gold_after":      "操作后金币",
	"bid_total":       "出价总额",
	"bid_unit_price":  "出价单价",
	"outbid_id":       "被超越者角色ID",
	"outbid_gold":     "被超越者退款",
	"buyout":          "是否一口价",
	"whole_listing":   "是否整单",
	"bidder_id":       "出价者角色ID",
	// —— 团本记录（kind=raid，服务端 raid_log.go 一直在写）——
	"online_members": "在线人数",
	"roster_members": "名单人数",
	"wall_ms":        "耗时(毫秒)",
	"conn_id":        "连接ID",
	"session_online": "会话在线",
	// —— 场景加载诊断（kind=scene，2026-09 新增类别）——
	// 这些原本混在团本记录里（选人选场景时每秒数条"引导包被推迟"），
	// 为了不淹没团本事件、也不占团本文件轮转窗口，单独成类。
	"defer_reason":          "推迟原因(状态机)",
	"char_id":               "角色ID(诊断)",
	"actor_ready_source":    "角色就绪来源",
	"packet_count":          "包数量",
	"resume_packet_index":   "续传包序号",
	"resume_post_stage":     "续传阶段",
	"tail_actor_generation": "角色代次",
	// granted[] / request_items[] 子字段
	"commodity_id":       "商品ID",
	"item_id":            "道具ID",
	"count":              "数量",
	"section":            "分区",
	"cera_price":         "价格(一组价)",
	"attribute_value":    "属性值",
	"linked_item_id":     "关联道具ID",
	"inventory_slot":     "背包格",
	"avatar_choices":     "装扮选择",
	"package_selections": "礼包选择",

	// —— 对话（chat）——
	"mode":                      "模式",
	"mode_name":                 "模式名",
	"message":                   "内容",
	"message_len":               "字数",
	"message_bytes":             "字节数",
	"message_hex":               "原始字节",
	"recipients":                "接收者",
	"item_links":                "物品链接",
	"scope":                     "范围",
	"notification":              "通知类型",
	"speaker_name":              "说话人",
	"target_character_id":       "对方角色ID",
	"target_name":               "对方角色名",
	"target_channel_id":         "对方频道",
	"conversation_id":           "会话ID",
	"resolved_by":               "解析方式",
	"material_item_id":          "喇叭材料ID",
	"material_slot":             "喇叭材料格",
	"material_remaining":        "喇叭材料剩余",
	"material_removed":          "喇叭材料消耗",
	"robot_identity_projection": "假人身份投影",

	// —— 副本（dungeon）——
	"dungeon_id":         "副本ID",
	"dungeon_name":       "副本名",
	"dungeon_path":       "副本路径",
	"client_dungeon_id":  "客户端副本ID",
	"request_dungeon_id": "请求副本ID",
	"maze_index":         "房间序号",
	"difficulty":         "难度",
	"selection_mode":     "选择模式",
	"entry_option":       "进入方式",
	"party_state":        "队伍状态",
	"lifecycle_token":    "生命周期令牌",
	"character_level":    "角色等级",
	"practice":           "练习场",
	"tutorial":           "教学副本",
	"started_at":         "进副本时间",
	"elapsed_seconds":    "通关用时",
	// 无色小晶块消耗：只统计技能消耗（晶体契约每 30 秒自动扣的那个不计入）。
	"colorless_spent":          "无色小晶块消耗",
	"killed_normal":            "击杀普通怪",
	"killed_champion":          "击杀精英怪",
	"killed_boss":              "击杀BOSS",
	"clear_source":             "通关来源",
	"run_status":               "通关状态",
	"room":                     "房间",
	"map_id":                   "地图ID",
	"defeated_actor_count":     "击败单位数",
	"monster_experience_total": "怪物经验合计",
	"exit_reason":              "退出原因",
	"cleared":                  "是否通关",
	"settlement_entry_sent":    "结算入口已发",
	"settlement_result_sent":   "结算结果已发",
	"town_return_sent":         "回城已发",
	// 副本「三合一」：这条是从哪几个原始事件合并出来的（进入/通关/离开）。
	"merged_events": "原始事件（已合并）",
}

// recFieldLabel 取字段的中文标签；未收录返回原名（外加标记，便于把缺的字段报回来补）。
func recFieldLabel(key string) string {
	if label, ok := recFieldLabels[key]; ok {
		return label
	}
	return key + "（未汉化）"
}

// recBoolFields 取值是"是/否"的字段（布尔直接显示 true/false 不好读）。
var recBoolFields = map[string]struct{}{
	"cleared":                {},
	"practice":               {},
	"tutorial":               {},
	"settlement_entry_sent":  {},
	"settlement_result_sent": {},
	"town_return_sent":       {},
	// 拍卖行 / 金币寄售：这些布尔是"这次到底是哪种操作"的关键判据，翻成是/否更好读。
	"buyout":         {},
	"whole_listing":  {},
	"delivered":      {},
	"session_online": {},
}

// recFieldValueText 把某个字段的值变成人看的文本。
// accountNames 为空时账号仍显示原主键（翻译不到不能变成看不见）。
func recFieldValueText(key string, value any, accountNames map[string]string) string {
	if value == nil {
		return "—"
	}
	if text, ok := value.(string); ok {
		return recFieldStringValue(key, text, accountNames)
	}
	if flag, ok := value.(bool); ok {
		if _, isBool := recBoolFields[key]; isBool || key == "cleared" {
			if flag {
				return "是"
			}
			return "否"
		}
		if flag {
			return "是"
		}
		return "否"
	}
	if seconds, ok := recNumberSeconds(key, value); ok {
		return recDurationText(seconds)
	}
	return fmt.Sprintf("%v", value)
}

// recFieldStringValue 字符串值的可读化（账号翻译、秒数、已知枚举）。
func recFieldStringValue(key, text string, accountNames map[string]string) string {
	trimmed := strings.TrimSpace(text)
	if key == "account_id" && trimmed != "" {
		return recAccountText(trimmed, accountNames)
	}
	if key == "elapsed_seconds" {
		if seconds, ok := parseRecSeconds(trimmed); ok {
			return recDurationText(seconds)
		}
	}
	switch key {
	case "result":
		switch trimmed {
		case "ok":
			return "成功"
		case "rejected":
			return "被拒绝"
		}
	case "kind":
		switch trimmed {
		case "cera":
			return "点券消费"
		case "chat":
			return "对话"
		case "dungeon":
			return "副本"
		case "raid":
			return "团本"
		case "gold_consign":
			return "金币寄售"
		case "auction":
			return "拍卖行"
		case "scene":
			return "场景加载诊断"
		}
	case "source":
		// 点券消费的来源：商城成功那条记录**没有** source，所以这里只翻译已知的两种。
		// 不认识的取值原样显示（服务端以后加新来源时不会变成空白）。
		switch trimmed {
		case "gold_consign":
			return "金币寄售"
		case "aligned_command":
			return "系统命令（金库等）"
		}
	}
	return text
}

// recAccountText 账号显示：一律显示真实账号名（记录里存的是 dnf:<sha256(用户名)> 主键，
// 不可逆且运营看不懂）。翻译不到的（从未注册过、外部工具造的 wpf-… 主键）原样显示主键
// 并标注"未匹配到账号名"，绝不静默留空。
func recAccountText(accountID string, accountNames map[string]string) string {
	if name, ok := accountNames[accountID]; ok && strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	// 本页映射里没有时再查一次全局缓存（同一次会话里别的页签可能已经翻译过这个账号）。
	if name := acctNames.cached(accountID); name != "" {
		return name
	}
	return accountID + "（未匹配到账号名）"
}

// recNumberSeconds 判断某字段是不是"秒数"（JSON 解出来的数字是 float64）。
func recNumberSeconds(key string, value any) (float64, bool) {
	if key != "elapsed_seconds" {
		return 0, false
	}
	switch n := value.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// parseRecSeconds 解析字符串形态的秒数。
func parseRecSeconds(raw string) (float64, bool) {
	var seconds float64
	if _, err := fmt.Sscanf(raw, "%f", &seconds); err != nil {
		return 0, false
	}
	return seconds, true
}

// recDurationText 秒数 → "X 分 Y 秒"（不足 1 分钟只显示秒；顺便给出原始秒数便于核对）。
func recDurationText(seconds float64) string {
	total := int(seconds + 0.5)
	if total < 0 {
		total = 0
	}
	minutes := total / 60
	rest := total % 60
	if minutes == 0 {
		return fmt.Sprintf("%d 秒", rest)
	}
	if rest == 0 {
		return fmt.Sprintf("%d 分（%d 秒）", minutes, total)
	}
	return fmt.Sprintf("%d 分 %d 秒（%d 秒）", minutes, rest, total)
}
