package serverdir

// 初始化/启动前对"内网 IPv4"的兜底（2026-09-18 新增）。
//
// 背景：服务端（DNF90Control / DNF90Server）解析 instance.json 里的
// server.advertiseIp = AUTO_DETECT 时，**只接受私有网段 IPv4**
// （10.0.0.0/8、172.16.0.0/12、192.168.0.0/16，且排除回环与虚拟网卡）；
// 一个都找不到就直接失败：
//
//	auto-detect server.advertiseIp: no active private LAN IPv4 address was found
//
// 而且**不能拿公网 IP 顶替**——显式填公网地址会被校验拒绝：
//
//	90cn game channels require server.advertiseIp to be a private LAN IPv4 address or 127.0.0.1
//
// 只有公网 IP 直绑网卡的云主机/VPS 就会卡在这一步（2026-09-18 在 203.0.113.11 实测复现）。
// 所以这里在点初始化/一键启动前先探测一次：没有私有 IPv4 就给**主网卡**加一个静态备用地址。
//
// 几个刻意的选择：
//   - 加的是**备用地址**（不是新装虚拟网卡）：一条 netsh 就能加，随时可删；
//   - **不设网关**：默认路由不变，公网出口照旧，不会把玩家流量带跑偏；
//   - netsh 优先用**接口索引**而不是网卡名：中文/本地化网卡名（「以太网」）以及
//     引号被粘成弯引号的问题都绕开了——之前手工加地址时正是在这里报
//     "文件名、目录名或卷标语法不正确"；
//   - 失败时给出**能直接照抄的手工命令**，而不是把 netsh 的原始报错丢出去。

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// autoPrivateLANIPv4 自动添加的备用私有地址。
//
// 取值原则：随便一个不会跟真实内网撞车的私有网段即可。用 192.168.x 是因为服务端
// 在多个候选里**优先挑 192.168.x**（privateLANSubnetRank），选它就不会被别的网卡盖过。
const (
	autoPrivateLANIPv4     = "192.168.137.10"
	autoPrivateLANIPv4Mask = "255.255.255.0"
)

// isPrivateLANIPv4 是否服务端认可的那种「私有网段 IPv4」。
// 口径与服务端 isPrivateLANIPv4 保持一致：IPv4、非回环、RFC1918。
func isPrivateLANIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil || ip.IsLoopback() {
		return false
	}
	return ip4[0] == 10 ||
		(ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) ||
		(ip4[0] == 192 && ip4[1] == 168)
}

// findPrivateLANIPv4 扫描**已启用**的网卡，返回第一个私有 IPv4。
// 跳过回环；down 掉的网卡不算（服务端也要求 FlagUp）。
func findPrivateLANIPv4() (string, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			if isPrivateLANIPv4(ipNet.IP) {
				return ipNet.IP.To4().String(), true
			}
		}
	}
	return "", false
}

// outboundIPv4 取本机出网用的 IPv4（连一个不可达的外部地址只为拿到本地端点，不发包）。
func outboundIPv4() net.IP {
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return nil
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.To4()
	}
	return nil
}

// primaryInterface 找"主网卡"：优先出口 IP 所在的那张，其次第一张 up 且非回环、带 IPv4 的网卡。
// 返回网卡与它的当前 IPv4（用于日志说明）。
func primaryInterface() (net.Interface, string, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return net.Interface{}, "", false
	}
	matches := func(iface net.Interface, want net.IP) (string, bool) {
		if want == nil {
			return "", false
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", false
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil || ipNet.IP.To4() == nil {
				continue
			}
			if ipNet.IP.To4().Equal(want) {
				return ipNet.IP.To4().String(), true
			}
		}
		return "", false
	}

	want := outboundIPv4()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ip, ok := matches(iface, want); ok {
			return iface, ip, true
		}
	}
	// 出口 IP 没匹配上（多网卡/VPN 场景）：退而求其次，第一张带 IPv4 的 up 网卡。
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ip := ifaceIPv4(iface); ip != nil {
			return iface, ip.String(), true
		}
	}
	return net.Interface{}, "", false
}

// ifaceIPv4 返回网卡上的第一个 IPv4（用于 primaryInterface 的兜底分支）。
func ifaceIPv4(iface net.Interface) net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP != nil && ipNet.IP.To4() != nil {
			return ipNet.IP.To4()
		}
	}
	return nil
}

