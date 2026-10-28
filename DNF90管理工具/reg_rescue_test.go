package main

// 管理员建号表单的救援代码校验（与服务端 store.ValidRescueCode 同一规则：
// 6 位纯数字）。这里先拦一道只是为了避免白跑一次网络请求，规则本身必须一致。

import "testing"

func TestAdmValidRescueCode(t *testing.T) {
	good := []string{"123456", "000000", "999999"}
	for _, code := range good {
		if !admValidRescueCode(code) {
			t.Fatalf("%q 应当是合法救援代码", code)
		}
	}
	bad := []string{"", "12345", "1234567", "12345a", "abcdef", "12 456", "１２３４５６"}
	for _, code := range bad {
		if admValidRescueCode(code) {
			t.Fatalf("%q 不应通过校验", code)
		}
	}
}
