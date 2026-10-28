package main

// account 型封禁的目标解析（管理工具 / 运维工具 GUI 共用一套语义）。
//
// 背景（2026-09-20 修的 bug）：
//   管理工具「封禁」页有两种填法 —— 「封禁值」（精确账号ID）与「用户名」（自动解析），
//   但运维工具的 manageBanNow 在 account 分支里**无条件**拿 ban_value 去
//   GetAccountByUsername，完全没用传进来的 username 字段。
//   于是「只填用户名、封禁值留空」时，实际查询的是空字符串 →
//   必然 user-not-found：`ban-failed: 账号不存在或查询失败: user-not-found (HTTP 500)`，
//   改从页面看就是"封号坏了"。
//
// 现在的解析顺序（三种输入都支持）：
//   1) username 非空            → 按用户名解析 account_id（管理工具主流程）
//   2) value 形如 dnf:<hex>     → 视为精确账号 ID，直封不查库
//                                 （老库迁移来的 dnf:<md5> 账号库里查不到，也要能封）
//   3) 其余非空 value           → 按用户名解析（运维工具 GUI 一直以来的用法）

import (
	"context"
	"strings"

	"dnf90ops/internal/store"
)

type accountLookupMode int

const (
	accountLookupEmpty accountLookupMode = iota
	accountLookupByUsername
	accountLookupByID
)

// chooseAccountLookup 决定用哪种方式解析目标；纯函数，便于单测。
func chooseAccountLookup(value, username string) (accountLookupMode, string) {
	if u := strings.TrimSpace(username); u != "" {
		return accountLookupByUsername, u
	}

	v := strings.TrimSpace(value)
	if v == "" {
		return accountLookupEmpty, ""
	}
	if store.LooksLikeAccountID(v) {
		return accountLookupByID, v
	}
	return accountLookupByUsername, v
}

// resolveAccountTarget 解析 account 型封禁目标，返回 (account_id, 展示用用户名)。
// 账号 ID 形式的目标查不到用户名时 username 返回空串，但封禁照做（直封语义）。
func resolveAccountTarget(ctx context.Context, st *store.Store, value, username string) (string, string, error) {
	mode, operand := chooseAccountLookup(value, username)
	switch mode {
	case accountLookupEmpty:
		return "", "", store.ErrUserNotFound
	case accountLookupByID:
		if acc, err := st.GetAccountByID(ctx, operand); err == nil && acc != nil {
			return operand, acc.Username, nil
		}
		return operand, "", nil
	default:
		acc, err := st.GetAccountByUsername(ctx, operand)
		if err != nil {
			return "", "", err
		}
		return acc.AccountID, acc.Username, nil
	}
}

// banTargetLabel 生成错误提示里"查的是谁"的描述，便于运营自己排查填错字段。
func banTargetLabel(value, username string) string {
	mode, operand := chooseAccountLookup(value, username)
	if mode == accountLookupByID {
		return "账号ID " + operand
	}
	if operand == "" {
		return "（未填写）"
	}
	return "用户名 " + operand
}

// banTargetDimensionLabel 把封禁维度线值翻成中文，供警告/日志文案使用
// （一键三封某个维度没有取值时要能说清"跳过的是哪一项"）。
func banTargetDimensionLabel(banType string) string {
	switch strings.TrimSpace(banType) {
	case store.BanAccount:
		return "账号"
	case store.BanIP:
		return "最近登录IP"
	case store.BanMachine:
		return "机器码"
	default:
		return banType
	}
}
