// 公网 IPv4 自动获取（与 S4OpsTool ip.go 同源）。
package serverdir

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ipSources 公网 IP 探测源，按优先级排列，失败自动换下一个。
var ipSources = []string{
	"https://api.ipify.org",
	"https://ipv4.icanhazip.com",
	"https://4.ipw.cn",
	"https://ifconfig.me/ip",
}

var ipv4Re = regexp.MustCompile(`\b(\d{1,3}(?:\.\d{1,3}){3})\b`)

// GetPublicIP 自动获取本机公网 IPv4。
func GetPublicIP() (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for _, src := range ipSources {
		resp, err := client.Get(src)
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		if ip := parseIP(string(b)); ip != "" {
			return ip, nil
		}
		lastErr = fmt.Errorf("unparsable response from %s", src)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all public IP sources failed")
	}
	return "", lastErr
}

// parseIP 从任意文本中提取第一个合法的公网 IPv4。
func parseIP(s string) string {
	m := ipv4Re.FindString(s)
	if m == "" {
		return ""
	}
	ip := net.ParseIP(m)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return ""
	}
	// ipify 等源个别返回末尾带 \n，做一次 TrimSpace。
	return strings.TrimSpace(m)
}
