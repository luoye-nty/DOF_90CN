//go:build paid

// 注册限制 + 管理员建号端点：
//   - GET/POST /manage/v1/registration-limit：查询/应用「关闭玩家自助注册」配置（持久化 config.json）；
//   - POST /manage/v1/account/register：管理员建号（绕过注册限制，mode 固定 open，自动按注册赠送规则登记点券）；
//   - POST /manage/v1/account/password：管理员改密（改密后强制该账号全部会话下线）；
//   - POST /manage/v1/account/force-logout：强制指定账号下线。
package manage

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"dnf90ops/internal/store"
)

// registrationLimitApplyRequest POST /manage/v1/registration-limit 入参。
// MaxPerIP 用指针以区分「没传这个字段」与「显式传 0（0 现在是"不允许注册"）」：
// 老版本管理工具只会发 closed，此时必须保留服务端已有的 max_per_ip，不能被默认 0 冲掉。
// CodeRequired 同样用指针：老版本管理工具不发这个字段时，必须保留服务端已有的开关，
// 不能被零值 false 顺手关掉（否则运营刚打开"注册要注册码"，另一次保存又把它关了）。
type registrationLimitApplyRequest struct {
	Closed       bool  `json:"closed"`
	MaxPerIP     *int  `json:"max_per_ip,omitempty"`
	CodeRequired *bool `json:"code_required,omitempty"`
}

// registrationCodeCreateRequest POST /manage/v1/registration-codes 入参。
type registrationCodeCreateRequest struct {
	Count int `json:"count"`
}

// registrationCodeRevokeRequest POST /manage/v1/registration-codes/revoke 入参。
type registrationCodeRevokeRequest struct {
	Codes []string `json:"codes"`
}

// accountRegisterRequest POST /manage/v1/account/register 入参。
type accountRegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Email 老字段：管理工具早期版本用它填「邮箱」。现在前端改成填救援代码，
	// 这里保留字段是为了老版本管理工具不发 rescue_code 时也能建号（值仍写 email 列）。
	Email string `json:"email"`
	// RescueCode 6 位数字救援代码（找回密码用）；留空表示不设置。
	RescueCode string `json:"rescue_code"`
}

// accountPasswordRequest POST /manage/v1/account/password 入参。
type accountPasswordRequest struct {
	Username    string `json:"username"`
	NewPassword string `json:"new_password"`
}

// accountForceLogoutRequest POST /manage/v1/account/force-logout 入参。
type accountForceLogoutRequest struct {
	Username string `json:"username"`
}

// handleRegistrationLimitGet 返回当前注册限制配置快照。
func (s *Server) handleRegistrationLimitGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.RegistrationClosedStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, "registration-limit-status-unavailable")
		return
	}
	writeOK(w, s.bridge.RegistrationClosedStatus())
}

// handleRegistrationLimitApply 应用并持久化注册限制配置。
func (s *Server) handleRegistrationLimitApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.RegistrationClosedApply == nil {
		writeErr(w, http.StatusServiceUnavailable, "registration-limit-apply-unavailable")
		return
	}
	var req registrationLimitApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	// 未携带 max_per_ip 时沿用当前值（兼容只管理 closed 的老客户端）。
	// 缺省用 -1（不限制）：新语义下 0 是"不允许注册"，拿它当零值会误关注册。
	maxPerIP := -1
	codeRequired := false
	if s.bridge.RegistrationClosedStatus != nil {
		current := s.bridge.RegistrationClosedStatus()
		maxPerIP = current.MaxPerIP
		codeRequired = current.CodeRequired
	}
	if req.MaxPerIP != nil {
		maxPerIP = *req.MaxPerIP
	}
	if req.CodeRequired != nil {
		codeRequired = *req.CodeRequired
	}
	status, err := s.bridge.RegistrationClosedApply(r.Context(), req.Closed, maxPerIP, codeRequired)
	if err != nil {
		// 越界等用户可修正的错误按 400 返回，其余按 500。
		if strings.Contains(err.Error(), "每 IP 注册上限") {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "apply-failed: "+err.Error())
		return
	}
	writeOK(w, status)
}

// ---------- 注册码（邀请码）：生成 / 列表 / 作废 ----------

// handleRegistrationCodeCreate POST /manage/v1/registration-codes {count}
// 批量生成注册码（8 位短码，形如 ABCD-EFGH，一次性：用过即失效）。
func (s *Server) handleRegistrationCodeCreate(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.RegistrationCodeCreate == nil {
		writeErr(w, http.StatusServiceUnavailable, "registration-code-unavailable")
		return
	}
	var req registrationCodeCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	if req.Count <= 0 || req.Count > 200 {
		writeErr(w, http.StatusBadRequest, "生成数量必须在 1~200 之间")
		return
	}
	codes, err := s.bridge.RegistrationCodeCreate(r.Context(), req.Count)
	if err != nil {
		s.log.Error("create registration codes failed", "count", req.Count, "error", err)
		writeErr(w, http.StatusInternalServerError, "生成注册码失败: "+err.Error())
		return
	}
	s.log.Info("registration codes created", "count", len(codes))
	writeOK(w, map[string]any{"codes": codes, "count": len(codes)})
}