// netshIPv4AddAddress 添加静态地址；target 传"接口索引"或网卡名都行（索引优先，避开本地化名字）。
func netshIPv4AddAddress(target, ip, mask string) error {
	cmd := exec.Command("netsh", "interface", "ipv4", "add", "address", target, ip, mask)
	cmd.SysProcAttr = hideWindowAttr()
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// EnsurePrivateLANIPv4 确保本机有一个服务端认的私有 IPv4：
// 已有就直接返回；没有就给主网卡加一个备用地址（见文件头注释的取舍）。
//
// 幂等：重复调用不会重复加地址（第一次加完第二次探测就能找到）。
//
// **失败只告警、不阻断**：旧版控制器没有内网 IP 会硬失败（所以这里要在它之前补），
// 但新版控制器会自己回退到 127.0.0.1 继续跑；如果这里改成返回错误，反而会把
// "本来能起来"的部署挡在门外。所以补地址失败时只打印可照抄的手工命令，
// 让后面的控制器/服务端按自己的规则去决定成败。
func EnsurePrivateLANIPv4(onLine func(string)) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	if ip, ok := findPrivateLANIPv4(); ok {
		if onLine != nil {
			onLine(fmt.Sprintf("内网 IPv4 检查通过: %s", ip))
		}
		return nil
	}

	if onLine != nil {
		onLine("未检测到内网 IPv4（服务端 advertiseIp=AUTO_DETECT 只认私有网段），尝试自动添加备用地址 ...")
	}

	iface, currentIP, ok := primaryInterface()
	if !ok {
		if onLine != nil {
			onLine("警告: 未检测到内网 IPv4，且找不到可用的物理网卡；请手动执行 " +
				manualAddHint("<网卡名或索引>") + "，或把 runtime\\config\\instance.json 的 server.advertiseIp 设为 127.0.0.1")
		}
		return nil
	}

	// 先按接口索引加（最稳），失败再按网卡名重试一次。
	idx := strconv.Itoa(iface.Index)
	err := netshIPv4AddAddress(idx, autoPrivateLANIPv4, autoPrivateLANIPv4Mask)
	if err != nil {
		if nameErr := netshIPv4AddAddress(iface.Name, autoPrivateLANIPv4, autoPrivateLANIPv4Mask); nameErr != nil {
			if onLine != nil {
				onLine(fmt.Sprintf("警告: 自动添加内网 IP 失败（%v）；多半是没有以管理员身份运行。"+
					"请手动执行 %s，或把 runtime\\config\\instance.json 的 server.advertiseIp 设为 127.0.0.1",
					err, manualAddHint(iface.Name)))
			}
			return nil
		}
	}

	got, found := findPrivateLANIPv4()
	if !found {
		if onLine != nil {
			onLine(fmt.Sprintf("警告: 已执行添加命令但复查仍没有内网 IPv4；请手动确认 %s", manualAddHint(iface.Name)))
		}
		return nil
	}

	if onLine != nil {
		onLine(fmt.Sprintf("已为网卡 %s（索引 %d，当前地址 %s）添加备用内网 IP %s/%s",
			iface.Name, iface.Index, currentIP, autoPrivateLANIPv4, autoPrivateLANIPv4Mask))
		onLine("说明：该地址不设网关，默认路由与公网出口不受影响；外网玩家连接仍走「公网 IP」那一栏。撤销命令: " +
			manualDeleteHint(iface.Name))
	}
	if got != autoPrivateLANIPv4 {
		// 理论上不会走到：探测到的是别的私有地址（例如同时还有 VPC 网卡），那也满足服务端要求。
		if onLine != nil {
			onLine(fmt.Sprintf("内网 IPv4 复查结果: %s", got))
		}
	}
	return nil
}

func manualAddHint(target string) string {
	return fmt.Sprintf("netsh interface ipv4 add address \"%s\" %s %s", target, autoPrivateLANIPv4, autoPrivateLANIPv4Mask)
}

func manualDeleteHint(target string) string {
	return fmt.Sprintf("netsh interface ipv4 delete address \"%s\" %s", target, autoPrivateLANIPv4)
}
