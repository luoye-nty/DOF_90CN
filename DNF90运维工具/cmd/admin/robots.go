package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

type robotProvisionBatchRequest struct {
	RobotType       string `json:"robot_type"`
	Count           int    `json:"count"`
	StartIndex      int    `json:"start_index"`
	AccountPrefix   string `json:"account_prefix"`
	NamePrefix      string `json:"name_prefix"`
	ProfilePrefix   string `json:"profile_prefix"`
	ChannelID       int    `json:"channel_id"`
	AutoStart       bool   `json:"auto_start"`
	Job             int    `json:"job"`
	GrowType        int    `json:"grow_type"`
	Level           int    `json:"level"`
	Village         int    `json:"village"`
	Area            int    `json:"area"`
	X               int    `json:"x"`
	Y               int    `json:"y"`
	ExpertStoreKind string `json:"expert_store_kind,omitempty"`
	ExpertStoreCost uint32 `json:"expert_store_cost,omitempty"`
}

type robotIDRequest struct {
	ID string `json:"id"`
}

type robotRelocateRequest struct {
	ID        string `json:"id"`
	ChannelID int    `json:"channel_id"`
	Village   int    `json:"village"`
	Area      int    `json:"area"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	MoveType  int    `json:"move_type"`
	AutoStart *bool  `json:"auto_start,omitempty"`
}

type robotBatchStartStopRequest struct {
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
}

func (s *webServer) handleRobots(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gm.robots(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, "读取机器人列表失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotReference(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gm.robotReference(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, "读取机器人频道和地图目录失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotStatus(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gm.robotStatus(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, "读取机器人状态失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotOperations(w http.ResponseWriter, r *http.Request) {
	raw, err := s.gm.robotOperations(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, "读取机器人操作记录失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotAutomation(w http.ResponseWriter, r *http.Request) {
	var (
		raw json.RawMessage
		err error
	)
	if r.Method == http.MethodPost {
		var body map[string]any
		if decodeErr := decodeBody(r, &body); decodeErr != nil {
			fail(w, http.StatusBadRequest, "请求格式错误")
			return
		}
		raw, err = s.gm.updateRobotAutomation(r.Context(), body)
	} else {
		raw, err = s.gm.robotAutomation(r.Context())
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "操作机器人自动策略失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotSpeech(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Modes []struct {
			ID        string `json:"id"`
			Megaphone bool   `json:"megaphone"`
		} `json:"modes"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(body.Modes) == 0 || len(body.Modes) > 1024 {
		fail(w, http.StatusBadRequest, "机器人喊话模式数量不正确")
		return
	}
	for index := range body.Modes {
		body.Modes[index].ID = strings.TrimSpace(body.Modes[index].ID)
		if body.Modes[index].ID == "" {
			fail(w, http.StatusBadRequest, "机器人 ID 不能为空")
			return
		}
	}
	raw, err := s.gm.updateRobotSpeech(r.Context(), body)
	if err != nil {
		fail(w, http.StatusBadGateway, "保存机器人喊话模式失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotProfile(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	id, _ := body["id"].(string)
	if strings.TrimSpace(id) == "" {
		fail(w, http.StatusBadRequest, "机器人 ID 不能为空")
		return
	}
	raw, err := s.gm.updateRobotProfile(r.Context(), body)
	if err != nil {
		fail(w, http.StatusBadGateway, "保存机器人配置失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotRecycle(w http.ResponseWriter, r *http.Request) {
	var (
		raw json.RawMessage
		err error
	)
	if r.Method == http.MethodPost {
		var body map[string]any
		if decodeErr := decodeBody(r, &body); decodeErr != nil {
			fail(w, http.StatusBadRequest, "请求格式错误")
			return
		}
		raw, err = s.gm.updateRobotRecycle(r.Context(), body)
	} else {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			fail(w, http.StatusBadRequest, "机器人 ID 不能为空")
			return
		}
		raw, err = s.gm.robotRecycle(r.Context(), id)
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "操作机器人回收设置失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotProvisionBatch(w http.ResponseWriter, r *http.Request) {
	var body robotProvisionBatchRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.AccountPrefix = strings.TrimSpace(body.AccountPrefix)
	body.NamePrefix = strings.TrimSpace(body.NamePrefix)
	body.ProfilePrefix = strings.TrimSpace(body.ProfilePrefix)
	body.RobotType = strings.ToLower(strings.TrimSpace(body.RobotType))
	if body.RobotType == "" {
		body.RobotType = "normal"
	}
	body.ExpertStoreKind = strings.ToLower(strings.TrimSpace(body.ExpertStoreKind))
	if body.Count < 1 || body.Count > 64 {
		fail(w, http.StatusBadRequest, "每次创建数量必须在 1~64 之间")
		return
	}
	if body.StartIndex <= 0 {
		body.StartIndex = 1
	}
	if body.AccountPrefix == "" || body.NamePrefix == "" || body.ProfilePrefix == "" {
		fail(w, http.StatusBadRequest, "账号前缀、角色名前缀和机器人前缀不能为空")
		return
	}
	if body.ChannelID <= 0 || body.ChannelID > 65535 {
		fail(w, http.StatusBadRequest, "频道 ID 不正确")
		return
	}
	if body.Job < 0 || body.Job > 15 || body.GrowType < 0 || body.Level < 1 || body.Level > 90 ||
		body.Village < 0 || body.Village > 255 || body.Area < 0 || body.Area > 255 ||
		body.X < 0 || body.X > 65535 || body.Y < 0 || body.Y > 65535 {
		fail(w, http.StatusBadRequest, "职业、等级或位置参数不正确")
		return
	}
	if body.RobotType != "professional" && body.RobotType != "personal_store" && body.RobotType != "normal" {
		fail(w, http.StatusBadRequest, "机器人类型不正确")
		return
	}
	if body.RobotType == "professional" {
		if body.ExpertStoreKind != "disjoint" && body.ExpertStoreKind != "enchant" {
			fail(w, http.StatusBadRequest, "副职业机器人必须选择分解机或附魔机")
			return
		}
	} else if body.ExpertStoreKind != "" {
		fail(w, http.StatusBadRequest, "只有副职业机器人可以设置专家摊类型")
		return
	}
	raw, err := s.gm.provisionRobots(r.Context(), body)
	if err != nil {
		fail(w, http.StatusBadGateway, "批量创建机器人失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotStart(w http.ResponseWriter, r *http.Request) {
	s.handleRobotIDAction(w, r, true)
}

func (s *webServer) handleRobotStop(w http.ResponseWriter, r *http.Request) {
	s.handleRobotIDAction(w, r, false)
}

func (s *webServer) handleRobotRelocate(w http.ResponseWriter, r *http.Request) {
	var body robotRelocateRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if body.ID == "" {
		fail(w, http.StatusBadRequest, "机器人 ID 不能为空")
		return
	}
	if body.ChannelID <= 0 || body.ChannelID > 65535 ||
		body.Village < 0 || body.Village > 255 || body.Area < 0 || body.Area > 255 ||
		body.X < 0 || body.X > 65535 || body.Y < 0 || body.Y > 65535 ||
		body.MoveType < 0 || body.MoveType > 255 {
		fail(w, http.StatusBadRequest, "频道、城镇、区域或坐标参数不正确")
		return
	}
	raw, err := s.gm.relocateRobot(r.Context(), body)
	if err != nil {
		fail(w, http.StatusBadGateway, "修改机器人位置失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotIDAction(w http.ResponseWriter, r *http.Request, start bool) {
	var body robotIDRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if body.ID == "" {
		fail(w, http.StatusBadRequest, "机器人 ID 不能为空")
		return
	}
	var (
		raw json.RawMessage
		err error
	)
	if start {
		raw, err = s.gm.startRobot(r.Context(), body.ID)
	} else {
		raw, err = s.gm.stopRobot(r.Context(), body.ID)
	}
	if err != nil {
		action := "下线"
		if start {
			action = "上线"
		}
		fail(w, http.StatusBadGateway, "机器人"+action+"失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotBatchStartStop(w http.ResponseWriter, r *http.Request) {
	var body robotBatchStartStopRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.Action = strings.ToLower(strings.TrimSpace(body.Action))
	if body.Action != "start" && body.Action != "stop" {
		fail(w, http.StatusBadRequest, "批量动作只支持 start 或 stop")
		return
	}
	if len(body.IDs) == 0 {
		fail(w, http.StatusBadRequest, "请至少选择一个机器人")
		return
	}
	if len(body.IDs) > 500 {
		fail(w, http.StatusBadRequest, "单次最多操作 500 个机器人")
		return
	}
	for index := range body.IDs {
		body.IDs[index] = strings.TrimSpace(body.IDs[index])
		if body.IDs[index] == "" {
			fail(w, http.StatusBadRequest, "机器人 ID 不能为空")
			return
		}
	}
	raw, err := s.gm.robotBatch(r.Context(), body)
	if err != nil {
		fail(w, http.StatusBadGateway, "批量操作机器人失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

type robotExpertStoreRequest struct {
	ID   string `json:"id"`
	Cost uint32 `json:"cost"`
}

type robotPrivateStoreItem struct {
	ItemID uint32 `json:"item_id"`
	Count  uint32 `json:"count"`
	Price  uint32 `json:"price"`
}

type robotPrivateStoreRequest struct {
	ID    string                  `json:"id"`
	Title string                  `json:"title"`
	Items []robotPrivateStoreItem `json:"items"`
}

func (s *webServer) handleRobotExpertStoreOpen(w http.ResponseWriter, r *http.Request) {
	var body robotExpertStoreRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if body.ID == "" {
		fail(w, http.StatusBadRequest, "机器人 ID 不能为空")
		return
	}
	raw, err := s.gm.openRobotExpertStore(r.Context(), body.ID, body.Cost)
	if err != nil {
		fail(w, http.StatusBadGateway, "打开专家摊失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotExpertStoreClose(w http.ResponseWriter, r *http.Request) {
	var body robotIDRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if body.ID == "" {
		fail(w, http.StatusBadRequest, "机器人 ID 不能为空")
		return
	}
	raw, err := s.gm.closeRobotExpertStore(r.Context(), body.ID)
	if err != nil {
		fail(w, http.StatusBadGateway, "关闭专家摊失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotExpertStoreCost(w http.ResponseWriter, r *http.Request) {
	var body robotExpertStoreRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if body.ID == "" {
		fail(w, http.StatusBadRequest, "机器人 ID 不能为空")
		return
	}
	raw, err := s.gm.setRobotExpertStoreCost(r.Context(), body.ID, body.Cost)
	if err != nil {
		fail(w, http.StatusBadGateway, "修改专家摊费用失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) decodeRobotPrivateStoreRequest(w http.ResponseWriter, r *http.Request) (robotPrivateStoreRequest, bool) {
	var body robotPrivateStoreRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return body, false
	}
	body.ID = strings.TrimSpace(body.ID)
	body.Title = strings.TrimSpace(body.Title)
	if body.ID == "" || body.Title == "" || len(body.Items) == 0 || len(body.Items) > 14 {
		fail(w, http.StatusBadRequest, "机器人、商店标题或商品行数不正确")
		return body, false
	}
	seen := make(map[uint32]struct{}, len(body.Items))
	for _, item := range body.Items {
		if item.ItemID == 0 || item.Count == 0 || item.Price == 0 {
			fail(w, http.StatusBadRequest, "商品 ID、数量和单价必须大于 0")
			return body, false
		}
		if _, duplicate := seen[item.ItemID]; duplicate {
			fail(w, http.StatusBadRequest, "同一个商品不能重复配置")
			return body, false
		}
		seen[item.ItemID] = struct{}{}
	}
	return body, true
}

func (s *webServer) handleRobotPrivateStoreConfig(w http.ResponseWriter, r *http.Request) {
	body, ok := s.decodeRobotPrivateStoreRequest(w, r)
	if !ok {
		return
	}
	raw, err := s.gm.configureRobotPrivateStore(r.Context(), body)
	if err != nil {
		fail(w, http.StatusBadGateway, "保存个人商店配置失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotPrivateStoreOpen(w http.ResponseWriter, r *http.Request) {
	body, ok := s.decodeRobotPrivateStoreRequest(w, r)
	if !ok {
		return
	}
	raw, err := s.gm.openRobotPrivateStore(r.Context(), body)
	if err != nil {
		fail(w, http.StatusBadGateway, "开启个人商店失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func (s *webServer) handleRobotPrivateStoreClose(w http.ResponseWriter, r *http.Request) {
	var body robotIDRequest
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	body.ID = strings.TrimSpace(body.ID)
	if body.ID == "" {
		fail(w, http.StatusBadRequest, "机器人 ID 不能为空")
		return
	}
	raw, err := s.gm.closeRobotPrivateStore(r.Context(), body.ID)
	if err != nil {
		fail(w, http.StatusBadGateway, "关闭个人商店失败："+err.Error())
		return
	}
	writeRobotData(w, raw)
}

func writeRobotData(w http.ResponseWriter, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": nil})
		return
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		fail(w, http.StatusBadGateway, "机器人接口返回格式错误："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}
