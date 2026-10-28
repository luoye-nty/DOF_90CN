//go:build paid

// 频道管理端点（/manage/v1/channels*）。
//
// 语义：开启/关闭频道 = 在服务端 channel_info.etc 的 [server] 段里增删该频道行。
// 为什么不是调服务端接口：go-server 的频道目录与下发给客户端的频道表都在 Service.Start
// 时从该文件一次性构建并缓存（internal/services/dnfbridge/service.go loadChannelAssets），
// 运行期没有 setter。改文件后必须重启 DNF90Server 才生效——因此响应里固定带
// restart_required=true，管理端 UI 必须把这句话显示出来，不能让人以为点完就热更了。
//
// 具体解析/改写/保护规则全在 internal/channelstore，本文件只做协议编排与鉴权。
package manage

import (
	"net/http"
	"strconv"
	"strings"
)

// channelApplyResponse POST /manage/v1/channels/apply 响应体。
type channelApplyResponse struct {
	Changed []int         `json:"changed"`
	Status  ChannelStatus `json:"status"`
	Message string        `json:"message"`
	// HotApplied 本次改动是否已经在运行中的服务端上热生效（无需重启）。
	HotApplied bool `json:"hot_applied"`
	// HotApplyError 热生效失败原因；非空时 HotApplied=false，但改动**已经落盘**，
	// 重启 DNF90Server 后照样生效。
	HotApplyError string `json:"hot_apply_error,omitempty"`
	// Reload 热生效明细（成功时才有）。
	Reload *ChannelReloadStatus `json:"reload,omitempty"`
	// RestartRequired 仅当热生效失败时为 true：需要重启服务端才能让改动生效。
	RestartRequired bool `json:"restart_required"`
}

// GET /manage/v1/channels
// 返回当前 channel_info.etc 的频道目录与启用状态（含已关闭但可恢复的频道）。
func (s *Server) handleChannelsGet(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: timeNow(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	if s.bridge == nil || s.bridge.ChannelsStatus == nil {
		writeErr(w, http.StatusBadGateway, "gm-not-available: 运维端未注入频道管理能力，请升级运维工具")
		return
	}
	status, err := s.bridge.ChannelsStatus(r.Context())
	if err != nil {
		audit.fail(err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	audit.fields["total"] = status.Total
	audit.fields["enabled"] = status.Enabled
	audit.fields["disabled"] = status.Disabled
	audit.ok()
	writeOK(w, status)
}

// POST /manage/v1/channels/apply {channel_ids:[...], enabled:bool}
// 批量开启或关闭频道；只增删 [server] 段里的频道行，写入前自动备份，
// 关闭保护频道（引导 19 / 默认城镇 11 / 最后一个普通城镇频道）时直接拒绝且不落盘。
func (s *Server) handleChannelsApply(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: timeNow(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	if s.bridge == nil || s.bridge.ChannelsApply == nil {
		writeErr(w, http.StatusBadGateway, "gm-not-available: 运维端未注入频道管理能力，请升级运维工具")
		return
	}
	var req struct {
		ChannelIDs []int `json:"channel_ids"`
		Enabled    bool  `json:"enabled"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	ids := make([]int, 0, len(req.ChannelIDs))
	seen := make(map[int]struct{}, len(req.ChannelIDs))
	for _, id := range req.ChannelIDs {
		if id <= 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, "bad-request: channel_ids 不能为空")
		return
	}
	if len(ids) > 400 {
		writeErr(w, http.StatusBadRequest, "bad-request: channel_ids 一次最多 400 个")
		return
	}
	audit.fields["channel_ids"] = ids
	audit.fields["enabled"] = req.Enabled

	changed, status, err := s.bridge.ChannelsApply(r.Context(), ids, req.Enabled)
	if err != nil {
		audit.fail(err)
		// 保护规则不满足属于请求语义错误（400），其余按 502 上抛。
		status := http.StatusBadGateway
		if isChannelPolicyError(err) {
			status = http.StatusBadRequest
		}
		writeErr(w, status, err.Error())
		return
	}
	audit.fields["changed"] = changed
	audit.ok()

	// 落盘成功后立刻请求服务端热生效，免得使用者还要记得去重启。
	// 热生效失败不算操作失败：文件已经改好了，服务端下次启动会用新表；
	// 这里把失败原因如实带回给管理端，由它提示"需要重启"。
	var reload *ChannelReloadStatus
	hotErrText := ""
	if len(changed) > 0 && s.bridge.ChannelsReload != nil {
		reloaded, reloadErr := s.bridge.ChannelsReload(r.Context())
		if reloadErr != nil {
			hotErrText = reloadErr.Error()
			audit.fields["hot_apply_error"] = hotErrText
		} else {
			reload = &reloaded
			audit.fields["hot_applied"] = true
			audit.fields["game_ports_opened"] = reloaded.GamePortsOpened
			audit.fields["game_ports_closed"] = reloaded.GamePortsClosed
		}
	} else if len(changed) == 0 {
		hotErrText = ""
	} else {
		hotErrText = "运维端未注入频道热重载能力，请升级运维工具"
	}

	message := "没有需要改动的频道（这些频道已经是目标状态）"
	switch {
	case len(changed) == 0:
	case hotErrText == "":
		if req.Enabled {
			message = "已开启 " + itoaList(changed) + " 号频道并热生效，新登录/重开的玩家立即可见"
		} else {
			message = "已关闭 " + itoaList(changed) + " 号频道并热生效，新登录/重开的玩家不再看到"
		}
	case req.Enabled:
		message = "已开启 " + itoaList(changed) + " 号频道（已落盘），但热生效失败：" + hotErrText
	default:
		message = "已关闭 " + itoaList(changed) + " 号频道（已落盘），但热生效失败：" + hotErrText
	}

	writeOK(w, channelApplyResponse{
		Changed:         changed,
		Status:          status,
		Message:         message,
		HotApplied:      reload != nil,
		HotApplyError:   hotErrText,
		Reload:          reload,
		RestartRequired: len(changed) > 0 && reload == nil,
	})
}

// isChannelPolicyError 判断是否属于"保护规则拒绝"（用户可修正）而不是环境故障。
func isChannelPolicyError(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	for _, marker := range []string{"不能关闭", "至少要保留", "无法安全开启", "未指定要操作的频道", "没有结束标记"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// itoaList 把频道号列表拼成 "12,13,14" 形态，超过 12 个时省略尾部。
func itoaList(values []int) string {
	parts := make([]string, 0, len(values))
	for index, value := range values {
		if index >= 12 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}
