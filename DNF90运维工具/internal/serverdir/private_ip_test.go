package serverdir

// 内网 IPv4 兜底逻辑的纯函数测试（private_ip.go）。
//
// 这里只测"判定口径"，不测 netsh 执行（那要有管理员权限的真实网卡，放到部署机上验证）。
// 判定口径必须与服务端 dnfbridge 的 isPrivateLANIPv4 完全一致，否则会出现
// "工具觉得有内网 IP、服务端却探测不到" 这种最难查的错位。

import (
	"net"
	"testing"
)

func TestIsPrivateLANIPv4(t *testing.T) {
	yes := []string{
		"10.0.0.1", "10.255.255.254",
		"172.16.0.1", "172.31.255.254",
		"192.168.0.1", "192.168.137.10",
	}
	no := []string{
		"127.0.0.1",       // 回环
		"203.0.113.11",    // 公网（本次踩坑的那台服务器）
		"26.196.103.125",  // Radmin VPN 的地址段：非私有网段，服务端不认
		"169.254.103.125", // APIPA 自动私有地址：也不是 RFC1918
		"172.15.0.1",      // 172.16/12 的下边界之外
		"172.32.0.1",      // 上边界之外
		"192.169.0.1",
		"8.8.8.8",
		"::1", // IPv6
	}
	for _, s := range yes {
		if !isPrivateLANIPv4(net.ParseIP(s)) {
			t.Fatalf("%s 应当被判定为私有网段 IPv4", s)
		}
	}
	for _, s := range no {
		if isPrivateLANIPv4(net.ParseIP(s)) {
			t.Fatalf("%s 不应当被判定为私有网段 IPv4", s)
		}
	}
	if isPrivateLANIPv4(nil) {
		t.Fatal("nil 不应当是私有网段 IPv4")
	}
}

// TestFindPrivateLANIPv4Invariant：真机探测结果必须自洽——
// 报"找到了"就必须真的满足 isPrivateLANIPv4（否则自动化会在错误的前提下跳过补地址）。
func TestFindPrivateLANIPv4Invariant(t *testing.T) {
	ip, ok := findPrivateLANIPv4()
	if !ok {
		t.Skip("本机没有私有网段 IPv4（正好是要自动补地址的场景，跳过）")
	}
	if !isPrivateLANIPv4(net.ParseIP(ip)) {
		t.Fatalf("findPrivateLANIPv4 返回了 %q，但它不满足私有网段判定", ip)
	}
}

// TestManualHintsUseQuotableForm：给用户抄的命令必须带引号包住网卡名
// （中文机器上「以太网」不带引号也能用，但带引号更稳，且能覆盖带空格的网卡名）。
func TestManualHintsUseQuotableForm(t *testing.T) {
	add := manualAddHint("以太网")
	if add != `netsh interface ipv4 add address "以太网" 192.168.137.10 255.255.255.0` {
		t.Fatalf("manualAddHint = %q", add)
	}
	del := manualDeleteHint("以太网")
	if del != `netsh interface ipv4 delete address "以太网" 192.168.137.10` {
		t.Fatalf("manualDeleteHint = %q", del)
	}
}
