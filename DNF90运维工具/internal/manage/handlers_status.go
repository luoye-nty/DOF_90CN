//go:build paid

// GET /manage/v1/status：汇总服务端运行状态、认证服务、MySQL、在线人数、
// CDK 汇总、授权状态、配对码设置状态与管理服务自身信息。
package manage

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"dnf90ops/internal/config"
	"dnf90ops/internal/store"
)

// 授权档位与剩余天数的哨兵值（与 cmd/authserver/license_tier.go 的 kind 取值对齐）。
const (
	kindForever   = "forever" // 永久卡
	remainForever = -1        // 剩余天数：长期有效
	remainUnknown = -2        // 剩余天数：尚未用新版校验过，未知
)

// buildLicenseStatus 组装授权状态。
//
// 剩余天数由到期时间（绝对值）现算，避免"客户久未开工具 → 快照天数失真"；
// 只在从未用新版校验过时回退到配置里的快照值。
func buildLicenseStatus(cfg *config.Config) licenseStatus {
	lic := licenseStatus{Edition: "paid", RemainDays: remainUnknown}
	if cfg == nil {
		return lic
	}
	lc := cfg.License
	lic.CDKSet = strings.TrimSpace(lc.CDK) != ""
	lic.Kind, lic.ExpireAt, lic.CheckedAt = lc.Kind, lc.ExpireAt, lc.CheckedAt
	switch {
	case lc.Kind == kindForever:
		lic.RemainDays = remainForever
	case strings.TrimSpace(lc.ExpireAt) != "":
		if exp, err := time.Parse(time.RFC3339, lc.ExpireAt); err == nil {
			d := time.Until(exp)
			switch {
			case d <= 0:
				lic.RemainDays = 0
			default:
				const day = 24 * time.Hour
				lic.RemainDays = int((d + day - 1) / day)
			}
		} else {
			lic.RemainDays = lc.RemainDays
		}
	case lc.RemainDays != 0:
		lic.RemainDays = lc.RemainDays
	}
	return lic
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	resp := statusResponse{
		Listen: s.listen,
		Cert: certStatus{
			CertPath: s.certPath,
			KeyPath:  s.keyPath,
		},
		License: buildLicenseStatus(s.cfg),
	}
	if _, err := os.Stat(s.certPath); err == nil {
		resp.Cert.Exists = true
	}
	resp.Pairing.Enabled = s.auth.Enabled()

	// 服务端目录/运行状态（复用 serverdir.Inspect，由 Bridge 注入）。
	if s.bridge != nil && s.bridge.Inspect != nil && s.bridge.ServerRoot != nil {
		root := strings.TrimSpace(s.bridge.ServerRoot())
		if root != "" {
			if ins := s.bridge.Inspect(root); ins != nil {
				resp.Server = serverStatus{
					Root:          ins.Root,
					RootExists:    ins.RootExists,
					ServerExe:     ins.ServerExeExists,
					Instance:      ins.InstanceExists,
					InstanceValid: ins.InstanceValid,
					DatabaseMode:  ins.DatabaseMode,
					ChannelPort:   ins.ChannelPort,
					AdminPort:     ins.AdminPort,
					Running:       ins.Running,
					Ready:         ins.Ready,
					NeedsInit:     ins.NeedsInit,
					Message:       ins.Message,
				}
				resp.MySQL.Mode = ins.DatabaseMode
			}
		}
	}

	// 认证服务状态。
	if s.bridge != nil && s.bridge.AuthRunning != nil {
		resp.Auth.Running = s.bridge.AuthRunning()
	}
	if s.cfg != nil {
		resp.Auth.PublicAddr = s.cfg.Public.ListenAddr
	}
	if s.cfg != nil {
		resp.Auth.InternalURL = s.cfg.Internal.ListenAddr
	}

	// MySQL 配置与存活性（连接池由认证服务持有；认证运行即认为可用）。
	if s.cfg != nil {
		host := s.cfg.MySQL.Host
		if host == "" {
			host = "127.0.0.1"
		}
		port := s.cfg.MySQL.Port
		if port <= 0 {
			port = 13306
		}
		resp.MySQL.Configured = host + ":" + strconv.Itoa(port) + "/" + s.cfg.MySQL.DBName
	}
	st := s.bridgeStore()
	resp.MySQL.Alive = st != nil
	if resp.MySQL.Mode == "" {
		resp.MySQL.Mode = "unknown"
	}

	// 在线人数。
	if st != nil && s.bridge != nil && s.bridge.OnlineCount != nil {
		resp.Online = s.bridge.OnlineCount()
	}

	// CDK 汇总。
	if st != nil {
		s.fillCDKSummary(ctx, st, &resp.CDK)
	}

	writeOK(w, resp)
}

// bridgeStore 复用主程序认证服务连接池；认证未运行返回 nil。
func (s *Server) bridgeStore() *store.Store {
	if s.bridge == nil || s.bridge.Store == nil {
		return nil
	}
	return s.bridge.Store()
}

// fillCDKSummary 统计 redeem_codes 与 redeem_redemptions（单次快照，避免每行一查）。
func (s *Server) fillCDKSummary(ctx context.Context, st *store.Store, sum *cdkSummary) {
	codes, err := st.ListRedeemCodes(ctx, "", 1, 10000)
	if err == nil {
		sum.Total = len(codes)
		for i := range codes {
			if codes[i].Enabled {
				sum.Enabled++
			} else {
				sum.Disabled++
			}
		}
	}
	reds, err := st.ListRedeemRedemptions(ctx, "", 20000)
	if err == nil {
		sum.Redemptions = len(reds)
		for i := range reds {
			if reds[i].MailStatus == "failed" {
				sum.FailedMails++
			}
		}
	}
}
