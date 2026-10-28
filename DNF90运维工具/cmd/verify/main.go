// DNF90运维工具验收自检命令：
//
//	verify -config config.json
//
// 依次验证 MySQL 连接、幂等建表、注册(邀请码模式需先手工生成邀请码)/登录换票/内部验证/登出。
// 会向账号库写入一条 verify_* 测试账号（注册后保留，便于人工核对）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"dnf90ops/internal/config"
	"dnf90ops/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "config.json 路径")
	username := flag.String("username", "", "测试用户名（默认自动生成 verify_<时间戳>）")
	password := flag.String("password", "VerifyPass123", "测试密码")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fail("加载配置失败: %v", err)
	}

	st, err := store.Open(cfg.MySQL)
	if err != nil {
		fail("MySQL 连接失败: %v (host=%s:%d user=%s db=%s)", err, cfg.MySQL.Host, cfg.MySQL.Port, cfg.MySQL.User, cfg.MySQL.DBName)
	}
	defer st.Close()
	step("MySQL 连接成功", fmt.Sprintf("host=%s:%d db=%s user=%s", cfg.MySQL.Host, cfg.MySQL.Port, cfg.MySQL.DBName, cfg.MySQL.User))

	if err := st.EnsureSchema(ctx); err != nil {
		fail("幂等建表失败: %v", err)
	}
	step("幂等建表成功", "dnf_accounts / launcher_invites / launcher_sessions (CREATE TABLE IF NOT EXISTS)")

	uname := *username
	if uname == "" {
		uname = fmt.Sprintf("verify_%d", time.Now().UnixNano()%10000000000)
	}
	invite := ""
	if cfg.RegistrationMode == "invite" {
		code, _, err := st.EnsureBootstrapInvite(ctx)
		if err != nil {
			fail("invite 模式取邀请码失败: %v", err)
		}
		if code == "" {
			code, err = st.CreateInvite(ctx)
			if err != nil {
				fail("生成邀请码失败: %v", err)
			}
		}
		invite = code
		step("邀请码模式", "bootstrap invite code: "+code)
	}

	// 自检工具建号不受"每 IP 注册上限"约束：传 -1（不限制）。
	// 不能再用 0 —— 0 现在是"不允许注册"，自检建号会被自己的闸拦下。
	accountID, err := st.Register(ctx, uname, *password, "verify@dnf90.local", invite, cfg.RegistrationMode, "127.0.0.1", "", -1)
	if err != nil {
		if err == store.ErrUsernameTaken {
			step("注册（已存在，复用）", "username="+uname)
			acct, e2 := st.GetAccountByUsername(ctx, uname)
			if e2 != nil {
				fail("读取既有账号失败: %v", e2)
			}
			accountID = acct.AccountID
		} else {
			fail("注册失败: %v", err)
		}
	} else {
		step("注册成功", "username="+uname+" account_id="+accountID)
	}

	if cfg.RegistrationMode == "open" {
		step("账号主键约定", "account_id = dnf:<sha256(username)> = "+accountID)
	}

	if _, _, _, err := st.Login(ctx, uname, "wrong-password-xx", store.MachineIdentity{}, "127.0.0.1", cfg.TicketTTL()); err == nil {
		fail("错误密码登录未被拒绝")
	} else {
		step("错误密码拒绝", "err="+err.Error())
	}

	verifyMachine := store.MachineIdentity{MachineID: "verify-machine"}
	_, ticket, _, err := st.Login(ctx, uname, *password, verifyMachine, "127.0.0.1", cfg.TicketTTL())
	if err != nil {
		fail("登录换票失败: %v", err)
	}
	step("登录换票成功", fmt.Sprintf("ticket_len=%d (库内仅存 sha256 摘要)", len(ticket)))

	gotAccountID, err := st.VerifyTicket(ctx, ticket, verifyMachine, "127.0.0.1")
	if err != nil {
		fail("内部验证失败: %v", err)
	}
	if gotAccountID != accountID {
		fail("内部验证返回账号不一致: got=%s want=%s", gotAccountID, accountID)
	}
	step("内部验证通过", "account_id="+gotAccountID+" (等价 POST 127.0.0.1:19091/internal/verify-game-ticket)")

	if err := st.Logout(ctx, ticket); err != nil {
		fail("登出失败: %v", err)
	}
	if _, err := st.VerifyTicket(ctx, ticket, store.MachineIdentity{}, "127.0.0.1"); err == nil {
		fail("登出后 ticket 仍可验证")
	} else {
		step("登出后票据失效", "err="+err.Error())
	}

	fmt.Println("=== ALL CHECKS PASSED ===")
}

// step 打印一条通过的检查项。
// 刻意只接"已格式化好的字符串"：早期版本把它写成 printf 包装（step(name, format, args...)），
// 调用点大量传变量当 format，go vet 会持续报 non-constant format string，
// 而且变量里一旦含 % 就会被当成动词解析出错。需要格式化时调用方自己 fmt.Sprintf。
func step(name, detail string) {
	fmt.Printf("[PASS] %-24s %s\n", name, detail)
}

func fail(format string, args ...any) {
	fmt.Printf("[FAIL] %s\n", fmt.Sprintf(format, args...))
	os.Exit(1)
}
