package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"dnf90ops/internal/store"
)

type accountJSON struct {
	AccountID   string `json:"account_id"`
	Username    string `json:"username"`
	State       string `json:"state"`
	LastLoginIP string `json:"last_login_ip,omitempty"`
	LastLoginAt any    `json:"last_login_at,omitempty"`
	CreatedAt   any    `json:"created_at,omitempty"`
}

type banJSON struct {
	ID        int64  `json:"id"`
	BanType   string `json:"ban_type"`
	BanValue  string `json:"ban_value"`
	Username  string `json:"username"`
	Reason    string `json:"reason"`
	Operator  string `json:"operator"`
	BannedAt  any    `json:"banned_at"`
	ExpiresAt any    `json:"expires_at"`
	RevokedAt any    `json:"revoked_at"`
	Active    bool   `json:"active"`
}

type classGrowView struct {
	FirstGrow int      `json:"first_grow"`
	Name      string   `json:"name"`
	Awakes    []string `json:"awakes"`
}

type classView struct {
	JobID int             `json:"job_id"`
	Name  string          `json:"name"`
	Grows []classGrowView `json:"grows"`
}

type allCharacterRow struct {
	AccountID   string `json:"account_id"`
	Username    string `json:"username"`
	CharacterID string `json:"character_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Gold        int64  `json:"gold"`
	Cera        int64  `json:"cera"`
	Online      bool   `json:"online"`
}

type allCharactersResponse struct {
	Accounts   int               `json:"accounts"`
	Characters int               `json:"characters"`
	Rows       []allCharacterRow `json:"rows"`
	ScannedAt  time.Time         `json:"scanned_at"`
	Cached     bool              `json:"cached"`
}

func nullableTime(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time.Truncate(time.Second)
}

func accountRowJSON(row store.AccountRow) accountJSON {
	return accountJSON{
		AccountID:   row.AccountID,
		Username:    row.Username,
		State:       row.State,
		LastLoginIP: row.LastLoginIP,
		LastLoginAt: nullableTime(row.LastLoginAt),
		CreatedAt:   nullableTime(row.CreatedAt),
	}
}

func (s *webServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	data := map[string]any{
		"game_online": false,
	}
	if raw, err := s.gm.status(ctx); err != nil {
		data["game_error"] = err.Error()
	} else {
		data["game_online"] = true
		if len(raw) > 0 && string(raw) != "null" {
			data["status"] = json.RawMessage(raw)
		}
	}
	if raw, err := s.gm.channels(ctx); err == nil && len(raw) > 0 && string(raw) != "null" {
		var overview struct {
			TotalChannels   int `json:"total_channels"`
			EnabledChannels int `json:"enabled_channels"`
			Channels        []struct {
				Ordinary bool `json:"ordinary"`
			} `json:"channels"`
		}
		if json.Unmarshal(raw, &overview) == nil {
			count := overview.EnabledChannels
			if count == 0 {
				count = overview.TotalChannels
			}
			data["channels"] = count
			ordinary := 0
			for _, channel := range overview.Channels {
				if channel.Ordinary {
					ordinary++
				}
			}
			data["ordinary_channels"] = ordinary
		}
	}
	if s.store != nil {
		if total, active, err := s.store.CountBans(ctx); err == nil {
			data["ban_total"] = total
			data["ban_active"] = active
		}
	}
	if _, ok := data["ban_total"]; !ok {
		data["ban_total"] = 0
		data["ban_active"] = 0
	}
	writeOK(w, data)
}

func (s *webServer) handleSearchAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.dbRequired(w) {
		return
	}
	limit := queryInt(r, "limit", 300)
	rows, err := s.store.SearchAccounts(r.Context(), r.URL.Query().Get("q"), limit)
	if err != nil {
		fail(w, http.StatusInternalServerError, "查询账号失败: "+err.Error())
		return
	}
	out := make([]accountJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, accountRowJSON(row))
	}
	writeOK(w, out)
}

func (s *webServer) handleAccountCharacters(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	if accountID == "" {
		fail(w, http.StatusBadRequest, "缺少账号 ID")
		return
	}
	raw, err := s.gm.accountCharacters(r.Context(), accountID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	var rows []struct {
		CharacterID    string `json:"character_id"`
		AccountID      string `json:"account_id"`
		Slot           int    `json:"slot"`
		Name           string `json:"name"`
		Job            string `json:"job"`
		ProfessionName string `json:"profession_name"`
		Level          int    `json:"level"`
		Gold           int64  `json:"gold"`
		Online         bool   `json:"online"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		writeRawOK(w, raw)
		return
	}
	writeOK(w, rows)
}