// handleRegistrationCodeList GET /manage/v1/registration-codes?state=&limit=
// state 留空=全部；unused=未使用；used=已使用；revoked=已作废。
func (s *Server) handleRegistrationCodeList(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.RegistrationCodeList == nil {
		writeErr(w, http.StatusServiceUnavailable, "registration-code-unavailable")
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	codes, err := s.bridge.RegistrationCodeList(r.Context(), state, limit)
	if err != nil {
		s.log.Error("list registration codes failed", "state", state, "error", err)
		writeErr(w, http.StatusInternalServerError, "查询注册码失败: "+err.Error())
		return
	}
	if codes == nil {
		codes = []RegistrationCode{}
	}
	unused := 0
	for _, c := range codes {
		if strings.EqualFold(c.State, "unused") {
			unused++
		}
	}
	writeOK(w, map[string]any{"items": codes, "total": len(codes), "unused": unused})
}

// handleRegistrationCodeRevoke POST /manage/v1/registration-codes/revoke {codes:[...]}
// 作废未使用的注册码（已使用的码不动：那枚码对应的账号是真注册出来了）。
func (s *Server) handleRegistrationCodeRevoke(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.RegistrationCodeRevoke == nil {
		writeErr(w, http.StatusServiceUnavailable, "registration-code-unavailable")
		return
	}
	var req registrationCodeRevokeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	if len(req.Codes) == 0 {
		writeErr(w, http.StatusBadRequest, "请先选择要作废的注册码")
		return
	}
	if len(req.Codes) > 200 {
		writeErr(w, http.StatusBadRequest, "一次最多作废 200 枚注册码")
		return
	}
	revoked, err := s.bridge.RegistrationCodeRevoke(r.Context(), req.Codes)
	if err != nil {
		s.log.Error("revoke registration codes failed", "count", len(req.Codes), "error", err)
		writeErr(w, http.StatusInternalServerError, "作废注册码失败: "+err.Error())
		return
	}
	s.log.Info("registration codes revoked", "requested", len(req.Codes), "revoked", revoked)
	writeOK(w, map[string]any{"revoked": revoked, "requested": len(req.Codes)})
}

// handleAccountRegister 管理员建号。
func (s *Server) handleAccountRegister(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.AdminRegister == nil {
		writeErr(w, http.StatusServiceUnavailable, "account-register-unavailable")
		return
	}
	var req accountRegisterRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 请填写账号名")
		return
	}
	if req.Password == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 请填写密码")
		return
	}
	rescueCode := strings.TrimSpace(req.RescueCode)
	if rescueCode != "" && !store.ValidRescueCode(rescueCode) {
		writeErr(w, http.StatusBadRequest, "bad-request: 救援代码必须是 6 位数字（留空表示不设置）")
		return
	}
	accountID, err := s.bridge.AdminRegister(r.Context(), strings.TrimSpace(req.Username), req.Password, strings.TrimSpace(req.Email), rescueCode)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrUsernameTaken):
			writeErr(w, http.StatusConflict, "该账号已注册，请直接登录或改换账号名")
		case strings.Contains(err.Error(), "password-too-short"):
			writeErr(w, http.StatusBadRequest, "密码至少 8 位")
		case errors.Is(err, store.ErrInvalidRescueCode):
			writeErr(w, http.StatusBadRequest, "救援代码必须是 6 位数字（留空表示不设置）")
		case strings.Contains(err.Error(), "认证服务未运行"):
			writeErr(w, http.StatusServiceUnavailable, "认证服务未运行，无法建号")
		default:
			s.log.Error("admin register failed", "username", req.Username, "error", err)
			writeErr(w, http.StatusInternalServerError, "建号失败: "+err.Error())
		}
		return
	}
	s.log.Info("admin created account", "account_id", accountID)
	writeOK(w, map[string]string{"account_id": accountID, "message": "账号创建成功"})
}

// handleAccountPassword 管理员改密。
func (s *Server) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.AdminChangePassword == nil {
		writeErr(w, http.StatusServiceUnavailable, "account-password-unavailable")
		return
	}
	var req accountPasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 请填写账号名")
		return
	}
	if len(req.NewPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "新密码至少 8 位")
		return
	}
	if err := s.bridge.AdminChangePassword(r.Context(), strings.TrimSpace(req.Username), req.NewPassword); err != nil {
		switch {
		case errors.Is(err, store.ErrUserNotFound):
			writeErr(w, http.StatusNotFound, "账号不存在")
		case strings.Contains(err.Error(), "password-too-short"):
			writeErr(w, http.StatusBadRequest, "新密码至少 8 位")
		default:
			s.log.Error("admin change password failed", "username", req.Username, "error", err)
			writeErr(w, http.StatusInternalServerError, "改密失败: "+err.Error())
		}
		return
	}
	s.log.Info("admin changed password", "username", req.Username)
	writeOK(w, map[string]string{"message": "密码已修改，该账号旧会话已全部下线"})
}

// handleAccountForceLogout 强制指定账号下线。
func (s *Server) handleAccountForceLogout(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.AdminForceLogout == nil {
		writeErr(w, http.StatusServiceUnavailable, "account-force-logout-unavailable")
		return
	}
	var req accountForceLogoutRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 请填写账号名")
		return
	}
	kicked, err := s.bridge.AdminForceLogout(r.Context(), username)
	if err != nil {
		s.log.Error("admin force logout failed", "username", username, "error", err)
		writeErr(w, http.StatusInternalServerError, "强制下线失败: "+err.Error())
		return
	}
	s.log.Info("admin force logout", "username", username, "kicked", kicked)
	writeOK(w, map[string]any{"message": "已强制下线", "kicked_sessions": kicked})
}
