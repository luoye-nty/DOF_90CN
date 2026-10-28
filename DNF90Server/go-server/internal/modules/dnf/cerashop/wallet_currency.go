package cerashop

import (
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// WalletCurrency 按名字标识账号钱包里的一个字段，供 GM / 管理接口读写。
//
// 存在的理由：钱包的三个字段分别落在账号 Metadata 的三个键上，键名是 cerashop 的
// 内部细节。GM 接口需要按名字操作它们（尤其是造测试数据：把点券清零、只留代币券），
// 所以在这里开一个受控的出口，而不是让调用方去猜 "account_token_cera" 这种字符串。
type WalletCurrency string

const (
	// CurrencyCera 是点券。
	CurrencyCera WalletCurrency = "cera"
	// CurrencyTokenCera 是代币券。
	CurrencyTokenCera WalletCurrency = "token_cera"
	// CurrencyHappyTokenCera 是欢乐代币券。
	CurrencyHappyTokenCera WalletCurrency = "happy_token_cera"
)

// MetadataKey 返回该字段在账号 Metadata 里的键名。
func (c WalletCurrency) MetadataKey() (string, bool) {
	switch c {
	case CurrencyCera:
		return accountCeraMetadataKey, true
	case CurrencyTokenCera:
		return accountTokenCeraMetadataKey, true
	case CurrencyHappyTokenCera:
		return accountHappyTokenCeraMetadataKey, true
	default:
		return "", false
	}
}

// Known 表示这是一个合法的钱包字段名。
func (c WalletCurrency) Known() bool {
	_, ok := c.MetadataKey()
	return ok
}

// BalanceOf 读该字段的当前值（未知字段返回 0,false）。
func (c WalletCurrency) BalanceOf(account dnfrepo.AccountRecord) (int64, bool) {
	switch c {
	case CurrencyCera:
		return Balance(account), true
	case CurrencyTokenCera:
		return TokenCeraBalance(account), true
	case CurrencyHappyTokenCera:
		return HappyTokenCeraBalance(account), true
	default:
		return 0, false
	}
}

// SetOn 把值写进账号记录（调用方负责保存账号记录）。未知字段返回 false，不做任何改动。
//
// 值会被夹到 [0, MaxWalletBalance]：客户端按有符号 32 位读这三个字段，超过上限会
// 显示成负数，所以这里直接挡住，而不是写进去等客户端出问题。
func (c WalletCurrency) SetOn(account *dnfrepo.AccountRecord, value int64) bool {
	if account == nil || !c.Known() {
		return false
	}
	if value < 0 {
		value = 0
	}
	if value > MaxWalletBalance {
		value = MaxWalletBalance
	}
	switch c {
	case CurrencyCera:
		SetBalance(account, value)
	case CurrencyTokenCera:
		SetTokenCeraBalance(account, value)
	case CurrencyHappyTokenCera:
		SetHappyTokenCeraBalance(account, value)
	}
	return true
}