func (s *webServer) handleAccountDetail(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	if accountID == "" {
		fail(w, http.StatusBadRequest, "缺少账号 ID")
		return
	}
	raw, err := s.gm.accountDetail(r.Context(), accountID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) lookupAccount(ctx context.Context, value string) (*store.Account, bool) {
	if s.store == nil {
		return nil, false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	if account, err := s.store.GetAccountByID(ctx, value); err == nil && account != nil {
		return account, true
	}
	if account, err := s.store.GetAccountByUsername(ctx, value); err == nil && account != nil {
		return account, true
	}
	return nil, false
}

func (s *webServer) firstCharacter(ctx context.Context, accountID string) (*gmCharacterInfo, error) {
	characters, err := s.gm.nativeCharacters(ctx, accountID, "")
	if err != nil {
		return nil, err
	}
	if len(characters) == 0 {
		return nil, fmt.Errorf("该账号下没有角色")
	}
	return &characters[0], nil
}

// resolveCharacterName 把“账号 / 用户名 / 角色名”解析成角色。
//
// 注意：纯数字既可能是登录器用户名（本服大量账号就是手机号/QQ号），也可能是
// 角色名。这里绝不能把它当成 character_id 直接返回，否则邮件、点券等页面
// 填 614659186 这样的账号会被误判成 ID=614659186，进而报“角色不存在”。
func (s *webServer) resolveCharacterName(ctx context.Context, ref string) (*gmCharacterInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("缺少角色参数")
	}
	if account, ok := s.lookupAccount(ctx, ref); ok {
		return s.firstCharacter(ctx, account.AccountID)
	}
	characters, err := s.gm.nativeCharacters(ctx, "", ref)
	if err == nil {
		for i := range characters {
			if characters[i].Name == ref {
				return &characters[i], nil
			}
		}
		if len(characters) == 1 {
			return &characters[0], nil
		}
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("没找到账号或角色【%s】。请确认：用户名是登录器注册的完整账号，角色名是游戏内实际角色名，且该账号已经创建过角色", ref)
}

func (s *webServer) characterIDFromBody(w http.ResponseWriter, r *http.Request, body map[string]any) (string, bool) {
	id := strings.TrimSpace(asString(body["character_id"]))
	if id != "" {
		return id, true
	}
	ref := strings.TrimSpace(asString(body["character"]))
	if ref == "" {
		fail(w, http.StatusBadRequest, "缺少 character_id 或 character")
		return "", false
	}
	character, err := s.resolveCharacterName(r.Context(), ref)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return "", false
	}
	return character.CharacterID, true
}

func (s *webServer) handleFindCharacter(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	result := map[string]any{
		"keyword":    keyword,
		"characters": []gmCharacterInfo{},
		"matched_by": "未找到",
	}
	if keyword == "" {
		writeOK(w, result)
		return
	}
	if account, ok := s.lookupAccount(r.Context(), keyword); ok {
		characters, err := s.gm.nativeCharacters(r.Context(), account.AccountID, "")
		if err != nil {
			fail(w, http.StatusOK, err.Error())
			return
		}
		result["characters"] = characters
		if strings.HasPrefix(strings.ToLower(keyword), "dnf:") {
			result["matched_by"] = "账号ID"
		} else {
			result["matched_by"] = "用户名"
		}
		writeOK(w, result)
		return
	}
	characters, err := s.gm.nativeCharacters(r.Context(), "", keyword)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	if len(characters) > 0 {
		result["characters"] = characters
		result["matched_by"] = "角色名"
	}
	writeOK(w, result)
}

func (s *webServer) buildAllCharacters(ctx context.Context, refresh bool) (*allCharactersResponse, error) {
	s.allMu.Lock()
	defer s.allMu.Unlock()
	if !refresh && s.allCache != nil && time.Since(s.allScanned) < time.Minute {
		copyValue := *s.allCache
		copyValue.Cached = true
		return &copyValue, nil
	}
	if s.store == nil {
		return nil, fmt.Errorf("账号数据库不可用")
	}
	accounts, err := s.store.SearchAccounts(ctx, "", 5000)
	if err != nil {
		return nil, err
	}
	type accountResult struct {
		rows []allCharacterRow
	}
	results := make([]accountResult, len(accounts))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		go func(index int, account store.AccountRow) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			characters, err := s.gm.nativeCharacters(ctx, account.AccountID, "")
			if err != nil {
				return
			}
			rows := make([]allCharacterRow, 0, len(characters))
			for _, character := range characters {
				rows = append(rows, allCharacterRow{
					AccountID:   account.AccountID,
					Username:    account.Username,
					CharacterID: character.CharacterID,
					Name:        character.Name,
					Level:       character.Level,
					Gold:        character.Gold,
					Cera:        character.Cera,
					Online:      character.Online,
				})
			}
			results[index].rows = rows
		}(i, accounts[i])
	}
	wg.Wait()

	rows := make([]allCharacterRow, 0, 64)
	for _, result := range results {
		rows = append(rows, result.rows...)
	}
	out := &allCharactersResponse{
		Accounts:   len(accounts),
		Characters: len(rows),
		Rows:       rows,
		ScannedAt:  time.Now(),
	}
	s.allCache = out
	s.allScanned = time.Now()
	copyValue := *out
	copyValue.Cached = false
	return &copyValue, nil
}

func (s *webServer) handleAllCharacters(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result, err := s.buildAllCharacters(ctx, refresh)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeOK(w, result)
}

func (s *webServer) handleClassTree(w http.ResponseWriter, r *http.Request) {
	out := make([]classView, 0, len(acctClassTree))
	for _, cls := range acctClassTree {
		grows := make([]classGrowView, 0, len(cls.Grows))
		for _, grow := range cls.Grows {
			awakes := make([]string, len(grow.Awakes))
			copy(awakes, grow.Awakes)
			grows = append(grows, classGrowView{
				FirstGrow: grow.FirstGrow,
				Name:      grow.Name,
				Awakes:    awakes,
			})
		}
		out = append(out, classView{
			JobID: cls.JobID,
			Name:  cls.Name,
			Grows: grows,
		})
	}
	writeOK(w, out)
}

func (s *webServer) handleItems(w http.ResponseWriter, r *http.Request) {
	values := url.Values{}
	query := r.URL.Query()
	for _, key := range []string{"q", "kind", "tag", "segment", "group", "job", "expiration"} {
		if value := strings.TrimSpace(query.Get(key)); value != "" {
			values.Set(key, value)
		}
	}
	for _, key := range []string{"min_level", "max_level", "rarity", "offset"} {
		if value := strings.TrimSpace(query.Get(key)); value != "" {
			values.Set(key, value)
		}
	}
	limit := queryInt(r, "limit", 50)
	if limit < 1 || limit > 200 {
		limit = 50
	}
	values.Set("limit", strconv.Itoa(limit))
	if values.Get("rarity") == "" {
		values.Set("rarity", "-1")
	}
	if values.Get("offset") == "" {
		values.Set("offset", "0")
	}
	raw, err := s.gm.itemsPage(r.Context(), values)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, normalizeItemsPage(raw))
}

