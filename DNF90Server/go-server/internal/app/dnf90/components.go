// Package dnf90 contains the single composition root for the DNF90 server.
package dnf90

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	appkit "longheng.io/server/internal/platform/app"
	"longheng.io/server/internal/services/dnfbridge"
	"longheng.io/server/internal/services/dnfrobot"
	logicdnf "longheng.io/server/internal/services/logic/dnf"
)

const (
	ServiceName       = "dnfbridge"
	DefaultConfigPath = "configs/dnfbridge.toml"
	DefaultLogicPath  = "configs/dnf/logic.toml"
)

// Components wires storage before the protocol service.
func Components(env *appkit.Env) ([]appkit.Component, error) {
	return components(env, nil)
}

// ComponentsWithShutdown adds a loopback admin endpoint that cancels the
// application context, allowing the Windows launcher to stop cleanly.
func ComponentsWithShutdown(cancel context.CancelFunc) appkit.Builder {
	return func(env *appkit.Env) ([]appkit.Component, error) {
		return components(env, cancel)
	}
}

func components(env *appkit.Env, cancel context.CancelFunc) ([]appkit.Component, error) {
	if cancel != nil && env != nil && env.AdminMux != nil {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("shutdown requested\n"))
			go cancel()
		})
		if env.AdminAuth != nil {
			handler = env.AdminAuth("platform", handler)
		}
		env.AdminMux.HandleFunc("/local/shutdown", handler)
	}

	configPath := strings.TrimSpace(os.Getenv(logicdnf.EnvConfigPath))
	if configPath == "" {
		configPath = DefaultLogicPath
	}
	cfg, err := logicdnf.LoadConfigForEnv(configPath, env)
	if err != nil {
		return nil, err
	}
	runtime, err := logicdnf.NewRuntime(env, cfg)
	if err != nil {
		return nil, err
	}
	robotsEnabled := dnfRobotsEnabled()
	robotSessionSecret := ""
	if robotsEnabled {
		robotSessionSecret = env.Core().Config.Admin.Token
	}
	bridge := dnfbridge.NewWithOptions(env, dnfbridge.ServiceOptions{
		RepositoryProvider:         runtime.Group,
		LocalDebugMode:             localDNFDebugModeEnabled(env),
		LocalDebugStrikeOnSkillUse: localDNFDebugStrikeOnSkillUseEnabled(env),
		DebugKillRoomHotkey:        dnfDebugKillRoomHotkeyEnabled(),
		RobotSessionSecret:         robotSessionSecret,
	})
	if env != nil && env.AdminMux != nil {
		adminListen := strings.TrimSpace(env.Core().Config.Admin.Listen)
		if adminListen != "" {
			if err := bridge.ConfigurePaymentWeb("http://" + adminListen); err != nil {
				return nil, err
			}
			env.AdminMux.Handle(dnfbridge.PaymentWebOrdinaryPath, bridge.PaymentWebHandler())
		}
	}
	registerDNFDebugRoutes(env, bridge)
	registerDNFGMRoutes(env, bridge)
	registerGMAdminRoutes(env, bridge)
	var robotManager *dnfrobot.Manager
	if robotsEnabled {
		robotManager = dnfrobot.NewManager(env.Logger, runtime.Group, bridge, env.Core().Config.Admin.Token)
		registerDNFRobotGMRoutes(env, bridge, robotManager)
	}
	if env != nil && env.AdminMux != nil {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var request struct {
				PID       uint32 `json:"pid"`
				AccountID string `json:"account_id"`
				ClientIP  string `json:"client_ip,omitempty"`
				// LidLease is the launcher's signed statement over
				// (server_id, pid, account_id, expiry). It is optional so an
				// older launcher keeps working; when the server enforces the
				// launcher identity it is the difference between a seat and a
				// refusal.
				LidLease string `json:"lid_lease,omitempty"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			identity := bridge.LauncherIdentityInfo()
			lease := strings.TrimSpace(request.LidLease)
			var err error
			switch {
			case lease != "":
				// A lease names the launcher, so it takes precedence over the
				// legacy shapes: the client_ip form is the SSH-attested remote
				// path, and combining the two would need a proof over both
				// dimensions.
				attested, lidErr := bridge.VerifyLauncherIdentityLease(
					request.PID,
					request.AccountID,
					lease,
				)
				if lidErr != nil {
					if identity.Mode == dnfbridge.LauncherIdentityModeEnforce {
						// 401, never 400. A 400 is exactly what an older server
						// returns for an unknown JSON field, and the launcher
						// answers a 400 by retrying without the proof — which
						// would silently register an unattested PID that the game
						// gate then refuses, so the operator sees an unexplained
						// connect failure instead of "this launcher is not
						// authorized".
						http.Error(w, "launcher identity lease rejected: "+lidErr.Error(),
							http.StatusUnauthorized)
						return
					}
					// Audit: record why, then keep the pre-feature behaviour.
					bridge.LogLauncherIdentityVerdict(request.PID, request.AccountID, lidErr)
				}
				err = bridge.RegisterClientAccountAttested(request.PID, request.AccountID, attested)
			case identity.Mode == dnfbridge.LauncherIdentityModeEnforce:
				// Enforce mode with no proof at all. Registering the account here
				// would only move the refusal to the game socket, where the
				// symptom is an unexplained connect failure.
				lidErr := bridge.RejectBareLauncherIdentityBind(request.PID, request.AccountID)
				http.Error(w, "launcher identity lease required: "+lidErr.Error(),
					http.StatusUnauthorized)
				return
			case strings.TrimSpace(request.ClientIP) != "":
				err = bridge.RegisterRemoteClientAccount(request.PID, request.ClientIP, request.AccountID)
			default:
				err = bridge.RegisterClientAccount(request.PID, request.AccountID)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		if env.AdminAuth != nil {
			handler = env.AdminAuth("platform", handler)
		}
		env.AdminMux.HandleFunc("/local/client-account", handler)

		// The launcher reads this before it signs anything: the server instance
		// ID is what stops a proof from one server run being replayed against
		// the next, and the mode tells the launcher whether a proof is required
		// or merely observed. It is admin-authenticated like every other
		// /local/* route, so only a caller that already holds the admin token
		// can read it.
		infoHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(bridge.LauncherIdentityInfo())
		})
		if env.AdminAuth != nil {
			infoHandler = env.AdminAuth("platform", infoHandler)
		}
		env.AdminMux.HandleFunc("/local/lid-info", infoHandler)
	}
	components := make([]appkit.Component, 0, 3)
	if runtime.Repository != nil {
		components = append(components, runtime.Repository)
	}
	components = append(components, bridge)
	if robotManager != nil {
		components = append(components, robotManager)
	}
	return components, nil
}

func dnfRobotsEnabled() bool {
	value := strings.TrimSpace(os.Getenv("DNFBRIDGE_ROBOT_ENABLED"))
	return strings.EqualFold(value, "true")
}

func localDNFDebugModeEnabled(env *appkit.Env) bool {
	if env == nil || !strings.EqualFold(env.Core().Config.Service.Environment, "local") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("DNFBRIDGE_DEBUG_MODE")), "true")
}

func localDNFDebugStrikeOnSkillUseEnabled(env *appkit.Env) bool {
	return localDNFDebugModeEnabled(env) &&
		strings.EqualFold(strings.TrimSpace(os.Getenv("DNFBRIDGE_DEBUG_STRIKE_ON_SKILL_USE")), "true")
}

// dnfDebugKillRoomHotkeyEnabled exposes the private "KRM1" room-clear request
// used by the standalone LYPlugins test DLL. It is an explicit opt-in and is
// deliberately independent of DNFBRIDGE_DEBUG_MODE: that profile also grants
// the speed and settlement-rank debug behaviour, which testers may not want.
func dnfDebugKillRoomHotkeyEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("DNFBRIDGE_DEBUG_KILL_ROOM")), "true")
}
