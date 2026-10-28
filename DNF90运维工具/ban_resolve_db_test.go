package main

// 真实库只读集成验证（默认跳过，需显式给库地址才跑）。
//
// 复现的是 2026-09-20 报上来的 bug 的真实场景：
//   管理工具「封禁类型=account、封禁值留空、用户名=1000000001」→ 旧实现拿空字符串
//   去 GetAccountByUsername → user-not-found → 页面弹「封号失败 (HTTP 500)」。
// 现在应该能解析出该账号的 account_id。
//
// 运行方式（只读，不会写任何封禁记录）：
//   set DNF90_TEST_DB=127.0.0.1:13306/dnf_local
//   set DNF90_TEST_DB_USER=root
//   set DNF90_TEST_DB_PASS=xxxx
//   go test -tags paid -run TestResolveAccountTargetAgainstRealDB -v .
//
// 不设 DNF90_TEST_DB 时自动跳过，所以 CI / 普通 go test 跑不受影响。

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dnf90ops/internal/config"
	"dnf90ops/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	target := strings.TrimSpace(os.Getenv("DNF90_TEST_DB")) // host:port/dbname
	if target == "" {
		t.Skip("未设置 DNF90_TEST_DB（host:port/dbname），跳过真实库验证")
	}
	slash := strings.LastIndex(target, "/")
	if slash <= 0 {
		t.Fatalf("DNF90_TEST_DB 格式应为 host:port/dbname，实际 %q", target)
	}
	hostPort, dbName := target[:slash], target[slash+1:]
	colon := strings.LastIndex(hostPort, ":")
	if colon <= 0 {
		t.Fatalf("DNF90_TEST_DB 缺少端口：%q", target)
	}
	port, err := strconv.Atoi(hostPort[colon+1:])
	if err != nil {
		t.Fatalf("DNF90_TEST_DB 端口非法：%q", target)
	}

	st, err := store.Open(config.MySQLConfig{
		Host:     hostPort[:colon],
		Port:     port,
		User:     os.Getenv("DNF90_TEST_DB_USER"),
		Password: os.Getenv("DNF90_TEST_DB_PASS"),
		DBName:   dbName,
	})
	if err != nil {
		t.Fatalf("连接测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestResolveAccountTargetAgainstRealDB(t *testing.T) {
	st := openTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 取库里真实存在的一个账号当样本（只读）。
	list, err := st.ListAccounts(ctx, "")
	if err != nil {
		t.Fatalf("ListAccounts 失败：%v", err)
	}
	if len(list) == 0 {
		t.Skip("测试库 launcher_accounts 为空，跳过")
	}
	sample := list[0]
	t.Logf("样本账号：username=%s account_id=%s", sample.Username, sample.AccountID)

	// ① 管理工具主流程：只填用户名、封禁值留空 → 必须解析出 account_id（旧实现在这里必然失败）
	accID, name, err := resolveAccountTarget(ctx, st, "", sample.Username)
	if err != nil {
		t.Fatalf("只填用户名解析失败（这就是被修的 bug）：%v", err)
	}
	if accID != sample.AccountID || name != sample.Username {
		t.Fatalf("解析结果 = (%s, %s)，期望 (%s, %s)", accID, name, sample.AccountID, sample.Username)
	}

	// ② 直接粘贴精确账号 ID（管理工具 UI 承诺「直封精确账号ID」）
	accID2, name2, err := resolveAccountTarget(ctx, st, sample.AccountID, "")
	if err != nil {
		t.Fatalf("精确账号ID 直封解析失败：%v", err)
	}
	if accID2 != sample.AccountID || name2 != sample.Username {
		t.Fatalf("直封解析结果 = (%s, %s)，期望 (%s, %s)", accID2, name2, sample.AccountID, sample.Username)
	}

	// ③ 运维工具 GUI 老用法：把用户名填在唯一那个输入框里
	accID3, _, err := resolveAccountTarget(ctx, st, sample.Username, "")
	if err != nil || accID3 != sample.AccountID {
		t.Fatalf("GUI 用法解析结果 = (%s, %v)，期望 %s", accID3, err, sample.AccountID)
	}

	// ④ 不存在的用户名：必须是 user-not-found（而不是别的错），错误提示要带上查的是谁
	_, _, err = resolveAccountTarget(ctx, st, "", "no-such-user-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err == nil {
		t.Fatal("不存在的用户名竟然解析成功了")
	}
	if !strings.Contains(err.Error(), "user-not-found") {
		t.Fatalf("不存在的用户名错误 = %v，期望含 user-not-found", err)
	}
	if label := banTargetLabel("", "某个不存在的号"); !strings.Contains(label, "某个不存在的号") {
		t.Fatalf("错误标签没有带上查询目标：%q", label)
	}
}