func (s *webServer) handleItemDetail(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gm.itemDetail(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleItemCategories(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gm.itemCategories(r.Context())
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleItemMailOptions(w http.ResponseWriter, r *http.Request) {
	values := url.Values{}
	for key, list := range r.URL.Query() {
		for _, value := range list {
			values.Add(key, value)
		}
	}
	raw, err := s.gm.itemMailOptions(r.Context(), strings.TrimSpace(r.PathValue("id")), values)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleAvatarSets(w http.ResponseWriter, r *http.Request) {
	values := url.Values{}
	for key, list := range r.URL.Query() {
		for _, value := range list {
			values.Add(key, value)
		}
	}
	raw, err := s.gm.avatarSets(r.Context(), values)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleBanList(w http.ResponseWriter, r *http.Request) {
	if !s.dbRequired(w) {
		return
	}
	limit := queryInt(r, "limit", 300)
	if limit < 1 || limit > 5000 {
		limit = 300
	}
	rows, err := s.store.ListBans(r.Context(), limit)
	if err != nil {
		fail(w, http.StatusOK, "查询封禁失败: "+err.Error())
		return
	}
	now := time.Now()
	out := make([]banJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, banJSON{
			ID:        row.ID,
			BanType:   row.BanType,
			BanValue:  row.BanValue,
			Username:  row.Username,
			Reason:    row.Reason,
			Operator:  row.Operator,
			BannedAt:  row.BannedAt,
			ExpiresAt: nullableTime(row.ExpiresAt),
			RevokedAt: nullableTime(row.RevokedAt),
			Active:    row.Active(now),
		})
	}
	writeOK(w, out)
}

func (s *webServer) resolveBanTarget(ctx context.Context, targetType, target string) (string, string, string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", "", "", fmt.Errorf("封禁目标不能为空")
	}
	switch strings.ToLower(strings.TrimSpace(targetType)) {
	case "", "username":
		if s.store == nil {
			return "", "", "", fmt.Errorf("账号数据库不可用")
		}
		resolved, err := s.store.AccountBanTargets(ctx, target)
		if err != nil {
			return "", "", "", fmt.Errorf("账号不存在或查询失败: %w", err)
		}
		return store.BanAccount, resolved.AccountID, resolved.Username, nil
	case "account":
		if s.store == nil {
			return "", "", "", fmt.Errorf("账号数据库不可用")
		}
		resolved, err := s.store.AccountBanTargets(ctx, target)
		if err == nil {
			return store.BanAccount, resolved.AccountID, resolved.Username, nil
		}
		if store.LooksLikeAccountID(target) {
			return store.BanAccount, target, "", nil
		}
		return "", "", "", fmt.Errorf("账号不存在或查询失败: %w", err)
	case "ip":
		return store.BanIP, target, "", nil
	case "machine":
		return store.BanMachine, target, "", nil
	default:
		return "", "", "", fmt.Errorf("不支持的封禁类型：%s", targetType)
	}
}

func (s *webServer) handleBanCreate(w http.ResponseWriter, r *http.Request) {
	if !s.dbRequired(w) {
		return
	}
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	targetType := asString(body["target_type"])
	target := asString(body["target"])
	banType, banValue, username, err := s.resolveBanTarget(r.Context(), targetType, target)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	reason := strings.TrimSpace(asString(body["reason"]))
	if reason == "" {
		reason = "网页后台封禁"
	}
	days := int(asInt64(body["days"]))
	if days < 0 {
		days = 0
	}
	record, err := s.store.Ban(r.Context(), banType, banValue, username, reason, "webadmin", days)
	if err != nil {
		fail(w, http.StatusOK, "封禁失败: "+err.Error())
		return
	}
	kicked := 0
	kickError := ""
	if asBool(body["kick"]) {
		kicked, err = s.gm.kickSessions(r.Context(), banType, banValue)
		if err != nil {
			kickError = err.Error()
		}
		if banType == store.BanAccount {
			_, _ = s.store.CreateGMKickEvent(r.Context(), banValue, "kick", reason, "webadmin")
		}
	}
	writeOK(w, map[string]any{
		"ban_type":   record.BanType,
		"ban_value":  record.BanValue,
		"username":   record.Username,
		"days":       days,
		"kicked":     kicked,
		"kick_error": kickError,
	})
}

func (s *webServer) handleBanDelete(w http.ResponseWriter, r *http.Request) {
	if !s.dbRequired(w) {
		return
	}
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	banType := strings.TrimSpace(asString(body["ban_type"]))
	banValue := strings.TrimSpace(asString(body["ban_value"]))
	if banType == "" || banValue == "" {
		fail(w, http.StatusBadRequest, "缺少 ban_type 或 ban_value")
		return
	}
	ok, err := s.store.Unban(r.Context(), banType, banValue)
	if err != nil {
		fail(w, http.StatusOK, "解封失败: "+err.Error())
		return
	}
	extra := make([]string, 0, 4)
	if banType == store.BanAccount {
		if target, err := s.store.AccountBanTargets(r.Context(), banValue); err == nil {
			if related, err := s.store.ListActiveIPMachineBansByUsername(r.Context(), target.Username); err == nil {
				for _, item := range related {
					if removed, err := s.store.Unban(r.Context(), item.BanType, item.BanValue); err == nil && removed {
						extra = append(extra, item.BanType+"="+item.BanValue)
					}
				}
			}
		}
	}
	writeOK(w, map[string]any{
		"unbanned": ok,
		"extra":    extra,
	})
}

func (s *webServer) handleBanKick(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	banType := strings.TrimSpace(asString(body["ban_type"]))
	banValue := strings.TrimSpace(asString(body["ban_value"]))
	kicked, err := s.gm.kickSessions(r.Context(), banType, banValue)
	if err != nil {
		fail(w, http.StatusOK, "踢下线失败: "+err.Error())
		return
	}
	if banType == store.BanAccount && s.store != nil {
		_, _ = s.store.CreateGMKickEvent(r.Context(), banValue, "kick", "网页后台踢下线", "webadmin")
	}
	writeOK(w, map[string]any{"kicked": kicked})
}

func (s *webServer) handleRecordsMeta(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gm.recordsMeta(r.Context())
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, normalizeRecordsMeta(raw))
}

func (s *webServer) handleRecordsQuery(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if asString(body["kind"]) == "" {
		body["kind"] = "dungeon"
	}
	limit := int(asInt64(body["limit"]))
	if limit < 1 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	body["limit"] = limit
	if _, ok := body["offset"]; !ok {
		body["offset"] = 0
	}
	raw, err := s.gm.recordsQuery(r.Context(), body)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleRaidRewardPools(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gm.raidRewardPools(r.Context())
	if err != nil {
		if upstreamMissingRoute(err) {
			writeOK(w, map[string]any{
				"groups":    []any{},
				"overrides": map[string]any{"weights": map[string]any{}},
				"notes": []string{
					"当前游戏服务端未启用团本奖励池接口（HTTP 404）。网页后台本身正常；需要 dnf90server 支持 /dnf/gm/gameplay/raid-reward-pools 后才能读取和修改奖励池。",
				},
				"unsupported": true,
			})
			return
		}
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleRaidRewardRateGet(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gm.raidRewardRate(r.Context())
	if err != nil {
		if upstreamMissingRoute(err) {
			writeOK(w, map[string]any{
				"weights":     map[string]any{},
				"items":       map[string]any{},
				"unsupported": true,
			})
			return
		}
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleRaidRewardRateSet(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	raw, err := s.gm.setRaidRewardRate(r.Context(), body)
	if err != nil {
		if upstreamMissingRoute(err) {
			fail(w, http.StatusOK, "当前游戏服务端未启用团本奖励池接口，未做任何修改")
			return
		}
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleChannelsReload(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gm.reloadChannels(r.Context())
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleSendMail(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	attachments := make([]map[string]any, 0, 10)
	if rawAttachments, ok := body["attachments"].([]any); ok {
		for _, raw := range rawAttachments {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			itemID := asInt64(item["item_id"])
			count := asInt64(item["count"])
			if itemID <= 0 || count <= 0 {
				continue
			}
			attachments = append(attachments, map[string]any{
				"item_id":         itemID,
				"count":           count,
				"bind":            asBool(item["bind"]),
				"reinforce_level": asInt64(item["reinforce_level"]),
				"amplify_level":   asInt64(item["amplify_level"]),
				"amplify_type":    asInt64(item["amplify_type"]),
			})
		}
	}
	if len(attachments) > 10 {
		fail(w, http.StatusBadRequest, "单封邮件最多 10 件物品")
		return
	}
	gold := asInt64(body["gold"])
	if len(attachments) == 0 && gold == 0 {
		fail(w, http.StatusBadRequest, "邮件至少要有物品或金币")
		return
	}
	title := strings.TrimSpace(asString(body["title"]))
	if title == "" {
		title = "GM 发放"
	}
	content := strings.TrimSpace(asString(body["body"]))
	if content == "" {
		content = "请在邮箱中领取。"
	}
	payload := map[string]any{
		"character_id": characterID,
		"title":        title,
		"body":         content,
		"gold":         gold,
		"attachments":  attachments,
	}
	raw, err := s.gm.sendMail(r.Context(), payload)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	log.Printf("发送邮件 character_id=%s items=%d gold=%d", characterID, len(attachments), gold)
	writeRawOK(w, raw)
}

func (s *webServer) handleRichMail(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	itemID := asInt64(body["item_id"])
	if itemID <= 0 {
		fail(w, http.StatusBadRequest, "缺少 item_id")
		return
	}
	count := asInt64(body["count"])
	if count <= 0 {
		count = 1
	}
	emblemIDs := make([]uint32, 0, 8)
	for _, value := range asStringSlice(body["emblem_item_ids"]) {
		if parsed, err := strconv.ParseUint(value, 10, 32); err == nil && parsed > 0 {
			emblemIDs = append(emblemIDs, uint32(parsed))
		}
	}
	if rawList, ok := body["emblem_item_ids"].([]any); ok {
		emblemIDs = emblemIDs[:0]
		for _, value := range rawList {
			if parsed := asInt64(value); parsed > 0 {
				emblemIDs = append(emblemIDs, uint32(parsed))
			}
		}
	}
	title := strings.TrimSpace(asString(body["title"]))
	if title == "" {
		title = "GM 发放"
	}
	content := strings.TrimSpace(asString(body["body"]))
	if content == "" {
		content = "请在邮箱中领取物品。"
	}
	payload := map[string]any{
		"item_id":              itemID,
		"count":                count,
		"title":                title,
		"body":                 content,
		"expire_days":          asInt64(body["expire_days"]),
		"reinforce":            asInt64(body["reinforce"]),
		"amplify_type":         asInt64(body["amplify_type"]),
		"equipment_quality":    asString(body["equipment_quality"]),
		"forging":              asInt64(body["forging"]),
		"emblem_item_ids":      emblemIDs,
		"enchant_bead_item_id": asInt64(body["enchant_bead_item_id"]),
	}
	raw, err := s.gm.sendRichMail(r.Context(), characterID, payload)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleGrantCera(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	amount := asInt64(body["amount"])
	if amount <= 0 {
		fail(w, http.StatusBadRequest, "数量必须大于 0")
		return
	}
	currency := strings.TrimSpace(asString(body["currency"]))
	switch currency {
	case "cera", "token_cera", "happy_token_cera", "gold":
	default:
		fail(w, http.StatusBadRequest, "不支持的币种")
		return
	}
	mode := strings.TrimSpace(asString(body["mode"]))
	if mode != "add" && mode != "set" {
		fail(w, http.StatusBadRequest, "发放方式只支持 add 或 set")
		return
	}
	characterID := ""
	if name := strings.TrimSpace(asString(body["character_name"])); name != "" {
		character, err := s.resolveCharacterName(r.Context(), name)
		if err != nil {
			fail(w, http.StatusOK, err.Error())
			return
		}
		characterID = character.CharacterID
	} else if accountRef := strings.TrimSpace(asString(body["account_id"])); accountRef != "" {
		account, ok := s.lookupAccount(r.Context(), accountRef)
		if !ok {
			fail(w, http.StatusOK, "账号不存在："+accountRef)
			return
		}
		character, err := s.firstCharacter(r.Context(), account.AccountID)
		if err != nil {
			fail(w, http.StatusOK, err.Error())
			return
		}
		characterID = character.CharacterID
	} else {
		fail(w, http.StatusBadRequest, "请填写角色名或账号")
		return
	}
	reply, err := s.gm.walletUpdate(r.Context(), map[string]any{
		"character_id": characterID,
		"currency":     currency,
		"mode":         mode,
		"amount":       amount,
	})
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	var result map[string]any
	if len(reply.Data) > 0 {
		_ = json.Unmarshal(reply.Data, &result)
	}
	if result == nil {
		result = map[string]any{}
	}
	labels := map[string]string{
		"cera":             "点券",
		"token_cera":       "代币券",
		"happy_token_cera": "欢乐代币券",
		"gold":             "金币",
	}
	modes := map[string]string{"add": "加量", "set": "设为"}
	result["biz"] = labels[currency] + modes[mode]
	result["character_id"] = characterID
	writeOK(w, result)
}

func (s *webServer) handleWalletQuery(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := strings.TrimSpace(asString(body["character_name"]))
	if name == "" {
		fail(w, http.StatusBadRequest, "请填写角色名")
		return
	}
	character, err := s.resolveCharacterName(r.Context(), name)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	raw, err := s.gm.walletQuery(r.Context(), map[string]any{
		"character_id": character.CharacterID,
	})
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleBlackDiamondGet(w http.ResponseWriter, r *http.Request) {
	characterID := strings.TrimSpace(r.URL.Query().Get("id"))
	if characterID == "" {
		fail(w, http.StatusBadRequest, "缺少角色 ID")
		return
	}
	raw, err := s.gm.blackDiamondStatus(r.Context(), characterID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleBlackDiamondPost(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	days := int(asInt64(body["days"]))
	if days <= 0 {
		days = 30
	}
	if days > 3650 {
		fail(w, http.StatusBadRequest, "黑钻天数需在 1~3650 之间")
		return
	}
	raw, err := s.gm.activateBlackDiamond(r.Context(), characterID, days)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleSetLevel(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.setLevel(r.Context(), characterID, int(asInt64(body["level"])))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleSetSPTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.setSPTP(r.Context(), characterID,
		asInt64(body["sp"]), asInt64(body["tp"]), asBool(body["extend_total"]))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleSetJob(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.setJob(r.Context(), characterID,
		int(asInt64(body["job"])),
		int(asInt64(body["first_grow"])),
		int(asInt64(body["second_grow"])))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleSetCareer(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.setCareer(r.Context(), characterID,
		int(asInt64(body["job"])),
		int(asInt64(body["first"])),
		int(asInt64(body["awakening"])))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleSetSkillBonus(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.setSkillBonus(r.Context(), characterID,
		asInt64(body["bonus_sp"]), asInt64(body["bonus_tp"]))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleRepairHappy(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.repairHappy(r.Context(), characterID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

var (
	dungeonsOnce sync.Once
	dungeonsMap  map[string]int
)

func maxDifficultyDungeons() map[string]int {
	dungeonsOnce.Do(func() {
		dungeonsMap = make(map[string]int)
		_ = json.Unmarshal(dungeonsMaxDifficultyRaw, &dungeonsMap)
	})
	return dungeonsMap
}

func (s *webServer) handleUnlockDungeons(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	dungeons := maxDifficultyDungeons()
	if len(dungeons) == 0 {
		fail(w, http.StatusInternalServerError, "内嵌副本难度表为空")
		return
	}
	raw, err := s.gm.unlockDungeons(r.Context(), characterID, dungeons)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	var result map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &result)
	}
	if result == nil {
		result = map[string]any{}
	}
	result["submitted"] = len(dungeons)
	writeOK(w, result)
}

func (s *webServer) handleUnlockEquipSlots(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.unlockEquipSlots(r.Context(), characterID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleUnlockDungeonDifficulties(w http.ResponseWriter, r *http.Request) {
	s.characterAction(w, r, s.gm.unlockDungeonDifficulties)
}

func (s *webServer) handleUnlockHell(w http.ResponseWriter, r *http.Request) {
	s.characterAction(w, r, s.gm.unlockHell)
}

func (s *webServer) characterAction(w http.ResponseWriter, r *http.Request,
	fn func(context.Context, string) (json.RawMessage, error)) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := fn(r.Context(), characterID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleDungeonAccess(w http.ResponseWriter, r *http.Request) {
	s.characterGet(w, r, s.gm.dungeonAccess)
}

func (s *webServer) handleEquipmentSlots(w http.ResponseWriter, r *http.Request) {
	s.characterGet(w, r, s.gm.equipmentSlots)
}

func (s *webServer) handleInventory(w http.ResponseWriter, r *http.Request) {
	s.characterGet(w, r, s.gm.inventory)
}

func (s *webServer) handleMailbox(w http.ResponseWriter, r *http.Request) {
	s.characterGet(w, r, s.gm.mailbox)
}

func (s *webServer) characterGet(w http.ResponseWriter, r *http.Request,
	fn func(context.Context, string) (json.RawMessage, error)) {
	characterID := strings.TrimSpace(r.URL.Query().Get("id"))
	if characterID == "" {
		fail(w, http.StatusBadRequest, "缺少角色 ID")
		return
	}
	raw, err := fn(r.Context(), characterID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleCharacterJob(w http.ResponseWriter, r *http.Request) {
	characterID := strings.TrimSpace(r.URL.Query().Get("id"))
	if characterID == "" {
		fail(w, http.StatusBadRequest, "缺少角色 ID")
		return
	}
	raw, err := s.gm.characterJob(r.Context(), characterID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	var job struct {
		CharacterID string `json:"character_id"`
		Job         int    `json:"job"`
		FirstGrow   int    `json:"first_grow"`
		SecondGrow  int    `json:"second_grow"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		fail(w, http.StatusOK, "解析职业信息失败: "+err.Error())
		return
	}
	jobName, growName, awakeName := "未知职业", "未知转职", ""
	for _, cls := range acctClassTree {
		if cls.JobID != job.Job {
			continue
		}
		jobName = cls.Name
		for _, grow := range cls.Grows {
			if grow.FirstGrow != job.FirstGrow {
				continue
			}
			growName = grow.Name
			if job.SecondGrow > 0 && job.SecondGrow <= len(grow.Awakes) {
				awakeName = grow.Awakes[job.SecondGrow-1]
			}
			break
		}
		break
	}
	display := jobName + " / " + growName
	if awakeName != "" {
		display += " / " + awakeName
	}
	writeOK(w, map[string]any{
		"character_id": job.CharacterID,
		"job":          job.Job,
		"first_grow":   job.FirstGrow,
		"second_grow":  job.SecondGrow,
		"job_name":     jobName,
		"grow_name":    growName,
		"awake_name":   awakeName,
		"display":      display,
	})
}

func (s *webServer) handleCharacterDetail(w http.ResponseWriter, r *http.Request) {
	characterID := strings.TrimSpace(r.URL.Query().Get("id"))
	if characterID == "" {
		characterID = strings.TrimSpace(r.URL.Query().Get("character_id"))
	}
	if characterID == "" {
		fail(w, http.StatusBadRequest, "缺少角色 ID")
		return
	}
	raw, err := s.gm.characterDetail(r.Context(), characterID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleRaidAdmissionCounters(w http.ResponseWriter, r *http.Request) {
	characterID := strings.TrimSpace(r.URL.Query().Get("id"))
	if characterID == "" {
		characterID = strings.TrimSpace(r.URL.Query().Get("character_id"))
	}
	if characterID == "" {
		fail(w, http.StatusBadRequest, "缺少角色 ID")
		return
	}
	raw, err := s.gm.raidAdmissionCounters(r.Context(), characterID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleResetRaidAdmissionCounter(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raid := strings.ToLower(strings.TrimSpace(asString(body["raid"])))
	counter := strings.ToLower(strings.TrimSpace(asString(body["counter"])))
	if raid == "" || counter == "" {
		fail(w, http.StatusBadRequest, "缺少团本或次数类型")
		return
	}
	raw, err := s.gm.resetRaidAdmissionCounter(r.Context(), characterID, raid, counter)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleSetGold(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.setGold(r.Context(), characterID, asInt64(body["value"]))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleSetFatigue(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	mode := strings.ToLower(strings.TrimSpace(asString(body["mode"])))
	if mode != "max" && mode != "set" {
		fail(w, http.StatusBadRequest, "mode 只支持 max（回满）或 set（指定值）")
		return
	}
	raw, err := s.gm.setFatigue(r.Context(), characterID, mode, asInt64(body["value"]))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleSetAccountCubes(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.setAccountCubes(r.Context(), characterID,
		int(asInt64(body["selection"])), asInt64(body["count"]))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleDeleteCharacter(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.deleteCharacter(r.Context(), characterID, asString(body["confirm_name"]))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleInventoryDelete(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	payload := map[string]any{
		"container": asString(body["container"]),
		"key":       asString(body["key"]),
		"count":     asInt64(body["count"]),
	}
	raw, err := s.gm.inventoryDelete(r.Context(), characterID, payload)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleInventoryClear(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.inventoryClear(r.Context(), characterID, asString(body["container"]))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleMailboxClear(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.mailboxClear(r.Context(), characterID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleMailboxDelete(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := s.gm.mailboxDelete(r.Context(), characterID,
		asStringSlice(body["mail_ids"]), asBool(body["force"]))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleEquipUnequip(w http.ResponseWriter, r *http.Request) {
	s.equipAction(w, r, s.gm.equipUnequip)
}

func (s *webServer) handleEquipDelete(w http.ResponseWriter, r *http.Request) {
	s.equipAction(w, r, s.gm.equipDelete)
}

func (s *webServer) equipAction(w http.ResponseWriter, r *http.Request,
	fn func(context.Context, string, string) (json.RawMessage, error)) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	raw, err := fn(r.Context(), characterID, asString(body["key"]))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleQuests(w http.ResponseWriter, r *http.Request) {
	characterID := strings.TrimSpace(r.URL.Query().Get("id"))
	if characterID == "" {
		fail(w, http.StatusBadRequest, "缺少角色 ID")
		return
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("kw"))
	if keyword == "" {
		raw, err := s.gm.quests(r.Context(), characterID)
		if err != nil {
			fail(w, http.StatusOK, err.Error())
			return
		}
		writeRawOK(w, raw)
		return
	}
	limit := queryInt(r, "limit", 200)
	raw, err := s.gm.questsSearch(r.Context(), characterID, keyword, limit)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	var data struct {
		Character json.RawMessage   `json:"character"`
		Quests    []json.RawMessage `json:"quests"`
	}
	result := map[string]any{"search": []any{}}
	if json.Unmarshal(raw, &data) == nil {
		if len(data.Character) > 0 {
			result["character"] = json.RawMessage(data.Character)
		}
		if data.Quests != nil {
			result["search"] = data.Quests
		}
	} else {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil {
			result["search"] = list
		}
	}
	writeOK(w, result)
}

func (s *webServer) handleQuestDetail(w http.ResponseWriter, r *http.Request) {
	characterID := strings.TrimSpace(r.URL.Query().Get("id"))
	if characterID == "" {
		characterID = strings.TrimSpace(r.URL.Query().Get("character_id"))
	}
	questID := strings.TrimSpace(r.PathValue("qid"))
	if characterID == "" || questID == "" {
		fail(w, http.StatusBadRequest, "缺少角色 ID 或任务 ID")
		return
	}
	raw, err := s.gm.questDetail(r.Context(), characterID, questID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleQuestAction(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	action := strings.TrimSpace(asString(body["action"]))
	switch action {
	case "accept", "ready", "give-up", "complete", "uncomplete":
	default:
		fail(w, http.StatusBadRequest, "不支持的任务操作："+action)
		return
	}
	questID := asString(body["quest_id"])
	if questID == "" {
		fail(w, http.StatusBadRequest, "缺少任务 ID")
		return
	}
	raw, err := s.gm.questAction(r.Context(), characterID, questID, action)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleQuestBatch(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	characterID, ok := s.characterIDFromBody(w, r, body)
	if !ok {
		return
	}
	rawIDs, _ := body["quest_ids"].([]any)
	ids := make([]int64, 0, len(rawIDs))
	for _, value := range rawIDs {
		if id := asInt64(value); id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		fail(w, http.StatusBadRequest, "quest_ids 不能为空")
		return
	}
	raw, err := s.gm.questBatch(r.Context(), characterID, ids)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeRawOK(w, raw)
}

func (s *webServer) handleAccountBlackDiamond(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	accountRef := strings.TrimSpace(asString(body["account"]))
	account, ok := s.lookupAccount(r.Context(), accountRef)
	if !ok {
		fail(w, http.StatusOK, "账号不存在："+accountRef)
		return
	}
	character, err := s.firstCharacter(r.Context(), account.AccountID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	days := int(asInt64(body["days"]))
	if days <= 0 {
		days = 30
	}
	if days > 3650 {
		fail(w, http.StatusBadRequest, "黑钻天数需在 1~3650 之间")
		return
	}
	raw, err := s.gm.activateBlackDiamond(r.Context(), character.CharacterID, days)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	if data == nil {
		data = map[string]any{}
	}
	data["days"] = days
	data["account"] = account.Username
	data["via_character"] = character.Name
	writeOK(w, data)
}

func (s *webServer) handleAccountBlackDiamondGet(w http.ResponseWriter, r *http.Request) {
	accountRef := strings.TrimSpace(r.URL.Query().Get("account"))
	account, ok := s.lookupAccount(r.Context(), accountRef)
	if !ok {
		fail(w, http.StatusOK, "账号不存在："+accountRef)
		return
	}
	character, err := s.firstCharacter(r.Context(), account.AccountID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	raw, err := s.gm.blackDiamondStatus(r.Context(), character.CharacterID)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK           bool            `json:"ok"`
		Account      string          `json:"account"`
		ViaCharacter string          `json:"via_character"`
		Data         json.RawMessage `json:"data"`
	}{
		OK: true, Account: account.Username, ViaCharacter: character.Name, Data: raw,
	})
}

func (s *webServer) handleAccountValue(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	accountRef := strings.TrimSpace(asString(body["account"]))
	account, ok := s.lookupAccount(r.Context(), accountRef)
	if !ok {
		fail(w, http.StatusOK, "账号不存在："+accountRef)
		return
	}
	field := strings.TrimSpace(asString(body["field"]))
	names := map[string]string{
		"cera":                      "点券",
		"cargo_gold":                "货物金币",
		"honor_experience":          "荣誉经验",
		"growth_capsule_experience": "成长胶囊经验",
	}
	if _, ok := names[field]; !ok {
		fail(w, http.StatusBadRequest, "字段名不支持："+field)
		return
	}
	raw, err := s.gm.accountValue(r.Context(), account.AccountID, field, asInt64(body["value"]))
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	_ = raw
	writeOK(w, map[string]any{
		"account":    account.Username,
		"field":      field,
		"field_name": names[field],
		"value":      asInt64(body["value"]),
	})
}

func (s *webServer) targetsFor(scope string, selected []string, uniqueAccount bool) ([]batchTarget, error) {
	if scope == "per_account" {
		uniqueAccount = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := s.buildAllCharacters(ctx, false)
	if err != nil {
		return nil, err
	}
	selectedSet := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		selectedSet[id] = struct{}{}
	}
	seenAccount := make(map[string]struct{})
	out := make([]batchTarget, 0, len(result.Rows))
	for _, row := range result.Rows {
		if scope == "selected" {
			if _, ok := selectedSet[row.CharacterID]; !ok {
				continue
			}
		}
		if uniqueAccount {
			if _, ok := seenAccount[row.AccountID]; ok {
				continue
			}
			seenAccount[row.AccountID] = struct{}{}
		}
		out = append(out, batchTarget{
			AccountID:   row.AccountID,
			Username:    row.Username,
			CharacterID: row.CharacterID,
			Name:        row.Name,
			Online:      row.Online,
		})
	}
	if len(out) > 5000 {
		out = out[:5000]
	}
	return out, nil
}

func (s *webServer) runBatch(job *batchJob, targets []batchTarget,
	fn func(context.Context, batchTarget) error) {
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(target batchTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := fn(ctx, target)
			result := batchResult{
				OK:      err == nil,
				Account: target.Username,
				Target:  target.Name,
			}
			if err != nil {
				result.Message = err.Error()
			} else {
				result.Message = "ok"
			}
			job.record(result)
		}(target)
	}
	wg.Wait()
	job.finish()
}

func (s *webServer) handleBatchCera(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	amount := asInt64(body["amount"])
	if amount <= 0 {
		fail(w, http.StatusBadRequest, "数量必须大于 0")
		return
	}
	currency := strings.TrimSpace(asString(body["currency"]))
	switch currency {
	case "cera", "token_cera", "happy_token_cera", "gold":
	default:
		fail(w, http.StatusBadRequest, "不支持的币种")
		return
	}
	mode := strings.TrimSpace(asString(body["mode"]))
	if mode != "add" && mode != "set" {
		fail(w, http.StatusBadRequest, "发放方式只支持 add 或 set")
		return
	}
	targets, err := s.targetsFor(asString(body["scope"]), asStringSlice(body["character_ids"]), false)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	job := s.batches.create("cera", len(targets))
	go s.runBatch(job, targets, func(ctx context.Context, target batchTarget) error {
		_, err := s.gm.walletUpdate(ctx, map[string]any{
			"character_id": target.CharacterID,
			"currency":     currency,
			"mode":         mode,
			"amount":       amount,
		})
		return err
	})
	writeOK(w, map[string]any{"job_id": job.ID, "total": len(targets)})
}

func (s *webServer) handleBatchBlackDiamond(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	days := int(asInt64(body["days"]))
	if days <= 0 {
		days = 30
	}
	if days > 3650 {
		fail(w, http.StatusBadRequest, "黑钻天数需在 1~3650 之间")
		return
	}
	targets, err := s.targetsFor("all", nil, true)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	job := s.batches.create("black-diamond", len(targets))
	go s.runBatch(job, targets, func(ctx context.Context, target batchTarget) error {
		_, err := s.gm.activateBlackDiamond(ctx, target.CharacterID, days)
		return err
	})
	writeOK(w, map[string]any{"job_id": job.ID, "total": len(targets)})
}

func (s *webServer) handleBatchMail(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	attachments := make([]map[string]any, 0, 10)
	if rawAttachments, ok := body["attachments"].([]any); ok {
		for _, raw := range rawAttachments {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			itemID := asInt64(item["item_id"])
			count := asInt64(item["count"])
			if itemID <= 0 || count <= 0 {
				continue
			}
			attachments = append(attachments, map[string]any{
				"item_id":         itemID,
				"count":           count,
				"bind":            asBool(item["bind"]),
				"reinforce_level": asInt64(item["reinforce_level"]),
				"amplify_level":   asInt64(item["amplify_level"]),
				"amplify_type":    asInt64(item["amplify_type"]),
			})
		}
	}
	gold := asInt64(body["gold"])
	if len(attachments) == 0 && gold == 0 {
		fail(w, http.StatusBadRequest, "物品邮件至少要有物品或金币")
		return
	}
	if len(attachments) > 10 {
		fail(w, http.StatusBadRequest, "单封邮件最多 10 件物品")
		return
	}
	title := strings.TrimSpace(asString(body["title"]))
	if title == "" {
		title = "GM 发放"
	}
	content := strings.TrimSpace(asString(body["body"]))
	if content == "" {
		content = "请在邮箱中领取。"
	}
	targets, err := s.targetsFor(asString(body["scope"]), asStringSlice(body["character_ids"]), false)
	if err != nil {
		fail(w, http.StatusOK, err.Error())
		return
	}
	job := s.batches.create("mail", len(targets))
	go s.runBatch(job, targets, func(ctx context.Context, target batchTarget) error {
		_, err := s.gm.sendMail(ctx, map[string]any{
			"character_id": target.CharacterID,
			"title":        title,
			"body":         content,
			"gold":         gold,
			"attachments":  attachments,
		})
		return err
	})
	writeOK(w, map[string]any{"job_id": job.ID, "total": len(targets)})
}

func (s *webServer) handleBatchGet(w http.ResponseWriter, r *http.Request) {
	job := s.batches.get(strings.TrimSpace(r.PathValue("id")))
	if job == nil {
		fail(w, http.StatusOK, "任务不存在（或已被清理）")
		return
	}
	writeOK(w, job.snapshot())
}

func (s *webServer) handleBatchList(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.batches.list())
}

// upstreamMissingRoute 判断上游错误是否只是“这条管理路由不存在”。
// 旧版 dnf90server 没有团本奖励池接口，网页后台应给出可读提示，而不是把
// 404 原文当成红色异常抛给运营。
func upstreamMissingRoute(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "http 404") || strings.Contains(text, "404 page not found")
}

// normalizeItemsPage 把游戏服务端 omitempty 掉的零值字段补回来。
//
// 网页表格直接按字段取值（例如 rarity 为 0 时显示 0，而不是“-”），所以这里
// 只做展示层归一化，不改变任何非零值。顺带把 Go 零时间 expiration_date 去掉，
// 避免界面把“无期限”显示成 0001-01-01。
func normalizeItemsPage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return raw
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return raw
	}
	items, ok := data["items"].([]any)
	if !ok {
		return raw
	}
	defaults := map[string]any{
		"minimum_level":      float64(0),
		"rarity":             float64(0),
		"stack_limit":        float64(0),
		"equipment_type":     "",
		"grade":              float64(0),
		"usable_period_days": float64(0),
		"price":              float64(0),
		"icon_index":         float64(0),
		"icon_path":          "",
	}
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		for key, value := range defaults {
			if _, exists := item[key]; !exists {
				item[key] = value
			}
		}
		if item["expiration_date"] == "0001-01-01T00:00:00Z" {
			delete(item, "expiration_date")
		}
	}
	out, err := json.Marshal(data)
	if err != nil {
		return raw
	}
	return out
}

// normalizeRecordsMeta 补回旧后台界面用到的 lines 字段（0 表示尚未统计）。
func normalizeRecordsMeta(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return raw
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return raw
	}
	files, ok := data["files"].([]any)
	if !ok {
		return raw
	}
	for _, rawFile := range files {
		file, ok := rawFile.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := file["lines"]; !exists {
			file["lines"] = float64(0)
		}
	}
	out, err := json.Marshal(data)
	if err != nil {
		return raw
	}
	return out
}
