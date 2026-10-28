// 第③步：服主 launcher 端 yh 授权回调核销（V13 方案 §5③）。
//
// 登录器在登录上报时携带 yh_ticket + yh_nonce（见 loginRequest.YhTicket / YhNonce），
// launcher 收到后回调独立 authserver（/api/yh/check）核销：
//   - 核销通过：继续原有登录/发票流程；
//   - 核销失败：拒绝登录（403 yh-check-failed），防止 patch 登录器伪造本地验签后直连。
//
// 兼容性设计：
//   - 未调用 SetYhCheck（未配置）→ 完全不回调，行为与改造前一致（向后兼容）；
//   - 请求未携带 yh_ticket/yh_nonce（旧版外网版登录器）→ 跳过回调，保留存量用户；
//   - 请求只带半套或带全但核销失败 → 拒绝，防止伪造半套绕过。
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// YhCheckFunc 回调 authserver 核销一次 yh 授权。err 非空表示核销失败（拒绝放行）。
// targetIP 为登录请求中携带的目标服务器 IP（登录器本次要连接的服），
// authserver 侧与票据内嵌 server_ip 比对（ipEquivalent），防止跨服复用票据。
type YhCheckFunc func(ctx context.Context, ticket, nonce, targetIP string) error

// yhCheckPin 验证服务器证书 DER SHA-256（TLS pinning 用）。
//
// 【已去授权】不再内置作者服务器的证书指纹：留空即可，实际值只能由环境变量
// YH_CHECK_PIN 提供（十六进制小写）。去授权后本文件构造出的回调也不会被装配
// （见 server.go 的 SetYhCheck 空实现）。
const yhCheckPin = ""

// YhCheckClient authserver 回调客户端。
type YhCheckClient struct {
	baseURL string
	key     string
	http    *http.Client
	log     *slog.Logger
}

// checkRequest / checkResponse 对齐 authserver /api/yh/check 协议。
type yhCheckRequest struct {
	Ticket   string `json:"ticket"`
	Nonce    string `json:"nonce"`
	TargetIP string `json:"target_ip"`
}

type yhCheckResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"msg,omitempty"`
}

// NewYhCheckClient 构造回调客户端。baseURL 形如 https://<验证服务器>:8443，
// key 为 X-Yh-Callback-Key 回调密钥，pinDERHex 为服务端证书 DER SHA-256（空则用 yhCheckPin）。
//
// 【已去授权】源码里不再写死任何验证服务器地址；本函数构造出的客户端也不会被
// 装配（server.go 的 SetYhCheck 是空实现）。
func NewYhCheckClient(baseURL, key, pinDERHex string, log *slog.Logger) (*YhCheckClient, error) {
	if baseURL == "" {
		return nil, errors.New("empty base url")
	}
	if key == "" {
		return nil, errors.New("empty callback key")
	}
	pin := yhCheckPin
	if pinDERHex != "" {
		pin = pinDERHex
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// 自签证书：不信任系统根，仅允许证书指纹匹配 pin 的服务端。
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // 下面 VerifyPeerCertificate 手动 pin
				VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
					if len(rawCerts) == 0 {
						return errors.New("yh callback: no peer certificate")
					}
					sum := sha256.Sum256(rawCerts[0])
					got := hex.EncodeToString(sum[:])
					if got != pin {
						return fmt.Errorf("yh callback: certificate pin mismatch (got %s)", got)
					}
					return nil
				},
			},
		},
	}
	if log == nil {
		log = slog.Default()
	}
	return &YhCheckClient{baseURL: strings.TrimRight(baseURL, "/"), key: key, http: client, log: log}, nil
}

// Check 实现 YhCheckFunc：回调 authserver /api/yh/check。
func (c *YhCheckClient) Check(ctx context.Context, ticket, nonce, targetIP string) error {
	body, err := json.Marshal(yhCheckRequest{Ticket: ticket, Nonce: nonce, TargetIP: targetIP})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/yh/check", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Yh-Callback-Key", c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("yh callback: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("yh callback: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("yh callback: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out yhCheckResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("yh callback: decode: %w", err)
	}
	if !out.OK {
		if out.Message == "" {
			out.Message = "authserver rejected"
		}
		return errors.New(out.Message)
	}
	return nil
}

// NewYhCheckAuto 装配回调客户端（主程序启动时调用），优先级：
//
//  1. 环境变量覆盖：YH_CHECK_URL + YH_CHECK_KEY 必须成对出现才生效
//     （YH_CHECK_PIN 可选），适合 key 轮换/临时指到其他 authserver 时使用；
//     只残留半套视为未配置，自动回退内置，避免误配导致赞助版不装配；
//
//  2. 内置配置：赞助版（paid）编译时 yhBuiltinSecrets() 返回内置混淆常量，
//     零外部配置自动开启核销；免费版返回空串 → (nil, nil) 不装配。
//
//     返回 (nil, nil) 表示不启用回调，行为与改造前一致。
func NewYhCheckAuto(log *slog.Logger) (*YhCheckClient, error) {
	envURL := strings.TrimSpace(os.Getenv("YH_CHECK_URL"))
	envKey := strings.TrimSpace(os.Getenv("YH_CHECK_KEY"))
	url, key, pin := yhBuiltinSecrets()
	if envURL != "" && envKey != "" {
		url, key = envURL, envKey
		pin = strings.TrimSpace(os.Getenv("YH_CHECK_PIN"))
	}
	if url == "" {
		return nil, nil
	}
	if key == "" {
		return nil, fmt.Errorf("yh callback key empty")
	}
	return NewYhCheckClient(url, key, pin, log)
}
