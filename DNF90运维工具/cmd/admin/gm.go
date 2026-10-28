package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type gmClient struct {
	base  string
	token string
	http  *http.Client

	mu  sync.Mutex
	seq uint64
}

type gmReply struct {
	Data    json.RawMessage
	Message string
	Kicked  int
}

type gmCharacterInfo struct {
	CharacterID string `json:"character_id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Gold        int64  `json:"gold"`
	Cera        int64  `json:"cera"`
	Online      bool   `json:"online"`
}

func newGMClient(base, token string) *gmClient {
	return &gmClient{
		base:  strings.TrimRight(strings.TrimSpace(base), "/"),
		token: strings.TrimSpace(token),
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *gmClient) call(ctx context.Context, method, path string, body any) (*gmReply, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("构造请求失败: %w", err)
		}
		reader = bytes.NewReader(raw)
	} else if method == http.MethodPost {
		reader = strings.NewReader("{}")
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Admin-Token", c.token)
	req.Header.Set("X-Admin-Actor", "webadmin")
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Content-Type", "application/json")
		confirmPath := path
		if idx := strings.IndexByte(confirmPath, '?'); idx >= 0 {
			confirmPath = confirmPath[:idx]
		}
		req.Header.Set("X-Admin-Confirm", method+" "+confirmPath)
		c.mu.Lock()
		c.seq++
		seq := c.seq
		c.mu.Unlock()
		req.Header.Set("X-Admin-Idempotency-Key",
			fmt.Sprintf("webadmin-%s-%d", time.Now().Format("20060102T150405.000000000"), seq))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接游戏服务端失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("读取游戏服务端响应失败: %w", err)
	}

	var envelope struct {
		OK      *bool           `json:"ok"`
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   string          `json:"error"`
		Message string          `json:"message"`
		Kicked  *int            `json:"kicked"`
	}
	parseErr := json.Unmarshal(raw, &envelope)
	if parseErr != nil {
		text := strings.TrimSpace(string(raw))
		if len(text) > 300 {
			text = text[:300] + "…"
		}
		return nil, fmt.Errorf("游戏服务端返回非 JSON（HTTP %d）：%s", resp.StatusCode, text)
	}

	success := true
	if envelope.Success != nil {
		success = *envelope.Success
	}
	if envelope.OK != nil {
		success = success && *envelope.OK
	}
	if !success {
		message := strings.TrimSpace(envelope.Error)
		if message == "" {
			message = "游戏服务端拒绝了请求"
		}
		return nil, fmt.Errorf("%s", message)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(envelope.Error)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", message)
	}

	reply := &gmReply{
		Data:    envelope.Data,
		Message: envelope.Message,
	}
	if envelope.Kicked != nil {
		reply.Kicked = *envelope.Kicked
	}
	return reply, nil
}

func (c *gmClient) get(ctx context.Context, path string) (*gmReply, error) {
	return c.call(ctx, http.MethodGet, path, nil)
}

func (c *gmClient) post(ctx context.Context, path string, body any) (*gmReply, error) {
	return c.call(ctx, http.MethodPost, path, body)
}

func (c *gmClient) status(ctx context.Context) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/status")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) channels(ctx context.Context) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/channels")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) nativeCharacters(ctx context.Context, accountID, characterName string) ([]gmCharacterInfo, error) {
	values := url.Values{}
	if strings.TrimSpace(accountID) != "" {
		values.Set("account_id", strings.TrimSpace(accountID))
	}
	if strings.TrimSpace(characterName) != "" {
		values.Set("character_name", strings.TrimSpace(characterName))
	}
	reply, err := c.get(ctx, "/local/dnf-gm/characters?"+values.Encode())
	if err != nil {
		return nil, err
	}
	var rows []gmCharacterInfo
	if len(reply.Data) > 0 && string(reply.Data) != "null" {
		if err := json.Unmarshal(reply.Data, &rows); err != nil {
			return nil, fmt.Errorf("解析角色列表失败: %w", err)
		}
	}
	return rows, nil
}

func (c *gmClient) accountCharacters(ctx context.Context, accountID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/accounts/"+url.PathEscape(accountID)+"/characters")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) accountDetail(ctx context.Context, accountID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/accounts/"+url.PathEscape(accountID))
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) accountValue(ctx context.Context, accountID, field string, value int64) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/accounts/"+url.PathEscape(accountID)+"/value", map[string]any{
		"field": field,
		"value": value,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) itemsPage(ctx context.Context, values url.Values) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/items/page?"+values.Encode())
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) itemDetail(ctx context.Context, itemID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/items/"+url.PathEscape(itemID)+"/detail")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) itemCategories(ctx context.Context) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/items/categories")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) itemMailOptions(ctx context.Context, itemID string, values url.Values) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/items/"+url.PathEscape(itemID)+"/mail-options?"+values.Encode())
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) avatarSets(ctx context.Context, values url.Values) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/avatar-sets?"+values.Encode())
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) recordsMeta(ctx context.Context) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/records/meta")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) recordsQuery(ctx context.Context, body map[string]any) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/records/query", body)
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) raidRewardPools(ctx context.Context) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/gameplay/raid-reward-pools")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) raidRewardRate(ctx context.Context) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/gameplay/raid-reward-rate")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) setRaidRewardRate(ctx context.Context, body map[string]any) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/gameplay/raid-reward-rate", body)
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) reloadChannels(ctx context.Context) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/channels/reload", map[string]any{})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) blackDiamondStatus(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/local/dnf-gm/character/black-diamond?id="+url.QueryEscape(characterID))
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) activateBlackDiamond(ctx context.Context, characterID string, days int) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/local/dnf-gm/character/black-diamond", map[string]any{
		"character_id": characterID,
		"days":         days,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) setLevel(ctx context.Context, characterID string, level int) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/local/dnf-gm/character/level", map[string]any{
		"character_id": characterID,
		"level":        level,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) setSPTP(ctx context.Context, characterID string, sp, tp int64, extendTotal bool) (json.RawMessage, error) {
	body := map[string]any{
		"character_id": characterID,
		"remaining_sp": sp,
		"remaining_tp": tp,
	}
	if extendTotal {
		body["extend_total"] = true
	}
	reply, err := c.post(ctx, "/local/dnf-gm/character/sptp", body)
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) characterJob(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/local/dnf-gm/character/job?id="+url.QueryEscape(characterID))
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) setJob(ctx context.Context, characterID string, job, firstGrow, secondGrow int) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/local/dnf-gm/character/job", map[string]any{
		"character_id": characterID,
		"job":          job,
		"first_grow":   firstGrow,
		"second_grow":  secondGrow,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) setCareer(ctx context.Context, characterID string, job, first, awakening int) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/career", map[string]any{
		"job":       job,
		"first":     first,
		"awakening": awakening,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) setSkillBonus(ctx context.Context, characterID string, bonusSP, bonusTP int64) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/skill-points", map[string]any{
		"bonus_sp": bonusSP,
		"bonus_tp": bonusTP,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) repairHappy(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/local/dnf-gm/wallet/repair-happy-token-cera", map[string]any{
		"character_id": characterID,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) unlockDungeons(ctx context.Context, characterID string, dungeons map[string]int) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/local/dnf-gm/dungeons/unlock-all", map[string]any{
		"character_id": characterID,
		"dungeons":     dungeons,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) unlockEquipSlots(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/local/dnf-gm/equip-slots/unlock-all", map[string]any{
		"character_id": characterID,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) unlockDungeonDifficulties(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/dungeons/unlock-difficulties", map[string]any{})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) unlockHell(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/dungeons/unlock-hell", map[string]any{})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) dungeonAccess(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/dungeons")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) characterDetail(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/detail")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) raidAdmissionCounters(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/raid-counters")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) resetRaidAdmissionCounter(ctx context.Context, characterID, raid, counter string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/raid-counters/reset", map[string]any{
		"raid":    raid,
		"counter": counter,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) equipmentSlots(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/equipment-slots")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) inventory(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/inventory")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) mailbox(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/mailbox")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) quests(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/quests")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) questsSearch(ctx context.Context, characterID, keyword string, limit int) (json.RawMessage, error) {
	values := url.Values{}
	values.Set("q", keyword)
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	reply, err := c.get(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/quests/search?"+values.Encode())
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) questDetail(ctx context.Context, characterID, questID string) (json.RawMessage, error) {
	reply, err := c.get(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/quests/"+url.PathEscape(questID)+"/detail")
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) questAction(ctx context.Context, characterID, questID, action string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/quests/"+url.PathEscape(questID)+"/"+url.PathEscape(action), map[string]any{})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) questBatch(ctx context.Context, characterID string, questIDs []int64) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/quests/complete-batch", map[string]any{
		"quest_ids": questIDs,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) setGold(ctx context.Context, characterID string, value int64) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/gold", map[string]any{
		"value": value,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) setFatigue(ctx context.Context, characterID, mode string, value int64) (json.RawMessage, error) {
	body := map[string]any{
		"character_id": characterID,
		"mode":         mode,
	}
	if mode == "set" {
		body["value"] = value
	}
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/fatigue", body)
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) deleteCharacter(ctx context.Context, characterID, confirmName string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/delete", map[string]any{
		"confirm_name": confirmName,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) inventoryDelete(ctx context.Context, characterID string, body map[string]any) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/inventory/delete", body)
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) inventoryClear(ctx context.Context, characterID, container string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/inventory/clear", map[string]any{
		"container": container,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) mailboxClear(ctx context.Context, characterID string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/local/dnf-gm/mail/clear", map[string]any{
		"character_id": characterID,
		"confirm":      true,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) mailboxDelete(ctx context.Context, characterID string, mailIDs []string, force bool) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/mailbox/delete", map[string]any{
		"mail_ids": mailIDs,
		"force":    force,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) equipUnequip(ctx context.Context, characterID, key string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/equipment/unequip", map[string]any{"key": key})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) equipDelete(ctx context.Context, characterID, key string) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/equipment/delete", map[string]any{"key": key})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) setAccountCubes(ctx context.Context, characterID string, selection int, count int64) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/account-cubes", map[string]any{
		"selection": selection,
		"count":     count,
	})
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) sendMail(ctx context.Context, body map[string]any) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/local/dnf-gm/mail", body)
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) sendRichMail(ctx context.Context, characterID string, body map[string]any) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/dnf/gm/characters/"+url.PathEscape(characterID)+"/mail", body)
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) walletQuery(ctx context.Context, body map[string]any) (json.RawMessage, error) {
	reply, err := c.post(ctx, "/local/dnf-gm/wallet/query", body)
	if err != nil {
		return nil, err
	}
	return reply.Data, nil
}

func (c *gmClient) walletUpdate(ctx context.Context, body map[string]any) (*gmReply, error) {
	return c.post(ctx, "/local/dnf-gm/wallet/update", body)
}

func (c *gmClient) kickSessions(ctx context.Context, banType, banValue string) (int, error) {
	reply, err := c.post(ctx, "/local/dnf-gm/kick-sessions", map[string]any{
		"ban_type":  banType,
		"ban_value": banValue,
	})
	if err != nil {
		return 0, err
	}
	return reply.Kicked, nil
}

func (c *gmClient) characterMailOptions(ctx context.Context, itemID string, values url.Values) (json.RawMessage, error) {
	return c.itemMailOptions(ctx, itemID, values)
}
